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
#
# Idempotent re-runs:
#   Re-running with the SAME parameters converges without errors and leaves a
#   healthy stack unchanged — steps either detect completed work and skip it
#   (cluster create, nofile cap, Gateway API CRDs, OpenBao init/bootstrap) or
#   apply their changes convergently (kubectl apply / upserts).
#   Re-running with ADDITIONAL opt-in flags (e.g. WITH_METRICS_SERVER=true,
#   WITH_PROMETHEUS=true) installs only the newly enabled components and leaves
#   the already-deployed ones untouched. Two flags are NOT additive:
#   WITH_REGISTRY_CACHE only takes effect on a cluster CREATED with it (the
#   mirror files stay inert otherwise — wire_node_registry_mirror warns), and
#   WITH_CONTROLPLANE on a provisioned standalone stack is a mode change (the
#   ControlPlane provisions its own MariaDB/Memcached), not an opt-in — start
#   from a fresh cluster for both.
#   Removing a previously enabled flag does NOT uninstall that run's components —
#   cleanup is hack/teardown-infra.sh's job.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
CLUSTER_NAME="${CLUSTER_NAME:-cobaltcore}"
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

# The kind config render_kind_config starts from. Defaults to the single
# control-plane-node hack/kind-config.yaml. The known alternative is
# hack/kind-config-multinode.yaml (one control plane plus two workers), for
# suites that need more than one schedulable node. A relative path resolves
# against the working directory of the `make deploy-infra` invocation, which is
# the repository root under `make`. The `:-` expansion means an explicitly empty
# KIND_CONFIG= falls back to the default rather than to an empty path.
#
# Two preconditions on any config passed here. Its control-plane node has to
# stay at nodes[0], because render_kind_config applies the KIND_HOST_PORT
# override to nodes[0] and nothing else — a config whose first node is a worker
# gets the wrong node's mapping rewritten, with no error. And it only takes
# effect on the run that creates the cluster; see warn_unused_kind_config for
# what happens otherwise.
KIND_CONFIG="${KIND_CONFIG:-${SCRIPT_DIR}/kind-config.yaml}"

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

# Escape hatch for check_relocated_infrastructure. The guard aborts on a cluster
# that still carries the pre-relocation OpenBao namespace or GarageCluster, and
# the documented way past it deletes them — which destroys openbao-init-keys (the
# root token and every unseal-key share) and the raft PVCs behind it before the
# replacement stack has been proven. Set ALLOW_PRE_RELOCATION=true to let the two
# stacks coexist for a migration window instead: deploy-infra then warns and
# continues, leaving the retired instances running so their secrets can be read
# out first. This DEFERS the split-brain the guard describes, it does not resolve
# it — delete the old stack once the new one serves.
ALLOW_PRE_RELOCATION="${ALLOW_PRE_RELOCATION:-false}"

# Gates the opt-in chaos-mesh kind overlay (deploy/kind/chaos-mesh) and the
# host-side kernel-module load. Defaults to false so the kind Quick Start
# stays minimal; set WITH_CHAOS_MESH=true to enable chaos-engineering tests
WITH_CHAOS_MESH="${WITH_CHAOS_MESH:-false}"

# Gates the host-side load of the kernel modules the OVN chassis suites need
# (openvswitch for the Open vSwitch datapath, geneve for the tunnels between
# chassis). No kind overlay sits behind this flag; the ovn-operator is deployed
# the way every operator is. Defaults to false so the kind Quick Start needs no
# sudo; set WITH_OVN_KERNEL_MODULES=true on a host that runs those suites.
WITH_OVN_KERNEL_MODULES="${WITH_OVN_KERNEL_MODULES:-false}"

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

# Gates the opt-in dizzy kind overlay (deploy/kind/dizzy) which installs
# VictoriaMetrics + Grafana for dizzy load/chaos runs. Defaults to false so the
# kind Quick Start stays minimal; set WITH_DIZZY=true to install.
WITH_DIZZY="${WITH_DIZZY:-false}"

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

# Marks this cluster as one that runs third-party infrastructure only: MariaDB,
# Memcached, OpenBao, ESO, cert-manager and the rest of the stack, but no CobaltCore
# operator pod. That is the target-cluster half of the two-cluster devstack — the
# operators live on the management cluster (hack/deploy-mgmt-cluster.sh) and
# project their children onto this one through a registered kubeconfig.
#
# The c5c3-operator HelmRelease is suspended and its Deployment scaled to zero on
# EVERY branch of the ControlPlane block below, including the WITH_CONTROLPLANE
# ones that would otherwise deploy it. The scale is what the suspend cannot do on
# a re-used cluster: a HelmRelease suspended after the chart installed leaves the
# Deployment running, and a second operator reconciling the CRs this cluster
# receives is exactly what the split exists to prevent. The service-operator
# HelmReleases are already suspended by the kind base overlay on the default
# path. Defaults to false so a single-cluster deploy is unchanged.
INFRA_ONLY="${INFRA_ONLY:-false}"

# Gateway API CRD release installed before the keystone-operator HelmRelease so
# the operator's HTTPRoute watch has a registered kind at startup.
# Keep aligned with sigs.k8s.io/gateway-api in the operator go.mod files (they
# all pin the same version): a CRD bundle OLDER than the compiled-in types
# still registers the kind, but the API server silently PRUNES the fields the
# bundle's schema does not know — v1.1.0 predates HTTPRoute
# spec.rules[].timeouts, so a rendered timeouts stanza vanished on apply.
# A live bundle whose version differs from this pin is upgraded in place by
# install_gateway_api_crds(), so a re-used cluster converges without a teardown.
# When bumping this, re-check the gwapi_crds presence list in
# install_gateway_api_crds(): it enumerates the standard-channel CRD names of
# this bundle, and a release that adds a CRD needs the new name added there —
# otherwise the presence check passes on the old set and skips the install.
GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-v1.6.1}"
GATEWAY_API_CRDS_URL="${GATEWAY_API_CRDS_URL:-https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml}"

# Envoy Gateway CRD release asset, applied right after the Gateway API bundle.
# The envoy-gateway HelmRelease (deploy/kind/base/envoy-gateway.yaml) runs with
# crds.enabled=false: the chart's bundled CRD copy carries the Gateway API
# EXPERIMENTAL channel, which the standard bundle's safe-upgrades
# ValidatingAdmissionPolicy refuses on top of GATEWAY_API_VERSION. The upstream
# gateway-crds-helm chart cannot deliver the gateway.envoyproxy.io group either
# (its release Secret exceeds the 1Mi limit), so this script owns the group
# through the release's plain CRD asset — see the CRD-ownership decision in
# deploy/kind/base/envoy-gateway.yaml. Keep the pin inside the chart's SemVer
# range there; Renovate bumps both through the same envoy-gateway group.
ENVOY_GATEWAY_VERSION="${ENVOY_GATEWAY_VERSION:-v1.9.0}"
ENVOY_GATEWAY_CRDS_URL="${ENVOY_GATEWAY_CRDS_URL:-https://github.com/envoyproxy/gateway/releases/download/${ENVOY_GATEWAY_VERSION}/envoy-gateway-crds.yaml}"

# flux-operator release applied in Step 2 before the FluxInstance CR is created
# Kept as a script-local constant so Renovate can bump it
# via renovate.json custom managers.
FLUX_OPERATOR_VERSION="v0.58.1"

