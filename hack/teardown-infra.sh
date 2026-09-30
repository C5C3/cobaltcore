#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/teardown-infra.sh — Delete the kind E2E cluster, or, under
# EXTERNAL_CLUSTER=true, remove the infrastructure stack hack/deploy-infra.sh put
# on the cluster the current kubeconfig context points at (see
# teardown_external_cluster).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

CLUSTER_NAME="${CLUSTER_NAME:-cobaltcore}"

# When true, also remove the opt-in registry pull-through caches (#564) — the
# proxy containers AND their persistent volumes. Defaults to false: deleting the
# cache on every teardown would defeat its whole point (surviving kind delete /
# recreate cycles), so a plain `make teardown-infra` leaves the warm cache
# running for the next `WITH_REGISTRY_CACHE=true make deploy-infra`. Set
# PURGE_REGISTRY_CACHE=true to reclaim the disk / start cold.
PURGE_REGISTRY_CACHE="${PURGE_REGISTRY_CACHE:-false}"

# Selects the external-cluster teardown, the counterpart of
# `EXTERNAL_CLUSTER=true make deploy-infra`: the stack is removed from the cluster
# the current kubeconfig context points at, which itself stays. Neither kind nor
# docker is called. Any value other than `true` keeps the kind teardown.
EXTERNAL_CLUSTER="${EXTERNAL_CLUSTER:-false}"

# The overlay root the deploy applied, resolved the way hack/deploy-infra.sh
# resolves it: relative to REPO_ROOT unless absolute. Read only under
# EXTERNAL_CLUSTER=true.
EXTERNAL_OVERLAY="${EXTERNAL_OVERLAY:-deploy/lab/metal-stack}"
case "${EXTERNAL_OVERLAY}" in
  /*) OVERLAY_ROOT="${EXTERNAL_OVERLAY}" ;;
  *) OVERLAY_ROOT="${REPO_ROOT}/${EXTERNAL_OVERLAY}" ;;
esac

# Seconds each external-cluster delete may wait for its objects to be gone. A
# wait that runs out ends the teardown with exit 1, naming what is still there.
TEARDOWN_TIMEOUT="${TEARDOWN_TIMEOUT:-600}"

# The API groups whose CRDs the stack registers: the CobaltCore operators
# (operators/*/config/crd/bases: c5c3.io and <svc>.openstack.c5c3.io), the
# HelmReleases and Flux Kustomizations of deploy/flux-system, and the two bundles
# hack/deploy-infra.sh installs itself (Gateway API, Envoy Gateway). The groups
# of the platform (autoscaling.k8s.io, cert.gardener.cloud, dns.gardener.cloud,
# crd.projectcalico.org, metallb.io, snapshot.storage.k8s.io) are absent on
# purpose and must stay absent.
STACK_CRD_GROUPS='cert-manager\.io|external-secrets\.io|k8s\.mariadb\.com|openbao\.org|garage\.rajsingh\.info|rabbitmq\.com|gateway\.networking\.k8s\.io|gateway\.envoyproxy\.io|monitoring\.coreos\.com|c5c3\.io|openstack\.k-orc\.cloud|(source|kustomize|helm|notification|image)\.toolkit\.fluxcd\.io|fluxcd\.controlplane\.io'

# ---------------------------------------------------------------------------
# log — Print a timestamped log message (ISO 8601 UTC).
# ---------------------------------------------------------------------------
log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

