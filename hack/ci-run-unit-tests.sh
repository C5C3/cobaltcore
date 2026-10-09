#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-run-unit-tests.sh — Run OpenStack service unit tests in a container.
#
# Runs stestr-based unit tests inside a venv-builder container image. Handles
# volume mounts for source, constraints, test excludes, and result collection.
#
# Required env vars:
#   SERVICE_NAME       — OpenStack service name (e.g. keystone)
#   SERVICE_VERSION    — Version string for PBR PKG-INFO
#   INSTALL_SPEC       — pip install spec (e.g. .[ldap] or .)
#   VENV_BUILDER_IMAGE — Docker image to run tests in
#   RELEASE            — Release directory name (e.g. 2026.1)
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
RELEASE="${RELEASE:?RELEASE is required (e.g. 2026.1)}"

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
# nova's unit suite runs under eventlet, the way its tox py3 env does
# (OS_NOVA_DISABLE_EVENTLET_PATCHING=False in nova's tox.ini). At 33.0.0
# nova/cmd/scheduler.py selects the threading backend at import while
# nova/tests/unit/__init__.py has already selected eventlet, and oslo.service
# raises BackendAlreadySelected during stestr discovery, before any exclude
# list applies. With the variable set, every monkey_patch.patch() call patches
# eventlet and the scheduler import is a no-op. The other services never read
# the variable.
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
  -e OS_NOVA_DISABLE_EVENTLET_PATCHING=False \
  "${VENV_BUILDER_IMAGE}" \
  bash -c '
    set -e
    source /var/lib/openstack/bin/activate
    # pbr only trusts PKG-INFO when its Name matches the package name in
    # setup.cfg / pyproject.toml, and that name can differ from the CI
    # service name (placement ships as openstack-placement). Derive it
    # from the source tree, falling back to SERVICE_NAME.
    PKG_NAME="$(python3 <<"PYEOF"
import configparser, os
name = None
cp = configparser.ConfigParser()
try:
    if cp.read("setup.cfg") and cp.has_option("metadata", "name"):
        name = cp.get("metadata", "name")
except configparser.Error:
    pass
if not name and os.path.exists("pyproject.toml"):
    import tomllib
    with open("pyproject.toml", "rb") as f:
        name = tomllib.load(f).get("project", {}).get("name")
print(name or os.environ["SERVICE_NAME"])
PYEOF
)"
    printf "Metadata-Version: 2.1\nName: %s\nVersion: %s\n" "$PKG_NAME" "$SERVICE_VERSION" > PKG-INFO
    TEST_REQ_ARG=""
    if [ -f test-requirements.txt ]; then
      TEST_REQ_ARG="-r test-requirements.txt"
    fi
    if [ -f .stestr.conf ]; then
      uv pip install --prefix /var/lib/openstack \
        --constraint /workspace/upper-constraints.txt \
        $TEST_REQ_ARG "${INSTALL_SPEC}" stestr testtools
      stestr init
      set +e
      stestr run $EXCLUDE_LIST_ARG; TEST_EXIT=$?
      set -e
      stestr last --subunit > /workspace/results/testresults.subunit || true
    else
      # pytest fallback for services without a .stestr.conf (e.g. horizon,
      # whose Django suite runs under pytest). The exclude-list mechanism
      # (EXCLUDE_LIST_ARG) is stestr-only and intentionally unused here.
      # Services shipping tools/unit_tests.sh (horizon up to 26.x) drive their
      # own pytest invocation with the correct per-project settings modules;
      # horizon 27.0.0+, which dropped the driver, runs the same four
      # projects through the explicit invocations below; anything else falls
      # back to a plain pytest run.
      # hacking mirrors the upstream tox py3 env: it is absent from
      # test-requirements.txt, but the local-hacking-rule unit tests
      # (horizon/test/unit/hacking/) import pycodestyle and fail collection
      # without it. The pin matches horizon tox.ini.
      uv pip install --prefix /var/lib/openstack \
        --constraint /workspace/upper-constraints.txt \
        $TEST_REQ_ARG "${INSTALL_SPEC}" pytest "hacking>=7.0.0,<7.1.0"
      set +e
      if [ -f tools/unit_tests.sh ]; then
        bash tools/unit_tests.sh "$(pwd)"; TEST_EXIT=$?
        # Collect JUnit XML. tools/unit_tests.sh is expected to write reports to
        # test_reports/*.xml; warn loudly when none are found so a green run
        # with an empty result set is diagnosable instead of silently swallowed
        # by the previous "cp ... 2>/dev/null || true".
        copied=0
        for report in test_reports/*.xml; do
          [ -e "$report" ] || continue
          cp "$report" /workspace/results/
          copied=1
        done
        if [ "$copied" -eq 0 ]; then
          echo "::warning::No JUnit XML found under test_reports/*.xml for ${SERVICE_NAME}; result collection is empty" >&2
        fi
      elif [ "${SERVICE_NAME}" = "horizon" ]; then
        # horizon 27.0.0 (2026.2) dropped tools/unit_tests.sh, and its tox.ini
        # runs the four pytest invocations below itself. Each project needs its
        # own Django settings module; a bare pytest run has none, and every
        # test module fails collection with ImproperlyConfigured.
        TEST_EXIT=0
        run_pytest() {
          local name="$1" settings="$2"
          shift 2
          python -m pytest -v --ds="$settings" \
            --junitxml="/workspace/results/${name}_test_results.xml" "$@" || TEST_EXIT=1
        }
        run_pytest openstack_auth openstack_auth.tests.settings openstack_auth
        run_pytest horizon horizon.test.settings horizon
        run_pytest openstack_dashboard openstack_dashboard.test.settings \
          -m "not selenium and not integration and not plugin_test" \
          --ignore=openstack_dashboard/test/selenium openstack_dashboard
        run_pytest plugin openstack_dashboard.test.settings openstack_dashboard/test/test_plugins
      else
        python -m pytest --junitxml=/workspace/results/testresults.xml; TEST_EXIT=$?
      fi
      set -e
    fi
    exit $TEST_EXIT
  '
