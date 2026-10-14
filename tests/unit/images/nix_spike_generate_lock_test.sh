#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify nix/spike/generate-lock.sh refuses bad input before it writes
# anything, writes the release inputs into pyproject.toml and sources.json,
# leaves the previous lock as it was when a step fails, and that the committed
# 2026.1 locks it wrote are well formed.
#
# The generator checks the release directory first, the four tools next and
# the service key last, so the first two failures need no tool at all. The
# key check runs with stub uv and nix commands on PATH, which exit non-zero
# when called. The other runs use a throwaway repository root with fixture
# release files for a service named demo and stub commands on PATH: nix
# answers the prefetch with a fixture upstream tree, uv writes a placeholder
# uv.lock. They need yq and jq and never reach the network. Every run uses
# the bash that runs this file. The generator needs Bash 4 or later past the
# prefetch, so the two runs that get there skip under an older bash.
#
# The committed locks are pinned by hand and stay behind when Renovate moves
# a pin in releases/2026.1/source-refs.yaml or a patch is added or retired, so
# they are checked for their shape, not against the current release files.
#
# Usage: bash tests/unit/images/nix_spike_generate_lock_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

GENERATOR="$PROJECT_ROOT/nix/spike/generate-lock.sh"
LOCKS="$PROJECT_ROOT/nix/spike/locks/2026.1"
SERVICES=(keystone nova glance)
FIXTURE_REV=0123456789abcdef0123456789abcdef01234567

# make_tree prints the path of a throwaway repository root for the service
# demo: the generator, scripts/apply-constraint-overrides.sh, the release
# files, one patch, an upstream tree with a test-requirements.txt, and stub
# nix and uv commands in stubs/.
make_tree() {
  local tree
  tree=$(mktemp -d)
  mkdir -p "$tree/nix/spike" "$tree/scripts" "$tree/releases/2026.1" "$tree/overrides/2026.1" \
    "$tree/images/venv-builder" "$tree/patches/demo/2026.1" "$tree/upstream" "$tree/stubs"
  cp "$GENERATOR" "$tree/nix/spike/"
  cp "$PROJECT_ROOT/scripts/apply-constraint-overrides.sh" "$tree/scripts/"

  echo 'demo: "1.2.3"' >"$tree/releases/2026.1/source-refs.yaml"
  printf '%s\n' 'demo:' '  pip_extras: [ldap, oauth1]' '  pip_packages: [lhafile]' \
    >"$tree/releases/2026.1/extra-packages.yaml"
  printf '%s\n' 'oslo.db===18.0.0' 'cryptography===43.0.3' 'horizon===25.7.0' \
    >"$tree/releases/2026.1/upper-constraints.txt"
  echo '-horizon' >"$tree/overrides/2026.1/constraints.txt"
  printf '%s\n' 'cryptography==48.0.1' 'uwsgi==2.0.31' >"$tree/images/venv-builder/requirements.txt"
  printf '%s\n' '--- a/demo/api.py' '+++ b/demo/api.py' >"$tree/patches/demo/2026.1/0001-fix.patch"
  printf '%s\n' '# Comment line' 'hacking~=6.1.0 # Apache-2.0' '' '-r requirements.txt' \
    '  foo>=1.0  ' 'bar; python_version >= "3.10"' 'stestr>=1.0.0' \
    >"$tree/upstream/test-requirements.txt"

  cat >"$tree/stubs/nix" <<EOF
#!/bin/sh
echo '{"locked":{"rev":"$FIXTURE_REV"},"hash":"sha256-fixture","storePath":"$tree/upstream"}'
EOF
  printf '#!/bin/sh\necho "# stub lock" >uv.lock\n' >"$tree/stubs/uv"
  chmod +x "$tree/stubs/nix" "$tree/stubs/uv"
  echo "$tree"
}

# seed_lock writes a previous lock for demo into the tree.
seed_lock() {
  local dest="$1/nix/spike/locks/2026.1/demo" file
  mkdir -p "$dest"
  for file in pyproject.toml uv.lock sources.json; do
    echo "previous $file" >"$dest/$file"
  done
}

# lock_state prints a checksum of the names and contents of a lock directory.
lock_state() {
  (cd "$1" && ls -A && cat -- *) | cksum
}