# ---------------------------------------------------------------------------
# purge_registry_cache — Remove every registry pull-through cache container and
# volume, identified by the cobaltcore.registry-cache=true label that
# start_registry_cache stamps on them. Decoupled from the upstream set (no need
# to know the registry list here) and best-effort throughout. No-op unless
# PURGE_REGISTRY_CACHE=true.
# ---------------------------------------------------------------------------
purge_registry_cache() {
  if [[ "${PURGE_REGISTRY_CACHE}" != "true" ]]; then
    log "Leaving registry pull-through caches running (set PURGE_REGISTRY_CACHE=true to remove them)."
    return 0
  fi

  if ! command -v docker >/dev/null 2>&1; then
    log "WARNING: docker not found — cannot purge registry caches."
    return 0
  fi

  log "Purging registry pull-through caches (PURGE_REGISTRY_CACHE=true)..."

  local containers volumes
  containers=$(docker ps -aq --filter label=cobaltcore.registry-cache=true 2>/dev/null) || true
  if [[ -n "${containers}" ]]; then
    # shellcheck disable=SC2086
    docker rm -f ${containers} >/dev/null 2>&1 || true
    log "  Removed registry-cache container(s)."
  fi

  volumes=$(docker volume ls -q --filter label=cobaltcore.registry-cache=true 2>/dev/null) || true
  if [[ -n "${volumes}" ]]; then
    # shellcheck disable=SC2086
    docker volume rm ${volumes} >/dev/null 2>&1 || true
    log "  Removed registry-cache volume(s)."
  fi

  if [[ -z "${containers}" && -z "${volumes}" ]]; then
    log "  No registry-cache containers or volumes found."
  fi
}

# ---------------------------------------------------------------------------
# delete_and_wait WHAT ARGS... — `kubectl delete ARGS` that waits for the objects
# to be gone, bounded by TEARDOWN_TIMEOUT.
#
# Absence is success: --ignore-not-found covers a missing object, and a kind
# whose CRD is already gone (a second run, or a deploy that never installed it)
# and an empty `-f -` (an overlay without a Gateway) are filtered out of the
# result as well. Anything else kubectl reports (a wait
# that ran out, which names the objects still present, or an unreachable API
# server) exits 1 with kubectl's output. stdin is passed through, for `-f -`.
# ---------------------------------------------------------------------------
delete_and_wait() {
  local what="$1"
  shift
  log "Deleting ${what}..."

  local out rc=0
  out="$(kubectl delete "$@" --ignore-not-found --wait --timeout="${TEARDOWN_TIMEOUT}s" 2>&1)" || rc=$?
  if [[ ${rc} -eq 0 ]]; then
    return 0
  fi

  local unexplained
  unexplained="$(grep -vE \
    -e ' deleted$' \
    -e '^No resources found' \
    -e '^Warning: ' \
    -e 'resource mapping not found' \
    -e 'no matches for kind' \
    -e 'ensure CRDs are installed first' \
    -e "doesn't have a resource type" \
    -e 'no objects passed to delete' \
    -e '^[[:space:]]*$' <<<"${out}" || true)"
  if [[ -z "${unexplained}" ]]; then
    return 0
  fi

  log "ERROR: deleting ${what} failed or did not finish within ${TEARDOWN_TIMEOUT}s:"
  local line
  while IFS= read -r line; do
    log "         ${line}"
  done <<<"${unexplained}"
  exit 1
}

# ---------------------------------------------------------------------------
# resume_installed_flux_objects — Lift the suspension of every Flux object whose
# deletion has something to clean up.
#
# The helm-controller does not uninstall the chart of a suspended HelmRelease it
# finalizes, and the kustomize-controller does not prune the inventory of a
# suspended Kustomization. The kind base applies the nine service-operator
# releases and the K-ORC Kustomization suspended, and hack/deploy-infra.sh
# suspends more at runtime (c5c3-operator and the K-ORC source on the default
# path, every operator under INFRA_ONLY), so a chart that installed before its
# suspension would stay in the cluster with its ClusterRoles.
#
# Only a suspended object that installed something is resumed: a HelmRelease with
# a release history, a Kustomization with an inventory. Resuming one that never
# ran would have Flux install it only to uninstall it again inside the wait.
# Absence is tolerated: on a second run the Flux CRDs are gone.
# ---------------------------------------------------------------------------
resume_installed_flux_objects() {
  local ref
  while IFS= read -r ref; do
    [[ -n "${ref}" ]] || continue
    log "  Resuming HelmRelease ${ref} so its chart is uninstalled."
    kubectl patch helmrelease "${ref#*/}" -n "${ref%%/*}" --type merge \
      -p '{"spec":{"suspend":false}}' >/dev/null 2>&1 || true
  done < <(kubectl get helmrelease -A -o json 2>/dev/null |
    yq -r '.items[] | select(.spec.suspend == true and ((.status.history // []) | length) > 0) | .metadata.namespace + "/" + .metadata.name' 2>/dev/null || true)

  while IFS= read -r ref; do
    [[ -n "${ref}" ]] || continue
    log "  Resuming Kustomization flux-system/${ref} so its inventory is pruned."
    kubectl patch kustomization "${ref}" -n flux-system --type merge \
      -p '{"spec":{"suspend":false}}' >/dev/null 2>&1 || true
  done < <(kubectl get kustomization -n flux-system -o json 2>/dev/null |
    yq -r '.items[] | select(.spec.suspend == true and ((.status.inventory.entries // []) | length) > 0) | .metadata.name' 2>/dev/null || true)
}

