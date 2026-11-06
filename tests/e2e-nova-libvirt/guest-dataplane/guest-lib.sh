#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# The library of the guest data-plane suite (chainsaw-test.yaml beside it),
# sourced by every script step as `. ./guest-lib.sh` with NAMESPACE set to
# openstack.
#
# The probe server runs guest-user-data.sh, which writes one line per probe to
# the guest's serial console. Nova keeps that console as the console log:
#
#   guest-probe <kind> <seq> <HH:MM:SS UTC> <result> [<detail>]
#
# gl_console reads the log through the compute API, and gl_window judges the
# lines of one kind by their sequence numbers alone, so no verdict maps the
# guest's clock to the runner's. A fault passes when every line after the last
# one read before it says ok and no number is missing.
#
# Every check prints an OK: line on success, and a FAIL: line and returns 1
# otherwise. set -euo pipefail is the caller's, and no function relies on it.
# Every openstack call runs in the client pod nova-pool-guest-client, whose
# environment holds the bootstrap admin of keystone-nova-pool.

# The client pod of 06-client-pod.yaml.
GL_CLIENT=nova-pool-guest-client
# The suite's OpenStack resources (step 5 creates them).
GL_PROBE=nova-pool-guest-probe
GL_PEER=nova-pool-guest-peer
GL_VOLUME=nova-pool-guest-vol
GL_NET=nova-pool-guest-net
GL_SUBNET=nova-pool-guest-subnet
GL_PROBE_SG=nova-pool-guest-probe-sg
GL_PEER_SG=nova-pool-guest-peer-sg
# The tenant subnet of the two guests; the peer's ICMP rule admits it.
# shellcheck disable=SC2034 # read by chainsaw-test.yaml
GL_CIDR=10.241.0.0/24
# The cirros image 04-image-seed-job.yaml registers under this fixed id.
# shellcheck disable=SC2034 # read by chainsaw-test.yaml
GL_IMAGE=4e0a0000-0000-4000-8000-000000000010
# The ConfigMap the steps hand ids to each other through.
GL_STATE=nova-pool-guest
# The label every chaos object of the suite carries.
GL_CHAOS_LABEL=chaos.c5c3.io/suite=nova-libvirt-guest-dataplane
# Seconds between two polls of a wait. The unit test sets it to 0.
GL_POLL="${GL_POLL:-5}"
# A whole probe line. A line cut short by the read, or garbled by a kernel
# message on the same console, does not match.
GL_PROBE_RE='guest-probe (dhcp|metadata|net|disk) [0-9]+ [0-9]{2}:[0-9]{2}:[0-9]{2} (ok|lost|error|none)( .*)?$'

# gl_os <args...>: the openstack CLI in the client pod.
gl_os() {
  kubectl exec -n "$NAMESPACE" "$GL_CLIENT" -- openstack "$@"
}

# gl_record <key> <value>: write a value into the suite's ConfigMap.
gl_record() {
  kubectl patch configmap "$GL_STATE" -n "$NAMESPACE" --type=merge \
    -p "{\"data\":{\"$1\":\"$2\"}}" >/dev/null
}

# gl_recall <key>: a value of the suite's ConfigMap; returns 1 naming the key
# when it is absent.
gl_recall() {
  local value
  value="$(kubectl get configmap "$GL_STATE" -n "$NAMESPACE" -o json |
    jq -r --arg k "$1" '.data[$k] // empty')" || value=""
  if [ -z "$value" ]; then
    echo "FAIL: ConfigMap $GL_STATE holds no $1"
    return 1
  fi
  echo "$value"
}

# gl_wait_status <server|volume> <name> <status> <seconds>: polls the status
# until it reads the wanted one. A server in ERROR or a volume in an error
# state ends the wait at once, a server with its fault.
gl_wait_status() {
  local kind="$1" name="$2" want="$3" seconds="$4" end status fault=""
  end=$((SECONDS + seconds))
  while :; do
    status="$(gl_os "$kind" show "$name" -f value -c status 2>/dev/null)" || status="unreadable"
    if [ "$status" = "$want" ]; then
      echo "OK: $kind $name is $want"
      return 0
    fi
    case "$status" in
      ERROR | error*)
        if [ "$kind" = server ]; then
          fault="$(gl_os server show "$name" -f value -c fault 2>&1)" || true
        fi
        echo "FAIL: $kind $name is $status${fault:+: $fault}"
        return 1
        ;;
    esac
    if [ "$SECONDS" -ge "$end" ]; then
      echo "FAIL: $kind $name is $status, not $want, after $seconds s"
      return 1
    fi
    sleep "$GL_POLL"
  done
}

