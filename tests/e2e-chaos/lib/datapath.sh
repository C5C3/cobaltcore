#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# OVN data-path probes of the multinode chaos leg: logical switch ports
# plugged into network namespaces on the nodes, and pings between them that
# run through a fault.
#
# Run from the suite directory as ../lib/datapath.sh, with NAMESPACE set by
# Chainsaw. A port is built the way ../../e2e-ovn-overlay/geneve-datapath/
# builds one: an internal OVS port on br-int carrying the iface-id of the
# logical switch port, moved into a network namespace that configures the
# port's MAC and IP.
#
# Usage:
#   datapath.sh probe <node>
#   datapath.sh nb <central> <ovn-nbctl arguments...>
#   datapath.sh sb <central> <ovn-sbctl arguments...>
#   datapath.sh plug <node> <lsp> <port> <netns> <mac> <ip>
#   datapath.sh ping-start <pair> <node> <netns> <peer-ip>
#   datapath.sh ping-wait <pair> <seconds>
#   datapath.sh ping-verdict <suite> <pair>
#   datapath.sh ping-dark <pair> <seconds>
#   datapath.sh dump
#   datapath.sh teardown [<central>]
#
# probe         Starts the privileged hostNetwork pod chaos-dp-<node>, pinned
#               with nodeName, that mounts the node's /run/openvswitch and
#               /run/netns (Bidirectional). A pod of that name that is not
#               Ready is replaced: after a node restart it holds nothing.
# nb, sb        Runs ovn-nbctl or ovn-sbctl in a one-shot pod over the client
#               certificate <central>-client, against the internal address the
#               OVNCentral publishes, with --timeout=10 unless an argument sets
#               one. Connection-level failures are retried, every other
#               failure is not. Prints the command's output and exits 0; on
#               any failure prints the pod's log and exits 1.
# plug          Waits up to 120 s for br-int in chaos-dp-<node>, then attaches
#               <port> to <lsp> in <netns> with <mac> and <ip>/24. Idempotent;
#               the pod's annotation chaos.c5c3.io/plugs records the port for
#               teardown.
# ping-start    Starts pod chaos-dp-ping-<pair> on <node>, which runs
#               `ip netns exec <netns> ping -D -O -i 0.5 -W 1 -e <id> <peer-ip>`
#               until it is deleted. Its log is the record. <id> is the
#               smallest ICMP identifier no other ping pod in NAMESPACE holds;
#               the pod's annotation chaos.c5c3.io/icmp-id records it.
# ping-wait     Waits up to <seconds> for a reply on <pair> that arrived after
#               the call.
# ping-verdict  Counts the sequence numbers lost on <pair> from the first reply
#               to the last packet sent, less one still in flight, so replies
#               that stop for good count as lost. Reports the longest gap
#               between two replies and fails above the lost budget of the
#               line `# Chaos datapath budget: <pair> lost=<n>` in
#               <suite>/chainsaw-test.yaml. A missing budget line or an empty
#               record fails.
# ping-dark     Fails when a reply on <pair> arrived within the last <seconds>.
# dump          Prints the datapath pods, OVS and the namespaces on each probe
#               node, and the tail of each ping record. Always exits 0.
# teardown      Removes every recorded port and namespace, deletes every
#               datapath pod, and with <central> the switch chaos-dp-sw.
#               Always exits 0.
#
# Every pod carries the label app.kubernetes.io/name=chaos-datapath, which no
# chaos selector matches.
#
# Environment overrides:
#   CHAOS_DATAPATH_IMAGE         the probe image (the image of the OVNCentral's
#                                Northbound StatefulSet in NAMESPACE)
#   CHAOS_DATAPATH_SUITES_DIR    where ping-verdict reads <suite>/ (..)
#   CHAOS_DATAPATH_POLL_SECONDS  the poll interval of ping-wait (2)
#   CHAOS_DATAPATH_ATTEMPTS      the connection attempts of nb and sb (30)
#
# docs/reference/testing/chaos-e2e-tests.md describes the probes (Test
# Patterns, Node faults).

