#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Helpers of keystone-operator-clock-skew, sourced by its scripts from the
# suite directory with NAMESPACE set by Chainsaw.

# shellcheck source=tests/e2e-chaos/lib/faults.sh
. ../lib/faults.sh

OPERATOR_NS=keystone-system
LEASE=keystone.openstack.c5c3.io
STATE=operator-clock-skew-state

# lease_holder: the pod that holds the Lease, the part of holderIdentity before
# its first underscore; empty when the Lease or its holder is missing.
lease_holder() {
  local holder
  holder="$(kubectl get lease "$LEASE" -n "$OPERATOR_NS" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)"
  printf '%s' "${holder%%_*}"
}

# restarts <pod>: the restartCount of the manager container of an operator pod.
restarts() {
  kubectl get pod "$1" -n "$OPERATOR_NS" \
    -o jsonpath='{.status.containerStatuses[?(@.name=="manager")].restartCount}'
}

# wait_available <deployment> <replicas> <seconds>: wait until the Deployment in
# NAMESPACE has spec.replicas and availableReplicas both at <replicas>.
wait_available() {
  local deploy="$1" want="$2" deadline got
  deadline=$(($(date +%s) + $3))
  while true; do
    got="$(kubectl get deployment "$deploy" -n "$NAMESPACE" \
      -o jsonpath='{.spec.replicas}/{.status.availableReplicas}' 2>/dev/null || true)"
    if [ "$got" = "${want}/${want}" ]; then
      return 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "FAIL: deployment ${deploy} reads replicas/available ${got:-<none>}, want ${want}/${want} within $3s"
      return 1
    fi
    sleep 5
  done
}

# cert_offset_hours: the whole hours from now to the notAfter of the API
# server's serving certificate, rounded up, plus 24, read with openssl from
# the server of the current kubeconfig context. A missing openssl or a date it
# cannot parse prints the FAIL line of the suite and returns 1.
cert_offset_hours() {
  local server hostport host enddate notafter now
  if ! command -v openssl >/dev/null 2>&1; then
    echo "FAIL: cannot read the API server certificate's notAfter: openssl is not on PATH"
    return 1
  fi
  server="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
  hostport="${server#*://}"
  hostport="${hostport%%/*}"
  host="${hostport%:*}"
  enddate="$(openssl s_client -connect "$hostport" -servername "$host" </dev/null 2>/dev/null |
    openssl x509 -noout -enddate 2>&1 || true)"
  if ! notafter="$(date -u -d "${enddate#notAfter=}" +%s 2>/dev/null)" || [ "${enddate#notAfter=}" = "$enddate" ]; then
    echo "FAIL: cannot read the API server certificate's notAfter: ${server} answered '${enddate}'"
    return 1
  fi
  now="$(date -u +%s)"
  echo "$(((notafter - now + 3599) / 3600 + 24))"
}
