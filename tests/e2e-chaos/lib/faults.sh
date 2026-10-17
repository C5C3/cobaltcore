#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# The fault phases of the suites that inject HTTPChaos, TimeChaos, IOChaos
# and StressChaos (#1391), sourced by their scripts. NAMESPACE is the
# suite's namespace, where every chaos object of the suite lives.
#
# A phase applies its object, waits for AllInjected, runs a canary that proves
# the fault reached its target, asserts, and lifts the object before the next
# phase. AllInjected says only that Chaos Mesh set the fault up: TimeChaos
# reaches PID 1 of the container and its children, HTTPChaos new connections,
# IOChaos the paths below volumePath. A canary that sees no effect prints
# `FAIL: <kind>/<name> reports AllInjected but <observation>`, so a fault that
# missed its target cannot pass a "nothing happened" claim.
#
# docs/reference/testing/chaos-e2e-tests.md (Fault Kinds Beyond PodChaos and
# NetworkChaos) describes the phases.

# Every kind of chaos object a suite may create; diagnostics.sh lists the same.
CHAOS_KINDS=podchaos,networkchaos,httpchaos,timechaos,iochaos,stresschaos,dnschaos

# fault_render <file> [NAME=VALUE...]: print the file with every @NAME@
# replaced by its VALUE. A placeholder left without a value fails.
fault_render() {
  local file="$1" pair out left args=()
  shift
  for pair in "$@"; do
    args+=(-e "s|@${pair%%=*}@|${pair#*=}|g")
  done
  if [ ${#args[@]} -eq 0 ]; then
    out="$(cat "$file")" || return 1
  else
    out="$(sed "${args[@]}" "$file")" || return 1
  fi
  left="$( { grep -oE '@[A-Z][A-Z0-9_]*@' <<<"$out" || true; } | sort -u | xargs)"
  if [ -n "$left" ]; then
    echo "FAIL: ${file}: no value for ${left}" >&2
    return 1
  fi
  printf '%s\n' "$out"
}

# fault_inject <file> <kind/name> [NAME=VALUE...]: apply the rendered file in
# NAMESPACE and wait up to 120 s for the object's AllInjected condition.
fault_inject() {
  local file="$1" ref="$2" rendered
  shift 2
  rendered="$(fault_render "$file" "$@")" || return 1
  kubectl apply -n "$NAMESPACE" -f - <<<"$rendered" || return 1
  kubectl wait "$ref" -n "$NAMESPACE" --for=condition=AllInjected --timeout=120s
}

# fault_lift <kind/name>: delete the object, then wait until it is gone, so the
# controller has released the fault before the next phase applies its own.
fault_lift() {
  kubectl delete "$1" -n "$NAMESPACE" --ignore-not-found --wait=true --timeout=120s || return 1
  kubectl wait "$1" -n "$NAMESPACE" --for=delete --timeout=120s
}

# fault_lift_suite <suite>: print, then delete, every chaos object labelled
# chaos.c5c3.io/suite=<suite> in NAMESPACE, the lift of a suite's catch.
fault_lift_suite() {
  kubectl get "$CHAOS_KINDS" -n "$NAMESPACE" -l "chaos.c5c3.io/suite=$1" -o yaml 2>&1 || true
  kubectl delete "$CHAOS_KINDS" -n "$NAMESPACE" -l "chaos.c5c3.io/suite=$1" \
    --ignore-not-found --wait=true --timeout=120s
}

# canary_fail <kind/name> <observation>: the line of a canary that saw no
# effect. Returns 1.
canary_fail() {
  echo "FAIL: $1 reports AllInjected but $2"
  return 1
}

# canary_dd <kind/name> <PHASE> <pod> <container> <file> latency|enospc|eio:
# write 40 KiB into <file> with dd through kubectl exec in NAMESPACE (set it
# for the call when the pod runs elsewhere) and print INJECTED-<PHASE>-OK when
# the write shows the fault: latency wants at least 1 s beyond the round trip
# of an empty exec into the same container, enospc a failure naming "No space
# left on device", eio one naming "Input/output error" (glibc) or "I/O error"
# (musl, the busybox dd of the Alpine-based OpenBao image). dd runs with
# conv=fsync, which busybox dd accepts as well as GNU dd.
canary_dd() {
  local ref="$1" phase="$2" pod="$3" container="$4" file="$5" mode="$6"
  local out rc=0 start base_ms ms want
  start="$(date +%s%N)"
  kubectl exec "$pod" -n "$NAMESPACE" -c "$container" -- true >/dev/null 2>&1 || true
  base_ms=$((($(date +%s%N) - start) / 1000000))
  start="$(date +%s%N)"
  out="$(kubectl exec "$pod" -n "$NAMESPACE" -c "$container" -- \
    dd if=/dev/zero of="$file" bs=4k count=10 conv=fsync 2>&1)" || rc=$?
  ms=$((($(date +%s%N) - start) / 1000000 - base_ms))
  case "$mode" in
    latency)
      if [ "$rc" -ne 0 ]; then
        echo "FAIL: the latency canary could not write ${file}: ${out}"
        return 1
      fi
      if [ "$ms" -lt 1000 ]; then
        canary_fail "$ref" "dd wrote 40 KiB to ${file} in ${ms} ms beyond an empty exec's ${base_ms} ms"
        return 1
      fi
      ;;
    enospc | eio)
      want="No space left on device"
      if [ "$mode" = eio ]; then
        want="Input/output error|I/O error"
      fi
      if [ "$rc" -eq 0 ]; then
        canary_fail "$ref" "dd wrote 40 KiB to ${file} in ${ms} ms"
        return 1
      fi
      if ! grep -qE "$want" <<<"$out"; then
        canary_fail "$ref" "dd failed without '${want}': ${out}"
        return 1
      fi
      ;;
    *)
      echo "canary_dd: unknown mode '${mode}'" >&2
      return 2
      ;;
  esac
  echo "dd into ${file}: exit ${rc} after ${ms} ms beyond an empty exec's ${base_ms} ms"
  echo "INJECTED-${phase}-OK"
}

