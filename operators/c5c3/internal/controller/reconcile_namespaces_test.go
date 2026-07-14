// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the namespace sub-reconciler reconcileNamespaces and the ownership
// labels that stand in for the controller owner reference a cross-namespace child
// cannot carry. The tests cover both lifecycles (Managed creates and labels;
// External only verifies), the never-adopt guard that keeps a Managed lifecycle
// from taking over — and eventually deleting — somebody else's namespace, the
// Terminating waits, and the no-assignments short circuit.
package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/forge/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

func namespacesTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := c5c3v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding c5c3 scheme: %v", err)
	}
	return s
}

// namespacedControlPlane builds a ControlPlane that places Keystone in an
// operator-owned namespace and the dashboard in a pre-existing one.
func namespacedControlPlane() *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cp",
			Namespace:  "openstack",
			Generation: 1,
			UID:        types.UID("cp-uid"),
		},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
					Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
						Name:      "identity",
						Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
					},
				},
				Horizon: &c5c3v1alpha1.ServiceHorizonSpec{
					Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
						Name:      "dashboard",
						Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
					},
				},
			},
		},
	}
}

func namespacesCondition(cp *c5c3v1alpha1.ControlPlane) *metav1.Condition {
	return conditions.GetCondition(cp.Status.Conditions, conditionTypeNamespacesReady)
}

// existingNamespace returns a Namespace object with the given labels.
func existingNamespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// TestReconcileNamespaces_NoAssignments verifies the default path costs nothing:
// a ControlPlane whose services stay in its own namespace reports True at once.
func TestReconcileNamespaces_NoAssignments(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespacesTestScheme(t)
	cp := namespacedControlPlane()
	cp.Spec.Services.Keystone.Namespace = nil
	cp.Spec.Services.Horizon.Namespace = nil

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := namespacesCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NoDedicatedNamespaces"))
}

// TestReconcileNamespaces_ManagedCreatesAndLabels verifies the Managed lifecycle
// creates the namespace and stamps it with the ownership labels — the labels are
// what license the teardown to delete it again, and what let the watch resolve an
// event on it back to the ControlPlane.
func TestReconcileNamespaces_ManagedCreatesAndLabels(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespacesTestScheme(t)
	cp := namespacedControlPlane()
	cp.Spec.Services.Horizon = nil // Keystone's Managed namespace only.

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	// First pass creates and requeues.
	res, err := r.reconcileNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(namespaceRequeueAfter))

	ns := &corev1.Namespace{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "identity"}, ns)).To(Succeed())
	g.Expect(ns.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(ns.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "openstack"))
	g.Expect(ns.Labels).To(HaveKeyWithValue(managedByLabel, managedByValue))

	// Second pass observes the namespace it owns and goes Ready.
	res, err = r.reconcileNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := namespacesCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NamespacesReady"))
}

// TestReconcileNamespaces_ManagedRefusesToAdoptForeignNamespace is the guard that
// matters most: a Managed lifecycle DELETES its namespace at teardown, so silently
// adopting a pre-existing one would destroy every workload in it. The reconciler
// fails loud and never touches it.
func TestReconcileNamespaces_ManagedRefusesToAdoptForeignNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespacesTestScheme(t)
	cp := namespacedControlPlane()
	cp.Spec.Services.Horizon = nil

	foreign := existingNamespace("identity", map[string]string{"team": "platform"})
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, foreign).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(namespaceRequeueAfter))

	cond := namespacesCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NamespaceNotOwned"))
	g.Expect(cond.Message).To(ContainSubstring("lifecycle External"))

	// The foreign namespace is left exactly as it was.
	live := &corev1.Namespace{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "identity"}, live)).To(Succeed())
	g.Expect(live.Labels).To(Equal(map[string]string{"team": "platform"}))
}

