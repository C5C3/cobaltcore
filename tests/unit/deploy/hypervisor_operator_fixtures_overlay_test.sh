#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the hypervisor-operator fixture overlay, the K-ORC example of the
# OpenStack objects openstack-hypervisor-operator expects:
#   1. deploy/kind/hypervisor-operator-fixtures/{kustomization,fixtures,image}.yaml
#      exist with SPDX headers.
#   2. The kustomization's resources list names fixtures.yaml and image.yaml
#      and nothing else, and the file names no parent-directory path
#      (kubectl's embedded kustomize cannot lift the load restrictor,
#      kubernetes/kubectl#948).
#   3. kustomize build renders 11 objects, all in openstack, each
#      authenticating through k-orc-clouds-yaml, cloud admin (kustomize, yq).
#   4. The names the hypervisor operator hard-codes: flavor ID "1", project
#      "test" in domain "cc3test", and the image name, URL and sha256 (yq).
#   5. The User import names the account the ControlPlane provisions, the
#      NovaHypervisorOperatorAccountName literal in controlplane_webhook.go (yq).
#   6. Nothing applies the overlay by default: no non-comment line of
#      hack/deploy-infra.sh names hypervisor-operator-fixtures, and kustomize
#      build of deploy/flux-system and deploy/kind/base render no hvo- object
#      (kustomize).
#
# Checks 3-5 and the kustomize half of check 6 are counted as SKIP when yq or
# kustomize is not on PATH.
#
# Usage: bash tests/unit/deploy/hypervisor_operator_fixtures_overlay_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

DEPLOY_INFRA_SCRIPT="$PROJECT_ROOT/hack/deploy-infra.sh"
FLUX_SYSTEM_DIR="$PROJECT_ROOT/deploy/flux-system"
KIND_BASE_DIR="$PROJECT_ROOT/deploy/kind/base"
OVERLAY_DIR="$PROJECT_ROOT/deploy/kind/hypervisor-operator-fixtures"
OVERLAY_KUSTOMIZATION="$OVERLAY_DIR/kustomization.yaml"
OVERLAY_FIXTURES="$OVERLAY_DIR/fixtures.yaml"
OVERLAY_IMAGE="$OVERLAY_DIR/image.yaml"
WEBHOOK_SOURCE="$PROJECT_ROOT/operators/c5c3/api/v1alpha1/controlplane_webhook.go"

CIRROS_URL="https://download.cirros-cloud.net/0.6.3/cirros-0.6.3-x86_64-disk.img"
CIRROS_SHA256="7d6355852aeb6dbcd191bcda7cd74f1536cfe5cbf8a10495a7283a8396e4b75b"

have() {
  command -v "$1" >/dev/null 2>&1
}

# The first value a yq expression yields over the rendered overlay on stdin.
rendered_value() {
  yq -N -r "select(. != null) | $1" - | head -n 1
}

