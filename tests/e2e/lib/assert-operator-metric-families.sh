#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/lib/assert-operator-metric-families.sh: prove that an operator
# release exports its own Prometheus families with the labels dashboards and
# alerts query.
#
# A suite calls this once the Job, rotation or re-mint whose family it reads
# has run, and before it deletes the CR, whose series go with it. The chart
# runs two replicas and only the leader reconciles, so the script reads every
# pod of the release and judges checks 3 to 5 on the union of their bodies:
#   1. the release has at least one pod;
#   2. every pod's metrics endpoint answers non-empty through the API server's
#      pod proxy;
#   3. some <prefix>_reconcile_duration_seconds_count line has a value of 1 or
#      more;
#   4. no <prefix>_reconcile_errors_total line carries condition_type="UNKNOWN",
#      the label of an error from a sub-reconciler missing from the operator's
#      condition map;
#   5. every <sample> has a line of exactly its family name that carries every
#      listed label pair, in any order. A name ending in _total or _count needs
#      a value of 1 or more, any other name a number. When a sample is
#      missing, every pod whose containers restarted is named too: the
#      families live in the operator's memory and a Job is recorded once, so
#      a restart after the record loses its sample.
# A Job's terminal state is recorded on the operator's next pass, so checks 3
# and 5 re-read the bodies every 5 s until METRICS_WAIT_SECONDS (default 60)
# run out. The unit test sets 0.
#
# Usage:
#   assert-operator-metric-families.sh <operator-namespace> <release> <prefix> [<sample>...]
#
# <release> is the Helm release name, <prefix> the operator's metric prefix
# (e.g. keystone_operator). A <sample> is a family name followed by the label
# pairs it must carry, in exposition syntax:
#   <family>{<label>="<value>"[,<label>="<value>"...]}
# A pair counts only between "{" or "," and "," or "}", so namespace="x"
# never matches subnamespace="x". Each check prints one OK: line, or a FAIL:
# line with its evidence and exits 1; check 5 prints one line per sample.
# Fewer than three arguments, an empty argument, or a malformed sample prints
# the usage line on stderr and exits 2.
#
# Chainsaw runs script steps in the suite directory, so a suite calls this as
# "../../lib/assert-operator-metric-families.sh". The script only reads.

set -euo pipefail

usage() {
  echo "usage: assert-operator-metric-families.sh <operator-namespace> <release> <prefix> [<sample>...]" >&2
  exit 2
}

if [ "$#" -lt 3 ]; then
  usage
fi
for arg in "$@"; do
  if [ -z "$arg" ]; then
    usage
  fi
done

# Not NAMESPACE: chainsaw injects that variable into every script step.
ns="$1"
release="$2"
prefix="$3"
shift 3

# The two reconcile families checks 3 and 4 read.
duration_count="${prefix}_reconcile_duration_seconds_count"
errors_total="${prefix}_reconcile_errors_total"

sample_re='^[a-zA-Z_:][a-zA-Z0-9_:]*\{[a-zA-Z_][a-zA-Z0-9_]*="[^"]*"(,[a-zA-Z_][a-zA-Z0-9_]*="[^"]*")*\}$'
for sample in "$@"; do
  if ! [[ $sample =~ $sample_re ]]; then
    usage
  fi
done

# The chart's metrics.port, which the Deployment passes as
# --metrics-bind-address. The endpoint serves plain HTTP without
# authentication.
metrics_port=8080

# count_lines <text>: the number of non-empty lines. grep -c prints 0 and exits
# 1 on no match.
count_lines() {
  grep -c . <<<"$1" || true
}

# scrape: reads every pod's body into union. An unreachable or empty endpoint
# leaves its FAIL: line in scrape_error and returns 1.
scrape() {
  local pod body
  union=""
  scrape_error=""
  for pod in $pods; do
    if ! body="$(kubectl get --raw "/api/v1/namespaces/$ns/pods/$pod:$metrics_port/proxy/metrics")"; then
      scrape_error="FAIL: metrics endpoint of pod $pod unreachable"
      return 1
    fi
    if [ -z "$body" ]; then
      scrape_error="FAIL: metrics endpoint of pod $pod returned nothing"
      return 1
    fi
    union+="$body"$'\n'
  done
}

# family_lines <family> <pairs> <mode>: the lines of union whose metric name is
# exactly <family>, that carry every pair of <pairs> (label="value" joined by
# commas, or empty), and whose value passes <mode>: "count" needs a number of 1
# or more, "number" any number, "any" nothing. Pairs are split by their own
# pattern, not on commas, because a value may hold one. A counter prints as 12
# or as 1.2e+06, so awk compares the value as a number, under LC_ALL=C so that
# no locale reads the exponent form with a decimal comma.
family_lines() {
  LC_ALL=C awk -v fam="$1" -v want="$2" -v mode="$3" '
    BEGIN {
      n = 0
      while (want != "" && match(want, /^[a-zA-Z_][a-zA-Z0-9_]*="[^"]*"/)) {
        pairs[++n] = substr(want, 1, RLENGTH)
        want = substr(want, RLENGTH + 2)
      }
    }
    index($0, fam "{") == 1 {
      # The value follows the last "}", since a label value may hold one.
      end = 0
      for (i = length($0); i > length(fam); i--) {
        if (substr($0, i, 1) == "}") { end = i; break }
      }
      if (end == 0) next
      labels = "," substr($0, length(fam) + 2, end - length(fam) - 2) ","
      rest = substr($0, end + 1)
    }
    index($0, fam " ") == 1 {
      labels = ",,"
      rest = substr($0, length(fam) + 1)
    }
    index($0, fam "{") == 1 || index($0, fam " ") == 1 {
      split(rest, f, " ")
      if (mode != "any" && f[1] !~ /^[-+]?([0-9]+[.]?[0-9]*|[.][0-9]+)([eE][-+]?[0-9]+)?$/) next
      if (mode == "count" && f[1] + 0 < 1) next
      for (j = 1; j <= n; j++) {
        if (index(labels, "," pairs[j] ",") == 0) next
      }
      print
    }' <<<"$union"
}

