// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/c5c3/forge/internal/common/conditions"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
)

func TestReconcile_NotFoundIsNoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newTestReconciler(testScheme())

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "missing"},
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
}

func TestReconcile_DeletingCRSkipsPipeline(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	now := metav1.NewTime(time.Now())
	h.DeletionTimestamp = &now
	// A finalizer is required for the fake client to accept an object with a
	// DeletionTimestamp; the operator itself never installs one.
	h.Finalizers = []string{"test.c5c3.io/keep"}
	r := newTestReconciler(testScheme(), h)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-horizon"},
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	// No pipeline step ran: no conditions were persisted.
	var got horizonv1alpha1.Horizon
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-horizon"}, &got)).To(Succeed())
	g.Expect(got.Status.Conditions).To(BeEmpty())
}

func TestReconcile_SecretsGateShortCircuitsAndPersistsStatus(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	// Store ready but the SECRET_KEY Secret is absent: the pipeline must stop
	// at Secrets, persist SecretsReady=False, and aggregate Ready=False.
	r := newTestReconciler(testScheme(), h, readyClusterSecretStore(openBaoClusterStoreName))

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-horizon"},
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueSecretPolling))

	var got horizonv1alpha1.Horizon
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-horizon"}, &got)).To(Succeed())

	secretsCond := conditions.GetCondition(got.Status.Conditions, "SecretsReady")
	g.Expect(secretsCond).NotTo(BeNil())
	g.Expect(secretsCond.Status).To(Equal(metav1.ConditionFalse))

	readyCond := conditions.GetCondition(got.Status.Conditions, "Ready")
	g.Expect(readyCond).NotTo(BeNil())
	g.Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))

	// The chain short-circuited: no downstream condition was set.
	g.Expect(conditions.GetCondition(got.Status.Conditions, "DeploymentReady")).To(BeNil())
}

func TestHorizonSecretNameExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	h := testHorizon()
	g.Expect(horizonSecretNameExtractor(h)).To(Equal([]string{"horizon-secret-key"}))

	// Empty reference yields no index entries rather than an empty string.
	h.Spec.SecretKeyRef.Name = ""
	g.Expect(horizonSecretNameExtractor(h)).To(BeNil())

	// Wrong type is tolerated (nil, not panic).
	g.Expect(horizonSecretNameExtractor(&corev1.Secret{})).To(BeNil())
}
