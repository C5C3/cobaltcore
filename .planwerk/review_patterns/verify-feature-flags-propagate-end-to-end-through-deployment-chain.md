# Review Pattern: Verify feature flags propagate end-to-end through deployment chain

**Review-Area**: testing
**Detection-Hint**: When a feature flag (env var, build flag) is added, trace it through every script, workflow, and chart values file from CI entrypoint to the rendered manifest. Check that conditional templates actually receive the value they gate on.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

For any new conditional Helm template or kustomize patch gated on a value, confirm every deployment path (CI workflow, local Makefile target, deploy script) sets that value. Check the helm install/upgrade invocation passes --set or --values for the gating key. Verify suspend/resume patches on HelmRelease aren't silently making Flux-applied values inert.

## Why it matters

A feature flag that is plumbed in the chart but not threaded through the deploy script renders zero output for the gated template. Downstream e2e assertions (e.g. chainsaw checks for the ServiceMonitor) cascade-fail with confusing errors, and the feature appears working in unit tests but is dead on arrival in CI.

## Examples from external reviews

### CC-0100 — berendt
- **Feedback**: In the CI e2e-prometheus job, hack/ci-deploy-operator.sh runs `helm install` without passing `monitoring.serviceMonitor.enabled=true`. With chart default `monitoring.serviceMonitor.enabled: false`, the gated template renders zero output — no ServiceMonitor in `openstack`.
- **What was missed**: For any new conditional Helm template or kustomize patch gated on a value, confirm every deployment path (CI workflow, local Makefile target, deploy script) sets that value. Check the helm install/upgrade invocation passes --set or --values for the gating key. Verify suspend/resume patches on HelmRelease aren't silently making Flux-applied values inert.
- **Fix**: Threaded WITH_PROMETHEUS through hack/ci-deploy-operator.sh: the script now reads WITH_PROMETHEUS as an optional env var, builds a helm_args array, and conditionally appends `--set monitoring.serviceMonitor.enabled=true` when WITH_PROMETHEUS=true. The e2e-prometheus job exports WITH_PROMETHEUS=true.
