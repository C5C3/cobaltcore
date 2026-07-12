#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/deploy-infra.sh — Deploy full infrastructure stack to a kind cluster.
#
# Implements the 8-step deployment sequence:
#   1. Create kind cluster (using hack/kind-config.yaml)
#   2. Install flux-operator + apply FluxInstance — applies the
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
# Fresh-cluster bootstrap installs flux-operator and
#   applies FluxInstance/flux without requiring the Flux CLI.
# wait_for_fluxinstance gates Step 3 on Ready=True.
# reconcile_helmrepository_sources replaces
#   `flux reconcile source helm` with a kubectl annotate loop.
# preflight_checks drops `flux` from required commands.
# Deploys full infrastructure stack to kind cluster.
# Applies manifests in two phases with health waits between.
# Invokes existing OpenBao bootstrap scripts from
#   deploy/openbao/bootstrap/.
# set -euo pipefail, SPDX Apache-2.0 header, feature ID.
# Configurable timeouts via environment variables.
# envoy-gateway HelmRelease is gated in Phase 3 and a
#   Gateway/openstack-gw Programmed=True wait runs after Step 5 (once the
#   EnvoyProxy CR that GatewayClass/envoy's parametersRef targets has been
#   applied via the infrastructure overlay), dumping describe +
#   envoy-gateway-system pod logs on timeout.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
CLUSTER_NAME="${CLUSTER_NAME:-forge}"
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
# suffix. See docs/quick-start.md.
KIND_HOST_PORT="${KIND_HOST_PORT:-443}"

# Soft/hard RLIMIT_NOFILE applied to every kind node's containerd after cluster
# creation (see cap_node_nofile). Docker Desktop ships containerd with
# LimitNOFILE=infinity, so pods inherit ~1e9 open files; uWSGI (the Keystone API
# server) then allocates memory proportional to the fd count at worker startup
# and is OOM-killed regardless of its memory limit. 1048576 matches the kind node
# container's own limit and is ample for every workload in this stack. Set to
# empty to skip the cap entirely (e.g. on a runtime that already ships a sane
# limit). See #546.
#
# Uses `-` (not `:-`) so an explicitly-empty NODE_NOFILE_LIMIT= opts out, while
# an unset variable still takes the 1048576 default.
NODE_NOFILE_LIMIT="${NODE_NOFILE_LIMIT-1048576}"

# Gates the opt-in chaos-mesh kind overlay (deploy/kind/chaos-mesh) and the
# host-side kernel-module load. Defaults to false so the kind Quick Start
# stays minimal; set WITH_CHAOS_MESH=true to enable chaos-engineering tests
WITH_CHAOS_MESH="${WITH_CHAOS_MESH:-false}"

# Gates the opt-in kube-prometheus-stack kind overlay (deploy/kind/prometheus)
# which installs Prometheus + Grafana for visualising keystone-operator
# metrics. Defaults to false so the kind Quick Start stays minimal; set
# WITH_PROMETHEUS=true to install the monitoring stack.
WITH_PROMETHEUS="${WITH_PROMETHEUS:-false}"

# Gates the opt-in metrics-server kind overlay (deploy/kind/metrics-server)
# required by the HPA/autoscaling recipe: without a resource-metrics API the
# generated HorizontalPodAutoscaler reports `unknown/80%` and never scales.
# Defaults to false so the kind Quick Start stays minimal; set
# WITH_METRICS_SERVER=true to install it.
WITH_METRICS_SERVER="${WITH_METRICS_SERVER:-false}"

# Gates the opt-in transparent registry pull-through cache (#564). When true,
# deploy-infra brings up one small distribution-registry (registry:2/3) proxy
# per upstream registry on the `kind` Docker network (start_registry_cache),
# injects a containerd `config_path` registry-mirror patch into the rendered
# kind config (render_kind_config), and writes a `certs.d/<host>/hosts.toml`
# into every node so containerd resolves unmodified image refs
# (`ghcr.io/c5c3/keystone:…`) through the local cache (wire_node_registry_mirror).
# Each proxy has its own persistent Docker volume, so the cache survives
# `kind delete` / recreate cycles. Defaults to false so the default Quick Start
# and every CI job are byte-for-byte unchanged — the cache is strictly local-dev
# only. Mirror entries advertise `capabilities = ["pull", "resolve"]`, so
# containerd falls back to the origin registry if a proxy is down: the cache can
# never hard-break a pull.
#
# The engine is the standard `registry` (CNCF distribution) in pull-through
# proxy mode, NOT Zot: distribution streams each blob from the upstream to
# containerd while caching it inline, so even a cold first pull runs at ~origin
# speed. Zot's `sync` extension copies the whole image into its own store before
# serving (returning 404s to containerd until the copy finishes), which makes a
# cold deploy slower, not faster — the wrong shape for transparent mirroring.
WITH_REGISTRY_CACHE="${WITH_REGISTRY_CACHE:-false}"

# Multi-arch distribution-registry image (linux/amd64 + linux/arm64) used for
# every pull-through cache container. Pinned by tag AND digest so a `docker pull`
# is reproducible and Renovate can bump both (see renovate.json → the
# docker.io/library/registry custom manager). Run in proxy mode purely via the
# REGISTRY_PROXY_REMOTEURL env var, so no config file has to be rendered or
# mounted — the image's default config serves the proxy on :5000 with filesystem
# storage at /var/lib/registry.
#
# Deliberately pinned to the 2.8.x line, NOT distribution 3.x: 3.x regressed
# pull-through proxying of GHCR-backed vanity registries — `oci.external-secrets.io`
# (an external-secrets vanity front for ghcr.io) 404s under registry:3.1.1 but
# serves fine under 2.8.3. renovate.json disables MAJOR bumps for this pin so it
# stays on 2.x until 3.x fixes the regression; minor/patch (2.8.x) still automerge.
REGISTRY_CACHE_IMAGE="registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"

# Docker network the proxies attach to. kind puts every node on a single
# user-defined bridge network named `kind` (shared across all clusters, not
# per-CLUSTER_NAME), whose embedded DNS resolves the proxy container names.
# Keeping the caches on this network — and NOT scoping their names by
# CLUSTER_NAME — lets a single warm cache serve every kind cluster on the host.
# Override only if you run kind with KIND_EXPERIMENTAL_DOCKER_NETWORK set.
KIND_DOCKER_NETWORK="${KIND_DOCKER_NETWORK:-kind}"

# The upstream registries fronted by the cache, one proxy each. Each entry is
# `<certs.d host>|<upstream URL>|<name suffix>`:
#   - certs.d host   — the directory name under /etc/containerd/certs.d/ and the
#                      registry namespace containerd resolves (e.g. docker.io).
#   - upstream URL    — the registry proxy's REGISTRY_PROXY_REMOTEURL AND the
#                      containerd `server` fallback (docker.io resolves to
#                      registry-1.docker.io).
#   - name suffix     — appended to `registry-cache-` for the container/volume
#                      names.
# A plain indexed array of pipe-delimited tuples (not an associative array) keeps
# this bash 3.2-compatible. The set is the six registry hosts a live
# WITH_CONTROLPLANE deploy actually pulls from, taken from a pod-image inventory:
# the four common hubs (docker.io, ghcr.io, registry.k8s.io, quay.io) plus two
# per-project vanity fronts — oci.external-secrets.io (external-secrets, backed by
# ghcr.io) and docker-registry3.mariadb.com (mariadb-operator). gcr.io is not used.
# A registry NOT listed here simply pulls from its origin (uncached), so extend
# this list if a future component introduces another host.
REGISTRY_CACHE_UPSTREAMS=(
  "docker.io|https://registry-1.docker.io|dockerio"
  "ghcr.io|https://ghcr.io|ghcr"
  "registry.k8s.io|https://registry.k8s.io|k8s"
  "quay.io|https://quay.io|quay"
  "oci.external-secrets.io|https://oci.external-secrets.io|eso"
  "docker-registry3.mariadb.com|https://docker-registry3.mariadb.com|mariadb"
)

# Gates the opt-in c5c3 ControlPlane bring-up. When true, deploy-infra
# does NOT create the shared MariaDB/Memcached CRs — the c5c3 ControlPlane
# provisions them in managed mode — and the c5c3-operator, K-ORC, and
# keystone-operator are deployed (not suspended) so a ControlPlane CR can
# reconcile the full chain end-to-end. The CR itself is NOT applied by default:
# the ControlPlane Quick Start has the user create and apply it by hand (just as
# the per-service Quick Start applies the Keystone CR), so deploy-infra only
# brings up the operator stack. Defaults to false so the default Quick Start and
# the keystone E2E path are unchanged.
WITH_CONTROLPLANE="${WITH_CONTROLPLANE:-false}"

# Companion to WITH_CONTROLPLANE: when both are true, deploy-infra also applies
# the bundled ControlPlane CR (deploy/kind/controlplane) automatically — the old
# all-in-one behaviour, kept for demos and automation. Defaults to false so the
# Quick Start's manual `kubectl apply` step is the norm. Ignored unless
# WITH_CONTROLPLANE=true.
WITH_CONTROLPLANE_CR="${WITH_CONTROLPLANE_CR:-false}"

