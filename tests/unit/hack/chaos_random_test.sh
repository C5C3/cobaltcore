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
#   4. hack/chaos-random.sh's settings: CHAOS_RANDOM_ENV is required and ci or
#      lab, the window defaults to 30m on ci and 6h on lab, and a malformed
#      seed, window, tail or warm-up is a usage error; draw prints the plan of
#      a seed, the same twice, and `make -n chaos-random-<sub>` calls it.
#   5. The preflight's selection: an entry whose instance label matches no pod
#      fails with "catalogue entry <name> selects no pod in ci", a NetworkChaos
#      whose target matches none fails, a terminating pod and a Succeeded one
#      select nothing, on lab a namespace without chaos-mesh.org/inject is
#      closed, and a missing namespace dizzy fails naming it.
#   6. quiet-tail, with kubectl's service proxy to VictoriaMetrics stubbed: an
#      empty result counts 0 and passes, 3 rising to 5 fails with 2 errors,
#      "status":"error" and a failed kubectl get --raw each fail with their
#      message, and a run without a lift fails.
#   7. stuck-resources: the Job's own listing, run here against a stubbed
#      openstack CLI, flags a server in BUILD and a volume in error_deleting
#      and passes ACTIVE and available; a Job that fails fails the check with
#      its last log line. stuck-objects fails on a left WorkflowNode and on a
#      pod terminating for more than 300 s.
#   8. no-restarts and memory-growth judged again: a restarted target is
#      exempt and named in the skip count, a restarted other container fails
#      no-restarts with the windows open at its first sample after the
#      restart. memory-growth skips a growing flux-system container unless
#      the soak's meta.json lists no namespace to skip. The JUnit file holds one testsuite per check, which
#      hack/ci-junit-failed-suites.py reads as random-<check> with the detail
#      and the seed.
#   9. run: a refused Workflow apply fails workflow with kubectl's message and
#      stops the soak; a soak Job still running 600 s after the churn end is
#      stopped and fails dizzy-exit with "did not finish within 600 s"; up
#      exits 1 with "up: controlplane not ready within 1800 s" and the
#      ControlPlane's conditions when its wait fails, after it onboarded the
#      database-engine tenant with the root token of openbao-init-keys once
#      the MariaDB was Ready; a MariaDB that does not get Ready and an
#      init-output without a root token stop up before the onboarding; every
#      file of tests/chaos-random/kind/ the script names is tracked by git.
#
# Steps 5 to 9 source the script in a subshell (its BASH_SOURCE guard keeps
# cr_main from running) with a kubectl stub first on PATH, and replace the
# clock, the sleeps and the checks a test does not cover by plain functions.
#
# Usage: bash tests/unit/hack/chaos_random_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DRAW_PY="$PROJECT_ROOT/tests/chaos-random/draw.py"
CHAOS_RANDOM_SH="$PROJECT_ROOT/hack/chaos-random.sh"
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
# The stubs of steps 5 to 9
# ---------------------------------------------------------------------------

# make_stubs <dir> — A kubectl stub in <dir>/bin that appends its argv to
# $CALL_LOG and answers from files in <dir> and from the environment:
#   KUBECTL_VERSION_RC       non-empty: `version` fails
#   KUBECTL_MISSING_NS       space-separated namespaces `get namespace` misses
#   KUBECTL_CP_READY         the ControlPlane's Ready status (default True)
#   KUBECTL_ACCOMPLISHED     the Workflow's Accomplished status (default True)
#   KUBECTL_APPLY_FAIL       a text; `apply -f <file containing it>` fails
#   KUBECTL_WAIT_FAIL        a text; `wait <object containing it>` fails
#   KUBECTL_STUCK_STATE      the stuck Job's true condition (default Complete)
#   KUBECTL_INIT_OUTPUT      the init-output of openbao-init-keys (default
#                            {"root_token":"s.root"})
#   VM_RC                    non-empty: `get --raw` fails
# and the files pods.json, namespaces.json, soak-job.json, chaos.json,
# left.txt, stuck.log, vm.<epoch> and vm.default.
make_stubs() {
  local dir="$1"
  mkdir -p "$dir/bin"
  echo '{"items":[]}' >"$dir/pods.json"
  echo '{"items":[]}' >"$dir/namespaces.json"
  echo '{"items":[]}' >"$dir/chaos.json"
  echo '{"status":"success","data":{"resultType":"vector","result":[]}}' >"$dir/vm.default"
  : >"$dir/left.txt"
  : >"$dir/stuck.log"
  cat >"$dir/bin/kubectl" <<'STUB'
#!/bin/bash
dir="$(cd "$(dirname "$0")/.." && pwd)"
echo "$*" >>"$CALL_LOG"
args="$*"
case "$args" in
  version*)
    if [ -n "${KUBECTL_VERSION_RC:-}" ]; then echo "The connection to the server localhost:8080 was refused" >&2; exit 1; fi
    echo "Client Version: v1.35.0" ;;
  "get --raw "*)
    if [ -n "${VM_RC:-}" ]; then echo "Error from server (ServiceUnavailable): the server is currently unable to handle the request" >&2; exit 1; fi
    t="${args##*time=}"
    if [ -f "$dir/vm.$t" ]; then cat "$dir/vm.$t"; else cat "$dir/vm.default"; fi ;;
  "get namespace "*)
    ns="$(echo "$args" | awk '{ print $3 }')"
    for m in ${KUBECTL_MISSING_NS:-}; do
      if [ "$m" = "$ns" ]; then echo "Error from server (NotFound): namespaces \"$ns\" not found" >&2; exit 1; fi
    done
    echo "namespace/$ns" ;;
  "get controlplane "*) printf '%s' "${KUBECTL_CP_READY:-True}" ;;
  "get controlplane/"*" -o json")
    printf '%s' '{"kind":"ControlPlane","metadata":{"name":"controlplane"},"status":{"conditions":[{"type":"Ready","status":"False","reason":"DBCredentialsNotReady","message":"waiting"}]}}' ;;
  "get workflows.chaos-mesh.org -A"*) cat "$dir/workflows.txt" 2>/dev/null || true ;;
  "get workflows.chaos-mesh.org "*) printf '%s' "${KUBECTL_ACCOMPLISHED:-True}" ;;
  "get workflownodes.chaos-mesh.org "*) node="$(echo "$args" | awk '{ print $3 }')"; printf '%s' "${node%-*}" ;;
  "get job dizzy-soak "*) cat "$dir/soak-job.json" 2>/dev/null || true ;;
  "get pods -A -o json") cat "$dir/pods.json" ;;
  "get namespaces -o json") cat "$dir/namespaces.json" ;;
  "get nodes "*) printf '%s' kind-control-plane ;;
  "get podchaos,networkchaos,workflownodes.chaos-mesh.org "*) cat "$dir/left.txt" ;;
  "get podchaos,networkchaos -n "*"-o json") cat "$dir/chaos.json" ;;
  "get podchaos,networkchaos -n "*"-o name") ;;
  "get neutron "*) printf '%s' '{"syncMode":"log","suspend":true}' ;;
  "get job chaos-random-stuck "*) printf '%s' "${KUBECTL_STUCK_STATE:-Complete}" ;;
  "get secret openbao-init-keys "*)
    init='{"root_token":"s.root"}'
    printf '%s' "${KUBECTL_INIT_OUTPUT:-$init}" | base64 | tr -d '\n' ;;
  "logs job/chaos-random-stuck "*) cat "$dir/stuck.log" ;;
  "get "*" -A -o json") echo '{"items":[]}' ;;
  "apply -f "*)
    if [ -n "${KUBECTL_APPLY_FAIL:-}" ] && [[ "$args" == *"$KUBECTL_APPLY_FAIL"* ]]; then
      echo 'Error from server (BadRequest): error when creating "workflow.json": admission webhook "vworkflow.kb.io" denied the request: spec.templates[1].deadline: Invalid value' >&2
      exit 1
    fi
    echo "applied" ;;
  "wait "*)
    if [ -n "${KUBECTL_WAIT_FAIL:-}" ] && [[ "$args" == *"$KUBECTL_WAIT_FAIL"* ]]; then
      echo "error: timed out waiting for the condition" >&2
      exit 1
    fi ;;
  "create -f -") cat >"$dir/created.json" ;;
  *) ;;
