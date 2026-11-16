#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-resolve-changes.sh emits the e2e-bgp-fabric change signal and
# that .github/workflows/ci.yaml and the Makefile consume it.
#
# The signal crosses three places and a mismatch in any of them is silent:
# GitHub Actions resolves an unknown `needs.<job>.outputs.<name>` to the empty
# string rather than failing, so a renamed output or an unwired FILTER_ env var
# leaves the BGP fabric job permanently skipped.
#
# The resolve script is executed for real in all of its branches; the ci.yaml
# sides are asserted against the workflow file. Unlike the e2e-ovn-overlay
# job, this one loads no run-scoped image, so it must neither schedule
# build-e2e-images nor wait for it. The Makefile target runs against stubs of
# kubectl and chainsaw: it starts the suite only when every node is a kind
# node.
#
# Modelled on the sibling tests/unit/hack/ci_resolve_changes_ovn_overlay_test.sh.
#
# Usage: bash tests/unit/hack/ci_resolve_changes_bgp_fabric_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC2034 # read by tests/lib/ci_yaml.sh
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"
MAKEFILE="$PROJECT_ROOT/Makefile"

ALL_OPERATORS_FIXTURE="keystone ovn"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_resolve.sh
source "$PROJECT_ROOT/tests/lib/ci_resolve.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

# run_resolve <ref> <filter> [ENV=value ...]: the e2e-bgp-fabric line the
# resolve script emits for the given ref and suite filter.
run_resolve() {
  local ref="$1" filter="$2"
  shift 2
  resolve_output e2e-bgp-fabric "$ref" "$ALL_OPERATORS_FIXTURE" \
    FILTER_tests_bgp_fabric="$filter" "$@"
}

test_own_filter_is_honoured() {
  echo "Test: on a branch the job follows its own filter"

  assert_eq "a changed suite or overlay is signalled" \
    "e2e-bgp-fabric=true" "$(run_resolve refs/heads/main true)"
  assert_eq "an untouched suite is not signalled" \
    "e2e-bgp-fabric=false" "$(run_resolve refs/heads/main false)"
}

test_only_its_own_inputs_schedule_the_job() {
  echo "Test: only the suite, the overlay, ci:full and a tag schedule it"

  assert_eq "a shared Go change does not schedule the job" \
    "e2e-bgp-fabric=false" "$(run_resolve refs/heads/main false FILTER_go_common=true)"
  assert_eq "an ovn-operator change does not schedule the job" \
    "e2e-bgp-fabric=false" "$(run_resolve refs/heads/main false FILTER_ovn=true)"
  assert_eq "another suite does not schedule the job" \
    "e2e-bgp-fabric=false" "$(run_resolve refs/heads/main false FILTER_tests_ovn_overlay=true)"
  assert_eq "a change to the shared e2e substrate does not schedule the job" \
    "e2e-bgp-fabric=false" "$(run_resolve refs/heads/main false FILTER_e2e_shared=true)"
  assert_eq "ci:full schedules the job" \
    "e2e-bgp-fabric=true" "$(run_resolve refs/heads/main false PR_LABELS='["ci:full"]')"
  assert_eq "a v* tag schedules the job" \
    "e2e-bgp-fabric=true" "$(run_resolve refs/tags/v1.2.3 false)"
}

test_the_night_and_the_noop_resolve_it_off() {
  echo "Test: a scheduled run, the no-op path and an unwired filter emit false"

  assert_eq "a scheduled run with the filter set does not run the job" \
    "e2e-bgp-fabric=false" "$(run_resolve refs/heads/main true EVENT_NAME=schedule)"
  assert_eq "the labeled no-op path emits an explicit false" \
    "e2e-bgp-fabric=false" \
    "$(run_resolve refs/heads/main true EVENT_ACTION=labeled EVENT_LABEL=bug)"
  # The script runs under `set -u`; the `:-false` default is what keeps an
  # unwired filter from aborting the changes job.
  assert_eq "the output is emitted even with no FILTER_ env var" \
    "e2e-bgp-fabric=false" \
    "$(resolve_output e2e-bgp-fabric refs/heads/main "$ALL_OPERATORS_FIXTURE")"
}

test_the_signal_builds_no_image() {
  echo "Test: the job alone does not schedule the image build"

  local outputs
  outputs="$(resolve_outputs refs/heads/main "$ALL_OPERATORS_FIXTURE" FILTER_tests_bgp_fabric=true)"
  assert_contains "the suite change schedules the job" "$outputs" "e2e-bgp-fabric=true"
  assert_contains "the suite change builds no E2E image" "$outputs" "build-e2e-images=false"
}

test_ci_yaml_wires_the_signal() {
  echo "Test: ci.yaml declares the filter, passes it in and exports the output"

  assert_filter_is_wired tests_bgp_fabric e2e-bgp-fabric

  local block
  block="$(filter_block tests_bgp_fabric)"
  assert_contains "the filter lists the suite" "$block" "'tests/e2e-bgp-fabric/**'"
  assert_contains "the filter lists the frr-k8s overlay" "$block" "'deploy/overlays/kind/frr-k8s/**'"
  # The deploy stack and the hack scripts reach e2e-infra through the canary;
  # listing them here would run a self-hosted kind job for every helper edit.
  assert_not_contains "the filter does not carry the deploy stack" "$block" "'deploy/**'"
  assert_not_contains "the filter does not carry the hack scripts" "$block" "'hack/**'"
  assert_not_contains "the filter does not carry the operators" "$block" "operators/"
}

