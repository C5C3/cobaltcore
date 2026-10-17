#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the operator image jobs of .github/workflows/ci.yaml:
#   - build-and-push builds the flake attribute <op>-operator-image through
#     build-nix-image with the Cachix cache of setup-nix, pushes it by digest
#     as an OCI manifest and exports the digest artifact merge-operator-images
#     downloads;
#   - the manifest step of merge-operator-images passes every index
#     annotation of docker/metadata-action and the revision annotation
#     hack/ci-resolve-e2e-images.sh reads to `docker buildx imagetools
#     create`, with the tags and the per-platform digests, and fails without
#     a digest or without the metadata annotations;
#   - verify-codegen checks the operator vendor hashes with
#     `make nix-lock-check-operators`, which runs the generator's Nix-free
#     check outside the lock-tools shell.
#
# Both publish jobs run on a push only, so no pull request runs them; a
# dropped --annotation or OCI format would leave the index without the
# revision, and every e2e run would then rebuild every operator image
# without a red check. The manifest step runs here from ci.yaml against a
# stub docker that logs its arguments.
#
# Usage: bash tests/unit/ci/operator_image_jobs_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
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

SHA=0123456789abcdef0123456789abcdef01234567
DIGEST=1111111111111111111111111111111111111111111111111111111111111111

# docker logs each argument on a line of its own to $DOCKER_LOG.
mkdir -p "$TMP_DIR/bin"
cat >"$TMP_DIR/bin/docker" <<'EOF'
#!/bin/bash
printf '%s\n' "$@" >>"$DOCKER_LOG"
EOF
chmod +x "$TMP_DIR/bin/docker"

# merge_script <digest dir> prints the run script of the manifest step for
# keystone, reading its digests from <digest dir>.
merge_script() {
  job_step merge-operator-images "Create and push manifest list" |
    sed -n '/^        run: |$/,$p' | sed '1d; s/^          //' |
    sed "s#\${{ matrix.operator }}#keystone#g; s#/tmp/digests#$1#g"
}

# run_merge <digest dir> [<metadata JSON>] runs the manifest step the way a
# runner does, with the given metadata-action output or, by default, the one
# of a push to main. Stores the combined output in OUTPUT, the exit status in
# RC and the docker arguments in ARGS.
run_merge() {
  local log="$TMP_DIR/docker.log"
  : >"$log"
  RC=0
  OUTPUT=$(
    PATH="$TMP_DIR/bin:$PATH" DOCKER_LOG="$log" IMAGE=ghcr.io/c5c3/keystone-operator \
      GITHUB_SHA="$SHA" DOCKER_METADATA_OUTPUT_JSON="${2:-$(jq -cn --arg sha "$SHA" '{
        tags: ["ghcr.io/c5c3/keystone-operator:sha-\($sha)", "ghcr.io/c5c3/keystone-operator:latest"],
        annotations: [
          "index:org.opencontainers.image.revision=\($sha)",
          "index:org.opencontainers.image.source=https://github.com/C5C3/cobaltcore",
          "index:org.opencontainers.image.title=keystone-operator"
        ]}')}" bash --noprofile --norc -eo pipefail -c "$(merge_script "$1")" 2>&1
  ) || RC=$?
  ARGS=$(cat "$log")
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_build_and_push_publishes_through_build_nix_image() {
  echo "Test: build-and-push builds the operator image from the flake and pushes an OCI manifest by digest"

  local job line
  job=$(job_block build-and-push)
  for line in \
    "uses: ./.github/actions/setup-nix" \
    'cachix-auth-token: ${{ secrets.CACHIX_AUTH_TOKEN }}' \
    "uses: ./.github/actions/build-nix-image" \
    'attribute: ${{ matrix.operator }}-operator-image' \
    'image-name: ${{ env.IMAGE_PREFIX }}/${{ matrix.operator }}-operator' \
    "push-by-digest: 'true'" \
    'artifact-name: digests-operator-${{ matrix.operator }}-${{ steps.platform-pair.outputs.value }}' \
    "manifest-format: oci"; do
    assert_contains "build-and-push has '$line'" "$job" "$line"
  done
  assert_not_contains "build-and-push runs no BuildKit build" "$job" "docker/build-push-action"
  assert_contains "merge-operator-images downloads the digest artifacts build-and-push exports" \
    "$(job_step merge-operator-images "Download digests")" 'pattern: digests-operator-${{ matrix.operator }}-*'
}

