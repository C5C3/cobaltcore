#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/run-tempest.sh — Run Tempest API tests against a service in the kind cluster.
# Feature: CC-0035
#
# Orchestrates Tempest execution for a given OpenStack service:
#   1. Validate prerequisites (docker, kubectl, yq, service config directory)
#   2. Build the Tempest container image using the service Dockerfile
#   3. Extract the admin password from the Kubernetes secret
#   4. Run Tempest inside the container with the service-specific configuration
#      in two sequential stestr phases (core tempest.api.* vs
#      keystone_tempest_plugin.*) to isolate suites that share Keystone's
#      dynamic service_providers state — see docs/reference/tempest-test-
#      infrastructure.md
#   5. Convert subunit results to JUnit XML for CI artifact upload
#
# REQ-006: Local Tempest execution orchestration script.
# REQ-011: set -euo pipefail, SPDX Apache-2.0 header, CC-0035 reference.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
SERVICE="${SERVICE:-}"
SERVICE_NAME="${SERVICE_NAME:-}"       # K8s service name; default: <SERVICE>-api
RELEASE="${RELEASE:-2025.2}"
TEMPEST_IMAGE="${TEMPEST_IMAGE:-c5c3/tempest:local}"
BUILD_IMAGE="${BUILD_IMAGE:-true}"     # Set to false to skip image build
OUTPUT_DIR="${OUTPUT_DIR:-${REPO_ROOT}/_output/tempest}"
TEMPEST_TIMEOUT="${TEMPEST_TIMEOUT:-1800}"
NAMESPACE="${NAMESPACE:-openstack}"
ADMIN_SECRET="${ADMIN_SECRET:-keystone-admin}"  # K8s secret holding admin password
TEMPEST_CONCURRENCY="${TEMPEST_CONCURRENCY:-4}" # stestr worker count; cap at replicas × uwsgi.processes

# ---------------------------------------------------------------------------
# log — Print a timestamped log message (ISO 8601 UTC).
# ---------------------------------------------------------------------------
log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

# ---------------------------------------------------------------------------
# usage — Print usage information and exit.
# ---------------------------------------------------------------------------
usage() {
  cat <<EOF
Usage: SERVICE=<service> hack/run-tempest.sh

Run Tempest API tests against a deployed OpenStack service in the kind cluster.

Required:
  SERVICE              OpenStack service to test (e.g., keystone)

Optional:
  SERVICE_NAME         Kubernetes service name (default: <SERVICE>-api)
  RELEASE              Release version (default: 2025.2)
  TEMPEST_IMAGE        Docker image tag (default: c5c3/tempest:local)
  BUILD_IMAGE          Build the image before running (default: true)
  OUTPUT_DIR           Directory for test results (default: _output/tempest)
  TEMPEST_TIMEOUT      Timeout for Tempest run in seconds (default: 1800)
  NAMESPACE            Kubernetes namespace for the service (default: openstack)
  ADMIN_SECRET         Kubernetes secret name holding admin password (default: keystone-admin)
  TEMPEST_CONCURRENCY  stestr worker count (default: 4); must not exceed replicas × uwsgi.processes

Examples:
  SERVICE=keystone hack/run-tempest.sh
  BUILD_IMAGE=false SERVICE=keystone hack/run-tempest.sh
EOF
  exit 1
}

# ---------------------------------------------------------------------------
# preflight_checks — Verify prerequisites before running Tempest.
# ---------------------------------------------------------------------------
preflight_checks() {
  log "Running pre-flight checks..."

  if [[ -z "${SERVICE}" ]]; then
    log "ERROR: SERVICE is required. Set SERVICE=<service> (e.g., SERVICE=keystone)."
    usage
  fi

  for cmd in docker kubectl yq; do
    if ! command -v "${cmd}" &>/dev/null; then
      log "ERROR: '${cmd}' is not installed or not in PATH."
      exit 1
    fi
  done

  if ! docker info &>/dev/null; then
    log "ERROR: Docker is not running. Please start Docker and try again."
    exit 1
  fi

  # Verify service-specific Tempest configuration exists.
  local release_slug="${RELEASE//./-}"
  local config_dir="${REPO_ROOT}/tests/tempest/${SERVICE}-${release_slug}"
  if [[ ! -d "${config_dir}" ]]; then
    log "ERROR: Tempest configuration directory not found: tests/tempest/${SERVICE}-${release_slug}/"
    log "Available services:"
    # shellcheck disable=SC2012
    ls -1 "${REPO_ROOT}/tests/tempest/" 2>/dev/null | sed 's/^/  /' || log "  (none)"
    exit 1
  fi

  if [[ ! -f "${config_dir}/tempest.conf" ]]; then
    log "ERROR: tempest.conf not found in tests/tempest/${SERVICE}-${release_slug}/"
    exit 1
  fi

  # Verify test-refs.yaml exists for the release.
  if [[ ! -f "${REPO_ROOT}/releases/${RELEASE}/test-refs.yaml" ]]; then
    log "ERROR: releases/${RELEASE}/test-refs.yaml not found."
    exit 1
  fi

  log "Pre-flight checks passed."
}

