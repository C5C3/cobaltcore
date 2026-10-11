#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the lab ControlPlane under deploy/lab/metal-stack/controlplane, the
# quick start's OVNCentral and ControlPlane applied by hand on the lab, and its
# Ceph variant under deploy/lab/metal-stack/controlplane-ceph, which Part 3 of
# the quick start applies:
#   1. kustomization.yaml, ovncentral.yaml and controlplane-lab.yaml exist with
#      SPDX headers, the kustomization lists exactly the two manifests, the
#      variant's kustomization.yaml exists with SPDX headers and lists
#      ../controlplane alone, and no .gitignore pattern matches any of the four
#      files.
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
#   6. The variant renders the same two objects in openstack, with the volume
#      backends nfs1 and ceph1 in this order, nfs1 equal to the base's, and
#      the RBD backup target cephbk: ceph1 on the pool volumes as the user
#      cinder, cephbk on backups as cinder-backup, both with the monitor
#      rook-ceph-mon-a.rook-ceph.svc.cluster.local alone, the networks
#      10.240.0.0/13 and 10.248.0.0/18 and their key Secret, ceph1 with the
#      libvirt secret UUID, and neither with an nfs block, a clusterName on
#      ceph1 or a secretUUID on cephbk. Without the backends and the backup
#      target the variant's render equals the base's.
#   7. The variant's names come from the lab manifests it relies on: the
#      secretUUID is the uuid= of ceph-secret.sh in
#      hypervisor/libvirt-configmap.yaml, each key Secret is the target of an
#      ExternalSecret of ceph/cluster/keys-pull.yaml in the namespace of the
#      ControlPlane of controlplane/controlplane-lab.yaml with the one key
#      userKey, each pool a CephBlockPool of ceph/cluster/pools.yaml, each
#      user a CephClient of ceph/cluster/clients.yaml, and each monitor
#      rook-ceph-mon-a in the namespace of the CephCluster of
#      ceph/cluster/cluster.yaml, which runs one mon.
#
# LAB_ROOT overrides the deploy/lab/metal-stack tree every check reads; the
# .gitignore part of check 1 keeps its repository-relative paths.
# QUICK_START_DOC overrides the page check 4 reads. Checks 2, 3 and 6 are
# counted as SKIP when kustomize or yq is not on PATH, checks 4 and 7 when yq
# is not, and the .gitignore part of check 1 outside a git work tree. A failing
# kustomize build counts checks 2, 3 or 6 as FAIL and prints the build's error;
# a page without a marker block fails check 4 instead of comparing against
# empty input, and a lab file check 7 reads no value from fails that check
# with the file's name instead of comparing two empty strings.
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

LAB_ROOT="${LAB_ROOT:-$PROJECT_ROOT/deploy/lab/metal-stack}"
CONTROLPLANE_DIR="$LAB_ROOT/controlplane"
CEPH_VARIANT_DIR="$LAB_ROOT/controlplane-ceph"
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

