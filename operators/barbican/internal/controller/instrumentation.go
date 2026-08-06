// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Sub-reconciler instrumentation helper for the Barbican controller.
package controller

import (
	"fmt"

	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/c5c3/forge/internal/common/instrumentation"
	barbicanmetrics "github.com/c5c3/forge/operators/barbican/internal/metrics"
)

// subReconcilerConditionTypes maps a sub_reconciler label value to the
// condition_type it drives. The instrumenter consults this map to attribute
// errors to the correct Ready sub-condition. It carries the full pipeline
// vocabulary, so a step never reaches the instrumenter unmapped.
//
// The mapping is guarded in both directions: every value MUST be a member of
// subConditionTypes and every pipeline step name MUST be a key here. If a
// sub_reconciler name reaches the instrumenter without a key, the helper falls
// back to instrumentation.ConditionTypeUnknown ("UNKNOWN") rather than an empty
// label so the drift surfaces in alerts.
//
// "DBConnectionSecret" and "Config" deliberately reuse "SecretsReady" rather
// than introducing a dedicated ConfigReady condition. Both sub-reconcilers
// produce Secret artefacts consumed by every downstream reconciler; any failure
// there blocks the same downstream graph as a SecretsReady failure, so
// collapsing them under the existing condition keeps the status contract
// minimal — a distinct sub_reconciler label on the error counter disambiguates
// during triage.
//
// sub-reconciler and condition names, not credentials.
//
//nolint:gosec // G101 false positive: "Secrets"/"SecretsReady" are symbolic
var subReconcilerConditionTypes = map[string]string{
	"Secrets":            "SecretsReady",
	"DBConnectionSecret": "SecretsReady",
	"SecretStores":       conditionTypeSecretStoresReady,
	"Config":             "SecretsReady",
	"Database":           "DatabaseReady",
	"Deployment":         "DeploymentReady",
	"HTTPRoute":          conditionTypeHTTPRouteReady,
	"HealthCheck":        conditionTypeBarbicanAPIReady,
	"HPA":                "HPAReady",
	"NetworkPolicy":      conditionTypeNetworkPolicyReady,
	"DBClean":            conditionTypeDBCleanReady,
}

// instrumenter wraps every sub-reconciler call with the shared duration/error
// instrumentation (barbican_operator_reconcile_duration_seconds and
// barbican_operator_reconcile_errors_total). It owns its metric vectors, which
// RegisterMetrics exposes on the controller-runtime registry at startup. The var
// indirection lets unit tests rebind it to an isolated prometheus registry
// without polluting the production registry; production code MUST NOT reassign
// it.
var instrumenter = instrumentation.NewSubReconcilerInstrumenter("barbican_operator", subReconcilerConditionTypes)

// RegisterMetrics exposes the operator's Prometheus collectors on the
// controller-runtime registry: the shared sub-reconciler duration/error vectors
// and the per-CR db-sync, db-clean and secret-store collectors. It returns an
// error on a duplicate registration rather than panicking mid-reconcile, so
// main.go can fail startup cleanly. Call it exactly once during operator setup.
func RegisterMetrics() error {
	if err := instrumenter.Register(ctrlmetrics.Registry); err != nil {
		return fmt.Errorf("registering barbican_operator sub-reconciler metrics: %w", err)
	}
	if err := barbicanmetrics.Register(); err != nil {
		return fmt.Errorf("registering barbican_operator collectors: %w", err)
	}
	return nil
}