# ---------------------------------------------------------------------------
# build_tempest_image — Build the Tempest container image.
# ---------------------------------------------------------------------------
build_tempest_image() {
  log "Building Tempest image '${TEMPEST_IMAGE}'..."

  local test_refs="${REPO_ROOT}/releases/${RELEASE}/test-refs.yaml"
  local plugin_key="${SERVICE}-tempest-plugin"
  local tempest_version
  local plugin_version

  tempest_version=$("${SCRIPT_DIR}/resolve-test-ref.sh" "${test_refs}" "tempest")
  plugin_version=$("${SCRIPT_DIR}/resolve-test-ref.sh" "${test_refs}" "${plugin_key}")

  log "  Tempest version: ${tempest_version}"
  log "  Plugin version (${plugin_key}): ${plugin_version}"

  # The build-arg name is derived from the plugin key (e.g. keystone-tempest-plugin
  # becomes KEYSTONE_TEMPEST_PLUGIN_VERSION).
  local build_arg_name
  build_arg_name=$(echo "${plugin_key}" | tr '[:lower:]-' '[:upper:]_')_VERSION

  docker build \
    -t "${TEMPEST_IMAGE}" \
    -f "${REPO_ROOT}/images/tempest/Dockerfile" \
    --build-arg "TEMPEST_VERSION=${tempest_version}" \
    --build-arg "${build_arg_name}=${plugin_version}" \
    --build-context "python-base=docker-image://ghcr.io/c5c3/python-base:latest" \
    --build-context "venv-builder=docker-image://ghcr.io/c5c3/venv-builder:latest" \
    --build-context "upper-constraints=${REPO_ROOT}/releases/${RELEASE}" \
    "${REPO_ROOT}/images/tempest"

  log "Tempest image built successfully."
}

# ---------------------------------------------------------------------------
# extract_admin_password — Get the admin password from the Kubernetes secret.
# ---------------------------------------------------------------------------
extract_admin_password() {
  local password
  password=$(kubectl get secret "${ADMIN_SECRET}" -n "${NAMESPACE}" \
    -o jsonpath='{.data.password}' 2>/dev/null | base64 -d) || true

  if [[ -z "${password}" ]]; then
    log "ERROR: Could not extract admin password from secret '${NAMESPACE}/${ADMIN_SECRET}'."
    log "Ensure the kind cluster is running and ExternalSecrets have synced."
    exit 1
  fi

  echo "${password}"
}

