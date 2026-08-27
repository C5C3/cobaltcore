// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	batchv1 "k8s.io/api/batch/v1"

	"github.com/c5c3/cobaltcore/internal/common/job"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	neutronmetrics "github.com/c5c3/cobaltcore/operators/neutron/internal/metrics"
)

// dbJobUIDAnnotationKey returns the dedupe annotation key for the DB-related Job
// identified by the given phase suffix ("db-sync", "db-expand", …), via the
// shared job.JobUIDAnnotationKey. The annotation lives on the Neutron CR so it
// survives Job deletion; each phase keeps an independent dedupe annotation.
func dbJobUIDAnnotationKey(jobSuffix string) string {
	return job.JobUIDAnnotationKey(jobSuffix)
}

// recordDBJobTerminalState observes the named DB-related Job's terminal
// condition and emits neutron_operator_db_sync_total /
// neutron_operator_db_sync_duration_seconds exactly once per (Job suffix, Job
// UID) tuple, delegating to the shared job.RecordJobTerminalState. The jobSuffix
// identifies the phase ("db-sync", "db-expand", "db-migrate", "db-contract") and
// selects the per-phase dedupe annotation; observed is the Job the database step
// already read this pass, threaded in so this function does not re-Get it. It is
// best-effort: a transient patch failure defers emission to the next reconcile
// and records a DBSyncMetricEmissionDeferred Warning event so the degradation is
// visible via `kubectl describe neutron`.
func (r *NeutronReconciler) recordDBJobTerminalState(ctx context.Context, neutron *neutronv1alpha1.Neutron, jobSuffix string, observed *batchv1.Job) {
	job.RecordJobTerminalState(ctx, r.Client, r.Recorder, neutron, jobSuffix, observed,
		"DBSyncMetricEmissionDeferred",
		func(result string, duration time.Duration) {
			neutronmetrics.RecordDBSync(neutron.Name, neutron.Namespace, result, duration)
		})
}
