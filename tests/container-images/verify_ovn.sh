#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify ovn container image meets requirements
# Usage: bash tests/container-images/verify_ovn.sh [image_name]
# Default image: ovn
# Requires: Docker daemon running
# The expected OVN version comes from hack/ci-resolve-ovn-version.sh, which
# reads the single pin in images/ovn/Dockerfile. Set OVN_DOCKERFILE to point
# that resolver at a different Dockerfile.

set -euo pipefail

IMAGE="${1:-ovn}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# The resolver is the only parser of the 'ARG OVN_VERSION=' pin, so this
# script never reads the Dockerfile itself.
EXPECTED=$("$SCRIPT_DIR/../../hack/ci-resolve-ovn-version.sh")

# --- Test 1: the OVN binaries report the pinned version ---
test_ovn_versions() {
  echo "Test: OVN binaries report version $EXPECTED"
  local output first_line client exit_code=0

  output=$(docker run --rm "$IMAGE" ovn-northd --version 2>&1) || exit_code=$?
  first_line="${output%%$'\n'*}"
  assert_eq "ovn-northd --version exits 0" "0" "$exit_code"
  assert_eq "ovn-northd reports the pinned version" "ovn-northd $EXPECTED" "$first_line"

  exit_code=0
  output=$(docker run --rm "$IMAGE" ovn-controller --version 2>&1) || exit_code=$?
  first_line="${output%%$'\n'*}"
  assert_eq "ovn-controller --version exits 0" "0" "$exit_code"
  assert_eq "ovn-controller reports the pinned version" "ovn-controller $EXPECTED" "$first_line"

  # The ctl clients print the OVSDB schema version after their own version,
  # so the version line is matched inside the whole output.
  for client in ovn-nbctl ovn-sbctl; do
    exit_code=0
    output=$(docker run --rm "$IMAGE" "$client" --version 2>&1) || exit_code=$?
    assert_eq "$client --version exits 0" "0" "$exit_code"
    assert_contains "$client output names version $EXPECTED" "$output" "$EXPECTED"
  done
}

# --- Test 2: the OVS daemons and the client tools run ---
test_ovs_daemons_and_clients() {
  echo "Test: OVS daemons and client tools run"
  local output first_line prog tool matched re exit_code

  for prog in ovs-vswitchd ovsdb-server; do
    exit_code=0
    output=$(docker run --rm "$IMAGE" "$prog" --version 2>&1) || exit_code=$?
    first_line="${output%%$'\n'*}"
    assert_eq "$prog --version exits 0" "0" "$exit_code"
    assert_starts_with "$prog names Open vSwitch" "$first_line" "$prog (Open vSwitch) "

    # OVS carries its own version, independent of the OVN pin, so the check
    # is on the shape of the version instead of a literal value.
    re="^$prog \(Open vSwitch\) [0-9]+\.[0-9]+\.[0-9]+"
    matched=no
    if [[ "$first_line" =~ $re ]]; then
      matched=yes
    fi
    assert_eq "$prog reports an X.Y.Z version" "yes" "$matched"
  done

  for tool in ovs-vsctl ovsdb-client ovsdb-tool ovs-appctl ovn-appctl ovs-ofctl; do
    exit_code=0
    docker run --rm "$IMAGE" sh -c "command -v $tool" > /dev/null 2>&1 || exit_code=$?
    assert_eq "$tool resolves on PATH" "0" "$exit_code"

    exit_code=0
    docker run --rm "$IMAGE" "$tool" --version > /dev/null 2>&1 || exit_code=$?
    assert_eq "$tool --version exits 0" "0" "$exit_code"
  done
}

# --- Test 3: the shipped OVS is the revision OVN links against ---
test_ovs_pin_consistency() {
  echo "Test: shipped OVS matches the OVS revision OVN links against"
  # OVN builds against the OVS tree its 'ovs' submodule points to and prints
  # that revision on its 'Open vSwitch Library' line. ovs-vswitchd prints the
  # version of the daemon binary the image ships. Equal values prove both come
  # from the same OVS revision, which is what decision D1 of #898 buys with a
  # single pin. Each grep is scoped to one line, so no other number in the
  # output can be picked up by accident.
  local output linked shipped exit_code=0

  output=$(docker run --rm "$IMAGE" ovn-northd --version 2>&1) || exit_code=$?
  assert_eq "ovn-northd --version exits 0" "0" "$exit_code"
  linked=$(grep 'Open vSwitch Library' <<< "$output" \
    | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1 || true)

  exit_code=0
  output=$(docker run --rm "$IMAGE" ovs-vswitchd --version 2>&1) || exit_code=$?
  assert_eq "ovs-vswitchd --version exits 0" "0" "$exit_code"
  shipped=$(grep -oE '[0-9]+\.[0-9]+\.[0-9]+' <<< "${output%%$'\n'*}" | head -1 || true)

  assert_not_empty "ovn-northd names the OVS library version it links against" "$linked"
  assert_not_empty "ovs-vswitchd names its version" "$shipped"
  assert_eq "shipped OVS matches the OVS revision OVN links against" "$linked" "$shipped"
}

