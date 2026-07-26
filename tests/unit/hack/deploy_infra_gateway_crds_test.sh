#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh `install_gateway_api_crds` skips the
# `kubectl apply --server-side` when all ten standard-channel Gateway API CRDs
# already exist AT the pinned bundle version (so a re-run against a provisioned
# cluster no longer hits an SSA field-manager conflict with helm-controller),
# upgrades an OLDER live bundle in place onto the pin while refusing to
# downgrade a NEWER one, still runs the 3-attempt retry install on a bare
# cluster, tells an INCOMPLETE live CRD set apart from a bare one, and fails
# fast on a deterministic apply refusal — a field-manager conflict or a
# downgrade the live bundle rejects — instead of retrying it.
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

# The ten standard-channel CRD names the v1.6.1 bundle ships and the function
# probes. A bare cluster reports every one missing — that, not "any one
# missing", is the fresh-install state. test_gwapi_crd_list_matches_script keeps
# this literal in sync with the script's own gwapi_crds array.
GWAPI_ALL_CRDS="backendtlspolicies.gateway.networking.k8s.io \
gatewayclasses.gateway.networking.k8s.io \
gateways.gateway.networking.k8s.io \
grpcroutes.gateway.networking.k8s.io \
httproutes.gateway.networking.k8s.io \
listenersets.gateway.networking.k8s.io \
referencegrants.gateway.networking.k8s.io \
tcproutes.gateway.networking.k8s.io \
tlsroutes.gateway.networking.k8s.io \
udproutes.gateway.networking.k8s.io"

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
# Test 1: all ten CRDs present but the live bundle is OLDER than the pin
# → upgrade in place onto the pin instead of warning and returning
#
# A bundle older than the compiled-in gateway-api types still registers the
# kind but silently prunes the fields its schema does not know (v1.1.0 has no
# HTTPRoute spec.rules[].timeouts), so a re-used cluster must converge on the
# pin rather than be told to recreate itself.
# ---------------------------------------------------------------------------
test_older_bundle_upgrades_in_place() {
  echo "Test: install_gateway_api_crds upgrades an older live bundle in place"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  APPLY_LOG="$tmp/apply.log"
  : >"$APPLY_LOG"
  MISSING_CRDS=""
  LIVE_BUNDLE_VERSION="v1.1.0"
  APPLY_FAIL=0
  # Pin an explicit requested version so this test does not have to be edited
  # every time Renovate bumps GATEWAY_API_VERSION.
  export GATEWAY_API_VERSION="v9.9.9"
  export GATEWAY_API_CRDS_URL="https://example.test/gwapi/standard-install.yaml"

  install_kubectl_stub "$tmp"

  local output exit_code
  output="$(run_install "$tmp")"
  exit_code=$?
  unset GATEWAY_API_VERSION
  unset GATEWAY_API_CRDS_URL

  assert_eq "install_gateway_api_crds exits 0 after the in-place upgrade" "0" "$exit_code"

  local apply_count
  apply_count="$(wc -l <"$APPLY_LOG" | tr -d ' ')"
  assert_eq "the pinned bundle is applied exactly once" "1" "$apply_count"
  assert_file_contains "the upgrade uses server-side apply" \
    "$APPLY_LOG" "server-side"
  assert_file_contains "the upgrade targets the pinned GATEWAY_API_CRDS_URL bundle" \
    "$APPLY_LOG" "https://example.test/gwapi/standard-install.yaml"

  assert_contains "the log names the live bundle it is upgrading from" "$output" \
    "present at bundle v1.1.0"
  assert_contains "the log announces the in-place upgrade to the pin" "$output" \
    "applying an in-place upgrade to v9.9.9"
  assert_contains "the success log line is reached" "$output" \
    "Gateway API CRDs installed."

  assert_not_contains "drift is no longer reported as a warn-and-return" "$output" \
    "WARNING: the live bundle does not match"
  assert_not_contains "the cluster is not told to recreate itself" "$output" \
    "Recreate the cluster"
  assert_not_contains "a complete live set is not mislabelled INCOMPLETE" "$output" \
    "INCOMPLETE Gateway API CRD set"
}

# ---------------------------------------------------------------------------
# Test 1a: all ten CRDs present but the live bundle is NEWER than the pin
# → warn and return, never apply
#
# Convergence is one-way. Switching to a branch with an older pin must not push
# the older schema over the live bundle: the API server would prune every field
# the older schema does not know from the live Gateways and HTTPRoutes — the
# very failure the upgrade direction exists to prevent, inflicted deliberately.
# ---------------------------------------------------------------------------
test_newer_bundle_is_not_downgraded() {
  echo "Test: install_gateway_api_crds refuses to downgrade a newer live bundle"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  APPLY_LOG="$tmp/apply.log"
  : >"$APPLY_LOG"
  MISSING_CRDS=""
  LIVE_BUNDLE_VERSION="v9.9.9"
  APPLY_FAIL=0
  # The pin is older than what the cluster already carries.
  export GATEWAY_API_VERSION="v1.1.0"

  install_kubectl_stub "$tmp"

  local output exit_code
  output="$(run_install "$tmp")"
  exit_code=$?
  unset GATEWAY_API_VERSION

  assert_eq "install_gateway_api_crds still exits 0" "0" "$exit_code"

  local apply_count
  apply_count="$(wc -l <"$APPLY_LOG" | tr -d ' ')"
  assert_eq "a newer live bundle is never applied over" "0" "$apply_count"

  assert_contains "the log says the live bundle is newer" "$output" \
    "NEWER than the requested v1.1.0"
  assert_contains "the warning names the pruning this would cause" "$output" \
    "prune every field"
  assert_contains "the warning names the recreate remedy" "$output" \
    "teardown-infra"
  assert_not_contains "a downgrade is never announced as an upgrade" "$output" \
    "applying an in-place upgrade"
}

