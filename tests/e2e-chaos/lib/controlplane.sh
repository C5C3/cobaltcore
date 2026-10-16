#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ControlPlane bring-up of the chaos suites on the controlplane leg, sourced by
# their scripts as `. ../lib/controlplane.sh` with NAMESPACE set by Chainsaw.
#
# Each suite brings up a Keystone-only ControlPlane of its own in chainsaw's
# ephemeral namespace, with the commands and wait budgets of
# tests/e2e/c5c3/keystone-service: seed the per-CR OpenBao paths, apply the CR,
# wait for its infrastructure, onboard the database tenant, then wait for the
# Keystone and the tail of the pipeline. Every function works in NAMESPACE,
# prints OK: or ERROR: lines and returns non-zero when a wait runs out;
# set -euo pipefail is the caller's.
#
# Fixtures that name the namespace carry @NS@, which cp_apply_file and
# cp_delete_file replace.

CP_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CP_BOOTSTRAP_DIR="${CP_LIB_DIR}/../../../deploy/openbao/bootstrap"
# shellcheck source=tests/e2e-chaos/lib/openbao.sh
. "${CP_LIB_DIR}/openbao.sh"
# The K-ORC kinds a Keystone-only ControlPlane and its registrations project.
CP_KORC_KINDS=applicationcredentials.openstack.k-orc.cloud,domains.openstack.k-orc.cloud,endpoints.openstack.k-orc.cloud,projects.openstack.k-orc.cloud,regions.openstack.k-orc.cloud,roleassignments.openstack.k-orc.cloud,roles.openstack.k-orc.cloud,services.openstack.k-orc.cloud,users.openstack.k-orc.cloud

# cp_wait_condition <resource> <name> <type> <status> <seconds>: poll the
# condition every 5 s rather than `kubectl wait`, so a condition that is
# briefly absent or unreadable is retried and the failure names the last
# status read.
cp_wait_condition() {
  local res="$1" name="$2" ctype="$3" want="$4" timeout="$5" deadline status
  deadline=$(($(date +%s) + timeout))
  while true; do
    status="$(cp_status "$res" "$name" "$ctype" 2>/dev/null || true)"
    if [ "$status" = "$want" ]; then
      echo "OK: ${res}/${name} ${ctype}=${want}"
      return 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "ERROR: ${res}/${name} ${ctype} not ${want} within ${timeout}s (last: '${status:-<none>}')"
      kubectl get "$res" "$name" -n "$NAMESPACE" \
        -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}: {.message}){"\n"}{end}' 2>&1 || true
      return 1
    fi
    sleep 5
  done
}

# cp_status <resource> <name> <type>: the status of one condition, empty when
# it is absent. A failed read fails the call, so a check that a condition is
# not True never passes on an object it could not read.
cp_status() {
  kubectl get "$1" "$2" -n "$NAMESPACE" \
    -o "jsonpath={.status.conditions[?(@.type=='$3')].status}"
}

# cp_reason <resource> <name> <type>: the reason of one condition, empty when
# it is absent. A failed read fails the call, as in cp_status.
cp_reason() {
  kubectl get "$1" "$2" -n "$NAMESPACE" \
    -o "jsonpath={.status.conditions[?(@.type=='$3')].reason}"
}

# cp_export_bao_token: export the OpenBao root token for the two bootstrap
# scripts, which read it from their environment.
cp_export_bao_token() {
  BAO_TOKEN="$(bao_root_token)" || return 1
  export BAO_TOKEN
}

# cp_seed <cp>: seed the per-CR OpenBao paths of NAMESPACE/<cp>. deploy-infra
# seeds only the identity it was given, so an ephemeral plane seeds its own.
cp_seed() {
  cp_export_bao_token || return 1
  KORC_CONTROLPLANES="${NAMESPACE}/$1" "${CP_BOOTSTRAP_DIR}/write-bootstrap-secrets.sh" || return 1
  echo "OK: the per-CR OpenBao paths of ${NAMESPACE}/$1 are seeded"
}

# cp_wait_infrastructure <cp>: the managed MariaDB and Memcached are up.
cp_wait_infrastructure() {
  cp_wait_condition controlplane "$1" InfrastructureReady True 480 || return 1
  kubectl wait mariadb/openstack-db -n "$NAMESPACE" --for=condition=Ready --timeout=60s
}

