#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/chaos-random.sh — Run random chaos under dizzy load (#1395): draw faults
# from tests/chaos-random/catalogue.yaml with tests/chaos-random/draw.py,
# apply them as one Chaos Mesh Workflow against a running ControlPlane while
# the in-cluster dizzy soak of hack/dizzy-soak.sh churns, and judge the run.
# CI runs it every night on the kind plane of tests/chaos-random/kind/
# (e2e-chaos-random); the lab runs it through hack/lab-chaos.sh random.
#
# Subcommands, wrapped by the make targets chaos-random-<subcommand>:
#   draw          Print the plan of a seed; needs no cluster.
#   up            (ci only) Bring up the kind plane and wait for each stage.
#   preflight     Read-only checks of the cluster for a run.
#   run           preflight, draw, start the soak, apply the Workflow after the
#                 warm-up, record the fault windows and targets, wait for the
#                 soak, judge, and write _output/chaos-random/<run>/.
#   report [run]  Print report.md of the newest or a named run.
#
# Settings (environment):
#   CHAOS_RANDOM_ENV           required: ci or lab.
#   CHAOS_RANDOM_SEED          the draw's seed (default: the epoch seconds at
#                              run or draw).
#   CHAOS_RANDOM_WINDOW        the span the faults are drawn over (default 30m
#                              on ci, 6h on lab).
#   CHAOS_RANDOM_TAIL          the churn after the window (default 10m).
#   CHAOS_RANDOM_WARMUP        the churn before the first fault (default 120s).
#   CHAOS_RANDOM_CP_NAME       the ControlPlane (default controlplane).
#   CHAOS_RANDOM_CP_NAMESPACE  its namespace (default openstack).
# The DIZZY_* settings of hack/dizzy-soak.sh pass through to the soak. On ci,
# DIZZY_SCENARIO defaults to tests/chaos-random/kind/scenario.yaml.
#
# A run's id is its UTC start, YYYYMMDDTHHMMSSZ, and its Workflow is
# chaos-random-<id, lowercased> in the ControlPlane's namespace. The run
# writes plan.json, workflow.json, windows.tsv (one line per chaos template
# that read AllInjected: injected at, template, entry, lifted at), targets.tsv
# (template and <namespace>/<pod> of every container record), soak/ (the
# soak's report), memory-growth.tsv, random-verdict.json, report.md, and
# _output/reports/chaos-random-junit.xml. It exits 0 on PASS and 1 on FAIL.
#
# docs/reference/testing/chaos-e2e-tests.md (Random runs) describes the run.

set -euo pipefail

CR_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CR_REPO_ROOT="$(cd "${CR_SCRIPT_DIR}/.." && pwd)"

# Settings.
CHAOS_RANDOM_ENV="${CHAOS_RANDOM_ENV:-}"
CHAOS_RANDOM_SEED="${CHAOS_RANDOM_SEED:-}"
CHAOS_RANDOM_WINDOW="${CHAOS_RANDOM_WINDOW:-}"
CHAOS_RANDOM_TAIL="${CHAOS_RANDOM_TAIL:-10m}"
CHAOS_RANDOM_WARMUP="${CHAOS_RANDOM_WARMUP:-120s}"
CHAOS_RANDOM_CP_NAME="${CHAOS_RANDOM_CP_NAME:-controlplane}"
CHAOS_RANDOM_CP_NAMESPACE="${CHAOS_RANDOM_CP_NAMESPACE:-openstack}"

# The paths, the draw's parameters and the waits in seconds. Plain variables,
# so the unit test can point them elsewhere or shorten them after sourcing.
CR_CATALOGUE="${CR_REPO_ROOT}/tests/chaos-random/catalogue.yaml"
CR_DRAW="${CR_REPO_ROOT}/tests/chaos-random/draw.py"
CR_KIND_DIR="${CR_REPO_ROOT}/tests/chaos-random/kind"
CR_OUTPUT_DIR="${CR_REPO_ROOT}/_output/chaos-random"
CR_REPORTS_DIR="${CR_REPO_ROOT}/_output/reports"
CR_SOAK="${CR_REPO_ROOT}/hack/dizzy-soak.sh"
CR_SOAK_RUNNER="${CR_REPO_ROOT}/hack/dizzy-soak-runner.sh"
CR_SOAK_OUTPUT_DIR="${CR_REPO_ROOT}/_output/dizzy/soak"
CR_CONSISTENCY="${CR_REPO_ROOT}/tests/e2e-chaos/lib/consistency.sh"
CR_BROKER_VHOST="${CR_REPO_ROOT}/tests/e2e/cinder/broker-vhost.sh"
CR_DB_TENANT="${CR_REPO_ROOT}/deploy/openbao/bootstrap/setup-database-tenant.sh"
CR_DISCOVER_HOSTS="${CR_REPO_ROOT}/tests/e2e/nova/discover-hosts.sh"
CR_STUCK_IMAGE="ghcr.io/c5c3/tempest:2026.1"
CR_VM_QUERY="/api/v1/namespaces/dizzy/services/dizzy-victoria-metrics-server:8428/proxy/api/v1/query"
CR_POLL=5
CR_ACCOMPLISH_SLACK=600
CR_SOAK_FINISH=600
CR_GONE=300
CR_STUCK_DEADLINE=300
CR_STUCK_POD_SECONDS=300
CR_WAIT_OVNCENTRAL=300
CR_WAIT_MARIADB=600
CR_WAIT_CONTROLPLANE=1800
CR_WAIT_CHASSIS=300
CR_WAIT_COMPUTE=300
CR_WAIT_SEED=300

# The run, set by cr_run and read by the trap.
CR_RUN=""
CR_DIR=""
CR_WORKFLOW=""
CR_SOAK_STARTED=""
CR_DB_SYNC_PATCHED=""

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
cr_usage() {
  cat >&2 <<EOF
Usage: hack/chaos-random.sh <subcommand>

Subcommands:
  draw            Print the plan of a seed (no cluster).
  up              (ci) Bring up the kind plane of tests/chaos-random/kind/.
  preflight       Read-only checks of the cluster.
  run             Run random chaos under the dizzy soak and judge it.
  report [run]    Print report.md of the newest or a named run.

Settings (environment):
  CHAOS_RANDOM_ENV           ci or lab (required).
  CHAOS_RANDOM_SEED          the draw's seed (default: the epoch seconds).
  CHAOS_RANDOM_WINDOW        the span of the faults (default 30m on ci, 6h on lab).
  CHAOS_RANDOM_TAIL          the churn after the window (default 10m).
  CHAOS_RANDOM_WARMUP        the churn before the first fault (default 120s).
  CHAOS_RANDOM_CP_NAME       the ControlPlane (default controlplane).
  CHAOS_RANDOM_CP_NAMESPACE  its namespace (default openstack).
The DIZZY_* settings of hack/dizzy-soak.sh pass through to the soak.
EOF
  exit 2
}

# cr_log <message> — A progress line with the UTC time.
cr_log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

# cr_fail <message> — An ERROR line, then exit 1.
cr_fail() {
  echo "ERROR: $*" >&2
  exit 1
}

# cr_first_line <text> — The first line of the text.
cr_first_line() {
  printf '%s\n' "${1%%$'\n'*}"
}

# cr_last_line <text> — The last non-empty line of the text.
cr_last_line() {
  printf '%s\n' "$1" | awk 'NF { last = $0 } END { print last }'
}

