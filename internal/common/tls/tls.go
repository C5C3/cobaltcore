package tls

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

// CertificateOpts configures the Certificate to create.
type CertificateOpts struct {
	Name       string
	Namespace  string
	SecretName string // name of the Secret cert-manager will create
	IssuerRef  IssuerRef
	DNSNames   []string
}

// IssuerRef references a cert-manager issuer.
type IssuerRef struct {
	Name  string
	Kind  string // "ClusterIssuer" or "Issuer"
	Group string // typically "cert-manager.io"
}

// certificateGVK is the GroupVersionKind for cert-manager Certificate resources.
var certificateGVK = schema.GroupVersionKind{
	Group:   "cert-manager.io",
	Version: "v1",
	Kind:    "Certificate",
}

// EnsureCertificate creates or updates a cert-manager Certificate CR with the given
// options. Sets owner references for garbage collection. Returns the name of the
// Certificate. (CC-0005 / REQ-007)
func EnsureCertificate(ctx context.Context, c client.Client, owner client.Object, scheme *runtime.Scheme, opts CertificateOpts) (string, error) {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName(opts.Name)
	cert.SetNamespace(opts.Namespace)

	spec := map[string]interface{}{
		"secretName": opts.SecretName,
		"issuerRef": map[string]interface{}{
			"name":  opts.IssuerRef.Name,
			"kind":  opts.IssuerRef.Kind,
			"group": opts.IssuerRef.Group,
		},
	}
	if len(opts.DNSNames) > 0 {
		dnsNames := make([]interface{}, len(opts.DNSNames))
		for i, d := range opts.DNSNames {
			dnsNames[i] = d
		}
		spec["dnsNames"] = dnsNames
	}

	if err := unstructured.SetNestedField(cert.Object, spec, "spec"); err != nil {
		return "", fmt.Errorf("setting Certificate spec: %w", err)
	}

	// Set owner reference so the Certificate is garbage-collected with the owner.
	// We need to set TypeMeta on the unstructured object for SetControllerReference.
	cert.SetAPIVersion(certificateGVK.Group + "/" + certificateGVK.Version)
	cert.SetKind(certificateGVK.Kind)

	if err := controllerutil.SetControllerReference(owner, cert, scheme); err != nil {
		return "", fmt.Errorf("setting controller reference: %w", err)
	}

	// Check if the Certificate already exists.
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(certificateGVK)
	err := c.Get(ctx, client.ObjectKeyFromObject(cert), existing)
	if apierrors.IsNotFound(err) {
		if err := c.Create(ctx, cert); err != nil {
			return "", fmt.Errorf("creating Certificate: %w", err)
		}
		return opts.Name, nil
	}
	if err != nil {
		return "", fmt.Errorf("checking for existing Certificate: %w", err)
	}

	// Update the existing Certificate's spec and owner references.
	existing.Object["spec"] = cert.Object["spec"]
	existing.SetOwnerReferences(cert.GetOwnerReferences())
	if err := c.Update(ctx, existing); err != nil {
		return "", fmt.Errorf("updating Certificate: %w", err)
	}

	return opts.Name, nil
}

// GetTLSSecret retrieves the TLS certificate and key from a Kubernetes Secret
// created by cert-manager. Returns the tls.crt and tls.key values.
// (CC-0005 / REQ-007)
func GetTLSSecret(ctx context.Context, c client.Client, name, namespace string) (cert []byte, key []byte, err error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, secret); err != nil {
		return nil, nil, fmt.Errorf("getting TLS secret %s/%s: %w", namespace, name, err)
	}

	certData, certOK := secret.Data["tls.crt"]
	keyData, keyOK := secret.Data["tls.key"]

	if !certOK || !keyOK {
		var missing []string
		if !certOK {
			missing = append(missing, "tls.crt")
		}
		if !keyOK {
			missing = append(missing, "tls.key")
		}
		return nil, nil, fmt.Errorf("secret %s/%s missing required keys: %v", namespace, name, missing)
	}

	return certData, keyData, nil
}
