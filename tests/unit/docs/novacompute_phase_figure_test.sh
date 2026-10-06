#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the node phases figure names every phase of a NovaCompute node.
#
# docs/diagrams/compute-node-phases.drawio and .svg draw the phases a node in
# a NovaCompute pool passes through. operators/nova/api/v1alpha1/
# novacompute_types.go defines each as
# `NovaComputeNode<Name> NovaComputeNodePhase = "<value>"`. check_phase_figure
# looks for every value in both files of the pair as a label of its own: in
# the .drawio as a cell value, in the .svg as a <text> element. It prints one
# problem per line and nothing when the figure names every phase. The check
# runs one way: a label the figure has and the types lack is not reported. The
# fixture tests prove each check on a scratch tree before the last test runs
# it on the repository.
#
# Usage: bash tests/unit/docs/novacompute_phase_figure_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

FIGURE="compute-node-phases"

# check_phase_figure <types.go> <diagrams-dir>: print one problem per line,
# sorted, and nothing when both files of the figure name every phase. A
# missing types file or one without a phase constant is the only line.
check_phase_figure() {
  local types="$1" dir="$2"
  local drawio="$dir/$FIGURE.drawio" svg="$dir/$FIGURE.svg"

  if [[ ! -f "$types" ]]; then
    echo "no types file: $types"
    return
  fi

  local values
  values="$(sed -n 's/^[[:space:]]*NovaComputeNode[A-Za-z]*[[:space:]][[:space:]]*NovaComputeNodePhase[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "$types")"
  if [[ -z "$values" ]]; then
    echo "no NovaComputeNodePhase constants in $types"
    return
  fi

  {
    [[ -f "$drawio" ]] || echo "missing figure: $FIGURE.drawio"
    [[ -f "$svg" ]] || echo "missing figure: $FIGURE.svg"
    local value
    while IFS= read -r value; do
      if [[ -f "$drawio" ]] && ! grep -F -q -- "value=\"$value\"" "$drawio"; then
        echo "not in drawio: $value"
      fi
      if [[ -f "$svg" ]] && ! grep -F -q -- ">$value<" "$svg"; then
        echo "not in svg: $value"
      fi
    done <<<"$values"
  } | LC_ALL=C sort
}

# ---------------------------------------------------------------------------
# Fixture
# ---------------------------------------------------------------------------

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

# build_fixture: write a consistent tree and print its root. The types stub
# defines three phases, one of them padded the way gofmt aligns a shorter name
# and with a trailing comment, beside a constant of another type. diagrams/
# holds a pair that carries the three values.
build_fixture() {
  local root value n=0
  root="$(mktemp -d "$TMP_ROOT/fixture.XXXXXX")"
  mkdir -p "$root/diagrams"
  cat >"$root/types.go" <<'EOF'
package v1alpha1

type NovaComputeNodePhase string

const (
	// NovaComputeNodePending is a selected node.
	NovaComputeNodePending NovaComputeNodePhase = "Pending"
	NovaComputeNodeActive  NovaComputeNodePhase = "Active" // registered
	NovaComputeNodeDraining NovaComputeNodePhase = "Draining"
)

const NovaComputeNodeSelectorKey = "Selector"
EOF
  {
    echo '<mxfile host="drawio" type="device"><diagram id="f" name="F"><mxGraphModel><root>'
    for value in Pending Active Draining; do
      n=$((n + 1))
      echo "<mxCell id=\"f-$n\" value=\"$value\" vertex=\"1\" parent=\"1\"/>"
    done
    echo '</root></mxGraphModel></diagram></mxfile>'
  } >"$root/diagrams/$FIGURE.drawio"
  {
    echo '<svg xmlns="http://www.w3.org/2000/svg">'
    for value in Pending Active Draining; do
      echo "<text x=\"0\" y=\"0\">$value</text>"
    done
    echo '</svg>'
  } >"$root/diagrams/$FIGURE.svg"
  printf '%s\n' "$root"
}

