// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package tls

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	envtestutil "github.com/c5c3/forge/internal/common/testutil/envtest"
)

// Feature: CC-0005

// createNamespace creates a unique namespace in the cluster for test isolation.
func createNamespace(ctx context.Context, g Gomega, c client.Client, name string) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())
	return ns
}

// createOwner creates a ConfigMap via the API server so it gets a real UID assigned.
func createOwner(ctx context.Context, g Gomega, c client.Client, namespace string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-owner",
			Namespace: namespace,
		},
	}
	g.Expect(c.Create(ctx, cm)).To(Succeed())
	return cm
}

// ---------------------------------------------------------------------------
// TestIntegration_EnsureCertificate_CreatesInCluster
// ---------------------------------------------------------------------------

func TestIntegration_EnsureCertificate_CreatesInCluster(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := createNamespace(ctx, g, c, "test-cert-create")
	owner := createOwner(ctx, g, c, ns.Name)

	desired := &certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cert",
			Namespace: ns.Name,
		},
		Spec: certmanagerv1.CertificateSpec{
			SecretName: "test-cert-tls",
			IssuerRef: cmmeta.ObjectReference{
				Name: "letsencrypt",
				Kind: "ClusterIssuer",
			},
			DNSNames: []string{"example.com"},
		},
	}

	err := EnsureCertificate(ctx, c, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	// Verify the Certificate exists in the cluster.
	created := &certmanagerv1.Certificate{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "test-cert", Namespace: ns.Name}, created)).To(Succeed())

	// Verify spec correctness.
	g.Expect(created.Spec.SecretName).To(Equal("test-cert-tls"))
	g.Expect(created.Spec.IssuerRef.Name).To(Equal("letsencrypt"))
	g.Expect(created.Spec.IssuerRef.Kind).To(Equal("ClusterIssuer"))
	g.Expect(created.Spec.DNSNames).To(ConsistOf("example.com"))

	// Verify owner reference is set.
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("test-owner"))
	g.Expect(created.OwnerReferences[0].UID).To(Equal(owner.UID))
}

// ---------------------------------------------------------------------------
// TestIntegration_EnsureCertificate_UpdatesExisting
// ---------------------------------------------------------------------------

func TestIntegration_EnsureCertificate_UpdatesExisting(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := createNamespace(ctx, g, c, "test-cert-update")
	owner := createOwner(ctx, g, c, ns.Name)

	// Create initial Certificate.
	initial := &certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cert",
			Namespace: ns.Name,
		},
		Spec: certmanagerv1.CertificateSpec{
			SecretName: "test-cert-tls",
			IssuerRef: cmmeta.ObjectReference{
				Name: "letsencrypt",
				Kind: "ClusterIssuer",
			},
			DNSNames: []string{"example.com"},
		},
	}

	err := EnsureCertificate(ctx, c, owner, initial)
	g.Expect(err).NotTo(HaveOccurred())

	// Update the Certificate with a different issuer and DNS names.
	updated := &certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cert",
			Namespace: ns.Name,
		},
		Spec: certmanagerv1.CertificateSpec{
			SecretName: "updated-tls",
			IssuerRef: cmmeta.ObjectReference{
				Name: "letsencrypt-prod",
				Kind: "ClusterIssuer",
			},
			DNSNames: []string{"prod.example.com"},
		},
	}

	err = EnsureCertificate(ctx, c, owner, updated)
	g.Expect(err).NotTo(HaveOccurred())

	// Verify the Certificate was updated with the new values.
	result := &certmanagerv1.Certificate{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "test-cert", Namespace: ns.Name}, result)).To(Succeed())
	g.Expect(result.Spec.SecretName).To(Equal("updated-tls"))
	g.Expect(result.Spec.IssuerRef.Name).To(Equal("letsencrypt-prod"))
	g.Expect(result.Spec.DNSNames).To(ConsistOf("prod.example.com"))
}

// ---------------------------------------------------------------------------
// TestIntegration_GetTLSSecret_RetrievesSecret
// ---------------------------------------------------------------------------

func TestIntegration_GetTLSSecret_RetrievesSecret(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := createNamespace(ctx, g, c, "test-secret-get")

	// Create a TLS Secret manually (simulating what cert-manager would create).
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-tls-secret",
			Namespace: ns.Name,
		},
		Data: map[string][]byte{
			"tls.crt": []byte("cert-data"),
			"tls.key": []byte("key-data"),
		},
		Type: corev1.SecretTypeTLS,
	}
	g.Expect(c.Create(ctx, secret)).To(Succeed())

	// Retrieve the Secret using GetTLSSecret.
	retrieved, err := GetTLSSecret(ctx, c, ns.Name, "my-tls-secret")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(retrieved).NotTo(BeNil())
	g.Expect(retrieved.Data["tls.crt"]).To(Equal([]byte("cert-data")))
	g.Expect(retrieved.Data["tls.key"]).To(Equal([]byte("key-data")))
	g.Expect(retrieved.Type).To(Equal(corev1.SecretTypeTLS))
}

// ---------------------------------------------------------------------------
// TestIntegration_GetTLSSecret_ReturnsError_WhenNotFound
// ---------------------------------------------------------------------------

func TestIntegration_GetTLSSecret_ReturnsError_WhenNotFound(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	createNamespace(ctx, g, c, "test-secret-notfound")

	// Attempt to get a Secret that does not exist.
	secret, err := GetTLSSecret(ctx, c, "test-secret-notfound", "nonexistent-secret")
	g.Expect(err).To(HaveOccurred())
	g.Expect(secret).To(BeNil())
}
