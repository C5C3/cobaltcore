#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the preBuild hook of nix/lib/python-env.nix, which commits a fetched
# OpenStack tree to a throwaway repository for pbr, starts no background git
# maintenance.
#
# git commit runs `git maintenance run --auto` after the commit (detached in
# git 2.54, the git of nixpkgs-images). Its `git repack -d` deletes the loose
# object directories under .git/objects/ while setuptools already walks the
# tree, and the project build then fails at random with "[Errno 2] No such
# file or directory: './.git/objects/60'" (glance 33.0.0 on PR #1425). The
# hook passes -c maintenance.auto=false, so the commit starts no child.
#
# The hook is read from the file itself, so the test runs the commands the
# build runs. A control commit without the flag first shows that this host's
# git starts maintenance at all; where it does not, the check would pass
# vacuously and is skipped.
#
# Usage: bash tests/unit/images/python_env_pbr_commit_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

PYTHON_ENV="$PROJECT_ROOT/nix/lib/python-env.nix"

# extract_pre_build
# Print the body of the preBuild string of python-env.nix with the two Nix
# interpolations it carries replaced by fixture values.
extract_pre_build() {
  awk '
    /preBuild = '"''"'$/ { inside = 1; next }
    inside && /^[[:space:]]*'"''"';$/ { exit }
    inside { print }
  ' "$PYTHON_ENV" |
    sed -e 's/[$]{name}/fixture/g' -e 's/[$]{sources[.]ref}/1.0.0/g'
}

# make_tree <dir>
# Write a small source tree, with one file its .gitignore matches.
make_tree() {
  local dir="$1" i
  mkdir -p "$dir/fixture/sub"
  for i in 1 2 3 4 5 6 7 8; do
    echo "value = $i" > "$dir/fixture/mod$i.py"
    echo "value = $i" > "$dir/fixture/sub/mod$i.py"
  done
  echo "fixture/local.py" > "$dir/.gitignore"
  echo "local = True" > "$dir/fixture/local.py"
}

# children <trace>
# Print the git subcommands the traced run started that touch the object
# store in the background: maintenance, gc and repack.
children() {
  grep -oE '"argv":\[[^]]*"(maintenance|gc|repack)"[^]]*\]' "$1" | sort -u || true
}

# isolated_git_env
# Keep the user's and the system's git configuration out of the runs.
isolated_git_env() {
  export HOME="$1" GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
}

test_hook_is_extractable() {
  echo "Test: the preBuild hook of python-env.nix is extractable"
  local hook
  hook=$(extract_pre_build)
  assert_contains "the hook runs git init" "$hook" "git init"
  assert_contains "the hook commits" "$hook" "commit --quiet"
  assert_not_contains "the hook carries no other Nix interpolation" "$hook" "\${"
}

test_hook_starts_no_maintenance() {
  echo "Test: the hook commits the tree and starts no background maintenance"
  local tmp hook exit_code=0 control spawned subject
  tmp=$(mktemp -d)
  hook=$(extract_pre_build)

  (
    isolated_git_env "$tmp"
    make_tree "$tmp/control"
    cd "$tmp/control" &&
      git init --quiet &&
      git add --all --force &&
      GIT_TRACE2_EVENT="$tmp/control.json" git -c user.name=t -c user.email=t@localhost \
        commit --quiet --message control
  ) > /dev/null 2>&1
  control=$(children "$tmp/control.json")

  if [ -z "$control" ]; then
    echo "  SKIP: this git ($(git --version)) starts no maintenance on commit, so the check would pass vacuously"
    SKIP=$((SKIP + 3))
    rm -rf "$tmp"
    return
  fi

  (
    isolated_git_env "$tmp"
    make_tree "$tmp/tree"
    cd "$tmp/tree" && GIT_TRACE2_EVENT="$tmp/hook.json" bash -euo pipefail -c "$hook"
  ) || exit_code=$?
  assert_eq "the hook exits 0" "0" "$exit_code"

  spawned=$(children "$tmp/hook.json")
  [ -z "$spawned" ] || echo "    started: $spawned"
  assert_eq "the hook's commit starts no maintenance, gc or repack" "" "$spawned"

  subject=$(cd "$tmp/tree" && HOME="$tmp" GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
    git log -1 --format='%s|%an|%ad|%(trailers)' --date=raw 2>&1)
  assert_eq "the commit keeps its fixed subject, author and date" "fixture 1.0.0|nix|1 +0000|" "$subject"

  rm -rf "$tmp"
}

if ! command -v git >/dev/null 2>&1; then
  echo "SKIP: git not installed (every case needs it)"
  SKIP=$((SKIP + 1))
else
  test_hook_is_extractable
  echo ""
  test_hook_starts_no_maintenance
fi

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
