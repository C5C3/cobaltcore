// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
	"github.com/c5c3/forge/operators/keystone/internal/metrics"
)

// dbJobUIDAnnotationFormat builds the annotation key used to dedupe terminal
// metric emission for a given DB-related Job. The annotation lives on the
// Keystone CR so it survives Job deletion (e.g. when RunJob recreates the Job
// after a PodSpec hash change) and the operator can still skip re-emission
// when the same UID is observed twice.
//
// The %s placeholder is the buildDBJob nameSuffix ("db-sync", "db-expand",
// "db-migrate", "db-contract", "schema-check"). Each phase keeps an
// independent dedupe annotation so metric emission for the expand-migrate-
// contract upgrade Jobs and the post-sync schema-check Job is never silently
// suppressed by an unrelated phase having already been observed.
const dbJobUIDAnnotationFormat = "forge.c5c3.io/last-%s-job-uid"

// dbJobUIDAnnotationKey returns the dedupe annotation key for the Job
// identified by the given nameSuffix (see dbJobUIDAnnotationFormat).
func dbJobUIDAnnotationKey(jobSuffix string) string {
	return fmt.Sprintf(dbJobUIDAnnotationFormat, jobSuffix)
}

// recordDBJobTerminalState observes the named DB-related Job's terminal
// condition and emits keystone_operator_db_sync_total /
// keystone_operator_db_sync_duration_seconds exactly once per (Job suffix, Job
// UID) tuple. The jobSuffix argument is the buildDBJob
// nameSuffix that identifies the phase ("db-sync", "db-expand", "db-migrate",
// "db-contract", "schema-check"); the same suffix selects the per-phase
// dedupe annotation via dbJobUIDAnnotationKey, so terminal emission for one
// phase never silently suppresses another. The function is best-effort: any
// error reading the Job or persisting the UID annotation is swallowed rather
// than surfaced as a reconcile failure, because the metric is an observability
// signal that must not block the sub-reconciler.
//
// Idempotence: the last-observed Job UID is stored as a Keystone CR
// annotation. Re-observing the same UID skips emission; a Job recreated by
// RunJob (e.g. after a PodSpec hash change) carries a fresh UID and therefore
// drives a fresh metric observation. Duration is computed as
// condition.LastTransitionTime minus Job.CreationTimestamp.
//
// Ordering: the dedupe annotation is patched
// BEFORE metrics.RecordDBSync. If the patch fails (transient apiserver error,
// stale-RV conflict), the metric is NOT emitted on this pass — the next
// reconcile re-evaluates the Job and either emits then (after a successful
// patch) or surfaces a single deferred-emission log line. This preserves the
// at-most-once-per-UID guarantee documented in
// docs/reference/keystone-operator-metrics.md against transient apiserver
// failures, at the cost of postponing the observation by one reconcile.
//
// Visibility on persistent patch failure:
// the deferred-emission log is emitted at Info (default level) and a
// Warning event is recorded on the Keystone CR. A persistent apiserver
// Patch failure would otherwise silently degrade db_sync metric emission
// to at-most-once-per-UID-per-successful-patch — invisible at default log
// levels until an alert fires for missing samples. Emitting at Info plus a
// CR-visible event ensures cluster operators notice the degradation
// directly via `kubectl describe keystone` and default-verbosity logs.
func (r *KeystoneReconciler) recordDBJobTerminalState(ctx context.Context, keystone *keystonev1alpha1.Keystone, jobSuffix string) {
	annotationKey := dbJobUIDAnnotationKey(jobSuffix)
	jobName := fmt.Sprintf("%s-%s", keystone.Name, jobSuffix)
	var dbJob batchv1.Job
	if err := r.Get(ctx, types.NamespacedName{
		Name:      jobName,
		Namespace: keystone.Namespace,
	}, &dbJob); err != nil {
		return
	}

	var (
		result       string
		transitionAt metav1.Time
	)
	for _, c := range dbJob.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			result = "succeeded"
			transitionAt = c.LastTransitionTime
		case batchv1.JobFailed:
			result = "failed"
			transitionAt = c.LastTransitionTime
		case batchv1.JobSuspended, batchv1.JobFailureTarget, batchv1.JobSuccessCriteriaMet:
			// Non-terminal transitions: a Suspended Job may be resumed and
			// FailureTarget/SuccessCriteriaMet flip briefly before the
			// terminal JobFailed/JobComplete is set. Ignore here — the
			// metric is emitted only on terminal transitions.
		}
		if result != "" {
			break
		}
	}
	if result == "" {
		return
	}

	uid := string(dbJob.UID)
	if uid != "" && keystone.Annotations[annotationKey] == uid {
		return
	}

	// Persist the observed UID BEFORE recording the metric so a transient
	// patch failure cannot cause double-counting on the next reconcile.
	// Patching the caller's keystone directly
	// would let the client overwrite its in-memory status conditions
	// (accumulated by earlier sub-reconcilers on this pass) with the
	// backend copy, so the patch operates on a separate DeepCopy. The
	// annotation and post-patch ResourceVersion are mirrored back onto the
	// caller's object so a later Status().Update does not hit a stale-RV
	// conflict.
	patchTarget := keystone.DeepCopy()
	patch := client.MergeFrom(patchTarget.DeepCopy())
	if patchTarget.Annotations == nil {
		patchTarget.Annotations = make(map[string]string)
	}
	patchTarget.Annotations[annotationKey] = uid
	if err := r.Patch(ctx, patchTarget, patch); err != nil {
		// Log at Info (default level) and
		// emit a Warning event on the Keystone CR so persistent apiserver
		// Patch failures are visible without raising log verbosity. A
		// silent V(1) line could let the at-most-once-per-UID degradation
		// persist until alerts fire for missing samples.
		log.FromContext(ctx).Info(
			"recordDBJobTerminalState: patching last-observed Job UID failed; "+
				"db_sync metric emission deferred to the next reconcile",
			"keystone", keystone.Name,
			"jobSuffix", jobSuffix,
			"err", err.Error(),
		)
		r.Recorder.Eventf(keystone, corev1.EventTypeWarning, "DBSyncMetricEmissionDeferred",
			"Patching last-observed %s Job UID failed; db_sync metric emission deferred to the next reconcile: %v",
			jobSuffix, err)
		return
	}
	if keystone.Annotations == nil {
		keystone.Annotations = make(map[string]string)
	}
	keystone.Annotations[annotationKey] = uid
	keystone.ResourceVersion = patchTarget.ResourceVersion

	duration := transitionAt.Sub(dbJob.CreationTimestamp.Time)
	if duration < 0 {
		duration = 0
	}
	metrics.RecordDBSync(keystone.Name, keystone.Namespace, result, duration)
}
