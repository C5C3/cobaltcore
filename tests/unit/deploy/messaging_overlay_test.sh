#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the message-bus opt-in overlay:
#   - deploy/kind/messaging/{kustomization,shared-rabbitmq}.yaml exist with
#     SPDX headers, and the kustomization references the one local file and
#     nothing outside the overlay (no parent-directory paths, no remote URLs).
#   - kustomize build of the overlay renders one document and no more, under
#     the default LoadRestrictionsRootOnly security check (no --load-restrictor
#     flag): the shared-rabbitmq RabbitmqCluster in openstack.
#   - That broker carries the sizing the 4-vCPU kind node has room for: one
#     replica, 100m CPU and 512Mi memory requested, 512Mi memory limited, and
#     no CPU limit (a limit would throttle the broker during a suite).
#   - kustomize build of deploy/flux-system, deploy/kind/base and
#     deploy/kind/infrastructure renders ZERO RabbitmqCluster documents
#     (default posture: no broker unless the overlay is asked for).
# Usage: bash tests/unit/deploy/messaging_overlay_test.sh

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
KIND_INFRA_DIR="$PROJECT_ROOT/deploy/kind/infrastructure"
KIND_MESSAGING_DIR="$PROJECT_ROOT/deploy/kind/messaging"
KIND_MESSAGING_KUSTOMIZATION="$KIND_MESSAGING_DIR/kustomization.yaml"
KIND_MESSAGING_BROKER="$KIND_MESSAGING_DIR/shared-rabbitmq.yaml"

# Read a single value out of a rendered stream. Prints the first line of the
# yq result, or the empty string when the expression matches nothing.
render_value() {
  local stream="$1" expression="$2"
  printf '%s\n' "$stream" | yq -r "$expression" 2>/dev/null \
    | grep -v '^---$' | grep -v '^null$' | head -n1
}

# Count lines of the rendered stream matching an extended regular expression.
count_matches() {
  local stream="$1" pattern="$2"
  printf '%s\n' "$stream" | { grep -cE "$pattern" || true; }
}

# Render a kustomization the way hack/deploy-infra.sh does: no
# --load-restrictor flag, because kubectl's embedded kustomize has none.
# Prints the rendered stream on success, the error output on failure.
render_dir() {
  kustomize build "$1" 2>&1
}

# Assert that a rendered stream carries no RabbitmqCluster. Used for the three
# default-posture directories.
assert_renders_no_broker() {
  local label="$1" dir="$2"

  if ! command -v kustomize >/dev/null 2>&1; then
    echo "  SKIP: kustomize not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local rendered
  if ! rendered="$(render_dir "$dir")"; then
    echo "  FAIL: kustomize build $dir failed:"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 1))
    return
  fi

  local broker_count
  broker_count="$(count_matches "$rendered" '^kind:[[:space:]]+RabbitmqCluster[[:space:]]*$')"
  assert_eq "$label renders no RabbitmqCluster" "0" "${broker_count// /}"
}

