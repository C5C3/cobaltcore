#!/bin/sh
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# The entrypoint of spine1, the EVPN route reflector. It addresses the
# loopback and the three underlay links before FRR starts; the spine runs no
# VRF and no VXLAN device.
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
wait_for eth3

ip addr add 192.0.2.193/32 dev lo
ip addr add 192.0.2.225/31 dev eth1
ip addr add 192.0.2.227/31 dev eth2
ip addr add 192.0.2.229/31 dev eth3

# The image's ENTRYPOINT and CMD. The topology's entrypoint replaces both, so
# this line starts FRR the way the image would.
exec /sbin/tini -- /usr/lib/frr/docker-start
