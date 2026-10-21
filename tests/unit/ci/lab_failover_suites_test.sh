#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the static contract of the lab failover suites and fixtures under
# tests/lab-failover/ (#1393):
#   1. The suite directories are the scenarios of hack/lab-failover.sh, and
#      the check fails on a copy that lacks one.
#   2. Each suite has one `# Chaos budget:` line in the form the harness's
#      BUDGET_RE reads and one `# Chaos budget measured:` line after it,
#      spec.namespace openstack-ha, one lf_workload_start and every
#      workload.sh call with the directory name as its id, one verify with
#      --placement-audit cp-ha-nova --ovn-db-sync cp-ha-neutron, one
#      fault-start and one fault-end mark, a stop in a cleanup, an lf_dump in
#      the catch of every step, a timeout on every script, and every chaos
#      manifest's delete in a finally.
#   3. Every chaos manifest is in openstack-ha, selects pods of openstack-ha
#      only, carries a duration unless it is a pod-kill, and its @KEY@
#      placeholders are the keys of its suite's lf_render call; the check
#      fails on a copy with a placeholder the call leaves out.
#   4. Nothing under tests/lab-failover/ names allowHostNetworkTesting,
#      nodes: or nodeSelectors:, and no script tests a cp_status or cp_reason
#      read inside `[`, where bash drops the read's exit status.
#   5. The fixtures: ControlPlane openstack-ha/cp-ha with profile Standard and
#      no profileRef, gateway, publicEndpoint, hypervisorOperator or
#      backupBackend, centralRef cp-ha-ovn and the Horizon key Secret; an
#      OVNCentral without replicas; no OpenBaoTenant, because the c5c3-operator
#      admits openstack-ha itself; the ExternalSecret's remote key and tenant
#      store;
#      the seed image id and flavor of the seed Job, which the library's
#      LF_SEED_IMAGE and LF_SEED_FLAVOR name, and the compute id of the lab;
#      the purge Job cp-ha-purge runs on the seed Job's image, env and
#      securityContext, every attempt of its script gives up on a deleting
#      volume before activeDeadlineSeconds deletes the pod and its log, and
#      the deadline's pods are gone before down stops waiting for it after
#      LF_WAIT_PURGE.
#   6. Every new file carries the SPDX pair, and make shellcheck lints the
#      library.
#
# The checks of 2 and 3 that read the YAML structure need mikefarah yq and
# are skipped without it, as in tests/unit/ci/chaos_workload_test.sh.
#
# Usage: bash tests/unit/ci/lab_failover_suites_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
LF_DIR="$PROJECT_ROOT/tests/lab-failover"
LF_SH="$PROJECT_ROOT/hack/lab-failover.sh"
FIXTURES="$LF_DIR/fixtures"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# The two budget lines, as tests/e2e-chaos/lib/workload.sh and
# tests/unit/ci/chaos_workload_test.sh read them.
BUDGET_RE='^# Chaos budget: failures=([0-9]+) recovery=([0-9]+)s$'
MEASURED_RE='^# Chaos budget measured: [0-9]{4}-[0-9]{2}-[0-9]{2}, [a-z]+, failures [0-9]+(/[0-9]+){0,2}, recovery [0-9]+(/[0-9]+){0,2} s$'

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

have_yq() {
  command -v yq >/dev/null 2>&1 && yq --version 2>&1 | grep -q 'mikefarah'
}

# data_lines <file> — The lines of a YAML file without its comments.
data_lines() {
  grep -v '^[[:space:]]*#' "$1"
}

# scenario_problems <script> <suites dir> — One line per scenario of the
# script without a suite directory, and per suite directory without a
# scenario.
scenario_problems() {
  local scenarios suites file
  scenarios="$(bash -c '. "$1" && printf "%s\n" "${LF_SCENARIOS[@]}"' _ "$1" | sort)"
  suites="$(for file in "$2"/*/chainsaw-test.yaml; do
    if [[ -e "$file" ]]; then basename "$(dirname "$file")"; fi
  done | sort)"
  comm -23 <(echo "$scenarios") <(echo "$suites") | sed 's/^/scenario without a suite directory: /'
  comm -13 <(echo "$scenarios") <(echo "$suites") | sed 's/^/suite directory without a scenario: /'
}

