# Pattern: Operator entrypoint via shared bootstrap

**Component**: operators/*/main.go + internal/common/bootstrap
**Category**: service-structure
**Applies-When**: Adding a new operator to the monorepo (e.g., glance, nova, neutron)

## Description

Every operator main.go follows an identical structure: package-level scheme var, init() with clientgoscheme.AddToScheme + kubebuilder scaffold:scheme marker, main() calling bootstrap.Run(bootstrap.ManagerConfig{...}) with operator-specific Scheme, LeaderElectionID, and SetupFunc. Error handling logs via ctrl.Log.WithName("setup").Error() then os.Exit(1). The bootstrap package centralises flag parsing, zap logging, metrics, health probes, and manager lifecycle — operators only provide configuration.

## Examples

### `operators/keystone/main.go:1-40`

```go
package main

import (
	"os"

	"github.com/c5c3/forge/internal/common/bootstrap"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	if err := bootstrap.Run(bootstrap.ManagerConfig{
		Scheme:           scheme,
		LeaderElectionID: "keystone.openstack.c5c3.io",
		SetupFunc: func(_ ctrl.Manager) error {
			// +kubebuilder:scaffold:builder — register controllers here
			return nil
		},
	}); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to run manager")
		os.Exit(1)
	}
}
```

### `operators/c5c3/main.go:1-40`

```go
package main

import (
	"os"

	"github.com/c5c3/forge/internal/common/bootstrap"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	if err := bootstrap.Run(bootstrap.ManagerConfig{
		Scheme:           scheme,
		LeaderElectionID: "c5c3.openstack.c5c3.io",
		SetupFunc: func(_ ctrl.Manager) error {
			// +kubebuilder:scaffold:builder — register controllers here
			return nil
		},
	}); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to run manager")
		os.Exit(1)
	}
}
```

