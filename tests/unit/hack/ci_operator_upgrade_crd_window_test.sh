#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-operator-upgrade-crd-window.sh with a stubbed kubectl:
#   - an unchanged CRD set and one CR exit 0, and the stub log holds, in
#     order, the Deployment read, the CR read, the CRD read, the server-side
#     apply, the Established wait, the replace, the dry-run create and the
#     probe patch, with PROBE_PATCH as one argument;
#   - the CRD lines name a schema change (generation 3 -> 4), an unchanged
#     generation, and a CRD that is new in this chart (empty `get -f`, or the
#     bare object kubectl prints for a one-file CRD dir);
#   - check (a): a spec value missing after the apply exits 1 with its path,
#     two changed paths print two FAIL: lines, an extra defaulted field passes,
#     a CR without .spec passes, and false, null, empty-container and array
#     leaves are compared too (spec.a[1].b paths);
#   - ALLOW_SPEC_CHANGES turns a listed change into an ALLOWED: line, and a
#     listed path that kept its value exits 1;
#   - check (b): conflicts are retried up to five attempts, each with a fresh
#     read, an invalid
#     unchanged update exits 1 with the server message, an API server warning
#     is repeated, and a replace that raises metadata.generation exits 1;
#   - check (c): a rejected dry-run create exits 1 and names the -w copy,
#     and DRY_RUN_CREATE=false skips the create with a SKIP: line;
#   - the probe: a rejected PROBE_PATCH exits 1, a PROBE_PATCH that leaves
#     metadata.generation alone exits 2, a patch with spaces stays one
#     argument, a failed first poll read is skipped,
#     a lagging status.observedGeneration or a Ready=False (or a condition
#     reconciled at the old generation) times out with exit 1, and
#     READY_CONDITION picks the condition the probe waits for;
#   - the Deployment guards: a :dev image exits 1 before any apply, a second
#     read with another image or generation exits 1, and a failing read or a
#     Deployment without a manager container exits 2;
#   - a rejected apply exits 1 without a replace, a CRD that is not
#     Established exits 1, and a CR read that fails before the apply exits 2;
#   - two CRS entries run checks (a) to (c) on both and probe only the first;
#   - every invalid input (a missing or blank OPERATOR, CRS or PROBE_PATCH,
#     OPERATOR=Key, a CRS entry without /, PROBE_PATCH='[]',
#     DRY_RUN_CREATE=maybe, PROBE_TIMEOUT=0, an operator without a CRD dir,
#     a PATH without jq) exits 2 with an ::error:: line and no kubectl call.
#
# Usage: bash tests/unit/hack/ci_operator_upgrade_crd_window_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
WINDOW_SH="$PROJECT_ROOT/hack/ci-operator-upgrade-crd-window.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# The script parses every kubectl answer with jq. The stub PATH below is
# tightened to the stub dir plus /usr/bin and /bin, so a jq installed
# elsewhere (Homebrew, ~/.local) is linked into the stub dir.
JQ_BIN="$(command -v jq 2>/dev/null || true)"

# The CRD dir as the script passes it to kubectl: it cds to the repo root.
CRD_DIR="operators/keystone/helm/keystone-operator/crds"
KS_CRD="keystones.keystone.openstack.c5c3.io"
IB_CRD="keystoneidentitybackends.keystone.openstack.c5c3.io"
CR="$KS_CRD/keystone-op-upgrade"
CR2="$IB_CRD/keystone-op-upgrade-ldap"
PATCH='{"spec":{"fernet":{"suspend":true}}}'
SPEC='{"fernet":{"maxActiveKeys":3,"rotationSchedule":"0 0 * * 0"}}'
PROBED_SPEC='{"fernet":{"maxActiveKeys":3,"rotationSchedule":"0 0 * * 0","suspend":true}}'
BASE=(OPERATOR=keystone "CRS=$CR" "PROBE_PATCH=$PATCH" POLL_INTERVAL=0 PROBE_TIMEOUT=1)

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stub <dir>
# kubectl logs every argument in brackets, one invocation per line, to
# $KUBECTL_LOG, so a test can tell one argument from two. It answers from the
# fixtures in $STUB_STATE and keeps markers and counters in $STUB_STATE/run.
# A CR's index <i> is its position in $CRS (for a replace, the position of
# the entry whose name matches the file's metadata.name).
#   get deployment     deploy.json; from the second read on deploy2.json when
#                      it exists; fails when $STUB_DEPLOY_FAIL is set.
#   get -f <dir>       crds-before.json until the apply, then crds-after.json.
#   get <cr>           <i>.before.json until the apply (fails when
#                      $STUB_BEFORE_FAIL is set), <i>.probed.json after the
#                      patch (the first read fails when $STUB_POLL_FAIL_FIRST
#                      is set), <i>.replaced.json after a replace when it
#                      exists, else <i>.after.json.
#   apply              marks the apply; fails when $STUB_APPLY_FAIL is set.
#   wait               fails when $STUB_WAIT_FAIL is set.
#   replace            pops one of ok, warn, conflict, invalid from
#                      $STUB_REPLACE_RESULTS per call (ok when exhausted); ok
#                      and warn print <i>.replaced.json, or the file given.
#   create             copies the file to run/create.<n>.json; fails when
#                      $STUB_CREATE_FAIL is set.
#   patch              fails when $STUB_PATCH_FAIL is set; else marks the
#                      patch and prints <i>.probed.json, or <i>.after.json
#                      when $STUB_PATCH_NOCHANGE is set.
make_stub() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
printf '[%s]' "$@" >>"$KUBECTL_LOG"
echo >>"$KUBECTL_LOG"
state="$STUB_STATE"
run="$STUB_STATE/run"

unexpected() {
  echo "[kubectl-stub] unexpected invocation: $*" >&2
  exit 64
}

# emit <fixture> — prints a fixture file; a missing one is a stub error.
emit() {
  if [ ! -f "$state/$1" ]; then
    echo "[kubectl-stub] missing fixture $1" >&2
    exit 65
  fi
  cat "$state/$1"
}

# bump <counter> — increments run/<counter> and prints the new value.
bump() {
  local n=0
  if [ -f "$run/$1" ]; then
    n="$(cat "$run/$1")"
  fi
  n=$((n + 1))
  echo "$n" >"$run/$1"
  echo "$n"
}

# index_of <entry> — the position of <entry> in $CRS.
index_of() {
  local entries i
  read -r -a entries <<<"$CRS"
  for i in "${!entries[@]}"; do
    if [ "${entries[$i]}" = "$1" ]; then
      echo "$i"
      return 0
    fi
  done
  return 1
}

# index_of_name <name> — the position of the $CRS entry named <name>.
index_of_name() {
  local entries i
  read -r -a entries <<<"$CRS"
  for i in "${!entries[@]}"; do
    if [ "${entries[$i]#*/}" = "$1" ]; then
      echo "$i"
      return 0
    fi
  done
  return 1
}

