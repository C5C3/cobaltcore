#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-operator-upgrade-main-matrix.sh, which decides the operator
# upgrade suites a push to main runs, against a stubbed registry:
#   - the operators whose sha-<HEAD_SHA> tag exists form the matrix, in glob
#     order, and the image map sends their :dev ref to that digest, every other
#     operator's :dev ref to :latest, and each further ref their suites load to
#     itself;
#   - a published operator whose pin equals its Chart.yaml version is left out
#     with a notice, because its baseline is the commit itself, while its :dev
#     ref still maps to the digest for a suite that runs it as a sibling;
#   - a commit that published nothing yields the __none__ sentinel and
#     has-operators=false;
#   - a flaky inspect is retried, a failing one fails the script after three
#     attempts, and a malformed HEAD_SHA or INSPECT_RETRY_DELAY fails before the
#     first inspect;
#   - a ref a suite needs that is not published yet drops that suite with a
#     warning;
#   - a chart without a leg row, and a tree without charts, fail the script.
#
# IMAGE_INSPECT_CMD points at a stub that answers per ref from a mode file
# (digest, notfound, error, flaky) and logs every call. A case that needs an
# operator selected runs the script in a staged tree whose pins are frozen
# below their chart versions, so it does not depend on the repository's pins.
#
# Usage: bash tests/unit/hack/ci_operator_upgrade_main_matrix_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
MATRIX_SH="$PROJECT_ROOT/hack/ci-operator-upgrade-main-matrix.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

SHA=0123456789abcdef0123456789abcdef01234567
P=ghcr.io/c5c3

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# The stub registry. A ref's mode is the content of $MODE_DIR/<ref with / : @
# swapped for _>, notfound when there is no such file. digest answers the JSON
# string of sha256:<sha256 of the ref>; flaky fails with a timeout on its first
# call and answers like digest after. Every call appends the ref to $CALLS.
cat >"$TMP_DIR/inspect" <<'STUB'
#!/bin/bash
ref="$1"
echo "$ref" >>"$CALLS"
key="$(printf '%s' "$ref" | tr '/:@' '___')"
mode="$(cat "$MODE_DIR/$key" 2>/dev/null || echo notfound)"
if [ "$mode" = "flaky" ]; then
  if [ ! -e "$MODE_DIR/$key.seen" ]; then
    : >"$MODE_DIR/$key.seen"
    echo "dial tcp: i/o timeout" >&2
    exit 1
  fi
  mode=digest
fi
case "$mode" in
  digest) printf '"sha256:%s"\n' "$(printf '%s' "$ref" | sha256sum | cut -d' ' -f1)" ;;
  notfound) echo "ERROR: $ref: not found" >&2; exit 1 ;;
  error) echo "dial tcp: i/o timeout" >&2; exit 1 ;;
  garbage) echo '"hello"' ;;
esac
STUB
chmod +x "$TMP_DIR/inspect"

# digest_of <ref> — the digest the stub answers for <ref>.
digest_of() {
  printf 'sha256:%s' "$(printf '%s' "$1" | sha256sum | cut -d' ' -f1)"
}

# reset_registry — every ref notfound, no calls logged.
reset_registry() {
  rm -rf "$TMP_DIR/modes"
  mkdir -p "$TMP_DIR/modes"
  : >"$TMP_DIR/calls"
}

# set_mode <mode> <ref...>
set_mode() {
  local mode="$1" ref
  shift
  for ref in "$@"; do
    printf '%s' "$mode" >"$TMP_DIR/modes/$(printf '%s' "$ref" | tr '/:@' '___')"
  done
}

