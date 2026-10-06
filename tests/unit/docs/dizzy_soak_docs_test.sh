#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the documentation of the dizzy soak (#1274):
#   1. docs/reference/testing/dizzy-chaos-testing.md has `## In-cluster soak`
#      once, after `## Running a soak` and before `## Variables`, with its
#      nine H3 sections in order.
#   2. The section names the four make targets, the unbounded start
#      `DIZZY_SOAK_DURATION=0 make dizzy-soak-start`, `report <run>` and
#      `dizzy mix cleanup --run`.
#   3. The settings table lists every setting the two scripts read from the
#      environment and every DIZZY_SOAK_* variable they name, and nothing
#      else.
#   4. The checks table names the checks of hack/dizzy-soak-runner.sh, no
#      more and no fewer.
#   5. `### Lab soak run` holds the sentence that no run is recorded or a
#      line that starts with `The run of 20YY-MM-DD`, not both and not
#      neither.
#   6. The cleanup block parses as bash.
#   7. docs/reference/infrastructure/infrastructure-manifests.md has
#      `#### Lab dizzy soak` once, between `### Lab dizzy stack` and
#      `#### Lab dizzy run`; e2e-deployment.md has dizzy-soak/ in the lab tree
#      and the soak step of the teardown; the Quick Start (metal-stack) names
#      the soak in its dizzy and teardown paragraphs; dependency-management.md
#      has the runner image's bullet.
#
# Subsections run from their heading to the next heading of the same or a
# higher level, outside fenced code.
#
# Usage: bash tests/unit/docs/dizzy_soak_docs_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TESTING_DOC="$PROJECT_ROOT/docs/reference/testing/dizzy-chaos-testing.md"
MANIFESTS_DOC="$PROJECT_ROOT/docs/reference/infrastructure/infrastructure-manifests.md"
E2E_DOC="$PROJECT_ROOT/docs/reference/infrastructure/e2e-deployment.md"
QUICK_START_DOC="$PROJECT_ROOT/docs/quick-start-metal-stack.md"
DEPENDENCY_DOC="$PROJECT_ROOT/docs/contributing/dependency-management.md"
SOAK_SH="$PROJECT_ROOT/hack/dizzy-soak.sh"
RUNNER_SH="$PROJECT_ROOT/hack/dizzy-soak-runner.sh"

PLACEHOLDER="No lab run of the soak is recorded yet."

# headings <file> — "<line>:<heading>" for every heading outside fenced code.
headings() {
  awk '
    /^[[:space:]]*(```|~~~)/ { fenced = !fenced; next }
    !fenced && /^#+ / { print NR ":" $0 }
  ' "$1"
}

# section <file> <heading> — The lines below <heading>, fenced code
# included, up to the next heading of the same or a higher level.
section() {
  awk -v heading="$2" '
    /^[[:space:]]*(```|~~~)/ { fenced = !fenced }
    !fenced && /^#+ / {
      level = index($0, " ") - 1
      if (inside && level <= depth) exit
      if ($0 == heading) { inside = 1; depth = level; next }
    }
    inside { print }
  ' "$1"
}

# line_of <file> <heading> — The line number of <heading>, empty without it.
line_of() {
  headings "$1" | awk -v heading="$2" '{ n = $0; sub(/:.*/, "", n); sub(/^[0-9]+:/, "") } $0 == heading { print n; exit }'
}

# lab_run_state <text> — placeholder, record, both or neither.
lab_run_state() {
  local placeholder record
  placeholder="$(grep -cxF "$PLACEHOLDER" <<<"$1" || true)"
  record="$(grep -cE '^The run of 20[0-9]{2}-[0-9]{2}-[0-9]{2}' <<<"$1" || true)"
  if [[ "$placeholder" != "0" && "$record" != "0" ]]; then
    echo both
  elif [[ "$placeholder" != "0" ]]; then
    echo placeholder
  elif [[ "$record" != "0" ]]; then
    echo record
  else
    echo neither
  fi
}

SOAK="$(section "$TESTING_DOC" '## In-cluster soak')"

