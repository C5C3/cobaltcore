#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/deploy-infra.sh — Deploy full infrastructure stack to a kind cluster.
# Feature: CC-0010, CC-0085
#
# Implements the 8-step deployment sequence:
#   1. Create kind cluster (using hack/kind-config.yaml)
#   2. Install flux-operator + apply FluxInstance (CC-0085) — applies the
#      ControlPlane flux-operator release, then the bootstrap-scope
#      namespaces.yaml + fluxinstance.yaml, then waits for FluxInstance/flux
#      Ready so the Flux toolkit CRDs are registered before Step 3.
#   3. Apply base kustomize overlay (namespaces, HelmRepositories, HelmReleases)
#   4. Wait for HelmReleases to become Ready (cert-manager first, then TLS
#      prerequisites for OpenBao, then remaining releases)
#   5. Apply infrastructure kustomize overlay (CRD-dependent resources)
#   6. Wait for OpenBao pods to become Ready
#   7. Bootstrap OpenBao (init, unseal, configure)
#   8. Wait for ExternalSecrets to sync
#
# REQ-001 (CC-0085): Fresh-cluster bootstrap installs flux-operator and
#   applies FluxInstance/flux without requiring the Flux CLI.
# REQ-002 (CC-0085): wait_for_fluxinstance gates Step 3 on Ready=True.
# REQ-003 (CC-0085): reconcile_helmrepository_sources replaces
#   `flux reconcile source helm` with a kubectl annotate loop.
# REQ-005 (CC-0085): preflight_checks drops `flux` from required commands.
# REQ-001 (CC-0010): Deploys full infrastructure stack to kind cluster.
# REQ-004 (CC-0010): Applies manifests in two phases with health waits between.
# REQ-005 (CC-0010): Invokes existing OpenBao bootstrap scripts from
#   deploy/openbao/bootstrap/.
# REQ-011 (CC-0010): set -euo pipefail, SPDX Apache-2.0 header, feature ID.
# REQ-012 (CC-0010): Configurable timeouts via environment variables.
# REQ-005 (CC-0088): envoy-gateway HelmRelease is gated in Phase 3 and a
#   Gateway/openstack-gw Programmed=True wait runs after Step 5 (once the
#   EnvoyProxy CR that GatewayClass/envoy's parametersRef targets has been
#   applied via the infrastructure overlay), dumping describe +
#   envoy-gateway-system pod logs on timeout.

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

# Host port that kind binds to forward into the Envoy data-plane NodePort
# (containerPort 31443). Defaults to 443 so the documented Quick Start URL
# `https://keystone.127-0-0-1.nip.io/v3` works unchanged on Linux + rootful
# Docker (the CI baseline). Override to a non-privileged port (e.g. 8443)
# on hosts that cannot bind <1024 — typical on macOS Docker Desktop without
# the `vmnetd` privileged helper, rootless Docker, or Podman. With an
# override, the endpoint becomes `https://keystone.127-0-0-1.nip.io:${KIND_HOST_PORT}/v3`
# and any sample CR's `spec.endpoints.public` must include the same `:PORT`
# suffix. See docs/quick-start.md (CC-0088).
KIND_HOST_PORT="${KIND_HOST_PORT:-443}"

# Gates the opt-in chaos-mesh kind overlay (deploy/kind/chaos-mesh) and the
# host-side kernel-module load. Defaults to false so the kind Quick Start
# stays minimal; set WITH_CHAOS_MESH=true to enable chaos-engineering tests
# (CC-0097).
WITH_CHAOS_MESH="${WITH_CHAOS_MESH:-false}"

# Gateway API CRD release installed before the keystone-operator HelmRelease so
# the operator's HTTPRoute watch has a registered kind at startup (CC-0065).
# Keep aligned with sigs.k8s.io/gateway-api in operators/keystone/go.mod.
GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-v1.1.0}"
GATEWAY_API_CRDS_URL="${GATEWAY_API_CRDS_URL:-https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml}"