# run_matrix [<script>] [ENV=value ...] — runs the script (the real one unless
# the first argument is a path) against the stub. Stores stdout+stderr in OUT,
# the exit status in RC and the GITHUB_OUTPUT content in WRITTEN.
run_matrix() {
  local script="$MATRIX_SH"
  if [ $# -gt 0 ] && [ -f "$1" ]; then
    script="$1"
    shift
  fi
  : >"$TMP_DIR/gh_output"
  RC=0
  OUT=$(env HEAD_SHA="$SHA" IMAGE_INSPECT_CMD="$TMP_DIR/inspect" INSPECT_RETRY_DELAY=0 \
    MODE_DIR="$TMP_DIR/modes" CALLS="$TMP_DIR/calls" GITHUB_OUTPUT="$TMP_DIR/gh_output" \
    "$@" bash "$script" 2>&1) || RC=$?
  WRITTEN=$(cat "$TMP_DIR/gh_output")
}

# output <key> — the value of <key> in WRITTEN.
output() {
  sed -n "s/^$1=//p" <<<"$WRITTEN"
}

# map_value <ref> — what the written image map sends <ref> to.
map_value() {
  output image-map | jq -r --arg ref "$1" '.[$ref] // "<absent>"'
}

# calls_for <ref> — how often the stub was asked for <ref>.
calls_for() {
  grep -cFx -- "$1" "$TMP_DIR/calls" || true
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_published_operators_form_the_matrix() {
  echo "Test: keystone and nova published, glance not"
  local script
  script="$(stage_tree glance keystone nova)"
  reset_registry
  set_mode digest "$P/keystone-operator:sha-$SHA" "$P/nova-operator:sha-$SHA" \
    "$P/keystone:2026.1" "$P/tempest:2026.1" "$P/nova:2026.1"

  run_matrix "$script"
  [ "$RC" -eq 0 ] || echo "$OUT"
  assert_eq "the script exits 0" "0" "$RC"
  assert_eq "operators names keystone and nova in glob order" \
    '{"operator":["keystone","nova"]}' "$(output operators)"
  assert_eq "has-operators is true" "true" "$(output has-operators)"
  assert_eq "keystone's :dev ref maps to the digest of its sha tag" \
    "$P/keystone-operator@$(digest_of "$P/keystone-operator:sha-$SHA")" \
    "$(map_value "$P/keystone-operator:dev")"
  assert_eq "glance's :dev ref maps to :latest" "$P/glance-operator:latest" \
    "$(map_value "$P/glance-operator:dev")"
  assert_eq "nova's service image maps to itself" "$P/nova:2026.1" "$(map_value "$P/nova:2026.1")"
  assert_eq "an image no selected suite loads is not inspected" "0" "$(calls_for "$P/glance:2026.1")"
  assert_contains "the log names the published digest" "$OUT" \
    "keystone-operator: published at $(digest_of "$P/keystone-operator:sha-$SHA")"
  assert_contains "the log names the commit that did not publish glance" "$OUT" \
    "glance-operator: not published by 0123456"
  assert_eq "stdout repeats what GITHUB_OUTPUT holds" "$WRITTEN" "$(grep -E '^(operators|has-operators|image-map)=' <<<"$OUT")"

  # Every ref the two suites load is in the map, so load-e2e-images never
  # falls back to a run-scoped tag that main runs do not push.
  local op ref missing=""
  for op in keystone nova; do
    while IFS= read -r ref; do
      [ "$(map_value "$ref")" != "<absent>" ] || missing="$missing $ref"
    done < <(OPERATOR="$op" bash "$PROJECT_ROOT/hack/ci-operator-upgrade-leg.sh" |
      sed -n '/^refs<<EOF$/,/^EOF$/p' | sed '1d;$d')
  done
  assert_eq "every ref of the selected suites is in the map" "" "$missing"
}

test_nothing_published() {
  echo "Test: a commit that published no operator image"
  reset_registry

  run_matrix
  assert_eq "the script exits 0" "0" "$RC"
  assert_eq "operators is the sentinel" '{"operator":["__none__"]}' "$(output operators)"
  assert_eq "has-operators is false" "false" "$(output has-operators)"
  assert_eq "a missing tag is asked once" "1" "$(calls_for "$P/keystone-operator:sha-$SHA")"
}

test_a_flaky_inspect_is_retried() {
  echo "Test: an inspect that fails once is retried"
  local script
  script="$(stage_tree glance)"
  reset_registry
  set_mode flaky "$P/glance-operator:sha-$SHA"
  set_mode digest "$P/glance:2026.1"

  run_matrix "$script"
  assert_eq "the script exits 0" "0" "$RC"
  assert_eq "glance is published" '{"operator":["glance"]}' "$(output operators)"
  assert_eq "the stub was asked twice for glance's sha tag" "2" "$(calls_for "$P/glance-operator:sha-$SHA")"
  assert_contains "the retry is logged" "$OUT" \
    "::warning::inspect of $P/glance-operator:sha-$SHA failed (attempt 1/3)"
}

test_a_failing_inspect_fails_the_script() {
  echo "Test: an inspect that keeps failing fails the script"
  reset_registry
  set_mode error "$P/cinder-operator:sha-$SHA"

  run_matrix
  assert_eq "the script exits 1" "1" "$RC"
  assert_contains "the error names the ref and the registry's answer" "$OUT" \
    "::error::cannot tell whether $P/cinder-operator:sha-$SHA exists: dial tcp: i/o timeout"
  assert_eq "it gave up after three inspects" "3" "$(calls_for "$P/cinder-operator:sha-$SHA")"
  assert_eq "nothing is written to GITHUB_OUTPUT" "" "$WRITTEN"

  reset_registry
  set_mode garbage "$P/barbican-operator:sha-$SHA"
  run_matrix
  assert_eq "an answer that is not a digest exits 1" "1" "$RC"
  assert_contains "it names the answer" "$OUT" "::error::unexpected digest for $P/barbican-operator:sha-$SHA: hello"
}

test_inputs_are_checked_before_any_inspect() {
  echo "Test: a malformed HEAD_SHA or INSPECT_RETRY_DELAY fails before the first inspect"

  local value
  for value in abc "" "$(tr '[:lower:]' '[:upper:]' <<<"$SHA")"; do
    reset_registry
    run_matrix HEAD_SHA="$value"
    assert_eq "HEAD_SHA='$value' exits 1" "1" "$RC"
    assert_contains "it quotes the value" "$OUT" \
      "::error::HEAD_SHA must be a 40-character commit SHA (got: '$value')"
    assert_eq "no inspect ran" "" "$(cat "$TMP_DIR/calls")"
  done

  reset_registry
  run_matrix INSPECT_RETRY_DELAY=x
  assert_eq "INSPECT_RETRY_DELAY=x exits 1" "1" "$RC"
  assert_contains "it names the delay" "$OUT" "INSPECT_RETRY_DELAY must be a whole number of seconds (got: 'x')"
  assert_eq "no inspect ran" "" "$(cat "$TMP_DIR/calls")"
}

test_an_unpublished_ref_drops_its_suites() {
  echo "Test: ovn and neutron published before their OVN image"
  local ovn_version script
  ovn_version="$(bash "$PROJECT_ROOT/hack/ci-resolve-ovn-version.sh")"
  script="$(stage_tree neutron ovn)"
  reset_registry
  set_mode digest "$P/ovn-operator:sha-$SHA" "$P/neutron-operator:sha-$SHA" "$P/neutron:2026.1"

  run_matrix "$script"
  assert_eq "the script exits 0" "0" "$RC"
  assert_contains "ovn is dropped with a warning" "$OUT" \
    "::warning::ovn-operator: $P/ovn:${ovn_version} is not published yet; the run that follows Build Images covers it"
  assert_contains "neutron is dropped with a warning" "$OUT" \
    "::warning::neutron-operator: $P/ovn:${ovn_version} is not published yet; the run that follows Build Images covers it"
  assert_eq "the OVN image is asked once for both suites" "1" "$(calls_for "$P/ovn:${ovn_version}")"
  assert_eq "operators is the sentinel" '{"operator":["__none__"]}' "$(output operators)"
  assert_eq "has-operators is false" "false" "$(output has-operators)"
  assert_eq "neutron's sibling ovn-operator maps to its published digest" \
    "$P/ovn-operator@$(digest_of "$P/ovn-operator:sha-$SHA")" "$(map_value "$P/ovn-operator:dev")"
}

# stage_tree <operator...> — a scratch tree with the four scripts under hack/,
# the OVN pin, and per operator a chart at 0.2.0 and an upgrade suite pinned to
# the frozen 0.1.0. Echoes the staged copy of the script.
stage_tree() {
  local tree="$TMP_DIR/tree" op
  rm -rf "$tree"
  mkdir -p "$tree/hack" "$tree/images/ovn"
  cp "$MATRIX_SH" "$PROJECT_ROOT/hack/ci-operator-upgrade-leg.sh" \
    "$PROJECT_ROOT/hack/ci-resolve-ovn-version.sh" "$PROJECT_ROOT/hack/ci-read-chart-version.sh" \
    "$tree/hack/"
  cp "$PROJECT_ROOT/images/ovn/source.json" "$tree/images/ovn/"
  for op in "$@"; do
    mkdir -p "$tree/operators/$op/helm/$op-operator" "$tree/tests/e2e-operator-upgrade/$op-helm-upgrade"
    printf 'name: %s-operator\nversion: 0.2.0\n' "$op" >"$tree/operators/$op/helm/$op-operator/Chart.yaml"
    pin "$op" 0.1.0
  done
  echo "$tree/hack/ci-operator-upgrade-main-matrix.sh"
}

# pin <operator> <version> — set the baseline pin of a staged operator.
pin() {
  printf '# scratch pin\nversion: "%s"\n' "$2" \
    >"$TMP_DIR/tree/tests/e2e-operator-upgrade/$1-helm-upgrade/baseline.yaml"
}

test_a_pin_main_still_publishes_is_left_out() {
  echo "Test: a published operator pinned to its own Chart.yaml version is left out"
  local script
  script="$(stage_tree keystone nova)"
  pin keystone 0.2.0
  reset_registry
  set_mode digest "$P/keystone-operator:sha-$SHA" "$P/nova-operator:sha-$SHA" \
    "$P/keystone:2026.1" "$P/tempest:2026.1" "$P/nova:2026.1"

  run_matrix "$script"
  assert_eq "the script exits 0" "0" "$RC"
  assert_eq "operators names nova only" '{"operator":["nova"]}' "$(output operators)"
  assert_contains "the notice names the pin and the commit" "$OUT" \
    "::notice::keystone-operator: the baseline 0.2.0 is what 0123456 published, so the suite would upgrade the commit onto itself; skipped until Chart.yaml moves past 0.2.0"
  assert_eq "the left-out suite's images are not inspected" "0" "$(calls_for "$P/keystone:2026.1")"
  assert_eq "keystone's :dev ref still maps to its digest" \
    "$P/keystone-operator@$(digest_of "$P/keystone-operator:sha-$SHA")" \
    "$(map_value "$P/keystone-operator:dev")"

  pin nova 0.2.0
  run_matrix "$script"
  assert_eq "with both pins unfrozen the script exits 0" "0" "$RC"
  assert_eq "operators is the sentinel" '{"operator":["__none__"]}' "$(output operators)"
  assert_eq "has-operators is false" "false" "$(output has-operators)"

  # A sibling whose own suite is left out still runs as the published image.
  script="$(stage_tree neutron ovn)"
  pin ovn 0.2.0
  reset_registry
  set_mode digest "$P/ovn-operator:sha-$SHA" "$P/neutron-operator:sha-$SHA" "$P/neutron:2026.1" \
    "$P/ovn:$(bash "$PROJECT_ROOT/hack/ci-resolve-ovn-version.sh")"
  run_matrix "$script"
  assert_eq "the sibling case exits 0" "0" "$RC"
  assert_eq "operators names neutron only" '{"operator":["neutron"]}' "$(output operators)"
  assert_eq "neutron's sibling ovn-operator maps to its published digest" \
    "$P/ovn-operator@$(digest_of "$P/ovn-operator:sha-$SHA")" "$(map_value "$P/ovn-operator:dev")"
}

test_a_chart_without_a_leg_row_fails() {
  echo "Test: a published operator without a leg row fails the script"
  local script
  script="$(stage_tree designate keystone)"
  reset_registry
  set_mode digest "$P/designate-operator:sha-$SHA"

  run_matrix "$script"
  assert_eq "the script exits 1" "1" "$RC"
  assert_contains "the leg script's error reaches the log" "$OUT" \
    "::error::no upgrade leg is defined for operator 'designate'; add it to hack/ci-operator-upgrade-leg.sh"
  assert_eq "nothing is written to GITHUB_OUTPUT" "" "$WRITTEN"
}

test_a_tree_without_charts_fails() {
  echo "Test: a tree without operator charts fails before any inspect"
  local script
  script="$(stage_tree)"
  reset_registry

  run_matrix "$script"
  assert_eq "the script exits 1" "1" "$RC"
  assert_contains "it names the glob" "$OUT" "::error::no operator chart found under operators/*/helm/"
  assert_eq "no inspect ran" "" "$(cat "$TMP_DIR/calls")"
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

test_published_operators_form_the_matrix
test_nothing_published
test_a_flaky_inspect_is_retried
test_a_failing_inspect_fails_the_script
test_inputs_are_checked_before_any_inspect
test_an_unpublished_ref_drops_its_suites
test_a_pin_main_still_publishes_is_left_out
test_a_chart_without_a_leg_row_fails
test_a_tree_without_charts_fails

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