test_usage_and_missing_release() {
  echo "Test: a wrong argument count and an unknown release exit 1 before any tool runs"
  local output exit_code=0

  output=$("$BASH" "$GENERATOR" keystone 2>&1) || exit_code=$?
  assert_eq "one argument exits 1" "1" "$exit_code"
  assert_contains "the usage line is printed" "$output" "Usage:"

  exit_code=0
  output=$(env PATH=/nonexistent "$BASH" "$GENERATOR" keystone 2027.9 2>&1) || exit_code=$?
  assert_eq "release 2027.9 exits 1" "1" "$exit_code"
  assert_contains "the message names the missing source-refs.yaml" \
    "$output" "releases/2027.9/source-refs.yaml"
}

test_missing_tools_are_named_and_locks_untouched() {
  echo "Test: tools missing from PATH are named and the committed locks stay as they are"
  local before after output exit_code=0

  before=$(cat "$LOCKS"/*/* | cksum)
  output=$(env PATH=/nonexistent "$BASH" "$GENERATOR" keystone 2026.1 2>&1) || exit_code=$?
  after=$(cat "$LOCKS"/*/* | cksum)

  assert_eq "an empty PATH exits 1" "1" "$exit_code"
  assert_contains "the message names all four tools" "$output" \
    "generate-lock.sh: required tool(s) not on PATH: uv nix yq jq"
  assert_eq "the committed lock files are unchanged" "$before" "$after"
}

test_missing_service_key_names_the_key() {
  echo "Test: a service without a source-refs.yaml key exits 1 and names the key"

  if ! command -v yq >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: yq or jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local stubs output exit_code=0
  stubs=$(mktemp -d)
  printf '#!/bin/sh\necho "stub $0 called" >&2\nexit 3\n' >"$stubs/uv"
  cp "$stubs/uv" "$stubs/nix"
  chmod +x "$stubs/uv" "$stubs/nix"

  output=$(env PATH="$stubs:$PATH" "$BASH" "$GENERATOR" swift 2026.1 2>&1) || exit_code=$?
  rm -rf "$stubs"

  assert_eq "service swift exits 1" "1" "$exit_code"
  assert_contains "the message names the key and the file" \
    "$output" "no key 'swift' in releases/2026.1/source-refs.yaml"
  assert_not_contains "neither uv nor nix ran" "$output" "stub"
}

