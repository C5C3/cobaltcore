// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package dashboards_test validates the shipped Grafana dashboard JSON
// against the metrics contract exposed by the Barbican operator.
//
// Tests enforce two invariants:
//  1. The dashboard file is syntactically valid JSON and contains the
//     required panel set.
//  2. Every metric referenced in a panel target's PromQL expression
//     resolves to a collector that the operator actually registers. This
//     prevents dashboard drift when a metric is renamed or removed.
package dashboards_test

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/c5c3/cobaltcore/internal/common/instrumentation"
	"github.com/c5c3/cobaltcore/operators/barbican/internal/metrics"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// dashboardPath is the dashboard file loaded by every test; it lives next
// to this test file so `go test` finds it regardless of the working
// directory.
const dashboardPath = "barbican-operator.json"

// histogramSuffixes are the metric-family suffixes emitted by the
// Prometheus client for histograms. Dashboard expressions use
// `_bucket` with histogram_quantile(), and occasionally `_sum`/`_count`;
// strip these before comparing against the registered base name.
var histogramSuffixes = []string{"_bucket", "_sum", "_count"}

// metricNameRE matches any Prometheus identifier. We later filter the
// matches down to those starting with `barbican_operator_` so we ignore
// PromQL keywords, label names, and unrelated metric names.
var metricNameRE = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)

// dashboard is the minimal subset of the Grafana schema required by the
// tests. Decoding only these fields keeps us resilient to arbitrary
// schema keys (templating, fieldConfig, etc.).
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
// JSON with the required top-level fields and the full panel set.
func TestDashboardParsesAsJSON(t *testing.T) {
	g := NewGomegaWithT(t)

	d := loadDashboard(t)

	g.Expect(d.UID).To(Equal("barbican-operator"),
		"dashboard uid must be stable for provisioning")
	g.Expect(d.Title).NotTo(BeEmpty(),
		"dashboard must advertise a human-readable title")
	g.Expect(len(d.Panels)).To(BeNumerically(">=", 6),
		"dashboard must contain the six panels of the operator dashboard set")

	// Every panel in the canonical set has at least one target with a
	// non-empty PromQL expression; a missing expr means the panel would
	// render blank in Grafana.
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

	// The db-sync panel surfaces the per-CR barbican_operator_db_sync_*
	// collectors, which the sibling operators also expose; assert it by title
	// so a rename or accidental removal fails here.
	g.Expect(titles).To(HaveKey("db_sync duration p95 and failure rate"),
		"dashboard must include the db-sync duration/failure panel")

	// The db-clean twin covers the recurring clean-up CronJob, which no
	// sibling operator runs.
	g.Expect(titles).To(HaveKey("db_clean duration p95 and failure rate"),
		"dashboard must include the db-clean duration/failure panel")

	// The re-mint panel is the only view on AppRole credential rotation; a
	// reactive re-mint means the store was already serving a rejected
	// credential, so the panel must not silently disappear.
	g.Expect(titles).To(HaveKey("Secret-store AppRole re-mints per trigger"),
		"dashboard must include the secret-store re-mint panel")

	// The end-to-end reconcile panel surfaces the built-in
	// controller_runtime_reconcile_time_seconds histogram, which covers
	// orchestration and status updates that the per-sub-reconciler histogram
	// does not. Assert it by title so a rename or accidental removal fails here.
	g.Expect(titles).To(HaveKey("End-to-end reconcile duration (controller-runtime)"),
		"dashboard must include the controller-runtime end-to-end reconcile panel")
}

// TestDashboardReferencesOnlyRegisteredMetrics guards against dashboard
// drift: every `barbican_operator_*` identifier that appears in a panel
// target must map to a metric the operator actually registers on
// ctrlmetrics.Registry. If a collector is renamed or removed, this test
// fails loudly before the dashboard ships.
func TestDashboardReferencesOnlyRegisteredMetrics(t *testing.T) {
	g := NewGomegaWithT(t)

	// Force every collector's descriptor to appear in Gather() output.
	// Prometheus omits metric families that never received a sample, so each
	// public helper is called once with a disposable label set. Test-only
	// labels keep the probe samples clearly distinguishable if they ever leak
	// into a live registry.
	//
	// DECISION: mirror the two surfaces the controller's RegisterMetrics
	// exposes at operator startup: an instrumentation.NewMetrics instance
	// carrying the production prefix, and the per-CR metrics.Register. The
	// controller registers the same prefix on the same registry, so calling
	// its RegisterMetrics here in addition would be a duplicate registration;
	// this test binary is the only barbican_operator registrant, so the
	// mirrored instance yields the exact metric families production emits.
	const probe = "dashboard_test_probe"
	subReconcilerMetrics := instrumentation.NewMetrics("barbican_operator")
	g.Expect(subReconcilerMetrics.Register(ctrlmetrics.Registry)).To(Succeed())
	g.Expect(metrics.Register()).To(Succeed())
	subReconcilerMetrics.ObserveReconcileDuration(probe, 0)
	subReconcilerMetrics.RecordReconcileError(probe, probe)
	metrics.RecordDBSync(probe, probe, "succeeded", 0)
	metrics.RecordDBClean(probe, probe, "succeeded", 0)
	metrics.RecordSecretStoreRemint(probe, probe, "proactive")

	families, err := ctrlmetrics.Registry.Gather()
	g.Expect(err).NotTo(HaveOccurred(),
		"controller-runtime registry must gather without error")

	registered := make(map[string]struct{})
	for _, fam := range families {
		name := fam.GetName()
		if strings.HasPrefix(name, "barbican_operator_") {
			registered[name] = struct{}{}
		}
	}
	// Clean up the probe series so subsequent tests in the same binary
	// see a clean per-CR registry. The re-mint counter is keyed by the store
	// CR, so it needs the second helper.
	metrics.DeleteForBarbican(probe, probe)
	metrics.DeleteForBarbicanSecretStore(probe, probe)
	g.Expect(registered).NotTo(BeEmpty(),
		"expected at least one barbican_operator_* metric to be registered")

	d := loadDashboard(t)

	seen := make(map[string]struct{})
	for _, p := range d.Panels {
		for _, tr := range p.Targets {
			for _, tok := range metricNameRE.FindAllString(tr.Expr, -1) {
				if !strings.HasPrefix(tok, "barbican_operator_") {
					continue
				}
				seen[stripHistogramSuffix(tok)] = struct{}{}
			}
		}
	}
	g.Expect(seen).NotTo(BeEmpty(),
		"dashboard must reference at least one barbican_operator_* metric")

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
