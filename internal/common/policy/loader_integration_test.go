//go:build integration

package policy_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/policy"
	testenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
)

var k8sClient client.Client

const testNamespace = "test-policy"

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

func TestLoadPolicyFromConfigMap_Success(t *testing.T) {
	ctx := context.Background()

	policyYAML := "identity:get_user: \"role:admin\"\nidentity:list_users: \"role:admin or role:reader\"\n"

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "policy-success",
			Namespace: testNamespace,
		},
		Data: map[string]string{
			"policy.yaml": policyYAML,
		},
	}
	if err := k8sClient.Create(ctx, cm); err != nil {
		t.Fatalf("failed to create ConfigMap: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cm)
	})

	rules, err := policy.LoadPolicyFromConfigMap(ctx, k8sClient, "policy-success", testNamespace)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(rules))
	}
	if rules["identity:get_user"] != "role:admin" {
		t.Fatalf("expected identity:get_user=%q, got %q", "role:admin", rules["identity:get_user"])
	}
	if rules["identity:list_users"] != "role:admin or role:reader" {
		t.Fatalf("expected identity:list_users=%q, got %q", "role:admin or role:reader", rules["identity:list_users"])
	}
}

func TestLoadPolicyFromConfigMap_ConfigMapNotFound(t *testing.T) {
	ctx := context.Background()

	_, err := policy.LoadPolicyFromConfigMap(ctx, k8sClient, "nonexistent-cm", testNamespace)
	if err == nil {
		t.Fatal("expected error for nonexistent ConfigMap, got nil")
	}
}

func TestLoadPolicyFromConfigMap_MissingPolicyYAMLKey(t *testing.T) {
	ctx := context.Background()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "policy-no-key",
			Namespace: testNamespace,
		},
		Data: map[string]string{
			"other.yaml": "key: value",
		},
	}
	if err := k8sClient.Create(ctx, cm); err != nil {
		t.Fatalf("failed to create ConfigMap: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cm)
	})

	_, err := policy.LoadPolicyFromConfigMap(ctx, k8sClient, "policy-no-key", testNamespace)
	if err == nil {
		t.Fatal("expected error for missing policy.yaml key, got nil")
	}
}

func TestLoadPolicyFromConfigMap_InvalidYAML(t *testing.T) {
	ctx := context.Background()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "policy-invalid-yaml",
			Namespace: testNamespace,
		},
		Data: map[string]string{
			"policy.yaml": "{{invalid yaml: [not closed",
		},
	}
	if err := k8sClient.Create(ctx, cm); err != nil {
		t.Fatalf("failed to create ConfigMap: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cm)
	})

	_, err := policy.LoadPolicyFromConfigMap(ctx, k8sClient, "policy-invalid-yaml", testNamespace)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}

func TestLoadPolicyFromConfigMap_EmptyPolicy(t *testing.T) {
	ctx := context.Background()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "policy-empty",
			Namespace: testNamespace,
		},
		Data: map[string]string{
			"policy.yaml": "{}\n",
		},
	}
	if err := k8sClient.Create(ctx, cm); err != nil {
		t.Fatalf("failed to create ConfigMap: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cm)
	})

	rules, err := policy.LoadPolicyFromConfigMap(ctx, k8sClient, "policy-empty", testNamespace)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(rules) != 0 {
		t.Fatalf("expected empty map, got %v", rules)
	}
}
