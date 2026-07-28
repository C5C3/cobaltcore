#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify glance container image meets requirements
# Usage: bash tests/container-images/verify_glance.sh [image_name]
# Default image: c5c3/glance:31.1.0
# Requires: Docker daemon running

set -euo pipefail

IMAGE="${1:-c5c3/glance:31.1.0}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# --- Test 1: glance-manage --version outputs version and exits 0 ---
test_glance_manage_version() {
  echo "Test: glance-manage --version succeeds"
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" glance-manage --version 2>&1) || exit_code=$?

  assert_eq "glance-manage --version exits 0" "0" "$exit_code"
  assert_not_empty "version output is non-empty" "$version"
}

# --- Test 2: glance-api console script is present ---
test_glance_api_present() {
  echo "Test: glance-api console script is present"
  # The issue asks only that glance-api is present, not that it runs: the
  # eventlet launcher (2025.2's mode) needs operator-mounted runtime config to
  # start. Its mere presence proves uv's --prefix install generated the
  # setup.cfg console_scripts entry despite skipping PBR wsgi_scripts.
  local path exit_code=0
  path=$(docker run --rm "$IMAGE" sh -c 'command -v glance-api' 2>&1) || exit_code=$?

  assert_eq "command -v glance-api exits 0" "0" "$exit_code"
  assert_not_empty "glance-api resolves to a path" "$path"
}

# --- Test 3: glance is importable ---
test_glance_importable() {
  echo "Test: glance imports cleanly"
  local exit_code=0
  docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c "import glance" > /dev/null 2>&1 || exit_code=$?

  assert_eq "import glance exits 0" "0" "$exit_code"
}

# --- Test 4: the uWSGI entry script is present, executable, and parses ---
test_wsgi_shim_present() {
  echo "Test: glance-wsgi-api shim is present, executable, and parses"
  # The operator launches 2026.1+ with
  # `--wsgi-file /var/lib/openstack/bin/glance-wsgi-api` (glance's stock
  # module path ignores --pyargv, so the shim redirects config discovery to
  # the mounted --config-dir roots). Executing the shim needs operator-mounted
  # runtime config, so assert what the launch needs without running it: the
  # file exists, is executable, and is valid Python (ast.parse writes no
  # bytecode, unlike py_compile, which would fail as the openstack user).
  local exit_code=0
  docker run --rm "$IMAGE" \
    sh -c 'test -x /var/lib/openstack/bin/glance-wsgi-api' \
    > /dev/null 2>&1 || exit_code=$?
  assert_eq "glance-wsgi-api exists and is executable" "0" "$exit_code"

  local parse_exit=0
  docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import ast; ast.parse(open("/var/lib/openstack/bin/glance-wsgi-api").read())' \
    > /dev/null 2>&1 || parse_exit=$?
  assert_eq "glance-wsgi-api parses as Python" "0" "$parse_exit"
}

# --- Test 5: the S3 store driver resolved boto3 ---
test_s3_driver_and_boto3() {
  echo "Test: glance_store S3 driver resolved boto3"
  # glance_store[s3] pulls boto3. The s3 driver guards its boto3 import
  # (except ImportError: boto_session = None), so importing the module proves
  # nothing on its own — assert the guard's outcome (boto_session is not None)
  # plus a plain `import boto3` to prove the extra actually resolved.
  local boto3_exit=0
  docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c "import boto3" > /dev/null 2>&1 || boto3_exit=$?
  assert_eq "import boto3 exits 0" "0" "$boto3_exit"

  local driver_exit=0
  docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import sys; from glance_store._drivers import s3; sys.exit(0 if s3.boto_session is not None else 1)' \
    > /dev/null 2>&1 || driver_exit=$?
  assert_eq "s3 driver boto_session is not None" "0" "$driver_exit"
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

  # gcc should not be present
  local gcc_exit=0
  docker run --rm "$IMAGE" which gcc > /dev/null 2>&1 || gcc_exit=$?
  assert_nonzero_exit "gcc not found" "$gcc_exit"

  # python3-dev should not be installed
  local pydev_exit=0
  docker run --rm "$IMAGE" dpkg -s python3-dev > /dev/null 2>&1 || pydev_exit=$?
  assert_nonzero_exit "python3-dev not installed" "$pydev_exit"

  # uv should not be present in the final image
  local uv_exit=0
  docker run --rm "$IMAGE" which uv > /dev/null 2>&1 || uv_exit=$?
  assert_nonzero_exit "uv not found" "$uv_exit"
}

