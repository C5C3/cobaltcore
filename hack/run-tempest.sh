#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/run-tempest.sh — Run Tempest API tests against a service in the kind cluster.
# Feature: CC-0035
#
# Orchestrates the Tempest test lifecycle:
#   1. Validate SERVICE argument and test configuration files
#   2. Build tempest container image chain (python-base → venv-builder → tempest)
#   3. Load tempest image into the kind cluster
#   4. Create ConfigMap with test configuration (tempest.conf, include/exclude lists)
#   5. Run Tempest as a Kubernetes pod with credentials from K8s secrets
#   6. Collect JUnit XML results and pod logs
#
# REQ-006: Orchestration script for local Tempest execution.
# REQ-011: set -euo pipefail, SPDX Apache-2.0 header, CC-0035 reference.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ---------------------------------------------------------------------------
# Configuration (CC-0035 REQ-006)
# ---------------------------------------------------------------------------
CLUSTER_NAME="${CLUSTER_NAME:-forge-e2e}"
TEMPEST_TIMEOUT="${TEMPEST_TIMEOUT:-600}"
TEMPEST_IMAGE="${TEMPEST_IMAGE:-c5c3/tempest:local}"
TEMPEST_NAMESPACE="${TEMPEST_NAMESPACE:-openstack}"
OUTPUT_DIR="${OUTPUT_DIR:-${REPO_ROOT}/_output/reports}"

# ---------------------------------------------------------------------------
# log — Print a timestamped log message (ISO 8601 UTC).
# Matches the pattern from deploy-infra.sh (CC-0010).
# ---------------------------------------------------------------------------
log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

# ---------------------------------------------------------------------------
# usage — Print usage information and exit.
# ---------------------------------------------------------------------------
usage() {
  cat <<EOF
Usage: $0 <SERVICE>

Run Tempest API tests against a service in the kind cluster.

Arguments:
  SERVICE    The service to test (e.g., keystone). Must have a configuration
             directory at tests/tempest/<SERVICE>/ containing tempest.conf,
             include-tests.txt, and exclude-tests.txt.

Environment variables:
  CLUSTER_NAME       Kind cluster name (default: forge-e2e)
  TEMPEST_TIMEOUT    Timeout in seconds for Tempest pod (default: 600)
  TEMPEST_IMAGE      Tempest Docker image tag (default: c5c3/tempest:local)
  TEMPEST_NAMESPACE  Kubernetes namespace for Tempest pod (default: openstack)
  OUTPUT_DIR         Output directory for JUnit XML reports (default: _output/reports)
  SKIP_BUILD         Set to "true" to skip building the tempest image (default: false)

Examples:
  $0 keystone
  TEMPEST_TIMEOUT=900 $0 keystone
  SKIP_BUILD=true $0 keystone
EOF
}

# ---------------------------------------------------------------------------
# validate — Verify prerequisites before running Tempest.
# ---------------------------------------------------------------------------
validate() {
  local service="$1"

  if [[ -z "${service}" ]]; then
    log "ERROR: SERVICE argument is required."
    usage
    exit 1
  fi

  local config_dir="${REPO_ROOT}/tests/tempest/${service}"
  for file in tempest.conf include-tests.txt exclude-tests.txt; do
    if [[ ! -f "${config_dir}/${file}" ]]; then
      log "ERROR: Required file not found: ${config_dir}/${file}"
      exit 1
    fi
  done

  for cmd in docker kind kubectl yq; do
    if ! command -v "${cmd}" &>/dev/null; then
      log "ERROR: '${cmd}' is not installed or not in PATH."
      exit 1
    fi
  done

  if ! kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
    log "ERROR: Kind cluster '${CLUSTER_NAME}' does not exist."
    log "Run 'make deploy-infra' first."
    exit 1
  fi
}

# ---------------------------------------------------------------------------
# build_tempest_image — Build the tempest image chain from version pins.
# Reads versions from releases/2025.2/test-refs.yaml (CC-0035 REQ-001).
# ---------------------------------------------------------------------------
build_tempest_image() {
  if [[ "${SKIP_BUILD:-false}" == "true" ]]; then
    log "SKIP_BUILD=true — skipping image build."
    return 0
  fi

  local tempest_version keystone_plugin_version
  tempest_version=$(yq '.tempest' "${REPO_ROOT}/releases/2025.2/test-refs.yaml")
  keystone_plugin_version=$(yq '."keystone-tempest-plugin"' "${REPO_ROOT}/releases/2025.2/test-refs.yaml")

  if [[ -z "${tempest_version}" || "${tempest_version}" == "null" ]]; then
    log "ERROR: tempest version not found in releases/2025.2/test-refs.yaml"
    exit 1
  fi
  if [[ -z "${keystone_plugin_version}" || "${keystone_plugin_version}" == "null" ]]; then
    log "ERROR: keystone-tempest-plugin version not found in releases/2025.2/test-refs.yaml"
    exit 1
  fi

  log "Building tempest image chain (tempest=${tempest_version}, keystone-tempest-plugin=${keystone_plugin_version})..."

  log "  Building python-base..."
  docker build -t python-base "${REPO_ROOT}/images/python-base/"

  log "  Building venv-builder..."
  docker build -t venv-builder "${REPO_ROOT}/images/venv-builder/"

  log "  Building tempest..."
  docker build -t "${TEMPEST_IMAGE}" \
    --build-arg "TEMPEST_VERSION=${tempest_version}" \
    --build-arg "KEYSTONE_TEMPEST_PLUGIN_VERSION=${keystone_plugin_version}" \
    --build-context "upper-constraints=${REPO_ROOT}/releases/2025.2" \
    "${REPO_ROOT}/images/tempest/"

  log "Tempest image built: ${TEMPEST_IMAGE}"
}

