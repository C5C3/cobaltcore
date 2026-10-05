#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the `### Lab autoscaling` subsection of
# docs/reference/infrastructure/infrastructure-manifests.md (#1223):
#   1. the heading occurs once, after `### Lab ControlPlane`, and the next
#      heading of any level is `### Lab hypervisors`
#   2. one line each starts with `vpa_block V<n> '{"updateMode":` for V1 to V4,
#      with the modes "Off", "Initial", "Recreate" and "Recreate", the floors
#      "50m", "60m", "70m" and "80m" and "maxAllowed":{"cpu":"200m"}; V4's
#      alone holds "minReplicas":1, the other three "minReplicas":null
#   3. `updateMode: InPlaceOrRecreate` occurs once as a whole line
#   4. the hand VPA's target and container policy lines occur once each as
#      whole lines, leading whitespace and a `- ` list marker stripped
#   5. each name comes from its source: the Placement operator's app name,
#      which the block's `sel=` line selects on, and its container, the
#      block's `dep=controlplane-placement`, the shared VPA flow's container
#      policy (`*`, RequestsOnly, cpu and memory), the Minimal profile's CPU
#      request, and the load Job's name and Secret
#   6. the subsection removes what it sets: the block with `vpa_block V5 null`,
#      the HPA block with `"autoscaling":null`, the hand VPA and the load Job
#   7. the helpers run from the page, with `kubectl` stubbed and `sleep` a
#      no-op: `millis` converts 50m, 1 and 0.05 to millicores, and
#      `await_target V1 50` returns 0 on a target of 50m
#   8. empty: `target` prints nothing for a VPA without a recommendation, and
#      `pod` prints nothing for an empty pod list
#   9. absent: `target` prints nothing when the VPA is not found, and
#      `await_target V1 50` then prints its GATE FAILED line and returns 1
#  10. upstream error: `state` prints one line holding the error when every
#      `kubectl` call fails, and `ready=0` for a Deployment that omits
#      readyReplicas
#  11. `await_hpa` passes on a CPU reading, prints its GATE FAILED line on a
#      null one, and runs its fourth argument once before each poll
#  12. the patches parse: `vpa_block` sends V1 to V4's block as the Placement
#      API's verticalAutoscaling and V5's null, the H1 patch sets the Keystone
#      API's autoscaling with maxReplicas 3 and the H6 one null, and
#      `drop_job_pods` deletes the Succeeded pods of the Keystone child's Jobs
#
# Checks 7 to 12 need jq and count as one SKIP each without it. The helpers
# run with the block's own `ns=`, `dep=`, `sel=` and `ks=` lines. Subsections
# run from their heading to the next heading of any level, fenced code
# included. INFRA_MANIFESTS_DOC overrides the page.
#
# Usage: bash tests/unit/docs/infrastructure_manifests_lab_autoscaling_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

DOC="${INFRA_MANIFESTS_DOC:-$PROJECT_ROOT/docs/reference/infrastructure/infrastructure-manifests.md}"

if [[ ! -f "$DOC" ]]; then
  echo "FAIL: $DOC does not exist"
  exit 1
fi

# headings prints "<line>:<text>" for every heading outside fenced code.
headings() {
  awk '
    /^[[:space:]]*(```|~~~)/ { fenced = !fenced; next }
    !fenced && /^#+ / { print NR ":" $0 }
  ' "$DOC"
}

# subsection prints the lines below the heading $1, fenced code included, up
# to the next heading of any level outside fenced code.
subsection() {
  awk -v heading="$1" '
    /^[[:space:]]*(```|~~~)/ { fenced = !fenced }
    !fenced && /^#+ / { if (inside) exit; if ($0 == heading) { inside = 1; next } }
    inside { print }
  ' "$DOC"
}

# count_items <text> <line> prints how many lines of <text> are <line> once
# their leading whitespace and a `- ` list marker are stripped.
count_items() {
  awk -v want="$2" '{ sub(/^[[:space:]]+/, ""); sub(/^- /, "") } $0 == want { n++ } END { print n + 0 }' <<<"$1"
}

