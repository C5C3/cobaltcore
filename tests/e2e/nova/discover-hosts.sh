#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/nova/discover-hosts.sh: map a nova-compute host into cell1 and wait
# until nova knows about it.
#
# A compute service registers itself in the cell database when it starts, but
# the scheduler only places servers on a host that also has a host mapping in
# the API database. nova-scheduler writes those mappings from a 300-second
# periodic, which is longer than a suite can spend before it boots its first
# server, so the suites run the same discovery by hand through this helper.
#
# The discovery runs in the conductor pod because that workload carries both
# database connections: OS_API_DATABASE__CONNECTION holds the host mappings and
# OS_DATABASE__CONNECTION holds the cell the compute registered in
# (operators/nova/internal/controller/reconcile_conductor.go). The rendered
# configuration is mounted there too, so nova-manage reads what the services
# read.
#
# The two failure paths are kept apart because they point at different things.
# A discover_hosts that exits non-zero means the command never ran against the
# databases (no conductor pod to exec into, or nova-manage refusing on a
# database error), so the helper reports the exit code and stops at once. A
# host that never turns up in list_hosts means the compute has not registered
# itself, so the helper reports the table it read last.
#
# Usage: discover-hosts.sh <nova-name> <namespace> [host]
#
# Chainsaw runs script steps in the suite directory, so the calls read
# "../discover-hosts.sh ...". Discovery is idempotent: a host that already has
# its mapping is skipped, so a repeated round or a retried step converges.

set -euo pipefail

# The conductor side is fixed by the operator: the Deployment is named
# <nova>-conductor with the service in the "conductor" container
# (reconcile_conductor.go), and the configuration directory is the one the
# workloads pass to --config-dir (reconcile_config.go).
CONTAINER="conductor"
CONFIG_DIR="/etc/nova/nova.conf.d"

# Discovery and lookup repeat until the deadline, 5 seconds apart. A Deployment
# is Available as soon as its container runs, which is some 20 seconds before
# nova-compute writes its service and compute node records, and discover_hosts
# only maps a compute that has registered. One discovery in front of the poll
# therefore finds nothing and leaves list_hosts empty for good. The deadline is
# counted in seconds rather than attempts because each round costs two kubectl
# execs, and it stays inside the 3m the calling steps allow so the report
# below is printed before chainsaw kills the script.
DEADLINE=150
INTERVAL=5

usage() {
  echo "usage: discover-hosts.sh <nova-name> <namespace> [host]" >&2
  exit 2
}

# nova_manage runs nova-manage inside the conductor pod. A failure (an absent
# Deployment, a database nova-manage cannot reach) travels out with the exit
# code of kubectl or of nova-manage.
nova_manage() {
  kubectl exec -n "${NS}" "deploy/${NOVA}-conductor" -c "${CONTAINER}" -- \
    nova-manage --config-dir "${CONFIG_DIR}" "$@"
}

if [ "$#" -lt 2 ] || [ "$#" -gt 3 ]; then
  usage
fi

NOVA="$1"
NS="$2"
# The host to wait for. The fake-driver compute the suites deploy registers as
# fake-1. The nova tempest legs pass the kind node's name instead: the OVN
# chassis registers under that name, and neutron binds a port only to a host
# that has a live chassis, so their compute has to run under it too.
HOST="${3:-fake-1}"

TABLE=""
SECONDS=0
while :; do
  # --verbose names the hosts it mapped, which is the record of what discovery
  # saw in this round.
  set +e
  nova_manage cell_v2 discover_hosts --verbose
  rc=$?
  set -e
  if [ "${rc}" -ne 0 ]; then
    echo "ERROR: discover_hosts exited ${rc}" >&2
    exit 1
  fi

  # list_hosts prints one prettytable with the columns Cell Name, Cell UUID and
  # Hostname. Splitting on the pipes puts Hostname in field 4, since field 1 is
  # the empty string in front of the first pipe, and the borders between the
  # rows carry no pipe at all. Failures are kept in the table so the error text
  # reaches the report below. The match is whole-line and fixed-string: the host
  # comes from the caller and a node name carries dots, which grep would
  # otherwise read as wildcards.
  TABLE="$(nova_manage cell_v2 list_hosts 2>&1 || true)"
  if printf '%s\n' "${TABLE}" |
    awk -F'|' 'NF>=4 {gsub(/^ +| +$/, "", $4); print $4}' | grep -qxF "${HOST}"; then
    echo "OK: host ${HOST} mapped into cell1"
    exit 0
  fi
  if [ "${SECONDS}" -ge "${DEADLINE}" ]; then
    break
  fi
  sleep "${INTERVAL}"
done

echo "ERROR: host ${HOST} not mapped after ${SECONDS}s" >&2
printf '%s\n' "${TABLE}" >&2
exit 1
