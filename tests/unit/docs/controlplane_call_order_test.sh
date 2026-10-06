#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the pages that list the ControlPlane sub-reconcilers or their
# conditions in call order follow the order the operator calls them in.
#
# Reconcile in operators/c5c3/internal/controller/controlplane_controller.go
# calls every sub-reconciler as `return r.reconcileX(ctx, &cp)`, in the order
# RunPipeline and RunSequentialGroup run them. Three lists restate that order:
#   - the step table under `## Reconciliation Flow` in
#     docs/reference/c5c3/controlplane-reconciler.md, one row per call, its
#     Sets column naming the condition a step owns or `none`
#   - the "In call order the condition types are" sentence in
#     docs/reference/c5c3/controlplane-crd.md
#   - the condition table of Step 6 in docs/quick-start-controlplane.md
# check_call_order compares the table's Step column with the calls, then the
# sentence and the Step 6 table with the table's Sets column. It prints the
# first entry where each list parts from the one it is compared with, and
# nothing when all three agree. That the Step 6 table names every entry of
# subConditionTypes is checked by quick_start_controlplane_nova_test.sh. The
# fixture tests prove each check on a scratch tree before the last test runs
# it on the repository.
#
# Usage: bash tests/unit/docs/controlplane_call_order_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/quick_start_controlplane.sh
source "$PROJECT_ROOT/tests/lib/quick_start_controlplane.sh"

# reconcile_calls <controller.go>: the sub-reconcilers Reconcile calls, one per
# line, in call order.
reconcile_calls() {
  grep -oE 'return r\.reconcile[A-Za-z]+\(ctx, &cp\)' "$1" |
    sed -E 's/return r\.(reconcile[A-Za-z]+).*/\1/'
}

