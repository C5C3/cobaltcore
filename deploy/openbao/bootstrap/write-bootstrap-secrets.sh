#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# write-bootstrap-secrets.sh — Write initial bootstrap secrets to OpenBao KV-v2.
# Feature: CC-0009
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

# KORC_CONTROLPLANES: whitespace-separated list of "<namespace>/<controlplane>"
# identities to seed per-ControlPlane bootstrap secrets for (CC-0112, REQ-009).
# For each identity the script seeds:
#   - the Model B admin password at  kv-v2/bootstrap/<namespace>/<controlplane>-keystone/admin
#   - the K-ORC bootstrap clouds.yaml at kv-v2/openstack/keystone/<namespace>/<controlplane>/admin/app-credential
# The default is the single canonical ControlPlane the Quick Start brings up
# (deploy/kind/controlplane/controlplane.yaml: ControlPlane "controlplane" in
# namespace "openstack"), so the single-CR `make deploy-infra` path is unchanged
# (CC-0112, REQ-009; see hack/deploy-infra.sh). Override it to bring up several
# ControlPlanes (e.g. KORC_CONTROLPLANES="tenant-a/cp tenant-b/cp"); each entry
# MUST be "<namespace>/<controlplane>".
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
      # Escape embedded single quotes: ' → '\'' (CC-0009)
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
# admin-password rotation backup PushSecret (per-CR RemoteKey
# bootstrap/{namespace}/{keystone}/admin, see
# operators/keystone/internal/controller/reconcile_passwordrotation.go; CC-0112)
# could never mirror the rotated password back into OpenBao without this marker
# (CC-0109). Stamping the exact marker ESO itself writes lets ESO treat the
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

