#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the metal-stack node probe, the read-only survey Job of #1138, and
# the NFS module load test of #1194 beside it:
#   1. deploy/lab/metal-stack/probe/{kustomization,node-probe,nfs-module-load}.yaml
#      exist with SPDX headers.
#   2. The kustomization's resources list names node-probe.yaml alone, and no
#      non-comment line names a parent-directory path (kubectl's embedded
#      kustomize cannot lift the load restrictor, kubernetes/kubectl#948).
#   3. kustomize build renders one document, the batch/v1 Job node-probe in
#      default, with backoffLimit 0, ttlSecondsAfterFinished 3600,
#      restartPolicy Never, no hostPID, no hostNetwork, no ServiceAccount
#      token, no init container, and one privileged container on a
#      digest-pinned debian image with the host root as its one volume,
#      mounted read-only at /host. node-probe.yaml read alone, the file the
#      per-node yq form applies past kustomize, equals the render.
#   4. The container's script reads the NIC list from the host's sysfs and
#      the registered filesystems from /proc/filesystems, ends with exit 0,
#      names none of nsenter, chroot, modprobe, insmod, rmmod, mount, umount,
#      sysctl, apt-get, apt, tee, dd, mknod and rm, redirects nothing but
#      stderr to /dev/null, and never names /host/var/run.
#   5. Run against an empty stand-in for the host root, the script exits 0 and
#      prints the twelve headers in order with its absent and NOT FOUND
#      fallbacks, NOT FOUND for each of the five NFS module files and the six
#      Chaos Mesh module files, nfs4 and nfsd as not registered, and both
#      containerd socket paths as absent with no [grpc] address set; against
#      a populated one it reports a module file, a module built into the
#      kernel, the NFS server binary and a NIC, nfs stays NOT FOUND beside an
#      nfsd.ko and ip_set_hash_ip beside an ip_set_hash_ipport.ko, the loaded
#      modules are the NFS ones, sunrpc and the four Chaos Mesh ones alone,
#      nfs4 is registered, a regular file at containerd's socket path prints
#      present, not a socket, a bound socket at k3s's prints socket, and of
#      four address keys in config.toml only the [grpc] address prints, not
#      its tcp_address. An indented [grpc] table prints its address without
#      a trailing comment, a quoted address keeps a `#` inside its quotes, and
#      a [grpc] table without an address prints not set, and not the address
#      of an indented table after it. Each stand-in brings its own
#      /proc/modules and /proc/filesystems. A script that fails the command
#      or redirection check of 4 is not run.
#   6. nfs-module-load.yaml, which 2 and 3 keep out of the render, is read and
#      never run: one batch/v1 Job nfs-module-load in default with
#      backoffLimit 0, ttlSecondsAfterFinished 3600, restartPolicy Never, no
#      hostPID, no hostNetwork, no ServiceAccount token, no init container,
#      and one privileged container with a read-only root filesystem and a
#      memory limit, on the image of host-prepare in
#      deploy/lab/metal-stack/hypervisor/libvirt-daemonset.yaml pulled
#      IfNotPresent, with /lib/modules as its one volume, mounted under the
#      volume's name read-only at /lib/modules. Its /bin/bash -c script
#      removes what it loaded with rmmod and never with modprobe -r, ends with
#      exit 0, names none of nsenter, chroot, insmod, mount, umount, sysctl,
#      apt-get, apt, tee, dd, mknod and rm, and redirects nothing but stderr
#      (2>/dev/null, 2>&1).
#   7. Run against a stand-in node (its /proc/modules, /sys/module and
#      /proc/filesystems) with stub modprobe and rmmod first on PATH, the load
#      script exits 0 and prints its four headers, nfsd and nfsv4 as loaded,
#      nfs, which the stand-in had loaded, as already loaded, and nfsd as
#      registered. It names the modules its load added but not vhost_net,
#      which the stub loads as another pod would during the run, removes them
#      over more than one rmmod pass, leaves the modules loaded before, and
#      prints `module list as before` last; a module rmmod refuses is named
#      on a last `still loaded:` line. When the stub also loads nfsv4, as
#      another pod would during the run, nfsv4 prints already loaded, neither
#      it nor dns_resolver is named or removed, and auth_rpcgss, which nfsv4
#      uses, is named on the last `still loaded:` line. On a stand-in with no
#      NFS module loaded, when the stub also loads nfs during the run, nfs
#      prints already loaded and is neither named nor removed although
#      nfsv4, which the run loads, depends on it, and lockd, grace and
#      sunrpc, which nfs uses, are named on the last `still loaded:` line.
#      With a modprobe that fails it prints each FAILED line with modprobe's
#      message and none as the modules the load added, and still exits 0. A
#      script that fails the command or redirection check of 6, or calls
#      modprobe or rmmod by path, is not run.
#
# Checks 3 to 5 are counted as SKIP when kustomize or yq is not on PATH, and
# checks 6 and 7 when yq is not; a failing kustomize build counts 3 to 5 as
# FAIL and prints the build's error.
#
# Usage: bash tests/unit/deploy/metal_stack_probe_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

PROBE_DIR="$PROJECT_ROOT/deploy/lab/metal-stack/probe"
PROBE_KUSTOMIZATION="$PROBE_DIR/kustomization.yaml"
PROBE_MANIFEST="$PROBE_DIR/node-probe.yaml"
LOAD_MANIFEST="$PROBE_DIR/nfs-module-load.yaml"
LIBVIRT_DAEMONSET="$PROJECT_ROOT/deploy/lab/metal-stack/hypervisor/libvirt-daemonset.yaml"

IMAGE_PATTERN='^docker\.io/library/debian:[^@]+@sha256:[a-f0-9]{64}$'

