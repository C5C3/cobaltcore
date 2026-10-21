#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/keystone-pressure-ab.sh — Compare two Keystone service images under
# dizzy's closed-loop token-path load, `dizzy keystone pressure`, on a kind
# stack. It is the keystone-pressure CI job and `make keystone-pressure-ab`.
#
# Two standalone Keystone CRs run side by side in the namespace openstack of
# the `make deploy-infra` stack: keystone-pressure-base on the image of the
# build without the change, keystone-pressure-patched on the image of the
# build with it. Each has a database of its own on openstack-db and a
# Memcached of its own, because two Keystones on one Memcached read each
# other's cached users (see tests/e2e/nova/basic-deployment/00-keystone-cr.yaml)
# and a shared cache would let one run warm or flush the other's. A client pod
# runs the pinned dizzy image in the same namespace and reaches both through
# their Services, so no Gateway, port-forward or TLS of the workstation adds
# to the latencies.
#
# For every profile of tests/load/keystone-pressure/ in KEYSTONE_PRESSURE_PROFILES,
# the client runs the profile against both Keystones one after the other, with
# the same scenario and seed, and compares the two run records with
# `dizzy keystone report --compare`, base first. The order alternates from
# profile to profile, so neither build always runs first on a warming stack.
#
# Run records, comparisons (text, HTML and JSON) and summary.md land in
# KEYSTONE_PRESSURE_OUTPUT; summary.md is appended to GITHUB_STEP_SUMMARY when
# that is set. The script exits 1 when a preflight, a deployment, a run or a
# comparison fails, never on a latency delta: reading the numbers is the
# reviewer's job. When both images are the same build, the deltas are the
# noise of the stack.
#
# Settings (environment):
#   KEYSTONE_PRESSURE_BASE_IMAGE     (required) local image of the build without
#                                    the change, e.g. ghcr.io/c5c3/keystone:2026.2-main.
#   KEYSTONE_PRESSURE_PATCHED_IMAGE  (required) local image of the build with it.
#   KEYSTONE_PRESSURE_RELEASE        OpenStack release both images ship
#                                    (default: the newest under releases/).
#   KEYSTONE_PRESSURE_PROFILES       profiles to run, space-separated (default
#                                    the four Keystone-only ones).
#   KEYSTONE_PRESSURE_DURATION       dizzy --duration of every run, e.g. 2m
#                                    (default: each profile's own).
#   KEYSTONE_PRESSURE_PROFILE_DURATIONS
#                                    per-profile --duration, space-separated
#                                    <profile>=<duration> pairs that take
#                                    precedence over KEYSTONE_PRESSURE_DURATION,
#                                    e.g. effective-fan=6m.
#   KEYSTONE_PRESSURE_ARGS           extra `dizzy keystone pressure` flags.
#   KEYSTONE_PRESSURE_REPLICAS       Keystone API replicas per build (default 1).
#   KEYSTONE_PRESSURE_OUTPUT         output directory (default
#                                    _output/keystone-pressure/<release>-<UTC time>).
#   KEYSTONE_PRESSURE_LOAD_IMAGES    true: tag both images and the dizzy image
#                                    and load them into kind (default true);
#                                    false: the nodes hold them already.
#   KEYSTONE_PRESSURE_KEEP           true: leave the Keystones and the client pod.
#   DIZZY_VERSION                    dizzy image tag (default the pin of hack/dizzy.sh).
#   KIND_CLUSTER                     kind cluster name (default cobaltcore).

set -euo pipefail

AB_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# The dizzy pin and log come from hack/dizzy.sh, the one pin of the repository.
# shellcheck source=hack/dizzy.sh
source "${AB_SCRIPT_DIR}/dizzy.sh"

# The profiles a standalone Keystone can run. The middleware profiles
# (token-propagation, middleware-cache, bad-token-flood) send tokens to Nova,
# Neutron, Cinder and Glance, which a standalone Keystone's catalog does not
# list. revocation-lookup, whose revocation events stay in the database,
# runs last.
AB_DEFAULT_PROFILES="warm-cache effective-fan cold-role-cache revocation-lookup"

