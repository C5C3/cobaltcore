#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# The library of the lab failover suites (#1393) and of hack/lab-failover.sh,
# sourced as `. ../lib/failover.sh` with NAMESPACE set to openstack-ha.
#
# It reads the plane under test, ControlPlane cp-ha, and its backing services:
# the Galera cluster openstack-db, the broker openstack-rabbitmq and the two
# Raft clusters of OVNCentral cp-ha-ovn, three members each. It also starts the
# suites' workload, drains the API pods through the eviction subresource and
# renders the suites' chaos manifests. It sources
# ../../e2e-chaos/lib/controlplane.sh (cp_wait_condition, cp_status, cp_reason,
# cp_seed, cp_onboard_database, cp_teardown, cp_dump) and
# ../../e2e-chaos/lib/pods.sh (pod_uids).
#
# Every wait polls every LF_POLL_INTERVAL seconds (lf_evict_drain every
# LF_DRAIN_SAMPLE), prints an OK: line or a <CHECK>-OK token (GALERA-OK,
# BROKER-OK, RAFT-OK, NORTHD-SYNC-OK, DRAIN-OK) on success and an ERROR: line
# otherwise, and returns 1 when its timeout runs out. A failed status read
# counts as "not yet" until then. lf_wait_expired fails at once when its check
# fails, and lf_evict_drain on an unreadable PDB or pod list and on an
# eviction error other than a refusal, a throttle or NotFound.
# set -euo pipefail is the caller's, and no function relies on it. Every
# kubectl call works on the ambient context, which hack/lab-failover.sh pins.

LF_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/e2e-chaos/lib/controlplane.sh
. "${LF_LIB_DIR}/../../e2e-chaos/lib/controlplane.sh"
# shellcheck source=tests/e2e-chaos/lib/pods.sh
. "${LF_LIB_DIR}/../../e2e-chaos/lib/pods.sh"

LF_CP=cp-ha
LF_DB=openstack-db
LF_BROKER=openstack-rabbitmq
LF_OVN=cp-ha-ovn
# The eight API services, in the order api-eviction-drain drains them.
# shellcheck disable=SC2034 # read by the suites that source this library
LF_APIS="keystone horizon glance placement barbican neutron cinder nova"
# The ordinals of the three members of each Galera, broker and Raft cluster.
LF_MEMBERS="0 1 2"
# The size of each Galera, broker and Raft cluster, and the API replicas of
# Standard.
LF_MEMBER_COUNT="$(wc -w <<<"$LF_MEMBERS" | tr -d ' ')"
# The flavor and the image the seed Job (../fixtures/04-seed-job.yaml)
# creates, which the server stream boots from.
LF_SEED_FLAVOR=m1.nano
LF_SEED_IMAGE=4e0a0000-0000-4000-8000-000000001393
# The harness of the chaos suites; the unit test points it at a stub.
LF_WORKLOAD="${LF_LIB_DIR}/../../e2e-chaos/lib/workload.sh"
# Seconds between two polls of a wait. lf_evict_drain samples the endpoints
# every LF_DRAIN_SAMPLE seconds and retries a refused or throttled eviction
# after LF_EVICT_RETRY seconds. The unit test sets all three to 0.
LF_POLL_INTERVAL=5
LF_DRAIN_SAMPLE=2
LF_EVICT_RETRY=5
# The bound of each wait of lf_baseline; its five waits fit the suites' 10m
# step.
LF_BASELINE_WAIT=120
# The bound of a chaos object's delete, which waits for Chaos Mesh's recovery.
LF_CHAOS_DELETE_WAIT=120

# lf_since <epoch>: the seconds from <epoch> to now.
lf_since() {
  echo $(($(date +%s) - $1))
}

# lf_first_line <text>: the first line of <text>, for an error message.
lf_first_line() {
  printf '%s\n' "${1%%$'\n'*}"
}

