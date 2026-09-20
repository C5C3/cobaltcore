#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the nova operator reaches the e2e-operators matrix, the two Go test
# matrices and the helm filter in .github/workflows/ci.yaml.
#
# Every signal here fails silently when it is missing. ALL_OPERATORS is the sole
# source of the e2e-operators matrix, so an operator absent from it never
# produces a leg and its Chainsaw suites under tests/e2e/nova/ are lint-checked
# and never applied to a cluster. SERVICE_OPERATORS is what tells the resolver
# that nova ships an OpenStack service image per release, so an image rebuild
# reaches the nova leg without the operator's Go gates. The helm filter is the
# sole gate on helm-validate, so a pull request touching only
# operators/nova/helm/ renders, lints and unit-tests nothing. The nova chart
# takes namespace-scoped RBAC instead of refusing it the way the ovn and neutron
# charts do, so every helm-validate scenario has to render.
#
# The assertions this file does not carry yet are the ones whose wiring is not
# in ci.yaml yet: the tempest_nova paths filter and the tempest legs (#1040),
# and the ControlPlane leg (#1019). They belong here once that wiring exists.
# The e2e suites and the chaos leg are wired, and asserted below.
#
# Nova stays out of the two-cluster placed-services suite
# (tests/e2e-multicluster/placed-services/), per the author on 2026-09-17. The
# suite proves where the children land, and every grant of the
# target-cluster-access chart a Nova would exercise (Deployments, Jobs,
# CronJobs, HTTPRoutes, NetworkPolicies, HorizontalPodAutoscalers) is exercised
# by the placed Keystone, Barbican, OVNCentral and Neutron already. The placed
# Neutron runs against a deliberately unreachable broker
# (rabbitmq.openstack.invalid in its messaging Secret), and a Nova cannot: its
# scheduler and conductor report ready only off a live broker connection, so
# membership would mean a real broker on the target cluster, a seventh operator
# on the management cluster and an eighth image inside a 90-minute job. Nova's
# remote teardown stays proven by the operator's envtest
# (operators/nova/internal/controller/) and, against a real second cluster, by
# #1013 when compute clusters arrive. Nothing here asserts on that suite, and
# #1019 does not reopen the question.
#
# Usage: bash tests/unit/ci/nova_e2e_matrix_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_resolve.sh
source "$PROJECT_ROOT/tests/lib/ci_resolve.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

# The real list from the ci.yaml resolve step env block. A shorter one would
# make the two matrix scenarios below assert nothing.
ALL_OPS="keystone c5c3 horizon glance placement barbican ovn neutron cinder nova"

CHART="$PROJECT_ROOT/operators/nova/helm/nova-operator"

# The PATH a stubbed run prepends its stub directory to, captured before any of
# those runs rewrites it.
BASE_PATH="$PATH"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Echo the flags of every `helm template` scenario the helm-validate job runs,
# one scenario per line; the default-values scenario comes out as an empty line.
# Reading them out of the job keeps this test rendering what CI renders: a
# scenario added there alone would leave the chart unproven under it.
helm_template_scenarios() {
  job_step helm-validate "Helm template" |
    grep -oE 'helm template test "\$chart".*' |
    sed -e 's/^helm template test "\$chart" *//' -e 's/ 2>&1).*$//'
}

# Echo how many releases the operator ships a service image for. The resolve
# step reads the same script, so a release added under releases/ moves the
# expected ref counts below with it.
release_count() {
  OPERATOR="$1" "$PROJECT_ROOT/hack/ci-service-image-releases.sh" |
    grep -c . || true
}

# make_discover_stubs <dir>
# Writes the two stubs tests/e2e/nova/discover-hosts.sh resolves off PATH.
#
# The kubectl stub answers the two nova-manage calls the helper makes and reads
# its behaviour from the environment: cell_v2 discover_hosts counts the round in
# $STUB_ROUNDS and exits $STUB_RC from round $STUB_RC_ROUND on (0 before it),
# cell_v2 list_hosts prints a one-row prettytable naming $STUB_HOST. A run that
# is meant to miss therefore ends on a failed discovery after one lookup,
# instead of polling to the helper's 150-second deadline.
#
# The sleep stub returns at once, so the 5-second interval between the rounds
# costs nothing here.
make_discover_stubs() {
  local dir="$1"

  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
case "$*" in
  *discover_hosts*)
    round=$(($(cat "$STUB_ROUNDS") + 1))
    printf '%s' "$round" >"$STUB_ROUNDS"
    if [ "$round" -ge "$STUB_RC_ROUND" ]; then
      exit "$STUB_RC"
    fi
    ;;
  *list_hosts*)
    cat <<TABLE
+-----------+--------------------------------------+----------+
| Cell Name |              Cell UUID               | Hostname |
+-----------+--------------------------------------+----------+
|   cell1   | 11111111-2222-3333-4444-555555555555 | $STUB_HOST |
+-----------+--------------------------------------+----------+
TABLE
    ;;
esac
exit 0
STUB
  chmod +x "$dir/kubectl"

  cat >"$dir/sleep" <<'STUB'
#!/bin/bash
exit 0
STUB
  chmod +x "$dir/sleep"
}

# run_discover_hosts <dir> <host> <rc> <rc_round> [args...]
# Runs tests/e2e/nova/discover-hosts.sh with the stubs in <dir> first on PATH.
# <host> is the hostname the stubbed list_hosts prints, <rc> the exit code the
# stubbed discovery reports from round <rc_round> on. Echoes the helper's
# stdout, writes its stderr to <dir>/stderr and returns its exit code.
run_discover_hosts() {
  local dir="$1" host="$2" rc="$3" rc_round="$4"
  shift 4
  printf '0' >"$dir/rounds"
  (
    PATH="$dir:$PATH"
    STUB_HOST="$host"
    STUB_RC="$rc"
    STUB_RC_ROUND="$rc_round"
    STUB_ROUNDS="$dir/rounds"
    export PATH STUB_HOST STUB_RC STUB_RC_ROUND STUB_ROUNDS
    bash "$PROJECT_ROOT/tests/e2e/nova/discover-hosts.sh" "$@"
  ) 2>"$dir/stderr"
}

# make_tempest_stubs <dir>
# Writes the four stubs hack/ci-run-tempest.sh resolves off PATH, so the runner
# can be exercised without a cluster, a network or a container runtime.
#
# kubectl answers the admin-secret lookup with a base64 password and records
# every port-forward argv in $STUB_PF_LOG. A real forward outlives the suite it
# serves, so the stub blocks on /bin/sleep once it has logged, which is what the
# runner's post-run liveness check reads; a port listed in $STUB_PF_DEAD_PORTS
# returns instead, which is the forward that dropped mid-run. curl records every
# argv in $STUB_CURL_LOG and answers a URL only when $STUB_PF_LOG already holds
# a forward for its port: a port nobody forwarded refuses the connection, which
# curl -sf reports as exit 7. That coupling also takes the race out of the
# assertions, because a poll that succeeded proves the backgrounded forward
# reached the log. A port listed in $STUB_CURL_DEAD_PORTS is forwarded and
# answers an HTTP error (exit 22) instead, which is the API that never becomes
# ready. docker records the argv of the container run in $STUB_DOCKER_LOG and
# starts nothing. sleep returns at once, so the ten one-second readiness polls
# of a target that stays dead cost nothing.
make_tempest_stubs() {
  local dir="$1"

  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
case "$1" in
  port-forward)
    printf '%s\n' "$*" >>"$STUB_PF_LOG"
    mapping="${*: -1}"
    port="${mapping%%:*}"
    case " ${STUB_PF_DEAD_PORTS} " in
      *" ${port} "*) exit 0 ;;
    esac
    # Absolute path: the stub `sleep` next to this file returns at once.
    exec /bin/sleep 600
    ;;
  get)
    printf 'c2VjcmV0'
    ;;
esac
exit 0
STUB
  chmod +x "$dir/kubectl"

  cat >"$dir/curl" <<'STUB'
