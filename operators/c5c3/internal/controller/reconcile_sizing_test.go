// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Sizing sub-reconciler, which resolves spec.sizing and reports
// SizingReady.
package controller

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// sizingControlPlane returns a ControlPlane in "openstack" with the given
// sizing and nothing else; reconcileSizing reads no other field.
func sizingControlPlane(name string, sizing *c5c3v1alpha1.ControlPlaneSizingSpec) *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "openstack", Generation: 4},
		Spec:       c5c3v1alpha1.ControlPlaneSpec{Sizing: sizing},
	}
}

func profileRef(name string) *c5c3v1alpha1.ControlPlaneSizingSpec {
	return &c5c3v1alpha1.ControlPlaneSizingSpec{ProfileRef: &c5c3v1alpha1.SizingProfileRef{Name: name}}
}

func testSizingProfile(name string, base c5c3v1alpha1.SizingProfileName) *c5c3v1alpha1.SizingProfile {
	return &c5c3v1alpha1.SizingProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       c5c3v1alpha1.SizingProfileSpec{Base: base},
	}
}

// failSizingProfileGets fails every SizingProfile read with err.
func failSizingProfileGets(err error) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*c5c3v1alpha1.SizingProfile); ok {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
}

func TestReconcileSizing_BuiltinNeedsNoRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sizing  *c5c3v1alpha1.ControlPlaneSizingSpec
		message string
	}{
		{name: "no sizing", message: `sizing resolved from built-in profile "Standard"`},
		{
			name:    "profile Minimal",
			sizing:  &c5c3v1alpha1.ControlPlaneSizingSpec{Profile: c5c3v1alpha1.SizingProfileMinimal},
			message: `sizing resolved from built-in profile "Minimal"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			s := controllerTestScheme(t)
			// Any SizingProfile read fails, so a pass that reads one cannot
			// report True.
			c := fake.NewClientBuilder().WithScheme(s).
				WithInterceptorFuncs(failSizingProfileGets(errors.New("unexpected read"))).Build()
			r := &ControlPlaneReconciler{Client: c, Scheme: s}
			cp := sizingControlPlane("cp", tc.sizing)

			res, err := r.reconcileSizing(context.Background(), cp)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res).To(Equal(ctrl.Result{}))
			cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeSizingReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal("SizingResolved"))
			g.Expect(cond.Message).To(Equal(tc.message))
			g.Expect(cond.ObservedGeneration).To(Equal(int64(4)))
		})
	}
}

func TestReconcileSizing_ExistingProfile(t *testing.T) {
	g := NewGomegaWithT(t)
	s := controllerTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(testSizingProfile("site", c5c3v1alpha1.SizingProfileMinimal)).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}
	cp := sizingControlPlane("cp", profileRef("site"))

	_, err := r.reconcileSizing(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeSizingReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("SizingResolved"))
	g.Expect(cond.Message).To(Equal(`sizing resolved from SizingProfile "site" (base "Minimal")`))
}

func TestReconcileSizing_MissingProfile(t *testing.T) {
	g := NewGomegaWithT(t)
	s := controllerTestScheme(t)
	r := &ControlPlaneReconciler{Client: fake.NewClientBuilder().WithScheme(s).Build(), Scheme: s}
	cp := sizingControlPlane("cp", profileRef("gone"))

	_, err := r.reconcileSizing(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	g.Expect(err.Error()).To(Equal(`reading SizingProfile "gone": sizingprofiles.c5c3.io "gone" not found`))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeSizingReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("SizingProfileNotFound"))
	g.Expect(cond.Message).To(Equal(`SizingProfile "gone" not found; the children keep their last projected sizing`))
}

func TestReconcileSizing_GetError(t *testing.T) {
	g := NewGomegaWithT(t)
	s := controllerTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithInterceptorFuncs(failSizingProfileGets(apierrors.NewServiceUnavailable("etcd is down"))).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}
	cp := sizingControlPlane("cp", profileRef("site"))

	_, err := r.reconcileSizing(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsNotFound(err)).To(BeFalse())
	g.Expect(err.Error()).To(ContainSubstring(`reading SizingProfile "site"`))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeSizingReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("SizingProfileError"))
	g.Expect(cond.Message).To(ContainSubstring("etcd is down"))
}

// TestReconcile_MissingProfileStopsPipeline runs a full pass against a
// ControlPlane whose profile is gone: the Sizing step fails first, so no later
// step runs and Ready stays False.
func TestReconcile_MissingProfileStopsPipeline(t *testing.T) {
	g := NewGomegaWithT(t)
	s := controllerTestScheme(t)
	cp := sizingControlPlane("cp", profileRef("gone"))
	// The finalizer is already in place, so the pass goes straight to the
	// pipeline instead of requeuing after installing it.
	cp.Finalizers = []string{controlPlaneORCFinalizer}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cp)})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())

	var got c5c3v1alpha1.ControlPlane
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(cp), &got)).To(Succeed())
	sizing := conditions.GetCondition(got.Status.Conditions, conditionTypeSizingReady)
	g.Expect(sizing).NotTo(BeNil())
	g.Expect(sizing.Reason).To(Equal("SizingProfileNotFound"))
	g.Expect(conditions.GetCondition(got.Status.Conditions, conditionTypeNamespacesReady)).To(BeNil(),
		"no step after Sizing may run while the profile is missing")
	ready := conditions.GetCondition(got.Status.Conditions, conditionTypeReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
}

func TestAggregateReady_SizingBlocksIt(t *testing.T) {
	g := NewGomegaWithT(t)
	conds := make([]metav1.Condition, 0, len(subConditionTypes))
	for _, ct := range subConditionTypes {
		c := trueCondition(ct)
		if ct == conditionTypeSizingReady {
			c.Status = metav1.ConditionFalse
		}
		conds = append(conds, c)
	}
	g.Expect(subConditionTypes).To(ContainElement(conditionTypeSizingReady))
	g.Expect(conditions.AllTrue(conds, subConditionTypes...)).To(BeFalse())
}

func TestSizingProfileToControlPlaneMapper(t *testing.T) {
	s := controllerTestScheme(t)
	referencing := sizingControlPlane("a", profileRef("site"))
	other := sizingControlPlane("b", profileRef("other"))
	other.Namespace = "tenant-b"
	builtin := sizingControlPlane("c", &c5c3v1alpha1.ControlPlaneSizingSpec{Profile: c5c3v1alpha1.SizingProfileMinimal})
	builtin.Namespace = "tenant-c"
	unsized := sizingControlPlane("d", nil)
	unsized.Namespace = "tenant-d"
	objs := []client.Object{referencing, other, builtin, unsized}

	t.Run("returns exactly the referencing ControlPlanes", func(t *testing.T) {
		g := NewGomegaWithT(t)
		r := &ControlPlaneReconciler{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()}
		got := r.sizingProfileToControlPlaneMapper(context.Background(), testSizingProfile("site", ""))
		g.Expect(got).To(ConsistOf(reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "openstack", Name: "a"}}))
	})

	t.Run("returns none for an unreferenced profile", func(t *testing.T) {
		g := NewGomegaWithT(t)
		r := &ControlPlaneReconciler{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()}
		g.Expect(r.sizingProfileToControlPlaneMapper(context.Background(), testSizingProfile("unused", ""))).To(BeEmpty())
	})

	t.Run("returns nil when the List fails", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New("cache unavailable")
			},
		}).Build()
		r := &ControlPlaneReconciler{Client: c}
		g.Expect(r.sizingProfileToControlPlaneMapper(context.Background(), testSizingProfile("site", ""))).To(BeNil())
	})
}
