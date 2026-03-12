#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# deploy-infra.sh — Orchestrates local infrastructure deployment on a kind cluster.
# Feature: CC-0010

set -euo pipefail

# ---------------------------------------------------------------------------
# Path resolution
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ---------------------------------------------------------------------------
# Configuration (override via environment variables)
# ---------------------------------------------------------------------------
CLUSTER_NAME="${CLUSTER_NAME:-c5c3}"
HELM_RELEASE_TIMEOUT="${HELM_RELEASE_TIMEOUT:-600s}"
OPENBAO_POD_TIMEOUT="${OPENBAO_POD_TIMEOUT:-300s}"
EXTERNAL_SECRET_TIMEOUT="${EXTERNAL_SECRET_TIMEOUT:-300s}"

# ---------------------------------------------------------------------------
# log — Print a timestamped log message (ISO 8601 UTC).
# ---------------------------------------------------------------------------
log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

# ---------------------------------------------------------------------------
# wait_for_helmreleases — Wait for all HelmReleases in all namespaces to
# become Ready. Polls until every HelmRelease has condition Ready=True or
# the timeout is reached.
# ---------------------------------------------------------------------------
wait_for_helmreleases() {
  local timeout="${HELM_RELEASE_TIMEOUT}"
  # Strip trailing 's' for arithmetic if present.
  local timeout_secs="${timeout%s}"
  local interval=10
  local elapsed=0

  log "Waiting for all HelmReleases to become Ready (timeout: ${timeout}) ..."

  while true; do
    local total not_ready
    total=$(kubectl get helmreleases --all-namespaces --no-headers 2>/dev/null | wc -l)
    if [[ "${total}" -eq 0 ]]; then
      log "No HelmReleases found yet. Waiting ..."
    else
      not_ready=$(kubectl get helmreleases --all-namespaces -o json \
        | jq '[.items[] | select(.status.conditions // [] | map(select(.type == "Ready" and .status == "True")) | length == 0)] | length')
      if [[ "${not_ready}" -eq 0 ]]; then
        log "All ${total} HelmRelease(s) are Ready."
        return 0
      fi
      log "${not_ready}/${total} HelmRelease(s) not yet Ready (${elapsed}s elapsed)."
    fi

    if [[ "${elapsed}" -ge "${timeout_secs}" ]]; then
      log "ERROR: Timed out waiting for HelmReleases after ${timeout_secs}s."
      kubectl get helmreleases --all-namespaces
      return 1
    fi

    sleep "${interval}"
    elapsed=$((elapsed + interval))
  done
}

# ---------------------------------------------------------------------------
# wait_for_pods — Wait for pods matching a label selector in a namespace
# to become Ready.
#   $1 — namespace
#   $2 — label selector (e.g. app.kubernetes.io/name=openbao)
#   $3 — timeout (e.g. 300s)
#
# NOTE: The effective maximum wait can be up to 2x the configured timeout.
# The pod-existence polling loop uses the full timeout to wait for pods to
# appear, and then kubectl wait uses the full timeout again to wait for
# the Ready condition. This is intentional — the CI job timeout (40 min)
# provides the outer bound, and script-level timeouts are designed to fire
# and produce diagnostics before the CI runner is killed (CC-0010).
# ---------------------------------------------------------------------------
wait_for_pods() {
  local namespace="$1"
  local selector="$2"
  local timeout="$3"

  log "Waiting for pods (namespace=${namespace}, selector=${selector}, timeout=${timeout}) ..."

  # Wait until at least one pod exists before handing off to kubectl wait.
  # kubectl wait exits immediately with an error when no pods match the
  # selector, which races with async pod scheduling (review #1, CC-0010).
  local timeout_secs="${timeout%s}"
  local deadline=$(( $(date +%s) + timeout_secs ))
  until kubectl get pods -l "${selector}" -n "${namespace}" --no-headers 2>/dev/null | grep -q .; do
    if [[ $(date +%s) -ge ${deadline} ]]; then
      log "ERROR: Timed out waiting for pods to appear (namespace=${namespace}, selector=${selector})."
      return 1
    fi
    sleep 5
  done

  kubectl wait --for=condition=Ready pod \
    -l "${selector}" \
    -n "${namespace}" \
    --timeout="${timeout}"
  log "Pods matching '${selector}' in namespace '${namespace}' are Ready."
}

# ---------------------------------------------------------------------------
# wait_for_externalsecrets — Wait for ExternalSecrets to reach SecretSynced
# condition in the openstack namespace.
# ---------------------------------------------------------------------------
wait_for_externalsecrets() {
  local timeout="${EXTERNAL_SECRET_TIMEOUT}"
  local timeout_secs="${timeout%s}"
  local interval=10
  local elapsed=0

  log "Waiting for ExternalSecrets to sync (timeout: ${timeout}) ..."

  while true; do
    local total not_synced
    total=$(kubectl get externalsecrets -n openstack --no-headers 2>/dev/null | wc -l)
    if [[ "${total}" -eq 0 ]]; then
      log "No ExternalSecrets found yet in namespace 'openstack'. Waiting ..."
    else
      not_synced=$(kubectl get externalsecrets -n openstack -o json \
        | jq '[.items[] | select(.status.conditions // [] | map(select(.type == "SecretSynced" and .status == "True")) | length == 0)] | length')
      if [[ "${not_synced}" -eq 0 ]]; then
        log "All ${total} ExternalSecret(s) are synced."
        return 0
      fi
      log "${not_synced}/${total} ExternalSecret(s) not yet synced (${elapsed}s elapsed)."
    fi

    if [[ "${elapsed}" -ge "${timeout_secs}" ]]; then
      log "ERROR: Timed out waiting for ExternalSecrets after ${timeout_secs}s."
      kubectl get externalsecrets -n openstack
      return 1
    fi

    sleep "${interval}"
    elapsed=$((elapsed + interval))
  done
}

