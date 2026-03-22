#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/deploy-infra.sh — Deploy full infrastructure stack to a kind cluster.
# Feature: CC-0010
#
# Implements the 8-step deployment sequence:
#   1. Create kind cluster (using hack/kind-config.yaml)
#   2. Install FluxCD
#   3. Apply base kustomize overlay (namespaces, HelmRepositories, HelmReleases)
#   4. Wait for HelmReleases to become Ready (cert-manager first, then TLS
#      prerequisites for OpenBao, then remaining releases)
#   5. Apply infrastructure kustomize overlay (CRD-dependent resources)
#   6. Wait for OpenBao pods to become Ready
#   7. Bootstrap OpenBao (init, unseal, configure)
#   8. Wait for ExternalSecrets to sync
#
# REQ-001: Deploys full infrastructure stack to kind cluster.
# REQ-004: Applies manifests in two phases with health waits between.
# REQ-005: Invokes existing OpenBao bootstrap scripts from deploy/openbao/bootstrap/.
# REQ-011: set -euo pipefail, SPDX Apache-2.0 header, CC-0010 reference.
# REQ-012: Configurable timeouts via environment variables.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ---------------------------------------------------------------------------
# Configuration (REQ-012)
# ---------------------------------------------------------------------------
CLUSTER_NAME="${CLUSTER_NAME:-forge-e2e}"
HELMRELEASE_TIMEOUT="${HELMRELEASE_TIMEOUT:-600}"
POD_TIMEOUT="${POD_TIMEOUT:-300}"
EXTERNALSECRET_TIMEOUT="${EXTERNALSECRET_TIMEOUT:-120}"

# OpenBao init parameters (match deploy/openbao/bootstrap/init-unseal.sh)
KEY_SHARES=5
KEY_THRESHOLD=3
# OPENBAO_NAMESPACE is this script's internal variable for the OpenBao namespace.
# The bootstrap scripts (deploy/openbao/bootstrap/) read NAMESPACE from common.sh,
# which defaults to the same value ('openbao-system').  When the NAMESPACE env var
# is set, both this script and the bootstrap scripts receive it correctly because
# child processes inherit the environment.  Do NOT set OPENBAO_NAMESPACE directly —
# set NAMESPACE instead so that both layers stay in sync (CC-0010).
OPENBAO_NAMESPACE="${NAMESPACE:-openbao-system}"
BAO_ADDR="${BAO_ADDR:-https://127.0.0.1:8200}"
VAULT_CACERT="${VAULT_CACERT:-/openbao/tls/ca.crt}"
SECRET_NAME="openbao-init-keys"

# ---------------------------------------------------------------------------
# log — Print a timestamped log message (ISO 8601 UTC).
# Matches the pattern from deploy/openbao/bootstrap/common.sh.
# ---------------------------------------------------------------------------
log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

# ---------------------------------------------------------------------------
# wait_for_helmreleases — Wait until all HelmReleases show Ready=True.
#
# Polls every 10 seconds up to HELMRELEASE_TIMEOUT. Checks that every
# HelmRelease across all namespaces has condition Ready with status True.
# ---------------------------------------------------------------------------
wait_for_helmreleases() {
  local timeout="$1"
  shift
  local releases=("$@")
  local deadline=$(( $(date +%s) + timeout ))

  log "Waiting up to ${timeout}s for HelmReleases to become Ready: ${releases[*]}"

  while true; do
    local all_ready=true

    for release in "${releases[@]}"; do
      # Find the namespace for this HelmRelease
      local ns
      ns=$(kubectl get helmrelease --all-namespaces -o json 2>/dev/null \
        | jq -r --arg name "${release}" '.items[] | select(.metadata.name == $name) | .metadata.namespace' 2>/dev/null) || true

      if [[ -z "${ns}" ]]; then
        log "  HelmRelease '${release}' not found yet."
        all_ready=false
        continue
      fi

      local ready_status
      ready_status=$(kubectl get helmrelease "${release}" -n "${ns}" -o json 2>/dev/null \
        | jq -r '.status.conditions[]? | select(.type == "Ready") | .status' 2>/dev/null) || true

      if [[ "${ready_status}" != "True" ]]; then
        local reason message
        reason=$(kubectl get helmrelease "${release}" -n "${ns}" -o json 2>/dev/null \
          | jq -r '.status.conditions[]? | select(.type == "Ready") | .reason // "Pending"' 2>/dev/null) || true
        message=$(kubectl get helmrelease "${release}" -n "${ns}" -o json 2>/dev/null \
          | jq -r '.status.conditions[]? | select(.type == "Ready") | .message // ""' 2>/dev/null) || true
        log "  HelmRelease '${release}' in namespace '${ns}' is not Ready yet (reason: ${reason:-Pending})."
        if [[ -n "${message}" ]]; then
          log "    ${message}"
        fi
        all_ready=false
      fi
    done

    if [[ "${all_ready}" == "true" ]]; then
      log "All HelmReleases are Ready."
      return 0
    fi

    if [[ $(date +%s) -ge ${deadline} ]]; then
      log "ERROR: Timed out waiting for HelmReleases after ${timeout}s."
      log "HelmRelease status:"
      kubectl get helmrelease --all-namespaces 2>/dev/null || true
      exit 1
    fi

    sleep 10
  done
}