esac
exit 0
STUB
  # The soak's driver: records the subcommand, and report writes a soak run.
  cat >"$dir/bin/dizzy-soak" <<'STUB'
#!/bin/bash
echo "soak $*" >>"$CALL_LOG"
if [ "$1" = report ]; then
  run="$(date -u +%Y%m%dT%H%M%SZ)"
  mkdir -p "$SOAK_OUT/$run/platform"
  cp -R "$SOAK_FIXTURE/." "$SOAK_OUT/$run/" 2>/dev/null || true
fi
exit 0
STUB
  cat >"$dir/bin/broker-vhost" <<'STUB'
#!/bin/bash
echo "broker-vhost $*" >>"$CALL_LOG"
STUB
  # setup-database-tenant.sh: records its arguments and the token it got.
  cat >"$dir/bin/db-tenant" <<'STUB'
#!/bin/bash
echo "db-tenant $* BAO_TOKEN=${BAO_TOKEN:-}" >>"$CALL_LOG"
STUB
  chmod +x "$dir/bin/kubectl" "$dir/bin/dizzy-soak" "$dir/bin/broker-vhost" "$dir/bin/db-tenant"
}

# new_case <tmp> — Fresh stubs, output and call log under <tmp>.
new_case() {
  rm -rf "$1"
  mkdir -p "$1/out" "$1/reports" "$1/soak-out" "$1/soak-fixture/platform" "$1/tmpdir"
  make_stubs "$1"
  export CALL_LOG="$1/calls.log" SOAK_OUT="$1/soak-out" SOAK_FIXTURE="$1/soak-fixture"
  : >"$CALL_LOG"
}

# in_cr <tmp> [env_var=value...] — Source the script in the current (sub)shell
# with the stubs first on PATH and the given settings, its paths in <tmp>.
# shellcheck disable=SC2034 # the sourced script reads them
in_cr() {
  local tmp="$1" assignment
  shift
  unset CHAOS_RANDOM_ENV CHAOS_RANDOM_SEED CHAOS_RANDOM_WINDOW CHAOS_RANDOM_TAIL CHAOS_RANDOM_WARMUP \
    CHAOS_RANDOM_CP_NAME CHAOS_RANDOM_CP_NAMESPACE DIZZY_ARGS DIZZY_SCENARIO
  for assignment in "$@"; do
    export "${assignment?}"
  done
  PATH="$tmp/bin:$PATH"
  TMPDIR="$tmp/tmpdir"
  export PATH TMPDIR
  # shellcheck source=hack/chaos-random.sh
  source "$CHAOS_RANDOM_SH"
  CR_OUTPUT_DIR="$tmp/out"
  CR_REPORTS_DIR="$tmp/reports"
  CR_SOAK="$tmp/bin/dizzy-soak"
  CR_SOAK_OUTPUT_DIR="$tmp/soak-out"
  CR_BROKER_VHOST="$tmp/bin/broker-vhost"
  CR_DB_TENANT="$tmp/bin/db-tenant"
  CR_DISCOVER_HOSTS="$tmp/bin/broker-vhost"
  CR_POLL=0
}

# checks_of <dir> <name> — "pass detail" of a check in <dir>/checks.jsonl.
check_in() {
  jq -r --arg n "$2" 'select(.name == $n) | "\(.pass) \(.detail)"' "$1/checks.jsonl" 2>/dev/null
}

# pod_json <namespace> <name> <labels json> [phase] [deletionTimestamp]
pod_json() {
  jq -cn --arg ns "$1" --arg name "$2" --argjson labels "$3" --arg phase "${4:-Running}" --arg del "${5:-}" \
    '{metadata: ({namespace: $ns, name: $name, labels: $labels} + (if $del != "" then {deletionTimestamp: $del} else {} end)),
      status: {phase: $phase}}'
}