# starting_with <text> <prefix> prints the lines of <text> that start with
# <prefix>.
starting_with() {
  awk -v prefix="$2" 'index($0, prefix) == 1' <<<"$1"
}

AUTOSCALING="$(subsection '### Lab autoscaling')"

# helper <name> prints the definition of the shell function <name> from the
# subsection: its first line alone when that line ends in `}`, and otherwise
# every line up to the next one that is `}` alone.
helper() {
  awk -v name="$1" '
    !f && index($0, name "() {") == 1 { f = 1; print; if ($0 ~ /}[[:space:]]*$/) exit; next }
    f { print; if ($0 == "}") exit }
  ' <<<"$AUTOSCALING"
}

HELPER_NAMES=(flat millis pod target state vpa_block await_target hpa_line hpa_state drop_job_pods await_hpa)
HELPERS=""
for name in "${HELPER_NAMES[@]}"; do
  HELPERS+="$(helper "$name")"$'\n'
done

# VARS holds the block's assignments of ns, dep, sel and ks, each a whole line.
VARS="$(awk '/^(ns|dep|sel|ks)=/' <<<"$AUTOSCALING")"

# run <dir> <commands> runs <commands> in bash with the helpers and the VARS
# of the page, `vpa` set to controlplane-placement, `auto` set to <dir> and
# `sleep` as a no-op. `kubectl` is a stub that writes its arguments to
# <dir>/args, one per line, and answers `kubectl <verb> <kind> ...` from
# <dir>/stub: it prints all.err or <kind>.err to stderr and exits 1 when one
# exists, and otherwise prints <kind>.out, or nothing.
run() {
  bash -c '
    eval "$1"
    eval "$2"
    vpa=controlplane-placement auto=$3
    sleep() { :; }
    kubectl() {
      local f
      printf "%s\n" "$@" >"$auto/args"
      for f in "$auto/stub/all" "$auto/stub/$2"; do
        if [[ -f "$f.err" ]]; then cat "$f.err" >&2; return 1; fi
      done
      if [[ -f "$auto/stub/$2.out" ]]; then cat "$auto/stub/$2.out"; fi
    }
    eval "$4"
  ' _ "$HELPERS" "$VARS" "$1" "$2"
}

# patch_of <dir> prints the argument the last `kubectl` call of a run in <dir>
# passed after -p.
patch_of() {
  { grep -A1 -x -- -p "$1/args" 2>/dev/null || true; } | tail -n 1
}

# joined prints the subsection with each line that ends in a backslash joined
# to the next one.
joined() {
  awk '/\\$/ { held = held substr($0, 1, length($0) - 1); next } { print held $0; held = "" }' <<<"$AUTOSCALING"
}

# vpa_case <n> sets mode, floor and replicas to the update mode, the
# minAllowed.cpu and the minReplicas of case V<n>.
vpa_case() {
  case "$1" in
    1) mode=Off floor=50m replicas=null ;;
    2) mode=Initial floor=60m replicas=null ;;
    3) mode=Recreate floor=70m replicas=null ;;
    4) mode=Recreate floor=80m replicas=1 ;;
  esac
}

# stub_dir prints a fresh directory with an empty stub/ below it.
stub_dir() {
  local dir
  dir="$(mktemp -d "$TMP/case.XXXXXX")"
  mkdir "$dir/stub"
  echo "$dir"
}

