#!/bin/bash
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
# Usage: bash tests/unit/ci/lab_failover_lib_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
LIB="$PROJECT_ROOT/tests/lab-failover/lib/failover.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT
STUB="$TMP_DIR/stub"
mkdir -p "$STUB/bin"

# The stub kubectl appends every call to $STUB_STATE/calls and answers from
# the environment; like kubectl, it prints the keys of a map sorted:
#   STUB_GALERA_<i>       "<size> <status> <state> <ready>" of openstack-db-<i>
#                         (default "3 Primary Synced ON")
#   STUB_MARIADB_READY    Ready of mariadb/openstack-db (default True)
#   STUB_GALERA_READY     GaleraReady of mariadb/openstack-db (default True)
#   STUB_EXEC_FAIL        the pod whose exec fails, STUB_EXEC_FAIL_TIMES times
#                         (default every time)
#   STUB_RABBIT_<i>       cluster_status JSON of openstack-rabbitmq-server-<i>
#                         (default three running nodes, nothing else)
#   STUB_RABBIT_PREAMBLE  a line rabbitmqctl prints before its JSON
#   STUB_QUEUES           list_queues JSON (default one healthy quorum queue)
#   STUB_ROLE_<db>_<i>    the Raft role of cp-ha-ovn-<db>-<i> (default leader
#                         for member 0, follower otherwise)
#   STUB_SYNC_FAIL        non-empty: ovn-nbctl sync fails
#   STUB_PDB              absent, or the PDB's JSON (default keystone's shape)
#   STUB_PODS             how many API pods the selector matches (default 3)
#   STUB_EVICT_SEQ        the answers to the evictions in order: ok, 429 (the
#                         disruption budget), throttled, notfound or forbidden
#                         (default ok every time)
#   STUB_REPLACE_SEQ      per evicted pod, what each pod list shows for it in
#                         order: terminating (the original with a
#                         deletionTimestamp beside a Ready new pod), notready
#                         or ready (a new pod; default ready)
#   STUB_ENDPOINTS        the ready endpoint counts in order, the last one
#                         repeated, fail for a read that fails (default 3)
#   STUB_CP_REASON        the reason of the ControlPlane's Ready (AllReady)
#   STUB_PHASE            desiredPhase of a chaos object in order (default
#                         Stop)
#   STUB_UID_SEQ          the UIDs of a StatefulSet pod in order (default new)
#   STUB_POD_PHASE_SEQ    the phases of that pod in order (default Running)
# The stub workload.sh appends its argv and CHAOS_WORKLOAD_ADMIN_SECRET to the
# calls.
cat >"$STUB/bin/kubectl" <<'SH'
#!/bin/bash
state="$STUB_STATE"
echo "$*" >>"$state/calls"
# The defaults are plain strings: a `}` inside ${VAR:-...} would end the
# expansion.
cluster_default='{"running_nodes":["rabbit@a","rabbit@b","rabbit@c"],"partitions":{},"alarms":[]}'
queues_default='[{"name":"q1","type":"quorum","members":["a","b","c"],"online":["a","b","c"]},{"name":"c1","type":"classic","members":"","online":""}]'
pdb_default='{"spec":{"minAvailable":1,"selector":{"matchLabels":{"app.kubernetes.io/instance":"cp-ha-keystone","app.kubernetes.io/name":"keystone"},"matchExpressions":[{"key":"batch.kubernetes.io/job-name","operator":"DoesNotExist"}]}}}'
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
fail_exec() {
  local times
  [ "$1" = "${STUB_EXEC_FAIL:-}" ] || return 1
  if [ -n "${STUB_EXEC_FAIL_TIMES:-}" ]; then
    times="$(next "fail-$1" "$(seq -s, 1 "$STUB_EXEC_FAIL_TIMES"),done")"
    [ "$times" != done ] || return 1
  fi
  echo "error: unable to upgrade connection: container not found (\"$1\")" >&2
  exit 1
}
case "$1" in
  exec)
    pod="$2"
    fail_exec "$pod"
    case "$*" in
      *"-c mariadb -- sh -c"*)
        i="${pod##*-}"
        var="STUB_GALERA_${i}"
        read -r size status st ready <<<"${!var:-3 Primary Synced ON}"
        printf 'wsrep_cluster_size\t%s\nwsrep_cluster_status\t%s\nwsrep_local_state_comment\t%s\nwsrep_ready\t%s\n' \
          "$size" "$status" "$st" "$ready"
        ;;
      *"rabbitmqctl cluster_status --formatter json"*)
        i="${pod##*-}"
        var="STUB_RABBIT_${i}"
        [ -n "${STUB_RABBIT_PREAMBLE:-}" ] && echo "$STUB_RABBIT_PREAMBLE"
        echo "${!var:-$cluster_default}"
        ;;
      *"rabbitmqctl list_queues -p / name type members online --formatter json"*)
        echo "${STUB_QUEUES-$queues_default}"
        ;;
      *"rabbitmqctl eval"*) echo '{ok,pause_minority}' ;;
      *"cluster/status"*)
        db="${pod#cp-ha-ovn-}"
        i="${db##*-}"
        db="${db%-*}"
        var="STUB_ROLE_${db}_${i}"
        if [ "$i" = 0 ]; then role="${!var:-leader}"; else role="${!var:-follower}"; fi
        printf 'Name: OVN\nStatus: cluster member\nRole: %s\nTerm: 2\n' "$role"
        ;;
      *"ovn-nbctl"*)
        if [ -n "${STUB_SYNC_FAIL:-}" ]; then
          echo "ovn-nbctl: timeout expired" >&2
          exit 1
        fi
        ;;
    esac
    ;;
  get)
    case "$*" in
      "get mariadb openstack-db "*"=='Ready')].status}") printf '%s' "${STUB_MARIADB_READY-True}" ;;
      "get mariadb openstack-db "*"=='GaleraReady')].status}") printf '%s' "${STUB_GALERA_READY-True}" ;;
      "get controlplane cp-ha "*"].status}") printf 'True' ;;
      "get controlplane cp-ha "*"].reason}") printf '%s' "${STUB_CP_REASON-AllReady}" ;;
      *"{.status.experiment.desiredPhase}") printf '%s' "$(next phase "${STUB_PHASE-Stop}")" ;;
      "get pods -n openstack-ha -l statefulset.kubernetes.io/pod-name="*"metadata.uid}")
        printf '%s' "$(next uid "${STUB_UID_SEQ:-new}")"
        ;;
      "get pod "*"{.status.phase}") printf '%s' "$(next pod-phase "${STUB_POD_PHASE_SEQ:-Running}")" ;;
      "get pdb cp-ha-"*)
        if [ "${STUB_PDB:-}" = absent ]; then
          echo "Error from server (NotFound): poddisruptionbudgets.policy \"$3\" not found" >&2
          exit 1
        fi
        echo "${STUB_PDB:-$pdb_default}"
        ;;
      "get pods -n openstack-ha -l "*" -o json")
        items=""
        for x in $(seq 1 "${STUB_PODS:-3}"); do
          if ! grep -qx "api-$x" "$state/evicted" 2>/dev/null; then
            item="$(pod "api-$x" "uid-$x" True)"
          else
            case "$(next "replace-$x" "${STUB_REPLACE_SEQ:-ready}")" in
              terminating) item="$(pod "api-$x" "uid-$x" True 2026-10-19T12:00:00Z),$(pod "api-$x-new" "new-$x" True)" ;;
              notready) item="$(pod "api-$x-new" "new-$x" False)" ;;
              *) item="$(pod "api-$x-new" "new-$x" True)" ;;
            esac
          fi
          items="${items:+$items,}$item"
        done
        echo "{\"items\":[$items]}"
        ;;
      "get endpointslices -n openstack-ha -l kubernetes.io/service-name="*)
        count="$(next endpoints "${STUB_ENDPOINTS:-3}")"
        if [ "$count" = fail ]; then
          echo "Error from server (InternalError): the stub refuses the read" >&2
          exit 1
        fi
        echo "{\"items\":[{\"endpoints\":[$(for _ in $(seq 1 "$count"); do printf '{"conditions":{"ready":true}},'; done){\"conditions\":{\"ready\":false}}]}]}"
        ;;
    esac
    ;;
  create)
    cat >>"$state/evictions"
    echo >>"$state/evictions"
    pod="$(sed -E 's|.*/pods/([^/]+)/eviction|\1|' <<<"$3")"
    case "$(next evict "${STUB_EVICT_SEQ:-ok}")" in
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
    ;;
