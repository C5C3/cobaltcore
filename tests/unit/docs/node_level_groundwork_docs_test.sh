#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the node-level groundwork contract stays documented across the four
# pages that carry it:
#   - docs/reference/ci-cd/ci-workflow.md has the
#     `### Kernel-module-dependent suites` subsection and states the gating
#     rule with WITH_OVN_KERNEL_MODULES, hack/kind-config-multinode.yaml and
#     `continue-on-error: true`.
#   - docs/contributing/adding-a-new-operator.md has the
#     `## Node-level workloads` section and names the two non-Restricted
#     security-context helpers plus the access value a node-level workload
#     depends on, and why per-node state is persisted in a namespaced object
#     rather than a Node annotation.
#   - docs/reference/target-clusters.md documents what the chassis layer added
#     (daemonsets, gated with the privilegedNamespaces PodSecurity label), what
#     that label costs, and how to take it off again.
#   - docs/reference/infrastructure/e2e-deployment.md keeps KIND_CONFIG and
#     WITH_OVN_KERNEL_MODULES as rows of its environment-variable table, not
#     as passing prose mentions.
#
# Usage: bash tests/unit/docs/node_level_groundwork_docs_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

CI_DOC="$PROJECT_ROOT/docs/reference/ci-cd/ci-workflow.md"
OPERATOR_DOC="$PROJECT_ROOT/docs/contributing/adding-a-new-operator.md"
TARGET_DOC="$PROJECT_ROOT/docs/reference/target-clusters.md"
E2E_DOC="$PROJECT_ROOT/docs/reference/infrastructure/e2e-deployment.md"

for doc in "$CI_DOC" "$OPERATOR_DOC" "$TARGET_DOC" "$E2E_DOC"; do
  if [[ ! -f "$doc" ]]; then
    echo "FAIL: $doc does not exist"
    exit 1
  fi
done

# --- Test 1: ci-workflow.md carries the gating rule ---
test_ci_workflow_documents_kernel_module_gating() {
  echo "Test: ci-workflow.md documents the kernel-module gating rule"

  assert_file_contains "'### Kernel-module-dependent suites' heading present" \
    "$CI_DOC" \
    '^### Kernel-module-dependent suites'
  assert_file_contains "gating rule names WITH_OVN_KERNEL_MODULES" \
    "$CI_DOC" \
    'WITH_OVN_KERNEL_MODULES'
  assert_file_contains "gating rule names hack/kind-config-multinode.yaml" \
    "$CI_DOC" \
    'hack/kind-config-multinode\.yaml'
  assert_file_contains "gating rule names the non-blocking entry state" \
    "$CI_DOC" \
    'continue-on-error: true'
}

# --- Test 2: adding-a-new-operator.md carries the per-node pattern ---
test_operator_guide_documents_node_level_workloads() {
  echo "Test: adding-a-new-operator.md documents the node-level workload pattern"

  assert_file_contains "'## Node-level workloads' section present" \
    "$OPERATOR_DOC" \
    '^## Node-level workloads'
  assert_file_contains "posture rule names CapabilitySecurityContext" \
    "$OPERATOR_DOC" \
    'CapabilitySecurityContext'
  assert_file_contains "posture rule names PrivilegedSecurityContext" \
    "$OPERATOR_DOC" \
    'PrivilegedSecurityContext'
  # Both helpers ship in internal/common/deployment, beside
  # RestrictedSecurityContext. A reader who greps for the two names above has to
  # learn from the page which one a container takes, and where they came from.
  assert_file_contains "the two escapes are placed in the shared package" \
    "$OPERATOR_DOC" \
    'two escapes beside it'
  assert_file_contains "the escapes are dated to the chassis that added them" \
    "$OPERATOR_DOC" \
    'added with the OVN chassis'
  # The system-id is persisted in the namespaced per-node object, not in a Node
  # annotation: patch on nodes cannot be narrowed to one annotation key, so the
  # access chart grants nodes read-only. Pin the rejection so a revision that
  # reintroduces the Node annotation has to revisit the grant too.
  assert_file_contains "per-node values route the system-id to the namespaced object" \
    "$OPERATOR_DOC" \
    'into that same namespaced per-node object'
  assert_file_contains "per-node values reject the Node-annotation alternative" \
    "$OPERATOR_DOC" \
    'grants `nodes` read-only and has no `patch` opt-in at all'
  assert_file_contains "posture rule cites the privilegedNamespaces value" \
    "$OPERATOR_DOC" \
    'privilegedNamespaces'
}

# --- Test 3: target-clusters.md documents the chassis grants ---
test_target_clusters_documents_chassis_grants() {
  echo "Test: target-clusters.md documents the grants a node-level workload needs"

  assert_file_contains "per-namespace Role grant on daemonsets documented" \
    "$TARGET_DOC" \
    'daemonsets'
  assert_file_contains "privilegedNamespaces value documented" \
    "$TARGET_DOC" \
    'privilegedNamespaces'
  # What the label costs, and how to take it off again. Both are the parts an
  # operator opting in has to read before it does, and neither is enforceable
  # by the chart.
  assert_file_contains "the node-root reach of the label is stated" \
    "$TARGET_DOC" \
    'root on every schedulable node'
  assert_file_contains "the manual label removal is named" \
    "$TARGET_DOC" \
    'kubectl label namespace <ns> pod-security.kubernetes.io/enforce-'
  # createNamespaces: false is the third revocation that strands the label: the
  # chart stops rendering the Namespace while helm.sh/resource-policy: keep
  # holds the live one, so the privileged label outlives the value that asked
  # for it while the Role still grants create on workloads there. It is also the
  # mode the multi-cluster CI job installs in, so a page that enumerates only
  # uninstall and namespaces would read as the safe posture.
  assert_file_contains "the three stranding revocations are enumerated" \
    "$TARGET_DOC" \
    'The other three revocations do not'
  # The three are not equal in what they leave behind. templates/role.yaml
  # ranges over values.namespaces and carries no resource-policy annotation, so
  # only createNamespaces: false strands the label next to a Role that still
  # grants create on workloads; the other two take the Role with them. A page
  # that reads the Role as surviving all three overstates two of them.
  assert_file_contains "the stranded Role is scoped to the one revocation that keeps it" \
    "$TARGET_DOC" \
    'take the Role with them and leave only the label'
  assert_file_contains "an empty privilegedNamespaces is not read as enforcement" \
    "$TARGET_DOC" \
    'empty `privilegedNamespaces` proves nothing while'
  # No writable half on nodes anywhere in the chart.
  assert_file_not_contains "no nodePatch value is documented" \
    "$TARGET_DOC" \
    'nodePatch'
}

# --- Test 4: e2e-deployment.md keeps both knobs as table rows ---
test_e2e_deployment_lists_both_knobs_as_table_rows() {
  echo "Test: e2e-deployment.md lists KIND_CONFIG and WITH_OVN_KERNEL_MODULES as table rows"

  # Anchor on the leading backticked name in the first column so a prose
  # mention outside the environment-variable table does not satisfy the check.
  assert_file_contains "KIND_CONFIG has an environment-variable table row" \
    "$E2E_DOC" \
    '^| `KIND_CONFIG` |'
  assert_file_contains "WITH_OVN_KERNEL_MODULES has an environment-variable table row" \
    "$E2E_DOC" \
    '^| `WITH_OVN_KERNEL_MODULES` |'
}

# --- Run ---
test_ci_workflow_documents_kernel_module_gating
test_operator_guide_documents_node_level_workloads
test_target_clusters_documents_chassis_grants
test_e2e_deployment_lists_both_knobs_as_table_rows

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
