// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
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
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	neutronmetrics "github.com/c5c3/cobaltcore/operators/neutron/internal/metrics"
)

// Condition type and reasons for the recurring OVN database synchronisation. The
// condition is True while the CronJob is projected and the most recent run it
// spawned did not fail; a failed run flips it False until a later run succeeds
// or the sync is suspended. A run that wedges instead of failing cannot hold it
// True either: ovnDBSyncActiveDeadlineSeconds turns every stalled run into a
// terminal failure.
//
// A CR without spec.ovnDBSync reports True as well: no CronJob is the configured
// state, not a degraded one, and the condition has to resolve or the aggregate
// Ready never does.
const (
	conditionTypeOVNDBSyncReady         = "OVNDBSyncReady"
	conditionReasonOVNDBSyncNotRequired = "OVNDBSyncNotRequired"
	conditionReasonOVNDBSyncScheduled   = "OVNDBSyncScheduled"
	conditionReasonOVNDBSyncSuspended   = "OVNDBSyncSuspended"
	conditionReasonOVNDBSyncJobFailed   = "OVNDBSyncJobFailed"
)

// componentOVNDBSync is the component label value of the sync CronJob and the
// Jobs it spawns, the container name inside them, and the suffix the
// terminal-metric dedupe annotation is keyed on. It differs from the API
// component, which is what keeps the sync pods out of the API Service selector.
const componentOVNDBSync = "ovn-db-sync"

// ovnDBSyncNameSuffix is appended to metadata.name to name the sync CronJob. The
// api package mirrors this literal (it derives
// neutronv1alpha1.MaxNeutronNameLength from it) because it cannot import this
// package, and the validating webhook bounds metadata.name against that
// derivation, so the plain form always fits the API server's CronJob name cap.
const ovnDBSyncNameSuffix = "-ovn-db-sync"

// ovnDBSyncActiveDeadlineSeconds caps how long one run may stay active. It is
// what keeps a wedged sync observable: this step reports on runs that reached a
// terminal condition, and a pod stuck in ImagePullBackOff or a sync util blocked
// on an unresponsive Northbound reaches none on its own, so the run would stay
// active forever while OVNDBSyncReady kept reporting a healthy schedule. The
// deadline turns every such wedge into a JobFailed within the hour. An hour is
// far above the runtime of a comparison of both databases.
const ovnDBSyncActiveDeadlineSeconds int64 = 3600

// ovnDBSyncCronJobName returns the name of the sync CronJob for the given
// Neutron.
func ovnDBSyncCronJobName(neutron *neutronv1alpha1.Neutron) string {
	return neutron.Name + ovnDBSyncNameSuffix
}

// effectiveOVNDBSync returns the settings the sync CronJob runs with,
// materializing the operator defaults for the fields spec.ovnDBSync leaves
// unset. A nil block resolves like an empty one; deciding whether a CronJob
// exists at all is the caller's, because a nil block means no CronJob rather
// than a defaulted one.
//
// Resolving here rather than in the defaulting webhook keeps an unset field
// tracking the operator default across upgrades instead of freezing today's
// value into the stored CR, the same contract effectiveLogging follows.
func effectiveOVNDBSync(spec *neutronv1alpha1.OVNDBSyncSpec) neutronv1alpha1.OVNDBSyncSpec {
	var out neutronv1alpha1.OVNDBSyncSpec
	if spec != nil {
		out = *spec
	}
	if out.Schedule == "" {
		out.Schedule = neutronv1alpha1.DefaultOVNDBSyncSchedule
	}
	if out.SyncMode == "" {
		out.SyncMode = neutronv1alpha1.DefaultOVNDBSyncMode
	}
	return out
}

