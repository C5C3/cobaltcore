#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/dizzy.sh — Fetch the pinned dizzy load/chaos tester and drive it against
# the kind ControlPlane.
#
# Subcommands:
#   stage-dashboards   Copy dizzy's three Grafana dashboards into
#                      deploy/kind/dizzy/dashboards/ for the kind overlay's
#                      configMapGenerator.
#   chaos <service>    Run the dizzy chaos soak against keystone or glance,
#                      exporting per-operation metrics over OTLP.
#
# The dizzy version is pinned once via the DIZZY_VERSION variable. Every other
# input is an optional environment override: DIZZY_SCENARIO, DIZZY_SECRET,
# DIZZY_CP_NAMESPACE, DIZZY_AUTH_URL, DIZZY_ARGS, and KIND_CLUSTER.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ---------------------------------------------------------------------------
# Pinned version — bumped by Renovate via a regex custom manager that matches
# this line literally, so it must stay verbatim and appear exactly once.
# ---------------------------------------------------------------------------
DIZZY_VERSION="${DIZZY_VERSION:-v0.1.0}"

# Extracted-source cache and the generated clouds.yaml both live under the
# git-ignored _output/ tree.
DIZZY_CACHE_DIR="${REPO_ROOT}/_output/dizzy/${DIZZY_VERSION}"
CLOUDS_FILE="${REPO_ROOT}/_output/dizzy/clouds.yaml"

# ---------------------------------------------------------------------------
# log — Print a timestamped log message (ISO 8601 UTC).
# Matches the pattern from hack/deploy-infra.sh.
# ---------------------------------------------------------------------------
log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

# ---------------------------------------------------------------------------
# usage — Print the subcommand/env-override reference to stderr.
# ---------------------------------------------------------------------------
usage() {
  cat >&2 <<EOF
Usage: hack/dizzy.sh <subcommand>

Subcommands:
  stage-dashboards       Copy dizzy's three Grafana dashboards into
                         deploy/kind/dizzy/dashboards/.
  chaos <keystone|glance>
                         Run the dizzy chaos soak against the named service
                         using the ControlPlane admin credentials.

Environment overrides:
  DIZZY_VERSION          dizzy version to pin (default ${DIZZY_VERSION}).
  DIZZY_SCENARIO         Scenario file path (default the cached
                         scenarios/<service>/small.yaml).
  DIZZY_SECRET           ControlPlane admin Secret name
                         (default controlplane-keystone-admin-credentials).
  DIZZY_CP_NAMESPACE     Namespace of the admin Secret (default openstack).
  DIZZY_AUTH_URL         Keystone auth URL override.
  DIZZY_ARGS             Extra args appended to the dizzy invocation.
  KIND_CLUSTER           kind cluster name (default cobaltcore).
EOF
}

# ---------------------------------------------------------------------------
# ensure_cache — Populate ${DIZZY_CACHE_DIR} with dizzy's extracted sources.
#
# A cache hit (the directory already exists) skips the download entirely.
# Otherwise the release tarball is fetched into a staging dir on the SAME
# filesystem as the cache, extracted, and moved into place with an atomic
# rename — so an aborted download/extract never leaves a half-populated cache.
# ---------------------------------------------------------------------------
ensure_cache() {
  if [[ -d "${DIZZY_CACHE_DIR}" ]]; then
    log "dizzy ${DIZZY_VERSION} sources already cached at ${DIZZY_CACHE_DIR}."
    return 0
  fi

  local url="https://github.com/B42Labs/dizzy/archive/refs/tags/${DIZZY_VERSION}.tar.gz"
  log "Fetching dizzy ${DIZZY_VERSION} sources from ${url}..."

  mkdir -p "${REPO_ROOT}/_output/dizzy"
  local staging
  staging="$(mktemp -d "${REPO_ROOT}/_output/dizzy/.staging.XXXXXX")"
  trap 'rm -rf "${staging:-}"' RETURN

  local payload="${staging}/payload"
  mkdir -p "${payload}"

  if ! curl -fsSL "${url}" -o "${staging}/dizzy.tar.gz"; then
    log "ERROR: failed to download dizzy sources from ${url}"
    rm -rf "${staging}"
    exit 1
  fi

  # --strip-components=1 drops the archive's dizzy-<version>/ top level so the
  # payload (contrib/, scenarios/, ...) is not nested one directory deeper.
  if ! tar -xzf "${staging}/dizzy.tar.gz" -C "${payload}" --strip-components=1; then
    log "ERROR: failed to extract dizzy sources from ${url}"
    rm -rf "${staging}"
    exit 1
  fi

  mv "${payload}" "${DIZZY_CACHE_DIR}"
  log "dizzy ${DIZZY_VERSION} sources cached at ${DIZZY_CACHE_DIR}."
}

