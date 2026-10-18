#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the check-nix-lock-drift audit on fixtures and on the repository.
#
# The audit cds to the directory four levels above itself, so each case copies
# it into a temporary tree that carries only the files the audit reads: one
# release with a keystone and a tempest lock that match their pins,
# scripts/apply-constraint-overrides.sh, an image pin, and a stub
# hack/nix-lock.sh that answers --check-operators. A case changes one pin or
# lock and asserts on the line it targets. The tree is no git checkout, so L7
# reports itself skipped, except in the L7 case, which commits the tree.
#
# The audit needs yq (mikefarah) v4.49.1 or newer and jq; without them every
# case is skipped. The cases that merge the constraint overrides need GNU
# sed, which scripts/apply-constraint-overrides.sh edits with, and the L7
# case needs git.
#
# Usage: bash tests/unit/ci/check_nix_lock_drift_audit_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SKILL_DIR=".claude/skills/check-nix-lock-drift/scripts"
AUDIT="$SKILL_DIR/audit-nix-lock-drift.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

# A git hook exports these (in a linked worktree with an absolute GIT_DIR),
# and they would point the git calls of the L7 case and of the audit at the
# outer repository.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_COMMON_DIR GIT_OBJECT_DIRECTORY

REV="3526925ae34a66e626bec985628a421a7694168e"

# ---------------------------------------------------------------------------
# Tool checks
# ---------------------------------------------------------------------------

have_tools() {
  command -v jq >/dev/null 2>&1 && command -v yq >/dev/null 2>&1 &&
    grep -q mikefarah <<<"$(yq --version 2>&1)" &&
    [[ "$(yq -n -r '"a\tb"' 2>/dev/null)" == $'a\tb' ]]
}

# have_gnu_sed: the in-place edit with the I flag that
# scripts/apply-constraint-overrides.sh runs.
have_gnu_sed() {
  printf 'A===1\n' >"$TMP_ROOT/probe"
  sed -i '/^a===/Id' "$TMP_ROOT/probe" 2>/dev/null && [[ ! -s "$TMP_ROOT/probe" ]]
}

have_git() {
  command -v git >/dev/null 2>&1
}

# skip_unless <what> <check>: print a SKIP line and return 1 unless <check>
# succeeds.
skip_unless() {
  if ! "$2"; then
    echo "  SKIP: $1"
    SKIP=$((SKIP + 1))
    return 1
  fi
}

# ---------------------------------------------------------------------------
# Fixture
# ---------------------------------------------------------------------------

