// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// dbTLSTestScheme registers core, Keystone, and cert-manager types so the
// fake client can persist the issued client Certificate (CC-0106, REQ-002).
func dbTLSTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = keystonev1alpha1.AddToScheme(s)
	_ = certmanagerv1.AddToScheme(s)
	return s
}

// dbTLSManagedKeystone returns a managed-mode Keystone CR (Database.ClusterRef
// set) with DB TLS enabled in the given mode.
func dbTLSManagedKeystone(mode string) *keystonev1alpha1.Keystone {
	ks := dbTLSBaseKeystone()
	ks.Spec.Database.ClusterRef = &corev1.LocalObjectReference{Name: "openstack-db"}
	ks.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Enabled:             true,
		Mode:                mode,
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-server-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "test-keystone-db-client"},
	}
	return ks
}

// dbTLSBaseKeystone returns a minimal Keystone CR shared by the DB-TLS tests.
func dbTLSBaseKeystone() *keystonev1alpha1.Keystone {
	return &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-keystone",
			Namespace:  "default",
			Generation: 7,
		},
		Spec: keystonev1alpha1.KeystoneSpec{
			Replicas: 1,
			Image:    commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
			Database: commonv1.DatabaseSpec{
				Database:  "keystone",
				SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"},
			},
		},
	}
}

func dbTLSReconciler(s *runtime.Scheme, objs ...client.Object) *KeystoneReconciler {
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &KeystoneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}
}

// expectDBTLSProjection asserts that the projected volume sources the
// expected ca/cert/key file names from the expected caBundle / clientCert
// Secret names (CC-0106, REQ-002, REQ-014). Centralising the assertion keeps
// every Deployment/Job builder test in lockstep with the helper.
func expectDBTLSProjection(g Gomega, projected *corev1.ProjectedVolumeSource, caBundleSecretName, clientCertSecretName string) {
	g.Expect(projected).NotTo(BeNil(), "Projected source must be set on db-tls Volume")
	g.Expect(projected.Sources).To(HaveLen(2),
		"Projected sources must reference the CA bundle Secret and the client-cert Secret (CC-0106)")

	caSrc := projected.Sources[0].Secret
	g.Expect(caSrc).NotTo(BeNil(),
		"first Projected source must be the caBundleSecretRef Secret (CC-0106)")
	g.Expect(caSrc.Name).To(Equal(caBundleSecretName),
		"caBundleSecretRef.Name must be honored verbatim (CC-0106, REQ-014)")
	g.Expect(caSrc.Items).To(ConsistOf(corev1.KeyToPath{Key: "ca.crt", Path: "ca.crt"}),
		"caBundleSecretRef must contribute only ca.crt (CC-0106, REQ-014)")

	clientSrc := projected.Sources[1].Secret
	g.Expect(clientSrc).NotTo(BeNil(),
		"second Projected source must be the clientCertSecretRef Secret (CC-0106)")
	g.Expect(clientSrc.Name).To(Equal(clientCertSecretName),
		"clientCertSecretRef.Name must be honored verbatim (CC-0106, REQ-014)")
	g.Expect(clientSrc.Items).To(ConsistOf(
		corev1.KeyToPath{Key: "tls.crt", Path: "tls.crt"},
		corev1.KeyToPath{Key: "tls.key", Path: "tls.key"},
	), "clientCertSecretRef must contribute tls.crt and tls.key (CC-0106, REQ-014)")
}