# ---------------------------------------------------------------------------
# Test 4: the settings, draw and the make targets
# ---------------------------------------------------------------------------
test_the_settings() {
  echo "Test: the settings are checked before any call, and draw needs no cluster"

  local tmp rc out sub
  tmp="$TMP_DIR/settings"
  new_case "$tmp"
  rc=0
  out="$(env -u CHAOS_RANDOM_ENV PATH="$tmp/bin:$PATH" bash "$CHAOS_RANDOM_SH" draw 2>&1)" || rc=$?
  assert_eq "an unset CHAOS_RANDOM_ENV is a usage error" "2" "$rc"
  assert_contains "naming it" "$out" "CHAOS_RANDOM_ENV is not set; it is ci or lab"
  rc=0
  out="$(env CHAOS_RANDOM_ENV=staging PATH="$tmp/bin:$PATH" bash "$CHAOS_RANDOM_SH" draw 2>&1)" || rc=$?
  assert_eq "CHAOS_RANDOM_ENV=staging is a usage error" "2" "$rc"
  assert_contains "naming it" "$out" "CHAOS_RANDOM_ENV must be ci or lab, not 'staging'"
  for case in "CHAOS_RANDOM_SEED=seven|CHAOS_RANDOM_SEED must be a whole number" \
    "CHAOS_RANDOM_WINDOW=0|CHAOS_RANDOM_WINDOW must be a duration" "CHAOS_RANDOM_WINDOW=30x|CHAOS_RANDOM_WINDOW must be a duration" \
    "CHAOS_RANDOM_TAIL=ten|CHAOS_RANDOM_TAIL must be a duration" "CHAOS_RANDOM_WARMUP=-1s|CHAOS_RANDOM_WARMUP must be a duration"; do
    rc=0
    out="$(env CHAOS_RANDOM_ENV=ci "${case%%|*}" PATH="$tmp/bin:$PATH" bash "$CHAOS_RANDOM_SH" draw 2>&1)" || rc=$?
    assert_eq "${case%%|*} is a usage error" "2" "$rc"
    assert_contains "naming the setting" "$out" "${case#*|}"
  done
  assert_eq "no setting error called kubectl" "" "$(cat "$CALL_LOG")"

  assert_eq "the window defaults to 30m on ci and 6h on lab" "1800 21600" \
    "$( (in_cr "$tmp" CHAOS_RANDOM_ENV=ci; cr_check_settings; a="$CR_WINDOW_S"; CHAOS_RANDOM_ENV=lab; CHAOS_RANDOM_WINDOW=""; cr_check_settings; echo "$a $CR_WINDOW_S") )"
  assert_eq "a duration of hours, minutes and seconds" "5430 90 0 7" \
    "$( (in_cr "$tmp"; echo "$(cr_seconds 1h30m30s) $(cr_seconds 90s) $(cr_seconds 0) $(cr_seconds 7)") )"

  local first second
  first="$(CHAOS_RANDOM_ENV=ci CHAOS_RANDOM_SEED=7 bash "$CHAOS_RANDOM_SH" draw)"
  second="$(CHAOS_RANDOM_ENV=ci CHAOS_RANDOM_SEED=7 bash "$CHAOS_RANDOM_SH" draw)"
  assert_eq "draw prints the same plan for a seed twice" "$first" "$second"
  assert_eq "the plan of seed 7 on ci over 30 minutes" "7 ci 1800" "$(jq -r '"\(.seed) \(.environment) \(.window)"' <<<"$first")"
  assert_not_contains "and seed 8 another" "$(CHAOS_RANDOM_ENV=ci CHAOS_RANDOM_SEED=8 bash "$CHAOS_RANDOM_SH" draw)" "$first"
  for sub in draw up run report; do
    assert_eq "make -n chaos-random-$sub calls hack/chaos-random.sh $sub" "hack/chaos-random.sh $sub" \
      "$(make -s -n -C "$PROJECT_ROOT" "chaos-random-$sub" 2>/dev/null)"
  done
}

