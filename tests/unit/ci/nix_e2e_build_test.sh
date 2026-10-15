#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Drive hack/ci-nix-build-e2e-images.sh in a scratch workspace with stub nix,
# git and nproc binaries first on PATH and scratch account files:
#   - the service lines without a third field and the Tempest releases become
#     the flake attributes <name>-<rel_>-image, built in one nix invocation
#     without the sandbox, one job per CPU, as the build users of the group
#     e2ebld; each image is streamed to .nix-e2e/<name>-<release>.tar and
#     listed in .nix-e2e/images.txt; a nova-compute line is skipped;
#   - the group e2ebld (gid 40000) and one user per CPU (uids 40001 and up)
#     are written to the account files before nix runs;
#   - git is told to trust the workspace before anything else runs;
#   - a line with four fields exits 1 with the line named, builds nothing and
#     writes no build user; an attribute nix cannot build exits with nix's
#     status and writes no image;
#   - an empty image set prints "nothing to build" and exits 0.
#
# The build-e2e-images job of .github/workflows/ci.yaml runs the script in the
# nixos/nix container; this test checks what the script does with its input,
# and that the job's checkout persists no token, which a build user could read
# in /github/runner_temp.
#
# Usage: bash tests/unit/ci/nix_e2e_build_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SCRIPT="$PROJECT_ROOT/hack/ci-nix-build-e2e-images.sh"
# shellcheck disable=SC2034 # read by tests/lib/ci_yaml.sh
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

STUB_DIR="$TMP_DIR/bin"
export NIX_LOG="$TMP_DIR/nix.log"
export GIT_LOG="$TMP_DIR/git.log"
mkdir -p "$STUB_DIR"