# ---------------------------------------------------------------------------
# load_image — Load the tempest image into the kind cluster.
# ---------------------------------------------------------------------------
load_image() {
  log "Loading tempest image into kind cluster '${CLUSTER_NAME}'..."
  kind load docker-image "${TEMPEST_IMAGE}" --name "${CLUSTER_NAME}"
  log "Image loaded."
}

# ---------------------------------------------------------------------------
# create_configmap — Create a ConfigMap with Tempest test configuration.
# ---------------------------------------------------------------------------
create_configmap() {
  local service="$1"
  local config_dir="${REPO_ROOT}/tests/tempest/${service}"
  local cm_name="tempest-config-${service}"

  log "Creating ConfigMap '${cm_name}' in namespace '${TEMPEST_NAMESPACE}'..."

  # Delete existing ConfigMap if present (idempotent).
  kubectl delete configmap "${cm_name}" -n "${TEMPEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null

  kubectl create configmap "${cm_name}" \
    -n "${TEMPEST_NAMESPACE}" \
    --from-file="tempest.conf=${config_dir}/tempest.conf" \
    --from-file="include-tests.txt=${config_dir}/include-tests.txt" \
    --from-file="exclude-tests.txt=${config_dir}/exclude-tests.txt"

  log "ConfigMap '${cm_name}' created."
}

# ---------------------------------------------------------------------------
# run_tempest — Run Tempest as a Kubernetes pod and wait for completion.
#
# The pod:
#   - Mounts test config from ConfigMap (tempest.conf, include/exclude lists)
#   - Injects admin credentials from the keystone-admin K8s Secret
#   - Runs tempest, converts subunit output to JUnit XML
#   - Stores results at /tmp/tempest-results.xml for collection
# ---------------------------------------------------------------------------
run_tempest() {
  local service="$1"
  local pod_name="tempest-${service}"
  local cm_name="tempest-config-${service}"

  # Clean up any previous pod.
  kubectl delete pod "${pod_name}" -n "${TEMPEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null

  log "Running Tempest pod '${pod_name}' in namespace '${TEMPEST_NAMESPACE}'..."

  # Apply the pod manifest. The inline script:
  #   1. Substitutes the admin password placeholder in tempest.conf
  #   2. Initializes a Tempest workspace
  #   3. Runs Tempest with include/exclude lists
  #   4. Converts results to JUnit XML regardless of test outcome
  cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${pod_name}
  namespace: ${TEMPEST_NAMESPACE}
  labels:
    app.kubernetes.io/name: tempest
    app.kubernetes.io/component: api-test
    c5c3.io/service: ${service}
spec:
  restartPolicy: Never
  containers:
    - name: tempest
      image: ${TEMPEST_IMAGE}
      imagePullPolicy: Never
      command:
        - /bin/bash
        - -c
        - |
          set -euo pipefail
          TEMPEST_RC=0

          mkdir -p /tmp/tempest-logs

          # Ensure Tempest logs are always printed to stdout for kubectl logs.
          trap 'echo "=== Tempest log ===" && cat /tmp/tempest-logs/tempest.log 2>/dev/null || echo "(no log file)" && echo "=== End Tempest log ==="' EXIT

          echo "Initializing Tempest workspace..."
          cd /tmp
          tempest init workspace
          cd workspace
          sed "s|\\\${KEYSTONE_ADMIN_PASSWORD}|\${KEYSTONE_ADMIN_PASSWORD}|" \
            /mnt/tempest-config/tempest.conf > etc/tempest.conf

          # Verify password was substituted (diagnostic, no secrets in logs).
          if grep -q '\${KEYSTONE_ADMIN_PASSWORD}' etc/tempest.conf; then
            echo "ERROR: Password placeholder was not substituted in tempest.conf"
            exit 1
          fi
          echo "Config ready (admin_password length: \${#KEYSTONE_ADMIN_PASSWORD})"

          # Pre-flight: verify admin auth works before running Tempest.
          echo "Verifying admin authentication..."
          python3 << 'PYEOF'
          import json, urllib.request, urllib.error, os, sys, configparser
          c = configparser.ConfigParser()
          c.read('etc/tempest.conf')
          base = c.get('identity', 'uri_v3')
          pw = os.environ['KEYSTONE_ADMIN_PASSWORD']
          body = json.dumps({'auth': {'identity': {'methods': ['password'],
              'password': {'user': {'name': 'admin', 'password': pw,
                                    'domain': {'name': 'Default'}}}}}}).encode()
          req = urllib.request.Request(f'{base}/auth/tokens', body,
                                      headers={'Content-Type': 'application/json'})
          try:
              r = urllib.request.urlopen(req)
              print(f'Admin auth OK (HTTP {r.status})')
          except urllib.error.HTTPError as e:
              print(f'Admin auth FAILED (HTTP {e.code}): {e.read().decode()[:300]}')
              sys.exit(1)
          PYEOF

          echo "Running Tempest tests..."
          tempest run \
            --include-list /mnt/tempest-config/include-tests.txt \
            --exclude-list /mnt/tempest-config/exclude-tests.txt \
            || TEMPEST_RC=\$?

          echo "Converting results to JUnit XML..."
          tempest last --subunit 2>/dev/null \
            | subunit2junitxml > /tmp/tempest-results.xml 2>/dev/null || true

          exit \${TEMPEST_RC}
      env:
        - name: TEMPEST_CONFIG_DIR
          value: /tmp/workspace/etc
        - name: KEYSTONE_ADMIN_PASSWORD
          valueFrom:
            secretKeyRef:
              name: keystone-admin
              key: password
      volumeMounts:
        - name: tempest-config
          mountPath: /mnt/tempest-config
          readOnly: true
  volumes:
    - name: tempest-config
      configMap:
        name: ${cm_name}
EOF

  log "Waiting up to ${TEMPEST_TIMEOUT}s for Tempest pod to complete..."

  local elapsed=0
  local phase=""
  while [[ "${elapsed}" -lt "${TEMPEST_TIMEOUT}" ]]; do
    phase=$(kubectl get pod "${pod_name}" -n "${TEMPEST_NAMESPACE}" \
      -o jsonpath='{.status.phase}' 2>/dev/null) || true
    case "${phase}" in
      Succeeded) return 0 ;;
      Failed)
        log "Tempest tests reported failures."
        return 1
        ;;
    esac
    sleep 5
    elapsed=$((elapsed + 5))
  done
  log "ERROR: Tempest pod did not complete within ${TEMPEST_TIMEOUT}s (phase: ${phase:-Unknown})."
  return 1
}