# placeholder_problems <suite dir> — One line per chaos manifest whose @KEY@
# placeholders differ from the keys of the suite's lf_render call for it, or
# that no lf_render call renders.
placeholder_problems() {
  local dir="$1" manifest name call tokens keys
  for manifest in "$dir"/[0-9][0-9]-*.yaml; do
    [[ -e "$manifest" ]] || continue
    name="$(basename "$manifest")"
    call="$(grep -oE "lf_render ${name}( [A-Z_]+=[^ |]+)*" "$dir/chainsaw-test.yaml" || true)"
    if [[ -z "$call" ]]; then
      echo "$(basename "$dir")/${name}: no lf_render call renders it"
      continue
    fi
    tokens="$(grep -oE '@[A-Z_]+@' "$manifest" | tr -d '@' | sort -u | xargs)"
    keys="$(grep -oE ' [A-Z_]+=' <<<"$call" | tr -d ' =' | sort -u | xargs)"
    if [[ "$tokens" != "$keys" ]]; then
      echo "$(basename "$dir")/${name}: placeholders '${tokens}', lf_render fills '${keys}'"
    fi
  done
}

# --- Test 1 ---------------------------------------------------------------
test_scenarios_are_the_directories() {
  echo "Test: the suite directories are the scenarios of hack/lab-failover.sh"
  assert_eq "no difference" "" "$(scenario_problems "$LF_SH" "$LF_DIR")"
  assert_eq "eleven scenarios" "11" \
    "$(bash -c '. "$1" && echo "${#LF_SCENARIOS[@]}"' _ "$LF_SH")"

  local copy="$TMP_DIR/scenarios"
  mkdir -p "$copy"
  cp -R "$LF_DIR"/. "$copy/"
  mv "$copy/api-eviction-drain" "$copy/api-drain"
  assert_contains "a missing directory is a problem" "$(scenario_problems "$LF_SH" "$copy")" \
    "scenario without a suite directory: api-eviction-drain"
  assert_contains "an extra directory is a problem" "$(scenario_problems "$LF_SH" "$copy")" \
    "suite directory without a scenario: api-drain"
}

# --- Test 2 ---------------------------------------------------------------
check_suite() {
  local dir="$1" suite file budget measured ids
  suite="$(basename "$dir")"
  file="$dir/chainsaw-test.yaml"
  assert_eq "$suite: one budget line" "1" "$(grep -c '^# Chaos budget:' "$file")"
  budget="$(grep -m1 '^# Chaos budget:' "$file")"
  assert_eq "$suite: the budget line reads failures=<N> recovery=<S>s" "yes" \
    "$([[ "$budget" =~ $BUDGET_RE ]] && echo yes || echo "no: $budget")"
  assert_eq "$suite: one measured line" "1" "$(grep -c '^# Chaos budget measured:' "$file")"
  measured="$(grep -A1 '^# Chaos budget:' "$file" | sed -n 2p)"
  assert_eq "$suite: the measured line follows it" "yes" \
    "$([[ "$measured" =~ $MEASURED_RE ]] && echo yes || echo "no: $measured")"
  assert_eq "$suite: spec.namespace is openstack-ha" "1" "$(grep -cx '  namespace: openstack-ha' "$file")"
  ids="$(grep -oE 'workload\.sh (start|mark|verify|dump|stop) [^ ]+' "$file" | awk '{ print $3 }' | sort -u)"
  assert_eq "$suite: every workload.sh call uses the directory name as its id" "$suite" "$ids"
  assert_eq "$suite: one verify with both consistency checks" "1" \
    "$(grep -c "workload\.sh verify $suite --placement-audit cp-ha-nova --ovn-db-sync cp-ha-neutron$" "$file")"
  assert_eq "$suite: one verify at all" "1" "$(grep -c "workload\.sh verify" "$file")"
  assert_eq "$suite: one fault-start mark" "1" "$(grep -c "workload\.sh mark $suite fault-start" "$file")"
  assert_eq "$suite: one fault-end mark" "1" "$(grep -c "workload\.sh mark $suite fault-end" "$file")"
  assert_eq "$suite: one lf_workload_start with the directory name as its id" "1" \
    "$(grep -cE "^ +lf_workload_start $suite [a-z,]+$" "$file")"
  if ! have_yq; then
    SKIP=$((SKIP + 4))
    return 0
  fi
  assert_eq "$suite: a stop in a cleanup" "1" "$(yq 'explode(.) | [.spec.steps[].cleanup[]? | select(has("script"))
    | select(.script.content == "../../e2e-chaos/lib/workload.sh stop '"$suite"'")] | length' "$file")"
  assert_eq "$suite: the catch of every step calls lf_dump" "true" "$(yq 'explode(.) | [.spec.steps[]
    | [(.catch // [])[] | .script.content // ""] | join("\n") | test("(?m)^lf_dump$")] | all' "$file")"
  assert_eq "$suite: every script has a timeout" "0" "$(yq 'explode(.) | [.spec.steps[]
    | (.try, .catch, .cleanup, .finally) | .[]? | select(has("script")) | select(.script.timeout == null)] | length' "$file")"
  local manifest kind name finally missing=""
  finally="$(yq 'explode(.) | [.spec.steps[].finally[]? | .script.content // ""] | join("\n")' "$file")"
  for manifest in "$dir"/[0-9][0-9]-*.yaml; do
    [[ -e "$manifest" ]] || continue
    kind="$(yq '.kind | downcase' "$manifest")"
    name="$(yq '.metadata.name' "$manifest")"
    grep -qx "lf_delete_chaos ${kind} ${name}" <<<"$finally" || missing="${missing} ${kind}/${name}"
  done
  assert_eq "$suite: every chaos manifest is deleted in a finally" "" "$missing"
}

test_suites() {
  echo "Test: each suite keeps the harness contract and lifts its fault"
  local dir
  for dir in "$LF_DIR"/*/; do
    [[ -f "$dir/chainsaw-test.yaml" ]] || continue
    check_suite "${dir%/}"
  done
}

# --- Test 3 ---------------------------------------------------------------
test_manifests() {
  echo "Test: every chaos manifest stays in openstack-ha, self-limits and is filled by its lf_render call"
  local dir manifest problems="" copy
  for dir in "$LF_DIR"/*/; do
    [[ -f "$dir/chainsaw-test.yaml" ]] || continue
    problems="${problems}$(placeholder_problems "${dir%/}")"
  done
  assert_eq "every placeholder is filled and every manifest rendered" "" "$problems"
  assert_eq "eleven suites, ten chaos manifests" "10" "$(find "$LF_DIR" -mindepth 2 -maxdepth 2 -name '[0-9][0-9]-*chaos.yaml' | wc -l | tr -d ' ')"

  copy="$TMP_DIR/placeholders"
  mkdir -p "$copy"
  cp -R "$LF_DIR/galera-primary-loss" "$copy/"
  sed -i.bak 's/ POD_B="\$next"//' "$copy/galera-primary-loss/chainsaw-test.yaml"
  assert_contains "a placeholder the call leaves out is a problem" "$(placeholder_problems "$copy/galera-primary-loss")" \
    "galera-primary-loss/01-podchaos.yaml: placeholders 'POD_A POD_B', lf_render fills 'POD_A'"
  printf 'metadata:\n  name: x\n' >"$copy/galera-primary-loss/02-podchaos.yaml"
  assert_contains "a manifest no call renders is a problem" "$(placeholder_problems "$copy/galera-primary-loss")" \
    "galera-primary-loss/02-podchaos.yaml: no lf_render call renders it"

  if ! have_yq; then
    echo "  SKIP: yq not installed (the field checks of the manifests skipped)"
    SKIP=$((SKIP + 1))
    return 0
  fi
  local bad=""
  for manifest in "$LF_DIR"/*/[0-9][0-9]-*chaos.yaml; do
    bad="${bad}$(yq '
      select(.metadata.namespace != "openstack-ha"
        or ([(.spec.selector.pods, .spec.target.selector.pods) | select(. != null) | keys | .[]] | unique | join(",")) != "openstack-ha"
        or (.spec.action != "pod-kill" and .spec.duration == null)
        or (.spec.action == "pod-kill" and .spec.gracePeriod != 0))
      | filename' "$manifest")"
  done
  assert_eq "every manifest is in openstack-ha, selects there alone and has a duration unless pod-kill" "" "$bad"
  printf 'metadata:\n  namespace: openstack-ha\nspec:\n  action: pod-failure\n  selector:\n    pods:\n      openstack: [x]\n' \
    >"$TMP_DIR/stray.yaml"
  assert_eq "a manifest selecting in openstack without a duration is caught" "$TMP_DIR/stray.yaml" \
    "$(yq 'select(([(.spec.selector.pods, .spec.target.selector.pods) | select(. != null) | keys | .[]] | unique | join(",")) != "openstack-ha"
      or (.spec.action != "pod-kill" and .spec.duration == null)) | filename' "$TMP_DIR/stray.yaml")"
}

# --- Test 4 ---------------------------------------------------------------
test_no_host_network_or_nodes() {
  echo "Test: no suite reaches a node or a hostNetwork pod, and no read hides its status"
  assert_eq "no allowHostNetworkTesting, nodes: or nodeSelectors:" "" \
    "$(grep -rnE 'allowHostNetworkTesting|(^|[[:space:]])nodes:|nodeSelectors:' "$LF_DIR" || true)"
  assert_eq "no cp_status or cp_reason read inside [" "" \
    "$(grep -rnE '\[\[? *"?\$\((cp_status|cp_reason) ' "$LF_DIR" "$LF_SH" || true)"
}

# --- Test 5 ---------------------------------------------------------------
test_fixtures() {
  echo "Test: the fixtures are the second plane of boundary 1 to 3"
  local cp="$FIXTURES/03-controlplane.yaml" deadline bound retries grace
  assert_eq "the ControlPlane carries none of the dropped keys" "" \
    "$(data_lines "$cp" | grep -E '^[[:space:]]*(profileRef|gateway|publicEndpoint|hypervisorOperator|backupBackend):' || true)"
  assert_eq "the OVNCentral sets no replicas" "" \
    "$(data_lines "$FIXTURES/01-ovncentral.yaml" | grep -E 'replicas:|northd:|storage' || true)"
  # The c5c3-operator admits openstack-ha for the dedicated Barbican store of
  # cp-ha, under the guardrail spec; a fixture tenant would make it skip.
  assert_eq "no fixture is an OpenBaoTenant" "" \
    "$(grep -lE '^kind: OpenBaoTenant$' "$FIXTURES"/*.yaml || true)"
  assert_eq "the tenant fixture is gone" "" "$(find "$FIXTURES" -name 00-openbao-tenant.yaml)"
  assert_file_contains_fixed "the seed Job creates the lab's image id" "$FIXTURES/04-seed-job.yaml" \
    'SEED_ID="4e0a0000-0000-4000-8000-000000001393"'
  assert_file_contains_fixed "the seed Job creates m1.nano" "$FIXTURES/04-seed-job.yaml" \
    'openstack flavor create --vcpus 1 --ram 64 --disk 0 --public m1.nano'
  assert_eq "the library's seed image and flavor are the seed Job's" \
    "4e0a0000-0000-4000-8000-000000001393 m1.nano" \
    "$(bash -c '. "$1" && echo "$LF_SEED_IMAGE $LF_SEED_FLAVOR"' _ "$LF_DIR/lib/failover.sh")"
  assert_file_contains_fixed "the fake compute carries the lab's compute id" "$FIXTURES/05-fake-compute.yaml" \
    'value: d13d13d1-0000-4000-8000-000000001393'
  assert_eq "the fake compute names no other plane" "" \
    "$(data_lines "$FIXTURES/05-fake-compute.yaml" | grep -E 'controlplane-keystone|namespace: openstack$' || true)"
  if ! have_yq; then
    echo "  SKIP: yq not installed (the field checks of the fixtures skipped)"
    SKIP=$((SKIP + 1))
    return 0
  fi
  assert_eq "the ControlPlane is openstack-ha/cp-ha on Standard" "ControlPlane openstack-ha/cp-ha Standard" \
    "$(yq '.kind + " " + .metadata.namespace + "/" + .metadata.name + " " + .spec.sizing.profile' "$cp")"
  assert_eq "the ControlPlane references cp-ha-ovn" "cp-ha-ovn" "$(yq '.spec.services.neutron.ovn.centralRef.name' "$cp")"
  assert_eq "the ControlPlane names the Horizon key Secret" "cp-ha-horizon-secret-key secret-key" \
    "$(yq '.spec.services.horizon.secretKeyRef.name + " " + .spec.services.horizon.secretKeyRef.key' "$cp")"
  assert_eq "the ControlPlane runs all eight services" "barbican cinder glance horizon keystone neutron nova placement" \
    "$(yq '.spec.services | keys | sort | join(" ")' "$cp")"
  assert_eq "the OVNCentral is openstack-ha/cp-ha-ovn on the lab's CA issuer" "openstack-ha/cp-ha-ovn openstack-ovn-ca-issuer" \
    "$(yq '.metadata.namespace + "/" + .metadata.name + " " + .spec.tls.issuerRef.name' "$FIXTURES/01-ovncentral.yaml")"
  assert_eq "the ExternalSecret reads the plane's key through the tenant store" \
    "bootstrap/openstack-ha/cp-ha-horizon/secret-key secret-key openbao-tenant-store SecretStore cp-ha-horizon-secret-key" \
    "$(yq '.spec.data[0].remoteRef.key + " " + .spec.data[0].remoteRef.property + " " + .spec.secretStoreRef.name + " " + .spec.secretStoreRef.kind + " " + .spec.target.name' \
      "$FIXTURES/02-horizon-secret-key-externalsecret.yaml")"
  assert_eq "the purge Job is openstack-ha/cp-ha-purge" "openstack-ha/cp-ha-purge" \
    "$(yq '.metadata.namespace + "/" + .metadata.name' "$FIXTURES/06-purge-job.yaml")"
  assert_eq "the purge Job runs on the seed Job's image, env and securityContext" \
    "$(yq -o json -I0 '.spec.template.spec.containers[0] | [.image, .env, .securityContext]' "$FIXTURES/04-seed-job.yaml")" \
    "$(yq -o json -I0 '.spec.template.spec.containers[0] | [.image, (.env | map(select(.name != "OS_VOLUME_API_VERSION"))), .securityContext]' \
      "$FIXTURES/06-purge-job.yaml")"
  deadline="$(yq '.spec.activeDeadlineSeconds // 0' "$FIXTURES/06-purge-job.yaml")"
  assert_gte "the purge Job has an activeDeadlineSeconds" "$deadline" 1
  # Each attempt gets 10 s over its bound for its pod start and its last poll
  # (sleep 5 and one list), and the Job controller backs off 10 s, 20 s, ...
  # between attempts.
  bound="$(yq '.spec.template.spec.containers[0].args[0]' "$FIXTURES/06-purge-job.yaml" |
    sed -nE 's/^ *if \[ "\$\{SECONDS\}" -ge ([0-9]+) \]; then$/\1/p')"
  assert_gte "the purge script bounds its wait on a deleting volume" "${bound:-0}" 1
  retries="$(yq '.spec.backoffLimit // 6' "$FIXTURES/06-purge-job.yaml")"
  assert_gte "every purge attempt and backoff end inside activeDeadlineSeconds" \
    "$((deadline - (retries + 1) * (${bound:-0} + 10) - 10 * ((1 << retries) - 1)))" 1
  # A pod the deadline deletes runs out its termination grace first.
  grace="$(yq '.spec.template.spec.terminationGracePeriodSeconds // 30' "$FIXTURES/06-purge-job.yaml")"
  assert_gte "the purge Job's pods are gone before down stops waiting for it" \
    "$(($(bash -c '. "$1" && echo "$LF_WAIT_PURGE"' _ "$LF_SH") - deadline - grace))" 1
  assert_eq "every namespaced fixture is in openstack-ha" "" \
    "$(yq 'select(.metadata.namespace != "openstack-ha") | filename + " " + .kind' "$FIXTURES"/*.yaml)"
}

# --- Test 6 ---------------------------------------------------------------
test_spdx_and_lint() {
  echo "Test: every new file carries the SPDX pair, and make shellcheck lints the library"
  local file missing=""
  while IFS= read -r file; do
    if ! grep -q '^# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company$' "$file" ||
      ! grep -q '^# SPDX-License-Identifier: Apache-2.0$' "$file"; then
      missing="${missing} ${file#"$PROJECT_ROOT"/}"
    fi
  done < <(find "$LF_DIR" -type f \( -name '*.yaml' -o -name '*.sh' \); echo "$LF_SH")
  assert_eq "every file carries the SPDX pair" "" "$missing"
  assert_contains "make shellcheck lints tests/lab-failover/lib" \
    "$(awk '/^shellcheck:/ { s = 1; next } s && /^[^\t]/ { exit } s' "$PROJECT_ROOT/Makefile")" \
    "shellcheck --severity=warning tests/lab-failover/lib/*.sh"
}

test_scenarios_are_the_directories
test_suites
test_manifests
test_no_host_network_or_nodes
test_fixtures
test_spdx_and_lint

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