# need_jq counts one SKIP and fails when jq is not on PATH.
need_jq() {
  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed, the helpers cannot run"
    SKIP=$((SKIP + 1))
    return 1
  fi
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

VPA_50M='{"status":{"recommendation":{"containerRecommendations":[{"containerName":"placement-api","target":{"cpu":"50m"}}]}}}'
ONE_POD='{"items":[{"metadata":{"name":"p-1","uid":"u-1"},"spec":{"containers":[{"resources":{"requests":{"cpu":"15m","memory":"128Mi"}}}]},"status":{"containerStatuses":[{"restartCount":0,"resources":{"requests":{"cpu":"15m"}}}]}}]}'

# hpa_json <averageUtilization> prints an HPA list of one Keystone HPA at one
# replica with that CPU reading.
hpa_json() {
  printf '{"items":[{"metadata":{"name":"controlplane-keystone"},"spec":{"scaleTargetRef":{"name":"controlplane-keystone"}},"status":{"desiredReplicas":1,"currentReplicas":1,"currentMetrics":[{"resource":{"current":{"averageUtilization":%s}}}]}}]}' "$1"
}

# --- Test 1: the subsection's place ---
test_position() {
  echo "Test: '### Lab autoscaling' occurs once, after Lab ControlPlane and before Lab hypervisors"
  local all count line controlplane next
  all="$(headings)"
  count="$(grep -cxE '[0-9]+:### Lab autoscaling' <<<"$all" || true)"
  assert_eq "'### Lab autoscaling' occurs once" "1" "$count"
  line="$({ grep -xE '[0-9]+:### Lab autoscaling' <<<"$all" || true; } | head -n 1 | cut -d: -f1)"
  controlplane="$({ grep -xE '[0-9]+:### Lab ControlPlane' <<<"$all" || true; } | head -n 1 | cut -d: -f1)"
  if [[ -n "$line" && -n "$controlplane" ]] && ((controlplane < line)); then
    echo "  PASS: the subsection sits on line $line, after line $controlplane"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the subsection is missing or out of place (Lab ControlPlane '${controlplane}', Lab autoscaling '${line}')"
    FAIL=$((FAIL + 1))
  fi
  next="$(awk -F: -v line="${line:-0}" '$1 > line { sub(/^[0-9]+:/, ""); print; exit }' <<<"$all")"
  assert_eq "the next heading is '### Lab hypervisors'" "### Lab hypervisors" "$next"
  assert_not_empty "the subsection has a body" "$AUTOSCALING"
}

# --- Test 2: the four ControlPlane blocks ---
test_vpa_blocks() {
  echo "Test: V1 to V4 set Off, Initial, Recreate and Recreate with their floors, and only V4 sets minReplicas 1"
  local n mode floor replicas block
  assert_eq "four lines start with a vpa_block that sets an updateMode" "4" \
    "$(starting_with "$AUTOSCALING" 'vpa_block V' | grep -cF "'{\"updateMode\":" || true)"
  for n in 1 2 3 4; do
    vpa_case "$n"
    block="$(starting_with "$AUTOSCALING" "vpa_block V$n '{\"updateMode\":")"
    assert_eq "one vpa_block line for V$n" "1" "$(grep -c . <<<"$block" || true)"
    assert_contains "V$n sets updateMode $mode" "$block" "\"updateMode\":\"$mode\""
    assert_contains "V$n sets the floor $floor" "$block" "\"minAllowed\":{\"cpu\":\"$floor\"}"
    assert_contains "V$n sets maxAllowed.cpu 200m" "$block" '"maxAllowed":{"cpu":"200m"}'
    assert_contains "V$n sets minReplicas $replicas" "$block" "\"minReplicas\":$replicas"
  done
}

# --- Test 3: the hand VPA's mode ---
test_in_place_mode() {
  echo "Test: the hand VPA sets InPlaceOrRecreate"
  assert_eq "'updateMode: InPlaceOrRecreate' is one whole line" "1" \
    "$(count_items "$AUTOSCALING" 'updateMode: InPlaceOrRecreate')"
}

# --- Test 4: the hand VPA has the shape BuildVPA renders ---
test_hand_vpa_shape() {
  echo "Test: the hand VPA targets the Placement Deployment with the policy BuildVPA renders"
  local item
  for item in \
    'containerName: "*"' \
    'controlledValues: RequestsOnly' \
    'controlledResources: [cpu, memory]' \
    'kind: Deployment' \
    'name: controlplane-placement'; do
    assert_eq "'$item' is one whole line" "1" "$(count_items "$AUTOSCALING" "$item")"
  done
}

# --- Test 5: the names come from their sources ---
# A renamed constant fails here before the block selects no pod.
test_name_sources() {
  echo "Test: each name the block uses is the one its source sets"
  local deployment="$PROJECT_ROOT/operators/placement/internal/controller/reconcile_deployment.go"
  local vpa_flow="$PROJECT_ROOT/internal/common/deployment/vpa_flow.go"
  local loadgen="$PROJECT_ROOT/tests/e2e-autoscaling/02-keystone-loadgen-job.yaml"
  local app
  app="$(sed -n 's/^const placementAppName = "\(.*\)"$/\1/p' "$deployment")"
  assert_not_empty "the Placement operator's app name is read from placementAppName" "$app"
  assert_contains "the block's selector uses the app name" \
    "$(starting_with "$AUTOSCALING" 'sel=')" "app.kubernetes.io/name=${app},"
  assert_eq "the block sets dep=controlplane-placement once" "1" \
    "$(count_items "$AUTOSCALING" 'dep=controlplane-placement')"
  assert_eq "the Placement operator names its container placement-api" 'Name:    "placement-api",' \
    "$(grep -A1 -F 'Container: deployment.ContainerParams{' "$deployment" | tail -n 1 | sed 's/^[[:space:]]*//')"
  assert_file_contains_fixed "BuildVPA renders RequestsOnly" \
    "$vpa_flow" 'ContainerControlledValuesRequestsOnly'
  assert_file_contains "BuildVPA renders the default container policy" \
    "$vpa_flow" 'ContainerName:[[:space:]]*vpav1\.DefaultContainerResourcePolicy,'
  assert_file_contains_fixed "BuildVPA controls cpu and memory" \
    "$vpa_flow" '[]corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory}'
  assert_file_contains_fixed "the Minimal profile requests 15m of CPU" \
    "$PROJECT_ROOT/operators/c5c3/api/v1alpha1/sizing_profiles.go" 'minimalServiceCPURequest = "15m"'
  assert_file_contains_fixed "the load Job is autoscaling-keystone-loadgen" \
    "$loadgen" 'name: autoscaling-keystone-loadgen'
  assert_file_contains_fixed "the load Job mounts k-orc-clouds-yaml" \
    "$loadgen" 'secretName: k-orc-clouds-yaml'
}

# --- Test 6: what the block sets, it removes ---
test_removes_what_it_sets() {
  echo "Test: the block removes the VPA block, the hand VPA, the HPA block and the load Job"
  assert_not_empty "a line starts with 'vpa_block V5 null'" \
    "$(starting_with "$AUTOSCALING" 'vpa_block V5 null')"
  assert_not_empty "a ControlPlane patch sets '\"autoscaling\":null'" \
    "$(grep -F 'kubectl patch controlplane' <<<"$AUTOSCALING" | grep -F '"autoscaling":null' || true)"
  assert_contains "the hand VPA is deleted" "$AUTOSCALING" 'kubectl delete vpa lab-placement-inplace -n openstack'
  assert_contains "the load Job is deleted" "$AUTOSCALING" 'kubectl delete job autoscaling-keystone-loadgen -n openstack'
}

# --- Test 7: millis and a target at the floor ---
test_millis_and_target() {
  echo "Test: millis converts to millicores, and await_target passes on a target at the floor"
  need_jq || return 0
  local name dir out
  for name in "${HELPER_NAMES[@]}"; do
    assert_not_empty "the subsection defines $name" "$(helper "$name")"
  done
  dir="$(stub_dir)"
  assert_eq "millis prints 50, 1000 and 50 for 50m, 1 and 0.05" $'50\n1000\n50' \
    "$(run "$dir" 'millis <<<50m; millis <<<1; millis <<<0.05')"
  printf '%s' "$VPA_50M" >"$dir/stub/vpa.out"
  out="$(run "$dir" 'await_target V1 50; echo "rc=$?"' 2>&1)"
  assert_contains "await_target V1 50 names the target" "$out" 'V1 target 50m at '
  assert_contains "await_target V1 50 returns 0 on 50m" "$out" 'rc=0'
  assert_contains "await_target V1 50 writes V1.log" "$(cat "$dir/V1.log" 2>/dev/null || true)" 'V1 target 50m at '
}

# --- Test 8: empty answers ---
test_empty() {
  echo "Test: target and pod print nothing for an empty answer"
  need_jq || return 0
  local dir
  dir="$(stub_dir)"
  printf '%s' '{"status":{}}' >"$dir/stub/vpa.out"
  printf '%s' '{"items":[]}' >"$dir/stub/pod.out"
  assert_eq "target prints nothing for a VPA without a recommendation" "" \
    "$(run "$dir" 'target controlplane-placement' 2>&1)"
  assert_eq "pod prints nothing for an empty pod list" "" "$(run "$dir" 'pod' 2>&1)"
}

# --- Test 9: an absent VPA ---
test_absent() {
  echo "Test: target prints nothing for an absent VPA, and await_target fails its gate"
  need_jq || return 0
  local dir out
  dir="$(stub_dir)"
  echo 'Error from server (NotFound): verticalpodautoscalers.autoscaling.k8s.io "controlplane-placement" not found' \
    >"$dir/stub/vpa.err"
  assert_eq "target prints nothing for an absent VPA" "" "$(run "$dir" 'target controlplane-placement' 2>&1)"
  out="$(run "$dir" 'await_target V1 50; echo "rc=$?"' 2>&1)"
  assert_contains "await_target V1 50 prints its GATE FAILED line" "$out" \
    'GATE FAILED: V1 no target of 50m within 600 s'
  assert_contains "await_target V1 50 returns 1" "$out" 'rc=1'
}

# --- Test 10: state on a failing API server and at zero ready replicas ---
test_state() {
  echo "Test: state prints one line on a failing API server, and ready=0 for an omitted readyReplicas"
  need_jq || return 0
  local dir out
  dir="$(stub_dir)"
  echo 'Unable to connect to the server: dial tcp 192.0.2.1:443: i/o timeout' >"$dir/stub/all.err"
  out="$(run "$dir" 'state' 2>&1)"
  assert_eq "state prints one line" "1" "$(grep -c . <<<"$out" || true)"
  assert_contains "the line holds the error" "$out" 'Unable to connect to the server'
  dir="$(stub_dir)"
  printf '%s' "$ONE_POD" >"$dir/stub/pod.out"
  printf '%s' "$VPA_50M" >"$dir/stub/vpa.out"
  printf '%s' 'VPAReady' >"$dir/stub/placement.out"
  out="$(run "$dir" 'state' 2>&1)"
  assert_contains "state prints the pod's fields" "$out" 'pod=[p-1 u-1 0 15m 15m 128Mi]'
  assert_contains "state prints ready=0 for an omitted readyReplicas" "$out" ' ready=0 '
  assert_contains "state prints the target and the reason" "$out" 'target=50m vpaready=VPAReady'
}

# --- Test 11: await_hpa ---
test_await_hpa() {
  echo "Test: await_hpa passes on a CPU reading, fails on none, and runs its command before each poll"
  need_jq || return 0
  local dir out
  dir="$(stub_dir)"
  hpa_json 42 >"$dir/stub/hpa.out"
  printf '%s' '1' >"$dir/stub/deployment.out"
  out="$(run "$dir" "await_hpa metric 300 'cpu=[0-9]+ '; echo \"rc=\$?\"" 2>&1)"
  assert_contains "await_hpa metric passes on cpu=42" "$out" 'metric at '
  assert_contains "await_hpa metric returns 0" "$out" 'rc=0'
  dir="$(stub_dir)"
  hpa_json null >"$dir/stub/hpa.out"
  printf '%s' '1' >"$dir/stub/deployment.out"
  out="$(run "$dir" "count() { echo x >>\"\$auto/count\"; }; await_hpa metric 300 'cpu=[0-9]+ ' count; echo \"rc=\$?\"" 2>&1)"
  assert_contains "await_hpa metric prints its GATE FAILED line on a null reading" "$out" \
    'GATE FAILED: metric not within 300 s'
  assert_contains "await_hpa metric returns 1" "$out" 'rc=1'
  assert_eq "the fourth argument runs once before each of the 30 polls" "30" \
    "$(grep -c . "$dir/count" 2>/dev/null || true)"
}

