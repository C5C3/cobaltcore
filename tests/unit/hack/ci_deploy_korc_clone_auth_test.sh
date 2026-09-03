#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-deploy-korc.sh reaches github.com the way a self-hosted
# runner needs it to, using recording git/kubectl/sleep stubs on PATH:
#   - git never waits for a credential prompt (GIT_TERMINAL_PROMPT=0);
#   - with GITHUB_TOKEN set, the clone AND the detached checkout (a blob-less
#     clone fetches on demand) carry the token as the basic-auth header
#     actions/checkout sends, scoped to https://github.com/, and the raw token
#     is in no argv;
#   - without a token the clone stays anonymous;
#   - a clone that fails is retried with a backoff and the deploy goes on once
#     it succeeds;
#   - a clone that never succeeds fails the step with a ::error:: before
#     anything is applied to the cluster.
#
# The pin gates in front of the clone are covered by
# tests/unit/hack/ci_deploy_korc_commit_pin_test.sh; the fixtures here pass
# them so every case reaches the clone.
#
# Usage: bash tests/unit/hack/ci_deploy_korc_clone_auth_test.sh

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
GIT_LOG="$TMP_DIR/git.log"
KUBECTL_LOG="$TMP_DIR/kubectl.log"
CLONE_COUNT="$TMP_DIR/clone-count"

KORC_URL="https://github.com/k-orc/openstack-resource-controller"

# A syntactically valid 40-char SHA and the per-commit image tag upstream
# derives from it, so the fixtures pass the commit gate and the drift guard.
VALID_COMMIT="0123456789abcdef0123456789abcdef01234567"
VALID_TAG="commit-0123456"
VALID_DIGEST="sha256:$(printf 'a%.0s' {1..64})"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

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

  # git: records its argv and the auth-relevant environment, one line per
  # call. The first GIT_STUB_FAIL_CLONES clone calls fail like a rejected
  # clone (exit 128); the count lives in CLONE_COUNT across calls.
  cat >"$STUB_DIR/git" <<'STUB'
#!/bin/bash
{
  printf 'git %s' "$*"
  printf ' | GIT_TERMINAL_PROMPT=%s' "${GIT_TERMINAL_PROMPT-unset}"
  printf ' | GIT_CONFIG_COUNT=%s' "${GIT_CONFIG_COUNT-unset}"
  printf ' | GIT_CONFIG_KEY_0=%s' "${GIT_CONFIG_KEY_0-unset}"
  printf ' | GIT_CONFIG_VALUE_0=%s\n' "${GIT_CONFIG_VALUE_0-unset}"
} >>"$GIT_LOG"
if [ "$1" = clone ]; then
  n=0
  [ -f "$CLONE_COUNT" ] && n="$(cat "$CLONE_COUNT")"
  n=$((n + 1))
  echo "$n" >"$CLONE_COUNT"
  if [ "$n" -le "${GIT_STUB_FAIL_CLONES:-0}" ]; then
    echo "fatal: could not read Username for 'https://github.com': terminal prompts disabled" >&2
    exit 128
  fi
fi
exit 0
STUB

  # kubectl: records its argv and succeeds.
  cat >"$STUB_DIR/kubectl" <<'STUB'
#!/bin/bash
echo "kubectl $*" >>"$KUBECTL_LOG"
exit 0
STUB

  # sleep: the backoff is asserted through the ::warning:: lines, not waited
  # for.
  cat >"$STUB_DIR/sleep" <<'STUB'
#!/bin/bash
exit 0
STUB

  chmod +x "$STUB_DIR"/git "$STUB_DIR"/kubectl "$STUB_DIR"/sleep
}

# run_deploy [VAR=value ...]
# Runs the deploy script against the fixtures with the stubs first on PATH and
# fresh logs. GITHUB_TOKEN, GIT_CONFIG_* and GIT_STUB_FAIL_CLONES start out
# unset; the arguments are exported on top of that. Stores the combined
# stdout/stderr in OUTPUT, the exit status in RC, and the recorded git and
# kubectl calls in GIT_CALLS and KUBECTL_CALLS.
run_deploy() {
  RC=0
  rm -f "$GIT_LOG" "$KUBECTL_LOG" "$CLONE_COUNT"
  OUTPUT="$(
    unset GITHUB_TOKEN GIT_CONFIG_COUNT GIT_CONFIG_KEY_0 GIT_CONFIG_VALUE_0 GIT_STUB_FAIL_CLONES
    for assignment in "$@"; do
      export "${assignment?}"
    done
    KORC_SOURCE="$TMP_DIR/source.yaml" KORC_RELEASE="$TMP_DIR/release.yaml" \
      PATH="$STUB_DIR:$PATH" GIT_LOG="$GIT_LOG" KUBECTL_LOG="$KUBECTL_LOG" CLONE_COUNT="$CLONE_COUNT" \
      bash "$DEPLOY_KORC_SH" 2>&1
  )" || RC=$?

  GIT_CALLS=""
  if [ -f "$GIT_LOG" ]; then
    GIT_CALLS="$(cat "$GIT_LOG")"
  fi
  KUBECTL_CALLS=""
  if [ -f "$KUBECTL_LOG" ]; then
    KUBECTL_CALLS="$(cat "$KUBECTL_LOG")"
  fi
}

