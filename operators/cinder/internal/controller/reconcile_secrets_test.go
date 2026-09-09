// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
)

func TestReconcileSecrets_StoreNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	r := newCinderTestReconciler(cinder, notReadyClusterSecretStore(openBaoClusterStoreName))

	res, digest, err := r.reconcileSecrets(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(digest).To(BeEmpty())

	cond := cinderCondition(cinder, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("SecretStoreNotReady"))
}

// TestReconcileSecrets_DBSecretMissingKeyWaits covers the half-synced database
// Secret: it exists, so the fast path finds it, but it carries only the username
// ESO wrote first. A DSN assembled from it would name an empty password.
func TestReconcileSecrets_DBSecretMissingKeyWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	halfSynced := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cinder-db", Namespace: testNamespace},
		Data:       map[string][]byte{"username": []byte("cinder")},
	}
	r := newCinderTestReconciler(cinder,
		readyClusterSecretStore(openBaoClusterStoreName),
		halfSynced, cinderServiceUserSecret("svc-pw"))

	res, digest, err := r.reconcileSecrets(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(digest).To(BeEmpty())

	cond := cinderCondition(cinder, "SecretsReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForDBCredentials"))
}

// TestReconcileSecrets_ServiceUserKeyMissingWaits proves the service-user gate
// checks the key spec.serviceUser.secretRef.key names rather than the Secret's
// mere existence.
func TestReconcileSecrets_ServiceUserKeyMissingWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	wrongKey := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cinder-service-user", Namespace: testNamespace},
		Data:       map[string][]byte{"not-password": []byte("svc-pw")},
	}
	r := newCinderTestReconciler(cinder,
		readyClusterSecretStore(openBaoClusterStoreName),
		cinderDBSecret(), wrongKey)

	res, digest, err := r.reconcileSecrets(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(digest).To(BeEmpty())

	cond := cinderCondition(cinder, "SecretsReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForServiceUserCredentials"))
}

// TestReconcileSecrets_KeystoneFreeSkipsServiceUserGate covers the Cinder that
// runs without the Keystone integration: no service-user Secret exists at all,
// and gating on one would leave the CR waiting forever for a Secret nothing ever
// creates.
func TestReconcileSecrets_KeystoneFreeSkipsServiceUserGate(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := keystoneFreeCinder()
	r := newCinderTestReconciler(cinder,
		readyClusterSecretStore(openBaoClusterStoreName), cinderDBSecret())

	res, digest, err := r.reconcileSecrets(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(digest).To(BeEmpty(), "there is no service-user password to roll the pods on")

	cond := cinderCondition(cinder, "SecretsReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("SecretsAvailable"))
}

func TestReconcileSecrets_AllPresentReturnsStableDigest(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	r := newCinderTestReconciler(cinder,
		readyClusterSecretStore(openBaoClusterStoreName),
		cinderDBSecret(), cinderServiceUserSecret("svc-pw"))

	res, digest, err := r.reconcileSecrets(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	// The digest is the SHA-256 of the service-user password and is stable.
	g.Expect(digest).To(Equal(secrets.AdminPasswordDigest("svc-pw")))

	cond := cinderCondition(cinder, "SecretsReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("SecretsAvailable"))

	// A second pass returns the identical digest (no dependency on iteration
	// order or wall-clock).
	_, digest2, err := r.reconcileSecrets(context.Background(), r.Client, cinder)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(digest2).To(Equal(digest))
}

// TestEffectiveServiceUserKey covers the two CRs that bypassed the defaulting
// webhook: one without a service user at all and one whose secretRef leaves the
// key empty.
func TestEffectiveServiceUserKey(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(effectiveServiceUserKey(keystoneFreeCinder())).To(Equal("password"))

	bypassed := validCinder()
	bypassed.Spec.ServiceUser.SecretRef.Key = ""
	g.Expect(effectiveServiceUserKey(bypassed)).To(Equal("password"))

	custom := validCinder()
	custom.Spec.ServiceUser.SecretRef.Key = "cinder-password"
	g.Expect(effectiveServiceUserKey(custom)).To(Equal("cinder-password"))
}
