#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# setup-database-tenant.sh — Provision the per-tenant MariaDB database-engine
# connection and role for one MANAGED ControlPlane's Keystone service DB user.
#
# MODE: this is a managed-database onboarding step. An External-mode ControlPlane
# (spec.services.keystone.mode: External) has NO managed database — the c5c3
# operator's reconcileDBCredentials is skipped for it — so it must not be
# onboarded here; there is no MariaDB CR to read and no DB credential path to
# issue.
#
# This is the stage-(b) counterpart to the bootstrap engine mount performed by
# setup-secret-engines.sh (enable_database). The database secrets engine is
# mounted at database/mariadb during bootstrap, but per-tenant connection and
# role configuration cannot happen there: the managed MariaDB instances do not
# exist at bootstrap time. This script is therefore run once per ControlPlane,
# after its MariaDB is Ready, to configure:
#
#   - database/mariadb/config/keystone-<namespace>
#       the connection to the ControlPlane's MariaDB, authenticated as root.
#   - database/mariadb/roles/keystone-<namespace>
#       a role that issues short-lived MySQL users with ALL PRIVILEGES on the
#       Keystone database and auto-revokes them at lease end.
#
# The role is keyed on the ControlPlane NAMESPACE alone: the
# one-ControlPlane-per-namespace admission contract makes the namespace a unique
# tenant key, and cluster-unique namespaces keep the role name collision-free (a
# hyphen-joined <namespace>-<controlplane> would be ambiguous — e.g. ns=a-b/name=c
# and ns=a/name=b-c both flatten to keystone-a-b-c and would overwrite each other's
# connection config on the second onboarding). Namespace-only keying is also what
# lets the keystone-db-dynamic policy scope reads to the caller's OWN namespace
# with an exact ACL-template match (no over-matching wildcard).
#
# Engine-issued credentials are then read at database/mariadb/creds/<role> by
# the c5c3 operator's per-ControlPlane VaultDynamicSecret generator
# (reconcile_dbcredentials.go). The role name derivation below MUST stay in sync
# with dbDynamicRoleFor in operators/c5c3/internal/controller/reconcile_dbcredentials.go.
#
# Idempotent: config and role writes are upserts, so re-running refreshes a
# rotated root password or an updated database name.
#
# Usage: setup-database-tenant.sh <namespace> <controlplane>
# Requires: BAO_TOKEN in the environment (kubectl access to the openbao-0 pod).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

###############################################################################
# Configuration
###############################################################################
BAO_TOKEN="${BAO_TOKEN:?BAO_TOKEN must be set}"

# Lease TTLs for issued credentials. default_ttl MUST stay above the c5c3
# operator's DB-credential ExternalSecret refreshInterval (24h) so ESO re-issues a
# fresh lease before the previous one expires; the default_ttl − refreshInterval
# gap (48h − 24h = 24h) is the window the keystone operator has to roll the pods
# onto a fresh credential before the previous, still-in-use lease is revoked. A
# full-day gap means a stalled rollout (bad image, resource pressure) has to
# persist for a day — long enough to page on-call — before a running Keystone
# loses DB access, instead of silently arming an outage within a couple of hours.
# max_ttl caps the absolute lifetime of any issued credential. Override via the
# environment to tune the rotation cadence.
#
# INVARIANT: these TTLs only bind while the minting auth token outlives them.
# OpenBao revokes a dynamic-secret lease together with the token that created
# it, so the keystone-db auth role (setup-auth.sh) pins its token_ttl/
# token_max_ttl to DB_CREDS_MAX_TTL. Raising DB_CREDS_* beyond 72h without
# raising the role's token TTLs silently caps the effective credential
# lifetime at the token's — the failure mode that once killed running
# Keystones hourly under a 1h token.
DB_CREDS_DEFAULT_TTL="${DB_CREDS_DEFAULT_TTL:-48h}"
DB_CREDS_MAX_TTL="${DB_CREDS_MAX_TTL:-72h}"

CP_NS="${1:?usage: setup-database-tenant.sh <namespace> <controlplane>}"
CP_NAME="${2:?usage: setup-database-tenant.sh <namespace> <controlplane>}"

# get_controlplane_field prints a jsonpath field from the live ControlPlane CR,
# or the supplied default when the field is empty/absent. Callers MUST verify the
# ControlPlane CR exists first (see main): this fallback is only meaningful for a
# genuinely-unset field, not a failed lookup — the two are indistinguishable here.
get_controlplane_field() {
  local jsonpath="$1"
  local default="$2"
  local value
  value="$(kubectl get controlplane "${CP_NAME}" -n "${CP_NS}" \
    -o "jsonpath=${jsonpath}" 2>/dev/null || true)"
  if [[ -z "${value}" ]]; then
    echo "${default}"
  else
    echo "${value}"
  fi
}

