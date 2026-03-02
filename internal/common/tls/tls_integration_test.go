//go:build integration

package tls_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/testutil"
	testenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
	"github.com/c5c3/forge/internal/common/tls"
)

var k8sClient client.Client

func TestMain(m *testing.M) {
	_, c, teardown, err := testenvtest.SetupEnvTest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup envtest: %v\n", err)
		os.Exit(1)
	}
	k8sClient = c
	code := m.Run()
	teardown()
	os.Exit(code)
}

func TestEnsureCertificate_Creates(t *testing.T) {
	ctx := context.Background()
	ns := testutil.CreateTestNamespace(t, ctx, k8sClient, "test-tls-")

	err := tls.EnsureCertificate(ctx, k8sClient,
		"test-cert", ns.Name,
		[]string{"api.example.com", "internal.example.com"},
		"test-cert-tls",
		"letsencrypt-prod", "ClusterIssuer", "cert-manager.io",
	)
	if err != nil {
		t.Fatalf("EnsureCertificate returned error: %v", err)
	}

	// Verify CR exists with correct spec
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "cert-manager.io", Version: "v1", Kind: "Certificate",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-cert", Namespace: ns.Name}, obj); err != nil {
		t.Fatalf("getting Certificate: %v", err)
	}

	// Check secretName
	secretName, _, _ := unstructured.NestedString(obj.Object, "spec", "secretName")
	if secretName != "test-cert-tls" {
		t.Fatalf("expected secretName=test-cert-tls, got %s", secretName)
	}

	// Check dnsNames
	dnsNames, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "dnsNames")
	if len(dnsNames) != 2 || dnsNames[0] != "api.example.com" {
		t.Fatalf("unexpected dnsNames: %v", dnsNames)
	}

	// Check issuerRef (CC-0005: verify all three fields to catch regressions)
	issuerName, _, _ := unstructured.NestedString(obj.Object, "spec", "issuerRef", "name")
	if issuerName != "letsencrypt-prod" {
		t.Fatalf("expected issuerRef.name=letsencrypt-prod, got %s", issuerName)
	}

	issuerKind, _, _ := unstructured.NestedString(obj.Object, "spec", "issuerRef", "kind")
	if issuerKind != "ClusterIssuer" {
		t.Fatalf("expected issuerRef.kind=ClusterIssuer, got %s", issuerKind)
	}

	issuerGroup, _, _ := unstructured.NestedString(obj.Object, "spec", "issuerRef", "group")
	if issuerGroup != "cert-manager.io" {
		t.Fatalf("expected issuerRef.group=cert-manager.io, got %s", issuerGroup)
	}
}

func TestEnsureCertificate_Idempotent(t *testing.T) {
	ctx := context.Background()
	ns := testutil.CreateTestNamespace(t, ctx, k8sClient, "test-tls-")

	if err := tls.EnsureCertificate(ctx, k8sClient,
		"test-cert-idem", ns.Name,
		[]string{"api.example.com"},
		"test-cert-idem-tls",
		"letsencrypt-prod", "ClusterIssuer", "cert-manager.io",
	); err != nil {
		t.Fatalf("first call returned error: %v", err)
	}

	if err := tls.EnsureCertificate(ctx, k8sClient,
		"test-cert-idem", ns.Name,
		[]string{"api.example.com"},
		"test-cert-idem-tls",
		"letsencrypt-prod", "ClusterIssuer", "cert-manager.io",
	); err != nil {
		t.Fatalf("second call returned error: %v", err)
	}
}

func TestEnsureCertificate_OwnerReferences(t *testing.T) {
	ctx := context.Background()
	ns := testutil.CreateTestNamespace(t, ctx, k8sClient, "test-tls-")

	ownerRef := metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "my-deployment",
		UID:        "12345678-1234-1234-1234-123456789abc",
	}

	err := tls.EnsureCertificate(ctx, k8sClient,
		"test-cert-owner", ns.Name,
		[]string{"api.example.com"},
		"test-cert-owner-tls",
		"letsencrypt-prod", "ClusterIssuer", "cert-manager.io",
		ownerRef,
	)
	if err != nil {
		t.Fatalf("EnsureCertificate returned error: %v", err)
	}

	// Verify CR exists and has owner references
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "cert-manager.io", Version: "v1", Kind: "Certificate",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-cert-owner", Namespace: ns.Name}, obj); err != nil {
		t.Fatalf("getting Certificate: %v", err)
	}

	refs := obj.GetOwnerReferences()
	if len(refs) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(refs))
	}
	if refs[0].Name != "my-deployment" {
		t.Fatalf("expected owner reference name=my-deployment, got %s", refs[0].Name)
	}
	if refs[0].Kind != "Deployment" {
		t.Fatalf("expected owner reference kind=Deployment, got %s", refs[0].Kind)
	}
}

func TestGetTLSSecret_Exists(t *testing.T) {
	ctx := context.Background()
	ns := testutil.CreateTestNamespace(t, ctx, k8sClient, "test-tls-")

	// Pre-create a TLS Secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-tls-secret",
			Namespace: ns.Name,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte("fake-cert-data"),
			corev1.TLSPrivateKeyKey: []byte("fake-key-data"),
		},
	}
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatalf("creating Secret: %v", err)
	}

	got, err := tls.GetTLSSecret(ctx, k8sClient, ns.Name, "my-tls-secret")
	if err != nil {
		t.Fatalf("GetTLSSecret returned error: %v", err)
	}
	if got.Name != "my-tls-secret" {
		t.Fatalf("expected secret name=my-tls-secret, got %s", got.Name)
	}
	if string(got.Data[corev1.TLSCertKey]) != "fake-cert-data" {
		t.Fatalf("unexpected tls.crt data: %s", got.Data[corev1.TLSCertKey])
	}
}

func TestGetTLSSecret_NotFound(t *testing.T) {
	ctx := context.Background()
	ns := testutil.CreateTestNamespace(t, ctx, k8sClient, "test-tls-")

	_, err := tls.GetTLSSecret(ctx, k8sClient, ns.Name, "nonexistent-secret")
	if err == nil {
		t.Fatal("expected error for non-existent secret, got nil")
	}
}
