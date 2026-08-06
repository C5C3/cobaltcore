// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package metrics exposes Prometheus collectors for the Barbican operator.
package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// dbSyncDurationBuckets are the histogram bucket boundaries for
// barbican_operator_db_sync_duration_seconds. DB sync jobs are measured in
// seconds-to-minutes, so the range from 1 s to 10 min captures the realistic
// distribution. Mirrors keystone's buckets.
// barbican_operator_db_clean_duration_seconds shares them: the clean-up
// hard-deletes the rows that fell out of the retention window, a delete volume
// that lands in the same seconds-to-minutes band.
var dbSyncDurationBuckets = []float64{1, 5, 10, 30, 60, 120, 300, 600}

// collectors bundles the per-CR metric vectors the operator exposes. The struct
// exists so tests can bind an isolated instance to a private registry;
// production code uses the package-level globalColls registered on
// ctrlmetrics.Registry exactly once. The sub-reconciler duration/error pair
// lives in the shared instrumentation package and is registered by the
// operator's RegisterMetrics; only the per-CR job and secret-store collectors
// stay here.
type collectors struct {
	dbSyncTotal      *prometheus.CounterVec
	dbSyncDuration   *prometheus.HistogramVec
	dbCleanTotal     *prometheus.CounterVec
	dbCleanDuration  *prometheus.HistogramVec
	storeRemintTotal *prometheus.CounterVec
}

// newCollectors builds a fresh set of collector vectors. It does NOT register
// them; callers choose the registry.
func newCollectors() *collectors {
	return &collectors{
		dbSyncTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "barbican_operator_db_sync_total",
			Help: "Count of db-sync jobs terminated per Barbican CR, labelled by the terminal state.",
		}, []string{"barbican", "namespace", "result"}),
		dbSyncDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "barbican_operator_db_sync_duration_seconds",
			Help:    "Duration in seconds of terminated db-sync jobs.",
			Buckets: dbSyncDurationBuckets,
		}, []string{"barbican", "namespace"}),
		dbCleanTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "barbican_operator_db_clean_total",
			Help: "Count of db-clean jobs terminated per Barbican CR, labelled by the terminal state.",
		}, []string{"barbican", "namespace", "result"}),
		dbCleanDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "barbican_operator_db_clean_duration_seconds",
			Help:    "Duration in seconds of terminated db-clean jobs.",
			Buckets: dbSyncDurationBuckets,
		}, []string{"barbican", "namespace"}),
		storeRemintTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "barbican_operator_secretstore_remints_total",
			Help: "Count of AppRole secret-ID re-mints per BarbicanSecretStore CR, labelled by what triggered them.",
		}, []string{"store", "namespace", "trigger"}),
	}
}

// register adds every vector in c to reg. Returns the first error the
// Registerer emits, typically a duplicate-registration error under the
// controller-runtime global registry if callers register twice.
func (c *collectors) register(reg prometheus.Registerer) error {
	for _, coll := range []prometheus.Collector{
		c.dbSyncTotal,
		c.dbSyncDuration,
		c.dbCleanTotal,
		c.dbCleanDuration,
		c.storeRemintTotal,
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
// The operator's RegisterMetrics calls it during setup.
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
func RecordDBSync(barbican, namespace, result string, duration time.Duration) {
	globalColls.recordDBSync(barbican, namespace, result, duration)
}

// RecordDBClean increments the db-clean terminal-state counter and records one
// observation in the db-clean duration histogram. result is expected to be
// "succeeded" or "failed". In-progress jobs MUST NOT call it: the counter
// represents terminal transitions only.
func RecordDBClean(barbican, namespace, result string, duration time.Duration) {
	globalColls.recordDBClean(barbican, namespace, result, duration)
}

// RecordSecretStoreRemint counts one AppRole secret-ID re-mint for the named
// BarbicanSecretStore. trigger is expected to be "proactive" (the operator
// refreshed the credential before its TTL lapsed) or "reactive" (the credential
// was already rejected, so the operator minted a replacement). The split is
// what tells a healthy rotation schedule apart from one that keeps arriving too
// late.
func RecordSecretStoreRemint(store, namespace, trigger string) {
	globalColls.recordSecretStoreRemint(store, namespace, trigger)
}

// DeleteForBarbican drops every series tagged with the given Barbican name and
// namespace from the per-CR collectors. The sub-reconciler metrics
// intentionally carry no CR labels, so there is nothing to delete there. The
// secret-store re-mint counter is keyed by the store CR rather than by its
// parent Barbican, so the store controller's finalizer clears it through
// DeleteForBarbicanSecretStore.
func DeleteForBarbican(name, namespace string) {
	globalColls.deleteForBarbican(name, namespace)
}

// DeleteForBarbicanSecretStore drops the re-mint series tagged with the given
// BarbicanSecretStore name and namespace. The counter is keyed by the store CR,
// so the store controller's finalizer clears it when the store goes away, not
// when its parent Barbican does.
func DeleteForBarbicanSecretStore(store, namespace string) {
	globalColls.deleteForBarbicanSecretStore(store, namespace)
}

// --- internal methods (bound to an instance) -------------------------------
//
// The public package-level helpers above (RecordDBSync, RecordDBClean,
// RecordSecretStoreRemint, DeleteForBarbican, DeleteForBarbicanSecretStore) are
// thin wrappers that forward to the matching method below on globalColls. The
// methods are also exercised directly by collectors_test.go against an isolated
// registry.

func (c *collectors) recordDBSync(barbican, namespace, result string, duration time.Duration) {
	c.dbSyncTotal.WithLabelValues(barbican, namespace, result).Inc()
	c.dbSyncDuration.WithLabelValues(barbican, namespace).Observe(duration.Seconds())
}

func (c *collectors) recordDBClean(barbican, namespace, result string, duration time.Duration) {
	c.dbCleanTotal.WithLabelValues(barbican, namespace, result).Inc()
	c.dbCleanDuration.WithLabelValues(barbican, namespace).Observe(duration.Seconds())
}

func (c *collectors) recordSecretStoreRemint(store, namespace, trigger string) {
	c.storeRemintTotal.WithLabelValues(store, namespace, trigger).Inc()
}

func (c *collectors) deleteForBarbican(name, namespace string) {
	labels := prometheus.Labels{"barbican": name, "namespace": namespace}
	c.dbSyncTotal.DeletePartialMatch(labels)
	c.dbSyncDuration.DeletePartialMatch(labels)
	c.dbCleanTotal.DeletePartialMatch(labels)
	c.dbCleanDuration.DeletePartialMatch(labels)
}

func (c *collectors) deleteForBarbicanSecretStore(store, namespace string) {
	c.storeRemintTotal.DeletePartialMatch(prometheus.Labels{"store": store, "namespace": namespace})
}
