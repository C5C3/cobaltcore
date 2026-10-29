#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/lab-chaos.sh — Run the chaos sessions of the metal-stack lab (#1395):
# the proving run of Chaos Mesh, the fault runs F1 to F5 under probe servers of
# the runner's own, the random chaos run under the six-hour dizzy soak, and the
# record of the session. It replaces the paste-a-section blocks of the Proving
# run and the Lab fault runs of
# docs/reference/infrastructure/infrastructure-manifests.md and is started by
# hand, after the go-ahead on the shared lab (D4 of #1386).
#
# Subcommands, wrapped by the make targets lab-chaos-<subcommand>:
#   preflight       Read-only checks of the lab, one OK: or FAIL: line each.
#   proving         Steps 1 to 5 of the proving run; proving.tsv.
#   faults          Boot lab-chaos-0 and lab-chaos-1 with the guest probes,
#                   attach lab-chaos-nfs, run F1 to F5 and the volume checks;
#                   faults.tsv.
#   random          hack/chaos-random.sh run on lab for LAB_CHAOS_RANDOM_DURATION.
#   cleanup         Delete what faults created; a second call deletes nothing.
#   teardown-check  Step 6 of the proving run: a delay left active, then
#                   EXTERNAL_CLUSTER=true make teardown-infra. Destructive: it
#                   ends the session.
#   record          Write record.md of the run directory.
#
# Settings (environment):
#   LAB_CHAOS_CONTEXT          the kube context of the lab; required by every
#                              subcommand but record. A kind- context is refused.
#   LAB_CHAOS_GO_AHEAD         the lab's shoot name, .data.shootName of the
#                              ConfigMap kube-system/shoot-info; required by
#                              every subcommand but preflight and record.
#   LAB_CHAOS_RUN              the run id (default: the UTC start of the first
#                              changing subcommand, kept in
#                              _output/lab-chaos/current).
#   LAB_CHAOS_RANDOM_DURATION  the window of the random run (default 6h).
#
# Every subcommand that reads the lab pins the context: it writes `kubectl
# config view --raw --minify --flatten --context "$LAB_CHAOS_CONTEXT"` to a
# temporary file of mode 0600 and exports KUBECONFIG to it. OpenStack calls
# use _output/dizzy/clouds.yaml (cloud devstack-c5c3), which generate_clouds_yaml
# of hack/dizzy.sh writes under EXTERNAL_CLUSTER=true with the Gateway CA,
# through the port-forward of Part 1, Step 6 of the metal-stack quick start.
#
# Exit codes: 0 on success, 1 on a failed check, step or fault, 2 on a usage
# error.

set -euo pipefail

LC_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LC_REPO_ROOT="$(cd "${LC_SCRIPT_DIR}/.." && pwd)"

# Settings.
LAB_CHAOS_CONTEXT="${LAB_CHAOS_CONTEXT:-}"
LAB_CHAOS_GO_AHEAD="${LAB_CHAOS_GO_AHEAD:-}"
LAB_CHAOS_RUN="${LAB_CHAOS_RUN:-}"
LAB_CHAOS_RANDOM_DURATION="${LAB_CHAOS_RANDOM_DURATION:-6h}"

# The namespace, the names and the waits in seconds. Plain variables, so the
# unit test can point them elsewhere or set a wait to 0 after sourcing.
NAMESPACE="openstack"
LC_OUTPUT_DIR="${LC_REPO_ROOT}/_output/lab-chaos"
LC_CLOUDS="${LC_REPO_ROOT}/_output/dizzy/clouds.yaml"
LC_CLOUD="devstack-c5c3"
LC_KEYSTONE_URL="https://keystone.127-0-0-1.nip.io:8443/v3"
LC_MODULES_DAEMONSET="${LC_REPO_ROOT}/deploy/lab/metal-stack/chaos-mesh/modules-daemonset.yaml"
LC_CHAOS_RANDOM="${LC_REPO_ROOT}/hack/chaos-random.sh"
LC_CHAOS_RANDOM_OUTPUT="${LC_REPO_ROOT}/_output/chaos-random"
LC_IMAGE="cirros-kvm"
LC_FLAVOR="hvo-smoke-test"
LC_NET="lab-chaos-net"
LC_SUBNET="lab-chaos-subnet"
LC_CIDR="10.239.0.0/24"
LC_GROUP="lab-chaos"
LC_PROBE="lab-chaos-0"
LC_PEER="lab-chaos-1"
LC_VOLUME="lab-chaos-nfs"
LC_VOLUME_AFTER="lab-chaos-vol-after"
LC_LABEL="app.kubernetes.io/part-of=lab-chaos"
LC_POLL=5
LC_WAIT_PROBES=300
LC_WAIT_INJECTED=60
LC_WAIT_REPLACED=300
LC_WAIT_READY=600
LC_WAIT_PODS=300
LC_WAIT_VOLUME=300
LC_HOLD_SCALE=300
LC_PROBE_WINDOW=20

# Set by lc_kubeconfig, lc_read_shoot and lc_run_dir.
LC_KUBECONFIG=""
LC_SHOOT=""
LC_DIR=""
LC_HOST=""
LC_DOMAIN=""

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
lc_usage() {
  cat >&2 <<EOF
Usage: hack/lab-chaos.sh <subcommand>

Subcommands:
  preflight        Read-only checks of the lab.
  proving          Steps 1 to 5 of the proving run.
  faults           The fault runs F1 to F5 under lab-chaos-0 and lab-chaos-1.
  random           The random chaos run under the dizzy soak.
  cleanup          Delete what faults created (idempotent).
  teardown-check   Proving step 6, then make teardown-infra (ends the session).
  record           Write record.md of the run.

Settings (environment):
  LAB_CHAOS_CONTEXT          kube context of the lab (required but by record).
  LAB_CHAOS_GO_AHEAD         the lab's shoot name (required but by preflight and record).
  LAB_CHAOS_RUN              run id (default: kept in _output/lab-chaos/current).
  LAB_CHAOS_RANDOM_DURATION  window of the random run (default 6h).
EOF
  exit 2
}

# lc_log <message> — A progress line with the UTC time.
lc_log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

# lc_fail <message> — An ERROR line, then exit 1.
lc_fail() {
  echo "ERROR: $*"
  exit 1
}

# lc_check_fail <message> — A FAIL line of a check, then exit 1.
lc_check_fail() {
  echo "FAIL: $*"
  exit 1
}

# lc_first_line <text> — The first line of the text.
lc_first_line() {
  printf '%s\n' "${1%%$'\n'*}"
}

# lc_timeout <seconds> <command...> — The command under timeout(1) when the
# workstation has it; macOS has none by default.
lc_timeout() {
  local seconds="$1"
  shift
  if command -v timeout >/dev/null 2>&1; then
    timeout "$seconds" "$@"
  else
    "$@"
  fi
}

# os <arguments...> — The OpenStack CLI against the lab's clouds.yaml.
os() {
  OS_CLIENT_CONFIG_FILE="$LC_CLOUDS" OS_CLOUD="$LC_CLOUD" openstack "$@"
}

# lc_check_settings [record] — The usage errors of the settings, before any
# call. A missing context is a FAIL: line.
lc_check_settings() {
  if [[ "${1:-}" != record && -z "$LAB_CHAOS_CONTEXT" ]]; then
    echo "FAIL: LAB_CHAOS_CONTEXT is not set; name the kube context of the lab"
    exit 2
  fi
  if [[ -n "$LAB_CHAOS_RUN" && ! "$LAB_CHAOS_RUN" =~ ^[0-9]{8}T[0-9]{6}Z$ ]]; then
    echo "FAIL: LAB_CHAOS_RUN must be a run id such as 20261030T080000Z, not '${LAB_CHAOS_RUN}'"
    exit 2
  fi
  if [[ ! "$LAB_CHAOS_RANDOM_DURATION" =~ ^([0-9]+h)?([0-9]+m)?([0-9]+s)?$ || "$LAB_CHAOS_RANDOM_DURATION" != *[1-9]* ]]; then
    echo "FAIL: LAB_CHAOS_RANDOM_DURATION must be a duration such as 6h, not '${LAB_CHAOS_RANDOM_DURATION}'"
    exit 2
  fi
}

