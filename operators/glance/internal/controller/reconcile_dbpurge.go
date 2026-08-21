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

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/job"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/glance/internal/metrics"
)

// Condition type and reasons for the recurring database purge. The condition is
// True while the CronJob is projected and the most recent run it spawned did not
// fail; a failed run flips it False until a later run succeeds or the purge is
// suspended. A run that wedges instead of failing cannot hold it True either:
// dbPurgeActiveDeadlineSeconds turns every stalled run into a terminal failure.
//
// A suspended purge stays True — pausing it is an operator's deliberate posture,
// not a failure — but reports it under its own reason, because the backlog it
// stops draining keeps growing and nothing else says so: the metric simply stops
// incrementing, which raises no alert on its own.
const (
	conditionTypeDBPurgeReady       = "DBPurgeReady"
	conditionReasonDBPurgeScheduled = "DBPurgeScheduled"
	conditionReasonDBPurgeSuspended = "DBPurgeSuspended"
	conditionReasonDBPurgeJobFailed = "DBPurgeJobFailed"
)

// dbPurgeMaxRows bounds how many rows a single per-table delete transaction
// removes, keeping the purge Galera-friendly: a write-set is replicated and
// certified as one unit, so an unbounded delete of a long-neglected backlog
// would stall the cluster for the duration of the transaction. It is
// operator-owned rather than an API field — glance's own default of 100 rows per
// table would lose ground against a busy deployment's daily growth.
//
// The cap applies per invocation, so the schedule sets the throughput: on the
// default daily schedule the purge drains at most dbPurgeMaxRows rows per table
// per day. That converges only while the deployment soft-deletes fewer rows per
// table than one run removes — a cloud importing more images a day than the cap
// grows its tasks table faster than the purge drains it, and a brownfield
// backlog takes one run per batch to work off. spec.dbPurge.schedule is the
// lever for both: an hourly purge multiplies the daily ceiling by 24.
const dbPurgeMaxRows = 1000

// dbPurgeActiveDeadlineSeconds caps how long one run may stay active. It is what
// keeps a wedged purge observable: reconcileDBPurge reports on runs that reached
// a terminal condition, and a pod stuck in ImagePullBackOff or a glance-manage
// blocked on a database lock reaches none on its own — the run would stay active
// forever while DBPurgeReady kept reporting a healthy schedule. The deadline
// turns every such wedge into a JobFailed within the hour, surfacing it as
// DBPurgeReady=False plus a Warning event. An hour is far above the runtime of a
// bounded delete of dbPurgeMaxRows rows per table.
const dbPurgeActiveDeadlineSeconds int64 = 3600

// dbPurgeNameSuffix names the purge CronJob after its Glance. The api package
// mirrors this literal (it derives glancev1alpha1.MaxGlanceNameLength from it)
// because it cannot import this package.
const dbPurgeNameSuffix = "-db-purge"

// dbPurgeNameHashLength is what the collapsed form below spends on the hash: a
// separator plus eight hex characters.
const dbPurgeNameHashLength = 1 + 8

// dbPurgeCronJobName returns the name of the purge CronJob, collapsing an
// over-long Glance name onto a content-stable hash.
//
// Admission bounds metadata.name so the plain "{name}-db-purge" form fits the
// MaxCronJobNameLength characters Kubernetes allows a CronJob name — but on
// create only, and it has to be: metadata.name is immutable, so on update the
// rule could only fire against an object a pre-upgrade operator already
// admitted, including the finalizer-removal update that completes its deletion.
// An operator upgrade therefore inherits Glance CRs the bound would reject, and
// building their name unconditionally would fail every reconcile on an apply the
// API server refuses, forever, with no field left to edit to repair it. Every
// admissible name keeps the documented plain form.
func dbPurgeCronJobName(glanceName string) string {
	if len(glanceName) <= glancev1alpha1.MaxGlanceNameLength {
		return glanceName + dbPurgeNameSuffix
	}
	sum := sha256.Sum256([]byte(glanceName))
	// The trim keeps the truncation from ending a DNS label on "-" or "." —
	// which would make the very name this function exists to produce
	// unapplicable — and only ever shortens, so the cap still holds.
	kept := strings.TrimRight(glanceName[:glancev1alpha1.MaxGlanceNameLength-dbPurgeNameHashLength], "-.")
	return fmt.Sprintf("%s-%x%s", kept, sum[:4], dbPurgeNameSuffix)
}

// effectiveDBPurge returns the settings the purge CronJob runs with,
// materializing the operator defaults for the fields spec.dbPurge leaves unset.
// A nil block behaves exactly like an empty one. It is pure and total: no input
// can fail, so there is no error path, and the result is always fully resolved
// (RetentionDays and PurgeImagesTable non-nil, Schedule non-empty).
//
// Resolving here rather than in the defaulting webhook keeps an unset field
// tracking the operator default across upgrades instead of freezing today's
// value into the stored CR — the same contract effectiveLogging and
// effectiveImportFiltering follow.
func effectiveDBPurge(spec *glancev1alpha1.DBPurgeSpec) glancev1alpha1.DBPurgeSpec {
	var out glancev1alpha1.DBPurgeSpec
	if spec != nil {
		out = *spec
	}
	if out.RetentionDays == nil {
		retention := glancev1alpha1.DefaultDBPurgeRetentionDays
		out.RetentionDays = &retention
	}
	if out.Schedule == "" {
		out.Schedule = glancev1alpha1.DefaultDBPurgeSchedule
	}
	if out.PurgeImagesTable == nil {
		out.PurgeImagesTable = ptr.To(false)
	}
	return out
}

