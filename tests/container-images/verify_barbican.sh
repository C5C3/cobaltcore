#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify barbican container image meets requirements
# Usage: bash tests/container-images/verify_barbican.sh [image_name]
# Default image: c5c3/barbican:22.0.0
# Requires: Docker daemon running

set -euo pipefail

IMAGE="${1:-c5c3/barbican:22.0.0}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"
# shellcheck source=tests/lib/build_tools.sh
source "$SCRIPT_DIR/../lib/build_tools.sh"
# shellcheck source=tests/lib/uwsgi.sh
source "$SCRIPT_DIR/../lib/uwsgi.sh"

# --- Test 1: barbican-manage --version outputs version and exits 0 ---
test_barbican_manage_version() {
  echo "Test: barbican-manage --version succeeds"
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" barbican-manage --version 2>&1) || exit_code=$?

  assert_eq "barbican-manage --version exits 0" "0" "$exit_code"
  assert_not_empty "version output is non-empty" "$version"
}

# --- Test 2: barbican-status console script runs ---
test_barbican_status_help() {
  echo "Test: barbican-status --help succeeds"
  # Plain --help needs no config and no database, so it proves a second
  # console script uv generated is runnable.
  local help_output exit_code=0
  help_output=$(docker run --rm "$IMAGE" barbican-status --help 2>&1) || exit_code=$?

  assert_eq "barbican-status --help exits 0" "0" "$exit_code"
  assert_not_empty "help output is non-empty" "$help_output"
}

# --- Test 3: barbican is importable ---
test_barbican_importable() {
  echo "Test: barbican imports cleanly"
  local exit_code=0
  docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c "import barbican" > /dev/null 2>&1 || exit_code=$?

  assert_eq "import barbican exits 0" "0" "$exit_code"
}

# --- Test 4: the OpenBao secret-store backend loads as a plugin ---
test_vault_backend_loadable() {
  echo "Test: vault secret store entry point and castellan vault driver load"
  # This is the OpenBao backend contract. Neither half is reached by module
  # path at runtime, so neither is asserted by one here: barbican resolves
  # [secretstore] enabled_secretstore_plugins = vault_plugin through the
  # stevedore entry point in the barbican.secretstore.plugin namespace, and
  # castellan_secret_store resolves [key_manager] backend = vault through
  # key_manager.API(), a DriverManager lookup in the castellan.drivers
  # namespace. Entry-point metadata is the packaging artefact this image is
  # known to lose pieces of (uv's --prefix mode already drops the PBR
  # wsgi_scripts entry barbican-wsgi-api), and importing either module by path
  # would go green off the .py file on disk while the runtime lookup raises
  # stevedore NoMatches on the first secret write against OpenBao.
  # Stderr is captured rather than discarded: a dropped entry point, a moved
  # plugin module and an unresolved castellan driver otherwise collapse into
  # the same bare exit code that has to be reproduced by hand to tell apart.
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'from importlib.metadata import entry_points
eps = {e.name: e for e in entry_points(group="barbican.secretstore.plugin")}
eps["vault_plugin"].load()
drivers = {e.name: e for e in entry_points(group="castellan.drivers")}
drivers["vault"].load()' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "vault_plugin entry point and castellan vault driver load" "0" "$exit_code"
}

