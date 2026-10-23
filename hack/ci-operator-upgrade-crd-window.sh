#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-operator-upgrade-crd-window.sh — Check the CRs of an operator upgrade
# suite while the new CRDs already run and the released operator still does.
#
# An operator upgrade applies the new CRDs first and rolls the operator second
# (Flux's `crds: CreateReplace` takes the same order). In between, the released
# controller and its webhooks serve the CRs under a schema they were not built
# for. A suite calls this script after it captured its baseline and before it
# runs `helm upgrade`. The script applies the CRDs of the in-repo chart
# server-side, waits until they are Established, and checks every CR in CRS:
#
#   (a) it reads back with every spec value the released CRDs stored; a new
#       default is fine, a pruned or rewritten value fails;
#   (b) an unchanged update of it is admitted and keeps metadata.generation;
#   (c) a create of its stored spec is admitted (a server-side dry run).
#
# It then patches the first CR with PROBE_PATCH and waits until the released
# controller has reconciled that generation to READY_CONDITION=True, which is
# the status write a new status schema can reject. Guards before and after
# prove that the operator Deployment did not roll meanwhile.
#
# Kubernetes 1.33 and later ratchet CRD validation: an unchanged update is
# admitted even when the CR violates a new required field, enum, maximum or
# type, and a property the new schema drops is pruned without an error. Check
# (c) validates the whole stored spec without ratcheting, and check (a) sees
# the pruned value.
#
# Required env vars:
#   OPERATOR    — operator name (keystone, c5c3, ...). The CRDs come from
#                 operators/<op>/helm/<op>-operator/crds/, the controller is
#                 deployment/<op>-operator.
#   CRS         — space-separated <resource>/<name> entries in CR_NAMESPACE,
#                 e.g. keystones.keystone.openstack.c5c3.io/keystone-op-upgrade.
#                 The first entry gets the probe.
#   PROBE_PATCH — a JSON merge patch (an object) that advances
#                 metadata.generation of the first CR.
#
# Optional env vars:
#   OPERATOR_NAMESPACE — namespace of the operator Deployment
#                        (default: <op>-system).
#   CR_NAMESPACE       — namespace of the CRs (default: openstack).
#   READY_CONDITION    — condition the probe waits for (default: Ready).
#   PROBE_TIMEOUT      — seconds the probe waits (default: 120).
#   DRY_RUN_CREATE     — true or false (default: true). false skips check (c)
#                        for every CR, for a webhook that rejects a second CR
#                        of its kind on create (the c5c3 ControlPlane).
#   ALLOW_SPEC_CHANGES — space-separated spec paths, written as check (a)
#                        prints them (spec.fernet.maxActiveKeys,
#                        spec.a[0].b), whose value may change under the new
#                        CRDs. A listed path that kept its value on every CR
#                        fails the run, so a stale entry cannot hide a later
#                        change.
#   POLL_INTERVAL      — seconds between update retries and probe reads
#                        (default: 2).
#
# Usage:
#   OPERATOR=keystone \
#   CRS=keystones.keystone.openstack.c5c3.io/keystone-op-upgrade \
#   PROBE_PATCH='{"spec":{"fernet":{"suspend":true}}}' \
#     hack/ci-operator-upgrade-crd-window.sh
#
# Prints OK:, FAIL:, SKIP:, ALLOWED: and crd/ lines on stdout.
#
# Exit codes:
#   0 — every check passed
#   1 — a window check failed; the FAIL: line names it
#   2 — invalid input, a precondition that cannot be read, or a PROBE_PATCH
#       that does not advance metadata.generation (an ::error:: line on
#       stderr)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

# ---------------------------------------------------------------------------
# die — invalid input or an unreadable precondition: annotate and exit 2.
# fail — a window check failed: print the FAIL: line and exit 1.
# ---------------------------------------------------------------------------
die() {
  echo "::error::$*" >&2
  exit 2
}

fail() {
  echo "FAIL: $*"
  exit 1
}

# require <var> <hint> — the variable must hold more than whitespace.
require() {
  local value="${!1}"
  if [[ -z "${value//[[:space:]]/}" ]]; then
    echo "::error::$1 must be set ($2)" >&2
    exit 2
  fi
}

