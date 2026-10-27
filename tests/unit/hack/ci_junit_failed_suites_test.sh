#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-junit-failed-suites.py on reports shaped like chainsaw's
# JUNIT-TEST report (one <testsuite name="tests/e2e-chaos/<dir>"> per suite):
#   - a <failure message="exit status 1"> prints "<suite>\texit status 1",
#     where <suite> is the last path element of the testsuite name, not the
#     testcase name;
#   - an <error> counts as a failure, an empty or absent message prints "-",
#     and only the first line of a multi-line message is printed;
#   - passed and skipped testcases print nothing, and so does no argument at
#     all; both exit 0;
#   - two reports print their failed suites in argument order;
#   - malformed XML and a missing file exit 2 with
#     "ERROR: <path> is no JUnit report:" and print no suite, even when an
#     earlier report holds a failure.
#
# Usage: bash tests/unit/hack/ci_junit_failed_suites_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
TOOL="$PROJECT_ROOT/hack/ci-junit-failed-suites.py"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

TAB="$(printf '\t')"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# tool <args...> — run the reader; OUT holds stdout, ERR stderr, RC the exit
# code.
tool() {
  RC=0
  OUT="$(python3 -I "$TOOL" "$@" 2>"$TMP_DIR/stderr")" || RC=$?
  ERR="$(cat "$TMP_DIR/stderr")"
}

# suite <dir> <testcase-name> [<inner-xml>] — one chainsaw testsuite element.
suite() {
  printf '  <testsuite name="tests/e2e-chaos/%s" tests="1" failures="0" errors="0" id="0" time="">\n' "$1"
  printf '    <testcase name="%s" classname="" time="1.0">%s</testcase>\n' "$2" "${3:-}"
  printf '  </testsuite>\n'
}

# report <file> <suite-xml...> — wrap testsuite elements in <testsuites>.
report() {
  local file="$1"
  shift
  {
    echo '<testsuites name="chainsaw-report" time="10.0" tests="3" failures="1" skipped="0">'
    printf '%s\n' "$@"
    echo '</testsuites>'
  } >"$file"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_a_failure_prints_its_suite_and_message() {
  echo "Test: a failed testcase prints the suite directory and its message"
  report "$TMP_DIR/one.xml" \
    "$(suite placement-mariadb-outage placement-mariadb-outage)" \
    "$(suite memcached-network-partition keystone-memcached-network-partition \
      '<failure message="exit status 1"></failure>')" \
    "$(suite cinder-nfs-outage cinder-nfs-outage '<skipped message=""></skipped>')"

  tool "$TMP_DIR/one.xml"
  assert_eq "it exits 0" "0" "$RC"
  assert_eq "it prints the directory, a tab and the message" \
    "memcached-network-partition${TAB}exit status 1" "$OUT"
}

test_an_error_and_an_empty_message_count() {
  echo "Test: an <error> counts, an empty or absent message prints -"
  report "$TMP_DIR/error.xml" \
    "$(suite eso-down eso-down '<error message="context deadline exceeded"></error>')" \
    "$(suite eso-pod-kill eso-pod-kill '<failure message=""></failure>')" \
    "$(suite mariadb-pod-kill mariadb-pod-kill '<failure></failure>')" \
    "$(suite openbao-sealed openbao-sealed '<failure message="first line&#10;second line"></failure>')"

  tool "$TMP_DIR/error.xml"
  assert_eq "it exits 0" "0" "$RC"
  assert_eq "an error, two empty messages and the first line of a long one" \
    "eso-down${TAB}context deadline exceeded
eso-pod-kill${TAB}-
mariadb-pod-kill${TAB}-
openbao-sealed${TAB}first line" "$OUT"
}

test_a_green_report_and_no_argument_print_nothing() {
  echo "Test: passed and skipped testcases, and no argument, print nothing"
  report "$TMP_DIR/green.xml" \
    "$(suite placement-mariadb-outage placement-mariadb-outage)" \
    "$(suite cinder-nfs-outage cinder-nfs-outage '<skipped message=""></skipped>')"

  tool "$TMP_DIR/green.xml"
  assert_eq "a green report exits 0" "0" "$RC"
  assert_eq "a green report prints nothing" "" "$OUT"

  tool
  assert_eq "no argument exits 0" "0" "$RC"
  assert_eq "no argument prints nothing" "" "$OUT"
}

test_two_reports_print_in_argument_order() {
  echo "Test: two reports print their failed suites in argument order"
  report "$TMP_DIR/a.xml" "$(suite nova-broker-outage nova-broker-outage '<failure message="a"></failure>')"
  report "$TMP_DIR/b.xml" "$(suite cinder-broker-outage cinder-broker-outage '<failure message="b"></failure>')"

  tool "$TMP_DIR/b.xml" "$TMP_DIR/a.xml"
  assert_eq "b before a" "cinder-broker-outage${TAB}b
nova-broker-outage${TAB}a" "$OUT"
}

test_malformed_xml_exits_two() {
  echo "Test: malformed XML and a missing file exit 2 and print no suite"
  report "$TMP_DIR/red.xml" "$(suite nova-broker-outage nova-broker-outage '<failure message="a"></failure>')"
  printf '<testsuites><testsuite name="tests/e2e-chaos/x">' >"$TMP_DIR/cut.xml"

  tool "$TMP_DIR/red.xml" "$TMP_DIR/cut.xml"
  assert_eq "a cut report exits 2" "2" "$RC"
  assert_starts_with "it names the report" "$ERR" \
    "ERROR: $TMP_DIR/cut.xml is no JUnit report:"
  assert_eq "it prints no suite, not even the earlier report's" "" "$OUT"

  tool "$TMP_DIR/absent.xml"
  assert_eq "a missing report exits 2" "2" "$RC"
  assert_starts_with "it names the missing report" "$ERR" \
    "ERROR: $TMP_DIR/absent.xml is no JUnit report:"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_a_failure_prints_its_suite_and_message
test_an_error_and_an_empty_message_count
test_a_green_report_and_no_argument_print_nothing
test_two_reports_print_in_argument_order
test_malformed_xml_exits_two

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
