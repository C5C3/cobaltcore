#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Run the read-only host survey and interpret it on the workstation. Keep the
# raw Job output for diagnostics; only this wrapper assigns readiness statuses.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

usage() {
  cat <<'EOF'
Usage: hack/lab-node-probe.sh [--verbose]

Probe every worker in the current kubeconfig context, print an actionable
table, save raw logs, and remove this run's temporary Jobs. Host reads only.

  --verbose                       Also print each node's raw survey
  --help                          Show this help

Environment:
  WITH_NFS=true                    Check optional NFS client requirements
  WITH_CHAOS_MESH=true             Check optional Chaos Mesh requirements
  NODE_PROBE_NODE_SELECTOR         Override the default worker selector
                                  (!node-role.kubernetes.io/control-plane,
                                   !node-role.kubernetes.io/master)
  NODE_PROBE_NAMESPACE             Job namespace (default: default)
  NODE_PROBE_TIMEOUT               Seconds to wait per Job (default: 300)
  NODE_PROBE_OUTPUT_DIR            New directory for results (default: temp dir)
  NO_COLOR                        Disable terminal colors when nonempty

Exit: 0 = no detected blockers (review WARN rows); 1 = prerequisite blocker;
      2 = usage, API, probe, or cleanup error. This is not a full deployment test.
EOF
}

verbose=false
for arg in "$@"; do
  case "$arg" in
    --help|-h) usage; exit 0 ;;
    --verbose) verbose=true ;;
    *) printf 'Unknown argument: %s\n' "$arg" >&2; usage >&2; exit 2 ;;
  esac
done
for tool in kubectl jq yq; do
  command -v "$tool" >/dev/null || { echo "Missing required tool: $tool" >&2; exit 2; }
done
WITH_NFS="${WITH_NFS:-false}"
WITH_CHAOS_MESH="${WITH_CHAOS_MESH:-false}"
for value in "$WITH_NFS" "$WITH_CHAOS_MESH"; do
  case "$value" in true|false) ;; *) echo 'WITH_NFS and WITH_CHAOS_MESH must be true or false.' >&2; exit 2 ;; esac
done
namespace="${NODE_PROBE_NAMESPACE:-default}"
timeout="${NODE_PROBE_TIMEOUT:-300}"
if [[ ! "$timeout" =~ ^[1-9][0-9]*$ ]]; then
  echo 'NODE_PROBE_TIMEOUT must be a positive number of seconds.' >&2; exit 2
fi
selector="${NODE_PROBE_NODE_SELECTOR-!node-role.kubernetes.io/control-plane,!node-role.kubernetes.io/master}"
if [[ -n "${NODE_PROBE_OUTPUT_DIR:-}" ]]; then
  mkdir -m 700 "$NODE_PROBE_OUTPUT_DIR" || exit 2
  output="$(cd "$NODE_PROBE_OUTPUT_DIR" && pwd)"
else
  output="$(mktemp -d "${TMPDIR:-/tmp}/cobaltcore-node-probe.XXXXXX")"
fi
printf 'scope\tstatus\tcheck\tdetail\n' >"$output/summary.tsv"
: >"$output/summary.txt"
jobs=()
context=""
scope=cluster
blocks=0 errors=0 warnings=0
color=false
if [[ -t 1 && "${TERM:-dumb}" != dumb && -z "${NO_COLOR:-}" ]]; then color=true; fi

