#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Helpers of mariadb-io-fault, sourced by its scripts from the suite directory
# with NAMESPACE set by Chainsaw.

# shellcheck source=tests/e2e-chaos/lib/faults.sh
. ../lib/faults.sh

MARIADB=mariadb-io-chaos
KEYSTONE_CR=keystone-chaos-io
WRITER=mariadb-io-writer

# ready <kind> <name>: the status of the CR's Ready condition.
ready() {
  kubectl get "$1" "$2" -n "$NAMESPACE" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true
}

# wait_mariadb_ready <seconds>: wait until the MariaDB reads Ready=True.
wait_mariadb_ready() {
  local deadline=$(($(date +%s) + $1))
  until [ "$(ready mariadb "$MARIADB")" = True ]; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "FAIL: mariadb ${MARIADB} is not Ready=True within $1s"
      kubectl get mariadb "$MARIADB" -n "$NAMESPACE" -o yaml 2>&1 | sed -n '/^status:/,$p'
      return 1
    fi
    sleep 5
  done
  echo "OK: mariadb ${MARIADB} is Ready=True"
}

# acked_writes: how many writes of the writer answered 201 so far.
acked_writes() {
  kubectl logs "$WRITER" -n "$NAMESPACE" 2>/dev/null | grep -cE '^WRITE [0-9]+ 201$' || true
}

# hold <seconds> <check-ready> [<want-new-writes>]: wait <seconds>, reading
# every 10 s; with check-ready set to yes, MariaDB and the Keystone must read
# Ready=True on every read, and with want-new-writes the writer must log at
# least one more acknowledged write than at the start.
hold() {
  local secs="$1" check="$2" want="${3:-}" before end
  before="$(acked_writes)"
  end=$(($(date +%s) + secs))
  while [ "$(date +%s)" -lt "$end" ]; do
    if [ "$check" = yes ]; then
      if [ "$(ready mariadb "$MARIADB")" != True ] || [ "$(ready keystone "$KEYSTONE_CR")" != True ]; then
        echo "FAIL: under the fault mariadb reads Ready=$(ready mariadb "$MARIADB") and the Keystone Ready=$(ready keystone "$KEYSTONE_CR")"
        return 1
      fi
    fi
    sleep 10
  done
  if [ -n "$want" ] && [ "$(acked_writes)" -le "$before" ]; then
    echo "FAIL: the writer logged no acknowledged write in ${secs}s"
    return 1
  fi
  echo "OK: held ${secs}s; the writer has $(acked_writes) acknowledged writes"
}

# mariadb_check <database>: mariadb-check of the database as root, the way
# tests/e2e-autoscaling/mariadb-sql.sh runs SQL: the root password goes over
# stdin, so it is on no command line.
mariadb_check() {
  local name key password
  name="$(kubectl get mariadb "$MARIADB" -n "$NAMESPACE" -o 'jsonpath={.spec.rootPasswordSecretKeyRef.name}')"
  key="$(kubectl get mariadb "$MARIADB" -n "$NAMESPACE" -o 'jsonpath={.spec.rootPasswordSecretKeyRef.key}')"
  password="$(kubectl get secret "$name" -n "$NAMESPACE" -o "jsonpath={.data.${key:-password}}" | base64 -d)"
  # shellcheck disable=SC2016 # $1 and $(cat) expand in the pod's shell.
  printf '%s' "$password" | kubectl exec -i -n "$NAMESPACE" "${MARIADB}-0" -c mariadb -- \
    sh -c 'export MYSQL_PWD; MYSQL_PWD="$(cat)"; exec mariadb-check -uroot "$1"' sh "$1"
}
