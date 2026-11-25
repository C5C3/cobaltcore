#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Shared block extractors for the tests that assert the wiring of the CI
# workflow: .github/workflows/ci.yaml, the reusable workflows it calls and the
# path filters of .github/ci-filters.yaml.
#
# The workflow is asserted against by grepping scoped slices of it, and the
# slicing rules are one piece of knowledge: a job body ends at the next 2-space
# line, a step body at the next 6-space one, a matrix entry at the next
# 10-space one, a filter at the next top-level key. Kept here so a change to
# those rules is edited once instead of once per test.
#
# A job of ci.yaml that calls a reusable workflow (`uses:
# ./.github/workflows/<file>`) keeps its needs, its if and the inputs it passes
# in ci.yaml, and its runner, timeout, matrix and steps in the called file.
# job_block prints both halves as one body, so a test reads the job the way it
# runs and does not care which file a key lives in.
#
# A helper that finds nothing ends the test with a failure. An empty slice
# would let every negative assertion on it pass: a renamed job, step or filter
# must fail the test that names it. Use job_exists, step_exists or
# filter_exists to ask without failing.
#
# Source this after tests/lib/assertions.sh, with CI_YAML set. CI_FILTERS
# defaults to the ci-filters.yaml beside the workflow directory of CI_YAML.

# _ci_yaml_missing <message> — report what a helper did not find and end the
# test. The helpers run inside $(...), where an exit ends the subshell alone,
# so the test's own shell is signalled to stop.
_ci_yaml_missing() {
  echo "  FAIL: $* (tests/lib/ci_yaml.sh found nothing to assert against)" >&2
  kill -s USR1 "$$"
  exit 1
}
trap 'echo "Results: aborted, a ci_yaml.sh helper found nothing" >&2; exit 1' USR1

# ci_filters_file — the path filters the changes job hands to paths-filter.
ci_filters_file() {
  if [ -n "${CI_FILTERS:-}" ]; then
    printf '%s\n' "$CI_FILTERS"
  else
    printf '%s\n' "$(dirname "$(dirname "$CI_YAML")")/ci-filters.yaml"
  fi
}

# filter_exists <filter-key>
filter_exists() {
  grep -qx "$1:" "$(ci_filters_file)" 2>/dev/null
}

# filter_block <filter-key>
#
# Echo the path list of the given filter key. Filter keys sit at the top level
# of ci-filters.yaml and their entries deeper, so the next key ends the block.
# Scoping matters for shared entries such as flake.lock, which go_common and the
# image filters list.
filter_block() {
  filter_exists "$1" || _ci_yaml_missing "no filter '$1' in $(ci_filters_file)"
  awk -v key="$1:" '
    $0 == key { in_block = 1; next }
    in_block && /^[a-z0-9_]+:$/ { exit }
    in_block { print }
  ' "$(ci_filters_file)"
}

# filter_names — every filter key of ci-filters.yaml, in file order.
filter_names() {
  grep -E '^[a-z0-9_]+:$' "$(ci_filters_file)" | sed 's/:$//'
}