# --- Test 1: the place and the subsections ---
test_position() {
  echo "Test: '## In-cluster soak' sits once between '## Running a soak' and '## Variables'"

  local all running soak variables
  all="$(headings "$TESTING_DOC")"
  assert_eq "'## In-cluster soak' occurs once" "1" "$(grep -cxE '[0-9]+:## In-cluster soak' <<<"$all" || true)"
  running="$(line_of "$TESTING_DOC" '## Running a soak')"
  soak="$(line_of "$TESTING_DOC" '## In-cluster soak')"
  variables="$(line_of "$TESTING_DOC" '## Variables')"
  assert_eq "it follows '## Running a soak' and precedes '## Variables'" "true" \
    "$([[ -n "$running" && -n "$soak" && -n "$variables" && "$running" -lt "$soak" && "$soak" -lt "$variables" ]] &&
      echo true || echo false)"
  assert_eq "its H3 sections, in order" "$(printf '%s\n' \
    '### Soak prerequisites' '### Soak commands' '### Soak settings' '### Report directory' \
    '### Verdict checks' '### Limits' '### Freeing the claim' \
    '### Reclaiming resources after a killed runner' '### Lab soak run')" \
    "$(awk '/^[[:space:]]*(```|~~~)/ { f = !f; next } !f && /^### /' <<<"$SOAK")"
}

# --- Test 2: the commands ---
# shellcheck disable=SC2016 # the page's literal text, not expanded here
test_commands() {
  echo "Test: the section names the commands"

  local text
  for text in 'make dizzy-soak-start' 'make dizzy-soak-status' 'make dizzy-soak-stop' 'make dizzy-soak-report' \
    'DIZZY_SOAK_DURATION=0 make dizzy-soak-start' 'hack/dizzy-soak.sh report 20261012T081500Z' \
    'dizzy mix cleanup --run run-<id>.json' '"mix", "cleanup", "--os-cloud", "dizzy-soak", "--run"' \
    '[Lab dizzy soak](../infrastructure/infrastructure-manifests.md#lab-dizzy-soak)'; do
    assert_contains "the section holds '$text'" "$SOAK" "$text"
  done
}

# --- Test 3: the settings table ---
# shellcheck disable=SC2016 # awk and sed programs, not expanded here
test_settings_table() {
  echo "Test: the settings table lists every setting the two scripts read"

  local read_vars table
  # The settings: every variable a script reads from the environment with a
  # default, every DIZZY_SOAK_* variable it names outside comments, and
  # DIZZY_VERSION, which the driver takes from hack/dizzy.sh.
  read_vars="$( {
    awk '/^[A-Z][A-Z0-9_]*="\$\{/ {
      name = substr($0, 1, index($0, "=") - 1)
      if (index($0, name "=\"${" name ":-") == 1 || index($0, name "=\"${" name "-") == 1) print name
    }' "$SOAK_SH" "$RUNNER_SH"
    grep -vhE '^[[:space:]]*#' "$SOAK_SH" "$RUNNER_SH" | grep -oE 'DIZZY_SOAK_[A-Z0-9_]+'
    echo DIZZY_VERSION
  } | sort -u)"
  table="$(section "$TESTING_DOC" '### Soak settings' | sed -nE 's/^\| `([A-Z][A-Z0-9_]*)` \|.*/\1/p' | sort)"
  assert_gte "the scripts read at least the 13 settings" "$(grep -c . <<<"$read_vars")" 13
  assert_eq "the table lists them, no more and no fewer" "$read_vars" "$table"
}

# --- Test 4: the checks table ---
# shellcheck disable=SC2016 # a sed program, not expanded here
test_checks_table() {
  echo "Test: the checks table names the runner's checks"

  local runner_checks table
  runner_checks="$(grep -oE '^[[:space:]]*check [a-z0-9-]+ ' "$RUNNER_SH" | awk '{ print $2 }' | sort -u)"
  table="$(section "$TESTING_DOC" '### Verdict checks' | sed -nE 's/^\| `([a-z0-9-]+)` \|.*/\1/p' | sort)"
  assert_eq "the runner has six checks" "6" "$(grep -c . <<<"$runner_checks")"
  assert_eq "the table names them, no more and no fewer" "$runner_checks" "$table"
}

