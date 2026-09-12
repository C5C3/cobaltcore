#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-create-kind-cluster.sh — Create the e2e kind cluster on a CI runner.
#
# Wraps `kind create cluster` in the two things helm/kind-action does not do,
# and which every e2e job on the reused `self-hosted` runners needs:
#
#   1. Reset the runner first (hack/ci-reset-kind-cluster.sh). A cancelled job
#      leaves its node containers behind, and the next job's creation then fails
#      with `node(s) already exist for a cluster with the name "cobaltcore"`.
#   2. Retry the creation. A creation that fails leaves a partial cluster, so the
#      reset in front of the next attempt is what makes the retry a clean one.
#
# The binary comes from helm/kind-action's tool cache, which the composite action
# in .github/actions/create-kind-cluster installs with `install_only: true`
# before calling this script — that keeps kind, kubectl and the post-job
# `kind delete cluster` exactly where they were before this wrapper existed.
#
# Required env vars:
#   CLUSTER_NAME             — cluster to create (default: $KIND_CLUSTER)
#
# Optional env vars:
#   KIND_CLUSTER             — fallback for CLUSTER_NAME
#   KIND_CONFIG              — kind config file (default: none, a plain cluster)
#   KIND_VERSION             — version installed, for the tool-cache lookup below
#   KIND_WAIT                — control-plane readiness wait (default: 60s)
#   KIND_CREATE_ATTEMPTS     — creation attempts, reset in front of each (default: 2)
#   KIND_CREATE_RETRY_DELAY  — seconds between attempts (default: 10)
#   KIND_RESET_SCRIPT        — reset script path (default: hack/ci-reset-kind-cluster.sh)
#
# Usage:
#   CLUSTER_NAME=cobaltcore KIND_CONFIG=hack/kind-config.yaml \
#     hack/ci-create-kind-cluster.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

CLUSTER_NAME="${CLUSTER_NAME:-${KIND_CLUSTER:-}}"
KIND_CONFIG="${KIND_CONFIG:-}"
KIND_WAIT="${KIND_WAIT:-60s}"
KIND_CREATE_ATTEMPTS="${KIND_CREATE_ATTEMPTS:-2}"
KIND_CREATE_RETRY_DELAY="${KIND_CREATE_RETRY_DELAY:-10}"
KIND_RESET_SCRIPT="${KIND_RESET_SCRIPT:-${SCRIPT_DIR}/ci-reset-kind-cluster.sh}"
KIND_CLUSTER_LABEL="io.x-k8s.kind.cluster"

KIND_BIN=""

# ---------------------------------------------------------------------------
# log — Print a timestamped log message (ISO 8601 UTC).
# ---------------------------------------------------------------------------
log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

# ---------------------------------------------------------------------------
# resolve_kind — Echo the kind binary to use. helm/kind-action appends its tool
# cache directory to GITHUB_PATH, so PATH normally carries it; the cache path is
# reconstructed as a fallback because a PATH that failed to carry over would
# otherwise take down every e2e job at once.
# ---------------------------------------------------------------------------
resolve_kind() {
  if command -v kind >/dev/null 2>&1; then
    command -v kind
    return 0
  fi

  local arch cached
  case "$(uname -m)" in
    x86_64) arch="amd64" ;;
    aarch64 | arm64) arch="arm64" ;;
    *) return 1 ;;
  esac

  if [[ -n "${RUNNER_TOOL_CACHE:-}" && -n "${KIND_VERSION:-}" ]]; then
    cached="${RUNNER_TOOL_CACHE}/kind/${KIND_VERSION}/${arch}/kind/bin/kind"
    if [[ -x "${cached}" ]]; then
      echo "${cached}"
      return 0
    fi
  fi

  return 1
}

# ---------------------------------------------------------------------------
# create_cluster — One `kind create cluster` attempt.
# ---------------------------------------------------------------------------
create_cluster() {
  local args=(create cluster "--name=${CLUSTER_NAME}" "--wait=${KIND_WAIT}")

  if [[ -n "${KIND_CONFIG}" ]]; then
    args+=("--config=${KIND_CONFIG}")
  fi

  log "${KIND_BIN} ${args[*]}"
  "${KIND_BIN}" "${args[@]}"
}

