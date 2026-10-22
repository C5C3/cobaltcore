#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/dizzy.sh — Fetch the pinned dizzy load/chaos tester and drive it against
# the kind ControlPlane or, under EXTERNAL_CLUSTER=true, an external cluster
# through its two port-forwards.
#
# Subcommands:
#   stage-dashboards   Copy dizzy's three Grafana dashboards into
#                      deploy/kind/dizzy/dashboards/ for the kind overlay's
#                      configMapGenerator.
#   chaos <service>    Run the dizzy chaos soak against keystone or glance,
#                      exporting per-operation metrics over OTLP.
#   pressure <profile> Run dizzy's closed-loop Keystone token-path load with a
#                      profile of tests/load/keystone-pressure/ and keep its
#                      run record under _output/dizzy/pressure/.
#   compare <base> <other>
#                      Compare two pressure run records with
#                      `dizzy keystone report --compare`.
#
# The dizzy version is pinned once via the DIZZY_VERSION variable. Every other
# input is an optional environment override: DIZZY_SCENARIO, DIZZY_SECRET,
# DIZZY_CP_NAMESPACE, DIZZY_AUTH_URL, DIZZY_ARGS, DIZZY_PRESSURE_LABEL,
# KIND_CLUSTER, and EXTERNAL_CLUSTER.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ---------------------------------------------------------------------------
# Pinned version — bumped by Renovate via a regex custom manager that matches
# this line literally, so it must stay verbatim and appear exactly once.
# ---------------------------------------------------------------------------
DIZZY_VERSION="${DIZZY_VERSION:-v0.7.0}"

# Extracted-source cache and the generated clouds.yaml both live under the
# git-ignored _output/ tree.
DIZZY_CACHE_DIR="${REPO_ROOT}/_output/dizzy/${DIZZY_VERSION}"
CLOUDS_FILE="${REPO_ROOT}/_output/dizzy/clouds.yaml"

# The pressure profiles of the repository and the root of the run records the
# pressure subcommand keeps, one directory per label and profile.
PRESSURE_PROFILE_DIR="${REPO_ROOT}/tests/load/keystone-pressure"
PRESSURE_OUTPUT_DIR="${REPO_ROOT}/_output/dizzy/pressure"

# Selects the external-cluster mode, as in hack/deploy-infra.sh and
# hack/teardown-infra.sh; only the value true does. The helper then calls no
# docker: Keystone is reached through the Gateway port-forward on
# EXTERNAL_PUBLIC_PORT and VictoriaMetrics through a second one on 8428, the two
# the deploy's completion banner prints.
EXTERNAL_CLUSTER="${EXTERNAL_CLUSTER:-false}"

# The local port of the Gateway port-forward, PUBLIC_PORT of
# hack/deploy-infra.sh in external mode. tests/unit/hack/dizzy_sh_test.sh holds
# the two equal.
EXTERNAL_PUBLIC_PORT="8443"

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
  pressure <profile>     Run dizzy keystone pressure with the profile
                         tests/load/keystone-pressure/<profile>.yaml and keep
                         the run record in
                         _output/dizzy/pressure/<label>/<profile>/.
  compare <base> <other> Compare two pressure run records; each argument is a
                         run-*.json file or a directory whose newest
                         run-*.json is taken.

