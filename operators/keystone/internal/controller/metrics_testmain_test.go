// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"os"
	"testing"
)

// TestMain registers the operator's production Prometheus collectors on the
// controller-runtime registry once for the whole test binary. Lazy registration
// was removed in favor of explicit startup registration (RegisterMetrics), so
// the metric-assertion tests that read ctrlmetrics.Registry — the reconcile
// duration/error wiring checks and the per-CR rotation-age/db-sync tests — must
// trigger registration themselves, exactly as main.go does at operator startup.
func TestMain(m *testing.M) {
	if err := RegisterMetrics(); err != nil {
		panic("registering production metrics for test binary: " + err.Error())
	}
	os.Exit(m.Run())
}
