#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/c5c3/full-controlplane-keystone/extraconfig-edit.sh: Link 7e of
# the full-chain suite.
#
# The step 1 script of chainsaw-test.yaml sources this file between Link 7d
# and Link 8a:
#
#   . ./extraconfig-edit.sh
#
# Chainsaw hands a script's content to bash as one argument, and Linux caps
# one argument at 128 KiB (MAX_ARG_STRLEN). With this link inline the step 1
# script passes that cap and bash never starts. Sourced, the link runs in the
# step's own shell: it reads CP_NAME, CP_NS and KEYSTONE_CR from it, runs
# under its set -euo pipefail, and its exit 1 ends the step.

# reconcileKeystone projects MergedExtraConfig(globalExtraConfig,
# services.keystone.extraConfig) onto the child's spec.extraConfig on
# every pass, and a nil merge clears the field. One merge patch sets
# [DEFAULT] instance_format, the inert, catalog-valid marker of
# tests/e2e/keystone/config-pruning, on the live plane. The edit has to
# reach the child's spec, a new ConfigMap, a finished rollout and the
# file a Ready pod mounts. Clearing the block has to bring the child
# back to its original ConfigMap: the render is content-addressed, and
# the prune keeps the previous ConfigMap. Each half has its own 600s
# deadline. The variables carry an L7E_ prefix so Link 8a keeps its
# own G0 and G1.
L7E_MARKER=controlplane-propagation
L7E_LINE="instance_format = ${L7E_MARKER}"
# l7e_pairs: the number of key pairs in the extraConfig map on stdin
# (absent, {} and empty sections all count 0).
l7e_pairs() { jq '[(. // {}) | .[] | keys[]] | length'; }
l7e_template_cm() {
  kubectl get deployment "${KEYSTONE_CR}" -n "${CP_NS}" \
    -o jsonpath='{.spec.template.spec.volumes[?(@.name=="config")].configMap.name}' 2>/dev/null || true
}
# l7e_wait <deadline> <stage> <check>: polls <check> every 5s until
# it succeeds. Each check leaves what it read in L7E_LAST.
l7e_wait() {
  L7E_LAST=""
  until "$3"; do
    if [ "$(date +%s)" -ge "$1" ]; then
      echo "ERROR: Link 7e stopped at $2 after 600s (last read: ${L7E_LAST:-nothing})"
      exit 1
    fi
    sleep 5
  done
  echo "OK: Link 7e $2 (${L7E_LAST})"
}
l7e_rollout() { # <deadline>
  local left=$(( $1 - $(date +%s) ))
  if [ "${left}" -lt 1 ]; then
    left=1
  fi
  if ! kubectl rollout status "deployment/${KEYSTONE_CR}" -n "${CP_NS}" --timeout="${left}s"; then
    echo "ERROR: Link 7e stopped at the rollout after 600s (last read: $(kubectl get deployment "${KEYSTONE_CR}" -n "${CP_NS}" \
      -o jsonpath='updated {.status.updatedReplicas}, ready {.status.readyReplicas} of {.spec.replicas}' 2>/dev/null || true))"
    exit 1
  fi
}
l7e_cp_generation() {
  local obs
  obs="$(kubectl get controlplane "${CP_NAME}" -n "${CP_NS}" -o jsonpath='{.status.observedGeneration}' 2>/dev/null || true)"
  L7E_LAST="ControlPlane observedGeneration '${obs}'"
  [ "${obs}" = "${L7E_GEN}" ]
}
l7e_child_marker() {
  local v
  v="$(kubectl get keystone "${KEYSTONE_CR}" -n "${CP_NS}" \
    -o jsonpath='{.spec.extraConfig.DEFAULT.instance_format}' 2>/dev/null || true)"
  L7E_LAST="child spec.extraConfig.DEFAULT.instance_format '${v}'"
  [ "${v}" = "${L7E_MARKER}" ]
}
l7e_child_cleared() {
  local n
  n="$(kubectl get keystone "${KEYSTONE_CR}" -n "${CP_NS}" -o json 2>/dev/null \
    | jq '.spec.extraConfig' | l7e_pairs || true)"
  L7E_LAST="${n:-?} key(s) in the child's spec.extraConfig"
  [ "${n}" = "0" ]
}
l7e_child_ready() {
  local ks gen obs rdy
  ks="$(kubectl get keystone "${KEYSTONE_CR}" -n "${CP_NS}" -o json 2>/dev/null)" || ks='{}'
  gen="$(jq -r '.metadata.generation // ""' <<<"${ks}")"
  obs="$(jq -r '.status.observedGeneration // ""' <<<"${ks}")"
  rdy="$(jq -r '[.status.conditions[]? | select(.type == "Ready") | .status][0] // ""' <<<"${ks}")"
  L7E_LAST="child generation '${gen}', observedGeneration '${obs}', Ready '${rdy}'"
  [ -n "${gen}" ] && [ "${obs}" = "${gen}" ] && [ "${rdy}" = "True" ]
}
l7e_new_cm() {
  local cm conf
  cm="$(l7e_template_cm)"
  L7E_LAST="template ConfigMap '${cm}'"
  if [ -z "${cm}" ] || [ "${cm}" = "${L7E_C0}" ]; then
    return 1
  fi
  conf="$(kubectl get cm "${cm}" -n "${CP_NS}" -o jsonpath='{.data.keystone\.conf}' 2>/dev/null || true)"
  if ! grep -Fxq "${L7E_LINE}" <<<"${conf}"; then
    L7E_LAST="${L7E_LAST} without '${L7E_LINE}'"
    return 1
  fi
  L7E_C1="${cm}"
}
l7e_old_cm() {
  local cm
  cm="$(l7e_template_cm)"
  L7E_LAST="template ConfigMap '${cm}'"
  [ "${cm}" = "${L7E_C0}" ]
}
l7e_pod_file() {
  local pod
  pod="$(kubectl get pods -n "${CP_NS}" -l "app.kubernetes.io/instance=${KEYSTONE_CR},app.kubernetes.io/component=api" \
    -o json 2>/dev/null | jq -r --arg c "${L7E_C1}" '[.items[] | select(.metadata.deletionTimestamp == null)
      | select(any(.spec.volumes[]?; .name == "config" and .configMap.name == $c))
      | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))
      | .metadata.name][0] // empty' || true)"
  if [ -z "${pod}" ]; then
    L7E_LAST="no Ready API pod mounts ${L7E_C1}"
    return 1
  fi
  if ! kubectl exec -n "${CP_NS}" "${pod}" -c keystone -- python3 -c '