# lc_require <command...> — Each command is on PATH, or a FAIL line.
lc_require() {
  local cmd missing=""
  for cmd in "$@"; do
    command -v "$cmd" >/dev/null 2>&1 || missing="${missing} ${cmd}"
  done
  if [[ -n "$missing" ]]; then
    lc_check_fail "not on PATH:${missing}"
  fi
  echo "OK: $* on PATH"
}

# lc_kubeconfig — Pin the context: refuse a kind context before any call,
# write the context's minified kubeconfig to a mode-0600 temporary file, and
# export KUBECONFIG to it.
lc_kubeconfig() {
  local out
  if [[ "$LAB_CHAOS_CONTEXT" == kind-* ]]; then
    lc_check_fail "context ${LAB_CHAOS_CONTEXT} is a kind cluster; the lab chaos runs are for the lab only"
  fi
  LC_KUBECONFIG="$(mktemp "${TMPDIR:-/tmp}/lab-chaos-kubeconfig.XXXXXX")"
  chmod 600 "$LC_KUBECONFIG"
  trap 'rm -f "${LC_KUBECONFIG:-}"' EXIT
  if ! out="$(kubectl config view --raw --minify --flatten --context "$LAB_CHAOS_CONTEXT" 2>&1 >"$LC_KUBECONFIG")"; then
    lc_check_fail "context ${LAB_CHAOS_CONTEXT} is not in the kubeconfig: $(lc_first_line "$out")"
  fi
  export KUBECONFIG="$LC_KUBECONFIG"
  echo "OK: context ${LAB_CHAOS_CONTEXT}"
}

# lc_read_shoot — Set LC_SHOOT to .data.shootName of kube-system/shoot-info.
lc_read_shoot() {
  local out
  if ! out="$(kubectl get configmap shoot-info -n kube-system -o jsonpath='{.data.shootName}' 2>&1)"; then
    if [[ "$out" == *"(NotFound)"* ]]; then
      lc_check_fail "not a Gardener shoot: no ConfigMap kube-system/shoot-info"
    fi
    lc_check_fail "cannot read the ConfigMap kube-system/shoot-info: $(lc_first_line "$out")"
  fi
  [[ -n "$out" ]] || lc_check_fail "the ConfigMap kube-system/shoot-info names no shootName"
  LC_SHOOT="$out"
  echo "OK: Gardener shoot ${LC_SHOOT}"
}

# lc_go_ahead — LAB_CHAOS_GO_AHEAD names the shoot of the context.
lc_go_ahead() {
  [[ -n "$LC_SHOOT" ]] || lc_read_shoot
  if [[ "$LAB_CHAOS_GO_AHEAD" != "$LC_SHOOT" ]]; then
    lc_fail "no go-ahead for shoot '${LC_SHOOT}': LAB_CHAOS_GO_AHEAD is '${LAB_CHAOS_GO_AHEAD}'; set it to ${LC_SHOOT} once the shared lab is yours"
  fi
  echo "OK: go-ahead for shoot ${LC_SHOOT}"
}

# lc_clouds — Write _output/dizzy/clouds.yaml with hack/dizzy.sh, as the
# soak's workstation side does.
lc_clouds() {
  (
    export EXTERNAL_CLUSTER=true
    # shellcheck source=hack/dizzy.sh
    source "${LC_REPO_ROOT}/hack/dizzy.sh"
    generate_clouds_yaml
  ) >/dev/null
}

# lc_run_dir — Set LC_DIR to the run's directory: LAB_CHAOS_RUN, the one
# _output/lab-chaos/current names, or a new one at the UTC time.
lc_run_dir() {
  local run="$LAB_CHAOS_RUN"
  if [[ -z "$run" && -f "${LC_OUTPUT_DIR}/current" ]]; then
    run="$(cat "${LC_OUTPUT_DIR}/current")"
  fi
  if [[ -z "$run" ]]; then
    run="$(date -u '+%Y%m%dT%H%M%SZ')"
  fi
  LC_DIR="${LC_OUTPUT_DIR}/${run}"
  mkdir -p "$LC_DIR"
  echo "$run" >"${LC_OUTPUT_DIR}/current"
  lc_log "run ${run}: ${LC_DIR}"
}

# lc_open <subcommand> — The settings, the context, the shoot, the go-ahead,
# the clouds.yaml and the run directory of a changing subcommand.
lc_open() {
  lc_check_settings "$1"
  lc_require kubectl jq yq python3 openstack
  lc_kubeconfig
  lc_read_shoot
  lc_go_ahead
  lc_clouds || lc_fail "cannot write ${LC_CLOUDS} (hack/dizzy.sh's error is above)"
  lc_run_dir
  echo "$LC_SHOOT" >"${LC_DIR}/shoot"
}

# ---------------------------------------------------------------------------
# preflight
# ---------------------------------------------------------------------------
lc_preflight() {
  local out nodes loaded computes
  lc_check_settings preflight
  lc_require kubectl jq yq python3 openstack
  lc_kubeconfig
  lc_read_shoot
  if ! lc_clouds; then
    lc_check_fail "cannot write ${LC_CLOUDS}: the ControlPlane's admin Secret or the Gateway CA is missing (hack/dizzy.sh's error is above)"
  fi
  if ! out="$(os token issue -f value -c id 2>&1)"; then
    lc_check_fail "openstack token issue failed through the port-forward ($(lc_first_line "$out")); start the port-forward of Part 1, Step 6"
  fi
  echo "OK: openstack token issue through the port-forward"
  if ! out="$(kubectl get helmrelease chaos-mesh -n chaos-mesh -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}' 2>&1)" ||
    [[ "$out" != True ]]; then
    lc_check_fail "HelmRelease chaos-mesh/chaos-mesh is not Ready (read '$(lc_first_line "$out")')"
  fi
  echo "OK: HelmRelease chaos-mesh/chaos-mesh is Ready"
  if ! out="$(kubectl rollout status daemonset/chaos-daemon -n chaos-mesh --timeout=120s 2>&1)"; then
    lc_check_fail "DaemonSet chaos-mesh/chaos-daemon is not rolled out: $(lc_first_line "$out")"
  fi
  echo "OK: DaemonSet chaos-mesh/chaos-daemon is rolled out"
  nodes="$(kubectl get nodes -o name | grep -c . || true)"
  loaded="$(kubectl logs -n chaos-mesh -l app.kubernetes.io/name=chaos-mesh-modules -c load --prefix --tail=-1 2>/dev/null |
    grep -c 'are loaded$' || true)"
  if [[ "$loaded" != "$nodes" ]]; then
    lc_check_fail "chaos-mesh-modules printed ${loaded} 'are loaded' lines for ${nodes} nodes"
  fi
  echo "OK: one 'are loaded' line per node (${nodes})"
  if ! out="$(kubectl get namespace dizzy -o name 2>&1)"; then
    lc_check_fail "namespace dizzy is missing: $(lc_first_line "$out")"
  fi
  echo "OK: namespace dizzy"
  if ! out="$(kubectl get controlplane controlplane -n "$NAMESPACE" -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}' 2>&1)" ||
    [[ "$out" != True ]]; then
    lc_check_fail "ControlPlane ${NAMESPACE}/controlplane is not Ready=True (read '$(lc_first_line "$out")')"
  fi
  echo "OK: ControlPlane ${NAMESPACE}/controlplane is Ready=True"
  if ! out="$(kubectl get images.openstack.k-orc.cloud/hvo-cirros-kvm flavors.openstack.k-orc.cloud/hvo-smoke-test-flavor \
      -n "$NAMESPACE" -o json 2>&1)" ||
    [[ "$(jq '[.items[] | select(any(.status.conditions[]?; .type == "Available" and .status == "True"))] | length' <<<"$out" 2>/dev/null)" != 2 ]]; then
    lc_check_fail "image hvo-cirros-kvm and flavor hvo-smoke-test-flavor are missing or not Available; apply deploy/lab/metal-stack/hypervisor-fixtures"
  fi
  echo "OK: image hvo-cirros-kvm and flavor hvo-smoke-test-flavor are Available"
  if ! out="$(kubectl get cinderbackend nfs1 -n "$NAMESPACE" -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}' 2>&1)" ||
    [[ "$out" != True ]]; then
    lc_check_fail "the Cinder backend nfs1 is missing or not Ready (read '$(lc_first_line "$out")'); apply deploy/lab/metal-stack/controlplane-nfs as in Part 3, Step 1"
  fi
  echo "OK: the Cinder backend nfs1 is Ready"
  if ! out="$(os volume type show nfs -f value -c name 2>&1)"; then
    lc_check_fail "volume type nfs is missing: $(lc_first_line "$out"); create it as in Part 3, Step 2"
  fi
  echo "OK: volume type nfs"
  if ! computes="$(os compute service list --service nova-compute -c State -f value 2>&1)"; then
    lc_check_fail "cannot list the nova-compute services: $(lc_first_line "$computes")"
  fi
  if [[ "$(grep -cx up <<<"$computes" || true)" -lt 2 ]]; then
    lc_check_fail "fewer than two nova-compute services are up; the fault runs need a peer on another node"
  fi
  echo "OK: $(grep -cx up <<<"$computes") nova-compute services are up"
  if ! out="$(os server list --all-projects --name '^lab-chaos-' -f value -c Name 2>&1)"; then
    lc_check_fail "cannot list the servers: $(lc_first_line "$out")"
  fi
  if [[ -n "$out" ]]; then
    lc_check_fail "servers of a previous session exist: $(paste -sd ' ' - <<<"$out"); run make lab-chaos-cleanup"
  fi
  echo "OK: no server named lab-chaos-*"
}

