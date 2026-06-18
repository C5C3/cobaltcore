#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh `wait_for_gateway_programmed` polls the Gateway
# object until its `.status.conditions[type=Programmed].status == True` and
# surfaces diagnostics (`kubectl describe` + envoy-gateway-system pod logs)
# on timeout.
#
# Follows the stub-kubectl + source-and-invoke pattern established by
# deploy_infra_reconcile_sources_test.sh.
#
# Usage: bash tests/unit/hack/deploy_infra_gateway_wait_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# install_kubectl_stub <dir> <mode>
# Writes a stub `kubectl` into <dir> that fakes the API responses
# `wait_for_gateway_programmed` uses. The mode controls the `get gateway`
# response:
#   - "programmed"    → returns Programmed=True on every call (happy path)
#   - "pending"       → always returns Programmed=False so the caller times out
# In both modes, `describe gateway/...` writes a sentinel into
# ${KUBECTL_DESCRIBE_LOG}, `get pods -n envoy-gateway-system` prints a fixed
# pod name list, and `logs <pod> -n envoy-gateway-system ...` writes a line
# per invocation into ${KUBECTL_LOGS_LOG}. All other invocations exit 0
# silently.
install_kubectl_stub() {
  local dir="$1"
  local mode="$2"
  mkdir -p "$dir"
  local describe_log="${KUBECTL_DESCRIBE_LOG}"
  local logs_log="${KUBECTL_LOGS_LOG}"

  cat >"$dir/kubectl" <<STUB
#!/bin/bash
# Stub kubectl for tests/unit/hack/deploy_infra_gateway_wait_test.sh.
if [[ "\$1" == "get" && "\$2" == gateway/* ]]; then
  name="\${2#gateway/}"
  if [[ "${mode}" == "programmed" ]]; then
    cat <<JSON
{"status":{"conditions":[{"type":"Programmed","status":"True","reason":"Programmed","message":"Gateway is ready"}]}}
JSON
  else
    cat <<JSON
{"status":{"conditions":[{"type":"Programmed","status":"False","reason":"Pending","message":"listener not ready"}]}}
JSON
  fi
  exit 0
fi
if [[ "\$1" == "describe" && "\$2" == gateway/* ]]; then
  printf '%s\n' "describe \$*" >>"${describe_log}"
  echo "Name: openstack-gw"
  exit 0
fi
if [[ "\$1" == "get" && "\$2" == "pods" ]]; then
  # Return a JSONPath-like space-separated pod list for the
  # envoy-gateway-system namespace.
  printf '%s' "envoy-gateway-0 envoy-openstack-openstack-gw-abc"
  exit 0
fi
if [[ "\$1" == "logs" ]]; then
  printf '%s\n' "logs \$*" >>"${logs_log}"
  echo "fake log output"
  exit 0
fi
exit 0
STUB
  chmod +x "$dir/kubectl"
}

# Source the script and call the function under test in a subshell with PATH
# pointing at the stub. Echoes combined stdout/stderr; returns the exit
# status.
run_wait() {
  local stub_dir="$1"
  local timeout_arg="$2"
  (
    # Prepend the stub dir so our fake kubectl takes precedence, but keep the
    # rest of the PATH so real date/sleep/dirname/jq remain available — the
    # function under test does deadline math with `$(date +%s)` which breaks
    # under a PATH that contains only the stub dir.
    PATH="$stub_dir:$PATH"
    export PATH KUBECTL_DESCRIBE_LOG KUBECTL_LOGS_LOG
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    wait_for_gateway_programmed openstack-gw openstack "${timeout_arg}"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test 1: happy path — Programmed=True on first poll returns 0
# ---------------------------------------------------------------------------
test_happy_path_returns_zero() {
  echo "Test: wait_for_gateway_programmed returns 0 when Programmed=True"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  KUBECTL_DESCRIBE_LOG="$tmp/describe.log"
  KUBECTL_LOGS_LOG="$tmp/logs.log"
  : >"$KUBECTL_DESCRIBE_LOG"
  : >"$KUBECTL_LOGS_LOG"

  install_kubectl_stub "$tmp" "programmed"

  local output exit_code
  output="$(run_wait "$tmp" 30)"
  exit_code=$?

  assert_eq "wait_for_gateway_programmed exits 0 on happy path" "0" "$exit_code"
  assert_contains "log line announces wait" "$output" \
    "Waiting up to 30s for Gateway/openstack-gw"
  assert_contains "log line reports Programmed success" "$output" \
    "Gateway/openstack-gw is Programmed."

  # No diagnostics should have been dumped on the happy path.
  local describe_count logs_count
  describe_count="$(wc -l <"$KUBECTL_DESCRIBE_LOG" | tr -d ' ')"
  logs_count="$(wc -l <"$KUBECTL_LOGS_LOG" | tr -d ' ')"
  assert_eq "kubectl describe not invoked on happy path" "0" "$describe_count"
  assert_eq "kubectl logs not invoked on happy path" "0" "$logs_count"
}

# ---------------------------------------------------------------------------
# Test 2: timeout path — surfaces diagnostics and exits non-zero
# ---------------------------------------------------------------------------
test_timeout_dumps_diagnostics() {
  echo "Test: wait_for_gateway_programmed dumps describe + logs and exits non-zero on timeout"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  KUBECTL_DESCRIBE_LOG="$tmp/describe.log"
  KUBECTL_LOGS_LOG="$tmp/logs.log"
  : >"$KUBECTL_DESCRIBE_LOG"
  : >"$KUBECTL_LOGS_LOG"

  install_kubectl_stub "$tmp" "pending"

  # Timeout of 0 means the deadline is reached on the first iteration, so the
  # function prints one "not Programmed yet" log line, then emits the
  # diagnostics and exits 1 without sleeping.
  local output exit_code
  output="$(run_wait "$tmp" 0)"
  exit_code=$?

  assert_nonzero_exit "wait_for_gateway_programmed exits non-zero on timeout" "$exit_code"
  assert_contains "timeout message surfaced" "$output" \
    "ERROR: Timed out waiting for Gateway/openstack-gw"
  assert_contains "gateway description dump header surfaced" "$output" \
    "Gateway description:"
  # Match the header prefix rather than the exact trailing text so
  # future tweaks to the diagnostic window (e.g. `--since=10m, tail
  # 200`) don't require retuning this assertion.
  assert_contains "envoy-gateway-system pod log header surfaced" "$output" \
    "envoy-gateway-system pod logs"

  # kubectl describe called once on timeout.
  local describe_count
  describe_count="$(wc -l <"$KUBECTL_DESCRIBE_LOG" | tr -d ' ')"
  assert_eq "kubectl describe gateway/... invoked once on timeout" "1" "$describe_count"
  assert_file_contains "describe targets the openstack-gw Gateway" \
    "$KUBECTL_DESCRIBE_LOG" "gateway/openstack-gw"

  # kubectl logs called once per pod returned by the stub (two pods).
  local logs_count
  logs_count="$(wc -l <"$KUBECTL_LOGS_LOG" | tr -d ' ')"
  assert_eq "kubectl logs invoked once per envoy-gateway-system pod (2 pods)" \
    "2" "$logs_count"
  assert_file_contains "logs scoped to envoy-gateway-system namespace" \
    "$KUBECTL_LOGS_LOG" "envoy-gateway-system"
  # The `--since=10m` filter keeps diagnostic output focused on the
  # recent failure window — verify it's wired so the log-focus fix is
  # traceable to this test. Pattern omits the leading `--`
  # so the shared `assert_file_contains` helper (which calls `grep -q`
  # without an `--` separator) matches the flag literally.
  assert_file_contains "logs request a --since=10m window to bound the diagnostic output" \
    "$KUBECTL_LOGS_LOG" "since=10m"
}

# ---------------------------------------------------------------------------
# Test 3: the script wires envoy-gateway into the Phase-3 wait list
# and calls wait_for_gateway_programmed afterwards.
# Static-text check so this test stays independent of the stub plumbing
# above.
# ---------------------------------------------------------------------------
test_main_calls_gateway_wait_after_phase_3() {
  echo "Test: main() wires envoy-gateway into Phase 3 and invokes wait_for_gateway_programmed"

  assert_file_contains "envoy-gateway appears in the Phase-3 HelmRelease wait list" \
    "$DEPLOY_INFRA_SH" "envoy-gateway"
  assert_file_contains "wait_for_gateway_programmed is invoked by main()" \
    "$DEPLOY_INFRA_SH" "wait_for_gateway_programmed openstack-gw openstack"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_happy_path_returns_zero
test_timeout_dumps_diagnostics
test_main_calls_gateway_wait_after_phase_3

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
