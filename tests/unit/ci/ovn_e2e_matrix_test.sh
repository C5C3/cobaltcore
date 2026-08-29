#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the ovn operator reaches the e2e-operators matrix and the helm filter
# in .github/workflows/ci.yaml, and that build-e2e-images builds both of its
# images.
#
# Every signal here fails silently when it is missing. ALL_OPERATORS is the sole
# source of the e2e-operators matrix, so an operator absent from it never
# produces a leg and its Chainsaw suites under tests/e2e/ovn/ are lint-checked
# and never applied to a cluster. The helm filter is the sole gate on
# helm-validate, so a PR touching only operators/ovn/helm/ renders, lints and
# unit-tests nothing. build-e2e-images is the sole producer of the run-tagged
# images the E2E jobs pull, so an image it skips fails those jobs an hour into
# the run.
#
# Usage: bash tests/unit/ci/ovn_e2e_matrix_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_resolve.sh
source "$PROJECT_ROOT/tests/lib/ci_resolve.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Echo the paths-filter block of the given filter key. Filter keys sit at
# 12-space indent and their entries deeper, so the next key at that indent ends
# the block. Scoping matters for shared entries such as operators/Dockerfile,
# which every operator filter lists.
filter_block() {
  awk -v key="            $1:" '
    $0 == key { in_block = 1; next }
    in_block && /^            [a-z0-9_]+:$/ { exit }
    in_block { print }
  ' "$CI_YAML"
}

# Echo the body of the top-level ci.yaml job <name>. The block ends at the next
# 2-space line that is not part of the body — the next job key, or the comment
# header introducing it.
job_block() {
  awk -v key="  $1:" '
    $0 == key { in_block = 1; next }
    in_block && /^  [#a-z0-9-]/ { exit }
    in_block { print }
  ' "$CI_YAML"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_all_operators_lists_ovn() {
  echo "Test: ci.yaml ALL_OPERATORS includes ovn"

  local line
  line=$(grep "ALL_OPERATORS:" "$CI_YAML" | head -1)
  assert_contains "ALL_OPERATORS lists ovn" "$line" "ovn"
  assert_contains "ALL_OPERATORS still lists keystone" "$line" "keystone"
}

test_ovn_filter_is_wired() {
  echo "Test: the ovn paths filter reaches the resolve step"

  assert_file_contains "the paths filter exists" "$CI_YAML" "^ *ovn:$"
  assert_file_contains "the filter is passed to the resolve step" "$CI_YAML" \
    "FILTER_ovn: \${{ steps.filter.outputs.ovn }}"

  local block
  block=$(filter_block ovn)
  assert_contains "the operator source path is covered" "$block" "operators/ovn/**"

  # The images live in a filter of their own: rebuilding the OVS/OVN daemons or
  # the backup shifter runs the ovn e2e leg without pulling the operator's Go
  # gates in with them.
  assert_file_contains "the image filter exists" "$CI_YAML" "^ *image_ovn:$"
  assert_file_contains "the image filter is passed to the resolve step" "$CI_YAML" \
    "FILTER_image_ovn: \${{ steps.filter.outputs.image_ovn }}"
  block=$(filter_block image_ovn)
  assert_contains "the OVN image is covered" "$block" "images/ovn/**"
  assert_contains "the backup-shifter image is covered" "$block" "images/backup-shifter/**"
}

test_ovn_change_produces_an_e2e_leg() {
  echo "Test: an operators/ovn change puts ovn in the e2e-operators matrix"

  local all_operators="keystone c5c3 horizon glance placement barbican ovn"
  local matrix
  matrix=$(resolve_output e2e-operators refs/heads/main "$all_operators" FILTER_ovn=true)

  assert_contains "the matrix carries the ovn leg" "$matrix" '"ovn"'
  assert_contains "the matrix keeps the operator axis" "$matrix" '"operator"'
  assert_not_contains "the sentinel is gone once an operator changed" \
    "$matrix" "__none__"
}

test_helm_filter_covers_the_ovn_chart() {
  echo "Test: an ovn chart change re-runs helm-validate"

  # The helm filter is the operators/*/helm/** glob; the chart directory
  # under it is what makes the glob cover this operator.
  assert_contains "the helm filter covers every operators/<op>/helm/ tree" \
    "$(filter_block helm)" "operators/*/helm/**"
  assert_eq "the ovn chart lives under that glob" "yes" \
    "$([ -d "$PROJECT_ROOT/operators/ovn/helm/ovn-operator" ] && echo yes || echo no)"
}

test_helm_validate_renders_the_ovn_chart() {
  echo "Test: helm-validate lints, templates and unit-tests the ovn chart"

  # The three `for chart in ...` loops of helm-validate — lint, template and
  # unittest — iterate the operators/*/helm/*-operator glob, so the chart is
  # covered by living in that layout (asserted above).
  local loops
  loops=$(grep -cF 'for chart in operators/*/helm/*-operator' "$CI_YAML")
  assert_eq "the lint, template and unittest loops iterate the chart glob" "3" "$loops"
}

test_build_e2e_images_builds_the_ovn_images() {
  echo "Test: build-e2e-images reaches both ovn images on every run"

  # The ovn-operator image and the OVN daemon image are consumed by jobs whose
  # triggers have nothing to do with an operators/ovn change, so neither may
  # sit behind a copy of another job's `if:`.
  local job
  job=$(job_block build-e2e-images)

  # The operator image is one of the keys hack/ci-resolve-e2e-images.sh derives
  # from the tree — every operators/<op>/ with a go.mod gets a map entry, built
  # when its own sources changed and reused from main's digest otherwise — so a
  # consumer always has an ovn-operator:dev to pull.
  assert_contains "the job resolves its images through the shared script" "$job" \
    "hack/ci-resolve-e2e-images.sh"
  assert_eq "the ovn operator is one of the tree-derived image keys" "yes" \
    "$([ -f "$PROJECT_ROOT/operators/ovn/go.mod" ] && echo yes || echo no)"

  # The daemon image carries no OpenStack release and no go.mod, so nothing
  # derives it and the map never names it. It keeps a build step of its own,
  # which runs on every run of this job; load-e2e-images falls back to the
  # run-scoped tag for a reference the map does not carry.
  assert_contains "the daemon image has its own build step" "$job" \
    "name: Build OVN image"
  assert_contains "the step resolves the pin instead of repeating it" "$job" \
    "hack/ci-resolve-ovn-version.sh"
  assert_contains "the step builds through the shared script" "$job" \
    "hack/ci-build-ovn-image.sh"
  assert_contains "the daemon image is pushed under the run tag" "$job" \
    'push_image "${IMAGE_PREFIX}/ovn:${OVN_VERSION}"'

  # images/ovn/Dockerfile holds the pin. A copy in the workflow would keep
  # pointing at the old tag the next time the Dockerfile is bumped.
  local pin
  pin=$("$PROJECT_ROOT/hack/ci-resolve-ovn-version.sh")
  assert_eq "the workflow never spells the pinned version" "0" \
    "$(grep -cF "$pin" "$CI_YAML")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_all_operators_lists_ovn
test_ovn_filter_is_wired
test_ovn_change_produces_an_e2e_leg
test_helm_filter_covers_the_ovn_chart
test_helm_validate_renders_the_ovn_chart
test_build_e2e_images_builds_the_ovn_images

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