# ---------------------------------------------------------------------------
# proving: steps 1 to 5 of the proving run
# ---------------------------------------------------------------------------

# lc_times <file> — Five request times through the port-forward, one per line.
lc_times() {
  local _
  for _ in 1 2 3 4 5; do
    curl -sk -o /dev/null -w '%{time_total}\n' "$LC_KEYSTONE_URL"
  done | tee "$1"
}

# lc_proving_row <step> <pass|FAIL> <detail> — A row of proving.tsv.
lc_proving_row() {
  printf '%s\t%s\t%s\n' "$1" "$2" "$3" >>"${LC_DIR}/proving.tsv"
  lc_log "proving step $1: $2: $3"
}

# lc_chaos <file> — Apply a chaos object and label it as the runner's.
lc_chaos() {
  kubectl apply -f "$1" >/dev/null
}

lc_cmd_proving() {
  local d failed=0 chart loaded nodes largest sel before after rc
  lc_open proving
  d="${LC_DIR}/proving"
  mkdir -p "$d"
  : >"${LC_DIR}/proving.tsv"

  # 1. State.
  {
    chart="$(kubectl get helmrelease chaos-mesh -n chaos-mesh -o jsonpath='{.status.history[0].chartVersion}')"
    echo "chart ${chart}"
    kubectl rollout status daemonset/chaos-daemon -n chaos-mesh --timeout=120s
    kubectl logs -n chaos-mesh -l app.kubernetes.io/name=chaos-mesh-modules -c load --prefix --tail=-1
    kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name} {.status.nodeInfo.kubeletVersion} {.status.nodeInfo.osImage}{"\n"}{end}'
  } >"${d}/step1.log" 2>&1 || true
  nodes="$(kubectl get nodes -o name | grep -c . || true)"
  loaded="$(grep -c 'are loaded$' "${d}/step1.log" || true)"
  if [[ -n "${chart:-}" && "$loaded" == "$nodes" ]] && grep -q 'successfully rolled out' "${d}/step1.log"; then
    lc_proving_row 1 pass "chart ${chart}; chaos-daemon rolled out; ${loaded} 'are loaded' lines for ${nodes} nodes"
  else
    lc_proving_row 1 FAIL "chart '${chart:-}'; ${loaded} 'are loaded' lines for ${nodes} nodes; see proving/step1.log"
    failed=1
  fi

  # 2. Baseline.
  lc_times "${d}/step2.times" >/dev/null
  largest="$(sort -n "${d}/step2.times" | tail -n 1)"
  if [[ "$(grep -c . "${d}/step2.times")" == 5 ]]; then
    lc_proving_row 2 pass "baseline $(paste -sd ',' "${d}/step2.times" | sed 's/,/, /g') s; largest ${largest} s"
  else
    lc_proving_row 2 FAIL "five requests did not answer; see proving/step2.times"
    failed=1
  fi

  # 3. PodChaos: a Keystone API pod is killed and replaced.
  sel='app.kubernetes.io/name=keystone,app.kubernetes.io/component=api'
  before="$(kubectl get pods -n "$NAMESPACE" -l "$sel" -o jsonpath='{.items[*].metadata.uid}')"
  cat >"${d}/kill.yaml" <<EOF
apiVersion: chaos-mesh.org/v1alpha1
kind: PodChaos
metadata:
  name: lab-keystone-kill
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/part-of: lab-chaos
spec:
  action: pod-kill
  mode: one
  selector:
    namespaces: [${NAMESPACE}]
    labelSelectors:
      app.kubernetes.io/name: keystone
      app.kubernetes.io/component: api
EOF
  rc=0
  {
    lc_chaos "${d}/kill.yaml" &&
      kubectl wait podchaos/lab-keystone-kill -n "$NAMESPACE" --for=condition=AllInjected --timeout="${LC_WAIT_INJECTED}s" &&
      lc_until "$LC_WAIT_REPLACED" "a Keystone API pod with a new UID" \
        bash -c "[ \"\$(kubectl get pods -n $NAMESPACE -l '$sel' -o jsonpath='{.items[*].metadata.uid}')\" != '$before' ]" &&
      kubectl wait pod -n "$NAMESPACE" -l "$sel" --for=condition=Ready --timeout="${LC_WAIT_PODS}s" &&
      kubectl delete podchaos lab-keystone-kill -n "$NAMESPACE" --wait --timeout=60s
  } >"${d}/step3.log" 2>&1 || rc=$?
  if [[ "$rc" == 0 ]]; then
    lc_proving_row 3 pass "AllInjected; a Keystone API pod with a new UID became Ready; the PodChaos was deleted within 60 s"
  else
    kubectl delete podchaos lab-keystone-kill -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
    lc_proving_row 3 FAIL "see proving/step3.log: $(tail -n 1 "${d}/step3.log")"
    failed=1
  fi

  # 4. NetworkChaos: 300 ms on every Keystone API pod, then released.
  lc_delay_yaml >"${d}/delay.yaml"
  rc=0
  {
    lc_chaos "${d}/delay.yaml" &&
      kubectl wait networkchaos/lab-keystone-delay -n "$NAMESPACE" --for=condition=AllInjected --timeout="${LC_WAIT_INJECTED}s"
  } >"${d}/step4.log" 2>&1 || rc=$?
  lc_times "${d}/step4.delayed" >/dev/null
  kubectl delete networkchaos lab-keystone-delay -n "$NAMESPACE" --wait --timeout=120s >>"${d}/step4.log" 2>&1 || rc=1
  lc_times "${d}/step4.released" >/dev/null
  local verdict
  verdict="$(awk -v l="$largest" 'FNR == 1 { f++ }
    f == 1 && $1 < l + 0.2 { bad = bad " delayed " $1 } f == 2 && $1 >= l + 0.1 { bad = bad " released " $1 }
    END { print (bad == "" ? "ok" : bad) }' "${d}/step4.delayed" "${d}/step4.released")"
  if [[ "$rc" == 0 && "$verdict" == ok ]]; then
    lc_proving_row 4 pass "delayed $(paste -sd ',' "${d}/step4.delayed" | sed 's/,/, /g') s, each at least 0.2 s above ${largest} s; released $(paste -sd ',' "${d}/step4.released" | sed 's/,/, /g') s, each less than 0.1 s above it"
  else
    lc_proving_row 4 FAIL "${verdict}; see proving/step4.*"
    failed=1
  fi

  # 5. Scope: a pod in a namespace without the annotation is not selected.
  lc_proving_scope "$d" || failed=1

  [[ "$failed" == 0 ]] || exit 1
}