# Name of the ControlPlane CR brought up under WITH_CONTROLPLANE=true. The
# c5c3-operator projects its Keystone Service as "{CONTROLPLANE_NAME}-keystone",
# and the per-CR Model B admin-password bootstrap path is derived from it.
# Keep this in lockstep with metadata.name of the CR you apply (by hand, or the
# bundled one under WITH_CONTROLPLANE_CR=true, which is renamed to match). Defaults
# to "controlplane". Ignored unless WITH_CONTROLPLANE=true.
CONTROLPLANE_NAME="${CONTROLPLANE_NAME:-controlplane}"

# Selects HOW the ControlPlane operator stack (keystone-operator, K-ORC,
# c5c3-operator) is provided under WITH_CONTROLPLANE=true:
#   flux     — the default: deploy the published c5c3-operator chart and the
#              K-ORC Flux GitRepository/Kustomization, and un-suspend
#              keystone-operator so the c5c3-operator dependsOn is satisfied.
#   external — the operators are deployed OUT OF BAND (e.g. the e2e-controlplane
#              CI job installs keystone-operator + c5c3-operator as local dev
#              images via hack/ci-deploy-operator.sh and K-ORC via
#              hack/ci-deploy-korc.sh). deploy-infra then only prepares the
#              shared prerequisites (TLS issuers, OpenBao + per-CR seeding, ESO
#              store) and SUSPENDS the Flux ControlPlane stack so it does not
#              fight the dev operators or block on the GHCR-published chart.
# Ignored unless WITH_CONTROLPLANE=true.
CONTROLPLANE_OPERATORS="${CONTROLPLANE_OPERATORS:-flux}"

# Single-node footprint of the ControlPlane-projected backing services. On the
# WITH_CONTROLPLANE path the c5c3-operator provisions MariaDB and Memcached itself
# from the ControlPlane CR; these knobs patch spec.infrastructure.{database,cache}
# of the bundled CR before it is applied (WITH_CONTROLPLANE_CR=true).
# Default 1 replica so a single-node kind gets a single-instance non-Galera MariaDB
# and a single Memcached pod (the CRD default is 3, which spins up a 3-node Galera
# cluster plus 3 Memcached pods and OOM-kills a laptop-sized kind).
#   CONTROLPLANE_DB_REPLICAS=3     — Galera HA cluster (2 is rejected by the c5c3
#                                    validating webhook: Galera needs a quorum).
#   CONTROLPLANE_CACHE_REPLICAS=N  — N Memcached pods.
#   CONTROLPLANE_DB_STORAGE=100Gi  — per-replica MariaDB volume size (default 512Mi,
#                                    a test-sized volume vs the 100Gi CRD default
#                                    that a kind/CI run never fills). Must be a
#                                    Kubernetes quantity in Mi/Gi/Ti.
# database.replicas AND database.storageSize are IMMUTABLE after the ControlPlane CR
# is created, so change them on a fresh environment (teardown-infra first);
# cache.replicas is reconciled live.
# Ignored unless WITH_CONTROLPLANE=true and WITH_CONTROLPLANE_CR=true.
CONTROLPLANE_DB_REPLICAS="${CONTROLPLANE_DB_REPLICAS:-1}"
CONTROLPLANE_CACHE_REPLICAS="${CONTROLPLANE_CACHE_REPLICAS:-1}"
CONTROLPLANE_DB_STORAGE="${CONTROLPLANE_DB_STORAGE:-512Mi}"

# Gateway API CRD release installed before the keystone-operator HelmRelease so
# the operator's HTTPRoute watch has a registered kind at startup.
# Keep aligned with sigs.k8s.io/gateway-api in operators/keystone/go.mod.
GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-v1.1.0}"
GATEWAY_API_CRDS_URL="${GATEWAY_API_CRDS_URL:-https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml}"

# flux-operator release applied in Step 2 before the FluxInstance CR is created
# Kept as a script-local constant so Renovate can bump it
# via renovate.json custom managers.
FLUX_OPERATOR_VERSION="v0.54.1"

# OpenBao init parameters (match deploy/openbao/bootstrap/init-unseal.sh)
KEY_SHARES=5
KEY_THRESHOLD=3
# OPENBAO_NAMESPACE is the namespace the OpenBao server runs in. The bootstrap
# scripts (deploy/openbao/bootstrap/) resolve the same OPENBAO_NAMESPACE env var
# in common.sh (same 'openbao-system' default), so setting it once configures
# both layers — the export below propagates it to the child scripts. The generic
# NAMESPACE env var is deliberately NOT honored: chainsaw injects
# NAMESPACE=<ephemeral test namespace> into every e2e script step, which must
# not redirect where the bootstrap scripts exec their bao commands.
OPENBAO_NAMESPACE="${OPENBAO_NAMESPACE:-openbao-system}"
export OPENBAO_NAMESPACE
BAO_ADDR="${BAO_ADDR:-https://127.0.0.1:8200}"
VAULT_CACERT="${VAULT_CACERT:-/openbao/tls/ca.crt}"
VAULT_CLIENT_CERT="${VAULT_CLIENT_CERT:-/openbao/client-tls/tls.crt}"
VAULT_CLIENT_KEY="${VAULT_CLIENT_KEY:-/openbao/client-tls/tls.key}"
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
# wait_for_fluxinstance — Wait until FluxInstance/flux is Ready.
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
# kubectl-only equivalent of `flux reconcile source helm`.
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
      # the output to a meaningful post-mortem window.
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
  # annotate.
  for cmd in docker kind kubectl jq; do
    if ! command -v "${cmd}" &>/dev/null; then
      log "ERROR: '${cmd}' is not installed or not in PATH."
      exit 1
    fi
  done

  # yq is a hard dependency only on the WITH_CONTROLPLANE path: Step 5 pipes the
  # rendered infrastructure overlay through `yq eval` to drop the MariaDB/Memcached
  # CRs (the ControlPlane provisions those in managed mode). Check it up front so
  # the run fails here instead of deep in Step 5 after a kind cluster already
  # exists. The default Quick Start stays yq-free.
  if [[ "${WITH_CONTROLPLANE}" == "true" ]] && ! command -v yq &>/dev/null; then
    log "ERROR: WITH_CONTROLPLANE=true requires 'yq' on PATH (used to drop MariaDB/Memcached from the infrastructure overlay)."
    exit 1
  fi

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
# "unable to flush ip sets for pod …" and AllInjected stays False.
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
# enable_operator_servicemonitor RELEASE NAMESPACE [TIMEOUT] — Toggle the given
# operator chart's `monitoring.serviceMonitor.enabled` value to true so the
# kube-prometheus-stack Prometheus instance can scrape the operator metrics
# endpoint via the rendered ServiceMonitor. Used for both keystone-operator
# (keystone-system) and horizon-operator (horizon-system).
#
# Callable only when WITH_PROMETHEUS=true. Patches spec.values via
# strategic-merge so any other values set on the HelmRelease (including the
# kind-base suspend patch) are preserved.
#
# DECISION: handle suspended HelmRelease in kind
# Ambiguity: deploy/kind/base/kustomization.yaml suspends the operator
#   HelmReleases so CI can `helm install` them manually with a locally built
#   image (and the ControlPlane path un-suspends them). A suspended HelmRelease
#   never reconciles, so a wait-for-Ready against it would always time out.
# Chose: patch spec.values regardless of suspend state, then read the current
#   spec.suspend; skip the wait when suspended (logging the rationale) and
#   wait for Ready otherwise. The patch remains durable: when ci-deploy-
#   operator.sh later installs the chart, its callers can read the value, and
#   if the suspend patch is removed the HelmRelease will reconcile on the new
#   values without further action.
# Reason: matches the task contract literally for non-kind callers while
#   keeping the kind path green; the suspend semantics are owned by Flux, not
#   by this script.
# ---------------------------------------------------------------------------
enable_operator_servicemonitor() {
  local release="$1"
  local namespace="$2"
  local timeout="${3:-${HELMRELEASE_TIMEOUT}}"

  log "Enabling ${release} ServiceMonitor..."
  kubectl patch helmrelease "${release}" -n "${namespace}" --type=merge \
    -p '{"spec":{"values":{"monitoring":{"serviceMonitor":{"enabled":true}}}}}'

  local suspended
  suspended=$(kubectl get helmrelease "${release}" -n "${namespace}" \
    -o jsonpath='{.spec.suspend}' 2>/dev/null || true)
  if [[ "${suspended}" == "true" ]]; then
    log "${release} HelmRelease is suspended (kind base patch); skipping reconcile wait."
    log "  spec.values patch is durable — re-enable Flux management or run ci-deploy-operator.sh"
    log "  with monitoring.serviceMonitor.enabled=true to render the ServiceMonitor."
    return 0
  fi

  wait_for_helmreleases "${timeout}" "${release}"
  log "${release} HelmRelease reconciled with monitoring.serviceMonitor.enabled=true."
}

