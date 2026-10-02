#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the steps of docs/quick-start-controlplane.md and the references to
# them:
#   - the page's `## Step` headings are the seven steps, in order
#   - every phrase of STEP_PHRASES that names one of those steps by what it
#     holds (the OVN central, the CR, the tenant onboarding, the chain, the
#     Verify checks) names the step that holds it, on the page, in the guides
#     under docs/guides/, in the hints of hack/deploy-infra.sh and in the
#     comments under deploy/lab/
#
# Outside the page a phrase names one of its steps only when its words say
# so; any other match names a step of its own file (a guide's `## Step`, the
# script's `Step 1/8`) or of another page (the metal-stack quick start the lab
# comments follow), and is skipped and listed as a SKIP. A guide that links the
# page names it as the tutorial or the quick start in the phrase, the 300
# characters before it or the 80 after it; the script and the lab manifests
# name it by path, in the phrase.
#
# tests/unit/docs/no_conflict_markers_test.sh owns the docs-wide check for
# merge conflict markers. QUICK_START_DOC overrides the page.
#
# Usage: bash tests/unit/docs/quick_start_controlplane_steps_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

QUICK_START_DOC="${QUICK_START_DOC:-$PROJECT_ROOT/docs/quick-start-controlplane.md}"

if [[ ! -f "$QUICK_START_DOC" ]]; then
  echo "FAIL: $QUICK_START_DOC does not exist"
  exit 1
fi

# STEP_PHRASES pairs the step a phrase must name with an extended regular
# expression for the phrase; the phrase holds one "Step <n>", and a phrase
# without one is a wording that names no step and is always stale. The
# entries are the wordings the references to the page use.
STEP_PHRASES=(
  '3|OVNCentral` of Step [0-9]+'
  '3|central (of|from) Step [0-9]+'
  '3|central from the top of this step'
  '3|quick-start-controlplane\.md, Step [0-9]+, the `# controlplane-ovn\.yaml`'
  '4|block of Step [0-9]+'
  '4|Step [0-9]+ CR'
  '4|CR (of|in) Step [0-9]+'
  '4|publicEndpoint` from Step [0-9]+'
  '4|apply that in Step [0-9]+'
  '4|quick-start-controlplane\.md, Step [0-9]+, the first `# controlplane\.yaml`'
  '5|onboard[a-z]* (of|in) Step [0-9]+'
  '5|Step [0-9]+ onboard'
  "5|onboarding as the tutorial's Step [0-9]+"
  '5|onboard[^().]*\(docs/quick-start-controlplane\.md, Step [0-9]+'
  '6|Step [0-9]+ chain'
  '7|checks? in (its )?Step [0-9]+'
  '7|leaves behind in Step [0-9]+'
  "7|Step [0-9]+'s \\*\\*Boot"
  '7|exports in its Step [0-9]+'
)

# --- Test 1: the seven step headings, in order ---
test_step_headings() {
  echo "Test: the page's '## Step' headings are the seven steps, in order"
  local want got
  want="$(printf '%s\n' \
    'Step 1 — Clone' \
    'Step 2 — Cluster + ControlPlane stack' \
    'Step 3 — Deploy the OVN control plane' \
    'Step 4 — Create the ControlPlane CR' \
    'Step 5 — Onboard the OpenBao database-engine tenant' \
    'Step 6 — Watch the chain reconcile' \
    'Step 7 — Verify')"
  got="$(grep '^## Step ' "$QUICK_START_DOC" | sed 's/^## //' || true)"
  assert_eq "step headings" "$want" "$got"
}

# --- Test 2: every step reference names the step that holds its subject ---
test_step_references() {
  echo "Test: every step reference names the step that holds its subject"
  local files=("$QUICK_START_DOC") file out line checked=0 stale=0
  while IFS= read -r file; do
    files+=("$file")
  done < <(
    find "$PROJECT_ROOT/docs/guides" -type f -name '*.md'
    echo "$PROJECT_ROOT/hack/deploy-infra.sh"
    find "$PROJECT_ROOT/deploy/lab" -type f
  )
  # The first input holds the entries. Every other file is folded into one
  # line, the indentation and a leading comment marker, quote mark or `log "`
  # of each line dropped, so a phrase that wraps still matches.
  out="$(awk -v root="$PROJECT_ROOT/" -v page="$QUICK_START_DOC" '
    # names_page tells whether the match m at offset s of the folded file
    # names a step of the page; see the header for the rule.
    function names_page(s, m) {
      if (file == page) return 1
      if (file !~ /\.md$/) return m ~ /quick-start-controlplane\.md/
      if (text !~ /quick-start-controlplane\.md/) return 0
      return (substr(text, s > 300 ? s - 300 : 1, s > 300 ? 300 : s - 1) m substr(text, s + length(m), 80)) ~ /[Tt]utorial|[Qq]uick [Ss]tart|quick-start-controlplane/
    }
    function scan(   i, t, off, s, m, got) {
      for (i = 1; i <= n; i++) {
        t = text
        off = 0
        while (match(t, re[i])) {
          m = substr(t, RSTART, RLENGTH)
          t = substr(t, RSTART + RLENGTH)
          s = off + RSTART
          off = s + RLENGTH - 1
          if (!names_page(s, m)) { print "SKIPPED\t" substr(file, length(root) + 1) "\t" m; continue }
          got = match(m, /Step [0-9]+/) ? substr(m, RSTART + 5, RLENGTH - 5) : "no number"
          checked++
          if (got != want[i]) print "STALE\t" substr(file, length(root) + 1) "\t" want[i] "\t" got "\t" m
        }
      }
    }
    NR == FNR { n++; want[n] = substr($0, 1, index($0, "|") - 1); re[n] = substr($0, index($0, "|") + 1); next }
    FNR == 1 { if (file != "") scan(); file = FILENAME; text = "" }
    { line = $0; sub(/^[[:space:]]*(#+|>|log ")?[[:space:]]*/, "", line); gsub(/[[:space:]]+/, " ", line); text = text " " line }
    END { if (file != "") scan(); print "CHECKED\t" checked + 0 }
  ' <(printf '%s\n' "${STEP_PHRASES[@]}") "${files[@]}")"
  while IFS=$'\t' read -r -a line; do
    case "${line[0]}" in
      CHECKED) checked="${line[1]}" ;;
      SKIPPED)
        echo "  SKIP: ${line[1]}: '${line[2]}' does not name the page"
        SKIP=$((SKIP + 1))
        ;;
      STALE)
        echo "  FAIL: ${line[1]} names Step ${line[2]} as ${line[3]}: ${line[4]}"
        FAIL=$((FAIL + 1))
        stale=$((stale + 1))
        ;;
    esac
  done <<<"$out"
  if [[ "$checked" -eq 0 ]]; then
    echo "  FAIL: no step reference found"
    FAIL=$((FAIL + 1))
  elif [[ "$stale" -eq 0 ]]; then
    echo "  PASS: $checked step references name their step"
    PASS=$((PASS + 1))
  fi
}

test_step_headings
test_step_references

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