say() { printf '%s\n' "$*"; printf '%s\n' "$*" >>"$output/summary.txt"; }
table() { say ''; say "$*"; say 'STATUS  CHECK                   FINDING / NEXT STEP'; }
row() {
  local status="$1" check="$2" detail="$3" code=0
  case "$status" in
    BLOCK) blocks=$((blocks + 1)); code=31 ;;
    ERROR) errors=$((errors + 1)); code=31 ;;
    WARN) warnings=$((warnings + 1)); code=33 ;;
    PASS) code=32 ;;
    AUTO) code=36 ;;
    SKIP) code=90 ;;
  esac
  printf '%s\t%s\t%s\t%s\n' "$scope" "$status" "$check" "$detail" >>"$output/summary.tsv"
  printf '%-7s %-23s %s\n' "$status" "$check" "$detail" >>"$output/summary.txt"
  if "$color"; then printf '\033[%sm%-7s\033[0m %-23s %s\n' "$code" "$status" "$check" "$detail"
  else printf '%-7s %-23s %s\n' "$status" "$check" "$detail"; fi
}
kube() {
  {
    printf 'kubectl --context=%q --request-timeout=30s ' "$context"
    printf '%q ' "$@"
    printf '\n'
  } >>"$output/commands.log"
  kubectl --context="$context" --request-timeout=30s "$@"
}
cleanup_job() {
  kube delete -n "$namespace" "$1" --ignore-not-found --wait=true --timeout=60s --request-timeout=90s >>"$output/cleanup.log" 2>&1
}
cleanup() {
  local rc=$? job
  trap - EXIT
  for job in ${jobs[@]+"${jobs[@]}"}; do
    if ! cleanup_job "$job"; then
      echo "Cleanup failed for $job in $namespace; see $output/cleanup.log" >&2
      rc=2
    fi
  done
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Sections are intentionally matched exactly; a missing fact must never PASS.
section() {
  awk -v header="== $1" '
    /^== / { inside = ($0 == header || (header == "== module files for" && index($0, header " ") == 1)); next }
    inside { print }
  ' "$raw"
}
complete_survey() {
  local header binary
  for header in 'kvm device' cpu 'loaded modules' 'module files for' filesystems \
    'nested / iommu' memory disks cgroup 'host os / binaries' 'containerd socket' nics rook; do
    [[ -n "$(section "$header")" ]] || return 1
  done
  for binary in usr/sbin/libvirtd usr/bin/qemu-system-x86_64 usr/sbin/ovs-vswitchd; do
    section 'host os / binaries' | grep -qxE "$binary: (absent|present)" || return 1
  done
}
module_group() {
  local check="$1" phase="$2" m fact missing="" unloaded=""
  shift 2
  for m in "$@"; do
    fact="$(section 'module files for' | awk -v key="$m:" '$1 == key { print $2 }')"
    if [[ -z "$fact" || "$fact" == NOT ]]; then
      missing="$missing $m"
    elif [[ "$fact" != builtin && "$fact" != /host/lib/modules/* ]]; then
      row ERROR "$check" "Unrecognized module result for $m; inspect raw log."
      return
    elif [[ "$fact" != builtin ]] && ! section 'loaded modules' | grep -qx "${m//-/_}"; then
      unloaded="$unloaded $m"
    fi
  done
  if [[ -n "$missing" ]]; then
    row BLOCK "$check" "Missing module files:$missing. Install matching host kernel modules; deployment cannot supply them."
  elif [[ -n "$unloaded" ]]; then
    row AUTO "$check" "Available, not loaded:$unloaded. $phase loads them."
  else
    row PASS "$check" 'Required modules are available and loaded or built in.'
  fi
}
summarize_node() {
  local model flags cgroup mtu memory disks binaries rook
  if section 'kvm device' | grep -q '^c.* /dev/kvm$'; then
    row PASS /dev/kvm 'KVM character device present.'
  else
    row BLOCK /dev/kvm 'KVM device unavailable. Enable hardware virtualization and the host KVM driver.'
  fi
  model="$(section cpu | sed -n 's/^Model name:[[:space:]]*//p')"
  flags="$(section cpu | sed -n 's/^cpus with vmx|svm: //p')"
  if [[ "$flags" =~ ^[0-9]+$ && "$flags" -gt 0 && -n "$model" ]]; then
    row PASS CPU "$model; $flags logical CPUs expose virtualization."
    printf '%s\t%s\n' "$scope" "$model" >>"$output/cpu-models.tsv"
  else
    row BLOCK CPU 'CPU model or vmx/svm flags unavailable. Check host virtualization support.'
  fi
  module_group 'KVM / OVN modules' 'Part 2 (libvirt / OVN chassis)' vhost_net openvswitch geneve
  cgroup="$(section cgroup)"
  if [[ "$cgroup" == cgroup2fs ]]; then row PASS 'cgroup v2' cgroup2fs
  else row BLOCK 'cgroup v2' "Found ${cgroup:-unknown}; configure the host to use cgroup v2."; fi

  binaries="$(section 'host os / binaries' | awk '$1 ~ /\/(libvirtd|qemu-system-x86_64|ovs-vswitchd):$/ && $2 == "present" { sub(/:$/, "", $1); printf "%s ", $1 }')"
  if [[ -n "$binaries" ]]; then
    row WARN 'Host binaries' "Present: $binaries; verify host services will not conflict with Part 2's containers."
  else
    row AUTO 'Host binaries' 'libvirt, QEMU and OVS run in Part 2 containers; absent host binaries are expected.'
  fi
  if [[ "$WITH_NFS" == true ]]; then
    module_group 'NFS client modules' 'make deploy-infra WITH_NFS=true' nfs nfsv4
    row AUTO 'NFS server' 'make deploy-infra WITH_NFS=true installs Ganesha and CSI; host rpc.nfsd/nfsd is not required.'
  else row SKIP NFS 'Optional; enable checks with WITH_NFS=true if deploying Part 3.'; fi
  if [[ "$WITH_CHAOS_MESH" == true ]]; then
    module_group 'Chaos Mesh modules' 'make deploy-infra WITH_CHAOS_MESH=true' ip_set ip_set_hash_ip ip_set_hash_net xt_set sch_netem sch_tbf
    if section 'containerd socket' | grep -qx 'run/containerd/containerd.sock: socket'; then
      row PASS 'Chaos runtime socket' /run/containerd/containerd.sock
    else row BLOCK 'Chaos runtime socket' 'Default containerd socket absent. Adapt the Chaos Mesh runtime/socket configuration.'; fi
  else row SKIP 'Chaos Mesh' 'Optional; enable module/socket checks with WITH_CHAOS_MESH=true.'; fi

  rook="$(section rook | head -n 1)"
  if [[ "$rook" == '/var/lib/rook: absent' ]]; then row PASS 'Ceph host data' 'No existing /var/lib/rook; make deploy-infra WITH_CEPH=true installs Rook/Ceph.'
  elif [[ "$rook" == '/var/lib/rook: present' ]]; then
    row WARN 'Ceph host data' '/var/lib/rook exists: expected for deployed Ceph; before a fresh install, check ownership and recover/clean up safely.'
  else row ERROR 'Ceph host data' 'Missing Rook directory result; inspect raw log.'; fi
  mtu="$(section nics | awk '/^cali[^:]*: mtu=[0-9]+ / { split($2, a, "="); if (!seen++ || a[2]+0 < min) min=a[2]+0 } END { if (seen) print min }')"
  if [[ -z "$mtu" ]]; then row WARN 'Pod MTU' 'No cali* MTU observed; verify the CNI MTU supports global_physnet_mtu=1460.'
  elif (( mtu < 1460 )); then row BLOCK 'Pod MTU' "Minimum observed MTU $mtu < 1460; adjust the network or Neutron global_physnet_mtu."
  else row PASS 'Pod MTU' "Minimum observed cali* MTU $mtu >= 1460; end-to-end path MTU is not tested."; fi
  memory="$(section memory | awk '/^MemTotal:/ { printf "%.1f GiB RAM", $2/1048576 }')"
  disks="$(section disks | awk '$NF == "/host/var/lib" { print $4 " free / " $2 " total at /var/lib" }')"
  row WARN Capacity "${memory:-RAM unknown}; ${disks:-/var/lib capacity unknown}. Size VM disks and workload requests; no universal capacity threshold."
}

context="$(kubectl config current-context 2>"$output/context-error.log")" || { echo "Cannot read kubeconfig context; see $output" >&2; exit 2; }
printf '%s\n' "$context" >"$output/context.txt"
say "CobaltCore node probe — context: $context"
say "Results: $output"
say 'PASS ready | BLOCK fix before proceeding | WARN review | AUTO provided later | SKIP optional'
say "Options: WITH_NFS=$WITH_NFS WITH_CHAOS_MESH=$WITH_CHAOS_MESH"
table 'Cluster'
if ! kube get nodes -l "$selector" -o json >"$output/nodes.json" 2>"$output/nodes-error.log"; then
  row ERROR Workers 'Cannot list nodes; check context/RBAC and nodes-error.log.'; exit 2
fi
count="$(jq '.items | length' "$output/nodes.json")"
if (( count < 2 )); then row BLOCK Workers "$count selected; this lab needs at least two workers. Check NODE_PROBE_NODE_SELECTOR."
else row PASS Workers "$count selected (selector: ${selector:-all nodes})."; fi
zones="$(jq -r '[.items[] | (.metadata.labels["topology.kubernetes.io/zone"] // "")] | unique | join(",")' "$output/nodes.json")"
if [[ -z "$zones" || "$zones" == *,* ]]; then row BLOCK Zone 'Workers need the same nonempty topology.kubernetes.io/zone label.'
else row PASS Zone "$zones"; fi
if kube get storageclasses -o json >"$output/storageclasses.json" 2>"$output/storageclasses-error.log"; then
  defaults="$(jq -r '[.items[] | select(.metadata.annotations["storageclass.kubernetes.io/is-default-class"] == "true") | .metadata.name] | join(", ")' "$output/storageclasses.json")"
  if [[ -z "$defaults" ]]; then row BLOCK StorageClass 'No default StorageClass; configure one before make deploy-infra.'
  else row PASS StorageClass "$defaults; raw-block support and provisioning capacity are verified during deployment."; fi
else row ERROR StorageClass 'Cannot read StorageClasses; see storageclasses-error.log.'; fi
if kube get daemonset node-local-dns -n kube-system --ignore-not-found -o name >"$output/node-local-dns.txt" 2>"$output/node-local-dns-error.log"; then
  if [[ -s "$output/node-local-dns.txt" ]]; then row BLOCK 'Node-local DNS' 'node-local-dns exists; resolve the OpenBao network-policy incompatibility before deployment.'
  else row PASS 'Node-local DNS' 'No kube-system/node-local-dns DaemonSet.'; fi
else row ERROR 'Node-local DNS' 'Cannot inspect DaemonSet; see node-local-dns-error.log.'; fi

run_id="node-probe-$(date +%s)-$$-$RANDOM"
index=0
: >"$output/cpu-models.tsv"
while IFS= read -r node; do
  scope="$node"
  table "Node: $node"
  if ! jq -e --arg node "$node" '.items[] | select(.metadata.name == $node) | any(.status.conditions[]?; .type == "Ready" and .status == "True")' "$output/nodes.json" >/dev/null; then
    row BLOCK Ready 'Node is not Ready; restore node health and rerun the probe.'
    continue
  fi
  row PASS Ready 'Kubernetes node reports Ready.'
  if jq -e --arg node "$node" '.items[] | select(.metadata.name == $node) | .spec.unschedulable == true' "$output/nodes.json" >/dev/null; then
    row WARN Scheduling 'Node is cordoned; this pinned probe does not prove workload schedulability.'
  fi
  index=$((index + 1))
  job="job/$run_id-$index"
  export PROBE_NODE="$node" PROBE_JOB_NAME="$run_id-$index" PROBE_NAMESPACE="$namespace" PROBE_DEADLINE="$timeout"
  # A unique name per run avoids deleting another run's probe or stale logs.
  if ! yq '.metadata.name = strenv(PROBE_JOB_NAME) | .metadata.namespace = strenv(PROBE_NAMESPACE) | .spec.template.spec.nodeName = strenv(PROBE_NODE) | .spec.activeDeadlineSeconds = env(PROBE_DEADLINE)' \
    "$REPO_ROOT/deploy/lab/metal-stack/probe/node-probe.yaml" >"$output/$node.yaml" 2>"$output/$node-render.log"; then
    row ERROR Probe "Cannot render Job; see $node-render.log."; continue
  fi
  jobs+=("$job")
  say 'Running read-only host survey...'
  raw="$output/$node.log"
  if ! kube create -f "$output/$node.yaml" >"$output/$node-create.log" 2>&1; then
    row ERROR Probe "Cannot create Job; see $node-create.log."
  elif ! kube wait -n "$namespace" --for=condition=complete "$job" --timeout="${timeout}s" --request-timeout="$((timeout + 30))s" >"$output/$node-wait.log" 2>&1; then
    row ERROR Probe "Job failed or timed out; see $node-diagnostics.log and $node.log."
    kube describe -n "$namespace" "$job" >"$output/$node-diagnostics.log" 2>&1 || true
    kube describe pods -n "$namespace" -l "job-name=$run_id-$index" >>"$output/$node-diagnostics.log" 2>&1 || true
    kube logs -n "$namespace" "$job" >"$raw" 2>&1 || true
  elif ! kube logs -n "$namespace" "$job" >"$raw" 2>"$output/$node-logs-error.log"; then
    row ERROR Probe "Cannot read completed Job logs; see $node-logs-error.log."
  elif ! complete_survey; then
    row ERROR Probe "Incomplete survey; see $node.log."
  else
    summarize_node
    if "$verbose"; then cat "$raw"; fi
  fi
  if cleanup_job "$job"; then
    unset 'jobs[${#jobs[@]}-1]'
  else row ERROR Cleanup "Could not delete $job; see cleanup.log. Cleanup will be retried on exit."; fi
done < <(jq -r '.items[].metadata.name' "$output/nodes.json")

scope=cluster
table 'Cross-node checks and deployment stages'
models="$(cut -f2 "$output/cpu-models.tsv" | sort -u | wc -l | tr -d ' ')"
if (( models > 1 )); then row BLOCK 'CPU model agreement' 'Workers have different CPU models; use compatible workers for the lab migration test.'
elif (( $(wc -l <"$output/cpu-models.tsv") == count && count > 0 )); then
  row PASS 'CPU model agreement' 'Selected workers report the same CPU model.'
else row WARN 'CPU model agreement' 'Not all selected workers returned a CPU model; comparison is incomplete.'; fi
row WARN 'Nova CPU configuration' 'Match cpuModels in deploy/lab/metal-stack/hypervisor/compute.yaml to the hardware; CPU name alone cannot prove libvirt compatibility.'
row AUTO Infrastructure 'make deploy-infra installs shared services/operators; WITH_CEPH=true adds Rook/Ceph. They need not exist before probing.'
row AUTO Hypervisors 'Part 2 installs libvirt/QEMU, OVN chassis and nova-compute; make deploy-infra alone does not install these.'
row WARN 'Remaining validation' 'Check registry/Helm egress and storage capacity; run hack/lab-node-ports.sh in Part 2 for migration TCP ports. Geneve connectivity needs end-to-end testing.'
say ''
say "Summary: $blocks blocker(s), $warnings review item(s), $errors probe error(s)."
if (( errors )); then say 'Probe incomplete. Resolve ERROR rows and rerun.'
elif (( blocks )); then say 'Resolve BLOCK rows before proceeding, then rerun.'
else say 'No detected blockers. Review WARN rows before continuing with Step 3.'; fi
say "Saved tables: $output/summary.txt and summary.tsv; raw logs and commands are beside them."
if (( errors )); then exit 2; elif (( blocks )); then exit 1; fi
