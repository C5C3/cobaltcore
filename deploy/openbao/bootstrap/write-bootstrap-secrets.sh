#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# write-bootstrap-secrets.sh — Write initial bootstrap secrets to OpenBao KV-v2.
#
# This script is idempotent: each secret is only written when it does not
# already exist in the KV store.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

###############################################################################
# Configuration
###############################################################################
BAO_TOKEN="${BAO_TOKEN:?BAO_TOKEN must be set}"

# KORC_CONTROLPLANES: whitespace-separated list of
# "<namespace>/<controlplane>[/<keystone-namespace>]" identities to seed
# per-ControlPlane bootstrap secrets.
#
# The optional third segment is the KEYSTONE SERVICE NAMESPACE — where the admin
# password is read from when spec.services.keystone.namespace places the Keystone
# service in a namespace of its own. It defaults to <namespace>, so a ControlPlane
# without a namespace assignment is spelled exactly as before. It must be given
# when there IS one: the seeded path must match adminPasswordRemoteKeyFor in the
# c5c3 operator (bootstrap/<keystone-namespace>/<controlplane>-keystone/admin),
# which follows the Keystone child so it agrees with the keystone-operator's
# rotation PushSecret — seeding under <namespace> instead would write a path
# nothing reads.
#
# MODE CONTRACT: entries are MANAGED-mode ControlPlane identities only. An
# External-mode ControlPlane (spec.services.keystone.mode: External) must NOT be
# listed here — nothing is seeded for it, and a listed entry would MIS-SEED it.
# In External mode the admin password of the pre-existing Keystone is owned
# out-of-band in the user-supplied passwordSecretRef Secret, and the c5c3
# operator's reconcileAdminPassword short-circuits (IsExternalKeystone) without
# projecting an ExternalSecret or reading any bootstrap path; the operator also
# rejects services.horizon in External mode, so the seeded Horizon secret-key
# would go unconsumed. Seeding an External-mode identity here would therefore
# write a generated admin password at bootstrap/<namespace>/<controlplane>-keystone/admin
# unrelated to the external installation's real one — a path nothing reads.
#
# For each MANAGED-mode identity the script seeds:
#   - the Model B admin password at  kv-v2/bootstrap/<keystone-namespace>/<controlplane>-keystone/admin
#   - the Horizon Django SECRET_KEY at kv-v2/bootstrap/<namespace>/<controlplane>-horizon/secret-key
#
# The Horizon secret-key path stays on <namespace>: it is a kind-only shim, read
# by an ExternalSecret in the namespace the dashboard's SECRET_KEY Secret lives
# in. A dashboard placed in a namespace of its own needs its own key material
# there — see the secretKeyRef note on ServiceHorizonSpec.
# The stage-(a) per-ControlPlane static DB credential seed is RETIRED (#439):
# managed-mode Keystone draws engine-issued short-lived DB credentials from the
# OpenBao database engine (see setup-database-tenant.sh), so no static DB
# password is seeded at rest. A single static credential for standalone
# (non-ControlPlane) Keystone demos is still seeded at
# kv-v2/openstack/keystone/openstack/standalone/db (brownfield-only).
# The K-ORC bootstrap clouds.yaml is no longer seeded here: the operator now seeds
# it from reconcileKORC (seedBootstrapCloudsYAML).
# The default is the single canonical ControlPlane the Quick Start brings up
# (deploy/kind/controlplane/controlplane.yaml: ControlPlane "controlplane" in
# namespace "openstack"), so the single-CR `make deploy-infra` path is unchanged
# (see hack/deploy-infra.sh). Override it to bring up several
# ControlPlanes (e.g. KORC_CONTROLPLANES="tenant-a/cp tenant-b/cp"); each entry
# MUST be "<namespace>/<controlplane>[/<keystone-namespace>]".
KORC_CONTROLPLANES="${KORC_CONTROLPLANES:-openstack/controlplane}"

# Marker value: use as a key's value to generate a cryptographically random
# password (base64, 32 bytes) inside the pod using OpenBao's sys/tools/random
# API.  Generating in-pod avoids cleartext passwords appearing in host process
# argument lists.
readonly GENERATED_PASSWORD="@generate"