# ---------------------------------------------------------------------------
# openbao_kube_exec — Execute a command inside the openbao-0 pod.
# Does NOT pass BAO_TOKEN (used for init/unseal before the token exists).
# ---------------------------------------------------------------------------
openbao_kube_exec() {
  kubectl exec -n "${OPENBAO_NAMESPACE}" openbao-0 -- \
    env BAO_ADDR="${BAO_ADDR}" VAULT_CACERT="${VAULT_CACERT}" VAULT_CLIENT_CERT="${VAULT_CLIENT_CERT}" VAULT_CLIENT_KEY="${VAULT_CLIENT_KEY}" "$@"
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
  # (success, set -e failure, or signal), not only on success.
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
# openbao_onboard_database_tenant — Provision the OpenBao database-engine
# connection + per-tenant role for a managed ControlPlane, once its MariaDB is
# Ready. This is the stage-(b) tenant-onboarding step (#439): managed-mode
# Keystone draws engine-issued credentials from database/mariadb/creds/keystone-
# <ns>-<cp>, which only exist after setup-database-tenant.sh configures the role.
#
# Arguments:
#   $1 — ControlPlane namespace
#   $2 — ControlPlane name
#   $3 — MariaDB CR name (optional, default openstack-db)
# ---------------------------------------------------------------------------
openbao_onboard_database_tenant() {
  local cp_ns="$1"
  local cp_name="$2"
  local mariadb_name="${3:-openstack-db}"

  log "--- Onboarding OpenBao database-engine tenant '${cp_ns}/${cp_name}' ---"

  # The ControlPlane projects the MariaDB after it reconciles; wait for the CR to
  # appear before waiting on its Ready condition (kubectl wait errors on a
  # not-yet-existent resource).
  log "Waiting for MariaDB '${mariadb_name}' to be projected in namespace '${cp_ns}'..."
  local i
  for i in $(seq 1 30); do
    if kubectl get "mariadb/${mariadb_name}" -n "${cp_ns}" >/dev/null 2>&1; then
      break
    fi
    sleep 10
  done
  log "Waiting for MariaDB '${mariadb_name}' to become Ready..."
  if ! kubectl wait "mariadb/${mariadb_name}" -n "${cp_ns}" \
    --for=condition=Ready --timeout="${POD_TIMEOUT}s"; then
    log "ERROR: MariaDB '${mariadb_name}' in namespace '${cp_ns}' did not become Ready; cannot onboard the database-engine tenant."
    exit 1
  fi

  export BAO_TOKEN
  trap 'unset BAO_TOKEN' EXIT
  BAO_TOKEN=$(kubectl get secret "${SECRET_NAME}" -n "${OPENBAO_NAMESPACE}" \
    -o jsonpath='{.data.init-output}' | base64 -d | jq -r '.root_token')
  if [[ -z "${BAO_TOKEN}" || "${BAO_TOKEN}" == "null" ]]; then
    log "ERROR: Could not extract root token from ${OPENBAO_NAMESPACE}/${SECRET_NAME}."
    exit 1
  fi

  local tenant_script="${REPO_ROOT}/deploy/openbao/bootstrap/setup-database-tenant.sh"
  log "Running setup-database-tenant.sh ${cp_ns} ${cp_name}..."
  bash "${tenant_script}" "${cp_ns}" "${cp_name}"
  unset BAO_TOKEN
  log "Database-engine tenant '${cp_ns}/${cp_name}' onboarded."
}

# ---------------------------------------------------------------------------
# render_kind_config — Produce the kind-config YAML that `kind create cluster`
# should consume, applying the `KIND_HOST_PORT` override and/or the
# WITH_REGISTRY_CACHE containerd registry-mirror patch when requested.
#
# When neither knob is active (`KIND_HOST_PORT == 443` and
# WITH_REGISTRY_CACHE != true — the default), the checked-in
# hack/kind-config.yaml is copied verbatim — no `yq` dependency at runtime, so
# CI (which feeds hack/kind-config.yaml straight to helm/kind-action) stays
# byte-for-byte unchanged.
#
# Otherwise `yq` is required and the applicable transforms are layered onto a
# copy of the checked-in file:
#   - KIND_HOST_PORT override: rewrite the single `nodes[0].extraPortMappings[]`
#     entry whose `hostPort` is 443, leaving `containerPort` (31443), `protocol`
#     (TCP), and `listenAddress` (127.0.0.1) untouched. The Envoy proxy NodePort
#     and the Gateway listener port are intentionally unaffected — only the
#     host-side binding moves to a non-privileged port.
#   - WITH_REGISTRY_CACHE: append a `containerdConfigPatches` entry that sets the
#     CRI registry `config_path` to /etc/containerd/certs.d, the ONLY way
#     containerd resolves an unmodified `ghcr.io/…` ref to a mirror. This must be
#     set at cluster-creation time (containerd reads config.toml at startup);
#     wire_node_registry_mirror then drops the per-host hosts.toml files, which
#     containerd re-reads per pull. Keeping the patch out of the checked-in file
#     and only in this rendered tempfile is what guarantees CI is unaffected.
#
# Arguments:
#   $1 — destination path for the rendered config
# Errors:
#   - exits 1 if KIND_HOST_PORT is not a positive integer in [1, 65535]
#   - exits 1 if `yq` is required (either transform) but not on PATH
# ---------------------------------------------------------------------------
render_kind_config() {
  local out_path="$1"
  local src="${SCRIPT_DIR}/kind-config.yaml"

  if [[ ! "${KIND_HOST_PORT}" =~ ^[0-9]+$ ]] \
    || (( KIND_HOST_PORT < 1 || KIND_HOST_PORT > 65535 )); then
    log "ERROR: KIND_HOST_PORT='${KIND_HOST_PORT}' is not a valid TCP port (1-65535)."
    exit 1
  fi

  local need_port=false need_cache=false
  [[ "${KIND_HOST_PORT}" != "443" ]] && need_port=true
  [[ "${WITH_REGISTRY_CACHE}" == "true" ]] && need_cache=true

  if [[ "${need_port}" == "false" && "${need_cache}" == "false" ]]; then
    cp "${src}" "${out_path}"
    return 0
  fi

  if ! command -v yq >/dev/null 2>&1; then
    log "ERROR: rendering the kind config requires 'yq' on PATH (KIND_HOST_PORT override and/or WITH_REGISTRY_CACHE=true)."
    exit 1
  fi

  cp "${src}" "${out_path}"

  if [[ "${need_port}" == "true" ]]; then
    # Select-and-mutate the single hostPort=443 entry; idempotent if the input
    # already uses the override port (yq's `select(... == 443)` matches nothing
    # and the document passes through unchanged).
    KIND_HOST_PORT="${KIND_HOST_PORT}" yq -i \
      '(.nodes[0].extraPortMappings[] | select(.hostPort == 443)).hostPort = (env(KIND_HOST_PORT) | tonumber)' \
      "${out_path}"
  fi

  if [[ "${need_cache}" == "true" ]]; then
    # Append (never replace) so a hypothetical pre-existing patch survives.
    local containerd_patch
    containerd_patch=$'[plugins."io.containerd.grpc.v1.cri".registry]\n  config_path = "/etc/containerd/certs.d"'
    CONTAINERD_PATCH="${containerd_patch}" yq -i \
      '.containerdConfigPatches = ((.containerdConfigPatches // []) + [strenv(CONTAINERD_PATCH)])' \
      "${out_path}"
  fi
}

# render_controlplane_replicas rewrites the ControlPlane backing-service knobs —
# spec.infrastructure.{database,cache}.replicas and database.storageSize — of the
# CR(s) named ${CONTROLPLANE_NAME} in the given manifest file, from
# CONTROLPLANE_DB_REPLICAS / CONTROLPLANE_CACHE_REPLICAS / CONTROLPLANE_DB_STORAGE.
# The values are validated first so an invalid footprint is rejected here, before
# `kubectl apply`, rather than by the c5c3 validating webhook after the CR is sent:
# the replica counts must be a positive integer, the database count may not be 2 (a
# two-node Galera cluster cannot hold a quorum — the webhook rejects it), and the
# storage size must match the CRD quantity pattern (digits + Mi/Gi/Ti). Returns
# non-zero on an invalid footprint instead of exiting, so the caller decides how to
# handle it; main() runs under `set -e`, so the bare call there still fails the
# deploy fast. Name-scoped to the CR we just (possibly) renamed so a growing overlay
# is not silently rewritten; tonumber keeps the replica values integers (the CRD
# schema types replicas as integer) while storageSize stays a string.
# Extracted from main() so it is unit-testable
# (tests/unit/hack/deploy_infra_controlplane_replicas_test.sh).
render_controlplane_replicas() {
  local manifest="$1"

  local knob val
  for knob in CONTROLPLANE_DB_REPLICAS CONTROLPLANE_CACHE_REPLICAS; do
    val="${!knob}"
    if [[ ! "${val}" =~ ^[0-9]+$ ]] || (( val < 1 )); then
      log "ERROR: ${knob}='${val}' is not a positive integer."
      return 1
    fi
  done
  if (( CONTROLPLANE_DB_REPLICAS == 2 )); then
    log "ERROR: CONTROLPLANE_DB_REPLICAS=2 is rejected (Galera needs a quorum); use 1 (standalone) or >=3."
    return 1
  fi
  # Mirror the CRD's storageSize pattern (^[0-9]+(Mi|Gi|Ti)$) so a typo is caught
  # here rather than surfacing as an admission error from the c5c3 webhook. Keep
  # this regex in lockstep with the +kubebuilder:validation:Pattern marker on
  # DatabaseSpec.StorageSize (internal/common/types/types.go) — the CRD field
  # CONTROLPLANE_DB_STORAGE is projected into; if that pattern changes, change
  # this one too.
  if [[ ! "${CONTROLPLANE_DB_STORAGE}" =~ ^[0-9]+(Mi|Gi|Ti)$ ]]; then
    log "ERROR: CONTROLPLANE_DB_STORAGE='${CONTROLPLANE_DB_STORAGE}' is not a valid quantity (expected digits + Mi/Gi/Ti, e.g. 512Mi)."
    return 1
  fi

  # `with(paths; update)` binds the select clause once and runs all three
  # assignments against each matched node, so the CR-scoping predicate is not
  # repeated per field (and cannot drift between them). tonumber keeps the
  # replica values integers (the CRD schema types replicas as integer) while
  # storageSize stays a string. A select that matches nothing is a no-op, so the
  # rewrite stays idempotent on an already-scaled or unrelated overlay.
  CONTROLPLANE_DB_REPLICAS="${CONTROLPLANE_DB_REPLICAS}" CONTROLPLANE_CACHE_REPLICAS="${CONTROLPLANE_CACHE_REPLICAS}" CONTROLPLANE_DB_STORAGE="${CONTROLPLANE_DB_STORAGE}" CONTROLPLANE_NAME="${CONTROLPLANE_NAME}" yq -i \
    'with(select(.kind == "ControlPlane" and .metadata.name == strenv(CONTROLPLANE_NAME));
       .spec.infrastructure.database.replicas = (strenv(CONTROLPLANE_DB_REPLICAS) | tonumber)
       | .spec.infrastructure.cache.replicas = (strenv(CONTROLPLANE_CACHE_REPLICAS) | tonumber)
       | .spec.infrastructure.database.storageSize = strenv(CONTROLPLANE_DB_STORAGE))' \
    "${manifest}"
}

