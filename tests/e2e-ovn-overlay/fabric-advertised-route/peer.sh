#!/bin/sh
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Starts one fabric peer of the fabric-advertised-route suite, a stand-in for
# one leaf of the node's leaf pair. It runs in the Pods fabric-peer-1 and
# fabric-peer-2 under hostPID: true, so $$ is this shell's PID in the node's
# PID namespace while its network namespace is the Pod's own, and PID 1 is the
# node's init, whose network namespace is the node's.
#
# The veth pair is created in the node's namespace: the uplink $UPLINK stays
# there, down and in no VRF, and its peer moves into this Pod as eth1. The
# OVNChassis owns the node end from there: fabric-node.sh sets its accept_ra to
# 0, takes it into the peer bridge and brings it up. That is the difference to
# tests/e2e-bgp-fabric/frr-k8s-fabric-peering/peer.sh, which enslaves the
# uplink to the VRF itself.

set -eu

: "${UPLINK:?}" "${PEER_NAME:?}" "${ROUTER_ID:?}"

nsenter -t 1 -n -- ip link add "$UPLINK" mtu 9000 type veth peer name eth1 mtu 9000 netns $$
ip link set eth1 up

sed -e "s/@HOSTNAME@/$PEER_NAME/" -e "s/@ROUTER_ID@/$ROUTER_ID/" \
  /config/peer-frr.conf >/etc/frr/frr.conf
cp /config/daemons /etc/frr/daemons

exec /sbin/tini -- /usr/lib/frr/docker-start
