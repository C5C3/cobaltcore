#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-resolve-e2e-images.sh splits the E2E images into the ones a
# pull request has to build and the ones it can take from what main published,
# and that it hands both to the job in the shapes the workflow reads:
#   - the key set covers every image a consumer can name, derived from the tree
#     rather than listed in the workflow;
#   - a changed operator, service, Tempest or proxy source lands in the build
#     set and maps to the run-scoped tag the push step writes;
#   - everything else maps to the index digest behind its published tag;
#   - a source that has never been published is built instead of failing the
#     run, which is what a new operator looks like before its first merge.
#
# The registry is stubbed through IMAGE_INSPECT_CMD, so the script itself runs
# for real against the real tree; nothing here reimplements its decisions.
#
# Follows the project-native bash test pattern (tests/lib/assertions.sh),
# mirroring tests/unit/hack/ci_generate_cleanup_matrix_test.sh.
#
# Usage: bash tests/unit/hack/ci_resolve_e2e_images_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
RESOLVE_SH="$PROJECT_ROOT/hack/ci-resolve-e2e-images.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

STUB_DIR="$TMP_DIR/bin"
INSPECT_LOG="$TMP_DIR/inspect.log"
ENV_FILE="$TMP_DIR/github_env"
OUT_FILE="$TMP_DIR/github_output"

DIGEST_RE='^ghcr\.io/c5c3/[a-z0-9-]+@sha256:[0-9a-f]{64}$'

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_inspect_stub <dir>
# A recording stand-in for `docker buildx imagetools inspect`: it appends the
# reference it was asked about to $INSPECT_LOG and answers with a digest derived
# from that reference, so each source resolves to a distinct, stable value.
# Steered from the test through the environment:
#   STUB_MISSING    — space-separated refs that answer "not found" on stderr
#   STUB_FLAKY      — space-separated refs that fail with a transient error
#   STUB_BAD_DIGEST — when non-empty, answer with something that is not a digest
make_inspect_stub() {
  local dir="$1"
  mkdir -p "$dir"

  cat >"$dir/inspect" <<'STUB'
#!/bin/bash
echo "$1" >>"$INSPECT_LOG"
case " ${STUB_MISSING:-} " in *" $1 "*) echo "ERROR: $1: not found" >&2; exit 1 ;; esac
case " ${STUB_FLAKY:-} " in *" $1 "*) echo "dial tcp: i/o timeout" >&2; exit 1 ;; esac
if [ -n "${STUB_BAD_DIGEST:-}" ]; then
  echo '"not-a-digest"'
  exit 0
fi
if command -v sha256sum >/dev/null 2>&1; then
  hash="$(printf '%s' "$1" | sha256sum | cut -d' ' -f1)"
else
  hash="$(printf '%s' "$1" | shasum -a 256 | cut -d' ' -f1)"
fi
printf '"sha256:%s"\n' "$hash"
STUB
  chmod +x "$dir/inspect"
}

# run_resolve [VAR=value ...]
# Runs the resolver against the real tree with the stub registry and a fresh
# log. Stores the combined stdout/stderr in OUTPUT, the exit status in RC and
# the image-map JSON in MAP; the GITHUB_ENV lines stay in $ENV_FILE for
# env_block and env_value.
run_resolve() {
  RC=0
  : >"$INSPECT_LOG"
  : >"$ENV_FILE"
  : >"$OUT_FILE"
  OUTPUT="$(
    for assignment in "$@"; do
      export "${assignment?}"
    done
    INSPECT_LOG="$INSPECT_LOG" \
      IMAGE_INSPECT_CMD="$STUB_DIR/inspect" \
      IMAGE_PREFIX="ghcr.io/c5c3" \
      RUN_TAG="e2e-test" \
      INSPECT_RETRY_DELAY=0 \
      GITHUB_ENV="$ENV_FILE" \
      GITHUB_OUTPUT="$OUT_FILE" \
      bash "$RESOLVE_SH" 2>&1
  )" || RC=$?
  MAP="$(sed -n 's/^image-map=//p' "$OUT_FILE")"
}

# env_block <NAME> — the lines of that GITHUB_ENV heredoc block, space-joined.
env_block() {
  sed -n "/^$1<<EOF$/,/^EOF$/p" "$ENV_FILE" | sed '1d;$d' | tr '\n' ' ' |
    sed 's/ *$//'
}

# env_value <NAME> — the value of that GITHUB_ENV "NAME=value" line.
env_value() {
  sed -n "s/^$1=//p" "$ENV_FILE"
}

