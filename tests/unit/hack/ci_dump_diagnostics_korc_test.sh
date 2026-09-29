#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the K-ORC section of hack/ci-dump-diagnostics.sh (#1107). The dump
# reads the controller log through deploy/orc-controller-manager, the
# Deployment hack/ci-deploy-korc.sh and the Flux path create, and guards the
# read on a `kubectl get deployment`:
#   - no Deployment (a leg without K-ORC): the heading and a SKIP line that
#     carries kubectl's reason, no log call;
#   - Deployment present: the heading and the controller log, placed between
#     the chaos-daemon block and the Events listing;
#   - Deployment present but its pod cannot start (the ImagePullBackOff an
#     expired quay tag produces): kubectl's error is printed and the dump
#     still exits 0;
#   - Deployment present with an empty log: the next heading follows the K-ORC
#     heading directly.
#
# Usage: bash tests/unit/hack/ci_dump_diagnostics_korc_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DUMP_SH="$PROJECT_ROOT/hack/ci-dump-diagnostics.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

KORC_HEADING="=== K-ORC controller logs (last 200 lines) ==="
PULL_ERROR='Error from server (BadRequest): container "manager" in pod "orc-controller-manager-0" is waiting to start: trying and failing to pull image'

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# write_kubectl_stub <dir> <deploy_exit> <logs_mode>
# Creates a kubectl shim in <dir> that answers only the verbs the dump script
# invokes with OPERATOR unset:
#   - `get deployment orc-controller-manager -n orc-system` → exits
#     <deploy_exit>, with kubectl's NotFound text on stderr when non-zero
#   - `logs -n orc-system deploy/orc-controller-manager …` → <logs_mode>
#     "ok" prints STUBBED-KORC-LOGS, "empty" prints nothing, "error" prints
#     the ImagePullBackOff error on stderr and exits 1
#   - `get ns chaos-mesh` and `get --raw` → exit 1; the other infrastructure
#     verbs and `api-resources` → exit 0 silently
# Any other invocation prints `[kubectl-stub] unexpected invocation: …` on
# stderr and exits 64.
write_kubectl_stub() {
  local dir="$1" deploy_exit="$2" logs_mode="$3"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<STUB
#!/bin/bash
case "\$1" in
  get)
    case "\$2" in
      deployment)
        if [ "\$3" = "orc-controller-manager" ] && [ "\$4" = "-n" ] \\
            && [ "\$5" = "orc-system" ]; then
          if [ ${deploy_exit} -ne 0 ]; then
            echo 'Error from server (NotFound): deployments.apps "orc-controller-manager" not found' >&2
          fi
          exit ${deploy_exit}
        fi
        ;;
      ns)
        if [ "\$3" = "chaos-mesh" ]; then
          exit 1
        fi
        ;;
      helmrelease|pods|daemonsets|events|nodes|fluxinstance,fluxreport)
        exit 0
        ;;
      --raw)
        exit 1
        ;;
    esac
    ;;
  logs)
    if [ "\$2" = "-n" ] && [ "\$3" = "orc-system" ] \\
        && [ "\$4" = "deploy/orc-controller-manager" ]; then
      case "${logs_mode}" in
        ok) echo "STUBBED-KORC-LOGS" ;;
        error) echo '${PULL_ERROR}' >&2; exit 1 ;;
      esac
      exit 0
    fi
    ;;
  api-resources)
    exit 0
    ;;
esac
echo "[kubectl-stub] unexpected invocation: \$*" >&2
exit 64
STUB
  chmod +x "$dir/kubectl"

  # Neither optional tool may pollute the stdout the assertions read.
  local tool
  for tool in flux docker jq; do
    cat >"$dir/$tool" <<'STUB'
#!/bin/bash
exit 0
STUB
    chmod +x "$dir/$tool"
  done
}

# run_dump <stub_dir>
# Executes the dump script without an OPERATOR, PATH limited to the stub dir
# plus /usr/bin and /bin. Echoes combined stdout/stderr; returns the script's
# exit code.
run_dump() {
  local stub_dir="$1"
  (
    PATH="$stub_dir:/usr/bin:/bin"
    export PATH
    OPERATOR=""
    export OPERATOR
    bash "$DUMP_SH"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test A: Deployment absent -> heading and SKIP line with kubectl's reason
# ---------------------------------------------------------------------------
test_skip_when_deployment_absent() {
  echo "Test: prints the heading and a SKIP line when the K-ORC Deployment is absent"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  write_kubectl_stub "$tmp" 1 ok

  local output exit_code
  output="$(run_dump "$tmp")"
  exit_code=$?

  assert_eq "dump script exits 0 without K-ORC" "0" "$exit_code"
  assert_contains "the K-ORC heading is printed" "$output" "$KORC_HEADING"
  assert_contains "the SKIP line carries kubectl's NotFound" "$output" \
    "SKIP: deployment/orc-controller-manager not readable in orc-system (Error from server (NotFound)"
  assert_not_contains "no log is read without the Deployment" \
    "$output" "STUBBED-KORC-LOGS"
}

# ---------------------------------------------------------------------------
# Test B: Deployment present -> controller log between chaos-daemon and Events
# ---------------------------------------------------------------------------
test_logs_when_deployment_present() {
  echo "Test: dumps the controller log when the K-ORC Deployment is present"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  write_kubectl_stub "$tmp" 0 ok

  local output exit_code
  output="$(run_dump "$tmp")"
  exit_code=$?

  assert_eq "dump script exits 0 with K-ORC" "0" "$exit_code"
  assert_contains "the controller log is printed" "$output" "STUBBED-KORC-LOGS"
  assert_not_contains "no SKIP line when the Deployment is readable" \
    "$output" "SKIP: deployment/orc-controller-manager"

  local section
  section="$(sed -n '/^=== DaemonSet chaos-daemon detail ===$/,/^=== Events (last 50) ===$/p' <<<"$output")"
  assert_contains "the K-ORC section sits between chaos-daemon detail and Events" \
    "$section" "${KORC_HEADING}"$'\n'"STUBBED-KORC-LOGS"
}

# ---------------------------------------------------------------------------
# Test C: pod cannot start -> kubectl's error is printed, the dump exits 0
# ---------------------------------------------------------------------------
test_logs_error_is_printed_not_fatal() {
  echo "Test: prints kubectl's error and exits 0 when the K-ORC pod cannot start"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  write_kubectl_stub "$tmp" 0 error

  local output exit_code
  output="$(run_dump "$tmp")"
  exit_code=$?

  assert_eq "dump script exits 0 when kubectl logs fails" "0" "$exit_code"
  assert_contains "the ImagePullBackOff error follows the K-ORC heading" \
    "$output" "${KORC_HEADING}"$'\n'"${PULL_ERROR}"
  assert_contains "the dump goes on to the Events listing" \
    "$output" "=== Events (last 50) ==="
}

# ---------------------------------------------------------------------------
# Test D: empty controller log -> the next heading follows directly
# ---------------------------------------------------------------------------
test_empty_log_runs_into_the_next_heading() {
  echo "Test: an empty controller log leaves the heading followed by the next section"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  write_kubectl_stub "$tmp" 0 empty

  local output exit_code
  output="$(run_dump "$tmp")"
  exit_code=$?

  assert_eq "dump script exits 0 on an empty log" "0" "$exit_code"
  assert_contains "the Events heading follows the K-ORC heading directly" \
    "$output" "${KORC_HEADING}"$'\n'"=== Events (last 50) ==="
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_skip_when_deployment_absent
test_logs_when_deployment_present
test_logs_error_is_printed_not_fatal
test_empty_log_runs_into_the_next_heading

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
