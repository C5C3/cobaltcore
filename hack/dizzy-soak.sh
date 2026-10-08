#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/dizzy-soak.sh — Drive the long-running dizzy soak (#1274) from the
# workstation: a Job in the namespace dizzy runs `dizzy mix chaos` inside the
# cluster of the current kubeconfig context, in the OpenStack project
# dizzy-soak, and writes a report with a PASS or FAIL verdict to the claim
# dizzy-soak-reports. The cluster needs the dizzy stack (WITH_DIZZY=true), a
# Ready ControlPlane and, for the default scenario, the lab's hypervisor
# fixtures.
#
# Subcommands, wrapped by the four make targets dizzy-soak-<subcommand>:
#   start          Apply <overlay>/dizzy-soak, write the soak's Secrets and
#                  ConfigMaps and create the Job.
#   status         Print the soak's state, pod, start time and last log lines.
#   stop           Stop a running soak, wait for its report and fetch it.
#   report [run]   Copy a run's report directory (default: the newest) to
#                  _output/dizzy/soak/<run>/.
#
# The dizzy version comes from hack/dizzy.sh, the one pin of the repository,
# which this script sources for it and for log. The script opens no
# port-forward and works on the cluster of the current kubeconfig context.

set -euo pipefail

SOAK_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=hack/dizzy.sh
source "${SOAK_SCRIPT_DIR}/dizzy.sh"