# lf_wait_cp_ready <secs>: ControlPlane cp-ha reads Ready=True with reason
# AllReady.
lf_wait_cp_ready() {
  local reason
  cp_wait_condition controlplane "$LF_CP" Ready True "$1" || return 1
  reason="$(cp_reason controlplane "$LF_CP" Ready 2>/dev/null)" || reason=""
  if [ "$reason" != "AllReady" ]; then
    echo "ERROR: controlplane/${LF_CP} is Ready with reason '${reason:-<none>}', not AllReady"
    return 1
  fi
  echo "OK: controlplane/${LF_CP} is Ready=True/AllReady"
}

# galera_member_status <pod>: the Galera state of one member as
# `size=<n> status=<s> state=<s> ready=<s>`, from wsrep_cluster_size,
# wsrep_cluster_status, wsrep_local_state_comment and wsrep_ready. The query
# runs as root in the container mariadb, which carries MARIADB_ROOT_PASSWORD;
# the password reaches the client as MYSQL_PWD and stays out of every argv.
# A failed exec, as on a member that is down, prints `unreachable: <error>`
# and returns 1.
galera_member_status() {
  local out
  # shellcheck disable=SC2016 # $MARIADB_ROOT_PASSWORD and $1 expand in the container
  if ! out="$(kubectl exec "$1" -n "$NAMESPACE" -c mariadb -- sh -c \
    'MYSQL_PWD="$MARIADB_ROOT_PASSWORD" exec mariadb -uroot -N -B -e "$1"' _ \
    "SHOW GLOBAL STATUS WHERE Variable_name IN ('wsrep_cluster_size','wsrep_cluster_status','wsrep_local_state_comment','wsrep_ready')" 2>&1)"; then
    echo "unreachable: $(lf_first_line "$out")"
    return 1
  fi
  awk -F'\t' 'NF == 2 { v[$1] = $2 }
    END { printf "size=%s status=%s state=%s ready=%s\n", v["wsrep_cluster_size"],
      v["wsrep_cluster_status"], v["wsrep_local_state_comment"], v["wsrep_ready"] }' <<<"$out"
}

# galera_wait_healthy <secs>: openstack-db-0 to -2 all report
# `size=<LF_MEMBER_COUNT> status=Primary state=Synced ready=ON`, and MariaDB
# openstack-db reads Ready=True and GaleraReady=True. Prints
# GALERA-OK <elapsed>s.
galera_wait_healthy() {
  local secs="$1" start deadline i pod st healthy last ready galera
  start="$(date +%s)"
  deadline=$((start + secs))
  while true; do
    healthy=1
    last=""
    for i in $LF_MEMBERS; do
      pod="${LF_DB}-${i}"
      st="$(galera_member_status "$pod")" || healthy=0
      if [ "$st" != "size=${LF_MEMBER_COUNT} status=Primary state=Synced ready=ON" ]; then healthy=0; fi
      last="${last}  ${pod}: ${st}"$'\n'
    done
    ready="$(cp_status mariadb "$LF_DB" Ready 2>/dev/null)" || ready=""
    galera="$(cp_status mariadb "$LF_DB" GaleraReady 2>/dev/null)" || galera=""
    if [ "$healthy" = 1 ] && [ "$ready" = "True" ] && [ "$galera" = "True" ]; then
      echo "GALERA-OK $(lf_since "$start")s"
      return 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "ERROR: Galera ${LF_DB} not healthy within ${secs}s (Ready='${ready:-<none>}', GaleraReady='${galera:-<none>}'); the last status of each member:"
      printf '%s' "$last"
      return 1
    fi
    sleep "$LF_POLL_INTERVAL"
  done
}

# lf_broker_ctl <pod> <rabbitmqctl arguments...>: rabbitmqctl in the
# container rabbitmq of <pod>, with the lines before its JSON dropped. A failed
# exec prints `unreachable: <error>`, and a reply without a line that starts
# with [ or { prints `unreadable: <first line>`; both return 1.
lf_broker_ctl() {
  local pod="$1" out json
  shift
  out="$(kubectl exec "$pod" -n "$NAMESPACE" -c rabbitmq -- rabbitmqctl "$@" 2>&1)" || {
    echo "unreachable: $(lf_first_line "$out")"
    return 1
  }
  json="$(awk '/^[[{]/ { json = 1 } json' <<<"$out")"
  if [ -z "$json" ]; then
    echo "unreadable: $(lf_first_line "$out")"
    return 1
  fi
  printf '%s\n' "$json"
}

