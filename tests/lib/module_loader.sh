#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Shared helpers for the tests that run the module load scripts of the
# metal-stack lab overlays against a stub modprobe
# (tests/unit/deploy/metal_stack_nfs_test.sh,
# tests/unit/deploy/metal_stack_chaos_mesh_test.sh).
#
# unsafe_load_script is what keeps such a test from loading modules into this
# machine's kernel. Kept here so a change to it, or to the stubs, is edited
# once instead of once per test.
#
# Source this after tests/lib/assertions.sh. The sourcing test declares PASS,
# FAIL, LIBVIRT_DAEMONSET and STUB_KERNEL.

# host_prepare_image — the image of host-prepare in libvirt-daemonset.yaml.
host_prepare_image() {
  yq -N -r 'select(. != null) | .spec.template.spec.initContainers[] | select(.name == "host-prepare") | .image' \
    "$LIBVIRT_DAEMONSET" | head -n 1
}

# assert_same_nonempty <description> <value> <reference value>
# An empty string on either side fails: two empty images are not one image.
assert_same_nonempty() {
  if [[ -n "$2" && -n "$3" && "$2" == "$3" ]]; then
    echo "  PASS: $1"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $1 (got '$2', want '$3')"
    FAIL=$((FAIL + 1))
  fi
}

# write_stubs <dir>
# A modprobe that appends its arguments to $MODPROBE_LOG and fails, as modprobe
# does, for a module named in $MODPROBE_FAIL, and a uname that reports
# STUB_KERNEL.
write_stubs() {
  mkdir -p "$1"
  cat >"$1/modprobe" <<'EOF'
#!/bin/sh
echo "$*" >>"$MODPROBE_LOG"
case " ${MODPROBE_FAIL:-} " in
  *" $1 "*)
    echo "modprobe: FATAL: Module $1 not found in directory /lib/modules/stub" >&2
    exit 1
    ;;
esac
exit 0
EOF
  printf '#!/bin/sh\necho %s\n' "$STUB_KERNEL" >"$1/uname"
  chmod +x "$1/modprobe" "$1/uname"
}

# run_script <script> <stub dir> <log> [failing modules]
# Runs <script> with bash -c, the stubs first on PATH. Prints its stdout;
# returns its exit status.
run_script() {
  : >"$3"
  MODPROBE_LOG="$3" MODPROBE_FAIL="${4:-}" PATH="$2:$PATH" bash -c "$1" 2>/dev/null
}

# unsafe_load_script <script>
# True when <script> would reach this machine's kernel past the stubs: it
# calls modprobe by path, or kmod, insmod or rmmod.
unsafe_load_script() {
  grep -qE '/modprobe([^[:alnum:]_]|$)|(^|[^[:alnum:]_])(kmod|insmod|rmmod)([^[:alnum:]_]|$)' <<<"$1"
}
