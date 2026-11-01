#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the OpenBao section of hack/ci-dump-diagnostics.sh (#1580). The shared
# OpenBao runs in shared-services, which the namespace dump never reads, so the
# dump reads openbao-0 by name and guards the read on a `kubectl get pod`:
#   - no pod (a stack without OpenBao): the heading and a SKIP line that
#     carries kubectl's reason, no log call;
#   - pod present: the heading and the server log, placed between the
#     chaos-daemon block and the K-ORC block;
#   - pod present but its log unreadable: kubectl's error is printed and the
#     dump still exits 0;
#   - pod present with an empty log: the K-ORC heading follows the OpenBao
#     heading directly.
#
# Usage: bash tests/unit/hack/ci_dump_diagnostics_openbao_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DUMP_SH="$PROJECT_ROOT/hack/ci-dump-diagnostics.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

OPENBAO_HEADING="=== OpenBao server log (last 200 lines) ==="
KORC_HEADING="=== K-ORC controller logs (last 200 lines) ==="
LOGS_ERROR='Error from server (BadRequest): container "openbao" in pod "openbao-0" is waiting to start: PodInitializing'
STUB_MARKER="[kubectl-stub] unexpected invocation"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# write_kubectl_stub <dir> <pod_exit> <logs_mode>
# Creates a kubectl shim in <dir> that answers only the verbs the dump script
# invokes with OPERATOR unset:
#   - `get pod openbao-0 -n shared-services` → exits <pod_exit>, with kubectl's
#     NotFound text on stderr when non-zero
#   - `logs -n shared-services openbao-0 --tail=200 --all-containers` →
#     <logs_mode>: "ok" prints STUBBED-OPENBAO-LOGS, "empty" prints nothing,
#     "error" prints the BadRequest error on stderr and exits 1
#   - `get deployment orc-controller-manager -n orc-system` → exits 1 with
#     kubectl's NotFound on stderr (no K-ORC on this cluster)
#   - `get ns chaos-mesh` and `get --raw` → exit 1; the other infrastructure
#     verbs and `api-resources` → exit 0 silently
# Any other invocation prints `[kubectl-stub] unexpected invocation: …` on
# stderr and exits 64.
write_kubectl_stub() {
  local dir="$1" pod_exit="$2" logs_mode="$3"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<STUB
#!/bin/bash
case "\$1" in
  get)
    case "\$2" in
      pod)
        if [ "\$3" = "openbao-0" ] && [ "\$4" = "-n" ] \\
            && [ "\$5" = "shared-services" ]; then
          if [ ${pod_exit} -ne 0 ]; then
            echo 'Error from server (NotFound): pods "openbao-0" not found' >&2
          fi
          exit ${pod_exit}
        fi
        ;;
      deployment)
        if [ "\$3" = "orc-controller-manager" ] && [ "\$4" = "-n" ] \\
            && [ "\$5" = "orc-system" ]; then
          echo 'Error from server (NotFound): deployments.apps "orc-controller-manager" not found' >&2
          exit 1
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
    if [ "\$2" = "-n" ] && [ "\$3" = "shared-services" ] \\
        && [ "\$4" = "openbao-0" ] && [ "\$5" = "--tail=200" ] \\
        && [ "\$6" = "--all-containers" ]; then
      case "${logs_mode}" in
        ok) echo "STUBBED-OPENBAO-LOGS" ;;
        error) echo '${LOGS_ERROR}' >&2; exit 1 ;;
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

  # None of the stubbed tools may pollute the stdout the assertions read.
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
# Test A: pod absent -> heading and SKIP line with kubectl's reason
# ---------------------------------------------------------------------------
test_skip_when_pod_absent() {
  echo "Test: prints the heading and a SKIP line when openbao-0 is absent"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  write_kubectl_stub "$tmp" 1 ok

  local output exit_code
  output="$(run_dump "$tmp")"
  exit_code=$?

  assert_eq "dump script exits 0 without OpenBao" "0" "$exit_code"
  assert_contains "the SKIP line under the heading carries kubectl's NotFound" "$output" \
    "${OPENBAO_HEADING}"$'\n'"SKIP: pod/openbao-0 not readable in shared-services (Error from server (NotFound)"
  assert_not_contains "no log is read without the pod" \
    "$output" "STUBBED-OPENBAO-LOGS"
  assert_not_contains "the stub models every kubectl call the dump makes" \
    "$output" "$STUB_MARKER"
}

# ---------------------------------------------------------------------------
# Test B: pod present -> server log between chaos-daemon and K-ORC
# ---------------------------------------------------------------------------
test_logs_when_pod_present() {
  echo "Test: dumps the server log when openbao-0 is present"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  write_kubectl_stub "$tmp" 0 ok

  local output exit_code
  output="$(run_dump "$tmp")"
  exit_code=$?

  assert_eq "dump script exits 0 with OpenBao" "0" "$exit_code"
  assert_not_contains "no SKIP line when the pod is readable" \
    "$output" "SKIP: pod/openbao-0"

  local section
  section="$(sed -n '/^=== DaemonSet chaos-daemon detail ===$/,/^=== K-ORC controller logs/p' <<<"$output")"
  assert_contains "the OpenBao section sits between chaos-daemon detail and K-ORC" \
    "$section" "${OPENBAO_HEADING}"$'\n'"STUBBED-OPENBAO-LOGS"$'\n'"${KORC_HEADING}"
  assert_not_contains "the stub models every kubectl call the dump makes" \
    "$output" "$STUB_MARKER"
}

# ---------------------------------------------------------------------------
# Test C: log unreadable -> kubectl's error is printed, the dump exits 0
# ---------------------------------------------------------------------------
test_logs_error_is_printed_not_fatal() {
  echo "Test: prints kubectl's error and exits 0 when the log cannot be read"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  write_kubectl_stub "$tmp" 0 error

  local output exit_code
  output="$(run_dump "$tmp")"
  exit_code=$?

  assert_eq "dump script exits 0 when kubectl logs fails" "0" "$exit_code"
  assert_contains "the BadRequest sits under the heading, the K-ORC block after it" \
    "$output" "${OPENBAO_HEADING}"$'\n'"${LOGS_ERROR}"$'\n'"${KORC_HEADING}"
  assert_not_contains "the stub models every kubectl call the dump makes" \
    "$output" "$STUB_MARKER"
}

# ---------------------------------------------------------------------------
# Test D: empty server log -> the K-ORC heading follows directly
# ---------------------------------------------------------------------------
test_empty_log_runs_into_the_korc_heading() {
  echo "Test: an empty server log leaves the heading followed by the K-ORC section"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  write_kubectl_stub "$tmp" 0 empty

  local output exit_code
  output="$(run_dump "$tmp")"
  exit_code=$?

  assert_eq "dump script exits 0 on an empty log" "0" "$exit_code"
  assert_contains "the K-ORC heading follows the OpenBao heading directly" \
    "$output" "${OPENBAO_HEADING}"$'\n'"${KORC_HEADING}"
  assert_not_contains "the stub models every kubectl call the dump makes" \
    "$output" "$STUB_MARKER"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_skip_when_pod_absent
test_logs_when_pod_present
test_logs_error_is_printed_not_fatal
test_empty_log_runs_into_the_korc_heading

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