KEYSTONE_PRESSURE_BASE_IMAGE="${KEYSTONE_PRESSURE_BASE_IMAGE:-}"
KEYSTONE_PRESSURE_PATCHED_IMAGE="${KEYSTONE_PRESSURE_PATCHED_IMAGE:-}"
KEYSTONE_PRESSURE_RELEASE="${KEYSTONE_PRESSURE_RELEASE:-}"
KEYSTONE_PRESSURE_PROFILES="${KEYSTONE_PRESSURE_PROFILES:-${AB_DEFAULT_PROFILES}}"
KEYSTONE_PRESSURE_DURATION="${KEYSTONE_PRESSURE_DURATION:-}"
KEYSTONE_PRESSURE_PROFILE_DURATIONS="${KEYSTONE_PRESSURE_PROFILE_DURATIONS:-}"
KEYSTONE_PRESSURE_ARGS="${KEYSTONE_PRESSURE_ARGS:-}"
KEYSTONE_PRESSURE_REPLICAS="${KEYSTONE_PRESSURE_REPLICAS:-1}"
KEYSTONE_PRESSURE_OUTPUT="${KEYSTONE_PRESSURE_OUTPUT:-}"
KEYSTONE_PRESSURE_LOAD_IMAGES="${KEYSTONE_PRESSURE_LOAD_IMAGES:-true}"
KEYSTONE_PRESSURE_KEEP="${KEYSTONE_PRESSURE_KEEP:-false}"
KIND_CLUSTER="${KIND_CLUSTER:-cobaltcore}"

# The objects of the run and the waits. Plain variables, so the unit test can
# point them elsewhere after sourcing.
AB_NAMESPACE="openstack"
AB_PREFIX="keystone-pressure"
AB_SIDES="base patched"
AB_CLIENT_POD="${AB_PREFIX}-client"
AB_CLOUDS_SECRET="${AB_PREFIX}-clouds"
AB_SCENARIOS_CONFIGMAP="${AB_PREFIX}-scenarios"
AB_KEYSTONE_IMAGE_REPO="ghcr.io/c5c3/keystone"
AB_DIZZY_IMAGE_REPO="ghcr.io/b42labs/dizzy"
AB_READY_TIMEOUT="900s"
AB_CACHE_TIMEOUT="300s"
AB_POD_TIMEOUT="300s"
AB_DELETE_TIMEOUT="300s"

# Set by ab_preflight and ab_main.
AB_OUTPUT_DIR=""
AB_A_A="false"

# ---------------------------------------------------------------------------
# ab_usage — Print the settings to stderr.
# ---------------------------------------------------------------------------
ab_usage() {
  cat >&2 <<EOF
Usage: KEYSTONE_PRESSURE_BASE_IMAGE=<image> KEYSTONE_PRESSURE_PATCHED_IMAGE=<image> \\
       hack/keystone-pressure-ab.sh [cleanup]

Runs dizzy keystone pressure against two Keystones, one per image, on the kind
stack of the current kubeconfig context and compares them per profile.
With 'cleanup' it only deletes what a kept run (KEYSTONE_PRESSURE_KEEP=true)
left behind.

Settings (environment):
  KEYSTONE_PRESSURE_BASE_IMAGE     local image of the build without the change (required).
  KEYSTONE_PRESSURE_PATCHED_IMAGE  local image of the build with it (required).
  KEYSTONE_PRESSURE_RELEASE        release of both images (default the newest under releases/).
  KEYSTONE_PRESSURE_PROFILES       profiles, space-separated (default: ${AB_DEFAULT_PROFILES}).
  KEYSTONE_PRESSURE_DURATION       dizzy --duration of every run (default each profile's own).
  KEYSTONE_PRESSURE_PROFILE_DURATIONS
                                   <profile>=<duration> pairs that override it per profile.
  KEYSTONE_PRESSURE_ARGS           extra dizzy keystone pressure flags.
  KEYSTONE_PRESSURE_REPLICAS       Keystone API replicas per build (default 1).
  KEYSTONE_PRESSURE_OUTPUT         output directory (default _output/keystone-pressure/<release>-<time>).
  KEYSTONE_PRESSURE_LOAD_IMAGES    true: load the images into kind KIND_CLUSTER (default true).
  KEYSTONE_PRESSURE_KEEP           true: leave the Keystones and the client pod.
  DIZZY_VERSION                    dizzy image tag (default ${DIZZY_VERSION}).
  KIND_CLUSTER                     kind cluster name (default cobaltcore).
EOF
}

# ab_fail <message> — Log the error and exit 1.
ab_fail() {
  log "ERROR: $1"
  exit 1
}

# ab_newest_release — Print the newest release under releases/. A plain sort
# orders YYYY.N releases, whose N is a single digit.
ab_newest_release() {
  local dir newest=""
  for dir in "${REPO_ROOT}"/releases/*/; do
    dir="${dir%/}"
    newest="$(printf '%s\n%s\n' "${newest}" "${dir##*/}" | sort | tail -n 1)"
  done
  printf '%s\n' "${newest}"
}