# gl_wait_compute_up <host> <seconds> <since>: the nova-compute service of
# the host reads up, and its last report is later than since, a UTC time in
# the form `date -u +%Y-%m-%dT%H:%M:%S` prints. Nova reads a service as up for
# 60 s after its last report, so a nova-compute killed a moment ago still
# reads up; only a report after since shows a live one. The runner and the
# kind node share one clock.
gl_wait_compute_up() {
  local host="$1" seconds="$2" since="$3" end out state updated at
  end=$((SECONDS + seconds))
  while :; do
    out="$(gl_os compute service list --service nova-compute --host "$host" \
      -f value -c State -c "Updated At" 2>/dev/null)" || out="unreadable"
    read -r state updated <<<"$out"
    # The first 14 digits of the time, without its fraction, compare as a
    # number: 2026-11-06T10:00:05.000000 is 20261106100005.
    at="${updated%%.*}"
    at="${at//[!0-9]/}"
    at="${at:0:14}"
    if [ "$state" = up ] && [ "${#at}" -eq 14 ] && [ "$at" -gt "${since//[!0-9]/}" ]; then
      echo "OK: the compute service of $host is up, last report at $updated"
      return 0
    fi
    if [ "$SECONDS" -ge "$end" ]; then
      echo "FAIL: the compute service of $host is ${state:-absent} with its last report at ${updated:-none}, not up with a report after $since, after $seconds s"
      return 1
    fi
    sleep "$GL_POLL"
  done
}

# gl_console <server>: the probe lines of the server's console log. The last
# line of the log is dropped, because the serial console writes a line in
# pieces and a read can end in the middle of one; the CLI ends its output in a
# newline either way, so a complete last line cannot be told apart.
gl_console() {
  local log
  log="$(gl_os console log show "$1" 2>/dev/null)" || log=""
  log="$(tr -d '\r' <<<"$log" | sed '$d' | grep -oE "$GL_PROBE_RE")" || log=""
  if [ -z "$log" ]; then
    echo "FAIL: no guest-probe line in the console log of $1"
    return 1
  fi
  printf '%s\n' "$log"
}

# gl_last_seq <kind>: the highest seq of the kind in the console on stdin.
gl_last_seq() {
  local seq
  seq="$(awk -v k="$1" '$1 == "guest-probe" && $2 == k && (!f || $3 + 0 > m) { m = $3 + 0; f = 1 }
    END { if (f) print m }')"
  if [ -z "$seq" ]; then
    echo "FAIL: no $1 line in the console log"
    return 1
  fi
  echo "$seq"
}

# gl_window <kind> <after> <min> [<result>]: passes when the console on stdin
# holds at least min lines of the kind with a seq above after, every one
# saying result (ok unless given), and their seqs run from after+1 without a
# gap.
gl_window() {
  local lines
  lines="$(awk -v k="$1" '$1 == "guest-probe" && $2 == k' | sort -s -n -k3,3)"
  if [ -z "$lines" ]; then
    echo "FAIL: no $1 line in the console log"
    return 1
  fi
  awk -v k="$1" -v after="$2" -v min="$3" -v want="${4:-ok}" '
    $3 + 0 > after + 0 { n++; seq[n] = $3 + 0; res[n] = $5; line[n] = $0 }
    END {
      for (i = 1; i <= n; i++) {
        if (res[i] != want) {
          printf "FAIL: %s line %d says %s: %s\n", k, seq[i], res[i], line[i]
          exit 1
        }
      }
      prev = after + 0
      for (i = 1; i <= n; i++) {
        if (seq[i] != prev + 1) {
          printf "FAIL: %s sequence jumps from %d to %d\n", k, prev, seq[i]
          exit 1
        }
        prev = seq[i]
      }
      if (n < min + 0) {
        printf "FAIL: %d %s lines after %d, want %d\n", n, k, after, min
        exit 1
      }
      printf "OK: %s %d..%d all %s\n", k, seq[1], seq[n], want
    }' <<<"$lines"
}