# ---------------------------------------------------------------------------
# Test 5: the preflight
# ---------------------------------------------------------------------------
test_the_preflight() {
  echo "Test: the preflight proves every entry of the environment selects a pod"

  local tmp="$TMP_DIR/preflight" rc out
  new_case "$tmp"
  # One pod per entry of a small catalogue.
  cat >"$tmp/catalogue.yaml" <<'YAML'
- name: keystone-api-pod-kill
  kind: PodChaos
  environments: [ci, lab]
  duration: 30s
  recovery: 60s
  spec:
    action: pod-kill
    mode: one
    selector:
      namespaces: ["@NS@"]
      labelSelectors: {app.kubernetes.io/name: keystone, app.kubernetes.io/instance: "@CP@-keystone"}
- name: keystone-mariadb-partition
  kind: NetworkChaos
  environments: [ci]
  duration: 120s
  recovery: 60s
  spec:
    action: partition
    mode: all
    selector:
      namespaces: ["@NS@"]
      labelSelectors: {app.kubernetes.io/name: mariadb}
    direction: both
    target:
      mode: all
      selector:
        namespaces: ["@NS@"]
        labelSelectors: {app.kubernetes.io/name: keystone}
        expressionSelectors: [{key: batch.kubernetes.io/job-name, operator: DoesNotExist}]
- name: libvirt-pod-kill
  kind: PodChaos
  environments: [lab]
  duration: 30s
  recovery: 600s
  spec:
    action: pod-kill
    mode: one
    selector:
      namespaces: ["@NS@"]
      labelSelectors: {app.kubernetes.io/name: libvirt}
YAML
  {
    echo '{"items":['
    pod_json openstack keystone-0 '{"app.kubernetes.io/name":"keystone","app.kubernetes.io/instance":"controlplane-keystone"}'
    echo ','
    pod_json openstack openstack-db-0 '{"app.kubernetes.io/name":"mariadb"}'
    echo ']}'
  } >"$tmp/pods.json"
  echo '{"items":[{"metadata":{"name":"openstack"}}]}' >"$tmp/namespaces.json"

  rc=0
  out="$( (in_cr "$tmp" CHAOS_RANDOM_ENV=ci; CR_CATALOGUE="$tmp/catalogue.yaml"; cr_check_settings; cr_preflight) 2>&1)" || rc=$?
  assert_eq "a plane where every entry selects a pod passes" "0" "$rc"
  assert_contains "and says so" "$out" "OK: every one of the 2 catalogue entries of ci selects a pod"
  assert_contains "it checks the tools first" "$out" "OK: kubectl jq yq python3 on PATH"

  {
    echo '{"items":['
    pod_json openstack keystone-0 '{"app.kubernetes.io/name":"keystone","app.kubernetes.io/instance":"cp-keystone"}'
    echo ','
    pod_json openstack openstack-db-0 '{"app.kubernetes.io/name":"mariadb"}'
    echo ']}'
  } >"$tmp/pods.json"
  rc=0
  out="$( (in_cr "$tmp" CHAOS_RANDOM_ENV=ci; CR_CATALOGUE="$tmp/catalogue.yaml"; cr_check_settings; cr_preflight) 2>&1)" || rc=$?
  assert_eq "an entry whose instance label is wrong fails" "1" "$rc"
  assert_contains "naming the entry and the environment" "$out" "FAIL: catalogue entry keystone-api-pod-kill selects no pod in ci"
  assert_not_contains "the partition still selects its pods" "$out" "keystone-mariadb-partition"

  {
    echo '{"items":['
    pod_json openstack keystone-0 '{"app.kubernetes.io/name":"keystone","app.kubernetes.io/instance":"controlplane-keystone-db","batch.kubernetes.io/job-name":"db-sync"}'
    echo ','
    pod_json openstack keystone-1 '{"app.kubernetes.io/name":"keystone","app.kubernetes.io/instance":"controlplane-keystone"}' Running 2026-01-01T00:00:00Z
    echo ','
    pod_json openstack keystone-2 '{"app.kubernetes.io/name":"keystone","app.kubernetes.io/instance":"controlplane-keystone"}' Succeeded
    echo ','
    pod_json openstack openstack-db-0 '{"app.kubernetes.io/name":"mariadb"}'
    echo ']}'
  } >"$tmp/pods.json"
  rc=0
  out="$( (in_cr "$tmp" CHAOS_RANDOM_ENV=ci; CR_CATALOGUE="$tmp/catalogue.yaml"; cr_check_settings; cr_preflight) 2>&1)" || rc=$?
  assert_contains "a NetworkChaos target that only a Job pod matches fails" "$out" \
    "FAIL: catalogue entry keystone-mariadb-partition selects no pod in ci"
  assert_contains "a terminating and a Succeeded pod select nothing" "$out" \
    "FAIL: catalogue entry keystone-api-pod-kill selects no pod in ci"

  {
    echo '{"items":['
    pod_json openstack keystone-0 '{"app.kubernetes.io/name":"keystone","app.kubernetes.io/instance":"controlplane-keystone"}'
    echo ','
    pod_json openstack libvirt-0 '{"app.kubernetes.io/name":"libvirt"}'
    echo ']}'
  } >"$tmp/pods.json"
  rc=0
  out="$( (in_cr "$tmp" CHAOS_RANDOM_ENV=lab; CR_CATALOGUE="$tmp/catalogue.yaml"; cr_check_settings; cr_preflight) 2>&1)" || rc=$?
  assert_contains "on lab a namespace without chaos-mesh.org/inject is closed" "$out" \
    "FAIL: catalogue entry libvirt-pod-kill selects no pod in lab"
  echo '{"items":[{"metadata":{"name":"openstack","annotations":{"chaos-mesh.org/inject":"enabled"}}}]}' >"$tmp/namespaces.json"
  rc=0
  out="$( (in_cr "$tmp" CHAOS_RANDOM_ENV=lab; CR_CATALOGUE="$tmp/catalogue.yaml"; cr_check_settings; cr_preflight) 2>&1)" || rc=$?
  assert_eq "and open with it" "0" "$rc"
  assert_contains "the lab's two entries" "$out" "OK: every one of the 2 catalogue entries of lab selects a pod"

  rc=0
  out="$( (in_cr "$tmp" CHAOS_RANDOM_ENV=ci KUBECTL_MISSING_NS=dizzy; cr_check_settings; cr_preflight) 2>&1)" || rc=$?
  assert_eq "without the namespace dizzy the preflight fails" "1" "$rc"
  assert_contains "naming it" "$out" 'FAIL: namespace dizzy is missing: Error from server (NotFound): namespaces "dizzy" not found'
  rc=0
  out="$( (in_cr "$tmp" CHAOS_RANDOM_ENV=ci KUBECTL_CP_READY=False; cr_check_settings; cr_preflight) 2>&1)" || rc=$?
  assert_contains "a ControlPlane not Ready fails" "$out" "FAIL: ControlPlane openstack/controlplane is not Ready=True (read 'False')"
  echo '{"status":{"conditions":[]}}' >"$tmp/soak-job.json"
  rc=0
  out="$( (in_cr "$tmp" CHAOS_RANDOM_ENV=ci; cr_check_settings; cr_preflight) 2>&1)" || rc=$?
  assert_contains "a running soak fails" "$out" "FAIL: a soak runs (Job dizzy/dizzy-soak)"
  rm -f "$tmp/soak-job.json"
  echo "openstack/chaos-random-20261029t220000z" >"$tmp/workflows.txt"
  rc=0
  out="$( (in_cr "$tmp" CHAOS_RANDOM_ENV=ci; cr_check_settings; cr_preflight) 2>&1)" || rc=$?
  assert_contains "a random run's Workflow fails" "$out" "FAIL: a random run's Workflow exists: openstack/chaos-random-20261029t220000z"
}

# ---------------------------------------------------------------------------
# Test 6: quiet-tail
# ---------------------------------------------------------------------------
vm_value() {
  printf '{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[%s,"%s"]}]}}\n' "$1" "$2"
}

