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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	testenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
	"github.com/c5c3/forge/internal/common/tls"
)

var (
	k8sClient client.Client
	testScheme *runtime.Scheme
)

const testNamespace = "test-tls"

func TestMain(m *testing.M) {
	_, c, teardown, err := testenvtest.SetupEnvTest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup envtest: %v\n", err)
		os.Exit(1)
	}
	k8sClient = c
	testScheme = c.Scheme()

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

// newOwner creates a ConfigMap to use as an owner object for controller references.
func newOwner(t *testing.T, ctx context.Context, name string) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
	}
	if err := k8sClient.Create(ctx, cm); err != nil {
		t.Fatalf("failed to create owner ConfigMap: %v", err)
	}
	// Re-fetch to populate UID and other server-set fields.
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(cm), cm); err != nil {
		t.Fatalf("failed to get owner ConfigMap: %v", err)
	}
	return cm
}

func TestEnsureCertificate_Creates(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(t, ctx, "owner-creates")

	opts := tls.CertificateOpts{
		Name:       "cert-creates",
		Namespace:  testNamespace,
		SecretName: "cert-creates-secret",
		IssuerRef: tls.IssuerRef{
			Name:  "my-issuer",
			Kind:  "ClusterIssuer",
			Group: "cert-manager.io",
		},
		DNSNames: []string{"example.com", "www.example.com"},
	}

	name, err := tls.EnsureCertificate(ctx, k8sClient, owner, testScheme, opts)
	if err != nil {
		t.Fatalf("EnsureCertificate returned error: %v", err)
	}
	if name != opts.Name {
		t.Fatalf("expected name %q, got %q", opts.Name, name)
	}

	// Verify the Certificate CR was created with correct spec fields.
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "cert-manager.io",
		Version: "v1",
		Kind:    "Certificate",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: opts.Name, Namespace: testNamespace}, cert); err != nil {
		t.Fatalf("failed to get Certificate: %v", err)
	}

	secretName, _, _ := unstructured.NestedString(cert.Object, "spec", "secretName")
	if secretName != opts.SecretName {
		t.Fatalf("expected spec.secretName %q, got %q", opts.SecretName, secretName)
	}

	issuerName, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "name")
	if issuerName != opts.IssuerRef.Name {
		t.Fatalf("expected spec.issuerRef.name %q, got %q", opts.IssuerRef.Name, issuerName)
	}

	issuerKind, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "kind")
	if issuerKind != opts.IssuerRef.Kind {
		t.Fatalf("expected spec.issuerRef.kind %q, got %q", opts.IssuerRef.Kind, issuerKind)
	}

	issuerGroup, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "group")
	if issuerGroup != opts.IssuerRef.Group {
		t.Fatalf("expected spec.issuerRef.group %q, got %q", opts.IssuerRef.Group, issuerGroup)
	}

	dnsNames, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "dnsNames")
	if len(dnsNames) != len(opts.DNSNames) {
		t.Fatalf("expected %d dnsNames, got %d", len(opts.DNSNames), len(dnsNames))
	}
	for i, expected := range opts.DNSNames {
		if dnsNames[i] != expected {
			t.Fatalf("dnsNames[%d]: expected %q, got %q", i, expected, dnsNames[i])
		}
	}
}