# --- Test 1: overlay files exist with SPDX headers ---
test_overlay_files_exist_with_spdx() {
  echo "Test: deploy/kind/hypervisor-operator-fixtures/{kustomization,fixtures,image}.yaml exist with SPDX headers"

  local f
  for f in "$OVERLAY_KUSTOMIZATION" "$OVERLAY_FIXTURES" "$OVERLAY_IMAGE"; do
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

# --- Test 2: the kustomization lists the two files alone, no parent dirs ---
test_kustomization_is_self_contained() {
  echo "Test: kustomization lists fixtures.yaml and image.yaml alone and names no parent directory"

  if [[ ! -f "$OVERLAY_KUSTOMIZATION" ]]; then
    echo "  FAIL: $OVERLAY_KUSTOMIZATION does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  # The items of the top-level resources key only: the range ends at the next
  # top-level key, so the list of an images: or patches: block is not read as a
  # resource.
  local entries
  entries="$(awk '
    /^resources:/ { in_list = 1; next }
    in_list && /^[^[:space:]#-]/ { in_list = 0 }
    in_list && /^[[:space:]]*-[[:space:]]+/ {
      sub(/^[[:space:]]*-[[:space:]]+/, ""); sub(/[[:space:]]+$/, ""); print
    }
  ' "$OVERLAY_KUSTOMIZATION")"
  assert_eq "kustomization.yaml's resources list names exactly fixtures.yaml and image.yaml" \
    "$(printf '%s\n%s' fixtures.yaml image.yaml)" "$entries"

  # Comments may explain the rule; only a non-comment line can break it.
  local parent_refs
  parent_refs="$( { grep -vE '^[[:space:]]*#' "$OVERLAY_KUSTOMIZATION" | grep -F '../' || true; } | wc -l)"
  assert_eq "kustomization.yaml names no '../' path" "0" "${parent_refs// /}"
}

# --- Tests 3-5: the rendered overlay ---
test_rendered_overlay() {
  echo "Test: kustomize build renders the fixtures with the names the hypervisor operator hard-codes"

  if ! have kustomize; then
    echo "  SKIP: kustomize not installed (12 checks skipped)"
    SKIP=$((SKIP + 12))
    return
  fi
  if ! have yq; then
    echo "  SKIP: yq not installed (12 checks skipped)"
    SKIP=$((SKIP + 12))
    return
  fi

  # No --load-restrictor: kubectl apply -k cannot pass one either.
  local rendered
  if ! rendered="$(kustomize build "$OVERLAY_DIR" 2>&1)"; then
    echo "  FAIL: kustomize build $OVERLAY_DIR failed (default LoadRestrictionsRootOnly):"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 12))
    return
  fi

  local count outside uncredentialed
  count="$(printf '%s\n' "$rendered" | yq -N -r 'select(. != null) | .kind' - | grep -c .)"
  assert_eq "the overlay renders 11 objects" "11" "$count"
  outside="$(printf '%s\n' "$rendered" \
    | yq -N -r 'select(. != null and .metadata.namespace != "openstack") | .kind + "/" + .metadata.name' -)"
  assert_eq "every object lives in openstack" "" "$outside"
  uncredentialed="$(printf '%s\n' "$rendered" \
    | yq -N -r 'select(. != null and (.spec.cloudCredentialsRef.secretName != "k-orc-clouds-yaml" or .spec.cloudCredentialsRef.cloudName != "admin")) | .kind + "/" + .metadata.name' -)"
  assert_eq "every object authenticates through k-orc-clouds-yaml, cloud admin" "" "$uncredentialed"

  assert_eq "the Flavor's ID is \"1\"" "1" \
    "$(printf '%s\n' "$rendered" | rendered_value 'select(.kind == "Flavor") | .spec.resource.id')"
  assert_eq "the Flavor's ID is a string, not a number" "!!str" \
    "$(printf '%s\n' "$rendered" | rendered_value 'select(.kind == "Flavor") | .spec.resource.id | tag')"
  assert_eq "the managed Project is named test" "test" \
    "$(printf '%s\n' "$rendered" | rendered_value 'select(.kind == "Project") | .spec.resource.name')"
  assert_eq "the Project lives in the managed Domain" "hvo-cc3test" \
    "$(printf '%s\n' "$rendered" | rendered_value 'select(.kind == "Project") | .spec.resource.domainRef')"
  assert_eq "the managed Domain is named cc3test" "cc3test" \
    "$(printf '%s\n' "$rendered" | rendered_value 'select(.kind == "Domain" and .metadata.name == "hvo-cc3test") | .spec.resource.name')"
  assert_eq "the Image is named cirros-kvm" "cirros-kvm" \
    "$(printf '%s\n' "$rendered" | rendered_value 'select(.kind == "Image") | .spec.resource.name')"
  assert_eq "the Image downloads cirros 0.6.3" "$CIRROS_URL" \
    "$(printf '%s\n' "$rendered" | rendered_value 'select(.kind == "Image") | .spec.resource.content.download.url')"
  assert_eq "the Image checks the upstream sha256" "sha256:$CIRROS_SHA256" \
    "$(printf '%s\n' "$rendered" | rendered_value 'select(.kind == "Image") | .spec.resource.content.download.hash | .algorithm + ":" + .value')"

  local account user
  account="$(sed -n 's/^const NovaHypervisorOperatorAccountName = "\(.*\)"$/\1/p' "$WEBHOOK_SOURCE")"
  user="$(printf '%s\n' "$rendered" | rendered_value 'select(.kind == "User") | .spec.import.filter.name')"
  if [[ -n "$account" && "$user" == "$account" ]]; then
    echo "  PASS: the User import names the ControlPlane's account ($account)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the User import names '$user'; controlplane_webhook.go provisions '$account'"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 6: the default overlays render no fixture ---
test_default_overlays_render_no_fixtures() {
  echo "Test: hack/deploy-infra.sh, deploy/flux-system and deploy/kind/base apply no hypervisor-operator fixture"

  # deploy-infra.sh applies its kind overlays with kubectl apply -k directly,
  # not through the two kustomizations below. Comments may name the overlay;
  # only a non-comment line can apply it.
  if [[ ! -f "$DEPLOY_INFRA_SCRIPT" ]]; then
    echo "  FAIL: $DEPLOY_INFRA_SCRIPT does not exist"
    FAIL=$((FAIL + 1))
  else
    local script_refs
    script_refs="$( { grep -vE '^[[:space:]]*#' "$DEPLOY_INFRA_SCRIPT" | grep -F 'hypervisor-operator-fixtures' || true; } | wc -l)"
    assert_eq "hack/deploy-infra.sh never names hypervisor-operator-fixtures outside a comment" "0" "${script_refs// /}"
  fi

  if ! have kustomize; then
    echo "  SKIP: kustomize not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local dir rendered count
  for dir in "$FLUX_SYSTEM_DIR" "$KIND_BASE_DIR"; do
    if ! rendered="$(kustomize build "$dir" 2>&1)"; then
      echo "  FAIL: kustomize build $dir failed:"
      echo "$rendered" | head -20
      FAIL=$((FAIL + 1))
      continue
    fi
    count="$( { printf '%s\n' "$rendered" | grep -E '^[[:space:]]*name:[[:space:]]+hvo-' || true; } | wc -l)"
    assert_eq "${dir#"$PROJECT_ROOT"/} renders zero hvo- objects" "0" "${count// /}"
  done
}

# --- Run ---
test_overlay_files_exist_with_spdx
test_kustomization_is_self_contained
test_rendered_overlay
test_default_overlays_render_no_fixtures

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