# ---------------------------------------------------------------------------
# Pre-flight checks
# ---------------------------------------------------------------------------
preflight() {
  log "Running pre-flight checks ..."

  # Check required CLI tools.
  local required_tools=("docker" "flux" "kind" "kubectl" "jq")
  local missing=()
  for tool in "${required_tools[@]}"; do
    if ! command -v "${tool}" &>/dev/null; then
      missing+=("${tool}")
    fi
  done
  if [[ ${#missing[@]} -gt 0 ]]; then
    log "ERROR: Required tools not found: ${missing[*]}"
    log "Install missing tools and retry. See hack/install-test-deps.sh for automated installation."
    exit 1
  fi
  log "All required CLI tools found."

  # Check Docker daemon.
  if ! docker info > /dev/null 2>&1; then
    log "ERROR: Docker daemon is not running. Please start Docker and retry."
    exit 1
  fi
  log "Docker daemon is running."

  # Check for existing kind cluster.
  if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
    log "WARNING: kind cluster '${CLUSTER_NAME}' already exists. Skipping cluster creation."
    return 1
  fi

  return 0
}

# ===========================================================================
# Main deployment sequence
# ===========================================================================
main() {
  log "=== Infrastructure Deployment — cluster: ${CLUSTER_NAME} ==="

  # --- Pre-flight + Step 1: Create kind cluster ----------------------------
  if preflight; then
    log "[Step 1/8] Creating kind cluster '${CLUSTER_NAME}' ..."
    kind create cluster --name "${CLUSTER_NAME}" --config "${REPO_ROOT}/hack/kind-config.yaml"
    log "kind cluster '${CLUSTER_NAME}' created."
  else
    log "[Step 1/8] Skipping cluster creation (already exists)."
  fi

  # --- Step 2: Install Flux -----------------------------------------------
  log "[Step 2/8] Installing Flux ..."
  flux install
  log "Flux installed."

  # --- Step 3: Apply base kustomization -----------------------------------
  log "[Step 3/8] Applying base kustomization (deploy/kind/base) ..."
  kubectl apply -k "${REPO_ROOT}/deploy/kind/base"
  log "Base kustomization applied."

  # --- Step 4: Wait for HelmReleases Ready --------------------------------
  log "[Step 4/8] Waiting for HelmReleases ..."
  wait_for_helmreleases

  # --- Step 5: Apply infrastructure kustomization -------------------------
  log "[Step 5/8] Applying infrastructure kustomization (deploy/kind/infrastructure) ..."
  kubectl apply -k "${REPO_ROOT}/deploy/kind/infrastructure"
  log "Infrastructure kustomization applied."

  # --- Step 6: Wait for OpenBao pods Ready --------------------------------
  log "[Step 6/8] Waiting for OpenBao pods ..."
  wait_for_pods "openbao-system" "app.kubernetes.io/name=openbao" "${OPENBAO_POD_TIMEOUT}"

  # --- Step 7: OpenBao bootstrap ------------------------------------------
  log "[Step 7/8] Running OpenBao bootstrap scripts ..."

  local bootstrap_dir="${REPO_ROOT}/deploy/openbao/bootstrap"

  # 7a. Init and unseal.
  # kind overlay uses single-node Raft mode (1 replica) — override the default
  # 3-replica PODS array in init-unseal.sh (CC-0010).
  export OPENBAO_PODS="openbao-0"
  log "  Running init-unseal.sh ..."
  bash "${bootstrap_dir}/init-unseal.sh"

  # Extract root token from the Kubernetes Secret created by init-unseal.sh.
  # The secret stores the full bao operator init JSON output under the
  # 'init-output' key. We decode it and extract the root_token field.
  BAO_TOKEN=$(kubectl get secret openbao-init-keys -n openbao-system \
    -o jsonpath='{.data.init-output}' | base64 -d | jq -r '.root_token')
  export BAO_TOKEN
  log "  Extracted BAO_TOKEN from openbao-init-keys secret."

  # 7b–7e. Remaining bootstrap scripts (all require BAO_TOKEN).
  local scripts=(
    "setup-secret-engines.sh"
    "setup-auth.sh"
    "setup-policies.sh"
    "write-bootstrap-secrets.sh"
  )
  for script in "${scripts[@]}"; do
    log "  Running ${script} ..."
    bash "${bootstrap_dir}/${script}"
  done

  log "OpenBao bootstrap complete."

  # --- Step 8: Wait for ExternalSecrets synced ----------------------------
  log "[Step 8/8] Waiting for ExternalSecrets to sync ..."
  wait_for_externalsecrets

  log "=== Infrastructure deployment complete ==="
}

main "$@"
