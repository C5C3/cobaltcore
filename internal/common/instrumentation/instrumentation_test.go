// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package instrumentation

import (
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// gatherMetric returns the first MetricFamily whose Name matches, or nil so
// callers can distinguish "absent" from "found".
func gatherMetric(t *testing.T, reg prometheus.Gatherer, name string) *dto.MetricFamily {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() == name {
			return fam
		}
	}
	return nil
}

// findMetricByLabels returns the single Metric in famName whose labels equal
// want, or nil when no such series exists yet.
func findMetricByLabels(t *testing.T, g prometheus.Gatherer, famName string, want map[string]string) *dto.Metric {
	t.Helper()
	fam := gatherMetric(t, g, famName)
	if fam == nil {
		return nil
	}
	for _, m := range fam.GetMetric() {
		if labelsMatch(m.GetLabel(), want) {
			return m
		}
	}
	return nil
}

func labelsMatch(got []*dto.LabelPair, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, lp := range got {
		if want[lp.GetName()] != lp.GetValue() {
			return false
		}
	}
	return true
}

func histogramSampleCount(t *testing.T, reg prometheus.Gatherer, famName string, labels map[string]string) uint64 {
	t.Helper()
	m := findMetricByLabels(t, reg, famName, labels)
	if m == nil {
		return 0
	}
	return m.GetHistogram().GetSampleCount()
}