# ---------------------------------------------------------------------------
# stack_namespaces — The namespaces the stack creates, one per line: every
# Namespace of deploy/flux-system/namespaces.yaml, the two the kind base files
# declare (envoy-gateway-system, headlamp-system), and flux-system. No other
# namespace is ever deleted. Fails when yq cannot read the file.
# ---------------------------------------------------------------------------
stack_namespaces() {
  yq -N -r 'select(.kind == "Namespace") | .metadata.name' \
    "${REPO_ROOT}/deploy/flux-system/namespaces.yaml" || return 1
  printf '%s\n' envoy-gateway-system headlamp-system flux-system
}

# ---------------------------------------------------------------------------
# read_stack_crds — Set STACK_CRDS to the CRDs of STACK_CRD_GROUPS present on the
# cluster, one `customresourcedefinition.apiextensions.k8s.io/<name>` per line.
# Exits 1 when the CRDs cannot be listed: an unreadable cluster is not one
# without CRDs.
# ---------------------------------------------------------------------------
read_stack_crds() {
  local all
  if ! all="$(kubectl get crd -o name)"; then
    log "ERROR: cannot list the cluster's CRDs (kubectl's error is above)."
    exit 1
  fi
  STACK_CRDS="$(grep -E "\.(${STACK_CRD_GROUPS})$" <<<"${all}" || true)"
}

