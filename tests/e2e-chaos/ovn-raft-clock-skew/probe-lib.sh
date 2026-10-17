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

# cert_offset_hours: the whole hours from now to the latest notAfter of the
# Certificates labelled app.kubernetes.io/instance=ovn-clock-chaos, rounded
# up, plus 24. Without such a Certificate it prints the FAIL line of the suite
# and returns 1.
cert_offset_hours() {
  local latest notafter now
  latest="$(kubectl get certificate -n "$NAMESPACE" -l "app.kubernetes.io/instance=${CR}" \
    -o jsonpath='{range .items[*]}{.status.notAfter}{"\n"}{end}' | grep . | sort | tail -n1 || true)"
  if [ -z "$latest" ]; then
    echo "FAIL: no Certificate labelled app.kubernetes.io/instance=${CR}"
    return 1
  fi
  notafter="$(date -u -d "$latest" +%s)" || return 1
  now="$(date -u +%s)"
  echo "$(((notafter - now + 3599) / 3600 + 24))"
}
