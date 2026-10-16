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
# shellcheck source=tests/lib/build_tools.sh
source "$SCRIPT_DIR/../lib/build_tools.sh"

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

# --- Test 1: libvirtd reports libvirt 12 ---
test_libvirtd_version() {
  echo "Test: libvirtd --version reports libvirt 12"
  local version first_line exit_code=0
  version=$(docker run --rm "$IMAGE" libvirtd --version 2>&1) || exit_code=$?
  first_line=$(printf '%s\n' "$version" | head -n 1)

  assert_eq "libvirtd --version exits 0" "0" "$exit_code"
  # The major moves with the nixpkgs-images revision flake.lock pins, and
  # nova-compute's libvirt-python links the same libvirt derivation
  # (checks.<system>.libvirt-pairing), so client and daemon move together.
  # libvirtd prints the name it was started by, and nixpkgs' wrapper starts
  # it under the wrapper's path, so the name may carry a directory.
  assert_matches "first line reports libvirt 12, the libvirt nova-compute links" \
    "$first_line" '^(/[^ ]*/)?libvirtd \(libvirt\) 12\.'
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

# --- Test 3b: the commands of the lab scripts resolve ---
test_lab_commands() {
  echo "Test: the commands the lab scripts run in this image resolve"
  local tool exit_code output
  # libvirt-configmap.yaml, nfs/client-modules-daemonset.yaml,
  # chaos-mesh/modules-daemonset.yaml and probe/nfs-module-load.yaml under
  # deploy/lab/metal-stack run these by name.
  for tool in systemctl systemd-run rmmod sha256sum comm cut sort tr awk grep getent; do
    exit_code=0
    docker run --rm "$IMAGE" sh -c "command -v $tool" > /dev/null 2>&1 || exit_code=$?
    assert_eq "$tool is on PATH" "0" "$exit_code"
  done

  # A host unit runs virt-admin by its absolute path through nsenter --mount.
  exit_code=0
  docker run --rm "$IMAGE" test -x /usr/bin/virt-admin || exit_code=$?
  assert_eq "/usr/bin/virt-admin is executable" "0" "$exit_code"

  # The host's modules are compressed; modprobe has to decompress them.
  output=$(docker run --rm "$IMAGE" modprobe --version 2>&1) || true
  assert_contains "modprobe reads zstd-compressed modules" "$output" "+ZSTD"
  assert_contains "modprobe reads xz-compressed modules" "$output" "+XZ"
}

# --- Test 4: the UEFI firmware and its descriptor are installed ---
test_ovmf_firmware() {
  echo "Test: the OVMF code and vars files resolve and the x86_64 descriptor names them by a stable path"
  local file exit_code descriptor=/usr/share/qemu/firmware/60-edk2-x86_64.json output
  # The two files are links to QEMU's edk2 build, under the names noble's
  # ovmf package used; test -f follows them.
  for file in /usr/share/OVMF/OVMF_CODE_4M.fd /usr/share/OVMF/OVMF_VARS_4M.fd; do
    exit_code=0
    docker run --rm "$IMAGE" test -f "$file" > /dev/null 2>&1 || exit_code=$?
    assert_eq "$file resolves to a file" "0" "$exit_code"
  done

  exit_code=0
  output=$(docker run --rm "$IMAGE" cat "$descriptor" 2>&1) || exit_code=$?
  assert_eq "$descriptor exists" "0" "$exit_code"
  # libvirt records the descriptor's firmware path in a UEFI guest's XML, so
  # it must name a path the next image has too, not a store path.
  assert_contains "the descriptor names the code file below /usr/share/qemu" \
    "$output" '"filename": "/usr/share/qemu/edk2-x86_64-code.fd"'
  assert_not_contains "the descriptor names no store path" "$output" "/nix/store/"
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
  echo "Test: the libvirt-qemu user and the kvm and libvirt groups exist with noble's IDs"
  local uid entry group gid line exit_code=0
  # The DEVIATION comment in images/libvirt/image.nix leans on these accounts
  # instead of an openstack user. nixpkgs' libvirt compiles root as its
  # default QEMU user, and the consumers write user = "root" into qemu.conf;
  # the accounts keep noble's IDs, and the lab relies on the kvm group's ID
  # differing from the host's.
  uid=$(docker run --rm "$IMAGE" id -u libvirt-qemu 2>&1) || exit_code=$?
  assert_eq "id -u libvirt-qemu exits 0" "0" "$exit_code"
  assert_eq "libvirt-qemu has UID 64055" "64055" "$uid"
  assert_eq "libvirt-qemu's primary group is kvm (104)" "104" \
    "$(docker run --rm "$IMAGE" id -g libvirt-qemu 2>&1)"

  for entry in kvm:104 libvirt:103; do
    group="${entry%%:*}"
    gid="${entry#*:}"
    exit_code=0
    line=$(docker run --rm "$IMAGE" getent group "$group" 2>&1) || exit_code=$?
    assert_eq "group $group exists" "0" "$exit_code"
    assert_eq "group $group has GID $gid" "$gid" "$(cut -d: -f3 <<<"$line")"
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

  exit_code=0
  docker run --rm "$IMAGE" test -f /etc/libvirt/qemu/networks/default.xml || exit_code=$?
  assert_eq "the definition of the default network stays" "0" "$exit_code"
}

# --- Test 8: no build tooling in the runtime image ---
test_no_build_tooling() {
  echo "Test: build tooling is absent"
  local tool exit_code
  assert_no_build_tools
  for tool in git python3; do
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
  # missing QEMU driver makes virsh fail to connect to qemu:///system. The
  # log file named in /etc/libvirt/libvirtd.conf is written only when
  # libvirtd reads that file, the one the lab and the e2e suite write.
  # shellcheck disable=SC2016 # expanded by the container's shell
  output=$(docker run --rm "$IMAGE" sh -c '
    echo "log_outputs=\"1:file:/tmp/libvirtd.log\"" >>/etc/libvirt/libvirtd.conf
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
      virsh -c qemu:///system domcapabilities --virttype qemu --arch x86_64 &&
      if [ -s /tmp/libvirtd.log ]; then echo "libvirtd.log written"; fi
  ' 2>&1) || exit_code=$?

  assert_eq "the daemon smoke test exits 0" "0" "$exit_code"
  assert_contains "virsh reports the QEMU hypervisor" "$output" "Running hypervisor: QEMU"
  assert_contains "domcapabilities names the x86_64 emulator" \
    "$output" "<path>/usr/bin/qemu-system-x86_64</path>"
  assert_contains "domcapabilities offers UEFI firmware" "$output" "<value>efi</value>"
  assert_contains "libvirtd reads /etc/libvirt/libvirtd.conf" "$output" "libvirtd.log written"
}

# --- Test 10: QEMU has the rbd block driver and no network one ---
test_qemu_rbd_driver() {
  echo "Test: qemu-img lists the rbd block driver and no driver that opens a network connection"
  local output formats driver exit_code=0
  # QEMU is built without modules (images/libvirt/qemu.nix), so the drivers
  # are the ones compiled in. qemu-img --help lists them below "Supported
  # image formats:" (QEMU 10; older releases print "Supported formats:" with
  # the list on the same line), up to the "See" line of its footer.
  output=$(docker run --rm "$IMAGE" qemu-img --help 2>&1) || exit_code=$?
  assert_eq "qemu-img --help exits 0" "0" "$exit_code"
  formats=" $(printf '%s\n' "$output" | sed -n '/^Supported.*formats:/,/^See /{/^See /d;s/^Supported.*formats://;p;}' |
    tr -s '[:space:]' ' ') "
  assert_contains "qemu-img --help lists the raw driver" "$formats" " raw "
  assert_contains "the rbd driver is compiled in" "$formats" " rbd "
  for driver in curl http https ftp ftps iscsi nfs ssh; do
    assert_not_contains "the $driver driver is absent" "$formats" " $driver "
  done

  # With the rbd driver, librados runs and fails with "error connecting: No
  # such file or directory", because no ceph.conf names a monitor. That is as
  # far as a container without a Ceph gets. qemu-img exits 1, so the exit
  # code is not asserted; a QEMU that rewords the message fails this test.
  output=$(docker run --rm "$IMAGE" timeout 60 qemu-img info rbd:volumes/x 2>&1) || true
  assert_not_contains "qemu-img finds the rbd block driver" "$output" "Unknown driver 'rbd'"
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
test_lab_commands
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
