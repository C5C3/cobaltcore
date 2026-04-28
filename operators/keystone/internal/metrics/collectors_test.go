// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// expectedReconcileDurationBuckets are the exact buckets prescribed by
// CC-0089 REQ-001 for the keystone_operator_reconcile_duration_seconds
// histogram.
var expectedReconcileDurationBuckets = []float64{
	0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30,
}

// newCollectorsForTest is a test-only constructor that returns a fresh
// collectors set bound to reg. It delegates to NewTestRecorder so the
// registration-and-panic logic lives in exactly one place. Each unit
// test gets a new Registerer so gather output is deterministic and free
// of cross-test interference (CC-0089, REQ-008).
func newCollectorsForTest(reg prometheus.Registerer) *collectors {
	return NewTestRecorder(reg).c
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

func TestReconcileDurationHistogramRegistered(t *testing.T) {
	g := NewGomegaWithT(t)

	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)
	g.Expect(c).NotTo(BeNil())

	// Bucket enumeration requires at least one observation, so record one
	// with zero duration under a dummy sub_reconciler. This is purely a
	// gather trigger; the bucket list is a property of the descriptor.
	c.observeReconcileDuration("dummy", 0)

	fam := gatherMetric(t, reg, "keystone_operator_reconcile_duration_seconds")
	g.Expect(fam).NotTo(BeNil(), "histogram keystone_operator_reconcile_duration_seconds must be registered")
	g.Expect(fam.GetType()).To(Equal(dto.MetricType_HISTOGRAM))

	metrics := fam.GetMetric()
	g.Expect(metrics).NotTo(BeEmpty())
	buckets := metrics[0].GetHistogram().GetBucket()
	got := make([]float64, 0, len(buckets))
	for _, b := range buckets {
		got = append(got, b.GetUpperBound())
	}
	g.Expect(got).To(Equal(expectedReconcileDurationBuckets),
		"reconcile_duration bucket boundaries MUST match CC-0089 REQ-001 exactly")
}

func TestReconcileErrorsCounterLabels(t *testing.T) {
	g := NewGomegaWithT(t)

	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	c.recordReconcileError("fernet", "FernetKeysReady")

	fam := gatherMetric(t, reg, "keystone_operator_reconcile_errors_total")
	g.Expect(fam).NotTo(BeNil())
	g.Expect(fam.GetType()).To(Equal(dto.MetricType_COUNTER))
	g.Expect(fam.GetMetric()).To(HaveLen(1))

	labels := fam.GetMetric()[0].GetLabel()
	names := make([]string, 0, len(labels))
	values := map[string]string{}
	for _, l := range labels {
		names = append(names, l.GetName())
		values[l.GetName()] = l.GetValue()
	}
	g.Expect(names).To(ConsistOf("sub_reconciler", "condition_type"),
		"reconcile_errors label set MUST be exactly {sub_reconciler, condition_type} (CC-0089 REQ-002)")
	g.Expect(values["sub_reconciler"]).To(Equal("fernet"))
	g.Expect(values["condition_type"]).To(Equal("FernetKeysReady"))
	g.Expect(fam.GetMetric()[0].GetCounter().GetValue()).To(Equal(1.0))
}

func TestErrorCounterNotIncrementedOnSuccess(t *testing.T) {
	g := NewGomegaWithT(t)

	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	// Success path: duration observation only, no error.
	c.observeReconcileDuration("fernet", 42*time.Millisecond)

	fam := gatherMetric(t, reg, "keystone_operator_reconcile_errors_total")
	// A counter with no observations and no labels pre-created is absent
	// from the gather output, which is the desired outcome.
	if fam != nil {
		for _, m := range fam.GetMetric() {
			g.Expect(m.GetCounter().GetValue()).To(Equal(0.0),
				"success path must never increment reconcile_errors (CC-0089 REQ-002)")
		}
	}
}

