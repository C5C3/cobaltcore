#!/usr/bin/env bats
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
# Usage: bin/bats tests/unit/images/nix_spike_generate_lock.bats

load '../../lib/bats/common'
load '../../lib/bats/stubs'

GENERATOR="$PROJECT_ROOT/nix/spike/generate-lock.sh"
LOCKS="$PROJECT_ROOT/nix/spike/locks/2026.1"
SERVICES=(keystone nova glance)
FIXTURE_REV=0123456789abcdef0123456789abcdef01234567

# make_tree prints the path of a throwaway repository root for the service
# demo: the generator, scripts/apply-constraint-overrides.sh, the release
# files, one patch, an upstream tree with a test-requirements.txt. The stub
# nix and uv commands come from stub_tree_tools.
make_tree() {
  local tree
  tree=$(mktemp -d "$BATS_TEST_TMPDIR/tree.XXXXXX")
  mkdir -p "$tree/nix/spike" "$tree/scripts" "$tree/releases/2026.1" "$tree/overrides/2026.1" \
    "$tree/images/venv-builder" "$tree/patches/demo/2026.1" "$tree/upstream"
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
  echo "$tree"
}

# stub_tree_tools <tree> declares the stub nix and uv commands for a tree of
# make_tree: nix answers the prefetch with the upstream tree of <tree>, uv
# writes a placeholder uv.lock.
stub_tree_tools() {
  local tree="$1"
  stub_script nix <<EOF
echo '{"locked":{"rev":"$FIXTURE_REV"},"hash":"sha256-fixture","storePath":"$tree/upstream"}'
EOF
  stub_script uv <<'EOF'
echo "# stub lock" >uv.lock
EOF
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

@test "a wrong argument count and an unknown release exit 1 before any tool runs" {
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

@test "tools missing from PATH are named and the committed locks stay as they are" {
  local before after output exit_code=0

  before=$(cat "$LOCKS"/*/* | cksum)
  output=$(env PATH=/nonexistent "$BASH" "$GENERATOR" keystone 2026.1 2>&1) || exit_code=$?
  after=$(cat "$LOCKS"/*/* | cksum)

  assert_eq "an empty PATH exits 1" "1" "$exit_code"
  assert_contains "the message names all four tools" "$output" \
    "generate-lock.sh: required tool(s) not on PATH: uv nix yq jq"
  assert_eq "the committed lock files are unchanged" "$before" "$after"
}

@test "a service without a source-refs.yaml key exits 1 and names the key" {
  require_tool yq jq

  local output exit_code=0
  stub_script uv <<'EOF'
echo "stub $0 called" >&2
exit 3
EOF
  stub_script nix <<'EOF'
echo "stub $0 called" >&2
exit 3
EOF

  output=$(with_stubs "$BASH" "$GENERATOR" swift 2026.1 2>&1) || exit_code=$?

  assert_eq "service swift exits 1" "1" "$exit_code"
  assert_contains "the message names the key and the file" \
    "$output" "no key 'swift' in releases/2026.1/source-refs.yaml"
  assert_not_contains "neither uv nor nix ran" "$output" "stub"
}

@test "an offline run writes the release inputs into pyproject.toml and sources.json" {
  require_tool yq jq
  [ "${BASH_VERSINFO[0]}" -ge 4 ] || skip "bash $BASH_VERSION has no associative arrays"

  local tree out pyproject output exit_code=0
  tree=$(make_tree)
  stub_tree_tools "$tree"
  out="$tree/nix/spike/locks/2026.1/demo"
  pyproject="$out/pyproject.toml"

  output=$(with_stubs "$BASH" "$tree/nix/spike/generate-lock.sh" demo 2026.1 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "$output"
  assert_eq "the run exits 0" "0" "$exit_code"

  assert_file_matches "the extras are joined onto the service" "$pyproject" '^    "demo\[ldap,oauth1\]",$'
  assert_file_matches "the pip_packages are dependencies" "$pyproject" '^    "lhafile",$'
  assert_file_matches "a venv-builder package the constraints pin is listed by name" \
    "$pyproject" '^    "cryptography",$'
  assert_file_matches "a venv-builder package the constraints lack keeps its pin" \
    "$pyproject" '^    "uwsgi==2.0.31",$'
  assert_file_matches "a === constraint is written with ==" "$pyproject" '^    "oslo.db==18.0.0",$'
  assert_file_not_matches "a constraint the overrides remove is left out" "$pyproject" '"horizon=='
  assert_file_matches "a trailing comment is stripped from a test requirement" \
    "$pyproject" '^    "hacking~=6.1.0",$'
  assert_file_matches "surrounding whitespace is trimmed from a test requirement" \
    "$pyproject" '^    "foo>=1.0",$'
  assert_file_contains_fixed "a double quote in a test requirement is escaped" \
    "$pyproject" '    "bar; python_version >= \"3.10\"",'
  assert_file_not_matches "a comment line is no test requirement" "$pyproject" 'Comment line'
  assert_file_not_matches "a pip option is no test requirement" "$pyproject" '"-r requirements.txt"'
  assert_file_matches "stestr keeps the range test-requirements.txt gives it" \
    "$pyproject" '^    "stestr>=1.0.0",$'
  assert_file_not_matches "stestr is not listed a second time" "$pyproject" '^    "stestr",$'
  assert_file_matches "testtools, which test-requirements.txt lacks, is added" \
    "$pyproject" '^    "testtools",$'
  assert_file_contains_fixed "the git source names the prefetched commit" "$pyproject" \
    "demo = { git = \"https://github.com/openstack/demo\", rev = \"$FIXTURE_REV\" }"
  assert_eq "sources.json carries the ref, the prefetched commit and hash, and the patch" \
    "{\"ref\":\"1.2.3\",\"rev\":\"$FIXTURE_REV\",\"hash\":\"sha256-fixture\",\"patches\":[\"patches/demo/2026.1/0001-fix.patch\"]}" \
    "$(jq -c . "$out/sources.json")"
  assert_eq "uv.lock is the file uv lock wrote" "# stub lock" "$(cat "$out/uv.lock")"
}

@test "a failing nix prefetch or uv lock exits with its status and leaves the previous lock as it was" {
  require_tool yq jq
  [ "${BASH_VERSINFO[0]}" -ge 4 ] || skip "bash $BASH_VERSION has no associative arrays"

  local tree dest before exit_code=0
  tree=$(make_tree)
  stub_tree_tools "$tree"
  dest="$tree/nix/spike/locks/2026.1/demo"
  seed_lock "$tree"
  before=$(lock_state "$dest")

  stub_script nix <<'EOF'
exit 3
EOF
  with_stubs "$BASH" "$tree/nix/spike/generate-lock.sh" demo 2026.1 >/dev/null 2>&1 || exit_code=$?
  assert_eq "a failing nix prefetch exits with nix's status" "3" "$exit_code"
  assert_eq "the lock is unchanged after a failed prefetch" "$before" "$(lock_state "$dest")"

  stub_tree_tools "$tree"
  stub_script uv <<'EOF'
exit 4
EOF
  exit_code=0
  with_stubs "$BASH" "$tree/nix/spike/generate-lock.sh" demo 2026.1 >/dev/null 2>&1 || exit_code=$?
  assert_eq "a failing uv lock exits with uv's status" "4" "$exit_code"
  assert_eq "the lock is unchanged after a failed uv lock" "$before" "$(lock_state "$dest")"
}

@test "a release patch that edits test-requirements.txt is refused before the prefetch" {
  require_tool yq jq

  local tree dest before output exit_code=0
  tree=$(make_tree)
  stub_tree_tools "$tree"
  dest="$tree/nix/spike/locks/2026.1/demo"
  seed_lock "$tree"
  before=$(lock_state "$dest")
  printf '%s\n' '--- a/test-requirements.txt' '+++ b/test-requirements.txt' '@@ -1 +1,2 @@' \
    ' stestr>=1.0.0' '+fixtures>=3.0.0' >"$tree/patches/demo/2026.1/0002-test-requirements.patch"
  stub_script nix <<'EOF'
echo "stub nix called" >&2
exit 3
EOF

  output=$(with_stubs "$BASH" "$tree/nix/spike/generate-lock.sh" demo 2026.1 2>&1) || exit_code=$?
  assert_eq "the run exits 1" "1" "$exit_code"
  assert_contains "the message names the patch" "$output" \
    "patches/demo/2026.1/0002-test-requirements.patch edits the requirements"
  assert_not_contains "nix did not run" "$output" "stub nix called"
  assert_eq "the lock is unchanged" "$before" "$(lock_state "$dest")"
}

@test "each committed sources.json carries a release tag, a commit, a hash and its own patch paths" {
  require_tool jq

  local svc sources ref other
  for svc in "${SERVICES[@]}"; do
    sources="$LOCKS/$svc/sources.json"

    ref=$(jq -r '.ref' "$sources")
    if [[ "$ref" =~ ^[0-9]+(\.[0-9]+)+$ ]]; then
      check_passed "$svc ref is a release tag"
    else
      echo "$svc ref is not a release tag: $ref"
      soft fail "$svc ref is a release tag"
    fi
    if [[ "$(jq -r '.rev' "$sources")" =~ ^[0-9a-f]{40}$ ]]; then
      check_passed "$svc rev is a full commit id"
    else
      echo "$svc rev is not a 40-character commit id: $(jq -r '.rev' "$sources")"
      soft fail "$svc rev is a full commit id"
    fi
    soft assert_starts_with "$svc hash is an SRI sha256" "$(jq -r '.hash' "$sources")" "sha256-"
    other=$(jq -r '.patches[]' "$sources" | grep -v "^patches/$svc/2026\.1/[^/]*\.patch$") || true
    soft assert_eq "$svc patches lie in patches/$svc/2026.1/" "" "$other"
  done
}

@test "the committed pyproject.toml files carry the release inputs" {
  local keystone="$LOCKS/keystone/pyproject.toml" rev

  rev=$(sed -n 's/.*"rev": "\(.*\)".*/\1/p' "$LOCKS/keystone/sources.json")

  assert_file_matches "keystone carries its ldap extra" "$keystone" '"keystone\[ldap\]",'
  assert_file_matches "uwsgi keeps its venv-builder pin" "$keystone" '"uwsgi==2.0.31",'
  assert_file_matches "the git source names the commit of sources.json" \
    "$keystone" "rev = \"$rev\""
  assert_file_matches "the lhafile override is a constraint" "$keystone" '"lhafile==0.3.1",'
  assert_file_not_matches "the horizon pin is removed by the overrides" "$keystone" '"horizon=='
  assert_file_matches "cryptography is constrained, not pinned twice" "$keystone" '^    "cryptography",$'
  assert_file_matches "the test group carries keystone's test-requirements" \
    "$keystone" '^    "oslotest>=3.2.0",$'
  assert_file_matches "oslo.db keeps its extras in the test group" \
    "$keystone" '^    "oslo.db\[fixtures,mysql,postgresql\]>=6.0.0",$'
  assert_file_matches "the SPDX licence comment is written" "$keystone" '^# SPDX-License-Identifier: Apache-2.0$'
  assert_file_matches "glance carries glance_store[s3]" \
    "$LOCKS/glance/pyproject.toml" '"glance_store\[s3\]",'
  assert_file_matches "nova is listed without extras" "$LOCKS/nova/pyproject.toml" '^    "nova",$'
}

@test "generate-lock.sh is shellcheck-clean at --severity=warning" {
  require_tool shellcheck

  local output exit_code=0
  output=$(shellcheck --severity=warning "$GENERATOR" 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "$output"
  assert_eq "shellcheck exits 0" "0" "$exit_code"
}