# ---------------------------------------------------------------------------
# wait_for_pods — Wait for pods matching a label selector to be Ready.
#
# Arguments:
#   $1 — namespace
#   $2 — label selector (e.g., app.kubernetes.io/name=openbao)
#   $3 — timeout in seconds
# ---------------------------------------------------------------------------
wait_for_pods() {
  local namespace="$1"
  local selector="$2"
  local timeout="$3"
  local deadline=$(( $(date +%s) + timeout ))

  log "Waiting up to ${timeout}s for pods with selector '${selector}' in namespace '${namespace}' to be Ready..."

  while true; do
    local total
    total=$(kubectl get pods -n "${namespace}" -l "${selector}" --no-headers 2>/dev/null | wc -l | tr -d ' ') || true

    if [[ "${total}" -gt 0 ]]; then
      local ready
      ready=$(kubectl get pods -n "${namespace}" -l "${selector}" -o json 2>/dev/null \
        | jq '[.items[] | select(.status.conditions[]? | select(.type == "Ready" and .status == "True"))] | length' 2>/dev/null) || true

      if [[ "${ready}" -eq "${total}" ]]; then
        log "All ${total} pod(s) with selector '${selector}' in '${namespace}' are Ready."
        return 0
      fi

      log "  ${ready:-0}/${total} pod(s) Ready for selector '${selector}' in '${namespace}'."
    else
      log "  No pods found yet for selector '${selector}' in '${namespace}'."
    fi

    if [[ $(date +%s) -ge ${deadline} ]]; then
      log "ERROR: Timed out waiting for pods after ${timeout}s."
      kubectl get pods -n "${namespace}" -l "${selector}" 2>/dev/null || true
      exit 1
    fi

    sleep 10
  done
}

# ---------------------------------------------------------------------------
# wait_for_pods_running — Wait for pods to reach Running phase.
#
# Unlike wait_for_pods (which waits for Ready), this only requires pods to be
# in the Running phase. Useful for pods like OpenBao that only become Ready
# after an external init/unseal step.
#
# Arguments:
#   $1 — namespace
#   $2 — label selector (e.g., app.kubernetes.io/name=openbao)
#   $3 — timeout in seconds
# ---------------------------------------------------------------------------
wait_for_pods_running() {
  local namespace="$1"
  local selector="$2"
  local timeout="$3"
  local deadline=$(( $(date +%s) + timeout ))

  log "Waiting up to ${timeout}s for pods with selector '${selector}' in namespace '${namespace}' to be Running..."

  while true; do
    local total
    total=$(kubectl get pods -n "${namespace}" -l "${selector}" --no-headers 2>/dev/null | wc -l | tr -d ' ') || true

    if [[ "${total}" -gt 0 ]]; then
      local running
      running=$(kubectl get pods -n "${namespace}" -l "${selector}" -o json 2>/dev/null \
        | jq '[.items[] | select(.status.phase == "Running")] | length' 2>/dev/null) || true

      if [[ "${running}" -eq "${total}" ]]; then
        log "All ${total} pod(s) with selector '${selector}' in '${namespace}' are Running."
        return 0
      fi

      log "  ${running:-0}/${total} pod(s) Running for selector '${selector}' in '${namespace}'."
    else
      log "  No pods found yet for selector '${selector}' in '${namespace}'."
    fi

    if [[ $(date +%s) -ge ${deadline} ]]; then
      log "ERROR: Timed out waiting for pods to reach Running phase after ${timeout}s."
      kubectl get pods -n "${namespace}" -l "${selector}" 2>/dev/null || true
      exit 1
    fi

    sleep 10
  done
}

