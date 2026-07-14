// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/forge/internal/common/conditions"
)

// projectParams builds a ChildProjectionParams over the skelCR test type.
func projectParams(child *skelCR, conds *[]metav1.Condition) ChildProjectionParams[*skelCR] {
	return ChildProjectionParams[*skelCR]{
		Child:           child,
		ConditionType:   "ChildReady",
		ReadyReason:     "ChildIsReady",
		ReadyMessage:    "child is ready",
		WaitingReason:   "WaitingForChild",
		WaitingMessage:  "child not ready",
		RejectedReason:  "ChildRejected",
		RejectedMessage: func(err error) string { return "rejected: " + err.Error() },
		ErrorReason:     "ChildError",
		ErrorMessage:    func(err error) string { return "error: " + err.Error() },
		WaitRequeue:     10 * time.Second,
		Conditions:      conds,
		Generation:      5,
		ChildConditions: func(c *skelCR) []metav1.Condition { return c.Status.Conditions },
	}
}

func TestProjectChild_MirrorsChildReady(t *testing.T) {
	g := gomega.NewWithT(t)
	s := skelScheme(t)
	owner := newSkelCR()
	owner.Name = "owner"
	// Pre-create the child with a Ready condition so the applied server object
	// carries it back for the readiness mirror.
	child := newSkelCR()
	child.Name = "child"
	conditions.SetCondition(&child.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue})
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner, child).
		WithStatusSubresource(child).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).Build()

	desired := newSkelCR()
	desired.Name = "child"
	var conds []metav1.Condition
	res, err := ProjectChild(context.Background(), c, s, owner, projectParams(desired, &conds))
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(res.IsZero()).To(gomega.BeTrue())
	cond := conditions.GetCondition(conds, "ChildReady")
	g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(gomega.Equal("ChildIsReady"))
	g.Expect(cond.ObservedGeneration).To(gomega.Equal(int64(5)))
	// SSA set the owner as controller.
	fetched := &skelCR{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(desired), fetched)).To(gomega.Succeed())
	g.Expect(metav1.IsControlledBy(fetched, owner)).To(gomega.BeTrue())
}

func TestProjectChild_WaitsWhenChildNotReady(t *testing.T) {
	g := gomega.NewWithT(t)
	s := skelScheme(t)
	owner := newSkelCR()
	owner.Name = "owner"
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).Build()

	desired := newSkelCR()
	desired.Name = "child"
	var conds []metav1.Condition
	res, err := ProjectChild(context.Background(), c, s, owner, projectParams(desired, &conds))
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(res.RequeueAfter).To(gomega.Equal(10 * time.Second))
	cond := conditions.GetCondition(conds, "ChildReady")
	g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(gomega.Equal("WaitingForChild"))
}

func TestProjectChild_InvalidRejectionSurfacesDistinctReason(t *testing.T) {
	g := gomega.NewWithT(t)
	s := skelScheme(t)
	owner := newSkelCR()
	owner.Name = "owner"
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(context.Context, client.WithWatch, k8sruntime.ApplyConfiguration, ...client.ApplyOption) error {
				return apierrors.NewInvalid(schema.GroupKind{Kind: "SkelCR"}, "child", nil)
			},
		}).Build()

	desired := newSkelCR()
	desired.Name = "child"
	var conds []metav1.Condition
	_, err := ProjectChild(context.Background(), c, s, owner, projectParams(desired, &conds))
	g.Expect(err).To(gomega.HaveOccurred())
	cond := conditions.GetCondition(conds, "ChildReady")
	g.Expect(cond.Reason).To(gomega.Equal("ChildRejected"))
	g.Expect(cond.Message).To(gomega.ContainSubstring("rejected:"))
}

func TestProjectChild_GenericErrorReason(t *testing.T) {
	g := gomega.NewWithT(t)
	s := skelScheme(t)
	owner := newSkelCR()
	owner.Name = "owner"
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(context.Context, client.WithWatch, k8sruntime.ApplyConfiguration, ...client.ApplyOption) error {
				return fmt.Errorf("boom")
			},
		}).Build()

	desired := newSkelCR()
	desired.Name = "child"
	var conds []metav1.Condition
	_, err := ProjectChild(context.Background(), c, s, owner, projectParams(desired, &conds))
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(conditions.GetCondition(conds, "ChildReady").Reason).To(gomega.Equal("ChildError"))
}

