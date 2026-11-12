#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the Envoy Gateway work did NOT alter the production overlay
# The operator stays platform-agnostic; kind-only
# addons must live under deploy/kind/ and deploy/flux-system/ must be
# byte-identical to origin/main for the following paths:
#
#   - deploy/flux-system/fluxinstance.yaml
#   - deploy/flux-system/releases/*
#
# releases/kustomization.yaml is the operator composition root, not a runtime
# release. That one file is excluded from the historical byte comparison;
# the existing raw FluxInstance and release guards remain in effect.
#
# deliberately removes chaos-mesh from the production overlay
# entirely:
#   - the `sources/chaos-mesh.yaml` and `releases/chaos-mesh.yaml` files
#     are relocated into the kind-only opt-in overlay at
#     deploy/kind/chaos-mesh/ so the overlay is self-contained (no
#     parent-directory references — kubectl#948); and
#   - kustomization.yaml drops the corresponding entries from `resources:`.
# kustomization.yaml and the two relocated chaos-mesh files are therefore
# excluded from the byte-identity check. The chaos_mesh_overlay_test.sh
# suite asserts the resulting posture (production renders zero chaos-mesh
# resources; the overlay renders the full bundle).
#
# enforces OpenBao mTLS by editing
# deploy/flux-system/releases/openbao/helmrelease.yaml in place:
#   - the HA raft listener requires & verifies client certs
#     (tls_client_ca_file + tls_require_and_verify_client_cert = true);
#   - all three retry_join stanzas carry leader_client_cert_file +
#     leader_client_key_file pointing at /openbao/client-tls; and
#   - server.volumes/volumeMounts mount the openbao-client-tls Secret
#     at /openbao/client-tls.
# openbao.yaml is therefore excluded from the byte-identity check.
#
# Issue #1207 enables the cert-manager chart's startup API check by editing
# deploy/flux-system/releases/cert-manager.yaml in place
# (startupapicheck.enabled: true). cert-manager.yaml is therefore excluded
# from the byte-identity check; tests/unit/deploy/cert_manager_release_test.sh
# pins the value.
#
# The mariadb-operator major move lifts the chart floor of
# deploy/flux-system/releases/mariadb-operator.yaml and
# mariadb-operator-crds.yaml in place to the release the operators' Go
# module pins (>=26.10.2 <27.0.0). Both files are therefore excluded from
# the byte-identity check.
#
# Issue #1656 lifts the chart floor of
# deploy/flux-system/releases/memcached-operator.yaml in place to 0.5.0
# (>=0.5.0 <1.0.0), the first release that publishes one status.serverList
# entry per Memcached pod. memcached-operator.yaml is therefore excluded from
# the byte-identity check.
#
# In addition, rendering deploy/flux-system/sources/ must produce
# ONLY Flux SOURCE objects — HelmRepository, OCIRepository, GitRepository — so
# that no HelmRelease / Gateway controller leaks into the sources directory.
# Its kustomization.yaml is build configuration, not a runtime source object;
# inspect the rendered catalog rather than classifying added files by name.
# OCIRepository joined the list for the openbao-operator chart, which pins an
# artifact digest (a HelmRepository can only carry a chart version, which does
# not gate a re-pushed OCI tag); GitRepository is what sources/k-orc.yaml has
# always been.
#
# The historical byte comparison skips without a git worktree or origin/main.
# The source catalog check runs independently, including from shipped tarballs,
# when kustomize, yq and jq are available.
#
# Usage: bash tests/unit/deploy/production_posture_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Preconditions — skip gracefully when git context is unavailable.
# ---------------------------------------------------------------------------
preflight() {
  if ! command -v git >/dev/null 2>&1; then
    return 1
  fi
  if ! git -C "$PROJECT_ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    return 1
  fi
  # Need a local ref for origin/main; CI clones should have it.
  if ! git -C "$PROJECT_ROOT" rev-parse --verify --quiet origin/main >/dev/null 2>&1; then
    return 1
  fi
  return 0
}

