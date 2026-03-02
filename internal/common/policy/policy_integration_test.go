//go:build integration

package policy_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/policy"
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

func TestLoadPolicyFromConfigMap_ValidYAML(t *testing.T) {
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-policy-"}}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, ns)
	})

	policyYAML := "identity:list_users: \"role:admin\"\nidentity:get_user: \"role:admin or role:reader\"\n"
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "valid-policy",
			Namespace: ns.Name,
		},
		Data: map[string]string{
			"policy.yaml": policyYAML,
		},
	}
	if err := k8sClient.Create(ctx, cm); err != nil {
		t.Fatalf("creating ConfigMap: %v", err)
	}

	rules, err := policy.LoadPolicyFromConfigMap(ctx, k8sClient, ns.Name, "valid-policy")
	if err != nil {
		t.Fatalf("LoadPolicyFromConfigMap returned error: %v", err)
	}

	expected := map[string]string{
		"identity:list_users": "role:admin",
		"identity:get_user":   "role:admin or role:reader",
	}
	for k, v := range expected {
		if rules[k] != v {
			t.Fatalf("expected rules[%q]=%q, got %q", k, v, rules[k])
		}
	}
	if len(rules) != len(expected) {
		t.Fatalf("expected %d rules, got %d", len(expected), len(rules))
	}
}

func TestLoadPolicyFromConfigMap_MissingConfigMap(t *testing.T) {
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-policy-"}}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, ns)
	})

	_, err := policy.LoadPolicyFromConfigMap(ctx, k8sClient, ns.Name, "nonexistent-cm")
	if err == nil {
		t.Fatal("expected error for missing ConfigMap, got nil")
	}
}

func TestLoadPolicyFromConfigMap_MissingKey(t *testing.T) {
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-policy-"}}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, ns)
	})

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-policy-key",
			Namespace: ns.Name,
		},
		Data: map[string]string{
			"other.yaml": "key: value",
		},
	}
	if err := k8sClient.Create(ctx, cm); err != nil {
		t.Fatalf("creating ConfigMap: %v", err)
	}

	_, err := policy.LoadPolicyFromConfigMap(ctx, k8sClient, ns.Name, "no-policy-key")
	if err == nil {
		t.Fatal("expected error for missing policy.yaml key, got nil")
	}
	if !strings.Contains(err.Error(), "does not contain key \"policy.yaml\"") {
		t.Fatalf("expected error about missing key, got: %v", err)
	}
}

func TestLoadPolicyFromConfigMap_InvalidYAML(t *testing.T) {
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-policy-"}}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, ns)
	})

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "invalid-yaml",
			Namespace: ns.Name,
		},
		Data: map[string]string{
			"policy.yaml": "not: [valid: yaml: content",
		},
	}
	if err := k8sClient.Create(ctx, cm); err != nil {
		t.Fatalf("creating ConfigMap: %v", err)
	}

	_, err := policy.LoadPolicyFromConfigMap(ctx, k8sClient, ns.Name, "invalid-yaml")
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
	if !strings.Contains(err.Error(), "parsing policy.yaml from ConfigMap") {
		t.Fatalf("expected error about parsing policy.yaml, got: %v", err)
	}
}
