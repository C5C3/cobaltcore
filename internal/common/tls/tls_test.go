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
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/forge/internal/common/multicluster"
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

// TestEnsureCertificate_remoteCreatesLabelledAndUnowned pins the target-cluster
// path: the Certificate is stamped with the ownership labels and carries no
// owner reference, because a reference to a CR on another cluster names a UID
// this one cannot resolve.
func TestEnsureCertificate_remoteCreatesLabelledAndUnowned(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := multicluster.Remote(fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build())

	ready, err := EnsureCertificate(context.Background(), c, s, owner, testCertificate())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())

	created := &certmanagerv1.Certificate{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-cert", Namespace: "default"}, created)).To(Succeed())
	g.Expect(created.Labels).To(Equal(map[string]string{
		multicluster.OwnerKindLabel:      "ConfigMap",
		multicluster.OwnerNameLabel:      "test-owner",
		multicluster.OwnerNamespaceLabel: "default",
	}))
	g.Expect(created.OwnerReferences).To(BeEmpty(), "a remote child must carry no owner reference")
}

// TestEnsureCertificate_remoteRefusesAForeignCertificate is the update side of
// the claim. A Certificate of this name on a shared target cluster may well be
// somebody else's: rewriting its spec reissues it under this operator's issuer,
// dnsNames and secretName, and breaks the workload consuming the Secret it
// keeps renewing.
func TestEnsureCertificate_remoteRefusesAForeignCertificate(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	foreign := testCertificate()
	foreign.Spec.IssuerRef.Name = "someone-elses-issuer"
	foreign.Spec.SecretName = "someone-elses-tls"
	// Live: read back from the API server, which is what the claim recognizes.
	foreign.UID = "foreign-certificate-uid"

	c := multicluster.Remote(fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, foreign).
		Build())

	ready, err := EnsureCertificate(context.Background(), c, s, owner, testCertificate())

	g.Expect(err).To(MatchError(ContainSubstring("refusing to adopt pre-existing")))
	g.Expect(ready).To(BeFalse())

	untouched := &certmanagerv1.Certificate{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-cert", Namespace: "default"}, untouched)).To(Succeed())
	g.Expect(untouched.Spec.IssuerRef.Name).To(Equal("someone-elses-issuer"))
	g.Expect(untouched.Spec.SecretName).To(Equal("someone-elses-tls"))
	g.Expect(untouched.Labels).To(BeEmpty(), "a refused Certificate must not be marked for this owner's teardown")
}

// The other side of that recognition: a Certificate this owner does own is
// updated as it always was, and on a target cluster the claim also stamps the
// ownership labels the teardown selects on.
func TestEnsureCertificate_remoteUpdatesItsOwnCertificate(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	mine := testCertificate()
	mine.Spec.DNSNames = []string{"stale.example.com"}
	mine.UID = "my-certificate-uid"
	mine.Labels = map[string]string{
		multicluster.OwnerKindLabel:      "ConfigMap",
		multicluster.OwnerNameLabel:      "test-owner",
		multicluster.OwnerNamespaceLabel: "default",
	}

	c := multicluster.Remote(fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, mine).
		Build())

	_, err := EnsureCertificate(context.Background(), c, s, owner, testCertificate())
	g.Expect(err).NotTo(HaveOccurred())

	updated := &certmanagerv1.Certificate{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-cert", Namespace: "default"}, updated)).To(Succeed())
	g.Expect(updated.Spec.DNSNames).To(Equal([]string{"test.example.com"}))
	g.Expect(updated.OwnerReferences).To(BeEmpty(), "a remote child must carry no owner reference")
}

// TestEnsureCertificate_remoteClaimReachesAConvergedCertificate covers the
// Certificate this operator wrote before ownership moved off the owner
// reference: it carries a dangling reference to a CR on the management cluster,
// no ownership labels, and — being converged — the very spec the operator would
// write. The claim corrects the metadata in memory either way, so a spec-only
// update gate would discard that correction on every pass: the reference would
// stay, the target's garbage collector would collect the Certificate as an
// orphan once the owner's kind is registered there, and the teardown sweep would
// never see it.
func TestEnsureCertificate_remoteClaimReachesAConvergedCertificate(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	stale := testCertificate()
	stale.UID = "my-certificate-uid"
	stale.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       owner.Name,
		UID:        owner.UID,
		Controller: ptr.To(true),
	}}

	c := multicluster.Remote(fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, stale).
		Build())

	_, err := EnsureCertificate(context.Background(), c, s, owner, testCertificate())
	g.Expect(err).NotTo(HaveOccurred())

	claimed := &certmanagerv1.Certificate{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(stale), claimed)).To(Succeed())
	g.Expect(claimed.OwnerReferences).To(BeEmpty(),
		"the reference names a UID this cluster resolves to nothing, so it must reach the cluster dropped")
	g.Expect(claimed.Labels).To(Equal(map[string]string{
		multicluster.OwnerKindLabel:      "ConfigMap",
		multicluster.OwnerNameLabel:      "test-owner",
		multicluster.OwnerNamespaceLabel: "default",
	}), "the labels are what the teardown sweep selects on")
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

// --- isCertificateReady ---

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
	g.Expect(isCertificateReady(cert)).To(BeTrue())
}

func TestIsCertificateReady_false_noConditions(t *testing.T) {
	g := NewGomegaWithT(t)
	cert := &certmanagerv1.Certificate{}
	g.Expect(isCertificateReady(cert)).To(BeFalse())
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
	g.Expect(isCertificateReady(cert)).To(BeFalse())
}
