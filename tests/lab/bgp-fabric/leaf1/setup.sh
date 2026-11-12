#!/bin/sh
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# The entrypoint of leaf1, a VTEP. It builds the deployment VRF cobaltcore
# with its L3VNI 20001 (a bridge with this VTEP's router MAC and a VXLAN
# device from the loopback) before FRR starts, as FRR's EVPN guide does on
# Linux.
set -eu

# containerlab attaches the links after the container has started, so wait
# for them before anything enslaves or addresses one.
wait_for() {
  i=0
  until [ -e "/sys/class/net/$1" ]; do
    i=$((i + 1))
    [ "$i" -lt 600 ] || { echo "setup.sh: $1 did not appear within 60 s" >&2; exit 1; }
    sleep 0.1
  done
}
wait_for eth1
wait_for eth2

ip addr add 192.0.2.129/32 dev lo
ip link add cobaltcore type vrf table 20001
ip link set cobaltcore up
ip link add br20001 type bridge
ip link set br20001 master cobaltcore addrgenmode none
ip link set br20001 address 02:cc:00:00:01:01
ip link add vni20001 type vxlan local 192.0.2.129 dstport 4789 id 20001 nolearning
ip link set vni20001 master br20001 addrgenmode none
ip link set vni20001 type bridge_slave neigh_suppress on learning off
# The fabric links carry 9216 and VXLAN over IPv4 takes 50 bytes; at the
# default 1500 these two devices would fragment a 9000-byte node packet.
ip link set br20001 mtu 9166
ip link set vni20001 mtu 9166
ip link set vni20001 up
ip link set br20001 up
# eth1 faces node1 and belongs to the deployment VRF; eth2 is the underlay
# link to spine1.
ip link set eth1 master cobaltcore
ip addr add 192.0.2.224/31 dev eth2

# The image's ENTRYPOINT and CMD. The topology's entrypoint replaces both, so
# this line starts FRR the way the image would.
exec /sbin/tini -- /usr/lib/frr/docker-start
