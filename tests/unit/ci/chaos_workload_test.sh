#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the workload and consistency harness of the chaos suites
# (tests/e2e-chaos/lib/, #1387):
#   1. workload.py run, against a stub OpenStack it talks HTTP to: every
#      stream reaches READY and writes parseable records; a 503, a closed port
#      and a 401 each end up in the record the harness promises; a payload
#      or a provider listing that differs fails its read, and so does a
#      provider create without a Location; a bad command line exits 2 before
#      any request; --resolve reaches the stub by a name that resolves
#      nowhere, and --insecure-tls an HTTPS stub with a self-signed
#      certificate; the stop file sweeps, checks and ends in DONE; stuck
#      resources fail their check after the settle window.
#   2. workload.py evaluate, on hand-written logs: each violation exits 1,
#      each input error exits 2, and the summary line carries the counts.
#   3. workload.sh, against a stub kubectl: the pod start passes, each
#      verb's failure paths, the budget line and the two out-of-band checks.
#   4. The adoption: the thirty-four suites that start a workload carry one
#      valid budget line, verify and stop it and time every call;
#      deletion-stuck-finalizer says why it has none.
#   5. The wiring: every non-ovn e2e-chaos leg loads the Tempest image the
#      pod runs on, the pod and network legs load and deploy Placement for
#      the placement stream, and make shellcheck lints lib/*.sh and every
#      suite's probe-lib.sh.
#
# The stub serves Keystone, Nova, Neutron, Cinder, Glance, Placement,
# Barbican and a plain HTTP target on one localhost port, each under a path
# prefix of its own, and
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

CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

TMP_DIR="$(mktemp -d)"
STUB_PID=""
TLS_STUB_PID=""
cleanup() {
  local pid
  for pid in "$STUB_PID" "$TLS_STUB_PID"; do
    if [ -n "$pid" ]; then
      kill "$pid" 2>/dev/null || true
    fi
  done
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
# Like Barbican, the stub writes secret_ref under its host_href, a host the
# workload cannot reach.
SECRET_HREF = "http://barbican.example.invalid:9311/v1/secrets/"
LOCK = threading.Lock()


def fresh():
    return {"tokens": set(), "issued": 0, "requests": [], "fail": {},
            "fail_n": [], "bad_flavors": False,
            "unauthorized_once": [], "hold_build": False, "held_reads": 0,
            "error_volumes": 0, "most_volumes": 0, "bad_payload": False,
            "rp_dup": False, "rp_no_location": False,
            "servers": {}, "networks": {}, "ports": {}, "volumes": {},
            "attachments": {}, "images": {}, "ghosts": {},
            "resource_providers": {}, "secrets": {}, "rp_ghosts": {},
            "secret_ghosts": {}}


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
                    [time.time(), method, self.path, code,
                     self.headers.get("Host")])
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
        if data and "Content-Type" not in headers:
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
            STATE["unauthorized_once"].append((q["prefix"], q.get("method")))
        elif name == "bad-payload":
            STATE["bad_payload"] = True
        elif name == "rp-dup-listing":
            STATE["rp_dup"] = True
        elif name == "rp-no-location":
            STATE["rp_no_location"] = True
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
            STATE["rp_ghosts"]["rp-ghost"] = {"uuid": "rp-ghost", "name": f"chaos-wl-{q['id']}-placement-ghost"}
            STATE["secret_ghosts"]["s-ghost"] = {"id": "s-ghost", "name": f"chaos-wl-{q['id']}-secret-ghost"}
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
            STATE["resource_providers"]["rp-left"] = {"uuid": "rp-left", "name": p + "placement-99"}
            STATE["secrets"]["s-left"] = {"id": "s-left", "name": p + "secret-99", "payload": "x"}
            for n in range(1, 4):
                STATE["secrets"][f"s-keep{n}"] = {"id": f"s-keep{n}", "name": f"keep-{n}", "payload": "x"}
        elif name == "requests":
            return 200, STATE["requests"]
        elif name == "state":
            out = {k: sorted(v.get("name") or v["id"] for v in STATE[k].values())
                   for k in ("servers", "networks", "ports", "volumes", "attachments", "images",
                             "resource_providers", "secrets")}
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
        for rule in list(STATE["unauthorized_once"]):
            if path.startswith(rule[0]) and rule[1] in (None, method):
                STATE["unauthorized_once"].remove(rule)
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
        if path.startswith("/placement/resource_providers"):
            return self.placement(method, path[len("/placement/resource_providers"):], q, body)
        if path.startswith("/keymanager/v1/secrets"):
            return self.barbican(method, path[len("/keymanager/v1/secrets"):], q, body)
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

    def placement(self, method, rest, q, body):
        rps = STATE["resource_providers"]
        # Below microversion 1.20 a create answers 201 with no body.
        if self.headers.get("OpenStack-API-Version"):
            return 400, {"error": "the stub serves microversion 1.0 only"}, {}
        if rest == "" and method == "POST":
            name = (body or {}).get("name")
            if not name or any(r["name"] == name for r in rps.values()):
                return 409, {"error": "bad or duplicate name"}, {}
            rid = new_id()
            rps[rid] = {"uuid": rid, "name": name}
            if STATE["rp_no_location"]:
                return 201, None, {}
            return 201, None, {"Location": "/placement/resource_providers/" + rid}
        if rest == "" and method == "GET":
            listed = list(rps.values()) + list(STATE["rp_ghosts"].values())
            if "name" in q:
                listed = [r for r in listed if r["name"] == q["name"]]
                # A second provider of the name, which Placement must never
                # list.
                if STATE["rp_dup"]:
                    listed.append({"uuid": "rp-dup", "name": q["name"]})
            return 200, {"resource_providers": listed}, {}
        rid = rest.lstrip("/")
        if rid not in rps:
            return 404, {"error": "no resource provider"}, {}
        if method == "GET":
            return 200, rps[rid], {}
        if method == "DELETE":
            del rps[rid]
            return 204, None, {}
        return 405, None, {}

    def barbican(self, method, rest, q, body):
        secrets = STATE["secrets"]
        if rest == "" and method == "POST":
            b = body or {}
            if b.get("payload_content_type") != "text/plain" or b.get("secret_type") != "passphrase" \
                    or not b.get("payload") or not b.get("name"):
                return 400, {"error": "want a text/plain passphrase with a name"}, {}
            sid = new_id()
            secrets[sid] = {"id": sid, "name": b["name"], "payload": b["payload"]}
            return 201, {"secret_ref": SECRET_HREF + sid}, {}
        if rest == "" and method == "GET":
            # Pages of two, as a Barbican whose max_limit_paging is 2 answers
            # a limit of 100.
            if q.get("limit") != "100":
                return 400, {"error": "want limit=100"}, {}
            listed = sorted(list(secrets.values()) + list(STATE["secret_ghosts"].values()),
                            key=lambda s: s["id"])
            offset = int(q.get("offset", "0"))
            page = [{"secret_ref": SECRET_HREF + s["id"], "name": s["name"]}
                    for s in listed[offset:offset + 2]]
            return 200, {"secrets": page, "total": len(listed)}, {}
        sid, _, sub = rest.lstrip("/").partition("/")
        if sid not in secrets:
            return 404, {"error": "no secret"}, {}
        if sub == "payload" and method == "GET":
            if self.headers.get("Accept") != "text/plain":
                return 406, {"error": "want Accept: text/plain"}, {}
            payload = secrets[sid]["payload"] + ("x" if STATE["bad_payload"] else "")
            return 200, payload, {"Content-Type": "text/plain"}
        if sub == "" and method == "GET":
            return 200, {"secret_ref": SECRET_HREF + sid, "name": secrets[sid]["name"]}, {}
        if sub == "" and method == "DELETE":
            del secrets[sid]
            return 204, None, {}
        return 405, None, {}


server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
# With a certificate and its key the stub serves HTTPS.
if len(sys.argv) == 4:
    import ssl
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.load_cert_chain(sys.argv[2], sys.argv[3])
    server.socket = tls.wrap_socket(server.socket, server_side=True)
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
  --placement "$STUB/placement"
  --barbican "$STUB/keymanager"
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
  echo "Test: run reaches READY on all eight streams and writes a parseable record per operation"
  ctl POST /_control/reset >/dev/null

  start_run ready -- --streams token,http,network,server,volume,image,placement,secret "${ENDPOINTS[@]}"
  wait_for "eight READY lines" 30 ready_on token http network server volume image placement secret
  finish_run

  assert_eq "run exits 0" "0" "$RUN_RC"
  local s
  for s in token http network server volume image placement secret; do
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
    "image create" "image upload" "image active" "image delete" \
    "placement create-rp" "placement list-rp" "placement delete-rp" \
    "secret create-secret" "secret read-payload" "secret delete-secret"; do
    assert_contains "an ok record for $op" "$ops" "$op true"
  done
  assert_eq "no failed record" "" "$(records 'not r["ok"]')"
  local reqs
  reqs="$(ctl GET /_control/requests)"
  assert_contains "the placement stream finds its provider by name" "$reqs" \
    '"GET", "/placement/resource_providers?name=chaos-wl-t1-placement-1", 200'
  assert_contains "the secret stream reads the payload at its own root, not at secret_ref's host" "$reqs" \
    '"GET", "/keymanager/v1/secrets/'
  assert_eq "every stream-created provider and secret is gone" "[[], []]" \
    "$(ctl GET /_control/state | jq_py '[d["resource_providers"], d["secrets"]]')"
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