# gl_wait_window <server> <kind> <after> <min> <seconds> [<result>]: gl_window
# over the server's console every GL_POLL seconds. A failed read, as while
# nova-compute is down, and a window that is still short are polled on; a
# line that says another result or a gap ends the wait at once. On timeout it
# prints the last failure.
gl_wait_window() {
  local server="$1" kind="$2" after="$3" min="$4" seconds="$5" result="${6:-ok}" end out console
  end=$((SECONDS + seconds))
  while :; do
    if console="$(gl_console "$server")"; then
      if out="$(gl_window "$kind" "$after" "$min" "$result" <<<"$console")"; then
        echo "$out"
        return 0
      fi
      case "$out" in
        *" says "* | *" sequence jumps "*)
          echo "$out"
          return 1
          ;;
      esac
    else
      out="$console"
    fi
    if [ "$SECONDS" -ge "$end" ]; then
      echo "${out/FAIL: /FAIL: after $seconds s: }"
      return 1
    fi
    sleep "$GL_POLL"
  done
}

# gl_wait_result <server> <kind> <result> <after> <seconds>: the seq of the
# first line of the kind with a seq above after that says result.
gl_wait_result() {
  local server="$1" kind="$2" result="$3" after="$4" seconds="$5" end console seq
  end=$((SECONDS + seconds))
  while :; do
    if console="$(gl_console "$server")"; then
      seq="$(awk -v k="$kind" -v r="$result" -v a="$after" '
        $1 == "guest-probe" && $2 == k && $3 + 0 > a + 0 && $5 == r && (!f || $3 + 0 < m) { m = $3 + 0; f = 1 }
        END { if (f) print m }' <<<"$console")"
      if [ -n "$seq" ]; then
        echo "$seq"
        return 0
      fi
    fi
    if [ "$SECONDS" -ge "$end" ]; then
      echo "FAIL: no $kind $result line after $after within $seconds s"
      return 1
    fi
    sleep "$GL_POLL"
  done
}

# gl_last_ok_disk: the seq of the last ok disk line of a console log file
# read raw on stdin, with the line EOF appended (`cat <log> && echo EOF`). The
# last line is dropped: the marker alone when the log ends in a newline, the
# marker and the line a stop cut short when it does not.
gl_last_ok_disk() {
  local seq
  seq="$(sed '$d' | tr -d '\r' | grep -oE "$GL_PROBE_RE" |
    awk '$2 == "disk" && $5 == "ok" && (!f || $3 + 0 > m) { m = $3 + 0; f = 1 }
      END { if (f) print m }')" || seq=""
  if [ -z "$seq" ]; then
    echo "FAIL: no ok disk line in the console log"
    return 1
  fi
  echo "$seq"
}

# gl_backend_holds <value> <n>: the value read at byte 4096 of the volume file
# is probe-<n>, the counter of the last ok disk line, or probe-<n+1> when the
# stop fell between the write of n+1 and its line.
gl_backend_holds() {
  case "$1" in
    "probe-$2" | "probe-$(($2 + 1))")
      echo "OK: the volume file holds $1, the last ok disk line counted $2"
      ;;
    *)
      echo "FAIL: the volume file holds '$1', the last ok disk line counted $2"
      return 1
      ;;
  esac
}

# gl_libvirt_pod: the Running libvirt-e2e pod that is not terminating.
gl_libvirt_pod() {
  local pod
  pod="$(kubectl get pod -n "$NAMESPACE" -l app.kubernetes.io/name=libvirt-e2e -o json |
    jq -r '[.items[] | select(.status.phase == "Running" and .metadata.deletionTimestamp == null)
      | .metadata.name][0] // empty')" || pod=""
  if [ -z "$pod" ]; then
    echo "FAIL: no Running libvirt-e2e pod"
    return 1
  fi
  echo "$pod"
}

