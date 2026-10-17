#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the fault phases and the probes of the suites that inject HTTPChaos,
# TimeChaos, IOChaos and StressChaos (#1391):
#   1. tests/e2e-chaos/lib/faults.sh, against a stub kubectl: the @NAME@
#      substitution and its refusal of a placeholder without a value; the
#      apply and the AllInjected wait; the delete and the wait for it; the
#      suite-wide lift; the state read; the canary line; the dd canary,
#      timed beyond an empty exec, the cgroup canary and the OOM evidence on
#      their pass and fail paths; the probe pod's label, securityContext and verdict; the
#      sentinel wait on a match, a FAIL line, an ended pod and the deadline.
#   2. tests/e2e-chaos/lib/probe.py, against a stub HTTP server: expect on a
#      status, on no status and on no answer, its canary line, its refusal of
#      a 2xx, its retry window, its headers and its token, issued after
#      retries, refused or not at all; cinder with a token and with noauth
#      on a status it waits for, a status it must never see, a hold that
#      breaks, a hold through an unanswered read, a hold no read confirms, a
#      volume that vanishes, a backup it creates and watches, and a delete.
#
# Usage: bash tests/unit/ci/chaos_faults_lib_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
LIB="$PROJECT_ROOT/tests/e2e-chaos/lib/faults.sh"
PROBE_PY="$PROJECT_ROOT/tests/e2e-chaos/lib/probe.py"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
STUB_PID=""
cleanup() {
  if [ -n "$STUB_PID" ]; then
    kill "$STUB_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT
mkdir -p "$TMP_DIR/bin" "$TMP_DIR/cg"

# The stub kubectl appends every call to calls and the stdin of an apply to
# applied. A delete exits STUB_DELETE_RC. Every exec sleeps STUB_EXEC_DELAY
# seconds first, then dd behaves as STUB_DD says (slow, fast, enospc, eio,
# and cat reads cg/<file>. The restartCount and the last termination reason of
# a container are STUB_RESTARTS and STUB_REASON, the phase of a pod is
# STUB_PHASE and its log the file log, and a run writes its arguments to run.
# STUB_APPLY_RC fails the apply.
cat >"$TMP_DIR/bin/kubectl" <<'SH'
#!/bin/bash
D="$STUB_DIR"
echo "$*" >>"$D/calls"
case "$1" in
  apply)
    cat >>"$D/applied"
    exit "${STUB_APPLY_RC:-0}"
    ;;
  delete)
    exit "${STUB_DELETE_RC:-0}"
    ;;
  exec)
    sleep "${STUB_EXEC_DELAY:-0}"
    case "$*" in
      *" -- dd "*)
        case "${STUB_DD:-fast}" in
          slow) sleep 1.1; echo "10+0 records out" >&2 ;;
          fast) echo "10+0 records out" >&2 ;;
          enospc) echo "dd: error writing '/x': No space left on device" >&2; exit 1 ;;
          eio) echo "dd: error writing '/x': Input/output error" >&2; exit 1 ;;
          eio-musl) echo "dd: error writing '/x': I/O error" >&2; exit 1 ;;
        esac
        ;;
      *" -- cat /sys/fs/cgroup/"*)
        all="$*"
        f="${all##*/sys/fs/cgroup/}"
        if [ ! -f "$D/cg/$f" ]; then
          echo "cat: /sys/fs/cgroup/$f: No such file or directory" >&2
          exit 1
        fi
        cat "$D/cg/$f"
        ;;
    esac
    ;;
  get)
    case "$*" in
      *restartCount*) printf '%s' "${STUB_RESTARTS:-}" ;;
      *terminated.reason*) printf '%s' "${STUB_REASON:-}" ;;
      *"{.status.phase}"*) printf '%s' "${STUB_PHASE:-}" ;;
    esac
    ;;
  logs)
    cat "$D/log" 2>/dev/null
    ;;
  run)
    printf '%s\n' "$*" >"$D/run"
    ;;
esac
exit 0
SH
chmod +x "$TMP_DIR/bin/kubectl"

# run_lib <VAR=value...> -- <shell code>: run code with the library sourced
# under set -euo pipefail, NAMESPACE=ns and the stub first on PATH, from the
# scratch directory; OUT and RC hold the result.
run_lib() {
  local envs=()
  while [ "$1" != "--" ]; do
    envs+=("$1")
    shift
  done
  shift
  RC=0
  OUT="$(cd "$TMP_DIR" && env PATH="$TMP_DIR/bin:$PATH" STUB_DIR="$TMP_DIR" NAMESPACE=ns \
    ${envs[@]+"${envs[@]}"} bash -c "set -euo pipefail; . '$LIB'; $1" 2>&1)" || RC=$?
}

