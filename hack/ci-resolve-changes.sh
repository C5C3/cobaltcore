#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-resolve-changes.sh — Resolve effective CI changes into job flags.
#
# Reads the paths-filter outputs (passed as FILTER_* env vars) plus the pull
# request's label set, and decides which jobs and which matrix legs run. The
# rule is one line long: a job runs when one of ITS OWN inputs changed, never
# because Go code changed somewhere. A job's inputs are the code it tests, the
# images it loads, the suite it runs, and the scripts it calls.
#
# Labels only ever ADD jobs. ci:full runs everything, ci:tempest / ci:chaos /
# ci:controlplane / ci:multicluster switch on one area each. run-chaos is kept
# as an alias of ci:chaos.
#
# A `labeled` event for a label outside that set resolves to nothing at all
# (noop=true), so adding a triage label to a pull request no longer cancels and
# restarts its pipeline.
#
# Required env vars:
#   ALL_OPERATORS     — Space-separated list of every operator (e.g. "keystone")
#   SERVICE_OPERATORS — Space-separated subset that ships an OpenStack service
#                       image (the union of the keys in
#                       releases/*/source-refs.yaml)
#   GITHUB_OUTPUT     — GitHub Actions output file (set automatically)
#   GITHUB_REF        — Git ref (set automatically)
#
# Optional env vars:
#   CANARY_OPERATOR   — Operator whose e2e leg runs for a change to the shared
#                       e2e substrate or the workflow plumbing (default keystone)
#   FILTER_<filter>   — One per paths-filter key; anything but "true" is false
#   PR_LABELS         — JSON array of the pull request's label names. Unset, "",
#                       null and any non-array all mean "no labels"
#   EVENT_NAME        — github.event_name (default pull_request)
#   EVENT_ACTION      — github.event.action, for the labeled no-op
#   EVENT_LABEL       — github.event.label.name, for the labeled no-op
#
# To add a new operator <op>:
#   1. Add a paths filter <op> listing operators/<op>/**
#   2. Add a paths filter tests_e2e_<op> listing tests/e2e/<op>/** and
#      tests/e2e/<op>-operator/**
#   3. Add <op> to ALL_OPERATORS in the ci.yaml resolve step
#   4. When the operator ships a service image, add a paths filter image_<op>
#      listing images/<op>/** and patches/<op>/**, and add <op> to
#      SERVICE_OPERATORS
#   The test matrices, the e2e matrix and the build set follow automatically;
#   tests/unit/ci/change_classes_wiring_test.sh fails when a step is missed.

set -euo pipefail

# ---------------------------------------------------------------------------
# Guards and defaults
# ---------------------------------------------------------------------------
if [[ -z "${ALL_OPERATORS:-}" ]]; then
  echo "::error::ALL_OPERATORS must be set (space-separated list of operator names)"
  exit 1
fi
if [[ -z "${SERVICE_OPERATORS:-}" ]]; then
  echo "::error::SERVICE_OPERATORS must be set (space-separated list of operators with a service image)"
  exit 1
fi
CANARY_OPERATOR="${CANARY_OPERATOR:-keystone}"
EVENT_NAME="${EVENT_NAME:-pull_request}"

# Services that have a Tempest configuration directory under tests/tempest/.
# Mirrors the service loop in hack/ci-generate-tempest-matrix.sh.
TEMPEST_ALL_SERVICES="keystone glance barbican"

# ---------------------------------------------------------------------------
# Helpers
#
# Sets are space-separated strings rather than associative arrays so the script
# runs on bash 3.2 (macOS) as well as the runners' bash 5.
# ---------------------------------------------------------------------------

# filter_on <name> — true when FILTER_<name> is exactly "true".
filter_on() {
  local var="FILTER_$1"
  [[ "${!var:-false}" == "true" ]]
}

# set_has <set> <item>
set_has() {
  case " $1 " in
    *" $2 "*) return 0 ;;
    *) return 1 ;;
  esac
}

# set_add <set> <item...> — echoes the set with the items appended once each.
set_add() {
  local acc="$1" item
  shift
  for item in "$@"; do
    set_has "$acc" "$item" || acc="${acc:+$acc }$item"
  done
  echo "$acc"
}

# order_by <ordering> <set> — echoes <set> in <ordering> order.
order_by() {
  local out="" item
  for item in $1; do
    set_has "$2" "$item" && out="${out:+$out }$item"
  done
  echo "$out"
}

# json_list <set> — echoes a compact JSON array, [] when the set is empty.
json_list() {
  # shellcheck disable=SC2086 # deliberate: split the space-separated set
  printf '%s\n' ${1:-} | jq -Rnc '[inputs | select(length > 0)]'
}

