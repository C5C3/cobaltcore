#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the kind-only posture of the proving OpenBao instance:
#
#   - kustomize build deploy/kind/infrastructure renders exactly one
#     OpenBaoCluster/openbao-instance, and it renders `paused: true` — the
#     operator blind-creates the fixed-name unseal Secret on its first
#     reconcile, so it must not run before hack/deploy-infra.sh has
#     materialized and stamped the key custodied in the management OpenBao.
#   - the same build renders exactly one ExternalSecret/openbao-instance-unseal-key,
#     and that ExternalSecret is refreshPolicy: CreatedOnce. The Secret is the
#     instance's STATIC SEAL: a second, different value under a live instance
#     makes its raft store undecryptable for good, so the ExternalSecret must
#     materialize once and never converge.
#   - kustomize build deploy/flux-system/infrastructure renders ZERO of either.
#     Profile Development (one replica, no PDB, no anti-affinity), a static seal
#     whose key sits in a Secret in the shared openstack namespace, 1Gi of raft
#     storage and a cluster-scoped auth-delegator binding are a proving posture,
#     not a production one; the production instance arrives with the Barbican
#     onboarding.
#   - spec.selfInit.requests still carries exactly the pinned request names.
#     self-init is ONE-SHOT — the stanzas run only against freshly initialized
#     storage — so a request added to a CR whose PVC survived is silently never
#     applied. Pinning the list here means such an edit cannot land without
#     confronting that.
#   - the openbao-operator HelmRelease runs the operator multi-tenant with
#     namespacePodSecurityLabels.mode external, sets no WATCH_NAMESPACE (which
#     would pin the controller to one namespace), and the kind overlay carries
#     the OpenBaoTenant that admits openstack. A namespace without a tenant
#     never gets its instance reconciled.
#
# Usage: bash tests/unit/deploy/openbao_instance_overlay_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

KIND_INFRA_DIR="$PROJECT_ROOT/deploy/kind/infrastructure"
PROD_INFRA_DIR="$PROJECT_ROOT/deploy/flux-system/infrastructure"
INSTANCE_FILE="$KIND_INFRA_DIR/openbao-instance.yaml"
OPERATOR_RELEASE_FILE="$PROJECT_ROOT/deploy/flux-system/releases/openbao-operator.yaml"

# The complete permanent configuration of the instance, in declaration order.
SELF_INIT_REQUESTS="barbican_kv barbican_secretstore_policy approle_auth \
barbican_approle_role kubernetes_auth kubernetes_auth_config provisioner_policy \
provisioner_k8s_role"

# --- Test 1: the kind overlay renders the paused CR and its ExternalSecret ---
test_kind_overlay_renders_paused_instance() {
  echo "Test: kustomize build deploy/kind/infrastructure renders the paused OpenBaoCluster + its ExternalSecret"

  if ! command -v kustomize >/dev/null 2>&1 || ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: kustomize or yq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local rendered
  if ! rendered="$(kustomize build "$KIND_INFRA_DIR" 2>&1)"; then
    echo "  FAIL: kustomize build $KIND_INFRA_DIR failed:"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 4))
    return
  fi

  local cr_count paused es_count refresh_policy creation_policy
  cr_count="$(printf '%s\n' "$rendered" | yq -r \
    '[select(.kind == "OpenBaoCluster" and .metadata.name == "openbao-instance")] | length' \
    2>/dev/null | awk '{s+=$1} END {print s+0}')"
  paused="$(printf '%s\n' "$rendered" | yq -r \
    'select(.kind == "OpenBaoCluster" and .metadata.name == "openbao-instance") | .spec.paused' \
    2>/dev/null | head -n1)"
  es_count="$(printf '%s\n' "$rendered" | yq -r \
    '[select(.kind == "ExternalSecret" and .metadata.name == "openbao-instance-unseal-key")] | length' \
    2>/dev/null | awk '{s+=$1} END {print s+0}')"
  refresh_policy="$(printf '%s\n' "$rendered" | yq -r \
    'select(.kind == "ExternalSecret" and .metadata.name == "openbao-instance-unseal-key") | .spec.refreshPolicy' \
    2>/dev/null | head -n1)"
  creation_policy="$(printf '%s\n' "$rendered" | yq -r \
    'select(.kind == "ExternalSecret" and .metadata.name == "openbao-instance-unseal-key") | .spec.target.creationPolicy' \
    2>/dev/null | head -n1)"

  assert_eq "the kind overlay renders exactly one OpenBaoCluster/openbao-instance" "1" "$cr_count"
  assert_eq "the rendered instance starts paused" "true" "$paused"
  assert_eq "the kind overlay renders exactly one ExternalSecret/openbao-instance-unseal-key" \
    "1" "$es_count"
  assert_eq "the unseal-key ExternalSecret materializes once and never converges" \
    "CreatedOnce" "$refresh_policy"
  # Orphan keeps the single controller-ownerReference slot free for the
  # OpenBaoCluster. Under Owner, ESO takes it, hack/deploy-infra.sh can attach
  # no ownership proof the operator accepts — its openbao.org/owner-uid
  # annotation is denied to outside writers by the operator's own
  # ValidatingAdmissionPolicy — and the instance never adopts the custodied key.
  assert_eq "the unseal-key ExternalSecret leaves the controller ownerReference slot free" \
    "Orphan" "$creation_policy"
}

