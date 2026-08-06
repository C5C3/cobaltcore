#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify barbican container image meets requirements
# Usage: bash tests/container-images/verify_barbican.sh [image_name]
# Default image: c5c3/barbican:21.0.0
# Requires: Docker daemon running

set -euo pipefail

IMAGE="${1:-c5c3/barbican:21.0.0}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

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

# --- Test 8: uwsgi is runnable (serves barbican-api at runtime) ---
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
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
