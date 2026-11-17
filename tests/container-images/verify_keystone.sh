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
  assert_uwsgi_graceful_shutdown
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
  --hook-master-start "unix_signal:15 gracefully_kill_them_all" --shutdown-sockets --threaded-logger \
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

# --- Test 8: a login write that lost to a concurrent login is retried ---
test_login_write_retry_patch_applied() {
  echo "Test: a login write whose transaction raises DBDeadlock is retried"
  # Proves the image build applied
  # patches/keystone/<release>/0004-retry-login-writes-that-conflict-with-a-concurrent-login.patch
  # (#1739). Five methods on the login path read a row and write it in a
  # transaction of their own: set_last_active_at, add_user_to_group_expires
  # and cleanup_stale_group_memberships of the shadow users backend, and
  # _record_failed_auth and _reset_failed_auth of the identity backend. On
  # MariaDB 12.3.3 the second of two concurrent logins of one user fails
  # there with error 1020, which oslo.db raises as DBConsistencyError, a
  # subclass of DBDeadlock. Each method is called once with a first write
  # transaction that raises DBDeadlock: the patched image opens a second one,
  # an unpatched image lets the error escape. The sessions are mocked and the
  # retry's wait is patched, so neither a database nor a configuration file is
  # needed. Stderr is echoed on failure so the reason is named.
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import sys
from unittest import mock

from oslo_db import exception as db_exc

from keystone.common import sql
from keystone.identity.backends import sql as identity_sql
from keystone.identity.shadow_backends import sql as shadow_sql

shadow = object.__new__(shadow_sql.ShadowUsers)
identity = object.__new__(identity_sql.Identity)
CALLS = (
    ("set_last_active_at", lambda: shadow.set_last_active_at("u")),
    ("add_user_to_group_expires",
     lambda: shadow.add_user_to_group_expires("u", "g")),
    ("cleanup_stale_group_memberships",
     lambda: shadow.cleanup_stale_group_memberships("u", "i", [])),
    ("_record_failed_auth", lambda: identity._record_failed_auth("u")),
    ("_reset_failed_auth", lambda: identity._reset_failed_auth("u")),
)
for name, call in CALLS:
    attempts = []

    def session_for_write():
        attempts.append(None)
        if len(attempts) == 1:
            raise db_exc.DBDeadlock()
        return mock.MagicMock()

    with mock.patch.object(sql, "session_for_write", session_for_write), \
            mock.patch.object(sql, "session_for_read"), \
            mock.patch("time.sleep"):
        try:
            call()
        except db_exc.DBDeadlock:
            sys.exit("%s lets a DBDeadlock escape: the write is not retried"
                     % name)
    if len(attempts) != 2:
        sys.exit("%s opened %d write transactions, want 2"
                 % (name, len(attempts)))' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "every login write is retried after a DBDeadlock" "0" "$exit_code"
}

# --- Test 9: a key repository is judged by its key files ---
test_key_repository_mode_patch_applied() {
  echo "Test: a world-readable key repository warns only for a world-readable key"
  # Proves the image build applied
  # patches/keystone/<release>/0005-judge-a-world-readable-key-repository-by-its-key-files.patch
  # (#1786). Upstream FernetUtils.validate_key_repository warns "key_repository
  # is world readable" whenever others may read or enter the directory, and
  # the kubelet creates every volume mount point with such a mode. The patched
  # image warns only when a key file in it is readable by others. The program
  # builds a 0755 repository in the kubelet's Secret volume layout (0 ->
  # ..data/0, ..data -> a 0755 directory) with a 0400 key, which must stay
  # silent, and then makes the key 0444, which must warn. An unpatched image
  # warns on the first call already. Stderr carries the logged warning and is
  # echoed on failure.
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import logging
import os
import sys
import tempfile

from keystone.common import fernet_utils

WARNING = "key_repository is world readable"


class Collect(logging.Handler):
    def __init__(self):
        super().__init__()
        self.messages = []

    def emit(self, record):
        self.messages.append(record.getMessage())


handler = Collect()
fernet_utils.LOG.logger.addHandler(handler)
fernet_utils.LOG.logger.setLevel(logging.WARNING)

directory = tempfile.mkdtemp()
os.chmod(directory, 0o755)
data_dir = os.path.join(directory, "..2026_11_17_00_00_00.1")
os.mkdir(data_dir)
os.chmod(data_dir, 0o755)
key = os.path.join(data_dir, "0")
with open(key, "w") as key_file:
    key_file.write("key")
os.chmod(key, 0o400)
os.symlink(os.path.basename(data_dir), os.path.join(directory, "..data"))
os.symlink(os.path.join("..data", "0"), os.path.join(directory, "0"))
utils = fernet_utils.FernetUtils(directory, 3, "fernet_tokens")
if not utils.validate_key_repository():
    sys.exit("validate_key_repository rejected %s" % directory)
for message in handler.messages:
    if WARNING in message:
        sys.exit("a 0400 key in a 0755 repository is reported: %s" % message)
os.chmod(key, 0o444)
utils.validate_key_repository()
if not any(WARNING in message for message in handler.messages):
    sys.exit("a 0444 key in a 0755 repository is not reported")' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "only a key file readable by others is reported" "0" "$exit_code"
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
test_login_write_retry_patch_applied
echo ""
test_key_repository_mode_patch_applied
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
