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
	"strings"
	"testing"
	"time"

	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

// terminatingImportMeta builds the ObjectMeta of a Terminating K-ORC CR: a
// K-ORC finalizer holds it and its DeletionTimestamp is set — the state the
// identity imports sit in once the teardown has revoked the application
// credential their finalizers would authenticate with.
func terminatingImportMeta(name, ns, finalizer string) metav1.ObjectMeta {
	ts := metav1.NewTime(metav1.Now().Add(-30 * time.Second))
	return metav1.ObjectMeta{
		Name:              name,
		Namespace:         ns,
		Finalizers:        []string{finalizer},
		DeletionTimestamp: &ts,
	}
}

// TestReconcileDelete_ReleasesUnmanagedImportsWithoutStall is the regression
// guard for the teardown wedge the external-keystone suite exposed: after the
// managed children (application credential included) are gone, the only CRs
// left are Unmanaged imports whose K-ORC finalizers can never run again — the
// revoked credential is the one they authenticate with. reconcileDelete must
// release them immediately (Normal event, finalizer held, no Warning), NOT
// wait out the five-minute stall window and alarm with ORCTeardownStalled.
func TestReconcileDelete_ReleasesUnmanagedImportsWithoutStall(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := deletingExternalControlPlane() // deleted 1s ago — well inside the stall window
	ns := childNamespace(cp)
	svc := &orcv1alpha1.Service{
		ObjectMeta: terminatingImportMeta(keystoneServiceName(cp), ns, "openstack.k-orc.cloud/service"),
		Spec:       orcv1alpha1.ServiceSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged},
	}
	domain := &orcv1alpha1.Domain{
		ObjectMeta: terminatingImportMeta(adminDomainRef(cp), ns, "openstack.k-orc.cloud/domain"),
		Spec:       orcv1alpha1.DomainSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, svc, domain).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(c.Get(context.Background(), key, cp)).To(Succeed())

	res, err := r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter),
		"the release pass must requeue to confirm the imports are gone")

	// Stripping the only (K-ORC) finalizer completes the deletions.
	err = c.Get(context.Background(), client.ObjectKeyFromObject(svc), &orcv1alpha1.Service{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the released Service import must be gone")
	err = c.Get(context.Background(), client.ObjectKeyFromObject(domain), &orcv1alpha1.Domain{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the released Domain import must be gone")

	g.Expect(controllerutil.ContainsFinalizer(cp, controlPlaneORCFinalizer)).To(BeTrue(),
		"the ControlPlane finalizer is held until the follow-up pass confirms emptiness")
	events := drainEvents(rec)
	g.Expect(events).To(ContainElement(ContainSubstring("ORCImportsReleased")))
	g.Expect(events).NotTo(ContainElement(ContainSubstring("Warning")),
		"releasing unmanaged imports orphans nothing and must not alarm")

	// The follow-up pass finds nothing remaining and releases the ControlPlane.
	g.Expect(c.Get(context.Background(), key, cp)).To(Succeed())
	res, err = r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	err = c.Get(context.Background(), key, &c5c3v1alpha1.ControlPlane{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	g.Expect(drainEvents(rec)).To(ContainElement(ContainSubstring("ORCTeardownComplete")))
}

// TestReconcileDelete_WaitsForOwnedPushSecretCleanup is the regression guard
// for the OpenBao-orphan race the external-keystone suite exposed: the owned
// PushSecrets carry DeletionPolicy=Delete, and ESO can only delete the
// mirrored OpenBao data while the per-tenant store and its ServiceAccount are
// alive — both die in the GC cascade the moment the ControlPlane finalizer is
// released. reconcileDelete must therefore delete the PushSecrets itself and
// hold the finalizer until they are gone.
func TestReconcileDelete_WaitsForOwnedPushSecretCleanup(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := deletingExternalControlPlane()
	ns := childNamespace(cp)
	// An owned PushSecret still live (not yet Terminating), held by ESO's
	// finalizer once deleted — the state right after the CP delete lands.
	ps := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            adminAppCredentialPushSecretName(cp),
			Namespace:       ns,
			OwnerReferences: ownedByCP(cp),
			Finalizers:      []string{"pushsecret.externalsecrets.io/finalizer"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ps).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(c.Get(context.Background(), key, cp)).To(Succeed())

	res, err := r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter),
		"the teardown must wait for ESO to finish the OpenBao cleanup")

	gotPS := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(ps), gotPS)).To(Succeed())
	g.Expect(gotPS.DeletionTimestamp.IsZero()).To(BeFalse(),
		"the owned PushSecret must have been deleted by the teardown, not left to GC")
	g.Expect(controllerutil.ContainsFinalizer(cp, controlPlaneORCFinalizer)).To(BeTrue(),
		"the ControlPlane finalizer must be held while the PushSecret cleanup runs")
	g.Expect(drainEvents(rec)).NotTo(ContainElement(ContainSubstring("ORCTeardownComplete")))

	// ESO finishes: the remote data is deleted and the finalizer released.
	gotPS.Finalizers = nil
	g.Expect(c.Update(context.Background(), gotPS)).To(Succeed())

	g.Expect(c.Get(context.Background(), key, cp)).To(Succeed())
	res, err = r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	err = c.Get(context.Background(), key, &c5c3v1alpha1.ControlPlane{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	g.Expect(drainEvents(rec)).To(ContainElement(ContainSubstring("ORCTeardownComplete")))
}

// TestReconcileDelete_MixedRemainderStillWaitsForManaged asserts the release
// shortcut stays gated on the managed children: while a managed CR (here the
// application credential, whose revocation is real OpenStack work) is still
// Terminating, the unmanaged imports keep their K-ORC finalizers and the
// teardown waits at the K-ORC cadence.
func TestReconcileDelete_MixedRemainderStillWaitsForManaged(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := deletingExternalControlPlane()
	ns := childNamespace(cp)
	ac := &orcv1alpha1.ApplicationCredential{
		// ManagementPolicy unset counts as managed (fail-loud default).
		ObjectMeta: terminatingImportMeta(adminAppCredentialName(cp), ns, "openstack.k-orc.cloud/applicationcredential"),
	}
	svc := &orcv1alpha1.Service{
		ObjectMeta: terminatingImportMeta(keystoneServiceName(cp), ns, "openstack.k-orc.cloud/service"),
		Spec:       orcv1alpha1.ServiceSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ac, svc).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(c.Get(context.Background(), key, cp)).To(Succeed())

	res, err := r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))

	gotSvc := &orcv1alpha1.Service{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(svc), gotSvc)).To(Succeed())
	g.Expect(gotSvc.Finalizers).To(ContainElement("openstack.k-orc.cloud/service"),
		"unmanaged imports must NOT be released while a managed CR still needs K-ORC")

	g.Expect(drainEvents(rec)).NotTo(ContainElement(ContainSubstring("ORCImportsReleased")))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeKORCReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal("FinalizingORC"))
}

