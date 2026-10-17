#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the suite-local probes and helpers of the fault suites of #1391:
#   1. mariadb-io-fault/write-probe.py, against a stub Keystone: every write
#      that answered 201 is listed afterwards (ACKED-OK), a lost one is named
#      (ACKED-MISSING), a refused one is not counted, and a listing that keeps
#      failing fails.
#   2. keystone-api-clock-skew/token-probe.py, against a stub Keystone whose
#      clock the test moves: every sentinel of the phases on a skew that
#      landed, and the FAIL line of each threshold on one that fell short.
#   3. glance-staging-io-fault/import-probe.py, against a stub Glance: an
#      import that turns active, a stage that is refused and leaves the image
#      queued, a stage that passes under the fault, the stuck-import check and
#      the cleanup across two pages.
#   4. The parsers of the suites' probe-lib.sh files, with a stub kubectl and
#      a stub openssl: quantity_bytes and quantity_millicores, field,
#      lease_holder and both cert_offset_hours.
#
# Usage: bash tests/unit/ci/chaos_suite_probes_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CHAOS="$PROJECT_ROOT/tests/e2e-chaos"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
STUB_PID=""
BG_PID=""
cleanup() {
  for pid in "$BG_PID" "$STUB_PID"; do
    if [ -n "$pid" ]; then
      kill "$pid" 2>/dev/null || true
    fi
  done
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT
mkdir -p "$TMP_DIR/bin"

cat >"$TMP_DIR/stub_server.py" <<'PY'
"""A stub Keystone and a stub Glance on one port, steered through /_control."""
import json
import threading
import time
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOCK = threading.Lock()
STATE = {}


def reset():
    STATE.clear()
    STATE.update({
        # Keystone: the clock offset and the lifetime of the next tokens, the
        # tokens by value with their expiry, and the projects by name.
        "offset": 0, "lifetime": 300, "n": 0, "tokens": {},
        "projects": {}, "drop_next": False, "write_code": 201,
        "list_code": 200,
        # Glance: the answer to a stage, the status a refused stage leaves,
        # and the images by id.
        "stage_code": 204, "refused_status": "queued", "images": {},
    })


reset()


def stamp(t):
    return datetime.fromtimestamp(t, timezone.utc).strftime(
        "%Y-%m-%dT%H:%M:%S.%fZ")


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def send(self, code, body=None, headers=None):
        data = json.dumps(body).encode() if body is not None else b""
        self.send_response(code)
        for key, value in (headers or {}).items():
            self.send_header(key, value)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def body(self):
        length = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(length)

    def do_POST(self):
        data = self.body()
        if self.path == "/_control":
            change = json.loads(data)
            with LOCK:
                if change.pop("reset", False):
                    reset()
                STATE.update(change)
            self.send(204)
        elif self.path == "/v3/auth/tokens":
            with LOCK:
                STATE["n"] += 1
                token = f"t{STATE['n']}"
                issued = time.time() + STATE["offset"]
                expires = issued + STATE["lifetime"]
                STATE["tokens"][token] = expires
            self.send(201, {"token": {
                "issued_at": stamp(issued), "expires_at": stamp(expires),
                "project": {"id": "p1"}}}, {"X-Subject-Token": token})
        elif self.path == "/v3/projects":
            name = json.loads(data)["project"]["name"]
            with LOCK:
                code = STATE["write_code"]
                if code == 201 and not STATE["drop_next"]:
                    STATE["projects"][name] = f"id-{name}"
                STATE["drop_next"] = False
            self.send(code, {"project": {"name": name}})
        elif self.path == "/v2/images":
            name = json.loads(data)["name"]
            with LOCK:
                image = f"img{len(STATE['images']) + 1}"
                STATE["images"][image] = {"id": image, "name": name,
                                          "status": "queued"}
            self.send(201, {"id": image})
        elif self.path.endswith("/import"):
            with LOCK:
                STATE["images"][self.path.split("/")[3]]["status"] = "active"
            self.send(202)
        else:
            self.send(404)

    def do_PUT(self):
        self.body()
        image = self.path.split("/")[3]
        with LOCK:
            code = STATE["stage_code"]
            STATE["images"][image]["status"] = (
                "uploading" if 200 <= code < 300 else STATE["refused_status"])
        self.send(code)

    def do_DELETE(self):
        kind, ident = self.path.split("/")[2:4]
        with LOCK:
            if kind == "projects":
                STATE["projects"] = {n: i for n, i in STATE["projects"].items()
                                     if i != ident}
            else:
                STATE["images"].pop(ident, None)
        self.send(204)

    def do_GET(self):
        if self.path == "/_control/projects":
            with LOCK:
                self.send(200, sorted(STATE["projects"]))
        elif self.path == "/_control/images":
            with LOCK:
                self.send(200, sorted(i["name"] for i in STATE["images"].values()))
        elif self.path == "/v3/auth/tokens":
            with LOCK:
                expires = STATE["tokens"].get(self.headers.get("X-Subject-Token"))
                now = time.time() + STATE["offset"]
            if expires is not None and expires > now:
                self.send(200, {"token": {"expires_at": stamp(expires)}})
            else:
                self.send(404)
        elif self.path == "/v3/projects":
            with LOCK:
                code = STATE["list_code"]
                found = [{"name": n, "id": i} for n, i in STATE["projects"].items()]
            if code != 200:
                self.send(code)
            else:
                self.send(200, {"projects": found + [{"name": "admin", "id": "a"}]})
        elif self.path.startswith("/v2/images?status="):
            want = self.path.split("=", 1)[1]
            with LOCK:
                found = [i for i in STATE["images"].values() if i["status"] == want]
            self.send(200, {"images": found})
        elif self.path == "/v2/images?limit=100":
            # Two pages: the first image, then the rest behind a next link.
            with LOCK:
                found = sorted(STATE["images"].values(), key=lambda i: i["id"])
            self.send(200, {"images": found[:1], "next": "/v2/images?page=2"})
        elif self.path == "/v2/images?page=2":
            with LOCK:
                found = sorted(STATE["images"].values(), key=lambda i: i["id"])
            self.send(200, {"images": found[1:]})
        elif self.path.startswith("/v2/images/"):
            with LOCK:
                image = STATE["images"].get(self.path.split("/")[3])
            self.send(200 if image else 404, image)
        else:
            self.send(404)


server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
print(server.server_address[1], flush=True)
server.serve_forever()
PY

U=""
start_stub() {
  python3 "$TMP_DIR/stub_server.py" >"$TMP_DIR/stub.port" 2>"$TMP_DIR/stub.err" &
  STUB_PID=$!
  local _ port
  for _ in $(seq 1 50); do
    port="$(head -1 "$TMP_DIR/stub.port" 2>/dev/null || true)"
    if [ -n "$port" ]; then
      U="http://127.0.0.1:$port"
      return 0
    fi
    sleep 0.1
  done
  echo "  FAIL: the stub server did not start: $(cat "$TMP_DIR/stub.err")"
  FAIL=$((FAIL + 1))
  return 1
}

# control <json>: change the stub's state; "reset": true starts afresh.
control() {
  python3 -c 'import sys, urllib.request; urllib.request.urlopen(urllib.request.Request(sys.argv[1], data=sys.argv[2].encode(), method="POST"))' \
    "$U/_control" "$1"
}

# stub_get <what>: the project or image names the stub holds, as JSON.
stub_get() {
  python3 -c 'import sys, urllib.request; print(urllib.request.urlopen(sys.argv[1]).read().decode())' "$U/_control/$1"
}

# start_bg <program> [VAR=value...]: run a probe in the background the way a
# probe pod runs it; its output goes to the file log.
start_bg() {
  local prog="$1"
  shift
  env KEYSTONE="$U/v3" OS_PASSWORD=pw "$@" python3 -u -c "$prog" >"$TMP_DIR/log" 2>&1 &
  BG_PID=$!
}

# await <regex> <seconds>: wait until the log has a line matching regex.
await() {
  local _
  for _ in $(seq 1 $(($2 * 10))); do
    if grep -qE -- "$1" "$TMP_DIR/log"; then
      return 0
    fi
    sleep 0.1
  done
  echo "  (no line matching $1 within $2s: $(tail -3 "$TMP_DIR/log"))"
  return 1
}

# finish: wait for the background probe; OUT and RC hold its log and status.
finish() {
  RC=0
  wait "$BG_PID" || RC=$?
  BG_PID=""
  OUT="$(cat "$TMP_DIR/log")"
}

# write-probe.py with its waits cut short and its stop file in the scratch
# directory.
write_probe() {
  sed -e 's/^PERIOD = 2$/PERIOD = 0.1/' -e 's/^LIST_SECONDS = 60$/LIST_SECONDS = 1/' \
    -e 's/time\.sleep(5)/time.sleep(0.1)/' -e "s|^STOP = \"/tmp/stop\"$|STOP = \"$TMP_DIR/stop\"|" \
    "$CHAOS/mariadb-io-fault/write-probe.py"
}

# --- Test 1 ---------------------------------------------------------------
test_write_probe() {
  echo "Test: write-probe.py finds every acknowledged write, names a lost one and fails on a listing that keeps failing"

  control '{"reset": true}'
  rm -f "$TMP_DIR/stop"
  start_bg "$(write_probe)"
  await '^WRITE 3 201$' 10
  touch "$TMP_DIR/stop"
  finish
  assert_eq "every acknowledged write listed passes" "0" "$RC"
  assert_contains "with ACKED-OK" "$OUT" "ACKED-OK "
  assert_contains "and DONE" "$OUT" "DONE"
  assert_eq "the io-chaos projects are deleted" "[]" "$(stub_get projects)"

  control '{"reset": true, "drop_next": true}'
  rm -f "$TMP_DIR/stop"
  start_bg "$(write_probe)"
  await '^WRITE 2 201$' 10
  touch "$TMP_DIR/stop"
  finish
  assert_eq "an acknowledged write that is gone fails" "1" "$RC"
  assert_contains "and is named" "$OUT" "ACKED-MISSING io-chaos-1"
  assert_not_contains "without ACKED-OK" "$OUT" "ACKED-OK"

  control '{"reset": true, "write_code": 503}'
  rm -f "$TMP_DIR/stop"
  start_bg "$(write_probe)"
  await '^WRITE 2 503$' 10
  touch "$TMP_DIR/stop"
  finish
  assert_eq "a refused write is not required afterwards" "0" "$RC"
  assert_contains "and not counted" "$OUT" "ACKED-OK 0"

  control '{"reset": true, "list_code": 500}'
  rm -f "$TMP_DIR/stop"
  start_bg "$(write_probe)"
  await '^WRITE 1 201$' 10
  touch "$TMP_DIR/stop"
  finish
  assert_eq "a listing that keeps failing fails" "1" "$RC"
  assert_contains "and says so" "$OUT" "FAIL: cannot list io-chaos projects: GET /projects answered HTTP 500"
}

# token-probe.py with its markers in the scratch directory, polled every
# 0.1 s.
token_probe() {
  sed -e 's/^MARKER_POLL = 2$/MARKER_POLL = 0.1/' -e "s|f\"/tmp/{name}\"|f\"$TMP_DIR/{name}\"|" \
    "$CHAOS/keystone-api-clock-skew/token-probe.py"
}

# token_run <behind> <ahead> <ahead lifetime>: run token-probe.py through its
# phases, the stub clock behind and then ahead by the offsets; it stops at
# the first FAIL line.
token_run() {
  rm -f "$TMP_DIR"/{behind,pre,ahead,after}
  # T_old expires a minute before it is issued, so the probe need not wait.
  control '{"reset": true, "lifetime": -60}'
  start_bg "$(token_probe)"
  if await '^EXPIRED-REJECTED 404$' 10; then
    control "{\"offset\": $1, \"lifetime\": 300}"
    touch "$TMP_DIR/behind"
    if await '^BEHIND-ACCEPTS-EXPIRED 200$|^FAIL:' 10 && ! grep -q '^FAIL:' "$TMP_DIR/log"; then
      control '{"offset": 0}'
      touch "$TMP_DIR/pre"
      await '^PRE-ISSUED$' 10
      control "{\"offset\": $2, \"lifetime\": $3}"
      touch "$TMP_DIR/ahead"
      if await '^AHEAD-REJECTS-FRESH 404$|^FAIL:' 10 && ! grep -q '^FAIL:' "$TMP_DIR/log"; then
        control '{"offset": 0, "lifetime": 300}'
        touch "$TMP_DIR/after"
      fi
    fi
  fi
  finish
}

# --- Test 2 ---------------------------------------------------------------
test_token_probe() {
  echo "Test: token-probe.py passes a skew that landed and fails each threshold it falls short of"

  token_run -3600 7200 300
  assert_eq "a skew of -1h, then 2h, passes" "0" "$RC"
  assert_contains "BEHIND-OFFSET reads the hour" "$OUT" "BEHIND-OFFSET -3600"
  assert_contains "the expired token validates behind" "$OUT" "BEHIND-ACCEPTS-EXPIRED 200"
  assert_contains "AHEAD-OFFSET reads two hours" "$OUT" "AHEAD-OFFSET 7200"
  assert_contains "a fresh token is rejected ahead" "$OUT" "AHEAD-REJECTS-FRESH 404"
  assert_contains "the skewed token outlives its lifetime" "$OUT" "SKEWED-TOKEN-OUTLIVES 200"
  assert_contains "and a fresh token validates" "$OUT" "RECOVERED-OK"

  token_run -20 7200 300
  assert_eq "a skew that missed the API fails behind" "1" "$RC"
  assert_contains "with the offset" "$OUT" "FAIL: BEHIND-OFFSET -20"

  token_run -3600 7000 300
  assert_eq "a skew under 7100 s fails ahead" "1" "$RC"
  assert_contains "with the offset" "$OUT" "FAIL: AHEAD-OFFSET 7000"

  token_run -3600 7200 -400
  assert_eq "a skewed token that expires under 6900 s ahead fails" "1" "$RC"
  assert_contains "with its remaining life" "$OUT" "FAIL: SKEWED-TOKEN-OUTLIVES 200, expires_at 6800 s ahead"
}

# import_run <phase>: run import-probe.py for one phase; OUT and RC.
import_run() {
  start_bg "$(cat "$CHAOS/glance-staging-io-fault/import-probe.py")" GLANCE="$U" PHASE="$1"
  finish
}

# --- Test 3 ---------------------------------------------------------------
test_import_probe() {
  echo "Test: import-probe.py judges the import, the refused stage, stuck imports and the cleanup"

  control '{"reset": true}'
  import_run latency
  assert_eq "a staged import that turns active passes" "0" "$RC"
  assert_contains "with its sentinel" "$OUT" "LATENCY-IMPORT-ACTIVE"
  assert_contains "the image id is printed" "$OUT" "IMAGE-ID img1"

  control '{"reset": true, "stage_code": 413}'
  import_run enospc
  assert_eq "a refused stage that leaves the image queued passes" "0" "$RC"
  assert_contains "with the code" "$OUT" "ENOSPC-STAGE-REFUSED 413"

  control '{"reset": true, "stage_code": 500, "refused_status": "uploading"}'
  import_run eio
  assert_eq "a refused stage that leaves the image uploading fails" "1" "$RC"
  assert_contains "and says so" "$OUT" "FAIL: image img1 is uploading after a refused stage, want queued"

  control '{"reset": true, "stage_code": 204}'
  import_run eio
  assert_eq "a stage that passes under the fault fails" "1" "$RC"
  assert_contains "and says so" "$OUT" "FAIL: stage answered 204 while the staging volume refused writes"

  control '{"reset": true, "images": {"x1": {"id": "x1", "name": "stage-eio", "status": "uploading"}}}'
  import_run check
  assert_eq "an image left uploading fails the check" "1" "$RC"
  assert_contains "and is named" "$OUT" "FAIL: images left uploading or importing: stage-eio (x1) uploading"

  control '{"reset": true, "images": {"x1": {"id": "x1", "name": "stage-eio", "status": "queued"}}}'
  import_run check
  assert_eq "no stuck image passes the check" "0" "$RC"
  assert_contains "with its sentinel" "$OUT" "NO-STUCK-IMPORT-OK"

  control '{"reset": true, "images": {"x1": {"id": "x1", "name": "stage-eio", "status": "queued"}, "x2": {"id": "x2", "name": "keep", "status": "active"}, "x3": {"id": "x3", "name": "stage-latency", "status": "active"}}}'
  import_run cleanup
  assert_eq "the cleanup passes" "0" "$RC"
  assert_contains "with its sentinel" "$OUT" "CLEANUP-OK"
  assert_eq "it deletes every stage-* image on both pages and nothing else" '["keep"]' "$(stub_get images)"

  import_run bogus
  assert_eq "an unknown phase fails" "1" "$RC"
  assert_contains "and names it" "$OUT" "FAIL: unknown PHASE bogus"
}

# lib <suite> <VAR=value...> -- <shell code>: run code with the suite's
# probe-lib.sh sourced from the suite directory, NAMESPACE=ns and the stubs
# first on PATH; OUT and RC hold the result.
lib() {
  local suite="$1" envs=()
  shift
  while [ "$1" != "--" ]; do
    envs+=("$1")
    shift
  done
  shift
  RC=0
  OUT="$(cd "$CHAOS/$suite" && env PATH="$TMP_DIR/bin:$PATH" NAMESPACE=ns \
    ${envs[@]+"${envs[@]}"} bash -c ". ./probe-lib.sh; $1" 2>&1)" || RC=$?
}

# The stub kubectl prints STUB_OUT for every call; the stub openssl prints
# STUB_ENDDATE for x509 and nothing for s_client.
cat >"$TMP_DIR/bin/kubectl" <<'SH'
#!/bin/bash
printf '%s' "${STUB_OUT:-}"
SH
cat >"$TMP_DIR/bin/openssl" <<'SH'
#!/bin/bash
cat >/dev/null
if [ "$1" = x509 ]; then
  printf '%s\n' "${STUB_ENDDATE:-}"
fi
SH
chmod +x "$TMP_DIR/bin/kubectl" "$TMP_DIR/bin/openssl"

# --- Test 4 ---------------------------------------------------------------
test_quantity_parsers() {
  echo "Test: quantity_bytes and quantity_millicores convert every suffix the metrics and the VPA print"
  local q want
  for pair in 250Mi=262144000 262144k=262144000 1.5Gi=1610612736 2G=2000000000 536870912=536870912; do
    q="${pair%%=*}" want="${pair#*=}"
    lib keystone-api-stress-autoscaling -- "quantity_bytes $q"
    assert_eq "quantity_bytes $q" "$want" "$OUT"
  done
  lib keystone-api-stress-autoscaling -- 'quantity_bytes 12x'
  assert_eq "an unknown unit fails" "1" "$RC"
  lib keystone-api-stress-autoscaling -- 'quantity_bytes Mi'
  assert_contains "and so does no number" "$OUT" "not a quantity: 'Mi'"

  for pair in 1500m=1500 2=2000 1234567890n=1234 250000u=250; do
    q="${pair%%=*}" want="${pair#*=}"
    lib keystone-api-stress-autoscaling -- "quantity_millicores $q"
    assert_eq "quantity_millicores $q" "$want" "$OUT"
  done
  lib keystone-api-stress-autoscaling -- 'quantity_millicores 1Gi'
  assert_contains "a memory quantity is no CPU quantity" "$OUT" "not a CPU quantity: '1Gi'"
}

# --- Test 5 ---------------------------------------------------------------
test_raft_status_and_cert_offsets() {
  echo "Test: field reads cluster/status, lease_holder the Lease, and cert_offset_hours rounds the notAfter up"
  local status
  status="$(printf '%s\n' 'abcd' 'Name: OVN_Southbound' 'Server ID: abcd (abcd1234-ef)' \
    'Role: follower' 'Term: 3' 'Leader: 9f2e' 'Leader: unknown')"
  lib ovn-raft-clock-skew -- "field '$status' Role"
  assert_eq "field reads the role" "follower" "$OUT"
  lib ovn-raft-clock-skew -- "field '$status' 'Server ID'"
  assert_eq "the first word of a name with a space" "abcd" "$OUT"
  lib ovn-raft-clock-skew -- "field '$status' Leader"
  assert_eq "the first of two lines" "9f2e" "$OUT"
  lib ovn-raft-clock-skew -- "field '$status' Vote"
  assert_eq "a missing name reads empty" "" "$OUT"

  local soon later
  soon="$(date -u -d '@'$(($(date +%s) + 9 * 3600 + 1800)) +%Y-%m-%dT%H:%M:%SZ)"
  later="$(date -u -d '@'$(($(date +%s) + 30 * 3600 + 1800)) +%Y-%m-%dT%H:%M:%SZ)"
  lib ovn-raft-clock-skew STUB_OUT="$(printf '%s\n' "$later" "" "$soon")" -- 'cert_offset_hours'
  assert_eq "the latest notAfter of the Certificates, rounded up, plus 24" "55" "$OUT"
  lib ovn-raft-clock-skew -- 'cert_offset_hours'
  assert_eq "no Certificate fails" "1" "$RC"
  assert_contains "with the suite's line" "$OUT" "FAIL: no Certificate labelled app.kubernetes.io/instance=ovn-clock-chaos"

  lib keystone-operator-clock-skew STUB_OUT="keystone-operator-7d9f-x2k_4c1e-9a7b" -- 'lease_holder'
  assert_eq "lease_holder cuts holderIdentity at the first underscore" "keystone-operator-7d9f-x2k" "$OUT"
  lib keystone-operator-clock-skew -- 'lease_holder'
  assert_eq "a missing holder reads empty" "" "$OUT"

  lib keystone-operator-clock-skew STUB_OUT="https://127.0.0.1:6443" \
    STUB_ENDDATE="notAfter=$(date -u -d '@'$(($(date +%s) + 9 * 3600 + 1800)) '+%b %e %H:%M:%S %Y GMT')" -- 'cert_offset_hours'
  assert_eq "the API server certificate's notAfter, rounded up, plus 24" "34" "$OUT"
  lib keystone-operator-clock-skew STUB_OUT="https://127.0.0.1:6443" STUB_ENDDATE="unable to load certificate" -- 'cert_offset_hours'
  assert_eq "an answer without notAfter fails" "1" "$RC"
  assert_contains "with the suite's line" "$OUT" \
    "FAIL: cannot read the API server certificate's notAfter: https://127.0.0.1:6443 answered 'unable to load certificate'"
}

# --- Run ---
if start_stub; then
  test_write_probe
  test_token_probe
  test_import_probe
fi
test_quantity_parsers
test_raft_status_and_cert_offsets

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