###############################################################################
# Main
###############################################################################
main() {
  # Keyed on the ControlPlane namespace alone (see header): unique + collision-free,
  # and matched exactly by the keystone-db-dynamic templated policy.
  local role_name="keystone-${CP_NS}"
  local config_name="${role_name}"

  log "=== Provisioning MariaDB database-engine tenant '${CP_NS}/${CP_NAME}' ==="
  log "Namespace : ${NAMESPACE}"
  log "BAO_ADDR  : ${BAO_ADDR}"
  log "Role      : database/mariadb/roles/${role_name}"

  # Fail loudly if the ControlPlane CR cannot be read. get_controlplane_field
  # below falls back to the projection defaults (openstack-db / keystone) on an
  # empty result, which is correct for a genuinely-unset field but would silently
  # mask a missing CR or an unreachable cluster — the script would then provision
  # a database-engine tenant against those defaults for a ControlPlane that does
  # not exist. Verifying existence up front keeps the empty-vs-absent fallback
  # unambiguous.
  if ! kubectl get controlplane "${CP_NAME}" -n "${CP_NS}" >/dev/null 2>&1; then
    log "ERROR: ControlPlane '${CP_NAME}' not found in namespace '${CP_NS}' (or the cluster is unreachable)."
    exit 1
  fi

  # Resolve the MariaDB cluster name and Keystone database name from the live
  # ControlPlane spec. Defaults mirror the c5c3 operator's projection defaults
  # (openstack-db / keystone) so a ControlPlane that leaves them unset resolves
  # to the same values the operator projects.
  local mariadb_name database_name
  mariadb_name="$(get_controlplane_field '{.spec.infrastructure.database.clusterRef.name}' 'openstack-db')"
  database_name="$(get_controlplane_field '{.spec.infrastructure.database.database}' 'keystone')"

  log "MariaDB   : ${mariadb_name}.${CP_NS}.svc:3306"
  log "Database  : ${database_name}"

  # Resolve the MariaDB root credential from the effective
  # spec.rootPasswordSecretKeyRef on the live MariaDB CR. mariadb-operator
  # webhook-defaults this field when the CR is created without it, so reading it
  # back from the live CR avoids hardcoding the operator's Secret-naming
  # convention.
  local root_secret_name root_secret_key
  root_secret_name="$(kubectl get mariadb "${mariadb_name}" -n "${CP_NS}" \
    -o 'jsonpath={.spec.rootPasswordSecretKeyRef.name}' 2>/dev/null || true)"
  if [[ -z "${root_secret_name}" ]]; then
    log "ERROR: could not resolve spec.rootPasswordSecretKeyRef.name on MariaDB '${mariadb_name}' in namespace '${CP_NS}'."
    exit 1
  fi
  root_secret_key="$(kubectl get mariadb "${mariadb_name}" -n "${CP_NS}" \
    -o 'jsonpath={.spec.rootPasswordSecretKeyRef.key}' 2>/dev/null || true)"
  if [[ -z "${root_secret_key}" ]]; then
    root_secret_key="password"
  fi

  local root_password_b64 root_password
  root_password_b64="$(kubectl get secret "${root_secret_name}" -n "${CP_NS}" \
    -o "jsonpath={.data.${root_secret_key}}" 2>/dev/null || true)"
  if [[ -z "${root_password_b64}" ]]; then
    log "ERROR: MariaDB root Secret '${root_secret_name}' (key '${root_secret_key}') not found or empty in namespace '${CP_NS}'."
    exit 1
  fi
  root_password="$(echo "${root_password_b64}" | base64 -d)"

  # Write the connection config. The password is piped via stdin
  # (password=-) so the cleartext root password never appears in a process
  # argument list. verify_connection=false because the MariaDB may not yet be
  # accepting connections when this runs on a fresh cluster; the first
  # credential issuance validates the connection instead.
  log "Writing database/mariadb/config/${config_name} ..."
  printf '%s' "${root_password}" | bao_exec_stdin bao write "database/mariadb/config/${config_name}" \
    plugin_name=mysql-database-plugin \
    connection_url="{{username}}:{{password}}@tcp(${mariadb_name}.${CP_NS}.svc:3306)/" \
    allowed_roles="${role_name}" \
    username=root \
    password=- \
    verify_connection=false
  log "Connection config written."

  # Write the role. creation_statements creates a short-lived MySQL user with
  # ALL PRIVILEGES on the Keystone database; revocation_statements drops it at
  # lease end. The database identifier is backtick-quoted (escaped \` for the
  # bash double-quoted string) so a hyphenated database name is valid MySQL.
  local creation_stmt revocation_stmt
  creation_stmt="CREATE USER '{{name}}'@'%' IDENTIFIED BY '{{password}}'; GRANT ALL PRIVILEGES ON \`${database_name}\`.* TO '{{name}}'@'%';"
  revocation_stmt="DROP USER IF EXISTS '{{name}}'@'%';"

  log "Writing database/mariadb/roles/${role_name} (default_ttl=${DB_CREDS_DEFAULT_TTL}, max_ttl=${DB_CREDS_MAX_TTL}) ..."
  bao_exec bao write "database/mariadb/roles/${role_name}" \
    db_name="${config_name}" \
    creation_statements="${creation_stmt}" \
    revocation_statements="${revocation_stmt}" \
    default_ttl="${DB_CREDS_DEFAULT_TTL}" \
    max_ttl="${DB_CREDS_MAX_TTL}"
  log "Role written."

  log "=== Done ==="
}

main "$@"
