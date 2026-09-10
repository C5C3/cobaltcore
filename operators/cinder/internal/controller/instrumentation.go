// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Sub-reconciler instrumentation helper for the Cinder controllers.
package controller

import (
	"fmt"

	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/c5c3/cobaltcore/internal/common/instrumentation"
	"github.com/c5c3/cobaltcore/operators/cinder/internal/metrics"
)

// subReconcilerConditionTypes maps a sub_reconciler label value to the
// condition_type it drives. The instrumenter consults this map to attribute
// errors to the correct Ready sub-condition.
//
// The mapping is guarded in both directions: every value MUST be a member of
// subConditionTypes and every pipeline step name MUST be a key here. If a
// sub_reconciler name reaches the instrumenter without a key, the helper falls
// back to instrumentation.ConditionTypeUnknown ("UNKNOWN") rather than an empty
// label so the drift surfaces in alerts.
//
// "DBConnectionSecret", "TransportURLSecret" and "Config" deliberately reuse
// "SecretsReady" rather than introducing a dedicated ConfigReady condition. All
// three sub-reconcilers produce artefacts consumed by every downstream
// reconciler; any failure there blocks the same downstream graph as a
// SecretsReady failure, so collapsing them under the existing condition keeps
// the status contract minimal. A distinct sub_reconciler label on the error
// counter disambiguates during triage.
//
// The map holds sub-reconciler and condition names, not credentials.
//
//nolint:gosec // G101 false positive: "Secrets"/"SecretsReady" are symbolic
var subReconcilerConditionTypes = map[string]string{
	"Secrets":            "SecretsReady",
	"DBConnectionSecret": "SecretsReady",
	"TransportURLSecret": "SecretsReady",
	"Config":             "SecretsReady",
	"Backends":           conditionTypeBackendsReady,
	"BackupBackend":      conditionTypeBackupBackendReady,
	"Database":           "DatabaseReady",
	"Scheduler":          "SchedulerReady",
	"VolumeServices":     "VolumeServicesReady",
	"BackupService":      "BackupServiceReady",
	"Deployment":         "DeploymentReady",
	"DBPurge":            conditionTypeDBPurgeReady,
	"HTTPRoute":          conditionTypeHTTPRouteReady,
	"HealthCheck":        conditionTypeCinderAPIReady,
	"HPA":                "HPAReady",
	"NetworkPolicy":      conditionTypeNetworkPolicyReady,
}

// instrumenter wraps every sub-reconciler call with the shared duration/error
// instrumentation (cinder_operator_reconcile_duration_seconds and
// cinder_operator_reconcile_errors_total). It owns its metric vectors, which
// RegisterMetrics exposes on the controller-runtime registry at startup. The var
// indirection lets unit tests rebind it to an isolated prometheus registry
// without polluting the production registry; production code MUST NOT reassign
// it.
var instrumenter = instrumentation.NewSubReconcilerInstrumenter("cinder_operator", subReconcilerConditionTypes)

// RegisterMetrics exposes the operator's Prometheus collectors on the
// controller-runtime registry: the shared sub-reconciler duration/error vectors
// and the per-CR db-sync, db-purge and service-remove collectors. It returns an
// error on a duplicate registration rather than panicking mid-reconcile, so
// main.go can fail startup cleanly. Call it exactly once during operator setup.
func RegisterMetrics() error {
	if err := instrumenter.Register(ctrlmetrics.Registry); err != nil {
		return fmt.Errorf("registering cinder_operator sub-reconciler metrics: %w", err)
	}
	if err := metrics.Register(); err != nil {
		return fmt.Errorf("registering cinder_operator collectors: %w", err)
	}
	return nil
}