esac
exit 0
SH
chmod +x "$STUB/bin/kubectl"
# shellcheck disable=SC2016 # $* and the variables expand when the stub runs
printf '#!/bin/bash\necho "workload.sh CHAOS_WORKLOAD_ADMIN_SECRET=${CHAOS_WORKLOAD_ADMIN_SECRET:-} $*" >>"$STUB_STATE/calls"\n' \
  >"$STUB/workload.sh"
chmod +x "$STUB/workload.sh"

# run_lib <VAR=value...> -- <shell code>: run code under set -euo pipefail
# with the library sourced, the stub first on PATH, NAMESPACE=openstack-ha and
# every poll interval 0; LF_OUT and LF_RC hold the result.
run_lib() {
  local envs=()
  while [ "$1" != "--" ]; do
    envs+=("$1")
    shift
  done
  shift
  rm -rf "$STUB/state"
  mkdir -p "$STUB/state"
  : >"$STUB/state/calls"
  LF_RC=0
  LF_OUT="$(env STUB_STATE="$STUB/state" NAMESPACE=openstack-ha PATH="$STUB/bin:$PATH" \
    ${envs[@]+"${envs[@]}"} bash -c "set -euo pipefail; . '$LIB'
      LF_POLL_INTERVAL=0 LF_DRAIN_SAMPLE=0 LF_EVICT_RETRY=0; $1" 2>&1)" || LF_RC=$?
}