EXPECTED_HEADERS="$(printf '%s\n' \
  '== kvm device' \
  '== cpu' \
  '== loaded modules' \
  '== module files for' \
  '== filesystems' \
  '== nested / iommu' \
  '== memory' \
  '== disks' \
  '== cgroup' \
  '== host os / binaries' \
  '== containerd socket' \
  '== nics')"

EXPECTED_LOAD_HEADERS="$(printf '%s\n' \
  '== load' \
  '== filesystems' \
  '== modules the load added' \
  '== unload')"

RENDERED=""

have() {
  command -v "$1" >/dev/null 2>&1
}

# The first value a yq expression yields over the YAML on stdin: the rendered
# probe, or a manifest read by file such as nfs-module-load.yaml.
rendered_value() {
  yq -N -r "select(. != null) | $1" - | head -n 1
}

# Render the probe directory into RENDERED. When the render cannot be read,
# counts the caller's checks as SKIP (kustomize or yq missing) or FAIL (the
# build failed) and returns 1.
render_probe() {
  local checks="$1"

  if ! have kustomize || ! have yq; then
    echo "  SKIP: kustomize or yq not installed ($checks checks skipped)"
    SKIP=$((SKIP + checks))
    return 1
  fi

  # No --load-restrictor: kubectl apply -k cannot pass one either.
  if ! RENDERED="$(kustomize build "$PROBE_DIR" 2>&1)"; then
    echo "  FAIL: kustomize build $PROBE_DIR failed (default LoadRestrictionsRootOnly):"
    echo "$RENDERED" | head -20
    FAIL=$((FAIL + checks))
    return 1
  fi
}

# --- Test 1: probe files exist with SPDX headers ---
test_probe_files_exist_with_spdx() {
  echo "Test: deploy/lab/metal-stack/probe/{kustomization,node-probe,nfs-module-load}.yaml exist with SPDX headers"

  local f
  for f in "$PROBE_KUSTOMIZATION" "$PROBE_MANIFEST" "$LOAD_MANIFEST"; do
    if [[ ! -f "$f" ]]; then
      echo "  FAIL: $f does not exist"
      FAIL=$((FAIL + 1))
      continue
    fi
    assert_file_contains "$(basename "$f") has SPDX FileCopyrightText header" \
      "$f" "SPDX-FileCopyrightText: Copyright 2026 SAP SE"
    assert_file_contains "$(basename "$f") has SPDX-License-Identifier: Apache-2.0" \
      "$f" "SPDX-License-Identifier: Apache-2.0"
  done
}

