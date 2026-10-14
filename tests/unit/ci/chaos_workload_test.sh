#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the workload and consistency harness of the chaos suites
# (tests/e2e-chaos/lib/, #1387):
#   1. workload.py run, against a stub OpenStack it talks HTTP to: every
#      stream reaches READY and writes parseable records; a 503, a closed port
#      and a 401 each end up in the record the harness promises; a bad command
#      line exits 2 before any request; the stop file sweeps, checks and ends
#      in DONE; stuck resources fail their check after the settle window.
#   2. workload.py evaluate, on hand-written logs: each violation exits 1,
#      each input error exits 2, and the summary line carries the counts.
#
# The stub serves Keystone, Nova, Neutron, Cinder, Glance and a plain HTTP
# target on one localhost port, each under a path prefix of its own, and
# answers /_control/ requests that steer it and report what it saw. It is
# written to the scratch directory and run with the runner's python3, which is
# also the interpreter workload.py is run with.
#
# Usage: bash tests/unit/ci/chaos_workload_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
LIB_DIR="$PROJECT_ROOT/tests/e2e-chaos/lib"
WORKLOAD_PY="$LIB_DIR/workload.py"

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

# ---------------------------------------------------------------------------
# The stub OpenStack
# ---------------------------------------------------------------------------

cat >"$TMP_DIR/stub_server.py" <<'PY'
"""A stub of the OpenStack APIs workload.py talks to, on one port."""
import json
import os
import sys
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit

PASSWORD = "s3cret"
PROJECT = "proj1"
NOAUTH = "admin:admin"
CONNECTOR_KEYS = {"host", "initiator", "platform", "os_type", "multipath"}
LOCK = threading.Lock()


def fresh():
    return {"tokens": set(), "issued": 0, "requests": [], "fail": {},
            "fail_n": [], "bad_flavors": False,
            "unauthorized_once": [], "hold_build": False, "held_reads": 0,
            "error_volumes": 0, "most_volumes": 0,
            "servers": {}, "networks": {}, "ports": {}, "volumes": {},
            "attachments": {}, "images": {}, "ghosts": {}}


STATE = fresh()


