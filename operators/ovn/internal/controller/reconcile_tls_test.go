// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	mctestutil "github.com/c5c3/cobaltcore/internal/common/testutil/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// The names of the three Certificates the step requests for the shared fixture,
// which runs no relay.
var testCertificateNames = []string{"ovn-nb-server", "ovn-sb-server", "ovn-client"}

// clientTLSSecret builds the Secret cert-manager writes the client keypair
// into, carrying only the named keys, so a test can leave one out.
func clientTLSSecret(keys ...string) *corev1.Secret {
	data := map[string][]byte{}
	for _, key := range keys {
		data[key] = []byte("material")
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ovn-client", Namespace: testNamespace},
		Data:       data,
	}
}

// issueCertificates runs one TLS pass and marks the three Certificates it
// requested as issued, which is what cert-manager does between two passes.
func issueCertificates(ctx context.Context, t *testing.T, r *OVNCentralReconciler, cr *ovnv1alpha1.OVNCentral) {
	t.Helper()

	if _, err := r.reconcileTLS(ctx, r.Client, cr); err != nil {
		t.Fatalf("requesting the certificates: %v", err)
	}
	for _, name := range testCertificateNames {
		if err := simulators.SimulateCertificateReady(ctx, r.Client, centralKey(name)); err != nil {
			t.Fatalf("marking Certificate %s ready: %v", name, err)
		}
	}
}

// spec.tls is required, so a cluster without cert-manager cannot serve an
// OVNCentral at all. The step says so and polls rather than applying a
// Certificate the API server would reject with "no matches for kind".
func TestReconcileTLS_WithoutCertManagerWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNCentral()
	r := newTestOVNCentralReconciler(t, cr)
	r.certManagerAvailable = false

	res, err := r.reconcileTLS(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	cond := ovnCentralCondition(cr, conditionTypeTLSReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonCertManagerUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("cert-manager is not installed"))

	g.Expect(apierrors.IsNotFound(r.Get(ctx, centralKey("ovn-client"), &certmanagerv1.Certificate{}))).
		To(BeTrue(), "no Certificate may be requested from a cluster that does not serve the kind")
}

// A probe that fails establishes nothing about the target cluster, so the pass
// fails rather than guessing. The condition has to move with it: every caller
// returns straight into the status write, whose Ready aggregation runs
// regardless of the reconcile error.
func TestReconcileTLS_ProbeFailureSurfacesCapabilityProbeFailed(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNCentral()
	cr.Generation = 4
	r := newTestOVNCentralReconciler(t, cr)

	_, err := r.reconcileTLS(ctx, mctestutil.UnprobeableChildren(r.Client), cr)

	g.Expect(err).To(MatchError(ContainSubstring("probing the target cluster for the Certificate kind")))

	cond := ovnCentralCondition(cr, conditionTypeTLSReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.CapabilityProbeFailed))
	g.Expect(cond.ObservedGeneration).To(Equal(cr.Generation))
}

// The first pass requests all three Certificates and waits for every one of
// them. Requesting them in one pass is what keeps a fresh install off one
// polling interval per certificate.
func TestReconcileTLS_RequestsAllThreeAndWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNCentral()
	r := newTestOVNCentralReconciler(t, cr)

	res, err := r.reconcileTLS(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	for _, name := range testCertificateNames {
		g.Expect(r.Get(ctx, centralKey(name), &certmanagerv1.Certificate{})).To(Succeed())
	}

	cond := ovnCentralCondition(cr, conditionTypeTLSReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonCertificatePending))
	g.Expect(cond.Message).To(Equal(
		"Waiting for cert-manager to issue ovn-nb-server, ovn-sb-server, ovn-client"))
	g.Expect(cr.Status.ClientSecretName).To(BeEmpty())
}

// Every Certificate carries the issuer the CR names, kind included. A hard-coded
// scope would send the request to an issuer that does not exist and leave the
// CR pending with nothing to point at.
func TestReconcileTLS_EveryCertificateCarriesTheSpecIssuerRef(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNCentral()
	cr.Spec.TLS.IssuerRef = ovnv1alpha1.OVNIssuerRef{Name: "tenant-ca", Kind: "Issuer"}
	r := newTestOVNCentralReconciler(t, cr)

	_, err := r.reconcileTLS(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	for _, name := range testCertificateNames {
		var cert certmanagerv1.Certificate
		g.Expect(r.Get(ctx, centralKey(name), &cert)).To(Succeed())
		g.Expect(cert.Spec.IssuerRef.Name).To(Equal("tenant-ca"))
		g.Expect(cert.Spec.IssuerRef.Kind).To(Equal("Issuer"))
		g.Expect(cert.Spec.IssuerRef.Group).To(Equal(certificateIssuerGroup))
	}
}

// cert-manager reports a Certificate Ready a moment before its Secret is
// readable. The step waits for the Secret too, because that is what the
// workloads mount and what status.clientSecretName points an OVNChassis at.
func TestReconcileTLS_ReadyCertificatesWithoutTheSecretStayPending(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNCentral()
	r := newTestOVNCentralReconciler(t, cr)
	issueCertificates(ctx, t, r, cr)

	res, err := r.reconcileTLS(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	cond := ovnCentralCondition(cr, conditionTypeTLSReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonCertificatePending))
	g.Expect(cond.Message).To(Equal("Waiting for cert-manager to write Secret ovn-client"))
	g.Expect(cr.Status.ClientSecretName).To(BeEmpty())
}

// An issuer that is not CA-backed issues a valid keypair and no ca.crt at all.
// OVN authenticates every peer against that CA, so the control plane cannot
// come up: the step names the cause and stops retrying, because only an edit to
// spec.tls.issuerRef changes the outcome.
func TestReconcileTLS_SecretWithoutCACertIsReportedAndNotRetried(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNCentral()
	r := newTestOVNCentralReconciler(t, cr, clientTLSSecret("tls.crt", "tls.key"))
	issueCertificates(ctx, t, r, cr)

	res, err := r.reconcileTLS(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred(), "a retry cannot make an issuer CA-backed")
	g.Expect(res.IsZero()).To(BeTrue(), "polling would repeat a verdict that cannot change on its own")

	cond := ovnCentralCondition(cr, conditionTypeTLSReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonCertificateError))
	g.Expect(cond.Message).To(Equal(
		"issuer ClusterIssuer/openstack-ovn-ca issued no ca.crt; spec.tls.issuerRef must name a CA issuer"))
	g.Expect(cr.Status.ClientSecretName).To(BeEmpty())
}