# rabbit_cluster <pod>: the broker's view from <pod> as
# `running=<n> partitions=<n> alarms=<n>`, the lengths of .running_nodes,
# .partitions and .alarms of `rabbitmqctl cluster_status`. A failed exec
# prints `unreachable: <error>`, a reply that is no JSON `unreadable:
# <first line>`, and both return 1.
rabbit_cluster() {
  local out
  out="$(lf_broker_ctl "$1" cluster_status --formatter json)" || {
    echo "$out"
    return 1
  }
  jq -r '"running=\(.running_nodes | length) partitions=\(.partitions | length) alarms=\(.alarms | length)"' \
    <<<"$out" 2>/dev/null || {
    echo "unreadable: $(lf_first_line "$out")"
    return 1
  }
}

# rabbit_quorum_queues: the quorum queues of the vhost / as
# `offline=<queues with a member offline> under3=<queues with fewer than 3 members>`,
# read in server-0. An empty queue list reads `offline= under3=0`.
rabbit_quorum_queues() {
  local out
  out="$(lf_broker_ctl "${LF_BROKER}-server-0" list_queues -p / name type members online --formatter json)" || {
    echo "$out"
    return 1
  }
  jq -rs '(add // []) | [.[] | select(.type == "quorum")]
    | "offline=\([.[] | select((.online | length) != (.members | length)) | .name] | join(","))"
      + " under3=\([.[] | select((.members | length) < 3)] | length)"' <<<"$out" 2>/dev/null || {
    echo "unreadable: $(lf_first_line "$out")"
    return 1
  }
}

# rabbit_wait_healthy <secs>: every broker pod reports
# `running=<LF_MEMBER_COUNT> partitions=0 alarms=0`, and every quorum queue
# of the vhost / has all its members online. Prints `BROKER-OK <elapsed>s`
# with the count of quorum queues with fewer than 3 members, an observation
# for the record.
rabbit_wait_healthy() {
  local secs="$1" start deadline i pod st healthy last queues
  start="$(date +%s)"
  deadline=$((start + secs))
  while true; do
    healthy=1
    last=""
    for i in $LF_MEMBERS; do
      pod="${LF_BROKER}-server-${i}"
      st="$(rabbit_cluster "$pod")" || healthy=0
      if [ "$st" != "running=${LF_MEMBER_COUNT} partitions=0 alarms=0" ]; then healthy=0; fi
      last="${last}  ${pod}: ${st}"$'\n'
    done
    queues="$(rabbit_quorum_queues)" || healthy=0
    case "$queues" in
      "offline= under3="*) ;;
      *) healthy=0 ;;
    esac
    if [ "$healthy" = 1 ]; then
      echo "BROKER-OK $(lf_since "$start")s quorum-queues-under-3-members=${queues##*under3=}"
      return 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "ERROR: broker ${LF_BROKER} not healthy within ${secs}s; the last status of each node:"
      printf '%s' "$last"
      echo "  quorum queues: ${queues}"
      return 1
    fi
    sleep "$LF_POLL_INTERVAL"
  done
}

# rabbit_partition_handling: the broker's cluster_partition_handling, read in
# server-1; the lab's broker printed {ok,pause_minority} on 2026-10-19.
rabbit_partition_handling() {
  kubectl exec "${LF_BROKER}-server-1" -n "$NAMESPACE" -c rabbitmq -- \
    rabbitmqctl eval 'application:get_env(rabbit, cluster_partition_handling).'
}

# ovn_cluster_status <nb|sb> <i>: `ovs-appctl cluster/status` of the database
# in pod cp-ha-ovn-<db>-<i>, through the unixctl socket of its ovsdb-server.
ovn_cluster_status() {
  local schema
  case "$1" in
    nb) schema=OVN_Northbound ;;
    sb) schema=OVN_Southbound ;;
    *)
      echo "ERROR: unknown OVN database '$1', want nb or sb" >&2
      return 2
      ;;
  esac
  kubectl exec "${LF_OVN}-$1-$2" -n "$NAMESPACE" -c ovsdb -- \
    ovs-appctl -t "/var/run/ovn/ovn$1_db.ctl" cluster/status "$schema"
}