calls() {
  cat "$STUB/state/calls"
}

# --- Test 1 ---------------------------------------------------------------
test_galera_wait_healthy() {
  echo "Test: galera_wait_healthy passes on three synced Primary members and names a stuck one"
  run_lib -- 'galera_wait_healthy 0'
  assert_eq "it returns 0 on a healthy cluster" "0" "$LF_RC"
  assert_contains "it prints GALERA-OK" "$LF_OUT" "GALERA-OK "
  assert_contains "it reads the members in the container mariadb" "$(calls)" \
    "exec openstack-db-2 -n openstack-ha -c mariadb -- sh -c"
  assert_not_contains "the root password stays out of the argv" "$(calls)" "-p\""

  run_lib STUB_GALERA_2="1 non-Primary Initialized OFF" -- 'galera_wait_healthy 0'
  assert_eq "it returns 1 on a member stuck at non-Primary" "1" "$LF_RC"
  assert_contains "it says so" "$LF_OUT" "ERROR: Galera openstack-db not healthy within 0s"
  assert_contains "it names the member's last status" "$LF_OUT" \
    "openstack-db-2: size=1 status=non-Primary state=Initialized ready=OFF"

  run_lib STUB_GALERA_READY=False -- 'galera_wait_healthy 0'
  assert_eq "it returns 1 when GaleraReady is False" "1" "$LF_RC"
  assert_contains "it names the condition" "$LF_OUT" "GaleraReady='False'"
}

# --- Test 2 ---------------------------------------------------------------
test_galera_member_down() {
  echo "Test: a member whose exec fails is not healthy yet, and no abort"
  run_lib STUB_EXEC_FAIL=openstack-db-1 -- 'galera_member_status openstack-db-1'
  assert_eq "galera_member_status returns 1 on a failed exec" "1" "$LF_RC"
  assert_contains "it prints the error as unreachable" "$LF_OUT" "unreachable: error: unable to upgrade connection"

  run_lib STUB_EXEC_FAIL=openstack-db-1 -- 'galera_wait_healthy 0; echo not-reached'
  assert_eq "galera_wait_healthy returns 1 at its deadline" "1" "$LF_RC"
  assert_contains "it names the unreachable member" "$LF_OUT" "openstack-db-1: unreachable:"

  run_lib STUB_EXEC_FAIL=openstack-db-1 STUB_EXEC_FAIL_TIMES=2 -- 'galera_wait_healthy 60; echo after'
  assert_eq "it passes once the member answers again" "0" "$LF_RC"
  assert_contains "it went on polling" "$LF_OUT" "GALERA-OK"
  assert_contains "the caller goes on under set -e" "$LF_OUT" "after"
  assert_eq "it read the member three times" "3" "$(grep -c '^exec openstack-db-1 ' "$STUB/state/calls")"
}

