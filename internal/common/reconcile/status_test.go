// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var testSubConditionTypes = []string{"AReady", "BReady"}

func TestCurrentAggregateRejectsMissingStaleFailedAndErroredObservations(t *testing.T) {
	g := gomega.NewWithT(t)
	gate := metav1.Condition{Type: "AReady", Status: metav1.ConditionTrue, ObservedGeneration: 1}
	for _, tc := range []struct {
		gate       *metav1.Condition
		generation int64
		passErr    error
		message    string
	}{
		{nil, 1, nil, "has not been reported"},
		{&gate, 2, nil, "last observed generation 1"},
		{&metav1.Condition{Type: "AReady", Status: metav1.ConditionFalse, ObservedGeneration: 1, Reason: "Waiting"}, 1, nil, "Waiting"},
		{&gate, 1, context.DeadlineExceeded, "pass failed"},
	} {
		var conds []metav1.Condition
		if tc.gate != nil {
			conds = append(conds, *tc.gate)
		}
		setAggregateReady(&conds, tc.generation, []string{"AReady"}, true, tc.passErr)
		ready := meta.FindStatusCondition(conds, "Ready")
		g.Expect(ready.Status).To(gomega.Equal(metav1.ConditionFalse))
		g.Expect(ready.ObservedGeneration).To(gomega.Equal(tc.generation))
		g.Expect(ready.Message).To(gomega.ContainSubstring(tc.message))
	}
}

func TestSetAggregateReady_AllTrue(t *testing.T) {
	g := gomega.NewWithT(t)

	conds := []metav1.Condition{
		{Type: "AReady", Status: metav1.ConditionTrue, Reason: "Ready"},
		{Type: "BReady", Status: metav1.ConditionTrue, Reason: "Ready"},
	}

	SetAggregateReady(&conds, 7, testSubConditionTypes)

	ready := meta.FindStatusCondition(conds, "Ready")
	g.Expect(ready).NotTo(gomega.BeNil())
	g.Expect(ready.Status).To(gomega.Equal(metav1.ConditionTrue))
	g.Expect(ready.Reason).To(gomega.Equal("AllReady"))
	g.Expect(ready.Message).To(gomega.Equal("All sub-conditions are ready"))
	g.Expect(ready.ObservedGeneration).To(gomega.Equal(int64(7)))
}

func TestSetAggregateReady_OneFalse(t *testing.T) {
	g := gomega.NewWithT(t)

	conds := []metav1.Condition{
		{Type: "AReady", Status: metav1.ConditionTrue, Reason: "Ready"},
		{Type: "BReady", Status: metav1.ConditionFalse, Reason: "Waiting"},
	}

	SetAggregateReady(&conds, 1, testSubConditionTypes)

	ready := meta.FindStatusCondition(conds, "Ready")
	g.Expect(ready).NotTo(gomega.BeNil())
	g.Expect(ready.Status).To(gomega.Equal(metav1.ConditionFalse))
	g.Expect(ready.Reason).To(gomega.Equal("NotAllReady"))
	g.Expect(ready.Message).To(gomega.Equal("One or more sub-conditions are not ready"))
}

// A missing sub-condition (not merely False) must aggregate to NotAllReady —
// absence means the sub-reconciler has not converged yet.
func TestSetAggregateReady_MissingCondition(t *testing.T) {
	g := gomega.NewWithT(t)

	conds := []metav1.Condition{
		{Type: "AReady", Status: metav1.ConditionTrue, Reason: "Ready"},
	}

	SetAggregateReady(&conds, 1, testSubConditionTypes)

	ready := meta.FindStatusCondition(conds, "Ready")
	g.Expect(ready).NotTo(gomega.BeNil())
	g.Expect(ready.Status).To(gomega.Equal(metav1.ConditionFalse))
}

// testStatusObject builds a fake client around a Pod whose status writes can
// be intercepted; UpdateStatus is exercised against Pod.Status as the generic
// status snapshot type.
func testStatusClient(t *testing.T, statusErr error) (client.Client, *corev1.Pod) {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}}
	builder := fake.NewClientBuilder().
		WithScheme(clientgoscheme.Scheme).
		WithObjects(pod).
		WithStatusSubresource(pod)
	if statusErr != nil {
		builder = builder.WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
				return statusErr
			},
		})
	}
	return builder.Build(), pod
}

