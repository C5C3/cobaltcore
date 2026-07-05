// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var testSubConditionTypes = []string{"AReady", "BReady"}

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
