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
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
