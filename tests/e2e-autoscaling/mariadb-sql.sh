#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e-autoscaling/mariadb-sql.sh: run one SQL statement as root on the
# suite's MariaDB and print the result rows without column names.
#
# Usage:
#   mariadb-sql.sh <statement>
#
# The root password comes from the Secret the live MariaDB CR's
# spec.rootPasswordSecretKeyRef names, the way
# deploy/openbao/bootstrap/setup-database-tenant.sh resolves it. Like that
# script, it passes the password over stdin: the mariadb client reads it as
# MYSQL_PWD, so it is on no command line, neither kubectl's on the runner nor
# the client's inside the pod.

set -euo pipefail

NS="openstack"
MARIADB="openstack-db"

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <statement>" >&2
  exit 2
fi

name="$(kubectl get mariadb "${MARIADB}" -n "${NS}" -o 'jsonpath={.spec.rootPasswordSecretKeyRef.name}')"
key="$(kubectl get mariadb "${MARIADB}" -n "${NS}" -o 'jsonpath={.spec.rootPasswordSecretKeyRef.key}')"
if [[ -z "${name}" ]]; then
  echo "ERROR: MariaDB ${MARIADB} names no spec.rootPasswordSecretKeyRef" >&2
  exit 1
fi
password="$(kubectl get secret "${name}" -n "${NS}" -o "jsonpath={.data.${key:-password}}" | base64 -d)"
if [[ -z "${password}" ]]; then
  echo "ERROR: the MariaDB root Secret ${name} carries no ${key:-password}" >&2
  exit 1
fi

# shellcheck disable=SC2016 # $1 and $(cat) expand in the pod's shell.
printf '%s' "${password}" | kubectl exec -i -n "${NS}" "${MARIADB}-0" -c mariadb -- \
  sh -c 'export MYSQL_PWD; MYSQL_PWD="$(cat)"; exec mariadb -uroot -N -e "$1"' sh "$1"
