# Pattern: Strict-equality opt-in flag with three-site gating in deploy-infra

**Component**: hack/deploy-infra.sh
**Category**: configuration
**Applies-When**: Adding a new opt-in feature flag (e.g. WITH_FLUX_CLI, WITH_CHAOS_MESH) that gates host-side prerequisites, kustomize apply, and Phase 3 helm-release wait

## Description

Each opt-in flag is declared in the configuration block as `WITH_<FEATURE>="${WITH_<FEATURE>:-false}"`, echoed in the deploy banner so the resolved value is visible in CI logs, and gated by strict `[[ "${WITH_<FEATURE>}" == "true" ]]` comparison at every site (host-side preflight call, opt-in kustomize apply after the base overlay, dynamic append to the helm-release wait list). Strict equality rejects accidental non-canonical values like `yes` / `1`. The flag's three gates always go in the same order: preflight, post-base apply, wait-list append.

## Examples

### `hack/deploy-infra.sh:65-69`

```
# Gates the opt-in chaos-mesh kind overlay (deploy/kind/chaos-mesh) and the
# host-side kernel-module load. Defaults to false so the kind Quick Start
# stays minimal; set WITH_CHAOS_MESH=true to enable chaos-engineering tests
# (CC-0097).
WITH_CHAOS_MESH="${WITH_CHAOS_MESH:-false}"
```

### `hack/deploy-infra.sh:809-813, 893-898, 932-934`

```
if [[ "${WITH_CHAOS_MESH}" == "true" ]]; then
    load_chaos_mesh_kernel_modules
  else
    log "Skipping chaos-mesh kernel modules (WITH_CHAOS_MESH=false)."
  fi
  ...
  if [[ "${WITH_CHAOS_MESH}" == "true" ]]; then
    kubectl apply -k "${REPO_ROOT}/deploy/kind/chaos-mesh"
    log "Chaos Mesh kind overlay applied (WITH_CHAOS_MESH=true)."
  fi
  ...
  local helm_releases=(prometheus-operator-crds openbao mariadb-operator-crds mariadb-operator external-secrets memcached-operator envoy-gateway)
  if [[ "${WITH_CHAOS_MESH}" == "true" ]]; then
    helm_releases+=(chaos-mesh)
  fi
```