# AB_DURATION_PATTERN — what dizzy's --duration takes here: a sequence of
# whole numbers with a unit, such as 2m or 1m30s.
AB_DURATION_PATTERN='^([0-9]+(ms|s|m|h))+$'

# ab_duration <profile> — Print the --duration of the profile's runs: its pair
# in KEYSTONE_PRESSURE_PROFILE_DURATIONS, else KEYSTONE_PRESSURE_DURATION,
# else nothing, and the profile's own applies.
ab_duration() {
  local pair
  for pair in ${KEYSTONE_PRESSURE_PROFILE_DURATIONS}; do
    if [[ "${pair%%=*}" == "$1" ]]; then
      printf '%s\n' "${pair#*=}"
      return 0
    fi
  done
  printf '%s\n' "${KEYSTONE_PRESSURE_DURATION}"
}

# ab_image_tag <side> — The tag the Keystone of <side> runs: the release with a
# suffix, which the operator still parses as the release.
ab_image_tag() {
  printf '%s-pressure-%s\n' "${KEYSTONE_PRESSURE_RELEASE}" "$1"
}

# ---------------------------------------------------------------------------
# ab_preflight — Every check before anything is changed, each with a message
# of its own.
# ---------------------------------------------------------------------------
ab_preflight() {
  local cmd
  for cmd in kubectl base64; do
    command -v "${cmd}" >/dev/null 2>&1 || ab_fail "'${cmd}' is not installed or not in PATH."
  done
  if [[ "${KEYSTONE_PRESSURE_LOAD_IMAGES}" == "true" ]]; then
    for cmd in docker kind; do
      command -v "${cmd}" >/dev/null 2>&1 ||
        ab_fail "'${cmd}' is not installed or not in PATH; it loads the images into kind (KEYSTONE_PRESSURE_LOAD_IMAGES=false skips that)."
    done
  fi

  # The settings.
  [[ -n "${KEYSTONE_PRESSURE_BASE_IMAGE}" ]] ||
    ab_fail "KEYSTONE_PRESSURE_BASE_IMAGE is not set; name the image of the build without the change."
  [[ -n "${KEYSTONE_PRESSURE_PATCHED_IMAGE}" ]] ||
    ab_fail "KEYSTONE_PRESSURE_PATCHED_IMAGE is not set; name the image of the build with the change."
  if [[ -z "${KEYSTONE_PRESSURE_RELEASE}" ]]; then
    KEYSTONE_PRESSURE_RELEASE="$(ab_newest_release)"
  fi
  [[ "${KEYSTONE_PRESSURE_RELEASE}" =~ ^[0-9]{4}\.[0-9]+$ && -d "${REPO_ROOT}/releases/${KEYSTONE_PRESSURE_RELEASE}" ]] ||
    ab_fail "KEYSTONE_PRESSURE_RELEASE '${KEYSTONE_PRESSURE_RELEASE}' names no release under releases/."
  [[ -n "${KEYSTONE_PRESSURE_PROFILES// /}" ]] || ab_fail "KEYSTONE_PRESSURE_PROFILES names no profile."
  local profile
  for profile in ${KEYSTONE_PRESSURE_PROFILES}; do
    [[ "${profile}" =~ ^[a-z0-9][a-z0-9-]*$ && -f "${PRESSURE_PROFILE_DIR}/${profile}.yaml" ]] ||
      ab_fail "no pressure profile '${profile}' in ${PRESSURE_PROFILE_DIR}."
  done
  if [[ -n "${KEYSTONE_PRESSURE_DURATION}" && ! "${KEYSTONE_PRESSURE_DURATION}" =~ ${AB_DURATION_PATTERN} ]]; then
    ab_fail "KEYSTONE_PRESSURE_DURATION must be a duration such as 2m or 1m30s."
  fi
  local pair
  for pair in ${KEYSTONE_PRESSURE_PROFILE_DURATIONS}; do
    [[ "${pair}" == *=* && "${pair%%=*}" =~ ^[a-z0-9][a-z0-9-]*$ && "${pair#*=}" =~ ${AB_DURATION_PATTERN} ]] ||
      ab_fail "KEYSTONE_PRESSURE_PROFILE_DURATIONS holds '${pair}'; each entry is <profile>=<duration>, such as effective-fan=6m."
  done
  [[ "${KEYSTONE_PRESSURE_REPLICAS}" =~ ^[1-9][0-9]*$ ]] ||
    ab_fail "KEYSTONE_PRESSURE_REPLICAS must be a positive number."
  [[ "${DIZZY_VERSION}" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]] ||
    ab_fail "DIZZY_VERSION must be an image tag of letters, digits, '.', '_' and '-'."

  # The images.
  if [[ "${KEYSTONE_PRESSURE_LOAD_IMAGES}" == "true" ]]; then
    local image
    for image in "${KEYSTONE_PRESSURE_BASE_IMAGE}" "${KEYSTONE_PRESSURE_PATCHED_IMAGE}"; do
      docker image inspect "${image}" >/dev/null 2>&1 ||
        ab_fail "no local image ${image}; pull or build it first."
    done
  fi

  # The cluster and the stack.
  kubectl version --request-timeout=2s >/dev/null 2>&1 ||
    ab_fail "kubectl is not configured or no cluster is reachable"
  kubectl get crd keystones.keystone.openstack.c5c3.io >/dev/null 2>&1 ||
    ab_fail "the keystone-operator is not installed; run \`OPERATOR=keystone IMAGE_REPO=ghcr.io/c5c3/keystone-operator NAMESPACE=keystone-system hack/ci-deploy-operator.sh\` first"
  kubectl get crd memcacheds.memcached.c5c3.io >/dev/null 2>&1 ||
    ab_fail "the memcached-operator is not installed; run \`make deploy-infra\` first"
  kubectl get mariadbs.k8s.mariadb.com openstack-db -n "${AB_NAMESPACE}" >/dev/null 2>&1 ||
    ab_fail "no MariaDB ${AB_NAMESPACE}/openstack-db; run \`make deploy-infra\` first"
  local secret
  for secret in keystone-db keystone-admin; do
    kubectl get secret "${secret}" -n "${AB_NAMESPACE}" >/dev/null 2>&1 ||
      ab_fail "no Secret ${AB_NAMESPACE}/${secret}; run \`make deploy-infra\` first"
  done
}