# --- Test 8: uwsgi is runnable (serves glance-wsgi-api at runtime) ---
test_uwsgi_runnable() {
  echo "Test: uwsgi --version succeeds"
  # Transitively proves the libpython3.12t64 apt wiring: the venv-builder
  # uwsgi binary links libpython3.12.so.1.0, which python-base does not ship.
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" /var/lib/openstack/bin/uwsgi --version 2>&1) || exit_code=$?

  assert_eq "uwsgi --version exits 0" "0" "$exit_code"
  assert_not_empty "uwsgi version output is non-empty" "$version"
}

# --- Test 9: qemu-img is runnable (image_conversion import plugin) ---
test_qemu_img_runnable() {
  echo "Test: qemu-img --version succeeds"
  # Proves the qemu-utils apt wiring: the image_conversion import plugin shells
  # out to `qemu-img info` and `qemu-img convert` on the staged image.
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" qemu-img --version 2>&1) || exit_code=$?

  assert_eq "qemu-img --version exits 0" "0" "$exit_code"
  assert_not_empty "qemu-img version output is non-empty" "$version"
}

# --- Test 10: lhafile is importable (image_decompression import plugin) ---
test_lhafile_importable() {
  echo "Test: lhafile imports cleanly"
  # Proves the lhafile pip wiring: the image_decompression import plugin imports
  # lhafile to unpack LHA archives.
  local exit_code=0
  docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c "import lhafile" > /dev/null 2>&1 || exit_code=$?

  assert_eq "import lhafile exits 0" "0" "$exit_code"
}

# --- Test 11: the installed lhafile matches a recorded pin ---
test_lhafile_version_pinned() {
  echo "Test: installed lhafile matches a recorded constraint-override pin"
  # lhafile is the one pip input of this image that upper-constraints.txt does
  # not carry, so without a pin it floats to whatever PyPI serves at build time
  # and "which version is in our image" is only answerable by opening the image.
  # The pin lives in overrides/<release>/constraints.txt, which
  # apply-constraint-overrides.sh merges into the constraints file the install
  # runs against. This asserts both halves: every release records a pin, and the
  # version that actually landed is one of them.
  local project_root
  project_root="$(cd "$SCRIPT_DIR/../.." && pwd)"

  local overrides missing=""
  local -a pins=()
  for overrides in "$project_root"/overrides/*/constraints.txt; do
    [ -f "$overrides" ] || continue
    local pin
    pin=$(sed -n 's/^lhafile===\(.*\)$/\1/p' "$overrides")
    if [ -z "$pin" ]; then
      missing="${missing} ${overrides#"$project_root"/}"
    else
      pins+=("$pin")
    fi
  done

  assert_eq "every overrides/*/constraints.txt pins lhafile" "" "$missing"

  # stderr is discarded rather than folded into the value: a DeprecationWarning
  # on import would otherwise make the version multi-line and fail the match on
  # a correctly pinned image. The exit code below is what catches a real failure.
  local installed exit_code=0
  installed=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    "import importlib.metadata as m; print(m.version('lhafile'))" 2>/dev/null) || exit_code=$?

  assert_eq "reading the installed lhafile version exits 0" "0" "$exit_code"

  # Whole-value comparison against each pin, not a substring test: a substring
  # test passes 0.3.1 against a 0.3.10 pin (every prefix matches) and passes an
  # empty value against anything, which is the opposite of what this asserts.
  local candidate matched=""
  for candidate in "${pins[@]}"; do
    if [ "$candidate" = "$installed" ]; then
      matched="yes"
    fi
  done
  assert_eq "installed lhafile version ${installed} is a recorded pin (${pins[*]})" "yes" "$matched"
}

# --- Run all tests ---
echo "=== glance container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_glance_manage_version
echo ""
test_glance_api_present
echo ""
test_glance_importable
echo ""
test_wsgi_shim_present
echo ""
test_s3_driver_and_boto3
echo ""
test_runs_as_openstack_user
echo ""
test_no_build_tools_in_final_image
echo ""
test_uwsgi_runnable
echo ""
test_qemu_img_runnable
echo ""
test_lhafile_importable
echo ""
test_lhafile_version_pinned
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
