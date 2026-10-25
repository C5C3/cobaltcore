#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-operator-upgrade-leg.sh, the table of what each operator
# upgrade suite loads and deploys:
#   - every operator's refs, siblings and flags, the keystone row alone with
#     the Tempest image, Chaos Mesh and the pod-kill pass, with the ovn and
#     neutron rows
#     taking the OVN version from a stubbed hack/ci-resolve-ovn-version.sh,
#     IMAGE_PREFIX prefixing every ref, and GITHUB_OUTPUT receiving what
#     stdout shows;
#   - a failing OVN version resolver fails the row that needs it;
#   - an operator outside the table, a missing OPERATOR and a malformed one
#     fail with their ::error:: lines;
#   - every chart under operators/*/helm/*-operator has a row, so a new
#     operator cannot reach the upgrade matrix without one;
#   - RELEASE is the release every suite's fixtures pin in spec.image.tag and
#     its installedRelease asserts check, so a release move that re-pins one
#     side fails here instead of in ten self-hosted upgrade jobs.
#
# The script is staged beside the stub resolver, which it calls through its
# own directory.
#
# Usage: bash tests/unit/hack/ci_operator_upgrade_leg_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
LEG_SH="$PROJECT_ROOT/hack/ci-operator-upgrade-leg.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# Stage the script beside a resolver stub that prints 99.1.2, or fails under
# OVN_RESOLVE_FAIL=1 the way the real one does: an ::error:: line on stderr.
mkdir -p "$TMP_DIR/hack"
cp "$LEG_SH" "$TMP_DIR/hack/ci-operator-upgrade-leg.sh"
cat >"$TMP_DIR/hack/ci-resolve-ovn-version.sh" <<'STUB'
#!/bin/bash
if [ "${OVN_RESOLVE_FAIL:-}" = "1" ]; then
  echo "::error::no \"version\" in images/ovn/source.json" >&2
  exit 1
fi
echo "99.1.2"
STUB
chmod +x "$TMP_DIR/hack/ci-operator-upgrade-leg.sh" "$TMP_DIR/hack/ci-resolve-ovn-version.sh"

# run_leg <operator> [ENV=value ...] runs the staged script with GITHUB_OUTPUT
# in the scratch directory. Stores stdout in OUT, stderr in ERR, the exit
# status in RC and the GITHUB_OUTPUT content in WRITTEN.
run_leg() {
  local operator="$1"
  shift
  : >"$TMP_DIR/gh_output"
  RC=0
  OUT=$(env OPERATOR="$operator" GITHUB_OUTPUT="$TMP_DIR/gh_output" "$@" \
    bash "$TMP_DIR/hack/ci-operator-upgrade-leg.sh" 2>"$TMP_DIR/stderr") || RC=$?
  ERR=$(cat "$TMP_DIR/stderr")
  WRITTEN=$(cat "$TMP_DIR/gh_output")
}