# ---------------------------------------------------------------------------
# ab_load_images — Tag both Keystone images with their side's tag and load
# them and the dizzy image into kind.
# ---------------------------------------------------------------------------
ab_load_images() {
  local base_id patched_id
  base_id="$(docker image inspect -f '{{.Id}}' "${KEYSTONE_PRESSURE_BASE_IMAGE}")"
  patched_id="$(docker image inspect -f '{{.Id}}' "${KEYSTONE_PRESSURE_PATCHED_IMAGE}")"
  if [[ "${base_id}" == "${patched_id}" ]]; then
    AB_A_A="true"
    log "Both images are the same build (${base_id}): this is an A/A run, and its deltas are the stack's noise."
  fi

  docker tag "${KEYSTONE_PRESSURE_BASE_IMAGE}" "${AB_KEYSTONE_IMAGE_REPO}:$(ab_image_tag base)"
  docker tag "${KEYSTONE_PRESSURE_PATCHED_IMAGE}" "${AB_KEYSTONE_IMAGE_REPO}:$(ab_image_tag patched)"

  local dizzy_image="${AB_DIZZY_IMAGE_REPO}:${DIZZY_VERSION}"
  if ! docker image inspect "${dizzy_image}" >/dev/null 2>&1; then
    log "Pulling ${dizzy_image}..."
    docker pull "${dizzy_image}" >/dev/null
  fi

  local image
  for image in "${AB_KEYSTONE_IMAGE_REPO}:$(ab_image_tag base)" \
    "${AB_KEYSTONE_IMAGE_REPO}:$(ab_image_tag patched)" "${dizzy_image}"; do
    ab_kind_load "${image}"
  done
}