# lc_delay_yaml — The NetworkChaos of steps 4 and 6.
lc_delay_yaml() {
  cat <<EOF
apiVersion: chaos-mesh.org/v1alpha1
kind: NetworkChaos
metadata:
  name: lab-keystone-delay
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/part-of: lab-chaos
spec:
  action: delay
  mode: all
  selector:
    namespaces: [${NAMESPACE}]
    labelSelectors:
      app.kubernetes.io/name: keystone
      app.kubernetes.io/component: api
  delay:
    latency: 300ms
  # The fault ends by itself when the run is abandoned.
  duration: 10m
EOF
}

# lc_proving_scope <dir> — Step 5: the probe pod of chaos-scope-probe keeps its
# UID and restart count, and the PodChaos selects no pod.
lc_proving_scope() {
  local d="$1" image uid after events rc=0
  image="$(yq -r '.spec.template.spec.containers[0].image' "$LC_MODULES_DAEMONSET")"
  {
    kubectl create namespace chaos-scope-probe
    kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: scope-probe
  namespace: chaos-scope-probe
  labels:
    app: scope-probe
spec:
  automountServiceAccountToken: false
  containers:
    - name: hold
      image: ${image}
      command: ["/bin/bash", "-c", "trap 'exit 0' TERM; sleep infinity & wait"]
      securityContext:
        runAsNonRoot: true
        runAsUser: 65534
        runAsGroup: 65534
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: ["ALL"]
      resources:
        requests: {cpu: 1m, memory: 8Mi}
        limits: {memory: 32Mi}
EOF
    kubectl wait pod/scope-probe -n chaos-scope-probe --for=condition=Ready --timeout=120s
  } >"${d}/step5.log" 2>&1 || rc=$?
  uid="$(kubectl get pod scope-probe -n chaos-scope-probe -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
  kubectl apply -f - >>"${d}/step5.log" 2>&1 <<EOF || rc=1
apiVersion: chaos-mesh.org/v1alpha1
kind: PodChaos
metadata:
  name: lab-scope-probe
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/part-of: lab-chaos
spec:
  action: pod-kill
  mode: one
  selector:
    namespaces: [chaos-scope-probe]
    labelSelectors:
      app: scope-probe
EOF
  lc_sleep 60
  after="$(kubectl get pod scope-probe -n chaos-scope-probe -o jsonpath='{.metadata.uid} restarts={.status.containerStatuses[0].restartCount}' 2>/dev/null || true)"
  events="$(kubectl get events -n "$NAMESPACE" --field-selector involvedObject.name=lab-scope-probe 2>&1 || true)"
  printf '%s\n%s\n' "$after" "$events" >>"${d}/step5.log"
  kubectl delete podchaos lab-scope-probe -n "$NAMESPACE" --wait --timeout=60s >>"${d}/step5.log" 2>&1 || rc=1
  kubectl delete namespace chaos-scope-probe --wait --timeout=120s >>"${d}/step5.log" 2>&1 || rc=1
  if [[ "$rc" == 0 && -n "$uid" && "$after" == "${uid} restarts=0" ]] && grep -q 'no pod is selected' <<<"$events"; then
    lc_proving_row 5 pass "the probe pod kept its UID with restarts=0; the PodChaos selected no pod"
  else
    lc_proving_row 5 FAIL "UID before '${uid}', after '${after}'; see proving/step5.log"
    return 1
  fi
}

# ---------------------------------------------------------------------------
# faults: the page's helpers, on lab-chaos-0
# ---------------------------------------------------------------------------

# lc_sleep <seconds>
lc_sleep() {
  sleep "$1"
}

# lc_until <seconds> <what> <command...> — Poll the command every LC_POLL
# seconds until it succeeds; a GATE FAILED line after the bound.
lc_until() {
  local end=$((SECONDS + $1)) what="$2"
  shift 2
  while [[ "$SECONDS" -lt "$end" ]]; do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    lc_sleep "$LC_POLL"
  done
  echo "GATE FAILED: ${what} not within $(($1)) s"
  return 1
}

# node_libvirt_pod — The libvirt pod of lab-chaos-0's node.
node_libvirt_pod() {
  kubectl get pod -n "$NAMESPACE" -l app.kubernetes.io/name=libvirt --field-selector "spec.nodeName=${LC_HOST}" -o name
}

# pids — The host PIDs of lab-chaos-0's QEMU and of libvirtd; [t] keeps pgrep
# from matching the sh that runs it.
pids() {
  kubectl exec -n "$NAMESPACE" "$(node_libvirt_pod)" -c libvirtd -- \
    sh -c "echo qemu=\$(pgrep -f 'gues[t]=${LC_DOMAIN},') libvirtd=\$(pgrep -x libvirtd)"
}

# field <command...> — One field: the output of the command, and its error
# output only when it fails, so a warning on stderr stays out of the field.
field() {
  local err
  { err=$("$@" 2>&1 1>&3 3>&-) || echo "${err}"; } 3>&1 | tr -s ' \n\r' '_'
}

# state — One line: what the APIs, the operators and libvirt report for
# lab-chaos-0 and its node.
state() {
  echo "server=$(field os server show "$LC_PROBE" -c status -f value)" \
    "domain=$(field lc_timeout 10 kubectl exec -n "$NAMESPACE" "$(node_libvirt_pod)" -c libvirtd -- virsh domstate --reason "${LC_DOMAIN}")" \
    "compute=$(field os compute service list --service nova-compute --host "${LC_HOST}" -c State -f value)" \
    "agents=$(field os network agent list --host "${LC_HOST}" -c Alive -f value)" \
    "volume=$(field os volume show "$LC_VOLUME" -c status -f value)" \
    "cinder-volume=$(field os volume service list --service cinder-volume --host controlplane-cinder@nfs1 -c State -f value)" \
    "ready=$(field kubectl get novacompute,ovnchassis,neutronmetadataagent,cinder,cinderbackend -n "$NAMESPACE" -o jsonpath='{range .items[*]}{.kind}:{.status.conditions[?(@.type=="Ready")].status},{end}')" \
    "hypervisor=$(field kubectl get hypervisor "${LC_HOST}" -o jsonpath='{.status.conditions[?(@.type=="LibVirtConnection")].status}')" \
    "nfs-server=$(field kubectl get pod -n "$NAMESPACE" -l app.kubernetes.io/name=nfs-server -o jsonpath='{range .items[*]}{.spec.nodeName}/{.status.phase}/{.status.containerStatuses[0].ready},{end}')"
}

# watch_state <file> — Append "<UTC time> <state>" whenever the state changes;
# runs until killed.
watch_state() {
  local previous="" current
  while :; do
    current=$(state)
    if [ "${current}" != "${previous}" ]; then
      echo "$(date -u +%T) ${current}" >>"$1"
      previous=${current}
    fi
    sleep 5
  done
}

# console_log — The last 120 lines of lab-chaos-0's console log, or a failure
# when the call fails or prints nothing; the serial console ends lines in \r\n.
console_log() {
  local log
  log=$(os console log show --lines 120 "$LC_PROBE" 2>/dev/null) && [ -n "${log}" ] || return 1
  tr -d '\r' <<<"${log}"
}

# last_probe <net|disk> — The newest line of the guest probe in the console
# log on stdin.
last_probe() {
  grep "lab-probe $1 " | tail -n 1
}

