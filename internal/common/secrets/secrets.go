package secrets

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// StoreRef identifies an external secret store.
type StoreRef struct {
	Name string
	Kind string // "ClusterSecretStore" or "SecretStore"
}

// PushSecretOpts configures the PushSecret to create.
type PushSecretOpts struct {
	Name          string
	Namespace     string
	SecretName    string   // source Secret name
	SecretKeys    []string // keys to push from the source Secret
	RemoteKeyBase string   // base path in external store (e.g. "secret/data/keystone")
	StoreRef      StoreRef // reference to ClusterSecretStore
}

// externalSecretGVK is the GroupVersionKind for ESO ExternalSecret resources.
var externalSecretGVK = schema.GroupVersionKind{
	Group:   "external-secrets.io",
	Version: "v1beta1",
	Kind:    "ExternalSecret",
}

// pushSecretGVK is the GroupVersionKind for ESO PushSecret resources.
var pushSecretGVK = schema.GroupVersionKind{
	Group:   "external-secrets.io",
	Version: "v1alpha1",
	Kind:    "PushSecret",
}

// IsExternalSecretReady checks if an ExternalSecret CR has a Ready=True condition
// in its status. Uses unstructured access since ESO types are not imported.
// (CC-0005 / REQ-002)
func IsExternalSecretReady(ctx context.Context, c client.Client, name, namespace string) (bool, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(externalSecretGVK)

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

	for _, cond := range conditions {
		condMap, ok := cond.(map[string]interface{})
		if !ok {
			continue
		}
		if condMap["type"] == "Ready" && condMap["status"] == "True" {
			return true, nil
		}
	}

	return false, nil
}

// IsSecretReady checks if a Kubernetes Secret exists and has at least one data key.
// (CC-0005 / REQ-003)
func IsSecretReady(ctx context.Context, c client.Client, name, namespace string) (bool, error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting Secret %s/%s: %w", namespace, name, err)
	}

	return len(secret.Data) > 0, nil
}

// GetSecretValue retrieves a specific key's value from a Kubernetes Secret.
// Returns an error if the Secret does not exist or the key is missing. (CC-0005 / REQ-003)
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

// EnsurePushSecret creates or updates a PushSecret CR that pushes the given source
// Secret keys to an external secret store. Sets owner references for garbage
// collection. Returns the name of the PushSecret. (CC-0005 / REQ-003)
func EnsurePushSecret(ctx context.Context, c client.Client, owner client.Object, scheme *runtime.Scheme, opts PushSecretOpts) (string, error) {
	pushSecret := buildPushSecret(opts)

	if err := controllerutil.SetControllerReference(owner, pushSecret, scheme); err != nil {
		return "", fmt.Errorf("setting owner reference on PushSecret %s/%s: %w", opts.Namespace, opts.Name, err)
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(pushSecretGVK)

	err := c.Get(ctx, client.ObjectKey{Name: opts.Name, Namespace: opts.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		if createErr := c.Create(ctx, pushSecret); createErr != nil {
			return "", fmt.Errorf("creating PushSecret %s/%s: %w", opts.Namespace, opts.Name, createErr)
		}
		return opts.Name, nil
	}
	if err != nil {
		return "", fmt.Errorf("getting PushSecret %s/%s: %w", opts.Namespace, opts.Name, err)
	}

	// Update spec fields on the existing object.
	existing.Object["spec"] = pushSecret.Object["spec"]
	existing.SetOwnerReferences(pushSecret.GetOwnerReferences())
	if updateErr := c.Update(ctx, existing); updateErr != nil {
		return "", fmt.Errorf("updating PushSecret %s/%s: %w", opts.Namespace, opts.Name, updateErr)
	}

	return opts.Name, nil
}

// buildPushSecret constructs an unstructured PushSecret object from the given options.
func buildPushSecret(opts PushSecretOpts) *unstructured.Unstructured {
	dataEntries := make([]interface{}, 0, len(opts.SecretKeys))
	for _, key := range opts.SecretKeys {
		dataEntries = append(dataEntries, map[string]interface{}{
			"match": map[string]interface{}{
				"secretKey": key,
				"remoteRef": map[string]interface{}{
					"remoteKey": fmt.Sprintf("%s/%s", opts.RemoteKeyBase, key),
				},
			},
		})
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(pushSecretGVK)
	obj.SetName(opts.Name)
	obj.SetNamespace(opts.Namespace)

	spec := map[string]interface{}{
		"secretStoreRefs": []interface{}{
			map[string]interface{}{
				"name": opts.StoreRef.Name,
				"kind": opts.StoreRef.Kind,
			},
		},
		"selector": map[string]interface{}{
			"secret": map[string]interface{}{
				"name": opts.SecretName,
			},
		},
		"data": dataEntries,
	}
	obj.Object["spec"] = spec

	return obj
}