# ---------------------------------------------------------------------------
# ab_kind_load <image> — Load a local image into kind. A docker on the
# containerd image store keeps a pulled multi-platform image as an index with
# one platform's content, which `kind load docker-image` fails to import
# ("content digest ... not found"); the node's platform alone, saved to an
# archive, loads.
# ---------------------------------------------------------------------------
ab_kind_load() {
  local image="$1"
  log "Loading ${image} into kind cluster ${KIND_CLUSTER}..."
  if kind load docker-image "${image}" --name "${KIND_CLUSTER}" >/dev/null 2>&1; then
    return 0
  fi
  local arch archive rc=0
  arch="$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.architecture}')"
  archive="$(mktemp "${TMPDIR:-/tmp}/keystone-pressure-image.XXXXXX")"
  log "Loading ${image} for linux/${arch} from an archive instead..."
  docker save --platform "linux/${arch}" "${image}" -o "${archive}" &&
    kind load image-archive "${archive}" --name "${KIND_CLUSTER}" >/dev/null || rc=$?
  rm -f "${archive}"
  ((rc == 0)) || ab_fail "cannot load ${image} into kind cluster ${KIND_CLUSTER}"
}

# ---------------------------------------------------------------------------
# ab_render_side <side> — Print the Memcached and the Keystone of <side>.
# ---------------------------------------------------------------------------
ab_render_side() {
  local side="$1"
  local name="${AB_PREFIX}-${side}"
  cat <<EOF
apiVersion: memcached.c5c3.io/v1beta1
kind: Memcached
metadata:
  name: ${name}-cache
  namespace: ${AB_NAMESPACE}
  labels:
    app.kubernetes.io/part-of: ${AB_PREFIX}
spec:
  replicas: 1
  image: memcached:1.6
---
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: Keystone
metadata:
  name: ${name}
  namespace: ${AB_NAMESPACE}
  labels:
    app.kubernetes.io/part-of: ${AB_PREFIX}
spec:
  deployment:
    replicas: ${KEYSTONE_PRESSURE_REPLICAS}
    resources:
      requests:
        cpu: 500m
        memory: 256Mi
      limits:
        memory: 1Gi
  image:
    repository: ${AB_KEYSTONE_IMAGE_REPO}
    tag: "$(ab_image_tag "${side}")"
    pullPolicy: IfNotPresent
  database:
    clusterRef:
      name: openstack-db
    database: keystone_pressure_${side}
    secretRef:
      name: keystone-db
  cache:
    clusterRef:
      name: ${name}-cache
    backend: dogpile.cache.pymemcache
  fernet:
    rotationSchedule: "0 0 * * 0"
    maxActiveKeys: 3
  bootstrap:
    adminUser: admin
    adminPasswordSecretRef:
      name: keystone-admin
    region: RegionOne
EOF
}

# ---------------------------------------------------------------------------
# ab_render_client — Print the client pod: the dizzy image, idle until the
# script runs dizzy in it with kubectl exec.
# ---------------------------------------------------------------------------
ab_render_client() {
  cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${AB_CLIENT_POD}
  namespace: ${AB_NAMESPACE}
  labels:
    app.kubernetes.io/part-of: ${AB_PREFIX}
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 5
  securityContext:
    runAsNonRoot: true
    runAsUser: 65534
    runAsGroup: 65534
    fsGroup: 65534
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: dizzy
      image: ${AB_DIZZY_IMAGE_REPO}:${DIZZY_VERSION}
      imagePullPolicy: IfNotPresent
      command: ["sleep", "86400"]
      env:
        - name: HOME
          value: /tmp
        - name: OS_CLIENT_CONFIG_FILE
          value: /etc/openstack/clouds.yaml
      resources:
        requests:
          cpu: 500m
          memory: 256Mi
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: ["ALL"]
      volumeMounts:
        - name: work
          mountPath: /work
        - name: tmp
          mountPath: /tmp
        - name: scenarios
          mountPath: /scenarios
          readOnly: true
        - name: clouds
          mountPath: /etc/openstack
          readOnly: true
  volumes:
    - name: work
      emptyDir: {}
    - name: tmp
      emptyDir: {}
    - name: scenarios
      configMap:
        name: ${AB_SCENARIOS_CONFIGMAP}
    - name: clouds
      secret:
        secretName: ${AB_CLOUDS_SECRET}
        defaultMode: 0440
EOF
}

# ---------------------------------------------------------------------------
# ab_render_clouds <password> — Print the clouds.yaml with one cloud per side,
# each the side's Keystone Service. The bootstrap registers that URL for
# every interface, so the catalog keeps the client inside the cluster.
# ---------------------------------------------------------------------------
ab_render_clouds() {
  local escaped side
  escaped="$(printf '%s' "$1" | sed "s/'/''/g")"
  echo "clouds:"
  for side in ${AB_SIDES}; do
    cat <<EOF
  ${side}:
    auth:
      auth_url: http://${AB_PREFIX}-${side}.${AB_NAMESPACE}.svc:5000/v3
      username: admin
      password: '${escaped}'
      project_name: admin
      user_domain_name: Default
      project_domain_name: Default
    identity_api_version: 3
    interface: internal
EOF
  done
}

