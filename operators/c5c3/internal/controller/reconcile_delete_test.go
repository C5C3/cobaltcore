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
	"slices"
	"strings"
	"testing"
	"time"

	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
	placementv1alpha1 "github.com/c5c3/forge/operators/placement/api/v1alpha1"
	openbaov1alpha1 "github.com/dc-tec/openbao-operator/api/v1alpha1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/forge/internal/common/conditions"
	commonmulticluster "github.com/c5c3/forge/internal/common/multicluster"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	barbicanv1alpha1 "github.com/c5c3/forge/operators/barbican/api/v1alpha1"
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

// placingControlPlane returns a fresh (not deleting) ControlPlane that places its
// Keystone in a namespace of its own on the named target cluster, which is what
// makes it a candidate for the remote-children finalizer.
func placingControlPlane(targetCluster string) *c5c3v1alpha1.ControlPlane {
	cp := korcControlPlane()
	cp.Spec.Services.Keystone = &c5c3v1alpha1.ServiceKeystoneSpec{
		Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
			Name: placedTeardownNamespace, Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
		},
		TargetClusterRef: &commonv1.TargetClusterRefSpec{Name: targetCluster},
	}
	return cp
}

// TestReconcile_RemoteChildrenFinalizerInstall pins the gate on the finalizer that
// holds a ControlPlane in etcd until the namespaces it placed on a target cluster
// are swept: it goes on a ControlPlane that places a service on a cluster that
// resolves, and on no other. A cluster that does not resolve is not an error here
// — nothing has been written to it that the finalizer would have to reclaim, and
// reconcileNamespaces reports the failure — so the install is retried on a later
// pass instead.
func TestReconcile_RemoteChildrenFinalizerInstall(t *testing.T) {
	// reconcileTwice runs the passes that install the ORC finalizer and, when the
	// gate admits it, the remote-children one, and returns the persisted CR.
	reconcileTwice := func(g *WithT, r *ControlPlaneReconciler, c client.Client, cp *c5c3v1alpha1.ControlPlane) *c5c3v1alpha1.ControlPlane {
		key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
		for range 2 {
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			g.Expect(err).NotTo(HaveOccurred(), "installing a finalizer must not fail the pass")
		}
		got := &c5c3v1alpha1.ControlPlane{}
		g.Expect(c.Get(context.Background(), key, got)).To(Succeed())
		return got
	}

	t.Run("installs it for a placed ControlPlane whose cluster resolves", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := controllerTestScheme(t)
		cp := placingControlPlane(placedTeardownCluster)
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
			WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}).Build()
		r := &ControlPlaneReconciler{
			Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10),
			Resolver: &childrenResolver{children: fake.NewClientBuilder().WithScheme(s).Build()},
		}

		got := reconcileTwice(g, r, c, cp)
		g.Expect(controllerutil.ContainsFinalizer(got, commonmulticluster.RemoteChildrenFinalizer)).To(BeTrue(),
			"a ControlPlane that places a service on a resolvable cluster must carry the remote-children finalizer")
	})

	t.Run("never installs it for a ControlPlane that places nothing", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := controllerTestScheme(t)
		cp := korcControlPlane()
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
			WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}).Build()
		r := &ControlPlaneReconciler{
			Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10),
			Resolver: &childrenResolver{children: fake.NewClientBuilder().WithScheme(s).Build()},
		}

		got := reconcileTwice(g, r, c, cp)
		g.Expect(controllerutil.ContainsFinalizer(got, commonmulticluster.RemoteChildrenFinalizer)).To(BeFalse(),
			"a ControlPlane whose children all stay at home has nothing for the finalizer to hold")
	})

	t.Run("skips the install while the cluster does not resolve", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := controllerTestScheme(t)
		cp := placingControlPlane(placedTeardownCluster)
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
			WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}).Build()
		r := &ControlPlaneReconciler{
			Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10),
			Resolver: &childrenResolver{err: mcruntime.ErrClusterNotFound},
		}

		got := reconcileTwice(g, r, c, cp)
		g.Expect(controllerutil.ContainsFinalizer(got, commonmulticluster.RemoteChildrenFinalizer)).To(BeFalse(),
			"an unwritten cluster leaves nothing to reclaim, so the install waits")
		cond := conditions.GetCondition(got.Status.Conditions, conditionTypeNamespacesReady)
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable),
			"the unresolvable cluster is reported by the namespace step, not by the finalizer install")
	})

	// ANY, not ALL. reconcileNamespaces resolves and writes per namespace inside
	// its loop, so the resolvable cluster's namespaces are created on this very
	// pass whatever the sibling ref does. Demanding every cluster would leave that
	// written half without a finalizer, and the ORC stall escape — which releases
	// the ORC finalizer expecting this one to hold the CR open — would then let the
	// CR leave etcd with those namespaces standing.
	t.Run("installs it when only one of two clusters resolves", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := controllerTestScheme(t)
		cp := placingControlPlane(placedTeardownCluster)
		cp.Spec.Services.Horizon = &c5c3v1alpha1.ServiceHorizonSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
				Name: "dashboard", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
			},
			TargetClusterRef: &commonv1.TargetClusterRefSpec{Name: "deregistered"},
		}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
			WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}).Build()
		r := &ControlPlaneReconciler{
			Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10),
			Resolver: &childrenResolver{
				children: fake.NewClientBuilder().WithScheme(s).Build(),
				errNames: map[string]error{"deregistered": mcruntime.ErrClusterNotFound},
			},
		}

		got := reconcileTwice(g, r, c, cp)
		g.Expect(controllerutil.ContainsFinalizer(got, commonmulticluster.RemoteChildrenFinalizer)).To(BeTrue(),
			"the namespaces written to the resolvable cluster need the finalizer that reclaims them")
	})

	t.Run("keeps it once installed, even when the cluster stops resolving", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := controllerTestScheme(t)
		cp := placingControlPlane(placedTeardownCluster)
		cp.Finalizers = []string{controlPlaneORCFinalizer, commonmulticluster.RemoteChildrenFinalizer}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
			WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}).Build()
		r := &ControlPlaneReconciler{
			Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10),
			Resolver: &childrenResolver{err: mcruntime.ErrClusterNotFound},
		}

		got := reconcileTwice(g, r, c, cp)
		g.Expect(controllerutil.ContainsFinalizer(got, commonmulticluster.RemoteChildrenFinalizer)).To(BeTrue(),
			"children already on a cluster that dropped out still have to be swept or abandoned")
	})
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

// TestDeleteOwnedPushSecrets_SweepsLabelOwnedInDedicatedNamespace pins the widened
// teardown: a service-account credential PushSecret delivered into a dedicated
// service namespace carries the ownership LABELS, not a controller reference, and
// must still get its DeletionPolicy=Delete OpenBao purge while that namespace's
// tenant store is alive. deleteOwnedPushSecrets therefore sweeps every namespace
// the ControlPlane occupies and matches isControlPlaneChild — while a same-named
// foreign PushSecret in that shared namespace is left alone.
func TestDeleteOwnedPushSecrets_SweepsLabelOwnedInDedicatedNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	s := korcTestScheme(t)

	cp := deletingNamespacedControlPlane(time.Second) // places Keystone in "identity"
	homeNS := childNamespace(cp)

	// A label-owned PushSecret in the dedicated namespace, held by ESO's finalizer.
	delivered := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cp-service-account-nova-backup", Namespace: "identity",
			Labels:     controlPlaneChildLabels(cp),
			Finalizers: []string{"pushsecret.externalsecrets.io/finalizer"},
		},
	}
	// A finalizer-less owned PushSecret at home — gone with the Delete.
	home := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name: adminAppCredentialPushSecretName(cp), Namespace: homeNS, OwnerReferences: ownedByCP(cp),
		},
	}
	// Somebody else's PushSecret of the same name in the shared namespace.
	foreign := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-service-account-nova-backup", Namespace: "dashboard"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, delivered, home, foreign).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	remaining, err := r.deleteOwnedPushSecrets(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	// The delivery-namespace PushSecret was Deleted and, held by its finalizer, is
	// still present — so it is reported as remaining and gates the finalizer release.
	gotDelivered := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(delivered), gotDelivered)).To(Succeed())
	g.Expect(gotDelivered.DeletionTimestamp.IsZero()).To(BeFalse(),
		"the label-owned delivery PushSecret must be deleted, not left to a GC cascade that never reaches it")
	names := make([]string, 0, len(remaining))
	for _, pending := range remaining {
		names = append(names, pending.pushSecret.Namespace+"/"+pending.pushSecret.Name)
	}
	g.Expect(names).To(ContainElement("identity/cp-service-account-nova-backup"))

	// The finalizer-less owned PushSecret at home is gone.
	err = c.Get(context.Background(), client.ObjectKeyFromObject(home), &esov1alpha1.PushSecret{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the owned home PushSecret must be swept too")

	// The foreign PushSecret is untouched.
	gotForeign := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(foreign), gotForeign)).To(Succeed())
	g.Expect(gotForeign.DeletionTimestamp.IsZero()).To(BeTrue(),
		"a same-named PushSecret we do not own must be left alone")
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
	g.Expect(children).To(HaveLen(5+len(externalCatalogInterfaces)+2+3+3+3),
		"the sweep must enumerate the catalog imports, the declared entry, and the three preserved-orphan "+
			"image-catalog, placement-catalog, and key-manager-catalog names (none of those services is set in "+
			"External mode)")
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
// sweep: a Managed ControlPlane with no image, no placement, and no key-manager
// service enumerates exactly the five identity/admin CRs it always did, plus the
// three preserved-orphan catalog names each of those teardowns adds
// unconditionally (all NotFound-tolerated when the service was never set) — and
// nothing more, so neither the External-mode nor the per-service additions widen
// the managed blast radius.
func TestOrcChildObjects_ManagedModeUnchanged(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := korcControlPlane() // services.glance, .placement, and .barbican unset
	children := orcChildObjects(cp)

	g.Expect(children).To(HaveLen(14))
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
		glanceCatalogServiceName(cp),
		glanceCatalogEndpointName(cp, "internal"),
		glanceCatalogEndpointName(cp, "public"),
		placementCatalogServiceName(cp),
		placementCatalogEndpointName(cp, "internal"),
		placementCatalogEndpointName(cp, "public"),
		barbicanCatalogServiceName(cp),
		barbicanCatalogEndpointName(cp, "internal"),
		barbicanCatalogEndpointName(cp, "public"),
	))
}

