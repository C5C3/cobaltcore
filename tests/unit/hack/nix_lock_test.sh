#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/nix-lock.sh refuses bad input before it writes anything, writes
# the release inputs into pyproject.toml and sources.json, keeps the previous
# lock when a step fails, writes the noVNC hash, deletes the locks nothing
# pins, reports stale locks, resolves for the interpreter flake.nix builds
# the images with, and that the committed locks under nix/locks/ are the set
# the release files pin.
#
# The generator checks the release directory first and the six tools next,
# so those failures need no tool at all. The other runs use a throwaway git
# repository with fixture release files for three services and stub commands
# on PATH: nix answers each prefetch with a fixture upstream tree and logs its
# arguments, uv logs whether a uv.lock was there when it ran and writes a
# placeholder uv.lock. They need yq, jq, python3 and git and never reach the
# network. Every run uses the bash that runs this file. The generator needs
# Bash 4 or later past the tool check, so those runs skip under an older bash.
#
# Usage: bash tests/unit/hack/nix_lock_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

GENERATOR="$PROJECT_ROOT/hack/nix-lock.sh"
LOCKS="$PROJECT_ROOT/nix/locks"
FIXTURE_REV=0123456789abcdef0123456789abcdef01234567
NOVNC_REV=63107bd06d9e1f6136ff21aeda8cd62cbf0d433e

# can_run prints a SKIP line and returns 1 unless yq, jq, python3 and git are
# installed and the bash is 4 or later; $1 is the number of skipped checks.
can_run() {
  local tool
  for tool in yq jq python3 git; do
    if ! command -v "$tool" >/dev/null 2>&1; then
      echo "  SKIP: $tool not installed ($1 checks skipped)"
      SKIP=$((SKIP + $1))
      return 1
    fi
  done
  if [ "${BASH_VERSINFO[0]}" -lt 4 ]; then
    echo "  SKIP: bash $BASH_VERSION has no associative arrays ($1 checks skipped)"
    SKIP=$((SKIP + $1))
    return 1
  fi
}