# step_table <reconciler.md> <field>: one cell of every numbered row of the
# step table under `## Reconciliation Flow`, without blanks and backticks.
# Field 3 is the Step column, field 4 the Sets column.
step_table() {
  awk -F '|' -v field="$2" '
    /^## Reconciliation Flow$/ { in_flow = 1; next }
    in_flow && /^##+ / { exit }
    in_flow && /^\| [0-9]+ \| / { cell = $field; gsub(/[ `]/, "", cell); print cell }
  ' "$1"
}

# condition_sentence <crd.md>: the condition types the "In call order"
# sentence names, one per line.
condition_sentence() {
  awk '
    /^In call order the condition types are / { in_sentence = 1 }
    in_sentence && /^$/ { exit }
    in_sentence { print }
  ' "$1" | grep -oE '`[A-Za-z]+`' | tr -d '`'
}

# first_difference <label> <want> <have>: print the first entry where the
# newline-separated list <have> parts from <want>, and nothing when they agree.
first_difference() {
  local label="$1" want="$2" have="$3" n=0 w h
  [[ "$want" == "$have" ]] && return
  while IFS='|' read -r w h; do
    n=$((n + 1))
    if [[ "$w" != "$h" ]]; then
      echo "$label $n: ${h:-nothing}, want ${w:-nothing}"
      return
    fi
  done < <(paste -d '|' <(printf '%s\n' "$want") <(printf '%s\n' "$have"))
}

# check_call_order <controller.go> <reconciler.md> <crd.md> <quick-start.md>:
# print one problem per list, and nothing when the three lists follow the
# calls. A controller without a call or a page without the step table is the
# only line.
check_call_order() {
  local controller="$1" reconciler="$2" crd="$3" quick_start="$4"
  local calls steps sets

  calls="$(reconcile_calls "$controller")"
  if [[ -z "$calls" ]]; then
    echo "no sub-reconciler calls in $controller"
    return
  fi
  steps="$(step_table "$reconciler" 3)"
  if [[ -z "$steps" ]]; then
    echo "no step table in $reconciler"
    return
  fi
  sets="$(step_table "$reconciler" 4 | grep -vx 'none')"

  first_difference "step table row" "$calls" "$steps"
  first_difference "condition sentence entry" "$sets" "$(condition_sentence "$crd")"
  first_difference "Step 6 table row" "$sets" \
    "$(step6_chain "$quick_start" | awk -F ' → ' '{ for (i = 1; i <= NF; i++) print $i }')"
}

# ---------------------------------------------------------------------------
# Fixture
# ---------------------------------------------------------------------------

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

# build_fixture: write a consistent tree and print its root. The controller
# stub calls reconcileDelete, which is no sub-reconciler, and four steps;
# reconcileRefresh sets no condition. The step table is followed by a heading
# and a numbered row of another table, the sentence by a paragraph that names
# another condition, and the Step 6 table by the next step.
build_fixture() {
  local root
  root="$(mktemp -d "$TMP_ROOT/fixture.XXXXXX")"
  cat >"$root/controller.go" <<'EOF'
package controller

func (r *ControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if result, err := r.reconcileDelete(ctx, &cp); !result.IsZero() || err != nil {
		return result, err
	}
	steps := []func() (ctrl.Result, error){
		func() (ctrl.Result, error) {
			return r.reconcileAlpha(ctx, &cp)
		},
		func() (ctrl.Result, error) {
			return r.reconcileBeta(ctx, &cp)
		},
		func() (ctrl.Result, error) {
			return r.reconcileRefresh(ctx, &cp)
		},
		func() (ctrl.Result, error) {
			return r.reconcileGamma(ctx, &cp)
		},
	}
}
EOF
  cat >"$root/reconciler.md" <<'EOF'
## Reconciliation Flow

| # | Step | Sets | Gate | Does | Requeue |
| --- | --- | --- | --- | --- | --- |
| 1 | `reconcileAlpha` | `AlphaReady` | nothing | a | none |
| 2 | `reconcileBeta` | `BetaReady` | `AlphaReady` | b | 5s |
| 3 | `reconcileRefresh` | none | `BetaReady` | c | none |
| 4 | `reconcileGamma` | `GammaReady` | nothing | d | 10s |

### Execution Model

| 1 | `reconcileOther` | `OtherReady` | x | y | z |
EOF
  cat >"$root/crd.md" <<'EOF'
In call order the condition types are `AlphaReady`, `BetaReady` and
`GammaReady`.

`OtherReady` stands in the next paragraph.
EOF
  cat >"$root/quick-start.md" <<'EOF'
## Step 6 — Watch the chain reconcile

| Condition | Waits for |
| --- | --- |
| `AlphaReady` | Nothing |
| `BetaReady` | `AlphaReady` |
| `GammaReady` | Nothing |

## Step 7 — Use it
EOF
  printf '%s\n' "$root"
}

# check_fixture <root>: run check_call_order on the four files of a fixture.
check_fixture() {
  check_call_order "$1/controller.go" "$1/reconciler.md" "$1/crd.md" "$1/quick-start.md"
}

# rewrite <file> <sed-script>: apply a sed script in place (BSD and GNU sed).
rewrite() {
  sed "$2" "$1" >"$1.tmp" && mv "$1.tmp" "$1"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_consistent_fixture_reports_nothing() {
  echo "Test: lists that follow the calls report nothing"

  local root
  root="$(build_fixture)"
  assert_eq "no problems" "" "$(check_fixture "$root")"
}

test_missing_calls_or_table_is_the_only_report() {
  echo "Test: a controller without a call or a page without the step table is the only report"

  local root
  root="$(build_fixture)"
  rewrite "$root/controller.go" '/return r\.reconcile/d'
  assert_eq "no calls" "no sub-reconciler calls in $root/controller.go" \
    "$(check_fixture "$root")"

  root="$(build_fixture)"
  rewrite "$root/reconciler.md" 's/^## Reconciliation Flow$/## Flow/'
  assert_eq "no step table" "no step table in $root/reconciler.md" \
    "$(check_fixture "$root")"
}

test_reordered_calls_are_reported() {
  echo "Test: calls the code reorders are reported at the first row the table lists differently"

  local root
  root="$(build_fixture)"
  rewrite "$root/controller.go" 's/reconcileAlpha/reconcileSwap/; s/reconcileBeta/reconcileAlpha/; s/reconcileSwap/reconcileBeta/'
  assert_eq "Beta called before Alpha" "step table row 1: reconcileAlpha, want reconcileBeta" \
    "$(check_fixture "$root")"
}

# reconcileRefresh sets no condition, so a missing row of it moves only the
# Step column.
test_missing_step_row_is_reported() {
  echo "Test: a step the table leaves out is reported at its row"

  local root
  root="$(build_fixture)"
  rewrite "$root/reconciler.md" '/`reconcileRefresh`/d'
  assert_eq "no Refresh row" "step table row 3: reconcileGamma, want reconcileRefresh" \
    "$(check_fixture "$root")"
}

test_swapped_sentence_entries_are_reported() {
  echo "Test: two conditions the sentence swaps are reported at the first of them"

  local root
  root="$(build_fixture)"
  rewrite "$root/crd.md" 's/`AlphaReady`, `BetaReady`/`BetaReady`, `AlphaReady`/'
  assert_eq "Beta named before Alpha" "condition sentence entry 1: BetaReady, want AlphaReady" \
    "$(check_fixture "$root")"
}

test_missing_step6_row_is_reported() {
  echo "Test: a condition the Step 6 table leaves out is reported at its row"

  local root
  root="$(build_fixture)"
  rewrite "$root/quick-start.md" '/^| `BetaReady` |/d'
  assert_eq "no BetaReady row" "Step 6 table row 2: GammaReady, want BetaReady" \
    "$(check_fixture "$root")"
}

test_repository_lists_follow_the_calls() {
  echo "Test: the repository's three lists follow the order Reconcile calls the sub-reconcilers in"

  assert_eq "no problems in the reconciler reference, the CRD reference and the quick start" "" \
    "$(check_call_order \
      "$PROJECT_ROOT/operators/c5c3/internal/controller/controlplane_controller.go" \
      "$PROJECT_ROOT/docs/reference/c5c3/controlplane-reconciler.md" \
      "$PROJECT_ROOT/docs/reference/c5c3/controlplane-crd.md" \
      "$PROJECT_ROOT/docs/quick-start-controlplane.md")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_consistent_fixture_reports_nothing
test_missing_calls_or_table_is_the_only_report
test_reordered_calls_are_reported
test_missing_step_row_is_reported
test_swapped_sentence_entries_are_reported
test_missing_step6_row_is_reported
test_repository_lists_follow_the_calls

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