# ovn_role <nb|sb> <i>: the Role: value of member <i>. Returns 1 when the
# exec fails.
ovn_role() {
  local out
  out="$(ovn_cluster_status "$1" "$2" 2>&1)" || return 1
  sed -n 's/^Role: //p' <<<"$out" | head -n 1
}

# ovn_leader <nb|sb>: the one member pod reporting Role: leader. On zero or
# more than one leader it prints every member's role to stderr and returns 1.
ovn_leader() {
  local db="$1" i pod role count=0 leader="" roles=""
  for i in $LF_MEMBERS; do
    pod="${LF_OVN}-${db}-${i}"
    role="$(ovn_role "$db" "$i")" || role="<unreachable>"
    roles="${roles} ${pod}=${role:-<none>}"
    if [ "$role" = "leader" ]; then
      count=$((count + 1))
      leader="$pod"
    fi
  done
  if [ "$count" -eq 1 ]; then
    echo "$leader"
    return 0
  fi
  echo "ERROR: ${db} has ${count} leaders:${roles}" >&2
  return 1
}

# ovn_follower <nb|sb> [leader]: the lowest-index member pod that is not
# <leader>, or not the one ovn_leader names when no leader is given. Returns 1
# when ovn_leader finds no single leader. A caller that targets the leader as
# well passes the pod it read, so a leader change between two reads cannot
# make the follower the same pod.
ovn_follower() {
  local leader="${2:-}" i
  if [ -z "$leader" ]; then
    leader="$(ovn_leader "$1")" || return 1
  fi
  for i in $LF_MEMBERS; do
    if [ "${LF_OVN}-$1-${i}" != "$leader" ]; then
      echo "${LF_OVN}-$1-${i}"
      return 0
    fi
  done
}

# ovn_wait_healthy <nb|sb> <secs>: the LF_MEMBER_COUNT members report
# `Status: cluster member` and exactly one reports `Role: leader`. Prints
# RAFT-OK <db> leader=<pod> <elapsed>s.
ovn_wait_healthy() {
  local db="$1" secs="$2" start deadline i pod out status role members leaders leader last
  start="$(date +%s)"
  deadline=$((start + secs))
  while true; do
    members=0
    leaders=0
    leader=""
    last=""
    for i in $LF_MEMBERS; do
      pod="${LF_OVN}-${db}-${i}"
      if out="$(ovn_cluster_status "$db" "$i" 2>&1)"; then
        status="$(sed -n 's/^Status: //p' <<<"$out" | head -n 1)"
        role="$(sed -n 's/^Role: //p' <<<"$out" | head -n 1)"
      else
        status="unreachable"
        role=""
      fi
      if [ "$status" = "cluster member" ]; then members=$((members + 1)); fi
      if [ "$role" = "leader" ]; then
        leaders=$((leaders + 1))
        leader="$pod"
      fi
      last="${last} ${pod}=${status:-<none>}/${role:-<none>}"
    done
    if [ "$members" -eq "$LF_MEMBER_COUNT" ] && [ "$leaders" -eq 1 ]; then
      echo "RAFT-OK ${db} leader=${leader} $(lf_since "$start")s"
      return 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "ERROR: the ${db} Raft cluster is not healthy within ${secs}s (${members} of ${LF_MEMBER_COUNT} members, ${leaders} leaders):${last}"
      return 1
    fi
    sleep "$LF_POLL_INTERVAL"
  done
}

# ovn_nbctl_sync <pod> <secs>: `ovn-nbctl --wait=sb sync` in the Northbound
# member <pod>, bounded by its own --timeout of <secs>. It returns once northd
# has carried the Northbound database into the Southbound one.
ovn_nbctl_sync() {
  kubectl exec "$1" -n "$NAMESPACE" -c ovsdb -- \
    ovn-nbctl --db=unix:/var/run/ovn/ovnnb_db.sock --timeout="$2" --wait=sb sync
}

