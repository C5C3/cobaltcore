#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the `Pull and re-tag E2E images` step of the load-e2e-images composite
# action resolves each canonical ref through the build job's image-map, and
# falls back to the run-scoped tag for anything the map does not carry.
#
# The map is what lets build-e2e-images stop rebuilding images main already
# published: an image this run built is pulled from its run-scoped tag as
# before, while an image it reused is pulled by digest. Both end up under the
# canonical local ref the `kind load docker-image` steps name, so a mistake here
# surfaces an hour later as a deploy pulling the wrong image rather than as a
# failure in this step.
#
# The step's shell body is extracted from action.yaml and executed against a
# recording docker stub, so this exercises the real snippet rather than a copy.
#
# The map reaches the action only through each consumer's with: block. A load
# step without its image-map: line runs with an empty map and pulls every image
# from its run-scoped tag, which a reused image does not have, so that job fails
# on its first pull of a reused image. The consumer check reads ci.yaml alone and
# runs without jq.
#
# Usage: bash tests/unit/ci/load_e2e_images_map_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
ACTION_YAML="$PROJECT_ROOT/.github/actions/load-e2e-images/action.yaml"
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
DOCKER_LOG="$TMP_DIR/docker.log"
STEP_SH="$TMP_DIR/step.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Extract the `run:` body of the pull step into a runnable script at $1. A
# composite action nests two columns further left than a workflow job: the step
# sits at 4-space indent, its keys at 6 and the body at 8, so the first non-blank
# line with less indentation ends the block.
extract_pull_step() {
  local out="$1"
  awk '
    /^    - name: Pull and re-tag E2E images$/ { in_step = 1; next }
    in_step && /^      run: \|$/               { in_run = 1; next }
    in_run {
      if ($0 == "") { print ""; next }
      if ($0 !~ /^        /) { exit }
      print substr($0, 9)
    }
  ' "$ACTION_YAML" >"$out"
}

# make_docker_stub <dir>
# A recording docker stub: it appends its argv to $DOCKER_LOG and succeeds.
make_docker_stub() {
  local dir="$1"
  mkdir -p "$dir"

  cat >"$dir/docker" <<'STUB'
#!/bin/bash
echo "docker $*" >>"$DOCKER_LOG"
exit 0
STUB
  chmod +x "$dir/docker"
}

# run_step <images> <image-map> [operator-variant]
# Runs the extracted step with the docker stub prepended to PATH (so jq and
# coreutils still resolve) and a fresh log. Stores the combined stdout/stderr in
# OUTPUT, the exit status in RC and the recorded argv in LOG.
run_step() {
  local images="$1" image_map="$2" variant="${3:-}"
  RC=0
  : >"$DOCKER_LOG"
  OUTPUT="$(
    PATH="$STUB_DIR:$PATH" \
      DOCKER_LOG="$DOCKER_LOG" \
      IMAGES="$images" \
      RUN_ID="12345" \
      IMAGE_MAP="$image_map" \
      OPERATOR_VARIANT="$variant" \
      bash "$STEP_SH" 2>&1
  )" || RC=$?
  LOG="$(cat "$DOCKER_LOG")"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_extraction_finds_the_step() {
  echo "Test: the pull step's body is extractable from action.yaml"

  extract_pull_step "$STEP_SH"
  assert_file_contains "the extracted body pulls images" "$STEP_SH" "docker pull"
  assert_file_contains "the extracted body re-tags them" "$STEP_SH" "docker tag"
  assert_file_contains "the extracted body reads the map" "$STEP_SH" "IMAGE_MAP"
}

test_a_mapped_ref_is_pulled_by_digest() {
  echo "Test: a ref the map carries is pulled from the mapped reference"

  # This is the whole point of the map: the image was published by main, so the
  # run pulls the digest behind it instead of rebuilding and pushing it.
  local digest="ghcr.io/c5c3/keystone@sha256:1111111111111111111111111111111111111111111111111111111111111111"
  run_step "ghcr.io/c5c3/keystone:2025.2" \
    "{\"ghcr.io/c5c3/keystone:2025.2\": \"${digest}\"}"

  assert_eq "the step succeeds" "0" "$RC"
  assert_contains "it pulls the mapped digest" "$LOG" "docker pull ${digest}"
  assert_contains "it re-tags it to the canonical ref" "$LOG" \
    "docker tag ${digest} ghcr.io/c5c3/keystone:2025.2"
  assert_not_contains "it does not touch the run-scoped tag" "$LOG" "e2e-12345"
}

