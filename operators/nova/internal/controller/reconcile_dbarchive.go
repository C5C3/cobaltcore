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
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/job"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/nova/internal/metrics"
)

// Condition type and reasons for the recurring database archive. The condition
// is True while the CronJob is projected and the most recent run it spawned did
// not fail; a failed run flips it False until a later run succeeds or the
// archive is suspended. A run that wedges instead of failing cannot hold it True
// either: dbArchiveActiveDeadlineSeconds turns every stalled run into a terminal
// failure.
//
// A suspended archive stays True. Pausing it is an operator's deliberate
// posture, not a failure, but it reports under its own reason, because the
// soft-deleted rows it stops moving keep accumulating in the live tables and
// nothing else says so: the metric simply stops incrementing, which raises no
// alert on its own.
const (
	conditionTypeDBArchiveReady       = "DBArchiveReady"
	conditionReasonDBArchiveScheduled = "DBArchiveScheduled"
	conditionReasonDBArchiveSuspended = "DBArchiveSuspended"
	conditionReasonDBArchiveJobFailed = "DBArchiveJobFailed"
	// conditionReasonDBArchiveMetricEmissionDeferred is an event reason only: it
	// names the Warning job.RecordJobTerminalState raises when it cannot persist
	// the dedupe annotation and therefore leaves the metric to the next pass.
	conditionReasonDBArchiveMetricEmissionDeferred = "DBArchiveMetricEmissionDeferred"
)

// dbArchiveActiveDeadlineSeconds caps how long one run may stay active. It is
// what keeps a wedged archive observable: reconcileDBArchive reports on runs
// that reached a terminal condition, and a pod stuck in ImagePullBackOff or a
// nova-manage blocked on a database lock reaches none on its own. The run would
// stay active forever while DBArchiveReady kept reporting a healthy schedule.
// The deadline turns every such wedge into a JobFailed within the hour,
// surfacing it as DBArchiveReady=False plus a Warning event. A healthy run never
// reaches it: dbArchiveRunBudgetSeconds stops starting batches well before, so a
// backlog larger than one run can move is worked off over several runs instead
// of failing each of them at the deadline.
const dbArchiveActiveDeadlineSeconds int64 = 3600

// dbArchiveRunBudgetSeconds bounds how long one run keeps starting batches. It
// sits ten minutes under dbArchiveActiveDeadlineSeconds, which leaves the batch
// in flight when the budget runs out room to finish before the deadline kills
// the pod: stopping between batches never interrupts a move half done.
const dbArchiveRunBudgetSeconds = dbArchiveActiveDeadlineSeconds - 600

// dbArchiveNameSuffix names the archive CronJob after its Nova. The api package
// mirrors this literal (it derives novav1alpha1.MaxNovaNameLength from it)
// because it cannot import this package.
const dbArchiveNameSuffix = "-db-archive"

// componentDBArchive is the app.kubernetes.io/component value of the archive
// CronJob, of the Jobs it spawns and of their pods. It doubles as the container
// name and as the Job-phase suffix the terminal-metric dedupe annotation is
// keyed on, all three of which name the same workload.
const componentDBArchive = "db-archive"

// Environment variable names the archive script reads its bounds from. They are
// plain shell variables rather than oslo.config OS_<GROUP>__<OPTION> overrides:
// nova-manage takes the three as command-line arguments, and the script is what
// turns them into one.
const (
	dbArchiveMaxRowsEnvVarName       = "MAX_ROWS"
	dbArchiveSleepEnvVarName         = "SLEEP"
	dbArchiveRetentionDaysEnvVarName = "RETENTION_DAYS"
)

// dbArchiveNameHashLength is what the collapsed form below spends on the hash: a
// separator plus eight hex characters.
const dbArchiveNameHashLength = 1 + 8