# ovn_wait_northd_sync <secs>: ovn_nbctl_sync in the Northbound leader
# returns. Prints NORTHD-SYNC-OK <elapsed>s.
ovn_wait_northd_sync() {
  local secs="$1" start deadline leader out left
  start="$(date +%s)"
  deadline=$((start + secs))
  while true; do
    left=$((deadline - $(date +%s)))
    if [ "$left" -lt 1 ]; then left=1; fi
    if leader="$(ovn_leader nb 2>&1)" && out="$(ovn_nbctl_sync "$leader" "$left" 2>&1)"; then
      echo "NORTHD-SYNC-OK $(lf_since "$start")s"
      return 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "ERROR: ovn-nbctl --wait=sb sync did not return within ${secs}s: $(lf_first_line "${out:-$leader}")"
      return 1
    fi
    sleep "$LF_POLL_INTERVAL"
  done
}

# lf_until <secs> <description> <command...>: run the command every
# LF_POLL_INTERVAL seconds until it succeeds, for at most <secs>. Prints
# `OK: <description> after <elapsed>s` and the command's output, or an ERROR
# with the output of its last run.
lf_until() {
  local secs="$1" what="$2" start deadline out
  shift 2
  start="$(date +%s)"
  deadline=$((start + secs))
  while true; do
    if out="$("$@" 2>&1)"; then
      echo "OK: ${what} after $(lf_since "$start")s"
      if [ -n "$out" ]; then printf '%s\n' "$out"; fi
      return 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "ERROR: ${what} did not hold within ${secs}s; the last check printed:"
      printf '%s\n' "$out"
      return 1
    fi
    sleep "$LF_POLL_INTERVAL"
  done
}

