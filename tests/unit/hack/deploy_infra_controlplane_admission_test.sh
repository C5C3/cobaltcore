#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh `wait_for_controlplane_admission` ends the
# WITH_CONTROLPLANE step only once the API server answers a server-side
# dry-run of the ControlPlane manifests: it returns once the dry-run is
# admitted, or once every record of kubectl's stderr is a denial by a
# webhook or the CRD's schema (the answer on a re-run against a differing
# live CR); it keeps polling through a missing kind, an unreachable
# webhook, a refused connection and a stderr without a record;
# it never applies anything for real; and it exits 1 with the operators'
# pods and logs on timeout.
#
# Follows the stub-kubectl + source-and-invoke pattern of
# deploy_infra_cert_manager_webhook_test.sh. The stub answers the n-th
# dry-run with the file responses/<n>, else responses/default, else admits
# it.
#
# Usage: bash tests/unit/hack/deploy_infra_controlplane_admission_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# The stderr shapes kubectl prints for a dry-run the cluster does not admit
# yet. The first two are the ones Part 1, Step 4 of the metal-stack quick
# start met on the lab.
NO_MATCHES='error: resource mapping not found for name: "controlplane-ovn" namespace: "openstack" from "/tmp/controlplane.yaml": no matches for kind "OVNCentral" in version "ovn.openstack.c5c3.io/v1alpha1"
ensure CRDs are installed first'
WEBHOOK_UNREACHABLE='Error from server (InternalError): error when creating "/tmp/controlplane.yaml": Internal error occurred: failed calling webhook "mcontrolplane.kb.io": failed to call webhook: Post "https://c5c3-operator-webhook-service.c5c3-system.svc:443/mutate-c5c3-io-v1alpha1-controlplane?timeout=10s": dial tcp 10.96.12.34:443: connect: connection refused'
PATCH_UNREACHABLE='Error from server (InternalError): error when applying patch:
{"spec":{"region":"RegionTwo"}}
to:
Resource: "c5c3.io/v1alpha1, Resource=controlplanes", GroupVersionKind: "c5c3.io/v1alpha1, Kind=ControlPlane"
Name: "controlplane", Namespace: "openstack"
for: "/tmp/controlplane.yaml": error when patching "/tmp/controlplane.yaml": Internal error occurred: failed calling webhook "mcontrolplane.kb.io": failed to call webhook: Post "https://c5c3-operator-webhook-service.c5c3-system.svc:443/mutate-c5c3-io-v1alpha1-controlplane?timeout=10s": dial tcp 10.96.12.34:443: connect: connection refused'
CONNECTION_REFUSED='The connection to the server 127.0.0.1:6443 was refused - did you specify the right host or port?'

