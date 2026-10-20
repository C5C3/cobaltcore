#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the env-key-material suites, one per operator, that run
# tests/e2e/lib/assert-operator-env-clean.sh against the operator the
# e2e-operator leg deployed (#1503):
#   1. Every operator with a chart under operators/<op>/helm/<op>-operator has
#      exactly one suite: tests/e2e/c5c3/env-key-material for c5c3, whose leg
#      runs tests/e2e/c5c3/, and tests/e2e/<op>-operator/env-key-material for
#      the other nine, whose legs run that directory. No other directory of
#      that name exists.
#   2. Each suite is named <op>-env-key-material, runs in <op>-system, gives
#      its scan step a timeout and a catch, and calls the scan for the
#      <op>-operator Deployment in <op>-system with its debug image: the
#      service image at 2026.1, the OVN image at the pin
#      hack/ci-resolve-ovn-version.sh prints for ovn, and the keystone image
#      for c5c3. The catch prints the ephemeral containers' states and never
#      reads their logs, which hold the values the scan judged.
#   3. The debug image is on the node of the leg: 2026.1 is a release the
#      service operator ships an image for, which the leg loads; the ovn leg
#      loads the OVN image at the pin; the c5c3 leg resolves and loads the
#      keystone image.
#   4. An edit to the scan runs every leg: the seven filters that list
#      tests/e2e/lib/** cover it, and the c5c3, ovn and neutron filters name
#      it, the ovn and neutron ones beside their -operator directory.
#   5. The chart the scan reads still names the container manager and sets
#      runAsUser 65532 on the pod, which the ephemeral container's --target
#      and its read of the manager's environ depend on, and make shellcheck
#      lints tests/e2e/lib/.
#
# The checks of 2 read the YAML structure with mikefarah yq; without it they
# are skipped and counted.
#
# Usage: bash tests/unit/ci/operator_env_suites_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC2034 # read by tests/lib/ci_yaml.sh
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

SCAN="$PROJECT_ROOT/tests/e2e/lib/assert-operator-env-clean.sh"
SERVICE_OPS="keystone horizon glance placement barbican neutron cinder nova"
FILTER_LIB_OPS="keystone horizon glance placement barbican cinder nova"

HAVE_YQ=0
if command -v yq >/dev/null 2>&1 && yq --version 2>&1 | grep -q mikefarah; then
  HAVE_YQ=1
fi

# suite_of <op>: the suite file of the operator.
suite_of() {
  if [ "$1" = "c5c3" ]; then
    echo "$PROJECT_ROOT/tests/e2e/c5c3/env-key-material/chainsaw-test.yaml"
  else
    echo "$PROJECT_ROOT/tests/e2e/$1-operator/env-key-material/chainsaw-test.yaml"
  fi
}

# chart_ops: the operators with a chart, sorted, space-separated.
chart_ops() {
  local dir
  for dir in "$PROJECT_ROOT"/operators/*/helm/*-operator; do
    dir="${dir%/helm/*}"
    echo "${dir##*/}"
  done | LC_ALL=C sort | paste -s -d ' ' -
}

# if_block <text> <op>: the lines between `if [ "${OPERATOR}" = "<op>" ]; then`
# and the first `fi` after it, from a run: script.
if_block() {
  awk -v open="if [ \"\${OPERATOR}\" = \"$2\" ]; then" '
    { line = $0; sub(/^[ \t]+/, "", line) }
    line == open { in_block = 1; next }
    in_block && line == "fi" { in_block = 0 }
    in_block { print line }
  ' <<<"$1"
}

# --- Test 1: one suite per operator ---
test_every_operator_has_one_suite() {
  echo "Test: every operator with a chart has exactly one env-key-material suite"

  local ops op found expected
  ops="$(chart_ops)"
  assert_eq "the operators with a chart" \
    "barbican c5c3 cinder glance horizon keystone neutron nova ovn placement" "$ops"
  for op in $ops; do
    if [ -f "$(suite_of "$op")" ]; then
      echo "  PASS: the $op suite exists"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $(suite_of "$op") is missing"
      FAIL=$((FAIL + 1))
    fi
  done

  found="$(cd "$PROJECT_ROOT" && find tests -type d -name env-key-material | LC_ALL=C sort | paste -s -d ' ' -)"
  expected="$(for op in $ops; do
    f="$(suite_of "$op")"
    f="${f%/chainsaw-test.yaml}"
    echo "${f#"$PROJECT_ROOT"/}"
  done | LC_ALL=C sort | paste -s -d ' ' -)"
  assert_eq "no other env-key-material directory exists" "$expected" "$found"
}

