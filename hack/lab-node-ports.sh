#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/lab-node-ports.sh — Prove that the hypervisor ports pass between the nodes.
#
# Live migration between two hypervisors (#1142) needs libvirt's TLS port 16514
# and the QEMU migration range 49152-49215 open from every node to every other
# node on the node network. This script checks exactly that on the cluster the
# current kubeconfig context points at, and changes nothing on it but the Pods
# it creates and deletes again:
#
#   - one listener Pod per node, lab-node-ports-listen-<node>, host-networked and
#     pinned to the node, binding every port on 0.0.0.0 (perl, IO::Socket::INET);
#   - one client Pod per ordered node pair (A, B), lab-node-ports-client-<a>-<b>,
#     host-networked on A, connecting to B's first InternalIP on every port
#     (bash /dev/tcp under coreutils timeout), the address status.hostIP and so
#     the NovaCompute live_migration_inbound_addr resolve to.
#
# Every Pod is unprivileged (no capability, no privilege escalation, read-only
# root filesystem, UID 65534, no ServiceAccount token): the ports are above
# 1024, so binding them needs nothing. The image is the node probe's pinned
# debian image (deploy/lab/metal-stack/probe/node-probe.yaml), which carries
# bash, perl and timeout, so the check has no image or Renovate pin of its own.
#
# Output: one line per ordered pair, then a summary:
#
#   worker-a (10.128.44.10) -> worker-b (10.128.44.11): 65/65 open
#   worker-b (10.128.44.11) -> worker-a (10.128.44.10): 63/65 open, closed: 16514 49215
#
# A port is `closed` when the connect fails, `listener bind failed` when the
# destination's listener could not bind it (49152-49215 sit in Linux's ephemeral
# range, so an outgoing connection on the node can hold one), and `no result`
# when the client produced no line for it (the Pod never ran, its image did not
# pull, or the client wait ran out). All three fail the run.
#
# Optional env vars:
#   NODE_PORTS_TCP             — ports and inclusive ranges, space-separated
#                                (default: "16514 49152-49215", 65 ports)
#   NODE_PORTS_NAMESPACE       — namespace of the Pods (default: default)
#   NODE_PORTS_IMAGE           — image with bash, perl and timeout (default: the
#                                image: line of the node probe manifest)
#   NODE_PORTS_NODE_SELECTOR   — label selector limiting the nodes (default: all)
#   NODE_PORTS_CONNECT_TIMEOUT — seconds per connect (default: 5)
#   NODE_PORTS_POD_TIMEOUT     — seconds for the listener waits and the removal
#                                of an earlier run's Pods; the client wait adds
#                                NODE_PORTS_CONNECT_TIMEOUT per port, the scan's
#                                length when a firewall drops the packets
#                                (default: 120)
#
# Exit codes:
#   0 — every port of every ordered pair is open
#   1 — at least one port is closed, has no result, or was not bound by the
#       listener
#   2 — usage or cluster error (a missing tool, an invalid knob, fewer than two
#       nodes, a node without an InternalIP or whose first one is IPv6 or not
#       an address, Pods of an earlier run that cannot be removed, Pods that
#       cannot be created, listeners that never became Ready)
#
# The Pods carry app.kubernetes.io/name=lab-node-ports. A run first deletes the
# ones an earlier run left behind, and an EXIT trap deletes every Pod with that
# label in the namespace on every path.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

NODE_PORTS_TCP="${NODE_PORTS_TCP:-16514 49152-49215}"
NODE_PORTS_NAMESPACE="${NODE_PORTS_NAMESPACE:-default}"
NODE_PORTS_IMAGE="${NODE_PORTS_IMAGE:-}"
NODE_PORTS_NODE_SELECTOR="${NODE_PORTS_NODE_SELECTOR:-}"
NODE_PORTS_CONNECT_TIMEOUT="${NODE_PORTS_CONNECT_TIMEOUT:-5}"
NODE_PORTS_POD_TIMEOUT="${NODE_PORTS_POD_TIMEOUT:-120}"