# ---------------------------------------------------------------------------
# ensure_binary — Install github.com/B42Labs/dizzy at the pinned version into
# ${REPO_ROOT}/bin unless an already-installed binary reports that version.
# ---------------------------------------------------------------------------
ensure_binary() {
  local bin="${REPO_ROOT}/bin/dizzy"

  # `go version -m` fails (non-zero) when the binary is absent; guarding it in
  # the if-condition keeps set -e from aborting, and the buildinfo of a wrong
  # version simply misses the grep, taking the install path.
  if go version -m "${bin}" 2>/dev/null | grep -qF "${DIZZY_VERSION}"; then
    log "dizzy ${DIZZY_VERSION} already installed at ${bin}."
    return 0
  fi

  log "Installing dizzy ${DIZZY_VERSION} into ${REPO_ROOT}/bin..."
  GOBIN="${REPO_ROOT}/bin" go install "github.com/B42Labs/dizzy/cmd/dizzy@${DIZZY_VERSION}"
}

# ---------------------------------------------------------------------------
# stage_dashboards — Copy the three dizzy dashboards into the kind overlay.
# ---------------------------------------------------------------------------
stage_dashboards() {
  ensure_cache

  local dest="${REPO_ROOT}/deploy/kind/dizzy/dashboards"
  mkdir -p "${dest}"

  local dashboard
  for dashboard in overview.json api-operations.json time-to-ready.json; do
    cp -f "${DIZZY_CACHE_DIR}/contrib/otel/dashboards/${dashboard}" "${dest}/${dashboard}"
  done
  log "Staged dizzy dashboards into ${dest}."
}

# ---------------------------------------------------------------------------
# generate_clouds_yaml — Write ${CLOUDS_FILE} (mode 600) from the ControlPlane
# admin Secret and the probed Keystone host port.
# ---------------------------------------------------------------------------
generate_clouds_yaml() {
  local secret="${DIZZY_SECRET:-controlplane-keystone-admin-credentials}"
  local namespace="${DIZZY_CP_NAMESPACE:-openstack}"

  local encoded
  if ! encoded="$(kubectl get secret "${secret}" -n "${namespace}" \
      -o jsonpath='{.data.password}' 2>/dev/null)"; then
    log "ERROR: could not read admin Secret '${secret}' in namespace '${namespace}'."
    log "       Wait for the ControlPlane to be Ready first, e.g.:"
    log "         kubectl wait controlplane/controlplane -n openstack --for=condition=Ready"
    exit 1
  fi

  local password
  password="$(printf '%s' "${encoded}" | base64 -d 2>/dev/null)"
  if [[ -z "${password}" ]]; then
    log "ERROR: admin Secret '${secret}' in namespace '${namespace}' has an empty password key."
    exit 1
  fi

  # Escape single quotes for the YAML single-quoted scalar below.
  local escaped
  escaped="$(printf '%s' "${password}" | sed "s/'/''/g")"

  local auth_url
  if [[ -n "${DIZZY_AUTH_URL:-}" ]]; then
    auth_url="${DIZZY_AUTH_URL}"
  else
    local hostport="443"
    local port_out
    # `docker port` prints one line per binding (e.g. 127.0.0.1:443); take the
    # first line's host port (the part after the last colon). Guarded so an
    # unavailable docker / non-kind cluster falls back to :443 instead of
    # aborting under set -e/pipefail.
    if port_out="$(docker port "${KIND_CLUSTER:-cobaltcore}-control-plane" 31443/tcp 2>/dev/null)" \
        && [[ -n "${port_out}" ]]; then
      local first_line="${port_out%%$'\n'*}"
      hostport="${first_line##*:}"
    else
      log "docker port probe for the Keystone host port failed; falling back to :443."
    fi
    auth_url="https://keystone.127-0-0-1.nip.io:${hostport}/v3"
  fi

  mkdir -p "$(dirname "${CLOUDS_FILE}")"
  (
    umask 077
    cat > "${CLOUDS_FILE}" <<EOF
clouds:
  devstack-c5c3:
    auth:
      auth_url: ${auth_url}
      username: admin
      password: '${escaped}'
      project_name: admin
      user_domain_name: Default
      project_domain_name: Default
    identity_api_version: 3
    verify: false
EOF
  )
  log "Wrote OpenStack credentials to ${CLOUDS_FILE} (mode 600)."
}