# --- Test 2: each suite ---
test_each_suite_scans_its_operator() {
  echo "Test: each suite scans its own operator with its debug image"

  local op f call
  for op in $(chart_ops); do
    f="$(suite_of "$op")"
    [ -f "$f" ] || continue
    case "$op" in
      c5c3) call="../../lib/assert-operator-env-clean.sh c5c3-system c5c3-operator ghcr.io/c5c3/keystone:2026.1" ;;
      ovn) call='../../lib/assert-operator-env-clean.sh ovn-system ovn-operator "ghcr.io/c5c3/ovn:$tag"' ;;
      *) call="../../lib/assert-operator-env-clean.sh $op-system $op-operator ghcr.io/c5c3/$op:2026.1" ;;
    esac
    assert_file_contains_fixed "the $op suite calls the scan for its operator and image" "$f" "$call"
    assert_eq "the $op suite calls the scan once" "1" \
      "$(grep -v '^[[:space:]]*#' "$f" | grep -c 'assert-operator-env-clean.sh ')"
    assert_file_contains_fixed "the $op suite carries the license header" "$f" \
      "SPDX-License-Identifier: Apache-2.0"
    assert_file_contains_fixed "the $op catch prints the ephemeral containers' states" "$f" \
      "-o jsonpath='{range .status.ephemeralContainerStatuses[*]}{.name}{\" \"}{.state}{\"\\n\"}{end}'"
    assert_file_not_contains "the $op suite never reads a container log" "$f" "kubectl logs"

    if [ "$HAVE_YQ" -eq 0 ]; then
      echo "  SKIP: the structure of the $op suite (mikefarah yq not found)"
      SKIP=$((SKIP + 1))
      continue
    fi
    assert_eq "the $op suite is named $op-env-key-material" "$op-env-key-material" \
      "$(yq '.metadata.name' "$f")"
    assert_eq "the $op suite runs in $op-system" "$op-system" "$(yq '.spec.namespace' "$f")"
    assert_eq "the $op suite has one step" "1" "$(yq '.spec.steps | length' "$f")"
    assert_eq "the $op scan step has a timeout" "180s" "$(yq '.spec.steps[0].try[0].script.timeout' "$f")"
    assert_eq "the $op scan step has a catch" "1" "$(yq '.spec.steps[0].catch | length' "$f")"
    assert_contains "the $op catch reads its operator's pods" \
      "$(yq '.spec.steps[0].catch[0].script.content' "$f")" \
      "-n $op-system -l app.kubernetes.io/instance=$op-operator"
  done

  # The ovn suite resolves the pin with the script the leg resolves it with.
  local ovn
  ovn="$(suite_of ovn)"
  assert_file_contains_fixed "the ovn suite resolves the OVN pin" "$ovn" \
    'tag="$(../../../../hack/ci-resolve-ovn-version.sh)" || exit 1'
  if [ -x "$(cd "$(dirname "$ovn")" && cd ../../../.. && pwd)/hack/ci-resolve-ovn-version.sh" ]; then
    echo "  PASS: ../../../../hack/ci-resolve-ovn-version.sh resolves from the suite directory"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: ../../../../hack/ci-resolve-ovn-version.sh is not executable from the suite directory"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 3: the debug image is on the node ---