// TestOrcChildObjects_GlanceCatalogEnumeration pins the image-catalog teardown
// names: they are enumerated via the preserved-orphan cover when services.glance
// is unset (a row left behind by a glance removal without the opt-in is still torn
// down with the ControlPlane) and via the managedCatalogRows row when it is set —
// exactly once either way, since naming a CR twice would make the stall escape
// Update the same object off two stale reads.
func TestOrcChildObjects_GlanceCatalogEnumeration(t *testing.T) {
	glanceCatalogNames := func(cp *c5c3v1alpha1.ControlPlane) []string {
		return []string{
			glanceCatalogServiceName(cp),
			glanceCatalogEndpointName(cp, "internal"),
			glanceCatalogEndpointName(cp, "public"),
		}
	}
	countNames := func(cp *c5c3v1alpha1.ControlPlane) map[string]int {
		counts := map[string]int{}
		for _, child := range orcChildObjects(cp) {
			counts[child.name]++
		}
		return counts
	}

	t.Run("unset enumerates the preserved-orphan names", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := korcControlPlane() // services.glance unset
		counts := countNames(cp)
		for _, name := range glanceCatalogNames(cp) {
			g.Expect(counts[name]).To(Equal(1), "preserved-orphan catalog CR %q must be named exactly once", name)
		}
	})

	t.Run("set enumerates them once via the catalog row", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := korcControlPlane()
		cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{}
		counts := countNames(cp)
		for _, name := range glanceCatalogNames(cp) {
			g.Expect(counts[name]).To(Equal(1),
				"a glance-set ControlPlane must name catalog CR %q via the row alone, not also the preserved-orphan cover", name)
		}
	})
}

// TestOrcChildObjects_PlacementCatalogEnumeration pins the placement-catalog
// teardown names on the same terms as the image ones: enumerated via the
// preserved-orphan cover when services.placement is unset (a row left behind by a
// placement removal without the opt-in is still torn down with the ControlPlane)
// and via the managedCatalogRows row when it is set — exactly once either way,
// since naming a CR twice would make the stall escape Update the same object off
// two stale reads.
func TestOrcChildObjects_PlacementCatalogEnumeration(t *testing.T) {
	placementCatalogNames := func(cp *c5c3v1alpha1.ControlPlane) []string {
		return []string{
			placementCatalogServiceName(cp),
			placementCatalogEndpointName(cp, "internal"),
			placementCatalogEndpointName(cp, "public"),
		}
	}
	countNames := func(cp *c5c3v1alpha1.ControlPlane) map[string]int {
		counts := map[string]int{}
		for _, child := range orcChildObjects(cp) {
			counts[child.name]++
		}
		return counts
	}

	t.Run("unset enumerates the preserved-orphan names", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := korcControlPlane() // services.placement unset
		counts := countNames(cp)
		for _, name := range placementCatalogNames(cp) {
			g.Expect(counts[name]).To(Equal(1), "preserved-orphan catalog CR %q must be named exactly once", name)
		}
	})

	t.Run("set enumerates them once via the catalog row", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := korcControlPlane()
		cp.Spec.Services.Placement = &c5c3v1alpha1.ServicePlacementSpec{}
		counts := countNames(cp)
		for _, name := range placementCatalogNames(cp) {
			g.Expect(counts[name]).To(Equal(1),
				"a placement-set ControlPlane must name catalog CR %q via the row alone, "+
					"not also the preserved-orphan cover", name)
		}
	})
}

// TestOrcChildObjects_BarbicanCatalogEnumeration pins the key-manager-catalog
// teardown names on the same terms as the image and placement ones: enumerated via
// the preserved-orphan cover when services.barbican is unset (a row left behind by
// a barbican removal without the opt-in is still torn down with the ControlPlane)
// and via the managedCatalogRows row when it is set — exactly once either way,
// since naming a CR twice would make the stall escape Update the same object off
// two stale reads.
func TestOrcChildObjects_BarbicanCatalogEnumeration(t *testing.T) {
	barbicanCatalogNames := func(cp *c5c3v1alpha1.ControlPlane) []string {
		return []string{
			barbicanCatalogServiceName(cp),
			barbicanCatalogEndpointName(cp, "internal"),
			barbicanCatalogEndpointName(cp, "public"),
		}
	}
	countNames := func(cp *c5c3v1alpha1.ControlPlane) map[string]int {
		counts := map[string]int{}
		for _, child := range orcChildObjects(cp) {
			counts[child.name]++
		}
		return counts
	}

	t.Run("unset enumerates the preserved-orphan names", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := korcControlPlane() // services.barbican unset
		counts := countNames(cp)
		for _, name := range barbicanCatalogNames(cp) {
			g.Expect(counts[name]).To(Equal(1), "preserved-orphan catalog CR %q must be named exactly once", name)
		}
	})

	t.Run("set enumerates them once via the catalog row", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := korcControlPlane()
		cp.Spec.Services.Barbican = &c5c3v1alpha1.ServiceBarbicanSpec{}
		counts := countNames(cp)
		for _, name := range barbicanCatalogNames(cp) {
			g.Expect(counts[name]).To(Equal(1),
				"a barbican-set ControlPlane must name catalog CR %q via the row alone, "+
					"not also the preserved-orphan cover", name)
		}
	})
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
	if err := glancev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding glance scheme: %v", err)
	}
	if err := placementv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding placement scheme: %v", err)
	}
	if err := barbicanv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding barbican scheme: %v", err)
	}
	if err := openbaov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding openbao scheme: %v", err)
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

// TestCrossNamespaceServiceChildren_IncludesGlance verifies the Glance child is
// enumerated for the namespace it was assigned to, and excluded from any other —
// so a Glance placed in a namespace of its own is torn down by the finalizer sweep
// (which carries no owner reference to garbage-collect it), while a namespace it
// was never placed in never names it.
func TestCrossNamespaceServiceChildren_IncludesGlance(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()
	cp.Spec.Services.Glance = &c5c3v1alpha1.ServiceGlanceSpec{
		Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
			Name: "images", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
		},
	}

	hasGlance := func(namespace string) bool {
		for _, child := range crossNamespaceServiceChildren(cp, namespace) {
			if _, ok := child.(*glancev1alpha1.Glance); ok && child.GetName() == glanceName(cp) {
				return true
			}
		}
		return false
	}

	g.Expect(hasGlance("images")).To(BeTrue(), "the Glance child is enumerated for its assigned namespace")
	g.Expect(hasGlance("unrelated")).To(BeFalse(), "a namespace Glance was not placed in must not name it")
}