# cr_seconds <value> — The seconds of a duration of h, m and s parts such as
# 6h, 30m, 1h30m or 120s, or of a bare number; empty and malformed print
# nothing and return 1.
cr_seconds() {
  local re='^(([0-9]+)h)?(([0-9]+)m)?(([0-9]+)s)?$'
  if [[ "$1" =~ ^[0-9]+$ ]]; then
    echo $((10#$1))
    return 0
  fi
  [[ -n "$1" && "$1" =~ $re ]] || return 1
  echo $((10#${BASH_REMATCH[2]:-0} * 3600 + 10#${BASH_REMATCH[4]:-0} * 60 + 10#${BASH_REMATCH[6]:-0}))
}

# cr_sleep <seconds> — Sleep in the background and wait for it, so a trapped
# TERM or INT runs its handler at once instead of after the sleep.
cr_sleep() {
  sleep "$1" &
  wait "$!" || true
}

# cr_now — The current time in epoch seconds.
cr_now() {
  date -u +%s
}

# cr_time <epoch> — The RFC 3339 UTC form of an epoch time.
cr_time() {
  jq -nr --argjson t "$1" '$t | todate'
}

# cr_epoch <RFC 3339 time> — The epoch seconds of an RFC 3339 UTC time.
cr_epoch() {
  jq -nr --arg t "$1" '$t | fromdateiso8601'
}

# cr_check_settings — The usage errors of the settings, before any call. Sets
# the defaults that depend on the environment and CR_WINDOW_S, CR_TAIL_S and
# CR_WARMUP_S.
cr_check_settings() {
  case "$CHAOS_RANDOM_ENV" in
    ci) CHAOS_RANDOM_WINDOW="${CHAOS_RANDOM_WINDOW:-30m}" ;;
    lab) CHAOS_RANDOM_WINDOW="${CHAOS_RANDOM_WINDOW:-6h}" ;;
    "")
      echo "ERROR: CHAOS_RANDOM_ENV is not set; it is ci or lab" >&2
      exit 2
      ;;
    *)
      echo "ERROR: CHAOS_RANDOM_ENV must be ci or lab, not '${CHAOS_RANDOM_ENV}'" >&2
      exit 2
      ;;
  esac
  if [[ -n "$CHAOS_RANDOM_SEED" && ! "$CHAOS_RANDOM_SEED" =~ ^[0-9]+$ ]]; then
    echo "ERROR: CHAOS_RANDOM_SEED must be a whole number, not '${CHAOS_RANDOM_SEED}'" >&2
    exit 2
  fi
  if ! CR_WINDOW_S="$(cr_seconds "$CHAOS_RANDOM_WINDOW")" || [[ "$CR_WINDOW_S" -eq 0 ]]; then
    echo "ERROR: CHAOS_RANDOM_WINDOW must be a duration such as 30m or 6h, not '${CHAOS_RANDOM_WINDOW}'" >&2
    exit 2
  fi
  if ! CR_TAIL_S="$(cr_seconds "$CHAOS_RANDOM_TAIL")"; then
    echo "ERROR: CHAOS_RANDOM_TAIL must be a duration such as 10m, not '${CHAOS_RANDOM_TAIL}'" >&2
    exit 2
  fi
  if ! CR_WARMUP_S="$(cr_seconds "$CHAOS_RANDOM_WARMUP")"; then
    echo "ERROR: CHAOS_RANDOM_WARMUP must be a duration such as 120s, not '${CHAOS_RANDOM_WARMUP}'" >&2
    exit 2
  fi
}

# cr_draw_args — The draw's pause and overlap parameters of the environment.
cr_draw_args() {
  if [[ "$CHAOS_RANDOM_ENV" == lab ]]; then
    echo "--pause-min 60 --pause-max 600 --overlap 0.33"
  else
    echo "--pause-min 30 --pause-max 120 --overlap 0.33"
  fi
}

# cr_draw <seed> <plan> [workflow name] [workflow file] — Draw a plan (and a
# Workflow) from the catalogue; draw.py's exit code.
cr_draw() {
  local seed="$1" plan="$2" name="${3:-chaos-random}" workflow="${4:-}" args catalogue
  read -r -a args <<<"$(cr_draw_args)"
  if ! catalogue="$(yq -o=json "$CR_CATALOGUE")"; then
    echo "ERROR: cannot read ${CR_CATALOGUE} (yq's error is above)" >&2
    return 2
  fi
  python3 -I "$CR_DRAW" --environment "$CHAOS_RANDOM_ENV" --seed "$seed" --window "$CR_WINDOW_S" \
    "${args[@]}" --name "$name" --cp "$CHAOS_RANDOM_CP_NAME" --namespace "$CHAOS_RANDOM_CP_NAMESPACE" \
    --plan "$plan" ${workflow:+--workflow "$workflow"} <<<"$catalogue"
}

# cr_require <command...> — Each command is on PATH, or a FAIL line names the
# missing ones and it returns 1.
cr_require() {
  local cmd missing=""
  for cmd in "$@"; do
    command -v "$cmd" >/dev/null 2>&1 || missing="${missing} ${cmd}"
  done
  if [[ -n "$missing" ]]; then
    echo "FAIL: not on PATH:${missing}"
    return 1
  fi
  echo "OK: $* on PATH"
}

# ---------------------------------------------------------------------------
# draw
# ---------------------------------------------------------------------------
cr_cmd_draw() {
  cr_check_settings
  local seed="${CHAOS_RANDOM_SEED:-$(cr_now)}"
  cr_draw "$seed" -
}

# ---------------------------------------------------------------------------
# up: the kind plane, stage by stage
# ---------------------------------------------------------------------------

# cr_stage_failed <stage> <seconds> <kubectl get arguments...> — The stage
# did not get ready: name it, dump its objects and exit 1.
cr_stage_failed() {
  local stage="$1" seconds="$2"
  shift 2
  echo "up: ${stage} not ready within ${seconds} s" >&2
  echo "=== ${stage}: kubectl get $* ===" >&2
  kubectl get "$@" -o wide >&2 || true
  # A named object comes back without .items, a selector as a List.
  kubectl get "$@" -o json 2>/dev/null | jq -r '(.items // [.])[] | "\(.kind)/\(.metadata.name):",
    (.status.conditions[]? | "  \(.type)=\(.status) \(.reason // ""): \(.message // "")")' >&2 || true
  exit 1
}

# cr_wait_for <stage> <seconds> <object> <condition> — kubectl wait for the
# condition of the object in the ControlPlane's namespace.
cr_wait_for() {
  local stage="$1" seconds="$2" object="$3" condition="$4"
  cr_log "up: waiting up to ${seconds} s for ${object} ${condition}"
  if ! kubectl wait "$object" -n "$CHAOS_RANDOM_CP_NAMESPACE" --for="condition=${condition}" --timeout="${seconds}s" >/dev/null; then
    cr_stage_failed "$stage" "$seconds" "$object" -n "$CHAOS_RANDOM_CP_NAMESPACE"
  fi
}

# cr_onboard_db_tenant — Onboard the ControlPlane's OpenBao database-engine
# tenant with setup-database-tenant.sh, once its MariaDB is Ready. The job's
# deploy-infra runs with WITH_CONTROLPLANE_CR=false and leaves this step to
# whoever applies the ControlPlane; without it the engine roles do not exist,
# the DB credential ExternalSecrets get no data, and the ControlPlane never
# turns Ready.
cr_onboard_db_tenant() {
  local ns="$CHAOS_RANDOM_CP_NAMESPACE" encoded token rc=0
  cr_log "up: waiting up to ${CR_WAIT_MARIADB} s for mariadb/openstack-db Ready"
  if ! kubectl wait mariadb/openstack-db -n "$ns" --for=create --timeout="${CR_WAIT_MARIADB}s" >/dev/null ||
    ! kubectl wait mariadb/openstack-db -n "$ns" --for=condition=Ready --timeout="${CR_WAIT_MARIADB}s" >/dev/null; then
    cr_stage_failed mariadb "$CR_WAIT_MARIADB" mariadb/openstack-db -n "$ns"
  fi
  if ! encoded="$(kubectl get secret openbao-init-keys -n shared-services -o jsonpath='{.data.init-output}')"; then
    cr_fail "up: cannot read Secret shared-services/openbao-init-keys (kubectl's error is above)"
  fi
  # printf is a builtin, so the root token never appears in a process's argv.
  token="$(printf '%s' "$encoded" | jq -Rr '@base64d | fromjson | .root_token' 2>/dev/null)" || token=""
  case "$token" in
    '' | null) cr_fail "up: Secret shared-services/openbao-init-keys carries no root_token" ;;
  esac
  BAO_TOKEN="$token" "$CR_DB_TENANT" "$ns" "$CHAOS_RANDOM_CP_NAME" || rc=$?
  if [[ "$rc" -ne 0 ]]; then
    cr_fail "up: setup-database-tenant.sh ${ns} ${CHAOS_RANDOM_CP_NAME} failed with exit ${rc} (its error is above)"
  fi
}

