#!/usr/bin/env bats
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify every deploy-infra flag a ci.yaml job sets on its "Setup E2E
# infrastructure" step reaches hack/deploy-infra.sh through the
# setup-e2e-infra composite action.
#
# A composite action sees the caller's step env, but the action's own
# "Deploy infrastructure stack" step hands make deploy-infra only the keys its
# env block names as KEY: ${{ env.KEY }}. A key a job sets that the action
# does not thread is dropped without a warning, and make deploy-infra installs
# the default stack: the chaos legs race a missing chaos-controller-manager,
# the ControlPlane legs come up without a ControlPlane, the cinder legs without
# an NFS export. A key the action threads that deploy-infra.sh never reads is
# the same silence one hop later. And the quick-start canary, e2e-infra, has to
# keep setting no WITH_ key, or every run installs an opt-in it was meant to
# leave out.
#
# Usage: bin/bats tests/unit/ci/setup_e2e_infra_env_threading.bats

load '../../lib/bats/common'

# shellcheck disable=SC2034 # read by tests/lib/ci_yaml.sh
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"
ACTION_YAML="$PROJECT_ROOT/.github/actions/setup-e2e-infra/action.yaml"
DEPLOY_INFRA="$PROJECT_ROOT/hack/deploy-infra.sh"

# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Echo the action's "Deploy infrastructure stack" step, name line excluded, up
# to and including its run: line.
deploy_step() {
  awk '
    /^    - name: Deploy infrastructure stack$/ { in_step = 1; next }
    in_step { print }
    in_step && /^      run: / { exit }
  ' "$ACTION_YAML"
}

# Echo every KEY the deploy step threads as `KEY: ${{ env.KEY }}`, one per
# line.
threaded_keys() {
  deploy_step | awk '
    /^        [A-Z_][A-Z0-9_]*: / {
      key = $1; sub(/:$/, "", key)
      if ($0 == "        " key ": ${{ env." key " }}") print key
    }
  '
}

# Echo every KEY a job's "Setup E2E infrastructure" step body sets in its env
# block, one per line.
setup_step_keys() {
  awk '
    /^          [A-Z_][A-Z0-9_]*:/ { key = $1; sub(/:$/, "", key); print key }
  ' <<<"$1"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

@test "setup-e2e-infra threads every key a job's setup step sets" {
  local threaded job step keys key jobs_with_step=""
  threaded=$(threaded_keys)
  assert_not_empty "the action threads at least one key" "$threaded"

  for job in $(job_names); do
    step=$(job_step "$job" "Setup E2E infrastructure")
    [ -n "$step" ] || continue
    jobs_with_step="${jobs_with_step} ${job}"

    keys=$(setup_step_keys "$step")
    if [ -z "$keys" ]; then
      check_passed "$job sets no key, so every flag keeps its deploy-infra.sh default"
      continue
    fi
    for key in $keys; do
      if grep -qx -- "$key" <<<"$threaded"; then
        check_passed "$job sets $key and setup-e2e-infra threads it"
      else
        soft fail "$job sets $key but setup-e2e-infra does not thread it"
      fi
    done
  done

  assert_not_empty "at least one job carries the setup step" "$jobs_with_step"
}

@test "every key the action threads is read by hack/deploy-infra.sh" {
  local key step
  for key in $(threaded_keys); do
    soft assert_file_contains_fixed "deploy-infra.sh reads $key" "$DEPLOY_INFRA" \
      "\${$key:-"
  done

  # The action assumes the calling job created the kind cluster already, so
  # deploy-infra.sh must not create a second one.
  step=$(deploy_step)
  assert_contains "the step hard-codes SKIP_KIND_CREATE" "$step" \
    'SKIP_KIND_CREATE: "true"'
  assert_eq "the step ends in make deploy-infra" "      run: make deploy-infra" \
    "$(tail -1 <<<"$step")"
}

@test "the e2e-infra canary keeps the quick-start default stack" {
  local step
  step=$(job_step e2e-infra "Setup E2E infrastructure")
  assert_not_empty "e2e-infra carries the setup step" "$step"
  assert_contains "it uses the shared composite action" "$step" \
    "uses: ./.github/actions/setup-e2e-infra"
  assert_not_contains "e2e-infra sets no WITH_ key, so the canary keeps the quick-start default" \
    "$step" "WITH_"
}