def new_id():
    return str(uuid.uuid4())


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def do_GET(self):
        self.dispatch("GET")

    def do_POST(self):
        self.dispatch("POST")

    def do_PUT(self):
        self.dispatch("PUT")

    def do_DELETE(self):
        self.dispatch("DELETE")

    def dispatch(self, method):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b""
        url = urlsplit(self.path)
        query = {k: v[0] for k, v in parse_qs(url.query).items()}
        with LOCK:
            if url.path.startswith("/_control/"):
                code, body = self.control(method, url.path, query)
            else:
                code, body, headers = self.route(method, url.path, query, raw)
                STATE["requests"].append(
                    [time.time(), method, self.path, code])
                return self.reply(code, body, headers)
        self.reply(code, body, {})

    def reply(self, code, body, headers):
        if isinstance(body, (bytes, str)):
            data = body.encode() if isinstance(body, str) else body
        else:
            data = json.dumps(body).encode() if body is not None else b""
        self.send_response(code)
        for key, value in headers.items():
            self.send_header(key, value)
        if data:
            self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        if data:
            self.wfile.write(data)

    # -- control --------------------------------------------------------
    def control(self, method, path, q):
        global STATE
        name = path[len("/_control/"):]
        if name == "reset":
            STATE = fresh()
        elif name == "fail":
            STATE["fail"][q["prefix"]] = int(q["code"])
        elif name == "fail-n":
            STATE["fail_n"].append({"prefix": q["prefix"], "method": q["method"],
                                    "code": int(q["code"]), "count": int(q["count"])})
        elif name == "bad-flavors":
            STATE["bad_flavors"] = True
        elif name == "unauthorized-once":
            STATE["unauthorized_once"].append(q["prefix"])
        elif name == "hold-build":
            STATE["hold_build"] = True
        elif name == "error-volumes":
            STATE["error_volumes"] = int(q["count"])
        elif name == "seed-stuck":
            STATE["servers"]["s-stuck"] = {"id": "s-stuck", "name": "stuck-server", "status": "BUILD"}
            STATE["volumes"]["v-stuck"] = {"id": "v-stuck", "name": "stuck-volume", "status": "attaching"}
            STATE["images"]["i-stuck"] = {"id": "i-stuck", "name": "stuck-image", "status": "queued"}
            STATE["ghosts"]["n-ghost"] = {"id": "n-ghost", "name": f"chaos-wl-{q['id']}-network-ghost"}
            for n in range(1, 13):
                STATE["ghosts"][f"n-ghost{n}"] = {"id": f"n-ghost{n}", "name": f"chaos-wl-{q['id']}-network-ghost-{n}"}
        elif name == "seed-leftovers":
            p = f"chaos-wl-{q['id']}-"
            STATE["servers"]["s-left"] = {"id": "s-left", "name": p + "server-99", "status": "ACTIVE"}
            STATE["networks"]["n-left"] = {"id": "n-left", "name": p + "network-99"}
            STATE["ports"]["p-left"] = {"id": "p-left", "name": p + "network-99", "network_id": "n-left"}
            STATE["volumes"]["v-left"] = {"id": "v-left", "name": p + "volume-99", "status": "in-use"}
            STATE["attachments"]["a-left"] = {"id": "a-left", "volume_id": "v-left", "status": "attached"}
            STATE["images"]["i-left"] = {"id": "i-left", "name": p + "image-99", "status": "active"}
            for n in range(1, 4):
                STATE["images"][f"i-keep{n}"] = {"id": f"i-keep{n}", "name": f"keep-{n}", "status": "active"}
        elif name == "requests":
            return 200, STATE["requests"]
        elif name == "state":
            out = {k: sorted(v.get("name") or v["id"] for v in STATE[k].values())
                   for k in ("servers", "networks", "ports", "volumes", "attachments", "images")}
            out["issued"] = STATE["issued"]
            out["most_volumes"] = STATE["most_volumes"]
            out["held_reads"] = STATE["held_reads"]
            return 200, out
        else:
            return 404, {"error": name}
        return 200, {}

    # -- routing --------------------------------------------------------
    def route(self, method, path, q, raw):
        for prefix, code in STATE["fail"].items():
            if path.startswith(prefix):
                return code, {"error": "stub failure"}, {}
        for rule in STATE["fail_n"]:
            if rule["count"] > 0 and rule["method"] == method and path.startswith(rule["prefix"]):
                rule["count"] -= 1
                return rule["code"], {"error": "stub failure"}, {}
        if path.startswith("/identity/v3/auth/tokens"):
            return self.keystone(method, raw)
        if path.startswith("/http/"):
            if path == "/http/multiple":
                return 300, {"versions": []}, {}
            return 200, "ok", {}
        token = self.headers.get("X-Auth-Token")
        cinder_noauth = path.startswith("/volume/v3/volumes") or path.startswith("/volume/v3/attachments")
        if cinder_noauth:
            if token != NOAUTH:
                return 401, {"error": "noauth wants admin:admin"}, {}
        elif token not in STATE["tokens"]:
            return 401, {"error": "unknown token"}, {}
        for prefix in list(STATE["unauthorized_once"]):
            if path.startswith(prefix):
                STATE["unauthorized_once"].remove(prefix)
                STATE["tokens"].discard(token)
                return 401, {"error": "token revoked once"}, {}
        body = json.loads(raw) if raw and self.headers.get("Content-Type") == "application/json" else None
        if path.startswith("/compute/v2.1/"):
            return self.nova(method, path[len("/compute/v2.1/"):], q, body)
        if path.startswith("/network/v2.0/"):
            return self.neutron(method, path[len("/network/v2.0/"):], body)
        if path.startswith("/volume/v3/"):
            rest = path[len("/volume/v3/"):]
            if rest.startswith(PROJECT + "/"):
                rest = rest[len(PROJECT) + 1:]
            return self.cinder(method, rest, q, body)
        if path.startswith("/image/v2/images"):
            return self.glance(method, path[len("/image/v2/images"):], q, raw)
        return 404, {"error": path}, {}

    def keystone(self, method, raw):
        if method == "POST":
            req = json.loads(raw)
            user = req["auth"]["identity"]["password"]["user"]
            scope = req["auth"]["scope"]["project"]
            if user["password"] != PASSWORD or user["name"] != "admin" or scope["name"] != "admin":
                return 401, {"error": "bad credentials"}, {}
            STATE["issued"] += 1
            token = f"tok-{STATE['issued']}"
            STATE["tokens"].add(token)
            return 201, {"token": {"project": {"id": PROJECT}}}, {"X-Subject-Token": token}
        auth = self.headers.get("X-Auth-Token")
        subject = self.headers.get("X-Subject-Token")
        if auth not in STATE["tokens"] or subject not in STATE["tokens"]:
            return 404, {"error": "token not found"}, {}
        return 200, {"token": {"project": {"id": PROJECT}}}, {"X-Subject-Token": subject}

    def nova(self, method, rest, q, body):
        servers = STATE["servers"]
        if self.headers.get("OpenStack-API-Version") != "compute 2.37":
            return 400, {"error": "want compute 2.37"}, {}
        if rest == "flavors" and method == "GET":
            if STATE["bad_flavors"]:
                return 200, {"flavors": ["m1.nano"]}, {}
            return 200, {"flavors": [{"id": "f1", "name": "m1.nano"}]}, {}
        if rest == "servers" and method == "POST":
            s = body["server"]
            if s.get("networks") != "none" or s.get("flavorRef") != "f1" or not s.get("imageRef"):
                return 400, {"error": "bad server request"}, {}
            sid = new_id()
            servers[sid] = {"id": sid, "name": s["name"],
                            "status": "BUILD" if STATE["hold_build"] else "ACTIVE",
                            "held": STATE["hold_build"]}
            return 202, {"server": {"id": sid}}, {}
        if rest == "servers/detail" and method == "GET":
            if q.get("all_tenants") != "1":
                return 400, {"error": "want all_tenants=1"}, {}
            return 200, {"servers": list(servers.values())}, {}
        sid = rest[len("servers/"):]
        if sid not in servers:
            return 404, {"error": "no server"}, {}
        if method == "GET":
            if servers[sid].get("held"):
                STATE["held_reads"] += 1
            return 200, {"server": servers[sid]}, {}
        if method == "DELETE":
            del servers[sid]
            return 204, None, {}
        return 405, None, {}

    def neutron(self, method, rest, body):
        kind, _, rid = rest.partition("/")
        if kind not in ("networks", "ports"):
            return 404, {"error": rest}, {}
        items = STATE[kind]
        if not rid and method == "POST":
            item = dict(body[kind[:-1]])
            if kind == "ports" and item.get("network_id") not in STATE["networks"]:
                return 404, {"error": "no network"}, {}
            item["id"] = new_id()
            items[item["id"]] = item
            return 201, {kind[:-1]: item}, {}
        if not rid and method == "GET":
            listed = list(items.values())
            if kind == "networks":
                listed += list(STATE["ghosts"].values())
            return 200, {kind: listed}, {}
        if rid not in items:
            return 404, {"error": "not found"}, {}
        if method == "GET":
            return 200, {kind[:-1]: items[rid]}, {}
        if method == "DELETE":
            if kind == "networks" and any(p["network_id"] == rid for p in STATE["ports"].values()):
                return 409, {"error": "network in use"}, {}
            del items[rid]
            return 204, None, {}
        return 405, None, {}

    def cinder(self, method, rest, q, body):
        vols, atts = STATE["volumes"], STATE["attachments"]
        version = self.headers.get("OpenStack-API-Version")
        if rest == "volumes" and method == "POST":
            if body["volume"].get("size") != 1:
                return 400, {"error": "want size 1"}, {}
            vid = new_id()
            status = "available"
            if STATE["error_volumes"] > 0:
                STATE["error_volumes"] -= 1
                status = "error"
            vols[vid] = {"id": vid, "name": body["volume"]["name"], "status": status}
            STATE["most_volumes"] = max(STATE["most_volumes"], len(vols))
            return 202, {"volume": dict(vols[vid], status="creating")}, {}
        if rest == "volumes/detail" and method == "GET":
            if q.get("all_tenants") != "1":
                return 400, {"error": "want all_tenants=1"}, {}
            return 200, {"volumes": list(vols.values())}, {}
        if rest.startswith("volumes/"):
            vid = rest[len("volumes/"):]
            if vid not in vols:
                return 404, {"error": "no volume"}, {}
            if method == "GET":
                return 200, {"volume": vols[vid]}, {}
            if method == "DELETE":
                if vols[vid]["status"] not in ("available", "error"):
                    return 400, {"error": "volume is " + vols[vid]["status"]}, {}
                del vols[vid]
                return 202, None, {}
        if rest == "attachments" and method == "POST":
            a = body["attachment"]
            if version != "volume 3.27" or a.get("volume_uuid") not in vols:
                return 400, {"error": "bad attachment request"}, {}
            aid = new_id()
            atts[aid] = {"id": aid, "volume_id": a["volume_uuid"], "status": "reserved",
                         "instance_uuid": a.get("instance_uuid")}
            vols[a["volume_uuid"]]["status"] = "reserved"
            return 200, {"attachment": atts[aid]}, {}
        if rest == "attachments" and method == "GET":
            if version != "volume 3.27":
                return 400, {"error": "want volume 3.27"}, {}
            vid = q.get("volume_id")
            return 200, {"attachments": [a for a in atts.values() if a["volume_id"] == vid]}, {}
        if rest.startswith("attachments/"):
            aid, _, action = rest[len("attachments/"):].partition("/")
            if aid not in atts:
                return 404, {"error": "no attachment"}, {}
            vol = vols[atts[aid]["volume_id"]]
            if method == "PUT":
                if version != "volume 3.27" or set(body["attachment"]["connector"]) != CONNECTOR_KEYS:
                    return 400, {"error": "bad attachment update"}, {}
                vol["status"] = "attaching"
                atts[aid]["connection_info"] = {"driver_volume_type": "nfs"}
                return 200, {"attachment": atts[aid]}, {}
            if method == "POST" and action == "action":
                if version != "volume 3.44" or body != {"os-complete": None}:
                    return 400, {"error": "bad os-complete"}, {}
                atts[aid]["status"] = "attached"
                vol["status"] = "in-use"
                return 204, None, {}
            if method == "DELETE":
                if version != "volume 3.27":
                    return 400, {"error": "want volume 3.27"}, {}
                # cinder 28.0.0 asks Nova before it deletes an attachment of an
                # instance with connection info; a noauth Cinder has no Nova.
                if atts[aid].get("instance_uuid") and atts[aid].get("connection_info"):
                    return 500, {"computeFault": {"code": 500, "message": "EndpointNotFound"}}, {}
                del atts[aid]
                vol["status"] = "available"
                return 200, {"attachments": []}, {}
        return 404, {"error": rest}, {}

    def glance(self, method, rest, q, raw):
        imgs = STATE["images"]
        if rest == "" and method == "POST":
            req = json.loads(raw)
            if req.get("disk_format") != "raw" or req.get("container_format") != "bare":
                return 400, {"error": "want raw/bare"}, {}
            iid = new_id()
            imgs[iid] = {"id": iid, "name": req["name"], "status": "queued"}
            return 201, imgs[iid], {}
        if rest == "" and method == "GET":
            # Pages of two, linked the way Glance links them.
            ordered = sorted(imgs.values(), key=lambda i: i["id"])
            marker = q.get("marker")
            if marker:
                ordered = [i for i in ordered if i["id"] > marker]
            page = ordered[:2]
            out = {"images": page}
            if len(ordered) > 2:
                out["next"] = "/v2/images?marker=" + page[-1]["id"]
            return 200, out, {}
        iid, _, sub = rest.lstrip("/").partition("/")
        if iid not in imgs:
            return 404, {"error": "no image"}, {}
        if sub == "file" and method == "PUT":
            if self.headers.get("Content-Type") != "application/octet-stream" or len(raw) != 1024 * 1024:
                return 400, {"error": "want 1 MiB of octets"}, {}
            imgs[iid]["status"] = "active"
            return 204, None, {}
        if method == "GET":
            return 200, imgs[iid], {}
        if method == "DELETE":
            del imgs[iid]
            return 204, None, {}
        return 405, None, {}


