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
#   5. each of the four PodChaos is deleted by a
#      `kubectl delete podchaos <name> -n openstack` line
#   6. two `kubectl scale deployment/nfs-server -n openstack` lines scale to 0
#      first and to 1 second
#   7. the subsection names no `allowHostNetworkTesting`
#   8. each label comes from its source: the libvirt DaemonSet, the NFS server
#      manifest and the constants of the nova and ovn operators
#   9. the `#### Proving run` subsection holds the record of its lab run, a line
#      that starts with `The run of 20YY-MM-DD`, and not the sentence that no
#      run is recorded
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
  line="$(grep -xE '[0-9]+:#### Lab fault runs' <<<"$all" | head -n 1 | cut -d: -f1)"
  proving="$(grep -xE '[0-9]+:#### Proving run' <<<"$all" | head -n 1 | cut -d: -f1)"
  controlplane="$(grep -xE '[0-9]+:### Lab ControlPlane' <<<"$all" | head -n 1 | cut -d: -f1)"
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

# --- Test 5: every experiment is deleted ---
test_experiments_deleted() {
  echo "Test: each PodChaos is deleted by its section"
  local name
  for name in lab-libvirt-kill lab-nova-compute-kill lab-ovn-controller-kill lab-nfs-server-kill; do
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

# --- Test 7: no network fault on a host-network pod ---
test_no_host_network_testing() {
  echo "Test: the subsection names no allowHostNetworkTesting"
  assert_not_contains "no allowHostNetworkTesting" "$FAULTS" 'allowHostNetworkTesting'
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

test_position
test_podchaos_only
test_experiment_lines
test_label_lines
test_experiments_deleted
test_scale_order
test_no_host_network_testing
test_label_sources
test_proving_run_record

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
