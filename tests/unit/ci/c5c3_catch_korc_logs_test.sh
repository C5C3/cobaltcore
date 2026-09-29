#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify that every c5c3 e2e suite which drives K-ORC kinds captures the
# K-ORC controller log in its catch block, and reads it by a handle that
# matches a pod:
#   - no file under tests/e2e/c5c3/ selects K-ORC by
#     app.kubernetes.io/name=openstack-resource-controller. The upstream
#     manifest labels the controller pod control-plane: controller-manager
#     only, so that selector printed "No resources found in orc-system
#     namespace." where the log should have been (#1107).
#   - every chainsaw-test.yaml with a catch: key that drives
#     *.openstack.k-orc.cloud kinds or reads orc-system reads the log through
#     deploy/orc-controller-manager, the Deployment hack/ci-deploy-korc.sh and
#     the Flux path both create. secret-store-scoping creates no K-ORC kind
#     but dumps the log all the same, so the orc-system arm covers it.
#   - at least 11 suites are examined, so an empty or moved glob cannot pass
#     the second check vacuously.
# Usage: bash tests/unit/ci/c5c3_catch_korc_logs_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

SUITES_DIR="$PROJECT_ROOT/tests/e2e/c5c3"
DEAD_SELECTOR='app.kubernetes.io/name=openstack-resource-controller'
LOG_LINE='kubectl logs -n orc-system deploy/orc-controller-manager'
# The c5c3 suites the second test examined when it was written. A suite may
# join; the floor only keeps the glob honest.
MIN_SUITES=11

# --- Test 1: no c5c3 file selects K-ORC by the label no pod carries ---
test_no_c5c3_file_selects_korc_by_the_dead_label() {
  echo "Test: no file under tests/e2e/c5c3/ selects K-ORC by $DEAD_SELECTOR"

  local offenders
  offenders="$(grep -rlF -- "$DEAD_SELECTOR" "$SUITES_DIR" 2>/dev/null || true)"
  assert_eq "no c5c3 file carries the dead K-ORC label selector (offending: ${offenders:-none})" \
    "" "$offenders"
}

# --- Test 2: every c5c3 suite with a catch that touches K-ORC reads the log ---
test_every_korc_suite_with_a_catch_reads_the_deployment() {
  echo "Test: every c5c3 suite with a catch that touches K-ORC reads deploy/orc-controller-manager"

  local file examined=0
  for file in "$SUITES_DIR"/*/chainsaw-test.yaml; do
    [[ -f "$file" ]] || continue
    grep -qE '^[[:space:]]*catch:' "$file" || continue
    grep -qF -e 'k-orc.cloud' -e 'orc-system' "$file" || continue
    examined=$((examined + 1))
    assert_file_contains_fixed "${file#"$PROJECT_ROOT"/} reads the K-ORC log through deploy/orc-controller-manager" \
      "$file" "$LOG_LINE"
  done

  assert_gte "at least $MIN_SUITES c5c3 suites with a catch that touches K-ORC examined (found $examined)" \
    "$examined" "$MIN_SUITES"
}

# --- Run ---
test_no_c5c3_file_selects_korc_by_the_dead_label
test_every_korc_suite_with_a_catch_reads_the_deployment

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