// deletingExternalOptInControlPlane returns an External-mode ControlPlane being
// deleted that declared one opt-in catalog entry — the one thing this operator
// created in the external catalog, and therefore the one thing it removes from it.
func deletingExternalOptInControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := deletingExternalControlPlane()
	cp.Spec.Services.Keystone.External.Catalog = &c5c3v1alpha1.ExternalCatalogSpec{
		ManagedEntries: []c5c3v1alpha1.ExternalCatalogEntrySpec{{
			Type: "image",
			Endpoints: []c5c3v1alpha1.ExternalCatalogEndpointSpec{
				{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "https://glance.example.com"},
			},
		}},
	}
	return cp
}

// externalModeORCChildren returns the owned K-ORC CRs an External-mode
// ControlPlane projects, with the ManagementPolicy each really carries: the
// ApplicationCredential and any opt-in catalog entry are Managed (their finalizers
// revoke/delete at the Keystone level), while the admin User/Domain and the whole
// identity catalog — the Service plus one Endpoint per interface — are Unmanaged
// imports whose CR deletion cannot touch the external Keystone.
func externalModeORCChildren(cp *c5c3v1alpha1.ControlPlane) []client.Object {
	ns := childNamespace(cp)
	objs := []client.Object{
		&orcv1alpha1.ApplicationCredential{
			ObjectMeta: metav1.ObjectMeta{Name: adminAppCredentialName(cp), Namespace: ns},
			Spec:       orcv1alpha1.ApplicationCredentialSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyManaged},
		},
		&orcv1alpha1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: keystoneServiceName(cp), Namespace: ns},
			Spec: orcv1alpha1.ServiceSpec{
				ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged,
				Import:           &orcv1alpha1.ServiceImport{Filter: &orcv1alpha1.ServiceFilter{}},
			},
		},
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
	for _, iface := range externalCatalogInterfaces {
		objs = append(objs, &orcv1alpha1.Endpoint{
			ObjectMeta: metav1.ObjectMeta{Name: keystoneEndpointImportName(cp, iface), Namespace: ns},
			Spec: orcv1alpha1.EndpointSpec{
				ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged,
				Import:           &orcv1alpha1.EndpointImport{Filter: &orcv1alpha1.EndpointFilter{}},
			},
		})
	}
	for _, entry := range externalManagedCatalogEntries(cp) {
		objs = append(objs, &orcv1alpha1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: catalogEntryServiceName(cp, entry.Type), Namespace: ns},
			Spec:       orcv1alpha1.ServiceSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyManaged},
		})
		for _, ep := range entry.Endpoints {
			objs = append(objs, &orcv1alpha1.Endpoint{
				ObjectMeta: metav1.ObjectMeta{
					Name:      catalogEntryEndpointName(cp, entry.Type, ep.Interface),
					Namespace: ns,
				},
				Spec: orcv1alpha1.EndpointSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyManaged},
			})
		}
	}
	return objs
}