cr_cmd_up() {
  cr_check_settings
  if [[ "$CHAOS_RANDOM_ENV" != ci ]]; then
    cr_fail "up brings up the kind plane; it runs with CHAOS_RANDOM_ENV=ci only"
  fi
  if [[ "$CHAOS_RANDOM_CP_NAME" != controlplane || "$CHAOS_RANDOM_CP_NAMESPACE" != openstack ]]; then
    cr_fail "up applies ${CR_KIND_DIR}, which names ControlPlane openstack/controlplane; keep CHAOS_RANDOM_CP_NAME and CHAOS_RANDOM_CP_NAMESPACE at their defaults"
  fi
  local ns="$CHAOS_RANDOM_CP_NAMESPACE" cp="$CHAOS_RANDOM_CP_NAME" node
  if ! node="$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')" || [[ -z "$node" ]]; then
    cr_fail "cannot read the cluster's node (kubectl's error is above)"
  fi

  # 1. The OVN central the ControlPlane's Neutron references.
  kubectl apply -f "${CR_KIND_DIR}/ovncentral.yaml"
  cr_wait_for ovncentral "$CR_WAIT_OVNCENTRAL" "ovncentral/${cp}-ovn" Ready
  # 2. The bus Secret, against a vhost of the plane's own on the kind broker.
  if ! "$CR_BROKER_VHOST" create "$cp" "${cp}-messaging" "$ns"; then
    cr_fail "up: cannot write the bus Secret ${ns}/${cp}-messaging (broker-vhost.sh's error is above)"
  fi
  # 3. The ControlPlane, after the chassis role its OVNChassis selects on,
  #    with the database-engine tenant of the MariaDB it projects.
  kubectl label node "$node" openstack.c5c3.io/chassis=true --overwrite >/dev/null
  kubectl apply -f "${CR_KIND_DIR}/controlplane.yaml"
  cr_onboard_db_tenant
  cr_wait_for controlplane "$CR_WAIT_CONTROLPLANE" "controlplane/${cp}" Ready
  # 4. The chassis and the metadata agent.
  kubectl apply -f "${CR_KIND_DIR}/ovnchassis.yaml" -f "${CR_KIND_DIR}/metadata-agent.yaml"
  cr_wait_for chassis "$CR_WAIT_CHASSIS" "ovnchassis/${cp}-chassis" Ready
  cr_wait_for metadata-agent "$CR_WAIT_CHASSIS" "neutronmetadataagent/${cp}-agent" Ready
  # 5. The fake compute and its host mapping.
  kubectl apply -f "${CR_KIND_DIR}/fake-compute.yaml"
  cr_wait_for compute "$CR_WAIT_COMPUTE" "deployment/${cp}-fake-compute" Available
  if ! "$CR_DISCOVER_HOSTS" "${cp}-nova" "$ns" "$node"; then
    cr_fail "up: compute host ${node} was not mapped (discover-hosts.sh's error is above)"
  fi
  # 6. The soak's flavor and image.
  kubectl delete job chaos-random-seed -n "$ns" --ignore-not-found >/dev/null
  kubectl apply -f "${CR_KIND_DIR}/seed-job.yaml"
  if ! kubectl wait job/chaos-random-seed -n "$ns" --for=condition=Complete --timeout="${CR_WAIT_SEED}s" >/dev/null; then
    kubectl logs job/chaos-random-seed -n "$ns" --tail=20 >&2 || true
    cr_stage_failed seed "$CR_WAIT_SEED" job/chaos-random-seed -n "$ns"
  fi
  cr_log "up: ControlPlane ${ns}/${cp} is Ready with its chassis, metadata agent, compute and seed; run make chaos-random-run"
}

# ---------------------------------------------------------------------------
# preflight
# ---------------------------------------------------------------------------

