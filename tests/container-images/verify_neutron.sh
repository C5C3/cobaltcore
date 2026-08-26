#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify neutron container image meets requirements
# Usage: bash tests/container-images/verify_neutron.sh [image_name]
# Default image: c5c3/neutron:27.0.3
# Requires: Docker daemon running

set -euo pipefail

IMAGE="${1:-c5c3/neutron:27.0.3}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# --- Test 1: neutron-db-manage console script runs ---
test_neutron_db_manage_help() {
  echo "Test: neutron-db-manage --help succeeds"
  # Proves the alembic console script uv generated is runnable. Plain --help
  # reads no configuration file and opens no database connection.
  local help_output exit_code=0
  help_output=$(docker run --rm "$IMAGE" neutron-db-manage --help 2>&1) || exit_code=$?

  assert_eq "neutron-db-manage --help exits 0" "0" "$exit_code"
  assert_not_empty "help output is non-empty" "$help_output"
}

# --- Test 2: neutron-status console script runs ---
test_neutron_status_help() {
  echo "Test: neutron-status --help succeeds"
  # Plain --help needs no config and no database, so it proves a second
  # console script uv generated is runnable.
  local help_output exit_code=0
  help_output=$(docker run --rm "$IMAGE" neutron-status --help 2>&1) || exit_code=$?

  assert_eq "neutron-status --help exits 0" "0" "$exit_code"
  assert_not_empty "help output is non-empty" "$help_output"
}

# --- Test 3: neutron is importable ---
test_neutron_importable() {
  echo "Test: neutron imports cleanly"
  local exit_code=0
  docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c "import neutron" > /dev/null 2>&1 || exit_code=$?

  assert_eq "import neutron exits 0" "0" "$exit_code"
}

# --- Test 4: the OVSDB client libraries are importable ---
test_ovsdb_client_libraries_importable() {
  echo "Test: ovs and ovsdbapp import cleanly"
  # The pinned OVSDB client libraries the ML2/OVN mechanism driver and the
  # OVN maintenance worker talk to the OVN databases with. They arrive as
  # transitive requirements, so nothing else in this image would notice if a
  # constraint bump dropped one of them.
  local exit_code=0
  docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c "import ovs, ovsdbapp" > /dev/null 2>&1 || exit_code=$?

  assert_eq "import ovs, ovsdbapp exits 0" "0" "$exit_code"
}

# --- Test 5: the uWSGI module path and its application symbol resolve ---
test_wsgi_module_resolvable() {
  echo "Test: neutron.wsgi.api resolves and binds application"
  # The neutron-operator launches uWSGI with --module neutron.wsgi.api, so
  # the module path and the application symbol both have to exist in the
  # image. Resolve the module spec instead of importing it: importing
  # neutron.wsgi.api executes server.boot_server(api.api_server) at module
  # level (spike #901 (b.2)), which reads the configuration the operator
  # mounts and the bare image does not carry. find_spec answers half the
  # question this test asks, whether the module path handed to uWSGI
  # resolves, without running module-level code, so do not simplify it into
  # a plain import.
  # ast.parse over the module source answers the other half without
  # executing it either. Both pinned tags open with an `application = None`
  # sentinel and rebind it only inside `with lock:` and `if application is
  # None:`, so the direct children of the module node are not enough: the
  # walk has to reach into nested statement bodies (`with`, `if`, `try`),
  # and a tree whose only binding is that sentinel has to be rejected. It
  # stops at every scope boundary though (FunctionDef, AsyncFunctionDef,
  # ClassDef, Lambda), because uWSGI looks `application` up as a module
  # global and a binding in a nested scope is a different symbol. Were a
  # partial upstream restructure to move the lock block into a helper and
  # leave the sentinel behind, counting that helper's assignment would ship
  # an image where uWSGI binds `application` to None and every request to
  # the neutron API fails.
  # Stderr is captured rather than discarded, so an unresolvable module and
  # a renamed symbol do not collapse into the same bare exit code.
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import ast, importlib.util, sys
spec = importlib.util.find_spec("neutron.wsgi.api")
if spec is None or spec.origin is None:
    sys.exit("neutron.wsgi.api does not resolve to a module file")
tree = ast.parse(open(spec.origin).read())
nested = {id(c) for n in ast.walk(tree)
          if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef, ast.Lambda))
          for c in ast.walk(n) if c is not n}
bindings = [n for n in ast.walk(tree)
            if isinstance(n, ast.Assign) and id(n) not in nested
            for t in n.targets if isinstance(t, ast.Name) and t.id == "application"]
if not bindings:
    sys.exit("neutron.wsgi.api binds no module-level application")