func TestKeyRotationAgeGaugePresentAndAbsent(t *testing.T) {
	g := NewGomegaWithT(t)

	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	completedAt := time.Now().Add(-7 * 24 * time.Hour)
	g.Expect(c.setKeyRotationAge("foo", "bar", "fernet", completedAt)).To(Succeed())

	fam := gatherMetric(t, reg, "keystone_operator_key_rotation_age_seconds")
	g.Expect(fam).NotTo(BeNil(),
		"key_rotation_age gauge must be visible after SetKeyRotationAge (CC-0089 REQ-003)")
	g.Expect(fam.GetMetric()).To(HaveLen(1))
	g.Expect(fam.GetMetric()[0].GetGauge().GetValue()).To(BeNumerically(">", 0))

	// Delete by full label set and verify the series is gone.
	c.deleteForKeystone("foo", "bar")
	fam = gatherMetric(t, reg, "keystone_operator_key_rotation_age_seconds")
	if fam != nil {
		g.Expect(fam.GetMetric()).To(BeEmpty(),
			"delete must remove rotation-age gauge series (CC-0089 REQ-003)")
	}
}

func TestKeyRotationGaugeOmitsUnparseableTimestamp(t *testing.T) {
	g := NewGomegaWithT(t)

	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	err := c.setKeyRotationAge("foo", "bar", "fernet", time.Time{})
	g.Expect(err).To(HaveOccurred(),
		"SetKeyRotationAge must return error for zero time (CC-0089 REQ-003)")

	fam := gatherMetric(t, reg, "keystone_operator_key_rotation_age_seconds")
	if fam != nil {
		g.Expect(fam.GetMetric()).To(BeEmpty(),
			"zero-time call MUST NOT set the gauge (CC-0089 REQ-003)")
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
		"both key_type series for foo/bar must be removed (CC-0089 REQ-004)")
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
		"db_sync_total series for foo/bar must be removed (CC-0089 REQ-004)")

	// db_sync_duration_seconds: foo/bar series must be gone.
	durFam := gatherMetric(t, reg, "keystone_operator_db_sync_duration_seconds")
	g.Expect(durFam).NotTo(BeNil())
	g.Expect(durFam.GetMetric()).To(HaveLen(1),
		"db_sync_duration_seconds series for foo/bar must be removed (CC-0089 REQ-004)")
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
		"each RecordDBSync call records exactly one sample (CC-0089 REQ-005)")
	g.Expect(hist.GetSampleSum()).To(BeNumerically("~", duration.Seconds(), 1e-9),
		"sample sum must equal the duration in seconds")
}

// TestSubReconcilerMetricsHaveNoCRLabels is the cardinality drift-guard for
// CC-0089 REQ-012. Adding a `keystone` or `namespace` label to either the
// reconcile_duration histogram or the reconcile_errors counter would
// explode cardinality (O(#CRs × #sub-reconcilers)) and is forbidden by the
// design. This test fails CI if either label ever appears on those
// metrics.
func TestSubReconcilerMetricsHaveNoCRLabels(t *testing.T) {
	g := NewGomegaWithT(t)

	reg := prometheus.NewRegistry()
	c := newCollectorsForTest(reg)

	// Emit one observation per metric so the gather output contains the
	// descriptor's label set (prometheus descriptors are enumerated via
	// actual samples, not up-front).
	c.observeReconcileDuration("fernet", time.Millisecond)
	c.recordReconcileError("fernet", "FernetKeysReady")

	forbidden := map[string]struct{}{"keystone": {}, "namespace": {}}
	checkNames := []string{
		"keystone_operator_reconcile_duration_seconds",
		"keystone_operator_reconcile_errors_total",
	}
	for _, name := range checkNames {
		fam := gatherMetric(t, reg, name)
		g.Expect(fam).NotTo(BeNil(), "metric %s must be registered", name)
		for _, m := range fam.GetMetric() {
			for _, l := range m.GetLabel() {
				_, bad := forbidden[l.GetName()]
				g.Expect(bad).To(BeFalse(),
					"metric %s must NOT have label %q — REQ-012 cardinality guard",
					name, l.GetName())
			}
		}
	}
}