// TestReconcileDelete_ExternalMode_TearsDownOnlyOwnedORCCRs is the AC-4 guard:
// deleting an External-mode ControlPlane removes exactly the K-ORC CRs the
// operator owns — and provably nothing else. A same-namespace K-ORC User that the
// ControlPlane never created (another tenant's import) must survive.
func TestReconcileDelete_ExternalMode_TearsDownOnlyOwnedORCCRs(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	s := korcTestScheme(t)
	// The opt-in variant, so the sweep is proven to cover the catalog imports AND
	// the entry CRs this ControlPlane created.
	cp := deletingExternalOptInControlPlane()
	foreign := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "someone-elses-user", Namespace: childNamespace(cp)},
		Spec:       orcv1alpha1.UserSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged},
	}
	// A same-namespace Endpoint import that looks like a catalog import of a
	// DIFFERENT ControlPlane: only the cp.Name-scoped names keep it safe.
	foreignEndpoint := &orcv1alpha1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "other-cp-identity-endpoint-public", Namespace: childNamespace(cp)},
	}
	objs := append([]client.Object{cp, foreign, foreignEndpoint}, externalModeORCChildren(cp)...)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	// No K-ORC finalizers are seeded, so the CRs vanish on Delete and the sweep
	// releases the ControlPlane finalizer in one pass.
	res, err := r.reconcileDelete(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	g.Expect(controllerutil.ContainsFinalizer(cp, controlPlaneORCFinalizer)).To(BeFalse(),
		"the ControlPlane finalizer must be released once every owned K-ORC CR is gone")

	// Every owned K-ORC CR is gone — including the three per-interface identity
	// Endpoint imports and the opt-in entry's Service/Endpoint.
	children := orcChildObjects(cp)
	g.Expect(children).To(HaveLen(5+len(externalCatalogInterfaces)+2),
		"the sweep must enumerate the catalog imports and the declared entry")
	for _, child := range children {
		obj := child.newObj()
		key := types.NamespacedName{Name: child.name, Namespace: childNamespace(cp)}
		g.Expect(apierrors.IsNotFound(c.Get(ctx, key, obj))).To(BeTrue(),
			"owned K-ORC CR %s must be deleted", key.Name)
	}

	// ... and provably nothing else. The unrelated imports survive untouched.
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "someone-elses-user", Namespace: childNamespace(cp)},
		&orcv1alpha1.User{})).To(Succeed(), "a K-ORC CR the ControlPlane does not own must never be swept")
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(foreignEndpoint), &orcv1alpha1.Endpoint{})).
		To(Succeed(), "another ControlPlane's catalog import must never be swept")
}

// TestDeleteORCResources_ExternalMode_LeavesUnmanagedImportsUntouched pins WHY the
// sweep has zero blast radius on the external installation: the admin User/Domain
// AND the whole identity catalog the sweep deletes are Unmanaged imports, so
// removing their CRs cannot delete the OpenStack resources behind them — the
// external catalog is left bit-for-bit intact. Only the ApplicationCredential is
// Managed — its K-ORC finalizer revokes at the Keystone level before the CR delete
// returns, so authenticating with the revoked credential afterwards yields 404
// "Could not find Application Credential" (not 401).
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

	// The catalog itself: the identity Service and every endpoint interface are
	// imports, so teardown never removes a row from the external catalog.
	svc := &orcv1alpha1.Service{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: keystoneServiceName(cp), Namespace: childNamespace(cp)}, svc)).To(Succeed())
	g.Expect(svc.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged),
		"the identity Service is an import, so its CR delete cannot touch the external catalog")
	g.Expect(svc.Spec.Import).NotTo(BeNil())
	for _, iface := range externalCatalogInterfaces {
		ep := &orcv1alpha1.Endpoint{}
		g.Expect(c.Get(ctx, types.NamespacedName{
			Name: keystoneEndpointImportName(cp, iface), Namespace: childNamespace(cp),
		}, ep)).To(Succeed())
		g.Expect(ep.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged),
			"the %q endpoint is an import, so its CR delete cannot touch the external catalog", iface)
		g.Expect(ep.Spec.Import).NotTo(BeNil())
	}

	ac := &orcv1alpha1.ApplicationCredential{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: adminAppCredentialName(cp), Namespace: childNamespace(cp)}, ac)).To(Succeed())
	g.Expect(ac.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged),
		"the app credential is the only identity object the operator minted, so the only one it revokes")

	remaining, hasLiveWork, err := r.deleteORCResources(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(hasLiveWork).To(BeTrue(), "live (not-yet-Terminating) CRs must announce the teardown once")
	g.Expect(remaining).To(BeEmpty())
}