# ---------------------------------------------------------------------------
# wait_for_externalsecrets — Wait for ExternalSecrets to reach SecretSynced.
#
# Arguments:
#   $1 — namespace
#   $2 — timeout in seconds
#   $3..N — ExternalSecret names
# ---------------------------------------------------------------------------
wait_for_externalsecrets() {
  local namespace="$1"
  local timeout="$2"
  shift 2
  local secrets=("$@")
  local deadline=$(( $(date +%s) + timeout ))

  log "Waiting up to ${timeout}s for ExternalSecrets to sync in namespace '${namespace}': ${secrets[*]}"

  while true; do
    local all_synced=true

    for secret in "${secrets[@]}"; do
      local synced_status
      synced_status=$(kubectl get externalsecret "${secret}" -n "${namespace}" -o json 2>/dev/null \
        | jq -r '.status.conditions[]? | select(.type == "Ready") | .status' 2>/dev/null) || true

      if [[ "${synced_status}" != "True" ]]; then
        local reason
        reason=$(kubectl get externalsecret "${secret}" -n "${namespace}" -o json 2>/dev/null \
          | jq -r '.status.conditions[]? | select(.type == "Ready") | .reason // "Unknown"' 2>/dev/null) || true
        log "  ExternalSecret '${secret}' not synced yet (reason: ${reason:-Pending})."
        all_synced=false
      fi
    done

    if [[ "${all_synced}" == "true" ]]; then
      log "All ExternalSecrets are synced."
      return 0
    fi

    if [[ $(date +%s) -ge ${deadline} ]]; then
      log "ERROR: Timed out waiting for ExternalSecrets after ${timeout}s."
      kubectl get externalsecret -n "${namespace}" 2>/dev/null || true
      exit 1
    fi

    sleep 10
  done
}

# ---------------------------------------------------------------------------
# wait_for_crds — Wait until specific CRDs are registered in the API server.
#
# Arguments:
#   $1 — timeout in seconds
#   $2..N — CRD names (e.g., memcacheds.memcached.c5c3.io)
# ---------------------------------------------------------------------------
wait_for_crds() {
  local timeout="$1"
  shift
  local crds=("$@")
  local deadline=$(( $(date +%s) + timeout ))

  log "Waiting up to ${timeout}s for CRDs to be registered: ${crds[*]}"

  while true; do
    local all_found=true

    for crd in "${crds[@]}"; do
      if ! kubectl get crd "${crd}" &>/dev/null; then
        log "  CRD '${crd}' not registered yet."
        all_found=false
      fi
    done

    if [[ "${all_found}" == "true" ]]; then
      log "All required CRDs are registered."
      return 0
    fi

    if [[ $(date +%s) -ge ${deadline} ]]; then
      log "ERROR: Timed out waiting for CRDs after ${timeout}s."
      kubectl get crd 2>/dev/null | grep -E "$(IFS='|'; echo "${crds[*]}")" || true
      exit 1
    fi

    sleep 5
  done
}

# ---------------------------------------------------------------------------
# preflight_checks — Verify prerequisites before deployment.
# ---------------------------------------------------------------------------
preflight_checks() {
  log "Running pre-flight checks..."

  # Check that required CLI tools are available.
  for cmd in docker kind kubectl flux jq; do
    if ! command -v "${cmd}" &>/dev/null; then
      log "ERROR: '${cmd}' is not installed or not in PATH."
      exit 1
    fi
  done

  # Check that Docker is running.
  if ! docker info &>/dev/null; then
    log "ERROR: Docker is not running. Please start Docker and try again."
    exit 1
  fi

  # Check that no existing kind cluster with the same name exists.
  # Skipped when SKIP_KIND_CREATE=true (CI mode where helm/kind-action creates the cluster).
  if [[ "${SKIP_KIND_CREATE:-false}" != "true" ]]; then
    if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
      log "ERROR: Kind cluster '${CLUSTER_NAME}' already exists."
      log "Run 'make teardown-infra' to delete it first, then retry."
      exit 1
    fi
  fi

  log "Pre-flight checks passed."
}