# map_value <key> — what the image map says to pull for that canonical ref.
map_value() {
  jq -r --arg key "$1" '.[$key] // "<missing>"' <<<"$MAP"
}

# attempts_on <ref> — how many times the stub was asked about that reference.
attempts_on() {
  grep -cx -- "$1" "$INSPECT_LOG"
}

# expected_keys — the image set derived from the tree, one canonical ref per
# line. The resolver has to cover exactly this, so a new operator or release
# directory extends both sides at once.
expected_keys() {
  local dir op release
  for dir in "$PROJECT_ROOT"/operators/*/; do
    [ -f "${dir}go.mod" ] || continue
    op="$(basename "$dir")"
    echo "ghcr.io/c5c3/${op}-operator:dev"
    while read -r release; do
      [ -n "$release" ] || continue
      echo "ghcr.io/c5c3/${op}:${release}"
    done < <(OPERATOR="$op" "$PROJECT_ROOT/hack/ci-service-image-releases.sh")
  done
  for dir in "$PROJECT_ROOT"/releases/*/; do
    echo "ghcr.io/c5c3/tempest:$(basename "$dir")"
  done
  echo "ghcr.io/c5c3/keystone-federation-proxy:dev"
}

# ---------------------------------------------------------------------------
# Test 1: with nothing changed, every image comes from main
# ---------------------------------------------------------------------------
test_empty_inputs_reuse_every_image() {
  echo "Test: unset, empty, [] and the __none__ sentinel all mean 'build nothing'"

  # The resolver spells "none" four ways depending on which path produced it, and
  # a form read as a name would build an image called "[]".
  local form
  for form in "UNSET" "" "[]" '["__none__"]'; do
    if [ "$form" = "UNSET" ]; then
      run_resolve
    else
      run_resolve CHANGED_OPERATORS="$form" CHANGED_SERVICES="$form"
    fi

    local label="${form:-<empty>}"
    assert_eq "form $label: the resolver exits 0" "0" "$RC"
    assert_eq "form $label: no operator image is built" "" "$(env_block BUILD_OPERATORS)"
    assert_eq "form $label: no service image is built" "" "$(env_block BUILD_SERVICE_IMAGES)"
    assert_eq "form $label: no Tempest image is built" "" "$(env_block BUILD_TEMPEST_RELEASES)"
    assert_eq "form $label: the proxy is not built" "false" "$(env_value BUILD_PROXY)"
    assert_eq "form $label: the base images are not needed" "false" \
      "$(env_value NEEDS_BASE_IMAGES)"
    assert_eq "form $label: every image resolves to a digest" "true" \
      "$(jq -r --arg re "$DIGEST_RE" '[.[] | test($re)] | all' <<<"$MAP")"
  done
}

# ---------------------------------------------------------------------------
# Test 2: the key set is the tree's image set
# ---------------------------------------------------------------------------
test_the_key_set_covers_every_image_in_the_tree() {
  echo "Test: the map has one key per image a consumer can name"

  run_resolve

  assert_eq "the map's keys are the tree's images" \
    "$(expected_keys | sort)" "$(jq -r 'keys[]' <<<"$MAP" | sort)"

  # Eight operators, six services across two releases, two Tempest images and
  # the federation proxy. The number moves with the tree; the equality above is
  # what keeps it honest.
  assert_eq "the tree yields 23 images today" "23" "$(jq -r 'length' <<<"$MAP")"

  assert_eq "an operator image is keyed by its dev tag" "true" \
    "$(jq 'has("ghcr.io/c5c3/keystone-operator:dev")' <<<"$MAP")"
  assert_eq "a service image is keyed by its release" "true" \
    "$(jq 'has("ghcr.io/c5c3/glance:2025.2")' <<<"$MAP")"
  assert_eq "a Tempest image is keyed by its release" "true" \
    "$(jq 'has("ghcr.io/c5c3/tempest:2026.1")' <<<"$MAP")"
  assert_eq "the federation proxy is in the map" "true" \
    "$(jq 'has("ghcr.io/c5c3/keystone-federation-proxy:dev")' <<<"$MAP")"

  # c5c3 and ovn ship no service image, so they contribute an operator image and
  # nothing else.
  assert_eq "an operator without a service image contributes no service key" "0" \
    "$(jq -r '[keys[] | select(startswith("ghcr.io/c5c3/c5c3:"))] | length' <<<"$MAP")"

  # One lookup per image, and no image looked up twice: 23 round trips to the
  # registry is already the bulk of this step's runtime on a no-build run.
  assert_eq "every image is looked up" "$(jq -r 'length' <<<"$MAP")" \
    "$(grep -c . "$INSPECT_LOG")"
  assert_eq "no source is looked up twice" "$(grep -c . "$INSPECT_LOG")" \
    "$(sort -u "$INSPECT_LOG" | grep -c .)"
}

