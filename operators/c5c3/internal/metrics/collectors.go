// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package metrics exposes Prometheus collectors for the c5c3 operator (CC-0110).
package metrics

import (
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// reconcileDurationBuckets are the histogram bucket boundaries for
// c5c3_operator_reconcile_duration_seconds (CC-0110, REQ-026).
//
// The buckets span the observed sub-reconciler latency range — from fast
// no-op reconciles (10 ms) up to long-running ControlPlane provisioning
// (30 s). The set is intentionally fixed by contract; see the cardinality
// drift-guard test.
var reconcileDurationBuckets = []float64{
	0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30,
}

// collectors bundles every metric vector the operator exposes. The struct
// exists so tests can bind an isolated instance to a private registry;
// production code uses the package-level globalColls registered on
// ctrlmetrics.Registry exactly once (CC-0110, REQ-026).
//
// DECISION (CC-0110 L2): per-CR c5c3 metrics (e.g. provisioning age,
// component-level counters) are intentionally out of scope here. The two
// sub-reconciler vectors below are the instrumentation contract that the
// controller's instrumentation layer (task 2.2) consumes; keeping the
// package minimal avoids speculative cardinality. Add per-CR collectors
// only when a concrete observability requirement lands.
type collectors struct {
	reconcileDuration *prometheus.HistogramVec
	reconcileErrors   *prometheus.CounterVec
}

// newCollectors builds a fresh set of collector vectors. It does NOT
// register them — callers choose the registry.
func newCollectors() *collectors {
	return &collectors{
		reconcileDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "c5c3_operator_reconcile_duration_seconds",
			Help:    "Wall-clock duration of a c5c3 sub-reconciler invocation, in seconds (CC-0110, REQ-026).",
			Buckets: reconcileDurationBuckets,
		}, []string{"sub_reconciler"}),
		reconcileErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "c5c3_operator_reconcile_errors_total",
			Help: "Count of c5c3 sub-reconciler errors, partitioned by sub-reconciler and the condition type it failed to satisfy (CC-0110, REQ-026).",
		}, []string{"sub_reconciler", "condition_type"}),
	}
}

// register adds every vector in c to reg. Returns the first error the
// Registerer emits, typically a duplicate-registration error under the
// controller-runtime global registry if callers register twice.
func (c *collectors) register(reg prometheus.Registerer) error {
	for _, coll := range []prometheus.Collector{
		c.reconcileDuration,
		c.reconcileErrors,
	} {
		if err := reg.Register(coll); err != nil {
			return err
		}
	}
	return nil
}

// TestRecorder exposes the same recording surface as the package-level
// metric helpers but bound to a private collector set on a caller-supplied
// Registerer. It is intended for tests in dependent packages (e.g. the
// controller package's instrumentation_test.go) that must verify
// instrumentation behaviour without polluting the controller-runtime
// production registry. Production code MUST use the package-level
// ObserveReconcileDuration / RecordReconcileError functions (CC-0110,
// REQ-026).
type TestRecorder struct {
	c *collectors
}

// NewTestRecorder builds a fresh collector set on reg and returns a
// TestRecorder that drives those collectors. The returned recorder is
// safe to use in a single test goroutine; tests that swap it into
// production code MUST restore the original via t.Cleanup.
func NewTestRecorder(reg prometheus.Registerer) *TestRecorder {
	c := newCollectors()
	if err := c.register(reg); err != nil {
		// Test registries are always empty in new Prometheus registries;
		// a registration error here is a programmer bug in the test
		// setup and must be surfaced loudly.
		panic(fmt.Sprintf("metrics: test registry rejected collectors: %v", err))
	}
	return &TestRecorder{c: c}
}

// ObserveReconcileDuration records a duration sample on the recorder's
// private collector set, mirroring the package-level helper signature.
func (r *TestRecorder) ObserveReconcileDuration(subReconciler string, d time.Duration) {
	r.c.observeReconcileDuration(subReconciler, d)
}

// RecordReconcileError increments the recorder's private error counter,
// mirroring the package-level helper signature.
func (r *TestRecorder) RecordReconcileError(subReconciler, conditionType string) {
	r.c.recordReconcileError(subReconciler, conditionType)
}

// globalColls is the single production instance, registered on the
// controller-runtime metrics registry exactly once via initOnce.
var (
	globalColls *collectors
	initOnce    sync.Once
)

// globalCollectors returns the lazily-initialized package-wide collectors,
// registering them on ctrlmetrics.Registry on first access. Using
// sync.Once ensures registration is idempotent across repeated test runs
// (CC-0110, REQ-026).
func globalCollectors() *collectors {
	initOnce.Do(func() {
		globalColls = newCollectors()
		if err := globalColls.register(ctrlmetrics.Registry); err != nil {
			// Duplicate-registration on the controller-runtime registry
			// is a startup bug; fail fast rather than silently hide it.
			panic(fmt.Sprintf("metrics: failed to register collectors on controller-runtime registry: %v", err))
		}
	})
	return globalColls
}

// ObserveReconcileDuration records the wall-clock duration of a single
// sub-reconciler invocation (CC-0110, REQ-026).
func ObserveReconcileDuration(subReconciler string, d time.Duration) {
	globalCollectors().observeReconcileDuration(subReconciler, d)
}

// RecordReconcileError increments the reconcile-error counter for a given
// sub-reconciler and the condition type it failed to drive to True (CC-0110,
// REQ-026).
func RecordReconcileError(subReconciler, conditionType string) {
	globalCollectors().recordReconcileError(subReconciler, conditionType)
}

// --- internal methods (bound to an instance) -------------------------------
//
// Each public package-level helper above (ObserveReconcileDuration,
// RecordReconcileError) is a thin wrapper that resolves globalCollectors()
// and forwards to the matching method below. The methods are also exercised
// directly by collectors_test.go against an isolated registry so they are
// not test-only (CC-0110, REQ-026).

func (c *collectors) observeReconcileDuration(subReconciler string, d time.Duration) {
	c.reconcileDuration.WithLabelValues(subReconciler).Observe(d.Seconds())
}

func (c *collectors) recordReconcileError(subReconciler, conditionType string) {
	c.reconcileErrors.WithLabelValues(subReconciler, conditionType).Inc()
}
