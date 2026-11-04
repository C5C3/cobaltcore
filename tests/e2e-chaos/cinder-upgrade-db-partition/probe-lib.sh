#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Helpers of cinder-upgrade-db-partition, sourced by its scripts from the
# suite directory with NAMESPACE set by Chainsaw.
#
# The suite cuts the pods of one phase Job of the Cinder upgrade off from
# MariaDB at a time (02-netpol-expand.yaml, 03-netpol-contract.yaml) and holds
# the cut for HOLD_SECONDS. hold_phase waits for the phase and its Job's pod,
# then proves the phase does not move and the Job does not succeed while the
# partition holds.

CR=cinder-upg-part
HOLD_SECONDS=90

# phase_rank <phase>: the position of a phase in the upgrade flow
# (internal/common/database/upgrade.go), 0 for the empty steady state.
phase_rank() {
  case "$1" in
    Expanding) echo 1 ;;
    Migrating) echo 2 ;;
    RollingUpdate) echo 3 ;;
    Contracting) echo 4 ;;
    *) echo 0 ;;
  esac
}

# cr_state: "<upgradePhase>|<installedRelease>" from one read. A failed read
# fails the call.
cr_state() {
  kubectl get cinder "$CR" -n "$NAMESPACE" \
    -o 'jsonpath={.status.upgradePhase}|{.status.installedRelease}'
}

# job_succeeded <job>: the Job's .status.succeeded, empty while it has not
# succeeded or does not exist yet.
job_succeeded() {
  kubectl get job "$1" -n "$NAMESPACE" -o 'jsonpath={.status.succeeded}' 2>/dev/null || true
}

# running_pods <job>: the names of the Job's pods in phase Running.
running_pods() {
  kubectl get pods -n "$NAMESPACE" -l "job-name=$1" \
    --field-selector=status.phase=Running -o 'jsonpath={.items[*].metadata.name}'
}

# hold_phase <expand|contract> <Phase> <wait seconds>: wait up to <wait
# seconds> for upgradePhase <Phase> and a Running pod of Job
# cinder-upg-part-db-<expand|contract>, then require for HOLD_SECONDS, every
# 5 s, that the phase still reads <Phase> and the Job has not succeeded.
#
# The partition was applied before the patch, so a Job that succeeds, a phase
# past <Phase> or a finished upgrade before the pod is seen Running means a
# connect got through the cut: that fails with the FAIL line the suite names,
# not with a timeout. An empty phase is the moment before the operator saw the
# patch, and counts as not yet.
hold_phase() {
  local slug="$1" want="$2" limit="$3" job="${CR}-db-$1" rank deadline state phase release pods succeeded end
  local finished="FAIL: the db-${slug} Job finished while its partition should hold"
  rank="$(phase_rank "$want")"
  deadline=$(($(date +%s) + limit))
  while true; do
    if ! state="$(cr_state)"; then
      echo "FAIL: reading cinder/${CR} failed while waiting for ${want}"
      return 1
    fi
    phase="${state%%|*}"
    release="${state##*|}"
    succeeded="$(job_succeeded "$job")"
    if [ -n "$succeeded" ] && [ "$succeeded" != "0" ]; then
      echo "$finished (succeeded=${succeeded} before a Running pod was seen)"
      return 1
    fi
    if [ "$(phase_rank "$phase")" -gt "$rank" ] || { [ -z "$phase" ] && [ "$release" = "2026.2" ]; }; then
      echo "$finished (upgradePhase='${phase}' installedRelease=${release} before a Running pod was seen)"
      return 1
    fi
    pods=""
    if [ "$phase" = "$want" ]; then
      pods="$(running_pods "$job" || true)"
    fi
    if [ -n "$pods" ]; then
      echo "OK: upgradePhase=${want}, ${job} runs pod ${pods}"
      break
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "FAIL: no Running pod of ${job} with upgradePhase ${want} within ${limit}s (upgradePhase='${phase}' installedRelease=${release})"
      kubectl get pods -n "$NAMESPACE" -l "job-name=${job}" -o wide 2>&1 || true
      return 1
    fi
    sleep 2
  done

  end=$(($(date +%s) + HOLD_SECONDS))
  while true; do
    if ! state="$(cr_state)"; then
      echo "FAIL: reading cinder/${CR} failed during the ${slug} hold"
      return 1
    fi
    phase="${state%%|*}"
    succeeded="$(job_succeeded "$job")"
    if [ -n "$succeeded" ] && [ "$succeeded" != "0" ]; then
      echo "$finished (succeeded=${succeeded} during the hold)"
      return 1
    fi
    if [ "$phase" != "$want" ]; then
      echo "$finished (upgradePhase moved to '${phase}' during the hold)"
      return 1
    fi
    if [ "$(date +%s)" -ge "$end" ]; then
      break
    fi
    sleep 5
  done
  echo "$(printf '%s' "$slug" | tr '[:lower:]' '[:upper:]')-HELD-OK: upgradePhase stayed ${want} and ${job} did not succeed for ${HOLD_SECONDS}s"
}
