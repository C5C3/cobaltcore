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

// seriesForNeutron returns the metrics within fam whose neutron+namespace labels
// match, isolating a CR's series from any recorded on the process-wide registry.
func seriesForNeutron(fam *dto.MetricFamily, neutron, namespace string) []*dto.Metric {
	if fam == nil {
		return nil
	}
	var out []*dto.Metric
	for _, m := range fam.GetMetric() {
		labels := map[string]string{}
		for _, l := range m.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		if labels["neutron"] == neutron && labels["namespace"] == namespace {
			out = append(out, m)
		}
	}
	return out
}

func TestDbSyncCounterIncrementsOnTerminalStateOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	c.recordDBSync("foo", "bar", "succeeded", 12*time.Second)

	fam := gatherMetric(t, reg, "neutron_operator_db_sync_total")
	g.Expect(fam).NotTo(BeNil())
	g.Expect(fam.GetMetric()).To(HaveLen(1))
	values := map[string]string{}
	for _, l := range fam.GetMetric()[0].GetLabel() {
		values[l.GetName()] = l.GetValue()
	}
	g.Expect(values).To(HaveKeyWithValue("neutron", "foo"))
	g.Expect(values).To(HaveKeyWithValue("namespace", "bar"))
	g.Expect(values).To(HaveKeyWithValue("result", "succeeded"))
	g.Expect(fam.GetMetric()[0].GetCounter().GetValue()).To(Equal(1.0))

	durFam := gatherMetric(t, reg, "neutron_operator_db_sync_duration_seconds")
	g.Expect(durFam).NotTo(BeNil())
	g.Expect(durFam.GetMetric()).To(HaveLen(1))
	g.Expect(durFam.GetMetric()[0].GetHistogram().GetSampleCount()).To(Equal(uint64(1)))
}

func TestDbSyncDurationHistogramObservedOnce(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	c.recordDBSync("foo", "bar", "succeeded", 12345*time.Millisecond)

	fam := gatherMetric(t, reg, "neutron_operator_db_sync_duration_seconds")
	g.Expect(fam).NotTo(BeNil())
	g.Expect(fam.GetMetric()).To(HaveLen(1))
	g.Expect(fam.GetMetric()[0].GetHistogram().GetSampleCount()).To(Equal(uint64(1)))
	g.Expect(fam.GetMetric()[0].GetHistogram().GetSampleSum()).To(BeNumerically("~", 12.345, 0.001))
}

// TestGlobalCollectorPathRecordsAndDeletes exercises the package-level wrappers
// (RecordDBSync, DeleteForNeutron) against the real controller-runtime registry,
// the in-process path production uses. Register's error branch is intentionally
// NOT exercised here (it would poison the process-global sync.Once); the
// duplicate-registration error is covered against a fresh registry by
// TestRegisterDuplicateReturnsError.
func TestGlobalCollectorPathRecordsAndDeletes(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := ctrlmetrics.Registry

	g.Expect(Register()).To(Succeed())

	const (
		targetName   = "global-target"
		targetNs     = "global-ns"
		survivorName = "global-survivor"
		survivorNs   = "global-ns"
	)

	RecordDBSync(targetName, targetNs, "succeeded", 3*time.Second)
	RecordDBSync(survivorName, survivorNs, "succeeded", 4*time.Second)

	total := gatherMetric(t, reg, "neutron_operator_db_sync_total")
	g.Expect(seriesForNeutron(total, targetName, targetNs)).To(HaveLen(1),
		"RecordDBSync must publish a db_sync_total series on ctrlmetrics.Registry")
	dur := gatherMetric(t, reg, "neutron_operator_db_sync_duration_seconds")
	g.Expect(seriesForNeutron(dur, targetName, targetNs)).To(HaveLen(1))

	DeleteForNeutron(targetName, targetNs)

	total = gatherMetric(t, reg, "neutron_operator_db_sync_total")
	g.Expect(seriesForNeutron(total, targetName, targetNs)).To(BeEmpty(),
		"DeleteForNeutron must remove the target series (stale-series leak guard)")
	g.Expect(seriesForNeutron(total, survivorName, survivorNs)).To(HaveLen(1),
		"an unrelated CR's series must survive the delete")

	dur = gatherMetric(t, reg, "neutron_operator_db_sync_duration_seconds")
	g.Expect(seriesForNeutron(dur, targetName, targetNs)).To(BeEmpty())
	g.Expect(seriesForNeutron(dur, survivorName, survivorNs)).To(HaveLen(1))
}

