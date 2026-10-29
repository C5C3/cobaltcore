#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the random chaos runs (#1395):
#   1. tests/chaos-random/draw.py on the repository's catalogue: the same
#      seed draws a byte-identical plan and Workflow, another seed another
#      plan; over 200 seeds of each environment a plan never ends past the
#      window, never repeats an entry in consecutive steps, pairs only entries
#      whose first label selectors differ, and holds only entries of its
#      environment; offsets follow length and pause, and a pause is at least
#      the step's largest recovery plus --pause-min and at most plus
#      --pause-max.
#   2. The Workflow: a Serial entry whose children alternate the steps and the
#      pauses, with no pause last; a chaos template s<NN>-<entry> with its
#      entry's kind, deadline and spec, @CP@ and @NS@ replaced; a step of two
#      as the Parallel template s<NN>; a Suspend template per pause.
#   3. The exit codes: an all-lab catalogue drawn for ci and an empty list exit
#      1 naming the environment, a window of 10 s exits 1 naming it, an entry
#      without duration exits 2 naming the entry and the field, as do an
#      unknown kind, an unknown environment and a duration of 2m.
#
# Usage: bash tests/unit/hack/chaos_random_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DRAW_PY="$PROJECT_ROOT/tests/chaos-random/draw.py"
CATALOGUE="$PROJECT_ROOT/tests/chaos-random/catalogue.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# draw <catalogue json file> <args...> — Run draw.py with the catalogue on
# stdin; sets RC, OUT (stdout) and ERR (stderr).
draw() {
  local input="$1"
  shift
  RC=0
  OUT="$(python3 -I "$DRAW_PY" "$@" <"$input" 2>"$TMP_DIR/err")" || RC=$?
  ERR="$(cat "$TMP_DIR/err")"
}

CI_ARGS=(--environment ci --window 1800 --pause-min 30 --pause-max 120 --overlap 0.33)
LAB_ARGS=(--environment lab --window 21600 --pause-min 60 --pause-max 600 --overlap 0.33)

