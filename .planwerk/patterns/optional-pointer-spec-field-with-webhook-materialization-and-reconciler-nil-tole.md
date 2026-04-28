# Pattern: Optional-pointer spec field with webhook materialization and reconciler nil-tolerance

**Component**: operators/keystone/api/v1alpha1, operators/keystone/internal/controller
**Category**: configuration
**Applies-When**: Adding a new optional sub-spec on KeystoneSpec (or a future operator's CR spec) that needs documented production defaults applied at admission AND must remain safe when a CR observed by a freshly upgraded operator bypassed the webhook

## Description

The CRD field is declared as a pointer (`*Logging`, `*UWSGI`, `*Resources`). The defaulting webhook materializes a baseline struct when the pointer is nil, and partial-fills zero-valued sub-fields when non-nil — with a documented carve-out for bool fields (because false is indistinguishable from explicit false). The reconciler additionally exposes a small `effectiveX(*Spec) Spec` helper that re-applies the same baseline when spec is nil, so downstream code can dereference unconditionally. This covers the pre-existing-CR-during-operator-upgrade scenario without the reconciler having to repeat the defaulting logic in every call site. The carve-out for bool defaulting and the dual webhook+reconciler defaulting are both explicit conventions used in this codebase.

## Examples

### `operators/keystone/api/v1alpha1/keystone_webhook.go:115-135`

```go
// REQ-001 (CC-0098): Default zero-valued sub-fields of spec.logging.
// When the pointer is nil, materialize the production baseline so downstream
// reconciler code dereferences spec.logging unconditionally (mirrors the
// Resources-when-nil pattern below). When non-nil, partial-fill zero values
// (Format, Level) but leave Debug alone — bool's zero value is indistinguishable
// from explicit false, so we cannot safely override it (mirrors the
// UWSGISpec.HTTPKeepAlive carve-out documented above).
if obj.Spec.Logging == nil {
    obj.Spec.Logging = &LoggingSpec{
        Format: "text",
        Level:  "INFO",
        Debug:  false,
    }
} else {
    if obj.Spec.Logging.Format == "" {
        obj.Spec.Logging.Format = "text"
    }
    if obj.Spec.Logging.Level == "" {
        obj.Spec.Logging.Level = "INFO"
    }
}
```

### `operators/keystone/internal/controller/reconcile_config.go:284-303`

```go
// effectiveLogging returns the LoggingSpec to use for config rendering,
// materializing the production defaults when spec.logging is nil. The
// defaulting webhook materializes the same baseline at admission, so this
// fallback only matters when a CR bypasses the webhook (e.g. a pre-existing
// CR observed by a freshly upgraded operator). Mirrors the UWSGISpec
// nil-tolerance pattern at reconcile_deployment.go:317 (CC-0098, REQ-001).
func effectiveLogging(spec *keystonev1alpha1.LoggingSpec) keystonev1alpha1.LoggingSpec {
    out := keystonev1alpha1.LoggingSpec{Format: "text", Level: "INFO"}
    if spec == nil {
        return out
    }
    out = *spec
    if out.Format == "" {
        out.Format = "text"
    }
    if out.Level == "" {
        out.Level = "INFO"
    }
    return out
}
```

