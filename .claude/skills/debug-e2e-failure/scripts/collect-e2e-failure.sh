#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# collect-e2e-failure.sh — pull the failure evidence for a chainsaw e2e CI run.
#
# Read-only against the repository and GitHub: talks to GitHub via `gh` (GET
# requests only) and writes only under _output/e2e-failure/ (or --out).
#
# Usage:
#   collect-e2e-failure.sh --run <run-id> [--attempt <n>] [--job <ere>] [--repo <owner/repo>] [--out <dir>]
#   collect-e2e-failure.sh --pr <number>  [--job <ere>] [--repo <owner/repo>] [--out <dir>]
#   collect-e2e-failure.sh --log-file <path> [--out <dir>]      # offline: no gh
#   collect-e2e-failure.sh --list-signatures
#
# With --pr the newest failed workflow run among the PR's checks is resolved
# via `gh pr checks`. The script then:
#   1. lists the run's failed and cancelled jobs (runner, failed step) and,
#      per failed job, the skipped jobs downstream of it (from ci.yaml needs),
#   2. downloads each failed job's full log (and a job-wall-cancelled job's)
#      by job id, ANSI-stripped, plus a copy without the workflow's echoed
#      script source,
#   3. fetches the check-run annotations of those jobs (runner death, job wall),
#   4. extracts the chainsaw failure blocks (--- FAIL / | ERROR |) into a
#      condensed excerpt,
#   5. runs the signature scan: every row of SIGNATURES below against the log
#      and the annotations, plus the job-metadata checks (zero recorded steps),
#   6. maps failed chainsaw test names back to suite directories under tests/,
#   7. lists which images the jobs reused from main vs built in this run,
#   8. lists the run's artifacts (JUnit reports, tempest results).
#
# --log-file runs steps 4-7 against a saved log (`gh run view --log` output,
# a job log from the API, or any plain log) without talking to GitHub.
#
# Exit codes: 0 evidence collected, 1 usage error / gh missing / no failed
# run found, 2 run still in progress (logs not yet available).

set -euo pipefail