# --- Test 2b --------------------------------------------------------------
test_a_503_on_create_fails_placement_and_secret_while_token_keeps_going() {
  echo "Test: a 503 to POST /resource_providers and POST /v1/secrets fails create-rp and create-secret while the token stream stays ok"
  ctl POST /_control/reset >/dev/null
  ctl POST "/_control/fail-n?prefix=/placement/resource_providers&method=POST&code=503&count=3" >/dev/null
  ctl POST "/_control/fail-n?prefix=/keymanager/v1/secrets&method=POST&code=503&count=3" >/dev/null

  start_run create503 -- --streams token,placement,secret "${ENDPOINTS[@]}"
  wait_for "READY on all three streams after the failed creates" 30 ready_on token placement secret
  finish_run

  assert_eq "run exits 0" "0" "$RUN_RC"
  assert_eq "three create-rp records are ok: false with code 503" \
    "$(printf 'placement create-rp false 503\nplacement create-rp false 503\nplacement create-rp false 503')" \
    "$(records 'not r["ok"] and r["s"] == "placement"')"
  assert_eq "three create-secret records are ok: false with code 503" \
    "$(printf 'secret create-secret false 503\nsecret create-secret false 503\nsecret create-secret false 503')" \
    "$(records 'not r["ok"] and r["s"] == "secret"')"
  assert_contains "the token stream kept writing ok records" "$(records 'r["s"] == "token"')" "token validate true 200"
  assert_eq "no token record failed" "" "$(records 'r["s"] == "token" and not r["ok"]')"
  assert_eq "a failed create leaves nothing to clean up" "" "$(records 'r["op"].startswith("cleanup-")')"
}

# --- Test 2c --------------------------------------------------------------
test_resolve_sends_the_request_to_the_mapped_address() {
  echo "Test: --resolve reaches the stub by a name that resolves nowhere and keeps that name in the Host header"
  ctl POST /_control/reset >/dev/null
  local port name
  port="$(cat "$TMP_DIR/port")"
  # The .invalid top-level domain never resolves, so a passing run proves the
  # mapping.
  name="chaos-stub.invalid"

  start_run resolve -- --streams token,http --keystone "http://${name}:${port}/identity/v3" \
    --http "$STUB/http/ok" --resolve "${name}:127.0.0.1"
  wait_for "READY on token and http" 30 ready_on token http
  finish_run

  assert_eq "run exits 0" "0" "$RUN_RC"
  local token
  token="$(records 'r["s"] == "token"')"
  assert_contains "an ok issue record" "$token" "token issue true 201"
  assert_contains "an ok validate record" "$token" "token validate true 200"
  assert_eq "no failed record" "" "$(records 'not r["ok"]')"
  local hosts
  hosts="$(ctl GET /_control/requests | jq_py 'sorted(set(x[4] for x in d))')"
  assert_contains "the token requests carry the name in the Host header" "$hosts" "'${name}:${port}'"
  assert_contains "the http stream's address resolves as before" "$hosts" "'127.0.0.1:${port}'"

  ctl POST /_control/reset >/dev/null
  local closed first
  closed="$(closed_port)"
  start_run resolve-closed -- --streams token --keystone "http://${name}:${closed}/identity/v3" \
    --resolve "${name}:127.0.0.1"
  wait_for "two token records" 30 bash -c "[ \"\$(grep -c '\"s\": \"token\"' '$RUN_OUT')\" -ge 2 ]"
  finish_run

  first="$(grep -m1 '"s": "token"' "$RUN_OUT")"
  assert_contains "a closed port behind the mapping is an issue record" "$first" '"op": "issue"'
  assert_contains "it is not ok" "$first" '"ok": false'
  assert_contains "its code is null" "$first" '"code": null'
  assert_contains "its msg holds the URLError" "$first" 'URLError'
  assert_eq "the token stream never printed READY" "0" "$(grep -cx 'READY token' "$RUN_OUT")"

  # The value splits on its first colon, so an IPv6 ADDR is accepted. Nothing
  # listens behind it, which needs no IPv6 stub: the stream still has to start.
  start_run resolve-v6 -- --streams token --keystone "http://${name}:${closed}/identity/v3" \
    --resolve "${name}:::1"
  wait_for "a token record" 30 grep -q '"s": "token"' "$RUN_OUT"
  finish_run

  assert_not_contains "--resolve ${name}:::1 is no usage error" "$(cat "$RUN_ERR")" \
    "usage: --resolve wants HOST:ADDR"
  assert_contains "the token stream writes an issue record behind the IPv6 ADDR" \
    "$(grep -m1 '"s": "token"' "$RUN_OUT")" '"op": "issue"'
}

