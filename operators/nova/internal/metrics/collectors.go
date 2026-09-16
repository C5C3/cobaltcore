// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package metrics exposes Prometheus collectors for the Nova operator.
package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// jobDurationBuckets are the histogram bucket boundaries shared by the two
// per-CR job histograms. A db-sync is measured in seconds-to-minutes, so the
// range 1 s – 10 min captures the realistic distribution; the archive caps each
// run at a bounded row count per table, so it lands inside the same band. They
// mirror cinder's buckets.
var jobDurationBuckets = []float64{1, 5, 10, 30, 60, 120, 300, 600}

// collectors bundles the per-CR metric vectors the operator exposes. The struct
// exists so tests can bind an isolated instance to a private registry;
// production code uses the package-level globalColls registered on
// ctrlmetrics.Registry exactly once. The sub-reconciler duration/error pair
// lives in the shared instrumentation package and is registered by the
// operator's RegisterMetrics; only the per-CR job collectors stay here.
type collectors struct {
	dbSyncTotal       *prometheus.CounterVec
	dbSyncDuration    *prometheus.HistogramVec
	dbArchiveTotal    *prometheus.CounterVec
	dbArchiveDuration *prometheus.HistogramVec
}

// newCollectors builds a fresh set of collector vectors. It does NOT register
// them — callers choose the registry.
func newCollectors() *collectors {
	return &collectors{
		dbSyncTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "nova_operator_db_sync_total",
			Help: "Count of db-sync jobs terminated per Nova CR, labelled by the terminal state.",
		}, []string{"nova", "namespace", "result"}),
		dbSyncDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "nova_operator_db_sync_duration_seconds",
			Help:    "Duration in seconds of terminated db-sync jobs.",
			Buckets: jobDurationBuckets,
		}, []string{"nova", "namespace"}),
		dbArchiveTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "nova_operator_db_archive_total",
			Help: "Count of db-archive jobs terminated per Nova CR, labelled by the terminal state.",
		}, []string{"nova", "namespace", "result"}),
		dbArchiveDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "nova_operator_db_archive_duration_seconds",
			Help:    "Duration in seconds of terminated db-archive jobs.",
			Buckets: jobDurationBuckets,
		}, []string{"nova", "namespace"}),
	}
}

// register adds every vector in c to reg. Returns the first error the
// Registerer emits, typically a duplicate-registration error under the
// controller-runtime global registry if callers register twice.
func (c *collectors) register(reg prometheus.Registerer) error {
	for _, coll := range []prometheus.Collector{
		c.dbSyncTotal,
		c.dbSyncDuration,
		c.dbArchiveTotal,
		c.dbArchiveDuration,
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
func RecordDBSync(nova, namespace, result string, duration time.Duration) {
	globalColls.recordDBSync(nova, namespace, result, duration)
}

// RecordDBArchive increments the db-archive terminal-state counter and records
// one observation in the db-archive duration histogram. The job moves nova's
// soft-deleted rows into the shadow tables, so a run that keeps failing is a
// backlog that only grows; result is expected to be "succeeded" or "failed".
// In-progress jobs MUST NOT call it: the counter represents terminal
// transitions only.
func RecordDBArchive(nova, namespace, result string, duration time.Duration) {
	globalColls.recordDBArchive(nova, namespace, result, duration)
}

// DeleteForNova drops every series tagged with the given Nova name and namespace
// from the per-CR collectors. The sub-reconciler metrics intentionally carry no
// CR labels, so there is nothing to delete there.
func DeleteForNova(name, namespace string) {
	globalColls.deleteForNova(name, namespace)
}

// --- internal methods (bound to an instance) -------------------------------
//
// The public package-level helpers above (RecordDBSync, RecordDBArchive,
// DeleteForNova) are thin wrappers that forward to the matching method below on
// globalColls. The methods are also exercised directly by collectors_test.go
// against an isolated registry.

func (c *collectors) recordDBSync(nova, namespace, result string, duration time.Duration) {
	c.dbSyncTotal.WithLabelValues(nova, namespace, result).Inc()
	c.dbSyncDuration.WithLabelValues(nova, namespace).Observe(duration.Seconds())
}

func (c *collectors) recordDBArchive(nova, namespace, result string, duration time.Duration) {
	c.dbArchiveTotal.WithLabelValues(nova, namespace, result).Inc()
	c.dbArchiveDuration.WithLabelValues(nova, namespace).Observe(duration.Seconds())
}

func (c *collectors) deleteForNova(name, namespace string) {
	labels := prometheus.Labels{"nova": name, "namespace": namespace}
	c.dbSyncTotal.DeletePartialMatch(labels)
	c.dbSyncDuration.DeletePartialMatch(labels)
	c.dbArchiveTotal.DeletePartialMatch(labels)
	c.dbArchiveDuration.DeletePartialMatch(labels)
}
