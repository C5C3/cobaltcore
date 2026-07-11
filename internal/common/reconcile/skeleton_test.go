// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"context"
	"testing"

	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/forge/internal/common/conditions"
)

// skelStatus and skelCR are a minimal registrable CR carrying a metav1.Condition
// status, so the generic Skeleton can be exercised against a fake client.
type skelStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:generate=false
type skelCR struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Status            skelStatus `json:"status,omitempty"`
}

func (c *skelCR) DeepCopyObject() runtime.Object { return c.DeepCopy() }

func (c *skelCR) DeepCopy() *skelCR {
	out := &skelCR{TypeMeta: c.TypeMeta, ObjectMeta: *c.ObjectMeta.DeepCopy()}
	out.Status.Conditions = append([]metav1.Condition(nil), c.Status.Conditions...)
	return out
}

var skelGVK = schema.GroupVersion{Group: "test.c5c3.io", Version: "v1"}

func skelScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(skelGVK.WithKind("SkelCR"), &skelCR{})
	metav1.AddToGroupVersion(s, skelGVK)
	return s
}

func snapSkelStatus(cr *skelCR) *skelStatus {
	return &skelStatus{Conditions: append([]metav1.Condition(nil), cr.Status.Conditions...)}
}

func newSkelCR() *skelCR {
	return &skelCR{
		TypeMeta:   metav1.TypeMeta{Kind: "SkelCR", APIVersion: "test.c5c3.io/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "cr", Namespace: "ns", Generation: 7},
	}
}

func skelSkeleton() Skeleton[*skelCR, skelStatus] {
	return Skeleton[*skelCR, skelStatus]{
		SubConditionTypes: []string{"AReady", "BReady"},
		Conditions:        func(c *skelCR) *[]metav1.Condition { return &c.Status.Conditions },
	}
}

func TestSkeleton_SetReadyAggregates(t *testing.T) {
	g := gomega.NewWithT(t)
	cr := newSkelCR()
	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{Type: "AReady", Status: metav1.ConditionTrue})
	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{Type: "BReady", Status: metav1.ConditionTrue})

	skelSkeleton().SetReady(cr)

	ready := conditions.GetCondition(cr.Status.Conditions, "Ready")
	g.Expect(ready).NotTo(gomega.BeNil())
	g.Expect(ready.Status).To(gomega.Equal(metav1.ConditionTrue))
	g.Expect(ready.ObservedGeneration).To(gomega.Equal(int64(7)))
}

func TestSkeleton_MarkFailedSetsCondition(t *testing.T) {
	g := gomega.NewWithT(t)
	cr := newSkelCR()

	skelSkeleton().MarkFailed(cr, "AReady", "ConfigError", context.DeadlineExceeded)

	cond := conditions.GetCondition(cr.Status.Conditions, "AReady")
	g.Expect(cond).NotTo(gomega.BeNil())
	g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(gomega.Equal("ConfigError"))
	g.Expect(cond.Message).To(gomega.Equal(context.DeadlineExceeded.Error()))
	g.Expect(cond.ObservedGeneration).To(gomega.Equal(int64(7)))
}

func TestSkeleton_UpdateStatusAggregatesAndRunsExtraMutate(t *testing.T) {
	g := gomega.NewWithT(t)
	s := skelScheme(t)
	cr := newSkelCR()
	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{Type: "AReady", Status: metav1.ConditionTrue})
	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{Type: "BReady", Status: metav1.ConditionTrue})
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cr).WithStatusSubresource(cr).Build()

	before := snapSkelStatus(cr)
	extraRan := false
	res, err := skelSkeleton().UpdateStatus(context.Background(), c, cr, before, &cr.Status, func() {
		extraRan = true
	}, ctrl.Result{}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(res.IsZero()).To(gomega.BeTrue())
	g.Expect(extraRan).To(gomega.BeTrue(), "extraMutate must run")
	g.Expect(conditions.GetCondition(cr.Status.Conditions, "Ready").Status).To(gomega.Equal(metav1.ConditionTrue))

	// A converged pass (before == current) writes nothing new but still returns
	// the result.
	after := snapSkelStatus(cr)
	res2, err2 := skelSkeleton().UpdateStatus(context.Background(), c, cr, after, &cr.Status, nil, ctrl.Result{}, nil)
	g.Expect(err2).NotTo(gomega.HaveOccurred())
	g.Expect(res2.IsZero()).To(gomega.BeTrue())
}

func skelConditions(c *skelCR) *[]metav1.Condition { return &c.Status.Conditions }

func TestSkeleton_RunParallelGroupMergesConditions(t *testing.T) {
	g := gomega.NewWithT(t)
	cr := newSkelCR()
	steps := []ParallelStep[*skelCR]{
		{Name: "a", ConditionType: "AReady", Fn: func(_ context.Context, c *skelCR) (ctrl.Result, error) {
			conditions.SetCondition(skelConditions(c), metav1.Condition{Type: "AReady", Status: metav1.ConditionTrue})
			return ctrl.Result{}, nil
		}},
		{Name: "b", ConditionType: "BReady", Fn: func(_ context.Context, c *skelCR) (ctrl.Result, error) {
			conditions.SetCondition(skelConditions(c), metav1.Condition{Type: "BReady", Status: metav1.ConditionTrue})
			return ctrl.Result{}, nil
		}},
	}
	// A pass-through instrument that just runs the sub-reconciler.
	instrument := func(ctx context.Context, _ string, fn func(context.Context) (ctrl.Result, error)) (ctrl.Result, error) {
		return fn(ctx)
	}

	_, err := skelSkeleton().RunParallelGroup(context.Background(), cr, instrument, steps)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(conditions.GetCondition(cr.Status.Conditions, "AReady")).NotTo(gomega.BeNil())
	g.Expect(conditions.GetCondition(cr.Status.Conditions, "BReady")).NotTo(gomega.BeNil())
}

var _ client.Object = (*skelCR)(nil)