# ---------------------------------------------------------------------------
# Test 1: the plan
# ---------------------------------------------------------------------------
test_the_plan() {
  echo "Test: a seed replays its plan, and every plan keeps to the draw's rules"

  yq -o=json "$CATALOGUE" >"$TMP_DIR/catalogue.json"
  draw "$TMP_DIR/catalogue.json" --seed 7 "${CI_ARGS[@]}" --workflow "$TMP_DIR/w7a.json"
  cp "$TMP_DIR/w7a.json" "$TMP_DIR/w7a.keep"
  local first="$OUT"
  draw "$TMP_DIR/catalogue.json" --seed 7 "${CI_ARGS[@]}" --workflow "$TMP_DIR/w7b.json"
  assert_eq "seed 7 draws exit 0" "0" "$RC"
  assert_eq "seed 7 draws the same plan twice" "$first" "$OUT"
  if cmp -s "$TMP_DIR/w7a.keep" "$TMP_DIR/w7b.json"; then
    echo "  PASS: and the same Workflow"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: seed 7 rendered two different Workflows"
    FAIL=$((FAIL + 1))
  fi
  draw "$TMP_DIR/catalogue.json" --seed 8 "${CI_ARGS[@]}"
  assert_not_contains "seed 8 draws another plan" "$OUT" "$first"
  assert_eq "the plan names its seed, environment and window" "7 ci 1800" \
    "$(jq -r '"\(.seed) \(.environment) \(.window)"' <<<"$first")"

  # One line per entry: its name, environments, duration, recovery and the
  # first key=value of its selector's labelSelectors.
  jq -r '.[] | [.name, (.environments | join(",")), (.duration | rtrimstr("s")), (.recovery | rtrimstr("s")),
    (.spec.selector.labelSelectors | to_entries[0] | "\(.key)=\(.value)")] | @tsv' \
    "$TMP_DIR/catalogue.json" >"$TMP_DIR/entries.tsv"

  local env seed problems="" plans=0 pairs=0 args
  for env in ci lab; do
    if [[ "$env" == ci ]]; then args=("${CI_ARGS[@]}"); else args=("${LAB_ARGS[@]}"); fi
    for seed in $(seq 1 200); do
      draw "$TMP_DIR/catalogue.json" --seed "$seed" "${args[@]}"
      if [[ "$RC" != 0 ]]; then
        problems="${problems}${env} seed ${seed}: exit ${RC}: ${ERR}"$'\n'
        continue
      fi
      plans=$((plans + 1))
      pairs=$((pairs + $(jq '[.steps[] | select((.entries | length) == 2)] | length' <<<"$OUT")))
      problems="${problems}$(jq -r --arg env "$env" --arg seed "$seed" --rawfile table "$TMP_DIR/entries.tsv" \
        --argjson pmin "${args[5]}" --argjson pmax "${args[7]}" '
        ($table | split("\n") | map(select(. != "") | split("\t"))
          | map({key: .[0], value: {envs: (.[1] | split(",")), duration: (.[2] | tonumber),
                 recovery: (.[3] | tonumber), first: .[4]}}) | from_entries) as $e
        | . as $plan
        | [ (.steps | to_entries[] | .key as $i | .value as $s
             | (if $s.offset + $s.length > $plan.window then "step \($s.index) ends past the window" else empty end),
               (if $i > 0 and ([$s.entries[] | IN($plan.steps[$i - 1].entries[])] | any) then "step \($s.index) repeats an entry of step \($i)" else empty end),
               (if ($s.entries | length) == 2 and $e[$s.entries[0]].first == $e[$s.entries[1]].first then "step \($s.index) pairs two entries of one first label selector" else empty end),
               (if ([$s.entries[] | $e[.].envs | index($env) == null] | any) then "step \($s.index) holds an entry outside \($env)" else empty end),
               (if $s.length != ([$s.entries[] | $e[.].duration] | max) then "step \($s.index) has length \($s.length)" else empty end),
               (([$s.entries[] | $e[.].recovery] | max) as $r
                | if $s.pause < $r + $pmin or $s.pause > $r + $pmax then "step \($s.index) pauses \($s.pause) s" else empty end),
               (if $i > 0 and $s.offset != $plan.steps[$i - 1].offset + $plan.steps[$i - 1].length + $plan.steps[$i - 1].pause
                then "step \($s.index) starts at \($s.offset)" else empty end)),
            (if .steps[0].offset != 0 then "the first step starts at \(.steps[0].offset)" else empty end) ]
        | .[] | "\($env) seed \($seed): \(.)"' <<<"$OUT")"$'\n'
    done
  done
  assert_eq "400 plans were drawn" "400" "$plans"
  assert_eq "no plan breaks a rule" "" "$(grep -v '^$' <<<"$problems")"
  assert_gte "the plans hold steps of two" "$pairs" 1
}

