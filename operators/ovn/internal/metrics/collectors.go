// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package metrics exposes Prometheus collectors for the OVN operator.
package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// backupDurationBuckets are the histogram bucket boundaries for
// ovn_operator_backup_duration_seconds. A backup run copies both OVN databases
// with ovsdb-client, which on a logical model of any realistic size finishes in
// seconds, so the range from 1 s to 10 min covers the distribution with the
// upper buckets left for a run that is starved of I/O. The values mirror the
// db-sync buckets of the sibling operators, so a dashboard can put the two side
// by side.
var backupDurationBuckets = []float64{1, 5, 10, 30, 60, 120, 300, 600}

// collectors bundles the per-CR metric vectors the operator exposes. The struct
// exists so tests can bind an isolated instance to a private registry;
// production code uses the package-level globalColls registered on
// ctrlmetrics.Registry exactly once.
type collectors struct {
	backupTotal    *prometheus.CounterVec
	backupDuration *prometheus.HistogramVec
}

// newCollectors builds a fresh set of collector vectors. It does NOT register
// them; callers choose the registry.
func newCollectors() *collectors {
	return &collectors{
		backupTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ovn_operator_backup_total",
			Help: "Count of backup jobs terminated per OVNCentral CR, labelled by the terminal state.",
		}, []string{"ovncentral", "namespace", "result"}),
		backupDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ovn_operator_backup_duration_seconds",
			Help:    "Duration in seconds of terminated backup jobs.",
			Buckets: backupDurationBuckets,
		}, []string{"ovncentral", "namespace"}),
	}
}

// register adds every vector in c to reg. Returns the first error the
// Registerer emits, typically a duplicate-registration error under the
// controller-runtime global registry if callers register twice.
func (c *collectors) register(reg prometheus.Registerer) error {
	for _, coll := range []prometheus.Collector{
		c.backupTotal,
		c.backupDuration,
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
// registry exactly once and returns any registration error, so a duplicate
// registration surfaces as a clean operator-startup failure rather than a
// mid-reconcile panic. Repeated calls return the memoized first result.
func Register() error {
	registerOnce.Do(func() {
		registerErr = globalColls.register(ctrlmetrics.Registry)
	})
	return registerErr
}

// RecordBackup increments the backup terminal-state counter and records one
// observation in the backup duration histogram. result is expected to be
// "succeeded" or "failed". A run that has not finished MUST NOT call it: the
// counter represents terminal transitions only.
func RecordBackup(ovncentral, namespace, result string, duration time.Duration) {
	globalColls.recordBackup(ovncentral, namespace, result, duration)
}

// DeleteForOVNCentral drops every series tagged with the given OVNCentral name
// and namespace, so a deleted CR does not leak a time series for the operator's
// lifetime.
func DeleteForOVNCentral(name, namespace string) {
	globalColls.deleteForOVNCentral(name, namespace)
}

// --- internal methods (bound to an instance) -------------------------------
//
// The package-level helpers above forward to the matching method below on
// globalColls. The methods are also exercised directly by collectors_test.go
// against an isolated registry.

func (c *collectors) recordBackup(ovncentral, namespace, result string, duration time.Duration) {
	c.backupTotal.WithLabelValues(ovncentral, namespace, result).Inc()
	c.backupDuration.WithLabelValues(ovncentral, namespace).Observe(duration.Seconds())
}

func (c *collectors) deleteForOVNCentral(name, namespace string) {
	labels := prometheus.Labels{"ovncentral": name, "namespace": namespace}
	c.backupTotal.DeletePartialMatch(labels)
	c.backupDuration.DeletePartialMatch(labels)
}