// dbArchiveScript runs one archive run, a loop of bounded batches, and decides
// what the exit codes mean.
//
// "nova-manage db archive_deleted_rows" answers with a severity rather than with
// a success flag: 0 means nothing was archived, 1 means rows were archived, 2
// means the row cap was invalid, 3 means no API database connection was
// configured, and 4 means --before could not be parsed. Only the first two are
// outcomes of a healthy batch. The loop starts the next batch after a 1, sleeping
// $SLEEP seconds first, and stops on a 0, on anything above 1, or once the run
// budget is spent; the run then exits 0 unless the last batch failed.
//
// The loop is the script's rather than nova-manage's own --until-complete, which
// turns --max_rows into a batch size and runs until the backlog is empty however
// long that takes: on a neglected database that outlasts the active deadline and
// fails the run every day. The script passes neither --purge, which would empty
// the shadow tables the archive promises to keep, nor --sleep, which nova-manage
// only reads together with --until-complete.
//
// --before is assembled only when RETENTION_DAYS is set, so an archive without a
// retention window passes no date at all and every soft-deleted row is eligible.
// --task-log rides along with it and only with it: task_log rows are never
// soft-deleted, so without a date the flag would move the current audit period
// out from under the os-instance_usage_audit_log API, as nova-manage's own help
// warns. The date arithmetic is GNU coreutils' "date -d", which the python-base
// Debian image the nova image builds on ships. "$before" is deliberately
// unquoted: an empty value must add no argument, and a quoted expansion would
// hand nova-manage an empty one.
var dbArchiveScript = `before=""; ` +
	`if [ -n "${RETENTION_DAYS:-}" ]; then ` +
	`before="--before $(date -u -d "-${RETENTION_DAYS} days" +%Y-%m-%d) --task-log"; fi; ` +
	`end=$(( $(date +%s) + ` + strconv.FormatInt(dbArchiveRunBudgetSeconds, 10) + ` )); rc=1; ` +
	`while [ "$rc" -eq 1 ] && [ "$(date +%s)" -lt "$end" ]; do ` +
	`rc=0; nova-manage --config-dir ` + novaConfigDir + ` db archive_deleted_rows ` +
	`--all-cells --max_rows "$MAX_ROWS" $before || rc=$?; ` +
	`if [ "$rc" -eq 1 ]; then sleep "$SLEEP"; fi; ` +
	`done; ` +
	`test "$rc" -le 1`

// dbArchiveCronJobName returns the name of the archive CronJob, collapsing an
// over-long Nova name onto a content-stable hash.
//
// Admission bounds metadata.name so the plain "{name}-db-archive" form fits the
// MaxCronJobNameLength characters Kubernetes allows a CronJob name, but on
// create only, and it has to be: metadata.name is immutable, so on update the
// rule could only fire against an object a pre-upgrade operator already
// admitted, including the finalizer-removal update that completes its deletion.
// An operator upgrade therefore inherits Nova CRs the bound would reject, and
// building their name unconditionally would fail every reconcile on an apply the
// API server refuses, forever, with no field left to edit to repair it. Every
// admissible name keeps the documented plain form.
func dbArchiveCronJobName(novaName string) string {
	if len(novaName) <= novav1alpha1.MaxNovaNameLength {
		return novaName + dbArchiveNameSuffix
	}
	sum := sha256.Sum256([]byte(novaName))
	// The trim keeps the truncation from ending a DNS label on "-" or ".", which
	// would make the very name this function exists to produce unapplicable, and
	// only ever shortens, so the cap still holds.
	kept := strings.TrimRight(novaName[:novav1alpha1.MaxNovaNameLength-dbArchiveNameHashLength], "-.")
	return fmt.Sprintf("%s-%x%s", kept, sum[:4], dbArchiveNameSuffix)
}

// effectiveDBArchive returns the settings the archive CronJob runs with,
// materializing the operator defaults for the fields spec.dbArchive leaves
// unset. A nil block behaves exactly like an empty one. It is pure and total: no
// input can fail, so there is no error path.
//
// RetentionDays is the one field with no default. Unset means no --before at
// all, so resolving it to a number here would silently hold back rows the
// contract says are eligible.
//
// Resolving at reconcile time rather than in the defaulting webhook keeps an
// unset field tracking the operator default across upgrades instead of freezing
// today's value into the stored CR.
func effectiveDBArchive(spec *novav1alpha1.DBArchiveSpec) novav1alpha1.DBArchiveSpec {
	var out novav1alpha1.DBArchiveSpec
	if spec != nil {
		out = *spec
	}
	if out.Schedule == "" {
		out.Schedule = novav1alpha1.DefaultDBArchiveSchedule
	}
	out.MaxRows = ptr.To(ptr.Deref(out.MaxRows, novav1alpha1.DefaultDBArchiveMaxRows))
	out.Sleep = ptr.To(ptr.Deref(out.Sleep, novav1alpha1.DefaultDBArchiveSleep))
	return out
}