# stage_tree: write a consistent tree and print its root. Release 2026.1 pins
# keystone with the ldap extra, a pip package and one patch, a derived block
# demo-compute that the keystone lock carries as a dependency group, and the
# tempest version and its four plugins. The keystone lock fetches its project
# at the rev of its sources.json. The overrides strip horizon and repin
# lhafile, and the constraints pin cryptography, which the locks then list
# by name alone. images/demo/source.json is a complete pin.
stage_tree() {
  local t
  t="$(mktemp -d "$TMP_ROOT/tree.XXXXXX")"
  mkdir -p "$t/$SKILL_DIR" "$t/scripts" "$t/hack" "$t/releases/2026.1" \
    "$t/overrides/2026.1" "$t/patches/keystone/2026.1" "$t/nix/locks/2026.1/keystone" \
    "$t/nix/locks/2026.1/tempest" "$t/images/demo"
  cp "$PROJECT_ROOT/$AUDIT" "$t/$AUDIT"
  cp "$PROJECT_ROOT/scripts/apply-constraint-overrides.sh" "$t/scripts/"
  cat >"$t/hack/nix-lock.sh" <<'EOF'
#!/bin/bash
# Stub of --check-operators: NIX_LOCK_STUB_FAIL=1 reports a stale pin.
if [ -n "${NIX_LOCK_STUB_FAIL:-}" ]; then
  echo operators/demo/vendor.json
  echo "nix-lock.sh: the operator vendor hashes are stale; run make nix-lock and commit the result" >&2
  exit 1
fi
echo "nix-lock.sh: the operator vendor hashes are current"
EOF
  printf '# Git refs\nkeystone: "29.0.0"\n' >"$t/releases/2026.1/source-refs.yaml"
  cat >"$t/releases/2026.1/test-refs.yaml" <<'EOF'
tempest: "46.3.0"
barbican-tempest-plugin: "4.6.0"
keystone-tempest-plugin: "0.22.0"
neutron-tempest-plugin: "3.2.0"
cinder-tempest-plugin: "1.23.0"
EOF
  printf 'cryptography===48.0.1\nhorizon===25.7.0\nlhafile===0.2.0\npython_ldap===3.4.5\n' \
    >"$t/releases/2026.1/upper-constraints.txt"
  printf '# overrides\n-horizon\nlhafile===0.3.1\n' >"$t/overrides/2026.1/constraints.txt"
  cat >"$t/releases/2026.1/extra-packages.yaml" <<'EOF'
keystone:
  pip_extras:
    - ldap
  pip_packages:
    - python-memcached
demo-compute:
  pip_extras: []
  pip_packages:
    - libvirt-python
EOF
  printf '# base pins\ncryptography==48.0.1\nuwsgi==2.0.31\n' >"$t/nix/base-requirements.txt"
  printf 'diff --git a/x b/x\n' >"$t/patches/keystone/2026.1/0001-fixture.patch"
  jq -n --arg rev "$REV" '{rev: $rev, hash: "sha256-demo"}' >"$t/images/demo/source.json"

  jq -n --arg rev "$REV" '{ref: "29.0.0", rev: $rev, hash: "sha256-keystone", project: "keystone",
    patches: ["patches/keystone/2026.1/0001-fixture.patch"]}' >"$t/nix/locks/2026.1/keystone/sources.json"
  write_pyproject keystone '"keystone[ldap]", "python-memcached", "cryptography", "uwsgi==2.0.31"' \
    >"$t/nix/locks/2026.1/keystone/pyproject.toml"
  printf '\n[dependency-groups]\ntest = ["stestr"]\ndemo-compute = ["libvirt-python"]\n' \
    >>"$t/nix/locks/2026.1/keystone/pyproject.toml"
  printf '\n[tool.uv.sources]\nkeystone = { git = "https://github.com/openstack/keystone", rev = "%s" }\n' \
    "$REV" >>"$t/nix/locks/2026.1/keystone/pyproject.toml"
  jq -n '{ref: "46.3.0", project: "tempest"}' >"$t/nix/locks/2026.1/tempest/sources.json"
  write_pyproject tempest \
    '"tempest==46.3.0", "barbican-tempest-plugin==4.6.0", "keystone-tempest-plugin==0.22.0", "neutron-tempest-plugin==3.2.0", "cinder-tempest-plugin==1.23.0", "python-openstackclient", "cryptography", "uwsgi==2.0.31"' \
    >"$t/nix/locks/2026.1/tempest/pyproject.toml"
  printf 'version = 1\n' >"$t/nix/locks/2026.1/keystone/uv.lock"
  printf 'version = 1\n' >"$t/nix/locks/2026.1/tempest/uv.lock"
  printf '%s\n' "$t"
}

# write_pyproject <name> <dependencies>: print a lock's pyproject.toml with
# the merged constraints of the fixture.
write_pyproject() {
  cat <<EOF
[project]
name = "cobaltcore-$1-2026.1"
version = "0"
dependencies = [$2]

[tool.uv]
package = false
constraint-dependencies = ["cryptography==48.0.1", "python_ldap==3.4.5", "lhafile==0.3.1"]
EOF
}

# rewrite <file> <sed-script>: apply a sed script in place (BSD and GNU sed).
rewrite() {
  sed "$2" "$1" >"$1.tmp" && mv "$1.tmp" "$1"
}

# fixture_git <args>: git without the user's and the system's configuration,
# whose core.hooksPath could reject the fixture commits, as isolated_git_env
# of tests/unit/images/python_env_pbr_commit_test.sh keeps it out.
fixture_git() {
  GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null git "$@"
}

