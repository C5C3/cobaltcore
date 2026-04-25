#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh `render_kind_config` honours the KIND_HOST_PORT
# override and falls back to the checked-in kind-config.yaml verbatim when the
# override is unset (CC-0088).
#
# Sources deploy-infra.sh and invokes the function in a subshell so we can
# assert against the rendered tempfile without spinning up an actual cluster.
# Skipped when `yq` is not installed (the override path requires it).
#
# Usage: bash tests/unit/hack/deploy_infra_kind_host_port_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"
KIND_CONFIG_FILE="$PROJECT_ROOT/hack/kind-config.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# Source the script and call render_kind_config in a subshell with the given
# KIND_HOST_PORT, writing to <tmp>/rendered.yaml. Echoes combined output and
# returns the function's exit status. Subshell isolates env mutations and
# prevents the script's `main` from running (the BASH_SOURCE guard at the
# bottom of deploy-infra.sh skips main when sourced).
run_render() {
  local out_path="$1"
  local kind_host_port="$2"
  (
    export KIND_HOST_PORT="${kind_host_port}"
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    render_kind_config "${out_path}"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test 1: default (KIND_HOST_PORT=443) copies the file verbatim — byte-equal.
# This guarantees the CI baseline (Linux + rootful Docker) sees no behaviour
# change when no override is set (CC-0088).
# ---------------------------------------------------------------------------
test_default_port_byte_equal_copy() {
  echo "Test: render_kind_config with KIND_HOST_PORT=443 produces a byte-equal copy (CC-0088)"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  local out="$tmp/rendered.yaml"
  local output exit_code
  output="$(run_render "$out" "443")"
  exit_code=$?

  assert_eq "render_kind_config exits 0 with default port" "0" "$exit_code"

  if [[ ! -f "$out" ]]; then
    echo "  FAIL: render_kind_config did not produce the output file"
    FAIL=$((FAIL + 1))
    echo "  output was: $output"
    return
  fi

  if cmp -s "$out" "$KIND_CONFIG_FILE"; then
    echo "  PASS: rendered file is byte-equal to hack/kind-config.yaml"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: rendered file differs from hack/kind-config.yaml"
    FAIL=$((FAIL + 1))
    diff "$KIND_CONFIG_FILE" "$out" | head -20
  fi
}

# ---------------------------------------------------------------------------
# Test 2: override (KIND_HOST_PORT=8443) rewrites only the hostPort field;
# containerPort, protocol, and listenAddress stay intact (CC-0088).
# ---------------------------------------------------------------------------
test_override_rewrites_only_hostport() {
  echo "Test: render_kind_config with KIND_HOST_PORT=8443 rewrites only hostPort (CC-0088)"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  local out="$tmp/rendered.yaml"
  local output exit_code
  output="$(run_render "$out" "8443")"
  exit_code=$?

  assert_eq "render_kind_config exits 0 with override port" "0" "$exit_code"

  if [[ ! -f "$out" ]]; then
    echo "  FAIL: render_kind_config did not produce the output file"
    FAIL=$((FAIL + 1))
    echo "  output was: $output"
    return
  fi

  # The selector keys on hostPort==8443 (the override value) so we re-find
  # the entry that previously matched 443 and verify the sibling fields.
  local mapping
  mapping="$(yq -r '.nodes[0].extraPortMappings[] | select(.hostPort == 8443)' "$out")"
  if [[ -z "$mapping" ]]; then
    echo "  FAIL: no extraPortMappings entry with hostPort=8443 in rendered output"
    FAIL=$((FAIL + 4))
    return
  fi

  assert_eq "rendered hostPort is 8443" \
    "8443" \
    "$(yq -r '.nodes[0].extraPortMappings[] | select(.hostPort == 8443) | .hostPort' "$out")"

  assert_eq "containerPort still 31443 after override" \
    "31443" \
    "$(yq -r '.nodes[0].extraPortMappings[] | select(.hostPort == 8443) | .containerPort' "$out")"

  assert_eq "protocol still TCP after override" \
    "TCP" \
    "$(yq -r '.nodes[0].extraPortMappings[] | select(.hostPort == 8443) | .protocol' "$out")"

  assert_eq "listenAddress still 127.0.0.1 after override" \
    "127.0.0.1" \
    "$(yq -r '.nodes[0].extraPortMappings[] | select(.hostPort == 8443) | .listenAddress' "$out")"

  # The original hostPort=443 entry must be gone (otherwise both ports would
  # bind, and one of them — the privileged one — would re-trigger the
  # original failure on hosts that need the override).
  local count_443
  count_443="$(yq -r '[.nodes[0].extraPortMappings[] | select(.hostPort == 443)] | length' "$out")"
  assert_eq "no leftover hostPort=443 entry after override" "0" "$count_443"
}

# ---------------------------------------------------------------------------
# Test 3: invalid KIND_HOST_PORT values are rejected with a clear error
# before kind ever runs. Catches typos like `KIND_HOST_PORT=eightthousand`.
# ---------------------------------------------------------------------------
test_invalid_port_rejected() {
  echo "Test: render_kind_config rejects non-numeric and out-of-range ports (CC-0088)"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  local out="$tmp/rendered.yaml"

  local output exit_code
  output="$(run_render "$out" "not-a-port")"
  exit_code=$?
  assert_nonzero_exit "non-numeric KIND_HOST_PORT exits non-zero" "$exit_code"
  assert_contains "non-numeric error message surfaces the offending value" \
    "$output" "KIND_HOST_PORT='not-a-port'"

  output="$(run_render "$out" "65536")"
  exit_code=$?
  assert_nonzero_exit "out-of-range KIND_HOST_PORT exits non-zero" "$exit_code"

  output="$(run_render "$out" "0")"
  exit_code=$?
  assert_nonzero_exit "KIND_HOST_PORT=0 exits non-zero" "$exit_code"
}

# ---------------------------------------------------------------------------
# Test 4: main() wires the rendered tempfile into `kind create cluster`
# and references the KIND_HOST_PORT env var in its log preamble. Static
# text checks keep this independent of stub plumbing (CC-0088).
# ---------------------------------------------------------------------------
test_main_uses_rendered_config() {
  echo "Test: main() invokes render_kind_config and references KIND_HOST_PORT (CC-0088)"

  assert_file_contains "render_kind_config is called from main()" \
    "$DEPLOY_INFRA_SH" "render_kind_config "
  # Match the unique tempfile variable rather than the leading `--config`
  # flag, since BSD grep parses `--…` as an option (CC-0088).
  assert_file_contains "kind create cluster consumes the rendered tempfile" \
    "$DEPLOY_INFRA_SH" 'kind_cfg'
  assert_file_contains "KIND_HOST_PORT is documented in the log preamble" \
    "$DEPLOY_INFRA_SH" "KIND_HOST_PORT"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_default_port_byte_equal_copy
test_override_rewrites_only_hostport
test_invalid_port_rejected
test_main_uses_rendered_config

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
