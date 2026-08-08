#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-generate-tempest-matrix.sh — Generate Tempest release matrix from releases/ directories.
#
# Scans releases/*/ directories and, for each release, emits one matrix entry
# per Tempest-covered service (keystone, glance, barbican). Each service requires
# a matching Tempest config directory at tests/tempest/<service>-<slug>/ (e.g.
# keystone-2025-2, glance-2025-2 and barbican-2025-2 for release 2025.2); a
# missing directory for any service is a hard failure.
#
# Each emitted entry carries:
#   service          — service under test (keystone|glance|barbican)
#   release          — OpenStack release (e.g. 2025.2)
#   config-dir       — tests/tempest/<service>-<slug>
#   cr-name          — Keystone CR the CI job waits on and port-forwards
#   service-k8s-name — K8s Service name for the keystone port-forward (== cr-name)
# and, for the glance service only, additionally:
#   glance-cr-name   — Glance CR the CI job waits on; doubles as the K8s Service
#                      name for the glance port-forward
# and, for the barbican service only, additionally:
#   barbican-cr-name — Barbican CR the CI job waits on; doubles as the K8s
#                      Service name for the barbican port-forward
#
# For keystone the cr-name/service-k8s-name are keystone-tempest-<slug>; the
# glance leg runs against its own keystone-glance-tempest-<slug> identity CR and
# the glance-tempest-<slug> image CR, the barbican leg against its own
# keystone-barbican-tempest-<slug> identity CR and the barbican-tempest-<slug>
# key-manager CR.
#
# Required env vars:
#   GITHUB_OUTPUT — GitHub Actions output file (set automatically by Actions)
#
# Extracted from ci.yaml inline script (review #2).
# set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

# Default GITHUB_OUTPUT to /dev/null for local execution.
: "${GITHUB_OUTPUT:=/dev/null}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ---------------------------------------------------------------------------
# 1. Discover release directories
# ---------------------------------------------------------------------------
shopt -s nullglob
dirs=("${REPO_ROOT}"/releases/*/)
entries=()

for d in "${dirs[@]}"; do
  release="${d%/}"
  release="${release##*/}"
  slug="${release//./-}"
  for service in keystone glance barbican; do
    config_dir="tests/tempest/${service}-${slug}"
    if [[ ! -d "${REPO_ROOT}/${config_dir}" ]]; then
      echo "::error::Missing Tempest config directory: ${config_dir} (for service ${service}, release ${release})"
      exit 1
    fi
    cr_name="keystone-tempest-${slug}"
    extra_keys=""
    if [[ "${service}" != "keystone" ]]; then
      # Every non-keystone leg runs against its own Keystone identity CR (waited
      # on and port-forwarded by the CI job) plus its own service CR.
      cr_name="keystone-${service}-tempest-${slug}"
      extra_keys=",\"${service}-cr-name\":\"${service}-tempest-${slug}\""
    fi
    entries+=("{\"service\":\"${service}\",\"release\":\"${release}\",\"config-dir\":\"${config_dir}\",\"cr-name\":\"${cr_name}\",\"service-k8s-name\":\"${cr_name}\"${extra_keys}}")
  done
done

# ---------------------------------------------------------------------------
# 2. Emit matrix JSON to GITHUB_OUTPUT
# ---------------------------------------------------------------------------
if [[ ${#entries[@]} -eq 0 ]]; then
  echo 'tempest-releases={"include":[]}' >> "$GITHUB_OUTPUT"
else
  matrix=$(printf '%s\n' "${entries[@]}" | jq -sc '{"include": .}')
  echo "tempest-releases=${matrix}" >> "$GITHUB_OUTPUT"
fi