# --- Test 2d --------------------------------------------------------------
test_insecure_tls_is_needed_against_a_self_signed_stub() {
  echo "Test: an HTTPS stub with a self-signed certificate answers with --insecure-tls and fails verification without it"
  if ! command -v openssl >/dev/null 2>&1; then
    echo "  SKIP: openssl not installed (TLS checks skipped)"
    SKIP=$((SKIP + 1))
    return 0
  fi
  local name="chaos-stub.invalid" port
  if ! openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=${name}" \
    -addext "subjectAltName=DNS:${name}" -keyout "$TMP_DIR/tls.key" -out "$TMP_DIR/tls.crt" \
    >"$TMP_DIR/openssl.log" 2>&1; then
    FAIL=$((FAIL + 1))
    echo "  FAIL: openssl req -x509 could not write a certificate: $(cat "$TMP_DIR/openssl.log")"
    return 0
  fi
  python3 -I "$TMP_DIR/stub_server.py" "$TMP_DIR/tls-port" "$TMP_DIR/tls.crt" "$TMP_DIR/tls.key" &
  TLS_STUB_PID=$!
  wait_for "the HTTPS stub's port" 10 test -f "$TMP_DIR/tls-port"
  port="$(cat "$TMP_DIR/tls-port")"

  start_run tls -- --streams token --keystone "https://${name}:${port}/identity/v3" \
    --resolve "${name}:127.0.0.1" --insecure-tls
  wait_for "READY token" 30 ready_on token
  finish_run

  assert_eq "run with --insecure-tls exits 0" "0" "$RUN_RC"
  local token
  token="$(records 'r["s"] == "token"')"
  assert_contains "an ok issue record over HTTPS" "$token" "token issue true 201"
  assert_contains "an ok validate record over HTTPS" "$token" "token validate true 200"
  assert_eq "no failed record" "" "$(records 'not r["ok"]')"

  start_run tls-verify -- --streams token --keystone "https://${name}:${port}/identity/v3" \
    --resolve "${name}:127.0.0.1"
  wait_for "two token records" 30 bash -c "[ \"\$(grep -c '\"s\": \"token\"' '$RUN_OUT')\" -ge 2 ]"
  finish_run

  local first
  first="$(grep -m1 '"s": "token"' "$RUN_OUT")"
  assert_contains "without --insecure-tls the issue record is not ok" "$first" '"ok": false'
  assert_contains "its code is null" "$first" '"code": null'
  assert_contains "its msg names the failed verification" "$first" 'CERTIFICATE_VERIFY_FAILED'
  assert_eq "the token stream never printed READY" "0" "$(grep -cx 'READY token' "$RUN_OUT")"

  kill "$TLS_STUB_PID" 2>/dev/null || true
  wait "$TLS_STUB_PID" 2>/dev/null || true
  TLS_STUB_PID=""
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

# --- Test 3d --------------------------------------------------------------
test_a_payload_that_differs_fails_read_payload() {
  echo "Test: a payload that differs from the one stored fails read-payload and the cycle deletes its secret"
  ctl POST /_control/reset >/dev/null

  start_run payload -- --streams secret "${ENDPOINTS[@]}"
  wait_for "READY secret" 30 ready_on secret
  ctl POST /_control/bad-payload >/dev/null
  wait_for "a failed read-payload record" 30 grep -q '"op": "read-payload", "ok": false' "$RUN_OUT"
  finish_run

  assert_contains "read-payload fails on the 200 that carried another payload" \
    "$(records 'r["op"] == "read-payload" and not r["ok"]')" "secret read-payload false 200"
  assert_contains "it says the payload differs" "$(grep -m1 '"op": "read-payload", "ok": false' "$RUN_OUT")" \
    "answered another payload"
  assert_contains "the failed cycle deletes its secret" "$(records 'r["op"] == "cleanup-secret"')" \
    "secret cleanup-secret true 204"
  assert_eq "no secret is left at the stub" "[]" "$(ctl GET /_control/state | jq_py 'd["secrets"]')"
}

# --- Test 3e --------------------------------------------------------------
test_a_closed_placement_port_records_a_null_code() {
  echo "Test: a closed --placement port is a create-rp record with code null and the URLError text"
  ctl POST /_control/reset >/dev/null
  local port
  port="$(closed_port)"

  start_run closed-rp CHAOS_WORKLOAD_SETTLE_SECONDS=1 -- --streams placement --keystone "$STUB/identity/v3" \
    --placement "http://127.0.0.1:${port}/"
  wait_for "two placement records" 30 bash -c "[ \"\$(grep -c '\"s\": \"placement\"' '$RUN_OUT')\" -ge 2 ]"
  finish_run

  local first
  first="$(grep -m1 '"s": "placement"' "$RUN_OUT")"
  assert_contains "the first record is create-rp" "$first" '"op": "create-rp"'
  assert_contains "it is not ok" "$first" '"ok": false'
  assert_contains "its code is null" "$first" '"code": null'
  assert_contains "its msg holds the URLError" "$first" 'URLError'
  assert_eq "the placement stream never printed READY" "0" "$(grep -cx 'READY placement' "$RUN_OUT")"
  assert_contains "the check reports the unreachable API" "$(cat "$RUN_OUT")" \
    "CONSISTENCY-FAIL resource-providers: URLError"
}

# --- Test 3f --------------------------------------------------------------
test_a_provider_listing_that_differs_fails_list_rp() {
  echo "Test: a listing by name that holds another provider fails list-rp and the cycle deletes its provider"
  ctl POST /_control/reset >/dev/null

  start_run listrp -- --streams placement "${ENDPOINTS[@]}"
  wait_for "READY placement" 30 ready_on placement
  ctl POST /_control/rp-dup-listing >/dev/null
  wait_for "a failed list-rp record" 30 grep -q '"op": "list-rp", "ok": false' "$RUN_OUT"
  finish_run

  assert_contains "list-rp fails on the 200 that lists a second provider" \
    "$(records 'r["op"] == "list-rp" and not r["ok"]')" "placement list-rp false 200"
  assert_contains "it names what the listing held" "$(grep -m1 '"op": "list-rp", "ok": false' "$RUN_OUT")" \
    "rp-dup"
  assert_contains "the failed cycle deletes its provider" "$(records 'r["op"] == "cleanup-resource-provider"')" \
    "placement cleanup-resource-provider true 204"
  assert_eq "no provider is left at the stub" "[]" "$(ctl GET /_control/state | jq_py 'd["resource_providers"]')"
}

# --- Test 3g --------------------------------------------------------------
test_a_create_without_a_location_fails_create_rp() {
  echo "Test: a 201 without a Location fails create-rp, and the sweep deletes the provider it created"
  ctl POST /_control/reset >/dev/null

  start_run nolocation -- --streams placement "${ENDPOINTS[@]}"
  wait_for "READY placement" 30 ready_on placement
  ctl POST /_control/rp-no-location >/dev/null
  wait_for "a failed create-rp record" 30 grep -q '"op": "create-rp", "ok": false' "$RUN_OUT"
  finish_run

  assert_contains "create-rp fails on the 201 that names no provider" \
    "$(records 'r["op"] == "create-rp" and not r["ok"]')" "placement create-rp false 201"
  assert_contains "it says the Location is missing" "$(grep -m1 '"op": "create-rp", "ok": false' "$RUN_OUT")" \
    "answered without a Location"
  assert_eq "the cycle had nothing to delete" "" "$(records 'r["op"] == "cleanup-resource-provider"')"
  assert_eq "the sweep deletes what the failed creates left" "[]" \
    "$(ctl GET /_control/state | jq_py 'd["resource_providers"]')"
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

# --- Test 4c --------------------------------------------------------------
test_a_401_to_the_provider_listing_reissues_the_token() {
  echo "Test: a 401 to GET /resource_providers?name= issues one new token and the listing succeeds on the retry"
  ctl POST /_control/reset >/dev/null

  start_run reauth-rp -- --streams placement "${ENDPOINTS[@]}"
  wait_for "READY placement" 30 ready_on placement
  ctl POST "/_control/unauthorized-once?prefix=/placement/resource_providers&method=GET" >/dev/null
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
print(refused[1], refused[2].split("=")[0])
print(issue[1], issue[2], issue[3])
print("same call" if (retry[1], retry[2]) == (refused[1], refused[2]) else "other call", retry[3])')"
  assert_contains "the 401 answered the listing by name" "$verdict" "GET /placement/resource_providers?name"
  assert_contains "the 401 is followed by a token issue" "$verdict" "POST /identity/v3/auth/tokens 201"
  assert_contains "and by the same listing again, which succeeds" "$verdict" "same call 200"
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
  # <label>|<run options>|<the usage line>
  local missing=(
    "placement without --placement|--streams=placement --keystone=$STUB/identity/v3|usage: stream placement needs --placement"
    "secret without --barbican|--streams=secret --keystone=$STUB/identity/v3|usage: stream secret needs --barbican"
    "placement without --keystone|--streams=placement --placement=$STUB/placement|usage: stream placement needs --keystone"
    "secret without --keystone|--streams=secret --barbican=$STUB/keymanager|usage: stream secret needs --keystone"
  )
  local want
  for entry in "${missing[@]}"; do
    label="${entry%%|*}"
    want="${entry##*|}"
    args="${entry#*|}"
    args="${args%|*}"
    rc=0
    # shellcheck disable=SC2086 # the case strings are split on purpose
    out="$(OS_PASSWORD=s3cret python3 -I "$WORKLOAD_PY" run --id t1 --stop-file "$TMP_DIR/x.stop" $args 2>&1)" || rc=$?
    assert_eq "${label}: exit 2" "2" "$rc"
    assert_eq "${label}: the usage line names the option" "$want" "$out"
  done
  local value
  for value in nocolon :127.0.0.1 chaos-stub.invalid: chaos-stub.invalid:not-an-ip; do
    rc=0
    out="$(OS_PASSWORD=s3cret python3 -I "$WORKLOAD_PY" run --id t1 --stop-file "$TMP_DIR/x.stop" \
      --streams token --keystone "$STUB/identity/v3" --resolve "$value" 2>&1)" || rc=$?
    assert_eq "--resolve ${value}: exit 2" "2" "$rc"
    assert_eq "--resolve ${value}: the usage line" "usage: --resolve wants HOST:ADDR" "$out"
  done
  rc=0
  out="$(OS_PASSWORD=s3cret python3 -I "$WORKLOAD_PY" run --id t1 --stop-file "$TMP_DIR/x.stop" --streams=bogus 2>&1)" || rc=$?
  assert_contains "the usage line lists all eight streams" "$out" \
    "known streams are token,http,network,server,volume,image,placement,secret"
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

  start_run sweep -- --streams token,network,server,volume,image,placement,secret "${ENDPOINTS[@]}"
  wait_for "READY on the six resource streams" 30 ready_on network server volume image placement secret
  # The next server stays in BUILD, so the stop file lands while the server
  # stream waits for ACTIVE.
  ctl POST /_control/hold-build >/dev/null
  wait_for "a read of a held server" 30 bash -c \
    "[ \"\$(python3 -I '$TMP_DIR/stub_ctl.py' '$STUB' GET /_control/state | python3 -I -c 'import json,sys; print(json.load(sys.stdin)[\"held_reads\"])')\" -ge 1 ]"
  finish_run

  assert_eq "run exits 0" "0" "$RUN_RC"
  assert_eq "DONE is the last line" "DONE" "$(tail -n1 "$RUN_OUT")"
  local check
  for check in networks servers volumes images resource-providers secrets; do
    assert_eq "CONSISTENCY-OK ${check}" "1" "$(grep -cx "CONSISTENCY-OK ${check}" "$RUN_OUT")"
  done
  assert_eq "no CONSISTENCY-FAIL" "0" "$(grep -c '^CONSISTENCY-FAIL' "$RUN_OUT")"

  local state
  state="$(ctl GET /_control/state)"
  assert_eq "no prefixed resource is left at the stub" "[]" \
    "$(jq_py '[n for k in ("servers", "networks", "ports", "volumes", "images", "resource_providers", "secrets") for n in d[k] if n.startswith("chaos-wl-t1-")]' <<<"$state")"
  assert_eq "the leftover volume's attachment is gone" "[]" "$(jq_py 'd["attachments"]' <<<"$state")"
  assert_eq "the unprefixed images stay" "['keep-1', 'keep-2', 'keep-3']" "$(jq_py 'd["images"]' <<<"$state")"
  # The leftover secret sits on the second page of two, past the three
  # unprefixed ones.
  assert_eq "the unprefixed secrets stay" "['keep-1', 'keep-2', 'keep-3']" "$(jq_py 'd["secrets"]' <<<"$state")"

  local sweeps kind
  sweeps="$(records 'r["op"].startswith("sweep-")')"
  for kind in servers ports networks volumes attachments images resource-providers secrets; do
    assert_contains "an ok sweep-${kind} record" "$sweeps" "sweep-${kind} true"
  done
  assert_eq "no sweep record failed" "" "$(grep -v ' true ' <<<"$sweeps")"
  assert_eq "the held server has a create record and no active record" \
    "$(records 'r["s"] == "server" and r["op"] == "create" and r["ok"]' | wc -l)" \
    "$(($(records 'r["s"] == "server" and r["op"] == "active"' | wc -l) + 1))"
}

# --- Test 7 ---------------------------------------------------------------
test_stuck_resources_fail_the_checks_after_the_settle_window() {
  echo "Test: stuck resources fail all six checks, each after re-reading for the settle window"
  ctl POST /_control/reset >/dev/null

  start_run stuck CHAOS_WORKLOAD_SETTLE_SECONDS=2 -- --streams network,server,volume,image,placement,secret "${ENDPOINTS[@]}"
  wait_for "READY on the six resource streams" 30 ready_on network server volume image placement secret
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
  assert_contains "CONSISTENCY-FAIL resource-providers names the prefixed provider" "$out" \
    "CONSISTENCY-FAIL resource-providers: resource provider chaos-wl-t1-placement-ghost (rp-ghost) is left over"
  assert_contains "CONSISTENCY-FAIL secrets names the prefixed secret" "$out" \
    "CONSISTENCY-FAIL secrets: secret chaos-wl-t1-secret-ghost (s-ghost) is left over"

  local spans
  spans="$(ctl GET /_control/requests | python3 -I -c '
import json, sys
r = json.load(sys.stdin)
for path in ("/compute/v2.1/servers/detail?all_tenants=1",
             "/volume/v3/proj1/volumes/detail?all_tenants=1",
             "/image/v2/images", "/network/v2.0/networks",
             "/placement/resource_providers",
             "/keymanager/v1/secrets?limit=100&offset=0"):
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

  # An operation still in flight when the fault starts fails because of it,
  # so the lower edge reads its end: begun at 999.4, failed 17 s later (the
  # volume stream's gone poll in a local run of cinder-mariadb-outage).
  evaluate_log straddle "${base}"'{"t": 999.4, "s": "volume", "op": "gone", "ok": false, "code": null, "ms": 17027, "msg": ""}'$'\n'"$(rec 1200 volume get true)"$'\n'"DONE"$'\n' \
    --budget-failures 1 "${MARKS[@]}"
  assert_eq "a failure begun before fault_start and ended after it: exit 0" "0" "$EVAL_RC"
  assert_contains "it counts against the budget" "$EVAL_OUT" "stream volume: 1 ok, 1 failed in window (budget 1), 0 outside"

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

# ---------------------------------------------------------------------------
# workload.sh, against a stub kubectl
# ---------------------------------------------------------------------------

WORKLOAD_SH="$LIB_DIR/workload.sh"
KSTUB="$TMP_DIR/kstub"
mkdir -p "$KSTUB/bin"

# The stub kubectl keeps its state in $STUB_DIR:
#   calls        one line per call; an argument over 300 bytes is written as
#                <N bytes>, the source after `run ... -c` lands in
#                run-source and the --overrides JSON in overrides
#   log          what `logs <pod>` prints; log.after is appended to it when
#                the stop file is touched, log.later on the
#                $STUB_READY_AFTER-th `logs <pod>` call
#   clock        the pod clock `exec ... date +%s` reads; each read adds 10
#   ann-<mark>   the annotations `annotate` wrote
# and reads these switches: STUB_PHASE (Running), STUB_POD_MISSING,
# STUB_EXEC_FAIL (the date exec fails with it), STUB_TOUCH_FAIL,
# STUB_AUDIT_RC and STUB_AUDIT_OUT, STUB_CRONJOB (the sync mode the CronJob's
# command carries; unset, there is no CronJob), STUB_JOB_STATE, STUB_JOB_LOG.
cat >"$KSTUB/bin/kubectl" <<'SH'
#!/bin/bash
{
  for a in "$@"; do
    if [ "${#a}" -gt 300 ]; then printf '<%d bytes> ' "${#a}"; else printf '%s ' "$a"; fi
  done
  echo
} >>"$STUB_DIR/calls"
missing() {
  echo "Error from server (NotFound): $1 \"$2\" not found" >&2
  exit 1
}
args="$*"
case "$1" in
  run)
    prev=""
    for a in "$@"; do
      if [ "$prev" = "-c" ]; then printf '%s' "$a" >"$STUB_DIR/run-source"; fi
      case "$a" in --overrides=*) printf '%s' "${a#--overrides=}" >"$STUB_DIR/overrides" ;; esac
      prev="$a"
    done
    ;;
  delete | create) ;;
  annotate)
    for a in "$@"; do
      case "$a" in
        chaos.c5c3.io/*=*) k="${a%%=*}"; printf '%s' "${a#*=}" >"$STUB_DIR/ann-${k#chaos.c5c3.io/}" ;;
      esac
    done
    ;;
  logs)
    case "$args" in
      *job/*) printf '%s\n' "${STUB_JOB_LOG:-}"; exit 0 ;;
    esac
    if [ -n "${STUB_POD_MISSING:-}" ]; then missing pods "$2"; fi
    n=$(( $(cat "$STUB_DIR/logs.calls" 2>/dev/null || echo 0) + 1 ))
    echo "$n" >"$STUB_DIR/logs.calls"
    if [ "$n" = "${STUB_READY_AFTER:-}" ]; then cat "$STUB_DIR/log.later" >>"$STUB_DIR/log"; fi
    cat "$STUB_DIR/log" 2>/dev/null
    ;;
  describe)
    if [ -n "${STUB_POD_MISSING:-}" ]; then missing pods "$3"; fi
    printf 'Name:         %s\nStatus:       %s\n    Command:\n      python3\n      -c\n      """Workload and consistency harness."""\n    State:        Running\n    Environment:\n      OS_PASSWORD:  s3cret\n' "$3" "${STUB_PHASE:-Running}"
    ;;
  exec)
    case "$args" in
      *"date +%s.%N"*)
        if [ -n "${STUB_EXEC_FAIL:-}" ]; then echo "$STUB_EXEC_FAIL" >&2; exit 1; fi
        echo "${STUB_CLOCK:-1760000123.456789012}"
        ;;
      *"date +%s"*)
        if [ -n "${STUB_EXEC_FAIL:-}" ]; then echo "$STUB_EXEC_FAIL" >&2; exit 1; fi
        c="$(cat "$STUB_DIR/clock")"
        echo "$c"
        echo $((c + 10)) >"$STUB_DIR/clock"
        ;;
      *"touch /tmp/chaos-workload.stop"*)
        if [ -n "${STUB_TOUCH_FAIL:-}" ]; then echo "$STUB_TOUCH_FAIL" >&2; exit 1; fi
        cat "$STUB_DIR/log.after" >>"$STUB_DIR/log" 2>/dev/null
        ;;
      *nova-manage*)
        printf '%s\n' "${STUB_AUDIT_OUT:-}"
        exit "${STUB_AUDIT_RC:-0}"
        ;;
      *) echo "stub kubectl: unexpected exec: $args" >&2; exit 1 ;;
    esac
    ;;
  get)
    case "$2" in
      secret) printf '%s' "$(printf 's3cret' | base64)" ;;
      cronjob)
        if [ -z "${STUB_CRONJOB:-}" ]; then missing cronjobs.batch "$3"; fi
        printf '["neutron-ovn-db-sync-util","--config-file","/etc/neutron/neutron.conf","--ovn-neutron_sync_mode","%s"]' "$STUB_CRONJOB"
        ;;
      job) printf '%s' "${STUB_JOB_STATE:-}" ;;
      pod)
        if [ -n "${STUB_POD_MISSING:-}" ]; then missing pods "$3"; fi
        case "$args" in
          *phase=*) printf 'phase=%s fault-start=%s fault-end=%s\n' "${STUB_PHASE:-Running}" \
            "$(cat "$STUB_DIR/ann-fault-start" 2>/dev/null)" "$(cat "$STUB_DIR/ann-fault-end" 2>/dev/null)" ;;
          *fault-start*) cat "$STUB_DIR/ann-fault-start" 2>/dev/null ;;
          *fault-end*) cat "$STUB_DIR/ann-fault-end" 2>/dev/null ;;
          *status.phase*) printf '%s' "${STUB_PHASE:-Running}" ;;
        esac
        ;;
      *) echo "stub kubectl: unexpected get: $args" >&2; exit 1 ;;
    esac
    ;;
  *) echo "stub kubectl: unexpected call: $args" >&2; exit 1 ;;
esac
exit 0
SH
printf '#!/bin/sh\nexit 0\n' >"$KSTUB/bin/sleep"
chmod +x "$KSTUB/bin/kubectl" "$KSTUB/bin/sleep"

# new_stub — empty the stub state; the pod clock starts at 1000.
new_stub() {
  rm -rf "$KSTUB/state"
  mkdir -p "$KSTUB/state"
  : >"$KSTUB/state/calls"
  : >"$KSTUB/state/log"
  echo 1000 >"$KSTUB/state/clock"
}

# wl <dir> [VAR=value...] -- <workload.sh args> — run workload.sh from <dir>
# with the stub first on PATH; WL_OUT and WL_RC hold the result.
wl() {
  local dir="$1"
  shift
  local envs=()
  while [ "$1" != "--" ]; do
    envs+=("$1")
    shift
  done
  shift
  WL_RC=0
  WL_OUT="$(cd "$dir" && env STUB_DIR="$KSTUB/state" NAMESPACE=openstack PATH="$KSTUB/bin:$PATH" \
    ${envs[@]+"${envs[@]}"} bash "$WORKLOAD_SH" "$@" 2>&1)" || WL_RC=$?
}

calls() {
  cat "$KSTUB/state/calls"
}

# suite_dir <name> <header lines> — a scratch suite directory whose
# chainsaw-test.yaml starts with the given header.
suite_dir() {
  mkdir -p "$TMP_DIR/suites/$1"
  printf '%s\napiVersion: chainsaw.kyverno.io/v1alpha1\nkind: Test\n' "$2" >"$TMP_DIR/suites/$1/chainsaw-test.yaml"
  echo "$TMP_DIR/suites/$1"
}

# --- Test 13 --------------------------------------------------------------
test_start_runs_the_pod_as_specified() {
  echo "Test: start runs chaos-wl-<id> with both labels, the image, the password only with --keystone, and waits for READY"
  new_stub
  printf 'READY token\nREADY http\n' >"$KSTUB/state/log.later"
  wl "$TMP_DIR" STUB_READY_AFTER=3 -- start t-one -- --streams token,http --keystone http://k:5000/v3 --http http://h/
  assert_eq "start exits 0" "0" "$WL_RC"
  assert_contains "it reports READY" "$WL_OUT" "OK: workload chaos-wl-t-one is READY on token,http"
  local c
  c="$(calls)"
  assert_contains "a leftover pod is deleted first" "$(head -n1 <<<"$c")" \
    "delete pod chaos-wl-t-one -n openstack --ignore-not-found --wait=true --timeout=60s"
  local run
  run="$(grep '^run ' <<<"$c")"
  assert_contains "the pod is chaos-wl-<id>" "$run" "run chaos-wl-t-one -n openstack"
  assert_contains "both labels" "$run" "--labels=app.kubernetes.io/name=chaos-workload,chaos.c5c3.io/workload=t-one"
  assert_contains "the default image" "$run" "--image=ghcr.io/c5c3/tempest:2026.1 --restart=Never"
  assert_contains "the admin password with --keystone" "$run" "--env OS_PASSWORD=s3cret"
  assert_contains "the run options follow the source" "$run" \
    "--command -- python3 -c <"
  assert_contains "with the id and the stop file" "$run" \
    "run --id t-one --stop-file /tmp/chaos-workload.stop --streams token,http --keystone http://k:5000/v3 --http http://h/"
  assert_eq "the source is workload.py" "$(cat "$WORKLOAD_PY")" "$(cat "$KSTUB/state/run-source")"
  local container
  container="$(jq_py '[c for c in d["spec"]["containers"]]' <"$KSTUB/state/overrides")"
  assert_contains "the overrides name the container kubectl run creates" "$container" "'name': 'chaos-wl-t-one'"
  assert_contains "the pod gets requests and a memory limit" "$container" \
    "'resources': {'requests': {'cpu': '50m', 'memory': '128Mi'}, 'limits': {'memory': '256Mi'}}"
  assert_contains "and runs as the image's non-root user without privileges" "$container" \
    "'runAsNonRoot': True, 'runAsUser': 42424, 'runAsGroup': 42424, 'allowPrivilegeEscalation': False, 'capabilities': {'drop': ['ALL']}"
  assert_eq "the pod mounts no ServiceAccount token" "False" \
    "$(jq_py 'd["spec"]["automountServiceAccountToken"]' <"$KSTUB/state/overrides")"
  assert_contains "merged strategically" "$run" "--override-type=strategic"
  assert_eq "the log was read until both READY lines were there" "3" "$(cat "$KSTUB/state/logs.calls")"

  new_stub
  printf 'READY http\n' >"$KSTUB/state/log"
  wl "$TMP_DIR" CHAOS_WORKLOAD_IMAGE=registry.example/tempest:x -- start t-two -- --streams http --http http://h/
  assert_eq "start without --keystone exits 0" "0" "$WL_RC"
  c="$(calls)"
  assert_not_contains "no password without --keystone" "$c" "OS_PASSWORD"
  assert_not_contains "and no Secret read" "$c" "get secret"
  assert_contains "CHAOS_WORKLOAD_IMAGE sets the image" "$c" "--image=registry.example/tempest:x "
}

# --- Test 14 --------------------------------------------------------------
test_start_fails_on_a_terminal_pod_or_a_missing_ready() {
  echo "Test: start fails on a pod that ended before READY and on a READY that never comes"
  new_stub
  printf 'usage: stream network needs --keystone\n' >"$KSTUB/state/log"
  wl "$TMP_DIR" STUB_PHASE=Failed -- start t1 -- --streams network --neutron http://n/
  assert_eq "a Failed pod: exit 1" "1" "$WL_RC"
  assert_contains "it names the phase" "$WL_OUT" "FAIL: start t1: the pod reached Failed before READY on: network"
  assert_contains "it prints the log" "$WL_OUT" "usage: stream network needs --keystone"
  assert_contains "and the pod description" "$WL_OUT" "Name:         chaos-wl-t1"
  assert_contains "with the password cut out" "$WL_OUT" "OS_PASSWORD:  <redacted>"
  assert_not_contains "nowhere in clear text" "$WL_OUT" "s3cret"
  assert_contains "and the command in short" "$WL_OUT" "Command:      python3 -c <lib/workload.py> run ..."
  assert_not_contains "without the source of workload.py" "$WL_OUT" "Workload and consistency harness."
  assert_contains "the lines after the command stay" "$WL_OUT" "State:        Running"

  new_stub
  printf 'READY token\n' >"$KSTUB/state/log"
  wl "$TMP_DIR" CHAOS_WORKLOAD_READY_SECONDS=1 -- start t1 -- --streams token,http --keystone http://k/v3 --http http://h/
  assert_eq "a READY that never comes: exit 1" "1" "$WL_RC"
  assert_contains "it names the missing stream and the bound" "$WL_OUT" "FAIL: start t1: no READY within 1s on: http"
  assert_contains "it prints the log" "$WL_OUT" "READY token"
}

# --- Test 15 --------------------------------------------------------------
test_start_rejects_an_invalid_id() {
  echo "Test: start refuses an id outside ^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$"
  local id40 id41
  id40="$(printf 'a%.0s' $(seq 1 40))"
  id41="${id40}a"
  new_stub
  wl "$TMP_DIR" -- start Bad_Id -- --streams http --http http://h/
  assert_eq "Bad_Id: exit 2" "2" "$WL_RC"
  assert_contains "it quotes the id" "$WL_OUT" "invalid id 'Bad_Id'"
  wl "$TMP_DIR" -- start "$id41" -- --streams http --http http://h/
  assert_eq "41 characters: exit 2" "2" "$WL_RC"
  wl "$TMP_DIR" -- start -leading -- --streams http --http http://h/
  assert_eq "a leading dash: exit 2" "2" "$WL_RC"
  assert_eq "kubectl was never called" "" "$(calls)"
  printf 'READY http\n' >"$KSTUB/state/log"
  wl "$TMP_DIR" -- start "$id40" -- --streams http --http http://h/
  assert_eq "40 characters are accepted" "0" "$WL_RC"
  wl "$TMP_DIR" -- start t1 --streams http
  assert_eq "start without the -- separator: exit 2" "2" "$WL_RC"
}

# --- Test 16 --------------------------------------------------------------
test_mark_writes_the_pod_clock_and_fails_with_the_exec_error() {
  echo "Test: mark writes the pod clock to the annotation and fails with the exec error"
  new_stub
  wl "$TMP_DIR" STUB_CLOCK=1760000123.456789012 -- mark t1 fault-start
  assert_eq "mark exits 0" "0" "$WL_RC"
  assert_eq "the pod clock lands in chaos.c5c3.io/fault-start" "1760000123.456789012" "$(cat "$KSTUB/state/ann-fault-start")"
  assert_contains "the clock is read in the pod" "$(calls)" "exec chaos-wl-t1 -n openstack -- date +%s.%N"
  assert_contains "the annotation is overwritten" "$(calls)" \
    "annotate pod chaos-wl-t1 -n openstack chaos.c5c3.io/fault-start=1760000123.456789012 --overwrite"

  new_stub
  wl "$TMP_DIR" STUB_EXEC_FAIL="error: unable to upgrade connection: container not found" -- mark t1 fault-end
  assert_eq "a failed exec: exit 1" "1" "$WL_RC"
  assert_contains "it prints the error" "$WL_OUT" "error: unable to upgrade connection: container not found"
  assert_eq "nothing is annotated" "no" "$([ -f "$KSTUB/state/ann-fault-end" ] && echo yes || echo no)"

  wl "$TMP_DIR" -- mark t1 fault-middle
  assert_eq "an unknown mark: exit 2" "2" "$WL_RC"
}

# --- Test 17 --------------------------------------------------------------
test_verify_refuses_a_missing_double_or_malformed_budget_line() {
  echo "Test: verify refuses a suite without exactly one valid budget line"
  local dir
  new_stub
  dir="$(suite_dir no-budget '# A suite without a budget.')"
  wl "$dir" -- verify t1
  assert_eq "no budget line: exit 1" "1" "$WL_RC"
  assert_contains "it names the file" "$WL_OUT" "$dir/chainsaw-test.yaml has no '# Chaos budget"

  dir="$(suite_dir two-budgets $'# Chaos budget: failures=0 recovery=0s\n# Chaos budget: failures=2 recovery=60s')"
  wl "$dir" -- verify t1
  assert_eq "two budget lines: exit 1" "1" "$WL_RC"
  assert_contains "it names the file and the count" "$WL_OUT" "$dir/chainsaw-test.yaml has 2 '# Chaos budget:' lines"

  dir="$(suite_dir bad-budget '# Chaos budget: failures=0 recovery=abc')"
  wl "$dir" -- verify t1
  assert_eq "recovery=abc: exit 1" "1" "$WL_RC"
  assert_contains "it names the line and the file" "$WL_OUT" \
    "'# Chaos budget: failures=0 recovery=abc' in $dir/chainsaw-test.yaml does not read"
  assert_eq "the workload was left alone" "" "$(calls)"
}

# good_run — a workload log that passes evaluate without marks, given to the
# stub as what the pod prints after the stop file.
good_run() {
  printf 'READY token\n'
  rec 1001 token issue true 201
  rec 1031 token validate true
  printf 'DONE\n'
}

# --- Test 18 --------------------------------------------------------------
test_verify_maps_the_placement_audit_exit_codes() {
  echo "Test: verify maps the placement audit's exit codes 0, 3 and 1"
  local dir
  dir="$(suite_dir audit '# Chaos budget: failures=0 recovery=0s')"

  new_stub
  good_run >"$KSTUB/state/log.after"
  wl "$dir" STUB_AUDIT_RC=0 STUB_AUDIT_OUT="Processed 1 allocation." -- verify t1 --placement-audit nova-x
  assert_eq "exit 0 from the audit: verify passes" "0" "$WL_RC"
  assert_contains "it reports the audit" "$WL_OUT" "CONSISTENCY-OK placement-audit"
  assert_contains "evaluate ran" "$WL_OUT" "stream token: 2 ok"
  assert_contains "without marks it waited 30 s on the pod clock" "$WL_OUT" "observing until 1030"
  local c
  c="$(calls)"
  assert_contains "the audit runs in the API container" "$c" \
    "exec deploy/nova-x -n openstack -c nova-api -- nova-manage --config-dir /etc/nova/nova.conf.d placement audit --verbose"
  assert_contains "the stop file is created" "$c" "exec chaos-wl-t1 -n openstack -- touch /tmp/chaos-workload.stop"
  assert_contains "the pod is deleted" "$(tail -n1 <<<"$c")" "delete pod chaos-wl-t1 -n openstack --ignore-not-found --wait=false"

  new_stub
  good_run >"$KSTUB/state/log.after"
  wl "$dir" STUB_AUDIT_RC=3 STUB_AUDIT_OUT="Allocations were set on resource provider rp1 for consumer c1" -- verify t1 --placement-audit nova-x
  assert_eq "exit 3 from the audit: exit 1" "1" "$WL_RC"
  assert_contains "it is a consistency failure" "$WL_OUT" "CONSISTENCY-FAIL placement-audit"
  assert_contains "with the output" "$WL_OUT" "for consumer c1"

  new_stub
  good_run >"$KSTUB/state/log.after"
  wl "$dir" STUB_AUDIT_RC=1 STUB_AUDIT_OUT="An error has occurred" -- verify t1 --placement-audit nova-x
  assert_eq "exit 1 from the audit: exit 1" "1" "$WL_RC"
  assert_contains "it names the code" "$WL_OUT" "FAIL: placement-audit: nova-manage exited 1"
  assert_contains "with the output" "$WL_OUT" "An error has occurred"

  # With marks: the window reaches evaluate and verify waits past
  # fault_end + recovery + 30 = 1010 + 10 + 30 = 1050.
  dir="$(suite_dir marked '# Chaos budget: failures=1 recovery=10s')"
  new_stub
  printf '1000.5' >"$KSTUB/state/ann-fault-start"
  printf '1010.25' >"$KSTUB/state/ann-fault-end"
  {
    rec 990 token issue true 201
    rec 1005 token issue false 503
    rec 1055 token issue true 201
    printf 'DONE\n'
  } >"$KSTUB/state/log.after"
  wl "$dir" -- verify t1
  assert_eq "one failure in the window at a budget of 1: verify passes" "0" "$WL_RC"
  assert_contains "the marks reach evaluate" "$WL_OUT" "stream token: 2 ok, 1 failed in window (budget 1), 0 outside"
  assert_contains "it waits past fault_end + recovery + 30" "$WL_OUT" "observing until 1050"
  assert_eq "it read the pod clock until it passed 1050" "1060" "$(cat "$KSTUB/state/clock")"

  # A log that breaks the budget: the audit still runs and the pod still goes.
  dir="$(suite_dir audit '# Chaos budget: failures=0 recovery=0s')"
  new_stub
  {
    printf 'READY token\n'
    rec 1001 token issue true 201
    rec 1011 token issue false 503
    printf 'DONE\n'
  } >"$KSTUB/state/log.after"
  wl "$dir" STUB_AUDIT_RC=0 -- verify t1 --placement-audit nova-x
  assert_eq "a log evaluate fails: exit 1" "1" "$WL_RC"
  assert_contains "it names evaluate" "$WL_OUT" "FAIL: workload.py evaluate exited 1"
  assert_contains "the audit still runs" "$WL_OUT" "CONSISTENCY-OK placement-audit"
  assert_contains "the log is printed" "$WL_OUT" "=== workload pod chaos-wl-t1: last 200 log lines ==="
  assert_contains "the pod is still deleted" "$(calls)" "delete pod chaos-wl-t1 -n openstack --ignore-not-found --wait=false"
  assert_contains "the verdict comes last" "$(tail -n1 <<<"$WL_OUT")" "FAIL: verify t1"
}

# --- Test 19 --------------------------------------------------------------
test_verify_fails_on_a_stop_exec_error_or_a_missing_done() {
  echo "Test: verify fails with the log and the pod status on a failed stop and on a missing DONE"
  local dir
  dir="$(suite_dir stop '# Chaos budget: failures=0 recovery=0s')"
  new_stub
  printf 'READY token\n' >"$KSTUB/state/log"
  wl "$dir" STUB_TOUCH_FAIL="error: Internal error occurred: container is not running" -- verify t1
  assert_eq "a failed touch: exit 1" "1" "$WL_RC"
  assert_contains "it prints the exec error" "$WL_OUT" "creating the stop file failed: error: Internal error occurred"
  assert_contains "the log" "$WL_OUT" "READY token"
  assert_contains "and the pod status" "$WL_OUT" "Status:       Running"

  new_stub
  printf 'READY token\n' >"$KSTUB/state/log"
  rec 1001 token issue true 201 >"$KSTUB/state/log.after"
  wl "$dir" CHAOS_WORKLOAD_DONE_SECONDS=1 -- verify t1
  assert_eq "no DONE: exit 1" "1" "$WL_RC"
  assert_contains "it names the bound" "$WL_OUT" "no DONE within 1s of the stop file"
  assert_contains "the log" "$WL_OUT" '"op": "issue"'
  assert_contains "and the pod status" "$WL_OUT" "Status:       Running"

  new_stub
  printf 'READY token\n' >"$KSTUB/state/log"
  rec 1001 token issue true 201 >"$KSTUB/state/log.after"
  wl "$dir" STUB_PHASE=Failed -- verify t1
  assert_eq "a pod that ended without DONE: exit 1" "1" "$WL_RC"
  assert_contains "it names the phase" "$WL_OUT" "the workload ended Failed without DONE"

  new_stub
  wl "$dir" STUB_POD_MISSING=1 -- verify t1
  assert_eq "a missing pod: exit 1" "1" "$WL_RC"
  assert_contains "it says the pod is missing" "$WL_OUT" 'the workload pod is missing: Error from server (NotFound): pods "chaos-wl-t1" not found'
}

# --- Test 20 --------------------------------------------------------------
test_verify_reads_the_ovn_db_sync_log() {
  echo "Test: verify runs the OVN db-sync CronJob once and reads its log"
  local dir c
  dir="$(suite_dir ovn '# Chaos budget: failures=0 recovery=0s')"

  new_stub
  good_run >"$KSTUB/state/log.after"
  wl "$dir" STUB_CRONJOB=log STUB_JOB_STATE="SuccessCriteriaMet Complete" \
    STUB_JOB_LOG=$'Neutron OVN DB sync started\nNetwork found in OVN NB DB but not in Neutron, network_id=x\nNeutron OVN DB sync completed' \
    -- verify t1 --ovn-db-sync neutron-x
  assert_eq "a drift line: exit 1" "1" "$WL_RC"
  assert_contains "it is a consistency failure" "$WL_OUT" "CONSISTENCY-FAIL ovn-db-sync"
  assert_contains "with the drift line" "$WL_OUT" "Network found in OVN NB DB but not in Neutron, network_id=x"
  c="$(calls)"
  assert_contains "the Job is created from the CronJob" "$c" \
    "create job chaos-wl-t1-ovn-db-sync -n openstack --from=cronjob/neutron-x-ovn-db-sync"
  assert_contains "and deleted afterwards" "$c" "delete job chaos-wl-t1-ovn-db-sync -n openstack --ignore-not-found --wait=false"

  # The warnings of neutron 28.0.1 that report a difference without the words
  # "found in".
  local line
  while IFS= read -r line; do
    new_stub
    good_run >"$KSTUB/state/log.after"
    wl "$dir" STUB_CRONJOB=log STUB_JOB_STATE="SuccessCriteriaMet Complete" \
      STUB_JOB_LOG=$'Neutron OVN DB sync started\n'"${line}"$'\nNeutron OVN DB sync completed' \
      -- verify t1 --ovn-db-sync neutron-x
    assert_eq "'${line}': exit 1" "1" "$WL_RC"
    assert_contains "it is a consistency failure with the line" "$WL_OUT" $'CONSISTENCY-FAIL ovn-db-sync: Neutron and OVN disagree\n'"${line}"
  done <<'LINES'
Number of Port Groups to add: 1, remove: 0
ACLs to be added: 2 ACLs to be removed: 0
ACL should be present in OVN but is not, adding:{'port_group': 'pg_x'}
ACL should not be present in OVN, removing{'port_group': 'pg_x'}
DHCP options for subnet s1 present in Neutron but out of sync with OVN NB DB
Router 1 port forwarding for floating IPs ['f1'] Neutron out of sync or missing in OVN NB DB
Router Port port_id=p1 needs to be updated in OVN NB DB as network(s) have changed
Port QoS policy missing in OVN NB DB, port_id=p1
Floating IP QoS policy missing in OVN NB DB, fip_id=f1
The floating IP NAT rules must be updated to match the ``stateless_nat_enabled`` configuration flag.
LINES

  new_stub
  good_run >"$KSTUB/state/log.after"
  wl "$dir" STUB_CRONJOB=log STUB_JOB_STATE="SuccessCriteriaMet Complete" \
    STUB_JOB_LOG=$'Neutron OVN DB sync started\nHost h1 found both in OVN SB DB and Neutron. Trigger updating its SegmentHostMapping in Neutron, to keep OVN SB DB and Neutron data consistent\nNeutron OVN DB sync completed' \
    -- verify t1 --ovn-db-sync neutron-x
  assert_eq "a clean log: exit 0" "0" "$WL_RC"
  assert_contains "it reports the comparison" "$WL_OUT" "CONSISTENCY-OK ovn-db-sync"
  assert_contains "and shows that the sync ran" "$WL_OUT" "Neutron OVN DB sync completed"

  # The Southbound pass says "found in" too, but compares no Northbound row.
  # Its "New host" line comes on every run for a chassis without bridge
  # mappings (the nova leg of CI run 38800821802).
  while IFS= read -r line; do
    new_stub
    good_run >"$KSTUB/state/log.after"
    wl "$dir" STUB_CRONJOB=log STUB_JOB_STATE="SuccessCriteriaMet Complete" \
      STUB_JOB_LOG=$'Neutron OVN DB sync started\n'"${line}"$'\nNeutron OVN DB sync completed' \
      -- verify t1 --ovn-db-sync neutron-x
    assert_eq "'${line}': exit 0" "0" "$WL_RC"
    assert_contains "it reports the comparison" "$WL_OUT" "CONSISTENCY-OK ovn-db-sync"
  done <<'LINES'
New host h1 found in OVN SB DB, but not in Neutron. Add its SegmentHostMapping in Neutron
Stale host h1 found in Neutron, but not in OVN SB DB. Clear its SegmentHostMapping in Neutron
LINES

  new_stub
  good_run >"$KSTUB/state/log.after"
  wl "$dir" STUB_CRONJOB=log STUB_JOB_STATE="SuccessCriteriaMet Complete" \
    STUB_JOB_LOG=$'Neutron OVN DB sync started\nNew host h1 found in OVN SB DB, but not in Neutron. Add its SegmentHostMapping in Neutron\nPort found in Neutron but not in OVN NB DB, port_id=p1\nNeutron OVN DB sync completed' \
    -- verify t1 --ovn-db-sync neutron-x
  assert_eq "a Northbound line beside a Southbound one: exit 1" "1" "$WL_RC"
  assert_contains "it fails on the Northbound line" "$WL_OUT" \
    $'CONSISTENCY-FAIL ovn-db-sync: Neutron and OVN disagree\nPort found in Neutron but not in OVN NB DB, port_id=p1'
  assert_not_contains "and leaves the Southbound line out" "$WL_OUT" "New host h1"

  # The Job would pass, so only the mode check can fail the run.
  new_stub
  good_run >"$KSTUB/state/log.after"
  wl "$dir" STUB_CRONJOB=repair STUB_JOB_STATE="SuccessCriteriaMet Complete" \
    STUB_JOB_LOG=$'Neutron OVN DB sync started\nNeutron OVN DB sync completed' \
    -- verify t1 --ovn-db-sync neutron-x
  assert_eq "a CronJob in repair mode: exit 1" "1" "$WL_RC"
  assert_contains "it asks for log mode" "$WL_OUT" \
    "cronjob neutron-x-ovn-db-sync does not run in log mode: set spec.ovnDBSync.syncMode: log on the Neutron CR"
  assert_not_contains "no Job is created" "$(calls)" "create job"

  new_stub
  good_run >"$KSTUB/state/log.after"
  wl "$dir" STUB_CRONJOB=log STUB_JOB_STATE="" CHAOS_WORKLOAD_DB_SYNC_SECONDS=1 -- verify t1 --ovn-db-sync neutron-x
  assert_eq "a Job that never finishes: exit 1" "1" "$WL_RC"
  assert_contains "it names the bound" "$WL_OUT" "FAIL: ovn-db-sync: job chaos-wl-t1-ovn-db-sync did not complete within 1s"
  assert_contains "the Job is deleted" "$(calls)" "delete job chaos-wl-t1-ovn-db-sync -n openstack --ignore-not-found --wait=false"

  new_stub
  good_run >"$KSTUB/state/log.after"
  wl "$dir" STUB_CRONJOB=log STUB_JOB_STATE="SuccessCriteriaMet Complete" STUB_JOB_LOG="" \
    -- verify t1 --ovn-db-sync neutron-x
  assert_eq "a completed Job whose log never reports completion: exit 1" "1" "$WL_RC"
  assert_contains "it names the missing line" "$WL_OUT" "has no 'Neutron OVN DB sync completed' line"

  new_stub
  good_run >"$KSTUB/state/log.after"
  wl "$dir" STUB_CRONJOB=log STUB_JOB_STATE="FailureTarget Failed" STUB_JOB_LOG="Traceback: cannot connect to OVN NB" \
    -- verify t1 --ovn-db-sync neutron-x
  assert_eq "a failed Job: exit 1" "1" "$WL_RC"
  assert_contains "it says the Job failed" "$WL_OUT" "FAIL: ovn-db-sync: job chaos-wl-t1-ovn-db-sync failed"
  assert_contains "with its log" "$WL_OUT" "Traceback: cannot connect to OVN NB"
  assert_contains "the failed Job is deleted too" "$(calls)" "delete job chaos-wl-t1-ovn-db-sync -n openstack --ignore-not-found --wait=false"

  new_stub
  good_run >"$KSTUB/state/log.after"
  wl "$dir" -- verify t1 --ovn-db-sync neutron-x
  assert_eq "a missing CronJob: exit 1" "1" "$WL_RC"
  assert_contains "it says how to get one" "$WL_OUT" \
    "cronjob neutron-x-ovn-db-sync not found: set spec.ovnDBSync {syncMode: log, suspend: true} on the Neutron CR"
  assert_not_contains "no Job is created" "$(calls)" "create job"
}

# --- Test 21 --------------------------------------------------------------
test_dump_and_stop_exit_0_without_a_pod() {
  echo "Test: dump and stop exit 0 when the pod is gone"
  new_stub
  wl "$TMP_DIR" STUB_POD_MISSING=1 -- dump t1
  assert_eq "dump exits 0" "0" "$WL_RC"
  assert_contains "it says what it dumps" "$WL_OUT" "=== chaos workload chaos-wl-t1 ==="
  wl "$TMP_DIR" STUB_POD_MISSING=1 -- stop t1
  assert_eq "stop exits 0" "0" "$WL_RC"
  assert_contains "stop deletes without waiting" "$(calls)" "delete pod chaos-wl-t1 -n openstack --ignore-not-found --wait=false"

  new_stub
  printf '1000.5' >"$KSTUB/state/ann-fault-start"
  printf 'READY token\n' >"$KSTUB/state/log"
  wl "$TMP_DIR" -- dump t1
  assert_contains "dump prints the phase and the marks of a live pod" "$WL_OUT" "phase=Running fault-start=1000.5 fault-end="
  assert_contains "and its log" "$WL_OUT" "READY token"
}

# ---------------------------------------------------------------------------
# Adoption
# ---------------------------------------------------------------------------

CHAOS_DIR="$PROJECT_ROOT/tests/e2e-chaos"
ADOPTERS="api-pod-kill-pdb barbican-keystone-outage barbican-mariadb-outage \
barbican-operator-pod-kill cinder-broker-pod-kill cinder-keystone-outage \
cinder-mariadb-outage cinder-operator-pod-kill cinder-volume-pod-kill \
cronjob-rotation-failure glance-keystone-outage \
glance-mariadb-outage glance-operator-pod-kill horizon-dashboard-pod-kill \
horizon-memcached-pod-kill horizon-operator-pod-kill mariadb-network-latency \
mariadb-network-partition mariadb-pod-kill memcached-network-partition \
memcached-pod-kill neutron-keystone-outage neutron-metadata-agent-pod-kill \
neutron-operator-pod-kill nova-keystone-outage nova-operator-pod-kill \
nova-placement-outage openbao-pod-kill operator-pod-crash operator-pod-kill \
ovn-operator-pod-kill placement-keystone-outage placement-mariadb-outage \
placement-operator-pod-kill"
BUDGET_RE='^# Chaos budget: failures=([0-9]+) recovery=([0-9]+)s$'
MEASURED_RE='^# Chaos budget measured: [0-9]{4}-[0-9]{2}-[0-9]{2}, [a-z]+, failures [0-9]+(/[0-9]+){0,2}, recovery [0-9]+(/[0-9]+){0,2} s$'

have_yq() {
  command -v yq >/dev/null 2>&1
}

# check_adopter <suite> <file> — the contract of a suite that calls start.
check_adopter() {
  local suite="$1" file="$2" budget measured ids
  assert_eq "$suite: exactly one budget line" "1" "$(grep -c '^# Chaos budget:' "$file")"
  budget="$(grep -m1 '^# Chaos budget:' "$file")"
  assert_eq "$suite: the budget line reads failures=<N> recovery=<S>s" "yes" \
    "$([[ "$budget" =~ $BUDGET_RE ]] && echo yes || echo "no: $budget")"
  measured="$(grep -A1 '^# Chaos budget:' "$file" | sed -n 2p)"
  assert_eq "$suite: the measured line follows it" "yes" \
    "$([[ "$measured" =~ $MEASURED_RE ]] && echo yes || echo "no: $measured")"
  ids="$(grep -oE 'workload\.sh (start|mark|verify|dump|stop) [^ ]+' "$file" | awk '{print $3}' | sort -u)"
  assert_eq "$suite: every workload.sh call uses the directory name as its id" "$suite" "$ids"
  assert_eq "$suite: one verify" "1" "$(grep -c "workload\.sh verify $suite" "$file")"
  assert_eq "$suite: fault-start and fault-end come in a pair" \
    "$(grep -c "workload\.sh mark $suite fault-start" "$file")" \
    "$(grep -c "workload\.sh mark $suite fault-end" "$file")"
  if ! have_yq; then
    # The two yq checks below.
    SKIP=$((SKIP + 2))
    return 0
  fi
  assert_eq "$suite: a stop in a cleanup block" "1" "$(yq 'explode(.) | [.spec.steps[].cleanup[]? | select(has("script"))
    | select(.script.content == "../lib/workload.sh stop '"$suite"'")] | length' "$file")"
  assert_eq "$suite: a dump in a catch block" "true" "$(yq 'explode(.) | [.spec.steps[].catch[]? | select(has("script"))
    | select(.script.content == "../lib/workload.sh dump '"$suite"'")] | length > 0' "$file")"
}

# --- Test 22 --------------------------------------------------------------
test_every_suite_adopts_the_harness_or_declines_it() {
  echo "Test: the thirty-four adopters keep the harness contract and deletion-stuck-finalizer declines it"
  local file suite found=""
  for file in "$CHAOS_DIR"/*/chainsaw-test.yaml; do
    suite="$(basename "$(dirname "$file")")"
    if grep -q 'lib/workload\.sh start' "$file"; then
      found="$found $suite"
      check_adopter "$suite" "$file"
    fi
  done
  # shellcheck disable=SC2086 # word splitting builds the sorted list
  assert_eq "exactly the thirty-four suites call workload.sh start" "$(echo $ADOPTERS)" \
    "$(printf '%s\n' $found | sort | xargs)"
  local finalizer="$CHAOS_DIR/deletion-stuck-finalizer/chainsaw-test.yaml"
  assert_file_contains_fixed "deletion-stuck-finalizer says why it has no workload" "$finalizer" \
    "# Chaos workload: none (the Keystone under test is being deleted)"
  assert_eq "lib/ holds no chainsaw-test.yaml, so it is no suite" "no" \
    "$([ -e "$LIB_DIR/chainsaw-test.yaml" ] && echo yes || echo no)"

  if ! have_yq; then
    echo "  SKIP: yq not installed (two checks per adopter and the untimed-call check skipped)"
    SKIP=$((SKIP + 1))
    return 0
  fi
  local untimed
  untimed="$(for file in "$CHAOS_DIR"/*/chainsaw-test.yaml; do
    yq 'explode(.) | .spec.steps[] | (.try, .catch, .cleanup, .finally) | .[]? | select(has("script"))
      | .script | select(.content | test("workload\\.sh")) | select(has("timeout") | not) | .content' "$file" |
      sed "s|^|$(basename "$(dirname "$file")"): |"
  done)"
  assert_eq "every script that calls workload.sh sets timeout:" "" "$untimed"
}