// TestReconcileNamespaces_ExternalRequiresThePreexistingNamespace verifies the
// External lifecycle never creates: a missing namespace parks the condition and
// requeues rather than conjuring the namespace the lifecycle said is not ours.
func TestReconcileNamespaces_ExternalRequiresThePreexistingNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespacesTestScheme(t)
	cp := namespacedControlPlane()
	cp.Spec.Services.Keystone.Namespace = nil // dashboard's External namespace only.

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(namespaceRequeueAfter))

	cond := namespacesCondition(cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NamespaceNotFound"))

	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "dashboard"}, &corev1.Namespace{})).
		NotTo(Succeed(), "an External namespace must never be created by the operator")
}

// TestReconcileNamespaces_ExternalIsNeverLabelled verifies a pre-existing External
// namespace passes the gate untouched: no ownership labels, so the teardown can
// never mistake it for one it may delete.
func TestReconcileNamespaces_ExternalIsNeverLabelled(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespacesTestScheme(t)
	cp := namespacedControlPlane()
	cp.Spec.Services.Keystone.Namespace = nil

	preexisting := existingNamespace("dashboard", map[string]string{"team": "platform"})
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, preexisting).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(namespacesCondition(cp).Status).To(Equal(metav1.ConditionTrue))

	live := &corev1.Namespace{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "dashboard"}, live)).To(Succeed())
	g.Expect(live.Labels).To(Equal(map[string]string{"team": "platform"}),
		"an External namespace must never be labelled by the operator")
}

// TestReconcileNamespaces_TerminatingWaits verifies a namespace on its way out —
// ours or somebody else's — parks the condition instead of projecting children
// into a namespace the API server is about to reject writes for.
func TestReconcileNamespaces_TerminatingWaits(t *testing.T) {
	now := metav1.Now()

	t.Run("managed", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := namespacesTestScheme(t)
		cp := namespacedControlPlane()
		cp.Spec.Services.Horizon = nil

		terminating := existingNamespace("identity", controlPlaneChildLabels(cp))
		terminating.DeletionTimestamp = &now
		terminating.Finalizers = []string{"kubernetes"}

		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, terminating).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s}

		res, err := r.reconcileNamespaces(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(namespaceRequeueAfter))
		g.Expect(namespacesCondition(cp).Reason).To(Equal("NamespaceTerminating"))
	})

	t.Run("external", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := namespacesTestScheme(t)
		cp := namespacedControlPlane()
		cp.Spec.Services.Keystone.Namespace = nil

		terminating := existingNamespace("dashboard", nil)
		terminating.DeletionTimestamp = &now
		terminating.Finalizers = []string{"kubernetes"}

		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, terminating).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s}

		res, err := r.reconcileNamespaces(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(namespaceRequeueAfter))
		g.Expect(namespacesCondition(cp).Reason).To(Equal("NamespaceTerminating"))
	})
}

// TestIsControlPlaneChild covers both ownership tests: the owner reference (the
// same-namespace case) and the labels (the cross-namespace case, where no owner
// reference is possible), plus the collision an object carrying neither must not
// be adopted through.
func TestIsControlPlaneChild(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespacesTestScheme(t)
	cp := namespacedControlPlane()

	owned := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "owned", Namespace: "openstack"}}
	g.Expect(controllerutil.SetControllerReference(cp, owned, s)).To(Succeed())
	g.Expect(isControlPlaneChild(owned, cp)).To(BeTrue(), "the owner reference must be honoured")

	labelled := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "labelled", Namespace: "identity"}}
	stampControlPlaneChildLabels(labelled, cp)
	g.Expect(isControlPlaneChild(labelled, cp)).To(BeTrue(), "the ownership labels must be honoured")

	// A same-named object of ANOTHER ControlPlane: the name matches, the namespace
	// label does not, so it must not be adopted.
	other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      "labelled",
		Namespace: "identity",
		Labels: map[string]string{
			controlPlaneNameLabel:      "cp",
			controlPlaneNamespaceLabel: "other-ns",
		},
	}}
	g.Expect(isControlPlaneChild(other, cp)).To(BeFalse(),
		"a child of a same-named ControlPlane in another namespace must not be adopted")

	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "identity"}}
	g.Expect(isControlPlaneChild(foreign, cp)).To(BeFalse())
}