# --- Test 3 ---------------------------------------------------------------
test_rabbit_wait_healthy() {
  echo "Test: rabbit_wait_healthy passes on three running nodes with online quorum queues"
  run_lib -- 'rabbit_wait_healthy 0'
  assert_eq "it returns 0 on a healthy broker" "0" "$LF_RC"
  assert_contains "it prints BROKER-OK" "$LF_OUT" "BROKER-OK "
  assert_contains "with the count of small quorum queues" "$LF_OUT" "s quorum-queues-under-3-members=0"

  run_lib STUB_RABBIT_PREAMBLE="Cluster status of node rabbit@a ..." -- 'rabbit_cluster openstack-rabbitmq-server-0'
  assert_eq "a line before the JSON is dropped" "running=3 partitions=0 alarms=0" "$LF_OUT"

  run_lib STUB_RABBIT_1='{"running_nodes":["a","b","c"],"partitions":{"rabbit@a":["rabbit@b"]},"alarms":[]}' -- 'rabbit_wait_healthy 0'
  assert_eq "it returns 1 on a non-empty partitions map" "1" "$LF_RC"
  assert_contains "it names the node's view" "$LF_OUT" "openstack-rabbitmq-server-1: running=3 partitions=1 alarms=0"

  run_lib STUB_QUEUES='[{"name":"q1","type":"quorum","members":["a","b","c"],"online":["a","b"]}]' -- 'rabbit_wait_healthy 0'
  assert_eq "it returns 1 on a quorum queue with an offline member" "1" "$LF_RC"
  assert_contains "it names the queue" "$LF_OUT" "quorum queues: offline=q1 under3=0"

  run_lib STUB_QUEUES='[]' -- 'rabbit_wait_healthy 0'
  assert_eq "an empty queue list passes" "0" "$LF_RC"
  assert_contains "and counts 0 small quorum queues" "$LF_OUT" "quorum-queues-under-3-members=0"

  run_lib STUB_QUEUES='[{"name":"q1","type":"quorum","members":["a"],"online":["a"]}]' -- 'rabbit_wait_healthy 0'
  assert_eq "a quorum queue with one member is no failure" "0" "$LF_RC"
  assert_contains "it is counted" "$LF_OUT" "quorum-queues-under-3-members=1"

  run_lib STUB_EXEC_FAIL=openstack-rabbitmq-server-2 -- 'rabbit_wait_healthy 0'
  assert_eq "a node whose exec fails is not healthy" "1" "$LF_RC"
  assert_contains "it is named unreachable" "$LF_OUT" "openstack-rabbitmq-server-2: unreachable:"

  run_lib STUB_QUEUES='Listing queues for vhost / ...' -- 'rabbit_quorum_queues'
  assert_eq "a reply without JSON returns 1" "1" "$LF_RC"
  assert_eq "it reads as unreadable" "unreadable: Listing queues for vhost / ..." "$LF_OUT"
  run_lib STUB_QUEUES='Listing queues for vhost / ...' -- 'rabbit_wait_healthy 0'
  assert_eq "and the broker is not healthy" "1" "$LF_RC"
  assert_contains "it names the reply" "$LF_OUT" "quorum queues: unreadable: Listing queues for vhost / ..."
  run_lib STUB_RABBIT_0='{not json' -- 'rabbit_cluster openstack-rabbitmq-server-0'
  assert_eq "malformed JSON returns 1" "1" "$LF_RC"
  assert_eq "it reads as unreadable" "unreadable: {not json" "$LF_OUT"

  run_lib -- 'rabbit_partition_handling'
  assert_contains "rabbit_partition_handling asks server-1" "$(calls)" \
    "exec openstack-rabbitmq-server-1 -n openstack-ha -c rabbitmq -- rabbitmqctl eval application:get_env(rabbit, cluster_partition_handling)."
}

