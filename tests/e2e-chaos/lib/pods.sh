#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# The recovery check of the suites that kill the pods of a Deployment, sourced
# by their scripts. Recovery is UID-gated (README.md): a pod the kill met must
# be gone, so a Deployment that only reads Ready again proves nothing.

# pod_uids <namespace> <selector>: the UIDs of the pods the selector matches.
pod_uids() {
  kubectl get pods -n "$1" -l "$2" -o jsonpath='{.items[*].metadata.uid}'
}

# wait_replaced <namespace> <selector> <pre-kill UIDs> <seconds>: no pre-kill
# pod left, spec.replicas new pods Ready, and the Deployment the selector
# matches back at readyReplicas == spec.replicas, polled every 2 s for at most
# <seconds>. A gracePeriod 0 kill removes the pods at once, while readyReplicas
# keeps its pre-kill value until the ReplicaSet and the Deployment controller
# have both seen the loss, so the new pods' own Ready condition decides.
wait_replaced() {
  local ns="$1" sel="$2" old="$3" secs="$4" deploy desired deadline pods left fresh ready uid status
  deploy="$(kubectl get deploy -n "$ns" -l "$sel" -o jsonpath='{.items[0].metadata.name}')" || return 1
  desired="$(kubectl get deploy "$deploy" -n "$ns" -o jsonpath='{.spec.replicas}')" || return 1
  deadline=$(($(date +%s) + secs))
  while true; do
    pods="$(kubectl get pods -n "$ns" -l "$sel" \
      -o jsonpath='{range .items[*]}{.metadata.uid} {.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}')" || return 1
    left=""
    fresh=0
    while read -r uid status; do
      [ -n "$uid" ] || continue
      case " ${old} " in
        *" ${uid} "*) left="${left} ${uid}" ;;
        *) if [ "$status" = "True" ]; then fresh=$((fresh + 1)); fi ;;
      esac
    done <<<"$pods"
    ready="$(kubectl get deploy "$deploy" -n "$ns" -o jsonpath='{.status.readyReplicas}')" || return 1
    if [ -z "$left" ] && [ "$fresh" -ge "$desired" ] && [ "${ready:-0}" -eq "$desired" ]; then
      echo "OK: ${ns}/${deploy} replaced every pre-kill pod with ${fresh} Ready pods and is at readyReplicas ${desired}"
      return 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "FAIL: ${ns}/${deploy} ${secs} s after the kill: pre-kill pods left:${left:- none}, new pods Ready ${fresh} of ${desired}, readyReplicas ${ready:-0} of ${desired}"
      return 1
    fi
    sleep 2
  done
}
