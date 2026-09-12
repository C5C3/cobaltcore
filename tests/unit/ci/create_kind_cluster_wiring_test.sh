#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify every e2e job creates its kind cluster through the create-kind-cluster
# composite action, and that the action still carries the pieces of
# helm/kind-action the jobs depend on.
#
# A job that reaches for `helm/kind-action` directly gets the behaviour that
# broke run 34707123151: `kind create cluster` and nothing else, so a cluster a
# cancelled job left on a self-hosted runner fails it in its first minute with
# `node(s) already exist for a cluster with the name "cobaltcore"`. That mistake
# is invisible — the job passes on a clean runner and fails on a dirty one — so
# it is pinned here rather than left to review.
#
# Usage: bash tests/unit/ci/create_kind_cluster_wiring_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"
ACTION_YAML="$PROJECT_ROOT/.github/actions/create-kind-cluster/action.yaml"
CREATE_SH="$PROJECT_ROOT/hack/ci-create-kind-cluster.sh"
RESET_SH="$PROJECT_ROOT/hack/ci-reset-kind-cluster.sh"

ACTION_REF="./.github/actions/create-kind-cluster"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# cluster_creating_steps — For every workflow step that passes a `cluster-name:`
# input, echo the action it uses. Walks the file rather than matching step names,
# so a new job that spells its step differently is covered too.
cluster_creating_steps() {
  awk '
    /^      - name: / || /^      - uses: / { uses = "" }
    /^        uses: / { uses = $2 }
    /^          cluster-name: / { print (uses == "" ? "<none>" : uses) }
  ' "$CI_YAML"
}

# kind_config_inputs — Echo every `config:` value passed to a create-kind-cluster
# step, one per line.
kind_config_inputs() {
  awk -v ref="$ACTION_REF" '
    $0 == "        uses: " ref { in_step = 1; next }
    in_step && /^        [a-z]/ && !/^        with:/ { in_step = 0 }
    in_step && /^          config: / { print $2 }
  ' "$CI_YAML"
}

# ---------------------------------------------------------------------------
# Test A: no job creates a cluster on its own any more
# ---------------------------------------------------------------------------
test_no_direct_kind_action() {
  echo "Test: no job uses helm/kind-action directly"

  local direct
  direct="$(grep -c "uses: helm/kind-action" "$CI_YAML" | tr -d ' ')"

  assert_eq "ci.yaml has no helm/kind-action step of its own" "0" "$direct"
  assert_file_contains "the composite action is the one place it is used" \
    "$ACTION_YAML" "uses: helm/kind-action@"
}

# ---------------------------------------------------------------------------
# Test B: every cluster-creating step goes through the composite action
# ---------------------------------------------------------------------------
test_every_step_uses_the_action() {
  echo "Test: every cluster-creating step uses the composite action"

  local steps count wrong
  steps="$(cluster_creating_steps)"
  count="$(printf '%s\n' "$steps" | sed '/^$/d' | wc -l | tr -d ' ')"
  wrong="$(printf '%s\n' "$steps" | sed '/^$/d' | grep -vc "^${ACTION_REF}$" || true)"

  assert_gte "at least the eleven e2e jobs create a cluster" "$count" "11"
  assert_eq "every one of them uses ${ACTION_REF}" "0" "$wrong"

  local uses
  uses="$(grep -c "uses: ${ACTION_REF}$" "$CI_YAML" | tr -d ' ')"
  assert_eq "no step uses the action without naming a cluster" "$count" "$uses"
}

# ---------------------------------------------------------------------------
# Test C: the inputs the action needs are the inputs the jobs pass
# ---------------------------------------------------------------------------
test_inputs_match() {
  echo "Test: the jobs pass the inputs the action declares"

  local version_inputs cluster_inputs
  version_inputs="$(grep -c "^          version: \${{ env.KIND_VERSION }}$" "$CI_YAML" | tr -d ' ')"
  cluster_inputs="$(cluster_creating_steps | sed '/^$/d' | wc -l | tr -d ' ')"

  assert_eq "each cluster-creating step pins the kind version from env.KIND_VERSION" \
    "$cluster_inputs" "$version_inputs"

  assert_file_contains "the action declares version" "$ACTION_YAML" "^  version:"
  assert_file_contains "the action declares cluster-name" "$ACTION_YAML" "^  cluster-name:"
  assert_file_contains "the action declares config" "$ACTION_YAML" "^  config:"

  local config
  while IFS= read -r config; do
    [ -n "$config" ] || continue
    if [ -f "$PROJECT_ROOT/$config" ]; then
      assert_eq "kind config '$config' exists" "yes" "yes"
    else
      assert_eq "kind config '$config' exists" "yes" "no"
    fi
  done < <(kind_config_inputs | sort -u)
}

# ---------------------------------------------------------------------------
# Test D: what the action keeps from helm/kind-action
# ---------------------------------------------------------------------------
test_action_keeps_install_and_teardown() {
  echo "Test: the action installs the binaries and keeps the post-job teardown"

  assert_file_contains "helm/kind-action is SHA-pinned with a version comment" \
    "$ACTION_YAML" "uses: helm/kind-action@[0-9a-f]\{40\} # v[0-9]"
  assert_file_contains "it runs install-only, so the creation is ours" \
    "$ACTION_YAML" 'install_only: "true"'
  # The post step of helm/kind-action runs `kind delete cluster --name
  # <cluster_name>`: without this input it would delete `chart-testing`, its
  # default, and leave this job's cluster on the runner — the very leftover the
  # reset exists to clear.
  assert_file_contains "the teardown still points at this job's cluster" \
    "$ACTION_YAML" "cluster_name: \${{ inputs.cluster-name }}"
  assert_file_contains "the creation runs through the hack script" \
    "$ACTION_YAML" "run: hack/ci-create-kind-cluster.sh"
  assert_file_contains "the cluster name reaches the script" \
    "$ACTION_YAML" "CLUSTER_NAME: \${{ inputs.cluster-name }}"
  assert_file_contains "the config reaches the script" \
    "$ACTION_YAML" "KIND_CONFIG: \${{ inputs.config }}"
}

# ---------------------------------------------------------------------------
# Test E: the scripts the action calls can be executed
# ---------------------------------------------------------------------------
test_scripts_are_executable() {
  echo "Test: both scripts are executable"

  # `run: hack/ci-create-kind-cluster.sh` executes the file directly, and the
  # create script executes the reset script the same way.
  if [ -x "$CREATE_SH" ]; then
    assert_eq "hack/ci-create-kind-cluster.sh is executable" "yes" "yes"
  else
    assert_eq "hack/ci-create-kind-cluster.sh is executable" "yes" "no"
  fi

  if [ -x "$RESET_SH" ]; then
    assert_eq "hack/ci-reset-kind-cluster.sh is executable" "yes" "yes"
  else
    assert_eq "hack/ci-reset-kind-cluster.sh is executable" "yes" "no"
  fi

  assert_file_contains "the create script calls the reset script" \
    "$CREATE_SH" "ci-reset-kind-cluster.sh"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_no_direct_kind_action
test_every_step_uses_the_action
test_inputs_match
test_action_keeps_install_and_teardown
test_scripts_are_executable

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