// TestCrossNamespaceServiceChildren_IncludesPlacement is the same guard for the
// Placement child: it is enumerated for the namespace it was assigned to and
// excluded from any other, so a Placement placed in a namespace of its own is torn
// down by the finalizer sweep (it carries no owner reference to garbage-collect
// it).
func TestCrossNamespaceServiceChildren_IncludesPlacement(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()
	cp.Spec.Services.Placement = &c5c3v1alpha1.ServicePlacementSpec{
		Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
			Name: "compute", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
		},
	}

	hasPlacement := func(namespace string) bool {
		for _, child := range crossNamespaceServiceChildren(cp, namespace) {
			if _, ok := child.(*placementv1alpha1.Placement); ok && child.GetName() == placementName(cp) {
				return true
			}
		}
		return false
	}

	g.Expect(hasPlacement("compute")).To(BeTrue(), "the Placement child is enumerated for its assigned namespace")
	g.Expect(hasPlacement("unrelated")).To(BeFalse(), "a namespace Placement was not placed in must not name it")
}

// TestDeleteServiceChildrenIn_SweepsOwnedGlanceBackends verifies the cross-namespace
// teardown reaps the projected GlanceBackend children the ControlPlane placed in a
// dedicated namespace: a c5c3-owned backend carrying the glance child's name prefix
// is deleted and reported as remaining (so the sweep waits for it), while a
// hand-created backend that merely shares the namespace is left untouched.
func TestDeleteServiceChildrenIn_SweepsOwnedGlanceBackends(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespaceTeardownScheme(t)

	cp := deletingControlPlane(time.Minute)
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Glance: &c5c3v1alpha1.ServiceGlanceSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
				Name: "images", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
			},
		},
	}

	// A projected, label-owned backend carrying the glance child's name prefix (a
	// cross-namespace child cannot carry an owner reference).
	owned := &glancev1alpha1.GlanceBackend{
		ObjectMeta: metav1.ObjectMeta{
			Name: glanceBackendName(cp, "primary"), Namespace: "images",
			Labels: controlPlaneChildLabels(cp),
		},
	}
	// A hand-created backend sharing the namespace and the prefix but owned by nobody.
	foreign := &glancev1alpha1.GlanceBackend{
		ObjectMeta: metav1.ObjectMeta{Name: glanceBackendName(cp, "byo"), Namespace: "images"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, owned, foreign).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	remaining, err := r.deleteServiceChildrenIn(context.Background(), cp, "images")
	g.Expect(err).NotTo(HaveOccurred())

	// The owned backend was deleted and reported as remaining so the sweep waits.
	g.Expect(remaining).To(ContainElement("images/" + glanceBackendName(cp, "primary")))
	err = c.Get(context.Background(), client.ObjectKeyFromObject(owned), &glancev1alpha1.GlanceBackend{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the owned projected backend must be swept")

	// The foreign backend is untouched and never reported as remaining.
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(foreign), &glancev1alpha1.GlanceBackend{})).
		To(Succeed(), "a hand-created backend we do not own must never be swept")
	g.Expect(remaining).NotTo(ContainElement("images/" + glanceBackendName(cp, "byo")))
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

	// Keystone, Glance, and Placement in the External namespace, so their credential
	// material lands there.
	cp := deletingControlPlane(time.Minute)
	cp.Spec.Services = c5c3v1alpha1.ServicesSpec{
		Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
				Name:      "shared-ns",
				Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
			},
		},
		Glance: &c5c3v1alpha1.ServiceGlanceSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
				Name:      "shared-ns",
				Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
			},
		},
		Placement: &c5c3v1alpha1.ServicePlacementSpec{
			Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
				Name:      "shared-ns",
				Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
			},
		},
	}
	// A service account delivering its credentials into that same External namespace:
	// its source Secret and consumer ExternalSecret are label-owned residue that must
	// be swept too.
	cp.Spec.KORC.ServiceAccounts = []c5c3v1alpha1.ServiceAccountSpec{{
		Name:            "nova",
		Project:         c5c3v1alpha1.ServiceAccountProjectSpec{Name: "service"},
		TargetNamespace: "shared-ns",
	}}
	sa := cp.Spec.KORC.ServiceAccounts[0]

	ours := &esov1.SecretStore{ObjectMeta: metav1.ObjectMeta{
		Name: esoTenantStoreName, Namespace: "shared-ns", Labels: controlPlaneChildLabels(cp),
	}}
	adminPw := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Name: adminPasswordSecretName(cp), Namespace: "shared-ns", Labels: controlPlaneChildLabels(cp),
	}}
	glanceDB := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Name: glanceDBCredentialSecretName(cp), Namespace: "shared-ns", Labels: controlPlaneChildLabels(cp),
	}}
	// The Dynamic-mode Glance DB-credential objects: the generator, its mTLS client
	// Certificate, and the generator's ServiceAccount are label-owned residue too.
	glanceDBVDS := &esgenv1alpha1.VaultDynamicSecret{ObjectMeta: metav1.ObjectMeta{
		Name: glanceDBCredentialSecretName(cp), Namespace: "shared-ns", Labels: controlPlaneChildLabels(cp),
	}}
	glanceDBCert := &unstructured.Unstructured{}
	glanceDBCert.SetGroupVersionKind(certificateGVK)
	glanceDBCert.SetName(glanceDBCredentialClientCertName(cp))
	glanceDBCert.SetNamespace("shared-ns")
	glanceDBCert.SetLabels(controlPlaneChildLabels(cp))
	glanceDBSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: glanceDBCredentialServiceAccountName, Namespace: "shared-ns", Labels: controlPlaneChildLabels(cp),
	}}
	// The Placement credential material in the same four shapes.
	placementDB := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Name: placementDBCredentialSecretName(cp), Namespace: "shared-ns", Labels: controlPlaneChildLabels(cp),
	}}
	placementDBVDS := &esgenv1alpha1.VaultDynamicSecret{ObjectMeta: metav1.ObjectMeta{
		Name: placementDBCredentialSecretName(cp), Namespace: "shared-ns", Labels: controlPlaneChildLabels(cp),
	}}
	placementDBCert := &unstructured.Unstructured{}
	placementDBCert.SetGroupVersionKind(certificateGVK)
	placementDBCert.SetName(placementDBCredentialClientCertName(cp))
	placementDBCert.SetNamespace("shared-ns")
	placementDBCert.SetLabels(controlPlaneChildLabels(cp))
	placementDBSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: placementDBCredentialServiceAccountName, Namespace: "shared-ns", Labels: controlPlaneChildLabels(cp),
	}}
	saSource := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: serviceAccountSourceSecretName(cp, sa), Namespace: "shared-ns", Labels: controlPlaneChildLabels(cp),
	}}
	saES := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Name: serviceAccountCredentialsSecretName(cp, sa), Namespace: "shared-ns", Labels: controlPlaneChildLabels(cp),
	}}
	// Somebody else's ServiceAccount of the same fixed name in the shared namespace.
	foreignSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: esoTenantServiceAccountName, Namespace: "shared-ns",
	}}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shared-ns"}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		cp, ns, ours, adminPw, glanceDB, glanceDBVDS, glanceDBCert, glanceDBSA,
		placementDB, placementDBVDS, placementDBCert, placementDBSA, saSource, saES, foreignSA,
	).Build()
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
		Name: serviceAccountSourceSecretName(cp, sa), Namespace: "shared-ns",
	}, &corev1.Secret{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the service-account source Secret must be swept")

	err = c.Get(context.Background(), types.NamespacedName{
		Name: serviceAccountCredentialsSecretName(cp, sa), Namespace: "shared-ns",
	}, &esov1.ExternalSecret{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the service-account consumer ExternalSecret must be swept")

	err = c.Get(context.Background(), types.NamespacedName{
		Name: adminPasswordSecretName(cp), Namespace: "shared-ns",
	}, &esov1.ExternalSecret{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "our credential material must be swept")

	err = c.Get(context.Background(), types.NamespacedName{
		Name: glanceDBCredentialSecretName(cp), Namespace: "shared-ns",
	}, &esov1.ExternalSecret{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the Glance DB-credential ExternalSecret must be swept")

	err = c.Get(context.Background(), types.NamespacedName{
		Name: glanceDBCredentialSecretName(cp), Namespace: "shared-ns",
	}, &esgenv1alpha1.VaultDynamicSecret{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the Glance VaultDynamicSecret generator must be swept")

	sweptGlanceCert := &unstructured.Unstructured{}
	sweptGlanceCert.SetGroupVersionKind(certificateGVK)
	err = c.Get(context.Background(), types.NamespacedName{
		Name: glanceDBCredentialClientCertName(cp), Namespace: "shared-ns",
	}, sweptGlanceCert)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the Glance DB-credential Certificate must be swept")

	err = c.Get(context.Background(), types.NamespacedName{
		Name: glanceDBCredentialServiceAccountName, Namespace: "shared-ns",
	}, &corev1.ServiceAccount{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the Glance DB-credential ServiceAccount must be swept")

	err = c.Get(context.Background(), types.NamespacedName{
		Name: placementDBCredentialSecretName(cp), Namespace: "shared-ns",
	}, &esov1.ExternalSecret{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the Placement DB-credential ExternalSecret must be swept")

	err = c.Get(context.Background(), types.NamespacedName{
		Name: placementDBCredentialSecretName(cp), Namespace: "shared-ns",
	}, &esgenv1alpha1.VaultDynamicSecret{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the Placement VaultDynamicSecret generator must be swept")

	sweptPlacementCert := &unstructured.Unstructured{}
	sweptPlacementCert.SetGroupVersionKind(certificateGVK)
	err = c.Get(context.Background(), types.NamespacedName{
		Name: placementDBCredentialClientCertName(cp), Namespace: "shared-ns",
	}, sweptPlacementCert)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the Placement DB-credential Certificate must be swept")

	err = c.Get(context.Background(), types.NamespacedName{
		Name: placementDBCredentialServiceAccountName, Namespace: "shared-ns",
	}, &corev1.ServiceAccount{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the Placement DB-credential ServiceAccount must be swept")

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

// --- barbican ensemble teardown ---

// barbicanTeardownNamespace is the namespace the fixtures below assign to the
// Barbican service, so its child, its secret store, and the dedicated OpenBao
// ensemble all land outside the ControlPlane's own namespace.
const barbicanTeardownNamespace = "keymanager"

// deletingBarbicanControlPlane returns a deleting ControlPlane whose Barbican
// service takes a dedicated secret store in a namespace of its own, under the
// given lifecycle.
func deletingBarbicanControlPlane(
	deletionAge time.Duration, lifecycle c5c3v1alpha1.ServiceNamespaceLifecycle,
) *c5c3v1alpha1.ControlPlane {
	cp := deletingControlPlane(deletionAge)
	cp.Spec.Services.Barbican = &c5c3v1alpha1.ServiceBarbicanSpec{
		Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
			Name: barbicanTeardownNamespace, Lifecycle: lifecycle,
		},
		SecretStore: c5c3v1alpha1.ServiceBarbicanSecretStoreSpec{
			Dedicated: &c5c3v1alpha1.BarbicanDedicatedSecretStoreSpec{},
		},
	}
	return cp
}

// ownedBarbicanCertificate builds one of the ensemble's cert-manager Certificates
// the way the projection leaves it: unstructured (no Go type ships for them) and
// carrying the ownership labels a cross-namespace child takes instead of an owner
// reference.
func ownedBarbicanCertificate(cp *c5c3v1alpha1.ControlPlane, name string) *unstructured.Unstructured {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName(name)
	cert.SetNamespace(cp.BarbicanNamespace())
	cert.SetLabels(controlPlaneChildLabels(cp))
	return cert
}

// ownedBarbicanEnsemble returns the label-owned ensemble objects that outlive the
// dedicated OpenBao instance: the tenant admitting the namespace, both transport
// Certificates, the provisioner account, the TokenRequest grant, the static-seal
// Secret, and the cluster-scoped auth-delegator binding.
func ownedBarbicanEnsemble(cp *c5c3v1alpha1.ControlPlane) []client.Object {
	ns, name := cp.BarbicanNamespace(), barbicanOpenBaoName(cp)
	return []client.Object{
		&openbaov1alpha1.OpenBaoTenant{
			ObjectMeta: metav1.ObjectMeta{
				Name: name + barbicanOpenBaoTenantSuffix, Namespace: ns, Labels: controlPlaneChildLabels(cp),
			},
			Spec: openbaov1alpha1.OpenBaoTenantSpec{TargetNamespace: ns},
		},
		ownedBarbicanCertificate(cp, name+barbicanOpenBaoServerCertSuffix),
		ownedBarbicanCertificate(cp, name+barbicanOpenBaoCACertSuffix),
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: name + barbicanOpenBaoProvisionerSuffix, Namespace: ns, Labels: controlPlaneChildLabels(cp),
		}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{
			Name: name + barbicanOpenBaoTokenGrantSuffix, Namespace: ns, Labels: controlPlaneChildLabels(cp),
		}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
			Name: name + barbicanOpenBaoTokenGrantSuffix, Namespace: ns, Labels: controlPlaneChildLabels(cp),
		}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: name + barbicanOpenBaoUnsealSecretSuffix, Namespace: ns, Labels: controlPlaneChildLabels(cp),
		}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{
			Name: barbicanOpenBaoAuthDelegatorName(name, ns), Labels: controlPlaneChildLabels(cp),
		}},
	}
}

// expectSwept asserts every named object is gone from the cluster.
func expectSwept(t *testing.T, c client.Client, objs ...client.Object) {
	t.Helper()
	g := NewGomegaWithT(t)
	for _, obj := range objs {
		fresh := obj.DeepCopyObject().(client.Object)
		key := client.ObjectKeyFromObject(obj)
		g.Expect(apierrors.IsNotFound(c.Get(context.Background(), key, fresh))).
			To(BeTrue(), "%T %s must be swept", obj, key)
	}
}

// expectPresent asserts every named object is still in the cluster.
func expectPresent(t *testing.T, c client.Client, objs ...client.Object) {
	t.Helper()
	g := NewGomegaWithT(t)
	for _, obj := range objs {
		fresh := obj.DeepCopyObject().(client.Object)
		key := client.ObjectKeyFromObject(obj)
		g.Expect(c.Get(context.Background(), key, fresh)).
			To(Succeed(), "%T %s must be left alone", obj, key)
	}
}

// TestCrossNamespaceServiceChildren_IncludesBarbicanEnsemble pins the WAIT SET the
// Barbican namespace contributes: the child, its secret store, and the dedicated
// OpenBao instance. The instance has to be in there. The namespace must not be
// deleted until the openbao-operator has run the instance's finalizer, and that
// finalizer works through the tenant RBAC in this very namespace — deleting the
// namespace first reaps the RBAC mid-run and wedges it in Terminating.
func TestCrossNamespaceServiceChildren_IncludesBarbicanEnsemble(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := deletingBarbicanControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	// Keystone in a namespace of its own, so the per-namespace split is provable.
	cp.Spec.Services.Keystone = &c5c3v1alpha1.ServiceKeystoneSpec{
		Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
			Name: "identity", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
		},
	}

	children := crossNamespaceServiceChildren(cp, barbicanTeardownNamespace)
	g.Expect(children).To(HaveLen(3))
	g.Expect(children[0]).To(BeAssignableToTypeOf(&barbicanv1alpha1.Barbican{}))
	g.Expect(children[0].GetName()).To(Equal(barbicanName(cp)))
	g.Expect(children[1]).To(BeAssignableToTypeOf(&barbicanv1alpha1.BarbicanSecretStore{}))
	g.Expect(children[1].GetName()).To(Equal(barbicanSecretStoreName(cp)))
	g.Expect(children[2]).To(BeAssignableToTypeOf(&openbaov1alpha1.OpenBaoCluster{}))
	g.Expect(children[2].GetName()).To(Equal(barbicanOpenBaoName(cp)))

	// The other namespaces are unaffected: Keystone's names its own child alone, and
	// a namespace Barbican was never placed in names nothing.
	identity := crossNamespaceServiceChildren(cp, "identity")
	g.Expect(identity).To(HaveLen(1))
	g.Expect(identity[0]).To(BeAssignableToTypeOf(&keystonev1alpha1.Keystone{}))
	g.Expect(crossNamespaceServiceChildren(cp, "unrelated")).To(BeEmpty())
}

// TestDeleteServiceChildrenIn_BarbicanEnsembleOrdersTheTenantAfterTheInstance is
// the ordering guard on the ensemble sweep. While the instance is still finalizing
// the tenant that admitted the namespace stays put — deleting it first strips the
// RBAC the openbao-operator needs to finish, so the instance never goes and the
// namespace never becomes deletable. Everything the instance does not depend on
// comes down right away, including the cluster-scoped auth-delegator binding no
// namespace deletion would ever reclaim.
func TestDeleteServiceChildrenIn_BarbicanEnsembleOrdersTheTenantAfterTheInstance(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingBarbicanControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	ns := barbicanTeardownNamespace

	child := &barbicanv1alpha1.Barbican{ObjectMeta: metav1.ObjectMeta{
		Name: barbicanName(cp), Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}
	store := &barbicanv1alpha1.BarbicanSecretStore{ObjectMeta: metav1.ObjectMeta{
		Name: barbicanSecretStoreName(cp), Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}
	// The instance is held by the openbao-operator's finalizer, so the Delete leaves
	// it Terminating rather than gone — the state the tenant must outlive.
	instance := &openbaov1alpha1.OpenBaoCluster{ObjectMeta: metav1.ObjectMeta{
		Name: barbicanOpenBaoName(cp), Namespace: ns, Labels: controlPlaneChildLabels(cp),
		Finalizers: []string{openbaov1alpha1.OpenBaoClusterFinalizer},
	}}
	ensemble := ownedBarbicanEnsemble(cp)
	tenant, rest := ensemble[0], ensemble[1:]
	// The POSITIVE case of the undeclared-store sweep: an owned, prefix-matching
	// store nobody names any more, which a spec edit landing moments before the
	// delete leaves behind. Without it the sweep's Delete never runs in this suite.
	staleStore := &barbicanv1alpha1.BarbicanSecretStore{ObjectMeta: metav1.ObjectMeta{
		Name: barbicanName(cp) + "-stale", Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}

	objs := append([]client.Object{cp, child, store, instance, staleStore}, ensemble...)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	remaining, err := r.deleteServiceChildrenIn(ctx, cp, ns)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(remaining).To(ConsistOf(
		ns+"/"+barbicanName(cp),
		ns+"/"+barbicanSecretStoreName(cp),
		ns+"/"+barbicanOpenBaoName(cp),
		ns+"/"+barbicanName(cp)+"-stale",
	), "the child, the store, the instance, and the swept undeclared store gate the namespace deletion")
	expectSwept(t, c, staleStore)

	expectPresent(t, c, tenant)
	expectSwept(t, c, rest...)

	// The openbao-operator finishes: its finalizer goes and the instance leaves etcd.
	live := &openbaov1alpha1.OpenBaoCluster{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(instance), live)).To(Succeed())
	live.Finalizers = nil
	g.Expect(c.Update(ctx, live)).To(Succeed())

	remaining, err = r.deleteServiceChildrenIn(ctx, cp, ns)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(remaining).To(BeEmpty(), "nothing gates the namespace deletion once the instance is gone")
	expectSwept(t, c, tenant)
}

// TestDeleteServiceChildrenIn_BarbicanEnsembleToleratesAlreadyGoneObjects covers
// the edge path a re-run always takes: most of the ensemble was reclaimed by an
// earlier pass (or never projected, because the service took an external secret
// store), so every Get is a NotFound the sweep must swallow while it still reaps
// what IS there.
func TestDeleteServiceChildrenIn_BarbicanEnsembleToleratesAlreadyGoneObjects(t *testing.T) {
	g := NewGomegaWithT(t)
	s := namespaceTeardownScheme(t)

	cp := deletingBarbicanControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	ensemble := ownedBarbicanEnsemble(cp)
	// Only the static-seal Secret and the cluster-scoped binding survived.
	sealSecret, binding := ensemble[len(ensemble)-2], ensemble[len(ensemble)-1]

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, sealSecret, binding).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	remaining, err := r.deleteServiceChildrenIn(context.Background(), cp, barbicanTeardownNamespace)
	g.Expect(err).NotTo(HaveOccurred(), "an already-gone ensemble object must not fail the teardown")
	g.Expect(remaining).To(BeEmpty())
	expectSwept(t, c, sealSecret, binding)
}

// TestBarbicanTeardown_LeavesForeignEnsembleObjectsAlone is the blast-radius guard
// on the two objects the sweep could destroy for somebody else. The OpenBaoTenant
// admitting the namespace may predate this ControlPlane (in the kind stack the
// proving instance's tenant already admits it), and the auth-delegator binding is
// cluster-scoped, so a name collision reaches across the whole cluster. Neither is
// touched without the ownership labels — on either lifecycle path.
func TestBarbicanTeardown_LeavesForeignEnsembleObjectsAlone(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingBarbicanControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleExternal)
	ns, name := barbicanTeardownNamespace, barbicanOpenBaoName(cp)

	foreignTenant := &openbaov1alpha1.OpenBaoTenant{
		ObjectMeta: metav1.ObjectMeta{Name: name + barbicanOpenBaoTenantSuffix, Namespace: ns},
		Spec:       openbaov1alpha1.OpenBaoTenantSpec{TargetNamespace: ns},
	}
	foreignBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: barbicanOpenBaoAuthDelegatorName(name, ns)},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, foreignTenant, foreignBinding).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	_, err := r.deleteServiceChildrenIn(ctx, cp, ns)
	g.Expect(err).NotTo(HaveOccurred())
	expectPresent(t, c, foreignTenant, foreignBinding)

	r.sweepExternalNamespaceResidue(ctx, c, cp, ns)
	expectPresent(t, c, foreignTenant, foreignBinding)
}

// TestReconcileDelete_RemovesTheColocatedAuthDelegatorBinding closes the hole no
// namespace sweep can reach. With Barbican co-located in the ControlPlane's own
// namespace there is no dedicated namespace, so teardownDedicatedNamespaces returns
// at once and the ensemble sweep never runs; the binding is cluster-scoped, so the
// GC cascade behind the released finalizer cannot collect it either. Deleting the
// ControlPlane has to remove it by name, and a same-named binding belonging to
// somebody else has to survive that: the name is cluster-wide, so a collision is
// not confined to one namespace.
func TestReconcileDelete_RemovesTheColocatedAuthDelegatorBinding(t *testing.T) {
	// colocatedBarbicanControlPlane returns a deleting ControlPlane whose Barbican
	// service declares no namespace block, so it shares the ControlPlane's namespace.
	colocatedBarbicanControlPlane := func(g *WithT) *c5c3v1alpha1.ControlPlane {
		cp := deletingControlPlane(time.Minute)
		cp.Spec.Services.Barbican = &c5c3v1alpha1.ServiceBarbicanSpec{
			SecretStore: c5c3v1alpha1.ServiceBarbicanSecretStoreSpec{
				Dedicated: &c5c3v1alpha1.BarbicanDedicatedSecretStoreSpec{},
			},
		}
		g.Expect(cp.DedicatedServiceNamespaces()).To(BeEmpty(),
			"the fixture must be co-located, or the per-namespace sweep would cover the binding")
		return cp
	}

	t.Run("deletes the binding it owns", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		s := namespaceTeardownScheme(t)

		cp := colocatedBarbicanControlPlane(g)
		binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{
			Name:   barbicanOpenBaoAuthDelegatorName(barbicanOpenBaoName(cp), cp.BarbicanNamespace()),
			Labels: controlPlaneChildLabels(cp),
		}}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, binding).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

		key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
		g.Expect(c.Get(ctx, key, cp)).To(Succeed())

		res, err := r.reconcileDelete(ctx, cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res).To(Equal(ctrl.Result{}), "nothing gates this teardown, so it releases in one pass")
		g.Expect(apierrors.IsNotFound(c.Get(ctx, key, &c5c3v1alpha1.ControlPlane{}))).To(BeTrue())

		expectSwept(t, c, binding)
	})

	t.Run("leaves a foreign binding of the same name alone", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		s := namespaceTeardownScheme(t)

		cp := colocatedBarbicanControlPlane(g)
		foreign := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{
			Name: barbicanOpenBaoAuthDelegatorName(barbicanOpenBaoName(cp), cp.BarbicanNamespace()),
		}}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, foreign).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

		g.Expect(c.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, cp)).To(Succeed())

		_, err := r.reconcileDelete(ctx, cp)
		g.Expect(err).NotTo(HaveOccurred())

		expectPresent(t, c, foreign)
	})
}

// TestSweepExternalNamespaceResidue_RemovesTheBarbicanResidue covers the External
// lifecycle, where the namespace survives the ControlPlane so nothing cascades and
// every object has to be named: the Barbican child and its secret store, the whole
// dedicated OpenBao ensemble (the cluster-scoped auth-delegator binding included),
// and the DB-credential material.
func TestSweepExternalNamespaceResidue_RemovesTheBarbicanResidue(t *testing.T) {
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingBarbicanControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleExternal)
	ns := barbicanTeardownNamespace

	child := &barbicanv1alpha1.Barbican{ObjectMeta: metav1.ObjectMeta{
		Name: barbicanName(cp), Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}
	store := &barbicanv1alpha1.BarbicanSecretStore{ObjectMeta: metav1.ObjectMeta{
		Name: barbicanSecretStoreName(cp), Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}
	instance := &openbaov1alpha1.OpenBaoCluster{ObjectMeta: metav1.ObjectMeta{
		Name: barbicanOpenBaoName(cp), Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}
	// The DB-credential material, in the same four shapes as Glance's and
	// Placement's: the ExternalSecret, the Dynamic-mode generator, its mTLS client
	// Certificate, and the ServiceAccount whose token it authenticates with.
	dbES := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Name: barbicanDBCredentialSecretName(cp), Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}
	dbVDS := &esgenv1alpha1.VaultDynamicSecret{ObjectMeta: metav1.ObjectMeta{
		Name: barbicanDBCredentialSecretName(cp), Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}
	dbCert := ownedBarbicanCertificate(cp, barbicanDBCredentialClientCertName(cp))
	dbSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: barbicanDBCredentialServiceAccountName, Namespace: ns, Labels: controlPlaneChildLabels(cp),
	}}

	residue := append([]client.Object{child, store, instance, dbES, dbVDS, dbCert, dbSA},
		ownedBarbicanEnsemble(cp)...)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(append([]client.Object{cp}, residue...)...).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	r.sweepExternalNamespaceResidue(ctx, c, cp, ns)
	expectSwept(t, c, residue...)
}

// --- placed-namespace teardown ---

const (
	// placedTeardownNamespace is the namespace the fixtures below place Keystone
	// in, and placedTeardownCluster the target cluster that namespace lives on.
	placedTeardownNamespace = "identity"
	placedTeardownCluster   = "remote-a"
)

// deletingPlacedControlPlane returns a deleting ControlPlane that places its
// Keystone — and with it the namespace, the backing services, and the credential
// material scoped to that namespace — on a target cluster, under the given
// lifecycle.
func deletingPlacedControlPlane(
	deletionAge time.Duration, lifecycle c5c3v1alpha1.ServiceNamespaceLifecycle,
) *c5c3v1alpha1.ControlPlane {
	cp := deletingControlPlane(deletionAge)
	cp.Spec.Services.Keystone = &c5c3v1alpha1.ServiceKeystoneSpec{
		Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
			Name: placedTeardownNamespace, Lifecycle: lifecycle,
		},
		TargetClusterRef: &commonv1.TargetClusterRefSpec{Name: placedTeardownCluster},
	}
	cp.Finalizers = []string{controlPlaneORCFinalizer, commonmulticluster.RemoteChildrenFinalizer}
	return cp
}