# probes_ok — True when both probes wrote a new "ok" line within
# LC_PROBE_WINDOW seconds.
probes_ok() {
  local before after
  before=$(console_log) || return 1
  lc_sleep "$LC_PROBE_WINDOW"
  after=$(console_log) || return 1
  [ "$(last_probe net <<<"${after}")" != "$(last_probe net <<<"${before}")" ] &&
    last_probe net <<<"${after}" | grep -q ' ok$' &&
    [ "$(last_probe disk <<<"${after}")" != "$(last_probe disk <<<"${before}")" ] &&
    last_probe disk <<<"${after}" | grep -q ' ok [0-9]*$'
}

# wait_replaced <selector> <uids> <node|any> — Up to LC_WAIT_REPLACED seconds
# for pods of the selector without the UIDs, on lab-chaos-0's node for node,
# then up to LC_WAIT_READY for every pod of the selector to be Ready.
wait_replaced() {
  local end=$((SECONDS + LC_WAIT_REPLACED)) uids
  while [ "${SECONDS}" -lt "${end}" ]; do
    if [ "$3" = node ]; then
      uids=$(kubectl get pod -n "$NAMESPACE" -l "$1" --field-selector "spec.nodeName=${LC_HOST}" -o jsonpath='{.items[*].metadata.uid}')
    else
      uids=$(kubectl get pod -n "$NAMESPACE" -l "$1" -o jsonpath='{.items[*].metadata.uid}')
    fi
    if [ -n "${uids}" ] && ! echo " ${uids} " | grep -q " $2 "; then
      kubectl wait pod -n "$NAMESPACE" -l "$1" --for=condition=Ready --timeout="${LC_WAIT_READY}s" >/dev/null && return 0
      break
    fi
    lc_sleep 2
  done
  echo "GATE FAILED: no Ready replacement for $1"
  return 1
}

# recovered <seconds> — The gate before the next fault: within the bound
# lab-chaos-0 is ACTIVE, nova-compute on its node is up, Neutron lists network
# agents of its node and every one is alive, and both probes write "ok".
recovered() {
  local end=$((SECONDS + $1)) agents
  while [ "${SECONDS}" -lt "${end}" ]; do
    if [ "$(os server show "$LC_PROBE" -c status -f value)" = ACTIVE ] &&
      [ "$(os compute service list --service nova-compute --host "${LC_HOST}" -c State -f value)" = up ] &&
      agents=$(os network agent list --host "${LC_HOST}" -c Alive -f value) &&
      [ -n "${agents}" ] && ! grep -qv True <<<"${agents}" &&
      probes_ok; then
      echo "recovered at $(date -u +%T)"
      return 0
    fi
    lc_sleep "$LC_POLL"
  done
  echo "GATE FAILED: ${LC_PROBE}, nova-compute, the agents of ${LC_HOST} or the guest probes did not recover in $1 s"
  return 1
}

# lc_wait_status <command...> <want> — Poll a status command until it prints
# want, within LC_WAIT_VOLUME seconds.
lc_wait_status() {
  local want="${*: -1}" end=$((SECONDS + LC_WAIT_VOLUME)) got
  set -- "${@:1:$(($# - 1))}"
  while [[ "$SECONDS" -lt "$end" ]]; do
    got="$("$@" 2>/dev/null || true)"
    [[ "$got" == "$want" ]] && return 0
    lc_sleep "$LC_POLL"
  done
  echo "not ${want} within ${LC_WAIT_VOLUME} s (read '${got:-}')"
  return 1
}

# lc_user_data <peer address> — The user-data of lab-chaos-0: wait for the
# volume, then the two probe loops of the page, writing to the serial console.
lc_user_data() {
  cat <<EOF
#!/bin/sh
# The guest probes of hack/lab-chaos.sh faults: one line every two seconds
# each, on the serial console Nova keeps as the console log.
i=0
while [ ! -b /dev/vdb ] && [ \$i -lt 300 ]; do sleep 1; i=\$((i+1)); done
(while :; do if ping -c 1 -W 1 $1 >/dev/null 2>&1; then r=ok; else r=lost; fi; echo "lab-probe net \$(date -u +%T) \$r"; sleep 2; done >/dev/ttyS0 2>&1) &
(i=0; while :; do i=\$((i+1)); if echo "probe-\$i" | dd of=/dev/vdb bs=512 seek=8 2>/dev/null && sync; then r=ok; else r=error; fi; echo "lab-probe disk \$(date -u +%T) \$r \$i"; sleep 2; done >/dev/ttyS0 2>&1) &
EOF
}

# lc_boot — The network, the server group, the two servers, the volume, and
# the wait for both probes.
lc_boot() {
  local d="$1" peer_ip end log
  lc_log "faults: creating ${LC_NET}, ${LC_GROUP}, ${LC_PEER}, ${LC_PROBE} and ${LC_VOLUME}"
  os network show "$LC_NET" >/dev/null 2>&1 || os network create "$LC_NET" >/dev/null
  os subnet show "$LC_SUBNET" >/dev/null 2>&1 ||
    os subnet create --network "$LC_NET" --subnet-range "$LC_CIDR" "$LC_SUBNET" >/dev/null
  os server group show "$LC_GROUP" >/dev/null 2>&1 || os server group create --policy anti-affinity "$LC_GROUP" >/dev/null
  local group
  group="$(os server group show "$LC_GROUP" -f value -c id)"
  os server create --image "$LC_IMAGE" --flavor "$LC_FLAVOR" --network "$LC_NET" --hint "group=${group}" "$LC_PEER" >/dev/null
  lc_wait_status os server show "$LC_PEER" -c status -f value ACTIVE || return 1
  peer_ip="$(os server show "$LC_PEER" -f json -c addresses | jq -r --arg n "$LC_NET" '.addresses[$n][0] // empty')"
  [[ -n "$peer_ip" ]] || { echo "${LC_PEER} has no address on ${LC_NET}"; return 1; }
  echo "$peer_ip" >"${d}/peer.address"
  lc_user_data "$peer_ip" >"${d}/user-data.sh"
  os server create --image "$LC_IMAGE" --flavor "$LC_FLAVOR" --network "$LC_NET" --hint "group=${group}" \
    --user-data "${d}/user-data.sh" "$LC_PROBE" >/dev/null
  lc_wait_status os server show "$LC_PROBE" -c status -f value ACTIVE || return 1
  os volume create --type nfs --size 1 "$LC_VOLUME" >/dev/null
  lc_wait_status os volume show "$LC_VOLUME" -c status -f value available || return 1
  os server add volume "$LC_PROBE" "$LC_VOLUME" >/dev/null
  lc_wait_status os volume show "$LC_VOLUME" -c status -f value in-use || return 1
  LC_HOST="$(os server show "$LC_PROBE" -c OS-EXT-SRV-ATTR:host -f value)"
  LC_DOMAIN="$(os server show "$LC_PROBE" -c OS-EXT-SRV-ATTR:instance_name -f value)"
  printf 'host %s\ndomain %s\npeer-host %s\n' "$LC_HOST" "$LC_DOMAIN" \
    "$(os server show "$LC_PEER" -c OS-EXT-SRV-ATTR:host -f value)" >"${d}/servers"
  # That cirros runs a #! user-data script as root at boot is the assumption
  # this wait proves.
  end=$((SECONDS + LC_WAIT_PROBES))
  while [[ "$SECONDS" -lt "$end" ]]; do
    log="$(console_log || true)"
    if last_probe net <<<"$log" | grep -q ' ok$' && last_probe disk <<<"$log" | grep -q ' ok [0-9]*$'; then
      lc_log "faults: both guest probes write ok"
      return 0
    fi
    lc_sleep "$LC_POLL"
  done
  echo "the probe server wrote no lab-probe line within ${LC_WAIT_PROBES} s"
  return 1
}

