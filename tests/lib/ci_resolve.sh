#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Shared helpers for the tests that pin a hack/ci-resolve-changes.sh output to
# the ci.yaml jobs consuming it (tests/unit/ci/*_output_test.sh).
#
# The resolve-script invocation contract is one piece of knowledge, and three
# output tests exercise it. Kept here so a change to that contract — a new
# required env var, a renamed GITHUB_OUTPUT shape — is edited once instead of
# once per output test, where missing one leaves a test passing for the wrong
# reason.
#
# Source this after tests/lib/assertions.sh, with PROJECT_ROOT and CI_YAML set.
#
# SERVICE_OPERATORS and CANARY_OPERATOR are supplied as defaults so a caller that
# only cares about ALL_OPERATORS keeps working; both are overridden by anything
# the caller passes, including an empty value, since the caller's assignments are
# applied after these.

# The service operators (the union of the keys in releases/*/source-refs.yaml) and
# the canary operator the ci.yaml resolve step passes. Kept here so a test that
# does not care about either still satisfies the resolve script's guards.
CI_RESOLVE_DEFAULT_SERVICE_OPERATORS="keystone horizon glance placement barbican neutron"
CI_RESOLVE_DEFAULT_CANARY_OPERATOR="keystone"

# resolve_output <output-key> <ref> <all-operators> [ENV=value ...]
#
# Runs hack/ci-resolve-changes.sh for real with the given ref and env, and
# echoes the `<output-key>=...` line it wrote to GITHUB_OUTPUT.
#
# The script emits its per-job outputs well before the final jq matrix step, so
# a later abort still leaves a greppable line behind. Echoing the exit status
# instead of that stale line keeps a broken run from passing an assertion while
# the real "Resolve effective changes" step would fail the whole changes job.
resolve_output() {
  local key="$1" ref="$2" all_operators="$3"
  shift 3
  local out rc

  out=$(mktemp)
  env \
    SERVICE_OPERATORS="$CI_RESOLVE_DEFAULT_SERVICE_OPERATORS" \
    CANARY_OPERATOR="$CI_RESOLVE_DEFAULT_CANARY_OPERATOR" \
    "$@" \
    ALL_OPERATORS="$all_operators" \
    GITHUB_OUTPUT="$out" \
    GITHUB_REF="$ref" \
    bash "$PROJECT_ROOT/hack/ci-resolve-changes.sh" >/dev/null 2>&1
  rc=$?

  if [ "$rc" -ne 0 ]; then
    echo "resolve script exited $rc"
  else
    grep "^${key}=" "$out"
  fi
  rm -f "$out"
}

# resolve_outputs <ref> <all-operators> [ENV=value ...]
#
# Same contract as resolve_output, but echoes the whole GITHUB_OUTPUT rather than
# one line, so a scenario test can assert several outputs from a single run.
# Echoes the exit status line on failure for the same reason resolve_output does.
resolve_outputs() {
  local ref="$1" all_operators="$2"
  shift 2
  local out rc

  out=$(mktemp)
  env \
    SERVICE_OPERATORS="$CI_RESOLVE_DEFAULT_SERVICE_OPERATORS" \
    CANARY_OPERATOR="$CI_RESOLVE_DEFAULT_CANARY_OPERATOR" \
    "$@" \
    ALL_OPERATORS="$all_operators" \
    GITHUB_OUTPUT="$out" \
    GITHUB_REF="$ref" \
    bash "$PROJECT_ROOT/hack/ci-resolve-changes.sh" >/dev/null 2>&1
  rc=$?

  if [ "$rc" -ne 0 ]; then
    echo "resolve script exited $rc"
  else
    cat "$out"
  fi
  rm -f "$out"
}

# assert_filter_is_wired <filter-name> <output-key>
#
# Asserts the three declarative ci.yaml sides of a change signal: the paths
# filter, the FILTER_ env var handed to the resolve step, and the output export
# on the changes job. The consuming side stays with the caller — a signal may
# have one consumer or several, and each test pins its own.
assert_filter_is_wired() {
  local filter="$1" output="$2"

  assert_file_contains "the paths filter exists" "$CI_YAML" \
    "^ *${filter}:$"
  assert_file_contains "the filter is passed to the resolve step" "$CI_YAML" \
    "FILTER_${filter}: \${{ steps.filter.outputs.${filter} }}"
  assert_file_contains "the changes job exports the output" "$CI_YAML" \
    "${output}: \${{ steps.result.outputs.${output} }}"
}