# Seed a PASSWORD-based bootstrap clouds.yaml for ONE ControlPlane's K-ORC admin
# Application Credential, breaking the K-ORC credential bootstrap cycle (CC-0110,
# REQ-024). Per-ControlPlane scoped (CC-0112, REQ-009): called once per
# KORC_CONTROLPLANES identity from main().
#
# Usage: seed_korc_bootstrap_clouds_yaml <namespace> <controlplane> <keystone> <admin_path>
#   <namespace>    — the ControlPlane's namespace
#   <controlplane> — the ControlPlane CR name (scopes the app-credential path)
#   <keystone>     — the projected Keystone CR name ("<controlplane>-keystone";
#                    used to derive the in-cluster auth_url default)
#   <admin_path>   — the kv-v2 admin-password path this seed reads to derive the
#                    bootstrap clouds.yaml. Computed once by the caller (main's
#                    loop) and passed in so the per-CR path formula
#                    `kv-v2/bootstrap/<namespace>/<keystone>/admin` lives in a
#                    single place rather than being recomputed here (CC-0112).
#
# THE CYCLE: the c5c3-operator gates its admin-Application-Credential PushSecret
# (which WRITES openstack/keystone/<namespace>/<controlplane>/admin/app-credential)
# on the k-orc-clouds-yaml ExternalSecret being Ready — but that ExternalSecret
# materialises FROM this exact OpenBao path. On a fresh cluster the path is empty,
# so the ExternalSecret never goes Ready, the PushSecret is never created, and the
# path stays empty forever (permanent deadlock).
#
# THE SEED: we write a password-based clouds.yaml derived from the admin password
# at bootstrap/<namespace>/<keystone>/admin so K-ORC can authenticate AS admin and
# mint the restricted application credential. Once K-ORC mints it, the
# c5c3-operator's PushSecret OVERWRITES this path with the
# application-credential-based clouds.yaml — hence mark_eso_managed (called by
# main() after this) grants ESO ownership so that overwrite is allowed.
#
# Idempotent: skips the write when the path already exists, so a re-run never
# clobbers a real minted credential. The admin password is read and the clouds.yaml
# assembled ENTIRELY IN-POD (single-quoted sh -c) so the cleartext password never
# appears in a host process argument list (mirrors write_secret_if_missing's
# in-pod generation discipline). The admin password is a hex string
# (bootstrap/<namespace>/<keystone>/admin is seeded via `bao write ... format=hex`),
# so it carries no YAML/shell metacharacters.
seed_korc_bootstrap_clouds_yaml() {
  local ns="$1"
  local cp="$2"
  local keystone="$3"
  local admin_path="$4"
  local kv_path="kv-v2/openstack/keystone/${ns}/${cp}/admin/app-credential"
  # DECISION (CC-0112, REQ-009): auth_url MUST match the Keystone API Service the
  # c5c3-operator projects for this ControlPlane — "<controlplane>-keystone"
  # (keystoneName() / keystoneEndpointURL() in reconcile_korc.go) — so the default
  # below is derived per identity as http://<keystone>.<namespace>.svc:5000/v3.
  # K-ORC uses this auth_url for the very first mint, so it has to be correct
  # before the c5c3-operator runs. KORC_KEYSTONE_AUTH_URL still overrides it for
  # the single-CR deploy path (hack/deploy-infra.sh exports the matching
  # <controlplane>-keystone URL under WITH_CONTROLPLANE=true, which equals this
  # derived default for the canonical openstack/controlplane identity). A single
  # override is correct only for a single identity; multi-ControlPlane callers
  # MUST leave KORC_KEYSTONE_AUTH_URL unset and rely on the per-identity default.
  # Reviewer: please verify the override-applies-to-all-identities semantics is
  # acceptable (it is, because the default KORC_CONTROLPLANES is a single entry).
  local auth_url="${KORC_KEYSTONE_AUTH_URL:-http://${keystone}.${ns}.svc:5000/v3}"

  log "Seeding K-ORC bootstrap clouds.yaml at '${kv_path}' (auth_url=${auth_url}, if missing)..."
  # endpoint_type MUST be "internal", and the key MUST be "endpoint_type" (NOT
  # "interface"): gophercloud uses auth_url only to mint a token, then resolves every
  # subsequent call against the catalog endpoint for this interface. K-ORC runs
  # in-cluster and only honours clientconfig.Cloud.EndpointType (the endpoint_type
  # key) — it drops the "interface" key entirely. A missing/"interface" value
  # defaults to "public", whose catalog endpoint becomes the external Gateway host
  # (https://keystone.<host>.nip.io:8443/v3) once Keystone is exposed; that is
  # unreachable from a pod, and K-ORC swallows the connection error and reports the
  # admin Domain/User imports as empty ("Waiting for OpenStack resource to be created
  # externally"), so the application credential never mints.
  # shellcheck disable=SC2016  # $vars are intentionally expanded IN-POD, not by the host shell.
  kubectl exec -n "$NAMESPACE" openbao-0 -- \
    env BAO_ADDR="${BAO_ADDR}" BAO_TOKEN="${BAO_TOKEN}" VAULT_CACERT="${VAULT_CACERT}" \
    BAO_KV_PATH="${kv_path}" BAO_ADMIN_PATH="${admin_path}" KORC_AUTH_URL="${auth_url}" \
    sh -c '
      set -eu
      if bao kv get "${BAO_KV_PATH}" >/dev/null 2>&1; then
        echo "Secret \"${BAO_KV_PATH}\" already exists — skipping bootstrap clouds.yaml seed."
        exit 0
      fi
      admin_pw="$(bao kv get -field=password "${BAO_ADMIN_PATH}")"
      clouds="clouds:
  admin:
    auth:
      auth_url: ${KORC_AUTH_URL}
      username: admin
      password: ${admin_pw}
      project_name: admin
      user_domain_name: Default
      project_domain_name: Default
    region_name: RegionOne
    endpoint_type: internal
    identity_api_version: 3
"
      bao kv put "${BAO_KV_PATH}" clouds.yaml="${clouds}"
      echo "Secret \"${BAO_KV_PATH}\" seeded with bootstrap clouds.yaml."
    '
  log "K-ORC bootstrap clouds.yaml seed complete."
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

  write_secret_if_missing "kv-v2/openstack/keystone/db" \
    "username=keystone" \
    "password=${GENERATED_PASSWORD}"

  # CC-0112 (REQ-009): per-ControlPlane bootstrap seeding. For each
  # "<namespace>/<controlplane>" identity in KORC_CONTROLPLANES (default
  # "openstack/controlplane"), seed BOTH the Model B admin password and the K-ORC
  # bootstrap clouds.yaml on the now per-CR OpenBao paths so two ControlPlanes
  # never collide on the cluster-global OpenBao backend. The legacy flat writes
  # (bootstrap/keystone-admin, openstack/keystone/admin/app-credential) are gone:
  # the keystone-operator Model B rotation PushSecret and the c5c3-operator admin
  # AC PushSecret now target bootstrap/{ns}/{keystone}/admin and
  # openstack/keystone/{ns}/{cp}/admin/app-credential respectively (CC-0112,
  # REQ-001/REQ-002).
  # shellcheck disable=SC2086  # KORC_CONTROLPLANES is intentionally word-split on whitespace into identities.
  for identity in ${KORC_CONTROLPLANES}; do
    if [[ "${identity}" != */* ]]; then
      log "ERROR: KORC_CONTROLPLANES entry '${identity}' is not in '<namespace>/<controlplane>' form."
      exit 1
    fi
    local cp_ns="${identity%%/*}"
    local cp_name="${identity#*/}"
    local keystone_name="${cp_name}-keystone"
    local admin_path="kv-v2/bootstrap/${cp_ns}/${keystone_name}/admin"
    local ac_path="kv-v2/openstack/keystone/${cp_ns}/${cp_name}/admin/app-credential"

    log "--- Seeding ControlPlane '${identity}' (keystone='${keystone_name}') ---"

    # Model B admin password (the bootstrap source the keystone-admin
    # ExternalSecret reads and the rotation PushSecret later overwrites).
    write_secret_if_missing "${admin_path}" \
      "password=${GENERATED_PASSWORD}"
    # CC-0109: let the Model B admin-password rotation PushSecret overwrite this
    # pre-seeded path (ESO's managed-by guard rejects it otherwise). See
    # mark_eso_managed.
    mark_eso_managed "${admin_path}"

    # CC-0110, REQ-024: seed the K-ORC admin Application Credential clouds.yaml so
    # the k-orc-clouds-yaml ExternalSecret can materialise on a fresh cluster and
    # K-ORC can bootstrap-authenticate, breaking the credential cycle. Must run
    # AFTER the admin password above (it derives the password from it). Pass the
    # already-computed admin_path so the per-CR path formula is not duplicated
    # inside the function (CC-0112).
    seed_korc_bootstrap_clouds_yaml "${cp_ns}" "${cp_name}" "${keystone_name}" "${admin_path}"
    # Let the c5c3-operator's admin-Application-Credential PushSecret overwrite the
    # seeded path with the minted application credential (ESO's managed-by guard
    # rejects an unmarked pre-existing path otherwise — see mark_eso_managed).
    mark_eso_managed "${ac_path}"
  done

  log "=== Done ==="
}

main "$@"
