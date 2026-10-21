#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the driver of the lab failover suites, hack/lab-failover.sh (#1393):
#   1. Without a subcommand and with an unknown one it prints a usage to
#      stderr and exits 2, and `make -n lab-failover-<subcommand>` calls it
#      with the subcommand.
#   2. preflight exits 2 with LAB_FAILOVER_CONTEXT unset, on a malformed
#      LAB_FAILOVER_MIN_FREE_MEMORY and on a LAB_FAILOVER_RUN that is no label
#      value, and exits 1 on a context missing from the kubeconfig (naming
#      it), on a kind- context before any kubectl call, on an API that does
#      not answer (kubectl's error on stderr), on a cluster without
#      kube-system/shoot-info, on each missing prerequisite (a FAIL line
#      naming it), and on free memory requests below the floor (naming
#      both). On the lab it prints one OK line per check, the shoot, the
#      server and the free figure, and exits 0; a node that is not Ready does
#      not count, and a pod counts with its init containers, its sidecars and
#      its overhead.
#   3. up, run and down exit 1 naming the expected and the given shoot without
#      LAB_FAILOVER_GO_AHEAD and with another shoot's name, with no mutating
#      call. up refuses an existing openstack-ha with nothing applied.
#   4. up brings the plane up in order: the namespace with its labels, the
#      two Secret copies with type, data and the copy label only,
#      the OVNCentral, the seed, the ControlPlane, the ExternalSecret after
#      ESOTenantStoreReady, the database tenant, the db-sync patch, the seed
#      Job, the fake compute and its host mapping; it writes versions.txt and
#      topology.txt. It applies no OpenBaoTenant.
#   5. Across up, run and down no call but the two Secret reads names the
#      namespace openstack.
#   6. run runs the eleven suites in order, one summary.tsv row each, through
#      a KUBECONFIG of mode 0600 that names the context; a scenario argument
#      or LAB_FAILOVER_SCENARIOS narrows it; an unknown scenario exits 2 with
#      the eleven names and no chainsaw call; a namespace without the run
#      label or a plane that is not Ready exits 1 before any suite; a suite
#      that fails before verify stops the run with a FAIL row reading none and
#      prints the suites not run.
#   7. down prints nothing to delete without the namespace, refuses a
#      namespace without the run label with no delete call, deletes in order,
#      every delete absent-tolerant, and on a namespace that does not go lists
#      what is left and exits 1. Between the ControlPlane and the namespace it
#      deletes the tenant openstack-ha an earlier up applied, then waits until
#      no OpenBaoTenant targets openstack-ha; a tenant that stays,
#      or a tenant list that keeps failing, stops it with exit 1 before the
#      namespace delete, naming the tenant or the error. It deletes the
#      workload pods, then purges
#      the plane's volumes and images with Job cp-ha-purge before it deletes
#      the seed Job and the ControlPlane,
#      skips the purge without the seed Job, and stops before the seed Job and
#      the ControlPlane, printing the Job's log, when the purge fails.
#   8. The temporary kubeconfig is gone after every subcommand.
#
# The script is sourced in a subshell (its BASH_SOURCE guard keeps lf_main
# from running) with a stub kubectl, chainsaw and sleep first on PATH; the
# output directory, the bootstrap scripts and discover-hosts.sh are pointed at
# stubs after sourcing.
#
# Usage: bash tests/unit/hack/lab_failover_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
LF_SH="$PROJECT_ROOT/hack/lab-failover.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# The kubectl calls that change the cluster.
MUTATING='^kubectl (apply|create|delete|patch|replace|scale|exec|label|annotate|rollout restart) '