// onTarget stamps obj with the labels a child written to a TARGET cluster carries
// — the owner triple the shared sweep selects on plus this operator's
// cross-namespace pair — the way claimChildOwnership leaves it. A remote child has
// no owner reference, so its labels are the whole of its identity.
func onTarget(cp *c5c3v1alpha1.ControlPlane, obj client.Object) client.Object {
	obj.SetLabels(remoteChildLabels(cp))
	return obj
}

// abandonImmediately compresses the abandon window to nothing for the duration of
// one test, so an unresolvable cluster is given up on in the first pass instead of
// after five minutes of wall clock. It is the package-level knob
// internal/common/multicluster documents for exactly this.
func abandonImmediately(t *testing.T) {
	t.Helper()
	previous := commonmulticluster.AbandonAfter
	commonmulticluster.AbandonAfter = 0
	t.Cleanup(func() { commonmulticluster.AbandonAfter = previous })
}

// placedNamespaceOnTarget builds the namespace reconcileNamespaces creates on a
// TARGET cluster: the ownership labels every remote child carries, plus the UID
// annotation that is the only mark distinguishing this ControlPlane from a
// same-named one owned by another management cluster.
func placedNamespaceOnTarget(cp *c5c3v1alpha1.ControlPlane, name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        name,
		Labels:      remoteChildLabels(cp),
		Annotations: map[string]string{controlPlaneUIDAnnotation: string(cp.UID)},
	}}
}

