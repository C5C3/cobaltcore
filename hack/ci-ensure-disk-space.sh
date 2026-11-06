#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-ensure-disk-space.sh — Secure the free space an e2e job needs on the
# runner's Docker disk before the job fills it.
#
# The `self-hosted` runners are reused between jobs and keep every image a job
# pulled. Their own disk guard (the "Set up runner" and "Complete runner" hooks
# of B42Labs/openstack-github-runner-manager) clears everything only once the
# Docker disk is at 80% or more after a job, so a job can start with little
# more than a fifth of the disk free: about 40 GiB of the 193 GiB disk. A kind
# node is a container whose /var lives on that disk, and every node keeps its
# own copy of each image `kind load docker-image` hands it. A job whose nodes
# need more than the runner happens to have free dies mid-load, as the
# multinode chaos leg did in run 42148206213 (#1618): it started with 43 GiB
# free and failed loading nova-compute:2026.1 into a worker with
#
#   ctr: failed to extract layer ... no space left on device
#
# This script reads the free space of the filesystem that holds Docker's data
# and compares it with what the job needs. With enough free it changes nothing,
# so the images the runner already holds stay and the job pulls nothing again.
# Short of it, it removes every image no container uses (`docker image prune
# --all`) and the build cache (`docker builder prune --all`), the same data the
# runner's own guard reclaims, and measures again. An e2e job owns its runner
# for its duration (hack/ci-reset-kind-cluster.sh), so nothing else needs those
# images now. Containers and volumes are left alone: the kind nodes of the job,
# and a named volume such as a registry cache, survive. Running it after the
# kind cluster exists keeps the node image, which the nodes use.
#
# Required env vars:
#   DISK_SPACE_REQUIRED_GIB — free GiB the job needs, a positive integer
#
# Optional env vars:
#   DISK_SPACE_PATH         — a path on the filesystem to measure (default:
#                             Docker's data root, `docker info`'s DockerRootDir)
#
# Usage:
#   DISK_SPACE_REQUIRED_GIB=75 hack/ci-ensure-disk-space.sh
#
# Exit codes:
#   0 — the filesystem has the required space, before or after the prune
#   1 — still short after the prune; the job would run out of disk
#   2 — usage error, or the free space cannot be measured

set -euo pipefail

DISK_SPACE_REQUIRED_GIB="${DISK_SPACE_REQUIRED_GIB:-}"
DISK_SPACE_PATH="${DISK_SPACE_PATH:-}"

# ---------------------------------------------------------------------------
# log — Print a timestamped log message (ISO 8601 UTC).
# ---------------------------------------------------------------------------
log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

# ---------------------------------------------------------------------------
# die — Report a usage or measurement error and exit 2.
# ---------------------------------------------------------------------------
die() {
  echo "::error::hack/ci-ensure-disk-space.sh: $*" >&2
  exit 2
}

# ---------------------------------------------------------------------------
# free_kib <path> — Echo the KiB available to unprivileged users on the
# filesystem holding <path>, the Avail column of POSIX `df -Pk`.
# ---------------------------------------------------------------------------
free_kib() {
  local out kib
  out="$(df -Pk "$1" 2>&1)" || die "df on ${1} failed: ${out}"
  kib="$(awk 'NR == 2 { print $4 }' <<<"${out}")"
  [[ "${kib}" =~ ^[0-9]+$ ]] || die "df on ${1} printed no free space: ${out}"
  echo "${kib}"
}

# ---------------------------------------------------------------------------
# gib <kib> — Echo KiB as whole GiB, rounded down, the unit `df -h` prints.
# ---------------------------------------------------------------------------
gib() {
  echo $(($1 / 1048576))
}

# ---------------------------------------------------------------------------
# report <label> <path> — Print the filesystem line of `df -h` for the record.
# ---------------------------------------------------------------------------
report() {
  log "$1: $(df -Ph "$2" 2>/dev/null | awk 'NR == 2 { print $3 " used, " $4 " free of " $2 " (" $5 ")" }')"
}

# ---------------------------------------------------------------------------
# prune — Remove the images no container uses and the build cache. A failed
# builder prune (no builder on the host) is logged; the image prune is what
# frees the space and must work.
# ---------------------------------------------------------------------------
prune() {
  local out
  log "docker image prune --all --force"
  if ! out="$(docker image prune --all --force 2>&1)"; then
    echo "::error::docker image prune failed: ${out//$'\n'/ | }" >&2
    exit 1
  fi
  grep -E '^Total reclaimed space' <<<"${out}" || true

  log "docker builder prune --all --force"
  if ! out="$(docker builder prune --all --force 2>&1)"; then
    log "WARNING: docker builder prune failed, carrying on: ${out//$'\n'/ | }"
  else
    grep -E '^Total' <<<"${out}" || true
  fi
}

main() {
  if [[ ! "${DISK_SPACE_REQUIRED_GIB}" =~ ^[1-9][0-9]*$ ]]; then
    die "DISK_SPACE_REQUIRED_GIB must be a positive integer, got '${DISK_SPACE_REQUIRED_GIB}'"
  fi
  command -v docker >/dev/null 2>&1 || die "docker is not on PATH"

  local path="${DISK_SPACE_PATH}"
  if [[ -z "${path}" ]]; then
    path="$(docker info --format '{{.DockerRootDir}}' 2>&1)" ||
      die "docker info failed: ${path}"
    [[ "${path}" == /* ]] || die "docker info printed no data root: ${path}"
  fi

  local need_kib=$((DISK_SPACE_REQUIRED_GIB * 1048576)) free
  free="$(free_kib "${path}")"
  report "Before" "${path}"
  if ((free >= need_kib)); then
    log "OK: ${path} has $(gib "${free}") GiB free, the job needs ${DISK_SPACE_REQUIRED_GIB} GiB; nothing removed."
    return 0
  fi

  log "${path} has $(gib "${free}") GiB free, the job needs ${DISK_SPACE_REQUIRED_GIB} GiB: removing the images no container uses and the build cache."
  prune
  free="$(free_kib "${path}")"
  report "After" "${path}"
  if ((free >= need_kib)); then
    log "OK: ${path} has $(gib "${free}") GiB free after the prune, the job needs ${DISK_SPACE_REQUIRED_GIB} GiB."
    return 0
  fi

  echo "::error::${path} has $(gib "${free}") GiB free after removing every unused image and the build cache; the job needs ${DISK_SPACE_REQUIRED_GIB} GiB and would run out of disk mid-run. The rest of the disk belongs to containers, volumes or files outside Docker's reach." >&2
  exit 1
}

main "$@"