func TestDeleteOrphanedChild(t *testing.T) {
	g := gomega.NewWithT(t)
	s := skelScheme(t)
	owner := newSkelCR()
	owner.Name = "owner"
	owner.UID = "owner-uid"

	// Owned child is deleted.
	child := newSkelCR()
	child.Name = "child"
	g.Expect(controllerutil.SetControllerReference(owner, child, s)).To(gomega.Succeed())
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner, child).Build()
	g.Expect(DeleteOrphanedChild(context.Background(), c, owner, &skelCR{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: child.Namespace}})).To(gomega.Succeed())
	err := c.Get(context.Background(), client.ObjectKey{Namespace: child.Namespace, Name: "child"}, &skelCR{})
	g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())

	// Absent child is a no-op.
	g.Expect(DeleteOrphanedChild(context.Background(), c, owner, &skelCR{ObjectMeta: metav1.ObjectMeta{Name: "absent", Namespace: child.Namespace}})).To(gomega.Succeed())

	// Externally-owned child (not controlled by owner) is left alone.
	foreign := newSkelCR()
	foreign.Name = "foreign"
	c2 := fake.NewClientBuilder().WithScheme(s).WithObjects(foreign).Build()
	g.Expect(DeleteOrphanedChild(context.Background(), c2, owner, &skelCR{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: foreign.Namespace}})).To(gomega.Succeed())
	g.Expect(c2.Get(context.Background(), client.ObjectKey{Namespace: foreign.Namespace, Name: "foreign"}, &skelCR{})).To(gomega.Succeed(), "foreign child must survive")
}

// TestProjectChild_UnownedSkipsOwnerReference pins the cross-namespace projection
// path: the child is applied with no owner reference, so the apply is not refused
// for a namespace the GC cascade cannot reach. Readiness mirroring is unchanged.
func TestProjectChild_UnownedSkipsOwnerReference(t *testing.T) {
	g := gomega.NewWithT(t)
	s := skelScheme(t)
	owner := newSkelCR()
	owner.Name = "owner"
	owner.UID = "owner-uid"

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()

	desired := newSkelCR()
	desired.Name = "child"
	desired.Namespace = "elsewhere"
	var conds []metav1.Condition
	params := projectParams(desired, &conds)
	params.Unowned = true

	res, err := ProjectChild(context.Background(), c, s, owner, params)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(res.RequeueAfter).To(gomega.Equal(10*time.Second), "a fresh child is not ready yet")

	fetched := &skelCR{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Namespace: "elsewhere", Name: "child"}, fetched)).To(gomega.Succeed())
	g.Expect(fetched.OwnerReferences).To(gomega.BeEmpty(), "a cross-namespace child must carry no owner reference")
}

// TestDeleteOrphanedChildFunc_HonorsThePredicate covers the label-based ownership
// test a cross-namespace child needs: an owner reference is absent there, so the
// caller supplies the predicate. An object the predicate rejects is left alone.
func TestDeleteOrphanedChildFunc_HonorsThePredicate(t *testing.T) {
	g := gomega.NewWithT(t)
	s := skelScheme(t)

	labelled := newSkelCR()
	labelled.Name = "labelled"
	labelled.Namespace = "elsewhere"
	labelled.Labels = map[string]string{"owner": "cp"}

	foreign := newSkelCR()
	foreign.Name = "foreign"
	foreign.Namespace = "elsewhere"

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(labelled, foreign).Build()
	ours := func(o client.Object) bool { return o.GetLabels()["owner"] == "cp" }

	g.Expect(DeleteOrphanedChildFunc(context.Background(), c,
		&skelCR{ObjectMeta: metav1.ObjectMeta{Name: "labelled", Namespace: "elsewhere"}}, ours)).To(gomega.Succeed())
	err := c.Get(context.Background(), client.ObjectKey{Namespace: "elsewhere", Name: "labelled"}, &skelCR{})
	g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())

	g.Expect(DeleteOrphanedChildFunc(context.Background(), c,
		&skelCR{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "elsewhere"}}, ours)).To(gomega.Succeed())
	g.Expect(c.Get(context.Background(), client.ObjectKey{Namespace: "elsewhere", Name: "foreign"}, &skelCR{})).
		To(gomega.Succeed(), "an object the predicate rejects must survive")

	// Absent object is a no-op.
	g.Expect(DeleteOrphanedChildFunc(context.Background(), c,
		&skelCR{ObjectMeta: metav1.ObjectMeta{Name: "absent", Namespace: "elsewhere"}}, ours)).To(gomega.Succeed())
}
