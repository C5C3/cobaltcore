package tls

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// certificateGVK is the GroupVersionKind for the cert-manager Certificate CR.
var certificateGVK = schema.GroupVersionKind{
	Group:   "cert-manager.io",
	Version: "v1",
	Kind:    "Certificate",
}

// EnsureCertificate creates a cert-manager.io/v1 Certificate custom resource
// using an unstructured object. The Certificate's spec.secretName is set to
// "{name}-tls", and issuerRef points to a ClusterIssuer with the given
// issuerName. Owner references are set from the variadic owners parameter.
//
// The operation is idempotent: if the Certificate already exists, no error is
// returned. (CC-0005)
func EnsureCertificate(ctx context.Context, c client.Client, name, namespace, issuerName, commonName string, dnsNames []string, owners ...metav1.OwnerReference) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(certificateGVK)
	obj.SetName(name)
	obj.SetNamespace(namespace)

	secretName := name + "-tls"
	if err := unstructured.SetNestedField(obj.Object, secretName, "spec", "secretName"); err != nil {
		return fmt.Errorf("setting Certificate spec.secretName: %w", err)
	}

	if err := unstructured.SetNestedField(obj.Object, commonName, "spec", "commonName"); err != nil {
		return fmt.Errorf("setting Certificate spec.commonName: %w", err)
	}

	issuerRef := map[string]interface{}{
		"name": issuerName,
		"kind": "ClusterIssuer",
	}
	if err := unstructured.SetNestedField(obj.Object, issuerRef, "spec", "issuerRef"); err != nil {
		return fmt.Errorf("setting Certificate spec.issuerRef: %w", err)
	}

	dnsNamesIface := make([]interface{}, len(dnsNames))
	for i, d := range dnsNames {
		dnsNamesIface[i] = d
	}
	if err := unstructured.SetNestedSlice(obj.Object, dnsNamesIface, "spec", "dnsNames"); err != nil {
		return fmt.Errorf("setting Certificate spec.dnsNames: %w", err)
	}

	if len(owners) > 0 {
		obj.SetOwnerReferences(owners)
	}

	if err := c.Create(ctx, obj); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating Certificate %s/%s: %w", namespace, name, err)
	}
	return nil
}
