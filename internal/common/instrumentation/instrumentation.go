// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package instrumentation

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrl "sigs.k8s.io/controller-runtime"
)

// ConditionTypeUnknown is the condition_type label emitted when an
// Instrumenter is invoked with a sub-reconciler name absent from its
// condition-type map. Emitting an explicit "UNKNOWN" sentinel instead of an
// empty label makes any drift visible in dashboards and alerts rather than
// silently producing an empty condition_type.
const ConditionTypeUnknown = "UNKNOWN"

// reconcileDurationBuckets are the histogram bucket boundaries for the
// <prefix>_reconcile_duration_seconds histogram. They span the observed
// sub-reconciler latency range — from fast no-op reconciles (10 ms) up to
// long-running provisioning work (30 s). The set is fixed by contract; the
// cardinality and bucket drift-guard tests assert it.
var reconcileDurationBuckets = []float64{
	0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30,
}

// Metrics bundles the two sub-reconciler metric vectors a single operator
// exposes: a duration histogram labelled by sub_reconciler, and an error
// counter labelled by sub_reconciler and condition_type. Construct one per
// operator with NewMetrics (unregistered — call Register at startup) or
// NewMetricsOnRegistry (tests, eagerly registered on a caller-supplied
// registry).
type Metrics struct {
	reconcileDuration *prometheus.HistogramVec
	reconcileErrors   *prometheus.CounterVec
}

// newMetrics builds the metric vectors for prefix without registering them.
func newMetrics(prefix string) *Metrics {
	return &Metrics{
		reconcileDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    prefix + "_reconcile_duration_seconds",
			Help:    "Wall-clock duration of a sub-reconciler invocation, in seconds.",
			Buckets: reconcileDurationBuckets,
		}, []string{"sub_reconciler"}),
		reconcileErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_reconcile_errors_total",
			Help: "Count of sub-reconciler errors, partitioned by sub-reconciler and the condition type it failed to satisfy.",
		}, []string{"sub_reconciler", "condition_type"}),
	}
}

// NewMetrics returns an unregistered Metrics instance for prefix. Call Register
// (directly, or via NewSubReconcilerInstrumenter and Instrumenter.Register)
// during operator startup to expose its vectors on the controller-runtime
// registry. Recording a sample before registration is inert: the vector holds
// the sample locally but it is never scraped until the vector is registered.
func NewMetrics(prefix string) *Metrics {
	return newMetrics(prefix)
}

// NewMetricsOnRegistry returns a Metrics instance for prefix whose vectors are
// registered eagerly on reg. It is intended for tests that must verify
// instrumentation behaviour against an isolated prometheus.NewRegistry()
// without polluting the controller-runtime production registry. It panics if
// reg rejects the collectors, which only happens on a programmer error in the
// test setup.
func NewMetricsOnRegistry(prefix string, reg prometheus.Registerer) *Metrics {
	m := newMetrics(prefix)
	if err := m.Register(reg); err != nil {
		panic(fmt.Sprintf("instrumentation: registry rejected %s collectors: %v", prefix, err))
	}
	return m
}

// Register adds both vectors to reg, returning the first error it emits.
// Registration is not idempotent — registering the same vectors twice on one
// registry returns a duplicate-registration error — so operators call it
// exactly once at startup and surface any error as a clean startup failure
// instead of a mid-reconcile panic.
func (m *Metrics) Register(reg prometheus.Registerer) error {
	for _, coll := range []prometheus.Collector{m.reconcileDuration, m.reconcileErrors} {
		if err := reg.Register(coll); err != nil {
			return err
		}
	}
	return nil
}

// ObserveReconcileDuration records a single duration sample for the named
// sub-reconciler. It is a no-op-visible operation until the vector is
// registered.
func (m *Metrics) ObserveReconcileDuration(subReconciler string, d time.Duration) {
	m.reconcileDuration.WithLabelValues(subReconciler).Observe(d.Seconds())
}

// RecordReconcileError increments the error counter for a sub-reconciler and
// the condition type it failed to drive to True.
func (m *Metrics) RecordReconcileError(subReconciler, conditionType string) {
	m.reconcileErrors.WithLabelValues(subReconciler, conditionType).Inc()
}

// Instrumenter wraps sub-reconciler calls with duration and error
// instrumentation, attributing errors to a condition type via its
// conditionTypes map.
type Instrumenter struct {
	metrics        *Metrics
	conditionTypes map[string]string
}

// NewInstrumenter returns an Instrumenter that records through m and resolves
// the condition_type error label via conditionTypes. A name absent from the
// map falls back to ConditionTypeUnknown.
func NewInstrumenter(m *Metrics, conditionTypes map[string]string) *Instrumenter {
	return &Instrumenter{metrics: m, conditionTypes: conditionTypes}
}

// NewSubReconcilerInstrumenter builds an unregistered Metrics for prefix and
// wraps it in an Instrumenter that resolves the condition_type error label via
// conditionTypes. Operators declare one at package scope and call
// Instrumenter.Register once at startup to expose the metrics. This is the
// single constructor an operator's instrumentation glue needs; the metrics are
// owned by the returned Instrumenter.
func NewSubReconcilerInstrumenter(prefix string, conditionTypes map[string]string) *Instrumenter {
	return NewInstrumenter(NewMetrics(prefix), conditionTypes)
}

// Register exposes the Instrumenter's metric vectors on reg, returning any
// registration error rather than panicking so a duplicate-registration surfaces
// as a clean operator-startup failure.
func (i *Instrumenter) Register(reg prometheus.Registerer) error {
	return i.metrics.Register(reg)
}

// Instrument runs fn, observing its duration on every path (success, error,
// panic) and recording an error count if fn returns a non-nil error. name is
// the sub_reconciler label value; the condition_type label on the error
// counter is resolved via the Instrumenter's conditionTypes map, falling back
// to ConditionTypeUnknown when the name is unmapped.
//
// Panic-safety: the deferred duration emission runs before a panic unwinds the
// stack, so a crashing sub-reconciler still contributes a duration sample. The
// Instrumenter does NOT recover — panics propagate to the caller.
func (i *Instrumenter) Instrument(ctx context.Context, name string, fn func(context.Context) (ctrl.Result, error)) (ctrl.Result, error) {
	start := time.Now()
	defer func() {
		i.metrics.ObserveReconcileDuration(name, time.Since(start))
	}()

	res, err := fn(ctx)
	if err != nil {
		condType, ok := i.conditionTypes[name]
		if !ok {
			condType = ConditionTypeUnknown
		}
		i.metrics.RecordReconcileError(name, condType)
	}
	return res, err
}