if all(isinstance(b.value, ast.Constant) and b.value.value is None for b in bindings):
    sys.exit("neutron.wsgi.api only binds application = None at module level")' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "neutron.wsgi.api resolves and binds application" "0" "$exit_code"
}

# --- Test 6: the shipped paste configuration is present ---
test_api_paste_shipped() {
  echo "Test: api-paste.ini ships in the image"
  # The package data file #904 relies on. The operator either points
  # [DEFAULT] api_paste_config at this path or mounts the file to
  # /etc/neutron/api-paste.ini, and both routes need it inside the image.
  local exit_code=0
  docker run --rm "$IMAGE" \
    test -f /var/lib/openstack/etc/neutron/api-paste.ini || exit_code=$?

  assert_eq "api-paste.ini is present" "0" "$exit_code"
}

# --- Test 7: the companion console scripts run ---
test_companion_scripts_runnable() {
  echo "Test: companion console scripts run"
  # The process inventory of Meta #898 (D7, D11, D12). Each of these runs as
  # its own process from its own console script instead of through the uWSGI
  # module path of test 5, so a script uv failed to generate stays invisible
  # to every other check here. --help is what makes the check reach past the
  # wrapper: oslo.config answers it inside argparse and exits 0 before any
  # configuration file is read, so the script has to import its entry-point
  # target first. A wrapper uv still generates after a constraint bump drops
  # a transitive import dies with ModuleNotFoundError here instead of at
  # container start. Stderr is echoed on failure so the missing module is
  # named rather than collapsed into a bare exit code.
  local script name err exit_code

  for script in /var/lib/openstack/bin/neutron-periodic-workers \
    /var/lib/openstack/bin/neutron-ovn-maintenance-worker \
    /var/lib/openstack/bin/neutron-ovn-metadata-agent \
    /var/lib/openstack/bin/neutron-ovn-db-sync-util; do
    exit_code=0
    err=$(docker run --rm "$IMAGE" "$script" --help 2>&1 >/dev/null) || exit_code=$?
    [ "$exit_code" -eq 0 ] || echo "    $err"
    name="$(basename "$script")"

    assert_eq "$name --help exits 0" "0" "$exit_code"
  done
}

# --- Test 8: the ovsdb-client binary is callable ---
test_ovsdb_client_binary() {
  echo "Test: ovsdb-client --version succeeds"
  # Proves the openvswitch-common apt wiring. neutron's pre_fork_initialize
  # shells out to `ovsdb-client transact` and dies with "[Errno 2] No such
  # file or directory: 'ovsdb-client'" when the binary is absent, which is a
  # startup failure of every API worker rather than a degraded feature.
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" ovsdb-client --version 2>&1) || exit_code=$?

  assert_eq "ovsdb-client --version exits 0" "0" "$exit_code"
  assert_contains "ovsdb-client names Open vSwitch" "$version" "Open vSwitch"
}

# --- Test 9: the metadata agent's helper binaries are callable ---
test_metadata_agent_binaries() {
  echo "Test: haproxy and ip run"
  # Proves the haproxy and iproute2 apt wiring. The
  # neutron-ovn-metadata-agent spawns `haproxy -f <cfg>` per network through
  # `ip netns exec`, so both binaries have to resolve on PATH inside this
  # image. Neither is pulled in by the virtualenv.
  local output exit_code=0
  output=$(docker run --rm "$IMAGE" haproxy -v 2>&1) || exit_code=$?
  assert_eq "haproxy -v exits 0" "0" "$exit_code"
  assert_not_empty "haproxy version output is non-empty" "$output"

  exit_code=0
  output=$(docker run --rm "$IMAGE" ip -V 2>&1) || exit_code=$?
  assert_eq "ip -V exits 0" "0" "$exit_code"
  assert_not_empty "ip version output is non-empty" "$output"
}

# --- Test 10: runs as openstack user ---
test_runs_as_openstack_user() {
  echo "Test: container runs as openstack user"
  local whoami_output exit_code=0
  whoami_output=$(docker run --rm "$IMAGE" whoami 2>&1) || exit_code=$?

  assert_eq "whoami exits 0" "0" "$exit_code"
  assert_eq "whoami outputs openstack" "openstack" "$whoami_output"
}

# --- Test 11: no build tools in final image ---
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

# --- Test 12: uwsgi is runnable (serves the neutron API at runtime) ---
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
echo "=== neutron container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_neutron_db_manage_help
echo ""
test_neutron_status_help
echo ""
test_neutron_importable
echo ""
test_ovsdb_client_libraries_importable
echo ""
test_wsgi_module_resolvable
echo ""
test_api_paste_shipped
echo ""
test_companion_scripts_runnable
echo ""
test_ovsdb_client_binary
echo ""
test_metadata_agent_binaries
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
