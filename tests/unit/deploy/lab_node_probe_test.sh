#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Exercise the user command against a fake API, including failure and cleanup
# paths. The existing metal_stack_probe_test.sh enforces host read-only safety.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
PASS=0 FAIL=0
# shellcheck source=tests/lib/assertions.sh
source "$ROOT/tests/lib/assertions.sh"
for tool in jq yq; do
  command -v "$tool" >/dev/null || { echo "SKIP: $tool required"; exit 0; }
done
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
mkdir "$scratch/bin"
cat >"$scratch/bin/kubectl" <<'MOCK'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >>"$CASE_DIR/calls.log"
if [[ "$1" == config ]]; then echo test-context; exit; fi
[[ "$1" == --context=test-context && "$2" == --request-timeout=30s ]] || exit 90
shift 2
case "$1 $2" in
  'get nodes')
    if [[ "$SCENARIO" == api-fail ]]; then echo Forbidden >&2; exit 1; fi
    jq -n --arg scenario "$SCENARIO" '{items: ["worker-a", "worker-b"] | map({metadata: {name: ., labels: {"topology.kubernetes.io/zone": "zone-a"}}, status: {conditions: [{type: "Ready", status: "True"}]}})} |
      if $scenario == "zero-nodes" then .items = []
      elif $scenario == "not-ready" then .items[0].status.conditions[0].status = "False"
      elif $scenario == "mixed-zone" then .items[1].metadata.labels["topology.kubernetes.io/zone"] = "zone-b"
      elif $scenario == "missing-zone" then del(.items[0].metadata.labels["topology.kubernetes.io/zone"])
      else . end'
    ;;
  'get storageclasses')
    if [[ "$SCENARIO" == no-storage ]]; then echo '{"items":[]}'
    else echo '{"items":[{"metadata":{"name":"default-class","annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}]}' ; fi ;;
  'get daemonset')
    if [[ "$SCENARIO" == dns-denied ]]; then echo Forbidden >&2; exit 1; fi
    if [[ "$SCENARIO" == node-dns ]]; then echo daemonset.apps/node-local-dns; fi ;;
  'create -f')
    yq -o=json '.' "$3" >"$CASE_DIR/$PROBE_NODE.json"
    name="$(jq -r .metadata.name "$CASE_DIR/$PROBE_NODE.json")"
    [[ "$name" == node-probe-*-* && "$name" != node-probe ]] || exit 91
    if [[ "$SCENARIO" == create-fail && "$PROBE_NODE" == worker-a ]]; then echo 'admission denied' >&2; exit 1; fi
    touch "$CASE_DIR/$name.active"
    echo "job.batch/$name created" ;;
  'wait -n')
    if [[ "$SCENARIO" == interrupted ]]; then kill -TERM "$PPID"; exit 143; fi
    if [[ "$SCENARIO" == timeout && "$PROBE_NODE" == worker-a ]]; then echo 'condition timed out' >&2; exit 1; fi
    echo complete ;;
  'logs -n')
    if [[ "$SCENARIO" == logs-fail && "$PROBE_NODE" == worker-a ]]; then echo Forbidden >&2; exit 1; fi
    if [[ "$SCENARIO" == truncated && "$PROBE_NODE" == worker-a ]]; then printf '== kvm device\nabsent\n== rook\n/var/lib/rook: absent\n'; exit; fi
    if [[ "$PROBE_NODE" == worker-a ]]; then
      case "$SCENARIO" in
        blocker) sed -e 's/^crw.*\/dev\/kvm$/absent/' -e 's@^vhost_net:.*@vhost_net: NOT FOUND@' -e 's/^cgroup2fs$/tmpfs/' -e 's/calib: mtu=1500/calib: mtu=1400/' "$FIXTURE" ;;
        mixed-cpu) sed 's/Intel(R)/AMD/' "$FIXTURE" ;;
        existing) sed -e 's@/var/lib/rook: absent@/var/lib/rook: present@' -e 's@usr/sbin/libvirtd: absent@usr/sbin/libvirtd: present@' "$FIXTURE" ;;
        unknown-mtu) sed 's/^cali/other/' "$FIXTURE" ;;
        optional-ready) sed 's/: NOT FOUND/: builtin/' "$FIXTURE" ;;
        chaos-socket) sed -e 's/: NOT FOUND/: builtin/' -e 's@run/containerd/containerd.sock: socket@run/containerd/containerd.sock: absent@' "$FIXTURE" ;;
        *) cat "$FIXTURE" ;;
      esac
    elif [[ "$SCENARIO" == optional-ready || "$SCENARIO" == chaos-socket ]]; then sed 's/: NOT FOUND/: builtin/' "$FIXTURE"
    else cat "$FIXTURE"; fi ;;
  'describe -n'|'describe pods') echo 'Pod diagnosis: image pull failed' ;;
  'delete -n')
    [[ "$4" == job/node-probe-*-* ]] || exit 92
    if [[ "$SCENARIO" == cleanup-fail && ! -e "$CASE_DIR/delete-failed" ]]; then
      touch "$CASE_DIR/delete-failed"; echo Forbidden >&2; exit 1
    fi
    rm -f "$CASE_DIR/${4#job/}.active" ;;
  *) echo "Unexpected kubectl: $*" >&2; exit 99 ;;