case "$*" in
  "get deployment "*" -n "*" -o json")
    if [ -n "${STUB_DEPLOY_FAIL:-}" ]; then
      echo "Error from server (NotFound): deployments.apps \"$3\" not found" >&2
      exit 1
    fi
    if [ "$(bump deploy)" -gt 1 ] && [ -f "$state/deploy2.json" ]; then
      emit deploy2.json
    else
      emit deploy.json
    fi
    exit 0
    ;;
  "get -f "*" --ignore-not-found -o json")
    if [ -f "$run/applied" ]; then
      emit crds-after.json
    else
      emit crds-before.json
    fi
    exit 0
    ;;
  "get "*/*" -n "*" -o json")
    i="$(index_of "$2")" || unexpected "$@"
    if [ ! -f "$run/applied" ]; then
      if [ -n "${STUB_BEFORE_FAIL:-}" ]; then
        echo "Error from server (NotFound): ${2%%/*} \"${2#*/}\" not found" >&2
        exit 1
      fi
      emit "$i.before.json"
    elif [ -f "$run/$i.patched" ]; then
      if [ -n "${STUB_POLL_FAIL_FIRST:-}" ] && [ "$(bump "$i.polls")" -eq 1 ]; then
        echo "Unable to connect to the server: stub-poll-timeout" >&2
        exit 1
      fi
      emit "$i.probed.json"
    elif [ -f "$run/$i.replaced" ] && [ -f "$state/$i.replaced.json" ]; then
      emit "$i.replaced.json"
    else
      emit "$i.after.json"
    fi
    exit 0
    ;;
  "apply --server-side --force-conflicts -f "*)
    if [ -n "${STUB_APPLY_FAIL:-}" ]; then
      echo 'The CustomResourceDefinition "keystones.keystone.openstack.c5c3.io" is invalid: spec.versions[0].schema: stub-apply-rejected' >&2
      exit 1
    fi
    touch "$run/applied"
    echo "customresourcedefinition.apiextensions.k8s.io/keystones.keystone.openstack.c5c3.io serverside-applied"
    exit 0
    ;;
  "wait --for condition=Established -f "*" --timeout=60s")
    if [ -n "${STUB_WAIT_FAIL:-}" ]; then
      echo "error: timed out waiting for the condition on customresourcedefinitions/keystones.keystone.openstack.c5c3.io" >&2
      exit 1
    fi
    echo "customresourcedefinition.apiextensions.k8s.io/keystones.keystone.openstack.c5c3.io condition met"
    exit 0
    ;;
  "replace -f "*" -o json")
    n="$(bump replace)"
    read -r -a results <<<"${STUB_REPLACE_RESULTS:-}"
    result="${results[$((n - 1))]:-ok}"
    i="$(index_of_name "$(jq -r '.metadata.name' "$3")")" || unexpected "$@"
    case "$result" in
      ok | warn)
        if [ "$result" = warn ]; then
          echo "Warning: spec.fernet: Invalid value: stub-ratcheted" >&2
        fi
        touch "$run/$i.replaced"
        if [ -f "$state/$i.replaced.json" ]; then
          emit "$i.replaced.json"
        else
          cat "$3"
        fi
        exit 0
        ;;
      conflict)
        echo "Error from server (Conflict): error when replacing \"$3\": Operation cannot be fulfilled on keystones.keystone.openstack.c5c3.io \"keystone-op-upgrade\": the object has been modified; please apply your changes to the latest version and try again" >&2
        exit 1
        ;;
      invalid)
        echo 'The Keystone "keystone-op-upgrade" is invalid: spec.fernet: Invalid value: stub-transition-rule' >&2
        exit 1
        ;;
    esac
    echo "[kubectl-stub] unknown STUB_REPLACE_RESULTS entry: $result" >&2
    exit 64
    ;;
  "create --dry-run=server -f "*)
    n="$(bump create)"
    cp "$4" "$run/create.$n.json"
    name="$(jq -r '.metadata.name' "$4")"
    if [ -n "${STUB_CREATE_FAIL:-}" ]; then
      echo "The Keystone \"$name\" is invalid: spec.fernet.maxActiveKeys: Invalid value: 3: stub-create-rejected" >&2
      exit 1
    fi
    echo "keystone.keystone.openstack.c5c3.io/$name created (server dry run)"
    exit 0
    ;;
  "patch "*" -n "*" --type=merge -p "*" -o json")
    i="$(index_of "$2")" || unexpected "$@"
    if [ -n "${STUB_PATCH_FAIL:-}" ]; then
      echo 'The Keystone "keystone-op-upgrade" is invalid: spec.fernet.suspend: Invalid value: stub-patch-rejected' >&2
      exit 1
    fi
    touch "$run/$i.patched"
    if [ -n "${STUB_PATCH_NOCHANGE:-}" ]; then
      emit "$i.after.json"
    else
      emit "$i.probed.json"
    fi
    exit 0
    ;;
esac
unexpected "$@"
STUB
  chmod +x "$dir/kubectl"
  ln -sf "$JQ_BIN" "$dir/jq"
}

# keystone <generation> <observedGeneration> <ready> <readyGeneration> [<spec>] [<conditions>]
# The Keystone keystone-op-upgrade with a Ready condition of status <ready>
# at <readyGeneration>. <spec> is a JSON object (default: $SPEC); an empty
# <spec> leaves .spec out. <conditions> is a JSON array of further
# conditions.
keystone() {
  jq -cn --argjson g "$1" --argjson og "$2" --arg ready "$3" --argjson rg "$4" \
    --arg spec "${5-$SPEC}" --argjson more "${6:-[]}" '
    {apiVersion: "keystone.openstack.c5c3.io/v1alpha1", kind: "Keystone",
     metadata: {name: "keystone-op-upgrade", namespace: "openstack", generation: $g},
     status: {observedGeneration: $og,
       conditions: ([{type: "Ready", status: $ready, observedGeneration: $rg}] + $more)}}
    + (if $spec == "" then {} else {spec: ($spec | fromjson)} end)'
}

# condition <type> <status> <generation>
# One status condition object.
condition() {
  jq -cn --arg t "$1" --arg s "$2" --argjson g "$3" \
    '{type: $t, status: $s, observedGeneration: $g}'
}

# backend <generation>
# The KeystoneIdentityBackend keystone-op-upgrade-ldap, Ready at <generation>.
backend() {
  jq -cn --argjson g "$1" '
    {apiVersion: "keystone.openstack.c5c3.io/v1alpha1", kind: "KeystoneIdentityBackend",
     metadata: {name: "keystone-op-upgrade-ldap", namespace: "openstack", generation: $g},
     spec: {domainName: "ldap", ldap: {url: "ldap://ldap.example.invalid"}},
     status: {observedGeneration: $g,
       conditions: [{type: "Ready", status: "True", observedGeneration: $g}]}}'
}

# deployment [<image>] [<generation>] [<container>]
# The keystone-operator Deployment with a kube-rbac-proxy sidecar first and
# the operator container (default name: manager) second.
deployment() {
  jq -cn --arg image "${1:-ghcr.io/c5c3/keystone-operator:latest}" \
    --argjson g "${2:-1}" --arg c "${3:-manager}" '
    {apiVersion: "apps/v1", kind: "Deployment",
     metadata: {name: "keystone-operator", namespace: "keystone-system", generation: $g},
     spec: {template: {spec: {containers: [
       {name: "kube-rbac-proxy", image: "quay.io/brancz/kube-rbac-proxy:v0.18.0"},
       {name: $c, image: $image}]}}}}'
}