# The process scan gl_pids runs in the libvirt pod, as `bash -c` with the
# domain name as $1 and the proc root as $2. The libvirt image carries no
# pgrep, and nixpkgs wraps libvirtd, so the daemon's process name is
# .libvirtd-wrapp while its argv[0] is the libvirtd it was started as. QEMU
# carries `-name guest=<domain>,...`.
# shellcheck disable=SC2016 # expands in the pod
GL_PIDS_SCRIPT='
name="$1" proc="${2:-/proc}" qemu="" libvirtd=""
for dir in "$proc"/[0-9]*; do
  pid="${dir##*/}"
  [ "$pid" != "$$" ] || continue
  args=()
  mapfile -d "" -t args 2>/dev/null <"$dir/cmdline" || continue
  [ "${#args[@]}" -gt 0 ] || continue
  if [ "${args[0]##*/}" = libvirtd ]; then
    libvirtd="${libvirtd:+$libvirtd,}$pid"
  fi
  for arg in "${args[@]}"; do
    case "$arg" in
      "guest=$name,"*)
        qemu="${qemu:+$qemu,}$pid"
        break
        ;;
    esac
  done
done
echo "qemu=$qemu libvirtd=$libvirtd"
'

# gl_pids <instance_name>: `qemu=<pid> libvirtd=<pid>` as the libvirt pod sees
# them. Anything but one process of each fails, so two empty reads never
# compare equal.
gl_pids() {
  local name="$1" pod out qemu libvirtd n
  pod="$(gl_libvirt_pod)" || {
    echo "$pod"
    return 1
  }
  out="$(kubectl exec -n "$NAMESPACE" "$pod" -c libvirtd -- \
    bash -c "$GL_PIDS_SCRIPT" gl-pids "$name" 2>&1)" || {
    echo "FAIL: the process scan in $pod failed: $out"
    return 1
  }
  out="$(grep '^qemu=' <<<"$out")" || out=""
  qemu="${out#qemu=}"
  qemu="${qemu%% *}"
  libvirtd="${out##* libvirtd=}"
  n="$(tr ',' '\n' <<<"$qemu" | grep -c .)" || n=0
  if [ "$n" != 1 ]; then
    echo "FAIL: $n QEMU processes of $name in $pod: ${qemu:-none}"
    return 1
  fi
  n="$(tr ',' '\n' <<<"$libvirtd" | grep -c .)" || n=0
  if [ "$n" != 1 ]; then
    echo "FAIL: $n libvirtd processes in $pod: ${libvirtd:-none}"
    return 1
  fi
  echo "qemu=$qemu libvirtd=$libvirtd"
}

# gl_pid <qemu|libvirtd> <gl_pids output>: one PID of the line.
gl_pid() {
  local value="${2##*"$1"=}"
  echo "${value%% *}"
}

# gl_pod_kill <name> <k=v[,k=v]>: a PodChaos pod-kill of one pod the labels
# select, and the wait for AllInjected.
gl_pod_kill() {
  local name="$1" labels
  labels="$(tr ',' '\n' <<<"$2" | sed 's/^/      /; s/=/: /')"
  kubectl apply -f - <<EOF || return 1
apiVersion: chaos-mesh.org/v1alpha1
kind: PodChaos
metadata:
  name: ${name}
  namespace: ${NAMESPACE}
  labels:
    ${GL_CHAOS_LABEL%%=*}: ${GL_CHAOS_LABEL#*=}
spec:
  action: pod-kill
  mode: one
  selector:
    namespaces: [${NAMESPACE}]
    labelSelectors:
${labels}
EOF
  if ! kubectl wait "podchaos/$name" -n "$NAMESPACE" \
    --for=condition=AllInjected --timeout=120s >/dev/null 2>&1; then
    kubectl describe podchaos "$name" -n "$NAMESPACE" 2>&1 || true
    echo "FAIL: PodChaos $name did not reach AllInjected within 120 s"
    return 1
  fi
  echo "OK: PodChaos $name is AllInjected"
}

# gl_wait_replaced <selector> <uid> <seconds>: a pod of the selector with
# another UID that is not terminating, and then Ready, within the bound.
gl_wait_replaced() {
  local selector="$1" uid="$2" seconds="$3" start="$SECONDS" end pod left
  end=$((SECONDS + seconds))
  while :; do
    pod="$(kubectl get pod -n "$NAMESPACE" -l "$selector" -o json |
      jq -r --arg uid "$uid" '[.items[] | select(.metadata.uid != $uid and .metadata.deletionTimestamp == null)
        | .metadata.name + " " + .metadata.uid][0] // empty')" || pod=""
    if [ -n "$pod" ]; then
      left=$((end - SECONDS))
      [ "$left" -gt 0 ] || left=1
      if kubectl wait pod "${pod%% *}" -n "$NAMESPACE" --for=condition=Ready \
        --timeout="${left}s" >/dev/null 2>&1; then
        echo "OK: ${pod%% *} (${pod#* }) is Ready after $((SECONDS - start)) s"
        return 0
      fi
    fi
    [ "$SECONDS" -lt "$end" ] || break
    sleep "$GL_POLL"
  done
  echo "FAIL: no Ready pod of $selector other than $uid within $seconds s"
  return 1
}

