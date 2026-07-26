#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh `install_envoy_gateway_crds` applies the pinned
# envoy-gateway-crds.yaml release asset on a bare cluster, skips a complete
# live set (the gateway.envoyproxy.io CRDs carry no comparable version
# annotation, and on a pre-split cluster helm-controller field-manages them),
# warns on an INCOMPLETE set before attempting the apply, fails fast on a
# deterministic field-manager conflict, and retries transient failures three
# times.
#
# Follows the stub-kubectl + source-and-invoke pattern established by
# deploy_infra_gateway_crds_test.sh.
#
# Usage: bash tests/unit/hack/deploy_infra_envoy_gateway_crds_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# The eight CRD names the ENVOY_GATEWAY_VERSION envoy-gateway-crds.yaml asset
# ships and the function probes. test_eg_crd_list_matches_script keeps this
# literal in sync with the script's own eg_crds array.
EG_ALL_CRDS="backends.gateway.envoyproxy.io \
backendtrafficpolicies.gateway.envoyproxy.io \
clienttrafficpolicies.gateway.envoyproxy.io \
envoyextensionpolicies.gateway.envoyproxy.io \
envoypatchpolicies.gateway.envoyproxy.io \
envoyproxies.gateway.envoyproxy.io \
httproutefilters.gateway.envoyproxy.io \
securitypolicies.gateway.envoyproxy.io"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# install_kubectl_stub <dir>
# Writes a stub `kubectl` into <dir> that fakes the API responses
# `install_envoy_gateway_crds` uses. The stub's behaviour is baked in from the
# caller-set variables:
#   - APPLY_LOG    — every `apply` argv is appended here, one per line.
#   - MISSING_CRDS — space-separated CRD names that report NotFound
#                    (empty → every CRD reports present).
#   - APPLY_FAIL   — when "1", `apply` still logs but exits non-zero.
#   - APPLY_OUTPUT — what a failing `apply` writes to stderr, so a
#                    field-manager conflict can be told apart from a
#                    transient error.
# Everything else exits 0 silently.
install_kubectl_stub() {
  local dir="$1"
  mkdir -p "$dir"
  local apply_log="${APPLY_LOG}"
  local missing_crds="${MISSING_CRDS:-}"
  local apply_fail="${APPLY_FAIL:-0}"
  local apply_output="${APPLY_OUTPUT:-error: the server could not find the requested resource}"

  cat >"$dir/kubectl" <<STUB
#!/bin/bash
# Stub kubectl for tests/unit/hack/deploy_infra_envoy_gateway_crds_test.sh.
if [[ "\$1" == "apply" ]]; then
  printf '%s\n' "\$*" >>"${apply_log}"
  if [[ "${apply_fail}" == "1" ]]; then
    printf '%s\n' "${apply_output}" >&2
    exit 1
  fi
  exit 0
fi
if [[ "\$1" == "get" && "\$2" == "crd" ]]; then
  # Presence check — one CRD name per invocation.
  for arg in "\$@"; do
    for missing in ${missing_crds}; do
      if [[ "\$arg" == "\$missing" ]]; then
        exit 1
      fi
    done
  done
  exit 0
fi
exit 0
STUB
  chmod +x "$dir/kubectl"

  # No-op sleep so the retry test does not actually wait out the backoff.
  printf '#!/bin/bash\nexit 0\n' >"$dir/sleep"
  chmod +x "$dir/sleep"
}

