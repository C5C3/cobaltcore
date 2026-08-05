#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh `check_relocated_infrastructure` refuses to run
# against a cluster that still holds infrastructure from before the
# shared-services relocation.
#
# The relocation moves namespace-scoped StatefulSet storage, so the data does
# NOT come along and nothing prunes what stays behind — `kubectl apply -k` does
# not prune and the FluxInstance declares no spec.sync. Continuing would leave a
# second, unsealed OpenBao serving every historical secret (root token and unseal
# keys in plaintext) that nobody watches any more. The guard converts that silent
# split-brain into a hard stop with the deletion command.
#
# Usage: bash tests/unit/hack/deploy_infra_relocation_guard_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_kubectl_stub <dir> <present>...
# Installs a kubectl stub in <dir> that succeeds only for the `get` lookups
# whose joined arguments appear in <present>. Every other lookup reproduces how
# the real kubectl reports a definite miss — a NotFound error on stderr — which
# is what the guard distinguishes from "I could not ask".
make_kubectl_stub() {
  local dir="$1"
  shift
  mkdir -p "$dir"
  printf '%s\n' "$@" >"$dir/present.txt"
  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
# The guard only ever runs read-only `kubectl get` lookups.
args="$*"
while IFS= read -r want; do
  [ -n "$want" ] || continue
  [ "$args" = "$want" ] && exit 0
done <"$(dirname "$0")/present.txt"
echo "Error from server (NotFound): ${args} not found" >&2
exit 1
STUB
  chmod +x "$dir/kubectl"
}

# make_unreachable_kubectl_stub <dir>
# Installs a kubectl stub that fails the way an API server which is not
# answering does — the state deploy-infra is in whenever cap_node_nofile has
# just restarted containerd (and with it the static kube-apiserver pod) and the
# best-effort wait_for_node_ready gave up. This is NOT evidence of absence.
make_unreachable_kubectl_stub() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
echo "The connection to the server 127.0.0.1:6443 was refused - did you specify the right host or port?" >&2
exit 1
STUB
  chmod +x "$dir/kubectl"
}

# run_guard <stub_dir> [allow_pre_relocation]
# Sources deploy-infra.sh in a subshell with the kubectl stub prepended to PATH
# and invokes check_relocated_infrastructure. Echoes combined stdout/stderr;
# returns the exit status of the guard.
run_guard() {
  local stub_dir="$1"
  local allow="${2:-false}"
  (
    PATH="$stub_dir:$PATH"
    ALLOW_PRE_RELOCATION="$allow"
    export PATH ALLOW_PRE_RELOCATION
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    check_relocated_infrastructure
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test 1: a retired openbao-system namespace aborts the run
# ---------------------------------------------------------------------------
test_retired_openbao_namespace_aborts() {
  echo "Test: check_relocated_infrastructure aborts when openbao-system still exists"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_kubectl_stub "$tmp" "get namespace openbao-system"

  local output exit_code
  output="$(run_guard "$tmp")"
  exit_code=$?

  assert_nonzero_exit "the guard exits non-zero on a pre-relocation cluster" "$exit_code"
  assert_contains "the error names the retired namespace" \
    "$output" "namespace 'openbao-system' still exists"
  assert_contains "the error explains that the raft store does not move" \
    "$output" "raft store does NOT move with it"
  assert_contains "the error hands over the deletion command" \
    "$output" "kubectl delete namespace openbao-system"
  # That deletion destroys the sole copy of the root token and the unseal-key
  # shares, so the abort must also name the way to keep both stacks running.
  assert_contains "the abort names the ALLOW_PRE_RELOCATION escape hatch" \
    "$output" "ALLOW_PRE_RELOCATION=true"
}

# ---------------------------------------------------------------------------
# Test 2: a clean cluster passes
# ---------------------------------------------------------------------------
test_clean_cluster_passes() {
  echo "Test: check_relocated_infrastructure is a no-op on a clean cluster"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # Nothing is present: every lookup fails, exactly as on a fresh kind cluster
  # (and on any cluster already deployed from this branch).
  make_kubectl_stub "$tmp"

  local output exit_code
  output="$(run_guard "$tmp")"
  exit_code=$?

  assert_eq "the guard exits 0 on a clean cluster" "0" "$exit_code"
  assert_not_contains "the guard stays silent on a clean cluster" "$output" "ERROR:"
}

# ---------------------------------------------------------------------------
# Test 4: an unreadable cluster aborts instead of passing green
# ---------------------------------------------------------------------------
test_unreachable_cluster_aborts() {
  echo "Test: check_relocated_infrastructure aborts when it cannot read the cluster"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_unreachable_kubectl_stub "$tmp"

  local output exit_code
  output="$(run_guard "$tmp")"
  exit_code=$?

  # Before the fix both lookups were `kubectl get ... >/dev/null 2>&1`, so a
  # refused connection was indistinguishable from a missing object and the guard
  # silently waved a pre-relocation cluster through to Step 2.
  assert_nonzero_exit "the guard exits non-zero when the lookup is inconclusive" "$exit_code"
  assert_contains "the error says the verdict could not be determined" \
    "$output" "cannot determine whether this cluster predates"
  assert_contains "the error quotes the underlying kubectl failure" \
    "$output" "connection to the server"
}

# ---------------------------------------------------------------------------
# Test 5: ALLOW_PRE_RELOCATION downgrades the abort to a warning
# ---------------------------------------------------------------------------
test_allow_pre_relocation_overrides_abort() {
  echo "Test: ALLOW_PRE_RELOCATION=true continues against a pre-relocation cluster"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_kubectl_stub "$tmp" "get namespace openbao-system"

  local output exit_code
  output="$(run_guard "$tmp" true)"
  exit_code=$?

  # Without an override the only way past the guard is deleting the old stack,
  # which destroys the sole copy of the root token and the unseal-key shares
  # before the replacement has been proven.
  assert_eq "the guard continues under the override" "0" "$exit_code"
  assert_contains "the override still reports what it found" \
    "$output" "namespace 'openbao-system' still exists"
  assert_contains "the override warns that both stacks now run side by side" \
    "$output" "ALLOW_PRE_RELOCATION=true"
}

# ---------------------------------------------------------------------------
# Test 7: the side-by-side window the override opens is actually waitable
#
# The override leaves HelmRelease/openbao running in openbao-system while Step 3
# creates the second one in shared-services. wait_for_helmreleases resolves a
# bare release name with an all-namespaces lookup, so that name now matches two
# objects: every poll would query the two-line result as one namespace, fail
# RFC-1123 validation, read no Ready status, and burn the full
# HELMRELEASE_TIMEOUT before aborting a deploy that has already applied half the
# new stack — with the old one untouched. Waiting on the qualified
# 'shared-services/openbao' is what makes the escape hatch deliver its promise.
# ---------------------------------------------------------------------------

# make_helmrelease_kubectl_stub <dir>
# Installs a kubectl stub for a cluster in the ALLOW_PRE_RELOCATION window:
# HelmRelease/openbao exists twice, Ready only in shared-services.
make_helmrelease_kubectl_stub() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
case "$*" in
  "get helmrelease --all-namespaces -o json")
    echo '{"items":[{"metadata":{"name":"openbao","namespace":"openbao-system"}},{"metadata":{"name":"openbao","namespace":"shared-services"}}]}' ;;
  "get helmrelease openbao -n shared-services -o json")
    echo '{"status":{"conditions":[{"type":"Ready","status":"True"}]}}' ;;
  "get helmrelease openbao -n openbao-system -o json")
    echo '{"status":{"conditions":[{"type":"Ready","status":"False","reason":"Retired"}]}}' ;;
  *)
    echo "error: unexpected kubectl invocation: $*" >&2
    exit 1 ;;
