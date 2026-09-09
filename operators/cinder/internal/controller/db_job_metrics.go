// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	batchv1 "k8s.io/api/batch/v1"

	"github.com/c5c3/cobaltcore/internal/common/job"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/cinder/internal/metrics"
)

// dbJobUIDAnnotationKey returns the dedupe annotation key for the DB-related Job
// identified by the given phase suffix ("db-sync", …), via the shared
// job.JobUIDAnnotationKey. The annotation lives on the Cinder CR so it survives
// Job deletion; each phase keeps an independent dedupe annotation.
func dbJobUIDAnnotationKey(jobSuffix string) string {
	return job.JobUIDAnnotationKey(jobSuffix)
}

// recordDBJobTerminalState observes the named DB-related Job's terminal
// condition and emits cinder_operator_db_sync_total /
// cinder_operator_db_sync_duration_seconds exactly once per (Job suffix, Job
// UID) tuple, delegating to the shared job.RecordJobTerminalState. observed is
// the Job the sync or upgrade flow already read this pass, threaded in so this
// function does not re-Get it. It is best-effort: a transient patch failure
// defers emission to the next reconcile and records a
// DBSyncMetricEmissionDeferred Warning event so the degradation is visible via
// `kubectl describe cinder`.
func (r *CinderReconciler) recordDBJobTerminalState(ctx context.Context, cinder *cinderv1alpha1.Cinder,
	jobSuffix string, observed *batchv1.Job,
) {
	job.RecordJobTerminalState(ctx, r.Client, r.Recorder, cinder, jobSuffix, observed,
		"DBSyncMetricEmissionDeferred",
		func(result string, duration time.Duration) {
			metrics.RecordDBSync(cinder.Name, cinder.Namespace, result, duration)
		})
}
