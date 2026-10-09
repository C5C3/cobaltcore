#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh gates the lab Ceph behind WITH_CEPH, an opt-in of
# the external-cluster mode alone:
#   1. WITH_CEPH defaults to false, true, false and yes pass through verbatim,
#      and CEPH_TIMEOUT defaults to 900.
#   2. The script carries seven literal "${WITH_CEPH}" == "true" gates (listed
#      in test_gate_count), the banner names the flag, and the composite action
#      setup-e2e-infra does not thread it (#1339 owns kind and CI).
#   3. The apply of the overlay root's ceph/ follows the base apply and sits
#      directly under a gate; rook-ceph is appended to the Phase 3 wait list
#      under a gate while the base list stays as documented; the apply of
#      ceph/cluster/ follows, under a gate, a wait_for_crds line that names the
#      three Rook CRDs; wait_for_ceph_cluster and then sync_ceph_client_keys
#      run under one gate after the OpenBao bootstrap; neither apply passes
#      --load-restrictor and nothing pipes kustomize build into kubectl.
#   4. preflight_checks refuses WITH_CEPH=true in kind mode before Docker is
#      asked, passes WITH_CEPH=yes there, passes WITH_CEPH=true for the default
#      lab overlay under EXTERNAL_CLUSTER=true, and refuses it for an overlay
#      without ceph/cluster/kustomization.yaml or without ceph/ at all, after
#      the base/ check; WITH_CEPH=yes passes the same overlay.
#   5. wait_for_ceph_cluster, against a kubectl stub, returns 0 and logs the
#      fsid once the CephCluster is Ready with HEALTH_OK and at least as many
#      ready OSD Deployments as its device set counts, also with an OSD more
#      than the count, as after a lowered count, keeps polling until then, and
#      exits 1 with the phase, the health and the ready OSDs on a timeout:
#      under HEALTH_WARN, with two of three OSDs ready, for an empty answer and
#      for a read that fails. The timeout dump names the prepare pods' and the
#      operator's logs and not the other pods'.
#   6. sync_ceph_client_keys annotates both SecretStores before it waits on
#      them, waits on both PushSecrets before it annotates the ExternalSecrets,
#      then waits for those in openstack; a store or a PushSecret that stays
#      unready exits 1 with what to read, before the next step.
#
# The script is sourced (its BASH_SOURCE guard keeps main() from running); the
# functions run against shims on a private PATH (preflight) or a recording
# kubectl stub in front of the real PATH with the real jq (the waits).
#
# Usage: bash tests/unit/hack/deploy_infra_ceph_flag_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"
SETUP_ACTION="$PROJECT_ROOT/.github/actions/setup-e2e-infra/action.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# The literal gate the tests count and locate.
GATE='"${WITH_CEPH}" == "true"'

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# resolve <expression> [env_var=value...]
# Sources deploy-infra.sh with the given overrides and echoes <expression>.
resolve() {
  local expression="$1"
  shift
  (
    unset WITH_CEPH CEPH_TIMEOUT
    for assignment in "$@"; do
      export "${assignment?}"
    done
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    eval "printf '%s' \"${expression}\""
  )
}

# assert_file_contains_literal DESCRIPTION FILE LITERAL
# Same contract as assert_file_contains, but matches with grep -F so the `${}`
# and `()` in the pinned lines need no escaping.
assert_file_contains_literal() {
  local description="$1" file="$2" literal="$3"
  if grep -qF -- "$literal" "$file"; then
    echo "  PASS: $description"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $description (literal not found in $file)"
    echo "    expected line: $literal"
    FAIL=$((FAIL + 1))
  fi
}

# line_of <fixed string> — the first line of deploy-infra.sh containing it.
line_of() {
  grep -nF -- "$1" "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1
}

# gate_before <line> — the last WITH_CEPH gate above <line>.
gate_before() {
  grep -nF -- "$GATE" "$DEPLOY_INFRA_SH" |
    awk -F: -v target="${1:-0}" '$1 < target { last = $1 } END { print last }'
}

# assert_directly_after <description> <line a> <line b> — b is the line after a.
assert_directly_after() {
  if [ -n "$2" ] && [ -n "$3" ] && [ "$3" -eq "$(($2 + 1))" ]; then
    echo "  PASS: $1"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $1 (line '${3}' does not follow line '${2}')"
    FAIL=$((FAIL + 1))
  fi
}