# make_tree prints the path of a throwaway git repository with the
# generator, scripts/apply-constraint-overrides.sh, release files for the
# services placement (a setup.cfg naming openstack-placement, a
# .stestr.conf, a test-requirements.txt and a patch), horizon (no
# .stestr.conf) and barbican (no test-requirements.txt, a pyproject.toml
# naming openstack-barbican), the tempest pins, the noVNC pin, and stub nix
# and uv commands in stubs/. The constraints spell PyMySQL as PyPI does, the
# venv-builder pin as pymysql. Each run logs to stubs/nix.log and
# stubs/uv.log; NIX_STUB_FAIL makes every nix call exit 9.
make_tree() {
  local tree
  tree=$(mktemp -d)
  mkdir -p "$tree/hack" "$tree/scripts" "$tree/releases/2026.1" "$tree/overrides/2026.1" \
    "$tree/images/venv-builder" "$tree/images/nova" "$tree/patches/placement/2026.1" \
    "$tree/upstream/placement" "$tree/upstream/horizon" "$tree/upstream/barbican" "$tree/stubs"
  cp "$GENERATOR" "$tree/hack/"
  cp "$PROJECT_ROOT/scripts/apply-constraint-overrides.sh" "$tree/scripts/"

  printf '%s\n' 'placement: "15.0.0"' 'horizon: "25.7.0"' 'barbican: "22.0.0"' \
    >"$tree/releases/2026.1/source-refs.yaml"
  printf '%s\n' 'tempest: "46.3.0"' 'barbican-tempest-plugin: "4.6.0"' \
    'keystone-tempest-plugin: "0.22.0"' 'neutron-tempest-plugin: "3.2.0"' \
    'cinder-tempest-plugin: "1.23.0"' >"$tree/releases/2026.1/test-refs.yaml"
  printf '%s\n' 'placement:' '  pip_extras: [ldap]' '  pip_packages: [lhafile]' \
    >"$tree/releases/2026.1/extra-packages.yaml"
  printf '%s\n' 'oslo.db===18.0.0' 'cryptography===43.0.3' 'horizon===25.7.0' 'PyMySQL===1.1.2' \
    >"$tree/releases/2026.1/upper-constraints.txt"
  echo '-horizon' >"$tree/overrides/2026.1/constraints.txt"
  printf '%s\n' 'cryptography==48.0.1' 'pymysql==1.2.3' 'uwsgi==2.0.31' \
    >"$tree/images/venv-builder/requirements.txt"
  printf '{\n  "version": "v1.7.0",\n  "rev": "%s"\n}\n' "$NOVNC_REV" >"$tree/images/nova/novnc.json"
  printf '%s\n' '--- a/placement/api.py' '+++ b/placement/api.py' \
    >"$tree/patches/placement/2026.1/0001-fix.patch"

  printf '%s\n' '[metadata]' 'name = openstack-placement' >"$tree/upstream/placement/setup.cfg"
  printf '%s\n' '[DEFAULT]' 'test_path=./placement/tests' >"$tree/upstream/placement/.stestr.conf"
  printf '%s\n' '# Comment line' 'stestr>=1.0.0 # Apache-2.0' '' '-r requirements.txt' \
    '  oslotest>=3.2.0  ' >"$tree/upstream/placement/test-requirements.txt"
  printf '%s\n' '[project]' 'name = "horizon"' >"$tree/upstream/horizon/pyproject.toml"
  printf '%s\n' 'pytest>=5.3.5' >"$tree/upstream/horizon/test-requirements.txt"
  printf '%s\n' '[DEFAULT]' 'test_path=./barbican/tests' >"$tree/upstream/barbican/.stestr.conf"
  printf '%s\n' '[project]' 'name = "openstack-barbican"' >"$tree/upstream/barbican/pyproject.toml"

  cat >"$tree/stubs/nix" <<EOF
#!/bin/sh
echo "\$*" >>"$tree/stubs/nix.log"
[ -z "\${NIX_STUB_FAIL:-}" ] || exit 9
for ref; do :; done
case "\$ref" in
  github:novnc/*) echo '{"locked":{"rev":"$NOVNC_REV"},"hash":"sha256-novnc","storePath":"/nonexistent"}' ;;
  github:openstack/*)
    repo=\${ref#github:openstack/}
    echo "{\"locked\":{\"rev\":\"$FIXTURE_REV\"},\"hash\":\"sha256-fixture\",\"storePath\":\"$tree/upstream/\${repo%%/*}\"}"
    ;;
  *) exit 9 ;;
esac
EOF
  cat >"$tree/stubs/uv" <<EOF
#!/bin/sh
if [ -f uv.lock ]; then echo "seeded: \$(cat uv.lock)" >>"$tree/stubs/uv.log"; else echo unseeded >>"$tree/stubs/uv.log"; fi
[ -z "\${UV_STUB_FAIL:-}" ] || exit 4
echo "# stub lock" >uv.lock
EOF
  chmod +x "$tree/stubs/nix" "$tree/stubs/uv"
  printf '%s\n' 'stubs/' 'upstream/' >"$tree/.gitignore"
  git -C "$tree" init --quiet
  git -C "$tree" add --all
  git -C "$tree" -c user.name=test -c user.email=test@localhost commit --quiet --message fixture
  echo "$tree"
}

# run_generator runs the generator of a tree with its stubs first on PATH.
run_generator() {
  local tree="$1"
  shift
  env PATH="$tree/stubs:$PATH" "$BASH" "$tree/hack/nix-lock.sh" "$@"
}

# lock_state prints a checksum of the names and contents of a lock directory.
lock_state() {
  (cd "$1" && ls -A && cat -- *) | cksum
}

# seed_lock writes a previous lock for placement into the tree.
seed_lock() {
  local dest="$1/nix/locks/2026.1/placement" file
  mkdir -p "$dest"
  for file in pyproject.toml uv.lock sources.json; do
    echo "previous $file" >"$dest/$file"
  done
}

test_usage_and_missing_release() {
  echo "Test: a wrong argument list and an unknown release exit 1 before any tool runs"
  local output exit_code=0

  output=$("$BASH" "$GENERATOR" keystone 2>&1) || exit_code=$?
  assert_eq "one service argument exits 1" "1" "$exit_code"
  assert_contains "the usage line is printed" "$output" "Usage:"

  exit_code=0
  output=$("$BASH" "$GENERATOR" --all extra 2>&1) || exit_code=$?
  assert_eq "--all with an argument exits 1" "1" "$exit_code"

  exit_code=0
  output=$(env PATH=/nonexistent "$BASH" "$GENERATOR" keystone 2027.9 2>&1) || exit_code=$?
  assert_eq "release 2027.9 exits 1" "1" "$exit_code"
  assert_eq "the message names the missing source-refs.yaml" \
    "nix-lock.sh: releases/2027.9/source-refs.yaml not found: is '2027.9' a release directory?" "$output"
}

test_missing_tools_are_named_and_locks_untouched() {
  echo "Test: tools missing from PATH are named and the committed locks stay as they are"
  local before after output exit_code=0

  before=$(cat "$LOCKS"/*/*/* | cksum)
  output=$(env PATH=/nonexistent "$BASH" "$GENERATOR" keystone 2026.1 2>&1) || exit_code=$?
  after=$(cat "$LOCKS"/*/*/* | cksum)

  assert_eq "an empty PATH exits 1" "1" "$exit_code"
  assert_eq "the message names all six tools" \
    "nix-lock.sh: required tool(s) not on PATH: uv nix yq jq python3 git" "$output"
  assert_eq "the committed lock files are unchanged" "$before" "$after"
}

test_unknown_keys_are_named() {
  echo "Test: a name without a source-refs.yaml key and a missing tempest pin exit 1"
  can_run 6 || return

  local tree output exit_code=0
  tree=$(make_tree)
  output=$(run_generator "$tree" swift 2026.1 2>&1) || exit_code=$?
  assert_eq "swift exits 1" "1" "$exit_code"
  assert_contains "the message names the key and the file" \
    "$output" "nix-lock.sh: no key 'swift' in releases/2026.1/source-refs.yaml"
  assert_eq "nix was not called" "" "$(cat "$tree/stubs/nix.log" 2>/dev/null)"

  sed -i.bak '/^cinder-tempest-plugin:/d' "$tree/releases/2026.1/test-refs.yaml"
  exit_code=0
  output=$(run_generator "$tree" tempest 2026.1 2>&1) || exit_code=$?
  assert_eq "tempest without the cinder plugin pin exits 1" "1" "$exit_code"
  assert_contains "the message names the plugin and test-refs.yaml" \
    "$output" "nix-lock.sh: no key 'cinder-tempest-plugin' in releases/2026.1/test-refs.yaml"
  assert_eq "no tempest lock was written" "false" \
    "$([ -e "$tree/nix/locks/2026.1/tempest" ] && echo true || echo false)"
  rm -rf "$tree"
}

test_service_lock_carries_the_release_inputs() {
  echo "Test: a service lock carries the project name, the release inputs and the seed"
  can_run 19 || return

  local tree out pyproject output exit_code=0
  tree=$(make_tree)
  out="$tree/nix/locks/2026.1/placement"
  pyproject="$out/pyproject.toml"
  seed_lock "$tree"

  output=$(run_generator "$tree" placement 2026.1 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "$output"
  assert_eq "the run exits 0" "0" "$exit_code"

  assert_file_contains "the header names the generator and make nix-lock" "$pyproject" \
    '^# Generated by hack/nix-lock.sh placement 2026.1; regenerate with make nix-lock, do not edit.$'
  assert_file_contains "the project name from setup.cfg carries the extras" \
    "$pyproject" '^    "openstack-placement\[ldap\]",$'
  assert_file_contains_fixed "the git source is keyed by the project name" "$pyproject" \
    "openstack-placement = { git = \"https://github.com/openstack/placement\", rev = \"$FIXTURE_REV\" }"
  assert_file_contains "the pip_packages are dependencies" "$pyproject" '^    "lhafile",$'
  assert_file_contains "a venv-builder package the constraints pin is listed by name" \
    "$pyproject" '^    "cryptography",$'
  assert_file_contains "a venv-builder pin matches its constraint case-insensitively" \
    "$pyproject" '^    "pymysql",$'
  assert_file_not_contains "the venv-builder version of a case-folded name is dropped" \
    "$pyproject" '"pymysql==1.2.3"'
  assert_file_contains "a venv-builder package the constraints lack keeps its pin" \
    "$pyproject" '^    "uwsgi==2.0.31",$'
  assert_file_contains "a === constraint is written with ==" "$pyproject" '^    "oslo.db==18.0.0",$'
  assert_file_not_contains "a constraint the overrides remove is left out" "$pyproject" '"horizon=='
  assert_file_contains "stestr keeps the range test-requirements.txt gives it" \
    "$pyproject" '^    "stestr>=1.0.0",$'
  assert_file_not_contains "stestr is not listed a second time" "$pyproject" '^    "stestr",$'
  assert_file_contains "testtools, which test-requirements.txt lacks, is added" \
    "$pyproject" '^    "testtools",$'
  assert_file_contains "surrounding whitespace is trimmed from a test requirement" \
    "$pyproject" '^    "oslotest>=3.2.0",$'
  assert_file_not_contains "a pip option is no test requirement" "$pyproject" '"-r requirements.txt"'
  assert_eq "sources.json carries the ref, commit, hash, project and patch" \
    "{\"ref\":\"15.0.0\",\"rev\":\"$FIXTURE_REV\",\"hash\":\"sha256-fixture\",\"project\":\"openstack-placement\",\"patches\":[\"patches/placement/2026.1/0001-fix.patch\"]}" \
    "$(jq -c . "$out/sources.json")"
  assert_eq "uv ran on the committed uv.lock" "seeded: previous uv.lock" "$(cat "$tree/stubs/uv.log")"
  assert_eq "uv.lock is the file uv lock wrote" "# stub lock" "$(cat "$out/uv.lock")"
  rm -rf "$tree"
}

test_test_groups() {
  echo "Test: a suite without .stestr.conf gets pytest, one without test-requirements.txt the stestr pair"
  can_run 9 || return

  local tree pyproject exit_code=0
  tree=$(make_tree)
  run_generator "$tree" horizon 2026.1 >/dev/null 2>&1 || exit_code=$?
  run_generator "$tree" barbican 2026.1 >/dev/null 2>&1 || exit_code=$?
  assert_eq "both runs exit 0" "0" "$exit_code"

  pyproject="$tree/nix/locks/2026.1/horizon/pyproject.toml"
  assert_file_contains "horizon gets the hacking pin of its tox py3 env" \
    "$pyproject" '^    "hacking>=7.0.0,<7.1.0",$'
  assert_file_contains "horizon keeps the pytest range of test-requirements.txt" \
    "$pyproject" '^    "pytest>=5.3.5",$'
  assert_file_not_contains "horizon gets no stestr" "$pyproject" '"stestr'

  pyproject="$tree/nix/locks/2026.1/barbican/pyproject.toml"
  assert_eq "barbican's test group is stestr and testtools alone" '"stestr", "testtools",' \
    "$(sed -n '/^test = \[$/,/^\]$/p' "$pyproject" | sed -e '1d' -e '$d' | tr -d '\n' | tr -s ' ' | sed 's/^ //')"
  assert_eq "a service without patches gets an empty list" "[]" \
    "$(jq -c .patches "$tree/nix/locks/2026.1/barbican/sources.json")"
  assert_eq "the pyproject.toml name is the project" "openstack-barbican" \
    "$(jq -r .project "$tree/nix/locks/2026.1/barbican/sources.json")"
  assert_file_contains_fixed "the git source is keyed by the pyproject.toml name" "$pyproject" \
    "openstack-barbican = { git = \"https://github.com/openstack/barbican\", rev = \"$FIXTURE_REV\" }"
  assert_eq "the uv stub ran unseeded for a new lock" "unseeded" "$(tail -n 1 "$tree/stubs/uv.log")"
  rm -rf "$tree"
}

test_tempest_lock() {
  echo "Test: the tempest lock pins the five test-refs.yaml keys and fetches nothing"
  can_run 9 || return

  local tree out pyproject output exit_code=0
  tree=$(make_tree)
  out="$tree/nix/locks/2026.1/tempest"
  pyproject="$out/pyproject.toml"

  output=$(run_generator "$tree" tempest 2026.1 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "$output"
  assert_eq "the run exits 0" "0" "$exit_code"
  assert_file_contains "tempest is pinned" "$pyproject" '^    "tempest==46.3.0",$'
  assert_file_contains "the barbican plugin is pinned" "$pyproject" '^    "barbican-tempest-plugin==4.6.0",$'
  assert_file_contains "the cinder plugin is pinned" "$pyproject" '^    "cinder-tempest-plugin==1.23.0",$'
  assert_file_contains "the openstack CLI is a dependency" "$pyproject" '^    "python-openstackclient",$'
  assert_file_not_contains "tempest has no test group" "$pyproject" '^\[dependency-groups\]$'
  assert_file_not_contains "tempest has no git source" "$pyproject" '^\[tool.uv.sources\]$'
  assert_eq "sources.json holds the version and the project" '{"ref":"46.3.0","project":"tempest"}' \
    "$(jq -c . "$out/sources.json")"
  assert_eq "nix was not called" "" "$(cat "$tree/stubs/nix.log" 2>/dev/null)"
  rm -rf "$tree"
}

test_all_writes_novnc_and_deletes_orphans() {
  echo "Test: --all writes every lock and the noVNC hash and deletes the locks nothing pins"
  can_run 7 || return

  local tree output exit_code=0
  tree=$(make_tree)
  mkdir -p "$tree/nix/locks/2025.2/keystone" "$tree/nix/locks/2026.1/swift"
  touch "$tree/nix/locks/2025.2/keystone/uv.lock" "$tree/nix/locks/2026.1/swift/uv.lock"

  output=$(run_generator "$tree" --all 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "$output"
  assert_eq "the run exits 0" "0" "$exit_code"
  assert_eq "one lock per source-refs key plus tempest" "barbican horizon placement tempest" \
    "$(cd "$tree/nix/locks/2026.1" && echo *)"
  assert_eq "a release nothing pins is deleted" "false" \
    "$([ -e "$tree/nix/locks/2025.2" ] && echo true || echo false)"
  assert_contains "the deletion of a name nothing pins is reported" "$output" \
    "nix-lock.sh: deleted nix/locks/2026.1/swift"
  assert_eq "novnc.json keeps version and rev and gains the hash, in that order" \
    "[\"version\",\"rev\",\"hash\"]" "$(jq -c 'keys_unsorted' "$tree/images/nova/novnc.json")"
  assert_eq "the hash is the prefetched one" "sha256-novnc" "$(jq -r .hash "$tree/images/nova/novnc.json")"
  assert_contains "the noVNC prefetch names the commit" "$(cat "$tree/stubs/nix.log")" \
    "github:novnc/noVNC/$NOVNC_REV"
  rm -rf "$tree"
}

test_check_reports_stale_locks() {
  echo "Test: --check exits 0 on current locks and 1 with the stale message after a pin moved"
  can_run 5 || return

  local tree output exit_code=0
  tree=$(make_tree)
  run_generator "$tree" --all >/dev/null 2>&1
  git -C "$tree" add --all
  git -C "$tree" -c user.name=test -c user.email=test@localhost commit --quiet --message locks

  output=$(run_generator "$tree" --check 2>&1) || exit_code=$?
  assert_eq "a current tree exits 0" "0" "$exit_code"

  sed -i.bak 's/^placement: .*/placement: "15.0.1"/' "$tree/releases/2026.1/source-refs.yaml"
  rm -f "$tree/releases/2026.1/source-refs.yaml.bak"
  exit_code=0
  output=$(run_generator "$tree" --check 2>&1) || exit_code=$?
  assert_eq "a moved pin exits 1" "1" "$exit_code"
  assert_contains "the stale sources.json is listed" "$output" "nix/locks/2026.1/placement/sources.json"
  assert_contains "the stale message names make nix-lock" "$output" \
    "nix-lock.sh: the locks are stale; run make nix-lock and commit the result"
  assert_not_contains "an unchanged lock is not listed" "$output" " M nix/locks/2026.1/barbican"
  rm -rf "$tree"
}

