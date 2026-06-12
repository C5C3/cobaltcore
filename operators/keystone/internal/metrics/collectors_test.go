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
)

// newCollectorsForTest returns a fresh per-CR collectors set bound to reg.
// Each unit test gets a new Registerer so gather output is deterministic and
// free of cross-test interference. The sub-reconciler
// duration/error pair is tested in internal/common/instrumentation.
func newCollectorsForTest(reg prometheus.Registerer) *collectors {
	c := newCollectors()
	if err := c.register(reg); err != nil {
		// Test registries are always empty in new Prometheus registries;
		// a registration error here is a programmer bug in the test setup
		// and must be surfaced loudly.
		panic(fmt.Sprintf("metrics: test registry rejected collectors: %v", err))
	}
	return c
}

// gatherMetric returns the first MetricFamily whose Name matches. If not
// present, it returns nil so callers can distinguish "absent" from "found".
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

func TestKeyRotationAgeGaugePresentAndAbsent(t *testing.T) {
	g := NewGomegaWithT(t)

	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	completedAt := time.Now().Add(-7 * 24 * time.Hour)
	g.Expect(c.setKeyRotationAge("foo", "bar", "fernet", completedAt)).To(Succeed())

	fam := gatherMetric(t, reg, "keystone_operator_key_rotation_age_seconds")
	g.Expect(fam).NotTo(BeNil(),
		"key_rotation_age gauge must be visible after SetKeyRotationAge")
	g.Expect(fam.GetMetric()).To(HaveLen(1))
	g.Expect(fam.GetMetric()[0].GetGauge().GetValue()).To(BeNumerically(">", 0))

	// Delete by full label set and verify the series is gone.
	c.deleteForKeystone("foo", "bar")
	fam = gatherMetric(t, reg, "keystone_operator_key_rotation_age_seconds")
	if fam != nil {
		g.Expect(fam.GetMetric()).To(BeEmpty(),
			"delete must remove rotation-age gauge series")
	}
}

func TestKeyRotationGaugeOmitsUnparseableTimestamp(t *testing.T) {
	g := NewGomegaWithT(t)

	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	err := c.setKeyRotationAge("foo", "bar", "fernet", time.Time{})
	g.Expect(err).To(HaveOccurred(),
		"SetKeyRotationAge must return error for zero time")

	fam := gatherMetric(t, reg, "keystone_operator_key_rotation_age_seconds")
	if fam != nil {
		g.Expect(fam.GetMetric()).To(BeEmpty(),
			"zero-time call MUST NOT set the gauge")
	}
}

func TestDeleteKeystoneSeriesRemovesGauge(t *testing.T) {
	g := NewGomegaWithT(t)

	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	now := time.Now()
	g.Expect(c.setKeyRotationAge("foo", "bar", "fernet", now.Add(-time.Hour))).To(Succeed())
	g.Expect(c.setKeyRotationAge("foo", "bar", "credential", now.Add(-2*time.Hour))).To(Succeed())
	// Another (keystone, namespace) pair that must survive the delete.
	g.Expect(c.setKeyRotationAge("other", "other", "fernet", now.Add(-30*time.Minute))).To(Succeed())

	c.recordDBSync("foo", "bar", "succeeded", 5*time.Second)
	c.recordDBSync("other", "other", "succeeded", 6*time.Second)

	c.deleteForKeystone("foo", "bar")

	// key_rotation_age: only the "other/other/fernet" series should remain.
	rotFam := gatherMetric(t, reg, "keystone_operator_key_rotation_age_seconds")
	g.Expect(rotFam).NotTo(BeNil())
	g.Expect(rotFam.GetMetric()).To(HaveLen(1),
		"both key_type series for foo/bar must be removed")
	for _, l := range rotFam.GetMetric()[0].GetLabel() {
		if l.GetName() == "keystone" {
			g.Expect(l.GetValue()).To(Equal("other"))
		}
		if l.GetName() == "namespace" {
			g.Expect(l.GetValue()).To(Equal("other"))
		}
	}

	// db_sync_total: foo/bar series must be gone.
	syncFam := gatherMetric(t, reg, "keystone_operator_db_sync_total")
	g.Expect(syncFam).NotTo(BeNil())
	g.Expect(syncFam.GetMetric()).To(HaveLen(1),
		"db_sync_total series for foo/bar must be removed")

	// db_sync_duration_seconds: foo/bar series must be gone.
	durFam := gatherMetric(t, reg, "keystone_operator_db_sync_duration_seconds")
	g.Expect(durFam).NotTo(BeNil())
	g.Expect(durFam.GetMetric()).To(HaveLen(1),
		"db_sync_duration_seconds series for foo/bar must be removed")
}

