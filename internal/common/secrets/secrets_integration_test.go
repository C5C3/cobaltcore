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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/secrets"
	"github.com/c5c3/forge/internal/common/testutil/builders"
	testenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
	"github.com/c5c3/forge/internal/common/testutil/simulators"
)

var (
	k8sClient client.Client
	k8sScheme *runtime.Scheme
)

const testNamespace = "test-secrets"

func TestMain(m *testing.M) {
	_, c, teardown, err := testenvtest.SetupEnvTest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup envtest: %v\n", err)
		os.Exit(1)
	}
	k8sClient = c
	k8sScheme = c.Scheme()

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
// IsExternalSecretReady
// ---------------------------------------------------------------------------

func TestIsExternalSecretReady_NotFound(t *testing.T) {
	ctx := context.Background()

	ready, err := secrets.IsExternalSecretReady(ctx, k8sClient, "nonexistent-es", testNamespace)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if ready {
		t.Fatal("expected false for nonexistent ExternalSecret, got true")
	}
}

func TestIsExternalSecretReady_Ready(t *testing.T) {
	ctx := context.Background()
	name := "test-es-ready"

	targetData := map[string][]byte{"key": []byte("value")}
	if err := simulators.SimulateExternalSecretSync(ctx, k8sClient, name, testNamespace, targetData); err != nil {
		t.Fatalf("SimulateExternalSecretSync failed: %v", err)
	}

	ready, err := secrets.IsExternalSecretReady(ctx, k8sClient, name, testNamespace)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if !ready {
		t.Fatal("expected true for synced ExternalSecret, got false")
	}
}

// ---------------------------------------------------------------------------
// IsSecretReady
// ---------------------------------------------------------------------------

func TestIsSecretReady_NotFound(t *testing.T) {
	ctx := context.Background()

	ready, err := secrets.IsSecretReady(ctx, k8sClient, "nonexistent-secret", testNamespace)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if ready {
		t.Fatal("expected false for nonexistent Secret, got true")
	}
}

func TestIsSecretReady_Ready(t *testing.T) {
	ctx := context.Background()

	_, err := builders.NewSecretBuilder().
		WithName("test-secret-ready").
		WithNamespace(testNamespace).
		WithData(map[string][]byte{"user": []byte("admin")}).
		Create(ctx, k8sClient)
	if err != nil {
		t.Fatalf("failed to create Secret: %v", err)
	}

	ready, err := secrets.IsSecretReady(ctx, k8sClient, "test-secret-ready", testNamespace)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if !ready {
		t.Fatal("expected true for Secret with data, got false")
	}
}

func TestIsSecretReady_EmptyData(t *testing.T) {
	ctx := context.Background()

	_, err := builders.NewSecretBuilder().
		WithName("test-secret-empty").
		WithNamespace(testNamespace).
		Create(ctx, k8sClient)
	if err != nil {
		t.Fatalf("failed to create Secret: %v", err)
	}

	ready, err := secrets.IsSecretReady(ctx, k8sClient, "test-secret-empty", testNamespace)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if ready {
		t.Fatal("expected false for Secret with empty data, got true")
	}
}

// ---------------------------------------------------------------------------
// GetSecretValue
// ---------------------------------------------------------------------------

func TestGetSecretValue_NotFound(t *testing.T) {
	ctx := context.Background()

	_, err := secrets.GetSecretValue(ctx, k8sClient, "nonexistent-secret-val", testNamespace, "key")
	if err == nil {
		t.Fatal("expected error for nonexistent Secret, got nil")
	}
}

func TestGetSecretValue_KeyMissing(t *testing.T) {
	ctx := context.Background()

	_, err := builders.NewSecretBuilder().
		WithName("test-secret-keymissing").
		WithNamespace(testNamespace).
		WithData(map[string][]byte{"existing": []byte("value")}).
		Create(ctx, k8sClient)
	if err != nil {
		t.Fatalf("failed to create Secret: %v", err)
	}

	_, err = secrets.GetSecretValue(ctx, k8sClient, "test-secret-keymissing", testNamespace, "nonexistent")
	if err == nil {
		t.Fatal("expected error for missing key, got nil")
	}
}

func TestGetSecretValue_Success(t *testing.T) {
	ctx := context.Background()

	_, err := builders.NewSecretBuilder().
		WithName("test-secret-getval").
		WithNamespace(testNamespace).
		WithData(map[string][]byte{"password": []byte("s3cret")}).
		Create(ctx, k8sClient)
	if err != nil {
		t.Fatalf("failed to create Secret: %v", err)
	}

	val, err := secrets.GetSecretValue(ctx, k8sClient, "test-secret-getval", testNamespace, "password")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if val != "s3cret" {
		t.Fatalf("expected %q, got %q", "s3cret", val)
	}
}

// ---------------------------------------------------------------------------
// EnsurePushSecret
// ---------------------------------------------------------------------------

func TestEnsurePushSecret_Creates(t *testing.T) {
	ctx := context.Background()

	owner := createOwnerConfigMap(t, ctx, "ps-owner-create")

	opts := secrets.PushSecretOpts{
		Name:          "test-ps-create",
		Namespace:     testNamespace,
		SecretName:    "source-secret",
		SecretKeys:    []string{"user", "pass"},
		RemoteKeyBase: "secret/data/keystone",
		StoreRef: secrets.StoreRef{
			Name: "vault-store",
			Kind: "ClusterSecretStore",
		},
	}

	name, err := secrets.EnsurePushSecret(ctx, k8sClient, owner, k8sScheme, opts)
	if err != nil {
		t.Fatalf("EnsurePushSecret returned error: %v", err)
	}
	if name != opts.Name {
		t.Fatalf("expected name %q, got %q", opts.Name, name)
	}

	assertPushSecretSpec(t, ctx, opts)
}

