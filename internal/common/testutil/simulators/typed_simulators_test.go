// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package simulators

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTypedScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	_ = mariadbv1alpha1.AddToScheme(s)
	_ = esov1alpha1.AddToScheme(s)
	_ = certmanagerv1.AddToScheme(s)
	return s
}

// --- SimulateDatabaseReady ---

func TestSimulateDatabaseReady(t *testing.T) {
	g := NewGomegaWithT(t)

	db := &mariadbv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{Name: "test-db", Namespace: "default"},
	}

	c := fake.NewClientBuilder().
		WithScheme(newTypedScheme()).
		WithObjects(db).
		WithStatusSubresource(db).
		Build()

	err := SimulateDatabaseReady(context.Background(), c, client.ObjectKeyFromObject(db))
	g.Expect(err).NotTo(HaveOccurred())

	updated := &mariadbv1alpha1.Database{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(db), updated)).To(Succeed())
	g.Expect(updated.Status.Conditions).To(HaveLen(1))
	g.Expect(updated.Status.Conditions[0].Type).To(Equal("Ready"))
	g.Expect(updated.Status.Conditions[0].Status).To(Equal(metav1.ConditionTrue))
	g.Expect(updated.Status.Conditions[0].Reason).To(Equal("DatabaseReady"))
}

func TestSimulateDatabaseReady_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)

	db := &mariadbv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{Name: "test-db", Namespace: "default"},
	}

	c := fake.NewClientBuilder().
		WithScheme(newTypedScheme()).
		WithObjects(db).
		WithStatusSubresource(db).
		Build()

	ctx := context.Background()
	key := client.ObjectKeyFromObject(db)

	g.Expect(SimulateDatabaseReady(ctx, c, key)).To(Succeed())
	g.Expect(SimulateDatabaseReady(ctx, c, key)).To(Succeed())

	updated := &mariadbv1alpha1.Database{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())
	g.Expect(updated.Status.Conditions).To(HaveLen(1), "expected exactly 1 condition after two calls")
}

func TestSimulateDatabaseReady_notFound(t *testing.T) {
	g := NewGomegaWithT(t)

	c := fake.NewClientBuilder().
		WithScheme(newTypedScheme()).
		Build()

	key := client.ObjectKey{Name: "missing", Namespace: "default"}
	err := SimulateDatabaseReady(context.Background(), c, key)
	g.Expect(err).To(HaveOccurred())
}

// --- SimulateUserReady ---

func TestSimulateUserReady(t *testing.T) {
	g := NewGomegaWithT(t)

	user := &mariadbv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "test-user", Namespace: "default"},
	}

	c := fake.NewClientBuilder().
		WithScheme(newTypedScheme()).
		WithObjects(user).
		WithStatusSubresource(user).
		Build()

	err := SimulateUserReady(context.Background(), c, client.ObjectKeyFromObject(user))
	g.Expect(err).NotTo(HaveOccurred())

	updated := &mariadbv1alpha1.User{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(user), updated)).To(Succeed())
	g.Expect(updated.Status.Conditions).To(HaveLen(1))
	g.Expect(updated.Status.Conditions[0].Type).To(Equal("Ready"))
	g.Expect(updated.Status.Conditions[0].Status).To(Equal(metav1.ConditionTrue))
	g.Expect(updated.Status.Conditions[0].Reason).To(Equal("UserReady"))
}

func TestSimulateUserReady_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)

	user := &mariadbv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "test-user", Namespace: "default"},
	}

	c := fake.NewClientBuilder().
		WithScheme(newTypedScheme()).
		WithObjects(user).
		WithStatusSubresource(user).
		Build()

	ctx := context.Background()
	key := client.ObjectKeyFromObject(user)

	g.Expect(SimulateUserReady(ctx, c, key)).To(Succeed())
	g.Expect(SimulateUserReady(ctx, c, key)).To(Succeed())

	updated := &mariadbv1alpha1.User{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())
	g.Expect(updated.Status.Conditions).To(HaveLen(1), "expected exactly 1 condition after two calls")
}

func TestSimulateUserReady_notFound(t *testing.T) {
	g := NewGomegaWithT(t)

	c := fake.NewClientBuilder().
		WithScheme(newTypedScheme()).
		Build()

	key := client.ObjectKey{Name: "missing", Namespace: "default"}
	err := SimulateUserReady(context.Background(), c, key)
	g.Expect(err).To(HaveOccurred())
}

// --- SimulateGrantReady ---

func TestSimulateGrantReady(t *testing.T) {
	g := NewGomegaWithT(t)

	grant := &mariadbv1alpha1.Grant{
		ObjectMeta: metav1.ObjectMeta{Name: "test-grant", Namespace: "default"},
	}

	c := fake.NewClientBuilder().
		WithScheme(newTypedScheme()).
		WithObjects(grant).
		WithStatusSubresource(grant).
		Build()

	err := SimulateGrantReady(context.Background(), c, client.ObjectKeyFromObject(grant))
	g.Expect(err).NotTo(HaveOccurred())

	updated := &mariadbv1alpha1.Grant{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(grant), updated)).To(Succeed())
	g.Expect(updated.Status.Conditions).To(HaveLen(1))
	g.Expect(updated.Status.Conditions[0].Type).To(Equal("Ready"))
	g.Expect(updated.Status.Conditions[0].Status).To(Equal(metav1.ConditionTrue))
	g.Expect(updated.Status.Conditions[0].Reason).To(Equal("GrantReady"))
}

