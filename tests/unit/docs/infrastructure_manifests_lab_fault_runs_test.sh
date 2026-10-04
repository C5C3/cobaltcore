#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the `#### Lab fault runs` subsection of
# docs/reference/infrastructure/infrastructure-manifests.md (#1222):
#   1. the heading occurs once, after `#### Proving run` and before
#      `### Lab ControlPlane`
#   2. every `kind: <X>Chaos` line of the subsection is `kind: PodChaos`, and
#      there are four
#   3. `action: pod-kill`, `mode: one` and `namespaces: [openstack]` occur
#      four times and `nodes: [${host}]` three times, each as a whole line, so
#      prose that quotes them does not count
#   4. the label lines of the four selectors occur as whole lines
#   5. each of the four PodChaos is created by one `name: <name>` line, and
#      awaited and deleted under that name by a
#      `kubectl wait podchaos/<name> -n openstack` and a
#      `kubectl delete podchaos <name> -n openstack` line
#   6. two `kubectl scale deployment/nfs-server -n openstack` lines scale to 0
#      first and to 1 second
#   7. the subsection names no `allowHostNetworkTesting`, and holds the record
#      of its lab run, a line that starts with `The run of 20YY-MM-DD`
#   8. each label and name comes from its source: the libvirt DaemonSet, the
#      NFS server manifest with its Deployment and container name, the
#      constants of the nova and ovn operators, and the names of the lab's
#      NovaCompute and OVNChassis, which the instance labels carry
#   9. the `#### Proving run` subsection holds the record of its lab run, a line
#      that starts with `The run of 20YY-MM-DD`, and not the sentence that no
#      run is recorded
#  10. `recovered`, run from the page with `openstack` stubbed, holds on alive
#      agents and fails on an empty agent list, a failed agent call and a dead
#      agent
#  11. the `### Lab NFS stack` subsection links the lab fault runs and does not
#      say that no run is recorded
#  12. `probes_ok`, run from the page with `openstack` stubbed, holds on new
#      `ok` lines of both probes and fails on a stalled probe and on a first
#      console read that fails or prints nothing
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

FAULTS="$(subsection '#### Lab fault runs')"

# --- Test 1: the subsection's place ---
test_position() {
  echo "Test: '#### Lab fault runs' occurs once, between the Proving run and Lab ControlPlane"
  local all count line proving controlplane
  all="$(headings)"
  count="$(grep -cxE '[0-9]+:#### Lab fault runs' <<<"$all" || true)"
  assert_eq "'#### Lab fault runs' occurs once" "1" "$count"
  line="$({ grep -xE '[0-9]+:#### Lab fault runs' <<<"$all" || true; } | head -n 1 | cut -d: -f1)"
  proving="$({ grep -xE '[0-9]+:#### Proving run' <<<"$all" || true; } | head -n 1 | cut -d: -f1)"
  controlplane="$({ grep -xE '[0-9]+:### Lab ControlPlane' <<<"$all" || true; } | head -n 1 | cut -d: -f1)"
  if [[ -n "$line" && -n "$proving" && -n "$controlplane" ]] &&
    ((proving < line && line < controlplane)); then
    echo "  PASS: the subsection sits on line $line, between line $proving and line $controlplane"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the subsection is missing or out of place (Proving run '${proving}', Lab fault runs '${line}', Lab ControlPlane '${controlplane}')"
    FAIL=$((FAIL + 1))
  fi
  assert_not_empty "the subsection has a body" "$FAULTS"
}

# --- Test 2: PodChaos only ---
test_podchaos_only() {
  echo "Test: every Chaos kind of the subsection is PodChaos, four of them"
  local kinds others
  kinds="$(grep -E 'kind: [A-Za-z]*Chaos' <<<"$FAULTS" | sed -E 's/^[[:space:]]+//' || true)"
  others="$(grep -vxF 'kind: PodChaos' <<<"$kinds" || true)"
  assert_eq "no kind other than PodChaos" "" "$others"
  assert_eq "four PodChaos" "4" "$(count_lines "$FAULTS" 'kind: PodChaos')"
}

# --- Test 3: the experiment lines ---
test_experiment_lines() {
  echo "Test: the four experiments kill one pod in openstack, three of them on lab-0's node"
  assert_eq "'action: pod-kill' four times" "4" "$(count_lines "$FAULTS" 'action: pod-kill')"
  assert_eq "'mode: one' four times" "4" "$(count_lines "$FAULTS" 'mode: one')"
  assert_eq "'namespaces: [openstack]' four times" "4" "$(count_lines "$FAULTS" 'namespaces: [openstack]')"
  # shellcheck disable=SC2016 # the YAML line names the shell variable
  assert_eq "'nodes: [\${host}]' three times" "3" "$(count_lines "$FAULTS" 'nodes: [${host}]')"
}

