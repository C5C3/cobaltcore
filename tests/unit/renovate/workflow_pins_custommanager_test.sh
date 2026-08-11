#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json has customManagers for the five GitHub Actions env-var
# tool pins:
#   - CONTROLLER_GEN_VERSION → kubernetes-sigs/controller-tools
#   - GOFUMPT_VERSION        → mvdan/gofumpt
#   - GOLANGCI_LINT_VERSION  → golangci/golangci-lint
#   - KIND_VERSION           → kubernetes-sigs/kind
#   - YQ_VERSION             → mikefarah/yq
#
# For each: regex must capture the literal in the workflow file that pins it,
# and the paired packageRule must disable majors and automerge minor/patch.
#
# KIND_VERSION carries one extra obligation. It is pinned twice — in ci.yaml for
# the e2e clusters and in hack/install-test-deps.sh for local development — and the
# two must agree, or CI and a developer's kind enforce NetworkPolicy egress
# differently. Renovate must therefore bump both in ONE pull request; ungrouped it
# would open two, each leaving the lockstep assertion below red.
#
# Usage: bash tests/unit/renovate/workflow_pins_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"

# Map: env_var:expected_packageName:workflow_file
TOOLS=(
  "CONTROLLER_GEN_VERSION:kubernetes-sigs/controller-tools:.github/workflows/ci.yaml"
  "GOFUMPT_VERSION:mvdan/gofumpt:.github/workflows/ci.yaml"
  "GOLANGCI_LINT_VERSION:golangci/golangci-lint:.github/workflows/ci.yaml"
  "KIND_VERSION:kubernetes-sigs/kind:.github/workflows/ci.yaml"
  "YQ_VERSION:mikefarah/yq:.github/workflows/verify-container-images.yaml"
)

test_each_workflow_pin_has_manager() {
  echo "Test: every workflow env-var pin has a customManager whose regex matches"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed"
    SKIP=$((SKIP + 1))
    return
  fi
  if ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: perl not installed"
    SKIP=$((SKIP + 1))
    return
  fi

  local triplet env_var pkg_name wf_file entry match_string captured
  for triplet in "${TOOLS[@]}"; do
    env_var="${triplet%%:*}"
    pkg_name="$(printf '%s' "$triplet" | cut -d: -f2)"
    wf_file="${triplet##*:}"

    if [[ ! -f "$PROJECT_ROOT/$wf_file" ]]; then
      echo "  SKIP: $wf_file missing (skipping $env_var)"
      SKIP=$((SKIP + 1))
      continue
    fi

    entry="$(jq -c --arg pkg "$pkg_name" '.customManagers[]
      | select(.packageNameTemplate == $pkg
               and ((.managerFilePatterns // []) | join(",") | contains("workflows")))' \
      "$RENOVATE_FILE" | head -1)"

    if [ -z "$entry" ]; then
      echo "  FAIL: no customManager for ${env_var} → ${pkg_name} covering workflows"
      FAIL=$((FAIL + 1))
      continue
    fi

    match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"
    captured="$(REGEX="$match_string" FILE="$PROJECT_ROOT/$wf_file" perl -e '
      my $re = $ENV{REGEX};
      local $/; open my $fh, "<", $ENV{FILE} or die $!;
      my $content = <$fh>;
      if ($content =~ /$re/) { print $+{currentValue} // ""; }
    ')"

    if [ -n "$captured" ]; then
      echo "  PASS: ${env_var} → ${pkg_name} regex captures ${captured} in ${wf_file}"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: ${env_var} regex did not match in ${wf_file}"
      FAIL=$((FAIL + 1))
    fi
  done
}

test_kind_version_is_pinned_in_lockstep() {
  echo "Test: the kind pins in ci.yaml and install-test-deps.sh agree, and bump together"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local ci_file install_file ci_kind install_kind
  ci_file="$PROJECT_ROOT/.github/workflows/ci.yaml"
  install_file="$PROJECT_ROOT/hack/install-test-deps.sh"

  ci_kind="$(sed -n 's/^[[:space:]]*KIND_VERSION:[[:space:]]*"\{0,1\}\(v[0-9.]*\)"\{0,1\}[[:space:]]*$/\1/p' \
    "$ci_file" | head -1)"
  install_kind="$(sed -n 's/^KIND_VERSION="\(v[0-9.]*\)"$/\1/p' "$install_file" | head -1)"

  assert_not_empty "ci.yaml pins KIND_VERSION" "$ci_kind"
  # CI creating its clusters with the helm/kind-action default (v0.31.0) while
  # local development installs a newer kind is how a NetworkPolicy egress posture
  # that only holds on one of them reaches main unnoticed.
  assert_eq "the ci.yaml and install-test-deps.sh kind pins agree" "$install_kind" "$ci_kind"

  # One group, so Renovate bumps both files in a single pull request. Two rules
  # would open two, and each would fail the assertion above until the other landed.
  local groups
  groups="$(jq -r '[.packageRules[]
    | select(((.matchPackageNames // []) | index("kubernetes-sigs/kind")) != null)
    | select(((.matchUpdateTypes // []) | index("minor")) != null)
    | .groupName] | unique | join(",")' "$RENOVATE_FILE")"
  assert_eq "exactly one Renovate group owns the kind pins" "kind" "$groups"
}

test_workflow_pins_share_a_package_rule() {
  echo "Test: workflow tool pins share a major-disable + minor-automerge packageRule"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local pkg
  for pkg in kubernetes-sigs/controller-tools golangci/golangci-lint mikefarah/yq; do
    local major_rule
    major_rule="$(jq -c --arg pkg "$pkg" '.packageRules[]
      | select(
          ((.matchPackageNames // []) | index($pkg)) != null
          and (((.matchUpdateTypes // []) | index("major")) != null)
        )' "$RENOVATE_FILE" | head -1)"

    if [ -z "$major_rule" ]; then
      echo "  FAIL: no major-disable packageRule covering ${pkg}"
      FAIL=$((FAIL + 1))
      continue
    fi
    assert_eq "major ${pkg} updates are disabled" \
      "false" \
      "$(jq -r '.enabled' <<<"$major_rule")"
  done

  local minor_rule
  minor_rule="$(jq -c '.packageRules[]
    | select(
        ((.matchPackageNames // []) | index("mikefarah/yq")) != null
        and (((.matchUpdateTypes // []) | index("minor")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no minor/patch packageRule covering mikefarah/yq"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_eq "minor/patch workflow tool updates wait minimumReleaseAge=3 days" \
    "3 days" \
    "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
}

test_each_workflow_pin_has_manager
test_kind_version_is_pinned_in_lockstep
test_workflow_pins_share_a_package_rule

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
