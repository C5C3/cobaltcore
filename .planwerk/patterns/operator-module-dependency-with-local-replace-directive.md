# Pattern: Operator module dependency with local replace directive

**Component**: operators/*/go.mod
**Category**: configuration
**Applies-When**: Creating a new operator module that depends on internal/common

## Description

Every operator go.mod requires internal/common at v0.0.0 (pseudo-version for local-only module) and declares a replace directive pointing to ../../internal/common for GOWORK=off CI builds. Direct dependencies are controller-runtime v0.23.x, apimachinery v0.35.x, and client-go v0.35.x. The module path follows github.com/c5c3/forge/operators/<name>.

## Examples

### `operators/keystone/go.mod:1`

```
module github.com/c5c3/forge/operators/keystone

go 1.25.0

require (
	github.com/c5c3/forge/internal/common v0.0.0
	k8s.io/apimachinery v0.35.1
	k8s.io/client-go v0.35.1
	sigs.k8s.io/controller-runtime v0.23.1
)

// ... indirect deps ...

replace github.com/c5c3/forge/internal/common => ../../internal/common
```

### `operators/c5c3/go.mod:1`

```
module github.com/c5c3/forge/operators/c5c3

go 1.25.0

require (
	github.com/c5c3/forge/internal/common v0.0.0
	k8s.io/apimachinery v0.35.1
	k8s.io/client-go v0.35.1
	sigs.k8s.io/controller-runtime v0.23.1
)

// ... indirect deps ...

replace github.com/c5c3/forge/internal/common => ../../internal/common
```

