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

# --- Test 6: the system-grant cache patch is applied ---
test_system_grant_patch_applied() {
  echo "Test: a system grant invalidates the computed assignment cache"
  # Proves that the image carries the three invalidations of
  # patches/keystone/2026.1/0003-invalidate-assignment-cache-on-modify-operations.patch
  # (#732): both system grants and the domain-deletion callback. Keystone
  # 30.0.0 (2026.2) carries the upstream change natively, so both release
  # images pass. Without it, a system grant leaves the cached
  # list_role_assignments result in place, and K-ORC's read-back of a new
  # system-scoped RoleAssignment stays empty until [cache] expiration_time;
  # a deleted domain's assignments stay listed just as long. The driver, the
  # role API and the cache region are mocked, so neither a database nor a
  # configuration file is needed.
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import sys
from unittest import mock
from keystone.assignment import core

manager = object.__new__(core.Manager)
manager.driver = mock.Mock()
region = mock.Mock()
with mock.patch.object(core, "PROVIDERS") as providers, \
        mock.patch.object(core, "COMPUTED_ASSIGNMENTS_REGION", region):
    providers.role_api.get_role.return_value = {"id": "r", "domain_id": None}
    manager.create_system_grant_for_user("u", "r")
    if region.invalidate.call_count != 1:
        sys.exit("create_system_grant_for_user did not invalidate "
                 "COMPUTED_ASSIGNMENTS_REGION")
    manager.create_system_grant_for_group("g", "r")
    if region.invalidate.call_count != 2:
        sys.exit("create_system_grant_for_group did not invalidate "
                 "COMPUTED_ASSIGNMENTS_REGION")
    manager._delete_domain_assignments(None, None, None, {"resource_info": "d"})
    if region.invalidate.call_count != 3:
        sys.exit("the domain-deletion callback did not invalidate "
                 "COMPUTED_ASSIGNMENTS_REGION")' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "a system grant invalidates the computed assignment cache" "0" "$exit_code"
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
test_system_grant_patch_applied
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
