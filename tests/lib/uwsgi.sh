#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Shared check that the uWSGI of a Nix-built service image imports the
# service package, for the verify scripts under tests/container-images/.
#
# The operators launch the API of every service through the venv's uwsgi with
# --module or --wsgi-file, so the import has to succeed inside uWSGI's
# embedded interpreter, not only under /var/lib/openstack/bin/python. The two
# differ: uWSGI derives the program name of its interpreter from
# readlink /proc/self/exe, and Python finds the venv (its pyvenv.cfg, and
# with it the site-packages that hold the service) beside that path or not at
# all. A bin/uwsgi that links into the uwsgi package's store path therefore
# starts with the base interpreter's sys.prefix, and `uwsgi --version` still
# exits 0 while every application import fails with ModuleNotFoundError.
#
# --pyrun runs a script in that interpreter: uWSGI exits with the status a
# SystemExit in the script carries, so the probe exits 1 with the exception
# and sys.prefix on failure. Kept here so the probe is edited once instead of
# once per image.

# assert_uwsgi_imports <module>
# Assert that `uwsgi --pyrun` imports <module>. Needs $IMAGE and the caller's
# PASS/FAIL counters.
assert_uwsgi_imports() {
  local module="$1" output exit_code=0
  local probe
  probe=$(
    cat <<EOF
import sys
try:
    import ${module}
except ImportError as exc:
    sys.exit("uwsgi cannot import ${module}: %s (sys.prefix=%s)" % (exc, sys.prefix))
EOF
  )
  # The probe file goes through /tmp because --pyrun takes a path; the
  # heredoc text travels in an environment variable so no quoting layer of
  # docker run or sh -c touches it.
  output=$(docker run --rm -e UWSGI_PROBE="$probe" "$IMAGE" sh -c \
    'printf "%s\n" "$UWSGI_PROBE" > /tmp/uwsgi-probe.py && uwsgi --pyrun /tmp/uwsgi-probe.py' 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $(grep -E 'uwsgi cannot import|Error' <<<"$output" | tail -3)"
  assert_eq "uwsgi --pyrun imports ${module}" "0" "$exit_code"
}
