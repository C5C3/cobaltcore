#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/deploy-mgmt-cluster.sh — Bring up the management cluster of the
# two-cluster devstack.
#
# The management cluster runs the forge operators and nothing else: the
# Keystone, Barbican and ControlPlane CRs live here, while every child they
# project lands on a registered target cluster. The target half is a second kind
# cluster brought up with `INFRA_ONLY=true CLUSTER_NAME=forge-target
# hack/deploy-infra.sh`, which installs the third-party infrastructure (MariaDB,
# Memcached, OpenBao, ESO) and no forge operator.
#
# What this script installs:
#   1. A kind cluster (no config file: no host ports, no NodePort mappings —
#      nothing is served from this cluster).
#   2. flux-operator + FluxInstance/flux, exactly as hack/deploy-infra.sh
#      bootstraps them.
#   3. cert-manager in full, plus the CRD sets the operators' local watches need:
#      mariadb-operator, external-secrets, openbao-operator, and the Prometheus
#      operator CRDs. Every controller-runtime watch registers against the
#      management cluster at builder time, so these kinds have to be installed
#      here even when every child is written elsewhere.
#
# What it does NOT install: the operators themselves. Run
# hack/ci-deploy-operator.sh for each of them afterwards, e.g.
#
#   OPERATOR=keystone IMAGE_REPO=ghcr.io/c5c3/keystone-operator \
#     NAMESPACE=keystone-system hack/ci-deploy-operator.sh
#   OPERATOR=barbican IMAGE_REPO=ghcr.io/c5c3/barbican-operator \
#     NAMESPACE=barbican-system hack/ci-deploy-operator.sh
#
# That script needs cert-manager's CA injection for the webhook certificates,
# which is why cert-manager is waited for before anything else here.
#
# Registering the target cluster (a labelled kubeconfig Secret in
# c5c3-clusters) is a separate step; see docs/reference/target-clusters.md.
#
# Re-runs converge: the cluster create is skipped when the cluster exists and
# every manifest is applied idempotently.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
CLUSTER_NAME="${CLUSTER_NAME:-forge-mgmt}"
HELMRELEASE_TIMEOUT="${HELMRELEASE_TIMEOUT:-600}"

# flux-operator release applied before the FluxInstance CR is created, pinned as
# a script-local constant so Renovate can bump it via renovate.json custom
# managers. hack/deploy-infra.sh carries the same pin for the target cluster;
# the two are bumped together by one customManagers entry and
# tests/unit/hack/deploy_mgmt_cluster_test.sh fails when they drift apart.
FLUX_OPERATOR_VERSION="v0.58.0"

# The Flux source/release pairs applied to this cluster, in dependency order.
# Each entry is `<source file>|<release file>|<namespace>/<release name>`,
# relative to deploy/flux-system/. A plain indexed array of pipe-delimited
# tuples keeps this bash 3.2-compatible (matching the deploy-infra.sh
# convention).
FLUX_RELEASES=(
  "sources/cert-manager.yaml|releases/cert-manager.yaml|cert-manager/cert-manager"
  "sources/mariadb-operator.yaml|releases/mariadb-operator-crds.yaml|mariadb-system/mariadb-operator-crds"
  "sources/external-secrets.yaml|releases/external-secrets.yaml|external-secrets/external-secrets"
  "sources/openbao-operator.yaml|releases/openbao-operator.yaml|openbao-operator-system/openbao-operator"
  "sources/prometheus-community.yaml|releases/prometheus-operator-crds.yaml|monitoring/prometheus-operator-crds"
)

# ---------------------------------------------------------------------------
# log — Print a timestamped log message (ISO 8601 UTC).
# Matches hack/deploy-infra.sh.
# ---------------------------------------------------------------------------
log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

