package simulators

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SimulateCertificateReady creates a cert-manager Certificate custom resource
// (if it does not already exist) and patches its status sub-resource to set a
// Ready condition with status "True".
//
// In a real cluster the cert-manager operator would reconcile Certificate
// objects and update their status automatically. In envtest the operator is
// absent, so this simulator patches the status directly.
func SimulateCertificateReady(ctx context.Context, c client.Client, name, namespace string) error {
	return simulateConditionsOnlyReady(ctx, c, schema.GroupVersionKind{
		Group:   "cert-manager.io",
		Version: "v1",
		Kind:    "Certificate",
	}, name, namespace, "Ready", "Certificate is ready")
}
