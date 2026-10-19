#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/lab-failover.sh — Run the failover suites of tests/lab-failover/
# (#1393) on the metal-stack lab, against a second ControlPlane beside the
# lab's own: openstack-ha/cp-ha with the Standard profile, its three-member
# OVNCentral cp-ha-ovn and a fake compute. The lab's openstack/controlplane,
# its hypervisors and every other object stay untouched; in the lab's Ceph pool
# volumes and Garage bucket glance-images, which the plane shares, down
# deletes what the plane stored. The script is started by hand, after the
# go-ahead on the shared lab (D4 of #1386).
#
# Subcommands, wrapped by the four make targets lab-failover-<subcommand>:
#   preflight          Read-only checks of the lab: the tools, the context,
#                      the API, the Gardener shoot, Chaos Mesh, the objects
#                      the plane needs, and the free memory requests.
#   up                 preflight, then bring the plane up and write
#                      _output/lab-failover/<run>/versions.txt and
#                      topology.txt. It refuses an existing openstack-ha and
#                      is not resumable: after a failed step, run down.
#   run [scenario...]  Run the suites in order, append a row per suite to
#                      _output/lab-failover/<run>/summary.tsv, and stop at the
#                      first FAIL.
#   down               Delete the plane's volumes and images through its own
#                      APIs, then what up created, the namespace last. A
#                      second down resumes a failed one.
#
# Settings (environment):
#   LAB_FAILOVER_CONTEXT          the kube context of the lab; required.
#   LAB_FAILOVER_GO_AHEAD         the lab's shoot name, .data.shootName of the
#                                 ConfigMap kube-system/shoot-info; required
#                                 by up, run and down.
#   LAB_FAILOVER_MIN_FREE_MEMORY  the floor of free memory requests, <n>Gi or
#                                 <n>Mi (default 38Gi).
#   LAB_FAILOVER_RUN              the run id; up sets the UTC time when it is
#                                 empty, run and down read it from the
#                                 namespace's label lab-failover.c5c3.io/run.
#   LAB_FAILOVER_SCENARIOS        the suites run executes, space-separated
#                                 (default all eleven); arguments of run take
#                                 its place.
#
# Exit codes: 0 on success, 1 on a failed check or step, 2 on a usage error.
#
# Every subcommand pins the context: it writes `kubectl config view --raw
# --minify --flatten --context "$LAB_FAILOVER_CONTEXT"` to a temporary file of
# mode 0600, exports KUBECONFIG to it for every child (kubectl, chainsaw, the
# workload harness), and deletes it on exit. In the namespace openstack the
# script only reads the two Secrets it copies into openstack-ha.
#
# docs/reference/infrastructure/infrastructure-manifests.md#lab-failover-runs
# describes the runs and holds their record.

set -euo pipefail

LF_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LF_REPO_ROOT="$(cd "${LF_SCRIPT_DIR}/.." && pwd)"

# The namespace of the plane, which every function of the failover library
# works in.
NAMESPACE="openstack-ha"
# shellcheck source=tests/lab-failover/lib/failover.sh
. "${LF_REPO_ROOT}/tests/lab-failover/lib/failover.sh"

# Settings.
LAB_FAILOVER_CONTEXT="${LAB_FAILOVER_CONTEXT:-}"
LAB_FAILOVER_GO_AHEAD="${LAB_FAILOVER_GO_AHEAD:-}"
LAB_FAILOVER_MIN_FREE_MEMORY="${LAB_FAILOVER_MIN_FREE_MEMORY:-38Gi}"
LAB_FAILOVER_RUN="${LAB_FAILOVER_RUN:-}"
LAB_FAILOVER_SCENARIOS="${LAB_FAILOVER_SCENARIOS:-}"

# The eleven suites, in the order run executes them.
LF_SCENARIOS=(
  galera-member-loss
  galera-primary-loss
  rabbitmq-node-loss
  rabbitmq-minority-partition
  ovn-nb-leader-kill
  ovn-sb-leader-kill
  ovn-nb-member-loss
  ovn-sb-member-loss
  ovn-nb-quorum-loss
  ovn-sb-quorum-loss
  api-eviction-drain
)

