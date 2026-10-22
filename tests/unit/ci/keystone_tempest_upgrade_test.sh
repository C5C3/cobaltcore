#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the keystone-upgrade leg of the tempest job in
# .github/workflows/ci.yaml: the newest release's keystone suite, run against a
# Keystone deployed at the release before it and upgraded in place through
# expand-migrate-contract.
#
# Every piece of the wiring fails silently when it goes missing. Without the
# previous release's image in the image map and on the kind node, the node pulls
# the published tag and the leg upgrades main's image instead of this run's.
# Without the gate on "Deploy Keystone CR for Tempest", that step applies the
# fixture at the newest release first and the leg tests a fresh database. The
# installedRelease check after the bring-up is the only thing that tells such a
# fresh database apart from a migrated one, and the Job wait after the hop is
# what proves expand, migrate and contract ran. The first Ready after the hop
# comes before the operator re-runs db_sync --check at the new release, so only
# a Ready wait after that schema-check Job tells a schema at head apart from
# one left behind a head. The steps only work in their order, and the generator
# has to name the newest transition under releases/ with a CR name the fixture
# defines, or the leg waits out its timeouts on a resource that never exists.
#
# Usage: bash tests/unit/ci/keystone_tempest_upgrade_test.sh

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

GATE="if: matrix.service == 'keystone-upgrade'"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# step_line <step-name> — the line of the named step within the tempest job,
# empty when the job has no such step.
step_line() {
  job_block tempest | awk -v n="      - name: $1" '$0 == n { print NR; exit }'
}

# releases_in_order — the bare names of the releases/*/ directories, in glob
# order, one per line.
releases_in_order() {
  local d
  for d in "$PROJECT_ROOT"/releases/*/; do
    [ -d "$d" ] || continue
    d="${d%/}"
    echo "${d##*/}"
  done
}