# --- Test 4 ---------------------------------------------------------------
test_ovn() {
  echo "Test: ovn_leader names one leader and refuses zero or two; the Raft waits"
  run_lib STUB_ROLE_nb_0=follower STUB_ROLE_nb_1=leader -- 'ovn_leader nb'
  assert_eq "ovn_leader returns 0 on one leader" "0" "$LF_RC"
  assert_eq "it prints that pod alone" "cp-ha-ovn-nb-1" "$LF_OUT"
  assert_contains "it reads the role through the unixctl socket" "$(calls)" \
    "exec cp-ha-ovn-nb-1 -n openstack-ha -c ovsdb -- ovs-appctl -t /var/run/ovn/ovnnb_db.ctl cluster/status OVN_Northbound"

  run_lib STUB_ROLE_sb_0=follower -- 'ovn_leader sb'
  assert_eq "ovn_leader returns 1 on zero leaders" "1" "$LF_RC"
  assert_contains "it prints every role" "$LF_OUT" \
    "ERROR: sb has 0 leaders: cp-ha-ovn-sb-0=follower cp-ha-ovn-sb-1=follower cp-ha-ovn-sb-2=follower"

  run_lib STUB_ROLE_nb_2=leader -- 'ovn_leader nb'
  assert_eq "ovn_leader returns 1 on two leaders" "1" "$LF_RC"
  assert_contains "it prints every role" "$LF_OUT" \
    "ERROR: nb has 2 leaders: cp-ha-ovn-nb-0=leader cp-ha-ovn-nb-1=follower cp-ha-ovn-nb-2=leader"

  run_lib STUB_EXEC_FAIL=cp-ha-ovn-sb-2 -- 'ovn_role sb 2'
  assert_eq "ovn_role returns 1 on a failed exec" "1" "$LF_RC"

  run_lib STUB_EXEC_FAIL=cp-ha-ovn-sb-2 -- 'ovn_leader sb'
  assert_eq "ovn_leader still finds the leader beside a member that is down" "cp-ha-ovn-sb-0" "$LF_OUT"

  run_lib -- 'ovn_follower nb'
  assert_eq "ovn_follower skips the leader at index 0" "cp-ha-ovn-nb-1" "$LF_OUT"
  run_lib STUB_ROLE_nb_0=follower STUB_ROLE_nb_1=leader -- 'ovn_follower nb'
  assert_eq "ovn_follower takes index 0 when it is no leader" "cp-ha-ovn-nb-0" "$LF_OUT"
  run_lib STUB_ROLE_nb_0=candidate -- 'ovn_follower nb'
  assert_eq "ovn_follower returns 1 without a leader" "1" "$LF_RC"
  run_lib STUB_ROLE_nb_0=follower STUB_ROLE_nb_1=leader -- 'ovn_follower nb cp-ha-ovn-nb-0'
  assert_eq "with a leader given it never names that pod, whoever leads now" "cp-ha-ovn-nb-1" "$LF_OUT"
  assert_eq "and it reads no role" "" "$(calls)"

  run_lib -- 'ovn_nbctl_sync cp-ha-ovn-nb-2 10'
  assert_eq "ovn_nbctl_sync returns 0 when the sync returns" "0" "$LF_RC"
  assert_eq "it syncs in the given pod within its timeout" \
    "exec cp-ha-ovn-nb-2 -n openstack-ha -c ovsdb -- ovn-nbctl --db=unix:/var/run/ovn/ovnnb_db.sock --timeout=10 --wait=sb sync" \
    "$(calls)"
  run_lib STUB_SYNC_FAIL=1 -- 'ovn_nbctl_sync cp-ha-ovn-nb-2 10'
  assert_eq "it returns 1 when the sync fails" "1" "$LF_RC"

  run_lib -- 'ovn_wait_healthy sb 0'
  assert_eq "ovn_wait_healthy returns 0 on three members and one leader" "0" "$LF_RC"
  assert_contains "it prints RAFT-OK with the leader" "$LF_OUT" "RAFT-OK sb leader=cp-ha-ovn-sb-0 "
  run_lib STUB_EXEC_FAIL=cp-ha-ovn-nb-1 -- 'ovn_wait_healthy nb 0'
  assert_eq "ovn_wait_healthy returns 1 with a member down" "1" "$LF_RC"
  assert_contains "it names the member" "$LF_OUT" "(2 of 3 members, 1 leaders)"
  assert_contains "and its state" "$LF_OUT" "cp-ha-ovn-nb-1=unreachable/<none>"

  run_lib -- 'ovn_wait_northd_sync 0'
  assert_eq "ovn_wait_northd_sync returns 0 when the sync returns" "0" "$LF_RC"
  assert_contains "it prints NORTHD-SYNC-OK" "$LF_OUT" "NORTHD-SYNC-OK "
  assert_contains "it syncs in the Northbound leader" "$(calls)" \
    "exec cp-ha-ovn-nb-0 -n openstack-ha -c ovsdb -- ovn-nbctl --db=unix:/var/run/ovn/ovnnb_db.sock --timeout=1 --wait=sb sync"
  run_lib STUB_SYNC_FAIL=1 -- 'ovn_wait_northd_sync 0'
  assert_eq "ovn_wait_northd_sync returns 1 when the sync fails" "1" "$LF_RC"
  assert_contains "it names ovn-nbctl's message" "$LF_OUT" "ovn-nbctl: timeout expired"
}

