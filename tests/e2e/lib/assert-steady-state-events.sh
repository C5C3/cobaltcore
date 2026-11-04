#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/lib/assert-steady-state-events.sh: prove that an operator leaves a
# Ready, unchanged CR alone.
#
# An operator that keeps reconciling a Ready CR spends API-server capacity, and
# such a loop shows up as events on the CR. The operators record core/v1
# Events, whose client-side correlator raises an Event's count on a repeat
# instead of creating a new one, so the script compares counts:
#   1. the CR exists and its Ready condition is True; its UID and generation
#      are recorded;
#   2. the first snapshot of the events on the CR's UID, as name and count
#      pairs (an event without count counts as 1);
#   3. a window of EVENT_WINDOW_SECONDS (default 120; the unit test sets 0);
#   4. the second snapshot, then the CR again, which must still be Ready=True
#      at the same UID and generation;
#   5. the budget of 0: the sum over the second snapshot of the positive part
#      of each event's count difference, where an event absent from the first
#      snapshot counts with its full count and one that disappeared adds
#      nothing.
#
# Usage:
#   assert-steady-state-events.sh <namespace> <resource> <name>
#
# <resource> is the fully qualified plural (e.g.
# keystones.keystone.openstack.c5c3.io). Each check prints one OK: line, or a
# FAIL: line with its evidence and exits 1; check 5 prints one FAIL: line per
# event that rose, with its name, type, reason, delta and message. A kubectl
# error, a CR that is not found included, fails the check it belongs to. A
# missing or empty argument prints the usage line on stderr and exits 2.
#
# Chainsaw runs script steps in the suite directory, so a suite calls this as
# "../../lib/assert-steady-state-events.sh". The script only reads.

set -euo pipefail

usage() {
  echo "usage: assert-steady-state-events.sh <namespace> <resource> <name>" >&2
  exit 2
}

if [ "$#" -ne 3 ] || [ -z "$1" ] || [ -z "$2" ] || [ -z "$3" ]; then
  usage
fi

# Not NAMESPACE: chainsaw injects that variable into every script step.
ns="$1"
resource="$2"
name="$3"

# read_cr: "<uid> <generation> <Ready status>" of the CR.
read_cr() {
  kubectl get "$resource" "$name" -n "$ns" \
    -o jsonpath='{.metadata.uid}{" "}{.metadata.generation}{" "}{.status.conditions[?(@.type=="Ready")].status}'
}

# snapshot: one "<name>\t<count>\t<type>\t<reason>\t<message>" line per event
# on the CR. Not UID: bash makes that variable read-only.
snapshot() {
  kubectl get events -n "$ns" --field-selector "involvedObject.uid=$uid" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.count}{"\t"}{.type}{"\t"}{.reason}{"\t"}{.message}{"\n"}{end}'
}

# tag <label>: prefixes every non-empty line of stdin with <label> and a tab,
# so one awk pass tells the two snapshots apart.
tag() {
  awk -v label="$1" 'NF { print label "\t" $0 }'
}

# ── Check 1: the CR is Ready ──────────────────────────────────────────────
if ! cr="$(read_cr)"; then
  echo "FAIL: cannot read $resource $name in $ns"
  exit 1
fi
read -r uid generation ready <<<"$cr"
if [ "${ready:-}" != "True" ]; then
  echo "FAIL: $resource $name is not Ready (Ready=${ready:-absent})"
  exit 1
fi
echo "OK: $resource $name is Ready at generation $generation"

# ── Check 2: the first snapshot ───────────────────────────────────────────
if ! before="$(snapshot)"; then
  echo "FAIL: cannot list the events of $resource $name before the window"
  exit 1
fi
echo "OK: $resource $name has $(grep -c . <<<"$before" || true) events before the window"

# ── Check 3: the window ───────────────────────────────────────────────────
window="${EVENT_WINDOW_SECONDS:-120}"
sleep "$window"
echo "OK: waited ${window}s"

# ── Check 4: the second snapshot, and the CR is unchanged ─────────────────
if ! after="$(snapshot)"; then
  echo "FAIL: cannot list the events of $resource $name after the window"
  exit 1
fi
if ! cr="$(read_cr)"; then
  echo "FAIL: cannot read $resource $name in $ns after the window"
  exit 1
fi
read -r uid_after generation_after ready_after <<<"$cr"
if [ "${uid_after:-}" != "$uid" ] || [ "${generation_after:-}" != "$generation" ] || [ "${ready_after:-}" != "True" ]; then
  echo "FAIL: $resource $name changed during the window: generation $generation -> ${generation_after:-absent}, Ready=${ready_after:-absent}, uid $uid -> ${uid_after:-absent}"
  exit 1
fi
echo "OK: $resource $name is still Ready at generation $generation"

# ── Check 5: the event budget ─────────────────────────────────────────────
# The message is everything after the fourth tab of a snapshot line, the
# fifth of a tagged one.
rises="$({
  tag before <<<"$before"
  tag after <<<"$after"
} | LC_ALL=C awk -F'\t' '
  $1 == "before" { before[$2] = ($3 == "" ? 1 : $3 + 0); next }
  {
    delta = ($3 == "" ? 1 : $3 + 0) - (($2 in before) ? before[$2] : 0)
    if (delta <= 0) next
    message = $0
    for (i = 1; i <= 5; i++) sub(/^[^\t]*\t/, "", message)
    printf "%d\tFAIL: event %s %s %s rose by %d: %s\n", delta, $2, $4, $5, delta, message
  }')"
if [ -n "$rises" ]; then
  total="$(awk -F'\t' '{ sum += $1 } END { print sum }' <<<"$rises")"
  cut -f2- <<<"$rises"
  echo "FAIL: $resource $name gained $total events in ${window}s, the budget is 0"
  exit 1
fi
echo "OK: $resource $name gained no event in ${window}s"