Environment overrides:
  DIZZY_VERSION          dizzy version to pin (default ${DIZZY_VERSION}).
  DIZZY_SCENARIO         Scenario file path (default the cached
                         scenarios/<service>/small.yaml for chaos, the
                         profile's file for pressure).
  DIZZY_PRESSURE_LABEL   Directory under _output/dizzy/pressure/ that a
                         pressure run's record goes to, e.g. base or patched
                         (default run).
  DIZZY_SECRET           ControlPlane admin Secret name
                         (default controlplane-keystone-admin-credentials).
  DIZZY_CP_NAMESPACE     Namespace of the admin Secret (default openstack).
  DIZZY_AUTH_URL         Keystone auth URL override.
  DIZZY_ARGS             Extra args appended to the dizzy invocation.
  KIND_CLUSTER           kind cluster name (default cobaltcore).
  EXTERNAL_CLUSTER       true: reach Keystone through the Gateway port-forward
                         on ${EXTERNAL_PUBLIC_PORT} and VictoriaMetrics through one on 8428,
                         without docker (default false).
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
# admin Secret and the Keystone auth URL: DIZZY_AUTH_URL when set, the Gateway
# port-forward under EXTERNAL_CLUSTER=true, the kind node's probed host port
# otherwise. Under EXTERNAL_CLUSTER=true the file names gateway-ca.pem beside it
# as cacert; outside it, it says verify: false.
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
  elif [[ "${EXTERNAL_CLUSTER}" == "true" ]]; then
    auth_url="https://keystone.127-0-0-1.nip.io:${EXTERNAL_PUBLIC_PORT}/v3"
    log "Keystone auth URL: ${auth_url} (EXTERNAL_CLUSTER=true, the Gateway port-forward on ${EXTERNAL_PUBLIC_PORT})."
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

  # Under EXTERNAL_CLUSTER=true the admin password goes to a name that public
  # DNS resolves and to a local port any user of the workstation can bind, so
  # dizzy verifies the Gateway against its certificates, as OS_CACERT does in
  # Step 7 of docs/quick-start-metal-stack.md. The kind mode keeps verify: false.
  local tls_line="verify: false"
  if [[ "${EXTERNAL_CLUSTER}" == "true" ]]; then
    local ca_file
    ca_file="$(dirname "${CLOUDS_FILE}")/gateway-ca.pem"
    if ! kubectl get secret -n openstack --field-selector type=kubernetes.io/tls -o json |
        jq -r '.items[] | select(.metadata.name | endswith("-nip-io-tls")) | .data["ca.crt"] | @base64d' \
          >"${ca_file}" ||
        ! grep -q 'BEGIN CERTIFICATE' "${ca_file}"; then
      log "ERROR: found no Gateway certificate in the Secrets openstack/*-nip-io-tls;"
      log "       dizzy sends the admin password only to a Keystone it can verify."
      exit 1
    fi
    tls_line="cacert: '$(printf '%s' "${ca_file}" | sed "s/'/''/g")'"
    log "Keystone TLS: dizzy verifies the Gateway against ${ca_file}."
  fi

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
    ${tls_line}
EOF
  )
  log "Wrote OpenStack credentials to ${CLOUDS_FILE} (mode 600)."
}