test_a_built_ref_is_pulled_from_the_run_tag() {
  echo "Test: a ref the map points at the run-scoped tag is pulled from there"

  local built="ghcr.io/c5c3/glance-operator:e2e-12345-dev"
  run_step "ghcr.io/c5c3/glance-operator:dev" \
    "{\"ghcr.io/c5c3/glance-operator:dev\": \"${built}\"}"

  assert_eq "the step succeeds" "0" "$RC"
  assert_contains "it pulls the run-scoped tag" "$LOG" "docker pull ${built}"
  assert_contains "it re-tags it to the canonical ref" "$LOG" \
    "docker tag ${built} ghcr.io/c5c3/glance-operator:dev"
}

test_a_ref_the_map_misses_falls_back() {
  echo "Test: a ref the map does not carry falls back to the run-scoped tag"

  # A consumer may name an image the build job never heard of, and an image the
  # job pushed under the run-scoped tag is still loadable that way.
  run_step "ghcr.io/c5c3/ovn:24.09" '{"ghcr.io/c5c3/keystone:2025.2": "ghcr.io/c5c3/keystone@sha256:abc"}'

  assert_eq "the step succeeds" "0" "$RC"
  assert_contains "it says the map had no entry" "$OUTPUT" \
    "no image-map entry for ghcr.io/c5c3/ovn:24.09; pulling ghcr.io/c5c3/ovn:e2e-12345-24.09"
  assert_contains "it pulls the run-scoped tag" "$LOG" \
    "docker pull ghcr.io/c5c3/ovn:e2e-12345-24.09"
  assert_contains "it re-tags it to the canonical ref" "$LOG" \
    "docker tag ghcr.io/c5c3/ovn:e2e-12345-24.09 ghcr.io/c5c3/ovn:24.09"
}

test_an_empty_map_keeps_the_old_behaviour() {
  echo "Test: an empty image-map pulls every ref by run-scoped tag"

  # The input is optional, so a caller that passes no map behaves exactly as the
  # action did before the map existed.
  run_step "ghcr.io/c5c3/keystone-operator:dev
ghcr.io/c5c3/keystone:2025.2" ""

  assert_eq "the step succeeds" "0" "$RC"
  assert_contains "the operator image comes from the run-scoped tag" "$LOG" \
    "docker pull ghcr.io/c5c3/keystone-operator:e2e-12345-dev"
  assert_contains "the service image comes from the run-scoped tag" "$LOG" \
    "docker pull ghcr.io/c5c3/keystone:e2e-12345-2025.2"
  assert_not_contains "no lookup is reported" "$OUTPUT" "no image-map entry"
}

test_a_malformed_map_fails_before_pulling() {
  echo "Test: a malformed image-map fails before the first pull"

  # Failing halfway through would leave some images re-tagged and some not,
  # which the consumer only discovers at deploy time.
  run_step "ghcr.io/c5c3/keystone:2025.2" "{"

  assert_nonzero_exit "the step fails" "$RC"
  assert_contains "it says the map is malformed" "$OUTPUT" \
    "::error::image-map is not a JSON object"
  assert_eq "nothing was pulled" "" "$LOG"

  run_step "ghcr.io/c5c3/keystone:2025.2" '["not","an","object"]'

  assert_nonzero_exit "a JSON array is rejected too" "$RC"
  assert_eq "nothing was pulled" "" "$LOG"
}

test_a_ref_without_a_tag_still_fails() {
  echo "Test: a ref with no :tag suffix fails as it did before"

  run_step "ghcr.io/c5c3/keystone" '{"ghcr.io/c5c3/keystone": "ghcr.io/c5c3/keystone@sha256:abc"}'

  assert_nonzero_exit "the step fails" "$RC"
  assert_contains "it names the offending ref" "$OUTPUT" \
    "::error::Image ref 'ghcr.io/c5c3/keystone' has no :tag suffix"
  assert_eq "nothing was pulled" "" "$LOG"
}

test_blank_and_comment_lines_are_skipped() {
  echo "Test: blank lines and comments in the image list are ignored"

  run_step "
# the keystone leg also needs the federation proxy
ghcr.io/c5c3/keystone-operator:dev

" ""

  assert_eq "the step succeeds" "0" "$RC"
  assert_contains "the real ref is pulled" "$LOG" \
    "docker pull ghcr.io/c5c3/keystone-operator:e2e-12345-dev"
  assert_eq "only that one ref is pulled" "2" "$(grep -c . <<<"$LOG")"
}