# ---------------------------------------------------------------------------
# dump_failure_diagnostics — Print what the host looks like after a failed
# creation: the node containers that exist and the tail of their logs. Without
# this the next attempt's reset removes the evidence, and a creation that keeps
# failing leaves nothing behind to read — the cluster never comes up, so
# hack/ci-dump-diagnostics.sh has no API server to talk to either.
# ---------------------------------------------------------------------------
dump_failure_diagnostics() {
  command -v docker >/dev/null 2>&1 || return 0

  log "--- kind node containers on this host ---"
  docker ps -a --filter "label=${KIND_CLUSTER_LABEL}" \
    --format "{{.Names}}\t{{.Status}}\t{{.Label \"${KIND_CLUSTER_LABEL}\"}}" 2>&1 || true

  local id
  while IFS= read -r id; do
    [[ -n "${id}" ]] || continue
    log "--- docker logs (last 30 lines) of ${id} ---"
    docker logs --tail 30 "${id}" 2>&1 || true
  done < <(docker ps -aq --filter "label=${KIND_CLUSTER_LABEL}=${CLUSTER_NAME}" 2>/dev/null || true)
}

# ---------------------------------------------------------------------------
# verify_cluster — Confirm kind knows the cluster it just reported creating, and
# show the nodes it came up with.
# ---------------------------------------------------------------------------
verify_cluster() {
  if ! "${KIND_BIN}" get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
    echo "::error::'kind create cluster' reported success but kind does not list a cluster named '${CLUSTER_NAME}'." >&2
    return 1
  fi

  if command -v kubectl >/dev/null 2>&1; then
    log "--- nodes of '${CLUSTER_NAME}' ---"
    kubectl --context "kind-${CLUSTER_NAME}" get nodes -o wide 2>&1 || true
  fi
}

main() {
  if [[ -z "${CLUSTER_NAME}" ]]; then
    echo "::error::hack/ci-create-kind-cluster.sh needs CLUSTER_NAME (or KIND_CLUSTER)." >&2
    exit 2
  fi

  if [[ -n "${KIND_CONFIG}" && ! -f "${KIND_CONFIG}" ]]; then
    echo "::error::kind config '${KIND_CONFIG}' does not exist." >&2
    exit 2
  fi

  if [[ ! "${KIND_CREATE_ATTEMPTS}" =~ ^[1-9][0-9]*$ ]]; then
    echo "::error::KIND_CREATE_ATTEMPTS must be a positive integer — got '${KIND_CREATE_ATTEMPTS}'." >&2
    exit 2
  fi

  if [[ ! -f "${KIND_RESET_SCRIPT}" ]]; then
    echo "::error::reset script '${KIND_RESET_SCRIPT}' does not exist." >&2
    exit 2
  fi

  if ! KIND_BIN="$(resolve_kind)"; then
    echo "::error::kind is not on PATH and no binary was found in the runner tool cache." >&2
    exit 2
  fi

  log "Cluster name : ${CLUSTER_NAME}"
  log "Kind config  : ${KIND_CONFIG:-<none>}"
  log "Kind binary  : ${KIND_BIN}"
  log "Wait         : ${KIND_WAIT}"
  log "Attempts     : ${KIND_CREATE_ATTEMPTS}"

  local attempt=1
  while ((attempt <= KIND_CREATE_ATTEMPTS)); do
    log "=== Attempt ${attempt}/${KIND_CREATE_ATTEMPTS}: reset the runner, then create '${CLUSTER_NAME}' ==="

    if ! bash "${KIND_RESET_SCRIPT}" "${CLUSTER_NAME}"; then
      echo "::error::the runner still carries a kind cluster that could not be removed — not creating '${CLUSTER_NAME}'." >&2
      exit 1
    fi

    if create_cluster; then
      log "Kind cluster '${CLUSTER_NAME}' created."
      if ! verify_cluster; then
        exit 1
      fi
      return 0
    fi

    log "WARNING: creating '${CLUSTER_NAME}' failed on attempt ${attempt}/${KIND_CREATE_ATTEMPTS}."
    dump_failure_diagnostics

    attempt=$((attempt + 1))
    if ((attempt <= KIND_CREATE_ATTEMPTS)); then
      log "Retrying in ${KIND_CREATE_RETRY_DELAY}s; the reset in front of the next attempt clears the partial cluster."
      sleep "${KIND_CREATE_RETRY_DELAY}"
    fi
  done

  echo "::error::kind cluster '${CLUSTER_NAME}' could not be created in ${KIND_CREATE_ATTEMPTS} attempt(s)." >&2
  exit 1
}

main "$@"
