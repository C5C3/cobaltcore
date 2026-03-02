package testutil

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CreateTestNamespace creates a namespace with GenerateName and registers
// cleanup via t.Cleanup. The prefix should identify the calling package
// (e.g. "test-deploy-", "test-secrets-"). (CC-0005)
func CreateTestNamespace(t *testing.T, ctx context.Context, c client.Client, prefix string) *corev1.Namespace {
	t.Helper()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: prefix,
		},
	}
	if err := c.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create test namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Delete(ctx, ns)
	})
	return ns
}