# ---------------------------------------------------------------------------
# ab_cleanup — Delete everything the run created. The databases stay on
# openstack-db; a later run's Keystone migrates and bootstraps them again.
# ---------------------------------------------------------------------------
ab_cleanup() {
  log "Deleting the client pod, the Keystones and their Memcacheds..."
  kubectl delete pod "${AB_CLIENT_POD}" -n "${AB_NAMESPACE}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete secret "${AB_CLOUDS_SECRET}" -n "${AB_NAMESPACE}" --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete configmap "${AB_SCENARIOS_CONFIGMAP}" -n "${AB_NAMESPACE}" --ignore-not-found >/dev/null 2>&1 || true
  local side
  for side in ${AB_SIDES}; do
    kubectl delete keystones.keystone.openstack.c5c3.io "${AB_PREFIX}-${side}" -n "${AB_NAMESPACE}" \
      --ignore-not-found --timeout="${AB_DELETE_TIMEOUT}" >/dev/null 2>&1 ||
      log "WARNING: Keystone ${AB_NAMESPACE}/${AB_PREFIX}-${side} was not deleted within ${AB_DELETE_TIMEOUT}."
    kubectl delete memcacheds.memcached.c5c3.io "${AB_PREFIX}-${side}-cache" -n "${AB_NAMESPACE}" \
      --ignore-not-found --timeout="${AB_DELETE_TIMEOUT}" >/dev/null 2>&1 || true
  done
}

# ab_on_exit — The EXIT trap: clean up unless asked to keep.
ab_on_exit() {
  local rc=$?
  if [[ "${KEYSTONE_PRESSURE_KEEP}" == "true" ]]; then
    log "KEYSTONE_PRESSURE_KEEP=true: leaving the Keystones and ${AB_NAMESPACE}/${AB_CLIENT_POD}; remove them with hack/keystone-pressure-ab.sh cleanup."
  else
    ab_cleanup
  fi
  exit "${rc}"
}

# ---------------------------------------------------------------------------
# ab_deploy — Apply both sides, the client's Secret and ConfigMap, and the
# client pod, and wait for all of them.
# ---------------------------------------------------------------------------
ab_deploy() {
  local side
  for side in ${AB_SIDES}; do
    log "Deploying ${AB_NAMESPACE}/${AB_PREFIX}-${side} on ${AB_KEYSTONE_IMAGE_REPO}:$(ab_image_tag "${side}")..."
    ab_render_side "${side}" | kubectl apply -f - >/dev/null ||
      ab_fail "cannot apply the Keystone and Memcached of ${side} (kubectl's error is above)"
  done

  local encoded password
  encoded="$(kubectl get secret keystone-admin -n "${AB_NAMESPACE}" -o jsonpath='{.data.password}')"
  password="$(printf '%s' "${encoded}" | base64 -d)"
  [[ -n "${password}" ]] || ab_fail "the Secret ${AB_NAMESPACE}/keystone-admin has an empty password."

  local clouds
  clouds="$(mktemp "${TMPDIR:-/tmp}/keystone-pressure-clouds.XXXXXX")"
  (umask 077 && ab_render_clouds "${password}" >"${clouds}")
  if ! kubectl create secret generic "${AB_CLOUDS_SECRET}" -n "${AB_NAMESPACE}" \
      --from-file=clouds.yaml="${clouds}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null; then
    rm -f "${clouds}"
    ab_fail "cannot apply the Secret ${AB_NAMESPACE}/${AB_CLOUDS_SECRET} (kubectl's error is above)"
  fi
  rm -f "${clouds}"
  kubectl create configmap "${AB_SCENARIOS_CONFIGMAP}" -n "${AB_NAMESPACE}" \
    --from-file="${PRESSURE_PROFILE_DIR}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null ||
    ab_fail "cannot apply the ConfigMap ${AB_NAMESPACE}/${AB_SCENARIOS_CONFIGMAP} (kubectl's error is above)"

  for side in ${AB_SIDES}; do
    kubectl wait memcacheds.memcached.c5c3.io "${AB_PREFIX}-${side}-cache" -n "${AB_NAMESPACE}" \
      --for=condition=Ready --timeout="${AB_CACHE_TIMEOUT}" >/dev/null ||
      ab_fail "Memcached ${AB_NAMESPACE}/${AB_PREFIX}-${side}-cache is not Ready after ${AB_CACHE_TIMEOUT}"
  done
  for side in ${AB_SIDES}; do
    log "Waiting up to ${AB_READY_TIMEOUT} for ${AB_NAMESPACE}/${AB_PREFIX}-${side} to be Ready..."
    if ! kubectl wait keystones.keystone.openstack.c5c3.io "${AB_PREFIX}-${side}" -n "${AB_NAMESPACE}" \
        --for=condition=Ready --timeout="${AB_READY_TIMEOUT}" >/dev/null; then
      kubectl get keystones.keystone.openstack.c5c3.io "${AB_PREFIX}-${side}" -n "${AB_NAMESPACE}" \
        -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}' >&2 || true
      ab_fail "Keystone ${AB_NAMESPACE}/${AB_PREFIX}-${side} is not Ready after ${AB_READY_TIMEOUT}"
    fi
  done

  kubectl delete pod "${AB_CLIENT_POD}" -n "${AB_NAMESPACE}" --ignore-not-found --wait=true >/dev/null
  ab_render_client | kubectl apply -f - >/dev/null ||
    ab_fail "cannot create the client pod ${AB_NAMESPACE}/${AB_CLIENT_POD} (kubectl's error is above)"
  kubectl wait pod "${AB_CLIENT_POD}" -n "${AB_NAMESPACE}" --for=condition=Ready --timeout="${AB_POD_TIMEOUT}" >/dev/null ||
    ab_fail "the client pod ${AB_NAMESPACE}/${AB_CLIENT_POD} is not Ready after ${AB_POD_TIMEOUT}; kubectl describe pod names the cause"
}

