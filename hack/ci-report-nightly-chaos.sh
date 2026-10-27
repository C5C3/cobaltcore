#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-report-nightly-chaos.sh — File a red nightly chaos run per suite,
# leg, run and runner budget.
#
# The report-nightly-chaos job of .github/workflows/ci.yaml runs it after every
# scheduled night. It lists the jobs of the run and files each red finding
# through hack/ci-report-red-run.sh, which comments on the open
# github-actions[bot] issue of that title or opens one:
#
#   Nightly chaos suite <suite> is red   one per failed suite of a red leg, read
#                                        from the leg's JUnit report by
#                                        hack/ci-junit-failed-suites.py
#   Nightly chaos leg <leg> is red       a leg that is not green and names no
#                                        failed suite: it hit its wall, a step
#                                        outside the suites failed, or its
#                                        report is missing or unreadable
#   Nightly chaos run is red             no leg ran; the result is the
#                                        conclusion of the changes job, else of
#                                        build-e2e-images, "missing" when the
#                                        listing lacks it, else "skipped"
#   Nightly chaos run is over its runner budget
#                                        the completed self-hosted jobs ran
#                                        longer than BUDGET_MINUTES together
#
# A leg is a job named "e2e-chaos (<leg>, ...)"; a skipped one did not run. A
# green night files nothing. Every night, green or red, writes a table of the
# legs and the self-hosted total to $GITHUB_STEP_SUMMARY and to stdout. A failure
# message goes into the issue inside a fence longer than any backtick run in it,
# so an @name in it never notifies. A failed listing exits 1 before any write; a
# failed ci-report-red-run.sh call does not stop the later ones.
#
# Required env vars:
#   RUN_ID          — the workflow run to report
#   RUN_URL         — the URL of that run
#   REPORTS_DIR     — the directory the JUnit artifacts were downloaded into,
#                     one e2e-chaos-junit-report-<leg>/ directory per leg
# Optional env vars:
#   BUDGET_MINUTES  — the self-hosted minutes a night may take (default 600)
#   REPORT_SHA      — the commit the run tested; ci-report-red-run.sh names it
#   GITHUB_STEP_SUMMARY — the step summary file; GitHub Actions sets it
# gh reads GH_TOKEN and GH_REPO itself; the token needs actions: read and
# issues: write.
#
# Usage:
#   RUN_ID=123 RUN_URL=https://github.com/<owner>/<repo>/actions/runs/123 \
#     REPORTS_DIR=_output/nightly hack/ci-report-nightly-chaos.sh
#
# Exit codes:
#   0 — every finding was filed, or there was none
#   1 — a required env var is empty, the listing failed, or a finding was not
#       filed (each named on an "ERROR: not filed: <title>" line)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

for name in RUN_ID RUN_URL REPORTS_DIR; do
  if [ -z "${!name:-}" ]; then
    echo "ERROR: ${name} is empty" >&2
    exit 1
  fi
done
BUDGET_MINUTES="${BUDGET_MINUTES:-600}"
case "${BUDGET_MINUTES}" in
  '' | *[!0-9]*)
    echo "ERROR: BUDGET_MINUTES is no whole number" >&2
    exit 1
    ;;
esac

listing_failed() {
  echo "ERROR: listing the jobs of run ${RUN_ID} failed; nothing was filed" >&2
  exit 1
}

# minutes_of <seconds> — whole minutes, rounded up.
minutes_of() {
  echo $((($1 + 59) / 60))
}

# ---------------------------------------------------------------------------
# 1. The jobs of the run
#
# gh prints one object per page. Every page must be an object with a jobs
# array; anything else is no listing.
# ---------------------------------------------------------------------------
listing="$(gh api --paginate "repos/{owner}/{repo}/actions/runs/${RUN_ID}/jobs?filter=latest&per_page=100")" ||
  listing_failed