test_metadata_annotations_go_onto_the_index() {
  echo "Test: docker/metadata-action writes its annotations for the index, with the image's static values"

  local meta line
  meta=$(job_step merge-operator-images "Generate image metadata")
  assert_contains "the annotations are index annotations" "$meta" "DOCKER_METADATA_ANNOTATIONS_LEVELS: index"
  for line in \
    'org.opencontainers.image.title=${{ matrix.operator }}-operator' \
    'org.opencontainers.image.description=CobaltCore ${{ matrix.operator }} operator' \
    "org.opencontainers.image.licenses=Apache-2.0" \
    "org.opencontainers.image.vendor=SAP SE"; do
    assert_contains "the metadata step sets $line" "$meta" "$line"
  done
}

test_the_manifest_step_writes_the_annotations() {
  echo "Test: the manifest step creates the index with every annotation, the tags and the digests"

  local digests="$TMP_DIR/digests" expected
  mkdir -p "$digests"
  : >"$digests/$DIGEST"
  run_merge "$digests"
  [ "$RC" -eq 0 ] || echo "$OUTPUT"
  assert_eq "the step exits 0" "0" "$RC"
  expected="buildx
imagetools
create
--annotation
index:org.opencontainers.image.revision=$SHA
--annotation
index:org.opencontainers.image.source=https://github.com/C5C3/cobaltcore
--annotation
index:org.opencontainers.image.title=keystone-operator
--annotation
index:org.opencontainers.image.revision=$SHA
-t
ghcr.io/c5c3/keystone-operator:sha-$SHA
-t
ghcr.io/c5c3/keystone-operator:latest
ghcr.io/c5c3/keystone-operator@sha256:$DIGEST"
  assert_eq "docker gets the annotations, the explicit revision, the tags and the digest" "$expected" "$ARGS"
  assert_file_contains_fixed "the resolver reads the revision annotation the step writes" \
    "$PROJECT_ROOT/hack/ci-resolve-e2e-images.sh" '.annotations["org.opencontainers.image.revision"]'

  mkdir -p "$TMP_DIR/no-digests"
  run_merge "$TMP_DIR/no-digests"
  assert_nonzero_exit "the step fails without a digest" "$RC"
  assert_contains "it names the operator" "$OUTPUT" "::error::No digests found for keystone-operator"
  assert_eq "it creates no index" "" "$ARGS"

  run_merge "$digests" "$(jq -cn --arg sha "$SHA" '{tags: ["ghcr.io/c5c3/keystone-operator:sha-\($sha)"]}')"
  assert_nonzero_exit "the step fails when the metadata output has no annotations" "$RC"
  assert_contains "it names the missing annotations" "$OUTPUT" "docker/metadata-action wrote no annotations"
  assert_eq "it creates no index without them" "" "$ARGS"
}

test_verify_codegen_checks_the_operator_vendor_hashes() {
  echo "Test: verify-codegen runs the Nix-free check of the operator vendor hashes"

  assert_contains "verify-codegen runs make nix-lock-check-operators" \
    "$(job_step verify-codegen "Verify the operator vendor hashes")" "run: make nix-lock-check-operators"
  assert_not_contains "verify-codegen installs no Nix" "$(job_block verify-codegen)" "setup-nix"
  assert_eq "the target runs the generator's --check-operators outside the lock-tools shell" \
    "bash hack/nix-lock.sh --check-operators" \
    "$(make --no-print-directory -n -C "$PROJECT_ROOT" nix-lock-check-operators)"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
if ! command -v jq >/dev/null 2>&1; then
  echo "SKIP: jq not installed (all checks skipped)"
  echo ""
  echo "Results: 0 passed, 0 failed, 1 skipped"
  exit 0
fi

test_build_and_push_publishes_through_build_nix_image
test_metadata_annotations_go_onto_the_index
test_the_manifest_step_writes_the_annotations
test_verify_codegen_checks_the_operator_vendor_hashes

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