test_quiet_tail() {
  echo "Test: quiet-tail counts dizzy's failed operations after the last recovery"

  local tmp="$TMP_DIR/quiet" lift=1767225600 from to
  new_case "$tmp"
  mkdir -p "$tmp/run"
  # mariadb-pod-kill recovers within 60 s, keystone-api-pod-failure within 180.
  printf '%s\ts01-mariadb-pod-kill\tmariadb-pod-kill\t%s\n' "2026-01-01T00:00:00Z" "2026-01-01T00:00:00Z" >"$tmp/run/windows.tsv"
  printf '%s\ts03-keystone-api-pod-failure\tkeystone-api-pod-failure\t%s\n' "2026-01-01T00:00:00Z" "2026-01-01T00:00:00Z" >>"$tmp/run/windows.tsv"
  from=$((lift + 180))
  to=$((from + 600))
  run_quiet() {
    : >"$tmp/run/checks.jsonl"
    (in_cr "$tmp" "$@"; CR_DIR="$tmp/run"; CHAOS_RANDOM_ENV=ci; cr_check_quiet_tail "$to") >/dev/null 2>&1
    check_in "$tmp/run" quiet-tail
  }
  assert_eq "an empty result counts 0 and passes" \
    "true no failed operation between 2026-01-01T00:03:00Z and the churn end 2026-01-01T00:13:00Z" "$(run_quiet)"
  assert_contains "the queries are instant queries of the sum at both times" "$(cat "$CALL_LOG")" \
    "get --raw /api/v1/namespaces/dizzy/services/dizzy-victoria-metrics-server:8428/proxy/api/v1/query?query=sum%28dizzy_operation_errors_total%29&time=${from}"
  vm_value "$from" 3 >"$tmp/vm.$from"
  vm_value "$to" 5 >"$tmp/vm.$to"
  assert_eq "a result rising from 3 to 5 fails with 2 errors" \
    "false dizzy counted 2 failed operation(s) between 2026-01-01T00:03:00Z (the last lift 2026-01-01T00:00:00Z plus its recovery) and the churn end 2026-01-01T00:13:00Z" \
    "$(run_quiet)"
  vm_value "$to" 3 >"$tmp/vm.$to"
  assert_eq "an unchanged count passes" "true" "$(run_quiet | cut -d' ' -f1)"
  echo '{"status":"error","errorType":"bad_data","error":"cannot parse query"}' >"$tmp/vm.$to"
  assert_eq '"status":"error" fails with the message' \
    "false the query at 2026-01-01T00:13:00Z answered status error: cannot parse query" "$(run_quiet)"
  assert_eq "a failed kubectl get --raw fails with its message" \
    "false the query at 2026-01-01T00:03:00Z failed: Error from server (ServiceUnavailable): the server is currently unable to handle the request" \
    "$(run_quiet VM_RC=1)"
  printf '%s\ts01-mariadb-pod-kill\tmariadb-pod-kill\t-\n' "2026-01-01T00:00:00Z" >"$tmp/run/windows.tsv"
  assert_eq "a run without a lift fails" "false no fault was lifted, so there is no tail to judge" "$(run_quiet)"
}

# ---------------------------------------------------------------------------
# Test 7: stuck-resources and stuck-objects
# ---------------------------------------------------------------------------
test_stuck_checks() {
  echo "Test: stuck-resources and stuck-objects find what a run left behind"

  local tmp="$TMP_DIR/stuck" script
  new_case "$tmp"
  mkdir -p "$tmp/run" "$tmp/os/bin"
  script="$( (in_cr "$tmp" CHAOS_RANDOM_ENV=ci; cr_stuck_job) | jq -r '.spec.template.spec.containers[0].command[2]')"
  assert_eq "the Job runs on the tempest image with no token, the K-ORC Secret and a 300 s deadline" \
    "ghcr.io/c5c3/tempest:2026.1 false k-orc-clouds-yaml 300 admin true" \
    "$( (in_cr "$tmp" CHAOS_RANDOM_ENV=ci; cr_stuck_job) | jq -r '.spec as $s | $s.template.spec | "\(.containers[0].image) \(.automountServiceAccountToken) \(.volumes[0].secret.secretName) \($s.activeDeadlineSeconds) \(.containers[0].env[0].value) \(.securityContext.runAsNonRoot)"')"
  # The listing, against an openstack CLI that answers from OS_FIXTURE.
  cat >"$tmp/os/bin/openstack" <<'STUB'
#!/bin/bash
case "$1 $2" in
  "server list") cat "$OS_FIXTURE/servers.json" ;;
  "volume list") cat "$OS_FIXTURE/volumes.json" ;;
  "image list") cat "$OS_FIXTURE/images.json" ;;