# --- Test 1: overlay files exist with SPDX headers ---
test_overlay_files_exist_with_spdx() {
  echo "Test: deploy/kind/messaging/{kustomization,shared-rabbitmq}.yaml exist with SPDX headers"

  local f
  for f in "$KIND_MESSAGING_KUSTOMIZATION" "$KIND_MESSAGING_BROKER"; do
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

# --- Test 2: the kustomization is self-contained ---
test_kustomization_is_self_contained() {
  echo "Test: kustomization references the one local file and is self-contained"

  if [[ ! -f "$KIND_MESSAGING_KUSTOMIZATION" ]]; then
    echo "  FAIL: $KIND_MESSAGING_KUSTOMIZATION does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_file_contains "kustomization.yaml references the local shared-rabbitmq.yaml" \
    "$KIND_MESSAGING_KUSTOMIZATION" "shared-rabbitmq.yaml$"

  # Pin the no-parent-dir contract: any `../` reference re-introduces the
  # kubectl#948 load-restrictor failure, not only a `../../` one.
  local parent_refs
  parent_refs="$( { grep -cE '^[[:space:]]*-[[:space:]]+\.\./' "$KIND_MESSAGING_KUSTOMIZATION" || true; } )"
  assert_eq "kustomization.yaml has no parent-directory resource entries" \
    "0" "${parent_refs// /}"

  # A remote base would make the apply depend on network reachability of a
  # third-party repository at deploy time.
  local url_refs
  url_refs="$( { grep -cE '^[[:space:]]*-[[:space:]]+(https?|git|oci)://' "$KIND_MESSAGING_KUSTOMIZATION" || true; } )"
  assert_eq "kustomization.yaml has no remote resource entries" \
    "0" "${url_refs// /}"
}

# --- Test 3: kustomize build renders the single expected document ---
test_kustomize_build_renders_one_document() {
  echo "Test: kustomize build deploy/kind/messaging renders one document"

  if ! command -v kustomize >/dev/null 2>&1; then
    echo "  SKIP: kustomize not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local rendered
  if ! rendered="$(render_dir "$KIND_MESSAGING_DIR")"; then
    echo "  FAIL: kustomize build $KIND_MESSAGING_DIR failed (default LoadRestrictionsRootOnly):"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 1))
    return
  fi

  local actual
  actual="$(printf '%s\n' "$rendered" \
    | yq -r '.kind + "/" + .metadata.name + "@" + (.metadata.namespace // "")' 2>/dev/null \
    | grep -v '^---$' | sort)"
  assert_eq "the overlay renders the broker and nothing else" \
    "RabbitmqCluster/shared-rabbitmq@openstack" "$actual"
}

# --- Test 4: the broker carries the kind sizing ---
test_broker_sizing() {
  echo "Test: the shared-rabbitmq broker is sized for the 4-vCPU kind node"

  if ! command -v kustomize >/dev/null 2>&1 || ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: kustomize or yq not installed (6 checks skipped)"
    SKIP=$((SKIP + 6))
    return
  fi

  local rendered
  if ! rendered="$(render_dir "$KIND_MESSAGING_DIR")"; then
    echo "  FAIL: kustomize build $KIND_MESSAGING_DIR failed:"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 6))
    return
  fi

  local cr='select(.kind == "RabbitmqCluster" and .metadata.name == "shared-rabbitmq")'

  assert_eq "the broker declares the v1beta1 API of the cluster operator" \
    "rabbitmq.com/v1beta1" "$(render_value "$rendered" "$cr | .apiVersion")"
  assert_eq "the broker runs a single replica" \
    "1" "$(render_value "$rendered" "$cr | .spec.replicas")"

  # The operator's own default is 1 CPU / 2Gi per pod, which takes the last
  # schedulable CPU on the kind node and leaves the next pod Pending.
  assert_eq "the broker requests 100m CPU" \
    "100m" "$(render_value "$rendered" "$cr | .spec.resources.requests.cpu")"
  assert_eq "the broker requests 512Mi memory" \
    "512Mi" "$(render_value "$rendered" "$cr | .spec.resources.requests.memory")"
  assert_eq "the broker limits memory to 512Mi" \
    "512Mi" "$(render_value "$rendered" "$cr | .spec.resources.limits.memory")"

  # No CPU limit: a limit throttles the broker mid-suite, while the request
  # is what the scheduler reads.
  assert_eq "the broker declares no CPU limit" \
    "" "$(render_value "$rendered" "$cr | .spec.resources.limits.cpu")"
}

# --- Tests 5-7: default posture renders no broker ---
test_flux_system_renders_no_broker() {
  echo "Test: kustomize build deploy/flux-system renders no RabbitmqCluster"
  assert_renders_no_broker "the production overlay" "$FLUX_SYSTEM_DIR"
}

test_kind_base_renders_no_broker() {
  echo "Test: kustomize build deploy/kind/base renders no RabbitmqCluster"
  assert_renders_no_broker "kind/base" "$KIND_BASE_DIR"
}

test_kind_infrastructure_renders_no_broker() {
  echo "Test: kustomize build deploy/kind/infrastructure renders no RabbitmqCluster"
  assert_renders_no_broker "kind/infrastructure" "$KIND_INFRA_DIR"
}

# --- Run ---
test_overlay_files_exist_with_spdx
test_kustomization_is_self_contained
test_kustomize_build_renders_one_document
test_broker_sizing
test_flux_system_renders_no_broker
test_kind_base_renders_no_broker
test_kind_infrastructure_renders_no_broker

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
