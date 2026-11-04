#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the lab chaos subsections of
# docs/reference/infrastructure/infrastructure-manifests.md: `#### Proving run`
# and `#### Lab fault runs` (#1222, #1246), whose paste-a-section blocks
# hack/lab-chaos.sh replaced, and `#### Lab chaos runner` (#1395):
#   1. `#### Proving run`, `#### Lab fault runs`, `#### Lab chaos runner` and
#      `#### Lab failover runs` occur once each, in that order, before
#      `### Lab ControlPlane`
#   2. neither `#### Proving run` nor `#### Lab fault runs` holds a
#      `kind: <X>Chaos` line or a bash block any more, and both point at the
#      runner
#   3. both keep the record of their 2026-10-04 run, a line that starts with
#      `The run of 2026-10-04`, and their tables: the Proving run's six step
#      rows and the fault table's rows F1 to F5
#   4. the table of the `#### Proving run` judges steps 2 and 4 against the
#      run's baseline: row 2 calls its times the run's baseline, row 4 names
#      both distances to the largest time of step 2, and neither row names a
#      fixed 0.3 s; the paragraph below the table names row 4's distances and
#      the latency of the runner's NetworkChaos, ties the distances to that
#      delay, names the spread of step 2 as what shrinks the room under the
#      delay, and adds the distances to the recorded 0.306 s
#   5. the `### Lab NFS stack` subsection links the lab fault runs and does not
#      say that no run is recorded
#   6. `#### Lab chaos runner` names every make target lab-chaos-<sub> of the
#      Makefile, lists in its settings table exactly the LAB_CHAOS_* variables
#      the script reads, names the four steps of the failed-gate procedure and
#      the virsh console hand step, and points at the unit test
#   7. the runner's subsection is in exactly one mode: before the session it
#      holds the line `No lab run of the chaos runner is recorded yet.`, after
#      it a record that starts with `The run of 20YY-MM-DD` with the proving
#      table, the fault table with F1 to F5 and the random run's row
#      (record.md of `make lab-chaos-record`), never both
#   8. the selectors of F1 to F4 in hack/lab-chaos.sh carry the labels their
#      sources set: the libvirt DaemonSet, the NFS server manifest, the
#      constants of the nova and ovn operators, and the names of the lab's
#      NovaCompute and OVNChassis
#
# The helpers recovered and probes_ok moved into the runner with the block;
# tests/unit/hack/lab_chaos_test.sh pins them.
#
# Subsections run from their heading to the next heading of any level, fenced
# code included. INFRA_MANIFESTS_DOC overrides the page.
#
# Usage: bash tests/unit/docs/infrastructure_manifests_lab_fault_runs_test.sh

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

# count_lines <text> <line> prints how many lines of <text> are <line> once
# their leading whitespace is stripped.
count_lines() {
  awk -v want="$2" '{ sub(/^[[:space:]]+/, "") } $0 == want { n++ } END { print n + 0 }' <<<"$1"
}

LAB_CHAOS_SH="$PROJECT_ROOT/hack/lab-chaos.sh"
PROVING="$(subsection '#### Proving run')"
FAULTS="$(subsection '#### Lab fault runs')"
RUNNER="$(subsection '#### Lab chaos runner')"

# --- Test 1: the subsections' places ---
test_position() {
  echo "Test: the four lab chaos subsections occur once each, in order, before Lab ControlPlane"
  local all heading line previous=0 controlplane
  all="$(headings)"
  for heading in '#### Proving run' '#### Lab fault runs' '#### Lab chaos runner' '#### Lab failover runs'; do
    assert_eq "'$heading' occurs once" "1" "$(grep -cxF -- "$heading" <<<"$(cut -d: -f2- <<<"$all")" || true)"
    line="$(grep -nxF -- "$heading" <<<"$(cut -d: -f2- <<<"$all")" | head -n 1 | cut -d: -f1)"
    line="$(sed -n "${line:-0}p" <<<"$all" | cut -d: -f1)"
    if [[ -n "$line" && "$line" -gt "$previous" ]]; then
      echo "  PASS: '$heading' on line $line follows line $previous"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: '$heading' is missing or out of order (line '${line}', previous ${previous})"
      FAIL=$((FAIL + 1))
    fi
    previous="${line:-$previous}"
  done
  controlplane="$({ grep -xE '[0-9]+:### Lab ControlPlane' <<<"$all" || true; } | head -n 1 | cut -d: -f1)"
  assert_gte "Lab ControlPlane comes after them" "${controlplane:-0}" "$((previous + 1))"
}