# --- Test 5: the uWSGI module path and its application symbol resolve ---
test_wsgi_module_resolvable() {
  echo "Test: barbican.wsgi.api resolves and binds application"
  # The barbican-operator launches uWSGI with the module:symbol pair
  # barbican.wsgi.api:application, so both halves have to exist in the image.
  # Resolve the module spec instead of importing it: importing
  # barbican.wsgi.api executes app.get_api_wsgi_script() at module level,
  # which loads the paste config from the hardcoded
  # /etc/barbican/barbican-api-paste.ini, and the bare image does not contain
  # that file. find_spec answers half the question this test asks — is the
  # module path the operator hands uWSGI resolvable — without running
  # module-level code, so do not simplify it into a plain import.
  # ast.parse over the module source answers the other half without executing
  # it either. Both pinned releases open with an `application = None` sentinel
  # and only rebind it inside `with threading.Lock():`, so the direct children
  # of the module node are not enough: the walk has to reach into nested
  # statement bodies (`with`, `if`, `try`), and a tree whose only binding is
  # that sentinel has to be rejected. It stops at every scope boundary though
  # (FunctionDef, AsyncFunctionDef, ClassDef, Lambda), because uWSGI looks
  # `application` up as a module global and a binding in a nested scope is a
  # different symbol. Were a partial upstream restructure to move the lock
  # block into a helper and leave the sentinel behind, counting that helper's
  # assignment would ship an image where uWSGI binds `application` to None and
  # every request to barbican-api fails.
  # Stderr is captured rather than discarded, so an unresolvable module and a
  # renamed symbol do not collapse into the same bare exit code.
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import ast, importlib.util, sys
spec = importlib.util.find_spec("barbican.wsgi.api")
if spec is None or spec.origin is None:
    sys.exit("barbican.wsgi.api does not resolve to a module file")
tree = ast.parse(open(spec.origin).read())
nested = {id(c) for n in ast.walk(tree)
          if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef, ast.Lambda))
          for c in ast.walk(n) if c is not n}
bindings = [n for n in ast.walk(tree)
            if isinstance(n, ast.Assign) and id(n) not in nested
            for t in n.targets if isinstance(t, ast.Name) and t.id == "application"]
if not bindings:
    sys.exit("barbican.wsgi.api binds no module-level application")
if all(isinstance(b.value, ast.Constant) and b.value.value is None for b in bindings):
    sys.exit("barbican.wsgi.api only binds application = None at module level")' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "barbican.wsgi.api resolves and binds application" "0" "$exit_code"
}

# --- Test 6: runs as openstack user ---
test_runs_as_openstack_user() {
  echo "Test: container runs as openstack user"
  local whoami_output exit_code=0
  whoami_output=$(docker run --rm "$IMAGE" whoami 2>&1) || exit_code=$?

  assert_eq "whoami exits 0" "0" "$exit_code"
  assert_eq "whoami outputs openstack" "openstack" "$whoami_output"
}

# --- Test 7: no build tools in final image ---
test_no_build_tools_in_final_image() {
  echo "Test: no build tools in final image"
  assert_no_build_tools
}

# --- Test 8: uwsgi is runnable (serves barbican-api at runtime) ---
test_uwsgi_runnable() {
  echo "Test: uwsgi --version succeeds"
  # Proves the venv's uwsgi, which links libpython3.12.so.1.0, starts.
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" /var/lib/openstack/bin/uwsgi --version 2>&1) || exit_code=$?

  assert_eq "uwsgi --version exits 0" "0" "$exit_code"
  assert_not_empty "uwsgi version output is non-empty" "$version"
  assert_uwsgi_imports barbican
  assert_uwsgi_graceful_shutdown
}

# --- Test 9: a clean-up batch that lost to a concurrent write is retried ---
test_db_clean_retry_patch_applied() {
  echo "Test: a deadlocked db clean-up batch is retried and committed"
  # Proves both build paths applied
  # patches/barbican/2026.1/0001-add-batched-db-cleanup-to-prevent-innodb-buffer-pool-exhaustion.patch
  # and patches/barbican/<release>/000N-retry-a-clean-up-batch-that-conflicts-with-a-concurrent-write.patch.
  # One soft-deleted transport key is committed to an in-memory database,
  # and the first commit of the clean-up raises DBDeadlock, which oslo.db
  # raises for 1213 and 1205 and subclasses for MariaDB's 1020
  # (DBConsistencyError). The patched image rolls the batch back,
  # runs it again and commits it: one row in two commits. An unpatched 2026.2
  # image lets the DBDeadlock escape, and an unpatched 2026.1 image deletes
  # the row without any commit, because 22.0.0 commits once at the end of
  # barbican-manage db clean. The second commit calls the real one, the
  # retry's wait is mocked, and stderr is echoed on failure so the reason is
  # named.
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import sys
from unittest import mock

from oslo_db import exception as db_exc
from oslo_utils import timeutils

from barbican.common import config
from barbican.model import clean
from barbican.model import models
from barbican.model import repositories as repo

CONF = config.CONF
CONF.set_override("connection", "sqlite://", "database")
CONF.set_override("db_auto_create", True)
repo.setup_database_engine_and_factory()

key = models.TransportKey("plugin", "key")
key.deleted = True
key.deleted_at = timeutils.utcnow()
repo.get_session().add(key)
repo.commit()

real_commit = repo.commit
calls = []


def commit():
    calls.append(None)
    if len(calls) == 1:
        raise db_exc.DBDeadlock()
    real_commit()


with mock.patch.object(clean.repo, "commit", side_effect=commit) as commits, \
        mock.patch("time.sleep"):
    removed = clean.cleanup_softdeletes(models.TransportKey)
left = repo.get_session().query(models.TransportKey).count()
if removed != 1 or commits.call_count != 2 or left != 0:
    sys.exit("removed %d rows in %d commits with %d left, want 1 row in "
             "2 commits with 0 left" % (removed, commits.call_count, left))' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "a deadlocked clean-up batch is retried and committed" "0" "$exit_code"
}