# --- Test 5: the lab run ---
test_lab_run() {
  echo "Test: '### Lab soak run' holds the placeholder or a record, not both and not neither"

  local run state
  run="$(section "$TESTING_DOC" '### Lab soak run')"
  state="$(lab_run_state "$run")"
  if [[ "$state" == "placeholder" || "$state" == "record" ]]; then
    echo "  PASS: the subsection holds the $state"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the subsection holds $state of the placeholder and a 'The run of YYYY-MM-DD' line"
    FAIL=$((FAIL + 1))
  fi
  assert_eq "a text with both is refused" "both" \
    "$(lab_run_state "$(printf '%s\n' "$PLACEHOLDER" 'The run of 2026-10-12 on newforge passed.')")"
  assert_eq "a text with neither is refused" "neither" "$(lab_run_state 'The run is pending.')"
}

# --- Test 6: the cleanup block ---
test_cleanup_block_parses() {
  echo "Test: the cleanup block of the section parses as bash"

  local block
  block="$(section "$TESTING_DOC" '### Reclaiming resources after a killed runner' |
    awk '/^```bash$/ { f = 1; next } f && /^```$/ { exit } f { print }')"
  assert_contains "the block runs dizzy mix cleanup in a pod" "$block" "name: dizzy-soak-cleanup"
  if bash -n <<<"$block" 2>/dev/null; then
    echo "  PASS: the block parses with bash -n"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the block does not parse with bash -n"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 7: the other pages ---
# shellcheck disable=SC2016 # the pages' literal text, not expanded here
test_other_pages() {
  echo "Test: the reference, deployment, quick-start and dependency pages name the soak"

  local stack soak run
  stack="$(line_of "$MANIFESTS_DOC" '### Lab dizzy stack')"
  soak="$(line_of "$MANIFESTS_DOC" '#### Lab dizzy soak')"
  run="$(line_of "$MANIFESTS_DOC" '#### Lab dizzy run')"
  assert_eq "'#### Lab dizzy soak' occurs once" "1" \
    "$(headings "$MANIFESTS_DOC" | grep -cxE '[0-9]+:#### Lab dizzy soak' || true)"
  assert_eq "between '### Lab dizzy stack' and '#### Lab dizzy run'" "true" \
    "$([[ -n "$stack" && -n "$soak" && -n "$run" && "$stack" -lt "$soak" && "$soak" -lt "$run" ]] && echo true || echo false)"
  local manifests text
  manifests="$(section "$MANIFESTS_DOC" '#### Lab dizzy soak')"
  for text in '**Posture.**' '**Teardown.**' '`tests/unit/deploy/metal_stack_dizzy_soak_test.sh`' \
    'ClusterRole and ClusterRoleBinding `dizzy-soak-platform-reader`' 'PersistentVolumeClaim `dizzy-soak-reports`'; do
    assert_contains "Lab dizzy soak holds '$text'" "$manifests" "$text"
  done
  assert_contains "the opening of '## Metal-stack lab' links it" "$(section "$MANIFESTS_DOC" '## Metal-stack lab' | head -n 20)" \
    '[Lab dizzy soak](#lab-dizzy-soak)'

  assert_contains "the lab tree of e2e-deployment.md has dizzy-soak/" "$(cat "$E2E_DOC")" \
    '├── dizzy-soak/                     The dizzy soak (#1274), applied by make dizzy-soak-start'
  assert_contains "its teardown names the soak's step" "$(cat "$E2E_DOC")" \
    '`ERROR: cannot render <overlay>/dizzy-soak (kustomize'"'"'s error is above).`'
  assert_contains "the quick start's dizzy paragraph names the soak" "$(cat "$QUICK_START_DOC")" \
    '[In-cluster soak](./reference/testing/dizzy-chaos-testing.md#in-cluster-soak)'
  assert_contains "its teardown paragraph names the report fetch" "$(cat "$QUICK_START_DOC")" \
    '`make dizzy-soak-report` first'
  assert_contains "dependency-management.md has the runner image's bullet" "$(cat "$DEPENDENCY_DOC")" \
    '- **Lab soak runner image**'
}

# --- Run ---
if [[ -z "$SOAK" ]]; then
  echo "FAIL: $TESTING_DOC has no '## In-cluster soak' section"
  exit 1
fi

test_position
test_commands
test_settings_table
test_checks_table
test_lab_run
test_cleanup_block_parses
test_other_pages

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
