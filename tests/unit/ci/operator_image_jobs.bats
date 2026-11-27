#!/usr/bin/env bats
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
#   - the manifest step writes the index digest that `imagetools create`
#     reports in its --metadata-file, under RUNNER_TEMP, to GITHUB_OUTPUT
#     for the sign step, and fails without one;
#   - the `Read the chart version` step of merge-operator-images writes the
#     X.Y.Z version of the operator's Chart.yaml, quoted or not, for the
#     chart-version tag the upgrade suites pin, and only warns on anything
#     else, so the sha and latest tags are still published;
#   - verify-codegen checks the operator vendor hashes with
#     `make nix-lock-check-operators`, which runs the generator's Nix-free
#     check outside the lock-tools shell.
#
# Both publish jobs run on a push only, so no pull request runs them; a
# dropped --annotation or OCI format would leave the index without the
# revision, and every e2e run would then rebuild every operator image
# without a red check. The manifest step runs here from ci.yaml against a
# stub docker that logs its arguments and writes the metadata file, and the
# chart version step against a scratch Chart.yaml.
#
# Usage: bin/bats tests/unit/ci/operator_image_jobs.bats

load '../../lib/bats/common'
load '../../lib/bats/stubs'

# shellcheck disable=SC2034 # read by tests/lib/ci_yaml.sh
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

SHA=0123456789abcdef0123456789abcdef01234567
DIGEST=1111111111111111111111111111111111111111111111111111111111111111
INDEX_DIGEST=2222222222222222222222222222222222222222222222222222222222222222

# docker logs each argument on a line of its own to $DOCKER_LOG, then plays
# DOCKER_STUB_OUTCOME: ok writes the metadata file buildx v0.39.0 writes,
# with the index digest $INDEX_DIGEST, to the path after --metadata-file;
# empty writes {} there; none writes nothing; fail exits 1.
setup() {
  require_tool jq

  TMP_DIR="$BATS_TEST_TMPDIR"

  stub_script docker <<'EOF'
printf '%s\n' "$@" >>"$DOCKER_LOG"
metadata=""
while [ $# -gt 1 ]; do
  if [ "$1" = "--metadata-file" ]; then
    metadata="$2"
  fi
  shift
done
case "$DOCKER_STUB_OUTCOME" in
  ok)
    [ -z "$metadata" ] ||
      printf '{"containerimage.descriptor":{"mediaType":"application/vnd.oci.image.index.v1+json","digest":"sha256:%s","size":1609},"image.name":"ghcr.io/c5c3/keystone-operator:latest"}\n' \
        "$INDEX_DIGEST" >"$metadata"
    ;;
  empty) [ -z "$metadata" ] || echo '{}' >"$metadata" ;;
  none) ;;
  fail) exit 1 ;;
esac
EOF
}

# merge_script <digest dir> prints the run script of the manifest step for
# keystone, reading its digests from <digest dir>.
merge_script() {
  job_step merge-operator-images "Create and push manifest list" |
    sed -n '/^        run: |$/,$p' | sed '1d; s/^          //' |
    sed "s#\${{ matrix.operator }}#keystone#g; s#/tmp/digests#$1#g"
}

