package secrets

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// pushSecretGVK is the GroupVersionKind for the external-secrets PushSecret CR.
var pushSecretGVK = schema.GroupVersionKind{
	Group:   "external-secrets.io",
	Version: "v1alpha1",
	Kind:    "PushSecret",
}

// EnsurePushSecret creates an external-secrets.io/v1alpha1 PushSecret custom
// resource that pushes the contents of the named Kubernetes Secret to a remote
// secret store via the specified ClusterSecretStore.
//
// The PushSecret spec references:
//   - spec.secretStoreRefs: array with one entry pointing to secretStoreName
//     (kind: ClusterSecretStore)
//   - spec.selector.secret.name: the source Secret to push (same as name)
//   - spec.data: array with one entry mapping all keys to remoteKey
//
// Owner references are set from the variadic owners parameter.
//
// The function is idempotent: if the PushSecret already exists it returns nil.
// (CC-0005)
func EnsurePushSecret(ctx context.Context, c client.Client, name, namespace, secretStoreName, remoteKey string, owners ...metav1.OwnerReference) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(pushSecretGVK)
	obj.SetName(name)
	obj.SetNamespace(namespace)

	if len(owners) > 0 {
		obj.SetOwnerReferences(owners)
	}

	// spec.secretStoreRefs
	storeRefs := []interface{}{
		map[string]interface{}{
			"name": secretStoreName,
			"kind": "ClusterSecretStore",
		},
	}
	if err := unstructured.SetNestedSlice(obj.Object, storeRefs, "spec", "secretStoreRefs"); err != nil {
		return fmt.Errorf("setting PushSecret spec.secretStoreRefs: %w", err)
	}

	// spec.selector.secret.name
	if err := unstructured.SetNestedField(obj.Object, name, "spec", "selector", "secret", "name"); err != nil {
		return fmt.Errorf("setting PushSecret spec.selector.secret.name: %w", err)
	}

	// spec.data
	data := []interface{}{
		map[string]interface{}{
			"match": map[string]interface{}{
				"secretKey": "",
				"remoteRef": map[string]interface{}{
					"remoteKey": remoteKey,
				},
			},
		},
	}
	if err := unstructured.SetNestedSlice(obj.Object, data, "spec", "data"); err != nil {
		return fmt.Errorf("setting PushSecret spec.data: %w", err)
	}

	if err := c.Create(ctx, obj); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating PushSecret %s/%s: %w", namespace, name, err)
	}

	return nil
}