# The label every Pod carries (pod_json); both Pod deletes select by it.
POD_LABEL_KEY="app.kubernetes.io/name"
POD_LABEL_VALUE="lab-node-ports"
POD_LABEL="${POD_LABEL_KEY}=${POD_LABEL_VALUE}"

# The listener: one socket per port given as an argument, a line for each port
# it cannot bind, `listening` once every bind was tried, then accept-and-close
# forever. A TERM ends it at once (PID 1 ignores signals it does not handle).
LISTENER_SCRIPT="$(cat <<'PERL'
use strict; use warnings; use IO::Socket::INET; use IO::Select;
$| = 1;
$SIG{TERM} = sub { exit 0 };
my $sel = IO::Select->new;
for my $port (@ARGV) {
  my $sock = IO::Socket::INET->new(LocalAddr => "0.0.0.0", LocalPort => $port,
    Proto => "tcp", Listen => 16, ReuseAddr => 1);
  if ($sock) { $sel->add($sock) } else { print "bind failed $port\n" }
}
print "listening\n";
sleep while $sel->count == 0;
while (1) {
  for my $ready ($sel->can_read) { my $conn = $ready->accept; close $conn if $conn }
}
PERL
)"

# ---------------------------------------------------------------------------
# log — Print a timestamped log message (ISO 8601 UTC).
# ---------------------------------------------------------------------------
log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