# Write a secret only if it does not already exist in the KV-v2 store.
#
# Usage: write_secret_if_missing <kv_path> <key1>=<value1> [<key2>=<value2> ...]
#
# Use GENERATED_PASSWORD ("@generate") as a value to generate a random password
# inside the pod:  write_secret_if_missing "path" "password=@generate"
#
# The function checks whether the secret at <kv_path> is readable. If the
# secret exists (exit code 0), the write is skipped. Otherwise the secret is
# created with the supplied key/value pairs.
write_secret_if_missing() {
  local kv_path="$1"
  shift

  if bao_exec bao kv get -format=json "${kv_path}" >/dev/null 2>&1; then
    log "Secret '${kv_path}' already exists — skipping."
    return 0
  fi

  # Build bao kv put arguments, replacing @generate markers with in-pod
  # password generation so cleartext never travels as a kubectl exec arg.
  local put_args=""
  for arg in "$@"; do
    local key="${arg%%=*}"
    local val="${arg#*=}"
    if [[ "${val}" == "${GENERATED_PASSWORD}" ]]; then
      put_args+=" ${key}=\"\$(bao write -field=random_bytes sys/tools/random/32 format=hex)\""
    else
      # Quote non-generated values to guard against shell metacharacters
      # (spaces, equals signs in values, etc.) in the sh -c command string.
      # Escape embedded single quotes: ' → '\''
      local escaped_val="${val//\'/\'\\\'\'}"
      put_args+=" ${key}='${escaped_val}'"
    fi
  done

  log "Writing secret '${kv_path}'..."
  # Pass kv_path as an env var (BAO_KV_PATH) to the inner shell rather than
  # interpolating it into the sh -c command string. This avoids shell injection
  # if a future caller passes a path containing single quotes or metacharacters.
  kubectl exec -n "$NAMESPACE" openbao-0 -- \
    env BAO_ADDR="${BAO_ADDR}" BAO_TOKEN="${BAO_TOKEN}" VAULT_CACERT="${VAULT_CACERT}" \
    BAO_KV_PATH="${kv_path}" \
    sh -c "bao kv put \"\${BAO_KV_PATH}\" ${put_args}"
  log "Secret '${kv_path}' written."
}

# Mark a KV-v2 path as managed by the External Secrets Operator so its
# Vault/OpenBao PushSecret provider will adopt and overwrite it.
#
# Usage: mark_eso_managed <kv_path>
#
# ESO refuses to push to a pre-existing secret whose metadata lacks
# custom_metadata.managed-by=external-secrets and fails with the error
# "secret not managed by external-secrets"
# (external-secrets providers/v1/vault/client_push.go). A path seeded above with
# `bao kv put` carries no custom_metadata, so the Model B scheduled
# admin-password rotation backup PushSecret (per-CR RemoteKey bootstrap/{namespace}/{keystone}/admin, see operators/keystone/internal/controller/reconcile_passwordrotation.go)
# could never mirror the rotated password back into OpenBao without this marker
# Stamping the exact marker ESO itself writes lets ESO treat the
# seeded value as its own on the first push.
#
# Idempotent and migration-safe: it runs unconditionally (not gated on
# write_secret_if_missing's existence check), so it also marks a secret that a
# prior deploy already created without the marker. `bao kv metadata put` updates
# only the metadata — the stored password versions are left untouched.
mark_eso_managed() {
  local kv_path="$1"

  log "Marking '${kv_path}' as managed-by=external-secrets (ESO PushSecret adoption)..."
  kubectl exec -n "$NAMESPACE" openbao-0 -- \
    env BAO_ADDR="${BAO_ADDR}" BAO_TOKEN="${BAO_TOKEN}" VAULT_CACERT="${VAULT_CACERT}" \
    BAO_KV_PATH="${kv_path}" \
    sh -c "bao kv metadata put -custom-metadata=managed-by=external-secrets \"\${BAO_KV_PATH}\""
  log "Secret '${kv_path}' marked ESO-managed."
}

