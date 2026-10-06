#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the lab ControlPlane under deploy/lab/metal-stack/controlplane, the
# quick start's OVNCentral and ControlPlane applied by hand on the lab:
#   1. kustomization.yaml, ovncentral.yaml and controlplane-lab.yaml exist with
#      SPDX headers, the kustomization lists exactly the two manifests, and no
#      .gitignore pattern matches any of the three files.
#   2. The render is two objects, OVNCentral controlplane-ovn and ControlPlane
#      controlplane, both in openstack.
#   3. The rendered ControlPlane carries the lab settings: global_physnet_mtu
#      as the string "1460", an empty hypervisorOperator map, the Minimal
#      profile, the Cinder volume backend nfs1 on the share /volumes and the
#      backup backend nfsbk on /backups of the lab NFS server, and no
#      profileRef, metadataGateway or storageClassName.
#   4. The two files do not drift from docs/quick-start-controlplane.md:
#      controlplane-lab.yaml without the cinder block and the two lab keys
#      equals the first `# controlplane.yaml` block, its cinder block equals
#      the `# block-storage.yaml` fragment, and ovncentral.yaml equals the
#      `# controlplane-ovn.yaml` block, compared without comments and with
#      sorted keys.
#   5. controlplane-lab.yaml states the tenant MTU 1402 the setting yields.
#
# QUICK_START_DOC overrides the page check 4 reads. Checks 2 and 3 are counted
# as SKIP when kustomize or yq is not on PATH, check 4 when yq is not, and the
# .gitignore part of check 1 outside a git work tree. A failing kustomize build
# counts checks 2 and 3 as FAIL and prints the build's error; a page without a
# marker block fails check 4 instead of comparing against empty input.
#
# Usage: bash tests/unit/deploy/metal_stack_controlplane_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/kustomize_render.sh
source "$PROJECT_ROOT/tests/lib/kustomize_render.sh"

CONTROLPLANE_DIR="$PROJECT_ROOT/deploy/lab/metal-stack/controlplane"
QUICK_START_DOC="${QUICK_START_DOC:-$PROJECT_ROOT/docs/quick-start-controlplane.md}"

RENDERED=""

# extract_block <marker> <doc>
# Prints the first fenced yaml block of <doc> whose first line is <marker>,
# without the marker. Returns 1 when no block starts with it.
extract_block() {
  awk -v marker="$1" '
    BEGIN { in_block = 0; found = 0 }
    /^```yaml[[:space:]]*$/ {
      # Peek the next line; enter the block only when it is the marker.
      if ((getline next_line) > 0) {
        sub(/[[:space:]]+$/, "", next_line)
        if (next_line == marker) {
          in_block = 1
          found = 1
        }
      }
      next
    }
    in_block && /^```[[:space:]]*$/ { in_block = 0; exit }
    in_block { print }
    END { if (!found) exit 1 }
  ' "$2"
}

# normalize <file> [expression]
# <file> after <expression> (default: the document unchanged), without
# comments and with every map's keys sorted.
normalize() {
  yq -N -P "${2:-.} | ... comments=\"\" | sort_keys(..)" "$1"
}

# compare_block <tmpdir> <marker> <lab file> [expression]
# One check: <lab file> after <expression> equals the page block that starts
# with <marker>, both normalized. A page without that block is a FAIL.
compare_block() {
  local tmp="$1" marker="$2" file="$3" expression="${4:-.}" block
  block="$tmp/$(basename "$file")"
  if ! extract_block "$marker" "$QUICK_START_DOC" >"$block" || [[ ! -s "$block" ]]; then
    echo "  FAIL: could not locate the '$marker' block in $QUICK_START_DOC"
    FAIL=$((FAIL + 1))
    return
  fi
  assert_eq "$(basename "$file") matches the '$marker' block" \
    "$(normalize "$block")" "$(normalize "$file" "$expression")"
}

# --- Test 1: the three files, their SPDX headers and the resources list ---
test_files_spdx_resources_and_not_ignored() {
  echo "Test: the lab ControlPlane files carry SPDX headers, are listed and are not ignored"

  local name f
  for name in kustomization.yaml ovncentral.yaml controlplane-lab.yaml; do
    f="$CONTROLPLANE_DIR/$name"
    if [[ ! -f "$f" ]]; then
      echo "  FAIL: $f does not exist"
      FAIL=$((FAIL + 2))
      continue
    fi
    assert_file_contains "$name has SPDX FileCopyrightText header" \
      "$f" "SPDX-FileCopyrightText: Copyright 2026 SAP SE"
    assert_file_contains "$name has SPDX-License-Identifier: Apache-2.0" \
      "$f" "SPDX-License-Identifier: Apache-2.0"
  done

  if [[ -f "$CONTROLPLANE_DIR/kustomization.yaml" ]]; then
    assert_eq "kustomization.yaml lists ovncentral.yaml and controlplane-lab.yaml alone" \
      "$(printf '%s\n' ovncentral.yaml controlplane-lab.yaml)" \
      "$(resource_entries "$CONTROLPLANE_DIR/kustomization.yaml")"
  else
    echo "  FAIL: no kustomization.yaml whose resources could be read"
    FAIL=$((FAIL + 1))
  fi

  if ! have git || ! git -C "$PROJECT_ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    echo "  SKIP: not inside a git work tree (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi
  # --no-index checks the patterns even for a tracked file, which check-ignore
  # otherwise never reports: a force-added controlplane.yaml would still fail.
  local rc
  for name in kustomization.yaml ovncentral.yaml controlplane-lab.yaml; do
    git -C "$PROJECT_ROOT" check-ignore -q --no-index \
      "deploy/lab/metal-stack/controlplane/$name"
    rc=$?
    assert_eq "no .gitignore pattern matches $name" "1" "$rc"
  done
}

# --- Test 2: the render is the OVNCentral and the ControlPlane ---
test_render_two_objects() {
  echo "Test: kustomize build deploy/lab/metal-stack/controlplane"

  render "$CONTROLPLANE_DIR" 5 || return

  assert_eq "the render is ControlPlane/controlplane and OVNCentral/controlplane-ovn alone" \
    "$(printf '%s\n' ControlPlane/controlplane OVNCentral/controlplane-ovn)" \
    "$(printf '%s\n' "$RENDERED" |
      yq -N -r 'select(. != null) | .kind + "/" + .metadata.name' - | sort)"
  assert_eq "OVNCentral controlplane-ovn is rendered once" "1" \
    "$(count_named OVNCentral controlplane-ovn)"
  assert_eq "OVNCentral controlplane-ovn lives in openstack" "openstack" \
    "$(val OVNCentral controlplane-ovn '.metadata.namespace')"
  assert_eq "ControlPlane controlplane is rendered once" "1" \
    "$(count_named ControlPlane controlplane)"
  assert_eq "ControlPlane controlplane lives in openstack" "openstack" \
    "$(val ControlPlane controlplane '.metadata.namespace')"
}

