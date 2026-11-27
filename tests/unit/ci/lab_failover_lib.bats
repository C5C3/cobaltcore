#!/usr/bin/env bats
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the library of the lab failover suites
# (tests/lab-failover/lib/failover.sh, #1393) against a stub kubectl:
#   1. galera_wait_healthy passes on three `size=3 status=Primary
#      state=Synced ready=ON` members with Ready and GaleraReady True, and
#      fails with an ERROR naming the last status of a member stuck at
#      non-Primary, or with a condition that is not True.
#   2. galera_member_status reads a failed exec as `unreachable` and returns
#      1, and galera_wait_healthy takes that for "not yet": it passes once the
#      member answers, under set -euo pipefail.
#   3. rabbit_wait_healthy passes on `running=3 partitions=0 alarms=0` with
#      every quorum queue's online equal to its members, also behind a line
#      rabbitmqctl prints before its JSON; it fails on a non-empty partitions
#      map and on a queue with an offline member. An empty queue list passes,
#      and the count of quorum queues with fewer than 3 members prints as 0,
#      or as the count, which is no failure. A reply without JSON and
#      malformed JSON read as `unreadable` and return 1, and the broker is not
#      healthy then.
#   4. ovn_leader prints the one member reporting Role: leader and returns 1
#      printing every role on zero and on two leaders; ovn_role returns 1 on a
#      failed exec; ovn_follower names the lowest-index member that is no
#      leader, and with a leader given it reads no role and never names that
#      pod; ovn_nbctl_sync syncs in the given pod within its timeout;
#      ovn_wait_healthy and ovn_wait_northd_sync pass on a healthy cluster and
#      fail at their deadline.
#   5. lf_render replaces every given key and returns 1 naming @POD@ when it
#      is not given.
#   6. lf_evict_drain prints DRAIN-OK with refused=1 min-ready=1 when the
#      third eviction is refused by the disruption budget once; it logs a
#      failed endpoint read and takes no sample from it; it counts a NotFound
#      answer as a gone pod, and a throttled TooManyRequests as no refusal; it
#      returns 1 with kubectl's message on Forbidden, with `no PDB
#      cp-ha-<svc>`, with `selects 0 pods, want 3`, with refused=0 when no
#      eviction is refused, and with min-ready=0 when the Service had no ready
#      endpoint. It lists the pods with both the matchLabels and the
#      matchExpressions of the PDB, and is not done while an original UID is
#      still listed or a new pod is not Ready.
#   7. lf_wait_cp_ready refuses a Ready that is not AllReady; lf_until runs its
#      check until it holds and fails at its deadline with the last output;
#      lf_wait_expired waits for desiredPhase Stop with its check holding at
#      every sample, and fails when its check fails.
#   8. lf_wait_recreated waits for a new UID that is Running, and fails at its
#      deadline on the old UID.
#   9. lf_workload_start starts the harness with the id, the streams, the
#      plane's endpoints, its admin Secret and the seed flavor and image.
#  10. The library parses, and sourcing it calls nothing.
#
# Usage: bin/bats tests/unit/ci/lab_failover_lib.bats

load '../../lib/bats/common'
load '../../lib/bats/stubs'

LIB="$PROJECT_ROOT/tests/lab-failover/lib/failover.sh"

setup() {
  TMP_DIR="$BATS_TEST_TMPDIR"
  STUB="$TMP_DIR/stub"
  mkdir -p "$STUB"
  default_rules
}

# The stub kubectl answers by rules; like kubectl, it prints the keys of a
# map sorted. default_rules declares the answers of a healthy plane, and
# every call no rule names prints nothing and exits 0:
#   exec openstack-db-<i> ... -c mariadb -- sh -c ...
#                         galera_reply "<size> <status> <state> <ready>"
#                         (default 3 Primary Synced ON)
#   get mariadb openstack-db, Ready and GaleraReady (default True)
#   exec_fail_rule <pod> [<n>]
#                         the exec in <pod> fails, the first <n> times
#                         (default every time)
#   exec ... rabbitmqctl cluster_status
#                         cluster_status JSON of openstack-rabbitmq-server-<i>
#                         (default three running nodes, nothing else)
#   queues_rule <JSON>    list_queues JSON (default one healthy quorum queue)
#   role_rule <db> <i> <role>
#                         the Raft role of cp-ha-ovn-<db>-<i> (default leader
#                         for member 0, follower otherwise)
#   get pdb cp-ha-<svc>   the PDB's JSON (default keystone's shape)
#   pods_rule <n> <seq>   how many API pods the selector matches (default 3),
#                         and per evicted pod what each pod list shows for it
#                         in order: terminating (the original with a
#                         deletionTimestamp beside a Ready new pod), notready
#                         or ready (a new pod; default ready)
#   evict_rule <answer>   the answer to an eviction: ok, 429 (the disruption
#                         budget), throttled, notfound or forbidden (default
#                         ok every time)
#   endpoints_rule <n>    the ready endpoint count, fail for a read that fails
#                         (default 3)
#   get controlplane cp-ha
#                         Ready True with the reason AllReady
#   phase_rule <phase>    desiredPhase of a chaos object (default Stop)
#   uid_rule <uid>        the UID of a StatefulSet pod (default new)
#   pod_phase_rule <phase>
#                         the phase of that pod (default Running)
# seq_rules <rule> <a,b,...> declares the answers of a sequence, the last one
# repeated. A test declares what it changes before run_lib, and run_lib
# declares the defaults again after the run. The stub workload.sh appends its
# argv and CHAOS_WORKLOAD_ADMIN_SECRET to the calls.
# The defaults are plain strings, read into the rules as they are.
cluster_default='{"running_nodes":["rabbit@a","rabbit@b","rabbit@c"],"partitions":{},"alarms":[]}'
queues_default='[{"name":"q1","type":"quorum","members":["a","b","c"],"online":["a","b","c"]},{"name":"c1","type":"classic","members":"","online":""}]'
pdb_default='{"spec":{"minAvailable":1,"selector":{"matchLabels":{"app.kubernetes.io/instance":"cp-ha-keystone","app.kubernetes.io/name":"keystone"},"matchExpressions":[{"key":"batch.kubernetes.io/job-name","operator":"DoesNotExist"}]}}}'

