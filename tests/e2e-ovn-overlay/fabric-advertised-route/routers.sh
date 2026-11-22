#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# The logical routers of the fabric-advertised-route suite, written into the
# Northbound database the way a CMS writes them. The operator writes none of
# these rows: the router and port options of OVN dynamic routing belong to the
# CMS (docs/reference/networking/ovn-dynamic-routing.md).
#
#   routers.sh up <system-id>   create both routers on the chassis <system-id>
#   routers.sh down             delete them, and nothing else
#
# up creates two routers bound to the chassis:
#   - lr-adv, VRF id 42, redistributes its static routes. 198.51.100.10/32 and
#     2001:db8::10/128 lie in the chassis's announceable ranges;
#     203.0.113.10/32 and 2001:db8:0:1::10/128 are the baits, which lie outside
#     them and must not reach a peer.
#   - lr-learn, VRF id 10, with one port per peer bridge: the port names the
#     bridge in dynamic-routing-port-name, which the chassis's
#     dynamic-routing-port-mapping maps to the bridge's own interface, so the
#     leaf defaults FRR learns over each bridge land in Learned_Route.
#
# dynamic-routing-maintain-vrf is set last, after a --wait=hv sync: OVN 26.03.2
# creates a maintained VRF only when ovn-controller sees the option at a change
# of the port binding, and an option it saw at the port's creation left no VRF
# behind.
#
# It runs in the suite's client Pod, which mounts the central's client keypair
# at /certs, against NB_ADDRESS.

set -euo pipefail

nb() {
  ovn-nbctl --db="$NB_ADDRESS" -p /certs/tls.key -c /certs/tls.crt -C /certs/ca.crt --timeout=30 "$@"
}

up() {
  nb lr-add lr-adv -- set Logical_Router lr-adv options:chassis="$1" options:dynamic-routing=true \
    options:dynamic-routing-redistribute=static options:dynamic-routing-vrf-id=42
  nb lrp-add lr-adv lrp-adv 00:00:5e:00:53:01 203.0.113.1/24 2001:db8:113::1/64
  nb ls-add ls-adv -- lsp-add ls-adv ls-adv-lrp -- lsp-set-type ls-adv-lrp router \
    -- lsp-set-addresses ls-adv-lrp router -- lsp-set-options ls-adv-lrp router-port=lrp-adv
  nb lr-route-add lr-adv 198.51.100.10/32 203.0.113.2
  nb lr-route-add lr-adv 2001:db8::10/128 2001:db8:113::2
  nb lr-route-add lr-adv 203.0.113.10/32 203.0.113.2
  nb lr-route-add lr-adv 2001:db8:0:1::10/128 2001:db8:113::2
  nb lr-add lr-learn -- set Logical_Router lr-learn options:chassis="$1" options:dynamic-routing=true \
    options:dynamic-routing-vrf-id=10
  for i in 1 2; do
    nb lrp-add lr-learn "lrp-fab$i" "00:00:5e:00:53:1$i" \
      -- set Logical_Router_Port "lrp-fab$i" options:dynamic-routing-port-name="br-fab$i"
    nb ls-add "ls-fab$i" -- lsp-add "ls-fab$i" "ls-fab$i-lrp" -- lsp-set-type "ls-fab$i-lrp" router \
      -- lsp-set-addresses "ls-fab$i-lrp" router -- lsp-set-options "ls-fab$i-lrp" router-port="lrp-fab$i"
  done
  nb --wait=hv sync
  for p in lrp-adv lrp-fab1 lrp-fab2; do
    nb set Logical_Router_Port "$p" options:dynamic-routing-maintain-vrf=true
  done
}

down() {
  nb --if-exists lr-del lr-adv
  nb --if-exists lr-del lr-learn
  for ls in ls-adv ls-fab1 ls-fab2; do
    nb --if-exists ls-del "$ls"
  done
}

case "${1:-}" in
up)
  : "${2:?usage: routers.sh up <system-id>}"
  up "$2"
  ;;
down)
  down
  ;;
*)
  echo "usage: routers.sh up <system-id> | down" >&2
  exit 2
  ;;
esac
