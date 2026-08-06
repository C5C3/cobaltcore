// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/database"
	"github.com/c5c3/forge/internal/common/deployment"
	"github.com/c5c3/forge/internal/common/job"
	barbicanv1alpha1 "github.com/c5c3/forge/operators/barbican/api/v1alpha1"
	barbicanmetrics "github.com/c5c3/forge/operators/barbican/internal/metrics"
)

// Condition type and reasons for the recurring database clean-up. The condition
// is True while the CronJob is projected and the most recent run it spawned did
// not fail; a failed run flips it False until a later run succeeds or the
// clean-up is suspended. A run that wedges instead of failing cannot hold it
// True either: dbCleanActiveDeadlineSeconds turns every stalled run into a
// terminal failure.
//
// A suspended clean-up stays True — pausing it is an operator's deliberate
// posture, not a failure — but reports it under its own reason, because the
// backlog it stops draining keeps growing and nothing else says so: the metric
// simply stops incrementing, which raises no alert on its own.
const (
	conditionTypeDBCleanReady       = "DBCleanReady"
	conditionReasonDBCleanScheduled = "DBCleanScheduled"
	conditionReasonDBCleanSuspended = "DBCleanSuspended"
	conditionReasonDBCleanJobFailed = "DBCleanJobFailed"
	// conditionReasonDBCleanBlocked is the migration pause that cannot end on its
	// own: the release gate rejected the transition, so no Job will ever advance
	// status.installedRelease to the requested release and the pause holds until
	// the spec is corrected.
	conditionReasonDBCleanBlocked = "DBCleanBlocked"
)

// defaultDBCleanSchedule runs the clean-up daily at 00:01, resolved when
// spec.dbClean.schedule is empty. It lives here rather than beside
// barbicanv1alpha1.DefaultDBCleanRetentionDays because no admission-time check
// needs it: the retention default is read by the webhook's
// retention-reduction warning, the schedule by nothing outside this step.
const defaultDBCleanSchedule = "1 0 * * *"

// dbCleanActiveDeadlineSeconds caps how long one run may stay active. It is what
// keeps a wedged clean-up observable: reconcileDBClean reports on runs that
// reached a terminal condition, and a pod stuck in ImagePullBackOff or a
// barbican-manage blocked on a database lock reaches none on its own — the run
// would stay active forever while DBCleanReady kept reporting a healthy
// schedule. The deadline turns every such wedge into a JobFailed within the
// hour, surfacing it as DBCleanReady=False plus a Warning event.
const dbCleanActiveDeadlineSeconds int64 = 3600

// dbCleanNameSuffix names the clean-up CronJob after its Barbican. The api
// package mirrors this literal (it derives
// barbicanv1alpha1.MaxBarbicanNameLength from it) because it cannot import this
// package.
const dbCleanNameSuffix = "-db-clean"

// dbCleanNameHashLength is what the collapsed form below spends on the hash: a
// separator plus eight hex characters.
const dbCleanNameHashLength = 1 + 8

// dbCleanComponent is the app.kubernetes.io/component value the clean-up pods
// carry. It differs from the value the API pods carry, which is what keeps the
// clean-up pods out of the API Service selector.
const dbCleanComponent = "db-clean"

// dbCleanCronJobName returns the name of the clean-up CronJob, collapsing an
// over-long Barbican name onto a content-stable hash.
//
// Admission bounds metadata.name so the plain "{name}-db-clean" form fits the
// MaxCronJobNameLength characters Kubernetes allows a CronJob name — but on
// create only, and it has to be: metadata.name is immutable, so on update the
// rule could only fire against an object a pre-upgrade operator already
// admitted, including the finalizer-removal update that completes its deletion.
// An operator upgrade therefore inherits Barbican CRs the bound would reject,
// and building their name unconditionally would fail every reconcile on an apply
// the API server refuses, forever, with no field left to edit to repair it.
// Every admissible name keeps the documented plain form.
func dbCleanCronJobName(barbicanName string) string {
	if len(barbicanName) <= barbicanv1alpha1.MaxBarbicanNameLength {
		return barbicanName + dbCleanNameSuffix
	}
	sum := sha256.Sum256([]byte(barbicanName))
	// The trim keeps the truncation from ending a DNS label on "-" or "." —
	// which would make the very name this function exists to produce
	// unapplicable — and only ever shortens, so the cap still holds.
	kept := strings.TrimRight(barbicanName[:barbicanv1alpha1.MaxBarbicanNameLength-dbCleanNameHashLength], "-.")
	return fmt.Sprintf("%s-%x%s", kept, sum[:4], dbCleanNameSuffix)
}