# ---------------------------------------------------------------------------
# probe_ingest — Warn (but never abort) when the OTLP ingest path looks down.
# The kind node's 30428 mapping is probed in kind mode only; under
# EXTERNAL_CLUSTER=true localhost:8428 is a port-forward, which the warning
# names.
# ---------------------------------------------------------------------------
probe_ingest() {
  if [[ "${EXTERNAL_CLUSTER}" != "true" ]]; then
    local metrics_port
    if ! metrics_port="$(docker port "${KIND_CLUSTER:-cobaltcore}-control-plane" 30428/tcp 2>/dev/null)" \
        || [[ -z "${metrics_port}" ]]; then
      log "WARNING: kind cluster '${KIND_CLUSTER:-cobaltcore}' has no 30428 host-port mapping;"
      log "         OTLP ingest will not reach VictoriaMetrics. Recreate the cluster with"
      log "         the metrics port: make teardown-infra && WITH_DIZZY=true make deploy-infra"
    fi
  fi

  if ! curl -fsS http://localhost:8428/health >/dev/null 2>&1; then
    log "WARNING: no VictoriaMetrics at http://localhost:8428/health; metrics will be"
    log "         exported into the void (dizzy degrades export failures to warnings)."
    if [[ "${EXTERNAL_CLUSTER}" == "true" ]]; then
      log "         Open the port-forward first: kubectl -n dizzy port-forward svc/dizzy-victoria-metrics-server 8428:8428"
    fi
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
  # gophercloud prefers OS_CACERT to the cacert of clouds.yaml; an export left
  # over from Step 7 of docs/quick-start-metal-stack.md would replace the CA
  # file generate_clouds_yaml just wrote. An empty value falls through to it.
  OS_CACERT='' \
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
# list_pressure_profiles — Log the profile names under PRESSURE_PROFILE_DIR.
# ---------------------------------------------------------------------------
list_pressure_profiles() {
  local file
  for file in "${PRESSURE_PROFILE_DIR}"/*.yaml; do
    [[ -f "${file}" ]] || continue
    log "         $(basename "${file}" .yaml)"
  done
}

# ---------------------------------------------------------------------------
# run_pressure PROFILE — Run dizzy keystone pressure against the ControlPlane
# Keystone with tests/load/keystone-pressure/PROFILE.yaml (or DIZZY_SCENARIO).
#
# dizzy writes its run record into its working directory, so the run starts
# in _output/dizzy/pressure/<DIZZY_PRESSURE_LABEL>/<PROFILE>/; two labels such
# as base and patched keep the records of two builds apart for compare. The
# run exports over OTLP only when the dizzy stack is installed: pressure needs
# no VictoriaMetrics, and its token-path numbers come from the run record.
# ---------------------------------------------------------------------------
run_pressure() {
  local profile="${1:-}"
  if [[ -z "${profile}" ]]; then
    usage
    exit 1
  fi
  if [[ ! "${profile}" =~ ^[a-z0-9][a-z0-9-]*$ ]]; then
    log "ERROR: pressure profile '${profile}' must be a name such as warm-cache."
    exit 1
  fi
  local label="${DIZZY_PRESSURE_LABEL:-run}"
  if [[ ! "${label}" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
    log "ERROR: DIZZY_PRESSURE_LABEL '${label}' must start with a letter or digit and hold only letters, digits, '.', '_' and '-'."
    exit 1
  fi

  # Checked here, unlike the chaos scenario: a profile that names no file is
  # a typo the list of profiles answers better than dizzy's error.
  local scenario="${DIZZY_SCENARIO:-${PRESSURE_PROFILE_DIR}/${profile}.yaml}"
  if [[ ! -f "${scenario}" ]]; then
    log "ERROR: no pressure scenario at ${scenario}; the profiles are:"
    list_pressure_profiles
    exit 1
  fi
  # dizzy starts in the run directory, so a relative path is made absolute.
  scenario="$(cd "$(dirname "${scenario}")" && pwd)/$(basename "${scenario}")"

  ensure_binary
  generate_clouds_yaml

  local otel_args=()
  if kubectl get namespace dizzy >/dev/null 2>&1; then
    probe_ingest
    otel_args=(--otel)
  else
    log "No dizzy stack (namespace dizzy); the run exports no metrics."
  fi

  local extra_args=()
  read -r -a extra_args <<< "${DIZZY_ARGS:-}"

  local run_dir="${PRESSURE_OUTPUT_DIR}/${label}/${profile}"
  mkdir -p "${run_dir}"

  log "Starting dizzy keystone pressure (profile: ${profile}, scenario: ${scenario}, run record in ${run_dir})..."
  (
    cd "${run_dir}"
    OS_CACERT='' \
    OS_CLIENT_CONFIG_FILE="${CLOUDS_FILE}" \
    OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="http://localhost:8428/opentelemetry/v1/metrics" \
    OTEL_METRIC_EXPORT_INTERVAL=15000 \
      "${REPO_ROOT}/bin/dizzy" keystone pressure \
        --os-cloud devstack-c5c3 \
        --scenario "${scenario}" \
        ${otel_args[@]+"${otel_args[@]}"} \
        ${extra_args[@]+"${extra_args[@]}"}
  )
}

# ---------------------------------------------------------------------------
# resolve_record PATH — Print PATH when it is a file, or the newest run-*.json
# in the directory PATH. Exits 1 with an error on stderr otherwise.
# ---------------------------------------------------------------------------
resolve_record() {
  local path="$1"
  if [[ -f "${path}" ]]; then
    printf '%s\n' "${path}"
    return 0
  fi
  if [[ ! -d "${path}" ]]; then
    log "ERROR: ${path} is neither a run record nor a directory." >&2
    exit 1
  fi
  local file newest=""
  for file in "${path}"/run-*.json; do
    [[ -f "${file}" ]] || continue
    if [[ -z "${newest}" || "${file}" -nt "${newest}" ]]; then
      newest="${file}"
    fi
  done
  if [[ -z "${newest}" ]]; then
    log "ERROR: no run-*.json in ${path}." >&2
    exit 1
  fi
  printf '%s\n' "${newest}"
}

# ---------------------------------------------------------------------------
# run_compare BASE OTHER — Print the comparison of two keystone pressure run
# records, BASE the build without the change, and keep it as text and HTML in
# _output/dizzy/pressure/compare/. DIZZY_ARGS reaches dizzy, e.g.
# --allow-mismatch.
# ---------------------------------------------------------------------------
run_compare() {
  if [[ $# -ne 2 ]]; then
    usage
    exit 1
  fi
  local base other
  base="$(resolve_record "$1")"
  other="$(resolve_record "$2")"

  ensure_binary

  local extra_args=()
  read -r -a extra_args <<< "${DIZZY_ARGS:-}"

  local out_dir="${PRESSURE_OUTPUT_DIR}/compare"
  local stem
  stem="$(basename "${base}" .json)-vs-$(basename "${other}" .json)"
  mkdir -p "${out_dir}"

  log "Comparing ${base} (base) with ${other}..."
  "${REPO_ROOT}/bin/dizzy" keystone report --run "${base}" --compare "${other}" \
    ${extra_args[@]+"${extra_args[@]}"} | tee "${out_dir}/${stem}.txt"
  "${REPO_ROOT}/bin/dizzy" keystone report --run "${base}" --compare "${other}" --format html \
    ${extra_args[@]+"${extra_args[@]}"} > "${out_dir}/${stem}.html"
  log "Comparison written to ${out_dir}/${stem}.txt and ${stem}.html."
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
    pressure)
      shift
      run_pressure "$@"
      ;;
    compare)
      shift
      run_compare "$@"
      ;;
    *)
      usage
      exit 1
      ;;
  esac
}

# Run main only when executed directly so unit tests (tests/unit/hack/) can
# source this script and exercise individual functions.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
