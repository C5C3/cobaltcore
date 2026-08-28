#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh `wait_for_cert_manager_webhook` gates the
# Phase-2 TLS-prerequisite applies on the cert-manager webhook admitting a
# server-side dry-run: it returns once the dry-run passes, retries while the
# apiserver still reports the webhook unreachable or its caBundle stale,
# never applies anything for real, and surfaces diagnostics (caBundle
# presence, cert-manager pods and logs) on timeout.
#
# Follows the stub-kubectl + source-and-invoke pattern established by
# deploy_infra_gateway_wait_test.sh.
#
# Usage: bash tests/unit/hack/deploy_infra_cert_manager_webhook_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# The x509 line e2e-operator/neutron saw in run 33166067165, verbatim: the
# apiserver reached the webhook, but the caBundle in the webhook
# configuration did not match the serving certificate yet.
X509_ERROR='Error from server (InternalError): error when creating "cluster-issuer.yaml": Internal error occurred: failed calling webhook "webhook.cert-manager.io": failed to call webhook: Post "https://cert-manager-webhook.cert-manager.svc:443/validate?timeout=30s": tls: failed to verify certificate: x509: certificate signed by unknown authority'

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# install_kubectl_stub <dir> <admit_after>
# Writes a stub `kubectl` into <dir>. `apply --dry-run=server` appends a line
# to ${KUBECTL_DRYRUN_LOG} per call and fails with X509_ERROR until it has
# been called <admit_after> times, then succeeds. A real `apply` (no
# dry-run) appends to ${KUBECTL_APPLY_LOG}; the tests assert it stays empty.
# On the diagnostic path, `get validatingwebhookconfiguration/... -o
# jsonpath=...` prints a bundle and the mutating one prints nothing (so both
# branches of the caBundle report are exercised), `get pods -n cert-manager
# -o jsonpath=...` prints two pod names, and `logs ...` appends a line per
# invocation to ${KUBECTL_LOGS_LOG}. All other invocations exit 0 silently.
install_kubectl_stub() {
  local dir="$1"
  local admit_after="$2"
  mkdir -p "$dir"
  local dryrun_log="${KUBECTL_DRYRUN_LOG}"
  local apply_log="${KUBECTL_APPLY_LOG}"
  local logs_log="${KUBECTL_LOGS_LOG}"

  cat >"$dir/kubectl" <<STUB
#!/bin/bash
# Stub kubectl for tests/unit/hack/deploy_infra_cert_manager_webhook_test.sh.
if [[ "\$1" == "apply" ]]; then
  if [[ "\$*" == *"--dry-run=server"* ]]; then
    printf '%s\n' "dry-run \$*" >>"${dryrun_log}"
    if [[ "\$(wc -l <"${dryrun_log}" | tr -d ' ')" -ge ${admit_after} ]]; then
      echo "clusterissuer.cert-manager.io/selfsigned-cluster-issuer created (server dry run)"
      exit 0
    fi
    echo '${X509_ERROR}' >&2
    exit 1
  fi
  printf '%s\n' "apply \$*" >>"${apply_log}"
  exit 0
fi
if [[ "\$1" == "get" && "\$2" == validatingwebhookconfiguration/* ]]; then
  printf '%s' "LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0t"
  exit 0
fi
if [[ "\$1" == "get" && "\$2" == mutatingwebhookconfiguration/* ]]; then
  exit 0
fi
if [[ "\$1" == "get" && "\$2" == "pods" ]]; then
  if [[ "\$*" == *jsonpath* ]]; then
    printf '%s' "cert-manager-webhook-abc cert-manager-cainjector-def"
  else
    echo "NAME READY STATUS"
  fi
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
    # rest of the PATH so real date/sleep/basename/wc remain available — the
    # function under test does deadline math with `$(date +%s)`.
    PATH="$stub_dir:$PATH"
    export PATH KUBECTL_DRYRUN_LOG KUBECTL_APPLY_LOG KUBECTL_LOGS_LOG
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    wait_for_cert_manager_webhook "$stub_dir/cluster-issuer.yaml" "${timeout_arg}"
  ) 2>&1
}

# setup_logs <tmp>
# Points the three stub log files into <tmp> and truncates them.
setup_logs() {
  local tmp="$1"
  KUBECTL_DRYRUN_LOG="$tmp/dryrun.log"
  KUBECTL_APPLY_LOG="$tmp/apply.log"
  KUBECTL_LOGS_LOG="$tmp/logs.log"
  : >"$KUBECTL_DRYRUN_LOG"
  : >"$KUBECTL_APPLY_LOG"
  : >"$KUBECTL_LOGS_LOG"
}

line_count() {
  wc -l <"$1" | tr -d ' '
}

# ---------------------------------------------------------------------------
# Test 1: happy path — the first dry-run is admitted, nothing else happens
# ---------------------------------------------------------------------------
test_admitted_first_poll_returns_zero() {
  echo "Test: wait_for_cert_manager_webhook returns 0 once the dry-run is admitted"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  setup_logs "$tmp"
  install_kubectl_stub "$tmp" 1

  local output exit_code
  output="$(run_wait "$tmp" 30)"
  exit_code=$?

  assert_eq "exits 0 when the first dry-run is admitted" "0" "$exit_code"
  assert_contains "log line announces the wait with the manifest name" "$output" \
    "Waiting up to 30s for the cert-manager webhook to admit a server-side dry-run of cluster-issuer.yaml"
  assert_contains "log line reports the webhook admitting" "$output" \
    "cert-manager webhook admits requests."
  assert_eq "exactly one dry-run issued" "1" "$(line_count "$KUBECTL_DRYRUN_LOG")"
  assert_file_contains "the dry-run targets the manifest it was given" \
    "$KUBECTL_DRYRUN_LOG" "cluster-issuer.yaml"
  assert_eq "no real apply issued (the probe must not create anything)" "0" "$(line_count "$KUBECTL_APPLY_LOG")"
  assert_eq "no diagnostics dumped on the happy path" "0" "$(line_count "$KUBECTL_LOGS_LOG")"
}

# ---------------------------------------------------------------------------
# Test 2: retry path — the first dry-run hits the x509 race, the second passes
# (one real 5s poll interval elapses here)
# ---------------------------------------------------------------------------
test_retries_until_admitted() {
  echo "Test: wait_for_cert_manager_webhook retries a rejected dry-run until it is admitted"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  setup_logs "$tmp"
  install_kubectl_stub "$tmp" 2

  local output exit_code
  output="$(run_wait "$tmp" 30)"
  exit_code=$?

  assert_eq "exits 0 once a later dry-run is admitted" "0" "$exit_code"
  assert_contains "the rejected attempt is logged" "$output" \
    "cert-manager webhook is not admitting yet."
  assert_contains "the apiserver's error is surfaced verbatim" "$output" \
    "x509: certificate signed by unknown authority"
  assert_contains "the admitted attempt is logged" "$output" \
    "cert-manager webhook admits requests."
  assert_eq "two dry-runs issued" "2" "$(line_count "$KUBECTL_DRYRUN_LOG")"
  assert_eq "no real apply issued while retrying" "0" "$(line_count "$KUBECTL_APPLY_LOG")"
  assert_eq "no diagnostics dumped when a retry succeeds" "0" "$(line_count "$KUBECTL_LOGS_LOG")"
}

# ---------------------------------------------------------------------------
# Test 3: timeout path — surfaces diagnostics and exits non-zero
# ---------------------------------------------------------------------------
test_timeout_dumps_diagnostics() {
  echo "Test: wait_for_cert_manager_webhook dumps caBundle state + pod logs and exits non-zero on timeout"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  setup_logs "$tmp"
  install_kubectl_stub "$tmp" 999

  # Timeout of 0 means the deadline is reached on the first iteration, so the
  # function logs one rejected attempt, then emits the diagnostics and exits
  # 1 without sleeping.
  local output exit_code
  output="$(run_wait "$tmp" 0)"
  exit_code=$?

  assert_nonzero_exit "exits non-zero on timeout" "$exit_code"
  assert_contains "timeout message surfaced" "$output" \
    "ERROR: Timed out waiting for the cert-manager webhook after 0s."
  assert_contains "validating webhook caBundle reported as injected" "$output" \
    "validatingwebhookconfiguration/cert-manager-webhook: caBundle injected"
  assert_contains "mutating webhook caBundle reported as missing" "$output" \
    "mutatingwebhookconfiguration/cert-manager-webhook: caBundle missing"
  assert_contains "cert-manager pod log header surfaced" "$output" \
    "cert-manager pod logs"

  assert_eq "kubectl logs invoked once per cert-manager pod (2 pods)" \
    "2" "$(line_count "$KUBECTL_LOGS_LOG")"
  assert_file_contains "logs scoped to the cert-manager namespace" \
    "$KUBECTL_LOGS_LOG" "cert-manager --all-containers=true"
  # Pattern omits the leading `--` so the shared helper's plain `grep -q`
  # matches the flag literally.
  assert_file_contains "logs request a --since=10m window to bound the diagnostic output" \
    "$KUBECTL_LOGS_LOG" "since=10m"
  assert_eq "no real apply issued on the timeout path either" "0" "$(line_count "$KUBECTL_APPLY_LOG")"
}

# ---------------------------------------------------------------------------
# Test 4: main() probes the webhook with cluster-issuer.yaml between Phase 1
# (cert-manager Ready) and Phase 2 (the TLS-prerequisite applies), bounded
# by the documented WEBHOOK_TIMEOUT default.
# Static-text check so this test stays independent of the stub plumbing
# above.
# ---------------------------------------------------------------------------
test_main_probes_webhook_between_phase_1_and_2() {
  echo "Test: main() gates Phase 2 on wait_for_cert_manager_webhook"

  assert_file_contains "WEBHOOK_TIMEOUT defaults to 120s" \
    "$DEPLOY_INFRA_SH" 'WEBHOOK_TIMEOUT="${WEBHOOK_TIMEOUT:-120}"'
  assert_file_contains "the probe uses the ClusterIssuer manifest Phase 2 applies first" \
    "$DEPLOY_INFRA_SH" 'wait_for_cert_manager_webhook "${REPO_ROOT}/deploy/flux-system/infrastructure/cluster-issuer.yaml" "${WEBHOOK_TIMEOUT}"'

  local phase1_line probe_line phase2_line
  phase1_line="$(grep -n 'log "Phase 1: Waiting for cert-manager' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  probe_line="$(grep -n '^  wait_for_cert_manager_webhook ' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  phase2_line="$(grep -n 'log "Phase 2: Applying TLS prerequisites' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  assert_not_empty "Phase 1 log line found" "$phase1_line"
  assert_not_empty "webhook probe call found in main()" "$probe_line"
  assert_not_empty "Phase 2 log line found" "$phase2_line"
  assert_gte "the probe runs after Phase 1" "$probe_line" "$phase1_line"
  assert_gte "the probe runs before Phase 2" "$phase2_line" "$probe_line"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_admitted_first_poll_returns_zero
test_retries_until_admitted
test_timeout_dumps_diagnostics
test_main_probes_webhook_between_phase_1_and_2

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