# cr_selected_check <pods json> <namespaces json> <catalogue json> — One line
# per entry of the environment whose selector, or whose NetworkChaos target,
# selects no pod. A pod counts unless it is Succeeded, Failed or terminating;
# on lab a namespace without the annotation chaos-mesh.org/inject: enabled
# contributes no pod, since the lab's Chaos Mesh selects none there.
cr_selected_check() {
  jq -rn --slurpfile pods "$1" --slurpfile namespaces "$2" --slurpfile entries "$3" \
    --arg env "$CHAOS_RANDOM_ENV" --arg cp "$CHAOS_RANDOM_CP_NAME" --arg ns "$CHAOS_RANDOM_CP_NAMESPACE" '
    def sub_vars: if type == "string" then gsub("@CP@"; $cp) | gsub("@NS@"; $ns)
      elif type == "array" then map(sub_vars) elif type == "object" then map_values(sub_vars) else . end;
    ([$namespaces[0].items[] | select($env != "lab" or .metadata.annotations["chaos-mesh.org/inject"] == "enabled")
      | .metadata.name]) as $open
    | [$pods[0].items[] | select(.status.phase != "Succeeded" and .status.phase != "Failed"
        and .metadata.deletionTimestamp == null and (.metadata.namespace | IN($open[])))] as $live
    | def matches($s):
        . as $p
        | (($s.namespaces // []) | length == 0 or ($p.metadata.namespace | IN($s.namespaces[])))
          and all(($s.labelSelectors // {}) | to_entries[]; ($p.metadata.labels[.key] // null) == .value)
          and all(($s.expressionSelectors // [])[];
            ($p.metadata.labels[.key] // null) as $v | (.values // []) as $values
            | if .operator == "Exists" then $v != null
              elif .operator == "DoesNotExist" then $v == null
              elif .operator == "In" then ($v != null and ($v | IN($values[])))
              elif .operator == "NotIn" then ($v == null or ($v | IN($values[]) | not))
              else false end);
    def selects($s): $s != null and any($live[]; matches($s));
    $entries[0][] | select(.environments | index($env)) | (.spec | sub_vars) as $spec
    | select((selects($spec.selector) | not)
        or (.kind == "NetworkChaos" and $spec.target != null and (selects($spec.target.selector) | not)))
    | .name'
}

# cr_preflight — The read-only checks, one OK: or FAIL: line each; returns 1
# after the first FAIL: of the cluster, or after the catalogue's FAIL: lines.
cr_preflight() {
  local out states tmp failed
  cr_require kubectl jq yq python3 || return 1
  if ! out="$(kubectl version --request-timeout=10s 2>&1)"; then
    echo "FAIL: the cluster does not answer: $(cr_last_line "$out")"
    return 1
  fi
  echo "OK: the cluster answers"
  local namespace
  for namespace in chaos-mesh dizzy; do
    if ! out="$(kubectl get namespace "$namespace" -o name 2>&1)"; then
      echo "FAIL: namespace ${namespace} is missing: $(cr_first_line "$out")"
      return 1
    fi
    echo "OK: namespace ${namespace}"
  done
  if ! out="$(kubectl get controlplane "$CHAOS_RANDOM_CP_NAME" -n "$CHAOS_RANDOM_CP_NAMESPACE" \
      -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}' 2>&1)" || [[ "$out" != True ]]; then
    echo "FAIL: ControlPlane ${CHAOS_RANDOM_CP_NAMESPACE}/${CHAOS_RANDOM_CP_NAME} is not Ready=True (read '$(cr_first_line "$out")')"
    return 1
  fi
  echo "OK: ControlPlane ${CHAOS_RANDOM_CP_NAMESPACE}/${CHAOS_RANDOM_CP_NAME} is Ready=True"
  if ! out="$(kubectl get workflows.chaos-mesh.org -A -o 'jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' 2>&1)"; then
    echo "FAIL: cannot list the Chaos Mesh Workflows: $(cr_first_line "$out")"
    return 1
  fi
  if grep -q '/chaos-random-' <<<"$out"; then
    echo "FAIL: a random run's Workflow exists: $(grep '/chaos-random-' <<<"$out" | paste -sd ' ' -)"
    return 1
  fi
  if ! states="$(kubectl get job dizzy-soak -n dizzy --ignore-not-found -o json 2>&1)"; then
    echo "FAIL: cannot read the Job dizzy/dizzy-soak: $(cr_first_line "$states")"
    return 1
  fi
  if [[ -n "$states" ]] && [[ "$(jq -r '[.status.conditions[]? | select(.status == "True") | .type]
      | if any(.[]; . == "Complete" or . == "Failed") then "done" else "running" end' <<<"$states")" == running ]]; then
    echo "FAIL: a soak runs (Job dizzy/dizzy-soak); stop it with make dizzy-soak-stop"
    return 1
  fi
  echo "OK: no random run's Workflow and no soak"

  tmp="$(mktemp -d "${TMPDIR:-/tmp}/chaos-random-preflight.XXXXXX")"
  if ! kubectl get pods -A -o json >"${tmp}/pods.json" || ! kubectl get namespaces -o json >"${tmp}/namespaces.json" ||
    ! yq -o=json "$CR_CATALOGUE" >"${tmp}/catalogue.json"; then
    rm -rf "$tmp"
    echo "FAIL: cannot read the pods, the namespaces or the catalogue (the error is above)"
    return 1
  fi
  if ! failed="$(cr_selected_check "${tmp}/pods.json" "${tmp}/namespaces.json" "${tmp}/catalogue.json")"; then
    rm -rf "$tmp"
    echo "FAIL: cannot match the catalogue's selectors (jq's error is above)"
    return 1
  fi
  local total
  total="$(jq --arg env "$CHAOS_RANDOM_ENV" '[.[] | select(.environments | index($env))] | length' "${tmp}/catalogue.json")"
  rm -rf "$tmp"
  if [[ -n "$failed" ]]; then
    while IFS= read -r name; do
      echo "FAIL: catalogue entry ${name} selects no pod in ${CHAOS_RANDOM_ENV}"
    done <<<"$failed"
    return 1
  fi
  echo "OK: every one of the ${total} catalogue entries of ${CHAOS_RANDOM_ENV} selects a pod"
}

cr_cmd_preflight() {
  cr_check_settings
  cr_preflight
}

# ---------------------------------------------------------------------------
# run: the soak and the Workflow
# ---------------------------------------------------------------------------

# cr_soak_state — none, running, complete or failed: the Job dizzy/dizzy-soak.
cr_soak_state() {
  local job
  job="$(kubectl get job dizzy-soak -n dizzy --ignore-not-found -o json 2>/dev/null)" || {
    echo unknown
    return
  }
  if [[ -z "$job" ]]; then
    echo none
    return
  fi
  jq -r '[.status.conditions[]? | select(.status == "True") | .type] as $c
    | if any($c[]; . == "Complete") then "complete" elif any($c[]; . == "Failed") then "failed" else "running" end' <<<"$job"
}

# cr_template_of <workflow node> — The template of a WorkflowNode, cached in
# ${CR_DIR}/.nodes.
cr_template_of() {
  local node="$1" template
  template="$(awk -F '\t' -v n="$node" '$1 == n { print $2; exit }' "${CR_DIR}/.nodes" 2>/dev/null)"
  if [[ -z "$template" ]]; then
    template="$(kubectl get workflownodes.chaos-mesh.org "$node" -n "$CHAOS_RANDOM_CP_NAMESPACE" \
      -o 'jsonpath={.spec.templateName}' 2>/dev/null)" || template=""
    [[ -n "$template" ]] && printf '%s\t%s\n' "$node" "$template" >>"${CR_DIR}/.nodes"
  fi
  printf '%s\n' "$template"
}

# cr_observe — One read of the run's chaos objects: a template that reads
# AllInjected=True for the first time gets its windows line, each container
# record its targets line, and a template seen injected and no longer listed
# its lift. A read that fails changes nothing.
cr_observe() {
  local objects rows node injected records template now entry id
  objects="$(kubectl get podchaos,networkchaos -n "$CHAOS_RANDOM_CP_NAMESPACE" \
    -l "chaos-mesh.org/workflow=${CR_WORKFLOW}" -o json 2>/dev/null)" || return 0
  now="$(cr_now)"
  rows="$(jq -r '.items[] | [(.metadata.labels["chaos-mesh.org/controlled-by"] // ""),
      (any(.status.conditions[]?; .type == "AllInjected" and .status == "True") | tostring),
      ([.status.experiment.containerRecords[]?.id | split("/")[0:2] | join("/")] | unique | join(","))] | @tsv' \
    <<<"$objects" 2>/dev/null)" || return 0
  : >"${CR_DIR}/.present"
  while IFS=$'\t' read -r node injected records; do
    [[ -n "$node" ]] || continue
    template="$(cr_template_of "$node")"
    [[ -n "$template" ]] || continue
    echo "$template" >>"${CR_DIR}/.present"
    if [[ "$injected" == true ]] && ! grep -qxF -- "$template" "${CR_DIR}/.injected"; then
      entry="${template#*-}"
      printf '%s\t%s\t%s\n' "$(cr_time "$now")" "$template" "$entry" >>"${CR_DIR}/windows.tsv"
      echo "$template" >>"${CR_DIR}/.injected"
      cr_log "injected ${template}"
    fi
    if [[ -n "$records" ]]; then
      for id in ${records//,/ }; do
        if ! grep -qxF -- "${template}"$'\t'"${id}" "${CR_DIR}/targets.tsv"; then
          printf '%s\t%s\n' "$template" "$id" >>"${CR_DIR}/targets.tsv"
        fi
      done
    fi
  done <<<"$rows"
  cr_mark_lifts "$now"
}

# cr_mark_lifts <epoch> — Record the lift of every injected template that is
# no longer listed.
cr_mark_lifts() {
  local template
  while IFS= read -r template; do
    [[ -n "$template" ]] || continue
    if ! grep -qxF -- "$template" "${CR_DIR}/.present" && ! awk -F '\t' -v t="$template" '$2 == t { found = 1 } END { exit !found }' "${CR_DIR}/.lifted"; then
      printf '%s\t%s\n' "$(cr_time "$1")" "$template" >>"${CR_DIR}/.lifted"
      cr_log "lifted ${template}"
    fi
  done <"${CR_DIR}/.injected"
}

# cr_accomplished — The Workflow reads Accomplished=True.
cr_accomplished() {
  [[ "$(kubectl get workflows.chaos-mesh.org "$CR_WORKFLOW" -n "$CHAOS_RANDOM_CP_NAMESPACE" \
    -o 'jsonpath={.status.conditions[?(@.type=="Accomplished")].status}' 2>/dev/null)" == True ]]
}

# cr_delete_workflow — Delete the Workflow and wait up to CR_GONE seconds for
# its chaos objects to go; every injected template still listed is lifted
# then.
cr_delete_workflow() {
  local end left
  [[ -n "$CR_WORKFLOW" ]] || return 0
  cr_log "deleting Workflow ${CHAOS_RANDOM_CP_NAMESPACE}/${CR_WORKFLOW}; its faults lift"
  kubectl delete workflows.chaos-mesh.org "$CR_WORKFLOW" -n "$CHAOS_RANDOM_CP_NAMESPACE" --ignore-not-found \
    --wait=true --timeout="${CR_GONE}s" >/dev/null 2>&1 || true
  end=$(($(cr_now) + CR_GONE))
  while :; do
    left="$(kubectl get podchaos,networkchaos -n "$CHAOS_RANDOM_CP_NAMESPACE" \
      -l "chaos-mesh.org/workflow=${CR_WORKFLOW}" -o name 2>/dev/null)" || left="unknown"
    [[ -z "$left" ]] && break
    if [[ "$(cr_now)" -ge "$end" ]]; then
      cr_log "WARNING: chaos objects of ${CR_WORKFLOW} left after ${CR_GONE} s: $(paste -sd ' ' - <<<"$left")"
      break
    fi
    cr_sleep "$CR_POLL"
  done
  if [[ -n "$CR_DIR" && -f "${CR_DIR}/.injected" ]]; then
    : >"${CR_DIR}/.present"
    cr_mark_lifts "$(cr_now)"
  fi
}

# cr_finish_windows — Rewrite windows.tsv with the lift of each window as a
# fourth column, - when none was seen.
cr_finish_windows() {
  awk -F '\t' -v OFS='\t' -v lifts="${CR_DIR}/.lifted" '
    BEGIN { while ((getline line <lifts) > 0) { split(line, f, "\t"); lifted[f[2]] = f[1] } }
    { print $1, $2, $3, ($2 in lifted ? lifted[$2] : "-") }' "${CR_DIR}/windows.tsv" >"${CR_DIR}/windows.tsv.new"
  mv "${CR_DIR}/windows.tsv.new" "${CR_DIR}/windows.tsv"
}

# cr_fetch_soak — Fetch the soak's newest report into ${CR_DIR}/soak.
cr_fetch_soak() {
  local newest
  if ! "$CR_SOAK" report; then
    cr_log "WARNING: hack/dizzy-soak.sh report failed"
    return 1
  fi
  newest="$( (find "$CR_SOAK_OUTPUT_DIR" -mindepth 1 -maxdepth 1 -type d -name '[0-9]*T[0-9]*Z' 2>/dev/null || true) |
    awk -F / '{ print $NF }' | sort | awk -v run="$CR_RUN" '$0 >= run' | tail -n 1)"
  if [[ -z "$newest" ]]; then
    cr_log "WARNING: no soak report of this run under ${CR_SOAK_OUTPUT_DIR}"
    return 1
  fi
  rm -rf "${CR_DIR}/soak"
  cp -R "${CR_SOAK_OUTPUT_DIR}/${newest}" "${CR_DIR}/soak"
}

# ---------------------------------------------------------------------------
# run: the checks
# ---------------------------------------------------------------------------

# cr_check <name> <pass> <detail> — One check as a JSON line of
# ${CR_DIR}/checks.jsonl.
cr_check() {
  jq -cn --arg name "$1" --argjson pass "$2" --arg detail "$3" '{name: $name, pass: $pass, detail: $detail}' \
    >>"${CR_DIR}/checks.jsonl"
  if [[ "$2" == true ]]; then cr_log "check $1: pass: $3"; else cr_log "check $1: FAIL: $3"; fi
}

# cr_check_workflow <apply error> <timed out> — The Workflow was accepted,
# every drawn chaos template read AllInjected=True, and it was accomplished
# in time.
cr_check_workflow() {
  local apply_error="$1" timed_out="$2" drawn missing count
  if [[ -n "$apply_error" ]]; then
    cr_check workflow false "kubectl apply refused the Workflow: ${apply_error}"
    return
  fi
  drawn="$(jq -r '.spec.templates[] | select(.templateType == "PodChaos" or .templateType == "NetworkChaos") | .name' \
    "${CR_DIR}/workflow.json")"
  missing="$(while IFS= read -r t; do
    if [[ -n "$t" ]] && ! awk -F '\t' -v t="$t" '$2 == t { f = 1 } END { exit !f }' "${CR_DIR}/windows.tsv"; then
      echo "$t"
    fi
  done <<<"$drawn" | paste -sd ',' - | sed 's/,/, /g')"
  count="$(grep -c . <<<"$drawn" || true)"
  if [[ -n "$missing" ]]; then
    cr_check workflow false "never read AllInjected=True: ${missing}"
  elif [[ "$timed_out" == true ]]; then
    cr_check workflow false "the Workflow was not Accomplished within $((CR_WINDOW_S + CR_ACCOMPLISH_SLACK)) s"
  else
    cr_check workflow true "${count} chaos templates injected and lifted; the Workflow was Accomplished"
  fi
}

# cr_soak_checks <soak finished> — dizzy-exit, leak-check, controlplane-ready,
# error-rate and p95 as the soak's verdict.json has them.
cr_soak_checks() {
  local finished="$1" verdict="${CR_DIR}/soak/verdict.json" name row
  for name in dizzy-exit leak-check controlplane-ready error-rate p95; do
    if [[ "$name" == dizzy-exit && "$finished" != true ]]; then
      cr_check dizzy-exit false "the soak Job did not finish within ${CR_SOAK_FINISH} s of the churn end"
      continue
    fi
    if [[ ! -f "$verdict" ]]; then
      case "$name" in error-rate | p95) continue ;; esac
      cr_check "$name" false "the soak wrote no verdict.json"
      continue
    fi
    row="$(jq -c --arg n "$name" '[.checks[] | select(.name == $n)] | first // empty' "$verdict")"
    if [[ -z "$row" ]]; then
      case "$name" in error-rate | p95) continue ;; esac
      cr_check "$name" false "the soak's verdict.json has no ${name} check"
      continue
    fi
    cr_check "$name" "$(jq -r '.pass' <<<"$row")" "$(jq -r '.detail' <<<"$row")"
  done
}

# cr_exempt_pods — The <namespace>/<pod> of every target, one per line.
cr_exempt_pods() {
  awk -F '\t' 'NF >= 2 { print $2 }' "${CR_DIR}/targets.tsv" | sort -u
}

# cr_restarted_with_windows <platform dir> <exempt pods> — For every container
# of a pod outside the exempt pods whose restartCount rose during the run, its
# id, the first sample after the rise and the windows open then.
cr_restarted_with_windows() {
  EXEMPT="$2" awk -F '\t' -v windows="${CR_DIR}/windows.tsv" '
    BEGIN {
      split(ENVIRON["EXEMPT"], e, "\n")
      for (i in e) if (e[i] != "") exempt[e[i]] = 1
      while ((getline line <windows) > 0) { split(line, f, "\t"); n++; from[n] = f[1]; to[n] = f[4]; tmpl[n] = f[2] }
    }
    {
      if (($2 "/" $3) in exempt) next
      k = $4 SUBSEP $7
      if (k in last && $8 > last[k] && !(k in reported)) {
        reported[k] = 1
        open = ""
        for (i = 1; i <= n; i++) if (from[i] <= $1 && (to[i] == "-" || to[i] >= $1)) open = open (open == "" ? "" : ", ") tmpl[i]
        print $2 "/" $3 "/" $7 " by " $1 " (open: " (open == "" ? "none" : open) ")"
      }
      last[k] = $8
    }' "$1/containers.tsv" | paste -sd ';' - | sed 's/;/; /g'
}

# cr_rejudge — no-restarts, operator-leak and memory-growth again, on a copy of
# the soak's platform tables with EXEMPT_PODS set to the targets and the bounds
# of the soak's meta.json.
cr_rejudge() {
  local work exempt start percent mib skip heap goroutines result named
  if [[ ! -f "${CR_DIR}/soak/meta.json" ]]; then
    cr_check no-restarts false "the soak wrote no report to judge"
    cr_check operator-leak false "the soak wrote no report to judge"
    cr_check memory-growth false "the soak wrote no report to judge"
    return
  fi
  work="$(mktemp -d "${TMPDIR:-/tmp}/chaos-random-rejudge.XXXXXX")"
  cp -R "${CR_DIR}/soak/platform" "${work}/platform"
  exempt="$(cr_exempt_pods)"
  start="$(jq -r '.startTime' "${CR_DIR}/soak/meta.json")"
  percent="$(jq -r '.maxMemoryGrowthPercent // 10' "${CR_DIR}/soak/meta.json")"
  mib="$(jq -r '.maxMemoryGrowthMiB // 16' "${CR_DIR}/soak/meta.json")"
  skip="$(jq -r '(.memoryGrowthSkipNamespaces // ["flux-system"]) | join(" ")' "${CR_DIR}/soak/meta.json")"
  heap="$(jq -r '.maxOperatorHeapGrowthPercent // 10' "${CR_DIR}/soak/meta.json")"
  goroutines="$(jq -r '.maxOperatorGoroutineGrowth // 10' "${CR_DIR}/soak/meta.json")"
  # shellcheck disable=SC1090,SC2034 # the runner reads the seven after sourcing
  result="$(cd "$work" && (
    source "$CR_SOAK_RUNNER"
    RUN_START="$start"
    EXEMPT_PODS="$exempt"
    DIZZY_SOAK_MAX_MEMORY_GROWTH_PERCENT="$percent"
    DIZZY_SOAK_MAX_MEMORY_GROWTH_MIB="$mib"
    DIZZY_SOAK_MEMORY_GROWTH_SKIP_NAMESPACES="$skip"
    DIZZY_SOAK_MAX_OPERATOR_HEAP_GROWTH_PERCENT="$heap"
    DIZZY_SOAK_MAX_OPERATOR_GOROUTINE_GROWTH="$goroutines"
    check_no_restarts
    derive_operators_summary
    check_operator_leak
    derive_memory_growth
    check_memory_growth
  ))"
  cp "${work}/platform/memory-growth.tsv" "${CR_DIR}/memory-growth.tsv" 2>/dev/null || true
  local line name pass detail
  while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    name="$(jq -r '.name' <<<"$line")"
    pass="$(jq -r '.pass' <<<"$line")"
    detail="$(jq -r '.detail' <<<"$line")"
    if [[ "$name" == no-restarts && "$pass" == false ]]; then
      named="$(cr_restarted_with_windows "${work}/platform" "$exempt")"
      [[ -n "$named" ]] && detail="${detail}; ${named}"
    fi
    cr_check "$name" "$pass" "$detail"
  done <<<"$result"
  rm -rf "$work"
}

# cr_errors_at <epoch> — sum(dizzy_operation_errors_total) at the time, from
# the dizzy VictoriaMetrics through the API server's service proxy. An empty
# result is 0; a failed query prints its message and returns 1.
cr_errors_at() {
  local out
  if ! out="$(kubectl get --raw "${CR_VM_QUERY}?query=sum%28dizzy_operation_errors_total%29&time=$1" 2>&1)"; then
    echo "the query at $(cr_time "$1") failed: $(cr_last_line "$out")"
    return 1
  fi
  if ! jq -e . >/dev/null 2>&1 <<<"$out"; then
    echo "the query at $(cr_time "$1") answered no JSON: $(cr_first_line "$out")"
    return 1
  fi
  if [[ "$(jq -r '.status' <<<"$out")" != success ]]; then
    echo "the query at $(cr_time "$1") answered status $(jq -r '.status' <<<"$out"): $(jq -r '.error // .errorType // "no message"' <<<"$out")"
    return 1
  fi
  jq -r '(.data.result[0].value[1] // "0") | tonumber | floor' <<<"$out"
}

# cr_recovery_of <entry> — The recovery of a catalogue entry in seconds.
cr_recovery_of() {
  yq -r ".[] | select(.name == \"$1\") | .recovery" "$CR_CATALOGUE" | sed 's/s$//'
}

# cr_check_quiet_tail <churn end epoch> — No failed operation between the
# last lift plus its recovery and the churn end.
cr_check_quiet_tail() {
  local churn_end="$1" last_lift=0 from=0 template entry lifted at recovery v1 v2
  while IFS=$'\t' read -r _ template entry lifted; do
    [[ -n "$template" ]] || continue
    [[ "$lifted" == "-" ]] && continue
    at="$(cr_epoch "$lifted")"
    recovery="$(cr_recovery_of "$entry")"
    if [[ $((at + ${recovery:-60})) -gt "$from" ]]; then
      from=$((at + ${recovery:-60}))
      last_lift="$at"
    fi
  done <"${CR_DIR}/windows.tsv"
  if [[ "$from" -eq 0 ]]; then
    cr_check quiet-tail false "no fault was lifted, so there is no tail to judge"
    return
  fi
  if [[ "$from" -ge "$churn_end" ]]; then
    cr_check quiet-tail false "the last recovery ends at $(cr_time "$from"), not before the churn end $(cr_time "$churn_end")"
    return
  fi
  if ! v1="$(cr_errors_at "$from")"; then
    cr_check quiet-tail false "$v1"
    return
  fi
  if ! v2="$(cr_errors_at "$churn_end")"; then
    cr_check quiet-tail false "$v2"
    return
  fi
  if [[ $((v2 - v1)) -gt 0 ]]; then
    cr_check quiet-tail false "dizzy counted $((v2 - v1)) failed operation(s) between $(cr_time "$from") (the last lift $(cr_time "$last_lift") plus its recovery) and the churn end $(cr_time "$churn_end")"
  else
    cr_check quiet-tail true "no failed operation between $(cr_time "$from") and the churn end $(cr_time "$churn_end")"
  fi
}

# cr_check_consistency — placement-audit on deploy/<cp>-nova and ovn-db-sync
# on <cp>-neutron, the functions of tests/e2e-chaos/lib/consistency.sh.
cr_check_consistency() {
  local out rc
  rc=0
  # shellcheck disable=SC1090,SC2034 # NAMESPACE is read by the sourced checks
  out="$(NAMESPACE="$CHAOS_RANDOM_CP_NAMESPACE"; source "$CR_CONSISTENCY"; placement_audit "${CHAOS_RANDOM_CP_NAME}-nova" 2>&1)" || rc=$?
  printf '%s\n' "$out" >"${CR_DIR}/placement-audit.log"
  if [[ "$rc" -eq 0 ]] && grep -q '^CONSISTENCY-OK placement-audit' <<<"$out"; then
    cr_check placement-audit true "nova-manage placement audit found no orphaned allocation"
  else
    cr_check placement-audit false "$(grep -m1 -E '^(CONSISTENCY-FAIL|FAIL:)' <<<"$out" || cr_last_line "$out")"
  fi
  rc=0
  # shellcheck disable=SC1090,SC2034
  out="$(NAMESPACE="$CHAOS_RANDOM_CP_NAMESPACE"; source "$CR_CONSISTENCY"; ovn_db_sync "random-$(tr '[:upper:]' '[:lower:]' <<<"$CR_RUN")" "${CHAOS_RANDOM_CP_NAME}-neutron" 2>&1)" || rc=$?
  printf '%s\n' "$out" >"${CR_DIR}/ovn-db-sync.log"
  if [[ "$rc" -eq 0 ]] && grep -q '^CONSISTENCY-OK ovn-db-sync' <<<"$out"; then
    cr_check ovn-db-sync true "the OVN db-sync in log mode found Neutron and OVN in agreement"
  else
    cr_check ovn-db-sync false "$(grep -m1 -E '^(CONSISTENCY-FAIL|FAIL:)' <<<"$out" || cr_last_line "$out")"
  fi
}

# cr_stuck_job — The manifest of Job chaos-random-stuck.
cr_stuck_job() {
  # The listing prints the name, id and state of each resource outside the
  # states a steady plane leaves, and never a field of the Secret.
  local script
  script="$(cat <<'SH'
set -uo pipefail
list() {
  local what="$1" ok="$2"
  shift 2
  local out
  if ! out="$(openstack "$@" -f json 2>&1)"; then
    echo "ERROR: openstack $* failed: $(printf '%s\n' "$out" | tail -n 1)"
    exit 1
  fi
  printf '%s' "$out" | python3 -c '
import json, sys
what, ok = sys.argv[1], sys.argv[2].split(",")
rows = json.load(sys.stdin)
for row in rows:
    state = row.get("Status") or row.get("status") or ""
    name = row.get("Name") or "-"
    ident = row.get("ID") or "-"
    if state not in ok:
        print(f"STUCK {what} {name} {ident} {state}")
print(f"LISTED {what} {len(rows)}")
' "$what" "$ok"
}
list server ACTIVE,SHUTOFF,PAUSED,SUSPENDED,SHELVED_OFFLOADED server list --all-projects
list volume available,in-use volume list --all-projects
list image active,deactivated image list
echo "DONE"
SH
)"
  jq -n --arg ns "$CHAOS_RANDOM_CP_NAMESPACE" --arg image "$CR_STUCK_IMAGE" --arg script "$script" \
    --argjson deadline "$CR_STUCK_DEADLINE" '{
    apiVersion: "batch/v1", kind: "Job",
    metadata: {name: "chaos-random-stuck", namespace: $ns},
    spec: {backoffLimit: 0, activeDeadlineSeconds: $deadline, template: {spec: {
      restartPolicy: "Never", automountServiceAccountToken: false,
      securityContext: {runAsNonRoot: true, runAsUser: 42424, runAsGroup: 42424, seccompProfile: {type: "RuntimeDefault"}},
      containers: [{
        name: "stuck", image: $image, imagePullPolicy: "IfNotPresent",
        command: ["/bin/bash", "-c", $script],
        env: [{name: "OS_CLOUD", value: "admin"}, {name: "HOME", value: "/tmp"}],
        resources: {requests: {cpu: "50m", memory: "128Mi"}, limits: {memory: "512Mi"}},
        securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: ["ALL"]}},
        volumeMounts: [{name: "clouds", mountPath: "/etc/openstack", readOnly: true}, {name: "tmp", mountPath: "/tmp"}]
      }],
      volumes: [{name: "clouds", secret: {secretName: "k-orc-clouds-yaml", items: [{key: "clouds.yaml", path: "clouds.yaml"}]}},
                {name: "tmp", emptyDir: {}}]
    }}}}'
}

