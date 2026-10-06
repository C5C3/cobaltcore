#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the ControlPlane gate graph names every condition the operator
# aggregates.
#
# docs/diagrams/controlplane-gate-graph.drawio and .svg draw the conditions of
# a ControlPlane. subConditionTypes in
# operators/c5c3/internal/controller/controlplane_controller.go lists them:
# one entry per line, either a constant defined in the same file as
# `<constant> = "<value>"` or a quoted string. check_gate_graph resolves every
# entry and looks for its value in both files of the pair as a label of its
# own: in the .drawio as a cell value, in the .svg as a <text> element. It
# prints one problem per line and nothing when the figure names every
# condition. The check runs one way: a label the figure has and the slice
# lacks is not reported. The fixture tests prove each check on a scratch tree
# before the last test runs it on the repository.
#
# Usage: bash tests/unit/docs/controlplane_gate_graph_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

FIGURE="controlplane-gate-graph"

# check_gate_graph <controller.go> <diagrams-dir>: print one problem per line,
# sorted, and nothing when both files of the figure name every entry of
# subConditionTypes. A missing controller or an empty slice is the only line.
check_gate_graph() {
  local controller="$1" dir="$2"
  local drawio="$dir/$FIGURE.drawio" svg="$dir/$FIGURE.svg"

  if [[ ! -f "$controller" ]]; then
    echo "no controller: $controller"
    return
  fi

  local entries
  entries="$(awk '
    /^var subConditionTypes = \[\]string\{/ { in_slice = 1; next }
    in_slice && /^\}/ { exit }
    in_slice {
      sub(/^[ \t]+/, "")
      sub(/,?[ \t]*$/, "")
      if ($0 != "") print
    }
  ' "$controller")"
  if [[ -z "$entries" ]]; then
    echo "no subConditionTypes in $controller"
    return
  fi

  {
    [[ -f "$drawio" ]] || echo "missing figure: $FIGURE.drawio"
    [[ -f "$svg" ]] || echo "missing figure: $FIGURE.svg"
    local entry value
    while IFS= read -r entry; do
      case "$entry" in
        \"*\")
          value="${entry#\"}"
          value="${value%\"}"
          ;;
        *)
          value="$(awk -v name="$entry" '
            $1 == name && $2 == "=" { v = $3; gsub(/"/, "", v); print v; exit }
          ' "$controller")"
          if [[ -z "$value" ]]; then
            echo "unresolved constant: $entry"
            continue
          fi
          ;;
      esac
      if [[ -f "$drawio" ]] && ! grep -F -q -- "value=\"$value\"" "$drawio"; then
        echo "not in drawio: $value"
      fi
      if [[ -f "$svg" ]] && ! grep -F -q -- ">$value<" "$svg"; then
        echo "not in svg: $value"
      fi
    done <<<"$entries"
  } | LC_ALL=C sort
}

# ---------------------------------------------------------------------------
# Fixture
# ---------------------------------------------------------------------------

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

# build_fixture: write a consistent tree and print its root. The controller
# stub defines three condition constants, one of them with a trailing
# //nolint comment, and lists them plus one quoted string in
# subConditionTypes. diagrams/ holds a pair that carries the four values.
build_fixture() {
  local root value n=0
  root="$(mktemp -d "$TMP_ROOT/fixture.XXXXXX")"
  mkdir -p "$root/diagrams"
  cat >"$root/controller.go" <<'EOF'
package controller

const (
	conditionTypeAlphaReady = "AlphaReady"
	conditionTypeBetaReady  = "BetaReady" //nolint:gosec // G101 false positive: condition type name, not a credential.
	conditionTypeGammaReady = "GammaReady"
)

var subConditionTypes = []string{
	conditionTypeAlphaReady,
	conditionTypeBetaReady,
	conditionTypeGammaReady,
	"DeltaReady",
}
EOF
  {
    echo '<mxfile host="drawio" type="device"><diagram id="f" name="F"><mxGraphModel><root>'
    for value in AlphaReady BetaReady GammaReady DeltaReady; do
      n=$((n + 1))
      echo "<mxCell id=\"f-$n\" value=\"$value\" vertex=\"1\" parent=\"1\"/>"
    done
    echo '</root></mxGraphModel></diagram></mxfile>'
  } >"$root/diagrams/$FIGURE.drawio"
  {
    echo '<svg xmlns="http://www.w3.org/2000/svg">'
    for value in AlphaReady BetaReady GammaReady DeltaReady; do
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
  echo "Test: a figure that names every condition reports nothing"

  local root
  root="$(build_fixture)"
  assert_eq "no problems" "" \
    "$(check_gate_graph "$root/controller.go" "$root/diagrams")"
}

test_missing_controller_is_the_only_report() {
  echo "Test: a controller path that is not a file is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.svg"
  assert_eq "absent path" "no controller: $root/absent.go" \
    "$(check_gate_graph "$root/absent.go" "$root/diagrams")"
  assert_eq "a directory" "no controller: $root/diagrams" \
    "$(check_gate_graph "$root/diagrams" "$root/diagrams")"
}

test_missing_or_empty_slice_is_the_only_report() {
  echo "Test: a controller without subConditionTypes entries is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.drawio"
  rewrite "$root/controller.go" '/^var subConditionTypes/,/^}/d'
  assert_eq "no subConditionTypes block" "no subConditionTypes in $root/controller.go" \
    "$(check_gate_graph "$root/controller.go" "$root/diagrams")"

  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.drawio"
  rewrite "$root/controller.go" '/^var subConditionTypes/,/^}/{/^[[:space:]]/d;}'
  assert_eq "a block without an entry" "no subConditionTypes in $root/controller.go" \
    "$(check_gate_graph "$root/controller.go" "$root/diagrams")"
}

test_unresolved_constant_keeps_checking_the_rest() {
  echo "Test: an entry without a string definition is reported and the others are still checked"

  local root
  root="$(build_fixture)"
  rewrite "$root/controller.go" '/conditionTypeAlphaReady = /d'
  rewrite "$root/diagrams/$FIGURE.svg" '/>GammaReady</d'
  assert_eq "unresolved constant and the missing svg label" \
    "not in svg: GammaReady"$'\n'"unresolved constant: conditionTypeAlphaReady" \
    "$(check_gate_graph "$root/controller.go" "$root/diagrams")"
}

test_missing_figure_file_skips_only_its_searches() {
  echo "Test: a missing file of the pair skips only the searches in that file"

  local root
  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.drawio"
  rewrite "$root/diagrams/$FIGURE.svg" '/>GammaReady</d'
  assert_eq "no drawio, the svg is still searched" \
    "missing figure: $FIGURE.drawio"$'\n'"not in svg: GammaReady" \
    "$(check_gate_graph "$root/controller.go" "$root/diagrams")"

  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.svg"
  rewrite "$root/diagrams/$FIGURE.drawio" '/value="AlphaReady"/d'
  assert_eq "no svg, the drawio is still searched" \
    "missing figure: $FIGURE.svg"$'\n'"not in drawio: AlphaReady" \
    "$(check_gate_graph "$root/controller.go" "$root/diagrams")"
}

# BetaReady is the //nolint constant and DeltaReady the quoted entry, so these
# also prove that both resolve to their bare value.
test_missing_value_reports_exactly_its_line() {
  echo "Test: a value missing from one file reports exactly its line"

  local root
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.drawio" '/value="BetaReady"/d'
  assert_eq "the //nolint constant missing from the drawio" "not in drawio: BetaReady" \
    "$(check_gate_graph "$root/controller.go" "$root/diagrams")"

  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.svg" '/>DeltaReady</d'
  assert_eq "the quoted entry missing from the svg" "not in svg: DeltaReady" \
    "$(check_gate_graph "$root/controller.go" "$root/diagrams")"
}

test_svg_label_must_be_its_own_text_element() {
  echo "Test: a value that only stands inside a longer svg label is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.svg" 's#>GammaReady<#>GammaReady and more<#'
  assert_eq "label inside a longer text" "not in svg: GammaReady" \
    "$(check_gate_graph "$root/controller.go" "$root/diagrams")"
}

test_drawio_label_must_be_its_own_cell() {
  echo "Test: a value that only stands inside a longer drawio label is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.drawio" 's#value="GammaReady"#value="GammaReady and more"#'
  assert_eq "label inside a longer cell" "not in drawio: GammaReady" \
    "$(check_gate_graph "$root/controller.go" "$root/diagrams")"
}

test_extra_figure_label_is_not_reported() {
  echo "Test: a label the figure has and the slice lacks is not reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.svg" 's#^</svg>$#<text x="0" y="0">EpsilonReady</text></svg>#'
  rewrite "$root/diagrams/$FIGURE.drawio" 's#</root>#<mxCell id="f-9" value="EpsilonReady"/></root>#'
  assert_eq "no problems" "" \
    "$(check_gate_graph "$root/controller.go" "$root/diagrams")"
}

test_repository_figure_names_every_condition() {
  echo "Test: the repository's gate graph names every condition of subConditionTypes"

  local controller="$PROJECT_ROOT/operators/c5c3/internal/controller/controlplane_controller.go"
  local copy
  assert_eq "no problems in docs/diagrams" "" \
    "$(check_gate_graph "$controller" "$PROJECT_ROOT/docs/diagrams")"

  # The same check on a copy without the NovaReady label proves it reads the
  # real figure.
  copy="$(mktemp -d "$TMP_ROOT/copy.XXXXXX")"
  cp "$PROJECT_ROOT/docs/diagrams/$FIGURE.drawio" "$PROJECT_ROOT/docs/diagrams/$FIGURE.svg" "$copy/"
  rewrite "$copy/$FIGURE.svg" '/>NovaReady</d'
  assert_eq "a copy without NovaReady" "not in svg: NovaReady" \
    "$(check_gate_graph "$controller" "$copy")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_consistent_fixture_reports_nothing
test_missing_controller_is_the_only_report
test_missing_or_empty_slice_is_the_only_report
test_unresolved_constant_keeps_checking_the_rest
test_missing_figure_file_skips_only_its_searches
test_missing_value_reports_exactly_its_line
test_svg_label_must_be_its_own_text_element
test_drawio_label_must_be_its_own_cell
test_extra_figure_label_is_not_reported
test_repository_figure_names_every_condition

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
