#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify deploy/openbao/bootstrap/setup-database-tenant.sh fails loudly when the
# ControlPlane CR cannot be read, instead of silently falling back to the
# projection defaults (openstack-db / keystone).
#
# get_controlplane_field defaults on an empty kubectl result, which is correct
# for a genuinely-unset field but must NOT mask a missing CR or an unreachable
# cluster: without an up-front existence check the script would provision a
# database-engine tenant against the defaults for a ControlPlane that does not
# exist. This test drives the script with a kubectl stub that fails every lookup
# and asserts the ControlPlane-specific error surfaces.
#
# Usage: bash tests/unit/deploy/setup_database_tenant_missing_controlplane_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SCRIPT_UNDER_TEST="$PROJECT_ROOT/deploy/openbao/bootstrap/setup-database-tenant.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# make_failing_kubectl <dir>
# Installs a kubectl stub in <dir> that fails every invocation, simulating a
# missing ControlPlane CR (or an unreachable cluster).
make_failing_kubectl() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
# Every lookup fails: the ControlPlane CR (and everything else) is unreachable.
exit 1
STUB
  chmod +x "$dir/kubectl"
}

# run_script <stub_dir> <namespace> <controlplane>
# Runs the script with the kubectl stub prepended to PATH (so date/base64/bash
# stay available). Echoes combined stdout/stderr; returns the script exit code.
# NAMESPACE is exported to a chainsaw-style ephemeral test namespace on purpose:
# chainsaw injects NAMESPACE=<test namespace> into every e2e script step, and
# common.sh must resolve the OpenBao namespace from OPENBAO_NAMESPACE (or its
# openbao-system default) instead of picking that injected value up.
run_script() {
  local stub_dir="$1" ns="$2" cp="$3"
  (
    PATH="$stub_dir:$PATH"
    export PATH
    BAO_TOKEN="dummy-token"
    NAMESPACE="chainsaw-fresh-piranha"
    export BAO_TOKEN NAMESPACE
    bash "$SCRIPT_UNDER_TEST" "$ns" "$cp"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test: missing ControlPlane fails loudly instead of provisioning defaults
# ---------------------------------------------------------------------------
test_missing_controlplane_fails_loudly() {
  echo "Test: setup-database-tenant.sh errors on a missing ControlPlane"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_failing_kubectl "$tmp"

  local output exit_code
  output="$(run_script "$tmp" "tenant-a" "cp-a")"
  exit_code=$?

  assert_nonzero_exit "script exits non-zero when the ControlPlane is unreadable" "$exit_code"
  assert_contains "error names the missing ControlPlane, not a defaulted MariaDB" \
    "$output" "ControlPlane 'cp-a' not found in namespace 'tenant-a'"
  assert_not_contains "script does not provision a tenant against the defaults" \
    "$output" "Connection config written."
}

# ---------------------------------------------------------------------------
# Test: chainsaw-injected NAMESPACE does not redirect the OpenBao namespace
# ---------------------------------------------------------------------------
# Regression test for the e2e-controlplane failure where the chainsaw-injected
# NAMESPACE=<test namespace> leaked into common.sh and bao_exec tried
# `kubectl exec -n <test namespace> openbao-0` (pod not found). The OpenBao
# namespace must resolve from OPENBAO_NAMESPACE / its default, never from the
# generic NAMESPACE env var run_script poisons above.
test_injected_namespace_is_ignored() {
  echo "Test: setup-database-tenant.sh ignores a chainsaw-injected NAMESPACE"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_failing_kubectl "$tmp"

  local output
  output="$(run_script "$tmp" "tenant-a" "cp-a")"

  assert_contains "OpenBao namespace stays on the openbao-system default" \
    "$output" "Namespace : openbao-system"
  assert_not_contains "the injected test namespace never becomes the exec target" \
    "$output" "Namespace : chainsaw-fresh-piranha"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_missing_controlplane_fails_loudly
test_injected_namespace_is_ignored

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