# The stderr shapes of a dry-run the webhooks denied.
FORBIDDEN_DENIAL='Error from server (Forbidden): error when creating "/tmp/controlplane.yaml": admission webhook "vcontrolplane.kb.io" denied the request: spec.region: Forbidden: field is immutable'
INVALID_DENIAL='The ControlPlane "controlplane" is invalid: metadata.namespace: Forbidden: only one ControlPlane is permitted per namespace; "controlplane" already exists in namespace "openstack"'
PATCH_DENIAL='Error from server (Forbidden): error when applying patch:
{"spec":{"region":"RegionTwo"}}
to:
Resource: "c5c3.io/v1alpha1, Resource=controlplanes", GroupVersionKind: "c5c3.io/v1alpha1, Kind=ControlPlane"
Name: "controlplane", Namespace: "openstack"
for: "/tmp/controlplane.yaml": error when patching "/tmp/controlplane.yaml": admission webhook "vcontrolplane.kb.io" denied the request: spec.region: Forbidden: field is immutable'
# A CRD CEL rule rejects the patch. Beside a second record, kubectl prints it
# with an "Error from server (Invalid)" prefix instead of "The <kind> ... is
# invalid".
SCHEMA_INVALID_PATCH='Error from server (Invalid): error when applying patch:
{"spec":{"northbound":{"replicas":1}}}
to:
Resource: "ovn.openstack.c5c3.io/v1alpha1, Resource=ovncentrals", GroupVersionKind: "ovn.openstack.c5c3.io/v1alpha1, Kind=OVNCentral"
Name: "controlplane-ovn", Namespace: "openstack"
for: "/tmp/ovncentral.yaml": error when patching "/tmp/ovncentral.yaml": OVNCentral.ovn.openstack.c5c3.io "controlplane-ovn" is invalid: spec.northbound: Invalid value: "object": replicas is immutable: Raft membership changes are not supported'
WARNING_LINE='Warning: spec.services.horizon.publicEndpoint "http://horizon.example" uses http://: Use https://.'
OVN_WEBHOOK_UNREACHABLE='Error from server (InternalError): error when creating "/tmp/ovncentral.yaml": Internal error occurred: failed calling webhook "movncentral.kb.io": failed to call webhook: Post "https://ovn-operator-webhook-service.ovn-system.svc:443/mutate-ovn-openstack-c5c3-io-v1alpha1-ovncentral?timeout=10s": dial tcp 10.96.56.78:443: connect: connection refused'

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# setup_logs <tmp>
# Points the stub's log files and its response directory into <tmp> and
# truncates the logs. KUBECTL_CALLS_LOG gets every invocation,
# KUBECTL_DRYRUN_LOG every `apply --dry-run=server`, KUBECTL_APPLY_LOG every
# `apply` without it, and KUBECTL_LOGS_LOG every `logs`.
setup_logs() {
  local tmp="$1"
  KUBECTL_CALLS_LOG="$tmp/calls.log"
  KUBECTL_DRYRUN_LOG="$tmp/dryrun.log"
  KUBECTL_APPLY_LOG="$tmp/apply.log"
  KUBECTL_LOGS_LOG="$tmp/logs.log"
  KUBECTL_RESPONSES="$tmp/responses"
  : >"$KUBECTL_CALLS_LOG"
  : >"$KUBECTL_DRYRUN_LOG"
  : >"$KUBECTL_APPLY_LOG"
  : >"$KUBECTL_LOGS_LOG"
  mkdir -p "$KUBECTL_RESPONSES"
}

# respond <n|default> <stderr>
# The stub fails the n-th dry-run (or every dry-run without a file of its
# own) with <stderr>.
respond() {
  printf '%s\n' "$2" >"$KUBECTL_RESPONSES/$1"
}

# install_kubectl_stub <dir>
# Writes a stub `kubectl` into <dir>. `apply --dry-run=server` answers from
# the response files set by `respond`, and admits the dry-run when none
# applies. On the diagnostic path, `get pods -n <ns> -o jsonpath=...` prints
# two pod names for <ns>. All other invocations exit 0. A no-op `sleep`
# beside it keeps the polling tests from waiting out the 5s interval.
install_kubectl_stub() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<STUB
#!/bin/bash
# Stub kubectl for tests/unit/hack/deploy_infra_controlplane_admission_test.sh.
printf '%s\n' "\$*" >>"${KUBECTL_CALLS_LOG}"
if [[ "\$1" == "apply" ]]; then
  if [[ "\$*" == *"--dry-run=server"* ]]; then
    printf '%s\n' "dry-run \$*" >>"${KUBECTL_DRYRUN_LOG}"
    n="\$(wc -l <"${KUBECTL_DRYRUN_LOG}" | tr -d ' ')"
    for response in "${KUBECTL_RESPONSES}/\${n}" "${KUBECTL_RESPONSES}/default"; do
      if [[ -f "\${response}" ]]; then
        cat "\${response}" >&2
        exit 1
      fi
    done
    echo "controlplane.c5c3.io/controlplane created (server dry run)"
    exit 0
  fi
  printf '%s\n' "apply \$*" >>"${KUBECTL_APPLY_LOG}"
  exit 0
fi
if [[ "\$1" == "get" && "\$2" == "pods" ]]; then
  if [[ "\$*" == *jsonpath* ]]; then
    printf '%s' "\$4-manager-abc \$4-webhook-def"
  else
    echo "NAME READY STATUS"
  fi
  exit 0
fi
if [[ "\$1" == "logs" ]]; then
  printf '%s\n' "logs \$*" >>"${KUBECTL_LOGS_LOG}"
  echo "fake log output"
  exit 0