# Source the script and call the function under test in a subshell with PATH
# pointing at the stub. Echoes combined stdout/stderr; returns the exit status.
# The function calls `exit 1` on the failure path, so the subshell keeps the
# test process alive.
run_install() {
  local stub_dir="$1"
  (
    PATH="$stub_dir:$PATH"
    export PATH
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    install_envoy_gateway_crds
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test 1: bare cluster (every CRD missing) → exactly one apply of the asset
# ---------------------------------------------------------------------------
test_bare_cluster_applies_asset() {
  echo "Test: a bare cluster gets exactly one server-side apply of the asset"

  local tmp_dir
  tmp_dir="$(mktemp -d)"
  APPLY_LOG="$tmp_dir/apply.log"
  : >"$APPLY_LOG"
  MISSING_CRDS="$EG_ALL_CRDS" APPLY_FAIL=0 install_kubectl_stub "$tmp_dir/bin"

  local output status=0
  output="$(run_install "$tmp_dir/bin")" || status=$?

  assert_eq "install returns success" "0" "$status"
  assert_eq "exactly one apply attempt" "1" "$(wc -l <"$APPLY_LOG" | tr -d ' ')"
  assert_contains "apply targets the envoy-gateway-crds asset URL" \
    "$(cat "$APPLY_LOG")" "envoy-gateway-crds.yaml"
  assert_contains "apply is server-side" \
    "$(cat "$APPLY_LOG")" "--server-side"
  assert_contains "install banner names the pinned version" \
    "$output" "Installing Envoy Gateway CRDs"

  rm -rf "$tmp_dir"
}

# ---------------------------------------------------------------------------
# Test 2: complete live set → skip without any apply
#
# The CRDs carry no version annotation the function could reconcile against,
# and on a pre-split cluster helm-controller field-manages them — a re-assert
# would SSA-conflict.
# ---------------------------------------------------------------------------
test_complete_set_skips_apply() {
  echo "Test: a complete live CRD set is skipped without an apply"

  local tmp_dir
  tmp_dir="$(mktemp -d)"
  APPLY_LOG="$tmp_dir/apply.log"
  : >"$APPLY_LOG"
  MISSING_CRDS="" APPLY_FAIL=0 install_kubectl_stub "$tmp_dir/bin"

  local output status=0
  output="$(run_install "$tmp_dir/bin")" || status=$?

  assert_eq "install returns success" "0" "$status"
  assert_eq "no apply attempt" "0" "$(wc -l <"$APPLY_LOG" | tr -d ' ')"
  assert_contains "skip message names the reason" \
    "$output" "already present"

  rm -rf "$tmp_dir"
}

# ---------------------------------------------------------------------------
# Test 3: incomplete live set → warned about, then applied anyway
# ---------------------------------------------------------------------------
test_partial_crd_set_is_reported() {
  echo "Test: an incomplete live CRD set is warned about before the apply"

  local tmp_dir
  tmp_dir="$(mktemp -d)"
  APPLY_LOG="$tmp_dir/apply.log"
  : >"$APPLY_LOG"
  MISSING_CRDS="envoyproxies.gateway.envoyproxy.io" APPLY_FAIL=0 \
    install_kubectl_stub "$tmp_dir/bin"

  local output status=0
  output="$(run_install "$tmp_dir/bin")" || status=$?

  assert_eq "install returns success" "0" "$status"
  assert_contains "warning names the incomplete set" \
    "$output" "INCOMPLETE Envoy Gateway CRD set"
  assert_contains "warning lists the missing CRD" \
    "$output" "missing: envoyproxies.gateway.envoyproxy.io"
  assert_eq "exactly one apply attempt" "1" "$(wc -l <"$APPLY_LOG" | tr -d ' ')"

  rm -rf "$tmp_dir"
}

# ---------------------------------------------------------------------------
# Test 4: field-manager conflict → fail fast without a retry
# ---------------------------------------------------------------------------
test_conflict_fails_fast_without_retry() {
  echo "Test: a field-manager conflict fails fast without burning retries"

  local tmp_dir
  tmp_dir="$(mktemp -d)"
  APPLY_LOG="$tmp_dir/apply.log"
  : >"$APPLY_LOG"
  MISSING_CRDS="$EG_ALL_CRDS" APPLY_FAIL=1 \
    APPLY_OUTPUT="error: Apply failed with 1 conflict: conflict with \"helm-controller\"" \
    install_kubectl_stub "$tmp_dir/bin"

  local output status=0
  output="$(run_install "$tmp_dir/bin")" || status=$?

  assert_eq "install exits non-zero" "1" "$status"
  assert_eq "exactly one apply attempt (no retry)" "1" "$(wc -l <"$APPLY_LOG" | tr -d ' ')"
  assert_contains "error names the field-manager conflict" \
    "$output" "field-manager conflict"

  rm -rf "$tmp_dir"
}

# ---------------------------------------------------------------------------
# Test 5: transient failure → three attempts, then a terminal error
# ---------------------------------------------------------------------------
test_persistent_failure_retries_three_times() {
  echo "Test: a transient apply failure is retried three times"

  local tmp_dir
  tmp_dir="$(mktemp -d)"
  APPLY_LOG="$tmp_dir/apply.log"
  : >"$APPLY_LOG"
  MISSING_CRDS="$EG_ALL_CRDS" APPLY_FAIL=1 install_kubectl_stub "$tmp_dir/bin"

  local output status=0
  output="$(run_install "$tmp_dir/bin")" || status=$?

  assert_eq "install exits non-zero" "1" "$status"
  assert_eq "three apply attempts" "3" "$(wc -l <"$APPLY_LOG" | tr -d ' ')"
  assert_contains "terminal error names the asset URL" \
    "$output" "envoy-gateway-crds.yaml"

  rm -rf "$tmp_dir"
}

# ---------------------------------------------------------------------------
# Test 6: the probed eg_crds array matches this test's CRD list
# ---------------------------------------------------------------------------
test_eg_crd_list_matches_script() {
  echo "Test: the probed eg_crds array matches this test's CRD list"

  local script_crds
  script_crds="$(sed -n '/local eg_crds=(/,/^  )$/p' "$DEPLOY_INFRA_SH" \
    | grep -oE '[a-z]+\.gateway\.envoyproxy\.io' | sort | tr '\n' ' ')"

  local expected
  expected="$(printf '%s\n' $EG_ALL_CRDS | sort | tr '\n' ' ')"

  assert_eq "hack/deploy-infra.sh probes exactly the CRD names this test stubs" \
    "$expected" "$script_crds"
}

# ---------------------------------------------------------------------------
# Test 7: static wiring — main() invokes install_envoy_gateway_crds
# ---------------------------------------------------------------------------
test_main_calls_install_envoy_gateway_crds() {
  echo "Test: main() invokes install_envoy_gateway_crds"

  # Anchor on end-of-line so the bare call site matches but the
  # `install_envoy_gateway_crds()` definition and the docblock do not.
  assert_file_contains "install_envoy_gateway_crds is invoked by main()" \
    "$DEPLOY_INFRA_SH" "install_envoy_gateway_crds$"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_bare_cluster_applies_asset
test_complete_set_skips_apply
test_partial_crd_set_is_reported
test_conflict_fails_fast_without_retry
test_persistent_failure_retries_three_times
test_eg_crd_list_matches_script
test_main_calls_install_envoy_gateway_crds

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