# ---------------------------------------------------------------------------
# 0. Inputs
# ---------------------------------------------------------------------------
OPERATOR="${OPERATOR:-}"
CRS="${CRS:-}"
PROBE_PATCH="${PROBE_PATCH:-}"
CR_NAMESPACE="${CR_NAMESPACE:-openstack}"
READY_CONDITION="${READY_CONDITION:-Ready}"
PROBE_TIMEOUT="${PROBE_TIMEOUT:-120}"
DRY_RUN_CREATE="${DRY_RUN_CREATE:-true}"
ALLOW_SPEC_CHANGES="${ALLOW_SPEC_CHANGES:-}"
POLL_INTERVAL="${POLL_INTERVAL:-2}"

command -v jq >/dev/null 2>&1 || die "jq is required"

require OPERATOR "e.g. keystone"
require CRS "space-separated <resource>/<name> entries"
require PROBE_PATCH "a JSON merge patch that advances metadata.generation of the first CR"

if [[ ! "${OPERATOR}" =~ ^[a-z0-9]+$ ]]; then
  die "OPERATOR '${OPERATOR}' must match ^[a-z0-9]+\$"
fi
OPERATOR_NAMESPACE="${OPERATOR_NAMESPACE:-${OPERATOR}-system}"

crs=()
read -r -d '' -a crs <<<"${CRS}" || true
for cr in "${crs[@]}"; do
  if [[ ! "${cr}" =~ ^[a-z0-9.-]+/[a-z0-9.-]+$ ]]; then
    die "CRS entry '${cr}' is not <resource>/<name>"
  fi
done

if ! jq -e 'type == "object"' >/dev/null 2>&1 <<<"${PROBE_PATCH}"; then
  die "PROBE_PATCH must be a JSON object (a merge patch), got: ${PROBE_PATCH}"
fi
case "${DRY_RUN_CREATE}" in
  true | false) ;;
  *) die "DRY_RUN_CREATE must be true or false, got '${DRY_RUN_CREATE}'" ;;
esac
if [[ ! "${PROBE_TIMEOUT}" =~ ^[1-9][0-9]*$ ]]; then
  die "PROBE_TIMEOUT must be a positive integer, got '${PROBE_TIMEOUT}'"
fi
if [[ ! "${POLL_INTERVAL}" =~ ^[0-9]+$ ]]; then
  die "POLL_INTERVAL must be a non-negative integer, got '${POLL_INTERVAL}'"
fi

allowed=()
read -r -d '' -a allowed <<<"${ALLOW_SPEC_CHANGES}" || true