fi
exit 0
STUB
  chmod +x "$dir/kubectl"

  printf '#!/bin/bash\nexit 0\n' >"$dir/sleep"
  chmod +x "$dir/sleep"
}

# run_wait <stub_dir> <arguments of wait_for_controlplane_admission...>
# Sources the script and calls the function under test in a subshell with
# PATH pointing at the stub. Echoes combined stdout/stderr; returns the exit
# status.
run_wait() {
  local stub_dir="$1"
  shift
  (
    # Keep the rest of the PATH so the real date and awk remain available
    # to the function under test.
    PATH="$stub_dir:$PATH"
    export PATH
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    wait_for_controlplane_admission "$@"
  ) 2>&1
}

line_count() {
  wc -l <"$1" | tr -d ' '
}

# ---------------------------------------------------------------------------
# Test 1: the first dry-run is admitted
# ---------------------------------------------------------------------------
test_admitted_first_poll_returns_zero() {
  echo "Test: wait_for_controlplane_admission returns 0 once the dry-run is admitted"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  setup_logs "$tmp"
  install_kubectl_stub "$tmp"

  local output exit_code
  output="$(run_wait "$tmp" 30 "$tmp/controlplane.yaml" "$tmp/ovncentral.yaml")"
  exit_code=$?

  assert_eq "exits 0 when the first dry-run is admitted" "0" "$exit_code"
  assert_contains "the wait is announced with its timeout" "$output" \
    "Waiting up to 30s for the API server to admit a server-side dry-run of the ControlPlane manifests"
  assert_contains "the admission is logged" "$output" \
    "The cluster admits the ControlPlane manifests."
  assert_eq "exactly one dry-run issued" "1" "$(line_count "$KUBECTL_DRYRUN_LOG")"
  assert_file_contains_fixed "one dry-run covers every manifest" "$KUBECTL_DRYRUN_LOG" \
    "-f $tmp/controlplane.yaml -f $tmp/ovncentral.yaml"
  assert_eq "no apply without --dry-run=server (the probe creates nothing)" \
    "0" "$(line_count "$KUBECTL_APPLY_LOG")"
  assert_eq "no diagnostics on the happy path" "0" "$(line_count "$KUBECTL_LOGS_LOG")"
}

# ---------------------------------------------------------------------------
# Test 2: a missing CRD, then an unreachable webhook, then admitted
# ---------------------------------------------------------------------------
test_polls_through_a_missing_crd_and_an_unreachable_webhook() {
  echo "Test: wait_for_controlplane_admission polls through a missing kind and an unreachable webhook"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  setup_logs "$tmp"
  install_kubectl_stub "$tmp"
  respond 1 "$NO_MATCHES"
  respond 2 "$WEBHOOK_UNREACHABLE"

  local output exit_code
  output="$(run_wait "$tmp" 60 "$tmp/controlplane.yaml")"
  exit_code=$?

  assert_eq "exits 0 once the third dry-run is admitted" "0" "$exit_code"
  assert_eq "three dry-runs issued" "3" "$(line_count "$KUBECTL_DRYRUN_LOG")"
  assert_contains "a refused dry-run is logged as not admitting yet" "$output" \
    "  The cluster is not admitting the ControlPlane manifests yet."
  assert_contains "the missing kind is surfaced, indented by four spaces" "$output" \
    '    error: resource mapping not found for name: "controlplane-ovn"'
  assert_contains "the second line of the record is surfaced too" "$output" \
    "    ensure CRDs are installed first"
  assert_contains "the unreachable webhook is surfaced" "$output" \
    'failed calling webhook "mcontrolplane.kb.io"'
  assert_contains "the admission is logged" "$output" \
    "The cluster admits the ControlPlane manifests."
  assert_not_contains "neither refusal is read as a denial" "$output" \
    "The admission webhooks answer"
  assert_eq "no apply without --dry-run=server while polling" "0" "$(line_count "$KUBECTL_APPLY_LOG")"
}

# ---------------------------------------------------------------------------
# Tests 3-7: a stderr whose every record is a denial ends the wait at once
# ---------------------------------------------------------------------------