# commit_at <dir> <date> <message>: commit the whole tree at that author and
# committer date. --force adds what an excludes file of the user ignores, such
# as .claude/: git reads ~/.config/git/ignore without any configuration.
commit_at() {
  fixture_git -C "$1" add -A --force
  GIT_AUTHOR_DATE="$2" GIT_COMMITTER_DATE="$2" fixture_git -C "$1" -c user.name=test \
    -c user.email=test@example.invalid -c commit.gpgsign=false commit -q -m "$3"
}

# run_audit <dir> [VAR=value...]: run the staged audit with the variables in
# its environment; print its combined output and, last, "exit <code>".
run_audit() {
  local t="$1" rc=0
  shift
  env "$@" bash "$t/$AUDIT" 2>&1 || rc=$?
  echo "exit $rc"
}

# fails <output>: the [FAIL] lines of an audit run, without the summary.
fails() {
  grep '^\[FAIL\]' <<<"$1" | grep -v 'lock drift finding' || true
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_consistent_tree_passes() {
  echo "Test: a tree whose locks match their pins reports no [FAIL] and exits 0"
  skip_unless "yq (mikefarah) or jq not installed" have_tools || return

  local t out
  t="$(stage_tree)"
  out="$(run_audit "$t")"
  assert_eq "no [FAIL]" "" "$(fails "$out")"
  assert_contains "exit 0" "$out" "exit 0"
  assert_contains "L1 passes the keystone lock" "$out" \
    "[PASS] nix/locks/2026.1/keystone: sources.json, pyproject.toml and uv.lock present"
  assert_contains "L2 passes the tempest lock" "$out" \
    "[PASS] nix/locks/2026.1/tempest/sources.json: ref matches releases/2026.1/test-refs.yaml, project is tempest"
  assert_contains "L5 passes the image pin" "$out" "[PASS] images/demo/source.json: rev and hash set"
  assert_contains "L6 passes" "$out" "[PASS] hack/nix-lock.sh --check-operators"
  assert_contains "L7 is skipped outside a git checkout" "$out" "[INFO] L7 skipped: "
}

test_missing_tools_or_releases_fail_at_once() {
  echo "Test: no yq or no releases/ is one [FAIL] and exit 1"
  skip_unless "yq (mikefarah) or jq not installed" have_tools || return

  local t bin out
  t="$(stage_tree)"
  # A yq that is not mikefarah's counts as no yq.
  bin="$(mktemp -d "$TMP_ROOT/bin.XXXXXX")"
  printf '#!/bin/sh\necho "yq 3.4.3"\n' >"$bin/yq"
  chmod +x "$bin/yq"
  out="$(run_audit "$t" PATH="$bin:$PATH")"
  assert_eq "the only line names both tools" \
    "[FAIL] audit-nix-lock-drift: yq (mikefarah) v4.49.1 or newer and jq are required"$'\n'"exit 1" "$out"

  # A mikefarah yq older than v4.49.1 prints "\t" as a backslash and a t.
  cat >"$bin/yq" <<'EOF'
#!/bin/sh
case "$1" in
  --version) echo "yq (https://github.com/mikefarah/yq/) version v4.44.1" ;;
  *) printf '%s\n' 'a\tb' ;;
esac
EOF
  out="$(run_audit "$t" PATH="$bin:$PATH")"
  assert_eq "an old mikefarah yq counts as no yq" \
    "[FAIL] audit-nix-lock-drift: yq (mikefarah) v4.49.1 or newer and jq are required"$'\n'"exit 1" "$out"

  rm -r "$t/releases"
  out="$(run_audit "$t")"
  assert_eq "no releases/ directory" \
    "[FAIL] audit-nix-lock-drift: no releases/ directory under $(cd "$t" && pwd)"$'\n'"exit 1" "$out"
}

test_lagging_ref_fails() {
  echo "Test: a lock whose ref lags source-refs.yaml fails with both values"
  skip_unless "yq (mikefarah) or jq not installed" have_tools || return

  local t out
  t="$(stage_tree)"
  rewrite "$t/releases/2026.1/source-refs.yaml" 's/^keystone: "29.0.0"/keystone: "29.0.1"/'
  out="$(run_audit "$t")"
  assert_eq "the lagging ref is the only finding" \
    "[FAIL] nix/locks/2026.1/keystone/sources.json: ref 29.0.0 differs from 29.0.1, the keystone pin of releases/2026.1/source-refs.yaml" \
    "$(fails "$out")"
  assert_contains "exit 1" "$out" "exit 1"
}

