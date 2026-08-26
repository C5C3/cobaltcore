// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Sub-reconciler instrumentation helper for the OVN controllers.
package controller

import (
	"fmt"

	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/c5c3/cobaltcore/internal/common/instrumentation"
	ovnmetrics "github.com/c5c3/cobaltcore/operators/ovn/internal/metrics"
)

// subReconcilerConditionTypes maps a sub_reconciler label value to the
// condition_type it drives. The instrumenter consults this map to attribute
// errors to the correct Ready sub-condition.
//
// One map serves both pipelines of this operator, the OVNCentral one and the
// OVNChassis one, because the instrumenter is shared and a sub_reconciler name
// is unique across the two. The names below are the OVNCentral pipeline's.
//
// The mapping is guarded in both directions: every value MUST be a member of
// the owning CR's sub-condition list and every pipeline step name MUST be a key
// here. If a sub_reconciler name reaches the instrumenter without a key, the
// helper falls back to instrumentation.ConditionTypeUnknown ("UNKNOWN") rather
// than an empty label so the drift surfaces in alerts.
var subReconcilerConditionTypes = map[string]string{
	"TLS":        conditionTypeTLSReady,
	"Northbound": conditionTypeNorthboundReady,
	"Southbound": conditionTypeSouthboundReady,
	"Endpoints":  conditionTypeEndpointsReady,
	"Northd":     conditionTypeNorthdReady,
	"Relay":      conditionTypeRelayReady,
	"Backup":     conditionTypeBackupReady,
}

// instrumenter wraps every sub-reconciler call with the shared duration/error
// instrumentation (ovn_operator_reconcile_duration_seconds and
// ovn_operator_reconcile_errors_total). It owns its metric vectors, which
// RegisterMetrics exposes on the controller-runtime registry at startup. The var
// indirection lets unit tests rebind it to an isolated prometheus registry
// without polluting the production registry; production code MUST NOT reassign
// it.
var instrumenter = instrumentation.NewSubReconcilerInstrumenter("ovn_operator", subReconcilerConditionTypes)

// RegisterMetrics exposes the operator's Prometheus collectors on the
// controller-runtime registry: the shared sub-reconciler duration/error vectors
// and the per-CR backup collectors. It returns an error on a duplicate
// registration rather than panicking mid-reconcile, so main.go can fail startup
// cleanly. Call it exactly once during operator setup.
func RegisterMetrics() error {
	if err := instrumenter.Register(ctrlmetrics.Registry); err != nil {
		return fmt.Errorf("registering ovn_operator sub-reconciler metrics: %w", err)
	}
	if err := ovnmetrics.Register(); err != nil {
		return fmt.Errorf("registering ovn_operator collectors: %w", err)
	}
	return nil
}