# ---------------------------------------------------------------------------
# cap_node_nofile — Cap RLIMIT_NOFILE on every kind node's containerd.
#
# Docker Desktop ships the containerd service with LimitNOFILE=infinity, so pods
# inherit an ~1e9 open-file limit even though the node container itself is capped
# at 1048576. uWSGI (the Keystone API server) allocates a structure proportional
# to the fd count when a worker loads the app, so it immediately tries to
# allocate multiple GiB and is OOM-killed within seconds — regardless of the
# container memory limit (raising it to 512Mi/1Gi/2Gi makes no difference). See
# #546.
#
# We write a systemd drop-in that lowers containerd's LimitNOFILE to
# NODE_NOFILE_LIMIT and restart containerd. This must run BEFORE any workload is
# scheduled: a containerd restart does not change the limit of already-running
# containers, so only pods created afterwards inherit the sane value (the target
# is the fresh-deploy path). Idempotent — re-writing the same drop-in and
# restarting again is a no-op — and best-effort per node: a node that cannot be
# patched logs a warning but does not abort the deploy.
#
# No-op when NODE_NOFILE_LIMIT is empty (opt out on a runtime that already ships
# a sane limit).
# ---------------------------------------------------------------------------
cap_node_nofile() {
  if [[ -z "${NODE_NOFILE_LIMIT}" ]]; then
    log "NODE_NOFILE_LIMIT is empty — skipping containerd RLIMIT_NOFILE cap."
    return 0
  fi

  local nodes
  nodes=$(kind get nodes --name "${CLUSTER_NAME}" 2>/dev/null) || true
  if [[ -z "${nodes}" ]]; then
    log "WARNING: no kind nodes found for cluster '${CLUSTER_NAME}' — skipping RLIMIT_NOFILE cap."
    return 0
  fi

  log "Capping containerd RLIMIT_NOFILE to ${NODE_NOFILE_LIMIT} on kind node(s): ${nodes//$'\n'/ }"

  local node
  for node in ${nodes}; do
    if docker exec "${node}" sh -c '
      set -e
      mkdir -p /etc/systemd/system/containerd.service.d
      printf "[Service]\nLimitNOFILE='"${NODE_NOFILE_LIMIT}"'\n" \
        > /etc/systemd/system/containerd.service.d/nofile.conf
      systemctl daemon-reload
      systemctl restart containerd
    ' >/dev/null 2>&1; then
      log "  ${node}: containerd RLIMIT_NOFILE set to ${NODE_NOFILE_LIMIT}."
    else
      log "  WARNING: failed to cap RLIMIT_NOFILE on ${node} — uWSGI workloads (Keystone) may OOM-crashloop."
    fi
  done

  # containerd was just restarted; give the node a moment to re-register with the
  # API server before Step 2 starts issuing kubectl apply against it.
  wait_for_node_ready "${POD_TIMEOUT}"
}

# ---------------------------------------------------------------------------
# wait_for_node_ready — Wait until every kind node reports Ready=True.
#
# Called after cap_node_nofile restarts containerd, so the subsequent kubectl
# apply steps do not race a briefly-NotReady kubelet. Polls every 5s up to the
# supplied timeout; on timeout it logs the node status and returns 0 anyway. The
# wait is best-effort — like cap_node_nofile itself, it must never abort the
# deploy, so callers do not check its status.
# ---------------------------------------------------------------------------
wait_for_node_ready() {
  local timeout="$1"
  local deadline=$(( $(date +%s) + timeout ))

  log "Waiting up to ${timeout}s for all nodes to be Ready..."

  while true; do
    local not_ready
    not_ready=$(kubectl get nodes -o json 2>/dev/null \
      | jq -r '[.items[] | select((.status.conditions[]? | select(.type == "Ready") | .status) != "True")] | length' 2>/dev/null) || true

    if [[ "${not_ready}" == "0" ]]; then
      log "All nodes are Ready."
      return 0
    fi

    if [[ $(date +%s) -ge ${deadline} ]]; then
      log "WARNING: not all nodes Ready after ${timeout}s; continuing anyway."
      kubectl get nodes 2>/dev/null || true
      return 0
    fi

    sleep 5
  done
}

# ---------------------------------------------------------------------------
# start_registry_cache — Bring up one distribution-registry pull-through proxy
# per upstream on the kind Docker network, each backed by a persistent named
# volume.
#
# The registry runs in proxy mode via a single env var
# (REGISTRY_PROXY_REMOTEURL=<upstream>); its default config already serves on
# :5000 with filesystem storage at /var/lib/registry, so no config file is
# rendered or mounted. On a cache miss the proxy streams the blob from the
# upstream to containerd while writing it to the volume, so even a cold pull runs
# at ~origin speed. containerd sends the proxy the BARE repository path (no
# registry host); because each proxy fronts exactly one upstream the mapping is
# unambiguous — `library/nginx` cannot be confused with `c5c3/keystone`.
#
# Idempotent and reused across cluster recreates: a proxy that is already running
# is left untouched (only re-attached to the network if needed); a stale/exited
# one is removed and recreated. The containers and volumes carry the
# `forge.registry-cache=true` label so teardown-infra.sh's PURGE_REGISTRY_CACHE
# path can find and remove them without knowing the upstream set. Names are NOT
# scoped by CLUSTER_NAME so a single warm cache serves every kind cluster.
#
# Best-effort: a failure to start any single proxy logs a warning and continues —
# the mirror entries advertise capabilities ["pull","resolve"], so containerd
# falls back to the origin and the deploy still succeeds (just uncached).
#
# No-op unless WITH_REGISTRY_CACHE=true (the caller gates it, but guard here too
# so the function is safe to call directly).
# ---------------------------------------------------------------------------
start_registry_cache() {
  if [[ "${WITH_REGISTRY_CACHE}" != "true" ]]; then
    return 0
  fi

  # The `kind` network is created by `kind create cluster`; this runs after
  # Step 1, so it should exist. If it does not, the proxy containers would be
  # unreachable from the nodes — warn and skip rather than abort the deploy.
  if ! docker network inspect "${KIND_DOCKER_NETWORK}" >/dev/null 2>&1; then
    log "WARNING: Docker network '${KIND_DOCKER_NETWORK}' not found — skipping registry cache (kind nodes could not reach it)."
    return 0
  fi

  log "Starting registry pull-through caches (image ${REGISTRY_CACHE_IMAGE}) on network '${KIND_DOCKER_NETWORK}'..."

  local entry host url suffix container volume
  for entry in "${REGISTRY_CACHE_UPSTREAMS[@]}"; do
    IFS='|' read -r host url suffix <<<"${entry}"
    container="registry-cache-${suffix}"
    volume="registry-cache-${suffix}-data"

    # Persistent storage volume (survives kind delete / recreate).
    if ! docker volume inspect "${volume}" >/dev/null 2>&1; then
      docker volume create --label forge.registry-cache=true "${volume}" >/dev/null 2>&1 \
        || log "  WARNING: failed to create volume ${volume}."
    fi

    # Already running → ensure it is on the kind network and move on. This is
    # the common reuse-across-recreate path.
    if [[ "$(docker inspect -f '{{.State.Running}}' "${container}" 2>/dev/null)" == "true" ]]; then
      docker network connect "${KIND_DOCKER_NETWORK}" "${container}" >/dev/null 2>&1 || true
      log "  ${container}: already running (${host} → ${url})."
      continue
    fi

    # Exists but stopped/exited → remove so we can recreate cleanly.
    docker rm -f "${container}" >/dev/null 2>&1 || true

    if docker run -d \
      --name "${container}" \
      --restart unless-stopped \
      --label forge.registry-cache=true \
      --network "${KIND_DOCKER_NETWORK}" \
      -e "REGISTRY_PROXY_REMOTEURL=${url}" \
      -v "${volume}:/var/lib/registry" \
      "${REGISTRY_CACHE_IMAGE}" >/dev/null 2>&1; then
      log "  ${container}: started (${host} → ${url})."
    else
      log "  WARNING: failed to start ${container} for ${host}; containerd will fall back to the origin."
    fi
  done
}