# flux-operator release applied in Step 2 before the FluxInstance CR is created
# (CC-0085, REQ-001). Kept as a script-local constant so Renovate can bump it
# via renovate.json custom managers.
FLUX_OPERATOR_VERSION="v0.48.0"

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
# wait_for_fluxinstance — Wait until FluxInstance/flux is Ready (CC-0085, REQ-002).
#
# Polls every 10s up to HELMRELEASE_TIMEOUT for
# `.status.conditions[type=Ready].status == True` on FluxInstance/flux in
# flux-system. On timeout, dumps `kubectl describe fluxinstance/flux` and
# `kubectl get fluxreport/flux -o yaml` for diagnostics, then exits 1.
# ---------------------------------------------------------------------------
wait_for_fluxinstance() {
  local timeout="${1:-${HELMRELEASE_TIMEOUT}}"
  local deadline=$(( $(date +%s) + timeout ))

  log "Waiting up to ${timeout}s for FluxInstance/flux to become Ready..."

  while true; do
    local ready_status
    ready_status=$(kubectl get fluxinstance/flux -n flux-system -o json 2>/dev/null \
      | jq -r '.status.conditions[]? | select(.type == "Ready") | .status' 2>/dev/null) || true

    if [[ "${ready_status}" == "True" ]]; then
      log "FluxInstance/flux is Ready."
      return 0
    fi

    local reason message
    reason=$(kubectl get fluxinstance/flux -n flux-system -o json 2>/dev/null \
      | jq -r '.status.conditions[]? | select(.type == "Ready") | .reason // "Pending"' 2>/dev/null) || true
    message=$(kubectl get fluxinstance/flux -n flux-system -o json 2>/dev/null \
      | jq -r '.status.conditions[]? | select(.type == "Ready") | .message // ""' 2>/dev/null) || true
    log "  FluxInstance/flux is not Ready yet (reason: ${reason:-Pending})."
    if [[ -n "${message}" ]]; then
      log "    ${message}"
    fi

    if [[ $(date +%s) -ge ${deadline} ]]; then
      log "ERROR: Timed out waiting for FluxInstance/flux after ${timeout}s."
      log "FluxInstance description:"
      kubectl describe fluxinstance/flux -n flux-system 2>/dev/null || true
      log "FluxReport:"
      kubectl get fluxreport/flux -n flux-system -o yaml 2>/dev/null || true
      exit 1
    fi

    sleep 10
  done
}

# ---------------------------------------------------------------------------
# reconcile_helmrepository_sources — Force a reconcile of every HelmRepository
# in flux-system by annotating with reconcile.fluxcd.io/requestedAt — the
# kubectl-only equivalent of `flux reconcile source helm` (CC-0085, REQ-003).
#
# A no-op when no HelmRepositories exist (the for-loop body simply does not
# run). Each annotate failure is tolerated (`|| true`) so a transient API
# error on one repo does not abort the whole bootstrap.
# ---------------------------------------------------------------------------
reconcile_helmrepository_sources() {
  log "Reconciling HelmRepository sources..."
  local repos
  repos=$(kubectl get helmrepository -n flux-system -o jsonpath='{.items[*].metadata.name}' 2>/dev/null) || true
  for repo in ${repos}; do
    kubectl annotate "helmrepository/${repo}" \
      "reconcile.fluxcd.io/requestedAt=$(date +%s%N)" \
      --overwrite -n flux-system || true
  done
}