set -euo pipefail

LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUITES_DIR="${CHAOS_DATAPATH_SUITES_DIR:-${LIB_DIR}/..}"
POLL_SECONDS="${CHAOS_DATAPATH_POLL_SECONDS:-2}"
ATTEMPTS="${CHAOS_DATAPATH_ATTEMPTS:-30}"
NAMESPACE="${NAMESPACE:-}"
APP=chaos-datapath
SWITCH=chaos-dp-sw
PLUGS_KEY=chaos.c5c3.io/plugs
ID_RE='^[a-z0-9]([-a-z0-9]{0,40}[a-z0-9])?$'
# A reply line of ping -D: "[<epoch>.<frac>] 64 bytes from <ip>: icmp_seq=<n> ...".
REPLY_RE='^\[([0-9]+\.[0-9]+)\] [0-9]+ bytes from .*icmp_seq=([0-9]+)'
# The connection-level failures nb and sb retry, the vocabulary of
# ../../e2e-ovn-overlay/geneve-datapath/03-probe-scripts.yaml. A TLS failure
# reports a certificate or protocol error instead and is not retried.
RETRYABLE='Connection refused|Connection timed out|Network is unreachable|No route to host|Host is unreachable|Name or service not known|timeout exceeded'

usage() {
  echo "usage: datapath.sh probe <node>" >&2
  echo "       datapath.sh nb|sb <central> <arguments...>" >&2
  echo "       datapath.sh plug <node> <lsp> <port> <netns> <mac> <ip>" >&2
  echo "       datapath.sh ping-start <pair> <node> <netns> <peer-ip>" >&2
  echo "       datapath.sh ping-wait|ping-dark <pair> <seconds>" >&2
  echo "       datapath.sh ping-verdict <suite> <pair>" >&2
  echo "       datapath.sh dump" >&2
  echo "       datapath.sh teardown [<central>]" >&2
  exit 2
}

need_namespace() {
  if [ -z "$NAMESPACE" ]; then
    echo "datapath.sh: NAMESPACE is not set; run it from a Chainsaw script" >&2
    exit 2
  fi
}

# need_id <what> <value>: refuse a value that cannot name a pod.
need_id() {
  if ! [[ "$2" =~ $ID_RE ]]; then
    echo "datapath.sh: invalid $1 '$2': it must match ${ID_RE}" >&2
    exit 2
  fi
}

need_seconds() {
  if ! [[ "$1" =~ ^[0-9]+$ ]]; then
    echo "datapath.sh: '$1' is no number of seconds" >&2
    exit 2
  fi
}

# json_list <word...>: the words as a JSON list, for a container command. An
# argument never enters a script, whatever it holds.
json_list() {
  python3 -c 'import json, sys; print(json.dumps(sys.argv[1:]))' "$@"
}

probe_pod() {
  echo "chaos-dp-$1"
}

ping_pod() {
  echo "chaos-dp-ping-$1"
}

# image: the probe image, by default the Northbound image of the OVNCentral.
image() {
  local img="${CHAOS_DATAPATH_IMAGE:-}"
  if [ -z "$img" ]; then
    img="$(kubectl get sts -n "$NAMESPACE" -l app.kubernetes.io/name=ovncentral,app.kubernetes.io/component=nb \
      -o jsonpath='{.items[0].spec.template.spec.containers[0].image}' 2>/dev/null || true)"
  fi
  if [ -z "$img" ]; then
    echo "FAIL: no OVNCentral Northbound StatefulSet in ${NAMESPACE} names the probe image; set CHAOS_DATAPATH_IMAGE" >&2
    return 1
  fi
  echo "$img"
}

