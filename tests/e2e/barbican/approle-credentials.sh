#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/barbican/approle-credentials.sh — materialize the two Secrets a
# brownfield (server-mode) BarbicanSecretStore reads: the AppRole credentials
# and the CA bundle that authenticates the OpenBao server.
#
# The proving instance is the OpenBaoCluster openbao-instance in the openstack
# namespace (deploy/kind/infrastructure/openbao-instance.yaml). Its self-init
# provisions the barbican/ KV mount, the barbican AppRole, and a
# Kubernetes-auth role "provisioner" bound to the ServiceAccount
# openbao-instance-provisioner with audience openbao-instance. Self-init revokes
# the root token, so that role is the only entry into the instance — the same
# entry the barbican-operator uses for a managed store.
#
# A brownfield store is validate-only: the operator reads the credentials and
# renders them, and never provisions anything. This helper is what stands in for
# the out-of-band provisioning such a store assumes, so the e2e suites can
# exercise the server mode against an instance the cluster already runs.
#
# Unlike tests/e2e/infrastructure/openbao-instance/chainsaw-test.yaml, which
# drives the same flow with `kubectl exec` and the in-pod bao CLI, this helper
# talks to the API over a port-forward with curl. That is deliberate: it is the
# only way the CA Secret path is exercised, because a client outside the pod has
# to verify the server certificate against the bundle the store references.
#
# Usage: approle-credentials.sh <target-namespace>
#
# The target namespace is a required positional argument. Chainsaw injects
# NAMESPACE into every script step's environment (see tests/e2e/README.md), so a
# helper that fell back to $NAMESPACE would silently write into the test's
# namespace whenever a caller forgot the argument — the same trap
# tests/e2e-chaos/unseal-openbao.sh documents for its own namespace variable.

set -euo pipefail

# The instance side is fixed by the overlay: the CR, its CA Secret, and the
# provisioner ServiceAccount all live in openstack under conventional names.
INSTANCE_NAMESPACE="openstack"
INSTANCE="openbao-instance"
INSTANCE_CA_SECRET="${INSTANCE}-tls-ca"
PROVISIONER_SA="${INSTANCE}-provisioner"
# The audience the Kubernetes-auth role binds to. A default-audience token is
# rejected, which is the point of the bound audience.
TOKEN_AUDIENCE="${INSTANCE}"
# The AppRole barbican's vault plugin authenticates with.
APPROLE="barbican"

# The Secrets this helper writes. Both key contracts are fixed by
# operators/barbican/api/v1alpha1/barbicansecretstore_types.go
# (OpenBaoRoleIDKey, OpenBaoSecretIDKey, OpenBaoCAKey), so a store references
# them by name alone.
CREDENTIALS_SECRET="barbican-brownfield-approle"
CA_BUNDLE_SECRET="openbao-instance-ca"

# The server certificate carries the openbao-instance.openstack.svc SAN and no
# loopback IP SAN, so the request is addressed by that name and pinned to the
# forwarded loopback port with --resolve. Overridable so two concurrent runs on
# one host cannot collide on the local port.
SERVER_NAME="${INSTANCE}.${INSTANCE_NAMESPACE}.svc"
LOCAL_PORT="${APPROLE_LOCAL_PORT:-18200}"

TARGET_NAMESPACE="${1:-}"
if [ -z "${TARGET_NAMESPACE}" ]; then
  echo "usage: approle-credentials.sh <target-namespace>" >&2
  exit 2
fi

WORKDIR="$(mktemp -d)"
CA_FILE="${WORKDIR}/ca.crt"
PORT_FORWARD_PID=""
TOKEN=""

# The trap owns both the forwarded port and the CA bundle on disk, so an exit
# anywhere below leaves no background kubectl and no key material in /tmp.
cleanup() {
  if [ -n "${PORT_FORWARD_PID}" ]; then
    kill "${PORT_FORWARD_PID}" 2>/dev/null || true
    wait "${PORT_FORWARD_PID}" 2>/dev/null || true
  fi
  rm -rf "${WORKDIR}"
}
trap cleanup EXIT