# sample_family <sample> / sample_pairs <sample>: the two halves of a sample.
sample_family() {
  printf '%s' "${1%%\{*}"
}
sample_pairs() {
  local pairs="${1#*\{}"
  printf '%s' "${pairs%\}}"
}

# sample_mode <family>: a counter or a histogram's count needs 1 or more.
sample_mode() {
  case "$1" in
    *_total | *_count) echo count ;;
    *) echo number ;;
  esac
}

# list_pods: one "<pod> <restart count>..." line per pod of the release, a
# count per container.
list_pods() {
  kubectl get pods -n "$ns" -l "app.kubernetes.io/instance=$release" \
    -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.containerStatuses[*].restartCount}{"\n"}{end}'
}

# all_present: checks 3 and 5 both pass on union.
all_present() {
  local sample family
  [ -n "$(family_lines "$duration_count" "" count)" ] || return 1
  for sample in "$@"; do
    family="$(sample_family "$sample")"
    [ -n "$(family_lines "$family" "$(sample_pairs "$sample")" "$(sample_mode "$family")")" ] || return 1
  done
}

# ── Check 1: the release has a pod ────────────────────────────────────────
if ! pod_lines="$(list_pods)"; then
  echo "FAIL: cannot list the pods of release $release"
  exit 1
fi
pods="$(awk '{ print $1 }' <<<"$pod_lines")"
pod_count="$(count_lines "$pods")"
if [ "$pod_count" -eq 0 ]; then
  echo "FAIL: no pod of release $release in $ns"
  exit 1
fi
echo "OK: release $release runs $pod_count pods: $(paste -s -d ' ' - <<<"$pods")"

# ── Check 2: every metrics endpoint answers ───────────────────────────────
if ! scrape; then
  echo "$scrape_error"
  exit 1
fi
echo "OK: the metrics endpoints of all $pod_count pods answer"

# Checks 3 and 5 share one deadline, so the wait stays inside the step's
# timeout however many samples a suite passes. A failed re-read counts as an
# empty union for its round; one that is still failing at the deadline fails
# check 2.
metrics_wait="${METRICS_WAIT_SECONDS:-60}"
deadline=$((SECONDS + metrics_wait))
until all_present "$@"; do
  if [ "$SECONDS" -ge "$deadline" ]; then
    break
  fi
  sleep 5
  scrape || union=""
done
if [ -n "$scrape_error" ]; then
  echo "$scrape_error"
  exit 1
fi

# ── Check 3: the controller reconciled ────────────────────────────────────
if [ -z "$(family_lines "$duration_count" "" count)" ]; then
  echo "FAIL: no $duration_count line of release $release counts a reconcile after ${metrics_wait}s"
  grep -F "$duration_count" <<<"$union" || true
  exit 1
fi
echo "OK: $duration_count counts a reconcile"

# ── Check 4: no error carries the UNKNOWN condition type ──────────────────
unknown="$(family_lines "$errors_total" 'condition_type="UNKNOWN"' number)"
if [ -n "$unknown" ]; then
  echo "FAIL: $errors_total carries condition_type=\"UNKNOWN\", so a sub-reconciler is missing from the condition map"
  echo "$unknown"
  exit 1
fi
echo "OK: no $errors_total line carries condition_type=\"UNKNOWN\""

# ── Check 5: every sample is exported ─────────────────────────────────────
missing=0
for sample in "$@"; do
  family="$(sample_family "$sample")"
  mode="$(sample_mode "$family")"
  if [ -n "$(family_lines "$family" "$(sample_pairs "$sample")" "$mode")" ]; then
    echo "OK: sample $sample"
    continue
  fi
  missing=$((missing + 1))
  if [ "$mode" = count ]; then
    echo "FAIL: sample $sample has no line with a value of 1 or more after ${metrics_wait}s"
  else
    echo "FAIL: sample $sample has no line with a numeric value after ${metrics_wait}s"
  fi
  lines="$(family_lines "$family" "" any)"
  if [ -n "$lines" ]; then
    echo "$lines"
  else
    echo "  no line of family $family"
  fi
done
if [ "$missing" -ne 0 ]; then
  # The counts of check 1 predate the wait, and a restart inside it loses a
  # sample too, so they are read again. When that read fails, kubectl's
  # error reaches stderr and the counts of check 1 stand.
  if ! restarts="$(list_pods)"; then
    restarts="$pod_lines"
  fi
  awk '{
    n = 0
    for (i = 2; i <= NF; i++) n += $i
    if (n > 0) printf "FAIL: pod %s restarted %d times; in-memory counters recorded before the restart are gone\n", $1, n
  }' <<<"$restarts"
  exit 1
fi
