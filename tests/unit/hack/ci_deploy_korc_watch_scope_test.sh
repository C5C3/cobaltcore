#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the watch-scope guard of hack/ci-deploy-korc.sh, using git/kubectl
# stubs on PATH: once the controller Deployment is Available the script reads
# the manager container's args and
#   - fails with a ::error:: when any form of upstream's --namespace flag is
#     present (K-ORC would then ignore the ControlPlane namespaces);
#   - passes when the args carry no --namespace, including when an unrelated
#     flag merely contains the word (--metrics-namespace-label is not a scope).
#
# The pin gates and the clone are covered by the sibling
# ci_deploy_korc_commit_pin_test.sh and ci_deploy_korc_clone_auth_test.sh; the
# fixtures here pass them so every case reaches the guard.
#
# Usage: bash tests/unit/hack/ci_deploy_korc_watch_scope_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_KORC_SH="$PROJECT_ROOT/hack/ci-deploy-korc.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

STUB_DIR="$TMP_DIR/bin"
KUBECTL_LOG="$TMP_DIR/kubectl.log"
ARGS_FILE="$TMP_DIR/manager-args"

VALID_COMMIT="0123456789abcdef0123456789abcdef01234567"
VALID_TAG="commit-0123456"
VALID_DIGEST="sha256:$(printf 'a%.0s' {1..64})"

make_fixtures() {
  cat >"$TMP_DIR/source.yaml" <<EOF
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: k-orc
spec:
  ref:
    commit: ${VALID_COMMIT}
EOF

  cat >"$TMP_DIR/release.yaml" <<EOF
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: k-orc
spec:
  images:
    - name: controller
      newTag: ${VALID_TAG}
      digest: ${VALID_DIGEST}
EOF
}

make_stubs() {
  mkdir -p "$STUB_DIR"

  # git: the clone and the checkout succeed without touching the network.
  cat >"$STUB_DIR/git" <<'STUB'
#!/bin/bash
exit 0
STUB

  # kubectl: records its argv; a `get deployment` answers with the manager
  # args staged in ARGS_FILE, one per line, the way the jsonpath range renders
  # them. Every other call succeeds silently.
  cat >"$STUB_DIR/kubectl" <<'STUB'
#!/bin/bash
echo "kubectl $*" >>"$KUBECTL_LOG"
if [ "$1" = get ] && [ "$2" = deployment ]; then
  cat "$ARGS_FILE"
fi
exit 0
STUB

  chmod +x "$STUB_DIR"/git "$STUB_DIR"/kubectl
}

# run_deploy <manager args, one per line>
run_deploy() {
  RC=0
  rm -f "$KUBECTL_LOG"
  printf '%s\n' "$1" >"$ARGS_FILE"
  OUTPUT="$(
    KORC_SOURCE="$TMP_DIR/source.yaml" KORC_RELEASE="$TMP_DIR/release.yaml" \
      PATH="$STUB_DIR:$PATH" KUBECTL_LOG="$KUBECTL_LOG" ARGS_FILE="$ARGS_FILE" \
      bash "$DEPLOY_KORC_SH" 2>&1
  )" || RC=$?
  KUBECTL_CALLS=""
  if [ -f "$KUBECTL_LOG" ]; then
    KUBECTL_CALLS="$(cat "$KUBECTL_LOG")"
  fi
}

make_fixtures
make_stubs

UPSTREAM_ARGS=$'--metrics-bind-address=:8443\n--leader-elect\n--health-probe-bind-address=:8081'

# ---------------------------------------------------------------------------
# Test 1: the upstream args pass
# ---------------------------------------------------------------------------
test_upstream_args_pass() {
  echo "Test: the upstream manager args carry no --namespace and pass the guard"
  run_deploy "$UPSTREAM_ARGS"
  assert_eq "deploy exits 0" "0" "$RC"
  assert_contains "the guard reads the manager args after the wait" "$KUBECTL_CALLS" \
    "kubectl get deployment orc-controller-manager -n orc-system"
  assert_contains "the success line names the scope" "$OUTPUT" "watching every namespace"
}

# ---------------------------------------------------------------------------
# Test 2: --namespace=<ns> fails
# ---------------------------------------------------------------------------
test_namespace_equals_fails() {
  echo "Test: --namespace=orc-system fails the deploy"
  run_deploy "$UPSTREAM_ARGS"$'\n--namespace=orc-system'
  assert_nonzero_exit "deploy fails" "$RC"
  assert_contains "the error names the flag and the reason" "$OUTPUT" \
    "::error::The K-ORC manager in orc-system carries a --namespace flag"
  assert_contains "the error prints the offending args" "$OUTPUT" "  --namespace=orc-system"
}

# ---------------------------------------------------------------------------
# Test 3: the two-token form "--namespace <ns>" fails too
# ---------------------------------------------------------------------------
test_namespace_two_tokens_fails() {
  echo "Test: the two-token form --namespace openstack fails the deploy"
  run_deploy "$UPSTREAM_ARGS"$'\n--namespace\nopenstack'
  assert_nonzero_exit "deploy fails" "$RC"
  assert_contains "the error names the flag" "$OUTPUT" "carries a --namespace flag"
}

# ---------------------------------------------------------------------------
# Test 4: a flag that merely contains the word is not a scope restriction
# ---------------------------------------------------------------------------
test_lookalike_flag_passes() {
  echo "Test: a flag that only contains 'namespace' passes the guard"
  run_deploy "$UPSTREAM_ARGS"$'\n--metrics-namespace-label=team'
  assert_eq "deploy exits 0" "0" "$RC"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_upstream_args_pass
test_namespace_equals_fails
test_namespace_two_tokens_fails
test_lookalike_flag_passes

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