# crd <name> <generation>
# One CRD, as `kubectl get -f` prints it for a CRD dir that holds one file.
crd() {
  jq -cn --arg n "$1" --argjson g "$2" \
    '{apiVersion: "apiextensions.k8s.io/v1", kind: "CustomResourceDefinition", metadata: {name: $n, generation: $g}}'
}

# crd_list <crd-json>...
# A List as `kubectl get -f` prints several matches.
crd_list() {
  printf '%s\n' "$@" | jq -cs '{apiVersion: "v1", kind: "List", items: .}'
}

# write_fixtures <state-dir>
# The passing default: the released operator image, both CRDs at generation
# 1 before and after the apply, a Keystone at generation 2 that keeps its
# spec, and a probed read at generation 3 with Ready=True.
write_fixtures() {
  local state="$1"
  deployment >"$state/deploy.json"
  crd_list "$(crd "$KS_CRD" 1)" "$(crd "$IB_CRD" 1)" >"$state/crds-before.json"
  cp "$state/crds-before.json" "$state/crds-after.json"
  keystone 2 2 True 2 >"$state/0.before.json"
  cp "$state/0.before.json" "$state/0.after.json"
  keystone 3 3 True 3 "$PROBED_SPEC" >"$state/0.probed.json"
}

# run_window <tmp> [VAR=value | VAR]...
# Runs the script with the stub PATH, a fresh stub log and fresh stub
# markers. Every variable the script and the stub read is unset first; then
# each VAR=value is exported and each bare VAR is unset, in order, so a case
# passes "${BASE[@]}" and overrides or removes entries after it.
run_window() {
  local tmp="$1"
  shift
  rm -rf "$tmp/state/run" "$tmp/kubectl.log"
  mkdir -p "$tmp/state/run"
  (
    PATH="$tmp/bin:/usr/bin:/bin"
    export PATH
    export KUBECTL_LOG="$tmp/kubectl.log"
    export STUB_STATE="$tmp/state"
    unset OPERATOR CRS PROBE_PATCH OPERATOR_NAMESPACE CR_NAMESPACE \
      READY_CONDITION PROBE_TIMEOUT DRY_RUN_CREATE ALLOW_SPEC_CHANGES POLL_INTERVAL
    unset STUB_DEPLOY_FAIL STUB_BEFORE_FAIL STUB_POLL_FAIL_FIRST STUB_APPLY_FAIL \
      STUB_WAIT_FAIL STUB_REPLACE_RESULTS STUB_CREATE_FAIL STUB_PATCH_FAIL \
      STUB_PATCH_NOCHANGE
    for assignment in "$@"; do
      case "$assignment" in
        *=*)
          # shellcheck disable=SC2163 # export the NAME=value word as given
          export "$assignment"
          ;;
        *) unset "$assignment" ;;
      esac
    done
    bash "$WINDOW_SH"
  ) 2>&1
}

# calls <tmp> [<prefix>]
# How many logged kubectl invocations start with <prefix> (default: all).
calls() {
  local log="$1/kubectl.log"
  if [ ! -f "$log" ]; then
    echo 0
    return
  fi
  awk -v p="${2:-[}" 'index($0, p) == 1 {n++} END {print n + 0}' "$log"
}

# calls_after <tmp> <marker-prefix> <prefix>
# How many logged invocations start with <prefix> after the first one that
# starts with <marker-prefix>.
calls_after() {
  awk -v m="$2" -v p="$3" 'seen && index($0, p) == 1 {n++} index($0, m) == 1 {seen = 1} END {print n + 0}' \
    "$1/kubectl.log"
}

# log_in_order <tmp> <line>...
# Prints "in order" when each <line> is logged after the one before it, or
# names the first one that is not. A <line> is a whole logged invocation, or
# a prefix of one when it ends in "...".
log_in_order() {
  local log="$1/kubectl.log" after=0 line n
  shift
  for line in "$@"; do
    n="$(awk -v start="$after" -v want="$line" '
      BEGIN {
        prefix = (substr(want, length(want) - 2) == "...")
        if (prefix) want = substr(want, 1, length(want) - 3)
      }
      NR > start && ((prefix && index($0, want) == 1) || (!prefix && $0 == want)) {print NR; exit}
    ' "$log" 2>/dev/null)"
    if [ -z "$n" ]; then
      echo "no $line after log line $after"
      return
    fi
    after="$n"
  done
  echo "in order"
}

# replaces_after_read <tmp>
# How many logged replace calls directly follow a read of $CR, so a retry
# that resends the stale object is caught.
replaces_after_read() {
  awk -v g="[get][$CR][" 'index($0, "[replace]") == 1 && index(prev, g) == 1 {n++} {prev = $0} END {print n + 0}' \
    "$1/kubectl.log"
}

# lines_starting <output> <prefix>
# How many lines of <output> start with <prefix>.
lines_starting() {
  printf '%s\n' "$1" | awk -v p="$2" 'index($0, p) == 1 {n++} END {print n + 0}'
}

new_tmp() {
  local tmp
  tmp="$(mktemp -d)"
  make_stub "$tmp/bin"
  mkdir -p "$tmp/state"
  write_fixtures "$tmp/state"
  echo "$tmp"
}

# ---------------------------------------------------------------------------
# Test 1: an unchanged CRD set and one CR run every call in order
# ---------------------------------------------------------------------------
test_unchanged_crds_one_cr() {
  echo "Test: an unchanged CRD set and one CR exit 0 with every call in order"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(run_window "$tmp" "${BASE[@]}")"
  rc=$?

  assert_eq "exit code is 0" "0" "$rc"
  assert_eq "the stub log holds the calls in order" "in order" "$(log_in_order "$tmp" \
    "[get][deployment][keystone-operator][-n][keystone-system][-o][json]" \
    "[get][$CR][-n][openstack][-o][json]" \
    "[get][-f][$CRD_DIR][--ignore-not-found]..." \
    "[apply][--server-side][--force-conflicts][-f][$CRD_DIR]" \
    "[wait][--for][condition=Established][-f][$CRD_DIR][--timeout=60s]" \
    "[replace]..." \
    "[create][--dry-run=server]..." \
    "[patch][$CR][-n][openstack][--type=merge][-p][$PATCH][-o][json]")"
  assert_eq "PROBE_PATCH is one bracketed argument" "1" \
    "$(calls "$tmp" "[patch][$CR][-n][openstack][--type=merge][-p][$PATCH][-o][json]")"
  assert_eq "the Deployment is read before and after the window" "2" "$(calls "$tmp" "[get][deployment]")"
  assert_contains "the start guard" "$output" \
    "OK: deployment/keystone-operator runs ghcr.io/c5c3/keystone-operator:latest; the CRD window starts"
  assert_contains "the unchanged CRD line" "$output" \
    "crd/$KS_CRD: generation 1 (unchanged; the window runs the released schema)"
  assert_contains "check (a)" "$output" "OK: $CR keeps every spec value under the new CRDs"
  assert_contains "check (b)" "$output" "OK: $CR: an unchanged update is admitted"
  assert_contains "check (c)" "$output" "OK: $CR: the new CRDs admit its stored spec on create"
  assert_contains "the probe" "$output" \
    "OK: the released controller reconciled generation 3 of $CR to Ready=True under the new CRDs"
  assert_contains "the end guard" "$output" \
    "OK: the CRD window passed for 1 CR(s); deployment/keystone-operator still runs ghcr.io/c5c3/keystone-operator:latest"
  assert_not_contains "no FAIL: line" "$output" "FAIL:"
}

