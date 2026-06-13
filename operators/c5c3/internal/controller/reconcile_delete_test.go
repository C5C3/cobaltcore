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
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
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