// effectiveDBClean returns the settings the clean-up CronJob runs with,
// materializing the operator defaults for the fields spec.dbClean leaves unset.
// A nil block behaves exactly like an empty one. It is pure and total: no input
// can fail, so there is no error path, and the result is always fully resolved
// (RetentionDays, SoftDeleteExpiredSecrets and CleanUnassociatedProjects
// non-nil, Schedule non-empty).
//
// SoftDeleteExpiredSecrets resolves to true, a deliberate deviation from
// barbican's own CLI default of false: an expired secret is unusable, and
// without the pass its row is never soft-deleted and therefore never reaches the
// retention window the clean-up purges by. The field doc records the same
// deviation, so an operator who wants the upstream behaviour sets it to false.
//
// Resolving here rather than in the defaulting webhook keeps an unset field
// tracking the operator default across upgrades instead of freezing today's
// value into the stored CR.
func effectiveDBClean(spec *barbicanv1alpha1.DBCleanSpec) barbicanv1alpha1.DBCleanSpec {
	var out barbicanv1alpha1.DBCleanSpec
	if spec != nil {
		out = *spec
	}
	if out.RetentionDays == nil {
		retention := barbicanv1alpha1.DefaultDBCleanRetentionDays
		out.RetentionDays = &retention
	}
	if out.Schedule == "" {
		out.Schedule = defaultDBCleanSchedule
	}
	if out.SoftDeleteExpiredSecrets == nil {
		out.SoftDeleteExpiredSecrets = ptr.To(true)
	}
	if out.CleanUnassociatedProjects == nil {
		out.CleanUnassociatedProjects = ptr.To(false)
	}
	return out
}

