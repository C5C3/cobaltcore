#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/tempest/run-tests.sh — In-container Tempest execution helper.
#
# Runs Tempest in two sequential stestr phases (core tempest.api.* then
# <service>_tempest_plugin.*) and retries any failed tests once serially to
# absorb cross-suite race flakes. Sourced into the Tempest container by both
# hack/ci-run-tempest.sh and hack/run-tempest.sh so the retry/exit-code logic
# lives in one place and cannot drift between CI and local runs.
#
# A phase that executed no test fails the run: an include list that matches
# nothing would otherwise report tests="0" failures="0" and exit green. Checked
# per phase, because on a leg that selects both scopes the phase that ran keeps
# the merged count positive and masks the phase that did not.
#
# Either phase is optional: when phase-2-plugin.txt is empty (a pure
# tempest.api.* include list, e.g. the glance image leg) the plugin phase is
# skipped, and when phase-1-core.txt is empty (a pure <service>_tempest_plugin.*
# include list, e.g. the barbican key-manager leg) the core phase is, instead of
# running stestr with an empty include list. Both empty is rejected by the
# calling wrapper before the container starts.
#
# Expected layout inside the container:
#   /etc/tempest/tempest.conf
#   /etc/tempest/phases/phase-1-core.txt     (may be empty; core phase skipped)
#   /etc/tempest/phases/phase-2-plugin.txt   (may be empty; plugin phase skipped)
#   /etc/tempest/extract-failed.py
#   /etc/tempest/merge-retry-junit.py
#   /etc/tempest/exclude-tests.txt       (optional)
#   /output/                              (mounted, writable)
#
# Required env vars:
#   TEMPEST_CONCURRENCY — stestr worker count for the parallel phases
#
# Optional env vars:
#   TEMPEST_GROUP_START  — log group-start marker (default: "--- ")
#   TEMPEST_GROUP_END    — log group-end marker   (default: empty; CI sets it)
#   TEMPEST_ERROR_PREFIX — error line prefix      (default: "ERROR: ")

set -euo pipefail

: "${TEMPEST_CONCURRENCY:?TEMPEST_CONCURRENCY must be set}"
GROUP_START="${TEMPEST_GROUP_START:---- }"
GROUP_END="${TEMPEST_GROUP_END:-}"
ERROR_PREFIX="${TEMPEST_ERROR_PREFIX:-ERROR: }"

# HOME=/tmp: the openstack user's real home (/var/lib/openstack) is owned by
# root, so tempest cannot create ~/.tempest there. Redirect to /tmp.
export HOME=/tmp
mkdir -p /tmp/tempest-workspace /tmp/tempest-logs
cd /tmp/tempest-workspace
tempest init .
cp /etc/tempest/tempest.conf etc/tempest.conf

exclude_args=''
if [[ -f /etc/tempest/exclude-tests.txt ]]; then
  exclude_args='--exclude-list /etc/tempest/exclude-tests.txt'
fi

group_end() {
  if [[ -n "${GROUP_END}" ]]; then
    echo "${GROUP_END}"
  fi
}

run_phase() {
  local phase=$1 concurrency=$2 include_list=$3 subunit_out=$4
  echo
  echo "${GROUP_START}Tempest ${phase} (concurrency=${concurrency})"
  set +e
  # shellcheck disable=SC2086  # intentional word-splitting on exclude_args
  stestr run --include-list ${include_list} ${exclude_args} --concurrency ${concurrency} --subunit \
    | tee ${subunit_out} \
    | subunit2pyunit 2>&1 \
    | grep --line-buffered -E '\.\.\. '
  local phase_rc=${PIPESTATUS[0]}
  set -e
  group_end
  return ${phase_rc}
}

# Phase 1 is optional: an include list that only selects plugin tests (e.g. the
# barbican key-manager leg) has no tempest.* patterns, so the phase-1 file is
# empty and the core phase is skipped entirely. A skipped phase still needs an
# (empty) subunit so the concatenations below work.
overall_rc=0
if [[ -s /etc/tempest/phases/phase-1-core.txt ]]; then
  run_phase phase-1-core "${TEMPEST_CONCURRENCY}" \
    /etc/tempest/phases/phase-1-core.txt /output/phase-1-core.subunit || overall_rc=$?
else
  echo "phase-1-core: no patterns, skipping"
  : > /output/phase-1-core.subunit
fi

# Phase 2 is optional for the mirror-image reason: an include list that only
# selects core tempest.api.* tests (e.g. the glance image leg) has no
# <service>_tempest_plugin.* patterns, so the phase-2 file is empty and the
# plugin phase is skipped entirely.
phase2_rc=0
if [[ -s /etc/tempest/phases/phase-2-plugin.txt ]]; then
  run_phase phase-2-plugin "${TEMPEST_CONCURRENCY}" \
    /etc/tempest/phases/phase-2-plugin.txt /output/phase-2-plugin.subunit || phase2_rc=$?
else
  echo "phase-2-plugin: no patterns, skipping"
  : > /output/phase-2-plugin.subunit
fi
if [[ ${phase2_rc} -gt ${overall_rc} ]]; then
  overall_rc=${phase2_rc}
fi