# --- Test 10: the OpenBao storage index of a write is sent with the read ---
test_vault_index_patch_applied() {
  echo "Test: the storage index of a Vault write is sent with the next read"
  # Proves both build paths applied
  # patches/barbican/<release>/000N-carry-the-openbao-storage-index-from-a-write-to-its-reads.patch.
  # The adapter the patch mounts on castellan's requests session sends a
  # POST whose response carries X-Vault-Index, records that index, and
  # sends the GET that requires it with X-Vault-Index and
  # X-Vault-Inconsistent: forward-active-node. The transport below the
  # adapter is mocked, so no server is needed. An unpatched image fails on
  # the import of _StorageIndexAdapter, and stderr is echoed on failure so
  # the reason is named.
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import sys
from unittest import mock

import requests

from barbican.plugin.vault_secret_store import _StorageIndexAdapter
from barbican.plugin.vault_secret_store import _StorageIndexState

sent = []


def send(adapter, request, **kwargs):
    sent.append(request)
    response = requests.Response()
    response.status_code = 200
    response.request = request
    if request.method == "POST":
        response.headers["X-Vault-Index"] = "idx-1"
    return response


state = _StorageIndexState()
adapter = _StorageIndexAdapter(state)
url = "https://bao.example:8200/v1/barbican/data/key"
with mock.patch.object(requests.adapters.HTTPAdapter, "send",
                       autospec=True, side_effect=send):
    adapter.send(requests.Request("POST", url, json={}).prepare())
    state.required = state.written
    adapter.send(requests.Request("GET", url).prepare())
read = sent[-1].headers
got = (state.written, read.get("X-Vault-Index"),
       read.get("X-Vault-Inconsistent"))
want = ("idx-1", "idx-1", "forward-active-node")
if got != want:
    sys.exit("recorded index, sent index and consistency header are %r, "
             "want %r" % (got, want))' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "the storage index of a write is sent with the read" "0" "$exit_code"
}