# --- Test 1: tracked production-overlay files are unchanged vs origin/main
#             ---
test_production_overlay_unchanged() {
  echo "Test: deploy/flux-system/{fluxinstance,releases/*} unchanged vs origin/main"

  if ! preflight; then
    echo "  SKIP: git / origin/main unavailable (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  # `git diff --exit-code` returns non-zero when the worktree differs from
  # the ref. The chaos-mesh relocation deliberately removes
  # deploy/flux-system/releases/chaos-mesh.yaml (now at
  # deploy/kind/chaos-mesh/release.yaml), so it is carved out per-path with
  # `:(exclude)…` pathspecs. fluxinstance.yaml has no carve-out — it must
  # remain byte-identical.
  local groups_label=(
    "deploy/flux-system/fluxinstance.yaml"
    "deploy/flux-system/releases (excluding its kustomization.yaml composition root, chaos-mesh.yaml relocated, openbao.yaml edited in place, external-secrets.yaml edited in place, cert-manager.yaml edited in place, mariadb-operator.yaml/mariadb-operator-crds.yaml edited in place for the v26 chart floor, memcached-operator.yaml edited in place for the 0.5.0 chart floor, keystone-operator.yaml/horizon-operator.yaml edited in place for the image-digest valuesFrom, and the c5c3-operator.yaml/k-orc.yaml/garage-operator.yaml/glance-operator.yaml/placement-operator.yaml/openbao-operator.yaml/barbican-operator.yaml/ovn-operator.yaml/neutron-operator.yaml/cinder-operator.yaml/nova-operator.yaml/rabbitmq-cluster-operator.yaml/messaging-topology-operator.yaml releases added)"
  )
  local groups_specs=(
    "deploy/flux-system/fluxinstance.yaml"
    "deploy/flux-system/releases :(exclude)deploy/flux-system/releases/kustomization.yaml :(exclude)deploy/flux-system/releases/openbao.yaml :(exclude)deploy/flux-system/releases/c5c3-operator.yaml :(exclude)deploy/flux-system/releases/openbao/kustomization.yaml :(exclude)deploy/flux-system/releases/c5c3-operator/kustomization.yaml :(exclude)deploy/flux-system/releases/chaos-mesh.yaml :(exclude)deploy/flux-system/releases/openbao/helmrelease.yaml :(exclude)deploy/flux-system/releases/external-secrets.yaml :(exclude)deploy/flux-system/releases/cert-manager.yaml :(exclude)deploy/flux-system/releases/mariadb-operator.yaml :(exclude)deploy/flux-system/releases/mariadb-operator-crds.yaml :(exclude)deploy/flux-system/releases/memcached-operator.yaml :(exclude)deploy/flux-system/releases/c5c3-operator/helmrelease.yaml :(exclude)deploy/flux-system/releases/k-orc.yaml :(exclude)deploy/flux-system/releases/garage-operator.yaml :(exclude)deploy/flux-system/releases/keystone-operator.yaml :(exclude)deploy/flux-system/releases/horizon-operator.yaml :(exclude)deploy/flux-system/releases/glance-operator.yaml :(exclude)deploy/flux-system/releases/placement-operator.yaml :(exclude)deploy/flux-system/releases/openbao-operator.yaml :(exclude)deploy/flux-system/releases/barbican-operator.yaml :(exclude)deploy/flux-system/releases/ovn-operator.yaml :(exclude)deploy/flux-system/releases/neutron-operator.yaml :(exclude)deploy/flux-system/releases/cinder-operator.yaml :(exclude)deploy/flux-system/releases/nova-operator.yaml :(exclude)deploy/flux-system/releases/rabbitmq-cluster-operator.yaml :(exclude)deploy/flux-system/releases/messaging-topology-operator.yaml"
  )

  local diff_output=""
  local status=0
  local i
  # shellcheck disable=SC2068  # intentional word-splitting of the pathspec list
  for i in "${!groups_specs[@]}"; do
    # Use read -a to split the space-separated spec into pathspec args.
    local -a specs
    read -r -a specs <<< "${groups_specs[$i]}"
    if ! git -C "$PROJECT_ROOT" diff --quiet origin/main -- "${specs[@]}"; then
      status=1
      diff_output+=$'\n'"--- diff in ${groups_label[$i]} ---"$'\n'
      diff_output+="$(git -C "$PROJECT_ROOT" diff origin/main -- "${specs[@]}")"
    fi
  done

  if [[ "$status" -ne 0 ]]; then
    echo "  FAIL: production overlay has unexpected changes vs origin/main"
    echo "$diff_output" | head -80
    FAIL=$((FAIL + 1))
    return
  fi

  echo "  PASS: production overlay paths are byte-identical to origin/main"
  PASS=$((PASS + 1))
}

# --- Test 2: the source catalog renders only Flux source objects ---
test_source_catalog_is_flux_sources_only() {
  echo "Test: deploy/flux-system/sources renders only Flux source objects"

  local tool
  for tool in kustomize yq jq; do
    if ! command -v "$tool" >/dev/null 2>&1; then
      echo "  SKIP: $tool unavailable (1 check skipped)"
      SKIP=$((SKIP + 1))
      return
    fi
  done

  local rendered sources
  if ! rendered="$(kustomize build "$PROJECT_ROOT/deploy/flux-system/sources" 2>&1)"; then
    echo "  FAIL: source catalog does not render"
    printf '%s\n' "$rendered" >&2
    FAIL=$((FAIL + 1))
    return
  fi
  if ! sources="$(printf '%s\n' "$rendered" | yq -o=json -I0 '.' - | jq -s 'map(select(. != null))')"; then
    echo "  FAIL: source catalog output is not valid YAML objects"
    FAIL=$((FAIL + 1))
    return
  fi
  if ! printf '%s\n' "$sources" | jq -e 'length > 0 and all(.[];
    (.apiVersion | startswith("source.toolkit.fluxcd.io/")) and
    (.kind == "HelmRepository" or .kind == "OCIRepository" or .kind == "GitRepository"))' >/dev/null; then
    echo "  FAIL: source catalog is empty or contains non-source objects"
    printf '%s\n' "$sources" | jq -r '.[] | "    \(.apiVersion) \(.kind)/\(.metadata.name)"'
    FAIL=$((FAIL + 1))
    return
  fi

  echo "  PASS: source catalog renders only Flux source objects"
  PASS=$((PASS + 1))
}

# --- Run ---
test_production_overlay_unchanged
test_source_catalog_is_flux_sources_only

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