# OpenBao init parameters (match deploy/openbao/bootstrap/init-unseal.sh)
KEY_SHARES=5
KEY_THRESHOLD=3
# OPENBAO_NAMESPACE is the namespace the OpenBao server runs in. The bootstrap
# scripts (deploy/openbao/bootstrap/) resolve the same OPENBAO_NAMESPACE env var
# in common.sh (same 'shared-services' default), so setting it once configures
# both layers — the export below propagates it to the child scripts. The generic
# NAMESPACE env var is deliberately NOT honored: chainsaw injects
# NAMESPACE=<ephemeral test namespace> into every e2e script step, which must
# not redirect where the bootstrap scripts exec their bao commands.
OPENBAO_NAMESPACE="${OPENBAO_NAMESPACE:-shared-services}"
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
#
# Each argument is either a bare HelmRelease name — the namespace is then
# resolved by an all-namespaces lookup — or an explicit 'namespace/name'.
# Qualify an entry whenever the same name can legitimately exist twice at once:
# ALLOW_PRE_RELOCATION=true does exactly that for 'openbao', which then runs in
# both the retired openbao-system and the new shared-services. A bare lookup
# returns BOTH namespaces, and every poll would query the two-line result as a
# single namespace, fail RFC-1123 validation, and wait out the full timeout
# before aborting a deploy that has already applied half the new stack. An
# unqualified name that resolves to more than one namespace therefore fails
# immediately, naming the namespaces, rather than deadlocking.
# ---------------------------------------------------------------------------
wait_for_helmreleases() {
  local timeout="$1"
  shift
  local releases=("$@")
  local deadline=$(( $(date +%s) + timeout ))

  log "Waiting up to ${timeout}s for HelmReleases to become Ready: ${releases[*]}"

  while true; do
    local all_ready=true

    for entry in "${releases[@]}"; do
      local release="${entry##*/}"
      local ns
      if [[ "${entry}" == */* ]]; then
        ns="${entry%/*}"
      else
        # Find the namespace for this HelmRelease
        ns=$(kubectl get helmrelease --all-namespaces -o json 2>/dev/null \
          | jq -r --arg name "${release}" '.items[] | select(.metadata.name == $name) | .metadata.namespace' 2>/dev/null) || true

        if [[ $(grep -c . <<<"${ns}" | tr -d ' ') -gt 1 ]]; then
          log "ERROR: HelmRelease '${release}' exists in more than one namespace:"
          log "         ${ns//$'\n'/, }"
          log "       Waiting on the bare name cannot pick one. Pass the release as"
          log "       'namespace/${release}', or delete the retired copy."
          exit 1
        fi
      fi

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
# reconcile_helmrepository_sources — Force a reconcile of every Flux chart
# source in flux-system by annotating with reconcile.fluxcd.io/requestedAt —
# the kubectl-only equivalent of `flux reconcile source helm` / `... source oci`.
#
# Both source kinds are covered. Most charts ride a HelmRepository, but a chart
# pinned by artifact digest rather than by version is an OCIRepository
# (openbao-operator), and it is hard-gated by wait_for_helmreleases below.
# Leaving it out would let a failed first registry fetch sit out the source's
# 1h interval while the release wait runs into its timeout.
#
# A no-op when no such sources exist (the for-loop body simply does not run).
# Each annotate failure is tolerated (`|| true`) so a transient API error on
# one source does not abort the whole bootstrap.
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
# install_gateway_api_crds — Register the Gateway API CRDs before Flux owns them.
#
# Purpose: the Gateway API CRDs must be registered before the keystone-operator
# Pod starts, so its HTTPRoute watch finds the gateway.networking.k8s.io/v1 kind
# at startup. Without them the operator logs 'no matches for kind HTTPRoute' and
# never becomes Ready.
#
# Skip rationale: on a re-run against an already-provisioned cluster the CRDs
# already exist and may since be field-managed by another owner — the
# envoy-gateway chart via Flux's helm-controller. Re-asserting them with
# `kubectl apply --server-side` then fails with a field-manager conflict.
# Presence of all ten standard-channel CRDs AT the pinned bundle version
# already fulfills this step's purpose, so that case is skipped.
#
# A live bundle OLDER than the pin is reconciled: the run falls through to the
# server-side apply below and upgrades the CRDs in place, so a re-used dev
# cluster converges on GATEWAY_API_VERSION instead of needing a teardown. That
# direction is the one that matters, because a bundle older than the compiled-in
# gateway-api types silently prunes the fields the operators render.
#
# Convergence is deliberately ONE-WAY. A live bundle NEWER than the pin — routine
# when switching between branches that pin different versions — is warned about
# and skipped, never applied over. Applying an older schema in place would make
# the API server prune every field the older bundle does not know from the live
# Gateways and HTTPRoutes, which is the same silent-pruning failure this function
# exists to prevent, only inflicted deliberately. Upstream refuses part of that
# downgrade itself: the v1.5.0+ bundles ship a safe-upgrades
# ValidatingAdmissionPolicy that denies CRD writes annotated below v1.5.0.
#
# A live set carrying NO bundle-version annotation cannot be compared, so it also
# stays a warn-and-skip.
#
# Presence is probed per CRD, so an INCOMPLETE live set (some names present,
# some missing) is distinguishable from a bare cluster instead of silently
# taking the install path. It still attempts the install — a live bundle whose
# present CRDs are content-identical co-owns cleanly — but names the split set
# up front so the conflict, if it comes, is already diagnosed.
#
# --force-conflicts is deliberately NOT used: surfacing (or avoiding) the SSA
# conflict is correct; forcibly stealing field ownership from helm-controller is
# not. A conflict is therefore terminal and NOT retried — only transient
# failures (bundle fetch, API server hiccup) consume the retry budget.
# ---------------------------------------------------------------------------
install_gateway_api_crds() {
  # Standard-channel CRD names shipped by the GATEWAY_API_VERSION bundle. This
  # list is hand-maintained against that pin (see its definition above) — a
  # release that adds a standard-channel CRD needs its name added here, or the
  # presence check below passes on the stale set and, for a live bundle that
  # carries no comparable version annotation, skips the install.
  # NOTE: the bundle also ships a safe-upgrades ValidatingAdmissionPolicy and
  # its Binding — those are not CRDs and must stay out of this list.
  local gwapi_crds=(
    backendtlspolicies.gateway.networking.k8s.io
    gatewayclasses.gateway.networking.k8s.io
    gateways.gateway.networking.k8s.io
    grpcroutes.gateway.networking.k8s.io
    httproutes.gateway.networking.k8s.io
    listenersets.gateway.networking.k8s.io
    referencegrants.gateway.networking.k8s.io
    tcproutes.gateway.networking.k8s.io
    tlsroutes.gateway.networking.k8s.io
    udproutes.gateway.networking.k8s.io
  )

  # Probe each name separately. `kubectl get crd a b c` exits non-zero as soon
  # as ONE name is NotFound, which would make a partially-present set (a live
  # bundle whose standard channel differs from this pin by one CRD) look exactly
  # like a bare cluster and send the run down the install path — where the
  # co-owned CRDs then fail on a field-manager conflict.
  local gwapi_present=()
  local gwapi_missing=()
  local crd
  for crd in "${gwapi_crds[@]}"; do
    if kubectl get crd "${crd}" &>/dev/null; then
      gwapi_present+=("${crd}")
    else
      gwapi_missing+=("${crd}")
    fi
  done

  if (( ${#gwapi_missing[@]} == 0 )); then
    local live_bundle_version
    live_bundle_version="$(kubectl get crd httproutes.gateway.networking.k8s.io \
      -o jsonpath='{.metadata.annotations.gateway\.networking\.k8s\.io/bundle-version}' \
      2>/dev/null || true)"
    if [[ -z "${live_bundle_version}" ]]; then
      log "Gateway API CRDs already present (live bundle unknown); skipping install — another owner (e.g. the envoy-gateway chart via helm-controller) may manage them now."
      log "  WARNING: the live CRDs carry no bundle-version annotation, so the installed"
      log "           Gateway API version cannot be verified against the requested"
      log "           ${GATEWAY_API_VERSION} — an arbitrarily divergent bundle is accepted here."
      log "           Recreate the cluster (\`make teardown-infra\`) to install ${GATEWAY_API_VERSION}."
      return 0
    fi
    if [[ "${live_bundle_version}" == "${GATEWAY_API_VERSION}" ]]; then
      log "Gateway API CRDs already present (live bundle ${live_bundle_version}); skipping install — another owner (e.g. the envoy-gateway chart via helm-controller) may manage them now."
      return 0
    fi
    # Order the two versions so only the upgrade direction converges. `sort -V`
    # ranks the v-prefixed release tags the bundle annotation carries; the older
    # of the pair sorts first, so the live bundle is newer exactly when the pin
    # comes first.
    local gwapi_older
    gwapi_older="$(printf '%s\n%s\n' "${live_bundle_version}" "${GATEWAY_API_VERSION}" | sort -V | head -n1)"
    if [[ "${gwapi_older}" == "${GATEWAY_API_VERSION}" ]]; then
      log "Gateway API CRDs already present (live bundle ${live_bundle_version}); skipping install — the live bundle is NEWER than the requested ${GATEWAY_API_VERSION}."
      log "  WARNING: this step never downgrades. Applying the older ${GATEWAY_API_VERSION} schema"
      log "           in place would make the API server prune every field it does not know"
      log "           from the live Gateways and HTTPRoutes, and the bundle's own safe-upgrades"
      log "           ValidatingAdmissionPolicy refuses part of that anyway."
      log "           Recreate the cluster (\`make teardown-infra\`) to install ${GATEWAY_API_VERSION}."
      return 0
    fi
    # Upgrade drift IS reconciled: fall through to the server-side apply so a
    # re-used cluster converges on the pin (see the docblock above).
    log "Gateway API CRDs present at bundle ${live_bundle_version}, older than the requested ${GATEWAY_API_VERSION};"
    log "  applying an in-place upgrade to ${GATEWAY_API_VERSION}. If another owner (e.g. the"
    log "  envoy-gateway chart via helm-controller) field-manages the bundle, the apply below"
    log "  fails on a field-manager conflict — see the ERROR path there."
  fi

  # server-side apply avoids the 'metadata.annotations: Too long' error that
  # client-side apply hits on the upstream CRD bundle.
  log "=== Installing Gateway API CRDs (${GATEWAY_API_VERSION}) ==="
  if (( ${#gwapi_present[@]} > 0 && ${#gwapi_missing[@]} > 0 )); then
    log "  WARNING: the cluster carries an INCOMPLETE Gateway API CRD set."
    log "           present: ${gwapi_present[*]}"
    log "           missing: ${gwapi_missing[*]}"
    log "           The present CRDs are likely field-managed by another owner (the"
    log "           envoy-gateway chart via helm-controller), so the bundle apply below"
    log "           may fail on a field-manager conflict — see the ERROR path there."
  fi
  local gwapi_attempts=3
  local gwapi_attempt=0
  local gwapi_delay=5
  local gwapi_output
  while (( gwapi_attempt < gwapi_attempts )); do
    gwapi_attempt=$((gwapi_attempt + 1))
    if gwapi_output="$(kubectl apply --server-side -f "${GATEWAY_API_CRDS_URL}" 2>&1)"; then
      printf '%s\n' "${gwapi_output}"
      break
    fi
    printf '%s\n' "${gwapi_output}"
    # A field-manager conflict is deterministic — another owner holds the
    # fields and --force-conflicts is deliberately not used (see the docblock),
    # so re-applying the identical bundle cannot succeed. Fail fast with the
    # real diagnosis instead of burning the backoff on a misleading
    # 'after N attempts' message. Everything else (bundle fetch, API server
    # hiccup) is transient and keeps its retries.
    if [[ "${gwapi_output}" == *"conflict"* ]]; then
      log "ERROR: Gateway API CRD apply hit a field-manager conflict — another owner"
      log "       (e.g. the envoy-gateway chart via helm-controller) already manages"
      log "       part of this bundle. Not retried: the conflict is deterministic."
      log "       Recreate the cluster (\`make teardown-infra && make deploy-infra\`) to"
      log "       install ${GATEWAY_API_VERSION} cleanly."
      exit 1
    fi
    # A downgrade the live bundle itself refuses. From v1.5.0 the standard bundle
    # ships a safe-upgrades ValidatingAdmissionPolicy that denies CRD writes
    # annotated below v1.5.0, and a live set whose status.storedVersions names a
    # version the requested bundle no longer serves is rejected the same way.
    # Neither message contains "conflict", so without this branch the deterministic
    # denial burns all three attempts and exits pointing at the download URL.
    # The version gate above catches this for a complete live set; an INCOMPLETE
    # one reaches the apply without a version comparison, which is how it still
    # happens.
    if [[ "${gwapi_output}" == *"prohibited by default"* || "${gwapi_output}" == *"storedVersions"* ]]; then
      log "ERROR: Gateway API CRD apply was REFUSED because it would downgrade the live"
      log "       bundle: the cluster carries a Gateway API newer than the requested"
      log "       ${GATEWAY_API_VERSION}, and neither its safe-upgrades admission policy nor"
      log "       its stored API versions allow going back. Not retried: the refusal is"
      log "       deterministic."
      log "       Recreate the cluster (\`make teardown-infra && make deploy-infra\`) to"
      log "       install ${GATEWAY_API_VERSION} cleanly."
      exit 1
    fi
    if (( gwapi_attempt >= gwapi_attempts )); then
      log "ERROR: Failed to install Gateway API CRDs after ${gwapi_attempts} attempts from ${GATEWAY_API_CRDS_URL}"
      exit 1
    fi
    log "  Gateway API CRD apply failed (attempt ${gwapi_attempt}/${gwapi_attempts}); retrying in ${gwapi_delay}s..."
    sleep "${gwapi_delay}"
  done
  log "Gateway API CRDs installed."
}

# ---------------------------------------------------------------------------
# install_envoy_gateway_crds — Register the gateway.envoyproxy.io CRDs.
#
# Purpose: the envoy-gateway HelmRelease runs with crds.enabled=false (see the
# CRD-ownership decision in deploy/kind/base/envoy-gateway.yaml), so nothing
# else registers the controller's own API group. The upstream release asset
# carries exactly the eight gateway.envoyproxy.io CRDs and none of the Gateway
# API group, so applying it cannot collide with the safe-upgrades
# ValidatingAdmissionPolicy the standard bundle installs.
#
# Skip rationale: these CRDs carry no version annotation comparable to the
# Gateway API bundle-version, so a complete live set cannot be reconciled
# against the pin and is skipped. On a cluster provisioned before the CRD
# ownership split the set is also field-managed by helm-controller (the chart
# installed it from its crds/ directory), where a re-assert would
# SSA-conflict. Bumping ENVOY_GATEWAY_VERSION therefore takes effect on a
# fresh cluster only (make teardown-infra).
# ---------------------------------------------------------------------------
install_envoy_gateway_crds() {
  # CRD names shipped by the ENVOY_GATEWAY_VERSION envoy-gateway-crds.yaml
  # asset. Hand-maintained against that pin, like the gwapi_crds list above: a
  # release that adds a CRD needs its name added here, or the presence check
  # below passes on the stale set and skips the install.
  local eg_crds=(
    backends.gateway.envoyproxy.io
    backendtrafficpolicies.gateway.envoyproxy.io
    clienttrafficpolicies.gateway.envoyproxy.io
    envoyextensionpolicies.gateway.envoyproxy.io
    envoypatchpolicies.gateway.envoyproxy.io
    envoyproxies.gateway.envoyproxy.io
    httproutefilters.gateway.envoyproxy.io
    securitypolicies.gateway.envoyproxy.io
  )

  # Probe each name separately for the same reason install_gateway_api_crds()
  # does: a partially-present set must stay distinguishable from a bare
  # cluster.
  local eg_present=()
  local eg_missing=()
  local crd
  for crd in "${eg_crds[@]}"; do
    if kubectl get crd "${crd}" &>/dev/null; then
      eg_present+=("${crd}")
    else
      eg_missing+=("${crd}")
    fi
  done

  if (( ${#eg_missing[@]} == 0 )); then
    log "Envoy Gateway CRDs already present; skipping install — they carry no"
    log "  comparable version annotation, so convergence on ${ENVOY_GATEWAY_VERSION} cannot"
    log "  be verified. Recreate the cluster (\`make teardown-infra\`) to re-install."
    return 0
  fi

  log "=== Installing Envoy Gateway CRDs (${ENVOY_GATEWAY_VERSION}) ==="
  if (( ${#eg_present[@]} > 0 && ${#eg_missing[@]} > 0 )); then
    log "  WARNING: the cluster carries an INCOMPLETE Envoy Gateway CRD set."
    log "           present: ${eg_present[*]}"
    log "           missing: ${eg_missing[*]}"
    log "           The present CRDs may be field-managed by another owner (a"
    log "           pre-split envoy-gateway chart install via helm-controller), so"
    log "           the apply below may fail on a field-manager conflict."
  fi
  local eg_attempts=3
  local eg_attempt=0
  local eg_delay=5
  local eg_output
  while (( eg_attempt < eg_attempts )); do
    eg_attempt=$((eg_attempt + 1))
    if eg_output="$(kubectl apply --server-side -f "${ENVOY_GATEWAY_CRDS_URL}" 2>&1)"; then
      printf '%s\n' "${eg_output}"
      break
    fi
    printf '%s\n' "${eg_output}"
    # Mirrors the gwapi apply above: a field-manager conflict is deterministic,
    # so fail fast instead of burning the retry budget on it.
    if [[ "${eg_output}" == *"conflict"* ]]; then
      log "ERROR: Envoy Gateway CRD apply hit a field-manager conflict — another owner"
      log "       (a pre-split envoy-gateway chart install via helm-controller) already"
      log "       manages part of this set. Not retried: the conflict is deterministic."
      log "       Recreate the cluster (\`make teardown-infra && make deploy-infra\`) to"
      log "       install ${ENVOY_GATEWAY_VERSION} cleanly."
      exit 1
    fi
    if (( eg_attempt >= eg_attempts )); then
      log "ERROR: Failed to install Envoy Gateway CRDs after ${eg_attempts} attempts from ${ENVOY_GATEWAY_CRDS_URL}"
      exit 1
    fi
    log "  Envoy Gateway CRD apply failed (attempt ${eg_attempt}/${eg_attempts}); retrying in ${eg_delay}s..."
    sleep "${eg_delay}"
  done
  log "Envoy Gateway CRDs installed."
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
# relocated_object_exists — Probe one retired object for
# check_relocated_infrastructure, treating only a definite miss as absence.
#
# Returns 0 when the object exists and 1 when the API server reports it — or,
# for a CRD that was never installed, its whole resource type — as missing. Any
# OTHER kubectl failure exits the script: a connection refused, an expired
# credential, an RBAC denial on `get namespaces`, or a discovery error is not
# evidence that the retired stack is gone, and silently reading it as such turns
# the guard into a no-op exactly when it is needed. That is not hypothetical
# here: the guard runs immediately after cap_node_nofile restarts containerd on
# every kind node — taking the static kube-apiserver pod with it — and the
# follow-up wait_for_node_ready is best-effort, logging a warning and returning 0
# on timeout. Both lookups would then fail with connection refused and wave a
# pre-relocation cluster straight through to Step 2.
#
# Arguments: the `kubectl get` arguments naming the object.
# ---------------------------------------------------------------------------
relocated_object_exists() {
  local out rc
  out="$(kubectl get "$@" 2>&1)"
  rc=$?

  if [[ ${rc} -eq 0 ]]; then
    return 0
  fi

  # The three shapes in which the API server gives a DEFINITE negative answer:
  # "not found" for an absent object, and — for the GarageCluster lookup on a
  # cluster whose CRDs Step 5 has not installed yet, the clean-cluster case —
  # either the client-side discovery miss ("doesn't have a resource type") or
  # its server-side counterpart ("could not find the requested resource").
  if grep -qiE "not found|doesn't have a resource type|could not find the requested resource" \
    <<<"${out}"; then
    return 1
  fi

  log "ERROR: cannot determine whether this cluster predates the shared-services"
  log "       relocation — 'kubectl get $*' failed:"
  log "         ${out}"
  log "       Refusing to continue: an unreadable cluster is not an empty one, and"
  log "       deploying on top of a pre-relocation stack leaves a second unsealed"
  log "       OpenBao serving every historical secret. Restore access and rerun."
  exit 1
}

# ---------------------------------------------------------------------------
# check_relocated_infrastructure — Refuse to run against a pre-relocation stack.
#
# The shared OpenBao cluster moved out of openbao-system, and the Garage object
# store out of openstack, into shared-services. Both keep their state in
# namespace-scoped StatefulSet PVCs, so the move does not carry the data over:
# this script brings up EMPTY volumes in shared-services and nothing prunes what
# stays behind. `kubectl apply -k` (Step 3/5) does not prune, and
# deploy/flux-system/fluxinstance.yaml declares no spec.sync, so Flux never
# reconciles-and-prunes the repo either.
#
# Left in place, the retired OpenBao namespace keeps a second, unsealed instance
# serving every historical secret on openbao.openbao-system.svc:8200, plus the
# plaintext root token and all unseal keys in Secret openbao-init-keys and the
# server key in openbao-tls — unowned, unrotated, and no longer watched by
# anyone, while ESO is repointed at the new empty instance. The retired
# GarageCluster likewise keeps serving the objects Glance's database still points
# at while Glance is repointed at empty buckets, so every image stays `active`
# and every download 404s. Neither degrades visibly, so fail loudly here and let
# the operator delete them explicitly.
#
# Runs after Step 1 (the cluster exists) and before Step 2 applies any manifest.
# On a fresh cluster every lookup below reports the object missing and the
# function is a no-op. ALLOW_PRE_RELOCATION=true downgrades the abort to a
# warning for a migration window in which both stacks run side by side.
# ---------------------------------------------------------------------------
check_relocated_infrastructure() {
  local stale=false

  if relocated_object_exists namespace openbao-system; then
    stale=true
    log "ERROR: namespace 'openbao-system' still exists. The shared OpenBao cluster"
    log "       moved to 'shared-services' and this is a clean-install change: the"
    log "       raft store does NOT move with it, and nothing prunes the old one."
    log "       The old namespace still holds a running, unsealed OpenBao plus the"
    log "       plaintext root token and unseal keys in Secret openbao-init-keys."
    log "       Delete it explicitly before rerunning:"
    log "         kubectl delete namespace openbao-system"
  fi

  if relocated_object_exists garagecluster garage -n openstack; then
    stale=true
    log "ERROR: GarageCluster 'garage' still exists in 'openstack'. The Garage object"
    log "       store moved to 'shared-services' and its buckets are re-created empty"
    log "       there. The old cluster keeps running on the objects Glance's database"
    log "       still references, so leaving it costs node storage and makes every"
    log "       pre-existing image report active while its download 404s."
    log "       Delete it explicitly before rerunning, then remove the PVCs its"
    log "       StatefulSet leaves behind — and ONLY those: this namespace also"
    log "       holds the MariaDB volume carrying the Keystone database."
    log "         kubectl delete garagecluster garage -n openstack"
    log "         kubectl get pvc -n openstack | grep garage   # delete only these"
  fi

  if [[ "${stale}" == "true" ]]; then
    if [[ "${ALLOW_PRE_RELOCATION}" == "true" ]]; then
      log "WARNING: ALLOW_PRE_RELOCATION=true — continuing against a pre-relocation"
      log "         cluster. Both stacks now run side by side, which is the"
      log "         split-brain described above; delete the retired one once its"
      log "         secrets have been read out."
      return 0
    fi
    log "Aborting: this cluster predates the shared-services relocation."
    log "          Read the retired instance's secrets out BEFORE deleting it —"
    log "          openbao-init-keys has no second copy. Set"
    log "          ALLOW_PRE_RELOCATION=true to run both stacks side by side for a"
    log "          migration window instead of deleting first."
    exit 1
  fi
}

# ---------------------------------------------------------------------------
# load_host_kernel_modules PURPOSE MODULE... — Ensure the given kernel modules
# on the host, best-effort.
#
# Kind nodes share the host kernel, so a module a workload needs inside the
# cluster has to be loaded outside it. Every runtime failure is logged and
# swallowed: the load is skipped on non-Linux and without root or passwordless
# sudo, and a failing modprobe or apt-get only warns. PURPOSE names the caller
# in those log lines. The one non-zero return is the argument-count guard,
# which reports a caller bug, not a host condition.
# ---------------------------------------------------------------------------
load_host_kernel_modules() {
  if [[ $# -lt 2 ]]; then
    log "ERROR: load_host_kernel_modules needs a purpose and at least one module."
    return 1
  fi

  local purpose="$1"
  local modules=("${@:2}")

  if [[ "$(uname -s)" != "Linux" ]]; then
    log "Non-Linux host — skipping kernel-module load (${purpose} runs in the Linux VM kernel)."
    return 0
  fi

  local sudo_cmd=()
  if [[ "$(id -u)" -ne 0 ]]; then
    if sudo -n true 2>/dev/null; then
      sudo_cmd=(sudo -n)
    else
      log "WARNING: not root and no passwordless sudo — skipping kernel-module load; ${purpose} may fail."
      return 0
    fi
  fi

  log "Loading kernel modules for ${purpose}: ${modules[*]}"

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
    log "WARNING: modules missing and apt-get unavailable — ${purpose} tests may fail: ${missing[*]}"
    return 0
  fi

  local kver extra_pkg
  kver="$(uname -r)"
  extra_pkg="linux-modules-extra-${kver}"
  log "Installing ${extra_pkg} to provide missing modules: ${missing[*]}"
  if ! "${sudo_cmd[@]}" apt-get update -qq; then
    log "WARNING: apt-get update failed — ${purpose} tests may fail."
    return 0
  fi
  if ! "${sudo_cmd[@]}" apt-get install -y -qq "${extra_pkg}"; then
    log "WARNING: apt-get install ${extra_pkg} failed — ${purpose} tests may fail."
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
    log "WARNING: kernel modules still missing after retry — ${purpose} tests may fail: ${still_missing[*]}"
  fi
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
  # ip_set_hash_ip is the on-disk module name for the ipset hash:ip type; loading
  # it is enough — chaos-mesh only needs hash:net in practice, which is provided
  # by the same linux-modules-extra package. Keep the list aligned with what
  # chaos-daemon actually invokes via ipset/tc.
  load_host_kernel_modules "chaos-mesh NetworkChaos" ip_set ip_set_hash_ip ip_set_hash_net xt_set sch_netem sch_tbf
}

# ---------------------------------------------------------------------------
# load_ovn_kernel_modules — Ensure the OVN chassis prerequisites on the host.
#
# ovn-controller programs the Open vSwitch datapath and terminates the Geneve
# tunnels on the node it runs on; both need their module in the host kernel.
# Best-effort, like every caller of load_host_kernel_modules.
# ---------------------------------------------------------------------------
load_ovn_kernel_modules() {
  load_host_kernel_modules "OVN chassis (Open vSwitch datapath and Geneve tunnels)" openvswitch geneve
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
# openbao_kube_exec_stdin — Like openbao_kube_exec but with stdin forwarding
# (-i), so secret material can be piped in instead of passed as an argument.
# An argument is readable in two places: kubectl encodes every element of the
# remote command as a repeated `command=` query parameter and the API server
# records that request URI in its audit log, and inside the container the
# expanded argument sits in /proc/<pid>/cmdline for as long as the call runs.
# ---------------------------------------------------------------------------
openbao_kube_exec_stdin() {
  kubectl exec -i -n "${OPENBAO_NAMESPACE}" openbao-0 -- \
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

  # `bao operator unseal` takes the share only from an argument or an
  # interactive terminal prompt, so the share goes to sys/unseal instead:
  # `key=-` tells `bao write` to read the value from stdin, keeping it out of
  # both argv copies openbao_kube_exec_stdin describes. printf is a bash
  # builtin, so the share never reaches a local argv either.
  local i
  for i in $(seq 0 $(( KEY_THRESHOLD - 1 ))); do
    local key
    key=$(echo "${init_output}" | jq -r ".unseal_keys_b64[${i}]")
    printf '%s' "${key}" | openbao_kube_exec_stdin bao write sys/unseal key=- > /dev/null
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
# setup-database-tenant.sh ALSO provisions the glance, placement, and barbican
# engine connection+role pairs (database/mariadb/creds/<service>-<ns>) when the
# ControlPlane declares those services on the shared managed database, so this
# single call onboards every service tenant.
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

  # Wait on the MariaDB in EVERY namespace setup-database-tenant.sh will read a
  # root credential from, not just the ControlPlane's. Each service leg of that
  # script resolves its own service namespace and hard-exits when that namespace's
  # MariaDB root Secret is missing, so a ControlPlane that places Keystone,
  # Glance, Placement, or Barbican in a namespace of its own provisions a SECOND
  # openstack-db there on an independent timeline. Waiting only on the
  # ControlPlane's namespace lets the Keystone leg succeed and the Glance leg
  # exit 1 — a partially applied onboarding, with the operator already primed to
  # flip Glance to Dynamic.
  #
  # The namespaces are resolved from the live CR with the same defaults the
  # operator projects (an unset namespace block means the ControlPlane's own) and
  # under the same conditions the script applies, so the wait set matches the read
  # set exactly. The Glance leg in particular is SKIPPED for a dedicated glance
  # database (Static-only, no engine role), which also has its own clusterRef name
  # — waiting for the shared one in that namespace would block until timeout.
  #
  # Seeded with the ControlPlane's own namespace (the default every unset service
  # namespace resolves to), which also keeps the dedup test below from expanding
  # an empty array under `set -u`.
  local svc_ns_list=("${cp_ns}")
  local ns candidates=()

  candidates+=("$(kubectl get controlplane "${cp_name}" -n "${cp_ns}" \
    -o 'jsonpath={.spec.services.keystone.namespace.name}' 2>/dev/null || true)")

  local svc
  for svc in glance placement barbican; do
    if [[ -n "$(kubectl get controlplane "${cp_name}" -n "${cp_ns}" \
        -o "jsonpath={.spec.services.${svc}}" 2>/dev/null || true)" &&
      -z "$(kubectl get controlplane "${cp_name}" -n "${cp_ns}" \
        -o "jsonpath={.spec.services.${svc}.dedicatedBackingServices.database}" 2>/dev/null || true)" ]]; then
      candidates+=("$(kubectl get controlplane "${cp_name}" -n "${cp_ns}" \
        -o "jsonpath={.spec.services.${svc}.namespace.name}" 2>/dev/null || true)")
    fi
  done

  for ns in "${candidates[@]}"; do
    [[ -z "${ns}" ]] && continue
    if [[ ! " ${svc_ns_list[*]} " == *" ${ns} "* ]]; then
      svc_ns_list+=("${ns}")
    fi
  done

  # The ControlPlane projects the MariaDB after it reconciles; wait for the CR to
  # appear before waiting on its Ready condition (kubectl wait errors on a
  # not-yet-existent resource).
  local i
  for ns in "${svc_ns_list[@]}"; do
    log "Waiting for MariaDB '${mariadb_name}' to be projected in namespace '${ns}'..."
    for i in $(seq 1 30); do
      if kubectl get "mariadb/${mariadb_name}" -n "${ns}" >/dev/null 2>&1; then
        break
      fi
      sleep 10
    done
    log "Waiting for MariaDB '${mariadb_name}' in namespace '${ns}' to become Ready..."
    if ! kubectl wait "mariadb/${mariadb_name}" -n "${ns}" \
      --for=condition=Ready --timeout="${POD_TIMEOUT}s"; then
      log "ERROR: MariaDB '${mariadb_name}' in namespace '${ns}' did not become Ready; cannot onboard the database-engine tenant."
      exit 1
    fi
  done

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
# warn_unused_kind_config REASON — Report that KIND_CONFIG had no effect.
#
# render_kind_config runs on exactly one of the three Step-1 branches, the one
# that creates the cluster. On the other two a non-default KIND_CONFIG changes
# nothing, while the startup banner still prints it. A developer who reruns
# `make deploy-infra` with KIND_CONFIG=hack/kind-config-multinode.yaml without
# tearing the single-node cluster down first would otherwise read the banner as
# confirmation and go looking for the missing second DaemonSet pod in the
# operator.
#
# The comparison resolves KIND_CONFIG first. The default is absolute
# (${SCRIPT_DIR}/kind-config.yaml) while the documented way to set the variable
# is a repository-relative path, so a raw string compare fires the warning on
# `KIND_CONFIG=hack/kind-config.yaml` — the default itself, named the way the
# docs name it. A warning on the default is the one thing that would teach a
# developer to skip it.
#
# Arguments:
#   $1 — why the config is being ignored, spliced into the warning
# ---------------------------------------------------------------------------
warn_unused_kind_config() {
  local reason="$1"
  local resolved="${KIND_CONFIG}"

  # A path that does not exist stays as it is: this branch never reads the file,
  # so an unresolvable value is still worth naming in the warning.
  #
  # CDPATH is cleared for the `cd`: a developer who exports one in their profile
  # would otherwise have a relative KIND_CONFIG resolved against a CDPATH entry
  # instead of the working directory, and `cd` echoes the directory it landed in
  # whenever it found it that way — two lines in the substitution, so the
  # comparison below fails and the warning fires on the default.
  if [[ -e "${KIND_CONFIG}" ]]; then
    resolved="$(CDPATH='' cd -- "$(dirname -- "${KIND_CONFIG}")" && pwd)/$(basename -- "${KIND_CONFIG}")"
  fi

  if [[ "${resolved}" != "${SCRIPT_DIR}/kind-config.yaml" ]]; then
    log "WARNING: KIND_CONFIG='${KIND_CONFIG}' is ignored — ${reason}. The cluster keeps the config it was created with; pass this one to whatever creates it (in CI that is the helm/kind-action step's 'config:' input), or tear the cluster down first."
  fi
}

# ---------------------------------------------------------------------------
# render_kind_config — Produce the kind-config YAML that `kind create cluster`
# should consume, applying the `KIND_HOST_PORT` override and/or the
# WITH_REGISTRY_CACHE containerd registry-mirror patch when requested.
#
# The source file is `KIND_CONFIG` (default hack/kind-config.yaml; set it to
# hack/kind-config-multinode.yaml for a cluster with two workers). It is read,
# never written: every transform lands in the destination copy.
#
# When neither knob is active (`KIND_HOST_PORT == 443` and
# WITH_REGISTRY_CACHE != true — the default), the source file is copied
# verbatim — no `yq` dependency at runtime, so CI (which feeds
# hack/kind-config.yaml straight to helm/kind-action) stays byte-for-byte
# unchanged.
#
# Otherwise `yq` is required and the applicable transforms are layered onto a
# copy of the source file:
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
#   - exits 1 if KIND_CONFIG does not exist or is not readable
#   - exits 1 if KIND_HOST_PORT is not a positive integer in [1, 65535]
#   - exits 1 if `yq` is required (either transform) but not on PATH
# ---------------------------------------------------------------------------
render_kind_config() {
  local out_path="$1"
  local src="${KIND_CONFIG}"

  if [[ ! -r "${KIND_CONFIG}" ]]; then
    log "ERROR: KIND_CONFIG='${KIND_CONFIG}' does not exist or is not readable."
    exit 1
  fi

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
# is the fresh-deploy path). Convergent — a node is skipped without a containerd
# restart only when its drop-in AND the limit the running containerd reports are
# both already at NODE_NOFILE_LIMIT, so a healthy same-parameter re-run is
# read-only here while a node whose drop-in was written but whose restart failed
# is retried instead of masked — and best-effort per node: a node that cannot be
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

  local restarted=false
  local node
  for node in ${nodes}; do
    # Skip only when BOTH the drop-in on disk AND the limit the running
    # containerd reports are already at the cap. Checking the file alone would
    # permanently mask a prior run whose write succeeded but whose `systemctl
    # restart containerd` failed: the drop-in is on disk, so every later run
    # skips the node while containerd keeps the inherited ~1e9 limit — exactly
    # the #546 OOM-crashloop this function exists to prevent, and unrecoverable
    # by re-running.
    if docker exec "${node}" sh -c '
      grep -qx "LimitNOFILE='"${NODE_NOFILE_LIMIT}"'" \
        /etc/systemd/system/containerd.service.d/nofile.conf 2>/dev/null &&
      [ "$(systemctl show containerd -p LimitNOFILE --value 2>/dev/null)" = "'"${NODE_NOFILE_LIMIT}"'" ]
    ' 2>/dev/null; then
      log "  ${node}: containerd RLIMIT_NOFILE already capped to ${NODE_NOFILE_LIMIT} — skipping restart."
      continue
    fi
    if docker exec "${node}" sh -c '
      set -e
      mkdir -p /etc/systemd/system/containerd.service.d
      printf "[Service]\nLimitNOFILE='"${NODE_NOFILE_LIMIT}"'\n" \
        > /etc/systemd/system/containerd.service.d/nofile.conf
      systemctl daemon-reload
      systemctl restart containerd
    ' >/dev/null 2>&1; then
      log "  ${node}: containerd RLIMIT_NOFILE set to ${NODE_NOFILE_LIMIT}."
      restarted=true
    else
      log "  WARNING: failed to cap RLIMIT_NOFILE on ${node} — uWSGI workloads (Keystone) may OOM-crashloop."
    fi
  done

  # containerd was just restarted on at least one node; give the node(s) a moment
  # to re-register with the API server before Step 2 starts issuing kubectl apply
  # against them. Skipped when no containerd was restarted (every node's drop-in
  # already matched), so a healthy same-parameter re-run stays read-only here.
  if [[ "${restarted}" == "true" ]]; then
    wait_for_node_ready "${POD_TIMEOUT}"
  fi
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
# `cobaltcore.registry-cache=true` label so teardown-infra.sh's PURGE_REGISTRY_CACHE
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
      docker volume create --label cobaltcore.registry-cache=true "${volume}" >/dev/null 2>&1 \
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
      --label cobaltcore.registry-cache=true \
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
#
# containerd only honours these files when it was started with the certs.d
# config_path, which render_kind_config injects only on a fresh `kind create
# cluster` under WITH_REGISTRY_CACHE=true. When the probe finds a node whose
# containerd config lacks that setting (the cluster was created without the
# flag), we warn that the mirror files are inert and name the recreate remedy —
# but still write them, so they become active if the cluster is later recreated
# with the flag.
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
    if ! docker exec "${node}" grep -q '/etc/containerd/certs.d' /etc/containerd/config.toml 2>/dev/null; then
      log "  WARNING: ${node}: containerd config lacks the 'config_path = /etc/containerd/certs.d'"
      log "           registry setting — cluster '${CLUSTER_NAME}' was created WITHOUT"
      log "           WITH_REGISTRY_CACHE=true. The hosts.toml mirror files written below will"
      log "           be IGNORED and image pulls fall back to the origin registries. To activate"
      log "           the cache, recreate the cluster with WITH_REGISTRY_CACHE=true (e.g."
      log "           \`make teardown-infra && WITH_REGISTRY_CACHE=true make deploy-infra\`)."
    fi
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
  log "Kind config         : ${KIND_CONFIG} (override via KIND_CONFIG)"
  log "Node RLIMIT_NOFILE  : ${NODE_NOFILE_LIMIT:-<unset — skip cap>} (override via NODE_NOFILE_LIMIT)"
  log "Chaos Mesh         : ${WITH_CHAOS_MESH} (set WITH_CHAOS_MESH=true to install)"
  log "OVN kernel modules  : ${WITH_OVN_KERNEL_MODULES} (set WITH_OVN_KERNEL_MODULES=true to modprobe openvswitch and geneve on the host)"
  log "Prometheus stack    : ${WITH_PROMETHEUS} (set WITH_PROMETHEUS=true to install)"
  log "metrics-server      : ${WITH_METRICS_SERVER} (set WITH_METRICS_SERVER=true to install)"
  log "dizzy stack         : ${WITH_DIZZY} (VictoriaMetrics + Grafana for dizzy load/chaos runs; set WITH_DIZZY=true to install)"
  log "Registry cache      : ${WITH_REGISTRY_CACHE} (set WITH_REGISTRY_CACHE=true for a local pull-through cache; local-dev only)"
  log "ControlPlane stack  : ${WITH_CONTROLPLANE} (set WITH_CONTROLPLANE=true to provision infra via the c5c3 ControlPlane)"
  log "Infrastructure only : ${INFRA_ONLY} (set INFRA_ONLY=true for a target cluster that runs no CobaltCore operator)"
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

  # Load the OVN chassis modules the same way, gated on
  # WITH_OVN_KERNEL_MODULES so the default Quick Start does not require
  # passwordless sudo or modprobe access.
  if [[ "${WITH_OVN_KERNEL_MODULES}" == "true" ]]; then
    load_ovn_kernel_modules
  else
    log "Skipping OVN kernel modules (WITH_OVN_KERNEL_MODULES=false)."
  fi

  # Step 1: Create kind cluster
  log "=== Step 1/8: Create kind cluster ==="
  if [[ "${SKIP_KIND_CREATE:-false}" == "true" ]]; then
    warn_unused_kind_config "the cluster is pre-created (SKIP_KIND_CREATE=true)"
    log "SKIP_KIND_CREATE=true — assuming kind cluster '${CLUSTER_NAME}' already exists (CI mode)."
  elif kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
    warn_unused_kind_config "kind cluster '${CLUSTER_NAME}' already exists"
    log "Kind cluster '${CLUSTER_NAME}' already exists — skipping creation."
  else
    # Render the KIND_CONFIG file (default hack/kind-config.yaml) into a
    # tempfile so KIND_HOST_PORT overrides take effect without mutating the
    # checked-in file. We do not install a cleanup trap here:
    # openbao_bootstrap registers its own EXIT trap later in the run and a
    # second `trap ... EXIT` would overwrite it, leaking BAO_TOKEN into the
    # environment. The tempfile is a few hundred bytes and `/tmp` is
    # volume-cleared at reboot, so explicit deletion post-success is
    # sufficient.
    local kind_cfg
    kind_cfg="$(mktemp -t cobaltcore-kind-config.XXXXXX.yaml)"
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

  # Reject a cluster deployed before the shared-services relocation. Runs here
  # because it needs a reachable cluster (so not in preflight_checks) but must
  # precede the first manifest apply in Step 2.
  check_relocated_infrastructure

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

  install_gateway_api_crds
  install_envoy_gateway_crds

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

  # Opt-in dizzy overlay (VictoriaMetrics + Grafana). Layered on top of the base
  # so the default Quick Start stays minimal; enable with WITH_DIZZY=true. The
  # overlay is self-contained (no `../../` parent-dir references), so kubectl's
  # embedded kustomize renders it under the default LoadRestrictionsRootOnly
  # security check — same contract as the chaos-mesh, prometheus, and
  # metrics-server overlays (no `--load-restrictor` flag required,
  # kubernetes/kubectl#948).
  if [[ "${WITH_DIZZY}" == "true" ]]; then
    # Stage the three Grafana dashboard JSONs from the pinned dizzy release into
    # the git-ignored deploy/kind/dizzy/dashboards/ so the overlay's
    # configMapGenerator can reference them without a parent-dir traversal (same
    # LoadRestrictionsRootOnly contract as the prometheus overlay's staged JSON).
    # This MUST run immediately before `kubectl apply -k` so the files exist when
    # kustomize renders the ConfigMap.
    "${SCRIPT_DIR}/dizzy.sh" stage-dashboards
    kubectl apply -k "${REPO_ROOT}/deploy/kind/dizzy"
    log "dizzy kind overlay applied (WITH_DIZZY=true)."
    # VictoriaMetrics OTLP ingest is exposed on host 8428 → NodePort 30428 via
    # the kind extraPortMapping in hack/kind-config.yaml. A cluster created
    # before that mapping was added lacks the port, so host→VictoriaMetrics OTLP
    # ingest will not work. Probe the control-plane node's published port and
    # warn (but continue) so the operator knows to recreate the cluster.
    local dizzy_metrics_port
    dizzy_metrics_port="$(docker port "${CLUSTER_NAME}-control-plane" 30428/tcp 2>/dev/null || true)"
    if [[ -z "${dizzy_metrics_port}" ]]; then
      log "  WARNING: cluster '${CLUSTER_NAME}' predates the dizzy metrics port mapping"
      log "           (host 8428 → NodePort 30428) in hack/kind-config.yaml. Host→"
      log "           VictoriaMetrics OTLP ingest will NOT work. To activate it, recreate"
      log "           the cluster with WITH_DIZZY=true (e.g."
      log "           \`make teardown-infra && WITH_DIZZY=true make deploy-infra\`)."
    fi
  fi

  # the c5c3 ControlPlane stack (c5c3-operator + image and the K-ORC
  # GitRepository/Kustomization) is published and valid, so it would reconcile —
  # but running the full chain is opt-in. WITH_CONTROLPLANE=true deploys it; the
  # default leaves it suspended so the bring-up stays light and the keystone E2E
  # path is unchanged.
  if [[ "${WITH_CONTROLPLANE}" == "true" && "${CONTROLPLANE_OPERATORS}" == "flux" ]]; then
    # Deploy the full ControlPlane stack via Flux from the published c5c3-operator
    # chart and the K-ORC GitRepository/Kustomization. The kind base overlay
    # suspends the keystone-, horizon-, glance-, placement- and barbican-operator
    # HelmReleases for the local-build E2E path; un-suspend all five here so the
    # c5c3-operator HelmRelease's dependsOn is satisfied and the projected
    # service CRs can reconcile. Without the glance-operator the Glance CRDs
    # never install and the c5c3-operator's controlplane cache never syncs, so
    # the ControlPlane CR stays status-less. c5c3-operator, k-orc, and the
    # c5c3-charts / k-orc sources are left un-suspended (the base applied them
    # active).
    log "WITH_CONTROLPLANE=true: deploying the c5c3 ControlPlane stack (keystone-operator, horizon-operator, glance-operator, placement-operator, barbican-operator, k-orc, c5c3-operator)."
    kubectl patch helmrelease keystone-operator -n keystone-system \
      --type merge -p '{"spec":{"suspend":false}}' 2>/dev/null || true
    kubectl patch helmrelease horizon-operator -n horizon-system \
      --type merge -p '{"spec":{"suspend":false}}' 2>/dev/null || true
    kubectl patch helmrelease glance-operator -n glance-system \
      --type merge -p '{"spec":{"suspend":false}}' 2>/dev/null || true
    kubectl patch helmrelease placement-operator -n placement-system \
      --type merge -p '{"spec":{"suspend":false}}' 2>/dev/null || true
    kubectl patch helmrelease barbican-operator -n barbican-system \
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
    # CONTROLPLANE_OPERATORS=external: the service operators (keystone, horizon,
    # glance), K-ORC, and c5c3-operator are deployed out of band (e.g. the
    # e2e-controlplane CI job uses local dev images). Suspend the Flux
    # ControlPlane stack — the service-operator HelmReleases stay suspended so
    # they do not fight the dev images deployed via hack/ci-deploy-operator.sh —
    # and let the rest of this run only prepare the shared prerequisites (TLS,
    # OpenBao + per-CR seeding, ESO store).
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

  # INFRA_ONLY overrides whichever branch above ran: this cluster receives placed
  # children from a management cluster and must run no CobaltCore operator of its own.
  #
  # Every operator, not just c5c3: the WITH_CONTROLPLANE=true / flux branch above
  # un-suspends the five service operators the kind base overlay suspends, so
  # covering c5c3 alone would leave that combination with two controller sets
  # server-side-applying the same Deployments, ConfigMaps and credential Secrets.
  # Both commands tolerate absence — the HelmRelease may not be applied yet on a
  # fresh cluster, and the Deployment exists only where the chart already
  # installed once.
  if [[ "${INFRA_ONLY}" == "true" ]]; then
    log "INFRA_ONLY=true: suspending every CobaltCore operator HelmRelease and scaling its Deployment to zero."
    for operator in c5c3:c5c3-system keystone:keystone-system horizon:horizon-system \
                    glance:glance-system placement:placement-system barbican:barbican-system; do
      kubectl patch helmrelease "${operator%%:*}-operator" -n "${operator##*:}" \
        --type merge -p '{"spec":{"suspend":true}}' 2>/dev/null || true
      kubectl scale deployment -n "${operator##*:}" "${operator%%:*}-operator" --replicas=0 2>/dev/null || true
    done
  fi

  # Force-reconcile the Flux chart sources (HelmRepository and OCIRepository)
  # so chart indexes and pinned artifacts are available before HelmReleases
  # attempt to resolve charts. Without this, the helm-controller may see
  # unindexed or unfetched sources and wait until the next reconcile interval
  # (up to 1h) before retrying.
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
  # preserved exactly as before; garage-operator and openbao-operator are
  # appended as the last base releases, and chaos-mesh after them, to avoid
  # moving any other release's relative position. openbao is namespace-qualified
  # because ALLOW_PRE_RELOCATION=true keeps the retired openbao-system copy
  # alongside the new one, and a bare name matching two namespaces cannot be
  # waited on.
  local helm_releases=(prometheus-operator-crds shared-services/openbao mariadb-operator-crds mariadb-operator external-secrets memcached-operator envoy-gateway garage-operator openbao-operator)
  if [[ "${WITH_CHAOS_MESH}" == "true" ]]; then
    helm_releases+=(chaos-mesh)
  fi
  # kube-prometheus-stack is appended last so the relative
  # ordering of the nine base releases (and chaos-mesh) is preserved exactly.
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
  # so the relative ordering of the nine base releases is preserved exactly.
  if [[ "${WITH_METRICS_SERVER}" == "true" ]]; then
    helm_releases+=(metrics-server)
  fi
  # dizzy-victoria-metrics and dizzy-grafana are appended last so the relative
  # ordering of the base releases is preserved exactly. wait_for_helmreleases
  # resolves each release's namespace dynamically, so the names suffice.
  if [[ "${WITH_DIZZY}" == "true" ]]; then
    helm_releases+=(dizzy-victoria-metrics dizzy-grafana)
  fi
  wait_for_helmreleases "${release_wait_timeout}" "${helm_releases[@]}"

  if [[ "${WITH_DIZZY}" == "true" ]]; then
    local dizzy_grafana_url="https://dizzy.127-0-0-1.nip.io"
    if [[ "${KIND_HOST_PORT}" != "443" ]]; then
      dizzy_grafana_url="${dizzy_grafana_url}:${KIND_HOST_PORT}"
    fi
    log "dizzy Grafana: ${dizzy_grafana_url} (anonymous read-only; dashboards land once a dizzy soak exports metrics)"
  fi

  # with kube-prometheus-stack Ready, flip the operator charts'
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
    enable_operator_servicemonitor glance-operator glance-system "${HELMRELEASE_TIMEOUT}"
    enable_operator_servicemonitor placement-operator placement-system "${HELMRELEASE_TIMEOUT}"
    enable_operator_servicemonitor barbican-operator barbican-system "${HELMRELEASE_TIMEOUT}"
  fi

  # Step 5: Apply infrastructure kustomize overlay (CRD-dependent resources)
  log "=== Step 5/8: Apply infrastructure kustomize overlay ==="

  # Wait for operator CRDs to be registered before applying CRD-dependent
  # resources. HelmRelease Ready does not guarantee CRDs are available in
  # the API server — the operator pods may still be starting.
  # envoyproxies.gateway.envoyproxy.io is installed by the envoy-gateway
  # HelmRelease (Phase 3 above) and is required by the EnvoyProxy CR in
  # deploy/kind/infrastructure/envoy-nodeport.yaml.
  # openbaoclusters.openbao.org is registered by the openbao-operator
  # HelmRelease (Phase 3 above); waiting on it keeps the overlay apply from
  # racing that registration.
  wait_for_crds "${POD_TIMEOUT}" \
    memcacheds.memcached.c5c3.io \
    clustersecretstores.external-secrets.io \
    externalsecrets.external-secrets.io \
    mariadbs.k8s.mariadb.com \
    envoyproxies.gateway.envoyproxy.io \
    garageclusters.garage.rajsingh.info \
    garagebuckets.garage.rajsingh.info \
    garagekeys.garage.rajsingh.info \
    openbaoclusters.openbao.org

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
    # The standalone keystone-db ExternalSecret authenticates through the
    # per-tenant store (openbao-tenant-store), the enforced default since #606 —
    # the shared cluster store no longer grants any read on openstack/keystone/*.
    # There is no c5c3 operator on the standalone path to provision that store, so
    # create its in-cluster half (the eso-tenant-auth ServiceAccount, the mTLS
    # client Certificate, and the namespaced SecretStore) here, then force its ESO
    # re-validation so keystone-db can sync below. keystone-admin and
    # mariadb-root-password stay on the shared store (bootstrap/infrastructure reads).
    log "Provisioning the per-tenant OpenBao store for the standalone openstack namespace..."
    bash "${REPO_ROOT}/deploy/openbao/bootstrap/setup-eso-tenant.sh" openstack
    kubectl annotate secretstore/openbao-tenant-store -n openstack \
      "deploy.c5c3.io/reconcile-trigger=${now}" --overwrite || true
    kubectl wait secretstore/openbao-tenant-store -n openstack \
      --for=condition=Ready --timeout="${POD_TIMEOUT}s" || true

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

  # Proving OpenBao instance (deploy/kind/infrastructure/openbao-instance.yaml).
  # Same apply-before-dependency reason as the Garage block below: Step 5 applies
  # the CR before Step 7 seeds its unseal key in OpenBao.
  #
  # The adoption choreography exists because the openbao-operator blind-creates
  # the fixed-name Secret openbao-instance-unseal-key (Immutable, random key) on
  # its first reconcile, and accepts a pre-existing one only against an ownership
  # proof. The overlay therefore applies the CR paused; here the seeded key is
  # synced into that Secret, the proof is attached, and only then is the CR
  # un-paused, so the operator adopts the key custodied in the management
  # OpenBao.
  #
  # The proof is a controller ownerReference to the CR, NOT the operator's
  # openbao.org/owner-uid annotation. The operator accepts either one, but its
  # own ValidatingAdmissionPolicy openbao-lock-managed-resource-mutations
  # reserves that annotation for its own ServiceAccounts and denies every other
  # writer — clusterwide, at failurePolicy: Fail, and without restricting itself
  # to resources the operator already manages. An annotate here is rejected, so
  # the ownerReference is the only proof this script can attach.
  #
  # This runs BEFORE the Garage block and its readiness wait is collected after
  # it: the instance depends on nothing Garage provides, and everything the
  # un-pause starts (image pull, PVC bind, the two cert-manager Secrets,
  # self-init, unseal) is wall-clock on every deploy otherwise.
  #
  # The ExternalSecret is refreshPolicy: CreatedOnce, so this annotation only
  # breaks ESO's backoff from the Step 5 apply against a still-sealed OpenBao —
  # it cannot re-materialize the key of a live instance (that would seal it
  # permanently).
  log "Forcing openbao-instance unseal-key ExternalSecret sync..."
  kubectl annotate externalsecret/openbao-instance-unseal-key -n openstack \
    "force-sync=${now}" --overwrite || true
  wait_for_externalsecrets "openstack" "${EXTERNALSECRET_TIMEOUT}" \
    openbao-instance-unseal-key

  # Attach the ownership proof to the ESO-materialized Secret so the operator
  # adopts it, then un-pause. The ExternalSecret is creationPolicy: Orphan for
  # exactly this reason: an object carries at most one controller
  # ownerReference, so ESO must leave that slot free.
  local openbao_instance_uid
  openbao_instance_uid=$(kubectl get openbaocluster openbao-instance -n openstack \
    -o jsonpath='{.metadata.uid}')
  kubectl patch secret openbao-instance-unseal-key -n openstack --type merge \
    -p "{\"metadata\":{\"ownerReferences\":[{\"apiVersion\":\"openbao.org/v1alpha1\",\"kind\":\"OpenBaoCluster\",\"name\":\"openbao-instance\",\"uid\":\"${openbao_instance_uid}\",\"controller\":true,\"blockOwnerDeletion\":true}]}}"

  # The instance's API-server egress, resolved from the live cluster and applied in
  # the SAME patch as the un-pause, so the operator's very first reconcile already
  # renders the port-6443 rules.
  #
  # The openbao-operator's default-deny NetworkPolicy derives its API-server egress
  # from the in-cluster service VIP on port 443. kindnet enforces egress against the
  # POST-DNAT destination from kind 0.32 onwards, so that rule never matches: the
  # packet it inspects is addressed to the API server's own endpoint on 6443. The
  # instance then loses the API server, raft auto-join times out, self-init never
  # completes, and the partial raft state wedges every later initialization attempt
  # — recoverable only by deleting the CR AND its PVC.
  #
  # The addresses are read rather than hardcoded because a kind node address does
  # not survive a cluster re-creation. EndpointSlice default/kubernetes is where
  # kube-apiserver publishes them itself, so it needs no controller-manager and
  # exists on every conformant cluster.
  # The read runs under `if !` so a failed lookup reports through the same branch as
  # an empty one, rather than tripping `set -e` and dying without saying why.
  local api_server_endpoint_ips=""
  if ! api_server_endpoint_ips="$(kubectl get endpointslice kubernetes -n default -o json |
    jq -c '[.endpoints[]?.addresses[]?] | unique')"; then
    api_server_endpoint_ips=""
  fi
  if [[ -z "${api_server_endpoint_ips}" || "${api_server_endpoint_ips}" == "[]" ]]; then
    log "ERROR: no API server address in EndpointSlice default/kubernetes."
    log "       The OpenBao instance's NetworkPolicy would deny its API-server"
    log "       egress, wedging the raft store on first initialization. Aborting"
    log "       rather than un-pausing the instance into that state."
    exit 1
  fi
  log "Pinning the instance's API-server egress to ${api_server_endpoint_ips}..."

  # A JSON merge patch merges objects key by key, so spec.network.trustedIngressPeers
  # from the overlay survives. A later `kubectl apply -k` re-run cannot drop the
  # field either: it appears in neither the overlay nor the last-applied annotation.
  kubectl patch openbaocluster openbao-instance -n openstack --type merge \
    -p "{\"spec\":{\"network\":{\"apiServerEndpointIPs\":${api_server_endpoint_ips}},\"paused\":false}}"

  # Garage object store (S3 backend for the Glance e2e suites). Its
  # GarageCluster/GarageBucket/GarageKey CRs live in shared-services and are
  # applied, like the three ESO ExternalSecrets below, on BOTH paths — they are
  # not among the MariaDB/Memcached/standalone-shim resources the
  # WITH_CONTROLPLANE overlay filter drops — so this readiness block runs
  # unconditionally. garage-admin-token and garage-s3-credentials in
  # shared-services feed the GarageCluster's admin token and the GarageKey
  # import; the retained garage-s3-credentials in openstack serves the Glance
  # consumers. All three read the OpenBao paths
  # bootstrap/openstack/garage/{admin-token,s3-credentials} through the shared
  # cluster store (re-validated above on both paths); force a re-sync now that
  # OpenBao is up, the same reason as the standalone shims above.
  log "Forcing Garage ExternalSecret re-sync..."
  for es in garage-admin-token garage-s3-credentials; do
    kubectl annotate "externalsecret/${es}" -n shared-services \
      "force-sync=${now}" --overwrite || true
  done
  kubectl annotate externalsecret/garage-s3-credentials -n openstack \
    "force-sync=${now}" --overwrite || true
  wait_for_externalsecrets "shared-services" "${EXTERNALSECRET_TIMEOUT}" \
    garage-admin-token garage-s3-credentials
  wait_for_externalsecrets "openstack" "${EXTERNALSECRET_TIMEOUT}" \
    garage-s3-credentials

  # The GarageCluster CR was applied in Step 5, before its admin-token Secret
  # existed (that Secret is materialized by the ExternalSecret above). The
  # operator may have stopped retrying; patch an annotation to force a new
  # reconciliation now that the Secret is available, then wait for the cluster to
  # reach its terminal healthy phase. "Running" is the operator's fully-
  # operational phase (set once the Admin API is reachable); a storage-backed
  # GarageCluster never advances to the "Ready" phase value, and the operator
  # sets no "Ready" status condition (only health conditions such as
  # QuorumAtRisk), so the wait keys on status.phase rather than a condition.
  log "Triggering GarageCluster re-reconciliation..."
  kubectl patch garagecluster garage -n shared-services --type merge \
    -p "{\"metadata\":{\"annotations\":{\"deploy.c5c3.io/reconcile-trigger\":\"${now}\"}}}" || true
  log "Waiting for the GarageCluster CR to become Running..."
  kubectl wait garagecluster/garage -n shared-services \
    --for=jsonpath='{.status.phase}'=Running --timeout="${POD_TIMEOUT}s"
  log "GarageCluster CR is Running."

  # The openbao-operator runs multi-tenant, so its controller reconciles only in
  # namespaces an OpenBaoTenant has admitted
  # (deploy/kind/infrastructure/openbao-tenant.yaml). Wait for that onboarding
  # first: in an un-admitted namespace the controller pauses silently at V(1),
  # and the Available wait below would burn its whole timeout on a CR with an
  # empty status.
  log "Waiting for the OpenBaoTenant to be provisioned..."
  kubectl wait openbaotenant/openstack -n openstack \
    --for=jsonpath='{.status.provisioned}'=true --timeout="${POD_TIMEOUT}s"
  log "OpenBaoTenant is provisioned."

  # Collect the readiness of the proving OpenBao instance un-paused above.
  log "Waiting for the OpenBaoCluster CR to become Available..."
  kubectl wait openbaocluster/openbao-instance -n openstack \
    --for=condition=Available --timeout="${POD_TIMEOUT}s"
  log "OpenBaoCluster CR is Available."

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
      # Remove suspension from Flux HelmReleases and Kustomizations
      kubectl patch helmrelease/keystone-operator -n keystone-system --type merge \
        -p '{"spec":{"suspend":false}}' 2>/dev/null || true
      kubectl wait helmrelease/keystone-operator -n keystone-system \
        --for=condition=Ready --timeout="${HELMRELEASE_TIMEOUT}s" 2>/dev/null \
        || log "  keystone-operator not Ready yet (continuing; the ControlPlane tolerates it)."

      kubectl patch kustomization/k-orc -n flux-system --type merge \
        -p '{"spec":{"suspend":false}}' 2>/dev/null || true
      kubectl wait kustomization/k-orc -n flux-system \
        --for=condition=Ready --timeout="${HELMRELEASE_TIMEOUT}s" 2>/dev/null \
        || log "  k-orc Kustomization not Ready yet (continuing)."

      kubectl patch helmrelease/c5c3-operator -n c5c3-system --type merge \
        -p '{"spec":{"suspend":false}}' 2>/dev/null || true
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

      # Onboard the OpenBao database-engine tenant so managed-mode Keystone — and
      # Glance, when the ControlPlane declares it on the shared managed database —
      # can draw engine-issued (Dynamic) DB credentials (#439). The ControlPlane
      # CR always lives in the openstack namespace; its MariaDB defaults to
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