test_malformed_source_and_lagging_tempest_fail() {
  echo "Test: a malformed rev or hash fails, and a tempest lock that lags test-refs.yaml"
  skip_unless "yq (mikefarah) or jq not installed" have_tools || return

  local t out src
  t="$(stage_tree)"
  src="$t/nix/locks/2026.1/keystone/sources.json"
  jq '.rev = "abc" | .hash = "md5-x"' "$src" >"$src.tmp" && mv "$src.tmp" "$src"
  rewrite "$t/releases/2026.1/test-refs.yaml" 's/^tempest: "46.3.0"/tempest: "46.4.0"/'
  out="$(run_audit "$t")"
  assert_eq "the shape, the tempest ref and the tempest head dependency" \
    "[FAIL] nix/locks/2026.1/keystone/sources.json: rev abc is no 40-character commit; hash md5-x does not start with sha256-"$'\n'"[FAIL] nix/locks/2026.1/tempest/sources.json: ref 46.3.0 differs from 46.4.0, the tempest pin of releases/2026.1/test-refs.yaml"$'\n'"[FAIL] nix/locks/2026.1/keystone/pyproject.toml: [tool.uv.sources] fetches keystone at $REV, not at the rev abc of sources.json"$'\n'"[FAIL] nix/locks/2026.1/tempest/pyproject.toml: the first dependency is tempest==46.3.0, not tempest==46.4.0; the dependencies lack tempest==46.4.0" \
    "$(fails "$out")"
}

test_project_name_drift_fails() {
  echo "Test: a sources.json without a project fails, and a tempest lock of another project"
  skip_unless "yq (mikefarah) or jq not installed" have_tools || return

  local t out src
  t="$(stage_tree)"
  src="$t/nix/locks/2026.1/keystone/sources.json"
  jq 'del(.project)' "$src" >"$src.tmp" && mv "$src.tmp" "$src"
  src="$t/nix/locks/2026.1/tempest/sources.json"
  jq '.project = "keystone"' "$src" >"$src.tmp" && mv "$src.tmp" "$src"
  out="$(run_audit "$t")"
  assert_eq "both projects, once each" \
    "[FAIL] nix/locks/2026.1/keystone/sources.json: project is empty"$'\n'"[FAIL] nix/locks/2026.1/tempest/sources.json: project keystone is not tempest" \
    "$(fails "$out")"
}

test_orphan_lock_and_release_fail() {
  echo "Test: a lock no release pins fails, and a release without source-refs.yaml"
  skip_unless "yq (mikefarah) or jq not installed" have_tools || return

  local t out
  t="$(stage_tree)"
  mkdir -p "$t/nix/locks/2026.1/orphan" "$t/nix/locks/2025.2/keystone" "$t/releases/2027.1"
  cp "$t/nix/locks/2026.1/keystone/"* "$t/nix/locks/2026.1/orphan/"
  out="$(run_audit "$t")"
  assert_eq "the orphan lock, the orphan release and the release without pins" \
    "[FAIL] releases/2027.1: no source-refs.yaml"$'\n'"[FAIL] nix/locks/2025.2: no release pins it"$'\n'"[FAIL] nix/locks/2026.1/orphan: no release pins it" \
    "$(fails "$out")"

  t="$(stage_tree)"
  rm "$t/nix/locks/2026.1/tempest/uv.lock" "$t/nix/locks/2026.1/tempest/sources.json"
  out="$(run_audit "$t")"
  assert_eq "a lock without two of its files" \
    "[FAIL] nix/locks/2026.1/tempest: no sources.json, uv.lock" "$(fails "$out")"
}