// reconcileDBArchive projects the recurring database-archive CronJob and reports
// the outcome of the most recent run it spawned. The CronJob is ensured on every
// pass, so a Nova without spec.dbArchive still gets the sweep; spec.dbArchive
// only varies its schedule, its bounds, its retention window and its suspension.
//
// Run visibility is derived rather than watched: the CronJob controller spawns
// one Job per firing and prunes them by history limit, so the step lists the
// Jobs carrying this archive's labels, keeps the ones the CronJob controls, and
// reports on the newest that reached a terminal state. A failed run surfaces as
// DBArchiveReady=False plus a Warning event; every terminal run also feeds the
// db-archive metric pair exactly once per Job UID.
func (r *NovaReconciler) reconcileDBArchive(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova, art configArtifacts,
) (ctrl.Result, error) {
	archive := effectiveDBArchive(nova.Spec.DBArchive)

	// A failed ensure returns the wrapped error and sets no condition, matching
	// the sibling steps: DBArchiveReady then stays absent or stale, which already
	// keeps the aggregate Ready False, and the pipeline attributes the error to
	// this step.
	cronJob := dbArchiveCronJob(nova, art)
	if err := job.EnsureCronJob(ctx, children, r.Scheme, nova, cronJob); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring db-archive CronJob: %w", err)
	}

	var jobs batchv1.JobList
	if err := children.List(ctx, &jobs, client.InNamespace(nova.Namespace),
		client.MatchingLabels(componentLabels(nova, componentDBArchive))); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing db-archive Jobs: %w", err)
	}
	// EnsureCronJob decodes the apply response back into cronJob, so it carries
	// the persisted UID the owner-reference match below compares against.
	observed := newestTerminalJob(cronJob, jobs.Items)

	switch {
	// Suspension outranks a failed run, because a suspended CronJob spawns no
	// successor to supersede it: the failed Job stays the newest terminal one for
	// good (the default failedJobsHistoryLimit retains it), so a JobFailed arm
	// that won here would pin DBArchiveReady False, and with it the aggregate
	// Ready, until someone deleted the Job by hand. The failure is still named in
	// the message; it is just not the state the CR is in.
	case archive.Suspend:
		message := fmt.Sprintf("Database archive CronJob suspended via spec.dbArchive.suspend; no rows are being "+
			"moved into the shadow tables and the soft-deleted backlog keeps growing (schedule %q, "+
			"%d rows per table per batch when resumed)", archive.Schedule, *archive.MaxRows)
		if observed != nil && job.TerminalCondition(observed) == batchv1.JobFailed {
			message += fmt.Sprintf("; the last run (Job %s) failed before the pause", observed.Name)
		}
		conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBArchiveReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: nova.Generation,
			Reason:             conditionReasonDBArchiveSuspended,
			Message:            message,
		})
	case observed != nil && job.TerminalCondition(observed) == batchv1.JobFailed:
		conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBArchiveReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: nova.Generation,
			Reason:             conditionReasonDBArchiveJobFailed,
			Message: fmt.Sprintf("Database archive Job %s failed; the soft-deleted rows are no longer moving "+
				"out of the live tables", observed.Name),
		})
		r.Recorder.Eventf(nova, corev1.EventTypeWarning, conditionReasonDBArchiveJobFailed,
			"Database archive Job %s failed; inspect its pod logs. The soft-deleted instance rows keep "+
				"accumulating in the live tables until a run succeeds", observed.Name)
	default:
		conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBArchiveReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: nova.Generation,
			Reason:             conditionReasonDBArchiveScheduled,
			Message: fmt.Sprintf("Database archive scheduled %q, moving up to %d soft-deleted rows per table "+
				"per batch into the shadow tables%s", archive.Schedule, *archive.MaxRows,
				dbArchiveRetentionMessage(archive.RetentionDays)),
		})
	}

	// Emit the terminal metrics for the observed run. The shared helper dedupes on
	// the Job UID via an annotation on the Nova CR, so a run is counted once no
	// matter how many passes observe it, and a nil observed (no terminal run yet)
	// is a no-op. The annotation lands on the owner CR, which is why the patch
	// goes through the embedded client rather than children.
	job.RecordJobTerminalState(ctx, r.Client, r.Recorder, nova, componentDBArchive, observed,
		conditionReasonDBArchiveMetricEmissionDeferred,
		func(result string, duration time.Duration) {
			metrics.RecordDBArchive(nova.Name, nova.Namespace, result, duration)
		})

	return ctrl.Result{}, nil
}