# ---------------------------------------------------------------------------
# Test 3: a changed operator builds one image and reuses the rest
# ---------------------------------------------------------------------------
test_a_changed_operator_builds_only_its_own_image() {
  echo "Test: a changed operator builds its operator image, nothing else"

  run_resolve CHANGED_OPERATORS='["glance"]'

  assert_eq "the resolver exits 0" "0" "$RC"
  assert_eq "only glance is built" "glance" "$(env_block BUILD_OPERATORS)"
  assert_eq "the glance operator image carries the run-scoped tag" \
    "ghcr.io/c5c3/glance-operator:e2e-test-dev" \
    "$(map_value ghcr.io/c5c3/glance-operator:dev)"
  assert_contains "an unchanged operator image is pulled by digest" \
    "$(map_value ghcr.io/c5c3/keystone-operator:dev)" \
    "ghcr.io/c5c3/keystone-operator@sha256:"

  # Operator code and service-image sources are separate change classes: a Go
  # change must not rebuild the twelve OpenStack service images.
  assert_eq "the glance service images are not rebuilt" "" \
    "$(env_block BUILD_SERVICE_IMAGES)"
  assert_contains "the glance service image is pulled by digest" \
    "$(map_value ghcr.io/c5c3/glance:2025.2)" "ghcr.io/c5c3/glance@sha256:"
  assert_eq "the base images are not needed" "false" "$(env_value NEEDS_BASE_IMAGES)"
}

# ---------------------------------------------------------------------------
# Test 4: a changed service builds every release of it
# ---------------------------------------------------------------------------
test_a_changed_service_builds_all_its_releases() {
  echo "Test: a changed service image builds every release, and needs the base images"

  run_resolve CHANGED_SERVICES='["glance"]'

  assert_eq "the resolver exits 0" "0" "$RC"
  # The resolver's images_base class marks a service, never a single release, so
  # both releases build.
  assert_eq "both glance releases are built" "glance 2025.2 glance 2026.1" \
    "$(env_block BUILD_SERVICE_IMAGES)"
  assert_eq "the 2025.2 image carries the run-scoped tag" \
    "ghcr.io/c5c3/glance:e2e-test-2025.2" "$(map_value ghcr.io/c5c3/glance:2025.2)"
  assert_eq "the 2026.1 image carries the run-scoped tag" \
    "ghcr.io/c5c3/glance:e2e-test-2026.1" "$(map_value ghcr.io/c5c3/glance:2026.1)"
  # Service images build FROM venv-builder and python-base.
  assert_eq "the base images are needed" "true" "$(env_value NEEDS_BASE_IMAGES)"
  assert_eq "the glance operator image is not built" "" "$(env_block BUILD_OPERATORS)"
}

# ---------------------------------------------------------------------------
# Test 5 and 6: Tempest and the federation proxy
# ---------------------------------------------------------------------------
test_changed_tempest_builds_every_release() {
  echo "Test: a changed Tempest source builds every release"

  run_resolve CHANGED_TEMPEST=true

  assert_eq "the resolver exits 0" "0" "$RC"
  assert_eq "both Tempest releases are built" "2025.2 2026.1" \
    "$(env_block BUILD_TEMPEST_RELEASES)"
  assert_eq "the 2025.2 Tempest image carries the run-scoped tag" \
    "ghcr.io/c5c3/tempest:e2e-test-2025.2" "$(map_value ghcr.io/c5c3/tempest:2025.2)"
  # images/tempest/Dockerfile builds FROM venv-builder and python-base, and
  # ci-build-tempest-image.sh has no fallback that builds them itself.
  assert_eq "the base images are needed" "true" "$(env_value NEEDS_BASE_IMAGES)"
}

test_a_changed_proxy_builds_only_the_proxy() {
  echo "Test: a changed federation proxy builds the proxy and needs no base image"

  run_resolve CHANGED_PROXY=true

  assert_eq "the resolver exits 0" "0" "$RC"
  assert_eq "the proxy is built" "true" "$(env_value BUILD_PROXY)"
  assert_eq "the proxy carries the run-scoped tag" \
    "ghcr.io/c5c3/keystone-federation-proxy:e2e-test-dev" \
    "$(map_value ghcr.io/c5c3/keystone-federation-proxy:dev)"
  # The proxy builds FROM ubuntu:noble, so nothing else has to be built for it.
  assert_eq "the base images are not needed" "false" "$(env_value NEEDS_BASE_IMAGES)"
  assert_eq "no operator image is built" "" "$(env_block BUILD_OPERATORS)"
}