# --- Test 12: the patches the block sends ---
test_patches() {
  echo "Test: the ControlPlane patches parse and set their case's fields, and drop_job_pods deletes the Succeeded Job pods"
  need_jq || return 0
  local n mode floor replicas dir commands patch
  for n in 1 2 3 4; do
    vpa_case "$n"
    dir="$(stub_dir)"
    run "$dir" "$(starting_with "$AUTOSCALING" "vpa_block V$n '{\"updateMode\":")" >/dev/null 2>&1 || true
    assert_eq "V$n patches the Placement API's verticalAutoscaling" \
      "{\"maxAllowed\":{\"cpu\":\"200m\"},\"minAllowed\":{\"cpu\":\"$floor\"},\"minReplicas\":$replicas,\"updateMode\":\"$mode\"}" \
      "$(jq -cS '.spec.sizing.placement.api.verticalAutoscaling' <<<"$(patch_of "$dir")" 2>&1 || true)"
  done
  dir="$(stub_dir)"
  run "$dir" "$(starting_with "$AUTOSCALING" 'vpa_block V5 null')" >/dev/null 2>&1 || true
  assert_eq "V5 patches the Placement API's verticalAutoscaling to null" "true" \
    "$(jq '.spec.sizing.placement.api | has("verticalAutoscaling") and .verticalAutoscaling == null' <<<"$(patch_of "$dir")" 2>&1 || true)"

  commands="$(joined | grep -F 'kubectl patch controlplane' || true)"
  patch="$(grep -F '"autoscaling":{' <<<"$commands" || true)"
  assert_eq "one ControlPlane patch sets an autoscaling block" "1" "$(grep -c . <<<"$patch" || true)"
  dir="$(stub_dir)"
  run "$dir" "$patch" >/dev/null 2>&1 || true
  assert_eq "H1 patches the Keystone API's autoscaling up to three replicas" "true" \
    "$(jq '.spec.sizing.keystone.api.autoscaling.maxReplicas == 3' <<<"$(patch_of "$dir")" 2>&1 || true)"
  patch="$(grep -F '"autoscaling":null' <<<"$commands" || true)"
  assert_eq "one ControlPlane patch removes the autoscaling block" "1" "$(grep -c . <<<"$patch" || true)"
  dir="$(stub_dir)"
  run "$dir" "$patch" >/dev/null 2>&1 || true
  assert_eq "H6 patches the Keystone API's autoscaling to null" "true" \
    "$(jq '.spec.sizing.keystone.api | has("autoscaling") and .autoscaling == null' <<<"$(patch_of "$dir")" 2>&1 || true)"

  dir="$(stub_dir)"
  run "$dir" 'drop_job_pods' >/dev/null 2>&1 || true
  assert_eq "drop_job_pods deletes the Succeeded pods of the Keystone child's Jobs" \
    'delete pods -n openstack --ignore-not-found --field-selector=status.phase==Succeeded -l app.kubernetes.io/name=keystone,app.kubernetes.io/instance=controlplane-keystone,job-name' \
    "$(paste -sd ' ' "$dir/args" 2>/dev/null || true)"
}

test_position
test_vpa_blocks
test_in_place_mode
test_hand_vpa_shape
test_name_sources
test_removes_what_it_sets
test_millis_and_target
test_empty
test_absent
test_state
test_await_hpa
test_patches

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