# gl_guest_servers: the ids of the suite's two servers.
gl_guest_servers() {
  gl_os server list --name "^($GL_PROBE|$GL_PEER)\$" -f value -c ID
}

# gl_teardown_guests: deletes both servers and waits up to 300 s for them to
# go, then the volume once the delete has detached it, both security groups,
# the subnet and the network. A resource that is already gone is no failure.
gl_teardown_guests() {
  local end servers status
  gl_os server delete "$GL_PROBE" "$GL_PEER" >/dev/null 2>&1 || true
  end=$((SECONDS + 300))
  until servers="$(gl_guest_servers 2>&1)" && [ -z "$servers" ]; do
    if [ "$SECONDS" -ge "$end" ]; then
      echo "FAIL: $GL_PROBE or $GL_PEER still exists after 300 s: $servers"
      return 1
    fi
    sleep "$GL_POLL"
  done
  echo "OK: $GL_PROBE and $GL_PEER are gone"
  end=$((SECONDS + 120))
  while status="$(gl_os volume show "$GL_VOLUME" -f value -c status 2>/dev/null)"; do
    if [ "$status" = available ]; then
      gl_os volume delete "$GL_VOLUME" || {
        echo "FAIL: the delete of $GL_VOLUME failed"
        return 1
      }
      break
    fi
    if [ "$SECONDS" -ge "$end" ]; then
      echo "FAIL: $GL_VOLUME is still $status 120 s after the servers went"
      return 1
    fi
    sleep "$GL_POLL"
  done
  gl_os security group delete "$GL_PROBE_SG" >/dev/null 2>&1 || true
  gl_os security group delete "$GL_PEER_SG" >/dev/null 2>&1 || true
  gl_os subnet delete "$GL_SUBNET" >/dev/null 2>&1 || true
  gl_os network delete "$GL_NET" >/dev/null 2>&1 || true
  if [ -n "$(gl_os network list --name "$GL_NET" -f value -c ID 2>&1)" ]; then
    echo "FAIL: $GL_NET is left after the teardown"
    return 1
  fi
  echo "OK: the volume, the security groups and the network are gone"
}