# ---------------------------------------------------------------------------
# Test 7: a source that main has never published
# ---------------------------------------------------------------------------
test_an_unpublished_source_is_built_instead() {
  echo "Test: a source that is not published yet is built rather than fatal"

  # The state of a new operator, service or release before its first merge to
  # main. Failing here would block the pull request that introduces it.
  run_resolve STUB_MISSING="ghcr.io/c5c3/glance-operator:latest"

  assert_eq "the resolver exits 0" "0" "$RC"
  assert_contains "it says why the image is being built" "$OUTPUT" \
    "::notice::ghcr.io/c5c3/glance-operator:latest is not published yet; building ghcr.io/c5c3/glance-operator:dev"
  assert_eq "the image moves into the build set" "glance" "$(env_block BUILD_OPERATORS)"
  assert_eq "the image carries the run-scoped tag" \
    "ghcr.io/c5c3/glance-operator:e2e-test-dev" \
    "$(map_value ghcr.io/c5c3/glance-operator:dev)"
  # A missing tag is an answer, not a flake, so the retry loop stays out of it.
  assert_eq "the missing source is asked about exactly once" "1" \
    "$(attempts_on ghcr.io/c5c3/glance-operator:latest)"
}

# ---------------------------------------------------------------------------
# Test 8: a registry that keeps failing
# ---------------------------------------------------------------------------
test_a_failing_registry_is_retried_then_fatal() {
  echo "Test: any other inspect failure is retried three times, then fails the step"

  run_resolve STUB_FLAKY="ghcr.io/c5c3/keystone-operator:latest"

  assert_nonzero_exit "the resolver fails the step" "$RC"
  assert_eq "the source is asked about three times" "3" \
    "$(attempts_on ghcr.io/c5c3/keystone-operator:latest)"
  assert_contains "it warns between the attempts" "$OUTPUT" \
    "::warning::inspect of ghcr.io/c5c3/keystone-operator:latest failed (attempt 1/3)"
  assert_contains "it names the source it could not resolve" "$OUTPUT" \
    "::error::cannot resolve ghcr.io/c5c3/keystone-operator:latest after 3 attempts"
}

# ---------------------------------------------------------------------------
# Test 9-11: answers and inputs that are not what they claim to be
# ---------------------------------------------------------------------------
test_a_non_digest_answer_is_fatal() {
  echo "Test: an answer that is not a digest fails rather than reaching a consumer"

  # A map value that is not a digest would fail an hour later in the consumer's
  # docker pull, with nothing pointing back here.
  run_resolve STUB_BAD_DIGEST=1

  assert_nonzero_exit "the resolver fails the step" "$RC"
  assert_contains "it quotes what the registry answered" "$OUTPUT" \
    "::error::unexpected digest for ghcr.io/c5c3/barbican-operator:latest: not-a-digest"
}

test_a_malformed_changed_list_is_fatal() {
  echo "Test: a CHANGED_* value that is not a JSON array fails the step"

  # These come from the resolver, so a malformed one is a wiring bug rather than
  # user input, and silently reading it as "nothing changed" would ship a run
  # that builds none of the images it needs.
  run_resolve CHANGED_OPERATORS='not json'
  assert_nonzero_exit "a non-JSON operator list fails the step" "$RC"
  assert_contains "it names the variable" "$OUTPUT" \
    "::error::CHANGED_OPERATORS is not a JSON array"

  run_resolve CHANGED_SERVICES='{"glance":true}'
  assert_nonzero_exit "a JSON object instead of an array fails the step" "$RC"
  assert_contains "it names the variable" "$OUTPUT" \
    "::error::CHANGED_SERVICES is not a JSON array"
}

test_a_name_with_no_image_is_ignored() {
  echo "Test: a changed name with no image of that kind is reported and skipped"

  # ovn is an operator, but it ships no OpenStack service image, so it has no
  # service key to build.
  run_resolve CHANGED_SERVICES='["ovn"]'

  assert_eq "the resolver exits 0" "0" "$RC"
  assert_contains "it says which name it ignored" "$OUTPUT" "no e2e image for ovn; ignored"
  assert_eq "nothing is built for it" "" "$(env_block BUILD_SERVICE_IMAGES)"

  run_resolve CHANGED_OPERATORS='["nosuchoperator"]'

  assert_eq "an unknown operator name also exits 0" "0" "$RC"
  assert_contains "it says which name it ignored" "$OUTPUT" \
    "no e2e image for nosuchoperator; ignored"
  assert_eq "no operator image is built for it" "" "$(env_block BUILD_OPERATORS)"
}

