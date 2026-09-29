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
#   - the catch: block of every chainsaw-test.yaml that drives
#     *.openstack.k-orc.cloud kinds reads the log through
#     deploy/orc-controller-manager, the Deployment hack/ci-deploy-korc.sh and
#     the Flux path both create. At least 10 suites are examined, so an empty
#     or moved glob cannot pass the check vacuously.
#   - the catch: block of every suite in LOG_ONLY_SUITES reads the same log.
#     secret-store-scoping creates no K-ORC kind but dumps the log all the
#     same, and the glob cannot select it on orc-system: that string is in
#     the line under test, so deleting the line would drop the suite unseen.
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
# --pod-running-timeout bounds the wait `kubectl logs deploy/<name>` makes for a
# pod when the Deployment has none: 20 s by default, most of the 30 s exec
# budget a catch script inherits from tests/e2e/chainsaw-config.yaml.
LOG_LINE='kubectl logs -n orc-system deploy/orc-controller-manager --pod-running-timeout=5s'
# Suites that dump the K-ORC log without driving a K-ORC kind.
LOG_ONLY_SUITES=(secret-store-scoping)
# The c5c3 suites that drove K-ORC kinds when this test was written. A suite
# may join; the floor only guards against an empty or moved glob.
MIN_SUITES=10

# catch_block <file>
# Prints the catch: block of a chainsaw-test.yaml: the key line and every line
# after it up to the first one indented less than the key or a sibling key at
# its indent (finally:). The block's own list items share the key's indent.
catch_block() {
  awk '
    /^[[:space:]]*catch:/ { match($0, /^ */); ind = RLENGTH; f = 1; print; next }
    f && NF { match($0, /^ */); if (RLENGTH < ind || (RLENGTH == ind && $0 !~ /^ *- /)) f = 0 }
    f
  ' "$1"
}

# --- Test 1: no c5c3 file selects K-ORC by the label no pod carries ---
test_no_c5c3_file_selects_korc_by_the_dead_label() {
  echo "Test: no file under tests/e2e/c5c3/ selects K-ORC by $DEAD_SELECTOR"

  local offenders
  offenders="$(grep -rlF -- "$DEAD_SELECTOR" "$SUITES_DIR" 2>/dev/null || true)"
  assert_eq "no c5c3 file carries the dead K-ORC label selector (offending: ${offenders:-none})" \
    "" "$offenders"
}

# --- Test 2: every c5c3 suite with a catch that drives K-ORC reads the log in it ---
test_every_korc_suite_with_a_catch_reads_the_deployment() {
  echo "Test: every c5c3 suite with a catch that drives K-ORC kinds reads deploy/orc-controller-manager in it"

  local file examined=0
  for file in "$SUITES_DIR"/*/chainsaw-test.yaml; do
    [[ -f "$file" ]] || continue
    grep -qE '^[[:space:]]*catch:' "$file" || continue
    grep -qF 'k-orc.cloud' "$file" || continue
    examined=$((examined + 1))
    assert_contains "${file#"$PROJECT_ROOT"/} reads the K-ORC log through deploy/orc-controller-manager in its catch block" \
      "$(catch_block "$file")" "$LOG_LINE"
  done

  assert_gte "at least $MIN_SUITES c5c3 suites with a catch that drives K-ORC kinds examined (found $examined)" \
    "$examined" "$MIN_SUITES"
}

# --- Test 3: the suites that dump the log without driving K-ORC keep it ---
test_log_only_suites_read_the_deployment() {
  echo "Test: the c5c3 suites in LOG_ONLY_SUITES read deploy/orc-controller-manager in their catch"

  local suite
  for suite in "${LOG_ONLY_SUITES[@]}"; do
    assert_contains "tests/e2e/c5c3/$suite reads the K-ORC log through deploy/orc-controller-manager in its catch block" \
      "$(catch_block "$SUITES_DIR/$suite/chainsaw-test.yaml")" "$LOG_LINE"
  done
}

# --- Run ---
test_no_c5c3_file_selects_korc_by_the_dead_label
test_every_korc_suite_with_a_catch_reads_the_deployment
test_log_only_suites_read_the_deployment

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
