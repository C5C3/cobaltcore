// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	batchv1 "k8s.io/api/batch/v1"

	"github.com/c5c3/forge/internal/common/job"
	barbicanv1alpha1 "github.com/c5c3/forge/operators/barbican/api/v1alpha1"
	barbicanmetrics "github.com/c5c3/forge/operators/barbican/internal/metrics"
)

// dbJobUIDAnnotationKey returns the dedupe annotation key for the DB-related Job
// identified by the given phase suffix ("db-sync"), via the shared
// job.JobUIDAnnotationKey. The annotation lives on the Barbican CR so it
// survives Job deletion.
func dbJobUIDAnnotationKey(jobSuffix string) string {
	return job.JobUIDAnnotationKey(jobSuffix)
}

// recordDBJobTerminalState observes the named DB-related Job's terminal
// condition and emits barbican_operator_db_sync_total /
// barbican_operator_db_sync_duration_seconds exactly once per (Job suffix, Job
// UID) tuple, delegating to the shared job.RecordJobTerminalState. observed is
// the Job ReconcileSyncJobs already read this pass, threaded in so this function
// does not re-Get it. It is best-effort: a transient patch failure defers
// emission to the next reconcile and records a DBSyncMetricEmissionDeferred
// Warning event so the degradation is visible via `kubectl describe barbican`.
func (r *BarbicanReconciler) recordDBJobTerminalState(ctx context.Context, barbican *barbicanv1alpha1.Barbican, jobSuffix string, observed *batchv1.Job) {
	job.RecordJobTerminalState(ctx, r.Client, r.Recorder, barbican, jobSuffix, observed,
		"DBSyncMetricEmissionDeferred",
		func(result string, duration time.Duration) {
			barbicanmetrics.RecordDBSync(barbican.Name, barbican.Namespace, result, duration)
		})
}