# The functions next, pod, pod_list and evict run in the stub, as parts of
# an --exec body, with state set to $STUB_DIR/state.
# next <name> <comma list>: the next item of the list, the last one repeated.
next() {
  local n=0 item last=""
  [ -f "$state/$1" ] && n="$(cat "$state/$1")"
  echo $((n + 1)) >"$state/$1"
  IFS=, read -ra items <<<"$2"
  item="${items[$n]:-${items[${#items[@]}-1]}}"
  echo "$item"
}
# pod <name> <uid> <Ready status> [deletionTimestamp]: one item of a pod list.
pod() {
  printf '{"metadata":{"name":"%s","uid":"%s"%s},"status":{"conditions":[{"type":"Ready","status":"%s"}]}}' \
    "$1" "$2" "${4:+,\"deletionTimestamp\":\"$4\"}" "$3"
}
# pod_list <count> <replace seq>: the pod list of the API pods.
pod_list() {
  local items="" x item
  for x in $(seq 1 "$1"); do
    if ! grep -qx "api-$x" "$state/evicted" 2>/dev/null; then
      item="$(pod "api-$x" "uid-$x" True)"
    else
      case "$(next "replace-$x" "$2")" in
        terminating) item="$(pod "api-$x" "uid-$x" True 2026-10-19T12:00:00Z),$(pod "api-$x-new" "new-$x" True)" ;;
        notready) item="$(pod "api-$x-new" "new-$x" False)" ;;
        *) item="$(pod "api-$x-new" "new-$x" True)" ;;
      esac
    fi
    items="${items:+$items,}$item"
  done
  echo "{\"items\":[$items]}"
}
# evict <answer> <call arguments...>: the answer to the eviction the call
# posts on stdin, which it appends to evictions.
evict() {
  local answer="$1" pod
  shift
  cat >>"$state/evictions"
  echo >>"$state/evictions"
  pod="$(sed -E 's|.*/pods/([^/]+)/eviction|\1|' <<<"$3")"
  case "$answer" in
    ok) echo "$pod" >>"$state/evicted" ;;
    429)
      echo 'Error from server (TooManyRequests): Cannot evict pod as it would violate the pod'"'"'s disruption budget.' >&2
      exit 1
      ;;
    throttled)
      echo 'Error from server (TooManyRequests): the server has received too many requests and has asked us to try again later' >&2
      exit 1
      ;;
    notfound)
      echo "$pod" >>"$state/evicted"
      echo "Error from server (NotFound): pods \"$pod\" not found" >&2
      exit 1
      ;;
    forbidden)
      echo "Error from server (Forbidden): pods \"$pod\" is forbidden: User \"lab\" cannot create resource \"pods/eviction\"" >&2
      exit 1
      ;;
  esac
}

# galera_reply <size> <status> <state> <ready>: a member's answer to the
# status query.
galera_reply() {
  printf 'wsrep_cluster_size\t%s\nwsrep_cluster_status\t%s\nwsrep_local_state_comment\t%s\nwsrep_ready\t%s\n' \
    "$1" "$2" "$3" "$4"
}

# ovn_reply <role>: a Raft member's cluster/status.
ovn_reply() {
  printf 'Name: OVN\nStatus: cluster member\nRole: %s\nTerm: 2\n' "$1"
}

