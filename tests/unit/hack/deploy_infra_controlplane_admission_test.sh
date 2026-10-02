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
# pods and logs on timeout. The static tests at the end check the
# WITH_CONTROLPLANE block of main(): the ten-release and CRD waits behind
# the INFRA_ONLY gate, a probe before each of the three endings, and one
# apply of the bundled CR that ends the run when it fails. The last test
# runs preflight_checks, which refuses the bundled CR under INFRA_ONLY=true.
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

# line_between <fixed string> <after> <before>
# Prints the number of the first line of deploy-infra.sh that holds the
# string and lies strictly between the two line numbers.
line_between() {
  awk -v s="$1" -v a="$2" -v b="$3" \
    'NR > a && NR < b && index($0, s) { print NR; exit }' "$DEPLOY_INFRA_SH"
}

# line_of <fixed string> — the first line of deploy-infra.sh holding it.
line_of() {
  line_between "$1" 0 999999
}

# last_line_between <fixed string> <after> <before>
# Like line_between, but prints the last such line.
last_line_between() {
  awk -v s="$1" -v a="$2" -v b="$3" \
    'NR > a && NR < b && index($0, s) { n = NR } END { if (n) print n }' "$DEPLOY_INFRA_SH"
}

# continued_call <fixed string>
# Prints every command of deploy-infra.sh whose first line holds the string,
# its backslash-continued lines joined into one line.
continued_call() {
  awk -v s="$1" '
    !inside && index($0, s) { inside = 1; call = "" }
    inside {
      line = $0
      more = sub(/\\[[:space:]]*$/, "", line)
      call = call " " line
      if (!more) { print call; inside = 0 }
    }
  ' "$DEPLOY_INFRA_SH"
}

# assert_before <description> <line a> <line b> — a is a line before b.
assert_before() {
  if [ -n "$2" ] && [ -n "$3" ] && [ "$2" -lt "$3" ]; then
    echo "  PASS: $1"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $1 (line '${2}' is not before line '${3}')"
    FAIL=$((FAIL + 1))
  fi
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
# Tests 13-16: the WITH_CONTROLPLANE block of main() (static text, so these
# stay independent of the stub plumbing above)
# ---------------------------------------------------------------------------
test_main_waits_for_every_operator_release_and_its_crd() {
  echo "Test: main() waits for every operator release of the flux path, then one CRD per operator"

  # The expected set is the flux path's un-suspend list, which
  # check-service-parity requires of every service the kind base suspends,
  # plus the c5c3-operator, which the base applies active. An operator that
  # is added to that list but missing from either wait fails here.
  local unsuspended releases_call want got
  unsuspended="$(awk '
    pending && index($0, "\"suspend\":false") { print pending }
    { pending = "" }
    $1 == "kubectl" && $2 == "patch" && $3 == "helmrelease" && $4 ~ /^[a-z0-9-]+-operator$/ &&
      $5 == "-n" && $NF == "\\" { pending = $6 "/" $4 }
  ' "$DEPLOY_INFRA_SH")"
  assert_not_empty "the flux path un-suspends service operator releases" "$unsuspended"
  want="$(printf '%s\nc5c3-system/c5c3-operator\n' "$unsuspended" | sort -u | tr '\n' ' ' | sed 's/ $//')"
  releases_call="$(continued_call 'wait_for_helmreleases "${HELMRELEASE_TIMEOUT}"' |
    grep -F 'c5c3-system/c5c3-operator' || true)"
  assert_not_empty "a HELMRELEASE_TIMEOUT release wait names c5c3-system/c5c3-operator" "$releases_call"
  got="$(tr ' ' '\n' <<<"$releases_call" | grep -E '^[a-z0-9-]+/[a-z0-9-]+$' | sort | tr '\n' ' ' | sed 's/ $//' || true)"
  assert_eq "the wait names every un-suspended release and the c5c3-operator, namespace-qualified, and nothing else" "$want" "$got"
  assert_file_not_contains "no soft operator wait remains" "$DEPLOY_INFRA_SH" 'not Ready yet (continuing'

  local releases_end crds_line
  releases_end="$(line_of 'nova-system/nova-operator c5c3-system/c5c3-operator')"
  crds_line="$(line_between 'wait_for_crds "${POD_TIMEOUT}"' "$releases_end" 999999)"
  assert_eq "the CRD wait follows the release wait directly" "$((releases_end + 1))" "$crds_line"

  local crds_call names name plural group op file count=0 ops
  crds_call="$(continued_call 'wait_for_crds "${POD_TIMEOUT}"' | grep -F 'controlplanes.c5c3.io' || true)"
  assert_not_empty "a POD_TIMEOUT CRD wait names controlplanes.c5c3.io" "$crds_call"
  names="$(tr ' ' '\n' <<<"$crds_call" | grep -E '^[a-z0-9]+(\.[a-z0-9]+)+$' || true)"
  while IFS= read -r name; do
    [[ -z "$name" ]] && continue
    count=$((count + 1))
    plural="${name%%.*}"
    group="${name#*.}"
    op="${group%%.*}"
    file="operators/${op}/helm/${op}-operator/crds/${group}_${plural}.yaml"
    if [[ -f "$PROJECT_ROOT/$file" ]]; then
      echo "  PASS: ${name} ships as ${file}"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: ${name} has no ${file}"
      FAIL=$((FAIL + 1))
    fi
  done <<<"$names"
  assert_eq "the CRD wait names one CRD per awaited release" "$(wc -w <<<"$want" | tr -d ' ')" "$count"
  ops="$(sed -E 's/^[a-z0-9]+\.([a-z0-9]+).*$/\1/' <<<"$names" | sort -u | tr '\n' ' ' | sed 's/ $//')"
  assert_eq "one CRD per operator" \
    "$(tr ' ' '\n' <<<"$want" | sed -E 's#^.*/([a-z0-9]+)-operator$#\1#' | sort | tr '\n' ' ' | sed 's/ $//')" "$ops"
}