server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
with open(sys.argv[1] + ".tmp", "w") as handle:
    handle.write(str(server.server_address[1]))
# The rename makes the port file appear whole.
os.rename(sys.argv[1] + ".tmp", sys.argv[1])
server.serve_forever()
PY

cat >"$TMP_DIR/stub_ctl.py" <<'PY'
"""Send one control request to the stub and print its JSON answer."""
import sys
import urllib.request

opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
req = urllib.request.Request(sys.argv[1] + sys.argv[3], method=sys.argv[2], data=b"" if sys.argv[2] == "POST" else None)
with opener.open(req, timeout=10) as resp:
    sys.stdout.write(resp.read().decode())
PY

python3 -I "$TMP_DIR/stub_server.py" "$TMP_DIR/port" &
STUB_PID=$!
for _ in $(seq 1 100); do
  [ -f "$TMP_DIR/port" ] && break
  sleep 0.1
done
STUB="http://127.0.0.1:$(cat "$TMP_DIR/port" 2>/dev/null)"

# ctl <method> <path> — one control request; prints the answer.
ctl() {
  python3 -I "$TMP_DIR/stub_ctl.py" "$STUB" "$1" "$2"
}

# jq_py <python expression over d> — evaluate an expression on the JSON read
# from stdin and print the result.
jq_py() {
  python3 -I -c 'import json, sys; d = json.load(sys.stdin); print(eval(sys.argv[1]))' "$1"
}

# A free localhost port nobody listens on.
closed_port() {
  python3 -I -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}

ENDPOINTS=(
  --keystone "$STUB/identity/v3"
  --http "$STUB/http/ok"
  --neutron "$STUB/network"
  --nova "$STUB/compute/v2.1"
  --image-ref img-1
  --flavor m1.nano
  --cinder "$STUB/volume"
  --glance "$STUB/image"
)

# start_run <name> [VAR=value...] -- <run options> — start workload.py run
# with the id t1 in the background; RUN_OUT, RUN_ERR, RUN_STOP and RUN_PID
# name its files and process.
start_run() {
  local name="$1"
  shift
  local envs=()
  while [ "$1" != "--" ]; do
    envs+=("$1")
    shift
  done
  shift
  RUN_OUT="$TMP_DIR/$name.out"
  RUN_ERR="$TMP_DIR/$name.err"
  RUN_STOP="$TMP_DIR/$name.stop"
  rm -f "$RUN_STOP"
  env OS_PASSWORD=s3cret ${envs[@]+"${envs[@]}"} python3 -I "$WORKLOAD_PY" run --id t1 \
    --stop-file "$RUN_STOP" "$@" >"$RUN_OUT" 2>"$RUN_ERR" &
  RUN_PID=$!
}

# wait_for <description> <seconds> <command...> — poll a command every 0.1 s
# until it succeeds; returns 1 on the deadline.
wait_for() {
  local what="$1" limit="$2" i
  shift 2
  for ((i = 0; i < limit * 10; i++)); do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done
  echo "  (gave up waiting for $what after ${limit}s)"
  return 1
}

# ready_on <stream...> — every named stream printed READY.
ready_on() {
  local s
  for s in "$@"; do
    grep -qx "READY $s" "$RUN_OUT" || return 1
  done
}

# end_run — wait up to 60 s for the run to end; RUN_RC holds its exit status.
end_run() {
  local i
  for ((i = 0; i < 600; i++)); do
    if ! kill -0 "$RUN_PID" 2>/dev/null; then
      break
    fi
    sleep 0.1
  done
  if kill -0 "$RUN_PID" 2>/dev/null; then
    kill "$RUN_PID" 2>/dev/null
    echo "  (run did not end within 60s)"
  fi
  RUN_RC=0
  wait "$RUN_PID" || RUN_RC=$?
}

