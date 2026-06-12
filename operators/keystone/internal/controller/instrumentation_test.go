// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the sub-reconciler instrumentation wiring.
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

	"github.com/c5c3/forge/internal/common/instrumentation"
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

// withTestInstrumenter swaps the package-level instrumenter for one bound to a
// fresh prometheus.NewRegistry() so tests verify the wiring without polluting
// the controller-runtime production registry. The
// registry is returned so tests can gather it directly. Restoration is
// registered via t.Cleanup.
func withTestInstrumenter(t *testing.T) *prometheus.Registry {
	t.Helper()
	reg := prometheus.NewRegistry()
	m := instrumentation.NewMetricsOnRegistry("keystone_operator", reg)

	prev := instrumenter
	instrumenter = instrumentation.NewInstrumenter(m, subReconcilerConditionTypes)
	t.Cleanup(func() { instrumenter = prev })
	return reg
}

// TestInstrumentSubReconciler_RecordsThroughInstrumenter proves the local
// instrumentSubReconciler delegate records duration on success and attributes
// errors to the condition_type resolved from subReconcilerConditionTypes — the
// behaviour of the shared Instrumenter is exercised in
// internal/common/instrumentation; this test only verifies the keystone
// wiring (the map and the metrics.SubReconciler prefix).
func TestInstrumentSubReconciler_RecordsThroughInstrumenter(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := withTestInstrumenter(t)

	const name = "Database"
	durLabels := map[string]string{"sub_reconciler": name}
	errLabels := map[string]string{"sub_reconciler": name, "condition_type": "DatabaseReady"}

	_, err := instrumentSubReconciler(context.Background(), name, func(_ context.Context) (ctrl.Result, error) {
		return ctrl.Result{}, nil
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(histogramSampleCountOn(t, reg, "keystone_operator_reconcile_duration_seconds", durLabels)).
		To(Equal(uint64(1)), "success path must observe exactly one duration sample")

	_, err = instrumentSubReconciler(context.Background(), name, func(_ context.Context) (ctrl.Result, error) {
		return ctrl.Result{}, errors.New("boom")
	})
	g.Expect(err).To(HaveOccurred())
	g.Expect(counterValueOn(t, reg, "keystone_operator_reconcile_errors_total", errLabels)).
		To(Equal(1.0), "error path must attribute the error to condition_type=DatabaseReady via the map")
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