test_failures_leave_the_lock_untouched() {
  echo "Test: a refused patch, a failing prefetch and a failing uv lock leave the previous lock as it was"
  can_run 9 || return

  local tree dest before output exit_code=0
  tree=$(make_tree)
  dest="$tree/nix/locks/2026.1/placement"
  seed_lock "$tree"
  before=$(lock_state "$dest")

  printf '%s\n' '--- a/test-requirements.txt' '+++ b/test-requirements.txt' '@@ -1 +1,2 @@' \
    ' stestr>=1.0.0' '+fixtures>=3.0.0' >"$tree/patches/placement/2026.1/0002-test-requirements.patch"
  output=$(run_generator "$tree" placement 2026.1 2>&1) || exit_code=$?
  assert_eq "a requirement-editing patch exits 1" "1" "$exit_code"
  assert_contains "the message names the patch" "$output" \
    "patches/placement/2026.1/0002-test-requirements.patch edits the requirements"
  assert_eq "nix was not called" "" "$(cat "$tree/stubs/nix.log" 2>/dev/null)"
  assert_eq "the lock is unchanged after the refusal" "$before" "$(lock_state "$dest")"

  rm "$tree/patches/placement/2026.1/0002-test-requirements.patch"
  exit_code=0
  NIX_STUB_FAIL=1 run_generator "$tree" placement 2026.1 >/dev/null 2>&1 || exit_code=$?
  assert_eq "a failing prefetch exits with nix's status" "9" "$exit_code"
  assert_eq "uv did not run after a failed prefetch" "" "$(cat "$tree/stubs/uv.log" 2>/dev/null)"
  assert_eq "the lock is unchanged after a failed prefetch" "$before" "$(lock_state "$dest")"

  exit_code=0
  UV_STUB_FAIL=1 run_generator "$tree" placement 2026.1 >/dev/null 2>&1 || exit_code=$?
  assert_eq "a failing uv lock exits with uv's status" "4" "$exit_code"
  assert_eq "the lock is unchanged after a failed uv lock" "$before" "$(lock_state "$dest")"
  rm -rf "$tree"
}

