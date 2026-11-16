#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the frr-k8s opt-in overlay:
#   - deploy/overlays/kind/frr-k8s/{kustomization,namespace,source,release}.yaml
#     exist with SPDX headers, and the kustomization lists the three local
#     files in order, with no parent-directory path.
#   - kustomize build of the overlay renders exactly three objects under the
#     default LoadRestrictionsRootOnly check: the privileged Namespace
#     frr-k8s-system, HelmRepository/frr-k8s in flux-system, and
#     HelmRelease/frr-k8s in frr-k8s-system with an exact chart version, both
#     image tags of the form <version>@sha256:<digest>, and the per-node
#     opt-in label as the node selector. The chart defaults the stack relies
#     on are not overridden.
#   - kustomize build of deploy/bases/operators and deploy/overlays/kind/base
#     renders nothing named frr-k8s (production / default posture), and no
#     metal-stack overlay names frr-k8s: a node with a host FRR never runs it.
# Usage: bash tests/unit/deploy/frr_k8s_overlay_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

OVERLAY_DIR="$PROJECT_ROOT/deploy/overlays/kind/frr-k8s"
KUSTOMIZATION="$OVERLAY_DIR/kustomization.yaml"
OPERATORS_DIR="$PROJECT_ROOT/deploy/bases/operators"
KIND_BASE_DIR="$PROJECT_ROOT/deploy/overlays/kind/base"
METAL_STACK_DIR="$PROJECT_ROOT/deploy/overlays/metal-stack"

TAG_RE='^v?[0-9][0-9.]*@sha256:[0-9a-f]{64}$'

test_overlay_files_exist_with_spdx() {
  echo "Test: deploy/overlays/kind/frr-k8s/ holds four files with SPDX headers"

  local f
  for f in kustomization.yaml namespace.yaml source.yaml release.yaml; do
    if [[ ! -f "$OVERLAY_DIR/$f" ]]; then
      echo "  FAIL: $OVERLAY_DIR/$f does not exist"
      FAIL=$((FAIL + 1))
      continue
    fi
    assert_file_contains "$f has SPDX FileCopyrightText header" \
      "$OVERLAY_DIR/$f" "SPDX-FileCopyrightText: Copyright 2026 SAP SE"
    assert_file_contains "$f has SPDX-License-Identifier: Apache-2.0" \
      "$OVERLAY_DIR/$f" "SPDX-License-Identifier: Apache-2.0"
  done
}

test_kustomization_is_self_contained() {
  echo "Test: the kustomization lists the three local files in order"

  if [[ ! -f "$KUSTOMIZATION" ]]; then
    echo "  FAIL: $KUSTOMIZATION does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  local entries
  entries="$(awk '/^resources:/ { r = 1; next } r && /^[^[:space:]#]/ { r = 0 } r && /^[[:space:]]*-[[:space:]]/ { print $2 }' "$KUSTOMIZATION" | paste -sd' ' -)"
  assert_eq "resources are namespace.yaml, source.yaml, release.yaml" \
    "namespace.yaml source.yaml release.yaml" "$entries"

  local parent_refs
  parent_refs="$( { grep -E '^[[:space:]]*-[[:space:]]+\.\./' "$KUSTOMIZATION" || true; } | wc -l)"
  assert_eq "kustomization.yaml has no '../' resource entries" "0" "${parent_refs// /}"
}