test_the_job_consumes_the_signal() {
  echo "Test: the job gates on the output, stays non-blocking and runs the Makefile target"

  local job
  job="$(job_block e2e-bgp-fabric)"
  assert_not_empty "the e2e-bgp-fabric job exists" "$job"
  assert_contains "the job needs only changes" "$job" "needs: [changes]"
  assert_contains "the job reads the output the resolve script wrote" "$job" \
    "needs.changes.outputs.e2e-bgp-fabric == 'true'"
  assert_contains "the job keeps fork pull requests off the self-hosted runner" "$job" \
    "github.event.pull_request.head.repo.fork != true"
  assert_contains "the job is non-blocking under the kernel-module rule" "$job" \
    "continue-on-error: true"
  assert_contains "the job installs the frr-k8s overlay" "$job" 'WITH_FRR_K8S: "true"'
  assert_contains "the job runs the suite through the Makefile" "$job" "make e2e-bgp-fabric"
  assert_contains "the job uploads its JUnit report" "$job" "name: e2e-bgp-fabric-junit-report"
  assert_not_contains "the job does not wait for the image build" "$job" "build-e2e-images"
  assert_not_contains "the job loads no run-scoped image" "$job" "load-e2e-images"

  local cleanup_needs
  cleanup_needs="$(job_block cleanup-e2e-tags | grep -m1 'needs:' || true)"
  assert_not_empty "cleanup-e2e-tags declares its needs" "$cleanup_needs"
  assert_not_contains "cleanup-e2e-tags does not wait for the job" "$cleanup_needs" "e2e-bgp-fabric"

  assert_file_contains "the Makefile declares the target" "$MAKEFILE" "^e2e-bgp-fabric:$"
  assert_file_contains "the target is phony" "$MAKEFILE" "^\\.PHONY: e2e-bgp-fabric$"
  assert_file_contains_fixed "the first preflight names an unreachable cluster" "$MAKEFILE" \
    "kubectl is not configured or no cluster is reachable"
  assert_file_contains_fixed "the second preflight names the missing overlay" "$MAKEFILE" \
    "frr-k8s is not deployed; run WITH_FRR_K8S=true make deploy-infra first"
}

test_the_target_runs_only_against_kind() {
  echo "Test: make e2e-bgp-fabric starts the suite only on a cluster of kind nodes"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (9 checks skipped)"
    SKIP=$((SKIP + 9))
    return
  fi

  local tmp
  tmp="$(mktemp -d)"
  mkdir -p "$tmp/bin"
  # The kubectl stub reaches a cluster with frr-k8s deployed and answers
  # `get nodes` with $STUB_NODES, or fails when it is empty. The chainsaw
  # stub records that the suite started.
  cat >"$tmp/bin/kubectl" <<'EOF'
#!/bin/sh
case "$1 $2" in
  "get nodes") [ -n "$STUB_NODES" ] || exit 1; printf '%s\n' "$STUB_NODES" ;;
esac
exit 0
EOF
  cat >"$tmp/bin/chainsaw" <<'EOF'
#!/bin/sh
touch "$STUB_RAN"
EOF
  chmod +x "$tmp/bin/kubectl" "$tmp/bin/chainsaw"

  local kind='{"spec":{"providerID":"kind://docker/cobaltcore/cobaltcore-control-plane"}}'
  local desc nodes want_rc want_ran rc ran out
  while IFS='|' read -r desc nodes want_rc want_ran; do
    rm -f "$tmp/ran"
    out="$(env PATH="$tmp/bin:$PATH" STUB_NODES="$nodes" STUB_RAN="$tmp/ran" \
      make -s -C "$PROJECT_ROOT" e2e-bgp-fabric 2>&1)"
    rc=$?
    ran="$([[ -e "$tmp/ran" ]] && echo yes || echo no)"
    assert_eq "$desc: make exits $want_rc" "$want_rc" "$([[ "$rc" -eq 0 ]] && echo 0 || echo non-zero)"
    assert_eq "$desc: the suite starts: $want_ran" "$want_ran" "$ran"
  done <<EOF
every node is a kind node|{"items":[$kind]}|0|yes
one node is not a kind node|{"items":[$kind,{"spec":{"providerID":"metal://fra-equ01/a1b2"}}]}|non-zero|no
a node has no providerID|{"items":[$kind,{"spec":{}}]}|non-zero|no
the nodes cannot be read||non-zero|no
EOF
  assert_contains "the refusal names the kind requirement" "$out" \
    "it runs only against a kind cluster"
  rm -rf "$tmp"
}

test_own_filter_is_honoured
test_only_its_own_inputs_schedule_the_job
test_the_night_and_the_noop_resolve_it_off
test_the_signal_builds_no_image
test_ci_yaml_wires_the_signal
test_the_job_consumes_the_signal
test_the_target_runs_only_against_kind

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