# ---------------------------------------------------------------------------
# teardown_external_cluster — Remove the stack from the current context's cluster.
#
# The order makes every finalizer run while the controller that clears it still
# exists:
#   1. every ControlPlane in openstack, while the c5c3-operator runs, so it
#      reaps its children, then every OVNCentral there: the quick start's
#      central is referenced, not owned, and its database pods would hold
#      their PVCs through step 4;
#   2. the infrastructure overlay (MariaDB, Memcached, Garage, the OpenBao
#      instance and tenant, the ExternalSecrets, Certificates and issuers) and
#      the opt-in message bus, while their operators run; the proving OpenBao
#      instance is switched to deletionPolicy DeletePVCs first, because the
#      openbao-operator cannot finalize it under the default Retain;
#   3. the base overlay without its Namespaces and FluxInstance, after resuming
#      what was suspended, so the helm-controller uninstalls every chart and the
#      Flux Kustomizations prune K-ORC and the RabbitMQ operator; the Gateway and
#      GatewayClass go first, while Envoy Gateway still clears their finalizer,
#      and the opt-in kube-prometheus-stack release goes last;
#   4. the PVCs in shared-services and openstack, which Helm and the operators
#      leave behind. Not earlier: the openbao-operator chart's admission policy
#      denies deleting its managed PVCs to everyone but the operator until the
#      chart is uninstalled;
#   5. the FluxInstance, so the flux-operator uninstalls the toolkit;
#   6. the flux-system namespace and the flux-operator's cluster-scoped RBAC;
#   7. the stack namespaces (stack_namespaces), by name;
#   8. the CRDs of STACK_CRD_GROUPS.
# It ends with the count of stack CRDs and namespaces still present, which must
# both be zero. Every delete ignores absence, so a second run finds nothing and
# exits 0; a wait that runs out exits 1 (delete_and_wait). kube-system and the
# platform's namespaces and CRDs are never named.
# ---------------------------------------------------------------------------
teardown_external_cluster() {
  local cmd
  for cmd in kubectl yq; do
    if ! command -v "${cmd}" >/dev/null 2>&1; then
      log "ERROR: '${cmd}' is not installed or not in PATH."
      exit 1
    fi
  done

  if [[ ! -f "${OVERLAY_ROOT}/base/kustomization.yaml" ||
    ! -f "${OVERLAY_ROOT}/infrastructure/kustomization.yaml" ]]; then
    log "ERROR: EXTERNAL_OVERLAY='${EXTERNAL_OVERLAY}' has no base/ and infrastructure/ kustomization (resolved to ${OVERLAY_ROOT})."
    exit 1
  fi

  local out
  if ! out="$(kubectl version --request-timeout=10s 2>&1)"; then
    log "ERROR: the API server of the current kubeconfig context does not answer:"
    log "         ${out}"
    exit 1
  fi
  log "Kubeconfig context  : $(kubectl config current-context 2>/dev/null || echo '<none>')"
  log "Overlay             : ${OVERLAY_ROOT}"
  log "Wait per step       : ${TEARDOWN_TIMEOUT}s (override via TEARDOWN_TIMEOUT)"

  # 1. The ControlPlanes, only where the c5c3 CRD was ever installed.
  if kubectl get crd controlplanes.c5c3.io >/dev/null 2>&1; then
    delete_and_wait "the ControlPlanes in openstack" controlplane --all -n openstack
  fi
  # The standalone OVNCentral the quick start applies beside the ControlPlane
  # carries no finalizer; garbage collection reaps its StatefulSets, and the
  # PVC wait of step 4 covers their pods.
  if kubectl get crd ovncentrals.ovn.openstack.c5c3.io >/dev/null 2>&1; then
    delete_and_wait "the OVNCentrals in openstack" ovncentrals.ovn.openstack.c5c3.io --all -n openstack
  fi

  # 2. The CRs the infrastructure operators finalize, and the opt-in message
  # bus: its RabbitmqCluster would keep its finalizer once step 3 prunes the
  # cluster-operator.
  #
  # The proving OpenBao instance keeps the operator's default deletionPolicy,
  # Retain, under which the openbao-operator strips the owner references of its
  # unseal-key and root-token Secrets before it clears its finalizer. Neither
  # can succeed: the chart's admission policy denies the controller the patch
  # on the ESO-materialized unseal key hack/deploy-infra.sh adopted, and the
  # tenant RBAC grants no read on the root token self-init never writes.
  # DeletePVCs skips that step and deletes the instance's PVCs, which step 4
  # would delete anyway.
  if kubectl get openbaoclusters.openbao.org openbao-instance -n openstack >/dev/null 2>&1; then
    log "Setting deletionPolicy DeletePVCs on the proving OpenBao instance..."
    kubectl patch openbaoclusters.openbao.org openbao-instance -n openstack --type merge \
      -p '{"spec":{"deletionPolicy":"DeletePVCs"}}' >/dev/null
  fi
  delete_and_wait "the infrastructure overlay" -k "${OVERLAY_ROOT}/infrastructure"
  delete_and_wait "the message-bus overlay" -k "${REPO_ROOT}/deploy/kind/messaging"

  # 3. The Flux objects of the base overlay.
  resume_installed_flux_objects
  local render
  if ! render="$(kubectl kustomize "${OVERLAY_ROOT}/base")"; then
    log "ERROR: cannot render ${OVERLAY_ROOT}/base (kustomize's error is above)."
    exit 1
  fi
  printf '%s\n' "${render}" |
    yq 'select(.kind == "Gateway" or .kind == "GatewayClass")' |
    delete_and_wait "the Gateway and the GatewayClass" -f -
  printf '%s\n' "${render}" |
    yq 'select(.kind != "Namespace" and .kind != "FluxInstance" and .kind != "Gateway" and .kind != "GatewayClass")' |
    delete_and_wait "the base overlay (HelmReleases, Flux sources and Kustomizations)" -f -
  # The opt-in monitoring release, by file: its overlay's configMapGenerator needs
  # a file the deploy stages, so `-k` does not render on a fresh checkout.
  delete_and_wait "the kube-prometheus-stack release" -f "${REPO_ROOT}/deploy/kind/prometheus/release.yaml"

  # 4. The volumes Helm and the operators leave behind.
  delete_and_wait "the PVCs in shared-services" pvc --all -n shared-services
  delete_and_wait "the PVCs in openstack" pvc --all -n openstack

  # 5. The Flux toolkit.
  delete_and_wait "the FluxInstance" fluxinstance flux -n flux-system

  # 6. The flux-operator and its cluster-scoped RBAC, by the names the
  # install.yaml of FLUX_OPERATOR_VERSION (hack/deploy-infra.sh) gives them;
  # re-check them on every flux-operator bump. Its CRDs are covered by step 8.
  delete_and_wait "the flux-system namespace" namespace flux-system
  delete_and_wait "the flux-operator ClusterRoleBinding" clusterrolebinding flux-operator-cluster-admin
  delete_and_wait "the flux-operator ClusterRoles" \
    clusterrole flux-operator-edit flux-operator-view flux-web-admin flux-web-user

  # 7. The stack namespaces, flux-system aside: step 6 deleted it. The same list
  # is counted in step 9.
  local namespace_list namespaces=() namespaces_to_delete=() line
  if ! namespace_list="$(stack_namespaces)"; then
    log "ERROR: cannot read the stack namespaces from deploy/flux-system/namespaces.yaml (yq's error is above)."
    exit 1
  fi
  while IFS= read -r line; do
    [[ -n "${line}" ]] || continue
    namespaces+=("${line}")
    if [[ "${line}" != "flux-system" ]]; then
      namespaces_to_delete+=("${line}")
    fi
  done <<<"${namespace_list}"
  delete_and_wait "the stack namespaces" namespace "${namespaces_to_delete[@]}"

  # 8. The stack CRDs, by API group.
  local crds=()
  read_stack_crds
  while IFS= read -r line; do
    if [[ -n "${line}" ]]; then
      crds+=("${line}")
    fi
  done <<<"${STACK_CRDS}"
  if [[ ${#crds[@]} -gt 0 ]]; then
    delete_and_wait "the stack CRDs" "${crds[@]}"
  fi

  # 9. What is left.
  local crds_left namespaces_left
  read_stack_crds
  crds_left="$(grep -c . <<<"${STACK_CRDS}" || true)"
  if ! namespaces_left="$(kubectl get namespace "${namespaces[@]}" --ignore-not-found -o name)"; then
    log "ERROR: cannot read the stack namespaces (kubectl's error is above)."
    exit 1
  fi
  namespaces_left="$(grep -c . <<<"${namespaces_left}" || true)"
  log "Stack CRDs left: ${crds_left}; stack namespaces left: ${namespaces_left}"
  if [[ "${crds_left}" != "0" || "${namespaces_left}" != "0" ]]; then
    log "ERROR: the teardown left stack objects behind."
    exit 1
  fi
}

main() {
  log "=== Teardown Infrastructure ==="
  if [[ "${EXTERNAL_CLUSTER}" == "true" ]]; then
    teardown_external_cluster
  else
    log "Deleting kind cluster '${CLUSTER_NAME}'..."
    kind delete cluster --name "${CLUSTER_NAME}" 2>/dev/null || true
    log "Cluster '${CLUSTER_NAME}' deleted (or did not exist)."
    purge_registry_cache
  fi
  log "=== Done ==="
}

# Run main only when executed directly so unit tests (tests/unit/hack/) can
# source this script and exercise purge_registry_cache and the external-cluster
# teardown in isolation.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
