#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the OVN data-path probes (tests/e2e-chaos/lib/datapath.sh, #1392)
# on recorded `ping -D -O` logs and pod states that a stub kubectl serves:
#   1. ping-verdict passes a log whose lost count equals the budget and fails
#      one above it, printing the lost count and the longest gap in seconds;
#      a duplicate reply counts once. Replies that stop for good count as
#      lost up to the last packet sent, less one still in flight.
#   2. It exits 1 with "no reply recorded for <pair>" on an empty log and on a
#      log that holds only "no answer yet" lines, with "no budget line for
#      <pair>" when the suite header has none, on a second budget line, and
#      naming the pod when kubectl logs fails.
#   3. ping-dark fails on a reply inside its window and passes on a log whose
#      last reply is older, or that holds none.
#   4. ping-wait passes on a reply after the call and fails at its deadline.
#   5. teardown exits 0 when the probe pods and the OVNCentral are gone.
#   6. A usage error, an invalid pair and a missing NAMESPACE exit 2.
#   7. nb and sb print the command's stdout after the @@OUTPUT@@ marker and
#      nothing else, add --timeout=10 unless an argument sets a timeout, and
#      exit 1 with the pod's log when the pod fails. The pod's script, run
#      here with a stub tool, retries a connection failure, gives up on a TLS
#      failure with the tool's status, and stops after the last attempt.
#   8. plug records <port>:<netns> once in the probe pod's annotation, and
#      teardown removes every recorded namespace and OVS port, even when the
#      exec fails.
#   9. ping-start pings with -e and the smallest ICMP identifier no other
#      ping pod holds, records it in the pod's annotation, and exits 1 when
#      the ping pods cannot be listed. A probe pod's annotations stay empty.
#
# Usage: bash tests/unit/ci/chaos_datapath_lib_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
LIB="$PROJECT_ROOT/tests/e2e-chaos/lib/datapath.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT
STATE="$TMP_DIR/state"
SUITES="$TMP_DIR/suites"
mkdir -p "$TMP_DIR/bin"

# The stub kubectl logs every call to $STATE/kubectl.log.
# - `logs` of chaos-dp-ping-<pair> prints $STATE/log-<pair> and fails when
#   $STATE/logfail-<pair> exists; `logs` of an nb or sb pod prints
#   $STATE/ctl-log.
# - `exec ... date` prints $STATE/now, the pod clock. Any other exec fails when
#   $STATE/execfail exists, and otherwise runs its command here, against the
#   stub ip and ovs-vsctl.
# - `apply` writes the manifest to $STATE/applied, and `annotate` the plugs
#   value to $STATE/plugs.
# - `get` prints $STATE/addr-<central> for an OVNCentral, an image for a
#   StatefulSet or a pod, $STATE/phase for a pod phase, $STATE/probe-pods for
#   the probe pod list, $STATE/plugs for the plugs annotation and
#   $STATE/icmp-ids for the identifiers of the ping pods; that list fails when
#   $STATE/listfail exists.
# Every other call prints nothing and passes.
cat >"$TMP_DIR/bin/kubectl" <<'SH'
#!/bin/bash
echo "$*" >>"$STATE/kubectl.log"
case "$1" in
  logs)
    case "$2" in
      chaos-dp-nb-* | chaos-dp-sb-*) cat "$STATE/ctl-log" 2>/dev/null ;;
      *)
        pair="${2#chaos-dp-ping-}"
        if [ -e "$STATE/logfail-$pair" ]; then
          echo "Error from server (NotFound): pods \"$2\" not found" >&2
          exit 1
        fi
        cat "$STATE/log-$pair" 2>/dev/null
        ;;
    esac
    ;;
  exec)
    while [ "$1" != "--" ]; do shift; done
    shift
    if [ "$1" = date ]; then
      cat "$STATE/now"
      exit 0
    fi
    [ ! -e "$STATE/execfail" ] || exit 1
    PATH="$STUB_TOOLS:$PATH" "$@"
    exit $?
    ;;
  apply) cat >"$STATE/applied" ;;
  annotate)
    last="${*: -1}"
    printf '%s' "${last#*=}" >"$STATE/plugs"
    ;;
  get)
    case "$*" in
      "get ovncentral "*) cat "$STATE/addr-$3" 2>/dev/null ;;
      "get sts "*) echo ghcr.io/c5c3/ovn:test ;;
      *'{.spec.containers[0].image}'*) echo ghcr.io/c5c3/ovn:test ;;
      *'chaos\.c5c3\.io/icmp-id'*)
        [ ! -e "$STATE/listfail" ] || exit 1
        cat "$STATE/icmp-ids" 2>/dev/null
        ;;
      *"{.status.phase}"*) cat "$STATE/phase" 2>/dev/null ;;
      *"datapath=probe -o name"*) cat "$STATE/probe-pods" 2>/dev/null ;;
      *'chaos\.c5c3\.io/plugs'*) cat "$STATE/plugs" 2>/dev/null ;;
    esac
    ;;