# bao_call <method> <path> <body> <output-file> writes the response body to
# output-file and prints the HTTP status code. The token header is sent only once
# a token exists, so the login call travels unauthenticated.
bao_call() {
  local method="$1" path="$2" body="$3" out="$4"
  local -a args=(
    --silent --show-error
    --cacert "${CA_FILE}"
    --resolve "${SERVER_NAME}:${LOCAL_PORT}:127.0.0.1"
    --max-time 30
    --request "${method}"
    --output "${out}"
    --write-out '%{http_code}'
  )
  if [ -n "${TOKEN}" ]; then
    args+=(--header "X-Vault-Token: ${TOKEN}")
  fi
  if [ -n "${body}" ]; then
    args+=(--data "${body}")
  fi
  curl "${args[@]}" "https://${SERVER_NAME}:${LOCAL_PORT}${path}"
}

# call_or_die is bao_call plus the non-2xx exit. The response body is printed
# because OpenBao reports the actual cause there (a denied path, a rejected
# audience) while the status code alone says only "403".
call_or_die() {
  local method="$1" path="$2" body="$3" out="$4" what="$5"
  local code
  code="$(bao_call "${method}" "${path}" "${body}" "${out}")"
  case "${code}" in
  2??) return 0 ;;
  esac
  echo "ERROR: ${what} failed with HTTP ${code}" >&2
  cat "${out}" >&2
  echo >&2
  exit 1
}

# json_field <file> <key> [<key>...] prints the string at the given key path, or
# the empty string when any step of the path is missing or not a string.
json_field() {
  local file="$1"
  shift
  python3 - "${file}" "$@" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    doc = json.load(handle)
for key in sys.argv[2:]:
    if not isinstance(doc, dict):
        doc = None
        break
    doc = doc.get(key)
print(doc if isinstance(doc, str) else "")
PY
}

# ── 1. The CA bundle that authenticates the instance ─────────────────────────
if ! kubectl get secret "${INSTANCE_CA_SECRET}" -n "${INSTANCE_NAMESPACE}" >/dev/null 2>&1; then
  echo "ERROR: Secret ${INSTANCE_NAMESPACE}/${INSTANCE_CA_SECRET} not found; the proving OpenBao instance is not deployed" >&2
  exit 1
fi
kubectl get secret "${INSTANCE_CA_SECRET}" -n "${INSTANCE_NAMESPACE}" \
  -o 'jsonpath={.data.ca\.crt}' | base64 -d >"${CA_FILE}"
if [ ! -s "${CA_FILE}" ]; then
  echo "ERROR: Secret ${INSTANCE_NAMESPACE}/${INSTANCE_CA_SECRET} carries no ca.crt data" >&2
  exit 1
fi
echo "OK: read the trust bundle from ${INSTANCE_NAMESPACE}/${INSTANCE_CA_SECRET}"

# ── 2. A ServiceAccount token for the audience-bound provisioner role ────────
JWT="$(kubectl create token "${PROVISIONER_SA}" -n "${INSTANCE_NAMESPACE}" \
  --audience="${TOKEN_AUDIENCE}")"
if [ -z "${JWT}" ]; then
  echo "ERROR: minting a token for ServiceAccount ${INSTANCE_NAMESPACE}/${PROVISIONER_SA} returned nothing" >&2
  exit 1
fi

# ── 3. Forward the API port and wait for it to answer ───────────────────────
kubectl port-forward -n "${INSTANCE_NAMESPACE}" "svc/${INSTANCE}" \
  "${LOCAL_PORT}:8200" >"${WORKDIR}/port-forward.log" 2>&1 &
PORT_FORWARD_PID=$!

