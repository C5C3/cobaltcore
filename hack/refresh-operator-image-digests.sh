#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/refresh-operator-image-digests.sh — Pin the self-built operator images
# to their current digest.
#
# The Flux HelmReleases for the self-built operators (keystone-operator,
# c5c3-operator, horizon-operator, glance-operator, placement-operator)
# reference the mutable :latest image tag.
# A moved tag alone never rolls a running Deployment: the kubelet does not
# re-pull an image that is already present on the node, and the HelmRelease
# does not upgrade when neither chart version nor values change. This script
# closes that gap by resolving the digest currently behind each :latest tag
# and writing it into a per-operator ConfigMap that the HelmRelease consumes
# via valuesFrom (values key image.digest). A digest change updates the
# rendered pod spec (repository:tag@digest), which forces a pull and a
# rollout.
#
# Usage:
#   hack/refresh-operator-image-digests.sh    (or: make refresh-operator-digests)
#
# Operates on the current kubectl context, like every other deploy helper.
# Called by hack/deploy-infra.sh on the WITH_CONTROLPLANE=true
# CONTROLPLANE_OPERATORS=flux path; on all other paths the operator
# HelmReleases are suspended and the ConfigMaps are never created (the
# valuesFrom reference is optional). Run it standalone after a feature merge
# to roll the operators of a running cluster to the freshly built images.
#
# Requires kubectl to write the ConfigMaps and either docker (with buildx) or
# curl to resolve digests. Per-image resolve failures are logged and skipped
# so one unreachable image does not block the others; the exit code is non-zero
# when any image could not be refreshed.

set -euo pipefail

# Targets as `<helmrelease name>|<namespace>|<image ref>` tuples (same
# bash-3.2-safe idiom as REGISTRY_CACHE_UPSTREAMS in deploy-infra.sh). The
# ConfigMap is written into the HelmRelease's own namespace because Flux
# resolves valuesFrom references there. The ConfigMap name is derived as
# `<helmrelease name>-image-digest`.
OPERATOR_DIGEST_TARGETS=(
  "keystone-operator|keystone-system|ghcr.io/c5c3/keystone-operator:latest"
  "c5c3-operator|c5c3-system|ghcr.io/c5c3/c5c3-operator:latest"
  "horizon-operator|horizon-system|ghcr.io/c5c3/horizon-operator:latest"
  "glance-operator|glance-system|ghcr.io/c5c3/glance-operator:latest"
  "placement-operator|placement-system|ghcr.io/c5c3/placement-operator:latest"
)

# ---------------------------------------------------------------------------
# log — Print a timestamped log message (ISO 8601 UTC).
# Matches the pattern from deploy/openbao/bootstrap/common.sh.
# ---------------------------------------------------------------------------
log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

# ---------------------------------------------------------------------------
# preflight — Verify the required tools exist before touching anything.
# ---------------------------------------------------------------------------
preflight() {
  if ! command -v kubectl >/dev/null 2>&1; then
    log "ERROR: required tool not found: kubectl"
    exit 1
  fi
  if ! have_docker_buildx && ! have_curl; then
    log "ERROR: need either docker with buildx or curl to resolve image digests."
    exit 1
  fi
}

# ---------------------------------------------------------------------------
# have_docker_buildx — Return success when docker buildx is available.
# ---------------------------------------------------------------------------
have_docker_buildx() {
  command -v docker >/dev/null 2>&1 && docker buildx version >/dev/null 2>&1
}

# ---------------------------------------------------------------------------
# have_curl — Return success when curl is available.
# ---------------------------------------------------------------------------
have_curl() {
  command -v curl >/dev/null 2>&1
}

