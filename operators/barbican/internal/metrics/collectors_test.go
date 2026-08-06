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

// seriesFor returns the metrics within fam whose label named key plus namespace
// match, isolating a CR's series from any recorded on the process-wide
// registry. key is "barbican" for the job collectors and "store" for the
// re-mint counter.
func seriesFor(fam *dto.MetricFamily, key, name, namespace string) []*dto.Metric {
	if fam == nil {
		return nil
	}
	var out []*dto.Metric
	for _, m := range fam.GetMetric() {
		labels := map[string]string{}
		for _, l := range m.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		if labels[key] == name && labels["namespace"] == namespace {
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

	fam := gatherMetric(t, reg, "barbican_operator_db_sync_total")
	g.Expect(fam).NotTo(BeNil())
	g.Expect(fam.GetMetric()).To(HaveLen(1))
	values := map[string]string{}
	for _, l := range fam.GetMetric()[0].GetLabel() {
		values[l.GetName()] = l.GetValue()
	}
	g.Expect(values).To(HaveKeyWithValue("barbican", "foo"))
	g.Expect(values).To(HaveKeyWithValue("namespace", "bar"))
	g.Expect(values).To(HaveKeyWithValue("result", "succeeded"))
	g.Expect(fam.GetMetric()[0].GetCounter().GetValue()).To(Equal(1.0))

	durFam := gatherMetric(t, reg, "barbican_operator_db_sync_duration_seconds")
	g.Expect(durFam).NotTo(BeNil())
	g.Expect(durFam.GetMetric()).To(HaveLen(1))
	g.Expect(durFam.GetMetric()[0].GetHistogram().GetSampleCount()).To(Equal(uint64(1)))
}

func TestDbSyncDurationHistogramObservedOnce(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	c.recordDBSync("foo", "bar", "succeeded", 12345*time.Millisecond)

	fam := gatherMetric(t, reg, "barbican_operator_db_sync_duration_seconds")
	g.Expect(fam).NotTo(BeNil())
	g.Expect(fam.GetMetric()).To(HaveLen(1))
	g.Expect(fam.GetMetric()[0].GetHistogram().GetSampleCount()).To(Equal(uint64(1)))
	g.Expect(fam.GetMetric()[0].GetHistogram().GetSampleSum()).To(BeNumerically("~", 12.345, 0.001))
}

func TestDbCleanCounterIncrementsOnTerminalStateOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	c.recordDBClean("foo", "bar", "failed", 42*time.Second)

	fam := gatherMetric(t, reg, "barbican_operator_db_clean_total")
	g.Expect(fam).NotTo(BeNil())
	g.Expect(fam.GetMetric()).To(HaveLen(1))
	values := map[string]string{}
	for _, l := range fam.GetMetric()[0].GetLabel() {
		values[l.GetName()] = l.GetValue()
	}
	g.Expect(values).To(HaveKeyWithValue("barbican", "foo"))
	g.Expect(values).To(HaveKeyWithValue("namespace", "bar"))
	g.Expect(values).To(HaveKeyWithValue("result", "failed"))
	g.Expect(fam.GetMetric()[0].GetCounter().GetValue()).To(Equal(1.0))

	durFam := gatherMetric(t, reg, "barbican_operator_db_clean_duration_seconds")
	g.Expect(durFam).NotTo(BeNil())
	g.Expect(durFam.GetMetric()).To(HaveLen(1))
	g.Expect(durFam.GetMetric()[0].GetHistogram().GetSampleCount()).To(Equal(uint64(1)))
	g.Expect(durFam.GetMetric()[0].GetHistogram().GetSampleSum()).To(BeNumerically("~", 42.0, 0.001))

	// The clean pair must not bleed into the db-sync pair: both carry the same
	// barbican/namespace labels, so a copy-paste in recordDBClean would be
	// invisible without this assertion.
	g.Expect(gatherMetric(t, reg, "barbican_operator_db_sync_total").GetMetric()).To(BeEmpty())
}

func TestSecretStoreRemintCounterSeparatesTheTriggers(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	c.recordSecretStoreRemint("openbao-primary", "openstack", "proactive")
	c.recordSecretStoreRemint("openbao-primary", "openstack", "reactive")
	c.recordSecretStoreRemint("openbao-primary", "openstack", "reactive")

	fam := gatherMetric(t, reg, "barbican_operator_secretstore_remints_total")
	g.Expect(fam).NotTo(BeNil())
	g.Expect(fam.GetMetric()).To(HaveLen(2),
		"the trigger label must split the counter, not collapse it")

	byTrigger := map[string]float64{}
	for _, m := range fam.GetMetric() {
		values := map[string]string{}
		for _, l := range m.GetLabel() {
			values[l.GetName()] = l.GetValue()
		}
		g.Expect(values).To(HaveKeyWithValue("store", "openbao-primary"))
		g.Expect(values).To(HaveKeyWithValue("namespace", "openstack"))
		byTrigger[values["trigger"]] = m.GetCounter().GetValue()
	}
	g.Expect(byTrigger).To(Equal(map[string]float64{"proactive": 1, "reactive": 2}))
}

// TestGlobalCollectorPathRecordsAndDeletes exercises the package-level wrappers
// (RecordDBSync, DeleteForBarbican) against the real controller-runtime
// registry, the in-process path production uses. Register's error branch is
// intentionally NOT exercised here (it would poison the process-global
// sync.Once); the duplicate-registration error is covered against a fresh
// registry by TestRegisterDuplicateReturnsError.
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

	total := gatherMetric(t, reg, "barbican_operator_db_sync_total")
	g.Expect(seriesFor(total, "barbican", targetName, targetNs)).To(HaveLen(1),
		"RecordDBSync must publish a db_sync_total series on ctrlmetrics.Registry")
	dur := gatherMetric(t, reg, "barbican_operator_db_sync_duration_seconds")
	g.Expect(seriesFor(dur, "barbican", targetName, targetNs)).To(HaveLen(1))

	DeleteForBarbican(targetName, targetNs)

	total = gatherMetric(t, reg, "barbican_operator_db_sync_total")
	g.Expect(seriesFor(total, "barbican", targetName, targetNs)).To(BeEmpty(),
		"DeleteForBarbican must remove the target series (stale-series leak guard)")
	g.Expect(seriesFor(total, "barbican", survivorName, survivorNs)).To(HaveLen(1),
		"an unrelated CR's series must survive the delete")

	dur = gatherMetric(t, reg, "barbican_operator_db_sync_duration_seconds")
	g.Expect(seriesFor(dur, "barbican", targetName, targetNs)).To(BeEmpty())
	g.Expect(seriesFor(dur, "barbican", survivorName, survivorNs)).To(HaveLen(1))
}

