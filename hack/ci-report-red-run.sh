#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-report-red-run.sh — File a red workflow run on one tracking issue.
#
# The report job of .github/workflows/flux-path.yaml runs it after a red
# scheduled night. It comments on the open issue whose title is REPORT_TITLE
# and whose author is github-actions[bot], the author of every issue the
# workflow token opens, or creates that issue when none is open. The repository
# is public, so anyone can open an issue of that title; such an issue is never
# picked. A human closes the issue once the cause is fixed; the script never
# closes one. When several open issues match, it comments on the lowest number.
#
# It reads every page of the REST listing of open issues
# (`gh api --paginate`), which has no search-index lag and no cap, and matches
# the title exactly. Pull requests in that listing never match. A failed or
# unreadable listing exits 1 before any write, so a transient error never
# files a duplicate.
#
# The body names the workflow and the result, links the run, names the commit
# when REPORT_SHA is set, and then carries the content of SUMMARY_FILE, cut at
# 60000 bytes (SUMMARY_LIMIT): GitHub rejects a body over 65536 characters, and
# a character is at least one byte. The cut keeps whole lines and closes a
# fence it leaves open, so every code span and block of the kept part stays
# closed. A missing or empty SUMMARY_FILE gives a line pointing at the run log
# instead.
#
# Required env vars:
#   REPORT_TITLE   — the title of the tracking issue
#   REPORT_RESULT  — the result of the red job (failure, cancelled, ...)
#   RUN_URL        — the URL of the run
# Optional env vars:
#   REPORT_SHA     — the commit the run tested
#   SUMMARY_FILE   — a Markdown file to append to the body
#   GITHUB_WORKFLOW — the workflow name; GitHub Actions sets it
# gh reads GH_TOKEN and GH_REPO itself; the token needs issues: write.
#
# Usage:
#   REPORT_TITLE="Scheduled Flux path run is red" REPORT_RESULT=failure \
#     RUN_URL=https://github.com/<owner>/<repo>/actions/runs/<id> \
#     hack/ci-report-red-run.sh
#
# Exit codes:
#   0 — commented on the tracking issue, or created it
#   1 — a required env var is empty, or a gh call failed

set -euo pipefail

for name in REPORT_TITLE REPORT_RESULT RUN_URL; do
  if [ -z "${!name:-}" ]; then
    echo "ERROR: ${name} is empty" >&2
    exit 1
  fi
done

BOT_LOGIN="github-actions[bot]"
SUMMARY_LIMIT=60000

# gh joins the pages into one JSON list.
if ! issues="$(gh api --paginate "repos/{owner}/{repo}/issues?state=open&per_page=100")"; then
  echo "ERROR: listing the open issues failed; nothing was filed" >&2
  exit 1
fi
# `input` fails on an empty listing, so only a JSON list reaches the match.
# shellcheck disable=SC2016 # $t and $a are jq variables
if ! number="$(printf '%s' "${issues}" | jq -rn --arg t "${REPORT_TITLE}" --arg a "${BOT_LOGIN}" '
  input | if type == "array" then
            [.[] | select(.title == $t and .user.login == $a and (has("pull_request") | not))
                 | .number] | min // empty
          else error("not a list") end')"; then
  echo "ERROR: the listing of the open issues is no list of issues; nothing was filed" >&2
  exit 1
fi

body="$(mktemp)"
trap 'rm -f "${body}"' EXIT
{
  echo "The \`${GITHUB_WORKFLOW:-unknown}\` run ended with result \`${REPORT_RESULT}\`."
  echo
  echo "Run: ${RUN_URL}"
  if [ -n "${REPORT_SHA:-}" ]; then
    echo
    echo "Commit: ${REPORT_SHA}"
  fi
  echo
  if [ -n "${SUMMARY_FILE:-}" ] && [ -s "${SUMMARY_FILE}" ]; then
    if [ "$(wc -c <"${SUMMARY_FILE}")" -gt "${SUMMARY_LIMIT}" ]; then
      # Whole lines only: a row cut inside its message would lose the backtick
      # that closes its code span, and an @name in it would notify. A fence the
      # cut left open is closed, so the note below is no code.
      head -c "${SUMMARY_LIMIT}" "${SUMMARY_FILE}" | sed '$d' |
        awk '{ print } /^```/ { n++ } END { if (n % 2) print "```" }'
      echo
      echo "(The summary is cut at ${SUMMARY_LIMIT} bytes here. The run has all of it.)"
    else
      cat "${SUMMARY_FILE}"
    fi
  else
    echo "The job wrote no summary: it ended before the bring-up step or hit its wall. Read the run log."
  fi
} >"${body}"

if [ -n "${number}" ]; then
  if ! gh issue comment "${number}" --body-file "${body}"; then
    echo "ERROR: gh issue comment ${number} failed" >&2
    exit 1
  fi
  echo "OK: commented on #${number}"
else
  if ! gh issue create --title "${REPORT_TITLE}" --body-file "${body}"; then
    echo "ERROR: gh issue create failed" >&2
    exit 1
  fi
  echo "OK: created the tracking issue"
fi