# ---------------------------------------------------------------------------
# run_tempest — Execute Tempest tests inside the container.
# ---------------------------------------------------------------------------
run_tempest() {
  local admin_password="$1"
  local release_slug="${RELEASE//./-}"
  local config_dir="${REPO_ROOT}/tests/tempest/${SERVICE}-${release_slug}"
  local svc_name="${SERVICE_NAME:-${SERVICE}-api}"

  # Ensure output directory is writable by the container user (UID 42424 / openstack).
  mkdir -p "${OUTPUT_DIR}"
  chmod o+w "${OUTPUT_DIR}"

  # Start a port-forward unless the service is already reachable on localhost:5000
  # (e.g. the user left one running from the quick-start guide).
  local pf_pid=""
  if ! curl -sf http://localhost:5000/ >/dev/null 2>&1; then
    log "Port-forwarding svc/${svc_name} (${NAMESPACE}) → localhost:5000 ..."
    kubectl port-forward "svc/${svc_name}" -n "${NAMESPACE}" 5000:5000 >/dev/null 2>&1 &
    pf_pid=$!
    local ready=false
    for _ in $(seq 1 10); do
      if curl -sf http://localhost:5000/ >/dev/null 2>&1; then
        ready=true
        break
      fi
      sleep 1
    done
    if [[ "${ready}" != "true" ]]; then
      log "ERROR: Service at http://localhost:5000 did not become reachable after 10 attempts."
      exit 1
    fi
  else
    log "Using existing port-forward on localhost:5000."
  fi
  # Clean up port-forward on exit only if we started it.
  [[ -n "${pf_pid}" ]] && trap 'kill '"${pf_pid}"' 2>/dev/null || true' EXIT

  # On macOS, Docker Desktop containers cannot reach the host via localhost with
  # --network host. Use host.docker.internal (the host's loopback as seen from
  # inside a container) and the default bridge network instead.
  #
  # catalog_ip is used to map the Kubernetes cluster-internal service hostnames
  # (as returned in the Keystone service catalog) back to the port-forward running
  # on the host. On macOS host-gateway routes through Docker Desktop to localhost;
  # on Linux (--network host) the container shares the host network so 127.0.0.1
  # is already the correct loopback address.
  local container_host="localhost"
  local docker_network=("--network" "host")
  local catalog_ip="127.0.0.1"
  if [[ "$(uname)" == "Darwin" ]]; then
    container_host="host.docker.internal"
    docker_network=("--add-host" "host.docker.internal:host-gateway")
    catalog_ip="host-gateway"
  fi

  # Stage config files into a scratch directory that is mounted as /etc/tempest.
  # Mounting a directory (rather than individual files) avoids Docker Desktop on
  # macOS creating file-mount targets as directories when the parent path does not
  # exist in the image.
  local etc_dir="${OUTPUT_DIR}/config"
  mkdir -p "${etc_dir}"

  sed \
    -e "s|http://[^/]*:5000|http://${container_host}:5000|" \
    -e "s|\${KEYSTONE_ADMIN_PASSWORD}|${admin_password}|" \
    "${config_dir}/tempest.conf" > "${etc_dir}/tempest.conf"
  [[ -f "${config_dir}/exclude-tests.txt" ]] && cp "${config_dir}/exclude-tests.txt" "${etc_dir}/"

  # Scope-split the include list into two phase files so core tempest.api.*
  # and keystone_tempest_plugin.* run in separate sequential stestr passes.
  # Keystone injects the current list of federation service providers into
  # every token issue/validate response, so ServiceProvidersTest cleanups
  # race against tempest.api.identity.v3.test_tokens.TokensV3Test.
  # test_validate_token when both suites share workers. Upstream's
  # keystone-tempest gate only runs keystone_tempest_plugin.* and therefore
  # never sees this race; we run both suites and isolate them by time.
  if [[ ! -f "${config_dir}/include-tests.txt" ]]; then
    log "ERROR: ${config_dir}/include-tests.txt is required for scope-split execution."
    return 1
  fi
  local phases_dir="${etc_dir}/phases"
  mkdir -p "${phases_dir}"
  grep -E '^tempest\.' "${config_dir}/include-tests.txt" \
    > "${phases_dir}/phase-1-core.txt" || true
  grep -E '^keystone_tempest_plugin\.' "${config_dir}/include-tests.txt" \
    > "${phases_dir}/phase-2-plugin.txt" || true

  local total_patterns phase1_count phase2_count
  total_patterns=$(grep -cE '^[^#[:space:]]' \
    "${config_dir}/include-tests.txt" || true)
  phase1_count=$(wc -l < "${phases_dir}/phase-1-core.txt" | tr -d ' ')
  phase2_count=$(wc -l < "${phases_dir}/phase-2-plugin.txt" | tr -d ' ')
  if [[ $((phase1_count + phase2_count)) -ne "${total_patterns}" ]]; then
    log "ERROR: scope-split does not cover include-tests.txt: ${total_patterns} patterns, $((phase1_count + phase2_count)) covered (phase1=${phase1_count}, phase2=${phase2_count})."
    log "ERROR: every non-comment line must start with 'tempest.' or 'keystone_tempest_plugin.'."
    return 1
  fi
  if [[ "${phase1_count}" -eq 0 || "${phase2_count}" -eq 0 ]]; then
    log "ERROR: scope-split produced an empty phase (phase1=${phase1_count}, phase2=${phase2_count}); both phases must have at least one pattern."
    return 1
  fi

  log "Running Tempest tests for service '${SERVICE}'..."
  log "  Endpoint: http://${container_host}:5000"
  log "  Output:   ${OUTPUT_DIR}"
  log "  Timeout:  ${TEMPEST_TIMEOUT}s"
  log "  Phases:   phase-1-core (${phase1_count}), phase-2-plugin (${phase2_count})"

  local rc=0
  timeout "${TEMPEST_TIMEOUT}" \
    docker run --rm \
      --name "tempest-${SERVICE}" \
      "${docker_network[@]}" \
      --add-host "${svc_name}.${NAMESPACE}.svc.cluster.local:${catalog_ip}" \
      --add-host "${svc_name}.${NAMESPACE}.svc:${catalog_ip}" \
      -v "${etc_dir}:/etc/tempest:ro" \
      -v "${OUTPUT_DIR}:/output" \
      "${TEMPEST_IMAGE}" \
      bash -c "
        set -euo pipefail
        # HOME=/tmp: the openstack user's real home (/var/lib/openstack) is owned
        # by root, so tempest cannot create ~/.tempest there. Redirect to /tmp.
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
          echo \"--- Tempest phase: \${phase} ---\"
          set +e
          # shellcheck disable=SC2086  # intentional word-splitting on exclude_args
          stestr run --include-list /etc/tempest/phases/\${phase}.txt \${exclude_args} --concurrency ${TEMPEST_CONCURRENCY} --subunit | tee /output/\${phase}.subunit | subunit2pyunit 2>&1 | grep --line-buffered -E '\.\.\. '
          local phase_rc=\${PIPESTATUS[0]}
          set -e
          return \${phase_rc}
        }

        overall_rc=0
        run_phase phase-1-core || overall_rc=\$?
        phase2_rc=0
        run_phase phase-2-plugin || phase2_rc=\$?
        if [[ \${phase2_rc} -gt \${overall_rc} ]]; then
          overall_rc=\${phase2_rc}
        fi

        # Subunit v2 is stream-concatenation safe, so cat'ing the two phase
        # streams produces a single valid subunit stream.
        cat /output/phase-1-core.subunit /output/phase-2-plugin.subunit > /output/tempest.subunit
        subunit2junitxml < /output/tempest.subunit > /output/tempest-results.xml 2>/dev/null || true

        # Guard against stestr run --subunit exiting 0 on test failures:
        # check the JUnit XML for reported failures or errors.
        if [[ \${overall_rc} -eq 0 && -f /output/tempest-results.xml ]]; then
          if grep -qE 'failures=\"[1-9]|errors=\"[1-9]' /output/tempest-results.xml; then
            echo 'ERROR: Tempest reported test failures.'
            overall_rc=1
          fi
        fi
        exit \${overall_rc}
      " || rc=$?

  if [[ "${rc}" -eq 124 ]]; then
    log "ERROR: Tempest timed out after ${TEMPEST_TIMEOUT}s. Stopping container..."
    docker stop "tempest-${SERVICE}" 2>/dev/null || true
  fi

  return "${rc}"
}

