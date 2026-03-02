package secrets

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IsExternalSecretReady fetches an ExternalSecret CR via the unstructured client and returns
// true when a Ready=True condition exists. Returns (false, nil) if the ExternalSecret
// does not exist (NotFound). (CC-0005, REQ-002)
func IsExternalSecretReady(ctx context.Context, c client.Client, name, namespace string) (bool, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "external-secrets.io",
		Version: "v1beta1",
		Kind:    "ExternalSecret",
	})

	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting ExternalSecret %s/%s: %w", namespace, name, err)
	}

	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil {
		return false, fmt.Errorf("reading ExternalSecret status.conditions: %w", err)
	}
	if !found {
		return false, nil
	}

	for _, c := range conditions {
		condMap, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if condMap["type"] == "Ready" && condMap["status"] == "True" {
			return true, nil
		}
	}

	return false, nil
}

// IsSecretReady returns true if a core/v1 Secret with the given name exists.
// Returns (false, nil) if the Secret does not exist (NotFound). (CC-0005, REQ-003)
func IsSecretReady(ctx context.Context, c client.Client, name, namespace string) (bool, error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting Secret %s/%s: %w", namespace, name, err)
	}
	return true, nil
}

// GetSecretValue retrieves the value of a specific key from a Kubernetes Secret.
// The returned value may contain credentials — callers should not log it.
// Returns an error if the Secret or key does not exist. (CC-0005, REQ-003)
func GetSecretValue(ctx context.Context, c client.Client, name, namespace, key string) (string, error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, secret); err != nil {
		return "", fmt.Errorf("getting Secret %s/%s: %w", namespace, name, err)
	}

	val, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("key %q not found in Secret %s/%s", key, namespace, name)
	}

	return string(val), nil
}

// EnsurePushSecret creates a PushSecret CR (external-secrets.io/v1alpha1) via the unstructured
// client. The function is idempotent — if the PushSecret already exists, it returns nil.
// (CC-0005, REQ-003, REQ-009, REQ-010)
func EnsurePushSecret(ctx context.Context, c client.Client, name, namespace string, spec map[string]interface{}, ownerRefs ...metav1.OwnerReference) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "external-secrets.io",
		Version: "v1alpha1",
		Kind:    "PushSecret",
	})
	obj.SetName(name)
	obj.SetNamespace(namespace)

	if len(ownerRefs) > 0 {
		obj.SetOwnerReferences(ownerRefs)
	}

	if err := unstructured.SetNestedField(obj.Object, spec, "spec"); err != nil {
		return fmt.Errorf("setting PushSecret spec: %w", err)
	}

	err := c.Create(ctx, obj)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating PushSecret: %w", err)
	}
	return nil
}
