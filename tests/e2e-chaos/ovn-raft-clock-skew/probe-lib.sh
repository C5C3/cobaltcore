#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Helpers of ovn-raft-clock-skew, sourced by its scripts from the suite
# directory with NAMESPACE set by Chainsaw. Every call runs in the ovsdb
# container of a database pod, on the sockets under /var/run/ovn
# (operators/ovn/internal/controller/reconcile_database.go).

# shellcheck source=tests/e2e-chaos/lib/faults.sh
. ../lib/faults.sh

CR=ovn-clock-chaos
STATE=ovn-raft-clock-skew-state

# sb_pods: the names of the Southbound members, one per line.
sb_pods() {
  kubectl get pods -n "$NAMESPACE" -l "app.kubernetes.io/instance=${CR},app.kubernetes.io/component=sb" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'
}

# nb_pod: the name of the Northbound member.
nb_pod() {
  kubectl get pods -n "$NAMESPACE" -l "app.kubernetes.io/instance=${CR},app.kubernetes.io/component=nb" \
    -o jsonpath='{.items[0].metadata.name}'
}

# sb_status <pod>: cluster/status of OVN_Southbound as the member reports it.
sb_status() {
  kubectl exec "$1" -n "$NAMESPACE" -c ovsdb -- \
    ovs-appctl -t /var/run/ovn/ovnsb_db.ctl cluster/status OVN_Southbound
}

# field <status> <name>: the first word of a "<name>: <value>" line of a
# cluster/status answer, such as Role, Term, Leader or Server ID.
field() {
  sed -n "s/^$2: *//p" <<<"$1" | head -n1 | awk '{print $1}'
}

# member_log <pod>: the container log of a member and the ovsdb-server log
# file ovn-ctl writes under /var/log/ovn.
member_log() {
  kubectl logs "$1" -n "$NAMESPACE" -c ovsdb --tail=500 2>&1 || true
  kubectl exec "$1" -n "$NAMESPACE" -c ovsdb -- sh -c 'tail -n 500 /var/log/ovn/*.log' 2>&1 || true
}

# log_clock <pod>: the Unix time ovsdb-server in the member's ovsdb container
# stamps on a log line right now. `ovs-appctl vlog/message chaos-canary` makes
# it write one ("user-message: chaos-canary") to the file ovn-ctl gave it under
# /var/log/ovn, and the newest such line is read back through log_line_epoch.
# `vlog/reopen` is no substitute: ovsdb-server reopens a log file that was not
# rotated without writing anything, so the newest line would be whatever it
# last logged on its own, stamped before the fault.
log_clock() {
  local line
  kubectl exec "$1" -n "$NAMESPACE" -c ovsdb -- \
    ovs-appctl -t /var/run/ovn/ovnsb_db.ctl vlog/message chaos-canary >/dev/null || return 1
  line="$( { kubectl exec "$1" -n "$NAMESPACE" -c ovsdb -- sh -c 'tail -qn 20 /var/log/ovn/*.log' || true; } |
    { grep -E '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z\|.*\|user-message: chaos-canary$' || true; } | tail -n1)"
  if [ -z "$line" ]; then
    echo "FAIL: no 'user-message: chaos-canary' line in the ovsdb-server log of ${1}"
    return 1
  fi
  log_line_epoch "$line"
}

# log_line_epoch <line>: the Unix time of the timestamp an ovsdb-server log
# line starts with, 2026-10-18T01:47:36.061Z|00012|raft|INFO|... in the default
# pattern. A line without one prints a FAIL line and returns 1.
log_line_epoch() {
  local ts
  ts="$(sed -nE 's/^([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(\.[0-9]+)?Z\|.*/\1Z/p' <<<"$1")"
  if [ -z "$ts" ]; then
    echo "FAIL: no timestamp in the ovsdb-server log line '${1}'"
    return 1
  fi
  date -u -d "$ts" +%s
}