// reconcileDBPurge projects the recurring database-purge CronJob and reports the
// outcome of the most recent run it spawned. The CronJob is ensured on every
// pass — a Glance without spec.dbPurge still gets the task sweep — and
// spec.dbPurge only varies its retention, schedule, suspension, and whether the
// images-table pass is chained on.
//
// Run visibility is derived rather than watched: the CronJob controller spawns
// one Job per firing and prunes them by history limit, so the step lists the
// Jobs carrying this Glance's common labels, keeps the ones the CronJob
// controls, and reports on the newest that reached a terminal state. A failed
// run surfaces as DBPurgeReady=False plus a Warning event; every terminal run
// also feeds the db-purge metric pair exactly once per Job UID.
func (r *GlanceReconciler) reconcileDBPurge(ctx context.Context, children client.Client, glance *glancev1alpha1.Glance, configMapName string) (ctrl.Result, error) {
	purge := effectiveDBPurge(glance.Spec.DBPurge)

	// A failed ensure returns the wrapped error and sets no condition, matching
	// the sibling steps (keystone's trust flush, the shared NetworkPolicy flow):
	// DBPurgeReady then stays absent or stale, which already keeps the aggregate
	// Ready False, and the pipeline attributes the error to this step.
	cronJob := dbPurgeCronJob(glance, configMapName)
	if err := job.EnsureCronJob(ctx, children, r.Scheme, glance, cronJob); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring db purge CronJob: %w", err)
	}

	var jobs batchv1.JobList
	if err := children.List(ctx, &jobs, client.InNamespace(glance.Namespace),
		client.MatchingLabels(commonLabels(glance))); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing db purge Jobs: %w", err)
	}
	// EnsureCronJob decodes the apply response back into cronJob, so it carries
	// the persisted UID the owner-reference match below compares against.
	observed := newestTerminalJob(cronJob, jobs.Items)

	switch {
	// Suspension outranks a failed run, because a suspended CronJob spawns no
	// successor to supersede it: the failed Job stays the newest terminal one
	// for good (the default failedJobsHistoryLimit retains it), so a JobFailed
	// arm that won here would pin DBPurgeReady False — and with it the aggregate
	// Ready and the ControlPlane's mirrored GlanceReady — until someone deleted
	// the Job by hand. The failure is still named in the message; it is just not
	// the state the CR is in.
	case purge.Suspend:
		message := fmt.Sprintf("Database purge CronJob suspended via spec.dbPurge.suspend; no rows are being "+
			"hard-deleted and the soft-deleted backlog keeps growing (schedule %q, retention %d days when resumed)",
			purge.Schedule, *purge.RetentionDays)
		if observed != nil && job.TerminalCondition(observed) == batchv1.JobFailed {
			message += fmt.Sprintf("; the last run (Job %s) failed before the pause", observed.Name)
		}
		conditions.SetCondition(&glance.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBPurgeReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: glance.Generation,
			Reason:             conditionReasonDBPurgeSuspended,
			Message:            message,
		})
	case observed != nil && job.TerminalCondition(observed) == batchv1.JobFailed:
		conditions.SetCondition(&glance.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBPurgeReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: glance.Generation,
			Reason:             conditionReasonDBPurgeJobFailed,
			Message:            fmt.Sprintf("Database purge Job %s failed; the soft-deleted backlog is no longer shrinking", observed.Name),
		})
		r.Recorder.Eventf(glance, corev1.EventTypeWarning, conditionReasonDBPurgeJobFailed,
			"Database purge Job %s failed; inspect its pod logs — the soft-deleted image and task rows keep accumulating until a run succeeds",
			observed.Name)
	default:
		conditions.SetCondition(&glance.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBPurgeReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: glance.Generation,
			Reason:             conditionReasonDBPurgeScheduled,
			Message: fmt.Sprintf("Database purge scheduled %q, hard-deleting rows soft-deleted more than %d days ago",
				purge.Schedule, *purge.RetentionDays),
		})
	}

	// Emit the terminal metrics for the observed run. The shared helper dedupes on
	// the Job UID via an annotation on the Glance CR, so a run is counted once no
	// matter how many passes observe it, and a nil observed (no terminal run yet)
	// is a no-op. The annotation lands on the owner CR, which is why the patch
	// goes through the embedded client rather than children.
	job.RecordJobTerminalState(ctx, r.Client, r.Recorder, glance, "db-purge", observed,
		"DBPurgeMetricEmissionDeferred",
		func(result string, duration time.Duration) {
			metrics.RecordDBPurge(glance.Name, glance.Namespace, result, duration)
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

// dbPurgeCronJob builds the CronJob that hard-deletes the rows glance only ever
// soft-deletes. It mirrors keystone's trust-flush CronJob, with the resolved
// retention and schedule from effectiveDBPurge and the rendered glance-api.conf
// mounted read-only so glance-manage reaches the database.
func dbPurgeCronJob(glance *glancev1alpha1.Glance, configMapName string) *batchv1.CronJob {
	purge := effectiveDBPurge(glance.Spec.DBPurge)
	age := strconv.Itoa(int(*purge.RetentionDays))
	maxRows := strconv.Itoa(dbPurgeMaxRows)

	// `db purge` sweeps the tasks table and the image child tables but
	// deliberately leaves the images table itself alone, so a deleted image's UUID
	// can never be handed out again to a different image (OSSN-0075). Covering the
	// image rows takes the separate `db purge_images_table` pass, which surrenders
	// exactly that protection and is therefore opt-in. It is chained second: the
	// child rows reference the image rows, so purging them first keeps the deletes
	// free of dangling references.
	purgeCmd := "glance-manage --config-dir " + glanceConfigDir + " db purge --age_in_days " + age + " --max_rows " + maxRows
	purgeImagesCmd := "glance-manage --config-dir " + glanceConfigDir + " db purge_images_table --age_in_days " + age + " --max_rows " + maxRows
	script := purgeCmd
	if *purge.PurgeImagesTable {
		script += " && " + purgeImagesCmd
	}

	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dbPurgeCronJobName(glance.Name),
			Namespace: glance.Namespace,
			Labels:    commonLabels(glance),
		},
		Spec: batchv1.CronJobSpec{
			Schedule: purge.Schedule,
			Suspend:  ptr.To(purge.Suspend),
			// Two purges deleting from the same tables contend on the same rows:
			// on Galera the later write-set fails certification, on a single
			// MariaDB it waits out an InnoDB lock timeout. Forbid keeps a run that
			// outlasts its interval from being overtaken by the next firing —
			// without it the per-transaction dbPurgeMaxRows cap buys nothing once
			// the transactions run in parallel, and a wedged run would accumulate
			// one active Job per firing with no ceiling.
			ConcurrencyPolicy: batchv1.ForbidConcurrent,
			JobTemplate: batchv1.JobTemplateSpec{
				// The spawned Jobs inherit these labels, which is what lets the
				// reconcile above list the runs by label instead of by name — the
				// CronJob controller names each Job after the firing timestamp.
				ObjectMeta: metav1.ObjectMeta{
					Labels: commonLabels(glance),
				},
				Spec: batchv1.JobSpec{
					ActiveDeadlineSeconds: ptr.To(dbPurgeActiveDeadlineSeconds),
					Template: corev1.PodTemplateSpec{
						// A NetworkPolicy-enabled deployment selects the purge pods
						// through these labels — they are a superset of the Deployment's
						// selectorLabels — and the auto-derived DB and DNS egress is all
						// glance-manage needs, so no purge-specific rule is required
						// (keystone hit the opposite case: its rotation CronJobs also talk
						// to the apiserver, see reconcile_networkpolicy.go). The component
						// value differs from the one the API pods carry, which is what keeps
						// the purge pods out of the API Service selector.
						ObjectMeta: metav1.ObjectMeta{
							Labels: componentLabels(glance, "db-purge"),
						},
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyOnFailure,
							Containers: []corev1.Container{{
								Name:            "db-purge",
								Image:           glance.Spec.Image.Reference(),
								Command:         []string{"/bin/sh", "-eu", "-c", script},
								SecurityContext: deployment.RestrictedSecurityContext(),
								// Override [database].connection via the oslo.config env-var so
								// the purge reads the DB URL from the derived Secret instead of
								// the ConfigMap.
								Env: []corev1.EnvVar{database.ConnectionEnvVar(glance.Name)},
								VolumeMounts: []corev1.VolumeMount{
									{Name: "config", MountPath: glanceConfigDir, ReadOnly: true},
								},
							}},
							Volumes: []corev1.Volume{{
								Name: "config",
								VolumeSource: corev1.VolumeSource{
									ConfigMap: &corev1.ConfigMapVolumeSource{
										LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
									},
								},
							}},
						},
					},
				},
			},
		},
	}

	// Project the db-tls client keypair when database TLS is enabled: the DSN the
	// purge injects then names ssl_ca/ssl_cert/ssl_key paths under dbTLSMountPath,
	// so without the mount glance-manage fails to open them on every run. The gate
	// is the same glanceDBTLSEnabled the Deployment uses, so both decide
	// identically.
	if glanceDBTLSEnabled(glance) {
		tlsVol, tlsMount := glanceDBTLSVolumeAndMount(glance)
		podSpec := &cronJob.Spec.JobTemplate.Spec.Template.Spec
		podSpec.Volumes = append(podSpec.Volumes, tlsVol)
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, tlsMount)
	}
	return cronJob
}
