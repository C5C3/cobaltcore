// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the sub-reconciler instrumentation helper.
package controller

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/c5c3/forge/operators/keystone/internal/metrics"
)

// findMetricByLabels searches the gather output for a MetricFamily with the
// given name and returns the single Metric whose labels equal want. It returns
// nil when no such series exists yet (common for counters that have never been
// incremented).
func findMetricByLabels(t *testing.T, g prometheus.Gatherer, famName string, want map[string]string) *dto.Metric {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != famName {
			continue
		}
		for _, m := range fam.GetMetric() {
			if labelsMatch(m.GetLabel(), want) {
				return m
			}
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

// histogramSampleCountOn returns the sample_count for the series identified
// by (famName, labels) on the supplied registry, or 0 when the series has
// not been observed yet.
func histogramSampleCountOn(t *testing.T, reg prometheus.Gatherer, famName string, labels map[string]string) uint64 {
	t.Helper()
	m := findMetricByLabels(t, reg, famName, labels)
	if m == nil {
		return 0
	}
	return m.GetHistogram().GetSampleCount()
}

// counterValueOn returns the current value of the counter series identified
// by (famName, labels) on the supplied registry, or 0 when the series is
// absent.
func counterValueOn(t *testing.T, reg prometheus.Gatherer, famName string, labels map[string]string) float64 {
	t.Helper()
	m := findMetricByLabels(t, reg, famName, labels)
	if m == nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// histogramSampleCount and counterValue read the controller-runtime production
// registry. They exist for tests that exercise the real Reconcile loop (which
// writes to ctrlmetrics.Registry directly) — see keystone_controller_test.go
// and reconcile_database_test.go. New tests should prefer the *On variants
// with an isolated prometheus.NewRegistry() to avoid cross-test pollution.
func histogramSampleCount(t *testing.T, famName string, labels map[string]string) uint64 {
	t.Helper()
	return histogramSampleCountOn(t, ctrlmetrics.Registry, famName, labels)
}

func counterValue(t *testing.T, famName string, labels map[string]string) float64 {
	t.Helper()
	return counterValueOn(t, ctrlmetrics.Registry, famName, labels)
}

// histogramSampleSum returns the running sample_sum for the histogram series
// identified by (famName, labels) on the controller-runtime production
// registry, or 0 when the series has not been observed yet. Use it together
// with histogramSampleCount to validate per-reconcile contributions to a
// shared histogram while remaining tolerant of cross-test prior state.
func histogramSampleSum(t *testing.T, famName string, labels map[string]string) float64 {
	t.Helper()
	m := findMetricByLabels(t, ctrlmetrics.Registry, famName, labels)
	if m == nil {
		return 0
	}
	return m.GetHistogram().GetSampleSum()
}

// withTestRecorder swaps the package-level instrumentation hooks for a
// recorder bound to a fresh prometheus.NewRegistry() so tests verify
// behaviour without polluting the controller-runtime production registry
// The registry is returned so tests can
// gather it directly. Restoration is registered via t.Cleanup.
func withTestRecorder(t *testing.T) *prometheus.Registry {
	t.Helper()
	reg := prometheus.NewRegistry()
	rec := metrics.NewTestRecorder(reg)

	prevObs := instrumentObserveDuration
	prevErr := instrumentRecordError
	instrumentObserveDuration = rec.ObserveReconcileDuration
	instrumentRecordError = rec.RecordReconcileError
	t.Cleanup(func() {
		instrumentObserveDuration = prevObs
		instrumentRecordError = prevErr
	})
	return reg
}

func TestInstrumentSubReconciler_Success_RecordsDuration(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := withTestRecorder(t)

	const name = "TestSuccessSub"
	durLabels := map[string]string{"sub_reconciler": name}
	errLabels := map[string]string{"sub_reconciler": name, "condition_type": ""}

	res, err := instrumentSubReconciler(context.Background(), name, func(_ context.Context) (ctrl.Result, error) {
		return ctrl.Result{}, nil
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	g.Expect(histogramSampleCountOn(t, reg, "keystone_operator_reconcile_duration_seconds", durLabels)).
		To(Equal(uint64(1)),
			"success path must observe exactly one duration sample")
	g.Expect(counterValueOn(t, reg, "keystone_operator_reconcile_errors_total", errLabels)).
		To(Equal(0.0),
			"success path must NOT increment the reconcile_errors counter")
}

func TestInstrumentSubReconciler_Error_RecordsDurationAndError(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := withTestRecorder(t)

	// Use a real sub_reconciler name so condition_type resolves to a non-empty
	// value via subReconcilerConditionTypes.
	const name = "Database"
	const wantCondition = "DatabaseReady"
	durLabels := map[string]string{"sub_reconciler": name}
	errLabels := map[string]string{"sub_reconciler": name, "condition_type": wantCondition}

	sentinel := errors.New("boom")
	_, err := instrumentSubReconciler(context.Background(), name, func(_ context.Context) (ctrl.Result, error) {
		return ctrl.Result{}, sentinel
	})

	g.Expect(err).To(MatchError(sentinel))

	g.Expect(histogramSampleCountOn(t, reg, "keystone_operator_reconcile_duration_seconds", durLabels)).
		To(Equal(uint64(1)),
			"error path must still observe duration")
	g.Expect(counterValueOn(t, reg, "keystone_operator_reconcile_errors_total", errLabels)).
		To(Equal(1.0),
			"error path must increment reconcile_errors for (sub_reconciler, condition_type)")
}

// TestInstrumentSubReconciler_UnknownNameFallback verifies that a sub_reconciler
// name absent from subReconcilerConditionTypes records the error counter with
// condition_type=subReconcilerConditionTypeUnknown ("UNKNOWN") rather than an
// empty string. The static drift guards
// (TestSubReconcilerConditionTypesCoversAllNames /
// TestSubReconcilerConditionTypesCoversAllCallSites) should keep this fallback
// from firing in production, but if drift slips through the alert noise must be
// visible — not silently labelled "".
func TestInstrumentSubReconciler_UnknownNameFallback(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := withTestRecorder(t)

	const name = "TestUnmappedSub"
	errLabels := map[string]string{"sub_reconciler": name, "condition_type": subReconcilerConditionTypeUnknown}
	emptyLabels := map[string]string{"sub_reconciler": name, "condition_type": ""}

	_, err := instrumentSubReconciler(context.Background(), name, func(_ context.Context) (ctrl.Result, error) {
		return ctrl.Result{}, errors.New("boom")
	})
	g.Expect(err).To(HaveOccurred())

	g.Expect(counterValueOn(t, reg, "keystone_operator_reconcile_errors_total", errLabels)).
		To(Equal(1.0),
			"unmapped sub_reconciler name MUST surface as condition_type=%q in the error counter",
			subReconcilerConditionTypeUnknown)
	g.Expect(counterValueOn(t, reg, "keystone_operator_reconcile_errors_total", emptyLabels)).
		To(Equal(0.0),
			"unmapped sub_reconciler name MUST NOT emit reconcile_errors with an empty condition_type label")
}

func TestInstrumentSubReconciler_PanicSafe(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := withTestRecorder(t)

	const name = "TestPanicSub"
	durLabels := map[string]string{"sub_reconciler": name}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = instrumentSubReconciler(context.Background(), name, func(_ context.Context) (ctrl.Result, error) {
			panic("kaboom")
		})
	}()

	g.Expect(recovered).To(Equal("kaboom"),
		"panic must propagate to the caller — instrumentSubReconciler must not recover")

	g.Expect(histogramSampleCountOn(t, reg, "keystone_operator_reconcile_duration_seconds", durLabels)).
		To(Equal(uint64(1)),
			"deferred duration emission MUST fire before the panic unwinds")
}

// TestSubReconcilerConditionTypesCoversAllNames is a drift guard: every
// condition_type value in subReconcilerConditionTypes must be a member of
// subConditionTypes, otherwise an addition to one list without the other will
// silently produce metrics with a stale condition_type label. The reverse
// direction is NOT asserted because subConditionTypes may legitimately contain
// entries (e.g. aggregated conditions) that have no dedicated sub-reconciler.
func TestSubReconcilerConditionTypesCoversAllNames(t *testing.T) {
	g := NewGomegaWithT(t)

	known := make(map[string]struct{}, len(subConditionTypes))
	for _, ct := range subConditionTypes {
		known[ct] = struct{}{}
	}

	for name, condType := range subReconcilerConditionTypes {
		_, ok := known[condType]
		g.Expect(ok).To(BeTrue(),
			"sub_reconciler %q maps to condition_type %q which is not in subConditionTypes — "+
				"update subConditionTypes or fix the mapping", name, condType)
	}
}
