package simulators

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SimulatePushSecretReady creates an ESO PushSecret custom resource (if it does
// not already exist) and patches its status sub-resource to set a Ready
// condition with status "True".
//
// In a real cluster the external-secrets operator would reconcile PushSecret
// objects and update their status automatically. In envtest the operator is
// absent, so this simulator patches the status directly.
func SimulatePushSecretReady(ctx context.Context, c client.Client, name, namespace string) error {
	return simulateConditionsOnlyReady(ctx, c, schema.GroupVersionKind{
		Group:   "external-secrets.io",
		Version: "v1alpha1",
		Kind:    "PushSecret",
	}, name, namespace, "PushSecretSynced", "PushSecret was synced")
}
