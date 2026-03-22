# Pattern: Infrastructure deployment script with 8-step orchestration and configurable timeouts

**Component**: hack/
**Category**: service-structure
**Applies-When**: Adding a new deployment orchestration script that deploys multiple interdependent components to a kind cluster with health-wait gates between phases

## Description

Deployment scripts follow a numbered step sequence with log() delimiters ('=== Step N/M: description ==='), wait_for_* helper functions with configurable timeouts via environment variables, preflight checks for required CLI tools and Docker, and a SKIP_*_CREATE flag for CI mode where the cluster is pre-created by a GitHub Action. Each wait function polls every 10 seconds with a deadline, dumps diagnostic info on timeout, and exits 1. The script uses set -euo pipefail, SPDX Apache-2.0 header, and feature ID reference.

## Examples

### `hack/deploy-infra.sh:50-52`

```
log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}
```

### `hack/deploy-infra.sh:60-107`

```
wait_for_helmreleases() {
  local timeout="$1"
  shift
  local releases=("$@")
  local deadline=$(( $(date +%s) + timeout ))

  log "Waiting up to ${timeout}s for HelmReleases to become Ready: ${releases[*]}"

  while true; do
    local all_ready=true
    for release in "${releases[@]}"; do
      local ns
      ns=$(kubectl get helmrelease --all-namespaces -o json 2>/dev/null \
        | jq -r --arg name "${release}" '.items[] | select(.metadata.name == $name) | .metadata.namespace' 2>/dev/null) || true
      ...
    done
    if [[ $(date +%s) -ge ${deadline} ]]; then
      log "ERROR: Timed out waiting for HelmReleases after ${timeout}s."
      kubectl get helmrelease --all-namespaces 2>/dev/null || true
      exit 1
    fi
    sleep 10
  done
}
```