# ---------------------------------------------------------------------------
# Wiring
# ---------------------------------------------------------------------------

# --- Test 23 --------------------------------------------------------------
test_ci_loads_the_tempest_image_on_the_keystone_stack_legs() {
  echo "Test: every non-ovn e2e-chaos leg loads the Tempest image, and make shellcheck lints the harness"
  local stack kind_load
  stack="$(job_step e2e-chaos "Load E2E images (keystone stack)")"
  kind_load="$(job_step e2e-chaos "Load images into kind")"
  assert_contains "the keystone-stack pull runs on every leg but ovn" "$stack" "if: matrix.suite != 'ovn'"
  assert_contains "and pulls the Tempest image" "$stack" "\${{ env.IMAGE_PREFIX }}/tempest:2026.1"
  assert_contains "the keystone-stack kind load runs on every leg but ovn" "$kind_load" "if: matrix.suite != 'ovn'"
  assert_contains "and hands the Tempest image to the node" "$kind_load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/tempest:2026.1 --name"
  assert_not_contains "the nova-only pull no longer names it" \
    "$(job_step e2e-chaos "Load E2E images")" "tempest:2026.1"
  assert_not_contains "nor does the nova-only kind load" \
    "$(job_step e2e-chaos "Load nova leg images into kind")" "tempest:2026.1"

  local target
  target="$(awk '/^shellcheck:/ { on = 1; next } on && /^[^\t]/ { exit } on { print }' "$PROJECT_ROOT/Makefile")"
  assert_contains "make shellcheck lints tests/e2e-chaos/lib/*.sh" "$target" \
    "shellcheck --severity=warning tests/e2e-chaos/lib/*.sh"
  assert_contains "make shellcheck lints the probe libraries the suites source" "$target" \
    "tests/e2e-chaos/*/probe-lib.sh"
  if command -v shellcheck >/dev/null 2>&1; then
    local out rc=0
    out="$(cd "$PROJECT_ROOT" && shellcheck --severity=warning tests/e2e-chaos/lib/workload.sh 2>&1)" || rc=$?
    assert_eq "workload.sh passes shellcheck${out:+: $out}" "0" "$rc"
  else
    echo "  SKIP: shellcheck not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
  fi
}

