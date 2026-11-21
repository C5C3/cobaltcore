#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify tempest container image meets requirements
# Usage: bash tests/container-images/verify_tempest.sh [image_name]
# Default image: c5c3/tempest:46.3.0
# Requires: Docker daemon running

set -euo pipefail

IMAGE="${1:-c5c3/tempest:46.3.0}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"
# shellcheck source=tests/lib/build_tools.sh
source "$SCRIPT_DIR/../lib/build_tools.sh"

# --- Test 1: tempest --version outputs version and exits 0 ---
test_tempest_version() {
  echo "Test: tempest --version succeeds"
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" tempest --version 2>&1) || exit_code=$?

  assert_eq "tempest --version exits 0" "0" "$exit_code"

  assert_not_empty "version output is non-empty" "$version"
}

# --- Test 2: keystone-tempest-plugin is importable ---
test_keystone_tempest_plugin_importable() {
  echo "Test: keystone-tempest-plugin is importable"
  local exit_code=0
  docker run --rm "$IMAGE" python3 -c 'import keystone_tempest_plugin' > /dev/null 2>&1 || exit_code=$?

  assert_eq "import keystone_tempest_plugin exits 0" "0" "$exit_code"
}

# --- Test 3: openstack CLI is available (used by the E2E verify Job) ---
test_openstack_cli_available() {
  echo "Test: openstack --version succeeds"
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" openstack --version 2>&1) || exit_code=$?

  assert_eq "openstack --version exits 0" "0" "$exit_code"

  assert_not_empty "openstack version output is non-empty" "$version"
}

# --- Test 4: subunit2junitxml is available on PATH ---
test_subunit2junitxml_available() {
  echo "Test: subunit2junitxml is available"
  local path_output exit_code=0
  # "command" is a shell builtin, so the probe runs in sh.
  path_output=$(docker run --rm "$IMAGE" sh -c 'command -v subunit2junitxml' 2>&1) || exit_code=$?

  assert_eq "command -v subunit2junitxml exits 0" "0" "$exit_code"
  assert_not_empty "subunit2junitxml path is non-empty" "$path_output"
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
  assert_no_build_tools
}

# --- Test 7: osc-placement registers its openstack CLI commands ---
test_osc_placement_cli_registered() {
  echo "Test: osc-placement registers its openstack CLI commands"
  # osc-placement is installed for the `openstack resource class list`
  # round-trip, which needs its openstack.cli.extension entry points to
  # resolve — importing the module would only prove the wheel is on sys.path.
  # --help builds the command without contacting a cloud, so no credentials
  # are needed. cliff prints "Unknown command" and can still exit 0 for an
  # unregistered command, so assert on the rendered usage line too.
  local help_output exit_code=0
  help_output=$(docker run --rm "$IMAGE" openstack resource class list --help 2>&1) || exit_code=$?

  assert_eq "openstack resource class list --help exits 0" "0" "$exit_code"
  assert_contains "resource class list is a registered command" "$help_output" \
    "usage: openstack resource class list"
}

# --- Test 8: barbican-tempest-plugin is importable ---
test_barbican_tempest_plugin_importable() {
  echo "Test: barbican-tempest-plugin is importable"
  local exit_code=0
  docker run --rm "$IMAGE" python3 -c 'import barbican_tempest_plugin' > /dev/null 2>&1 || exit_code=$?

  assert_eq "import barbican_tempest_plugin exits 0" "0" "$exit_code"
}

# --- Test 9: neutron-tempest-plugin is importable ---
test_neutron_tempest_plugin_importable() {
  echo "Test: neutron-tempest-plugin is importable"
  local exit_code=0
  docker run --rm "$IMAGE" python3 -c 'import neutron_tempest_plugin' > /dev/null 2>&1 || exit_code=$?

  assert_eq "import neutron_tempest_plugin exits 0" "0" "$exit_code"
}