# --- Test 4: the label lines ---
test_label_lines() {
  echo "Test: the selectors carry the pod labels of the libvirt, nova-compute, ovn-controller and NFS server pods"
  local label
  for label in \
    'app.kubernetes.io/name: libvirt' \
    'app.kubernetes.io/name: novacompute' \
    'app.kubernetes.io/instance: lab' \
    'app.kubernetes.io/component: nova-compute' \
    'app.kubernetes.io/name: ovnchassis' \
    'app.kubernetes.io/instance: lab-chassis' \
    'app.kubernetes.io/component: ovn-controller' \
    'app.kubernetes.io/name: nfs-server'; do
    if [[ "$(count_lines "$FAULTS" "$label")" -ge 1 ]]; then
      echo "  PASS: '$label' is a line of the subsection"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: '$label' is no line of the subsection"
      FAIL=$((FAIL + 1))
    fi
  done
}

# --- Test 5: every experiment keeps one name from create to delete ---
test_experiments_deleted() {
  echo "Test: each PodChaos is created, awaited and deleted under one name"
  local name
  for name in lab-libvirt-kill lab-nova-compute-kill lab-ovn-controller-kill lab-nfs-server-kill; do
    assert_eq "a PodChaos named $name is created once" "1" "$(count_lines "$FAULTS" "name: $name")"
    assert_not_empty "'kubectl wait podchaos/$name -n openstack' is in the subsection" \
      "$(grep -F -- "kubectl wait podchaos/$name -n openstack" <<<"$FAULTS" || true)"
    assert_not_empty "'kubectl delete podchaos $name -n openstack' is in the subsection" \
      "$(grep -F -- "kubectl delete podchaos $name -n openstack" <<<"$FAULTS" || true)"
  done
}

# --- Test 6: the scale-down comes back ---
test_scale_order() {
  echo "Test: the NFS server is scaled to 0 and then to 1"
  local scales
  scales="$(grep -F 'kubectl scale deployment/nfs-server -n openstack' <<<"$FAULTS" || true)"
  assert_eq "two scale lines" "2" "$(grep -c . <<<"$scales" || true)"
  assert_contains "the first scales to 0" "$(sed -n 1p <<<"$scales")" '--replicas=0'
  assert_contains "the second scales to 1" "$(sed -n 2p <<<"$scales")" '--replicas=1'
}

# --- Test 7: no network fault on a host-network pod, and the record ---
test_no_host_network_testing() {
  echo "Test: the subsection names no allowHostNetworkTesting and records its lab run"
  assert_not_contains "no allowHostNetworkTesting" "$FAULTS" 'allowHostNetworkTesting'
  assert_not_empty "a line of the subsection starts with 'The run of 20YY-MM-DD'" \
    "$(grep -E '^The run of 20[0-9]{2}-[0-9]{2}-[0-9]{2}' <<<"$FAULTS" || true)"
}

# --- Test 8: the labels come from their sources ---
# A renamed constant fails here before the block selects no pod.
test_label_sources() {
  echo "Test: each label is the one its manifest or operator sets"
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
  assert_eq "the NFS server is deployment/nfs-server, which F4 and F5 address" "nfs-server" \
    "$(awk '/^kind: Deployment$/ { k = 1 } k && /^  name: / { print $2; exit }' "$PROJECT_ROOT/deploy/kind/nfs/nfs-server.yaml")"
  assert_eq "the NFS server's container is nfs-server, whose log F4 reads" "nfs-server" \
    "$(awk '/^      containers:$/ { c = 1 } c && /^        - name: / { print $3; exit }' "$PROJECT_ROOT/deploy/kind/nfs/nfs-server.yaml")"
}

# --- Test 9: the Proving run records its lab run ---
test_proving_run_record() {
  echo "Test: the Proving run holds the record of its lab run"
  local proving
  proving="$(subsection '#### Proving run')"
  assert_not_empty "the Proving run has a body" "$proving"
  assert_not_contains "the Proving run does not say that no run is recorded" "$proving" \
    'No lab run of this stack is recorded yet.'
  assert_not_empty "a line of the Proving run starts with 'The run of 20YY-MM-DD'" \
    "$(grep -E '^The run of 20[0-9]{2}-[0-9]{2}-[0-9]{2}' <<<"$proving" || true)"
}

