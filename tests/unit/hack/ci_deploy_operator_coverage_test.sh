#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the E2E_COVERAGE knob of hack/ci-deploy-operator.sh with a stubbed
# PATH (helm, kubectl, kind, docker):
#   - E2E_COVERAGE=true creates the counter directory on every node kind lists
#     and passes the hostPath volume, its mount and GOCOVERDIR to helm;
#   - unset or false passes none of them and never calls docker;
#   - an invalid value, a chart without extraVolumes, a cluster without nodes
#     and a failing docker exec each exit 1 with an ::error:: line before
#     helm runs.
#
# Follows tests/unit/hack/ci_deploy_operator_replicas_test.sh.
#
# Usage: bash tests/unit/hack/ci_deploy_operator_coverage_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_SH="$PROJECT_ROOT/hack/ci-deploy-operator.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stubs <dir>
# helm and docker log their argv and succeed; docker exits $DOCKER_EXIT when
# set. kubectl logs its argv, returns non-zero for `get
# mutatingwebhookconfigurations` so the deploy script skips the webhook
# readiness wait, and succeeds otherwise. kind logs its argv and prints
# $KIND_NODES, one node per line.
make_stubs() {
  local dir="$1"
  mkdir -p "$dir"

  cat >"$dir/helm" <<'STUB'
#!/bin/bash
echo "helm $*" >>"$HELM_LOG"
exit 0
STUB

  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
echo "kubectl $*" >>"$KUBECTL_LOG"
if [ "${1:-}" = "get" ] && [ "${2:-}" = "mutatingwebhookconfigurations" ]; then
  exit 1
fi
exit 0
STUB

  cat >"$dir/kind" <<'STUB'
#!/bin/bash
echo "kind $*" >>"$KIND_LOG"
if [ -n "${KIND_NODES:-}" ]; then
  printf '%s\n' $KIND_NODES
fi
exit 0
STUB

  cat >"$dir/docker" <<'STUB'
#!/bin/bash
echo "docker $*" >>"$DOCKER_LOG"
exit "${DOCKER_EXIT:-0}"
STUB
  chmod +x "$dir/helm" "$dir/kubectl" "$dir/kind" "$dir/docker"
}

# make_chart <dir> [with-extra-volumes]
# A minimal stub chart with a crds/ directory and a values.schema.json that
# names extraVolumes only when asked to.
make_chart() {
  local dir="$1" extra="${2:-}"
  mkdir -p "$dir/crds"
  printf 'apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n' \
    >"$dir/crds/dummy.yaml"
  if [ -n "$extra" ]; then
    echo '{"properties": {"extraVolumes": {}, "extraVolumeMounts": {}}}' >"$dir/values.schema.json"
  else
    echo '{"properties": {"extraEnv": {}}}' >"$dir/values.schema.json"
  fi
}

# run_deploy <tmp>
# Runs the deploy script against the stubs and the stub chart in <tmp>.
# E2E_COVERAGE, KIND_NODES and DOCKER_EXIT are inherited from the caller.
run_deploy() {
  local tmp="$1"
  (
    PATH="$tmp/bin:$PATH"
    export PATH
    export HELM_LOG="$tmp/helm.log"
    export KUBECTL_LOG="$tmp/kubectl.log"
    export KIND_LOG="$tmp/kind.log"
    export DOCKER_LOG="$tmp/docker.log"
    export OPERATOR="keystone"
    export IMAGE_REPO="ghcr.io/c5c3/keystone-operator"
    export CHART_DIR="$tmp/chart"
    unset KIND_CLUSTER
    bash "$DEPLOY_SH"
  ) 2>&1
}

# new_tmp [with-extra-volumes]
new_tmp() {
  local tmp
  tmp="$(mktemp -d)"
  make_stubs "$tmp/bin"
  make_chart "$tmp/chart" "${1:-}"
  touch "$tmp/helm.log" "$tmp/kubectl.log" "$tmp/kind.log" "$tmp/docker.log"
  echo "$tmp"
}

# shellcheck disable=SC2016 # the node shell's $1, as the docker stub logs it
MKDIR_LINE='sh -c mkdir -p "$1" && chmod 1777 "$1" _ /var/lib/cobaltcore-coverage/keystone'

# ---------------------------------------------------------------------------
# Test 1: E2E_COVERAGE=true prepares every node and passes the three values
# ---------------------------------------------------------------------------
test_coverage_on() {
  echo "Test: E2E_COVERAGE=true creates the node directory and mounts it with GOCOVERDIR"
  local tmp output rc
  tmp="$(new_tmp with-extra-volumes)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(E2E_COVERAGE=true KIND_NODES="cobaltcore-control-plane cobaltcore-worker" run_deploy "$tmp")"
  rc=$?

  assert_eq "deploy exits 0" "0" "$rc"
  assert_contains "the banner echoes the value" "$output" "E2E coverage        : true"
  assert_file_contains_fixed "kind lists the nodes of the default cluster" "$tmp/kind.log" \
    "kind get nodes --name cobaltcore"
  assert_eq "docker exec runs once per node" \
    "docker exec cobaltcore-control-plane ${MKDIR_LINE}
docker exec cobaltcore-worker ${MKDIR_LINE}" "$(cat "$tmp/docker.log")"
  assert_file_contains_fixed "helm gets the hostPath volume of the operator's directory" "$tmp/helm.log" \
    '--set-json extraVolumes=[{"name":"coverage","hostPath":{"path":"/var/lib/cobaltcore-coverage/keystone","type":"Directory"}}]'
  assert_file_contains_fixed "helm gets the mount at /coverage" "$tmp/helm.log" \
    '--set-json extraVolumeMounts=[{"name":"coverage","mountPath":"/coverage"}]'
  assert_file_contains_fixed "helm gets GOCOVERDIR=/coverage" "$tmp/helm.log" \
    '--set-json extraEnv=[{"name":"GOCOVERDIR","value":"/coverage"}]'
  assert_file_contains "helm install runs" "$tmp/helm.log" "helm install"
}