// TestOrcChildObjects_ManagedModeUnchanged is the golden-behavior guard on the
// sweep: a Managed ControlPlane still enumerates exactly the five CRs it always
// did, so the External-mode additions cannot widen the managed blast radius.
func TestOrcChildObjects_ManagedModeUnchanged(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := korcControlPlane()
	children := orcChildObjects(cp)

	g.Expect(children).To(HaveLen(5))
	names := make([]string, 0, len(children))
	for _, child := range children {
		names = append(names, child.name)
	}
	g.Expect(names).To(ConsistOf(
		adminAppCredentialName(cp),
		keystoneServiceName(cp),
		keystoneEndpointName(cp),
		adminUserRef(cp),
		adminDomainRef(cp),
	))
}

// TestOrcChildObjects_ExternalOptInEnumeratesDeclaredEntry proves the sweep tracks
// the spec: an entry declared today is torn down, and an entry the spec never
// declared is never named (so a stale CR is not swept by the finalizer — the
// reconcile-time prune owns that).
func TestOrcChildObjects_ExternalOptInEnumeratesDeclaredEntry(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := deletingExternalOptInControlPlane()
	names := make([]string, 0)
	for _, child := range orcChildObjects(cp) {
		names = append(names, child.name)
	}

	g.Expect(names).To(ContainElement(catalogEntryServiceName(cp, "image")))
	g.Expect(names).To(ContainElement(catalogEntryEndpointName(cp, "image", c5c3v1alpha1.ExternalEndpointTypePublic)))
	g.Expect(names).NotTo(ContainElement(catalogEntryServiceName(cp, "compute")),
		"an entry the spec never declared must not be named by the sweep")
	for _, iface := range externalCatalogInterfaces {
		g.Expect(names).To(ContainElement(keystoneEndpointImportName(cp, iface)))
	}
}

// ownedCatalogEntryCRs returns an entry Service/Endpoint pair carrying cp's
// controller reference and the catalog-entry name prefix — the CRs a declared
// `entryType` entry projects — so a test can seed them independently of what the
// spec declares today.
func ownedCatalogEntryCRs(
	t *testing.T, s *runtime.Scheme, cp *c5c3v1alpha1.ControlPlane, entryType string,
) (*orcv1alpha1.Service, *orcv1alpha1.Endpoint) {
	t.Helper()
	g := NewGomegaWithT(t)

	svc := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: catalogEntryServiceName(cp, entryType), Namespace: childNamespace(cp)},
		Spec:       orcv1alpha1.ServiceSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyManaged},
	}
	ep := &orcv1alpha1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      catalogEntryEndpointName(cp, entryType, c5c3v1alpha1.ExternalEndpointTypePublic),
			Namespace: childNamespace(cp),
		},
		Spec: orcv1alpha1.EndpointSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyManaged},
	}
	g.Expect(controllerutil.SetControllerReference(cp, svc, s)).To(Succeed())
	g.Expect(controllerutil.SetControllerReference(cp, ep, s)).To(Succeed())
	return svc, ep
}

// TestReconcileDelete_ExternalMode_SweepsUndeclaredOwnedEntryCRs closes the gap
// between the two enumerations: the reconcile-time prune finds entry CRs by
// OWNERSHIP, the teardown sweep used to find them by SPEC. They diverge whenever
// a declaration is dropped from a spec the prune never re-observed — it runs
// inside reconcileCatalogExternal, which reconcileCatalog gates on
// AdminCredentialReady and which never runs once DeletionTimestamp is set. The
// unswept CRs would then be garbage-collected into a permanent Terminating state
// behind their K-ORC finalizers, with the credentials Secret already gone and the
// stall escape blind to them — the exact `kubectl delete namespace` wedge
// reconcileDelete exists to prevent.
func TestReconcileDelete_ExternalMode_SweepsUndeclaredOwnedEntryCRs(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	s := korcTestScheme(t)
	cp := deletingExternalControlPlane() // the spec declares NO managed entries
	staleSvc, staleEp := ownedCatalogEntryCRs(t, s, cp, "image")
	// A CR carrying the entry prefix but owned by nobody: the prefix alone must not
	// sweep it, exactly as the reconcile-time prune requires.
	foreign := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: catalogEntryServiceName(cp, "compute"), Namespace: childNamespace(cp)},
	}

	objs := append([]client.Object{cp, staleSvc, staleEp, foreign}, externalModeORCChildren(cp)...)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	res, err := r.reconcileDelete(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	g.Expect(controllerutil.ContainsFinalizer(cp, controlPlaneORCFinalizer)).To(BeFalse())

	g.Expect(apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(staleSvc), &orcv1alpha1.Service{}))).
		To(BeTrue(), "an owned entry Service the spec no longer declares must still be swept")
	g.Expect(apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(staleEp), &orcv1alpha1.Endpoint{}))).
		To(BeTrue(), "an owned entry Endpoint the spec no longer declares must still be swept")
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(foreign), &orcv1alpha1.Service{})).
		To(Succeed(), "a prefixed CR this ControlPlane does not own must never be swept")
}