# /v1/sys/health answers unauthenticated, so a status code of any kind means the
# forward is up. 30 attempts one second apart cover the port-forward handshake.
forwarded=""
for _ in $(seq 1 30); do
  if curl --silent --cacert "${CA_FILE}" \
    --resolve "${SERVER_NAME}:${LOCAL_PORT}:127.0.0.1" \
    --max-time 5 --output /dev/null \
    "https://${SERVER_NAME}:${LOCAL_PORT}/v1/sys/health"; then
    forwarded="yes"
    break
  fi
  sleep 1
done
if [ -z "${forwarded}" ]; then
  echo "ERROR: https://${SERVER_NAME}:${LOCAL_PORT} did not answer through the port-forward" >&2
  cat "${WORKDIR}/port-forward.log" >&2
  exit 1
fi
echo "OK: forwarded ${INSTANCE_NAMESPACE}/${INSTANCE}:8200 to 127.0.0.1:${LOCAL_PORT}"

# ── 4. Kubernetes auth ──────────────────────────────────────────────────────
call_or_die POST /v1/auth/kubernetes/login \
  "$(printf '{"role":"provisioner","jwt":"%s"}' "${JWT}")" \
  "${WORKDIR}/login.json" "the Kubernetes-auth login as role provisioner"
TOKEN="$(json_field "${WORKDIR}/login.json" auth client_token)"
if [ -z "${TOKEN}" ]; then
  echo "ERROR: the Kubernetes-auth login returned no client token" >&2
  exit 1
fi
echo "OK: logged in through auth/kubernetes as role provisioner"

# ── 5. Read the role ID and mint a secret ID ────────────────────────────────
call_or_die GET "/v1/auth/approle/role/${APPROLE}/role-id" "" \
  "${WORKDIR}/role-id.json" "reading the ${APPROLE} AppRole role ID"
ROLE_ID="$(json_field "${WORKDIR}/role-id.json" data role_id)"
if [ -z "${ROLE_ID}" ]; then
  echo "ERROR: auth/approle/role/${APPROLE}/role-id returned an empty role-id" >&2
  exit 1
fi

# The mint takes no payload; the empty body keeps curl from sending one.
call_or_die POST "/v1/auth/approle/role/${APPROLE}/secret-id" "" \
  "${WORKDIR}/secret-id.json" "minting a ${APPROLE} AppRole secret ID"
SECRET_ID="$(json_field "${WORKDIR}/secret-id.json" data secret_id)"
if [ -z "${SECRET_ID}" ]; then
  echo "ERROR: auth/approle/role/${APPROLE}/secret-id returned an empty secret-id" >&2
  exit 1
fi
echo "OK: read the ${APPROLE} AppRole role ID and minted a secret ID"

# ── 6. Write the two Secrets the store references ───────────────────────────
# Idempotent create-or-apply so a re-run against the same namespace refreshes
# the credentials instead of failing on the existing objects. Overwriting the
# credentials is safe: an AppRole holds many secret IDs at once, so a secret ID
# an earlier run handed to another store keeps working.
kubectl create secret generic "${CREDENTIALS_SECRET}" \
  --namespace "${TARGET_NAMESPACE}" \
  --from-literal=role-id="${ROLE_ID}" \
  --from-literal=secret-id="${SECRET_ID}" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl create secret generic "${CA_BUNDLE_SECRET}" \
  --namespace "${TARGET_NAMESPACE}" \
  --from-file=ca.crt="${CA_FILE}" \
  --dry-run=client -o yaml | kubectl apply -f -

echo "OK: wrote ${TARGET_NAMESPACE}/${CREDENTIALS_SECRET} and ${TARGET_NAMESPACE}/${CA_BUNDLE_SECRET}"

# ── 7. Give the provisioner token back ──────────────────────────────────────
# Best-effort: the token expires on its own an hour later, so a failed revoke
# must not fail a run whose Secrets are already written.
bao_call POST /v1/auth/token/revoke-self "" "${WORKDIR}/revoke.json" >/dev/null || true