# --- Test 24 --------------------------------------------------------------
test_ci_runs_placement_on_the_pod_and_network_legs() {
  echo "Test: the pod and network e2e-chaos legs load and deploy Placement, the nova leg keeps its own steps"
  # The placement stream needs a Placement on every leg that runs a placement
  # suite: placement-operator-pod-kill and placement-keystone-outage on pod,
  # placement-mariadb-outage on network.
  local expr="(matrix.suite == 'pod' || matrix.suite == 'network')"
  local load
  load="$(job_step e2e-chaos "Load E2E images")"
  assert_contains "the pull of the placement-operator image covers both legs" "$load" \
    "\${{ ${expr} && format('{0}/placement-operator:dev', env.IMAGE_PREFIX) || '' }}"
  assert_contains "the pull of the placement service image covers both legs" "$load" \
    "\${{ ${expr} && format('{0}/placement:2026.1', env.IMAGE_PREFIX) || '' }}"
  local name step
  for name in "Load placement images into kind" "Deploy placement operator"; do
    step="$(job_step e2e-chaos "$name")"
    assert_eq "${name}: runs on the pod and network legs alone" \
      "if: matrix.suite == 'pod' || matrix.suite == 'network'" \
      "$(grep -E '^ *if:' <<<"$step" | sed 's/^ *//')"
  done
  assert_eq "the nova leg's placement deploy keeps its own condition" \
    "if: matrix.suite == 'nova'" \
    "$(job_step e2e-chaos "Deploy placement operator (nova leg)" | grep -E '^ *if:' | sed 's/^ *//')"
  assert_contains "the network leg runs the placement database outage" \
    "$(e2e_chaos_matrix_entry network)" "tests/e2e-chaos/placement-mariadb-outage"
}

