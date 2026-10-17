#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Helpers of nova-compute-neutron-http-delay, sourced by its scripts from the
# suite directory with NAMESPACE set by Chainsaw.

# shellcheck source=tests/e2e-chaos/lib/faults.sh
. ../lib/faults.sh

KEYSTONE=http://keystone-nova-vifd-chaos.openstack.svc.cluster.local:5000/v3

# chassis_node: the node the suite labelled in step 1.
chassis_node() {
  kubectl get nodes -l openstack.c5c3.io/chassis=true -o jsonpath='{.items[0].metadata.name}'
}

# os_run <pod> <seconds> <bash script> [K=V...]: run the script with the
# openstack client of the Tempest image as the bootstrap admin of this
# suite's Keystone, in a probe pod of ../lib/faults.sh, with K=V in its
# environment. Returns 0 when the pod Succeeded.
os_run() {
  local pod="$1" secs="$2" script="$3" pw pair envs=()
  shift 3
  pw="$(admin_password)" || return 1
  for pair in "$@"; do
    envs+=(--env "$pair")
  done
  probe_run "$pod" "$secs" --env "OS_AUTH_URL=${KEYSTONE}" --env OS_IDENTITY_API_VERSION=3 \
    --env OS_USERNAME=admin --env OS_USER_DOMAIN_NAME=Default --env OS_PROJECT_NAME=admin \
    --env OS_PROJECT_DOMAIN_NAME=Default --env "OS_PASSWORD=${pw}" --env HOME=/tmp \
    ${envs[@]+"${envs[@]}"} -- bash -c "$script"
}

# openstack_cleanup: delete every vifd-* server and vifd-net through the API,
# and fail when one is still listed. It does nothing once the Nova CR is gone,
# which is when the API is.
openstack_cleanup() {
  if ! kubectl get nova nova-vifd-chaos -n "$NAMESPACE" >/dev/null 2>&1; then
    echo "the Nova CR is gone; nothing to delete through its API"
    return 0
  fi
  # shellcheck disable=SC2016 # expanded by the probe pod's bash
  os_run nova-vifd-chaos-openstack-cleanup 300 '
    for server in vifd-base vifd-slow vifd-recovered; do
      openstack server delete --wait "$server" 2>&1 || true
    done
    openstack network delete vifd-net 2>&1 || true
    LEFT="$(openstack server list --all-projects --name "^vifd-" -f value -c ID 2>&1)$(openstack network list --name vifd-net -f value -c ID 2>&1)"
    if [ -n "$LEFT" ]; then echo "LEFT: $LEFT"; exit 1; fi
    echo OPENSTACK-CLEAN-OK'
}