# matrix_of <key> <set> — echoes {"<key>":[...]}, or the __none__ sentinel when
# the set is empty. Downstream jobs gate on the matching has-* flag; the
# sentinel keeps fromJson() valid for the ones that read the matrix ungated.
matrix_of() {
  if [[ -z "${2:-}" ]]; then
    echo "{\"$1\":[\"__none__\"]}"
    return
  fi
  # shellcheck disable=SC2086 # deliberate: split the space-separated set
  printf '%s\n' $2 | jq -Rnc --arg k "$1" '{($k): [inputs | select(length > 0)]}'
}

emit() { echo "$1=$2" >>"$GITHUB_OUTPUT"; }

# bool <condition-result> — normalises an exit status into true/false.
yesno() { if "$@"; then echo true; else echo false; fi; }

# ---------------------------------------------------------------------------
# Label set
# ---------------------------------------------------------------------------
LABELS=""
if [[ -n "${PR_LABELS:-}" ]]; then
  # Anything that is not a JSON array of strings resolves to no labels at all,
  # which is what a push event's empty expression renders to.
  LABELS=$(jq -r 'if type == "array" then .[] | select(type == "string") else empty end' \
    <<<"${PR_LABELS}" 2>/dev/null || true)
fi

has_label() {
  [[ -n "$LABELS" ]] && printf '%s\n' "$LABELS" | grep -Fxq -- "$1"
}

# ---------------------------------------------------------------------------
# 1. The labeled no-op
#
# Adding a label that does not steer CI must not cancel the run in flight. The
# workflow puts such an event in a concurrency group of its own; this resolves
# it to nothing so every gated job skips.
# ---------------------------------------------------------------------------
noop=false
if [[ "${EVENT_NAME}" == "pull_request" && "${EVENT_ACTION:-}" == "labeled" ]]; then
  event_label="${EVENT_LABEL:-}"
  if [[ "${event_label}" != ci:* && "${event_label}" != "run-chaos" ]]; then
    noop=true
  fi
fi

if [[ "${noop}" == "true" ]]; then
  emit noop true
  for out in go docs helm target-cluster-chart has-e2e-operators e2e-infra \
    e2e-chaos e2e-prometheus e2e-controlplane e2e-controlplane-sso \
    e2e-external-keystone e2e-multicluster e2e-ovn-overlay \
    e2e-operator-upgrade tempest changed-tempest changed-proxy \
    build-e2e-images actionlint; do
    emit "$out" false
  done
  for out in changed-operators changed-services tempest-services; do
    emit "$out" '[]'
  done
  emit test-targets "$(matrix_of target "")"
  emit e2e-operators "$(matrix_of operator "")"
  exit 0
fi
emit noop false

# ---------------------------------------------------------------------------
# 2. Inputs
# ---------------------------------------------------------------------------
full=false
has_label "ci:full" && full=true

is_tag=false
[[ "${GITHUB_REF:-}" == refs/tags/v* ]] && is_tag=true

# op_changed — the operator's own Go code is affected.
op_changed=""
for op in $ALL_OPERATORS; do
  if [[ "$is_tag" == "true" || "$full" == "true" ]] || filter_on "$op" || filter_on go_common; then
    op_changed=$(set_add "$op_changed" "$op")
  fi
done

# changed_services — the operator's service image sources are affected.
changed_services=""
if [[ "$is_tag" == "true" || "$full" == "true" ]] || filter_on images_base; then
  changed_services="$SERVICE_OPERATORS"
else
  for svc in $SERVICE_OPERATORS; do
    filter_on "image_${svc}" && changed_services=$(set_add "$changed_services" "$svc")
  done
fi

# The canary: a change to the shared e2e substrate or to the workflow plumbing
# proves itself against the infrastructure suite and one operator leg, not
# against the whole pipeline. ci:full is the way to ask for the rest.
canary=false
if filter_on e2e_shared || filter_on e2e_openbao || filter_on ci_plumbing || filter_on makefile; then
  canary=true
fi

# ---------------------------------------------------------------------------
# 3. Job flags
#
# force is what a tag push and ci:full have in common: every flag on, whatever
# the paths say. Every other flag is the OR of its own inputs and nothing else.
# ---------------------------------------------------------------------------
force=false
if [[ "$is_tag" == "true" || "$full" == "true" ]]; then
  force=true
fi

# or_force <bool> — echoes true when forced, otherwise the argument.
or_force() {
  if [[ "$force" == "true" || "$1" == "true" ]]; then
    echo true
  else
    echo false
  fi
}

op_is_changed() { set_has "$op_changed" "$1"; }

