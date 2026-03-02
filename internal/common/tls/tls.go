package tls

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

// EnsureCertificate creates a cert-manager Certificate CR (cert-manager.io/v1) via the
// unstructured client. The function is idempotent — if the Certificate already exists,
// it returns nil. (CC-0005, REQ-007, REQ-009, REQ-010)
func EnsureCertificate(ctx context.Context, c client.Client, name, namespace string, dnsNames []string, secretName, issuerName, issuerKind, issuerGroup string, ownerRefs ...metav1.OwnerReference) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "cert-manager.io",
		Version: "v1",
		Kind:    "Certificate",
	})
	obj.SetName(name)
	obj.SetNamespace(namespace)

	if len(ownerRefs) > 0 {
		obj.SetOwnerReferences(ownerRefs)
	}

	if err := unstructured.SetNestedField(obj.Object, secretName, "spec", "secretName"); err != nil {
		return fmt.Errorf("setting Certificate spec.secretName: %w", err)
	}

	dnsNamesIface := make([]interface{}, len(dnsNames))
	for i, d := range dnsNames {
		dnsNamesIface[i] = d
	}
	if err := unstructured.SetNestedSlice(obj.Object, dnsNamesIface, "spec", "dnsNames"); err != nil {
		return fmt.Errorf("setting Certificate spec.dnsNames: %w", err)
	}

	issuerRef := map[string]interface{}{
		"name":  issuerName,
		"kind":  issuerKind,
		"group": issuerGroup,
	}
	if err := unstructured.SetNestedField(obj.Object, issuerRef, "spec", "issuerRef"); err != nil {
		return fmt.Errorf("setting Certificate spec.issuerRef: %w", err)
	}

	err := c.Create(ctx, obj)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating Certificate: %w", err)
	}
	return nil
}

// GetTLSSecret retrieves a typed corev1.Secret by name and namespace.
// Returns the Secret or an error if it does not exist. (CC-0005, REQ-007)
func GetTLSSecret(ctx context.Context, c client.Client, namespace, name string) (*corev1.Secret, error) {
	secret := corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &secret); err != nil {
		return nil, fmt.Errorf("getting TLS Secret %s/%s: %w", namespace, name, err)
	}
	return &secret, nil
}