# --- Test 4: the ovn-ctl and ovs-ctl helper scripts run ---
test_ctl_scripts() {
  echo "Test: ovn-ctl and ovs-ctl are executable and run"
  local output exit_code=0

  docker run --rm "$IMAGE" test -x /usr/share/ovn/scripts/ovn-ctl || exit_code=$?
  assert_eq "ovn-ctl is executable" "0" "$exit_code"

  exit_code=0
  docker run --rm "$IMAGE" test -x /usr/share/openvswitch/scripts/ovs-ctl || exit_code=$?
  assert_eq "ovs-ctl is executable" "0" "$exit_code"

  exit_code=0
  output=$(docker run --rm "$IMAGE" /usr/share/openvswitch/scripts/ovs-ctl --help 2>&1) || exit_code=$?
  assert_eq "ovs-ctl --help exits 0" "0" "$exit_code"
  assert_not_empty "ovs-ctl --help prints its usage" "$output"

  # ovn-ctl prints the same usage text for --help, but its usage() returns
  # instead of exiting, so the script runs on and exits 1 with "missing
  # command name". The 'help' command is the zero-exit way to prove the
  # script and the ovn-lib file it sources run under the image's shell as the
  # non-root user.
  exit_code=0
  output=$(docker run --rm "$IMAGE" /usr/share/ovn/scripts/ovn-ctl help 2>&1) || exit_code=$?
  assert_eq "ovn-ctl help exits 0" "0" "$exit_code"
  assert_not_empty "ovn-ctl help prints its usage" "$output"
}

# --- Test 5: the daemons resolve every shared library they need ---
test_linked_libraries() {
  echo "Test: daemon binaries resolve their shared libraries"
  local output prog exit_code

  for prog in ovs-vswitchd ovsdb-server ovn-northd ovn-controller; do
    exit_code=0
    output=$(docker run --rm "$IMAGE" sh -c "ldd \"\$(command -v $prog)\"" 2>&1) || exit_code=$?
    assert_eq "ldd $prog exits 0" "0" "$exit_code"
    assert_not_contains "$prog has no unresolved libraries" "$output" "not found"

    # The OVSDB connections between the daemons speak TLS, which needs the
    # OpenSSL 3 runtime linked into the server side.
    if [ "$prog" = "ovsdb-server" ] || [ "$prog" = "ovn-northd" ]; then
      assert_contains "$prog links against libssl.so.3" "$output" "libssl.so.3"
    fi
  done
}

# --- Test 6: the runtime image carries no build toolchain ---
test_no_toolchain() {
  echo "Test: build toolchain and development headers are absent"
  local tool exit_code

  for tool in gcc cc make git autoconf python3; do
    exit_code=0
    docker run --rm "$IMAGE" sh -c "command -v $tool" > /dev/null 2>&1 || exit_code=$?
    assert_nonzero_exit "$tool not found" "$exit_code"
  done

  exit_code=0
  docker run --rm "$IMAGE" dpkg -s build-essential > /dev/null 2>&1 || exit_code=$?
  assert_nonzero_exit "build-essential not installed" "$exit_code"

  exit_code=0
  docker run --rm "$IMAGE" test -d /usr/include/openvswitch || exit_code=$?
  assert_nonzero_exit "OVS development headers not shipped" "$exit_code"
}

# --- Test 7: runs as openstack user with writable state directories ---
test_user_posture() {
  echo "Test: container runs as openstack user with writable state directories"
  local output dir exit_code=0

  output=$(docker run --rm "$IMAGE" id 2>&1) || exit_code=$?
  assert_eq "id exits 0" "0" "$exit_code"
  assert_contains "id reports uid 42424 (openstack)" "$output" "uid=42424(openstack)"

  for dir in /var/lib/ovn /var/run/ovn /var/lib/openvswitch /var/run/openvswitch \
    /etc/ovn /etc/openvswitch; do
    exit_code=0
    docker run --rm "$IMAGE" test -w "$dir" || exit_code=$?
    assert_eq "$dir is writable by the container user" "0" "$exit_code"
  done
}

# --- Test 8: the helpers the chassis DaemonSets and datapath probes call ---
test_chassis_helpers() {
  echo "Test: modprobe, ip and ping are available"
  # The chassis DaemonSets of #903 load host kernel modules with modprobe
  # (kmod) and inspect interfaces with ip (iproute2) from inside this image.
  # The datapath probes of the OVN overlay and southbound-outage e2e suites
  # ping across the Geneve tunnel with ping (iputils-ping).
  local tool exit_code

  for tool in modprobe ip ping; do
    exit_code=0
    docker run --rm "$IMAGE" sh -c "command -v $tool" > /dev/null 2>&1 || exit_code=$?
    assert_eq "$tool resolves on PATH" "0" "$exit_code"
  done
}

# --- Test 9: the OVSDB schemas ship and a database can be created from them ---
test_ovsdb_schemas() {
  echo "Test: OVSDB schemas ship and ovsdb-tool creates databases from them"
  # The schemas ride along in the same two COPY --from=build directives as the
  # ctl scripts. Nothing else here reads them: --version, command -v and ldd all
  # stay green when an upstream `make install` layout change drops
  # ovn-nb.ovsschema, and northd then dies at startup on the first pod. Creating
  # a database is the check that proves the file is both present and parseable.
  local schema exit_code

  for schema in /usr/share/ovn/ovn-nb.ovsschema /usr/share/ovn/ovn-sb.ovsschema \
    /usr/share/openvswitch/vswitch.ovsschema; do
    exit_code=0
    docker run --rm "$IMAGE" test -f "$schema" || exit_code=$?
    assert_eq "$schema ships in the runtime image" "0" "$exit_code"

    exit_code=0
    docker run --rm "$IMAGE" sh -c "ovsdb-tool create /tmp/test.db $schema" \
      > /dev/null 2>&1 || exit_code=$?
    assert_eq "ovsdb-tool creates a database from $schema" "0" "$exit_code"
  done
}

# --- Run all tests ---
echo "=== ovn container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_ovn_versions
echo ""
test_ovs_daemons_and_clients
echo ""
test_ovs_pin_consistency
echo ""
test_ctl_scripts
echo ""
test_linked_libraries
echo ""
test_no_toolchain
echo ""
test_user_posture
echo ""
test_chassis_helpers
echo ""
test_ovsdb_schemas
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
