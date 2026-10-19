#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the conventions every composite action under .github/actions/ and
# every workflow under .github/workflows/ follows: the SPDX header, an explicit
# shell on every composite run: step, the sbom input checks of
# supply-chain-attest, and a SHA pin on every external uses: reference.
#
# None of these is checked anywhere else. actionlint lints the workflows only,
# and checks no pins. The runner rejects a composite run: step without shell:
# only when it loads the action, so a rarely used action breaks the first job
# that uses it. A uses: reference by tag runs whatever the tag points to
# on the day of the run: a retargeted tag is a supply-chain change no review
# saw. Renovate's github-actions manager moves an @<sha> # vX.Y.Z pin and its
# comment together, so the pinned form costs no manual bumps. A docker://
# reference is pinned by its image digest instead.
#
# Usage: bash tests/unit/ci/actions_conventions_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

shopt -s nullglob
# GitHub loads action.yml as well as action.yaml, and runs workflows under
# either extension, so a file with the other one must not skip the checks.
ACTION_FILES=("$PROJECT_ROOT"/.github/actions/*/action.yaml
  "$PROJECT_ROOT"/.github/actions/*/action.yml)
WORKFLOW_FILES=("$PROJECT_ROOT"/.github/workflows/*.yaml
  "$PROJECT_ROOT"/.github/workflows/*.yml)
shopt -u nullglob

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Echo the path relative to the repository root.
rel() {
  printf '%s\n' "${1#"$PROJECT_ROOT/"}"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_the_file_sets_are_not_empty() {
  echo "Test: the composite actions and the workflows are found"

  # An empty set would let every test below pass on nothing.
  if [ "${#ACTION_FILES[@]}" -eq 0 ]; then
    echo "  FAIL: no .github/actions/*/action.y[a]ml found"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: ${#ACTION_FILES[@]} composite actions found"
    PASS=$((PASS + 1))
  fi
  if [ "${#WORKFLOW_FILES[@]}" -eq 0 ]; then
    echo "  FAIL: no .github/workflows/*.y[a]ml found"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: ${#WORKFLOW_FILES[@]} workflows found"
    PASS=$((PASS + 1))
  fi
}

test_every_action_and_workflow_carries_the_spdx_header() {
  echo "Test: every action and workflow carries the SPDX header"

  local f head5
  for f in ${ACTION_FILES[@]+"${ACTION_FILES[@]}"} \
    ${WORKFLOW_FILES[@]+"${WORKFLOW_FILES[@]}"}; do
    head5=$(head -5 "$f")
    assert_contains "$(rel "$f") carries SPDX-FileCopyrightText" "$head5" \
      "SPDX-FileCopyrightText"
    assert_contains "$(rel "$f") carries SPDX-License-Identifier" "$head5" \
      "SPDX-License-Identifier"
  done
}

test_every_composite_run_step_names_its_shell() {
  echo "Test: every run: step of a composite action names shell: bash"

  local f run_count shell_count
  for f in ${ACTION_FILES[@]+"${ACTION_FILES[@]}"}; do
    run_count=$(grep -cE '^[[:space:]]+(-[[:space:]]+)?run:([[:space:]]|$)' "$f" || true)
    shell_count=$(grep -cE '^[[:space:]]+(-[[:space:]]+)?shell:[[:space:]]+bash' "$f" || true)
    if [ "$run_count" -eq 0 ]; then
      echo "  SKIP: $(rel "$f") has no run: steps"
      SKIP=$((SKIP + 1))
      continue
    fi
    assert_eq "$(rel "$f") run: steps all name shell: bash" \
      "$run_count" "$shell_count"
  done
}

test_supply_chain_attest_validates_its_sbom_inputs() {
  echo "Test: supply-chain-attest rejects an sbom scan without its inputs"

  local action="$PROJECT_ROOT/.github/actions/supply-chain-attest/action.yaml"
  assert_file_contains_fixed "a missing image-digest is rejected" "$action" \
    "image-digest is required when scan-mode=sbom"
  assert_file_contains_fixed "a missing sbom-output-file is rejected" "$action" \
    "sbom-output-file is required when scan-mode=sbom"
}

test_every_external_uses_is_pinned() {
  echo "Test: every external uses: reference is pinned to a SHA or a digest"

  local f line n value external misses
  for f in ${ACTION_FILES[@]+"${ACTION_FILES[@]}"} \
    ${WORKFLOW_FILES[@]+"${WORKFLOW_FILES[@]}"}; do
    external=0
    misses=0
    while IFS= read -r line; do
      [[ "$line" =~ ^([0-9]+):[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*(.*)$ ]] ||
        continue
      n=${BASH_REMATCH[1]}
      value=${BASH_REMATCH[3]}
      case "$value" in
        ./*) continue ;;
      esac
      external=$((external + 1))
      if [[ "$value" =~ @[a-f0-9]{40}\ \#\ v ]]; then
        continue
      fi
      if [[ "$value" == docker://* && "$value" =~ @sha256:[a-f0-9]{64}$ ]]; then
        continue
      fi
      echo "  FAIL: $(rel "$f"):$n is not SHA-pinned: $value"
      FAIL=$((FAIL + 1))
      misses=$((misses + 1))
    done <<<"$(grep -nE '^[[:space:]]*(- )?uses:[[:space:]]' "$f")"

    if [ "$external" -eq 0 ]; then
      echo "  SKIP: $(rel "$f") has no external uses: references"
      SKIP=$((SKIP + 1))
    elif [ "$misses" -eq 0 ]; then
      echo "  PASS: $(rel "$f"): $external external uses: references are SHA-pinned"
      PASS=$((PASS + 1))
    fi
  done
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_the_file_sets_are_not_empty
test_every_action_and_workflow_carries_the_spdx_header
test_every_composite_run_step_names_its_shell
test_supply_chain_attest_validates_its_sbom_inputs
test_every_external_uses_is_pinned

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