// reconcileDBClean projects the recurring database clean-up CronJob and reports
// the outcome of the most recent run it spawned. The CronJob is ensured on every
// pass — a Barbican without spec.dbClean still gets the sweep, because barbican
// never hard-deletes on its own and an unbounded soft-delete backlog is a
// deferred outage rather than a posture worth offering — and spec.dbClean only
// varies its retention, schedule, extra passes, and suspension.
//
// Run visibility is derived rather than watched: the CronJob controller spawns
// one Job per firing and prunes them by history limit, so the step lists the
// Jobs carrying this Barbican's common labels, keeps the ones the CronJob
// controls, and reports on the newest that reached a terminal state. A failed
// run surfaces as DBCleanReady=False plus a Warning event; every terminal run
// also feeds the db-clean metric pair exactly once per Job UID.
func (r *BarbicanReconciler) reconcileDBClean(ctx context.Context, barbican *barbicanv1alpha1.Barbican, configSecretName string) (ctrl.Result, error) {
	clean := effectiveDBClean(barbican.Spec.DBClean)

	// No config yet means no credential-ready default secret store has produced a
	// barbican.conf for the clean-up to mount, so there is nothing to schedule
	// against: an empty Secret name renders a volume the API server rejects
	// outright. The Database step right behind this one requeues on the same
	// condition, so no requeue is needed here.
	if configSecretName == "" {
		conditions.SetCondition(&barbican.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBCleanReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: barbican.Generation,
			Reason:             conditionReasonWaitingForSecretStores,
			Message:            "Waiting for a credential-ready default secret store before scheduling the database clean-up",
		})
		return ctrl.Result{}, nil
	}

	// A failed ensure returns the wrapped error and sets no condition, matching
	// the sibling steps: DBCleanReady then stays absent or stale, which already
	// keeps the aggregate Ready False, and the pipeline attributes the error to
	// this step.
	cronJob := dbCleanCronJob(barbican, configSecretName)
	if err := job.EnsureCronJob(ctx, r.Client, r.Scheme, barbican, cronJob); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring db clean CronJob: %w", err)
	}

	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(barbican.Namespace),
		client.MatchingLabels(commonLabels(barbican))); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing db clean Jobs: %w", err)
	}
	// EnsureCronJob decodes the apply response back into cronJob, so it carries
	// the persisted UID the owner-reference match below compares against.
	observed := newestTerminalJob(cronJob, jobs.Items)

	switch {
	// Suspension outranks a failed run, because a suspended CronJob spawns no
	// successor to supersede it: the failed Job stays the newest terminal one
	// for good (the default failedJobsHistoryLimit retains it), so a JobFailed
	// arm that won here would pin DBCleanReady False — and with it the aggregate
	// Ready and the ControlPlane's mirrored BarbicanReady — until someone deleted
	// the Job by hand. The failure is still named in the message; it is just not
	// the state the CR is in.
	case clean.Suspend:
		message := fmt.Sprintf("Database clean-up CronJob suspended via spec.dbClean.suspend; no rows are being "+
			"hard-deleted and the soft-deleted backlog keeps growing (schedule %q, retention %d days when resumed)",
			clean.Schedule, *clean.RetentionDays)
		if observed != nil && job.TerminalCondition(observed) == batchv1.JobFailed {
			message += fmt.Sprintf("; the last run (Job %s) failed before the pause", observed.Name)
		}
		conditions.SetCondition(&barbican.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBCleanReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: barbican.Generation,
			Reason:             conditionReasonDBCleanSuspended,
			Message:            message,
		})
	// A migration pause outranks a failed run for the same reason an operator
	// pause does: the schedule spawns no successor while it holds, so the failed
	// Job would stay the newest terminal one and pin the condition False for the
	// whole upgrade.
	//
	// It is reported True only while the convergence it waits for can still
	// happen. dbCleanMigrationInFlight is a difference between the installed and
	// the requested release, not a Job in flight, and a release gate that
	// REJECTED the transition holds that difference open indefinitely with
	// nothing running — so a True here would assert a convergence that is not
	// coming while the soft-deleted backlog grows without bound and the only
	// condition reporting clean-up health says everything is fine.
	case dbCleanMigrationInFlight(barbican):
		status, reason := metav1.ConditionTrue, conditionReasonDBCleanSuspended
		message := fmt.Sprintf("Database clean-up CronJob paused while the schema converges from %q to %q; "+
			"it resumes on the schedule %q once the migration completes",
			installedReleaseOrNone(barbican), barbican.Spec.OpenStackRelease, clean.Schedule)
		if gateReason := dbCleanReleaseGateBlocked(barbican); gateReason != "" {
			status, reason = metav1.ConditionFalse, conditionReasonDBCleanBlocked
			message = fmt.Sprintf("Database clean-up CronJob paused for a schema convergence from %q to %q the release "+
				"gate rejected (DatabaseReady=%s), so no migration is running and the pause cannot end on its own; "+
				"no rows are being hard-deleted and the soft-deleted backlog keeps growing until spec.openStackRelease "+
				"and spec.image are corrected",
				installedReleaseOrNone(barbican), barbican.Spec.OpenStackRelease, gateReason)
		}
		conditions.SetCondition(&barbican.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBCleanReady,
			Status:             status,
			ObservedGeneration: barbican.Generation,
			Reason:             reason,
			Message:            message,
		})
	case observed != nil && job.TerminalCondition(observed) == batchv1.JobFailed:
		conditions.SetCondition(&barbican.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBCleanReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: barbican.Generation,
			Reason:             conditionReasonDBCleanJobFailed,
			Message:            fmt.Sprintf("Database clean-up Job %s failed; the soft-deleted backlog is no longer shrinking", observed.Name),
		})
		r.Recorder.Eventf(barbican, corev1.EventTypeWarning, conditionReasonDBCleanJobFailed,
			"Database clean-up Job %s failed; inspect its pod logs — the soft-deleted secret, container and order rows keep accumulating until a run succeeds",
			observed.Name)
	default:
		conditions.SetCondition(&barbican.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBCleanReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: barbican.Generation,
			Reason:             conditionReasonDBCleanScheduled,
			Message: fmt.Sprintf("Database clean-up scheduled %q, hard-deleting rows soft-deleted more than %d days ago",
				clean.Schedule, *clean.RetentionDays),
		})
	}

	// Emit the terminal metrics for the observed run. The shared helper dedupes on
	// the Job UID via an annotation on the Barbican CR, so a run is counted once
	// no matter how many passes observe it, and a nil observed (no terminal run
	// yet) is a no-op.
	job.RecordJobTerminalState(ctx, r.Client, r.Recorder, barbican, dbCleanComponent, observed,
		"DBCleanMetricEmissionDeferred",
		func(result string, duration time.Duration) {
			barbicanmetrics.RecordDBClean(barbican.Name, barbican.Namespace, result, duration)
		})

	return ctrl.Result{}, nil
}

