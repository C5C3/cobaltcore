#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the metrics-server opt-in overlay:
#   - deploy/kind/metrics-server/{kustomization,source,release}.yaml exist
#     with SPDX headers, the kustomization references the local
#     source/release files (no parent-directory paths), and the overlay does
#     NOT create a Namespace (the chart installs into the pre-existing
#     kube-system Namespace).
#   - kustomize build of the overlay renders the expected document set under
#     the default LoadRestrictionsRootOnly security check (no
#     --load-restrictor flag): HelmRepository/metrics-server in flux-system at
#     the upstream chart index, and HelmRelease/metrics-server in kube-system
#     whose chart version stays within major 3 and whose values carry
#     `--kubelet-insecure-tls`.
#   - kustomize build of deploy/flux-system and deploy/kind/base renders ZERO
#     metrics-server resources (production / default posture).
# Usage: bash tests/unit/deploy/metrics_server_overlay_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

FLUX_SYSTEM_DIR="$PROJECT_ROOT/deploy/flux-system"
KIND_BASE_DIR="$PROJECT_ROOT/deploy/kind/base"
KIND_MS_DIR="$PROJECT_ROOT/deploy/kind/metrics-server"
KIND_MS_KUSTOMIZATION="$KIND_MS_DIR/kustomization.yaml"
KIND_MS_SOURCE="$KIND_MS_DIR/source.yaml"
KIND_MS_RELEASE="$KIND_MS_DIR/release.yaml"

# Count resources of a given kind named metrics-server in a rendered stream.
# Reads the stream on stdin; prints the match count.
count_metrics_server_resources() {
  local kind="$1"
  awk -v want="$kind" '
      /^---$/ { k=""; }
      $0 ~ "^kind:[[:space:]]+" want "[[:space:]]*$" { k=want }
      /^[[:space:]]*name:[[:space:]]+metrics-server[[:space:]]*$/ {
        if (k == want) { print; k="" }
      }
    ' | wc -l
}

# --- Test 1: overlay files exist with SPDX headers ---
test_overlay_files_exist_with_spdx() {
  echo "Test: deploy/kind/metrics-server/{kustomization,source,release}.yaml exist with SPDX headers"

  local f
  for f in "$KIND_MS_KUSTOMIZATION" "$KIND_MS_SOURCE" "$KIND_MS_RELEASE"; do
    if [[ ! -f "$f" ]]; then
      echo "  FAIL: $f does not exist"
      FAIL=$((FAIL + 1))
      continue
    fi
    assert_file_contains "$(basename "$f") has SPDX FileCopyrightText header" \
      "$f" "SPDX-FileCopyrightText: Copyright 2026 SAP SE"
    assert_file_contains "$(basename "$f") has SPDX-License-Identifier: Apache-2.0" \
      "$f" "SPDX-License-Identifier: Apache-2.0"
  done
}

# --- Test 2: kustomization references local source/release, no parent dirs,
#             and creates no Namespace ---
test_kustomization_is_self_contained() {
  echo "Test: kustomization references local source/release and is self-contained"

  if [[ ! -f "$KIND_MS_KUSTOMIZATION" ]]; then
    echo "  FAIL: $KIND_MS_KUSTOMIZATION does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_file_contains "kustomization.yaml references the local source.yaml" \
    "$KIND_MS_KUSTOMIZATION" "source.yaml$"
  assert_file_contains "kustomization.yaml references the local release.yaml" \
    "$KIND_MS_KUSTOMIZATION" "release.yaml$"

  # Pin the no-parent-dir contract: a `../../` reference would re-introduce
  # the kubectl#948 load-restrictor failure.
  local parent_refs
  parent_refs="$( { grep -E '^[[:space:]]*-[[:space:]]+\.\./\.\.' "$KIND_MS_KUSTOMIZATION" || true; } | wc -l)"
  assert_eq "kustomization.yaml has no '../../' parent-directory resource entries" \
    "0" "${parent_refs// /}"

  # The overlay must NOT ship an inline namespace.yaml — the chart installs
  # into the pre-existing kube-system Namespace.
  local ns_entry
  ns_entry="$( { grep -E '^[[:space:]]*-[[:space:]]+namespace\.yaml[[:space:]]*$' "$KIND_MS_KUSTOMIZATION" || true; } | wc -l)"
  assert_eq "kustomization.yaml does not list a namespace.yaml resource" \
    "0" "${ns_entry// /}"
}