# ---------------------------------------------------------------------------
# Test 1a2: an apply refused as a downgrade fails fast with the real diagnosis
#
# An INCOMPLETE live set reaches the apply without a version comparison, so the
# bundle's own safe-upgrades ValidatingAdmissionPolicy is what refuses it. That
# message carries no "conflict", so without a dedicated match the deterministic
# denial would burn all three attempts and exit pointing at the download URL.
# ---------------------------------------------------------------------------
test_downgrade_denial_fails_fast_without_retry() {
  echo "Test: install_gateway_api_crds fails fast on a safe-upgrades denial"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  APPLY_LOG="$tmp/apply.log"
  : >"$APPLY_LOG"
  MISSING_CRDS="listenersets.gateway.networking.k8s.io"
  LIVE_BUNDLE_VERSION=""
  APPLY_FAIL=1
  APPLY_OUTPUT='error: gatewayclasses.gateway.networking.k8s.io is forbidden: ValidatingAdmissionPolicy '\''gateway.networking.k8s.io/safe-upgrades'\'' denied request: Installing CRDs with version before v1.5.0 is prohibited by default.'

  install_kubectl_stub "$tmp"

  local output exit_code
  output="$(run_install "$tmp")"
  exit_code=$?
  APPLY_OUTPUT=""

  assert_nonzero_exit "install_gateway_api_crds exits non-zero on a refused downgrade" \
    "$exit_code"

  local apply_count
  apply_count="$(wc -l <"$APPLY_LOG" | tr -d ' ')"
  assert_eq "a refused downgrade is applied exactly once, not retried" "1" "$apply_count"

  assert_contains "the ERROR names the downgrade as the cause" "$output" \
    "REFUSED because it would downgrade"
  assert_contains "the ERROR says it is deliberately not retried" "$output" \
    "deterministic"
  assert_not_contains "the misleading retry-exhaustion message is not used" "$output" \
    "after 3 attempts"
}

# ---------------------------------------------------------------------------
# Test 1b: all ten CRDs present AND the live bundle matches the pin →
# skip without applying
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

  local apply_count
  apply_count="$(wc -l <"$APPLY_LOG" | tr -d ' ')"
  assert_eq "no kubectl apply is issued when the live bundle already is the pin" \
    "0" "$apply_count"
  assert_not_contains "a matching bundle is not re-applied as an upgrade" "$output" \
    "applying an in-place upgrade"
}

# ---------------------------------------------------------------------------
# Test 1c: all ten present but no bundle-version annotation → the live version
# cannot be compared with the pin, so this stays a warn-and-return: re-applying
# an unidentifiable bundle would risk a downgrade nobody asked for
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

  local apply_count
  apply_count="$(wc -l <"$APPLY_LOG" | tr -d ' ')"
  assert_eq "an unidentifiable live bundle is warned about, never re-applied" \
    "0" "$apply_count"
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
# single presence call cannot tell "nine of ten present, co-owned by
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
  # other nine exist and are field-managed by someone else.
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
    "present: backendtlspolicies.gateway.networking.k8s.io"
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
# Test 3d: the stub's CRD universe matches the script's gwapi_crds array
#
# The stub answers "present" for every name it is not told is missing, so a
# GWAPI_ALL_CRDS that has drifted from the script's array would make the
# bare-cluster and partial-set tests vacuously green. Pin the two together.
# ---------------------------------------------------------------------------
test_gwapi_crd_list_matches_script() {
  echo "Test: the probed gwapi_crds array matches this test's CRD list"

  local script_crds
  script_crds="$(sed -n '/local gwapi_crds=(/,/^  )$/p' "$DEPLOY_INFRA_SH" \
    | grep -oE '[a-z]+\.gateway\.networking\.k8s\.io' | sort | tr '\n' ' ')"

  local expected
  expected="$(printf '%s\n' $GWAPI_ALL_CRDS | sort | tr '\n' ' ')"

  assert_eq "hack/deploy-infra.sh probes exactly the CRD names this test stubs" \
    "$expected" "$script_crds"
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
test_older_bundle_upgrades_in_place
test_newer_bundle_is_not_downgraded
test_downgrade_denial_fails_fast_without_retry
test_matching_bundle_skips_without_warning
test_unknown_bundle_version_warns
test_missing_crd_runs_apply_once
test_persistent_failure_retries_three_times
test_partial_crd_set_is_reported
test_conflict_fails_fast_without_retry
test_gwapi_crd_list_matches_script
test_main_calls_install_gateway_api_crds

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
