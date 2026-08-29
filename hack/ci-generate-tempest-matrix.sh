#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-generate-tempest-matrix.sh — Generate Tempest release matrix from releases/ directories.
#
# Scans releases/*/ directories and, for each release, emits one matrix entry
# per Tempest-covered service (keystone, glance, barbican, neutron). Each service
# requires a matching Tempest config directory at tests/tempest/<service>-<slug>/
# (e.g. keystone-2025-2, glance-2025-2, barbican-2025-2 and neutron-2025-2 for
# release 2025.2); a missing directory for any service is a hard failure.
#
# Each emitted entry carries:
#   service          — service under test (keystone|glance|barbican|neutron)
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
# and, for the neutron service only, additionally:
#   neutron-cr-name  — Neutron CR the CI job waits on; doubles as the K8s
#                      Service name for the neutron port-forward
#   ovn-cr-name      — OVNCentral CR the CI job waits on before the Neutron;
#                      Neutron renders no ml2_conf.ini until it is Ready
#
# For keystone the cr-name/service-k8s-name are keystone-tempest-<slug>; the
# glance leg runs against its own keystone-glance-tempest-<slug> identity CR and
# the glance-tempest-<slug> image CR, the barbican leg against its own
# keystone-barbican-tempest-<slug> identity CR and the barbican-tempest-<slug>
# key-manager CR, the neutron leg against its own keystone-neutron-tempest-<slug>
# identity CR, the neutron-tempest-<slug> network CR and the
# ovn-neutron-tempest-<slug> control plane behind it.
#
# Required env vars:
#   GITHUB_OUTPUT — GitHub Actions output file (set automatically by Actions)
#
# Optional env vars:
#   TEMPEST_SERVICES — Space-separated subset of the covered services to emit
#                      entries for (the tempest-services output of
#                      hack/ci-resolve-changes.sh). Unset or empty emits all
#                      three, so a local run and a skipped job never see an
#                      empty include list. An unknown name is a hard failure.
#                      The missing-directory check below still runs for every
#                      service of every release, whatever this is set to: a
#                      Tempest config directory that disappears must fail the
#                      build on the pull request that removed it, not on the
#                      next one that happens to select that service.
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

# Services this generator knows how to emit, in matrix order.
ALL_TEMPEST_SERVICES=(keystone glance barbican neutron)

# Resolve the selection once, and reject an unknown name before any output line
# is written.
selected=("${ALL_TEMPEST_SERVICES[@]}")
if [[ -n "${TEMPEST_SERVICES:-}" ]]; then
  read -ra selected <<< "${TEMPEST_SERVICES}"
  for want in "${selected[@]}"; do
    known=false
    for svc in "${ALL_TEMPEST_SERVICES[@]}"; do
      [[ "${want}" == "${svc}" ]] && known=true
    done
    if [[ "${known}" != "true" ]]; then
      echo "::error::Unknown tempest service: ${want}"
      exit 1
    fi
  done
fi

is_selected() {
  local want="$1" svc
  for svc in "${selected[@]}"; do
    [[ "${svc}" == "${want}" ]] && return 0
  done
  return 1
}

for d in "${dirs[@]}"; do
  release="${d%/}"
  release="${release##*/}"
  slug="${release//./-}"
  for service in "${ALL_TEMPEST_SERVICES[@]}"; do
    config_dir="tests/tempest/${service}-${slug}"
    if [[ ! -d "${REPO_ROOT}/${config_dir}" ]]; then
      echo "::error::Missing Tempest config directory: ${config_dir} (for service ${service}, release ${release})"
      exit 1
    fi
    # Checked above for every service; emitted only for the selected ones.
    is_selected "${service}" || continue
    cr_name="keystone-tempest-${slug}"
    extra_keys=""
    if [[ "${service}" != "keystone" ]]; then
      # Every non-keystone leg runs against its own Keystone identity CR (waited
      # on and port-forwarded by the CI job) plus its own service CR.
      cr_name="keystone-${service}-tempest-${slug}"
      extra_keys=",\"${service}-cr-name\":\"${service}-tempest-${slug}\""
    fi
    if [[ "${service}" == "neutron" ]]; then
      # The OVNCentral name is emitted here rather than rebuilt in the workflow
      # from the config-dir basename, so ci.yaml holds no copy of it. The
      # fixture tests/tempest/neutron-<slug>/03-ovncentral-cr.yaml holds the
      # other copy, and a rename there would leave CI waiting out its timeout on
      # a resource that does not exist, so
      # tests/unit/ci/neutron_e2e_matrix_test.sh asserts the two agree.
      extra_keys+=",\"ovn-cr-name\":\"ovn-neutron-tempest-${slug}\""
      # The neutron legs run at two stestr workers, not the script's default of
      # four. The tempest container shares the self-hosted runner's four vCPUs
      # with the kind node, and this leg's node also carries the OVN databases,
      # northd, two four-process Neutron API pods and both workers on top of the
      # Keystone stack every leg has. At four workers the first requests of
      # phase 1 starved the node: openstack-db-0 failed its probes and restarted,
      # kube-controller-manager and the keystone-operator went into
      # CrashLoopBackOff, and every tempest.api.network class lost its admin
      # token request to a closed connection. The keystone and glance legs, with
      # no OVN or Neutron on the node, are fine at four.
      extra_keys+=",\"tempest-concurrency\":\"2\""
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
