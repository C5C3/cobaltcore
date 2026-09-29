#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the metal-stack node probe, the read-only survey Job of #1138:
#   1. deploy/lab/metal-stack/probe/{kustomization,node-probe}.yaml exist with
#      SPDX headers.
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
#   4. The container's script reads the NIC list from the host's sysfs, ends
#      with exit 0, names none of nsenter, chroot, modprobe, insmod, rmmod,
#      mount, umount, sysctl, apt-get, apt, tee, dd, mknod and rm, and
#      redirects nothing but stderr to /dev/null.
#   5. Run against an empty stand-in for the host root, the script exits 0 and
#      prints the ten headers in order with its absent and NOT FOUND
#      fallbacks; against a populated one it reports a module file, a module
#      built into the kernel, the NFS server binary and a NIC. A script that
#      fails the command or redirection check of 4 is not run.
#
# Checks 3 to 5 are counted as SKIP when kustomize or yq is not on PATH; a
# failing kustomize build counts them as FAIL and prints the build's error.
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

IMAGE_PATTERN='^docker\.io/library/debian:[^@]+@sha256:[a-f0-9]{64}$'

EXPECTED_HEADERS="$(printf '%s\n' \
  '== kvm device' \
  '== cpu' \
  '== loaded modules' \
  '== module files for' \
  '== nested / iommu' \
  '== memory' \
  '== disks' \
  '== cgroup' \
  '== host os / binaries' \
  '== nics')"

RENDERED=""

have() {
  command -v "$1" >/dev/null 2>&1
}

# The first value a yq expression yields over the rendered probe on stdin.
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
  echo "Test: deploy/lab/metal-stack/probe/{kustomization,node-probe}.yaml exist with SPDX headers"

  local f
  for f in "$PROBE_KUSTOMIZATION" "$PROBE_MANIFEST"; do
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

# The forbidden commands a script names, space-separated. Whole words, so
# `/proc/mounts` and `MOUNTPOINT` do not count, but `$(mount)` and `mount;` do.
forbidden_commands() {
  printf '%s\n' "$1" \
    | grep -owE 'nsenter|chroot|modprobe|insmod|rmmod|mount|umount|sysctl|apt-get|apt|tee|dd|mknod|rm' \
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

  render_probe 7 || return

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
}

# --- Test 5: the script completes on any node and reports what it finds ---
test_script_reports_the_host_root_and_completes() {
  echo "Test: the probe script exits 0 on an empty host root and reports a populated one"

  render_probe 9 || return

  local script
  script="$(rendered_script)"

  # Only /host is swapped for a stand-in: every other path the script touches
  # is this machine's own, outside the pod's read-only mounts.
  if [[ -n "$(forbidden_commands "$script")" || "$(without_stderr_discard "$script")" == *">"* ]]; then
    echo "  FAIL: the script fails the forbidden-command or redirection check; not running it on this host (9 checks)"
    FAIL=$((FAIL + 9))
    return
  fi

  # Two stand-ins for the host root the pod mounts at /host: an empty one, and
  # one with a module file, a built-in module, the NFS server binary and a NIC.
  local tmp kver
  tmp="$(mktemp -d)"
  kver="$(uname -r)"
  mkdir -p "$tmp/empty" "$tmp/node/lib/modules/$kver/kernel/arch/x86/kvm" \
    "$tmp/node/usr/sbin" "$tmp/node/sys/class/net/lan0"
  : >"$tmp/node/lib/modules/$kver/kernel/arch/x86/kvm/kvm.ko"
  echo "kernel/drivers/net/tun.ko" >"$tmp/node/lib/modules/$kver/modules.builtin"
  : >"$tmp/node/usr/sbin/rpc.nfsd"
  echo 9000 >"$tmp/node/sys/class/net/lan0/mtu"
  echo up >"$tmp/node/sys/class/net/lan0/operstate"

  local empty node rc=0
  empty="$(sh -c "${script//\/host/$tmp/empty}" 2>/dev/null)" || rc=$?
  node="$(sh -c "${script//\/host/$tmp/node}" 2>/dev/null)"

  assert_eq "the script exits 0 on a host root that lacks everything" "0" "$rc"
  # The kernel version after `== module files for` is not part of the header.
  assert_eq "the script prints the ten headers in order" "$EXPECTED_HEADERS" \
    "$(printf '%s\n' "$empty" | grep -E '^== ' | sed -E 's/^(== module files for) .*/\1/')"
  assert_contains "a missing module file prints NOT FOUND" "$empty" "kvm: NOT FOUND"
  assert_contains "a missing binary prints absent" "$empty" "usr/sbin/libvirtd: absent"
  assert_contains "a host without NICs prints absent" "$empty" $'== nics\nabsent'

  assert_contains "a module file prints its path" \
    "$node" "kvm: $tmp/node/lib/modules/$kver/kernel/arch/x86/kvm/kvm.ko"
  assert_contains "a module built into the kernel prints builtin" "$node" "tun: builtin"
  assert_contains "an installed NFS server prints present" "$node" "usr/sbin/rpc.nfsd: present"
  assert_contains "a NIC prints its MTU and state" "$node" "lan0: mtu=9000 operstate=up"

  rm -rf "$tmp"
}

# --- Run ---
test_probe_files_exist_with_spdx
test_kustomization_is_self_contained
test_render_is_one_privileged_readonly_job
test_script_ends_with_exit_0_and_writes_nothing
test_script_reports_the_host_root_and_completes

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