// reconcileOVNDBSync projects the recurring OVN database-synchronisation CronJob
// and reports the outcome of the most recent run it spawned. A nil
// spec.ovnDBSync deletes any CronJob a previous spec created and reports the
// absence as configured: the sync reads, and in repair mode rewrites, the entire
// logical model, so scheduling it is a deliberate choice.
//
// Run visibility is derived rather than watched: the CronJob controller spawns
// one Job per firing and prunes them by history limit, so the step lists the Jobs
// carrying this CR's sync component labels, keeps the ones the CronJob controls,
// and reports on the newest that reached a terminal state. A failed run surfaces
// as OVNDBSyncReady=False plus a Warning event; every terminal run also feeds the
// ovn-db-sync metric pair exactly once per Job UID.
//
// The condition reports Job outcomes only, never drift. In log mode the utility
// exits 0 whether or not the two databases agree, so a run that found the
// Northbound out of step is indistinguishable here from one that found it
// consistent. The report lives in the run's log.
func (r *NeutronReconciler) reconcileOVNDBSync(ctx context.Context, children client.Client,
	neutron *neutronv1alpha1.Neutron, configMapName string,
) (ctrl.Result, error) {
	if neutron.Spec.OVNDBSync == nil {
		if err := job.DeleteCronJob(ctx, children, neutron.Namespace, ovnDBSyncCronJobName(neutron)); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting ovn-db-sync CronJob: %w", err)
		}
		conditions.SetCondition(&neutron.Status.Conditions, metav1.Condition{
			Type:               conditionTypeOVNDBSyncReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: neutron.Generation,
			Reason:             conditionReasonOVNDBSyncNotRequired,
			Message:            "No OVN database synchronisation is scheduled: spec.ovnDBSync is unset",
		})
		return ctrl.Result{}, nil
	}

	sync := effectiveOVNDBSync(neutron.Spec.OVNDBSync)

	// A failed ensure returns the wrapped error and sets no condition, matching
	// the sibling steps: OVNDBSyncReady then stays absent or stale, which already
	// keeps the aggregate Ready False, and the pipeline attributes the error to
	// this step.
	cronJob := buildOVNDBSyncCronJob(neutron, configMapName)
	if err := job.EnsureCronJob(ctx, children, r.Scheme, neutron, cronJob); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring ovn-db-sync CronJob: %w", err)
	}

	var jobs batchv1.JobList
	if err := children.List(ctx, &jobs, client.InNamespace(neutron.Namespace),
		client.MatchingLabels(componentLabels(neutron, componentOVNDBSync))); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing ovn-db-sync Jobs: %w", err)
	}
	// EnsureCronJob decodes the apply response back into cronJob, so it carries
	// the persisted UID the owner-reference match below compares against.
	observed := newestTerminalJob(cronJob, jobs.Items)

	switch {
	// Suspension outranks a failed run, because a suspended CronJob spawns no
	// successor to supersede it: the failed Job stays the newest terminal one for
	// good, so a JobFailed arm that won here would pin OVNDBSyncReady False — and
	// with it the aggregate Ready — until someone deleted the Job by hand. The
	// failure is still named in the message; it is just not the state the CR is in.
	case sync.Suspend:
		message := fmt.Sprintf("OVN database synchronisation suspended via spec.ovnDBSync.suspend; the Neutron "+
			"database and the OVN Northbound database are not being compared (schedule %q, mode %q when resumed)",
			sync.Schedule, sync.SyncMode)
		if observed != nil && job.TerminalCondition(observed) == batchv1.JobFailed {
			message += fmt.Sprintf("; the last run (Job %s) failed before the pause", observed.Name)
		}
		conditions.SetCondition(&neutron.Status.Conditions, metav1.Condition{
			Type:               conditionTypeOVNDBSyncReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: neutron.Generation,
			Reason:             conditionReasonOVNDBSyncSuspended,
			Message:            message,
		})
	case observed != nil && job.TerminalCondition(observed) == batchv1.JobFailed:
		conditions.SetCondition(&neutron.Status.Conditions, metav1.Condition{
			Type:               conditionTypeOVNDBSyncReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: neutron.Generation,
			Reason:             conditionReasonOVNDBSyncJobFailed,
			Message: fmt.Sprintf("OVN database synchronisation Job %s failed; the Neutron database and the OVN "+
				"Northbound database are no longer being compared. A %q line in its log means the Northbound at that "+
				"address was unreachable, not that the two databases disagree",
				observed.Name, "Could not retrieve schema from <address>"),
		})
		r.Recorder.Eventf(neutron, corev1.EventTypeWarning, conditionReasonOVNDBSyncJobFailed,
			"OVN database synchronisation Job %s failed; inspect its pod logs. A %q line means the Northbound "+
				"database was unreachable rather than out of step",
			observed.Name, "Could not retrieve schema from <address>")
	default:
		conditions.SetCondition(&neutron.Status.Conditions, metav1.Condition{
			Type:               conditionTypeOVNDBSyncReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: neutron.Generation,
			Reason:             conditionReasonOVNDBSyncScheduled,
			Message: fmt.Sprintf("OVN database synchronisation scheduled %q in %q mode",
				sync.Schedule, sync.SyncMode),
		})
	}

	// Emit the terminal metrics for the observed run. The shared helper dedupes on
	// the Job UID via an annotation on the Neutron CR, so a run is counted once no
	// matter how many passes observe it, and a nil observed (no terminal run yet)
	// is a no-op. The annotation lands on the owner CR, which is why the patch goes
	// through the embedded client rather than children.
	job.RecordJobTerminalState(ctx, r.Client, r.Recorder, neutron, componentOVNDBSync, observed,
		"OVNDBSyncMetricEmissionDeferred",
		func(result string, duration time.Duration) {
			neutronmetrics.RecordOVNDBSync(neutron.Name, neutron.Namespace, result, duration)
		})

	return ctrl.Result{}, nil
}

