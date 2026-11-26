#!/usr/bin/env bats
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Run every *_test.sh under tests/unit/ as one bats test, in LC_ALL=C sort
# order of the paths. A test is named by the path of its script relative to
# the repository root, the string the script receives as $0, and runs the
# script from the root. Its exit status is the result, and its output shows
# only when it fails. hack/test-shell.sh runs this file; it goes once every
# directory under tests/unit/ is ported to native bats files (#1897).
#
# bats evaluates this file once to count the tests and once per test, so
# the find below runs once per script and once more. bats also evals each
# description, so a script path must hold no $, backtick or double quote.

LEGACY_ROOT="$(cd "$BATS_TEST_DIRNAME/../.." && pwd)"

run_legacy_script() {
  cd "$LEGACY_ROOT" || return 1
  bash "$1"
}

legacy_scripts="$(cd "$LEGACY_ROOT" && find -L tests/unit ! -type d -name '*_test.sh')" || {
  echo 'legacy_scripts.bats: find could not walk tests/unit' >&2
  exit 1
}
legacy_scripts="$(printf '%s\n' "$legacy_scripts" | LC_ALL=C sort)"

# A here-document, not a process substitution, keeps the file within
# Bash 3.2 and the status of find above in hand.
while IFS= read -r legacy_script; do
  bats_test_function --description "$legacy_script" -- run_legacy_script "$legacy_script"
done <<EOF
$legacy_scripts
EOF
