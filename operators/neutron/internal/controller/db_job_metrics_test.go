// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	dto "github.com/prometheus/client_model/go"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	neutronmetrics "github.com/c5c3/cobaltcore/operators/neutron/internal/metrics"
)

// dbJobTestScheme carries the kinds the DB-job metrics bridge touches: the
// Neutron CR it patches the dedupe annotation onto, and the built-in kinds
// clientgoscheme registers (batch/v1 among them, for the observed Job).
func dbJobTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = neutronv1alpha1.AddToScheme(s)
	return s
}

// dbJobMetricsTestNeutron returns a Neutron CR carrying only the identity the
// bridge reads: the name and namespace that label the metric series, and the UID
// the fake client needs to own the annotation patch.
func dbJobMetricsTestNeutron(name, ns string) *neutronv1alpha1.Neutron {
	return &neutronv1alpha1.Neutron{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  ns,
			UID:        types.UID(name + "-uid"),
			Generation: 1,
		},
		Spec: neutronv1alpha1.NeutronSpec{OpenStackRelease: "2026.1"},
	}
}

// terminatedDBJob returns a Job in the given terminal state, named the way the
// database step names the phase's Job, and with a five-second run recorded in
// the CreationTimestamp/LastTransitionTime pair the duration histogram reads.
func terminatedDBJob(neutron *neutronv1alpha1.Neutron, jobSuffix, uid string, terminal batchv1.JobConditionType) *batchv1.Job {
	created := metav1.NewTime(time.Date(2026, 4, 22, 13, 0, 0, 0, time.UTC))
	terminated := metav1.NewTime(created.Add(5 * time.Second))
	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              neutron.Name + "-" + jobSuffix,
			Namespace:         neutron.Namespace,
			UID:               types.UID(uid),
			CreationTimestamp: created,
		},
		Status: batchv1.JobStatus{
			CompletionTime: &terminated,
			Conditions: []batchv1.JobCondition{{
				Type:               terminal,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: terminated,
			}},
		},
	}
	if terminal == batchv1.JobFailed {
		j.Status.Failed = 1
	} else {
		j.Status.Succeeded = 1
	}
	return j
}

// findMetricByLabels returns the series in famName on the controller-runtime
// registry whose label set equals want, or nil when it has never been observed.
func findMetricByLabels(t *testing.T, famName string, want map[string]string) *dto.Metric {
	t.Helper()
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != famName {
			continue
		}
		for _, m := range fam.GetMetric() {
			if len(m.GetLabel()) != len(want) {
				continue
			}
			match := true
			for _, lp := range m.GetLabel() {
				if want[lp.GetName()] != lp.GetValue() {
					match = false
					break
				}
			}
			if match {
				return m
			}
		}
	}
	return nil
}

// counterValue returns the counter value of the (famName, labels) series, or 0
// when the series is absent.
func counterValue(t *testing.T, famName string, labels map[string]string) float64 {
	t.Helper()
	m := findMetricByLabels(t, famName, labels)
	if m == nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// histogramSampleCount returns the sample count of the (famName, labels)
// histogram series, or 0 when the series is absent.
func histogramSampleCount(t *testing.T, famName string, labels map[string]string) uint64 {
	t.Helper()
	m := findMetricByLabels(t, famName, labels)
	if m == nil {
		return 0
	}
	return m.GetHistogram().GetSampleCount()
}

// TestRecordDBJobTerminalState_EmitsOncePerUID pins the dedupe the bridge exists
// for, across every phase suffix the database step passes it: a terminated Job
// stays terminated, so every reconcile in between observes the same terminal
// state and the counter must not track the number of passes. The failed phases
// also pin the result label, which the shared helper derives from the terminal
// condition type.
func TestRecordDBJobTerminalState_EmitsOncePerUID(t *testing.T) {
	cases := []struct {
		jobSuffix string
		terminal  batchv1.JobConditionType
		result    string
	}{
		{jobSuffix: "db-sync", terminal: batchv1.JobComplete, result: "succeeded"},
		{jobSuffix: "db-expand", terminal: batchv1.JobFailed, result: "failed"},
		{jobSuffix: "db-migrate", terminal: batchv1.JobComplete, result: "succeeded"},
		{jobSuffix: "db-contract", terminal: batchv1.JobFailed, result: "failed"},
	}

	for _, tc := range cases {
		t.Run(tc.jobSuffix, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(neutronmetrics.Register()).To(Succeed())

			s := dbJobTestScheme()
			neutron := dbJobMetricsTestNeutron(tc.jobSuffix+"-once", "ns-"+tc.jobSuffix+"-once")
			t.Cleanup(func() { neutronmetrics.DeleteForNeutron(neutron.Name, neutron.Namespace) })
			dbJob := terminatedDBJob(neutron, tc.jobSuffix, tc.jobSuffix+"-once-job-uid", tc.terminal)

			r := &NeutronReconciler{
				Client: fake.NewClientBuilder().
					WithScheme(s).
					WithObjects(neutron, dbJob).
					WithStatusSubresource(&neutronv1alpha1.Neutron{}).
					Build(),
				Scheme:   s,
				Recorder: record.NewFakeRecorder(10),
			}

			counterLabels := map[string]string{
				"neutron":   neutron.Name,
				"namespace": neutron.Namespace,
				"result":    tc.result,
			}
			durationLabels := map[string]string{
				"neutron":   neutron.Name,
				"namespace": neutron.Namespace,
			}

			r.recordDBJobTerminalState(context.Background(), neutron, tc.jobSuffix, dbJob)

			g.Expect(counterValue(t, "neutron_operator_db_sync_total", counterLabels)).To(Equal(1.0),
				"a terminated %s Job must be counted as result=%s", tc.jobSuffix, tc.result)
			g.Expect(histogramSampleCount(t, "neutron_operator_db_sync_duration_seconds", durationLabels)).
				To(Equal(uint64(1)), "the terminal transition must contribute one duration sample")
			g.Expect(neutron.Annotations).To(HaveKey(dbJobUIDAnnotationKey(tc.jobSuffix)),
				"the dedupe annotation must be mirrored back onto the in-memory CR")

			// Same Job, same UID: a second pass must not count it again.
			r.recordDBJobTerminalState(context.Background(), neutron, tc.jobSuffix, dbJob)

			g.Expect(counterValue(t, "neutron_operator_db_sync_total", counterLabels)).To(Equal(1.0),
				"the terminal state MUST be recorded at most once per (phase, Job UID)")
			g.Expect(histogramSampleCount(t, "neutron_operator_db_sync_duration_seconds", durationLabels)).
				To(Equal(uint64(1)))
		})
	}
}

