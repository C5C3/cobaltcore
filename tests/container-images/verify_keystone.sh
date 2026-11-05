#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify keystone container image meets requirements
# Usage: bash tests/container-images/verify_keystone.sh [image_name]
# Default image: c5c3/keystone:29.0.0
# Requires: Docker daemon running

set -euo pipefail

IMAGE="${1:-c5c3/keystone:29.0.0}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"
# shellcheck source=tests/lib/build_tools.sh
source "$SCRIPT_DIR/../lib/build_tools.sh"
# shellcheck source=tests/lib/pymemcache.sh
source "$SCRIPT_DIR/../lib/pymemcache.sh"
# shellcheck source=tests/lib/uwsgi.sh
source "$SCRIPT_DIR/../lib/uwsgi.sh"

# --- Test 1: keystone-manage --version outputs version and exits 0 ---
test_keystone_manage_version() {
  echo "Test: keystone-manage --version succeeds"
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" keystone-manage --version 2>&1) || exit_code=$?

  assert_eq "keystone-manage --version exits 0" "0" "$exit_code"

  assert_not_empty "version output is non-empty" "$version"
}

# --- Test 2: runs as openstack user ---
test_runs_as_openstack_user() {
  echo "Test: container runs as openstack user"
  local whoami_output exit_code=0
  whoami_output=$(docker run --rm "$IMAGE" whoami 2>&1) || exit_code=$?

  assert_eq "whoami exits 0" "0" "$exit_code"
  assert_eq "whoami outputs openstack" "openstack" "$whoami_output"
}

# --- Test 3: no build tools in final image ---
test_no_build_tools_in_final_image() {
  echo "Test: no build tools in final image"
  assert_no_build_tools
}

