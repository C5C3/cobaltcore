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

// seriesForNova returns the metrics within fam whose nova+namespace labels
// match, isolating a CR's series from any recorded on the process-wide registry.
func seriesForNova(fam *dto.MetricFamily, nova, namespace string) []*dto.Metric {
	if fam == nil {
		return nil
	}
	var out []*dto.Metric
	for _, m := range fam.GetMetric() {
		labels := map[string]string{}
		for _, l := range m.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		if labels["nova"] == nova && labels["namespace"] == namespace {
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

	fam := gatherMetric(t, reg, "nova_operator_db_sync_total")
	g.Expect(fam).NotTo(BeNil())
	g.Expect(fam.GetMetric()).To(HaveLen(1))
	values := map[string]string{}
	for _, l := range fam.GetMetric()[0].GetLabel() {
		values[l.GetName()] = l.GetValue()
	}
	g.Expect(values).To(HaveKeyWithValue("nova", "foo"))
	g.Expect(values).To(HaveKeyWithValue("namespace", "bar"))
	g.Expect(values).To(HaveKeyWithValue("result", "succeeded"))
	g.Expect(fam.GetMetric()[0].GetCounter().GetValue()).To(Equal(1.0))

	durFam := gatherMetric(t, reg, "nova_operator_db_sync_duration_seconds")
	g.Expect(durFam).NotTo(BeNil())
	g.Expect(durFam.GetMetric()).To(HaveLen(1))
	g.Expect(durFam.GetMetric()[0].GetHistogram().GetSampleCount()).To(Equal(uint64(1)))
	g.Expect(durFam.GetMetric()[0].GetHistogram().GetSampleSum()).To(BeNumerically("~", 12.0, 0.001))
}

// TestDbArchiveCounterIncrementsOnTerminalStateOnly covers the second pair: the
// recurring archive that moves nova's soft-deleted rows into the shadow tables.
// A failed run is a backlog that keeps growing, so the failed result has to
// reach the counter.
func TestDbArchiveCounterIncrementsOnTerminalStateOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	c.recordDBArchive("foo", "bar", "failed", 42*time.Second)

	fam := gatherMetric(t, reg, "nova_operator_db_archive_total")
	g.Expect(fam).NotTo(BeNil())
	g.Expect(fam.GetMetric()).To(HaveLen(1))
	values := map[string]string{}
	for _, l := range fam.GetMetric()[0].GetLabel() {
		values[l.GetName()] = l.GetValue()
	}
	g.Expect(values).To(HaveKeyWithValue("nova", "foo"))
	g.Expect(values).To(HaveKeyWithValue("namespace", "bar"))
	g.Expect(values).To(HaveKeyWithValue("result", "failed"))
	g.Expect(fam.GetMetric()[0].GetCounter().GetValue()).To(Equal(1.0))

	durFam := gatherMetric(t, reg, "nova_operator_db_archive_duration_seconds")
	g.Expect(durFam).NotTo(BeNil())
	g.Expect(durFam.GetMetric()[0].GetHistogram().GetSampleSum()).To(BeNumerically("~", 42.0, 0.001))

	// The archive pair must not bleed into the db-sync pair: both carry the same
	// nova/namespace labels, so a copy-paste in recordDBArchive would be invisible
	// without this assertion.
	g.Expect(gatherMetric(t, reg, "nova_operator_db_sync_total").GetMetric()).To(BeEmpty())
}

// TestGlobalCollectorPathRecordsAndDeletes exercises the package-level wrappers
// against the real controller-runtime registry — the in-process path production
// uses. Register's error branch is intentionally NOT exercised here (it would
// poison the process-global sync.Once); the duplicate-registration error is
// covered against a fresh registry by TestRegisterDuplicateReturnsError.
//
// A Nova is identified by its name and its namespace together, so the sweep
// must spare a CR of the same name in another namespace: dropping its series
// would blank the dashboards of a Nova that is still running.
func TestGlobalCollectorPathRecordsAndDeletes(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := ctrlmetrics.Registry

	g.Expect(Register()).To(Succeed())

	const (
		targetName   = "global-target"
		targetNs     = "global-ns"
		survivorName = "global-survivor"
		survivorNs   = "global-ns"
		namesakeNs   = "global-other-ns"
	)
	t.Cleanup(func() { DeleteForNova(targetName, namesakeNs) })

	RecordDBSync(targetName, targetNs, "succeeded", 3*time.Second)
	RecordDBArchive(targetName, targetNs, "succeeded", 4*time.Second)
	RecordDBSync(survivorName, survivorNs, "succeeded", 6*time.Second)
	RecordDBArchive(survivorName, survivorNs, "failed", 7*time.Second)
	RecordDBSync(targetName, namesakeNs, "succeeded", 8*time.Second)
	RecordDBArchive(targetName, namesakeNs, "failed", 9*time.Second)

	families := []string{
		"nova_operator_db_sync_total",
		"nova_operator_db_sync_duration_seconds",
		"nova_operator_db_archive_total",
		"nova_operator_db_archive_duration_seconds",
	}
	for _, name := range families {
		g.Expect(seriesForNova(gatherMetric(t, reg, name), targetName, targetNs)).To(HaveLen(1),
			"the record wrappers must publish %s on ctrlmetrics.Registry", name)
	}

	DeleteForNova(targetName, targetNs)

	for _, name := range families {
		fam := gatherMetric(t, reg, name)
		g.Expect(seriesForNova(fam, targetName, targetNs)).To(BeEmpty(),
			"DeleteForNova must remove the target's %s series (stale-series leak guard)", name)
		g.Expect(seriesForNova(fam, survivorName, survivorNs)).To(HaveLen(1),
			"an unrelated CR's %s series must survive the delete", name)
		g.Expect(seriesForNova(fam, targetName, namesakeNs)).To(HaveLen(1),
			"a CR of the same name in another namespace must keep its %s series", name)
	}
}

// TestDeleteForNovaUnknownNameKeepsEverything covers the edge case a deletion
// race produces: a CR whose series were never recorded (it was deleted before
// any job terminated) is swept anyway. The sweep must be a no-op rather than
// dropping the series of the CRs that did record.
func TestDeleteForNovaUnknownNameKeepsEverything(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	c.recordDBSync("recorded", "ns", "succeeded", time.Second)
	c.recordDBArchive("recorded", "ns", "succeeded", time.Second)

	c.deleteForNova("never-recorded", "ns")

	g.Expect(seriesForNova(gatherMetric(t, reg, "nova_operator_db_sync_total"), "recorded", "ns")).
		To(HaveLen(1), "sweeping an unknown CR must not touch a recorded one's series")
	g.Expect(seriesForNova(gatherMetric(t, reg, "nova_operator_db_archive_total"), "recorded", "ns")).
		To(HaveLen(1))
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