// TestTeardownDedicatedNamespaces_SweepsThePlacedNamespaceOnItsTarget is the
// central guard on the placed teardown: nothing on a target cluster collects what
// the ControlPlane wrote there — no owner reference and no garbage collection
// cascade crosses a cluster boundary — so every kind of
// controlPlaneRemoteChildKinds the ControlPlane owns in that namespace is deleted
// by name, on that cluster, and the Managed namespace goes with it on BOTH
// clusters, because reconcileNamespaces created it on both. An object in the same
// namespace that carries none of our labels is nobody's child and survives.
func TestTeardownDedicatedNamespaces_SweepsThePlacedNamespaceOnItsTarget(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingPlacedControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	ns := placedTeardownNamespace

	ours := []client.Object{
		onTarget(cp, &mariadbv1alpha1.MariaDB{ObjectMeta: metav1.ObjectMeta{Name: "keystone-db", Namespace: ns}}),
		onTarget(cp, &esov1.SecretStore{ObjectMeta: metav1.ObjectMeta{Name: esoTenantStoreName, Namespace: ns}}),
		onTarget(cp, &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
			Name: adminPasswordSecretName(cp), Namespace: ns,
		}}),
		onTarget(cp, &esgenv1alpha1.VaultDynamicSecret{ObjectMeta: metav1.ObjectMeta{
			Name: dbCredentialSecretName(cp), Namespace: ns,
		}}),
		onTarget(cp, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: esoTenantServiceAccountName, Namespace: ns,
		}}),
		onTarget(cp, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "keystone-admin", Namespace: ns}}),
	}
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "someone-elses", Namespace: ns}}
	remoteNS := placedNamespaceOnTarget(cp, ns)
	// The same namespace at home, where it carries the two cross-namespace labels a
	// local child is stamped with.
	localNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: controlPlaneChildLabels(cp)}}

	target := fake.NewClientBuilder().WithScheme(s).
		WithObjects(append(append([]client.Object{}, ours...), foreign, remoteNS)...).Build()
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, localNS).Build()
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: record.NewFakeRecorder(10),
		Resolver: &childrenResolver{children: target},
	}

	done, err := r.teardownDedicatedNamespaces(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue(), "a resolvable cluster is swept in one pass")

	expectSwept(t, target, ours...)
	expectPresent(t, target, foreign)
	expectSwept(t, target, remoteNS)
	expectSwept(t, local, localNS)
}

