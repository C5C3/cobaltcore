#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify deploy/openbao/bootstrap/setup-eso-tenant.sh fails loudly when the
# tenant namespace cannot be read, instead of applying the ServiceAccount /
# Certificate / SecretStore into a missing namespace (which would surface a less
# actionable error later). Mirrors setup_database_tenant_missing_controlplane_test.sh.
#
# Usage: bash tests/unit/deploy/setup_eso_tenant_missing_namespace_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SCRIPT_UNDER_TEST="$PROJECT_ROOT/deploy/openbao/bootstrap/setup-eso-tenant.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# make_failing_kubectl <dir>
# Installs a kubectl stub in <dir> that fails every invocation, simulating a
# missing namespace (or an unreachable cluster).
make_failing_kubectl() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
# Every lookup fails: the namespace (and everything else) is unreachable.
exit 1
STUB
  chmod +x "$dir/kubectl"
}

# run_script <stub_dir> <namespace>
# Runs the script with the kubectl stub prepended to PATH. Echoes combined
# stdout/stderr; returns the script exit code.
run_script() {
  local stub_dir="$1" ns="$2"
  (
    PATH="$stub_dir:$PATH"
    export PATH
    bash "$SCRIPT_UNDER_TEST" "$ns"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test: missing namespace fails loudly instead of applying manifests
# ---------------------------------------------------------------------------
test_missing_namespace_fails_loudly() {
  echo "Test: setup-eso-tenant.sh errors on a missing namespace"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_failing_kubectl "$tmp"

  local output exit_code
  output="$(run_script "$tmp" "tenant-a")"
  exit_code=$?

  assert_nonzero_exit "script exits non-zero when the namespace is unreadable" "$exit_code"
  assert_contains "error names the missing namespace" \
    "$output" "namespace 'tenant-a' not found"
  assert_not_contains "script does not apply the SecretStore against a missing namespace" \
    "$output" "Applying SecretStore"
}

# ---------------------------------------------------------------------------
# Test: usage error when no namespace argument is given
# ---------------------------------------------------------------------------
test_missing_argument_fails() {
  echo "Test: setup-eso-tenant.sh errors when no namespace argument is given"

  local output exit_code
  output="$(bash "$SCRIPT_UNDER_TEST" 2>&1)"
  exit_code=$?

  assert_nonzero_exit "script exits non-zero without a namespace argument" "$exit_code"
  assert_contains "error shows the usage line" \
    "$output" "usage: setup-eso-tenant.sh <namespace>"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_missing_namespace_fails_loudly
test_missing_argument_fails

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