// TestCrossNamespaceChildMapper verifies a labelled child resolves back to its
// ControlPlane, and an unlabelled object wakes nobody — the same-namespace
// children keep flowing through Owns() alone, and a foreign object in a service
// namespace must not enqueue a reconcile.
func TestCrossNamespaceChildMapper(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := namespacedControlPlane()

	labelled := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "identity"}}
	stampControlPlaneChildLabels(labelled, cp)
	g.Expect(crossNamespaceChildMapper(context.Background(), labelled)).To(ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "openstack", Name: "cp"}},
	))

	unlabelled := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "identity"}}
	g.Expect(crossNamespaceChildMapper(context.Background(), unlabelled)).To(BeEmpty())

	// A half-stamped object (one label only) is not resolvable and must not be
	// mapped to a ControlPlane in the empty namespace "".
	partial := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "child", Namespace: "identity",
		Labels: map[string]string{controlPlaneNameLabel: "cp"},
	}}
	g.Expect(crossNamespaceChildMapper(context.Background(), partial)).To(BeEmpty())
}

// TestCrossNamespaceChildPredicate verifies the Watch-leg predicate admits only
// objects carrying both ownership labels, across every event kind. An unlabelled
// or half-stamped object is filtered before the mapper runs, so the shared
// informers — and the cluster-wide Namespace informer — never wake the mapper for
// a foreign object.
func TestCrossNamespaceChildPredicate(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := namespacedControlPlane()
	p := crossNamespaceChildPredicate()

	labelled := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "identity"}}
	stampControlPlaneChildLabels(labelled, cp)
	g.Expect(p.Create(event.CreateEvent{Object: labelled})).To(BeTrue())
	g.Expect(p.Update(event.UpdateEvent{ObjectOld: labelled, ObjectNew: labelled})).To(BeTrue())
	g.Expect(p.Delete(event.DeleteEvent{Object: labelled})).To(BeTrue())
	g.Expect(p.Generic(event.GenericEvent{Object: labelled})).To(BeTrue())

	unlabelled := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "identity"}}
	g.Expect(p.Create(event.CreateEvent{Object: unlabelled})).To(BeFalse())
	g.Expect(p.Update(event.UpdateEvent{ObjectOld: unlabelled, ObjectNew: unlabelled})).To(BeFalse())
	g.Expect(p.Delete(event.DeleteEvent{Object: unlabelled})).To(BeFalse())
	g.Expect(p.Generic(event.GenericEvent{Object: unlabelled})).To(BeFalse())

	// A half-stamped object (one label only) is not a resolvable child, so the
	// predicate filters it exactly as crossNamespaceChildMapper would discard it.
	partial := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "child", Namespace: "identity",
		Labels: map[string]string{controlPlaneNameLabel: "cp"},
	}}
	g.Expect(p.Create(event.CreateEvent{Object: partial})).To(BeFalse())
}

// TestControlPlaneNamespaces verifies the occupied-namespace set: the
// ControlPlane's own namespace plus every service namespace, deduplicated.
func TestControlPlaneNamespaces(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := namespacedControlPlane()
	g.Expect(controlPlaneNamespaces(cp)).To(ConsistOf("openstack", "identity", "dashboard"))

	plain := namespacedControlPlane()
	plain.Spec.Services.Keystone.Namespace = nil
	plain.Spec.Services.Horizon.Namespace = nil
	g.Expect(controlPlaneNamespaces(plain)).To(ConsistOf("openstack"))

	colocated := namespacedControlPlane()
	colocated.Spec.Services.Horizon.Namespace.Name = "identity"
	colocated.Spec.Services.Horizon.Namespace.Lifecycle = c5c3v1alpha1.ServiceNamespaceLifecycleManaged
	g.Expect(controlPlaneNamespaces(colocated)).To(ConsistOf("openstack", "identity"),
		"co-located services share one namespace, which is listed once")
}
