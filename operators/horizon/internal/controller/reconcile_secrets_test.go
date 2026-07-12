// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/c5c3/forge/internal/common/conditions"
	commonv1 "github.com/c5c3/forge/internal/common/types"
)

func TestReconcileSecrets_StoreNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	r := newTestReconciler(testScheme(), h, notReadyClusterSecretStore(openBaoClusterStoreName))

	res, digest, err := r.reconcileSecrets(context.Background(), h)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueSecretPolling))
	g.Expect(digest).To(BeEmpty())
	cond := conditions.GetCondition(h.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("SecretStoreNotReady"))
}

func TestReconcileSecrets_SecretMissing(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	// Store ready, but neither the Secret nor its ExternalSecret exists.
	r := newTestReconciler(testScheme(), h, readyClusterSecretStore(openBaoClusterStoreName))

	res, digest, err := r.reconcileSecrets(context.Background(), h)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueSecretPolling))
	g.Expect(digest).To(BeEmpty())
	cond := conditions.GetCondition(h.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForSecretKey"))
	g.Expect(cond.Message).To(ContainSubstring("not found yet"))
}

func TestReconcileSecrets_SecretMissingExpectedKey(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	// The Secret exists but under the wrong data key; without the
	// ExternalSecret the gate attributes this as ExternalSecretMissing (the
	// Secret is unusable and no ExternalSecret explains why).
	r := newTestReconciler(
		testScheme(), h,
		readyClusterSecretStore(openBaoClusterStoreName),
		secretKeySecret("horizon-secret-key", "default", "wrong-key", "value"),
	)

	res, digest, err := r.reconcileSecrets(context.Background(), h)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueSecretPolling))
	g.Expect(digest).To(BeEmpty())
	cond := conditions.GetCondition(h.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForSecretKey"))
}

func TestReconcileSecrets_ReadyReturnsDigest(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	r := newTestReconciler(
		testScheme(), h,
		readyClusterSecretStore(openBaoClusterStoreName),
		secretKeySecret("horizon-secret-key", "default", "secret-key", "super-secret"),
	)

	res, digest, err := r.reconcileSecrets(context.Background(), h)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(digest).NotTo(BeEmpty())
	// The digest is a hash, never the raw key material.
	g.Expect(digest).NotTo(ContainSubstring("super-secret"))
	cond := conditions.GetCondition(h.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("SecretsAvailable"))
}

func TestReconcileSecrets_DigestChangesWithKeyMaterial(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	r1 := newTestReconciler(
		testScheme(), h,
		readyClusterSecretStore(openBaoClusterStoreName),
		secretKeySecret("horizon-secret-key", "default", "secret-key", "value-one"),
	)
	_, digest1, err := r1.reconcileSecrets(context.Background(), h)
	g.Expect(err).NotTo(HaveOccurred())

	r2 := newTestReconciler(
		testScheme(), h,
		readyClusterSecretStore(openBaoClusterStoreName),
		secretKeySecret("horizon-secret-key", "default", "secret-key", "value-two"),
	)
	_, digest2, err := r2.reconcileSecrets(context.Background(), h)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(digest1).NotTo(Equal(digest2), "a rotated SECRET_KEY must produce a different digest so the Deployment rolls")
}

// TestReconcileSecrets_NamespacedStoreReady_GatesThroughTenantStore verifies a
// Horizon that selects a namespaced SecretStore gates SecretsReady on that
// store (resolved in the Horizon's own namespace), not the cluster store.
func TestReconcileSecrets_NamespacedStoreReady_GatesThroughTenantStore(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store",
	}
	// Only the namespaced store is Ready; no cluster store exists.
	r := newTestReconciler(
		testScheme(), h,
		readySecretStore("openbao-tenant-store", "default"),
		secretKeySecret("horizon-secret-key", "default", "secret-key", "super-secret"),
	)

	res, digest, err := r.reconcileSecrets(context.Background(), h)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(digest).NotTo(BeEmpty())
	cond := conditions.GetCondition(h.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("SecretsAvailable"))
}

// TestReconcileSecrets_NamespacedStoreMissing_SetsCondition verifies a Horizon
// pinned to an absent namespaced SecretStore flips SecretsReady False with a
// message naming the namespaced kind and store name, even though no cluster
// store exists to fall back on.
func TestReconcileSecrets_NamespacedStoreMissing_SetsCondition(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store",
	}
	r := newTestReconciler(testScheme(), h)

	res, digest, err := r.reconcileSecrets(context.Background(), h)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueSecretPolling))
	g.Expect(digest).To(BeEmpty())
	cond := conditions.GetCondition(h.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("SecretStoreNotReady"))
	g.Expect(cond.Message).To(ContainSubstring("SecretStore"))
	g.Expect(cond.Message).To(ContainSubstring("openbao-tenant-store"))
}

func TestEffectiveSecretKeyKey_DefaultsWhenEmpty(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.SecretKeyRef.Key = ""

	g.Expect(effectiveSecretKeyKey(h)).To(Equal("secret-key"))

	h.Spec.SecretKeyRef.Key = "custom"
	g.Expect(effectiveSecretKeyKey(h)).To(Equal("custom"))
}
