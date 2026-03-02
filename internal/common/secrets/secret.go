package secrets

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IsSecretReady checks whether a corev1.Secret exists and contains at least one
// data key.
//
// It returns (true, nil) if the Secret exists and has data, (false, nil) if it
// does not exist or exists but has no data keys, and (false, err) if the API
// call fails for any other reason. (CC-0005)
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

// GetSecretValue fetches a corev1.Secret and returns the string value of the
// specified key from its Data field.
//
// It returns an error if the Secret does not exist or if the requested key is
// not present. (CC-0005)
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