# expect_denial_answers <description> <stderr>
expect_denial_answers() {
  local description="$1" stderr="$2"

  local tmp
  tmp="$(mktemp -d)"
  setup_logs "$tmp"
  install_kubectl_stub "$tmp"
  respond default "$stderr"

  local output exit_code
  output="$(run_wait "$tmp" 30 "$tmp/controlplane.yaml")"
  exit_code=$?

  assert_eq "${description}: exits 0" "0" "$exit_code"
  assert_eq "${description}: no further poll" "1" "$(line_count "$KUBECTL_DRYRUN_LOG")"
  assert_contains "${description}: the denial is logged as the webhooks' answer" "$output" \
    "The admission webhooks answer; the dry-run was denied:"
  assert_contains "${description}: the denial is surfaced, indented by two spaces" "$output" \
    "  $(head -n 1 <<<"$stderr")"
  assert_not_contains "${description}: no refusal is logged" "$output" \
    "not admitting the ControlPlane manifests yet"
  assert_eq "${description}: no apply without --dry-run=server" "0" "$(line_count "$KUBECTL_APPLY_LOG")"
  rm -rf "$tmp"
}

test_a_forbidden_denial_counts_as_an_answer() {
  echo "Test: a denied create with reason Forbidden is an answer"
  expect_denial_answers "Forbidden denial" "$FORBIDDEN_DENIAL"
}

test_an_invalid_denial_without_error_prefix_counts_as_an_answer() {
  echo "Test: a denial with reason Invalid, printed without an Error prefix, is an answer"
  expect_denial_answers "Invalid denial" "$INVALID_DENIAL"
}

test_a_denied_patch_record_counts_as_an_answer() {
  echo "Test: a multi-line patch record whose for: line is denied is an answer"
  expect_denial_answers "denied patch" "$PATCH_DENIAL"
}

test_a_warning_line_belongs_to_no_record() {
  echo "Test: a Warning: line before a denial belongs to no record"
  expect_denial_answers "warning and denial" "${WARNING_LINE}
${FORBIDDEN_DENIAL}"
}

test_a_schema_invalid_record_beside_a_denial_counts_as_an_answer() {
  echo "Test: a schema-Invalid patch record beside a webhook denial is an answer"
  expect_denial_answers "denial and schema invalid" "${FORBIDDEN_DENIAL}
${SCHEMA_INVALID_PATCH}"
}

# ---------------------------------------------------------------------------
# Tests 8-10: a stderr with a record that is no denial, or with no record at
# all, keeps the wait polling
# ---------------------------------------------------------------------------

# expect_polling <description> <stderr of poll 1> <stderr of poll 2|"">
# Fails the given polls and admits the next one.
expect_polling() {
  local description="$1" first="$2" second="$3"

  local tmp
  tmp="$(mktemp -d)"
  setup_logs "$tmp"
  install_kubectl_stub "$tmp"
  respond 1 "$first"
  local want=2
  if [[ -n "$second" ]]; then
    respond 2 "$second"
    want=3
  fi

  local output exit_code
  output="$(run_wait "$tmp" 60 "$tmp/controlplane.yaml" "$tmp/ovncentral.yaml")"
  exit_code=$?

  assert_eq "${description}: exits 0 once a later dry-run is admitted" "0" "$exit_code"
  assert_eq "${description}: ${want} dry-runs issued" "$want" "$(line_count "$KUBECTL_DRYRUN_LOG")"
  assert_contains "${description}: the refusal is logged" "$output" \
    "The cluster is not admitting the ControlPlane manifests yet."
  assert_not_contains "${description}: no refusal is read as a denial" "$output" \
    "The admission webhooks answer"
  assert_eq "${description}: no apply without --dry-run=server" "0" "$(line_count "$KUBECTL_APPLY_LOG")"
  rm -rf "$tmp"
}

test_a_denial_beside_a_failed_webhook_keeps_polling() {
  echo "Test: a denial beside a failed webhook call keeps the wait polling"
  expect_polling "denial and failed webhook" "${FORBIDDEN_DENIAL}
${OVN_WEBHOOK_UNREACHABLE}" ""
}