# ---------------------------------------------------------------------------
# openbao_kube_exec — Execute a command inside the openbao-0 pod.
# Does NOT pass BAO_TOKEN (used for init/unseal before the token exists).
# ---------------------------------------------------------------------------
openbao_kube_exec() {
  kubectl exec -n "${OPENBAO_NAMESPACE}" openbao-0 -- \
    env BAO_ADDR="${BAO_ADDR}" VAULT_CACERT="${VAULT_CACERT}" "$@"
}

# ---------------------------------------------------------------------------
# openbao_init_unseal — Initialize and unseal openbao-0 (single replica).
#
# The production init-unseal.sh hardcodes 3 replicas (HA mode) but the kind
# cluster runs only 1 replica. Rather than modifying the production script,
# we perform initialization and unsealing inline for just openbao-0.
# ---------------------------------------------------------------------------
openbao_init_unseal() {
  log "--- OpenBao Init & Unseal (single-replica mode) ---"

  # Wait for the OpenBao server to become reachable inside the pod.
  # The pod may be Running but the bao server needs a few seconds to start
  # listening on its port.
  local status_json=""
  local retries=30
  for i in $(seq 1 "${retries}"); do
    status_json=$(openbao_kube_exec bao status -format=json 2>/dev/null) || true
    if [[ -n "${status_json}" ]]; then
      break
    fi
    log "  Waiting for OpenBao server to become reachable (attempt ${i}/${retries})..."
    sleep 5
  done

  if [[ -z "${status_json}" ]]; then
    log "ERROR: Could not reach openbao-0 after ${retries} attempts."
    exit 1
  fi

  local initialized
  initialized=$(echo "${status_json}" | jq -r '.initialized') || true

  if [[ "${initialized}" == "true" ]]; then
    log "OpenBao is already initialized. Skipping initialization."
  else
    log "Initializing OpenBao (key-shares=${KEY_SHARES}, key-threshold=${KEY_THRESHOLD})..."

    local init_output
    init_output=$(openbao_kube_exec \
      bao operator init \
        -key-shares="${KEY_SHARES}" \
        -key-threshold="${KEY_THRESHOLD}" \
        -format=json)

    log "Initialization successful. Storing init output in Secret ${OPENBAO_NAMESPACE}/${SECRET_NAME}..."

    local encoded
    encoded=$(echo -n "${init_output}" | base64 | tr -d '\n')
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Secret
metadata:
  name: ${SECRET_NAME}
  namespace: ${OPENBAO_NAMESPACE}
type: Opaque
data:
  init-output: ${encoded}
EOF

    log "Secret ${OPENBAO_NAMESPACE}/${SECRET_NAME} created."
  fi

  # Check seal status and unseal if needed.
  local rc=0
  status_json=$(openbao_kube_exec bao status -format=json 2>/dev/null) || rc=$?

  if [[ "${rc}" -eq 0 ]]; then
    log "openbao-0 is already unsealed. Skipping unseal."
    return 0
  fi

  log "Unsealing openbao-0..."

  local init_output
  init_output=$(kubectl get secret "${SECRET_NAME}" \
    -n "${OPENBAO_NAMESPACE}" \
    -o jsonpath='{.data.init-output}' | base64 -d)

  local i
  for i in $(seq 0 $(( KEY_THRESHOLD - 1 ))); do
    local key
    key=$(echo "${init_output}" | jq -r ".unseal_keys_b64[${i}]")
    openbao_kube_exec bao operator unseal "${key}" > /dev/null
    log "  Applied unseal key $((i + 1))/${KEY_THRESHOLD} to openbao-0."
  done

  log "openbao-0 unsealed successfully."
}

