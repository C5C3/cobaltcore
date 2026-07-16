// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the field-index extractors and registration helpers shared by
// the Glance and GlanceBackend controllers.
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
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/conditions"
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

func TestReconcile_AddsFinalizerOnFirstPass(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance() // no finalizer yet
	r := newGlanceTestReconciler(glance)

	res, err := r.Reconcile(context.Background(), glanceRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Requeue).To(BeTrue(), "the finalizer add requeues so the next pass sees it persisted")
	got := getGlance(t, r.Client, "test-glance")
	g.Expect(got.Finalizers).To(ContainElement(glanceFinalizer))
}

func TestReconcile_FailingSecretsStepShortCircuitsPipeline(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Finalizers = []string{glanceFinalizer} // skip the finalizer-add requeue
	// The selected store is explicitly not Ready, so the Secrets step fails fast.
	r := newGlanceTestReconciler(glance, notReadyClusterSecretStore(openBaoClusterStoreName))

	res, err := r.Reconcile(context.Background(), glanceRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueSecretPolling))

	got := getGlance(t, r.Client, "test-glance")
	secrets := conditions.GetCondition(got.Status.Conditions, "SecretsReady")
	g.Expect(secrets).NotTo(BeNil())
	g.Expect(secrets.Status).To(Equal(metav1.ConditionFalse))
	// A later step must not have run: BackendsReady is never set when Secrets
	// short-circuits the pipeline.
	g.Expect(conditions.GetCondition(got.Status.Conditions, "BackendsReady")).To(BeNil())
	g.Expect(conditions.GetCondition(got.Status.Conditions, "DatabaseReady")).To(BeNil())
}

func TestReconcileDelete_LiveResourcesRetainFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Finalizers = []string{glanceFinalizer}
	// A live MariaDB Database owned by this Glance (key is the bare CR name).
	mdb := &mariadbv1alpha1.Database{}
	mdb.Name = "test-glance"
	mdb.Namespace = "default"
	r := newGlanceTestReconciler(glance, mdb)

	// Move the Glance into the deleting state.
	g.Expect(r.Delete(context.Background(), glance)).To(Succeed())

	res, err := r.Reconcile(context.Background(), glanceRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	got := getGlance(t, r.Client, "test-glance")
	g.Expect(got.Finalizers).To(ContainElement(glanceFinalizer),
		"the finalizer is retained one pass while live MariaDB resources remain")
}

func TestReconcileDelete_NoLiveResourcesReleasesFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Finalizers = []string{glanceFinalizer}
	r := newGlanceTestReconciler(glance) // no MariaDB CRs

	g.Expect(r.Delete(context.Background(), glance)).To(Succeed())

	res, err := r.Reconcile(context.Background(), glanceRequest)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	// With the finalizer released, the fake client garbage-collects the CR.
	var gone glancev1alpha1.Glance
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-glance"}, &gone)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the finalizer is released when no live MariaDB resource remains")
}

func TestSetReadyCondition_TrueOnlyWhenAllSubConditionsTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()

	// All eight sub-conditions True → aggregate Ready True.
	for _, ct := range subConditionTypes {
		conditions.SetCondition(&glance.Status.Conditions, metav1.Condition{
			Type:   ct,
			Status: metav1.ConditionTrue,
			Reason: "OK",
		})
	}
	setReadyCondition(glance)
	ready := conditions.GetCondition(glance.Status.Conditions, "Ready")
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))

	// Flip one sub-condition False → aggregate Ready flips False.
	conditions.SetCondition(&glance.Status.Conditions, metav1.Condition{
		Type:   "HPAReady",
		Status: metav1.ConditionFalse,
		Reason: "Degraded",
	})
	setReadyCondition(glance)
	ready = conditions.GetCondition(glance.Status.Conditions, "Ready")
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
}

// recordingFieldIndexer is a client.FieldIndexer that records the keys it was
// asked to register, so the registration helpers can be exercised without a
// running manager.
type recordingFieldIndexer struct {
	keys []string
}

func (r *recordingFieldIndexer) IndexField(_ context.Context, _ client.Object, field string, _ client.IndexerFunc) error {
	r.keys = append(r.keys, field)
	return nil
}

func TestGlanceSecretNameExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	// serviceUser + database Secret names, deduplicated.
	glance := testGlance()
	g.Expect(glanceSecretNameExtractor(glance)).To(ConsistOf("glance-service-user", "glance-db"))

	// The same Secret backing both references collapses to one entry.
	glance.Spec.Database.SecretRef.Name = "glance-service-user"
	g.Expect(glanceSecretNameExtractor(glance)).To(ConsistOf("glance-service-user"))

	// An empty database Secret name is skipped.
	glance.Spec.Database.SecretRef.Name = ""
	g.Expect(glanceSecretNameExtractor(glance)).To(ConsistOf("glance-service-user"))

	// The wrong object type yields nil rather than a panic.
	g.Expect(glanceSecretNameExtractor(&corev1.Secret{})).To(BeNil())
}

func TestGlanceBackendSecretNameExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	// Credentials Secret only.
	b := testGlanceBackend("store", "test-glance")
	g.Expect(glanceBackendSecretNameExtractor(b)).To(ConsistOf("store-s3-creds"))

	// A nil S3 block (bypassed admission) indexes nothing.
	b.Spec.S3 = nil
	g.Expect(glanceBackendSecretNameExtractor(b)).To(BeEmpty())

	// The wrong object type yields nil rather than a panic.
	g.Expect(glanceBackendSecretNameExtractor(&corev1.Secret{})).To(BeNil())
}

func TestGlanceBackendGlanceRefExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	b := testGlanceBackend("store", "test-glance")
	g.Expect(glanceBackendGlanceRefExtractor(b)).To(ConsistOf("test-glance"))

	// An empty glanceRef (bypassed admission) indexes nothing.
	b.Spec.GlanceRef.Name = ""
	g.Expect(glanceBackendGlanceRefExtractor(b)).To(BeNil())

	// The wrong object type yields nil rather than a panic.
	g.Expect(glanceBackendGlanceRefExtractor(&corev1.Secret{})).To(BeNil())
}

func TestRegisterGlanceIndexes_RegistersSecretNameKey(t *testing.T) {
	g := NewGomegaWithT(t)

	idx := &recordingFieldIndexer{}
	g.Expect(registerGlanceIndexes(context.Background(), idx)).To(Succeed())
	g.Expect(idx.keys).To(ConsistOf(GlanceSecretNameIndexKey))
}

func TestRegisterGlanceBackendIndexes_RegistersBothKeys(t *testing.T) {
	g := NewGomegaWithT(t)

	idx := &recordingFieldIndexer{}
	g.Expect(registerGlanceBackendIndexes(context.Background(), idx)).To(Succeed())
	g.Expect(idx.keys).To(ConsistOf(GlanceBackendGlanceRefIndexKey, GlanceBackendSecretNameIndexKey))
}