// newestTerminalJob returns the most recently created Job among jobs that the
// given CronJob controls and that has reached a terminal state, or nil when no
// such Job exists yet. Newest wins because the CronJob keeps a bounded history:
// an older failed run must not keep the condition False once a later run has
// succeeded.
func newestTerminalJob(cronJob *batchv1.CronJob, jobs []batchv1.Job) *batchv1.Job {
	var newest *batchv1.Job
	for i := range jobs {
		candidate := &jobs[i]
		if !metav1.IsControlledBy(candidate, cronJob) {
			continue
		}
		if job.TerminalCondition(candidate) == "" {
			continue
		}
		if newest == nil || candidate.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = candidate
		}
	}
	return newest
}

// dbCleanMigrationInFlight reports whether the installed schema is not at the
// release the spec asks for — a fresh install (no installed release) included,
// where there is no schema to clean yet either. It is the CronJob's second
// suspend input and is derived from the CR alone, so the projected CronJob stays
// a pure function of the Barbican it belongs to.
func dbCleanMigrationInFlight(barbican *barbicanv1alpha1.Barbican) bool {
	return barbican.Status.InstalledRelease != barbican.Spec.OpenStackRelease
}

// dbCleanReleaseGateBlocked returns the DatabaseReady reason when the release
// transition the migration pause waits for was REJECTED rather than merely not
// completed yet, and "" otherwise.
//
// Those four reasons are the terminal ones: checkImageReleaseMismatch and every
// rejectReleaseTransition arm refuse to advance status.installedRelease and
// leave it refused until spec.openStackRelease or spec.image changes. Every
// other DatabaseReady=False state — the provisioning waits, a sync in progress,
// a failed sync — either resolves on its own or is already reported as a
// failure, so the pause behind it stays self-limiting.
//
// The DBClean step runs ahead of the Database step, so it reads the previous
// pass's condition. That is the correct input here: a rejection is exactly the
// state that reproduces on every pass.
func dbCleanReleaseGateBlocked(barbican *barbicanv1alpha1.Barbican) string {
	cond := conditions.GetCondition(barbican.Status.Conditions, "DatabaseReady")
	if cond == nil || cond.Status != metav1.ConditionFalse {
		return ""
	}
	switch cond.Reason {
	case database.ReasonVersionParseError, database.ReasonDowngradeNotSupported,
		database.ReasonUpgradePathInvalid, conditionReasonImageReleaseMismatch:
		return cond.Reason
	default:
		return ""
	}
}

// installedReleaseOrNone renders status.installedRelease for the paused-condition
// message, naming the empty marker of a fresh install rather than printing "".
func installedReleaseOrNone(barbican *barbicanv1alpha1.Barbican) string {
	if barbican.Status.InstalledRelease == "" {
		return "no installed release"
	}
	return barbican.Status.InstalledRelease
}

// dbCleanCommand assembles the barbican-manage invocation from the resolved
// settings: the retention window, then the two optional passes in the order
// barbican documents them.
//
// It is a plain argv rather than a shell line: the passes are flags of one
// command, so there is nothing to chain and no shell to interpose.
func dbCleanCommand(clean barbicanv1alpha1.DBCleanSpec) []string {
	cmd := []string{"barbican-manage", "db", "clean", "--min-days", strconv.Itoa(int(*clean.RetentionDays))}
	if *clean.SoftDeleteExpiredSecrets {
		cmd = append(cmd, "--soft-delete-expired-secrets")
	}
	if *clean.CleanUnassociatedProjects {
		cmd = append(cmd, "--clean-unassociated-projects")
	}
	return cmd
}