esac
STUB
  chmod +x "$tmp/os/bin/openstack"
  echo '[{"ID": "s1", "Name": "ci-random-a", "Status": "ACTIVE"}, {"ID": "s2", "Name": "ci-random-b", "Status": "BUILD"}]' >"$tmp/os/servers.json"
  echo '[{"ID": "v1", "Name": "", "Status": "available"}, {"ID": "v2", "Name": "data", "Status": "error_deleting"}, {"ID": "v3", "Name": "root", "Status": "in-use"}]' >"$tmp/os/volumes.json"
  echo '[{"ID": "i1", "Name": "chaos-random", "Status": "active"}]' >"$tmp/os/images.json"
  OS_FIXTURE="$tmp/os" PATH="$tmp/os/bin:$PATH" bash -c "$script" >"$tmp/stuck.log" 2>&1
  : >"$tmp/run/checks.jsonl"
  (in_cr "$tmp" CHAOS_RANDOM_ENV=ci; CR_DIR="$tmp/run"; cr_check_stuck_resources) >/dev/null 2>&1
  assert_eq "a server in BUILD and a volume in error_deleting fail stuck-resources" \
    "false server ci-random-b s2 BUILD; volume data v2 error_deleting" "$(check_in "$tmp/run" stuck-resources)"
  echo '[{"ID": "s1", "Name": "ci-random-a", "Status": "ACTIVE"}]' >"$tmp/os/servers.json"
  echo '[{"ID": "v1", "Name": "", "Status": "available"}]' >"$tmp/os/volumes.json"
  OS_FIXTURE="$tmp/os" PATH="$tmp/os/bin:$PATH" bash -c "$script" >"$tmp/stuck.log" 2>&1
  : >"$tmp/run/checks.jsonl"
  (in_cr "$tmp" CHAOS_RANDOM_ENV=ci; CR_DIR="$tmp/run"; cr_check_stuck_resources) >/dev/null 2>&1
  assert_eq "ACTIVE and available pass" "true 1 servers, 1 volumes, 1 images, each in a steady state" \
    "$(check_in "$tmp/run" stuck-resources)"
  assert_eq "the Job was created from the manifest" "chaos-random-stuck" "$(jq -r '.metadata.name' "$tmp/created.json")"
  printf 'LISTED server 1\nERROR: openstack volume list --all-projects failed: Unable to establish connection to http://controlplane-cinder:8776\n' >"$tmp/stuck.log"
  : >"$tmp/run/checks.jsonl"
  (in_cr "$tmp" CHAOS_RANDOM_ENV=ci KUBECTL_STUCK_STATE=Failed; CR_DIR="$tmp/run"; cr_check_stuck_resources) >/dev/null 2>&1
  assert_eq "a failed Job fails the check with its last log line" \
    "false Job chaos-random-stuck failed: ERROR: openstack volume list --all-projects failed: Unable to establish connection to http://controlplane-cinder:8776" \
    "$(check_in "$tmp/run" stuck-resources)"

  # stuck-objects.
  : >"$tmp/run/checks.jsonl"
  (in_cr "$tmp" CHAOS_RANDOM_ENV=ci; CR_DIR="$tmp/run"; cr_check_stuck_objects) >/dev/null 2>&1
  assert_eq "a clean plane passes stuck-objects" "true" "$(check_in "$tmp/run" stuck-objects | cut -d' ' -f1)"
  echo "workflownode.chaos-mesh.org/s03-mariadb-pod-kill-x7k2p" >"$tmp/left.txt"
  : >"$tmp/run/checks.jsonl"
  (in_cr "$tmp" CHAOS_RANDOM_ENV=ci; CR_DIR="$tmp/run"; cr_check_stuck_objects) >/dev/null 2>&1
  assert_eq "a left WorkflowNode fails stuck-objects" \
    "false left in openstack: workflownode.chaos-mesh.org/s03-mariadb-pod-kill-x7k2p" "$(check_in "$tmp/run" stuck-objects)"
  : >"$tmp/left.txt"
  {
    echo '{"items":['
    pod_json openstack nova-api-0 '{}' Running 2026-01-01T00:00:00Z
    echo ','
    pod_json kube-system coredns-0 '{}' Running 2026-01-01T00:00:00Z
    echo ']}'
  } >"$tmp/pods.json"
  : >"$tmp/run/checks.jsonl"
  (in_cr "$tmp" CHAOS_RANDOM_ENV=ci; CR_DIR="$tmp/run"; cr_check_stuck_objects) >/dev/null 2>&1
  assert_eq "a pod of a sampled namespace terminating for more than 300 s fails it, kube-system is not sampled" \
    "false openstack/nova-api-0 terminating since 2026-01-01T00:00:00Z" "$(check_in "$tmp/run" stuck-objects)"
}