test_committed_locks_match_the_pins() {
  echo "Test: nix/locks holds one lock per source-refs key and release plus tempest"
  if ! command -v yq >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: yq or jq not installed"
    SKIP=$((SKIP + 1))
    return
  fi

  local refs release want have name dir file
  for refs in "$PROJECT_ROOT"/releases/*/source-refs.yaml; do
    release=$(basename "$(dirname "$refs")")
    want=$( (yq -r 'keys | .[]' "$refs"; echo tempest) | sort | tr '\n' ' ')
    have=$(cd "$LOCKS/$release" 2>/dev/null && for dir in */; do echo "${dir%/}"; done | sort | tr '\n' ' ')
    assert_eq "nix/locks/$release holds the pinned names" "$want" "$have"
    for name in $want; do
      dir="$LOCKS/$release/$name"
      for file in pyproject.toml uv.lock sources.json; do
        [ -s "$dir/$file" ] || {
          echo "  FAIL: $dir/$file is missing or empty"
          FAIL=$((FAIL + 1))
          continue
        }
      done
      assert_file_contains "$release/$name/pyproject.toml names the generator" "$dir/pyproject.toml" \
        "^# Generated by hack/nix-lock.sh $name $release; regenerate with make nix-lock, do not edit.$"
      if [ "$name" = tempest ]; then
        assert_eq "$release/tempest/sources.json holds ref and project" '["ref","project"]' \
          "$(jq -c keys_unsorted "$dir/sources.json")"
      else
        assert_eq "$release/$name/sources.json holds the five keys" '["ref","rev","hash","project","patches"]' \
          "$(jq -c keys_unsorted "$dir/sources.json")"
      fi
    done
  done
  assert_eq "placement's project is openstack-placement" "openstack-placement" \
    "$(jq -r .project "$LOCKS/2026.1/placement/sources.json")"
}