// TestGlobalCollectorPathRecordsAndDeletesDBClean is the db-clean half of the
// global-path coverage: RecordDBClean must publish on the controller-runtime
// registry and DeleteForBarbican must drop the clean series too, otherwise a
// deleted CR leaks a clean time series for the operator's lifetime.
func TestGlobalCollectorPathRecordsAndDeletesDBClean(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := ctrlmetrics.Registry

	g.Expect(Register()).To(Succeed())

	const (
		targetName   = "clean-target"
		targetNs     = "clean-ns"
		survivorName = "clean-survivor"
		survivorNs   = "clean-ns"
	)

	RecordDBClean(targetName, targetNs, "succeeded", 5*time.Second)
	RecordDBClean(survivorName, survivorNs, "failed", 6*time.Second)

	total := gatherMetric(t, reg, "barbican_operator_db_clean_total")
	g.Expect(seriesFor(total, "barbican", targetName, targetNs)).To(HaveLen(1),
		"RecordDBClean must publish a db_clean_total series on ctrlmetrics.Registry")
	dur := gatherMetric(t, reg, "barbican_operator_db_clean_duration_seconds")
	g.Expect(seriesFor(dur, "barbican", targetName, targetNs)).To(HaveLen(1))

	DeleteForBarbican(targetName, targetNs)

	total = gatherMetric(t, reg, "barbican_operator_db_clean_total")
	g.Expect(seriesFor(total, "barbican", targetName, targetNs)).To(BeEmpty(),
		"DeleteForBarbican must remove the target's clean series (stale-series leak guard)")
	g.Expect(seriesFor(total, "barbican", survivorName, survivorNs)).To(HaveLen(1),
		"an unrelated CR's clean series must survive the delete")

	dur = gatherMetric(t, reg, "barbican_operator_db_clean_duration_seconds")
	g.Expect(seriesFor(dur, "barbican", targetName, targetNs)).To(BeEmpty())
	g.Expect(seriesFor(dur, "barbican", survivorName, survivorNs)).To(HaveLen(1))
}

