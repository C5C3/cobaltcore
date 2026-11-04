#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# The out-of-band consistency checks of the chaos harness (#1387), sourced by
# lib/workload.sh for its verify command and by hack/chaos-random.sh for the
# verdict of a random run. Defining the functions is all that sourcing does.
#
#   placement_audit <nova-deployment>   nova-manage placement audit
#   ovn_db_sync <id> <neutron-cr>       one log-mode run of the Neutron's OVN
#                                       db-sync CronJob, as Job
#                                       chaos-wl-<id>-ovn-db-sync
#
# Both work in NAMESPACE, print CONSISTENCY-OK <check> when the check holds,
# and return 1 after a CONSISTENCY-FAIL or FAIL line otherwise.
# CHAOS_WORKLOAD_DB_SYNC_SECONDS bounds the wait for the db-sync Job (600).
#
# docs/reference/testing/chaos-e2e-tests.md (Consistency checks) describes
# them.

# placement_audit <deployment> — nova-manage placement audit in the API
# container, which carries [placement] and both database connections. Exit 3
# is orphaned allocations; every other non-zero code is an error of its own.
placement_audit() {
  local out rc=0
  echo "=== placement audit (deploy/$1) ==="
  out="$(kubectl exec "deploy/$1" -n "$NAMESPACE" -c nova-api -- \
    nova-manage --config-dir /etc/nova/nova.conf.d placement audit --verbose 2>&1)" || rc=$?
  case "$rc" in
    0)
      echo "$out"
      echo "CONSISTENCY-OK placement-audit"
      ;;
    3)
      echo "CONSISTENCY-FAIL placement-audit: nova-manage found orphaned allocations"
      echo "$out"
      return 1
      ;;
    *)
      echo "FAIL: placement-audit: nova-manage exited ${rc}"
      echo "$out"
      return 1
      ;;
  esac
}

# ovn_db_sync <id> <neutron> — one run of the Neutron's OVN db-sync CronJob in
# log mode. The utility exits 0 whether or not the databases agree, so its
# report lines are the verdict.
ovn_db_sync() {
  local cronjob="$2-ovn-db-sync" job="chaos-wl-$1-ovn-db-sync"
  local limit="${CHAOS_WORKLOAD_DB_SYNC_SECONDS:-600}"
  local state="" log drift deadline cmd
  echo "=== OVN database comparison (cronjob/${cronjob}) ==="
  if ! cmd="$(kubectl get cronjob "$cronjob" -n "$NAMESPACE" \
    -o 'jsonpath={.spec.jobTemplate.spec.template.spec.containers[0].command}' 2>/dev/null)"; then
    echo "FAIL: ovn-db-sync: cronjob ${cronjob} not found: set spec.ovnDBSync {syncMode: log, suspend: true} on the Neutron CR"
    return 1
  fi
  # The neutron-operator renders spec.ovnDBSync.syncMode into the command. In
  # repair mode the run would rewrite the Northbound database it reports on.
  if ! grep -qF '"--ovn-neutron_sync_mode","log"' <<<"$cmd"; then
    echo "FAIL: ovn-db-sync: cronjob ${cronjob} does not run in log mode: set spec.ovnDBSync.syncMode: log on the Neutron CR"
    echo "command: ${cmd}"
    return 1
  fi
  kubectl delete job "$job" -n "$NAMESPACE" --ignore-not-found >/dev/null
  if ! kubectl create job "$job" -n "$NAMESPACE" --from="cronjob/${cronjob}"; then
    echo "FAIL: ovn-db-sync: creating job ${job} from cronjob/${cronjob} failed"
    return 1
  fi
  deadline=$(($(date +%s) + limit))
  while true; do
    state="$(kubectl get job "$job" -n "$NAMESPACE" \
      -o 'jsonpath={.status.conditions[?(@.status=="True")].type}' 2>/dev/null || true)"
    case " ${state} " in
      *" Complete "* | *" Failed "*) break ;;
    esac
    if [ "$(date +%s)" -ge "$deadline" ]; then
      break
    fi
    sleep 10
  done
  log="$(kubectl logs "job/${job}" -n "$NAMESPACE" 2>&1 || true)"
  kubectl delete job "$job" -n "$NAMESPACE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  case " ${state} " in
    *" Complete "*) ;;
    *" Failed "*)
      echo "FAIL: ovn-db-sync: job ${job} failed"
      echo "$log"
      return 1
      ;;
    *)
      echo "FAIL: ovn-db-sync: job ${job} did not complete within ${limit}s"
      echo "$log"
      return 1
      ;;
  esac
  # A run that compared anything logs its completion, so a log without it
  # cannot pass as a clean comparison.
  if ! grep -q 'Neutron OVN DB sync completed' <<<"$log"; then
    echo "FAIL: ovn-db-sync: the log of job ${job} has no 'Neutron OVN DB sync completed' line"
    echo "$log"
    return 1
  fi
  # The warnings neutron 28.0.1 (ovn_db_sync.py) logs in log mode for each
  # difference it finds. Most say "found in", the rest use their own words.
  # The Southbound pass says "found in" too, but it runs in every mode, writes
  # the segment host mappings it names and compares no Northbound row. Its
  # "New host" line comes on every run for a chassis without bridge mappings,
  # which never gets a mapping. Only its lines name the OVN SB DB.
  drift="$(grep -E 'found in (Neutron|OVN)|Port Groups to add|ACLs to be added|should (not )?be present in OVN|out of sync|missing in OVN|needs to be updated in OVN|must be updated' <<<"$log" | grep -vF 'OVN SB DB' || true)"
  if [ -n "$drift" ]; then
    echo "CONSISTENCY-FAIL ovn-db-sync: Neutron and OVN disagree"
    echo "$drift"
    return 1
  fi
  grep -E 'sync (started|completed)' <<<"$log" || true
  echo "CONSISTENCY-OK ovn-db-sync"
}
