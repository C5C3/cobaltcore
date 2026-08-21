// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	batchv1 "k8s.io/api/batch/v1"

	"github.com/c5c3/cobaltcore/internal/common/job"
	placementv1alpha1 "github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1"
	placementmetrics "github.com/c5c3/cobaltcore/operators/placement/internal/metrics"
)

// dbJobUIDAnnotationKey returns the dedupe annotation key for the DB-related Job
// identified by the given phase suffix ("db-sync"), via the shared
// job.JobUIDAnnotationKey. The annotation lives on the Placement CR so it
// survives Job deletion.
func dbJobUIDAnnotationKey(jobSuffix string) string {
	return job.JobUIDAnnotationKey(jobSuffix)
}

// recordDBJobTerminalState observes the named DB-related Job's terminal
// condition and emits placement_operator_db_sync_total /
// placement_operator_db_sync_duration_seconds exactly once per (Job suffix, Job
// UID) tuple, delegating to the shared job.RecordJobTerminalState. observed is
// the Job ReconcileSyncJobs already read this pass, threaded in so this function
// does not re-Get it. It is best-effort: a transient patch failure defers
// emission to the next reconcile and records a DBSyncMetricEmissionDeferred
// Warning event so the degradation is visible via `kubectl describe placement`.
func (r *PlacementReconciler) recordDBJobTerminalState(ctx context.Context, placement *placementv1alpha1.Placement, jobSuffix string, observed *batchv1.Job) {
	job.RecordJobTerminalState(ctx, r.Client, r.Recorder, placement, jobSuffix, observed,
		"DBSyncMetricEmissionDeferred",
		func(result string, duration time.Duration) {
			placementmetrics.RecordDBSync(placement.Name, placement.Namespace, result, duration)
		})
}
