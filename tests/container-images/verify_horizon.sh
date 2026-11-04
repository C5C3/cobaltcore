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
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
