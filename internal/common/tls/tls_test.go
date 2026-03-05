// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package tls

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Feature: CC-0005

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = certmanagerv1.AddToScheme(s)
	return s
}

func testClient(scheme *runtime.Scheme, objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func testOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-owner",
			Namespace: "default",
			UID:       types.UID("owner-uid-1234"),
		},
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
	}
}

func desiredCertificate(name, namespace, secretName, issuerName string) *certmanagerv1.Certificate {
	return &certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: certmanagerv1.CertificateSpec{
			SecretName: secretName,
			IssuerRef: cmmeta.ObjectReference{
				Name: issuerName,
				Kind: "ClusterIssuer",
			},
			DNSNames: []string{"example.com"},
		},
	}
}

// ---------------------------------------------------------------------------
// EnsureCertificate tests
// ---------------------------------------------------------------------------

func TestEnsureCertificate_createsCertificate(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := testScheme()
	c := testClient(scheme)
	ctx := context.Background()
	owner := testOwner()

	desired := desiredCertificate("test-cert", "default", "test-cert-tls", "letsencrypt")

	err := EnsureCertificate(ctx, c, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	// Verify the Certificate was created.
	created := &certmanagerv1.Certificate{}
	err = c.Get(ctx, client.ObjectKeyFromObject(desired), created)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created.Spec.SecretName).To(Equal("test-cert-tls"))
	g.Expect(created.Spec.IssuerRef.Name).To(Equal("letsencrypt"))
	g.Expect(created.Spec.IssuerRef.Kind).To(Equal("ClusterIssuer"))
	g.Expect(created.Spec.DNSNames).To(ConsistOf("example.com"))
}

func TestEnsureCertificate_setsOwnerReference(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := testScheme()
	c := testClient(scheme)
	ctx := context.Background()
	owner := testOwner()

	desired := desiredCertificate("test-cert", "default", "test-cert-tls", "letsencrypt")

	err := EnsureCertificate(ctx, c, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	created := &certmanagerv1.Certificate{}
	err = c.Get(ctx, client.ObjectKeyFromObject(desired), created)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("test-owner"))
	g.Expect(created.OwnerReferences[0].UID).To(Equal(types.UID("owner-uid-1234")))
}

func TestEnsureCertificate_updatesCertificate(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := testScheme()
	c := testClient(scheme)
	ctx := context.Background()
	owner := testOwner()

	// Create initial Certificate.
	initial := desiredCertificate("test-cert", "default", "test-cert-tls", "letsencrypt")
	err := EnsureCertificate(ctx, c, owner, initial)
	g.Expect(err).NotTo(HaveOccurred())

	// Update the Certificate with a different issuer and secret name.
	updated := desiredCertificate("test-cert", "default", "updated-tls", "letsencrypt-prod")
	err = EnsureCertificate(ctx, c, owner, updated)
	g.Expect(err).NotTo(HaveOccurred())

	// Verify the Certificate was updated.
	result := &certmanagerv1.Certificate{}
	err = c.Get(ctx, client.ObjectKeyFromObject(updated), result)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.Spec.SecretName).To(Equal("updated-tls"))
	g.Expect(result.Spec.IssuerRef.Name).To(Equal("letsencrypt-prod"))
}

// ---------------------------------------------------------------------------
// GetTLSSecret tests
// ---------------------------------------------------------------------------

func TestGetTLSSecret_returnsSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := testScheme()

	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-tls-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"tls.crt": []byte("cert-data"),
			"tls.key": []byte("key-data"),
		},
		Type: corev1.SecretTypeTLS,
	}
	c := testClient(scheme, existing)
	ctx := context.Background()

	secret, err := GetTLSSecret(ctx, c, "default", "my-tls-secret")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(secret).NotTo(BeNil())
	g.Expect(secret.Data["tls.crt"]).To(Equal([]byte("cert-data")))
	g.Expect(secret.Data["tls.key"]).To(Equal([]byte("key-data")))
	g.Expect(secret.Type).To(Equal(corev1.SecretTypeTLS))
}

func TestGetTLSSecret_returnsError_whenNotFound(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := testScheme()
	c := testClient(scheme)
	ctx := context.Background()

	secret, err := GetTLSSecret(ctx, c, "default", "nonexistent-secret")
	g.Expect(err).To(HaveOccurred())
	g.Expect(secret).To(BeNil())
}
