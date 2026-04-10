#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-run-unit-tests.sh — Run OpenStack service unit tests in a container.
# Feature: CC-0055
#
# Runs stestr-based unit tests inside a venv-builder container image. Handles
# volume mounts for source, constraints, test excludes, and result collection.
#
# Required env vars:
#   SERVICE_NAME       — OpenStack service name (e.g. keystone)
#   SERVICE_VERSION    — Version string for PBR PKG-INFO
#   INSTALL_SPEC       — pip install spec (e.g. .[ldap] or .)
#   VENV_BUILDER_IMAGE — Docker image to run tests in
#   RELEASE            — Release directory name (e.g. 2025.2)
#
# Optional env vars:
#   WORKSPACE_DIR                  — Root workspace directory (default: $GITHUB_WORKSPACE or pwd)
#   OS_TEST_DBAPI_ADMIN_CONNECTION — oslo.db admin connection string

set -euo pipefail

# ---------------------------------------------------------------------------
# Validate required env vars
# ---------------------------------------------------------------------------
SERVICE_NAME="${SERVICE_NAME:?SERVICE_NAME is required (e.g. keystone)}"
SERVICE_VERSION="${SERVICE_VERSION:?SERVICE_VERSION is required}"
# INSTALL_SPEC is guaranteed non-empty by the :? guard below. When pip_extras
# is empty, ci-resolve-extra-packages.sh produces "." (bare package), which is
# a valid pip install spec.
INSTALL_SPEC="${INSTALL_SPEC:?INSTALL_SPEC is required (e.g. . or .[ldap])}"
VENV_BUILDER_IMAGE="${VENV_BUILDER_IMAGE:?VENV_BUILDER_IMAGE is required}"
RELEASE="${RELEASE:?RELEASE is required (e.g. 2025.2)}"

# ---------------------------------------------------------------------------
# Resolve workspace directory
# ---------------------------------------------------------------------------
WORKSPACE_DIR="${WORKSPACE_DIR:-${GITHUB_WORKSPACE:-$(pwd)}}"

# ---------------------------------------------------------------------------
# Validate source directory exists
# ---------------------------------------------------------------------------
if [[ ! -d "${WORKSPACE_DIR}/src/${SERVICE_NAME}" ]]; then
  echo "::error::Source directory not found: ${WORKSPACE_DIR}/src/${SERVICE_NAME}"
  exit 1
fi

# ---------------------------------------------------------------------------
# 1. Create output and exclude directories
# ---------------------------------------------------------------------------
mkdir -p "${WORKSPACE_DIR}/results" "${WORKSPACE_DIR}/releases/${RELEASE}/test-excludes"

# ---------------------------------------------------------------------------
# 2. Build exclude-list argument if service-specific file exists
# ---------------------------------------------------------------------------
# NOTE: EXCLUDE_LIST_ARG and TEST_REQ_ARG are intentionally unquoted in the
# inner bash -c script so that word splitting produces separate arguments.
# This is safe because SERVICE_NAME comes from a controlled CI matrix and
# must not contain spaces or shell metacharacters.
EXCLUDE_LIST_ARG=""
if [ -f "${WORKSPACE_DIR}/releases/${RELEASE}/test-excludes/${SERVICE_NAME}.txt" ]; then
  EXCLUDE_LIST_ARG="--exclude-list /workspace/test-excludes/${SERVICE_NAME}.txt"
fi

# ---------------------------------------------------------------------------
# 3. Run unit tests in container
# ---------------------------------------------------------------------------
docker run --rm --network host \
  -v "${WORKSPACE_DIR}/src/${SERVICE_NAME}:/workspace/src:rw" \
  -v "${WORKSPACE_DIR}/releases/${RELEASE}/upper-constraints.txt:/workspace/upper-constraints.txt:ro" \
  -v "${WORKSPACE_DIR}/releases/${RELEASE}/test-excludes:/workspace/test-excludes:ro" \
  -v "${WORKSPACE_DIR}/results:/workspace/results" \
  -w /workspace/src \
  -e EXCLUDE_LIST_ARG="${EXCLUDE_LIST_ARG}" \
  -e SERVICE_NAME="${SERVICE_NAME}" \
  -e SERVICE_VERSION="${SERVICE_VERSION}" \
  -e INSTALL_SPEC="${INSTALL_SPEC}" \
  -e OS_TEST_DBAPI_ADMIN_CONNECTION="${OS_TEST_DBAPI_ADMIN_CONNECTION:-}" \
  "${VENV_BUILDER_IMAGE}" \
  bash -c '
    set -e
    source /var/lib/openstack/bin/activate
    printf "Metadata-Version: 2.1\nName: %s\nVersion: %s\n" "$SERVICE_NAME" "$SERVICE_VERSION" > PKG-INFO
    TEST_REQ_ARG=""
    if [ -f test-requirements.txt ]; then
      TEST_REQ_ARG="-r test-requirements.txt"
    fi
    uv pip install --prefix /var/lib/openstack \
      --constraint /workspace/upper-constraints.txt \
      $TEST_REQ_ARG "${INSTALL_SPEC}" stestr testtools
    stestr init
    set +e
    stestr run $EXCLUDE_LIST_ARG; TEST_EXIT=$?
    set -e
    stestr last --subunit > /workspace/results/testresults.subunit || true
    exit $TEST_EXIT
  '
