#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Drive hack/ci-nix-build-e2e-images.sh in a scratch workspace with stub nix,
# git and nproc binaries first on PATH and scratch account files:
#   - the service lines and the Tempest releases become the flake attributes
#     <name>-<rel_>-image, <name> the third field of a line that has one
#     (nova-compute), built in one nix invocation without the sandbox, one
#     job per CPU, as the build users of the group e2ebld; each image is
#     streamed to .nix-e2e/<name>-<release>.tar and listed in
#     .nix-e2e/images.txt;
#   - BUILD_OVN adds the attribute ovn-image to that invocation, streamed to
#     .nix-e2e/ovn-<OVN_VERSION>.tar and listed as "ovn <OVN_VERSION>"; a
#     BUILD_OVN without OVN_VERSION exits 1 and builds nothing;
#   - BUILD_LIBVIRT adds the attribute libvirt-image, streamed to
#     .nix-e2e/libvirt-latest.tar and listed as "libvirt latest";
#   - BUILD_PROXY adds the attribute keystone-federation-proxy-image,
#     streamed to .nix-e2e/keystone-federation-proxy-latest.tar and listed as
#     "keystone-federation-proxy latest";
#   - each BUILD_OPERATORS line adds the attribute <op>-operator-image after
#     every other image, streamed to .nix-e2e/<op>-operator-latest.tar and
#     listed as "<op>-operator latest";
#   - the group e2ebld (gid 40000) and one user per CPU (uids 40001 and up)
#     are written to the account files before nix runs;
#   - git is told to trust the workspace before anything else runs;
#   - a line with four fields exits 1 with the line named, builds nothing and
#     writes no build user; an attribute nix cannot build exits with nix's
#     status and writes no image;
#   - an empty image set, BUILD_OVN=false, BUILD_LIBVIRT=false,
#     BUILD_PROXY=false, an unset BUILD_PROXY and an empty BUILD_OPERATORS
#     included, prints "nothing to build" and exits 0.
#
# The build-e2e-images job of .github/workflows/ci.yaml runs the script in the
# nixos/nix container; this test checks what the script does with its input,
# that the job's checkout persists no token, which a build user could read
# in /github/runner_temp, and that the job builds the federation proxy and
# the operator images through the script, retags them :dev and verifies the
# operator images.
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

# run_script <workspace> [--unset <NAME>] [VAR=value...] runs the script in a
# fresh workspace with fresh account files in a new ETC_DIR and sets OUTPUT
# and RC. Every BUILD_* input defaults to empty or false; --unset leaves NAME
# out of the environment instead.
run_script() {
  local workspace="$1" unset_name="" input
  local -a inputs=()
  shift
  if [ "${1:-}" = "--unset" ]; then
    unset_name="$2"
    shift 2
  fi
  for input in BUILD_SERVICE_IMAGES= BUILD_TEMPEST_RELEASES= BUILD_OVN=false OVN_VERSION= \
    BUILD_LIBVIRT=false BUILD_PROXY=false BUILD_OPERATORS= BUILD_COVER_OPERATORS=; do
    [ "${input%%=*}" = "$unset_name" ] || inputs+=("$input")
  done
  rm -rf "$workspace" "$NIX_LOG" "$GIT_LOG"
  mkdir -p "$workspace"
  ETC_DIR=$(mktemp -d "$TMP_DIR/etc.XXXXXX")
  echo "root:x:0:0:root:/root:/bin/bash" >"$ETC_DIR/passwd"
  echo "root:x:0:" >"$ETC_DIR/group"
  RC=0
  OUTPUT=$(cd "$workspace" && env ${unset_name:+-u "$unset_name"} PATH="$STUB_DIR:$PATH" \
    GITHUB_WORKSPACE="$workspace" NIX_E2E_ETC="$ETC_DIR" "${inputs[@]}" \
    "$@" bash "$SCRIPT" 2>&1) || RC=$?
}