# cgroup_memory <pod> <container> memory.max|memory.current|oom_kill <label>:
# print a cgroup v2 memory value of the container, read through kubectl exec
# in NAMESPACE; oom_kill comes from memory.events. A missing file and a
# memory.max of max each print a FAIL line naming <label> and return 1.
cgroup_memory() {
  local pod="$1" container="$2" what="$3" label="$4" file="$3" out
  if [ "$what" = oom_kill ]; then
    file=memory.events
  fi
  if ! out="$(kubectl exec "$pod" -n "$NAMESPACE" -c "$container" -- cat "/sys/fs/cgroup/${file}" 2>&1)"; then
    echo "FAIL: no cgroup v2 memory files in the ${label} container: ${out}"
    return 1
  fi
  case "$what" in
    oom_kill)
      out="$(awk '$1 == "oom_kill" { print $2 }' <<<"$out")"
      ;;
    memory.max)
      if [ "$out" = max ]; then
        echo "FAIL: the ${label} container has no memory limit"
        return 1
      fi
      ;;
  esac
  if ! [[ "$out" =~ ^[0-9]+$ ]]; then
    echo "FAIL: ${file} of the ${label} container reads '${out}', not a number"
    return 1
  fi
  echo "$out"
}

# oom_seen <pod> <container> <oom_kill> <restartCount>: print what shows an
# OOM kill in the container since oom_kill and restartCount were read, and
# return 0; while nothing does, print both readings and return 1. A container
# the OOM kill restarted starts a new cgroup, whose counter is back at zero;
# its restart with reason OOMKilled says the same.
oom_seen() {
  local now restarts reason
  now="$(cgroup_memory "$1" "$2" oom_kill "$2")" || now=0
  restarts="$(kubectl get pod "$1" -n "$NAMESPACE" \
    -o jsonpath="{.status.containerStatuses[?(@.name==\"$2\")].restartCount}")"
  reason="$(kubectl get pod "$1" -n "$NAMESPACE" \
    -o jsonpath="{.status.containerStatuses[?(@.name==\"$2\")].lastState.terminated.reason}")"
  if [ "$now" -gt "$3" ]; then
    echo "oom_kill rose from $3 to ${now}"
    return 0
  fi
  if [ "${restarts:-0}" -gt "${4:-0}" ] && [ "$reason" = OOMKilled ]; then
    echo "the $2 container restarted after an OOM kill"
    return 0
  fi
  echo "oom_kill stayed at ${now} and the container at restartCount ${restarts:-0}"
  return 1
}

# state <key>: a value an earlier step of the suite recorded in its state
# ConfigMap, which STATE names, in NAMESPACE.
state() {
  kubectl get configmap "$STATE" -n "$NAMESPACE" -o "jsonpath={.data.$1}"
}

# probe_start <pod> [--env K=V...] -- <command...>: start a pod on the Tempest
# image in NAMESPACE with the workload pod's resources and securityContext
# (lib/workload.sh) and the label app.kubernetes.io/name=chaos-probe, which no
# chaos selector matches. A pod left by an earlier run is deleted first. The
# pod mounts no token: a probe never calls the Kubernetes API.
probe_start() {
  local pod="$1" envs=()
  shift
  while [ $# -gt 0 ] && [ "$1" != "--" ]; do
    if [ "$1" != "--env" ] || [ $# -lt 2 ]; then
      echo "usage: probe_start <pod> [--env K=V...] -- <command...>" >&2
      return 2
    fi
    envs+=(--env "$2")
    shift 2
  done
  if [ $# -lt 2 ]; then
    echo "usage: probe_start <pod> [--env K=V...] -- <command...>" >&2
    return 2
  fi
  shift
  kubectl delete pod "$pod" -n "$NAMESPACE" --ignore-not-found --wait=true --timeout=60s >/dev/null || return 1
  kubectl run "$pod" -n "$NAMESPACE" --image="${CHAOS_WORKLOAD_IMAGE:-ghcr.io/c5c3/tempest:2026.1}" \
    --restart=Never --labels=app.kubernetes.io/name=chaos-probe \
    ${envs[@]+"${envs[@]}"} \
    --override-type=strategic --overrides="$(probe_overrides "$pod")" \
    --command -- "$@" >/dev/null
}

# probe_overrides <pod>: the resources and securityContext of a probe pod.
probe_overrides() {
  cat <<EOF
{"apiVersion": "v1", "spec": {"automountServiceAccountToken": false,
  "containers": [{"name": "$1",
  "resources": {"requests": {"cpu": "50m", "memory": "128Mi"},
                "limits": {"memory": "256Mi"}},
  "securityContext": {"runAsNonRoot": true, "runAsUser": 42424,
                      "runAsGroup": 42424, "allowPrivilegeEscalation": false,
                      "capabilities": {"drop": ["ALL"]},
                      "seccompProfile": {"type": "RuntimeDefault"}}}]}}
EOF
}

# probe_run <pod> <seconds> [--env K=V...] -- <command...>: probe_start, then
# wait up to <seconds> for the pod to end, print its log and its phase, and
# delete it. Returns 0 when the pod Succeeded.
probe_run() {
  local pod="$1" secs="$2" phase="" deadline
  shift 2
  probe_start "$pod" "$@" || return
  deadline=$(($(date +%s) + secs))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    phase="$(kubectl get pod "$pod" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    case "$phase" in
      Succeeded | Failed) break ;;
    esac
    sleep 2
  done
  kubectl logs "$pod" -n "$NAMESPACE" 2>&1 || true
  echo "pod ${pod} phase: ${phase:-<none>}"
  kubectl delete pod "$pod" -n "$NAMESPACE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  [ "$phase" = Succeeded ]
}

# wait_sentinel <pod> <regex> <seconds>: poll the log of a running probe pod
# every 2 s and print the first line that matches <regex>. A line starting
# with FAIL:, or a pod that ended before the match, prints the log and
# returns 1, and so does the deadline.
wait_sentinel() {
  local pod="$1" re="$2" secs="$3" log phase line deadline
  deadline=$(($(date +%s) + secs))
  while true; do
    # The phase is read before the log, so a pod that ended has written all
    # of its log by the time the log is searched.
    phase="$(kubectl get pod "$pod" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    log="$(kubectl logs "$pod" -n "$NAMESPACE" 2>/dev/null || true)"
    if line="$(grep -E -m1 -- "$re" <<<"$log")"; then
      echo "$line"
      return 0
    fi
    if grep -q '^FAIL:' <<<"$log"; then
      echo "$log"
      return 1
    fi
    case "$phase" in
      Succeeded | Failed)
        echo "$log"
        echo "FAIL: ${pod} reached ${phase} without a line matching ${re}"
        return 1
        ;;
    esac
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "$log"
      echo "FAIL: no line matching ${re} from ${pod} within ${secs}s"
      return 1
    fi
    sleep 2
  done
}

# job_run <file> <job> <seconds> [NAME=VALUE...]: delete a Job of that name
# left by an earlier run (its pod template is immutable), apply the rendered
# file in NAMESPACE, wait up to <seconds> for the Job to complete or fail,
# and print its log. Returns 0 when it completed.
job_run() {
  local file="$1" job="$2" secs="$3" rendered state="" deadline
  shift 3
  rendered="$(fault_render "$file" "$@")" || return 1
  kubectl delete job "$job" -n "$NAMESPACE" --ignore-not-found --wait=true --timeout=60s >/dev/null || return 1
  kubectl apply -n "$NAMESPACE" -f - <<<"$rendered" || return 1
  deadline=$(($(date +%s) + secs))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    state="$(kubectl get job "$job" -n "$NAMESPACE" \
      -o jsonpath='{.status.conditions[?(@.status=="True")].type}' 2>/dev/null || true)"
    case " ${state} " in
      *" Complete "* | *" Failed "*) break ;;
    esac
    sleep 5
  done
  kubectl logs "job/${job}" -n "$NAMESPACE" 2>&1 || true
  case " ${state} " in
    *" Complete "*) return 0 ;;
    *" Failed "*) echo "FAIL: job ${job} failed" ;;
    *) echo "FAIL: job ${job} did not finish within ${secs}s" ;;
  esac
  kubectl get pods -n "$NAMESPACE" -l "job-name=${job}" -o wide 2>&1 || true
  return 1
}

# admin_password: the bootstrap admin password of Secret keystone-admin in
# NAMESPACE, which the probes take as OS_PASSWORD.
admin_password() {
  local pw
  pw="$(kubectl get secret keystone-admin -n "$NAMESPACE" -o jsonpath='{.data.password}' | base64 -d)" || return 1
  if [ -z "$pw" ]; then
    echo "FAIL: Secret keystone-admin holds no password" >&2
    return 1
  fi
  printf '%s' "$pw"
}