# lf_wait_expired <kind> <name> <secs> [command...]: wait until the chaos
# object reads .status.experiment.desiredPhase Stop, which Chaos Mesh sets
# once its duration has passed. With a command, the command must succeed at
# every poll until then; its output is printed per sample.
lf_wait_expired() {
  local kind="$1" name="$2" secs="$3" start deadline phase out samples=0
  shift 3
  start="$(date +%s)"
  deadline=$((start + secs))
  while true; do
    phase="$(kubectl get "$kind" "$name" -n "$NAMESPACE" -o jsonpath='{.status.experiment.desiredPhase}' 2>/dev/null)" || phase=""
    if [ "$phase" = "Stop" ]; then
      echo "OK: ${kind}/${name} ran out its duration after $(lf_since "$start")s (${samples} samples)"
      return 0
    fi
    if [ $# -gt 0 ]; then
      if ! out="$("$@" 2>&1)"; then
        echo "ERROR: '$*' failed while ${kind}/${name} ran:"
        printf '%s\n' "$out"
        return 1
      fi
      samples=$((samples + 1))
      echo "sample ${samples} at $(lf_since "$start")s: $(lf_first_line "$out")"
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "ERROR: ${kind}/${name} did not reach desiredPhase Stop within ${secs}s (last: '${phase:-<none>}')"
      return 1
    fi
    sleep "$LF_POLL_INTERVAL"
  done
}

# lf_delete_chaos <kind> <name>: delete a chaos object and wait for it to go,
# which is when Chaos Mesh has recovered every pod it selected. Absent is
# fine, so catch and finally blocks call it too.
lf_delete_chaos() {
  kubectl delete "$1" "$2" -n "$NAMESPACE" --ignore-not-found --wait --timeout="${LF_CHAOS_DELETE_WAIT}s"
}

# lf_wait_recreated <pod> <uid> <secs>: the StatefulSet has replaced the pod
# <pod> of UID <uid> with a Running pod of the same name.
lf_wait_recreated() {
  local pod="$1" old="$2" secs="$3" start deadline uid phase
  start="$(date +%s)"
  deadline=$((start + secs))
  while true; do
    uid="$(pod_uids "$NAMESPACE" "statefulset.kubernetes.io/pod-name=${pod}" 2>/dev/null)" || uid=""
    phase="$(kubectl get pod "$pod" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null)" || phase=""
    if [ -n "$uid" ] && [ "$uid" != "$old" ] && [ "$phase" = "Running" ]; then
      echo "OK: ${pod} is Running again as ${uid} after $(lf_since "$start")s"
      return 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "ERROR: ${pod} is not Running with a new UID within ${secs}s (UID '${uid:-<none>}', phase '${phase:-<none>}')"
      return 1
    fi
    sleep "$LF_POLL_INTERVAL"
  done
}

# lf_ready_endpoints <service>: the ready endpoints of the Service's
# EndpointSlices.
lf_ready_endpoints() {
  kubectl get endpointslices -n "$NAMESPACE" -l "kubernetes.io/service-name=$1" -o json |
    jq '[.items[].endpoints[]? | select(.conditions.ready == true)] | length'
}

# lf_evict_drain <svc> <secs>: evict the LF_MEMBER_COUNT API pods of service
# <svc> through the pods/eviction subresource, so PDB cp-ha-<svc> sets the
# pace, and wait for as many new Ready pods.
#
# The pods are those the PDB's selector matches (matchLabels and
# matchExpressions). An eviction the API server answers with the PDB's
# "disruption budget" message is a refusal, and any other TooManyRequests, such
# as API Priority and Fairness throttling, is no refusal; both are retried
# after LF_EVICT_RETRY seconds. NotFound counts the pod as gone; any other
# error returns 1 with kubectl's message. Every LF_DRAIN_SAMPLE seconds the
# ready endpoints of Service cp-ha-<svc> are sampled. The drain is done when
# none of the original UIDs is left and LF_MEMBER_COUNT new pods are Ready.
# Prints DRAIN-OK <svc> refused=<n> min-ready=<m> <elapsed>s, and returns 1
# when no eviction was refused or the Service had no ready endpoint.
lf_evict_drain() {
  local svc="$1" secs="$2" name="${LF_CP}-$1" pdb sel pods originals pending pod uid
  local start deadline next_try=0 refused=0 min_ready="" ready out left fresh now
  if ! pdb="$(kubectl get pdb "$name" -n "$NAMESPACE" -o json 2>&1)"; then
    echo "ERROR: no PDB ${name}: $(lf_first_line "$pdb")"
    return 1
  fi
  sel="$(jq -r '.spec.selector as $s
    | [($s.matchLabels // {} | to_entries[] | "\(.key)=\(.value)"),
       (($s.matchExpressions // [])[]
         | if .operator == "In" then "\(.key) in (\(.values | join(",")))"
           elif .operator == "NotIn" then "\(.key) notin (\(.values | join(",")))"
           elif .operator == "Exists" then .key
           else "!\(.key)" end)]
    | join(",")' <<<"$pdb")" || sel=""
  if [ -z "$sel" ]; then
    echo "ERROR: PDB ${name} has no selector"
    return 1
  fi
  if ! pods="$(kubectl get pods -n "$NAMESPACE" -l "$sel" -o json 2>&1)"; then
    echo "ERROR: cannot list the pods of PDB ${name}: $(lf_first_line "$pods")"
    return 1
  fi
  originals="$(jq -r '.items[] | select(.metadata.deletionTimestamp == null)
    | "\(.metadata.name) \(.metadata.uid)"' <<<"$pods")" || originals=""
  if [ "$(grep -c . <<<"$originals")" -ne "$LF_MEMBER_COUNT" ]; then
    echo "ERROR: PDB ${name} selects $(grep -c . <<<"$originals") pods, want ${LF_MEMBER_COUNT}"
    return 1
  fi
  pending="$(awk '{ print $1 }' <<<"$originals" | xargs)"
  echo "drain ${svc}: evicting ${pending} (selector ${sel})"
  start="$(date +%s)"
  deadline=$((start + secs))
  while true; do
    # A failed read is no sample, and is logged: it could hide a moment
    # without a ready endpoint.
    ready="$(lf_ready_endpoints "$name" 2>&1)" || true
    case "$ready" in
      "" | *[!0-9]*) echo "drain ${svc}: no endpoint sample at $(lf_since "$start")s: $(lf_first_line "${ready:-empty}")" ;;
      *) if [ -z "$min_ready" ] || [ "$ready" -lt "$min_ready" ]; then min_ready="$ready"; fi ;;
    esac
    now="$(date +%s)"
    if [ -n "$pending" ] && [ "$now" -ge "$next_try" ]; then
      left=""
      for pod in $pending; do
        if out="$(printf '{"apiVersion":"policy/v1","kind":"Eviction","metadata":{"name":"%s","namespace":"%s"}}' \
          "$pod" "$NAMESPACE" | kubectl create --raw "/api/v1/namespaces/${NAMESPACE}/pods/${pod}/eviction" -f - 2>&1)"; then
          echo "drain ${svc}: ${pod} evicted at $(lf_since "$start")s"
        elif grep -q 'disruption budget' <<<"$out"; then
          refused=$((refused + 1))
          echo "drain ${svc}: ${pod} refused at $(lf_since "$start")s"
          left="${left} ${pod}"
          next_try=$((now + LF_EVICT_RETRY))
        elif grep -q 'TooManyRequests' <<<"$out"; then
          echo "drain ${svc}: ${pod} throttled at $(lf_since "$start")s: $(lf_first_line "$out")"
          left="${left} ${pod}"
          next_try=$((now + LF_EVICT_RETRY))
        elif grep -q 'NotFound' <<<"$out"; then
          echo "drain ${svc}: ${pod} is gone"
        else
          echo "ERROR: the eviction of ${pod} failed: ${out}"
          return 1
        fi
      done
      pending="$(xargs <<<"$left")"
    fi
    if [ -z "$pending" ] && pods="$(kubectl get pods -n "$NAMESPACE" -l "$sel" -o json 2>/dev/null)"; then
      fresh="$(jq -r --arg old "$(awk '{ print $2 }' <<<"$originals" | xargs)" '
        ($old | split(" ")) as $o
        | if any(.items[]; .metadata.uid | IN($o[])) then "old"
          else [.items[] | select(.metadata.deletionTimestamp == null)
            | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))] | length
          end' <<<"$pods")" || fresh=""
      if [ "$fresh" != "old" ] && [ -n "$fresh" ] && [ "$fresh" -ge "$LF_MEMBER_COUNT" ]; then
        echo "DRAIN-OK ${svc} refused=${refused} min-ready=${min_ready:-0} $(lf_since "$start")s"
        if [ "$refused" -eq 0 ]; then
          echo "ERROR: drain ${svc}: refused=0, PDB ${name} never refused an eviction"
          return 1
        fi
        if [ "${min_ready:-0}" -eq 0 ]; then
          echo "ERROR: drain ${svc}: min-ready=0, Service ${name} had no ready endpoint"
          return 1
        fi
        return 0
      fi
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "ERROR: drain ${svc} not done within ${secs}s (evictions pending: ${pending:-none}; refused=${refused}, min-ready=${min_ready:-<none>})"
      kubectl get pods -n "$NAMESPACE" -l "$sel" -o wide 2>&1 || true
      return 1
    fi
    sleep "$LF_DRAIN_SAMPLE"
  done
}