# cr_check_stuck_resources — Job chaos-random-stuck lists no server, volume or
# image outside the states a steady plane leaves.
cr_check_stuck_resources() {
  local ns="$CHAOS_RANDOM_CP_NAMESPACE" end state log stuck
  kubectl delete job chaos-random-stuck -n "$ns" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  if ! cr_stuck_job | kubectl create -f - >/dev/null; then
    cr_check stuck-resources false "cannot create Job ${ns}/chaos-random-stuck (kubectl's error is above)"
    return
  fi
  end=$(($(cr_now) + CR_STUCK_DEADLINE + 30))
  state=""
  while [[ "$(cr_now)" -lt "$end" ]]; do
    state="$(kubectl get job chaos-random-stuck -n "$ns" \
      -o 'jsonpath={.status.conditions[?(@.status=="True")].type}' 2>/dev/null || true)"
    case " ${state} " in
      *" Complete "* | *" Failed "*) break ;;
    esac
    cr_sleep "$CR_POLL"
  done
  log="$(kubectl logs job/chaos-random-stuck -n "$ns" --tail=-1 2>&1 || true)"
  printf '%s\n' "$log" >"${CR_DIR}/stuck-resources.log"
  kubectl delete job chaos-random-stuck -n "$ns" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  case " ${state} " in
    *" Complete "*) ;;
    *" Failed "*)
      cr_check stuck-resources false "Job chaos-random-stuck failed: $(cr_last_line "$log")"
      return
      ;;
    *)
      cr_check stuck-resources false "Job chaos-random-stuck did not finish within $((CR_STUCK_DEADLINE + 30)) s: $(cr_last_line "$log")"
      return
      ;;
  esac
  if ! grep -qx DONE <<<"$log"; then
    cr_check stuck-resources false "the listing did not finish: $(cr_last_line "$log")"
    return
  fi
  stuck="$(sed -n 's/^STUCK //p' <<<"$log" | paste -sd ';' - | sed 's/;/; /g')"
  if [[ -n "$stuck" ]]; then
    cr_check stuck-resources false "$stuck"
  else
    cr_check stuck-resources true "$(sed -n 's/^LISTED \([a-z]*\) \([0-9]*\)$/\2 \1s/p' <<<"$log" | paste -sd ',' - | sed 's/,/, /g'), each in a steady state"
  fi
}