# --- Test 2: the kustomization lists node-probe.yaml alone, no parent dirs ---
test_kustomization_is_self_contained() {
  echo "Test: kustomization lists node-probe.yaml alone and names no parent directory"

  if [[ ! -f "$PROBE_KUSTOMIZATION" ]]; then
    echo "  FAIL: $PROBE_KUSTOMIZATION does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  # The items of the top-level resources key only: the range ends at the next
  # top-level key.
  local entries
  entries="$(awk '
    /^resources:/ { in_list = 1; next }
    in_list && /^[^[:space:]#-]/ { in_list = 0 }
    in_list && /^[[:space:]]*-[[:space:]]+/ {
      sub(/^[[:space:]]*-[[:space:]]+/, ""); sub(/[[:space:]]+$/, ""); print
    }
  ' "$PROBE_KUSTOMIZATION")"
  assert_eq "kustomization.yaml's resources list names node-probe.yaml alone" \
    "node-probe.yaml" "$entries"

  # Comments may explain the rule; only a non-comment line can break it.
  local parent_refs
  parent_refs="$( { grep -vE '^[[:space:]]*#' "$PROBE_KUSTOMIZATION" | grep -F '../' || true; } | wc -l)"
  assert_eq "kustomization.yaml names no '../' path" "0" "${parent_refs// /}"
}

# --- Test 3: the rendered Job and its read-only posture ---
test_render_is_one_privileged_readonly_job() {
  echo "Test: kustomize build renders one privileged Job with the host root mounted read-only"

  render_probe 26 || return

  local count
  count="$(printf '%s\n' "$RENDERED" | yq -N -r 'select(. != null) | .kind' - | grep -c .)"
  assert_eq "the directory renders exactly one document" "1" "$count"

  # The per-node form pipes node-probe.yaml through yq into kubectl, past
  # kustomize; it applies the same Job only while the kustomization
  # transforms nothing.
  assert_eq "node-probe.yaml read alone equals the render" \
    "$(printf '%s\n' "$RENDERED" | yq -o=json -I=0 'sort_keys(..)' -)" \
    "$(yq -o=json -I=0 'sort_keys(..)' "$PROBE_MANIFEST")"

  local job='select(.kind == "Job")'
  local pod="$job | .spec.template.spec"
  local container="$pod | .containers[0]"

  assert_eq "the document is a batch/v1 object" "batch/v1" \
    "$(printf '%s\n' "$RENDERED" | rendered_value '.apiVersion')"
  assert_eq "the document is a Job" "Job" \
    "$(printf '%s\n' "$RENDERED" | rendered_value '.kind')"
  assert_eq "the Job is named node-probe" "node-probe" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$job | .metadata.name")"
  assert_eq "the Job lives in default" "default" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$job | .metadata.namespace")"
  assert_eq "the Job never retries (backoffLimit 0)" "0" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$job | .spec.backoffLimit")"
  assert_eq "the Job is removed an hour after it finishes" "3600" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$job | .spec.ttlSecondsAfterFinished")"

  assert_eq "the pod never restarts" "Never" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$pod | .restartPolicy")"
  assert_eq "the pod does not share the host PID namespace" "false" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$pod | .hostPID // false")"
  assert_eq "the pod does not share the host network namespace" "false" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$pod | .hostNetwork // false")"
  assert_eq "the pod mounts no ServiceAccount token" "false" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$pod | .automountServiceAccountToken")"
  assert_eq "the pod has no init container" "0" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$pod | .initContainers // [] | length")"
  assert_eq "the pod has exactly one container" "1" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$pod | .containers | length")"
  assert_eq "the container is named probe" "probe" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$container | .name")"

  local image
  image="$(printf '%s\n' "$RENDERED" | rendered_value "$container | .image")"
  if grep -qE "$IMAGE_PATTERN" <<<"$image"; then
    echo "  PASS: the image is a digest-pinned docker.io/library/debian tag"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the image '$image' is not a digest-pinned docker.io/library/debian tag"
    FAIL=$((FAIL + 1))
  fi

  assert_eq "the container is privileged" "true" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$container | .securityContext.privileged // false")"
  assert_eq "the container's root filesystem is read-only" "true" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$container | .securityContext.readOnlyRootFilesystem // false")"
  assert_not_empty "the container has a memory limit" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$container | .resources.limits.memory // \"\"")"

  assert_eq "the pod has exactly one volume" "1" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$pod | .volumes | length")"
  assert_eq "the volume is the host root" "/" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$pod | .volumes[0].hostPath.path")"
  assert_eq "the container has exactly one volume mount" "1" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$container | .volumeMounts | length")"
  assert_eq "the mount names the host-root volume" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$pod | .volumes[0].name")" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$container | .volumeMounts[0].name")"
  assert_eq "the host root is mounted at /host" "/host" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$container | .volumeMounts[0].mountPath")"
  assert_eq "the host root is mounted read-only" "true" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$container | .volumeMounts[0].readOnly // false")"
  assert_eq "the host root's submounts are read-only where the runtime can" "IfPossible" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$container | .volumeMounts[0].recursiveReadOnly")"
}

# The probe script, the container's one argument, from the render.
rendered_script() {
  printf '%s\n' "$RENDERED" \
    | yq -N -r 'select(.kind == "Job") | .spec.template.spec.containers[0].args[0] // ""' -
}

# Commands that change the node; the probe also forbids modprobe and rmmod,
# which the load test exists to run.
NODE_CHANGING_COMMANDS='nsenter|chroot|insmod|mount|umount|sysctl|apt-get|apt|tee|dd|mknod|rm'

# The forbidden commands a script names, space-separated. Whole words, so
# `/proc/mounts` and `MOUNTPOINT` do not count, but `$(mount)` and `mount;` do.
forbidden_commands() {
  printf '%s\n' "$1" \
    | grep -owE "modprobe|rmmod|$NODE_CHANGING_COMMANDS" \
    | sort -u | tr '\n' ' ' || true
}

# A script without its one permitted redirection, 2>/dev/null, which discards
# an error message: any `>` left in it redirects something else.
without_stderr_discard() {
  printf '%s\n' "${1//2>\/dev\/null/}"
}

# --- Test 4: the script ends with exit 0 and writes nothing ---
test_script_ends_with_exit_0_and_writes_nothing() {
  echo "Test: the probe script ends with exit 0 and writes nothing"

  render_probe 9 || return

  local container='select(.kind == "Job") | .spec.template.spec.containers[0]'
  local script
  script="$(rendered_script)"
  assert_not_empty "the container carries a script argument" "$script"
  assert_eq "the container runs the script with /bin/sh -c" "/bin/sh -c" \
    "$(printf '%s\n' "$RENDERED" | rendered_value "$container | .command // [] | join(\" \")")"

  local last
  last="$(printf '%s\n' "$script" | grep -v '^[[:space:]]*$' | tail -n 1)"
  assert_eq "the script's last command is exit 0" "exit 0" "$last"

  assert_eq "the script names none of the forbidden commands" \
    "" "$(forbidden_commands "$script")"
  assert_not_contains "the script redirects nothing but stderr to /dev/null" \
    "$(without_stderr_discard "$script")" ">"

  assert_contains "the script reads the NIC list from the host's sysfs" \
    "$script" "/host/sys/class/net/"
  assert_contains "the script looks up module files in the host's module tree" \
    "$script" "/host/lib/modules/"
  assert_contains "the script reads the registered filesystems" \
    "$script" "/proc/filesystems"
  assert_not_contains "the script never looks for the socket under /host/var/run" \
    "$script" "/host/var/run"
}

# The lines of a script's output under the header $2, up to the next header.
section() {
  printf '%s\n' "$1" | awk -v h="$2" '/^== / { inside = ($0 == h); next } inside'
}

# The probe script run against stand-ins: the host root $2 for /host, and
# $3/modules and $3/filesystems for the /proc files it reports.
run_probe_on() {
  local s="${1//\/host/$2}"
  s="${s//\/proc\/modules/$3/modules}"
  sh -c "${s//\/proc\/filesystems/$3/filesystems}" 2>/dev/null
}

# --- Test 5: the script completes on any node and reports what it finds ---
test_script_reports_the_host_root_and_completes() {
  echo "Test: the probe script exits 0 on an empty host root and reports a populated one"

  local checks=34
  render_probe "$checks" || return

  local script
  script="$(rendered_script)"

  # Only /host, /proc/modules and /proc/filesystems are swapped for stand-ins:
  # every other path the script touches is this machine's own, outside the
  # pod's read-only mounts.
  if [[ -n "$(forbidden_commands "$script")" || "$(without_stderr_discard "$script")" == *">"* ]]; then
    echo "  FAIL: the script fails the forbidden-command or redirection check; not running it on this host ($checks checks)"
    FAIL=$((FAIL + checks))
    return
  fi

  # Two stand-ins for the host root the pod mounts at /host: an empty one, and
  # one with four module files, three built-in modules, the NFS server binary,
  # a NIC, a regular file at containerd's socket path, a bound socket at
  # k3s's and a containerd config.toml. Each comes with a /proc/modules and a
  # /proc/filesystems: empty ones, and ones with the NFS server, its helpers,
  # four Chaos Mesh modules and two other modules loaded and nfs4 registered.
  local tmp kver
  tmp="$(mktemp -d)"
  kver="$(uname -r)"
  mkdir -p "$tmp/empty" "$tmp/node/lib/modules/$kver/kernel/arch/x86/kvm" \
    "$tmp/node/lib/modules/$kver/kernel/fs/nfsd" \
    "$tmp/node/lib/modules/$kver/kernel/net/netfilter/ipset" \
    "$tmp/node/usr/sbin" "$tmp/node/sys/class/net/lan0" \
    "$tmp/node/run/containerd" "$tmp/node/run/k3s/containerd" \
    "$tmp/node/etc/containerd" \
    "$tmp/empty-proc" "$tmp/node-proc"
  : >"$tmp/node/lib/modules/$kver/kernel/arch/x86/kvm/kvm.ko"
  : >"$tmp/node/lib/modules/$kver/kernel/fs/nfsd/nfsd.ko"
  : >"$tmp/node/lib/modules/$kver/kernel/net/netfilter/xt_set.ko"
  : >"$tmp/node/lib/modules/$kver/kernel/net/netfilter/ipset/ip_set_hash_ipport.ko"
  printf '%s\n' "kernel/drivers/net/tun.ko" "kernel/net/sunrpc/sunrpc.ko" \
    "kernel/net/sched/sch_netem.ko" >"$tmp/node/lib/modules/$kver/modules.builtin"
  : >"$tmp/node/usr/sbin/rpc.nfsd"
  echo 9000 >"$tmp/node/sys/class/net/lan0/mtu"
  echo up >"$tmp/node/sys/class/net/lan0/operstate"
  : >"$tmp/node/run/containerd/containerd.sock"
  python3 -c 'import socket, sys; socket.socket(socket.AF_UNIX).bind(sys.argv[1])' \
    "$tmp/node/run/k3s/containerd/containerd.sock"
  # Four address keys, as containerd config default writes them; the probe
  # prints the one of [grpc] and not its tcp_address.
  printf '%s\n' 'version = 2' '[debug]' '  address = "/run/containerd/debug.sock"' \
    '[grpc]' '  address = "/run/containerd/containerd.sock"' '  tcp_address = ""' \
    '[ttrpc]' '  address = ""' >"$tmp/node/etc/containerd/config.toml"
  : >"$tmp/empty-proc/modules"
  : >"$tmp/empty-proc/filesystems"
  # The probe lists nfsd, nfs_acl, sunrpc, ip_set_hash_net, xt_set, sch_netem
  # and sch_tbf, not ip_tunnel, which has `tun` inside its name, nor ext4.
  printf '%s\n' 'nfsd 811008 13 - Live 0x0' 'nfs_acl 16384 1 nfsd, Live 0x0' \
    'ip_tunnel 32768 0 - Live 0x0' 'sunrpc 704512 2 nfsd,nfs_acl, Live 0x0' \
    'ext4 1003520 1 - Live 0x0' 'ip_set_hash_net 53248 1 - Live 0x0' \
    'xt_set 45056 0 - Live 0x0' 'sch_netem 20480 0 - Live 0x0' \
    'sch_tbf 20480 0 - Live 0x0' >"$tmp/node-proc/modules"
  printf 'nodev\tsysfs\n\text4\nnodev\tnfs\nnodev\tnfs4\n' >"$tmp/node-proc/filesystems"

  local empty node rc=0
  empty="$(run_probe_on "$script" "$tmp/empty" "$tmp/empty-proc")" || rc=$?
  node="$(run_probe_on "$script" "$tmp/node" "$tmp/node-proc")"

  assert_eq "the script exits 0 on a host root that lacks everything" "0" "$rc"
  # The kernel version after `== module files for` is not part of the header.
  assert_eq "the script prints the twelve headers in order" "$EXPECTED_HEADERS" \
    "$(printf '%s\n' "$empty" | grep -E '^== ' | sed -E 's/^(== module files for) .*/\1/')"
  assert_contains "a missing module file prints NOT FOUND" "$empty" "kvm: NOT FOUND"
  assert_contains "a missing binary prints absent" "$empty" "usr/sbin/libvirtd: absent"
  assert_contains "a host without NICs prints absent" "$empty" $'== nics\nabsent'
  local m
  for m in nfsd nfs nfsv4 lockd sunrpc; do
    assert_contains "a missing $m module file prints NOT FOUND" "$empty" "$m: NOT FOUND"
  done
  assert_eq "a kernel without NFS filesystems prints not registered for nfs4 and nfsd" \
    "$(printf '%s\n' 'nfs4: not registered' 'nfsd: not registered')" \
    "$(section "$empty" '== filesystems')"
  for m in ip_set ip_set_hash_ip ip_set_hash_net xt_set sch_netem sch_tbf; do
    assert_contains "a missing $m module file prints NOT FOUND" "$empty" "$m: NOT FOUND"
  done
  assert_eq "a host root without run/ and config.toml prints both sockets absent and no address" \
    "$(printf '%s\n' 'run/containerd/containerd.sock: absent' \
      'run/k3s/containerd/containerd.sock: absent' 'config.toml [grpc] address: not set')" \
    "$(section "$empty" '== containerd socket')"

  assert_contains "a module file prints its path" \
    "$node" "kvm: $tmp/node/lib/modules/$kver/kernel/arch/x86/kvm/kvm.ko"
  assert_contains "a module built into the kernel prints builtin" "$node" "tun: builtin"
  assert_contains "an installed NFS server prints present" "$node" "usr/sbin/rpc.nfsd: present"
  assert_contains "a NIC prints its MTU and state" "$node" "lan0: mtu=9000 operstate=up"
  assert_contains "the nfsd module file prints its path" \
    "$node" "nfsd: $tmp/node/lib/modules/$kver/kernel/fs/nfsd/nfsd.ko"
  assert_contains "a built-in sunrpc prints builtin" "$node" "sunrpc: builtin"
  assert_contains "nfs does not match nfsd.ko and prints NOT FOUND" "$node" "nfs: NOT FOUND"
  assert_eq "the loaded-modules section lists the NFS modules, sunrpc and the four Chaos Mesh ones only" \
    "$(printf '%s\n' nfsd nfs_acl sunrpc ip_set_hash_net xt_set sch_netem sch_tbf)" \
    "$(section "$node" '== loaded modules')"
  assert_eq "a registered nfs4 prints registered, beside an nfsd that is not" \
    "$(printf '%s\n' 'nfs4: registered' 'nfsd: not registered')" \
    "$(section "$node" '== filesystems')"
  assert_contains "the xt_set module file prints its path" \
    "$node" "xt_set: $tmp/node/lib/modules/$kver/kernel/net/netfilter/xt_set.ko"
  assert_contains "a built-in sch_netem prints builtin" "$node" "sch_netem: builtin"
  assert_contains "ip_set_hash_ip does not match ip_set_hash_ipport.ko and prints NOT FOUND" \
    "$node" "ip_set_hash_ip: NOT FOUND"
  assert_eq "a regular file at the socket path prints present, not a socket, a bound socket prints socket, and only the [grpc] address prints" \
    "$(printf '%s\n' 'run/containerd/containerd.sock: present, not a socket' \
      'run/k3s/containerd/containerd.sock: socket' \
      'config.toml [grpc] address: "/run/containerd/containerd.sock"')" \
    "$(section "$node" '== containerd socket')"

  # Three other valid config.toml forms, in a host root that has nothing else.
  # The first has an indented [grpc] header, no spaces around `=`, a trailing
  # comment and an indented table with an address of its own after it; the
  # second has a `#` inside the quoted address; the third has a [grpc] table
  # without an address.
  local toml
  mkdir -p "$tmp/toml/etc/containerd"
  printf '%s\n' 'version = 2' '  [grpc]' '    address="/run/containerd/containerd.sock" # default' \
    '  [metrics]' '    address = "127.0.0.1:1338"' >"$tmp/toml/etc/containerd/config.toml"
  toml="$(run_probe_on "$script" "$tmp/toml" "$tmp/empty-proc")"
  assert_eq "an indented [grpc] table prints its address without the comment, not the next table's" \
    "$(printf '%s\n' 'run/containerd/containerd.sock: absent' \
      'run/k3s/containerd/containerd.sock: absent' \
      'config.toml [grpc] address: "/run/containerd/containerd.sock"')" \
    "$(section "$toml" '== containerd socket')"
  printf '%s\n' 'version = 2' '[grpc]' '  address = "/run/a#b.sock" # c' \
    >"$tmp/toml/etc/containerd/config.toml"
  toml="$(run_probe_on "$script" "$tmp/toml" "$tmp/empty-proc")"
  assert_eq "a quoted address keeps the # inside its quotes and drops the trailing comment" \
    "$(printf '%s\n' 'run/containerd/containerd.sock: absent' \
      'run/k3s/containerd/containerd.sock: absent' 'config.toml [grpc] address: "/run/a#b.sock"')" \
    "$(section "$toml" '== containerd socket')"
  printf '%s\n' 'version = 2' '[grpc]' '  tcp_address = ""' \
    '  [metrics]' '  address = "127.0.0.1:1338"' >"$tmp/toml/etc/containerd/config.toml"
  toml="$(run_probe_on "$script" "$tmp/toml" "$tmp/empty-proc")"
  assert_eq "a [grpc] table without an address prints not set, not the address of the indented table after it" \
    "$(printf '%s\n' 'run/containerd/containerd.sock: absent' \
      'run/k3s/containerd/containerd.sock: absent' 'config.toml [grpc] address: not set')" \
    "$(section "$toml" '== containerd socket')"

  rm -rf "$tmp"
}

# Check that the load test can be read. When it cannot, counts the caller's
# checks as SKIP (yq missing) or FAIL (the file is missing) and returns 1.
load_test_ready() {
  local checks="$1"

  if ! have yq; then
    echo "  SKIP: yq not installed ($checks checks skipped)"
    SKIP=$((SKIP + checks))
    return 1
  fi
  if [[ ! -f "$LOAD_MANIFEST" ]]; then
    echo "  FAIL: $LOAD_MANIFEST does not exist ($checks checks)"
    FAIL=$((FAIL + checks))
    return 1
  fi
}

# The load script, the container's one argument.
load_script() {
  yq -N -r '.spec.template.spec.containers[0].args[0] // ""' "$LOAD_MANIFEST"
}

# The forbidden commands the load script names, space-separated: the probe's
# list without modprobe and rmmod, which the load test exists to run.
load_forbidden_commands() {
  printf '%s\n' "$1" \
    | grep -owE "$NODE_CHANGING_COMMANDS" \
    | sort -u | tr '\n' ' ' || true
}

# The load script without its two permitted redirections, 2>/dev/null and
# 2>&1, which discard or capture an error message: any `>` left in it
# redirects something else.
without_stderr_redirects() {
  local script="${1//2>\/dev\/null/}"
  printf '%s\n' "${script//2>&1/}"
}

# --- Test 6: the load test is a separate, bounded Job ---
test_load_test_job_is_separate_and_bounded() {
  echo "Test: nfs-module-load.yaml is one bounded Job on host-prepare's image with /lib/modules read-only"

  load_test_ready 30 || return

  local pod='.spec.template.spec'
  local container="$pod | .containers[0]"

  assert_eq "nfs-module-load.yaml holds exactly one document" "1" \
    "$(yq -N -r 'select(. != null) | .kind' "$LOAD_MANIFEST" | grep -c .)"
  assert_eq "the document is a batch/v1 object" "batch/v1" \
    "$(rendered_value '.apiVersion' <"$LOAD_MANIFEST")"
  assert_eq "the document is a Job" "Job" \
    "$(rendered_value '.kind' <"$LOAD_MANIFEST")"
  assert_eq "the Job is named nfs-module-load" "nfs-module-load" \
    "$(rendered_value '.metadata.name' <"$LOAD_MANIFEST")"
  assert_eq "the Job lives in default" "default" \
    "$(rendered_value '.metadata.namespace' <"$LOAD_MANIFEST")"
  assert_eq "the Job never retries (backoffLimit 0)" "0" \
    "$(rendered_value '.spec.backoffLimit' <"$LOAD_MANIFEST")"
  assert_eq "the Job is removed an hour after it finishes" "3600" \
    "$(rendered_value '.spec.ttlSecondsAfterFinished' <"$LOAD_MANIFEST")"

  assert_eq "the pod never restarts" "Never" \
    "$(rendered_value "$pod | .restartPolicy" <"$LOAD_MANIFEST")"
  assert_eq "the pod does not share the host PID namespace" "false" \
    "$(rendered_value "$pod | .hostPID // false" <"$LOAD_MANIFEST")"
  assert_eq "the pod does not share the host network namespace" "false" \
    "$(rendered_value "$pod | .hostNetwork // false" <"$LOAD_MANIFEST")"
  assert_eq "the pod mounts no ServiceAccount token" "false" \
    "$(rendered_value "$pod | .automountServiceAccountToken" <"$LOAD_MANIFEST")"
  assert_eq "the pod has no init container" "0" \
    "$(rendered_value "$pod | .initContainers // [] | length" <"$LOAD_MANIFEST")"
  assert_eq "the pod has exactly one container" "1" \
    "$(rendered_value "$pod | .containers | length" <"$LOAD_MANIFEST")"

  # An empty reference fails: two empty strings are not the same image.
  local image ref_image
  image="$(rendered_value "$container | .image" <"$LOAD_MANIFEST")"
  ref_image="$(rendered_value '.spec.template.spec.initContainers[] | select(.name == "host-prepare") | .image' \
    <"$LIBVIRT_DAEMONSET")"
  if [[ -n "$ref_image" && "$image" == "$ref_image" ]]; then
    echo "  PASS: the image equals the image of host-prepare in libvirt-daemonset.yaml"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the image '$image' does not equal the image '$ref_image' of host-prepare in $LIBVIRT_DAEMONSET"
    FAIL=$((FAIL + 1))
  fi
  assert_eq "the container pulls the pinned image IfNotPresent" "IfNotPresent" \
    "$(rendered_value "$container | .imagePullPolicy" <"$LOAD_MANIFEST")"

  assert_eq "the container is privileged" "true" \
    "$(rendered_value "$container | .securityContext.privileged // false" <"$LOAD_MANIFEST")"
  assert_eq "the container's root filesystem is read-only" "true" \
    "$(rendered_value "$container | .securityContext.readOnlyRootFilesystem // false" <"$LOAD_MANIFEST")"
  assert_not_empty "the container has a memory limit" \
    "$(rendered_value "$container | .resources.limits.memory // \"\"" <"$LOAD_MANIFEST")"

  assert_eq "the pod has exactly one volume" "1" \
    "$(rendered_value "$pod | .volumes | length" <"$LOAD_MANIFEST")"
  assert_eq "the volume is the node's /lib/modules" "/lib/modules" \
    "$(rendered_value "$pod | .volumes[0].hostPath.path" <"$LOAD_MANIFEST")"
  assert_eq "the container has exactly one volume mount" "1" \
    "$(rendered_value "$container | .volumeMounts | length" <"$LOAD_MANIFEST")"
  assert_eq "the mount names the lib-modules volume" \
    "$(rendered_value "$pod | .volumes[0].name" <"$LOAD_MANIFEST")" \
    "$(rendered_value "$container | .volumeMounts[0].name" <"$LOAD_MANIFEST")"
  assert_eq "the module tree is mounted at /lib/modules" "/lib/modules" \
    "$(rendered_value "$container | .volumeMounts[0].mountPath" <"$LOAD_MANIFEST")"
  assert_eq "the module tree is mounted read-only" "true" \
    "$(rendered_value "$container | .volumeMounts[0].readOnly // false" <"$LOAD_MANIFEST")"

  local script last
  script="$(load_script)"
  assert_eq "the container runs the script with /bin/bash -c" "/bin/bash -c" \
    "$(rendered_value "$container | .command // [] | join(\" \")" <"$LOAD_MANIFEST")"
  last="$(printf '%s\n' "$script" | grep -v '^[[:space:]]*$' | tail -n 1)"
  assert_eq "the script's last command is exit 0" "exit 0" "$last"
  assert_contains "the script removes what it loaded with rmmod" "$script" "rmmod"
  assert_not_contains "the script never unloads with modprobe -r" "$script" "modprobe -r"
  assert_eq "the script names none of the forbidden commands" \
    "" "$(load_forbidden_commands "$script")"
  assert_not_contains "the script redirects nothing but stderr" \
    "$(without_stderr_redirects "$script")" ">"
}

# A stand-in node for the load script under $1, as an NFS mount leaves one:
# kvm, and nfs with the modules it uses, loaded and under sys/module, and the
# nfs and nfs4 filesystems registered. $1/uses gives, for the stubs, each
# module followed by the modules it uses, and $1/others the modules another
# pod loads during the run: vhost_net.
load_stand_in() {
  local m
  mkdir -p "$1/proc" "$1/sys/module"
  for m in kvm sunrpc grace lockd nfs; do
    echo "$m 16384 0 - Live 0x0" >>"$1/proc/modules"
    mkdir -p "$1/sys/module/$m"
  done
  printf 'nodev\tsysfs\n\text4\nnodev\tnfs\nnodev\tnfs4\n' >"$1/proc/filesystems"
  printf '%s\n' 'nfsd lockd grace sunrpc auth_rpcgss nfs_acl' 'nfs lockd grace sunrpc' \
    'nfsv4 nfs sunrpc auth_rpcgss dns_resolver' 'lockd grace sunrpc' \
    'auth_rpcgss sunrpc' 'nfs_acl sunrpc' >"$1/uses"
  echo vhost_net >"$1/others"
}

# The stub modprobe and rmmod of the load test, in the directory $1. Both act
# on the stand-in node STUB_NODE names.
write_load_stubs() {
  cat >"$1/modprobe" <<'EOF'
#!/bin/sh
# --show-depends prints the module and the modules it needs, those first; a
# load adds the ones not loaded yet. Loading nfsd also adds the modules the
# others file names and the modules they need, as another pod would between
# the load test's two snapshots. With a modprobe-fails file every call fails.
n="$STUB_NODE"
if [ -e "$n/modprobe-fails" ]; then
  echo "modprobe: FATAL: Module stub not found" >&2
  exit 1
fi
needs() {
  for d in $(sed -n "s/^$1 //p" "$n/uses"); do needs "$d"; done
  echo "$1"
}
case "$1" in
  --show-depends) needs "$2" | sed 's|.*|insmod /lib/modules/stub/kernel/&.ko.xz |' ;;
  -*) exit 1 ;;
  *)
    others=""
    if [ "$1" = nfsd ]; then
      for o in $(cat "$n/others"); do others="$others $(needs "$o")"; done
    fi
    for m in $(needs "$1") $others; do
      grep -q "^$m " "$n/proc/modules" && continue
      echo "$m 16384 0 - Live 0x0" >>"$n/proc/modules"
      mkdir -p "$n/sys/module/$m"
    done
    if [ "$1" = nfsd ]; then printf 'nodev\tnfsd\n' >>"$n/proc/filesystems"; fi
    ;;