# lf_render <file> [KEY=VALUE...]: the file with every @KEY@ replaced by
# VALUE. Returns 1 naming the placeholder when an @[A-Z_]+@ is left.
lf_render() {
  local file="$1" out pair left
  shift
  out="$(cat "$file")" || return 1
  # awk replaces the literal text: ENVIRON keeps a value's backslashes, and
  # index() reads no pattern characters.
  for pair in "$@"; do
    out="$(LF_KEY="@${pair%%=*}@" LF_VALUE="${pair#*=}" awk '{
      line = ""
      while ((i = index($0, ENVIRON["LF_KEY"])) > 0) {
        line = line substr($0, 1, i - 1) ENVIRON["LF_VALUE"]
        $0 = substr($0, i + length(ENVIRON["LF_KEY"]))
      }
      print line $0
    }' <<<"$out")"
  done
  left="$(grep -oE '@[A-Z_]+@' <<<"$out")" || left=""
  left="${left%%$'\n'*}"
  if [ -n "$left" ]; then
    echo "ERROR: ${file}: no value for ${left}" >&2
    return 1
  fi
  printf '%s\n' "$out"
}

# lf_baseline: the plane and its three clusters are healthy before a fault, so
# a scenario that left damage fails the next suite here.
lf_baseline() {
  lf_wait_cp_ready "$LF_BASELINE_WAIT" || return 1
  galera_wait_healthy "$LF_BASELINE_WAIT" || return 1
  rabbit_wait_healthy "$LF_BASELINE_WAIT" || return 1
  ovn_wait_healthy nb "$LF_BASELINE_WAIT" || return 1
  ovn_wait_healthy sb "$LF_BASELINE_WAIT"
}

