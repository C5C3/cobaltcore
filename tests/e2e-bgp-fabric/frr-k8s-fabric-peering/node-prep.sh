#!/bin/sh
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Prepares the kind node for the frr-k8s fabric peering suite, the way the
# device preparation of a compute node will: a VRF device "fabric" carrying
# the node's fabric loopback /32 and /128. It runs in the Pod fabric-node,
# which shares the node's network namespace (hostNetwork), so every device it
# creates is a device of the node.
#
#   up             delete what an earlier run left, snapshot the node's two
#                  default routes, create the VRF "fabric" with the
#                  loopbacks, then sleep until the Pod is deleted
#   kernel-routes  create the VRF "ovnvrf42" (a stand-in for an OVN
#                  ovnvrf<id>) with a dummy device and four host routes:
#                  two the fabric may carry, two baits it must not
#   down           delete the devices again (the Pod's preStop hook)

set -eu

# del <device>... deletes each device and ignores only a device that does not
# exist; any other failure stops the script with ip's message.
del() {
  for dev in "$@"; do
    if ! out="$(ip link del "$dev" 2>&1)"; then
      case "$out" in
        *"Cannot find device"*) ;;
        *)
          echo "ERROR: ip link del $dev: $out" >&2
          exit 1
          ;;
      esac
    fi
  done
}

case "${1:-}" in
  up)
    del ovn42 ovnvrf42 fabric
    ip -4 route show default >/tmp/default-v4.before
    ip -6 route show default >/tmp/default-v6.before
    if ! out="$(ip link add fabric type vrf table 100 2>&1)"; then
      echo "ERROR: cannot create a VRF device; the node kernel has no vrf module"
      echo "$out"
      exit 1
    fi
    ip link set fabric up
    ip addr add 192.0.2.1/32 dev fabric
    ip -6 addr add 2001:db8::1/128 dev fabric
    # The readiness probe reads this file: Ready means the devices exist.
    touch /tmp/prepared
    echo prepared
    exec sleep infinity
    ;;
  kernel-routes)
    ip link add ovnvrf42 type vrf table 42
    ip link set ovnvrf42 up
    ip link add ovn42 type dummy
    ip link set ovn42 master ovnvrf42
    ip link set ovn42 up
    ip route add 198.51.100.10/32 dev ovn42 vrf ovnvrf42
    ip route add 203.0.113.10/32 dev ovn42 vrf ovnvrf42
    ip -6 route add 2001:db8::10/128 dev ovn42 vrf ovnvrf42
    ip -6 route add 2001:db8:0:1::10/128 dev ovn42 vrf ovnvrf42
    echo "kernel routes added"
    ;;
  down)
    del ovn42 ovnvrf42 fabric
    echo "devices deleted"
    ;;
  *)
    echo "usage: $0 up|kernel-routes|down" >&2
    exit 2
    ;;
esac