# The rules below take the options of stub_on after their own arguments, so
# seq_rules can pass --times 1.
exec_fail_rule() {
  stub_on kubectl "exec $1 *" --rc 1 \
    --err "error: unable to upgrade connection: container not found (\"$1\")" ${2:+--times "$2"}
}
queues_rule() {
  stub_on kubectl 'exec *rabbitmqctl list_queues -p / name type members online --formatter json*' --out "$1"
}
role_rule() {
  stub_on kubectl "exec cp-ha-ovn-$1-$2 *cluster/status*" --out "$(ovn_reply "$3")"
}
pods_rule() {
  stub_on kubectl 'get pods -n openstack-ha -l * -o json' \
    --exec "$(printf 'state="$STUB_DIR/state"\n'; declare -f next pod pod_list; printf 'pod_list %q %q\n' "$1" "$2")"
}
evict_rule() {
  stub_on kubectl 'create *' \
    --exec "$(printf 'state="$STUB_DIR/state"\n'; declare -f evict; printf 'evict %q "$@"\n' "$1")" "${@:2}"
}
endpoints_rule() {
  local count="$1"
  shift
  if [ "$count" = fail ]; then
    stub_on kubectl 'get endpointslices -n openstack-ha -l kubernetes.io/service-name=*' --rc 1 \
      --err 'Error from server (InternalError): the stub refuses the read' "$@"
  else
    stub_on kubectl 'get endpointslices -n openstack-ha -l kubernetes.io/service-name=*' \
      --out "{\"items\":[{\"endpoints\":[$(for _ in $(seq 1 "$count"); do printf '{"conditions":{"ready":true}},'; done){\"conditions\":{\"ready\":false}}]}]}" "$@"
  fi
}
phase_rule() {
  stub_on kubectl '*{.status.experiment.desiredPhase}' --out-raw "$@"
}
uid_rule() {
  stub_on kubectl 'get pods -n openstack-ha -l statefulset.kubernetes.io/pod-name=*metadata.uid}' --out-raw "$@"
}
pod_phase_rule() {
  stub_on kubectl 'get pod *{.status.phase}' --out-raw "$@"
}