# ---------------------------------------------------------------------------
# openbao_bootstrap — Run the 4 remaining bootstrap scripts against openbao-0.
#
# These scripts all operate against openbao-0 only (via common.sh's bao_exec),
# so they work correctly in single-replica mode.
# ---------------------------------------------------------------------------
openbao_bootstrap() {
  log "--- OpenBao Bootstrap ---"

  # Extract root token from the init-keys Secret.
  export BAO_TOKEN
  # Ensure the root token is scrubbed from the environment on any exit path
  # (success, set -e failure, or signal), not only on success (CC-0010, W-007).
  trap 'unset BAO_TOKEN' EXIT
  BAO_TOKEN=$(kubectl get secret "${SECRET_NAME}" -n "${OPENBAO_NAMESPACE}" \
    -o jsonpath='{.data.init-output}' | base64 -d | jq -r '.root_token')

  if [[ -z "${BAO_TOKEN}" || "${BAO_TOKEN}" == "null" ]]; then
    log "ERROR: Could not extract root token from ${OPENBAO_NAMESPACE}/${SECRET_NAME}."
    exit 1
  fi

  log "Root token extracted. Running bootstrap scripts..."

  local bootstrap_dir="${REPO_ROOT}/deploy/openbao/bootstrap"
  local scripts=(
    setup-secret-engines.sh
    setup-auth.sh
    setup-policies.sh
    write-bootstrap-secrets.sh
  )

  for script in "${scripts[@]}"; do
    local script_path="${bootstrap_dir}/${script}"
    if [[ ! -x "${script_path}" ]]; then
      log "ERROR: Bootstrap script not found or not executable: ${script_path}"
      exit 1
    fi
    log "Running ${script}..."
    bash "${script_path}"
    log "${script} completed."
  done

  unset BAO_TOKEN
  log "All bootstrap scripts completed."
}