# The paths, the labels and the waits in seconds. Plain variables, so the unit
# test can point them elsewhere or set a wait to 0 after sourcing.
LF_SUITES_DIR="${LF_REPO_ROOT}/tests/lab-failover"
LF_FIXTURES_DIR="${LF_SUITES_DIR}/fixtures"
LF_OUTPUT_DIR="${LF_REPO_ROOT}/_output/lab-failover"
LF_DISCOVER_HOSTS="${LF_REPO_ROOT}/tests/e2e/nova/discover-hosts.sh"
LF_RUN_LABEL="lab-failover.c5c3.io/run"
LF_COPIED_LABEL="lab-failover.c5c3.io/copied"
LF_COPIED_SECRETS="garage-s3-credentials ceph-client-cinder"
LF_TENANT_NAMESPACE="openbao-operator-system"
LF_WAIT_OVN=600
LF_WAIT_INFRA=1500
LF_WAIT_TENANT_STORE=600
LF_WAIT_MARIADB=300
LF_WAIT_KEYSTONE=900
LF_WAIT_NOVA=1800
LF_WAIT_DB_SYNC=120
LF_WAIT_SEED=600
LF_WAIT_PURGE=600
LF_WAIT_COMPUTE=300
LF_WAIT_READY=900
LF_WAIT_RUN_READY=60
LF_WAIT_GONE=300
LF_WAIT_NAMESPACE_GONE=600

# Set by lf_kubeconfig, lf_preflight and up.
LF_KUBECONFIG=""
LF_SHOOT=""
LF_FREE_BEFORE=""

