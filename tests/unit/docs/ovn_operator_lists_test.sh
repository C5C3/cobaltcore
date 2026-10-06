#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the OVN docs restate the operator's step and condition lists.
#
# docs/guides/ovn/enable-ovn-operator-metrics.md names every value of the
# sub_reconciler label in one sentence, "`sub_reconciler` carries <word>
# values: ...", up to the first full stop. subReconcilerConditionTypes in
# operators/ovn/internal/controller/instrumentation.go has one key per value.
# The AllReady row of the Status Conditions table in
# docs/reference/ovn/ovn-central-crd.md and ovn-chassis-crd.md says how many
# sub-conditions Ready aggregates, "All <word> sub-conditions are True";
# centralSubConditionTypes and chassisSubConditionTypes list them.
#
# check_ovn_lists compares the backticked names of the sentence, without the
# two kind names, with the keys of the map in both directions, and each number
# word with the length of its list. It prints one problem per line and nothing
# when the pages agree with the code. The fixture tests prove each check on a
# scratch tree before the last test runs it on the repository.
#
# Usage: bash tests/unit/docs/ovn_operator_lists_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

METRICS_GUIDE="guides/ovn/enable-ovn-operator-metrics.md"
CENTRAL_PAGE="reference/ovn/ovn-central-crd.md"
CHASSIS_PAGE="reference/ovn/ovn-chassis-crd.md"

# word_number <word>: print the number a word from one to twenty names, and
# nothing for any other word.
word_number() {
  local word="$1" candidate n=0
  for candidate in one two three four five six seven eight nine ten eleven \
    twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty; do
    n=$((n + 1))
    if [[ "$candidate" == "$word" ]]; then
      echo "$n"
      return
    fi
  done
}

# slice_entries <file> <name>: print the entries of `var <name> = []string{`,
# one per line, without comment lines.
slice_entries() {
  awk -v name="$2" '
    $0 ~ "^var " name " = \\[\\]string\\{" { in_slice = 1; next }
    in_slice && /^\}/ { exit }
    in_slice {
      sub(/^[ \t]+/, "")
      sub(/,?[ \t]*$/, "")
      if ($0 != "" && substr($0, 1, 2) != "//") print
    }
  ' "$1"
}

# check_slice <go-file> <slice> <page>: report when the AllReady row of the
# page names another number than the slice has entries.
# shellcheck disable=SC2016 # the page's literal text, not expanded here
check_slice() {
  local go_file="$1" slice="$2" page="$3" entries word n
  if [[ ! -f "$go_file" ]]; then
    echo "no file: $go_file"
    return
  fi
  entries="$(slice_entries "$go_file" "$slice")"
  if [[ -z "$entries" ]]; then
    echo "no $slice in $go_file"
    return
  fi
  if [[ ! -f "$page" ]]; then
    echo "no page: $page"
    return
  fi
  word="$(sed -n 's/^| `Ready` | True | `AllReady` | All \([a-z]*\) sub-conditions are True |$/\1/p' "$page")"
  if [[ -z "$word" ]]; then
    echo "no AllReady row: $page"
    return
  fi
  n="$(printf '%s\n' "$entries" | grep -c .)"
  if [[ "$(word_number "$word")" != "$n" ]]; then
    echo "AllReady count: ${page##*/} says $word, $slice has $n"
  fi
}