cond=false
if [[ -n "$op_changed" ]] || filter_on makefile; then cond=true; fi
go=$(or_force "$cond")

cond=false
if filter_on e2e_infra || [[ "$canary" == "true" ]]; then cond=true; fi
e2e_infra=$(or_force "$cond")

cond=false
if filter_on tests_chaos || has_label "ci:chaos" || has_label "run-chaos"; then cond=true; fi
e2e_chaos=$(or_force "$cond")

cond=false
if filter_on tests_prometheus; then cond=true; fi
e2e_prometheus=$(or_force "$cond")

# The three ControlPlane jobs share two triggers: a change to operators/c5c3/**
# (they are the real tests of that operator, whose own e2e leg carries only the
# sibling CRDs) and the ci:controlplane label. Each adds its own suite filter on
# top.
#
# Deliberately FILTER_c5c3 rather than membership of op_changed: a shared Go
# change puts every operator in that set, and these three jobs are the most
# expensive in the pipeline (up to 195 minutes each). A shared change still runs
# the c5c3 e2e leg; ci:controlplane or ci:full asks for the full chain.
cp_common=false
if filter_on c5c3 || has_label "ci:controlplane"; then cp_common=true; fi

cond="$cp_common"
if filter_on tests_controlplane; then cond=true; fi
e2e_controlplane=$(or_force "$cond")

cond="$cp_common"
if filter_on tests_controlplane_sso; then cond=true; fi
e2e_controlplane_sso=$(or_force "$cond")

cond="$cp_common"
if filter_on tests_external_keystone; then cond=true; fi
e2e_external_keystone=$(or_force "$cond")

cond=false
if filter_on tests_multicluster || has_label "ci:multicluster"; then cond=true; fi
e2e_multicluster=$(or_force "$cond")

# The multi-node OVN overlay job. Its inputs are the suite, the ovn-operator
# and the OVN daemon image: the datapath it proves is built by the chassis
# DaemonSets those two ship, and nothing else reaches it.
#
# Deliberately FILTER_ovn rather than membership of op_changed, for the reason
# the ControlPlane trio gives above: the job brings up a second kind cluster
# shape of its own on a self-hosted runner, so a shared Go change must not
# schedule it. ci:full is how you ask for it anyway.
cond=false
if filter_on tests_ovn_overlay || filter_on ovn || filter_on image_ovn; then cond=true; fi
e2e_ovn_overlay=$(or_force "$cond")

cond=false
if filter_on tests_operator_upgrade || op_is_changed keystone; then cond=true; fi
e2e_operator_upgrade=$(or_force "$cond")

cond=false
if filter_on tempest_src || has_label "ci:tempest"; then cond=true; fi
tempest=$(or_force "$cond")

cond=false
if filter_on image_tempest || filter_on images_base; then cond=true; fi
changed_tempest=$(or_force "$cond")

cond=false
if filter_on image_proxy; then cond=true; fi
changed_proxy=$(or_force "$cond")

cond=false
if filter_on actionlint; then cond=true; fi
actionlint=$(or_force "$cond")

cond=false
if filter_on docs; then cond=true; fi
docs=$(or_force "$cond")

cond=false
if filter_on helm; then cond=true; fi
helm=$(or_force "$cond")

cond=false
if filter_on target_cluster_chart; then cond=true; fi
target_cluster_chart=$(or_force "$cond")
# ---------------------------------------------------------------------------
# 4. Matrices
# ---------------------------------------------------------------------------

# test / test-integration: common plus every operator whose code is affected.
test_targets=""
if [[ "$force" == "true" ]] || filter_on go_common || filter_on makefile; then
  test_targets="common"
fi
for op in $ALL_OPERATORS; do
  if op_is_changed "$op" || filter_on makefile; then
    test_targets=$(set_add "$test_targets" "$op")
  fi
done

# changed-operators: the operators whose own sources changed.
# hack/ci-resolve-e2e-images.sh reads this to decide which operator images
# build-e2e-images still has to build; it reuses the rest from main.
changed_operators="$op_changed"
if filter_on makefile; then
  changed_operators=$(set_add "$changed_operators" "$CANARY_OPERATOR")
fi

# e2e-operator legs.
e2e_ops="$op_changed"
for svc in $SERVICE_OPERATORS; do
  set_has "$changed_services" "$svc" && e2e_ops=$(set_add "$e2e_ops" "$svc")
done
filter_on image_ovn && e2e_ops=$(set_add "$e2e_ops" ovn)
filter_on image_proxy && e2e_ops=$(set_add "$e2e_ops" keystone)
for op in $ALL_OPERATORS; do
  filter_on "tests_e2e_${op}" && e2e_ops=$(set_add "$e2e_ops" "$op")
