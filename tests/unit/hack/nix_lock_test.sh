#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/nix-lock.sh refuses bad input before it writes anything, writes
# the release inputs into pyproject.toml and sources.json, writes the
# nova-compute group into the nova lock and refuses a release without its
# block, keeps the previous lock when a step fails, writes the noVNC hash and
# the four image pins,
# deletes the locks nothing pins, reports stale locks and pins, resolves for
# the interpreter flake.nix builds the images with, and that the committed
# locks under nix/locks/ are the set the release files pin, the committed nova
# locks carry the nova-compute group, and the committed image pins carry
# commits and hashes.
#
# The generator checks the release directory first and the six tools next,
# so those failures need no tool at all. The other runs use a throwaway git
# repository with fixture release files for three services, the image pins,
# a local OVN repository whose tree has an 'ovs' gitlink, and stub commands
# on PATH: nix answers each prefetch with a fixture upstream tree, fails each
# build with the vendor hash it "got" and logs its arguments, uv logs whether
# a uv.lock was there when it ran and writes a placeholder uv.lock. They need
# yq, jq, python3 and git and never reach the network. Every run uses the
# bash that runs this file. The generator needs Bash 4 or later past the
# tool check, so those runs skip under an older bash.
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
OVS_REV=fedcba9876543210fedcba9876543210fedcba98
HVO_REV=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
KNA_REV=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
PROXY_REV=cccccccccccccccccccccccccccccccccccccccc
OVN_PIN=images/ovn/source.json
HVO_PIN=images/openstack-hypervisor-operator/source.json
KNA_PIN=images/kvm-node-agent/source.json
PROXY_PIN=images/keystone-federation-proxy/source.json

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
# naming openstack-barbican), the tempest pins, the noVNC pin, the four
# image pins, a local OVN repository in upstream/ovn whose tree has the 'ovs'
# gitlink $OVS_REV, and stub nix and uv commands in stubs/. The OVN pin names
# that repository's commit and has no ovs object yet; the hvo pin's hash is
# the one the stub prefetches, the kna pin's is not; the proxy pin has no
# hash yet. The constraints spell
# PyMySQL as PyPI does, the base pin of nix/base-requirements.txt as pymysql.
# Each run logs to
# stubs/nix.log and stubs/uv.log; NIX_STUB_FAIL makes every nix call exit 9,
# NIX_STUB_FAIL_REF every nix call whose arguments contain its value,
# NIX_STUB_NO_GOT keeps a failing nix build from naming a hash.
make_tree() {
  local tree ovn
  tree=$(mktemp -d)
  ovn="$tree/upstream/ovn"
  mkdir -p "$tree/hack" "$tree/scripts" "$tree/releases/2026.1" "$tree/overrides/2026.1" \
    "$tree/nix" "$tree/images/nova" "$tree/patches/placement/2026.1" \
    "$tree/images/ovn" "$tree/images/openstack-hypervisor-operator" "$tree/images/kvm-node-agent" \
    "$tree/images/keystone-federation-proxy" \
    "$tree/upstream/placement" "$tree/upstream/horizon" "$tree/upstream/barbican" "$ovn" "$tree/stubs"
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
    >"$tree/nix/base-requirements.txt"
  printf '{\n  "version": "v1.7.0",\n  "rev": "%s"\n}\n' "$NOVNC_REV" >"$tree/images/nova/novnc.json"

  git -C "$ovn" init --quiet
  git -C "$ovn" config uploadpack.allowFilter true
  git -C "$ovn" config uploadpack.allowAnySHA1InWant true
  echo ovn >"$ovn/README.rst"
  git -C "$ovn" add README.rst
  git -C "$ovn" update-index --add --cacheinfo "160000,$OVS_REV,ovs"
  git -C "$ovn" -c user.name=test -c user.email=test@localhost commit --quiet --message ovn
  jq -n --arg rev "$(git -C "$ovn" rev-parse HEAD)" '{version: "v26.03.2", rev: $rev, hash: "sha256-stale"}' \
    >"$tree/$OVN_PIN"
  jq -n --arg rev "$HVO_REV" '{rev: $rev, hash: "sha256-go", vendorHash: "sha256-kept"}' >"$tree/$HVO_PIN"
  jq -n --arg rev "$KNA_REV" '{rev: $rev, hash: "sha256-old", vendorHash: "sha256-old-vendor"}' \
    >"$tree/$KNA_PIN"
  jq -n --arg rev "$PROXY_REV" '{version: "v2.4.20.3", rev: $rev}' >"$tree/$PROXY_PIN"
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
[ -z "\${NIX_STUB_FAIL_REF:-}" ] || case "\$*" in *"\$NIX_STUB_FAIL_REF"*) exit 9 ;; esac
case " \$* " in
  *" build "*)
    [ -n "\${NIX_STUB_NO_GOT:-}" ] || echo "         got:    sha256-vendor" >&2
    exit 1
    ;;