# make_stub_path <dir> <cmd>...
# A private PATH: an exit-0 shim for each <cmd> that records its call in
# $STUB_LOG, plus the two utilities the sourced script needs itself (dirname
# for SCRIPT_DIR, date for log).
make_stub_path() {
  local dir="$1" cmd
  shift
  mkdir -p "$dir"
  ln -s "$(command -v dirname)" "$dir/dirname"
  ln -s "$(command -v date)" "$dir/date"
  for cmd in "$@"; do
    # shellcheck disable=SC2016 # $* and $STUB_LOG expand when the shim runs
    printf '#!/bin/bash\necho "%s $*" >>"${STUB_LOG:-/dev/null}"\nexit 0\n' "$cmd" >"$dir/$cmd"
    chmod +x "$dir/$cmd"
  done
}

# run_preflight <path> [env_var=value...]
# Sources deploy-infra.sh with PATH=<path> and the given overrides and runs
# preflight_checks. Echoes combined output; returns its exit status.
run_preflight() {
  local path="$1"
  shift
  (
    unset EXTERNAL_CLUSTER EXTERNAL_OVERLAY WITH_CONTROLPLANE WITH_CONTROLPLANE_CR \
      CONTROLPLANE_NAME WITH_VPA WITH_METRICS_SERVER WITH_REGISTRY_CACHE \
      WITH_CHAOS_MESH WITH_OVN_KERNEL_MODULES WITH_NFS WITH_DIZZY WITH_PROMETHEUS \
      WITH_CEPH
    for assignment in "$@"; do
      export "${assignment?}"
    done
    PATH="$path"
    export PATH
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    preflight_checks
  ) 2>&1
}

# make_kubectl_stub <dir>
# A kubectl that records its argv in $KUBECTL_LOG and answers from the
# environment:
#   KUBECTL_CEPHCLUSTER        file answering `get cephcluster rook-ceph`
#   KUBECTL_CEPHCLUSTER_LATER  file answering it from the read after
#                              KUBECTL_CEPHCLUSTER_READS reads on (the reads
#                              are counted in cephcluster-reads beside the stub)
#   KUBECTL_CEPHCLUSTER_RC     non-empty: that read fails with Forbidden
#   KUBECTL_OSDS               file answering `get deployment -l
#                              app=rook-ceph-osd` (default: no Deployment)
#   KUBECTL_SECRETSTORE_WAIT_RC exit code of every SecretStore wait (default 0)
#   KUBECTL_PUSHSECRET_WAIT_RC  exit code of every PushSecret wait (default 0)
#   KUBECTL_PUSHSECRET_MESSAGE  the Ready message a PushSecret read prints
# `get pods -o name` answers an OSD prepare pod and a mon pod, `logs` one line
# naming what it read, and an ExternalSecret read a synced one.
make_kubectl_stub() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
dir="$(dirname "$0")"
echo "kubectl $*" >>"${KUBECTL_LOG:-/dev/null}"
case "$*" in
  "-n rook-ceph get cephcluster rook-ceph -o json")
    if [ -n "${KUBECTL_CEPHCLUSTER_RC:-}" ]; then
      echo 'Error from server (Forbidden): cephclusters.ceph.rook.io "rook-ceph" is forbidden' >&2
      exit 1
    fi
    reads=$(( $(cat "$dir/cephcluster-reads" 2>/dev/null || echo 0) + 1 ))
    echo "$reads" >"$dir/cephcluster-reads"
    if [ -n "${KUBECTL_CEPHCLUSTER_LATER:-}" ] && [ "$reads" -gt "${KUBECTL_CEPHCLUSTER_READS:-0}" ]; then
      cat "$KUBECTL_CEPHCLUSTER_LATER"
    else
      cat "$KUBECTL_CEPHCLUSTER"
    fi
    ;;
  "-n rook-ceph get deployment -l app=rook-ceph-osd -o json")
    cat "${KUBECTL_OSDS:-/dev/null}" 2>/dev/null || echo '{"items":[]}'
    ;;
  "-n rook-ceph get pods -o name")
    printf '%s\n' pod/rook-ceph-mon-a-6d9f7c9b4-x2kq7 pod/rook-ceph-osd-prepare-set1-data-0abcd-k8w2m
    ;;
  "-n rook-ceph logs "*)
    echo "log of ${4:-}"
    ;;
  "wait secretstore/openbao-ceph-store "*)
    exit "${KUBECTL_SECRETSTORE_WAIT_RC:-0}"
    ;;
  "wait --for=condition=Ready pushsecret/"*)
    exit "${KUBECTL_PUSHSECRET_WAIT_RC:-0}"
    ;;
  "get pushsecret/"*)
    printf '%s' "${KUBECTL_PUSHSECRET_MESSAGE:-}"
    ;;
  "get externalsecret "*)
    echo '{"status":{"conditions":[{"type":"Ready","status":"True","reason":"SecretSynced"}]}}'
    ;;