# ---------------------------------------------------------------------------
# wire_node_registry_mirror — Point every kind node's containerd at the registry
# caches by writing a hosts.toml per upstream under /etc/containerd/certs.d/.
#
# Same docker-exec mechanism as cap_node_nofile. The kind config already set
# `config_path = /etc/containerd/certs.d` (render_kind_config), so containerd
# picks these up per pull with no restart. Each file names the upstream `server`
# fallback and a single mirror `[host."http://<proxy>:5000"]` with capabilities
# ["pull","resolve"]; if the mirror is unreachable containerd falls straight
# through to `server`, so a down cache never hard-breaks a pull.
#
# Best-effort per node/host: a docker-exec failure warns but does not abort the
# deploy. No-op unless WITH_REGISTRY_CACHE=true.
# ---------------------------------------------------------------------------
wire_node_registry_mirror() {
  if [[ "${WITH_REGISTRY_CACHE}" != "true" ]]; then
    return 0
  fi

  local nodes
  nodes=$(kind get nodes --name "${CLUSTER_NAME}" 2>/dev/null) || true
  if [[ -z "${nodes}" ]]; then
    log "WARNING: no kind nodes found for cluster '${CLUSTER_NAME}' — skipping registry-mirror wiring."
    return 0
  fi

  log "Wiring containerd registry mirrors on kind node(s): ${nodes//$'\n'/ }"

  local node entry host url suffix container
  for node in ${nodes}; do
    for entry in "${REGISTRY_CACHE_UPSTREAMS[@]}"; do
      IFS='|' read -r host url suffix <<<"${entry}"
      container="registry-cache-${suffix}"
      # HOSTS_HOST/HOSTS_SERVER/HOSTS_MIRROR are passed through `docker exec env`
      # so the values are not interpolated by the node shell — only written into
      # the heredoc verbatim.
      if docker exec \
        -e HOSTS_HOST="${host}" \
        -e HOSTS_SERVER="${url}" \
        -e HOSTS_MIRROR="http://${container}:5000" \
        "${node}" sh -c '
          set -e
          dir="/etc/containerd/certs.d/${HOSTS_HOST}"
          mkdir -p "${dir}"
          cat > "${dir}/hosts.toml" <<EOF
server = "${HOSTS_SERVER}"

[host."${HOSTS_MIRROR}"]
  capabilities = ["pull", "resolve"]
EOF
        ' >/dev/null 2>&1; then
        log "  ${node}: ${host} → ${container}."
      else
        log "  WARNING: failed to write ${host} mirror on ${node}; that registry will pull from the origin."
      fi
    done
  done
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
  log "Node RLIMIT_NOFILE  : ${NODE_NOFILE_LIMIT:-<unset — skip cap>} (override via NODE_NOFILE_LIMIT)"
  log "Chaos Mesh         : ${WITH_CHAOS_MESH} (set WITH_CHAOS_MESH=true to install)"
  log "Prometheus stack    : ${WITH_PROMETHEUS} (set WITH_PROMETHEUS=true to install)"
  log "metrics-server      : ${WITH_METRICS_SERVER} (set WITH_METRICS_SERVER=true to install)"
  log "Registry cache      : ${WITH_REGISTRY_CACHE} (set WITH_REGISTRY_CACHE=true for a local pull-through cache; local-dev only)"
  log "ControlPlane stack  : ${WITH_CONTROLPLANE} (set WITH_CONTROLPLANE=true to provision infra via the c5c3 ControlPlane)"
  if [[ "${WITH_CONTROLPLANE}" == "true" ]]; then
    log "ControlPlane operators : ${CONTROLPLANE_OPERATORS} (flux = published chart + K-ORC Flux source; external = operators deployed out of band)"
    log "ControlPlane backing   : MariaDB replicas=${CONTROLPLANE_DB_REPLICAS} (>1 = Galera) storage=${CONTROLPLANE_DB_STORAGE}, Memcached replicas=${CONTROLPLANE_CACHE_REPLICAS} (override via CONTROLPLANE_DB_REPLICAS / CONTROLPLANE_DB_STORAGE / CONTROLPLANE_CACHE_REPLICAS)"
  fi
  log ""

  # Pre-flight checks
  preflight_checks

  # Load chaos-mesh kernel modules on the host before creating the cluster.
  # Kind nodes share the host kernel; NetworkChaos needs ipset/tc modules.
  # Gated on WITH_CHAOS_MESH so the default Quick Start does not require
  # passwordless sudo or modprobe access.
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
    # take effect without mutating the checked-in file. We do not
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

  # Cap containerd's RLIMIT_NOFILE on every node before any workload lands.
  # Runs on all three Step-1 paths (fresh create, pre-existing cluster,
  # SKIP_KIND_CREATE) because the fd limit is a property of the node's
  # containerd, not of how the cluster came to exist, and a uWSGI workload
  # (Keystone) scheduled later would otherwise OOM-crashloop. See #546.
  cap_node_nofile

  # Opt-in transparent registry pull-through cache (#564). Bring up the registry
  # proxies (now that the `kind` network exists) and wire every node's containerd
  # at them, before any image is pulled. Both are best-effort and no-op unless
  # WITH_REGISTRY_CACHE=true; the containerd config_path patch they rely on is
  # injected only into the rendered kind config (render_kind_config), so it takes
  # effect on a freshly created cluster. Both steps run BEFORE Step 2 so the very
  # first flux-operator / chart image pull can already hit the cache.
  if [[ "${WITH_REGISTRY_CACHE}" == "true" ]]; then
    start_registry_cache
    wire_node_registry_mirror
  else
    log "Skipping registry pull-through cache (WITH_REGISTRY_CACHE=false)."
  fi

  # Step 2: Install flux-operator and apply FluxInstance (/)
  #
  # Only the two bootstrap-scope manifests are applied here — the Namespace
  # resources and the FluxInstance CR. HelmRepository/HelmRelease objects from
  # deploy/flux-system/{sources,releases}/ intentionally come later (Step 3):
  # flux-operator's install.yaml only registers the fluxcd.controlplane.io
  # CRDs, and the source.toolkit.fluxcd.io / helm.toolkit.fluxcd.io CRDs
  # consumed by those objects are materialised only after the flux-operator
  # reconciles this FluxInstance. Applying them before wait_for_fluxinstance
  # would abort the script under `set -euo pipefail` with 'no matches for kind
  # "HelmRepository" in version "source.toolkit.fluxcd.io/v1"'.
  log "=== Step 2/8: Install flux-operator + apply FluxInstance ==="
  kubectl apply -f \
    "https://github.com/controlplaneio-fluxcd/flux-operator/releases/download/${FLUX_OPERATOR_VERSION}/install.yaml"
  kubectl apply -f "${REPO_ROOT}/deploy/flux-system/namespaces.yaml"
  kubectl apply -f "${REPO_ROOT}/deploy/flux-system/fluxinstance.yaml"
  wait_for_fluxinstance "${HELMRELEASE_TIMEOUT}"
  log "flux-operator installed and FluxInstance/flux is Ready."

  # Gateway API CRDs. Installed before the base kustomize overlay so
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
  # deploy/flux-system/{sources,releases}/ resolve to known Kinds.
  log "=== Step 3/8: Apply base kustomize overlay ==="
  kubectl apply -k "${REPO_ROOT}/deploy/kind/base"
  log "Base kustomize overlay applied."

  # Opt-in chaos-mesh overlay. Layered on top of the base so the
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

  # Opt-in kube-prometheus-stack overlay. Layered on top of
  # the base so the default Quick Start stays minimal; enable with
  # WITH_PROMETHEUS=true. The overlay is self-contained (no `../../` parent-dir
  # references), so kubectl's embedded kustomize renders it under the default
  # LoadRestrictionsRootOnly security check — same contract as the chaos-mesh
  # overlay (no `--load-restrictor` flag required, kubernetes/kubectl#948).
  #
  # The dashboard JSON copy step stages the Grafana
  # dashboard from operators/keystone/dashboards/ into the overlay root so
  # configMapGenerator can reference it without a parent-dir traversal. The
  # single source of truth lives at operators/keystone/dashboards/; the
  # destination is git-ignored as a build artifact, and the copy is idempotent
  # so `git status` after `make deploy-infra` shows no unexpected modifications.
  # The copy MUST run immediately before `kubectl apply -k` so the file exists
  # when kustomize renders the ConfigMap.
  if [[ "${WITH_PROMETHEUS}" == "true" ]]; then
    cp -f "${REPO_ROOT}/operators/keystone/dashboards/keystone-operator.json" "${REPO_ROOT}/deploy/kind/prometheus/keystone-operator.json"
    log "Dashboard JSON copied into deploy/kind/prometheus/ for kustomize configMapGenerator (WITH_PROMETHEUS=true)."
    kubectl apply -k "${REPO_ROOT}/deploy/kind/prometheus"
    log "Prometheus kind overlay applied (WITH_PROMETHEUS=true)."
  fi

  # Opt-in metrics-server overlay. Layered on top of the base so the default
  # Quick Start stays minimal; enable with WITH_METRICS_SERVER=true. The
  # overlay is self-contained (no `../../` parent-dir references), so kubectl's
  # embedded kustomize renders it under the default LoadRestrictionsRootOnly
  # security check — same contract as the chaos-mesh and prometheus overlays
  # (no `--load-restrictor` flag required, kubernetes/kubectl#948). It installs
  # the resource-metrics API the autoscaling recipe's HPA depends on.
  if [[ "${WITH_METRICS_SERVER}" == "true" ]]; then
    kubectl apply -k "${REPO_ROOT}/deploy/kind/metrics-server"
    log "metrics-server kind overlay applied (WITH_METRICS_SERVER=true)."
  fi

  # the c5c3 ControlPlane stack (c5c3-operator + image and the K-ORC
  # GitRepository/Kustomization) is published and valid, so it would reconcile —
  # but running the full chain is opt-in. WITH_CONTROLPLANE=true deploys it; the
  # default leaves it suspended so the bring-up stays light and the keystone E2E
  # path is unchanged.
  if [[ "${WITH_CONTROLPLANE}" == "true" && "${CONTROLPLANE_OPERATORS}" == "flux" ]]; then
    # Deploy the full ControlPlane stack via Flux from the published c5c3-operator
    # chart and the K-ORC GitRepository/Kustomization. The kind base overlay
    # suspends keystone-operator for the local-build E2E path; un-suspend it here
    # so the c5c3-operator HelmRelease's dependsOn is satisfied and the projected
    # Keystone CR can reconcile. c5c3-operator, k-orc, and the c5c3-charts /
    # k-orc sources are left un-suspended (the base applied them active).
    log "WITH_CONTROLPLANE=true: deploying the c5c3 ControlPlane stack (keystone-operator, horizon-operator, k-orc, c5c3-operator)."
    kubectl patch helmrelease keystone-operator -n keystone-system \
      --type merge -p '{"spec":{"suspend":false}}' 2>/dev/null || true
    kubectl patch helmrelease horizon-operator -n horizon-system \
      --type merge -p '{"spec":{"suspend":false}}' 2>/dev/null || true
    # Pin the GHCR :latest operator images to their current digest so a
    # feature merged since the last deploy actually rolls out (the tag is
    # mutable; the digest is resolved now and injected via the per-operator
    # image-digest ConfigMaps that the HelmReleases consume via valuesFrom).
    # Best-effort: on failure the releases fall back to tag-only resolution,
    # exactly the pre-digest behaviour.
    "${SCRIPT_DIR}/refresh-operator-image-digests.sh" \
      || log "WARNING: operator image digest refresh failed (best-effort); HelmReleases will resolve :latest by tag."
  elif [[ "${WITH_CONTROLPLANE}" == "true" ]]; then
    # CONTROLPLANE_OPERATORS=external: the keystone-operator, K-ORC, and
    # c5c3-operator are deployed out of band (e.g. the e2e-controlplane CI job
    # uses local dev images). Suspend the Flux ControlPlane stack — including
    # keystone-operator, which stays suspended so the base HelmRelease does not
    # fight the dev image deployed via hack/ci-deploy-operator.sh — and let the
    # rest of this run only prepare the shared prerequisites (TLS, OpenBao +
    # per-CR seeding, ESO store).
    log "WITH_CONTROLPLANE=true: ControlPlane operator stack provided externally (dev images); suspending the Flux stack."
    kubectl patch helmrelease c5c3-operator -n c5c3-system \
      --type merge -p '{"spec":{"suspend":true}}' 2>/dev/null || true
    kubectl patch kustomization k-orc -n flux-system \
      --type merge -p '{"spec":{"suspend":true}}' 2>/dev/null || true
    kubectl patch helmrepository c5c3-charts -n flux-system \
      --type merge -p '{"spec":{"suspend":true}}' 2>/dev/null || true
    kubectl patch gitrepository k-orc -n flux-system \
      --type merge -p '{"spec":{"suspend":true}}' 2>/dev/null || true
  else
    # Suspend the stack (best-effort, not awaited — see helm_releases below) so it
    # does not add idle reconcile churn competing with external-secrets /
    # kube-prometheus-stack for the controllers.
    log "Suspending the ControlPlane stack (c5c3-operator, k-orc); set WITH_CONTROLPLANE=true to deploy it."
    kubectl patch helmrelease c5c3-operator -n c5c3-system \
      --type merge -p '{"spec":{"suspend":true}}' 2>/dev/null || true
    kubectl patch kustomization k-orc -n flux-system \
      --type merge -p '{"spec":{"suspend":true}}' 2>/dev/null || true
    kubectl patch helmrepository c5c3-charts -n flux-system \
      --type merge -p '{"spec":{"suspend":true}}' 2>/dev/null || true
    kubectl patch gitrepository k-orc -n flux-system \
      --type merge -p '{"spec":{"suspend":true}}' 2>/dev/null || true
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

  # Phase 2: Apply TLS prerequisites that OpenBao and MariaDB need to start.
  # The openbao-tls Certificate creates the Secret mounted by the OpenBao
  # StatefulSet. The db-ca-issuer manifest creates the OpenStack DB CA
  # keypair Secret and the "openstack-db-ca-issuer" ClusterIssuer that
  # MariaDB/MaxScale and the Keystone DB-client mTLS path consume
  # The openbao-ca-issuer manifest creates the
  # OpenBao trust-domain CA keypair Secret and the "openbao-ca-issuer"
  # ClusterIssuer that signs openbao-tls and all openbao client certs
  # These resources are also part of the infrastructure
  # kustomization (applied in Step 5), but OpenBao and MariaDB cannot
  # become Ready until they exist.
  # Order matters:
  #   - selfsigned-cluster-issuer (cluster-issuer.yaml) must exist before
  #     either CA-issuer manifest (their CA Certificates are signed by it).
  #   - openbao-ca-issuer must exist before openbao-tls-cert.yaml (which
  #     references it via issuerRef).
  log "Phase 2: Applying TLS prerequisites (ClusterIssuer + OpenBao CA + OpenBao TLS Certificate + DB CA Issuer)..."
  kubectl apply -f "${REPO_ROOT}/deploy/flux-system/infrastructure/cluster-issuer.yaml"
  kubectl apply -f "${REPO_ROOT}/deploy/flux-system/infrastructure/openbao-ca-issuer.yaml"
  kubectl apply -f "${REPO_ROOT}/deploy/flux-system/infrastructure/openbao-tls-cert.yaml"
  kubectl apply -f "${REPO_ROOT}/deploy/flux-system/infrastructure/db-ca-issuer.yaml"

  # Phase 3: Wait for remaining HelmReleases now that OpenBao can mount its TLS secret.
  # envoy-gateway is kind-only (deploy/kind/base/envoy-gateway.yaml) and provides
  # the GatewayClass consumed by Gateway/openstack-gw; gating it here ensures
  # the wait_for_gateway_programmed poll below finds a reconciling controller
  log "Phase 3: Waiting for remaining HelmReleases..."
  # Build the release list dynamically so chaos-mesh is only awaited when the
  # opt-in overlay was applied. The surviving non-chaos order is
  # preserved exactly as before; chaos-mesh is appended last to avoid moving
  # any other release's relative position.
  local helm_releases=(prometheus-operator-crds openbao mariadb-operator-crds mariadb-operator external-secrets memcached-operator envoy-gateway)
  if [[ "${WITH_CHAOS_MESH}" == "true" ]]; then
    helm_releases+=(chaos-mesh)
  fi
  # kube-prometheus-stack is appended last so the relative
  # ordering of the seven base releases (and chaos-mesh) is preserved exactly.
  local release_wait_timeout="${HELMRELEASE_TIMEOUT}"
  if [[ "${WITH_PROMETHEUS}" == "true" ]]; then
    helm_releases+=(kube-prometheus-stack)
    # The monitoring stack is heavy (prometheus + grafana + alertmanager +
    # operator); on a loaded 4-vCPU runner it was still Progressing at the 600s
    # mark. Give the wait more headroom, but never shorten a caller-pinned
    # HELMRELEASE_TIMEOUT that is already larger..
    if [[ "${release_wait_timeout}" -lt 1200 ]]; then
      release_wait_timeout=1200
    fi
  fi
  # metrics-server is appended last (after chaos-mesh and kube-prometheus-stack)
  # so the relative ordering of the seven base releases is preserved exactly.
  if [[ "${WITH_METRICS_SERVER}" == "true" ]]; then
    helm_releases+=(metrics-server)
  fi
  wait_for_helmreleases "${release_wait_timeout}" "${helm_releases[@]}"

  # with kube-prometheus-stack Ready, flip both operator charts'
  # monitoring.serviceMonitor.enabled to true so Prometheus picks up the
  # metrics targets. Runs only when WITH_PROMETHEUS=true to keep the default
  # Quick Start free of monitoring-coreos-com CRD lookups. The horizon-operator
  # HelmRelease exists on every path: suspended on the default kind base
  # (durable-but-inert patch + skipped wait) and un-suspended under
  # WITH_CONTROLPLANE (patch + Ready wait), which is what makes the horizon
  # metrics guide's kind tip true.
  if [[ "${WITH_PROMETHEUS}" == "true" ]]; then
    enable_operator_servicemonitor keystone-operator keystone-system "${HELMRELEASE_TIMEOUT}"
    enable_operator_servicemonitor horizon-operator horizon-system "${HELMRELEASE_TIMEOUT}"
  fi

  # Step 5: Apply infrastructure kustomize overlay (CRD-dependent resources)
  log "=== Step 5/8: Apply infrastructure kustomize overlay ==="

  # Wait for operator CRDs to be registered before applying CRD-dependent
  # resources. HelmRelease Ready does not guarantee CRDs are available in
  # the API server — the operator pods may still be starting.
  # envoyproxies.gateway.envoyproxy.io is installed by the envoy-gateway
  # HelmRelease (Phase 3 above) and is required by the EnvoyProxy CR in
  # deploy/kind/infrastructure/envoy-nodeport.yaml.
  wait_for_crds "${POD_TIMEOUT}" \
    memcacheds.memcached.c5c3.io \
    clustersecretstores.external-secrets.io \
    externalsecrets.external-secrets.io \
    mariadbs.k8s.mariadb.com \
    envoyproxies.gateway.envoyproxy.io

  # Invalidate kubectl's client-side discovery cache so that the newly
  # registered CRDs are visible to kubectl apply.
  kubectl api-resources > /dev/null 2>&1 || true
  if [[ "${WITH_CONTROLPLANE}" == "true" ]]; then
    # The c5c3 ControlPlane provisions MariaDB/Memcached itself (managed mode), so
    # render the infrastructure overlay and drop those two CRs before applying —
    # the TLS issuers, OpenBao certs, and Gateway certs are still required.
    #
    # Also drop the three standalone-shim ExternalSecrets (keystone-admin,
    # keystone-db, mariadb-root-password). They are pinned to the DEFAULT
    # identity's OpenBao paths (openstack/controlplane), but WITH_CONTROLPLANE
    # seeds per-CR paths for openstack/${CONTROLPLANE_NAME} instead (see the
    # KORC_CONTROLPLANES export below) — so with a non-default CONTROLPLANE_NAME
    # the shims have no seeded source and would sit in SecretSyncedError forever.
    # The ControlPlane path does not use them anyway: the c5c3 operator projects
    # per-ControlPlane credential ExternalSecrets and the ControlPlane provisions
    # its own MariaDB root password. Step 8's shim wait is skipped to match.
    kubectl kustomize "${REPO_ROOT}/deploy/kind/infrastructure" \
      | yq eval 'select(.kind != "MariaDB" and .kind != "Memcached" and (.kind != "ExternalSecret" or (.metadata.name != "keystone-admin" and .metadata.name != "keystone-db" and .metadata.name != "mariadb-root-password")))' - \
      | kubectl apply -f -
    log "Infrastructure overlay applied WITHOUT MariaDB/Memcached and the standalone-shim ExternalSecrets (WITH_CONTROLPLANE=true; the ControlPlane provisions them)."
  else
    kubectl apply -k "${REPO_ROOT}/deploy/kind/infrastructure"
    log "Infrastructure kustomize overlay applied."
  fi

  # Gateway/openstack-gw can only report Programmed=True after the
  # EnvoyProxy CR (applied via the infrastructure overlay above) binds
  # its parametersRef on GatewayClass/envoy — so this wait must run
  # AFTER Step 5, not between Phase 3 and Step 5. Downstream HTTPRoute
  # resources (operator-created from the Keystone CR's spec.gateway) need a
  # Programmed listener to bind to.
  wait_for_gateway_programmed openstack-gw openstack "${HELMRELEASE_TIMEOUT}"

  # Step 6: Wait for OpenBao pod to be Running (not Ready — it becomes Ready
  # only after init+unseal in Step 7).
  log "=== Step 6/8: Wait for OpenBao pods ==="
  wait_for_pods_running "${OPENBAO_NAMESPACE}" "app.kubernetes.io/name=openbao" "${POD_TIMEOUT}"

  # Step 7: OpenBao bootstrap (init, unseal, configure)
  log "=== Step 7/8: OpenBao bootstrap ==="
  # WITH_CONTROLPLANE: the bootstrap (write-bootstrap-secrets.sh, run inside
  # openbao_bootstrap below) seeds the per-ControlPlane Model B admin password on
  # per-CR OpenBao paths.
  #
  # DECISION the default deployment's ControlPlane identity is
  # "openstack/${CONTROLPLANE_NAME}". The ControlPlane CR always lives in the
  # "openstack" namespace (deploy/kind/controlplane/controlplane.yaml; there is no
  # CONTROLPLANE_NAMESPACE knob), and its name is CONTROLPLANE_NAME (default
  # "controlplane"). Export it as KORC_CONTROLPLANES so write-bootstrap-secrets.sh
  # seeds bootstrap/openstack/${CONTROLPLANE_NAME}-keystone/admin — the exact path
  # the keystone-operator Model B rotation PushSecret targets. KORC_CONTROLPLANES
  # must therefore track CONTROLPLANE_NAME. With the default CONTROLPLANE_NAME this
  # equals write-bootstrap-secrets.sh's built-in KORC_CONTROLPLANES default
  # ("openstack/controlplane"), so the canonical single-CR deploy path is unchanged.
  # Reviewer: please verify.
  # the K-ORC clouds.yaml is now seeded by the operator (reconcileKORC →
  # seedBootstrapCloudsYAML), which also derives the in-cluster auth_url itself, so
  # the shell stack no longer seeds it or exports a K-ORC auth_url override.
  # (The admin-password ExternalSecret is now operator-projected per-ControlPlane
  # (reconcileAdminPassword); the kind overlay shim
  # (deploy/kind/infrastructure/keystone-admin-externalsecret.yaml) pins the
  # default identity. A non-default CONTROLPLANE_NAME therefore does NOT seed that
  # shim's source path, so on the ControlPlane path the three standalone-shim
  # ExternalSecrets are dropped from the overlay apply and skipped in Step 8
  # rather than re-pointed — see the overlay-apply and Step 8 blocks below. The
  # K-ORC clouds.yaml ExternalSecret is likewise created per-CR by the operator
  # and needs no manifest edit.)
  if [[ "${WITH_CONTROLPLANE}" == "true" ]]; then
    export KORC_CONTROLPLANES="openstack/${CONTROLPLANE_NAME}"
  fi
  openbao_init_unseal
  openbao_bootstrap

  # Wait for OpenBao to become Ready after unseal
  wait_for_pods "${OPENBAO_NAMESPACE}" "app.kubernetes.io/name=openbao" "${POD_TIMEOUT}"

  # The ClusterSecretStore was applied in Step 5, before OpenBao was
  # initialised and unsealed (Step 7). ESO's first store validation therefore
  # failed against a down/sealed OpenBao and the controller entered an
  # exponential backoff that can outlast EXTERNALSECRET_TIMEOUT. Bump an
  # annotation to force an immediate re-validation now that OpenBao is up, wait
  # for the store to report Ready, then force-sync the dependent
  # ExternalSecrets so Step 8 does not race ESO's backoff. Same
  # apply-before-dependency reason as the MariaDB reconcile-trigger below.
  local now
  now=$(date +%s)
  log "Forcing ESO ClusterSecretStore re-validation..."
  kubectl annotate clustersecretstore/openbao-cluster-store \
    "deploy.c5c3.io/reconcile-trigger=${now}" --overwrite || true
  kubectl wait clustersecretstore/openbao-cluster-store \
    --for=condition=Ready --timeout="${POD_TIMEOUT}s" || true

  # The standalone-shim ExternalSecrets (keystone-admin, keystone-db,
  # mariadb-root-password) are only applied — and only have a seeded OpenBao
  # source — on the non-ControlPlane path. WITH_CONTROLPLANE drops them from the
  # overlay apply above and seeds per-CR paths for openstack/${CONTROLPLANE_NAME}
  # instead, so there is nothing to force-sync or wait for here; the
  # per-ControlPlane credential ExternalSecrets are projected and verified later
  # by the c5c3 operator and the chain E2E suite.
  if [[ "${WITH_CONTROLPLANE}" == "true" ]]; then
    log "=== Step 8/8: Skipping standalone ExternalSecret wait (WITH_CONTROLPLANE=true) ==="
  else
    log "Forcing standalone ExternalSecret re-sync..."
    for es in keystone-admin keystone-db mariadb-root-password; do
      kubectl annotate "externalsecret/${es}" -n openstack \
        "force-sync=${now}" --overwrite || true
    done

    # Step 8: Wait for ExternalSecrets to sync
    log "=== Step 8/8: Wait for ExternalSecrets ==="
    wait_for_externalsecrets "openstack" "${EXTERNALSECRET_TIMEOUT}" \
      keystone-admin keystone-db mariadb-root-password
  fi

  if [[ "${WITH_CONTROLPLANE}" != "true" ]]; then
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
  else
    # WITH_CONTROLPLANE: the shared MariaDB/Memcached are NOT created here — the
    # ControlPlane provisions them. Bring up the operator stack so a ControlPlane
    # CR can reconcile; whether that CR is applied here or by hand depends on
    # WITH_CONTROLPLANE_CR (default: by hand — see the ControlPlane Quick Start).
    log "=== WITH_CONTROLPLANE: bringing up the c5c3 ControlPlane stack ==="
    if [[ "${CONTROLPLANE_OPERATORS}" == "flux" ]]; then
      kubectl wait helmrelease/keystone-operator -n keystone-system \
        --for=condition=Ready --timeout="${HELMRELEASE_TIMEOUT}s" 2>/dev/null \
        || log "  keystone-operator not Ready yet (continuing; the ControlPlane tolerates it)."
      kubectl wait kustomization/k-orc -n flux-system \
        --for=condition=Ready --timeout="${HELMRELEASE_TIMEOUT}s" 2>/dev/null \
        || log "  k-orc Kustomization not Ready yet (continuing)."
      kubectl wait helmrelease/c5c3-operator -n c5c3-system \
        --for=condition=Ready --timeout="${HELMRELEASE_TIMEOUT}s" 2>/dev/null \
        || log "  c5c3-operator not Ready yet (continuing)."

      # The projected Keystone references ghcr.io/c5c3/keystone:<release>; preload it
      # so kind need not pull it in-cluster. Best-effort — the image is public on GHCR.
      local cp_release="2025.2"
      if docker pull "ghcr.io/c5c3/keystone:${cp_release}" >/dev/null 2>&1; then
        kind load docker-image "ghcr.io/c5c3/keystone:${cp_release}" --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
        log "  Preloaded ghcr.io/c5c3/keystone:${cp_release} into kind."
      fi
    else
      # CONTROLPLANE_OPERATORS=external: the Flux stack is suspended and the
      # operators + service/client images are provided by the caller (the
      # e2e-controlplane CI job deploys keystone-operator + c5c3-operator via
      # hack/ci-deploy-operator.sh, K-ORC via hack/ci-deploy-korc.sh, and loads
      # keystone:2025.2 / tempest:2025.2 into kind). Skip the Flux waits and the
      # published-image preload — the suspended releases would never report Ready.
      log "  ControlPlane operators provided externally (CONTROLPLANE_OPERATORS=external);"
      log "  skipping Flux HelmRelease/Kustomization waits and the published-image preload."
    fi

    if [[ "${WITH_CONTROLPLANE_CR}" == "true" ]]; then
      # Render the ControlPlane overlay; when KIND_HOST_PORT is overridden, inject the
      # host port into spec.services.keystone.publicEndpoint so Keystone advertises the
      # externally reachable URL. The checked-in CR omits publicEndpoint on purpose: at
      # the default port 443 the operator derives https://keystone.127-0-0-1.nip.io/v3
      # from the gateway hostname, so no rewrite is needed. This mirrors the
      # render_kind_config host-port discipline (yq is a hard dependency on this path).
      local cp_manifest
      cp_manifest="$(mktemp)"
      kubectl kustomize "${REPO_ROOT}/deploy/kind/controlplane" > "${cp_manifest}"
      # The bundled CR is named "controlplane"; honour a CONTROLPLANE_NAME override
      # so the applied CR matches the {name}-keystone auth_url seeded above. Scope by
      # the original name so a growing overlay is not blindly renamed.
      if [[ "${CONTROLPLANE_NAME}" != "controlplane" ]]; then
        CONTROLPLANE_NAME="${CONTROLPLANE_NAME}" yq -i \
          '(select(.kind == "ControlPlane" and .metadata.name == "controlplane") | .metadata.name) = strenv(CONTROLPLANE_NAME)' \
          "${cp_manifest}"
        log "  Renamed bundled ControlPlane CR to '${CONTROLPLANE_NAME}' (CONTROLPLANE_NAME override)."
      fi
      if [[ "${KIND_HOST_PORT}" != "443" ]]; then
        # Name-scope the rewrite to the CR we just (possibly) renamed so adding
        # further ControlPlanes to the overlay does not get silently rewritten
        # with this hostname/port.
        KIND_HOST_PORT="${KIND_HOST_PORT}" CONTROLPLANE_NAME="${CONTROLPLANE_NAME}" yq -i \
          '(select(.kind == "ControlPlane" and .metadata.name == strenv(CONTROLPLANE_NAME)) | .spec.services.keystone.publicEndpoint) = "https://keystone.127-0-0-1.nip.io:" + strenv(KIND_HOST_PORT) + "/v3"' \
          "${cp_manifest}"
        log "  Set ControlPlane publicEndpoint to https://keystone.127-0-0-1.nip.io:${KIND_HOST_PORT}/v3 (KIND_HOST_PORT override)."
      fi

      # Project the single-node footprint onto the bundled CR. The bundled CR
      # already carries replicas: 1 and storageSize: 512Mi for the backing services;
      # this makes the values deploy-time configurable (CONTROLPLANE_DB_REPLICAS /
      # CONTROLPLANE_CACHE_REPLICAS / CONTROLPLANE_DB_STORAGE) without editing the
      # tracked manifest.
      render_controlplane_replicas "${cp_manifest}"
      log "  Set ControlPlane backing-service footprint: MariaDB replicas=${CONTROLPLANE_DB_REPLICAS} (>1 = Galera) storage=${CONTROLPLANE_DB_STORAGE}, Memcached replicas=${CONTROLPLANE_CACHE_REPLICAS}."

      # Apply the ControlPlane CR. Retry briefly: the c5c3-operator validating webhook
      # may need a moment after the chart install before it accepts the CR.
      local cp_attempt
      for cp_attempt in 1 2 3 4 5; do
        if kubectl apply -f "${cp_manifest}" 2>/dev/null; then
          break
        fi
        log "  ControlPlane CR apply attempt ${cp_attempt} failed (webhook warming up?); retrying..."
        sleep 10
      done
      rm -f "${cp_manifest}"
      log "  ControlPlane CR applied (WITH_CONTROLPLANE_CR=true). Watch the chain with:"
      log "    kubectl get controlplane -n openstack -w"
      log "  It provisions MariaDB/Memcached, projects Keystone, mints the K-ORC admin"
      log "  credential, and registers the identity catalog (not awaited here)."

      # Onboard the OpenBao database-engine tenant so managed-mode Keystone can
      # draw engine-issued (Dynamic) DB credentials (#439). The ControlPlane CR
      # always lives in the openstack namespace; its MariaDB defaults to
      # openstack-db. Idempotent, so it is safe even if a downstream suite also
      # onboards. The e2e-controlplane CI job uses WITH_CONTROLPLANE_CR=false and
      # runs setup-database-tenant.sh from its own chainsaw suite instead.
      openbao_onboard_database_tenant "openstack" "${CONTROLPLANE_NAME}"
    else
      log "  Operator stack is up. The ControlPlane CR is NOT applied automatically —"
      log "  create and apply it yourself (see docs/quick-start-controlplane.md), e.g.:"
      log "    kubectl apply -f deploy/kind/controlplane/controlplane.yaml"
      log "  Name the CR '${CONTROLPLANE_NAME}' (CONTROLPLANE_NAME must match the applied"
      log "  CR name — the per-CR Model B admin-password bootstrap path and the projected"
      log "  ${CONTROLPLANE_NAME}-keystone Service both derive from it; set CONTROLPLANE_NAME"
      log "  to change it);"
      log "  on a KIND_HOST_PORT override set spec.services.keystone.publicEndpoint to"
      log "  the matching :<port> URL. Or re-run with WITH_CONTROLPLANE_CR=true to apply"
      log "  the bundled CR for you."
    fi
  fi

  log ""
  log "=========================================="
  log "  Infrastructure deployment complete!"
  log "=========================================="
  log "Cluster: ${CLUSTER_NAME}"
  log "To tear down: make teardown-infra"
}

# Run main only when executed directly so unit tests (tests/unit/hack/) can
# source this script and exercise individual functions (/).
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
