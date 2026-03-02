//go:build integration

package config_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/config"
	"github.com/c5c3/forge/internal/common/testutil"
	testenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
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

func TestCreateImmutableConfigMap_CreatesWithHashSuffix(t *testing.T) {
	ctx := context.Background()
	ns := testutil.CreateTestNamespace(t, ctx, k8sClient, "test-config-")

	data := map[string]string{"key1": "value1", "key2": "value2"}
	generatedName, err := config.CreateImmutableConfigMap(ctx, k8sClient, "myconfig", ns.Name, data)
	if err != nil {
		t.Fatalf("CreateImmutableConfigMap returned error: %v", err)
	}

	// Verify name format: {name}-{8hexchars}
	if !strings.HasPrefix(generatedName, "myconfig-") {
		t.Fatalf("expected generated name to start with 'myconfig-', got %q", generatedName)
	}
	suffix := strings.TrimPrefix(generatedName, "myconfig-")
	if len(suffix) != 8 {
		t.Fatalf("expected 8 character hash suffix, got %q (len=%d)", suffix, len(suffix))
	}

	// Verify ConfigMap exists with correct properties.
	var cm corev1.ConfigMap
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: generatedName, Namespace: ns.Name}, &cm); err != nil {
		t.Fatalf("failed to get ConfigMap %s/%s: %v", ns.Name, generatedName, err)
	}

	if cm.Immutable == nil || !*cm.Immutable {
		t.Fatalf("expected ConfigMap to be immutable, got Immutable=%v", cm.Immutable)
	}

	for k, v := range data {
		if cm.Data[k] != v {
			t.Fatalf("expected ConfigMap data[%q]=%q, got %q", k, v, cm.Data[k])
		}
	}
}

func TestCreateImmutableConfigMap_DeterministicHash(t *testing.T) {
	ctx := context.Background()
	ns1 := testutil.CreateTestNamespace(t, ctx, k8sClient, "test-config-")
	ns2 := testutil.CreateTestNamespace(t, ctx, k8sClient, "test-config-")

	data := map[string]string{"alpha": "one", "beta": "two"}

	// Create in two separate namespaces to verify the hash suffix is derived
	// purely from data content, not from API server state. (CC-0005)
	name1, err := config.CreateImmutableConfigMap(ctx, k8sClient, "determ", ns1.Name, data)
	if err != nil {
		t.Fatalf("call in ns1 returned error: %v", err)
	}

	name2, err := config.CreateImmutableConfigMap(ctx, k8sClient, "determ", ns2.Name, data)
	if err != nil {
		t.Fatalf("call in ns2 returned error: %v", err)
	}

	// Same base name and same data must produce the same generated name,
	// proving the hash is deterministic and content-derived.
	if name1 != name2 {
		t.Fatalf("expected same generated name across namespaces, got %q and %q", name1, name2)
	}
}

func TestCreateImmutableConfigMap_DifferentDataDifferentHash(t *testing.T) {
	ctx := context.Background()
	ns := testutil.CreateTestNamespace(t, ctx, k8sClient, "test-config-")

	data1 := map[string]string{"key": "value1"}
	data2 := map[string]string{"key": "value2"}

	name1, err := config.CreateImmutableConfigMap(ctx, k8sClient, "diffhash", ns.Name, data1)
	if err != nil {
		t.Fatalf("first call returned error: %v", err)
	}

	name2, err := config.CreateImmutableConfigMap(ctx, k8sClient, "diffhash", ns.Name, data2)
	if err != nil {
		t.Fatalf("second call returned error: %v", err)
	}

	if name1 == name2 {
		t.Fatalf("expected different generated names for different data, both got %q", name1)
	}
}

func TestCreateImmutableConfigMap_Idempotent(t *testing.T) {
	ctx := context.Background()
	ns := testutil.CreateTestNamespace(t, ctx, k8sClient, "test-config-")

	data := map[string]string{"idem": "potent"}

	name1, err := config.CreateImmutableConfigMap(ctx, k8sClient, "idem", ns.Name, data)
	if err != nil {
		t.Fatalf("first call returned error: %v", err)
	}

	name2, err := config.CreateImmutableConfigMap(ctx, k8sClient, "idem", ns.Name, data)
	if err != nil {
		t.Fatalf("second call returned error: %v", err)
	}

	if name1 != name2 {
		t.Fatalf("expected same name on idempotent call, got %q and %q", name1, name2)
	}
}

func TestCreateImmutableConfigMap_OwnerReferences(t *testing.T) {
	ctx := context.Background()
	ns := testutil.CreateTestNamespace(t, ctx, k8sClient, "test-config-")

	data := map[string]string{"owner": "test"}
	ownerRef := metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       "fake-owner",
		UID:        "12345678-1234-1234-1234-123456789012",
	}

	generatedName, err := config.CreateImmutableConfigMap(ctx, k8sClient, "owned", ns.Name, data, ownerRef)
	if err != nil {
		t.Fatalf("CreateImmutableConfigMap returned error: %v", err)
	}

	var cm corev1.ConfigMap
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: generatedName, Namespace: ns.Name}, &cm); err != nil {
		t.Fatalf("failed to get ConfigMap %s/%s: %v", ns.Name, generatedName, err)
	}

	if len(cm.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(cm.OwnerReferences))
	}
	if cm.OwnerReferences[0].Name != "fake-owner" {
		t.Fatalf("expected owner reference name 'fake-owner', got %q", cm.OwnerReferences[0].Name)
	}
	if cm.OwnerReferences[0].UID != "12345678-1234-1234-1234-123456789012" {
		t.Fatalf("expected owner reference UID '12345678-1234-1234-1234-123456789012', got %q", cm.OwnerReferences[0].UID)
	}
}