# run_merge <digest dir> [<metadata JSON>] runs the manifest step the way a
# runner does, with the given metadata-action output or, by default, the one
# of a push to main, with RUNNER_TEMP and GITHUB_OUTPUT under TMP_DIR and no
# metadata file left from an earlier call. The docker stub plays
# DOCKER_STUB_OUTCOME (default ok). Stores the combined output in OUTPUT, the
# exit status in RC, the docker arguments in ARGS and what the step wrote to
# GITHUB_OUTPUT in WRITTEN.
run_merge() {
  local log="$TMP_DIR/docker.log" runner_temp="$TMP_DIR/runner-temp"
  : >"$log"
  mkdir -p "$runner_temp"
  rm -f "$runner_temp/imagetools-metadata.json"
  : >"$TMP_DIR/github_output"
  RC=0
  OUTPUT=$(
    PATH="$STUB_DIR/bin:$PATH" DOCKER_LOG="$log" IMAGE=ghcr.io/c5c3/keystone-operator \
      DOCKER_STUB_OUTCOME="${DOCKER_STUB_OUTCOME:-ok}" INDEX_DIGEST="$INDEX_DIGEST" \
      RUNNER_TEMP="$runner_temp" GITHUB_OUTPUT="$TMP_DIR/github_output" \
      GITHUB_SHA="$SHA" DOCKER_METADATA_OUTPUT_JSON="${2:-$(jq -cn --arg sha "$SHA" '{
        tags: ["ghcr.io/c5c3/keystone-operator:sha-\($sha)", "ghcr.io/c5c3/keystone-operator:latest"],
        annotations: [
          "index:org.opencontainers.image.revision=\($sha)",
          "index:org.opencontainers.image.source=https://github.com/C5C3/cobaltcore",
          "index:org.opencontainers.image.title=keystone-operator"
        ]}')}" bash --noprofile --norc -eo pipefail -c "$(merge_script "$1")" 2>&1
  ) || RC=$?
  ARGS=$(cat "$log")
  WRITTEN=$(cat "$TMP_DIR/github_output")
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

@test "build-and-push builds the operator image from the flake and pushes an OCI manifest by digest" {
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
    soft assert_contains "build-and-push has '$line'" "$job" "$line"
  done
  assert_not_contains "build-and-push runs no BuildKit build" "$job" "docker/build-push-action"
  assert_contains "merge-operator-images downloads the digest artifacts build-and-push exports" \
    "$(job_step merge-operator-images "Download digests")" 'pattern: digests-operator-${{ matrix.operator }}-*'
}

@test "docker/metadata-action writes its annotations for the index, with the image's static values" {
  local meta line
  meta=$(job_step merge-operator-images "Generate image metadata")
  assert_contains "the annotations are index annotations" "$meta" "DOCKER_METADATA_ANNOTATIONS_LEVELS: index"
  for line in \
    'org.opencontainers.image.title=${{ matrix.operator }}-operator' \
    'org.opencontainers.image.description=CobaltCore OpenStack ${{ matrix.operator }} operator' \
    "org.opencontainers.image.licenses=Apache-2.0" \
    "org.opencontainers.image.vendor=SAP SE"; do
    soft assert_contains "the metadata step sets $line" "$meta" "$line"
  done
}

@test "the manifest step creates the index with every annotation, the tags and the digests" {
  local digests="$TMP_DIR/digests" expected
  mkdir -p "$digests"
  : >"$digests/$DIGEST"
  run_merge "$digests"
  [ "$RC" -eq 0 ] || echo "$OUTPUT"
  assert_eq "the step exits 0" "0" "$RC"
  expected="buildx
imagetools
create
--metadata-file
$TMP_DIR/runner-temp/imagetools-metadata.json
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
  assert_eq "docker gets the metadata file, the annotations, the explicit revision, the tags and the digest" "$expected" "$ARGS"
  assert_eq "the step writes the index digest for the sign step" "digest=sha256:$INDEX_DIGEST" "$WRITTEN"
  assert_file_contains_fixed "the resolver reads the revision annotation the step writes" \
    "$PROJECT_ROOT/hack/ci-resolve-e2e-images.sh" '.annotations["org.opencontainers.image.revision"]'

  mkdir -p "$TMP_DIR/no-digests"
  run_merge "$TMP_DIR/no-digests"
  assert_nonzero_exit "the step fails without a digest" "$RC"
  assert_contains "it names the operator" "$OUTPUT" "::error::No digests found for keystone-operator"
  assert_eq "it creates no index" "" "$ARGS"
  assert_eq "it writes no index digest" "" "$WRITTEN"

  run_merge "$digests" "$(jq -cn --arg sha "$SHA" '{tags: ["ghcr.io/c5c3/keystone-operator:sha-\($sha)"]}')"
  assert_nonzero_exit "the step fails when the metadata output has no annotations" "$RC"
  assert_contains "it names the missing annotations" "$OUTPUT" "docker/metadata-action wrote no annotations"
  assert_eq "it creates no index without them" "" "$ARGS"
}

