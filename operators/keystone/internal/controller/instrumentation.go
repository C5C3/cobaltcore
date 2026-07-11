// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Sub-reconciler instrumentation helper for the Keystone controller
package controller

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/c5c3/forge/internal/common/instrumentation"
	"github.com/c5c3/forge/operators/keystone/internal/metrics"
)

// subReconcilerConditionTypes maps a sub_reconciler label value to the
// condition_type it drives. The instrumenter consults this map to attribute
// errors to the correct Ready sub-condition.
//
// Every value MUST be a member of subConditionTypes; the drift-guard test
// TestSubReconcilerConditionTypesCoversAllNames asserts this invariant. If a
// sub_reconciler name reaches the instrumenter without a key here, the helper
// falls back to instrumentation.ConditionTypeUnknown ("UNKNOWN") rather than
// an empty label so the drift surfaces in alerts.
//
// "Config" and "DBConnectionSecret" deliberately reuse "SecretsReady" rather
// than introducing a dedicated ConfigReady condition. Both sub-reconcilers
// produce ConfigMap/Secret artefacts consumed by every downstream reconciler;
// any failure there blocks the same downstream graph as a SecretsReady
// failure, so collapsing them under the existing condition keeps the public
// status contract minimal. The downstream-alert-mislabelling concern is
// accepted: a config parse failure surfaces as SecretsReady=False with a
// distinct sub_reconciler="Config" label on the error counter, which is
// sufficient to disambiguate during incident triage.
//
// sub-reconciler and condition names, not credentials.
//
//nolint:gosec // G101 false positive: "Secrets"/"SecretsReady" are symbolic
var subReconcilerConditionTypes = map[string]string{
	"Secrets":            "SecretsReady",
	"DBConnectionSecret": "SecretsReady",
	"Config":             "SecretsReady",
	"IdentityBackends":   conditionTypeIdentityBackendsReady,
	"FernetKeys":         "FernetKeysReady",
	"CredentialKeys":     "CredentialKeysReady",
	"NetworkPolicy":      "NetworkPolicyReady",
	"Database":           "DatabaseReady",
	"DatabaseTLS":        conditionTypeDatabaseTLSReady,
	"PolicyValidation":   conditionTypePolicyValidReady,
	"Deployment":         "DeploymentReady",
	"HTTPRoute":          conditionTypeHTTPRouteReady,
	"HealthCheck":        conditionTypeKeystoneAPIReady,
	"HPA":                "HPAReady",
	"Bootstrap":          "BootstrapReady",
	"TrustFlush":         "TrustFlushReady",
	"PasswordRotation":   conditionTypePasswordRotationReady,
}

// instrumenter wraps every sub-reconciler call with the shared duration/error
// instrumentation (keystone_operator_reconcile_duration_seconds and
// keystone_operator_reconcile_errors_total). It owns its metric vectors, which
// RegisterMetrics exposes on the controller-runtime registry at startup. The
// var indirection lets unit tests rebind it to an isolated prometheus registry
// without polluting the production registry; production code MUST NOT reassign
// it.
var instrumenter = instrumentation.NewSubReconcilerInstrumenter("keystone_operator", subReconcilerConditionTypes)

// RegisterMetrics exposes the operator's Prometheus collectors on the
// controller-runtime registry: the shared sub-reconciler duration/error vectors
// and the keystone-only per-CR collectors. It returns an error on a
// duplicate-registration rather than panicking mid-reconcile, so main.go can
// fail startup cleanly. Call it exactly once during operator setup.
func RegisterMetrics() error {
	if err := instrumenter.Register(ctrlmetrics.Registry); err != nil {
		return fmt.Errorf("registering keystone_operator sub-reconciler metrics: %w", err)
	}
	if err := metrics.Register(); err != nil {
		return fmt.Errorf("registering keystone per-CR metrics: %w", err)
	}
	return nil
}

// instrumentSubReconciler wraps a sub-reconciler call, observing duration on
// every path (success, error, panic) and recording an error count if fn
// returns non-nil. name is the sub_reconciler label value; the condition_type
// label on the error counter is resolved via subReconcilerConditionTypes,
// falling back to instrumentation.ConditionTypeUnknown when the name is
// unmapped.
func instrumentSubReconciler(ctx context.Context, name string, fn func(context.Context) (ctrl.Result, error)) (ctrl.Result, error) {
	return instrumenter.Instrument(ctx, name, fn)
}