# ---------------------------------------------------------------------------
# collect_results — Copy JUnit XML and logs from the Tempest pod.
# ---------------------------------------------------------------------------
collect_results() {
  local service="$1"
  local pod_name="tempest-${service}"

  mkdir -p "${OUTPUT_DIR}"

  log "Collecting Tempest results..."

  # Copy JUnit XML from the pod (best-effort).
  kubectl cp "${TEMPEST_NAMESPACE}/${pod_name}:/tmp/tempest-results.xml" \
    "${OUTPUT_DIR}/tempest-${service}-results.xml" 2>/dev/null || true

  # Print pod logs for CI visibility.
  log "--- Tempest pod logs ---"
  kubectl logs "${pod_name}" -n "${TEMPEST_NAMESPACE}" || true
  log "--- End of Tempest pod logs ---"

  if [[ -f "${OUTPUT_DIR}/tempest-${service}-results.xml" ]]; then
    log "JUnit XML report: ${OUTPUT_DIR}/tempest-${service}-results.xml"
  else
    log "WARNING: JUnit XML report not found in pod."
  fi
}

# ---------------------------------------------------------------------------
# cleanup — Remove the Tempest pod and ConfigMap.
# ---------------------------------------------------------------------------
cleanup() {
  local service="$1"
  local pod_name="tempest-${service}"
  local cm_name="tempest-config-${service}"

  log "Cleaning up Tempest resources..."
  kubectl delete pod "${pod_name}" -n "${TEMPEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
  kubectl delete configmap "${cm_name}" -n "${TEMPEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
}

# ---------------------------------------------------------------------------
# main — Orchestrate the Tempest test lifecycle.
# ---------------------------------------------------------------------------
main() {
  local service="${1:-}"

  log "=========================================="
  log "  Tempest API Tests (CC-0035)"
  log "=========================================="

  validate "${service}"

  log "Service            : ${service}"
  log "Cluster            : ${CLUSTER_NAME}"
  log "Timeout            : ${TEMPEST_TIMEOUT}s"
  log "Image              : ${TEMPEST_IMAGE}"
  log "Output             : ${OUTPUT_DIR}"
  log ""

  build_tempest_image
  load_image
  create_configmap "${service}"

  local rc=0
  run_tempest "${service}" || rc=$?

  # Always collect results regardless of test outcome.
  collect_results "${service}"

  # Clean up pod and ConfigMap to prevent resource accumulation across local retries.
  cleanup "${service}"

  if [[ "${rc}" -eq 0 ]]; then
    log ""
    log "=========================================="
    log "  Tempest tests PASSED"
    log "=========================================="
  else
    log ""
    log "=========================================="
    log "  Tempest tests FAILED"
    log "=========================================="
  fi

  exit "${rc}"
}

main "$@"
