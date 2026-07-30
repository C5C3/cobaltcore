// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the Placement reconcile entry point, the finalizer-gated
// deletion path, and the field-index plumbing.
package controller

import (
	"context"
	"testing"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/conditions"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
	placementv1alpha1 "github.com/c5c3/forge/operators/placement/api/v1alpha1"
)

func TestReconcile_AddsFinalizerOnFirstPass(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement() // no finalizer yet
	r := newPlacementTestReconciler(placement)

	res, err := r.Reconcile(context.Background(), placementRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{Requeue: true}), "the finalizer add requeues so the next pass sees it persisted")
	got := getPlacement(t, r.Client, "test-placement")
	g.Expect(got.Finalizers).To(ContainElement(placementFinalizer))
}

func TestReconcile_FailingSecretsStepShortCircuitsPipeline(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Finalizers = []string{placementFinalizer} // skip the finalizer-add requeue
	// The selected store is explicitly not Ready, so the Secrets step fails fast.
	r := newPlacementTestReconciler(placement, notReadyClusterSecretStore(openBaoClusterStoreName))

	res, err := r.Reconcile(context.Background(), placementRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	got := getPlacement(t, r.Client, "test-placement")
	cond := conditions.GetCondition(got.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	// A later step must not have run: the config step never rendered, so the
	// ExtraConfigHealthy condition it maintains is absent.
	g.Expect(conditions.GetCondition(got.Status.Conditions, "ExtraConfigHealthy")).To(BeNil())
	g.Expect(got.Status.ObservedGeneration).To(Equal(int64(1)))
}

func TestReconcile_NotFoundCRIsIgnored(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newPlacementTestReconciler() // no Placement seeded

	res, err := r.Reconcile(context.Background(), placementRequest)

	g.Expect(err).NotTo(HaveOccurred(), "a deleted CR is not an error")
	g.Expect(res.IsZero()).To(BeTrue())
}

func TestReconcileDelete_LiveResourcesRetainFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Finalizers = []string{placementFinalizer}
	// A live MariaDB Database owned by this Placement (key is the bare CR name).
	mdb := &mariadbv1alpha1.Database{}
	mdb.Name = "test-placement"
	mdb.Namespace = "default"
	r := newPlacementTestReconciler(placement, mdb)

	// Move the Placement into the deleting state.
	g.Expect(r.Delete(context.Background(), placement)).To(Succeed())

	res, err := r.Reconcile(context.Background(), placementRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	got := getPlacement(t, r.Client, "test-placement")
	g.Expect(got.Finalizers).To(ContainElement(placementFinalizer),
		"the finalizer is retained one pass while live MariaDB resources remain")
}

func TestReconcileDelete_NoLiveResourcesReleasesFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Finalizers = []string{placementFinalizer}
	r := newPlacementTestReconciler(placement) // no MariaDB CRs

	g.Expect(r.Delete(context.Background(), placement)).To(Succeed())

	res, err := r.Reconcile(context.Background(), placementRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	// With the finalizer released, the fake client garbage-collects the CR.
	var gone placementv1alpha1.Placement
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-placement"}, &gone)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the finalizer is released when no live MariaDB resource remains")
}

func TestReconcileDelete_WithoutFinalizerIsNoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
	r := newPlacementTestReconciler()

	res, err := r.reconcileDelete(context.Background(), placement)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(BeEmpty(),
		"a CR without the finalizer emits no cleanup events")
}

func TestSetReadyCondition_TrueOnlyWhenAllSubConditionsTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()

	// Every sub-condition True → aggregate Ready True.
	for _, ct := range subConditionTypes {
		conditions.SetCondition(&placement.Status.Conditions, metav1.Condition{
			Type:   ct,
			Status: metav1.ConditionTrue,
			Reason: "OK",
		})
	}
	setReadyCondition(placement)
	ready := conditions.GetCondition(placement.Status.Conditions, "Ready")
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))

	// Flip one sub-condition False → aggregate Ready flips False.
	conditions.SetCondition(&placement.Status.Conditions, metav1.Condition{
		Type:   "HPAReady",
		Status: metav1.ConditionFalse,
		Reason: "Degraded",
	})
	setReadyCondition(placement)
	ready = conditions.GetCondition(placement.Status.Conditions, "Ready")
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
}

// recordingFieldIndexer is a client.FieldIndexer that records the keys it was
// asked to register, so the registration helper can be exercised without a
// running manager.
type recordingFieldIndexer struct {
	keys []string
}

func (r *recordingFieldIndexer) IndexField(_ context.Context, _ client.Object, field string, _ client.IndexerFunc) error {
	r.keys = append(r.keys, field)
	return nil
}

func TestPlacementSecretNameExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	// serviceUser + database Secret names, deduplicated.
	placement := testPlacement()
	g.Expect(placementSecretNameExtractor(placement)).To(ConsistOf("placement-service-user", "placement-db"))

	// The same Secret backing both references collapses to one entry.
	placement.Spec.Database.SecretRef.Name = "placement-service-user"
	g.Expect(placementSecretNameExtractor(placement)).To(ConsistOf("placement-service-user"))

	// An empty database Secret name is skipped.
	placement.Spec.Database.SecretRef.Name = ""
	g.Expect(placementSecretNameExtractor(placement)).To(ConsistOf("placement-service-user"))

	// The wrong object type yields nil rather than a panic.
	g.Expect(placementSecretNameExtractor(&corev1.Secret{})).To(BeNil())
}

func TestRegisterPlacementIndexes_RegistersSecretNameKey(t *testing.T) {
	g := NewGomegaWithT(t)

	idx := &recordingFieldIndexer{}
	g.Expect(registerPlacementIndexes(context.Background(), idx)).To(Succeed())
	g.Expect(idx.keys).To(ConsistOf(PlacementSecretNameIndexKey))
}