test_python_is_the_flake_interpreter() {
  echo "Test: the locks resolve for the interpreter flake.nix builds the images with"

  local flake_minor generator_minor pyproject
  flake_minor=$(sed -n 's/^ *pythonFor = pkgs: pkgs\.python3\([0-9][0-9]*\);$/3.\1/p' "$PROJECT_ROOT/flake.nix")
  generator_minor=$(sed -n 's/^PYTHON_MINOR=//p' "$GENERATOR")
  assert_not_empty "flake.nix names the interpreter of the images" "$flake_minor"
  assert_eq "PYTHON_MINOR is that interpreter" "$flake_minor" "$generator_minor"
  for pyproject in "$LOCKS"/*/*/pyproject.toml; do
    assert_file_contains_fixed "${pyproject#"$LOCKS"/} requires that interpreter" "$pyproject" \
      "requires-python = \"==${flake_minor}.*\""
  done
}

test_generator_is_shellcheck_clean() {
  echo "Test: nix-lock.sh is shellcheck-clean at --severity=warning"

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
test_unknown_keys_are_named
test_service_lock_carries_the_release_inputs
test_test_groups
test_tempest_lock
test_all_writes_novnc_and_deletes_orphans
test_check_reports_stale_locks
test_failures_leave_the_lock_untouched
test_committed_locks_match_the_pins
test_python_is_the_flake_interpreter
test_generator_is_shellcheck_clean

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