test_patch_list_drift_fails() {
  echo "Test: a patch file absent from sources.json fails, a listed patch that is gone, and patches out of order"
  skip_unless "yq (mikefarah) or jq not installed" have_tools || return

  local t out src
  t="$(stage_tree)"
  printf 'diff --git a/y b/y\n' >"$t/patches/keystone/2026.1/0002-second.patch"
  out="$(run_audit "$t")"
  assert_eq "an unlisted patch" \
    "[FAIL] nix/locks/2026.1/keystone/sources.json: patches differ from patches/keystone/2026.1/: missing patches/keystone/2026.1/0002-second.patch" \
    "$(fails "$out")"

  t="$(stage_tree)"
  rm -r "$t/patches/keystone"
  out="$(run_audit "$t")"
  assert_eq "a listed patch without its file" \
    "[FAIL] nix/locks/2026.1/keystone/sources.json: patches differ from patches/keystone/2026.1/: extra patches/keystone/2026.1/0001-fixture.patch" \
    "$(fails "$out")"

  # The build applies the patches in list order.
  t="$(stage_tree)"
  printf 'diff --git a/y b/y\n' >"$t/patches/keystone/2026.1/0002-second.patch"
  src="$t/nix/locks/2026.1/keystone/sources.json"
  jq '.patches = ["patches/keystone/2026.1/0002-second.patch", "patches/keystone/2026.1/0001-fixture.patch"]' \
    "$src" >"$src.tmp" && mv "$src.tmp" "$src"
  out="$(run_audit "$t")"
  assert_eq "both patches, listed out of order" \
    "[FAIL] nix/locks/2026.1/keystone/sources.json: patches differ from patches/keystone/2026.1/: listed out of order" \
    "$(fails "$out")"
}

test_unreadable_files_fail_once() {
  echo "Test: a sources.json, pyproject.toml or image pin the parser rejects or of the wrong shape is one [FAIL] naming it"
  skip_unless "yq (mikefarah) or jq not installed" have_tools || return

  local t out
  t="$(stage_tree)"
  printf '{"ref": "29.0.0",\n' >"$t/nix/locks/2026.1/keystone/sources.json"
  out="$(run_audit "$t")"
  assert_starts_with "the broken sources.json" "$(fails "$out")" \
    "[FAIL] nix/locks/2026.1/keystone/sources.json: "
  assert_contains "with jq's message" "$(fails "$out")" "Unfinished JSON term"
  assert_eq "and only it" "1" "$(fails "$out" | wc -l | tr -d ' ')"

  t="$(stage_tree)"
  printf '[project\ndependencies = [\n' >"$t/nix/locks/2026.1/tempest/pyproject.toml"
  out="$(run_audit "$t")"
  assert_starts_with "the broken pyproject.toml with the parser's message" "$(fails "$out")" \
    "[FAIL] nix/locks/2026.1/tempest/pyproject.toml: Error: bad file 'nix/locks/2026.1/tempest/pyproject.toml': expected"
  assert_eq "and only it" "1" "$(fails "$out" | wc -l | tr -d ' ')"

  t="$(stage_tree)"
  rewrite "$t/nix/locks/2026.1/tempest/pyproject.toml" 's/^dependencies = .*/dependencies = [1]/'
  printf '[]\n' >"$t/images/demo/source.json"
  out="$(run_audit "$t")"
  assert_eq "a list of no strings and a pin of no object" \
    "[FAIL] nix/locks/2026.1/tempest/pyproject.toml: Error: [project].dependencies is not a list of strings"$'\n'"[FAIL] images/demo/source.json: not a JSON object" \
    "$(fails "$out")"
}