# privileged_pod <name> <node> <role> <image> <netns propagation> <command json>
# [<annotations json>] — the manifest of a hostNetwork pod on <node> that runs
# as root.
privileged_pod() {
  local annotations="${7:-}"
  [ -n "$annotations" ] || annotations='{}'
  cat <<EOF
{"apiVersion": "v1", "kind": "Pod",
 "metadata": {"name": "$1", "namespace": "${NAMESPACE}",
   "labels": {"app.kubernetes.io/name": "${APP}", "chaos.c5c3.io/datapath": "$3"},
   "annotations": ${annotations}},
 "spec": {"nodeName": "$2", "hostNetwork": true, "restartPolicy": "Never",
   "terminationGracePeriodSeconds": 1, "automountServiceAccountToken": false,
   "containers": [{"name": "probe", "image": "$4", "command": $6,
     "securityContext": {"privileged": true, "runAsUser": 0},
     "resources": {"requests": {"cpu": "10m", "memory": "16Mi"}, "limits": {"memory": "64Mi"}},
     "volumeMounts": [{"name": "run-ovs", "mountPath": "/run/openvswitch"},
                      {"name": "run-netns", "mountPath": "/run/netns", "mountPropagation": "$5"}]}],
   "volumes": [{"name": "run-ovs", "hostPath": {"path": "/run/openvswitch", "type": "DirectoryOrCreate"}},
               {"name": "run-netns", "hostPath": {"path": "/run/netns", "type": "DirectoryOrCreate"}}]}}
EOF
}

cmd_probe() {
  [ $# -eq 1 ] && [ -n "$1" ] || usage
  need_namespace
  local node="$1" pod ready img
  pod="$(probe_pod "$node")"
  ready="$(kubectl get pod "$pod" -n "$NAMESPACE" \
    -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
  if [ "$ready" = True ]; then
    echo "OK: ${pod} is Ready on ${node}"
    return 0
  fi
  kubectl delete pod "$pod" -n "$NAMESPACE" --ignore-not-found --wait=true --timeout=60s >/dev/null
  img="$(image)" || exit 1
  privileged_pod "$pod" "$node" probe "$img" Bidirectional '["sleep", "infinity"]' | kubectl apply -f - >/dev/null
  if ! kubectl wait --for=condition=Ready "pod/${pod}" -n "$NAMESPACE" --timeout=180s >/dev/null; then
    echo "FAIL: ${pod} is not Ready on ${node} within 180s"
    kubectl describe pod "$pod" -n "$NAMESPACE" 2>&1 | tail -20 || true
    exit 1
  fi
  echo "OK: ${pod} is Ready on ${node}"
}

# The script of the nb and sb pods. It retries the connection-level failures,
# prints the attempt's stderr, a marker line and its stdout when it succeeded,
# and exits with the command's status. The arguments arrive as "$@", never inside
# the script.
# shellcheck disable=SC2016 # expanded in the pod
CTL_SCRIPT='
set -u
attempt=0
while :; do
  attempt=$((attempt + 1))
  out="$("$TOOL" --db="$DB" -p /certs/tls.key -c /certs/tls.crt -C /certs/ca.crt "$@" 2>/tmp/err)"
  rc=$?
  err="$(cat /tmp/err)"
  if [ "$rc" -eq 0 ]; then
    [ -z "$err" ] || printf "%s\n" "$err"
    echo "@@OUTPUT@@"
    [ -z "$out" ] || printf "%s\n" "$out"
    exit 0
  fi
  if [ "$attempt" -lt "$ATTEMPTS" ] && grep -Eq "$RETRYABLE" <<<"$err"; then
    echo "retry ${attempt}/${ATTEMPTS}: ${err}"
    sleep 5
    continue
  fi
  echo "$TOOL exited ${rc} after ${attempt} attempts: ${out}${out:+ }${err}"
  exit "$rc"
done
'

# cmd_ctl nb|sb <central> <arguments...>
cmd_ctl() {
  local which="$1"
  shift
  [ $# -ge 2 ] && [ -n "$1" ] || usage
  need_namespace
  local central="$1" tool field addr img pod arg timeout_set=false args phase log deadline
  shift
  case "$which" in
    nb) tool=ovn-nbctl field=northbound ;;
    sb) tool=ovn-sbctl field=southbound ;;
  esac
  addr="$(kubectl get ovncentral "$central" -n "$NAMESPACE" -o "jsonpath={.status.${field}.internalDbAddress}" 2>/dev/null || true)"
  if [ -z "$addr" ]; then
    echo "FAIL: OVNCentral ${central} publishes no ${field} address"
    exit 1
  fi
  img="$(kubectl get sts "${central}-nb" -n "$NAMESPACE" -o 'jsonpath={.spec.template.spec.containers[0].image}' 2>/dev/null || true)"
  img="${CHAOS_DATAPATH_IMAGE:-$img}"
  if [ -z "$img" ]; then
    echo "FAIL: StatefulSet ${central}-nb names no image"
    exit 1
  fi
  for arg in "$@"; do
    case "$arg" in --timeout=*) timeout_set=true ;; esac
  done
  if [ "$timeout_set" = false ]; then
    set -- --timeout=10 "$@"
  fi
  args="$(json_list bash -c "$CTL_SCRIPT" _ "$@")"
  pod="chaos-dp-${which}-$(date +%s)-${RANDOM}"
  kubectl apply -f - >/dev/null <<EOF
{"apiVersion": "v1", "kind": "Pod",
 "metadata": {"name": "${pod}", "namespace": "${NAMESPACE}",
   "labels": {"app.kubernetes.io/name": "${APP}", "chaos.c5c3.io/datapath": "client"}},
 "spec": {"restartPolicy": "Never", "automountServiceAccountToken": false,
   "containers": [{"name": "ctl", "image": "${img}", "command": ${args},
     "env": [{"name": "TOOL", "value": "${tool}"}, {"name": "DB", "value": "${addr}"},
             {"name": "ATTEMPTS", "value": "${ATTEMPTS}"}, {"name": "RETRYABLE", "value": "${RETRYABLE}"}],
     "resources": {"requests": {"cpu": "10m", "memory": "16Mi"}, "limits": {"memory": "64Mi"}},
     "securityContext": {"allowPrivilegeEscalation": false, "capabilities": {"drop": ["ALL"]},
                         "seccompProfile": {"type": "RuntimeDefault"}},
     "volumeMounts": [{"name": "certs", "mountPath": "/certs", "readOnly": true}]}],
   "volumes": [{"name": "certs", "secret": {"secretName": "${central}-client"}}]}}
