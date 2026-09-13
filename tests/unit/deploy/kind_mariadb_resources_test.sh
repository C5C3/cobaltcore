#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the memory posture of the kind openstack-db MariaDB:
#
#   - kustomize build deploy/kind/infrastructure renders MariaDB/openstack-db
#     with a memory request equal to its memory limit. Without a request the
#     single replica is BestEffort, the QoS class the kernel OOM killer takes
#     first, while every operator workload requests 256Mi; CI run 34718789784
#     lost the database five times in 36 minutes that way and failed whichever
#     e2e suite was waiting on DatabaseReady.
#   - the same render carries NO cpu request and NO cpu limit: a 500m request
#     left pods Pending on the 4-vCPU keystone leg (#970), and a cpu limit
#     would throttle the SELECT 1 liveness probe the overlay already relaxed.
#   - kustomize build deploy/flux-system/infrastructure renders no resources
#     on the production MariaDB: the Galera posture is sized by its operators,
#     not by the kind overlay.
#
# Usage: bash tests/unit/deploy/kind_mariadb_resources_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

KIND_INFRA_DIR="$PROJECT_ROOT/deploy/kind/infrastructure"
PROD_INFRA_DIR="$PROJECT_ROOT/deploy/flux-system/infrastructure"
MARIADB_SELECT='select(.kind == "MariaDB" and .metadata.name == "openstack-db")'

# render_field <dir> <yq path>
# Prints the field of the rendered MariaDB/openstack-db, "null" when absent.
render_field() {
  local dir="$1" path="$2"
  kustomize build "$dir" 2>/dev/null \
    | yq -r "$MARIADB_SELECT | $path" 2>/dev/null | head -n1
}

test_kind_overlay_memory_request_equals_limit() {
  echo "Test: the kind overlay gives openstack-db a memory request equal to its limit, and no cpu"
  if ! command -v kustomize >/dev/null 2>&1 || ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: kustomize or yq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi
  if ! kustomize build "$KIND_INFRA_DIR" >/dev/null 2>&1; then
    echo "  FAIL: kustomize build $KIND_INFRA_DIR failed"
    FAIL=$((FAIL + 4))
    return
  fi

  local mem_request mem_limit cpu_request cpu_limit
  mem_request="$(render_field "$KIND_INFRA_DIR" '.spec.resources.requests.memory')"
  mem_limit="$(render_field "$KIND_INFRA_DIR" '.spec.resources.limits.memory')"
  cpu_request="$(render_field "$KIND_INFRA_DIR" '.spec.resources.requests.cpu')"
  cpu_limit="$(render_field "$KIND_INFRA_DIR" '.spec.resources.limits.cpu')"

  if [ -z "$mem_request" ] || [ "$mem_request" = "null" ]; then
    echo "  FAIL: the kind MariaDB renders no memory request (BestEffort, first OOM victim)"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: the kind MariaDB renders a memory request ($mem_request)"
    PASS=$((PASS + 1))
  fi
  assert_eq "memory limit equals the memory request" "$mem_request" "$mem_limit"
  assert_eq "no cpu request (a 500m request starved the 4-vCPU keystone leg, #970)" "null" "$cpu_request"
  assert_eq "no cpu limit (it would throttle the SELECT 1 probe)" "null" "$cpu_limit"
}

test_production_base_untouched() {
  echo "Test: the production MariaDB renders no resources block"
  if ! command -v kustomize >/dev/null 2>&1 || ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: kustomize or yq not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi
  local resources
  resources="$(render_field "$PROD_INFRA_DIR" '.spec.resources')"
  assert_eq "deploy/flux-system/infrastructure renders MariaDB/openstack-db without resources" \
    "null" "$resources"
}

test_kind_overlay_memory_request_equals_limit
test_production_base_untouched

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
