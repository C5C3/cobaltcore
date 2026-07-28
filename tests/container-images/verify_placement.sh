#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify placement container image meets requirements
# Usage: bash tests/container-images/verify_placement.sh [image_name]
# Default image: c5c3/placement:14.0.0
# Requires: Docker daemon running

set -euo pipefail

IMAGE="${1:-c5c3/placement:14.0.0}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# --- Test 1: placement-manage --version outputs version and exits 0 ---
test_placement_manage_version() {
  echo "Test: placement-manage --version succeeds"
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" placement-manage --version 2>&1) || exit_code=$?

  assert_eq "placement-manage --version exits 0" "0" "$exit_code"
  assert_not_empty "version output is non-empty" "$version"
}

# --- Test 2: placement-status console script runs ---
test_placement_status_help() {
  echo "Test: placement-status --help succeeds"
  # Plain --help needs no config and no database, so it proves the console
  # script uv generated is runnable.
  local help_output exit_code=0
  help_output=$(docker run --rm "$IMAGE" placement-status --help 2>&1) || exit_code=$?

  assert_eq "placement-status --help exits 0" "0" "$exit_code"
  assert_not_empty "help output is non-empty" "$help_output"
}

# --- Test 3: placement is importable ---
test_placement_importable() {
  echo "Test: placement imports cleanly"
  local exit_code=0
  docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c "import placement" > /dev/null 2>&1 || exit_code=$?

  assert_eq "import placement exits 0" "0" "$exit_code"
}

# --- Test 4: the uWSGI entry script is present, executable, and loadable ---
test_wsgi_entry_present() {
  echo "Test: placement-api entry is present, executable, and loadable"
  # The placement-operator will launch uWSGI with
  # `--wsgi-file /var/lib/openstack/bin/placement-api`. Executing the entry
  # needs operator-mounted runtime config, so assert what the launch needs
  # without running it: the file exists, is executable, and is valid Python
  # (ast.parse writes no bytecode, unlike py_compile, which would fail as the
  # openstack user).
  local exit_code=0
  docker run --rm "$IMAGE" \
    sh -c 'test -x /var/lib/openstack/bin/placement-api' \
    > /dev/null 2>&1 || exit_code=$?
  assert_eq "placement-api exists and is executable" "0" "$exit_code"

  local parse_exit=0
  docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import ast; ast.parse(open("/var/lib/openstack/bin/placement-api").read())' \
    > /dev/null 2>&1 || parse_exit=$?
  assert_eq "placement-api parses as Python" "0" "$parse_exit"

  # ast.parse only proves the entry is grammatical. The entry is hand-written
  # because upstream ships no WSGI script for either release, so its import
  # target is an assumption against two independently-moving trees — resolve
  # it here. Test 3 imports the package, not this submodule and symbol.
  # Execute the entry's own import statements rather than a re-typed copy: a
  # typo in the Dockerfile's printf parses fine and would only surface when
  # uWSGI loads the file at pod start. The remaining statement calls
  # init_application(), which needs the operator-mounted config, so it stays
  # out. The assert turns an entry that lost its imports into a failure
  # instead of an empty, trivially-passing exec.
  # The exec alone proves only whatever the entry imports today, so the
  # trailing explicit import stays as the independent oracle: an entry
  # rewritten as `from placement import wsgi` execs cleanly even after
  # init_application is renamed away, and uWSGI is what calls that symbol via
  # --wsgi-file. Stderr is captured rather than discarded — file missing, no
  # imports, module gone and symbol gone otherwise collapse into a bare exit
  # code that has to be reproduced by hand to tell apart.
  local import_exit=0 import_err=""
  import_err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import ast
src = open("/var/lib/openstack/bin/placement-api").read()
nodes = [n for n in ast.parse(src).body if isinstance(n, (ast.Import, ast.ImportFrom))]
assert nodes, "placement-api declares no imports"
exec(compile(ast.Module(body=nodes, type_ignores=[]), "placement-api", "exec"))
from placement.wsgi import init_application' \
    2>&1 > /dev/null) || import_exit=$?
  [ "$import_exit" -eq 0 ] || echo "    $import_err"
  assert_eq "placement-api imports and init_application resolve" "0" "$import_exit"
}

# --- Test 5: runs as openstack user ---
test_runs_as_openstack_user() {
  echo "Test: container runs as openstack user"
  local whoami_output exit_code=0
  whoami_output=$(docker run --rm "$IMAGE" whoami 2>&1) || exit_code=$?

  assert_eq "whoami exits 0" "0" "$exit_code"
  assert_eq "whoami outputs openstack" "openstack" "$whoami_output"
}

# --- Test 6: no build tools in final image ---
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

# --- Test 7: uwsgi is runnable (serves placement-api at runtime) ---
test_uwsgi_runnable() {
  echo "Test: uwsgi --version succeeds"
  # Transitively proves the libpython3.12t64 apt wiring: the venv-builder
  # uwsgi binary links libpython3.12.so.1.0, which python-base does not ship.
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" /var/lib/openstack/bin/uwsgi --version 2>&1) || exit_code=$?

  assert_eq "uwsgi --version exits 0" "0" "$exit_code"
  assert_not_empty "uwsgi version output is non-empty" "$version"
}

# --- Run all tests ---
echo "=== placement container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_placement_manage_version
echo ""
test_placement_status_help
echo ""
test_placement_importable
echo ""
test_wsgi_entry_present
echo ""
test_runs_as_openstack_user
echo ""
test_no_build_tools_in_final_image
echo ""
test_uwsgi_runnable
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
