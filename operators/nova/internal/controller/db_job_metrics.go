// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	batchv1 "k8s.io/api/batch/v1"

	"github.com/c5c3/cobaltcore/internal/common/job"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/nova/internal/metrics"
)

// dbJobUIDAnnotationKey returns the dedupe annotation key for the DB-related Job
// identified by the given phase suffix ("db-sync", …), via the shared
// job.JobUIDAnnotationKey. The annotation lives on the Nova CR so it survives
// Job deletion; each phase keeps an independent dedupe annotation.
func dbJobUIDAnnotationKey(jobSuffix string) string {
	return job.JobUIDAnnotationKey(jobSuffix)
}

// recordDBJobTerminalState observes the named DB-related Job's terminal
// condition and emits nova_operator_db_sync_total /
// nova_operator_db_sync_duration_seconds exactly once per (Job suffix, Job UID)
// tuple, delegating to the shared job.RecordJobTerminalState. observed is the
// Job the sync or upgrade flow already read this pass, threaded in so this
// function does not re-Get it. It is best-effort: a transient patch failure
// defers emission to the next reconcile and records a
// DBSyncMetricEmissionDeferred Warning event so the degradation is visible via
// `kubectl describe nova`.
func (r *NovaReconciler) recordDBJobTerminalState(ctx context.Context, nova *novav1alpha1.Nova,
	jobSuffix string, observed *batchv1.Job,
) {
	job.RecordJobTerminalState(ctx, r.Client, r.Recorder, nova, jobSuffix, observed,
		"DBSyncMetricEmissionDeferred",
		func(result string, duration time.Duration) {
			metrics.RecordDBSync(nova.Name, nova.Namespace, result, duration)
		})
}