# ab_exec <args...> — Run a command in the client pod.
ab_exec() {
  kubectl exec -n "${AB_NAMESPACE}" "${AB_CLIENT_POD}" -- "$@"
}

# ---------------------------------------------------------------------------
# ab_run <profile> <side> — Run the profile against the side's Keystone in
# /work/<profile>/<side>, where dizzy writes its run record. Returns dizzy's
# exit code.
# ---------------------------------------------------------------------------
ab_run() {
  local profile="$1" side="$2"
  local args=(keystone pressure --os-cloud "${side}" --scenario "/scenarios/${profile}.yaml")
  local duration
  duration="$(ab_duration "${profile}")"
  if [[ -n "${duration}" ]]; then
    args+=(--duration "${duration}")
  fi
  local extra_args=()
  read -r -a extra_args <<<"${KEYSTONE_PRESSURE_ARGS}"
  log "Running ${profile} against ${side}..."
  # The run directory is passed as $1 of the inline script, so no value is
  # spliced into the shell text.
  # shellcheck disable=SC2016 # the pod's sh expands $1 and $@
  ab_exec sh -c 'rm -rf "$1" && mkdir -p "$1" && cd "$1" && shift && exec dizzy "$@"' \
    sh "/work/${profile}/${side}" "${args[@]}" ${extra_args[@]+"${extra_args[@]}"}
}

# ab_compare <profile> — Compare the profile's base and patched records in
# text, HTML and JSON next to them.
ab_compare() {
  local profile="$1"
  log "Comparing ${profile}: base against patched..."
  # shellcheck disable=SC2016 # the pod's sh expands $1 and the globs
  ab_exec sh -c 'cd "$1" &&
    dizzy keystone report --run base/run-*.json --compare patched/run-*.json >comparison.txt &&
    dizzy keystone report --run base/run-*.json --compare patched/run-*.json --format html >comparison.html &&
    dizzy keystone report --run base/run-*.json --compare patched/run-*.json --format json >comparison.json' \
    sh "/work/${profile}"
}

