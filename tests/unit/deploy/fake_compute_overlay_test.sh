#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the fake-compute opt-in overlay the ControlPlane quick start applies:
#   1. deploy/kind/fake-compute/{kustomization,fake-compute}.yaml exist with
#      SPDX headers.
#   2. The kustomization's resources list names fake-compute.yaml and nothing
#      else, and the file names no parent-directory path (kubectl's embedded
#      kustomize cannot lift the load restrictor, kubernetes/kubectl#948).
#   3. The e2e fixture the overlay mirrors exists. The overlay is that fixture
#      renamed for the quick start's ControlPlane, so a fixture that moved
#      leaves the overlay pinned to nothing.
#   4. The fixture with controlplane-keystone replaced by controlplane equals
#      the overlay, document by document, once both are parsed (yq). The
#      tutorial and the full-ControlPlane suite therefore run one Deployment.
#   5. Every secretKeyRef and the compute-config volume name the quick start's
#      contract Secret, controlplane-nova-compute-config (yq).
#   6. kustomize build of the overlay renders the ConfigMap and the Deployment
#      in openstack and nothing else (kustomize, yq).
#   7. Nothing applies the overlay by default: no non-comment line of
#      hack/deploy-infra.sh names fake-compute, and kustomize build of
#      deploy/flux-system and deploy/kind/base render no
#      controlplane-fake-compute object (kustomize).
#
# Checks 4-6 and the kustomize half of check 7 are counted as SKIP when yq or
# kustomize is not on PATH.
#
# Usage: bash tests/unit/deploy/fake_compute_overlay_test.sh
#   FAKE_COMPUTE_FIXTURE=<path> overrides the e2e fixture under comparison.

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
OVERLAY_DIR="$PROJECT_ROOT/deploy/kind/fake-compute"
OVERLAY_KUSTOMIZATION="$OVERLAY_DIR/kustomization.yaml"
OVERLAY_MANIFEST="$OVERLAY_DIR/fake-compute.yaml"
FAKE_COMPUTE_FIXTURE="${FAKE_COMPUTE_FIXTURE:-$PROJECT_ROOT/tests/e2e/c5c3/full-controlplane-keystone/06-fake-compute.yaml}"

CONTRACT_SECRET="controlplane-nova-compute-config"
# transport_url plus the keystone_authtoken, service_user, placement, neutron
# and cinder passwords. The floor keeps check 5 from passing on a manifest that
# references no Secret at all.
MIN_CONTRACT_SECRET_REFS=6

have() {
  command -v "$1" >/dev/null 2>&1
}

# Parse a YAML stream into one compact JSON line per non-empty document. The
# comment-only header in front of the first "---" parses as a null document,
# which the select drops.
json_documents() {
  yq -o=json -I=0 'select(. != null)' "$@"
}

# Count objects named controlplane-fake-compute* in a rendered stream on stdin.
count_fake_compute_objects() {
  { grep -E '^[[:space:]]*name:[[:space:]]+controlplane-fake-compute' || true; } | wc -l
}