test_builds_service_and_tempest_images() {
  echo "Test: service lines and Tempest releases become streamed flake attributes"

  local ws="$TMP_DIR/ws-build"
  run_script "$ws" BUILD_SERVICE_IMAGES=$'glance 2026.1\nnova 2026.1 nova-compute\n' \
    BUILD_TEMPEST_RELEASES=$'2026.2\n'

  assert_eq "the script exits 0" "0" "$RC"
  assert_eq "nix runs once" "1" "$(wc -l <"$NIX_LOG" | tr -d ' ')"
  assert_eq "the three attributes are built in that one call, without the sandbox, as e2ebld, one job per CPU" \
    "--extra-experimental-features nix-command flakes build --option sandbox false --option build-users-group e2ebld --max-jobs 3 .#glance-2026_1-image .#nova-compute-2026_1-image .#tempest-2026_2-image --print-build-logs --out-link .nix-e2e/result" \
    "$(cat "$NIX_LOG")"
  assert_eq "images.txt lists the images in order, nova-compute under its own name" \
    $'glance 2026.1\nnova-compute 2026.1\ntempest 2026.2' "$(cat "$ws/.nix-e2e/images.txt")"
  assert_eq "the glance stream is written to its tar" "stream of glance-2026_1-image" \
    "$(cat "$ws/.nix-e2e/glance-2026.1.tar")"
  assert_eq "the nova-compute stream is written to its tar" "stream of nova-compute-2026_1-image" \
    "$(cat "$ws/.nix-e2e/nova-compute-2026.1.tar")"
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

  run_script "$ws" BUILD_PROXY=true NIX_MISSING_ATTR=keystone-federation-proxy-image
  assert_eq "a failing proxy build exits with nix's status" "1" "$RC"
  assert_eq "no keystone-federation-proxy-latest.tar is written" "false" \
    "$([ -e "$ws/.nix-e2e/keystone-federation-proxy-latest.tar" ] && echo true || echo false)"
}

test_builds_the_ovn_image() {
  echo "Test: BUILD_OVN builds ovn-image and lists it under OVN_VERSION"

  local ws="$TMP_DIR/ws-ovn"
  run_script "$ws" BUILD_OVN=true OVN_VERSION=26.03.2

  assert_eq "the script exits 0" "0" "$RC"
  assert_eq "nix builds the attribute ovn-image" \
    "--extra-experimental-features nix-command flakes build --option sandbox false --option build-users-group e2ebld --max-jobs 3 .#ovn-image --print-build-logs --out-link .nix-e2e/result" \
    "$(cat "$NIX_LOG")"
  assert_eq "images.txt lists the ovn image under its version" "ovn 26.03.2" \
    "$(cat "$ws/.nix-e2e/images.txt")"
  assert_eq "the ovn stream is written to its tar" "stream of ovn-image" \
    "$(cat "$ws/.nix-e2e/ovn-26.03.2.tar")"

  run_script "$ws" BUILD_SERVICE_IMAGES=$'keystone 2026.1\n' BUILD_OVN=true OVN_VERSION=26.03.2
  assert_eq "a service image and the ovn image build in one nix call" "1" \
    "$(wc -l <"$NIX_LOG" | tr -d ' ')"
  assert_contains "that call names both attributes in order" "$(cat "$NIX_LOG")" \
    ".#keystone-2026_1-image .#ovn-image --print-build-logs"
  assert_eq "images.txt lists both images in order" $'keystone 2026.1\novn 26.03.2' \
    "$(cat "$ws/.nix-e2e/images.txt")"
  assert_eq "the second result is the ovn stream" "stream of ovn-image" \
    "$(cat "$ws/.nix-e2e/ovn-26.03.2.tar")"
}