# lf_workload_start <id> <streams>: start the workload of the harness with the
# comma-separated <streams> against the plane's in-cluster endpoints, as the
# plane's admin. A stream reads only the endpoints it needs; the server stream
# boots the seed image on the seed flavor.
lf_workload_start() {
  local svc="${NAMESPACE}.svc"
  CHAOS_WORKLOAD_ADMIN_SECRET="${LF_CP}-keystone-admin-credentials" \
    "$LF_WORKLOAD" start "$1" -- --streams "$2" \
    --keystone "http://${LF_CP}-keystone.${svc}:5000/v3" \
    --http "http://${LF_CP}-horizon.${svc}:8080/auth/login/" \
    --glance "http://${LF_CP}-glance.${svc}:9292" \
    --placement "http://${LF_CP}-placement.${svc}:8778" \
    --barbican "http://${LF_CP}-barbican.${svc}:9311" \
    --neutron "http://${LF_CP}-neutron.${svc}:9696" \
    --cinder "http://${LF_CP}-cinder.${svc}:8776" \
    --nova "http://${LF_CP}-nova.${svc}:8774/v2.1" \
    --image-ref "$LF_SEED_IMAGE" --flavor "$LF_SEED_FLAVOR"
}

# lf_cluster_status: every Galera, broker and Raft status and the partition
# handling, for lf_dump and the topology.txt of hack/lab-failover.sh up.
lf_cluster_status() {
  local i db
  echo "=== Galera members ==="
  for i in $LF_MEMBERS; do
    echo "${LF_DB}-${i}: $(galera_member_status "${LF_DB}-${i}" 2>&1 || true)"
  done
  echo "=== broker nodes ==="
  for i in $LF_MEMBERS; do
    echo "${LF_BROKER}-server-${i}: $(rabbit_cluster "${LF_BROKER}-server-${i}" 2>&1 || true)"
  done
  echo "quorum queues: $(rabbit_quorum_queues 2>&1 || true)"
  echo "cluster_partition_handling: $(rabbit_partition_handling 2>&1 || true)"
  echo "=== Raft members ==="
  for db in nb sb; do
    for i in $LF_MEMBERS; do
      echo "${LF_OVN}-${db}-${i}: $(ovn_role "$db" "$i" 2>&1 || echo '<unreachable>')"
    done
  done
}

# lf_dump: the evidence of a failed step. Every command is `|| true`: a dump
# must not mask the step's own failure.
lf_dump() {
  local res
  cp_dump "$LF_CP" 2>&1 || true
  echo "=== pods of ${NAMESPACE} ==="
  kubectl get pods -n "$NAMESPACE" -o wide 2>&1 || true
  echo "=== events of ${NAMESPACE} ==="
  kubectl get events -n "$NAMESPACE" --sort-by=.lastTimestamp 2>&1 || true
  for res in "mariadb/${LF_DB}" "rabbitmqcluster/${LF_BROKER}" "ovncentral/${LF_OVN}"; do
    echo "=== ${res} status ==="
    kubectl get "$res" -n "$NAMESPACE" -o jsonpath='{.status}' 2>&1 || true
    echo
  done
  lf_cluster_status
  echo "=== mariadb-operator ==="
  kubectl logs -n mariadb-system -l app.kubernetes.io/name=mariadb-operator --tail=150 --all-containers 2>&1 || true
  echo "=== rabbitmq-cluster-operator ==="
  kubectl logs -n rabbitmq-system -l app.kubernetes.io/name=rabbitmq-cluster-operator --tail=150 --all-containers 2>&1 || true
  echo "=== ovn-operator ==="
  kubectl logs -n ovn-system -l app.kubernetes.io/name=ovn-operator --tail=150 --all-containers 2>&1 || true
}
