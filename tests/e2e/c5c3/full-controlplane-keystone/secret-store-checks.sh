#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/c5c3/full-controlplane-keystone/secret-store-checks.sh: the
# checks run_openstack_verify makes on the Barbican secret store after the
# verify Job of Link 7b and Link 8e.
#
# The step 1 script of chainsaw-test.yaml sources this file among its
# helpers:
#
#   . ./secret-store-checks.sh
#
# Chainsaw hands a script's content to bash as one argument, and Linux caps
# one argument at 128 KiB (MAX_ARG_STRLEN). With these checks inline the
# step 1 script passes that cap and bash never starts. Sourced, the functions
# run in the step's own shell: they read CP_NS and BARBICAN_BAO from it, run
# under its set -euo pipefail, and their exit 1 ends the step.

# The secret_store_metadata rows that carry the OpenBao storage index of a
# Barbican write; run_openstack_verify counts them around the verify Job.
VAULT_INDEX_SQL="SELECT COUNT(*) FROM barbican.secret_store_metadata WHERE \`key\` = 'vault_index'"

# One SQL statement as root on the owned MariaDB, rows printed without
# column names. The root password comes from the Secret the MariaDB
# CR's spec.rootPasswordSecretKeyRef names and goes over stdin as
# MYSQL_PWD, so it is on no command line. Callers capture stdout, so
# the errors go to stderr.
cp_sql() { # <statement>
  local secret key password
  secret="$(kubectl get mariadb openstack-db -n "${CP_NS}" -o 'jsonpath={.spec.rootPasswordSecretKeyRef.name}')"
  key="$(kubectl get mariadb openstack-db -n "${CP_NS}" -o 'jsonpath={.spec.rootPasswordSecretKeyRef.key}')"
  [ -n "${secret}" ] \
    || { echo "ERROR: MariaDB openstack-db names no spec.rootPasswordSecretKeyRef" >&2; return 1; }
  password="$(kubectl get secret "${secret}" -n "${CP_NS}" -o "jsonpath={.data.${key:-password}}" | base64 -d)"
  [ -n "${password}" ] \
    || { echo "ERROR: the MariaDB root Secret ${secret} carries no ${key:-password}" >&2; return 1; }
  printf '%s' "${password}" | kubectl exec -i -n "${CP_NS}" openstack-db-0 -c mariadb -- \
    sh -c 'export MYSQL_PWD; MYSQL_PWD="$(cat)"; exec mariadb -uroot -N -e "$1"' sh "$1"
}

# The storage index of the Job's Barbican writes. OpenBao 2.7 answers a
# write with X-Vault-Index, the Barbican image records it in a vault_index
# row of secret_store_metadata and sends it back with the read, and a
# malformed header would have failed the Job's payload comparison with
# HTTP 400. The secret round-trip and the encrypted volume's key both
# write, so the Job adds at least one row.
assert_vault_index_rows_added() { # <rows before the Job>
  local before="$1" after
  after="$(cp_sql "${VAULT_INDEX_SQL}")"
  if ! [[ "${before}" =~ ^[0-9]+$ && "${after}" =~ ^[0-9]+$ ]]; then
    echo "ERROR: counting vault_index rows returned '${before}' before and '${after}' after the Job, not two numbers"
    exit 1
  fi
  if [ "${after}" -lt 1 ] || [ "${after}" -le "${before}" ]; then
    echo "ERROR: barbican.secret_store_metadata holds ${after} vault_index rows after the Job and ${before} before it; the Barbican image recorded no OpenBao storage index for the Job's writes"
    exit 1
  fi
  echo "OK: the Job's Barbican writes recorded $((after - before)) vault_index rows (${after} in total)"
}

# The sizing the dedicated instance runs under. The CRD of
# openbao-operator 0.5 stores spec.resources (0.4 pruned it), and the
# voter container runs under spec.sizing.secretStore of the ControlPlane
# (Minimal: a 64Mi memory limit). A limit too small for OpenBao shows up
# as an OOMKilled container, which only a container that runs under the
# limit can be.
assert_secret_store_sizing() {
  local limit pods limits terminations
  limit="$(kubectl get openbaoclusters.openbao.org "${BARBICAN_BAO}" -n "${CP_NS}" \
    -o 'jsonpath={.spec.resources.limits.memory}')"
  test -n "${limit}" \
    || { echo "ERROR: dedicated OpenBaoCluster ${BARBICAN_BAO} returns no spec.resources.limits.memory; the CRD pruned the projected sizing"; exit 1; }
  echo "OK: dedicated OpenBaoCluster stores spec.resources.limits.memory ${limit}"
  pods="$(kubectl get pods -n "${CP_NS}" \
    -l "app.kubernetes.io/name=openbao,openbao.org/cluster=${BARBICAN_BAO}" -o name)"
  test -n "${pods}" \
    || { echo "ERROR: no pod of the dedicated OpenBaoCluster ${BARBICAN_BAO}"; exit 1; }
  # One line per container: its name and its memory limit.
  limits="$(kubectl get pods -n "${CP_NS}" \
    -l "app.kubernetes.io/name=openbao,openbao.org/cluster=${BARBICAN_BAO}" \
    -o 'jsonpath={range .items[*]}{range .spec.containers[*]}{.name}={.resources.limits.memory}{"\n"}{end}{end}')"
  if grep -qv "=${limit}$" <<<"${limits}"; then
    echo "ERROR: a container of the dedicated OpenBaoCluster ${BARBICAN_BAO} does not run under the projected ${limit} memory limit:"
    printf '%s\n' "${limits}"
    exit 1
  fi
  echo "OK: every container of the dedicated OpenBaoCluster ${BARBICAN_BAO} runs under the ${limit} memory limit"
  # One line per container, init containers included: its name, the
  # reason it terminated with, and the reason its last run did.
  terminations="$(kubectl get pods -n "${CP_NS}" \
    -l "app.kubernetes.io/name=openbao,openbao.org/cluster=${BARBICAN_BAO}" \
    -o 'jsonpath={range .items[*]}{range .status.initContainerStatuses[*]}{.name} {.state.terminated.reason} {.lastState.terminated.reason}{"\n"}{end}{range .status.containerStatuses[*]}{.name} {.state.terminated.reason} {.lastState.terminated.reason}{"\n"}{end}{end}')"
  if grep -q OOMKilled <<<"${terminations}"; then
    echo "ERROR: a container of the dedicated OpenBaoCluster ${BARBICAN_BAO} was OOMKilled under its ${limit} limit:"
    printf '%s\n' "${terminations}"
    exit 1
  fi
  echo "OK: no container of the dedicated OpenBaoCluster ${BARBICAN_BAO} was OOMKilled"
}