# ---------------------------------------------------------------------------
# Test 2: the CRD lines name a schema change, an unchanged CRD and a new one
# ---------------------------------------------------------------------------
test_crd_generations() {
  echo "Test: the CRD lines compare generations before and after the apply"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  crd_list "$(crd "$KS_CRD" 3)" "$(crd "$IB_CRD" 2)" >"$tmp/state/crds-before.json"
  crd_list "$(crd "$KS_CRD" 4)" "$(crd "$IB_CRD" 2)" >"$tmp/state/crds-after.json"
  output="$(run_window "$tmp" "${BASE[@]}")"
  rc=$?
  assert_eq "3 -> 4: exit code is 0" "0" "$rc"
  assert_contains "a moved generation is a schema change" "$output" \
    "crd/$KS_CRD: generation 3 -> 4 (schema changed)"
  assert_contains "an equal generation runs the released schema" "$output" \
    "crd/$IB_CRD: generation 2 (unchanged; the window runs the released schema)"

  : >"$tmp/state/crds-before.json"
  crd_list "$(crd "$KS_CRD" 1)" "$(crd "$IB_CRD" 1)" >"$tmp/state/crds-after.json"
  output="$(run_window "$tmp" "${BASE[@]}")"
  rc=$?
  assert_eq "empty before: exit code is 0" "0" "$rc"
  assert_contains "empty before: keystones is new" "$output" \
    "crd/$KS_CRD: new in this chart (generation 1)"
  assert_contains "empty before: keystoneidentitybackends is new" "$output" \
    "crd/$IB_CRD: new in this chart (generation 1)"

  crd "$KS_CRD" 3 >"$tmp/state/crds-before.json"
  crd_list "$(crd "$KS_CRD" 4)" "$(crd "$IB_CRD" 1)" >"$tmp/state/crds-after.json"
  output="$(run_window "$tmp" "${BASE[@]}")"
  rc=$?
  assert_eq "a single object before: exit code is 0" "0" "$rc"
  assert_contains "a single object before: its generation is read" "$output" \
    "crd/$KS_CRD: generation 3 -> 4 (schema changed)"
  assert_contains "a single object before: the other CRD is new" "$output" \
    "crd/$IB_CRD: new in this chart (generation 1)"
}

# ---------------------------------------------------------------------------
# Test 3: check (a) fails on a spec value the new CRDs lost
# ---------------------------------------------------------------------------
test_check_a_lost_value() {
  echo "Test: check (a) exits 1 on a lost spec value"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  keystone 2 2 True 2 '{"fernet":{"rotationSchedule":"0 0 * * 0"}}' >"$tmp/state/0.after.json"
  output="$(run_window "$tmp" "${BASE[@]}")"
  rc=$?
  assert_eq "exit code is 1" "1" "$rc"
  assert_contains "the FAIL: line names the path and both values" "$output" \
    "FAIL: $CR spec.fernet.maxActiveKeys was 3 under the released CRDs and is absent under the new ones"
  assert_eq "one FAIL: line" "1" "$(lines_starting "$output" "FAIL:")"
  assert_eq "no replace after a failed check (a)" "0" "$(calls "$tmp" "[replace]")"
}

# ---------------------------------------------------------------------------
# Test 4: two changed paths print two FAIL: lines before the exit
# ---------------------------------------------------------------------------
test_check_a_two_paths() {
  echo "Test: check (a) prints one FAIL: line per changed path"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  keystone 2 2 True 2 '{"fernet":{"rotationSchedule":"0 0 * * 1"}}' >"$tmp/state/0.after.json"
  output="$(run_window "$tmp" "${BASE[@]}")"
  rc=$?
  assert_eq "exit code is 1" "1" "$rc"
  assert_eq "two FAIL: lines" "2" "$(lines_starting "$output" "FAIL:")"
  assert_contains "the lost path" "$output" \
    "FAIL: $CR spec.fernet.maxActiveKeys was 3 under the released CRDs and is absent under the new ones"
  assert_contains "the rewritten path" "$output" \
    "FAIL: $CR spec.fernet.rotationSchedule was \"0 0 * * 0\" under the released CRDs and is \"0 0 * * 1\" under the new ones"
  assert_not_contains "check (a) does not pass" "$output" "keeps every spec value"
  assert_eq "no replace after a failed check (a)" "0" "$(calls "$tmp" "[replace]")"
}

# ---------------------------------------------------------------------------
# Test 5: an extra defaulted field passes check (a)
# ---------------------------------------------------------------------------
test_check_a_defaulted_field() {
  echo "Test: check (a) accepts a new default beside every old value"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  keystone 2 2 True 2 \
    '{"fernet":{"maxActiveKeys":3,"rotationSchedule":"0 0 * * 0","keyRepository":"secret"},"tokenProvider":"fernet"}' \
    >"$tmp/state/0.after.json"
  output="$(run_window "$tmp" "${BASE[@]}")"
  rc=$?
  assert_eq "exit code is 0" "0" "$rc"
  assert_contains "check (a) passes" "$output" "OK: $CR keeps every spec value under the new CRDs"
  assert_not_contains "no FAIL: line" "$output" "FAIL:"
}

# ---------------------------------------------------------------------------
# Test 6: a CR without .spec passes check (a)
# ---------------------------------------------------------------------------
test_check_a_no_spec() {
  echo "Test: check (a) accepts a CR without .spec"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  keystone 2 2 True 2 "" >"$tmp/state/0.before.json"
  keystone 2 2 True 2 "" >"$tmp/state/0.after.json"
  output="$(run_window "$tmp" "${BASE[@]}")"
  rc=$?
  assert_eq "no spec before or after: exit code is 0" "0" "$rc"
  assert_contains "no spec before or after: check (a) passes" "$output" \
    "OK: $CR keeps every spec value under the new CRDs"
  assert_eq "no spec before or after: the dry-run copy has no spec" "false" \
    "$(jq 'has("spec")' "$tmp/state/run/create.1.json" 2>&1)"

  keystone 2 2 True 2 '{"replicas":1}' >"$tmp/state/0.after.json"
  output="$(run_window "$tmp" "${BASE[@]}")"
  rc=$?
  assert_eq "a defaulted spec after: exit code is 0" "0" "$rc"
  assert_contains "a defaulted spec after: check (a) passes" "$output" \
    "OK: $CR keeps every spec value under the new CRDs"
}