# --- Run ---
test_run_reaches_ready_on_every_stream
test_a_503_fails_one_stream_and_leaves_the_others_ok
test_a_503_on_create_fails_placement_and_secret_while_token_keeps_going
test_resolve_sends_the_request_to_the_mapped_address
test_insecure_tls_is_needed_against_a_self_signed_stub
test_a_closed_port_records_a_null_code_and_the_urlerror
test_a_300_is_an_answer_to_the_http_stream
test_a_failed_cycle_deletes_what_it_created
test_a_failed_cycle_deletes_the_newest_resource_first
test_a_payload_that_differs_fails_read_payload
test_a_closed_placement_port_records_a_null_code
test_a_provider_listing_that_differs_fails_list_rp
test_a_create_without_a_location_fails_create_rp
test_a_401_reissues_the_token_once_and_retries
test_a_401_to_the_provider_listing_reissues_the_token
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
test_start_runs_the_pod_as_specified
test_start_fails_on_a_terminal_pod_or_a_missing_ready
test_start_rejects_an_invalid_id
test_mark_writes_the_pod_clock_and_fails_with_the_exec_error
test_verify_refuses_a_missing_double_or_malformed_budget_line
test_verify_maps_the_placement_audit_exit_codes
test_verify_fails_on_a_stop_exec_error_or_a_missing_done
test_verify_reads_the_ovn_db_sync_log
test_dump_and_stop_exit_0_without_a_pod
test_every_suite_adopts_the_harness_or_declines_it
test_ci_loads_the_tempest_image_on_the_keystone_stack_legs
test_ci_runs_placement_on_the_pod_and_network_legs

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