test_main_applies_the_bundled_cr_once_after_the_probe() {
  echo "Test: main() applies the bundled ControlPlane CR once, after the probe, and fails loudly"

  assert_file_not_contains "the apply retry counter is gone" "$DEPLOY_INFRA_SH" 'cp_attempt'
  assert_file_not_contains "the warm-up retry message is gone" "$DEPLOY_INFRA_SH" 'webhook warming up?'

  local apply_lines
  apply_lines="$(grep -F 'kubectl apply -f "${cp_manifest}"' "$DEPLOY_INFRA_SH" || true)"
  assert_eq "the bundled CR is applied on one line" "1" "$(grep -c . <<<"$apply_lines")"
  assert_not_contains "the apply keeps its stderr" "$apply_lines" "2>/dev/null"

  local apply_line fi_line branch applied_line render_line probe_line
  apply_line="$(line_of 'if ! kubectl apply -f "${cp_manifest}"; then')"
  assert_not_empty "the apply is tested for failure" "$apply_line"
  fi_line="$(awk -v a="${apply_line:-0}" 'NR > a && /^[[:space:]]*fi$/ { print NR; exit }' "$DEPLOY_INFRA_SH")"
  branch="$(sed -n "${apply_line:-1},${fi_line:-1}p" "$DEPLOY_INFRA_SH")"
  assert_contains "a failed apply is named" "$branch" "applying the ControlPlane CR failed"
  assert_contains "a failed apply ends the run" "$branch" "exit 1"
  applied_line="$(line_of 'ControlPlane CR applied (WITH_CONTROLPLANE_CR=true)')"
  assert_before "'ControlPlane CR applied' follows the failure branch" "$fi_line" "$applied_line"

  render_line="$(line_of 'render_controlplane_replicas "${cp_manifest}"')"
  probe_line="$(line_between 'wait_for_controlplane_admission "${WEBHOOK_TIMEOUT}" "${cp_manifest}"' \
    "${render_line:-0}" "${apply_line:-0}")"
  assert_not_empty "the bundled CR is probed between its last rewrite and its apply" "$probe_line"
}