# --- Test 3: kustomize build renders the metrics-server bundle with kind
#             tuning under the default LoadRestrictionsRootOnly check ---
test_kustomize_build_renders_bundle() {
  echo "Test: kustomize build deploy/kind/metrics-server renders HelmRepository + HelmRelease with kind tuning"

  if ! command -v kustomize >/dev/null 2>&1; then
    echo "  SKIP: kustomize not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  # Mirror the production invocation: NO --load-restrictor flag. kubectl's
  # embedded kustomize (used by hack/deploy-infra.sh) does not expose one.
  local rendered
  if ! rendered="$(kustomize build "$KIND_MS_DIR" 2>&1)"; then
    echo "  FAIL: kustomize build $KIND_MS_DIR failed (default LoadRestrictionsRootOnly):"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 5))
    return
  fi

  # HelmRepository in flux-system at the upstream chart index.
  local repo_ns repo_url
  repo_ns="$(printf '%s\n' "$rendered" | yq -r \
    'select(.kind == "HelmRepository" and .metadata.name == "metrics-server") | .metadata.namespace // ""' \
    2>/dev/null | head -n1)"
  repo_url="$(printf '%s\n' "$rendered" | yq -r \
    'select(.kind == "HelmRepository" and .metadata.name == "metrics-server") | .spec.url // ""' \
    2>/dev/null | head -n1)"
  assert_eq "HelmRepository/metrics-server lives in flux-system" "flux-system" "$repo_ns"
  assert_eq "HelmRepository/metrics-server points at the upstream chart index" \
    "https://kubernetes-sigs.github.io/metrics-server/" "$repo_url"

  # HelmRelease in kube-system with a major-3 version range and the
  # kubelet-insecure-tls arg.
  local rel_ns version arg
  rel_ns="$(printf '%s\n' "$rendered" | yq -r \
    'select(.kind == "HelmRelease" and .metadata.name == "metrics-server") | .metadata.namespace // ""' \
    2>/dev/null | head -n1)"
  version="$(printf '%s\n' "$rendered" | yq -r \
    'select(.kind == "HelmRelease" and .metadata.name == "metrics-server") | .spec.chart.spec.version // ""' \
    2>/dev/null | head -n1)"
  arg="$(printf '%s\n' "$rendered" | yq -r \
    'select(.kind == "HelmRelease" and .metadata.name == "metrics-server") | .spec.values.args[] | select(. == "--kubelet-insecure-tls")' \
    2>/dev/null | head -n1)"

  assert_eq "HelmRelease/metrics-server lives in kube-system" "kube-system" "$rel_ns"
  assert_eq "HelmRelease/metrics-server passes --kubelet-insecure-tls" \
    "--kubelet-insecure-tls" "$arg"

  # The version range must be pinned within major 3 so in-range updates need
  # no Renovate pin (mirrors the chaos-mesh / kube-prometheus-stack posture).
  if printf '%s' "$version" | grep -qE '>=3\.[0-9]+\.[0-9]+ <4\.0\.0'; then
    echo "  PASS: chart version range '$version' stays within major 3"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: chart version range '$version' is not a '>=3.x.y <4.0.0' single-major range"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 4: production flux-system renders zero metrics-server resources ---
test_flux_system_renders_no_metrics_server() {
  echo "Test: kustomize build deploy/flux-system renders no metrics-server resources"

  if ! command -v kustomize >/dev/null 2>&1; then
    echo "  SKIP: kustomize not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local rendered
  if ! rendered="$(kustomize build "$FLUX_SYSTEM_DIR" 2>&1)"; then
    echo "  FAIL: kustomize build $FLUX_SYSTEM_DIR failed:"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 2))
    return
  fi

  local repo_count release_count
  repo_count="$(printf '%s\n' "$rendered" | count_metrics_server_resources HelmRepository)"
  release_count="$(printf '%s\n' "$rendered" | count_metrics_server_resources HelmRelease)"
  assert_eq "production overlay renders zero HelmRepository/metrics-server" "0" "${repo_count// /}"
  assert_eq "production overlay renders zero HelmRelease/metrics-server" "0" "${release_count// /}"
}

# --- Test 5: kind/base renders zero metrics-server resources ---
test_kind_base_renders_no_metrics_server() {
  echo "Test: kustomize build deploy/kind/base renders no metrics-server resources"

  if ! command -v kustomize >/dev/null 2>&1; then
    echo "  SKIP: kustomize not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local rendered
  if ! rendered="$(kustomize build "$KIND_BASE_DIR" 2>&1)"; then
    echo "  FAIL: kustomize build $KIND_BASE_DIR failed:"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 1))
    return
  fi

  local resource_count
  resource_count="$(printf '%s\n' "$rendered" \
    | grep -cE '^[[:space:]]*name:[[:space:]]+metrics-server[[:space:]]*$' || true)"
  assert_eq "kind/base output contains no resource named metrics-server" "0" "${resource_count// /}"
}

# --- Run ---
test_overlay_files_exist_with_spdx
test_kustomization_is_self_contained
test_kustomize_build_renders_bundle
test_flux_system_renders_no_metrics_server
test_kind_base_renders_no_metrics_server

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
