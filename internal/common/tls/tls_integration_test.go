// A certificate blooms where trust must be arranged,
// its secret name and issuer neatly exchanged.
// The ClusterIssuer signs what DNS names demand,
// and owner refs bind child to parent's hand.
//
// Idempotent the call — create it twice,
// the second pass returns without a price.
// GetTLSSecret reads the PEM from etcd's keep,
// both cert and key from guarded data deep.
//
// If fields are missing, errors rise with care,
// no silent gaps where bytes should fill the air.
// With envtest's forge we test each lock and key;
// in integration's light, the trust runs free.

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

	"github.com/c5c3/forge/internal/common/testutil/builders"
	testenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
	"github.com/c5c3/forge/internal/common/tls"
)

var k8sClient client.Client

const testNamespace = "test-tls"

var certificateGVK = schema.GroupVersionKind{
	Group:   "cert-manager.io",
	Version: "v1",
	Kind:    "Certificate",
}

func TestMain(m *testing.M) {
	_, c, teardown, err := testenvtest.SetupEnvTest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup envtest: %v\n", err)
		os.Exit(1)
	}
	k8sClient = c

	ctx := context.Background()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNamespace,
		},
	}
	if err := k8sClient.Create(ctx, ns); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create test namespace: %v\n", err)
		teardown()
		os.Exit(1)
	}

	code := m.Run()
	teardown()
	os.Exit(code)
}

// ---------------------------------------------------------------------------
// EnsureCertificate tests
// ---------------------------------------------------------------------------

func TestEnsureCertificate_CreatesWithCorrectSpec(t *testing.T) {
	ctx := context.Background()
	name := "test-cert-spec"
	issuerName := "my-cluster-issuer"
	commonName := "service.example.com"
	dnsNames := []string{"service.example.com", "service.svc.cluster.local"}

	err := tls.EnsureCertificate(ctx, k8sClient, name, testNamespace, issuerName, commonName, dnsNames)
	if err != nil {
		t.Fatalf("EnsureCertificate returned error: %v", err)
	}

	// Fetch the Certificate CR and verify spec fields.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(certificateGVK)
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: testNamespace}, obj); err != nil {
		t.Fatalf("failed to get Certificate %s/%s: %v", testNamespace, name, err)
	}

	// Verify spec.secretName
	secretName, found, err := unstructured.NestedString(obj.Object, "spec", "secretName")
	if err != nil {
		t.Fatalf("error reading spec.secretName: %v", err)
	}
	if !found {
		t.Fatal("spec.secretName not found")
	}
	if secretName != name+"-tls" {
		t.Fatalf("expected spec.secretName=%q, got %q", name+"-tls", secretName)
	}

	// Verify spec.commonName
	cn, found, err := unstructured.NestedString(obj.Object, "spec", "commonName")
	if err != nil {
		t.Fatalf("error reading spec.commonName: %v", err)
	}
	if !found {
		t.Fatal("spec.commonName not found")
	}
	if cn != commonName {
		t.Fatalf("expected spec.commonName=%q, got %q", commonName, cn)
	}

	// Verify spec.issuerRef
	irName, found, err := unstructured.NestedString(obj.Object, "spec", "issuerRef", "name")
	if err != nil {
		t.Fatalf("error reading spec.issuerRef.name: %v", err)
	}
	if !found {
		t.Fatal("spec.issuerRef.name not found")
	}
	if irName != issuerName {
		t.Fatalf("expected spec.issuerRef.name=%q, got %q", issuerName, irName)
	}

	irKind, found, err := unstructured.NestedString(obj.Object, "spec", "issuerRef", "kind")
	if err != nil {
		t.Fatalf("error reading spec.issuerRef.kind: %v", err)
	}
	if !found {
		t.Fatal("spec.issuerRef.kind not found")
	}
	if irKind != "ClusterIssuer" {
		t.Fatalf("expected spec.issuerRef.kind=%q, got %q", "ClusterIssuer", irKind)
	}

	// Verify spec.dnsNames
	fetchedDNS, found, err := unstructured.NestedStringSlice(obj.Object, "spec", "dnsNames")
	if err != nil {
		t.Fatalf("error reading spec.dnsNames: %v", err)
	}
	if !found {
		t.Fatal("spec.dnsNames not found")
	}
	if len(fetchedDNS) != len(dnsNames) {
		t.Fatalf("expected %d dnsNames, got %d", len(dnsNames), len(fetchedDNS))
	}
	for i, expected := range dnsNames {
		if fetchedDNS[i] != expected {
			t.Fatalf("dnsNames[%d]: expected %q, got %q", i, expected, fetchedDNS[i])
		}
	}
}