# Subunit v2 is stream-concatenation safe, so cat'ing the two phase streams
# produces a single valid subunit stream.
cat /output/phase-1-core.subunit /output/phase-2-plugin.subunit > /output/tempest.subunit
subunit2junitxml < /output/tempest.subunit > /output/tempest-results.xml 2>/dev/null || true

# Floor, per phase: a phase that had patterns to run but executed no test is a
# failure, not a pass. An include list whose patterns match nothing — an upstream
# plugin bump that renamed the module path, or a plugin package that dropped out
# of the rebuilt image — makes stestr run zero tests and the JUnit report reads
# tests="0" failures="0" errors="0", which every check below reads as green. The
# calling wrapper only proves the include list is non-empty.
#
# Per phase and NOT on the merged report, because on a leg that selects both
# scopes (keystone selects tempest.api.identity AND keystone_tempest_plugin.tests)
# the phase that ran is what hides the phase that did not: its count keeps the
# merged tests=" above zero while the other phase's whole suite is silently gone.
assert_phase_ran() {
  local phase=$1 pattern_file=$2 subunit=$3 count
  # No patterns means the phase was skipped, not emptied — nothing to prove.
  [[ -s "${pattern_file}" ]] || return 0
  # sed quits on the first match, which SIGPIPEs subunit2junitxml; the trailing
  # `|| true` absorbs that so pipefail does not abort the script with no message.
  # The count is already captured by then — it sits in the opening testsuite tag.
  count=$(subunit2junitxml < "${subunit}" 2>/dev/null \
    | sed -n '/tests="/{s/.*tests="\([0-9]*\)".*/\1/p;q;}' || true)
  if [[ -z "${count}" || "${count}" -eq 0 ]]; then
    echo "${ERROR_PREFIX}Tempest ${phase} executed 0 tests (JUnit reports tests=\"${count:-<none>}\") — its include list matched nothing."
    exit 1
  fi
}
assert_phase_ran phase-1-core /etc/tempest/phases/phase-1-core.txt /output/phase-1-core.subunit
assert_phase_ran phase-2-plugin /etc/tempest/phases/phase-2-plugin.txt /output/phase-2-plugin.subunit

# Floor, merged: the per-phase checks cannot see a merged report that came out
# unparseable or empty for a reason outside either phase. Read before the retry
# so that case is caught here too: the retry rewrites this file, and no report
# means nothing proved a test ran.
total_tests=""
if [[ -f /output/tempest-results.xml ]]; then
  # Quit on the first match rather than piping into head: under pipefail a
  # SIGPIPE'd sed would abort the script with no message at all.
  total_tests=$(sed -n '/tests="/{s/.*tests="\([0-9]*\)".*/\1/p;q;}' /output/tempest-results.xml)
fi
if [[ -z "${total_tests}" || "${total_tests}" -eq 0 ]]; then
  echo "${ERROR_PREFIX}Tempest executed 0 tests (merged JUnit reports tests=\"${total_tests:-<none>}\") — no report proves a test ran."
  exit 1
fi

# Retry: if the JUnit report lists any failed or errored tests, re-run just
# those tests once serially (concurrency 1) to absorb cross-suite race flakes.
# Tests that pass on retry are rewritten as flakes, not failures, in the JUnit
# report. Tests that still fail stay as failures. If either helper (extract or
# merge) crashes we skip retry and fall through to the original exit code
# instead of aborting the whole run.
retried=0
if [[ -f /output/tempest-results.xml ]] \
   && grep -qE 'failures="[1-9]|errors="[1-9]' /output/tempest-results.xml; then
  : > /tmp/retry-list.txt
  if ! python3 /etc/tempest/extract-failed.py /output/tempest-results.xml \
       > /tmp/retry-list.txt; then
    echo "${ERROR_PREFIX}extract-failed.py failed; skipping retry."
    : > /tmp/retry-list.txt
  fi
  retry_count=$(wc -l < /tmp/retry-list.txt | tr -d ' ')
  if [[ ${retry_count} -gt 0 ]]; then
    retried=1
    echo
    echo "${GROUP_START}Tempest retry: ${retry_count} failed test(s), rerunning serially"
    cat /tmp/retry-list.txt
    run_phase retry 1 /tmp/retry-list.txt /output/retry.subunit || true

    cat /output/phase-1-core.subunit /output/phase-2-plugin.subunit /output/retry.subunit \
      > /output/tempest.subunit
    if ! python3 /etc/tempest/merge-retry-junit.py \
         /output/tempest-results.xml /output/retry.subunit; then
      echo "${ERROR_PREFIX}merge-retry-junit.py failed; retry results not merged."
      # The JUnit report is stale; let the exit-code logic below re-read it
      # and fail on any remaining failures rather than claiming success.
      retried=0
    fi
  fi
fi

# Decide exit code:
#   - If the JUnit report still lists failures/errors, fail.
#   - Else if we retried and the rewritten report is clean, succeed (the
#     original non-zero stestr exit was due to resolved flakes).
#   - Else fall through to the original phase exit code so an infra-level
#     stestr crash that left the JUnit incomplete still fails.
if [[ -f /output/tempest-results.xml ]] \
   && grep -qE 'failures="[1-9]|errors="[1-9]' /output/tempest-results.xml; then
  echo "${ERROR_PREFIX}Tempest reported test failures (including after retry)."
  overall_rc=1
elif [[ ${retried} -eq 1 ]]; then
  overall_rc=0
fi
exit ${overall_rc}
