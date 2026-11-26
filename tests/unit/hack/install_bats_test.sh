#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the `make install-bats` recipe: it installs the commit BATS_COMMIT
# names into bin/bats-core, links bin/bats to it and writes the commit to
# the stamp bin/bats-core/.bats-commit; a second run with the stamp in place
# needs no repository; a new commit replaces the install; and a fetch that
# fails or a checkout that reports another version than BATS_VERSION fails
# the target, removes its temporary directory and leaves an older install
# and its stamp as they were. It writes nothing outside LOCALBIN, a BATS
# set elsewhere included, and a LOCALBIN it cannot create fails the target
# with the install message, not the fetch message.
#
# The Makefile is copied into a scratch tree, and BATS_REPO points at a
# local git repository whose commits hold a stub bin/bats that prints
# "Bats 9.9.9", so no network is used and the real bin/ stays untouched.
#
# Usage: bash tests/unit/hack/install_bats_test.sh

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

# commit_upstream <message>
# Commits the work tree of $TREE/upstream and echoes the new commit ID. The
# runner has no git identity, so the commit names its own.
commit_upstream() {
  git -C "$TREE/upstream" add -A &&
    git -C "$TREE/upstream" -c user.name=test -c user.email=test@example.invalid \
      -c commit.gpgsign=false commit -q -m "$1" &&
    git -C "$TREE/upstream" rev-parse HEAD
}

# make_tree
# Creates a scratch tree in $TREE holding the real Makefile and a git
# repository $TREE/upstream whose only commit holds an executable bin/bats
# that prints "Bats 9.9.9". COMMIT is that commit, the tip of its branch,
# which git serves by ID.
make_tree() {
  # The file runs without -e, so an unchecked failure would continue with an
  # empty TREE and write into /.
  TREE="$(mktemp -d)" || {
    echo "mktemp -d could not create a scratch tree" >&2
    exit 1
  }
  cp "$MAKEFILE" "$TREE/Makefile"
  mkdir -p "$TREE/upstream/bin"
  git -C "$TREE/upstream" init -q
  printf '#!/bin/sh\necho "Bats 9.9.9"\n' > "$TREE/upstream/bin/bats"
  chmod +x "$TREE/upstream/bin/bats"
  COMMIT="$(commit_upstream "stub bats 9.9.9")" || {
    echo "could not commit the stub bats in $TREE/upstream" >&2
    exit 1
  }
}

# run_install [VAR=value ...]
# Runs `make install-bats` in the scratch tree with LOCALBIN, BATS_REPO,
# BATS_COMMIT and BATS_VERSION on the command line, where they win over
# values the outer test-shell run passed down, so the real bin/ is never
# written. A later VAR=value argument overrides an earlier one. Writes
# stdout and stderr to $TREE/stdout and $TREE/stderr and echoes make's exit
# code.
run_install() {
  local exit_code=0
  make --no-print-directory -C "$TREE" install-bats \
    LOCALBIN="$TREE/bin" BATS_REPO="$TREE/upstream" \
    BATS_COMMIT="$COMMIT" BATS_VERSION=v9.9.9 "$@" \
    >"$TREE/stdout" 2>"$TREE/stderr" || exit_code=$?
  echo "$exit_code"
}