esac
EOF
  cat >"$1/rmmod" <<'EOF'
#!/bin/sh
# Removes each named module, and refuses, as rmmod does, a name no module
# carries, such as the newline-joined list of a quoted $added, and a module
# that a loaded module still uses; it also refuses the modules the stuck file
# names.
n="$STUB_NODE"
rc=0
for m in "$@"; do
  case "$m" in
    *[!a-z0-9_]*)
      rc=1
      continue
      ;;
  esac
  busy=""
  for u in $(awk -v m="$m" '{ for (i = 2; i <= NF; i++) if ($i == m) print $1 }' "$n/uses"); do
    grep -q "^$u " "$n/proc/modules" && busy=1
  done
  if [ -n "$busy" ] || grep -qx "$m" "$n/stuck" 2>/dev/null; then
    rc=1
    continue
  fi
  grep -v "^$m " "$n/proc/modules" >"$n/modules.new"
  mv "$n/modules.new" "$n/proc/modules"
  rmdir "$n/sys/module/$m" 2>/dev/null
done
exit "$rc"
EOF
  chmod +x "$1/modprobe" "$1/rmmod"
}

# The load script run against the stand-in node $2, its /proc/modules,
# /sys/module and /proc/filesystems, with the stubs of $3 first on PATH.
run_load_on() {
  local s="${1//\/proc\/modules/$2/proc/modules}"
  s="${s//\/sys\/module/$2/sys/module}"
  STUB_NODE="$2" PATH="$3:$PATH" bash -c "${s//\/proc\/filesystems/$2/proc/filesystems}" 2>/dev/null
}

