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
# HelmReleases and Flux Kustomizations of deploy/flux-system, the two bundles
# hack/deploy-infra.sh installs itself (Gateway API, Envoy Gateway), the
# kvm.cloud.sap CRDs the charts of the lab hypervisors install
# (deploy/lab/metal-stack/hypervisor; Helm leaves them behind), and the
# chaos-mesh.org CRDs the opt-in Chaos Mesh release installs from its chart's
# crds/, which Helm never deletes either. The groups
# of the platform (autoscaling.k8s.io, cert.gardener.cloud, dns.gardener.cloud,
# crd.projectcalico.org, metallb.io, snapshot.storage.k8s.io) are absent on
# purpose and must stay absent.
STACK_CRD_GROUPS='cert-manager\.io|external-secrets\.io|k8s\.mariadb\.com|openbao\.org|garage\.rajsingh\.info|rabbitmq\.com|gateway\.networking\.k8s\.io|gateway\.envoyproxy\.io|monitoring\.coreos\.com|c5c3\.io|openstack\.k-orc\.cloud|(source|kustomize|helm|notification|image)\.toolkit\.fluxcd\.io|fluxcd\.controlplane\.io|kvm\.cloud\.sap|chaos-mesh\.org'

# The kinds of the cluster-scoped objects the stack's charts leave behind. Helm
# does not track a hook object as part of its release, so no uninstall removes
# it. The helm-controller labels every object of a HelmRelease, hooks included,
# with helm.toolkit.fluxcd.io/namespace, the namespace of the release, which is
# how the teardown finds them. These four kinds exist on every cluster, so the
# read needs no CRD check.
STACK_CHART_OBJECT_KINDS='clusterrole,clusterrolebinding,mutatingwebhookconfiguration,validatingwebhookconfiguration'

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
# declare (envoy-gateway-system, headlamp-system), chaos-mesh and dizzy, which
# the opt-in Chaos Mesh and dizzy overlays declare, and flux-system. No other
# namespace is ever deleted. Fails when yq cannot read the file.
# ---------------------------------------------------------------------------
stack_namespaces() {
  yq -N -r 'select(.kind == "Namespace") | .metadata.name' \
    "${REPO_ROOT}/deploy/flux-system/namespaces.yaml" || return 1
  printf '%s\n' envoy-gateway-system headlamp-system chaos-mesh dizzy flux-system
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
# read_stack_chart_objects NAMESPACE... — Set STACK_CHART_OBJECTS to the objects
# of STACK_CHART_OBJECT_KINDS whose helm.toolkit.fluxcd.io/namespace label names
# one of the NAMESPACEs, one `<kind>.<group>/<name>` per line. Exits 1 when the
# objects cannot be listed: an unreadable cluster is not one without leftovers.
# --ignore-not-found keeps kubectl's "No resources found" off stderr when
# nothing matches, as on a second run and in the final count.
# ---------------------------------------------------------------------------
read_stack_chart_objects() {
  local selector
  selector="helm.toolkit.fluxcd.io/namespace in ($(printf '%s\n' "$@" | paste -sd, -))"
  if ! STACK_CHART_OBJECTS="$(kubectl get "${STACK_CHART_OBJECT_KINDS}" \
    -l "${selector}" --ignore-not-found -o name)"; then
    log "ERROR: cannot read the cluster-scoped objects of the stack's charts (kubectl's error is above)."
    exit 1
  fi
}

# ---------------------------------------------------------------------------
# wait_for_stack_crs_gone <namespace> — Wait until no CR of the stack's
# namespaced CRDs (STACK_CRD_GROUPS) in <namespace> is still being reaped,
# bounded by TEARDOWN_TIMEOUT.
#
# Run after steps 1 and 2 and before step 3. `kubectl delete controlplane --wait`
# returns when the ControlPlane is gone. Its finalizer deletes the co-located
# Keystone and waits for it first, so the Keystone and the backup PushSecrets
# its openbao-finalizer purges are finalized by then. Garbage collection reaps
# the other children afterwards (the SecretStore, the dedicated Barbican OpenBao
# instance, the RabbitmqCluster), and the Keystone too when the finalizer gave
# up on it at its deadline; their finalizers need the operators step 3
# uninstalls. Without this wait a slow finalizer loses its controller and holds
# the namespace in Terminating for good.
#
# A CR is being reaped while it carries a deletionTimestamp, or while one of its
# owners of a stack kind is gone and garbage collection has yet to mark it.
# Nothing else is waited on, because nothing deletes it before step 3: the
# Gateway of the base overlay, which step 3 deletes, and the eso-tenant
# Certificate and SecretStore hack/deploy-infra.sh applies on the path without a
# ControlPlane, which the namespace delete of step 7 takes. An owner of another
# kind (a core object, a cluster-scoped one) is not read and counts as present.
# Cluster-scoped CRDs are skipped: kubectl ignores -n for them and would report
# the GatewayClass step 3 deletes.
#
# One kubectl call reads every kind per pass. A read that fails counts as a pass
# with objects left, and the wait names kubectl's error if it runs out, so an
# unreadable cluster never passes for a reaped namespace. A CRD deleted after
# the listing fails every read the same way; nothing in steps 1 and 2 deletes
# one. Exits 1 when the CRDs cannot be listed or the wait runs out, naming what
# is left.
# ---------------------------------------------------------------------------
wait_for_stack_crs_gone() {
  local namespace="$1" crds kinds owner_kinds json left errfile
  log "Waiting for the stack CRs in ${namespace} to be finalized..."
  if ! crds="$(kubectl get crd -o custom-columns=NAME:.metadata.name,SCOPE:.spec.scope,KIND:.spec.names.kind --no-headers)"; then
    log "ERROR: cannot read the scope of the cluster's CRDs (kubectl's error is above)."
    exit 1
  fi
  # "<plural>.<group> <Kind>", one namespaced stack CRD per line.
  crds="$(awk '$2 == "Namespaced" { print $1, $3 }' <<<"${crds}" | grep -E "^[^ ]+\.(${STACK_CRD_GROUPS}) " || true)"
  if [[ -z "${crds}" ]]; then
    return 0
  fi
  kinds="$(cut -d' ' -f1 <<<"${crds}" | paste -sd, -)"
  # "<group>/<Kind>", the form an ownerReference names its owner's kind in.
  owner_kinds="$(awk '{ sub(/^[^.]*\./, "", $1); print $1 "/" $2 }' <<<"${crds}" | paste -sd, -)"

  # The objects being reaped, as <Kind>/<name>. Chained selects rather than
  # `and`: on the right side of `and`, yq loses a variable read inside any_c.
  # shellcheck disable=SC2016 # $live, $kinds, $k and $u are yq variables
  local reaped='[.items[].metadata.uid] as $live |
    (strenv(OWNER_KINDS) | split(",")) as $kinds |
    .items[] |
    select(.metadata.deletionTimestamp != null or
      ([(.metadata.ownerReferences // [])[] |
        select(((.apiVersion | sub("/[^/]*$"; "")) + "/" + .kind) as $k | $kinds | any_c(. == $k)) |
        select(.uid as $u | $live | any_c(. == $u) | not)] | length > 0)) |
    .kind + "/" + .metadata.name'

  errfile="$(mktemp)"
  local deadline=$((SECONDS + TEARDOWN_TIMEOUT))
  while :; do
    if json="$(kubectl get "${kinds}" -n "${namespace}" -o json 2>"${errfile}")"; then
      left="$(OWNER_KINDS="${owner_kinds}" yq -r "${reaped}" <<<"${json}")" ||
        left="yq cannot read them (its error is above)"
    else
      left="cannot read them: $(head -n 1 "${errfile}")"
    fi
    if [[ -z "${left}" ]]; then
      rm -f "${errfile}"
      return 0
    fi
    if (( SECONDS >= deadline )); then
      rm -f "${errfile}"
      log "ERROR: the stack CRs in ${namespace} were not finalized within ${TEARDOWN_TIMEOUT}s:"
      local line
      while IFS= read -r line; do
        [[ -n "${line}" ]] && log "         ${line}"
      done <<<"${left}"
      log "       Their operators are still installed; look at each object's conditions before rerunning."
      exit 1
    fi
    sleep 5
  done
}

# ---------------------------------------------------------------------------
# wait_for_nfs_pods_gone — Wait until no pod in any namespace mounts an inline
# volume of csi-driver-nfs (driver nfs.csi.k8s.io) and no PersistentVolume of
# that driver is Bound, bounded by TEARDOWN_TIMEOUT.
#
# The kubelet unmounts such a volume through the driver's node plugin,
# csi-nfs-node. A pod that garbage collection is still terminating when the
# plugin goes stays in Terminating with its NFS mount on the node, and holds its
# namespace until step 7 runs out of time. The cinder-operator mounts its
# shares as inline `csi:` volumes. A pod that mounts a share through a claim,
# as the probe of the nfs-health suite does, is found by the PersistentVolume
# its claim binds instead: the teardown never deletes that namespace, so the
# pod would keep its mount and the PersistentVolume would stay behind. The
# match is by driver name across the cluster, so teardown_nfs calls this only
# while the stack's own HelmRelease csi-driver-nfs exists.
#
# The loop is the one of wait_for_stack_crs_gone: a read that fails counts as
# pods left, and so does a yq that cannot read the answer, so an unreadable
# cluster never passes for one without mounts. Exits 1 when the wait runs out,
# naming each pod as <namespace>/<name> and each PersistentVolume as
# pv/<name> (claim <namespace>/<name>).
# ---------------------------------------------------------------------------
wait_for_nfs_pods_gone() {
  local json pvs left errfile
  log "Waiting until no pod or bound PersistentVolume uses an nfs.csi.k8s.io volume..."
  local mounting='.items[] | select([(.spec.volumes // [])[] | select(.csi.driver == "nfs.csi.k8s.io")] | length > 0) | .metadata.namespace + "/" + .metadata.name'
  local bound='.items[] | select(.spec.csi.driver == "nfs.csi.k8s.io" and .status.phase == "Bound") | "pv/" + .metadata.name + " (claim " + .spec.claimRef.namespace + "/" + .spec.claimRef.name + ")"'

  errfile="$(mktemp)"
  local deadline=$((SECONDS + TEARDOWN_TIMEOUT))
  while :; do
    if json="$(kubectl get pods -A -o json 2>"${errfile}")" &&
      pvs="$(kubectl get pv -o json 2>"${errfile}")"; then
      left="$(yq -r "${mounting}" <<<"${json}" && yq -r "${bound}" <<<"${pvs}")" ||
        left="yq cannot read them (its error is above)"
    else
      left="cannot read them: $(head -n 1 "${errfile}")"
    fi
    if [[ -z "${left}" ]]; then
      rm -f "${errfile}"
      return 0
    fi
    if (( SECONDS >= deadline )); then
      rm -f "${errfile}"
      log "ERROR: pods or bound PersistentVolumes still use an nfs.csi.k8s.io volume after ${TEARDOWN_TIMEOUT}s:"
      local line
      while IFS= read -r line; do
        [[ -n "${line}" ]] && log "         ${line}"
      done <<<"${left}"
      log "       The csi-driver-nfs node plugin has to unmount them before it is removed; delete what owns these pods and claims and rerun."
      exit 1
    fi
    sleep 5
  done
}

# ---------------------------------------------------------------------------
# delete_where_crd_exists CRD WHAT ARGS... — delete_and_wait WHAT CRD ARGS...
# when CRD is installed, and nothing otherwise: an operator that was never
# installed, or is gone, has no objects left to delete.
# ---------------------------------------------------------------------------
delete_where_crd_exists() {
  local crd="$1" what="$2"
  shift 2
  if kubectl get crd "${crd}" >/dev/null 2>&1; then
    delete_and_wait "${what}" "${crd}" "$@"
  fi
}

# ---------------------------------------------------------------------------
# teardown_chaos_mesh — The first step of teardown_external_cluster, before
# step 0: release every Chaos Mesh fault and remove the overlay's chaos-mesh/,
# which hack/deploy-infra.sh applies under WITH_CHAOS_MESH=true. It runs while
# chaos-controller-manager, chaos-daemon and the helm-controller still run. A
# no-op unless ${OVERLAY_ROOT}/chaos-mesh/kustomization.yaml exists.
#   1. the scope of the cluster's chaos-mesh.org CRDs. A read that fails exits
#      1 before anything is deleted; without such a CRD, 2 and 3 are skipped;
#   2. the schedules and workflows, first, because they create experiments;
#   3. the experiments, every other namespaced kind of the group, then its
#      cluster-scoped kinds. An experiment keeps its finalizer until
#      chaos-controller-manager has released its fault. The per-pod records
#      podnetworkchaos, podiochaos and podhttpchaos are left out: the
#      controller writes them while it releases a fault, they carry no
#      finalizer, and step 8 removes them with their CRDs. 2 and 3 run in a
#      subshell, so a delete that runs out adds the hint before the exit: a
#      finalizer removed by hand would leave the fault injected;
#   4. the overlay without its Namespace: the HelmRepository, the HelmRelease
#      (its finalizer has the helm-controller uninstall the chart) and the
#      DaemonSet chaos-mesh-modules. The Namespace holds Helm's release
#      Secret, which the uninstall needs, so step 7 deletes it with the other
#      stack namespaces. A render that fails exits 1 before the delete.
# The modules chaos-mesh-modules loaded stay on the nodes until they reboot.
# ---------------------------------------------------------------------------
teardown_chaos_mesh() {
  if [[ ! -f "${OVERLAY_ROOT}/chaos-mesh/kustomization.yaml" ]]; then
    return 0
  fi

  # 1. "<plural>.chaos-mesh.org <scope>", one CRD of the group per line.
  local crds
  if ! crds="$(kubectl get crd -o custom-columns=NAME:.metadata.name,SCOPE:.spec.scope --no-headers)"; then
    log "ERROR: cannot read the scope of the cluster's CRDs (kubectl's error is above)."
    exit 1
  fi
  crds="$(awk '$1 ~ /\.chaos-mesh\.org$/ { print $1, $2 }' <<<"${crds}")"

  local creators experiments cluster_scoped
  creators="$(awk '$1 ~ /^(schedules|workflows)\./ { print $1 }' <<<"${crds}" | paste -sd, -)"
  experiments="$(awk '$2 == "Namespaced" && $1 !~ /^(schedules|workflows|podnetworkchaos|podiochaos|podhttpchaos)\./ { print $1 }' \
    <<<"${crds}" | paste -sd, -)"
  cluster_scoped="$(awk '$2 != "Namespaced" { print $1 }' <<<"${crds}" | paste -sd, -)"

  # 2. and 3., in a subshell, so a timeout can add the hint before the exit.
  if ! (
    if [[ -n "${creators}" ]]; then
      delete_and_wait "the Chaos Mesh schedules and workflows" "${creators}" --all -A
    fi
    if [[ -n "${experiments}" ]]; then
      delete_and_wait "the Chaos Mesh experiments" "${experiments}" --all -A
    fi
    if [[ -n "${cluster_scoped}" ]]; then
      delete_and_wait "the cluster-scoped Chaos Mesh objects" "${cluster_scoped}" --all
    fi
  ); then
    log "A Chaos Mesh experiment keeps its finalizer until chaos-controller-manager has released its fault. Read 'kubectl logs -n chaos-mesh deployment/chaos-controller-manager' before rerunning; do not remove the finalizer by hand, the fault would stay injected."
    exit 1
  fi

  # 4. The overlay, its Namespace aside.
  local render
  if ! render="$(kubectl kustomize "${OVERLAY_ROOT}/chaos-mesh")"; then
    log "ERROR: cannot render ${OVERLAY_ROOT}/chaos-mesh (kustomize's error is above)."
    exit 1
  fi
  printf '%s\n' "${render}" |
    yq 'select(.kind != "Namespace")' |
    delete_and_wait "the Chaos Mesh overlay (HelmRelease, HelmRepository and module loader)" -f -
}

# ---------------------------------------------------------------------------
# teardown_dizzy_soak — Remove the dizzy soak that `make dizzy-soak-start`
# started from ${OVERLAY_ROOT}/dizzy-soak, after the Chaos Mesh step and
# before step 0: a running soak keeps servers on the lab hypervisors, and a
# NovaCompute keeps its finalizer while Nova counts a server on its nodes. A
# no-op unless ${OVERLAY_ROOT}/dizzy-soak/kustomization.yaml exists.
#   1. the Job dizzy-soak and the pod dizzy-soak-reader, in the foreground.
#      The Job's pod gets TERM, so dizzy removes its resources and the runner
#      writes its report, within its grace period of 540 seconds, below
#      TEARDOWN_TIMEOUT;
#   2. the render of the directory: the K-ORC identity in openstack, which
#      K-ORC deletes in Keystone while it still runs, the ServiceAccount, the
#      ClusterRole and ClusterRoleBinding, and the claim with the reports. A
#      render that fails exits 1 before the delete.
# The Secrets and ConfigMaps start wrote go with their namespaces in step 7.
# ---------------------------------------------------------------------------
teardown_dizzy_soak() {
  if [[ ! -f "${OVERLAY_ROOT}/dizzy-soak/kustomization.yaml" ]]; then
    return 0
  fi
  delete_and_wait "the dizzy soak Job and reader pod" jobs.batch/dizzy-soak pod/dizzy-soak-reader \
    -n dizzy --cascade=foreground

  local render
  if ! render="$(kubectl kustomize "${OVERLAY_ROOT}/dizzy-soak")"; then
    log "ERROR: cannot render ${OVERLAY_ROOT}/dizzy-soak (kustomize's error is above)."
    exit 1
  fi
  printf '%s\n' "${render}" | delete_and_wait "the dizzy soak objects" -f -
}

# ---------------------------------------------------------------------------
# teardown_hypervisors — Step 0 of teardown_external_cluster: remove the lab
# hypervisors (deploy/lab/metal-stack/hypervisor and hypervisor-fixtures,
# applied by hand) while the ControlPlane, the operators, K-ORC,
# openstack-hypervisor-operator and kvm-node-agent still run. A no-op unless
# ${OVERLAY_ROOT}/hypervisor/kustomization.yaml exists. Each kind is deleted
# only where its CRD exists, like the ControlPlane in step 1:
#   1. the NovaComputes, NeutronMetadataAgents and OVNChassis in openstack. A
#      NovaCompute holds nova.openstack.c5c3.io/compute-drain until Nova counts
#      no server on its nodes, so a pool that still holds servers outlives the
#      wait, and the teardown exits 1 with a hint to delete them;
#   2. the Hypervisors, Evictions and Migrations of kvm.cloud.sap. Nodes own
#      them, so garbage collection never reaps them;
#   3. the fixtures, when the overlay has them. Keystone refuses to delete an
#      enabled domain and K-ORC does not disable it, so a domain that exists
#      is disabled first; the delete runs either way, so a rerun after a
#      partial delete still takes the image and the flavor. A read of the
#      domain that fails exits 1 before the delete: K-ORC applies no spec
#      change to an object that is being deleted, so a domain deleted while
#      enabled could never be disabled;
#   4. the hypervisor overlay, the two operators and the migration port
#      reservation (deploy/lab/metal-stack/migration-ports) with it;
#   5. the Deployment and PodDisruptionBudget maint-<node> the hypervisor
#      operator leaves in kube-system for every node. Its lifecycle controller
#      recreates them while it runs, hence after step 4;
#   6. the four node labels and the aggregates annotation the lab sets by
#      hand, and the custom-traits annotation an older lab still carries.
# Node state under /var/lib/nova, /var/lib/libvirt and /etc/pki stays, and so
# does the reservation in net.ipv4.ip_local_reserved_ports until the node
# reboots.
# ---------------------------------------------------------------------------
teardown_hypervisors() {
  if [[ ! -f "${OVERLAY_ROOT}/hypervisor/kustomization.yaml" ]]; then
    return 0
  fi

  # 1. The node layer. The NovaCompute delete runs in a subshell, so its
  # timeout can add the hint before the exit.
  if ! (delete_where_crd_exists novacomputes.nova.openstack.c5c3.io \
    "the NovaComputes in openstack" --all -n openstack); then
    log "Delete the servers on the lab hypervisors first (openstack server list --all-projects)."
    exit 1
  fi
  delete_where_crd_exists neutronmetadataagents.neutron.openstack.c5c3.io \
    "the NeutronMetadataAgents in openstack" --all -n openstack
  delete_where_crd_exists ovnchassis.ovn.openstack.c5c3.io "the OVNChassis in openstack" --all -n openstack

  # 2. What the two operators keep per node.
  delete_where_crd_exists hypervisors.kvm.cloud.sap "the Hypervisors" --all
  delete_where_crd_exists evictions.kvm.cloud.sap "the Evictions" --all
  delete_where_crd_exists migrations.kvm.cloud.sap "the Migrations" --all -A

  # 3. The fixtures, through the ControlPlane's credential. Only the disable
  # needs the domain; a missing domain kind means there is none.
  if [[ -f "${OVERLAY_ROOT}/hypervisor-fixtures/kustomization.yaml" ]]; then
    local domain
    if ! domain="$(kubectl get domains.openstack.k-orc.cloud hvo-cc3test -n openstack \
      --ignore-not-found -o name 2>&1)"; then
      if [[ "${domain}" != *"doesn't have a resource type"* ]]; then
        log "ERROR: cannot read the fixtures' domain hvo-cc3test:"
        log "         ${domain}"
        exit 1
      fi
      domain=""
    fi
    # kubectl may write warnings beside an empty answer; only the name counts.
    if grep -qxF 'domain.openstack.k-orc.cloud/hvo-cc3test' <<<"${domain}"; then
      log "Disabling the fixtures' domain hvo-cc3test..."
      kubectl patch domains.openstack.k-orc.cloud hvo-cc3test -n openstack --type merge \
        -p '{"spec":{"resource":{"enabled":false}}}' >/dev/null
      local out
      if ! out="$(kubectl wait domains.openstack.k-orc.cloud/hvo-cc3test -n openstack \
        --for=jsonpath='{.status.resource.enabled}'=false --timeout="${TEARDOWN_TIMEOUT}s" 2>&1)"; then
        log "ERROR: K-ORC did not disable the domain hvo-cc3test within ${TEARDOWN_TIMEOUT}s:"
        log "         ${out}"
        exit 1
      fi
    fi
    delete_and_wait "the hypervisor fixtures" -k "${OVERLAY_ROOT}/hypervisor-fixtures"
  fi

  # 4. The overlay.
  delete_and_wait "the hypervisor overlay" -k "${OVERLAY_ROOT}/hypervisor"

  # 5. The maintenance objects in kube-system, by name, in one delete.
  local nodes node names=()
  if ! nodes="$(kubectl get nodes -o name)"; then
    log "ERROR: cannot list the nodes (kubectl's error is above)."
    exit 1
  fi
  while IFS= read -r node; do
    [[ -n "${node}" ]] || continue
    names+=("maint-${node#node/}")
  done <<<"${nodes}"
  if [[ ${#names[@]} -gt 0 ]]; then
    delete_and_wait "the maintenance objects in kube-system" \
      deployment,poddisruptionbudget "${names[@]}" -n kube-system
  fi

  # 6. The node labels and the annotations.
  log "Removing the hypervisor labels and annotations from every node..."
  kubectl label nodes --all openstack.c5c3.io/chassis- openstack.c5c3.io/nova-compute-pool- \
    nova.openstack.cloud.sap/virt-driver- cobaltcore.cloud.sap/node-hypervisor-lifecycle- >/dev/null
  kubectl annotate nodes --all nova.openstack.cloud.sap/custom-traits- \
    nova.openstack.cloud.sap/aggregates- >/dev/null
}

# ---------------------------------------------------------------------------
# teardown_nfs — The end of step 2 of teardown_external_cluster: remove the NFS
# stack of the overlay's nfs/, which hack/deploy-infra.sh applies under
# WITH_NFS=true. It runs once the ControlPlane and its Cinder are gone and
# while the helm-controller, which uninstalls the chart, still runs (step 5
# removes it). A no-op unless ${OVERLAY_ROOT}/nfs/kustomization.yaml exists.
#   1. while the HelmRelease kube-system/csi-driver-nfs exists,
#      wait_for_nfs_pods_gone, so csi-nfs-node is still there to unmount every
#      inline nfs.csi.k8s.io volume and every one mounted through a claim.
#      Without that release the stack runs no NFS CSI driver: a deploy without
#      WITH_NFS=true, a cluster whose driver the platform runs (the deploy
#      refuses WITH_NFS=true there), or a second run. The pods and claims of a
#      driver the platform runs are not the teardown's to wait for. A read of
#      the release that fails exits 1 before anything is deleted;
#   2. the overlay: the HelmRepository, the HelmRelease csi-driver-nfs (its
#      finalizer has the helm-controller uninstall the chart), the NFS server
#      with its Service and claim, and the DaemonSet nfs-client-modules. The
#      release lives in kube-system, which step 7 leaves alone, so without this
#      delete the chart would stay on the cluster for good;
#   3. the NetworkPolicy of the overlay's nfs/client-policy.yaml, when the
#      overlay ships one, after the server it guarded. The deploy applies that
#      template with the node network filled in; a delete needs only its name;
#   4. the CSIDriver the release created, selected by the two labels the
#      helm-controller sets on every object of a release, so a CSIDriver the
#      platform ships is never named. After a clean uninstall it finds nothing.
# The kernel modules the overlay's pods loaded (nfs, nfsv4 and their
# dependencies) stay on the nodes until they reboot.
# ---------------------------------------------------------------------------
teardown_nfs() {
  if [[ ! -f "${OVERLAY_ROOT}/nfs/kustomization.yaml" ]]; then
    return 0
  fi
  # A missing HelmRelease kind means there is no release.
  local release
  if ! release="$(kubectl get helmrelease csi-driver-nfs -n kube-system \
    --ignore-not-found -o name 2>&1)"; then
    if [[ "${release}" != *"doesn't have a resource type"* ]]; then
      log "ERROR: cannot read the HelmRelease kube-system/csi-driver-nfs:"
      log "         ${release}"
      exit 1
    fi
    release=""
  fi
  # kubectl may write warnings beside an empty answer; only the name counts.
  if grep -qxF 'helmrelease.helm.toolkit.fluxcd.io/csi-driver-nfs' <<<"${release}"; then
    wait_for_nfs_pods_gone
  fi
  delete_and_wait "the NFS overlay" -k "${OVERLAY_ROOT}/nfs"
  if [[ -f "${OVERLAY_ROOT}/nfs/client-policy.yaml" ]]; then
    delete_and_wait "the NFS client policy" -f "${OVERLAY_ROOT}/nfs/client-policy.yaml"
  fi
  delete_and_wait "the CSIDriver of csi-driver-nfs" csidriver \
    -l 'helm.toolkit.fluxcd.io/name=csi-driver-nfs,helm.toolkit.fluxcd.io/namespace=kube-system'
}

# ---------------------------------------------------------------------------
# teardown_prometheus — Step 3 of teardown_external_cluster, after the base
# overlay: remove kube-prometheus-stack, which hack/deploy-infra.sh applies
# under WITH_PROMETHEUS=true, while the helm-controller still runs (step 5
# removes it).
#   1. the HelmRelease kube-prometheus-stack, by the file of the kind overlay,
#      which every Prometheus overlay inherits: the overlay's
#      configMapGenerator reads a dashboard the deploy stages, so `-k` does not
#      render on a fresh checkout. Its finalizer has the helm-controller
#      uninstall the chart, so the delete returns once the Prometheus
#      StatefulSet is gone;
#   2. the claims in monitoring, which Helm and the Prometheus Operator leave
#      behind: the claim of a volume claim template belongs to no release.
#      Where the default class has the reclaim policy Delete, the metrics go
#      with it;
#   3. the Service and the Endpoints kube-prometheus-stack-kubelet in
#      kube-system, which the Prometheus Operator writes for its kubelet
#      ServiceMonitor and no uninstall removes. Not earlier: the operator
#      writes both again while it runs.
# Not gated on the overlay: on a cluster deployed without WITH_PROMETHEUS=true
# the three deletes find nothing. A release delete that outlives
# TEARDOWN_TIMEOUT exits 1 before any claim is deleted. Step 7 deletes the
# namespace monitoring with the dashboard ConfigMaps.
# ---------------------------------------------------------------------------
teardown_prometheus() {
  delete_and_wait "the kube-prometheus-stack release" -f "${REPO_ROOT}/deploy/kind/prometheus/release.yaml"
  delete_and_wait "the PVCs in monitoring" pvc --all -n monitoring
  delete_and_wait "the kubelet Service and Endpoints the Prometheus Operator left in kube-system" \
    service,endpoints kube-prometheus-stack-kubelet -n kube-system
}

# ---------------------------------------------------------------------------
# teardown_dizzy — The end of step 3 of teardown_external_cluster: remove the
# dizzy metrics stack of the overlay's dizzy/, which hack/deploy-infra.sh
# applies under WITH_DIZZY=true, while the helm-controller and the
# source-controller still run (step 5 removes them). A no-op unless
# ${OVERLAY_ROOT}/dizzy/kustomization.yaml exists.
#   1. the HelmReleases dizzy-victoria-metrics and dizzy-grafana. Their
#      finalizer has the helm-controller uninstall both charts, so the delete
#      returns once the StatefulSet of VictoriaMetrics is gone;
#   2. the HelmRepositories victoria-metrics and grafana in flux-system;
#   3. the claims in dizzy, which Helm leaves behind: the claim of a volume
#      claim template belongs to no release. Not earlier, while the pod still
#      mounts it.
# The objects are named, not rendered: the overlay's configMapGenerator reads
# dashboards the deploy stages, so `kubectl kustomize` fails on a checkout
# without them, as for the kube-prometheus-stack release, which
# teardown_prometheus deletes by file before this function runs. Step 7
# deletes the namespace dizzy with the HTTPRoute and the ConfigMap.
# ---------------------------------------------------------------------------
teardown_dizzy() {
  if [[ ! -f "${OVERLAY_ROOT}/dizzy/kustomization.yaml" ]]; then
    return 0
  fi
  delete_and_wait "the dizzy HelmReleases" helmreleases.helm.toolkit.fluxcd.io \
    dizzy-victoria-metrics dizzy-grafana -n dizzy
  delete_and_wait "the dizzy HelmRepositories" helmrepositories.source.toolkit.fluxcd.io \
    victoria-metrics grafana -n flux-system
  delete_and_wait "the PVCs in dizzy" pvc --all -n dizzy
}

# ---------------------------------------------------------------------------
# teardown_external_cluster — Remove the stack from the current context's cluster.
#
# The order makes every finalizer run while the controller that clears it still
# exists. First, when the overlay has chaos-mesh/, Chaos Mesh goes
# (teardown_chaos_mesh): its schedules and workflows, then its experiments,
# while chaos-controller-manager still releases their faults, then the overlay
# without its Namespace, so the helm-controller uninstalls the chart. Next,
# when the overlay has dizzy-soak/, the dizzy soak goes (teardown_dizzy_soak):
# its Job and reader pod, which ends a running soak with its report, then the
# render of dizzy-soak/ (the K-ORC identity, the RBAC and the claim). Then:
#   0. the lab hypervisors, when the overlay has them (teardown_hypervisors),
#      while everything they need still runs;
#   1. every ControlPlane in openstack, while the c5c3-operator runs, so it
#      reaps its children, then every OVNCentral there: the quick start's
#      central is referenced, not owned, and its database pods would hold
#      their PVCs through step 4;
#   2. the infrastructure overlay (MariaDB, Memcached, Garage, the OpenBao
#      instance and tenant, the ExternalSecrets, Certificates and issuers) and
#      the opt-in message bus, while their operators run; the proving OpenBao
#      instance is switched to deletionPolicy DeletePVCs first, because the
#      openbao-operator cannot finalize it under the default Retain; then a
#      wait until no CR of the stack's namespaced CRDs in openstack is still
#      being reaped, since what a ControlPlane owned is finalized by an
#      operator step 3 removes (the ControlPlane delete of step 1 returns when
#      the ControlPlane is gone, not its children); then, when the overlay has
#      nfs/, the NFS stack (teardown_nfs): while the HelmRelease
#      kube-system/csi-driver-nfs exists, a wait until no pod mounts an
#      inline nfs.csi.k8s.io volume and no PersistentVolume of that driver is
#      Bound, then the overlay with that HelmRelease, while the
#      helm-controller can still uninstall its chart, the NetworkPolicy of
#      nfs/client-policy.yaml and the CSIDriver that release created. The
#      modules its pods loaded stay on the nodes until they reboot;
#   3. the base overlay without its Namespaces and FluxInstance, after resuming
#      what was suspended, so the helm-controller uninstalls every chart and the
#      Flux Kustomizations prune K-ORC and the RabbitMQ operator; the Gateway and
#      GatewayClass go first, while Envoy Gateway still clears their finalizer,
#      and the opt-in kube-prometheus-stack goes after the rest
#      (teardown_prometheus): its HelmRelease, so the helm-controller
#      uninstalls the chart, the claims in monitoring and the Service and
#      Endpoints kube-prometheus-stack-kubelet the Prometheus Operator left in
#      kube-system; then, when the overlay has dizzy/, the dizzy stack
#      (teardown_dizzy): its two HelmReleases, so the helm-controller
#      uninstalls both charts, its two HelmRepositories and its claims;
#   4. the PVCs in shared-services and openstack, which Helm and the operators
#      leave behind. Not earlier: the openbao-operator chart's admission policy
#      denies deleting its managed PVCs to everyone but the operator until the
#      chart is uninstalled;
#   5. the FluxInstance, so the flux-operator uninstalls the toolkit;
#   6. the flux-system namespace and the flux-operator's cluster-scoped RBAC;
#   7. the stack namespaces (stack_namespaces), by name, chaos-mesh and dizzy
#      among them, then the objects of STACK_CHART_OBJECT_KINDS whose
#      helm.toolkit.fluxcd.io/namespace label names one of them: Helm hook
#      objects, which no uninstall removes, then the two Leases
#      cert-manager's leader election leaves in kube-system, once no
#      cert-manager pod is left to renew them;
#   8. the CRDs of STACK_CRD_GROUPS, chaos-mesh.org among them.
# It ends with the count of stack CRDs, stack namespaces and cluster-scoped
# chart objects still present, which must all be zero. Every delete ignores
# absence, so a second run finds nothing and exits 0; a wait that runs out
# exits 1 (delete_and_wait). In kube-system only the maint-<node> objects of
# step 0, the csi-driver-nfs HelmRelease of step 2 with its chart, the
# kubelet Service and Endpoints of step 3, and the two cert-manager Leases of
# step 7 are deleted; the platform's namespaces and CRDs are never named.
# ---------------------------------------------------------------------------
teardown_external_cluster() {
  local cmd
  for cmd in kubectl yq; do
    if ! command -v "${cmd}" >/dev/null 2>&1; then
      log "ERROR: '${cmd}' is not installed or not in PATH."
      exit 1
    fi
  done
  # mikefarah/yq v4.40.1 or newer, the floor hack/deploy-infra.sh holds too
  # (require_mikefarah_yq): the -r shorthand came with v4.25.3, and the jq
  # wrapper Debian packages under the same name knows neither strenv nor any_c.
  # wait_for_stack_crs_gone would retry either one's error until
  # TEARDOWN_TIMEOUT, after the deletes of steps 0 to 2.
  if [[ "$(YQ_PROBE=1 yq -n -r 'strenv(YQ_PROBE) | tonumber' 2>/dev/null)" != "1" ]]; then
    local yq_version
    yq_version="$(yq --version 2>&1)" || true
    log "ERROR: 'yq' on PATH is not mikefarah/yq v4.40.1 or newer (yq --version: ${yq_version%%$'\n'*})."
    log "       Install a current release from https://github.com/mikefarah/yq; the yq package of Debian and PyPI is a jq wrapper with another expression language."
    exit 1
  fi

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

  # Chaos Mesh, before step 0: every fault is released while its controller
  # runs.
  teardown_chaos_mesh

  # The dizzy soak, before step 0: its servers would keep the NovaComputes.
  teardown_dizzy_soak

  # 0. The lab hypervisors.
  teardown_hypervisors

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
  # What the ControlPlane owned: garbage collection reaps it after step 1's
  # delete has returned, and its finalizers need the operators step 3 removes.
  wait_for_stack_crs_gone openstack
  # The NFS stack, once Cinder no longer mounts its shares and while the
  # helm-controller still runs.
  teardown_nfs

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
  # The opt-in monitoring release, then what its uninstall leaves behind.
  teardown_prometheus
  # The dizzy stack, while the helm-controller can still uninstall its charts.
  teardown_dizzy

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

  # 7. The stack namespaces, flux-system aside: step 6 deleted it, then the
  # cluster-scoped objects their charts left behind. The same list is counted
  # in step 9.
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
  # Read only now: with every stack namespace gone, no HelmRelease is left in
  # one, so an object whose label names one belongs to no release. Before step
  # 3 the same read also finds the objects of every installed chart. Each
  # object is logged before the delete, since no step names it, and the delete
  # comes before step 8, so the CRDs go last.
  local chart_objects=()
  read_stack_chart_objects "${namespaces[@]}"
  while IFS= read -r line; do
    [[ -n "${line}" ]] || continue
    log "  Chart leftover: ${line}"
    chart_objects+=("${line}")
  done <<<"${STACK_CHART_OBJECTS}"
  if [[ ${#chart_objects[@]} -gt 0 ]]; then
    delete_and_wait "the cluster-scoped objects the stack's charts left behind" "${chart_objects[@]}"
  fi
  # The Leases cert-manager's controller and cainjector elect their leader on.
  # The chart keeps them in kube-system (global.leaderElection.namespace), so
  # deleting the cert-manager namespace leaves them, and a cainjector deployed
  # within the lease duration waits for the old holder's Lease before it
  # injects the webhook's CA. Not earlier: a pod that still runs writes its
  # Lease again when it renews it.
  delete_and_wait "the cert-manager leader election Leases in kube-system" \
    lease cert-manager-cainjector-leader-election cert-manager-controller -n kube-system

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

  # 9. What is left: the stack CRDs, the stack namespaces and the cluster-scoped
  # objects of their charts. A chart object still present is named.
  local crds_left namespaces_left chart_objects_left
  read_stack_crds
  crds_left="$(grep -c . <<<"${STACK_CRDS}" || true)"
  if ! namespaces_left="$(kubectl get namespace "${namespaces[@]}" --ignore-not-found -o name)"; then
    log "ERROR: cannot read the stack namespaces (kubectl's error is above)."
    exit 1
  fi
  namespaces_left="$(grep -c . <<<"${namespaces_left}" || true)"
  read_stack_chart_objects "${namespaces[@]}"
  chart_objects_left="$(grep -c . <<<"${STACK_CHART_OBJECTS}" || true)"
  log "Stack CRDs left: ${crds_left}; stack namespaces left: ${namespaces_left}; cluster-scoped chart objects left: ${chart_objects_left}"
  while IFS= read -r line; do
    [[ -n "${line}" ]] || continue
    log "  Still present: ${line}"
  done <<<"${STACK_CHART_OBJECTS}"
  if [[ "${crds_left}" != "0" || "${namespaces_left}" != "0" || "${chart_objects_left}" != "0" ]]; then
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