# ---------------------------------------------------------------------------
# Test 6b: check (a) compares false, null, empty-container and array leaves
# ---------------------------------------------------------------------------
test_check_a_leaves() {
  echo "Test: check (a) compares false, null, empty-container and array leaves"
  local tmp output rc before
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  before='{"fernet":{"maxActiveKeys":3,"suspend":false},"tls":null,"extraConfig":{},"tags":[],"endpoints":[{"name":"a","port":1},{"name":"b","port":2}]}'
  keystone 2 2 True 2 "$before" >"$tmp/state/0.before.json"
  cp "$tmp/state/0.before.json" "$tmp/state/0.after.json"
  output="$(run_window "$tmp" "${BASE[@]}")"
  rc=$?
  assert_eq "every leaf kept: exit code is 0" "0" "$rc"
  assert_contains "every leaf kept: check (a) passes" "$output" \
    "OK: $CR keeps every spec value under the new CRDs"

  keystone 2 2 True 2 '{"fernet":{"maxActiveKeys":3},"tls":null,"extraConfig":{},"tags":[],"endpoints":[{"name":"a","port":1},{"name":"b","port":2}]}' \
    >"$tmp/state/0.after.json"
  output="$(run_window "$tmp" "${BASE[@]}")"
  rc=$?
  assert_eq "a pruned false: exit code is 1" "1" "$rc"
  assert_contains "a pruned false: the FAIL: line" "$output" \
    "FAIL: $CR spec.fernet.suspend was false under the released CRDs and is absent under the new ones"

  keystone 2 2 True 2 '{"fernet":{"maxActiveKeys":3,"suspend":false},"tags":[],"endpoints":[{"name":"a","port":1},{"name":"b","port":2}]}' \
    >"$tmp/state/0.after.json"
  output="$(run_window "$tmp" "${BASE[@]}")"
  rc=$?
  assert_eq "a pruned null and empty object: exit code is 1" "1" "$rc"
  assert_contains "a pruned null: the FAIL: line" "$output" \
    "FAIL: $CR spec.tls was null under the released CRDs and is absent under the new ones"
  assert_contains "a pruned empty object: the FAIL: line" "$output" \
    "FAIL: $CR spec.extraConfig was {} under the released CRDs and is absent under the new ones"
  assert_eq "a pruned null and empty object: two FAIL: lines" "2" "$(lines_starting "$output" "FAIL:")"

  keystone 2 2 True 2 '{"fernet":{"maxActiveKeys":3,"suspend":false},"tls":null,"extraConfig":{},"tags":[],"endpoints":[{"name":"a","port":1}]}' \
    >"$tmp/state/0.after.json"
  output="$(run_window "$tmp" "${BASE[@]}")"
  rc=$?
  assert_eq "a dropped array element: exit code is 1" "1" "$rc"
  assert_contains "a dropped array element: its name" "$output" \
    "FAIL: $CR spec.endpoints[1].name was \"b\" under the released CRDs and is absent under the new ones"
  assert_contains "a dropped array element: its port" "$output" \
    "FAIL: $CR spec.endpoints[1].port was 2 under the released CRDs and is absent under the new ones"

  output="$(run_window "$tmp" "${BASE[@]}" "ALLOW_SPEC_CHANGES=spec.endpoints[1].name spec.endpoints[1].port")"
  rc=$?
  assert_eq "an allowed array element: exit code is 0" "0" "$rc"
  assert_contains "an allowed array element: the ALLOWED: line" "$output" \
    "ALLOWED: $CR spec.endpoints[1].name changed (\"b\" -> absent), listed in ALLOW_SPEC_CHANGES"
  assert_not_contains "an allowed array element: no FAIL: line" "$output" "FAIL:"

  keystone 2 2 True 2 '{"fernet":{"maxActiveKeys":3,"suspend":true},"tls":null,"extraConfig":{},"tags":[],"endpoints":[{"name":"a","port":1},{"name":"b","port":2}]}' \
    >"$tmp/state/0.after.json"
  output="$(run_window "$tmp" "${BASE[@]}")"
  rc=$?
  assert_eq "a rewritten false: exit code is 1" "1" "$rc"
  assert_contains "a rewritten false: the FAIL: line" "$output" \
    "FAIL: $CR spec.fernet.suspend was false under the released CRDs and is true under the new ones"
}

# ---------------------------------------------------------------------------
# Test 7: ALLOW_SPEC_CHANGES allows a listed change and rejects a stale entry
# ---------------------------------------------------------------------------
test_allow_spec_changes() {
  echo "Test: ALLOW_SPEC_CHANGES allows a listed change and fails a stale entry"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  keystone 2 2 True 2 '{"fernet":{"rotationSchedule":"0 0 * * 0"}}' >"$tmp/state/0.after.json"
  output="$(run_window "$tmp" "${BASE[@]}" ALLOW_SPEC_CHANGES=spec.fernet.maxActiveKeys)"
  rc=$?
  assert_eq "a listed change: exit code is 0" "0" "$rc"
  assert_contains "a listed change: the ALLOWED: line" "$output" \
    "ALLOWED: $CR spec.fernet.maxActiveKeys changed (3 -> absent), listed in ALLOW_SPEC_CHANGES"
  assert_not_contains "a listed change: no FAIL: line" "$output" "FAIL:"

  cp "$tmp/state/0.before.json" "$tmp/state/0.after.json"
  output="$(run_window "$tmp" "${BASE[@]}" ALLOW_SPEC_CHANGES=spec.fernet.maxActiveKeys)"
  rc=$?
  assert_eq "a stale entry: exit code is 1" "1" "$rc"
  assert_contains "a stale entry: the FAIL: line" "$output" \
    "FAIL: ALLOW_SPEC_CHANGES names spec.fernet.maxActiveKeys, which kept its value; remove the entry"
  assert_not_contains "a stale entry: no ALLOWED: line" "$output" "ALLOWED:"
  assert_eq "a stale entry: no probe patch" "0" "$(calls "$tmp" "[patch]")"
}

# ---------------------------------------------------------------------------
# Test 8: conflicts are retried, five of them fail
# ---------------------------------------------------------------------------
test_replace_conflicts() {
  echo "Test: an unchanged update retries a conflict up to five attempts"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(run_window "$tmp" "${BASE[@]}" "STUB_REPLACE_RESULTS=conflict conflict ok")"
  rc=$?
  assert_eq "two conflicts: exit code is 0" "0" "$rc"
  assert_eq "two conflicts: three replace calls" "3" "$(calls "$tmp" "[replace]")"
  assert_eq "two conflicts: each replace follows a fresh read" "3" "$(replaces_after_read "$tmp")"
  assert_contains "two conflicts: check (b) passes" "$output" "OK: $CR: an unchanged update is admitted"

  output="$(run_window "$tmp" "${BASE[@]}" \
    "STUB_REPLACE_RESULTS=conflict conflict conflict conflict conflict")"
  rc=$?
  assert_eq "five conflicts: exit code is 1" "1" "$rc"
  assert_contains "five conflicts: the FAIL: line" "$output" \
    "FAIL: $CR: the unchanged update kept conflicting (5 attempts)"
  assert_eq "five conflicts: five replace calls" "5" "$(calls "$tmp" "[replace]")"
  assert_eq "five conflicts: each replace follows a fresh read" "5" "$(replaces_after_read "$tmp")"
  assert_eq "five conflicts: no create" "0" "$(calls "$tmp" "[create]")"
}