# ---------------------------------------------------------------------------
# resolve_image_digest_via_docker — Resolve the manifest digest behind an image ref.
#
# $1: image reference (e.g. ghcr.io/c5c3/keystone-operator:latest)
#
# Echoes the digest (sha256:...) on stdout; returns non-zero when the
# registry is unreachable or the resolved digest is empty. Same idiom as
# hack/ci-resolve-ubuntu-digest.sh.
# ---------------------------------------------------------------------------
resolve_image_digest_via_docker() {
  local image="$1"
  local digest
  if ! digest=$(docker buildx imagetools inspect "${image}" \
    --format '{{json .Manifest.Digest}}' 2>/dev/null | tr -d '"'); then
    return 1
  fi
  if [[ -z "${digest}" ]]; then
    return 1
  fi
  echo "${digest}"
}

# ---------------------------------------------------------------------------
# resolve_image_digest_via_curl — Resolve the manifest digest behind an image
# ref using curl against the registry HTTP API.
#
# This fallback avoids requiring docker on the host. It supports public and
# bearer-token protected registries (including ghcr.io) by honoring the
# standard WWW-Authenticate challenge.
# ---------------------------------------------------------------------------
resolve_image_digest_via_curl() {
  local image="$1"
  local registry repo reference digest tmp_headers http_code

  # Parse the image reference (simplified; handles most cases)
  if [[ "$image" == *"@"* ]]; then
    # Already has digest
    echo "${image#*@}"
    return 0
  fi

  # Extract registry, repo, and tag
  if [[ "$image" == *"/"* ]]; then
    registry="${image%%/*}"
    repo="${image#*/}"
  else
    registry="registry-1.docker.io"
    repo="library/$image"
  fi

  if [[ "$repo" == *":"* ]]; then
    reference="${repo##*:}"
    repo="${repo%:*}"
  else
    reference="latest"
  fi

  tmp_headers="$(mktemp)"
  trap 'rm -f "$tmp_headers"' RETURN

  # Try without auth first
  http_code=$(curl -s -w '%{http_code}' -o /dev/null \
    -H "Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json" \
    -D "$tmp_headers" \
    "https://${registry}/v2/${repo}/manifests/${reference}")

  if [[ "$http_code" == "200" ]]; then
    grep -i "Docker-Content-Digest:" "$tmp_headers" | sed 's/.*: //' | tr -d '\r'
    return 0
  fi

  # If 401, try with bearer token
  if [[ "$http_code" == "401" ]]; then
    local auth_header service scope realm token
    auth_header=$(grep -i "WWW-Authenticate:" "$tmp_headers" | head -1 | sed 's/.*: //')

    if [[ "$auth_header" == *"Bearer"* ]]; then
      realm=$(echo "$auth_header" | grep -o 'realm="[^"]*"' | cut -d'"' -f2)
      service=$(echo "$auth_header" | grep -o 'service="[^"]*"' | cut -d'"' -f2)
      scope="repository:${repo}:pull"

      if [[ -n "$realm" ]]; then
        local token_url="${realm}?service=${service}&scope=${scope}"
        token=$(curl -s "$token_url" | grep -o '"token":"[^"]*"' | cut -d'"' -f4)

        if [[ -n "$token" ]]; then
          # Retry with bearer token
          curl -s \
            -H "Authorization: Bearer $token" \
            -H "Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json" \
            -D "$tmp_headers" \
            "https://${registry}/v2/${repo}/manifests/${reference}" > /dev/null

          grep -i "Docker-Content-Digest:" "$tmp_headers" | sed 's/.*: //' | tr -d '\r'
          return 0
        fi
      fi
    fi
  fi

  return 1
}

# ---------------------------------------------------------------------------
# resolve_image_digest — Resolve the manifest digest behind an image ref.
#
# Prefer docker buildx when available, otherwise use curl against the registry
# HTTP API. If docker exists but fails for a given image, the curl fallback
# is still attempted.
# ---------------------------------------------------------------------------
resolve_image_digest() {
  local image="$1"
  local digest
  if have_docker_buildx; then
    if digest=$(resolve_image_digest_via_docker "${image}"); then
      echo "${digest}"
      return 0
    fi
  fi
  if have_curl; then
    if digest=$(resolve_image_digest_via_curl "${image}"); then
      echo "${digest}"
      return 0
    fi
  fi
  return 1
}