# --- Test 1: overlay files exist with SPDX headers ---
test_overlay_files_exist_with_spdx() {
  echo "Test: deploy/kind/fake-compute/{kustomization,fake-compute}.yaml exist with SPDX headers"

  local f
  for f in "$OVERLAY_KUSTOMIZATION" "$OVERLAY_MANIFEST"; do
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

# --- Test 2: the kustomization lists fake-compute.yaml alone, no parent dirs ---
test_kustomization_is_self_contained() {
  echo "Test: kustomization lists fake-compute.yaml alone and names no parent directory"

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
  assert_eq "kustomization.yaml's resources list names exactly fake-compute.yaml" \
    "fake-compute.yaml" "$entries"

  # Comments may explain the rule; only a non-comment line can break it.
  local parent_refs
  parent_refs="$( { grep -vE '^[[:space:]]*#' "$OVERLAY_KUSTOMIZATION" | grep -F '../' || true; } | wc -l)"
  assert_eq "kustomization.yaml names no '../' path" "0" "${parent_refs// /}"
}

# --- Test 3: the e2e fixture the overlay mirrors exists ---
test_fixture_exists() {
  echo "Test: the e2e fixture the overlay mirrors exists"

  if [[ -f "$FAKE_COMPUTE_FIXTURE" ]]; then
    echo "  PASS: $FAKE_COMPUTE_FIXTURE exists"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $FAKE_COMPUTE_FIXTURE does not exist; the overlay mirrors it"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 4: the renamed fixture equals the overlay document by document ---
test_overlay_mirrors_fixture() {
  echo "Test: the fixture renamed to controlplane equals the overlay, document by document"

  if ! have yq; then
    echo "  SKIP: yq not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi
  if [[ ! -f "$FAKE_COMPUTE_FIXTURE" || ! -f "$OVERLAY_MANIFEST" ]]; then
    echo "  SKIP: fixture or overlay missing, reported above (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local want got
  if ! want="$(sed 's/controlplane-keystone/controlplane/g' "$FAKE_COMPUTE_FIXTURE" | json_documents - 2>&1)"; then
    echo "  FAIL: yq could not parse the renamed fixture: $want"
    FAIL=$((FAIL + 2))
    return
  fi
  if ! got="$(json_documents "$OVERLAY_MANIFEST" 2>&1)"; then
    echo "  FAIL: yq could not parse $OVERLAY_MANIFEST: $got"
    FAIL=$((FAIL + 2))
    return
  fi

  local want_count got_count
  want_count="$(printf '%s\n' "$want" | grep -c .)"
  got_count="$(printf '%s\n' "$got" | grep -c .)"
  assert_eq "the overlay carries as many documents as the fixture" "$want_count" "$got_count"

  local i want_doc got_doc differing=""
  for ((i = 1; i <= want_count; i++)); do
    want_doc="$(printf '%s\n' "$want" | sed -n "${i}p")"
    got_doc="$(printf '%s\n' "$got" | sed -n "${i}p")"
    if [[ "$want_doc" != "$got_doc" ]]; then
      differing="${differing} ${i}"
    fi
  done
  if [[ -z "$differing" ]]; then
    echo "  PASS: every overlay document equals its renamed fixture document"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: overlay documents differ from the renamed fixture at position(s):${differing}"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 5: every Secret reference names the quick start's contract ---
test_secret_references() {
  echo "Test: every Secret reference names $CONTRACT_SECRET"

  if ! have yq; then
    echo "  SKIP: yq not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi
  if [[ ! -f "$OVERLAY_MANIFEST" ]]; then
    echo "  SKIP: overlay missing, reported above (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local names total foreign
  names="$(yq -N -r '.. | select(tag == "!!map" and has("secretKeyRef")) | .secretKeyRef.name' "$OVERLAY_MANIFEST")"
  total="$(printf '%s\n' "$names" | grep -c . || true)"
  foreign="$(printf '%s\n' "$names" | grep -v -x -F -- "$CONTRACT_SECRET" | grep -c . || true)"
  if [[ "$total" -ge "$MIN_CONTRACT_SECRET_REFS" && "$foreign" -eq 0 ]]; then
    echo "  PASS: all $total secretKeyRef names are $CONTRACT_SECRET"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: want at least $MIN_CONTRACT_SECRET_REFS secretKeyRef names, all $CONTRACT_SECRET; got $total, $foreign foreign:"
    printf '%s\n' "$names" | sed 's/^/    /'
    FAIL=$((FAIL + 1))
  fi

  local volume_secret
  volume_secret="$(yq -N -r 'select(.kind == "Deployment") | .spec.template.spec.volumes[] | select(.name == "compute-config") | .secret.secretName' "$OVERLAY_MANIFEST")"
  assert_eq "the compute-config volume mounts $CONTRACT_SECRET" "$CONTRACT_SECRET" "$volume_secret"
}

# --- Test 6: kustomize build renders the two objects in openstack ---
test_kustomize_build_renders_overlay() {
  echo "Test: kustomize build deploy/kind/fake-compute renders the ConfigMap and the Deployment in openstack"

  if ! have kustomize; then
    echo "  SKIP: kustomize not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi
  if ! have yq; then
    echo "  SKIP: yq not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  # No --load-restrictor: kubectl apply -k cannot pass one either.
  local rendered
  if ! rendered="$(kustomize build "$OVERLAY_DIR" 2>&1)"; then
    echo "  FAIL: kustomize build $OVERLAY_DIR failed (default LoadRestrictionsRootOnly):"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 1))
    return
  fi

  local objects
  objects="$(printf '%s\n' "$rendered" \
    | yq -N -r 'select(. != null) | .kind + "/" + .metadata.name + "/" + .metadata.namespace' - \
    | sort)"
  assert_eq "the overlay renders exactly the ConfigMap and the Deployment in openstack" \
    "$(printf '%s\n%s' 'ConfigMap/controlplane-fake-compute-conf/openstack' 'Deployment/controlplane-fake-compute/openstack')" \
    "$objects"
}

# --- Test 7: the default overlays render no fake compute ---
test_default_overlays_render_no_fake_compute() {
  echo "Test: hack/deploy-infra.sh, deploy/flux-system and deploy/kind/base apply no fake compute"

  # deploy-infra.sh applies its kind overlays with kubectl apply -k directly,
  # not through the two kustomizations below. Comments may name the overlay;
  # only a non-comment line can apply it.
  if [[ ! -f "$DEPLOY_INFRA_SCRIPT" ]]; then
    echo "  FAIL: $DEPLOY_INFRA_SCRIPT does not exist"
    FAIL=$((FAIL + 1))
  else
    local script_refs
    script_refs="$( { grep -vE '^[[:space:]]*#' "$DEPLOY_INFRA_SCRIPT" | grep -F 'fake-compute' || true; } | wc -l)"
    assert_eq "hack/deploy-infra.sh never names fake-compute outside a comment" "0" "${script_refs// /}"
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
    count="$(printf '%s\n' "$rendered" | count_fake_compute_objects)"
    assert_eq "${dir#"$PROJECT_ROOT"/} renders zero controlplane-fake-compute objects" \
      "0" "${count// /}"
  done
}

# --- Run ---
test_overlay_files_exist_with_spdx
test_kustomization_is_self_contained
test_fixture_exists
test_overlay_mirrors_fixture
test_secret_references
test_kustomize_build_renders_overlay
test_default_overlays_render_no_fake_compute

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