# ---------------------------------------------------------------------------
# Test 8: the soak's tables judged again, and the JUnit file
# ---------------------------------------------------------------------------
test_rejudge_and_junit() {
  echo "Test: the targets are exempt from no-restarts and memory-growth, and the JUnit file names each check"

  local tmp="$TMP_DIR/rejudge" i ts p
  new_case "$tmp"
  mkdir -p "$tmp/run/soak/platform"
  p="$tmp/run/soak/platform"
  : >"$p/usage.tsv"
  : >"$p/containers.tsv"
  : >"$p/pod-samples.log"
  : >"$p/errors.log"
  for i in $(seq 1 25); do
    ts="$(printf '2026-01-02T00:%02d:00Z' "$i")"
    echo "$ts" >>"$p/pod-samples.log"
    # keystone-0 is a target and restarts at sample 10; nova-0 restarts at 20.
    printf '%s\topenstack\tkeystone-0\tuid-k\t2026-01-01T00:00:00Z\tStatefulSet\tkeystone\t%s\t-\t-\n' "$ts" "$((i >= 10 ? 1 : 0))" >>"$p/containers.tsv"
    printf '%s\topenstack\tnova-0\tuid-n\t2026-01-01T00:00:00Z\tReplicaSet\tnova-api\t%s\t-\t-\n' "$ts" "$((i >= 20 ? 1 : 0))" >>"$p/containers.tsv"
    printf '%s\topenstack\tkeystone-0\tkeystone\t1\t104857600\n' "$ts" >>"$p/usage.tsv"
    printf '%s\topenstack\tnova-0\tnova-api\t1\t104857600\n' "$ts" >>"$p/usage.tsv"
    # source-controller grows by 8 MiB a sample.
    printf '%s\tflux-system\tsource-0\tuid-f\t2026-01-01T00:00:00Z\tReplicaSet\tmanager\t0\t-\t-\n' "$ts" >>"$p/containers.tsv"
    printf '%s\tflux-system\tsource-0\tmanager\t1\t%s\n' "$ts" "$(((50 + 8 * i) * 1048576))" >>"$p/usage.tsv"
  done
  echo '{"startTime": "2026-01-02T00:00:00Z", "maxMemoryGrowthPercent": 10, "maxMemoryGrowthMiB": 16, "namespaces": ["openstack"]}' \
    >"$tmp/run/soak/meta.json"
  # shellcheck disable=SC2034 # read by the sourced runner
  ( source "$PROJECT_ROOT/hack/dizzy-soak-runner.sh"; cd "$tmp/run/soak" && RUN_START=2026-01-02T00:00:00Z && derive_restarts )
  printf 's01-keystone-api-pod-failure\topenstack/keystone-0\n' >"$tmp/run/targets.tsv"
  printf '2026-01-02T00:18:30Z\ts02-nova-rabbitmq-partition\tnova-rabbitmq-partition\t2026-01-02T00:20:30Z\n' >"$tmp/run/windows.tsv"
  printf '2026-01-02T00:05:00Z\ts01-keystone-api-pod-failure\tkeystone-api-pod-failure\t2026-01-02T00:07:00Z\n' >>"$tmp/run/windows.tsv"
  : >"$tmp/run/checks.jsonl"
  (in_cr "$tmp" CHAOS_RANDOM_ENV=ci; CR_DIR="$tmp/run"; cr_rejudge) >/dev/null 2>&1
  assert_eq "a restarted other container fails no-restarts, named with the windows open at its first sample after the restart; flux-system counts there" \
    "false 1 restart(s) and 0 OOM kill(s) in 2 containers observed; 1 exempt pod(s) skipped; openstack/nova-0/nova-api by 2026-01-02T00:20:00Z (open: s02-nova-rabbitmq-partition)" \
    "$(check_in "$tmp/run" no-restarts)"
  assert_eq "memory-growth skips the target and flux-system and judges no restarted container" \
    "false no container could be judged; the reasons: restarted (1), exempt (1), namespace skipped (1)" \
    "$(check_in "$tmp/run" memory-growth)"
  assert_eq "the soak's own tables stay as the soak wrote them" "no" \
    "$([[ -e "$tmp/run/soak/platform/memory-growth.tsv" ]] && echo yes || echo no)"
  assert_eq "the judged table is the run's memory-growth.tsv" "exempt|namespace skipped|restarted" \
    "$(awk -F '\t' 'NR > 1 { print $9 }' "$tmp/run/memory-growth.tsv" | sort | paste -sd '|' -)"
  jq '.memoryGrowthSkipNamespaces = []' "$tmp/run/soak/meta.json" >"$tmp/meta.json"
  mv "$tmp/meta.json" "$tmp/run/soak/meta.json"
  : >"$tmp/run/checks.jsonl"
  (in_cr "$tmp" CHAOS_RANDOM_ENV=ci; CR_DIR="$tmp/run"; cr_rejudge) >/dev/null 2>&1
  assert_eq "a soak that skipped no namespace has flux-system judged again too" \
    "false flux-system/source-0/manager: 150.0 to 246.0 MiB (+96.0 MiB, 64.0 %); 1 container(s) of exempt pods skipped" \
    "$(check_in "$tmp/run" memory-growth)"

  # The outputs.
  echo '{"seed": 1234, "steps": [{"index": 1, "offset": 0, "entries": ["mariadb-pod-kill"], "length": 30, "pause": 100}]}' >"$tmp/run/plan.json"
  : >"$tmp/run/checks.jsonl"
  local verdict
  verdict="$( (in_cr "$tmp" CHAOS_RANDOM_ENV=ci CHAOS_RANDOM_WINDOW=30m; CR_DIR="$tmp/run"; CR_RUN=20260102T000000Z
    cr_check workflow true "1 chaos templates injected and lifted; the Workflow was Accomplished" >/dev/null
    cr_check quiet-tail false "dizzy counted 2 failed operation(s) between a & b <c>" >/dev/null
    cr_write_outputs) 2>/dev/null)"
  assert_eq "a failed check makes the verdict FAIL" "FAIL" "$verdict"
  assert_eq "random-verdict.json names the run, the seed and the checks" "FAIL 20260102T000000Z 1234 workflow,quiet-tail" \
    "$(jq -r '"\(.verdict) \(.run) \(.seed) \([.checks[].name] | join(","))"' "$tmp/run/random-verdict.json")"
  assert_eq "hack/ci-junit-failed-suites.py reads the failed check with the seed" \
    "random-quiet-tail	dizzy counted 2 failed operation(s) between a & b <c> (seed 1234)" \
    "$(python3 -I "$PROJECT_ROOT/hack/ci-junit-failed-suites.py" "$tmp/reports/chaos-random-junit.xml")"
  assert_eq "the JUnit file holds one testsuite per check" "2" "$(grep -c '<testsuite name="tests/chaos-random/random-' "$tmp/reports/chaos-random-junit.xml")"
  assert_contains "report.md names the replay" "$(cat "$tmp/run/report.md")" \
    'CHAOS_RANDOM_ENV=ci CHAOS_RANDOM_SEED=1234 CHAOS_RANDOM_WINDOW=30m make chaos-random-draw'
  assert_contains "and the plan" "$(cat "$tmp/run/report.md")" "| 1 | 0 | mariadb-pod-kill | 30 | 100 |"
}

# ---------------------------------------------------------------------------
# Test 9: run and up
# ---------------------------------------------------------------------------

# fake_clock — cr_now from a counter file that every call moves on by
# CLOCK_STEP seconds, and no sleep.
fake_clock() {
  cr_now() {
    local t
    t="$(cat "$CLOCK_FILE")"
    echo $((t + CLOCK_STEP)) >"$CLOCK_FILE"
    echo "$t"
  }
  cr_sleep() { :; }
}

# skip_late_checks — The checks run does not reach in these tests.
skip_late_checks() {
  cr_preflight() { echo "OK: preflight stubbed"; }
  cr_rejudge() { :; }
  cr_check_quiet_tail() { :; }
  cr_check_consistency() { :; }
  cr_check_stuck_resources() { :; }
  cr_check_stuck_objects() { :; }
}