test_project_input_drift_fails() {
  echo "Test: a missing extra, pip package, base pin or derived group fails"
  skip_unless "yq (mikefarah) or jq not installed" have_tools || return
  skip_unless "sed is no GNU sed" have_gnu_sed || return

  local t out
  t="$(stage_tree)"
  printf '    - pymysql\n' >>"$t/releases/2026.1/extra-packages.yaml"
  rewrite "$t/releases/2026.1/extra-packages.yaml" 's/    - ldap/    - ldap\
    - memcache/'
  rewrite "$t/nix/base-requirements.txt" 's/^uwsgi==2.0.31/uwsgi==2.0.32/'
  out="$(run_audit "$t")"
  assert_eq "the extra, the pip package of the derived block and the base pin" \
    "[FAIL] nix/locks/2026.1/keystone/pyproject.toml: the first dependency is keystone[ldap], not keystone[ldap,memcache]; the dependencies after the project differ from the pip packages and base pins: missing uwsgi==2.0.32; extra uwsgi==2.0.31; the group demo-compute differs from its block in releases/2026.1/extra-packages.yaml: missing pymysql"$'\n'"[FAIL] nix/locks/2026.1/tempest/pyproject.toml: the dependencies lack uwsgi==2.0.32" \
    "$(fails "$out")"

  t="$(stage_tree)"
  rewrite "$t/nix/locks/2026.1/keystone/pyproject.toml" '/^demo-compute = /d'
  out="$(run_audit "$t")"
  assert_eq "a derived block that no lock carries" \
    "[FAIL] releases/2026.1/extra-packages.yaml: the block demo-compute is the dependency group of no lock" \
    "$(fails "$out")"

  t="$(stage_tree)"
  printf '\n[dependency-groups]\ndemo-compute = ["libvirt-python"]\n' >>"$t/nix/locks/2026.1/tempest/pyproject.toml"
  out="$(run_audit "$t")"
  assert_eq "a derived block that two locks carry" \
    "[FAIL] releases/2026.1/extra-packages.yaml: the block demo-compute is a dependency group of 2026.1/keystone, 2026.1/tempest" \
    "$(fails "$out")"

  t="$(stage_tree)"
  rm "$t/releases/2026.1/extra-packages.yaml"
  out="$(run_audit "$t")"
  assert_eq "a release without extra-packages.yaml, and no lock blamed for it" \
    "[FAIL] releases/2026.1: no extra-packages.yaml" "$(fails "$out")"

  t="$(stage_tree)"
  printf 'keystone:\n\tpip_extras: [ldap]\n' >"$t/releases/2026.1/extra-packages.yaml"
  out="$(run_audit "$t")"
  assert_starts_with "an extra-packages.yaml yq cannot parse, with its message" "$(fails "$out")" \
    "[FAIL] releases/2026.1/extra-packages.yaml: Error: bad file 'releases/2026.1/extra-packages.yaml': "
  assert_eq "and no lock blamed for it" "1" "$(fails "$out" | wc -l | tr -d ' ')"

  t="$(stage_tree)"
  printf -- '- keystone\n' >"$t/releases/2026.1/extra-packages.yaml"
  out="$(run_audit "$t")"
  assert_eq "an extra-packages.yaml of no map" \
    "[FAIL] releases/2026.1/extra-packages.yaml: not a YAML map" "$(fails "$out")"

  # A map whose blocks yq rejects: an extras scalar, a block written as a list.
  t="$(stage_tree)"
  printf 'keystone:\n  pip_extras: ldap\ndemo-compute:\n  - pip_packages: [libvirt-python]\n' \
    >"$t/releases/2026.1/extra-packages.yaml"
  out="$(run_audit "$t")"
  assert_starts_with "a block yq rejects, with its message" "$(fails "$out")" \
    "[FAIL] releases/2026.1/extra-packages.yaml: the block keystone: Error: "
  assert_contains "and the other" "$(fails "$out")" \
    "[FAIL] releases/2026.1/extra-packages.yaml: the block demo-compute: Error: "
  assert_eq "and no lock blamed for them" "2" "$(fails "$out" | wc -l | tr -d ' ')"
  assert_contains "the audit runs to its summary" "$out" "[FAIL] 2 lock drift finding(s)"
}

test_unpinned_dependency_fails() {
  echo "Test: a dependency that no pip package or base pin names fails"
  skip_unless "yq (mikefarah) or jq not installed" have_tools || return
  skip_unless "sed is no GNU sed" have_gnu_sed || return

  local t out
  t="$(stage_tree)"
  rewrite "$t/releases/2026.1/extra-packages.yaml" '/    - python-memcached/d'
  rewrite "$t/nix/base-requirements.txt" '/^uwsgi==/d'
  out="$(run_audit "$t")"
  assert_eq "the dropped pip package and base pin, for the service lock only" \
    "[FAIL] nix/locks/2026.1/keystone/pyproject.toml: the dependencies after the project differ from the pip packages and base pins: extra python-memcached, uwsgi==2.0.31" \
    "$(fails "$out")"
}

