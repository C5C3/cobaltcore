package tls

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GetTLSSecret fetches a corev1.Secret by name and namespace, then validates
// that it contains both "tls.crt" and "tls.key" data keys. Returns the raw
// certificate and key bytes directly rather than the Secret object.
// If the Secret is not found or is missing required TLS keys, an error is
// returned. (CC-0005)
func GetTLSSecret(ctx context.Context, c client.Client, name, namespace string) (certPEM []byte, keyPEM []byte, err error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, secret); err != nil {
		return nil, nil, fmt.Errorf("getting TLS Secret %s/%s: %w", namespace, name, err)
	}

	certPEM, ok := secret.Data["tls.crt"]
	if !ok {
		return nil, nil, fmt.Errorf("TLS Secret %s/%s is missing required key %q", namespace, name, "tls.crt")
	}
	keyPEM, ok = secret.Data["tls.key"]
	if !ok {
		return nil, nil, fmt.Errorf("TLS Secret %s/%s is missing required key %q", namespace, name, "tls.key")
	}

	return certPEM, keyPEM, nil
}
