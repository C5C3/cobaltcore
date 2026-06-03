#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-service-image-releases.sh — Print the releases for which an operator
# ships an OpenStack service image.
# Feature: CC-0110
#
# source-refs.yaml is the single source of truth for which (service, release)
# pairs have a service image (mirrors hack/ci-generate-build-matrix.sh). An
# operator has a service image for a release only when
# releases/<release>/source-refs.yaml carries a ref for it. Orchestration
# operators such as c5c3 deploy upstream components via Helm and build no
# OpenStack service image, so they appear in no source-refs.yaml and yield no
# releases here — letting the CI service-image build/push and the e2e-operator
# image-load steps skip them instead of failing on a missing ref.
#
# Prints one release name per line, ascending; empty output when the operator
# has no service image. Reads $OPERATOR from the environment.
#
# REQ-007: set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

OPERATOR="${OPERATOR:?OPERATOR is required (e.g. keystone)}"

shopt -s nullglob
for release_dir in "${REPO_ROOT}"/releases/*/; do
  release="${release_dir%/}"
  release="${release##*/}"
  refs_file="${release_dir}source-refs.yaml"
  [ -f "${refs_file}" ] || continue
  ref=$(yq -r ".\"${OPERATOR}\" // \"\"" "${refs_file}")
  if [ -n "${ref}" ] && [ "${ref}" != "null" ]; then
    echo "${release}"
  fi
done
