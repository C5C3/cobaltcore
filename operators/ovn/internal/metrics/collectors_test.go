// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// newCollectorsForTest returns a fresh per-CR collectors set bound to reg. Each
// unit test gets a new Registerer so gather output is deterministic and free of
// cross-test interference.
func newCollectorsForTest(reg prometheus.Registerer) *collectors {
	c := newCollectors()
	if err := c.register(reg); err != nil {
		panic(fmt.Sprintf("metrics: test registry rejected collectors: %v", err))
	}
	return c
}

// gatherMetric returns the first MetricFamily whose Name matches, or nil.
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

// seriesForOVNCentral returns the metrics within fam whose ovncentral+namespace
// labels match, isolating one CR's series from any recorded on the process-wide
// registry.
func seriesForOVNCentral(fam *dto.MetricFamily, name, namespace string) []*dto.Metric {
	if fam == nil {
		return nil
	}
	var out []*dto.Metric
	for _, m := range fam.GetMetric() {
		labels := map[string]string{}
		for _, l := range m.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		if labels["ovncentral"] == name && labels["namespace"] == namespace {
			out = append(out, m)
		}
	}
	return out
}

// A recorded run must land on the counter series its own result labels and on
// the duration histogram, with the observation carrying the run's own duration
// rather than a bucket boundary.
func TestRecordBackupIncrementsTheLabelledSeries(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	c.recordBackup("ovn", "openstack", "failed", 42500*time.Millisecond)

	fam := gatherMetric(t, reg, "ovn_operator_backup_total")
	g.Expect(fam).NotTo(BeNil())
	g.Expect(fam.GetMetric()).To(HaveLen(1))
	values := map[string]string{}
	for _, l := range fam.GetMetric()[0].GetLabel() {
		values[l.GetName()] = l.GetValue()
	}
	g.Expect(values).To(HaveKeyWithValue("ovncentral", "ovn"))
	g.Expect(values).To(HaveKeyWithValue("namespace", "openstack"))
	g.Expect(values).To(HaveKeyWithValue("result", "failed"))
	g.Expect(fam.GetMetric()[0].GetCounter().GetValue()).To(Equal(1.0))

	durFam := gatherMetric(t, reg, "ovn_operator_backup_duration_seconds")
	g.Expect(durFam).NotTo(BeNil())
	g.Expect(durFam.GetMetric()).To(HaveLen(1))
	g.Expect(durFam.GetMetric()[0].GetHistogram().GetSampleCount()).To(Equal(uint64(1)))
	g.Expect(durFam.GetMetric()[0].GetHistogram().GetSampleSum()).To(BeNumerically("~", 42.5, 0.001))
}

// A second run of the same CR with the other result must open its own counter
// series rather than adding to the first: the result label is what tells a
// backup that stopped working from one that keeps succeeding.
func TestRecordBackupKeepsTheTwoResultsApart(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	c.recordBackup("ovn", "openstack", "succeeded", 3*time.Second)
	c.recordBackup("ovn", "openstack", "failed", 1*time.Second)

	fam := gatherMetric(t, reg, "ovn_operator_backup_total")
	g.Expect(fam.GetMetric()).To(HaveLen(2))
	for _, m := range fam.GetMetric() {
		g.Expect(m.GetCounter().GetValue()).To(Equal(1.0))
	}

	// The duration histogram carries no result label, so both runs share one
	// series.
	durFam := gatherMetric(t, reg, "ovn_operator_backup_duration_seconds")
	g.Expect(durFam.GetMetric()).To(HaveLen(1))
	g.Expect(durFam.GetMetric()[0].GetHistogram().GetSampleCount()).To(Equal(uint64(2)))
}

// TestGlobalCollectorPathRecordsAndDeletes exercises the package-level wrappers
// against the real controller-runtime registry, the in-process path production
// uses. Register's error branch is deliberately NOT exercised here (it would
// poison the process-global sync.Once); the duplicate-registration error is
// covered against a fresh registry by TestRegisterDuplicateReturnsError.
func TestGlobalCollectorPathRecordsAndDeletes(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := ctrlmetrics.Registry

	g.Expect(Register()).To(Succeed())

	const (
		targetName   = "backup-target"
		targetNs     = "backup-ns"
		survivorName = "backup-survivor"
		survivorNs   = "backup-ns"
	)

	RecordBackup(targetName, targetNs, "succeeded", 5*time.Second)
	RecordBackup(survivorName, survivorNs, "failed", 6*time.Second)

	total := gatherMetric(t, reg, "ovn_operator_backup_total")
	g.Expect(seriesForOVNCentral(total, targetName, targetNs)).To(HaveLen(1),
		"RecordBackup must publish a backup_total series on ctrlmetrics.Registry")
	dur := gatherMetric(t, reg, "ovn_operator_backup_duration_seconds")
	g.Expect(seriesForOVNCentral(dur, targetName, targetNs)).To(HaveLen(1))

	DeleteForOVNCentral(targetName, targetNs)

	total = gatherMetric(t, reg, "ovn_operator_backup_total")
	g.Expect(seriesForOVNCentral(total, targetName, targetNs)).To(BeEmpty(),
		"DeleteForOVNCentral must remove the target series (stale-series leak guard)")
	g.Expect(seriesForOVNCentral(total, survivorName, survivorNs)).To(HaveLen(1),
		"an unrelated CR's series must survive the delete")

	dur = gatherMetric(t, reg, "ovn_operator_backup_duration_seconds")
	g.Expect(seriesForOVNCentral(dur, targetName, targetNs)).To(BeEmpty())
	g.Expect(seriesForOVNCentral(dur, survivorName, survivorNs)).To(HaveLen(1))
}

// Register memoizes its first result, so a second call from a second manager
// setup must not report the duplicate registration the raw Registerer would.
func TestRegisterIsIdempotent(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(Register()).To(Succeed())
	g.Expect(Register()).To(Succeed(),
		"the sync.Once must return the memoized first result rather than re-registering")
}

func TestRegisterDuplicateReturnsError(t *testing.T) {
	g := NewGomegaWithT(t)

	reg := prometheus.NewRegistry()
	c := newCollectors()
	g.Expect(c.register(reg)).To(Succeed(),
		"first registration on a fresh registry must succeed")
	g.Expect(c.register(reg)).To(HaveOccurred(),
		"a duplicate registration must surface an error (the global path panics on this)")
}