# ---------------------------------------------------------------------------
# Test 9: an invalid unchanged update fails, a warning is repeated
# ---------------------------------------------------------------------------
test_replace_invalid_and_warn() {
  echo "Test: an invalid unchanged update exits 1, a warning passes"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(run_window "$tmp" "${BASE[@]}" STUB_REPLACE_RESULTS=invalid)"
  rc=$?
  assert_eq "invalid: exit code is 1" "1" "$rc"
  assert_contains "invalid: the FAIL: line carries the server message" "$output" \
    "FAIL: $CR: the new CRDs or the running webhooks reject an unchanged update: The Keystone \"keystone-op-upgrade\" is invalid: spec.fernet: Invalid value: stub-transition-rule"
  assert_eq "invalid: no retry" "1" "$(calls "$tmp" "[replace]")"

  output="$(run_window "$tmp" "${BASE[@]}" STUB_REPLACE_RESULTS=warn)"
  rc=$?
  assert_eq "warn: exit code is 0" "0" "$rc"
  assert_contains "warn: the warning is repeated" "$output" \
    "API server warning on $CR: Warning: spec.fernet: Invalid value: stub-ratcheted"
  assert_contains "warn: check (b) passes" "$output" "OK: $CR: an unchanged update is admitted"
}

# ---------------------------------------------------------------------------
# Test 10: a replace that raises metadata.generation fails
# ---------------------------------------------------------------------------
test_replace_generation() {
  echo "Test: an unchanged update that raises metadata.generation exits 1"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  keystone 3 2 True 2 >"$tmp/state/0.replaced.json"
  output="$(run_window "$tmp" "${BASE[@]}")"
  rc=$?
  assert_eq "exit code is 1" "1" "$rc"
  assert_contains "the FAIL: line names both generations" "$output" \
    "FAIL: $CR: the unchanged update changed metadata.generation (2 -> 3); the running mutating webhook rewrote the spec"
  assert_eq "no create after a failed check (b)" "0" "$(calls "$tmp" "[create]")"
}

# ---------------------------------------------------------------------------
# Test 11: check (c) fails on a rejected create, DRY_RUN_CREATE=false skips it
# ---------------------------------------------------------------------------
test_dry_run_create() {
  echo "Test: a rejected dry-run create exits 1, DRY_RUN_CREATE=false skips it"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(run_window "$tmp" "${BASE[@]}" STUB_CREATE_FAIL=1)"
  rc=$?
  assert_eq "rejected: exit code is 1" "1" "$rc"
  assert_contains "rejected: the FAIL: line names the copy and the server message" "$output" \
    "FAIL: $CR: a create of its stored spec (dry run, as keystone-op-upgrade-w) is rejected: The Keystone \"keystone-op-upgrade-w\" is invalid: spec.fernet.maxActiveKeys: Invalid value: 3: stub-create-rejected"
  assert_eq "rejected: the create is a server-side dry run" "1" \
    "$(calls "$tmp" "[create][--dry-run=server][-f]")"
  assert_eq "rejected: the copy is named keystone-op-upgrade-w" "keystone-op-upgrade-w" \
    "$(jq -r '.metadata.name' "$tmp/state/run/create.1.json" 2>&1)"
  assert_eq "rejected: the copy carries the stored spec" "$SPEC" \
    "$(jq -c '.spec' "$tmp/state/run/create.1.json" 2>&1)"
  assert_eq "rejected: no probe patch" "0" "$(calls "$tmp" "[patch]")"

  output="$(run_window "$tmp" "${BASE[@]}" DRY_RUN_CREATE=false)"
  rc=$?
  assert_eq "disabled: exit code is 0" "0" "$rc"
  assert_eq "disabled: no create" "0" "$(calls "$tmp" "[create]")"
  assert_contains "disabled: the SKIP: line" "$output" \
    "SKIP: $CR: dry-run create disabled (DRY_RUN_CREATE=false)"
}

# ---------------------------------------------------------------------------
# Test 12: a rejected PROBE_PATCH exits 1, one that changes nothing exits 2
# ---------------------------------------------------------------------------
test_probe_patch() {
  echo "Test: a rejected PROBE_PATCH exits 1, an ineffective one exits 2"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(run_window "$tmp" "${BASE[@]}" STUB_PATCH_FAIL=1)"
  rc=$?
  assert_eq "rejected: exit code is 1" "1" "$rc"
  assert_contains "rejected: the FAIL: line carries the server message" "$output" \
    "FAIL: $CR: PROBE_PATCH was rejected under the new CRDs: The Keystone \"keystone-op-upgrade\" is invalid: spec.fernet.suspend: Invalid value: stub-patch-rejected"

  keystone 2 2 True 2 "$PROBED_SPEC" >"$tmp/state/0.probed.json"
  output="$(run_window "$tmp" "${BASE[@]}")"
  rc=$?
  assert_eq "an unchanged generation: exit code is 2" "2" "$rc"
  assert_contains "an unchanged generation: the ::error:: line" "$output" \
    "::error::PROBE_PATCH did not advance metadata.generation of $CR (2 -> 2)"
  assert_eq "an unchanged generation: no poll read" "0" \
    "$(calls_after "$tmp" "[patch]" "[get][$CR]")"

  write_fixtures "$tmp/state"
  output="$(run_window "$tmp" "${BASE[@]}" STUB_PATCH_NOCHANGE=1)"
  rc=$?
  assert_eq "a patch that changes nothing: exit code is 2" "2" "$rc"
  assert_contains "a patch that changes nothing: the ::error:: line" "$output" \
    "::error::PROBE_PATCH did not advance metadata.generation of $CR (2 -> 2)"

  # A patch with spaces would turn into several arguments if the script let
  # the shell split it.
  local spaced='{"spec": {"fernet": {"suspend": true}}}'
  output="$(run_window "$tmp" "${BASE[@]}" "PROBE_PATCH=$spaced")"
  rc=$?
  assert_eq "a patch with spaces: exit code is 0" "0" "$rc"
  assert_eq "a patch with spaces: one bracketed argument" "1" \
    "$(calls "$tmp" "[patch][$CR][-n][openstack][--type=merge][-p][$spaced][-o][json]")"
}

# ---------------------------------------------------------------------------
# Test 13: a failed first poll read is skipped
# ---------------------------------------------------------------------------
test_poll_fail_first() {
  echo "Test: a failed first poll read is no observation"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(run_window "$tmp" "${BASE[@]}" STUB_POLL_FAIL_FIRST=1 PROBE_TIMEOUT=5)"
  rc=$?
  assert_eq "exit code is 0" "0" "$rc"
  assert_contains "the probe passes on the next read" "$output" \
    "OK: the released controller reconciled generation 3 of $CR to Ready=True under the new CRDs"
  assert_eq "two poll reads, the failed one and the passing one" "2" \
    "$(calls_after "$tmp" "[patch]" "[get][$CR]")"
  assert_not_contains "the failed read prints nothing" "$output" "stub-poll-timeout"
}