func TestOVNDbSyncCounterIncrementsOnTerminalStateOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	c.recordOVNDBSync("foo", "bar", "failed", 42*time.Second)

	fam := gatherMetric(t, reg, "neutron_operator_ovn_db_sync_total")
	g.Expect(fam).NotTo(BeNil())
	g.Expect(fam.GetMetric()).To(HaveLen(1))
	values := map[string]string{}
	for _, l := range fam.GetMetric()[0].GetLabel() {
		values[l.GetName()] = l.GetValue()
	}
	g.Expect(values).To(HaveKeyWithValue("neutron", "foo"))
	g.Expect(values).To(HaveKeyWithValue("namespace", "bar"))
	g.Expect(values).To(HaveKeyWithValue("result", "failed"))
	g.Expect(fam.GetMetric()[0].GetCounter().GetValue()).To(Equal(1.0))

	durFam := gatherMetric(t, reg, "neutron_operator_ovn_db_sync_duration_seconds")
	g.Expect(durFam).NotTo(BeNil())
	g.Expect(durFam.GetMetric()).To(HaveLen(1))
	g.Expect(durFam.GetMetric()[0].GetHistogram().GetSampleCount()).To(Equal(uint64(1)))
	g.Expect(durFam.GetMetric()[0].GetHistogram().GetSampleSum()).To(BeNumerically("~", 42.0, 0.001))

	// The ovn-db-sync pair must not bleed into the db-sync pair: both carry the
	// same neutron/namespace labels, so a copy-paste in recordOVNDBSync would be
	// invisible without this assertion.
	g.Expect(gatherMetric(t, reg, "neutron_operator_db_sync_total").GetMetric()).To(BeEmpty())
}

// TestGlobalCollectorPathRecordsAndDeletesOVNDBSync is the ovn-db-sync half of
// the global-path coverage: RecordOVNDBSync must publish on the
// controller-runtime registry and DeleteForNeutron must drop the ovn-db-sync
// series too, otherwise a deleted CR leaks an ovn-db-sync time series for the
// operator's lifetime.
func TestGlobalCollectorPathRecordsAndDeletesOVNDBSync(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := ctrlmetrics.Registry

	g.Expect(Register()).To(Succeed())

	const (
		targetName   = "ovn-db-sync-target"
		targetNs     = "ovn-db-sync-ns"
		survivorName = "ovn-db-sync-survivor"
		survivorNs   = "ovn-db-sync-ns"
	)

	RecordOVNDBSync(targetName, targetNs, "succeeded", 5*time.Second)
	RecordOVNDBSync(survivorName, survivorNs, "failed", 6*time.Second)

	total := gatherMetric(t, reg, "neutron_operator_ovn_db_sync_total")
	g.Expect(seriesForNeutron(total, targetName, targetNs)).To(HaveLen(1),
		"RecordOVNDBSync must publish an ovn_db_sync_total series on ctrlmetrics.Registry")
	dur := gatherMetric(t, reg, "neutron_operator_ovn_db_sync_duration_seconds")
	g.Expect(seriesForNeutron(dur, targetName, targetNs)).To(HaveLen(1))

	DeleteForNeutron(targetName, targetNs)

	total = gatherMetric(t, reg, "neutron_operator_ovn_db_sync_total")
	g.Expect(seriesForNeutron(total, targetName, targetNs)).To(BeEmpty(),
		"DeleteForNeutron must remove the target's ovn-db-sync series (stale-series leak guard)")
	g.Expect(seriesForNeutron(total, survivorName, survivorNs)).To(HaveLen(1),
		"an unrelated CR's ovn-db-sync series must survive the delete")

	dur = gatherMetric(t, reg, "neutron_operator_ovn_db_sync_duration_seconds")
	g.Expect(seriesForNeutron(dur, targetName, targetNs)).To(BeEmpty())
	g.Expect(seriesForNeutron(dur, survivorName, survivorNs)).To(HaveLen(1))
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