# finish_run — create the stop file and end the run.
finish_run() {
  touch "$RUN_STOP"
  end_run
}

# records <python filter over r> — print "s op ok code" for every record of
# RUN_OUT the filter keeps, one per line.
records() {
  python3 -I -c '
import json, sys
for line in open(sys.argv[1]):
    if line.startswith("{"):
        r = json.loads(line)
        if eval(sys.argv[2]):
            print(r["s"], r["op"], str(r["ok"]).lower(), r["code"])
' "$RUN_OUT" "${1:-True}"
}

count_lines() {
  grep -c "$1" <<<"$2" || true
}

# --- Test 1 ---------------------------------------------------------------
test_run_reaches_ready_on_every_stream() {
  echo "Test: run reaches READY on all six streams and writes a parseable record per operation"
  ctl POST /_control/reset >/dev/null

  start_run ready -- --streams token,http,network,server,volume,image "${ENDPOINTS[@]}"
  wait_for "six READY lines" 30 ready_on token http network server volume image
  finish_run

  assert_eq "run exits 0" "0" "$RUN_RC"
  local s
  for s in token http network server volume image; do
    assert_eq "one READY line for $s" "1" "$(grep -cx "READY $s" "$RUN_OUT")"
  done

  local bad
  bad="$(python3 -I -c '
import json, sys
bad = []
for n, line in enumerate(open(sys.argv[1]), 1):
    if not line.startswith("{"):
        continue
    r = json.loads(line)
    if set(r) != {"t", "s", "op", "ok", "code", "ms", "msg"} \
            or not isinstance(r["t"], float) or not isinstance(r["ok"], bool) \
            or not (r["code"] is None or isinstance(r["code"], int)) \
            or not isinstance(r["ms"], int) or not isinstance(r["msg"], str) \
            or len(r["msg"]) > 200:
        bad.append(str(n))
print(",".join(bad))
' "$RUN_OUT")"
  assert_eq "every record carries t, s, op, ok, code, ms and msg with their types" "" "$bad"

  local ops op
  ops="$(records 'r["ok"]')"
  for op in "token issue" "token validate" "http get" \
    "network create-network" "network create-port" "network delete-port" "network delete-network" \
    "server create" "server active" "server delete" "server gone" \
    "volume create" "volume available" "volume attach-create" "volume attach-update" \
    "volume attach-complete" "volume in-use" "volume detach" "volume detached" \
    "volume delete" "volume gone" \
    "image create" "image upload" "image active" "image delete"; do
    assert_contains "an ok record for $op" "$ops" "$op true"
  done
  assert_eq "no failed record" "" "$(records 'not r["ok"]')"
  assert_contains "the volume stream used the project-scoped path with --keystone" \
    "$(ctl GET /_control/requests)" '"/volume/v3/proj1/volumes"'

  # The source rides in one argv element of `python3 -c`, which the kernel
  # caps at 131072 bytes, and the Tempest image's interpreter is 3.10 or newer.
  local size
  size="$(wc -c <"$WORKLOAD_PY")"
  assert_eq "workload.py stays under 120000 bytes (it is ${size})" "yes" \
    "$([ "$size" -lt 120000 ] && echo yes || echo no)"
  assert_eq "workload.py parses with the Python 3.10 grammar" "ok" "$(python3 -I -c '
import ast, sys
src = open(sys.argv[1]).read()
compile(src, sys.argv[1], "exec")
ast.parse(src, feature_version=(3, 10))
print("ok")' "$WORKLOAD_PY" 2>&1)"
}

# --- Test 2 ---------------------------------------------------------------
test_a_503_fails_one_stream_and_leaves_the_others_ok() {
  echo "Test: a 503 on the http target fails that stream alone"
  ctl POST /_control/reset >/dev/null
  ctl POST "/_control/fail?prefix=/http/&code=503" >/dev/null

  start_run fail503 -- --streams token,http,network "${ENDPOINTS[@]}"
  wait_for "three failed http records and READY on token and network" 30 bash -c \
    "[ \"\$(grep -c '\"s\": \"http\"' '$RUN_OUT')\" -ge 3 ] && grep -qx 'READY token' '$RUN_OUT' && grep -qx 'READY network' '$RUN_OUT'"
  finish_run

  local http
  http="$(records 'r["s"] == "http"')"
  assert_not_empty "the http stream wrote records" "$http"
  assert_eq "every http record is ok: false with code 503" "" "$(grep -v 'http get false 503' <<<"$http")"
  assert_eq "the http stream never printed READY" "0" "$(grep -cx 'READY http' "$RUN_OUT")"
  assert_contains "the token stream kept writing ok records" "$(records 'r["s"] == "token"')" "token validate true 200"
  assert_contains "the network stream kept writing ok records" "$(records 'r["s"] == "network"')" "network delete-network true 204"
  assert_eq "no token or network record failed" "" "$(records 'r["s"] != "http" and not r["ok"]')"
}

# --- Test 3 ---------------------------------------------------------------
test_a_closed_port_records_a_null_code_and_the_urlerror() {
  echo "Test: a closed port is a record with code null and the URLError text; noauth Cinder works without --keystone"
  ctl POST /_control/reset >/dev/null
  local port
  port="$(closed_port)"

  start_run closed -- --streams http,volume --http "http://127.0.0.1:${port}/" --cinder "$STUB/volume"
  wait_for "two http records and READY on volume" 30 bash -c \
    "[ \"\$(grep -c '\"s\": \"http\"' '$RUN_OUT')\" -ge 2 ] && grep -qx 'READY volume' '$RUN_OUT'"
  finish_run

  local first
  first="$(grep -m1 '"s": "http"' "$RUN_OUT")"
  assert_contains "the http record is not ok" "$first" '"ok": false'
  assert_contains "its code is null" "$first" '"code": null'
  assert_contains "its msg holds the URLError" "$first" 'URLError'
  assert_contains "the noauth volume stream completes its cycle" "$(records 'r["s"] == "volume"')" "volume gone true 404"
  local reqs
  reqs="$(ctl GET /_control/requests)"
  assert_contains "without --keystone the volume paths carry no project id" "$reqs" '"/volume/v3/volumes"'
  assert_not_contains "and never the project-scoped one" "$reqs" '/volume/v3/proj1/'
  assert_eq "no token was issued" "0" "$(ctl GET /_control/state | jq_py 'd["issued"]')"
}

# --- Test 3a --------------------------------------------------------------
test_a_300_is_an_answer_to_the_http_stream() {
  echo "Test: the http stream takes a 300 Multiple Choices, Glance's root answer, as an answer"
  ctl POST /_control/reset >/dev/null

  start_run multiple -- --streams http --http "$STUB/http/multiple"
  wait_for "READY http" 30 ready_on http
  finish_run

  assert_eq "run exits 0" "0" "$RUN_RC"
  assert_contains "a 300 is an ok record" "$(records 'r["s"] == "http"')" "http get true 300"
  assert_eq "no failed record" "" "$(records 'not r["ok"]')"
}

