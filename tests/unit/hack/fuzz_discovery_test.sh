#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the `make fuzz` recipe: it finds every func Fuzz* in a *_fuzz_test.go
# file under internal/ and operators/ by discovery and runs one go test -fuzz
# invocation per target for FUZZ_TIME, a failing target stops the loop, and an
# empty discovery, a directory grep cannot search, or a target go test does not
# list fails the target instead of passing with nothing fuzzed. A Fuzz name
# followed by a lower-case letter is a helper to go test and is not fuzzed.
#
# The Makefile is copied into a scratch tree beside stub fuzz files, and a stub
# go first on PATH appends its arguments to $GO_LOG, so no Go toolchain runs.
#
# Usage: bash tests/unit/hack/fuzz_discovery_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
MAKEFILE="$PROJECT_ROOT/Makefile"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_tree
# Creates a scratch tree in $TREE holding the real Makefile, empty internal/
# and operators/ directories, and a stub go in $TREE/bin that appends its
# arguments to $GO_LOG. Asked to -list a target, the stub prints its name unless
# it is $UNLISTED_FUZZ, or prints a panic and exits 1 the way a failing test
# binary does when it is $BROKEN_FUZZ; asked to -fuzz a target, it exits 1 when
# that is $FAIL_FUZZ.
make_tree() {
  # The file runs without -e, so an unchecked failure would continue with an
  # empty TREE and copy the Makefile to /Makefile.
  TREE="$(mktemp -d)" || {
    echo "mktemp -d could not create a scratch tree" >&2
    exit 1
  }
  mkdir -p "$TREE/internal" "$TREE/operators" "$TREE/bin"
  cp "$MAKEFILE" "$TREE/Makefile"
  cat > "$TREE/bin/go" <<'STUB'
#!/bin/bash
echo "$*" >> "$GO_LOG"
case " $* " in
  *" -list=^${UNLISTED_FUZZ:-}\$ "*) exit 0 ;;
  *" -list=^${BROKEN_FUZZ:-}\$ "*) printf 'panic: stub TestMain setup failed\nFAIL\tstub\n'; exit 1 ;;
  *" -list=^"*) name="${2#-list=^}"; echo "${name%\$}" ;;
  *" -fuzz=^${FAIL_FUZZ:-}\$ "*) exit 1 ;;
esac
exit 0
STUB
  chmod +x "$TREE/bin/go"
}

# add_go_file <path under the tree> <func name>...
# Writes a Go test file declaring each named function with a fuzz target's
# signature, followed by a helper whose name does not start with Fuzz.
add_go_file() {
  local path="$1" fn
  shift
  mkdir -p "$TREE/$(dirname "$path")"
  {
    printf 'package stub\n\nimport "testing"\n'
    for fn in "$@"; do
      printf '\nfunc %s(f *testing.F) {}\n' "$fn"
    done
    printf '\nfunc helper() {}\n'
  } > "$TREE/$path"
}

# run_target
# Runs `make fuzz FUZZ_TIME=5s` in the scratch tree with the stub go first on
# PATH, writes its stdout and stderr to $TREE/stdout and $TREE/stderr, and
# echoes make's exit code.
run_target() {
  local exit_code=0
  : > "$TREE/go.log"
  GO_LOG="$TREE/go.log" PATH="$TREE/bin:$PATH" make -C "$TREE" fuzz FUZZ_TIME=5s \
    >"$TREE/stdout" 2>"$TREE/stderr" || exit_code=$?
  echo "$exit_code"
}

# list_call <fuzz target> <package dir>
# Echoes the go invocation the recipe makes to check go test lists a target.
list_call() {
  printf 'test -list=^%s$ ./%s' "$1" "$2"
}