# cp_onboard_database <cp>: onboard the database-engine tenant of NAMESPACE,
# which the dynamic DB credential needs, and wait for that credential.
cp_onboard_database() {
  cp_export_bao_token || return 1
  "${CP_BOOTSTRAP_DIR}/setup-database-tenant.sh" "$NAMESPACE" "$1" || return 1
  cp_wait_condition controlplane "$1" DBCredentialsReady True 180
}

# cp_wait_keystone <cp>: the projected Keystone is Ready.
cp_wait_keystone() {
  cp_wait_condition controlplane "$1" KeystoneReady True 720
}

# cp_wait_tail <cp>: the pipeline after the Keystone, then Ready=True/AllReady.
cp_wait_tail() {
  local cp="$1" ctype reason
  for ctype in KORCReady:300 AdminCredentialReady:120 ESOTenantStoreReady:120 Ready:120; do
    cp_wait_condition controlplane "$cp" "${ctype%%:*}" True "${ctype##*:}" || return 1
  done
  reason="$(cp_reason controlplane "$cp" Ready)"
  if [ "$reason" != "AllReady" ]; then
    echo "ERROR: controlplane/${cp} is Ready with reason '${reason:-<none>}', not AllReady"
    return 1
  fi
  echo "OK: controlplane/${cp} is Ready=True/AllReady"
}

# cp_bring_up <cp> <fixture>: the whole sequence, from the seed to AllReady.
# Its waits sum to 2100 s, which leaves the seed, the apply and the two
# bootstrap scripts 300 s of the 40-minute step that runs it, so a wait that
# runs out still prints its ERROR line before chainsaw kills the step.
cp_bring_up() {
  cp_seed "$1" || return 1
  kubectl apply -n "$NAMESPACE" -f "$2" || return 1
  cp_wait_infrastructure "$1" || return 1
  cp_onboard_database "$1" || return 1
  cp_wait_keystone "$1" || return 1
  cp_wait_tail "$1"
}

# cp_render <file>: the fixture with @NS@ replaced by NAMESPACE.
cp_render() {
  sed -e "s|@NS@|${NAMESPACE}|g" "$1"
}

# cp_apply_file <file>: apply the rendered fixture.
cp_apply_file() {
  cp_render "$1" | kubectl apply -n "$NAMESPACE" -f -
}

# cp_delete_file <file>: delete what cp_apply_file applied; absent is fine.
cp_delete_file() {
  cp_render "$1" | kubectl delete -n "$NAMESPACE" --ignore-not-found -f -
}

# cp_teardown <cp>: delete the ControlPlane and wait for it to go. Its
# finalizer takes the K-ORC children out of a Keystone that still serves, and
# releases stuck K-ORC finalizers after the 7-minute orcTeardownDeadline, so
# 12 minutes bound it.
cp_teardown() {
  kubectl delete controlplane "$1" -n "$NAMESPACE" --ignore-not-found --wait --timeout=12m
}

# cp_dump <cp>: the evidence of a failed step. Every call is `|| true`: a dump
# must not mask the step's own failure.
cp_dump() {
  local cp="$1"
  "${CP_LIB_DIR}/../diagnostics.sh" chaos "$cp" --cr-kind=controlplane \
    --dep-ns=orc-system --dep-label=control-plane=controller-manager 2>&1 || true
  "${CP_LIB_DIR}/../diagnostics.sh" chaos "$cp" --cr-kind=controlplane \
    --dep-ns=c5c3-system --dep-label=app.kubernetes.io/name=c5c3-operator 2>&1 || true
  echo "=== ControlPlane conditions ==="
  kubectl get controlplane "$cp" -n "$NAMESPACE" \
    -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}: {.message}){"\n"}{end}' 2>&1 || true
  echo "=== K-ORC objects ==="
  kubectl get "$CP_KORC_KINDS" -n "$NAMESPACE" -o wide 2>&1 || true
  echo "=== K-ORC controller ==="
  kubectl logs -n orc-system deploy/orc-controller-manager --pod-running-timeout=5s --tail=150 --all-containers 2>&1 || true
  echo "=== c5c3-operator ==="
  kubectl logs -n c5c3-system -l app.kubernetes.io/name=c5c3-operator --tail=150 --all-containers 2>&1 || true
}
