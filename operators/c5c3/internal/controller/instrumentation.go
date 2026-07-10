// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Sub-reconciler instrumentation helper for the ControlPlane controller
package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/c5c3/forge/internal/common/instrumentation"
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
var subReconcilerConditionTypes = map[string]string{
	"Infrastructure":  conditionTypeInfrastructureReady,
	"DBCredentials":   conditionTypeDBCredentialsReady,
	"Keystone":        conditionTypeKeystoneReady,
	"Horizon":         conditionTypeHorizonReady,
	"KORC":            conditionTypeKORCReady,
	"AdminCredential": conditionTypeAdminCredentialReady,
	"AdminPassword":   conditionTypeAdminPasswordReady,
	"Catalog":         conditionTypeCatalogReady,
	"ServiceAccounts": conditionTypeServiceAccountsReady,
}

// subReconcilerMetrics holds the shared sub-reconciler instrumentation metrics
// for the c5c3 operator (c5c3_operator_reconcile_duration_seconds and
// c5c3_operator_reconcile_errors_total). The vectors register lazily on the
// controller-runtime registry the first time a sample is recorded.
var subReconcilerMetrics = instrumentation.NewMetrics("c5c3_operator")

// instrumenter wraps every sub-reconciler call with the shared duration/error
// instrumentation, recording through subReconcilerMetrics. It indirects
// through a package-level var so unit tests can rebind it to an isolated
// prometheus registry without polluting the controller-runtime production
// registry. Production code MUST NOT reassign it.
var instrumenter = instrumentation.NewInstrumenter(subReconcilerMetrics, subReconcilerConditionTypes)

// instrumentSubReconciler wraps a sub-reconciler call, observing duration on
// every path (success, error, panic) and recording an error count if fn
// returns non-nil. name is the sub_reconciler label value; the condition_type
// label on the error counter is resolved via subReconcilerConditionTypes,
// falling back to instrumentation.ConditionTypeUnknown when the name is
// unmapped.
func instrumentSubReconciler(ctx context.Context, name string, fn func(context.Context) (ctrl.Result, error)) (ctrl.Result, error) {
	return instrumenter.Instrument(ctx, name, fn)
}
