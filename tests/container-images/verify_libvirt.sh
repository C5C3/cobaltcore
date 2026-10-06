#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify libvirt container image meets requirements
# Usage: bash tests/container-images/verify_libvirt.sh [image_name]
# Default image: libvirt
# Requires: Docker daemon running
#
# Every check runs without --privileged, as the pull-request job does: the
# daemon smoke test starts libvirtd and queries the QEMU driver, which needs
# neither /dev/kvm nor host access.

set -euo pipefail

IMAGE="${1:-libvirt}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# assert_matches <description> <actual> <regex>
# tests/lib/assertions.sh has no regular-expression assertion.
assert_matches() {
  local description="$1" actual="$2" regex="$3"
  if [[ "$actual" =~ $regex ]]; then
    echo "  PASS: $description"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $description"
    echo "    expected to match: $regex"
    echo "    actual: $actual"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 1: libvirtd reports libvirt 10 ---
test_libvirtd_version() {
  echo "Test: libvirtd --version reports libvirt 10"
  local version first_line exit_code=0
  version=$(docker run --rm "$IMAGE" libvirtd --version 2>&1) || exit_code=$?
  first_line=$(printf '%s\n' "$version" | head -n 1)

  assert_eq "libvirtd --version exits 0" "0" "$exit_code"
  # The major version is pinned on purpose: images/nova-compute/Dockerfile
  # builds libvirt-python against noble's libvirt 10 and links its libvirt0, so
  # client and daemon move together or not at all.
  assert_matches "first line reports libvirt 10, the libvirt0 images/nova-compute/Dockerfile links" \
    "$first_line" '^libvirtd \(libvirt\) 10\.'
}

# --- Test 2: QEMU and qemu-img report a version ---
test_qemu_versions() {
  echo "Test: qemu-system-x86_64 and qemu-img report a version"
  local version first_line exit_code=0
  version=$(docker run --rm "$IMAGE" qemu-system-x86_64 --version 2>&1) || exit_code=$?
  first_line=$(printf '%s\n' "$version" | head -n 1)

  assert_eq "qemu-system-x86_64 --version exits 0" "0" "$exit_code"
  assert_matches "first line reports a QEMU version" \
    "$first_line" '^QEMU emulator version [0-9]+\.[0-9]+'

  exit_code=0
  version=$(docker run --rm "$IMAGE" qemu-img --version 2>&1) || exit_code=$?
  first_line=$(printf '%s\n' "$version" | head -n 1)

  assert_eq "qemu-img --version exits 0" "0" "$exit_code"
  assert_matches "first line reports a qemu-img version" \
    "$first_line" '^qemu-img version [0-9]+\.[0-9]+'
}

# --- Test 3: the tools the consumer calls are on PATH ---
test_tools_on_path() {
  echo "Test: the libvirt, module and host tools are on PATH"
  local tool exit_code
  for tool in virsh virt-admin virtlogd modprobe systemd-run dmidecode ip; do
    exit_code=0
    # Wrapped in a shell: "command" is a shell builtin and the image declares no
    # ENTRYPOINT, so "docker run <image> command -v virsh" execs a binary that
    # does not exist and exits non-zero whatever is installed.
    docker run --rm "$IMAGE" sh -c "command -v $tool" > /dev/null 2>&1 || exit_code=$?
    assert_eq "$tool is on PATH" "0" "$exit_code"
  done
}

# --- Test 4: the OVMF UEFI firmware is installed ---
test_ovmf_firmware() {
  echo "Test: the OVMF code and vars files are present"
  local file exit_code
  for file in /usr/share/OVMF/OVMF_CODE_4M.fd /usr/share/OVMF/OVMF_VARS_4M.fd; do
    exit_code=0
    docker run --rm "$IMAGE" test -f "$file" > /dev/null 2>&1 || exit_code=$?
    assert_eq "$file exists" "0" "$exit_code"
  done
}

# --- Test 5: runs as root (libvirtd has to) ---
test_runs_as_root() {
  echo "Test: container runs as root"
  local uid exit_code=0
  uid=$(docker run --rm "$IMAGE" id -u 2>&1) || exit_code=$?

  assert_eq "id -u exits 0" "0" "$exit_code"
  assert_eq "id -u prints 0" "0" "$uid"
}

# --- Test 6: the guest accounts the packages create ---
test_guest_accounts() {
  echo "Test: the libvirt-qemu user and the kvm and libvirt groups exist"
  local uid group exit_code=0
  # The DEVIATION comment in the Dockerfile leans on these accounts instead of
  # an openstack user, and the packaged qemu.conf defaults run guests as
  # libvirt-qemu with the group kvm.
  uid=$(docker run --rm "$IMAGE" id -u libvirt-qemu 2>&1) || exit_code=$?
  assert_eq "id -u libvirt-qemu exits 0" "0" "$exit_code"
  assert_eq "libvirt-qemu has UID 64055" "64055" "$uid"

  for group in kvm libvirt; do
    exit_code=0
    docker run --rm "$IMAGE" getent group "$group" > /dev/null 2>&1 || exit_code=$?
    assert_eq "group $group exists" "0" "$exit_code"
  done
}

# --- Test 7: the 'default' NAT network is not autostarted ---
test_default_network_not_autostarted() {
  echo "Test: no libvirt network is autostarted"
  local listing exit_code=0
  listing=$(docker run --rm "$IMAGE" sh -c 'ls -A /etc/libvirt/qemu/networks/autostart' 2>&1) || exit_code=$?

  assert_eq "the autostart directory exists" "0" "$exit_code"
  # libvirtd runs in the host's network namespace; an autostarted 'default'
  # network would create virbr0 and NAT rules on the node.
  assert_eq "the autostart directory is empty" "" "$listing"
}

# --- Test 8: no build tooling in the runtime image ---
test_no_build_tooling() {
  echo "Test: build tooling is absent"
  local tool exit_code
  for tool in gcc git python3; do
    exit_code=0
    # Wrapped in a shell for the reason given in test_tools_on_path: without it
    # the assertion would pass for every tool, always.
    docker run --rm "$IMAGE" sh -c "command -v $tool" > /dev/null 2>&1 || exit_code=$?
    assert_nonzero_exit "$tool is not installed" "$exit_code"
  done
}

# --- Test 9: libvirtd starts and serves the QEMU driver ---
test_daemon_smoke() {
  echo "Test: libvirtd starts and serves the QEMU driver"
  local output exit_code=0
  # One container: start the daemon, wait at most 30 seconds for its socket,
  # then ask the QEMU driver for its version and its domain capabilities. A
  # missing QEMU driver makes virsh fail to connect to qemu:///system.
  # shellcheck disable=SC2016 # expanded by the container's shell
  output=$(docker run --rm "$IMAGE" sh -c '
    libvirtd -d || { echo "libvirtd -d exited non-zero"; exit 1; }
    waited=0
    while [ ! -S /run/libvirt/libvirt-sock ]; do
      if [ "$waited" -ge 30 ]; then
        echo "libvirtd did not open /run/libvirt/libvirt-sock within 30s"
        exit 1
      fi
      sleep 1
      waited=$((waited + 1))
    done
    virsh -c qemu:///system version &&
      virsh -c qemu:///system domcapabilities --virttype qemu --arch x86_64
  ' 2>&1) || exit_code=$?

  assert_eq "the daemon smoke test exits 0" "0" "$exit_code"
  assert_contains "virsh reports the QEMU hypervisor" "$output" "Running hypervisor: QEMU"
  assert_contains "domcapabilities names the x86_64 emulator" \
    "$output" "<path>/usr/bin/qemu-system-x86_64</path>"
}

# --- Run all tests ---
echo "=== libvirt container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_libvirtd_version
echo ""
test_qemu_versions
echo ""
test_tools_on_path
echo ""
test_ovmf_firmware
echo ""
test_runs_as_root
echo ""
test_guest_accounts
echo ""
test_default_network_not_autostarted
echo ""
test_no_build_tooling
echo ""
test_daemon_smoke
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