esac
exit 0
STUB
  chmod +x "$dir/kubectl"
}

# run_fn <stub_dir> <function> [env_var=value...]
# Sources deploy-infra.sh with the kubectl stub first on the real PATH (jq is
# the real one), a sleep of a fifth of a second, and the given overrides, and
# runs <function>. Echoes combined output; returns its status.
run_fn() {
  local stub_dir="$1" fn="$2"
  shift 2
  (
    unset CEPH_TIMEOUT POD_TIMEOUT EXTERNALSECRET_TIMEOUT
    for assignment in "$@"; do
      export "${assignment?}"
    done
    PATH="$stub_dir:$PATH"
    export PATH
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    sleep() { command sleep 0.2; }
    "$fn"
  ) 2>&1
}

# cephcluster <phase> <health> — a CephCluster with one device set of three.
cephcluster() {
  printf '{"spec":{"storage":{"storageClassDeviceSets":[{"name":"lightbits","count":3}]}},'
  printf '"status":{"phase":"%s","ceph":{"health":"%s","fsid":"5b1f7a2e-0c1d-4f6e-9a3b-2d8c7e6f5a41",' "$1" "$2"
  printf '"details":{"PG_AVAILABILITY":{"message":"Reduced data availability: 1 pg inactive","severity":"HEALTH_WARN"}}},'
  printf '"conditions":[{"type":"Progressing","status":"True","reason":"ClusterProgressing"}]}}\n'
}

# osd_deployments <ready>... — one OSD Deployment per argument, with that
# readyReplicas.
osd_deployments() {
  local items="" i=0 r
  for r in "$@"; do
    items="${items}${items:+,}{\"metadata\":{\"name\":\"rook-ceph-osd-${i}\"},\"status\":{\"readyReplicas\":${r}}}"
    i=$((i + 1))
  done
  printf '{"items":[%s]}\n' "$items"
}

# ---------------------------------------------------------------------------
# Test 1: the knobs
# ---------------------------------------------------------------------------
# shellcheck disable=SC2016 # resolve expands the expression after it sources the script
test_knobs() {
  echo "Test: WITH_CEPH and CEPH_TIMEOUT"

  assert_eq "WITH_CEPH defaults to false" "false" "$(resolve '${WITH_CEPH}')"
  assert_eq "WITH_CEPH=true is preserved" "true" "$(resolve '${WITH_CEPH}' WITH_CEPH=true)"
  assert_eq "WITH_CEPH=false is preserved" "false" "$(resolve '${WITH_CEPH}' WITH_CEPH=false)"
  assert_eq "WITH_CEPH=yes is preserved verbatim" "yes" "$(resolve '${WITH_CEPH}' WITH_CEPH=yes)"
  assert_eq "CEPH_TIMEOUT defaults to 900" "900" "$(resolve '${CEPH_TIMEOUT}')"
  assert_eq "CEPH_TIMEOUT is overridable" "60" "$(resolve '${CEPH_TIMEOUT}' CEPH_TIMEOUT=60)"
}

# ---------------------------------------------------------------------------
# Test 2: the strict gates, the banner and the composite action
# A typo like WITH_CEPH=yes must take the skip branch at every gate.
# ---------------------------------------------------------------------------
test_gate_count() {
  echo "Test: deploy-infra.sh has seven strict WITH_CEPH gates and names the flag in its banner"

  # The kind refusal, the external preflight check, the Step 3 apply, the
  # Phase 3 append, the Step 5 wait and apply, the waits after Step 8, and the
  # completion hint.
  local gate_count
  gate_count="$(grep -cE '"\$\{WITH_CEPH\}" == "true"' "$DEPLOY_INFRA_SH" || true)"
  assert_eq "deploy-infra.sh has exactly 7 strict WITH_CEPH==true gates" "7" "$gate_count"
  assert_eq "no gate compares WITH_CEPH any other way" "" \
    "$(grep -E 'WITH_CEPH\}?"? *(!=|==|=~)' "$DEPLOY_INFRA_SH" | grep -vF -- "$GATE" || true)"

  assert_file_contains "the banner has a Ceph storage stack line" "$DEPLOY_INFRA_SH" 'Ceph storage stack'
  assert_file_contains "the banner names the variable that turns it on" "$DEPLOY_INFRA_SH" 'set WITH_CEPH=true'
  assert_directly_after "the completion hint names the toolbox under a gate" \
    "$(gate_before "$(line_of 'Ceph:   kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph status')")" \
    "$(line_of 'Ceph:   kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph status')"

  assert_file_not_contains "setup-e2e-infra does not thread WITH_CEPH (#1339)" "$SETUP_ACTION" 'WITH_CEPH'
}