# lc_gate <fault> <seconds> — recovered, then the failed-gate procedure: the
# wait once more, a hard reboot of lab-chaos-0 (its probes restart from
# user-data), recovered 600. Prints one line, how the gate held, and returns 1
# when it did not; the waits go to faults/<fault>.gate.
lc_gate() {
  local f="$1" bound="$2" start="$SECONDS" log="${LC_DIR}/faults/${1}.gate"
  if recovered "$bound" >>"$log"; then
    echo "held at $((SECONDS - start)) s"
    return 0
  fi
  if recovered "$bound" >>"$log"; then
    echo "held at $((SECONDS - start)) s, after a second wait"
    return 0
  fi
  lc_log "faults: ${f}: the gate failed twice; hard-rebooting ${LC_PROBE}" >>"$log"
  echo "$(date -u +%T) hard reboot of ${LC_PROBE} after ${f}" >>"${LC_DIR}/faults/reboots"
  os server reboot --hard --wait "$LC_PROBE" >>"$log" 2>&1 || true
  if recovered 600 >>"$log"; then
    echo "failed twice; held at $((SECONDS - start)) s after the hard reboot"
    return 0
  fi
  echo "failed after the hard reboot"
  return 1
}

# lc_console_stats <file> <from HH:MM:SS> <to HH:MM:SS> — Of the probe lines
# of the console file inside the window: "<net lost> <net gap> <disk errors>
# <disk gap>", the gaps the largest time between two lines of a probe, in
# seconds.
lc_console_stats() {
  awk -v from="$2" -v to="$3" '
    function s(t, p) { split(t, p, ":"); return p[1] * 3600 + p[2] * 60 + p[3] }
    $1 == "lab-probe" && s($3) >= s(from) && s($3) <= s(to) {
      t = s($3)
      if ($2 == "net") { if ($4 == "lost") lost++; if (nl != "" && t - nl > ng) ng = t - nl; nl = t }
      if ($2 == "disk") { if ($4 == "error") errs++; if (dl != "" && t - dl > dg) dg = t - dl; dl = t }
    }
    END { printf "%d %d %d %d\n", lost, ng, errs, dg }' "$1"
}