# --- Test 10: cinder-tempest-plugin is importable ---
test_cinder_tempest_plugin_importable() {
  echo "Test: cinder-tempest-plugin is importable"
  local exit_code=0
  docker run --rm "$IMAGE" python3 -c 'import cinder_tempest_plugin' > /dev/null 2>&1 || exit_code=$?

  assert_eq "import cinder_tempest_plugin exits 0" "0" "$exit_code"
}

# --- Test 11: python-barbicanclient is importable ---
test_barbicanclient_importable() {
  echo "Test: python-barbicanclient is importable"
  local exit_code=0
  docker run --rm "$IMAGE" python3 -c 'import barbicanclient' > /dev/null 2>&1 || exit_code=$?

  assert_eq "import barbicanclient exits 0" "0" "$exit_code"
}

# --- Test 12: the cirros guest image of the libvirt guest suite ---
test_cirros_image_present() {
  echo "Test: the cirros 0.6.3 guest image is present with the pinned digest"
  # tests/e2e-nova-libvirt/guest-dataplane/ seeds Glance from this file. The
  # wanted digest is read from images/tempest/image.nix, and
  # tests/unit/images/tempest_cirros_pin_test.sh ties that pin to the one of
  # the kind fixture.
  local want digest exit_code=0
  want="$(sed -n 's/^ *sha256 = "\(.*\)";$/\1/p' "$SCRIPT_DIR/../../images/tempest/image.nix")"
  digest=$(docker run --rm "$IMAGE" python3 -c \
    'import hashlib; print(hashlib.sha256(open("/usr/share/cirros/cirros-0.6.3-x86_64-disk.img", "rb").read()).hexdigest())' \
    2>&1) || exit_code=$?

  assert_not_empty "images/tempest/image.nix pins a sha256" "$want"
  assert_eq "hashing the cirros image exits 0" "0" "$exit_code"
  assert_eq "the cirros image has the sha256 image.nix pins" "$want" "$digest"
}

