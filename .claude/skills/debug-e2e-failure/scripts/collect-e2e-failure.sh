#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# collect-e2e-failure.sh — pull the failure evidence for a chainsaw e2e CI run.
#
# Read-only against the repository: talks to GitHub via `gh` and writes only
# under _output/e2e-failure/ (or --out).
#
# Usage:
#   collect-e2e-failure.sh --run <run-id> [--repo <owner/repo>] [--out <dir>]
#   collect-e2e-failure.sh --pr <number>  [--repo <owner/repo>] [--out <dir>]
#
# With --pr the newest failed workflow run among the PR's checks is resolved
# via `gh pr checks`. The script then:
#   1. lists the run's failed jobs,
#   2. downloads the failed-step logs (gh run view --log-failed),
#   3. extracts the chainsaw failure blocks (--- FAIL / | ERROR |) into a
#      condensed excerpt,
#   4. maps failed chainsaw test names back to suite directories under tests/,
#   5. lists the run's artifacts (JUnit reports, tempest results) with the
#      download command.
#
# Exit codes: 0 evidence collected, 1 usage error / gh missing / no failed
# run found, 2 run still in progress (logs not yet available).

set -euo pipefail

die()  { echo "ERROR: $*" >&2; exit 1; }
info() { echo "[INFO] $*"; }
hdr()  { echo; echo "=== $* ==="; }

usage() {
  cat <<'EOF'
Usage:
  collect-e2e-failure.sh --run <run-id> [--repo <owner/repo>] [--out <dir>]
  collect-e2e-failure.sh --pr <number>  [--repo <owner/repo>] [--out <dir>]

Resolves the failed jobs of a CI workflow run (directly by id, or the newest
failed run among a PR's checks), downloads the failed-step logs, extracts the
chainsaw failure blocks, and lists the JUnit/diagnostic artifacts.
EOF
}

RUN_ID=""
PR_NUM=""
REPO=""
OUT_DIR=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --run)  RUN_ID="${2:?--run needs a value}"; shift 2 ;;
    --pr)   PR_NUM="${2:?--pr needs a value}"; shift 2 ;;
    --repo) REPO="${2:?--repo needs a value}"; shift 2 ;;
    --out)  OUT_DIR="${2:?--out needs a value}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "unknown argument: $1" ;;
  esac
done

[[ -n "${RUN_ID}" || -n "${PR_NUM}" ]] || { usage >&2; exit 1; }
[[ -n "${RUN_ID}" && -n "${PR_NUM}" ]] && die "--run and --pr are mutually exclusive"

command -v gh >/dev/null 2>&1 \
  || die "gh (GitHub CLI) is required — https://cli.github.com"
gh auth status >/dev/null 2>&1 \
  || die "gh is not authenticated — run: gh auth login"

# Run from the repo root so the suite-mapping step can grep tests/.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "${REPO_ROOT}"

if [[ -z "${REPO}" ]]; then
  REPO="$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null || true)"
  [[ -n "${REPO}" ]] || die "cannot resolve the repository from the current directory — pass --repo <owner/repo>"
fi

# ---------------------------------------------------------------------------
# Resolve --pr to the newest failed run id
# ---------------------------------------------------------------------------
if [[ -n "${PR_NUM}" ]]; then
  hdr "Resolving failed runs for PR #${PR_NUM} (${REPO})"
  # Each failing check links to .../actions/runs/<run-id>/job/<job-id>.
  failed_links="$(gh pr checks "${PR_NUM}" --repo "${REPO}" --json state,link \
    --jq '.[] | select(.state == "FAILURE") | .link' 2>/dev/null || true)"
  if [[ -z "${failed_links}" ]]; then
    pending="$(gh pr checks "${PR_NUM}" --repo "${REPO}" --json state \
      --jq '[.[] | select(.state == "IN_PROGRESS" or .state == "PENDING" or .state == "QUEUED")] | length' \
      2>/dev/null || echo 0)"
    if [[ "${pending}" != "0" ]]; then
      info "no failed checks yet, ${pending} check(s) still running — re-run once they finish"
      exit 2
    fi
    die "PR #${PR_NUM} has no failed checks — nothing to collect"
  fi
  RUN_ID="$(printf '%s\n' "${failed_links}" \
    | sed -nE 's#.*/actions/runs/([0-9]+)/job/[0-9]+.*#\1#p' \
    | sort -rn | head -n 1)"
  [[ -n "${RUN_ID}" ]] || die "could not extract a run id from the failed check links"
  info "newest failed run: ${RUN_ID}"
fi

OUT_DIR="${OUT_DIR:-_output/e2e-failure/run-${RUN_ID}}"

