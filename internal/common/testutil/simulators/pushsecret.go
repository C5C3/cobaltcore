package simulators

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SimulatePushSecretReady creates an ESO PushSecret custom resource (if it does
// not already exist) and patches its status sub-resource so that a "Ready"
// condition with status "True" is present. This simulates the behaviour of the
// external-secrets operator in envtest environments where ESO is not running.
func SimulatePushSecretReady(ctx context.Context, c client.Client, name, namespace string) error {
	return simulateUnstructuredReady(ctx, c, schema.GroupVersionKind{
		Group:   "external-secrets.io",
		Version: "v1alpha1",
		Kind:    "PushSecret",
	}, name, namespace, "Synced", "PushSecret is synced")
}
