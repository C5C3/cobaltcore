#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-run-tempest.sh — Run Tempest API tests in CI.
# Feature: CC-0050
#
# CI-specific Tempest execution wrapper that handles port-forwarding,
# config generation, and Docker-based test execution. This is the CI
# counterpart to hack/run-tempest.sh (which handles local execution
# including image building and more complex orchestration).
#
# Required env vars:
#   (none — all have sensible defaults for the keystone test scenario)
#
# Optional env vars:
#   SERVICE       — Service under test (default: keystone)
#   CONFIG_DIR    — Directory containing tempest.conf template, include/exclude
#                   lists (default: tests/tempest/${SERVICE})
#   NAMESPACE     — Kubernetes namespace (default: openstack)
#   ADMIN_SECRET  — Secret name holding admin password (default: keystone-admin)
#   OUTPUT_DIR    — Test output directory (default: _output/tempest)
#   TEMPEST_IMAGE — Tempest container image (default: c5c3/tempest:local)
#
# REQ-004: CI-specific Tempest wrapper script.
# REQ-007: set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
SERVICE="${SERVICE:-keystone}"
CONFIG_DIR="${CONFIG_DIR:-tests/tempest/${SERVICE}}"
NAMESPACE="${NAMESPACE:-openstack}"
ADMIN_SECRET="${ADMIN_SECRET:-keystone-admin}"
OUTPUT_DIR="${OUTPUT_DIR:-_output/tempest}"
TEMPEST_IMAGE="${TEMPEST_IMAGE:-c5c3/tempest:local}"

# Derive the service name used in k8s (e.g. keystone-tempest-api).
SERVICE_K8S_NAME="${SERVICE}-tempest-api"
CATALOG_SVC="${SERVICE_K8S_NAME}.${NAMESPACE}.svc.cluster.local"

# ---------------------------------------------------------------------------
# 1. Prepare output directories
# ---------------------------------------------------------------------------
mkdir -p "${OUTPUT_DIR}" "${OUTPUT_DIR}/config"
chmod o+w "${OUTPUT_DIR}"

# ---------------------------------------------------------------------------
# 2. Extract admin password from Kubernetes secret
# ---------------------------------------------------------------------------
ADMIN_PASSWORD_B64=$(kubectl get secret "${ADMIN_SECRET}" -n "${NAMESPACE}" \
  -o jsonpath='{.data.password}' 2>/dev/null) || {
  echo "::error::Secret '${ADMIN_SECRET}' not found in namespace '${NAMESPACE}'"
  exit 1
}
if [[ -z "${ADMIN_PASSWORD_B64}" ]]; then
  echo "::error::Secret '${ADMIN_SECRET}' in namespace '${NAMESPACE}' is missing the 'password' key"
  exit 1
fi
ADMIN_PASSWORD=$(echo "${ADMIN_PASSWORD_B64}" | base64 -d)

# ---------------------------------------------------------------------------
# 3. Set up port-forward and wait for readiness
# ---------------------------------------------------------------------------
kubectl port-forward "svc/${SERVICE_K8S_NAME}" -n "${NAMESPACE}" 5000:5000 >/dev/null 2>&1 &
PF_PID=$!
trap 'kill "${PF_PID}" 2>/dev/null || true' EXIT

ready=false
for _ in $(seq 1 10); do
  if curl -sf http://localhost:5000/ >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
if [[ "${ready}" != "true" ]]; then
  echo "::error::${SERVICE} API at http://localhost:5000 did not become reachable after 10 attempts"
  exit 1
fi

# ---------------------------------------------------------------------------
# 4. Generate Tempest config from template
# ---------------------------------------------------------------------------
# Escape sed metacharacters in the password to prevent substitution failures
# if ADMIN_PASSWORD contains |, &, or \ (CC-0050, review #2 comment 4).
ADMIN_PASSWORD_ESCAPED=$(printf '%s\n' "${ADMIN_PASSWORD}" | sed 's/[&/\|]/\\&/g')
# Replace both FQDN and short DNS forms of the service URL (CC-0050, review #2 comment 6).
sed -e "s|${SERVICE_K8S_NAME}\\.${NAMESPACE}\\.svc\\.cluster\\.local:5000|localhost:5000|" \
    -e "s|${SERVICE_K8S_NAME}\\.${NAMESPACE}\\.svc:5000|localhost:5000|" \
    -e "s|\${KEYSTONE_ADMIN_PASSWORD}|${ADMIN_PASSWORD_ESCAPED}|" \
    "${CONFIG_DIR}/tempest.conf" > "${OUTPUT_DIR}/config/tempest.conf"

[[ -f "${CONFIG_DIR}/include-tests.txt" ]] && cp "${CONFIG_DIR}/include-tests.txt" "${OUTPUT_DIR}/config/"
[[ -f "${CONFIG_DIR}/exclude-tests.txt" ]] && cp "${CONFIG_DIR}/exclude-tests.txt" "${OUTPUT_DIR}/config/"

# ---------------------------------------------------------------------------
# 5. Build tempest run command
# ---------------------------------------------------------------------------
TEMPEST_CMD="tempest run"
[[ -f "${OUTPUT_DIR}/config/include-tests.txt" ]] && TEMPEST_CMD+=" --include-list /etc/tempest/include-tests.txt"
[[ -f "${OUTPUT_DIR}/config/exclude-tests.txt" ]] && TEMPEST_CMD+=" --exclude-list /etc/tempest/exclude-tests.txt"
TEMPEST_CMD+=" --concurrency 1 --subunit"

# ---------------------------------------------------------------------------
# 6. Run Tempest in container
# ---------------------------------------------------------------------------
# Resolve workspace root for volume mounts. In GitHub Actions this is
# GITHUB_WORKSPACE; locally fall back to the git repo root.
WORKSPACE_ROOT="${GITHUB_WORKSPACE:-$(git rev-parse --show-toplevel)}"

docker run --rm \
  --network host \
  --add-host "${CATALOG_SVC}:127.0.0.1" \
  --add-host "${SERVICE_K8S_NAME}.${NAMESPACE}.svc:127.0.0.1" \
  -v "${WORKSPACE_ROOT}/${OUTPUT_DIR}/config:/etc/tempest:ro" \
  -v "${WORKSPACE_ROOT}/${OUTPUT_DIR}:/output" \
  "${TEMPEST_IMAGE}" \
  bash -c "
    set -euo pipefail
    export HOME=/tmp
    mkdir -p /tmp/tempest-workspace /tmp/tempest-logs
    cd /tmp/tempest-workspace
    tempest init .
    cp /etc/tempest/tempest.conf etc/tempest.conf
    set +e
    ${TEMPEST_CMD} | tee /output/tempest.subunit | subunit2pyunit 2>&1 | grep --line-buffered -E '\.\.\. '
    rc=\${PIPESTATUS[0]}
    set -e
    if [[ -f /output/tempest.subunit ]]; then
      subunit2junitxml < /output/tempest.subunit > /output/tempest-results.xml 2>/dev/null || true
    fi
    # Guard against tempest run --subunit exiting 0 on test failures:
    # check the JUnit XML for reported failures or errors.
    if [[ \${rc} -eq 0 && -f /output/tempest-results.xml ]]; then
      if grep -qE 'failures=\"[1-9]|errors=\"[1-9]' /output/tempest-results.xml; then
        echo '::error::Tempest reported test failures.'
        rc=1
      fi
    fi
    exit \${rc}
  "