ELEVEN="galera-member-loss galera-primary-loss rabbitmq-node-loss rabbitmq-minority-partition ovn-nb-leader-kill ovn-sb-leader-kill ovn-nb-member-loss ovn-sb-member-loss ovn-nb-quorum-loss ovn-sb-quorum-loss api-eviction-drain"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stubs <dir>
# A kubectl stub that appends its argv to $CALL_LOG, captures stdin into
# $CAPTURE_DIR and answers from the environment:
#   KUBECTL_CONTEXT         the one context of the kubeconfig (garden-lab)
#   KUBECTL_API_DOWN        non-empty: `version` fails
#   KUBECTL_NO_SHOOT_INFO   non-empty: kube-system/shoot-info is NotFound
#   KUBECTL_SHOOT           the shootName (newforge)
#   KUBECTL_MISSING         the `get` arguments of the one object that is
#                           missing, such as "crd podchaos.chaos-mesh.org"
#   KUBECTL_REQUESTED       the memory one pod requests (40Gi); the three Ready
#                           nodes offer 30Gi each, a NotReady one 100Gi
#   KUBECTL_NS              absent, labelled or unlabelled: openstack-ha
#                           (default absent)
#   KUBECTL_NS_STAYS        non-empty: the namespace delete times out
#   KUBECTL_FALSE           the condition type that reads False
#   KUBECTL_PURGE_FAILS     non-empty: Job cp-ha-purge does not complete
#   KUBECTL_TENANT_STAYS    non-empty: the tenant stale targets openstack-ha
#                           (beside openstack, which targets openstack)
#   KUBECTL_TENANT_LIST_FAILS non-empty: listing the OpenBaoTenants is
#                           Forbidden
# The chainsaw stub appends its argv and the mode of $KUBECONFIG to
# $CALL_LOG; the CHAINSAW_FAIL_AT-th call fails before verify, every other
# prints two evaluate lines. The bootstrap scripts and discover-hosts.sh
# append their argv. sleep is a no-op.
make_stubs() {
  local dir="$1"
  mkdir -p "$dir/bin" "$dir/bootstrap"
  cat >"$dir/bin/kubectl" <<'STUB'
#!/bin/bash
cap="$CAPTURE_DIR"
echo "kubectl $*" >>"$CALL_LOG"
missing() {
  [ -n "${KUBECTL_MISSING:-}" ] && [ "$1" = "$KUBECTL_MISSING" ]
}
not_found() {
  echo "Error from server (NotFound): $1 not found" >&2
  exit 1
}
case "$*" in
  "config view --raw --minify --flatten --context "*)
    ctx="${*: -1}"
    if [ "$ctx" != "${KUBECTL_CONTEXT:-garden-lab}" ]; then
      echo "error: cannot locate context ${ctx}" >&2
      exit 1
    fi
    printf 'apiVersion: v1\nkind: Config\ncurrent-context: %s\n' "$ctx"
    ;;
  "version --request-timeout=10s")
    if [ -n "${KUBECTL_API_DOWN:-}" ]; then
      echo 'The connection to the server api.lab.example:443 was refused - did you specify the right host or port?' >&2
      exit 1
    fi
    echo 'Server Version: v1.35.6'
    ;;
  "config view -o jsonpath={.clusters[0].cluster.server}") printf 'https://api.lab.example:443' ;;
  "get configmap shoot-info -n kube-system -o jsonpath={.data.shootName}")
    [ -z "${KUBECTL_NO_SHOOT_INFO:-}" ] || not_found 'configmaps "shoot-info"'
    printf '%s' "${KUBECTL_SHOOT:-newforge}"
    ;;
  "get clustersecretstore openbao-cluster-store -o jsonpath="*)
    if missing clustersecretstore; then printf 'False'; else printf 'True'; fi
    ;;
  "get pod openbao-0 -n shared-services -o jsonpath={.status.phase}")
    missing "pod openbao-0" && not_found 'pods "openbao-0"'
    printf 'Running'
    ;;
  "get nodes -o json")
    node() { printf '{"status":{"allocatable":{"memory":"%s"},"conditions":[{"type":"Ready","status":"%s"}]}}' "$1" "$2"; }
    echo "{\"items\":[$(node 30Gi True),$(node 31457280Ki True),$(node 30720Mi True),$(node 100Gi False)]}"
    ;;
  "get pods -A --field-selector=status.phase!=Succeeded,status.phase!=Failed -o json")
    # 40Gi, an init container of 512Mi, and 2.25Gi: an app container and a
    # sidecar of 1Gi each and an overhead of 256Mi.
    echo "{\"items\":[{\"spec\":{\"containers\":[{\"resources\":{\"requests\":{\"memory\":\"${KUBECTL_REQUESTED:-40Gi}\"}}}]}},{\"spec\":{\"containers\":[{}],\"initContainers\":[{\"resources\":{\"requests\":{\"memory\":\"512Mi\"}}}]}},{\"spec\":{\"containers\":[{\"resources\":{\"requests\":{\"memory\":\"1Gi\"}}}],\"initContainers\":[{\"restartPolicy\":\"Always\",\"resources\":{\"requests\":{\"memory\":\"1Gi\"}}}],\"overhead\":{\"memory\":\"256Mi\"}}}]}"
    ;;
  "get namespace openstack-ha -o name")
    [ "${KUBECTL_NS:-absent}" != absent ] || not_found 'namespaces "openstack-ha"'
    echo namespace/openstack-ha
    ;;
  "get namespace openstack-ha -o jsonpath={.metadata.labels.lab-failover\.c5c3\.io/run}")
    case "${KUBECTL_NS:-absent}" in
      absent) not_found 'namespaces "openstack-ha"' ;;
      labelled) printf '20261019T120000Z' ;;
    esac
    ;;
  "get "*" -o name")
    args="$*"
    args="${args#get }"
    missing "${args% -o name}" && not_found "${args% -o name}"
    echo "${args%% *}/x"
    ;;
  "create -f -") cat >"$cap/namespace.yaml" ;;
  "get secret garage-s3-credentials -n openstack -o json" | "get secret ceph-client-cinder -n openstack -o json")
    printf '{"apiVersion":"v1","kind":"Secret","type":"Opaque","data":{"key":"czNjcjN0"},"metadata":{"name":"%s","namespace":"openstack","uid":"u-1","resourceVersion":"42","annotations":{"reconcile.external-secrets.io/data-hash":"h"},"ownerReferences":[{"kind":"ExternalSecret","name":"x"}]}}\n' "$3"
    ;;
  "apply -n openstack-ha -f -") cat >>"$cap/secrets.json" ;;
  "get secret openbao-init-keys -n shared-services -o jsonpath={.data.init-output}")
    printf '{"root_token": "root"}' | base64
    ;;
  "get "*"].status}")
    ctype="$(sed -E "s/.*type=='([A-Za-z]+)'.*/\1/" <<<"$*")"
    if [ "$ctype" = "${KUBECTL_FALSE:-}" ]; then printf 'False'; else printf 'True'; fi
    ;;
  "get "*"].reason}") printf 'AllReady' ;;
  "get pods -n openstack-ha -l app.kubernetes.io/name=nova-fake-compute,app.kubernetes.io/instance=cp-ha-nova -o jsonpath={.items[0].spec.nodeName}")
    printf 'lab-node-1'
    ;;
  "delete namespace openstack-ha "*)
    if [ -n "${KUBECTL_NS_STAYS:-}" ]; then
      echo 'error: timed out waiting for the condition on namespaces/openstack-ha' >&2
      exit 1
    fi
    ;;
  "wait job/cp-ha-purge -n openstack-ha "*)
    if [ -n "${KUBECTL_PURGE_FAILS:-}" ]; then
      echo 'error: timed out waiting for the condition on jobs/cp-ha-purge' >&2
      exit 1
    fi
    ;;
  "logs job/cp-ha-purge -n openstack-ha --tail=50") echo 'ERROR: a volume of cp-ha did not delete:' ;;
  "api-resources --verbs=list --namespaced -o name") printf 'pods\npersistentvolumeclaims\n' ;;
  "get openbaotenants -n openbao-operator-system -o json")
    if [ -n "${KUBECTL_TENANT_LIST_FAILS:-}" ]; then
      echo 'Error from server (Forbidden): openbaotenants.openbao.org is forbidden: User "lab" cannot list resource "openbaotenants"' >&2
      exit 1
    fi
    if [ -n "${KUBECTL_TENANT_STAYS:-}" ]; then
      echo '{"items":[{"metadata":{"name":"openstack"},"spec":{"targetNamespace":"openstack"}},{"metadata":{"name":"stale"},"spec":{"targetNamespace":"openstack-ha"}}]}'
    else
      echo '{"items":[]}'
    fi
    ;;
  "get pods,persistentvolumeclaims -n openstack-ha --ignore-not-found")
    echo 'NAME                                  STATUS'
    echo 'persistentvolumeclaim/storage-openstack-db-0   Terminating'
    ;;
  "get "*" -o json") echo '{"items":[]}' ;;
