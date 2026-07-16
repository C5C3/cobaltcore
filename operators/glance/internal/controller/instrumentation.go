// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Sub-reconciler instrumentation helper for the Glance controller.
package controller

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/c5c3/forge/internal/common/instrumentation"
	glancemetrics "github.com/c5c3/forge/operators/glance/internal/metrics"
)

// subReconcilerConditionTypes maps a sub_reconciler label value to the
// condition_type it drives. The instrumenter consults this map to attribute
// errors to the correct Ready sub-condition.
//
// Every value MUST be a member of subConditionTypes; the drift-guard test
// TestSubReconcilerConditionTypesCoversAllNames asserts this invariant. If a
// sub_reconciler name reaches the instrumenter without a key here, the helper
// falls back to instrumentation.ConditionTypeUnknown ("UNKNOWN") rather than an
// empty label so the drift surfaces in alerts.
//
// "DBConnectionSecret" and "Config" deliberately reuse "SecretsReady" rather
// than introducing a dedicated ConfigReady condition. Both sub-reconcilers
// produce Secret/ConfigMap artefacts consumed by every downstream reconciler;
// any failure there blocks the same downstream graph as a SecretsReady failure,
// so collapsing them under the existing condition keeps the status contract
// minimal — a distinct sub_reconciler label on the error counter disambiguates
// during triage.
//
// sub-reconciler and condition names, not credentials.
//
//nolint:gosec // G101 false positive: "Secrets"/"SecretsReady" are symbolic
var subReconcilerConditionTypes = map[string]string{
	"Secrets":            "SecretsReady",
	"DBConnectionSecret": "SecretsReady",
	"Backends":           conditionTypeBackendsReady,
	"Config":             "SecretsReady",
	"Database":           "DatabaseReady",
	"Deployment":         "DeploymentReady",
	"HTTPRoute":          conditionTypeHTTPRouteReady,
	"HealthCheck":        conditionTypeGlanceAPIReady,
	"HPA":                "HPAReady",
	"NetworkPolicy":      conditionTypeNetworkPolicyReady,
}

// instrumenter wraps every sub-reconciler call with the shared duration/error
// instrumentation (glance_operator_reconcile_duration_seconds and
// glance_operator_reconcile_errors_total). It owns its metric vectors, which
// RegisterMetrics exposes on the controller-runtime registry at startup. The var
// indirection lets unit tests rebind it to an isolated prometheus registry
// without polluting the production registry; production code MUST NOT reassign
// it.
var instrumenter = instrumentation.NewSubReconcilerInstrumenter("glance_operator", subReconcilerConditionTypes)

// RegisterMetrics exposes the operator's Prometheus collectors on the
// controller-runtime registry: the shared sub-reconciler duration/error vectors
// and the glance-only per-CR db-sync collectors. It returns an error on a
// duplicate-registration rather than panicking mid-reconcile, so main.go can
// fail startup cleanly. Call it exactly once during operator setup.
func RegisterMetrics() error {
	if err := instrumenter.Register(ctrlmetrics.Registry); err != nil {
		return fmt.Errorf("registering glance_operator sub-reconciler metrics: %w", err)
	}
	if err := glancemetrics.Register(); err != nil {
		return fmt.Errorf("registering glance per-CR metrics: %w", err)
	}
	return nil
}

// instrumentSubReconciler wraps a sub-reconciler call, observing duration on
// every path (success, error, panic) and recording an error count if fn returns
// non-nil. name is the sub_reconciler label value; the condition_type label on
// the error counter is resolved via subReconcilerConditionTypes, falling back to
// instrumentation.ConditionTypeUnknown when the name is unmapped.
func instrumentSubReconciler(ctx context.Context, name string, fn func(context.Context) (ctrl.Result, error)) (ctrl.Result, error) {
	return instrumenter.Instrument(ctx, name, fn)
}