# ---------------------------------------------------------------------------
# Test 14: the probe times out on a lagging or failing status
# ---------------------------------------------------------------------------
test_probe_timeout() {
  echo "Test: the probe exits 1 until READY_CONDITION is True at the new generation"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  keystone 3 2 True 3 "$PROBED_SPEC" >"$tmp/state/0.probed.json"
  output="$(run_window "$tmp" "${BASE[@]}" PROBE_TIMEOUT=1)"
  rc=$?
  assert_eq "a lagging observedGeneration: exit code is 1" "1" "$rc"
  assert_contains "a lagging observedGeneration: the FAIL: line" "$output" \
    "FAIL: the released controller did not reconcile generation 3 of $CR within 1s under the new CRDs (status.observedGeneration=2, Ready=True at generation 3)"
  assert_eq "a lagging observedGeneration: no Deployment read after the probe" "1" \
    "$(calls "$tmp" "[get][deployment]")"

  keystone 3 3 False 3 "$PROBED_SPEC" >"$tmp/state/0.probed.json"
  output="$(run_window "$tmp" "${BASE[@]}" PROBE_TIMEOUT=1)"
  rc=$?
  assert_eq "Ready=False: exit code is 1" "1" "$rc"
  assert_contains "Ready=False: the FAIL: line" "$output" \
    "FAIL: the released controller did not reconcile generation 3 of $CR within 1s under the new CRDs (status.observedGeneration=3, Ready=False at generation 3)"

  keystone 3 3 True 2 "$PROBED_SPEC" >"$tmp/state/0.probed.json"
  output="$(run_window "$tmp" "${BASE[@]}" PROBE_TIMEOUT=1)"
  rc=$?
  assert_eq "Ready=True at the old generation: exit code is 1" "1" "$rc"
  assert_contains "Ready=True at the old generation: the FAIL: line" "$output" \
    "did not reconcile generation 3 of $CR within 1s under the new CRDs (status.observedGeneration=3, Ready=True at generation 2)"

  keystone 3 3 False 3 "$PROBED_SPEC" "[$(condition InfrastructureReady True 3)]" \
    >"$tmp/state/0.probed.json"
  output="$(run_window "$tmp" "${BASE[@]}" READY_CONDITION=InfrastructureReady PROBE_TIMEOUT=1)"
  rc=$?
  assert_eq "InfrastructureReady=True, Ready=False: exit code is 0" "0" "$rc"
  assert_contains "InfrastructureReady=True, Ready=False: the probe names the condition" "$output" \
    "OK: the released controller reconciled generation 3 of $CR to InfrastructureReady=True under the new CRDs"

  keystone 3 3 True 3 "$PROBED_SPEC" >"$tmp/state/0.probed.json"
  output="$(run_window "$tmp" "${BASE[@]}" READY_CONDITION=InfrastructureReady PROBE_TIMEOUT=1)"
  rc=$?
  assert_eq "Ready=True without InfrastructureReady: exit code is 1" "1" "$rc"
  assert_contains "Ready=True without InfrastructureReady: the FAIL: line" "$output" \
    "did not reconcile generation 3 of $CR within 1s under the new CRDs (status.observedGeneration=3, InfrastructureReady=none at generation none)"
}

# ---------------------------------------------------------------------------
# Test 15: the Deployment guards before and after the window
# ---------------------------------------------------------------------------
test_deployment_guards() {
  echo "Test: a :dev image or a rolled Deployment exits 1"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  deployment ghcr.io/c5c3/keystone-operator:dev >"$tmp/state/deploy.json"
  output="$(run_window "$tmp" "${BASE[@]}")"
  rc=$?
  assert_eq ":dev: exit code is 1" "1" "$rc"
  assert_contains ":dev: the FAIL: line" "$output" \
    "FAIL: deployment/keystone-operator already runs ghcr.io/c5c3/keystone-operator:dev; the CRD window must run before the helm upgrade"
  assert_eq ":dev: no apply" "0" "$(calls "$tmp" "[apply]")"

  deployment >"$tmp/state/deploy.json"
  deployment ghcr.io/c5c3/keystone-operator:v0.4.0 >"$tmp/state/deploy2.json"
  output="$(run_window "$tmp" "${BASE[@]}")"
  rc=$?
  assert_eq "another image after the window: exit code is 1" "1" "$rc"
  assert_contains "another image after the window: the FAIL: line" "$output" \
    "FAIL: deployment/keystone-operator changed during the CRD window (image ghcr.io/c5c3/keystone-operator:latest -> ghcr.io/c5c3/keystone-operator:v0.4.0, generation 1 -> 1)"
  assert_eq "another image after the window: the probe ran first" "1" "$(calls "$tmp" "[patch]")"

  deployment "" 2 >"$tmp/state/deploy2.json"
  output="$(run_window "$tmp" "${BASE[@]}")"
  rc=$?
  assert_eq "another generation after the window: exit code is 1" "1" "$rc"
  assert_contains "another generation after the window: the FAIL: line" "$output" \
    "changed during the CRD window (image ghcr.io/c5c3/keystone-operator:latest -> ghcr.io/c5c3/keystone-operator:latest, generation 1 -> 2)"
}

# ---------------------------------------------------------------------------
# Test 16: an unreadable Deployment or one without a manager exits 2
# ---------------------------------------------------------------------------
test_deployment_unreadable() {
  echo "Test: a failing Deployment read or a missing manager container exits 2"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(run_window "$tmp" "${BASE[@]}" STUB_DEPLOY_FAIL=1)"
  rc=$?
  assert_eq "a failing read: exit code is 2" "2" "$rc"
  assert_contains "a failing read: the ::error:: line carries kubectl's stderr" "$output" \
    "::error::cannot read deployment/keystone-operator in keystone-system: Error from server (NotFound): deployments.apps \"keystone-operator\" not found"
  assert_eq "a failing read: no apply" "0" "$(calls "$tmp" "[apply]")"

  deployment "" 1 operator >"$tmp/state/deploy.json"
  output="$(run_window "$tmp" "${BASE[@]}")"
  rc=$?
  assert_eq "no manager container: exit code is 2" "2" "$rc"
  assert_contains "no manager container: the ::error:: line" "$output" \
    "::error::deployment/keystone-operator has no manager container"
  assert_eq "no manager container: no apply" "0" "$(calls "$tmp" "[apply]")"
}

# ---------------------------------------------------------------------------
# Test 17: a rejected apply or a CRD that is not Established exits 1
# ---------------------------------------------------------------------------
test_apply_and_wait() {
  echo "Test: a rejected apply or a missing Established exits 1"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(run_window "$tmp" "${BASE[@]}" STUB_APPLY_FAIL=1)"
  rc=$?
  assert_eq "rejected apply: exit code is 1" "1" "$rc"
  assert_contains "rejected apply: the FAIL: line carries the server message" "$output" \
    "FAIL: the API server rejected the new CRDs: The CustomResourceDefinition \"keystones.keystone.openstack.c5c3.io\" is invalid: spec.versions[0].schema: stub-apply-rejected"
  assert_eq "rejected apply: no CR replace" "0" "$(calls "$tmp" "[replace]")"
  assert_eq "rejected apply: no wait" "0" "$(calls "$tmp" "[wait]")"

  output="$(run_window "$tmp" "${BASE[@]}" STUB_WAIT_FAIL=1)"
  rc=$?
  assert_eq "not Established: exit code is 1" "1" "$rc"
  assert_contains "not Established: the FAIL: line" "$output" \
    "FAIL: the new CRDs are not Established within 60s: error: timed out waiting for the condition"
  assert_eq "not Established: no CR replace" "0" "$(calls "$tmp" "[replace]")"
}