# One record per job: name, conclusion, URL, self-hosted flag and the seconds
# it ran, -1 when it has not completed. "-" stands for an empty field, since
# read collapses adjacent tabs.
records="$(printf '%s' "${listing}" | jq -rs '
  if length > 0 and all(.[]; type == "object" and (.jobs | type) == "array")
  then .[].jobs[]
       | [ (.name // "-"),
           (.conclusion // .status // "unknown"),
           (.html_url // "-"),
           (if any((.labels // [])[]; . == "self-hosted") then "self-hosted" else "-" end),
           (if .status == "completed" and .started_at and .completed_at
            then (try ((.completed_at | fromdateiso8601) - (.started_at | fromdateiso8601)) catch -1)
            else -1 end) ]
       | @tsv
  else error("no listing") end' 2>/dev/null)" || listing_failed

LEG_RE='^e2e-chaos \(([a-z0-9-]+),'

legs=()
leg_results=()
leg_urls=()
leg_seconds=()
changes_result=missing
changes_url=-
build_result=missing
build_url=-
self_hosted_seconds=0
budget_rows=""

while IFS=$'\t' read -r name result url host seconds; do
  [ -n "${name}" ] || continue
  if [ "${host}" = "self-hosted" ] && [ "${seconds}" -ge 0 ] && [ "${result}" != "skipped" ]; then
    self_hosted_seconds=$((self_hosted_seconds + seconds))
    budget_rows="${budget_rows}| \`${name}\` | ${result} | $(minutes_of "${seconds}") |"$'\n'
  fi
  case "${name}" in
    changes)
      changes_result="${result}"
      changes_url="${url}"
      ;;
    build-e2e-images)
      build_result="${result}"
      build_url="${url}"
      ;;
  esac
  if [[ "${name}" =~ ${LEG_RE} ]] && [ "${result}" != "skipped" ]; then
    legs+=("${BASH_REMATCH[1]}")
    leg_results+=("${result}")
    leg_urls+=("${url}")
    leg_seconds+=("${seconds}")
  fi
done <<<"${records}"

# ---------------------------------------------------------------------------
# 2. Filing
# ---------------------------------------------------------------------------
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
not_filed=""

# file_issue <title> <result> — file the finding in ${work}/summary.md through
# ci-report-red-run.sh.
file_issue() {
  if ! REPORT_TITLE="$1" REPORT_RESULT="$2" SUMMARY_FILE="${work}/summary.md" \
    "${SCRIPT_DIR}/ci-report-red-run.sh" </dev/null; then
    not_filed="${not_filed}$1"$'\n'
  fi
}

# fenced <text> — print text inside a text fence longer than any backtick run
# in it, so nothing in it renders or notifies.
fenced() {
  local fence='```'
  while [[ "$1" == *"${fence}"* ]]; do fence="${fence}\`"; done
  printf '%s\n%s\n%s\n' "${fence}text" "$1" "${fence}"
}

leg_failed_suites=()
i=0
while [ "${i}" -lt "${#legs[@]}" ]; do
  leg="${legs[i]}"
  leg_failed_suites+=("")
  if [ "${leg_results[i]}" = "success" ]; then
    i=$((i + 1))
    continue
  fi

  reports=()
  for f in "${REPORTS_DIR}/e2e-chaos-junit-report-${leg}"/*.xml; do
    [ -f "${f}" ] && reports+=("${f}")
  done
  reason=""
  failed=""
  if [ "${#reports[@]}" -eq 0 ]; then
    reason="The leg uploaded no JUnit report: it hit its wall or ended before its suites ran."
  elif ! failed="$(python3 -I "${SCRIPT_DIR}/ci-junit-failed-suites.py" "${reports[@]}" 2>"${work}/reader.err" </dev/null)"; then
    failed=""
    reason="The JUnit report of the leg is unreadable:"$'\n\n'"$(fenced "$(cat "${work}/reader.err")")"$'\n'
  elif [ -z "${failed}" ]; then
    reason="The JUnit report of the leg names no failed suite: a step outside the suites failed."
  fi

  if [ -n "${reason}" ]; then
    {
      echo "Leg: \`${leg}\`"
      echo
      echo "Job: ${leg_urls[i]}"
      echo
      echo "${reason}"
      echo
      echo "Read the job log."
    } >"${work}/summary.md"
    file_issue "Nightly chaos leg ${leg} is red" "${leg_results[i]}"
  else
    names=""
    while IFS=$'\t' read -r suite message; do
      [ -n "${suite}" ] || continue
      names="${names:+${names}, }${suite}"
      {
        echo "Leg: \`${leg}\`"
        echo
        echo "Suite: \`${suite}\`"
        echo
        echo "Job: ${leg_urls[i]}"
        echo
        echo "The suite failed with:"
        echo
        fenced "${message}"
      } >"${work}/summary.md"
      file_issue "Nightly chaos suite ${suite} is red" failure
    done <<<"${failed}"
    leg_failed_suites[i]="${names}"
  fi
  i=$((i + 1))
done

if [ "${#legs[@]}" -eq 0 ]; then
  if [ "${changes_result}" != "success" ]; then
    run_job=changes
    run_result="${changes_result}"
    run_url="${changes_url}"
  elif [ "${build_result}" != "success" ]; then
    run_job=build-e2e-images
    run_result="${build_result}"
    run_url="${build_url}"
  else
    run_job=e2e-chaos
    run_result=skipped
    run_url=-
  fi
  {
    echo "No \`e2e-chaos\` leg ran: the \`${run_job}\` job ended \`${run_result}\`."
    if [ "${run_url}" != "-" ]; then
      echo
      echo "Job: ${run_url}"
    fi
  } >"${work}/summary.md"
  file_issue "Nightly chaos run is red" "${run_result}"
fi

total_minutes="$(minutes_of "${self_hosted_seconds}")"
if [ "${self_hosted_seconds}" -gt $((BUDGET_MINUTES * 60)) ]; then
  {
    echo "The self-hosted jobs of the run took ${total_minutes} minutes, over the budget of ${BUDGET_MINUTES}."
    echo
    echo "| Job | Conclusion | Minutes |"
    echo "| --- | --- | --- |"
    printf '%s' "${budget_rows}"
    echo "| Total | | ${total_minutes} |"
  } >"${work}/summary.md"
  file_issue "Nightly chaos run is over its runner budget" failure
fi

# ---------------------------------------------------------------------------
# 3. The step summary
# ---------------------------------------------------------------------------
{
  echo "### Nightly chaos run"
  echo
  if [ "${#legs[@]}" -eq 0 ]; then
    echo "No \`e2e-chaos\` leg ran."
  else
    echo "| Leg | Conclusion | Minutes | Failed suites |"
    echo "| --- | --- | --- | --- |"
    i=0
    while [ "${i}" -lt "${#legs[@]}" ]; do
      minutes="—"
      [ "${leg_seconds[i]}" -ge 0 ] && minutes="$(minutes_of "${leg_seconds[i]}")"
      echo "| ${legs[i]} | ${leg_results[i]} | ${minutes} | ${leg_failed_suites[i]:-—} |"
      i=$((i + 1))
    done
  fi
  echo
  echo "Self-hosted runner time: ${total_minutes} of ${BUDGET_MINUTES} minutes."
} | tee -a "${GITHUB_STEP_SUMMARY:-/dev/null}"

if [ -n "${not_filed}" ]; then
  while IFS= read -r title; do
    if [ -n "${title}" ]; then
      echo "ERROR: not filed: ${title}" >&2
    fi
  done <<<"${not_filed}"
  exit 1
fi
exit 0
