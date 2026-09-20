#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-generate-tempest-matrix.sh — Generate Tempest release matrix from releases/ directories.
#
# Scans releases/*/ directories and, for each release, emits one matrix entry
# per Tempest-covered service (keystone, glance, barbican, neutron, cinder,
# nova). Each service requires a matching Tempest config directory at
# tests/tempest/<service>-<slug>/ (e.g. keystone-2025-2, glance-2025-2,
# barbican-2025-2, neutron-2025-2, cinder-2025-2 and nova-2025-2 for release
# 2025.2); a missing directory for any service is a hard failure.
#
# Each emitted entry carries:
#   service          — service under test
#                      (keystone|glance|barbican|neutron|cinder|nova)
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
# and, for the cinder service only, additionally:
#   cinder-cr-name   — Cinder CR the CI job waits on; doubles as the K8s Service
#                      name for the cinder port-forward
#   glance-cr-name   — Glance CR of the cinder leg's own image service, waited on
#                      before the Cinder and port-forwarded like the glance leg's
#   nova-cr-name     — Nova CR the compute-tagged volume tests boot a server on;
#                      doubles as the K8s Service name for the compute
#                      port-forward
#   ovn-cr-name      — OVNCentral CR behind that Nova's Neutron
#   neutron-cr-name  — Neutron CR the leg's Nova needs reachable to run at all,
#                      and that tempest's dynamic credentials clean up security
#                      groups through; the leg's servers carry no NIC, so no
#                      port is bound there. Doubles as the K8s Service name for
#                      the network port-forward
#   placement-cr-name — Placement CR the leg's Nova reports its inventory to
#   tempest-concurrency — stestr worker count for the leg (2)
# and, for the nova service only, additionally:
#   glance-cr-name   — Glance CR the compute tests boot their servers from;
#                      doubles as the K8s Service name for the image port-forward
#   plus the five compute-stack keys described under the cinder service above:
#   nova-cr-name, ovn-cr-name, neutron-cr-name, placement-cr-name and
#   tempest-concurrency. On a nova entry nova-cr-name is the leg's own service
#   CR, and its servers do carry a NIC, so a port is bound on that Neutron.
#
# For keystone the cr-name/service-k8s-name are keystone-tempest-<slug>; the
# glance leg runs against its own keystone-glance-tempest-<slug> identity CR and
# the glance-tempest-<slug> image CR, the barbican leg against its own
# keystone-barbican-tempest-<slug> identity CR and the barbican-tempest-<slug>
# key-manager CR, the neutron leg against its own keystone-neutron-tempest-<slug>
# identity CR, the neutron-tempest-<slug> network CR and the
# ovn-neutron-tempest-<slug> control plane behind it, and the cinder leg against
# its own keystone-cinder-tempest-<slug> identity CR, the cinder-tempest-<slug>
# volume CR, the glance-cinder-tempest-<slug> image CR the volume tests create
# from and the compute stack its compute-tagged tests boot a server on
# (nova-cinder-tempest-<slug>, placement-cinder-tempest-<slug>,
# neutron-cinder-tempest-<slug> and ovn-cinder-tempest-<slug>). The nova leg runs
# against its own keystone-nova-tempest-<slug> identity CR, the
# nova-tempest-<slug> compute CR and the placement-nova-tempest-<slug>,
# neutron-nova-tempest-<slug>, ovn-nova-tempest-<slug> and
# glance-nova-tempest-<slug> services that compute needs.
#
# Required env vars:
#   GITHUB_OUTPUT — GitHub Actions output file (set automatically by Actions)
#
# Optional env vars:
#   TEMPEST_SERVICES — Space-separated subset of the covered services to emit
#                      entries for (the tempest-services output of
#                      hack/ci-resolve-changes.sh). Unset or empty emits every
#                      service, so a local run and a skipped job never see an
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
ALL_TEMPEST_SERVICES=(keystone glance barbican neutron cinder nova)

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
    if [[ "${service}" == "cinder" ]]; then
      # The cinder leg carries a Glance of its own: the volume suites create
      # volumes from an image and upload volumes back to one, so the leg deploys
      # an image service next to the Cinder and waits on it. The name is emitted
      # here for the same reason ovn-cr-name is, and
      # tests/unit/ci/cinder_e2e_matrix_test.sh holds it against the fixture.
      extra_keys+=",\"glance-cr-name\":\"glance-cinder-tempest-${slug}\""
      # The compute stack the compute-tagged volume tests boot a server on: a
      # Nova with one fake compute, the Placement it reports inventory to, the
      # Neutron that Nova needs reachable to run at all and that tempest's
      # dynamic credentials clean up security groups through, and the OVNCentral
      # behind that Neutron. The leg's servers carry no NIC
      # (create_isolated_networks = false in its tempest.conf), so no port is
      # bound there. The names are emitted here for the same reason
      # glance-cr-name is, and the fixtures
      # tests/tempest/cinder-<slug>/09-ovncentral-cr.yaml, 10-neutron-cr.yaml,
      # 11-placement-cr.yaml and 13-nova-cr.yaml hold the other copy of each;
      # tests/unit/ci/cinder_e2e_matrix_test.sh holds the two sides together.
      extra_keys+=",\"nova-cr-name\":\"nova-cinder-tempest-${slug}\""
      extra_keys+=",\"ovn-cr-name\":\"ovn-cinder-tempest-${slug}\""
      extra_keys+=",\"neutron-cr-name\":\"neutron-cinder-tempest-${slug}\""
      extra_keys+=",\"placement-cr-name\":\"placement-cinder-tempest-${slug}\""
      # Two stestr workers rather than the script's default of four. This leg's
      # node carries the shared broker, the NFS server and four Cinder workloads
      # (api, scheduler, volume, backup) plus a Glance and the Keystone stack
      # every leg has, on the four vCPUs the tempest container shares with it.
      extra_keys+=",\"tempest-concurrency\":\"2\""
    fi
    if [[ "${service}" == "nova" ]]; then
      # The services a compute needs, each emitted here rather than rebuilt in
      # the workflow from the config-dir basename, so ci.yaml holds no copy of
      # them. The fixtures tests/tempest/nova-<slug>/03-ovncentral-cr.yaml,
      # 04-neutron-cr.yaml, 05-placement-cr.yaml, 06-glance-cr.yaml and
      # 10-nova-cr.yaml hold the other copy of each name, and a rename there
      # would leave CI waiting out its timeout on a resource that does not
      # exist, so tests/unit/ci/nova_e2e_matrix_test.sh asserts the two agree.
      extra_keys+=",\"ovn-cr-name\":\"ovn-nova-tempest-${slug}\""
      extra_keys+=",\"neutron-cr-name\":\"neutron-nova-tempest-${slug}\""
      extra_keys+=",\"placement-cr-name\":\"placement-nova-tempest-${slug}\""
      extra_keys+=",\"glance-cr-name\":\"glance-nova-tempest-${slug}\""
      # Two stestr workers, the count the Phase-0 lab of #1015 ran the compute
      # suite at. The node carries the whole compute stack on top of the
      # Keystone stack every leg has: the OVN databases, northd and a chassis,
      # a Neutron, a Placement, a Glance, the five Nova workloads (api,
      # conductor, scheduler, metadata, novncproxy) and the fake compute.
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
