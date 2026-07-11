// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	batchv1 "k8s.io/api/batch/v1"

	"github.com/c5c3/forge/internal/common/job"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
	"github.com/c5c3/forge/operators/keystone/internal/metrics"
)

// dbJobUIDAnnotationKey returns the dedupe annotation key for the DB-related Job
// identified by the given phase suffix ("db-sync", "db-expand", …), via the
// shared job.JobUIDAnnotationKey. The annotation lives on the Keystone CR so it
// survives Job deletion; each phase keeps an independent dedupe annotation.
func dbJobUIDAnnotationKey(jobSuffix string) string {
	return job.JobUIDAnnotationKey(jobSuffix)
}

// recordDBJobTerminalState observes the named DB-related Job's terminal
// condition and emits keystone_operator_db_sync_total /
// keystone_operator_db_sync_duration_seconds exactly once per (Job suffix, Job
// UID) tuple, delegating to the shared job.RecordJobTerminalState. The jobSuffix
// identifies the phase ("db-sync", "db-expand", …) and selects the per-phase
// dedupe annotation; observed is the Job RunJob already read this pass, threaded
// in so this function does not re-Get it (issue #361). It is best-effort: a
// transient patch failure defers emission to the next reconcile and records a
// DBSyncMetricEmissionDeferred Warning event so the degradation is visible via
// `kubectl describe keystone`.
func (r *KeystoneReconciler) recordDBJobTerminalState(ctx context.Context, keystone *keystonev1alpha1.Keystone, jobSuffix string, observed *batchv1.Job) {
	job.RecordJobTerminalState(ctx, r.Client, r.Recorder, keystone, jobSuffix, observed,
		"DBSyncMetricEmissionDeferred",
		func(result string, duration time.Duration) {
			metrics.RecordDBSync(keystone.Name, keystone.Namespace, result, duration)
		})
}
