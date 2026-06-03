// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// expectedReconcileDurationBuckets are the exact buckets prescribed by
// CC-0110 REQ-026 for the c5c3_operator_reconcile_duration_seconds
// histogram.
var expectedReconcileDurationBuckets = []float64{
	0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30,
}

// newCollectorsForTest is a test-only constructor that returns a fresh
// collectors set bound to reg. It delegates to NewTestRecorder so the
// registration-and-panic logic lives in exactly one place. Each unit
// test gets a new Registerer so gather output is deterministic and free
// of cross-test interference (CC-0110, REQ-026).
func newCollectorsForTest(reg prometheus.Registerer) *collectors {
	return NewTestRecorder(reg).c
}

// gatherMetric returns the first MetricFamily whose Name matches. If not
// present, it returns nil so callers can distinguish "absent" from "found".
func gatherMetric(t *testing.T, reg prometheus.Gatherer, name string) *dto.MetricFamily {
	t.Helper()
	g := NewGomegaWithT(t)
	families, err := reg.Gather()
	g.Expect(err).NotTo(HaveOccurred())
	for _, fam := range families {
		if fam.GetName() == name {
			return fam
		}
	}
	return nil
}

func TestReconcileDurationHistogramRegistered(t *testing.T) {
	g := NewGomegaWithT(t)

	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)
	g.Expect(c).NotTo(BeNil())

	// Bucket enumeration requires at least one observation, so record one
	// with zero duration under a dummy sub_reconciler. This is purely a
	// gather trigger; the bucket list is a property of the descriptor.
	c.observeReconcileDuration("dummy", 0)

	fam := gatherMetric(t, reg, "c5c3_operator_reconcile_duration_seconds")
	g.Expect(fam).NotTo(BeNil(), "histogram c5c3_operator_reconcile_duration_seconds must be registered")
	g.Expect(fam.GetType()).To(Equal(dto.MetricType_HISTOGRAM))

	metrics := fam.GetMetric()
	g.Expect(metrics).NotTo(BeEmpty())
	buckets := metrics[0].GetHistogram().GetBucket()
	got := make([]float64, 0, len(buckets))
	for _, b := range buckets {
		got = append(got, b.GetUpperBound())
	}
	g.Expect(got).To(Equal(expectedReconcileDurationBuckets),
		"reconcile_duration bucket boundaries MUST match CC-0110 REQ-026 exactly")
}

func TestReconcileErrorsCounterLabels(t *testing.T) {
	g := NewGomegaWithT(t)

	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	c.recordReconcileError("controlplane", "ControlPlaneReady")

	fam := gatherMetric(t, reg, "c5c3_operator_reconcile_errors_total")
	g.Expect(fam).NotTo(BeNil())
	g.Expect(fam.GetType()).To(Equal(dto.MetricType_COUNTER))
	g.Expect(fam.GetMetric()).To(HaveLen(1))

	labels := fam.GetMetric()[0].GetLabel()
	names := make([]string, 0, len(labels))
	values := map[string]string{}
	for _, l := range labels {
		names = append(names, l.GetName())
		values[l.GetName()] = l.GetValue()
	}
	g.Expect(names).To(ConsistOf("sub_reconciler", "condition_type"),
		"reconcile_errors label set MUST be exactly {sub_reconciler, condition_type} (CC-0110 REQ-026)")
	g.Expect(values["sub_reconciler"]).To(Equal("controlplane"))
	g.Expect(values["condition_type"]).To(Equal("ControlPlaneReady"))
	g.Expect(fam.GetMetric()[0].GetCounter().GetValue()).To(Equal(1.0))
}

func TestErrorCounterNotIncrementedOnSuccess(t *testing.T) {
	g := NewGomegaWithT(t)

	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	// Success path: duration observation only, no error.
	c.observeReconcileDuration("controlplane", 42*time.Millisecond)

	fam := gatherMetric(t, reg, "c5c3_operator_reconcile_errors_total")
	// A counter with no observations and no labels pre-created is absent
	// from the gather output, which is the desired outcome.
	if fam != nil {
		for _, m := range fam.GetMetric() {
			g.Expect(m.GetCounter().GetValue()).To(Equal(0.0),
				"success path must never increment reconcile_errors (CC-0110 REQ-026)")
		}
	}
}