test_a_failed_webhook_patch_and_a_refused_connection_keep_polling() {
  echo "Test: a patch record with a failed webhook call and a refused connection keep the wait polling"
  expect_polling "failed patch, then refused connection" "$PATCH_UNREACHABLE" "$CONNECTION_REFUSED"
}

test_a_stderr_without_a_record_keeps_polling() {
  echo "Test: a failed dry-run whose stderr holds no record keeps the wait polling"
  expect_polling "warning only" "$WARNING_LINE" ""
}

# ---------------------------------------------------------------------------
# Test 11: timeout path
# ---------------------------------------------------------------------------
test_timeout_dumps_operator_diagnostics_and_exits_one() {
  echo "Test: wait_for_controlplane_admission dumps the operators' pods and logs and exits 1 on timeout"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  setup_logs "$tmp"
  install_kubectl_stub "$tmp"
  respond default "$WEBHOOK_UNREACHABLE"

  local output exit_code
  output="$(run_wait "$tmp" 0 "$tmp/controlplane.yaml")"
  exit_code=$?

  assert_nonzero_exit "exits non-zero on timeout" "$exit_code"
  assert_contains "the timeout is logged" "$output" \
    "ERROR: Timed out after 0s waiting for the cluster to admit the ControlPlane manifests."
  assert_file_contains_fixed "the c5c3-operator HelmRelease is printed" "$KUBECTL_CALLS_LOG" \
    "get helmrelease -n c5c3-system c5c3-operator"
  assert_file_contains_fixed "the ovn-operator HelmRelease is printed" "$KUBECTL_CALLS_LOG" \
    "get helmrelease -n ovn-system ovn-operator"
  assert_file_contains_fixed "the c5c3-system pods are listed" "$KUBECTL_CALLS_LOG" \
    "get pods -n c5c3-system -o wide"
  assert_file_contains_fixed "the ovn-system pods are listed" "$KUBECTL_CALLS_LOG" \
    "get pods -n ovn-system -o wide"
  assert_eq "kubectl logs runs once per pod (two per namespace)" "4" "$(line_count "$KUBECTL_LOGS_LOG")"
  assert_file_contains_fixed "the c5c3-system pods' logs are read" "$KUBECTL_LOGS_LOG" \
    "logs c5c3-system-manager-abc -n c5c3-system --all-containers=true"
  assert_file_contains_fixed "the ovn-system pods' logs are read" "$KUBECTL_LOGS_LOG" \
    "logs ovn-system-webhook-def -n ovn-system --all-containers=true"
  assert_file_contains_fixed "the logs are bounded to the last 10 minutes" "$KUBECTL_LOGS_LOG" \
    "--since=10m --tail=200"
  assert_eq "no apply without --dry-run=server on the timeout path" "0" "$(line_count "$KUBECTL_APPLY_LOG")"
}

# ---------------------------------------------------------------------------
# Test 12: no manifest
# ---------------------------------------------------------------------------
test_no_manifest_exits_one_before_polling() {
  echo "Test: wait_for_controlplane_admission exits 1 without a manifest, before calling kubectl"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  setup_logs "$tmp"
  install_kubectl_stub "$tmp"

  local output exit_code
  output="$(run_wait "$tmp" 30)"
  exit_code=$?

  assert_nonzero_exit "exits non-zero without a manifest" "$exit_code"
  assert_contains "the missing manifest is named" "$output" \
    "ERROR: wait_for_controlplane_admission needs at least one manifest."
  assert_eq "kubectl is never called" "0" "$(line_count "$KUBECTL_CALLS_LOG")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_admitted_first_poll_returns_zero
test_polls_through_a_missing_crd_and_an_unreachable_webhook
test_a_forbidden_denial_counts_as_an_answer
test_an_invalid_denial_without_error_prefix_counts_as_an_answer
test_a_denied_patch_record_counts_as_an_answer
test_a_warning_line_belongs_to_no_record
test_a_schema_invalid_record_beside_a_denial_counts_as_an_answer
test_a_denial_beside_a_failed_webhook_keeps_polling
test_a_failed_webhook_patch_and_a_refused_connection_keep_polling
test_a_stderr_without_a_record_keeps_polling
test_timeout_dumps_operator_diagnostics_and_exits_one
test_no_manifest_exits_one_before_polling

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