# nix logs its arguments and fails like nix when an attribute is the one in
# NIX_MISSING_ATTR. Otherwise it writes, for the n-th .# attribute, an
# executable stream script that prints the attribute at the --out-link path,
# suffixed -<n> from the second attribute on, the way nix build links the
# results of several attributes.
cat >"$STUB_DIR/nix" <<'EOF'
#!/bin/bash
echo "$*" >>"$NIX_LOG"
args=("$@")
attrs=()
link=""
for ((i = 0; i < ${#args[@]}; i++)); do
  case "${args[i]}" in
    .#*) attrs+=("${args[i]#.#}") ;;
    --out-link) link="${args[i + 1]}" ;;
  esac
done
for attr in "${attrs[@]}"; do
  if [[ "$attr" == "${NIX_MISSING_ATTR:-}" ]]; then
    echo "error: flake 'git+file:///ws' does not provide attribute 'packages.x86_64-linux.${attr}'" >&2
    exit 1
  fi
done
for i in "${!attrs[@]}"; do
  out="$link"
  [ "$i" -eq 0 ] || out="${link}-${i}"
  printf '#!/bin/sh\necho "stream of %s"\n' "${attrs[$i]}" >"$out"
  chmod +x "$out"
done
exit 0
EOF
cat >"$STUB_DIR/git" <<'EOF'
#!/bin/sh
echo "$*" >>"$GIT_LOG"
EOF
printf '#!/bin/sh\necho 3\n' >"$STUB_DIR/nproc"
chmod +x "$STUB_DIR/nix" "$STUB_DIR/git" "$STUB_DIR/nproc"

# run_script <workspace> [VAR=value...] runs the script in a fresh workspace
# with fresh account files in a new ETC_DIR and sets OUTPUT and RC.
run_script() {
  local workspace="$1"
  shift
  rm -rf "$workspace" "$NIX_LOG" "$GIT_LOG"
  mkdir -p "$workspace"
  ETC_DIR=$(mktemp -d "$TMP_DIR/etc.XXXXXX")
  echo "root:x:0:0:root:/root:/bin/bash" >"$ETC_DIR/passwd"
  echo "root:x:0:" >"$ETC_DIR/group"
  RC=0
  OUTPUT=$(cd "$workspace" && env PATH="$STUB_DIR:$PATH" GITHUB_WORKSPACE="$workspace" \
    NIX_E2E_ETC="$ETC_DIR" BUILD_SERVICE_IMAGES="" BUILD_TEMPEST_RELEASES="" "$@" bash "$SCRIPT" 2>&1) || RC=$?
}

test_builds_service_and_tempest_images() {
  echo "Test: service lines and Tempest releases become streamed flake attributes"

  local ws="$TMP_DIR/ws-build"
  run_script "$ws" BUILD_SERVICE_IMAGES=$'glance 2026.1\nnova 2026.1 nova-compute\n' \
    BUILD_TEMPEST_RELEASES=$'2026.2\n'

  assert_eq "the script exits 0" "0" "$RC"
  assert_eq "nix runs once" "1" "$(wc -l <"$NIX_LOG" | tr -d ' ')"
  assert_eq "both attributes are built in that one call, without the sandbox, as e2ebld, one job per CPU" \
    "--extra-experimental-features nix-command flakes build --option sandbox false --option build-users-group e2ebld --max-jobs 3 .#glance-2026_1-image .#tempest-2026_2-image --print-build-logs --out-link .nix-e2e/result" \
    "$(cat "$NIX_LOG")"
  assert_not_contains "the nova-compute line is skipped" "$(cat "$NIX_LOG")" "nova"
  assert_eq "images.txt lists both images in order" $'glance 2026.1\ntempest 2026.2' \
    "$(cat "$ws/.nix-e2e/images.txt")"
  assert_eq "the glance stream is written to its tar" "stream of glance-2026_1-image" \
    "$(cat "$ws/.nix-e2e/glance-2026.1.tar")"
  assert_eq "the tempest stream is written to its tar" "stream of tempest-2026_2-image" \
    "$(cat "$ws/.nix-e2e/tempest-2026.2.tar")"
  assert_eq "the group e2ebld lists one build user per CPU" "e2ebld:x:40000:e2ebld1,e2ebld2,e2ebld3" \
    "$(tail -n 1 "$ETC_DIR/group")"
  assert_eq "the build users have uids from 40001 in the group e2ebld" \
    $'e2ebld1:x:40001:40000\ne2ebld2:x:40002:40000\ne2ebld3:x:40003:40000' \
    "$(grep '^e2ebld' "$ETC_DIR/passwd" | cut -d: -f1-4)"
  assert_eq "git trusts the workspace" "config --global --add safe.directory $ws" \
    "$(cat "$GIT_LOG")"
}

test_malformed_line_fails() {
  echo "Test: a line with four fields exits 1 and builds nothing"

  run_script "$TMP_DIR/ws-malformed" BUILD_SERVICE_IMAGES=$'keystone 2026.1\nnova 2026.1 nova-compute extra\n'

  assert_eq "the script exits 1" "1" "$RC"
  assert_contains "the line is named" "$OUTPUT" \
    "ci-nix-build-e2e-images.sh: malformed line 'nova 2026.1 nova-compute extra'"
  assert_eq "nix was not called" "" "$(cat "$NIX_LOG" 2>/dev/null)"
  assert_not_contains "no build user is written" "$(cat "$ETC_DIR/passwd" "$ETC_DIR/group")" "e2ebld"
}

test_missing_attribute_fails() {
  echo "Test: an attribute nix cannot build exits with nix's status and writes no image"

  local ws="$TMP_DIR/ws-missing"
  run_script "$ws" BUILD_SERVICE_IMAGES=$'keystone 2026.1\nswift 2026.1\n' \
    NIX_MISSING_ATTR=swift-2026_1-image

  assert_eq "the script exits 1" "1" "$RC"
  assert_contains "nix names the attribute" "$OUTPUT" \
    "does not provide attribute 'packages.x86_64-linux.swift-2026_1-image'"
  assert_eq "no image is streamed" "" "$(cd "$ws/.nix-e2e" && ls -- *.tar 2>/dev/null)"
  assert_eq "images.txt is empty" "" "$(cat "$ws/.nix-e2e/images.txt")"
}

test_nothing_to_build() {
  echo "Test: an empty image set builds nothing and exits 0"

  run_script "$TMP_DIR/ws-empty" BUILD_SERVICE_IMAGES=$'nova 2026.1 nova-compute\n'

  assert_eq "the script exits 0" "0" "$RC"
  assert_contains "it says so" "$OUTPUT" "ci-nix-build-e2e-images.sh: nothing to build"
  assert_eq "nix was not called" "" "$(cat "$NIX_LOG" 2>/dev/null)"
  assert_eq "images.txt is empty" "" "$(cat "$TMP_DIR/ws-empty/.nix-e2e/images.txt")"
  assert_not_contains "no build user is written" "$(cat "$ETC_DIR/passwd" "$ETC_DIR/group")" "e2ebld"
}

test_checkout_persists_no_token() {
  echo "Test: the build-e2e-images checkout persists no token for the build users to read"

  local checkout
  checkout=$(job_block build-e2e-images | awk '
    /^      - uses: actions\/checkout@/ { in_step = 1; print; next }
    in_step && /^      [-#]/ { exit }
    in_step { print }
  ')

  assert_not_empty "build-e2e-images has a checkout step" "$checkout"
  assert_contains "the checkout sets persist-credentials: false" "$checkout" "persist-credentials: false"
}

test_script_is_shellcheck_clean() {
  echo "Test: ci-nix-build-e2e-images.sh is shellcheck-clean at --severity=warning"

  if ! command -v shellcheck >/dev/null 2>&1; then
    echo "  SKIP: shellcheck not installed"
    SKIP=$((SKIP + 1))
    return
  fi

  local output exit_code=0
  output=$(shellcheck --severity=warning "$SCRIPT" 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "$output"
  assert_eq "shellcheck exits 0" "0" "$exit_code"
}

test_builds_service_and_tempest_images
test_malformed_line_fails
test_missing_attribute_fails
test_nothing_to_build
test_checkout_persists_no_token
test_script_is_shellcheck_clean

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
