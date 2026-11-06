#!/bin/sh
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# The user-data of nova-pool-guest-probe. Step 5 of chainsaw-test.yaml renders
# it with sed "s|@PEER@|<address of nova-pool-guest-peer>|", and cirros runs it
# as root at boot. It writes one line per probe to the serial console, which
# Nova keeps as the console log and guest-lib.sh reads:
#
#   guest-probe <kind> <seq> <HH:MM:SS UTC> <result> [<detail>]
#
#   dhcp      once, seq 1: ok <IPv4 of eth0>, or none
#   metadata  once, seq 1: ok <instance-id> from the metadata service, or error
#   net       every 2 s: ok when the peer answers one ping, else lost
#   disk      every 2 s: ok <n> when probe-<n> written at byte 4096 of the
#             attached volume reads back after the page cache is dropped,
#             else error <n>
#
# net and disk start once /dev/vdb appears, after at most 300 s. They run in
# the background, so the boot does not wait for the volume. A disk probe says
# error while /dev/vdb is no block device: dd would create a plain file there
# and read its own write back from RAM. The guest is busybox: no pkill, no
# ip -o.
PEER=@PEER@

# Kernel messages, the drop_caches note of every disk probe among them, stay
# off ttyS0, so they cannot split a probe line.
dmesg -n 1

# probe <kind> <seq> <result...>: one line on the serial console.
probe() {
  kind=$1
  seq=$2
  shift 2
  echo "guest-probe $kind $seq $(date -u +%T) $*" >/dev/ttyS0
}

address=$(ip -4 addr show eth0 | sed -n 's/.*inet \([0-9.]*\)\/.*/\1/p' | head -n 1)
if [ -n "$address" ]; then
  probe dhcp 1 ok "$address"
else
  probe dhcp 1 none
fi

# -f: an HTTP error answers nothing rather than an error page.
id=$(curl -sf -m 10 http://169.254.169.254/latest/meta-data/instance-id)
if [ -n "$id" ]; then
  probe metadata 1 ok "$id"
else
  probe metadata 1 error
fi

(
  i=0
  while [ ! -b /dev/vdb ] && [ "$i" -lt 300 ]; do
    sleep 1
    i=$((i + 1))
  done
  (
    n=0
    while :; do
      n=$((n + 1))
      if ping -c 1 -W 1 "$PEER" >/dev/null 2>&1; then
        probe net "$n" ok
      else
        probe net "$n" lost
      fi
      sleep 2
    done
  ) &
  (
    n=0
    while :; do
      n=$((n + 1))
      if [ -b /dev/vdb ] &&
        echo "probe-$n" | dd of=/dev/vdb bs=512 seek=8 conv=fsync,notrunc 2>/dev/null &&
        echo 1 >/proc/sys/vm/drop_caches &&
        [ "$(dd if=/dev/vdb bs=512 skip=8 count=1 2>/dev/null | head -n 1)" = "probe-$n" ]; then
        probe disk "$n" ok "$n"
      else
        probe disk "$n" error "$n"
      fi
      sleep 2
    done
  ) &
) &