esac
exit 0
STUB
  cat >"$dir/bin/chainsaw" <<'STUB'
#!/bin/bash
n=$(($(grep -c '^chainsaw ' "$CALL_LOG") + 1))
echo "chainsaw $*" >>"$CALL_LOG"
echo "kubeconfig $(ls -l "$KUBECONFIG" | cut -c1-10) $(grep current-context "$KUBECONFIG")" >>"$CALL_LOG"
echo "    | 12:00:00 | step-1 | SCRIPT | RUN |"
if [ "$n" = "${CHAINSAW_FAIL_AT:-}" ]; then
  echo "        FAIL: start: no READY within 300s on: token"
  exit 1
fi
echo "        stream token: 120 ok, 3 failed in window (budget 30), 0 outside; observed failures=3 recovery=12.5s"
echo "        stream network: 60 ok, 0 failed in window (budget 30), 0 outside; observed failures=0 recovery=0.0s"
echo "        OK: verify x"
exit 0
STUB
  # shellcheck disable=SC2016 # $0, $* and $CALL_LOG expand when the stubs run
  for script in "$dir/bootstrap/write-bootstrap-secrets.sh" "$dir/bootstrap/setup-database-tenant.sh" "$dir/discover-hosts.sh"; do
    printf '#!/bin/bash\necho "$(basename "$0") KORC_CONTROLPLANES=${KORC_CONTROLPLANES:-} $*" >>"$CALL_LOG"\n' >"$script"
  done
  # shellcheck disable=SC2016 # $* and $CALL_LOG expand when the stub runs
  printf '#!/bin/bash\necho "sleep $*" >>"$CALL_LOG"\n' >"$dir/bin/sleep"
  chmod +x "$dir"/bin/* "$dir"/bootstrap/*.sh "$dir/discover-hosts.sh"
}

# new_case <tmp> — A fresh stub directory, capture directory and logs.
new_case() {
  local tmp="$1"
  rm -rf "${tmp:?}/stub" "$tmp/capture" "$tmp/tmpdir" "$tmp/out"
  mkdir -p "$tmp/capture" "$tmp/tmpdir" "$tmp/out"
  make_stubs "$tmp/stub"
  export CALL_LOG="$tmp/calls.log" CAPTURE_DIR="$tmp/capture"
  : >"$CALL_LOG"
}

# run_lf <tmp> <subcommand...> [-- env_var=value...] — Source the script with
# the stubs first on PATH, the context garden-lab, the go-ahead newforge and
# the given settings, point the output, the bootstrap scripts and
# discover-hosts.sh at the stubs, set the waits of up and run to 0, set
# LF_WAIT_GONE to LF_TEST_WAIT_GONE when that is given, and run lf_main. stdout
# goes to <tmp>/stdout, stderr to <tmp>/stderr; returns lf_main's status.
run_lf() {
  local tmp="$1" args=() rc
  shift
  while [[ $# -gt 0 && "$1" != "--" ]]; do
    args+=("$1")
    shift
  done
  [[ $# -gt 0 ]] && shift
  (
    unset LAB_FAILOVER_RUN LAB_FAILOVER_SCENARIOS LAB_FAILOVER_MIN_FREE_MEMORY KUBECONFIG
    export LAB_FAILOVER_CONTEXT=garden-lab LAB_FAILOVER_GO_AHEAD=newforge
    for assignment in "$@"; do
      export "${assignment?}"
    done
    PATH="$tmp/stub/bin:${LF_TEST_PATH:-$PATH}"
    TMPDIR="$tmp/tmpdir"
    export PATH TMPDIR
    # shellcheck source=/dev/null
    source "$LF_SH"
    # shellcheck disable=SC2034 # read by the sourced script
    {
      LF_OUTPUT_DIR="$tmp/out"
      LF_DISCOVER_HOSTS="$tmp/stub/discover-hosts.sh"
      CP_BOOTSTRAP_DIR="$tmp/stub/bootstrap"
      LF_POLL_INTERVAL=0
      LF_WAIT_OVN=0 LF_WAIT_INFRA=0 LF_WAIT_TENANT_STORE=0 LF_WAIT_MARIADB=0 LF_WAIT_KEYSTONE=0
      LF_WAIT_NOVA=0 LF_WAIT_DB_SYNC=0 LF_WAIT_SEED=0 LF_WAIT_COMPUTE=0 LF_WAIT_READY=0
      LF_WAIT_RUN_READY=0
      [ -z "${LF_TEST_WAIT_GONE:-}" ] || LF_WAIT_GONE="$LF_TEST_WAIT_GONE"
    }
    lf_main ${args[@]+"${args[@]}"}
  ) >"$tmp/stdout" 2>"$tmp/stderr"
  rc=$?
  return "$rc"
}

# mutations — The mutating kubectl calls of $CALL_LOG.
mutations() {
  grep -E "$MUTATING" "$CALL_LOG" || true
}

# fails <tmp> <description> <status> <message> <subcommand> [env_var=value...]
# — The subcommand exits <status>, its output holds <message>, and it makes no
# mutating kubectl call.
fails() {
  local tmp="$1" description="$2" want="$3" message="$4" sub="$5" rc
  shift 5
  new_case "$tmp"
  run_lf "$tmp" "$sub" -- "$@"
  rc=$?
  assert_eq "$description: $sub exits $want" "$want" "$rc"
  assert_contains "$description: the message names it" "$(cat "$tmp/stdout" "$tmp/stderr")" "$message"
  assert_eq "$description: no mutating kubectl call" "" "$(mutations)"
}

# ---------------------------------------------------------------------------
# Test 1: usage and make targets
# ---------------------------------------------------------------------------
test_usage() {
  echo "Test: without a subcommand and with an unknown one the usage goes to stderr with exit 2"
  local tmp rc sub
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  for sub in "" "restart"; do
    new_case "$tmp"
    if [[ -z "$sub" ]]; then run_lf "$tmp"; else run_lf "$tmp" "$sub"; fi
    rc=$?
    assert_eq "'${sub:-<none>}' exits 2" "2" "$rc"
    assert_contains "'${sub:-<none>}' prints the usage to stderr" "$(cat "$tmp/stderr")" "Usage: hack/lab-failover.sh"
    assert_eq "'${sub:-<none>}' calls no kubectl" "" "$(cat "$CALL_LOG")"
  done
  for sub in preflight up run down; do
    assert_contains "the usage names $sub" "$(cat "$tmp/stderr")" "  $sub "
    assert_eq "make -n lab-failover-$sub calls hack/lab-failover.sh $sub" "hack/lab-failover.sh $sub" \
      "$(make -s -n -C "$PROJECT_ROOT" "lab-failover-$sub" 2>/dev/null)"
  done
}

# ---------------------------------------------------------------------------
# Test 2: preflight
# ---------------------------------------------------------------------------
test_preflight_refusals() {
  echo "Test: preflight refuses each missing setting, context, API and prerequisite"
  local tmp object run
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  fails "$tmp" "an unset context" 2 "LAB_FAILOVER_CONTEXT is not set" preflight LAB_FAILOVER_CONTEXT=
  assert_eq "an unset context: no kubectl call" "" "$(cat "$CALL_LOG")"
  fails "$tmp" "a malformed floor" 2 "LAB_FAILOVER_MIN_FREE_MEMORY must be <n>Gi or <n>Mi" preflight \
    LAB_FAILOVER_MIN_FREE_MEMORY=lots
  assert_eq "a malformed floor: no kubectl call" "" "$(cat "$CALL_LOG")"
  # The run id is a label value and a directory under _output/lab-failover/.
  for run in "../x" ".hidden" "$(printf 'a%.0s' $(seq 1 64))"; do
    fails "$tmp" "run id '${run:0:8}'" 2 "LAB_FAILOVER_RUN must be a label value" preflight LAB_FAILOVER_RUN="$run"
    assert_eq "run id '${run:0:8}': no kubectl call" "" "$(cat "$CALL_LOG")"
  done
  fails "$tmp" "a context not in the kubeconfig" 1 "FAIL: context garden-gone is not in the kubeconfig" preflight \
    LAB_FAILOVER_CONTEXT=garden-gone
  fails "$tmp" "a kind context" 1 "FAIL: context kind-cobaltcore is a kind cluster" preflight \
    LAB_FAILOVER_CONTEXT=kind-cobaltcore
  assert_eq "a kind context: no kubectl call at all" "" "$(grep '^kubectl ' "$CALL_LOG" || true)"
  fails "$tmp" "an API that does not answer" 1 "FAIL: the API of context garden-lab does not answer" preflight \
    KUBECTL_API_DOWN=1
  assert_contains "kubectl's own error goes to stderr" "$(cat "$tmp/stderr")" "api.lab.example:443 was refused"
  fails "$tmp" "a cluster without shoot-info" 1 "FAIL: not a Gardener shoot" preflight KUBECTL_NO_SHOOT_INFO=1

  for object in "namespace chaos-mesh" "crd podchaos.chaos-mesh.org" "crd networkchaos.chaos-mesh.org" \
    "clusterissuer openstack-ovn-ca-issuer" "secret openbao-init-keys -n shared-services" \
    "secret garage-s3-credentials -n openstack" "secret ceph-client-cinder -n openstack" \
    "crd controlplanes.c5c3.io" "crd ovncentrals.ovn.openstack.c5c3.io" "crd openbaotenants.openbao.org"; do
    fails "$tmp" "without $object" 1 "FAIL: " preflight KUBECTL_MISSING="$object"
    assert_contains "without $object: the FAIL line names it" "$(grep '^FAIL: ' "$tmp/stdout")" \
      "$(awk '{ if ($NF == "shared-services" || $NF == "openstack") print $NF "/" $2; else print $2 }' <<<"$object")"
  done
  fails "$tmp" "a ClusterSecretStore that is not Ready" 1 "FAIL: ClusterSecretStore openbao-cluster-store Ready is missing or not True" \
    preflight KUBECTL_MISSING=clustersecretstore
  fails "$tmp" "without openbao-0" 1 "FAIL: pod shared-services/openbao-0 is missing or not Running" \
    preflight KUBECTL_MISSING="pod openbao-0"
  fails "$tmp" "a floor above the free figure" 1 \
    "FAIL: free memory requests 47.2 GiB are below the floor LAB_FAILOVER_MIN_FREE_MEMORY=60Gi" \
    preflight LAB_FAILOVER_MIN_FREE_MEMORY=60Gi

  if [[ ! -x /usr/bin/chainsaw && ! -x /bin/chainsaw ]]; then
    new_case "$tmp"
    rm "$tmp/stub/bin/chainsaw"
    LF_TEST_PATH=/usr/bin:/bin run_lf "$tmp" preflight
    assert_eq "without chainsaw: preflight exits 1" "1" "$?"
    assert_contains "the FAIL line names chainsaw" "$(cat "$tmp/stdout")" "FAIL: not on PATH: chainsaw"
  else
    echo "  SKIP: chainsaw is installed in /usr/bin or /bin"
    SKIP=$((SKIP + 2))
  fi
}

test_preflight_passes() {
  echo "Test: preflight passes on the lab with one OK line per check and the figures"
  local tmp rc out
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  new_case "$tmp"
  run_lf "$tmp" preflight
  rc=$?
  out="$(cat "$tmp/stdout")"
  assert_eq "preflight exits 0" "0" "$rc"
  assert_eq "seventeen OK lines, one per check" "17" "$(grep -c '^OK: ' "$tmp/stdout")"
  assert_eq "no FAIL line" "" "$(grep '^FAIL: ' "$tmp/stdout" || true)"
  assert_contains "it names the shoot" "$out" "shoot: newforge"
  assert_contains "it names the server" "$out" "server: https://api.lab.example:443"
  assert_contains "the NotReady node does not count; init containers, sidecars and the overhead do" "$out" \
    "free memory requests: 47.2 GiB"
  assert_contains "it names the figures" "$out" "(allocatable 90 GiB on 3 Ready nodes, requested 42.7 GiB)"
  assert_eq "it makes no mutating call" "" "$(mutations)"
  assert_eq "the temporary kubeconfig is gone" "" "$(ls "$tmp/tmpdir")"
}

# ---------------------------------------------------------------------------
# Test 3: the go-ahead and the namespace refusal
# ---------------------------------------------------------------------------
test_go_ahead() {
  echo "Test: up, run and down need the go-ahead for the context's shoot"
  local tmp sub
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  for sub in up run down; do
    fails "$tmp" "$sub without a go-ahead" 1 "no go-ahead for shoot 'newforge': LAB_FAILOVER_GO_AHEAD is ''" "$sub" \
      LAB_FAILOVER_GO_AHEAD= KUBECTL_NS=labelled
    fails "$tmp" "$sub with forge's go-ahead on newforge" 1 \
      "no go-ahead for shoot 'newforge': LAB_FAILOVER_GO_AHEAD is 'forge'" "$sub" LAB_FAILOVER_GO_AHEAD=forge KUBECTL_NS=labelled
    assert_eq "$sub with forge's go-ahead: no chainsaw call" "" "$(grep '^chainsaw ' "$CALL_LOG" || true)"
  done
  fails "$tmp" "up on an existing namespace" 1 "namespace openstack-ha exists; refusing to adopt it" up KUBECTL_NS=unlabelled
}

# ---------------------------------------------------------------------------
# Test 4: up
# ---------------------------------------------------------------------------
# order_of <pattern...> — The line numbers of the first call matching each
# pattern, space-separated.
order_of() {
  local p
  for p in "$@"; do
    grep -n -m1 -F -- "$p" "$CALL_LOG" | cut -d: -f1
  done | xargs
}

test_up() {
  echo "Test: up brings the plane up in order and writes versions.txt and topology.txt"
  local tmp rc fixtures order sorted
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  fixtures="$PROJECT_ROOT/tests/lab-failover/fixtures"
  new_case "$tmp"
  run_lf "$tmp" up -- LAB_FAILOVER_RUN=20261019T120000Z
  rc=$?
  assert_eq "up exits 0" "0" "$rc"
  order="$(order_of "kubectl create -f -" \
    "get secret garage-s3-credentials -n openstack -o json" "get secret ceph-client-cinder -n openstack -o json" \
    "apply -f $fixtures/01-ovncentral.yaml" "get ovncentral cp-ha-ovn" "write-bootstrap-secrets.sh" \
    "apply -f $fixtures/03-controlplane.yaml" "type=='InfrastructureReady'" "type=='ESOTenantStoreReady'" \
    "apply -f $fixtures/02-horizon-secret-key-externalsecret.yaml" "get mariadb openstack-db" \
    "setup-database-tenant.sh" "type=='KeystoneReady'" "type=='NovaReady'" "patch neutron cp-ha-neutron" \
    "apply -f $fixtures/04-seed-job.yaml" "wait job/cp-ha-seed" "apply -f $fixtures/05-fake-compute.yaml" \
    "rollout status deploy/cp-ha-fake-compute" "discover-hosts.sh" \
    "get controlplane cp-ha -n openstack-ha -o jsonpath={.status.conditions[?(@.type=='Ready')].status}")"
  sorted="$(tr ' ' '\n' <<<"$order" | sort -n | xargs)"
  assert_eq "every step ran" "21" "$(wc -w <<<"$order" | tr -d ' ')"
  assert_eq "the steps ran in order" "$sorted" "$order"
  assert_eq "up applies no OpenBaoTenant: the c5c3-operator admits the namespace" "" \
    "$(mutations | grep -iE 'openbaotenant|00-openbao-tenant' || true)"
  assert_contains "the namespace carries the run label" "$(cat "$tmp/capture/namespace.yaml")" \
    'lab-failover.c5c3.io/run: "20261019T120000Z"'
  assert_contains "the namespace carries the apiserver-proxy label" "$(cat "$tmp/capture/namespace.yaml")" \
    "apiserver-proxy.networking.gardener.cloud/inject: disable"
  assert_contains "the namespace carries the Chaos Mesh annotation" "$(cat "$tmp/capture/namespace.yaml")" \
    "chaos-mesh.org/inject: enabled"
  assert_eq "the two copies keep type, data and the copy label alone" \
    '{"apiVersion":"v1","kind":"Secret","type":"Opaque","data":{"key":"czNjcjN0"},"metadata":{"name":"garage-s3-credentials","namespace":"openstack-ha","labels":{"lab-failover.c5c3.io/copied":"true"}}}
{"apiVersion":"v1","kind":"Secret","type":"Opaque","data":{"key":"czNjcjN0"},"metadata":{"name":"ceph-client-cinder","namespace":"openstack-ha","labels":{"lab-failover.c5c3.io/copied":"true"}}}' \
    "$(jq -c . "$tmp/capture/secrets.json")"
  assert_contains "the seed names the plane" "$(cat "$CALL_LOG")" "write-bootstrap-secrets.sh KORC_CONTROLPLANES=openstack-ha/cp-ha"
  assert_contains "the database tenant names the plane" "$(cat "$CALL_LOG")" "setup-database-tenant.sh KORC_CONTROLPLANES= openstack-ha cp-ha"
  assert_contains "the db-sync patch sets log mode and suspend" "$(cat "$CALL_LOG")" \
    'patch neutron cp-ha-neutron -n openstack-ha --type merge -p {"spec":{"ovnDBSync":{"syncMode":"log","suspend":true}}}'
  assert_contains "the host is mapped under the node's name" "$(cat "$CALL_LOG")" "discover-hosts.sh KORC_CONTROLPLANES= cp-ha-nova openstack-ha lab-node-1"
  assert_contains "versions.txt names the commit" "$(cat "$tmp/out/20261019T120000Z/versions.txt")" \
    "commit: $(git -C "$PROJECT_ROOT" rev-parse HEAD)"
  assert_contains "topology.txt holds the free memory before up" "$(cat "$tmp/out/20261019T120000Z/topology.txt")" \
    "before up: 47.2 GiB"
  assert_contains "topology.txt holds the partition handling" "$(cat "$tmp/out/20261019T120000Z/topology.txt")" \
    "cluster_partition_handling:"
  assert_contains "topology.txt holds the quota" "$(cat "$CALL_LOG")" "describe resourcequota -n openstack-ha"

  new_case "$tmp"
  run_lf "$tmp" up -- KUBECTL_FALSE=NovaReady
  assert_eq "a step that fails stops up with 1" "1" "$?"
  assert_contains "it names the step" "$(cat "$tmp/stdout")" "ERROR: up failed at: wait for NovaReady"
  assert_eq "nothing after it ran" "" "$(grep -F 'patch neutron' "$CALL_LOG" || true)"
}

# ---------------------------------------------------------------------------
# Test 5: the namespace openstack
# ---------------------------------------------------------------------------
test_openstack_untouched() {
  echo "Test: up, run and down read the two Secrets in openstack and nothing else there"
  local tmp calls
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  new_case "$tmp"
  run_lf "$tmp" up -- LAB_FAILOVER_RUN=20261019T120000Z
  run_lf "$tmp" run -- KUBECTL_NS=labelled
  run_lf "$tmp" down -- KUBECTL_NS=labelled
  calls="$(grep -E -- '-n openstack( |$)' "$CALL_LOG" | sort -u)"
  assert_eq "only the two Secrets are read in openstack" \
    "kubectl get secret ceph-client-cinder -n openstack -o json
kubectl get secret ceph-client-cinder -n openstack -o name
kubectl get secret garage-s3-credentials -n openstack -o json
kubectl get secret garage-s3-credentials -n openstack -o name" "$calls"
}

# ---------------------------------------------------------------------------
# Test 6: run
# ---------------------------------------------------------------------------
test_run() {
  echo "Test: run executes the suites in order and stops at the first FAIL"
  local tmp rc summary suites
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  summary="$tmp/out/20261019T120000Z/summary.tsv"

  new_case "$tmp"
  run_lf "$tmp" run -- KUBECTL_NS=labelled
  rc=$?
  assert_eq "run exits 0 when every suite passes" "0" "$rc"
  suites="$(grep '^chainsaw ' "$CALL_LOG" | awk '{ print $NF }' | xargs -n1 basename | xargs)"
  assert_eq "it runs the eleven in the table's order" "$ELEVEN" "$suites"
  assert_contains "it hands chainsaw the lab config and a report path per suite" "$(cat "$CALL_LOG")" \
    "chainsaw test --config $PROJECT_ROOT/tests/lab-failover/chainsaw-config.yaml --report-path $tmp/out/20261019T120000Z/galera-member-loss $PROJECT_ROOT/tests/lab-failover/galera-member-loss"
  assert_eq "chainsaw sees a KUBECONFIG of mode 0600 on the context" "kubeconfig -rw------- current-context: garden-lab" \
    "$(grep '^kubeconfig ' "$CALL_LOG" | sort -u)"
  assert_eq "summary.tsv has a header and eleven rows" "12" "$(wc -l <"$summary" | tr -d ' ')"
  assert_eq "every row passed" "11" "$(awk -F'\t' '$2 == "PASS"' "$summary" | wc -l | tr -d ' ')"
  assert_eq "a row carries the observed lines of evaluate" \
    "stream token: 120 ok, 3 failed in window (budget 30), 0 outside; observed failures=3 recovery=12.5s; stream network: 60 ok, 0 failed in window (budget 30), 0 outside; observed failures=0 recovery=0.0s" \
    "$(awk -F'\t' 'NR == 2 { print $5 }' "$summary")"
  assert_eq "each suite's log is kept" "11" "$(find "$tmp/out/20261019T120000Z" -name '*.log' | wc -l | tr -d ' ')"
  assert_eq "the temporary kubeconfig is gone" "" "$(ls "$tmp/tmpdir")"

  new_case "$tmp"
  run_lf "$tmp" run galera-member-loss -- KUBECTL_NS=labelled
  assert_eq "run galera-member-loss runs one suite" "galera-member-loss" \
    "$(awk -F'\t' 'NR > 1 { print $1 }' "$summary" | xargs)"
  new_case "$tmp"
  run_lf "$tmp" run -- KUBECTL_NS=labelled LAB_FAILOVER_SCENARIOS="ovn-sb-quorum-loss rabbitmq-node-loss"
  assert_eq "LAB_FAILOVER_SCENARIOS names the suites in its order" "ovn-sb-quorum-loss rabbitmq-node-loss" \
    "$(awk -F'\t' 'NR > 1 { print $1 }' "$summary" | xargs)"

  new_case "$tmp"
  run_lf "$tmp" run no-such-suite -- KUBECTL_NS=labelled
  assert_eq "an unknown scenario exits 2" "2" "$?"
  assert_contains "it names the scenario" "$(cat "$tmp/stderr")" "unknown scenario 'no-such-suite'"
  # shellcheck disable=SC2086 # one name per line
  assert_contains "it lists the eleven" "$(cat "$tmp/stderr")" "$(printf '  %s\n' $ELEVEN)"
  assert_eq "an unknown scenario: no call at all" "" "$(cat "$CALL_LOG")"
  fails "$tmp" "a namespace without the run label" 1 "carries no label lab-failover.c5c3.io/run" run KUBECTL_NS=unlabelled
  assert_eq "without the label: no chainsaw call" "" "$(grep '^chainsaw ' "$CALL_LOG" || true)"
  fails "$tmp" "a plane that is not Ready" 1 "ControlPlane openstack-ha/cp-ha is not Ready/AllReady; no suite runs" run \
    KUBECTL_NS=labelled KUBECTL_FALSE=Ready
  assert_eq "not Ready: no chainsaw call" "" "$(grep '^chainsaw ' "$CALL_LOG" || true)"

  new_case "$tmp"
  run_lf "$tmp" run -- KUBECTL_NS=labelled CHAINSAW_FAIL_AT=2
  rc=$?
  assert_eq "a failing second suite stops run with 1" "1" "$rc"
  assert_eq "two suites ran" "2" "$(grep -c '^chainsaw ' "$CALL_LOG")"
  assert_eq "the second row is a FAIL with no observed figures" "galera-primary-loss FAIL none" \
    "$(awk -F'\t' 'NR == 3 { print $1, $2, $5 }' "$summary")"
  assert_contains "it prints the nine not run" "$(cat "$tmp/stdout")" \
    "not run: rabbitmq-node-loss rabbitmq-minority-partition ovn-nb-leader-kill ovn-sb-leader-kill ovn-nb-member-loss ovn-sb-member-loss ovn-nb-quorum-loss ovn-sb-quorum-loss api-eviction-drain"
}

# ---------------------------------------------------------------------------
# Test 7: down
# ---------------------------------------------------------------------------
test_down() {
  echo "Test: down deletes in order, resumes, and refuses a namespace up did not make"
  local tmp rc deletes order sorted fixtures
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  fixtures="$PROJECT_ROOT/tests/lab-failover/fixtures"

  new_case "$tmp"
  run_lf "$tmp" down
  rc=$?
  assert_eq "down without the namespace exits 0" "0" "$rc"
  assert_contains "it says nothing to delete" "$(cat "$tmp/stdout")" "nothing to delete"
  assert_eq "it deletes nothing" "" "$(mutations)"

  fails "$tmp" "a namespace without the run label" 1 "carries no label lab-failover.c5c3.io/run" down KUBECTL_NS=unlabelled

  new_case "$tmp"
  run_lf "$tmp" down -- KUBECTL_NS=labelled
  rc=$?
  assert_eq "down exits 0" "0" "$rc"
  deletes="$(grep '^kubectl delete ' "$CALL_LOG")"
  assert_eq "it deletes in order, absent-tolerant" \
    "kubectl delete deployment cp-ha-fake-compute -n openstack-ha --ignore-not-found
kubectl delete pod -n openstack-ha -l app.kubernetes.io/name=chaos-workload --ignore-not-found --wait --timeout=300s
kubectl delete job cp-ha-purge -n openstack-ha --ignore-not-found --cascade=foreground
kubectl delete job cp-ha-seed -n openstack-ha --ignore-not-found
kubectl delete controlplane cp-ha -n openstack-ha --ignore-not-found --wait --timeout=12m
kubectl delete ovncentral cp-ha-ovn -n openstack-ha --ignore-not-found --wait --timeout=300s
kubectl delete pvc -n openstack-ha -l app.kubernetes.io/name=mariadb --ignore-not-found --wait --timeout=300s
kubectl delete pvc -n openstack-ha -l app.kubernetes.io/name=openstack-rabbitmq --ignore-not-found --wait --timeout=300s
kubectl delete secret -n openstack-ha -l lab-failover.c5c3.io/copied=true --ignore-not-found
kubectl delete externalsecret cp-ha-horizon-secret-key -n openstack-ha --ignore-not-found
kubectl delete openbaotenant openstack-ha -n openbao-operator-system --ignore-not-found --wait --timeout=300s
kubectl delete namespace openstack-ha --ignore-not-found --wait --timeout=600s" "$deletes"
  order="$(order_of "delete controlplane cp-ha" "delete openbaotenant openstack-ha -n openbao-operator-system" \
    "get openbaotenants -n openbao-operator-system -o json" "delete namespace openstack-ha")"
  sorted="$(tr ' ' '\n' <<<"$order" | sort -n | xargs)"
  assert_eq "it deletes an earlier up's tenant, then reads the OpenBaoTenants, between the ControlPlane and the namespace" "4" \
    "$(wc -w <<<"$order" | tr -d ' ')"
  assert_eq "and in that order" "$sorted" "$order"
  assert_eq "with no tenant targeting openstack-ha the wait returns after one read" "1" \
    "$(grep -c '^kubectl get openbaotenants ' "$CALL_LOG")"
  order="$(order_of "delete deployment cp-ha-fake-compute" "delete pod -n openstack-ha -l app.kubernetes.io/name=chaos-workload" \
    "get job cp-ha-seed -n openstack-ha -o name" "delete job cp-ha-purge" "apply -f $fixtures/06-purge-job.yaml" \
    "wait job/cp-ha-purge -n openstack-ha --for=condition=complete --timeout=600s" "delete job cp-ha-seed" \
    "delete controlplane cp-ha")"
  sorted="$(tr ' ' '\n' <<<"$order" | sort -n | xargs)"
  assert_eq "the purge runs between the workload pods and the seed Job" "8" "$(wc -w <<<"$order" | tr -d ' ')"
  assert_eq "and in that order" "$sorted" "$order"

  new_case "$tmp"
  run_lf "$tmp" down -- KUBECTL_NS=labelled KUBECTL_MISSING="job cp-ha-seed -n openstack-ha"
  assert_eq "down without the seed Job exits 0" "0" "$?"
  assert_contains "it says the plane stored nothing" "$(cat "$tmp/stdout")" \
    "no Job cp-ha-seed, so the plane stored no volume or image"
  assert_eq "it runs no purge" "" "$(grep -E 'cp-ha-purge|06-purge-job' "$CALL_LOG" || true)"
  assert_contains "it goes on to the ControlPlane" "$(cat "$CALL_LOG")" "delete controlplane cp-ha"

  new_case "$tmp"
  run_lf "$tmp" down -- KUBECTL_NS=labelled KUBECTL_PURGE_FAILS=1
  assert_eq "a purge that fails stops down with 1" "1" "$?"
  assert_contains "it names the step" "$(cat "$tmp/stdout")" \
    "ERROR: down failed at: delete the volumes and images of cp-ha through its APIs"
  assert_contains "it prints the Job's log" "$(cat "$tmp/stdout")" "ERROR: a volume of cp-ha did not delete:"
  assert_eq "it keeps the seed Job and the ControlPlane for another try" "" \
    "$(grep -E '^kubectl delete (job cp-ha-seed|controlplane) ' "$CALL_LOG" || true)"

  new_case "$tmp"
  run_lf "$tmp" down -- KUBECTL_NS=labelled KUBECTL_TENANT_STAYS=1 LF_TEST_WAIT_GONE=0
  assert_eq "a tenant that stays fails down with 1" "1" "$?"
  assert_contains "it names the step" "$(cat "$tmp/stdout")" \
    "ERROR: down failed at: wait for the OpenBaoTenants of openstack-ha to be gone"
  assert_contains "it names the tenant that targets openstack-ha, and only that one" "$(cat "$tmp/stdout")" \
    "OpenBaoTenants still target openstack-ha: stale"$'\n'
  assert_eq "it keeps the namespace" "" "$(grep '^kubectl delete namespace ' "$CALL_LOG" || true)"

  new_case "$tmp"
  run_lf "$tmp" down -- KUBECTL_NS=labelled KUBECTL_TENANT_LIST_FAILS=1 LF_TEST_WAIT_GONE=0
  assert_eq "a tenant list that fails fails down with 1" "1" "$?"
  assert_contains "it says the wait ran out" "$(cat "$tmp/stdout")" "did not hold within 0s"
  assert_contains "it names the error" "$(cat "$tmp/stdout")" "Error from server (Forbidden)"
  assert_eq "it keeps the namespace on a failed list too" "" "$(grep '^kubectl delete namespace ' "$CALL_LOG" || true)"

  new_case "$tmp"
  run_lf "$tmp" down -- KUBECTL_NS=labelled KUBECTL_NS_STAYS=1
  rc=$?
  assert_eq "a namespace that does not go fails down with 1" "1" "$rc"
  assert_contains "it names the step" "$(cat "$tmp/stdout")" "ERROR: down failed at: delete namespace openstack-ha"
  assert_contains "it lists what is left" "$(cat "$tmp/stdout")" "persistentvolumeclaim/storage-openstack-db-0"
  assert_eq "the temporary kubeconfig is gone" "" "$(ls "$tmp/tmpdir")"
}

test_usage
test_preflight_refusals
test_preflight_passes
test_go_ahead
test_up
test_openstack_untouched
test_run
test_down

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
