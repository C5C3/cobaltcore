// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package dashboards_test validates the shipped Grafana dashboard JSON
// against the metrics contract exposed by the Horizon operator.
//
// Tests enforce two invariants:
//  1. The dashboard file is syntactically valid JSON and contains the
//     required panel set.
//  2. Every horizon_operator_* metric referenced in a panel target's PromQL
//     expression resolves to a collector that the operator actually
//     registers. This prevents dashboard drift when a metric is renamed or
//     removed.
package dashboards_test

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/c5c3/forge/internal/common/instrumentation"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// dashboardPath is the dashboard file loaded by every test; it lives next
// to this test file so `go test` finds it regardless of the working
// directory.
const dashboardPath = "horizon-operator.json"

// histogramSuffixes are the metric-family suffixes emitted by the
// Prometheus client for histograms. Dashboard expressions use
// `_bucket` with histogram_quantile(), and occasionally `_sum`/`_count`;
// strip these before comparing against the registered base name.
var histogramSuffixes = []string{"_bucket", "_sum", "_count"}

// metricNameRE matches any Prometheus identifier. We later filter the
// matches down to those starting with `horizon_operator_` so we ignore
// PromQL keywords, label names, and unrelated metric names.
var metricNameRE = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)

// dashboard is the minimal subset of the Grafana schema required by the
// tests.
type dashboard struct {
	Title  string  `json:"title"`
	UID    string  `json:"uid"`
	Panels []panel `json:"panels"`
}

type panel struct {
	Type    string   `json:"type"`
	Title   string   `json:"title"`
	Targets []target `json:"targets"`
}

type target struct {
	RefID        string `json:"refId"`
	Expr         string `json:"expr"`
	LegendFormat string `json:"legendFormat"`
}

// loadDashboard reads and decodes the shipped dashboard file, failing the
// test with a clean message if either step fails.
func loadDashboard(t *testing.T) dashboard {
	t.Helper()
	g := NewGomegaWithT(t)

	raw, err := os.ReadFile(dashboardPath)
	g.Expect(err).NotTo(HaveOccurred(), "dashboard file %s must be readable", dashboardPath)

	var d dashboard
	g.Expect(json.Unmarshal(raw, &d)).To(Succeed(),
		"dashboard %s must be valid JSON", dashboardPath)
	return d
}

// TestDashboardParsesAsJSON verifies the shipped Grafana file is valid
// JSON with the required top-level fields and the horizon panel set.
func TestDashboardParsesAsJSON(t *testing.T) {
	g := NewGomegaWithT(t)

	d := loadDashboard(t)

	g.Expect(d.UID).To(Equal("horizon-operator"),
		"dashboard uid must be stable for provisioning")
	g.Expect(d.Title).NotTo(BeEmpty(),
		"dashboard must advertise a human-readable title")
	g.Expect(len(d.Panels)).To(BeNumerically(">=", 3),
		"dashboard must contain the duration, error-rate, and end-to-end panels")

	// Every panel has at least one target with a non-empty PromQL
	// expression; a missing expr means the panel would render blank.
	titles := make(map[string]struct{}, len(d.Panels))
	for i, p := range d.Panels {
		titles[p.Title] = struct{}{}
		g.Expect(p.Targets).NotTo(BeEmpty(),
			"panel %d (%q) must define at least one target", i, p.Title)
		for j, tr := range p.Targets {
			g.Expect(strings.TrimSpace(tr.Expr)).NotTo(BeEmpty(),
				"panel %d target %d expr must not be empty", i, j)
		}
	}

	// The end-to-end reconcile panel surfaces the built-in
	// controller_runtime_reconcile_time_seconds histogram, which covers
	// orchestration and status updates that the per-sub-reconciler histogram
	// does not. Assert it by title so a rename or accidental removal fails.
	g.Expect(titles).To(HaveKey("End-to-end reconcile duration (controller-runtime)"),
		"dashboard must include the controller-runtime end-to-end reconcile panel")
}

// TestDashboardReferencesOnlyRegisteredMetrics guards against dashboard
// drift: every `horizon_operator_*` identifier that appears in a panel
// target must map to a metric the operator actually registers on
// ctrlmetrics.Registry. If a collector is renamed or removed, this test
// fails loudly before the dashboard ships.
func TestDashboardReferencesOnlyRegisteredMetrics(t *testing.T) {
	g := NewGomegaWithT(t)

	// Wake the lazy registration and force every collector's descriptor to
	// appear in Gather() output: Prometheus omits metric families that never
	// received a sample, so each helper is probed once with a disposable
	// label set. This test binary is the only horizon_operator registrant,
	// so no duplicate registration occurs.
	// Registration is now explicit (lazy registration was removed in favour of
	// RegisterMetrics at operator startup), so register the vectors here before
	// probing.
	const probe = "dashboard_test_probe"
	subReconcilerMetrics := instrumentation.NewMetrics("horizon_operator")
	g.Expect(subReconcilerMetrics.Register(ctrlmetrics.Registry)).To(Succeed())
	subReconcilerMetrics.ObserveReconcileDuration(probe, 0)
	subReconcilerMetrics.RecordReconcileError(probe, probe)

	families, err := ctrlmetrics.Registry.Gather()
	g.Expect(err).NotTo(HaveOccurred(),
		"controller-runtime registry must gather without error")

	registered := make(map[string]struct{})
	for _, fam := range families {
		name := fam.GetName()
		if strings.HasPrefix(name, "horizon_operator_") {
			registered[name] = struct{}{}
		}
	}
	g.Expect(registered).NotTo(BeEmpty(),
		"expected at least one horizon_operator_* metric to be registered")

	d := loadDashboard(t)

	seen := make(map[string]struct{})
	for _, p := range d.Panels {
		for _, tr := range p.Targets {
			for _, tok := range metricNameRE.FindAllString(tr.Expr, -1) {
				if !strings.HasPrefix(tok, "horizon_operator_") {
					continue
				}
				seen[stripHistogramSuffix(tok)] = struct{}{}
			}
		}
	}
	g.Expect(seen).NotTo(BeEmpty(),
		"dashboard must reference at least one horizon_operator_* metric")

	for name := range seen {
		_, ok := registered[name]
		g.Expect(ok).To(BeTrue(),
			"dashboard references metric %q which is not registered by the operator",
			name)
	}
}

// stripHistogramSuffix returns the metric-family base name by trimming
// the Prometheus histogram-family suffixes (_bucket, _sum, _count). For
// non-histogram names the input is returned unchanged.
func stripHistogramSuffix(name string) string {
	for _, suf := range histogramSuffixes {
		if strings.HasSuffix(name, suf) {
			return strings.TrimSuffix(name, suf)
		}
	}
	return name
}