# --- Test 3b --------------------------------------------------------------
test_a_failed_cycle_deletes_what_it_created() {
  echo "Test: a failed cycle deletes what it created, so a fault cannot fill a quota"
  ctl POST /_control/reset >/dev/null
  ctl POST "/_control/error-volumes?count=4" >/dev/null

  start_run cleanup -- --streams volume --cinder "$STUB/volume"
  wait_for "READY volume" 30 ready_on volume
  finish_run

  assert_eq "run exits 0" "0" "$RUN_RC"
  assert_eq "four volumes reached error" "4" \
    "$(records 'r["op"] == "available" and not r["ok"]' | wc -l | tr -d ' ')"
  assert_eq "each was deleted by a cleanup-volume record" "4" \
    "$(records 'r["op"] == "cleanup-volume" and r["ok"]' | wc -l | tr -d ' ')"
  assert_eq "the stub never held more than one volume" "1" \
    "$(ctl GET /_control/state | jq_py 'd["most_volumes"]')"
  assert_eq "no cleanup record failed" "0" \
    "$(records 'r["op"].startswith("cleanup-") and not r["ok"]' | wc -l | tr -d ' ')"
}

# --- Test 3c --------------------------------------------------------------
test_a_failed_cycle_deletes_the_newest_resource_first() {
  echo "Test: a failed cycle deletes its attachment before the volume the attachment reserves"
  ctl POST /_control/reset >/dev/null
  # Two attach-update calls fail. Like Cinder, the stub refuses to delete the
  # volume their attachment still reserves.
  ctl POST "/_control/fail-n?prefix=/volume/v3/attachments/&method=PUT&code=503&count=2" >/dev/null

  start_run cleanup-order -- --streams volume --cinder "$STUB/volume"
  wait_for "READY volume" 30 ready_on volume
  finish_run

  assert_eq "run exits 0" "0" "$RUN_RC"
  assert_eq "two attach-update records failed" "2" \
    "$(records 'r["op"] == "attach-update" and not r["ok"]' | wc -l | tr -d ' ')"
  assert_eq "each failed cycle deleted its attachment, then its volume" \
    "$(printf 'cleanup-attachment\ncleanup-volume\ncleanup-attachment\ncleanup-volume')" \
    "$(records 'r["op"].startswith("cleanup-")' | awk '{print $2}')"
  assert_eq "no cleanup record failed" "0" \
    "$(records 'r["op"].startswith("cleanup-") and not r["ok"]' | wc -l | tr -d ' ')"
  assert_eq "no attachment is left" "[]" "$(ctl GET /_control/state | jq_py 'd["attachments"]')"
}

# --- Test 4 ---------------------------------------------------------------
test_a_401_reissues_the_token_once_and_retries() {
  echo "Test: a 401 to an authenticated call issues one new token and retries the call"
  ctl POST /_control/reset >/dev/null

  start_run reauth -- --streams network "${ENDPOINTS[@]}"
  wait_for "READY network" 30 ready_on network
  assert_eq "one token was issued before the 401" "1" "$(ctl GET /_control/state | jq_py 'd["issued"]')"
  ctl POST "/_control/unauthorized-once?prefix=/network/" >/dev/null
  wait_for "a request after the 401" 30 bash -c \
    "python3 -I '$TMP_DIR/stub_ctl.py' '$STUB' GET /_control/requests | python3 -I -c '
import json, sys
r = json.load(sys.stdin)
i = [n for n, x in enumerate(r) if x[3] == 401]
sys.exit(0 if i and len(r) > i[0] + 2 else 1)'"
  finish_run

  local verdict
  verdict="$(ctl GET /_control/requests | python3 -I -c '
import json, sys
r = json.load(sys.stdin)
i = [n for n, x in enumerate(r) if x[3] == 401][0]
refused, issue, retry = r[i], r[i + 1], r[i + 2]
print(issue[1], issue[2], issue[3])
print("same call" if (retry[1], retry[2]) == (refused[1], refused[2]) else "other call", retry[3])')"
  assert_contains "the 401 is followed by a token issue" "$verdict" "POST /identity/v3/auth/tokens 201"
  assert_contains "and by the same call again, which succeeds" "$verdict" "same call 20"
  assert_eq "two tokens were issued in all" "2" "$(ctl GET /_control/state | jq_py 'd["issued"]')"
  assert_eq "no record failed" "" "$(records 'not r["ok"]')"
}

# --- Test 4b --------------------------------------------------------------
test_a_stream_defect_ends_the_run_without_done() {
  echo "Test: an exception that is no request failure stops every stream and ends the run with its traceback"
  ctl POST /_control/reset >/dev/null
  # A flavor list of strings makes the server stream call .get on a str.
  ctl POST /_control/bad-flavors >/dev/null

  start_run crash -- --streams token,server "${ENDPOINTS[@]}"
  # No stop file: the run ends only when the crash stops the token stream too.
  end_run

  assert_eq "run exits 1" "1" "$RUN_RC"
  assert_contains "it prints the traceback" "$(cat "$RUN_ERR")" "AttributeError"
  assert_eq "it prints no DONE" "0" "$(grep -cx DONE "$RUN_OUT")"
}

# --- Test 5 ---------------------------------------------------------------
test_usage_errors_exit_2_before_any_request() {
  echo "Test: a bad command line exits 2 with a usage: line before any request"
  ctl POST /_control/reset >/dev/null
  local rc out
  local cases=(
    "empty:--streams="
    "unknown:--streams=bogus"
    "network without keystone:--streams=network --neutron=$STUB/network"
    "twice:--streams=http,http --http=$STUB/http/ok"
  )
  local entry label args
  for entry in "${cases[@]}"; do
    label="${entry%%:*}"
    args="${entry#*:}"
    rc=0
    # shellcheck disable=SC2086 # the case strings are split on purpose
    out="$(OS_PASSWORD=s3cret python3 -I "$WORKLOAD_PY" run --id t1 --stop-file "$TMP_DIR/x.stop" $args 2>&1)" || rc=$?
    assert_eq "streams ${label}: exit 2" "2" "$rc"
    assert_starts_with "streams ${label}: a usage: line" "$out" "usage:"
  done
  rc=0
  out="$(env -u OS_PASSWORD python3 -I "$WORKLOAD_PY" run --id t1 --stop-file "$TMP_DIR/x.stop" \
    --streams token --keystone "$STUB/identity/v3" 2>&1)" || rc=$?
  assert_eq "--keystone without OS_PASSWORD: exit 2" "2" "$rc"
  assert_contains "it names OS_PASSWORD" "$out" "usage: --keystone needs the admin password in OS_PASSWORD"
  local settle
  for settle in abc -1; do
    rc=0
    out="$(CHAOS_WORKLOAD_SETTLE_SECONDS=$settle python3 -I "$WORKLOAD_PY" run --id t1 --stop-file "$TMP_DIR/x.stop" \
      --streams http --http "$STUB/http/ok" 2>&1)" || rc=$?
    assert_eq "CHAOS_WORKLOAD_SETTLE_SECONDS=${settle}: exit 2" "2" "$rc"
    assert_contains "it names the value" "$out" "usage: CHAOS_WORKLOAD_SETTLE_SECONDS=${settle} is not a number of seconds"
  done
  assert_eq "the stub saw no request" "0" "$(ctl GET /_control/requests | jq_py 'len(d)')"
}