# --- Test 3: the lab settings on the rendered ControlPlane ---
test_lab_settings() {
  echo "Test: the rendered ControlPlane carries the lab settings"

  render "$CONTROLPLANE_DIR" 11 || return

  local mtu='.spec.services.neutron.extraConfig.DEFAULT.global_physnet_mtu'
  assert_eq "neutron global_physnet_mtu is 1460" "1460" \
    "$(val ControlPlane controlplane "$mtu")"
  # extraConfig is map[string]map[string]string: an unquoted 1460 is an int
  # the API server rejects.
  assert_eq "neutron global_physnet_mtu is a string" '!!str' \
    "$(val ControlPlane controlplane "$mtu | tag")"
  assert_eq "nova hypervisorOperator is a map" '!!map' \
    "$(val ControlPlane controlplane '.spec.services.nova.hypervisorOperator | tag')"
  assert_eq "nova hypervisorOperator is empty" "0" \
    "$(val ControlPlane controlplane '.spec.services.nova.hypervisorOperator | length')"
  assert_eq "the sizing profile is Minimal" "Minimal" \
    "$(val ControlPlane controlplane '.spec.sizing.profile')"
  assert_eq "no SizingProfile is referenced" "false" \
    "$(val ControlPlane controlplane '.spec.sizing | has("profileRef")')"
  assert_eq "no metadata gateway is published" "false" \
    "$(val ControlPlane controlplane '.spec.services.nova | has("metadataGateway")')"
  local cinder='.spec.services.cinder' share='.nfs.server + ":" + .nfs.path'
  assert_eq "cinder declares the volume backend nfs1 alone" "nfs1" \
    "$(val ControlPlane controlplane "$cinder.backends | map(.name) | join(\" \")")"
  assert_eq "nfs1 is the share /volumes of the lab NFS server" \
    "nfs-server.openstack.svc.cluster.local:/volumes" \
    "$(val ControlPlane controlplane "$cinder.backends[] | select(.name == \"nfs1\") | $share")"
  assert_eq "the backup backend nfsbk is the share /backups of the lab NFS server" \
    "nfs-server.openstack.svc.cluster.local:/backups" \
    "$(val ControlPlane controlplane "$cinder.backupBackend | select(.name == \"nfsbk\") | $share")"
  assert_not_contains "no storage class is named, so the default class applies" \
    "$RENDERED" "storageClassName"
}

# --- Test 4: no drift from the quick start ---
test_no_drift_from_the_quick_start() {
  echo "Test: the lab files match the quick start's blocks (${QUICK_START_DOC#"$PROJECT_ROOT"/})"

  if ! have yq; then
    echo "  SKIP: yq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi
  if [[ ! -f "$QUICK_START_DOC" ]]; then
    echo "  FAIL: $QUICK_START_DOC does not exist"
    FAIL=$((FAIL + 3))
    return
  fi

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  compare_block "$tmp" '# controlplane.yaml' "$CONTROLPLANE_DIR/controlplane-lab.yaml" \
    'del(.spec.services.neutron.extraConfig) | del(.spec.services.nova.hypervisorOperator) | del(.spec.services.cinder)'
  compare_block "$tmp" '# block-storage.yaml' "$CONTROLPLANE_DIR/controlplane-lab.yaml" \
    '{"cinder": .spec.services.cinder}'
  compare_block "$tmp" '# controlplane-ovn.yaml' "$CONTROLPLANE_DIR/ovncentral.yaml"
}

# --- Test 5: the comment states the tenant MTU ---
test_comment_states_the_tenant_mtu() {
  echo "Test: controlplane-lab.yaml states the tenant MTU the setting yields"

  assert_file_contains "controlplane-lab.yaml names the tenant MTU 1402" \
    "$CONTROLPLANE_DIR/controlplane-lab.yaml" "1402"
}

# --- Run ---
test_files_spdx_resources_and_not_ignored
test_render_two_objects
test_lab_settings
test_no_drift_from_the_quick_start
test_comment_states_the_tenant_mtu

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