# ---------------------------------------------------------------------------
# lf_usage — Print the subcommands and settings to stderr and exit 2.
# ---------------------------------------------------------------------------
lf_usage() {
  cat >&2 <<EOF
Usage: hack/lab-failover.sh <subcommand>

Subcommands:
  preflight            Read-only checks of the lab.
  up                   preflight, then bring up ControlPlane openstack-ha/cp-ha.
  run [scenario...]    Run the failover suites (default all eleven).
  down                 Delete the plane and the namespace openstack-ha.

Settings (environment):
  LAB_FAILOVER_CONTEXT          kube context of the lab (required).
  LAB_FAILOVER_GO_AHEAD         the lab's shoot name, required by up, run and down.
  LAB_FAILOVER_MIN_FREE_MEMORY  floor of free memory requests, <n>Gi or <n>Mi
                                (default 38Gi).
  LAB_FAILOVER_RUN              run id (default: up sets the UTC time; run and
                                down read the namespace's label).
  LAB_FAILOVER_SCENARIOS        suites of run, space-separated (default all).
EOF
  exit 2
}

# lf_log <message> — A progress line with the UTC time.
lf_log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

# lf_fail <message> — An ERROR line, then exit 1.
lf_fail() {
  echo "ERROR: $*"
  exit 1
}

# lf_check_fail <message> — A FAIL line of a check, then exit 1.
lf_check_fail() {
  echo "FAIL: $*"
  exit 1
}

# lf_require <command...> — Each command is on PATH, or a FAIL line names the
# missing ones.
lf_require() {
  local cmd missing=""
  for cmd in "$@"; do
    command -v "$cmd" >/dev/null 2>&1 || missing="${missing} ${cmd}"
  done
  if [[ -n "$missing" ]]; then
    lf_check_fail "not on PATH:${missing}"
  fi
  echo "OK: $* on PATH"
}

# lf_check_settings — The usage errors of the settings, before any call.
lf_check_settings() {
  if [[ -z "$LAB_FAILOVER_CONTEXT" ]]; then
    echo "ERROR: LAB_FAILOVER_CONTEXT is not set; name the kube context of the lab" >&2
    exit 2
  fi
  if [[ ! "$LAB_FAILOVER_MIN_FREE_MEMORY" =~ ^[0-9]+(Gi|Mi)$ ]]; then
    echo "ERROR: LAB_FAILOVER_MIN_FREE_MEMORY must be <n>Gi or <n>Mi, such as 38Gi, not '${LAB_FAILOVER_MIN_FREE_MEMORY}'" >&2
    exit 2
  fi
  if [[ -n "$LAB_FAILOVER_RUN" && ! "$LAB_FAILOVER_RUN" =~ ^[A-Za-z0-9]([A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?$ ]]; then
    echo "ERROR: LAB_FAILOVER_RUN must be a label value such as 20261019T120000Z, not '${LAB_FAILOVER_RUN}'" >&2
    exit 2
  fi
}

# lf_kubeconfig — Pin the context: refuse a kind context before any call,
# write the context's minified kubeconfig to a mode-0600 temporary file, and
# export KUBECONFIG to it.
lf_kubeconfig() {
  local out
  if [[ "$LAB_FAILOVER_CONTEXT" == kind-* ]]; then
    lf_check_fail "context ${LAB_FAILOVER_CONTEXT} is a kind cluster; the failover suites run on the lab only"
  fi
  LF_KUBECONFIG="$(mktemp "${TMPDIR:-/tmp}/lab-failover-kubeconfig.XXXXXX")"
  chmod 600 "$LF_KUBECONFIG"
  trap 'rm -f "${LF_KUBECONFIG:-}"' EXIT
  if ! out="$(kubectl config view --raw --minify --flatten --context "$LAB_FAILOVER_CONTEXT" 2>&1 >"$LF_KUBECONFIG")"; then
    lf_check_fail "context ${LAB_FAILOVER_CONTEXT} is not in the kubeconfig: $(lf_first_line "$out")"
  fi
  export KUBECONFIG="$LF_KUBECONFIG"
  echo "OK: context ${LAB_FAILOVER_CONTEXT}"
}

# lf_read_shoot — Set LF_SHOOT to .data.shootName of the ConfigMap
# kube-system/shoot-info, which every Gardener shoot carries.
lf_read_shoot() {
  local out
  if ! out="$(kubectl get configmap shoot-info -n kube-system -o jsonpath='{.data.shootName}' 2>&1)"; then
    if [[ "$out" == *"(NotFound)"* ]]; then
      lf_check_fail "not a Gardener shoot: no ConfigMap kube-system/shoot-info"
    fi
    lf_check_fail "cannot read the ConfigMap kube-system/shoot-info: $(lf_first_line "$out")"
  fi
  if [[ -z "$out" ]]; then
    lf_check_fail "the ConfigMap kube-system/shoot-info names no shootName"
  fi
  LF_SHOOT="$out"
  echo "OK: Gardener shoot ${LF_SHOOT}"
}

# lf_go_ahead — LAB_FAILOVER_GO_AHEAD names the shoot of the context.
lf_go_ahead() {
  if [[ -z "$LF_SHOOT" ]]; then
    lf_read_shoot
  fi
  if [[ "$LAB_FAILOVER_GO_AHEAD" != "$LF_SHOOT" ]]; then
    lf_fail "no go-ahead for shoot '${LF_SHOOT}': LAB_FAILOVER_GO_AHEAD is '${LAB_FAILOVER_GO_AHEAD}'; set it to ${LF_SHOOT} once the shared lab is yours"
  fi
  echo "OK: go-ahead for shoot ${LF_SHOOT}"
}

# lf_exists <what> <kubectl get arguments...> — The object exists.
lf_exists() {
  local what="$1" out
  shift
  if ! out="$(kubectl get "$@" -o name 2>&1)"; then
    lf_check_fail "${what} is missing: $(lf_first_line "$out")"
  fi
  echo "OK: ${what}"
}

# lf_field_is <what> <want> <jsonpath> <kubectl get arguments...> — The field
# of the object reads <want>.
lf_field_is() {
  local what="$1" want="$2" path="$3" out
  shift 3
  if ! out="$(kubectl get "$@" -o "jsonpath=${path}" 2>&1)" || [[ "$out" != "$want" ]]; then
    lf_check_fail "${what} is missing or not ${want} (read '$(lf_first_line "$out")')"
  fi
  echo "OK: ${what} is ${want}"
}

# lf_memory — "<free> <allocatable> <requested> <nodes> <ok>": the memory of
# the Ready nodes' .status.allocatable, the effective memory requests of every
# pod that is neither Succeeded nor Failed, and what is left, each in GiB with
# one decimal, and whether the free bytes reach LAB_FAILOVER_MIN_FREE_MEMORY.
# The quantity parser and the effective-request rule are those of
# hack/ci-check-node-budget.sh.
lf_memory() {
  local dir rc=0
  dir="$(mktemp -d "${TMPDIR:-/tmp}/lab-failover-memory.XXXXXX")"
  if kubectl get nodes -o json >"${dir}/nodes.json" &&
    kubectl get pods -A --field-selector=status.phase!=Succeeded,status.phase!=Failed -o json >"${dir}/pods.json"; then
    # shellcheck disable=SC2016 # $vars below are jq variables
    jq -rn --slurpfile n "${dir}/nodes.json" --slurpfile p "${dir}/pods.json" \
      --arg floor "$LAB_FAILOVER_MIN_FREE_MEMORY" '
      def qty:
        tostring
        | (capture("^(?<n>[0-9]+(\\.[0-9]*)?([eE][+-]?[0-9]+)?)(?<s>Ki|Mi|Gi|Ti|m|k|M|G|T)?$")
           // error("cannot parse the quantity \(.)"))
        | (.n | tonumber) * {"": 1, m: 0.001, k: 1e3, M: 1e6, G: 1e9, T: 1e12,
            Ki: 1024, Mi: 1048576, Gi: 1073741824, Ti: 1099511627776}[.s // ""];
      def request: (.resources.requests.memory // "0") | qty;
      def effective:
        ([.spec.containers[]? | request] | add // 0) as $app
        | (reduce (.spec.initContainers // [])[] as $c ({sidecars: 0, init: 0};
            ($c | request) as $r
            | if $c.restartPolicy == "Always"
              then .sidecars += $r | .init = ([.init, .sidecars] | max)
              else .init = ([.init, $r + .sidecars] | max)
              end)) as $acc
        | ([$app + $acc.sidecars, $acc.init] | max) + ((.spec.overhead.memory // "0") | qty);
      def gib: . / 1073741824 * 10 | floor / 10;
      [$n[0].items[] | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))] as $ready
      | ($ready | map(.status.allocatable.memory | qty) | add // 0) as $alloc
      | ($p[0].items | map(effective) | add // 0) as $req
      | ($alloc - $req) as $free
      | "\($free | gib) \($alloc | gib) \($req | gib) \($ready | length) \(if $free >= ($floor | qty) then "yes" else "no" end)"
    ' || rc=$?
  else
    rc=1
  fi
  rm -rf "$dir"
  return "$rc"
}

# ---------------------------------------------------------------------------
# lf_preflight — The read-only checks of the lab, in order, one OK: or FAIL:
# line each; the first FAIL: exits 1.
# ---------------------------------------------------------------------------
lf_preflight() {
  local server memory free alloc req nodes ok
  lf_check_settings
  lf_require kubectl jq python3 chainsaw
  lf_kubeconfig
  if ! kubectl version --request-timeout=10s >/dev/null; then
    lf_check_fail "the API of context ${LAB_FAILOVER_CONTEXT} does not answer (kubectl's error is above)"
  fi
  server="$(kubectl config view -o jsonpath='{.clusters[0].cluster.server}')"
  echo "OK: the API at ${server} answers"
  lf_read_shoot
  lf_exists "namespace chaos-mesh" namespace chaos-mesh
  lf_exists "CRD podchaos.chaos-mesh.org" crd podchaos.chaos-mesh.org
  lf_exists "CRD networkchaos.chaos-mesh.org" crd networkchaos.chaos-mesh.org
  lf_exists "ClusterIssuer openstack-ovn-ca-issuer" clusterissuer openstack-ovn-ca-issuer
  lf_field_is "ClusterSecretStore openbao-cluster-store Ready" True '{.status.conditions[?(@.type=="Ready")].status}' \
    clustersecretstore openbao-cluster-store
  lf_field_is "pod shared-services/openbao-0" Running '{.status.phase}' pod openbao-0 -n shared-services
  lf_exists "Secret shared-services/openbao-init-keys" secret openbao-init-keys -n shared-services
  lf_exists "Secret openstack/garage-s3-credentials" secret garage-s3-credentials -n openstack
  lf_exists "Secret openstack/ceph-client-cinder" secret ceph-client-cinder -n openstack
  lf_exists "CRD controlplanes.c5c3.io" crd controlplanes.c5c3.io
  lf_exists "CRD ovncentrals.ovn.openstack.c5c3.io" crd ovncentrals.ovn.openstack.c5c3.io
  lf_exists "CRD openbaotenants.openbao.org" crd openbaotenants.openbao.org
  if ! memory="$(lf_memory)"; then
    lf_check_fail "cannot sum the free memory requests (kubectl's or jq's error is above)"
  fi
  read -r free alloc req nodes ok <<<"$memory"
  LF_FREE_BEFORE="${free} GiB"
  if [[ "$ok" != "yes" ]]; then
    lf_check_fail "free memory requests ${free} GiB are below the floor LAB_FAILOVER_MIN_FREE_MEMORY=${LAB_FAILOVER_MIN_FREE_MEMORY} (allocatable ${alloc} GiB on ${nodes} Ready nodes, requested ${req} GiB)"
  fi
  echo "OK: free memory requests ${free} GiB reach the floor ${LAB_FAILOVER_MIN_FREE_MEMORY} (allocatable ${alloc} GiB on ${nodes} Ready nodes, requested ${req} GiB)"
  echo "shoot: ${LF_SHOOT}"
  echo "server: ${server}"
  echo "free memory requests: ${free} GiB"
}

# lf_step <description> <command...> — Run one step of up; on a failure name
# the step and exit 1.
lf_step() {
  local what="$1"
  shift
  lf_log "up: ${what}"
  if ! "$@"; then
    lf_fail "up failed at: ${what}; make lab-failover-down cleans up"
  fi
}

# lf_create_namespace <run> — The namespace openstack-ha with the run label,
# the lab's apiserver-proxy label and the Chaos Mesh annotation.
lf_create_namespace() {
  kubectl create -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: ${NAMESPACE}
  labels:
    ${LF_RUN_LABEL}: "$1"
    apiserver-proxy.networking.gardener.cloud/inject: disable
  annotations:
    chaos-mesh.org/inject: enabled
EOF
}

# lf_apply <fixture> — Apply one fixture of tests/lab-failover/fixtures/.
lf_apply() {
  kubectl apply -f "${LF_FIXTURES_DIR}/$1"
}

# lf_copy_secret <name> — Copy the Secret <name> from openstack into
# openstack-ha, with its type and data only and the label of a copy. The data
# goes through a pipe, never through an argv.
lf_copy_secret() {
  local json
  json="$(kubectl get secret "$1" -n openstack -o json)" || return 1
  jq --arg ns "$NAMESPACE" --arg label "$LF_COPIED_LABEL" \
    '{apiVersion: "v1", kind: "Secret", type, data,
      metadata: {name: .metadata.name, namespace: $ns, labels: {($label): "true"}}}' <<<"$json" |
    kubectl apply -n "$NAMESPACE" -f -
}

# lf_enable_ovn_db_sync — The ControlPlane does not project spec.ovnDBSync
# (reconcile_neutron.go), and a Neutron without it has no db-sync CronJob
# (reconcile_ovndbsync.go), which verify's --ovn-db-sync runs. The c5c3-operator
# applies the Neutron child with Server-Side Apply and never owns the field, so
# the patch stays.
lf_enable_ovn_db_sync() {
  kubectl patch neutron "${LF_CP}-neutron" -n "$NAMESPACE" --type merge \
    -p '{"spec":{"ovnDBSync":{"syncMode":"log","suspend":true}}}' || return 1
  lf_until "$LF_WAIT_DB_SYNC" "cronjob/${LF_CP}-neutron-ovn-db-sync exists" \
    kubectl get cronjob "${LF_CP}-neutron-ovn-db-sync" -n "$NAMESPACE" -o name
}

# lf_seed — The seed Job of flavor m1.nano and the seed image; its log on a
# failure.
lf_seed() {
  lf_apply 04-seed-job.yaml || return 1
  if ! kubectl wait job/"${LF_CP}-seed" -n "$NAMESPACE" --for=condition=complete --timeout="${LF_WAIT_SEED}s"; then
    kubectl logs job/"${LF_CP}-seed" -n "$NAMESPACE" --tail=50 || true
    return 1
  fi
}

# lf_purge — Delete every volume and image of the plane through its own APIs
# (06-purge-job.yaml), so nothing it stored stays in the lab's Ceph pool or
# Garage bucket once its databases are gone; the Job's log on a failure. The
# seed Job is the first writer of that storage: without it the plane stored
# nothing, and down deletes it only after a purge that passed, so a resumed
# down skips the purge.
lf_purge() {
  local out
  if ! out="$(kubectl get job "${LF_CP}-seed" -n "$NAMESPACE" -o name 2>&1)"; then
    if [[ "$out" == *"(NotFound)"* ]]; then
      echo "no Job ${LF_CP}-seed, so the plane stored no volume or image"
      return 0
    fi
    echo "ERROR: cannot read Job ${LF_CP}-seed: $(lf_first_line "$out")"
    return 1
  fi
  kubectl delete job "${LF_CP}-purge" -n "$NAMESPACE" --ignore-not-found --cascade=foreground || return 1
  lf_apply 06-purge-job.yaml || return 1
  if ! kubectl wait job/"${LF_CP}-purge" -n "$NAMESPACE" --for=condition=complete --timeout="${LF_WAIT_PURGE}s"; then
    kubectl logs job/"${LF_CP}-purge" -n "$NAMESPACE" --tail=50 || true
    return 1
  fi
}

# lf_fake_compute — The fake compute, mapped into cell1 under its node's name.
lf_fake_compute() {
  local node
  lf_apply 05-fake-compute.yaml || return 1
  kubectl rollout status deploy/"${LF_CP}-fake-compute" -n "$NAMESPACE" --timeout="${LF_WAIT_COMPUTE}s" || return 1
  node="$(kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/name=nova-fake-compute,app.kubernetes.io/instance="${LF_CP}-nova" \
    -o jsonpath='{.items[0].spec.nodeName}')" || return 1
  [[ -n "$node" ]] || return 1
  "$LF_DISCOVER_HOSTS" "${LF_CP}-nova" "$NAMESPACE" "$node"
}

# lf_write_versions <file> — The commit, the Flux HelmReleases, the operator
# images and the images of the backing services under test.
lf_write_versions() {
  {
    echo "commit: $(git -C "$LF_REPO_ROOT" rev-parse HEAD 2>/dev/null || echo unknown)"
    echo "shoot: ${LF_SHOOT}"
    echo "=== HelmReleases of flux-system: chart, version, status.history[0] ==="
    kubectl get helmreleases -n flux-system -o json |
      jq -r '.items[] | "\(.metadata.name) \(.spec.chart.spec.chart // "-") \(.spec.chart.spec.version // "-") \(.status.history[0] // {} | tojson)"' || true
    echo "=== operator Deployments in *-system namespaces ==="
    kubectl get deployments -A -o json |
      jq -r '.items[] | select(.metadata.namespace | endswith("-system"))
        | "\(.metadata.namespace)/\(.metadata.name) \([.spec.template.spec.containers[].image] | join(" "))"' || true
    echo "=== Galera, broker and OVN pods of ${NAMESPACE} ==="
    kubectl get pods -n "$NAMESPACE" -o json |
      jq -r --arg re "^(${LF_DB}-[0-9]+|${LF_BROKER}-server-[0-9]+|${LF_OVN}-.*)$" '.items[] | select(.metadata.name | test($re))
        | "\(.metadata.name) \([.spec.containers[].image] | join(" "))"' || true
  } >"$1"
}

# lf_write_topology <file> — The pods and their nodes, what the LimitRange
# admitted for the backing services, the quota, every Galera, broker and Raft
# status, the partition handling, the PDBs and the free memory.
lf_write_topology() {
  local after
  after="$(lf_memory 2>/dev/null)" || after="unknown"
  {
    echo "=== pods of ${NAMESPACE} ==="
    kubectl get pods -n "$NAMESPACE" -o wide || true
    echo "=== admitted requests and limits of the MariaDB, RabbitMQ and Memcached containers ==="
    kubectl get pods -n "$NAMESPACE" -o json |
      jq -r --arg re "^(${LF_DB}-[0-9]+|${LF_BROKER}-server-[0-9]+|openstack-memcached-.*)$" '.items[] | select(.metadata.name | test($re))
        | .metadata.name as $pod | ((.spec.initContainers // []) + .spec.containers)[]
        | "\($pod)/\(.name) requests=\(.resources.requests // {} | tojson) limits=\(.resources.limits // {} | tojson)"' || true
    echo "=== LimitRanges of ${NAMESPACE} ==="
    kubectl get limitrange -n "$NAMESPACE" -o yaml || true
    echo "=== ResourceQuota of ${NAMESPACE} ==="
    kubectl describe resourcequota -n "$NAMESPACE" || true
    lf_cluster_status
    echo "=== PodDisruptionBudgets of ${NAMESPACE} ==="
    kubectl get pdb -n "$NAMESPACE" || true
    echo "=== free memory requests ==="
    echo "before up: ${LF_FREE_BEFORE}"
    echo "after up: ${after%% *} GiB"
  } >"$1" 2>&1
}

# ---------------------------------------------------------------------------
# lf_up — preflight, the go-ahead, then the plane, from the namespace to
# Ready/AllReady, and the run's versions.txt and topology.txt.
# ---------------------------------------------------------------------------
lf_up() {
  local out dir name
  lf_preflight
  lf_go_ahead
  if out="$(kubectl get namespace "$NAMESPACE" -o name 2>&1)"; then
    lf_fail "namespace ${NAMESPACE} exists; refusing to adopt it (make lab-failover-down deletes a plane up made)"
  elif [[ "$out" != *"(NotFound)"* ]]; then
    lf_fail "cannot read namespace ${NAMESPACE}: $(lf_first_line "$out")"
  fi
  LAB_FAILOVER_RUN="${LAB_FAILOVER_RUN:-$(date -u +%Y%m%dT%H%M%SZ)}"
  dir="${LF_OUTPUT_DIR}/${LAB_FAILOVER_RUN}"
  mkdir -p "$dir"
  lf_log "up: run ${LAB_FAILOVER_RUN}, outputs in ${dir}"

  lf_step "create namespace ${NAMESPACE}" lf_create_namespace "$LAB_FAILOVER_RUN"
  lf_step "apply OpenBaoTenant ${LF_TENANT_NAMESPACE}/${NAMESPACE}" lf_apply 00-openbao-tenant.yaml
  for name in $LF_COPIED_SECRETS; do
    lf_step "copy Secret ${name} from openstack" lf_copy_secret "$name"
  done
  lf_step "apply OVNCentral ${LF_OVN}" lf_apply 01-ovncentral.yaml
  lf_step "wait for OVNCentral ${LF_OVN} Ready" cp_wait_condition ovncentral "$LF_OVN" Ready True "$LF_WAIT_OVN"
  lf_step "seed the OpenBao paths of ${NAMESPACE}/${LF_CP}" cp_seed "$LF_CP"
  lf_step "apply ControlPlane ${LF_CP}" lf_apply 03-controlplane.yaml
  lf_step "wait for InfrastructureReady" cp_wait_condition controlplane "$LF_CP" InfrastructureReady True "$LF_WAIT_INFRA"
  # The ExternalSecret of the Horizon secret key reads through the tenant
  # store, which the c5c3-operator creates in its ESOTenantStore step.
  lf_step "wait for ESOTenantStoreReady" cp_wait_condition controlplane "$LF_CP" ESOTenantStoreReady True "$LF_WAIT_TENANT_STORE"
  lf_step "apply ExternalSecret ${LF_CP}-horizon-secret-key on the tenant store" lf_apply 02-horizon-secret-key-externalsecret.yaml
  lf_step "wait for MariaDB ${LF_DB} Ready" cp_wait_condition mariadb "$LF_DB" Ready True "$LF_WAIT_MARIADB"
  lf_step "onboard the database tenant" cp_onboard_database "$LF_CP"
  lf_step "wait for KeystoneReady" cp_wait_condition controlplane "$LF_CP" KeystoneReady True "$LF_WAIT_KEYSTONE"
  lf_step "wait for NovaReady" cp_wait_condition controlplane "$LF_CP" NovaReady True "$LF_WAIT_NOVA"
  lf_step "enable the OVN db-sync CronJob of ${LF_CP}-neutron in log mode" lf_enable_ovn_db_sync
  lf_step "seed flavor m1.nano and the seed image" lf_seed
  lf_step "start the fake compute and map its host" lf_fake_compute
  lf_step "wait for ControlPlane ${LF_CP} Ready/AllReady" lf_wait_cp_ready "$LF_WAIT_READY"

  lf_log "up: writing ${dir}/versions.txt and ${dir}/topology.txt"
  lf_write_versions "${dir}/versions.txt"
  lf_write_topology "${dir}/topology.txt"
  lf_log "up: ControlPlane ${NAMESPACE}/${LF_CP} is Ready; run the suites with make lab-failover-run"
}

# lf_read_run — Set LAB_FAILOVER_RUN from the namespace's run label. Exits 0
# with "nothing to delete" when <absent-ok> is given and the namespace is
# absent, and exits 1 on a namespace up did not make.
lf_read_run() {
  local out
  if ! out="$(kubectl get namespace "$NAMESPACE" -o "jsonpath={.metadata.labels.${LF_RUN_LABEL//./\\.}}" 2>&1)"; then
    if [[ "$out" == *"(NotFound)"* && -n "${1:-}" ]]; then
      echo "nothing to delete: namespace ${NAMESPACE} does not exist"
      exit 0
    fi
    lf_fail "cannot read namespace ${NAMESPACE}: $(lf_first_line "$out")"
  fi
  if [[ -z "$out" ]]; then
    lf_fail "namespace ${NAMESPACE} carries no label ${LF_RUN_LABEL}, so up did not make it; refusing to touch it"
  fi
  LAB_FAILOVER_RUN="$out"
}

# ---------------------------------------------------------------------------
# lf_run [scenario...] — Run the suites in order, one summary.tsv row each,
# and stop at the first FAIL.
# ---------------------------------------------------------------------------
lf_run() {
  local scenarios=() s known valid dir summary start end rc observed i
  if [[ $# -gt 0 ]]; then
    scenarios=("$@")
  elif [[ -n "$LAB_FAILOVER_SCENARIOS" ]]; then
    read -r -a scenarios <<<"$LAB_FAILOVER_SCENARIOS"
  else
    scenarios=("${LF_SCENARIOS[@]}")
  fi
  for s in "${scenarios[@]}"; do
    valid=""
    for known in "${LF_SCENARIOS[@]}"; do
      if [[ "$s" == "$known" ]]; then valid=yes; fi
    done
    if [[ -z "$valid" ]]; then
      echo "ERROR: unknown scenario '${s}'; the scenarios are:" >&2
      printf '  %s\n' "${LF_SCENARIOS[@]}" >&2
      exit 2
    fi
  done
  lf_check_settings
  lf_require kubectl jq python3 chainsaw
  lf_kubeconfig
  lf_go_ahead
  lf_read_run
  if ! lf_wait_cp_ready "$LF_WAIT_RUN_READY"; then
    lf_fail "ControlPlane ${NAMESPACE}/${LF_CP} is not Ready/AllReady; no suite runs"
  fi
  dir="${LF_OUTPUT_DIR}/${LAB_FAILOVER_RUN}"
  summary="${dir}/summary.tsv"
  mkdir -p "$dir"
  if [[ ! -f "$summary" ]]; then
    printf 'scenario\tverdict\tstart\tend\tobserved\n' >"$summary"
  fi
  for i in "${!scenarios[@]}"; do
    s="${scenarios[$i]}"
    mkdir -p "${dir}/${s}"
    lf_log "run: ${s}"
    start="$(date -u +%FT%TZ)"
    rc=0
    chainsaw test --config "${LF_SUITES_DIR}/chainsaw-config.yaml" --report-path "${dir}/${s}" \
      "${LF_SUITES_DIR}/${s}" 2>&1 | tee "${dir}/${s}.log" || rc=$?
    end="$(date -u +%FT%TZ)"
    # The lines workload.py evaluate prints, one per stream.
    observed="$(grep -oE 'stream [a-z]+: .*observed failures=[0-9]+ recovery=[0-9.]+s' "${dir}/${s}.log" |
      awk 'NR > 1 { printf "; " } { printf "%s", $0 }')" || observed=""
    if [[ $rc -eq 0 ]]; then
      printf '%s\tPASS\t%s\t%s\t%s\n' "$s" "$start" "$end" "${observed:-none}" >>"$summary"
      lf_log "run: ${s} PASS"
      continue
    fi
    printf '%s\tFAIL\t%s\t%s\t%s\n' "$s" "$start" "$end" "${observed:-none}" >>"$summary"
    lf_log "run: ${s} FAIL; log ${dir}/${s}.log"
    if [[ $((i + 1)) -lt ${#scenarios[@]} ]]; then
      echo "not run: ${scenarios[*]:$((i + 1))}"
    fi
    exit 1
  done
  lf_log "run: ${#scenarios[@]} scenarios passed; summary ${summary}"
}

# lf_down_step <description> <command...> — Run one delete of down; on a
# failure list what is left in the namespace and exit 1.
lf_down_step() {
  local what="$1" kinds
  shift
  lf_log "down: ${what}"
  if "$@"; then
    return 0
  fi
  echo "ERROR: down failed at: ${what}; what is left in ${NAMESPACE}:"
  kinds="$(kubectl api-resources --verbs=list --namespaced -o name 2>/dev/null | paste -sd, -)" || kinds=""
  kubectl get "${kinds:-all}" -n "$NAMESPACE" --ignore-not-found 2>&1 || true
  echo "make lab-failover-down resumes the delete"
  exit 1
}

# ---------------------------------------------------------------------------
# lf_down — Delete what up created, in order, each delete absent-tolerant.
# ---------------------------------------------------------------------------
lf_down() {
  lf_check_settings
  lf_require kubectl jq
  lf_kubeconfig
  lf_go_ahead
  lf_read_run absent-ok
  lf_log "down: run ${LAB_FAILOVER_RUN}"
  lf_down_step "delete Deployment ${LF_CP}-fake-compute" \
    kubectl delete deployment "${LF_CP}-fake-compute" -n "$NAMESPACE" --ignore-not-found
  # A workload pod whose suite never reached its stop keeps creating volumes
  # and images, which the purge would race and leave behind.
  lf_down_step "delete the workload pods" kubectl delete pod -n "$NAMESPACE" -l app.kubernetes.io/name=chaos-workload \
    --ignore-not-found --wait --timeout="${LF_WAIT_GONE}s"
  lf_down_step "delete the volumes and images of ${LF_CP} through its APIs" lf_purge
  lf_down_step "delete Job ${LF_CP}-seed" kubectl delete job "${LF_CP}-seed" -n "$NAMESPACE" --ignore-not-found
  lf_down_step "delete ControlPlane ${LF_CP}" cp_teardown "$LF_CP"
  lf_down_step "delete OVNCentral ${LF_OVN}" kubectl delete ovncentral "$LF_OVN" -n "$NAMESPACE" \
    --ignore-not-found --wait --timeout="${LF_WAIT_GONE}s"
  lf_down_step "delete the Galera claims" kubectl delete pvc -n "$NAMESPACE" -l app.kubernetes.io/name=mariadb \
    --ignore-not-found --wait --timeout="${LF_WAIT_GONE}s"
  lf_down_step "delete the broker claims" kubectl delete pvc -n "$NAMESPACE" -l "app.kubernetes.io/name=${LF_BROKER}" \
    --ignore-not-found --wait --timeout="${LF_WAIT_GONE}s"
  lf_down_step "delete the copied Secrets" kubectl delete secret -n "$NAMESPACE" -l "${LF_COPIED_LABEL}=true" \
    --ignore-not-found
  lf_down_step "delete ExternalSecret ${LF_CP}-horizon-secret-key" \
    kubectl delete externalsecret "${LF_CP}-horizon-secret-key" -n "$NAMESPACE" --ignore-not-found
  lf_down_step "delete OpenBaoTenant ${LF_TENANT_NAMESPACE}/${NAMESPACE}" \
    kubectl delete openbaotenant "$NAMESPACE" -n "$LF_TENANT_NAMESPACE" --ignore-not-found --wait --timeout="${LF_WAIT_GONE}s"
  lf_down_step "delete namespace ${NAMESPACE}" \
    kubectl delete namespace "$NAMESPACE" --ignore-not-found --wait --timeout="${LF_WAIT_NAMESPACE_GONE}s"
  lf_log "down: namespace ${NAMESPACE} and everything up created are gone"
}

# ---------------------------------------------------------------------------
# lf_main
# ---------------------------------------------------------------------------
lf_main() {
  local sub="${1:-}"
  [[ $# -gt 0 ]] && shift
  case "$sub" in
    preflight) lf_preflight ;;
    up) lf_up ;;
    run) lf_run "$@" ;;
    down) lf_down ;;
    *) lf_usage ;;
  esac
}

# Run lf_main only when executed directly so the unit test
# (tests/unit/hack/lab_failover_test.sh) can source this script and drive it.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  lf_main "$@"
fi