# ---------------------------------------------------------------------------
# Test 2: unset and false pass nothing and never call docker
# ---------------------------------------------------------------------------
test_coverage_off() {
  local value
  for value in UNSET false; do
    echo "Test: E2E_COVERAGE ${value} adds no coverage value"
    local tmp output rc
    tmp="$(new_tmp with-extra-volumes)"

    if [ "$value" = "UNSET" ]; then
      output="$(unset E2E_COVERAGE; KIND_NODES=cobaltcore-control-plane run_deploy "$tmp")"
    else
      output="$(E2E_COVERAGE="$value" KIND_NODES=cobaltcore-control-plane run_deploy "$tmp")"
    fi
    rc=$?

    assert_eq "deploy exits 0" "0" "$rc"
    assert_contains "the banner says false" "$output" "E2E coverage        : false"
    assert_file_contains "helm install runs" "$tmp/helm.log" "helm install"
    assert_file_not_contains "helm gets no extraVolumes" "$tmp/helm.log" "extraVolumes"
    assert_file_not_contains "helm gets no extraVolumeMounts" "$tmp/helm.log" "extraVolumeMounts"
    assert_file_not_contains "helm gets no extraEnv" "$tmp/helm.log" "extraEnv"
    assert_eq "docker is never called" "" "$(cat "$tmp/docker.log")"
    rm -rf "$tmp"
  done
}

# ---------------------------------------------------------------------------
# Test 3: a value other than true or false exits 1 before any apply
# ---------------------------------------------------------------------------
test_coverage_invalid() {
  echo "Test: E2E_COVERAGE=yes is rejected"
  local tmp output rc
  tmp="$(new_tmp with-extra-volumes)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(E2E_COVERAGE=yes KIND_NODES=cobaltcore-control-plane run_deploy "$tmp")"
  rc=$?

  assert_eq "deploy exits 1" "1" "$rc"
  assert_contains "the ::error:: line names the value" "$output" \
    "::error::E2E_COVERAGE='yes' is not true or false"
  assert_eq "kubectl is never called" "" "$(cat "$tmp/kubectl.log")"
  assert_eq "helm is never called" "" "$(cat "$tmp/helm.log")"
  assert_eq "docker is never called" "" "$(cat "$tmp/docker.log")"
}

# ---------------------------------------------------------------------------
# Test 4: a chart without extraVolumes exits 1 before the CRDs are applied
# ---------------------------------------------------------------------------
test_chart_without_extra_volumes() {
  echo "Test: E2E_COVERAGE=true refuses a chart without extraVolumes"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(E2E_COVERAGE=true KIND_NODES=cobaltcore-control-plane run_deploy "$tmp")"
  rc=$?

  assert_eq "deploy exits 1" "1" "$rc"
  assert_contains "the ::error:: line names the chart" "$output" \
    "::error::E2E_COVERAGE=true needs a chart with extraVolumes (operator-library 0.12.0 or later); $tmp/chart has none"
  assert_file_not_contains "no CRD is applied" "$tmp/kubectl.log" "apply"
  assert_eq "helm is never called" "" "$(cat "$tmp/helm.log")"
  assert_eq "docker is never called" "" "$(cat "$tmp/docker.log")"
}

# ---------------------------------------------------------------------------
# Test 5: a kind cluster without nodes exits 1 before helm runs
# ---------------------------------------------------------------------------
test_no_kind_nodes() {
  echo "Test: E2E_COVERAGE=true fails when kind lists no nodes"
  local tmp output rc
  tmp="$(new_tmp with-extra-volumes)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(E2E_COVERAGE=true KIND_NODES="" run_deploy "$tmp")"
  rc=$?

  assert_eq "deploy exits 1" "1" "$rc"
  assert_contains "the ::error:: line names the cluster" "$output" \
    "::error::E2E_COVERAGE=true but kind cluster cobaltcore has no nodes"
  assert_eq "helm is never called" "" "$(cat "$tmp/helm.log")"
  assert_file_not_contains "no CRD is applied" "$tmp/kubectl.log" "apply"
}

# ---------------------------------------------------------------------------
# Test 6: a failing docker exec exits 1 before helm runs
# ---------------------------------------------------------------------------
test_node_dir_creation_fails() {
  echo "Test: E2E_COVERAGE=true fails when the node directory cannot be created"
  local tmp output rc
  tmp="$(new_tmp with-extra-volumes)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(E2E_COVERAGE=true DOCKER_EXIT=1 KIND_NODES="cobaltcore-control-plane cobaltcore-worker" \
    run_deploy "$tmp")"
  rc=$?

  assert_eq "deploy exits 1" "1" "$rc"
  assert_contains "the ::error:: line names the directory and the node" "$output" \
    "::error::cannot create /var/lib/cobaltcore-coverage/keystone on node cobaltcore-control-plane"
  assert_eq "the second node is not tried" "1" "$(grep -c . "$tmp/docker.log")"
  assert_eq "helm is never called" "" "$(cat "$tmp/helm.log")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_coverage_on
test_coverage_off
test_coverage_invalid
test_chart_without_extra_volumes
test_no_kind_nodes
test_node_dir_creation_fails

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