# ---------------------------------------------------------------------------
# Run status and failed jobs
# ---------------------------------------------------------------------------
hdr "Run ${RUN_ID} (${REPO})"
STATUS="$(gh run view "${RUN_ID}" --repo "${REPO}" --json status --jq .status)" \
  || die "run ${RUN_ID} not found in ${REPO}"
TITLE="$(gh run view "${RUN_ID}" --repo "${REPO}" --json displayTitle --jq .displayTitle)"
URL="$(gh run view "${RUN_ID}" --repo "${REPO}" --json url --jq .url)"
info "title:  ${TITLE}"
info "status: ${STATUS}"
info "url:    ${URL}"

hdr "Failed jobs"
FAILED_JOBS="$(gh run view "${RUN_ID}" --repo "${REPO}" --json jobs \
  --jq '.jobs[] | select(.conclusion == "failure") | "\(.name)\t\(.url)"' || true)"
if [[ -n "${FAILED_JOBS}" ]]; then
  printf '%s\n' "${FAILED_JOBS}"
else
  info "no job with conclusion=failure (yet)"
fi

if [[ "${STATUS}" != "completed" ]]; then
  info "run is still in progress — job logs become available once it completes"
  info "re-run this script when the run has finished: --run ${RUN_ID}"
  exit 2
fi

# ---------------------------------------------------------------------------
# Failed-step logs
# ---------------------------------------------------------------------------
mkdir -p "${OUT_DIR}"
LOG_FILE="${OUT_DIR}/failed-jobs.log"
hdr "Downloading failed-step logs"
gh run view "${RUN_ID}" --repo "${REPO}" --log-failed > "${LOG_FILE}" \
  || die "could not fetch the failed-step logs for run ${RUN_ID}"
info "wrote ${LOG_FILE} ($(wc -l < "${LOG_FILE}" | tr -d ' ') lines)"

# ---------------------------------------------------------------------------
# Chainsaw failure excerpt
# ---------------------------------------------------------------------------
# chainsaw runs on Go's testing framework: the authoritative failure marker is
# "--- FAIL: chainsaw/<test-name>". Step-level detail sits in the pipe-table
# lines whose status column reads ERROR, and in the expected-vs-actual diffs
# that follow. failFast:true means later failures may be cascade — always read
# the FIRST failing test.
EXCERPT="${OUT_DIR}/chainsaw-excerpt.log"
hdr "Extracting chainsaw failure blocks"
{
  echo "### go-test failure markers"
  grep -E -- '--- FAIL: chainsaw' "${LOG_FILE}" || true
  echo
  echo "### step-level ERROR windows"
  grep -E -B 2 -A 12 -- '\| ERROR' "${LOG_FILE}" || true
  echo
  echo "### generic error markers"
  grep -E -B 1 -A 4 'context deadline exceeded|Error from server|::error' "${LOG_FILE}" || true
  echo
  echo "### chainsaw summary"
  grep -E -A 6 'Tests Summary' "${LOG_FILE}" || true
} > "${EXCERPT}"
info "wrote ${EXCERPT} ($(wc -l < "${EXCERPT}" | tr -d ' ') lines)"
echo
info "first failure markers:"
grep -E -- '--- FAIL: chainsaw' "${LOG_FILE}" | head -n 10 || info "  (none — not a chainsaw failure; read ${EXCERPT})"

# ---------------------------------------------------------------------------
# Map failed chainsaw test names to suite directories
# ---------------------------------------------------------------------------
hdr "Suite mapping"
if [[ -d tests/e2e ]]; then
  grep -hoE -- '--- FAIL: chainsaw/[A-Za-z0-9_-]+' "${LOG_FILE}" 2>/dev/null \
    | sed 's#.*chainsaw/##' | sort -u | while IFS= read -r test_name; do
      info "test '${test_name}' is defined in:"
      grep -rl --include=chainsaw-test.yaml "name: ${test_name}\$" tests/ 2>/dev/null \
        | sed 's/^/       /' || echo "       (no chainsaw-test.yaml with metadata.name ${test_name} — renamed or non-chainsaw failure?)"
    done
else
  info "tests/e2e not found relative to ${REPO_ROOT} — skipping suite mapping"
fi

# ---------------------------------------------------------------------------
# Artifacts (JUnit reports, tempest results)
# ---------------------------------------------------------------------------
hdr "Artifacts of run ${RUN_ID}"
gh api "repos/${REPO}/actions/runs/${RUN_ID}/artifacts" \
  --jq '.artifacts[] | "\(.name)\t\(.size_in_bytes) bytes\texpired=\(.expired)"' || true
echo
info "download one with:"
info "  gh run download ${RUN_ID} --repo ${REPO} -n <name> -D ${OUT_DIR}/artifacts"

hdr "Done"
info "evidence under ${OUT_DIR}/"