esac
for ref; do :; done
case "\$ref" in
  github:novnc/*) echo '{"locked":{"rev":"$NOVNC_REV"},"hash":"sha256-novnc","storePath":"/nonexistent"}' ;;
  github:ovn-org/*) echo '{"hash":"sha256-ovn","storePath":"/nonexistent"}' ;;
  github:openvswitch/*) echo '{"hash":"sha256-ovs","storePath":"/nonexistent"}' ;;
  github:cobaltcore-dev/*) echo '{"hash":"sha256-go","storePath":"/nonexistent"}' ;;
  github:OpenIDC/*) echo '{"hash":"sha256-openidc","storePath":"/nonexistent"}' ;;
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

# run_generator runs the generator of a tree with its stubs first on PATH and
# the tree's OVN repository as the remote of the gitlink fetch.
run_generator() {
  local tree="$1"
  shift
  env PATH="$tree/stubs:$PATH" NIX_LOCK_OVN_REMOTE="file://$tree/upstream/ovn" \
    "$BASH" "$tree/hack/nix-lock.sh" "$@"
}

# matches prints true when $1 matches the extended regular expression $2.
matches() {
  if [[ "$1" =~ $2 ]]; then echo true; else echo false; fi
}

# pin_state prints a checksum of the four image pins of a tree.
pin_state() {
  (cd "$1" && cat "$OVN_PIN" "$HVO_PIN" "$KNA_PIN" "$PROXY_PIN") | cksum
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

# add_nova adds the service nova to a tree: its source-refs.yaml key, an
# upstream tree with a setup.cfg and a .stestr.conf, and the
# extra-packages.yaml blocks of nova and of nova-compute, the image the
# generator derives from nova. The nova-compute block names an extra, so its
# group starts with the project.
add_nova() {
  local tree="$1"
  echo 'nova: "33.0.0"' >>"$tree/releases/2026.1/source-refs.yaml"
  mkdir -p "$tree/upstream/nova"
  printf '%s\n' '[metadata]' 'name = nova' >"$tree/upstream/nova/setup.cfg"
  printf '%s\n' '[DEFAULT]' 'test_path=./nova/tests' >"$tree/upstream/nova/.stestr.conf"
  printf '%s\n' 'nova:' '  pip_extras: []' '  pip_packages: [pyroute2]' \
    'nova-compute:' '  pip_extras: [zvm]' '  pip_packages: [libvirt-python, pyroute2]' \
    >>"$tree/releases/2026.1/extra-packages.yaml"
}

# group_names <pyproject.toml> prints the names of the dependency groups in
# file order, on one line.
group_names() {
  sed -n '/^\[dependency-groups\]$/,/^\[tool/p' "$1" | sed -n 's/^\([a-z-]*\) = \[$/\1/p' |
    tr '\n' ' ' | sed 's/ $//'
}

# group_items <pyproject.toml> <group> prints the items of one dependency
# group, one per line, without quotes and commas.
group_items() {
  sed -n "/^$2 = \[\$/,/^\]\$/p" "$1" | sed -e '1d' -e '$d' -e 's/^ *"//' -e 's/",$//'
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
  assert_file_contains "a base package the constraints pin is listed by name" \
    "$pyproject" '^    "cryptography",$'
  assert_file_contains "a base pin matches its constraint case-insensitively" \
    "$pyproject" '^    "pymysql",$'
  assert_file_not_contains "the base version of a case-folded name is dropped" \
    "$pyproject" '"pymysql==1.2.3"'
  assert_file_contains "a base package the constraints lack keeps its pin" \
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
  assert_file_not_contains "a service no image derives from has no nova-compute group" \
    "$pyproject" '^nova-compute = \[$'
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

test_all_writes_the_ovn_pin_in_key_order_with_the_gitlink_commit() {
  echo "Test: --all writes the OVN pin with the hash of each tree and the commit of the ovs gitlink"
  can_run 7 || return

  local tree output exit_code=0
  tree=$(make_tree)
  output=$(run_generator "$tree" --all 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "$output"
  assert_eq "the run exits 0" "0" "$exit_code"
  assert_eq "the pin holds version, rev, hash and ovs, in that order" '["version","rev","hash","ovs"]' \
    "$(jq -c keys_unsorted "$tree/$OVN_PIN")"
  assert_eq "ovs holds rev and hash, in that order" '["rev","hash"]' \
    "$(jq -c '.ovs | keys_unsorted' "$tree/$OVN_PIN")"
  assert_eq "ovs.rev is the commit of the gitlink" "$OVS_REV" "$(jq -r .ovs.rev "$tree/$OVN_PIN")"
  assert_eq "the hashes are the prefetched ones" "sha256-ovn sha256-ovs" \
    "$(jq -r '"\(.hash) \(.ovs.hash)"' "$tree/$OVN_PIN")"
  assert_contains "the OVS prefetch names the gitlink commit" "$(cat "$tree/stubs/nix.log")" \
    "github:openvswitch/ovs/$OVS_REV"
  rm -rf "$tree"
}

test_a_tree_without_the_ovs_gitlink_fails_and_leaves_the_pin() {
  echo "Test: a bad rev, an unfetchable commit and a tree without the ovs gitlink fail and leave the pins"
  can_run 9 || return

  local tree ovn before output exit_code=0
  tree=$(make_tree)
  ovn="$tree/upstream/ovn"
  git -C "$ovn" rm --cached --quiet ovs
  git -C "$ovn" -c user.name=test -c user.email=test@localhost commit --quiet --message "no gitlink"
  jq --arg rev "$(git -C "$ovn" rev-parse HEAD)" '.rev = $rev' "$tree/$OVN_PIN" >"$tree/pin.json"
  mv "$tree/pin.json" "$tree/$OVN_PIN"
  before=$(pin_state "$tree")

  output=$(run_generator "$tree" --all 2>&1) || exit_code=$?
  assert_eq "a commit without the gitlink exits 1" "1" "$exit_code"
  assert_contains "the message names the commit" "$output" \
    "nix-lock.sh: ovn-org/ovn at $(git -C "$ovn" rev-parse HEAD) has no ovs gitlink"
  assert_eq "the pins are unchanged" "$before" "$(pin_state "$tree")"

  jq '.rev = "0000000000000000000000000000000000000000"' "$tree/$OVN_PIN" >"$tree/pin.json"
  mv "$tree/pin.json" "$tree/$OVN_PIN"
  before=$(pin_state "$tree")
  exit_code=0
  output=$(run_generator "$tree" --all 2>&1) || exit_code=$?
  assert_nonzero_exit "a commit the remote lacks fails" "$exit_code"
  assert_contains "the message names the commit" "$output" \
    "nix-lock.sh: cannot fetch ovn-org/ovn at 0000000000000000000000000000000000000000"
  assert_eq "the pins are unchanged after the failed fetch" "$before" "$(pin_state "$tree")"

  jq '.rev = "v26.03.2"' "$tree/$OVN_PIN" >"$tree/pin.json"
  mv "$tree/pin.json" "$tree/$OVN_PIN"
  exit_code=0
  output=$(run_generator "$tree" --all 2>&1) || exit_code=$?
  assert_eq "a tag as rev exits 1" "1" "$exit_code"
  assert_contains "the message names the pin" "$output" \
    "nix-lock.sh: images/ovn/source.json: rev is not a 40-character commit"
  rm -rf "$tree"
}

test_an_unchanged_go_hash_keeps_the_vendor_hash_without_a_build() {
  echo "Test: a Go pin whose hash did not move keeps its vendorHash and builds nothing"
  can_run 3 || return

  local tree output exit_code=0
  tree=$(make_tree)
  output=$(run_generator "$tree" --all 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "$output"
  assert_eq "the run exits 0" "0" "$exit_code"
  assert_eq "the hvo pin is unchanged" \
    "{\"rev\":\"$HVO_REV\",\"hash\":\"sha256-go\",\"vendorHash\":\"sha256-kept\"}" \
    "$(jq -c . "$tree/$HVO_PIN")"
  assert_not_contains "no build ran for hvo" "$(grep ' build ' "$tree/stubs/nix.log")" \
    "images/openstack-hypervisor-operator/manager.nix"
  rm -rf "$tree"
}

test_a_moved_go_hash_writes_the_vendor_hash_nix_printed() {
  echo "Test: a Go pin whose hash moved gets the new hash and the vendor hash the build printed"
  can_run 4 || return

  local tree output exit_code=0
  tree=$(make_tree)
  output=$(run_generator "$tree" --all 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "$output"
  assert_eq "the run exits 0" "0" "$exit_code"
  assert_eq "the kna pin holds rev, the new hash and the printed vendor hash, in that order" \
    "{\"rev\":\"$KNA_REV\",\"hash\":\"sha256-go\",\"vendorHash\":\"sha256-vendor\"}" \
    "$(jq -c . "$tree/$KNA_PIN")"
  assert_contains "the build reads the goModules of the kna manager" \
    "$(grep ' build ' "$tree/stubs/nix.log")" "images/kvm-node-agent/manager.nix"
  assert_contains "the write is reported" "$output" "nix-lock.sh: wrote $KNA_PIN"
  rm -rf "$tree"
}

test_a_build_without_got_fails_and_leaves_the_pin() {
  echo "Test: a vendor-hash build that names no hash fails and leaves the pin"
  can_run 3 || return

  local tree before output exit_code=0
  tree=$(make_tree)
  before=$(cksum <"$tree/$KNA_PIN")
  output=$(NIX_STUB_NO_GOT=1 run_generator "$tree" --all 2>&1) || exit_code=$?
  assert_eq "the run exits 1" "1" "$exit_code"
  assert_contains "the message names the image" "$output" \
    "nix-lock.sh: kvm-node-agent: nix build printed no vendor hash"
  assert_eq "the kna pin is unchanged" "$before" "$(cksum <"$tree/$KNA_PIN")"
  rm -rf "$tree"
}

test_check_reports_an_edited_pin_hash() {
  echo "Test: --check reports a committed pin whose hash differs from the prefetched one"
  can_run 3 || return

  local tree output exit_code=0
  tree=$(make_tree)
  run_generator "$tree" --all >/dev/null 2>&1
  jq '.hash = "sha256-edited"' "$tree/$OVN_PIN" >"$tree/pin.json"
  mv "$tree/pin.json" "$tree/$OVN_PIN"
  git -C "$tree" add --all
  git -C "$tree" -c user.name=test -c user.email=test@localhost commit --quiet --message pins

  output=$(run_generator "$tree" --check 2>&1) || exit_code=$?
  assert_eq "an edited pin exits 1" "1" "$exit_code"
  assert_contains "the pin is listed" "$output" " M $OVN_PIN"
  assert_contains "the stale message names make nix-lock" "$output" \
    "nix-lock.sh: the locks are stale; run make nix-lock and commit the result"
  rm -rf "$tree"
}

test_all_writes_the_proxy_pin_in_key_order() {
  echo "Test: --all writes the hash of the mod_auth_openidc commit into the proxy pin"
  can_run 5 || return

  local tree output exit_code=0
  tree=$(make_tree)
  output=$(run_generator "$tree" --all 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "$output"
  assert_eq "the run exits 0" "0" "$exit_code"
  assert_eq "the pin holds version, rev and the added hash, in that order" '["version","rev","hash"]' \
    "$(jq -c keys_unsorted "$tree/$PROXY_PIN")"
  assert_eq "the hash is the prefetched one" "sha256-openidc" "$(jq -r .hash "$tree/$PROXY_PIN")"
  assert_eq "the version stays" "v2.4.20.3" "$(jq -r .version "$tree/$PROXY_PIN")"
  assert_contains "the prefetch names the commit" "$(cat "$tree/stubs/nix.log")" \
    "github:OpenIDC/mod_auth_openidc/$PROXY_REV"
  rm -rf "$tree"
}

test_a_tag_pin_without_a_commit_fails_and_leaves_the_pin() {
  echo "Test: an empty and a malformed rev of the proxy and noVNC pins fail before the prefetch and leave the pin"
  can_run 16 || return

  local tree entry pin ref rev before output exit_code
  for entry in "$PROXY_PIN github:OpenIDC/" "images/nova/novnc.json github:novnc/"; do
    pin=${entry% *} ref=${entry#* }
    for rev in "" abc; do
      tree=$(make_tree)
      jq --arg rev "$rev" '.rev = $rev' "$tree/$pin" >"$tree/pin.json"
      mv "$tree/pin.json" "$tree/$pin"
      before=$(cksum <"$tree/$pin")
      exit_code=0
      output=$(run_generator "$tree" --all 2>&1) || exit_code=$?
      assert_eq "$pin: rev '$rev' exits 1" "1" "$exit_code"
      assert_contains "$pin: rev '$rev' is named as no commit" "$output" \
        "nix-lock.sh: $pin: rev is not a 40-character commit"
      assert_not_contains "$pin: rev '$rev' prefetches nothing for the pin" "$(cat "$tree/stubs/nix.log")" \
        "$ref"
      assert_eq "$pin: rev '$rev' leaves the pin byte-identical" "$before" "$(cksum <"$tree/$pin")"
      rm -rf "$tree"
    done
  done
}

test_a_failing_proxy_prefetch_leaves_the_pin() {
  echo "Test: a failing mod_auth_openidc prefetch exits with nix's status and leaves the proxy pin"
  can_run 3 || return

  local tree before exit_code=0
  tree=$(make_tree)
  before=$(cksum <"$tree/$PROXY_PIN")
  NIX_STUB_FAIL_REF=github:OpenIDC/ run_generator "$tree" --all >/dev/null 2>&1 || exit_code=$?
  assert_contains "the run reaches the mod_auth_openidc prefetch" "$(cat "$tree/stubs/nix.log")" \
    "github:OpenIDC/mod_auth_openidc/$PROXY_REV"
  assert_eq "a failing prefetch exits with nix's status" "9" "$exit_code"
  assert_eq "the proxy pin is byte-identical" "$before" "$(cksum <"$tree/$PROXY_PIN")"
  rm -rf "$tree"
}

test_check_reports_an_edited_proxy_pin_hash() {
  echo "Test: --check reports a committed proxy pin whose hash differs from the prefetched one"
  can_run 2 || return

  local tree output exit_code=0
  tree=$(make_tree)
  run_generator "$tree" --all >/dev/null 2>&1
  git -C "$tree" add --all
  git -C "$tree" -c user.name=test -c user.email=test@localhost commit --quiet --message pins
  jq '.hash = "sha256-edited"' "$tree/$PROXY_PIN" >"$tree/pin.json"
  mv "$tree/pin.json" "$tree/$PROXY_PIN"
  git -C "$tree" add --all
  git -C "$tree" -c user.name=test -c user.email=test@localhost commit --quiet --message "edited hash"

  output=$(run_generator "$tree" --check 2>&1) || exit_code=$?
  assert_eq "an edited proxy pin exits 1" "1" "$exit_code"
  assert_contains "the pin is listed" "$output" " M $PROXY_PIN"
  rm -rf "$tree"
}

test_an_empty_vendor_hash_is_recomputed() {
  echo "Test: a Go pin with an unchanged hash and an empty vendorHash gets the vendor hash the build printed"
  can_run 3 || return

  local tree output exit_code=0
  tree=$(make_tree)
  jq '.vendorHash = ""' "$tree/$HVO_PIN" >"$tree/pin.json"
  mv "$tree/pin.json" "$tree/$HVO_PIN"
  output=$(run_generator "$tree" --all 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "$output"
  assert_eq "the run exits 0" "0" "$exit_code"
  assert_contains "the build reads the goModules of the hvo manager" \
    "$(grep ' build ' "$tree/stubs/nix.log")" "images/openstack-hypervisor-operator/manager.nix"
  assert_eq "the hvo pin holds the printed vendor hash" "sha256-vendor" \
    "$(jq -r .vendorHash "$tree/$HVO_PIN")"
  rm -rf "$tree"
}

test_check_reports_an_ovs_rev_that_is_not_the_gitlink() {
  echo "Test: --check reports an ovs.rev that differs from the gitlink at the pinned OVN commit"
  can_run 3 || return

  local tree output exit_code=0
  tree=$(make_tree)
  run_generator "$tree" --all >/dev/null 2>&1
  jq '.ovs.rev = "1111111111111111111111111111111111111111"' "$tree/$OVN_PIN" >"$tree/pin.json"
  mv "$tree/pin.json" "$tree/$OVN_PIN"
  git -C "$tree" add --all
  git -C "$tree" -c user.name=test -c user.email=test@localhost commit --quiet --message pins

  output=$(run_generator "$tree" --check 2>&1) || exit_code=$?
  assert_eq "a foreign ovs.rev exits 1" "1" "$exit_code"
  assert_contains "the pin is listed" "$output" " M $OVN_PIN"
  assert_eq "ovs.rev is the commit of the gitlink again" "$OVS_REV" "$(jq -r .ovs.rev "$tree/$OVN_PIN")"
  rm -rf "$tree"
}

test_nix_stub_fail_leaves_every_pin_byte_identical() {
  echo "Test: a failing nix leaves the four image pins byte-identical"
  can_run 2 || return

  local tree before exit_code=0
  tree=$(make_tree)
  before=$(pin_state "$tree")
  NIX_STUB_FAIL=1 run_generator "$tree" --all >/dev/null 2>&1 || exit_code=$?
  assert_eq "a failing nix exits with its status" "9" "$exit_code"
  assert_eq "the four pins are unchanged" "$before" "$(pin_state "$tree")"
  rm -rf "$tree"
}

test_committed_pins_carry_revs_and_sri_hashes() {
  echo "Test: the committed image pins carry 40-character commits and SRI hashes"
  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed"
    SKIP=$((SKIP + 1))
    return
  fi

  local commit='^[0-9a-f]{40}$' sri='^sha256-[A-Za-z0-9+/]{43}=$' pin value
  for value in "$(jq -r .rev "$PROJECT_ROOT/$OVN_PIN")" "$(jq -r .ovs.rev "$PROJECT_ROOT/$OVN_PIN")"; do
    assert_eq "$OVN_PIN carries the commit $value" true "$(matches "$value" "$commit")"
  done
  for value in "$(jq -r .hash "$PROJECT_ROOT/$OVN_PIN")" "$(jq -r .ovs.hash "$PROJECT_ROOT/$OVN_PIN")"; do
    assert_eq "$OVN_PIN carries the SRI hash $value" true "$(matches "$value" "$sri")"
  done
  assert_eq "$OVN_PIN names a vX.Y.Z tag" true \
    "$(matches "$(jq -r .version "$PROJECT_ROOT/$OVN_PIN")" '^v[0-9]+\.[0-9]+\.[0-9]+$')"
  for pin in "$HVO_PIN" "$KNA_PIN"; do
    assert_eq "$pin holds rev, hash and vendorHash, in that order" '["rev","hash","vendorHash"]' \
      "$(jq -c keys_unsorted "$PROJECT_ROOT/$pin")"
    assert_eq "$pin carries a commit" true "$(matches "$(jq -r .rev "$PROJECT_ROOT/$pin")" "$commit")"
    assert_eq "$pin carries an SRI hash" true "$(matches "$(jq -r .hash "$PROJECT_ROOT/$pin")" "$sri")"
    assert_eq "$pin carries an SRI vendor hash" true "$(matches "$(jq -r .vendorHash "$PROJECT_ROOT/$pin")" "$sri")"
  done
  assert_eq "$PROXY_PIN holds version, rev and hash, in that order" '["version","rev","hash"]' \
    "$(jq -c keys_unsorted "$PROJECT_ROOT/$PROXY_PIN")"
  assert_eq "$PROXY_PIN carries a commit" true "$(matches "$(jq -r .rev "$PROJECT_ROOT/$PROXY_PIN")" "$commit")"
  assert_eq "$PROXY_PIN carries an SRI hash" true "$(matches "$(jq -r .hash "$PROJECT_ROOT/$PROXY_PIN")" "$sri")"
  assert_eq "$PROXY_PIN names a vX.Y.Z or vX.Y.Z.W tag" true \
    "$(matches "$(jq -r .version "$PROJECT_ROOT/$PROXY_PIN")" '^v[0-9]+(\.[0-9]+){2,3}$')"
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

test_nova_lock_carries_the_nova_compute_group() {
  echo "Test: the nova lock carries the nova-compute block as a dependency group after the test group"
  can_run 8 || return

  local tree pyproject output exit_code=0
  tree=$(make_tree)
  add_nova "$tree"
  pyproject="$tree/nix/locks/2026.1/nova/pyproject.toml"

  output=$(run_generator "$tree" nova 2026.1 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "$output"
  assert_eq "the run exits 0" "0" "$exit_code"
  assert_eq "the groups are test, then nova-compute" "test nova-compute" "$(group_names "$pyproject")"
  assert_eq "the group is the project with the block's extras, then its pip packages" \
    "nova[zvm] libvirt-python pyroute2" "$(group_items "$pyproject" nova-compute | tr '\n' ' ' | sed 's/ $//')"
  assert_file_contains "nova's own pip packages stay default dependencies" "$pyproject" '^    "pyroute2",$'
  assert_eq "libvirt-python is no default dependency" "" \
    "$(sed -n '/^dependencies = \[$/,/^\]$/p' "$pyproject" | grep libvirt-python)"

  sed -i.bak 's/^  pip_extras: \[zvm\]$/  pip_extras: []/' "$tree/releases/2026.1/extra-packages.yaml"
  exit_code=0
  run_generator "$tree" nova 2026.1 >/dev/null 2>&1 || exit_code=$?
  assert_eq "a block without extras exits 0" "0" "$exit_code"
  assert_eq "a block without extras lists its pip packages alone" "libvirt-python pyroute2" \
    "$(group_items "$pyproject" nova-compute | tr '\n' ' ' | sed 's/ $//')"
  rm -rf "$tree"
}

test_a_missing_derived_block_fails_and_leaves_the_lock() {
  echo "Test: a release without the nova-compute block fails before nix runs and keeps the nova lock"
  can_run 5 || return

  local tree dest before output exit_code=0 file
  tree=$(make_tree)
  add_nova "$tree"
  dest="$tree/nix/locks/2026.1/nova"
  mkdir -p "$dest"
  for file in pyproject.toml uv.lock sources.json; do
    echo "previous $file" >"$dest/$file"
  done
  before=$(lock_state "$dest")
  sed -i.bak '/^nova-compute:/,$d' "$tree/releases/2026.1/extra-packages.yaml"

  output=$(run_generator "$tree" nova 2026.1 2>&1) || exit_code=$?
  assert_eq "the run exits 1" "1" "$exit_code"
  assert_eq "the message names the block and the file" \
    "nix-lock.sh: no block 'nova-compute' in releases/2026.1/extra-packages.yaml" "$output"
  assert_eq "nix was not called" "" "$(cat "$tree/stubs/nix.log" 2>/dev/null)"
  assert_eq "uv was not called" "" "$(cat "$tree/stubs/uv.log" 2>/dev/null)"
  assert_eq "the nova lock is unchanged" "$before" "$(lock_state "$dest")"
  rm -rf "$tree"
}

test_committed_nova_locks_carry_the_group() {
  echo "Test: each committed nova lock carries the nova-compute group with libvirt-python after test"

  local pyproject release found=0
  for pyproject in "$LOCKS"/*/nova/pyproject.toml; do
    [ -f "$pyproject" ] || continue
    found=$((found + 1))
    release=$(basename "$(dirname "$(dirname "$pyproject")")")
    assert_eq "$release: the groups are test, then nova-compute" "test nova-compute" \
      "$(group_names "$pyproject")"
    assert_contains "$release: the nova-compute group names libvirt-python" \
      "$(group_items "$pyproject" nova-compute)" "libvirt-python"
  done
  assert_eq "nix/locks holds a nova lock for each release" \
    "$(find "$PROJECT_ROOT/releases" -name source-refs.yaml | wc -l | tr -d ' ')" "$found"
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
test_all_writes_the_ovn_pin_in_key_order_with_the_gitlink_commit
test_a_tree_without_the_ovs_gitlink_fails_and_leaves_the_pin
test_an_unchanged_go_hash_keeps_the_vendor_hash_without_a_build
test_a_moved_go_hash_writes_the_vendor_hash_nix_printed
test_a_build_without_got_fails_and_leaves_the_pin
test_check_reports_an_edited_pin_hash
test_all_writes_the_proxy_pin_in_key_order
test_a_tag_pin_without_a_commit_fails_and_leaves_the_pin
test_a_failing_proxy_prefetch_leaves_the_pin
test_check_reports_an_edited_proxy_pin_hash
test_an_empty_vendor_hash_is_recomputed
test_check_reports_an_ovs_rev_that_is_not_the_gitlink
test_nix_stub_fail_leaves_every_pin_byte_identical
test_failures_leave_the_lock_untouched
test_nova_lock_carries_the_nova_compute_group
test_a_missing_derived_block_fails_and_leaves_the_lock
test_committed_nova_locks_carry_the_group
test_committed_locks_match_the_pins
test_committed_pins_carry_revs_and_sri_hashes
test_python_is_the_flake_interpreter
test_generator_is_shellcheck_clean

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