# ---------------------------------------------------------------------------
# Test 12: the required environment
# ---------------------------------------------------------------------------
test_missing_required_env_fails_loudly() {
  echo "Test: the resolver refuses to guess its registry, tag or output files"

  # Without RUN_TAG it would push and map an image called ":-dev".
  local rc=0
  env -u RUN_TAG \
    IMAGE_INSPECT_CMD="$STUB_DIR/inspect" \
    IMAGE_PREFIX="ghcr.io/c5c3" \
    INSPECT_LOG="$INSPECT_LOG" \
    GITHUB_ENV="$ENV_FILE" \
    GITHUB_OUTPUT="$OUT_FILE" \
    bash "$RESOLVE_SH" >/dev/null 2>&1 || rc=$?
  assert_nonzero_exit "an unset RUN_TAG fails loudly" "$rc"

  rc=0
  env -u IMAGE_PREFIX \
    IMAGE_INSPECT_CMD="$STUB_DIR/inspect" \
    RUN_TAG="e2e-test" \
    INSPECT_LOG="$INSPECT_LOG" \
    GITHUB_ENV="$ENV_FILE" \
    GITHUB_OUTPUT="$OUT_FILE" \
    bash "$RESOLVE_SH" >/dev/null 2>&1 || rc=$?
  assert_nonzero_exit "an unset IMAGE_PREFIX fails loudly" "$rc"
}

# ---------------------------------------------------------------------------
# Test 13: the shapes the workflow reads
# ---------------------------------------------------------------------------
test_the_outputs_have_the_shapes_the_workflow_reads() {
  echo "Test: image-map is one line of JSON and the env blocks are heredocs"

  run_resolve CHANGED_OPERATORS='["glance"]' CHANGED_TEMPEST=true

  # A multi-line job output is truncated at the first newline, which would hand
  # every consumer a map they cannot parse.
  assert_eq "GITHUB_OUTPUT carries exactly one line" "1" "$(wc -l <"$OUT_FILE" | tr -d ' ')"
  assert_eq "the map parses as a JSON object" "true" "$(jq -e 'type == "object"' <<<"$MAP")"

  local block
  for block in BUILD_OPERATORS BUILD_SERVICE_IMAGES BUILD_TEMPEST_RELEASES; do
    assert_file_contains "$block is written as a heredoc block" "$ENV_FILE" "^$block<<EOF$"
  done
  assert_file_contains "BUILD_PROXY is written as a plain assignment" "$ENV_FILE" \
    "^BUILD_PROXY=false$"
  assert_file_contains "NEEDS_BASE_IMAGES is written as a plain assignment" "$ENV_FILE" \
    "^NEEDS_BASE_IMAGES=true$"

  # The job log has to show every decision; a wrong image in a consumer is
  # otherwise invisible until the deploy fails.
  assert_contains "the decisions are logged in a group" "$OUTPUT" "::group::Image map"
  assert_contains "a built image says so" "$OUTPUT" \
    "ghcr.io/c5c3/glance-operator:dev -> ghcr.io/c5c3/glance-operator:e2e-test-dev (built)"
  assert_contains "a reused image says so" "$OUTPUT" \
    "ghcr.io/c5c3/keystone-operator:dev -> ghcr.io/c5c3/keystone-operator@sha256:"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
if ! command -v jq >/dev/null 2>&1 || ! command -v yq >/dev/null 2>&1; then
  echo "SKIP: jq or yq not installed (all checks skipped)"
  echo ""
  echo "Results: 0 passed, 0 failed, 1 skipped"
  exit 0
fi

make_inspect_stub "$STUB_DIR"

test_empty_inputs_reuse_every_image
test_the_key_set_covers_every_image_in_the_tree
test_a_changed_operator_builds_only_its_own_image
test_a_changed_service_builds_all_its_releases
test_changed_tempest_builds_every_release
test_a_changed_proxy_builds_only_the_proxy
test_an_unpublished_source_is_built_instead
test_a_failing_registry_is_retried_then_fatal
test_a_non_digest_answer_is_fatal
test_a_malformed_changed_list_is_fatal
test_a_name_with_no_image_is_ignored
test_missing_required_env_fails_loudly
test_the_outputs_have_the_shapes_the_workflow_reads

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
