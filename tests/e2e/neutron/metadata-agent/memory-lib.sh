#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/neutron/metadata-agent/memory-lib.sh: the memory measurement of
# steps 9 to 11 of the metadata-agent suite.
#
# Chainsaw scripts share no shell state, but each runs in the suite directory,
# so the scripts of those steps source this file:
#
#   source ./memory-lib.sh
#
# The readings of all phases feed one rule
# (docs/reference/testing/sizing-calibration.md, Metadata agent memory), which
# is why every phase takes them through the same functions. NAMESPACE is the
# variable chainsaw hands every script.

# The number of networks the operator's memory default is sized for.
# shellcheck disable=SC2034 # read by the scripts that source this file
NETWORKS=32

# find_agent_pod: prints the agent pod that is not being deleted, if there is
# one.
find_agent_pod() {
  local pod found=""
  for pod in $(kubectl get pods -n "$NAMESPACE" \
    -l app.kubernetes.io/instance=neutron-agent,app.kubernetes.io/component=metadata-agent -o name); do
    if [ -z "$(kubectl get "$pod" -n "$NAMESPACE" -o 'jsonpath={.metadata.deletionTimestamp}')" ]; then
      found="$pod"
    fi
  done
  echo "$found"
}

# agent_pod: sets POD to that pod, and fails without one.
agent_pod() {
  POD="$(find_agent_pod)"
  if [ -z "$POD" ]; then
    echo "FAIL: no running pod of the metadata-agent DaemonSet found"
    exit 1
  fi
}

# section <name>: prints the lines stdin carries under the marker @<name>, up
# to the next marker of a reading.
section() {
  awk -v want="@$1" '/^@(current|stat|events|peak|comm)$/ { on = ($0 == want); next } on'
}

# reading: sets WS_MI, PEAK_MI, OOM_KILL and HAPROXY for $POD.
#
# The cgroup path comes from /proc/self/cgroup: the container is privileged and
# may share the node's cgroup namespace, where /sys/fs/cgroup/memory.current is
# not the container's file. The working set is memory.current less
# inactive_file, the figure the kubelet reports.
#
# One exec reads everything, so the values of a poll are of the same instant.
# It asks the image for sh and cat only. memory.peak is informational, and a
# kernel before 5.19 has no such file: the reading is then 0. The cat over the
# comm files exits 1 when a process ends between the glob and the read.
reading() {
  local out cur inactive peak
  # shellcheck disable=SC2016 # the container's sh expands the script
  if ! out="$(kubectl exec -n "$NAMESPACE" "$POD" -c metadata-agent -- sh -c '
    set -e
    cg=""
    while IFS=: read -r id _ path; do
      if [ "$id" = 0 ]; then cg="$path"; fi
    done < /proc/self/cgroup
    if [ -z "$cg" ]; then
      echo "no cgroup v2 entry in /proc/self/cgroup" >&2
      exit 1
    fi
    if [ "$cg" = / ]; then cg=""; fi
    dir="/sys/fs/cgroup${cg}"
    echo "@current"
    cat "$dir/memory.current"
    echo "@stat"
    cat "$dir/memory.stat"
    echo "@events"
    cat "$dir/memory.events"
    echo "@peak"
    cat "$dir/memory.peak" 2>/dev/null || echo 0
    echo "@comm"
    cat /proc/[0-9]*/comm 2>/dev/null || true
  ')"; then
    echo "FAIL: cannot read the cgroup of ${POD}"
    exit 1
  fi
  cur="$(section current <<< "$out")"
  inactive="$(section stat <<< "$out" | awk '$1 == "inactive_file" {print $2}')"
  peak="$(section peak <<< "$out")"
  OOM_KILL="$(section events <<< "$out" | awk '$1 == "oom_kill" {print $2}')"
  # grep -c prints the count and exits 1 on a count of 0.
  HAPROXY="$(section comm <<< "$out" | grep -cx haproxy || true)"
  WS_MI=$(( (cur - ${inactive:-0} + 1048575) / 1048576 ))
  PEAK_MI=$(( (peak + 1048575) / 1048576 ))
}

