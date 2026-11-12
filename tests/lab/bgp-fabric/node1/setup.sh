#!/bin/sh
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# The entrypoint of node1, the CobaltCore OpenStack node. It puts both
# uplinks into the fabric VRF, with the fabric loopback and the bait
# 203.0.113.0/24 on the VRF device, before FRR starts. eth0 stands in for the
# node network and keeps its address and its default routes.
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

ip link add fabric type vrf table 100
ip link set fabric up
ip link set eth1 master fabric
ip link set eth2 master fabric
ip addr add 192.0.2.1/32 dev fabric
ip addr add 2001:db8::1/128 dev fabric
ip addr add 203.0.113.1/24 dev fabric

# The image's ENTRYPOINT and CMD. The topology's entrypoint replaces both, so
# this line starts FRR the way the image would.
exec /sbin/tini -- /usr/lib/frr/docker-start