# gl_dump: what a failed step leaves to read. Every command ends in `|| true`,
# so a diagnostic that fails never takes the place of the step's failure.
gl_dump() {
  local kind pod job ns
  for kind in novacompute nova neutron cinder cinderbackend neutronmetadataagent ovnchassis; do
    echo "=== $kind ==="
    kubectl get "$kind" -n "$NAMESPACE" -o yaml 2>&1 || true
  done
  for pod in $(kubectl get pod -n "$NAMESPACE" -o name \
    -l app.kubernetes.io/name=libvirt-e2e 2>/dev/null || true); do
    echo "=== $pod ==="
    kubectl describe -n "$NAMESPACE" "$pod" 2>&1 | tail -40 || true
    kubectl logs -n "$NAMESPACE" "$pod" -c libvirtd --tail=200 2>&1 || true
    kubectl exec -n "$NAMESPACE" "$pod" -c libvirtd -- \
      virsh -c qemu:///system list --all 2>&1 || true
    # QEMU writes why a domain did not start into these files.
    kubectl exec -n "$NAMESPACE" "$pod" -c libvirtd -- sh -c \
      'for f in /var/log/libvirt/qemu/*; do [ -f "$f" ] || continue; echo "--- $f ---"; tail -n 50 "$f"; done' 2>&1 || true
  done
  for pod in $(kubectl get pod -n "$NAMESPACE" -o name \
    -l app.kubernetes.io/name=novacompute 2>/dev/null || true); do
    echo "=== $pod ==="
    kubectl describe -n "$NAMESPACE" "$pod" 2>&1 | tail -40 || true
    kubectl logs -n "$NAMESPACE" "$pod" --all-containers --tail=200 2>&1 || true
    kubectl logs -n "$NAMESPACE" "$pod" --all-containers --tail=60 --previous 2>&1 || true
  done
  # nova-pool-discover-hosts is the nova-operator's own host discovery Job.
  for job in nova-pool-catalog-setup nova-pool-catalog-extras nova-pool-cirros-seed \
    nova-pool-discover-hosts; do
    echo "=== job/$job ==="
    kubectl logs -n "$NAMESPACE" "job/$job" 2>&1 || true
  done
  for ns in nova neutron cinder; do
    echo "=== $ns-operator ==="
    kubectl logs -n "$ns-system" -l "app.kubernetes.io/name=$ns-operator" \
      --tail=300 2>&1 || true
  done
  echo "=== events ==="
  kubectl get events -n "$NAMESPACE" --sort-by=.lastTimestamp 2>&1 | tail -150 || true
  return 0
}

# gl_dump_chaos: the suite's chaos objects and the last 200 lines of the probe
# server's console log.
gl_dump_chaos() {
  echo "=== podchaos ==="
  kubectl describe podchaos -n "$NAMESPACE" -l "$GL_CHAOS_LABEL" 2>&1 || true
  echo "=== console log of $GL_PROBE ==="
  gl_os console log show --lines 200 "$GL_PROBE" 2>&1 | tr -d '\r' || true
  return 0
}

# gl_tempest_verdict <junit xml> <test id...>: every test id passed in the
# report and none was skipped.
gl_tempest_verdict() {
  python3 -I - "$@" <<'PY'
import re
import sys
import xml.etree.ElementTree as ET

path, ids = sys.argv[1], sys.argv[2:]
try:
    cases = list(ET.parse(path).getroot().iter("testcase"))
except (OSError, ET.ParseError) as exc:
    print(f"FAIL: cannot read {path}: {exc}")
    sys.exit(1)


def outcome(case):
    for child in case:
        if child.tag in ("skipped", "skip"):
            return "skipped", (child.text or child.get("message") or "").strip()
        if child.tag in ("failure", "error"):
            return "failed", (child.get("message") or "").strip()
    return "passed", ""


byid = {}
fixtures = {}
for case in cases:
    name = case.get("name", "")
    classname = case.get("classname", "")
    if classname:
        byid[classname + "." + re.sub(r"\[[^\]]*\]$", "", name)] = case
    else:
        fixture = re.match(r"^setUpClass \((.+)\)$", name)
        if fixture:
            fixtures[fixture.group(1)] = case

rc = 0
for test in ids:
    # A class skipped in skip_checks, or whose fixture failed and whose tests
    # all passed on the serial retry, keeps only its setUpClass row
    # (hack/tempest/merge-retry-junit.py).
    # An Element without children is falsy, hence the explicit None test.
    case = byid.get(test)
    if case is None:
        case = fixtures.get(test.rsplit(".", 1)[0])
    if case is None:
        print(f"FAIL: {path} has no testcase {test}")
        rc = 1
        continue
    state, message = outcome(case)
    if state == "skipped":
        print(f"FAIL: Tempest skipped {test}: {message}")
        rc = 1
    elif state == "failed":
        print(f"FAIL: Tempest failed {test}: {message}")
        rc = 1
if rc == 0:
    print(f"OK: Tempest passed the {len(ids)} scenario tests, none skipped")
sys.exit(rc)
PY
}
