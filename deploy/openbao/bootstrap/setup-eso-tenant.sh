#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# setup-eso-tenant.sh — Provision the in-cluster half of a STANDALONE (non-
# ControlPlane) Keystone/Horizon namespace's per-tenant OpenBao identity: the
# ServiceAccount, mTLS client Certificate, and namespaced SecretStore that let
# that namespace's ExternalSecrets and PushSecrets reach OpenBao as the
# "eso-tenant" role instead of the shared cluster identity.
#
# SCOPE: this is the MANUAL onboarding path for standalone Keystone/Horizon CRs
# (which have no operator above them to provision a tenant store) — for example
# the standalone dev shims hack/deploy-infra.sh brings up. A ControlPlane —
# Managed OR External — never needs it: the c5c3 operator provisions the SAME
# ServiceAccount / Certificate / SecretStore itself and DEFAULTS the control plane
# onto the store (reconcileESOTenantStore in
# operators/c5c3/internal/controller/reconcile_esotenant.go, which runs for every
# ControlPlane regardless of mode). Setting a ControlPlane's spec.secretStoreRef
# is therefore NOT an onboarding step — it is the opt-OUT override for a
# ControlPlane that manages its own store (StoreRefOverridden), the opposite of
# what a standalone CR does. This is the ESO counterpart to setup-database-tenant.sh
# (the standalone/managed split is analogous there too).
#
# The OpenBao side — the eso-tenant Kubernetes auth role and the eso-tenant
# templated policy — is created once at bootstrap by setup-auth.sh /
# setup-policies.sh (both provisioning routes converge on it); this script only
# creates the in-cluster objects a standalone namespace needs:
#
#   - ServiceAccount eso-tenant-auth
#       the identity ESO presents to OpenBao. The eso-tenant role binds this SA
#       name in ANY namespace; the eso-tenant policy then templates every path to
#       the caller's OWN namespace, so the tenant token is confined to its own
#       Keystone key/bootstrap material.
#   - Certificate eso-tenant-client-tls (cert-manager, ClusterIssuer openbao-ca-issuer)
#       the mTLS client certificate the OpenBao listener requires. The resulting
#       kubernetes.io/tls Secret carries tls.crt/tls.key AND ca.crt, so the
#       SecretStore below sources both its client cert and its CA trust anchor
#       from the same Secret (mirroring the DB-credential Certificate in
#       reconcile_dbcredentials.go).
#   - SecretStore openbao-tenant-store
#       the namespaced store a standalone Keystone/Horizon CR selects via
#       spec.secretStoreRef: {kind: SecretStore, name: openbao-tenant-store}. It
#       authenticates as the eso-tenant role with the eso-tenant-auth SA.
#
# After this runs and the SecretStore reports Ready, set the STANDALONE CR's
# spec.secretStoreRef to the namespaced store to route it through the per-tenant
# identity. On a cluster bootstrapped before this feature, re-run setup-auth.sh /
# setup-policies.sh first so the eso-tenant role and policy exist — otherwise
# pushes 403 and FernetKeysReady/CredentialKeysReady degrade.
#
# Idempotent: every object is applied with `kubectl apply`, so re-running
# refreshes the manifests without disrupting a live tenant.
#
# Usage: setup-eso-tenant.sh <namespace>
# Requires: kubectl access to the tenant's cluster.

set -euo pipefail

###############################################################################
# Configuration
###############################################################################
TENANT_NS="${1:?usage: setup-eso-tenant.sh <namespace>}"

# Fixed names — the eso-tenant OpenBao role binds this SA name, and a standalone
# Keystone/Horizon CR selects this SecretStore name via spec.secretStoreRef. They
# MUST match the constants the c5c3 operator uses (esoTenantStoreName /
# esoTenantServiceAccountName / esoTenantClientCertName in reconcile_esotenant.go)
# so the manual standalone route and the operator-provisioned ControlPlane route
# converge on the same objects.
SA_NAME="eso-tenant-auth"
CERT_NAME="eso-tenant-client-tls"
STORE_NAME="openbao-tenant-store"