# ---------------------------------------------------------------------------
# Test 2: the Workflow
# ---------------------------------------------------------------------------
test_the_workflow() {
  echo "Test: the Workflow alternates the steps and the pauses under one Serial entry"

  cat >"$TMP_DIR/small.json" <<'JSON'
[
 {"name": "a-pod-kill", "kind": "PodChaos", "environments": ["ci"], "duration": "30s", "recovery": "60s",
  "spec": {"action": "pod-kill", "mode": "one",
           "selector": {"namespaces": ["@NS@"], "labelSelectors": {"app.kubernetes.io/name": "a", "app.kubernetes.io/instance": "@CP@-a"}}}},
 {"name": "b-partition", "kind": "NetworkChaos", "environments": ["ci", "lab"], "duration": "120s", "recovery": "90s",
  "spec": {"action": "partition", "mode": "all", "direction": "both",
           "selector": {"namespaces": ["@NS@"], "labelSelectors": {"app.kubernetes.io/name": "b"}},
           "target": {"mode": "all", "selector": {"namespaces": ["@NS@"], "labelSelectors": {"app.kubernetes.io/name": "a"}}}}},
 {"name": "c-pod-failure", "kind": "PodChaos", "environments": ["ci"], "duration": "120s", "recovery": "60s",
  "spec": {"action": "pod-failure", "mode": "all",
           "selector": {"namespaces": ["kube-x"], "labelSelectors": {"app.kubernetes.io/name": "c"}}}}
]
JSON
  draw "$TMP_DIR/small.json" --seed 3 --environment ci --window 3600 --pause-min 10 --pause-max 20 --overlap 1 \
    --name chaos-random-x --cp cp1 --namespace ns1 --workflow "$TMP_DIR/w.json"
  assert_eq "it exits 0" "0" "$RC"
  local w="$TMP_DIR/w.json" plan="$OUT" n
  n="$(jq '.steps | length' <<<"$plan")"
  assert_gte "the plan holds steps" "$n" 2
  assert_eq "at an overlap of 1 a pair alternates with the one entry the previous step left" "a-pod-kill,b-partition c-pod-failure" \
    "$(jq -r '[.steps[0:2][] | .entries | sort | join(",")] | join(" ")' <<<"$plan")"
  assert_eq "the Workflow's kind, name and namespace" "chaos-mesh.org/v1alpha1 Workflow chaos-random-x ns1" \
    "$(jq -r '"\(.apiVersion) \(.kind) \(.metadata.name) \(.metadata.namespace)"' "$w")"
  assert_eq "its entry is the Serial template entry" "entry Serial" \
    "$(jq -r '.spec.entry as $e | .spec.templates[] | select(.name == $e) | "\(.name) \(.templateType)"' "$w")"
  assert_eq "the Serial children alternate the steps and pauses, with no pause last" \
    "$(jq -r '[.steps[] | (.index | tostring | if length < 2 then "0" + . else . end) as $nn
        | if (.entries | length) == 2 then "s" + $nn else "s" + $nn + "-" + .entries[0] end] as $s
      | [range(0; $s | length) as $i | $s[$i], (if $i < ($s | length) - 1 then "p" + ($s[$i] | sub("^s"; "") | sub("-.*"; "")) else empty end)]
      | join(" ")' <<<"$plan")" \
    "$(jq -r '.spec.templates[0].children | join(" ")' "$w")"
  assert_eq "a step of two is a Parallel of its two chaos templates" \
    "$(jq -r '.steps[0].entries | map("s01-" + .) | join(" ")' <<<"$plan")" \
    "$(jq -r '.spec.templates[] | select(.name == "s01") | select(.templateType == "Parallel") | .children | join(" ")' "$w")"
  assert_eq "a pause is a Suspend of the step's pause" "Suspend $(jq -r '.steps[0].pause' <<<"$plan")s" \
    "$(jq -r '.spec.templates[] | select(.name == "p01") | "\(.templateType) \(.deadline)"' "$w")"
  local tmpl
  tmpl="$(jq -c '[.spec.templates[] | select(.name | test("^s[0-9]+-a-pod-kill$"))][0]' "$w")"
  assert_eq "a PodChaos template carries the kind, the deadline and the spec under podChaos" \
    'PodChaos 30s {"action":"pod-kill","mode":"one","selector":{"namespaces":["ns1"],"labelSelectors":{"app.kubernetes.io/name":"a","app.kubernetes.io/instance":"cp1-a"}}}' \
    "$(jq -r '"\(.templateType) \(.deadline) \(.podChaos | tojson)"' <<<"$tmpl")"
  tmpl="$(jq -c '[.spec.templates[] | select(.name | test("^s[0-9]+-b-partition$"))][0]' "$w")"
  assert_eq "a NetworkChaos template carries its spec under networkChaos, the target replaced too" \
    'NetworkChaos 120s ns1 ns1' \
    "$(jq -r '"\(.templateType) \(.deadline) \(.networkChaos.selector.namespaces[0]) \(.networkChaos.target.selector.namespaces[0])"' <<<"$tmpl")"
  assert_eq "no template keeps a placeholder" "" "$(grep -o '@[A-Z]*@' "$w" | sort -u)"
  assert_eq "every chaos template of the plan is in the Workflow" \
    "$(jq -r '.steps[] | .index as $i | .entries[] | "s\($i | tostring | if length < 2 then "0" + . else . end)-\(.)"' <<<"$plan" | sort)" \
    "$(jq -r '.spec.templates[] | select(.templateType == "PodChaos" or .templateType == "NetworkChaos") | .name' "$w" | sort)"

  draw "$TMP_DIR/small.json" --seed 3 --environment lab --window 3600 --pause-min 10 --pause-max 20 --overlap 1 \
    --workflow "$TMP_DIR/w-lab.json"
  assert_eq "a single step is the chaos template itself, with no pause after it when it is the last" "s01-b-partition" \
    "$(jq -r '.spec.templates[0].children | join(" ")' "$TMP_DIR/w-lab.json")"
  assert_eq "and the defaults name the Workflow chaos-random in openstack" "chaos-random openstack" \
    "$(jq -r '"\(.metadata.name) \(.metadata.namespace)"' "$TMP_DIR/w-lab.json")"
}