@test "the manifest step fails when imagetools create reports no index digest" {
  local digests="$TMP_DIR/digests" outcome
  mkdir -p "$digests"
  : >"$digests/$DIGEST"

  for outcome in none empty; do
    DOCKER_STUB_OUTCOME=$outcome run_merge "$digests"
    soft assert_eq "$outcome: the step exits 1" "1" "$RC"
    soft assert_contains "$outcome: it names the metadata file" "$OUTPUT" \
      "::error::docker buildx imagetools create wrote no index digest to $TMP_DIR/runner-temp/imagetools-metadata.json"
    soft assert_not_contains "$outcome: it writes no digest" "$WRITTEN" "digest="
  done

  DOCKER_STUB_OUTCOME=fail run_merge "$digests"
  assert_nonzero_exit "a failed imagetools create fails the step" "$RC"
  assert_not_contains "it writes no digest" "$WRITTEN" "digest="
}

# run_chart_version <Chart.yaml version line> runs the chart version step of
# merge-operator-images for keystone in a scratch tree whose Chart.yaml holds
# that line, beside the hack/ci-read-chart-version.sh the step calls. Stores
# the combined output in OUTPUT, the exit status in RC and what the step wrote
# to GITHUB_OUTPUT in WRITTEN.
run_chart_version() {
  local tree="$TMP_DIR/chart-tree" script
  rm -rf "$tree"
  mkdir -p "$tree/operators/keystone/helm/keystone-operator" "$tree/hack"
  cp "$PROJECT_ROOT/hack/ci-read-chart-version.sh" "$tree/hack/"
  printf 'apiVersion: v2\nname: keystone-operator\n%s\nappVersion: "0.1.0"\n' "$1" \
    >"$tree/operators/keystone/helm/keystone-operator/Chart.yaml"
  : >"$tree/github_output"
  script=$(job_step merge-operator-images "Read the chart version" |
    sed -n '/^        run: |$/,$p' | sed '1d; s/^          //')
  RC=0
  OUTPUT=$(
    cd "$tree" && OPERATOR=keystone GITHUB_OUTPUT="$tree/github_output" \
      bash --noprofile --norc -eo pipefail -c "$script" 2>&1
  ) || RC=$?
  WRITTEN=$(cat "$tree/github_output")
}

@test "the chart version step writes the Chart.yaml version and only warns on anything else" {
  local form
  for form in 'version: 0.14.0' 'version: "0.14.0"'; do
    run_chart_version "$form"
    [ "$RC" -eq 0 ] || echo "$OUTPUT"
    soft assert_eq "'$form' exits 0" "0" "$RC"
    soft assert_eq "'$form' writes version=0.14.0" "version=0.14.0" "$WRITTEN"
  done

  # A failed step would stop the one imagetools create that writes every tag.
  for form in 'version: abc' 'version: 0.7.0-rc.1'; do
    run_chart_version "$form"
    soft assert_eq "'$form' exits 0" "0" "$RC"
    soft assert_contains "'$form' warns with the chart and the value" "$OUTPUT" \
      "::warning::operators/keystone/helm/keystone-operator/Chart.yaml has no X.Y.Z version (got: '${form#version: }'); the image gets no chart-version tag"
    soft assert_eq "'$form' writes no version" "" "$WRITTEN"
  done

  run_chart_version 'description: no version line'
  assert_eq "a Chart.yaml without a version line exits 0" "0" "$RC"
  assert_contains "it warns with the empty value" "$OUTPUT" "has no X.Y.Z version (got: '')"
  assert_eq "it writes no version" "" "$WRITTEN"
}

@test "verify-codegen runs the Nix-free check of the operator vendor hashes" {
  assert_contains "verify-codegen runs make nix-lock-check-operators" \
    "$(job_step verify-codegen "Verify the operator vendor hashes")" "run: make nix-lock-check-operators"
  assert_not_contains "verify-codegen installs no Nix" "$(job_block verify-codegen)" "setup-nix"
  assert_eq "the target runs the generator's --check-operators outside the lock-tools shell" \
    "bash hack/nix-lock.sh --check-operators" \
    "$(make --no-print-directory -n -C "$PROJECT_ROOT" nix-lock-check-operators)"
}