test_main_probes_before_each_by_hand_hint() {
  echo "Test: main() probes the manifests of each by-hand hint before printing it"

  local gate hint_overlay hint_kind
  gate="$(line_of 'elif [[ "${EXTERNAL_CLUSTER}" == "true" && -f "${OVERLAY_ROOT}/controlplane/kustomization.yaml" ]]; then')"
  hint_overlay="$(line_of 'kubectl apply -k ${OVERLAY_ROOT}/controlplane')"
  hint_kind="$(line_of 'kubectl apply -f deploy/kind/controlplane/controlplane.yaml')"
  assert_not_empty "the overlay ending's gate found" "$gate"
  assert_not_empty "the overlay hint found" "$hint_overlay"
  assert_not_empty "the kind hint found" "$hint_kind"
  gate="${gate:-0}"
  hint_overlay="${hint_overlay:-0}"
  hint_kind="${hint_kind:-0}"

  assert_not_empty "the overlay ending renders the overlay's controlplane directory" \
    "$(line_between 'kubectl kustomize "${OVERLAY_ROOT}/controlplane"' "$gate" "$hint_overlay")"
  assert_not_empty "the overlay ending probes before its hint" \
    "$(line_between 'wait_for_controlplane_admission "${WEBHOOK_TIMEOUT}"' "$gate" "$hint_overlay")"
  local render_err
  render_err="$(line_between 'cannot render ${OVERLAY_ROOT}/controlplane (the error is above).' "$gate" "$hint_overlay")"
  assert_not_empty "the overlay ending names a failed render" "$render_err"
  assert_not_empty "a failed overlay render ends the run" \
    "$(line_between 'exit 1' "${render_err:-0}" "$((${render_err:-0} + 3))")"

  local probe
  probe="$(line_between 'wait_for_controlplane_admission "${WEBHOOK_TIMEOUT}"' "$hint_overlay" "$hint_kind")"
  assert_not_empty "the kind ending probes before its hint" "$probe"
  assert_contains "the kind probe names the bundled CR the hint applies" \
    "$(sed -n "${probe:-1}p" "$DEPLOY_INFRA_SH")" '"${REPO_ROOT}/deploy/kind/controlplane/controlplane.yaml"'
  assert_file_contains_fixed "the bundled CR the kind probe names is a ControlPlane" \
    "$PROJECT_ROOT/deploy/kind/controlplane/controlplane.yaml" 'kind: ControlPlane'
  assert_contains "the kind probe adds the OVNCentral of Step 3" \
    "$(sed -n "${probe:-1}p" "$DEPLOY_INFRA_SH")" '"${REPO_ROOT}/deploy/lab/metal-stack/controlplane/ovncentral.yaml"'
}

test_main_gates_the_waits_on_infra_only() {
  echo "Test: main() runs the operator waits only on the flux path without INFRA_ONLY"

  local gate_count
  gate_count="$(grep -cF '"${INFRA_ONLY}" == "true"' "$DEPLOY_INFRA_SH" || true)"
  assert_eq "deploy-infra.sh still has exactly 1 strict INFRA_ONLY==true gate" "1" "$gate_count"

  local infra_gate flux_branch external_branch crds awaited skip
  infra_gate="$(line_of 'if [[ "${INFRA_ONLY}" != "true" ]]; then')"
  assert_not_empty "the operator waits are gated on INFRA_ONLY != true" "$infra_gate"
  infra_gate="${infra_gate:-0}"
  assert_contains "the gate opens with the ten-release wait" \
    "$(sed -n "$((infra_gate + 1))p" "$DEPLOY_INFRA_SH")" 'wait_for_helmreleases "${HELMRELEASE_TIMEOUT}"'
  flux_branch="$(last_line_between 'if [[ "${CONTROLPLANE_OPERATORS}" == "flux" ]]; then' 0 "$infra_gate")"
  external_branch="$(line_of '# CONTROLPLANE_OPERATORS=external: the Flux stack is suspended')"
  assert_before "the gate sits in the flux branch" "$flux_branch" "$infra_gate"
  assert_before "the gate sits before the external branch" "$infra_gate" "$external_branch"

  crds="$(line_between 'novas.nova.openstack.c5c3.io' "$infra_gate" 999999)"
  awaited="$(line_between 'operators_awaited=true' "$infra_gate" 999999)"
  skip="$(line_of 'Skipping the operator waits and the admission probe (INFRA_ONLY=true')"
  assert_before "operators_awaited turns true after the CRD wait" "$crds" "$awaited"
  assert_before "the INFRA_ONLY skip is logged after the waits" "$awaited" "$skip"
  assert_before "the INFRA_ONLY skip is logged in the flux branch" "$skip" "$external_branch"
  assert_before "operators_awaited starts false before the WITH_CONTROLPLANE banner" \
    "$(line_of 'local operators_awaited=false')" "$(line_of '=== WITH_CONTROLPLANE: bringing up')"

  local gate hint_overlay hint_kind probe awaited_gate
  gate="$(line_of 'elif [[ "${EXTERNAL_CLUSTER}" == "true" && -f "${OVERLAY_ROOT}/controlplane/kustomization.yaml" ]]; then')"
  hint_overlay="$(line_of 'kubectl apply -k ${OVERLAY_ROOT}/controlplane')"
  hint_kind="$(line_of 'kubectl apply -f deploy/kind/controlplane/controlplane.yaml')"
  probe="$(line_between 'wait_for_controlplane_admission "${WEBHOOK_TIMEOUT}"' "${gate:-0}" "${hint_overlay:-0}")"
  awaited_gate="$(last_line_between 'if [[ "${operators_awaited}" == "true" ]]; then' "${gate:-0}" "${probe:-0}")"
  assert_not_empty "the overlay probe runs only once the operators were awaited" "$awaited_gate"
  probe="$(line_between 'wait_for_controlplane_admission "${WEBHOOK_TIMEOUT}"' "${hint_overlay:-0}" "${hint_kind:-0}")"
  awaited_gate="$(last_line_between 'if [[ "${operators_awaited}" == "true" ]]; then' "${hint_overlay:-0}" "${probe:-0}")"
  assert_not_empty "the kind probe runs only once the operators were awaited" "$awaited_gate"
}

