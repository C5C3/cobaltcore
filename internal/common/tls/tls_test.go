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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = certmanagerv1.AddToScheme(s)
	return s
}

func testOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-owner",
			Namespace: "default",
			UID:       "test-uid",
		},
	}
}

func testCertificate() *certmanagerv1.Certificate {
	return &certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cert",
			Namespace: "default",
		},
		Spec: certmanagerv1.CertificateSpec{
			SecretName: "test-cert-tls",
			IssuerRef: cmmeta.IssuerReference{
				Name: "test-issuer",
				Kind: "ClusterIssuer",
			},
			DNSNames: []string{"test.example.com"},
		},
	}
}

// --- EnsureCertificate ---

func TestEnsureCertificate_creates(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build()

	ready, err := EnsureCertificate(context.Background(), c, s, owner, testCertificate())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "newly created certificate should not be ready")

	created := &certmanagerv1.Certificate{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-cert", Namespace: "default"}, created)).To(Succeed())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("test-owner"))
}

func TestEnsureCertificate_existingNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	cert := testCertificate()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, cert).
		WithStatusSubresource(cert).
		Build()

	ready, err := EnsureCertificate(context.Background(), c, s, owner, testCertificate())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
}

func TestEnsureCertificate_existingReady(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	now := metav1.Now()
	cert := testCertificate()
	cert.Status.Conditions = []certmanagerv1.CertificateCondition{
		{
			Type:               certmanagerv1.CertificateConditionReady,
			Status:             cmmeta.ConditionTrue,
			LastTransitionTime: &now,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, cert).
		WithStatusSubresource(cert).
		Build()

	ready, err := EnsureCertificate(context.Background(), c, s, owner, testCertificate())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
}

func TestEnsureCertificate_updates(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	existing := testCertificate()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		WithStatusSubresource(existing).
		Build()

	updated := testCertificate()
	updated.Spec.DNSNames = []string{"new.example.com"}

	ready, err := EnsureCertificate(context.Background(), c, s, owner, updated)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())

	fetched := &certmanagerv1.Certificate{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	g.Expect(fetched.Spec.DNSNames).To(Equal([]string{"new.example.com"}))
}

func TestEnsureCertificate_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build()

	ctx := context.Background()
	_, err := EnsureCertificate(ctx, c, s, owner, testCertificate())
	g.Expect(err).NotTo(HaveOccurred())
	_, err = EnsureCertificate(ctx, c, s, owner, testCertificate())
	g.Expect(err).NotTo(HaveOccurred())

	list := &certmanagerv1.CertificateList{}
	g.Expect(c.List(ctx, list, client.InNamespace("default"))).To(Succeed())
	g.Expect(list.Items).To(HaveLen(1))
}

// --- IsCertificateReady ---

func TestIsCertificateReady_true(t *testing.T) {
	g := NewGomegaWithT(t)
	now := metav1.Now()
	cert := &certmanagerv1.Certificate{
		Status: certmanagerv1.CertificateStatus{
			Conditions: []certmanagerv1.CertificateCondition{
				{
					Type:               certmanagerv1.CertificateConditionReady,
					Status:             cmmeta.ConditionTrue,
					LastTransitionTime: &now,
				},
			},
		},
	}
	g.Expect(IsCertificateReady(cert)).To(BeTrue())
}

func TestIsCertificateReady_false_noConditions(t *testing.T) {
	g := NewGomegaWithT(t)
	cert := &certmanagerv1.Certificate{}
	g.Expect(IsCertificateReady(cert)).To(BeFalse())
}

func TestIsCertificateReady_false_notTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	now := metav1.Now()
	cert := &certmanagerv1.Certificate{
		Status: certmanagerv1.CertificateStatus{
			Conditions: []certmanagerv1.CertificateCondition{
				{
					Type:               certmanagerv1.CertificateConditionReady,
					Status:             cmmeta.ConditionFalse,
					LastTransitionTime: &now,
				},
			},
		},
	}
	g.Expect(IsCertificateReady(cert)).To(BeFalse())
}