###############################################################################
# Main
###############################################################################
main() {
  log "=== Writing bootstrap secrets ==="
  log "Namespace : ${NAMESPACE}"
  log "BAO_ADDR  : ${BAO_ADDR}"

  write_secret_if_missing "kv-v2/infrastructure/mariadb" \
    "root-password=${GENERATED_PASSWORD}"

  # per-ControlPlane bootstrap seeding. For each MANAGED-mode
  # "<namespace>/<controlplane>" identity in KORC_CONTROLPLANES (default
  # "openstack/controlplane"), seed the per-CR Model B admin password and Horizon
  # secret-key on the per-CR OpenBao paths so two ControlPlanes never collide on
  # the cluster-global OpenBao backend. An External-mode ControlPlane is NEVER
  # seeded here (see the KORC_CONTROLPLANES contract above): its admin password is
  # user-supplied and reconcileAdminPassword short-circuits without reading any
  # bootstrap path. The K-ORC bootstrap clouds.yaml is now seeded by the
  # operator's reconcileKORC (seedBootstrapCloudsYAML) rather than here.
  # The legacy flat writes (bootstrap/keystone-admin,
  # openstack/keystone/admin/app-credential) are gone: the keystone-operator Model
  # B rotation PushSecret and the c5c3-operator admin AC PushSecret now target
  # bootstrap/{ns}/{keystone}/admin and
  # openstack/keystone/{ns}/{cp}/admin/app-credential respectively (/).
  # shellcheck disable=SC2086  # KORC_CONTROLPLANES is intentionally word-split on whitespace into identities.
  for identity in ${KORC_CONTROLPLANES}; do
    if [[ "${identity}" != */* ]]; then
      log "ERROR: KORC_CONTROLPLANES entry '${identity}' is not in '<namespace>/<controlplane>[/<keystone-namespace>]' form."
      exit 1
    fi
    local cp_ns cp_name keystone_ns rest
    cp_ns="${identity%%/*}"
    rest="${identity#*/}"
    cp_name="${rest%%/*}"
    # Third segment: the Keystone service namespace. Absent (no further "/") it
    # defaults to the ControlPlane's own namespace — the shape of a ControlPlane
    # that places no service in a namespace of its own.
    if [[ "${rest}" == */* ]]; then
      keystone_ns="${rest#*/}"
    else
      keystone_ns="${cp_ns}"
    fi
    if [[ -z "${cp_name}" || -z "${keystone_ns}" || "${keystone_ns}" == */* ]]; then
      log "ERROR: KORC_CONTROLPLANES entry '${identity}' is not in '<namespace>/<controlplane>[/<keystone-namespace>]' form."
      exit 1
    fi
    local keystone_name="${cp_name}-keystone"
    local horizon_name="${cp_name}-horizon"
    # The admin path follows the KEYSTONE service namespace so it matches
    # adminPasswordRemoteKeyFor and the keystone-operator's rotation PushSecret.
    local admin_path="kv-v2/bootstrap/${keystone_ns}/${keystone_name}/admin"
    local horizon_secret_key_path="kv-v2/bootstrap/${cp_ns}/${horizon_name}/secret-key"

    log "--- Seeding ControlPlane '${cp_ns}/${cp_name}' (keystone='${keystone_name}' in '${keystone_ns}', horizon='${horizon_name}') ---"

    # Model B admin password. This bootstrap source is now read by (a) the
    # operator-projected per-ControlPlane admin-password ExternalSecret (c5c3 operator reconcileAdminPassword) and (b) the kind-only
    # default-identity shim
    # (deploy/kind/infrastructure/keystone-admin-externalsecret.yaml); it is
    # later overwritten by the keystone-operator Model B rotation PushSecret.
    write_secret_if_missing "${admin_path}" \
      "password=${GENERATED_PASSWORD}"

    # Horizon Django SECRET_KEY. Read by (a) the kind-only default-identity
    # shim (deploy/kind/infrastructure/horizon-secret-key-externalsecret.yaml)
    # and (b) per-CR ExternalSecrets in test namespaces. Generated in-pod so
    # the cleartext never appears in host process argument lists. No
    # PushSecret targets this path, so no mark_eso_managed is needed.
    write_secret_if_missing "${horizon_secret_key_path}" \
      "secret-key=${GENERATED_PASSWORD}"
    # let the Model B admin-password rotation PushSecret overwrite this
    # pre-seeded path (ESO's managed-by guard rejects it otherwise). See
    # mark_eso_managed.
    mark_eso_managed "${admin_path}"

    # The stage-(a) per-ControlPlane STATIC DB credential seed
    # (kv-v2/openstack/keystone/{ns}/{cp}/db) is RETIRED here (#439): managed-mode
    # Keystone now draws short-lived, engine-issued DB credentials from the
    # OpenBao database engine (database/mariadb/creds/keystone-{ns}), so no
    # long-lived static DB password is seeded at rest. A ControlPlane that
    # explicitly opts back into credentialsMode: Static must have its KV path
    # seeded manually (see docs/guides/migrate-keystone-db-to-dynamic-credentials.md).
  done

  # Standalone (non-ControlPlane) Keystone demos still use a static KV credential.
  # It is read by the kind-only keystone-db ExternalSecret
  # (deploy/kind/infrastructure/keystone-db-externalsecret.yaml) and is
  # deliberately DEMOTED to brownfield-only: no PushSecret targets it, so no
  # mark_eso_managed is needed.
  #
  # The path carries the standalone namespace as its first segment
  # (openstack/keystone/{namespace}/standalone/db) so the eso-tenant templated
  # policy — which scopes reads to openstack/keystone/{caller-namespace}/* — matches
  # it (#606). The keystone-db ExternalSecret now reaches this path through the
  # per-tenant openbao-tenant-store instead of the shared cluster store.
  write_secret_if_missing "kv-v2/openstack/keystone/openstack/standalone/db" \
    "username=keystone" \
    "password=${GENERATED_PASSWORD}"

  log "=== Done ==="
}

main "$@"
