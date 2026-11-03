#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Helpers of novacompute-node-loss, sourced by its scripts from the suite
# directory with NAMESPACE set by Chainsaw. The helpers of
# ../novacompute-drain-compute-kill/probe-lib.sh, for a pool on two workers.
#
# The pool nova-loss-pool runs nova-compute on both workers, whose names step 1
# writes into ConfigMap nova-loss-verify as victim and survivor. os_probe runs
# a script with the openstack client in a pod on the Tempest image, as the
# catalog Job does: the admin password comes from Secret keystone-admin through
# secretKeyRef, so it never enters an argument, and the verdict is read off the
# pod's log.

POOL=nova-loss-pool
VERIFY=nova-loss-verify
POD_SELECTOR=app.kubernetes.io/name=novacompute,app.kubernetes.io/instance=nova-loss-pool,app.kubernetes.io/component=nova-compute

# verify_get <key>: a key of ConfigMap nova-loss-verify, which step 1 writes.
verify_get() {
  kubectl get configmap "$VERIFY" -n "$NAMESPACE" -o "jsonpath={.data.$1}"
}

# victim_node and survivor_node: the two workers of the pool.
victim_node() {
  verify_get victim
}
survivor_node() {
  verify_get survivor
}

# node_entry <node>: "<phase>|<serviceStatus>|<serviceState>|<disabledReason>|<instances>"
# of the pool's node entry named <node>, empty when there is none. A failed
# read fails the call.
node_entry() {
  kubectl get novacompute "$POOL" -n "$NAMESPACE" -o json |
    jq -r --arg node "$1" '(.status.nodes // [])[] | select(.name == $node)
      | "\(.phase // "")|\(.serviceStatus // "")|\(.serviceState // "")|\(.disabledReason // "")|\(.instances // "")"'
}

# node_names: the names of the pool's node entries, sorted, space-separated.
node_names() {
  kubectl get novacompute "$POOL" -n "$NAMESPACE" -o json |
    jq -r '[(.status.nodes // [])[].name] | sort | join(" ")'
}

# services_ready: "<status>/<reason>" of the pool's ServicesReady condition.
services_ready() {
  kubectl get novacompute "$POOL" -n "$NAMESPACE" \
    -o "jsonpath={.status.conditions[?(@.type=='ServicesReady')].status}/{.status.conditions[?(@.type=='ServicesReady')].reason}"
}

# services_message: the message of the pool's ServicesReady condition.
services_message() {
  kubectl get novacompute "$POOL" -n "$NAMESPACE" \
    -o "jsonpath={.status.conditions[?(@.type=='ServicesReady')].message}"
}

# event_count <reason>: the summed count of events of <reason> on the pool. An
# event without a count counts as 1.
event_count() {
  local uid
  uid="$(kubectl get novacompute "$POOL" -n "$NAMESPACE" -o jsonpath='{.metadata.uid}')"
  kubectl get events -n "$NAMESPACE" \
    --field-selector "involvedObject.uid=${uid},reason=$1" -o json |
    jq '[.items[] | .count // 1] | add // 0'
}

# pool_pods: "<name> <uid> <node> <Ready status>" of every pod of the pool.
pool_pods() {
  kubectl get pods -n "$NAMESPACE" -l "$POD_SELECTOR" \
    -o jsonpath='{range .items[*]}{.metadata.name} {.metadata.uid} {.spec.nodeName} {.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}'
}

# os_probe <name> <seconds> <script>: run <script> with bash in pod <name> on
# the Tempest image, with VICTIM, SURVIVOR and the OS_* variables of the admin
# set, and wait up to <seconds> for the pod to end. Prints the log; returns 1
# when the pod did not succeed in time.
os_probe() {
  local pod="$1" limit="$2" script="$3" victim survivor phase deadline overrides
  victim="$(victim_node)"
  survivor="$(survivor_node)"
  overrides="$(
    cat <<EOF
{"apiVersion": "v1", "spec": {"automountServiceAccountToken": false,
  "containers": [{"name": "${pod}",
  "env": [{"name": "VICTIM", "value": "${victim}"},
          {"name": "SURVIVOR", "value": "${survivor}"},
          {"name": "OS_AUTH_URL", "value": "http://keystone-nova-loss.openstack.svc.cluster.local:5000/v3"},
          {"name": "OS_IDENTITY_API_VERSION", "value": "3"},
          {"name": "OS_USERNAME", "value": "admin"},
          {"name": "OS_USER_DOMAIN_NAME", "value": "Default"},
          {"name": "OS_PROJECT_NAME", "value": "admin"},
          {"name": "OS_PROJECT_DOMAIN_NAME", "value": "Default"},
          {"name": "OS_PASSWORD", "valueFrom": {"secretKeyRef": {"name": "keystone-admin", "key": "password"}}}],
  "resources": {"requests": {"cpu": "50m", "memory": "128Mi"}, "limits": {"memory": "512Mi"}}}]}}
EOF
  )"
  kubectl delete pod "$pod" -n "$NAMESPACE" --ignore-not-found --wait=true >/dev/null
  kubectl run "$pod" -n "$NAMESPACE" --image=ghcr.io/c5c3/tempest:2026.1 --restart=Never \
    --override-type=strategic --overrides="$overrides" \
    --command -- /bin/bash -c "$script" >/dev/null
  deadline=$(($(date +%s) + limit))
  while true; do
    phase="$(kubectl get pod "$pod" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    case "$phase" in Succeeded | Failed) break ;; esac
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "FAIL: probe ${pod} did not end within ${limit}s (phase ${phase:-<none>})"
      kubectl logs "$pod" -n "$NAMESPACE" 2>&1 || true
      kubectl delete pod "$pod" -n "$NAMESPACE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
      return 1
    fi
    sleep 3
  done
  kubectl logs "$pod" -n "$NAMESPACE" 2>&1 || true
  kubectl delete pod "$pod" -n "$NAMESPACE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  [ "$phase" = "Succeeded" ]
}