# ---------------------------------------------------------------------------
# main — Orchestrate the 8-step deployment sequence.
# ---------------------------------------------------------------------------
main() {
  log "=========================================="
  log "  Deploy Infrastructure to Kind Cluster"
  log "=========================================="
  log "Cluster name        : ${CLUSTER_NAME}"
  log "HelmRelease timeout : ${HELMRELEASE_TIMEOUT}s"
  log "Pod timeout         : ${POD_TIMEOUT}s"
  log "ExternalSecret timeout : ${EXTERNALSECRET_TIMEOUT}s"
  log ""

  # Pre-flight checks
  preflight_checks

  # Step 1: Create kind cluster
  log "=== Step 1/8: Create kind cluster ==="
  if [[ "${SKIP_KIND_CREATE:-false}" == "true" ]]; then
    log "SKIP_KIND_CREATE=true — assuming kind cluster '${CLUSTER_NAME}' already exists (CI mode)."
  else
    kind create cluster \
      --name "${CLUSTER_NAME}" \
      --config "${SCRIPT_DIR}/kind-config.yaml" \
      --wait 60s
    log "Kind cluster '${CLUSTER_NAME}' created."
  fi

  # Step 2: Install FluxCD
  log "=== Step 2/8: Install FluxCD ==="
  flux install
  log "FluxCD installed."

  # Step 3: Apply base kustomize overlay (namespaces, HelmRepos, HelmReleases)
  log "=== Step 3/8: Apply base kustomize overlay ==="
  kubectl apply -k "${REPO_ROOT}/deploy/kind/base"
  log "Base kustomize overlay applied."

  # Force-reconcile HelmRepository sources so chart indexes are available
  # before HelmReleases attempt to resolve charts. Without this, the
  # helm-controller may see unindexed sources and wait until the next
  # reconcile interval (up to 1h) before retrying.
  log "Reconciling HelmRepository sources..."
  local repos
  repos=$(kubectl get helmrepository -n flux-system -o jsonpath='{.items[*].metadata.name}' 2>/dev/null) || true
  for repo in ${repos}; do
    flux reconcile source helm "${repo}" -n flux-system --timeout=60s || true
  done

  # Step 4: Wait for HelmReleases to become Ready (two phases)
  log "=== Step 4/8: Wait for HelmReleases ==="

  # Phase 1: cert-manager must be Ready before we can create TLS resources.
  log "Phase 1: Waiting for cert-manager..."
  wait_for_helmreleases "${HELMRELEASE_TIMEOUT}" cert-manager

  # Phase 2: Apply TLS prerequisites that OpenBao needs to start.
  # The openbao-tls Certificate creates the Secret mounted by the OpenBao
  # StatefulSet. These are also part of the infrastructure kustomization
  # (applied in Step 5), but OpenBao cannot start without them.
  log "Phase 2: Applying TLS prerequisites (ClusterIssuer + OpenBao TLS Certificate)..."
  kubectl apply -f "${REPO_ROOT}/deploy/flux-system/infrastructure/cluster-issuer.yaml"
  kubectl apply -f "${REPO_ROOT}/deploy/flux-system/infrastructure/openbao-tls-cert.yaml"

  # Phase 3: Wait for remaining HelmReleases now that OpenBao can mount its TLS secret.
  log "Phase 3: Waiting for remaining HelmReleases..."
  wait_for_helmreleases "${HELMRELEASE_TIMEOUT}" \
    prometheus-operator-crds openbao mariadb-operator-crds mariadb-operator external-secrets memcached-operator

  # Step 5: Apply infrastructure kustomize overlay (CRD-dependent resources)
  log "=== Step 5/8: Apply infrastructure kustomize overlay ==="

  # Wait for operator CRDs to be registered before applying CRD-dependent
  # resources. HelmRelease Ready does not guarantee CRDs are available in
  # the API server — the operator pods may still be starting.
  wait_for_crds "${POD_TIMEOUT}" \
    memcacheds.memcached.c5c3.io \
    clustersecretstores.external-secrets.io \
    externalsecrets.external-secrets.io \
    mariadbs.k8s.mariadb.com

  # Invalidate kubectl's client-side discovery cache so that the newly
  # registered CRDs are visible to kubectl apply.
  kubectl api-resources > /dev/null 2>&1 || true
  kubectl apply -k "${REPO_ROOT}/deploy/kind/infrastructure"
  log "Infrastructure kustomize overlay applied."

  # Step 6: Wait for OpenBao pod to be Running (not Ready — it becomes Ready
  # only after init+unseal in Step 7).
  log "=== Step 6/8: Wait for OpenBao pods ==="
  wait_for_pods_running "${OPENBAO_NAMESPACE}" "app.kubernetes.io/name=openbao" "${POD_TIMEOUT}"

  # Step 7: OpenBao bootstrap (init, unseal, configure)
  log "=== Step 7/8: OpenBao bootstrap ==="
  openbao_init_unseal
  openbao_bootstrap

  # Wait for OpenBao to become Ready after unseal
  wait_for_pods "${OPENBAO_NAMESPACE}" "app.kubernetes.io/name=openbao" "${POD_TIMEOUT}"

  # Step 8: Wait for ExternalSecrets to sync
  log "=== Step 8/8: Wait for ExternalSecrets ==="
  wait_for_externalsecrets "openstack" "${EXTERNALSECRET_TIMEOUT}" \
    keystone-admin keystone-db mariadb-root-password

  # Trigger MariaDB operator re-reconciliation.
  # The MariaDB CR was applied in Step 5 before the root password Secret
  # existed (it is created by ExternalSecrets in this step). The operator
  # may have stopped retrying; patching an annotation forces a new
  # reconciliation now that the Secret is available.
  log "Triggering MariaDB CR re-reconciliation..."
  kubectl patch mariadb openstack-db -n openstack --type merge \
    -p "{\"metadata\":{\"annotations\":{\"deploy.c5c3.io/reconcile-trigger\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}}}"

  # Wait for MariaDB to become Ready before declaring deployment complete.
  log "Waiting for MariaDB CR to become Ready..."
  kubectl wait mariadb/openstack-db -n openstack \
    --for=condition=Ready --timeout="${POD_TIMEOUT}s"
  log "MariaDB CR is Ready."

  log ""
  log "=========================================="
  log "  Infrastructure deployment complete!"
  log "=========================================="
  log "Cluster: ${CLUSTER_NAME}"
  log "To tear down: make teardown-infra"
}

main "$@"
