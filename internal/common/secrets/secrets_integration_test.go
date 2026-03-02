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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/secrets"
	"github.com/c5c3/forge/internal/common/testutil/builders"
	testenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
	"github.com/c5c3/forge/internal/common/testutil/simulators"
)

var k8sClient client.Client

const testNamespace = "test-secrets"

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
// IsExternalSecretReady tests
// ---------------------------------------------------------------------------

func TestIsExternalSecretReady_TrueAfterSync(t *testing.T) {
	ctx := context.Background()
	name := "es-ready"

	targetData := map[string][]byte{
		"username": []byte("admin"),
		"password": []byte("s3cret"),
	}
	if err := simulators.SimulateExternalSecretSync(ctx, k8sClient, name, testNamespace, targetData); err != nil {
		t.Fatalf("SimulateExternalSecretSync returned error: %v", err)
	}

	ready, err := secrets.IsExternalSecretReady(ctx, k8sClient, name, testNamespace)
	if err != nil {
		t.Fatalf("IsExternalSecretReady returned error: %v", err)
	}
	if !ready {
		t.Fatal("expected IsExternalSecretReady to return true after sync, got false")
	}
}

func TestIsExternalSecretReady_FalseForNonExistent(t *testing.T) {
	ctx := context.Background()

	ready, err := secrets.IsExternalSecretReady(ctx, k8sClient, "does-not-exist", testNamespace)
	if err != nil {
		t.Fatalf("IsExternalSecretReady returned error: %v", err)
	}
	if ready {
		t.Fatal("expected IsExternalSecretReady to return false for non-existent ExternalSecret, got true")
	}
}

// ---------------------------------------------------------------------------
// IsSecretReady tests
// ---------------------------------------------------------------------------

func TestIsSecretReady_TrueForSecretWithData(t *testing.T) {
	ctx := context.Background()

	_, err := builders.NewSecretBuilder().
		WithName("secret-with-data").
		WithNamespace(testNamespace).
		WithData(map[string][]byte{"key": []byte("value")}).
		Create(ctx, k8sClient)
	if err != nil {
		t.Fatalf("failed to create Secret: %v", err)
	}

	ready, err := secrets.IsSecretReady(ctx, k8sClient, "secret-with-data", testNamespace)
	if err != nil {
		t.Fatalf("IsSecretReady returned error: %v", err)
	}
	if !ready {
		t.Fatal("expected IsSecretReady to return true for Secret with data, got false")
	}
}

func TestIsSecretReady_FalseForNonExistent(t *testing.T) {
	ctx := context.Background()

	ready, err := secrets.IsSecretReady(ctx, k8sClient, "no-such-secret", testNamespace)
	if err != nil {
		t.Fatalf("IsSecretReady returned error: %v", err)
	}
	if ready {
		t.Fatal("expected IsSecretReady to return false for non-existent Secret, got true")
	}
}

// ---------------------------------------------------------------------------
// GetSecretValue tests
// ---------------------------------------------------------------------------

func TestGetSecretValue_ReturnsCorrectValue(t *testing.T) {
	ctx := context.Background()

	_, err := builders.NewSecretBuilder().
		WithName("secret-for-get").
		WithNamespace(testNamespace).
		WithData(map[string][]byte{
			"db-password": []byte("super-secret"),
		}).
		Create(ctx, k8sClient)
	if err != nil {
		t.Fatalf("failed to create Secret: %v", err)
	}

	val, err := secrets.GetSecretValue(ctx, k8sClient, "secret-for-get", testNamespace, "db-password")
	if err != nil {
		t.Fatalf("GetSecretValue returned error: %v", err)
	}
	if val != "super-secret" {
		t.Fatalf("expected GetSecretValue to return %q, got %q", "super-secret", val)
	}
}

func TestGetSecretValue_ErrorForMissingKey(t *testing.T) {
	ctx := context.Background()

	_, err := builders.NewSecretBuilder().
		WithName("secret-missing-key").
		WithNamespace(testNamespace).
		WithData(map[string][]byte{
			"exists": []byte("yes"),
		}).
		Create(ctx, k8sClient)
	if err != nil {
		t.Fatalf("failed to create Secret: %v", err)
	}

	_, err = secrets.GetSecretValue(ctx, k8sClient, "secret-missing-key", testNamespace, "does-not-exist")
	if err == nil {
		t.Fatal("expected GetSecretValue to return error for missing key, got nil")
	}
}

func TestGetSecretValue_ErrorForNonExistentSecret(t *testing.T) {
	ctx := context.Background()

	_, err := secrets.GetSecretValue(ctx, k8sClient, "totally-missing", testNamespace, "any-key")
	if err == nil {
		t.Fatal("expected GetSecretValue to return error for non-existent Secret, got nil")
	}
}

// ---------------------------------------------------------------------------
// EnsurePushSecret tests
// ---------------------------------------------------------------------------