# ---------------------------------------------------------------------------
# preflight_checks — Verify the CLI tools and a running Docker daemon.
# ---------------------------------------------------------------------------
preflight_checks() {
  log "Running pre-flight checks..."

  for cmd in docker kind kubectl; do
    if ! command -v "${cmd}" &>/dev/null; then
      log "ERROR: '${cmd}' is not installed or not in PATH."
      exit 1
    fi
  done

  if ! docker info &>/dev/null; then
    log "ERROR: Docker is not running. Please start Docker and try again."
    exit 1
  fi

  log "Pre-flight checks passed."
}

# ---------------------------------------------------------------------------
# wait_for_fluxinstance — Wait until FluxInstance/flux is Ready.
#
# Gates the source/release applies below: the source.toolkit.fluxcd.io and
# helm.toolkit.fluxcd.io CRDs are materialised only once flux-operator has
# reconciled the FluxInstance, and applying a HelmRepository before that aborts
# the run under `set -euo pipefail`.
#
# Arguments: $1 — timeout in seconds.
# ---------------------------------------------------------------------------
wait_for_fluxinstance() {
  local timeout="$1"
  local deadline=$(( $(date +%s) + timeout ))

  log "Waiting up to ${timeout}s for FluxInstance/flux to become Ready..."

  while true; do
    local ready_status
    ready_status=$(kubectl get fluxinstance/flux -n flux-system \
      -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null) || true

    if [[ "${ready_status}" == "True" ]]; then
      log "FluxInstance/flux is Ready."
      return 0
    fi

    log "  FluxInstance/flux is not Ready yet."

    if [[ $(date +%s) -ge ${deadline} ]]; then
      log "ERROR: Timed out waiting for FluxInstance/flux after ${timeout}s."
      kubectl describe fluxinstance/flux -n flux-system 2>/dev/null || true
      kubectl get fluxreport/flux -n flux-system -o yaml 2>/dev/null || true
      exit 1
    fi

    sleep 10
  done
}

# ---------------------------------------------------------------------------
# wait_for_helmreleases — Wait until every named HelmRelease shows Ready=True.
#
# Arguments: $1 — timeout in seconds, then one or more `namespace/name` entries.
# Namespaces are explicit (unlike deploy-infra.sh's all-namespace lookup)
# because this script applies the releases itself and knows where each lands.
# ---------------------------------------------------------------------------
wait_for_helmreleases() {
  local timeout="$1"
  shift
  local entries=("$@")
  local deadline=$(( $(date +%s) + timeout ))

  log "Waiting up to ${timeout}s for HelmReleases to become Ready: ${entries[*]}"

  while true; do
    local all_ready=true
    local entry ns name ready_status

    for entry in "${entries[@]}"; do
      ns="${entry%/*}"
      name="${entry##*/}"
      ready_status=$(kubectl get helmrelease "${name}" -n "${ns}" \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null) || true

      if [[ "${ready_status}" != "True" ]]; then
        log "  HelmRelease '${ns}/${name}' is not Ready yet."
        all_ready=false
      fi
    done

    if [[ "${all_ready}" == "true" ]]; then
      log "All HelmReleases are Ready."
      return 0
    fi

    if [[ $(date +%s) -ge ${deadline} ]]; then
      log "ERROR: Timed out waiting for HelmReleases after ${timeout}s."
      kubectl get helmrelease --all-namespaces 2>/dev/null || true
      exit 1
    fi

    sleep 10
  done
}

# ---------------------------------------------------------------------------
# reconcile_helmrepository_sources — Annotate every Flux chart source so the
# helm-controller sees a fetched artifact instead of waiting out the source's
# 1h interval. The kubectl-only equivalent of `flux reconcile source helm`,
# copied from hack/deploy-infra.sh.
# ---------------------------------------------------------------------------
reconcile_helmrepository_sources() {
  log "Reconciling Flux chart sources..."
  local kind names name
  for kind in helmrepository ocirepository; do
    names=$(kubectl get "${kind}" -n flux-system -o jsonpath='{.items[*].metadata.name}' 2>/dev/null) || true
    for name in ${names}; do
      kubectl annotate "${kind}/${name}" \
        "reconcile.fluxcd.io/requestedAt=$(date +%s%N)" \
        --overwrite -n flux-system || true
    done
  done
}