# ---------------------------------------------------------------------------
# Known failure signatures — keep in sync with SKILL.md § Known failure
# patterns (the first column is the pattern name used there).
#
# Format: <pattern-name>|<ERE>|<one-line meaning>
# The ERE is matched with `grep -E` against the ANSI-stripped log minus the
# workflow's echoed script source, and against the job annotations. It must
# not contain a literal `|`: give an alternative its own row under the same
# name. Write every ERE so it cannot match a script's source text (chainsaw
# prints the script body of each step): anchor on a literal a variable would
# not produce.
# ---------------------------------------------------------------------------
SIGNATURES="
runner-lost|The self-hosted runner lost communication with the server|infra: the runner died mid-job; nothing to fix in the tree, rerun the failed jobs
runner-lost|The runner has received a shutdown signal|infra: sibling job on a runner that was going down; rerun the failed jobs
anon-clone-401|could not read Username for 'https://github.com'|infra: GitHub refused an anonymous clone from the runner's old git; the cloning step lacks GITHUB_TOKEN
anon-clone-401|expected flush after ref listing|infra: anonymous upload-pack got a 401 challenge; the cloning step lacks GITHUB_TOKEN
korc-tag-expired|quay.io/orc/openstack-resource-controller.*not found|infra: the pinned K-ORC commit-<sha> tag expired on quay; re-pin commit, newTag and digest together
ghcr-transient|ghcr\.io.*\(Client\.Timeout exceeded while awaiting headers\)|infra: transient ghcr.io timeout past the bounded retries
ghcr-transient|docker login to [a-z0-9.-]+ failed after [0-9]+ attempts|infra: ghcr.io stayed unreachable through every login retry
ghcr-transient|docker push [a-z0-9./:@-]+ failed after [0-9]+ attempts|infra: ghcr.io stayed unreachable through every push retry
go-merge-with-main|FAIL: operators/[a-z0-9]+ is not tidy|infra: a module is not tidy; if the branch leaves it untouched, the merge with main is (PR CI tests the merge), tidy in a merged tree
go-merge-with-main|reading https://sum\.golang\.org/|infra: the go command fetched checksums from the sumdb, which is flaky; usually a module out of lockstep with main
go-merge-with-main|diff --git a/go\.work\.sum|infra: go.work.sum churn in verify-codegen: a workspace module lags main's versions
job-wall|has exceeded the maximum execution time|the job wall (timeout-minutes) cancelled the job before the suite's own timeout: no catch block, no JUnit; the last suite or step started is the one that stalled
korc-suspend-race|conflict with \"kustomize-controller\"|Flux reconciled k-orc before deploy-infra suspended it; fixed by the suspended Kustomization in deploy/kind/base
rabbitmq-recreate-race|rabbitmqcluster/[a-z0-9-]+ was not removed within|upstream cluster-operator re-created an unowned broker; check the FinalizingMessaging guard ran before blaming the suite
oom-kill|OOMKilled|a container was OOM-killed: openstack-db-0 = mariadb-oom, cinder-backup = issue #1003; read the node-pressure blocks of the dump
mariadb-oom|Recovering after a crash|mysqld restarted into crash recovery: an OOM kill of the kind MariaDB, the DatabaseReady=False victim suite is incidental
db-sync-1054|1054, \"Unknown column|a db-sync lost its connection mid-DDL and every retry hits the half-applied schema: read the earliest db-sync pod, not the latest
jmespath-nil|invalid type for: <nil>, expected|chainsaw aborted an assert at its first poll: a JMESPath function got an absent field; guard with || \`[]\`
evidence-after-finally|vhost ['a-z0-9_/-]+ not found|noise: pod tails captured after the suite's finally removed the vhost; the cause is in the catch block, not the later dump
shared-memcached-401|keystoneauth1\.exceptions\.http\.Unauthorized|a service got 401 from Keystone; with two Keystone CRs on one Memcached this is the identity-cache collision
aborted-connection|Aborted connection .*Got an error reading communication packets|noise: a short-lived MariaDB client (nova-manage, a healthcheck) exited without COM_QUIT; progress, not a hang
catalog-import-race|all catalog imports resolved .*expected '4', got '3'|K-ORC resolves the admin Endpoint after the Service flips Available; the suite sampled too early
korc-log-selector|No resources found in orc-system namespace|noise: the catch block's K-ORC log selector matches nothing, so K-ORC's side of the failure is not in this log
insufficient-cpu|nodes are available: .*Insufficient cpu|pods Pending on the node's CPU request budget; read the Node capacity block before adding requests
tempest-port-forward|port-forward on [0-9]+ died during the run|tempest failures against that localhost port are the dead forward, not the service
webhook-stale-keepalive|Error from server \(InternalError\).*failed calling webhook|apiserver could not reach a webhook; right after an operator rollout it is a stale keep-alive to the old pod IP
namespace-hijack|Error from server \(NotFound\): pods \"openbao-0\" not found|a helper used chainsaw's injected NAMESPACE instead of its own contract variable
"

die()  { echo "ERROR: $*" >&2; exit 1; }
info() { echo "[INFO] $*"; }
hdr()  { echo; echo "=== $* ==="; }

usage() {
  cat <<'EOF'
Usage:
  collect-e2e-failure.sh --run <run-id> [--attempt <n>] [--job <ere>] [--repo <owner/repo>] [--out <dir>]
  collect-e2e-failure.sh --pr <number>  [--job <ere>] [--repo <owner/repo>] [--out <dir>]
  collect-e2e-failure.sh --log-file <path> [--out <dir>]
  collect-e2e-failure.sh --list-signatures

Resolves the failed jobs of a CI workflow run (directly by id, or the newest
failed run among a PR's checks), downloads their logs and annotations, extracts
the chainsaw failure blocks, scans for the known failure signatures, and lists
the JUnit/diagnostic artifacts.

  --attempt <n>      read attempt n of the run instead of the latest
  --job <ere>        only the failed/cancelled jobs whose name matches the ERE
  --log-file <path>  offline: excerpt, signature scan and suite mapping on a
                     saved log; no gh needed
  --list-signatures  print the signature table and exit
EOF
}

RUN_ID=""
PR_NUM=""
REPO=""
OUT_DIR=""
ATTEMPT=""
JOB_RE=""
LOG_SRC=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --run)      RUN_ID="${2:?--run needs a value}"; shift 2 ;;
    --pr)       PR_NUM="${2:?--pr needs a value}"; shift 2 ;;
    --repo)     REPO="${2:?--repo needs a value}"; shift 2 ;;
    --out)      OUT_DIR="${2:?--out needs a value}"; shift 2 ;;
    --attempt)  ATTEMPT="${2:?--attempt needs a value}"; shift 2 ;;
    --job)      JOB_RE="${2:?--job needs a value}"; shift 2 ;;
    --log-file) LOG_SRC="${2:?--log-file needs a value}"; shift 2 ;;
    --list-signatures)
      printf '%s\n' "${SIGNATURES}" | grep -v '^$'
      exit 0 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "unknown argument: $1" ;;
  esac
done

if [[ -n "${LOG_SRC}" ]]; then
  [[ -z "${RUN_ID}${PR_NUM}" ]] || die "--log-file cannot be combined with --run or --pr"
  [[ -f "${LOG_SRC}" ]] || die "no such log file: ${LOG_SRC}"
  LOG_SRC="$(cd "$(dirname "${LOG_SRC}")" && pwd)/$(basename "${LOG_SRC}")"
else
  [[ -n "${RUN_ID}" || -n "${PR_NUM}" ]] || { usage >&2; exit 1; }
  [[ -n "${RUN_ID}" && -n "${PR_NUM}" ]] && die "--run and --pr are mutually exclusive"
  [[ -n "${ATTEMPT}" && -z "${RUN_ID}" ]] && die "--attempt needs --run"
fi
if [[ -n "${ATTEMPT}" ]]; then
  case "${ATTEMPT}" in *[!0-9]*) die "--attempt takes a number" ;; esac
fi

# Run from the repo root so the suite mapping can grep tests/ and the rerun
# check can read ci.yaml.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "${REPO_ROOT}"
CI_YAML=".github/workflows/ci.yaml"

ESC="$(printf '\033')"
# strip_ansi <in> <out>: drop colour escapes, keep everything else.
strip_ansi() {
  sed "s/${ESC}\[[0-9;]*[A-Za-z]//g" "$1" > "$2"
}
# strip_source <in> <out>: drop the workflow's echo of each step's `run:`
# script (the runner prints it in cyan right after the timestamp), then the
# colour escapes. chainsaw's own step lines carry the same colour mid-line and
# survive.
strip_source() {
  grep -v "[0-9]Z ${ESC}\[36;1m" "$1" | sed "s/${ESC}\[[0-9;]*[A-Za-z]//g" > "$2" || true
}

# ---------------------------------------------------------------------------
# Excerpt, signature scan, suite mapping, image provenance — shared by the
# online and the offline path.
# ---------------------------------------------------------------------------
# chainsaw runs on Go's testing framework: the authoritative failure marker is
# "--- FAIL: chainsaw/<test-name>". The step table row whose status column
# reads ERROR follows the SCRIPT LOG block that carries the step's stdout, so
# the window reaches further back than forward. failFast:true means later
# failures may be cascade — always read the FIRST failing test.
write_excerpt() { # <log> <nosrc-log> <excerpt>
  {
    echo "### go-test failure markers"
    grep -E -- '--- FAIL: chainsaw' "$1" || true
    echo
    echo "### step-level ERROR windows"
    grep -E -B 12 -A 4 -- '\|[[:space:]]*ERROR[[:space:]]*\|' "$1" || true
    echo
    echo "### workflow error annotations (with what preceded them)"
    grep -E -B 8 -- '##\[error\]' "$2" || true
    echo
    echo "### generic error markers"
    grep -E -B 1 -A 4 'context deadline exceeded|Error from server|Timed out waiting' "$2" || true
    echo
    echo "### chainsaw summary"
    grep -E -A 6 'Tests Summary' "$1" || true
  } > "$3"
  info "wrote $3 ($(wc -l < "$3" | tr -d ' ') lines)"
  echo
  info "first failure markers:"
  if grep -qE -- '--- FAIL: chainsaw/' "$1"; then
    grep -E -- '--- FAIL: chainsaw/' "$1" | sed -n 1,10p | tr '\t' ' ' | cut -c1-200 || true
  else
    info "  (none — not a chainsaw failure; read $3)"
  fi
}

MATCHED=""   # space-separated pattern names already reported
report_match() { # <name> <meaning> <evidence>
  echo "[MATCH] $1 — $2 — see SKILL.md § Known failure patterns"
  echo "        evidence: $3"
  MATCHED="${MATCHED} $1"
}

scan_signatures() { # <nosrc-log> [<annotations>]
  local files=("$1") name ere meaning hits line job text snip tab
  tab="$(printf '\t')"
  if [[ $# -gt 1 && -s "$2" ]]; then files+=("$2"); fi
  while IFS='|' read -r name ere meaning; do
    [[ -n "${name}" ]] || continue
    hits="$(cat "${files[@]}" | grep -cE -- "${ere}" || true)"
    [[ "${hits}" != "0" ]] || continue
    # sed reads to the end, so no stage of the pipe dies of SIGPIPE.
    line="$(cat "${files[@]}" | grep -E -- "${ere}" | sed -n 1p || true)"
    # gh log lines are <job>TAB<step>TAB<timestamp> <text>, annotation lines
    # <job>TAB<level>TAB<message>; a plain log has neither.
    job=""
    text="${line}"
    case "${line}" in
      *"${tab}"*) job="[${line%%"${tab}"*}] "; text="${line##*"${tab}"}" ;;
    esac
    text="$(printf '%s\n' "${text}" | sed -E 's/^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z //')"
    # Cut around the match, which may sit far into a long line.
    snip="$(printf '%s\n' "${text}" | grep -oE -- ".{0,70}(${ere}).{0,70}" | sed -n 1p || true)"
    [[ -n "${snip}" ]] || snip="$(printf '%s\n' "${text}" | cut -c1-160)"
    report_match "${name}" "${meaning}" "(${hits} line(s)) ${job}${snip}"
  done <<EOF
${SIGNATURES}
EOF
}

# verdict: which half of SKILL.md's table to start in.
verdict() {
  local infra="" suite="" name ere meaning
  while IFS='|' read -r name ere meaning; do
    [[ -n "${name}" ]] || continue
    case " ${MATCHED} " in *" ${name} "*) ;; *) continue ;; esac
    case "${meaning}" in
      infra:*) case " ${infra} " in *" ${name} "*) ;; *) infra="${infra} ${name}" ;; esac ;;
      noise:*) ;;
      *) case " ${suite} " in *" ${name} "*) ;; *) suite="${suite} ${name}" ;; esac ;;
    esac
  done <<EOF
${SIGNATURES}
EOF
  if [[ -n "${infra}" ]]; then
    info "infrastructure signature(s) matched:${infra} — start in the infrastructure half of the table"
  elif [[ -n "${suite}" ]]; then
    info "suite/product signature(s) matched:${suite} — check each against the first FAIL; 'noise:' lines are not causes"
  else
    info "no known cause matched (noise aside) — treat it as new: read the first FAIL and the diagnostics dump"
  fi
}

map_suites() { # <log>
  hdr "Suite mapping"
  if [[ ! -d tests ]]; then
    info "tests/ not found relative to ${REPO_ROOT} — skipping suite mapping"
    return 0
  fi
  local names test_name defs
  names="$(grep -oE -- '--- FAIL: chainsaw/[^ ]+' "$1" 2>/dev/null | sed 's#.*chainsaw/##' | sort -u || true)"
  if [[ -z "${names}" ]]; then
    info "no failed chainsaw test in the log"
    return 0
  fi
  while IFS= read -r test_name; do
    defs="$(grep -rlE --include=chainsaw-test.yaml "^  name: ${test_name}\$" tests/ 2>/dev/null || true)"
    if [[ -n "${defs}" ]]; then
      info "test '${test_name}' is defined in:"
      printf '%s\n' "${defs}" | sed 's/^/       /'
    else
      info "test '${test_name}': no chainsaw-test.yaml with that metadata.name (renamed, or a different checkout?)"
    fi
  done <<EOF
${names}
EOF
}

image_provenance() { # <log>
  hdr "Image provenance (load-e2e-images)"
  local pulls
  pulls="$(grep -E '##\[group\]Pull ' "$1" 2>/dev/null | sed -E 's/.*##\[group\]Pull //' | sort -u || true)"
  if [[ -z "${pulls}" ]]; then
    info "no 'Pull <src> → <ref>' lines in the log (no load-e2e-images step, or the job died before it)"
    return 0
  fi
  info "reused from main by digest ($(printf '%s\n' "${pulls}" | grep -c '@sha256:' || true)):"
  printf '%s\n' "${pulls}" | grep '@sha256:' | sed 's/^/       /' || true
  info "built in this run ($(printf '%s\n' "${pulls}" | grep -vc '@sha256:' || true)) — tag e2e-<run>-<tag>"
}

# ---------------------------------------------------------------------------
# Offline path
# ---------------------------------------------------------------------------
if [[ -n "${LOG_SRC}" ]]; then
  OUT_DIR="${OUT_DIR:-_output/e2e-failure/offline-$(basename "${LOG_SRC}" .log)}"
  mkdir -p "${OUT_DIR}"
  LOG_FILE="${OUT_DIR}/failed-jobs.log"
  NOSRC_FILE="${OUT_DIR}/failed-jobs.nosrc.log"
  hdr "Offline: ${LOG_SRC}"
  strip_ansi "${LOG_SRC}" "${LOG_FILE}"
  strip_source "${LOG_SRC}" "${NOSRC_FILE}"
  info "wrote ${LOG_FILE} ($(wc -l < "${LOG_FILE}" | tr -d ' ') lines)"
  hdr "Extracting chainsaw failure blocks"
  write_excerpt "${LOG_FILE}" "${NOSRC_FILE}" "${OUT_DIR}/chainsaw-excerpt.log"
  hdr "Signature scan"
  scan_signatures "${NOSRC_FILE}"
  verdict
  map_suites "${LOG_FILE}"
  image_provenance "${LOG_FILE}"
  hdr "Done"
  info "evidence under ${OUT_DIR}/"
  exit 0
fi

# ---------------------------------------------------------------------------
# Online path
# ---------------------------------------------------------------------------
command -v gh >/dev/null 2>&1 \
  || die "gh (GitHub CLI) is required — https://cli.github.com (or pass --log-file)"
gh auth status >/dev/null 2>&1 \
  || die "gh is not authenticated — run: gh auth login (or pass --log-file)"

if [[ -z "${REPO}" ]]; then
  REPO="$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null || true)"
  [[ -n "${REPO}" ]] || die "cannot resolve the repository from the current directory — pass --repo <owner/repo>"
fi

# Resolve --pr to the newest failed run id.
if [[ -n "${PR_NUM}" ]]; then
  hdr "Resolving failed runs for PR #${PR_NUM} (${REPO})"
  # Each failing check links to .../actions/runs/<run-id>/job/<job-id>. A job
  # the job wall cancelled reads CANCELLED here, not FAILURE.
  failed_links="$(gh pr checks "${PR_NUM}" --repo "${REPO}" --json state,link \
    --jq '.[] | select(.state == "FAILURE" or .state == "CANCELLED" or .state == "TIMED_OUT" or .state == "STARTUP_FAILURE") | .link' 2>/dev/null || true)"
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

attempt_args=()
suffix=""
if [[ -n "${ATTEMPT}" ]]; then
  attempt_args=(--attempt "${ATTEMPT}")
  suffix="-attempt-${ATTEMPT}"
fi
OUT_DIR="${OUT_DIR:-_output/e2e-failure/run-${RUN_ID}${suffix}}"

# ---------------------------------------------------------------------------
# Run status and jobs
# ---------------------------------------------------------------------------
hdr "Run ${RUN_ID} (${REPO})"
RUN_META="$(gh run view "${RUN_ID}" --repo "${REPO}" ${attempt_args[@]+"${attempt_args[@]}"} \
  --json status,conclusion,attempt,displayTitle,url,event,headBranch \
  --jq '[.status, (.conclusion // "-"), (.attempt|tostring), .event, .headBranch, .url, .displayTitle] | join("\u001f")')" \
  || die "run ${RUN_ID} not found in ${REPO}"
IFS="$(printf '\037')" read -r STATUS CONCLUSION RUN_ATTEMPT EVENT BRANCH URL TITLE <<EOF
${RUN_META}
EOF
info "title:   ${TITLE}"
info "status:  ${STATUS} / ${CONCLUSION} (attempt ${RUN_ATTEMPT}, ${EVENT} on ${BRANCH})"
info "url:     ${URL}"
mkdir -p "${OUT_DIR}"

# One line per job: id, name, conclusion, step count, failed steps, first step.
# Empty fields become "-" so a tab-split read keeps its columns.
JOBS_TSV="${OUT_DIR}/jobs.tsv"
gh run view "${RUN_ID}" --repo "${REPO}" ${attempt_args[@]+"${attempt_args[@]}"} --json jobs --jq '
  .jobs[] | [
    (.databaseId|tostring),
    .name,
    (if (.conclusion // "") == "" then "-" else .conclusion end),
    ((.steps|length)|tostring),
    ([.steps[] | select(.conclusion == "failure") | .name] | join(", ") | if . == "" then "-" else . end),
    (if (.steps|length) > 0 then "\(.steps[0].name)=\(.steps[0].conclusion // "-")" else "-" end)
  ] | @tsv' > "${JOBS_TSV}" || die "could not list the jobs of run ${RUN_ID}"

# Jobs of interest: failed ones, and cancelled ones that recorded steps (a
# job wall, or a sibling on a dying runner; a run-level cancellation shows up
# too and says so in its annotation). --job narrows both.
SEL_TSV="${OUT_DIR}/selected-jobs.tsv"
awk -F'\t' '$3 == "failure" || $3 == "timed_out" || ($3 == "cancelled" && $4 > 0)' "${JOBS_TSV}" > "${SEL_TSV}"
if [[ -n "${JOB_RE}" ]]; then
  # Through ENVIRON: `awk -v` would eat the backslashes of an escaped paren.
  JOB_RE="${JOB_RE}" awk -F'\t' '$2 ~ ENVIRON["JOB_RE"]' "${SEL_TSV}" > "${SEL_TSV}.tmp" \
    || die "--job: not a valid ERE: ${JOB_RE}"
  mv "${SEL_TSV}.tmp" "${SEL_TSV}"
fi

# Skipped jobs by base name (matrix suffix " (...)" dropped), for the rerun check.
SKIPPED="$(awk -F'\t' '$3 == "skipped" { n = $2; sub(/ \(.*$/, "", n); print n }' "${JOBS_TSV}" | sort -u | tr '\n' ' ')"
# ci.yaml needs, one line per job: "<job> <need> <need> ...". Read from the
# local checkout, which may differ from the run's commit.
NEEDS_MAP=""
if [[ -f "${CI_YAML}" ]]; then
  NEEDS_MAP="$(awk '
    /^  [a-z0-9_-]+:[[:space:]]*$/ { job = $1; sub(/:$/, "", job); next }
    /^    needs:/ {
      line = $0; sub(/^    needs:[[:space:]]*/, "", line)
      gsub(/[][,]/, " ", line); print job, line
    }' "${CI_YAML}")"
fi
# downstream_skipped <base job name>: skipped jobs that need it, transitively.
downstream_skipped() {
  [[ -n "${NEEDS_MAP}" && -n "${SKIPPED}" ]] || return 0
  printf '%s\n' "${NEEDS_MAP}" | awk -v target="$1" -v skipped=" ${SKIPPED} " '
    { job = $1; $1 = ""; deps[job] = $0; order[n++] = job }
    END {
      found[target] = 1; changed = 1; out = ""
      while (changed) {
        changed = 0
        for (i = 0; i < n; i++) {
          j = order[i]
          if (j in found || index(skipped, " " j " ") == 0) continue
          m = split(deps[j], d, " ")
          for (k = 1; k <= m; k++) if (d[k] in found) {
            found[j] = 1; changed = 1; out = out (out == "" ? "" : ", ") j; break
          }
        }
      }
      print out
    }'
}

hdr "Failed and cancelled jobs${JOB_RE:+ matching /${JOB_RE}/}"
ZERO_STEP_FAILED=""
ANY_DOWN=""
if [[ ! -s "${SEL_TSV}" ]]; then
  info "no failed job${JOB_RE:+ matching /${JOB_RE}/} (yet)"
else
  while IFS="$(printf '\t')" read -r id name concl nsteps fsteps _; do
    runner="$(gh api "repos/${REPO}/actions/jobs/${id}" --jq '.runner_name // "-"' 2>/dev/null </dev/null || echo '?')"
    echo "${concl}  ${name}  (job ${id}, runner ${runner:--}, ${nsteps} steps, failed step: ${fsteps})"
    if [[ "${concl}" == "failure" && "${nsteps}" == "0" ]]; then
      ZERO_STEP_FAILED="${ZERO_STEP_FAILED:+${ZERO_STEP_FAILED}; }${name} (job ${id}, runner ${runner:--})"
    fi
    if [[ "${concl}" == "failure" ]]; then
      base="${name%% (*}"
      down="$(downstream_skipped "${base}")"
      if [[ -n "${down}" ]]; then
        info "  skipped downstream (per local ci.yaml; a path-filtered job stays skipped): ${down}"
        ANY_DOWN=1
      fi
    fi
  done < "${SEL_TSV}"
  if [[ -n "${ANY_DOWN}" ]]; then
    info "the run's green count hides the skipped jobs above; a rerun of the failed jobs re-evaluates them: gh run rerun ${RUN_ID} --failed"
    info "  (only for an infra fault — see SKILL.md § Fix with the right shape)"
  fi
fi

if [[ "${STATUS}" != "completed" ]]; then
  info "run is still in progress — job logs become available once it completes"
  info "re-run this script when the run has finished: --run ${RUN_ID}"
  exit 2
fi

# ---------------------------------------------------------------------------
# Failed-job logs and annotations
# ---------------------------------------------------------------------------
RAW_FILE="${OUT_DIR}/failed-jobs.raw.log"
LOG_FILE="${OUT_DIR}/failed-jobs.log"
NOSRC_FILE="${OUT_DIR}/failed-jobs.nosrc.log"
ANNOT_FILE="${OUT_DIR}/annotations.txt"
: > "${RAW_FILE}"
: > "${ANNOT_FILE}"
NO_LOG=""
hdr "Downloading job logs and annotations"
# The job-id log endpoint, not `gh run view --job <id> --log-failed`: the
# latter resolves the job by name in the run's LATEST attempt (so --attempt
# would read the wrong log), and on the current runner log format it cannot
# attribute steps ("UNKNOWN STEP") and returns the whole job log anyway. A
# runner that died mid-job left no log blob: the endpoint answers 404.
while IFS="$(printf '\t')" read -r id name concl nsteps fsteps _; do
  annots="$(gh api "repos/${REPO}/check-runs/${id}/annotations" \
    --jq '.[] | "\(.annotation_level)\t\(.message | gsub("\n"; " "))"' 2>/dev/null </dev/null || true)"
  if [[ -n "${annots}" ]]; then
    printf '%s\n' "${annots}" | awk -v job="${name}" '{ print job "\t" $0 }' >> "${ANNOT_FILE}"
  fi
  # A cancelled job is read only when the job wall cancelled it; a run-level
  # cancellation (a newer push) says nothing about the change.
  case "${concl}" in
    failure|timed_out) ;;
    *) case "${annots}" in *"exceeded the maximum execution time"*) ;; *) continue ;; esac ;;
  esac
  part="${OUT_DIR}/.job-${id}.log"
  if gh api "repos/${REPO}/actions/jobs/${id}/logs" > "${part}" 2>/dev/null </dev/null && [[ -s "${part}" ]]; then
    info "${name}: $(wc -l < "${part}" | tr -d ' ') lines"
    # <job> TAB <step> TAB <line>, the layout `gh run view --log` prints.
    awk -v job="${name}" '{ print job "\t-\t" $0 }' "${part}" >> "${RAW_FILE}"
  else
    info "${name}: no log (HTTP 404: the runner never uploaded one)"
    NO_LOG="${NO_LOG}${name} (job ${id}); "
  fi
  rm -f "${part}"
done < "${SEL_TSV}"
strip_ansi "${RAW_FILE}" "${LOG_FILE}"
strip_source "${RAW_FILE}" "${NOSRC_FILE}"
rm -f "${RAW_FILE}"
info "wrote ${LOG_FILE} ($(wc -l < "${LOG_FILE}" | tr -d ' ') lines; ${NOSRC_FILE} without the echoed step scripts)"
info "wrote ${ANNOT_FILE} ($(wc -l < "${ANNOT_FILE}" | tr -d ' ') annotations)"
if [[ -s "${ANNOT_FILE}" ]]; then
  # Failure-level only: the warnings are cache-restore noise on every job.
  awk -F'\t' '$2 == "failure" { print "       " $1 ": " $3 }' "${ANNOT_FILE}" | cut -c1-240 || true
fi

# ---------------------------------------------------------------------------
# Chainsaw failure excerpt
# ---------------------------------------------------------------------------
hdr "Extracting chainsaw failure blocks"
write_excerpt "${LOG_FILE}" "${NOSRC_FILE}" "${OUT_DIR}/chainsaw-excerpt.log"

# ---------------------------------------------------------------------------
# Signature scan
# ---------------------------------------------------------------------------
hdr "Signature scan"
if [[ -n "${ZERO_STEP_FAILED}" ]]; then
  report_match "runner-lost" \
    "infra: a failed job with zero recorded steps is a runner that died mid-assignment" \
    "${ZERO_STEP_FAILED}"
  # Its sibling on the same runner is cancelled at "Set up job".
  sib="$(awk -F'\t' '$3 == "cancelled" && $4 == 1 && $6 == "Set up job=cancelled" { printf "%s%s (job %s)", (n++ ? "; " : ""), $2, $1 }' "${SEL_TSV}")"
  [[ -z "${sib}" ]] || info "        cancelled at 'Set up job' on the same fault: ${sib}"
fi
if [[ -n "${NO_LOG}" && -z "${ZERO_STEP_FAILED}" ]]; then
  info "failed without a downloadable log: ${NO_LOG}— read ${ANNOT_FILE}"
fi
scan_signatures "${NOSRC_FILE}" "${ANNOT_FILE}"
verdict

map_suites "${LOG_FILE}"
image_provenance "${LOG_FILE}"

# ---------------------------------------------------------------------------
# Artifacts (JUnit reports, tempest results)
# ---------------------------------------------------------------------------
hdr "Artifacts of run ${RUN_ID}"
gh api "repos/${REPO}/actions/runs/${RUN_ID}/artifacts?per_page=100" \
  --jq '.artifacts[] | "\(.name)\t\(.size_in_bytes) bytes\texpired=\(.expired)"' || true
echo
info "download one with:"
info "  gh run download ${RUN_ID} --repo ${REPO} -n <name> -D ${OUT_DIR}/artifacts"

hdr "Done"
info "evidence under ${OUT_DIR}/"
