#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the audited kind cluster: hack/kind-config-audit.yaml and the audit
# policy it mounts, hack/audit-policy.yaml.
#   - the port mappings of hack/kind-config.yaml and the audit config are equal,
#     and the comparison fails for a copy of the audit config with one mapping
#     dropped;
#   - the kubeadm patch passes the four audit extraArgs and mounts the two
#     extraVolumes, and the policy file reaches the node through extraMounts at
#     the path audit-policy-file names;
#   - the policy logs ServiceAccount requests at Metadata, everything else not
#     at all, and omits the RequestReceived stage;
#   - hack/ci-rbac-usage-report.sh reads the log directory and the rotation
#     limit the config sets.
#
# Uses `yq` so that formatting changes do not break the test. Skipped when
# `yq` is not installed.
#
# Usage: bash tests/unit/hack/kind_config_audit_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

KIND_CONFIG_FILE="$PROJECT_ROOT/hack/kind-config.yaml"
AUDIT_CONFIG_FILE="$PROJECT_ROOT/hack/kind-config-audit.yaml"
POLICY_FILE="$PROJECT_ROOT/hack/audit-policy.yaml"
REPORT_SCRIPT="$PROJECT_ROOT/hack/ci-rbac-usage-report.sh"

# port_mappings_equal <a> <b> — succeed when the control-plane node of both kind
# configs carries the same extraPortMappings.
port_mappings_equal() {
  [ "$(yq -o=json '.nodes[0].extraPortMappings' "$1")" = "$(yq -o=json '.nodes[0].extraPortMappings' "$2")" ]
}

# patch <expression> — evaluate expression on the ClusterConfiguration patch,
# which kind takes as a YAML document inside a string.
patch() {
  yq '.nodes[0].kubeadmConfigPatches[0]' "$AUDIT_CONFIG_FILE" | yq "$1"
}

# extra_arg <name> — the value of one apiServer extraArgs entry.
extra_arg() {
  patch ".apiServer.extraArgs[] | select(.name == \"$1\") | .value"
}

# --- Test 1: the port mappings of the two configs are equal ---
test_port_mappings() {
  echo "Test: the audit config carries hack/kind-config.yaml's port mappings"

  if port_mappings_equal "$KIND_CONFIG_FILE" "$AUDIT_CONFIG_FILE"; then
    assert_eq "the extraPortMappings of both configs are equal" "equal" "equal"
  else
    assert_eq "the extraPortMappings of both configs are equal" "equal" "different"
  fi
  assert_eq "the audit config has one node, the control-plane" "control-plane" \
    "$(yq '[.nodes[].role] | join(",")' "$AUDIT_CONFIG_FILE")"

  # The comparison itself has to see a dropped mapping, or the check above
  # passes for any pair of configs.
  local tmp
  tmp="$(mktemp -d)"
  yq 'del(.nodes[0].extraPortMappings[1])' "$AUDIT_CONFIG_FILE" >"$tmp/dropped.yaml"
  if port_mappings_equal "$KIND_CONFIG_FILE" "$tmp/dropped.yaml"; then
    assert_eq "a copy with one port mapping dropped fails the comparison" "different" "equal"
  else
    assert_eq "a copy with one port mapping dropped fails the comparison" "different" "different"
  fi
  rm -rf "$tmp"
}

# --- Test 2: the kubeadm patch and the mount ---
test_audit_patch() {
  echo "Test: the kubeadm patch enables the audit log and mounts the policy"

  assert_eq "the patch targets the ClusterConfiguration" "ClusterConfiguration" "$(patch '.kind')"
  assert_eq "audit-log-path" "/var/log/kubernetes/kube-apiserver-audit.log" "$(extra_arg audit-log-path)"
  assert_eq "audit-policy-file" "/etc/kubernetes/policies/audit-policy.yaml" "$(extra_arg audit-policy-file)"
  assert_eq "audit-log-maxsize (MB)" "100" "$(extra_arg audit-log-maxsize)"
  assert_eq "audit-log-maxbackup" "10" "$(extra_arg audit-log-maxbackup)"

  assert_eq "the policies volume: hostPath, mountPath, readOnly, pathType" \
    "/etc/kubernetes/policies /etc/kubernetes/policies true DirectoryOrCreate" \
    "$(patch '.apiServer.extraVolumes[] | select(.name == "audit-policies") | [.hostPath, .mountPath, .readOnly, .pathType] | join(" ")')"
  assert_eq "the log volume: hostPath, mountPath, readOnly, pathType" \
    "/var/log/kubernetes /var/log/kubernetes false DirectoryOrCreate" \
    "$(patch '.apiServer.extraVolumes[] | select(.name == "audit-logs") | [.hostPath, .mountPath, .readOnly, .pathType] | join(" ")')"

  assert_eq "the node mounts hack/audit-policy.yaml read-only" \
    "hack/audit-policy.yaml /etc/kubernetes/policies/audit-policy.yaml true" \
    "$(yq '.nodes[0].extraMounts[] | [.hostPath, .containerPath, .readOnly] | join(" ")' "$AUDIT_CONFIG_FILE")"
  assert_eq "audit-policy-file names the mounted file" \
    "$(yq '.nodes[0].extraMounts[0].containerPath' "$AUDIT_CONFIG_FILE")" "$(extra_arg audit-policy-file)"
  assert_contains "the log lands in the writable volume" \
    "$(extra_arg audit-log-path)" "$(patch '.apiServer.extraVolumes[] | select(.name == "audit-logs") | .mountPath')/"
}

# --- Test 3: the policy ---
test_policy() {
  echo "Test: hack/audit-policy.yaml logs ServiceAccounts at Metadata and nothing else"

  assert_eq "it is an audit.k8s.io/v1 Policy" "audit.k8s.io/v1 Policy" \
    "$(yq '[.apiVersion, .kind] | join(" ")' "$POLICY_FILE")"
  assert_eq "RequestReceived is omitted" "RequestReceived" "$(yq '.omitStages | join(",")' "$POLICY_FILE")"
  assert_eq "it has two rules" "2" "$(yq '.rules | length' "$POLICY_FILE")"
  assert_eq "the first rule logs Metadata" "Metadata" "$(yq '.rules[0].level' "$POLICY_FILE")"
  assert_eq "for the ServiceAccounts group" "system:serviceaccounts" \
    "$(yq '.rules[0].userGroups | join(",")' "$POLICY_FILE")"
  assert_eq "without managed fields" "true" "$(yq '.rules[0].omitManagedFields' "$POLICY_FILE")"
  assert_eq "the last rule logs nothing" "None" "$(yq '.rules[-1].level' "$POLICY_FILE")"
  assert_eq "for every request" "level" "$(yq '.rules[-1] | keys | join(",")' "$POLICY_FILE")"
}

# --- Test 4: the report script reads what the config writes ---
test_report_script() {
  echo "Test: hack/ci-rbac-usage-report.sh matches the log path and rotation limit"

  assert_file_contains_fixed "it reads the audit log directory" "$REPORT_SCRIPT" \
    "AUDIT_LOG_DIR=\"$(dirname "$(extra_arg audit-log-path)")\""
  assert_file_contains_fixed "it warns at the configured rotation limit" "$REPORT_SCRIPT" \
    "AUDIT_LOG_MAXBACKUP=$(extra_arg audit-log-maxbackup)"
}

# --- Run ---
if ! command -v yq >/dev/null 2>&1; then
  echo "SKIP: yq not installed (every check needs it)"
  SKIP=$((SKIP + 1))
else
  test_port_mappings
  test_audit_patch
  test_policy
  test_report_script
fi

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