// stalledExternalORCChildren returns every owned K-ORC CR of an External-mode
// ControlPlane, each stuck Terminating behind a K-ORC finalizer — the state the stall
// escape releases. The management policies are the ones the reconcilers really set,
// so a test can tell apart the CRs whose release leaks an OpenStack resource from the
// ones whose release costs nothing.
func stalledExternalORCChildren(cp *c5c3v1alpha1.ControlPlane) []client.Object {
	deletion := metav1.NewTime(metav1.Now().Add(-2 * orcTeardownStallTimeout))
	objs := externalModeORCChildren(cp)
	for _, obj := range objs {
		obj.SetFinalizers([]string{korcFinalizerPrefix + "stuck"})
		obj.SetDeletionTimestamp(&deletion)
	}
	return objs
}

// TestReconcileDelete_StallEscapeNamesOrphanedManagedResources is the guard on the
// blast radius the catalog-entry sweep added to the stall escape. The escape strips
// openstack.k-orc.cloud/* finalizers with no ManagementPolicy check, so it releases a
// Managed catalog-entry CR by removing the very finalizer that would have taken its
// row out of the customer's catalog. The row survives with no Kubernetes object naming
// it. That is unavoidable — the alternative is a permanently wedged namespace — but a
// flat list of CR names under "unable to reach Keystone to revoke" never says a
// catalog row leaked, and `kubectl delete namespace` makes the leak deterministic (the
// namespace controller reaps the entries' credentials Secret alongside their CRs).
//
// The escape must therefore name exactly the Managed CRs it orphaned, and never the
// Unmanaged imports, whose CR deletion could not have touched OpenStack anyway.
func TestReconcileDelete_StallEscapeNamesOrphanedManagedResources(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := deletingExternalOptInControlPlane()
	stalled := metav1.NewTime(metav1.Now().Add(-2 * orcTeardownStallTimeout))
	cp.DeletionTimestamp = &stalled

	objs := append([]client.Object{cp}, stalledExternalORCChildren(cp)...)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	rec := record.NewFakeRecorder(20)
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(c.Get(context.Background(), key, cp)).To(Succeed())

	res, err := r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}), "the stall escape must release without requeue")

	var orphanEvent string
	for _, event := range drainEvents(rec) {
		if strings.Contains(event, "ORCResourcesOrphaned") {
			orphanEvent = event
		}
	}
	g.Expect(orphanEvent).NotTo(BeEmpty(),
		"releasing a Managed K-ORC CR abandons its OpenStack resource and must be reported as such")
	g.Expect(orphanEvent).To(HavePrefix("Warning"))

	// The Managed CRs: the opt-in catalog rows this ControlPlane wrote into a catalog
	// it does not own, and the application credential it minted.
	g.Expect(orphanEvent).To(ContainSubstring(catalogEntryEndpointName(cp, "image", c5c3v1alpha1.ExternalEndpointTypePublic)))
	g.Expect(orphanEvent).To(ContainSubstring(catalogEntryServiceName(cp, "image")))
	g.Expect(orphanEvent).To(ContainSubstring(adminAppCredentialName(cp)))

	// The Unmanaged imports: their CR delete never called OpenStack, so nothing leaked.
	g.Expect(orphanEvent).NotTo(ContainSubstring(keystoneServiceName(cp)))
	g.Expect(orphanEvent).NotTo(ContainSubstring(adminUserRef(cp)))
	g.Expect(orphanEvent).NotTo(ContainSubstring(adminDomainRef(cp)))
	for _, iface := range externalCatalogInterfaces {
		g.Expect(orphanEvent).NotTo(ContainSubstring(keystoneEndpointImportName(cp, iface)))
	}
}

// TestIsManagedORCChild_UnsetPolicyCountsAsManaged pins the fail-loud default: K-ORC
// defaults managementPolicy to `managed`, so a CR whose policy the reconciler never
// stamped must be reported as orphaned rather than silently omitted from the warning.
func TestIsManagedORCChild_UnsetPolicyCountsAsManaged(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(isManagedORCChild(&orcv1alpha1.Service{})).To(BeTrue())
	g.Expect(isManagedORCChild(&orcv1alpha1.Service{
		Spec: orcv1alpha1.ServiceSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged},
	})).To(BeFalse())
}