// A Secret missing the keypair itself reports the key it lacks rather than the
// issuer message, which would name the wrong cause.
func TestReconcileTLS_SecretWithoutTheKeypairNamesTheMissingKey(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNCentral()
	r := newTestOVNCentralReconciler(t, cr, clientTLSSecret("tls.key", "ca.crt"))
	issueCertificates(ctx, t, r, cr)

	res, err := r.reconcileTLS(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	cond := ovnCentralCondition(cr, conditionTypeTLSReady)
	g.Expect(cond.Reason).To(Equal(conditionReasonCertificateError))
	g.Expect(cond.Message).To(Equal("issued Secret ovn-client lacks tls.crt"))
}

func TestReconcileTLS_IssuedSecretPublishesTheClientSecretName(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNCentral()
	r := newTestOVNCentralReconciler(t, cr, clientTLSSecret("tls.crt", "tls.key", "ca.crt"))
	issueCertificates(ctx, t, r, cr)

	res, err := r.reconcileTLS(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "an issued certificate set is not polled")

	cond := ovnCentralCondition(cr, conditionTypeTLSReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonCertificatesIssued))
	g.Expect(cr.Status.ClientSecretName).To(Equal("ovn-client"),
		"an OVNChassis is pointed at the client material through this field")
}

// A target cluster that grants the operator no certificates verb fails here.
// The condition has to flip on that pass: the aggregate Ready is re-derived
// from the sub-conditions at the new observedGeneration, so a condition left
// untouched would report the failed pass as ready.
func TestReconcileTLS_EnsureFailureIsCertificateError(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNCentral()

	c := ovnCentralFakeClientBuilder(t, cr).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if cert, ok := obj.(*certmanagerv1.Certificate); ok {
					return apierrors.NewForbidden(certmanagerv1.Resource("certificates"), cert.Name, nil)
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	r := &OVNCentralReconciler{
		Client: c, Scheme: newTestScheme(t),
		Recorder: record.NewFakeRecorder(10), certManagerAvailable: true,
	}

	res, err := r.reconcileTLS(ctx, r.Client, cr)

	g.Expect(err).To(MatchError(ContainSubstring("ensuring Certificate ovn-nb-server")))
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue(), "the API error must stay unwrappable")
	g.Expect(res.IsZero()).To(BeTrue())

	cond := ovnCentralCondition(cr, conditionTypeTLSReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonCertificateError))
}

// A CR that runs relays gets a fourth Certificate, issued to the relay tier
// itself. Without it the relays would listen with the Southbound database's
// server keypair, which is the database's identity to everything that verifies
// the name it dialled.
func TestReconcileTLS_RelayGetsItsOwnCertificate(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNCentral()
	cr.Spec.Relay = &ovnv1alpha1.OVNRelaySpec{Replicas: 2}
	r := newTestOVNCentralReconciler(t, cr)

	_, err := r.reconcileTLS(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	cert := &certmanagerv1.Certificate{}
	g.Expect(r.Get(ctx, centralKey("ovn-sb-relay"), cert)).To(Succeed())
	g.Expect(cert.Spec.SecretName).To(Equal("ovn-sb-relay"))
	g.Expect(cert.Spec.CommonName).To(Equal("ovn-sb-relay"))
	g.Expect(cert.Spec.Usages).To(ContainElements(
		certmanagerv1.UsageServerAuth, certmanagerv1.UsageClientAuth),
		"ovn-ctl configures a relay with one keypair for its listener and its upstream")
	g.Expect(cert.Spec.DNSNames).To(ContainElement("ovn-sb-relay." + testNamespace + ".svc.cluster.local"))
}

// A CR without spec.relay requests nothing for a tier it does not run.
func TestReconcileTLS_NoRelayCertificateWithoutARelay(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNCentral()
	r := newTestOVNCentralReconciler(t, cr)

	_, err := r.reconcileTLS(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(apierrors.IsNotFound(r.Get(ctx, centralKey("ovn-sb-relay"), &certmanagerv1.Certificate{}))).
		To(BeTrue())
}