# metadata_name <file> — the metadata.name of the YAML document in <file>.
metadata_name() {
  awk '
    /^metadata:/ { in_meta = 1; next }
    in_meta && /^[^ ]/ { in_meta = 0 }
    in_meta && /^  name:/ { sub(/^  name:[[:space:]]*/, ""); gsub(/"/, ""); print; exit }
  ' "$1"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_the_leg_pulls_and_loads_the_previous_keystone_image() {
  echo "Test: the leg pulls the previous release's Keystone image and loads it into kind"

  local step load_at kind_at
  assert_contains "Load E2E images pulls keystone:<upgrade-from> on the leg" \
    "$(job_step tempest "Load E2E images")" \
    "\${{ matrix.service == 'keystone-upgrade' && format('{0}/keystone:{1}', env.IMAGE_PREFIX, matrix.upgrade-from) || '' }}"

  step=$(job_step tempest "Load the previous release's Keystone image into kind")
  assert_contains "the kind load runs on the keystone-upgrade leg only" "$step" "$GATE"
  assert_contains "it loads keystone:<upgrade-from> into the leg's cluster" "$step" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/keystone:\${{ matrix.upgrade-from }} --name \${{ env.KIND_CLUSTER }}"

  load_at=$(step_line "Load images into kind")
  kind_at=$(step_line "Load the previous release's Keystone image into kind")
  assert_not_empty "the job carries the kind load step" "$kind_at"
  assert_gte "it comes after Load images into kind" "${kind_at:-0}" "${load_at:-0}"
}

test_the_fresh_deploy_skips_the_upgrade_leg() {
  echo "Test: the fresh Keystone deploy skips the keystone-upgrade leg"

  assert_contains "Deploy Keystone CR for Tempest is gated off the leg" \
    "$(job_step tempest "Deploy Keystone CR for Tempest")" \
    "if: matrix.service != 'keystone-upgrade'"
}

test_the_previous_release_bring_up() {
  echo "Test: the leg brings its Keystone up at the previous release and proves it"

  local step
  step=$(job_step tempest "Deploy Keystone CR at the previous release")
  assert_contains "the bring-up runs on the keystone-upgrade leg only" "$step" "$GATE"
  assert_contains "it reads the previous release from the matrix" "$step" \
    "UPGRADE_FROM: \${{ matrix.upgrade-from }}"
  assert_contains "it applies the newest fixture with the tag swapped as a string" "$step" \
    "yq '.spec.image.tag = strenv(UPGRADE_FROM)' \${{ matrix.config-dir }}/00-keystone-cr.yaml | kubectl apply -f -"
  assert_contains "it waits for installedRelease to be the previous release" "$step" \
    "--for=jsonpath='{.status.installedRelease}'=\"\$UPGRADE_FROM\" --timeout=60s"
  assert_contains "a fresh database fails the step with the installedRelease it found" "$step" \
    "echo \"::error::keystone/\$CR reports installedRelease '"
  assert_contains "and the error says what the leg would test instead" "$step" \
    "', not \$UPGRADE_FROM: the leg would run Tempest on a fresh database\""
}

test_the_upgrade_step() {
  echo "Test: the leg upgrades its Keystone in place to the leg's release"

  local step check_at ready_at
  step=$(job_step tempest "Upgrade Keystone to the leg's release")
  assert_contains "the hop runs on the keystone-upgrade leg only" "$step" "$GATE"
  assert_contains "it reads the target release from the matrix" "$step" \
    "RELEASE: \${{ matrix.release }}"
  assert_contains "the hop gets 600 s" "$step" 'HOP_TIMEOUT: "600"'
  assert_contains "it moves spec.image.tag to the target release" "$step" \
    "kubectl patch keystone/\"\$CR\" -n openstack --type merge -p \"{\\\"spec\\\":{\\\"image\\\":{\\\"tag\\\":\\\"\${RELEASE}\\\"}}}\""
  assert_contains "it waits for installedRelease to be the target release" "$step" \
    "--for=jsonpath='{.status.installedRelease}'=\"\$RELEASE\" --timeout=\"\${HOP_TIMEOUT}s\""
  assert_contains "a hop that stalls fails the step and points at DatabaseReady" "$step" \
    "echo \"::error::keystone/\$CR did not reach installedRelease \$RELEASE within \${HOP_TIMEOUT} s; the DatabaseReady condition"
  assert_contains "it waits for the expand, migrate and contract Jobs" "$step" \
    "kubectl wait job/\"\$CR\"-db-expand job/\"\$CR\"-db-migrate job/\"\$CR\"-db-contract -n openstack --for=condition=complete"
  assert_contains "it reads the image of the schema-check Job" "$step" \
    "kubectl get job \"\$CR\"-schema-check -n openstack -o jsonpath='{.spec.template.spec.containers[0].image}'"
  assert_contains "a schema-check Job not re-run at the target release fails the step" "$step" \
    "echo \"::error::job/\$CR-schema-check runs '\$check_image', not a :\${RELEASE} image"
  assert_contains "it waits for db_sync --check at the target release to complete" "$step" \
    "if ! kubectl wait job/\"\$CR\"-schema-check -n openstack --for=condition=complete"
  assert_contains "a check that does not complete fails the step" "$step" \
    "echo \"::error::job/\$CR-schema-check (keystone-manage db_sync --check at :\${RELEASE}) did not complete"
  # The status write that moves installedRelease reports Ready before the
  # operator re-runs db_sync --check at the new image, so a Ready wait above the
  # schema-check wait passes on that first Ready and lets Tempest race the check.
  check_at=$(grep -nF -- "kubectl wait job/\"\$CR\"-schema-check" <<<"$step" | head -1 | cut -d: -f1)
  ready_at=$(grep -nF -- "kubectl wait keystone/\"\$CR\" -n openstack --for=condition=Ready" <<<"$step" | head -1 | cut -d: -f1)
  assert_not_empty "it waits for Ready" "$ready_at"
  assert_gte "the Ready wait comes after the schema-check wait" "${ready_at:-0}" "$((${check_at:-0} + 1))"
  assert_contains "it waits for the rolling update of the API replicas" "$step" \
    "kubectl rollout status deploy/\"\$CR\" -n openstack"
  assert_contains "an API Deployment on another image fails the step" "$step" \
    "echo \"::error::deploy/\$CR runs \$image after the upgrade, not a :\${RELEASE} image\"; exit 1"
}

# Each step waits on something the step above it creates or changes. A step
# that drifted upward fails on a resource that is not there yet, and a Tempest
# run that drifted above the hop tests the previous release. A kind load that
# drifted below the bring-up fails nothing: kubelet pulls the published
# keystone:<upgrade-from> first, and the leg upgrades main's image.
test_the_steps_run_in_dependency_order() {
  echo "Test: the image load, the bring-up, the hop and the Tempest run come in dependency order"

  local previous_name previous_at at name
  previous_name="Load the previous release's Keystone image into kind"
  previous_at=$(step_line "$previous_name")
  assert_not_empty "the job carries '${previous_name}'" "$previous_at"
  for name in \
    "Deploy operator (keystone)" \
    "Deploy Keystone CR for Tempest" \
    "Deploy Keystone CR at the previous release" \
    "Upgrade Keystone to the leg's release" \
    "Run Tempest API tests"; do
    at=$(step_line "$name")
    assert_not_empty "the job carries '${name}'" "$at"
    assert_gte "'${name}' comes after '${previous_name}'" "${at:-0}" "${previous_at:-0}"
    previous_name="$name"
    previous_at="$at"
  done
}

# The real generator on the real tree: the leg's pair is the last two releases/
# directories, and the CR the two deploy steps wait on is the one the newest
# keystone fixture defines.
test_the_generator_names_the_newest_transition() {
  echo "Test: the generator names the newest transition under releases/"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (6 checks skipped)"
    SKIP=$((SKIP + 6))
    return
  fi

  local releases count newest prev out rc entry config_dir cr
  releases="$(releases_in_order)"
  count=$(printf '%s\n' "$releases" | grep -c .)
  if [ "$count" -lt 2 ]; then
    echo "  SKIP: fewer than two releases under releases/ (6 checks skipped)"
    SKIP=$((SKIP + 6))
    return
  fi
  newest=$(printf '%s\n' "$releases" | tail -1)
  prev=$(printf '%s\n' "$releases" | tail -2 | head -1)

  out="$TMP_DIR/generator-output"
  rc=0
  GITHUB_OUTPUT="$out" env -u TEMPEST_SERVICES \
    bash "$PROJECT_ROOT/hack/ci-generate-tempest-matrix.sh" >/dev/null 2>&1 || rc=$?
  assert_eq "the generator succeeds on the real tree" "0" "$rc"
  entry=$(sed -n 's/^tempest-releases=//p' "$out" |
    jq -c '[.include[] | select(.service == "keystone-upgrade")]')
  assert_eq "it emits exactly one keystone-upgrade entry" "1" \
    "$(jq 'length' <<<"$entry")"
  assert_eq "the entry upgrades from the second-to-last release to the last" \
    "${prev} ${newest}" "$(jq -r '.[0] | "\(."upgrade-from") \(.release)"' <<<"$entry")"
  config_dir=$(jq -r '.[0]."config-dir"' <<<"$entry")
  assert_eq "it runs the newest release's keystone config directory" \
    "tests/tempest/keystone-${newest//./-}" "$config_dir"
  cr=$(jq -r '.[0]."cr-name"' <<<"$entry")
  assert_eq "the CR it waits on is the one it port-forwards to" "$cr" \
    "$(jq -r '.[0]."service-k8s-name"' <<<"$entry")"
  assert_eq "the newest keystone fixture defines that CR as metadata.name" "$cr" \
    "$(metadata_name "$PROJECT_ROOT/$config_dir/00-keystone-cr.yaml" 2>/dev/null)"
}

# strenv() keeps the swapped tag a string: env() would turn 2026.1 into a YAML
# float, and spec.image.tag is a string field. Nothing else of the fixture may
# change, or the leg would no longer match the fresh keystone leg of the same
# release.
test_the_fixture_render_moves_only_the_tag() {
  echo "Test: the bring-up render moves only spec.image.tag, as a string"

  if [[ "$(yq --version 2>/dev/null)" != *mikefarah* ]]; then
    echo "  SKIP: mikefarah yq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local releases count newest prev fixture rendered
  releases="$(releases_in_order)"
  count=$(printf '%s\n' "$releases" | grep -c .)
  if [ "$count" -lt 2 ]; then
    echo "  SKIP: fewer than two releases under releases/ (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi
  newest=$(printf '%s\n' "$releases" | tail -1)
  prev=$(printf '%s\n' "$releases" | tail -2 | head -1)
  fixture="$PROJECT_ROOT/tests/tempest/keystone-${newest//./-}/00-keystone-cr.yaml"
  rendered="$TMP_DIR/rendered-keystone-cr.yaml"

  UPGRADE_FROM="$prev" yq '.spec.image.tag = strenv(UPGRADE_FROM)' "$fixture" >"$rendered"
  assert_eq "the render sets spec.image.tag to the previous release" "$prev" \
    "$(yq '.spec.image.tag' "$rendered")"
  assert_eq "the tag stays a string" '!!str' "$(yq '.spec.image.tag | tag' "$rendered")"
  assert_eq "nothing else of the fixture changes" \
    "$(yq -o=json -I=0 'del(.spec.image.tag)' "$fixture")" \
    "$(yq -o=json -I=0 'del(.spec.image.tag)' "$rendered")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_the_leg_pulls_and_loads_the_previous_keystone_image
test_the_fresh_deploy_skips_the_upgrade_leg
test_the_previous_release_bring_up
test_the_upgrade_step
test_the_steps_run_in_dependency_order
test_the_generator_names_the_newest_transition
test_the_fixture_render_moves_only_the_tag

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
