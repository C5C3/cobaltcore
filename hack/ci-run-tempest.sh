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
# Runs Tempest in two sequential stestr phases to isolate core tempest.api.*
# from keystone_tempest_plugin.* — they share state via Keystone's dynamic
# service_providers injection, which makes tempest.api.identity.v3.test_tokens.
# TokensV3Test.test_validate_token race against ServiceProvidersTest cleanup
# under parallel execution. Inside each phase, tests still run at the default
# concurrency of 4. See docs/reference/tempest-test-infrastructure.md.
#
# Required env vars:
#   (none — all have sensible defaults for the keystone test scenario)
#
# Optional env vars:
#   SERVICE       — Service under test (default: keystone)
#   CONFIG_DIR    — Directory containing tempest.conf template, include/exclude
#                   lists (default: tests/tempest/${SERVICE}-2025-2)
#   NAMESPACE     — Kubernetes namespace (default: openstack)
#   ADMIN_SECRET  — Secret name holding admin password (default: keystone-admin)
#   OUTPUT_DIR    — Test output directory (default: _output/tempest)
#   TEMPEST_IMAGE    — Tempest container image (default: c5c3/tempest:local)
#   SERVICE_K8S_NAME — K8s Service name for port-forward (default: ${SERVICE}-tempest-2025-2-api)
#   TEMPEST_CONCURRENCY — stestr worker count (default: 4). Must not exceed the
#                   request capacity of the Keystone target (replicas × uwsgi.processes).
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
CONFIG_DIR="${CONFIG_DIR:-tests/tempest/${SERVICE}-2025-2}"
NAMESPACE="${NAMESPACE:-openstack}"
ADMIN_SECRET="${ADMIN_SECRET:-keystone-admin}"
OUTPUT_DIR="${OUTPUT_DIR:-_output/tempest}"
TEMPEST_IMAGE="${TEMPEST_IMAGE:-c5c3/tempest:local}"
TEMPEST_CONCURRENCY="${TEMPEST_CONCURRENCY:-4}"

# Derive the service name used in k8s (e.g. keystone-tempest-2025-2-api).
# CC-0051: Allow override for release-specific CR names (e.g. keystone-tempest-2026-1-api).
SERVICE_K8S_NAME="${SERVICE_K8S_NAME:-${SERVICE}-tempest-2025-2-api}"
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

[[ -f "${CONFIG_DIR}/exclude-tests.txt" ]] && cp "${CONFIG_DIR}/exclude-tests.txt" "${OUTPUT_DIR}/config/"

# ---------------------------------------------------------------------------
# 5. Scope-split the include list into two phase files.
# ---------------------------------------------------------------------------
# Run core tempest.api.* and keystone_tempest_plugin.* in two sequential stestr
# invocations so they never share workers. Keystone injects the current list of
# federation service providers into every token issue/validate response, so
# ServiceProvidersTest cleanups racing against TokensV3Test.test_validate_token
# make the latter flaky under parallel execution. Upstream's keystone-tempest
# gate avoids this by only running keystone_tempest_plugin.* in its job; we run
# both suites and therefore isolate them by time.
PHASES_DIR="${OUTPUT_DIR}/config/phases"
mkdir -p "${PHASES_DIR}"

if [[ ! -f "${CONFIG_DIR}/include-tests.txt" ]]; then
  echo "::error::${CONFIG_DIR}/include-tests.txt is required for scope-split execution"
  exit 1
fi

grep -E '^tempest\.' "${CONFIG_DIR}/include-tests.txt" \
  > "${PHASES_DIR}/phase-1-core.txt" || true
grep -E '^keystone_tempest_plugin\.' "${CONFIG_DIR}/include-tests.txt" \
  > "${PHASES_DIR}/phase-2-plugin.txt" || true

# Invariant: every non-comment, non-empty line in include-tests.txt must land
# in exactly one phase file. Unknown prefixes would otherwise be silently
# dropped and leave tests unrun.
TOTAL_PATTERNS=$(grep -cE '^[^#[:space:]]' \
  "${CONFIG_DIR}/include-tests.txt" || true)
PHASE1_COUNT=$(wc -l < "${PHASES_DIR}/phase-1-core.txt" | tr -d ' ')
PHASE2_COUNT=$(wc -l < "${PHASES_DIR}/phase-2-plugin.txt" | tr -d ' ')
COVERED=$((PHASE1_COUNT + PHASE2_COUNT))

if [[ "${COVERED}" -ne "${TOTAL_PATTERNS}" ]]; then
  echo "::error::Scope-split does not cover include-tests.txt: ${TOTAL_PATTERNS} patterns, ${COVERED} covered (phase1=${PHASE1_COUNT}, phase2=${PHASE2_COUNT}). Every non-comment line must start with 'tempest.' or 'keystone_tempest_plugin.'."
  exit 1
fi
if [[ "${PHASE1_COUNT}" -eq 0 || "${PHASE2_COUNT}" -eq 0 ]]; then
  echo "::error::Scope-split produced an empty phase (phase1=${PHASE1_COUNT}, phase2=${PHASE2_COUNT}). Both phases must have at least one pattern."
  exit 1
fi

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

    exclude_args=''
    if [[ -f /etc/tempest/exclude-tests.txt ]]; then
      exclude_args='--exclude-list /etc/tempest/exclude-tests.txt'
    fi

    run_phase() {
      local phase=\$1
      echo
      echo \"::group::Tempest phase: \${phase}\"
      set +e
      # shellcheck disable=SC2086  # intentional word-splitting on exclude_args
      stestr run --include-list /etc/tempest/phases/\${phase}.txt \${exclude_args} --concurrency ${TEMPEST_CONCURRENCY} --subunit | tee /output/\${phase}.subunit | subunit2pyunit 2>&1 | grep --line-buffered -E '\.\.\. '
      local phase_rc=\${PIPESTATUS[0]}
      set -e
      echo '::endgroup::'
      return \${phase_rc}
    }

    overall_rc=0
    run_phase phase-1-core || overall_rc=\$?
    phase2_rc=0
    run_phase phase-2-plugin || phase2_rc=\$?
    if [[ \${phase2_rc} -gt \${overall_rc} ]]; then
      overall_rc=\${phase2_rc}
    fi

    # Merge the two subunit streams into the single artifact the CI upload
    # step expects. Subunit v2 is stream-concatenation safe by design.
    cat /output/phase-1-core.subunit /output/phase-2-plugin.subunit > /output/tempest.subunit
    subunit2junitxml < /output/tempest.subunit > /output/tempest-results.xml 2>/dev/null || true

    # Guard against stestr run --subunit exiting 0 on test failures:
    # check the JUnit XML for reported failures or errors.
    if [[ \${overall_rc} -eq 0 && -f /output/tempest-results.xml ]]; then
      if grep -qE 'failures=\"[1-9]|errors=\"[1-9]' /output/tempest-results.xml; then
        echo '::error::Tempest reported test failures.'
        overall_rc=1
      fi
    fi
    exit \${overall_rc}
  "