test_offline_run_writes_the_release_inputs() {
  echo "Test: an offline run writes the release inputs into pyproject.toml and sources.json"

  if ! command -v yq >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: yq or jq not installed (18 checks skipped)"
    SKIP=$((SKIP + 18))
    return
  fi
  if [ "${BASH_VERSINFO[0]}" -lt 4 ]; then
    echo "  SKIP: bash $BASH_VERSION has no associative arrays (18 checks skipped)"
    SKIP=$((SKIP + 18))
    return
  fi

  local tree out pyproject output exit_code=0
  tree=$(make_tree)
  out="$tree/nix/spike/locks/2026.1/demo"
  pyproject="$out/pyproject.toml"

  output=$(env PATH="$tree/stubs:$PATH" "$BASH" "$tree/nix/spike/generate-lock.sh" demo 2026.1 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "$output"
  assert_eq "the run exits 0" "0" "$exit_code"

  assert_file_contains "the extras are joined onto the service" "$pyproject" '^    "demo\[ldap,oauth1\]",$'
  assert_file_contains "the pip_packages are dependencies" "$pyproject" '^    "lhafile",$'
  assert_file_contains "a venv-builder package the constraints pin is listed by name" \
    "$pyproject" '^    "cryptography",$'
  assert_file_contains "a venv-builder package the constraints lack keeps its pin" \
    "$pyproject" '^    "uwsgi==2.0.31",$'
  assert_file_contains "a === constraint is written with ==" "$pyproject" '^    "oslo.db==18.0.0",$'
  assert_file_not_contains "a constraint the overrides remove is left out" "$pyproject" '"horizon=='
  assert_file_contains "a trailing comment is stripped from a test requirement" \
    "$pyproject" '^    "hacking~=6.1.0",$'
  assert_file_contains "surrounding whitespace is trimmed from a test requirement" \
    "$pyproject" '^    "foo>=1.0",$'
  assert_file_contains_fixed "a double quote in a test requirement is escaped" \
    "$pyproject" '    "bar; python_version >= \"3.10\"",'
  assert_file_not_contains "a comment line is no test requirement" "$pyproject" 'Comment line'
  assert_file_not_contains "a pip option is no test requirement" "$pyproject" '"-r requirements.txt"'
  assert_file_contains "stestr keeps the range test-requirements.txt gives it" \
    "$pyproject" '^    "stestr>=1.0.0",$'
  assert_file_not_contains "stestr is not listed a second time" "$pyproject" '^    "stestr",$'
  assert_file_contains "testtools, which test-requirements.txt lacks, is added" \
    "$pyproject" '^    "testtools",$'
  assert_file_contains_fixed "the git source names the prefetched commit" "$pyproject" \
    "demo = { git = \"https://github.com/openstack/demo\", rev = \"$FIXTURE_REV\" }"
  assert_eq "sources.json carries the ref, the prefetched commit and hash, and the patch" \
    "{\"ref\":\"1.2.3\",\"rev\":\"$FIXTURE_REV\",\"hash\":\"sha256-fixture\",\"patches\":[\"patches/demo/2026.1/0001-fix.patch\"]}" \
    "$(jq -c . "$out/sources.json")"
  assert_eq "uv.lock is the file uv lock wrote" "# stub lock" "$(cat "$out/uv.lock")"
  rm -rf "$tree"
}

test_failed_prefetch_or_lock_leaves_the_lock_untouched() {
  echo "Test: a failing nix prefetch or uv lock exits with its status and leaves the previous lock as it was"

  if ! command -v yq >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: yq or jq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi
  if [ "${BASH_VERSINFO[0]}" -lt 4 ]; then
    echo "  SKIP: bash $BASH_VERSION has no associative arrays (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local tree dest before exit_code=0
  tree=$(make_tree)
  dest="$tree/nix/spike/locks/2026.1/demo"
  seed_lock "$tree"
  before=$(lock_state "$dest")

  mv "$tree/stubs/nix" "$tree/stubs/nix.ok"
  printf '#!/bin/sh\nexit 3\n' >"$tree/stubs/nix"
  chmod +x "$tree/stubs/nix"
  env PATH="$tree/stubs:$PATH" "$BASH" "$tree/nix/spike/generate-lock.sh" demo 2026.1 >/dev/null 2>&1 || exit_code=$?
  assert_eq "a failing nix prefetch exits with nix's status" "3" "$exit_code"
  assert_eq "the lock is unchanged after a failed prefetch" "$before" "$(lock_state "$dest")"

  mv "$tree/stubs/nix.ok" "$tree/stubs/nix"
  printf '#!/bin/sh\nexit 4\n' >"$tree/stubs/uv"
  exit_code=0
  env PATH="$tree/stubs:$PATH" "$BASH" "$tree/nix/spike/generate-lock.sh" demo 2026.1 >/dev/null 2>&1 || exit_code=$?
  assert_eq "a failing uv lock exits with uv's status" "4" "$exit_code"
  assert_eq "the lock is unchanged after a failed uv lock" "$before" "$(lock_state "$dest")"
  rm -rf "$tree"
}

test_patch_editing_requirements_is_refused() {
  echo "Test: a release patch that edits test-requirements.txt is refused before the prefetch"

  if ! command -v yq >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: yq or jq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local tree dest before output exit_code=0
  tree=$(make_tree)
  dest="$tree/nix/spike/locks/2026.1/demo"
  seed_lock "$tree"
  before=$(lock_state "$dest")
  printf '%s\n' '--- a/test-requirements.txt' '+++ b/test-requirements.txt' '@@ -1 +1,2 @@' \
    ' stestr>=1.0.0' '+fixtures>=3.0.0' >"$tree/patches/demo/2026.1/0002-test-requirements.patch"
  printf '#!/bin/sh\necho "stub nix called" >&2\nexit 3\n' >"$tree/stubs/nix"

  output=$(env PATH="$tree/stubs:$PATH" "$BASH" "$tree/nix/spike/generate-lock.sh" demo 2026.1 2>&1) || exit_code=$?
  assert_eq "the run exits 1" "1" "$exit_code"
  assert_contains "the message names the patch" "$output" \
    "patches/demo/2026.1/0002-test-requirements.patch edits the requirements"
  assert_not_contains "nix did not run" "$output" "stub nix called"
  assert_eq "the lock is unchanged" "$before" "$(lock_state "$dest")"
  rm -rf "$tree"
}

test_committed_sources_are_well_formed() {
  echo "Test: each committed sources.json carries a release tag, a commit, a hash and its own patch paths"

  if ! command -v jq >/dev/null 2>&1; then
    local skipped=$((4 * ${#SERVICES[@]}))
    echo "  SKIP: jq not installed (${skipped} checks skipped)"
    SKIP=$((SKIP + skipped))
    return
  fi

  local svc sources ref other
  for svc in "${SERVICES[@]}"; do
    sources="$LOCKS/$svc/sources.json"

    ref=$(jq -r '.ref' "$sources")
    if [[ "$ref" =~ ^[0-9]+(\.[0-9]+)+$ ]]; then
      echo "  PASS: $svc ref is a release tag"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $svc ref is not a release tag: $ref"
      FAIL=$((FAIL + 1))
    fi
    if [[ "$(jq -r '.rev' "$sources")" =~ ^[0-9a-f]{40}$ ]]; then
      echo "  PASS: $svc rev is a full commit id"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $svc rev is not a 40-character commit id: $(jq -r '.rev' "$sources")"
      FAIL=$((FAIL + 1))
    fi
    assert_starts_with "$svc hash is an SRI sha256" "$(jq -r '.hash' "$sources")" "sha256-"
    other=$(jq -r '.patches[]' "$sources" | grep -v "^patches/$svc/2026\.1/[^/]*\.patch$")
    assert_eq "$svc patches lie in patches/$svc/2026.1/" "" "$other"
  done
}

test_committed_pyproject_carries_the_release_inputs() {
  echo "Test: the committed pyproject.toml files carry the release inputs"
  local keystone="$LOCKS/keystone/pyproject.toml" rev

  rev=$(sed -n 's/.*"rev": "\(.*\)".*/\1/p' "$LOCKS/keystone/sources.json")

  assert_file_contains "keystone carries its ldap extra" "$keystone" '"keystone\[ldap\]",'
  assert_file_contains "uwsgi keeps its venv-builder pin" "$keystone" '"uwsgi==2.0.31",'
  assert_file_contains "the git source names the commit of sources.json" \
    "$keystone" "rev = \"$rev\""
  assert_file_contains "the lhafile override is a constraint" "$keystone" '"lhafile==0.3.1",'
  assert_file_not_contains "the horizon pin is removed by the overrides" "$keystone" '"horizon=='
  assert_file_contains "cryptography is constrained, not pinned twice" "$keystone" '^    "cryptography",$'
  assert_file_contains "the test group carries keystone's test-requirements" \
    "$keystone" '^    "oslotest>=3.2.0",$'
  assert_file_contains "oslo.db keeps its extras in the test group" \
    "$keystone" '^    "oslo.db\[fixtures,mysql,postgresql\]>=6.0.0",$'
  assert_file_contains "the SPDX licence comment is written" "$keystone" '^# SPDX-License-Identifier: Apache-2.0$'
  assert_file_contains "glance carries glance_store[s3]" \
    "$LOCKS/glance/pyproject.toml" '"glance_store\[s3\]",'
  assert_file_contains "nova is listed without extras" "$LOCKS/nova/pyproject.toml" '^    "nova",$'
}

test_generator_is_shellcheck_clean() {
  echo "Test: generate-lock.sh is shellcheck-clean at --severity=warning"

  if ! command -v shellcheck >/dev/null 2>&1; then
    echo "  SKIP: shellcheck not installed"
    SKIP=$((SKIP + 1))
    return
  fi

  local output exit_code=0
  output=$(shellcheck --severity=warning "$GENERATOR" 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "$output"
  assert_eq "shellcheck exits 0" "0" "$exit_code"
}

test_usage_and_missing_release
test_missing_tools_are_named_and_locks_untouched
test_missing_service_key_names_the_key
test_offline_run_writes_the_release_inputs
test_failed_prefetch_or_lock_leaves_the_lock_untouched
test_patch_editing_requirements_is_refused
test_committed_sources_are_well_formed
test_committed_pyproject_carries_the_release_inputs
test_generator_is_shellcheck_clean

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