// TestAdminPasswordRotationAgeGaugeAndDelete proves the
// keystone_operator_key_rotation_age_seconds gauge accepts the
// key_type="admin-password" series emitted by reconcilePasswordRotation via
// observeRotationAge, and that deleteForKeystone reaps it. The admin-password
// key type is the addition to the existing fernet/credential rotations
// because DeletePartialMatch is scoped to (keystone, namespace), an
// unrelated CR's series must survive.
func TestAdminPasswordRotationAgeGaugeAndDelete(t *testing.T) {
	g := NewGomegaWithT(t)

	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	pastTime := time.Now().Add(-7 * 24 * time.Hour)
	g.Expect(c.setKeyRotationAge("foo", "bar", "admin-password", pastTime)).To(Succeed())
	// Another (keystone, namespace) pair that must survive the delete.
	g.Expect(c.setKeyRotationAge("other", "other", "admin-password", pastTime)).To(Succeed())

	fam := gatherMetric(t, reg, "keystone_operator_key_rotation_age_seconds")
	g.Expect(fam).NotTo(BeNil(),
		"key_rotation_age gauge must be visible for key_type=admin-password")
	g.Expect(fam.GetMetric()).To(HaveLen(2))

	// Locate the foo/bar/admin-password series and assert its label set + value.
	var fooSeries *dto.Metric
	for _, m := range fam.GetMetric() {
		labels := map[string]string{}
		for _, l := range m.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		if labels["keystone"] == "foo" && labels["namespace"] == "bar" {
			fooSeries = m
			g.Expect(labels).To(HaveKeyWithValue("key_type", "admin-password"),
				"admin-password rotation must be labelled key_type=admin-password")
		}
	}
	g.Expect(fooSeries).NotTo(BeNil(),
		"foo/bar/admin-password series must be present")
	g.Expect(fooSeries.GetGauge().GetValue()).To(BeNumerically(">", 0))

	// DeletePartialMatch is keystone+namespace scoped, so only foo/bar is reaped.
	c.deleteForKeystone("foo", "bar")

	fam = gatherMetric(t, reg, "keystone_operator_key_rotation_age_seconds")
	g.Expect(fam).NotTo(BeNil())
	g.Expect(fam.GetMetric()).To(HaveLen(1),
		"deleteForKeystone must remove the foo/bar admin-password series")
	for _, l := range fam.GetMetric()[0].GetLabel() {
		if l.GetName() == "keystone" {
			g.Expect(l.GetValue()).To(Equal("other"),
				"unrelated CR's admin-password series must survive the delete")
		}
	}
}

func TestDbSyncCounterIncrementsOnTerminalStateOnly(t *testing.T) {
	g := NewGomegaWithT(t)

	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	c.recordDBSync("foo", "bar", "succeeded", 12*time.Second)

	fam := gatherMetric(t, reg, "keystone_operator_db_sync_total")
	g.Expect(fam).NotTo(BeNil())
	g.Expect(fam.GetMetric()).To(HaveLen(1))
	labels := fam.GetMetric()[0].GetLabel()
	values := map[string]string{}
	for _, l := range labels {
		values[l.GetName()] = l.GetValue()
	}
	g.Expect(values).To(HaveKeyWithValue("keystone", "foo"))
	g.Expect(values).To(HaveKeyWithValue("namespace", "bar"))
	g.Expect(values).To(HaveKeyWithValue("result", "succeeded"))
	g.Expect(fam.GetMetric()[0].GetCounter().GetValue()).To(Equal(1.0))

	// Histogram observation count must be exactly 1.
	durFam := gatherMetric(t, reg, "keystone_operator_db_sync_duration_seconds")
	g.Expect(durFam).NotTo(BeNil())
	g.Expect(durFam.GetMetric()).To(HaveLen(1))
	g.Expect(durFam.GetMetric()[0].GetHistogram().GetSampleCount()).To(Equal(uint64(1)))
}

func TestDbSyncDurationHistogramObservedOnce(t *testing.T) {
	g := NewGomegaWithT(t)

	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	duration := 12345 * time.Millisecond
	c.recordDBSync("foo", "bar", "succeeded", duration)

	fam := gatherMetric(t, reg, "keystone_operator_db_sync_duration_seconds")
	g.Expect(fam).NotTo(BeNil())
	g.Expect(fam.GetMetric()).To(HaveLen(1))
	hist := fam.GetMetric()[0].GetHistogram()
	g.Expect(hist.GetSampleCount()).To(Equal(uint64(1)),
		"each RecordDBSync call records exactly one sample")
	g.Expect(hist.GetSampleSum()).To(BeNumerically("~", duration.Seconds(), 1e-9),
		"sample sum must equal the duration in seconds")
}