# gate <recovered> <agents> <status> runs the function <recovered> with a
# bound of one second against a stub `openstack` that reports lab-0 ACTIVE and
# nova-compute up, and prints <agents> as the node's agent list and exits
# <status>. Both probes write "ok". It prints the exit status of `recovered`.
gate() {
  bash -c '
    eval "$1"
    host=lab-node stub_agents=$2 stub_status=$3
    openstack() {
      case "$*" in
        "server show"*) echo ACTIVE ;;
        "compute service list"*) echo up ;;
        "network agent list"*) printf "%s" "${stub_agents}"; return "${stub_status}" ;;
      esac
    }
    probes_ok() { :; }
    sleep() { :; }
    recovered 1 >/dev/null
    echo "$?"
  ' _ "$@"
}

# --- Test 10: the gate needs alive agents ---
# An empty agent list or a failed agent call says nothing about the node's
# agents, so the gate stays shut on it as on a dead agent.
test_gate_needs_alive_agents() {
  echo "Test: the gate holds on alive agents of lab-0's node alone"
  local helper
  helper="$(awk '/^recovered\(\) \{$/ { f = 1 } f { print } f && /^}$/ { exit }' <<<"$FAULTS")"
  assert_not_empty "the subsection defines recovered" "$helper"
  assert_eq "two alive agents pass the gate" "0" "$(gate "$helper" $'True\nTrue\n' 0)"
  assert_eq "an empty agent list fails the gate" "1" "$(gate "$helper" '' 0)"
  assert_eq "a failed agent call fails the gate" "1" "$(gate "$helper" $'True\n' 1)"
  assert_eq "a dead agent fails the gate" "1" "$(gate "$helper" $'True\nFalse\n' 0)"
}

# --- Test 11: the Lab NFS stack names its lab run ---
test_nfs_stack_record() {
  echo "Test: the Lab NFS stack links its lab run"
  local nfs
  nfs="$(subsection '### Lab NFS stack')"
  assert_not_empty "the Lab NFS stack has a body" "$nfs"
  assert_not_contains "the Lab NFS stack does not say that no run is recorded" "$nfs" \
    'No lab run of this stack is recorded yet.'
  assert_contains "the Lab NFS stack links the lab fault runs" "$nfs" '(#lab-fault-runs)'
}

# probes <helpers> <first log> <first status> <second log> <second status>
# runs the function probes_ok of <helpers> against a stub `openstack` whose
# console log prints <first log> and exits <first status> before the
# 20-second sleep, and prints <second log> and exits <second status> after it.
# It prints the exit status of probes_ok.
probes() {
  bash -c '
    eval "$1"
    stub_log=$2 stub_status=$3 second_log=$4 second_status=$5
    openstack() { printf "%s" "${stub_log}"; return "${stub_status}"; }
    sleep() { stub_log=${second_log} stub_status=${second_status}; }
    probes_ok
    echo "$?"
  ' _ "$@"
}

# --- Test 12: the probes need a first console read ---
# A first read that fails or prints nothing holds no probe line to compare
# with, so the last "ok" line before a stall would count as new.
test_probes_need_first_read() {
  echo "Test: the probe check holds on new ok lines of both probes alone"
  local helpers old stalled new
  helpers="$(awk '/^(console_log|last_probe|probes_ok)\(\) \{$/ { f = 1 } f { print } f && /^}$/ { f = 0 }' <<<"$FAULTS")"
  assert_contains "the subsection defines probes_ok" "$helpers" 'probes_ok() {'
  old=$'lab-probe net 10:00:00 ok\nlab-probe disk 10:00:00 ok 1'
  stalled="$old"$'\nlab-probe net 10:00:20 ok'
  new="$stalled"$'\nlab-probe disk 10:00:20 ok 11'
  assert_eq "new ok lines of both probes pass" "0" "$(probes "$helpers" "$old" 0 "$new" 0)"
  assert_eq "a stalled disk probe fails" "1" "$(probes "$helpers" "$old" 0 "$stalled" 0)"
  assert_eq "a failed first read fails" "1" "$(probes "$helpers" '' 1 "$stalled" 0)"
  assert_eq "a failed first read that prints a log fails" "1" "$(probes "$helpers" "$old" 1 "$new" 0)"
  assert_eq "an empty first read fails" "1" "$(probes "$helpers" '' 0 "$stalled" 0)"
}

test_position
test_podchaos_only
test_experiment_lines
test_label_lines
test_experiments_deleted
test_scale_order
test_no_host_network_testing
test_label_sources
test_proving_run_record
test_gate_needs_alive_agents
test_nfs_stack_record
test_probes_need_first_read

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
