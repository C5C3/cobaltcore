# Pattern: Simulator pattern for envtest CRD controllers

**Component**: internal/common/testutil/simulators/
**Category**: testing
**Applies-When**: Adding a new simulator for a third-party CRD operator (e.g. cert-manager, ESO, MariaDB)

## Description

Each simulator is a single function in its own file that calls the shared simulateUnstructuredReady helper with the appropriate GVK, condition reason, and message. The helper uses createOrGet for idempotency, then patches status via client.MergeFrom with status.ready=true and a Ready condition. For CRDs needing custom status (like ExternalSecret which creates a target Secret), a dedicated implementation is used instead of the shared helper.

## Examples

### `internal/common/testutil/simulators/certificate.go:1-21`

```go
package simulators

import (
	"context"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func SimulateCertificateReady(ctx context.Context, c client.Client, name, namespace string) error {
	return simulateUnstructuredReady(ctx, c, schema.GroupVersionKind{
		Group:   "cert-manager.io",
		Version: "v1",
		Kind:    "Certificate",
	}, name, namespace, "Ready", "Certificate is ready")
}
```

### `internal/common/testutil/simulators/pushsecret.go:1-21`

```go
package simulators

import (
	"context"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func SimulatePushSecretReady(ctx context.Context, c client.Client, name, namespace string) error {
	return simulateUnstructuredReady(ctx, c, schema.GroupVersionKind{
		Group:   "external-secrets.io",
		Version: "v1alpha1",
		Kind:    "PushSecret",
	}, name, namespace, "Ready", "PushSecret is ready")
}
```