func TestEnsureCertificate_Idempotent(t *testing.T) {
	ctx := context.Background()
	name := "test-cert-idempotent"
	issuerName := "my-cluster-issuer"
	commonName := "idempotent.example.com"
	dnsNames := []string{"idempotent.example.com"}

	err := tls.EnsureCertificate(ctx, k8sClient, name, testNamespace, issuerName, commonName, dnsNames)
	if err != nil {
		t.Fatalf("first call to EnsureCertificate returned error: %v", err)
	}

	// Second call should return nil (AlreadyExists is swallowed).
	err = tls.EnsureCertificate(ctx, k8sClient, name, testNamespace, issuerName, commonName, dnsNames)
	if err != nil {
		t.Fatalf("second call to EnsureCertificate returned error: %v", err)
	}
}

func TestEnsureCertificate_SetsOwnerReferences(t *testing.T) {
	ctx := context.Background()
	name := "test-cert-ownerrefs"
	issuerName := "my-cluster-issuer"
	commonName := "owner.example.com"
	dnsNames := []string{"owner.example.com"}

	ownerRef := metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "my-deployment",
		UID:        "test-uid-12345",
	}

	err := tls.EnsureCertificate(ctx, k8sClient, name, testNamespace, issuerName, commonName, dnsNames, ownerRef)
	if err != nil {
		t.Fatalf("EnsureCertificate returned error: %v", err)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(certificateGVK)
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: testNamespace}, obj); err != nil {
		t.Fatalf("failed to get Certificate %s/%s: %v", testNamespace, name, err)
	}

	refs := obj.GetOwnerReferences()
	if len(refs) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(refs))
	}
	if refs[0].Name != "my-deployment" {
		t.Fatalf("expected owner reference name=%q, got %q", "my-deployment", refs[0].Name)
	}
	if refs[0].Kind != "Deployment" {
		t.Fatalf("expected owner reference kind=%q, got %q", "Deployment", refs[0].Kind)
	}
	if refs[0].APIVersion != "apps/v1" {
		t.Fatalf("expected owner reference apiVersion=%q, got %q", "apps/v1", refs[0].APIVersion)
	}
	if string(refs[0].UID) != "test-uid-12345" {
		t.Fatalf("expected owner reference uid=%q, got %q", "test-uid-12345", refs[0].UID)
	}
}

// ---------------------------------------------------------------------------
// GetTLSSecret tests
// ---------------------------------------------------------------------------

func TestGetTLSSecret_ReturnsCertAndKeyBytes(t *testing.T) {
	ctx := context.Background()
	name := "valid-tls-secret"

	_, err := builders.NewSecretBuilder().
		WithName(name).
		WithNamespace(testNamespace).
		WithData(map[string][]byte{
			"tls.crt": []byte("fake-cert-data"),
			"tls.key": []byte("fake-key-data"),
			"ca.crt":  []byte("fake-ca-data"),
		}).
		Create(ctx, k8sClient)
	if err != nil {
		t.Fatalf("failed to create test Secret: %v", err)
	}

	certPEM, keyPEM, err := tls.GetTLSSecret(ctx, k8sClient, name, testNamespace)
	if err != nil {
		t.Fatalf("GetTLSSecret returned error: %v", err)
	}
	if string(certPEM) != "fake-cert-data" {
		t.Fatalf("expected certPEM=%q, got %q", "fake-cert-data", string(certPEM))
	}
	if string(keyPEM) != "fake-key-data" {
		t.Fatalf("expected keyPEM=%q, got %q", "fake-key-data", string(keyPEM))
	}
}

func TestGetTLSSecret_ErrorForNonExistentSecret(t *testing.T) {
	ctx := context.Background()

	_, _, err := tls.GetTLSSecret(ctx, k8sClient, "does-not-exist", testNamespace)
	if err == nil {
		t.Fatal("expected error for non-existent Secret, got nil")
	}
}

func TestGetTLSSecret_ErrorForMissingTLSCrt(t *testing.T) {
	ctx := context.Background()
	name := "secret-no-crt"

	_, err := builders.NewSecretBuilder().
		WithName(name).
		WithNamespace(testNamespace).
		WithData(map[string][]byte{
			"tls.key": []byte("fake-key-data"),
		}).
		Create(ctx, k8sClient)
	if err != nil {
		t.Fatalf("failed to create test Secret: %v", err)
	}

	_, _, err = tls.GetTLSSecret(ctx, k8sClient, name, testNamespace)
	if err == nil {
		t.Fatal("expected error for Secret missing tls.crt, got nil")
	}
}

func TestGetTLSSecret_ErrorForMissingTLSKey(t *testing.T) {
	ctx := context.Background()
	name := "secret-no-key"

	_, err := builders.NewSecretBuilder().
		WithName(name).
		WithNamespace(testNamespace).
		WithData(map[string][]byte{
			"tls.crt": []byte("fake-cert-data"),
		}).
		Create(ctx, k8sClient)
	if err != nil {
		t.Fatalf("failed to create test Secret: %v", err)
	}

	_, _, err = tls.GetTLSSecret(ctx, k8sClient, name, testNamespace)
	if err == nil {
		t.Fatal("expected error for Secret missing tls.key, got nil")
	}
}