# ---------------------------------------------------------------------------
# expand_ports SPEC — Print the ports of SPEC (space-separated ports and
# inclusive a-b ranges) space-separated, in order. Exits 2 on an entry that is
# not a port in 1-65535 or a range whose start exceeds its end. Errors go to
# stderr, because callers capture stdout.
# ---------------------------------------------------------------------------
expand_ports() {
  local items=() item lo hi p out=()
  read -r -a items <<<"$1"
  for item in ${items[@]+"${items[@]}"}; do
    if [[ "${item}" =~ ^([0-9]+)-([0-9]+)$ ]]; then
      lo="${BASH_REMATCH[1]}"
      hi="${BASH_REMATCH[2]}"
    elif [[ "${item}" =~ ^[0-9]+$ ]]; then
      lo="${item}"
      hi="${item}"
    else
      log "ERROR: NODE_PORTS_TCP entry '${item}' is not a port or a port range." >&2
      exit 2
    fi
    if (( lo < 1 || hi > 65535 || lo > hi )); then
      log "ERROR: NODE_PORTS_TCP entry '${item}' is outside 1-65535 or runs backwards." >&2
      exit 2
    fi
    for (( p = lo; p <= hi; p++ )); do
      out+=("${p}")
    done
  done
  if [[ ${#out[@]} -eq 0 ]]; then
    log "ERROR: NODE_PORTS_TCP names no port." >&2
    exit 2
  fi
  echo "${out[*]}"
}

# ---------------------------------------------------------------------------
# resolve_image — Print NODE_PORTS_IMAGE, or the image the node probe manifest
# pins when it is unset. Exits 2 when neither is available, with the error on
# stderr.
# ---------------------------------------------------------------------------
resolve_image() {
  if [[ -n "${NODE_PORTS_IMAGE}" ]]; then
    echo "${NODE_PORTS_IMAGE}"
    return 0
  fi
  local manifest="${REPO_ROOT}/deploy/lab/metal-stack/probe/node-probe.yaml"
  if [[ ! -f "${manifest}" ]]; then
    log "ERROR: NODE_PORTS_IMAGE is unset and deploy/lab/metal-stack/probe/node-probe.yaml is missing." >&2
    exit 2
  fi
  local image
  image="$(grep -oE 'image: [^ ]+' "${manifest}" | head -n 1 | cut -d' ' -f2)"
  if [[ -z "${image}" ]]; then
    log "ERROR: NODE_PORTS_IMAGE is unset and deploy/lab/metal-stack/probe/node-probe.yaml names no image." >&2
    exit 2
  fi
  echo "${image}"
}

# ---------------------------------------------------------------------------
# pod_json NAME ROLE NODE IMAGE COMMAND_JSON — The Pod manifest both roles share:
# host network, pinned to NODE, never restarted, no ServiceAccount token, one
# unprivileged container running COMMAND_JSON (a JSON array).
# ---------------------------------------------------------------------------
pod_json() {
  jq -n --arg name "$1" --arg role "$2" --arg node "$3" --arg image "$4" \
    --arg ns "${NODE_PORTS_NAMESPACE}" --arg lkey "${POD_LABEL_KEY}" --arg lval "${POD_LABEL_VALUE}" \
    --argjson command "$5" '{
    apiVersion: "v1",
    kind: "Pod",
    metadata: {
      name: $name,
      namespace: $ns,
      labels: {($lkey): $lval, "lab-node-ports/role": $role}
    },
    spec: {
      nodeName: $node,
      hostNetwork: true,
      restartPolicy: "Never",
      automountServiceAccountToken: false,
      securityContext: {
        runAsNonRoot: true,
        runAsUser: 65534,
        runAsGroup: 65534,
        seccompProfile: {type: "RuntimeDefault"}
      },
      containers: [{
        name: $role,
        image: $image,
        command: $command,
        securityContext: {
          allowPrivilegeEscalation: false,
          capabilities: {drop: ["ALL"]},
          readOnlyRootFilesystem: true
        },
        resources: {requests: {cpu: "10m", memory: "32Mi"}, limits: {memory: "128Mi"}}
      }]
    }
  }'
}

# ---------------------------------------------------------------------------
# cleanup — EXIT trap: delete every Pod this script creates, whatever the path.
# ---------------------------------------------------------------------------
cleanup() {
  kubectl delete pod -n "${NODE_PORTS_NAMESPACE}" -l "${POD_LABEL}" \
    --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
}

# ---------------------------------------------------------------------------
# wait_for_phase NAME DEADLINE WAIT — Poll the Pod's phase every two seconds
# until it is Succeeded or Failed or the epoch second DEADLINE passes. WAIT is
# the length of the wait in seconds, for the warning. `kubectl wait` cannot wait
# for either of two phases.
# ---------------------------------------------------------------------------
wait_for_phase() {
  local name="$1" deadline="$2" wait="$3" phase
  while true; do
    phase="$(kubectl get pod "${name}" -n "${NODE_PORTS_NAMESPACE}" \
      -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    if [[ "${phase}" == "Succeeded" || "${phase}" == "Failed" ]]; then
      return 0
    fi
    if (( $(date +%s) >= deadline )); then
      log "WARNING: Pod ${name} did not finish within ${wait}s (phase '${phase:-unknown}')."
      return 0
    fi
    sleep 2
  done
}

main() {
  local cmd
  for cmd in kubectl jq; do
    if ! command -v "${cmd}" >/dev/null 2>&1; then
      log "ERROR: '${cmd}' is not installed or not in PATH."
      exit 2
    fi
  done
  local knob
  for knob in NODE_PORTS_CONNECT_TIMEOUT NODE_PORTS_POD_TIMEOUT; do
    if [[ ! "${!knob}" =~ ^[1-9][0-9]*$ ]]; then
      log "ERROR: ${knob}='${!knob}' is not a positive number of seconds."
      exit 2
    fi
  done

  trap cleanup EXIT

  local ports image port_list=()
  ports="$(expand_ports "${NODE_PORTS_TCP}")"
  image="$(resolve_image)"
  read -r -a port_list <<<"${ports}"
  local port_count="${#port_list[@]}"

  # The nodes and their first InternalIPs of either family, the address
  # status.hostIP (and so live_migration_inbound_addr) resolves to.
  local selector=()
  if [[ -n "${NODE_PORTS_NODE_SELECTOR}" ]]; then
    selector=(-l "${NODE_PORTS_NODE_SELECTOR}")
  fi
  local nodes_json
  if ! nodes_json="$(kubectl get nodes ${selector[@]+"${selector[@]}"} -o json)"; then
    log "ERROR: cannot list the nodes (kubectl's error is above)."
    exit 2
  fi
  local names=() ips=() name ip
  while read -r name ip; do
    if [[ -z "${name}" ]]; then
      continue
    fi
    if [[ -z "${ip}" ]]; then
      log "ERROR: node ${name} reports no InternalIP."
      exit 2
    fi
    # The listener binds 0.0.0.0, so a node listing IPv6 first cannot be
    # checked on the address migration dials.
    if [[ "${ip}" == *:* ]]; then
      log "ERROR: node ${name}'s first InternalIP is IPv6; live migration targets it (status.hostIP) and the listener binds IPv4 only."
      exit 2
    fi
    # A node writes its own status, and the address ends up in the client's
    # shell command: anything but a dotted quad is refused.
    if [[ ! "${ip}" =~ ^[0-9]+(\.[0-9]+){3}$ ]]; then
      log "ERROR: node ${name} reports an InternalIP that is not an address: '${ip}'."
      exit 2
    fi
    names+=("${name}")
    ips+=("${ip}")
  done < <(jq -r '.items[] | "\(.metadata.name) \(([.status.addresses[]? | select(.type == "InternalIP") | .address] | first) // "")"' <<<"${nodes_json}")
  if [[ ${#names[@]} -lt 2 ]]; then
    log "ERROR: the check needs at least two nodes, found ${#names[@]}."
    exit 2
  fi

  log "Checking TCP ${NODE_PORTS_TCP} (${port_count} ports) between ${#names[@]} nodes with ${image}"

  # A run whose EXIT trap never ran (SIGKILL) or whose delete failed leaves its
  # Pods behind, and their logs would be read as this run's results. They are
  # removed first, and the Pods are created rather than applied, so one that
  # still exists fails the run instead of being reused.
  if ! kubectl delete pod -n "${NODE_PORTS_NAMESPACE}" -l "${POD_LABEL}" \
    --ignore-not-found --wait --timeout="${NODE_PORTS_POD_TIMEOUT}s" >/dev/null; then
    log "ERROR: cannot remove the Pods of an earlier run (kubectl's error is above)."
    exit 2
  fi

  # Listeners, one per node, in one create.
  local i j listener_cmd
  listener_cmd="$(jq -nc --arg script "${LISTENER_SCRIPT}" \
    '$ARGS.positional as $ports | ["perl", "-e", $script, "--"] + $ports' --args "${port_list[@]}")"
  if ! for (( i = 0; i < ${#names[@]}; i++ )); do
    pod_json "lab-node-ports-listen-${names[i]}" listener "${names[i]}" "${image}" "${listener_cmd}"
  done | kubectl create -f - >/dev/null; then
    log "ERROR: cannot create the listener Pods (kubectl's error is above)."
    exit 2
  fi
  local wait_out
  if ! wait_out="$(kubectl wait --for=condition=Ready pod -l lab-node-ports/role=listener \
    -n "${NODE_PORTS_NAMESPACE}" --timeout="${NODE_PORTS_POD_TIMEOUT}s" 2>&1)"; then
    log "ERROR: the listeners did not become Ready within ${NODE_PORTS_POD_TIMEOUT}s:"
    log "         ${wait_out}"
    exit 2
  fi

  # Every bind is tried before `listening` is printed, so the bind failures are
  # known once the line is there.
  local deadline listener_log unbound=()
  deadline=$(( $(date +%s) + NODE_PORTS_POD_TIMEOUT ))
  for (( i = 0; i < ${#names[@]}; i++ )); do
    while true; do
      listener_log="$(kubectl logs "lab-node-ports-listen-${names[i]}" -n "${NODE_PORTS_NAMESPACE}" 2>/dev/null || true)"
      if grep -qx 'listening' <<<"${listener_log}"; then
        break
      fi
      if (( $(date +%s) >= deadline )); then
        log "ERROR: listener lab-node-ports-listen-${names[i]} never reported 'listening'."
        exit 2
      fi
      sleep 2
    done
    unbound[i]="$(sed -n 's/^bind failed //p' <<<"${listener_log}" | tr '\n' ' ')"
  done

  # Clients, one per ordered pair, all at once in one create.
  local client_script client_cmd
  if ! for (( i = 0; i < ${#names[@]}; i++ )); do
    for (( j = 0; j < ${#names[@]}; j++ )); do
      if (( i == j )); then
        continue
      fi
      client_script="for p in ${ports}; do if timeout ${NODE_PORTS_CONNECT_TIMEOUT} bash -c \"exec 3<>/dev/tcp/${ips[j]}/\${p}\" 2>/dev/null; then echo \"\${p} open\"; else echo \"\${p} closed\"; fi; done; exit 0"
      client_cmd="$(jq -nc --arg script "${client_script}" '["bash", "-c", $script]')"
      pod_json "lab-node-ports-client-${names[i]}-${names[j]}" client "${names[i]}" "${image}" "${client_cmd}"
    done
  done | kubectl create -f - >/dev/null; then
    log "ERROR: cannot create the client Pods (kubectl's error is above)."
    exit 2
  fi

  # Results, one line per pair. A client probes its ports one after another,
  # and each probe runs for the full connect timeout when a firewall drops the
  # packets, so the client wait grows with the port count.
  local failed_pairs=0 pairs=0 client client_log open closed bind_failed no_result p
  local open_ports closed_ports client_wait
  client_wait=$(( NODE_PORTS_POD_TIMEOUT + port_count * NODE_PORTS_CONNECT_TIMEOUT ))
  deadline=$(( $(date +%s) + client_wait ))
  echo ""
  for (( i = 0; i < ${#names[@]}; i++ )); do
    for (( j = 0; j < ${#names[@]}; j++ )); do
      if (( i == j )); then
        continue
      fi
      client="lab-node-ports-client-${names[i]}-${names[j]}"
      wait_for_phase "${client}" "${deadline}" "${client_wait}"
      client_log="$(kubectl logs "${client}" -n "${NODE_PORTS_NAMESPACE}" 2>/dev/null || true)"
      open_ports=" $(sed -n 's/^\([0-9]*\) open$/\1/p' <<<"${client_log}" | tr '\n' ' ') "
      closed_ports=" $(sed -n 's/^\([0-9]*\) closed$/\1/p' <<<"${client_log}" | tr '\n' ' ') "
      open=0
      closed=""
      bind_failed=""
      no_result=""
      for p in "${port_list[@]}"; do
        if [[ "${open_ports}" == *" ${p} "* ]]; then
          open=$((open + 1))
        elif [[ " ${unbound[j]} " == *" ${p} "* ]]; then
          bind_failed+=" ${p}"
        elif [[ "${closed_ports}" == *" ${p} "* ]]; then
          closed+=" ${p}"
        else
          no_result+=" ${p}"
        fi
      done
      local line="${names[i]} (${ips[i]}) -> ${names[j]} (${ips[j]}): ${open}/${port_count} open"
      if [[ -n "${closed}" ]]; then line+=", closed:${closed}"; fi
      if [[ -n "${bind_failed}" ]]; then line+=", listener bind failed:${bind_failed}"; fi
      if [[ -n "${no_result}" ]]; then line+=", no result:${no_result}"; fi
      echo "${line}"
      pairs=$((pairs + 1))
      if (( open != port_count )); then
        failed_pairs=$((failed_pairs + 1))
      fi
    done
  done
  echo ""

  if (( failed_pairs > 0 )); then
    log "FAIL: ${failed_pairs} of ${pairs} node pairs have a port that is not open."
    exit 1
  fi
  log "OK: all ${port_count} ports are open in both directions between all ${#names[@]} nodes (${pairs} pairs)."
}

# Run main only when executed directly so unit tests (tests/unit/hack/) can
# source this script and exercise expand_ports and resolve_image in isolation.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