# git_calls <subcommand-prefix>
# Echoes the recorded git lines starting with the given text.
git_calls() {
  printf '%s\n' "$GIT_CALLS" | grep "^git $1" || true
}

make_fixtures
make_stubs

# ---------------------------------------------------------------------------
# Test 1: GITHUB_TOKEN covers the clone and the checkout
# ---------------------------------------------------------------------------
test_token_covers_clone_and_checkout() {
  echo "Test: GITHUB_TOKEN reaches the clone and the detached checkout as the github.com-scoped header"

  local token="ghs_testtoken1234567890"
  local expected_value
  expected_value="AUTHORIZATION: basic $(printf 'x-access-token:%s' "$token" | base64 | tr -d '\n')"

  run_deploy "GITHUB_TOKEN=$token"

  local clone checkout argv_only
  clone="$(git_calls clone)"
  checkout="$(git_calls "-C .* checkout --detach")"
  argv_only="$(printf '%s\n' "$GIT_CALLS" | sed 's/ | .*//')"

  assert_eq "deploy exits 0" "0" "$RC"
  assert_contains "the clone targets K-ORC" "$clone" "git clone --filter=blob:none --no-checkout ${KORC_URL}"
  assert_contains "the clone never waits for a prompt" "$clone" "GIT_TERMINAL_PROMPT=0"
  assert_contains "the clone carries the github.com-scoped header" "$clone" \
    "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader"
  assert_contains "the header carries the token as x-access-token basic auth" "$clone" \
    "GIT_CONFIG_VALUE_0=${expected_value}"
  assert_not_empty "the pinned commit is checked out" "$checkout"
  assert_contains "the checkout pins the commit" "$checkout" "checkout --detach ${VALID_COMMIT}"
  assert_contains "the on-demand fetches of the checkout carry the same header" "$checkout" \
    "GIT_CONFIG_VALUE_0=${expected_value}"
  assert_not_contains "the raw token is in no git argument" "$argv_only" "$token"
  assert_contains "the manifests are applied once the source is in place" "$KUBECTL_CALLS" \
    "kubectl apply --server-side -k"
}

# ---------------------------------------------------------------------------
# Test 2: without a token the clone is anonymous
# ---------------------------------------------------------------------------
test_anonymous_without_token() {
  echo "Test: without GITHUB_TOKEN the clone is anonymous and never prompts"

  run_deploy

  local clone
  clone="$(git_calls clone)"

  assert_eq "deploy exits 0" "0" "$RC"
  assert_contains "the clone never waits for a prompt" "$clone" "GIT_TERMINAL_PROMPT=0"
  assert_contains "no auth header is configured without a token" "$clone" "GIT_CONFIG_COUNT=unset"
  assert_not_contains "no extraheader reaches git without a token" "$GIT_CALLS" "extraheader"
}

# ---------------------------------------------------------------------------
# Test 3: a failing clone is retried
# ---------------------------------------------------------------------------
test_failed_clone_is_retried() {
  echo "Test: a clone that fails twice is retried with a backoff and the deploy goes on"

  run_deploy "GIT_STUB_FAIL_CLONES=2"

  assert_eq "deploy exits 0 once the third clone succeeds" "0" "$RC"
  assert_eq "three clones were attempted" "3" "$(git_calls clone | grep -c .)"
  assert_contains "the first retry is announced with its backoff" "$OUTPUT" \
    "::warning::git clone of ${KORC_URL} failed (attempt 1/3); retrying in 5s"
  assert_contains "the second retry backs off longer" "$OUTPUT" \
    "(attempt 2/3); retrying in 10s"
  assert_not_empty "the checkout follows the successful clone" "$(git_calls "-C .* checkout --detach")"
  assert_contains "the manifests are applied" "$KUBECTL_CALLS" "kubectl apply --server-side -k"
}

# ---------------------------------------------------------------------------
# Test 4: a clone that never succeeds fails the step before any apply
# ---------------------------------------------------------------------------
test_unreachable_source_fails_before_apply() {
  echo "Test: a clone that never succeeds fails with ::error:: before anything is applied"

  run_deploy "GIT_STUB_FAIL_CLONES=99"

  assert_nonzero_exit "deploy fails" "$RC"
  assert_eq "the clone got its three attempts and no more" "3" "$(git_calls clone | grep -c .)"
  assert_contains "the error names the source and the attempts" "$OUTPUT" \
    "::error::Could not clone K-ORC from ${KORC_URL} after 3 attempts"
  assert_eq "nothing is checked out" "" "$(git_calls "-C .* checkout --detach")"
  assert_eq "nothing is applied to the cluster" "" "$KUBECTL_CALLS"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_token_covers_clone_and_checkout
test_anonymous_without_token
test_failed_clone_is_retried
test_unreachable_source_fails_before_apply

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
