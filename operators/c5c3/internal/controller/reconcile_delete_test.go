// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the ControlPlane ORC-teardown finalizer (reconcileDelete): the
// ControlPlane CR is held in etcd until the operator-owned K-ORC CRs are gone,
// with a bounded stall escape that force-removes their finalizers and releases
// the ControlPlane anyway.
package controller

import (
	"context"
	"testing"
	"time"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/forge/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// drainEvents returns every event currently buffered on the FakeRecorder. Each
// entry is "<type> <reason> <message>".
func drainEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		if len(rec.Events) > 0 {
			out = append(out, <-rec.Events)
		} else {
			return out
		}
	}
}

// deletingControlPlane returns a ControlPlane being deleted (DeletionTimestamp
// set deletionAge in the past) carrying the ORC-teardown finalizer, so
// reconcileDelete drives its teardown.
func deletingControlPlane(deletionAge time.Duration) *c5c3v1alpha1.ControlPlane {
	cp := korcControlPlane()
	ts := metav1.NewTime(metav1.Now().Add(-deletionAge))
	cp.DeletionTimestamp = &ts
	cp.Finalizers = []string{controlPlaneORCFinalizer}
	return cp
}

// TestReconcile_AddsORCFinalizerOnFirstReconcile asserts that a fresh
// (non-deleting) ControlPlane gets the ORC-teardown finalizer installed and the
// reconcile requeues before any sub-reconciler runs.
func TestReconcile_AddsORCFinalizerOnFirstReconcile(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{Requeue: true}),
		"first reconcile must requeue after installing the finalizer")

	got := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, got)).To(Succeed())
	g.Expect(controllerutil.ContainsFinalizer(got, controlPlaneORCFinalizer)).To(BeTrue(),
		"the ORC-teardown finalizer must be installed")
}

// TestReconcileDelete_NoFinalizer_NoOp asserts reconcileDelete is a no-op when
// the ControlPlane does not carry the ORC-teardown finalizer: it must not touch
// any K-ORC CR.
func TestReconcileDelete_NoFinalizer_NoOp(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	ac := &orcv1alpha1.ApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:       adminAppCredentialName(cp),
			Namespace:  childNamespace(cp),
			Finalizers: []string{"openstack.k-orc.cloud/applicationcredential"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ac).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	// A deleting ControlPlane that carries only a foreign finalizer.
	del := metav1.Now()
	cp.DeletionTimestamp = &del
	cp.Finalizers = []string{"example.com/other"}

	res, err := r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}), "no-op delete must return a zero result")

	got := &orcv1alpha1.ApplicationCredential{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(ac), got)).To(Succeed())
	g.Expect(got.DeletionTimestamp.IsZero()).To(BeTrue(),
		"reconcileDelete must not delete K-ORC CRs when its finalizer is absent")
	g.Expect(drainEvents(rec)).To(BeEmpty(), "no-op delete must not emit events")
}

// TestReconcileDelete_NoORCResources_ReleasesFinalizer asserts that when no
// K-ORC CRs remain, the ControlPlane finalizer is released in one pass (and the
// CR is then garbage-collected).
func TestReconcileDelete_NoORCResources_ReleasesFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := deletingControlPlane(0)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	// Refresh from the client so the Update carries the right resourceVersion.
	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(c.Get(context.Background(), key, cp)).To(Succeed())

	res, err := r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}), "release must return a zero result")

	err = c.Get(context.Background(), key, &c5c3v1alpha1.ControlPlane{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"releasing the last finalizer must let the ControlPlane be garbage-collected")
	g.Expect(drainEvents(rec)).To(ContainElement(ContainSubstring("ORCTeardownComplete")))
}

// TestReconcileDelete_WaitsWhileORCTerminating asserts that while an owned K-ORC
// CR is still present, reconcileDelete holds the ControlPlane finalizer, reports
// KORCReady=False/FinalizingORC, and requeues. Deleting the live CR marks it
// Terminating (it carries a K-ORC finalizer) and emits FinalizingORC once.
func TestReconcileDelete_WaitsWhileORCTerminating(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := deletingControlPlane(0)
	// A live AC (no DeletionTimestamp) carrying a K-ORC finalizer: deleting it
	// transitions it to Terminating rather than removing it outright.
	ac := &orcv1alpha1.ApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:       adminAppCredentialName(cp),
			Namespace:  childNamespace(cp),
			Finalizers: []string{"openstack.k-orc.cloud/applicationcredential"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ac).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(c.Get(context.Background(), key, cp)).To(Succeed())

	res, err := r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter),
		"a still-Terminating K-ORC CR must requeue at the K-ORC cadence")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeKORCReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("FinalizingORC"))

	g.Expect(controllerutil.ContainsFinalizer(cp, controlPlaneORCFinalizer)).To(BeTrue(),
		"the ControlPlane finalizer must be held while K-ORC CRs remain")

	gotAC := &orcv1alpha1.ApplicationCredential{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(ac), gotAC)).To(Succeed())
	g.Expect(gotAC.DeletionTimestamp.IsZero()).To(BeFalse(),
		"the owned K-ORC CR must have been marked for deletion")

	g.Expect(drainEvents(rec)).To(ContainElement(ContainSubstring("FinalizingORC")))
}

