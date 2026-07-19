#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh `install_gateway_api_crds` skips the
# `kubectl apply --server-side` when all five standard-channel Gateway API
# CRDs already exist (so a re-run against a provisioned cluster no longer hits
# an SSA field-manager conflict with helm-controller), still runs the 3-attempt
# retry install on a bare cluster, tells an INCOMPLETE live CRD set apart from a
# bare one, and fails fast on a field-manager conflict instead of retrying it.
#
# Follows the stub-kubectl + source-and-invoke pattern established by
# deploy_infra_gateway_wait_test.sh.
#
# Usage: bash tests/unit/hack/deploy_infra_gateway_crds_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# The five standard-channel CRD names the function probes. A bare cluster
# reports every one missing — that, not "any one missing", is the fresh-install
# state.
GWAPI_ALL_CRDS="gatewayclasses.gateway.networking.k8s.io \
gateways.gateway.networking.k8s.io \
grpcroutes.gateway.networking.k8s.io \
httproutes.gateway.networking.k8s.io \
referencegrants.gateway.networking.k8s.io"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# install_kubectl_stub <dir>
# Writes a stub `kubectl` into <dir> that fakes the API responses
# `install_gateway_api_crds` uses. The stub's behaviour is baked in from the
# caller-set variables:
#   - APPLY_LOG             — every `apply` argv is appended here, one per line.
#   - MISSING_CRDS          — space-separated CRD names that report NotFound
#                             (empty → every CRD reports present).
#   - LIVE_BUNDLE_VERSION   — printed for the jsonpath bundle-version query.
#   - APPLY_FAIL            — when "1", `apply` still logs but exits non-zero.
#   - APPLY_OUTPUT          — what a failing `apply` writes to stderr, so a
#                             field-manager conflict can be told apart from a
#                             transient error.
# Everything else exits 0 silently.
install_kubectl_stub() {
  local dir="$1"
  mkdir -p "$dir"
  local apply_log="${APPLY_LOG}"
  local missing_crds="${MISSING_CRDS:-}"
  local live_bundle_version="${LIVE_BUNDLE_VERSION:-}"
  local apply_fail="${APPLY_FAIL:-0}"
  local apply_output="${APPLY_OUTPUT:-error: the server could not find the requested resource}"

  cat >"$dir/kubectl" <<STUB
#!/bin/bash
# Stub kubectl for tests/unit/hack/deploy_infra_gateway_crds_test.sh.
if [[ "\$1" == "apply" ]]; then
  printf '%s\n' "\$*" >>"${apply_log}"
  if [[ "${apply_fail}" == "1" ]]; then
    printf '%s\n' "${apply_output}" >&2
    exit 1
  fi
  exit 0
fi
if [[ "\$1" == "get" && "\$2" == "crd" ]]; then
  # The bundle-version query carries a jsonpath arg; answer it with the
  # stubbed live version instead of a presence check.
  for arg in "\$@"; do
    if [[ "\$arg" == jsonpath=* ]]; then
      printf '%s' "${live_bundle_version}"
      exit 0
    fi
  done
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
    # Prepend the stub dir so our fake kubectl takes precedence, but keep the
    # rest of the PATH so real coreutils remain available.
    PATH="$stub_dir:$PATH"
    export PATH
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    install_gateway_api_crds
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test 1: all five CRDs present → skip the apply, log the live bundle version
# ---------------------------------------------------------------------------
test_all_present_skips_apply() {
  echo "Test: install_gateway_api_crds skips the apply when all five CRDs exist"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  APPLY_LOG="$tmp/apply.log"
  : >"$APPLY_LOG"
  MISSING_CRDS=""
  LIVE_BUNDLE_VERSION="v1.5.1"
  APPLY_FAIL=0

  install_kubectl_stub "$tmp"

  local output exit_code
  output="$(run_install "$tmp")"
  exit_code=$?

  assert_eq "install_gateway_api_crds exits 0 when all CRDs present" "0" "$exit_code"

  local apply_count
  apply_count="$(wc -l <"$APPLY_LOG" | tr -d ' ')"
  assert_eq "no kubectl apply is issued on the skip path" "0" "$apply_count"

  assert_contains "skip log line announces the CRDs are already present" "$output" \
    "Gateway API CRDs already present"
  assert_contains "skip log line reports the live bundle version" "$output" \
    "v1.5.1"
  assert_contains "skip log line notes another owner may manage the CRDs" "$output" \
    "helm-controller"
  # The stubbed live bundle (v1.5.1) differs from the GATEWAY_API_VERSION pin,
  # so the skip must not silently claim the requested bundle is installed.
  assert_contains "bundle drift against GATEWAY_API_VERSION is warned about" "$output" \
    "WARNING: the live bundle does not match the requested v1.1.0"
  assert_contains "drift warning names the recreate remedy" "$output" \
    "teardown-infra"
}

# ---------------------------------------------------------------------------
# Test 1b: all five CRDs present AND the live bundle matches the pin →
# skip without the drift warning
# ---------------------------------------------------------------------------
test_matching_bundle_skips_without_warning() {
  echo "Test: install_gateway_api_crds skips quietly when the live bundle matches"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  APPLY_LOG="$tmp/apply.log"
  : >"$APPLY_LOG"
  MISSING_CRDS=""
  LIVE_BUNDLE_VERSION="v9.9.9"
  APPLY_FAIL=0
  export GATEWAY_API_VERSION="v9.9.9"

  install_kubectl_stub "$tmp"

  local output exit_code
  output="$(run_install "$tmp")"
  exit_code=$?
  unset GATEWAY_API_VERSION

  assert_eq "install_gateway_api_crds exits 0 when the bundle matches" "0" "$exit_code"
  assert_contains "skip log line still announces the CRDs are already present" "$output" \
    "Gateway API CRDs already present"
  assert_not_contains "no drift warning when live bundle == GATEWAY_API_VERSION" "$output" \
    "WARNING: the live bundle does not match"
}

# ---------------------------------------------------------------------------
# Test 1c: all five present but no bundle-version annotation → the skip must
# say the live version could not be verified, not accept it silently
# ---------------------------------------------------------------------------
test_unknown_bundle_version_warns() {
  echo "Test: install_gateway_api_crds warns when the live bundle version is unknown"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  APPLY_LOG="$tmp/apply.log"
  : >"$APPLY_LOG"
  MISSING_CRDS=""
  LIVE_BUNDLE_VERSION=""   # e.g. a chart that templates the CRDs itself
  APPLY_FAIL=0

  install_kubectl_stub "$tmp"

  local output exit_code
  output="$(run_install "$tmp")"
  exit_code=$?

  assert_eq "install_gateway_api_crds still exits 0" "0" "$exit_code"
  assert_contains "skip log line reports the version as unknown" "$output" \
    "live bundle unknown"
  assert_contains "an unverifiable live bundle is warned about, not accepted silently" "$output" \
    "cannot be verified"
  assert_contains "the warning names the recreate remedy" "$output" \
    "teardown-infra"
}

# ---------------------------------------------------------------------------
# Test 2: bare cluster (every CRD missing) → install exactly once
# ---------------------------------------------------------------------------
test_missing_crd_runs_apply_once() {
  echo "Test: install_gateway_api_crds applies once on a bare cluster"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  APPLY_LOG="$tmp/apply.log"
  : >"$APPLY_LOG"
  MISSING_CRDS="$GWAPI_ALL_CRDS"   # bare cluster: nothing present yet
  LIVE_BUNDLE_VERSION=""
  APPLY_FAIL=0
  export GATEWAY_API_CRDS_URL="https://example.test/gwapi/standard-install.yaml"

  install_kubectl_stub "$tmp"

  local output exit_code
  output="$(run_install "$tmp")"
  exit_code=$?
  unset GATEWAY_API_CRDS_URL

  assert_eq "install_gateway_api_crds exits 0 on a successful install" "0" "$exit_code"

  local apply_count
  apply_count="$(wc -l <"$APPLY_LOG" | tr -d ' ')"
  assert_eq "exactly one kubectl apply is issued on the install path" "1" "$apply_count"

  assert_file_contains "apply uses server-side" \
    "$APPLY_LOG" "server-side"
  assert_file_contains "apply targets the GATEWAY_API_CRDS_URL bundle" \
    "$APPLY_LOG" "https://example.test/gwapi/standard-install.yaml"
  assert_contains "success log line surfaced" "$output" \
    "Gateway API CRDs installed."
}

# ---------------------------------------------------------------------------
# Test 3: persistent apply failure → 3 attempts, non-zero exit, ERROR log
# ---------------------------------------------------------------------------
test_persistent_failure_retries_three_times() {
  echo "Test: install_gateway_api_crds retries three times then fails"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  APPLY_LOG="$tmp/apply.log"
  : >"$APPLY_LOG"
  MISSING_CRDS="$GWAPI_ALL_CRDS"   # bare cluster: nothing present yet
  LIVE_BUNDLE_VERSION=""
  APPLY_FAIL=1

  install_kubectl_stub "$tmp"

  local output exit_code
  output="$(run_install "$tmp")"
  exit_code=$?

  assert_nonzero_exit "install_gateway_api_crds exits non-zero after exhausting retries" \
    "$exit_code"

  local apply_count
  apply_count="$(wc -l <"$APPLY_LOG" | tr -d ' ')"
  assert_eq "kubectl apply is retried exactly three times" "3" "$apply_count"

  assert_contains "ERROR log line reports the exhausted retries" "$output" \
    "ERROR: Failed to install Gateway API CRDs after 3 attempts"
}

# ---------------------------------------------------------------------------
# Test 3b: an INCOMPLETE live CRD set is named before the apply
#
# `kubectl get crd a b c` exits non-zero as soon as one name is NotFound, so a
# single presence call cannot tell "four of five present, co-owned by
# helm-controller" apart from a bare cluster. The partial set must be probed per
# CRD and reported, so the conflict that follows is already diagnosed instead of
# surfacing as a misleading retry-exhaustion message.
# ---------------------------------------------------------------------------
test_partial_crd_set_is_reported() {
  echo "Test: install_gateway_api_crds names an incomplete live CRD set"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  APPLY_LOG="$tmp/apply.log"
  : >"$APPLY_LOG"
  # A live bundle predating grpcroutes' promotion to the standard channel: the
  # other four exist and are field-managed by someone else.
  MISSING_CRDS="grpcroutes.gateway.networking.k8s.io"
  LIVE_BUNDLE_VERSION=""
  APPLY_FAIL=0

  install_kubectl_stub "$tmp"

  local output exit_code
  output="$(run_install "$tmp")"
  exit_code=$?

  assert_eq "a partial set still attempts the install" "0" "$exit_code"
  assert_contains "the incomplete set is called out" "$output" \
    "INCOMPLETE Gateway API CRD set"
  assert_contains "the missing CRD is named" "$output" \
    "missing: grpcroutes.gateway.networking.k8s.io"
  assert_contains "the already-present CRDs are named" "$output" \
    "present: gatewayclasses.gateway.networking.k8s.io"
  assert_not_contains "a partial set is NOT mistaken for a completed install" "$output" \
    "Gateway API CRDs already present"

  local apply_count
  apply_count="$(wc -l <"$APPLY_LOG" | tr -d ' ')"
  assert_eq "the bundle apply is still attempted once" "1" "$apply_count"
}

# ---------------------------------------------------------------------------
# Test 3c: a field-manager conflict fails fast — it is deterministic, so the
# retry budget must not be spent re-applying the identical bundle
# ---------------------------------------------------------------------------
test_conflict_fails_fast_without_retry() {
  echo "Test: install_gateway_api_crds fails fast on a field-manager conflict"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  APPLY_LOG="$tmp/apply.log"
  : >"$APPLY_LOG"
  MISSING_CRDS="grpcroutes.gateway.networking.k8s.io"
  LIVE_BUNDLE_VERSION=""
  APPLY_FAIL=1
  APPLY_OUTPUT='error: Apply failed with 4 conflicts: conflicts with "helm-controller"'

  install_kubectl_stub "$tmp"

  local output exit_code
  output="$(run_install "$tmp")"
  exit_code=$?
  APPLY_OUTPUT=""

  assert_nonzero_exit "install_gateway_api_crds exits non-zero on a conflict" "$exit_code"

  local apply_count
  apply_count="$(wc -l <"$APPLY_LOG" | tr -d ' ')"
  assert_eq "a conflict is applied exactly once, not retried" "1" "$apply_count"

  assert_contains "the ERROR names the conflict as the cause" "$output" \
    "field-manager conflict"
  assert_contains "the ERROR says it is deliberately not retried" "$output" \
    "Not retried"
  assert_not_contains "the misleading retry-exhaustion message is not used" "$output" \
    "after 3 attempts"
}

# ---------------------------------------------------------------------------
# Test 4: static wiring — main() invokes install_gateway_api_crds
# Static-text check so this test stays independent of the stub plumbing above,
# mirroring test_main_calls_gateway_wait_after_phase_3 in the sibling file.
# ---------------------------------------------------------------------------
test_main_calls_install_gateway_api_crds() {
  echo "Test: main() invokes install_gateway_api_crds"

  # Anchor on end-of-line so the bare call site matches but the
  # `install_gateway_api_crds()` definition and the docblock do not.
  assert_file_contains "install_gateway_api_crds is invoked by main()" \
    "$DEPLOY_INFRA_SH" "install_gateway_api_crds$"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_all_present_skips_apply
test_matching_bundle_skips_without_warning
test_unknown_bundle_version_warns
test_missing_crd_runs_apply_once
test_persistent_failure_retries_three_times
test_partial_crd_set_is_reported
test_conflict_fails_fast_without_retry
test_main_calls_install_gateway_api_crds

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
