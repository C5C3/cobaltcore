# Pattern: Parallel CI job with shared base dependency and gated downstream verification

**Component**: .github/workflows/build-images.yaml
**Category**: service-structure
**Applies-When**: Adding a new CI validation job that should run in parallel with an existing job and gate a downstream verification job

## Description

When adding a new validation job (like test-service-images) that runs in parallel with an existing build job (build-service-images), both depend on the same base jobs (build-base-images, verify-base-images) and the downstream verification job (verify-service-images) gates on both. The parallel job reuses the same source preparation steps (checkout, resolve source ref, checkout upstream, apply patches, apply constraint overrides) as the build job, following identical patterns for env var usage and expression injection defense.

## Examples

### `.github/workflows/build-images.yaml:502-503`

```
  test-service-images:
    needs: [build-base-images, verify-base-images]
```

### `.github/workflows/build-images.yaml:600-602`

```
  verify-service-images:
    if: github.event_name != 'pull_request'
    needs: [build-service-images, test-service-images]
```