# ---------------------------------------------------------------------------
# main — Orchestrate Tempest test execution.
# ---------------------------------------------------------------------------
main() {
  log "=========================================="
  log "  Tempest API Tests — ${SERVICE:-<unset>}"
  log "=========================================="

  preflight_checks

  log "Service   : ${SERVICE}"
  log "Release   : ${RELEASE}"
  log "Image     : ${TEMPEST_IMAGE}"
  log "Output    : ${OUTPUT_DIR}"
  log "Namespace : ${NAMESPACE}"
  log ""

  # Step 1: Build Tempest image
  if [[ "${BUILD_IMAGE}" == "true" ]]; then
    log "=== Step 1/3: Build Tempest image ==="
    build_tempest_image
  else
    log "=== Step 1/3: Skipping image build (BUILD_IMAGE=false) ==="
  fi

  # Step 2: Extract admin password
  log "=== Step 2/3: Extract admin password ==="
  local admin_password
  admin_password=$(extract_admin_password)
  log "Admin password extracted from ${NAMESPACE}/${ADMIN_SECRET}."

  # Step 3: Run Tempest
  log "=== Step 3/3: Run Tempest tests ==="
  local rc=0
  run_tempest "${admin_password}" || rc=$?

  log ""
  if [[ "${rc}" -eq 0 ]]; then
    log "=========================================="
    log "  Tempest tests PASSED"
    log "=========================================="
  else
    log "=========================================="
    log "  Tempest tests FAILED (exit code: ${rc})"
    log "=========================================="
  fi

  if [[ -f "${OUTPUT_DIR}/tempest-results.xml" ]]; then
    log "JUnit results: ${OUTPUT_DIR}/tempest-results.xml"
  fi
  if [[ -f "${OUTPUT_DIR}/tempest.subunit" ]]; then
    log "Subunit stream: ${OUTPUT_DIR}/tempest.subunit"
  fi

  exit "${rc}"
}

main "$@"