# --- Test 6 ---------------------------------------------------------------
test_the_stop_file_sweeps_checks_and_prints_done() {
  echo "Test: the stop file sweeps every prefixed resource, checks each service and prints DONE"
  ctl POST /_control/reset >/dev/null
  ctl POST "/_control/seed-leftovers?id=t1" >/dev/null

  start_run sweep -- --streams token,network,server,volume,image "${ENDPOINTS[@]}"
  wait_for "READY on the four resource streams" 30 ready_on network server volume image
  # The next server stays in BUILD, so the stop file lands while the server
  # stream waits for ACTIVE.
  ctl POST /_control/hold-build >/dev/null
  wait_for "a read of a held server" 30 bash -c \
    "[ \"\$(python3 -I '$TMP_DIR/stub_ctl.py' '$STUB' GET /_control/state | python3 -I -c 'import json,sys; print(json.load(sys.stdin)[\"held_reads\"])')\" -ge 1 ]"
  finish_run

  assert_eq "run exits 0" "0" "$RUN_RC"
  assert_eq "DONE is the last line" "DONE" "$(tail -n1 "$RUN_OUT")"
  local check
  for check in networks servers volumes images; do
    assert_eq "CONSISTENCY-OK ${check}" "1" "$(grep -cx "CONSISTENCY-OK ${check}" "$RUN_OUT")"
  done
  assert_eq "no CONSISTENCY-FAIL" "0" "$(grep -c '^CONSISTENCY-FAIL' "$RUN_OUT")"

  local state
  state="$(ctl GET /_control/state)"
  assert_eq "no prefixed resource is left at the stub" "[]" \
    "$(jq_py '[n for k in ("servers", "networks", "ports", "volumes", "images") for n in d[k] if n.startswith("chaos-wl-t1-")]' <<<"$state")"
  assert_eq "the leftover volume's attachment is gone" "[]" "$(jq_py 'd["attachments"]' <<<"$state")"
  assert_eq "the unprefixed images stay" "['keep-1', 'keep-2', 'keep-3']" "$(jq_py 'd["images"]' <<<"$state")"

  local sweeps kind
  sweeps="$(records 'r["op"].startswith("sweep-")')"
  for kind in servers ports networks volumes attachments images; do
    assert_contains "an ok sweep-${kind} record" "$sweeps" "sweep-${kind} true"
  done
  assert_eq "no sweep record failed" "" "$(grep -v ' true ' <<<"$sweeps")"
  assert_eq "the held server has a create record and no active record" \
    "$(records 'r["s"] == "server" and r["op"] == "create" and r["ok"]' | wc -l)" \
    "$(($(records 'r["s"] == "server" and r["op"] == "active"' | wc -l) + 1))"
}

# --- Test 7 ---------------------------------------------------------------
test_stuck_resources_fail_the_checks_after_the_settle_window() {
  echo "Test: stuck resources fail all four checks, each after re-reading for the settle window"
  ctl POST /_control/reset >/dev/null

  start_run stuck CHAOS_WORKLOAD_SETTLE_SECONDS=2 -- --streams network,server,volume,image "${ENDPOINTS[@]}"
  wait_for "READY on the four resource streams" 30 ready_on network server volume image
  ctl POST "/_control/seed-stuck?id=t1" >/dev/null
  finish_run

  assert_eq "run exits 0 (the verdict is evaluate's)" "0" "$RUN_RC"
  assert_eq "DONE is the last line" "DONE" "$(tail -n1 "$RUN_OUT")"
  local out
  out="$(cat "$RUN_OUT")"
  assert_contains "CONSISTENCY-FAIL servers names the BUILD server" "$out" \
    "CONSISTENCY-FAIL servers: server stuck-server (s-stuck) is BUILD"
  assert_contains "CONSISTENCY-FAIL volumes names the attaching volume" "$out" \
    "CONSISTENCY-FAIL volumes: volume stuck-volume (v-stuck) is attaching"
  assert_contains "CONSISTENCY-FAIL images names the queued image" "$out" \
    "CONSISTENCY-FAIL images: image stuck-image (i-stuck) is queued"
  assert_contains "CONSISTENCY-FAIL networks names the prefixed network" "$out" \
    "CONSISTENCY-FAIL networks: network chaos-wl-t1-network-ghost (n-ghost) is left over"
  assert_contains "it names ten of the thirteen networks and counts the rest" \
    "$(grep '^CONSISTENCY-FAIL networks' <<<"$out")" \
    "network chaos-wl-t1-network-ghost-9 (n-ghost9) is left over; and 3 more"

  local spans
  spans="$(ctl GET /_control/requests | python3 -I -c '
import json, sys
r = json.load(sys.stdin)
for path in ("/compute/v2.1/servers/detail?all_tenants=1",
             "/volume/v3/proj1/volumes/detail?all_tenants=1",
             "/image/v2/images", "/network/v2.0/networks"):
    t = [x[0] for x in r if x[1] == "GET" and x[2] == path]
    print(path, "re-read" if len(t) >= 2 and t[-1] - t[-2] >= 1.9 else "not re-read")')"
  assert_eq "each check re-read its listing after the 2 s settle window" "0" "$(count_lines 'not re-read' "$spans")"
}

# --- Test 8 ---------------------------------------------------------------
test_a_check_that_cannot_read_reports_the_error_as_detail() {
  echo "Test: a listing that keeps failing fails the sweep record and the check with the HTTP error"
  ctl POST /_control/reset >/dev/null

  start_run unreadable CHAOS_WORKLOAD_SETTLE_SECONDS=1 -- --streams server "${ENDPOINTS[@]}"
  wait_for "READY server" 30 ready_on server
  ctl POST "/_control/fail?prefix=/compute/v2.1/servers/detail&code=503" >/dev/null
  finish_run

  assert_eq "run exits 0" "0" "$RUN_RC"
  assert_contains "CONSISTENCY-FAIL servers carries the 503" "$(cat "$RUN_OUT")" "CONSISTENCY-FAIL servers: HTTP 503"
  assert_contains "the failed listing is a failed sweep-servers record" "$(records 'r["op"] == "sweep-servers"')" \
    "server sweep-servers false 503"
}