# --- Test 2: the production overlay renders neither ---
test_production_overlay_renders_no_instance() {
  echo "Test: kustomize build deploy/flux-system/infrastructure renders no proving instance"

  if ! command -v kustomize >/dev/null 2>&1 || ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: kustomize or yq not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local rendered
  if ! rendered="$(kustomize build "$PROD_INFRA_DIR" 2>&1)"; then
    echo "  FAIL: kustomize build $PROD_INFRA_DIR failed:"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 2))
    return
  fi

  local cr_count es_count
  cr_count="$(printf '%s\n' "$rendered" | yq -r \
    '[select(.kind == "OpenBaoCluster")] | length' 2>/dev/null | awk '{s+=$1} END {print s+0}')"
  es_count="$(printf '%s\n' "$rendered" | yq -r \
    '[select(.kind == "ExternalSecret" and .metadata.name == "openbao-instance-unseal-key")] | length' \
    2>/dev/null | awk '{s+=$1} END {print s+0}')"

  assert_eq "the production overlay renders zero OpenBaoCluster resources" "0" "$cr_count"
  assert_eq "the production overlay renders zero openbao-instance-unseal-key ExternalSecrets" \
    "0" "$es_count"
}

# --- Test 3: the one-shot self-init request list is pinned ---
test_self_init_request_list_is_pinned() {
  echo "Test: spec.selfInit.requests carries exactly the pinned request names"

  if [[ ! -f "$INSTANCE_FILE" ]]; then
    echo "  FAIL: $INSTANCE_FILE does not exist"
    FAIL=$((FAIL + 1))
    return
  fi
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local actual expected
  actual="$(yq -r 'select(.kind == "OpenBaoCluster") | .spec.selfInit.requests[].name' \
    "$INSTANCE_FILE" 2>/dev/null | tr '\n' ' ' | sed 's/ *$//')"
  expected="$(printf '%s' "$SELF_INIT_REQUESTS" | tr -s ' \\\n' ' ' | sed 's/ *$//')"

  assert_eq "self-init is one-shot: changing this list means recreating the instance (CR AND PVC)" \
    "$expected" "$actual"
}

# --- Test 4: the operator release runs multi-tenant, and openstack is onboarded ---
test_operator_release_runs_multi_tenant() {
  echo "Test: the openbao-operator HelmRelease runs multi-tenant and the kind overlay onboards openstack"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local mode psl_mode values
  mode="$(yq -r '.spec.values.tenancy.mode' "$OPERATOR_RELEASE_FILE" 2>/dev/null | head -n1)"
  psl_mode="$(yq -r '.spec.values.tenancy.namespacePodSecurityLabels.mode' \
    "$OPERATOR_RELEASE_FILE" 2>/dev/null | head -n1)"
  values="$(yq -r '.spec.values' "$OPERATOR_RELEASE_FILE" 2>/dev/null)"

  # The c5c3 operator projects a dedicated instance into service namespaces
  # created at runtime, which no single-tenant controller can reach.
  assert_eq "the release requests multi-tenant mode" "multi" "$mode"
  # Under the chart default (enforce) the provisioner holds update and patch on
  # namespaces cluster-wide and stamps restricted Pod Security labels onto every
  # onboarded namespace, openstack included.
  assert_eq "the provisioner is denied every Namespace mutation" "external" "$psl_mode"
  # tenancy.mode only shapes the chart's own output. The controller derives its
  # tenancy from WATCH_NAMESPACE, which no chart template sets, so any value
  # reintroduced here pins it to one namespace and strands every other one.
  assert_not_contains "no WATCH_NAMESPACE pins the controller to a single namespace" \
    "$values" "WATCH_NAMESPACE"

  if ! command -v kustomize >/dev/null 2>&1; then
    echo "  SKIP: kustomize not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local rendered tenant_target
  if ! rendered="$(kustomize build "$KIND_INFRA_DIR" 2>&1)"; then
    echo "  FAIL: kustomize build $KIND_INFRA_DIR failed:"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 1))
    return
  fi
  tenant_target="$(printf '%s\n' "$rendered" | yq -r \
    'select(.kind == "OpenBaoTenant" and .metadata.name == "openstack") | .spec.targetNamespace' \
    2>/dev/null | head -n1)"

  # Without a tenant admitting the namespace the controller waits for the
  # RoleBinding tenant onboarding creates and pauses every reconcile in it,
  # silently, at V(1), leaving the OpenBaoCluster with an empty status and no
  # StatefulSet until deploy-infra's Available wait times out.
  assert_eq "the kind overlay onboards the openstack namespace" "openstack" "$tenant_target"
}

# --- Run ---
test_kind_overlay_renders_paused_instance
test_production_overlay_renders_no_instance
test_self_init_request_list_is_pinned
test_operator_release_runs_multi_tenant

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