test_ovn_without_a_version_fails() {
  echo "Test: BUILD_OVN without OVN_VERSION exits 1 and builds nothing"

  run_script "$TMP_DIR/ws-ovn-noversion" BUILD_SERVICE_IMAGES=$'keystone 2026.1\n' BUILD_OVN=true

  assert_eq "the script exits 1" "1" "$RC"
  assert_contains "the message names both variables" "$OUTPUT" \
    "ci-nix-build-e2e-images.sh: BUILD_OVN is true but OVN_VERSION is empty"
  assert_eq "nix was not called" "" "$(cat "$NIX_LOG" 2>/dev/null)"
  assert_not_contains "no build user is written" "$(cat "$ETC_DIR/passwd" "$ETC_DIR/group")" "e2ebld"
}

test_builds_the_libvirt_image() {
  echo "Test: BUILD_LIBVIRT builds libvirt-image and lists it as libvirt latest"

  local ws="$TMP_DIR/ws-libvirt"
  run_script "$ws" BUILD_LIBVIRT=true

  assert_eq "the script exits 0" "0" "$RC"
  assert_eq "nix builds the attribute libvirt-image" \
    "--extra-experimental-features nix-command flakes build --option sandbox false --option build-users-group e2ebld --max-jobs 3 .#libvirt-image --print-build-logs --out-link .nix-e2e/result" \
    "$(cat "$NIX_LOG")"
  assert_eq "images.txt lists the libvirt image under latest, the tag docker load prints" \
    "libvirt latest" "$(cat "$ws/.nix-e2e/images.txt")"
  assert_eq "the libvirt stream is written to its tar" "stream of libvirt-image" \
    "$(cat "$ws/.nix-e2e/libvirt-latest.tar")"

  run_script "$ws" BUILD_SERVICE_IMAGES=$'nova 2026.1 nova-compute\n' BUILD_LIBVIRT=true
  assert_eq "the compute image and the libvirt image build in one nix call" "1" \
    "$(wc -l <"$NIX_LOG" | tr -d ' ')"
  assert_contains "that call names both attributes in order" "$(cat "$NIX_LOG")" \
    ".#nova-compute-2026_1-image .#libvirt-image --print-build-logs"
  assert_eq "the second result is the libvirt stream" "stream of libvirt-image" \
    "$(cat "$ws/.nix-e2e/libvirt-latest.tar")"

  run_script "$TMP_DIR/ws-libvirt-off" BUILD_LIBVIRT=false
  assert_contains "BUILD_LIBVIRT=false alone builds nothing" "$OUTPUT" \
    "ci-nix-build-e2e-images.sh: nothing to build"
}

test_builds_the_proxy_image() {
  echo "Test: BUILD_PROXY builds keystone-federation-proxy-image and lists it as keystone-federation-proxy latest"

  local ws="$TMP_DIR/ws-proxy"
  run_script "$ws" BUILD_PROXY=true

  assert_eq "the script exits 0" "0" "$RC"
  assert_eq "nix builds the attribute keystone-federation-proxy-image" \
    "--extra-experimental-features nix-command flakes build --option sandbox false --option build-users-group e2ebld --max-jobs 3 .#keystone-federation-proxy-image --print-build-logs --out-link .nix-e2e/result" \
    "$(cat "$NIX_LOG")"
  assert_eq "images.txt lists the proxy image under latest, the tag docker load prints" \
    "keystone-federation-proxy latest" "$(cat "$ws/.nix-e2e/images.txt")"
  assert_eq "the proxy stream is written to its tar" "stream of keystone-federation-proxy-image" \
    "$(cat "$ws/.nix-e2e/keystone-federation-proxy-latest.tar")"

  run_script "$ws" BUILD_LIBVIRT=true BUILD_PROXY=true
  assert_eq "the libvirt image and the proxy image build in one nix call" "1" \
    "$(wc -l <"$NIX_LOG" | tr -d ' ')"
  assert_contains "that call names both attributes in order" "$(cat "$NIX_LOG")" \
    ".#libvirt-image .#keystone-federation-proxy-image --print-build-logs"
  assert_eq "the second result is the proxy stream" "stream of keystone-federation-proxy-image" \
    "$(cat "$ws/.nix-e2e/keystone-federation-proxy-latest.tar")"

  run_script "$TMP_DIR/ws-proxy-off" BUILD_PROXY=false
  assert_contains "BUILD_PROXY=false alone builds nothing" "$OUTPUT" \
    "ci-nix-build-e2e-images.sh: nothing to build"
  assert_eq "BUILD_PROXY=false calls no nix" "" "$(cat "$NIX_LOG" 2>/dev/null)"

  run_script "$TMP_DIR/ws-proxy-unset" --unset BUILD_PROXY
  assert_eq "an unset BUILD_PROXY exits 0" "0" "$RC"
  assert_contains "an unset BUILD_PROXY alone builds nothing" "$OUTPUT" \
    "ci-nix-build-e2e-images.sh: nothing to build"
  assert_eq "an unset BUILD_PROXY calls no nix" "" "$(cat "$NIX_LOG" 2>/dev/null)"
}