# go_call <fuzz target> <package dir>
# Echoes the go invocations the recipe makes for one fuzz target: the list
# check, then the fuzz run.
go_call() {
  list_call "$1" "$2"
  printf '\ntest -run=^$ -fuzz=^%s$ -fuzztime=5s ./%s' "$1" "$2"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_runs_every_fuzz_target_once() {
  echo "Test: fuzz runs every func Fuzz* in a *_fuzz_test.go file once, with the budget"
  local code
  make_tree
  add_go_file internal/a/x_fuzz_test.go FuzzAlpha FuzzBeta
  add_go_file operators/b/y_fuzz_test.go FuzzGamma
  # A func Fuzz* outside a *_fuzz_test.go file is not looked for.
  add_go_file internal/a/x_test.go FuzzIgnored
  code="$(run_target)"

  assert_eq "the target exits 0" "0" "$code"
  assert_eq "go test listed and fuzzed each target once, in file path order" \
    "$(go_call FuzzAlpha internal/a; echo; go_call FuzzBeta internal/a; echo; go_call FuzzGamma operators/b)" \
    "$(cat "$TREE/go.log")"
  assert_contains "stdout names the target before it runs" \
    "$(cat "$TREE/stdout")" "Fuzzing FuzzGamma in operators/b for 5s..."
  rm -rf "$TREE"
}

test_a_failing_target_stops_the_loop() {
  echo "Test: a failing fuzz target stops fuzz before the next file"
  local code
  make_tree
  add_go_file internal/a/x_fuzz_test.go FuzzAlpha FuzzBeta
  add_go_file operators/b/y_fuzz_test.go FuzzGamma
  # FuzzBeta is the last target of its file, so only an exit inside the inner
  # loop keeps the outer loop from reaching FuzzGamma.
  code="$(FAIL_FUZZ=FuzzBeta run_target)"

  assert_nonzero_exit "the target exits non-zero" "$code"
  assert_eq "no target after the failing one ran" \
    "$(go_call FuzzAlpha internal/a; echo; go_call FuzzBeta internal/a)" \
    "$(cat "$TREE/go.log")"
  rm -rf "$TREE"
}

test_an_unlisted_target_fails_the_target() {
  echo "Test: a target go test does not list fails fuzz instead of passing with it unfuzzed"
  local code
  make_tree
  add_go_file internal/a/x_fuzz_test.go FuzzAlpha FuzzBeta
  add_go_file operators/b/y_fuzz_test.go FuzzGamma
  # go test -fuzz exits 0 without fuzzing when a build constraint excludes the
  # file, so the stub leaves FuzzBeta out of the list the way go test would.
  code="$(UNLISTED_FUZZ=FuzzBeta run_target)"

  assert_nonzero_exit "the target exits non-zero" "$code"
  assert_contains "stderr names the unlisted target" \
    "$(cat "$TREE/stderr")" "fuzz: go test does not list FuzzBeta in internal/a"
  assert_eq "the unlisted target and every target after it were not fuzzed" \
    "$(go_call FuzzAlpha internal/a; echo; list_call FuzzBeta internal/a)" \
    "$(cat "$TREE/go.log")"
  rm -rf "$TREE"
}

test_a_failing_list_prints_the_go_test_output() {
  echo "Test: a test binary that fails while listing fails fuzz and shows its output"
  local code stderr
  make_tree
  add_go_file internal/a/x_fuzz_test.go FuzzAlpha FuzzBeta
  add_go_file operators/b/y_fuzz_test.go FuzzGamma
  # A panic in TestMain reaches go test's stdout, not its stderr.
  code="$(BROKEN_FUZZ=FuzzBeta run_target)"
  stderr="$(cat "$TREE/stderr")"

  assert_nonzero_exit "the target exits non-zero" "$code"
  assert_contains "stderr carries the panic go test printed" \
    "$stderr" "panic: stub TestMain setup failed"
  assert_contains "stderr names the target the test binary failed on" \
    "$stderr" "fuzz: go test does not list FuzzBeta in internal/a"
  assert_eq "the target and every target after it were not fuzzed" \
    "$(go_call FuzzAlpha internal/a; echo; list_call FuzzBeta internal/a)" \
    "$(cat "$TREE/go.log")"
  rm -rf "$TREE"
}

test_a_lower_case_suffix_is_not_a_target() {
  echo "Test: a Fuzz name followed by a lower-case letter is a helper and is not fuzzed"
  local code
  make_tree
  # go test treats Fuzzy and FuzzerSeeds as helpers and Fuzz_seed as a target.
  add_go_file internal/a/x_fuzz_test.go FuzzAlpha Fuzzy FuzzerSeeds Fuzz_seed
  code="$(run_target)"

  assert_eq "the target exits 0" "0" "$code"
  assert_eq "go test listed and fuzzed only the targets" \
    "$(go_call FuzzAlpha internal/a; echo; go_call Fuzz_seed internal/a)" \
    "$(cat "$TREE/go.log")"
  rm -rf "$TREE"
}

test_an_empty_discovery_fails_the_target() {
  echo "Test: finding no fuzz target fails fuzz instead of passing with nothing fuzzed"
  local code
  make_tree
  add_go_file internal/a/x_fuzz_test.go
  add_go_file internal/a/x_test.go FuzzIgnored
  # A *_fuzz_test.go file holding only a helper has no target either.
  add_go_file internal/b/z_fuzz_test.go Fuzzy
  code="$(run_target)"

  assert_nonzero_exit "the target exits non-zero" "$code"
  assert_contains "stderr says no fuzz target was found" \
    "$(cat "$TREE/stderr")" \
    "fuzz: no func Fuzz* found in a *_fuzz_test.go file under internal/ or operators/"
  assert_eq "go never ran" "" "$(cat "$TREE/go.log")"
  rm -rf "$TREE"
}

test_an_unreadable_directory_fails_the_target() {
  echo "Test: a directory grep cannot search fails fuzz instead of skipping its targets"
  local code
  make_tree
  add_go_file internal/a/x_fuzz_test.go FuzzAlpha
  add_go_file operators/locked/y_fuzz_test.go FuzzGamma
  chmod 000 "$TREE/operators/locked"
  if [ -r "$TREE/operators/locked" ]; then
    echo "  SKIP: a mode-000 directory stays readable to this user (3 checks skipped)"
    SKIP=$((SKIP + 3))
    chmod 755 "$TREE/operators/locked"
    rm -rf "$TREE"
    return
  fi
  code="$(run_target)"
  chmod 755 "$TREE/operators/locked"

  assert_nonzero_exit "the target exits non-zero" "$code"
  assert_contains "stderr says grep could not search the tree" \
    "$(cat "$TREE/stderr")" "fuzz: grep could not search internal/ and operators/"
  assert_eq "go never ran" "" "$(cat "$TREE/go.log")"
  rm -rf "$TREE"
}

test_runs_every_fuzz_target_once
test_a_failing_target_stops_the_loop
test_an_unlisted_target_fails_the_target
test_a_failing_list_prints_the_go_test_output
test_a_lower_case_suffix_is_not_a_target
test_an_empty_discovery_fails_the_target
test_an_unreadable_directory_fails_the_target

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
