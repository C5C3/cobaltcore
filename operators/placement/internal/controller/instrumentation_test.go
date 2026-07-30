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

// withTestInstrumenter swaps the package-level instrumenter for one bound to a
// fresh prometheus.NewRegistry() so tests verify the wiring without polluting
// the controller-runtime production registry. Restoration is registered via
// t.Cleanup.
func withTestInstrumenter(t *testing.T) *prometheus.Registry {
	t.Helper()
	reg := prometheus.NewRegistry()
	m := instrumentation.NewMetricsOnRegistry("placement_operator", reg)

	prev := instrumenter
	instrumenter = instrumentation.NewInstrumenter(m, subReconcilerConditionTypes)
	t.Cleanup(func() { instrumenter = prev })
	return reg
}

// TestInstrumenterInstrument_RecordsMetrics proves the package instrumenter
// records duration on success and attributes errors to the condition_type
// resolved from subReconcilerConditionTypes — the behaviour of the shared
// Instrumenter is exercised in internal/common/instrumentation; this test only
// verifies the placement wiring (the map and the placement_operator metrics
// prefix).
func TestInstrumenterInstrument_RecordsMetrics(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := withTestInstrumenter(t)

	const name = "Config"
	durLabels := map[string]string{"sub_reconciler": name}
	errLabels := map[string]string{"sub_reconciler": name, "condition_type": "SecretsReady"}

	_, err := instrumenter.Instrument(context.Background(), name, func(_ context.Context) (ctrl.Result, error) {
		return ctrl.Result{}, nil
	})
	g.Expect(err).NotTo(HaveOccurred())
	m := findMetricByLabels(t, reg, "placement_operator_reconcile_duration_seconds", durLabels)
	g.Expect(m).NotTo(BeNil())
	g.Expect(m.GetHistogram().GetSampleCount()).To(Equal(uint64(1)),
		"success path must observe exactly one duration sample")

	_, err = instrumenter.Instrument(context.Background(), name, func(_ context.Context) (ctrl.Result, error) {
		return ctrl.Result{}, errors.New("boom")
	})
	g.Expect(err).To(HaveOccurred())
	m = findMetricByLabels(t, reg, "placement_operator_reconcile_errors_total", errLabels)
	g.Expect(m).NotTo(BeNil())
	g.Expect(m.GetCounter().GetValue()).To(Equal(1.0),
		"error path must attribute the error to condition_type=SecretsReady via the map")
}

// TestSubReconcilerConditionTypesCoversAllNames is one half of the drift guard:
// every condition_type value in subReconcilerConditionTypes must be a member of
// subConditionTypes, otherwise an addition to one list without the other will
// silently produce metrics with a stale condition_type label. The repo's
// condition-coverage audit keys on this exact test.
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

// TestPipelineStepNamesAreMapped is the other half of the drift guard, walking
// the mapping in the opposite direction: every step name the pipeline actually
// runs must be a key in subReconcilerConditionTypes, otherwise its error series
// carries condition_type=UNKNOWN. The names come from the pipeline itself
// (pipelineSteps plus the parallel group's members), so a step added to Reconcile
// without a mapping entry fails here rather than in a Prometheus query.
func TestPipelineStepNamesAreMapped(t *testing.T) {
	g := NewGomegaWithT(t)
	r := &PlacementReconciler{}

	var names []string
	// The parallel-group step carries no name of its own by design: it
	// self-instruments its members (see commonreconcile.Step), which are
	// collected from parallelSteps below.
	for _, step := range r.pipelineSteps(testPlacement()) {
		if step.Name != "" {
			names = append(names, step.Name)
		}
	}
	for _, sub := range r.parallelSteps() {
		names = append(names, sub.Name)
		g.Expect(subReconcilerConditionTypes[sub.Name]).To(Equal(sub.ConditionType),
			"parallel member %q reports condition %q but the metrics map says %q",
			sub.Name, sub.ConditionType, subReconcilerConditionTypes[sub.Name])
	}

	g.Expect(names).To(HaveLen(len(subReconcilerConditionTypes)),
		"every mapped sub_reconciler must correspond to exactly one pipeline step")
	for _, name := range names {
		g.Expect(subReconcilerConditionTypes).To(HaveKey(name),
			"pipeline step %q has no subReconcilerConditionTypes entry, so its errors "+
				"would be attributed to condition_type=UNKNOWN", name)
	}
}

// TestRegisterMetrics_ExposesOperatorMetrics proves the startup registration
// main.go performs puts both operator metric families on the controller-runtime
// registry under the placement_operator prefix. A vector without children is not
// gathered, so one instrumented call has to run first.
func TestRegisterMetrics_ExposesOperatorMetrics(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(RegisterMetrics()).To(Succeed())

	const name = "Database"
	_, err := instrumenter.Instrument(context.Background(), name, func(_ context.Context) (ctrl.Result, error) {
		return ctrl.Result{}, errors.New("boom")
	})
	g.Expect(err).To(HaveOccurred())

	g.Expect(findMetricByLabels(t, ctrlmetrics.Registry, "placement_operator_reconcile_duration_seconds",
		map[string]string{"sub_reconciler": name})).NotTo(BeNil())
	g.Expect(findMetricByLabels(t, ctrlmetrics.Registry, "placement_operator_reconcile_errors_total",
		map[string]string{"sub_reconciler": name, "condition_type": "DatabaseReady"})).NotTo(BeNil())
}
