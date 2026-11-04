#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-operator-upgrade-leg.sh — Print what one operator's helm upgrade
# suite (tests/e2e-operator-upgrade/<op>-helm-upgrade/) needs on its kind
# cluster: the images it loads, the infrastructure it opts into, and the
# operators and CRDs that run beside it.
#
# The table below is the one place that knows it. The reusable workflow
# .github/workflows/e2e-operator-upgrade.yaml reads it to prepare each matrix
# job, and hack/ci-operator-upgrade-main-matrix.sh reads it to map every image
# a main-side run loads. Each row mirrors what the e2e-operator job of
# .github/workflows/ci.yaml gives the e2e fixtures the suite copies.
#
# Required env vars:
#   OPERATOR      — Operator name (e.g. keystone)
#
# Optional env vars:
#   IMAGE_PREFIX  — Registry and owner every ref is prefixed with (default:
#                   ghcr.io/c5c3)
#   GITHUB_OUTPUT — Output file the values are appended to (default: /dev/null)
#
# Writes to GITHUB_OUTPUT and to stdout:
#   refs                    — heredoc block, one image ref per line
#   with-nfs                — "true" or empty
#   with-messaging          — "true" or empty
#   with-ovn-kernel-modules — "true" or empty
#   siblings                — space-separated operators deployed at :dev before
#                             the baseline
#   sibling-crds            — "true" when the c5c3-operator's watched CRDs and
#                             K-ORC are installed first, otherwise empty
#   with-chaos-mesh         — "true" or empty; deploy-infra installs Chaos Mesh
#                             for the second pass below
#   pod-kill-suite          — "true" when the job runs
#                             tests/e2e-operator-upgrade/<op>-helm-upgrade-pod-kill/
#                             as a second pass over a reinstalled baseline,
#                             otherwise empty; keystone alone sets it
#
# The ovn and neutron rows read the OVN version from
# hack/ci-resolve-ovn-version.sh. Errors go to stderr, so a caller that captures
# stdout still shows them.
#
# set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

OPERATOR="${OPERATOR:-}"
if [[ -z "${OPERATOR}" ]]; then
  echo "::error::OPERATOR is required (e.g. keystone)" >&2
  exit 1
fi
if [[ ! "${OPERATOR}" =~ ^[a-z][a-z0-9-]*$ ]]; then
  echo "::error::OPERATOR must be lowercase alphanumeric (with hyphens), got '${OPERATOR}'" >&2
  exit 1
fi
IMAGE_PREFIX="${IMAGE_PREFIX:-ghcr.io/c5c3}"
GITHUB_OUTPUT="${GITHUB_OUTPUT:-/dev/null}"

# The release every upgrade fixture pins (spec.image.tag and
# status.installedRelease); it moves with them, and
# tests/unit/hack/ci_operator_upgrade_leg_test.sh fails when the two differ.
RELEASE="2026.1"

refs=""
siblings=""
with_nfs=""
with_messaging=""
with_ovn_kernel_modules=""
sibling_crds=""
with_chaos_mesh=""
pod_kill_suite=""

case "${OPERATOR}" in
  # The second pass kills every operator pod in the middle of the helm
  # upgrade, under the token stream of tests/e2e-chaos/lib/workload.sh, whose
  # pod runs on the Tempest image.
  keystone)
    refs="keystone-operator:dev keystone:${RELEASE} tempest:${RELEASE}"
    with_chaos_mesh=true
    pod_kill_suite=true
    ;;
  horizon | glance | placement | barbican)
    refs="${OPERATOR}-operator:dev ${OPERATOR}:${RELEASE}"
    ;;
  # The ControlPlane controller watches the keystone, horizon, glance,
  # placement and K-ORC kinds, and its informers never sync without their
  # CRDs. The suite asserts the InfrastructureReady path, which needs no
  # sibling controller and no service image.
  c5c3)
    refs="c5c3-operator:dev"
    sibling_crds=true
    ;;
  # The ovn-operator rolls out the one OVN daemon image it hard-codes.
  ovn)
    ovn_version="$("${SCRIPT_DIR}/ci-resolve-ovn-version.sh")"
    refs="ovn-operator:dev ovn:${ovn_version}"
    with_ovn_kernel_modules=true
    ;;
  # A Neutron never reaches Ready without a live OVNCentral, so the
  # ovn-operator runs beside it.
  neutron)
    ovn_version="$("${SCRIPT_DIR}/ci-resolve-ovn-version.sh")"
    refs="neutron-operator:dev neutron:${RELEASE} ovn-operator:dev ovn:${ovn_version}"
    siblings="ovn"
    with_ovn_kernel_modules=true
    ;;
  # The volume service mounts the kind NFS export, and the suite takes a
  # vhost on the kind broker.
  cinder)
    refs="cinder-operator:dev cinder:${RELEASE}"
    with_nfs=true
    with_messaging=true
    ;;
  # The light tier: a Nova reaches Ready on Secrets and the broker alone, so
  # no sibling operator runs.
  nova)
    refs="nova-operator:dev nova:${RELEASE}"
    with_messaging=true
    ;;
  *)
    echo "::error::no upgrade leg is defined for operator '${OPERATOR}'; add it to hack/ci-operator-upgrade-leg.sh" >&2
    exit 1
    ;;
esac

{
  echo "refs<<EOF"
  for ref in ${refs}; do
    echo "${IMAGE_PREFIX}/${ref}"
  done
  echo "EOF"
  echo "with-nfs=${with_nfs}"
  echo "with-messaging=${with_messaging}"
  echo "with-ovn-kernel-modules=${with_ovn_kernel_modules}"
  echo "siblings=${siblings}"
  echo "sibling-crds=${sibling_crds}"
  echo "with-chaos-mesh=${with_chaos_mesh}"
  echo "pod-kill-suite=${pod_kill_suite}"
} | tee -a "${GITHUB_OUTPUT}"