CRD_DIR="operators/${OPERATOR}/helm/${OPERATOR}-operator/crds"
shopt -s nullglob
crd_files=("${CRD_DIR}"/*.yaml)
shopt -u nullglob
if [[ "${#crd_files[@]}" -eq 0 ]]; then
  die "no CRDs under ${CRD_DIR}"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
err="${tmp}/err"

# ---------------------------------------------------------------------------
# read_deploy — sets deploy_image (the manager container) and
# deploy_generation from the operator Deployment.
# ---------------------------------------------------------------------------
read_deploy() {
  local file="${tmp}/deploy.json"
  if ! kubectl get deployment "${OPERATOR}-operator" -n "${OPERATOR_NAMESPACE}" -o json \
    >"${file}" 2>"${err}"; then
    die "cannot read deployment/${OPERATOR}-operator in ${OPERATOR_NAMESPACE}: $(cat "${err}")"
  fi
  deploy_image="$(jq -r '[(.spec.template.spec.containers // [])[]
      | select(.name == "manager") | .image] | first // ""' "${file}")" ||
    die "cannot parse deployment/${OPERATOR}-operator"
  deploy_generation="$(jq -r '.metadata.generation // ""' "${file}")"
  if [[ -z "${deploy_image}" ]]; then
    die "deployment/${OPERATOR}-operator has no manager container"
  fi
}

# read_crd_generations <file> — writes one "<name>\t<generation>" line per CRD
# of the chart that exists in the cluster. kubectl prints nothing when none
# does, a bare object when the directory holds a single file, and a List
# otherwise (a single match included).
read_crd_generations() {
  if ! kubectl get -f "${CRD_DIR}" --ignore-not-found -o json >"${tmp}/crds.json" 2>"${err}"; then
    die "cannot read the CRDs of ${CRD_DIR}: $(cat "${err}")"
  fi
  jq -r '(.items // [.])[] | "\(.metadata.name)\t\(.metadata.generation)"' \
    "${tmp}/crds.json" >"$1" || die "cannot parse the CRDs of ${CRD_DIR}"
}

# print_warnings <cr> — repeats the API server's Warning: lines of the last
# kubectl call.
print_warnings() {
  local line
  while IFS= read -r line; do
    case "${line}" in
      Warning:*) echo "API server warning on $1: ${line}" ;;
    esac
  done <"${err}"
}

is_allowed() {
  local path
  for path in ${allowed[@]+"${allowed[@]}"}; do
    [[ "${path}" == "$1" ]] && return 0
  done
  return 1
}

# ---------------------------------------------------------------------------
# 1. Guard: the released operator still runs
# ---------------------------------------------------------------------------
read_deploy
image_before="${deploy_image}"
generation_before="${deploy_generation}"
if [[ "${image_before}" == *:dev ]]; then
  fail "deployment/${OPERATOR}-operator already runs ${image_before}; the CRD window must run before the helm upgrade"
fi
echo "OK: deployment/${OPERATOR}-operator runs ${image_before}; the CRD window starts"

# ---------------------------------------------------------------------------
# 2. Snapshot every CR under the released CRDs
# ---------------------------------------------------------------------------
for i in "${!crs[@]}"; do
  if ! kubectl get "${crs[$i]}" -n "${CR_NAMESPACE}" -o json >"${tmp}/${i}.before.json" 2>"${err}"; then
    die "cannot read ${crs[$i]} before the CRD apply: $(cat "${err}")"
  fi
done

# ---------------------------------------------------------------------------
# 3. Apply the new CRDs
# ---------------------------------------------------------------------------
# Server-side, as hack/ci-deploy-operator.sh installs them: client-side apply
# keeps the whole CRD in an annotation capped at 262,144 bytes.
read_crd_generations "${tmp}/crds.before"
if ! kubectl apply --server-side --force-conflicts -f "${CRD_DIR}" >/dev/null 2>"${err}"; then
  fail "the API server rejected the new CRDs: $(cat "${err}")"
fi
if ! kubectl wait --for condition=Established -f "${CRD_DIR}" --timeout=60s >/dev/null 2>"${err}"; then
  fail "the new CRDs are not Established within 60s: $(cat "${err}")"
fi
read_crd_generations "${tmp}/crds.after"
while IFS=$'\t' read -r name generation; do
  previous="$(awk -F '\t' -v n="${name}" '$1 == n {print $2}' "${tmp}/crds.before")"
  if [[ -z "${previous}" ]]; then
    echo "crd/${name}: new in this chart (generation ${generation})"
  elif [[ "${previous}" == "${generation}" ]]; then
    echo "crd/${name}: generation ${generation} (unchanged; the window runs the released schema)"
  else
    echo "crd/${name}: generation ${previous} -> ${generation} (schema changed)"
  fi
done <"${tmp}/crds.after"

# ---------------------------------------------------------------------------
# 4. Check every CR
# ---------------------------------------------------------------------------
# changed_paths <before.json> <after.json> — one "<path>\t<before>\t<after>"
# line per leaf value of the spec in <before> that <after> lost or changed. A
# leaf is a scalar (false and null included) or an empty array or object.
# Values are JSON; a lost one reads "absent". Paths print as spec.a.b[0].c.
changed_paths() {
  # paths(f) keeps a path only where f is truthy, so the leaf test returns a
  # boolean instead of the value: paths(scalars) would drop false and null.
  # shellcheck disable=SC2016 # $vars below are jq variables, not shell ones
  jq -rn --slurpfile b "$1" --slurpfile a "$2" '
    def fmt: map(if type == "number" then "[\(.)]" else ".\(.)" end) | "spec" + join("");
    ($b[0].spec // {}) as $before
    | ($a[0].spec // {}) as $after
    | $before
    | paths(if type == "object" or type == "array" then length == 0 else true end) as $p
    | ($before | getpath($p)) as $old
    | (try ($after | getpath($p[:-1])
          | if type == "object" or type == "array" then has($p[-1]) else false end)
       catch false) as $present
    | (if $present then ($after | getpath($p)) else null end) as $new
    | select(($present | not) or $new != $old)
    | "\($p | fmt)\t\($old | tojson)\t\(if $present then ($new | tojson) else "absent" end)"
  '
}

used_allowances=" "
probe_g0=""

for i in "${!crs[@]}"; do
  cr="${crs[$i]}"

  # (a) Readable, with every value the released CRDs stored.
  if ! kubectl get "${cr}" -n "${CR_NAMESPACE}" -o json >"${tmp}/${i}.after.json" 2>"${err}"; then
    fail "${cr} is not readable under the new CRDs: $(cat "${err}")"
  fi
  changed_paths "${tmp}/${i}.before.json" "${tmp}/${i}.after.json" >"${tmp}/${i}.changed" ||
    die "cannot compare the spec of ${cr}"
  failed=0
  while IFS=$'\t' read -r path old new; do
    if is_allowed "${path}"; then
      echo "ALLOWED: ${cr} ${path} changed (${old} -> ${new}), listed in ALLOW_SPEC_CHANGES"
      used_allowances+="${path} "
    else
      echo "FAIL: ${cr} ${path} was ${old} under the released CRDs and is ${new} under the new ones"
      failed=$((failed + 1))
    fi
  done <"${tmp}/${i}.changed"
  if [[ "${failed}" -gt 0 ]]; then
    exit 1
  fi
  echo "OK: ${cr} keeps every spec value under the new CRDs"

  # (b) An unchanged update is admitted and leaves the spec alone. A conflict
  # with a concurrent write (the controller's status update) is retried with a
  # fresh read.
  attempt=1
  current="${tmp}/${i}.current.json"
  while :; do
    if ! kubectl get "${cr}" -n "${CR_NAMESPACE}" -o json >"${current}" 2>"${err}"; then
      fail "${cr} is not readable under the new CRDs: $(cat "${err}")"
    fi
    if kubectl replace -f "${current}" -o json >"${tmp}/${i}.replaced.json" 2>"${err}"; then
      print_warnings "${cr}"
      break
    fi
    if grep -q 'the object has been modified' "${err}"; then
      if [[ "${attempt}" -ge 5 ]]; then
        fail "${cr}: the unchanged update kept conflicting (5 attempts)"
      fi
      attempt=$((attempt + 1))
      sleep "${POLL_INTERVAL}"
      continue
    fi
    fail "${cr}: the new CRDs or the running webhooks reject an unchanged update: $(cat "${err}")"
  done
  sent_generation="$(jq -r '.metadata.generation // ""' "${current}")"
  stored_generation="$(jq -r '.metadata.generation // ""' "${tmp}/${i}.replaced.json")" ||
    die "cannot parse the replaced ${cr}"
  if [[ "${sent_generation}" != "${stored_generation}" ]]; then
    fail "${cr}: the unchanged update changed metadata.generation (${sent_generation} -> ${stored_generation}); the running mutating webhook rewrote the spec"
  fi
  if [[ "${i}" -eq 0 ]]; then
    probe_g0="${stored_generation}"
  fi
  echo "OK: ${cr}: an unchanged update is admitted"

  # (c) A create of the stored spec is admitted. Ratcheting does not apply to
  # a create, so this validates every field against the new schema.
  if [[ "${DRY_RUN_CREATE}" == false ]]; then
    echo "SKIP: ${cr}: dry-run create disabled (DRY_RUN_CREATE=false)"
    continue
  fi
  copy="${tmp}/${i}.copy.json"
  jq '{apiVersion, kind,
       metadata: ({name: (.metadata.name + "-w"), namespace: .metadata.namespace}
         + (if .metadata.labels then {labels: .metadata.labels} else {} end)
         + ((.metadata.annotations // {})
             | del(.["kubectl.kubernetes.io/last-applied-configuration"])
             | if . == {} then {} else {annotations: .} end))}
      + (if has("spec") then {spec} else {} end)' "${tmp}/${i}.after.json" >"${copy}" ||
    die "cannot build the dry-run copy of ${cr}"
  copy_name="$(jq -r '.metadata.name' "${copy}")"
  if ! kubectl create --dry-run=server -f "${copy}" >/dev/null 2>"${err}"; then
    fail "${cr}: a create of its stored spec (dry run, as ${copy_name}) is rejected: $(cat "${err}")"
  fi
  echo "OK: ${cr}: the new CRDs admit its stored spec on create"
done

# A listed path that no CR changed would hide the next real change.
for path in ${allowed[@]+"${allowed[@]}"}; do
  if [[ "${used_allowances}" != *" ${path} "* ]]; then
    fail "ALLOW_SPEC_CHANGES names ${path}, which kept its value; remove the entry"
  fi
done

# ---------------------------------------------------------------------------
# 5. Probe: the released controller reconciles a new generation
# ---------------------------------------------------------------------------
# An unchanged update triggers no reconcile, and Ready=True alone is the status
# the released controller wrote before the CRDs changed. A spec patch makes it
# reconcile and write its status under the new schema.
cr="${crs[0]}"
if ! kubectl patch "${cr}" -n "${CR_NAMESPACE}" --type=merge -p "${PROBE_PATCH}" -o json \
  >"${tmp}/patched.json" 2>"${err}"; then
  fail "${cr}: PROBE_PATCH was rejected under the new CRDs: $(cat "${err}")"
fi
probe_g1="$(jq -r '.metadata.generation // 0' "${tmp}/patched.json")" ||
  die "cannot parse the patched ${cr}"
if [[ ! "${probe_g1}" =~ ^[0-9]+$ ]] || [[ ! "${probe_g0}" =~ ^[0-9]+$ ]] ||
  [[ "${probe_g1}" -le "${probe_g0}" ]]; then
  die "PROBE_PATCH did not advance metadata.generation of ${cr} (${probe_g0} -> ${probe_g1})"
fi

observed="none"
condition_status="none"
condition_generation="none"
deadline=$(($(date +%s) + PROBE_TIMEOUT))
while :; do
  # A read that fails or comes back empty is no observation; the next tick
  # retries.
  if kubectl get "${cr}" -n "${CR_NAMESPACE}" -o json >"${tmp}/probe.json" 2>/dev/null; then
    # shellcheck disable=SC2016 # $c is a jq variable, not a shell one
    values="$(jq -r --arg c "${READY_CONDITION}" '
      ([(.status.conditions // [])[] | select(.type == $c)] | first) as $cond
      | [(.status.observedGeneration // "none"), ($cond.status // "none"),
         ($cond.observedGeneration // "none")] | map(tostring) | @tsv' \
      "${tmp}/probe.json" 2>/dev/null || true)"
    if [[ -n "${values}" ]]; then
      IFS=$'\t' read -r observed condition_status condition_generation <<<"${values}"
    fi
  fi
  if [[ "${observed}" == "${probe_g1}" && "${condition_status}" == "True" &&
    "${condition_generation}" == "${probe_g1}" ]]; then
    echo "OK: the released controller reconciled generation ${probe_g1} of ${cr} to ${READY_CONDITION}=True under the new CRDs"
    break
  fi
  if [[ "$(date +%s)" -ge "${deadline}" ]]; then
    fail "the released controller did not reconcile generation ${probe_g1} of ${cr} within ${PROBE_TIMEOUT}s under the new CRDs (status.observedGeneration=${observed}, ${READY_CONDITION}=${condition_status} at generation ${condition_generation})"
  fi
  sleep "${POLL_INTERVAL}"
done

# ---------------------------------------------------------------------------
# 6. Guard: the operator did not roll during the window
# ---------------------------------------------------------------------------
read_deploy
if [[ "${deploy_image}" != "${image_before}" || "${deploy_generation}" != "${generation_before}" ]]; then
  fail "deployment/${OPERATOR}-operator changed during the CRD window (image ${image_before} -> ${deploy_image}, generation ${generation_before} -> ${deploy_generation})"
fi
echo "OK: the CRD window passed for ${#crs[@]} CR(s); deployment/${OPERATOR}-operator still runs ${image_before}"
