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
	"k8s.io/client-go/tools/record"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	"github.com/c5c3/cobaltcore/internal/common/testutil"
)

func TestReconcileSecrets_StoreNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	r := newGlanceTestReconciler(glance, notReadyClusterSecretStore(openBaoClusterStoreName))

	res, digest, err := r.reconcileSecrets(context.Background(), r.Client, glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
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

	res, digest, err := r.reconcileSecrets(context.Background(), r.Client, glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
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

	res, digest, err := r.reconcileSecrets(context.Background(), r.Client, glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
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

	res, digest, err := r.reconcileSecrets(context.Background(), r.Client, glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	// The digest is the SHA-256 of the service-user password and is stable.
	g.Expect(digest).To(Equal(secrets.AdminPasswordDigest("svc-pw")))

	cond := conditions.GetCondition(glance.Status.Conditions, "SecretsReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("SecretsAvailable"))

	// A second pass returns the identical digest (no dependency on iteration
	// order or wall-clock).
	_, digest2, err := r.reconcileSecrets(context.Background(), r.Client, glance)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(digest2).To(Equal(digest))
}

// TestReconcileSecrets_NamespaceScopedRefusesClusterStore pins the refusal a
// namespace-scoped operator gives a Glance that omits spec.secretStoreRef: its
// effective store is the cluster-scoped default, which a Role cannot grant, so
// the gate sets SecretsReady=False/ClusterSecretStoreUnsupported and requeues
// without reading the ClusterSecretStore at all.
func TestReconcileSecrets_NamespaceScopedRefusesClusterStore(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.SecretStoreRef = nil
	glance.Generation = 3
	c := glanceFakeClientBuilder(glance).
		WithInterceptorFuncs(testutil.ForbidClusterSecretStoreGet(t)).
		Build()
	r := &GlanceReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10), NamespaceScoped: true}

	res, digest, err := r.reconcileSecrets(context.Background(), r.Client, glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(digest).To(BeEmpty())
	cond := conditions.GetCondition(glance.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("ClusterSecretStoreUnsupported"))
	g.Expect(cond.ObservedGeneration).To(Equal(glance.Generation))
}