# --- Test 11: a secret metadata write that lost to a concurrent write is retried ---
test_secret_metadata_retry_patch_applied() {
  echo "Test: a secret metadata write that lost to a concurrent write is retried"
  # Proves the image build applied
  # patches/barbican/<release>/000N-retry-secret-metadata-writes-that-conflict-with-a-concurrent-write.patch
  # (#1752). create_replace_user_metadata, create_replace_user_metadatum and
  # delete_metadatum of SecretUserMetadatumRepo delete and insert rows of
  # secret_user_metadata in the request's transaction. Under concurrent
  # writes MariaDB fails them with error 1213 (a deadlock on a gap of the
  # unique index) or 1020, which oslo.db raises as DBDeadlock and its
  # subclass DBConsistencyError. Each method is called once on a secret
  # committed to an in-memory database, and its first INSERT or DELETE raises
  # DBDeadlock after the statement ran: the patched image rolls the session
  # back, runs the method again and stores the metadata, an unpatched image
  # lets the error escape. The retry's wait is mocked, and stderr is echoed
  # on failure so the reason is named.
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import sys
from unittest import mock

from oslo_db import exception as db_exc
from sqlalchemy import orm as sa_orm

from barbican.common import config
from barbican.model import models
from barbican.model import repositories as repo

CONF = config.CONF
CONF.set_override("connection", "sqlite://", "database")
CONF.set_override("db_auto_create", True)
repo.setup_database_engine_and_factory()

session = repo.get_session()
project = models.Project()
project.external_id = "verify"
project.save(session=session)
secret = models.Secret()
secret.project_id = project.id
secret.save(session=session)
secret_id = secret.id
repo.commit()

metadata = repo.get_secret_user_meta_repository()
real_save = models.SecretUserMetadatum.save
real_delete = sa_orm.Query.delete
state = {}


def lose_once():
    if state["lost"] is None:
        state["lost"] = True
        raise db_exc.DBDeadlock()


def save(model, session=None):
    real_save(model, session=session)
    if state["statement"] == "insert":
        lose_once()


def delete(query, *args, **kwargs):
    count = real_delete(query, *args, **kwargs)
    if state["statement"] == "delete":
        lose_once()
    return count


def check(name, statement, want, call):
    state.update(statement=statement, lost=None)
    try:
        with mock.patch.object(models.SecretUserMetadatum, "save",
                               autospec=True, side_effect=save), \
                mock.patch.object(sa_orm.Query, "delete", autospec=True,
                                  side_effect=delete), \
                mock.patch("time.sleep"):
            call()
    except db_exc.DBDeadlock:
        sys.exit("%s lets a DBDeadlock escape: the write is not retried"
                 % name)
    repo.commit()
    got = metadata.get_metadata_for_secret(secret_id)
    if not state["lost"] or got != want:
        sys.exit("%s: conflict raised: %s, metadata %r, want %r"
                 % (name, bool(state["lost"]), got, want))


check("create_replace_user_metadata", "insert", {"a": "1", "b": "2"},
      lambda: metadata.create_replace_user_metadata(
          secret_id, {"a": "1", "b": "2"}))
check("create_replace_user_metadatum", "insert", {"a": "3", "b": "2"},
      lambda: metadata.create_replace_user_metadatum(secret_id, "a", "3"))
check("delete_metadatum", "delete", {"b": "2"},
      lambda: metadata.delete_metadatum(secret_id, "a"))' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "secret metadata writes are retried after a conflict" "0" "$exit_code"
}