func TestEnsurePushSecret_CreatesWithCorrectSpec(t *testing.T) {
	ctx := context.Background()
	name := "ps-create"

	err := secrets.EnsurePushSecret(ctx, k8sClient, name, testNamespace, "my-store", "remote/key")
	if err != nil {
		t.Fatalf("EnsurePushSecret returned error: %v", err)
	}

	// Verify the PushSecret was created with the expected spec.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "external-secrets.io",
		Version: "v1alpha1",
		Kind:    "PushSecret",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: testNamespace}, obj); err != nil {
		t.Fatalf("failed to get PushSecret: %v", err)
	}

	// Check spec.secretStoreRefs
	storeRefs, found, err := unstructured.NestedSlice(obj.Object, "spec", "secretStoreRefs")
	if err != nil || !found {
		t.Fatalf("spec.secretStoreRefs not found or error: found=%v, err=%v", found, err)
	}
	if len(storeRefs) != 1 {
		t.Fatalf("expected 1 secretStoreRef, got %d", len(storeRefs))
	}
	ref, ok := storeRefs[0].(map[string]interface{})
	if !ok {
		t.Fatal("secretStoreRefs[0] is not a map")
	}
	if ref["name"] != "my-store" {
		t.Fatalf("expected secretStoreRef name %q, got %q", "my-store", ref["name"])
	}
	if ref["kind"] != "ClusterSecretStore" {
		t.Fatalf("expected secretStoreRef kind %q, got %q", "ClusterSecretStore", ref["kind"])
	}

	// Check spec.selector.secret.name
	selectorName, found, err := unstructured.NestedString(obj.Object, "spec", "selector", "secret", "name")
	if err != nil || !found {
		t.Fatalf("spec.selector.secret.name not found or error: found=%v, err=%v", found, err)
	}
	if selectorName != name {
		t.Fatalf("expected selector.secret.name %q, got %q", name, selectorName)
	}

	// Check spec.data
	data, found, err := unstructured.NestedSlice(obj.Object, "spec", "data")
	if err != nil || !found {
		t.Fatalf("spec.data not found or error: found=%v, err=%v", found, err)
	}
	if len(data) != 1 {
		t.Fatalf("expected 1 data entry, got %d", len(data))
	}
	dataEntry, ok := data[0].(map[string]interface{})
	if !ok {
		t.Fatal("data[0] is not a map")
	}
	match, ok := dataEntry["match"].(map[string]interface{})
	if !ok {
		t.Fatal("data[0].match is not a map")
	}
	if match["secretKey"] != "" {
		t.Fatalf("expected secretKey %q, got %q", "", match["secretKey"])
	}
	remoteRef, ok := match["remoteRef"].(map[string]interface{})
	if !ok {
		t.Fatal("data[0].match.remoteRef is not a map")
	}
	if remoteRef["remoteKey"] != "remote/key" {
		t.Fatalf("expected remoteKey %q, got %q", "remote/key", remoteRef["remoteKey"])
	}
}

func TestEnsurePushSecret_Idempotent(t *testing.T) {
	ctx := context.Background()
	name := "ps-idempotent"

	err := secrets.EnsurePushSecret(ctx, k8sClient, name, testNamespace, "my-store", "remote/key")
	if err != nil {
		t.Fatalf("first EnsurePushSecret call returned error: %v", err)
	}

	err = secrets.EnsurePushSecret(ctx, k8sClient, name, testNamespace, "my-store", "remote/key")
	if err != nil {
		t.Fatalf("second EnsurePushSecret call returned error: %v", err)
	}
}

func TestEnsurePushSecret_SetsOwnerReferences(t *testing.T) {
	ctx := context.Background()
	name := "ps-owner-refs"

	ownerRef := metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       "my-owner",
		UID:        types.UID("test-uid-12345"),
	}

	err := secrets.EnsurePushSecret(ctx, k8sClient, name, testNamespace, "my-store", "remote/key", ownerRef)
	if err != nil {
		t.Fatalf("EnsurePushSecret returned error: %v", err)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "external-secrets.io",
		Version: "v1alpha1",
		Kind:    "PushSecret",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: testNamespace}, obj); err != nil {
		t.Fatalf("failed to get PushSecret: %v", err)
	}

	refs := obj.GetOwnerReferences()
	if len(refs) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(refs))
	}
	if refs[0].Name != "my-owner" {
		t.Fatalf("expected owner reference name %q, got %q", "my-owner", refs[0].Name)
	}
	if refs[0].Kind != "ConfigMap" {
		t.Fatalf("expected owner reference kind %q, got %q", "ConfigMap", refs[0].Kind)
	}
	if refs[0].UID != types.UID("test-uid-12345") {
		t.Fatalf("expected owner reference UID %q, got %q", "test-uid-12345", refs[0].UID)
	}
}