# seq_rules <rule> <comma list>: the calls <rule> answers take the items of
# the list in order, the last one repeated. The last item is a lasting rule,
# and each item before it answers one call, declared after the ones it
# precedes so that it is newer.
seq_rules() {
  local rule="$1" items i
  IFS=, read -ra items <<<"$2"
  i=$((${#items[@]} - 1))
  "$rule" "${items[$i]}"
  while [ "$i" -gt 0 ]; do
    i=$((i - 1))
    "$rule" "${items[$i]}" --times 1
  done
}

default_rules() {
  stub_on kubectl '*'
  stub_on kubectl 'exec * -c mariadb -- sh -c *' --out "$(galera_reply 3 Primary Synced ON)"
  stub_on kubectl 'exec *rabbitmqctl cluster_status --formatter json*' --out "$cluster_default"
  queues_rule "$queues_default"
  stub_on kubectl 'exec *rabbitmqctl eval*' --out '{ok,pause_minority}'
  stub_on kubectl 'exec *cluster/status*' --out "$(ovn_reply follower)"
  stub_on kubectl 'exec cp-ha-ovn-[ns]b-0 *cluster/status*' --out "$(ovn_reply leader)"
  stub_on kubectl "get mariadb openstack-db *=='Ready')].status}" --out-raw True
  stub_on kubectl "get mariadb openstack-db *=='GaleraReady')].status}" --out-raw True
  stub_on kubectl 'get controlplane cp-ha *].status}' --out-raw True
  stub_on kubectl 'get controlplane cp-ha *].reason}' --out-raw AllReady
  phase_rule Stop
  uid_rule new
  pod_phase_rule Running
  stub_on kubectl 'get pdb cp-ha-*' --out "$pdb_default"
  pods_rule 3 ready
  endpoints_rule 3
  evict_rule ok
  stub_script workload.sh <<'SH'
echo "workload.sh CHAOS_WORKLOAD_ADMIN_SECRET=${CHAOS_WORKLOAD_ADMIN_SECRET:-} $*" >>"$STUB_STATE/calls"
SH
}

# run_lib <VAR=value...> -- <shell code>: run code under set -euo pipefail
# with the library sourced, the stub first on PATH, NAMESPACE=openstack-ha and
# every poll interval 0; LF_OUT and LF_RC hold the result. The calls and the
# state of the run stay in $STUB/state, and the rules go back to the
# defaults, so a rule a test declares before run_lib answers that run alone.
run_lib() {
  local envs=()
  while [ "$1" != "--" ]; do
    envs+=("$1")
    shift
  done
  shift
  rm -rf "$STUB/state"
  LF_RC=0
  LF_OUT="$(env STUB_STATE="$STUB_DIR/state" NAMESPACE=openstack-ha PATH="$STUB_DIR/bin:$PATH" \
    ${envs[@]+"${envs[@]}"} bash -c "set -euo pipefail; . '$LIB'
      LF_POLL_INTERVAL=0 LF_DRAIN_SAMPLE=0 LF_EVICT_RETRY=0; $1" 2>&1)" || LF_RC=$?
  mv "$STUB_DIR/state" "$STUB/state"
  stub_calls kubectl >>"$STUB/state/calls"
  rm -rf "$STUB_DIR"
  default_rules
}

calls() {
  cat "$STUB/state/calls"
}

# --- Test 1 ---------------------------------------------------------------
@test "galera_wait_healthy passes on three synced Primary members and names a stuck one" {
  run_lib -- 'galera_wait_healthy 0'
  assert_eq "it returns 0 on a healthy cluster" "0" "$LF_RC"
  assert_contains "it prints GALERA-OK" "$LF_OUT" "GALERA-OK "
  assert_contains "it reads the members in the container mariadb" "$(calls)" \
    "exec openstack-db-2 -n openstack-ha -c mariadb -- sh -c"
  assert_not_contains "the root password stays out of the argv" "$(calls)" "-p\""

  stub_on kubectl 'exec openstack-db-2 * -c mariadb -- sh -c *' --out "$(galera_reply 1 non-Primary Initialized OFF)"
  run_lib -- 'galera_wait_healthy 0'
  assert_eq "it returns 1 on a member stuck at non-Primary" "1" "$LF_RC"
  assert_contains "it says so" "$LF_OUT" "ERROR: Galera openstack-db not healthy within 0s"
  assert_contains "it names the member's last status" "$LF_OUT" \
    "openstack-db-2: size=1 status=non-Primary state=Initialized ready=OFF"

  stub_on kubectl "get mariadb openstack-db *=='GaleraReady')].status}" --out-raw False
  run_lib -- 'galera_wait_healthy 0'
  assert_eq "it returns 1 when GaleraReady is False" "1" "$LF_RC"
  assert_contains "it names the condition" "$LF_OUT" "GaleraReady='False'"
}

# --- Test 2 ---------------------------------------------------------------
@test "a member whose exec fails is not healthy yet, and no abort" {
  exec_fail_rule openstack-db-1
  run_lib -- 'galera_member_status openstack-db-1'
  assert_eq "galera_member_status returns 1 on a failed exec" "1" "$LF_RC"
  assert_contains "it prints the error as unreachable" "$LF_OUT" "unreachable: error: unable to upgrade connection"

  exec_fail_rule openstack-db-1
  run_lib -- 'galera_wait_healthy 0; echo not-reached'
  assert_eq "galera_wait_healthy returns 1 at its deadline" "1" "$LF_RC"
  assert_contains "it names the unreachable member" "$LF_OUT" "openstack-db-1: unreachable:"

  exec_fail_rule openstack-db-1 2
  run_lib -- 'galera_wait_healthy 60; echo after'
  assert_eq "it passes once the member answers again" "0" "$LF_RC"
  assert_contains "it went on polling" "$LF_OUT" "GALERA-OK"
  assert_contains "the caller goes on under set -e" "$LF_OUT" "after"
  assert_eq "it read the member three times" "3" "$(grep -c '^exec openstack-db-1 ' "$STUB/state/calls")"
}

# --- Test 3 ---------------------------------------------------------------
@test "rabbit_wait_healthy passes on three running nodes with online quorum queues" {
  run_lib -- 'rabbit_wait_healthy 0'
  assert_eq "it returns 0 on a healthy broker" "0" "$LF_RC"
  assert_contains "it prints BROKER-OK" "$LF_OUT" "BROKER-OK "
  assert_contains "with the count of small quorum queues" "$LF_OUT" "s quorum-queues-under-3-members=0"

  stub_on kubectl 'exec *rabbitmqctl cluster_status --formatter json*' \
    --out "Cluster status of node rabbit@a ..."$'\n'"$cluster_default"
  run_lib -- 'rabbit_cluster openstack-rabbitmq-server-0'
  assert_eq "a line before the JSON is dropped" "running=3 partitions=0 alarms=0" "$LF_OUT"

  stub_on kubectl 'exec openstack-rabbitmq-server-1 *rabbitmqctl cluster_status --formatter json*' \
    --out '{"running_nodes":["a","b","c"],"partitions":{"rabbit@a":["rabbit@b"]},"alarms":[]}'
  run_lib -- 'rabbit_wait_healthy 0'
  assert_eq "it returns 1 on a non-empty partitions map" "1" "$LF_RC"
  assert_contains "it names the node's view" "$LF_OUT" "openstack-rabbitmq-server-1: running=3 partitions=1 alarms=0"

  queues_rule '[{"name":"q1","type":"quorum","members":["a","b","c"],"online":["a","b"]}]'
  run_lib -- 'rabbit_wait_healthy 0'
  assert_eq "it returns 1 on a quorum queue with an offline member" "1" "$LF_RC"
  assert_contains "it names the queue" "$LF_OUT" "quorum queues: offline=q1 under3=0"

  queues_rule '[]'
  run_lib -- 'rabbit_wait_healthy 0'
  assert_eq "an empty queue list passes" "0" "$LF_RC"
  assert_contains "and counts 0 small quorum queues" "$LF_OUT" "quorum-queues-under-3-members=0"

  queues_rule '[{"name":"q1","type":"quorum","members":["a"],"online":["a"]}]'
  run_lib -- 'rabbit_wait_healthy 0'
  assert_eq "a quorum queue with one member is no failure" "0" "$LF_RC"
  assert_contains "it is counted" "$LF_OUT" "quorum-queues-under-3-members=1"

  exec_fail_rule openstack-rabbitmq-server-2
  run_lib -- 'rabbit_wait_healthy 0'
  assert_eq "a node whose exec fails is not healthy" "1" "$LF_RC"
  assert_contains "it is named unreachable" "$LF_OUT" "openstack-rabbitmq-server-2: unreachable:"

  queues_rule 'Listing queues for vhost / ...'
  run_lib -- 'rabbit_quorum_queues'
  assert_eq "a reply without JSON returns 1" "1" "$LF_RC"
  assert_eq "it reads as unreadable" "unreadable: Listing queues for vhost / ..." "$LF_OUT"
  queues_rule 'Listing queues for vhost / ...'
  run_lib -- 'rabbit_wait_healthy 0'
  assert_eq "and the broker is not healthy" "1" "$LF_RC"
  assert_contains "it names the reply" "$LF_OUT" "quorum queues: unreadable: Listing queues for vhost / ..."
  stub_on kubectl 'exec openstack-rabbitmq-server-0 *rabbitmqctl cluster_status --formatter json*' --out '{not json'
  run_lib -- 'rabbit_cluster openstack-rabbitmq-server-0'
  assert_eq "malformed JSON returns 1" "1" "$LF_RC"
  assert_eq "it reads as unreadable" "unreadable: {not json" "$LF_OUT"

  run_lib -- 'rabbit_partition_handling'
  assert_contains "rabbit_partition_handling asks server-1" "$(calls)" \
    "exec openstack-rabbitmq-server-1 -n openstack-ha -c rabbitmq -- rabbitmqctl eval application:get_env(rabbit, cluster_partition_handling)."
}

# --- Test 4 ---------------------------------------------------------------
@test "ovn_leader names one leader and refuses zero or two; the Raft waits" {
  role_rule nb 0 follower
  role_rule nb 1 leader
  run_lib -- 'ovn_leader nb'
  assert_eq "ovn_leader returns 0 on one leader" "0" "$LF_RC"
  assert_eq "it prints that pod alone" "cp-ha-ovn-nb-1" "$LF_OUT"
  assert_contains "it reads the role through the unixctl socket" "$(calls)" \
    "exec cp-ha-ovn-nb-1 -n openstack-ha -c ovsdb -- ovs-appctl -t /var/run/ovn/ovnnb_db.ctl cluster/status OVN_Northbound"

  role_rule sb 0 follower
  run_lib -- 'ovn_leader sb'
  assert_eq "ovn_leader returns 1 on zero leaders" "1" "$LF_RC"
  assert_contains "it prints every role" "$LF_OUT" \
    "ERROR: sb has 0 leaders: cp-ha-ovn-sb-0=follower cp-ha-ovn-sb-1=follower cp-ha-ovn-sb-2=follower"

  role_rule nb 2 leader
  run_lib -- 'ovn_leader nb'
  assert_eq "ovn_leader returns 1 on two leaders" "1" "$LF_RC"
  assert_contains "it prints every role" "$LF_OUT" \
    "ERROR: nb has 2 leaders: cp-ha-ovn-nb-0=leader cp-ha-ovn-nb-1=follower cp-ha-ovn-nb-2=leader"

  exec_fail_rule cp-ha-ovn-sb-2
  run_lib -- 'ovn_role sb 2'
  assert_eq "ovn_role returns 1 on a failed exec" "1" "$LF_RC"

  exec_fail_rule cp-ha-ovn-sb-2
  run_lib -- 'ovn_leader sb'
  assert_eq "ovn_leader still finds the leader beside a member that is down" "cp-ha-ovn-sb-0" "$LF_OUT"

  run_lib -- 'ovn_follower nb'
  assert_eq "ovn_follower skips the leader at index 0" "cp-ha-ovn-nb-1" "$LF_OUT"
  role_rule nb 0 follower
  role_rule nb 1 leader
  run_lib -- 'ovn_follower nb'
  assert_eq "ovn_follower takes index 0 when it is no leader" "cp-ha-ovn-nb-0" "$LF_OUT"
  role_rule nb 0 candidate
  run_lib -- 'ovn_follower nb'
  assert_eq "ovn_follower returns 1 without a leader" "1" "$LF_RC"
  role_rule nb 0 follower
  role_rule nb 1 leader
  run_lib -- 'ovn_follower nb cp-ha-ovn-nb-0'
  assert_eq "with a leader given it never names that pod, whoever leads now" "cp-ha-ovn-nb-1" "$LF_OUT"
  assert_eq "and it reads no role" "" "$(calls)"

  run_lib -- 'ovn_nbctl_sync cp-ha-ovn-nb-2 10'
  assert_eq "ovn_nbctl_sync returns 0 when the sync returns" "0" "$LF_RC"
  assert_eq "it syncs in the given pod within its timeout" \
    "exec cp-ha-ovn-nb-2 -n openstack-ha -c ovsdb -- ovn-nbctl --db=unix:/var/run/ovn/ovnnb_db.sock --timeout=10 --wait=sb sync" \
    "$(calls)"
  stub_on kubectl 'exec *ovn-nbctl*' --rc 1 --err 'ovn-nbctl: timeout expired'
  run_lib -- 'ovn_nbctl_sync cp-ha-ovn-nb-2 10'
  assert_eq "it returns 1 when the sync fails" "1" "$LF_RC"

  run_lib -- 'ovn_wait_healthy sb 0'
  assert_eq "ovn_wait_healthy returns 0 on three members and one leader" "0" "$LF_RC"
  assert_contains "it prints RAFT-OK with the leader" "$LF_OUT" "RAFT-OK sb leader=cp-ha-ovn-sb-0 "
  exec_fail_rule cp-ha-ovn-nb-1
  run_lib -- 'ovn_wait_healthy nb 0'
  assert_eq "ovn_wait_healthy returns 1 with a member down" "1" "$LF_RC"
  assert_contains "it names the member" "$LF_OUT" "(2 of 3 members, 1 leaders)"
  assert_contains "and its state" "$LF_OUT" "cp-ha-ovn-nb-1=unreachable/<none>"

  run_lib -- 'ovn_wait_northd_sync 0'
  assert_eq "ovn_wait_northd_sync returns 0 when the sync returns" "0" "$LF_RC"
  assert_contains "it prints NORTHD-SYNC-OK" "$LF_OUT" "NORTHD-SYNC-OK "
  assert_contains "it syncs in the Northbound leader" "$(calls)" \
    "exec cp-ha-ovn-nb-0 -n openstack-ha -c ovsdb -- ovn-nbctl --db=unix:/var/run/ovn/ovnnb_db.sock --timeout=1 --wait=sb sync"
  stub_on kubectl 'exec *ovn-nbctl*' --rc 1 --err 'ovn-nbctl: timeout expired'
  run_lib -- 'ovn_wait_northd_sync 0'
  assert_eq "ovn_wait_northd_sync returns 1 when the sync fails" "1" "$LF_RC"
  assert_contains "it names ovn-nbctl's message" "$LF_OUT" "ovn-nbctl: timeout expired"
}

# --- Test 5 ---------------------------------------------------------------
@test "lf_render fills every key and names a placeholder left over" {
  local manifest="$TMP_DIR/podchaos.yaml"
  printf 'pods:\n  openstack-ha: ["@POD_A@", "@POD@"]\n' >"$manifest"
  run_lib -- "lf_render '$manifest' POD=cp-ha-ovn-nb-0 'POD_A=a&b\\c'"
  assert_eq "lf_render returns 0 with every key" "0" "$LF_RC"
  assert_eq "it replaces each key literally" $'pods:\n  openstack-ha: ["a&b\\c", "cp-ha-ovn-nb-0"]' "$LF_OUT"
  run_lib -- "lf_render '$manifest' POD_A=x"
  assert_eq "lf_render returns 1 when @POD@ is not given" "1" "$LF_RC"
  assert_contains "it names @POD@" "$LF_OUT" "no value for @POD@"
  assert_not_contains "it prints no manifest" "$LF_OUT" "pods:"
}

# --- Test 6 ---------------------------------------------------------------
@test "lf_evict_drain lets the PDB set the pace and refuses a drain it did not see" {
  seq_rules evict_rule ok,ok,429,ok
  seq_rules endpoints_rule 3,1,3
  run_lib -- 'lf_evict_drain keystone 60'
  assert_eq "it returns 0 when the PDB refused once" "0" "$LF_RC"
  assert_contains "it prints DRAIN-OK" "$LF_OUT" "DRAIN-OK keystone refused=1 min-ready=1 "
  assert_contains "it reads the PDB of the plane's service" "$(calls)" "get pdb cp-ha-keystone -n openstack-ha -o json"
  assert_contains "it lists the pods with both the labels and the expression" "$(calls)" \
    "get pods -n openstack-ha -l app.kubernetes.io/instance=cp-ha-keystone,app.kubernetes.io/name=keystone,!batch.kubernetes.io/job-name -o json"
  assert_contains "it posts a policy/v1 Eviction" "$(cat "$STUB/state/evictions")" \
    '{"apiVersion":"policy/v1","kind":"Eviction","metadata":{"name":"api-3","namespace":"openstack-ha"}}'
  assert_contains "it posts to the eviction subresource" "$(calls)" \
    "create --raw /api/v1/namespaces/openstack-ha/pods/api-3/eviction -f -"
  assert_eq "it retried the refused pod" "2" "$(grep -c 'pods/api-3/eviction' "$STUB/state/calls")"
  assert_contains "it samples the Service's endpoints" "$(calls)" \
    "get endpointslices -n openstack-ha -l kubernetes.io/service-name=cp-ha-keystone -o json"

  seq_rules evict_rule ok,ok,429,ok
  seq_rules endpoints_rule fail,2
  run_lib -- 'lf_evict_drain keystone 60'
  assert_eq "a failed endpoint read is no sample" "0" "$LF_RC"
  assert_contains "it is logged" "$LF_OUT" "drain keystone: no endpoint sample at "
  assert_contains "with kubectl's message" "$LF_OUT" "s: Error from server (InternalError): the stub refuses the read"
  assert_contains "the samples that worked decide min-ready" "$LF_OUT" "DRAIN-OK keystone refused=1 min-ready=2 "

  seq_rules evict_rule ok,notfound,429,ok
  endpoints_rule 2
  run_lib -- 'lf_evict_drain keystone 60'
  assert_eq "a NotFound answer counts the pod as gone" "0" "$LF_RC"
  assert_contains "it says so" "$LF_OUT" "drain keystone: api-2 is gone"
  assert_eq "it does not evict a gone pod again" "1" "$(grep -c 'pods/api-2/eviction' "$STUB/state/calls")"

  seq_rules evict_rule ok,ok,throttled,ok
  run_lib -- 'lf_evict_drain keystone 60'
  assert_eq "a throttled eviction is no refusal" "1" "$LF_RC"
  assert_contains "it logs the throttle" "$LF_OUT" "drain keystone: api-3 throttled at "
  assert_contains "and counts no refusal" "$LF_OUT" "DRAIN-OK keystone refused=0 "
  assert_eq "it retried the throttled pod" "2" "$(grep -c 'pods/api-3/eviction' "$STUB/state/calls")"

  seq_rules evict_rule ok,ok,429,ok
  pods_rule 3 terminating,notready,ready
  run_lib -- 'lf_evict_drain keystone 60'
  assert_eq "it waits for the old UIDs to go and the new pods to turn Ready" "0" "$LF_RC"
  assert_contains "then it prints DRAIN-OK" "$LF_OUT" "DRAIN-OK keystone refused=1 "
  assert_eq "it listed the pods until all three were new and Ready" "4" \
    "$(grep -c '^get pods -n openstack-ha -l .* -o json$' "$STUB/state/calls")"
  evict_rule ok
  pods_rule 3 terminating
  run_lib -- 'lf_evict_drain keystone 0'
  assert_eq "an original UID still listed fails the drain at its deadline" "1" "$LF_RC"
  assert_not_contains "beside three Ready new pods it prints no DRAIN-OK" "$LF_OUT" "DRAIN-OK"
  assert_contains "with no eviction pending" "$LF_OUT" "evictions pending: none"
  evict_rule ok
  pods_rule 3 notready
  run_lib -- 'lf_evict_drain keystone 0'
  assert_eq "new pods that never turn Ready fail the drain" "1" "$LF_RC"
  assert_not_contains "no DRAIN-OK" "$LF_OUT" "DRAIN-OK"

  seq_rules evict_rule ok,forbidden
  run_lib -- 'lf_evict_drain keystone 60'
  assert_eq "it returns 1 on a Forbidden answer" "1" "$LF_RC"
  assert_contains "it prints kubectl's message" "$LF_OUT" \
    "ERROR: the eviction of api-2 failed: Error from server (Forbidden): pods \"api-2\" is forbidden"

  # shellcheck disable=SC2016 # $3 expands in the stub
  stub_on kubectl 'get pdb cp-ha-*' \
    --exec 'echo "Error from server (NotFound): poddisruptionbudgets.policy \"$3\" not found" >&2; exit 1'
  run_lib -- 'lf_evict_drain nova 60'
  assert_eq "it returns 1 without a PDB" "1" "$LF_RC"
  assert_contains "it names the PDB" "$LF_OUT" "ERROR: no PDB cp-ha-nova"
  assert_not_contains "it evicts nothing" "$(calls)" "eviction"

  pods_rule 0 ready
  run_lib -- 'lf_evict_drain glance 60'
  assert_eq "it returns 1 on an empty selection" "1" "$LF_RC"
  assert_contains "it names the count" "$LF_OUT" "ERROR: PDB cp-ha-glance selects 0 pods, want 3"
  pods_rule 2 ready
  run_lib -- 'lf_evict_drain glance 60'
  assert_contains "it refuses two pods as well" "$LF_OUT" "selects 2 pods, want 3"

  evict_rule ok
  run_lib -- 'lf_evict_drain placement 60'
  assert_eq "it returns 1 when no eviction was refused" "1" "$LF_RC"
  assert_contains "it still prints the drain" "$LF_OUT" "DRAIN-OK placement refused=0 min-ready=3"
  assert_contains "it names refused=0" "$LF_OUT" "ERROR: drain placement: refused=0"

  seq_rules evict_rule ok,ok,429,ok
  seq_rules endpoints_rule 3,0,2
  run_lib -- 'lf_evict_drain horizon 60'
  assert_eq "it returns 1 when the Service had no ready endpoint" "1" "$LF_RC"
  assert_contains "it names min-ready=0" "$LF_OUT" "ERROR: drain horizon: min-ready=0"

  evict_rule 429
  run_lib -- 'lf_evict_drain cinder 0'
  assert_eq "it returns 1 at its deadline" "1" "$LF_RC"
  assert_contains "it names the pods still to evict" "$LF_OUT" "evictions pending: api-1 api-2 api-3"
}

# --- Test 7 ---------------------------------------------------------------
@test "lf_wait_cp_ready wants AllReady; lf_wait_expired waits for Stop and holds its check" {
  run_lib -- 'lf_wait_cp_ready 0'
  assert_eq "lf_wait_cp_ready returns 0 on Ready/AllReady" "0" "$LF_RC"
  stub_on kubectl 'get controlplane cp-ha *].reason}' --out-raw NotAllReady
  run_lib -- 'lf_wait_cp_ready 0'
  assert_eq "it returns 1 on another reason" "1" "$LF_RC"
  assert_contains "it names the reason" "$LF_OUT" "reason 'NotAllReady', not AllReady"

  run_lib -- 'lf_wait_expired podchaos lf-x 0'
  assert_eq "lf_wait_expired returns 0 on Stop" "0" "$LF_RC"
  phase_rule Run
  run_lib -- 'lf_wait_expired podchaos lf-x 0'
  assert_eq "it returns 1 while the chaos still runs at its deadline" "1" "$LF_RC"
  assert_contains "it names the phase" "$LF_OUT" "(last: 'Run')"
  phase_rule Run
  run_lib -- 'lf_wait_expired podchaos lf-x 60 false'
  assert_eq "it returns 1 when its check fails" "1" "$LF_RC"
  assert_contains "it names the check" "$LF_OUT" "ERROR: 'false' failed while podchaos/lf-x ran"
  seq_rules phase_rule Run,Run,Stop
  run_lib -- 'lf_wait_expired podchaos lf-x 60 echo held'
  assert_eq "it returns 0 when its check held until Stop" "0" "$LF_RC"
  assert_eq "it prints each sample and counts them" \
    $'sample 1 at Ns: held\nsample 2 at Ns: held\nOK: podchaos/lf-x ran out its duration after Ns (2 samples)' \
    "$(sed -E 's/ [0-9]+s/ Ns/' <<<"$LF_OUT")"

  # The check runs in a subshell, so it counts its runs in a file.
  run_lib -- 'check() { echo x >>"$STUB_STATE/tries"; echo "try $(grep -c . "$STUB_STATE/tries")"; [ "$(grep -c . "$STUB_STATE/tries")" -ge 3 ]; }
    lf_until 60 three-tries check'
  assert_eq "lf_until returns 0 once its check holds" "0" "$LF_RC"
  assert_eq "it prints the OK line and the check's output" $'OK: three-tries after Ns\ntry 3' \
    "$(sed -E 's/ [0-9]+s/ Ns/' <<<"$LF_OUT")"
  assert_eq "it ran the check until then" "3" "$(grep -c . "$STUB/state/tries")"
  run_lib -- 'lf_until 0 never sh -c "echo nope; false"'
  assert_eq "it returns 1 at its deadline" "1" "$LF_RC"
  assert_eq "it names the check and its last output" $'ERROR: never did not hold within 0s; the last check printed:\nnope' "$LF_OUT"
}

# --- Test 8 ---------------------------------------------------------------
@test "lf_wait_recreated waits for a Running pod with a new UID" {
  seq_rules uid_rule old,new,new
  seq_rules pod_phase_rule Running,Pending,Running
  run_lib -- 'lf_wait_recreated cp-ha-ovn-nb-0 old 60'
  assert_eq "it returns 0 once a new UID is Running" "0" "$LF_RC"
  assert_contains "it names the new UID" "$LF_OUT" "OK: cp-ha-ovn-nb-0 is Running again as new after "
  assert_eq "it waited past the old UID and the Pending pod" "3" "$(grep -c '{.status.phase}$' "$STUB/state/calls")"
  assert_contains "it reads the UID by the StatefulSet's pod-name label" "$(calls)" \
    "get pods -n openstack-ha -l statefulset.kubernetes.io/pod-name=cp-ha-ovn-nb-0 -o jsonpath={.items[*].metadata.uid}"
  uid_rule old
  run_lib -- 'lf_wait_recreated cp-ha-ovn-nb-0 old 0'
  assert_eq "the old UID at the deadline fails" "1" "$LF_RC"
  assert_contains "it names the UID and the phase" "$LF_OUT" "(UID 'old', phase 'Running')"
}

# --- Test 9 ---------------------------------------------------------------
@test "lf_workload_start starts the harness against the plane as its admin" {
  run_lib -- "LF_WORKLOAD='$STUB_DIR/bin/workload.sh'; lf_workload_start galera-member-loss token,server"
  assert_eq "it returns 0" "0" "$LF_RC"
  assert_eq "it passes the id, the streams, the endpoints, the admin Secret and the seed" \
    "workload.sh CHAOS_WORKLOAD_ADMIN_SECRET=cp-ha-keystone-admin-credentials start galera-member-loss -- --streams token,server --keystone http://cp-ha-keystone.openstack-ha.svc:5000/v3 --http http://cp-ha-horizon.openstack-ha.svc:8080/auth/login/ --glance http://cp-ha-glance.openstack-ha.svc:9292 --placement http://cp-ha-placement.openstack-ha.svc:8778 --barbican http://cp-ha-barbican.openstack-ha.svc:9311 --neutron http://cp-ha-neutron.openstack-ha.svc:9696 --cinder http://cp-ha-cinder.openstack-ha.svc:8776 --nova http://cp-ha-nova.openstack-ha.svc:8774/v2.1 --image-ref 4e0a0000-0000-4000-8000-000000001393 --flavor m1.nano" \
    "$(calls)"
}

# --- Test 10 --------------------------------------------------------------
@test "the library parses, and sourcing it calls nothing" {
  assert_eq "bash -n passes" "0" "$(bash -n "$LIB" >/dev/null 2>&1 && echo 0 || echo 1)"
  run_lib -- 'true'
  assert_eq "sourcing it returns 0" "0" "$LF_RC"
  assert_eq "sourcing it calls no kubectl" "" "$(calls)"
}