# ---------------------------------------------------------------------------
# wait_for_gateway_programmed — Wait for a Gateway CR to report Programmed=True
# (CC-0088, REQ-005).
#
# Polls every 10s up to the supplied timeout for
# `.status.conditions[type=Programmed].status == True` on the named Gateway.
# On timeout, dumps `kubectl describe gateway/<name>` and the logs of every
# pod in the envoy-gateway-system namespace, then exits 1. This matches the
# diagnostic shape of wait_for_fluxinstance for consistency.
#
# Arguments:
#   $1 — gateway name (e.g., openstack-gw)
#   $2 — namespace (e.g., openstack)
#   $3 — timeout in seconds
# ---------------------------------------------------------------------------
wait_for_gateway_programmed() {
  local name="$1"
  local namespace="$2"
  local timeout="$3"
  local deadline=$(( $(date +%s) + timeout ))

  log "Waiting up to ${timeout}s for Gateway/${name} in namespace '${namespace}' to report Programmed=True..."

  while true; do
    local programmed_status
    programmed_status=$(kubectl get gateway/"${name}" -n "${namespace}" -o json 2>/dev/null \
      | jq -r '.status.conditions[]? | select(.type == "Programmed") | .status' 2>/dev/null) || true

    if [[ "${programmed_status}" == "True" ]]; then
      log "Gateway/${name} is Programmed."
      return 0
    fi

    local reason message
    reason=$(kubectl get gateway/"${name}" -n "${namespace}" -o json 2>/dev/null \
      | jq -r '.status.conditions[]? | select(.type == "Programmed") | .reason // "Pending"' 2>/dev/null) || true
    message=$(kubectl get gateway/"${name}" -n "${namespace}" -o json 2>/dev/null \
      | jq -r '.status.conditions[]? | select(.type == "Programmed") | .message // ""' 2>/dev/null) || true
    log "  Gateway/${name} is not Programmed yet (reason: ${reason:-Pending})."
    if [[ -n "${message}" ]]; then
      log "    ${message}"
    fi

    if [[ $(date +%s) -ge ${deadline} ]]; then
      log "ERROR: Timed out waiting for Gateway/${name} after ${timeout}s."
      log "Gateway description:"
      kubectl describe gateway/"${name}" -n "${namespace}" 2>/dev/null || true
      log "envoy-gateway-system pod logs (last 10m, tail 200):"
      local gw_pods
      gw_pods=$(kubectl get pods -n envoy-gateway-system -o jsonpath='{.items[*].metadata.name}' 2>/dev/null) || true
      # `--since=10m` keeps the dump focused on the most recent failure
      # window. On long-running timeouts the default --tail=200 may already
      # have rolled past the relevant crash frame; the time filter bounds
      # the output to a meaningful post-mortem window (CC-0088).
      for pod in ${gw_pods}; do
        log "--- logs for pod ${pod} ---"
        kubectl logs "${pod}" -n envoy-gateway-system --all-containers=true --since=10m --tail=200 2>/dev/null || true
      done
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
  # Flux CLI is intentionally omitted: bootstrap now installs flux-operator and
  # applies a FluxInstance via kubectl, and source reconciles use kubectl
  # annotate (CC-0085, REQ-005).
  for cmd in docker kind kubectl jq; do
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

  log "Pre-flight checks passed."
}

