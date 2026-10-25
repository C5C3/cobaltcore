#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Helpers of novacompute-drain-compute-kill, sourced by its scripts from the
# suite directory with NAMESPACE set by Chainsaw.
#
# The pool nova-drain-pool runs nova-compute on the one kind node, whose name
# step 1 writes into ConfigMap nova-drain-verify. os_probe runs a script with
# the openstack client in a pod on the Tempest image, as the catalog Job does:
# the admin password comes from Secret keystone-admin through secretKeyRef, so
# it never enters an argument, and the verdict is read off the pod's log.

POOL=nova-drain-pool
VERIFY=nova-drain-verify
POD_SELECTOR=app.kubernetes.io/name=novacompute,app.kubernetes.io/instance=nova-drain-pool,app.kubernetes.io/component=nova-compute

# pool_node: the node the pool runs on.
pool_node() {
  kubectl get configmap "$VERIFY" -n "$NAMESPACE" -o jsonpath='{.data.node}'
}

# verify_set <key> <value> and verify_get <key>: ConfigMap nova-drain-verify.
verify_set() {
  kubectl patch configmap "$VERIFY" -n "$NAMESPACE" --type=merge -p "{\"data\":{\"$1\":\"$2\"}}" >/dev/null
}
verify_get() {
  kubectl get configmap "$VERIFY" -n "$NAMESPACE" -o "jsonpath={.data.$1}"
}

# node_entry: "<phase>|<serviceStatus>|<disabledReason>|<instances>" of the
# pool's first node entry. A failed read fails the call.
node_entry() {
  kubectl get novacompute "$POOL" -n "$NAMESPACE" -o json |
    jq -r '(.status.nodes // [])[0] // {} | "\(.phase // "")|\(.serviceStatus // "")|\(.disabledReason // "")|\(.instances // "")"'
}

# services_ready: "<status>/<reason>" of the pool's ServicesReady condition.
services_ready() {
  kubectl get novacompute "$POOL" -n "$NAMESPACE" \
    -o "jsonpath={.status.conditions[?(@.type=='ServicesReady')].status}/{.status.conditions[?(@.type=='ServicesReady')].reason}"
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
# the Tempest image, with NODE and the OS_* variables of the admin set, and
# wait up to <seconds> for the pod to end. Prints the log; returns 1 when the
# pod did not succeed in time.
os_probe() {
  local pod="$1" limit="$2" script="$3" node phase deadline overrides
  node="$(pool_node)"
  overrides="$(
    cat <<EOF
{"apiVersion": "v1", "spec": {"automountServiceAccountToken": false,
  "containers": [{"name": "${pod}",
  "env": [{"name": "NODE", "value": "${node}"},
          {"name": "OS_AUTH_URL", "value": "http://keystone-nova-drain.openstack.svc.cluster.local:5000/v3"},
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
