// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package metrics exposes Prometheus collectors for the Keystone operator.
package metrics

import (
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// reconcileDurationBuckets are the histogram bucket boundaries for
// keystone_operator_reconcile_duration_seconds.
//
// The buckets span the observed sub-reconciler latency range — from fast
// no-op reconciles (10 ms) up to long-running DB sync jobs (30 s). The set
// is intentionally fixed by contract; see the cardinality drift-guard test.
var reconcileDurationBuckets = []float64{
	0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30,
}

// dbSyncDurationBuckets are the histogram bucket boundaries for
// keystone_operator_db_sync_duration_seconds. DB sync
// jobs are measured in seconds-to-minutes, so the range 1 s – 10 min
// captures the realistic distribution.
//
// DECISION: buckets chosen from the "suggested" list verbatim
// because the observability doc does not prescribe alternatives and the
// plan explicitly lists them.
var dbSyncDurationBuckets = []float64{1, 5, 10, 30, 60, 120, 300, 600}

// collectors bundles every metric vector the operator exposes. The struct
// exists so tests can bind an isolated instance to a private registry;
// production code uses the package-level globalColls registered on
// ctrlmetrics.Registry exactly once.
type collectors struct {
	reconcileDuration *prometheus.HistogramVec
	reconcileErrors   *prometheus.CounterVec
	keyRotationAge    *prometheus.GaugeVec
	dbSyncTotal       *prometheus.CounterVec
	dbSyncDuration    *prometheus.HistogramVec
}

// newCollectors builds a fresh set of collector vectors. It does NOT
// register them — callers choose the registry.
func newCollectors() *collectors {
	return &collectors{
		reconcileDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "keystone_operator_reconcile_duration_seconds",
			Help:    "Wall-clock duration of a Keystone sub-reconciler invocation, in seconds.",
			Buckets: reconcileDurationBuckets,
		}, []string{"sub_reconciler"}),
		reconcileErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "keystone_operator_reconcile_errors_total",
			Help: "Count of Keystone sub-reconciler errors, partitioned by sub-reconciler and the condition type it failed to satisfy.",
		}, []string{"sub_reconciler", "condition_type"}),
		keyRotationAge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "keystone_operator_key_rotation_age_seconds",
			Help: "Age in seconds of the most recent successful key rotation for a given Keystone CR and key type.",
		}, []string{"keystone", "namespace", "key_type"}),
		dbSyncTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "keystone_operator_db_sync_total",
			Help: "Count of db_sync jobs terminated per Keystone CR, labelled by the terminal state.",
		}, []string{"keystone", "namespace", "result"}),
		dbSyncDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "keystone_operator_db_sync_duration_seconds",
			Help:    "Duration in seconds of terminated db_sync jobs.",
			Buckets: dbSyncDurationBuckets,
		}, []string{"keystone", "namespace"}),
	}
}

// register adds every vector in c to reg. Returns the first error the
// Registerer emits, typically a duplicate-registration error under the
// controller-runtime global registry if callers register twice.
func (c *collectors) register(reg prometheus.Registerer) error {
	for _, coll := range []prometheus.Collector{
		c.reconcileDuration,
		c.reconcileErrors,
		c.keyRotationAge,
		c.dbSyncTotal,
		c.dbSyncDuration,
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
// ObserveReconcileDuration / RecordReconcileError functions.
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
// sub-reconciler invocation.
func ObserveReconcileDuration(subReconciler string, d time.Duration) {
	globalCollectors().observeReconcileDuration(subReconciler, d)
}

// RecordReconcileError increments the reconcile-error counter for a given
// sub-reconciler and the condition type it failed to drive to True.
func RecordReconcileError(subReconciler, conditionType string) {
	globalCollectors().recordReconcileError(subReconciler, conditionType)
}

// SetKeyRotationAge publishes the age in seconds of the most recent key
// rotation for (keystone, namespace, keyType). Returns an error and does
// NOT update the gauge if completedAt is the zero Time (e.g. when the CR
// annotation is missing or malformed).
func SetKeyRotationAge(keystone, namespace, keyType string, completedAt time.Time) error {
	return globalCollectors().setKeyRotationAge(keystone, namespace, keyType, completedAt)
}

// RecordDBSync increments the db_sync terminal-state counter and records
// one observation in the db_sync duration histogram. result is expected
// to be "succeeded" or "failed".
//
// DECISION: in-progress jobs MUST NOT call RecordDBSync; the counter
// represents terminal transitions only, so repeated polling of a running
// job does not inflate it. Level 2 wires the call-site at the job's
// terminal-state branch in reconcile_database.go.
func RecordDBSync(keystone, namespace, result string, duration time.Duration) {
	globalCollectors().recordDBSync(keystone, namespace, result, duration)
}

// DeleteForKeystone drops every series tagged with the given keystone name
// and namespace from the per-CR collectors (rotation age and db-sync). It
// is a no-op for the sub-reconciler metrics because those intentionally
// carry no CR labels.
func DeleteForKeystone(name, namespace string) {
	globalCollectors().deleteForKeystone(name, namespace)
}

// --- internal methods (bound to an instance) -------------------------------
//
// Each public package-level helper above (ObserveReconcileDuration,
// RecordReconcileError, SetKeyRotationAge, RecordDBSync, DeleteForKeystone)
// is a thin wrapper that resolves globalCollectors() and forwards to the
// matching method below. The methods are also exercised directly by
// collectors_test.go against an isolated registry so they are not
// test-only.

func (c *collectors) observeReconcileDuration(subReconciler string, d time.Duration) {
	c.reconcileDuration.WithLabelValues(subReconciler).Observe(d.Seconds())
}

func (c *collectors) recordReconcileError(subReconciler, conditionType string) {
	c.reconcileErrors.WithLabelValues(subReconciler, conditionType).Inc()
}

func (c *collectors) setKeyRotationAge(keystone, namespace, keyType string, completedAt time.Time) error {
	if completedAt.IsZero() {
		return fmt.Errorf("metrics: refusing to set key_rotation_age for %s/%s key_type=%s: zero timestamp", namespace, keystone, keyType)
	}
	age := time.Since(completedAt).Seconds()
	c.keyRotationAge.WithLabelValues(keystone, namespace, keyType).Set(age)
	return nil
}

func (c *collectors) recordDBSync(keystone, namespace, result string, duration time.Duration) {
	c.dbSyncTotal.WithLabelValues(keystone, namespace, result).Inc()
	c.dbSyncDuration.WithLabelValues(keystone, namespace).Observe(duration.Seconds())
}

func (c *collectors) deleteForKeystone(name, namespace string) {
	labels := prometheus.Labels{"keystone": name, "namespace": namespace}
	c.keyRotationAge.DeletePartialMatch(labels)
	c.dbSyncTotal.DeletePartialMatch(labels)
	c.dbSyncDuration.DeletePartialMatch(labels)
}
