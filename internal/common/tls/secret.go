package tls

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GetTLSSecret fetches a corev1.Secret by name and namespace, then validates
// that it contains both "tls.crt" and "tls.key" data keys. If the Secret is
// not found or is missing required TLS keys, an error is returned. (CC-0005)
func GetTLSSecret(ctx context.Context, c client.Client, name, namespace string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, secret); err != nil {
		return nil, fmt.Errorf("getting TLS Secret %s/%s: %w", namespace, name, err)
	}

	if _, ok := secret.Data["tls.crt"]; !ok {
		return nil, fmt.Errorf("TLS Secret %s/%s is missing required key %q", namespace, name, "tls.crt")
	}
	if _, ok := secret.Data["tls.key"]; !ok {
		return nil, fmt.Errorf("TLS Secret %s/%s is missing required key %q", namespace, name, "tls.key")
	}

	return secret, nil
}
