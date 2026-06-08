# Pattern: Shared CRD value types live once in commonv1; relocations leave a backward-compat alias

**Component**: internal/common/types (commonv1), operators/*/api/v1alpha1
**Category**: service-structure
**Applies-When**: A CRD value type (a sub-spec used by more than one operator's CRD) needs to be shared between the keystone and c5c3 operators, or an existing per-operator copy is being consolidated

## Description

Value types reused across operator CRDs are defined exactly once in internal/common/types and referenced as commonv1.<Type> from each operator's api package, rather than re-curated field-for-field per operator (which silently drifts). When an exported type is relocated out of an operator's api package into commonv1, a backward-compatible type alias (type X = commonv1.X) is left behind in the original package so existing in-repo and external importers compile unchanged. deepcopy is generated in commonv1 and the standalone per-operator deepcopy funcs are removed; the consuming DeepCopyInto references types.<Type>. This is the same discipline already applied to DatabaseSpec/CacheSpec/ImageSpec/PolicySpec/SecretRefSpec and now GatewaySpec.

## Examples

### `internal/common/types/types.go:166`

```go
// GatewaySpec configures the Gateway API HTTPRoute used to expose an OpenStack
// service externally. It is the single source of truth for the shared Gateway
// shape: both the keystone and c5c3 operators reuse this commonv1 type instead
// of maintaining their own field-for-field copies.
type GatewaySpec struct {
	ParentRef GatewayParentRefSpec `json:"parentRef"`
	// +kubebuilder:validation:MinLength=1
	Hostname string `json:"hostname"`
	// +optional
	Path string `json:"path,omitempty"`
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}
```

### `operators/keystone/api/v1alpha1/keystone_types.go:471`

```go
// GatewaySpec and GatewayParentRefSpec are aliased to the shared commonv1
// definitions (CC-0111). ... These aliases keep existing references —
// keystonev1alpha1.GatewaySpec and bare GatewaySpec{} literals alike —
// compiling unchanged.
type (
	GatewaySpec          = commonv1.GatewaySpec
	GatewayParentRefSpec = commonv1.GatewayParentRefSpec
)
```

