#!/usr/bin/env bats
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify every CI step that reaches github.com with the runner's git hands the
# workflow token to the script it runs.
#
# hack/ci-deploy-korc.sh only authenticates when GITHUB_TOKEN is set. A step
# that forgets the env clones anonymously,
# which the self-hosted runners' git (Ubuntu 24.04's 2.43) cannot rely on
# since 2026-09-02: GitHub answers the anonymous upload-pack request with a
# 401 challenge and the clone dies on "could not read Username for
# 'https://github.com'". The list of steps is derived from ci.yaml itself, so
# a new caller of the script is caught the moment it lands without the env.
#
# Usage: bin/bats tests/unit/ci/github_git_auth_wiring.bats

load '../../lib/bats/common'

CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

TOKEN_ENV='GITHUB_TOKEN: ${{ github.token }}'
CLONING_SCRIPTS="hack/ci-deploy-korc.sh"

# steps_running <script>
#
# Echo "<job><TAB><step name>" for every ci.yaml step whose body invokes
# <script>. Comment lines and the paths-filter entries (list items) that name
# the script are not invocations and are skipped.
steps_running() {
  awk -v script="$1" '
    /^  [a-z0-9-]+:$/ { job = $1; sub(/:$/, "", job) }
    /^      - name: / { step = $0; sub(/^      - name: /, "", step) }
    index($0, script) && $0 !~ /^ *#/ && $0 !~ /^ *- / { print job "\t" step }
  ' "$CI_YAML" | sort -u
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

@test "every ci.yaml step running a github.com-cloning script sets GITHUB_TOKEN" {
  # A script without a caller in ci.yaml is fine; the test of the known
  # callers below keeps the derivation itself honest.
  local script found job step body
  for script in $CLONING_SCRIPTS; do
    found="$(steps_running "$script")"
    while IFS=$'\t' read -r job step; do
      [ -n "$job" ] || continue
      body="$(job_step "$job" "$step")"
      soft assert_not_empty "$job / '$step' is a step ci_yaml.sh can slice" "$body"
      soft assert_contains "$job / '$step' hands the workflow token to $script" "$body" "$TOKEN_ENV"
    done <<<"$found"
  done
}

@test "the derivation sees the callers that exist today" {
  # A regression in steps_running() that finds nothing would pass the loop
  # above vacuously, so pin the callers known at the time of writing.
  local korc_steps
  korc_steps="$(steps_running hack/ci-deploy-korc.sh)"
  assert_contains "e2e-operator installs K-ORC for the c5c3 leg" "$korc_steps" \
    "e2e-operator	Install CRDs watched by the c5c3-operator"
  assert_contains "e2e-controlplane deploys K-ORC" "$korc_steps" "e2e-controlplane	Deploy K-ORC"
}