# --- Test 4: the runtime libraries and entry points work ---
test_runtime_libraries_present() {
  echo "Test: the runtime libraries and entry points work"
  # python-ldap links libldap and libsasl, which its closure carries; the
  # ldap extra of releases/<release>/extra-packages.yaml installs it. uWSGI
  # loads keystone-wsgi-public (operators/keystone, --wsgi-file). Stderr is
  # captured, so a missing library is named on failure.
  local output exit_code=0
  output=$(docker run --rm "$IMAGE" python -c 'import ldap' 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $output"
  assert_eq "import ldap exits 0" "0" "$exit_code"

  exit_code=0
  docker run --rm "$IMAGE" sh -c 'test -x /var/lib/openstack/bin/keystone-wsgi-public' || exit_code=$?
  assert_eq "keystone-wsgi-public is executable" "0" "$exit_code"

  exit_code=0
  output=$(docker run --rm "$IMAGE" uwsgi --version 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $output"
  assert_eq "uwsgi --version exits 0" "0" "$exit_code"
  assert_uwsgi_imports keystone
}

# --- Test 5: the implied-role patch is applied ---
test_implied_role_patch_applied() {
  echo "Test: implied roles expand in linear time and each role is looked up once"
  # Proves the image build applied
  # patches/keystone/<release>/0001-expand-implied-roles-in-linear-time-and-look-up-each-role-once.patch
  # (#1459), which the lock lists and nix/lib/python-env.nix applies.
  # Upstream's add_implied_roles checks every implied ref against a list of
  # the refs it has already checked, one dict comparison per entry, which
  # grows with the square of the result; _strip_domain_roles calls get_role
  # once per ref. The check counts operations instead of timing them, so a
  # slow runner cannot flip it: 4,000 refs whose member role implies reader
  # expand to 8,000 with fewer ref comparisons than refs (upstream makes
  # millions), and stripping the domain-specific reader takes one get_role
  # call per role, two for 8,000 refs, and no uncached list_roles_from_ids
  # call. The role API is mocked, so neither a database nor a configuration
  # file is needed. Stderr is echoed on failure so the reason is named rather
  # than collapsed into a bare exit code.
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import sys
from unittest import mock
from keystone.assignment import core

ROLES = {
    "member": {"id": "member", "domain_id": None},
    "reader": {"id": "reader", "domain_id": "d"},
}
COMPARISONS = [0]


class CountingRef(dict):
    """A ref that counts how often it is compared with ==."""

    def __eq__(self, other):
        COMPARISONS[0] += 1
        return dict.__eq__(self, other)

    __hash__ = None


def implied_roles(role_id):
    if role_id == "member":
        return [{"prior_role_id": "member", "implied_role_id": "reader"}]
    return []


manager = object.__new__(core.Manager)
with mock.patch.object(core, "PROVIDERS") as providers:
    role_api = providers.role_api
    role_api.list_implied_roles.side_effect = implied_roles
    role_api.get_role.side_effect = lambda role_id: ROLES[role_id]

    refs = [CountingRef(user_id="u", project_id="p%d" % i, role_id="member")
            for i in range(4000)]
    expanded = manager.add_implied_roles(refs)
    if len(expanded) != 8000:
        sys.exit("add_implied_roles returned %d refs, expected 8000"
                 % len(expanded))
    if COMPARISONS[0] >= len(refs):
        sys.exit("add_implied_roles compared refs %d times for %d refs: the "
                 "cycle check still scans a list" % (COMPARISONS[0], len(refs)))
    kept = manager._strip_domain_roles(expanded)
    if len(kept) != 4000:
        sys.exit("_strip_domain_roles kept %d refs, expected 4000" % len(kept))
    lookups = sorted(c.args[0] for c in role_api.get_role.call_args_list)
    if lookups != ["member", "reader"]:
        sys.exit("_strip_domain_roles called get_role %d times for 8000 refs, "
                 "expected once each for member and reader" % len(lookups))
    if role_api.list_roles_from_ids.call_count:
        sys.exit("_strip_domain_roles calls list_roles_from_ids, which no "
                 "cache answers")' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "implied roles expand in linear time and each role is looked up once" "0" "$exit_code"
}

# --- Test 6: [cache] socket_timeout bounds the pymemcache client ---
test_pymemcache_backend_bounded() {
  echo "Test: [cache] socket_timeout bounds the pymemcache client and errors are misses"
  # Keystone reads its [cache] section through this client.
  assert_pymemcache_backend_bounded
}

# --- Test 7: a lost database answers 503 instead of no response ---
test_lost_database_patch_applied() {
  echo "Test: a token request answers 503 at once while the database is unreachable"
  # Proves the image build applied
  # patches/keystone/<release>/0003-answer-unexpected-errors-instead-of-dropping-the-connection.patch
  # (#1610). Upstream lets the DBConnectionError of POST /v3/auth/tokens
  # escape to uWSGI, which writes no response, and the --http-keepalive of
  # the keystone-operator's command then holds an HTTP/1.1 client until the
  # router's 60-second socket timeout. The test runs the operator's default
  # uWSGI command (reconcile_deployment_pin_test.go) against a database
  # address that refuses connections. [database] max_retries = 0 makes the
  # first request fail at once, as every request of a running Keystone does
  # once its database goes away; with the operator's -1, oslo.db retries the
  # first connect of a new engine instead. The fernet and credential keys
  # let the worker load. Against an unpatched image the client's 10-second
  # timeout expires and the output names TimeoutError.
  local script output exit_code=0
  script=$(
    cat <<'EOF'
mkdir -p /etc/keystone/keystone.conf.d
cat > /etc/keystone/keystone.conf.d/keystone.conf <<'CONF'
[database]
connection = mysql+pymysql://keystone:unused@127.0.0.1:9/keystone
max_retries = 0

[cache]
enabled = false
CONF
for setup in fernet_setup credential_setup; do
  keystone-manage --config-dir /etc/keystone/keystone.conf.d/ "$setup" \
    --keystone-user "$(id -u)" --keystone-group "$(id -g)" >> /tmp/manage.log 2>&1 ||
    { cat /tmp/manage.log; exit 1; }
done
uwsgi --http :5000 --http-keepalive --log-master \
  --log-format '%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto) %(status))' \
  --wsgi-file /var/lib/openstack/bin/keystone-wsgi-public --master --lazy-apps --need-app \
  --processes 2 --threads 1 --pyargv=--config-dir=/etc/keystone/keystone.conf.d/ \
  > /tmp/uwsgi.log 2>&1 &
/var/lib/openstack/bin/python - <<'PY' || { tail -n 20 /tmp/uwsgi.log; exit 1; }
import http.client
import json
import sys
import time
import uuid

deadline = time.monotonic() + 60
while True:
    conn = http.client.HTTPConnection("127.0.0.1", 5000, timeout=5)
    try:
        conn.request("GET", "/v3")
        if conn.getresponse().status == 200:
            break
    except (OSError, http.client.HTTPException):
        pass
    finally:
        conn.close()
    if time.monotonic() > deadline:
        sys.exit("GET /v3 did not answer 200 within 60 s")
    time.sleep(1)

auth = {"auth": {
    "identity": {"methods": ["password"], "password": {"user": {
        "name": "admin", "password": "unused", "domain": {"id": "default"}}}},
    "scope": {"project": {"name": "admin", "domain": {"id": "default"}}},
}}
# HTTP/1.1 without "Connection: close": the keep-alive connection that
# upstream leaves without an answer for 60 seconds.
conn = http.client.HTTPConnection("127.0.0.1", 5000, timeout=10)
try:
    conn.request("POST", "/v3/auth/tokens", json.dumps(auth),
                 {"Content-Type": "application/json"})
    resp = conn.getresponse()
    body = resp.read().decode()
except (OSError, http.client.HTTPException) as exc:
    sys.exit("POST /v3/auth/tokens: %s: %s" % (type(exc).__name__, exc))
try:
    code = json.loads(body)["error"]["code"]
except (ValueError, KeyError, TypeError):
    code = None
if (resp.status != 503 or code != 503 or "127.0.0.1" in body
        or "DBConnectionError" in body):
    sys.exit("POST /v3/auth/tokens answered %d: %s" % (resp.status, body[:200]))

# werkzeug's own 404 keeps its answer, on the same connection.
try:
    conn.request("GET", "/v3/%s" % uuid.uuid4().hex)
    resp = conn.getresponse()
    resp.read()
except (OSError, http.client.HTTPException) as exc:
    sys.exit("GET /v3/<unrouted>: %s: %s" % (type(exc).__name__, exc))
if resp.status != 404:
    sys.exit("GET /v3/<unrouted> answered %d, expected 404" % resp.status)
PY
# The master logs a request after the response is sent.
answered=no
for _ in 1 2 3 4 5 6 7 8 9 10; do
  if grep -Eq 'POST /v3/auth/tokens => generated [1-9][0-9]* bytes in [0-9]+ msecs \(HTTP/1\.1 503\)' \
    /tmp/uwsgi.log; then
    answered=yes
    break
  fi
  sleep 1
done
if [ "$answered" != yes ]; then
  echo "uWSGI logged no answered 503 for POST /v3/auth/tokens"
  tail -n 20 /tmp/uwsgi.log
  exit 1
fi
if grep 'Unhandled error' /tmp/uwsgi.log; then
  exit 1
fi
EOF
  )
  output=$(docker run --rm --tmpfs /etc/keystone:uid=42424,gid=42424,mode=0755 \
    "$IMAGE" sh -c "$script" 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $output"

  assert_eq "a lost database answers 503 within the client timeout" "0" "$exit_code"
}

# --- Run all tests ---
echo "=== keystone container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_keystone_manage_version
echo ""
test_runs_as_openstack_user
echo ""
test_no_build_tools_in_final_image
echo ""
test_runtime_libraries_present
echo ""
test_implied_role_patch_applied
echo ""
test_pymemcache_backend_bounded
echo ""
test_lost_database_patch_applied
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
