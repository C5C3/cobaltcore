#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify every pipeline list of the service reconciler pages names the steps
# of its controller, in call order, and that the counts and the figure drawn
# from those steps follow the controller too.
#
# A service controller runs a []commonreconcile.Step slice. A step is named by
# the `Name:` field of a composite literal that also holds an `Fn:` field, in
# whatever order and on whatever lines the fields stand; the literal of a
# parallel-group member also holds `ConditionType:`. File order is call order.
# The reconciler pages under docs/reference/ restate that order per pipeline:
# each section holds a step table, whose header row starts with `| Step |` and
# whose first column names the step, and most of them also a fenced block that
# draws the pipeline. The Keystone pipeline has no block; its figure replaced
# it. A row whose first cell starts with `(`, such as `(prune)`, is a step
# without a name, and text in parentheses inside a block, such as
# `(parallel)`, is a note.
#
# check_pipeline prints one problem per line and nothing when the table and
# the block follow the controller. check_count does the same for a sentence
# that counts the steps or the sub-conditions of a pipeline, and
# check_pipeline_figure for a figure that draws one. The fixture tests prove
# each check on a scratch tree before the repository tests at the end run them
# on every pipeline, count and figure their tables list.
#
# REFERENCE_DOCS overrides the directory the pages are read from (default:
# docs/reference), so a check can run on a scratch copy of the pages.
#
# Usage: bash tests/unit/docs/service_pipeline_steps_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
REFERENCE_DOCS="${REFERENCE_DOCS:-$PROJECT_ROOT/docs/reference}"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# pipeline_steps <controller.go> [parallel]: the step names in call order, one
# per line; with parallel, only those of the parallel-group members. Every
# brace opens or closes a literal, except one inside a string or a comment, so
# a `Name:` field belongs to the literal it stands in, whatever line holds it.
# A name given as a constant is resolved through the line that assigns the
# constant a string in the same file; a constant without such a line prints
# as ?<constant>.
pipeline_steps() {
  awk -v only="${2:-}" '
    { line[NR] = $0 }
    $0 ~ /^[ \t]*(const[ \t]+)?[A-Za-z_][A-Za-z0-9_]*[ \t]*=[ \t]*"[^"]*"/ {
      s = $0
      sub(/^[ \t]*(const[ \t]+)?/, "", s)
      id = s
      sub(/[ \t=].*$/, "", id)
      v = s
      sub(/^[^"]*"/, "", v)
      sub(/".*$/, "", v)
      value[id] = v
    }
    END {
      for (i = 1; i <= NR; i++) {
        rest = line[i]
        gsub(/\\./, "", rest)
        code = ""
        while (match(rest, /"[^"]*"/)) {
          str = substr(rest, RSTART, RLENGTH)
          gsub(/[{}:\/]/, " ", str)
          code = code substr(rest, 1, RSTART - 1) str
          rest = substr(rest, RSTART + RLENGTH)
        }
        code = code rest
        sub(/\/\/.*$/, "", code)
        while (match(code, /[{}]|[A-Za-z_][A-Za-z0-9_]*:/)) {
          tok = substr(code, RSTART, RLENGTH)
          code = substr(code, RSTART + RLENGTH)
          if (tok == "{") {
            lit[++depth] = ++lits
          } else if (tok == "}") {
            depth--
          } else if (tok == "Fn:") {
            fn[lit[depth]] = 1
          } else if (tok == "ConditionType:") {
            member[lit[depth]] = 1
          } else if (tok == "Name:") {
            s = code
            sub(/^[ \t]+/, "", s)
            if (substr(s, 1, 1) == "\"") {
              s = substr(s, 2)
              sub(/".*$/, "", s)
            } else {
              sub(/[^A-Za-z0-9_].*$/, "", s)
              s = (s in value) ? value[s] : "?" s
            }
            names++
            name[names] = s
            of[names] = lit[depth]
          }
        }
      }
      for (k = 1; k <= names; k++)
        if (fn[of[k]] && (only != "parallel" || member[of[k]])) print name[k]
    }
  ' "$1"
}

# section_lines <page.md> <heading>: the lines of the section that starts at
# the line equal to <heading> and ends before the next heading of the same or
# a higher level. A line inside a code fence is no heading. Exits 1 when no
# line equals <heading>.
section_lines() {
  awk -v heading="$2" '
    function level(l,   n) { n = 0; while (substr(l, n + 1, 1) == "#") n++; return n }
    !found && $0 == heading { found = 1; want = level(heading); fence = ""; next }
    !found { next }
    {
      t = $0
      sub(/^[ \t]+/, "", t)
      m = substr(t, 1, 3)
      if (fence == "" && (m == "```" || m == "~~~")) fence = m
      else if (fence != "" && m == fence) fence = ""
      else if (fence == "" && $0 ~ /^#+ / && level($0) <= want) exit
      print
    }
    END { if (!found) exit 1 }
  ' "$1"
}

# step_table: read a section on stdin and print the first cell of every body
# row of its first table whose header row starts with `| Step |`, without
# blanks and backticks. A cell that starts with `(` is dropped.
step_table() {
  awk -F '|' '
    { t = $0; sub(/^[ \t]+/, "", t); m = substr(t, 1, 3) }
    fence == "" && (m == "```" || m == "~~~") { fence = m; next }
    fence != "" { if (m == fence) fence = ""; next }
    state == 0 && /^\| Step \|/ { state = 1; next }
    state == 1 { state = 2; next }
    state == 2 && !/^\|/ { exit }
    state == 2 {
      cell = $2
      gsub(/[ \t`]/, "", cell)
      if (substr(cell, 1, 1) != "(") print cell
    }
  '
}

# first_fence: read a section on stdin and print the lines inside its first
# code fence. Exits 1 when the section has none.
first_fence() {
  awk '
    { t = $0; sub(/^[ \t]+/, "", t); m = substr(t, 1, 3) }
    fence == "" && (m == "```" || m == "~~~") { fence = m; found = 1; next }
    fence != "" && m == fence { exit }
    fence != "" { print }
    END { if (!found) exit 1 }
  '
}

# block_names: read a pipeline block on stdin and print its names in reading
# order. Text in parentheses is removed, then every run of letters is a name.
block_names() {
  LC_ALL=C sed -e 's/([^)]*)//g' | LC_ALL=C grep -oE '[A-Za-z]+'
}

# first_difference <label> <want> <have>: print the first entry where the
# newline-separated list <have> parts from <want>, and nothing when they agree.
# A wanted entry ?<constant> is an unresolved step name and accepts any entry.
first_difference() {
  local label="$1" want="$2" have="$3" n=0 w h
  [[ "$want" == "$have" ]] && return
  while IFS='|' read -r w h; do
    n=$((n + 1))
    if [[ "$w" == \?* && -n "$h" ]]; then
      continue
    fi
    if [[ "$w" != "$h" ]]; then
      w="${w#\?}"
      echo "$label $n: ${h:-nothing}, want ${w:-nothing}"
      return
    fi
  done < <(paste -d '|' <(printf '%s\n' "$want") <(printf '%s\n' "$have"))
}

# check_pipeline <controller.go> <page.md> <heading> <block>: print one problem
# per line, and nothing when the step table and, with <block> yes, the pipeline
# block of the section follow the controller. A missing controller, a
# controller without steps, a missing page and a missing section end the check
# at once.
check_pipeline() {
  local controller="$1" page="$2" heading="$3" block="$4"
  local steps step section table fence

  if [[ ! -f "$controller" ]]; then
    echo "no controller: $controller"
    return
  fi
  steps="$(pipeline_steps "$controller")"
  if [[ -z "$steps" ]]; then
    echo "no steps in $controller"
    return
  fi
  while IFS= read -r step; do
    if [[ "$step" == \?* ]]; then
      echo "unresolved step name: ${step#\?} in $controller"
    fi
  done <<<"$steps"
  if [[ ! -f "$page" ]]; then
    echo "no page: $page"
    return
  fi
  if ! section="$(section_lines "$page" "$heading")"; then
    echo "no section: $heading in $page"
    return
  fi

  table="$(step_table <<<"$section")"
  if [[ -z "$table" ]]; then
    echo "no step table: $heading in $page"
  else
    first_difference "step table of $page $heading, entry" "$steps" "$table"
  fi

  [[ "$block" == "yes" ]] || return
  if ! fence="$(first_fence <<<"$section")"; then
    echo "no pipeline block: $heading in $page"
    return
  fi
  first_difference "pipeline block of $page $heading, entry" "$steps" "$(block_names <<<"$fence")"
}

# number_word <n>: print the word for a number from one to twenty, and the
# digits for any other number.
number_word() {
  local words=(one two three four five six seven eight nine ten eleven twelve
    thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty)
  if (($1 >= 1 && $1 <= 20)); then
    echo "${words[$1 - 1]}"
  else
    echo "$1"
  fi
}

# condition_types <controller.go>: the entries of the list the aggregate Ready
# of the controller follows, `var subConditionTypes = []string{` or
# `var <kind>SubConditionTypes = []string{`, one per line, without comment
# lines.
condition_types() {
  awk '
    /^var ([A-Za-z]+S|s)ubConditionTypes = \[\]string\{/ { in_list = 1; next }
    in_list && /^\}/ { exit }
    in_list {
      sub(/^[ \t]+/, "")
      sub(/,?[ \t]*$/, "")
      if ($0 != "" && substr($0, 1, 2) != "//") print
    }
  ' "$1"
}

# check_count <controller.go> <page.md> <phrase>: print a problem when the
# page, its lines joined by single spaces and read without regard to case,
# lacks <phrase> with each placeholder replaced by the number word of its
# count: {steps} the named steps of the controller, {parallel} the
# parallel-group members among them, {sequential} the other named steps and
# {conditions} the entries of its sub-condition list. A missing controller and
# a missing page are the only report.
check_count() {
  local controller="$1" page="$2" phrase="$3"
  local steps parallel conditions want

  if [[ ! -f "$controller" ]]; then
    echo "no controller: $controller"
    return
  fi
  if [[ ! -f "$page" ]]; then
    echo "no page: $page"
    return
  fi
  steps="$(pipeline_steps "$controller" | grep -c .)"
  parallel="$(pipeline_steps "$controller" parallel | grep -c .)"
  conditions="$(condition_types "$controller" | grep -c .)"
  want="${phrase//\{steps\}/$(number_word "$steps")}"
  want="${want//\{parallel\}/$(number_word "$parallel")}"
  want="${want//\{sequential\}/$(number_word $((steps - parallel)))}"
  want="${want//\{conditions\}/$(number_word "$conditions")}"
  if ! tr -s ' \t\n' '   ' <"$page" | grep -iqF -- "$want"; then
    echo "count of $page: no \"$want\""
  fi
}

# check_pipeline_figure <controller.go> <figure> <page.md>: print one problem
# per line, and nothing when the figure names every step of the controller: in
# <figure>.drawio as a cell value of its own, in <figure>.svg as a <text>
# element of its own, and as a word in the alt text of the image on <page.md>
# that shows <figure>.svg. The check runs one way: a label the figure has and
# the controller lacks is not reported. A missing controller and a missing page
# are the only report.
check_pipeline_figure() {
  local controller="$1" figure="$2" page="$3"
  local drawio="$2.drawio" svg="$2.svg" alt step

  if [[ ! -f "$controller" ]]; then
    echo "no controller: $controller"
    return
  fi
  if [[ ! -f "$page" ]]; then
    echo "no page: $page"
    return
  fi
  [[ -f "$drawio" ]] || echo "missing figure: $drawio"
  [[ -f "$svg" ]] || echo "missing figure: $svg"
  alt="$(sed -n "s|^!\[\(.*\)\](.*/${figure##*/}\.svg)\$|\1|p" "$page")"
  [[ -n "$alt" ]] || echo "no image of ${figure##*/}.svg on $page"
  while IFS= read -r step; do
    if [[ -f "$drawio" ]] && ! grep -qF -- "value=\"$step\"" "$drawio"; then
      echo "not in drawio: $step"
    fi
    if [[ -f "$svg" ]] && ! grep -qF -- ">$step</text>" "$svg"; then
      echo "not in svg: $step"
    fi
    if [[ -n "$alt" ]] && ! grep -qwF -- "$step" <<<"$alt"; then
      echo "not in the alt text: $step"
    fi
  done < <(pipeline_steps "$controller")
}

# ---------------------------------------------------------------------------
# Fixture
# ---------------------------------------------------------------------------

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

HEADING="## Pipeline"

# build_fixture: write a consistent tree and print its root. The controller
# stub aggregates Ready over five sub-conditions, a comment line among them, and
# runs a slice of four named entries, one of them named by a constant and one
# with its fields on lines of their own, an unnamed prune entry, and a parallel
# group of two members: one with Name: on the line above ConditionType:, one
# with its fields reordered around a comment line and a blank line and a brace
# in a comment and in a string of its function. An owner reference carries a
# Name: field that names no step. The page counts the steps and the
# sub-conditions over a line break, draws the six steps with a (parallel) note,
# shows the figure with an alt text naming them, runs a command block with a
# comment line before its step table, lists a (prune) row, and has another
# block and table under the next heading. The figure names each step in a cell
# and a <text> element of its own, beside a longer label that holds the name.
build_fixture() {
  local root step
  root="$(mktemp -d "$TMP_ROOT/fixture.XXXXXX")"
  cat >"$root/controller.go" <<'EOF'
package controller

const (
	stepBeta    = "Beta"
	stepUnused  = "Unused"
)

var subConditionTypes = []string{
	"AlphaReady",
	"BetaReady",
	"GammaReady",
	"DeltaReady",
	// Zeta reports through EpsilonReady.
	"EpsilonReady",
}

func (r *FixtureReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	owner := metav1.OwnerReference{Name: "fixture-owner", Kind: "Fixture"}
	pipeline := []commonreconcile.Step{
		{Name: "Alpha", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileAlpha(ctx, owner)
		}},
		{Name: stepBeta, Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileBeta(ctx)
		}},
		{Fn: func(ctx context.Context) (ctrl.Result, error) {
			return ctrl.Result{}, r.prune(ctx)
		}},
		{Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileParallelGroup(ctx, []commonreconcile.ParallelStep[*v1.Fixture]{
				{
					Name:          "Gamma",
					ConditionType: "GammaReady",
					Fn:            r.reconcileGamma,
				},
				{
					// Delta reads what Gamma wrote.
					ConditionType: "DeltaReady",

					Fn: func(ctx context.Context, f *v1.Fixture) (ctrl.Result, error) {
						// A } in a comment closes nothing, nor does one in a string.
						return r.reconcileDelta(ctx, f, "}")
					},
					Name: "Delta",
				},
			})
		}},
		{
			Name: "Zeta",
			Fn: func(ctx context.Context) (ctrl.Result, error) {
				return r.reconcileZeta(ctx)
			},
		},
		{Name: "Epsilon", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileEpsilon(ctx)
		}},
	}
	result, err := commonreconcile.RunPipeline(ctx, instrumenter.Instrument, pipeline)
	return r.updateStatus(ctx, result, err)
}
EOF
  cat >"$root/page.md" <<'EOF'
# Fixture Reconciler

The fixture controller runs four sequential sub-reconcilers and a parallel
group of two. Six steps run in all, and Ready aggregates five sub-conditions.

## Pipeline

```text
Alpha ──► Beta ──► (prune) ──► ┬─ Gamma
                               └─ Delta   (parallel)
──► Zeta ──► Epsilon
```

![The fixture pipeline: Alpha, Beta, an unnamed prune step, a parallel group of Gamma and Delta, then Zeta and Epsilon.](./figure.svg)

```bash
# Show the conditions the steps set
kubectl get fixture demo
```

| Step | What it does | Condition |
| --- | --- | --- |
| Alpha | a | `AlphaReady` |
| `Beta` | b | `BetaReady` |
| (prune) | c | none |
| Gamma | d | `GammaReady` |
| Delta | e | `DeltaReady` |
| Zeta | z | `EpsilonReady` |
| Epsilon | f | `EpsilonReady` |

### Execution Model

The steps run in the order above.

## Other

```text
Other ──► Thing
```

| Step | What it does | Condition |
| --- | --- | --- |
| Other | g | `OtherReady` |
EOF
  {
    echo '<mxfile><diagram id="f" name="F"><mxGraphModel><root>'
    for step in Alpha Beta Gamma Delta Zeta Epsilon; do
      echo "<mxCell id=\"$step\" value=\"$step\" vertex=\"1\" parent=\"1\"/>"
      echo "<mxCell id=\"$step-l\" value=\"the $step step\" vertex=\"1\" parent=\"1\"/>"
    done
    echo '</root></mxGraphModel></diagram></mxfile>'
  } >"$root/figure.drawio"
  {
    echo '<svg xmlns="http://www.w3.org/2000/svg">'
    for step in Alpha Beta Gamma Delta Zeta Epsilon; do
      echo "<text x=\"0\" y=\"0\">$step</text>"
      echo "<text x=\"0\" y=\"0\">the $step step</text>"
    done
    echo '</svg>'
  } >"$root/figure.svg"
  printf '%s\n' "$root"
}

# check_fixture <root> [<block>]: run check_pipeline on the fixture.
check_fixture() {
  check_pipeline "$1/controller.go" "$1/page.md" "$HEADING" "${2:-yes}"
}

# check_fixture_figure <root>: run check_pipeline_figure on the fixture.
check_fixture_figure() {
  check_pipeline_figure "$1/controller.go" "$1/figure" "$1/page.md"
}

# rewrite <file> <sed-script>: apply a sed script in place (BSD and GNU sed).
rewrite() {
  sed "$2" "$1" >"$1.tmp" && mv "$1.tmp" "$1"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_consistent_fixture_reports_nothing() {
  echo "Test: a table, a block and a figure that follow the controller report nothing"

  local root
  root="$(build_fixture)"
  assert_eq "no problems" "" "$(check_fixture "$root")"
  assert_eq "no figure problems" "" "$(check_fixture_figure "$root")"
}

test_steps_are_read_from_every_entry_form() {
  echo "Test: a step entry is read inline, over several lines and with its fields in any order"

  local root
  root="$(build_fixture)"
  assert_eq "every named step in call order" "Alpha
Beta
Gamma
Delta
Zeta
Epsilon" "$(pipeline_steps "$root/controller.go")"
  assert_eq "the parallel-group members" "Gamma
Delta" "$(pipeline_steps "$root/controller.go" parallel)"

  rewrite "$root/page.md" '/^| Zeta |/d'
  assert_eq "no row for the multi-line Zeta" \
    "step table of $root/page.md $HEADING, entry 5: Epsilon, want Zeta" \
    "$(check_fixture "$root")"
}

test_missing_controller_is_the_only_report() {
  echo "Test: a controller path that is no file is the only report"

  local root
  root="$(build_fixture)"
  assert_eq "no controller" "no controller: $root/missing.go" \
    "$(check_pipeline "$root/missing.go" "$root/page.md" "$HEADING" yes)"
}

test_controller_without_steps_is_the_only_report() {
  echo "Test: a controller without a step entry is the only report"

  local root
  root="$(build_fixture)"
  rewrite "$root/controller.go" '/Name:/d'
  assert_eq "no steps" "no steps in $root/controller.go" "$(check_fixture "$root")"
}

test_unresolved_constant_is_reported_and_the_rest_compared() {
  echo "Test: a step constant without a string is reported, and the other steps are still compared"

  local root
  root="$(build_fixture)"
  rewrite "$root/controller.go" '/stepBeta *= /d'
  assert_eq "unresolved stepBeta" "unresolved step name: stepBeta in $root/controller.go" \
    "$(check_fixture "$root")"

  rewrite "$root/page.md" '/^| Gamma |/d'
  assert_eq "unresolved stepBeta, then the missing Gamma row at its own entry" \
    "unresolved step name: stepBeta in $root/controller.go
step table of $root/page.md $HEADING, entry 3: Delta, want Gamma" \
    "$(check_fixture "$root")"
}

test_missing_page_or_section_is_the_only_report() {
  echo "Test: a page path that is no file, or a page without the heading, is the only report"

  local root
  root="$(build_fixture)"
  assert_eq "no page" "no page: $root/missing.md" \
    "$(check_pipeline "$root/controller.go" "$root/missing.md" "$HEADING" yes)"

  rewrite "$root/page.md" 's/^## Pipeline$/## Steps/'
  assert_eq "no section" "no section: $HEADING in $root/page.md" "$(check_fixture "$root")"
}

test_missing_step_table_still_checks_the_block() {
  echo "Test: a section without a step table is reported, and its block is still checked"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's/^| Step |/| Name |/'
  assert_eq "no step table" "no step table: $HEADING in $root/page.md" "$(check_fixture "$root")"

  rewrite "$root/page.md" 's/ ──► Beta//'
  assert_eq "no step table, and the block misses Beta" \
    "no step table: $HEADING in $root/page.md
pipeline block of $root/page.md $HEADING, entry 2: Gamma, want Beta" \
    "$(check_fixture "$root")"
}

test_missing_block_is_reported_only_when_expected() {
  echo "Test: a section without a fenced block is reported only when a block is expected"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" '/^```/,/^```/d'
  assert_eq "no pipeline block" "no pipeline block: $HEADING in $root/page.md" \
    "$(check_fixture "$root" yes)"
  assert_eq "no block expected" "" "$(check_fixture "$root" no)"
}

test_table_drift_is_reported_at_its_entry() {
  echo "Test: a row the table leaves out or adds is reported at its entry"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" '/^| Delta |/d'
  assert_eq "no Delta row" "step table of $root/page.md $HEADING, entry 4: Zeta, want Delta" \
    "$(check_fixture "$root")"

  root="$(build_fixture)"
  rewrite "$root/page.md" '/^| Epsilon |/d'
  assert_eq "no last row" "step table of $root/page.md $HEADING, entry 6: nothing, want Epsilon" \
    "$(check_fixture "$root")"

  root="$(build_fixture)"
  rewrite "$root/page.md" '/^| Epsilon |/a\
| Eta | g | `EtaReady` |
'
  assert_eq "a row the controller lacks" \
    "step table of $root/page.md $HEADING, entry 7: Eta, want nothing" \
    "$(check_fixture "$root")"
}

test_block_drift_is_reported_at_its_entry() {
  echo "Test: a name the block leaves out or swaps is reported at its entry"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's/└─ Delta /└─ /'
  assert_eq "no Delta in the block" \
    "pipeline block of $root/page.md $HEADING, entry 4: Zeta, want Delta" \
    "$(check_fixture "$root")"

  root="$(build_fixture)"
  rewrite "$root/page.md" 's/^Alpha ──► Beta /Beta ──► Alpha /'
  assert_eq "Beta drawn before Alpha" \
    "pipeline block of $root/page.md $HEADING, entry 1: Beta, want Alpha" \
    "$(check_fixture "$root")"
}

test_counts_follow_the_controller() {
  echo "Test: a count sentence that follows the controller reports nothing, over a line break and in any case"

  local root
  root="$(build_fixture)"
  assert_eq "sequential steps and parallel members" "" \
    "$(check_count "$root/controller.go" "$root/page.md" 'runs {sequential} sequential sub-reconcilers and a parallel group of {parallel}.')"
  assert_eq "every named step, capitalized on the page" "" \
    "$(check_count "$root/controller.go" "$root/page.md" '{steps} steps run in all')"
  assert_eq "the sub-conditions, without the comment line" "" \
    "$(check_count "$root/controller.go" "$root/page.md" 'aggregates {conditions} sub-conditions')"
  assert_eq "a number past twenty stays in digits" "21" "$(number_word 21)"
}

test_count_drift_is_reported_with_the_sentence_it_wants() {
  echo "Test: a count the controller outgrew is reported with the sentence the page lacks"

  local root
  root="$(build_fixture)"
  rewrite "$root/controller.go" '/{Name: "Epsilon"/i\
		{Name: "Eta", Fn: r.reconcileEta},
'
  assert_eq "a step added" "count of $root/page.md: no \"seven steps run in all\"" \
    "$(check_count "$root/controller.go" "$root/page.md" '{steps} steps run in all')"
  assert_eq "a sequential step added" \
    "count of $root/page.md: no \"runs five sequential sub-reconcilers and a parallel group of two.\"" \
    "$(check_count "$root/controller.go" "$root/page.md" 'runs {sequential} sequential sub-reconcilers and a parallel group of {parallel}.')"

  root="$(build_fixture)"
  rewrite "$root/controller.go" '/"EpsilonReady",/a\
	"ZetaReady",
'
  assert_eq "a sub-condition added" "count of $root/page.md: no \"aggregates six sub-conditions\"" \
    "$(check_count "$root/controller.go" "$root/page.md" 'aggregates {conditions} sub-conditions')"

  assert_eq "no controller" "no controller: $root/missing.go" \
    "$(check_count "$root/missing.go" "$root/page.md" '{steps} steps run in all')"
  assert_eq "no page" "no page: $root/missing.md" \
    "$(check_count "$root/controller.go" "$root/missing.md" '{steps} steps run in all')"
}

test_figure_drift_is_reported_where_it_is() {
  echo "Test: a step the drawio, the svg or the alt text leaves out is reported for that place"

  local root
  root="$(build_fixture)"
  rewrite "$root/figure.drawio" '/value="Zeta"/d'
  rewrite "$root/figure.svg" '/>Zeta</d'
  rewrite "$root/page.md" 's/, then Zeta and Epsilon/, then Epsilon/'
  assert_eq "Zeta left out of all three" "not in drawio: Zeta
not in svg: Zeta
not in the alt text: Zeta" "$(check_fixture_figure "$root")"

  root="$(build_fixture)"
  rewrite "$root/figure.drawio" 's/value="Alpha"/value="Alpha step"/'
  rewrite "$root/figure.svg" 's/>Alpha</>Alpha step</'
  assert_eq "Alpha only inside longer labels" "not in drawio: Alpha
not in svg: Alpha" "$(check_fixture_figure "$root")"
}

test_missing_figure_file_or_image_is_reported() {
  echo "Test: a missing figure file or image is reported, and the other places are still searched"

  local root
  root="$(build_fixture)"
  rm "$root/figure.svg"
  rewrite "$root/figure.drawio" '/value="Gamma"/d'
  assert_eq "no svg, the drawio is still searched" "missing figure: $root/figure.svg
not in drawio: Gamma" "$(check_fixture_figure "$root")"

  root="$(build_fixture)"
  rewrite "$root/page.md" '/^!\[/d'
  assert_eq "no image" "no image of figure.svg on $root/page.md" "$(check_fixture_figure "$root")"

  assert_eq "no controller" "no controller: $root/missing.go" \
    "$(check_pipeline_figure "$root/missing.go" "$root/figure" "$root/page.md")"
  assert_eq "no page" "no page: $root/missing.md" \
    "$(check_pipeline_figure "$root/controller.go" "$root/figure" "$root/missing.md")"
}

test_repository_pipelines_follow_their_controllers() {
  echo "Test: the pipeline lists of the reconciler pages follow their controllers"

  local controller page heading block
  while IFS='|' read -r controller page heading block; do
    assert_eq "$page $heading follows $controller" "" \
      "$(check_pipeline "$PROJECT_ROOT/operators/$controller" "$REFERENCE_DOCS/$page" "$heading" "$block")"
  done <<'EOF'
keystone/internal/controller/keystone_controller.go|keystone/keystone-reconciler.md|## Reconciliation Flow|no
glance/internal/controller/glance_controller.go|glance/glance-reconciler.md|## Pipeline|yes
barbican/internal/controller/barbican_controller.go|barbican/barbican-reconciler.md|## Pipeline|yes
placement/internal/controller/placement_controller.go|placement/placement-reconciler.md|## Pipeline|yes
horizon/internal/controller/horizon_controller.go|horizon/horizon-reconciler.md|## Pipeline|yes
cinder/internal/controller/cinder_controller.go|cinder/cinder-reconciler.md|## Pipeline|yes
nova/internal/controller/nova_controller.go|nova/nova-reconciler.md|## Pipeline|yes
nova/internal/controller/novacompute_controller.go|nova/nova-reconciler.md|### NovaCompute pipeline|yes
neutron/internal/controller/neutron_controller.go|neutron/neutron-reconciler.md|### Neutron|yes
neutron/internal/controller/neutronmetadataagent_controller.go|neutron/neutron-reconciler.md|### NeutronMetadataAgent|yes
ovn/internal/controller/ovncentral_controller.go|ovn/ovn-reconciler.md|### OVNCentral|yes
ovn/internal/controller/ovnchassis_controller.go|ovn/ovn-reconciler.md|### OVNChassis|yes
EOF
}

test_repository_counts_follow_their_controllers() {
  echo "Test: the step and sub-condition counts of the pages follow their controllers"

  local controller page phrase
  while IFS='|' read -r controller page phrase; do
    assert_eq "$page counts \"$phrase\" of $controller" "" \
      "$(check_count "$PROJECT_ROOT/operators/$controller" "$REFERENCE_DOCS/$page" "$phrase")"
  done <<'EOF'
keystone/internal/controller/keystone_controller.go|keystone/index.md|{conditions} typed sub-conditions
keystone/internal/controller/keystone_controller.go|../quick-start-extended.md|through {conditions} sub-conditions
glance/internal/controller/glance_controller.go|glance/glance-reconciler.md|with {steps} sub-reconcilers.
glance/internal/controller/glance_controller.go|glance/glance-reconciler.md|all {conditions} sub-conditions are
barbican/internal/controller/barbican_controller.go|barbican/barbican-reconciler.md|with {steps} sub-reconcilers.
barbican/internal/controller/barbican_controller.go|barbican/barbican-reconciler.md|The {parallel} members of the parallel group
barbican/internal/controller/barbican_controller.go|barbican/barbican-reconciler.md|all {conditions} sub-conditions are
placement/internal/controller/placement_controller.go|placement/placement-reconciler.md|with {steps} sub-reconcilers.
placement/internal/controller/placement_controller.go|placement/placement-reconciler.md|The {parallel} members of the parallel group
placement/internal/controller/placement_controller.go|placement/placement-reconciler.md|all {conditions} sub-conditions are
horizon/internal/controller/horizon_controller.go|horizon/horizon-reconciler.md|with {steps} sub-reconcilers and one unnamed prune step.
horizon/internal/controller/horizon_controller.go|horizon/horizon-reconciler.md|all {conditions} sub-conditions are
cinder/internal/controller/cinder_controller.go|cinder/cinder-reconciler.md|with {sequential} sequential sub-reconcilers and a parallel group of {parallel}.
cinder/internal/controller/cinder_controller.go|cinder/cinder-reconciler.md|The {parallel} members of the parallel group
cinder/internal/controller/cinder_controller.go|cinder/cinder-reconciler.md|all {conditions} sub-conditions are
nova/internal/controller/nova_controller.go|nova/nova-reconciler.md|with {sequential} sequential sub-reconcilers and a parallel group of {parallel}.
nova/internal/controller/nova_controller.go|nova/nova-reconciler.md|The {parallel} members of the parallel group
nova/internal/controller/nova_controller.go|nova/nova-reconciler.md|all {conditions} sub-conditions are
nova/internal/controller/novacompute_controller.go|nova/nova-reconciler.md|runs {steps} sequential steps
nova/internal/controller/novacompute_controller.go|nova/nova-reconciler.md|only when all {conditions} are.
neutron/internal/controller/neutron_controller.go|neutron/neutron-reconciler.md|`NeutronReconciler` with {steps} sub-reconcilers
neutron/internal/controller/neutron_controller.go|neutron/neutron-reconciler.md|`Neutron` aggregates {conditions},
neutron/internal/controller/neutronmetadataagent_controller.go|neutron/neutron-reconciler.md|and `NeutronMetadataAgentReconciler` with {steps}.
neutron/internal/controller/neutronmetadataagent_controller.go|neutron/neutron-reconciler.md|a `NeutronMetadataAgent` {conditions}.
ovn/internal/controller/ovncentral_controller.go|ovn/ovn-reconciler.md|`OVNCentralReconciler` with {steps} sub-reconcilers
ovn/internal/controller/ovncentral_controller.go|ovn/ovn-reconciler.md|`OVNCentral` aggregates {conditions},
ovn/internal/controller/ovnchassis_controller.go|ovn/ovn-reconciler.md|and `OVNChassisReconciler` with {steps}.
ovn/internal/controller/ovnchassis_controller.go|ovn/ovn-reconciler.md|an `OVNChassis` {conditions}.
EOF
}

test_repository_figure_follows_its_controller() {
  echo "Test: the reconciler pipeline figure follows the Keystone controller"

  assert_eq "service-reconciler-pipeline names every Keystone step" "" \
    "$(check_pipeline_figure "$PROJECT_ROOT/operators/keystone/internal/controller/keystone_controller.go" \
      "$PROJECT_ROOT/docs/diagrams/service-reconciler-pipeline" "$REFERENCE_DOCS/keystone/keystone-reconciler.md")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_consistent_fixture_reports_nothing
test_steps_are_read_from_every_entry_form
test_missing_controller_is_the_only_report
test_controller_without_steps_is_the_only_report
test_unresolved_constant_is_reported_and_the_rest_compared
test_missing_page_or_section_is_the_only_report
test_missing_step_table_still_checks_the_block
test_missing_block_is_reported_only_when_expected
test_table_drift_is_reported_at_its_entry
test_block_drift_is_reported_at_its_entry
test_counts_follow_the_controller
test_count_drift_is_reported_with_the_sentence_it_wants
test_figure_drift_is_reported_where_it_is
test_missing_figure_file_or_image_is_reported
test_repository_pipelines_follow_their_controllers
test_repository_counts_follow_their_controllers
test_repository_figure_follows_its_controller

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
