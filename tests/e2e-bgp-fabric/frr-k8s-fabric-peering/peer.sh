#!/bin/sh
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Starts one fabric peer, a stand-in for one leaf of the node's leaf pair.
# It runs in the Pods fabric-peer-1 and fabric-peer-2 under hostPID: true, so
# $$ is this shell's PID in the node's PID namespace while its network
# namespace is the Pod's own, and PID 1 is the node's init, whose network
# namespace is the node's. The veth pair is created in the node's namespace:
# the uplink $UPLINK stays there, enslaved to the VRF "fabric" that
# node-prep.sh created, and its peer moves into this Pod as eth1.
#
# accept_ra=0 has to be set before the uplink comes up. The peer's FRR sends
# router advertisements on eth1 for the unnumbered session, and a kind node
# takes one as a kernel default route in the VRF (proto ra, distance 0),
# which keeps the BGP IPv6 default out of the table.

set -eu

: "${UPLINK:?}" "${PEER_NAME:?}" "${ROUTER_ID:?}"

nsenter -t 1 -n -- ip link add "$UPLINK" mtu 9000 type veth peer name eth1 mtu 9000 netns $$
nsenter -t 1 -n -- sysctl -qw "net.ipv6.conf.$UPLINK.accept_ra=0"
nsenter -t 1 -n -- ip link set "$UPLINK" master fabric
nsenter -t 1 -n -- ip link set "$UPLINK" up
ip link set eth1 up

sed -e "s/@HOSTNAME@/$PEER_NAME/" -e "s/@ROUTER_ID@/$ROUTER_ID/" \
  /config/peer-frr.conf >/etc/frr/frr.conf
cp /config/daemons /etc/frr/daemons

exec /sbin/tini -- /usr/lib/frr/docker-start