# The overlay root, resolved as hack/teardown-infra.sh resolves it: relative
# to REPO_ROOT unless absolute.
EXTERNAL_OVERLAY="${EXTERNAL_OVERLAY:-deploy/lab/metal-stack}"
case "${EXTERNAL_OVERLAY}" in
  /*) OVERLAY_ROOT="${EXTERNAL_OVERLAY}" ;;
  *) OVERLAY_ROOT="${REPO_ROOT}/${EXTERNAL_OVERLAY}" ;;
esac
SOAK_DIR="${OVERLAY_ROOT}/dizzy-soak"

# Settings. A DIZZY_SOAK_DURATION that is set but empty is refused, so its
# default applies only when it is unset.
DIZZY_SOAK_SERVICE="${DIZZY_SOAK_SERVICE:-mix}"
DIZZY_SOAK_DURATION="${DIZZY_SOAK_DURATION-6h}"
DIZZY_SCENARIO="${DIZZY_SCENARIO:-}"
DIZZY_ARGS="${DIZZY_ARGS:-}"
DIZZY_AUTH_URL="${DIZZY_AUTH_URL:-}"
DIZZY_CP_NAME="${DIZZY_CP_NAME:-controlplane}"
DIZZY_CP_NAMESPACE="${DIZZY_CP_NAMESPACE:-openstack}"
DIZZY_SOAK_NAMESPACES="${DIZZY_SOAK_NAMESPACES:-}"
DIZZY_SOAK_SAMPLE_INTERVAL="${DIZZY_SOAK_SAMPLE_INTERVAL:-60}"
DIZZY_SOAK_MAX_ERROR_RATE="${DIZZY_SOAK_MAX_ERROR_RATE:-}"
DIZZY_SOAK_MAX_P95_SECONDS="${DIZZY_SOAK_MAX_P95_SECONDS:-}"

# The objects of the soak, the local report directory and the waits. Plain
# variables, so the unit test can point them elsewhere after sourcing.
SOAK_NAMESPACE="dizzy"
SOAK_JOB="dizzy-soak"
SOAK_POD_SELECTOR="batch.kubernetes.io/job-name=dizzy-soak"
SOAK_CLAIM="dizzy-soak-reports"
SOAK_READER_POD="dizzy-soak-reader"
SOAK_PASSWORD_SECRET="dizzy-soak-user-password"
SOAK_OUTPUT_DIR="${REPO_ROOT}/_output/dizzy/soak"
SOAK_IDENTITY_TIMEOUT=300
SOAK_POD_TIMEOUT=300
SOAK_STOP_TIMEOUT=600
SOAK_POLL_INTERVAL=5

# The scenario file start ships, set by soak_preflight, and a temporary
# clouds.yaml, removed on exit.
SOAK_SCENARIO_FILE=""
SOAK_CLOUDS_TMP=""

# ---------------------------------------------------------------------------
# soak_usage — Print the subcommands and settings to stderr.
# ---------------------------------------------------------------------------
soak_usage() {
  cat >&2 <<EOF
Usage: hack/dizzy-soak.sh <subcommand>

Subcommands:
  start                  Start the soak in the cluster of the current context.
  status                 Print the soak's state; exits 1 when there is none.
  stop                   Stop a running soak, wait for it and fetch its report.
  report [run]           Copy a run's report (default: the newest) to
                         _output/dizzy/soak/<run>/.

Settings (environment):
  DIZZY_SOAK_SERVICE          dizzy service to run, dizzy <service> chaos
                              (default mix).
  DIZZY_SOAK_DURATION         run time such as 6h or 90m; 0 runs until stopped
                              (default 6h).
  DIZZY_SCENARIO              scenario file (default <overlay>/dizzy-soak/scenario.yaml).
  DIZZY_ARGS                  extra dizzy flags, such as --set resources.servers=10.
  DIZZY_VERSION               dizzy image tag (default the pin of hack/dizzy.sh).
  DIZZY_AUTH_URL              Keystone URL (default
                              http://<DIZZY_CP_NAME>-keystone.<DIZZY_CP_NAMESPACE>.svc:5000/v3).
  DIZZY_CP_NAME               ControlPlane name (default controlplane).
  DIZZY_CP_NAMESPACE          ControlPlane namespace (default openstack).
  EXTERNAL_OVERLAY            overlay root (default deploy/lab/metal-stack).
  DIZZY_SOAK_NAMESPACES       platform namespaces to sample, space-separated
                              (default every namespace but kube-system,
                              kube-public, kube-node-lease, default and dizzy).
  DIZZY_SOAK_SAMPLE_INTERVAL  seconds between platform samples (default 60).
  DIZZY_SOAK_MAX_ERROR_RATE   maximum share of failed operations in percent
                              (default empty: no check).
  DIZZY_SOAK_MAX_P95_SECONDS  maximum overall p95 latency in seconds
                              (default empty: no check).
EOF
}

# soak_fail <message> [kubectl output] — Log the error, and kubectl's output
# when the read failed for another reason than absence, and exit 1.
soak_fail() {
  log "ERROR: $1"
  if [[ -n "${2:-}" && "$2" != *"(NotFound)"* ]]; then
    log "       ${2%%$'\n'*}"
  fi
  exit 1
}

# soak_read <command...> — Run a kubectl read whose output the caller parses
# with its stderr kept apart: what kubectl prints there beside a read that
# succeeds, such as a warning or a discovery error, goes on to stderr. When
# the read fails, its stderr goes to stdout instead, for soak_fail.
soak_read() {
  local err out rc=0
  err="$(mktemp "${TMPDIR:-/tmp}/dizzy-soak-err.XXXXXX")"
  out="$("$@" 2>"${err}")" || rc=$?
  if ((rc == 0)); then
    printf '%s\n' "${out}"
    cat "${err}" >&2
  else
    cat "${err}"
  fi
  rm -f "${err}"
  return "${rc}"
}

# soak_job_json — The Job dizzy-soak as JSON, empty when there is none.
soak_job_json() {
  kubectl get job "${SOAK_JOB}" -n "${SOAK_NAMESPACE}" -o json --ignore-not-found
}

# soak_job_state <job json> — none, running, complete or failed: the one
# definition start, status and stop share. A Job without a Complete or
# Failed condition runs, also while it has no active pod, such as before its
# pod is created or while an evicted pod terminates.
soak_job_state() {
  if [[ -z "$1" ]]; then
    echo none
    return
  fi
  jq -r '[.status.conditions[]? | select(.status == "True") | .type] as $c
    | if any($c[]; . == "Complete") then "complete"
      elif any($c[]; . == "Failed") then "failed"
      else "running" end' <<<"$1"
}

# soak_running_pod — The name of the soak's Running pod, empty without one.
soak_running_pod() {
  kubectl get pods -n "${SOAK_NAMESPACE}" -l "${SOAK_POD_SELECTOR}" \
    --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}'
}

# soak_valid_duration <value> — 0, or h, m and s parts with a non-zero one.
soak_valid_duration() {
  local re='^([0-9]+h)?([0-9]+m)?([0-9]+s)?$'
  [[ "$1" == "0" ]] && return 0
  [[ -n "$1" && "$1" =~ $re && "$1" =~ [1-9] ]]
}

# soak_number_in <value> <awk condition> — a decimal number for which the
# condition on v holds.
soak_number_in() {
  local re='^[0-9]+([.][0-9]+)?$'
  [[ "$1" =~ $re ]] && awk -v v="$1" "BEGIN { exit !($2) }"
}

# ---------------------------------------------------------------------------
# soak_preflight — The nine checks of start, in order, before anything is
# changed. Sets SOAK_SCENARIO_FILE.
# ---------------------------------------------------------------------------
soak_preflight() {
  local cmd out states ns="${DIZZY_CP_NAMESPACE}"
  for cmd in kubectl jq; do
    if ! command -v "${cmd}" >/dev/null 2>&1; then
      soak_fail "'${cmd}' is not installed or not in PATH."
    fi
  done

  # (a) the cluster answers.
  if ! out="$(kubectl version --request-timeout=2s 2>&1)"; then
    soak_fail "kubectl is not configured or no cluster is reachable" "${out##*$'\n'}"
  fi
  # (b) the dizzy stack.
  if ! out="$(kubectl get namespace "${SOAK_NAMESPACE}" -o name 2>&1)"; then
    soak_fail "the dizzy stack is not installed (no namespace ${SOAK_NAMESPACE}); deploy it with WITH_DIZZY=true first" "${out}"
  fi
  # (c) the overlay.
  if [[ ! -f "${SOAK_DIR}/kustomization.yaml" ]]; then
    soak_fail "no soak overlay at ${SOAK_DIR}/kustomization.yaml (EXTERNAL_OVERLAY='${EXTERNAL_OVERLAY}')"
  fi
  # (d) a Ready ControlPlane.
  if ! out="$(soak_read kubectl get controlplanes.c5c3.io -n "${ns}" -o json)"; then
    soak_fail "no ControlPlane found in namespace ${ns}" "${out}"
  fi
  if ! states="$(jq -r '.items[] | .metadata.name + " "
      + (([.status.conditions[]? | select(.type == "Ready") | .status] | first) // "Unknown")' <<<"${out}" 2>&1)"; then
    soak_fail "cannot read the ControlPlanes in namespace ${ns}" "${states}"
  fi
  if [[ -z "${states}" ]]; then
    soak_fail "no ControlPlane found in namespace ${ns}"
  fi
  local name status
  while read -r name status; do
    if [[ "${status}" != "True" ]]; then
      soak_fail "ControlPlane ${ns}/${name} is not Ready"
    fi
  done <<<"${states}"
  # (e) the credentials K-ORC creates the soak's identity with.
  if ! out="$(kubectl get secret k-orc-clouds-yaml -n "${ns}" -o name 2>&1)"; then
    soak_fail "the Secret k-orc-clouds-yaml is missing in namespace ${ns}" "${out}"
  fi
  # (f) the image and flavor the default scenario names.
  if [[ -z "${DIZZY_SCENARIO}" ]]; then
    local available
    if ! out="$(soak_read kubectl get images.openstack.k-orc.cloud/hvo-cirros-kvm \
        flavors.openstack.k-orc.cloud/hvo-smoke-test-flavor -n "${ns}" -o json)" ||
      ! available="$(jq -r '[.items[] | select(any(.status.conditions[]?; .type == "Available" and .status == "True"))]
        | length' <<<"${out}" 2>/dev/null)" || [[ "${available}" != "2" ]]; then
      soak_fail "the lab fixtures image/hvo-cirros-kvm and flavor/hvo-smoke-test-flavor are missing or not Available in ${ns}; apply ${OVERLAY_ROOT}/hypervisor-fixtures or name a scenario with DIZZY_SCENARIO"
    fi
  fi
  # (g) the scenario.
  SOAK_SCENARIO_FILE="${DIZZY_SCENARIO:-${SOAK_DIR}/scenario.yaml}"
  if [[ ! -f "${SOAK_SCENARIO_FILE}" ]]; then
    soak_fail "scenario file not found: ${SOAK_SCENARIO_FILE}"
  fi
  # (h) the settings.
  if ! soak_valid_duration "${DIZZY_SOAK_DURATION}"; then
    soak_fail "DIZZY_SOAK_DURATION must be 0 or a duration such as 6h"
  fi
  if [[ ! "${DIZZY_SOAK_SAMPLE_INTERVAL}" =~ ^[1-9][0-9]*$ ]]; then
    soak_fail "DIZZY_SOAK_SAMPLE_INTERVAL must be a positive number of seconds such as 60"
  fi
  if [[ -n "${DIZZY_SOAK_MAX_ERROR_RATE}" ]] && ! soak_number_in "${DIZZY_SOAK_MAX_ERROR_RATE}" 'v <= 100'; then
    soak_fail "DIZZY_SOAK_MAX_ERROR_RATE must be empty or a percentage from 0 to 100"
  fi
  if [[ -n "${DIZZY_SOAK_MAX_P95_SECONDS}" ]] && ! soak_number_in "${DIZZY_SOAK_MAX_P95_SECONDS}" 'v > 0'; then
    soak_fail "DIZZY_SOAK_MAX_P95_SECONDS must be empty or a positive number of seconds"
  fi
  if [[ ! "${DIZZY_VERSION}" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]]; then
    soak_fail "DIZZY_VERSION must be an image tag of letters, digits, '.', '_' and '-'"
  fi
  # (i) no soak runs.
  if ! out="$(soak_read soak_job_json)"; then
    soak_fail "cannot read the Job ${SOAK_NAMESPACE}/${SOAK_JOB}" "${out}"
  fi
  if [[ "$(soak_job_state "${out}")" == "running" ]]; then
    soak_fail "a soak is already running; stop it with make dizzy-soak-stop"
  fi
}

# soak_apply <what> <kubectl create args...> — Apply what `kubectl create
# ARGS --dry-run=client -o yaml` renders, so a second start replaces it.
soak_apply() {
  local what="$1"
  shift
  if ! kubectl create "$@" -n "${SOAK_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null; then
    soak_fail "cannot apply ${what} in ${SOAK_NAMESPACE} (kubectl's error is above)"
  fi
}

# soak_write_clouds — Write the clouds.yaml of the soak's user to
# SOAK_CLOUDS_TMP, mode 600. The password is a single-quoted YAML scalar with
# a ' doubled.
soak_write_clouds() {
  local encoded password escaped
  local auth_url="${DIZZY_AUTH_URL:-http://${DIZZY_CP_NAME}-keystone.${DIZZY_CP_NAMESPACE}.svc:5000/v3}"
  if ! encoded="$(soak_read kubectl get secret "${SOAK_PASSWORD_SECRET}" -n "${DIZZY_CP_NAMESPACE}" \
      -o jsonpath='{.data.password}')"; then
    soak_fail "cannot read the Secret ${DIZZY_CP_NAMESPACE}/${SOAK_PASSWORD_SECRET}" "${encoded}"
  fi
  if ! password="$(printf '%s' "${encoded}" | base64 -d)"; then
    soak_fail "the Secret ${DIZZY_CP_NAMESPACE}/${SOAK_PASSWORD_SECRET} holds no valid base64 password"
  fi
  if [[ -z "${password}" ]]; then
    soak_fail "the Secret ${DIZZY_CP_NAMESPACE}/${SOAK_PASSWORD_SECRET} has an empty password key"
  fi
  escaped="$(printf '%s' "${password}" | sed "s/'/''/g")"
  SOAK_CLOUDS_TMP="$(mktemp "${TMPDIR:-/tmp}/dizzy-soak-clouds.XXXXXX")"
  chmod 600 "${SOAK_CLOUDS_TMP}"
  cat >"${SOAK_CLOUDS_TMP}" <<EOF
clouds:
  dizzy-soak:
    auth:
      auth_url: ${auth_url}
      username: dizzy-soak
      password: '${escaped}'
      project_name: dizzy-soak
      user_domain_name: Default
      project_domain_name: Default
    identity_api_version: 3
    interface: internal
    region_name: RegionOne
EOF
}

# ---------------------------------------------------------------------------
# soak_start — Start the soak: the preflights, then the objects, then the
# Job, then the wait for its pod.
# ---------------------------------------------------------------------------
soak_start() {
  local out
  soak_preflight
  trap 'rm -f "${SOAK_CLOUDS_TMP:-}"' EXIT

  # A finished soak's Job, and a reader pod a report left behind, which
  # would hold the claim.
  if [[ -n "$(soak_job_json)" ]]; then
    log "Deleting the finished Job ${SOAK_NAMESPACE}/${SOAK_JOB}..."
    if ! kubectl delete job "${SOAK_JOB}" -n "${SOAK_NAMESPACE}" --cascade=foreground --wait \
        --timeout="${SOAK_POD_TIMEOUT}s" >/dev/null; then
      soak_fail "cannot delete the finished Job ${SOAK_NAMESPACE}/${SOAK_JOB} (kubectl's error is above)"
    fi
  fi
  if ! kubectl delete pod "${SOAK_READER_POD}" -n "${SOAK_NAMESPACE}" --ignore-not-found >/dev/null; then
    soak_fail "cannot delete the pod ${SOAK_NAMESPACE}/${SOAK_READER_POD} (kubectl's error is above)"
  fi

  # The user's password, once: K-ORC applies a password again only when the
  # Secret's name changes.
  if [[ -z "$(kubectl get secret "${SOAK_PASSWORD_SECRET}" -n "${DIZZY_CP_NAMESPACE}" --ignore-not-found -o name)" ]]; then
    local password
    password="$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom 2>/dev/null | head -c 32)" || true
    if [[ ${#password} -ne 32 ]]; then
      soak_fail "cannot generate the password of the soak's user"
    fi
    log "Creating the Secret ${DIZZY_CP_NAMESPACE}/${SOAK_PASSWORD_SECRET}..."
    if ! printf '%s' "${password}" | kubectl create secret generic "${SOAK_PASSWORD_SECRET}" \
        -n "${DIZZY_CP_NAMESPACE}" --from-file=password=/dev/stdin >/dev/null; then
      soak_fail "cannot create the Secret ${DIZZY_CP_NAMESPACE}/${SOAK_PASSWORD_SECRET} (kubectl's error is above)"
    fi
  fi

  log "Applying ${SOAK_DIR}..."
  if ! kubectl apply -k "${SOAK_DIR}" >/dev/null; then
    soak_fail "cannot apply ${SOAK_DIR} (kubectl's error is above)"
  fi
  log "Waiting for the soak identity (K-ORC Domain, Project, User, Role, RoleAssignment)..."
  if ! out="$(kubectl wait -f "${SOAK_DIR}/identity.yaml" --for=condition=Available \
      --timeout="${SOAK_IDENTITY_TIMEOUT}s" 2>&1)"; then
    log "ERROR: the soak identity did not become Available within ${SOAK_IDENTITY_TIMEOUT}s:"
    log "       ${out##*$'\n'}"
    kubectl get -f "${SOAK_DIR}/identity.yaml" || true
    exit 1
  fi

  soak_write_clouds
  soak_apply "the Secret dizzy-soak-clouds" secret generic dizzy-soak-clouds \
    --from-file=clouds.yaml="${SOAK_CLOUDS_TMP}"
  soak_apply "the ConfigMap dizzy-soak-runner" configmap dizzy-soak-runner \
    --from-file=dizzy-soak-runner.sh="${SOAK_SCRIPT_DIR}/dizzy-soak-runner.sh"
  soak_apply "the ConfigMap dizzy-soak-scenario" configmap dizzy-soak-scenario \
    --from-file=scenario.yaml="${SOAK_SCENARIO_FILE}"
  soak_apply "the ConfigMap dizzy-soak-config" configmap dizzy-soak-config \
    --from-literal=DIZZY_SOAK_SERVICE="${DIZZY_SOAK_SERVICE}" \
    --from-literal=DIZZY_SOAK_DURATION="${DIZZY_SOAK_DURATION}" \
    --from-literal=DIZZY_ARGS="${DIZZY_ARGS}" \
    --from-literal=DIZZY_VERSION="${DIZZY_VERSION}" \
    --from-literal=DIZZY_SOAK_NAMESPACES="${DIZZY_SOAK_NAMESPACES}" \
    --from-literal=DIZZY_SOAK_SAMPLE_INTERVAL="${DIZZY_SOAK_SAMPLE_INTERVAL}" \
    --from-literal=DIZZY_SOAK_MAX_ERROR_RATE="${DIZZY_SOAK_MAX_ERROR_RATE}" \
    --from-literal=DIZZY_SOAK_MAX_P95_SECONDS="${DIZZY_SOAK_MAX_P95_SECONDS}"

  log "Creating the Job ${SOAK_NAMESPACE}/${SOAK_JOB} (dizzy ${DIZZY_VERSION}, ${DIZZY_SOAK_SERVICE}, ${DIZZY_SOAK_DURATION})..."
  if ! sed "s#ghcr.io/b42labs/dizzy:DIZZY_VERSION#ghcr.io/b42labs/dizzy:${DIZZY_VERSION}#" "${SOAK_DIR}/job.yaml" |
    kubectl create -f - >/dev/null; then
    soak_fail "cannot create the Job ${SOAK_NAMESPACE}/${SOAK_JOB} (kubectl's error is above)"
  fi

  local waited=0 pod="" phase=""
  while ((waited <= SOAK_POD_TIMEOUT)); do
    read -r pod phase <<<"$(kubectl get pods -n "${SOAK_NAMESPACE}" -l "${SOAK_POD_SELECTOR}" \
      -o jsonpath='{.items[0].metadata.name} {.items[0].status.phase}' 2>/dev/null)" || true
    case "${phase}" in
      Running | Succeeded) break ;;
      Failed)
        log "ERROR: the soak pod ${SOAK_NAMESPACE}/${pod} failed; its last log lines:"
        kubectl logs "${pod}" -n "${SOAK_NAMESPACE}" -c runner --tail=20 || true
        exit 1
        ;;
    esac
    sleep "${SOAK_POLL_INTERVAL}"
    waited=$((waited + SOAK_POLL_INTERVAL))
  done
  if [[ "${phase}" != "Running" && "${phase}" != "Succeeded" ]]; then
    soak_fail "the soak pod did not start within ${SOAK_POD_TIMEOUT}s (kubectl describe pod -n ${SOAK_NAMESPACE} -l ${SOAK_POD_SELECTOR})"
  fi

  log "The soak runs in the pod ${SOAK_NAMESPACE}/${pod}. Follow it with:"
  log "  make dizzy-soak-status    its state and last log lines"
  log "  make dizzy-soak-stop      stop it and fetch its report"
  log "  make dizzy-soak-report    fetch its report directory"
}

# ---------------------------------------------------------------------------
# soak_status — Print the soak's state, pod and start time and the last log
# lines of its runner. Exits 1 when there is no soak.
# ---------------------------------------------------------------------------
soak_status() {
  local job state pod="" phase=""
  if ! job="$(soak_read soak_job_json)"; then
    soak_fail "cannot read the Job ${SOAK_NAMESPACE}/${SOAK_JOB}" "${job}"
  fi
  state="$(soak_job_state "${job}")"
  echo "soak: ${state}"
  if [[ "${state}" == "none" ]]; then
    exit 1
  fi
  read -r pod phase <<<"$(kubectl get pods -n "${SOAK_NAMESPACE}" -l "${SOAK_POD_SELECTOR}" \
    -o jsonpath='{.items[0].metadata.name} {.items[0].status.phase}' 2>/dev/null)" || true
  echo "pod: ${pod:-none} (${phase:-no phase})"
  echo "started: $(jq -r '.status.startTime // "not yet"' <<<"${job}")"
  if [[ -n "${pod}" ]]; then
    echo "last log lines:"
    kubectl logs "${pod}" -n "${SOAK_NAMESPACE}" -c runner --tail=10 2>&1 || true
  fi
}

# ---------------------------------------------------------------------------
# soak_stop — Send TERM to the runner, wait for the Job to finish and fetch
# the report. Exits 0 when the Job is Complete, 1 otherwise. It deletes a Job
# whose pod never ran and exits 1.
# ---------------------------------------------------------------------------
soak_stop() {
  local job pods pod state waited=0
  if ! job="$(soak_read soak_job_json)"; then
    soak_fail "cannot read the Job ${SOAK_NAMESPACE}/${SOAK_JOB}" "${job}"
  fi
  if [[ "$(soak_job_state "${job}")" != "running" ]]; then
    soak_fail "no soak is running"
  fi
  # One read decides between TERM and deleting the Job, and a read that
  # fails stops here: read as no pod, it would delete a running soak's Job.
  if ! pods="$(soak_read kubectl get pods -n "${SOAK_NAMESPACE}" -l "${SOAK_POD_SELECTOR}" -o json)"; then
    soak_fail "cannot read the pods of the Job ${SOAK_NAMESPACE}/${SOAK_JOB}" "${pods}"
  fi
  pod="$(jq -r '[.items[] | select(.status.phase == "Running") | .metadata.name][0] // empty' <<<"${pods}")"
  if [[ -z "${pod}" ]]; then
    if jq -e 'any(.items[]; .status.phase != "Pending")' <<<"${pods}" >/dev/null; then
      soak_fail "no soak is running"
    fi
    # The runner never started, as with a dizzy image that cannot be pulled:
    # dizzy created nothing and wrote no report, so deleting the Job stops it.
    log "The soak's pod never ran; deleting the Job ${SOAK_NAMESPACE}/${SOAK_JOB}..."
    if ! kubectl delete job "${SOAK_JOB}" -n "${SOAK_NAMESPACE}" --cascade=foreground --wait \
        --timeout="${SOAK_POD_TIMEOUT}s" >/dev/null; then
      soak_fail "cannot delete the Job ${SOAK_NAMESPACE}/${SOAK_JOB} (kubectl's error is above)"
    fi
    soak_fail "the soak's pod never ran, so there is no report; make dizzy-soak-start starts a soak again"
  fi

  log "Stopping the soak in ${SOAK_NAMESPACE}/${pod}: dizzy removes its resources, then the runner writes the report..."
  if ! kubectl exec "${pod}" -n "${SOAK_NAMESPACE}" -c runner -- kill -TERM 1; then
    soak_fail "cannot send TERM to the runner of ${SOAK_NAMESPACE}/${pod} (kubectl's error is above)"
  fi
  state="running"
  while ((waited < SOAK_STOP_TIMEOUT)); do
    sleep "${SOAK_POLL_INTERVAL}"
    waited=$((waited + SOAK_POLL_INTERVAL))
    state="$(soak_job_state "$(soak_job_json 2>/dev/null || true)")"
    if [[ "${state}" == "complete" || "${state}" == "failed" ]]; then
      break
    fi
  done
  if [[ "${state}" != "complete" && "${state}" != "failed" ]]; then
    soak_fail "the soak did not finish within ${SOAK_STOP_TIMEOUT}s; see make dizzy-soak-status"
  fi
  log "The soak's Job is ${state}."

  if ! (soak_report ""); then
    soak_fail "the soak finished ${state}, but its report could not be fetched; retry with make dizzy-soak-report"
  fi
  if [[ "${state}" != "complete" ]]; then
    exit 1
  fi
}

# soak_reader_pod — The manifest of the pod dizzy-soak-reader: the image of
# the container runner of job.yaml, asleep, with the claim mounted read-only.
soak_reader_pod() {
  local image
  image="$(awk '/^[[:space:]]*- name: runner$/ { runner = 1; next }
    runner && /^[[:space:]]*image:/ { print $2; exit }' "${SOAK_DIR}/job.yaml")"
  jq -n --arg name "${SOAK_READER_POD}" --arg namespace "${SOAK_NAMESPACE}" --arg image "${image}" \
    --arg claim "${SOAK_CLAIM}" '{
    apiVersion: "v1", kind: "Pod",
    metadata: {name: $name, namespace: $namespace},
    spec: {
      restartPolicy: "Never", automountServiceAccountToken: false, terminationGracePeriodSeconds: 1,
      securityContext: {runAsNonRoot: true, runAsUser: 65534, runAsGroup: 65534, seccompProfile: {type: "RuntimeDefault"}},
      containers: [{
        name: "reader", image: $image, imagePullPolicy: "IfNotPresent", command: ["sleep", "3600"],
        resources: {requests: {cpu: "10m", memory: "16Mi"}},
        securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: ["ALL"]}},
        volumeMounts: [{name: "reports", mountPath: "/reports", readOnly: true}]
      }],
      volumes: [{name: "reports", persistentVolumeClaim: {claimName: $claim, readOnly: true}}]
    }}'
}

# ---------------------------------------------------------------------------
# soak_report [run] — Copy /reports/<run> (default: the newest run) from the
# claim to SOAK_OUTPUT_DIR/<run>/: from the soak's pod while it runs, from a
# reader pod otherwise, which an EXIT trap deletes.
# ---------------------------------------------------------------------------
soak_report() {
  local run="${1:-}" pod container runs dest verdict
  pod="$(soak_running_pod 2>/dev/null)" || true
  container="runner"
  if [[ -z "${pod}" ]]; then
    if ! kubectl delete pod "${SOAK_READER_POD}" -n "${SOAK_NAMESPACE}" --ignore-not-found >/dev/null; then
      soak_fail "cannot delete the pod ${SOAK_NAMESPACE}/${SOAK_READER_POD} a report left behind (kubectl's error is above)"
    fi
    trap 'kubectl delete pod "${SOAK_READER_POD}" -n "${SOAK_NAMESPACE}" --ignore-not-found >/dev/null 2>&1 || true' EXIT
    log "Starting the pod ${SOAK_NAMESPACE}/${SOAK_READER_POD}, which mounts the claim ${SOAK_CLAIM} read-only..."
    if ! soak_reader_pod | kubectl create -f - >/dev/null; then
      soak_fail "cannot create the pod ${SOAK_NAMESPACE}/${SOAK_READER_POD} (kubectl's error is above)"
    fi
    if ! kubectl wait "pod/${SOAK_READER_POD}" -n "${SOAK_NAMESPACE}" --for=condition=Ready \
        --timeout="${SOAK_POD_TIMEOUT}s" >/dev/null; then
      soak_fail "the pod ${SOAK_NAMESPACE}/${SOAK_READER_POD} did not become Ready within ${SOAK_POD_TIMEOUT}s"
    fi
    pod="${SOAK_READER_POD}"
    container="reader"
  fi

  if ! runs="$(kubectl exec "${pod}" -n "${SOAK_NAMESPACE}" -c "${container}" -- ls -1 /reports 2>&1)"; then
    soak_fail "cannot list /reports in ${SOAK_NAMESPACE}/${pod}" "${runs}"
  fi
  runs="$(grep -E '^[0-9]{8}T[0-9]{6}Z$' <<<"${runs}" | sort || true)"
  if [[ -z "${runs}" ]]; then
    soak_fail "no soak report on the claim ${SOAK_CLAIM}"
  fi
  if [[ -z "${run}" ]]; then
    run="$(tail -n 1 <<<"${runs}")"
  elif ! grep -qxF -- "${run}" <<<"${runs}"; then
    log "ERROR: no soak report ${run} on the claim ${SOAK_CLAIM}; the claim holds:"
    printf '%s\n' "${runs}" | sed 's/^/         /'
    exit 1
  fi

  dest="${SOAK_OUTPUT_DIR}/${run}"
  mkdir -p "${SOAK_OUTPUT_DIR}"
  rm -rf "${dest}"
  log "Copying /reports/${run} from ${SOAK_NAMESPACE}/${pod} to ${dest}..."
  if ! kubectl cp -n "${SOAK_NAMESPACE}" -c "${container}" "${pod}:/reports/${run}" "${dest}" >/dev/null; then
    soak_fail "kubectl cp of /reports/${run} from ${SOAK_NAMESPACE}/${pod} failed (kubectl's error is above)"
  fi

  if [[ -f "${dest}/verdict.json" ]]; then
    verdict="$(jq -r '.verdict' "${dest}/verdict.json")"
    log "Report: ${dest}/report.md"
    log "Verdict: ${verdict}"
  else
    log "Report: ${dest} (the run is still going; report.md and verdict.json are written at its end)"
  fi
}

# ---------------------------------------------------------------------------
# soak_main
# ---------------------------------------------------------------------------
soak_main() {
  case "${1:-}" in
    start) soak_start ;;
    status) soak_status ;;
    stop) soak_stop ;;
    report) soak_report "${2:-}" ;;
    *)
      soak_usage
      exit 1
      ;;
  esac
}

# Run soak_main only when executed directly so unit tests (tests/unit/hack/)
# can source this script and exercise individual functions.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  soak_main "$@"
fi