// dbArchiveRetentionMessage renders the retention half of the scheduled message,
// or the empty string when no window is configured. An unset window is not a
// default the message could name: it means every soft-deleted row is eligible.
func dbArchiveRetentionMessage(retentionDays *int32) string {
	if retentionDays == nil {
		return ""
	}
	return fmt.Sprintf(", keeping rows deleted in the last %d days in place", *retentionDays)
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

// dbArchiveCronJob builds the CronJob that moves the rows nova only ever
// soft-deletes into their shadow twins. The schedule, the per-table row cap, the
// sleep between batches and the retention window come from effectiveDBArchive,
// and the rendered config is mounted read-only so nova-manage reaches both
// schemas.
//
// The whole ConfigMap is mounted rather than a per-key subset, the way the
// migration Jobs mount it: nova-manage needs no file selection, and the role
// overlays it sees beside nova.conf set worker counts and listen addresses it
// does not read.
func dbArchiveCronJob(nova *novav1alpha1.Nova, art configArtifacts) *batchv1.CronJob {
	archive := effectiveDBArchive(nova.Spec.DBArchive)

	// The same environment every Nova process runs with, so the archive reads both
	// database URLs from the derived Secrets instead of the placeholders in the
	// mounted config, plus the three bounds the script turns into arguments.
	// RETENTION_DAYS is absent while no window is set, which is what the script
	// tests for.
	env := append(novaWorkloadEnv(nova, roleManage),
		corev1.EnvVar{Name: dbArchiveMaxRowsEnvVarName, Value: strconv.Itoa(int(*archive.MaxRows))},
		corev1.EnvVar{Name: dbArchiveSleepEnvVarName, Value: strconv.Itoa(int(*archive.Sleep))})
	if archive.RetentionDays != nil {
		env = append(env, corev1.EnvVar{
			Name:  dbArchiveRetentionDaysEnvVarName,
			Value: strconv.Itoa(int(*archive.RetentionDays)),
		})
	}

	volumes := []corev1.Volume{
		{
			Name: configVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: art.configMapName},
				},
			},
		},
		{
			Name:         tmpVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}
	mounts := []corev1.VolumeMount{
		{Name: configVolumeName, MountPath: novaConfigDir, ReadOnly: true},
		{Name: tmpVolumeName, MountPath: tmpMountPath},
	}
	// The DSNs the archive injects name ssl_ca/ssl_cert/ssl_key paths under the
	// per-schema mount points while database TLS is on, so without the projection
	// nova-manage fails to open them on every run. The gate is the one the
	// workloads use, so both decide identically.
	tlsVolumes, tlsMounts := novaDBTLSVolumesAndMounts(nova)
	volumes = append(volumes, tlsVolumes...)
	mounts = append(mounts, tlsMounts...)

	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dbArchiveCronJobName(nova.Name),
			Namespace: nova.Namespace,
			Labels:    componentLabels(nova, componentDBArchive),
		},
		Spec: batchv1.CronJobSpec{
			Schedule: archive.Schedule,
			Suspend:  ptr.To(archive.Suspend),
			// Two archives moving the same rows contend on the same tables: on
			// Galera the later write-set fails certification, on a single MariaDB it
			// waits out an InnoDB lock timeout. Forbid keeps a run that outlasts its
			// interval from being overtaken by the next firing, which is what a first
			// run against a long-neglected backlog does.
			ConcurrencyPolicy: batchv1.ForbidConcurrent,
			JobTemplate: batchv1.JobTemplateSpec{
				// The spawned Jobs inherit these labels, which is what lets the
				// reconcile above list the runs by label instead of by name: the
				// CronJob controller names each Job after the firing timestamp.
				ObjectMeta: metav1.ObjectMeta{
					Labels: componentLabels(nova, componentDBArchive),
				},
				Spec: batchv1.JobSpec{
					ActiveDeadlineSeconds: ptr.To(dbArchiveActiveDeadlineSeconds),
					Template: corev1.PodTemplateSpec{
						// The component value differs from the one the API pods carry,
						// which is what keeps the archive pods out of the API Service
						// selector. A NetworkPolicy-enabled deployment still selects them:
						// its pod selector carries no component key, so the DNS, bus and
						// database egress nova-manage needs is already open.
						ObjectMeta: metav1.ObjectMeta{
							Labels: componentLabels(nova, componentDBArchive),
						},
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyOnFailure,
							// FSGroup makes the kubelet group-own the projected database TLS
							// files by the openstack GID. They are projected at mode 0400, so
							// without it they stay root:root and nova-manage, running as the
							// openstack UID, cannot read the client key it connects with. The
							// long-lived workloads get the same pod context from
							// deployment.BuildWorkload.
							SecurityContext: &corev1.PodSecurityContext{FSGroup: ptr.To(deployment.OpenStackUID)},
							Containers: []corev1.Container{{
								Name:            componentDBArchive,
								Image:           nova.Spec.Image.Reference(),
								Command:         []string{"/bin/sh", "-eu", "-c", dbArchiveScript},
								SecurityContext: deployment.RestrictedSecurityContext(),
								Env:             env,
								VolumeMounts:    mounts,
							}},
							Volumes: volumes,
						},
					},
				},
			},
		},
	}
	novaJobPod(nova).Apply(&cronJob.Spec.JobTemplate.Spec.Template.Spec)
	return cronJob
}