import sys
lines = open("/etc/keystone/keystone.conf.d/keystone.conf").read().splitlines()
sys.exit(0 if sys.argv[1] in lines else 1)
' "${L7E_LINE}"; then
    L7E_LAST="pod ${pod} mounts ${L7E_C1} without '${L7E_LINE}' in /etc/keystone/keystone.conf.d/keystone.conf"
    return 1
  fi
  L7E_LAST="pod ${pod} reads '${L7E_LINE}' from /etc/keystone/keystone.conf.d/keystone.conf"
}
l7e_cp_ready() {
  local cp obs rdy
  cp="$(kubectl get controlplane "${CP_NAME}" -n "${CP_NS}" -o json 2>/dev/null)" || cp='{}'
  obs="$(jq -r '.status.observedGeneration // ""' <<<"${cp}")"
  rdy="$(jq -r '[.status.conditions[]? | select(.type == "Ready") | "\(.status)/\(.reason)"][0] // ""' <<<"${cp}")"
  L7E_LAST="ControlPlane Ready '${rdy}' at observedGeneration '${obs}'"
  [ "${obs}" = "${L7E_GEN}" ] && [ "${rdy}" = "True/AllReady" ]
}

# A run that stopped between the two patches leaves the block on the
# never-deleted plane, and the patch below would then change nothing.
L7E_SET="$(kubectl get controlplane "${CP_NAME}" -n "${CP_NS}" -o json \
  | jq '.spec.services.keystone.extraConfig' | l7e_pairs)"
if [ "${L7E_SET}" != "0" ]; then
  echo "ERROR: services.keystone.extraConfig is already set; a previous run stopped inside Link 7e. Clear it with kubectl patch controlplane ${CP_NAME} -n ${CP_NS} --type merge -p '{\"spec\":{\"services\":{\"keystone\":{\"extraConfig\":null}}}}'"
  exit 1
fi
L7E_C0="$(l7e_template_cm)"
L7E_G0="$(kubectl get controlplane "${CP_NAME}" -n "${CP_NS}" -o jsonpath='{.metadata.generation}')"
if [ -z "${L7E_C0}" ] || ! [[ "${L7E_G0}" =~ ^[0-9]+$ ]]; then
  echo "ERROR: could not read the Keystone child's ConfigMap ('${L7E_C0}') or the ControlPlane generation ('${L7E_G0}')"; exit 1
fi
echo "OK: the Keystone child mounts ${L7E_C0} at ControlPlane generation ${L7E_G0}"

kubectl patch controlplane "${CP_NAME}" -n "${CP_NS}" --type merge \
  -p '{"spec":{"services":{"keystone":{"extraConfig":{"DEFAULT":{"instance_format":"controlplane-propagation"}}}}}}' \
  || { echo "ERROR: the ControlPlane webhook refused services.keystone.extraConfig"; exit 1; }
L7E_GEN=$(( L7E_G0 + 1 ))
L7E_DEADLINE=$(( $(date +%s) + 600 ))
l7e_wait "${L7E_DEADLINE}" "ControlPlane observedGeneration ${L7E_GEN}" l7e_cp_generation
l7e_wait "${L7E_DEADLINE}" "child spec.extraConfig" l7e_child_marker
l7e_wait "${L7E_DEADLINE}" "child Ready at its generation" l7e_child_ready
l7e_wait "${L7E_DEADLINE}" "new ConfigMap" l7e_new_cm
l7e_rollout "${L7E_DEADLINE}"
l7e_wait "${L7E_DEADLINE}" "mounted config" l7e_pod_file
l7e_wait "${L7E_DEADLINE}" "ControlPlane Ready" l7e_cp_ready

kubectl patch controlplane "${CP_NAME}" -n "${CP_NS}" --type merge \
  -p '{"spec":{"services":{"keystone":{"extraConfig":null}}}}'
L7E_GEN=$(( L7E_G0 + 2 ))
L7E_DEADLINE=$(( $(date +%s) + 600 ))
l7e_wait "${L7E_DEADLINE}" "ControlPlane observedGeneration ${L7E_GEN}" l7e_cp_generation
l7e_wait "${L7E_DEADLINE}" "child spec.extraConfig cleared" l7e_child_cleared
l7e_wait "${L7E_DEADLINE}" "original ConfigMap" l7e_old_cm
l7e_rollout "${L7E_DEADLINE}"
l7e_wait "${L7E_DEADLINE}" "ControlPlane Ready" l7e_cp_ready
echo "OK: a ControlPlane extraConfig edit reached the Keystone child, rolled it to ${L7E_C1} and back to ${L7E_C0}."