done
[[ "$canary" == "true" ]] && e2e_ops=$(set_add "$e2e_ops" "$CANARY_OPERATOR")
filter_on e2e_openbao && e2e_ops=$(set_add "$e2e_ops" barbican)
[[ "$force" == "true" ]] && e2e_ops="$ALL_OPERATORS"

# On a push the operator matrix keeps today's publish semantics: it drives
# build-and-push, merge-operator-images and helm-push, not the e2e jobs.
if [[ "${EVENT_NAME}" == "push" && "$is_tag" != "true" ]]; then
  e2e_ops=""
  if filter_on go_common || filter_on publish_legacy; then
    e2e_ops="$ALL_OPERATORS"
  else
    for op in $ALL_OPERATORS; do
      if filter_on "$op" || filter_on "image_${op}"; then
        e2e_ops=$(set_add "$e2e_ops" "$op")
      fi
    done
  fi
fi

# Tempest legs, by service. A change to something every leg shares (the image,
# the base images, the runner) exercises all three; a config edit under
# tests/tempest/<svc>-*/ narrows it to that service; the ci:tempest label
# narrows it to the services the pull request touches, and falls back to
# keystone when it touches none.
tempest_services=""
if [[ "$tempest" == "true" ]]; then
  any_tempest_service_filter=false
  for svc in $TEMPEST_ALL_SERVICES; do
    if filter_on "tempest_${svc}"; then any_tempest_service_filter=true; fi
  done

  if [[ "$force" == "true" ]] || filter_on image_tempest || filter_on images_base ||
    { filter_on tempest_src && [[ "$any_tempest_service_filter" == "false" ]]; }; then
    tempest_services="$TEMPEST_ALL_SERVICES"
  else
    for svc in $TEMPEST_ALL_SERVICES; do
      if filter_on "tempest_${svc}" || op_is_changed "$svc" ||
        filter_on "image_${svc}" || filter_on "tests_e2e_${svc}"; then
        tempest_services=$(set_add "$tempest_services" "$svc")
      fi
    done
    if [[ -z "$tempest_services" ]]; then
      tempest_services="keystone"
    fi
  fi
fi

has_e2e_operators=$(yesno test -n "$e2e_ops")

build_e2e_images=false
if [[ "$has_e2e_operators" == "true" || "$e2e_chaos" == "true" ||
  "$e2e_prometheus" == "true" || "$e2e_controlplane" == "true" ||
  "$e2e_controlplane_sso" == "true" || "$e2e_external_keystone" == "true" ||
  "$e2e_multicluster" == "true" || "$e2e_ovn_overlay" == "true" ||
  "$e2e_operator_upgrade" == "true" || "$tempest" == "true" ]]; then
  build_e2e_images=true
fi
# The build job only runs on pull requests; a push resolves it away so the
# publish path is never gated on it.
[[ "${EVENT_NAME}" == "push" && "$is_tag" != "true" ]] && build_e2e_images=false

# ---------------------------------------------------------------------------
# 5. Emit
# ---------------------------------------------------------------------------
emit go "$go"
emit docs "$docs"
emit helm "$helm"
emit target-cluster-chart "$target_cluster_chart"

emit e2e-infra "$e2e_infra"
emit e2e-chaos "$e2e_chaos"
emit e2e-prometheus "$e2e_prometheus"
emit e2e-controlplane "$e2e_controlplane"
emit e2e-controlplane-sso "$e2e_controlplane_sso"
emit e2e-external-keystone "$e2e_external_keystone"
emit e2e-multicluster "$e2e_multicluster"
emit e2e-ovn-overlay "$e2e_ovn_overlay"
emit e2e-operator-upgrade "$e2e_operator_upgrade"
emit tempest "$tempest"
emit actionlint "$actionlint"

emit changed-tempest "$changed_tempest"
emit changed-proxy "$changed_proxy"
emit build-e2e-images "$build_e2e_images"
emit has-e2e-operators "$has_e2e_operators"

emit changed-operators "$(json_list "$(order_by "$ALL_OPERATORS" "$changed_operators")")"
emit changed-services "$(json_list "$(order_by "$SERVICE_OPERATORS" "$changed_services")")"
emit tempest-services "$(json_list "$(order_by "$TEMPEST_ALL_SERVICES" "$tempest_services")")"

emit test-targets "$(matrix_of target "$(order_by "common $ALL_OPERATORS" "$test_targets")")"
emit e2e-operators "$(matrix_of operator "$(order_by "$ALL_OPERATORS" "$e2e_ops")")"