# check_ovn_lists <controller-dir> <docs-root>: print one problem per line,
# sorted, and nothing when the pages agree with the code. A missing source
# file or a list without entries is reported instead of its comparison.
# shellcheck disable=SC2016 # the page's literal text, not expanded here
check_ovn_lists() {
  local controller="$1" docs="$2"
  local instrumentation="$controller/instrumentation.go"
  local guide="$docs/$METRICS_GUIDE"

  {
    local keys="" names="" word n sentence
    if [[ -f "$instrumentation" ]]; then
      keys="$(awk '
        /^var subReconcilerConditionTypes = map\[string\]string\{/ { in_map = 1; next }
        in_map && /^\}/ { exit }
        in_map && match($0, /^[ \t]*"[A-Za-z]+":/) {
          key = substr($0, RSTART, RLENGTH - 1)
          sub(/^[ \t]*"/, "", key)
          sub(/"$/, "", key)
          print key
        }
      ' "$instrumentation" | LC_ALL=C sort)"
      [[ -n "$keys" ]] || echo "no subReconcilerConditionTypes in $instrumentation"
    else
      echo "no file: $instrumentation"
    fi

    if [[ -f "$guide" ]]; then
      # Join the paragraph, then keep the sentence from "carries" to its full
      # stop.
      sentence="$(awk '
        /`sub_reconciler` carries / { in_par = 1 }
        in_par && /^$/ { exit }
        in_par { text = text " " $0 }
        END { print text }
      ' "$guide" | sed -n 's/.*`sub_reconciler` carries \([a-z]*\) values:\([^.]*\)\..*/\1 \2/p')"
      if [[ -z "$sentence" ]]; then
        echo "no sub_reconciler sentence: $guide"
      else
        word="${sentence%% *}"
        names="$(printf '%s\n' "$sentence" | grep -o '`[A-Za-z]*`' | tr -d '`' \
          | grep -v -x -e OVNCentral -e OVNChassis | LC_ALL=C sort)"
        if [[ -n "$keys" ]]; then
          comm -23 <(printf '%s\n' "$keys") <(printf '%s\n' "$names") \
            | sed 's/^/not in the metrics guide: /'
          comm -13 <(printf '%s\n' "$keys") <(printf '%s\n' "$names") \
            | sed 's/^/not in subReconcilerConditionTypes: /'
          n="$(printf '%s\n' "$keys" | grep -c .)"
          if [[ "$(word_number "$word")" != "$n" ]]; then
            echo "sub_reconciler count: the metrics guide says $word, the map has $n"
          fi
        fi
      fi
    else
      echo "no page: $guide"
    fi

    check_slice "$controller/ovncentral_controller.go" centralSubConditionTypes "$docs/$CENTRAL_PAGE"
    check_slice "$controller/ovnchassis_controller.go" chassisSubConditionTypes "$docs/$CHASSIS_PAGE"
  } | LC_ALL=C sort
}

# ---------------------------------------------------------------------------
# Fixture
# ---------------------------------------------------------------------------

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

# build_fixture: write a consistent tree and print its root. controller/
# holds a map of five keys in two blocks, one with a comment line, and two
# slices: three entries, one of them quoted, and two entries below a comment
# line.
# docs/ holds the guide sentence over three lines and the two AllReady rows.
# shellcheck disable=SC2016 # the page's literal text, not expanded here
build_fixture() {
  local root
  root="$(mktemp -d "$TMP_ROOT/fixture.XXXXXX")"
  mkdir -p "$root/controller" "$root/docs/guides/ovn" "$root/docs/reference/ovn"
  cat >"$root/controller/instrumentation.go" <<'EOF'
package controller

var subReconcilerConditionTypes = map[string]string{
	"TLS":   conditionTypeTLSReady,
	"Relay": conditionTypeRelayReady,
	"VPA":   "VPAReady",

	"Central": conditionTypeCentralReady,
	// The client-Secret step reports under the condition of the step before it.
	"ClientSecret": conditionTypeCentralReady,
}
EOF
  cat >"$root/controller/ovncentral_controller.go" <<'EOF'
package controller

var centralSubConditionTypes = []string{
	conditionTypeTLSReady,
	conditionTypeRelayReady,
	"VPAReady",
}
EOF
  cat >"$root/controller/ovnchassis_controller.go" <<'EOF'
package controller

var chassisSubConditionTypes = []string{
	// The central comes first.
	conditionTypeCentralReady,
	conditionTypeNodesReady,
}
EOF
  cat >"$root/docs/$METRICS_GUIDE" <<'EOF'
# Enable OVN Operator Metrics

One operator serves two kinds, so `sub_reconciler` carries five values:
`TLS`, `Relay` and `VPA` from the `OVNCentral` pipeline, `Central` and
`ClientSecret` from the `OVNChassis` one. The backup pair carries `ovncentral`.

The next paragraph names `Other`.
EOF
  printf '%s\n' '| Type | Status | Reason | Meaning |' '| --- | --- | --- | --- |' \
    '| `Ready` | True | `AllReady` | All three sub-conditions are True |' \
    '| `Ready` | False | `NotAllReady` | At least one is not |' >"$root/docs/$CENTRAL_PAGE"
  printf '%s\n' '| Type | Status | Reason | Meaning |' '| --- | --- | --- | --- |' \
    '| `Ready` | True | `AllReady` | All two sub-conditions are True |' >"$root/docs/$CHASSIS_PAGE"
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
  echo "Test: pages that restate every list report nothing"

  local root
  root="$(build_fixture)"
  assert_eq "no problems" "" \
    "$(check_ovn_lists "$root/controller" "$root/docs")"
}

test_word_number_reads_one_to_twenty() {
  echo "Test: word_number reads the words one to twenty and nothing else"

  assert_eq "one" "1" "$(word_number one)"
  assert_eq "fifteen" "15" "$(word_number fifteen)"
  assert_eq "twenty" "20" "$(word_number twenty)"
  assert_eq "a digit" "" "$(word_number 15)"
  assert_eq "a capital" "" "$(word_number Six)"
}

test_missing_sources_are_reported() {
  echo "Test: a missing source file or page is reported instead of its comparison"

  local root
  root="$(build_fixture)"
  rm "$root/controller/instrumentation.go" "$root/controller/ovnchassis_controller.go"
  assert_eq "two controller files" \
    "no file: $root/controller/instrumentation.go"$'\n'"no file: $root/controller/ovnchassis_controller.go" \
    "$(check_ovn_lists "$root/controller" "$root/docs")"

  root="$(build_fixture)"
  rm "$root/docs/$METRICS_GUIDE" "$root/docs/$CENTRAL_PAGE"
  assert_eq "two pages" \
    "no page: $root/docs/$METRICS_GUIDE"$'\n'"no page: $root/docs/$CENTRAL_PAGE" \
    "$(check_ovn_lists "$root/controller" "$root/docs")"
}

test_lists_without_entries_are_reported() {
  echo "Test: a map, a slice, a sentence or a row that is not found is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/controller/instrumentation.go" 's/^var subReconcilerConditionTypes/var other/'
  rewrite "$root/controller/ovncentral_controller.go" '/^var centralSubConditionTypes/,/^}/{/^[[:space:]]/d;}'
  assert_eq "no map, an empty slice" \
    "no centralSubConditionTypes in $root/controller/ovncentral_controller.go"$'\n'"no subReconcilerConditionTypes in $root/controller/instrumentation.go" \
    "$(check_ovn_lists "$root/controller" "$root/docs")"

  root="$(build_fixture)"
  rewrite "$root/docs/$METRICS_GUIDE" 's/carries five values:/has five values:/'
  rewrite "$root/docs/$CHASSIS_PAGE" 's/All two sub-conditions/Both sub-conditions/'
  assert_eq "no sentence, no row" \
    "no AllReady row: $root/docs/$CHASSIS_PAGE"$'\n'"no sub_reconciler sentence: $root/docs/$METRICS_GUIDE" \
    "$(check_ovn_lists "$root/controller" "$root/docs")"
}

# shellcheck disable=SC2016 # the page's literal text, not expanded here
test_name_drift_reports_exactly_its_line() {
  echo "Test: a step name on one side only reports exactly its line"

  local root
  root="$(build_fixture)"
  rewrite "$root/controller/instrumentation.go" 's/^\(	"ClientSecret": .*\)$/\1\
	"ChassisVPA": "VPAReady",/'
  assert_eq "a key the guide lacks" \
    "not in the metrics guide: ChassisVPA"$'\n'"sub_reconciler count: the metrics guide says five, the map has 6" \
    "$(check_ovn_lists "$root/controller" "$root/docs")"

  root="$(build_fixture)"
  rewrite "$root/docs/$METRICS_GUIDE" 's/`Relay` and `VPA`/`Relay`, `Backup` and `VPA`/'
  assert_eq "a name the map lacks" "not in subReconcilerConditionTypes: Backup" \
    "$(check_ovn_lists "$root/controller" "$root/docs")"
}

# The fixture has a backticked word after the full stop of the sentence and
# one in the next paragraph. Without the full stop the sentence runs on to the
# next one.
test_sentence_ends_at_its_full_stop() {
  echo "Test: only the sentence up to its full stop is read"

  local root
  root="$(build_fixture)"
  rewrite "$root/docs/$METRICS_GUIDE" 's/one\. The backup pair/one, the backup pair/'
  assert_eq "a sentence without its full stop" "not in subReconcilerConditionTypes: ovncentral" \
    "$(check_ovn_lists "$root/controller" "$root/docs")"
}

test_count_drift_reports_exactly_its_line() {
  echo "Test: a number word that disagrees with its list reports exactly its line"

  local root
  root="$(build_fixture)"
  rewrite "$root/docs/$METRICS_GUIDE" 's/carries five values/carries four values/'
  assert_eq "the sub_reconciler word" \
    "sub_reconciler count: the metrics guide says four, the map has 5" \
    "$(check_ovn_lists "$root/controller" "$root/docs")"

  root="$(build_fixture)"
  rewrite "$root/docs/$CENTRAL_PAGE" 's/All three sub-conditions/All two sub-conditions/'
  assert_eq "the central row" \
    "AllReady count: ${CENTRAL_PAGE##*/} says two, centralSubConditionTypes has 3" \
    "$(check_ovn_lists "$root/controller" "$root/docs")"

  root="$(build_fixture)"
  rewrite "$root/controller/ovnchassis_controller.go" 's/^\(	conditionTypeNodesReady,\)$/\1\
	"VPAReady",/'
  assert_eq "an entry the chassis row does not count" \
    "AllReady count: ${CHASSIS_PAGE##*/} says two, chassisSubConditionTypes has 3" \
    "$(check_ovn_lists "$root/controller" "$root/docs")"
}

# shellcheck disable=SC2016 # the page's literal text, not expanded here
test_repository_pages_restate_the_lists() {
  echo "Test: the repository's OVN pages restate the operator's lists"

  local controller="$PROJECT_ROOT/operators/ovn/internal/controller"
  local copy
  assert_eq "no problems in docs/" "" \
    "$(check_ovn_lists "$controller" "$PROJECT_ROOT/docs")"

  # The same check on a copy without ChassisVPA and with a central row of
  # seven proves it reads the real pages.
  copy="$(mktemp -d "$TMP_ROOT/copy.XXXXXX")"
  mkdir -p "$copy/guides/ovn" "$copy/reference/ovn"
  cp "$PROJECT_ROOT/docs/$METRICS_GUIDE" "$copy/$METRICS_GUIDE"
  cp "$PROJECT_ROOT/docs/$CENTRAL_PAGE" "$copy/$CENTRAL_PAGE"
  cp "$PROJECT_ROOT/docs/$CHASSIS_PAGE" "$copy/$CHASSIS_PAGE"
  rewrite "$copy/$METRICS_GUIDE" 's/`ChassisVPA` and/and/'
  rewrite "$copy/$CENTRAL_PAGE" 's/All eight sub-conditions/All seven sub-conditions/'
  assert_eq "a copy without ChassisVPA and with seven" \
    "AllReady count: ${CENTRAL_PAGE##*/} says seven, centralSubConditionTypes has 8"$'\n'"not in the metrics guide: ChassisVPA" \
    "$(check_ovn_lists "$controller" "$copy")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_consistent_fixture_reports_nothing
test_word_number_reads_one_to_twenty
test_missing_sources_are_reported
test_lists_without_entries_are_reported
test_name_drift_reports_exactly_its_line
test_sentence_ends_at_its_full_stop
test_count_drift_reports_exactly_its_line
test_repository_pages_restate_the_lists

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
