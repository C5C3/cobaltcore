#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Gateway openstack-gw of the kind stack, for the suites that reach a Keystone
# through its https listener. Sourced by their scripts and probe libraries.

# gateway_ip: the ClusterIP of the proxy Service of openstack-gw, selected the
# way hack/deploy-infra.sh selects it. Fails, naming the Service, when there is
# none.
gateway_ip() {
  local ip
  ip="$(kubectl get svc -n envoy-gateway-system \
    -l gateway.envoyproxy.io/owning-gateway-name=openstack-gw \
    -o jsonpath='{.items[*].spec.clusterIP}')" || return 1
  if [ -z "$ip" ]; then
    echo "FAIL: no proxy Service of openstack-gw in envoy-gateway-system" >&2
    return 1
  fi
  echo "${ip%% *}"
}