// TestReconcileDatabaseTLS_CreatesCertificateWhenEnabled verifies REQ-002:
// managed-mode + TLS enabled provisions a cert-manager Certificate named
// "<name>-db-client" owned by the Keystone CR.
func TestReconcileDatabaseTLS_CreatesCertificateWhenEnabled(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTLSTestScheme()
	ks := dbTLSManagedKeystone("verify-full")
	r := dbTLSReconciler(s, ks)

	result, err := r.reconcileDatabaseTLS(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	// Newly created Certificate is not yet Ready, so the reconciler requeues.
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretPolling))

	cert := &certmanagerv1.Certificate{}
	g.Expect(r.Get(context.Background(), client.ObjectKey{Name: "test-keystone-db-client", Namespace: "default"}, cert)).To(Succeed())
	g.Expect(cert.Spec.SecretName).To(Equal("test-keystone-db-client"))
	g.Expect(cert.Spec.IssuerRef.Name).To(Equal(dbCAIssuerName))
	g.Expect(cert.Spec.IssuerRef.Kind).To(Equal("ClusterIssuer"))
	g.Expect(cert.Spec.Usages).To(ContainElement(certmanagerv1.UsageClientAuth))
	g.Expect(cert.OwnerReferences).To(HaveLen(1))
	g.Expect(cert.OwnerReferences[0].Name).To(Equal("test-keystone"))

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypeDatabaseTLSReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonCertificatePending))
	g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
}

// TestReconcileDatabaseTLS_ConditionTrueWhenIssued verifies REQ-002: once
// cert-manager marks the Certificate Ready, the condition flips to
// True/CertificateIssued and the reconciler stops requeuing.
func TestReconcileDatabaseTLS_ConditionTrueWhenIssued(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTLSTestScheme()
	ks := dbTLSManagedKeystone("verify-ca")

	issued := &certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-db-client", Namespace: "default"},
		Spec: certmanagerv1.CertificateSpec{
			SecretName: "test-keystone-db-client",
			CommonName: "test-keystone",
			IssuerRef:  cmmeta.IssuerReference{Name: dbCAIssuerName, Kind: "ClusterIssuer", Group: "cert-manager.io"},
			Usages: []certmanagerv1.KeyUsage{
				certmanagerv1.UsageClientAuth,
				certmanagerv1.UsageDigitalSignature,
				certmanagerv1.UsageKeyEncipherment,
			},
		},
		Status: certmanagerv1.CertificateStatus{
			Conditions: []certmanagerv1.CertificateCondition{{
				Type:   certmanagerv1.CertificateConditionReady,
				Status: cmmeta.ConditionTrue,
			}},
		},
	}
	r := dbTLSReconciler(s, ks, issued)

	result, err := r.reconcileDatabaseTLS(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}))

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypeDatabaseTLSReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(reasonCertificateIssued))
	g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
}

// TestReconcileDatabaseTLS_DisabledIsNoOp verifies REQ-002: a nil TLS block
// creates no Certificate and records NotRequired.
func TestReconcileDatabaseTLS_DisabledIsNoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTLSTestScheme()
	ks := dbTLSBaseKeystone() // TLS == nil
	r := dbTLSReconciler(s, ks)

	result, err := r.reconcileDatabaseTLS(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}))

	cert := &certmanagerv1.Certificate{}
	getErr := r.Get(context.Background(), client.ObjectKey{Name: "test-keystone-db-client", Namespace: "default"}, cert)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(), "no Certificate must be created when TLS is disabled")

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypeDatabaseTLSReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(reasonNotRequired))
}

// TestReconcileDatabaseTLS_BrownfieldExternallyManaged verifies REQ-014: a
// brownfield database (Host set, no ClusterRef) with TLS enabled does not get
// an operator-issued Certificate; the keypair is externally managed.
func TestReconcileDatabaseTLS_BrownfieldExternallyManaged(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTLSTestScheme()
	ks := dbTLSBaseKeystone()
	ks.Spec.Database.Host = "db.example.com"
	ks.Spec.Database.Port = 3306
	ks.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Enabled:             true,
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-server-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "external-db-client"},
	}
	r := dbTLSReconciler(s, ks)

	result, err := r.reconcileDatabaseTLS(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}))

	cert := &certmanagerv1.Certificate{}
	getErr := r.Get(context.Background(), client.ObjectKey{Name: "test-keystone-db-client", Namespace: "default"}, cert)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(), "no Certificate for a brownfield database")

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypeDatabaseTLSReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(reasonExternallyManaged))
}
