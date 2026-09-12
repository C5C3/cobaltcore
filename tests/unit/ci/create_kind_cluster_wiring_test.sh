#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify every e2e job creates its kind cluster through the create-kind-cluster
# composite action and tears it down with hack/ci-delete-kind-cluster.sh, and
# that the action still carries the pieces of helm/kind-action the jobs depend
# on.
#
# A job that reaches for `helm/kind-action` directly gets the behaviour that
# broke run 34707123151: `kind create cluster` and nothing else, so a cluster a
# cancelled job left on a self-hosted runner fails it in its first minute with
# `node(s) already exist for a cluster with the name "cobaltcore"`. That mistake
# is invisible — the job passes on a clean runner and fails on a dirty one — so
# it is pinned here rather than left to review.
#
# The teardown half is pinned for the same reason, from the other end: a job
# that creates a cluster and does not delete it leaves the post step of
# helm/kind-action to do it, one attempt with no retry, and run 34714006750 is
# what that costs — every suite passed, the deletion lost a docker race, and the
# job was reported failed.
#
# Usage: bash tests/unit/ci/create_kind_cluster_wiring_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"
ACTION_YAML="$PROJECT_ROOT/.github/actions/create-kind-cluster/action.yaml"
CREATE_SH="$PROJECT_ROOT/hack/ci-create-kind-cluster.sh"
RESET_SH="$PROJECT_ROOT/hack/ci-reset-kind-cluster.sh"
DELETE_SH="$PROJECT_ROOT/hack/ci-delete-kind-cluster.sh"

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
  # ...but it no longer decides whether the job failed. cleanup.sh runs
  # `kind delete cluster … || "${INPUT_IGNORE_FAILED_CLEAN}"`, so "false" here
  # is literally the command that fails the post step when a deletion loses a
  # race with the docker daemon. The deletion the job relies on is the
  # hack/ci-delete-kind-cluster.sh step, which runs earlier and retries.
  assert_file_contains "a failed post-step deletion cannot fail the job" \
    "$ACTION_YAML" 'ignore_failed_clean: "true"'
  assert_file_contains "the creation runs through the hack script" \
    "$ACTION_YAML" "run: hack/ci-create-kind-cluster.sh"
  assert_file_contains "the cluster name reaches the script" \
    "$ACTION_YAML" "CLUSTER_NAME: \${{ inputs.cluster-name }}"
  assert_file_contains "the config reaches the script" \
    "$ACTION_YAML" "KIND_CONFIG: \${{ inputs.config }}"
}

# ---------------------------------------------------------------------------
# Test D2: every job that creates a cluster also deletes it, last and always
# ---------------------------------------------------------------------------
test_every_job_tears_its_cluster_down() {
  echo "Test: every cluster-creating job ends with the teardown step"

  # Per job: does it create a cluster, does it run the teardown, is the teardown
  # the last step, and does it carry `if: always()`.
  local report
  report="$(awk '
    /^  [a-z0-9_-]+:$/ { job = $1; sub(/:$/, "", job) }
    /^      - / { last_step_job = job }
    /uses: \.\/\.github\/actions\/create-kind-cluster$/ { creates[job] = 1; jobs[job] = 1 }
    /^      - name: Delete kind cluster$/ { deletes[job] = 1; jobs[job] = 1; pending = job }
    pending != "" && /^        if: always\(\)$/ { guarded[pending] = 1; pending = "" }
    /run: hack\/ci-delete-kind-cluster\.sh$/ { runs_script[job] = 1; last_delete[job] = 1; next }
    /^      - / && $0 !~ /Delete kind cluster/ { last_delete[job] = 0 }
    END {
      for (j in jobs)
        print j, (creates[j] ? 1 : 0), (deletes[j] ? 1 : 0), \
              (guarded[j] ? 1 : 0), (runs_script[j] ? 1 : 0), (last_delete[j] ? 1 : 0)
    }
  ' "$CI_YAML" | sort)"

  local job creates deletes guarded runs_script is_last
  local checked=0
  while read -r job creates deletes guarded runs_script is_last; do
    [ -n "$job" ] || continue
    checked=$((checked + 1))
    assert_eq "$job creates a cluster and deletes it" "1 1" "$creates $deletes"
    assert_eq "$job guards the teardown with always()" "1" "$guarded"
    assert_eq "$job tears down through hack/ci-delete-kind-cluster.sh" "1" "$runs_script"
    # Last, so `Dump diagnostic info` still reads a live cluster.
    assert_eq "$job runs the teardown as its last step" "1" "$is_last"
  done <<<"$report"

  assert_gte "at least the eleven e2e jobs were checked" "$checked" "11"
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

  if [ -x "$DELETE_SH" ]; then
    assert_eq "hack/ci-delete-kind-cluster.sh is executable" "yes" "yes"
  else
    assert_eq "hack/ci-delete-kind-cluster.sh is executable" "yes" "no"
  fi

  assert_file_contains "the create script calls the reset script" \
    "$CREATE_SH" "ci-reset-kind-cluster.sh"
  assert_file_contains "the delete script sweeps through the reset script" \
    "$DELETE_SH" "ci-reset-kind-cluster.sh"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_no_direct_kind_action
test_every_step_uses_the_action
test_inputs_match
test_action_keeps_install_and_teardown
test_every_job_tears_its_cluster_down
test_scripts_are_executable

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
