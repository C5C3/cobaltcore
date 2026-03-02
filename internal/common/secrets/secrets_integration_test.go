//go:build integration

package secrets_test

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

	"github.com/c5c3/forge/internal/common/secrets"
	"github.com/c5c3/forge/internal/common/testutil"
	testenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
	"github.com/c5c3/forge/internal/common/testutil/simulators"
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

func createTestNamespace(t *testing.T, ctx context.Context) *corev1.Namespace {
	return testutil.CreateTestNamespace(t, ctx, k8sClient, "test-secrets-")
}

// ---------------------------------------------------------------------------
// IsExternalSecretReady
// ---------------------------------------------------------------------------

func TestIsExternalSecretReady_True(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	name := "ready-es"
	targetData := map[string][]byte{"key": []byte("value")}
	if err := simulators.SimulateExternalSecretSync(ctx, k8sClient, name, ns.Name, targetData); err != nil {
		t.Fatalf("SimulateExternalSecretSync: %v", err)
	}

	ready, err := secrets.IsExternalSecretReady(ctx, k8sClient, name, ns.Name)
	if err != nil {
		t.Fatalf("IsExternalSecretReady returned error: %v", err)
	}
	if !ready {
		t.Fatal("expected IsExternalSecretReady to return true, got false")
	}
}

func TestIsExternalSecretReady_NotReady(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "external-secrets.io",
		Version: "v1beta1",
		Kind:    "ExternalSecret",
	})
	obj.SetName("not-ready-es")
	obj.SetNamespace(ns.Name)
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("creating ExternalSecret: %v", err)
	}

	ready, err := secrets.IsExternalSecretReady(ctx, k8sClient, "not-ready-es", ns.Name)
	if err != nil {
		t.Fatalf("IsExternalSecretReady returned error: %v", err)
	}
	if ready {
		t.Fatal("expected IsExternalSecretReady to return false for bare ExternalSecret, got true")
	}
}

func TestIsExternalSecretReady_NotFound(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	ready, err := secrets.IsExternalSecretReady(ctx, k8sClient, "does-not-exist", ns.Name)
	if err != nil {
		t.Fatalf("IsExternalSecretReady returned error: %v", err)
	}
	if ready {
		t.Fatal("expected IsExternalSecretReady to return false for non-existent resource, got true")
	}
}

// ---------------------------------------------------------------------------
// IsSecretReady
// ---------------------------------------------------------------------------

func TestIsSecretReady_Exists(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-secret",
			Namespace: ns.Name,
		},
		Data: map[string][]byte{
			"key": []byte("value"),
		},
	}
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatalf("creating Secret: %v", err)
	}

	ready, err := secrets.IsSecretReady(ctx, k8sClient, "existing-secret", ns.Name)
	if err != nil {
		t.Fatalf("IsSecretReady returned error: %v", err)
	}
	if !ready {
		t.Fatal("expected IsSecretReady to return true for existing Secret, got false")
	}
}

func TestIsSecretReady_NotFound(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	ready, err := secrets.IsSecretReady(ctx, k8sClient, "does-not-exist", ns.Name)
	if err != nil {
		t.Fatalf("IsSecretReady returned error: %v", err)
	}
	if ready {
		t.Fatal("expected IsSecretReady to return false for non-existent Secret, got true")
	}
}

// ---------------------------------------------------------------------------
// GetSecretValue
// ---------------------------------------------------------------------------

func TestGetSecretValue_ReturnsValue(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: ns.Name,
		},
		Data: map[string][]byte{
			"password": []byte("s3cret"),
		},
	}
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatalf("creating Secret: %v", err)
	}

	val, err := secrets.GetSecretValue(ctx, k8sClient, "test-secret", ns.Name, "password")
	if err != nil {
		t.Fatalf("GetSecretValue returned error: %v", err)
	}
	if val != "s3cret" {
		t.Fatalf("expected GetSecretValue to return %q, got %q", "s3cret", val)
	}
}

func TestGetSecretValue_MissingKey(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "secret-missing-key",
			Namespace: ns.Name,
		},
		Data: map[string][]byte{
			"password": []byte("s3cret"),
		},
	}
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatalf("creating Secret: %v", err)
	}

	_, err := secrets.GetSecretValue(ctx, k8sClient, "secret-missing-key", ns.Name, "nonexistent")
	if err == nil {
		t.Fatal("expected GetSecretValue to return error for missing key, got nil")
	}
}

func TestGetSecretValue_MissingSecret(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	_, err := secrets.GetSecretValue(ctx, k8sClient, "does-not-exist", ns.Name, "key")
	if err == nil {
		t.Fatal("expected GetSecretValue to return error for missing Secret, got nil")
	}
}

// ---------------------------------------------------------------------------
// EnsurePushSecret
// ---------------------------------------------------------------------------

func TestEnsurePushSecret_Creates(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	spec := map[string]interface{}{
		"refreshInterval": "1h",
	}
	if err := secrets.EnsurePushSecret(ctx, k8sClient, "test-ps", ns.Name, spec); err != nil {
		t.Fatalf("EnsurePushSecret returned error: %v", err)
	}

	// Verify the PushSecret CR exists.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "external-secrets.io",
		Version: "v1alpha1",
		Kind:    "PushSecret",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-ps", Namespace: ns.Name}, obj); err != nil {
		t.Fatalf("failed to get PushSecret after creation: %v", err)
	}
}

func TestEnsurePushSecret_Idempotent(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	spec := map[string]interface{}{
		"refreshInterval": "1h",
	}

	if err := secrets.EnsurePushSecret(ctx, k8sClient, "idempotent-ps", ns.Name, spec); err != nil {
		t.Fatalf("first EnsurePushSecret returned error: %v", err)
	}

	if err := secrets.EnsurePushSecret(ctx, k8sClient, "idempotent-ps", ns.Name, spec); err != nil {
		t.Fatalf("second EnsurePushSecret returned error: %v", err)
	}
}

func TestEnsurePushSecret_OwnerReferences(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	spec := map[string]interface{}{
		"refreshInterval": "1h",
	}
	ownerRef := metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       "owner-cm",
		UID:        "fake-uid-12345",
	}

	if err := secrets.EnsurePushSecret(ctx, k8sClient, "owned-ps", ns.Name, spec, ownerRef); err != nil {
		t.Fatalf("EnsurePushSecret returned error: %v", err)
	}

	// Verify the PushSecret CR has the owner reference.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "external-secrets.io",
		Version: "v1alpha1",
		Kind:    "PushSecret",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "owned-ps", Namespace: ns.Name}, obj); err != nil {
		t.Fatalf("failed to get PushSecret: %v", err)
	}

	refs := obj.GetOwnerReferences()
	if len(refs) == 0 {
		t.Fatal("expected at least one ownerReference on PushSecret, got none")
	}
	found := false
	for _, ref := range refs {
		if ref.Name == "owner-cm" && ref.Kind == "ConfigMap" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected ownerReference with Name=owner-cm Kind=ConfigMap, got: %v", refs)
	}
}
