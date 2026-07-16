// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package metrics exposes Prometheus collectors for the Glance operator.
package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// dbSyncDurationBuckets are the histogram bucket boundaries for
// glance_operator_db_sync_duration_seconds. DB sync jobs are measured in
// seconds-to-minutes, so the range 1 s – 10 min captures the realistic
// distribution. Mirrors keystone's buckets.
var dbSyncDurationBuckets = []float64{1, 5, 10, 30, 60, 120, 300, 600}

// collectors bundles the per-CR metric vectors the operator exposes. The struct
// exists so tests can bind an isolated instance to a private registry;
// production code uses the package-level globalColls registered on
// ctrlmetrics.Registry exactly once. The sub-reconciler duration/error pair
// lives in the shared instrumentation package and is registered by the
// operator's RegisterMetrics (next commit); only the per-CR db-sync collectors
// stay here.
type collectors struct {
	dbSyncTotal    *prometheus.CounterVec
	dbSyncDuration *prometheus.HistogramVec
}

// newCollectors builds a fresh set of collector vectors. It does NOT register
// them — callers choose the registry.
func newCollectors() *collectors {
	return &collectors{
		dbSyncTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "glance_operator_db_sync_total",
			Help: "Count of db-sync jobs terminated per Glance CR, labelled by the terminal state.",
		}, []string{"glance", "namespace", "result"}),
		dbSyncDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "glance_operator_db_sync_duration_seconds",
			Help:    "Duration in seconds of terminated db-sync jobs.",
			Buckets: dbSyncDurationBuckets,
		}, []string{"glance", "namespace"}),
	}
}

// register adds every vector in c to reg. Returns the first error the
// Registerer emits, typically a duplicate-registration error under the
// controller-runtime global registry if callers register twice.
func (c *collectors) register(reg prometheus.Registerer) error {
	for _, coll := range []prometheus.Collector{
		c.dbSyncTotal,
		c.dbSyncDuration,
	} {
		if err := reg.Register(coll); err != nil {
			return err
		}
	}
	return nil
}

// globalColls is the single production instance. It is constructed at package
// init but not registered; Register exposes it on the controller-runtime
// registry exactly once at operator startup. Recording before Register is inert
// (the vectors hold samples locally until registered).
var (
	globalColls  = newCollectors()
	registerOnce sync.Once
	registerErr  error
)

// Register exposes the per-CR collectors on the controller-runtime metrics
// registry exactly once and returns any registration error, so a
// duplicate-registration surfaces as a clean operator-startup failure rather
// than a mid-reconcile panic. Repeated calls return the memoized first result.
// The operator's RegisterMetrics (next commit) calls it during setup.
func Register() error {
	registerOnce.Do(func() {
		registerErr = globalColls.register(ctrlmetrics.Registry)
	})
	return registerErr
}

// RecordDBSync increments the db-sync terminal-state counter and records one
// observation in the db-sync duration histogram. result is expected to be
// "succeeded" or "failed". In-progress jobs MUST NOT call it: the counter
// represents terminal transitions only.
func RecordDBSync(glance, namespace, result string, duration time.Duration) {
	globalColls.recordDBSync(glance, namespace, result, duration)
}

// DeleteForGlance drops every series tagged with the given Glance name and
// namespace from the per-CR collectors. The sub-reconciler metrics intentionally
// carry no CR labels, so there is nothing to delete there.
func DeleteForGlance(name, namespace string) {
	globalColls.deleteForGlance(name, namespace)
}

// --- internal methods (bound to an instance) -------------------------------
//
// The public package-level helpers above (RecordDBSync, DeleteForGlance) are
// thin wrappers that forward to the matching method below on globalColls. The
// methods are also exercised directly by collectors_test.go against an isolated
// registry.

func (c *collectors) recordDBSync(glance, namespace, result string, duration time.Duration) {
	c.dbSyncTotal.WithLabelValues(glance, namespace, result).Inc()
	c.dbSyncDuration.WithLabelValues(glance, namespace).Observe(duration.Seconds())
}

func (c *collectors) deleteForGlance(name, namespace string) {
	labels := prometheus.Labels{"glance": name, "namespace": namespace}
	c.dbSyncTotal.DeletePartialMatch(labels)
	c.dbSyncDuration.DeletePartialMatch(labels)
}