// TestReconcileDelete_ForceRemovesORCFinalizersAfterStall asserts the stall
// escape: once the ControlPlane has been Terminating past orcTeardownStallTimeout
// with K-ORC CRs still stuck, reconcileDelete strips their K-ORC finalizers
// (preserving non-K-ORC finalizers), emits a Warning, and releases the
// ControlPlane finalizer.
func TestReconcileDelete_ForceRemovesORCFinalizersAfterStall(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := deletingControlPlane(2 * orcTeardownStallTimeout)
	// An AC stuck Terminating behind a K-ORC finalizer AND a foreign finalizer
	// that must survive the force-remove.
	acDeletion := metav1.NewTime(metav1.Now().Add(-2 * orcTeardownStallTimeout))
	ac := &orcv1alpha1.ApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:              adminAppCredentialName(cp),
			Namespace:         childNamespace(cp),
			Finalizers:        []string{"openstack.k-orc.cloud/applicationcredential", "example.com/keep"},
			DeletionTimestamp: &acDeletion,
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ac).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(c.Get(context.Background(), key, cp)).To(Succeed())

	res, err := r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}), "the stall escape must release without requeue")

	// The K-ORC finalizer is stripped; the foreign one survives.
	gotAC := &orcv1alpha1.ApplicationCredential{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(ac), gotAC)).To(Succeed())
	g.Expect(gotAC.Finalizers).To(Equal([]string{"example.com/keep"}),
		"only the openstack.k-orc.cloud/* finalizer must be force-removed")

	// The ControlPlane finalizer is released, so the CR is garbage-collected.
	err = c.Get(context.Background(), key, &c5c3v1alpha1.ControlPlane{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the ControlPlane finalizer must be released after the stall escape")

	events := drainEvents(rec)
	g.Expect(events).To(ContainElement(SatisfyAll(
		ContainSubstring("Warning"),
		ContainSubstring("ORCTeardownStalled"),
	)), "the stall escape must emit a Warning ORCTeardownStalled event")
}

// deletingExternalControlPlane returns an External-mode ControlPlane being
// deleted, carrying the ORC-teardown finalizer.
func deletingExternalControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := korcExternalControlPlane()
	ts := metav1.NewTime(metav1.Now().Add(-time.Second))
	cp.DeletionTimestamp = &ts
	cp.Finalizers = []string{controlPlaneORCFinalizer}
	return cp
}

// externalModeORCChildren returns the five owned K-ORC CRs an External-mode
// ControlPlane projects, with the ManagementPolicy each really carries: the
// ApplicationCredential is Managed (its finalizer revokes at the Keystone level),
// while the admin User/Domain are Unmanaged imports whose CR deletion cannot touch
// the external Keystone.
func externalModeORCChildren(cp *c5c3v1alpha1.ControlPlane) []client.Object {
	ns := childNamespace(cp)
	return []client.Object{
		&orcv1alpha1.ApplicationCredential{
			ObjectMeta: metav1.ObjectMeta{Name: adminAppCredentialName(cp), Namespace: ns},
			Spec:       orcv1alpha1.ApplicationCredentialSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyManaged},
		},
		&orcv1alpha1.Service{ObjectMeta: metav1.ObjectMeta{Name: keystoneServiceName(cp), Namespace: ns}},
		&orcv1alpha1.Endpoint{ObjectMeta: metav1.ObjectMeta{Name: keystoneEndpointName(cp), Namespace: ns}},
		&orcv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{Name: adminUserRef(cp), Namespace: ns},
			Spec: orcv1alpha1.UserSpec{
				ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged,
				Import:           &orcv1alpha1.UserImport{Filter: &orcv1alpha1.UserFilter{}},
			},
		},
		&orcv1alpha1.Domain{
			ObjectMeta: metav1.ObjectMeta{Name: adminDomainRef(cp), Namespace: ns},
			Spec: orcv1alpha1.DomainSpec{
				ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged,
				Import:           &orcv1alpha1.DomainImport{Filter: &orcv1alpha1.DomainFilter{}},
			},
		},
	}
}