test_builds_the_operator_images() {
  echo "Test: BUILD_OPERATORS builds <op>-operator-image and lists it as <op>-operator latest"

  local ws="$TMP_DIR/ws-operators"
  run_script "$ws" BUILD_OPERATORS=$'keystone\nc5c3'

  assert_eq "the script exits 0" "0" "$RC"
  assert_eq "nix builds both operator images in one call" \
    "--extra-experimental-features nix-command flakes build --option sandbox false --option build-users-group e2ebld --max-jobs 3 .#keystone-operator-image .#c5c3-operator-image --print-build-logs --out-link .nix-e2e/result" \
    "$(cat "$NIX_LOG")"
  assert_eq "images.txt lists the operator images under latest, the tag docker load prints" \
    $'keystone-operator latest\nc5c3-operator latest' "$(cat "$ws/.nix-e2e/images.txt")"
  assert_eq "the keystone stream is written to its tar" "stream of keystone-operator-image" \
    "$(cat "$ws/.nix-e2e/keystone-operator-latest.tar")"
  assert_eq "the c5c3 stream is written to its tar" "stream of c5c3-operator-image" \
    "$(cat "$ws/.nix-e2e/c5c3-operator-latest.tar")"

  run_script "$ws" BUILD_SERVICE_IMAGES=$'glance 2026.1\n' BUILD_PROXY=true BUILD_OPERATORS=$'keystone\n'
  assert_contains "the service comes first and the operator after the proxy" "$(cat "$NIX_LOG")" \
    ".#glance-2026_1-image .#keystone-federation-proxy-image .#keystone-operator-image --print-build-logs"
  assert_eq "images.txt follows the attribute order" \
    $'glance 2026.1\nkeystone-federation-proxy latest\nkeystone-operator latest' \
    "$(cat "$ws/.nix-e2e/images.txt")"
  assert_eq "the third result is the operator stream" "stream of keystone-operator-image" \
    "$(cat "$ws/.nix-e2e/keystone-operator-latest.tar")"

  run_script "$TMP_DIR/ws-operators-missing" BUILD_OPERATORS=$'keystone\nswift' \
    NIX_MISSING_ATTR=swift-operator-image
  assert_eq "an operator the flake lacks exits with nix's status" "1" "$RC"
  assert_eq "no operator image is streamed" "" \
    "$(cd "$TMP_DIR/ws-operators-missing/.nix-e2e" && ls -- *.tar 2>/dev/null)"

  run_script "$TMP_DIR/ws-operators-empty" BUILD_OPERATORS=$'\n'
  assert_contains "an empty BUILD_OPERATORS alone builds nothing" "$OUTPUT" \
    "ci-nix-build-e2e-images.sh: nothing to build"
  assert_eq "an empty BUILD_OPERATORS calls no nix" "" "$(cat "$NIX_LOG" 2>/dev/null)"
}