# --- Test 12: the five 204 answers carry no body ---
test_no_body_on_204_patch_applied() {
  echo "Test: the five 204 handlers answer without a body"
  # Proves the image build applied
  # patches/barbican/<release>/000N-do-not-send-a-json-body-with-204-responses.patch
  # (#1811). Upstream exposes the handlers of DELETE
  # /secrets/{id}/metadata/{key}, PUT and DELETE /project-quotas/{project},
  # and POST and DELETE /secret-stores/{id}/preferred with template='json',
  # so pecan sends the body null with their 204, and Envoy answers the client
  # 502. Each request is sent with Accept: application/json to the WSGI
  # application over an in-memory database, as in Test 11. policy_enforcer =
  # None skips RBAC the way upstream's BarbicanAPIBaseTestCase._build_context
  # does, and is_multiple_backends_enabled is mocked, because the bare image
  # has no secret-store configuration. Three error answers follow: the quota
  # PUT without a body expects 400, the quota and preferred-store DELETEs with
  # nothing left to delete expect 404, and each must carry a JSON object,
  # which python-barbicanclient and Tempest read the error from. An unpatched
  # image exits 1 with one line per 204 request, and stderr is echoed on
  # failure.
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import json
import sys
from unittest import mock

import webob

from barbican.api import app
from barbican.common import config
from barbican import context
from barbican.model import models
from barbican.model import repositories as repo

CONF = config.CONF
CONF.set_override("connection", "sqlite://", "database")
CONF.set_override("db_auto_create", True)
repo.setup_database_engine_and_factory()

session = repo.get_session()
project = models.Project()
project.external_id = "verify"
project.save(session=session)
secret = models.Secret()
secret.project_id = project.id
secret.save(session=session)
store = models.SecretStores(name="verify", store_plugin="store_crypto")
store.save(session=session)
secret_id, store_id = secret.id, store.id
repo.get_secret_user_meta_repository().create_replace_user_metadatum(
    secret_id, "extra", "1")
repo.commit()
repo.clear()

wsgi_app = app.build_wsgi_app(transactional=True)
bad = []


def call(method, path, body=None, want_status=204):
    request = webob.Request.blank(path, method=method)
    request.accept = "application/json"
    if body is not None:
        request.content_type = "application/json"
        request.body = body
    ctx = context.RequestContext(project_id="verify", is_admin=True)
    ctx.policy_enforcer = None
    request.environ["barbican.context"] = ctx
    response = request.get_response(wsgi_app)
    if want_status == 204:
        got = (response.status_int, response.body)
        want = (204, b"")
        ok = got == want
    else:
        got = (response.status_int, response.content_type, response.body)
        want = (want_status, "application/json", "a JSON object")
        ok = (got[:2] == want[:2]
              and isinstance(json.loads(response.body), dict))
    if not ok:
        bad.append("%s %s answers %r, want %r" % (method, path, got, want))


with mock.patch("barbican.common.utils.is_multiple_backends_enabled",
                return_value=True):
    call("DELETE", "/secrets/%s/metadata/extra" % secret_id)
    call("PUT", "/project-quotas/verify",
         json.dumps({"project_quotas": {}}).encode())
    call("DELETE", "/project-quotas/verify")
    call("POST", "/secret-stores/%s/preferred" % store_id)
    call("DELETE", "/secret-stores/%s/preferred" % store_id)
    call("PUT", "/project-quotas/verify", b"", want_status=400)
    call("DELETE", "/project-quotas/verify", want_status=404)
    call("DELETE", "/secret-stores/%s/preferred" % store_id, want_status=404)

if bad:
    sys.exit("\n".join(bad))' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "the five 204 handlers send no body and JSON errors" "0" "$exit_code"
}