// TestRecordDBJobTerminalState_NilObservedIsANoOp covers the just-created Job:
// the database step threads a nil Job through before the first terminal
// condition exists, and neither a metric nor a dedupe annotation may result.
func TestRecordDBJobTerminalState_NilObservedIsANoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(neutronmetrics.Register()).To(Succeed())

	s := dbJobTestScheme()
	neutron := dbJobMetricsTestNeutron("nil-observed", "ns-nil-observed")
	t.Cleanup(func() { neutronmetrics.DeleteForNeutron(neutron.Name, neutron.Namespace) })

	r := &NeutronReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(neutron).
			WithStatusSubresource(&neutronv1alpha1.Neutron{}).
			Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	labels := map[string]string{
		"neutron":   neutron.Name,
		"namespace": neutron.Namespace,
		"result":    "succeeded",
	}

	r.recordDBJobTerminalState(context.Background(), neutron, "db-sync", nil)

	g.Expect(counterValue(t, "neutron_operator_db_sync_total", labels)).To(Equal(0.0),
		"a nil observed Job must not emit a metric")
	g.Expect(neutron.Annotations).NotTo(HaveKey(dbJobUIDAnnotationKey("db-sync")))
}

// TestRecordDBJobTerminalState_DefersOnPatchFailure pins the ordering invariant:
// when the dedupe annotation patch fails, the metric is NOT emitted on this
// pass. The next reconcile re-evaluates the same Job and either emits then
// (after a successful patch) or defers again, so the
// at-most-once-per-(phase, Job UID) guarantee survives a transient apiserver
// failure. The deferral raises a Warning event, otherwise the degradation would
// be invisible at default log levels.
func TestRecordDBJobTerminalState_DefersOnPatchFailure(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(neutronmetrics.Register()).To(Succeed())

	s := dbJobTestScheme()
	neutron := dbJobMetricsTestNeutron("patch-failure", "ns-patch-failure")
	t.Cleanup(func() { neutronmetrics.DeleteForNeutron(neutron.Name, neutron.Namespace) })
	dbJob := terminatedDBJob(neutron, "db-sync", "patch-failure-job-uid", batchv1.JobComplete)

	recorder := record.NewFakeRecorder(10)
	r := &NeutronReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(neutron, dbJob).
			WithStatusSubresource(&neutronv1alpha1.Neutron{}).
			WithInterceptorFuncs(interceptor.Funcs{
				Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
					if _, isNeutron := obj.(*neutronv1alpha1.Neutron); isNeutron {
						return fmt.Errorf("simulated apiserver Patch failure")
					}
					return nil
				},
			}).
			Build(),
		Scheme:   s,
		Recorder: recorder,
	}

	counterLabels := map[string]string{
		"neutron":   neutron.Name,
		"namespace": neutron.Namespace,
		"result":    "succeeded",
	}
	durationLabels := map[string]string{
		"neutron":   neutron.Name,
		"namespace": neutron.Namespace,
	}

	r.recordDBJobTerminalState(context.Background(), neutron, "db-sync", dbJob)

	g.Expect(counterValue(t, "neutron_operator_db_sync_total", counterLabels)).To(Equal(0.0),
		"the metric MUST NOT be emitted when the dedupe annotation patch fails")
	g.Expect(histogramSampleCount(t, "neutron_operator_db_sync_duration_seconds", durationLabels)).
		To(Equal(uint64(0)),
			"the duration histogram MUST NOT receive a sample when the patch fails")
	g.Expect(neutron.Annotations).NotTo(HaveKey(dbJobUIDAnnotationKey("db-sync")),
		"a failed Patch MUST NOT mirror the dedupe annotation back onto the in-memory CR")
	g.Expect(recorder.Events).To(Receive(ContainSubstring("Warning DBSyncMetricEmissionDeferred")),
		"a deferred emission MUST raise a Warning event on the Neutron CR")
}
