// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Sub-reconciler instrumentation helper for the Keystone controller
// (CC-0089, REQ-001, REQ-002, REQ-007).
package controller

import (
	"context"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/c5c3/forge/operators/keystone/internal/metrics"
)

// subReconcilerConditionTypeUnknown is the condition_type label emitted when
// instrumentSubReconciler is invoked with a name absent from
// subReconcilerConditionTypes. The drift-guard tests
// TestSubReconcilerConditionTypesCoversAllNames and
// TestSubReconcilerConditionTypesCoversAllCallSites should catch every such
// gap before code lands, but the explicit "UNKNOWN" sentinel makes any escape
// visible in dashboards/alerts rather than silently emitting an empty label
// (CC-0089, REQ-002).
const subReconcilerConditionTypeUnknown = "UNKNOWN"

// subReconcilerConditionTypes maps a sub_reconciler label value to the
// condition_type it drives. instrumentSubReconciler consults this map to
// attribute errors to the correct Ready sub-condition (CC-0089, REQ-002).
//
// Every value MUST be a member of subConditionTypes; the drift-guard test
// TestSubReconcilerConditionTypesCoversAllNames asserts this invariant.
// Every key MUST cover every sub_reconciler name passed at any
// instrumentSubReconciler call site or parallelSubReconciler struct literal
// in this package; TestSubReconcilerConditionTypesCoversAllCallSites walks
// the source AST to enforce this. If both guards are bypassed, the helper's
// fallback emits subReconcilerConditionTypeUnknown rather than an empty
// label so the drift surfaces in alerts.
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
	"FernetKeys":         "FernetKeysReady",
	"CredentialKeys":     "CredentialKeysReady",
	"NetworkPolicy":      "NetworkPolicyReady",
	"Database":           "DatabaseReady",
	"PolicyValidation":   conditionTypePolicyValidReady,
	"Deployment":         "DeploymentReady",
	"HTTPRoute":          conditionTypeHTTPRouteReady,
	"HealthCheck":        conditionTypeKeystoneAPIReady,
	"HPA":                "HPAReady",
	"Bootstrap":          "BootstrapReady",
	"TrustFlush":         "TrustFlushReady",
}

// instrumentObserveDuration and instrumentRecordError indirect through
// package-level vars so unit tests can rebind them to an isolated
// prometheus.NewRegistry() and avoid leaking test-only label values into
// the production controller-runtime registry. Production code MUST NOT
// reassign these (CC-0089, REQ-007, REQ-008).
var (
	instrumentObserveDuration = metrics.ObserveReconcileDuration
	instrumentRecordError     = metrics.RecordReconcileError
)

// instrumentSubReconciler wraps a sub-reconciler call, observing duration on
// every path (success, error, panic) and recording an error count if fn
// returns non-nil. name is the sub_reconciler label value; the condition_type
// label on the error counter is resolved via subReconcilerConditionTypes.
// If the name is missing from the map, the helper falls back to
// subReconcilerConditionTypeUnknown ("UNKNOWN") rather than emitting an empty
// label, so any drift escaping the static guards
// (TestSubReconcilerConditionTypesCoversAllNames /
// TestSubReconcilerConditionTypesCoversAllCallSites) is visible in alerts
// (CC-0089, REQ-001, REQ-002, REQ-007).
//
// Panic-safety: the deferred emission runs before the panic unwinds the
// stack, so a crashing sub-reconciler still contributes a duration sample.
// The helper intentionally does NOT recover — panics propagate to the caller.
func instrumentSubReconciler(ctx context.Context, name string, fn func(context.Context) (ctrl.Result, error)) (ctrl.Result, error) {
	start := time.Now()
	defer func() {
		instrumentObserveDuration(name, time.Since(start))
	}()

	res, err := fn(ctx)
	if err != nil {
		condType, ok := subReconcilerConditionTypes[name]
		if !ok {
			condType = subReconcilerConditionTypeUnknown
		}
		instrumentRecordError(name, condType)
	}
	return res, err
}