# new_stub: forget the calls, the applied objects, the log and the cgroup.
new_stub() {
  rm -f "$TMP_DIR/calls" "$TMP_DIR/applied" "$TMP_DIR/log" "$TMP_DIR"/cg/*
  touch "$TMP_DIR/calls"
}

calls() {
  cat "$TMP_DIR/calls"
}

cat >"$TMP_DIR/chaos.yaml" <<'EOF'
kind: TimeChaos
metadata:
  name: skew
spec:
  selector:
    pods:
      keystone-system: ["@LEADER@"]
  timeOffset: "@OFFSET@"
EOF

# --- Test 1 ---------------------------------------------------------------
test_fault_render_fills_every_placeholder() {
  echo "Test: fault_render fills every @NAME@ and refuses one without a value"
  new_stub
  run_lib -- 'fault_render chaos.yaml LEADER=op-0 OFFSET=26h'
  assert_eq "it renders" "0" "$RC"
  assert_contains "the pod is filled in" "$OUT" 'keystone-system: ["op-0"]'
  assert_contains "the offset is filled in" "$OUT" 'timeOffset: "26h"'
  assert_not_contains "no placeholder is left" "$OUT" "@"

  run_lib -- 'fault_render chaos.yaml LEADER=op-0'
  assert_eq "a placeholder without a value fails" "1" "$RC"
  assert_contains "and names it" "$OUT" "FAIL: chaos.yaml: no value for @OFFSET@"

  printf 'kind: HTTPChaos\n' >"$TMP_DIR/plain.yaml"
  run_lib -- 'fault_render plain.yaml'
  assert_eq "a file without placeholders renders as it is" "kind: HTTPChaos" "$OUT"
}

# --- Test 2 ---------------------------------------------------------------
test_fault_inject_applies_and_waits_for_all_injected() {
  echo "Test: fault_inject applies the rendered object and waits for AllInjected"
  new_stub
  run_lib -- 'fault_inject chaos.yaml timechaos/skew LEADER=op-1 OFFSET=1h'
  assert_eq "it returns 0" "0" "$RC"
  assert_contains "it applies in the namespace from stdin" "$(calls)" "apply -n ns -f -"
  assert_contains "the applied object is the rendered one" "$(cat "$TMP_DIR/applied")" 'timeOffset: "1h"'
  assert_contains "it waits 120 s for AllInjected" "$(calls)" \
    "wait timechaos/skew -n ns --for=condition=AllInjected --timeout=120s"

  new_stub
  run_lib STUB_APPLY_RC=1 -- 'fault_inject chaos.yaml timechaos/skew LEADER=op-1 OFFSET=1h'
  assert_eq "a failed apply fails" "1" "$RC"
  assert_not_contains "and waits for nothing" "$(calls)" "wait timechaos/skew"

  new_stub
  run_lib -- 'fault_inject chaos.yaml timechaos/skew LEADER=op-1'
  assert_eq "a render that fails applies nothing" "1" "$RC"
  assert_eq "no kubectl call was made" "" "$(calls)"
}

# --- Test 3 ---------------------------------------------------------------
test_fault_lift_deletes_and_waits_for_the_delete() {
  echo "Test: fault_lift deletes the object and waits until it is gone"
  new_stub
  run_lib -- 'fault_lift iochaos/io-eio'
  assert_eq "it returns 0" "0" "$RC"
  assert_eq "it deletes, then waits" \
    "$(printf '%s\n' \
      'delete iochaos/io-eio -n ns --ignore-not-found --wait=true --timeout=120s' \
      'wait iochaos/io-eio -n ns --for=delete --timeout=120s')" \
    "$(calls)"

  new_stub
  run_lib STUB_DELETE_RC=1 -- 'fault_lift iochaos/io-eio'
  assert_eq "a delete that fails fails" "1" "$RC"
  assert_not_contains "and waits for nothing" "$(calls)" "wait iochaos/io-eio"

  new_stub
  run_lib -- 'fault_lift_suite mariadb-io-fault'
  assert_eq "the suite-wide lift returns 0" "0" "$RC"
  assert_eq "it prints, then deletes, every kind by the suite label" \
    "$(printf '%s\n' \
      'get podchaos,networkchaos,httpchaos,timechaos,iochaos,stresschaos,dnschaos -n ns -l chaos.c5c3.io/suite=mariadb-io-fault -o yaml' \
      'delete podchaos,networkchaos,httpchaos,timechaos,iochaos,stresschaos,dnschaos -n ns -l chaos.c5c3.io/suite=mariadb-io-fault --ignore-not-found --wait=true --timeout=120s')" \
    "$(calls)"
  run_lib STUB_DELETE_RC=1 -- 'fault_lift_suite mariadb-io-fault'
  assert_eq "a delete that fails fails the lift" "1" "$RC"
}

# --- Test 4 ---------------------------------------------------------------
test_canary_fail_names_the_object() {
  echo "Test: canary_fail prints the canary line and returns 1"
  run_lib -- 'canary_fail timechaos/skew "the Lease renewTime is 2 s from the runner clock"'
  assert_eq "it returns 1" "1" "$RC"
  assert_eq "it prints the line" \
    "FAIL: timechaos/skew reports AllInjected but the Lease renewTime is 2 s from the runner clock" "$OUT"
}

# --- Test 5 ---------------------------------------------------------------
test_canary_dd_on_each_mode() {
  echo "Test: canary_dd passes a slow or refused write and fails a write the fault missed"
  new_stub
  run_lib STUB_DD=slow -- 'canary_dd iochaos/lat LATENCY db-0 mariadb /var/lib/mysql/.chaos-canary latency'
  assert_eq "a write that takes over 1 s passes the latency canary" "0" "$RC"
  assert_contains "and prints the sentinel" "$OUT" "INJECTED-LATENCY-OK"
  assert_contains "dd runs in the container, synced" "$(calls)" \
    "exec db-0 -n ns -c mariadb -- dd if=/dev/zero of=/var/lib/mysql/.chaos-canary bs=4k count=10 conv=fsync"
  assert_contains "after an empty exec into the same container" "$(calls)" \
    "exec db-0 -n ns -c mariadb -- true"

  run_lib STUB_DD=fast -- 'canary_dd iochaos/lat LATENCY db-0 mariadb /f latency'
  assert_eq "a fast write fails the latency canary" "1" "$RC"
  assert_contains "with the canary line" "$OUT" "FAIL: iochaos/lat reports AllInjected but dd wrote 40 KiB to /f in"

  run_lib STUB_EXEC_DELAY=1.1 STUB_DD=fast -- 'canary_dd iochaos/lat LATENCY db-0 mariadb /f latency'
  assert_eq "a slow exec round trip alone fails the latency canary" "1" "$RC"
  assert_contains "and the line names the empty exec" "$OUT" "beyond an empty exec's"

  run_lib STUB_DD=eio -- 'canary_dd iochaos/lat LATENCY db-0 mariadb /f latency'
  assert_eq "a failed write fails the latency canary" "1" "$RC"
  assert_contains "and says it could not write" "$OUT" "FAIL: the latency canary could not write /f"

  run_lib STUB_DD=enospc -- 'canary_dd iochaos/full ENOSPC db-0 mariadb /f enospc'
  assert_eq "No space left on device passes the enospc canary" "0" "$RC"
  assert_contains "and prints the sentinel" "$OUT" "INJECTED-ENOSPC-OK"

  run_lib STUB_DD=fast -- 'canary_dd iochaos/full ENOSPC db-0 mariadb /f enospc'
  assert_eq "a write that succeeds fails the enospc canary" "1" "$RC"
  assert_contains "with the canary line" "$OUT" "FAIL: iochaos/full reports AllInjected but dd wrote 40 KiB to /f"

  run_lib STUB_DD=enospc -- 'canary_dd iochaos/eio EIO db-0 mariadb /f eio'
  assert_eq "another error fails the eio canary" "1" "$RC"
  assert_contains "and quotes it" "$OUT" "but dd failed without 'Input/output error|I/O error'"

  run_lib STUB_DD=eio -- 'canary_dd iochaos/eio EIO db-0 mariadb /f eio'
  assert_eq "Input/output error passes the eio canary" "0" "$RC"
  run_lib STUB_DD=eio-musl -- 'canary_dd iochaos/eio EIO openbao-0 openbao /f eio'
  assert_eq "musl's I/O error passes the eio canary" "0" "$RC"
  assert_contains "and prints the sentinel" "$OUT" "INJECTED-EIO-OK"

  run_lib -- 'canary_dd iochaos/x X db-0 mariadb /f eios'
  assert_eq "an unknown mode is a usage error" "2" "$RC"
}

# --- Test 6 ---------------------------------------------------------------
test_cgroup_memory_reads_and_refuses() {
  echo "Test: cgroup_memory reads a value and fails on a missing file or no limit"
  new_stub
  printf '2147483648\n' >"$TMP_DIR/cg/memory.max"
  printf 'low 0\nhigh 0\nmax 4\noom 2\noom_kill 3\noom_group_kill 0\n' >"$TMP_DIR/cg/memory.events"
  run_lib -- 'cgroup_memory b-0 backup memory.max backup'
  assert_eq "memory.max is read" "2147483648" "$OUT"
  run_lib -- 'cgroup_memory b-0 backup oom_kill backup'
  assert_eq "oom_kill comes from memory.events" "3" "$OUT"
  run_lib -- 'cgroup_memory b-0 backup memory.current backup'
  assert_eq "a missing file fails" "1" "$RC"
  assert_contains "with its line" "$OUT" "FAIL: no cgroup v2 memory files in the backup container"
  printf 'max\n' >"$TMP_DIR/cg/memory.max"
  run_lib -- 'cgroup_memory b-0 backup memory.max backup'
  assert_eq "an unlimited container fails" "1" "$RC"
  assert_eq "with its line" "FAIL: the backup container has no memory limit" "$OUT"
  printf 'n/a\n' >"$TMP_DIR/cg/memory.current"
  run_lib -- 'cgroup_memory b-0 backup memory.current backup'
  assert_eq "a value that is no number fails" "1" "$RC"
  assert_eq "with its line" "FAIL: memory.current of the backup container reads 'n/a', not a number" "$OUT"
}

# --- Test 6b --------------------------------------------------------------
test_oom_seen_reads_the_counter_and_the_restart() {
  echo "Test: oom_seen sees a risen oom_kill or an OOMKilled restart, and nothing else"
  new_stub
  printf 'oom 3\noom_kill 3\n' >"$TMP_DIR/cg/memory.events"
  run_lib -- 'oom_seen b-0 backup 2 0'
  assert_eq "a risen counter is an OOM kill" "0" "$RC"
  assert_eq "and says so" "oom_kill rose from 2 to 3" "$OUT"

  run_lib STUB_RESTARTS=1 STUB_REASON=OOMKilled -- 'oom_seen b-0 backup 3 0'
  assert_eq "a restart after an OOM kill is one too" "0" "$RC"
  assert_eq "and says so" "the backup container restarted after an OOM kill" "$OUT"

  rm -f "$TMP_DIR/cg/memory.events"
  run_lib STUB_RESTARTS=1 STUB_REASON=OOMKilled -- 'oom_seen b-0 backup 3 0'
  assert_eq "it is one when the restarted cgroup cannot be read" "0" "$RC"

  printf 'oom_kill 3\n' >"$TMP_DIR/cg/memory.events"
  run_lib STUB_RESTARTS=1 STUB_REASON=Error -- 'oom_seen b-0 backup 3 0'
  assert_eq "a restart for another reason is none" "1" "$RC"
  assert_eq "and both readings are printed" "oom_kill stayed at 3 and the container at restartCount 1" "$OUT"
  assert_contains "the restartCount is the named container's" "$(calls)" \
    'get pod b-0 -n ns -o jsonpath={.status.containerStatuses[?(@.name=="backup")].restartCount}'
}

# --- Test 6c --------------------------------------------------------------
test_state_reads_the_suite_configmap() {
  echo "Test: state reads a key of the ConfigMap STATE names"
  new_stub
  run_lib -- 'STATE=suite-state; state leader-sid'
  assert_eq "it returns 0" "0" "$RC"
  assert_eq "it reads the key from the ConfigMap in the namespace" \
    "get configmap suite-state -n ns -o jsonpath={.data.leader-sid}" "$(calls)"
}

# --- Test 7 ---------------------------------------------------------------
test_probe_run_starts_a_confined_pod_and_reads_its_verdict() {
  echo "Test: probe_run starts the probe pod, prints its log and returns its verdict"
  new_stub
  printf 'FAILCLOSED-OK\n' >"$TMP_DIR/log"
  run_lib STUB_PHASE=Succeeded -- 'probe_run p1 10 --env OS_PASSWORD=pw -- python3 -u -c "print(1)" expect'
  assert_eq "a pod that Succeeded passes" "0" "$RC"
  assert_contains "the log is printed" "$OUT" "FAILCLOSED-OK"
  assert_contains "and the phase" "$OUT" "pod p1 phase: Succeeded"
  local run
  run="$(cat "$TMP_DIR/run")"
  assert_contains "it runs on the Tempest image" "$run" "--image=ghcr.io/c5c3/tempest:2026.1 --restart=Never"
  assert_contains "with the probe label" "$run" "--labels=app.kubernetes.io/name=chaos-probe"
  assert_contains "with the env" "$run" "--env OS_PASSWORD=pw"
  assert_contains "as a non-root user" "$run" '"runAsNonRoot": true'
  assert_contains "without a token" "$run" '"automountServiceAccountToken": false'
  assert_contains "and the command" "$run" "--command -- python3 -u -c print(1) expect"
  assert_contains "a leftover pod is deleted first" "$(head -1 "$TMP_DIR/calls")" "delete pod p1 -n ns --ignore-not-found"

  run_lib STUB_PHASE=Failed -- 'probe_run p1 10 -- python3 -c x'
  assert_eq "a pod that Failed fails" "1" "$RC"
  run_lib STUB_PHASE=Running -- 'probe_run p1 0 -- python3 -c x'
  assert_eq "a pod that never ends fails at the deadline" "1" "$RC"
  run_lib -- 'probe_run p1 10 --env -- x'
  assert_eq "an --env without a value is a usage error" "2" "$RC"
}

# --- Test 8 ---------------------------------------------------------------
test_wait_sentinel_paths() {
  echo "Test: wait_sentinel returns the matching line and fails on FAIL, an ended pod or the deadline"
  new_stub
  printf 'EXPIRED-REJECTED 404\nBEHIND-OFFSET -3601\n' >"$TMP_DIR/log"
  run_lib STUB_PHASE=Running -- 'wait_sentinel probe "^BEHIND-OFFSET " 5'
  assert_eq "a matching line passes" "0" "$RC"
  assert_eq "and is printed" "BEHIND-OFFSET -3601" "$OUT"

  printf 'FAIL: BEHIND-OFFSET -20\n' >"$TMP_DIR/log"
  run_lib STUB_PHASE=Running -- 'wait_sentinel probe "^BEHIND-ACCEPTS-EXPIRED " 5'
  assert_eq "a FAIL line fails" "1" "$RC"
  assert_contains "and the log is printed" "$OUT" "FAIL: BEHIND-OFFSET -20"

  printf 'PRE-ISSUED\n' >"$TMP_DIR/log"
  run_lib STUB_PHASE=Failed -- 'wait_sentinel probe "^AHEAD-OFFSET " 5'
  assert_eq "an ended pod fails" "1" "$RC"
  assert_contains "with its phase" "$OUT" "FAIL: probe reached Failed without a line matching ^AHEAD-OFFSET "

  run_lib STUB_PHASE=Running -- 'wait_sentinel probe "^AHEAD-OFFSET " 0'
  assert_eq "the deadline fails" "1" "$RC"
  assert_contains "and says so" "$OUT" "FAIL: no line matching ^AHEAD-OFFSET  from probe within 0s"
}

# ---------------------------------------------------------------------------
# probe.py against a stub server
# ---------------------------------------------------------------------------

cat >"$TMP_DIR/stub_server.py" <<'PY'
"""A stub Keystone, a stub target and a stub Cinder on one port."""
import json
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

STATE = {"flip": 0, "flaky": 0, "statuses": ["available"], "polls": 0,
         "deleted": False}
LOCK = threading.Lock()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def send(self, code, body=b"", headers=None):
        self.send_response(code)
        for key, value in (headers or {}).items():
            self.send_header(key, value)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def issue(self):
        body = json.dumps({"token": {"project": {"id": "p1"}}}).encode()
        self.send(201, body, {"X-Subject-Token": "tok"})

    def noauth(self):
        return self.headers.get("X-Auth-Token") == "admin:admin"

    # status <kind>: the next of the statuses; GONE answers 404 and HTTP503
    # a 503 without a body.
    def status(self, kind):
        with LOCK:
            n = min(STATE["polls"], len(STATE["statuses"]) - 1)
            STATE["polls"] += 1
            status = "GONE" if STATE["deleted"] else STATE["statuses"][n]
        if status == "GONE":
            self.send(404)
        elif status == "HTTP503":
            self.send(503)
        else:
            self.send(200, json.dumps({kind: {"status": status}}).encode())

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        data = self.rfile.read(length)
        if self.path == "/v3/auth/tokens":
            self.issue()
        elif self.path == "/bad/v3/auth/tokens":
            self.send(401)
        elif self.path == "/down/v3/auth/tokens":
            self.send(503)
        elif self.path == "/flaky/v3/auth/tokens":
            with LOCK:
                STATE["flaky"] += 1
                up = STATE["flaky"] > 2
            if up:
                self.issue()
            else:
                self.send(503)
        elif self.path == "/cinder/v3/backups" and self.noauth():
            ok = json.loads(data)["backup"].get("volume_id") == "v1"
            self.send(202 if ok else 400,
                      json.dumps({"backup": {"id": "b1"}}).encode())
        elif self.path == "/_control/statuses":
            with LOCK:
                STATE["statuses"] = json.loads(data)
                STATE["polls"] = 0
                STATE["deleted"] = False
            self.send(204)
        elif self.path.endswith("/volumes"):
            self.send(202, json.dumps({"volume": {"id": "v1"}}).encode())
        else:
            self.send(404)

    def do_DELETE(self):
        with LOCK:
            STATE["deleted"] = True
        self.send(202)

    def do_PUT(self):
        time.sleep(3)
        self.send(200)

    def do_GET(self):
        if self.path.startswith("/code/"):
            self.send(int(self.path.rsplit("/", 1)[1]))
        elif self.path == "/close":
            self.close_connection = True
            self.connection.close()
        elif self.path == "/subject":
            ok = (self.headers.get("X-Auth-Token") == "tok"
                  and self.headers.get("X-Subject-Token") == "tok")
            self.send(200 if ok else 401)
        elif self.path == "/header":
            ok = self.headers.get("OpenStack-API-Version") == "placement 1.10"
            self.send(200 if ok else 400)
        elif self.path == "/flip":
            with LOCK:
                STATE["flip"] += 1
                code = 503 if STATE["flip"] < 3 else 200
            self.send(code)
        elif self.path.startswith("/cinder/v3/p1/volumes/v1"):
            self.status("volume")
        elif self.path.startswith("/cinder/v3/backups/b1") and self.noauth():
            self.status("backup")
        elif (self.path.startswith("/cinder/v3/p1/volumes?name=")
              or self.path.startswith("/cinder/v3/volumes?name=")
              and self.noauth()):
            body = {"volumes": [{"id": "v1", "name": self.path.split("=", 1)[1]}]}
            self.send(200, json.dumps(body).encode())
        else:
            self.send(404)


server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
print(server.server_address[1], flush=True)
server.serve_forever()
PY

STUB_PORT=""
start_stub() {
  python3 "$TMP_DIR/stub_server.py" >"$TMP_DIR/stub.port" 2>"$TMP_DIR/stub.err" &
  STUB_PID=$!
  local _
  for _ in $(seq 1 50); do
    STUB_PORT="$(head -1 "$TMP_DIR/stub.port" 2>/dev/null || true)"
    [ -n "$STUB_PORT" ] && return 0
    sleep 0.1
  done
  echo "  FAIL: the stub server did not start: $(cat "$TMP_DIR/stub.err")"
  FAIL=$((FAIL + 1))
  return 1
}

# probe <args...>: run probe.py the way a probe pod runs it, with
# POLL_INTERVAL cut to 0.2 s and TOKEN_SECONDS to 1 s, and OS_PASSWORD from
# PROBE_PASSWORD (pw when unset); POUT and PRC hold the result.
probe() {
  PRC=0
  POUT="$(OS_PASSWORD="${PROBE_PASSWORD-pw}" python3 -c "$(sed -e 's/^POLL_INTERVAL = 5$/POLL_INTERVAL = 0.2/' \
    -e 's/^TOKEN_SECONDS = 120$/TOKEN_SECONDS = 1/' "$PROBE_PY")" "$@" 2>&1)" || PRC=$?
}

# --- Test 9 --------------------------------------------------------------
test_probe_expect() {
  echo "Test: probe.py expect judges a status, no status, no answer, a 2xx and a retry window"
  local u="http://127.0.0.1:$STUB_PORT"

  probe expect --check "GET $u/code/503 503" --check "GET $u/code/200 200" --ok INJECTED-503-OK
  assert_eq "two answers that match pass" "0" "$PRC"
  assert_contains "and print the sentinel" "$POUT" "INJECTED-503-OK"

  probe expect --canary httpchaos/candidates-503 --check "GET $u/code/200 503" --ok INJECTED-503-OK
  assert_eq "an answer the fault should have changed fails" "1" "$PRC"
  assert_contains "with the canary line" "$POUT" \
    "FAIL: httpchaos/candidates-503 reports AllInjected but GET $u/code/200 answered HTTP 200, want 503"
  assert_not_contains "and no sentinel" "$POUT" "INJECTED-503-OK"

  probe expect --check "GET $u/close none"
  assert_eq "a closed connection is no HTTP status" "0" "$PRC"
  probe expect --check "GET $u/code/500 none"
  assert_eq "a 500 is a status" "1" "$PRC"

  probe expect --body '{"port": {"name": "canary"}}' --check "PUT $u/ports/x silent@1" --check "GET $u/code/200 200@5"
  assert_eq "no answer within the request timeout is silent" "0" "$PRC"
  probe expect --check "GET $u/code/200 silent@1"
  assert_eq "an answer is not silent" "1" "$PRC"

  probe expect --within 5 --refuse-2xx "answered {code} with a token Keystone could not validate" \
    --check "GET $u/code/204 503"
  assert_eq "a 2xx fails at once under --refuse-2xx" "1" "$PRC"
  assert_contains "with the text, the code filled in" "$POUT" \
    "FAIL: answered 204 with a token Keystone could not validate"

  probe expect --within 5 --check "GET $u/flip 200" --ok RECOVERY-OK
  assert_eq "a retry window waits out two 503s" "0" "$PRC"
  assert_contains "and logs each retry" "$POUT" "retry: GET $u/flip answered HTTP 503"

  probe expect --keystone "$u/v3" --subject --check "GET $u/subject 200"
  assert_eq "the token goes out as X-Auth-Token and X-Subject-Token" "0" "$PRC"
  probe expect --keystone "$u/v3" --check "GET $u/subject 200"
  assert_eq "without --subject it is X-Auth-Token alone" "1" "$PRC"

  probe expect --check "GET $u/code/200 maybe"
  assert_eq "a WANT that is no status, none or silent is a usage error" "2" "$PRC"

  probe expect --header "OpenStack-API-Version: placement 1.10" --check "GET $u/header 200"
  assert_eq "a --header goes out with the request" "0" "$PRC"
  probe expect --header "OpenStack-API-Version" --check "GET $u/header 200"
  assert_eq "a --header without a colon is a usage error" "2" "$PRC"
}

# --- Test 9b -------------------------------------------------------------
test_probe_token() {
  echo "Test: probe.py retries a Keystone that answers 5xx and fails on a 4xx, the deadline or no password"
  local u="http://127.0.0.1:$STUB_PORT"

  probe expect --keystone "$u/flaky/v3" --check "GET $u/code/200 200"
  assert_eq "a Keystone that answers 503 twice gets retried" "0" "$PRC"
  assert_contains "and each retry is logged" "$POUT" "retry for the token: HTTP 503"

  probe expect --keystone "$u/bad/v3" --check "GET $u/code/200 200"
  assert_eq "a 4xx for the token fails at once" "1" "$PRC"
  assert_contains "and names it" "$POUT" "FAIL: POST $u/bad/v3/auth/tokens answered HTTP 401"

  probe expect --keystone "$u/down/v3" --check "GET $u/code/200 200"
  assert_eq "a Keystone that keeps answering 503 fails at the deadline" "1" "$PRC"
  assert_contains "and says so" "$POUT" "FAIL: no token from $u/down/v3 within 1s"

  PROBE_PASSWORD="" probe expect --keystone "$u/v3" --check "GET $u/code/200 200"
  assert_eq "an empty OS_PASSWORD fails" "1" "$PRC"
  assert_contains "and says so" "$POUT" "FAIL: OS_PASSWORD is empty; the probe cannot issue a token"
}

# --- Test 10 --------------------------------------------------------------
test_probe_cinder() {
  echo "Test: probe.py cinder waits for a status, refuses a status it must never see, holds and deletes"
  local u="http://127.0.0.1:$STUB_PORT" c="http://127.0.0.1:$STUB_PORT/cinder"
  # statuses <json list>: the statuses the stub volume reports, in turn.
  statuses() {
    python3 -c 'import sys, urllib.request; urllib.request.urlopen(urllib.request.Request(sys.argv[1], data=sys.argv[2].encode(), method="POST"))' \
      "$u/_control/statuses" "$1"
  }

  statuses '["creating", "error"]'
  probe cinder --cinder "$c" --keystone "$u/v3" volume img-abort --create --image i1 \
    --want error --within 5 --hold 1 --never available \
    --never-message "image-to-volume succeeded through an aborted download" --ok FAILCLOSED-OK
  assert_eq "a volume that reads error and stays there passes" "0" "$PRC"
  assert_contains "it is created" "$POUT" "CREATED volume img-abort v1"
  assert_contains "and prints the sentinel" "$POUT" "FAILCLOSED-OK"

  statuses '["creating", "available"]'
  probe cinder --cinder "$c" --keystone "$u/v3" volume img-abort --create --image i1 \
    --want error --within 5 --never available \
    --never-message "image-to-volume succeeded through an aborted download"
  assert_eq "a volume that turns available fails at once" "1" "$PRC"
  assert_contains "with the never message" "$POUT" "FAIL: image-to-volume succeeded through an aborted download"

  statuses '["error", "error", "available"]'
  probe cinder --cinder "$c" --keystone "$u/v3" volume img-abort \
    --want error --within 5 --hold 2 --never available \
    --never-message "image-to-volume succeeded through an aborted download"
  assert_eq "a hold that breaks fails" "1" "$PRC"
  assert_contains "with the never message" "$POUT" "FAIL: image-to-volume succeeded through an aborted download"

  statuses '["error", "HTTP503", "error"]'
  probe cinder --cinder "$c" --keystone "$u/v3" volume img-abort \
    --want error --within 5 --hold 1 --never available --ok FAILCLOSED-OK
  assert_eq "a read Cinder does not answer during the hold changes nothing" "0" "$PRC"
  assert_contains "and is logged" "$POUT" "retry during the hold: unknown (HTTP 503"
  assert_contains "the hold still passes" "$POUT" "FAILCLOSED-OK"

  statuses '["error", "error", "HTTP503", "HTTP503", "HTTP503", "HTTP503", "HTTP503", "HTTP503", "HTTP503", "HTTP503", "error"]'
  probe cinder --cinder "$c" --keystone "$u/v3" volume img-abort \
    --want error --within 5 --hold 1 --never available --ok FAILCLOSED-OK
  assert_eq "a read after the hold's end that names the status ends it" "0" "$PRC"
  assert_contains "the hold passes" "$POUT" "FAILCLOSED-OK"

  statuses '["error", "HTTP503"]'
  probe cinder --cinder "$c" --keystone "$u/v3" volume img-abort \
    --want error --within 1 --hold 1 --never available --ok FAILCLOSED-OK
  assert_eq "a hold no read confirms fails" "1" "$PRC"
  assert_contains "and says so" "$POUT" \
    "FAIL: volume img-abort: no read in the 1s after the 1s hold named a status, want error"
  assert_not_contains "and no sentinel" "$POUT" "FAILCLOSED-OK"

  statuses '["error", "error", "HTTP503"]'
  probe cinder --cinder "$c" --keystone "$u/v3" volume img-abort \
    --want error --within 1 --hold 1 --never available --ok FAILCLOSED-OK
  assert_eq "a hold only its first read confirms fails" "1" "$PRC"
  assert_contains "and says so" "$POUT" \
    "FAIL: volume img-abort: no read in the 1s after the 1s hold named a status, want error"
  assert_not_contains "and no sentinel" "$POUT" "FAILCLOSED-OK"

  statuses '["creating", "error"]'
  probe cinder --cinder "$c" --keystone "$u/v3" volume img-base --create --want available --within 5
  assert_eq "an error when available is wanted fails at once" "1" "$PRC"
  assert_contains "and names it" "$POUT" "FAIL: volume img-base reached error, want available"

  statuses '["creating", "GONE"]'
  probe cinder --cinder "$c" --keystone "$u/v3" volume img-base --want available --within 5
  assert_eq "a volume that vanishes fails at once" "1" "$PRC"
  assert_contains "and is named gone" "$POUT" "FAIL: volume img-base reached gone, want available"

  statuses '["creating", "available"]'
  probe cinder --cinder "$c" backup bk1 --create --volume img-base --watch 5
  assert_eq "a noauth backup create and watch passes" "0" "$PRC"
  assert_contains "the backup is created of the volume it names" "$POUT" "CREATED backup bk1 b1"
  assert_contains "it prints each status change" "$POUT" "STATUS creating"
  assert_contains "and the final one" "$POUT" "STATUS available"

  statuses '["available"]'
  probe cinder --cinder "$c" --keystone "$u/v3" volume img-base --want available --delete --within 5 --ok RECOVERED-OK
  assert_eq "a delete waits for the 404" "0" "$PRC"
  assert_contains "and says so" "$POUT" "DELETED volume img-base"

  probe cinder --cinder "$c" backup b1 --create
  assert_eq "a backup create without --volume is a usage error" "2" "$PRC"
}

# --- Run ---
test_fault_render_fills_every_placeholder
test_fault_inject_applies_and_waits_for_all_injected
test_fault_lift_deletes_and_waits_for_the_delete
test_canary_fail_names_the_object
test_canary_dd_on_each_mode
test_cgroup_memory_reads_and_refuses
test_oom_seen_reads_the_counter_and_the_restart
test_state_reads_the_suite_configmap
test_probe_run_starts_a_confined_pod_and_reads_its_verdict
test_wait_sentinel_paths
if start_stub; then
  test_probe_expect
  test_probe_token
  test_probe_cinder
fi

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