test_run_and_up() {
  echo "Test: run stops the soak on a refused Workflow and on a soak that outlives the churn, and up names its stage"

  local tmp="$TMP_DIR/run" rc out dir
  new_case "$tmp"
  echo '{"verdict": "PASS", "checks": [{"name": "dizzy-exit", "pass": true, "detail": "dizzy exited 0"}]}' >"$tmp/soak-fixture/verdict.json"
  rc=0
  out="$( (in_cr "$tmp" CHAOS_RANDOM_ENV=ci CHAOS_RANDOM_SEED=7 CHAOS_RANDOM_WARMUP=0s CHAOS_RANDOM_TAIL=0s KUBECTL_APPLY_FAIL=workflow.json
    skip_late_checks; CR_GONE=0; cr_cmd_run) 2>&1)" || rc=$?
  dir="$(find "$tmp/out" -mindepth 1 -maxdepth 1 -type d | head -n 1)"
  assert_eq "a refused Workflow apply exits 1" "1" "$rc"
  assert_eq "workflow fails with kubectl's message" \
    "false kubectl apply refused the Workflow: Error from server (BadRequest): error when creating \"workflow.json\": admission webhook \"vworkflow.kb.io\" denied the request: spec.templates[1].deadline: Invalid value" \
    "$(check_in "$dir" workflow)"
  assert_eq "and the soak is started once and stopped" "soak start soak stop soak report" \
    "$(grep '^soak ' "$CALL_LOG" | paste -sd ' ' -)"
  assert_eq "the soak's own checks still count" "true dizzy exited 0" "$(check_in "$dir" dizzy-exit)"
  assert_contains "the soak ran with the seed and the kind scenario" "$out" "run "
  assert_eq "the run directory holds the plan, the Workflow and the verdict" "plan.json random-verdict.json report.md workflow.json" \
    "$(cd "$dir" && ls plan.json random-verdict.json report.md workflow.json | paste -sd ' ' -)"

  new_case "$tmp"
  echo '{"status":{"conditions":[]}}' >"$tmp/soak-job.json"
  echo 1767225600 >"$tmp/clock"
  rc=0
  out="$( (in_cr "$tmp" CHAOS_RANDOM_ENV=ci CHAOS_RANDOM_SEED=7 CHAOS_RANDOM_WARMUP=0s CHAOS_RANDOM_TAIL=0s CLOCK_FILE="$tmp/clock" CLOCK_STEP=100
    skip_late_checks; fake_clock; CR_GONE=0; cr_cmd_run) 2>&1)" || rc=$?
  dir="$(find "$tmp/out" -mindepth 1 -maxdepth 1 -type d | head -n 1)"
  assert_eq "a soak Job still running 600 s after the churn end fails the run" "1" "$rc"
  assert_eq "dizzy-exit fails with the bound" "false the soak Job did not finish within 600 s of the churn end" \
    "$(check_in "$dir" dizzy-exit)"
  assert_eq "the soak is stopped" "soak start soak stop soak report" "$(grep '^soak ' "$CALL_LOG" | paste -sd ' ' -)"
  assert_contains "the Workflow was applied and deleted" "$(cat "$CALL_LOG")" "delete workflows.chaos-mesh.org chaos-random-"

  new_case "$tmp"
  rc=0
  out="$( (in_cr "$tmp" CHAOS_RANDOM_ENV=ci KUBECTL_WAIT_FAIL=controlplane/controlplane; cr_cmd_up) 2>&1)" || rc=$?
  assert_eq "up exits 1 when the ControlPlane's wait fails" "1" "$rc"
  assert_contains "naming the stage and its bound" "$out" "up: controlplane not ready within 1800 s"
  assert_contains "and the conditions of the named object" "$out" \
    "ControlPlane/controlplane:
  Ready=False DBCredentialsNotReady: waiting"
  assert_not_contains "and applies nothing after it" "$(cat "$CALL_LOG")" "ovnchassis.yaml"
  assert_contains "after the central, the vhost and the node's chassis label" "$(cat "$CALL_LOG")" \
    "label node kind-control-plane openstack.c5c3.io/chassis=true --overwrite"
  # The plane's DB credentials exist only once its database-engine tenant is
  # onboarded: the job's deploy-infra leaves that to up, and a run without it
  # waited 1800 s on a ControlPlane that could not turn Ready.
  assert_eq "up onboards the tenant between the MariaDB and the ControlPlane's Ready" \
    "apply controlplane.yaml|wait mariadb --for=create|wait mariadb --for=condition=Ready|db-tenant openstack controlplane BAO_TOKEN=s.root|wait controlplane/controlplane" \
    "$(awk '/^apply -f .*controlplane\.yaml$/ { print "apply controlplane.yaml" }
      /^wait mariadb\/openstack-db / { print "wait mariadb " $5 }
      /^db-tenant / { print }
      /^wait controlplane\/controlplane / { print "wait controlplane/controlplane" }' "$CALL_LOG" | paste -sd '|' -)"

  new_case "$tmp"
  rc=0
  out="$( (in_cr "$tmp" CHAOS_RANDOM_ENV=ci KUBECTL_WAIT_FAIL=mariadb/openstack-db; cr_cmd_up) 2>&1)" || rc=$?
  assert_eq "up exits 1 when the MariaDB does not get Ready" "1" "$rc"
  assert_contains "naming the stage and its bound" "$out" "up: mariadb not ready within 600 s"
  assert_not_contains "and onboards nothing" "$(cat "$CALL_LOG")" "db-tenant"

  new_case "$tmp"
  rc=0
  out="$( (in_cr "$tmp" CHAOS_RANDOM_ENV=ci KUBECTL_INIT_OUTPUT='{"keys":[]}'; cr_cmd_up) 2>&1)" || rc=$?
  assert_eq "up exits 1 on an init-output without a root token" "1" "$rc"
  assert_contains "naming the Secret" "$out" "up: Secret shared-services/openbao-init-keys carries no root_token"
  assert_not_contains "and onboards nothing" "$(cat "$CALL_LOG")" "db-tenant"
  assert_not_contains "nor waits for the ControlPlane" "$(cat "$CALL_LOG")" "wait controlplane/controlplane"
  rc=0
  out="$( (in_cr "$tmp" CHAOS_RANDOM_ENV=lab; cr_cmd_up) 2>&1)" || rc=$?
  assert_contains "up refuses lab" "$out" "up brings up the kind plane; it runs with CHAOS_RANDOM_ENV=ci only"

  # Every file of tests/chaos-random/kind/ the script applies or ships is in
  # the git tree: .gitignore ignores the basename controlplane.yaml, so a
  # plain git add once left that fixture out and up failed on the runner.
  local files file untracked=""
  files="$(grep -oE '\$\{CR_KIND_DIR\}/[a-z0-9-]+\.yaml' "$CHAOS_RANDOM_SH" | sed 's|.*/||' | sort -u)"
  assert_eq "up applies six files and run ships one scenario" "7" "$(grep -c . <<<"$files")"
  for file in $files; do
    git -C "$PROJECT_ROOT" ls-files --error-unmatch "tests/chaos-random/kind/${file}" >/dev/null 2>&1 ||
      untracked="${untracked:+$untracked }${file}"
  done
  assert_eq "every fixture the script names is tracked by git" "" "$untracked"
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
test_the_settings
test_the_preflight
test_quiet_tail
test_stuck_checks
test_rejudge_and_junit
test_run_and_up

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