esac
STUB
  chmod +x "$dir/kubectl"
}

# run_wait_for_helmreleases <stub_dir> <timeout> <release>...
run_wait_for_helmreleases() {
  local stub_dir="$1" timeout="$2"
  shift 2
  (
    PATH="$stub_dir:$PATH"
    export PATH
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    wait_for_helmreleases "$timeout" "$@"
  ) 2>&1
}

test_duplicate_release_name_is_waitable() {
  echo "Test: wait_for_helmreleases handles the openbao name existing in two namespaces"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_helmrelease_kubectl_stub "$tmp"

  # A namespace-qualified entry ignores the duplicate and returns as soon as the
  # release is Ready in the namespace named.
  local output exit_code
  output="$(run_wait_for_helmreleases "$tmp" 1 shared-services/openbao)"
  exit_code=$?

  assert_eq "the qualified release is awaited in its own namespace" "0" "$exit_code"
  assert_contains "the wait reports success" "$output" "All HelmReleases are Ready."

  # A bare name that matches two namespaces cannot be waited on at all. It must
  # say so at once instead of polling an invalid namespace until the timeout.
  local started elapsed
  started=$(date +%s)
  output="$(run_wait_for_helmreleases "$tmp" 1 openbao)"
  exit_code=$?
  elapsed=$(( $(date +%s) - started ))

  assert_nonzero_exit "an ambiguous bare release name fails" "$exit_code"
  assert_contains "the error names the ambiguity" \
    "$output" "exists in more than one namespace"
  assert_contains "the error lists both namespaces" \
    "$output" "openbao-system, shared-services"
  assert_contains "the error names the qualified form to use instead" \
    "$output" "namespace/openbao"
  # Before the fix this path polled `-n "openbao-system\nshared-services"` and
  # only gave up after HELMRELEASE_TIMEOUT (600s by default).
  assert_eq "the ambiguity is reported without waiting out the timeout" \
    "true" "$([ "$elapsed" -lt 5 ] && echo true || echo false)"
}

# ---------------------------------------------------------------------------
# Test 8: production-caller contract — Phase 3 waits on the qualified name
#
# The function-level test above is only worth its runtime if the deploy path
# actually passes the qualified entry.
# ---------------------------------------------------------------------------
test_phase3_waits_on_qualified_openbao() {
  echo "Test: the Phase 3 wait list names openbao with its namespace"

  assert_file_contains \
    "the helm_releases array qualifies openbao as shared-services/openbao" \
    "$DEPLOY_INFRA_SH" \
    'helm_releases=(prometheus-operator-crds shared-services/openbao mariadb-operator-crds'
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_retired_openbao_namespace_aborts
test_clean_cluster_passes
test_unreachable_cluster_aborts
test_allow_pre_relocation_overrides_abort
test_duplicate_release_name_is_waitable
test_phase3_waits_on_qualified_openbao

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