EOF
  deadline=$(($(date +%s) + ATTEMPTS * 20 + 120))
  while true; do
    phase="$(kubectl get pod "$pod" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    case "$phase" in Succeeded | Failed) break ;; esac
    if [ "$(date +%s)" -ge "$deadline" ]; then
      break
    fi
    sleep 1
  done
  log="$(kubectl logs "$pod" -n "$NAMESPACE" 2>&1 || true)"
  kubectl delete pod "$pod" -n "$NAMESPACE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  if [ "$phase" = Succeeded ]; then
    sed -n '/^@@OUTPUT@@$/,$p' <<<"$log" | sed 1d
    return 0
  fi
  echo "FAIL: ${tool} $* in pod ${pod} ended ${phase:-without a phase}:"
  printf '%s\n' "$log"
  exit 1
}

cmd_plug() {
  [ $# -eq 6 ] || usage
  need_namespace
  local node="$1" lsp="$2" port="$3" netns="$4" mac="$5" ip="$6" pod plugs
  pod="$(probe_pod "$node")"
  # shellcheck disable=SC2016 # expanded in the pod
  if ! kubectl exec -n "$NAMESPACE" "$pod" -- bash -c '
    set -eu
    lsp="$1"; port="$2"; ns="$3"; mac="$4"; addr="$5"
    i=0
    until ovs-vsctl --timeout=5 br-exists br-int; do
      i=$((i + 1))
      if [ "$i" -ge 60 ]; then
        echo "FAIL: br-int did not appear within 120s"
        exit 1
      fi
      sleep 2
    done
    ovs-vsctl --may-exist add-port br-int "$port" \
      -- set Interface "$port" type=internal "external_ids:iface-id=$lsp"
    [ -e "/run/netns/$ns" ] || ip netns add "$ns"
    ip netns exec "$ns" ip link show "$port" >/dev/null 2>&1 \
      || ip link set "$port" netns "$ns"
    ip netns exec "$ns" ip link set "$port" address "$mac"
    ip netns exec "$ns" ip addr replace "$addr/24" dev "$port"
    ip netns exec "$ns" ip link set "$port" up
  ' bash "$lsp" "$port" "$netns" "$mac" "$ip"; then
    echo "FAIL: plugging ${lsp} into ${netns} on ${node} failed"
    exit 1
  fi
  plugs="$(kubectl get pod "$pod" -n "$NAMESPACE" -o "jsonpath={.metadata.annotations.chaos\.c5c3\.io/plugs}")"
  case " ${plugs} " in
    *" ${port}:${netns} "*) ;;
    *) kubectl annotate pod "$pod" -n "$NAMESPACE" --overwrite \
      "${PLUGS_KEY}=$(xargs <<<"${plugs} ${port}:${netns}")" >/dev/null ;;
  esac
  echo "OK: ${lsp} answers as ${ip} in ${netns} on ${node}"
}