# ---------------------------------------------------------------------------
# main — Orchestrate the 3-step bring-up.
# ---------------------------------------------------------------------------
main() {
  log "=========================================="
  log "  Deploy the management cluster"
  log "=========================================="
  log "Cluster name        : ${CLUSTER_NAME}"
  log "HelmRelease timeout : ${HELMRELEASE_TIMEOUT}s"
  log ""

  preflight_checks

  # Step 1: Create the kind cluster. No config file — this cluster serves no
  # traffic, so it needs neither the host-port mappings nor the containerd
  # tuning hack/kind-config.yaml carries for the workload cluster.
  log "=== Step 1/3: Create kind cluster ==="
  if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
    log "Kind cluster '${CLUSTER_NAME}' already exists — skipping creation."
  else
    kind create cluster --name "${CLUSTER_NAME}" --wait 60s
    log "Kind cluster '${CLUSTER_NAME}' created."
  fi
  # Pin the context explicitly: on the skip path above the current context may
  # still be the target cluster's, and every kubectl below has to land here.
  kubectl config use-context "kind-${CLUSTER_NAME}"

  # Step 2: Install flux-operator and apply the FluxInstance. The namespaces
  # manifest comes with it — it declares every namespace the releases below land
  # in, plus the `openstack` namespace the placed CRs live in on this side.
  log "=== Step 2/3: Install flux-operator + apply FluxInstance ==="
  kubectl apply -f \
    "https://github.com/controlplaneio-fluxcd/flux-operator/releases/download/${FLUX_OPERATOR_VERSION}/install.yaml"
  kubectl apply -f "${REPO_ROOT}/deploy/flux-system/namespaces.yaml"
  kubectl apply -f "${REPO_ROOT}/deploy/flux-system/fluxinstance.yaml"
  wait_for_fluxinstance "${HELMRELEASE_TIMEOUT}"
  log "flux-operator installed and FluxInstance/flux is Ready."

  # Step 3: Apply the pinned source/release pairs and wait for them. The files
  # are applied one by one rather than through a kustomization: kustomize refuses
  # `../` resource references under its default load restrictor, so an overlay
  # for this subset would have to duplicate the manifests.
  log "=== Step 3/3: Apply Flux sources and releases ==="
  local entry
  for entry in "${FLUX_RELEASES[@]}"; do
    kubectl apply -f "${REPO_ROOT}/deploy/flux-system/${entry%%|*}"
    local rest="${entry#*|}"
    kubectl apply -f "${REPO_ROOT}/deploy/flux-system/${rest%%|*}"
  done
  reconcile_helmrepository_sources

  # cert-manager first and on its own: hack/ci-deploy-operator.sh depends on its
  # CA injection for the operator webhooks, and external-secrets and
  # openbao-operator declare a dependsOn on it, so nothing else can be Ready
  # before it is.
  log "Phase 1: Waiting for cert-manager..."
  wait_for_helmreleases "${HELMRELEASE_TIMEOUT}" cert-manager/cert-manager

  log "Phase 2: Waiting for the remaining HelmReleases..."
  local remaining=()
  for entry in "${FLUX_RELEASES[@]}"; do
    local release="${entry##*|}"
    if [[ "${release}" != "cert-manager/cert-manager" ]]; then
      remaining+=("${release}")
    fi
  done
  wait_for_helmreleases "${HELMRELEASE_TIMEOUT}" "${remaining[@]}"

  log ""
  log "=========================================="
  log "  Management cluster ready!"
  log "=========================================="
  log "Cluster: ${CLUSTER_NAME} (context kind-${CLUSTER_NAME})"
  log "Next: deploy the operators with hack/ci-deploy-operator.sh, then register"
  log "      the target cluster (see docs/reference/target-clusters.md)."
  log "To tear down: kind delete cluster --name ${CLUSTER_NAME}"
}

# Run main only when executed directly so unit tests (tests/unit/hack/) can
# source this script and exercise individual functions.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