esac
exit 0
SH
chmod +x "$TMP_DIR/bin/kubectl"

# The stub tools the exec'd scripts and the nb/sb pod script call, in
# $STUB_TOOLS. ip and ovs-vsctl log their arguments to $STATE/tools.log and
# pass, except that `ip ... link show` fails: the port is in no namespace yet.
# ovn-nbctl and ovn-sbctl log theirs too and act out the next line of
# $STATE/outcomes: refused (a connection failure), tls (a TLS failure, exit 3)
# or ok (a warning on stderr and two rows). sleep returns at once.
STUB_TOOLS="$TMP_DIR/tools"
mkdir -p "$STUB_TOOLS"
cat >"$STUB_TOOLS/ip" <<'SH'
#!/bin/bash
echo "ip $*" >>"$STATE/tools.log"
case "$*" in *"link show"*) exit 1 ;; esac
exit 0
SH
cat >"$STUB_TOOLS/ovs-vsctl" <<'SH'
#!/bin/bash
echo "ovs-vsctl $*" >>"$STATE/tools.log"
exit 0
SH
cat >"$STUB_TOOLS/ovn-nbctl" <<'SH'
#!/bin/bash
echo "$(basename "$0") $*" >>"$STATE/tools.log"
outcome="$(head -1 "$STATE/outcomes")"
tail -n +2 "$STATE/outcomes" >"$STATE/outcomes.next" && mv "$STATE/outcomes.next" "$STATE/outcomes"
case "$outcome" in
  refused)
    echo "ovn-nbctl: ssl:10.96.0.5:6641: database connection failed (Connection refused)" >&2
    exit 1
    ;;
  tls)
    echo "ovn-nbctl: ssl:10.96.0.5:6641: database connection failed (Protocol error)" >&2
    exit 3
    ;;
  ok)
    echo "warn line" >&2
    printf '%s\n' row1 row2
    ;;