// TestOrcTeardownChildren_DeclaredEntryNamedExactlyOnce guards the merge of the
// two enumerations. A declared entry appears in both, and naming it twice would
// make forceRemoveKORCFinalizers Update the same object off two stale reads — the
// second Update losing to a Conflict.
func TestOrcTeardownChildren_DeclaredEntryNamedExactlyOnce(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	s := korcTestScheme(t)
	cp := deletingExternalOptInControlPlane()
	svc, ep := ownedCatalogEntryCRs(t, s, cp, "image")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, svc, ep).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	children, err := r.orcTeardownChildren(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	seen := map[string]int{}
	for _, child := range children {
		seen[child.key()]++
	}
	for key, n := range seen {
		g.Expect(n).To(Equal(1), "child %s must be named exactly once", key)
	}
	g.Expect(children).To(HaveLen(len(orcChildObjects(cp))),
		"the declared entry is already spec-derived, so ownership adds nothing")
}

// TestOrcTeardownChildren_ManagedModeSkipsTheOwnershipSweep keeps the managed
// blast radius byte-identical: Managed mode projects no catalog-entry CRs, so it
// never pays for the List and can never name one.
func TestOrcTeardownChildren_ManagedModeSkipsTheOwnershipSweep(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	s := korcTestScheme(t)
	cp := korcControlPlane()
	// A prefixed, owned CR that External mode would sweep. Managed mode must not.
	svc, _ := ownedCatalogEntryCRs(t, s, cp, "image")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, svc).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	children, err := r.orcTeardownChildren(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(children).To(HaveLen(len(orcChildObjects(cp))))
	for _, child := range children {
		g.Expect(child.name).NotTo(Equal(svc.Name))
	}
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

// --- Service-account teardown ---

// deletingControlPlaneWithServiceAccount returns a deleting ControlPlane with one
// declared service account, so reconcileDelete sweeps its managed User/Project.
func deletingControlPlaneWithServiceAccount(deletionAge time.Duration) *c5c3v1alpha1.ControlPlane {
	cp := deletingControlPlane(deletionAge)
	cp.Spec.KORC.ServiceAccounts = []c5c3v1alpha1.ServiceAccountSpec{{
		Name:    "nova",
		Project: c5c3v1alpha1.ServiceAccountProjectSpec{Name: "service"},
	}}
	return cp
}

func TestOrcChildObjects_IncludesServiceAccountChildren(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()
	cp.Spec.KORC.ServiceAccounts = []c5c3v1alpha1.ServiceAccountSpec{{
		Name:    "nova",
		Project: c5c3v1alpha1.ServiceAccountProjectSpec{Name: "service"},
		Roles:   []string{"member"},
	}}
	sa := cp.Spec.KORC.ServiceAccounts[0]

	names := map[string]bool{}
	for _, child := range orcChildObjects(cp) {
		names[child.name] = true
	}
	g.Expect(names).To(HaveKey(serviceAccountUserRef(cp, sa)))
	g.Expect(names).To(HaveKey(serviceAccountUserProbeRef(cp, sa)))
	g.Expect(names).To(HaveKey(serviceAccountProjectRef(cp, sa)))
	g.Expect(names).To(HaveKey(serviceAccountProjectProbeRef(cp, sa)))
	g.Expect(names).To(HaveKey(serviceAccountRoleImportRef(cp, "member")))
	g.Expect(names).To(HaveKey(serviceAccountRoleAssignmentRef(cp, sa, "member")))
}

func TestIsManagedORCChild_ClassifiesProject(t *testing.T) {
	g := NewGomegaWithT(t)
	managed := &orcv1alpha1.Project{Spec: orcv1alpha1.ProjectSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyManaged}}
	unmanaged := &orcv1alpha1.Project{Spec: orcv1alpha1.ProjectSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged}}
	g.Expect(isManagedORCChild(managed)).To(BeTrue(), "a managed Project leaks on force-remove")
	g.Expect(isManagedORCChild(unmanaged)).To(BeFalse(), "an unmanaged reference Project is a CR-only delete")
}

// TestIsManagedORCChild_ClassifiesRoleChildren pins the two role kinds: the managed
// RoleAssignment leaks on force-remove (its finalizer revokes the assignment in
// Keystone), while the unmanaged Role import is a CR-only delete that must be
// force-releasable without a false orphan warning.
func TestIsManagedORCChild_ClassifiesRoleChildren(t *testing.T) {
	g := NewGomegaWithT(t)
	managedAssignment := &orcv1alpha1.RoleAssignment{
		Spec: orcv1alpha1.RoleAssignmentSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyManaged},
	}
	unmanagedRole := &orcv1alpha1.Role{
		Spec: orcv1alpha1.RoleSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged},
	}
	g.Expect(isManagedORCChild(managedAssignment)).To(BeTrue(), "a managed RoleAssignment leaks on force-remove")
	g.Expect(isManagedORCChild(unmanagedRole)).To(BeFalse(), "an unmanaged Role import is a CR-only delete")
}

