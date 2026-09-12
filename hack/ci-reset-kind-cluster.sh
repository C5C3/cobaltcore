#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-reset-kind-cluster.sh — Clear leftover kind clusters off a runner.
#
# The `self-hosted` runners are reused between jobs and nothing in the runner's
# own teardown removes a kind cluster. A job GitHub cancels mid-flight — every
# push to a pull request cancels the run in progress — never reaches the post
# step that deletes its cluster, so its node containers outlive the job and the
# next e2e job on that host dies in its first minute with
#
#   ERROR: failed to create cluster: node(s) already exist for a cluster with
#   the name "cobaltcore"
#
# or, when the leftover belongs to a cluster of another name, with a host-port
# conflict: hack/kind-config.yaml and hack/kind-config-multinode.yaml bind the
# same two host ports, so one stale control-plane node is enough to block every
# other e2e job on that runner. This script removes both kinds of leftover
# before the cluster is created, which makes the runner's state irrelevant to
# whether a job can start.
#
# Sweeping every cluster rather than only the one about to be created is safe
# because an e2e job owns its runner for its duration: two clusters on one host
# contend for those host ports, so a cluster that exists before this job creates
# one cannot belong to a job running alongside it. To keep that true for a job
# that creates a second cluster of its own, the sweep happens once: the first
# run in a job clears everything and leaves a marker in RUNNER_TEMP, and later
# runs remove only the cluster they are about to create.
#
# Required env vars:
#   (none — the cluster name may come from the first argument instead)
#
# Optional env vars:
#   CLUSTER_NAME           — cluster about to be created (default: $KIND_CLUSTER)
#   KIND_CLUSTER           — fallback for CLUSTER_NAME
#   KIND_RESET_SCOPE       — all | cluster | auto (default: auto, see above)
#   KIND_RESET_STATE_FILE  — marker path (default: a run-scoped file in $RUNNER_TEMP)
#   KIND_DELETE_TIMEOUT    — seconds a `kind delete cluster` may take (default: 120)
#
# Usage:
#   hack/ci-reset-kind-cluster.sh [cluster-name]
#
# Exits 0 when the runner carries no cluster this job has to care about, 1 when
# a leftover survived deletion, and 2 on a usage error.

set -euo pipefail

CLUSTER_NAME="${1:-${CLUSTER_NAME:-${KIND_CLUSTER:-}}}"

# The label kind stamps on every node container, and the one it looks at when it
# refuses to create a cluster whose nodes already exist. Node containers are the
# whole footprint of a kind cluster on the host: the `kind` docker network is
# shared and stays, and the registry pull-through caches of
# hack/deploy-infra.sh carry a label of their own (cobaltcore.registry-cache),
# so nothing here touches them.
KIND_CLUSTER_LABEL="io.x-k8s.kind.cluster"

KIND_RESET_SCOPE="${KIND_RESET_SCOPE:-auto}"
# The marker that tells a second run in the same job to leave the cluster the
# first one created alone. The runner recreates RUNNER_TEMP per job, and the run
# identifiers in the name keep the marker job-scoped even where it does not: a
# marker from another job is then simply absent, and the sweep runs in full.
KIND_RESET_STATE_FILE="${KIND_RESET_STATE_FILE:-${RUNNER_TEMP:-/tmp}/cobaltcore-kind-reset-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-1}.done}"
KIND_DELETE_TIMEOUT="${KIND_DELETE_TIMEOUT:-120}"

# ---------------------------------------------------------------------------
# log — Print a timestamped log message (ISO 8601 UTC).
# ---------------------------------------------------------------------------
log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

# ---------------------------------------------------------------------------
# run_with_timeout <seconds> <command…> — Run a command under `timeout` when the
# coreutils binary is there, plain otherwise. `kind delete cluster` talks to a
# docker daemon that can hang; the force-remove below is the fallback, and it
# only ever runs if this returns.
# ---------------------------------------------------------------------------
run_with_timeout() {
  local seconds="$1"
  shift
  if command -v timeout >/dev/null 2>&1; then
    timeout "${seconds}" "$@"
  else
    "$@"
  fi
}

# ---------------------------------------------------------------------------
# node_containers <cluster> — Print the container IDs of that cluster's nodes,
# one per line. Filters on the label value, so no output means no leftover.
# ---------------------------------------------------------------------------
node_containers() {
  docker ps -aq --filter "label=${KIND_CLUSTER_LABEL}=$1" 2>/dev/null || true
}

