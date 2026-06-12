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

	"github.com/c5c3/forge/internal/common/instrumentation"
)

const (
	reconcileDurationMetric = "c5c3_operator_reconcile_duration_seconds"
	reconcileErrorsMetric   = "c5c3_operator_reconcile_errors_total"
)

// findMetricByLabels searches the gather output for a MetricFamily with the
// given name and returns the single Metric whose labels equal want. It returns
// nil when no such series exists yet.
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

// histogramSampleCountOn returns the sample_count for the series identified by
// (famName, labels) on the supplied registry, or 0 when the series has not been
// observed yet.
func histogramSampleCountOn(t *testing.T, reg prometheus.Gatherer, famName string, labels map[string]string) uint64 {
	t.Helper()
	m := findMetricByLabels(t, reg, famName, labels)
	if m == nil {
		return 0
	}
	return m.GetHistogram().GetSampleCount()
}

// counterValueOn returns the current value of the counter series identified by
// (famName, labels) on the supplied registry, or 0 when the series is absent.
func counterValueOn(t *testing.T, reg prometheus.Gatherer, famName string, labels map[string]string) float64 {
	t.Helper()
	m := findMetricByLabels(t, reg, famName, labels)
	if m == nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// withTestInstrumenter swaps the package-level instrumenter for one bound to a
// fresh prometheus.NewRegistry() so tests verify the wiring without polluting
// the controller-runtime production registry. The registry
// is returned so tests can gather it directly. Restoration is registered via
// t.Cleanup.
func withTestInstrumenter(t *testing.T) *prometheus.Registry {
	t.Helper()
	reg := prometheus.NewRegistry()
	m := instrumentation.NewMetricsOnRegistry("c5c3_operator", reg)

	prev := instrumenter
	instrumenter = instrumentation.NewInstrumenter(m, subReconcilerConditionTypes)
	t.Cleanup(func() { instrumenter = prev })
	return reg
}

// TestInstrumentSubReconciler_RecordsThroughInstrumenter proves the local
// instrumentSubReconciler delegate records duration on success and attributes
// errors to the condition_type resolved from subReconcilerConditionTypes — the
// behaviour of the shared Instrumenter is exercised in
// internal/common/instrumentation; this test only verifies the c5c3 wiring
// (the map and the subReconcilerMetrics prefix).
func TestInstrumentSubReconciler_RecordsThroughInstrumenter(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := withTestInstrumenter(t)

	const name = "Infrastructure"
	durLabels := map[string]string{"sub_reconciler": name}
	errLabels := map[string]string{"sub_reconciler": name, "condition_type": conditionTypeInfrastructureReady}

	_, err := instrumentSubReconciler(context.Background(), name, func(_ context.Context) (ctrl.Result, error) {
		return ctrl.Result{}, nil
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(histogramSampleCountOn(t, reg, reconcileDurationMetric, durLabels)).
		To(Equal(uint64(1)), "success path must observe exactly one duration sample")

	_, err = instrumentSubReconciler(context.Background(), name, func(_ context.Context) (ctrl.Result, error) {
		return ctrl.Result{}, errors.New("boom")
	})
	g.Expect(err).To(HaveOccurred())
	g.Expect(counterValueOn(t, reg, reconcileErrorsMetric, errLabels)).
		To(Equal(1.0), "error path must attribute the error to the mapped condition_type")
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