// TestTeardownDedicatedNamespaces_PlacedExternalNamespaceKeepsTheTrioLast covers
// the other lifecycle: an External namespace survives the ControlPlane on both
// clusters, so its residue is named and deleted on the cluster it lives on — and
// the ORDER is what the named sweep is for, with the tenant store trio going after
// everything that authenticated through it.
func TestTeardownDedicatedNamespaces_PlacedExternalNamespaceKeepsTheTrioLast(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingPlacedControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleExternal)
	ns := placedTeardownNamespace

	var deletes []string
	residue := []client.Object{
		onTarget(cp, &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
			Name: adminPasswordSecretName(cp), Namespace: ns,
		}}),
		onTarget(cp, &esov1.SecretStore{ObjectMeta: metav1.ObjectMeta{Name: esoTenantStoreName, Namespace: ns}}),
		onTarget(cp, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: esoTenantServiceAccountName, Namespace: ns,
		}}),
	}
	remoteNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	target := fake.NewClientBuilder().WithScheme(s).
		WithObjects(append(append([]client.Object{}, residue...), remoteNS)...).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deletes = append(deletes, obj.GetName())
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: record.NewFakeRecorder(10),
		Resolver: &childrenResolver{children: target},
	}

	done, err := r.teardownDedicatedNamespaces(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue())

	expectSwept(t, target, residue...)
	expectPresent(t, target, remoteNS)
	g.Expect(deletes).To(ContainElements(adminPasswordSecretName(cp), esoTenantStoreName))
	g.Expect(slices.Index(deletes, adminPasswordSecretName(cp))).
		To(BeNumerically("<", slices.Index(deletes, esoTenantStoreName)),
			"the credential material must be deleted before the store it authenticates through")
}