# phase <release> <networks> <timeout-seconds>: wait, then sample.
#
# Polls reading every 5 s until the agent runs one haproxy per network, keeps
# polling for 30 s more, prints the MEMORY-READING line with the largest
# working set of all polls, and applies the four checks.
phase() {
  local release="$1" target="$2" timeout="$3" limit limit_mi restarts deadline largest=0
  limit="$(kubectl get "$POD" -n "$NAMESPACE" \
    -o 'jsonpath={.spec.containers[?(@.name=="metadata-agent")].resources.limits.memory}')"
  if [[ "$limit" =~ ^([0-9]+)Mi$ ]]; then
    limit_mi="${BASH_REMATCH[1]}"
  elif [[ "$limit" =~ ^([0-9]+)Gi$ ]]; then
    limit_mi=$(( BASH_REMATCH[1] * 1024 ))
  else
    echo "FAIL: unexpected memory limit ${limit}"
    exit 1
  fi

  deadline=$(( SECONDS + timeout ))
  while :; do
    reading
    if [ "$WS_MI" -gt "$largest" ]; then largest="$WS_MI"; fi
    if [ "$HAPROXY" -eq "$target" ] || [ "$SECONDS" -ge "$deadline" ]; then break; fi
    sleep 5
  done
  if [ "$HAPROXY" -eq "$target" ]; then
    deadline=$(( SECONDS + 30 ))
    while [ "$SECONDS" -lt "$deadline" ]; do
      sleep 5
      reading
      if [ "$WS_MI" -gt "$largest" ]; then largest="$WS_MI"; fi
    done
  fi

  echo "MEMORY-READING release=${release} networks=${target} working_set_mi=${largest} peak_mi=${PEAK_MI} oom_kill=${OOM_KILL} haproxy=${HAPROXY} limit_mi=${limit_mi}"
  if [ "$HAPROXY" -ne "$target" ]; then
    echo "FAIL: the agent runs ${HAPROXY} of ${target} haproxy processes after ${timeout}s"
    exit 1
  fi
  restarts="$(kubectl get "$POD" -n "$NAMESPACE" \
    -o 'jsonpath={.status.containerStatuses[?(@.name=="metadata-agent")].restartCount}')"
  if [ "$restarts" != "0" ]; then
    echo "FAIL: ${POD} container metadata-agent restarted ${restarts} time(s)"
    exit 1
  fi
  if [ "$OOM_KILL" != "0" ]; then
    echo "FAIL: ${POD} counts ${OOM_KILL} OOM kill(s) in its cgroup"
    exit 1
  fi
  if [ $(( largest * 10 )) -gt $(( limit_mi * 9 )) ]; then
    echo "FAIL: working set ${largest}Mi is above 90% of the ${limit_mi}Mi limit"
    exit 1
  fi
}

# processes <release>: prints one MEMORY-PROCESSES line per process name in the
# agent container. VmRSS counts a shared page once per process, so the sums are
# informational and can exceed the working set of the cgroup.
processes() {
  kubectl exec -n "$NAMESPACE" "$POD" -c metadata-agent -- \
    sh -c 'cat /proc/[0-9]*/status 2>/dev/null || true' |
    awk -v release="$1" '
      $1 == "Name:" { name = $0; sub(/^Name:[ \t]*/, "", name); gsub(/ /, "_", name) }
      $1 == "VmRSS:" { count[name]++; kb[name] += $2 }
      END {
        for (n in count)
          printf "MEMORY-PROCESSES release=%s comm=%s count=%d rss_mi=%d\n", release, n, count[n], int((kb[n] + 1023) / 1024)
      }' | sort
}

# probe_overrides <pod> <mode> <first> <last>: prints the `kubectl run
# --overrides` payload of a probe pod, which runs probe.sh of
# 07-memory-probe-script.yaml on a range of networks. IMG is the OVN image and
# NB the Northbound address, both set by the caller.
probe_overrides() {
  cat <<JSON
{
  "apiVersion": "v1",
  "spec": {
    "volumes": [
      {"name": "certs", "secret": {"secretName": "neutron-agent-ovn-client"}},
      {"name": "probe", "configMap": {"name": "neutron-agent-memory-probe-script"}}
    ],
    "containers": [
      {
        "name": "$1",
        "image": "${IMG}",
        "command": ["bash", "/probe/probe.sh", "$2", "$3", "$4"],
        "env": [{"name": "NB_ADDRESS", "value": "${NB}"}],
        "volumeMounts": [
          {"name": "certs", "mountPath": "/certs", "readOnly": true},
          {"name": "probe", "mountPath": "/probe", "readOnly": true}
        ]
      }
    ]
  }
}
JSON
}