# --- Test 7: the load script reports each result and removes only its own modules ---
test_load_test_script_reports_and_completes() {
  echo "Test: the load script reports loads and failures and removes only its own modules, with stub modprobe and rmmod"

  load_test_ready 20 || return

  local script
  script="$(load_script)"

  # The stubs and the stand-in node replace modprobe, rmmod, /proc/modules,
  # /sys/module and /proc/filesystems. A script that calls modprobe or rmmod
  # by path, or kmod, would reach this machine's kernel past the stubs.
  if [[ -n "$(load_forbidden_commands "$script")" || "$(without_stderr_redirects "$script")" == *">"* ]] \
    || grep -qE '/(modprobe|rmmod)([^[:alnum:]_]|$)|(^|[^[:alnum:]_])kmod([^[:alnum:]_]|$)' <<<"$script"; then
    echo "  FAIL: the script fails the forbidden-command or redirection check, or calls modprobe or rmmod by path; not running it on this host (20 checks)"
    FAIL=$((FAIL + 20))
    return
  fi

  local tmp
  tmp="$(mktemp -d)"
  mkdir -p "$tmp/bin"
  write_load_stubs "$tmp/bin"

  # Five runs on fresh stand-ins: modprobe loads, another pod also loads
  # nfsv4 while the run loads nfsd, another pod loads nfs on a node with no
  # NFS module loaded, rmmod refuses nfs_acl, and modprobe fails.
  local loads raced client stuck fails rc_loads=0 rc_fails=0
  load_stand_in "$tmp/loads"
  loads="$(run_load_on "$script" "$tmp/loads" "$tmp/bin")" || rc_loads=$?
  load_stand_in "$tmp/raced"
  echo nfsv4 >>"$tmp/raced/others"
  raced="$(run_load_on "$script" "$tmp/raced" "$tmp/bin")"
  load_stand_in "$tmp/client"
  echo 'kvm 16384 0 - Live 0x0' >"$tmp/client/proc/modules"
  rmdir "$tmp/client/sys/module/"{sunrpc,grace,lockd,nfs}
  echo nfs >>"$tmp/client/others"
  client="$(run_load_on "$script" "$tmp/client" "$tmp/bin")"
  load_stand_in "$tmp/stuck"
  echo nfs_acl >"$tmp/stuck/stuck"
  stuck="$(run_load_on "$script" "$tmp/stuck" "$tmp/bin")"
  load_stand_in "$tmp/fails"
  : >"$tmp/fails/modprobe-fails"
  fails="$(run_load_on "$script" "$tmp/fails" "$tmp/bin")" || rc_fails=$?

  assert_eq "the script exits 0 when modprobe loads every module" "0" "$rc_loads"
  assert_eq "the script prints the four load-test headers in order" "$EXPECTED_LOAD_HEADERS" \
    "$(printf '%s\n' "$loads" | grep -E '^== ')"
  assert_eq "a module not yet loaded prints loaded, one loaded before already loaded" \
    "$(printf '%s\n' 'nfsd: loaded' 'nfs: already loaded' 'nfsv4: loaded')" \
    "$(section "$loads" '== load')"
  assert_eq "the nfsd filesystem is registered once nfsd is loaded" \
    "$(printf '%s\n' 'nfs4: registered' 'nfsd: registered')" \
    "$(section "$loads" '== filesystems')"
  assert_eq "the load added its NFS modules, neither vhost_net, which another pod loaded, nor one loaded before" \
    "$(printf '%s\n' auth_rpcgss dns_resolver nfs_acl nfsd nfsv4)" \
    "$(section "$loads" '== modules the load added')"
  assert_eq "the node keeps the modules loaded before the run and the one another pod loaded" \
    "$(printf '%s\n' grace kvm lockd nfs sunrpc vhost_net)" \
    "$(cut -d ' ' -f1 "$tmp/loads/proc/modules" | sort)"
  assert_eq "the last line confirms the module list is as before" "module list as before" \
    "$(printf '%s\n' "$loads" | tail -n 1)"

  assert_eq "nfsv4, which another pod loads while the run loads nfsd, prints already loaded" \
    "$(printf '%s\n' 'nfsd: loaded' 'nfs: already loaded' 'nfsv4: already loaded')" \
    "$(section "$raced" '== load')"
  assert_eq "the load added nfsd and its dependencies, neither nfsv4 nor dns_resolver, which another pod loaded" \
    "$(printf '%s\n' auth_rpcgss nfs_acl nfsd)" \
    "$(section "$raced" '== modules the load added')"
  assert_eq "the node keeps nfsv4 and dns_resolver, which another pod loaded, and auth_rpcgss, which nfsv4 uses" \
    "$(printf '%s\n' auth_rpcgss dns_resolver grace kvm lockd nfs nfsv4 sunrpc vhost_net)" \
    "$(cut -d ' ' -f1 "$tmp/raced/proc/modules" | sort)"
  assert_eq "auth_rpcgss, which the other pod's nfsv4 holds, is named on the last line" "still loaded: auth_rpcgss" \
    "$(printf '%s\n' "$raced" | tail -n 1 | sed 's/ *$//')"

  assert_eq "nfs, which another pod loads while the run loads nfsd, prints already loaded" \
    "$(printf '%s\n' 'nfsd: loaded' 'nfs: already loaded' 'nfsv4: loaded')" \
    "$(section "$client" '== load')"
  assert_eq "the load added nfsd, nfsv4 and their dependencies but not nfs, which nfsv4 depends on and another pod loaded" \
    "$(printf '%s\n' auth_rpcgss dns_resolver grace lockd nfs_acl nfsd nfsv4 sunrpc)" \
    "$(section "$client" '== modules the load added')"
  assert_eq "the node keeps nfs, which another pod loaded, and lockd, grace and sunrpc, which nfs uses" \
    "$(printf '%s\n' grace kvm lockd nfs sunrpc vhost_net)" \
    "$(cut -d ' ' -f1 "$tmp/client/proc/modules" | sort)"
  assert_eq "lockd, grace and sunrpc, which the other pod's nfs holds, are named on the last line" \
    "still loaded: grace lockd sunrpc" \
    "$(printf '%s\n' "$client" | tail -n 1 | sed 's/ *$//')"

  assert_eq "a module rmmod refuses is named on the last line" "still loaded: nfs_acl" \
    "$(printf '%s\n' "$stuck" | tail -n 1 | sed 's/ *$//')"

  assert_eq "the script exits 0 when modprobe fails" "0" "$rc_fails"
  assert_eq "a failed load prints modprobe's message, a module loaded before already loaded" \
    "$(printf '%s\n' 'nfsd: FAILED: modprobe: FATAL: Module stub not found' 'nfs: already loaded' \
      'nfsv4: FAILED: modprobe: FATAL: Module stub not found')" \
    "$(section "$fails" '== load')"
  assert_eq "nfsd is not registered when its load failed" \
    "$(printf '%s\n' 'nfs4: registered' 'nfsd: not registered')" \
    "$(section "$fails" '== filesystems')"
  assert_eq "a load that adds no module prints none" "none" \
    "$(section "$fails" '== modules the load added')"

  rm -rf "$tmp"
}

# --- Run ---
test_probe_files_exist_with_spdx
test_kustomization_is_self_contained
test_render_is_one_privileged_readonly_job
test_script_ends_with_exit_0_and_writes_nothing
test_script_reports_the_host_root_and_completes
test_load_test_job_is_separate_and_bounded
test_load_test_script_reports_and_completes

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