# ---------------------------------------------------------------------------
# Test 18: a CR read that fails before the apply exits 2
# ---------------------------------------------------------------------------
test_before_read_fails() {
  echo "Test: a CR that cannot be read before the apply exits 2"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(run_window "$tmp" "${BASE[@]}" STUB_BEFORE_FAIL=1)"
  rc=$?
  assert_eq "exit code is 2" "2" "$rc"
  assert_contains "the ::error:: line carries kubectl's stderr" "$output" \
    "::error::cannot read $CR before the CRD apply: Error from server (NotFound): $KS_CRD \"keystone-op-upgrade\" not found"
  assert_eq "no apply" "0" "$(calls "$tmp" "[apply]")"
}

# ---------------------------------------------------------------------------
# Test 19: two CRS entries are both checked, only the first is probed
# ---------------------------------------------------------------------------
test_two_crs() {
  echo "Test: two CRS entries run checks (a) to (c) on both and probe the first"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  backend 1 >"$tmp/state/1.before.json"
  cp "$tmp/state/1.before.json" "$tmp/state/1.after.json"
  output="$(run_window "$tmp" "${BASE[@]}" "CRS=$CR $CR2")"
  rc=$?
  assert_eq "exit code is 0" "0" "$rc"
  local cr
  for cr in "$CR" "$CR2"; do
    assert_contains "check (a) for $cr" "$output" "OK: $cr keeps every spec value under the new CRDs"
    assert_contains "check (b) for $cr" "$output" "OK: $cr: an unchanged update is admitted"
    assert_contains "check (c) for $cr" "$output" "OK: $cr: the new CRDs admit its stored spec on create"
  done
  assert_eq "two replace calls" "2" "$(calls "$tmp" "[replace]")"
  assert_eq "two dry-run creates" "2" "$(calls "$tmp" "[create][--dry-run=server]")"
  assert_eq "the second copy is named keystone-op-upgrade-ldap-w" "keystone-op-upgrade-ldap-w" \
    "$(jq -r '.metadata.name' "$tmp/state/run/create.2.json" 2>&1)"
  assert_eq "one patch" "1" "$(calls "$tmp" "[patch]")"
  assert_eq "the patch names the first CR" "1" "$(calls "$tmp" "[patch][$CR][-n][openstack]")"
  assert_eq "the second CR is not polled" "0" "$(calls_after "$tmp" "[patch]" "[get][$CR2]")"
  assert_contains "the probe names the first CR" "$output" \
    "OK: the released controller reconciled generation 3 of $CR to Ready=True under the new CRDs"
  assert_contains "the end line counts both CRs" "$output" "OK: the CRD window passed for 2 CR(s)"
}

# ---------------------------------------------------------------------------
# Test 20: invalid input exits 2 before any kubectl call
# ---------------------------------------------------------------------------

# expect_input_error <tmp> <description> <message> [VAR=value | VAR]...
# Runs the script with BASE and the given changes and expects exit 2, an
# ::error::<message> line and an empty stub log.
expect_input_error() {
  local tmp="$1" description="$2" message="$3" output rc
  shift 3
  output="$(run_window "$tmp" "${BASE[@]}" "$@")"
  rc=$?
  assert_eq "$description: exit code is 2" "2" "$rc"
  assert_contains "$description: the ::error:: line" "$output" "::error::$message"
  assert_eq "$description: no kubectl call" "0" "$(calls "$tmp")"
}

test_invalid_inputs() {
  echo "Test: invalid input exits 2 with an ::error:: line and no kubectl call"
  local tmp
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  expect_input_error "$tmp" "missing OPERATOR" "OPERATOR must be set (e.g. keystone)" OPERATOR
  expect_input_error "$tmp" "blank OPERATOR" "OPERATOR must be set (e.g. keystone)" $'OPERATOR= \t '
  expect_input_error "$tmp" "missing CRS" "CRS must be set" CRS
  expect_input_error "$tmp" "blank CRS" "CRS must be set" $'CRS=  \n '
  expect_input_error "$tmp" "missing PROBE_PATCH" "PROBE_PATCH must be set" PROBE_PATCH
  expect_input_error "$tmp" "blank PROBE_PATCH" "PROBE_PATCH must be set" "PROBE_PATCH=   "
  expect_input_error "$tmp" "OPERATOR=Key" "OPERATOR 'Key' must match ^[a-z0-9]+\$" OPERATOR=Key
  expect_input_error "$tmp" "a CRS entry without /" \
    "CRS entry 'keystone-op-upgrade' is not <resource>/<name>" "CRS=$CR keystone-op-upgrade"
  expect_input_error "$tmp" "PROBE_PATCH='[]'" \
    "PROBE_PATCH must be a JSON object (a merge patch), got: []" "PROBE_PATCH=[]"
  expect_input_error "$tmp" "DRY_RUN_CREATE=maybe" \
    "DRY_RUN_CREATE must be true or false, got 'maybe'" DRY_RUN_CREATE=maybe
  expect_input_error "$tmp" "PROBE_TIMEOUT=0" \
    "PROBE_TIMEOUT must be a positive integer, got '0'" PROBE_TIMEOUT=0
  expect_input_error "$tmp" "OPERATOR=nosuch" \
    "no CRDs under operators/nosuch/helm/nosuch-operator/crds" OPERATOR=nosuch

  # A PATH with bash, dirname and the kubectl stub, but no jq.
  mkdir -p "$tmp/nojq"
  ln -s "$(command -v bash)" "$tmp/nojq/bash"
  ln -s "$(command -v dirname)" "$tmp/nojq/dirname"
  ln -s "$tmp/bin/kubectl" "$tmp/nojq/kubectl"
  expect_input_error "$tmp" "a PATH without jq" "jq is required" "PATH=$tmp/nojq"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
if [ -z "$JQ_BIN" ]; then
  echo "SKIP: jq not installed (every case needs it)"
  SKIP=$((SKIP + 1))
else
  test_unchanged_crds_one_cr
  test_crd_generations
  test_check_a_lost_value
  test_check_a_two_paths
  test_check_a_defaulted_field
  test_check_a_no_spec
  test_check_a_leaves
  test_allow_spec_changes
  test_replace_conflicts
  test_replace_invalid_and_warn
  test_replace_generation
  test_dry_run_create
  test_probe_patch
  test_poll_fail_first
  test_probe_timeout
  test_deployment_guards
  test_deployment_unreadable
  test_apply_and_wait
  test_before_read_fails
  test_two_crs
  test_invalid_inputs
fi

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