# ---------------------------------------------------------------------------
# probe_ingest — Warn (but never abort) when the OTLP ingest path looks down.
# ---------------------------------------------------------------------------
probe_ingest() {
  local metrics_port
  if ! metrics_port="$(docker port "${KIND_CLUSTER:-cobaltcore}-control-plane" 30428/tcp 2>/dev/null)" \
      || [[ -z "${metrics_port}" ]]; then
    log "WARNING: kind cluster '${KIND_CLUSTER:-cobaltcore}' has no 30428 host-port mapping;"
    log "         OTLP ingest will not reach VictoriaMetrics. Recreate the cluster with"
    log "         the metrics port: make teardown-infra && WITH_DIZZY=true make deploy-infra"
  fi

  if ! curl -fsS http://localhost:8428/health >/dev/null 2>&1; then
    log "WARNING: no VictoriaMetrics at http://localhost:8428/health; metrics will be"
    log "         exported into the void (dizzy degrades export failures to warnings)."
  fi
}

# ---------------------------------------------------------------------------
# run_chaos SERVICE — Run the dizzy chaos soak against keystone or glance.
# ---------------------------------------------------------------------------
run_chaos() {
  local service="${1:-}"
  case "${service}" in
    keystone | glance) ;;
    *)
      usage
      exit 1
      ;;
  esac

  ensure_binary
  ensure_cache

  # Not pre-validated: a missing scenario must reach dizzy so its own
  # scenario-not-found error surfaces.
  local scenario="${DIZZY_SCENARIO:-${DIZZY_CACHE_DIR}/scenarios/${service}/small.yaml}"

  generate_clouds_yaml
  probe_ingest

  # Split DIZZY_ARGS on whitespace into an array so an empty value adds zero
  # args (not one empty-string arg). The ${a[@]+"${a[@]}"} idiom at the
  # invocation keeps stock-macOS bash 3.2 happy: expanding an empty array
  # under set -u is an "unbound variable" there.
  local extra_args=()
  read -r -a extra_args <<< "${DIZZY_ARGS:-}"

  log "Starting dizzy ${service} chaos soak (scenario: ${scenario})..."
  OS_CLIENT_CONFIG_FILE="${CLOUDS_FILE}" \
  OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="http://localhost:8428/opentelemetry/v1/metrics" \
  OTEL_METRIC_EXPORT_INTERVAL=15000 \
    "${REPO_ROOT}/bin/dizzy" "${service}" chaos \
      --os-cloud devstack-c5c3 \
      --scenario "${scenario}" \
      --otel \
      ${extra_args[@]+"${extra_args[@]}"}
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------
main() {
  local subcommand="${1:-}"
  case "${subcommand}" in
    stage-dashboards)
      stage_dashboards
      ;;
    chaos)
      shift
      run_chaos "$@"
      ;;
    *)
      usage
      exit 1
      ;;
  esac
}

main "$@"
