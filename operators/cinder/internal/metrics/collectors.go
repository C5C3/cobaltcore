// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package metrics exposes Prometheus collectors for the Cinder operator.
package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// jobDurationBuckets are the histogram bucket boundaries shared by the three
// per-CR job histograms. A db-sync is measured in seconds-to-minutes, so the
// range 1 s – 10 min captures the realistic distribution; the purge caps each
// delete transaction at a bounded row count and a service-remove is a single
// statement, so both land inside the same band. They mirror glance's buckets.
var jobDurationBuckets = []float64{1, 5, 10, 30, 60, 120, 300, 600}

// collectors bundles the per-CR metric vectors the operator exposes. The struct
// exists so tests can bind an isolated instance to a private registry;
// production code uses the package-level globalColls registered on
// ctrlmetrics.Registry exactly once. The sub-reconciler duration/error pair
// lives in the shared instrumentation package and is registered by the
// operator's RegisterMetrics; only the per-CR job collectors stay here.
type collectors struct {
	dbSyncTotal           *prometheus.CounterVec
	dbSyncDuration        *prometheus.HistogramVec
	dbPurgeTotal          *prometheus.CounterVec
	dbPurgeDuration       *prometheus.HistogramVec
	serviceRemoveTotal    *prometheus.CounterVec
	serviceRemoveDuration *prometheus.HistogramVec
}

// newCollectors builds a fresh set of collector vectors. It does NOT register
// them — callers choose the registry.
func newCollectors() *collectors {
	return &collectors{
		dbSyncTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cinder_operator_db_sync_total",
			Help: "Count of db-sync jobs terminated per Cinder CR, labelled by the terminal state.",
		}, []string{"cinder", "namespace", "result"}),
		dbSyncDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cinder_operator_db_sync_duration_seconds",
			Help:    "Duration in seconds of terminated db-sync jobs.",
			Buckets: jobDurationBuckets,
		}, []string{"cinder", "namespace"}),
		dbPurgeTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cinder_operator_db_purge_total",
			Help: "Count of db-purge jobs terminated per Cinder CR, labelled by the terminal state.",
		}, []string{"cinder", "namespace", "result"}),
		dbPurgeDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cinder_operator_db_purge_duration_seconds",
			Help:    "Duration in seconds of terminated db-purge jobs.",
			Buckets: jobDurationBuckets,
		}, []string{"cinder", "namespace"}),
		serviceRemoveTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cinder_operator_service_remove_total",
			Help: "Count of service-remove jobs terminated per Cinder CR, labelled by the terminal state.",
		}, []string{"cinder", "namespace", "result"}),
		serviceRemoveDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cinder_operator_service_remove_duration_seconds",
			Help:    "Duration in seconds of terminated service-remove jobs.",
			Buckets: jobDurationBuckets,
		}, []string{"cinder", "namespace"}),
	}
}

// register adds every vector in c to reg. Returns the first error the
// Registerer emits, typically a duplicate-registration error under the
// controller-runtime global registry if callers register twice.
func (c *collectors) register(reg prometheus.Registerer) error {
	for _, coll := range []prometheus.Collector{
		c.dbSyncTotal,
		c.dbSyncDuration,
		c.dbPurgeTotal,
		c.dbPurgeDuration,
		c.serviceRemoveTotal,
		c.serviceRemoveDuration,
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
func RecordDBSync(cinder, namespace, result string, duration time.Duration) {
	globalColls.recordDBSync(cinder, namespace, result, duration)
}

// RecordDBPurge increments the db-purge terminal-state counter and records one
// observation in the db-purge duration histogram. result is expected to be
// "succeeded" or "failed". In-progress jobs MUST NOT call it: the counter
// represents terminal transitions only.
func RecordDBPurge(cinder, namespace, result string, duration time.Duration) {
	globalColls.recordDBPurge(cinder, namespace, result, duration)
}

// RecordServiceRemove increments the service-remove terminal-state counter and
// records one observation in the service-remove duration histogram. The job runs
// when a CinderBackend or a CinderBackupBackend detaches, so its failures are
// what keeps a finalizer held; result is expected to be "succeeded" or "failed".
// In-progress jobs MUST NOT call it: the counter represents terminal transitions
// only.
func RecordServiceRemove(cinder, namespace, result string, duration time.Duration) {
	globalColls.recordServiceRemove(cinder, namespace, result, duration)
}

// DeleteForCinder drops every series tagged with the given Cinder name and
// namespace from the per-CR collectors. The sub-reconciler metrics intentionally
// carry no CR labels, so there is nothing to delete there.
func DeleteForCinder(name, namespace string) {
	globalColls.deleteForCinder(name, namespace)
}

// --- internal methods (bound to an instance) -------------------------------
//
// The public package-level helpers above (RecordDBSync, RecordDBPurge,
// RecordServiceRemove, DeleteForCinder) are thin wrappers that forward to the
// matching method below on globalColls. The methods are also exercised directly
// by collectors_test.go against an isolated registry.

func (c *collectors) recordDBSync(cinder, namespace, result string, duration time.Duration) {
	c.dbSyncTotal.WithLabelValues(cinder, namespace, result).Inc()
	c.dbSyncDuration.WithLabelValues(cinder, namespace).Observe(duration.Seconds())
}

func (c *collectors) recordDBPurge(cinder, namespace, result string, duration time.Duration) {
	c.dbPurgeTotal.WithLabelValues(cinder, namespace, result).Inc()
	c.dbPurgeDuration.WithLabelValues(cinder, namespace).Observe(duration.Seconds())
}

func (c *collectors) recordServiceRemove(cinder, namespace, result string, duration time.Duration) {
	c.serviceRemoveTotal.WithLabelValues(cinder, namespace, result).Inc()
	c.serviceRemoveDuration.WithLabelValues(cinder, namespace).Observe(duration.Seconds())
}

func (c *collectors) deleteForCinder(name, namespace string) {
	labels := prometheus.Labels{"cinder": name, "namespace": namespace}
	c.dbSyncTotal.DeletePartialMatch(labels)
	c.dbSyncDuration.DeletePartialMatch(labels)
	c.dbPurgeTotal.DeletePartialMatch(labels)
	c.dbPurgeDuration.DeletePartialMatch(labels)
	c.serviceRemoveTotal.DeletePartialMatch(labels)
	c.serviceRemoveDuration.DeletePartialMatch(labels)
}