# --- Test 2: the blocks are gone ---
test_blocks_replaced() {
  echo "Test: the Proving run and the Lab fault runs hold no chaos object and no bash block, and point at the runner"
  local name text
  for name in PROVING FAULTS; do
    text="${!name}"
    assert_not_empty "$name has a body" "$text"
    assert_eq "$name holds no kind: <X>Chaos line" "" "$(grep -E 'kind: [A-Za-z]*Chaos' <<<"$text" || true)"
    assert_eq "$name holds no bash block" "" "$(grep -E '^[[:space:]]*```bash' <<<"$text" || true)"
    assert_contains "$name points at the runner" "$text" "(#lab-chaos-runner)"
  done
  assert_contains "the Proving run names the make target of steps 1 to 5" "$PROVING" '`make lab-chaos-proving`'
  assert_contains "and that of step 6" "$PROVING" '`make lab-chaos-teardown-check`'
  assert_contains "the Lab fault runs name the make target of F1 to F5" "$FAULTS" '`make lab-chaos-faults`'
  assert_not_contains "no allowHostNetworkTesting" "$FAULTS" 'allowHostNetworkTesting'
}

# --- Test 3: the 2026-10-04 records and their tables stay ---
test_records_kept() {
  echo "Test: both subsections keep the record of their 2026-10-04 run and their tables"
  local step fault
  assert_not_empty "the Proving run keeps its record" "$(grep -E '^The run of 2026-10-04 ' <<<"$PROVING" || true)"
  assert_not_empty "the Lab fault runs keep theirs" "$(grep -E '^The run of 2026-10-04 ' <<<"$FAULTS" || true)"
  for step in 1 2 3 4 5 6; do
    assert_not_empty "the Proving run's table has a row for step $step" "$(grep -E "^\| ${step} \|" <<<"$PROVING" || true)"
  done
  for fault in F1 F2 F3 F4 F5; do
    assert_not_empty "the fault table has a row for $fault" "$(grep -E "^\| ${fault} " <<<"$FAULTS" || true)"
  done
}

# --- Test 4: the Proving run judges the delay against the run's baseline ---
test_proving_run_baseline_rows() {
  echo "Test: the Proving run's rows for steps 2 and 4 compare with the run's baseline"
  local row2 row4
  row2="$(grep -E '^\| 2 \|' <<<"$PROVING" || true)"
  row4="$(grep -E '^\| 4 \|' <<<"$PROVING" || true)"
  assert_not_empty "the Proving run's table has a row for step 2" "$row2"
  assert_not_empty "the Proving run's table has a row for step 4" "$row4"
  assert_contains "step 2 is the run's baseline" "$row2" "the run's baseline"
  assert_contains "the delayed times are judged against step 2" "$row4" \
    'each at least 0.2 s above the largest time of step 2'
  assert_contains "the released times are judged against step 2" "$row4" \
    'each less than 0.1 s above the largest time of step 2'
  assert_not_contains "step 2 names no fixed 0.3 s" "$row2" '0.3 s'
  assert_not_contains "step 4 names no fixed 0.3 s" "$row4" '0.3 s'
  # The paragraph below the table restates row 4's distances, so it is read
  # against the distances row 4 names.
  local inj rel para
  inj="$(sed -nE 's/.*each at least ([0-9.]+) s above the largest time of step 2.*/\1/p' <<<"$row4")"
  rel="$(sed -nE 's/.*each less than ([0-9.]+) s above the largest time of step 2.*/\1/p' <<<"$row4")"
  para="$(awk '/^Steps 2 and 4 name no absolute time/ { p = 1 } p && /^$/ { exit } p' <<<"$PROVING" | tr '\n' ' ')"
  assert_not_empty "the Proving run explains the baseline below its table" "$para"
  assert_contains "the paragraph names row 4's injected distance" "$para" "at least ${inj} s above it"
  assert_contains "the paragraph names row 4's released distance" "$para" "less than ${rel} s above it"
  # The delay is the latency of the runner's NetworkChaos, and the runner
  # judges step 4 by the same distances.
  local delay_ms room
  delay_ms="$(sed -nE 's/^[[:space:]]*latency: ([0-9]+)ms$/\1/p' "$LAB_CHAOS_SH" | head -n 1)"
  assert_not_empty "the runner's NetworkChaos sets a latency" "$delay_ms"
  assert_contains "the runner judges the delayed times by row 4's distance" "$(cat "$LAB_CHAOS_SH")" "\$1 < l + ${inj}"
  assert_contains "and the released times" "$(cat "$LAB_CHAOS_SH")" "\$1 >= l + ${rel}"
  assert_contains "the paragraph names the NetworkChaos latency" "$para" "delay of ${delay_ms} ms counts as injected"
  room="$(awk -v ms="$delay_ms" -v d="$inj" 'BEGIN { printf "%g", ms / 1000 - d }')"
  assert_contains "the paragraph ties both distances to the delay" "$para" \
    "${inj} s is ${room} s below the delay, and ${rel} s is ${rel} s above no delay, which is the room for jitter of a request as slow as the largest time of step 2."
  assert_contains "the room shrinks by step 2's spread under the delay" "$para" \
    "the room is ${room} s less the spread of step 2's times under the delay, and ${rel} s plus that spread after the release."
  assert_contains "the worked example adds both distances to 0.306 s" "$para" \
    "With a largest baseline time of 0.306 s the two limits are $(awk -v d="$inj" 'BEGIN { printf "%g", 0.306 + d }') s and $(awk -v d="$rel" 'BEGIN { printf "%g", 0.306 + d }') s."
}

