#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/refresh-operator-image-digests.sh — Pin the self-built operator images
# to their current digest.
#
# The Flux HelmReleases for the self-built operators (keystone-operator,
# c5c3-operator, horizon-operator) reference the mutable :latest image tag.
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
# Requires docker (with buildx) to resolve digests and kubectl to write the
# ConfigMaps. Per-image resolve failures are logged and skipped so one
# unreachable image does not block the others; the exit code is non-zero when
# any image could not be refreshed.

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
  local tool
  for tool in docker kubectl; do
    if ! command -v "${tool}" >/dev/null 2>&1; then
      log "ERROR: required tool not found: ${tool}"
      exit 1
    fi
  done
  if ! docker buildx version >/dev/null 2>&1; then
    log "ERROR: docker buildx is required to resolve image digests (docker buildx version failed)."
    exit 1
  fi
}

# ---------------------------------------------------------------------------
# resolve_image_digest — Resolve the manifest digest behind an image ref.
#
# $1: image reference (e.g. ghcr.io/c5c3/keystone-operator:latest)
#
# Echoes the digest (sha256:...) on stdout; returns non-zero when the
# registry is unreachable or the resolved digest is empty. Same idiom as
# hack/ci-resolve-ubuntu-digest.sh.
# ---------------------------------------------------------------------------
resolve_image_digest() {
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