# cr_check_stuck_objects — No PodChaos, NetworkChaos or WorkflowNode is left
# in the namespace, no pod of a sampled namespace has terminated for more than
# CR_STUCK_POD_SECONDS, and no object of the soak's condition kinds carries a
# deletionTimestamp.
cr_check_stuck_objects() {
  local ns="$CHAOS_RANDOM_CP_NAMESPACE" problems="" out kinds kind namespaces excluded now
  if out="$(kubectl get podchaos,networkchaos,workflownodes.chaos-mesh.org -n "$ns" -o name 2>&1)"; then
    [[ -n "$out" ]] && problems="${problems}left in ${ns}: $(paste -sd ' ' - <<<"$out"); "
  else
    problems="${problems}cannot list the chaos objects: $(cr_first_line "$out"); "
  fi
  namespaces="$(jq -c '.namespaces // null' "${CR_DIR}/soak/meta.json" 2>/dev/null || echo null)"
  [[ -n "$namespaces" ]] || namespaces=null
  # The runner's constants; sourcing it only defines them and its functions.
  # shellcheck disable=SC1090
  excluded="$(source "$CR_SOAK_RUNNER" && echo "$EXCLUDED_NAMESPACES")"
  # shellcheck disable=SC1090
  kinds="$(source "$CR_SOAK_RUNNER" && echo "$CONDITION_KINDS")"
  now="$(cr_now)"
  if out="$(kubectl get pods -A -o json 2>&1)"; then
    out="$(jq -r --argjson listed "$namespaces" --arg excluded "$excluded" --argjson now "$now" --argjson bound "$CR_STUCK_POD_SECONDS" '
      ($excluded | split(" ")) as $ex
      | .items[] | select(.metadata.deletionTimestamp != null)
      | select(if $listed != null then (.metadata.namespace | IN($listed[])) else (.metadata.namespace | IN($ex[]) | not) end)
      | select($now - (.metadata.deletionTimestamp | fromdateiso8601) > $bound)
      | "\(.metadata.namespace)/\(.metadata.name) terminating since \(.metadata.deletionTimestamp)"' <<<"$out" | paste -sd ';' - | sed 's/;/; /g')"
    [[ -n "$out" ]] && problems="${problems}${out}; "
  else
    problems="${problems}cannot list the pods: $(cr_first_line "$out"); "
  fi
  for kind in $kinds; do
    if out="$(kubectl get "$kind" -A -o json 2>&1)"; then
      out="$(jq -r --arg kind "$kind" '.items[] | select(.metadata.deletionTimestamp != null)
        | "\($kind) \(.metadata.namespace // "-")/\(.metadata.name) deleting since \(.metadata.deletionTimestamp)"' <<<"$out" |
        paste -sd ';' - | sed 's/;/; /g')"
      [[ -n "$out" ]] && problems="${problems}${out}; "
    else
      case "$out" in
        *"doesn't have a resource type"* | *"no matches for kind"*) ;;
        *) problems="${problems}cannot list ${kind}: $(cr_first_line "$out"); " ;;
      esac
    fi
  done
  if [[ -n "$problems" ]]; then
    cr_check stuck-objects false "${problems%; }"
  else
    cr_check stuck-objects true "no chaos object or WorkflowNode left, no pod terminating for more than ${CR_STUCK_POD_SECONDS} s, no service object deleting"
  fi
}