# --- Test 5 ---------------------------------------------------------------
test_lf_render() {
  echo "Test: lf_render fills every key and names a placeholder left over"
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
test_lf_evict_drain() {
  echo "Test: lf_evict_drain lets the PDB set the pace and refuses a drain it did not see"
  run_lib STUB_EVICT_SEQ=ok,ok,429,ok STUB_ENDPOINTS=3,1,3 -- 'lf_evict_drain keystone 60'
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

  run_lib STUB_EVICT_SEQ=ok,ok,429,ok STUB_ENDPOINTS=fail,2 -- 'lf_evict_drain keystone 60'
  assert_eq "a failed endpoint read is no sample" "0" "$LF_RC"
  assert_contains "it is logged" "$LF_OUT" "drain keystone: no endpoint sample at "
  assert_contains "with kubectl's message" "$LF_OUT" "s: Error from server (InternalError): the stub refuses the read"
  assert_contains "the samples that worked decide min-ready" "$LF_OUT" "DRAIN-OK keystone refused=1 min-ready=2 "

  run_lib STUB_EVICT_SEQ=ok,notfound,429,ok STUB_ENDPOINTS=2 -- 'lf_evict_drain keystone 60'
  assert_eq "a NotFound answer counts the pod as gone" "0" "$LF_RC"
  assert_contains "it says so" "$LF_OUT" "drain keystone: api-2 is gone"
  assert_eq "it does not evict a gone pod again" "1" "$(grep -c 'pods/api-2/eviction' "$STUB/state/calls")"

  run_lib STUB_EVICT_SEQ=ok,ok,throttled,ok -- 'lf_evict_drain keystone 60'
  assert_eq "a throttled eviction is no refusal" "1" "$LF_RC"
  assert_contains "it logs the throttle" "$LF_OUT" "drain keystone: api-3 throttled at "
  assert_contains "and counts no refusal" "$LF_OUT" "DRAIN-OK keystone refused=0 "
  assert_eq "it retried the throttled pod" "2" "$(grep -c 'pods/api-3/eviction' "$STUB/state/calls")"

  run_lib STUB_EVICT_SEQ=ok,ok,429,ok STUB_REPLACE_SEQ=terminating,notready,ready -- 'lf_evict_drain keystone 60'
  assert_eq "it waits for the old UIDs to go and the new pods to turn Ready" "0" "$LF_RC"
  assert_contains "then it prints DRAIN-OK" "$LF_OUT" "DRAIN-OK keystone refused=1 "
  assert_eq "it listed the pods until all three were new and Ready" "4" \
    "$(grep -c '^get pods -n openstack-ha -l .* -o json$' "$STUB/state/calls")"
  run_lib STUB_EVICT_SEQ=ok STUB_REPLACE_SEQ=terminating -- 'lf_evict_drain keystone 0'
  assert_eq "an original UID still listed fails the drain at its deadline" "1" "$LF_RC"
  assert_not_contains "beside three Ready new pods it prints no DRAIN-OK" "$LF_OUT" "DRAIN-OK"
  assert_contains "with no eviction pending" "$LF_OUT" "evictions pending: none"
  run_lib STUB_EVICT_SEQ=ok STUB_REPLACE_SEQ=notready -- 'lf_evict_drain keystone 0'
  assert_eq "new pods that never turn Ready fail the drain" "1" "$LF_RC"
  assert_not_contains "no DRAIN-OK" "$LF_OUT" "DRAIN-OK"

  run_lib STUB_EVICT_SEQ=ok,forbidden -- 'lf_evict_drain keystone 60'
  assert_eq "it returns 1 on a Forbidden answer" "1" "$LF_RC"
  assert_contains "it prints kubectl's message" "$LF_OUT" \
    "ERROR: the eviction of api-2 failed: Error from server (Forbidden): pods \"api-2\" is forbidden"

  run_lib STUB_PDB=absent -- 'lf_evict_drain nova 60'
  assert_eq "it returns 1 without a PDB" "1" "$LF_RC"
  assert_contains "it names the PDB" "$LF_OUT" "ERROR: no PDB cp-ha-nova"
  assert_not_contains "it evicts nothing" "$(calls)" "eviction"

  run_lib STUB_PODS=0 -- 'lf_evict_drain glance 60'
  assert_eq "it returns 1 on an empty selection" "1" "$LF_RC"
  assert_contains "it names the count" "$LF_OUT" "ERROR: PDB cp-ha-glance selects 0 pods, want 3"
  run_lib STUB_PODS=2 -- 'lf_evict_drain glance 60'
  assert_contains "it refuses two pods as well" "$LF_OUT" "selects 2 pods, want 3"

  run_lib STUB_EVICT_SEQ=ok -- 'lf_evict_drain placement 60'
  assert_eq "it returns 1 when no eviction was refused" "1" "$LF_RC"
  assert_contains "it still prints the drain" "$LF_OUT" "DRAIN-OK placement refused=0 min-ready=3"
  assert_contains "it names refused=0" "$LF_OUT" "ERROR: drain placement: refused=0"

  run_lib STUB_EVICT_SEQ=ok,ok,429,ok STUB_ENDPOINTS=3,0,2 -- 'lf_evict_drain horizon 60'
  assert_eq "it returns 1 when the Service had no ready endpoint" "1" "$LF_RC"
  assert_contains "it names min-ready=0" "$LF_OUT" "ERROR: drain horizon: min-ready=0"

  run_lib STUB_EVICT_SEQ=429 -- 'lf_evict_drain cinder 0'
  assert_eq "it returns 1 at its deadline" "1" "$LF_RC"
  assert_contains "it names the pods still to evict" "$LF_OUT" "evictions pending: api-1 api-2 api-3"
}

# --- Test 7 ---------------------------------------------------------------
test_cp_ready_and_expiry() {
  echo "Test: lf_wait_cp_ready wants AllReady; lf_wait_expired waits for Stop and holds its check"
  run_lib -- 'lf_wait_cp_ready 0'
  assert_eq "lf_wait_cp_ready returns 0 on Ready/AllReady" "0" "$LF_RC"
  run_lib STUB_CP_REASON=NotAllReady -- 'lf_wait_cp_ready 0'
  assert_eq "it returns 1 on another reason" "1" "$LF_RC"
  assert_contains "it names the reason" "$LF_OUT" "reason 'NotAllReady', not AllReady"

  run_lib -- 'lf_wait_expired podchaos lf-x 0'
  assert_eq "lf_wait_expired returns 0 on Stop" "0" "$LF_RC"
  run_lib STUB_PHASE=Run -- 'lf_wait_expired podchaos lf-x 0'
  assert_eq "it returns 1 while the chaos still runs at its deadline" "1" "$LF_RC"
  assert_contains "it names the phase" "$LF_OUT" "(last: 'Run')"
  run_lib STUB_PHASE=Run -- 'lf_wait_expired podchaos lf-x 60 false'
  assert_eq "it returns 1 when its check fails" "1" "$LF_RC"
  assert_contains "it names the check" "$LF_OUT" "ERROR: 'false' failed while podchaos/lf-x ran"
  run_lib STUB_PHASE=Run,Run,Stop -- 'lf_wait_expired podchaos lf-x 60 echo held'
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
test_lf_wait_recreated() {
  echo "Test: lf_wait_recreated waits for a Running pod with a new UID"
  run_lib STUB_UID_SEQ=old,new,new STUB_POD_PHASE_SEQ=Running,Pending,Running -- 'lf_wait_recreated cp-ha-ovn-nb-0 old 60'
  assert_eq "it returns 0 once a new UID is Running" "0" "$LF_RC"
  assert_contains "it names the new UID" "$LF_OUT" "OK: cp-ha-ovn-nb-0 is Running again as new after "
  assert_eq "it waited past the old UID and the Pending pod" "3" "$(grep -c '{.status.phase}$' "$STUB/state/calls")"
  assert_contains "it reads the UID by the StatefulSet's pod-name label" "$(calls)" \
    "get pods -n openstack-ha -l statefulset.kubernetes.io/pod-name=cp-ha-ovn-nb-0 -o jsonpath={.items[*].metadata.uid}"
  run_lib STUB_UID_SEQ=old -- 'lf_wait_recreated cp-ha-ovn-nb-0 old 0'
  assert_eq "the old UID at the deadline fails" "1" "$LF_RC"
  assert_contains "it names the UID and the phase" "$LF_OUT" "(UID 'old', phase 'Running')"
}

# --- Test 9 ---------------------------------------------------------------
test_lf_workload_start() {
  echo "Test: lf_workload_start starts the harness against the plane as its admin"
  run_lib -- "LF_WORKLOAD='$STUB/workload.sh'; lf_workload_start galera-member-loss token,server"
  assert_eq "it returns 0" "0" "$LF_RC"
  assert_eq "it passes the id, the streams, the endpoints, the admin Secret and the seed" \
    "workload.sh CHAOS_WORKLOAD_ADMIN_SECRET=cp-ha-keystone-admin-credentials start galera-member-loss -- --streams token,server --keystone http://cp-ha-keystone.openstack-ha.svc:5000/v3 --http http://cp-ha-horizon.openstack-ha.svc:8080/auth/login/ --glance http://cp-ha-glance.openstack-ha.svc:9292 --placement http://cp-ha-placement.openstack-ha.svc:8778 --barbican http://cp-ha-barbican.openstack-ha.svc:9311 --neutron http://cp-ha-neutron.openstack-ha.svc:9696 --cinder http://cp-ha-cinder.openstack-ha.svc:8776 --nova http://cp-ha-nova.openstack-ha.svc:8774/v2.1 --image-ref 4e0a0000-0000-4000-8000-000000001393 --flavor m1.nano" \
    "$(calls)"
}

# --- Test 10 --------------------------------------------------------------
test_library_parses_and_sources_quietly() {
  echo "Test: the library parses, and sourcing it calls nothing"
  assert_eq "bash -n passes" "0" "$(bash -n "$LIB" >/dev/null 2>&1 && echo 0 || echo 1)"
  run_lib -- 'true'
  assert_eq "sourcing it returns 0" "0" "$LF_RC"
  assert_eq "sourcing it calls no kubectl" "" "$(calls)"
}

test_galera_wait_healthy
test_galera_member_down
test_rabbit_wait_healthy
test_ovn
test_lf_render
test_lf_evict_drain
test_cp_ready_and_expiry
test_lf_wait_recreated
test_lf_workload_start
test_library_parses_and_sources_quietly

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