# expected <refs> <with-nfs> <with-messaging> <with-ovn-kernel-modules>
#   <siblings> <sibling-crds> <with-chaos-mesh> <pod-kill-suite> — the output a
#   row should print, refs given space-separated and prefixed with
#   ghcr.io/c5c3/.
expected() {
  local ref
  echo "refs<<EOF"
  for ref in $1; do echo "ghcr.io/c5c3/${ref}"; done
  echo "EOF"
  echo "with-nfs=$2"
  echo "with-messaging=$3"
  echo "with-ovn-kernel-modules=$4"
  echo "siblings=$5"
  echo "sibling-crds=$6"
  echo "with-chaos-mesh=$7"
  echo "pod-kill-suite=$8"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_every_row() {
  echo "Test: each operator's refs, siblings and flags match the table"

  local row operator refs nfs messaging kernel siblings crds chaos podkill
  # operator|refs|with-nfs|with-messaging|with-ovn-kernel-modules|siblings|sibling-crds|with-chaos-mesh|pod-kill-suite
  for row in \
    "keystone|keystone-operator:dev keystone:2026.1 tempest:2026.1||||||true|true" \
    "c5c3|c5c3-operator:dev|||||true||" \
    "horizon|horizon-operator:dev horizon:2026.1||||||" \
    "glance|glance-operator:dev glance:2026.1||||||" \
    "placement|placement-operator:dev placement:2026.1||||||" \
    "barbican|barbican-operator:dev barbican:2026.1||||||" \
    "ovn|ovn-operator:dev ovn:99.1.2|||true||||" \
    "neutron|neutron-operator:dev neutron:2026.1 ovn-operator:dev ovn:99.1.2|||true|ovn|||" \
    "cinder|cinder-operator:dev cinder:2026.1|true|true|||||" \
    "nova|nova-operator:dev nova:2026.1||true|||||"; do
    IFS='|' read -r operator refs nfs messaging kernel siblings crds chaos podkill <<<"$row"
    run_leg "$operator"
    assert_eq "$operator exits 0" "0" "$RC"
    assert_eq "$operator prints its row" \
      "$(expected "$refs" "$nfs" "$messaging" "$kernel" "$siblings" "$crds" "$chaos" "$podkill")" "$OUT"
    assert_eq "$operator writes to GITHUB_OUTPUT what stdout shows" "$OUT" "$WRITTEN"
  done
}

test_image_prefix() {
  echo "Test: IMAGE_PREFIX prefixes every ref"

  run_leg neutron IMAGE_PREFIX=registry.example/team
  assert_eq "neutron exits 0" "0" "$RC"
  assert_eq "every ref carries the prefix" \
    "registry.example/team/neutron-operator:dev
registry.example/team/neutron:2026.1
registry.example/team/ovn-operator:dev
registry.example/team/ovn:99.1.2" "$(sed -n '/^refs<<EOF$/,/^EOF$/p' <<<"$OUT" | sed '1d;$d')"
}

test_failing_ovn_resolver_fails_the_row() {
  echo "Test: a failing OVN version resolver fails the ovn and neutron rows"

  local operator
  for operator in ovn neutron; do
    run_leg "$operator" OVN_RESOLVE_FAIL=1
    assert_eq "$operator exits 1" "1" "$RC"
    assert_contains "$operator shows the resolver's error" "$ERR" '::error::no "version" in images/ovn/source.json'
    assert_eq "$operator prints nothing on stdout" "" "$OUT"
    assert_eq "$operator writes nothing to GITHUB_OUTPUT" "" "$WRITTEN"
  done

  run_leg keystone OVN_RESOLVE_FAIL=1
  assert_eq "a row without OVN does not call the resolver" "0" "$RC"
}

test_unknown_and_missing_operator() {
  echo "Test: an operator outside the table, a missing and a malformed OPERATOR fail"

  run_leg designate
  assert_eq "designate exits 1" "1" "$RC"
  assert_eq "designate names the table" \
    "::error::no upgrade leg is defined for operator 'designate'; add it to hack/ci-operator-upgrade-leg.sh" "$ERR"
  assert_eq "designate prints nothing on stdout" "" "$OUT"

  run_leg ""
  assert_eq "a missing OPERATOR exits 1" "1" "$RC"
  assert_eq "it names the missing OPERATOR" "::error::OPERATOR is required (e.g. keystone)" "$ERR"

  run_leg "Nova"
  assert_eq "a malformed OPERATOR exits 1" "1" "$RC"
  assert_eq "it quotes the malformed value" \
    "::error::OPERATOR must be lowercase alphanumeric (with hyphens), got 'Nova'" "$ERR"
}

test_every_chart_has_a_row() {
  echo "Test: every chart under operators/*/helm/*-operator has a row"

  # The real script calls the real OVN resolver, which needs jq.
  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed"
    SKIP=$((SKIP + 1))
    return
  fi
  local chart operator checked=0 rc
  for chart in "$PROJECT_ROOT"/operators/*/helm/*-operator/Chart.yaml; do
    [ -f "$chart" ] || continue
    operator="${chart#"$PROJECT_ROOT"/operators/}"
    operator="${operator%%/*}"
    checked=$((checked + 1))
    OPERATOR="$operator" bash "$LEG_SH" >/dev/null 2>"$TMP_DIR/stderr"
    rc=$?
    [ "$rc" -eq 0 ] || cat "$TMP_DIR/stderr"
    assert_eq "$operator has a row" "0" "$rc"
  done
  assert_gte "the ten operator charts were checked" "$checked" "10"
}

test_release_matches_the_fixtures() {
  echo "Test: RELEASE is the release every upgrade fixture pins"

  local release fixture tag suite value checked=0
  release="$(sed -n 's/^RELEASE="\(.*\)"$/\1/p' "$LEG_SH")"
  assert_not_empty "the script sets RELEASE" "$release"
  for fixture in "$PROJECT_ROOT"/tests/e2e-operator-upgrade/*-helm-upgrade/0*.yaml; do
    [ -f "$fixture" ] || continue
    # The tag: line of the CR's top-level spec.image block; a fixture without
    # one (the ovn-operator picks its own image) is not a pin.
    tag="$(awk '/^  image:$/ { in_image = 1; next }
      in_image && /^    tag:/ { gsub(/"/, "", $2); print $2; exit }
      /^  [^ ]/ { in_image = 0 }' "$fixture")"
    [ -n "$tag" ] || continue
    checked=$((checked + 1))
    assert_eq "${fixture#"$PROJECT_ROOT"/} pins spec.image.tag ${release}" "$release" "$tag"
  done
  assert_gte "the fixtures with a service image were checked" "$checked" "1"

  for suite in "$PROJECT_ROOT"/tests/e2e-operator-upgrade/*-helm-upgrade/chainsaw-test.yaml; do
    while IFS= read -r value; do
      assert_eq "${suite#"$PROJECT_ROOT"/} asserts installedRelease ${release}" "$release" "$value"
    done < <(sed -n 's/^ *installedRelease: *"\{0,1\}\([^"]*\)"\{0,1\} *$/\1/p' "$suite")
  done
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_every_row
test_image_prefix
test_failing_ovn_resolver_fails_the_row
test_unknown_and_missing_operator
test_every_chart_has_a_row
test_release_matches_the_fixtures

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