# ---------------------------------------------------------------------------
# render_digest_values — Render the Helm values payload for one digest.
#
# $1: digest (sha256:...)
#
# This exact payload is stored under the ConfigMap's values.yaml key and
# merged into the HelmRelease values by Flux; the two-space indentation of
# the digest key is load-bearing YAML.
# ---------------------------------------------------------------------------
render_digest_values() {
  printf 'image:\n  digest: %s\n' "$1"
}

# ---------------------------------------------------------------------------
# render_digest_configmap — Render the per-operator digest ConfigMap.
#
# $1: ConfigMap name
# $2: namespace
# $3: digest (sha256:...)
# ---------------------------------------------------------------------------
render_digest_configmap() {
  local name="$1"
  local namespace="$2"
  local digest="$3"
  cat <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${name}
  namespace: ${namespace}
data:
  values.yaml: |
$(render_digest_values "${digest}" | sed 's/^/    /')
EOF
}

# ---------------------------------------------------------------------------
# current_configmap_values — Read the stored values.yaml payload of a digest
# ConfigMap. Echoes an empty string when the ConfigMap does not exist.
#
# $1: ConfigMap name
# $2: namespace
# ---------------------------------------------------------------------------
current_configmap_values() {
  kubectl get configmap "$1" -n "$2" \
    -o jsonpath='{.data.values\.yaml}' 2>/dev/null || true
}

# ---------------------------------------------------------------------------
# annotate_helmrelease_reconcile — Request a reconcile of one HelmRelease by
# annotating with reconcile.fluxcd.io/requestedAt — the kubectl-only
# equivalent of `flux reconcile helmrelease` (same pattern as
# reconcile_helmrepository_sources in deploy-infra.sh). Best-effort: inert on
# suspended releases (deliberate — the default and CI paths keep the operator
# releases suspended) and tolerated on transient API errors.
#
# $1: HelmRelease name
# $2: namespace
# ---------------------------------------------------------------------------
annotate_helmrelease_reconcile() {
  kubectl annotate "helmrelease/$1" \
    "reconcile.fluxcd.io/requestedAt=$(date +%s%N)" \
    --overwrite -n "$2" || true
}

# ---------------------------------------------------------------------------
# refresh_operator_image_digests — Resolve every target image's digest, apply
# the per-operator ConfigMaps, and request a HelmRelease reconcile for each
# digest that changed. Returns non-zero when any image failed to resolve.
# ---------------------------------------------------------------------------
refresh_operator_image_digests() {
  local failures=0
  local entry name namespace image cm_name digest desired existing
  for entry in "${OPERATOR_DIGEST_TARGETS[@]}"; do
    IFS='|' read -r name namespace image <<<"${entry}"
    cm_name="${name}-image-digest"
    if ! digest=$(resolve_image_digest "${image}"); then
      log "WARNING: could not resolve digest for ${image}; leaving any existing ${cm_name} ConfigMap in place."
      failures=$((failures + 1))
      continue
    fi
    desired=$(render_digest_values "${digest}")
    existing=$(current_configmap_values "${cm_name}" "${namespace}")
    render_digest_configmap "${cm_name}" "${namespace}" "${digest}" | kubectl apply -f -
    if [[ "${existing}" == "${desired}" ]]; then
      log "${image} digest unchanged (${digest}); skipping reconcile request."
    else
      log "${image} pinned to ${digest}; requesting HelmRelease ${name} reconcile."
      annotate_helmrelease_reconcile "${name}" "${namespace}"
    fi
  done
  if ((failures > 0)); then
    return 1
  fi
  return 0
}

main() {
  preflight
  log "Refreshing operator image digest ConfigMaps..."
  refresh_operator_image_digests
  log "Operator image digests refreshed."
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