# ---------------------------------------------------------------------------
# run: the outputs
# ---------------------------------------------------------------------------

# cr_xml <text> — The text with the five XML special characters escaped.
cr_xml() {
  sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/"/\&quot;/g' -e "s/'/\&apos;/g" <<<"$1"
}

# cr_write_outputs — random-verdict.json, report.md and the JUnit file from
# checks.jsonl; echoes the verdict.
cr_write_outputs() {
  local seed verdict
  seed="$(jq -r '.seed' "${CR_DIR}/plan.json" 2>/dev/null || echo "${CHAOS_RANDOM_SEED:-unknown}")"
  touch "${CR_DIR}/checks.jsonl"
  jq -s --arg run "$CR_RUN" --arg env "$CHAOS_RANDOM_ENV" --arg seed "$seed" --arg window "$CHAOS_RANDOM_WINDOW" '
    {verdict: (if length > 0 and all(.[]; .pass) then "PASS" else "FAIL" end), run: $run, environment: $env,
     seed: ($seed | tonumber? // $seed), window: $window, checks: .}' "${CR_DIR}/checks.jsonl" >"${CR_DIR}/random-verdict.json"
  verdict="$(jq -r '.verdict' "${CR_DIR}/random-verdict.json")"

  mkdir -p "$CR_REPORTS_DIR"
  {
    echo '<?xml version="1.0" encoding="UTF-8"?>'
    echo '<testsuites name="chaos-random">'
    local line name pass detail
    while IFS= read -r line; do
      [[ -n "$line" ]] || continue
      name="$(jq -r '.name' <<<"$line")"
      pass="$(jq -r '.pass' <<<"$line")"
      detail="$(jq -r '.detail | gsub("\n"; " ")' <<<"$line")"
      if [[ "$pass" == true ]]; then
        echo "  <testsuite name=\"tests/chaos-random/random-${name}\" tests=\"1\" failures=\"0\">"
        echo "    <testcase name=\"random-${name}\" classname=\"chaos-random\"/>"
      else
        echo "  <testsuite name=\"tests/chaos-random/random-${name}\" tests=\"1\" failures=\"1\">"
        echo "    <testcase name=\"random-${name}\" classname=\"chaos-random\">"
        echo "      <failure message=\"$(cr_xml "${detail} (seed ${seed})")\"/>"
        echo "    </testcase>"
      fi
      echo "  </testsuite>"
    done <"${CR_DIR}/checks.jsonl"
    echo '</testsuites>'
  } >"${CR_REPORTS_DIR}/chaos-random-junit.xml"

  cr_write_report "$seed" "$verdict"
  echo "$verdict"
}

# cr_window_errors — One table row per window: dizzy's failed operations
# between its injection and its lift plus recovery, from two instant queries.
cr_window_errors() {
  local injected template entry lifted from to recovery v1 v2 shown
  while IFS=$'\t' read -r injected template entry lifted; do
    [[ -n "$template" ]] || continue
    shown="—"
    if [[ "$lifted" != "-" ]]; then
      from="$(cr_epoch "$injected")"
      recovery="$(cr_recovery_of "$entry")"
      to=$(($(cr_epoch "$lifted") + ${recovery:-60}))
      if v1="$(cr_errors_at "$from")" && v2="$(cr_errors_at "$to")"; then
        shown="$((v2 - v1))"
      else
        shown="query failed"
      fi
    fi
    printf '| %s | %s | %s | %s | %s | %s |\n' "$template" "$entry" "$injected" "$lifted" \
      "$(awk -F '\t' -v t="$template" '$1 == t { printf "%s%s", (n++ ? ", " : ""), $2 }' "${CR_DIR}/targets.tsv")" "$shown"
  done <"${CR_DIR}/windows.tsv"
}

# cr_write_report <seed> <verdict> — report.md of the run.
cr_write_report() {
  {
    printf '# Random chaos run %s\n\n' "$CR_RUN"
    printf 'Verdict: **%s**. Environment `%s`, seed `%s`, window `%s`, ControlPlane `%s/%s`.\n\n' "$2" \
      "$CHAOS_RANDOM_ENV" "$1" "$CHAOS_RANDOM_WINDOW" "$CHAOS_RANDOM_CP_NAMESPACE" "$CHAOS_RANDOM_CP_NAME"
    printf 'Replay the draw with `CHAOS_RANDOM_ENV=%s CHAOS_RANDOM_SEED=%s CHAOS_RANDOM_WINDOW=%s make chaos-random-draw`.\n\n' \
      "$CHAOS_RANDOM_ENV" "$1" "$CHAOS_RANDOM_WINDOW"
    printf '| Check | Result | Detail |\n| --- | --- | --- |\n'
    jq -r '"| \(.name) | \(if .pass then "pass" else "FAIL" end) | \(.detail | gsub("\\|"; "\\|")) |"' "${CR_DIR}/checks.jsonl"
    printf '\n## Plan\n\n| Step | Offset (s) | Entries | Length (s) | Pause (s) |\n| --- | --- | --- | --- | --- |\n'
    jq -r '.steps[]? | "| \(.index) | \(.offset) | \(.entries | join(", ")) | \(.length) | \(.pause) |"' "${CR_DIR}/plan.json" 2>/dev/null || true
    printf '\n## Fault windows\n\nThe failed operations of dizzy per window, between the injection and the lift plus the recovery, are reported and not judged: an overlap of two random faults has no budget of its own. `quiet-tail` judges the churn after the last recovery.\n\n'
    printf '| Template | Entry | Injected | Lifted | Targets | dizzy errors |\n| --- | --- | --- | --- | --- | --- |\n'
    cr_window_errors
    if [[ -f "${CR_DIR}/soak/report.md" ]]; then
      printf '\nThe soak'"'"'s own report is soak/report.md; its verdict.json judges every pod, the targets included.\n'
    fi
  } >"${CR_DIR}/report.md"
}

# ---------------------------------------------------------------------------
# run
# ---------------------------------------------------------------------------

# cr_on_signal — The TERM and INT trap of run: lift the faults, stop the soak,
# write what the run has, exit 1.
cr_on_signal() {
  trap - TERM INT
  cr_log "stopped by a signal: deleting the Workflow, stopping the soak"
  cr_delete_workflow
  if [[ -n "$CR_SOAK_STARTED" ]]; then
    "$CR_SOAK" stop || true
    cr_fetch_soak || true
  fi
  cr_restore_db_sync
  if [[ -n "$CR_DIR" && -d "$CR_DIR" ]]; then
    [[ -f "${CR_DIR}/windows.tsv" ]] && cr_finish_windows
    cr_check workflow false "the run was stopped by a signal before it finished"
    cr_write_outputs >/dev/null || true
  fi
  exit 1
}

# cr_restore_db_sync — Remove spec.ovnDBSync again when run set it.
cr_restore_db_sync() {
  if [[ -n "$CR_DB_SYNC_PATCHED" ]]; then
    kubectl patch neutron "${CHAOS_RANDOM_CP_NAME}-neutron" -n "$CHAOS_RANDOM_CP_NAMESPACE" --type json \
      -p '[{"op":"remove","path":"/spec/ovnDBSync"}]' >/dev/null || cr_log "WARNING: cannot remove spec.ovnDBSync from ${CHAOS_RANDOM_CP_NAME}-neutron"
    CR_DB_SYNC_PATCHED=""
  fi
}

cr_cmd_run() {
  cr_check_settings
  local ns="$CHAOS_RANDOM_CP_NAMESPACE" seed apply_error="" timed_out=false finished=true
  local soak_start churn_end deadline state out

  # 1. Preflight.
  cr_preflight || exit 1

  # 2. The draw.
  CR_RUN="$(date -u '+%Y%m%dT%H%M%SZ')"
  seed="${CHAOS_RANDOM_SEED:-$(cr_now)}"
  CHAOS_RANDOM_SEED="$seed"
  CR_WORKFLOW="chaos-random-$(tr '[:upper:]' '[:lower:]' <<<"$CR_RUN")"
  CR_DIR="${CR_OUTPUT_DIR}/${CR_RUN}"
  mkdir -p "$CR_DIR"
  : >"${CR_DIR}/windows.tsv"
  : >"${CR_DIR}/targets.tsv"
  : >"${CR_DIR}/checks.jsonl"
  : >"${CR_DIR}/.injected"
  : >"${CR_DIR}/.lifted"
  : >"${CR_DIR}/.nodes"
  : >"${CR_DIR}/.present"
  cr_log "run ${CR_RUN}: environment ${CHAOS_RANDOM_ENV}, seed ${seed}, window ${CHAOS_RANDOM_WINDOW}, Workflow ${ns}/${CR_WORKFLOW}, output ${CR_DIR}"
  if ! cr_draw "$seed" "${CR_DIR}/plan.json" "$CR_WORKFLOW" "${CR_DIR}/workflow.json"; then
    cr_fail "the draw failed (draw.py's message is above)"
  fi
  trap cr_on_signal TERM INT

  # 3. The OVN db-sync CronJob the ovn-db-sync check runs.
  if [[ -z "$(kubectl get neutron "${CHAOS_RANDOM_CP_NAME}-neutron" -n "$ns" -o 'jsonpath={.spec.ovnDBSync}' 2>/dev/null)" ]]; then
    kubectl patch neutron "${CHAOS_RANDOM_CP_NAME}-neutron" -n "$ns" --type merge \
      -p '{"spec":{"ovnDBSync":{"syncMode":"log","suspend":true}}}' >/dev/null
    CR_DB_SYNC_PATCHED=1
    : >"${CR_DIR}/ovn-db-sync-patched"
  fi

  # 4. The soak, for the warm-up, the window and the tail.
  local soak_env=(DIZZY_SOAK_DURATION="$((CR_WARMUP_S + CR_WINDOW_S + CR_TAIL_S))s"
    DIZZY_ARGS="--seed ${seed}${DIZZY_ARGS:+ ${DIZZY_ARGS}}"
    DIZZY_CP_NAME="$CHAOS_RANDOM_CP_NAME" DIZZY_CP_NAMESPACE="$ns")
  if [[ "$CHAOS_RANDOM_ENV" == ci ]]; then
    soak_env+=(DIZZY_SCENARIO="${DIZZY_SCENARIO:-${CR_KIND_DIR}/scenario.yaml}")
  fi
  if ! env "${soak_env[@]}" "$CR_SOAK" start; then
    cr_restore_db_sync
    cr_fail "the soak did not start (hack/dizzy-soak.sh's error is above)"
  fi
  CR_SOAK_STARTED=1
  soak_start="$(cr_now)"
  churn_end=$((soak_start + CR_WARMUP_S + CR_WINDOW_S + CR_TAIL_S))
  echo "$(cr_time "$churn_end")" >"${CR_DIR}/churn-end"

  # 5. The warm-up, then the Workflow.
  cr_log "warm-up: ${CR_WARMUP_S} s of churn before the first fault"
  cr_sleep "$CR_WARMUP_S"
  if ! out="$(kubectl apply -f "${CR_DIR}/workflow.json" 2>&1)"; then
    apply_error="$(cr_last_line "$out")"
    cr_log "ERROR: kubectl apply refused the Workflow: ${apply_error}; stopping the soak"
    "$CR_SOAK" stop || true
    finished=true
  else
    # 6. The windows and the targets, until the Workflow is accomplished.
    deadline=$(($(cr_now) + CR_WINDOW_S + CR_ACCOMPLISH_SLACK))
    while :; do
      cr_observe
      if cr_accomplished; then
        cr_observe
        break
      fi
      if [[ "$(cr_now)" -ge "$deadline" ]]; then
        timed_out=true
        break
      fi
      cr_sleep "$CR_POLL"
    done
    # 7. The Workflow goes, and its faults with it.
    cr_delete_workflow

    # 8. The soak's end, its report.
    local now
    now="$(cr_now)"
    if [[ "$now" -lt "$churn_end" ]]; then
      cr_log "waiting $((churn_end - now)) s for the churn end $(cr_time "$churn_end")"
      cr_sleep $((churn_end - now))
    fi
    deadline=$((churn_end + CR_SOAK_FINISH))
    while :; do
      state="$(cr_soak_state)"
      case "$state" in complete | failed) break ;; esac
      if [[ "$(cr_now)" -ge "$deadline" ]]; then
        finished=false
        break
      fi
      cr_sleep "$CR_POLL"
    done
    if [[ "$finished" == true ]]; then
      cr_log "the soak's Job is ${state}"
    else
      cr_log "ERROR: the soak Job did not finish within ${CR_SOAK_FINISH} s of the churn end; stopping it"
      "$CR_SOAK" stop || true
    fi
  fi
  cr_fetch_soak || true
  CR_SOAK_STARTED=""
  cr_finish_windows

  # 9. The checks.
  cr_check_workflow "$apply_error" "$timed_out"
  cr_soak_checks "$finished"
  cr_rejudge
  cr_check_quiet_tail "$churn_end"
  cr_check_consistency
  cr_check_stuck_resources
  cr_check_stuck_objects

  # 10. spec.ovnDBSync as it was.
  cr_restore_db_sync
  trap - TERM INT

  # 11. and 12. The outputs and the verdict.
  local verdict
  verdict="$(cr_write_outputs)"
  cr_log "verdict ${verdict}: ${CR_DIR}/report.md"
  rm -f "${CR_DIR}/.injected" "${CR_DIR}/.lifted" "${CR_DIR}/.nodes" "${CR_DIR}/.present"
  [[ "$verdict" == PASS ]]
}

# ---------------------------------------------------------------------------
# report
# ---------------------------------------------------------------------------
cr_cmd_report() {
  local run="${1:-}"
  if [[ -z "$run" ]]; then
    run="$( (find "$CR_OUTPUT_DIR" -mindepth 1 -maxdepth 1 -type d -name '[0-9]*T[0-9]*Z' 2>/dev/null || true) |
      awk -F / '{ print $NF }' | sort | tail -n 1)"
    [[ -n "$run" ]] || cr_fail "no random run under ${CR_OUTPUT_DIR}"
  fi
  [[ -f "${CR_OUTPUT_DIR}/${run}/report.md" ]] || cr_fail "no report.md of run ${run} under ${CR_OUTPUT_DIR}"
  cat "${CR_OUTPUT_DIR}/${run}/report.md"
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------
cr_main() {
  case "${1:-}" in
    draw) cr_cmd_draw ;;
    up) cr_cmd_up ;;
    preflight) cr_cmd_preflight ;;
    run) cr_cmd_run ;;
    report) cr_cmd_report "${2:-}" ;;
    *) cr_usage ;;
  esac
}

# Run cr_main only when executed directly so unit tests (tests/unit/hack/) can
# source this script and exercise individual functions.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  cr_main "$@"
fi