# --- Test 13: keystone-tempest-plugin carries the cleanups of #1840 ---
test_keystone_plugin_registers_cleanups() {
  echo "Test: keystone-tempest-plugin registers the cleanups of its patch"
  # nix/lib/patches/keystone-tempest-plugin-0.22.0-register-test-cleanups.patch
  # registers cleanups at 17 places, one hunk each. For each hunk, the source
  # of the class's method, with every run of whitespace collapsed to one space,
  # must hold a text the unpatched method lacks: addCleanup( where the method
  # registers none, otherwise the client call and the object of the new
  # cleanup. Where the unpatched method already deletes another object that
  # way, the text must occur twice. The files are parsed with ast; no test
  # module is imported.
  local program output hunks n line description result exit_code=0
  hunks=$(grep -c '^@@ ' "$SCRIPT_DIR/../../nix/lib/patches/keystone-tempest-plugin-0.22.0-register-test-cleanups.patch")
  program=$(cat <<'PY'
import ast
import importlib.util
import os

HUNKS = (
    (1, "rbac/v3/test_service.py", "SystemMemberTests",
     "test_identity_delete_service", "addCleanup(", 1),
    (2, "rbac/v3/test_policy.py", "SystemMemberTests",
     "test_identity_delete_policy", "addCleanup(", 1),
    (3, "rbac/v3/test_consumer.py", "SystemMemberTests",
     "test_identity_delete_consumer", "addCleanup(", 1),
    (4, "rbac/v3/test_region.py", "SystemMemberTests",
     "test_identity_delete_region", "addCleanup(", 1),
    (5, "rbac/v3/test_role.py", "SystemMemberTests",
     "test_identity_delete_role", "addCleanup(", 1),
    (6, "rbac/v3/test_role.py", "SystemMemberTests",
     "test_identity_delete_domain_role", "addCleanup(", 1),
    (7, "rbac/v3/test_group.py", "SystemMemberTests",
     "test_identity_delete_group", "addCleanup(", 1),
    (8, "rbac/v3/test_group.py", "DomainMemberTests",
     "test_identity_delete_group", "delete_group, group1['id']", 1),
    (9, "rbac/v3/test_group.py", "ProjectManagerTests",
     "test_identity_delete_group", "delete_group, group1['id']", 1),
    (10, "rbac/v3/test_project.py", "DomainMemberTests",
     "test_identity_delete_project", "delete_project, project_id", 2),
    (11, "rbac/v3/test_user.py", "SystemAdminTests",
     "test_identity_list_users", "delete_domain", 1),
    (12, "rbac/v3/test_user.py", "SystemMemberTests",
     "test_identity_list_users", "delete_domain", 1),
    (13, "rbac/v3/test_user.py", "DomainMemberTests",
     "test_identity_delete_user", "delete_user, user['id']", 2),
    (14, "rbac/v3/test_protocol.py", "IdentityV3RbacProtocolTests",
     "setUpClass", "delete_domain", 1),
    (15, "api/identity/v3/test_identity_providers.py",
     "IndentityProvidersTest",
     "test_update_mapping_from_identity_provider_protocol",
     "delete_mapping_rule, new_mapping_id", 1),
    (16, "rbac/v3/test_domain_config.py", "SystemMemberTests",
     "test_identity_delete_domain_config", "addCleanup(", 1),
    (17, "rbac/v3/test_endpoint.py", "SystemMemberTests",
     "test_identity_delete_endpoint", "addCleanup(", 1),
)

spec = importlib.util.find_spec("keystone_tempest_plugin")
tests = os.path.join(spec.submodule_search_locations[0], "tests")
for n, rel, cls, meth, needle, want in HUNKS:
    name = f"hunk {n}: {rel} {cls}.{meth}"
    with open(os.path.join(tests, rel), encoding="utf-8") as f:
        source = f.read()
    tree = ast.parse(source)
    classes = [c for c in tree.body
               if isinstance(c, ast.ClassDef) and c.name == cls]
    if not classes:
        print(f"{n}\t{name}\tclass {cls} missing")
        continue
    methods = [m for m in classes[0].body
               if isinstance(m, ast.FunctionDef) and m.name == meth]
    if not methods:
        print(f"{n}\t{name}\tmethod {meth} missing")
        continue
    text = " ".join(ast.get_source_segment(source, methods[0]).split())
    found = text.count(needle)
    result = "ok" if found >= want else f"found {found} of {want} '{needle}'"
    print(f"{n}\t{name}\t{result}")
PY
)
  output=$(docker run --rm "$IMAGE" python3 -c "$program" 2>&1) || exit_code=$?
  if [ "$exit_code" -ne 0 ]; then
    echo "  python3 exited $exit_code:"
    while IFS= read -r line; do
      echo "    $line"
    done <<<"$output"
  fi

  # One assertion per hunk of the patch; a hunk without a result line (python3
  # stopped before it, or HUNKS lacks it) fails too, so a crash cannot pass with
  # fewer assertions. A HUNKS entry beyond the patch's hunks fails the count.
  assert_eq "HUNKS has one entry per hunk of the patch" "$hunks" \
    "$(grep -c '^[0-9][0-9]*	' <<<"$output" || true)"
  for n in $(seq 1 "$hunks"); do
    line=$(grep -m 1 "^${n}	" <<<"$output" || true)
    if [ -z "$line" ]; then
      assert_eq "hunk $n: the check printed a result" "ok" "no result"
      continue
    fi
    IFS=$'\t' read -r _ description result <<<"$line"
    assert_eq "$description" "ok" "$result"
  done
}

# --- Run all tests ---
echo "=== tempest container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_tempest_version
echo ""
test_keystone_tempest_plugin_importable
echo ""
test_openstack_cli_available
echo ""
test_subunit2junitxml_available
echo ""
test_runs_as_openstack_user
echo ""
test_no_build_tools_in_final_image
echo ""
test_osc_placement_cli_registered
echo ""
test_barbican_tempest_plugin_importable
echo ""
test_neutron_tempest_plugin_importable
echo ""
test_barbicanclient_importable
echo ""
test_cinder_tempest_plugin_importable
echo ""
test_cirros_image_present
echo ""
test_keystone_plugin_registers_cleanups
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