# rewrite <file> <sed-script>: apply a sed script in place (BSD and GNU sed).
rewrite() {
  sed "$2" "$1" >"$1.tmp" && mv "$1.tmp" "$1"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_consistent_fixture_reports_nothing() {
  echo "Test: a figure that names every phase reports nothing"

  local root
  root="$(build_fixture)"
  assert_eq "no problems" "" \
    "$(check_phase_figure "$root/types.go" "$root/diagrams")"
}

test_missing_types_file_is_the_only_report() {
  echo "Test: a types path that is not a file is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.svg"
  assert_eq "absent path" "no types file: $root/absent.go" \
    "$(check_phase_figure "$root/absent.go" "$root/diagrams")"
  assert_eq "a directory" "no types file: $root/diagrams" \
    "$(check_phase_figure "$root/diagrams" "$root/diagrams")"
}

test_types_without_phases_is_the_only_report() {
  echo "Test: a types file without a phase constant is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.drawio"
  rewrite "$root/types.go" '/NovaComputeNodePhase = /d'
  assert_eq "no phase constant" "no NovaComputeNodePhase constants in $root/types.go" \
    "$(check_phase_figure "$root/types.go" "$root/diagrams")"
}

test_missing_figure_file_skips_only_its_searches() {
  echo "Test: a missing file of the pair skips only the searches in that file"

  local root
  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.drawio"
  rewrite "$root/diagrams/$FIGURE.svg" '/>Draining</d'
  assert_eq "no drawio, the svg is still searched" \
    "missing figure: $FIGURE.drawio"$'\n'"not in svg: Draining" \
    "$(check_phase_figure "$root/types.go" "$root/diagrams")"

  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.svg"
  rewrite "$root/diagrams/$FIGURE.drawio" '/value="Pending"/d'
  assert_eq "no svg, the drawio is still searched" \
    "missing figure: $FIGURE.svg"$'\n'"not in drawio: Pending" \
    "$(check_phase_figure "$root/types.go" "$root/diagrams")"
}

# Active is the padded constant with a trailing comment, so this also proves
# that its value is read past the padding and without the comment.
test_missing_label_reports_exactly_its_line() {
  echo "Test: a phase missing from one file reports exactly its line"

  local root
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.drawio" '/value="Active"/d'
  assert_eq "a phase missing from the drawio" "not in drawio: Active" \
    "$(check_phase_figure "$root/types.go" "$root/diagrams")"

  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.svg" '/>Active</d'
  assert_eq "a phase missing from the svg" "not in svg: Active" \
    "$(check_phase_figure "$root/types.go" "$root/diagrams")"
}

test_new_phase_without_label_is_reported() {
  echo "Test: a phase added to the types without a label is reported in both files"

  local root
  root="$(build_fixture)"
  rewrite "$root/types.go" 's/^)$/	NovaComputeNodeParked NovaComputeNodePhase = "Parked"\
)/'
  assert_eq "a new phase" "not in drawio: Parked"$'\n'"not in svg: Parked" \
    "$(check_phase_figure "$root/types.go" "$root/diagrams")"
}

test_label_must_be_its_own_element() {
  echo "Test: a phase that only stands inside a longer label is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.drawio" 's#value="Draining"#value="Draining node"#'
  rewrite "$root/diagrams/$FIGURE.svg" 's#>Draining<#>Draining node<#'
  assert_eq "label inside a longer cell and text" \
    "not in drawio: Draining"$'\n'"not in svg: Draining" \
    "$(check_phase_figure "$root/types.go" "$root/diagrams")"
}

test_repository_figure_names_every_phase() {
  echo "Test: the repository's node phases figure names every NovaComputeNodePhase"

  local types="$PROJECT_ROOT/operators/nova/api/v1alpha1/novacompute_types.go"
  local copy
  assert_eq "no problems in docs/diagrams" "" \
    "$(check_phase_figure "$types" "$PROJECT_ROOT/docs/diagrams")"

  # The same check on a copy without the Conflict label proves it reads the
  # real figure.
  copy="$(mktemp -d "$TMP_ROOT/copy.XXXXXX")"
  cp "$PROJECT_ROOT/docs/diagrams/$FIGURE.drawio" "$PROJECT_ROOT/docs/diagrams/$FIGURE.svg" "$copy/"
  rewrite "$copy/$FIGURE.svg" 's#>Conflict<#>Clash<#'
  assert_eq "a copy without Conflict" "not in svg: Conflict" \
    "$(check_phase_figure "$types" "$copy")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_consistent_fixture_reports_nothing
test_missing_types_file_is_the_only_report
test_types_without_phases_is_the_only_report
test_missing_figure_file_skips_only_its_searches
test_missing_label_reports_exactly_its_line
test_new_phase_without_label_is_reported
test_label_must_be_its_own_element
test_repository_figure_names_every_phase

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