test_cover_variant_pulls_the_cover_entry() {
  echo "Test: operator-variant cover fills an operator's :dev ref from the map's :cover entry"

  local digest="ghcr.io/c5c3/keystone@sha256:2222222222222222222222222222222222222222222222222222222222222222"
  run_step "ghcr.io/c5c3/keystone-operator:dev
ghcr.io/c5c3/keystone:2026.1" \
    "{\"ghcr.io/c5c3/keystone-operator:cover\": \"ghcr.io/c5c3/keystone-operator:e2e-12345-cover\",
      \"ghcr.io/c5c3/keystone-operator:dev\": \"ghcr.io/c5c3/keystone-operator@sha256:abc\",
      \"ghcr.io/c5c3/keystone:2026.1\": \"${digest}\"}" cover

  assert_eq "the step succeeds" "0" "$RC"
  assert_contains "it pulls the cover image" "$LOG" \
    "docker pull ghcr.io/c5c3/keystone-operator:e2e-12345-cover"
  assert_contains "it tags the cover image as the :dev ref" "$LOG" \
    "docker tag ghcr.io/c5c3/keystone-operator:e2e-12345-cover ghcr.io/c5c3/keystone-operator:dev"
  assert_not_contains "it leaves the production :dev entry alone" "$LOG" \
    "keystone-operator@sha256:abc"
  assert_contains "a service image keeps its own entry" "$LOG" "docker pull ${digest}"
  assert_contains "and its own ref" "$LOG" "docker tag ${digest} ghcr.io/c5c3/keystone:2026.1"
}

test_cover_variant_falls_back_to_the_run_tag() {
  echo "Test: under operator-variant cover a map without the :cover key falls back to e2e-<run>-cover"

  run_step "ghcr.io/c5c3/keystone-operator:dev" \
    '{"ghcr.io/c5c3/keystone-operator:dev": "ghcr.io/c5c3/keystone-operator@sha256:abc"}' cover

  assert_eq "the step succeeds" "0" "$RC"
  assert_contains "it says the map had no cover entry" "$OUTPUT" \
    "no image-map entry for ghcr.io/c5c3/keystone-operator:cover; pulling ghcr.io/c5c3/keystone-operator:e2e-12345-cover"
  assert_contains "it pulls the run-scoped cover tag" "$LOG" \
    "docker pull ghcr.io/c5c3/keystone-operator:e2e-12345-cover"
  assert_contains "it tags it as the :dev ref" "$LOG" \
    "docker tag ghcr.io/c5c3/keystone-operator:e2e-12345-cover ghcr.io/c5c3/keystone-operator:dev"

  run_step "ghcr.io/c5c3/keystone-operator:dev" "" cover
  assert_contains "an empty map pulls the run-scoped cover tag too" "$LOG" \
    "docker pull ghcr.io/c5c3/keystone-operator:e2e-12345-cover"

  # Only an operator's :dev ref has a cover variant.
  run_step "ghcr.io/c5c3/keystone-federation-proxy:dev" "" cover
  assert_contains "a :dev ref that is no operator keeps its own tag" "$LOG" \
    "docker pull ghcr.io/c5c3/keystone-federation-proxy:e2e-12345-dev"
}

test_an_unknown_variant_fails_before_pulling() {
  echo "Test: an operator-variant other than empty or cover fails before the first pull"

  run_step "ghcr.io/c5c3/keystone-operator:dev" "" debug

  assert_nonzero_exit "the step fails" "$RC"
  assert_contains "it names the value" "$OUTPUT" \
    "::error::operator-variant must be empty or cover, got 'debug'"
  assert_eq "nothing was pulled" "" "$LOG"
}

# Echo one line per load-e2e-images consumer in ci.yaml outside
# build-e2e-images: "OK <job> <line>" when the step's image-map: line follows
# before the next step, "MISS <job> <line>" otherwise.
consumer_scan() {
  awk '
    /^jobs:$/ { in_jobs = 1; next }
    !in_jobs { next }
    /^  [a-z0-9-]+:$/ {
      if (pending) print "MISS " pjob " " pending
      pending = 0; job = $0; gsub(/[ :]/, "", job); next
    }
    job == "build-e2e-images" { next }
    /^      (- |  )uses: \.\/\.github\/actions\/load-e2e-images$/ {
      if (pending) print "MISS " pjob " " pending
      pending = NR; pjob = job; next
    }
    pending && /^      - / { print "MISS " pjob " " pending; pending = 0 }
    pending && $0 == "          image-map: ${{ needs.build-e2e-images.outputs.image-map }}" {
      print "OK " pjob " " pending; pending = 0
    }
    END { if (pending) print "MISS " pjob " " pending }
  ' "$CI_YAML"
}

test_every_consumer_passes_the_map() {
  echo "Test: every load-e2e-images consumer passes the build job's image map"

  local scan status job line
  scan=$(consumer_scan)
  while read -r status job line; do
    [ "$status" = "MISS" ] || continue
    echo "  FAIL: $job loads E2E images at ci.yaml:$line without the build job's image-map"
    FAIL=$((FAIL + 1))
  done <<<"$scan"
  # Every consumer line in ci.yaml, whatever its indentation, spacing, quoting,
  # trailing slash or trailing comment, must be one the scan judged; a form the
  # scan does not know would otherwise pass unseen.
  local total
  total=$(grep -cE "^[[:space:]]+(-[[:space:]]+)?uses:[[:space:]]+[\"']?\./\.github/actions/load-e2e-images/?([\"'[:space:]#]|\$)" \
    "$CI_YAML" || true)
  assert_gte "ci.yaml has load-e2e-images consumers" "$total" 1
  assert_eq "the scan judges every load-e2e-images consumer in ci.yaml" "$total" \
    "$(grep -c . <<<"$scan" || true)"

  assert_file_contains_fixed "build-e2e-images exports the map" "$CI_YAML" \
    'image-map: ${{ steps.images.outputs.image-map }}'
  local step
  step=$(job_step build-e2e-images "Resolve images")
  assert_contains "the map comes from the Resolve images step" "$step" "id: images"
  assert_contains "which runs the image resolver" "$step" \
    "run: hack/ci-resolve-e2e-images.sh"
}

# Echo one "<job> <cover|plain>" line per load-e2e-images consumer in ci.yaml
# outside build-e2e-images: cover when the step passes operator-variant: cover
# before the next step begins.
variant_scan() {
  awk '
    /^jobs:$/ { in_jobs = 1; next }
    !in_jobs { next }
    /^  [a-z0-9-]+:$/ {
      if (pending) print pjob " plain"
      pending = 0; job = $0; gsub(/[ :]/, "", job); next
    }
    job == "build-e2e-images" { next }
    /^      (- |  )uses: \.\/\.github\/actions\/load-e2e-images$/ {
      if (pending) print pjob " plain"
      pending = 1; pjob = job; next
    }
    pending && /^      - / { print pjob " plain"; pending = 0 }
    pending && $0 == "          operator-variant: cover" { print pjob " cover"; pending = 0 }
    END { if (pending) print pjob " plain" }
  ' "$CI_YAML"
}

test_the_coverage_legs_pull_the_cover_operator() {
  echo "Test: the load steps of e2e-operator, e2e-chaos and tempest pass operator-variant: cover, no other does"

  # The three coverage jobs deploy the instrumented operators; every other
  # job keeps the production binary that ships.
  local scan
  scan=$(variant_scan)
  assert_eq "e2e-operator has one cover load step" "1" "$(grep -cx 'e2e-operator cover' <<<"$scan")"
  assert_eq "e2e-chaos has two cover load steps" "2" "$(grep -cx 'e2e-chaos cover' <<<"$scan")"
  assert_eq "tempest has three cover load steps" "3" "$(grep -cx 'tempest cover' <<<"$scan")"
  assert_eq "every load step of the three jobs passes it" "0" \
    "$(grep -cE '^(e2e-operator|e2e-chaos|tempest) plain$' <<<"$scan")"
  assert_eq "ci.yaml passes operator-variant: cover six times in all" "6" \
    "$(grep -c '^          operator-variant: cover$' "$CI_YAML")"
  assert_eq "the scan judges every load-e2e-images consumer in ci.yaml" \
    "$(consumer_scan | grep -c .)" "$(grep -c . <<<"$scan")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_every_consumer_passes_the_map
test_the_coverage_legs_pull_the_cover_operator

if ! command -v jq >/dev/null 2>&1; then
  echo "SKIP: jq not installed (the pull-step checks are skipped)"
  SKIP=$((SKIP + 1))
  echo ""
  echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
  if [ "$FAIL" -gt 0 ]; then
    exit 1
  fi
  exit 0
fi

make_docker_stub "$STUB_DIR"
extract_pull_step "$STEP_SH"

test_extraction_finds_the_step
test_a_mapped_ref_is_pulled_by_digest
test_a_built_ref_is_pulled_from_the_run_tag
test_a_ref_the_map_misses_falls_back
test_an_empty_map_keeps_the_old_behaviour
test_a_malformed_map_fails_before_pulling
test_a_ref_without_a_tag_still_fails
test_blank_and_comment_lines_are_skipped
test_cover_variant_pulls_the_cover_entry
test_cover_variant_falls_back_to_the_run_tag
test_an_unknown_variant_fails_before_pulling

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