# --- Test 1: the four files, their SPDX headers and the resources lists ---
test_files_spdx_resources_and_not_ignored() {
  echo "Test: the lab ControlPlane files carry SPDX headers, are listed and are not ignored"

  local files='controlplane/kustomization.yaml controlplane/ovncentral.yaml
controlplane/controlplane-lab.yaml controlplane-ceph/kustomization.yaml'
  local name f
  for name in $files; do
    f="$LAB_ROOT/$name"
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
  if [[ -f "$CEPH_VARIANT_DIR/kustomization.yaml" ]]; then
    assert_eq "controlplane-ceph/kustomization.yaml lists ../controlplane alone" \
      "../controlplane" "$(resource_entries "$CEPH_VARIANT_DIR/kustomization.yaml")"
  else
    echo "  FAIL: no controlplane-ceph/kustomization.yaml whose resources could be read"
    FAIL=$((FAIL + 1))
  fi

  if ! have git || ! git -C "$PROJECT_ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    echo "  SKIP: not inside a git work tree (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi
  # --no-index checks the patterns even for a tracked file, which check-ignore
  # otherwise never reports: a force-added controlplane.yaml would still fail.
  local rc
  for name in $files; do
    git -C "$PROJECT_ROOT" check-ignore -q --no-index \
      "deploy/lab/metal-stack/$name"
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

# --- Test 6: the Ceph variant renders the base plus the RBD entries ---
test_ceph_variant_renders_the_ceph_backends() {
  echo "Test: kustomize build deploy/lab/metal-stack/controlplane-ceph"

  local checks=24 base tmp
  render "$CONTROLPLANE_DIR" "$checks" || return
  base="$RENDERED"
  render "$CEPH_VARIANT_DIR" "$checks" || return

  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  printf '%s\n' "$base" >"$tmp/base.yaml"
  printf '%s\n' "$RENDERED" >"$tmp/variant.yaml"

  assert_eq "the render is ControlPlane/controlplane and OVNCentral/controlplane-ovn alone" \
    "$(printf '%s\n' ControlPlane/controlplane OVNCentral/controlplane-ovn)" \
    "$(printf '%s\n' "$RENDERED" |
      yq -N -r 'select(. != null) | .kind + "/" + .metadata.name' - | sort)"
  assert_eq "OVNCentral controlplane-ovn lives in openstack" "openstack" \
    "$(val OVNCentral controlplane-ovn '.metadata.namespace')"
  assert_eq "ControlPlane controlplane lives in openstack" "openstack" \
    "$(val ControlPlane controlplane '.metadata.namespace')"

  local cinder='.spec.services.cinder'
  local ceph1="$cinder.backends[] | select(.name == \"ceph1\")"
  local backup="$cinder.backupBackend"
  assert_eq "cinder declares the volume backends nfs1 and ceph1, in this order" "nfs1 ceph1" \
    "$(val ControlPlane controlplane "$cinder.backends | map(.name) | join(\" \")")"
  local nfs1="select(.kind == \"ControlPlane\") | $cinder.backends[] | select(.name == \"nfs1\")"
  assert_eq "the variant's nfs1 equals the base's" \
    "$(normalize "$tmp/base.yaml" "$nfs1")" "$(normalize "$tmp/variant.yaml" "$nfs1")"

  local monitor='rook-ceph-mon-a.rook-ceph.svc.cluster.local' networks='10.240.0.0/13 10.248.0.0/18'
  assert_eq "ceph1 is of type RBD" "RBD" "$(val ControlPlane controlplane "$ceph1 | .type")"
  assert_eq "ceph1 keeps its volumes in the pool volumes" "volumes" \
    "$(val ControlPlane controlplane "$ceph1 | .rbd.pool")"
  assert_eq "ceph1 authenticates as the user cinder" "cinder" \
    "$(val ControlPlane controlplane "$ceph1 | .rbd.user")"
  assert_eq "ceph1 names the monitor $monitor alone" "$monitor" \
    "$(val ControlPlane controlplane "$ceph1 | .rbd.monitors | join(\" \")")"
  assert_eq "ceph1 names the networks $networks" "$networks" \
    "$(val ControlPlane controlplane "$ceph1 | .rbd.networks | join(\" \")")"
  assert_eq "ceph1 reads its key from the Secret ceph-client-cinder" "ceph-client-cinder" \
    "$(val ControlPlane controlplane "$ceph1 | .rbd.keySecretRef.name")"
  assert_eq "ceph1 names the lab's libvirt secret UUID" "090e4a3c-6c20-4e74-82dc-1a70382babe8" \
    "$(val ControlPlane controlplane "$ceph1 | .rbd.secretUUID")"
  assert_eq "ceph1 sets no clusterName, so the satellite's default applies" "false" \
    "$(val ControlPlane controlplane "$ceph1 | .rbd | has(\"clusterName\")")"
  assert_eq "ceph1 carries no nfs block" "false" \
    "$(val ControlPlane controlplane "$ceph1 | has(\"nfs\")")"

  assert_eq "the backup target is cephbk" "cephbk" \
    "$(val ControlPlane controlplane "$backup.name")"
  assert_eq "cephbk is of type RBD" "RBD" "$(val ControlPlane controlplane "$backup.type")"
  assert_eq "cephbk writes to the pool backups" "backups" \
    "$(val ControlPlane controlplane "$backup.rbd.pool")"
  assert_eq "cephbk authenticates as the user cinder-backup" "cinder-backup" \
    "$(val ControlPlane controlplane "$backup.rbd.user")"
  assert_eq "cephbk names ceph1's monitors" \
    "$(val ControlPlane controlplane "$ceph1 | .rbd.monitors | join(\" \")")" \
    "$(val ControlPlane controlplane "$backup.rbd.monitors | join(\" \")")"
  assert_eq "cephbk names ceph1's networks" \
    "$(val ControlPlane controlplane "$ceph1 | .rbd.networks | join(\" \")")" \
    "$(val ControlPlane controlplane "$backup.rbd.networks | join(\" \")")"
  assert_eq "cephbk reads its key from the Secret ceph-client-cinder-backup" "ceph-client-cinder-backup" \
    "$(val ControlPlane controlplane "$backup.rbd.keySecretRef.name")"
  assert_eq "cephbk carries no nfs block" "false" \
    "$(val ControlPlane controlplane "$backup | has(\"nfs\")")"
  assert_eq "cephbk carries no secretUUID" "false" \
    "$(val ControlPlane controlplane "$backup.rbd | has(\"secretUUID\")")"

  local rest="del($cinder.backends) | del($cinder.backupBackend)"
  assert_eq "without the backends and the backup target the variant equals the base" \
    "$(normalize "$tmp/base.yaml" "$rest")" "$(normalize "$tmp/variant.yaml" "$rest")"
}

# readable <value> <what> <file>
# Returns 0 when <value> is not empty. Otherwise records one FAIL that names
# <what> and <file> relative to LAB_ROOT, and returns 1, so a check never
# compares two empty strings.
readable() {
  [[ -n "$1" ]] && return 0
  echo "  FAIL: could not read $2 from ${3#"$LAB_ROOT"/}"
  FAIL=$((FAIL + 1))
  return 1
}

# patch_field <patch> <entry> <expression>
# <expression> on the value of the operation of the JSON patch <patch> whose
# value is the entry named <entry>, empty when there is none.
patch_field() {
  yq -r ".[] | select(.value.name == \"$2\") | .value.$3 // \"\"" - <<<"$1" 2>/dev/null
}

# --- Test 7: the variant's names come from the lab manifests ---
test_ceph_variant_names_match_the_lab() {
  echo "Test: the Ceph variant names the lab's Ceph objects and libvirt secret"

  if ! have yq; then
    echo "  SKIP: yq not installed (10 checks skipped)"
    SKIP=$((SKIP + 10))
    return
  fi

  local variant="$CEPH_VARIANT_DIR/kustomization.yaml"
  local configmap="$LAB_ROOT/hypervisor/libvirt-configmap.yaml"
  local keys="$LAB_ROOT/ceph/cluster/keys-pull.yaml"
  local pools="$LAB_ROOT/ceph/cluster/pools.yaml"
  local clients="$LAB_ROOT/ceph/cluster/clients.yaml"
  local cluster="$LAB_ROOT/ceph/cluster/cluster.yaml"
  local patch
  patch="$(yq -r '.patches[] | select(.target.kind == "ControlPlane") | .patch // ""' "$variant" 2>/dev/null)"

  local uuid lab_uuid
  uuid="$(patch_field "$patch" ceph1 rbd.secretUUID)"
  lab_uuid="$(yq -r 'select(.kind == "ConfigMap") | .data["ceph-secret.sh"] // ""' "$configmap" 2>/dev/null |
    sed -n 's/^[[:space:]]*uuid=//p' | head -n 1)"
  if readable "$uuid" "ceph1's secretUUID" "$variant" &&
    readable "$lab_uuid" "the uuid= line of ceph-secret.sh" "$configmap"; then
    assert_eq "ceph1's secretUUID is the uuid= of ceph-secret.sh in hypervisor/libvirt-configmap.yaml" \
      "$lab_uuid" "$uuid"
  fi

  # Cinder reads keySecretRef in the ControlPlane's namespace, and an
  # ExternalSecret writes its target into its own.
  local entry secret key_names pool pool_names user client_names controlplane_namespace
  controlplane_namespace="$(yq -r 'select(.kind == "ControlPlane") | .metadata.namespace // ""' \
    "$CONTROLPLANE_DIR/controlplane-lab.yaml" 2>/dev/null)"
  for entry in ceph1 cephbk; do
    secret="$(patch_field "$patch" "$entry" rbd.keySecretRef.name)"
    if readable "$secret" "${entry}'s keySecretRef" "$variant" &&
      readable "$controlplane_namespace" "the ControlPlane's namespace" "$CONTROLPLANE_DIR/controlplane-lab.yaml"; then
      key_names="$(yq -r "select(.kind == \"ExternalSecret\" and .metadata.namespace == \"$controlplane_namespace\" and .spec.target.name == \"$secret\") | .spec.data[].secretKey" \
        "$keys" 2>/dev/null)"
      if readable "$key_names" "the data keys of an ExternalSecret in $controlplane_namespace that writes $secret" "$keys"; then
        assert_eq "${entry}'s Secret $secret is written by an ExternalSecret of ceph/cluster/keys-pull.yaml in $controlplane_namespace with the one key userKey" \
          "userKey" "$key_names"
      fi
    fi
  done

  pool_names="$(yq -r 'select(.kind == "CephBlockPool") | .metadata.name // ""' "$pools" 2>/dev/null)"
  client_names="$(yq -r 'select(.kind == "CephClient") | .metadata.name // ""' "$clients" 2>/dev/null)"
  for entry in ceph1 cephbk; do
    pool="$(patch_field "$patch" "$entry" rbd.pool)"
    if readable "$pool" "${entry}'s pool" "$variant" &&
      readable "$pool_names" "the CephBlockPool names" "$pools"; then
      if grep -qxF -- "$pool" <<<"$pool_names"; then
        echo "  PASS: ${entry}'s pool $pool is a CephBlockPool of ceph/cluster/pools.yaml"
        PASS=$((PASS + 1))
      else
        echo "  FAIL: ${entry}'s pool $pool is no CephBlockPool of ceph/cluster/pools.yaml ($(paste -sd' ' - <<<"$pool_names"))"
        FAIL=$((FAIL + 1))
      fi
    fi
    user="$(patch_field "$patch" "$entry" rbd.user)"
    if readable "$user" "${entry}'s user" "$variant" &&
      readable "$client_names" "the CephClient names" "$clients"; then
      if grep -qxF -- "$user" <<<"$client_names"; then
        echo "  PASS: ${entry}'s user $user is a CephClient of ceph/cluster/clients.yaml"
        PASS=$((PASS + 1))
      else
        echo "  FAIL: ${entry}'s user $user is no CephClient of ceph/cluster/clients.yaml ($(paste -sd' ' - <<<"$client_names"))"
        FAIL=$((FAIL + 1))
      fi
    fi
  done

  # Rook names the monitors a, b, c, ... and gives each a Service
  # rook-ceph-mon-<letter> in the CephCluster's namespace; with one mon the
  # only one is rook-ceph-mon-a.
  local namespace mon_count monitors
  namespace="$(yq -r 'select(.kind == "CephCluster") | .metadata.namespace // ""' "$cluster" 2>/dev/null)"
  mon_count="$(yq -r 'select(.kind == "CephCluster") | .spec.mon.count // ""' "$cluster" 2>/dev/null)"
  for entry in ceph1 cephbk; do
    monitors="$(patch_field "$patch" "$entry" 'rbd.monitors | join(" ")')"
    if readable "$monitors" "${entry}'s monitors" "$variant" &&
      readable "$namespace" "the CephCluster's namespace" "$cluster"; then
      assert_eq "${entry}'s monitor is rook-ceph-mon-a in the CephCluster's namespace" \
        "rook-ceph-mon-a.${namespace}.svc.cluster.local" "$monitors"
    fi
  done
  if readable "$mon_count" "the CephCluster's spec.mon.count" "$cluster"; then
    assert_eq "the CephCluster runs one mon, so rook-ceph-mon-a is the only one" "1" "$mon_count"
  fi
}

# --- Run ---
test_files_spdx_resources_and_not_ignored
test_render_two_objects
test_lab_settings
test_no_drift_from_the_quick_start
test_comment_states_the_tenant_mtu
test_ceph_variant_renders_the_ceph_backends
test_ceph_variant_names_match_the_lab

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