# --- Test 8b --------------------------------------------------------------
test_a_volume_sweep_without_a_token_still_ends_in_done() {
  echo "Test: a volume sweep that cannot get a token records the failed listing and the run still ends in DONE"
  ctl POST /_control/reset >/dev/null

  start_run notoken CHAOS_WORKLOAD_SETTLE_SECONDS=1 -- --streams volume --keystone "$STUB/identity/v3" --cinder "$STUB/volume"
  wait_for "READY volume" 30 ready_on volume
  # Keystone stops issuing tokens and Cinder refuses the stream's token once,
  # so the stream holds no token when the stop file lands.
  ctl POST "/_control/fail?prefix=/identity/&code=503" >/dev/null
  ctl POST "/_control/unauthorized-once?prefix=/volume/" >/dev/null
  wait_for "a failed volume record" 30 grep -q '"s": "volume".*"ok": false' "$RUN_OUT"
  finish_run

  assert_eq "run exits 0" "0" "$RUN_RC"
  assert_eq "DONE is the last line" "DONE" "$(tail -n1 "$RUN_OUT")"
  assert_contains "the failed listing is a failed sweep-volumes record" "$(records 'r["op"] == "sweep-volumes"')" \
    "volume sweep-volumes false 503"
  assert_eq "nothing reached stderr" "" "$(cat "$RUN_ERR")"
}

# ---------------------------------------------------------------------------
# evaluate
# ---------------------------------------------------------------------------

# rec <t> <stream> <op> <ok> [code] — one record line.
rec() {
  printf '{"t": %s, "s": "%s", "op": "%s", "ok": %s, "code": %s, "ms": 5, "msg": ""}\n' \
    "$1" "$2" "$3" "$4" "${5:-200}"
}

# evaluate_log <name> <log text> [options] — run evaluate on the text; prints
# its output, EVAL_RC holds the exit status.
evaluate_log() {
  local file="$TMP_DIR/$1.log"
  printf '%s' "$2" >"$file"
  shift 2
  EVAL_RC=0
  EVAL_OUT="$(python3 -I "$WORKLOAD_PY" evaluate --log "$file" "$@" 2>&1)" || EVAL_RC=$?
}

MARKS=(--fault-start 1000 --fault-end 1100 --recovery 60)

# --- Test 9 ---------------------------------------------------------------
test_evaluate_passes_a_clean_log_and_fails_each_violation() {
  echo "Test: evaluate passes a clean log and fails each violation (window 1000..1160)"
  local base
  base="$(rec 900 token issue true 201)"$'\n'"$(rec 1200 token issue true 201)"$'\n'

  evaluate_log clean "${base}DONE"$'\n' --budget-failures 0 "${MARKS[@]}"
  assert_eq "no failures: exit 0" "0" "$EVAL_RC"

  local two
  two="$(rec 1010 token issue false 503)"$'\n'"$(rec 1150 token validate false)"$'\n'
  evaluate_log at-budget "${base}${two}DONE"$'\n' --budget-failures 2 "${MARKS[@]}"
  assert_eq "two in-window failures at a budget of 2: exit 0" "0" "$EVAL_RC"

  evaluate_log over-budget "${base}${two}$(rec 1160 token issue false null)"$'\n'"DONE"$'\n' \
    --budget-failures 2 "${MARKS[@]}"
  assert_eq "three in-window failures at a budget of 2: exit 1" "1" "$EVAL_RC"
  assert_contains "it names the count and the budget" "$EVAL_OUT" "FAIL: 3 failed records in the fault window, budget 2"

  # The budget is the suite's: the streams share it.
  evaluate_log shared "${base}$(rec 1010 token issue false 503)"$'\n'"$(rec 1020 token issue false 503)"$'\n'"$(rec 1030 http get false 503)"$'\n'"$(rec 1200 http get true)"$'\n'"DONE"$'\n' \
    --budget-failures 2 "${MARKS[@]}"
  assert_eq "2 + 1 in-window failures of two streams at a budget of 2: exit 1" "1" "$EVAL_RC"
  assert_contains "the streams' failures add up" "$EVAL_OUT" "FAIL: 3 failed records in the fault window, budget 2"

  evaluate_log before "${base}$(rec 999.9 token issue false 503)"$'\n'"DONE"$'\n' --budget-failures 9 "${MARKS[@]}"
  assert_eq "a failure before fault_start: exit 1" "1" "$EVAL_RC"
  assert_contains "it is outside the window" "$EVAL_OUT" "FAIL: stream token: 1 failed record(s) outside the fault window"

  evaluate_log after "${base}$(rec 1160.1 token issue false 503)"$'\n'"DONE"$'\n' --budget-failures 9 "${MARKS[@]}"
  assert_eq "a failure after fault_end + recovery: exit 1" "1" "$EVAL_RC"
  assert_contains "the record is printed" "$EVAL_OUT" 'offending: {"t": 1160.1'

  # A sweep runs after the stop file, whatever the clock of its record says.
  evaluate_log sweep "${base}$(rec 1050 server sweep-servers false 409)"$'\n'"$(rec 1250 server create true 202)"$'\n'"DONE"$'\n' \
    --budget-failures 9 "${MARKS[@]}"
  assert_eq "a failed sweep record inside the window: exit 1" "1" "$EVAL_RC"
  assert_contains "it is judged outside the window" "$EVAL_OUT" "FAIL: stream server: 1 failed record(s) outside the fault window"

  evaluate_log sweep-only "${base}$(rec 900 server create true 202)"$'\n'"$(rec 1300 server sweep-servers true 204)"$'\n'"DONE"$'\n' \
    --budget-failures 9 "${MARKS[@]}"
  assert_eq "an ok sweep record is no recovery: exit 1" "1" "$EVAL_RC"
  assert_contains "it names the stream" "$EVAL_OUT" "FAIL: stream server: no ok record after the recovery deadline"

  evaluate_log late "$(rec 1050 token issue true 201)"$'\n'"$(rec 1170 http get false 503)"$'\n'"$(rec 1100 http get true)"$'\n'"$(rec 1200 token issue true 201)"$'\n'"DONE"$'\n' \
    --budget-failures 9 --fault-start 1000 --fault-end 1100 --recovery 80
  assert_eq "a stream without an ok record after the deadline: exit 1" "1" "$EVAL_RC"
  assert_contains "it names the stream" "$EVAL_OUT" "FAIL: stream http: no ok record after the recovery deadline"

  evaluate_log consistency "${base}CONSISTENCY-FAIL servers: server x (1) is BUILD"$'\n'"DONE"$'\n' \
    --budget-failures 0 "${MARKS[@]}"
  assert_eq "a CONSISTENCY-FAIL line: exit 1" "1" "$EVAL_RC"
  assert_contains "the line is repeated" "$EVAL_OUT" "FAIL: CONSISTENCY-FAIL servers: server x (1) is BUILD"

  evaluate_log no-done "$base" --budget-failures 0 "${MARKS[@]}"
  assert_eq "no DONE: exit 1" "1" "$EVAL_RC"
  assert_contains "it says so" "$EVAL_OUT" "FAIL: the workload printed no DONE"

  # start waits for READY, so a failure before the READY line of its stream
  # happened while the service was still coming up.
  local warm
  warm="$(rec 890 token issue false 503)"$'\n'"$(rec 895 token issue true 201)"$'\n'"READY token"$'\n'"$(rec 1200 token issue true 201)"$'\n'
  evaluate_log warmup "${warm}DONE"$'\n' --budget-failures 0 "${MARKS[@]}"
  assert_eq "a failure before READY is not judged: exit 0" "0" "$EVAL_RC"
  assert_contains "it is reported apart" "$EVAL_OUT" "stream token: 1 failed record(s) before READY, not judged"
  evaluate_log after-ready "${warm}$(rec 950 token issue false 503)"$'\n'"DONE"$'\n' --budget-failures 9 "${MARKS[@]}"
  assert_eq "a failure after READY and before fault_start: exit 1" "1" "$EVAL_RC"
  assert_contains "the offending record carries the record fields only" "$EVAL_OUT" \
    'offending: {"t": 950, "s": "token", "op": "issue", "ok": false, "code": 503, "ms": 5, "msg": ""}'
}

