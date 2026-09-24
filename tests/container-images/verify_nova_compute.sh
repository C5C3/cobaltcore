#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify nova-compute container image meets requirements
# Usage: bash tests/container-images/verify_nova_compute.sh [image_name]
# Default image: c5c3/nova-compute:32.0.0
# Optional env: NOVA_COMPUTE_RELEASE, the image's release (CI sets it from
#   matrix.release); unset, test 2 finds the release from nova's version
# Requires: Docker daemon running

set -euo pipefail

IMAGE="${1:-c5c3/nova-compute:32.0.0}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$SCRIPT_DIR/../.."
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# --- Test 1: the console scripts ship and nova-compute runs ---
test_console_scripts() {
  echo "Test: the compute console scripts ship and nova-compute runs"
  # nova-compute is the process this image exists for, nova-manage its
  # operator tool, nova-rootwrap the root helper sudo runs and privsep-helper
  # the program rootwrap starts as root for every privsep context. One
  # container checks the four and prints the names that are absent or not
  # executable.
  local err exit_code=0 missing

  missing=$(docker run --rm "$IMAGE" sh -c \
    'for name; do test -x "/var/lib/openstack/bin/$name" || echo "$name"; done' sh \
    nova-compute nova-manage nova-rootwrap privsep-helper 2>&1) ||
    missing="docker run failed: $missing"

  assert_eq "the four console scripts are present and executable" "" "$missing"

  # oslo.config answers --help inside argparse before reading any
  # configuration file, so the wrapper has to import its entry-point target
  # first. Stderr is echoed on failure so a missing module is named.
  err=$(docker run --rm "$IMAGE" nova-compute --help 2>&1 >/dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "nova-compute --help exits 0" "0" "$exit_code"
}

# --- Test 2: the libvirt binding imports at the release's pin ---
test_libvirt_binding_pin() {
  echo "Test: the libvirt binding imports at the pin of the image's release"
  local output exit_code=0 version versions nova_version binding_version
  local matches count release override pin="" pin_source=""

  output=$(docker run --rm "$IMAGE" /var/lib/openstack/bin/python -c \
    'import libvirt; print(libvirt.getVersion())' 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $output"

  assert_eq "import libvirt exits 0" "0" "$exit_code"

  # getVersion() is the version of the libvirt library the binding loaded,
  # as major * 1000000 + minor * 1000 + release; noble ships 10.0.0. A value
  # that is no integer (a traceback) counts as 0, so it never reaches the
  # arithmetic of assert_gte.
  version="${output##*$'\n'}"
  [[ "$version" =~ ^[0-9]+$ ]] || version=0
  assert_gte "libvirt.getVersion() is at least 10000000 (libvirt 10.0.0)" "$version" 10000000

  exit_code=0
  versions=$(docker run --rm "$IMAGE" /var/lib/openstack/bin/python -c \
    'import importlib.metadata as m; print(m.version("nova"), m.version("libvirt-python"))' \
    2>&1) || exit_code=$?
  if [ "$exit_code" -ne 0 ]; then
    echo "    $versions"
    echo "  FAIL: the installed nova and libvirt-python versions are readable"
    FAIL=$((FAIL + 1))
    return
  fi
  versions="${versions##*$'\n'}"
  nova_version="${versions% *}"
  binding_version="${versions#* }"

  # The release the image is built for. Both CI jobs name it in
  # NOVA_COMPUTE_RELEASE, since a branch or SHA pin in source-refs.yaml gives
  # nova a pbr version that no source-refs.yaml value carries. Without it, as
  # in a local run, it is the one whose source-refs.yaml pins the image's nova.
  # A version no release carries, or one two releases carry, would make any pin
  # compared below the wrong one.
  if [ -n "${NOVA_COMPUTE_RELEASE:-}" ]; then
    release="$NOVA_COMPUTE_RELEASE"
  else
    matches=$(grep -lxF "nova: \"$nova_version\"" "$REPO_ROOT"/releases/*/source-refs.yaml || true)
    count=$(grep -c . <<<"$matches" || true)
    if [ "$count" -eq 0 ]; then
      echo "  FAIL: nova $nova_version is no release's source-ref in releases/*/source-refs.yaml; no pin compared"
      FAIL=$((FAIL + 1))
      return
    fi
    if [ "$count" -gt 1 ]; then
      echo "  FAIL: nova $nova_version is the source-ref of more than one release: $(tr '\n' ' ' <<<"${matches//"$REPO_ROOT"\//}")"
      FAIL=$((FAIL + 1))
      return
    fi
    release=$(basename "$(dirname "$matches")")
  fi

  # The override file first, then upper-constraints.txt: on pull requests
  # checkout-service-source has already applied the overrides to
  # upper-constraints.txt in the workspace, while the push-side verify job
  # reads a clean checkout. Reading the override first gives both the pin the
  # build used.
  override="$REPO_ROOT/overrides/$release/constraints.txt"
  if [ -f "$override" ] && grep -qxF -- '-libvirt-python' "$override"; then
    echo "  FAIL: libvirt-python is unpinned for $release: overrides/$release/constraints.txt removes the pin"
    FAIL=$((FAIL + 1))
    return
  fi
  if [ -f "$override" ]; then
    pin=$(sed -n 's/^libvirt-python===\([^;[:space:]]*\).*/\1/p' "$override" | head -n 1)
    pin_source="overrides/$release/constraints.txt"
  fi
  if [ -z "$pin" ]; then
    pin=$(sed -n 's/^libvirt-python===\([^;[:space:]]*\).*/\1/p' \
      "$REPO_ROOT/releases/$release/upper-constraints.txt" 2>/dev/null | head -n 1) || true
    pin_source="releases/$release/upper-constraints.txt"
  fi
  if [ -z "$pin" ]; then
    echo "  FAIL: libvirt-python is unpinned for $release: neither overrides/$release/constraints.txt nor releases/$release/upper-constraints.txt pins it"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_eq "libvirt-python equals the $release pin ($pin_source)" "$pin" "$binding_version"
}

# --- Test 3: the driver and host libraries are importable ---
test_driver_imports() {
  echo "Test: the libvirt driver and the host libraries import cleanly"
  # The libvirt driver, the os-brick connectors for iSCSI and NVMe, the LUKS
  # encryptor and the ovsdbapp IDL os-vif plugs OVS ports through. The last
  # one is the Open vSwitch client this image carries: os-vif's default
  # [os_vif_ovs] ovsdb_interface is native, so no OVS apt package is needed.
  # Stderr is echoed on failure so the module that went missing is named.
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" /var/lib/openstack/bin/python -c \
    "import nova.virt.libvirt.driver, os_brick.initiator.connectors.iscsi, os_brick.initiator.connectors.nvmeof, os_brick.encryptors.luks, vif_plug_ovs.ovsdb.impl_idl" \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "import of the driver and host libraries exits 0" "0" "$exit_code"
}

# --- Test 4: the host tools run ---
test_host_tools() {
  echo "Test: the host tools run"
  # One container and one assertion per tool, so a package that went missing
  # or moved names itself. The comments in the nova-compute block of
  # releases/<release>/extra-packages.yaml name the code that runs each tool.
  local tool exit_code err
  local -a tools=(
    "qemu-img --version"
    "iscsiadm --version"
    "multipath -h"
    "nvme version"
    "lsscsi --version"
    "/lib/udev/scsi_id --version"
    "cryptsetup --version"
    "genisoimage --version"
  )

  for tool in "${tools[@]}"; do
    exit_code=0
    # shellcheck disable=SC2086 # deliberate: split the tool and its argument
    err=$(docker run --rm "$IMAGE" $tool 2>&1 > /dev/null) || exit_code=$?
    [ "$exit_code" -eq 0 ] || echo "    $err"

    assert_eq "$tool exits 0" "0" "$exit_code"
  done
}

# --- Test 5: no node identity is baked into the image ---
test_no_baked_node_identity() {
  echo "Test: no node identity is baked into the image"
  # The postinst scripts of open-iscsi and nvme-cli write the initiator name,
  # the host NQN and the host ID at build time, and multipath-tools ships a
  # multipath.conf. os-brick reports the IQN and the NQN to Cinder as the
  # node's identity, so a baked file would give every compute node the same
  # one. The consumer mounts the host's files. One container prints the paths
  # that exist.
  local path present exit_code=0
  local -a identity=(
    /etc/iscsi/initiatorname.iscsi
    /etc/nvme/hostnqn
    /etc/nvme/hostid
    /etc/multipath.conf
  )

  present=$(docker run --rm "$IMAGE" sh -c \
    'for file; do if test -e "$file"; then echo "$file"; fi; done' sh \
    "${identity[@]}" 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $present"

  assert_eq "the identity check runs" "0" "$exit_code"
  for path in "${identity[@]}"; do
    assert_not_contains "$path is not baked into the image" "$present" "$path"
  done
}

# --- Test 6: the rootwrap and sudo posture ---
test_rootwrap_sudo_posture() {
  echo "Test: nova's default root helper resolves and is confined"
  # nova's root helper is `sudo nova-rootwrap /etc/nova/rootwrap.conf`, which
  # every privsep context of nova-compute runs. Checks run as the default
  # user unless stated.
  local conf output exit_code=0 path writable

  conf=$(docker run --rm "$IMAGE" cat /etc/nova/rootwrap.conf 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $conf"
  if grep -qx 'filters_path=/var/lib/openstack/etc/nova/rootwrap.d' <<<"$conf"; then
    echo "  PASS: rootwrap.conf points filters_path at the installed filters"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: rootwrap.conf has no line filters_path=/var/lib/openstack/etc/nova/rootwrap.d"
    FAIL=$((FAIL + 1))
  fi
  if grep -q '^exec_dirs=/var/lib/openstack/bin,' <<<"$conf"; then
    echo "  PASS: rootwrap.conf searches /var/lib/openstack/bin first"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: rootwrap.conf has no line starting with exec_dirs=/var/lib/openstack/bin,"
    FAIL=$((FAIL + 1))
  fi

  exit_code=0
  output=$(docker run --rm "$IMAGE" \
    test -f /var/lib/openstack/etc/nova/rootwrap.d/compute.filters 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $output"
  assert_eq "compute.filters is installed" "0" "$exit_code"

  # The files rootwrap trusts and the binaries it runs as root. A path the
  # service user could write is a path through which it could run anything
  # as root without the rule below. One container prints the writable ones.
  local -a trusted=(
    /etc/nova/rootwrap.conf
    /var/lib/openstack/etc/nova/rootwrap.d
    /var/lib/openstack/etc/nova/rootwrap.d/compute.filters
    /var/lib/openstack/bin
    /var/lib/openstack/bin/privsep-helper
    /etc/sudoers.d/nova-compute
  )
  exit_code=0
  writable=$(docker run --rm "$IMAGE" sh -c \
    'for path; do if test -w "$path"; then echo "$path"; fi; done' sh \
    "${trusted[@]}" 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $writable"
  assert_eq "the writability check runs" "0" "$exit_code"
  for path in "${trusted[@]}"; do
    assert_not_contains "$path is not writable by the service user" "$writable" "$path"
  done

  # The one sudo rule, and nothing else without a password.
  exit_code=0
  output=$(docker run --rm "$IMAGE" sudo -n -l 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $output"
  assert_contains "sudo -l lists the nova-rootwrap rule" \
    "$output" "/var/lib/openstack/bin/nova-rootwrap /etc/nova/rootwrap.conf *"
  assert_eq "sudo -l lists exactly one NOPASSWD entry" \
    "1" "$(grep -c NOPASSWD <<<"$output" || true)"

  exit_code=0
  output=$(docker run --rm "$IMAGE" sudo -n true 2>&1) || exit_code=$?
  [ "$exit_code" -ne 0 ] || echo "    $output"
  assert_nonzero_exit "sudo -n true is refused" "$exit_code"

  # Exit 99 is rootwrap's answer to a command no filter admits. Reaching it
  # proves that sudo resolved nova-rootwrap through secure_path and that
  # rootwrap loaded the filters.
  exit_code=0
  output=$(docker run --rm "$IMAGE" \
    sudo -n nova-rootwrap /etc/nova/rootwrap.conf id 2>&1 >/dev/null) || exit_code=$?
  [ "$exit_code" -eq 99 ] || echo "    $output"
  assert_eq "sudo nova-rootwrap refuses id with exit 99" "99" "$exit_code"
  assert_contains "rootwrap names the refused command" "$output" "Unauthorized command: id"

  # privsep-helper runs as root and fails on the absent configuration file,
  # which proves that rootwrap found it through exec_dirs. Without the
  # /var/lib/openstack/bin entry rootwrap exits 96 with Executable not found.
  exit_code=0
  output=$(docker run --rm "$IMAGE" \
    sudo -n nova-rootwrap /etc/nova/rootwrap.conf privsep-helper \
    --config-file /tmp/absent.conf \
    --privsep_context os_brick.privileged.default \
    --privsep_sock_path /tmp/absent.sock 2>&1) || exit_code=$?
  [ "$exit_code" -eq 1 ] || echo "    $output"
  assert_eq "privsep-helper runs through rootwrap and exits 1" "1" "$exit_code"
  assert_contains "privsep-helper reads its configuration as root" \
    "$output" "ConfigFilesNotFoundError"

  # secure_path is global, so a consumer that runs nova-compute as root
  # resolves the same default helper.
  exit_code=0
  output=$(docker run --rm --user 0 "$IMAGE" \
    sudo -n nova-rootwrap /etc/nova/rootwrap.conf id 2>&1 >/dev/null) || exit_code=$?
  [ "$exit_code" -eq 99 ] || echo "    $output"
  assert_eq "as root, sudo nova-rootwrap refuses id with exit 99" "99" "$exit_code"
}

# --- Test 7: runs as openstack user ---
test_runs_as_openstack_user() {
  echo "Test: container runs as openstack user"
  local whoami_output exit_code=0
  whoami_output=$(docker run --rm "$IMAGE" whoami 2>&1) || exit_code=$?

  assert_eq "whoami exits 0" "0" "$exit_code"
  assert_eq "whoami outputs openstack" "openstack" "$whoami_output"
}

# --- Test 8: no build tools in final image ---
test_no_build_tools_in_final_image() {
  echo "Test: no build tools in final image"
  # The build stage installs libvirt-dev and pkg-config to compile the
  # binding; the runtime stage copies /var/lib/openstack alone.
  local check exit_code
  local -a checks=(
    "which gcc"
    "which pkg-config"
    "which uv"
    "dpkg -s python3-dev"
    "dpkg -s libvirt-dev"
  )

  for check in "${checks[@]}"; do
    exit_code=0
    # shellcheck disable=SC2086 # deliberate: split the command and its argument
    docker run --rm "$IMAGE" $check > /dev/null 2>&1 || exit_code=$?
    assert_nonzero_exit "$check fails" "$exit_code"
  done
}

# --- Test 9: the state directories ship empty and owned by 42424 ---
test_state_directories() {
  echo "Test: the state directories ship empty and owned by 42424"
  # /var/lib/nova is meant as [DEFAULT] state_path, where a compute stores its
  # node identity. instances is instances_path, the default
  # $state_path/instances, and tmp is [oslo_concurrency] lock_path. Both have
  # to exist, be empty and be owned by 42424, the UID the container runs under.
  # The consumer (#1061) mounts /var/lib/nova from the host, which replaces
  # this layout: only a plain `docker run` gets it, and the ownership of the
  # host directories is the pod's to set. A failed docker run shows up as the
  # actual value of each assertion.
  local output
  output=$(docker run --rm "$IMAGE" \
    sh -c 'find /var/lib/nova -mindepth 1 -maxdepth 1 -type d -printf "%f\n" | sort | tr "\n" " "' 2>&1) || true
  assert_eq "the two state directories are present" "instances tmp " "$output"

  output=$(docker run --rm "$IMAGE" \
    sh -c 'find /var/lib/nova -mindepth 2 | wc -l' 2>&1) || true
  assert_eq "the state directories are empty" "0" "$(echo "$output" | tr -d ' ')"

  # The owning UID of the whole tree as a sorted set, so a directory the
  # chown -R missed shows up here instead of only on the one that is checked.
  output=$(docker run --rm "$IMAGE" \
    sh -c 'stat -c %u /var/lib/nova /var/lib/nova/* | sort -u | tr "\n" " "' 2>&1) || true
  assert_eq "the state tree is owned by 42424 alone" "42424 " "$output"
}

# --- Run all tests ---
echo "=== nova-compute container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_console_scripts
echo ""
test_libvirt_binding_pin
echo ""
test_driver_imports
echo ""
test_host_tools
echo ""
test_no_baked_node_identity
echo ""
test_rootwrap_sudo_posture
echo ""
test_runs_as_openstack_user
echo ""
test_no_build_tools_in_final_image
echo ""
test_state_directories
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