# ---------------------------------------------------------------------------
# ab_write_summary <results> — Write summary.md from the results, one
# "<profile> <base rc> <patched rc> <compare rc>" line per profile, and the
# comparisons copied out of the pod.
# ---------------------------------------------------------------------------
ab_write_summary() {
  local results="$1" summary="${AB_OUTPUT_DIR}/summary.md"
  {
    echo "## Keystone pressure ${KEYSTONE_PRESSURE_RELEASE}: base against patched"
    echo
    echo "- base: \`${KEYSTONE_PRESSURE_BASE_IMAGE}\`"
    echo "- patched: \`${KEYSTONE_PRESSURE_PATCHED_IMAGE}\`"
    echo "- dizzy ${DIZZY_VERSION}, ${KEYSTONE_PRESSURE_REPLICAS} Keystone replica(s) per build, duration ${KEYSTONE_PRESSURE_DURATION:-of each profile}${KEYSTONE_PRESSURE_PROFILE_DURATIONS:+ (${KEYSTONE_PRESSURE_PROFILE_DURATIONS})}"
    if [[ "${AB_A_A}" == "true" ]]; then
      echo "- both images are the same build: an A/A run, whose deltas are the noise of the stack"
    fi
    echo
    echo "Every delta is patched minus base: a negative latency delta and a positive ops/s delta mean the patched build is faster."
    echo
    local profile base_rc patched_rc compare_rc
    while read -r profile base_rc patched_rc compare_rc; do
      [[ -n "${profile}" ]] || continue
      echo "### ${profile}"
      echo
      if [[ "${base_rc}" != "0" || "${patched_rc}" != "0" ]]; then
        echo "The run failed (dizzy exit code base ${base_rc}, patched ${patched_rc}); see the job log."
      elif [[ "${compare_rc}" != "0" ]]; then
        echo "The comparison failed (exit code ${compare_rc}); see the job log."
      else
        echo '```text'
        cat "${AB_OUTPUT_DIR}/${profile}/comparison.txt"
        echo '```'
      fi
      echo
    done <<<"${results}"
  } >"${summary}"
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    cat "${summary}" >>"${GITHUB_STEP_SUMMARY}"
  fi
  log "Summary written to ${summary}."
}

# ---------------------------------------------------------------------------
# ab_main
# ---------------------------------------------------------------------------
ab_main() {
  case "${1:-}" in
    "") ;;
    cleanup)
      ab_cleanup
      return 0
      ;;
    *)
      ab_usage
      exit 1
      ;;
  esac

  ab_preflight
  AB_OUTPUT_DIR="${KEYSTONE_PRESSURE_OUTPUT:-${REPO_ROOT}/_output/keystone-pressure/${KEYSTONE_PRESSURE_RELEASE}-$(date -u '+%Y%m%dT%H%M%SZ')}"
  mkdir -p "${AB_OUTPUT_DIR}"

  if [[ "${KEYSTONE_PRESSURE_LOAD_IMAGES}" == "true" ]]; then
    ab_load_images
  fi

  trap ab_on_exit EXIT
  # A run that was kept or killed may have left the objects behind; they
  # start over from a fresh Keystone process and an empty Memcached.
  ab_cleanup
  ab_deploy

  local results="" failed=0 index=0 profile side order base_rc patched_rc compare_rc rc
  for profile in ${KEYSTONE_PRESSURE_PROFILES}; do
    order="base patched"
    if ((index % 2 == 1)); then
      order="patched base"
    fi
    index=$((index + 1))
    base_rc=0
    patched_rc=0
    for side in ${order}; do
      rc=0
      ab_run "${profile}" "${side}" || rc=$?
      if [[ "${side}" == "base" ]]; then base_rc="${rc}"; else patched_rc="${rc}"; fi
    done
    compare_rc="-"
    if [[ "${base_rc}" == "0" && "${patched_rc}" == "0" ]]; then
      compare_rc=0
      ab_compare "${profile}" || compare_rc=$?
    fi
    if [[ "${base_rc}" != "0" || "${patched_rc}" != "0" || "${compare_rc}" != "0" ]]; then
      failed=1
    fi
    results="${results}${profile} ${base_rc} ${patched_rc} ${compare_rc}"$'\n'
  done

  log "Copying the run records and comparisons to ${AB_OUTPUT_DIR}..."
  kubectl cp "${AB_NAMESPACE}/${AB_CLIENT_POD}:/work" "${AB_OUTPUT_DIR}" >/dev/null ||
    ab_fail "cannot copy /work out of ${AB_NAMESPACE}/${AB_CLIENT_POD}"
  ab_write_summary "${results}"

  if ((failed)); then
    log "ERROR: at least one run or comparison failed; see ${AB_OUTPUT_DIR}/summary.md."
    return 1
  fi
  log "Done: ${AB_OUTPUT_DIR}/summary.md."
}

# Run ab_main only when executed directly so unit tests (tests/unit/hack/) can
# source this script and exercise individual functions.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  ab_main "$@"
fi