// TestReconcileDelete_ExternalMode_TearsDownOnlyOwnedORCCRs is the AC-4 guard:
// deleting an External-mode ControlPlane removes exactly the K-ORC CRs the
// operator owns — and provably nothing else. A same-namespace K-ORC User that the
// ControlPlane never created (another tenant's import) must survive.
func TestReconcileDelete_ExternalMode_TearsDownOnlyOwnedORCCRs(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	s := korcTestScheme(t)
	cp := deletingExternalControlPlane()
	foreign := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "someone-elses-user", Namespace: childNamespace(cp)},
		Spec:       orcv1alpha1.UserSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged},
	}
	objs := append([]client.Object{cp, foreign}, externalModeORCChildren(cp)...)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	// No K-ORC finalizers are seeded, so the CRs vanish on Delete and the sweep
	// releases the ControlPlane finalizer in one pass.
	res, err := r.reconcileDelete(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	g.Expect(controllerutil.ContainsFinalizer(cp, controlPlaneORCFinalizer)).To(BeFalse(),
		"the ControlPlane finalizer must be released once every owned K-ORC CR is gone")

	// Every owned K-ORC CR is gone.
	for _, child := range orcChildResources {
		obj := child.newObj()
		key := types.NamespacedName{Name: child.name(cp), Namespace: childNamespace(cp)}
		g.Expect(apierrors.IsNotFound(c.Get(ctx, key, obj))).To(BeTrue(),
			"owned K-ORC CR %s must be deleted", key.Name)
	}

	// ... and provably nothing else. The unrelated import survives untouched.
	survivor := &orcv1alpha1.User{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "someone-elses-user", Namespace: childNamespace(cp)}, survivor)).
		To(Succeed(), "a K-ORC CR the ControlPlane does not own must never be swept")
}

// TestDeleteORCResources_ExternalMode_LeavesUnmanagedImportsUntouched pins WHY the
// sweep has zero blast radius on the external installation: the admin User/Domain
// the sweep deletes are Unmanaged imports, so removing their CRs cannot delete the
// OpenStack resources behind them. Only the ApplicationCredential is Managed — its
// K-ORC finalizer revokes at the Keystone level before the CR delete returns, so
// authenticating with the revoked credential afterwards yields 404 "Could not find
// Application Credential" (not 401).
func TestDeleteORCResources_ExternalMode_LeavesUnmanagedImportsUntouched(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	s := korcTestScheme(t)
	cp := deletingExternalControlPlane()
	objs := append([]client.Object{cp}, externalModeORCChildren(cp)...)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	// Read the management policies the sweep is about to act on, BEFORE the sweep.
	user := &orcv1alpha1.User{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: adminUserRef(cp), Namespace: childNamespace(cp)}, user)).To(Succeed())
	g.Expect(user.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged))
	g.Expect(user.Spec.Import).NotTo(BeNil(), "the admin User is an import, not an owned resource")

	domain := &orcv1alpha1.Domain{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: adminDomainRef(cp), Namespace: childNamespace(cp)}, domain)).To(Succeed())
	g.Expect(domain.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged))
	g.Expect(domain.Spec.Import).NotTo(BeNil())

	ac := &orcv1alpha1.ApplicationCredential{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: adminAppCredentialName(cp), Namespace: childNamespace(cp)}, ac)).To(Succeed())
	g.Expect(ac.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged),
		"the app credential is the only identity object the operator minted, so the only one it revokes")

	remaining, hasLiveWork, err := r.deleteORCResources(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(hasLiveWork).To(BeTrue(), "live (not-yet-Terminating) CRs must announce the teardown once")
	g.Expect(remaining).To(BeEmpty())
}

// TestReconcileDelete_ExternalMode_NoORCResources_ReleasesFinalizer covers the
// edge path where the K-ORC chain never converged: an External-mode ControlPlane
// deleted before any K-ORC CR was projected must still release its finalizer
// rather than wedge on Terminating.
func TestReconcileDelete_ExternalMode_NoORCResources_ReleasesFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := deletingExternalControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	res, err := r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	g.Expect(controllerutil.ContainsFinalizer(cp, controlPlaneORCFinalizer)).To(BeFalse())
}