esac
MOCK
chmod +x "$scratch/bin/kubectl"
export PATH="$scratch/bin:$PATH"
export FIXTURE="$ROOT/tests/fixtures/lab-node-probe/ready.log"
run_case() {
  local scenario="$1" expected="$2"
  shift 2
  export SCENARIO="$scenario" CASE_DIR="$scratch/$scenario"
  mkdir "$CASE_DIR"
  rc=0
  env NO_COLOR=1 WITH_NFS=false WITH_CHAOS_MESH=false NODE_PROBE_TIMEOUT=1 \
    NODE_PROBE_OUTPUT_DIR="$CASE_DIR/results" "$@" \
    bash "$ROOT/hack/lab-node-probe.sh" >"$CASE_DIR/output" 2>&1 || rc=$?
  assert_eq "$scenario exit status" "$expected" "$rc"
  # No active test Job may remain, even when another worker or a read failed.
  assert_eq "$scenario cleans up only its Jobs" '' "$(find "$CASE_DIR" -name '*.active' -print)"
  report="$(cat "$CASE_DIR/results/summary.tsv")"
}
run_case ready 0
assert_contains 'absence of host binaries is expected' "$report" $'AUTO\tHost binaries'
assert_contains 'available unloaded modules are provided later' "$report" $'AUTO\tKVM / OVN modules'
assert_contains 'missing optional modules do not block' "$report" $'SKIP\tNFS'
assert_file_contains 'unregistered NFS filesystems do not block' "$CASE_DIR/output" '0 blocker'
assert_eq 'plain output has no ANSI escape' '' "$(LC_ALL=C grep $'\033' "$CASE_DIR/output" || true)"
assert_eq 'Job is pinned to its requested worker' worker-a "$(jq -r .spec.template.spec.nodeName "$CASE_DIR/worker-a.json")"
assert_eq 'host mount remains read-only' true "$(jq -r '.spec.template.spec.containers[0].volumeMounts[0].readOnly' "$CASE_DIR/worker-a.json")"
assert_eq 'probe gets a deadline' 1 "$(jq .spec.activeDeadlineSeconds "$CASE_DIR/worker-a.json")"
assert_eq 'probe gets no API token' false "$(jq .spec.template.spec.automountServiceAccountToken "$CASE_DIR/worker-a.json")"
assert_eq 'successful raw log saved' "$(cat "$FIXTURE")" "$(cat "$CASE_DIR/results/worker-a.log")"
assert_file_contains_fixed 'saved command log pins context' "$CASE_DIR/results/commands.log" --context=test-context

run_case blocker 1
for check in /dev/kvm 'KVM / OVN modules' 'cgroup v2' 'Pod MTU'; do
  assert_contains "detect $check blocker" "$report" "$(printf 'worker-a\tBLOCK\t%s' "$check")"
done
assert_contains 'second worker still probed after blockers' "$report" $'worker-b\tPASS\t/dev/kvm'
run_case optional-missing 1 WITH_NFS=true WITH_CHAOS_MESH=true
assert_contains 'enabled NFS requirements block when missing' "$report" $'BLOCK\tNFS client modules'
assert_contains 'enabled Chaos requirements block when missing' "$report" $'BLOCK\tChaos Mesh modules'
run_case optional-ready 0 WITH_NFS=true WITH_CHAOS_MESH=true
assert_contains 'built-in modules are accepted' "$report" $'PASS\tNFS client modules'
assert_contains 'NFS server supplied by deploy' "$report" $'AUTO\tNFS server'
run_case chaos-socket 1 WITH_CHAOS_MESH=true
assert_contains 'wrong runtime socket blocks enabled Chaos' "$report" $'BLOCK\tChaos runtime socket'
run_case existing 0
assert_contains 'existing Rook data needs review, not deletion' "$report" $'WARN\tCeph host data'
assert_contains 'installed host binary is not proof of conflict' "$report" $'WARN\tHost binaries'
run_case unknown-mtu 0
assert_contains 'unrecognized CNI is not a false MTU pass' "$report" $'WARN\tPod MTU'
run_case mixed-cpu 1
assert_contains 'cross-node CPU mismatch blocks' "$report" $'BLOCK\tCPU model agreement'
for scenario in mixed-zone missing-zone zero-nodes not-ready no-storage node-dns; do run_case "$scenario" 1; done
for scenario in create-fail timeout logs-fail truncated; do
  run_case "$scenario" 2
  assert_contains "$scenario reports probe error" "$report" $'worker-a\tERROR\tProbe'
  assert_contains "$scenario still probes next worker" "$report" $'worker-b\tPASS\t/dev/kvm'
done
assert_file_contains 'timeout diagnosis saved before cleanup' "$scratch/timeout/results/worker-a-diagnostics.log" 'image pull failed'
run_case api-fail 2
run_case dns-denied 2
assert_contains 'API denial is not confused with absent DNS' "$report" $'ERROR\tNode-local DNS'
run_case cleanup-fail 2
assert_contains 'cleanup failure is reported' "$report" $'ERROR\tCleanup'
run_case interrupted 143

echo "Results: $PASS passed, $FAIL failed"
[[ "$FAIL" == 0 ]]
