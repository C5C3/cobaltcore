#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify horizon container image meets requirements
# Usage: bash tests/container-images/verify_horizon.sh [image_name]
# Default image: c5c3/horizon:25.7.0
# Requires: Docker daemon running

set -euo pipefail

IMAGE="${1:-c5c3/horizon:25.7.0}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"
# shellcheck source=tests/lib/build_tools.sh
source "$SCRIPT_DIR/../lib/build_tools.sh"
# shellcheck source=tests/lib/uwsgi.sh
source "$SCRIPT_DIR/../lib/uwsgi.sh"

# --- Test 1: openstack_dashboard is importable ---
test_openstack_dashboard_importable() {
  echo "Test: openstack_dashboard imports cleanly"
  local exit_code=0
  docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c "import openstack_dashboard" > /dev/null 2>&1 || exit_code=$?

  assert_eq "import openstack_dashboard exits 0" "0" "$exit_code"
}

# --- Test 2: oslo.log's JSON formatter is importable ---
# The horizon-operator renders oslo_log.formatters.JSONFormatter into the
# LOGGING dictConfig for spec.logging.format json; without it Django fails
# with "Unable to configure formatter 'json'" and uWSGI loads no app.
test_oslo_log_json_formatter_importable() {
  echo "Test: oslo_log.formatters.JSONFormatter imports cleanly"
  local exit_code=0
  docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c 'import oslo_log.formatters as f; f.JSONFormatter' > /dev/null 2>&1 || exit_code=$?

  assert_eq "import oslo_log.formatters.JSONFormatter exits 0" "0" "$exit_code"
}

