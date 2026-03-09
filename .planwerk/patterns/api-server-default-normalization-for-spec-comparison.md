# Pattern: API-server default normalization for spec comparison

**Component**: internal/common/normalize, internal/common/deployment, internal/common/job
**Category**: data-access
**Applies-When**: Implementing create-or-update logic that compares a desired Kubernetes resource spec against the stored (API-server-defaulted) spec to avoid spurious updates

## Description

When comparing a caller-constructed Kubernetes resource spec against an API-server-stored spec, the desired spec must be deep-copied and normalized to fill in API-server defaults before the comparison. This prevents infinite update loops caused by zero-value fields in the caller's spec differing from defaulted fields in the stored spec. The comparison uses the normalized copy, but the update (if needed) writes the original spec to let the API server apply its own defaults. Both pod-level defaults (ServiceAccountName, DNSPolicy, SchedulerName, TerminationGracePeriodSeconds, EnableServiceLinks) and container-level defaults (ImagePullPolicy, TerminationMessagePath, TerminationMessagePolicy) must be normalized.

The normalization functions live in the shared `normalize` package (`internal/common/normalize/`) and are imported by both `deployment` and `job` packages. This avoids code duplication and ensures a single source of truth for the defaulting logic.

## Examples

### `internal/common/job/job.go:59-61`

```go
desiredSpec := job.Spec.Template.Spec.DeepCopy()
normalize.PodSpecDefaults(desiredSpec)
if !apiequality.Semantic.DeepEqual(existing.Spec.Template.Spec, *desiredSpec) {
```

### `internal/common/deployment/deployment.go:47-50`

```go
desiredSpec := deploy.Spec.DeepCopy()
normalize.PodSpecDefaults(&desiredSpec.Template.Spec)

if !apiequality.Semantic.DeepEqual(existing.Spec, *desiredSpec) {
```