test_builds_the_cover_images() {
  echo "Test: BUILD_COVER_OPERATORS builds <op>-operator-cover-image and lists it under cover"

  local ws="$TMP_DIR/ws-cover"
  run_script "$ws" BUILD_COVER_OPERATORS=$'keystone\nnova'

  assert_eq "the script exits 0" "0" "$RC"
  assert_eq "nix builds both cover images in one call" \
    "--extra-experimental-features nix-command flakes build --option sandbox false --option build-users-group e2ebld --max-jobs 3 .#keystone-operator-cover-image .#nova-operator-cover-image --print-build-logs --out-link .nix-e2e/result" \
    "$(cat "$NIX_LOG")"
  assert_eq "images.txt lists the cover images under cover, the tag docker load prints" \
    $'keystone-operator cover\nnova-operator cover' "$(cat "$ws/.nix-e2e/images.txt")"
  assert_eq "the keystone cover stream is written to its tar" "stream of keystone-operator-cover-image" \
    "$(cat "$ws/.nix-e2e/keystone-operator-cover.tar")"
  assert_eq "the nova cover stream is written to its tar" "stream of nova-operator-cover-image" \
    "$(cat "$ws/.nix-e2e/nova-operator-cover.tar")"

  run_script "$ws" BUILD_OPERATORS=$'keystone\n' BUILD_COVER_OPERATORS=$'keystone\n'
  assert_eq "an operator image and its cover image build in one nix call" "1" \
    "$(wc -l <"$NIX_LOG" | tr -d ' ')"
  assert_contains "the cover attribute follows the latest one" "$(cat "$NIX_LOG")" \
    ".#keystone-operator-image .#keystone-operator-cover-image --print-build-logs"
  assert_eq "images.txt follows the attribute order" \
    $'keystone-operator latest\nkeystone-operator cover' "$(cat "$ws/.nix-e2e/images.txt")"

  run_script "$TMP_DIR/ws-cover-empty" BUILD_COVER_OPERATORS=$'\n'
  assert_contains "an empty BUILD_COVER_OPERATORS alone builds nothing" "$OUTPUT" \
    "ci-nix-build-e2e-images.sh: nothing to build"
  assert_eq "an empty BUILD_COVER_OPERATORS calls no nix" "" "$(cat "$NIX_LOG" 2>/dev/null)"

  run_script "$TMP_DIR/ws-cover-unset" --unset BUILD_COVER_OPERATORS BUILD_OPERATORS=$'keystone\n'
  assert_eq "an unset BUILD_COVER_OPERATORS exits 0" "0" "$RC"
  assert_not_contains "an unset BUILD_COVER_OPERATORS adds no cover attribute" "$(cat "$NIX_LOG")" \
    "cover-image"
}

test_ci_wires_the_cover_images() {
  echo "Test: build-e2e-images resolves, builds, verifies and pushes the cover images"

  assert_contains "Resolve images asks for the cover images when a coverage job runs" \
    "$(job_step build-e2e-images "Resolve images")" \
    "BUILD_COVER_IMAGES: \${{ needs.changes.outputs.has-e2e-operators == 'true' || needs.changes.outputs.e2e-chaos == 'true' || needs.changes.outputs.tempest == 'true' }}"
  assert_contains "Build Nix images passes BUILD_COVER_OPERATORS" \
    "$(job_step build-e2e-images "Build Nix images")" \
    'BUILD_COVER_OPERATORS: ${{ env.BUILD_COVER_OPERATORS }}'

  local verify push
  verify=$(job_step build-e2e-images "Verify operator images")
  assert_contains "the verify step loops over BUILD_COVER_OPERATORS" "$verify" \
    'for op in $BUILD_COVER_OPERATORS; do'
  assert_contains "the verify step checks each cover image as the cover variant" "$verify" \
    'bash tests/container-images/verify_operator_image.sh "${IMAGE_PREFIX}/${op}-operator:cover" "${op}" cover'
  push=$(job_step build-e2e-images "Push E2E images to GHCR")
  assert_contains "the push step loops over BUILD_COVER_OPERATORS" "$push" \
    'for op in $BUILD_COVER_OPERATORS; do'
  assert_contains "the push step pushes each cover image" "$push" \
    'push_image "${IMAGE_PREFIX}/${op}-operator:cover"'
}