# ---------------------------------------------------------------------------
# Test 3: the order of the applies and waits
# ---------------------------------------------------------------------------
test_apply_and_wait_order() {
  echo "Test: the two Ceph applies, the Phase 3 append and the waits sit under their gates in order"

  local base_line apply_line append_line crds_line cluster_line wait_line sync_line bootstrap_line
  base_line="$(line_of 'kubectl apply -k "${OVERLAY_ROOT}/base"')"
  apply_line="$(line_of 'kubectl apply -k "${OVERLAY_ROOT}/ceph"')"
  assert_not_empty "the apply of the overlay root's ceph/ is found" "$apply_line"
  assert_eq "it follows the base apply" "true" \
    "$([[ -n "$apply_line" && -n "$base_line" && "$apply_line" -gt "$base_line" ]] && echo true || echo false)"
  assert_directly_after "and sits directly under a WITH_CEPH gate" "$(gate_before "$apply_line")" "$apply_line"
  assert_file_contains_literal "the apply is logged" "$DEPLOY_INFRA_SH" \
    'log "Ceph operator overlay ${OVERLAY_ROOT}/ceph applied (WITH_CEPH=true)."'

  assert_file_contains "helm_releases keeps the base releases in the documented order" "$DEPLOY_INFRA_SH" \
    'helm_releases=(prometheus-operator-crds shared-services/openbao mariadb-operator-crds mariadb-operator external-secrets memcached-operator envoy-gateway garage-operator openbao-operator)'
  assert_file_not_contains "the base helm_releases array does not hard-code rook-ceph" "$DEPLOY_INFRA_SH" \
    'helm_releases=(prometheus-operator-crds.*rook-ceph'
  append_line="$(line_of 'helm_releases+=(rook-ceph)')"
  assert_not_empty "rook-ceph is appended to the Phase 3 wait list" "$append_line"
  assert_directly_after "under a WITH_CEPH gate" "$(gate_before "$append_line")" "$append_line"
  assert_eq "before the wait on the list" "true" \
    "$([[ -n "$append_line" && "$append_line" -lt "$(line_of 'wait_for_helmreleases "${release_wait_timeout}" "${helm_releases[@]}"')" ]] && echo true || echo false)"

  crds_line="$(line_of 'wait_for_crds "${POD_TIMEOUT}" cephclusters.ceph.rook.io cephblockpools.ceph.rook.io cephclients.ceph.rook.io')"
  cluster_line="$(line_of 'kubectl apply -k "${OVERLAY_ROOT}/ceph/cluster"')"
  assert_not_empty "a wait_for_crds line names the three Rook CRDs" "$crds_line"
  assert_directly_after "it sits directly under a WITH_CEPH gate" "$(gate_before "$crds_line")" "$crds_line"
  assert_directly_after "the apply of ceph/cluster/ follows it" "$crds_line" "$cluster_line"
  assert_eq "after the infrastructure overlay apply of Step 5" "true" \
    "$([[ -n "$cluster_line" && "$cluster_line" -gt "$(line_of 'log "Infrastructure kustomize overlay applied."')" ]] && echo true || echo false)"
  assert_file_contains_literal "the apply is logged" "$DEPLOY_INFRA_SH" \
    'log "Ceph cluster overlay ${OVERLAY_ROOT}/ceph/cluster applied (WITH_CEPH=true)."'

  wait_line="$(grep -nE '^[[:space:]]+wait_for_ceph_cluster$' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  sync_line="$(grep -nE '^[[:space:]]+sync_ceph_client_keys$' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  bootstrap_line="$(line_of 'log "OpenBaoCluster CR is Available."')"
  assert_directly_after "wait_for_ceph_cluster is called directly under a WITH_CEPH gate" \
    "$(gate_before "$wait_line")" "$wait_line"
  assert_directly_after "sync_ceph_client_keys follows it under the same gate" "$wait_line" "$sync_line"
  assert_eq "both run after the OpenBao bootstrap and Step 8" "true" \
    "$([[ -n "$wait_line" && "$wait_line" -gt "$bootstrap_line" ]] && echo true || echo false)"

  # kubectl apply -k uses the embedded kustomize, which has no
  # --load-restrictor (kubernetes/kubectl#948); both overlays are local.
  assert_eq "neither Ceph apply passes --load-restrictor" "" \
    "$(grep -F 'kubectl apply -k "${OVERLAY_ROOT}/ceph' "$DEPLOY_INFRA_SH" | grep -F -- '--load-restrictor' || true)"
  assert_eq "nothing pipes kustomize build into kubectl apply" "" \
    "$(grep -E 'kustomize build.*\| *kubectl apply -f -' "$DEPLOY_INFRA_SH" || true)"
}

# ---------------------------------------------------------------------------
# Test 4: preflight
# ---------------------------------------------------------------------------
test_preflight() {
  echo "Test: preflight_checks refuses WITH_CEPH=true in kind mode and for an overlay without both Ceph kustomizations"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stub_path "$tmp/bin" docker kind kubectl jq
  export STUB_LOG="$tmp/stub.log"

  : >"$STUB_LOG"
  output="$(run_preflight "$tmp/bin" WITH_CEPH=true)"
  rc=$?
  assert_eq "WITH_CEPH=true is refused in kind mode" "1" "$rc"
  assert_contains "with the kind-mode error" "$output" \
    "ERROR: WITH_CEPH=true is not supported in kind mode: the Ceph overlay exists only under deploy/lab/metal-stack, and Ceph on kind and in CI is tracked by #1339."
  assert_eq "before Docker is asked" "" "$(grep '^docker ' "$STUB_LOG" || true)"
  assert_not_contains "and before preflight passes" "$output" "Pre-flight checks passed."

  output="$(run_preflight "$tmp/bin" WITH_CEPH=yes)"
  rc=$?
  assert_eq "WITH_CEPH=yes passes the kind preflight" "0" "$rc"
  assert_not_contains "without the refusal" "$output" "is not supported in kind mode"

  output="$(run_preflight "$tmp/bin" EXTERNAL_CLUSTER=true WITH_CEPH=true)"
  rc=$?
  assert_eq "WITH_CEPH=true passes with the default lab overlay, which has ceph/ and ceph/cluster/" "0" "$rc"
  assert_contains "and reaches the end of preflight" "$output" "Pre-flight checks passed."

  # An overlay with the two kustomizations Steps 3 and 5 apply and ceph/
  # without ceph/cluster/.
  mkdir -p "$tmp/no-cluster/base" "$tmp/no-cluster/infrastructure" "$tmp/no-cluster/ceph/cluster"
  : >"$tmp/no-cluster/base/kustomization.yaml"
  : >"$tmp/no-cluster/infrastructure/kustomization.yaml"
  : >"$tmp/no-cluster/ceph/kustomization.yaml"
  local refusal="ERROR: EXTERNAL_CLUSTER=true WITH_CEPH=true needs $tmp/no-cluster/ceph/kustomization.yaml and $tmp/no-cluster/ceph/cluster/kustomization.yaml, which do not both exist (EXTERNAL_OVERLAY='$tmp/no-cluster')."

  : >"$STUB_LOG"
  output="$(run_preflight "$tmp/bin" EXTERNAL_CLUSTER=true WITH_CEPH=true EXTERNAL_OVERLAY="$tmp/no-cluster")"
  rc=$?
  assert_eq "an overlay without ceph/cluster/kustomization.yaml is refused" "1" "$rc"
  assert_contains "the refusal names both kustomizations" "$output" "$refusal"
  assert_eq "and comes before the cluster is contacted" "" "$(grep '^kubectl ' "$STUB_LOG" || true)"

  output="$(run_preflight "$tmp/bin" EXTERNAL_CLUSTER=true WITH_CEPH=yes EXTERNAL_OVERLAY="$tmp/no-cluster")"
  rc=$?
  assert_eq "WITH_CEPH=yes passes the same overlay" "0" "$rc"
  assert_not_contains "without the refusal" "$output" "WITH_CEPH=true needs"

  # Without ceph/ at all, and with ceph/cluster/ alone.
  rm "$tmp/no-cluster/ceph/kustomization.yaml"
  output="$(run_preflight "$tmp/bin" EXTERNAL_CLUSTER=true WITH_CEPH=true EXTERNAL_OVERLAY="$tmp/no-cluster")"
  rc=$?
  assert_eq "an overlay without either Ceph kustomization is refused" "1" "$rc"
  assert_contains "with the same message" "$output" "WITH_CEPH=true needs"
  : >"$tmp/no-cluster/ceph/cluster/kustomization.yaml"
  output="$(run_preflight "$tmp/bin" EXTERNAL_CLUSTER=true WITH_CEPH=true EXTERNAL_OVERLAY="$tmp/no-cluster")"
  rc=$?
  assert_eq "an overlay with ceph/cluster/ but no ceph/ kustomization is refused" "1" "$rc"

  # The overlay check comes first: an overlay with neither base/ nor ceph/ is
  # reported for its base/.
  mkdir -p "$tmp/empty"
  output="$(run_preflight "$tmp/bin" EXTERNAL_CLUSTER=true WITH_CEPH=true EXTERNAL_OVERLAY="$tmp/empty")"
  rc=$?
  assert_nonzero_exit "an overlay with neither base/ nor ceph/ is refused" "$rc"
  assert_contains "for its missing base/ and infrastructure/" "$output" \
    "EXTERNAL_OVERLAY='$tmp/empty' has no base/ and infrastructure/ kustomization"
  assert_not_contains "and not for its ceph/" "$output" "WITH_CEPH=true needs"
  unset STUB_LOG
}

# ---------------------------------------------------------------------------
# Test 5: wait_for_ceph_cluster
# ---------------------------------------------------------------------------
test_wait_for_ceph_cluster() {
  echo "Test: wait_for_ceph_cluster waits for HEALTH_OK and one ready OSD per entry of the device set"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_kubectl_stub "$tmp/bin"
  export KUBECTL_LOG="$tmp/kubectl.log"
  cephcluster Ready HEALTH_OK >"$tmp/ready.json"
  cephcluster Ready HEALTH_WARN >"$tmp/warn.json"
  cephcluster Progressing HEALTH_WARN >"$tmp/progressing.json"
  osd_deployments 1 1 1 >"$tmp/osds-3.json"
  osd_deployments 1 1 1 1 >"$tmp/osds-4.json"
  osd_deployments 1 1 0 >"$tmp/osds-2.json"
  echo '{}' >"$tmp/empty.json"

  output="$(run_fn "$tmp/bin" wait_for_ceph_cluster \
    KUBECTL_CEPHCLUSTER="$tmp/ready.json" KUBECTL_OSDS="$tmp/osds-3.json")"
  rc=$?
  assert_eq "a Ready, HEALTH_OK cluster with three ready OSDs passes" "0" "$rc"
  assert_contains "and logs the fsid" "$output" \
    "Ceph: phase Ready, HEALTH_OK, 3 of 3 OSDs ready, fsid 5b1f7a2e-0c1d-4f6e-9a3b-2d8c7e6f5a41"

  # Rook keeps an OSD when the device set's count goes down.
  output="$(run_fn "$tmp/bin" wait_for_ceph_cluster CEPH_TIMEOUT=1 \
    KUBECTL_CEPHCLUSTER="$tmp/ready.json" KUBECTL_OSDS="$tmp/osds-4.json")"
  rc=$?
  assert_eq "a Ready, HEALTH_OK cluster with four ready OSDs for a count of three passes" "0" "$rc"
  assert_contains "and logs both numbers" "$output" "Ceph: phase Ready, HEALTH_OK, 4 of 3 OSDs ready,"

  # Progressing on the first read, Ready on the second.
  rm -f "$tmp/bin/cephcluster-reads"
  : >"$KUBECTL_LOG"
  output="$(run_fn "$tmp/bin" wait_for_ceph_cluster CEPH_TIMEOUT=30 \
    KUBECTL_CEPHCLUSTER="$tmp/progressing.json" KUBECTL_CEPHCLUSTER_LATER="$tmp/ready.json" \
    KUBECTL_CEPHCLUSTER_READS=1 KUBECTL_OSDS="$tmp/osds-3.json")"
  rc=$?
  assert_eq "a cluster that turns healthy while it is polled passes" "0" "$rc"
  assert_contains "after logging the state it waited on" "$output" \
    "Ceph: phase 'Progressing', health 'HEALTH_WARN', 3 of 3 OSDs ready."
  assert_eq "on the second read" "2" \
    "$(grep -cxF 'kubectl -n rook-ceph get cephcluster rook-ceph -o json' "$KUBECTL_LOG")"

  output="$(run_fn "$tmp/bin" wait_for_ceph_cluster CEPH_TIMEOUT=1 \
    KUBECTL_CEPHCLUSTER="$tmp/progressing.json" KUBECTL_OSDS="$tmp/osds-2.json")"
  rc=$?
  assert_eq "a Progressing cluster with HEALTH_WARN and two ready OSDs times out" "1" "$rc"
  assert_contains "with the phase, the health and the ready OSDs" "$output" \
    "ERROR: Ceph did not reach HEALTH_OK with 3 OSDs within 1s (phase 'Progressing', health 'HEALTH_WARN', 2 OSDs ready)."
  assert_contains "the health details are printed" "$output" "Reduced data availability: 1 pg inactive"
  assert_contains "and the conditions" "$output" "ClusterProgressing"
  assert_contains "the OSD prepare pod's log is printed" "$output" \
    "log of pod/rook-ceph-osd-prepare-set1-data-0abcd-k8w2m"
  assert_contains "and the operator's" "$output" "log of deploy/rook-ceph-operator"
  assert_not_contains "but not the mon's" "$output" "log of pod/rook-ceph-mon-a"
  assert_not_contains "and no success line" "$output" "Ceph: phase Ready"

  output="$(run_fn "$tmp/bin" wait_for_ceph_cluster CEPH_TIMEOUT=1 \
    KUBECTL_CEPHCLUSTER="$tmp/warn.json" KUBECTL_OSDS="$tmp/osds-3.json")"
  rc=$?
  assert_eq "a Ready cluster with HEALTH_WARN and every OSD ready times out" "1" "$rc"
  assert_contains "naming the health" "$output" "(phase 'Ready', health 'HEALTH_WARN', 3 OSDs ready)."

  output="$(run_fn "$tmp/bin" wait_for_ceph_cluster CEPH_TIMEOUT=1 \
    KUBECTL_CEPHCLUSTER="$tmp/ready.json" KUBECTL_OSDS="$tmp/osds-2.json")"
  rc=$?
  assert_eq "a HEALTH_OK cluster with two of three OSDs ready times out" "1" "$rc"
  assert_contains "naming the ready OSDs" "$output" \
    "ERROR: Ceph did not reach HEALTH_OK with 3 OSDs within 1s (phase 'Ready', health 'HEALTH_OK', 2 OSDs ready)."

  output="$(run_fn "$tmp/bin" wait_for_ceph_cluster CEPH_TIMEOUT=1 \
    KUBECTL_CEPHCLUSTER="$tmp/empty.json" KUBECTL_OSDS="$tmp/osds-3.json")"
  rc=$?
  assert_eq "an empty answer is not ready" "1" "$rc"
  assert_contains "it has no phase, health or count" "$output" \
    "ERROR: Ceph did not reach HEALTH_OK with unknown OSDs within 1s (phase 'unknown', health 'unknown', 3 OSDs ready)."
  assert_not_contains "and no success line" "$output" "Ceph: phase Ready"

  output="$(run_fn "$tmp/bin" wait_for_ceph_cluster CEPH_TIMEOUT=1 KUBECTL_CEPHCLUSTER_RC=1 \
    KUBECTL_CEPHCLUSTER="$tmp/ready.json" KUBECTL_OSDS="$tmp/osds-3.json")"
  rc=$?
  assert_eq "a read that fails is not ready" "1" "$rc"
  assert_contains "and is reported as a timeout" "$output" "ERROR: Ceph did not reach HEALTH_OK"
  assert_not_contains "with no success line" "$output" "Ceph: phase Ready"
  unset KUBECTL_LOG
}

# ---------------------------------------------------------------------------
# Test 6: sync_ceph_client_keys
# ---------------------------------------------------------------------------
test_sync_ceph_client_keys() {
  echo "Test: sync_ceph_client_keys forces and waits for the stores, the PushSecrets and the ExternalSecrets, in that order"

  local tmp output rc calls
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_kubectl_stub "$tmp/bin"
  export KUBECTL_LOG="$tmp/kubectl.log"

  : >"$KUBECTL_LOG"
  output="$(run_fn "$tmp/bin" sync_ceph_client_keys)"
  rc=$?
  assert_eq "the sync passes when every object becomes Ready" "0" "$rc"
  assert_contains "and logs the hand-off" "$output" \
    "Ceph client keys ceph-client-cinder ceph-client-cinder-backup synced into openstack."
  assert_contains "it waits for both ExternalSecrets in openstack" "$output" \
    "Waiting up to 120s for ExternalSecrets to sync in namespace 'openstack': ceph-client-cinder ceph-client-cinder-backup"

  # The calls that change or wait, without the trigger value.
  calls="$(grep -E '^kubectl (annotate|wait) ' "$KUBECTL_LOG" | sed -E 's/=[0-9]+ --overwrite$//')"
  assert_eq "the stores are annotated, then waited on; then the PushSecrets; then the ExternalSecrets are annotated" \
    "$(printf '%s\n' \
      'kubectl annotate secretstore/openbao-ceph-store -n rook-ceph deploy.c5c3.io/reconcile-trigger' \
      'kubectl annotate secretstore/openbao-ceph-store -n openstack deploy.c5c3.io/reconcile-trigger' \
      'kubectl wait secretstore/openbao-ceph-store -n rook-ceph --for=condition=Ready --timeout=300s' \
      'kubectl wait secretstore/openbao-ceph-store -n openstack --for=condition=Ready --timeout=300s' \
      'kubectl annotate pushsecret/ceph-client-cinder -n rook-ceph force-sync' \
      'kubectl annotate pushsecret/ceph-client-cinder-backup -n rook-ceph force-sync' \
      'kubectl wait --for=condition=Ready pushsecret/ceph-client-cinder -n rook-ceph --timeout=120s' \
      'kubectl wait --for=condition=Ready pushsecret/ceph-client-cinder-backup -n rook-ceph --timeout=120s' \
      'kubectl annotate externalsecret/ceph-client-cinder -n openstack force-sync' \
      'kubectl annotate externalsecret/ceph-client-cinder-backup -n openstack force-sync')" \
    "$calls"
  assert_contains "then the ExternalSecrets are read" "$(cat "$KUBECTL_LOG")" \
    "kubectl get externalsecret ceph-client-cinder-backup -n openstack -o json"

  : >"$KUBECTL_LOG"
  output="$(run_fn "$tmp/bin" sync_ceph_client_keys KUBECTL_SECRETSTORE_WAIT_RC=1)"
  rc=$?
  assert_eq "a store that stays unready exits 1" "1" "$rc"
  assert_contains "naming the first store and the roles setup-auth.sh writes" "$output" \
    "ERROR: SecretStore rook-ceph/openbao-ceph-store is not Ready; read 'kubectl -n rook-ceph describe secretstore openbao-ceph-store' (a 403 means setup-auth.sh did not write the push-ceph-keys or read-ceph-keys role)."
  assert_not_contains "before any PushSecret is forced" "$(cat "$KUBECTL_LOG")" "pushsecret"

  : >"$KUBECTL_LOG"
  output="$(run_fn "$tmp/bin" sync_ceph_client_keys KUBECTL_PUSHSECRET_WAIT_RC=1 \
    KUBECTL_PUSHSECRET_MESSAGE='set secret failed: could not write remote ref userKey to target secretstore openbao-ceph-store: Code: 403')"
  rc=$?
  assert_eq "a PushSecret that stays unready exits 1" "1" "$rc"
  assert_contains "with its Ready message" "$output" \
    "ERROR: PushSecret rook-ceph/ceph-client-cinder is not Ready: set secret failed: could not write remote ref userKey to target secretstore openbao-ceph-store: Code: 403"
  assert_not_contains "before any ExternalSecret is forced" "$(cat "$KUBECTL_LOG")" "externalsecret"

  output="$(run_fn "$tmp/bin" sync_ceph_client_keys KUBECTL_PUSHSECRET_WAIT_RC=1)"
  rc=$?
  assert_eq "a PushSecret without a Ready condition exits 1 too" "1" "$rc"
  assert_contains "and says so" "$output" \
    "ERROR: PushSecret rook-ceph/ceph-client-cinder is not Ready: <no Ready condition>"
  unset KUBECTL_LOG
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_knobs
test_gate_count
test_apply_and_wait_order
test_preflight
test_wait_for_ceph_cluster
test_sync_ceph_client_keys

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
