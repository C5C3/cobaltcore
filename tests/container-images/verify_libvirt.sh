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

# --- Test 10: QEMU loads the rbd block driver and no network one ---
test_qemu_rbd_driver() {
  echo "Test: qemu-img loads the rbd block driver from qemu-block-extra, and no network driver is installed"
  local output exit_code=0
  # Wrapped in a shell, so the multiarch glob expands in the container and the
  # check is the same on amd64 and arm64.
  docker run --rm "$IMAGE" sh -c 'ls /usr/lib/*-linux-gnu/qemu/block-rbd.so' > /dev/null 2>&1 || exit_code=$?
  assert_eq "block-rbd.so is installed under /usr/lib/<multiarch>/qemu" "0" "$exit_code"
  # The Dockerfile deletes the package's network modules; ls prints none of
  # them while all four are gone.
  output=$(docker run --rm "$IMAGE" sh -c 'ls /usr/lib/*-linux-gnu/qemu/block-curl.so \
    /usr/lib/*-linux-gnu/qemu/block-iscsi.so /usr/lib/*-linux-gnu/qemu/block-nfs.so \
    /usr/lib/*-linux-gnu/qemu/block-ssh.so 2>/dev/null') || true
  assert_eq "the curl, iscsi, nfs and ssh block modules are absent" "" "$output"

  # Probed on 2026-10-10 in ubuntu:noble: without qemu-block-extra qemu-img
  # prints "Unable to load block driver rbd. Perhaps you want to install
  # qemu-block-extra package?"; with it, librados runs and fails with "error
  # connecting: No such file or directory", because no ceph.conf names a
  # monitor. That is as far as a container without a Ceph gets. qemu-img exits
  # 1 in both cases, so the exit code is not asserted; a QEMU that rewords
  # either message fails this test.
  output=$(docker run --rm "$IMAGE" timeout 60 qemu-img info rbd:volumes/x 2>&1) || true
  assert_not_contains "qemu-img finds the rbd block driver" "$output" "Unable to load block driver rbd"
  assert_contains "qemu-img reaches librados, which finds no monitor" "$output" "error connecting"
}

# --- Test 11: libvirtd keeps a private, ephemeral ceph secret ---
test_ceph_secret() {
  echo "Test: virsh defines a private, ephemeral ceph secret and sets it from a base64 file"
  local output exit_code=0 key=AQDhK2VnAAAAABAA7q3+8z9Hq1lnO4JmNo2Gkw==
  # The calls of the lab's ceph-secret.sh
  # (deploy/lab/metal-stack/hypervisor/libvirt-configmap.yaml) against this
  # libvirtd, with a made-up key and the trailing newline a Secret volume
  # carries. A second secret, not private, reads the value back:
  # secret-get-value prints the stored bytes as base64, so it equals the
  # file's text only when --file decoded it.
  # shellcheck disable=SC2016 # expanded by the container's shell
  output=$(docker run --rm -e KEY="$key" "$IMAGE" sh -c '
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
    printf "%s\n" "$KEY" >/tmp/key
    # secret <uuid> <private> <usage name>
    secret() {
      printf "<secret ephemeral=\"yes\" private=\"%s\"><uuid>%s</uuid><usage type=\"ceph\"><name>%s</name></usage></secret>\n" \
        "$2" "$1" "$3" >/tmp/secret.xml
      virsh -c qemu:///system secret-define /tmp/secret.xml >/dev/null &&
        virsh -c qemu:///system secret-set-value "$1" --file /tmp/key >/dev/null
    }
    secret 090e4a3c-6c20-4e74-82dc-1a70382babe8 yes client.cinder && echo "private set"
    secret 6f2b1c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d no client.probe && echo "public set"
    virsh -c qemu:///system secret-get-value 090e4a3c-6c20-4e74-82dc-1a70382babe8 >/dev/null 2>&1 ||
      echo "private value refused"
    echo "public value [$(virsh -c qemu:///system secret-get-value 6f2b1c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d)]"
    echo "files under /etc/libvirt/secrets [$(ls /etc/libvirt/secrets)]"
  ' 2>&1) || exit_code=$?

  assert_eq "the secret test exits 0" "0" "$exit_code"
  assert_contains "a private ephemeral ceph secret is defined and set from the file" "$output" "private set"
  assert_contains "a private secret refuses secret-get-value" "$output" "private value refused"
  assert_contains "--file decodes the base64 key and ignores its trailing newline" \
    "$output" "public value [$key]"
  assert_contains "ephemeral secrets leave no file under /etc/libvirt/secrets" \
    "$output" "files under /etc/libvirt/secrets []"
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
test_qemu_rbd_driver
echo ""
test_ceph_secret
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