# _job_block_in <file> <job> — the raw body of the job in one workflow file.
_job_block_in() {
  awk -v key="  $2:" '
    /^jobs:$/ { in_jobs = 1; next }
    in_jobs && $0 == key { in_block = 1; next }
    in_block && /^  [#a-z0-9-]/ { exit }
    in_block { print }
  ' "$1"
}

# job_exists <job> — whether ci.yaml has a job of that name.
job_exists() {
  [ -f "$CI_YAML" ] || return 1
  awk -v key="  $1:" '/^jobs:$/ { in_jobs = 1; next } in_jobs && $0 == key { found = 1; exit } END { exit !found }' "$CI_YAML"
}

# called_workflow <job>
#
# Echo the path of the reusable workflow the job calls, empty when the job runs
# its own steps.
called_workflow() {
  local uses
  uses=$(_job_block_in "$CI_YAML" "$1" | sed -n 's|^    uses: \./\.github/workflows/\([^ ]*\)$|\1|p' | head -1)
  [ -n "$uses" ] && printf '%s\n' "$(dirname "$CI_YAML")/$uses"
}

# job_caller <job>
#
# Echo the part of the job ci.yaml holds itself. For a job that calls a
# reusable workflow that is its needs, if, permissions, uses and with; for any
# other job it is the whole body, as job_block prints it.
job_caller() {
  job_exists "$1" || _ci_yaml_missing "no job '$1' in $CI_YAML"
  _job_block_in "$CI_YAML" "$1"
}

# job_workflow <job>
#
# Echo the workflow file whose jobs: map holds the steps of the ci.yaml job:
# the reusable workflow it calls, else ci.yaml itself. For yq queries such as
# .jobs["<job>"].steps, which read one file.
job_workflow() {
  job_exists "$1" || _ci_yaml_missing "no job '$1' in $CI_YAML"
  local called
  called=$(called_workflow "$1")
  printf '%s\n' "${called:-$CI_YAML}"
}

# ci_workflows
#
# Echo ci.yaml and every reusable workflow it calls, one path per line: the
# files a check over the whole CI run reads.
ci_workflows() {
  printf '%s\n' "$CI_YAML"
  sed -n 's|^    uses: \./\.github/workflows/\([^ ]*\)$|\1|p' "$CI_YAML" | sort -u |
    while read -r called; do
      printf '%s\n' "$(dirname "$CI_YAML")/$called"
    done
}

# job_block <job>
#
# Echo the body of the top-level ci.yaml job. The block ends at the next 2-space
# line that is not part of the body: the next job key, or the comment header
# introducing it. A job that calls a reusable workflow gets the body of the
# called job appended: the job of the same name in that file, else its first.
job_block() {
  job_exists "$1" || _ci_yaml_missing "no job '$1' in $CI_YAML"
  _job_block_in "$CI_YAML" "$1"
  local called callee
  called=$(called_workflow "$1")
  [ -n "$called" ] || return 0
  [ -f "$called" ] || _ci_yaml_missing "job '$1' calls $called, which does not exist"
  callee="$1"
  if ! grep -qx "  $1:" "$called"; then
    callee=$(awk '/^jobs:$/ { in_jobs = 1; next } in_jobs && /^  [a-z0-9-]+:$/ { gsub(/[ :]/, ""); print; exit }' "$called")
  fi
  [ -n "$callee" ] || _ci_yaml_missing "$called defines no job"
  _job_block_in "$called" "$callee"
}

# job_names
#
# Echo every top-level job of ci.yaml, in file order. Job keys sit at 2-space
# indent under jobs:, the same lines job_block starts a body at.
job_names() {
  awk '
    /^jobs:$/ { in_jobs = 1; next }
    in_jobs && /^  [a-z0-9-]+:$/ { gsub(/[ :]/, ""); print }
  ' "$CI_YAML"
}

# step_exists <job> <step-name>
step_exists() {
  job_exists "$1" || return 1
  local body
  body=$(job_block "$1")
  grep -qxF -- "      - name: $2" <<<"$body"
}

# job_step <job> <step-name>
#
# Echo the body of the named step of the named job. Steps sit at 6-space
# indent, so the next line at that indent ends the block: the following step,
# or the comment introducing it. Scoping to the job matters because
# e2e-operator, e2e-chaos and tempest all carry a "Load E2E images" step.
job_step() {
  step_exists "$1" "$2" || _ci_yaml_missing "no step '$2' in job '$1'"
  job_block "$1" | awk -v key="      - name: $2" '
    $0 == key { in_block = 1; next }
    in_block && /^      [-#]/ { exit }
    in_block { print }
  '
}

# e2e_chaos_matrix_entry <suite>
#
# Echo the e2e-chaos matrix entry whose suite key is <suite>. Entries sit at
# 10-space indent and their keys deeper, so the next line at that indent ends
# the block: the following entry, or the comment introducing it.
e2e_chaos_matrix_entry() {
  local body
  body=$(job_block e2e-chaos)
  grep -qxF -- "          - suite: $1" <<<"$body" ||
    _ci_yaml_missing "no e2e-chaos matrix entry '$1'"
  awk -v key="          - suite: $1" '
    $0 == key { in_block = 1; next }
    in_block && /^          [-#]/ { exit }
    in_block { print }
  ' <<<"$body"
}

# workflow_kind_pin <file>
#
# Echo the KIND_VERSION the env of the workflow <file> pins, quoted or not.
workflow_kind_pin() {
  sed -n 's/^[[:space:]]*KIND_VERSION:[[:space:]]*"\{0,1\}\(v[0-9.]*\)"\{0,1\}[[:space:]]*$/\1/p' "$1" | head -1
}
