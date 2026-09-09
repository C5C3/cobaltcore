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

// seriesForCinder returns the metrics within fam whose cinder+namespace labels
// match, isolating a CR's series from any recorded on the process-wide registry.
func seriesForCinder(fam *dto.MetricFamily, cinder, namespace string) []*dto.Metric {
	if fam == nil {
		return nil
	}
	var out []*dto.Metric
	for _, m := range fam.GetMetric() {
		labels := map[string]string{}
		for _, l := range m.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		if labels["cinder"] == cinder && labels["namespace"] == namespace {
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

	fam := gatherMetric(t, reg, "cinder_operator_db_sync_total")
	g.Expect(fam).NotTo(BeNil())
	g.Expect(fam.GetMetric()).To(HaveLen(1))
	values := map[string]string{}
	for _, l := range fam.GetMetric()[0].GetLabel() {
		values[l.GetName()] = l.GetValue()
	}
	g.Expect(values).To(HaveKeyWithValue("cinder", "foo"))
	g.Expect(values).To(HaveKeyWithValue("namespace", "bar"))
	g.Expect(values).To(HaveKeyWithValue("result", "succeeded"))
	g.Expect(fam.GetMetric()[0].GetCounter().GetValue()).To(Equal(1.0))

	durFam := gatherMetric(t, reg, "cinder_operator_db_sync_duration_seconds")
	g.Expect(durFam).NotTo(BeNil())
	g.Expect(durFam.GetMetric()).To(HaveLen(1))
	g.Expect(durFam.GetMetric()[0].GetHistogram().GetSampleCount()).To(Equal(uint64(1)))
	g.Expect(durFam.GetMetric()[0].GetHistogram().GetSampleSum()).To(BeNumerically("~", 12.0, 0.001))
}

func TestDbPurgeCounterIncrementsOnTerminalStateOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	c.recordDBPurge("foo", "bar", "failed", 42*time.Second)

	fam := gatherMetric(t, reg, "cinder_operator_db_purge_total")
	g.Expect(fam).NotTo(BeNil())
	g.Expect(fam.GetMetric()).To(HaveLen(1))
	values := map[string]string{}
	for _, l := range fam.GetMetric()[0].GetLabel() {
		values[l.GetName()] = l.GetValue()
	}
	g.Expect(values).To(HaveKeyWithValue("cinder", "foo"))
	g.Expect(values).To(HaveKeyWithValue("namespace", "bar"))
	g.Expect(values).To(HaveKeyWithValue("result", "failed"))
	g.Expect(fam.GetMetric()[0].GetCounter().GetValue()).To(Equal(1.0))

	durFam := gatherMetric(t, reg, "cinder_operator_db_purge_duration_seconds")
	g.Expect(durFam).NotTo(BeNil())
	g.Expect(durFam.GetMetric()[0].GetHistogram().GetSampleSum()).To(BeNumerically("~", 42.0, 0.001))

	// The purge pair must not bleed into the db-sync pair: both carry the same
	// cinder/namespace labels, so a copy-paste in recordDBPurge would be invisible
	// without this assertion.
	g.Expect(gatherMetric(t, reg, "cinder_operator_db_sync_total").GetMetric()).To(BeEmpty())
}

// TestServiceRemoveCounterIncrementsOnTerminalStateOnly covers the third pair:
// the job that detaches a backend. A failed run is what keeps the backend's
// finalizer held, so the failed result must reach the counter.
func TestServiceRemoveCounterIncrementsOnTerminalStateOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	c.recordServiceRemove("foo", "bar", "failed", 7*time.Second)

	fam := gatherMetric(t, reg, "cinder_operator_service_remove_total")
	g.Expect(fam).NotTo(BeNil())
	g.Expect(fam.GetMetric()).To(HaveLen(1))
	values := map[string]string{}
	for _, l := range fam.GetMetric()[0].GetLabel() {
		values[l.GetName()] = l.GetValue()
	}
	g.Expect(values).To(HaveKeyWithValue("cinder", "foo"))
	g.Expect(values).To(HaveKeyWithValue("namespace", "bar"))
	g.Expect(values).To(HaveKeyWithValue("result", "failed"))

	durFam := gatherMetric(t, reg, "cinder_operator_service_remove_duration_seconds")
	g.Expect(durFam).NotTo(BeNil())
	g.Expect(durFam.GetMetric()[0].GetHistogram().GetSampleSum()).To(BeNumerically("~", 7.0, 0.001))

	// The three pairs share their label set, so a copy-paste in
	// recordServiceRemove would land on the wrong vector unnoticed.
	g.Expect(gatherMetric(t, reg, "cinder_operator_db_sync_total").GetMetric()).To(BeEmpty())
	g.Expect(gatherMetric(t, reg, "cinder_operator_db_purge_total").GetMetric()).To(BeEmpty())
}

// TestGlobalCollectorPathRecordsAndDeletes exercises the package-level wrappers
// against the real controller-runtime registry — the in-process path production
// uses. Register's error branch is intentionally NOT exercised here (it would
// poison the process-global sync.Once); the duplicate-registration error is
// covered against a fresh registry by TestRegisterDuplicateReturnsError.
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
	RecordDBPurge(targetName, targetNs, "succeeded", 4*time.Second)
	RecordServiceRemove(targetName, targetNs, "succeeded", 5*time.Second)
	RecordDBSync(survivorName, survivorNs, "succeeded", 6*time.Second)
	RecordDBPurge(survivorName, survivorNs, "failed", 7*time.Second)
	RecordServiceRemove(survivorName, survivorNs, "failed", 8*time.Second)

	families := []string{
		"cinder_operator_db_sync_total",
		"cinder_operator_db_sync_duration_seconds",
		"cinder_operator_db_purge_total",
		"cinder_operator_db_purge_duration_seconds",
		"cinder_operator_service_remove_total",
		"cinder_operator_service_remove_duration_seconds",
	}
	for _, name := range families {
		g.Expect(seriesForCinder(gatherMetric(t, reg, name), targetName, targetNs)).To(HaveLen(1),
			"the record wrappers must publish %s on ctrlmetrics.Registry", name)
	}

	DeleteForCinder(targetName, targetNs)

	for _, name := range families {
		fam := gatherMetric(t, reg, name)
		g.Expect(seriesForCinder(fam, targetName, targetNs)).To(BeEmpty(),
			"DeleteForCinder must remove the target's %s series (stale-series leak guard)", name)
		g.Expect(seriesForCinder(fam, survivorName, survivorNs)).To(HaveLen(1),
			"an unrelated CR's %s series must survive the delete", name)
	}
}

// TestDeleteForCinderUnknownNameKeepsEverything covers the edge case a deletion
// race produces: a CR whose series were never recorded (it was deleted before
// any job terminated) is swept anyway. The sweep must be a no-op rather than
// dropping the series of the CRs that did record.
func TestDeleteForCinderUnknownNameKeepsEverything(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	c.recordDBSync("recorded", "ns", "succeeded", time.Second)
	c.recordServiceRemove("recorded", "ns", "succeeded", time.Second)

	c.deleteForCinder("never-recorded", "ns")

	g.Expect(seriesForCinder(gatherMetric(t, reg, "cinder_operator_db_sync_total"), "recorded", "ns")).
		To(HaveLen(1), "sweeping an unknown CR must not touch a recorded one's series")
	g.Expect(seriesForCinder(gatherMetric(t, reg, "cinder_operator_service_remove_total"), "recorded", "ns")).
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
