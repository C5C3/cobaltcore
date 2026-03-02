package secrets

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IsExternalSecretReady fetches an ExternalSecret CR by name and namespace and
// inspects its status.conditions for a Ready=True condition.
//
// It returns (true, nil) if the ExternalSecret exists and has a Ready=True
// condition, (false, nil) if it does not exist or exists but is not yet ready,
// and (false, err) if the API call fails for any other reason. (CC-0005)
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
		return false, fmt.Errorf("reading ExternalSecret %s/%s status.conditions: %w", namespace, name, err)
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