# --- Test 3: static assets are pre-built at image-build time ---
test_static_assets_present() {
  echo "Test: static assets are pre-built"

  # Every path below /var/lib/openstack is a symlink into /nix/store, so the
  # count follows links: a plain `find -type f` counts none.
  local asset_count exit_code=0
  asset_count=$(docker run --rm "$IMAGE" /var/lib/openstack/bin/python -c \
    'import os
print(sum(len(files) for _, _, files in os.walk("/var/lib/openstack/horizon-static", followlinks=True)))' \
    2>&1 | tr -d '[:space:]') || exit_code=$?

  assert_eq "static root listing exits 0" "0" "$exit_code"
  if [ "${asset_count:-0}" -gt 0 ]; then
    echo "  PASS: /var/lib/openstack/horizon-static contains files ($asset_count)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: /var/lib/openstack/horizon-static is empty"
    FAIL=$((FAIL + 1))
  fi

  # django-compressor offline manifest proves `compress --force` ran.
  local manifest_exit=0
  docker run --rm "$IMAGE" \
    test -f /var/lib/openstack/horizon-static/dashboard/manifest.json > /dev/null 2>&1 || manifest_exit=$?
  assert_eq "offline compression manifest exists" "0" "$manifest_exit"
}

# --- Test 4: runs as openstack user ---
test_runs_as_openstack_user() {
  echo "Test: container runs as openstack user"
  local whoami_output exit_code=0
  whoami_output=$(docker run --rm "$IMAGE" whoami 2>&1) || exit_code=$?

  assert_eq "whoami exits 0" "0" "$exit_code"
  assert_eq "whoami outputs openstack" "openstack" "$whoami_output"
}

# --- Test 5: no build tools in final image ---
test_no_build_tools_in_final_image() {
  echo "Test: no build tools in final image"
  assert_no_build_tools
}

# --- Test 6: uwsgi is runnable (serves openstack_dashboard.wsgi at runtime) ---
test_uwsgi_runnable() {
  echo "Test: uwsgi --version succeeds"
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" /var/lib/openstack/bin/uwsgi --version 2>&1) || exit_code=$?

  assert_eq "uwsgi --version exits 0" "0" "$exit_code"
  assert_not_empty "uwsgi version output is non-empty" "$version"
  assert_uwsgi_imports openstack_dashboard
  assert_uwsgi_graceful_shutdown
}

# --- Test 7: the rendered pymemcache OPTIONS bound Django's cache ---
test_django_pymemcache_bounded() {
  echo "Test: the rendered pymemcache OPTIONS bound Django's cache and errors are misses"
  # The horizon-operator renders CACHES["default"]["OPTIONS"] for
  # PyMemcacheCache in defaultSettings in
  # operators/horizon/internal/controller/reconcile_config.go (#1560): 1 s
  # connect and read timeouts and ignore_exc. The check builds Django's
  # PyMemcacheCache with those OPTIONS against a local server that accepts
  # and never answers, and expects the default within 3 s, which pins the
  # option names against the Django and pymemcache of the image. Without
  # the OPTIONS the call blocks for as long as TCP does; a 10 s alarm ends
  # it, so the check fails without hanging the job. Stderr is echoed on
  # failure so the reason is named.
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import signal
import socket
import sys
import threading
import time

import django.conf
from django.core.cache.backends.memcached import PyMemcacheCache


def still_blocked(signum, frame):
    sys.exit("cache.get was still blocked after 10 s: the OPTIONS do not "
             "bound the pymemcache client")


signal.signal(signal.SIGALRM, still_blocked)
signal.alarm(10)

# A server that accepts connections and never answers. Every accepted
# socket stays referenced: a closed one would answer with EOF at once.
listener = socket.socket()
listener.bind(("127.0.0.1", 0))
listener.listen(8)
accepted = []


def accept():
    while True:
        accepted.append(listener.accept()[0])


threading.Thread(target=accept, daemon=True).start()

django.conf.settings.configure()
cache = PyMemcacheCache(
    "127.0.0.1:%d" % listener.getsockname()[1],
    {"OPTIONS": {"connect_timeout": 1, "timeout": 1, "ignore_exc": True}},
)
start = time.monotonic()
value = cache.get("k", "fallback")
took = time.monotonic() - start
if value != "fallback":
    sys.exit("cache.get returned %r, expected the default" % (value,))
if took >= 3:
    sys.exit("cache.get took %.2f s, expected under 3 s" % took)' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "the rendered pymemcache OPTIONS bound Django's cache and errors are misses" "0" "$exit_code"
}

# --- Test 8: uWSGI serves the static assets with their Content-Type ---
test_static_assets_typed() {
  echo "Test: uWSGI serves the static assets with their Content-Type"
  # The horizon-operator serves /static through uWSGI's --static-map
  # (uwsgiCommand in operators/horizon/internal/controller/reconcile_deployment.go),
  # and uWSGI types each file from /etc/mime.types. Without that table it
  # sends the assets untyped, and a browser never sniffs an untyped SVG as an
  # image, so the login page logo stays empty (#1624). The check starts uWSGI
  # with the operator's --static-map and no application and fetches the
  # splash logo the login page links and one compressed stylesheet. The
  # probe travels in an environment variable, as in tests/lib/uwsgi.sh.
  local probe output exit_code=0
  probe=$(
    cat <<'EOF'
import glob
import os
import sys
import time
import urllib.error
import urllib.request

root = "/var/lib/openstack/horizon-static"
stylesheets = sorted(glob.glob(root + "/dashboard/css/*.css"))
if not stylesheets:
    sys.exit("no stylesheet under %s/dashboard/css" % root)
paths = [
    "/static/dashboard/img/logo-splash.svg",
    "/static/" + os.path.relpath(stylesheets[0], root),
]
for path in paths:
    for _ in range(50):
        try:
            response = urllib.request.urlopen("http://127.0.0.1:8080" + path, timeout=2)
            break
        except urllib.error.HTTPError as exc:
            sys.exit("GET %s -> HTTP %d" % (path, exc.code))
        except OSError:
            time.sleep(0.2)
    else:
        sys.exit("uWSGI never answered GET %s" % path)
    print("%s %s" % (path.rsplit(".", 1)[1], response.headers.get("Content-Type", "none")))
EOF
  )
  output=$(docker run --rm -e STATIC_PROBE="$probe" "$IMAGE" sh -c \
    'uwsgi --http :8080 --static-map /static=/var/lib/openstack/horizon-static --logto /tmp/uwsgi.log &
     /var/lib/openstack/bin/python -c "$STATIC_PROBE"' 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $output"

  assert_eq "static probe exits 0" "0" "$exit_code"
  assert_eq "logo-splash.svg is served as image/svg+xml" "image/svg+xml" "$(awk '$1 == "svg" { print $2 }' <<<"$output")"
  assert_eq "the compressed stylesheet is served as text/css" "text/css" "$(awk '$1 == "css" { print $2 }' <<<"$output")"
}

# --- Test 9: the RBAC Policies panel checks the catalog before it asks Neutron ---
test_rbac_policies_panel_checks_catalog_first() {
  echo "Test: the RBAC Policies panel asks Neutron only for a catalog with a network service"
  # Proves the image build applied
  # patches/horizon/<release>/0001-check-the-service-catalog-before-the-rbac-policies-panel-asks-neutron.patch
  # (#1706). Upstream RBACPolicies.allowed() asks Neutron for its
  # rbac-policies extension even when the token's catalog has no network
  # service, and logs "RBAC Policies panel will not be displayed." at ERROR
  # on every page that renders the sidebar. The probe calls allowed() for an
  # active admin user, first with a catalog without a network service, which
  # must answer False without an ERROR record, then with a network service
  # and a mocked extension probe, which must answer True after one probe
  # call. Django needs a SECRET_KEY in local_settings.py: without one,
  # openstack_dashboard/settings.py tries to write a key file into the
  # read-only store and fails with PermissionError. Against an unpatched
  # image the first case fails and the output quotes the ERROR line. Stderr
  # is echoed on failure so the reason is named.
  local probe err="" exit_code=0
  probe=$(
    cat <<'EOF'
import datetime
import logging
import sys
import types
from unittest import mock

import django

django.setup()

from django.test import RequestFactory
from openstack_auth import user as auth_user
from openstack_dashboard.api import neutron
from openstack_dashboard.dashboards.admin.rbac_policies import panel


class Collect(logging.Handler):
    def __init__(self):
        super().__init__(level=logging.ERROR)
        self.messages = []

    def emit(self, record):
        self.messages.append(record.getMessage())


# A silenced logger would let the first case pass on any image.
collect = Collect()
panel.LOG.addHandler(collect)
panel.LOG.error("verify: handler self-check")
if collect.messages != ["verify: handler self-check"]:
    sys.exit("the handler on panel.LOG caught %r" % (collect.messages,))
collect.messages.clear()


def service(kind, name, url):
    return {"type": kind, "name": name, "endpoints": [
        {"region": "RegionOne", "interface": "internal", "url": url}]}


def allowed(catalog):
    request = RequestFactory().get("/")
    request.session = {}
    token = types.SimpleNamespace(
        id="verify",
        expires=datetime.datetime.now(datetime.timezone.utc)
        + datetime.timedelta(hours=1))
    request.user = auth_user.User(
        id="1", token=token, user="admin", tenant_id="1",
        service_catalog=catalog, roles=[{"id": "1", "name": "admin"}],
        services_region="RegionOne", enabled=True,
        endpoint="http://keystone.invalid/v3")
    # An inactive or unauthenticated user holds no service permission, so
    # the first case would pass for the wrong reason.
    if not (request.user.is_authenticated and request.user.is_active):
        sys.exit("the probe's user is not active and authenticated")
    return panel.RBACPolicies().allowed({"request": request})


identity = service("identity", "keystone", "http://keystone.invalid/v3")
network = service("network", "neutron", "http://neutron.invalid")

result = allowed([identity])
if result is not False or collect.messages:
    sys.exit("without a network service allowed() returned %r and logged %r"
             % (result, collect.messages))

with mock.patch.object(neutron, "is_extension_supported",
                       return_value=True) as extension_probe:
    result = allowed([identity, network])
if result is not True or extension_probe.call_count != 1:
    sys.exit("with a network service allowed() returned %r after %d probe(s)"
             % (result, extension_probe.call_count))
EOF
  )
  err=$(docker run --rm --tmpfs /etc/openstack-dashboard:uid=42424,gid=42424 \
    -e DJANGO_SETTINGS_MODULE=openstack_dashboard.settings -e PANEL_PROBE="$probe" "$IMAGE" sh -c \
    'printf "SECRET_KEY = \"verify\"\n" > /etc/openstack-dashboard/local_settings.py &&
     exec /var/lib/openstack/bin/python -c "$PANEL_PROBE"' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "the RBAC Policies panel checks the catalog before it asks Neutron" "0" "$exit_code"
}

# --- Test 10: OPENSTACK_REQUEST_TIMEOUT reaches every client ---
test_request_timeout_reaches_every_client() {
  echo "Test: OPENSTACK_REQUEST_TIMEOUT bounds every Keystone and service client"
  # Proves the image build applied
  # patches/horizon/<release>/0002-add-a-request-timeout-for-the-keystone-and-service-clients.patch
  # (#1799). Upstream Horizon builds every keystoneauth session and service
  # client without a timeout, and the horizon-operator's
  # OPENSTACK_REQUEST_TIMEOUT = 30 is inert there. The probe renders
  # OPENSTACK_REQUEST_TIMEOUT = 7 into local_settings.py, asserts that
  # openstack_auth's get_session() carries it, and calls every patched
  # factory with its session or client constructor mocked. The factories
  # differ by release: horizon 25.x still builds a neutronclient, 27.x a
  # second Nova session (computeclient). The second pass sets the setting
  # to None: the sessions get timeout=None, and the Glance client gets no
  # timeout argument, so glanceclient keeps its 600 seconds. A fresh request
  # per call keeps the memoized factories from answering from a cache.
  # Stderr is echoed on failure so the reason is named.
  local probe err="" exit_code=0
  probe=$(
    cat <<'EOF'
import datetime
import importlib.metadata
import sys
import types
from unittest import mock

import django

django.setup()

from django.conf import settings
from django.test import RequestFactory
from django.test.utils import override_settings
from openstack_auth import user as auth_user
from openstack_auth import utils as auth_utils
from openstack_dashboard.api import _nova
from openstack_dashboard.api import cinder
from openstack_dashboard.api import glance
from openstack_dashboard.api import keystone
from openstack_dashboard.api import neutron
from openstack_dashboard.api import placement

if settings.OPENSTACK_REQUEST_TIMEOUT != 7:
    sys.exit("OPENSTACK_REQUEST_TIMEOUT is %r, expected 7 from "
             "local_settings.py" % (settings.OPENSTACK_REQUEST_TIMEOUT,))


# The factories look an endpoint up by OPENSTACK_ENDPOINT_TYPE (publicURL by
# default), so every service carries a public and an internal entry.
def service(kind, name, url):
    return {"type": kind, "name": name, "endpoints": [
        {"region": "RegionOne", "interface": interface, "url": url}
        for interface in ("public", "internal")]}


CATALOG = [
    service("identity", "keystone", "http://keystone.invalid/v3"),
    service("network", "neutron", "http://neutron.invalid"),
    service("compute", "nova", "http://nova.invalid/v2.1"),
    service("volumev3", "cinderv3", "http://cinder.invalid/v3/1"),
    service("image", "glance", "http://glance.invalid"),
    service("placement", "placement", "http://placement.invalid"),
]


def request():
    req = RequestFactory().get("/")
    req.session = {}
    token = types.SimpleNamespace(
        id="verify", project={"id": "1", "domain_id": "default"},
        expires=datetime.datetime.now(datetime.timezone.utc)
        + datetime.timedelta(hours=1))
    req.user = auth_user.User(
        id="1", token=token, user="admin", tenant_id="1",
        service_catalog=CATALOG, roles=[{"id": "1", "name": "admin"}],
        services_region="RegionOne", enabled=True,
        endpoint="http://keystone.invalid/v3")
    return req


def keyword(target, attribute, call, extra=()):
    patches = [mock.patch.object(target, attribute)]
    patches += [mock.patch(name) for name in extra]
    mocks = [p.start() for p in patches]
    try:
        call()
    finally:
        for p in patches:
            p.stop()
    return mocks[0].call_args.kwargs


CONNECTION = ("openstack.connection.Connection",)
CHECKS = [
    ("keystone session", lambda: keyword(
        keystone.session, "Session", lambda: keystone.keystoneclient(request()))),
    ("placement session", lambda: keyword(
        placement.session, "Session", lambda: placement.make_adapter(request()))),
    ("neutron session", lambda: keyword(
        neutron.session, "Session", lambda: neutron.networkclient(request()),
        CONNECTION)),
    ("novaclient", lambda: keyword(
        _nova.nova_client, "Client",
        lambda: _nova.novaclient(request(), version="2.1"))),
    ("cinderclient", lambda: keyword(
        cinder.cinder_client, "Client", lambda: cinder.cinderclient(request()))),
]
major = int(importlib.metadata.version("horizon").split(".")[0])
if major < 27:
    CHECKS.append(("neutronclient", lambda: keyword(
        neutron.neutron_client, "Client",
        lambda: neutron.neutronclient(request()))))
else:
    CHECKS.append(("nova session", lambda: keyword(
        _nova.session, "Session", lambda: _nova.computeclient(request()),
        CONNECTION)))


def glance_kwargs():
    with mock.patch.object(glance.VERSIONS, "get_active_version") as version:
        glance.glanceclient(request())
    return version.return_value["client"].Client.call_args.kwargs


def check(expected):
    found = auth_utils.get_session().timeout
    if found != expected:
        sys.exit("get_session().timeout is %r, expected %r" % (found, expected))
    for name, kwargs in CHECKS:
        found = kwargs()
        if "timeout" not in found or found["timeout"] != expected:
            sys.exit("the %s got %r, expected timeout=%r"
                     % (name, found, expected))


check(7)
found = glance_kwargs()
if found.get("timeout") != 7:
    sys.exit("the Glance client got %r, expected timeout=7" % (found,))

with override_settings(OPENSTACK_REQUEST_TIMEOUT=None):
    check(None)
    found = glance_kwargs()
    if "timeout" in found:
        sys.exit("with the setting at None the Glance client got %r, "
                 "expected no timeout argument" % (found,))
EOF
  )
  err=$(docker run --rm --tmpfs /etc/openstack-dashboard:uid=42424,gid=42424 \
    -e DJANGO_SETTINGS_MODULE=openstack_dashboard.settings -e TIMEOUT_PROBE="$probe" "$IMAGE" sh -c \
    'printf "SECRET_KEY = \"verify\"\nOPENSTACK_REQUEST_TIMEOUT = 7\n" > /etc/openstack-dashboard/local_settings.py &&
     exec /var/lib/openstack/bin/python -W ignore -c "$TIMEOUT_PROBE"' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "OPENSTACK_REQUEST_TIMEOUT reaches every Keystone and service client" "0" "$exit_code"
}

# --- Test 11: an oversized session cookie is logged and announced at the login ---
test_session_cookie_size_is_announced() {
  echo "Test: an oversized session cookie is logged and announced at the login"
  # Proves the image build applied
  # patches/horizon/<release>/0003-warn-when-the-session-cookie-exceeds-the-size-a-browser-keeps.patch
  # (#1845). Upstream Horizon checks the session cookie only on the request
  # side; a browser discards a cookie over its limit, so the next request
  # carries none, and the login page and the log stay silent. The probe
  # asserts that MIDDLEWARE lists SessionCookieSizeMiddleware directly
  # before Django's SessionMiddleware, then sends a small and a large
  # signed-cookie session through the image's middleware chain with
  # django.test.Client: the small one must set a session cookie, no
  # logout_reason cookie and log nothing, the large one must set
  # logout_reason and logout_status=error and log one ERROR record naming
  # its size. Last it drops the session cookie, as a browser would, follows
  # the redirects from LOGIN_REDIRECT_URL as a browser does after the login,
  # and asserts that they end on the login page and that it shows the
  # message in its alert-danger box. local_settings.py
  # sets CACHES to the local-memory cache, because upstream's default
  # points at a Memcached on 127.0.0.1 and the login page reads the cache;
  # COMPRESS_OFFLINE and STATIC_ROOT let the login page render from the
  # manifest built into the image; ALLOWED_HOSTS admits the test client.
  # Against an unpatched image the MIDDLEWARE check fails. Stderr is echoed
  # on failure so the reason is named.
  local probe err="" exit_code=0
  probe=$(
    cat <<'EOF'
import logging
import re
import secrets
import sys
import types

import django

django.setup()

from django.conf import settings
from django.http import HttpResponse
from django.test import Client
from django.test.utils import override_settings
from django.urls import include, path

SIZE_CHECK = "horizon.middleware.SessionCookieSizeMiddleware"
SESSION = "django.contrib.sessions.middleware.SessionMiddleware"
REASON = ("Your session is too large for a browser cookie and the login "
          "cannot complete. Contact your administrator.")

middleware = list(settings.MIDDLEWARE)
if (SIZE_CHECK not in middleware or SESSION not in middleware
        or middleware.index(SIZE_CHECK) + 1 != middleware.index(SESSION)):
    sys.exit("MIDDLEWARE does not list %s directly before %s: %r"
             % (SIZE_CHECK, SESSION, middleware))

from horizon.middleware import session_cookie_size


class Collect(logging.Handler):
    def __init__(self):
        super().__init__(level=logging.ERROR)
        self.messages = []

    def emit(self, record):
        self.messages.append(record.getMessage())


# A silenced logger would let the large session fail for the wrong reason.
collect = Collect()
session_cookie_size.LOG.addHandler(collect)
session_cookie_size.LOG.error("verify: handler self-check")
if collect.messages != ["verify: handler self-check"]:
    sys.exit("the handler on session_cookie_size.LOG caught %r"
             % (collect.messages,))
collect.messages.clear()


# The signed-cookie store compresses the session and the hex shrinks to
# about 60 percent: 6000 hex characters give a cookie of about 4.7 KB.
def pad(request, size):
    request.session["pad"] = secrets.token_hex(size // 2)
    return HttpResponse()


verify_urls = types.ModuleType("verify_urls")
verify_urls.urlpatterns = [
    path("verify-session/<int:size>/", pad),
    path("", include("openstack_dashboard.urls")),
]
sys.modules["verify_urls"] = verify_urls

with override_settings(ROOT_URLCONF="verify_urls"):
    client = Client()

    r = client.get("/verify-session/1000/")
    if settings.SESSION_COOKIE_NAME not in r.cookies:
        sys.exit("a small session set no %s cookie: %s"
                 % (settings.SESSION_COOKIE_NAME, sorted(r.cookies)))
    if "logout_reason" in r.cookies or collect.messages:
        sys.exit("a small session was flagged: %s, %r"
                 % (sorted(r.cookies), collect.messages))

    r = client.get("/verify-session/6000/")
    cookie = r.cookies.get(settings.SESSION_COOKIE_NAME)
    size = len(cookie.key) + len(cookie.value) if cookie else 0
    status = r.cookies.get("logout_status")
    if ("logout_reason" not in r.cookies or status is None
            or status.value != "error"):
        sys.exit("a %d-byte session cookie set no logout_reason: %s"
                 % (size, sorted(r.cookies)))
    if (len(collect.messages) != 1
            or "%dB >= 4093B" % size not in collect.messages[0]):
        sys.exit("expected one ERROR record naming %dB, got %r"
                 % (size, collect.messages))

    client.cookies.pop(settings.SESSION_COOKIE_NAME, None)
    r = client.get(settings.LOGIN_REDIRECT_URL, follow=True)
    if r.request["PATH_INFO"] != settings.LOGIN_URL:
        sys.exit("%s without a session did not end on the login page: %r"
                 % (settings.LOGIN_REDIRECT_URL, r.redirect_chain))
    match = re.search(r'alert (alert-\w+)" id="logout_reason">\s*<p>(.*?)</p>',
                      r.content.decode())
    if (r.status_code != 200 or match is None
            or match.groups() != ("alert-danger", REASON)):
        sys.exit("the login page (HTTP %d) shows no error reason: %r"
                 % (r.status_code, match and match.groups()))
EOF
  )
  err=$(docker run --rm --tmpfs /etc/openstack-dashboard:uid=42424,gid=42424 \
    -e DJANGO_SETTINGS_MODULE=openstack_dashboard.settings -e COOKIE_PROBE="$probe" "$IMAGE" sh -c \
    'printf "%s\n" \
       "SECRET_KEY = \"verify\"" \
       "SESSION_ENGINE = \"django.contrib.sessions.backends.signed_cookies\"" \
       "CACHES = {\"default\": {\"BACKEND\": \"django.core.cache.backends.locmem.LocMemCache\"}}" \
       "COMPRESS_OFFLINE = True" \
       "STATIC_ROOT = \"/var/lib/openstack/horizon-static\"" \
       "ALLOWED_HOSTS = [\"testserver\"]" \
       > /etc/openstack-dashboard/local_settings.py &&
     exec /var/lib/openstack/bin/python -W ignore -c "$COOKIE_PROBE"' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "an oversized session cookie is logged and announced at the login" "0" "$exit_code"
}

# --- Run all tests ---
echo "=== horizon container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_openstack_dashboard_importable
echo ""
test_oslo_log_json_formatter_importable
echo ""
test_static_assets_present
echo ""
test_runs_as_openstack_user
echo ""
test_no_build_tools_in_final_image
echo ""
test_uwsgi_runnable
echo ""
test_django_pymemcache_bounded
echo ""
test_static_assets_typed
echo ""
test_rbac_policies_panel_checks_catalog_first
echo ""
test_request_timeout_reaches_every_client
echo ""
test_session_cookie_size_is_announced
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