# ---------------------------------------------------------------------------
# Test 3: the exit codes
# ---------------------------------------------------------------------------
test_the_exit_codes() {
  echo "Test: draw.py exits 1 without a step and 2 on an input error"

  echo '[{"name": "x", "kind": "PodChaos", "environments": ["lab"], "duration": "30s", "recovery": "60s", "spec": {}}]' \
    >"$TMP_DIR/lab-only.json"
  draw "$TMP_DIR/lab-only.json" --seed 1 "${CI_ARGS[@]}"
  assert_eq "an all-lab catalogue for ci exits 1" "1" "$RC"
  assert_eq "naming the environment" "draw.py: no catalogue entry for environment ci" "$ERR"

  echo '[]' >"$TMP_DIR/empty.json"
  draw "$TMP_DIR/empty.json" --seed 1 "${CI_ARGS[@]}"
  assert_eq "an empty list exits 1 with the same message" "1 draw.py: no catalogue entry for environment ci" "$RC $ERR"

  draw "$TMP_DIR/catalogue.json" --seed 7 --environment ci --window 10 --pause-min 30 --pause-max 120 --overlap 0.33
  assert_eq "a window of 10 s exits 1 naming it" "1 draw.py: the window of 10 s holds no step" "$RC $ERR"

  echo '[{"name": "x", "kind": "PodChaos", "environments": ["ci"], "recovery": "60s", "spec": {}}]' >"$TMP_DIR/bad.json"
  draw "$TMP_DIR/bad.json" --seed 1 "${CI_ARGS[@]}"
  assert_eq "an entry without duration exits 2 naming the entry and the field" "2 draw.py: entry x: duration is missing" "$RC $ERR"

  echo '[{"name": "x", "kind": "IOChaos", "environments": ["ci"], "duration": "30s", "recovery": "60s", "spec": {}}]' >"$TMP_DIR/bad.json"
  draw "$TMP_DIR/bad.json" --seed 1 "${CI_ARGS[@]}"
  assert_eq "an unknown kind exits 2" "2 draw.py: entry x: kind \"IOChaos\" is neither PodChaos nor NetworkChaos" "$RC $ERR"

  echo '[{"name": "x", "kind": "PodChaos", "environments": ["ci", "prod"], "duration": "30s", "recovery": "60s", "spec": {}}]' >"$TMP_DIR/bad.json"
  draw "$TMP_DIR/bad.json" --seed 1 "${CI_ARGS[@]}"
  assert_eq "an unknown environment exits 2" "2 draw.py: entry x: environment \"prod\" is neither ci nor lab" "$RC $ERR"

  echo '[{"name": "x", "kind": "PodChaos", "environments": ["ci"], "duration": "2m", "recovery": "60s", "spec": {}}]' >"$TMP_DIR/bad.json"
  draw "$TMP_DIR/bad.json" --seed 1 "${CI_ARGS[@]}"
  assert_eq "a duration of 2m exits 2" "2 draw.py: entry x: duration \"2m\" is not <n>s with n >= 1" "$RC $ERR"

  echo 'not json' >"$TMP_DIR/bad.json"
  draw "$TMP_DIR/bad.json" --seed 1 "${CI_ARGS[@]}"
  assert_eq "a catalogue that is not JSON exits 2" "2" "$RC"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
if ! command -v yq >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1 || ! command -v python3 >/dev/null 2>&1; then
  echo "SKIP: yq, jq or python3 not installed (every check of this file skipped)"
  echo ""
  echo "Results: 0 passed, 0 failed, 1 skipped"
  exit 0
fi

test_the_plan
test_the_workflow
test_the_exit_codes

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