test_kustomize_build_renders_bundle() {
  echo "Test: kustomize build deploy/overlays/kind/frr-k8s renders the pinned bundle"

  if ! command -v kustomize >/dev/null 2>&1 || ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: kustomize or yq not installed (16 checks skipped)"
    SKIP=$((SKIP + 16))
    return
  fi

  # The production invocation: NO --load-restrictor flag.
  local rendered
  if ! rendered="$(kustomize build "$OVERLAY_DIR" 2>&1)"; then
    echo "  FAIL: kustomize build $OVERLAY_DIR failed:"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 16))
    return
  fi

  q() { printf '%s\n' "$rendered" | yq -r "$1" 2>/dev/null; }

  assert_eq "the overlay renders exactly three objects" \
    "HelmRelease/frr-k8s HelmRepository/frr-k8s Namespace/frr-k8s-system" \
    "$(q '.kind + "/" + .metadata.name' | grep -v '^---$' | sort | paste -sd' ' -)"

  assert_eq "Namespace frr-k8s-system enforces the privileged level" "privileged" \
    "$(q 'select(.kind == "Namespace") | .metadata.labels["pod-security.kubernetes.io/enforce"]')"

  local repo='select(.kind == "HelmRepository" and .metadata.name == "frr-k8s")'
  assert_eq "HelmRepository/frr-k8s lives in flux-system" "flux-system" "$(q "$repo | .metadata.namespace")"
  assert_eq "HelmRepository/frr-k8s points at the upstream chart index" \
    "https://metallb.github.io/frr-k8s" "$(q "$repo | .spec.url")"
  assert_eq "HelmRepository/frr-k8s refreshes hourly" "1h" "$(q "$repo | .spec.interval")"

  local rel='select(.kind == "HelmRelease" and .metadata.name == "frr-k8s")'
  assert_eq "HelmRelease/frr-k8s lives in frr-k8s-system" "frr-k8s-system" "$(q "$rel | .metadata.namespace")"
  assert_eq "HelmRelease/frr-k8s pins chart 0.0.27 exactly" "0.0.27" "$(q "$rel | .spec.chart.spec.version")"
  assert_eq "HelmRelease/frr-k8s reads HelmRepository/frr-k8s in flux-system" \
    "HelmRepository/frr-k8s/flux-system" \
    "$(q "$rel | .spec.chart.spec.sourceRef | .kind + \"/\" + .name + \"/\" + .namespace")"
  assert_eq "install and upgrade retry three times" "3 3" \
    "$(q "$rel | (.spec.install.remediation.retries | tostring) + \" \" + (.spec.upgrade.remediation.retries | tostring)")"
  assert_eq "HelmRelease/frr-k8s has no dependsOn" "null" "$(q "$rel | .spec.dependsOn")"

  assert_eq "the frr-k8s image comes from quay.io/metallb/frr-k8s" "quay.io/metallb/frr-k8s" \
    "$(q "$rel | .spec.values.frrk8s.image.repository")"
  assert_eq "the FRR image comes from quay.io/frrouting/frr" "quay.io/frrouting/frr" \
    "$(q "$rel | .spec.values.frrk8s.frr.image.repository")"

  local tag
  for tag in "$(q "$rel | .spec.values.frrk8s.image.tag")" "$(q "$rel | .spec.values.frrk8s.frr.image.tag")"; do
    if grep -qE "$TAG_RE" <<<"$tag"; then
      echo "  PASS: image tag '$tag' is <version>@sha256:<64 hex>"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: image tag '$tag' is not <version>@sha256:<64 hex>"
      FAIL=$((FAIL + 1))
    fi
  done

  assert_eq "the DaemonSet selects only nodes labelled openstack.c5c3.io/frr-k8s=true" \
    '{"openstack.c5c3.io/frr-k8s":"true"}' \
    "$(q "$rel | .spec.values.frrk8s.nodeSelector" | yq -o=json -I=0 '.')"

  # acceptIncomingBGPConnections, crds and prometheus stay at chart default.
  assert_eq "no value overrides the chart defaults the stack relies on" "false false false" \
    "$(q "$rel | (.spec.values.frrk8s.frr | has(\"acceptIncomingBGPConnections\") | tostring) + \" \" + (.spec.values | has(\"crds\") | tostring) + \" \" + (.spec.values | has(\"prometheus\") | tostring)")"
}

# The exact-pin regex must reject what a range or a bare tag looks like, or the
# check above proves nothing.
test_tag_regex_rejects_unpinned_tags() {
  echo "Test: the tag check rejects a bare tag, a digest without a version and a short digest"

  local bad
  for bad in "v0.0.27" "sha256:de862b74a3a314eb76c8fb4cabc9d2c55cca51002cd022704c32df717f9366b7" \
    "10.6.2@sha256:89e5e1ec" ""; do
    if grep -qE "$TAG_RE" <<<"$bad"; then
      echo "  FAIL: the tag check accepts '$bad'"
      FAIL=$((FAIL + 1))
    else
      echo "  PASS: the tag check rejects '$bad'"
      PASS=$((PASS + 1))
    fi
  done
}

test_default_overlays_render_no_frr_k8s() {
  echo "Test: deploy/bases/operators and deploy/overlays/kind/base render nothing named frr-k8s"

  if ! command -v kustomize >/dev/null 2>&1; then
    echo "  SKIP: kustomize not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local dir rendered count
  for dir in "$OPERATORS_DIR" "$KIND_BASE_DIR"; do
    if ! rendered="$(kustomize build "$dir" 2>&1)"; then
      echo "  FAIL: kustomize build $dir failed:"
      echo "$rendered" | head -20
      FAIL=$((FAIL + 1))
      continue
    fi
    count="$(grep -cE '^[[:space:]]*(name|namespace):[[:space:]]+frr-k8s(-system)?[[:space:]]*$' <<<"$rendered" || true)"
    assert_eq "${dir#"$PROJECT_ROOT"/} renders no object named frr-k8s" "0" "$count"
  done
}

test_metal_stack_never_names_frr_k8s() {
  echo "Test: no metal-stack overlay installs frr-k8s"

  local hits
  hits="$(grep -rl 'frr-k8s' "$METAL_STACK_DIR" 2>/dev/null || true)"
  assert_eq "no file under deploy/overlays/metal-stack/ names frr-k8s" "" "$hits"
}

test_overlay_files_exist_with_spdx
test_kustomization_is_self_contained
test_kustomize_build_renders_bundle
test_tag_regex_rejects_unpinned_tags
test_default_overlays_render_no_frr_k8s
test_metal_stack_never_names_frr_k8s

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