test_tempest_pins_follow_nix_lock() {
  echo "Test: the tempest lock holds the five pins hack/nix-lock.sh installs"
  skip_unless "yq (mikefarah) or jq not installed" have_tools || return

  local t out
  t="$(stage_tree)"
  printf 'octavia-tempest-plugin: "2.9.0"\n' >>"$t/releases/2026.1/test-refs.yaml"
  out="$(run_audit "$t")"
  assert_eq "a pin hack/nix-lock.sh does not install" "" "$(fails "$out")"

  t="$(stage_tree)"
  rewrite "$t/releases/2026.1/test-refs.yaml" '/^cinder-tempest-plugin:/d'
  out="$(run_audit "$t")"
  assert_eq "a pin hack/nix-lock.sh needs" \
    "[FAIL] nix/locks/2026.1/tempest/pyproject.toml: releases/2026.1/test-refs.yaml pins no cinder-tempest-plugin" \
    "$(fails "$out")"
}

test_uv_source_rev_drift_fails() {
  echo "Test: a [tool.uv.sources] rev other than the one of sources.json fails"
  skip_unless "yq (mikefarah) or jq not installed" have_tools || return

  local t out other="0123456789abcdef0123456789abcdef01234567"
  t="$(stage_tree)"
  rewrite "$t/nix/locks/2026.1/keystone/pyproject.toml" "s/rev = \"$REV\"/rev = \"$other\"/"
  out="$(run_audit "$t")"
  assert_eq "the rev uv resolved against" \
    "[FAIL] nix/locks/2026.1/keystone/pyproject.toml: [tool.uv.sources] fetches keystone at $other, not at the rev $REV of sources.json" \
    "$(fails "$out")"

  t="$(stage_tree)"
  rewrite "$t/nix/locks/2026.1/keystone/pyproject.toml" '/^keystone = { git/d'
  out="$(run_audit "$t")"
  assert_eq "no source for the project" \
    "[FAIL] nix/locks/2026.1/keystone/pyproject.toml: [tool.uv.sources] fetches keystone at (none), not at the rev $REV of sources.json" \
    "$(fails "$out")"
}

test_constraint_drift_fails() {
  echo "Test: an override line absent from constraint-dependencies fails"
  skip_unless "yq (mikefarah) or jq not installed" have_tools || return
  skip_unless "sed is no GNU sed" have_gnu_sed || return

  local t out
  t="$(stage_tree)"
  printf 'lhafile===0.0.1\n' >>"$t/overrides/2026.1/constraints.txt"
  out="$(run_audit "$t")"
  assert_contains "the keystone lock names the override it lacks" "$(fails "$out")" \
    "[FAIL] nix/locks/2026.1/keystone/pyproject.toml: constraint-dependencies differ from the merged upper-constraints: missing lhafile==0.0.1; extra lhafile==0.3.1"
  assert_eq "one finding per lock" "2" "$(fails "$out" | wc -l | tr -d ' ')"

  t="$(stage_tree)"
  rm "$t/releases/2026.1/upper-constraints.txt"
  out="$(run_audit "$t")"
  assert_starts_with "a failed merge" "$(fails "$out")" \
    "[FAIL] L4 2026.1: apply-constraint-overrides.sh exited 1 (Error: 'releases/2026.1/upper-constraints.txt' not found."
  assert_not_contains "L4 is skipped for that release" "$out" "constraint-dependencies match"
}

test_image_pin_and_operator_drift_fail() {
  echo "Test: a malformed image pin fails, and a stale operator vendor hash"
  skip_unless "yq (mikefarah) or jq not installed" have_tools || return

  local t out
  t="$(stage_tree)"
  jq '.rev = "abc" | .hash = ""' "$t/images/demo/source.json" >"$t/pin.json"
  mv "$t/pin.json" "$t/images/demo/source.json"
  printf 'pin.ovs\n' >"$t/images/demo/demo.nix"
  : >"$t/images/demo/manager.nix"
  out="$(run_audit "$t" NIX_LOCK_STUB_FAIL=1)"
  assert_eq "every defect of the pin and the stale vendor hash" \
    "[FAIL] images/demo/source.json: rev is no 40-character commit; hash is empty; ovs.rev is no 40-character commit; ovs.hash is empty; vendorHash is empty"$'\n'"[FAIL] hack/nix-lock.sh --check-operators exited 1: operators/demo/vendor.json" \
    "$(fails "$out")"
}