test_the_debug_image_is_on_the_node() {
  echo "Test: the leg of each suite loads the suite's debug image"

  local resolve load op releases
  resolve="$(job_step e2e-operator "Resolve E2E images")"
  load="$(job_step e2e-operator "Load images into kind")"
  assert_not_empty "the e2e-operator job resolves its images" "$resolve"
  assert_not_empty "the e2e-operator job loads its images" "$load"

  # A service operator's leg loads its image for every release it ships.
  assert_contains "the leg loads the service image of every release" "$load" \
    'kind load docker-image "${IMAGE_PREFIX}/${OPERATOR}:${release}" --name "${KIND_CLUSTER}"'
  for op in $SERVICE_OPS; do
    releases="$(OPERATOR="$op" "$PROJECT_ROOT/hack/ci-service-image-releases.sh")"
    assert_contains "$op ships a 2026.1 service image" " $(paste -s -d ' ' - <<<"$releases") " " 2026.1 "
  done

  assert_contains "the ovn leg resolves the OVN image" "$(if_block "$resolve" ovn)" \
    'refs="${refs}"$'"'"'\n'"'"'"${IMAGE_PREFIX}/ovn:${OVN_VERSION}"'
  assert_contains "the ovn leg loads the OVN image" "$(if_block "$load" ovn)" \
    'kind load docker-image "${IMAGE_PREFIX}/ovn:${OVN_VERSION}" --name "${KIND_CLUSTER}"'
  assert_contains "the ovn leg resolves the pin with the suite's script" "$resolve" \
    'OVN_VERSION="$(hack/ci-resolve-ovn-version.sh)"'

  assert_contains "the c5c3 leg resolves the keystone image" "$(if_block "$resolve" c5c3)" \
    'refs="${refs}"$'"'"'\n'"'"'"${IMAGE_PREFIX}/keystone:2026.1"'
  assert_contains "the c5c3 leg loads the keystone image" "$(if_block "$load" c5c3)" \
    'kind load docker-image "${IMAGE_PREFIX}/keystone:2026.1" --name "${KIND_CLUSTER}"'

  # The run step reaches the suites: tests/e2e/c5c3/ for c5c3, the
  # -operator directory for the other nine.
  assert_contains "the leg runs the -operator directory" \
    "$(job_step e2e-operator "Run E2E tests")" 'test_dirs+=("tests/e2e/${OPERATOR}-operator/")'
}

# --- Test 4: the change filters ---
test_an_edit_to_the_scan_runs_every_leg() {
  echo "Test: an edit to the scan runs the leg of every operator"

  local op block
  for op in $FILTER_LIB_OPS; do
    block="$(filter_block "tests_e2e_$op")"
    assert_contains "tests_e2e_$op lists the shared directory" "$block" "'tests/e2e/lib/**'"
  done
  for op in c5c3 ovn neutron; do
    block="$(filter_block "tests_e2e_$op")"
    assert_contains "tests_e2e_$op names the scan" "$block" \
      "'tests/e2e/lib/assert-operator-env-clean.sh'"
  done
  for op in ovn neutron; do
    assert_contains "tests_e2e_$op lists its -operator directory" \
      "$(filter_block "tests_e2e_$op")" "'tests/e2e/$op-operator/**'"
  done
  assert_contains "tests_e2e_c5c3 lists the directory of its suite" \
    "$(filter_block tests_e2e_c5c3)" "'tests/e2e/c5c3/**'"
}

# --- Test 5: what the scan depends on ---
test_the_chart_matches_the_scan() {
  echo "Test: the chart names the container the scan targets and runs the pod as 65532"

  local tpl
  tpl="$PROJECT_ROOT/operators/shared/helm/operator-library/templates/_deployment.tpl"
  assert_file_contains_fixed "the scan targets the container manager" "$SCAN" "target=manager"
  assert_file_contains_fixed "the chart names its container manager" "$tpl" "        - name: manager"
  # The pod-level securityContext sits at 6 spaces, its fields at 8; the
  # container-level one is deeper. The ephemeral container takes the pod's.
  assert_eq "the pod securityContext sets runAsUser 65532" "1" \
    "$(awk '/^      securityContext:$/ { pod = 1; next } pod && /^        runAsUser: 65532$/ { n++ } /^      [a-z]/ { pod = 0 } END { print n + 0 }' "$tpl")"

  assert_file_contains_fixed "make shellcheck lints tests/e2e/lib/" "$PROJECT_ROOT/Makefile" \
    "@shellcheck --severity=warning tests/e2e/lib/*.sh"
}

# --- Run ---
test_every_operator_has_one_suite
test_each_suite_scans_its_operator
test_the_debug_image_is_on_the_node
test_an_edit_to_the_scan_runs_every_leg
test_the_chart_matches_the_scan

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
