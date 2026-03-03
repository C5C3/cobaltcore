// A ConfigMap is born from data's gentle hand,
// its keys and values carefully planned.
// A hash is woven through its given name,
// so changed content never looks the same.
//
// Immutable it stands, steadfast and true,
// no careless patch may alter what it knew.
// Idempotent the call — create it twice,
// the cluster answers once, precise and nice.
//
// With owner references the bond is set,
// a parent's seal the child shall not forget.
// When garbage collection sweeps the floor,
// the orphaned map shall trouble us no more.
//
// So here we test, with envtest's gentle stage,
// each verse a case upon this integration page.

//go:build integration

package config_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/config"
	testenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
)

var k8sClient client.Client

const testNamespace = "test-config"

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

func TestCreateImmutableConfigMap_CreatesWithHashAndImmutable(t *testing.T) {
	ctx := context.Background()
	data := map[string]string{
		"keystone.conf": "[DEFAULT]\ndebug = true",
	}

	cm, err := config.CreateImmutableConfigMap(ctx, k8sClient, "my-config", testNamespace, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the name contains a hash suffix.
	if cm.Name == "my-config" {
		t.Fatalf("expected name to contain hash suffix, got %q", cm.Name)
	}
	if len(cm.Name) <= len("my-config-") {
		t.Fatalf("expected name to be longer than base name + dash, got %q", cm.Name)
	}

	// Verify immutability.
	if cm.Immutable == nil || !*cm.Immutable {
		t.Fatalf("expected ConfigMap to be immutable, got Immutable=%v", cm.Immutable)
	}

	// Verify data.
	if cm.Data["keystone.conf"] != data["keystone.conf"] {
		t.Fatalf("expected data[keystone.conf]=%q, got %q", data["keystone.conf"], cm.Data["keystone.conf"])
	}

	// Verify namespace.
	if cm.Namespace != testNamespace {
		t.Fatalf("expected namespace=%q, got %q", testNamespace, cm.Namespace)
	}

	// Clean up.
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cm)
	})
}

func TestCreateImmutableConfigMap_Idempotent(t *testing.T) {
	ctx := context.Background()
	data := map[string]string{
		"idempotent.conf": "value1",
	}

	cm1, err := config.CreateImmutableConfigMap(ctx, k8sClient, "idem-config", testNamespace, data)
	if err != nil {
		t.Fatalf("first call: unexpected error: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cm1)
	})

	cm2, err := config.CreateImmutableConfigMap(ctx, k8sClient, "idem-config", testNamespace, data)
	if err != nil {
		t.Fatalf("second call: unexpected error: %v", err)
	}

	if cm1.Name != cm2.Name {
		t.Fatalf("expected same name, got %q and %q", cm1.Name, cm2.Name)
	}
	if cm1.UID != cm2.UID {
		t.Fatalf("expected same UID (same object), got %q and %q", cm1.UID, cm2.UID)
	}
}

func TestCreateImmutableConfigMap_DifferentDataDifferentHash(t *testing.T) {
	ctx := context.Background()

	data1 := map[string]string{"key": "value1"}
	data2 := map[string]string{"key": "value2"}

	cm1, err := config.CreateImmutableConfigMap(ctx, k8sClient, "hash-config", testNamespace, data1)
	if err != nil {
		t.Fatalf("first create: unexpected error: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cm1)
	})

	cm2, err := config.CreateImmutableConfigMap(ctx, k8sClient, "hash-config", testNamespace, data2)
	if err != nil {
		t.Fatalf("second create: unexpected error: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cm2)
	})

	if cm1.Name == cm2.Name {
		t.Fatalf("expected different names for different data, both got %q", cm1.Name)
	}
}

func TestCreateImmutableConfigMap_OwnerReferencesSet(t *testing.T) {
	ctx := context.Background()
	data := map[string]string{"owner-test": "data"}

	owner := metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "my-deployment",
		UID:        "test-uid-12345",
	}

	cm, err := config.CreateImmutableConfigMap(ctx, k8sClient, "owned-config", testNamespace, data, owner)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cm)
	})

	if len(cm.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(cm.OwnerReferences))
	}
	if cm.OwnerReferences[0].Name != "my-deployment" {
		t.Fatalf("expected owner name %q, got %q", "my-deployment", cm.OwnerReferences[0].Name)
	}
	if cm.OwnerReferences[0].Kind != "Deployment" {
		t.Fatalf("expected owner kind %q, got %q", "Deployment", cm.OwnerReferences[0].Kind)
	}
}

func TestCreateImmutableConfigMap_EmptyData(t *testing.T) {
	ctx := context.Background()
	data := map[string]string{}

	cm, err := config.CreateImmutableConfigMap(ctx, k8sClient, "empty-config", testNamespace, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cm)
	})

	// Verify it was created with a hash suffix even for empty data.
	if cm.Name == "empty-config" {
		t.Fatalf("expected name with hash suffix, got %q", cm.Name)
	}

	if cm.Immutable == nil || !*cm.Immutable {
		t.Fatalf("expected ConfigMap to be immutable")
	}

	if len(cm.Data) != 0 {
		t.Fatalf("expected empty data map, got %v", cm.Data)
	}
}