func TestReconcileDelete_ServiceAccount_TearsDownManagedUserAndProject(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := deletingControlPlaneWithServiceAccount(0)
	sa := cp.Spec.KORC.ServiceAccounts[0]
	ns := childNamespace(cp)
	user := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name: serviceAccountUserRef(cp, sa), Namespace: ns,
			Finalizers: []string{"openstack.k-orc.cloud/user"},
		},
		Spec: orcv1alpha1.UserSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyManaged},
	}
	project := &orcv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name: serviceAccountProjectRef(cp, sa), Namespace: ns,
			Finalizers: []string{"openstack.k-orc.cloud/project"},
		},
		Spec: orcv1alpha1.ProjectSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyManaged},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, user, project).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	res, err := r.reconcileDelete(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(BeNumerically(">", 0),
		"reconcileDelete must hold the finalizer while the managed User/Project are Terminating")

	// Both managed CRs were Deleted (Terminating behind their K-ORC finalizers).
	gotUser := &orcv1alpha1.User{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: user.Name, Namespace: ns}, gotUser)).To(Succeed())
	g.Expect(gotUser.DeletionTimestamp).NotTo(BeNil(), "the managed User must be Terminating")
	gotProject := &orcv1alpha1.Project{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: project.Name, Namespace: ns}, gotProject)).To(Succeed())
	g.Expect(gotProject.DeletionTimestamp).NotTo(BeNil(), "the managed Project must be Terminating")

	// The ControlPlane still carries its finalizer until they are gone.
	g.Expect(controllerutil.ContainsFinalizer(cp, controlPlaneORCFinalizer)).To(BeTrue())
}

// --- cross-namespace teardown (issue #646) ---

// namespaceTeardownScheme extends the K-ORC test scheme with the service-child
// and backing-service types the cross-namespace teardown deletes.
func namespaceTeardownScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := korcTestScheme(t)
	if err := keystonev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding keystone scheme: %v", err)
	}
	if err := horizonv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding horizon scheme: %v", err)
	}
	if err := mariadbv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding mariadb scheme: %v", err)
	}
	return s
}

// deletingNamespacedControlPlane returns a deleting ControlPlane that placed
// Keystone in an operator-owned namespace and Horizon in a pre-existing one.
func deletingNamespacedControlPlane(deletionAge time.Duration) *c5c3v1alpha1.ControlPlane {
	cp := deletingControlPlane(deletionAge)
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
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
	}
	return cp
}

// TestTeardownDedicatedNamespaces_NoAssignments verifies the default costs
// nothing: a ControlPlane with no service namespaces reports done at once.
func TestTeardownDedicatedNamespaces_NoAssignments(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespaceTeardownScheme(t)
	cp := deletingControlPlane(time.Minute)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	done, err := r.teardownDedicatedNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue())
}

// TestTeardownDedicatedNamespaces_WaitsForServiceChildren pins the ordering: the
// service children are deleted and WAITED on before anything else, because their
// own operators run a sequenced ESO cleanup through the tenant store in the same
// namespace — removing the store first would strand their key material in OpenBao.
func TestTeardownDedicatedNamespaces_WaitsForServiceChildren(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespaceTeardownScheme(t)
	cp := deletingNamespacedControlPlane(time.Minute)

	// A Keystone child held by its own cleanup finalizer, so the Delete leaves it
	// Terminating rather than gone.
	keystone := &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name: keystoneName(cp), Namespace: "identity",
			Finalizers: []string{"keystone.openstack.c5c3.io/cleanup"},
		},
	}
	stampControlPlaneChildLabels(keystone, cp)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "identity", Labels: controlPlaneChildLabels(cp),
	}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, keystone, ns).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	done, err := r.teardownDedicatedNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeFalse(), "the sweep must wait for the service child")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNamespacesReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("FinalizingNamespaces"))

	// The Keystone child was deleted (Terminating), and the namespace still stands:
	// deleting it now would cascade the child out from under its own cleanup.
	live := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: keystoneName(cp), Namespace: "identity",
	}, live)).To(Succeed())
	g.Expect(live.DeletionTimestamp).NotTo(BeNil())
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "identity"}, &corev1.Namespace{})).To(Succeed())
}