# --- Test 10 --------------------------------------------------------------
test_evaluate_without_marks_fails_any_failure() {
  echo "Test: without marks there is no window"
  local base
  base="$(rec 900 token issue true 201)"$'\n'"$(rec 950 http get true)"$'\n'
  evaluate_log nomarks-clean "${base}DONE"$'\n' --budget-failures 100 --recovery 600
  assert_eq "a clean log: exit 0" "0" "$EVAL_RC"
  evaluate_log nomarks-fail "${base}$(rec 960 http get false 502)"$'\n'"DONE"$'\n' --budget-failures 100 --recovery 600
  assert_eq "one failed record, whatever the budget: exit 1" "1" "$EVAL_RC"
  evaluate_log nomarks-never "$(rec 900 token issue true 201)"$'\n'"$(rec 950 http get false 502)"$'\n'"DONE"$'\n' \
    --budget-failures 100 --recovery 600
  assert_contains "a stream without any ok record is named" "$EVAL_OUT" "FAIL: stream http: no ok record"
  evaluate_log nomarks-empty "DONE"$'\n' --budget-failures 0 --recovery 0
  assert_eq "a log with DONE and no record: exit 1" "1" "$EVAL_RC"
}

# --- Test 11 --------------------------------------------------------------
test_evaluate_input_errors_exit_2_naming_the_cause() {
  echo "Test: evaluate input errors exit 2 and name the cause"
  evaluate_log empty "" --budget-failures 0 "${MARKS[@]}"
  assert_eq "an empty log: exit 2" "2" "$EVAL_RC"
  assert_contains "it says empty" "$EVAL_OUT" "is empty"

  evaluate_log badjson "$(rec 900 token issue true 201)"$'\n'"READY token"$'\n'"{not json"$'\n'"DONE"$'\n' \
    --budget-failures 0 "${MARKS[@]}"
  assert_eq "a { line that is not JSON: exit 2" "2" "$EVAL_RC"
  assert_contains "it names line 3" "$EVAL_OUT" "line 3 of"

  evaluate_log onemark "$(rec 900 token issue true 201)"$'\n'"DONE"$'\n' --budget-failures 0 --recovery 0 --fault-start 1000
  assert_eq "only --fault-start: exit 2" "2" "$EVAL_RC"
  assert_contains "it says the marks go together" "$EVAL_OUT" "--fault-start and --fault-end go together"

  evaluate_log reversed "$(rec 900 token issue true 201)"$'\n'"DONE"$'\n' --budget-failures 0 --recovery 0 \
    --fault-start 1100 --fault-end 1000
  assert_eq "fault_end before fault_start: exit 2" "2" "$EVAL_RC"
  assert_contains "it names both marks" "$EVAL_OUT" "fault_end 1000.0 is before fault_start 1100.0"
}

# --- Test 12 --------------------------------------------------------------
test_evaluate_summary_counts_match_the_hand_computed_values() {
  echo "Test: the summary line carries the counts and the recovery offset"
  # Window 1000..1160. token: ok at 990, 1050, 1180, 1190; failed in the
  # window at 1010 and 1120.5 (recovery 1120.5 - 1100 = 20.5 s), outside at
  # 1170. http: failed in the window at 1050 only, before fault_end, so its
  # recovery is floored at 0; ok at 1200.
  local log
  log="$(rec 990 token issue true 201)
$(rec 1010 token issue false 503)
$(rec 1050 token validate true)
$(rec 1120.5 token issue false null)
$(rec 1170 token validate false 401)
$(rec 1180 token issue true 201)
$(rec 1190 token validate true)
$(rec 1050 http get false 503)
$(rec 1200 http get true)
DONE
"
  evaluate_log summary "$log" --budget-failures 5 "${MARKS[@]}"
  assert_contains "the token line" "$EVAL_OUT" \
    "stream token: 4 ok, 2 failed in window (budget 5), 1 outside; observed failures=2 recovery=20.5s"
  assert_contains "the http line" "$EVAL_OUT" \
    "stream http: 1 ok, 1 failed in window (budget 5), 0 outside; observed failures=1 recovery=0.0s"
  assert_eq "the outside failure fails the run" "1" "$EVAL_RC"
}

# --- Run ---
test_run_reaches_ready_on_every_stream
test_a_503_fails_one_stream_and_leaves_the_others_ok
test_a_closed_port_records_a_null_code_and_the_urlerror
test_a_300_is_an_answer_to_the_http_stream
test_a_failed_cycle_deletes_what_it_created
test_a_failed_cycle_deletes_the_newest_resource_first
test_a_401_reissues_the_token_once_and_retries
test_a_stream_defect_ends_the_run_without_done
test_usage_errors_exit_2_before_any_request
test_the_stop_file_sweeps_checks_and_prints_done
test_stuck_resources_fail_the_checks_after_the_settle_window
test_a_check_that_cannot_read_reports_the_error_as_detail
test_a_volume_sweep_without_a_token_still_ends_in_done
test_evaluate_passes_a_clean_log_and_fails_each_violation
test_evaluate_without_marks_fails_any_failure
test_evaluate_input_errors_exit_2_naming_the_cause
test_evaluate_summary_counts_match_the_hand_computed_values

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