# lc_state_diff <baseline> <state> — The fields of the state that differ from
# the baseline: domain by its running_ prefix, agents by holding True alone,
# nfs-server by its phase and ready flag, every other field as text.
lc_state_diff() {
  awk -v base="$1" -v now="$2" '
    function norm(k, v) {
      if (k == "domain") return (v ~ /^running_/) ? "running" : v
      if (k == "agents") return (v != "" && v !~ /(^|_)False(_|$)/ && v ~ /True/) ? "alive" : v
      if (k == "nfs-server") {
        # The node may change; the phase and the ready flag of each pod count.
        m = split(v, pods, ","); v = ""
        for (j = 1; j <= m; j++) { sub(/^[^\/]*\//, "", pods[j]); v = v (j > 1 ? "," : "") pods[j] }
        return v
      }
      return v
    }
    BEGIN {
      n = split(base, b, " "); for (i = 1; i <= n; i++) { split(b[i], kv, "="); bv[kv[1]] = norm(kv[1], substr(b[i], length(kv[1]) + 2)) }
      n = split(now, c, " ")
      for (i = 1; i <= n; i++) {
        split(c[i], kv, "="); v = norm(kv[1], substr(c[i], length(kv[1]) + 2))
        if (bv[kv[1]] != v) out = out (out == "" ? "" : ", ") kv[1] "=" v
      }
      print (out == "" ? "no field moved" : out)
    }'
}

# lc_pod_fault <f> <name> <selector> <node|any> <gate seconds> — One PodChaos
# pod-kill of the selector's pod (on lab-chaos-0's node for node), the wait for
# its replacement and the gate, with the page's per-fault files and one row of
# faults.tsv.
lc_pod_fault() {
  local f="$1" name="$2" sel="$3" scope="$4" bound="$5" d="${LC_DIR}/faults" before watcher applied t0 back gate
  local labels nodes=""
  lc_log "faults: ${f}: PodChaos ${name}"
  pids >"${d}/${f}.before" 2>&1 || true
  if [[ "$scope" == node ]]; then
    before="$(kubectl get pod -n "$NAMESPACE" -l "$sel" --field-selector "spec.nodeName=${LC_HOST}" -o jsonpath='{.items[*].metadata.uid}')"
    nodes="    nodes: [${LC_HOST}]"
  else
    before="$(kubectl get pod -n "$NAMESPACE" -l "$sel" -o jsonpath='{.items[*].metadata.uid}')"
  fi
  labels="$(tr ',' '\n' <<<"$sel" | sed 's/^/      /; s/=/: /')"
  watch_state "${d}/${f}.log" &
  watcher=$!
  applied="$(date -u +%T)"
  t0="$SECONDS"
  echo "applied at ${applied}" >>"${d}/${f}.log"
  kubectl apply -f - >/dev/null <<EOF
apiVersion: chaos-mesh.org/v1alpha1
kind: PodChaos
metadata:
  name: ${name}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/part-of: lab-chaos
spec:
  action: pod-kill
  mode: one
  selector:
    namespaces: [${NAMESPACE}]
${nodes}
    labelSelectors:
${labels}
EOF
  if ! kubectl wait "podchaos/${name}" -n "$NAMESPACE" --for=condition=AllInjected --timeout="${LC_WAIT_INJECTED}s" >/dev/null 2>&1; then
    kubectl describe podchaos "$name" -n "$NAMESPACE" >"${d}/${f}.describe" 2>&1 || true
    kill "$watcher" 2>/dev/null || true
    kubectl delete podchaos "$name" -n "$NAMESPACE" --ignore-not-found --wait --timeout=60s >/dev/null 2>&1 || true
    printf '%s\t%s\t-\t-\t-\t-\t-\tthe PodChaos did not reach AllInjected within %s s\t-\n' "$f" "$name" "$LC_WAIT_INJECTED" >>"${LC_DIR}/faults.tsv"
    return 2
  fi
  kubectl get podchaos "$name" -n "$NAMESPACE" -o jsonpath='{.spec.selector.nodes}{" "}{.status.experiment.containerRecords[*].id}{"\n"}' >"${d}/${f}.selected"
  if wait_replaced "$sel" "$before" "$scope" >>"${d}/${f}.log"; then
    back="$((SECONDS - t0)) s"
  else
    back="no Ready replacement within ${LC_WAIT_REPLACED} s"
  fi
  gate="$(lc_gate "$f" "$bound")" || true
  kill "$watcher" 2>/dev/null || true
  lc_fault_row "$f" "$name" "$applied" "$back" "$gate"
  kubectl delete podchaos "$name" -n "$NAMESPACE" --wait --timeout=60s >/dev/null 2>&1 || true
  [[ "$gate" != "failed after the hard reboot" ]]
}

# lc_fault_row <f> <fault> <from> <pod back> <gate> — The per-fault files after
# the gate and the row of faults.tsv.
lc_fault_row() {
  local f="$1" d="${LC_DIR}/faults" to qemu_before qemu_after host qemu console grew apis lost ngap errs dgap
  to="$(date -u +%T)"
  pids >"${d}/${f}.after" 2>&1 || true
  state >"${d}/${f}.state" 2>&1 || true
  os console log show "$LC_PROBE" 2>/dev/null | tr -d '\r' | grep 'lab-probe ' >"${d}/${f}.console" || true
  read -r lost ngap errs dgap <<<"$(lc_console_stats "${d}/${f}.console" "$3" "$to")"
  qemu_before="$(sed -n 's/^qemu=\([0-9]*\).*/\1/p' "${d}/${f}.before" | head -n 1)"
  qemu_after="$(sed -n 's/^qemu=\([0-9]*\).*/\1/p' "${d}/${f}.after" | head -n 1)"
  host="$(field os server show "$LC_PROBE" -c status -f value)"
  if [[ -n "$qemu_before" && "$qemu_before" == "$qemu_after" ]]; then
    qemu="${host}; QEMU PID ${qemu_before} kept"
  else
    qemu="${host}; QEMU PID ${qemu_before:-?} then ${qemu_after:-?}"
  fi
  grew="$(grep -c . "${d}/${f}.console" || true)"
  console="the log kept growing (${grew} probe lines)"
  [[ "$grew" -gt 0 ]] || console="no probe line in the console log"
  apis="$(lc_state_diff "$(cat "${d}/baseline.state")" "$(cat "${d}/${f}.state")"); $(($(grep -c . "${d}/${f}.log" || true) - 1)) state changes; the gate ${5}"
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t-\n' "$f" "$2" "$4" "$qemu" \
    "$([[ "$lost" == 0 ]] && echo "no lost line" || echo "${lost} lost lines"), largest gap ${ngap} s" \
    "$([[ "$errs" == 0 ]] && echo "no error line" || echo "${errs} error lines"), largest gap ${dgap} s" \
    "$console" "$apis" >>"${LC_DIR}/faults.tsv"
}

lc_cmd_faults() {
  local d f rc=0 gate applied t0 watcher back skip="" counter last out
  lc_open faults
  d="${LC_DIR}/faults"
  mkdir -p "$d"
  printf 'fault\tname\tpod back Ready after\tlab-chaos-0 and its QEMU\tguest network\tguest disk\tconsole\tAPIs and operators\tissue\n' >"${LC_DIR}/faults.tsv"
  # In this shell, not a command substitution: lc_boot sets LC_HOST and
  # LC_DOMAIN.
  if ! lc_boot "$d" >"${d}/boot.log" 2>&1; then
    lc_fail "faults: $(tail -n 1 "${d}/boot.log"); see ${d}/boot.log, and make lab-chaos-cleanup deletes what was created"
  fi
  state >"${d}/baseline.state"
  pids >"${d}/baseline.pids"
  lc_log "faults: baseline $(cat "${d}/baseline.state")"

  # F1 to F4: a pod-kill each. F4 hits the NFS server wherever it runs.
  set -- \
    "F1 lab-libvirt-kill app.kubernetes.io/name=libvirt node 600" \
    "F2 lab-nova-compute-kill app.kubernetes.io/name=novacompute,app.kubernetes.io/instance=lab,app.kubernetes.io/component=nova-compute node 600" \
    "F3 lab-ovn-controller-kill app.kubernetes.io/name=ovnchassis,app.kubernetes.io/instance=lab-chassis,app.kubernetes.io/component=ovn-controller node 600" \
    "F4 lab-nfs-server-kill app.kubernetes.io/name=nfs-server any 900"
  for f in "$@"; do
    read -r f name sel scope bound <<<"$f"
    if [[ -n "$skip" ]]; then
      echo "SKIP: ${f} (gate failed after the reboot)" | tee -a "${d}/skipped"
      continue
    fi
    rc=0
    lc_pod_fault "$f" "$name" "$sel" "$scope" "$bound" || rc=$?
    case "$rc" in
      0) ;;
      2) lc_fail "faults: ${f}: the PodChaos ${name} did not reach AllInjected; see ${d}/${f}.describe" ;;
      *) skip=1 ;;
    esac
  done

  # F5: the NFS server scaled to 0 for LC_HOLD_SCALE seconds.
  if [[ -n "$skip" ]]; then
    echo "SKIP: F5 (gate failed after the reboot)" | tee -a "${d}/skipped"
  else
    lc_log "faults: F5: nfs-server scaled to 0 for ${LC_HOLD_SCALE} s"
    pids >"${d}/F5.before" 2>&1 || true
    watch_state "${d}/F5.log" &
    watcher=$!
    applied="$(date -u +%T)"
    t0="$SECONDS"
    echo "scaled to 0 at ${applied}" >>"${d}/F5.log"
    kubectl scale deployment/nfs-server -n "$NAMESPACE" --replicas=0 >/dev/null
    kubectl wait pod -n "$NAMESPACE" -l app.kubernetes.io/name=nfs-server --for=delete --timeout=120s >/dev/null 2>&1 || true
    lc_sleep "$LC_HOLD_SCALE"
    echo "scaled to 1 at $(date -u +%T)" >>"${d}/F5.log"
    kubectl scale deployment/nfs-server -n "$NAMESPACE" --replicas=1 >/dev/null
    if kubectl rollout status deployment/nfs-server -n "$NAMESPACE" --timeout="${LC_WAIT_READY}s" >/dev/null 2>&1; then
      back="Ready $((SECONDS - t0 - LC_HOLD_SCALE)) s after the scale-up"
    else
      back="not rolled out within ${LC_WAIT_READY} s of the scale-up"
    fi
    gate="$(lc_gate F5 900)" || true
    kill "$watcher" 2>/dev/null || true
    lc_fault_row F5 nfs-server-scale "$applied" "$back" "$gate"
    kubectl get events -n "$NAMESPACE" --sort-by=.lastTimestamp 2>/dev/null |
      grep -E 'nfs-server|AttachVolume|Multi-Attach' >"${d}/F5.events" || true
  fi

  # After F5: a new volume on the backend whose server came back.
  if out="$( { os volume create --type nfs --size 1 "$LC_VOLUME_AFTER" >/dev/null &&
      lc_wait_status os volume show "$LC_VOLUME_AFTER" -c status -f value available; } 2>&1)"; then
    echo "available" >"${d}/vol-after"
  else
    echo "$out" >"${d}/vol-after"
  fi
  os volume delete "$LC_VOLUME_AFTER" >/dev/null 2>&1 || true

  # The disk counter: stop the server, read byte 4096 of the volume file
  # through the nfs1 volume pod, compare it with the last ok disk line.
  last="$(console_log | last_probe disk || true)"
  os server stop "$LC_PROBE" >/dev/null 2>&1 || true
  lc_wait_status os server show "$LC_PROBE" -c status -f value SHUTOFF >/dev/null || true
  local volume_id
  volume_id="$(os volume show "$LC_VOLUME" -c id -f value)"
  counter="$(kubectl exec -n "$NAMESPACE" deployment/controlplane-cinder-volume-nfs1 -- sh -c \
    "f=\$(ls /var/lib/cinder/mnt/*/volume-${volume_id}); dd if=\"\$f\" bs=512 skip=8 count=1 2>/dev/null | head -c 16" 2>&1 | tr -d '\0' || true)"
  printf 'last ok line: %s\nvolume file: %s\n' "$last" "$counter" >"${d}/counter"
  local n="${last##* }" m="${counter#probe-}"
  if [[ "$last" == *" ok "* && "$m" =~ ^[0-9]+$ && ( "$m" == "$n" || "$m" == "$((n + 1))" ) ]]; then
    echo "OK: the volume file holds probe-${m}, the last ok disk line counted ${n}"
  else
    echo "FAIL: the volume file holds '${counter}', the last ok disk line is '${last}'"
    rc=1
  fi
  lc_log "faults: faults.tsv written; run make lab-chaos-cleanup when the session is done with the servers"
  [[ -z "$skip" && "$rc" == 0 ]] || exit 1
}

# ---------------------------------------------------------------------------
# random, cleanup, teardown-check
# ---------------------------------------------------------------------------
lc_cmd_random() {
  local before after rc=0
  lc_open random
  before="$( (find "$LC_CHAOS_RANDOM_OUTPUT" -mindepth 1 -maxdepth 1 -type d 2>/dev/null || true) | sort | tail -n 1)"
  CHAOS_RANDOM_ENV=lab CHAOS_RANDOM_WINDOW="$LAB_CHAOS_RANDOM_DURATION" "$LC_CHAOS_RANDOM" run || rc=$?
  after="$( (find "$LC_CHAOS_RANDOM_OUTPUT" -mindepth 1 -maxdepth 1 -type d 2>/dev/null || true) | sort | tail -n 1)"
  if [[ -n "$after" && "$after" != "$before" ]]; then
    rm -f "${LC_DIR}/random"
    ln -s "$after" "${LC_DIR}/random"
    lc_log "random: ${LC_DIR}/random -> ${after}"
  fi
  return "$rc"
}

lc_cmd_cleanup() {
  local name deleted=0
  lc_open cleanup
  for name in $(os server list --all-projects --name '^lab-chaos-' -f value -c ID 2>/dev/null); do
    os server delete --wait "$name" >/dev/null && deleted=$((deleted + 1))
  done
  for name in $(os volume list --all-projects --name "$LC_VOLUME" -f value -c ID 2>/dev/null) \
    $(os volume list --all-projects --name "$LC_VOLUME_AFTER" -f value -c ID 2>/dev/null); do
    os volume delete "$name" >/dev/null && deleted=$((deleted + 1))
  done
  if os subnet show "$LC_SUBNET" >/dev/null 2>&1; then
    os subnet delete "$LC_SUBNET" >/dev/null && deleted=$((deleted + 1))
  fi
  if os network show "$LC_NET" >/dev/null 2>&1; then
    os network delete "$LC_NET" >/dev/null && deleted=$((deleted + 1))
  fi
  if os server group show "$LC_GROUP" >/dev/null 2>&1; then
    os server group delete "$LC_GROUP" >/dev/null && deleted=$((deleted + 1))
  fi
  if [[ -n "$(kubectl get podchaos,networkchaos -n "$NAMESPACE" -l "$LC_LABEL" -o name 2>/dev/null)" ]]; then
    kubectl delete podchaos,networkchaos -n "$NAMESPACE" -l "$LC_LABEL" --wait --timeout=120s >/dev/null && deleted=$((deleted + 1))
  fi
  if [[ "$(kubectl get deployment nfs-server -n "$NAMESPACE" -o jsonpath='{.spec.replicas}' 2>/dev/null)" == 0 ]]; then
    kubectl scale deployment/nfs-server -n "$NAMESPACE" --replicas=1 >/dev/null
    lc_log "cleanup: scaled nfs-server back to 1"
  fi
  lc_log "cleanup: deleted ${deleted} object(s)"
}

lc_cmd_teardown_check() {
  local d out crds ns chart
  lc_open teardown-check
  if [[ -n "$(os server list --all-projects --name '^lab-chaos-' -f value -c Name 2>/dev/null)" ]]; then
    lc_fail "teardown-check ends the session and the lab-chaos-* servers still exist; run make lab-chaos-cleanup first"
  fi
  d="${LC_DIR}/proving"
  mkdir -p "$d"
  lc_delay_yaml >"${d}/delay.yaml"
  lc_chaos "${d}/delay.yaml"
  kubectl wait networkchaos/lab-keystone-delay -n "$NAMESPACE" --for=condition=AllInjected --timeout="${LC_WAIT_INJECTED}s" >/dev/null
  lc_log "teardown-check: a delay is active; EXTERNAL_CLUSTER=true make teardown-infra"
  if ! EXTERNAL_CLUSTER=true make -C "$LC_REPO_ROOT" teardown-infra >"${d}/step6.log" 2>&1; then
    lc_proving_row 6 FAIL "make teardown-infra failed; see proving/step6.log: $(tail -n 1 "${d}/step6.log")"
    exit 1
  fi
  crds="$(kubectl get crd -o name | grep -c 'chaos-mesh\.org' || true)"
  ns="$(kubectl get namespace chaos-mesh 2>&1 | grep -o NotFound || echo present)"
  chart="$(kubectl get clusterrole,clusterrolebinding,mutatingwebhookconfiguration,validatingwebhookconfiguration -o name | grep -c chaos-mesh || true)"
  printf '%s\n%s\n%s\n' "$crds" "$ns" "$chart" >>"${d}/step6.log"
  if grep -q 'Deleting the Chaos Mesh experiments...' "${d}/step6.log" && [[ "$crds $ns $chart" == "0 NotFound 0" ]]; then
    lc_proving_row 6 pass "the teardown exited 0 and deleted the experiments first; then 0, NotFound and 0"
  else
    lc_proving_row 6 FAIL "then ${crds}, ${ns} and ${chart}; see proving/step6.log"
    exit 1
  fi
}

# ---------------------------------------------------------------------------
# record
# ---------------------------------------------------------------------------
lc_cmd_record() {
  local run date commit shoot chart nodes hand random verdict
  lc_check_settings record
  run="${LAB_CHAOS_RUN:-$(cat "${LC_OUTPUT_DIR}/current" 2>/dev/null || true)}"
  [[ -n "$run" && -d "${LC_OUTPUT_DIR}/${run}" ]] || lc_fail "no run directory under ${LC_OUTPUT_DIR}; set LAB_CHAOS_RUN"
  LC_DIR="${LC_OUTPUT_DIR}/${run}"
  date="${run:0:4}-${run:4:2}-${run:6:2}"
  commit="$(git -C "$LC_REPO_ROOT" rev-parse --short=9 HEAD 2>/dev/null || echo unknown)"
  shoot="$(cat "${LC_DIR}/shoot" 2>/dev/null || echo unknown)"
  chart="$(sed -n 's/^chart //p' "${LC_DIR}/proving/step1.log" 2>/dev/null | head -n 1)"
  nodes="$(awk 'NF == 3 || NF > 3 { if ($2 ~ /^v[0-9]/) { n++; v = $2 } } END { if (n) printf "%d workers on Kubernetes %s", n, v }' "${LC_DIR}/proving/step1.log" 2>/dev/null)"
  hand="none"
  [[ -s "${LC_DIR}/hand-actions.txt" ]] && hand="$(paste -sd ';' "${LC_DIR}/hand-actions.txt" | sed 's/;/; /g')"
  {
    printf 'The run of %s ran `make lab-chaos-proving`, `make lab-chaos-faults` and `make lab-chaos-random` from commit `%s` on shoot `%s`, %s, with Chaos Mesh chart %s. Hand actions: %s.\n' \
      "$date" "$commit" "$shoot" "${nodes:-the nodes of proving/step1.log}" "${chart:-unknown}" "$hand"
    printf '\n| Step | Result | Measured |\n| --- | --- | --- |\n'
    awk -F '\t' '{ printf "| %s | %s | %s |\n", $1, $2, $3 }' "${LC_DIR}/proving.tsv" 2>/dev/null || true
    printf '\n| Fault | Pod back Ready after | lab-chaos-0 and its QEMU | Guest network | Guest disk | Console | What the APIs and operators reported | Issue |\n| --- | --- | --- | --- | --- | --- | --- | --- |\n'
    awk -F '\t' 'NR > 1 { printf "| %s `%s` | %s | %s | %s | %s | %s | %s | %s |\n", $1, $2, $3, $4, $5, $6, $7, $8, $9 }' "${LC_DIR}/faults.tsv" 2>/dev/null || true
    random="${LC_DIR}/random/random-verdict.json"
    printf '\n| Random run | Seed | Window | Steps drawn | Verdict | Failed checks |\n| --- | --- | --- | --- | --- | --- |\n'
    if [[ -f "$random" ]]; then
      verdict="$(jq -r '.verdict' "$random")"
      printf '| %s | %s | %s | %s | %s | %s |\n' "$(jq -r '.run' "$random")" "$(jq -r '.seed' "$random")" "$(jq -r '.window' "$random")" \
        "$(jq '.steps | length' "${LC_DIR}/random/plan.json" 2>/dev/null || echo "?")" "$verdict" \
        "$(jq -r '[.checks[] | select(.pass | not) | .name] | if length == 0 then "none" else join(", ") end' "$random")"
    else
      printf '| not run | — | — | — | — | — |\n'
    fi
  } >"${LC_DIR}/record.md"
  echo "record: ${LC_DIR}/record.md"
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------
lc_main() {
  case "${1:-}" in
    preflight) lc_preflight ;;
    proving) lc_cmd_proving ;;
    faults) lc_cmd_faults ;;
    random) lc_cmd_random ;;
    cleanup) lc_cmd_cleanup ;;
    teardown-check) lc_cmd_teardown_check ;;
    record) lc_cmd_record ;;
    *) lc_usage ;;
  esac
}

# Run lc_main only when executed directly so unit tests (tests/unit/hack/) can
# source this script and exercise individual functions.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  lc_main "$@"
fi