# leftover_temp_dirs
# Echoes the bin/bats-core.* directories a run left behind, if any.
leftover_temp_dirs() {
  find "$TREE/bin" -maxdepth 1 -name 'bats-core.*' 2>/dev/null
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_installs_the_pinned_commit() {
  echo "Test: install-bats installs the pinned commit, links bin/bats and writes the stamp"
  local code
  make_tree
  code="$(run_install)"

  assert_eq "the target exits 0" "0" "$code"
  assert_eq "bin/bats --version prints the stub's version" \
    "Bats 9.9.9" "$("$TREE/bin/bats" --version 2>&1)"
  assert_eq "the stamp holds the pinned commit" \
    "$COMMIT" "$(cat "$TREE/bin/bats-core/.bats-commit" 2>/dev/null)"
  assert_eq "bin/bats is a relative link into bin/bats-core" \
    "bats-core/bin/bats" "$(readlink "$TREE/bin/bats")"
  assert_contains "stdout says what it installs" \
    "$(cat "$TREE/stdout")" "Installing bats-core v9.9.9 into $TREE/bin..."
  assert_eq "no temporary directory is left" "" "$(leftover_temp_dirs)"
  rm -rf "$TREE"
}

test_a_second_run_needs_no_repository() {
  echo "Test: a second install-bats run with the stamp in place exits 0 without output or repository"
  local code
  make_tree
  run_install >/dev/null
  code="$(run_install BATS_REPO="$TREE/missing")"

  assert_eq "the target exits 0" "0" "$code"
  assert_eq "stdout is empty" "" "$(cat "$TREE/stdout")"
  assert_eq "stderr is empty" "" "$(cat "$TREE/stderr")"
  rm -rf "$TREE"
}

test_a_new_commit_replaces_the_install() {
  echo "Test: a new commit in BATS_COMMIT replaces the install and the stamp"
  local code new_commit
  make_tree
  run_install >/dev/null
  echo marker > "$TREE/upstream/marker"
  new_commit="$(commit_upstream "add a marker")"
  code="$(run_install BATS_COMMIT="$new_commit")"

  assert_eq "the target exits 0" "0" "$code"
  assert_eq "the stamp holds the new commit" \
    "$new_commit" "$(cat "$TREE/bin/bats-core/.bats-commit" 2>/dev/null)"
  if [ -f "$TREE/bin/bats-core/marker" ]; then
    echo "  PASS: bin/bats-core holds the tree of the new commit"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: bin/bats-core lacks the marker of the new commit"
    FAIL=$((FAIL + 1))
  fi
  assert_eq "no temporary directory is left" "" "$(leftover_temp_dirs)"
  rm -rf "$TREE"
}

test_a_missing_repository_fails_without_leftovers() {
  echo "Test: a BATS_REPO that does not exist fails install-bats and leaves no directory"
  local code
  make_tree
  code="$(run_install BATS_REPO="$TREE/missing")"

  assert_nonzero_exit "the target exits non-zero" "$code"
  assert_contains "stderr says the fetch failed" \
    "$(cat "$TREE/stderr")" \
    "install-bats: could not fetch bats-core $COMMIT from $TREE/missing"
  if [ -e "$TREE/bin/bats-core" ]; then
    echo "  FAIL: bin/bats-core exists after a failed fetch"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: bin/bats-core does not exist"
    PASS=$((PASS + 1))
  fi
  assert_eq "no temporary directory is left" "" "$(leftover_temp_dirs)"
  rm -rf "$TREE"
}

test_a_version_mismatch_fails_and_installs_nothing() {
  echo "Test: a checkout that reports another version than BATS_VERSION fails install-bats"
  local code
  make_tree
  code="$(run_install BATS_VERSION=v9.9.8)"

  assert_nonzero_exit "the target exits non-zero" "$code"
  assert_contains "stderr names the reported and the pinned version" \
    "$(cat "$TREE/stderr")" \
    "install-bats: commit $COMMIT reports 'Bats 9.9.9', BATS_VERSION says v9.9.8"
  if [ -e "$TREE/bin/bats-core" ] || [ -L "$TREE/bin/bats" ]; then
    echo "  FAIL: bin/bats-core or bin/bats exists after a failed version check"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: neither bin/bats-core nor bin/bats exists"
    PASS=$((PASS + 1))
  fi
  assert_eq "no temporary directory is left" "" "$(leftover_temp_dirs)"
  rm -rf "$TREE"
}

test_a_failed_update_keeps_the_older_install() {
  echo "Test: a failed update leaves the older install and its stamp as they were"
  local code new_commit
  make_tree
  run_install >/dev/null
  echo marker > "$TREE/upstream/marker"
  new_commit="$(commit_upstream "add a marker")"
  code="$(run_install BATS_COMMIT="$new_commit" BATS_VERSION=v9.9.8)"

  assert_nonzero_exit "the target exits non-zero" "$code"
  assert_eq "the stamp still holds the older commit" \
    "$COMMIT" "$(cat "$TREE/bin/bats-core/.bats-commit" 2>/dev/null)"
  assert_eq "bin/bats still runs the older install" \
    "Bats 9.9.9" "$("$TREE/bin/bats" --version 2>&1)"
  assert_eq "no temporary directory is left" "" "$(leftover_temp_dirs)"
  rm -rf "$TREE"
}

test_a_bats_outside_localbin_stays_untouched() {
  echo "Test: install-bats links bin/bats and leaves a BATS outside LOCALBIN as it was"
  local code
  make_tree
  mkdir -p "$TREE/system"
  printf '#!/bin/sh\necho "Bats 1.0.0"\n' > "$TREE/system/bats"
  chmod +x "$TREE/system/bats"
  code="$(run_install BATS="$TREE/system/bats")"

  assert_eq "the target exits 0" "0" "$code"
  assert_eq "the BATS outside LOCALBIN still runs as before" \
    "Bats 1.0.0" "$("$TREE/system/bats" --version 2>&1)"
  assert_eq "bin/bats runs the install" \
    "Bats 9.9.9" "$("$TREE/bin/bats" --version 2>&1)"
  rm -rf "$TREE"
}

test_a_localbin_that_cannot_be_created_fails_as_an_install_error() {
  echo "Test: a LOCALBIN that cannot be created fails install-bats with the install message, not the fetch message"
  local code
  make_tree
  # A regular file as parent makes mkdir -p fail for any user, root included.
  : > "$TREE/blocker"
  code="$(run_install LOCALBIN="$TREE/blocker/bin")"

  assert_nonzero_exit "the target exits non-zero" "$code"
  assert_contains "stderr names LOCALBIN" \
    "$(cat "$TREE/stderr")" \
    "install-bats: could not install bats-core into $TREE/blocker/bin"
  assert_not_contains "stderr does not blame the fetch" \
    "$(cat "$TREE/stderr")" "could not fetch"
  rm -rf "$TREE"
}

test_installs_the_pinned_commit
test_a_second_run_needs_no_repository
test_a_new_commit_replaces_the_install
test_a_missing_repository_fails_without_leftovers
test_a_version_mismatch_fails_and_installs_nothing
test_a_failed_update_keeps_the_older_install
test_a_bats_outside_localbin_stays_untouched
test_a_localbin_that_cannot_be_created_fails_as_an_install_error

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
