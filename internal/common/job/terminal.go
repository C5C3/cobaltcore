// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package job

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// JobUIDAnnotationFormat builds the annotation key used to dedupe terminal
// metric emission for a migration Job. The annotation lives on the owner CR so
// it survives Job deletion (for example when a job runner recreates the Job
// after a PodSpec hash change) and the operator can still skip re-emission when
// the same UID is observed twice. The %s placeholder is the Job phase suffix
// ("db-sync", "db-expand", …); each phase keeps an independent dedupe
// annotation so terminal emission for one phase never silently suppresses
// another.
const JobUIDAnnotationFormat = "forge.c5c3.io/last-%s-job-uid"

// JobUIDAnnotationKey returns the dedupe annotation key for the Job identified
// by the given phase suffix.
func JobUIDAnnotationKey(jobSuffix string) string {
	return fmt.Sprintf(JobUIDAnnotationFormat, jobSuffix)
}

// RecordJobTerminalState observes the given migration Job's terminal condition
// and invokes record exactly once per (jobSuffix, Job UID) tuple, deduping via
// a per-phase annotation on owner. record receives the terminal result
// ("succeeded"/"failed") and the observed duration (LastTransitionTime minus
// Job CreationTimestamp, clamped at zero), so the caller can emit its
// service-specific metric.
//
// observed is the Job the caller already read this pass, threaded in so this
// function does not re-Get it; a nil observed (a just-created Job with no
// terminal condition) is a no-op.
//
// Ordering: the dedupe annotation is patched BEFORE record is called. If the
// patch fails (transient apiserver error, stale-RV conflict), record is NOT
// invoked this pass — the caller re-evaluates next reconcile — preserving the
// at-most-once-per-UID guarantee against transient failures. On a patch failure
// the function logs at Info and, when recorder is non-nil, records a Warning
// event with deferEventReason on owner so persistent failures are visible via
// `kubectl describe` without raising log verbosity. It is best-effort: errors
// are swallowed rather than surfaced as a reconcile failure.
func RecordJobTerminalState(
	ctx context.Context,
	c client.Client,
	recorder record.EventRecorder,
	owner client.Object,
	jobSuffix string,
	observed *batchv1.Job,
	deferEventReason string,
	record func(result string, duration time.Duration),
) {
	// A just-created Job (the runner returned nil) has no terminal condition.
	if observed == nil {
		return
	}
	annotationKey := JobUIDAnnotationKey(jobSuffix)

	var (
		result       string
		transitionAt metav1.Time
	)
	for _, cond := range observed.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case batchv1.JobComplete:
			result = "succeeded"
			transitionAt = cond.LastTransitionTime
		case batchv1.JobFailed:
			result = "failed"
			transitionAt = cond.LastTransitionTime
		case batchv1.JobSuspended, batchv1.JobFailureTarget, batchv1.JobSuccessCriteriaMet:
			// Non-terminal transitions: a Suspended Job may be resumed and
			// FailureTarget/SuccessCriteriaMet flip briefly before the terminal
			// JobFailed/JobComplete is set. Ignore — emit only on terminal
			// transitions.
		}
		if result != "" {
			break
		}
	}
	if result == "" {
		return
	}

	uid := string(observed.UID)
	if uid != "" && owner.GetAnnotations()[annotationKey] == uid {
		return
	}

	// Persist the observed UID BEFORE recording so a transient patch failure
	// cannot cause double-counting on the next reconcile. Patch a DeepCopy so
	// the client does not overwrite the caller's in-memory status conditions
	// (accumulated by earlier sub-reconcilers this pass) with the backend copy;
	// the annotation and post-patch ResourceVersion are mirrored back onto the
	// caller's object so a later Status().Update does not hit a stale-RV
	// conflict.
	patchTarget, ok := owner.DeepCopyObject().(client.Object)
	if !ok {
		return
	}
	patchBase, ok := patchTarget.DeepCopyObject().(client.Object)
	if !ok {
		return
	}
	patch := client.MergeFrom(patchBase)
	anns := patchTarget.GetAnnotations()
	if anns == nil {
		anns = make(map[string]string)
	}
	anns[annotationKey] = uid
	patchTarget.SetAnnotations(anns)
	if err := c.Patch(ctx, patchTarget, patch); err != nil {
		log.FromContext(ctx).Info(
			"RecordJobTerminalState: patching last-observed Job UID failed; "+
				"metric emission deferred to the next reconcile",
			"owner", owner.GetName(),
			"jobSuffix", jobSuffix,
			"err", err.Error(),
		)
		if recorder != nil {
			recorder.Eventf(owner, corev1.EventTypeWarning, deferEventReason,
				"Patching last-observed %s Job UID failed; metric emission deferred to the next reconcile: %v",
				jobSuffix, err)
		}
		return
	}
	ownerAnns := owner.GetAnnotations()
	if ownerAnns == nil {
		ownerAnns = make(map[string]string)
	}
	ownerAnns[annotationKey] = uid
	owner.SetAnnotations(ownerAnns)
	owner.SetResourceVersion(patchTarget.GetResourceVersion())

	duration := transitionAt.Sub(observed.CreationTimestamp.Time)
	if duration < 0 {
		duration = 0
	}
	record(result, duration)
}