test_lock_older_than_pin_is_an_info() {
  echo "Test: L7 names a pin committed after its lock, and a shallow clone skips it"
  skip_unless "yq (mikefarah) or jq not installed" have_tools || return
  skip_unless "git not installed" have_git || return

  local t out
  t="$(stage_tree)"
  fixture_git -C "$t" init -q
  commit_at "$t" 2026-01-01T00:00:00Z locks
  printf '# bumped\n' >>"$t/releases/2026.1/source-refs.yaml"
  commit_at "$t" 2026-02-01T00:00:00Z pin
  out="$(run_audit "$t")"
  assert_contains "the keystone lock is older than its source-refs.yaml" "$out" \
    "[INFO] nix/locks/2026.1/keystone: releases/2026.1/source-refs.yaml changed on 2026-02-01, after the lock (2026-01-01); make nix-lock-check decides"
  assert_contains "the tempest lock does not read source-refs.yaml" "$out" \
    "[INFO] nix/locks/2026.1/tempest: no pin changed after the lock (2026-01-01)"
  assert_eq "L7 adds no [FAIL]" "" "$(fails "$out")"

  fixture_git clone -q --depth 1 "file://$t" "$t.shallow"
  out="$(run_audit "$t.shallow")"
  assert_contains "a shallow clone skips L7" "$out" "[INFO] L7 skipped: shallow clone"
}

test_without_gnu_sed_skips_the_merge() {
  echo "Test: without GNU sed L4 and the base pins of L3 are skipped with an [INFO] each"
  skip_unless "yq (mikefarah) or jq not installed" have_tools || return

  local t bin out
  t="$(stage_tree)"
  # A sed without the in-place edit, as the merge needs it.
  bin="$(mktemp -d "$TMP_ROOT/bin.XXXXXX")"
  printf '#!/bin/sh\ncase "$*" in *-i*) exit 1 ;; esac\nexec %s "$@"\n' "$(command -v sed)" >"$bin/sed"
  chmod +x "$bin/sed"
  rewrite "$t/nix/base-requirements.txt" 's/^uwsgi==2.0.31/uwsgi==2.0.32/'
  out="$(run_audit "$t" PATH="$bin:$PATH")"
  assert_contains "L4 is skipped" "$out" \
    "[INFO] L4 skipped: scripts/apply-constraint-overrides.sh needs GNU sed; run the audit inside nix develop .#lock-tools"
  assert_contains "the base pins are skipped" "$out" \
    "[INFO] releases/2026.1: the base pins are not checked, the constraints did not merge"
  assert_eq "the moved base pin goes unseen" "" "$(fails "$out")"
}

test_repository_passes() {
  echo "Test: the audit passes on the repository"
  skip_unless "yq (mikefarah) or jq not installed" have_tools || return

  local out
  out="$(run_audit "$PROJECT_ROOT")"
  assert_eq "no [FAIL] at HEAD" "" "$(fails "$out")"
  assert_contains "exit 0" "$out" "exit 0"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_consistent_tree_passes
test_missing_tools_or_releases_fail_at_once
test_lagging_ref_fails
test_malformed_source_and_lagging_tempest_fail
test_project_name_drift_fails
test_orphan_lock_and_release_fail
test_patch_list_drift_fails
test_unreadable_files_fail_once
test_project_input_drift_fails
test_unpinned_dependency_fails
test_tempest_pins_follow_nix_lock
test_uv_source_rev_drift_fails
test_constraint_drift_fails
test_image_pin_and_operator_drift_fail
test_lock_older_than_pin_is_an_info
test_without_gnu_sed_skips_the_merge
test_repository_passes

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