// TestGlobalCollectorPathRecordsAndDeletesRemints covers the secret-store half:
// the re-mint counter is keyed by the store CR, so DeleteForBarbican must leave
// it alone and DeleteForBarbicanSecretStore must clear it.
func TestGlobalCollectorPathRecordsAndDeletesRemints(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := ctrlmetrics.Registry

	g.Expect(Register()).To(Succeed())

	const (
		targetStore   = "remint-target"
		targetNs      = "remint-ns"
		survivorStore = "remint-survivor"
		survivorNs    = "remint-ns"
	)

	RecordSecretStoreRemint(targetStore, targetNs, "proactive")
	RecordSecretStoreRemint(survivorStore, survivorNs, "reactive")

	total := gatherMetric(t, reg, "barbican_operator_secretstore_remints_total")
	g.Expect(seriesFor(total, "store", targetStore, targetNs)).To(HaveLen(1),
		"RecordSecretStoreRemint must publish on ctrlmetrics.Registry")

	DeleteForBarbican(targetStore, targetNs)
	total = gatherMetric(t, reg, "barbican_operator_secretstore_remints_total")
	g.Expect(seriesFor(total, "store", targetStore, targetNs)).To(HaveLen(1),
		"the parent-CR delete must not reach a store-keyed series")

	DeleteForBarbicanSecretStore(targetStore, targetNs)

	total = gatherMetric(t, reg, "barbican_operator_secretstore_remints_total")
	g.Expect(seriesFor(total, "store", targetStore, targetNs)).To(BeEmpty(),
		"DeleteForBarbicanSecretStore must remove the target series (stale-series leak guard)")
	g.Expect(seriesFor(total, "store", survivorStore, survivorNs)).To(HaveLen(1),
		"an unrelated store's series must survive the delete")
}

// TestDeleteForUnrecordedCRIsANoOp covers the finalizer path of a CR that never
// ran a job or minted a credential: the delete helpers must not panic and must
// leave every other series in place.
func TestDeleteForUnrecordedCRIsANoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	c.recordDBSync("recorded", "ns", "succeeded", time.Second)
	c.recordSecretStoreRemint("recorded-store", "ns", "proactive")

	c.deleteForBarbican("never-reconciled", "ns")
	c.deleteForBarbicanSecretStore("never-provisioned", "ns")

	g.Expect(seriesFor(gatherMetric(t, reg, "barbican_operator_db_sync_total"), "barbican", "recorded", "ns")).
		To(HaveLen(1))
	g.Expect(seriesFor(gatherMetric(t, reg, "barbican_operator_secretstore_remints_total"), "store", "recorded-store", "ns")).
		To(HaveLen(1))
}

// TestRegisterIsIdempotent pins the sync.Once contract: a second Register call
// must return the memoized result rather than a duplicate-registration error,
// so an operator that wires it from two setup paths still starts.
func TestRegisterIsIdempotent(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(Register()).To(Succeed())
	g.Expect(Register()).To(Succeed())
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