cmd_ping_start() {
  [ $# -eq 4 ] || usage
  need_namespace
  local pair="$1" node="$2" netns="$3" peer="$4" pod img cmd held id=1
  need_id pair "$pair"
  pod="$(ping_pod "$pair")"
  img="$(kubectl get pod "$(probe_pod "$node")" -n "$NAMESPACE" -o 'jsonpath={.spec.containers[0].image}' 2>/dev/null || true)"
  if [ -z "$img" ]; then
    echo "FAIL: no probe pod on ${node}; run datapath.sh probe ${node} first"
    exit 1
  fi
  kubectl delete pod "$pod" -n "$NAMESPACE" --ignore-not-found --wait=true --timeout=60s >/dev/null
  # Every ping in a network namespace receives every echo reply that reaches
  # it and keeps the ones that carry its identifier. Without -e, iputils takes
  # the process id for a raw socket (a fresh namespace allows no ping socket),
  # and that is 1 for a ping that runs as its container's PID 1. Two pairs
  # from one namespace would then count each other's replies, so each ping
  # takes an identifier no other ping pod holds.
  if ! held="$(kubectl get pods -n "$NAMESPACE" -l "app.kubernetes.io/name=${APP},chaos.c5c3.io/datapath=ping" \
    -o 'jsonpath={range .items[*]}{.metadata.annotations.chaos\.c5c3\.io/icmp-id}{"\n"}{end}')"; then
    echo "FAIL: listing the ping pods in ${NAMESPACE} failed"
    exit 1
  fi
  while grep -qx "$id" <<<"$held"; do
    id=$((id + 1))
  done
  cmd="$(json_list ip netns exec "$netns" ping -D -O -i 0.5 -W 1 -e "$id" "$peer")"
  privileged_pod "$pod" "$node" ping "$img" HostToContainer "$cmd" "{\"chaos.c5c3.io/icmp-id\": \"${id}\"}" |
    kubectl apply -f - >/dev/null
  if ! kubectl wait --for=condition=Ready "pod/${pod}" -n "$NAMESPACE" --timeout=180s >/dev/null; then
    echo "FAIL: ${pod} is not running on ${node} within 180s"
    kubectl logs "$pod" -n "$NAMESPACE" 2>&1 | tail -20 || true
    exit 1
  fi
  echo "OK: ${pod} pings ${peer} from ${netns} on ${node} with ICMP id ${id}"
}

# record <pair>: the ping log of the pair, or exit 1 naming the pod.
record() {
  local pod err out
  pod="$(ping_pod "$1")"
  err="$(mktemp)"
  if ! out="$(kubectl logs "$pod" -n "$NAMESPACE" 2>"$err")"; then
    echo "FAIL: cannot read the log of pod ${pod}: $(cat "$err")"
    rm -f "$err"
    exit 1
  fi
  rm -f "$err"
  printf '%s\n' "$out"
}

# replies: "<timestamp> <seq>" of every reply line on stdin.
replies() {
  sed -nE "s/${REPLY_RE}.*/\\1 \\2/p"
}

# now_of <pair>: the clock of the ping pod, or exit 1.
now_of() {
  local pod out
  pod="$(ping_pod "$1")"
  if ! out="$(kubectl exec "$pod" -n "$NAMESPACE" -- date +%s.%N 2>&1)" || ! [[ "$out" =~ ^[0-9]+(\.[0-9]+)?$ ]]; then
    echo "FAIL: reading the clock of pod ${pod} failed: ${out}"
    exit 1
  fi
  echo "$out"
}

cmd_ping_wait() {
  [ $# -eq 2 ] || usage
  need_namespace
  local pair="$1" secs="$2" since deadline log last
  need_id pair "$pair"
  need_seconds "$secs"
  since="$(now_of "$pair")" || {
    echo "$since"
    exit 1
  }
  deadline=$(($(date +%s) + secs))
  while true; do
    log="$(record "$pair")" || {
      echo "$log"
      exit 1
    }
    last="$(replies <<<"$log" | tail -1 | cut -d' ' -f1)"
    if [ -n "$last" ] && awk -v a="$last" -v b="$since" 'BEGIN { exit !(a > b) }'; then
      echo "OK: ${pair} answered at ${last}"
      return 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "FAIL: no reply on ${pair} within ${secs} s"
      tail -5 <<<"$log"
      exit 1
    fi
    sleep "$POLL_SECONDS"
  done
}

cmd_ping_verdict() {
  [ $# -eq 2 ] || usage
  need_namespace
  local suite="$1" pair="$2" file lines budget log rows sent
  need_id pair "$pair"
  file="${SUITES_DIR}/${suite}/chainsaw-test.yaml"
  lines="$(grep -E "^# Chaos datapath budget: ${pair} lost=[0-9]+$" "$file" 2>/dev/null || true)"
  if [ -z "$lines" ]; then
    echo "FAIL: no budget line for ${pair}: want '# Chaos datapath budget: ${pair} lost=<n>' in ${file}"
    exit 1
  fi
  if [ "$(grep -c . <<<"$lines")" -gt 1 ]; then
    echo "FAIL: ${file} has more than one budget line for ${pair}"
    exit 1
  fi
  budget="${lines##*lost=}"
  log="$(record "$pair")" || {
    echo "$log"
    exit 1
  }
  rows="$(replies <<<"$log")"
  if [ -z "$rows" ]; then
    echo "FAIL: no reply recorded for ${pair}"
    tail -20 <<<"$log"
    exit 1
  fi
  # The highest sequence number of any line, a reply, a "no answer yet" or an
  # error, is the last packet sent. Every packet before it has had its time to
  # answer.
  sent="$(sed -nE 's/.*icmp_seq=([0-9]+).*/\1/p' <<<"$log" | sort -n | tail -1)"
  # One pass: the first and last sequence number, the distinct ones, the
  # longest gap between consecutive replies, and the verdict. The last one is
  # the last reply's, or the one before the last packet sent when that is later.
  awk -v pair="$pair" -v budget="$budget" -v sent="${sent:-0}" '
    NR == 1 { first = $2; prev = $1 }
    { if (!($2 in seen)) { seen[$2] = 1; distinct++ }
      if ($2 < first) first = $2
      if ($2 > last) last = $2
      if ($1 - prev > gap) gap = $1 - prev
      prev = $1 }
    END {
      if (sent - 1 > last) last = sent - 1
      lost = (last - first + 1) - distinct
      printf "datapath %s: %d replies, seq %d-%d, lost %d (budget %d), longest gap %.1f s\n",
        pair, distinct, first, last, lost, budget, gap
      if (lost > budget) {
        printf "FAIL: datapath %s lost %d, over its budget of %d\n", pair, lost, budget
        exit 1
      }
      printf "DATAPATH-OK %s\n", pair
    }' <<<"$rows"
}

cmd_ping_dark() {
  [ $# -eq 2 ] || usage
  need_namespace
  local pair="$1" secs="$2" log last now
  need_id pair "$pair"
  need_seconds "$secs"
  log="$(record "$pair")" || {
    echo "$log"
    exit 1
  }
  now="$(now_of "$pair")" || {
    echo "$now"
    exit 1
  }
  last="$(replies <<<"$log" | tail -1 | cut -d' ' -f1)"
  if [ -z "$last" ]; then
    echo "DARK-OK ${pair}: no reply at all"
    return 0
  fi
  awk -v pair="$pair" -v secs="$secs" -v last="$last" -v now="$now" 'BEGIN {
    ago = now - last
    if (ago < secs) {
      printf "FAIL: a reply on %s arrived %.1f s ago, inside the %d-second dark window\n", pair, ago, secs
      exit 1
    }
    printf "DARK-OK %s: last reply %.1f s ago\n", pair, ago
  }'
}

cmd_dump() {
  local pod
  echo "=== chaos datapath pods ==="
  kubectl get pods -n "$NAMESPACE" -l "app.kubernetes.io/name=${APP}" -o wide 2>&1 || true
  for pod in $(kubectl get pods -n "$NAMESPACE" -l "app.kubernetes.io/name=${APP},chaos.c5c3.io/datapath=probe" -o name 2>/dev/null); do
    echo "--- ${pod} ---"
    kubectl exec --request-timeout=30s -n "$NAMESPACE" "$pod" -- ovs-vsctl --timeout=5 show 2>&1 || true
    kubectl exec --request-timeout=30s -n "$NAMESPACE" "$pod" -- ip netns list 2>&1 || true
  done
  for pod in $(kubectl get pods -n "$NAMESPACE" -l "app.kubernetes.io/name=${APP},chaos.c5c3.io/datapath=ping" -o name 2>/dev/null); do
    echo "--- ${pod}: last 40 lines ---"
    kubectl logs -n "$NAMESPACE" "$pod" --tail=40 2>&1 || true
  done
  exit 0
}

cmd_teardown() {
  [ $# -le 1 ] || usage
  local pod plugs
  for pod in $(kubectl get pods -n "$NAMESPACE" -l "app.kubernetes.io/name=${APP},chaos.c5c3.io/datapath=probe" -o name 2>/dev/null || true); do
    plugs="$(kubectl get -n "$NAMESPACE" "$pod" -o "jsonpath={.metadata.annotations.chaos\.c5c3\.io/plugs}" 2>/dev/null || true)"
    [ -n "$plugs" ] || continue
    # shellcheck disable=SC2016,SC2086 # expanded in the pod; one plug per word
    kubectl exec --request-timeout=30s -n "$NAMESPACE" "$pod" -- bash -c '
      for plug in "$@"; do
        ip netns del "${plug#*:}" >/dev/null 2>&1 || true
        ovs-vsctl --timeout=5 --if-exists del-port br-int "${plug%%:*}" || true
      done
    ' bash $plugs 2>&1 || true
  done
  if [ $# -eq 1 ] && [ -n "$1" ]; then
    (
      ATTEMPTS=6
      cmd_ctl nb "$1" --if-exists ls-del "$SWITCH"
    ) || true
  fi
  kubectl delete pods -n "$NAMESPACE" -l "app.kubernetes.io/name=${APP}" --ignore-not-found --wait=false 2>&1 || true
  echo "OK: the datapath is torn down"
  exit 0
}

if [ $# -lt 1 ]; then
  usage
fi
command="$1"
shift
case "$command" in
  probe) cmd_probe "$@" ;;
  nb | sb) cmd_ctl "$command" "$@" ;;
  plug) cmd_plug "$@" ;;
  ping-start) cmd_ping_start "$@" ;;
  ping-wait) cmd_ping_wait "$@" ;;
  ping-verdict) cmd_ping_verdict "$@" ;;
  ping-dark) cmd_ping_dark "$@" ;;
  dump) cmd_dump ;;
  teardown) need_namespace && cmd_teardown "$@" ;;
  *) usage ;;
esac