#!/bin/bash
printf '%s\n' "$*" >>"$STUB_CURL_LOG"
url="${*: -1}"
port="${url#*localhost:}"
port="${port%%/*}"
case " ${STUB_CURL_DEAD_PORTS} " in
  *" ${port} "*) exit 22 ;;
esac
grep -qF " ${port}:${port}" "$STUB_PF_LOG" || exit 7
exit 0
STUB
  chmod +x "$dir/curl"

  cat >"$dir/docker" <<'STUB'
#!/bin/bash
printf '%s\n' "$*" >>"$STUB_DOCKER_LOG"
exit 0
STUB
  chmod +x "$dir/docker"

  cat >"$dir/sleep" <<'STUB'
#!/bin/bash
exit 0
STUB
  chmod +x "$dir/sleep"
}

# run_ci_tempest <dir> <nova-name> <console-name> <dead-ports> [placement-name] [dropped-ports]
# Runs hack/ci-run-tempest.sh with the stubs in <dir> first on PATH, as the leg
# named by <nova-name>, <console-name> and <placement-name> (any may be empty,
# which is a leg that names no Nova and no Placement). <dead-ports> is the
# space-separated list of ports the stubbed curl reports an HTTP error for,
# <dropped-ports> the list whose forward exits as soon as it has logged. Echoes
# the runner's combined output and returns its exit code. The output directory
# and the workspace root stay inside <dir>, so a run writes nothing into the
# repository tree. Each call starts from empty logs.
run_ci_tempest() {
  local dir="$1" nova="$2" console="$3" dead="$4" placement="${5:-}" dropped="${6:-}"
  : >"$dir/pf.log"
  : >"$dir/curl.log"
  : >"$dir/docker.log"
  (
    PATH="$dir:$BASE_PATH"
    OUTPUT_DIR="$dir/output"
    GITHUB_WORKSPACE="$dir"
    NOVA_K8S_NAME="$nova"
    NOVA_CONSOLE_K8S_NAME="$console"
    PLACEMENT_K8S_NAME="$placement"
    STUB_PF_LOG="$dir/pf.log"
    STUB_CURL_LOG="$dir/curl.log"
    STUB_DOCKER_LOG="$dir/docker.log"
    STUB_CURL_DEAD_PORTS="$dead"
    STUB_PF_DEAD_PORTS="$dropped"
    export PATH OUTPUT_DIR GITHUB_WORKSPACE NOVA_K8S_NAME NOVA_CONSOLE_K8S_NAME
    export PLACEMENT_K8S_NAME
    export STUB_PF_LOG STUB_CURL_LOG STUB_DOCKER_LOG STUB_CURL_DEAD_PORTS
    export STUB_PF_DEAD_PORTS
    bash "$PROJECT_ROOT/hack/ci-run-tempest.sh"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_all_operators_lists_nova() {
  echo "Test: ci.yaml ALL_OPERATORS includes nova"

  local line
  line=$(grep "ALL_OPERATORS:" "$CI_YAML" | head -1)
  assert_contains "ALL_OPERATORS lists nova" "$line" "nova"
  assert_contains "ALL_OPERATORS still lists keystone" "$line" "keystone"
}

test_service_operators_lists_nova() {
  echo "Test: ci.yaml SERVICE_OPERATORS includes nova"

  # nova ships an OpenStack service image for every release that carries a nova
  # key in source-refs.yaml. Missing from this list, the resolver treats it like
  # the orchestration operators: the image is never rebuilt for a pull request
  # that changes it, and the e2e leg loads whatever main last published.
  local line
  line=$(grep "SERVICE_OPERATORS:" "$CI_YAML" | head -1)
  assert_contains "SERVICE_OPERATORS lists nova" "$line" "nova"
  assert_not_empty "nova ships a service image per release" \
    "$(OPERATOR=nova "$PROJECT_ROOT/hack/ci-service-image-releases.sh")"
}

test_nova_filter_is_wired() {
  echo "Test: the nova paths filter reaches the resolve step"

  assert_file_contains "the paths filter exists" "$CI_YAML" "^ *nova:$"
  assert_file_contains "the filter is passed to the resolve step" "$CI_YAML" \
    "FILTER_nova: \${{ steps.filter.outputs.nova }}"

  local block
  block=$(filter_block nova)
  assert_contains "the operator source path is covered" "$block" "operators/nova/**"
}

test_nova_change_produces_an_e2e_leg() {
  echo "Test: an operators/nova change puts nova in the e2e-operators matrix"

  local matrix
  matrix=$(resolve_output e2e-operators refs/heads/main "$ALL_OPS" FILTER_nova=true)

  assert_contains "the matrix carries the nova leg" "$matrix" '"nova"'
  assert_contains "the matrix keeps the operator axis" "$matrix" '"operator"'
  assert_not_contains "the sentinel is gone once an operator changed" \
    "$matrix" "__none__"
}

test_helm_filter_covers_the_nova_chart() {
  echo "Test: a nova chart change re-runs helm-validate"

  # The helm filter is the operators/*/helm/** glob; the chart directory under
  # it is what makes the glob cover this operator.
  assert_contains "the helm filter covers every operators/<op>/helm/ tree" \
    "$(filter_block helm)" "operators/*/helm/**"
  assert_eq "the nova chart lives under that glob" "yes" \
    "$([ -d "$CHART" ] && echo yes || echo no)"
}

test_helm_validate_renders_the_nova_chart() {
  echo "Test: the nova chart renders under every helm-validate scenario"

  if ! command -v helm >/dev/null 2>&1; then
    echo "  SKIP: helm not installed"
    SKIP=$((SKIP + 1))
    return
  fi

  # helm-validate vendors the operator-library subchart with `make helm-deps`
  # before it renders anything. charts/ is gitignored, so do the same here
  # instead of skipping on a working copy that has not run the target yet.
  if ! compgen -G "$CHART/charts/operator-library-*.tgz" >/dev/null; then
    helm dependency build --skip-refresh "$CHART" >/dev/null 2>&1
  fi

  local scenarios
  scenarios=$(helm_template_scenarios)
  assert_not_empty "the job's template scenarios are readable" "$scenarios"
  assert_contains "the namespace-scoped scenario is one of them" "$scenarios" \
    "--set rbac.namespaceScoped=true"

  local flags out failed=""
  while IFS= read -r flags; do
    # shellcheck disable=SC2086 # deliberate: split the scenario's flags
    if ! out=$(helm template test "$CHART" $flags 2>&1); then
      failed="${failed}${flags:-default values}: ${out}"$'\n'
    fi
  done <<<"$scenarios"

  # ovn-operator and neutron-operator refuse Scenario 5 by overriding the
  # operator-library.chart.namespaceScopedUnsupported hook, and the job reads
  # that documented refusal as a pass. The nova chart carries no such override,
  # so a failed render there is a real one.
  assert_eq "every scenario renders" "" "$failed"

  # The refusal is not the only way to lose the mode: a chart can also render
  # nothing at all under it. Assert the namespaced Role is what comes out.
  out=$(helm template test "$CHART" --set rbac.namespaceScoped=true \
    --set webhook.enabled=false 2>&1)
  assert_contains "namespace-scoped RBAC produces a Role" "$out" "kind: Role"
}

test_go_matrices_list_nova() {
  echo "Test: the unit and integration test matrices include nova"

  # Both matrices are resolved per pull request rather than hand-written, so an
  # operator reaches them in two steps: the jobs read the resolver's list, and
  # the resolver puts the operator in it when its own code changes. An operator
  # missing from either half compiles and ships with its `go test` leg never
  # run, and the pipeline stays green because no job failed.
  local matrix_count
  matrix_count=$(grep -c \
    'matrix: ${{ fromJson(needs.changes.outputs.test-targets) }}' "$CI_YAML") || true

  assert_eq "both the test and the test-integration matrix read test-targets" \
    "2" "$matrix_count"

  assert_contains "a nova change puts nova in the test matrix" \
    "$(resolve_output test-targets refs/heads/main "$ALL_OPS" FILTER_nova=true)" \
    '"nova"'
}

test_cleanup_matrices_cover_the_nova_images() {
  echo "Test: the derived cleanup package lists cover the nova images"

  # cleanup-images.yaml and ci.yaml's cleanup-e2e-tags both build their package
  # matrix from this generator, so coverage is a property of its output rather
  # than of a list someone has to remember to extend. An uncovered package
  # leaks its run-scoped GHCR tags on every pull request.
  local matrix all_packages e2e_packages
  matrix=$(cd "$PROJECT_ROOT" && bash hack/ci-generate-cleanup-matrix.sh)
  all_packages=$(echo "$matrix" | sed -n 's/^cleanup-packages=//p')
  e2e_packages=$(echo "$matrix" | sed -n 's/^cleanup-e2e-packages=//p')

  assert_contains "the nightly sweep covers nova-operator" \
    "$all_packages" '"nova-operator"'
  assert_contains "the per-run sweep covers nova-operator" \
    "$e2e_packages" '"nova-operator"'
  assert_contains "the nightly sweep covers nova" \
    "$all_packages" '"nova"'
  assert_contains "the per-run sweep covers nova" \
    "$e2e_packages" '"nova"'
}

test_a_keystone_only_change_produces_no_nova_leg() {
  echo "Test: a keystone-only change keeps nova out of the e2e-operators matrix"

  # The positive case above proves the filter reaches the matrix; this one
  # proves it still gates. A filter wired to a constant would satisfy the
  # positive assertion and put every operator on every pull request.
  local matrix
  matrix=$(resolve_output e2e-operators refs/heads/main "$ALL_OPS" \
    FILTER_keystone=true)

  assert_contains "the matrix carries the keystone leg" "$matrix" '"keystone"'
  assert_not_contains "and no nova leg" "$matrix" '"nova"'
}

test_nova_image_filter_is_wired() {
  echo "Test: an images/nova change reaches changed-services"

  # changed-services is the list build-e2e-images rebuilds from source. An
  # image edit missing from it leaves the nova leg loading whatever main last
  # published, so the suites pass against the old image and the change under
  # review is never run. patches/nova/ belongs in the same filter:
  # hack/ci-build-service-image.sh applies patches/<op>/<release>/*.patch into
  # the source it builds, so a patch edited on its own changes that image.
  assert_filter_is_wired image_nova changed-services

  local block
  block=$(filter_block image_nova)
  assert_contains "the image build context is covered" "$block" "images/nova/**"
  assert_contains "the source patches are covered" "$block" "patches/nova/**"

  assert_eq "an image change rebuilds nova and nothing else" \
    'changed-services=["nova"]' \
    "$(resolve_output changed-services refs/heads/main "$ALL_OPS" \
      FILTER_image_nova=true)"

  # The rebuild is what schedules the leg here: the operator's own Go gates stay
  # shut, so the image is the only thing that puts nova on the runner.
  local outputs
  outputs=$(resolve_outputs refs/heads/main "$ALL_OPS" FILTER_image_nova=true)
  assert_contains "the rebuilt image schedules the nova leg" "$outputs" \
    'e2e-operators={"operator":["nova"]}'
  assert_contains "no Go job is asked for" "$outputs" "go=false"
  assert_contains "and no operator counts as changed" "$outputs" \
    "changed-operators=[]"

  # And it still gates: a run that matched no filter rebuilds nothing.
  assert_eq "an untouched image is not rebuilt" 'changed-services=[]' \
    "$(resolve_output changed-services refs/heads/main "$ALL_OPS")"
}

test_nova_e2e_filter_is_wired() {
  echo "Test: an edit to the nova suites alone runs the nova leg"

  # The suites sit in two directories and the e2e-operator run step probes for
  # the -operator one. tests/e2e/nova-operator/ arrives with #1039's
  # scrape-target suite; the filter names it ahead of that, so the first suite
  # landing there schedules the leg instead of none.
  assert_filter_is_wired tests_e2e_nova e2e-operators

  local block
  block=$(filter_block tests_e2e_nova)
  assert_contains "the per-CR suites are covered" "$block" "tests/e2e/nova/**"
  assert_contains "the operator-level suites are covered" "$block" \
    "tests/e2e/nova-operator/**"

  local outputs
  outputs=$(resolve_outputs refs/heads/main "$ALL_OPS" \
    FILTER_tests_e2e_nova=true)
  assert_contains "a suite edit runs the nova leg" "$outputs" \
    'e2e-operators={"operator":["nova"]}'
  assert_contains "and spends no runner on the Go matrices" "$outputs" \
    'test-targets={"target":["__none__"]}'

  # That leg and no other, in both directions: a sibling's suite edit schedules
  # its own leg alone and leaves nova out.
  assert_eq "a glance suite edit runs the glance leg alone" \
    'e2e-operators={"operator":["glance"]}' \
    "$(resolve_output e2e-operators refs/heads/main "$ALL_OPS" \
      FILTER_tests_e2e_glance=true)"
}

test_nova_leg_opts_into_the_broker() {
  echo "Test: the nova e2e leg asks for the broker and nothing else"

  # Every Nova process dials the bus, and the scheduler and the conductor
  # report ready off their broker connection. deploy-infra.sh installs the
  # broker only when it is asked to, and setup-e2e-infra reads the flag from
  # this step's env, so a Nova on a leg without it waits out its readiness on
  # an unreachable transport. The NFS export and the host kernel modules stay
  # off: no Nova suite mounts a volume or places a chassis.
  local setup
  setup=$(job_step e2e-operator "Setup E2E infrastructure")

  assert_not_empty "the setup step is readable" "$setup"
  assert_contains "the step uses the shared composite action" "$setup" \
    "uses: ./.github/actions/setup-e2e-infra"
  assert_contains "the nova leg opts into the shared broker" "$setup" \
    "WITH_MESSAGING: \${{ (matrix.operator == 'cinder' || matrix.operator == 'nova') && 'true' || '' }}"
  assert_contains "the NFS export stays cinder-only" "$setup" \
    "WITH_NFS: \${{ matrix.operator == 'cinder' && 'true' || '' }}"

  # The kernel modules are loaded on the runner host itself, so a nova arm on
  # that line would touch the host for suites that place no chassis.
  local modules
  modules=$(grep WITH_OVN_KERNEL_MODULES <<<"$setup")
  assert_not_empty "the kernel-module flag is readable" "$modules"
  assert_not_contains "the chassis modules stay off the nova leg" "$modules" \
    "nova"
}

test_nova_leg_deploys_the_sibling_operators() {
  echo "Test: the nova e2e leg deploys and loads its five sibling operators"

  # A Nova has no identity-free posture: spec.keystoneEndpoint and
  # spec.serviceUser are required on the CRD, so the leg needs a Keystone. The
  # functional suites boot a server, which takes a Placement allocation, a
  # Glance image and a Neutron port, and that Neutron reaches Ready only behind
  # a live OVNCentral. A sibling missing from the job leaves its CRs
  # unreconciled and every suite that reads one waiting out its own timeout.
  local deploy op
  for op in keystone placement glance neutron; do
    deploy=$(job_step e2e-operator "Deploy ${op}-operator")
    assert_not_empty "the ${op}-operator deploy step exists" "$deploy"
    # The whole condition, not a substring of it: an extra `|| always()` would
    # install this operator on all ten legs.
    assert_eq "the ${op} deploy runs on the nova leg alone" \
      "if: matrix.operator == 'nova'" \
      "$(grep -E '^ *if:' <<<"$deploy" | sed 's/^ *//')"
    assert_contains "it goes through the shared deploy script" "$deploy" \
      "run: hack/ci-deploy-operator.sh"
    assert_contains "it deploys the ${op} operator" "$deploy" "OPERATOR: ${op}"
    assert_contains "it uses the run-tagged ${op}-operator image" "$deploy" \
      "IMAGE_PREFIX }}/${op}-operator"
    assert_contains "it lands in the ${op} Namespace" "$deploy" \
      "NAMESPACE: ${op}-system"
  done

  # The keystone install here is a plain one: the CIDR allowlist belongs to the
  # keystone leg's oidc-federation suite, and ci-deploy-operator.sh would
  # render it into this chart too.
  assert_not_contains "the keystone sibling takes no federation override" \
    "$(job_step e2e-operator "Deploy keystone-operator")" \
    "FEDERATION_METADATA_ALLOW_CIDRS"

  # The neutron leg's ovn step is widened rather than copied: job_step returns
  # the first step of a name, so a second "Deploy ovn-operator" would be
  # invisible to every assertion in this directory.
  local ovn
  ovn=$(job_step e2e-operator "Deploy ovn-operator")
  assert_eq "the ovn deploy runs on the neutron and nova legs alone" \
    "if: matrix.operator == 'neutron' || matrix.operator == 'nova'" \
    "$(grep -E '^ *if:' <<<"$ovn" | sed 's/^ *//')"

  # Order is the load-bearing part: the nova-operator's own Nova never resolves
  # a Keystone endpoint, a Placement or a Neutron that nothing reconciles yet.
  local job keystone_at placement_at glance_at ovn_at neutron_at nova_at
  job=$(job_block e2e-operator)
  keystone_at=$(printf '%s\n' "$job" |
    grep -nF "name: Deploy keystone-operator" | head -1 | cut -d: -f1)
  placement_at=$(printf '%s\n' "$job" |
    grep -nF "name: Deploy placement-operator" | head -1 | cut -d: -f1)
  glance_at=$(printf '%s\n' "$job" |
    grep -nF "name: Deploy glance-operator" | head -1 | cut -d: -f1)
  ovn_at=$(printf '%s\n' "$job" |
    grep -nF "name: Deploy ovn-operator" | head -1 | cut -d: -f1)
  neutron_at=$(printf '%s\n' "$job" |
    grep -nF "name: Deploy neutron-operator" | head -1 | cut -d: -f1)
  nova_at=$(printf '%s\n' "$job" |
    grep -nF "name: Deploy operator" | head -1 | cut -d: -f1)
  assert_not_empty "the job deploys keystone-operator" "$keystone_at"
  assert_not_empty "the job deploys placement-operator" "$placement_at"
  assert_not_empty "the job deploys glance-operator" "$glance_at"
  assert_not_empty "the job deploys ovn-operator" "$ovn_at"
  assert_not_empty "the job deploys neutron-operator" "$neutron_at"
  assert_not_empty "the job deploys the matrix operator itself" "$nova_at"
  assert_gte "placement-operator comes after keystone-operator" \
    "$placement_at" "$keystone_at"
  assert_gte "glance-operator after placement-operator" \
    "$glance_at" "$placement_at"
  assert_gte "ovn-operator after glance-operator" "$ovn_at" "$glance_at"
  assert_gte "neutron-operator after ovn-operator" "$neutron_at" "$ovn_at"
  assert_gte "and the matrix operator's own deploy last" \
    "$nova_at" "$neutron_at"

  # kind pulls nothing the run did not load, so the load step reads the list the
  # resolve step publishes rather than naming the images a second time: the
  # five operator images and every service image each sibling publishes (ovn
  # publishes none). Pinning 2025.2 alone would leave a 2026.1 Nova talking to
  # 2025.2 siblings.
  local resolve load
  resolve=$(job_step e2e-operator "Resolve E2E images")
  load=$(job_step e2e-operator "Load images into kind")
  assert_contains "the resolve step branches on the nova leg" "$resolve" \
    'if [ "${OPERATOR}" = "nova" ]'
  assert_contains "it resolves the five sibling operator images" "$resolve" \
    'for sibling in keystone placement glance ovn neutron; do'
  assert_contains "at the run-scoped dev tag" "$resolve" \
    '${IMAGE_PREFIX}/${sibling}-operator:dev'
  assert_contains "with their release list read from source-refs.yaml" \
    "$resolve" 'OPERATOR="${sibling}" hack/ci-service-image-releases.sh'
  assert_contains "one service image per release" "$resolve" \
    '${IMAGE_PREFIX}/${sibling}:${release}'
  assert_contains "plus the OVN daemon image at the resolved pin" "$resolve" \
    '${IMAGE_PREFIX}/ovn:${OVN_VERSION}'
  assert_contains "the load step branches on the nova leg too" "$load" \
    'if [ "${OPERATOR}" = "nova" ]'
  assert_contains "it reads the resolved list" "$load" \
    'REFS: ${{ steps.e2e-images.outputs.refs }}'
  assert_contains "and loads every ref in it onto the kind node in one call" \
    "$load" 'kind load docker-image ${REFS} --name "${KIND_CLUSTER}"'

  # Reading the branch proves the shape, not the count. Run the step's own
  # script to see which refs the leg actually pulls and loads: a ref the
  # resolver drops is an ImagePullBackOff halfway through the suites.
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed"
    SKIP=$((SKIP + 1))
    return
  fi

  local script output refs
  script=$(mktemp)
  output=$(mktemp)
  yq -r '.jobs.e2e-operator.steps[]
    | select(.name == "Resolve E2E images") | .run' "$CI_YAML" >"$script"

  # The script shells out to hack/ by relative path.
  (cd "$PROJECT_ROOT" &&
    OPERATOR=nova IMAGE_PREFIX=ghcr.io/c5c3 GITHUB_OUTPUT="$output" \
      bash "$script")
  refs=$(awk '/^refs<<EOF$/ { in_b = 1; next }
    in_b && /^EOF$/ { exit }
    in_b { print }' "$output")

  # nova-operator:dev and one nova image per release, then each sibling's
  # operator image and its service images, then ovn:<pin> and the tempest
  # image.
  local expected sibling
  expected=$((1 + $(release_count nova)))
  for sibling in keystone placement glance ovn neutron; do
    expected=$((expected + 1 + $(release_count "$sibling")))
  done
  expected=$((expected + 2))
  assert_eq "the nova leg resolves its own images plus every sibling's" \
    "$expected" "$(printf '%s\n' "$refs" | wc -l | tr -d ' ')"
  assert_contains "its own operator image" "$refs" \
    "ghcr.io/c5c3/nova-operator:dev"
  for op in keystone placement glance ovn neutron; do
    assert_contains "the ${op}-operator image" "$refs" \
      "ghcr.io/c5c3/${op}-operator:dev"
  done
  assert_contains "the 2025.2 keystone service image" "$refs" \
    "ghcr.io/c5c3/keystone:2025.2"
  assert_contains "and the 2026.1 one beside it" "$refs" \
    "ghcr.io/c5c3/keystone:2026.1"
  assert_contains "every release each sibling ships" "$refs" \
    "ghcr.io/c5c3/neutron:2026.1"
  assert_contains "the OVN daemon image at the pin the scripts resolve" \
    "$refs" "ghcr.io/c5c3/ovn:$(cd "$PROJECT_ROOT" &&
      hack/ci-resolve-ovn-version.sh)"
  assert_contains "the tempest image the functional suites' Jobs run" \
    "$refs" "ghcr.io/c5c3/tempest:2025.2"

  # And the branch still gates: the neutron leg keeps the refs it had.
  : >"$output"
  (cd "$PROJECT_ROOT" &&
    OPERATOR=neutron IMAGE_PREFIX=ghcr.io/c5c3 GITHUB_OUTPUT="$output" \
      bash "$script")
  refs=$(awk '/^refs<<EOF$/ { in_b = 1; next }
    in_b && /^EOF$/ { exit }
    in_b { print }' "$output")

  # neutron-operator:dev, one neutron image per release, ovn-operator:dev and
  # ovn:<pin>.
  assert_eq "the neutron leg resolves its own images plus the two OVN ones" \
    "$((1 + $(release_count neutron) + 2))" \
    "$(printf '%s\n' "$refs" | wc -l | tr -d ' ')"
  assert_not_contains "the sibling block is nova-only" "$refs" "keystone"

  rm -f "$script" "$output"
}

test_nova_leg_narrows_parallelism_and_budget() {
  echo "Test: the nova e2e leg runs two suites at a time inside a 150-minute wall"

  # A Nova suite of #1039 stands a Keystone, an OVNCentral, a Neutron, a
  # Placement, a Glance and the five Nova workloads on one kind node, so at the
  # shared config's parallel: 4 the Pods stay Pending on "Insufficient cpu" and
  # the CRs never reach Ready. Read the condition together with its body, so a
  # nova arm on a branch that no longer narrows anything does not pass.
  #
  # The wall covers what the leg does before its first suite (the kind broker,
  # fourteen sibling image loads and five sibling deploys) and the fifteen
  # suites behind it: at 90 minutes the leg was cancelled on 2026-09-18 with
  # three full-tier suites unfinished and no suite failing. It is an expression
  # on the matrix operator rather than a higher flat number, so only the nova
  # leg spends the extra runner time and no other leg's wall moves with it.
  local narrowing
  narrowing=$(job_step e2e-operator "Run E2E tests" | awk '
    /^ *parallel=\(\)$/ { in_block = 1; next }
    in_block && /^ *fi$/ { exit }
    in_block { print }
  ')

  assert_not_empty "the chainsaw run step narrows the parallelism" "$narrowing"
  assert_contains "the nova leg is one of the narrowed ones" "$narrowing" \
    '[ "${OPERATOR}" = "nova" ]'
  assert_contains "it runs two suites at a time" "$narrowing" \
    "parallel=(--parallel 2)"

  # The arm was added, not swapped in: both legs that were narrowed before it
  # still are.
  assert_contains "the neutron narrowing is kept" "$narrowing" \
    '[ "${OPERATOR}" = "neutron" ]'
  assert_contains "the cinder narrowing is kept" "$narrowing" \
    '[ "${OPERATOR}" = "cinder" ]'

  local job
  job=$(job_block e2e-operator)
  assert_contains "the nova leg gets 150 minutes and the others keep 68" "$job" \
    "timeout-minutes: \${{ matrix.operator == 'nova' && 150 || 68 }}"
  assert_not_contains "no flat wall is left beside the expression" "$job" \
    "timeout-minutes: 68"

  # runs-on on the line above already branches on the same matrix key, which is
  # the precedent this one follows. Asserting it here breaks both together if
  # the axis is ever renamed.
  assert_contains "runs-on branches on the same matrix key" "$job" \
    "runs-on: \${{ matrix.operator == 'keystone'"
}

test_nova_leg_dumps_the_siblings() {
  echo "Test: the nova e2e leg dumps its five sibling Namespaces"

  # The first dump derives its Namespace from the matrix operator and never
  # looks at the siblings', so a failed nova suite would carry no keystone-,
  # placement-, glance-, ovn- or neutron-operator log at all. The dump runs
  # under always(), so it is there when the bring-up itself failed: a sibling
  # deploy that fails at `helm install --wait` skips the chainsaw step but not
  # this one.
  local dump
  dump=$(job_step e2e-operator "Dump diagnostic info (nova siblings)")

  assert_not_empty "the sibling dump step exists" "$dump"
  assert_contains "it dumps on the nova leg even when the suites failed" \
    "$dump" "if: always() && matrix.operator == 'nova'"
  assert_contains "it goes through the shared dump script" "$dump" \
    "hack/ci-dump-diagnostics.sh"
  assert_contains "one call per sibling operator" "$dump" \
    "for op in keystone placement glance ovn neutron; do"
  # The step has no OPERATOR in its env, so a bare call would fall back to
  # OPERATOR="" and print the infrastructure section alone, five times.
  # OPERATOR_ONLY on every pass: the infrastructure block and the openstack Job
  # and pod logs do not change with OPERATOR, and the first dump already
  # emitted them. Without it this step dumps them five more times, which buries
  # the operator evidence it exists for and spends the grace window the cluster
  # delete below needs when the job wall cancels the run.
  assert_contains "each pass hands its sibling to the dump script" "$dump" \
    'OPERATOR="${op}" OPERATOR_ONLY=1 hack/ci-dump-diagnostics.sh'

  # The neutron leg's dump was not widened: on the nova leg the loop above is
  # what covers ovn-system, and the first dump still reads the matrix operator.
  assert_contains "the ovn dump stays neutron-only" \
    "$(job_step e2e-operator "Dump diagnostic info (ovn)")" \
    "if: always() && matrix.operator == 'neutron'"
  assert_contains "the first dump still follows the matrix operator" \
    "$(job_step e2e-operator "Dump diagnostic info")" \
    "OPERATOR: \${{ matrix.operator }}"
}

test_nova_leg_loads_the_tempest_image() {
  echo "Test: the nova e2e leg loads the tempest image its suites' Jobs run"

  # Every functional Nova suite drives its fixture through the `openstack`
  # client, and the client comes out of the tempest image: the catalog setup
  # Jobs, the image seed Jobs and the verify Jobs all name
  # ghcr.io/c5c3/tempest:2025.2, the 2026.1 suites included. kind pulls nothing
  # the run did not load, so a missing ref here is an ImagePullBackOff in the
  # first Job of every suite. The ref sits inside the nova branch: no other leg
  # of this job runs the client, and the ControlPlane suites that do sit in
  # e2e-controlplane with an image list of their own.
  local resolve nova_block
  resolve=$(job_step e2e-operator "Resolve E2E images")
  nova_block=$(awk '
    index($0, "[ \"${OPERATOR}\" = \"nova\" ]") {
      match($0, /^ */); prefix = substr($0, 1, RLENGTH); in_block = 1; next
    }
    in_block && $0 == prefix "fi" { exit }
    in_block { print }
  ' <<<"$resolve")

  assert_not_empty "the nova branch of the resolve step is readable" \
    "$nova_block"
  assert_contains "the tempest ref is resolved inside it" "$nova_block" \
    '${IMAGE_PREFIX}/tempest:2025.2'

  # Where the line sits is not the same claim as what the step publishes: the
  # list is built by concatenation and read back out of GITHUB_OUTPUT, so run
  # the script and look at the refs it actually emits.
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed"
    SKIP=$((SKIP + 1))
    return
  fi

  local script output refs
  script=$(mktemp)
  output=$(mktemp)
  yq -r '.jobs.e2e-operator.steps[]
    | select(.name == "Resolve E2E images") | .run' "$CI_YAML" >"$script"

  (cd "$PROJECT_ROOT" &&
    OPERATOR=nova IMAGE_PREFIX=ghcr.io/c5c3 GITHUB_OUTPUT="$output" \
      bash "$script")
  refs=$(awk '/^refs<<EOF$/ { in_b = 1; next }
    in_b && /^EOF$/ { exit }
    in_b { print }' "$output")

  assert_eq "the tempest image is the last ref the nova leg resolves" \
    "ghcr.io/c5c3/tempest:2025.2" "$(printf '%s\n' "$refs" | tail -1)"
  # Eighteen: the leg's own two, the five siblings' fifteen, the OVN daemon
  # image and this one. A release added under releases/ moves the number.
  assert_eq "it comes on top of the seventeen the leg already had" "18" \
    "$(printf '%s\n' "$refs" | wc -l | tr -d ' ')"

  # And the branch still gates: the cinder leg, whose suites run no client Job,
  # resolves no tempest ref and loads no gigabyte it never uses.
  : >"$output"
  (cd "$PROJECT_ROOT" &&
    OPERATOR=cinder IMAGE_PREFIX=ghcr.io/c5c3 GITHUB_OUTPUT="$output" \
      bash "$script")
  refs=$(awk '/^refs<<EOF$/ { in_b = 1; next }
    in_b && /^EOF$/ { exit }
    in_b { print }' "$output")
  assert_not_contains "and no sibling leg pulls it" "$refs" "tempest"

  rm -f "$script" "$output"
}

test_chaos_nova_leg_runs_the_nova_suites() {
  echo "Test: the e2e-chaos nova leg runs all three nova outage suites"

  # e2e-chaos enumerates test_dirs per leg (chainsaw's include/exclude-regex
  # flags are no-ops in v0.2.14), so a suite missing from the list is
  # lint-checked and never applied to a cluster. The images, the two opt-ins
  # and the three operator deploys are the rest of what the suites need: kind
  # pulls nothing the run did not load, and a CR whose operator never deployed
  # sits without status until the suite times out.
  local entry
  entry=$(e2e_chaos_matrix_entry nova)

  assert_not_empty "the matrix carries a nova leg" "$entry"
  assert_contains "it runs on the self-hosted runners" "$entry" \
    "runner: self-hosted"
  assert_contains "it runs the broker outage suite" "$entry" \
    "tests/e2e-chaos/nova-broker-outage"
  assert_contains "it runs the MariaDB outage suite" "$entry" \
    "tests/e2e-chaos/nova-mariadb-outage"
  assert_contains "it runs the placement outage suite" "$entry" \
    "tests/e2e-chaos/nova-placement-outage"

  # The health check comes first, so a Chaos Mesh that never answers is read
  # off one short suite instead of three that each stand a six-service stack up
  # before their fault fails to inject.
  local health_at first_outage_at
  health_at=$(printf '%s\n' "$entry" |
    grep -nF "tests/e2e/infrastructure/chaos-mesh-health" | head -1 | cut -d: -f1)
  first_outage_at=$(printf '%s\n' "$entry" |
    grep -nF "tests/e2e-chaos/nova-broker-outage" | head -1 | cut -d: -f1)
  assert_not_empty "the leg health-checks the chaos-mesh install" "$health_at"
  assert_gte "and does it before the first outage suite" \
    "$first_outage_at" "$health_at"

  # All three faults are NetworkChaos partitions, so the leg carries the same
  # sch_netem/ip_set dependency that keeps the network leg non-blocking. The
  # whole expression is read, not the nova arm alone: the other two legs stay
  # where they were.
  local job
  job=$(job_block e2e-chaos)
  assert_contains "the nova leg is named non-blocking beside the other two" \
    "$job" \
    "continue-on-error: \${{ matrix.suite == 'network' || matrix.suite == 'ovn' || matrix.suite == 'nova' }}"

  # Seven refs on top of the keystone stack every non-ovn leg loads and the OVN
  # pair the `!= 'pod'` entries already cover here.
  local load kind_load ref
  load=$(job_step e2e-chaos "Load E2E images")
  kind_load=$(job_step e2e-chaos "Load nova leg images into kind")
  assert_not_empty "the leg loads its own images onto the node" "$kind_load"
  assert_contains "that load runs on the nova leg alone" "$kind_load" \
    "if: matrix.suite == 'nova'"
  for ref in placement-operator:dev placement:2025.2 neutron-operator:dev \
    neutron:2025.2 nova-operator:dev nova:2025.2 tempest:2025.2; do
    # Pulled from GHCR ...
    assert_contains "the leg pulls ${ref}" "$load" \
      "matrix.suite == 'nova' && format('{0}/${ref}', env.IMAGE_PREFIX)"
    # ... and handed to the kind node, which the per-leg steps above do not do
    # for this leg: the placement pair is gated on pod, the neutron pair on
    # network.
    assert_contains "and ${ref} reaches the kind node" "$kind_load" \
      "kind load docker-image \${{ env.IMAGE_PREFIX }}/${ref}"
  done

  # nova-mariadb-outage and nova-placement-outage take a vhost on the shared
  # broker (nova-broker-outage brings its own RabbitmqCluster, the one its
  # fault severs), so the leg asks deploy-infra.sh for it. The NFS export is
  # not widened: no nova suite mounts a volume.
  local setup
  setup=$(job_step e2e-chaos "Setup E2E infrastructure")
  assert_contains "the nova leg opts into the shared broker" "$setup" \
    "WITH_MESSAGING: \${{ (matrix.suite == 'network' || matrix.suite == 'nova') && 'true' || '' }}"
  assert_contains "the NFS export stays network-only" "$setup" \
    "WITH_NFS: \${{ matrix.suite == 'network' && 'true' || '' }}"

  # Three deploys on top of the keystone stack and the ovn-operator the
  # `!= 'pod'` step installs here already. The first two names carry a
  # "(nova leg)" suffix: job_step returns the first step of an exact name, and
  # the network leg's placement and neutron steps are pinned by theirs, so a
  # second step under either name would be invisible to every assertion in this
  # directory.
  local spec name op ns deploy
  for spec in \
    "Deploy placement operator (nova leg)|placement|placement-system" \
    "Deploy neutron operator (nova leg)|neutron|neutron-system" \
    "Deploy nova operator|nova|nova-system"; do
    IFS='|' read -r name op ns <<<"$spec"
    deploy=$(job_step e2e-chaos "$name")
    assert_not_empty "the ${op}-operator is deployed" "$deploy"
    # The whole condition, not a substring of it: an extra `|| always()` would
    # install this operator on all four legs.
    assert_eq "the ${op} deploy runs on the nova leg alone" \
      "if: matrix.suite == 'nova'" \
      "$(grep -E '^ *if:' <<<"$deploy" | sed 's/^ *//')"
    assert_contains "it goes through the shared deploy script" "$deploy" \
      "run: hack/ci-deploy-operator.sh"
    assert_contains "it deploys the ${op} operator" "$deploy" "OPERATOR: ${op}"
    assert_contains "it uses the run-tagged ${op}-operator image" "$deploy" \
      "IMAGE_PREFIX }}/${op}-operator"
    assert_contains "it lands in its own Namespace" "$deploy" "NAMESPACE: ${ns}"
  done

  # Order is the load-bearing part: every suite's Nova resolves a Keystone
  # endpoint, a Placement and a Neutron, that Neutron reaches Ready only behind
  # a live OVNCentral, and none of them is reconciled by an operator that has
  # not been installed yet. The image load comes before the infrastructure step
  # for the same reason the other legs' do: deploy-infra.sh is what first
  # schedules Pods against those tags.
  local keystone_at ovn_at placement_at neutron_at nova_at run_at
  local kind_load_at setup_at
  keystone_at=$(printf '%s\n' "$job" |
    grep -nF "name: Deploy operator" | head -1 | cut -d: -f1)
  ovn_at=$(printf '%s\n' "$job" |
    grep -nF "name: Deploy ovn operator" | head -1 | cut -d: -f1)
  placement_at=$(printf '%s\n' "$job" |
    grep -nF "name: Deploy placement operator (nova leg)" | head -1 | cut -d: -f1)
  neutron_at=$(printf '%s\n' "$job" |
    grep -nF "name: Deploy neutron operator (nova leg)" | head -1 | cut -d: -f1)
  nova_at=$(printf '%s\n' "$job" |
    grep -nF "name: Deploy nova operator" | head -1 | cut -d: -f1)
  run_at=$(printf '%s\n' "$job" |
    grep -nF "name: Run chaos E2E tests" | head -1 | cut -d: -f1)
  kind_load_at=$(printf '%s\n' "$job" |
    grep -nF "name: Load nova leg images into kind" | head -1 | cut -d: -f1)
  setup_at=$(printf '%s\n' "$job" |
    grep -nF "name: Setup E2E infrastructure" | head -1 | cut -d: -f1)
  assert_not_empty "the job deploys the keystone-operator" "$keystone_at"
  assert_not_empty "the job deploys the ovn-operator" "$ovn_at"
  assert_not_empty "the job runs the chaos suites" "$run_at"
  assert_gte "the ovn deploy comes after the keystone one" \
    "$ovn_at" "$keystone_at"
  assert_gte "the leg's placement deploy after the ovn one" \
    "$placement_at" "$ovn_at"
  assert_gte "its neutron deploy after the placement one" \
    "$neutron_at" "$placement_at"
  assert_gte "its nova deploy last of the three" "$nova_at" "$neutron_at"
  assert_gte "and every deploy before the suites run" "$run_at" "$nova_at"
  assert_gte "the image load comes before the infrastructure step" \
    "$setup_at" "$kind_load_at"

  # The first dump derives its Namespace from the matrix suite and reads
  # keystone-system on this leg, so a failed nova suite would carry no nova-,
  # placement-, glance-, ovn- or neutron-operator log at all. always() keeps it
  # there when the bring-up itself failed.
  local dump
  dump=$(job_step e2e-chaos "Dump diagnostic info (nova leg)")
  assert_not_empty "the leg dumps the Namespaces the first dump misses" "$dump"
  assert_contains "it dumps even when the suites failed" "$dump" \
    "if: always() && matrix.suite == 'nova'"
  assert_contains "it goes through the shared dump script" "$dump" \
    "hack/ci-dump-diagnostics.sh"
  # One pass per operator because the script reads a single OPERATOR, and a
  # bare call would fall back to OPERATOR="" and print the infrastructure
  # section alone, five times.
  assert_contains "one call per Namespace beyond keystone-system" "$dump" \
    "for op in nova placement glance ovn neutron; do"
  # OPERATOR_ONLY on every pass: the infrastructure block and the openstack Job
  # and pod logs do not change with OPERATOR, and the step above already
  # emitted them. Without it this step dumps them five more times, which buries
  # the operator evidence it exists for and spends the grace window the cluster
  # delete below needs when the job wall cancels the run.
  assert_contains "each pass hands its operator to the dump script" "$dump" \
    'OPERATOR="${op}" OPERATOR_ONLY=1 hack/ci-dump-diagnostics.sh'

  # The wall. The leg loads eighteen images, runs eight operator deploys and
  # then three full-stack suites one after the other (parallel: 1), so 90
  # minutes — sized for the network leg, which runs ten lighter suites — would
  # arrive mid-suite: chainsaw is killed outright, no catch block runs and no
  # JUnit report is written, and a non-blocking leg then reports a cancellation
  # that names no suite.
  assert_contains "the nova leg gets a wall of its own" "$job" \
    "timeout-minutes: \${{ matrix.suite == 'nova' && 150 || 90 }}"

  # And the leg is its own: the network leg, at 78 of its 90 minutes, was not
  # widened into these suites.
  assert_not_contains "the network leg runs no nova suite" \
    "$(e2e_chaos_matrix_entry network)" "tests/e2e-chaos/nova-"
}

test_discover_hosts_takes_an_optional_host() {
  echo "Test: discover-hosts.sh takes the compute host as a third argument"

  # The tempest legs run their fake compute under the kind node's name, because
  # the OVN chassis registers under that name
  # (operators/ovn/internal/controller/reconcile_nodes.go) and neutron binds a
  # port only to a host that has a live chassis. The suites that call the helper
  # with two arguments still wait for fake-1, and a node name carries dots, so
  # the lookup has to match the host as a fixed string.
  local tmp out code
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_discover_stubs "$tmp"

  out=$(run_discover_hosts "$tmp" fake-1 0 1 nova-basic openstack)
  code=$?
  assert_eq "two arguments are still a complete call" "0" "$code"
  assert_contains "the host defaults to fake-1" "$out" \
    "OK: host fake-1 mapped into cell1"

  out=$(run_discover_hosts "$tmp" cobaltcore-control-plane 0 1 \
    nova-tempest-2025-2 openstack cobaltcore-control-plane)
  code=$?
  assert_eq "a third argument is accepted" "0" "$code"
  assert_contains "the helper waits for the host it was handed" "$out" \
    "OK: host cobaltcore-control-plane mapped into cell1"

  # nodeXa is what an unanchored regex match of node.a would accept. The stubbed
  # discovery fails on the second round, so the run ends after one lookup
  # instead of polling to the helper's 150-second deadline.
  out=$(run_discover_hosts "$tmp" nodeXa 1 2 \
    nova-tempest-2025-2 openstack node.a)
  code=$?
  assert_nonzero_exit "a dot in the host matches no other character" "$code"
  assert_not_contains "nodeXa does not answer for node.a" "$out" \
    "OK: host node.a"

  out=$(run_discover_hosts "$tmp" fake-1 0 1 nova-basic)
  code=$?
  assert_eq "one argument is rejected" "2" "$code"
  assert_contains "it prints the usage line" "$(cat "$tmp/stderr")" \
    "usage: discover-hosts.sh <nova-name> <namespace> [host]"

  out=$(run_discover_hosts "$tmp" fake-1 0 1 \
    nova-basic openstack a-host one-too-many)
  code=$?
  assert_eq "four arguments are rejected" "2" "$code"
  assert_contains "it prints the usage line there too" \
    "$(cat "$tmp/stderr")" \
    "usage: discover-hosts.sh <nova-name> <namespace> [host]"

  # A discovery that never reached the databases is reported as itself and stops
  # the run, so the 150 seconds are not spent waiting for a mapping nothing is
  # writing.
  out=$(run_discover_hosts "$tmp" fake-1 7 1 nova-basic openstack)
  code=$?
  assert_eq "a failed discovery exits 1" "1" "$code"
  assert_contains "it names the exit code of nova-manage" \
    "$(cat "$tmp/stderr")" "ERROR: discover_hosts exited 7"
  assert_not_contains "and reports no mapping" "$out" "OK:"
}

test_runner_forwards_the_nova_apis() {
  echo "Test: ci-run-tempest.sh forwards the nova API and the console proxy"

  # tempest.api.compute talks to the compute API, and test_novnc_bad_token
  # dials the novncproxy_base_url the API hands it, which the operator renders
  # as http://<nova>-novncproxy.<ns>.svc.cluster.local:6080/vnc_lite.html while
  # no gateway is set (operators/nova/internal/controller/reconcile_config.go,
  # consoleBaseURL). Both names resolve to nothing on the runner, so both need a
  # forward and an add-host. Each is gated on its own env var: the cinder legs
  # set the compute one alone, and a leg that sets neither runs the forwards it
  # ran before nova existed.
  local tmp out code pf

  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_tempest_stubs "$tmp"

  out=$(run_ci_tempest "$tmp" "" "" "")
  code=$?
  pf="$(cat "$tmp/pf.log")"
  assert_eq "a leg that names no Nova reaches the container" "0" "$code"
  assert_contains "keystone is forwarded on 5000" "$pf" "5000:5000"
  assert_not_contains "and nothing is forwarded on 8774" "$pf" "8774:8774"
  assert_not_contains "and nothing on 6080" "$pf" "6080:6080"
  assert_not_contains "the container resolves no nova name" \
    "$(cat "$tmp/docker.log")" "--add-host nova"

  out=$(run_ci_tempest "$tmp" nova-tempest-2025-2 nova-tempest-2025-2-novncproxy "")
  code=$?
  pf="$(cat "$tmp/pf.log")"
  assert_eq "the nova leg reaches the container" "0" "$code"
  assert_contains "the compute API is forwarded on 8774" "$pf" \
    "svc/nova-tempest-2025-2 -n openstack 8774:8774"
  assert_contains "the console proxy on 6080" "$pf" \
    "svc/nova-tempest-2025-2-novncproxy -n openstack 6080:6080"

  # Nova registers no healthcheck middleware, so the compute API is polled on
  # the path the operator probes. The console proxy answers /vnc_lite.html,
  # which is the page test_novnc_bad_token asks for.
  assert_contains "the compute API is polled on its root path" \
    "$(cat "$tmp/curl.log")" "http://localhost:8774/"
  assert_contains "the console proxy on the page the test dials" \
    "$(cat "$tmp/curl.log")" "http://localhost:6080/vnc_lite.html"

  # Both DNS forms, the way the runner add-hosts every other optional target.
  local docker_argv
  docker_argv="$(cat "$tmp/docker.log")"
  assert_contains "the container resolves the compute FQDN to the forward" \
    "$docker_argv" "nova-tempest-2025-2.openstack.svc.cluster.local:127.0.0.1"
  assert_contains "and its short form" "$docker_argv" \
    "nova-tempest-2025-2.openstack.svc:127.0.0.1"
  assert_contains "the console proxy FQDN too" "$docker_argv" \
    "nova-tempest-2025-2-novncproxy.openstack.svc.cluster.local:127.0.0.1"
  assert_contains "and its short form" "$docker_argv" \
    "nova-tempest-2025-2-novncproxy.openstack.svc:127.0.0.1"

  # A compute API that never answers stops the run before the container starts,
  # instead of handing tempest a catalog whose compute endpoint refuses.
  out=$(run_ci_tempest "$tmp" nova-tempest-2025-2 nova-tempest-2025-2-novncproxy 8774)
  code=$?
  assert_eq "an unreachable compute API fails the leg" "1" "$code"
  assert_contains "the error names the API and its port" "$out" \
    "::error::Nova API at http://localhost:8774 did not become reachable after 10 attempts"
  assert_eq "and no container is started" "" "$(cat "$tmp/docker.log")"

  out=$(run_ci_tempest "$tmp" nova-tempest-2025-2 nova-tempest-2025-2-novncproxy 6080)
  code=$?
  assert_eq "an unreachable console proxy fails it too" "1" "$code"
  assert_contains "under its own name" "$out" \
    "::error::NovaConsole API at http://localhost:6080 did not become reachable after 10 attempts"

  # The cinder legs attach volumes to a server and never open a console.
  out=$(run_ci_tempest "$tmp" nova-tempest-2025-2 "" "")
  code=$?
  pf="$(cat "$tmp/pf.log")"
  assert_eq "a cinder leg reaches the container" "0" "$code"
  assert_contains "it forwards the compute API" "$pf" "8774:8774"
  assert_not_contains "and no console proxy" "$pf" "6080:6080"
}

test_runner_forwards_placement() {
  echo "Test: ci-run-tempest.sh forwards the placement API"

  # Both compute-stack legs register a placement endpoint in the catalog on a
  # cluster-internal name (tests/tempest/nova-2025-2/01-catalog-setup-job.yaml)
  # and set `placement = true` under [service_available]. Tempest builds its
  # clients lazily, so without the forward the break lands on the first call: a
  # name-resolution error reported as a test error, not as a skip, and the
  # serial retry pass repeats it through the same missing forward.
  local tmp out code pf

  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_tempest_stubs "$tmp"

  out=$(run_ci_tempest "$tmp" "" "" "" "")
  code=$?
  pf="$(cat "$tmp/pf.log")"
  assert_eq "a leg that names no Placement reaches the container" "0" "$code"
  assert_not_contains "and nothing is forwarded on 8778" "$pf" "8778:8778"
  assert_not_contains "the container resolves no placement name" \
    "$(cat "$tmp/docker.log")" "--add-host placement"

  out=$(run_ci_tempest "$tmp" nova-tempest-2025-2 "" "" placement-nova-tempest-2025-2)
  code=$?
  pf="$(cat "$tmp/pf.log")"
  assert_eq "the compute-stack leg reaches the container" "0" "$code"
  assert_contains "the placement API is forwarded on 8778" "$pf" \
    "svc/placement-nova-tempest-2025-2 -n openstack 8778:8778"

  # Placement registers no healthcheck middleware either, so the poll asks for
  # the root path, which is what the operator probes
  # (operators/placement/internal/controller/reconcile_deployment_pin_test.go).
  assert_contains "the placement API is polled on its root path" \
    "$(cat "$tmp/curl.log")" "http://localhost:8778/"

  local docker_argv
  docker_argv="$(cat "$tmp/docker.log")"
  assert_contains "the container resolves the placement FQDN to the forward" \
    "$docker_argv" \
    "placement-nova-tempest-2025-2.openstack.svc.cluster.local:127.0.0.1"
  assert_contains "and its short form" "$docker_argv" \
    "placement-nova-tempest-2025-2.openstack.svc:127.0.0.1"

  # A placement API that never answers stops the run before the container
  # starts, the way every other optional target does.
  out=$(run_ci_tempest "$tmp" nova-tempest-2025-2 "" 8778 placement-nova-tempest-2025-2)
  code=$?
  assert_eq "an unreachable placement API fails the leg" "1" "$code"
  assert_contains "the error names the API and its port" "$out" \
    "::error::Placement API at http://localhost:8778 did not become reachable after 10 attempts"
}

test_runner_reports_a_forward_that_dropped() {
  echo "Test: ci-run-tempest.sh names a port-forward that died during the run"

  # kubectl pins a forward to one pod and exits for good when that pod goes
  # away. On a leg whose wall is 150 minutes that is a live risk, and with the
  # forward's output discarded the job log showed hundreds of connection errors
  # and no cause. Every forward now writes its own log into the results artifact,
  # and a forward that is gone when the container returns is named — Keystone on
  # 5000 included, which is the one every leg opens.
  local tmp out code

  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_tempest_stubs "$tmp"

  out=$(run_ci_tempest "$tmp" nova-tempest-2025-2 "" "" "")
  code=$?
  assert_eq "a leg whose forwards held returns the container's code" "0" "$code"
  assert_not_contains "and nothing is reported as dropped" "$out" \
    "port-forward on 8774 died"
  assert_eq "the compute forward keeps a log of its own" "yes" \
    "$(test -f "$tmp/output/port-forward-Nova.log" && echo yes || echo no)"
  assert_eq "the keystone forward keeps one too" "yes" \
    "$(test -f "$tmp/output/port-forward-Keystone.log" && echo yes || echo no)"
  assert_not_contains "and keystone is not reported as dropped either" "$out" \
    "port-forward on 5000 died"

  # 8774 is forwarded, answers its readiness poll out of $STUB_PF_LOG, and is
  # gone by the time the container returns.
  out=$(run_ci_tempest "$tmp" nova-tempest-2025-2 "" "" "" 8774)
  code=$?
  assert_eq "a dropped forward does not change the leg's exit code" "0" "$code"
  assert_contains "the log names the API that went away" "$out" \
    "::error::The Nova port-forward on 8774 died during the run"
  assert_contains "and says the failures below are not service failures" "$out" \
    "are not service failures"

  # Every test issues a token through the 5000 forward first, so a keystone
  # forward that dropped fails the rest of the suite in both stestr phases and
  # the serial retry pass. This leg names no optional API at all, which is the
  # keystone shape: there the 5000 forward is the only one there is to report.
  out=$(run_ci_tempest "$tmp" "" "" "" "" 5000)
  code=$?
  assert_eq "a dropped keystone forward does not change the exit code" "0" "$code"
  assert_contains "the log names keystone when the 5000 forward went away" "$out" \
    "::error::The Keystone port-forward on 5000 died during the run"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_all_operators_lists_nova
test_service_operators_lists_nova
test_nova_filter_is_wired
test_nova_change_produces_an_e2e_leg
test_helm_filter_covers_the_nova_chart
test_helm_validate_renders_the_nova_chart
test_go_matrices_list_nova
test_cleanup_matrices_cover_the_nova_images
test_a_keystone_only_change_produces_no_nova_leg
test_nova_image_filter_is_wired
test_nova_e2e_filter_is_wired
test_nova_leg_opts_into_the_broker
test_nova_leg_deploys_the_sibling_operators
test_nova_leg_narrows_parallelism_and_budget
test_nova_leg_dumps_the_siblings
test_nova_leg_loads_the_tempest_image
test_chaos_nova_leg_runs_the_nova_suites
test_discover_hosts_takes_an_optional_host
test_runner_forwards_the_nova_apis
test_runner_forwards_placement
test_runner_reports_a_forward_that_dropped

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