// dbCleanCronJob builds the CronJob that hard-deletes the rows barbican only
// ever soft-deletes. It carries the resolved retention, schedule and passes from
// effectiveDBClean and the rendered config Secret mounted read-only so
// barbican-manage reaches the database.
func dbCleanCronJob(barbican *barbicanv1alpha1.Barbican, configSecretName string) *batchv1.CronJob {
	clean := effectiveDBClean(barbican.Spec.DBClean)

	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dbCleanCronJobName(barbican.Name),
			Namespace: barbican.Namespace,
			Labels:    commonLabels(barbican),
		},
		Spec: batchv1.CronJobSpec{
			Schedule: clean.Schedule,
			// Suspended while the schema migrates as well as on operator request:
			// barbican-manage db upgrade takes DDL locks the clean-up's bulk DELETEs
			// on the same tables contend with — on Galera a TOI ALTER stalls the
			// cluster for the duration, on a single MariaDB the migration waits out
			// an InnoDB lock timeout and the Job can exhaust its backoff limit, which
			// wedges the upgrade until someone deletes the Job by hand.
			// ConcurrencyPolicy below guards a clean-up against a clean-up; nothing
			// else guards it against the migration.
			Suspend: ptr.To(clean.Suspend || dbCleanMigrationInFlight(barbican)),
			// Two clean-ups deleting from the same tables contend on the same rows:
			// on Galera the later write-set fails certification, on a single MariaDB
			// it waits out an InnoDB lock timeout. Forbid keeps a run that outlasts
			// its interval from being overtaken by the next firing — without it a
			// wedged run would accumulate one active Job per firing with no ceiling.
			ConcurrencyPolicy: batchv1.ForbidConcurrent,
			JobTemplate: batchv1.JobTemplateSpec{
				// The spawned Jobs inherit these labels, which is what lets the
				// reconcile above list the runs by label instead of by name — the
				// CronJob controller names each Job after the firing timestamp.
				ObjectMeta: metav1.ObjectMeta{
					Labels: commonLabels(barbican),
				},
				Spec: batchv1.JobSpec{
					ActiveDeadlineSeconds: ptr.To(dbCleanActiveDeadlineSeconds),
					Template: corev1.PodTemplateSpec{
						// A NetworkPolicy-enabled deployment selects the clean-up pods
						// through these labels — they are a superset of the Deployment's
						// selectorLabels — and the auto-derived DB and DNS egress is all
						// barbican-manage needs, so no clean-up-specific rule is required.
						// The component value differs from the one the API pods carry,
						// which is what keeps the clean-up pods out of the API Service
						// selector.
						ObjectMeta: metav1.ObjectMeta{
							Labels: componentLabels(barbican, dbCleanComponent),
						},
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyOnFailure,
							Containers: []corev1.Container{{
								Name:            dbCleanComponent,
								Image:           barbican.Spec.Image.Reference(),
								Command:         dbCleanCommand(clean),
								SecurityContext: deployment.RestrictedSecurityContext(),
								// Override [database] connection via the oslo.config env-var so
								// the clean-up reads the DB URL from the derived Secret instead
								// of the placeholder in the rendered config.
								Env: []corev1.EnvVar{database.ConnectionEnvVar(barbican.Name)},
								VolumeMounts: []corev1.VolumeMount{
									{Name: configVolumeName, MountPath: barbicanConfigMountPath, ReadOnly: true},
								},
							}},
							Volumes: []corev1.Volume{{
								Name: configVolumeName,
								VolumeSource: corev1.VolumeSource{
									Secret: &corev1.SecretVolumeSource{SecretName: configSecretName},
								},
							}},
						},
					},
				},
			},
		},
	}

	// Project the db-tls client keypair when database TLS is enabled: the DSN the
	// clean-up injects then names ssl_ca/ssl_cert/ssl_key paths under
	// dbTLSMountPath, so without the mount barbican-manage fails to open them on
	// every run. The gate is the same barbicanDBTLSEnabled the Deployment uses, so
	// both decide identically.
	if barbicanDBTLSEnabled(barbican) {
		tlsVol, tlsMount := barbicanDBTLSVolumeAndMount(barbican)
		podSpec := &cronJob.Spec.JobTemplate.Spec.Template.Spec
		podSpec.Volumes = append(podSpec.Volumes, tlsVol)
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, tlsMount)
	}
	return cronJob
}