func TestUpdateStatus_SkipsWriteWhenUnchanged(t *testing.T) {
	g := gomega.NewWithT(t)

	statusErr := fmt.Errorf("status update must not be called on an unchanged status")
	c, pod := testStatusClient(t, statusErr)

	mutated := false
	before := pod.Status.DeepCopy()
	result, err := UpdateStatus(context.Background(), c, pod, before, &pod.Status,
		func() { mutated = true }, ctrl.Result{RequeueAfter: 1}, nil)

	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"an unchanged status must skip the write; the failing interceptor proves it was not called")
	g.Expect(mutated).To(gomega.BeTrue(), "mutate must run before the skip decision")
	g.Expect(result).To(gomega.Equal(ctrl.Result{RequeueAfter: 1}))
}

func TestUpdateStatus_WritesWhenChanged(t *testing.T) {
	g := gomega.NewWithT(t)

	c, pod := testStatusClient(t, nil)

	before := pod.Status.DeepCopy()
	result, err := UpdateStatus(context.Background(), c, pod, before, &pod.Status,
		func() { pod.Status.Message = "changed" }, ctrl.Result{RequeueAfter: 2}, nil)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(result).To(gomega.Equal(ctrl.Result{RequeueAfter: 2}))

	fetched := &corev1.Pod{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(pod), fetched)).To(gomega.Succeed())
	g.Expect(fetched.Status.Message).To(gomega.Equal("changed"), "the changed status must be persisted")
}

// A nil before snapshot (defensive) always writes.
func TestUpdateStatus_NilSnapshotAlwaysWrites(t *testing.T) {
	g := gomega.NewWithT(t)

	statusErr := fmt.Errorf("write attempted")
	c, pod := testStatusClient(t, statusErr)

	_, err := UpdateStatus(context.Background(), c, pod, nil, &pod.Status,
		func() {}, ctrl.Result{}, nil)

	g.Expect(err).To(gomega.HaveOccurred(), "a nil snapshot must attempt the write")
	g.Expect(err.Error()).To(gomega.ContainSubstring("updating status:"))
}

// When both the reconcile and the status write fail, both errors survive via
// errors.Join and the result is zeroed so controller-runtime applies
// error-based backoff.
func TestUpdateStatus_JoinsBothErrors(t *testing.T) {
	g := gomega.NewWithT(t)

	reconcileErr := fmt.Errorf("sub-reconciler failed")
	statusErr := fmt.Errorf("status write failed")
	c, pod := testStatusClient(t, statusErr)

	result, err := UpdateStatus(context.Background(), c, pod, nil, &pod.Status,
		func() {}, ctrl.Result{RequeueAfter: 5}, reconcileErr)

	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(errors.Is(err, reconcileErr)).To(gomega.BeTrue())
	g.Expect(errors.Is(err, statusErr)).To(gomega.BeTrue())
	g.Expect(result).To(gomega.Equal(ctrl.Result{}))
}

// A pure reconcile error with a successful write is returned verbatim, not
// wrapped, so callers can errors.Is against sentinel errors.
func TestUpdateStatus_ReconcileErrorOnlyPreserved(t *testing.T) {
	g := gomega.NewWithT(t)

	reconcileErr := fmt.Errorf("sub-reconciler failed")
	c, pod := testStatusClient(t, nil)

	_, err := UpdateStatus(context.Background(), c, pod, nil, &pod.Status,
		func() { pod.Status.Message = "x" }, ctrl.Result{}, reconcileErr)

	g.Expect(err).To(gomega.MatchError(reconcileErr))
}

// statusConflict is the 409 a status write returns when the object changed
// after the pass read it.
func statusConflict() error {
	return apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "p", errors.New("the object has been modified"))
}

// contextWithLogCapture returns a context whose logger appends every line it
// writes, rendered by funcr, to the returned slice.
func contextWithLogCapture() (context.Context, *[]string) {
	var logs []string
	logger := funcr.New(func(prefix, args string) { logs = append(logs, prefix+" "+args) }, funcr.Options{})
	return log.IntoContext(context.Background(), logger), &logs
}

// A status write that loses to a concurrent write of the same object drops
// the computed status and requeues after RequeueNextPass with a nil error, so
// controller-runtime neither logs "Reconciler error" nor counts the pass as
// failed. The lost write leaves one Info line and no error line.
func TestUpdateStatus_ConflictRequeuesWithoutError(t *testing.T) {
	cases := map[string]struct {
		snapshot bool
	}{
		"snapshot":     {snapshot: true},
		"nil snapshot": {snapshot: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			c, pod := testStatusClient(t, statusConflict())
			ctx, logs := contextWithLogCapture()
			var before *corev1.PodStatus
			if tc.snapshot {
				before = pod.Status.DeepCopy()
			}

			result, err := UpdateStatus(ctx, c, pod, before, &pod.Status,
				func() { pod.Status.Message = "changed" }, ctrl.Result{}, nil)

			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(result).To(gomega.Equal(ctrl.Result{RequeueAfter: RequeueNextPass}))
			g.Expect(*logs).To(gomega.ConsistOf(gomega.And(
				gomega.ContainSubstring(`"msg"="status update conflict, requeueing"`),
				gomega.ContainSubstring(`"level"=0`))))
			g.Expect(*logs).NotTo(gomega.ContainElement(gomega.ContainSubstring("unable to update status")))
		})
	}
}