# --- Test 5: the Lab NFS stack names its lab run ---
test_nfs_stack_record() {
  echo "Test: the Lab NFS stack links its lab run"
  local nfs
  nfs="$(subsection '### Lab NFS stack')"
  assert_not_empty "the Lab NFS stack has a body" "$nfs"
  assert_not_contains "the Lab NFS stack does not say that no run is recorded" "$nfs" \
    'No lab run of this stack is recorded yet.'
  assert_contains "the Lab NFS stack links the lab fault runs" "$nfs" '(#lab-fault-runs)'
}

# --- Test 6: the runner's subsection ---
test_runner_section() {
  echo "Test: the Lab chaos runner names its targets, settings, gate procedure and hand step"
  local target targets read_vars table
  targets="$(grep -oE '^lab-chaos-[a-z-]+( lab-chaos-[a-z-]+)*:' "$PROJECT_ROOT/Makefile" | tr -d ':' | tr ' ' '\n')"
  assert_eq "the Makefile defines seven lab-chaos targets" "7" "$(grep -c . <<<"$targets")"
  for target in $targets; do
    assert_contains "the subsection runs make $target" "$RUNNER" "make ${target}"
  done
  read_vars="$(sed -nE 's/^(LAB_CHAOS_[A-Z_]+)="\$\{LAB_CHAOS_[A-Z_]+:-.*/\1/p' "$LAB_CHAOS_SH" | sort -u)"
  table="$(sed -nE 's/^\| `(LAB_CHAOS_[A-Z_]+)` \|.*/\1/p' <<<"$RUNNER" | sort -u)"
  assert_eq "the settings table lists exactly the variables the script reads" "$read_vars" "$table"
  assert_contains "the gate is repeated once" "$RUNNER" "1. repeats the wait once, with the same bound;"
  assert_contains "then lab-chaos-0 is hard-rebooted" "$RUNNER" '2. hard-reboots `lab-chaos-0`, whose probes restart from its user-data;'
  assert_contains "then recovered 600" "$RUNNER" '3. runs `recovered 600`;'
  assert_contains "then the remaining faults are skipped" "$RUNNER" '`SKIP: <f> (gate failed after the reboot)`'
  assert_contains "the virsh console check of F1 stays a hand step" "$RUNNER" 'virsh console <instance name>'
  assert_contains "the hand actions go to hand-actions.txt" "$RUNNER" '`hand-actions.txt`'
  assert_contains "the subsection points at the unit test" "$RUNNER" 'tests/unit/hack/lab_chaos_test.sh'
}

# --- Test 7: before or after the session, never both ---
test_record_mode() {
  echo "Test: the Lab chaos runner either says no run is recorded or holds the session's record"
  local pending recorded
  pending="$(grep -cxF 'No lab run of the chaos runner is recorded yet.' <<<"$RUNNER" || true)"
  recorded="$(grep -cE '^The run of 20[0-9]{2}-[0-9]{2}-[0-9]{2} ran `make lab-chaos-proving`' <<<"$RUNNER" || true)"
  assert_eq "exactly one of the two modes" "1" "$((pending + recorded))"
  if [[ "$recorded" == 1 ]]; then
    echo "  record mode: the subsection holds the session's record"
    assert_contains "the record has the proving table" "$RUNNER" '| Step | Result | Measured |'
    local fault
    for fault in F1 F2 F3 F4 F5; do
      assert_not_empty "the record's fault table has a row for $fault" "$(grep -E "^\| ${fault} " <<<"$RUNNER" || true)"
    done
    assert_contains "the record has the random run's row" "$RUNNER" '| Random run | Seed | Window | Steps drawn | Verdict | Failed checks |'
  else
    echo "  pending mode: no session of the runner is recorded"
    assert_eq "and no record of the runner" "" "$(grep -E '^\| Random run \|' <<<"$RUNNER" || true)"
  fi
}

# --- Test 8: the labels come from their sources ---
# A renamed constant fails here before a fault selects no pod.
test_label_sources() {
  echo "Test: the selectors of F1 to F4 carry the labels their manifests and operators set"
  local selectors
  selectors="$(grep -E '^    "F[1-4] lab-[a-z-]+-kill ' "$LAB_CHAOS_SH" || true)"
  assert_eq "the runner defines four pod faults" "4" "$(grep -c . <<<"$selectors")"
  assert_contains "F1 kills the libvirt pod of the node" "$selectors" '"F1 lab-libvirt-kill app.kubernetes.io/name=libvirt node 600"'
  assert_contains "F2 the nova-compute pod of the lab's NovaCompute" "$selectors" \
    '"F2 lab-nova-compute-kill app.kubernetes.io/name=novacompute,app.kubernetes.io/instance=lab,app.kubernetes.io/component=nova-compute node 600"'
  assert_contains "F3 the ovn-controller pod of the lab's OVNChassis" "$selectors" \
    '"F3 lab-ovn-controller-kill app.kubernetes.io/name=ovnchassis,app.kubernetes.io/instance=lab-chassis,app.kubernetes.io/component=ovn-controller node 600"'
  assert_contains "F4 the NFS server wherever it runs" "$selectors" '"F4 lab-nfs-server-kill app.kubernetes.io/name=nfs-server any 900"'
  assert_file_contains_fixed "the libvirt DaemonSet labels its pods app.kubernetes.io/name: libvirt" \
    "$PROJECT_ROOT/deploy/lab/metal-stack/hypervisor/libvirt-daemonset.yaml" 'app.kubernetes.io/name: libvirt'
  assert_file_contains_fixed "the NFS server labels its pods app.kubernetes.io/name: nfs-server" \
    "$PROJECT_ROOT/deploy/kind/nfs/nfs-server.yaml" 'app.kubernetes.io/name: nfs-server'
  assert_file_contains_fixed "the nova operator names the pool's pods novacompute" \
    "$PROJECT_ROOT/operators/nova/internal/controller/novacompute_controller.go" 'novaComputeAppName = "novacompute"'
  assert_file_contains_fixed "the nova operator names their component nova-compute" \
    "$PROJECT_ROOT/operators/nova/internal/controller/novacompute_controller.go" 'novaComputeComponent = "nova-compute"'
  assert_file_contains_fixed "the ovn operator names the chassis pods ovnchassis" \
    "$PROJECT_ROOT/operators/ovn/internal/controller/ovnchassis_controller.go" 'chassisAppName = "ovnchassis"'
  assert_file_contains_fixed "the ovn operator names their component ovn-controller" \
    "$PROJECT_ROOT/operators/ovn/internal/controller/reconcile_controller.go" 'componentOVNController = "ovn-controller"'
  # The operators set app.kubernetes.io/instance to the name of their CR.
  local compute="$PROJECT_ROOT/deploy/lab/metal-stack/hypervisor/compute.yaml"
  assert_eq "the lab's NovaCompute is named lab, the instance label of F2" "lab" \
    "$(awk '/^kind: NovaCompute$/ { k = 1 } k && /^  name: / { print $2; exit }' "$compute")"
  assert_eq "the lab's OVNChassis is named lab-chassis, the instance label of F3" "lab-chassis" \
    "$(awk '/^kind: OVNChassis$/ { k = 1 } k && /^  name: / { print $2; exit }' "$compute")"
  assert_eq "the NFS server is deployment/nfs-server, which F5 scales" "nfs-server" \
    "$(awk '/^kind: Deployment$/ { k = 1 } k && /^  name: / { print $2; exit }' "$PROJECT_ROOT/deploy/kind/nfs/nfs-server.yaml")"
  assert_contains "F5 scales deployment/nfs-server to 0 and back to 1" "$(cat "$LAB_CHAOS_SH")" \
    'kubectl scale deployment/nfs-server -n "$NAMESPACE" --replicas=1'
}

test_position
test_blocks_replaced
test_records_kept
test_proving_run_baseline_rows
test_nfs_stack_record
test_runner_section
test_record_mode
test_label_sources

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