// TestTeardownDedicatedNamespaces_DeletesTheManagedNamespace verifies a Managed
// namespace is deleted once its children are gone — that is the whole point of
// the Managed lifecycle, and the namespace delete cascades whatever is left in it.
func TestTeardownDedicatedNamespaces_DeletesTheManagedNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespaceTeardownScheme(t)
	cp := deletingNamespacedControlPlane(time.Minute)
	cp.Spec.Services.Horizon = nil

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "identity", Labels: controlPlaneChildLabels(cp),
	}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ns).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	done, err := r.teardownDedicatedNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue())

	err = c.Get(context.Background(), types.NamespacedName{Name: "identity"}, &corev1.Namespace{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a Managed namespace must be deleted with the ControlPlane")
}

// TestTeardownDedicatedNamespaces_RefusesToDeleteAnUnownedNamespace is the guard
// that matters most on the way out: a namespace carrying no ownership labels was
// not created by us, so deleting it would destroy every workload in it. It is left
// standing and the operator is warned.
func TestTeardownDedicatedNamespaces_RefusesToDeleteAnUnownedNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespaceTeardownScheme(t)
	cp := deletingNamespacedControlPlane(time.Minute)
	cp.Spec.Services.Horizon = nil

	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "identity", Labels: map[string]string{"team": "platform"},
	}}
	rec := record.NewFakeRecorder(10)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, foreign).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	done, err := r.teardownDedicatedNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue())

	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "identity"}, &corev1.Namespace{})).
		To(Succeed(), "a namespace we did not create must never be deleted")
	g.Expect(strings.Join(drainEvents(rec), "\n")).To(ContainSubstring("NamespaceNotOwned"))
}

// TestTeardownDedicatedNamespaces_SweepsExternalNamespaceResidue verifies the
// External lifecycle: the namespace survives, so nothing cascades and every object
// the ControlPlane placed there has to be named and deleted — while a same-named
// object belonging to somebody else in that shared namespace is left alone.
func TestTeardownDedicatedNamespaces_SweepsExternalNamespaceResidue(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespaceTeardownScheme(t)

	// Keystone in the External namespace, so its credential material lands there.
	cp := deletingControlPlane(time.Minute)
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
				Name:      "shared-ns",
				Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
			},
		},
	}

	ours := &esov1.SecretStore{ObjectMeta: metav1.ObjectMeta{
		Name: esoTenantStoreName, Namespace: "shared-ns", Labels: controlPlaneChildLabels(cp),
	}}
	adminPw := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Name: adminPasswordSecretName(cp), Namespace: "shared-ns", Labels: controlPlaneChildLabels(cp),
	}}
	// Somebody else's ServiceAccount of the same fixed name in the shared namespace.
	foreignSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: esoTenantServiceAccountName, Namespace: "shared-ns",
	}}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shared-ns"}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ns, ours, adminPw, foreignSA).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	done, err := r.teardownDedicatedNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue())

	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "shared-ns"}, &corev1.Namespace{})).
		To(Succeed(), "an External namespace must survive the ControlPlane")

	err = c.Get(context.Background(), types.NamespacedName{
		Name: esoTenantStoreName, Namespace: "shared-ns",
	}, &esov1.SecretStore{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "our tenant store must be swept")

	err = c.Get(context.Background(), types.NamespacedName{
		Name: adminPasswordSecretName(cp), Namespace: "shared-ns",
	}, &esov1.ExternalSecret{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "our credential material must be swept")

	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: esoTenantServiceAccountName, Namespace: "shared-ns",
	}, &corev1.ServiceAccount{})).To(Succeed(),
		"an object we do not own must survive, even under a name we also use")
}

// TestTeardownDedicatedNamespaces_StallEscape verifies the bounded escape: past the
// stall window a child that will not go must not make the namespace undeletable
// forever. The sweep warns, names what it left behind, and releases.
func TestTeardownDedicatedNamespaces_StallEscape(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespaceTeardownScheme(t)
	cp := deletingNamespacedControlPlane(orcTeardownStallTimeout + time.Minute)
	cp.Spec.Services.Horizon = nil

	wedged := &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name: keystoneName(cp), Namespace: "identity",
			Finalizers: []string{"keystone.openstack.c5c3.io/cleanup"},
		},
	}
	stampControlPlaneChildLabels(wedged, cp)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "identity", Labels: controlPlaneChildLabels(cp),
	}}
	rec := record.NewFakeRecorder(10)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, wedged, ns).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: rec}

	done, err := r.teardownDedicatedNamespaces(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue(), "the stall escape must release rather than wedge forever")

	events := strings.Join(drainEvents(rec), "\n")
	g.Expect(events).To(ContainSubstring("NamespaceTeardownStalled"))
	g.Expect(events).To(ContainSubstring("identity/" + keystoneName(cp)))
}