// newestTerminalJob returns the most recently created Job among jobs that the
// given CronJob controls and that has reached a terminal state, or nil when no
// such Job exists yet. Newest wins because the CronJob keeps a bounded history:
// an older failed run must not keep the condition False once a later run has
// succeeded. The controller reference is what separates a scheduled run from a
// Job someone created from the CronJob by hand, which carries the same labels
// but no controller.
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

// buildOVNDBSyncCronJob builds the CronJob that runs neutron-ovn-db-sync-util
// against the same two config files the API pods read. In log mode the utility
// reports what the Neutron database and the OVN Northbound database disagree on;
// in repair mode it deletes the Northbound objects Neutron does not know about
// and creates the ones it is missing.
func buildOVNDBSyncCronJob(neutron *neutronv1alpha1.Neutron, configMapName string) *batchv1.CronJob {
	sync := effectiveOVNDBSync(neutron.Spec.OVNDBSync)
	volumes, mounts := neutronWorkloadVolumes(neutron, configMapName)
	labels := componentLabels(neutron, componentOVNDBSync)

	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ovnDBSyncCronJobName(neutron),
			Namespace: neutron.Namespace,
			Labels:    commonLabels(neutron),
		},
		Spec: batchv1.CronJobSpec{
			Schedule: sync.Schedule,
			Suspend:  ptr.To(sync.Suspend),
			// Two runs walk the same two databases and, in repair mode, write the
			// second one: the later run would undo what the earlier is halfway through.
			// Forbid keeps a run that outlasts its interval from being overtaken by the
			// next firing, and keeps a wedged run from accumulating one active Job per
			// firing with no ceiling.
			ConcurrencyPolicy: batchv1.ForbidConcurrent,
			JobTemplate: batchv1.JobTemplateSpec{
				// The spawned Jobs inherit these labels, which is what lets the reconcile
				// above list the runs by label instead of by name — the CronJob controller
				// names each Job after the firing timestamp.
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: batchv1.JobSpec{
					ActiveDeadlineSeconds: ptr.To(ovnDBSyncActiveDeadlineSeconds),
					// A retry would walk the same unreachable Northbound and produce the same
					// failure minutes later, while the condition kept reporting a run in
					// flight. The next firing is the retry.
					BackoffLimit: ptr.To(int32(0)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							Containers: []corev1.Container{{
								Name:            componentOVNDBSync,
								Image:           neutron.Spec.Image.Reference(),
								Command:         neutronCommand("neutron-ovn-db-sync-util", "--ovn-neutron_sync_mode", sync.SyncMode),
								SecurityContext: deployment.RestrictedSecurityContext(),
								Env:             neutronWorkloadEnv(neutron),
								VolumeMounts:    mounts,
							}},
							Volumes: volumes,
						},
					},
				},
			},
		},
	}
}