func TestSimulateGrantReady_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)

	grant := &mariadbv1alpha1.Grant{
		ObjectMeta: metav1.ObjectMeta{Name: "test-grant", Namespace: "default"},
	}

	c := fake.NewClientBuilder().
		WithScheme(newTypedScheme()).
		WithObjects(grant).
		WithStatusSubresource(grant).
		Build()

	ctx := context.Background()
	key := client.ObjectKeyFromObject(grant)

	g.Expect(SimulateGrantReady(ctx, c, key)).To(Succeed())
	g.Expect(SimulateGrantReady(ctx, c, key)).To(Succeed())

	updated := &mariadbv1alpha1.Grant{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())
	g.Expect(updated.Status.Conditions).To(HaveLen(1), "expected exactly 1 condition after two calls")
}

func TestSimulateGrantReady_notFound(t *testing.T) {
	g := NewGomegaWithT(t)

	c := fake.NewClientBuilder().
		WithScheme(newTypedScheme()).
		Build()

	key := client.ObjectKey{Name: "missing", Namespace: "default"}
	err := SimulateGrantReady(context.Background(), c, key)
	g.Expect(err).To(HaveOccurred())
}

// --- SimulatePushSecretSynced ---

func TestSimulatePushSecretSynced(t *testing.T) {
	g := NewGomegaWithT(t)

	ps := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-ps", Namespace: "default"},
	}

	c := fake.NewClientBuilder().
		WithScheme(newTypedScheme()).
		WithObjects(ps).
		WithStatusSubresource(ps).
		Build()

	err := SimulatePushSecretSynced(context.Background(), c, client.ObjectKeyFromObject(ps))
	g.Expect(err).NotTo(HaveOccurred())

	updated := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(ps), updated)).To(Succeed())
	g.Expect(updated.Status.RefreshTime.IsZero()).To(BeFalse())
	g.Expect(updated.Status.Conditions).To(HaveLen(1))
	g.Expect(updated.Status.Conditions[0].Type).To(Equal(esov1alpha1.PushSecretReady))
	g.Expect(updated.Status.Conditions[0].Status).To(Equal(corev1.ConditionTrue))
	g.Expect(updated.Status.Conditions[0].Reason).To(Equal("PushSecretSynced"))
}

func TestSimulatePushSecretSynced_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)

	ps := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-ps", Namespace: "default"},
	}

	c := fake.NewClientBuilder().
		WithScheme(newTypedScheme()).
		WithObjects(ps).
		WithStatusSubresource(ps).
		Build()

	ctx := context.Background()
	key := client.ObjectKeyFromObject(ps)

	g.Expect(SimulatePushSecretSynced(ctx, c, key)).To(Succeed())
	g.Expect(SimulatePushSecretSynced(ctx, c, key)).To(Succeed())

	updated := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())
	g.Expect(updated.Status.Conditions).To(HaveLen(1), "expected exactly 1 condition after two calls")
}

func TestSimulatePushSecretSynced_notFound(t *testing.T) {
	g := NewGomegaWithT(t)

	c := fake.NewClientBuilder().
		WithScheme(newTypedScheme()).
		Build()

	key := client.ObjectKey{Name: "missing", Namespace: "default"}
	err := SimulatePushSecretSynced(context.Background(), c, key)
	g.Expect(err).To(HaveOccurred())
}

// --- SimulateCertificateReady ---

func TestSimulateCertificateReady(t *testing.T) {
	g := NewGomegaWithT(t)

	cert := &certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cert", Namespace: "default"},
	}

	c := fake.NewClientBuilder().
		WithScheme(newTypedScheme()).
		WithObjects(cert).
		WithStatusSubresource(cert).
		Build()

	err := SimulateCertificateReady(context.Background(), c, client.ObjectKeyFromObject(cert))
	g.Expect(err).NotTo(HaveOccurred())

	updated := &certmanagerv1.Certificate{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(cert), updated)).To(Succeed())
	g.Expect(updated.Status.Conditions).To(HaveLen(1))
	g.Expect(updated.Status.Conditions[0].Type).To(Equal(certmanagerv1.CertificateConditionReady))
	g.Expect(updated.Status.Conditions[0].Status).To(Equal(cmmeta.ConditionTrue))
	g.Expect(updated.Status.Conditions[0].Reason).To(Equal("CertificateReady"))
	g.Expect(updated.Status.Conditions[0].LastTransitionTime).NotTo(BeNil())
}

func TestSimulateCertificateReady_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)

	cert := &certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cert", Namespace: "default"},
	}

	c := fake.NewClientBuilder().
		WithScheme(newTypedScheme()).
		WithObjects(cert).
		WithStatusSubresource(cert).
		Build()

	ctx := context.Background()
	key := client.ObjectKeyFromObject(cert)

	g.Expect(SimulateCertificateReady(ctx, c, key)).To(Succeed())
	g.Expect(SimulateCertificateReady(ctx, c, key)).To(Succeed())

	updated := &certmanagerv1.Certificate{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())
	g.Expect(updated.Status.Conditions).To(HaveLen(1), "expected exactly 1 condition after two calls")
}

func TestSimulateCertificateReady_notFound(t *testing.T) {
	g := NewGomegaWithT(t)

	c := fake.NewClientBuilder().
		WithScheme(newTypedScheme()).
		Build()

	key := client.ObjectKey{Name: "missing", Namespace: "default"}
	err := SimulateCertificateReady(context.Background(), c, key)
	g.Expect(err).To(HaveOccurred())
}
