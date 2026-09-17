#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify OPERATOR_ONLY=1 keeps hack/ci-dump-diagnostics.sh to the sections that
# differ per operator. The nova legs of e2e-operator and e2e-chaos call the
# script once per sibling operator in one job; without the switch every pass
# repeats the cluster-wide infrastructure block and the pod and Job logs of
# ${NAMESPACE}, which are identical across the loop. That buries the operator
# evidence the loop exists for and spends the grace window the `Delete kind
# cluster` step needs when the job wall cancels the run.
#
# Usage: bash tests/unit/hack/ci_dump_diagnostics_operator_only_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DUMP_SH="$PROJECT_ROOT/hack/ci-dump-diagnostics.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# write_kubectl_stub <dir>
# A kubectl shim that answers every verb the dump script invokes. Each answer
# carries a marker the assertions below look for, so a section that ran is
# visible in stdout and a section that was skipped leaves no trace. `get jobs`
# and `get pods -n openstack` return one object name each so the loops that
# read them produce output.
write_kubectl_stub() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
case "$1" in
  get)
    case "$2" in
      helmrelease) echo "STUB-HELMRELEASES"; exit 0 ;;
      daemonsets) echo "STUB-DAEMONSETS"; exit 0 ;;
      events) echo "STUB-EVENTS"; exit 0 ;;
      nodes) exit 0 ;;
      ns) exit 1 ;;
      cm) echo "STUB-CONFIGMAPS"; exit 0 ;;
      jobs) echo "job/stub-job"; exit 0 ;;
      nova) echo "STUB-CR-STATUS"; exit 0 ;;
      pods)
        # `get pods -n <ns> -o name` drives the all-pod-logs loop; the
        # namespace-less form is the infrastructure summary.
        if [ "$3" = "-n" ]; then
          echo "pod/stub-pod"
        else
          echo "STUB-ALL-PODS"
        fi
        exit 0
        ;;
      --raw) exit 1 ;;
    esac
    ;;
  logs)
    # The operator's own log carries the operator namespace; the namespace-wide
    # loops read openstack.
    case "$3" in
      nova-system) echo "STUB-OPERATOR-LOGS" ;;
      *) echo "STUB-NAMESPACE-LOGS" ;;
    esac
    exit 0
    ;;
  describe) echo "STUB-DESCRIBE"; exit 0 ;;
  api-resources) exit 0 ;;
esac
echo "[kubectl-stub] unexpected invocation: $*" >&2
exit 64
STUB
  chmod +x "$dir/kubectl"

  # Neither optional tool may pollute the stdout the assertions read.
  for tool in flux docker jq; do
    cat >"$dir/$tool" <<'STUB'
#!/bin/bash
exit 0
STUB
    chmod +x "$dir/$tool"
  done
}

# run_dump <stub_dir> <operator_only>
# Executes the dump script for OPERATOR=nova with PATH limited to the stub dir.
run_dump() {
  local stub_dir="$1" operator_only="$2"
  (
    PATH="$stub_dir:/usr/bin:/bin"
    export PATH
    OPERATOR=nova
    OPERATOR_ONLY="$operator_only"
    export OPERATOR OPERATOR_ONLY
    bash "$DUMP_SH"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test A: unset -> every section is emitted, as every other caller expects
# ---------------------------------------------------------------------------
test_full_dump_without_the_switch() {
  echo "Test: an unset OPERATOR_ONLY still dumps every section"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  write_kubectl_stub "$tmp"

  local output exit_code
  output="$(run_dump "$tmp" "")"
  exit_code=$?

  assert_eq "dump script exits 0" "0" "$exit_code"
  assert_contains "the infrastructure block runs" "$output" "=== HelmReleases ==="
  assert_contains "the node-pressure block runs" "$output" \
    "=== Node capacity and allocated resources ==="
  assert_contains "the events block runs" "$output" "=== Events (last 50) ==="
  assert_contains "the Job descriptions run" "$output" "=== Job descriptions ==="
  assert_contains "the namespace pod logs run" "$output" \
    "=== All pod logs in openstack ==="
  assert_contains "the ConfigMap listing runs" "$output" \
    "=== ConfigMaps in openstack namespace ==="
  assert_contains "the operator sections run" "$output" "=== Operator pods ==="
  assert_contains "and the CR status with them" "$output" "=== Operator CR status ==="
}

# ---------------------------------------------------------------------------
# Test B: set -> the three operator sections alone
# ---------------------------------------------------------------------------
test_operator_only_drops_the_shared_sections() {
  echo "Test: OPERATOR_ONLY=1 keeps only the sections that differ per operator"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  write_kubectl_stub "$tmp"

  local output exit_code
  output="$(run_dump "$tmp" "1")"
  exit_code=$?

  assert_eq "dump script exits 0" "0" "$exit_code"

  # The three sections the looping caller is after.
  assert_contains "the operator pods are still dumped" "$output" \
    "=== Operator pods ==="
  assert_contains "the operator logs are still dumped" "$output" \
    "=== Operator logs ==="
  assert_contains "the operator CR status is still dumped" "$output" \
    "=== Operator CR status ==="

  # Everything that does not change with OPERATOR.
  assert_not_contains "no repeated HelmRelease listing" "$output" \
    "=== HelmReleases ==="
  assert_not_contains "no repeated node-pressure block" "$output" \
    "=== Node capacity and allocated resources ==="
  assert_not_contains "no repeated kernel-OOM block" "$output" \
    "=== Kernel OOM events on the kind node(s) ==="
  assert_not_contains "no repeated event listing" "$output" \
    "=== Events (last 50) ==="
  assert_not_contains "no repeated Job descriptions" "$output" \
    "=== Job descriptions ==="
  assert_not_contains "no repeated Job logs" "$output" \
    "=== Failed Job logs ==="
  assert_not_contains "no repeated namespace pod logs" "$output" \
    "=== All pod logs in openstack ==="
  assert_not_contains "no repeated ConfigMap listing" "$output" \
    "=== ConfigMaps in openstack namespace ==="
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_full_dump_without_the_switch
test_operator_only_drops_the_shared_sections

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