# ---------------------------------------------------------------------------
# load_chaos_mesh_kernel_modules — Ensure NetworkChaos prerequisites on the host.
#
# chaos-mesh's NetworkChaos uses ipset/iptables/tc inside the target pod's
# network namespace via nsenter. The underlying kernel modules must be loaded
# on the host kernel (Kind nodes share it), otherwise chaos-daemon fails with
# "unable to flush ip sets for pod …" and AllInjected stays False (CC-0049).
#
# Best-effort: skipped on non-Linux, and on Linux we warn but don't abort if
# modprobe is unavailable or fails — PodChaos-only flows still work.
# ---------------------------------------------------------------------------
load_chaos_mesh_kernel_modules() {
  if [[ "$(uname -s)" != "Linux" ]]; then
    log "Non-Linux host — skipping kernel-module load (chaos-mesh NetworkChaos runs in the Linux VM kernel)."
    return 0
  fi

  local sudo_cmd=()
  if [[ "$(id -u)" -ne 0 ]]; then
    if sudo -n true 2>/dev/null; then
      sudo_cmd=(sudo -n)
    else
      log "WARNING: not root and no passwordless sudo — skipping kernel-module load; NetworkChaos may fail."
      return 0
    fi
  fi

  # ip_set_hash_ip is the on-disk module name for the ipset hash:ip type; loading
  # it is enough — chaos-mesh only needs hash:net in practice, which is provided
  # by the same linux-modules-extra package. Keep the list aligned with what
  # chaos-daemon actually invokes via ipset/tc.
  local modules=(ip_set ip_set_hash_ip ip_set_hash_net xt_set sch_netem sch_tbf)
  log "Loading kernel modules for chaos-mesh NetworkChaos: ${modules[*]}"

  local missing=()
  local mod err
  for mod in "${modules[@]}"; do
    if [[ -d "/sys/module/${mod}" ]]; then
      continue
    fi
    if err=$("${sudo_cmd[@]}" modprobe "${mod}" 2>&1); then
      continue
    fi
    log "modprobe ${mod} failed: ${err}"
    missing+=("${mod}")
  done

  if [[ ${#missing[@]} -eq 0 ]]; then
    return 0
  fi

  # Ubuntu cloud images commonly omit linux-modules-extra, which ships ip_set,
  # xt_set and friends. Install it on demand and retry the modules that failed.
  if ! command -v apt-get &>/dev/null; then
    log "WARNING: modules missing and apt-get unavailable — NetworkChaos tests may fail: ${missing[*]}"
    return 0
  fi

  local kver extra_pkg
  kver="$(uname -r)"
  extra_pkg="linux-modules-extra-${kver}"
  log "Installing ${extra_pkg} to provide missing modules: ${missing[*]}"
  if ! "${sudo_cmd[@]}" apt-get update -qq; then
    log "WARNING: apt-get update failed — NetworkChaos tests may fail."
    return 0
  fi
  if ! "${sudo_cmd[@]}" apt-get install -y -qq "${extra_pkg}"; then
    log "WARNING: apt-get install ${extra_pkg} failed — NetworkChaos tests may fail."
    return 0
  fi

  local still_missing=()
  for mod in "${missing[@]}"; do
    if [[ -d "/sys/module/${mod}" ]]; then
      continue
    fi
    if err=$("${sudo_cmd[@]}" modprobe "${mod}" 2>&1); then
      continue
    fi
    log "modprobe ${mod} still failed after installing ${extra_pkg}: ${err}"
    still_missing+=("${mod}")
  done

  if [[ ${#still_missing[@]} -ne 0 ]]; then
    log "WARNING: kernel modules still missing after retry — NetworkChaos tests may fail: ${still_missing[*]}"
  fi
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
# render_kind_config — Produce the kind-config YAML that `kind create cluster`
# should consume, applying the `KIND_HOST_PORT` override when set (CC-0088).
#
# When `KIND_HOST_PORT == 443` (the default), the checked-in
# hack/kind-config.yaml is copied verbatim — no `yq` dependency at runtime.
# Otherwise, `yq` rewrites the single `nodes[0].extraPortMappings[]` entry
# whose `hostPort` is 443, leaving `containerPort` (31443), `protocol` (TCP),
# and `listenAddress` (127.0.0.1) untouched. The Envoy proxy NodePort and
# the Gateway listener port are intentionally unaffected — only the host-
# side binding moves to a non-privileged port.
#
# Arguments:
#   $1 — destination path for the rendered config
# Errors:
#   - exits 1 if KIND_HOST_PORT is not a positive integer in [1, 65535]
#   - exits 1 if `yq` is required (override path) but not on PATH
# ---------------------------------------------------------------------------
render_kind_config() {
  local out_path="$1"
  local src="${SCRIPT_DIR}/kind-config.yaml"

  if [[ ! "${KIND_HOST_PORT}" =~ ^[0-9]+$ ]] \
    || (( KIND_HOST_PORT < 1 || KIND_HOST_PORT > 65535 )); then
    log "ERROR: KIND_HOST_PORT='${KIND_HOST_PORT}' is not a valid TCP port (1-65535)."
    exit 1
  fi

  if [[ "${KIND_HOST_PORT}" == "443" ]]; then
    cp "${src}" "${out_path}"
    return 0
  fi

  if ! command -v yq >/dev/null 2>&1; then
    log "ERROR: KIND_HOST_PORT=${KIND_HOST_PORT} (override) requires 'yq' on PATH."
    exit 1
  fi

  # Select-and-mutate the single hostPort=443 entry; idempotent if the input
  # already uses the override port (yq's `select(... == 443)` matches nothing
  # and the document passes through unchanged).
  KIND_HOST_PORT="${KIND_HOST_PORT}" yq \
    '(.nodes[0].extraPortMappings[] | select(.hostPort == 443)).hostPort = (env(KIND_HOST_PORT) | tonumber)' \
    "${src}" > "${out_path}"
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
  log "Kind host port      : ${KIND_HOST_PORT} → 31443 (override via KIND_HOST_PORT)"
  log "Chaos Mesh         : ${WITH_CHAOS_MESH} (set WITH_CHAOS_MESH=true to install)"
  log ""

  # Pre-flight checks
  preflight_checks

  # Load chaos-mesh kernel modules on the host before creating the cluster.
  # Kind nodes share the host kernel; NetworkChaos needs ipset/tc modules.
  # Gated on WITH_CHAOS_MESH so the default Quick Start does not require
  # passwordless sudo or modprobe access (CC-0097).
  if [[ "${WITH_CHAOS_MESH}" == "true" ]]; then
    load_chaos_mesh_kernel_modules
  else
    log "Skipping chaos-mesh kernel modules (WITH_CHAOS_MESH=false)."
  fi

  # Step 1: Create kind cluster
  log "=== Step 1/8: Create kind cluster ==="
  if [[ "${SKIP_KIND_CREATE:-false}" == "true" ]]; then
    log "SKIP_KIND_CREATE=true — assuming kind cluster '${CLUSTER_NAME}' already exists (CI mode)."
  elif kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
    log "Kind cluster '${CLUSTER_NAME}' already exists — skipping creation."
  else
    # Render kind-config.yaml into a tempfile so KIND_HOST_PORT overrides
    # take effect without mutating the checked-in file (CC-0088). We do not
    # install a cleanup trap here: openbao_bootstrap registers its own EXIT
    # trap later in the run and a second `trap ... EXIT` would overwrite it,
    # leaking BAO_TOKEN into the environment. The tempfile is a few hundred
    # bytes and `/tmp` is volume-cleared at reboot, so explicit deletion
    # post-success is sufficient.
    local kind_cfg
    kind_cfg="$(mktemp -t forge-kind-config.XXXXXX.yaml)"
    render_kind_config "${kind_cfg}"
    kind create cluster \
      --name "${CLUSTER_NAME}" \
      --config "${kind_cfg}" \
      --wait 60s
    rm -f "${kind_cfg}"
    log "Kind cluster '${CLUSTER_NAME}' created."
  fi

  # Step 2: Install flux-operator and apply FluxInstance (CC-0085, REQ-001/REQ-002)
  #
  # Only the two bootstrap-scope manifests are applied here — the Namespace
  # resources and the FluxInstance CR. HelmRepository/HelmRelease objects from
  # deploy/flux-system/{sources,releases}/ intentionally come later (Step 3):
  # flux-operator's install.yaml only registers the fluxcd.controlplane.io
  # CRDs, and the source.toolkit.fluxcd.io / helm.toolkit.fluxcd.io CRDs
  # consumed by those objects are materialised only after the flux-operator
  # reconciles this FluxInstance. Applying them before wait_for_fluxinstance
  # would abort the script under `set -euo pipefail` with 'no matches for kind
  # "HelmRepository" in version "source.toolkit.fluxcd.io/v1"' (CC-0085).
  log "=== Step 2/8: Install flux-operator + apply FluxInstance ==="
  kubectl apply -f \
    "https://github.com/controlplaneio-fluxcd/flux-operator/releases/download/${FLUX_OPERATOR_VERSION}/install.yaml"
  kubectl apply -f "${REPO_ROOT}/deploy/flux-system/namespaces.yaml"
  kubectl apply -f "${REPO_ROOT}/deploy/flux-system/fluxinstance.yaml"
  wait_for_fluxinstance "${HELMRELEASE_TIMEOUT}"
  log "flux-operator installed and FluxInstance/flux is Ready."

  # Gateway API CRDs (CC-0065). Installed before the base kustomize overlay so
  # the keystone-operator Pod (deployed via HelmRelease in Step 3/4) finds the
  # gateway.networking.k8s.io/v1 HTTPRoute kind at startup. Without this, the
  # operator logs 'no matches for kind HTTPRoute' and never becomes Ready.
  # server-side apply avoids the 'metadata.annotations: Too long' error that
  # client-side apply hits on the upstream CRD bundle.
  log "=== Installing Gateway API CRDs (${GATEWAY_API_VERSION}) ==="
  local gwapi_attempts=3
  local gwapi_attempt=0
  local gwapi_delay=5
  while (( gwapi_attempt < gwapi_attempts )); do
    gwapi_attempt=$((gwapi_attempt + 1))
    if kubectl apply --server-side -f "${GATEWAY_API_CRDS_URL}"; then
      break
    fi
    if (( gwapi_attempt >= gwapi_attempts )); then
      log "ERROR: Failed to install Gateway API CRDs after ${gwapi_attempts} attempts from ${GATEWAY_API_CRDS_URL}"
      exit 1
    fi
    log "  Gateway API CRD apply failed (attempt ${gwapi_attempt}/${gwapi_attempts}); retrying in ${gwapi_delay}s..."
    sleep "${gwapi_delay}"
  done
  log "Gateway API CRDs installed."

  # Step 3: Apply base kustomize overlay (namespaces, HelmRepos, HelmReleases)
  #
  # Safe to run only after Step 2's wait_for_fluxinstance succeeds — at that
  # point flux-operator has materialised the Flux toolkit CRDs (source/helm
  # /kustomize/notification), so HelmRepository and HelmRelease objects under
  # deploy/flux-system/{sources,releases}/ resolve to known Kinds (CC-0085).
  log "=== Step 3/8: Apply base kustomize overlay ==="
  kubectl apply -k "${REPO_ROOT}/deploy/kind/base"
  log "Base kustomize overlay applied."

  # Opt-in chaos-mesh overlay (CC-0097). Layered on top of the base so the
  # default Quick Start stays minimal; enable with WITH_CHAOS_MESH=true.
  # The overlay is self-contained (no `../../` parent-dir references), so
  # kubectl's embedded kustomize renders it under the default
  # LoadRestrictionsRootOnly security check — no `--load-restrictor` flag
  # required (kubectl's embedded kustomize does not expose one,
  # kubernetes/kubectl#948).
  if [[ "${WITH_CHAOS_MESH}" == "true" ]]; then
    kubectl apply -k "${REPO_ROOT}/deploy/kind/chaos-mesh"
    log "Chaos Mesh kind overlay applied (WITH_CHAOS_MESH=true)."
  fi

  # Force-reconcile HelmRepository sources so chart indexes are available
  # before HelmReleases attempt to resolve charts. Without this, the
  # helm-controller may see unindexed sources and wait until the next
  # reconcile interval (up to 1h) before retrying.
  reconcile_helmrepository_sources

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
  # envoy-gateway is kind-only (deploy/kind/base/envoy-gateway.yaml) and provides
  # the GatewayClass consumed by Gateway/openstack-gw; gating it here ensures
  # the wait_for_gateway_programmed poll below finds a reconciling controller
  # (CC-0088, REQ-005).
  log "Phase 3: Waiting for remaining HelmReleases..."
  # Build the release list dynamically so chaos-mesh is only awaited when the
  # opt-in overlay was applied (CC-0097). The surviving non-chaos order is
  # preserved exactly as before; chaos-mesh is appended last to avoid moving
  # any other release's relative position.
  local helm_releases=(prometheus-operator-crds openbao mariadb-operator-crds mariadb-operator external-secrets memcached-operator envoy-gateway)
  if [[ "${WITH_CHAOS_MESH}" == "true" ]]; then
    helm_releases+=(chaos-mesh)
  fi
  wait_for_helmreleases "${HELMRELEASE_TIMEOUT}" "${helm_releases[@]}"

  # Step 5: Apply infrastructure kustomize overlay (CRD-dependent resources)
  log "=== Step 5/8: Apply infrastructure kustomize overlay ==="

  # Wait for operator CRDs to be registered before applying CRD-dependent
  # resources. HelmRelease Ready does not guarantee CRDs are available in
  # the API server — the operator pods may still be starting.
  # envoyproxies.gateway.envoyproxy.io is installed by the envoy-gateway
  # HelmRelease (Phase 3 above) and is required by the EnvoyProxy CR in
  # deploy/kind/infrastructure/envoy-nodeport.yaml (CC-0088).
  wait_for_crds "${POD_TIMEOUT}" \
    memcacheds.memcached.c5c3.io \
    clustersecretstores.external-secrets.io \
    externalsecrets.external-secrets.io \
    mariadbs.k8s.mariadb.com \
    envoyproxies.gateway.envoyproxy.io

  # Invalidate kubectl's client-side discovery cache so that the newly
  # registered CRDs are visible to kubectl apply.
  kubectl api-resources > /dev/null 2>&1 || true
  kubectl apply -k "${REPO_ROOT}/deploy/kind/infrastructure"
  log "Infrastructure kustomize overlay applied."

  # Gateway/openstack-gw can only report Programmed=True after the
  # EnvoyProxy CR (applied via the infrastructure overlay above) binds
  # its parametersRef on GatewayClass/envoy — so this wait must run
  # AFTER Step 5, not between Phase 3 and Step 5. Downstream HTTPRoute
  # resources (operator-created from keystone-api spec.gateway) need a
  # Programmed listener to bind to (CC-0088, REQ-005).
  wait_for_gateway_programmed openstack-gw openstack "${HELMRELEASE_TIMEOUT}"

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

# Run main only when executed directly so unit tests (tests/unit/hack/) can
# source this script and exercise individual functions (CC-0085, REQ-003/REQ-005).
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