# The OpenBao server the tenant store connects to, and the ClusterIssuer that
# signs the mTLS client certificate. These mirror the shared cluster store
# (deploy/eso/clustersecretstore.yaml) and the DB-credential Certificate
# (reconcile_dbcredentials.go); override via the environment for a mirror.
OPENBAO_SERVER="${OPENBAO_SERVER:-https://openbao.openbao-system.svc:8200}"
OPENBAO_CA_ISSUER="${OPENBAO_CA_ISSUER:-openbao-ca-issuer}"
CERT_DURATION="${CERT_DURATION:-8760h}"
CERT_RENEW_BEFORE="${CERT_RENEW_BEFORE:-720h}"

###############################################################################
# Main
###############################################################################
main() {
  echo "=== Provisioning per-tenant OpenBao identity for namespace '${TENANT_NS}' ==="
  echo "ServiceAccount : ${SA_NAME}"
  echo "Certificate    : ${CERT_NAME}"
  echo "SecretStore    : ${STORE_NAME}"

  # Fail loudly if the tenant namespace does not exist (or the cluster is
  # unreachable). Applying into a missing namespace would fail later with a less
  # actionable error; the up-front check mirrors setup-database-tenant.sh's
  # ControlPlane existence guard.
  if ! kubectl get namespace "${TENANT_NS}" >/dev/null 2>&1; then
    echo "ERROR: namespace '${TENANT_NS}' not found (or the cluster is unreachable)."
    exit 1
  fi

  # ServiceAccount the tenant SecretStore authenticates as.
  echo "Applying ServiceAccount ${SA_NAME}..."
  kubectl apply -f - <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${SA_NAME}
  namespace: ${TENANT_NS}
EOF

  # mTLS client Certificate. commonName is scoped to the tenant namespace so two
  # tenants never share a certificate identity (mirrors the DB-credential cert).
  echo "Applying Certificate ${CERT_NAME}..."
  kubectl apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: ${CERT_NAME}
  namespace: ${TENANT_NS}
spec:
  secretName: ${CERT_NAME}
  duration: ${CERT_DURATION}
  renewBefore: ${CERT_RENEW_BEFORE}
  commonName: ${CERT_NAME}.${TENANT_NS}.svc
  usages:
    - client auth
  issuerRef:
    name: ${OPENBAO_CA_ISSUER}
    kind: ClusterIssuer
EOF

  # Wait for the Certificate to be issued before applying the SecretStore: the
  # store's tls/caProvider refs point at the cert Secret, so applying it before
  # the Secret exists would leave the store NotReady until the next reconcile.
  echo "Waiting for Certificate ${CERT_NAME} to be Ready..."
  kubectl wait --for=condition=Ready "certificate/${CERT_NAME}" \
    -n "${TENANT_NS}" --timeout=120s

  # Namespaced SecretStore. All Secret refs are same-namespace (a namespaced
  # SecretStore resolves them locally): the client cert/key and CA all come from
  # the cert Secret above. auth.kubernetes uses the eso-tenant role with the
  # eso-tenant-auth SA, so the resolved token is confined to this namespace by
  # the eso-tenant templated policy.
  echo "Applying SecretStore ${STORE_NAME}..."
  kubectl apply -f - <<EOF
apiVersion: external-secrets.io/v1
kind: SecretStore
metadata:
  name: ${STORE_NAME}
  namespace: ${TENANT_NS}
spec:
  provider:
    vault:
      server: "${OPENBAO_SERVER}"
      path: "kv-v2"
      version: "v2"
      tls:
        certSecretRef:
          name: "${CERT_NAME}"
        keySecretRef:
          name: "${CERT_NAME}"
      auth:
        kubernetes:
          mountPath: "kubernetes/management"
          role: "eso-tenant"
          serviceAccountRef:
            name: "${SA_NAME}"
      caProvider:
        type: "Secret"
        name: "${CERT_NAME}"
        key: "ca.crt"
EOF

  echo "=== Done. Set the STANDALONE Keystone/Horizon CR's spec.secretStoreRef to"
  echo "    {kind: SecretStore, name: ${STORE_NAME}} to route it through the"
  echo "    per-tenant identity. A ControlPlane needs no such flip — the c5c3"
  echo "    operator provisions this store and defaults onto it. ==="
}

main "$@"
