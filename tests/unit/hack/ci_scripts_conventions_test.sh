#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the conventions every hack/ci-*.sh script follows: it fails fast, it
# carries the SPDX header, and a script that requires an env var carries at
# least one GitHub ::error:: annotation.
#
# Each convention guards a failure that is easy to miss in a workflow log. A
# script without set -e carries on past a failed command, and the step it runs
# in turns green on the last command's status. A script without the SPDX
# header goes unnoticed until someone runs a REUSE audit, which no CI job does.
# A required env var reported through a ${VAR:?} expansion prints plain log
# text, where an ::error:: line becomes an annotation on the run summary. The
# check asks for one annotation per such script, not one per variable, so a
# ${VAR:?} expansion beside an ::error:: check elsewhere in the script passes.
#
# One script is exempt from set -e by name: hack/ci-delete-kind-cluster.sh
# exits 0 always, so a teardown problem cannot turn a green test run red. The
# exemption holds only while the script still says so and still runs without
# -e, so the list cannot outlive its reason. Linting is make shellcheck's job
# in CI and is not repeated here.
#
# Usage: bash tests/unit/hack/ci_scripts_conventions_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

shopt -s nullglob
CI_SCRIPTS=("$PROJECT_ROOT"/hack/ci-*.sh)
shopt -u nullglob

# The scripts that run without set -e on purpose; each must say why with the
# phrase "Exits 0 always" in its header.
EXIT_ZERO_BY_DESIGN=(ci-delete-kind-cluster.sh)

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Return 0 when the script name is in EXIT_ZERO_BY_DESIGN.
exits_zero_by_design() {
  local n
  for n in "${EXIT_ZERO_BY_DESIGN[@]}"; do
    [ "$n" = "$1" ] && return 0
  done
  return 1
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_the_script_set_is_not_empty() {
  echo "Test: the hack/ci-*.sh scripts are found"

  # An empty set would let every test below pass on nothing.
  if [ "${#CI_SCRIPTS[@]}" -eq 0 ]; then
    echo "  FAIL: no hack/ci-*.sh found"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: ${#CI_SCRIPTS[@]} hack/ci-*.sh scripts found"
    PASS=$((PASS + 1))
  fi
}

test_every_ci_script_fails_fast() {
  echo "Test: every hack/ci-*.sh script runs set -euo pipefail, or says why not"

  local script name
  for script in ${CI_SCRIPTS[@]+"${CI_SCRIPTS[@]}"}; do
    name=$(basename "$script")
    if ! exits_zero_by_design "$name"; then
      assert_file_contains "$name fails fast" "$script" "^set -euo pipefail"
      continue
    fi

    if grep -qE '^set -[a-z]*e' "$script"; then
      echo "  FAIL: $name is listed in EXIT_ZERO_BY_DESIGN but runs set -e; the exemption no longer applies"
      FAIL=$((FAIL + 1))
    elif grep -q '^set -uo pipefail' "$script" && grep -q 'Exits 0 always' "$script"; then
      echo "  PASS: $name exits 0 by design (set -uo pipefail, header says Exits 0 always)"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $name is exempt from set -e but does not say why (set -uo pipefail and \"Exits 0 always\" expected)"
      FAIL=$((FAIL + 1))
    fi
  done
}

test_every_ci_script_carries_the_spdx_header() {
  echo "Test: every hack/ci-*.sh script carries the SPDX header on lines 2 and 4"

  local script name
  for script in ${CI_SCRIPTS[@]+"${CI_SCRIPTS[@]}"}; do
    name=$(basename "$script")
    assert_contains "$name carries SPDX-FileCopyrightText on line 2" \
      "$(sed -n 2p "$script")" "SPDX-FileCopyrightText"
    assert_contains "$name carries SPDX-License-Identifier on line 4" \
      "$(sed -n 4p "$script")" "SPDX-License-Identifier"
  done
}

test_required_env_is_announced_as_an_error() {
  echo "Test: a script that requires an env var carries an ::error:: annotation"

  local script name
  for script in ${CI_SCRIPTS[@]+"${CI_SCRIPTS[@]}"}; do
    name=$(basename "$script")
    # A script requires an env var when it expands one with ${VAR:?} or checks
    # one with a "must be set" message. Scripts with defaulted vars only are
    # not checked.
    if ! grep -qE '\$\{[A-Za-z_][A-Za-z0-9_]*:\?' "$script" &&
      ! grep -qE '::error::.*must be set' "$script"; then
      echo "  SKIP: $name has no required env vars"
      SKIP=$((SKIP + 1))
      continue
    fi
    if grep -q '::error::' "$script"; then
      echo "  PASS: $name requires an env var and carries an ::error:: annotation"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $name requires env vars but has no ::error:: annotation"
      FAIL=$((FAIL + 1))
    fi
  done
}

test_a_missing_operator_is_an_annotated_exit_one() {
  echo "Test: ci-service-image-releases.sh without OPERATOR exits 1 with an annotation"

  local script="$PROJECT_ROOT/hack/ci-service-image-releases.sh"
  if [ ! -f "$script" ]; then
    echo "  FAIL: hack/ci-service-image-releases.sh not found"
    FAIL=$((FAIL + 1))
    return
  fi

  # The callers always set OPERATOR, so the exit status is all they can see of
  # the guard; the annotation is what a person reading the run sees.
  local err rc
  err=$(OPERATOR='' bash "$script" 2>&1 >/dev/null)
  rc=$?
  assert_eq "a missing OPERATOR exits 1" "1" "$rc"
  assert_eq "and says so as an annotation on stderr" \
    "::error::OPERATOR must be set (e.g. keystone)" "$err"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_the_script_set_is_not_empty
test_every_ci_script_fails_fast
test_every_ci_script_carries_the_spdx_header
test_required_env_is_announced_as_an_error
test_a_missing_operator_is_an_annotated_exit_one

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
