package simulators

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SimulateCertificateReady creates a cert-manager Certificate custom resource
// (if it does not already exist) and patches its status sub-resource so that
// ready=true and a "Ready" condition with status "True" is present.  This
// simulates the behaviour of the cert-manager controller in envtest
// environments where cert-manager is not running.
func SimulateCertificateReady(ctx context.Context, c client.Client, name, namespace string) error {
	return simulateUnstructuredReady(ctx, c, schema.GroupVersionKind{
		Group:   "cert-manager.io",
		Version: "v1",
		Kind:    "Certificate",
	}, name, namespace, "Ready", "Certificate is ready")
}
