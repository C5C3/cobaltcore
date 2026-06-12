// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package instrumentation provides the shared sub-reconciler instrumentation
// primitives used by every forge operator. It exposes a metric pair
// (<prefix>_reconcile_duration_seconds histogram and
// <prefix>_reconcile_errors_total counter) and an Instrumenter that wraps a
// sub-reconciler call, observing its duration on every path and attributing
// errors to the condition type the sub-reconciler drives.
//
// Operators supply their own metric prefix and condition-type map; the metric
// names, label sets and histogram buckets are identical across operators by
// design, so the shared package keeps them in lockstep and avoids the
// byte-identical duplication that previously lived in each operator's
// controller and metrics packages.
package instrumentation
