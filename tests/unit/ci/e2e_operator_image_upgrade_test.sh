#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify that the image the e2e-operator job retags for the keystone
# image-upgrade suites is the image those suites patch to.
#
# The "Load images into kind" step of e2e-operator tags <operator>:<release>
# as <operator>:<release>-upgraded and loads it into kind, behind a guard that
# the operator ships <release>. The keystone image-upgrade and
# rolling-update-zero-downtime suites start a Keystone on <release> and patch
# its image tag to <release>-upgraded. No registry has that tag, so a fixture
# that names another -upgraded tag than the step loads ends in an
# ImagePullBackOff deep in the keystone leg, and a guard that names a release
# keystone does not ship skips the retag without a word. Each move of the
# default release edits both sides by hand, so this test reads the release out
# of the step and checks the guard, the fixtures and the suites' greps against
# it.
#
# Usage: bash tests/unit/ci/e2e_operator_image_upgrade_test.sh

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

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

load_step() {
  job_step e2e-operator "Load images into kind"
}

# Echo the release the step's guard names, the one it retags.
retagged_release() {
  load_step | sed -n 's/.*| grep -qx \([0-9][0-9.]*\); then$/\1/p'
}

# Echo, one per line, every fixture under the e2e trees that sets an image tag
# ending in -upgraded.
upgrade_patches() {
  (cd "$PROJECT_ROOT" && grep -rlE 'tag: "[^"]*-upgraded"' tests/e2e tests/e2e-*/) | sort
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_the_step_retags_the_release_its_guard_names() {
  echo "Test: the step retags and loads the release its guard names"
  local step release
  step="$(load_step)"
  release="$(retagged_release)"
  assert_not_empty "the guard names a release" "$release"
  assert_contains "the step tags that release's service image" "$step" \
    "docker tag \"\${IMAGE_PREFIX}/\${OPERATOR}:${release}\" \\"
  assert_contains "as <release>-upgraded" "$step" \
    "\"\${IMAGE_PREFIX}/\${OPERATOR}:${release}-upgraded\""
  assert_contains "and loads the retagged image into kind" "$step" \
    "kind load docker-image \"\${IMAGE_PREFIX}/\${OPERATOR}:${release}-upgraded\" --name \"\${KIND_CLUSTER}\""
}

test_keystone_ships_the_retagged_release() {
  echo "Test: keystone ships the retagged release, so the guard does not skip the retag"
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed"
    SKIP=$((SKIP + 1))
    return
  fi
  local release
  release="$(retagged_release)"
  assert_eq "hack/ci-service-image-releases.sh lists ${release} for keystone" "$release" \
    "$(OPERATOR=keystone "$PROJECT_ROOT/hack/ci-service-image-releases.sh" | grep -x -- "$release")"
}

test_every_upgrade_patch_names_the_retagged_image() {
  echo "Test: every -upgraded patch names the retagged image and starts on its release"
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed"
    SKIP=$((SKIP + 1))
    return
  fi
  local release patches patch
  release="$(retagged_release)"
  patches="$(upgrade_patches)"
  # The scan finding nothing would make the loop below pass vacuously.
  assert_eq "the scan finds the two keystone suites" \
    "tests/e2e/keystone/image-upgrade/01-patch-image.yaml
tests/e2e/keystone/rolling-update-zero-downtime/01-patch-image.yaml" "$patches"
  while IFS= read -r patch; do
    [[ -n "$patch" ]] || continue
    assert_eq "$patch patches to the retagged image" "${release}-upgraded" \
      "$(yq -r '.spec.image.tag' "$PROJECT_ROOT/$patch")"
    assert_eq "$(dirname "$patch")/00-keystone-cr.yaml starts on the retagged release" "$release" \
      "$(yq -r '.spec.image.tag' "$PROJECT_ROOT/$(dirname "$patch")/00-keystone-cr.yaml")"
  done <<<"$patches"
}

test_the_suites_grep_for_the_retagged_image() {
  echo "Test: the e2e trees name no -upgraded tag but the retagged one"
  local release tags
  release="$(retagged_release)"
  # The chainsaw steps that wait for the rollout grep the Deployment image for
  # the tag, so they name it as well, possibly with the dot escaped.
  tags="$(cd "$PROJECT_ROOT" && grep -rhoE '[0-9]{4}\\?\.[0-9]+-upgraded' tests/e2e tests/e2e-*/ | sed 's/[\]//' | sort -u)"
  assert_eq "every -upgraded tag is ${release}-upgraded" "${release}-upgraded" "$tags"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------

test_the_step_retags_the_release_its_guard_names
test_keystone_ships_the_retagged_release
test_every_upgrade_patch_names_the_retagged_image
test_the_suites_grep_for_the_retagged_image

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