# --- Test 13: the clean-up commits 500 rows per batch and can pause ---
test_db_clean_batch_options_patch_applied() {
  echo "Test: db clean commits small batches and pauses after each commit"
  # Proves both build paths applied
  # patches/barbican/2026.1/0006-commit-the-clean-up-in-small-batches-with-an-optional-pause.patch
  # and patches/barbican/2026.2/0005-commit-the-clean-up-in-small-batches-with-an-optional-pause.patch.
  # Five soft-deleted transport keys are committed to an in-memory database
  # and cleaned with batch_size=2 and batch_pause_ms=7: three committed
  # batches, each followed by a 7 ms pause. oslo.db calls time.sleep(0) on
  # each sqlite connection checkout, so only non-zero waits count. An
  # unpatched image has neither default nor the keyword and fails with an
  # AttributeError. The help text is squeezed to single spaces, because
  # argparse wraps it, and stderr is echoed on failure so the reason is named.
  local exit_code=0 err="" help="" out=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import sys
from unittest import mock

from oslo_utils import timeutils

from barbican.common import config
from barbican.model import clean
from barbican.model import models
from barbican.model import repositories as repo

if (clean.DEFAULT_CLEANUP_BATCH_SIZE, clean.DEFAULT_CLEANUP_BATCH_PAUSE_MS) \
        != (500, 0):
    sys.exit("defaults %r, want (500, 0)" % (
        (clean.DEFAULT_CLEANUP_BATCH_SIZE,
         clean.DEFAULT_CLEANUP_BATCH_PAUSE_MS),))

CONF = config.CONF
CONF.set_override("connection", "sqlite://", "database")
CONF.set_override("db_auto_create", True)
repo.setup_database_engine_and_factory()

for _ in range(5):
    key = models.TransportKey("plugin", "key")
    key.deleted = True
    key.deleted_at = timeutils.utcnow()
    repo.get_session().add(key)
repo.commit()

with mock.patch.object(clean.time, "sleep") as sleep, \
        mock.patch.object(clean.repo, "clear"):
    clean.clean_command(None, -1, False, False, False, None,
                        batch_size=2, batch_pause_ms=7)
waits = [c.args[0] for c in sleep.call_args_list if c.args[0]]
left = repo.get_session().query(models.TransportKey).count()
if waits != [0.007] * 3 or left != 0:
    sys.exit("waited %r with %d rows left, want [0.007] * 3 with 0 left"
             % (waits, left))' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"
  assert_eq "five rows in batches of 2 pause three times for 7 ms" "0" "$exit_code"

  exit_code=0
  help=$(docker run --rm "$IMAGE" barbican-manage db clean --help 2>/dev/null) || exit_code=$?
  help=$(printf '%s' "$help" | tr -s '[:space:]' ' ')
  assert_eq "barbican-manage db clean --help exits 0" "0" "$exit_code"
  assert_contains "--help lists --batch-size with its default" "$help" \
    "--batch-size <rows>, -b <rows> Number of rows to change per committed batch. Default is 500."
  assert_contains "--help lists --batch-pause-ms with its default" "$help" \
    "--batch-pause-ms <milliseconds> Milliseconds to wait after each committed batch; 0 disables the pause. Default is 0."

  exit_code=0
  out=$(docker run --rm "$IMAGE" barbican-manage db clean --batch-size 0 --db-url sqlite:// 2>&1) || exit_code=$?
  assert_eq "db clean --batch-size 0 exits 1" "1" "$exit_code"
  assert_contains "db clean --batch-size 0 names the bound" "$out" \
    "ERROR: batch_size must be at least 1, got 0"
}

# --- Test 14: a commit that raises leaves no prepared session behind ---
test_request_session_cleared_patch_applied() {
  echo "Test: the request session is cleared after a failed commit"
  # Proves both build paths applied
  # patches/barbican/2026.1/0007-clear-the-request-session-when-its-commit-fails.patch
  # and patches/barbican/2026.2/0006-clear-the-request-session-when-its-commit-fails.patch.
  # The transaction hook is built with a commit that raises
  # DBConnectionError, as a commit does on a socket read timeout, and runs
  # after a transactional request on an in-memory database. The patched hook
  # re-raises and clears the thread's session, so the next get_session()
  # returns a new session that runs SELECT 1. An unpatched hook skips the
  # clear and returns the old session, which SQLAlchemy leaves in prepared
  # state after a real failed commit. stderr is echoed on failure.
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import sys
from unittest import mock

from oslo_db import exception as db_exc
import sqlalchemy

from barbican.api import hooks
from barbican.common import config
from barbican.model import repositories as repo

CONF = config.CONF
CONF.set_override("connection", "sqlite://", "database")
CONF.set_override("db_auto_create", True)
repo.setup_database_engine_and_factory()

with mock.patch.object(repo, "commit",
                       side_effect=db_exc.DBConnectionError()):
    hook = hooks.BarbicanTransactionHook()
state = mock.Mock(spec=["request", "controller"])
state.controller = None
state.request = mock.Mock(spec=["transactional", "error"])
state.request.transactional = True
state.request.error = False

old = repo.get_session()
try:
    hook.after(state)
    sys.exit("after() did not re-raise the failed commit")
except db_exc.DBConnectionError:
    pass
new = repo.get_session()
if new is old:
    sys.exit("after() kept the session of the failed commit")
if new.execute(sqlalchemy.text("SELECT 1")).scalar() != 1:
    sys.exit("the new session does not run SELECT 1")' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "a failed commit leaves a new, usable session" "0" "$exit_code"
}

# --- Test 15: soft-deleted dependents go with their parent ---
test_db_clean_dependents_patch_applied() {
  echo "Test: db clean deletes soft-deleted metadata set after their secret with it"
  # Proves both build paths applied
  # patches/barbican/2026.1/0008-delete-soft-deleted-dependents-with-their-parent-in-the-clean-up.patch
  # and patches/barbican/2026.2/0007-delete-soft-deleted-dependents-with-their-parent-in-the-clean-up.patch.
  # Foreign keys are enforced on the in-memory database through
  # PRAGMA foreign_keys=ON on connect, as barbican/tests/database_utils.py
  # does. First check: a secret soft-deleted at S whose store and user
  # metadata carry S+1 s is cleaned with the cutoff at S+0.5 s, and all three
  # rows go. An unpatched image keeps the metadata and fails the DELETE of
  # the secret with "FOREIGN KEY constraint failed". Second check: three
  # soft-deleted transport keys and a secret whose store metadatum is live;
  # clean_command raises DBReferenceError and hands LOG.exception the 4 rows
  # its batches committed (the keys and the soft-deleted user metadatum),
  # and the secret and its live metadatum stay. The project id is read
  # before the first commit, because each batch detaches the loaded objects.
  # stderr is echoed on failure.
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import datetime
import sys
from unittest import mock

from sqlalchemy import event
from sqlalchemy.engine import Engine

from barbican.common import config
from barbican.model import clean
from barbican.model import models
from barbican.model import repositories as repo


@event.listens_for(Engine, "connect")
def _enforce_foreign_keys(dbapi_connection, connection_record):
    dbapi_connection.execute("PRAGMA foreign_keys=ON")


CONF = config.CONF
CONF.set_override("connection", "sqlite://", "database")
CONF.set_override("db_auto_create", True)
repo.setup_database_engine_and_factory()
session = repo.get_session()

parent_at = datetime.datetime(2026, 11, 22, 10, 11, 43)
later = parent_at + datetime.timedelta(seconds=1)
between = parent_at + datetime.timedelta(microseconds=500000)

project = models.Project()
project.external_id = "p"
project.save(session)
project_id = project.id


def secret_with_metadata(store_deleted):
    secret = models.Secret()
    secret.project_id = project_id
    secret.deleted = True
    secret.deleted_at = parent_at
    secret.save(session)
    store = models.SecretStoreMetadatum("k", "v")
    user = models.SecretUserMetadatum("k", "v")
    for row, deleted in ((store, store_deleted), (user, True)):
        row.secret_id = secret.id
        row.deleted = deleted
        row.deleted_at = later if deleted else None
        row.save(session)


def rows():
    s = repo.get_session()
    return tuple(s.query(m).count() for m in (
        models.Secret, models.SecretStoreMetadatum,
        models.SecretUserMetadatum))


secret_with_metadata(True)
repo.commit()
total = clean.cleanup_all(threshold_date=between)
if total != 3 or rows() != (0, 0, 0):
    sys.exit("cleaned %r with %r rows left, want 3 with (0, 0, 0)"
             % (total, rows()))

for _ in range(3):
    key = models.TransportKey("plugin", "key")
    key.deleted = True
    key.deleted_at = parent_at
    repo.get_session().add(key)
secret_with_metadata(False)
repo.commit()
with mock.patch.object(clean.LOG, "exception") as log_exception, \
        mock.patch.object(clean.repo, "clear"):
    try:
        clean.clean_command(None, -1, False, False, False, None)
    except Exception as exc:
        failure = type(exc).__name__
    else:
        sys.exit("a secret with a live metadata row was cleaned")
if failure != "DBReferenceError" or log_exception.call_args.args[1] != 4 \
        or rows() != (1, 1, 0):
    sys.exit("%s, %r entries counted, %r rows left, want DBReferenceError, "
             "4 and (1, 1, 0)" % (failure, log_exception.call_args.args[1],
                                  rows()))' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "soft-deleted metadata stamped after their secret go with it, and a failed run counts its batches" "0" "$exit_code"
}

# --- Run all tests ---
echo "=== barbican container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_barbican_manage_version
echo ""
test_barbican_status_help
echo ""
test_barbican_importable
echo ""
test_vault_backend_loadable
echo ""
test_wsgi_module_resolvable
echo ""
test_runs_as_openstack_user
echo ""
test_no_build_tools_in_final_image
echo ""
test_uwsgi_runnable
echo ""
test_db_clean_retry_patch_applied
echo ""
test_vault_index_patch_applied
echo ""
test_secret_metadata_retry_patch_applied
echo ""
test_no_body_on_204_patch_applied
echo ""
test_db_clean_batch_options_patch_applied
echo ""
test_request_session_cleared_patch_applied
echo ""
test_db_clean_dependents_patch_applied
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