esac
SH
cp "$STUB_TOOLS/ovn-nbctl" "$STUB_TOOLS/ovn-sbctl"
printf '#!/bin/bash\nexit 0\n' >"$STUB_TOOLS/sleep"
chmod +x "$STUB_TOOLS"/*

# reset: empty the stub state and write the suite s1 with the budget lines of
# the pairs a and b.
reset() {
  rm -rf "$STATE" "$SUITES"
  mkdir -p "$STATE" "$SUITES/s1"
  : >"$STATE/kubectl.log"
  cat >"$SUITES/s1/chainsaw-test.yaml" <<'EOF'
# Chainsaw chaos E2E test s1.
# Chaos matrix: ovn self pod-kill
# Chaos datapath budget: a lost=3
# Chaos datapath budget: b lost=2
# Chaos budget: failures=0 recovery=0s
apiVersion: chainsaw.kyverno.io/v1alpha1
EOF
}

# reply <timestamp> <seq>: one reply line of ping -D.
reply() {
  printf '[%s] 64 bytes from 10.97.0.11: icmp_seq=%s ttl=64 time=0.412 ms\n' "$1" "$2"
}

# gapped_log: replies 1 to 10 every 0.5 s, with 4 to 6 lost and answered by
# "no answer yet" lines, and a duplicate of 7. Three lost, longest gap 2.0 s.
gapped_log() {
  echo "PING 10.97.0.11 (10.97.0.11) 56(84) bytes of data."
  reply 1000.000000 1
  reply 1000.500000 2
  reply 1001.000000 3
  echo "[1001.500000] no answer yet for icmp_seq=4"
  echo "[1002.000000] no answer yet for icmp_seq=5"
  echo "[1002.500000] no answer yet for icmp_seq=6"
  reply 1003.000000 7
  printf '[1003.010000] 64 bytes from 10.97.0.11: icmp_seq=7 ttl=64 time=0.5 ms (DUP!)\n'
  reply 1003.500000 8
  reply 1004.000000 9
  reply 1004.500000 10
}

# run_dp [VAR=value...] -- <args...>: run datapath.sh with the stub first on
# PATH, NAMESPACE openstack and the suites under $SUITES; OUT and RC hold the
# result.
run_dp() {
  local envs=()
  while [ "$1" != "--" ]; do
    envs+=("$1")
    shift
  done
  shift
  RC=0
  OUT="$(env PATH="$TMP_DIR/bin:$PATH" STATE="$STATE" STUB_TOOLS="$STUB_TOOLS" NAMESPACE=openstack \
    CHAOS_DATAPATH_SUITES_DIR="$SUITES" CHAOS_DATAPATH_POLL_SECONDS=1 \
    ${envs[@]+"${envs[@]}"} bash "$LIB" "$@" 2>&1)" || RC=$?
}

kubectl_log() {
  cat "$STATE/kubectl.log"
}

tools_log() {
  cat "$STATE/tools.log" 2>/dev/null
}

# applied <jq filter>: the filter's raw output on the last applied manifest.
applied() {
  jq -r "$1" "$STATE/applied"
}

# run_ctl_pod: run the container command of the last applied nb or sb pod here,
# with the pod's env and the stub tools first on PATH; OUT and RC hold the
# result. The script's scratch file /tmp/err moves under $TMP_DIR.
run_ctl_pod() {
  local script args=() envs=()
  script="$(applied '.spec.containers[0].command[2]')"
  script="${script//\/tmp\/err/$TMP_DIR/err}"
  mapfile -t args < <(applied '.spec.containers[0].command[4:][]')
  mapfile -t envs < <(applied '.spec.containers[0].env[] | "\(.name)=\(.value)"')
  RC=0
  OUT="$(env PATH="$STUB_TOOLS:$PATH" STATE="$STATE" "${envs[@]}" \
    bash -c "$script" _ "${args[@]}" 2>&1)" || RC=$?
}

# --- Test 1 ---------------------------------------------------------------
test_the_verdict_against_the_budget() {
  echo "Test: ping-verdict passes the budget and fails one above it"
  reset
  gapped_log >"$STATE/log-a"
  run_dp -- ping-verdict s1 a
  assert_eq "three lost against a budget of three exits 0" "0" "$RC"
  assert_contains "it prints the count and the longest gap" "$OUT" \
    "datapath a: 7 replies, seq 1-10, lost 3 (budget 3), longest gap 2.0 s"
  assert_contains "and the verdict" "$OUT" "DATAPATH-OK a"

  gapped_log >"$STATE/log-b"
  run_dp -- ping-verdict s1 b
  assert_eq "three lost against a budget of two exits 1" "1" "$RC"
  assert_contains "it still prints the count and the gap" "$OUT" \
    "datapath b: 7 replies, seq 1-10, lost 3 (budget 2), longest gap 2.0 s"
  assert_contains "and names the budget" "$OUT" "FAIL: datapath b lost 3, over its budget of 2"
  assert_not_contains "and no OK" "$OUT" "DATAPATH-OK"

  echo "Test: ping-verdict counts replies that stop for good as lost"
  reset
  {
    echo "PING 10.97.0.11 (10.97.0.11) 56(84) bytes of data."
    for seq in 1 2 3 4 5; do reply "100${seq}.000000" "$seq"; done
    for seq in $(seq 6 20); do echo "[1010.${seq}00000] no answer yet for icmp_seq=${seq}"; done
  } >"$STATE/log-a"
  run_dp -- ping-verdict s1 a
  assert_eq "a dark tail over the budget exits 1" "1" "$RC"
  assert_contains "it counts up to the last packet sent, less one in flight" "$OUT" \
    "datapath a: 5 replies, seq 1-19, lost 14 (budget 3)"
  assert_contains "and names the budget" "$OUT" "FAIL: datapath a lost 14, over its budget of 3"

  {
    gapped_log
    echo "[1005.000000] no answer yet for icmp_seq=11"
  } >"$STATE/log-a"
  run_dp -- ping-verdict s1 a
  assert_eq "one packet still in flight is no loss" "0" "$RC"
  assert_contains "the count stays at three" "$OUT" "datapath a: 7 replies, seq 1-10, lost 3 (budget 3)"
}

# --- Test 2 ---------------------------------------------------------------
test_the_verdict_refuses_an_empty_record() {
  echo "Test: ping-verdict refuses an empty record, a missing budget and an unreadable log"
  reset
  : >"$STATE/log-a"
  run_dp -- ping-verdict s1 a
  assert_eq "an empty log exits 1" "1" "$RC"
  assert_contains "it says so" "$OUT" "FAIL: no reply recorded for a"

  printf '%s\n' "PING 10.97.0.11 (10.97.0.11) 56(84) bytes of data." \
    "[1001.500000] no answer yet for icmp_seq=1" "[1002.000000] no answer yet for icmp_seq=2" >"$STATE/log-a"
  run_dp -- ping-verdict s1 a
  assert_eq "a log without a reply exits 1" "1" "$RC"
  assert_contains "with the same reason" "$OUT" "FAIL: no reply recorded for a"

  gapped_log >"$STATE/log-c"
  run_dp -- ping-verdict s1 c
  assert_eq "a pair without a budget line exits 1" "1" "$RC"
  assert_contains "it names the pair" "$OUT" "FAIL: no budget line for c"
  assert_not_contains "before it reads any log" "$(cat "$STATE/kubectl.log")" "logs chaos-dp-ping-c"

  echo "# Chaos datapath budget: a lost=9" >>"$SUITES/s1/chainsaw-test.yaml"
  run_dp -- ping-verdict s1 a
  assert_eq "a second budget line exits 1" "1" "$RC"
  assert_contains "it names the file" "$OUT" "$SUITES/s1/chainsaw-test.yaml has more than one budget line for a"

  reset
  touch "$STATE/logfail-a"
  run_dp -- ping-verdict s1 a
  assert_eq "a failing kubectl logs exits 1" "1" "$RC"
  assert_contains "it names the pod and the error" "$OUT" \
    "FAIL: cannot read the log of pod chaos-dp-ping-a: Error from server (NotFound)"

  run_dp -- ping-verdict nosuite a
  assert_eq "a suite without a file exits 1" "1" "$RC"
  assert_contains "for want of a budget line" "$OUT" "FAIL: no budget line for a"
}

# --- Test 3 ---------------------------------------------------------------
test_the_dark_window() {
  echo "Test: ping-dark fails on a recent reply and passes on an old one"
  reset
  gapped_log >"$STATE/log-a"
  echo 1030.000000000 >"$STATE/now"
  run_dp -- ping-dark a 60
  assert_eq "a reply 25.5 s ago is inside a 60-second window" "1" "$RC"
  assert_contains "it names the age and the window" "$OUT" \
    "FAIL: a reply on a arrived 25.5 s ago, inside the 60-second dark window"

  echo 1100.000000000 >"$STATE/now"
  run_dp -- ping-dark a 60
  assert_eq "a reply 95.5 s ago passes" "0" "$RC"
  assert_contains "it reports the age" "$OUT" "DARK-OK a: last reply 95.5 s ago"

  printf '%s\n' "[1001.500000] no answer yet for icmp_seq=1" >"$STATE/log-a"
  run_dp -- ping-dark a 60
  assert_eq "a log without a reply passes" "0" "$RC"
  assert_contains "and says so" "$OUT" "DARK-OK a: no reply at all"

  touch "$STATE/logfail-a"
  run_dp -- ping-dark a 60
  assert_eq "an unreadable log fails" "1" "$RC"
  assert_contains "naming the pod" "$OUT" "chaos-dp-ping-a"
}

# --- Test 4 ---------------------------------------------------------------
test_the_wait_for_a_reply() {
  echo "Test: ping-wait passes on a fresh reply and fails at its deadline"
  reset
  echo 1004.200000000 >"$STATE/now"
  gapped_log >"$STATE/log-a"
  run_dp -- ping-wait a 5
  assert_eq "a reply after the call passes" "0" "$RC"
  assert_contains "it names the reply" "$OUT" "OK: a answered at 1004.500000"

  echo 1010.000000000 >"$STATE/now"
  run_dp -- ping-wait a 1
  assert_eq "a log whose replies predate the call fails" "1" "$RC"
  assert_contains "at the deadline" "$OUT" "FAIL: no reply on a within 1 s"
}

# --- Test 5 ---------------------------------------------------------------
test_teardown_tolerates_missing_pods() {
  echo "Test: teardown exits 0 when the probe pods and the central are gone"
  reset
  run_dp -- teardown
  assert_eq "without a central it exits 0" "0" "$RC"
  assert_contains "it says so" "$OUT" "OK: the datapath is torn down"
  assert_contains "it deletes every datapath pod" "$(cat "$STATE/kubectl.log")" \
    "delete pods -n openstack -l app.kubernetes.io/name=chaos-datapath --ignore-not-found"
  run_dp -- teardown gone-ovn
  assert_eq "a central that is gone does not fail it" "0" "$RC"
  assert_contains "and the switch delete reports why it did nothing" "$OUT" \
    "OVNCentral gone-ovn publishes no northbound address"
  run_dp -- dump
  assert_eq "dump of nothing exits 0" "0" "$RC"
}

# --- Test 6 ---------------------------------------------------------------
test_usage_errors() {
  echo "Test: usage errors exit 2"
  reset
  run_dp -- ping-verdict s1
  assert_eq "ping-verdict without a pair exits 2" "2" "$RC"
  run_dp -- ping-dark a
  assert_eq "ping-dark without seconds exits 2" "2" "$RC"
  run_dp -- ping-dark a soon
  assert_eq "ping-dark with a word for seconds exits 2" "2" "$RC"
  run_dp -- ping-verdict s1 'A;B'
  assert_eq "an invalid pair exits 2" "2" "$RC"
  run_dp -- plug w1 lsp-a
  assert_eq "plug with two of six arguments exits 2" "2" "$RC"
  run_dp -- nb
  assert_eq "nb without a central exits 2" "2" "$RC"
  run_dp -- ping-flood a
  assert_eq "an unknown command exits 2" "2" "$RC"
  run_dp NAMESPACE= -- ping-verdict s1 a
  assert_eq "a missing NAMESPACE exits 2" "2" "$RC"
  assert_contains "and says so" "$OUT" "NAMESPACE is not set"
}

# --- Test 7 ---------------------------------------------------------------
test_the_ctl_pod() {
  echo "Test: nb prints the command's stdout and adds the default timeout"
  reset
  echo ssl:10.96.0.5:6641 >"$STATE/addr-c1"
  printf '%s\n' "warn line" "@@OUTPUT@@" row1 row2 >"$STATE/ctl-log"
  echo Succeeded >"$STATE/phase"
  run_dp -- nb c1 --bare --columns=name list Logical_Switch
  assert_eq "a pod that succeeded exits 0" "0" "$RC"
  assert_eq "it prints exactly the rows after the marker" "$(printf 'row1\nrow2')" "$OUT"
  assert_eq "the pod runs the script with the arguments behind --timeout=10" \
    '["bash","-c","_","--timeout=10","--bare","--columns=name","list","Logical_Switch"]' \
    "$(applied '.spec.containers[0].command | del(.[2]) | tojson')"
  assert_eq "the pod runs ovn-nbctl against the published address" \
    "TOOL=ovn-nbctl DB=ssl:10.96.0.5:6641 ATTEMPTS=30" \
    "$(applied '[.spec.containers[0].env[] | select(.name != "RETRYABLE") | "\(.name)=\(.value)"] | join(" ")')"
  assert_eq "with the central's client certificate" "c1-client" "$(applied '.spec.volumes[0].secret.secretName')"
  assert_contains "it reads the Northbound address" "$(kubectl_log)" \
    "get ovncentral c1 -n openstack -o jsonpath={.status.northbound.internalDbAddress}"
  assert_contains "and deletes the pod" "$(kubectl_log)" "delete pod chaos-dp-nb-"

  echo "Test: sb keeps a timeout an argument sets"
  reset
  echo ssl:10.96.0.6:6642 >"$STATE/addr-c1"
  : >"$STATE/ctl-log"
  echo Succeeded >"$STATE/phase"
  run_dp -- sb c1 --timeout=30 --wait=sb sync
  assert_eq "it exits 0" "0" "$RC"
  assert_eq "and prints nothing for an empty output" "" "$OUT"
  assert_eq "the arguments carry only the caller's timeout" '["--timeout=30","--wait=sb","sync"]' \
    "$(applied '.spec.containers[0].command[4:] | tojson')"
  assert_eq "the pod runs ovn-sbctl" "ovn-sbctl" "$(applied '.spec.containers[0].env[0].value')"
  assert_contains "against the Southbound address" "$(kubectl_log)" "jsonpath={.status.southbound.internalDbAddress}"

  echo "Test: nb exits 1 with the log of a failed pod"
  reset
  echo ssl:10.96.0.5:6641 >"$STATE/addr-c1"
  echo "ovn-nbctl exited 3 after 1 attempts: ovn-nbctl: ssl:10.96.0.5:6641: (Protocol error)" >"$STATE/ctl-log"
  echo Failed >"$STATE/phase"
  run_dp -- nb c1 show
  assert_eq "a failed pod exits 1" "1" "$RC"
  assert_contains "it names the command, the pod and the phase" "$OUT" "FAIL: ovn-nbctl --timeout=10 show in pod chaos-dp-nb-"
  assert_contains "the phase" "$OUT" "ended Failed:"
  assert_contains "and prints the log" "$OUT" "ovn-nbctl exited 3 after 1 attempts"

  echo "Test: the pod script retries a connection failure"
  reset
  echo ssl:10.96.0.5:6641 >"$STATE/addr-c1"
  echo Succeeded >"$STATE/phase"
  run_dp -- nb c1 show
  printf '%s\n' refused ok >"$STATE/outcomes"
  run_ctl_pod
  assert_eq "a refused connection and then a success exit 0" "0" "$RC"
  assert_eq "it retried once" "1" "$(grep -c '^retry ' <<<"$OUT")"
  assert_contains "naming the error" "$OUT" "retry 1/30: ovn-nbctl: ssl:10.96.0.5:6641: database connection failed (Connection refused)"
  assert_eq "it prints the stderr, the marker and the rows" \
    "$(printf 'warn line\n@@OUTPUT@@\nrow1\nrow2')" "$(sed -n '/^warn line$/,$p' <<<"$OUT")"
  assert_contains "the tool got the certificates and the arguments" "$(tools_log)" \
    "ovn-nbctl --db=ssl:10.96.0.5:6641 -p /certs/tls.key -c /certs/tls.crt -C /certs/ca.crt --timeout=10 show"
  assert_eq "it ran the tool twice" "2" "$(grep -c '^ovn-nbctl ' "$STATE/tools.log")"

  echo "Test: the pod script does not retry a TLS failure"
  reset
  echo ssl:10.96.0.5:6641 >"$STATE/addr-c1"
  echo Succeeded >"$STATE/phase"
  run_dp -- nb c1 show
  printf '%s\n' tls ok >"$STATE/outcomes"
  run_ctl_pod
  assert_eq "it exits with the tool's status" "3" "$RC"
  assert_contains "after one attempt" "$OUT" "ovn-nbctl exited 3 after 1 attempts:"
  assert_not_contains "without a retry" "$OUT" "retry "
  assert_eq "it ran the tool once" "1" "$(grep -c '^ovn-nbctl ' "$STATE/tools.log")"

  echo "Test: the pod script stops after the last attempt"
  reset
  echo ssl:10.96.0.5:6641 >"$STATE/addr-c1"
  echo Succeeded >"$STATE/phase"
  run_dp CHAOS_DATAPATH_ATTEMPTS=2 -- nb c1 show
  printf '%s\n' refused refused ok >"$STATE/outcomes"
  run_ctl_pod
  assert_eq "two refused attempts of two exit 1" "1" "$RC"
  assert_contains "it says so" "$OUT" "ovn-nbctl exited 1 after 2 attempts:"
  assert_eq "it ran the tool twice" "2" "$(grep -c '^ovn-nbctl ' "$STATE/tools.log")"
}

# --- Test 8 ---------------------------------------------------------------
test_plug_and_teardown() {
  echo "Test: plug attaches the port and records it once"
  reset
  run_dp -- plug w1 lsp-a dpp-a dp-a 0a:00:0a:61:00:0a 10.97.0.10
  assert_eq "it exits 0" "0" "$RC"
  assert_contains "it says so" "$OUT" "OK: lsp-a answers as 10.97.0.10 in dp-a on w1"
  assert_contains "it adds the internal port with the iface-id" "$(tools_log)" \
    "ovs-vsctl --may-exist add-port br-int dpp-a -- set Interface dpp-a type=internal external_ids:iface-id=lsp-a"
  assert_contains "it moves the port into the namespace" "$(tools_log)" "ip link set dpp-a netns dp-a"
  assert_contains "with its MAC" "$(tools_log)" "ip netns exec dp-a ip link set dpp-a address 0a:00:0a:61:00:0a"
  assert_contains "and its address" "$(tools_log)" "ip netns exec dp-a ip addr replace 10.97.0.10/24 dev dpp-a"
  assert_contains "it records the port in the annotation" "$(kubectl_log)" \
    "annotate pod chaos-dp-w1 -n openstack --overwrite chaos.c5c3.io/plugs=dpp-a:dp-a"

  : >"$STATE/kubectl.log"
  run_dp -- plug w1 lsp-a dpp-a dp-a 0a:00:0a:61:00:0a 10.97.0.10
  assert_eq "a second plug of the same port exits 0" "0" "$RC"
  assert_not_contains "and does not annotate again" "$(kubectl_log)" "annotate"

  run_dp -- plug w1 lsp-b dpp-b dp-b 0a:00:0a:61:00:0b 10.97.0.11
  assert_contains "a second port joins the record" "$(kubectl_log)" \
    "annotate pod chaos-dp-w1 -n openstack --overwrite chaos.c5c3.io/plugs=dpp-a:dp-a dpp-b:dp-b"

  reset
  touch "$STATE/execfail"
  run_dp -- plug w1 lsp-a dpp-a dp-a 0a:00:0a:61:00:0a 10.97.0.10
  assert_eq "a failing plug exits 1" "1" "$RC"
  assert_contains "naming the port, the namespace and the node" "$OUT" "FAIL: plugging lsp-a into dp-a on w1 failed"
  assert_not_contains "and records nothing" "$(kubectl_log)" "annotate"

  echo "Test: teardown removes every recorded port and namespace"
  reset
  echo pod/chaos-dp-w1 >"$STATE/probe-pods"
  printf '%s' "dpp-a:dp-a dpp-b:dp-b" >"$STATE/plugs"
  run_dp -- teardown
  assert_eq "it exits 0" "0" "$RC"
  assert_contains "it deletes the first namespace" "$(tools_log)" "ip netns del dp-a"
  assert_contains "and its port" "$(tools_log)" "ovs-vsctl --timeout=5 --if-exists del-port br-int dpp-a"
  assert_contains "the second namespace" "$(tools_log)" "ip netns del dp-b"
  assert_contains "and its port" "$(tools_log)" "ovs-vsctl --timeout=5 --if-exists del-port br-int dpp-b"

  touch "$STATE/execfail"
  run_dp -- teardown
  assert_eq "a failing exec does not fail it" "0" "$RC"
  assert_contains "it still deletes the pods" "$(kubectl_log)" \
    "delete pods -n openstack -l app.kubernetes.io/name=chaos-datapath --ignore-not-found"
}

# --- Test 9 ---------------------------------------------------------------
test_each_ping_holds_its_own_identifier() {
  echo "Test: ping-start gives each ping an ICMP identifier no other ping pod holds"
  reset
  run_dp -- ping-start cp-victim w1 dp-cp 10.97.0.11
  assert_eq "the first ping starts" "0" "$RC"
  assert_contains "it names its identifier" "$OUT" \
    "OK: chaos-dp-ping-cp-victim pings 10.97.0.11 from dp-cp on w1 with ICMP id 1"
  assert_eq "it pings with identifier 1" \
    '["ip","netns","exec","dp-cp","ping","-D","-O","-i","0.5","-W","1","-e","1","10.97.0.11"]' \
    "$(applied '.spec.containers[0].command | tojson')"
  assert_eq "and records it on the pod" "1" "$(applied '.metadata.annotations["chaos.c5c3.io/icmp-id"]')"
  assert_contains "it lists the identifiers of the ping pods" "$(kubectl_log)" \
    "get pods -n openstack -l app.kubernetes.io/name=chaos-datapath,chaos.c5c3.io/datapath=ping"

  printf '%s\n' 1 >"$STATE/icmp-ids"
  run_dp -- ping-start cp-survivor w1 dp-cp 10.97.0.12
  assert_eq "a second ping from the same namespace starts" "0" "$RC"
  assert_eq "with identifier 2" "2" "$(applied '.spec.containers[0].command[12]')"
  assert_eq "recorded on its pod" "2" "$(applied '.metadata.annotations["chaos.c5c3.io/icmp-id"]')"

  printf '%s\n' 1 '' 3 2 >"$STATE/icmp-ids"
  run_dp -- ping-start victim-new w1 dp-victim 10.97.0.13
  assert_eq "it skips every identifier held, in any order" "4" "$(applied '.spec.containers[0].command[12]')"

  printf '%s\n' 2 3 >"$STATE/icmp-ids"
  run_dp -- ping-start cp-victim w1 dp-cp 10.97.0.11
  assert_eq "a freed identifier is taken again" "1" "$(applied '.spec.containers[0].command[12]')"

  rm -f "$STATE/applied"
  touch "$STATE/listfail"
  run_dp -- ping-start cp-victim w1 dp-cp 10.97.0.11
  assert_eq "a failing list exits 1" "1" "$RC"
  assert_contains "it says so" "$OUT" "FAIL: listing the ping pods in openstack failed"
  assert_eq "and starts no ping" "false" "$([ -e "$STATE/applied" ] && echo true || echo false)"

  echo "Test: a probe pod carries no annotation"
  reset
  run_dp -- probe w1
  assert_eq "it exits 0" "0" "$RC"
  assert_eq "its manifest holds an empty annotation object" "{}" "$(applied '.metadata.annotations | tojson')"
}

# --- Run ---
test_the_verdict_against_the_budget
test_the_verdict_refuses_an_empty_record
test_the_dark_window
test_the_wait_for_a_reply
test_teardown_tolerates_missing_pods
test_usage_errors
test_the_ctl_pod
test_plug_and_teardown
test_each_ping_holds_its_own_identifier

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