func TestEnsureCertificate_Idempotent(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(t, ctx, "owner-idempotent")

	opts := tls.CertificateOpts{
		Name:       "cert-idempotent",
		Namespace:  testNamespace,
		SecretName: "cert-idempotent-secret",
		IssuerRef: tls.IssuerRef{
			Name:  "my-issuer",
			Kind:  "ClusterIssuer",
			Group: "cert-manager.io",
		},
		DNSNames: []string{"idempotent.example.com"},
	}

	if _, err := tls.EnsureCertificate(ctx, k8sClient, owner, testScheme, opts); err != nil {
		t.Fatalf("first call to EnsureCertificate returned error: %v", err)
	}

	if _, err := tls.EnsureCertificate(ctx, k8sClient, owner, testScheme, opts); err != nil {
		t.Fatalf("second call to EnsureCertificate returned error: %v", err)
	}

	// Verify spec fields are unchanged after idempotent call.
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "cert-manager.io",
		Version: "v1",
		Kind:    "Certificate",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: opts.Name, Namespace: testNamespace}, cert); err != nil {
		t.Fatalf("failed to get Certificate after idempotent call: %v", err)
	}
	secretName, _, _ := unstructured.NestedString(cert.Object, "spec", "secretName")
	if secretName != opts.SecretName {
		t.Fatalf("spec.secretName changed after idempotent call: expected %q, got %q", opts.SecretName, secretName)
	}
	issuerName, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "name")
	if issuerName != opts.IssuerRef.Name {
		t.Fatalf("spec.issuerRef.name changed after idempotent call: expected %q, got %q", opts.IssuerRef.Name, issuerName)
	}
}

func TestEnsureCertificate_OwnerRef(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(t, ctx, "owner-ref")

	opts := tls.CertificateOpts{
		Name:       "cert-ownerref",
		Namespace:  testNamespace,
		SecretName: "cert-ownerref-secret",
		IssuerRef: tls.IssuerRef{
			Name:  "my-issuer",
			Kind:  "ClusterIssuer",
			Group: "cert-manager.io",
		},
		DNSNames: []string{"ownerref.example.com"},
	}

	if _, err := tls.EnsureCertificate(ctx, k8sClient, owner, testScheme, opts); err != nil {
		t.Fatalf("EnsureCertificate returned error: %v", err)
	}

	// Verify owner reference is set on the Certificate.
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "cert-manager.io",
		Version: "v1",
		Kind:    "Certificate",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: opts.Name, Namespace: testNamespace}, cert); err != nil {
		t.Fatalf("failed to get Certificate: %v", err)
	}

	ownerRefs := cert.GetOwnerReferences()
	if len(ownerRefs) == 0 {
		t.Fatal("expected at least one owner reference, got none")
	}

	found := false
	for _, ref := range ownerRefs {
		if ref.UID == owner.UID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected owner reference with UID %s, not found in %v", owner.UID, ownerRefs)
	}
}

func TestGetTLSSecret_NotFound(t *testing.T) {
	ctx := context.Background()

	_, _, err := tls.GetTLSSecret(ctx, k8sClient, "nonexistent-secret", testNamespace)
	if err == nil {
		t.Fatal("expected error for non-existent secret, got nil")
	}
}

func TestGetTLSSecret_MissingKeys(t *testing.T) {
	ctx := context.Background()

	// Create a Secret without tls.crt/tls.key.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "secret-missing-keys",
			Namespace: testNamespace,
		},
		Data: map[string][]byte{
			"other-key": []byte("some-data"),
		},
	}
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatalf("failed to create test secret: %v", err)
	}

	_, _, err := tls.GetTLSSecret(ctx, k8sClient, "secret-missing-keys", testNamespace)
	if err == nil {
		t.Fatal("expected error for secret with missing keys, got nil")
	}
}

func TestGetTLSSecret_Success(t *testing.T) {
	ctx := context.Background()

	expectedCert := []byte("fake-cert-data")
	expectedKey := []byte("fake-key-data")

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "secret-success",
			Namespace: testNamespace,
		},
		Data: map[string][]byte{
			"tls.crt": expectedCert,
			"tls.key": expectedKey,
		},
	}
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatalf("failed to create test secret: %v", err)
	}

	cert, key, err := tls.GetTLSSecret(ctx, k8sClient, "secret-success", testNamespace)
	if err != nil {
		t.Fatalf("GetTLSSecret returned error: %v", err)
	}

	if string(cert) != string(expectedCert) {
		t.Fatalf("expected cert %q, got %q", expectedCert, cert)
	}
	if string(key) != string(expectedKey) {
		t.Fatalf("expected key %q, got %q", expectedKey, key)
	}
}