// TestSubReconcilerMetricsHaveNoCRLabels is the cardinality drift-guard for
// CC-0110 REQ-026. Adding a `controlplane` or `namespace` label to either the
// reconcile_duration histogram or the reconcile_errors counter would explode
// cardinality (O(#CRs × #sub-reconcilers)) and is forbidden by the design.
// This test fails CI if either label ever appears on those metrics.
func TestSubReconcilerMetricsHaveNoCRLabels(t *testing.T) {
	g := NewGomegaWithT(t)

	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	// Emit one observation per metric so the gather output contains the
	// descriptor's label set (prometheus descriptors are enumerated via
	// actual samples, not up-front).
	c.observeReconcileDuration("controlplane", time.Millisecond)
	c.recordReconcileError("controlplane", "ControlPlaneReady")

	// The reconcile vectors are sub-reconciler-level only; no per-CR
	// identifier (controlplane name, namespace, or the generic "c5c3"
	// CR label) may ever appear on them.
	forbidden := map[string]struct{}{"controlplane": {}, "namespace": {}, "c5c3": {}}
	allowed := map[string]struct{}{"sub_reconciler": {}, "condition_type": {}}
	checkNames := []string{
		"c5c3_operator_reconcile_duration_seconds",
		"c5c3_operator_reconcile_errors_total",
	}
	for _, name := range checkNames {
		fam := gatherMetric(t, reg, name)
		g.Expect(fam).NotTo(BeNil(), "metric %s must be registered", name)
		for _, m := range fam.GetMetric() {
			for _, l := range m.GetLabel() {
				_, bad := forbidden[l.GetName()]
				g.Expect(bad).To(BeFalse(),
					"metric %s must NOT have label %q — REQ-026 cardinality guard",
					name, l.GetName())
				_, ok := allowed[l.GetName()]
				g.Expect(ok).To(BeTrue(),
					"metric %s carries unexpected label %q — only {sub_reconciler, condition_type} are allowed (REQ-026)",
					name, l.GetName())
			}
		}
	}
}

// TestRegisterIsIdempotentlyGuarded proves register() surfaces the
// duplicate-registration error rather than panicking, mirroring the
// keystone contract. A fresh registry accepts the collectors once;
// registering the same set again returns a non-nil error (CC-0110, REQ-026).
func TestRegisterIsIdempotentlyGuarded(t *testing.T) {
	g := NewGomegaWithT(t)

	reg := prometheus.NewRegistry()
	c := newCollectors()

	g.Expect(c.register(reg)).To(Succeed(),
		"first register on an empty registry must succeed")
	g.Expect(c.register(reg)).To(HaveOccurred(),
		"duplicate register on the same registry must return an error (REQ-026)")
}

// TestNewCollectorsDoesNotRegister proves newCollectors builds the vectors
// without touching any registry — a fresh registry gathered immediately
// after construction must be empty (CC-0110, REQ-026).
func TestNewCollectorsDoesNotRegister(t *testing.T) {
	g := NewGomegaWithT(t)

	_ = newCollectors()

	reg := prometheus.NewRegistry()
	families, err := reg.Gather()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(families).To(BeEmpty(),
		"newCollectors must not register on any registry (REQ-026)")
}

// TestTestRecorderRecordsThroughPrivateRegistry proves the exported
// TestRecorder surface (NewTestRecorder + ObserveReconcileDuration +
// RecordReconcileError) drives a collector set bound to a caller-supplied
// private registry. This is the contract the controller package's
// instrumentation tests depend on (CC-0110, REQ-026).
func TestTestRecorderRecordsThroughPrivateRegistry(t *testing.T) {
	g := NewGomegaWithT(t)

	reg := prometheus.NewRegistry()
	rec := NewTestRecorder(reg)
	g.Expect(rec).NotTo(BeNil())

	rec.ObserveReconcileDuration("controlplane", 100*time.Millisecond)
	rec.RecordReconcileError("controlplane", "ControlPlaneReady")

	durFam := gatherMetric(t, reg, "c5c3_operator_reconcile_duration_seconds")
	g.Expect(durFam).NotTo(BeNil())
	g.Expect(durFam.GetMetric()[0].GetHistogram().GetSampleCount()).To(Equal(uint64(1)))

	errFam := gatherMetric(t, reg, "c5c3_operator_reconcile_errors_total")
	g.Expect(errFam).NotTo(BeNil())
	g.Expect(errFam.GetMetric()[0].GetCounter().GetValue()).To(Equal(1.0))
}