// TestReconcileDelete_SweepsPlacedPushSecretsBeforeThatClustersStore is the
// per-cluster half of the OpenBao-orphan guard: a PushSecret carries
// DeletionPolicy=Delete, and ESO can only purge its OpenBao path while the tenant
// store IN ITS OWN NAMESPACE — on its own cluster — is alive. The teardown
// therefore deletes the placed cluster's PushSecrets and holds the ControlPlane
// until they are gone, so the store is still standing while ESO works.
func TestReconcileDelete_SweepsPlacedPushSecretsBeforeThatClustersStore(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingPlacedControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	ns := placedTeardownNamespace

	// Held by ESO's finalizer, so the Delete leaves it Terminating rather than gone.
	push := onTarget(cp, &esov1alpha1.PushSecret{ObjectMeta: metav1.ObjectMeta{
		Name: "cp-service-account-nova-backup", Namespace: ns,
		Finalizers: []string{"pushsecret.externalsecrets.io/finalizer"},
	}})
	store := onTarget(cp, &esov1.SecretStore{ObjectMeta: metav1.ObjectMeta{
		Name: esoTenantStoreName, Namespace: ns,
	}})
	target := fake.NewClientBuilder().WithScheme(s).WithObjects(push, store).Build()
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: record.NewFakeRecorder(10),
		Resolver: &childrenResolver{children: target},
	}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(local.Get(ctx, key, cp)).To(Succeed())
	res, err := r.reconcileDelete(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter),
		"the teardown must wait for ESO to finish the OpenBao cleanup on the target")

	live := &esov1alpha1.PushSecret{}
	g.Expect(target.Get(ctx, client.ObjectKeyFromObject(push), live)).To(Succeed())
	g.Expect(live.DeletionTimestamp.IsZero()).To(BeFalse(),
		"the placed PushSecret must be deleted by the teardown, not left to a cascade that never reaches it")
	expectPresent(t, target, store)

	// ESO finishes and releases the PushSecret; the store may go now.
	live.Finalizers = nil
	g.Expect(target.Update(ctx, live)).To(Succeed())
	g.Expect(local.Get(ctx, key, cp)).To(Succeed())
	res, err = r.reconcileDelete(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	expectSwept(t, target, store)
	g.Expect(apierrors.IsNotFound(local.Get(ctx, key, &c5c3v1alpha1.ControlPlane{}))).To(BeTrue(),
		"both finalizers must be released once the placed namespace is swept")
}

// TestDeleteBarbicanAuthDelegatorBinding_DeletesItOnTheServicesCluster pins the
// one child that lives outside every namespace. It is cluster-scoped, so neither
// the label-selected sweep (which lists one namespace) nor a namespace deletion
// reclaims it — and it was written on the cluster Barbican was placed on, so that
// is where it has to be deleted. A binding of the same name on the management
// cluster is a different object and must survive, even carrying our labels.
func TestDeleteBarbicanAuthDelegatorBinding_DeletesItOnTheServicesCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingBarbicanControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	cp.Spec.Services.Barbican.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: placedTeardownCluster}
	name := barbicanOpenBaoAuthDelegatorName(barbicanOpenBaoName(cp), cp.BarbicanNamespace())

	placed := onTarget(cp, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}})
	atHome := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: name, Labels: controlPlaneChildLabels(cp),
	}}
	target := fake.NewClientBuilder().WithScheme(s).WithObjects(placed).Build()
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, atHome).Build()
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: record.NewFakeRecorder(10),
		Resolver: &childrenResolver{children: target},
	}

	g.Expect(r.deleteBarbicanAuthDelegatorBinding(ctx, cp)).To(Succeed())
	expectSwept(t, target, placed)
	expectPresent(t, local, atHome)
}

// TestDeleteBarbicanAuthDelegatorBinding_ReleasesWhenTheTargetDeniesTheDelete
// closes the second half of the same wedge the unconditional get closes. The
// target's access chart grants get on ClusterRoleBindings always but create,
// patch and delete only behind authDelegatorBinding, so a cluster that had the
// flag on when the binding was written and has it off now answers the read with
// the binding and the delete with a 403 — every pass, with no stall breaker left
// on this path. Returning that error would hold the ControlPlane in Terminating
// forever, so the binding is left standing and named instead.
func TestDeleteBarbicanAuthDelegatorBinding_ReleasesWhenTheTargetDeniesTheDelete(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingBarbicanControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	cp.Spec.Services.Barbican.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: placedTeardownCluster}
	name := barbicanOpenBaoAuthDelegatorName(barbicanOpenBaoName(cp), cp.BarbicanNamespace())

	placed := onTarget(cp, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}})
	target := fake.NewClientBuilder().WithScheme(s).WithObjects(placed).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				return apierrors.NewForbidden(rbacv1.Resource("clusterrolebindings"), obj.GetName(), nil)
			},
		}).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build(),
		Scheme: s, Recorder: rec, Resolver: &childrenResolver{children: target},
	}

	g.Expect(r.deleteBarbicanAuthDelegatorBinding(ctx, cp)).To(Succeed(),
		"a denied delete must release the ControlPlane rather than wedge it in Terminating")
	expectPresent(t, target, placed)

	events := strings.Join(drainEvents(rec), "\n")
	g.Expect(events).To(ContainSubstring("AuthDelegatorBindingNotReclaimed"))
	g.Expect(events).To(ContainSubstring(name), "the event must name the binding left behind")
	g.Expect(events).To(ContainSubstring("authDelegatorBinding=true"),
		"and the grant that would have let the teardown reclaim it")
}

// Any other denial is a real failure the teardown may retry: only the cluster
// withholding the grant is unrecoverable, and swallowing the rest would release
// the ControlPlane over a conflict or an outage that the next pass would clear.
func TestDeleteBarbicanAuthDelegatorBinding_KeepsHoldingOnOtherDeleteErrors(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingBarbicanControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	cp.Spec.Services.Barbican.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: placedTeardownCluster}
	name := barbicanOpenBaoAuthDelegatorName(barbicanOpenBaoName(cp), cp.BarbicanNamespace())

	placed := onTarget(cp, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}})
	target := fake.NewClientBuilder().WithScheme(s).WithObjects(placed).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				return apierrors.NewConflict(rbacv1.Resource("clusterrolebindings"), obj.GetName(), nil)
			},
		}).Build()
	r := &ControlPlaneReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build(),
		Scheme: s, Recorder: record.NewFakeRecorder(10),
		Resolver: &childrenResolver{children: target},
	}

	g.Expect(r.deleteBarbicanAuthDelegatorBinding(ctx, cp)).NotTo(Succeed())
}

// TestReconcileDelete_HoldsBothFinalizersUntilThePlacedNamespaceIsSwept pins the
// release order: while a service child of the placed namespace is still
// Terminating behind its own operator's cleanup, neither finalizer may go — the
// service operator's own remote sweep is what that wait is for — and once it is
// gone both are released in the same pass.
func TestReconcileDelete_HoldsBothFinalizersUntilThePlacedNamespaceIsSwept(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingPlacedControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	ns := placedTeardownNamespace

	// The Keystone CR lives on the MANAGEMENT cluster whatever cluster it places
	// its own children on, held by its cleanup finalizer.
	keystone := &keystonev1alpha1.Keystone{ObjectMeta: metav1.ObjectMeta{
		Name: keystoneName(cp), Namespace: ns, Labels: controlPlaneChildLabels(cp),
		Finalizers: []string{"keystone.openstack.c5c3.io/cleanup"},
	}}
	placedChild := onTarget(cp, &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Name: adminPasswordSecretName(cp), Namespace: ns,
	}})
	target := fake.NewClientBuilder().WithScheme(s).WithObjects(placedChild).Build()
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, keystone).Build()
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: record.NewFakeRecorder(10),
		Resolver: &childrenResolver{children: target},
	}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(local.Get(ctx, key, cp)).To(Succeed())
	res, err := r.reconcileDelete(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(namespaceRequeueAfter))
	expectPresent(t, target, placedChild)

	held := &c5c3v1alpha1.ControlPlane{}
	g.Expect(local.Get(ctx, key, held)).To(Succeed())
	g.Expect(held.Finalizers).To(ConsistOf(controlPlaneORCFinalizer, commonmulticluster.RemoteChildrenFinalizer))

	// The keystone-operator finishes its own teardown, remote sweep included.
	live := &keystonev1alpha1.Keystone{}
	g.Expect(local.Get(ctx, client.ObjectKeyFromObject(keystone), live)).To(Succeed())
	live.Finalizers = nil
	g.Expect(local.Update(ctx, live)).To(Succeed())

	g.Expect(local.Get(ctx, key, cp)).To(Succeed())
	res, err = r.reconcileDelete(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	expectSwept(t, target, placedChild)
	g.Expect(apierrors.IsNotFound(local.Get(ctx, key, &c5c3v1alpha1.ControlPlane{}))).To(BeTrue())
}