# ---------------------------------------------------------------------------
# Test 17: preflight refuses the bundled CR on a cluster without operators
# ---------------------------------------------------------------------------

# run_preflight <stub_dir> [env_var=value...]
# Sources the script with the given overrides and runs preflight_checks with
# exit-0 stubs of the tools it looks for first on the PATH. Echoes combined
# stdout/stderr; returns the exit status.
run_preflight() {
  local stub_dir="$1"
  shift
  (
    unset EXTERNAL_CLUSTER INFRA_ONLY WITH_CONTROLPLANE WITH_CONTROLPLANE_CR
    for assignment in "$@"; do
      export "${assignment?}"
    done
    local tool
    for tool in docker kind kubectl jq yq; do
      printf '#!/bin/bash\nexit 0\n' >"$stub_dir/$tool"
      chmod +x "$stub_dir/$tool"
    done
    PATH="$stub_dir:$PATH"
    export PATH
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    preflight_checks
  ) 2>&1
}

test_preflight_refuses_infra_only_with_the_bundled_cr() {
  echo "Test: preflight refuses INFRA_ONLY=true together with WITH_CONTROLPLANE_CR=true"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  local output exit_code
  output="$(run_preflight "$tmp" INFRA_ONLY=true WITH_CONTROLPLANE=true WITH_CONTROLPLANE_CR=true)"
  exit_code=$?
  assert_nonzero_exit "the combination fails preflight" "$exit_code"
  assert_contains "the refusal names both flags and the reason" "$output" \
    "ERROR: INFRA_ONLY=true does not support WITH_CONTROLPLANE_CR=true: this cluster runs no CobaltCore operator to admit or reconcile the ControlPlane CR."

  output="$(run_preflight "$tmp" INFRA_ONLY=true WITH_CONTROLPLANE=true WITH_CONTROLPLANE_CR=false)"
  exit_code=$?
  assert_eq "INFRA_ONLY=true with the by-hand ending passes preflight" "0" "$exit_code"
  assert_not_contains "no refusal without WITH_CONTROLPLANE_CR=true" "$output" "does not support"

  output="$(run_preflight "$tmp" INFRA_ONLY=true WITH_CONTROLPLANE=false WITH_CONTROLPLANE_CR=true)"
  exit_code=$?
  assert_eq "WITH_CONTROLPLANE_CR=true is ignored without WITH_CONTROLPLANE=true" "0" "$exit_code"

  output="$(run_preflight "$tmp" INFRA_ONLY=false WITH_CONTROLPLANE=true WITH_CONTROLPLANE_CR=true)"
  exit_code=$?
  assert_eq "the bundled CR passes preflight on a cluster with operators" "0" "$exit_code"
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
test_main_waits_for_every_operator_release_and_its_crd
test_main_applies_the_bundled_cr_once_after_the_probe
test_main_probes_before_each_by_hand_hint
test_main_gates_the_waits_on_infra_only
test_preflight_refuses_infra_only_with_the_bundled_cr

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