func TestEnsurePushSecret_Idempotent(t *testing.T) {
	ctx := context.Background()

	owner := createOwnerConfigMap(t, ctx, "ps-owner-idempotent")

	opts := secrets.PushSecretOpts{
		Name:          "test-ps-idempotent",
		Namespace:     testNamespace,
		SecretName:    "source-secret",
		SecretKeys:    []string{"token"},
		RemoteKeyBase: "secret/data/keystone",
		StoreRef: secrets.StoreRef{
			Name: "vault-store",
			Kind: "ClusterSecretStore",
		},
	}

	if _, err := secrets.EnsurePushSecret(ctx, k8sClient, owner, k8sScheme, opts); err != nil {
		t.Fatalf("first call to EnsurePushSecret returned error: %v", err)
	}

	if _, err := secrets.EnsurePushSecret(ctx, k8sClient, owner, k8sScheme, opts); err != nil {
		t.Fatalf("second call to EnsurePushSecret returned error: %v", err)
	}

	// Verify the PushSecret still exists with correct spec.
	assertPushSecretExists(t, ctx, opts.Name)
}

func TestEnsurePushSecret_OwnerRef(t *testing.T) {
	ctx := context.Background()

	owner := createOwnerConfigMap(t, ctx, "ps-owner-ref")

	opts := secrets.PushSecretOpts{
		Name:          "test-ps-ownerref",
		Namespace:     testNamespace,
		SecretName:    "source-secret",
		SecretKeys:    []string{"key"},
		RemoteKeyBase: "secret/data/keystone",
		StoreRef: secrets.StoreRef{
			Name: "vault-store",
			Kind: "ClusterSecretStore",
		},
	}

	if _, err := secrets.EnsurePushSecret(ctx, k8sClient, owner, k8sScheme, opts); err != nil {
		t.Fatalf("EnsurePushSecret returned error: %v", err)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "external-secrets.io",
		Version: "v1alpha1",
		Kind:    "PushSecret",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: opts.Name, Namespace: testNamespace}, obj); err != nil {
		t.Fatalf("failed to get PushSecret: %v", err)
	}

	refs := obj.GetOwnerReferences()
	if len(refs) == 0 {
		t.Fatal("expected owner references, got none")
	}

	found := false
	for _, ref := range refs {
		if ref.Name == owner.Name && ref.Kind == "ConfigMap" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected owner reference to ConfigMap %q, not found in %v", owner.Name, refs)
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// createOwnerConfigMap creates a ConfigMap to serve as an owner for PushSecret tests.
func createOwnerConfigMap(t *testing.T, ctx context.Context, name string) *corev1.ConfigMap {
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

	// Re-read to populate UID and other server-set fields needed for owner references.
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: testNamespace}, cm); err != nil {
		t.Fatalf("failed to get owner ConfigMap: %v", err)
	}

	return cm
}

// assertPushSecretExists fetches the PushSecret and verifies it exists.
func assertPushSecretExists(t *testing.T, ctx context.Context, name string) {
	t.Helper()

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "external-secrets.io",
		Version: "v1alpha1",
		Kind:    "PushSecret",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: testNamespace}, obj); err != nil {
		t.Fatalf("failed to get PushSecret %s/%s: %v", testNamespace, name, err)
	}
}

// assertPushSecretSpec fetches the PushSecret and verifies its spec fields match the opts.
func assertPushSecretSpec(t *testing.T, ctx context.Context, opts secrets.PushSecretOpts) {
	t.Helper()

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "external-secrets.io",
		Version: "v1alpha1",
		Kind:    "PushSecret",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: opts.Name, Namespace: testNamespace}, obj); err != nil {
		t.Fatalf("failed to get PushSecret %s/%s: %v", testNamespace, opts.Name, err)
	}

	// Verify secretStoreRefs.
	storeRefs, found, err := unstructured.NestedSlice(obj.Object, "spec", "secretStoreRefs")
	if err != nil || !found {
		t.Fatalf("spec.secretStoreRefs not found: err=%v found=%v", err, found)
	}
	if len(storeRefs) != 1 {
		t.Fatalf("expected 1 secretStoreRef, got %d", len(storeRefs))
	}
	ref, ok := storeRefs[0].(map[string]interface{})
	if !ok {
		t.Fatal("secretStoreRef is not a map")
	}
	if ref["name"] != opts.StoreRef.Name {
		t.Fatalf("expected storeRef name %q, got %q", opts.StoreRef.Name, ref["name"])
	}
	if ref["kind"] != opts.StoreRef.Kind {
		t.Fatalf("expected storeRef kind %q, got %q", opts.StoreRef.Kind, ref["kind"])
	}

	// Verify selector.secret.name.
	secretName, found, err := unstructured.NestedString(obj.Object, "spec", "selector", "secret", "name")
	if err != nil || !found {
		t.Fatalf("spec.selector.secret.name not found: err=%v found=%v", err, found)
	}
	if secretName != opts.SecretName {
		t.Fatalf("expected selector secret name %q, got %q", opts.SecretName, secretName)
	}

	// Verify data entries count.
	data, found, err := unstructured.NestedSlice(obj.Object, "spec", "data")
	if err != nil || !found {
		t.Fatalf("spec.data not found: err=%v found=%v", err, found)
	}
	if len(data) != len(opts.SecretKeys) {
		t.Fatalf("expected %d data entries, got %d", len(opts.SecretKeys), len(data))
	}
}