// A pass that asked for an earlier requeue than RequeueNextPass keeps it
// under a conflict; a later one is brought forward to RequeueNextPass.
func TestUpdateStatus_ConflictKeepsShorterRequeue(t *testing.T) {
	cases := map[string]struct {
		pass, want ctrl.Result
	}{
		"shorter than RequeueNextPass": {
			pass: ctrl.Result{RequeueAfter: 500 * time.Millisecond},
			want: ctrl.Result{RequeueAfter: 500 * time.Millisecond},
		},
		"longer than RequeueNextPass": {
			pass: ctrl.Result{RequeueAfter: 5 * time.Minute},
			want: ctrl.Result{RequeueAfter: RequeueNextPass},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			c, pod := testStatusClient(t, statusConflict())

			result, err := UpdateStatus(context.Background(), c, pod, nil, &pod.Status,
				func() {}, tc.pass, nil)

			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(result).To(gomega.Equal(tc.want))
		})
	}
}

// A real reconcile failure keeps its error backoff under a conflicting
// write: UpdateStatus returns the reconcile error alone, not joined with the
// conflict, and drops the pass's requeue. The lost write leaves the same Info
// line as without a reconcile error and no error line.
func TestUpdateStatus_ConflictWithReconcileErrorReturnsOnlyIt(t *testing.T) {
	g := gomega.NewWithT(t)

	reconcileErr := fmt.Errorf("sub-reconciler failed")
	c, pod := testStatusClient(t, statusConflict())
	ctx, logs := contextWithLogCapture()

	result, err := UpdateStatus(ctx, c, pod, nil, &pod.Status,
		func() {}, ctrl.Result{RequeueAfter: 5 * time.Minute}, reconcileErr)

	g.Expect(result).To(gomega.Equal(ctrl.Result{}))
	g.Expect(err).To(gomega.BeIdenticalTo(reconcileErr))
	g.Expect(*logs).To(gomega.ConsistOf(gomega.ContainSubstring(`"msg"="status update conflict, requeueing"`)))
	g.Expect(*logs).NotTo(gomega.ContainElement(gomega.ContainSubstring("unable to update status")))
}

// Without an interceptor, the fake client's optimistic lock rejects a status
// write computed from a stale read with a real 409. The stale status is not
// persisted, and the pass requeues without an error.
func TestUpdateStatus_StaleResourceVersionConflicts(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()
	c, pod := testStatusClient(t, nil)

	stale := &corev1.Pod{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(pod), stale)).To(gomega.Succeed())
	newer := stale.DeepCopy()
	newer.Labels = map[string]string{"changed": "true"}
	g.Expect(c.Update(ctx, newer)).To(gomega.Succeed())

	result, err := UpdateStatus(ctx, c, stale, nil, &stale.Status,
		func() { stale.Status.Message = "stale" }, ctrl.Result{}, nil)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(result).To(gomega.Equal(ctrl.Result{RequeueAfter: RequeueNextPass}))
	fetched := &corev1.Pod{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(pod), fetched)).To(gomega.Succeed())
	g.Expect(fetched.Status.Message).To(gomega.BeEmpty(), "the stale status must not be persisted")
}

// Any failed write other than a 409 keeps the error path: an error-level log
// line, an empty result and the wrapped API error.
func TestUpdateStatus_NonConflictErrorKeepsErrorPath(t *testing.T) {
	g := gomega.NewWithT(t)

	c, pod := testStatusClient(t, apierrors.NewInternalError(errors.New("etcdserver: request timed out")))
	ctx, logs := contextWithLogCapture()

	result, err := UpdateStatus(ctx, c, pod, nil, &pod.Status,
		func() {}, ctrl.Result{RequeueAfter: 5}, nil)

	g.Expect(result).To(gomega.Equal(ctrl.Result{}))
	g.Expect(apierrors.IsInternalError(err)).To(gomega.BeTrue())
	g.Expect(err.Error()).To(gomega.ContainSubstring("updating status:"))
	g.Expect(*logs).To(gomega.ContainElement(gomega.ContainSubstring(`"msg"="unable to update status"`)))
}
