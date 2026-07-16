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

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/secrets"
)

func TestReconcileSecrets_StoreNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	r := newGlanceTestReconciler(glance, notReadyClusterSecretStore(openBaoClusterStoreName))

	res, digest, err := r.reconcileSecrets(context.Background(), glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueSecretPolling))
	g.Expect(digest).To(BeEmpty())
	cond := conditions.GetCondition(glance.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("SecretStoreNotReady"))
}

func TestReconcileSecrets_DBCredentialsMissing(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	// Store ready, service-user Secret present, but the DB credentials Secret is
	// absent: the first gate fails.
	r := newGlanceTestReconciler(glance,
		readyClusterSecretStore(openBaoClusterStoreName),
		glanceServiceUserSecret())

	res, digest, err := r.reconcileSecrets(context.Background(), glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueSecretPolling))
	g.Expect(digest).To(BeEmpty())
	cond := conditions.GetCondition(glance.Status.Conditions, "SecretsReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForDBCredentials"))
}

func TestReconcileSecrets_ServiceUserKeyMissing(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	// DB Secret complete, but the service-user Secret carries the wrong data key.
	wrongKey := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "glance-service-user", Namespace: "default"},
		Data:       map[string][]byte{"not-password": []byte("svc-pw")},
	}
	r := newGlanceTestReconciler(glance,
		readyClusterSecretStore(openBaoClusterStoreName),
		glanceDBSecret(), wrongKey)

	res, digest, err := r.reconcileSecrets(context.Background(), glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueSecretPolling))
	g.Expect(digest).To(BeEmpty())
	cond := conditions.GetCondition(glance.Status.Conditions, "SecretsReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForServiceUserCredentials"))
}

func TestReconcileSecrets_AllPresentReturnsStableDigest(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	r := newGlanceTestReconciler(glance,
		readyClusterSecretStore(openBaoClusterStoreName),
		glanceDBSecret(), glanceServiceUserSecret())

	res, digest, err := r.reconcileSecrets(context.Background(), glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	// The digest is the SHA-256 of the service-user password and is stable.
	g.Expect(digest).To(Equal(secrets.AdminPasswordDigest("svc-pw")))

	cond := conditions.GetCondition(glance.Status.Conditions, "SecretsReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("SecretsAvailable"))

	// A second pass returns the identical digest (no dependency on iteration
	// order or wall-clock).
	_, digest2, err := r.reconcileSecrets(context.Background(), glance)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(digest2).To(Equal(digest))
}