// TestTeardownDedicatedNamespaces_WaitsForAnUnresolvedTargetCluster covers the
// first of the two answers an unresolvable cluster gets. Engagement is
// asynchronous, so right after an operator restart a registered cluster looks
// exactly like a deregistered one: within the abandon window the teardown waits,
// keeping the finalizers, rather than releasing a ControlPlane whose children are
// running on a cluster that is about to answer.
func TestTeardownDedicatedNamespaces_WaitsForAnUnresolvedTargetCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingPlacedControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: rec,
		Resolver: &childrenResolver{err: mcruntime.ErrClusterNotFound},
	}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(local.Get(ctx, key, cp)).To(Succeed())
	res, err := r.reconcileDelete(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred(), "an unresolvable cluster must never fail the deletion pass")
	g.Expect(res.RequeueAfter).To(Equal(namespaceRequeueAfter))

	held := &c5c3v1alpha1.ControlPlane{}
	g.Expect(local.Get(ctx, key, held)).To(Succeed())
	g.Expect(held.Finalizers).To(ConsistOf(controlPlaneORCFinalizer, commonmulticluster.RemoteChildrenFinalizer))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNamespacesReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(drainEvents(rec)).NotTo(ContainElement(ContainSubstring("RemoteChildrenAbandoned")),
		"a cluster that may still be engaging must not be given up on")
}

// TestTeardownDedicatedNamespaces_AbandonsAnUnresolvedTargetClusterPastTheWindow
// covers the other answer. A cluster that has not resolved for the whole abandon
// window is deregistered as far as this operator can tell: its children are
// unreachable either way, so they are left running, a Warning records that they
// were, and the finalizers are released — holding the ControlPlane in Terminating
// forever would help nobody.
func TestTeardownDedicatedNamespaces_AbandonsAnUnresolvedTargetClusterPastTheWindow(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)
	abandonImmediately(t)

	cp := deletingPlacedControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: rec,
		Resolver: &childrenResolver{err: mcruntime.ErrClusterNotFound},
	}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(local.Get(ctx, key, cp)).To(Succeed())
	res, err := r.reconcileDelete(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	g.Expect(apierrors.IsNotFound(local.Get(ctx, key, &c5c3v1alpha1.ControlPlane{}))).To(BeTrue(),
		"an abandoned cluster must not strand the ControlPlane in Terminating")
	events := strings.Join(drainEvents(rec), "\n")
	g.Expect(events).To(ContainSubstring("RemoteChildrenAbandoned"))
	g.Expect(events).To(ContainSubstring(placedTeardownCluster))
	g.Expect(events).To(ContainSubstring(placedTeardownNamespace))
}

// TestTeardownDedicatedNamespaces_AbandonStillReapsTheNamespaceAtHome pins the
// half of the abandon path that IS reachable. A placed Managed namespace exists
// on both clusters, and the target's copy is unreclaimable once its cluster is
// given up on — but the management cluster's is reachable, ours, and nothing
// comes back for it after both finalizers are released.
func TestTeardownDedicatedNamespaces_AbandonStillReapsTheNamespaceAtHome(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)
	abandonImmediately(t)

	cp := deletingPlacedControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	localNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: placedTeardownNamespace, Labels: controlPlaneChildLabels(cp),
	}}
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, localNS).Build()
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: record.NewFakeRecorder(10),
		Resolver: &childrenResolver{err: mcruntime.ErrClusterNotFound},
	}

	done, err := r.teardownDedicatedNamespaces(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue(), "an abandoned cluster must not hold the release")
	expectSwept(t, local, localNS)
}

// TestDeleteManagedNamespace_RefusesAForeignManagementClustersNamespace is the
// guard on the one delete this operator makes on a cluster it does not own. The
// ownership labels name a ControlPlane by name and namespace only, and a target
// cluster may be registered by any number of management clusters — each able to
// run a ControlPlane called "openstack" in namespace "openstack", the quickstart
// defaults, and to place a service in a namespace called "identity". The UID
// stamped at creation is the one mark that tells them apart, and what it stops is
// one teardown cascading the other's database, PVC and tenant store away.
func TestDeleteManagedNamespace_RefusesAForeignManagementClustersNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingPlacedControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	// Same name, same namespace, same labels — a different management cluster's CR.
	theirs := placedNamespaceOnTarget(cp, placedTeardownNamespace)
	theirs.Annotations[controlPlaneUIDAnnotation] = "another-management-clusters-cp-uid"

	target := fake.NewClientBuilder().WithScheme(s).WithObjects(theirs).Build()
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: rec,
		Resolver: &childrenResolver{children: target},
	}

	done, err := r.teardownDedicatedNamespaces(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue(), "refusing the namespace must not wedge the release")
	expectPresent(t, target, theirs)
	g.Expect(strings.Join(drainEvents(rec), "\n")).To(ContainSubstring("NamespaceNotOwned"))
}

// TestDeleteManagedNamespace_StillReapsANamespaceWhoseMarkWasStripped is the
// counterpart of that guard. What proves the namespace is somebody else's is a
// mark naming somebody else — the annotation is an ordinary, mutable annotation
// on a cluster this operator does not own, so a mutating policy or an annotation
// pruner can take it off. Reading that as "not ours" would leak the namespace,
// and everything in it, permanently: nothing else ever comes back for it once the
// finalizers are released.
func TestDeleteManagedNamespace_StillReapsANamespaceWhoseMarkWasStripped(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingPlacedControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	stripped := placedNamespaceOnTarget(cp, placedTeardownNamespace)
	stripped.Annotations = nil

	target := fake.NewClientBuilder().WithScheme(s).WithObjects(stripped).Build()
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: record.NewFakeRecorder(10),
		Resolver: &childrenResolver{children: target},
	}

	done, err := r.teardownDedicatedNamespaces(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue())
	expectSwept(t, target, stripped)
}

// TestDeleteManagedNamespace_DecidesFromTheUncachedReader pins which of the target
// cluster's two readers answers the ownership question. The verdict authorises
// deleting a whole namespace — the service's database, its PVC, its tenant store
// — so it may not be read from an informer that trails the API server: here the
// cache still holds the pre-adoption, unlabelled copy while the live cluster has
// the one the operator created and owns.
func TestDeleteManagedNamespace_DecidesFromTheUncachedReader(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingPlacedControlPlane(time.Minute, c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	stale := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: placedTeardownNamespace}}
	live := placedNamespaceOnTarget(cp, placedTeardownNamespace)

	target := fake.NewClientBuilder().WithScheme(s).WithObjects(stale).Build()
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: record.NewFakeRecorder(10),
		Resolver: &childrenResolver{
			children: target,
			reader:   fake.NewClientBuilder().WithScheme(s).WithObjects(live).Build(),
		},
	}

	done, err := r.teardownDedicatedNamespaces(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue())
	expectSwept(t, target, stale)
}

// TestReconcileDelete_StallEscapeKeepsTheRemoteChildrenFinalizer guards the one
// release the escape may not make. It gives up on K-ORC without ever reaching the
// namespace sweep, so the children on the placed cluster are still standing: the
// remote-children finalizer stays on, and the next pass finishes the sweep and
// releases it.
func TestReconcileDelete_StallEscapeKeepsTheRemoteChildrenFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := namespaceTeardownScheme(t)

	cp := deletingPlacedControlPlane(orcTeardownStallTimeout+time.Minute,
		c5c3v1alpha1.ServiceNamespaceLifecycleManaged)
	// A managed K-ORC CR wedged behind a finalizer K-ORC can no longer run.
	wedged := &orcv1alpha1.ApplicationCredential{ObjectMeta: terminatingImportMeta(
		adminAppCredentialName(cp), childNamespace(cp), "openstack.k-orc.cloud/applicationcredential")}
	placedChild := onTarget(cp, &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
		Name: adminPasswordSecretName(cp), Namespace: placedTeardownNamespace,
	}})
	target := fake.NewClientBuilder().WithScheme(s).WithObjects(placedChild).Build()
	local := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, wedged).Build()
	rec := record.NewFakeRecorder(10)
	r := &ControlPlaneReconciler{
		Client: local, Scheme: s, Recorder: rec,
		Resolver: &childrenResolver{children: target},
	}

	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	g.Expect(local.Get(ctx, key, cp)).To(Succeed())
	_, err := r.reconcileDelete(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(strings.Join(drainEvents(rec), "\n")).To(ContainSubstring("ORCTeardownStalled"))

	escaped := &c5c3v1alpha1.ControlPlane{}
	g.Expect(local.Get(ctx, key, escaped)).To(Succeed())
	g.Expect(escaped.Finalizers).To(ConsistOf(commonmulticluster.RemoteChildrenFinalizer),
		"the escape gave up on K-ORC, not on the children the ControlPlane placed")
	expectPresent(t, target, placedChild)

	// The next pass runs the sweep the escape never reached.
	res, err := r.reconcileDelete(ctx, escaped)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	expectSwept(t, target, placedChild)
	g.Expect(apierrors.IsNotFound(local.Get(ctx, key, &c5c3v1alpha1.ControlPlane{}))).To(BeTrue())
}