func counterValue(t *testing.T, reg prometheus.Gatherer, famName string, labels map[string]string) float64 {
	t.Helper()
	m := findMetricByLabels(t, reg, famName, labels)
	if m == nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

func TestInstrumenter_Success_RecordsDuration(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	instr := NewInstrumenter(NewMetricsOnRegistry("test_operator", reg), map[string]string{"Foo": "FooReady"})

	const name = "Foo"
	durLabels := map[string]string{"sub_reconciler": name}
	errLabels := map[string]string{"sub_reconciler": name, "condition_type": "FooReady"}

	res, err := instr.Instrument(context.Background(), name, func(_ context.Context) (ctrl.Result, error) {
		return ctrl.Result{}, nil
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	g.Expect(histogramSampleCount(t, reg, "test_operator_reconcile_duration_seconds", durLabels)).
		To(Equal(uint64(1)), "success path must observe exactly one duration sample")
	g.Expect(counterValue(t, reg, "test_operator_reconcile_errors_total", errLabels)).
		To(Equal(0.0), "success path must NOT increment the reconcile_errors counter")
}

func TestInstrumenter_Error_RecordsDurationAndError(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	instr := NewInstrumenter(NewMetricsOnRegistry("test_operator", reg), map[string]string{"Database": "DatabaseReady"})

	const name = "Database"
	const wantCondition = "DatabaseReady"
	durLabels := map[string]string{"sub_reconciler": name}
	errLabels := map[string]string{"sub_reconciler": name, "condition_type": wantCondition}

	sentinel := errors.New("boom")
	_, err := instr.Instrument(context.Background(), name, func(_ context.Context) (ctrl.Result, error) {
		return ctrl.Result{}, sentinel
	})

	g.Expect(err).To(MatchError(sentinel))
	g.Expect(histogramSampleCount(t, reg, "test_operator_reconcile_duration_seconds", durLabels)).
		To(Equal(uint64(1)), "error path must still observe duration")
	g.Expect(counterValue(t, reg, "test_operator_reconcile_errors_total", errLabels)).
		To(Equal(1.0), "error path must increment reconcile_errors for (sub_reconciler, condition_type)")
}

// TestInstrumenter_UnknownNameFallback verifies that a sub_reconciler name
// absent from the condition-type map records the error counter with
// condition_type=ConditionTypeUnknown rather than an empty string.
func TestInstrumenter_UnknownNameFallback(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	instr := NewInstrumenter(NewMetricsOnRegistry("test_operator", reg), map[string]string{})

	const name = "Unmapped"
	unknownLabels := map[string]string{"sub_reconciler": name, "condition_type": ConditionTypeUnknown}
	emptyLabels := map[string]string{"sub_reconciler": name, "condition_type": ""}

	_, err := instr.Instrument(context.Background(), name, func(_ context.Context) (ctrl.Result, error) {
		return ctrl.Result{}, errors.New("boom")
	})
	g.Expect(err).To(HaveOccurred())

	g.Expect(counterValue(t, reg, "test_operator_reconcile_errors_total", unknownLabels)).
		To(Equal(1.0), "unmapped sub_reconciler name MUST surface as condition_type=%q", ConditionTypeUnknown)
	g.Expect(counterValue(t, reg, "test_operator_reconcile_errors_total", emptyLabels)).
		To(Equal(0.0), "unmapped sub_reconciler name MUST NOT emit an empty condition_type label")
}

func TestInstrumenter_PanicSafe(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	instr := NewInstrumenter(NewMetricsOnRegistry("test_operator", reg), map[string]string{})

	const name = "Panicky"
	durLabels := map[string]string{"sub_reconciler": name}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = instr.Instrument(context.Background(), name, func(_ context.Context) (ctrl.Result, error) {
			panic("kaboom")
		})
	}()

	g.Expect(recovered).To(Equal("kaboom"),
		"panic must propagate to the caller — Instrument must not recover")
	g.Expect(histogramSampleCount(t, reg, "test_operator_reconcile_duration_seconds", durLabels)).
		To(Equal(uint64(1)), "deferred duration emission MUST fire before the panic unwinds")
}

func TestReconcileDurationHistogramBuckets(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	m := NewMetricsOnRegistry("test_operator", reg)

	// Bucket enumeration requires at least one observation.
	m.ObserveReconcileDuration("dummy", 0)

	fam := gatherMetric(t, reg, "test_operator_reconcile_duration_seconds")
	g.Expect(fam).NotTo(BeNil(), "duration histogram must be registered")
	g.Expect(fam.GetType()).To(Equal(dto.MetricType_HISTOGRAM))

	got := make([]float64, 0, len(reconcileDurationBuckets))
	for _, b := range fam.GetMetric()[0].GetHistogram().GetBucket() {
		got = append(got, b.GetUpperBound())
	}
	g.Expect(got).To(Equal(reconcileDurationBuckets),
		"reconcile_duration bucket boundaries MUST be the contract set")
}

func TestReconcileErrorsCounterLabels(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	m := NewMetricsOnRegistry("test_operator", reg)

	m.RecordReconcileError("fernet", "FernetKeysReady")

	fam := gatherMetric(t, reg, "test_operator_reconcile_errors_total")
	g.Expect(fam).NotTo(BeNil())
	g.Expect(fam.GetType()).To(Equal(dto.MetricType_COUNTER))
	g.Expect(fam.GetMetric()).To(HaveLen(1))

	names := make([]string, 0, 2)
	values := map[string]string{}
	for _, l := range fam.GetMetric()[0].GetLabel() {
		names = append(names, l.GetName())
		values[l.GetName()] = l.GetValue()
	}
	g.Expect(names).To(ConsistOf("sub_reconciler", "condition_type"),
		"reconcile_errors label set MUST be exactly {sub_reconciler, condition_type}")
	g.Expect(values["sub_reconciler"]).To(Equal("fernet"))
	g.Expect(values["condition_type"]).To(Equal("FernetKeysReady"))
	g.Expect(fam.GetMetric()[0].GetCounter().GetValue()).To(Equal(1.0))
}

func TestErrorCounterNotIncrementedOnSuccess(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	m := NewMetricsOnRegistry("test_operator", reg)

	m.ObserveReconcileDuration("fernet", 42*time.Millisecond)

	fam := gatherMetric(t, reg, "test_operator_reconcile_errors_total")
	if fam != nil {
		for _, metric := range fam.GetMetric() {
			g.Expect(metric.GetCounter().GetValue()).To(Equal(0.0),
				"success path must never increment reconcile_errors")
		}
	}
}

// TestSubReconcilerMetricsHaveNoCRLabels is the cardinality drift-guard:
// neither sub-reconciler metric may carry a per-CR label (e.g. keystone,
// namespace) — that would explode cardinality to O(#CRs × #sub-reconcilers).
func TestSubReconcilerMetricsHaveNoCRLabels(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	m := NewMetricsOnRegistry("test_operator", reg)

	m.ObserveReconcileDuration("fernet", time.Millisecond)
	m.RecordReconcileError("fernet", "FernetKeysReady")

	forbidden := map[string]struct{}{"keystone": {}, "namespace": {}, "controlplane": {}}
	for _, name := range []string{
		"test_operator_reconcile_duration_seconds",
		"test_operator_reconcile_errors_total",
	} {
		fam := gatherMetric(t, reg, name)
		g.Expect(fam).NotTo(BeNil(), "metric %s must be registered", name)
		for _, metric := range fam.GetMetric() {
			for _, l := range metric.GetLabel() {
				_, bad := forbidden[l.GetName()]
				g.Expect(bad).To(BeFalse(),
					"metric %s must NOT carry the per-CR label %q", name, l.GetName())
			}
		}
	}
}

// TestLazyRegistrationIdempotent verifies that a lazy Metrics instance
// registers on the controller-runtime registry exactly once and that repeated
// recording does not panic. A unique prefix avoids colliding with any other
// lazy registration in this test binary.
func TestLazyRegistrationIdempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	m := NewMetrics("lazy_instr_test_operator")

	g.Expect(func() {
		m.ObserveReconcileDuration("foo", time.Millisecond)
		m.ObserveReconcileDuration("foo", time.Millisecond)
		m.RecordReconcileError("foo", "FooReady")
	}).NotTo(Panic(), "lazy registration must be idempotent across repeated records")

	durLabels := map[string]string{"sub_reconciler": "foo"}
	g.Expect(histogramSampleCount(t, ctrlmetrics.Registry, "lazy_instr_test_operator_reconcile_duration_seconds", durLabels)).
		To(Equal(uint64(2)), "both observations must land on the same registered vector")
}

// TestNewMetricsOnRegistryIsolation proves a private-registry instance does
// not leak its series into the controller-runtime production registry.
func TestNewMetricsOnRegistryIsolation(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	m := NewMetricsOnRegistry("isolated_test_operator", reg)

	m.ObserveReconcileDuration("foo", time.Millisecond)

	g.Expect(gatherMetric(t, reg, "isolated_test_operator_reconcile_duration_seconds")).NotTo(BeNil())
	g.Expect(gatherMetric(t, ctrlmetrics.Registry, "isolated_test_operator_reconcile_duration_seconds")).
		To(BeNil(), "private-registry instance must NOT register on the controller-runtime registry")
}