# ---------------------------------------------------------------------------
# leftover_clusters — Print the name of every kind cluster with node containers
# on this host, one per line. Reads the label off the containers rather than
# calling `kind get clusters`, so it works before helm/kind-action has put a
# kind binary on PATH.
# ---------------------------------------------------------------------------
leftover_clusters() {
  docker ps -a --filter "label=${KIND_CLUSTER_LABEL}" \
    --format "{{.Label \"${KIND_CLUSTER_LABEL}\"}}" 2>/dev/null |
    sed '/^[[:space:]]*$/d' | sort -u
}

# ---------------------------------------------------------------------------
# delete_cluster <cluster> — Remove one leftover cluster. `kind delete` goes
# first because it also drops the cluster's kubeconfig entry; whatever it leaves
# behind (or everything, when kind is not installed yet) goes with `docker rm
# -f`. Returns non-zero when a node container survives both, which is the one
# case the caller must not paper over: creating the cluster would fail again.
# ---------------------------------------------------------------------------
delete_cluster() {
  local cluster="$1" ids

  if command -v kind >/dev/null 2>&1; then
    log "  kind delete cluster --name ${cluster}"
    if ! run_with_timeout "${KIND_DELETE_TIMEOUT}" kind delete cluster --name "${cluster}"; then
      log "  WARNING: 'kind delete cluster --name ${cluster}' failed — removing the node containers directly."
    fi
  else
    log "  kind is not on PATH yet — removing the node containers with docker."
  fi

  ids="$(node_containers "${cluster}")"
  if [[ -n "${ids}" ]]; then
    log "  docker rm -f ${ids//$'\n'/ }"
    # shellcheck disable=SC2086 # word splitting is how the ID list is passed
    docker rm -f -v ${ids} >/dev/null 2>&1 || true
  fi

  ids="$(node_containers "${cluster}")"
  if [[ -n "${ids}" ]]; then
    echo "::error::kind cluster '${cluster}' still has node containers after deletion: ${ids//$'\n'/ }" >&2
    return 1
  fi

  log "  cluster '${cluster}' is gone."
}

# ---------------------------------------------------------------------------
# resolve_scope — Echo the scope this run works at: `all` sweeps every leftover
# cluster, `cluster` removes only CLUSTER_NAME. KIND_RESET_SCOPE=auto (the
# default) resolves to the first per job and to the second from then on.
# ---------------------------------------------------------------------------
resolve_scope() {
  case "${KIND_RESET_SCOPE}" in
    all | cluster)
      echo "${KIND_RESET_SCOPE}"
      ;;
    auto)
      if [[ -f "${KIND_RESET_STATE_FILE}" ]]; then
        echo "cluster"
      else
        echo "all"
      fi
      ;;
    *)
      echo "::error::KIND_RESET_SCOPE must be all, cluster or auto — got '${KIND_RESET_SCOPE}'." >&2
      return 2
      ;;
  esac
}

main() {
  if [[ -z "${CLUSTER_NAME}" ]]; then
    echo "::error::hack/ci-reset-kind-cluster.sh needs a cluster name (argument, CLUSTER_NAME or KIND_CLUSTER)." >&2
    exit 2
  fi

  if ! command -v docker >/dev/null 2>&1; then
    log "docker is not on PATH — no kind cluster can be left over here."
    return 0
  fi

  local scope
  scope="$(resolve_scope)" || exit 2

  local leftovers=() cluster
  while IFS= read -r cluster; do
    if [[ -n "${cluster}" ]]; then
      leftovers+=("${cluster}")
    fi
  done < <(leftover_clusters)

  if [[ ${#leftovers[@]} -eq 0 ]]; then
    log "No leftover kind cluster on this runner — nothing to reset."
  else
    log "Leftover kind cluster(s) on this runner: ${leftovers[*]} (scope: ${scope})"
  fi

  local status=0
  for cluster in ${leftovers[@]+"${leftovers[@]}"}; do
    if [[ "${scope}" == "cluster" && "${cluster}" != "${CLUSTER_NAME}" ]]; then
      log "Keeping '${cluster}': not this run's cluster, and the runner was already swept."
      continue
    fi
    log "Removing leftover kind cluster '${cluster}'..."
    delete_cluster "${cluster}" || status=1
  done

  if [[ "${status}" -ne 0 ]]; then
    return 1
  fi

  # Record that this job has seen a clean runner, so a second cluster created
  # later in the same job resets only its own name.
  mkdir -p "$(dirname "${KIND_RESET_STATE_FILE}")" 2>/dev/null || true
  : >"${KIND_RESET_STATE_FILE}" 2>/dev/null || true
}

main "$@"
