#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Helpers of neutron-metadata-agent-pod-kill, sourced by its scripts from the
# suite directory with NAMESPACE set by Chainsaw.
#
# The probe pods run on the labelled node with hostNetwork, privileged and as
# root, the shape of ../ovn-southbound-outage/: /run/netns is mounted
# Bidirectional, so the namespace the plug creates is visible to the probes
# after it. The neutron image carries python3 and iproute2 and the OVN image
# carries no python3, so the probes do the namespace work; the step plugs the
# port through the chassis pod, as tests/e2e/neutron/metadata-agent/ does.

PROBE_IMAGE=ghcr.io/c5c3/neutron:2026.1
BOOT_JOB=nova-meta-chaos-boot

# boot_value <key>: one value the boot step stored on its Job as
# chaos.c5c3.io/<key>.
boot_value() {
  kubectl get job "$BOOT_JOB" -n "$NAMESPACE" \
    -o "jsonpath={.metadata.annotations.chaos\.c5c3\.io/$1}"
}

# chassis_node: the node the suite labelled in step 1.
chassis_node() {
  kubectl get nodes -l openstack.c5c3.io/chassis=true -o jsonpath='{.items[0].metadata.name}'
}

# run_probe <pod> <script>: run /probe/<script> in a probe pod on the chassis
# node, wait up to five minutes for it to end and print its log. PHASE holds
# the last phase the pod reported.
run_probe() {
  local pod="$1" script="$2" node overrides
  node="$(chassis_node)"
  overrides="$(cat <<JSON
{
  "apiVersion": "v1",
  "spec": {
    "nodeName": "${node}",
    "hostNetwork": true,
    "terminationGracePeriodSeconds": 1,
    "volumes": [
      {"name": "probe", "configMap": {"name": "nova-meta-chaos-probe-scripts"}},
      {"name": "run-netns", "hostPath": {"path": "/run/netns", "type": "DirectoryOrCreate"}}
    ],
    "containers": [
      {
        "name": "${pod}",
        "image": "${PROBE_IMAGE}",
        "command": ["bash", "/probe/${script}"],
        "securityContext": {"privileged": true, "runAsUser": 0},
        "resources": {"requests": {"cpu": "10m", "memory": "32Mi"}, "limits": {"memory": "128Mi"}},
        "env": [
          {"name": "PORT_MAC", "value": "$(boot_value port-mac)"},
          {"name": "PORT_IP", "value": "$(boot_value port-ip)"},
          {"name": "META_IP", "value": "$(boot_value metadata-ip)"},
          {"name": "SERVER_ID", "value": "$(boot_value server-id)"}
        ],
        "volumeMounts": [
          {"name": "probe", "mountPath": "/probe", "readOnly": true},
          {"name": "run-netns", "mountPath": "/run/netns", "mountPropagation": "Bidirectional"}
        ]
      }
    ]
  }
}
JSON
)"
  kubectl delete pod "$pod" -n "$NAMESPACE" --ignore-not-found --wait=true --timeout=60s >/dev/null
  kubectl run "$pod" -n "$NAMESPACE" --image="$PROBE_IMAGE" --restart=Never \
    --overrides="$overrides" >/dev/null
  PHASE=""
  for _ in $(seq 1 150); do
    PHASE="$(kubectl get pod "$pod" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    case "$PHASE" in Succeeded | Failed) break ;; esac
    sleep 2
  done
  kubectl logs -n "$NAMESPACE" "$pod" 2>&1 || true
  echo "pod phase: ${PHASE:-<none>}"
}

# agent_pod: the name of the running metadata agent pod, empty without one.
agent_pod() {
  kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/instance=nova-meta-chaos-agent \
    --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true
}

# openstack_cleanup: delete meta-chaos-vm and meta-chaos-net through the API
# from a pod on the Tempest image, and fail when either is still listed. It
# does nothing once the Nova CR is gone, which is when the API is.
openstack_cleanup() {
  local pod=nova-meta-chaos-openstack-cleanup phase="" out pw
  if ! kubectl get nova nova-meta-chaos -n "$NAMESPACE" >/dev/null 2>&1; then
    echo "the Nova CR is gone; nothing to delete through its API"
    return 0
  fi
  pw="$(kubectl get secret keystone-admin -n "$NAMESPACE" -o jsonpath='{.data.password}' | base64 -d)"
  kubectl delete pod "$pod" -n "$NAMESPACE" --ignore-not-found --wait=true --timeout=60s >/dev/null
  kubectl run "$pod" -n "$NAMESPACE" --image=ghcr.io/c5c3/tempest:2026.1 --restart=Never \
    --env "OS_AUTH_URL=http://keystone-nova-meta-chaos.openstack.svc.cluster.local:5000/v3" \
    --env OS_IDENTITY_API_VERSION=3 --env OS_USERNAME=admin --env OS_USER_DOMAIN_NAME=Default \
    --env OS_PROJECT_NAME=admin --env OS_PROJECT_DOMAIN_NAME=Default --env "OS_PASSWORD=${pw}" \
    --command -- bash -c '
      openstack server delete --wait meta-chaos-vm 2>&1 || true
      openstack network delete meta-chaos-net 2>&1 || true
      LEFT="$(openstack server list --name "^meta-chaos-vm$" -f value -c ID 2>&1)$(openstack network list --name meta-chaos-net -f value -c ID 2>&1)"
      if [ -n "$LEFT" ]; then echo "LEFT: $LEFT"; exit 1; fi
      echo OPENSTACK-CLEAN-OK' >/dev/null
  for _ in $(seq 1 120); do
    phase="$(kubectl get pod "$pod" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    case "$phase" in Succeeded | Failed) break ;; esac
    sleep 2
  done
  out="$(kubectl logs -n "$NAMESPACE" "$pod" 2>&1 || true)"
  kubectl delete pod "$pod" -n "$NAMESPACE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  echo "$out"
  grep -qx OPENSTACK-CLEAN-OK <<<"$out"
}