test_ci_wires_the_operators_through_the_nix_step() {
  echo "Test: build-e2e-images builds the operator images in the Nix step and retags them :dev"

  local nix_step tag_step
  nix_step=$(job_step build-e2e-images "Build Nix images")
  assert_contains "Build Nix images passes BUILD_OPERATORS" "$nix_step" \
    'BUILD_OPERATORS: ${{ env.BUILD_OPERATORS }}'
  tag_step=$(job_step build-e2e-images "Tag operator images")
  assert_contains "the tag step retags each operator's latest as dev" "$tag_step" \
    'docker tag "${IMAGE_PREFIX}/${op}-operator:latest" "${IMAGE_PREFIX}/${op}-operator:dev"'
  assert_not_contains "build-e2e-images runs no make target to build an image" "$(job_block build-e2e-images)" \
    "make "
}

test_ci_verifies_the_operator_images() {
  echo "Test: build-e2e-images verifies each operator image it built"

  assert_contains "the verify step runs verify_operator_image.sh on each operator's dev image" \
    "$(job_step build-e2e-images "Verify operator images")" \
    'bash tests/container-images/verify_operator_image.sh "${IMAGE_PREFIX}/${op}-operator:dev" "${op}"'
}

test_ci_wires_the_proxy_through_the_nix_step() {
  echo "Test: build-e2e-images builds the federation proxy in the Nix step and retags it :dev"

  local filter nix_step tag_step glob
  filter=$(filter_block image_proxy)
  for glob in "'images/keystone-federation-proxy/**'" "'flake.nix'" "'flake.lock'" "'nix/lib/**'" \
    "'hack/ci-nix-build-e2e-images.sh'"; do
    assert_contains "the image_proxy filter names $glob" "$filter" "- $glob"
  done
  nix_step=$(job_step build-e2e-images "Build Nix images")
  assert_contains "Build Nix images passes BUILD_PROXY" "$nix_step" 'BUILD_PROXY: ${{ env.BUILD_PROXY }}'
  tag_step=$(job_step build-e2e-images "Tag federation proxy image")
  assert_contains "the tag step runs when the proxy is built" "$tag_step" "if: env.BUILD_PROXY == 'true'"
  assert_contains "the tag step retags latest as dev" "$tag_step" \
    'docker tag "${IMAGE_PREFIX}/keystone-federation-proxy:latest" "${IMAGE_PREFIX}/keystone-federation-proxy:dev"'
  assert_not_contains "build-e2e-images runs no docker build of the proxy" \
    "$(job_block build-e2e-images)" "images/keystone-federation-proxy/"
}

test_nothing_to_build() {
  echo "Test: an empty image set builds nothing and exits 0"

  run_script "$TMP_DIR/ws-empty" BUILD_SERVICE_IMAGES="" BUILD_OVN=false OVN_VERSION=26.03.2

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
test_builds_the_ovn_image
test_ovn_without_a_version_fails
test_builds_the_libvirt_image
test_builds_the_proxy_image
test_builds_the_operator_images
test_builds_the_cover_images
test_ci_wires_the_cover_images
test_ci_wires_the_operators_through_the_nix_step
test_ci_verifies_the_operator_images
test_ci_wires_the_proxy_through_the_nix_step
test_nothing_to_build
test_checkout_persists_no_token
test_script_is_shellcheck_clean

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
