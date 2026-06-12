// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package tls

import (
	"testing"

	. "github.com/onsi/gomega"

	envtestutil "github.com/c5c3/forge/internal/common/testutil/envtest"
	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestIntegration_EnsureCertificate(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-tls-ensure"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cert-owner", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	cert := &certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "integration-cert",
			Namespace: ns.Name,
		},
		Spec: certmanagerv1.CertificateSpec{
			SecretName: "integration-cert-tls",
			IssuerRef: cmmeta.IssuerReference{
				Name: "test-issuer",
				Kind: "ClusterIssuer",
			},
			DNSNames: []string{"test.example.com"},
		},
	}

	// Create.
	ready, err := EnsureCertificate(ctx, c, scheme, owner, cert)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "newly created certificate should not be ready")

	created := &certmanagerv1.Certificate{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(cert), created)).To(Succeed())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("cert-owner"))

	// Update DNS names.
	updated := cert.DeepCopy()
	updated.Spec.DNSNames = []string{"new.example.com"}
	ready, err = EnsureCertificate(ctx, c, scheme, owner, updated)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())

	fetched := &certmanagerv1.Certificate{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(cert), fetched)).To(Succeed())
	g.Expect(fetched.Spec.DNSNames).To(Equal([]string{"new.example.com"}))
}

func TestIntegration_EnsureCertificate_idempotent(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-tls-idem"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cert-owner-idem", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	cert := &certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "idem-cert",
			Namespace: ns.Name,
		},
		Spec: certmanagerv1.CertificateSpec{
			SecretName: "idem-cert-tls",
			IssuerRef: cmmeta.IssuerReference{
				Name: "test-issuer",
				Kind: "ClusterIssuer",
			},
			DNSNames: []string{"test.example.com"},
		},
	}

	_, err := EnsureCertificate(ctx, c, scheme, owner, cert)
	g.Expect(err).NotTo(HaveOccurred())
	_, err = EnsureCertificate(ctx, c, scheme, owner, cert)
	g.Expect(err).NotTo(HaveOccurred())

	list := &certmanagerv1.CertificateList{}
	g.Expect(c.List(ctx, list, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(list.Items).To(HaveLen(1))
}
