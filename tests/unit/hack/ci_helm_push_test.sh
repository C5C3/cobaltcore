#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-helm-push.sh with a stubbed helm on PATH:
#   - a push that succeeds runs helm once and passes the archive and the remote
#     through;
#   - the ghcr.io tag-fetch 404 (runs 39266486878 and 39922685226) is retried
#     until the push succeeds, with a ::warning:: per retry;
#   - the retries stop at HELM_PUSH_ATTEMPTS with an ::error:: and helm's exit
#     status;
#   - any other helm error fails on the first attempt, including a tag error
#     that is not a 404;
#   - a wrong argument count or a missing archive fails before helm runs.
#
# Usage: bash tests/unit/hack/ci_helm_push_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
PUSH_SH="$PROJECT_ROOT/hack/ci-helm-push.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

REMOTE="oci://ghcr.io/c5c3/charts"
TAG_404='Error: failed to perform "Tag" on destination: sha256:c9defeda34763a1c725bdb3180c1dcc495b0d1b5e357849e2e8f476e9bd8c5cb: not found'

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stub <dir>
# Writes a helm stub into <dir>. Each call appends its arguments to
# <dir>/calls and plays the next outcome of HELM_STUB_OUTCOMES (space-separated;
# the last one repeats): ok prints helm's success lines, tag404 the ghcr.io
# tag-fetch 404, tag401 a tag failure with another cause, auth an unrelated
# error.
make_stub() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/helm" <<'STUB'
#!/bin/bash
dir="$(dirname "$0")"
echo "$*" >>"$dir/calls"
n="$(wc -l <"$dir/calls" | tr -d ' ')"
set -- ${HELM_STUB_OUTCOMES}
while [ "$n" -gt 1 ] && [ $# -gt 1 ]; do
  shift
  n=$((n - 1))
done
case "$1" in
  ok)
    echo "Pushed: ghcr.io/c5c3/charts/placement-operator:0.7.0"
    echo "Digest: sha256:c9defeda34763a1c725bdb3180c1dcc495b0d1b5e357849e2e8f476e9bd8c5cb"
    exit 0
    ;;
  tag404)
    echo 'Error: failed to perform "Tag" on destination: sha256:c9defeda34763a1c725bdb3180c1dcc495b0d1b5e357849e2e8f476e9bd8c5cb: not found' >&2
    exit 1
    ;;
  tag401)
    echo 'Error: failed to perform "Tag" on destination: GET "https://ghcr.io/v2/c5c3/charts/placement-operator/manifests/sha256:c9defeda34763a1c725bdb3180c1dcc495b0d1b5e357849e2e8f476e9bd8c5cb": response status code 401: unauthorized' >&2
    exit 1
    ;;
  auth)
    echo 'Error: unexpected status from HEAD request to https://ghcr.io/v2/c5c3/charts/placement-operator/manifests/0.7.0: 403 Forbidden' >&2
    exit 1
    ;;
esac
exit 99
STUB
  chmod +x "$dir/helm"
}

# run_push <stub_dir> <outcomes> [args...]
# Runs the push script with the stub dir prepended to PATH and no retry delay.
# Echoes combined stdout/stderr; returns the script's exit status.
run_push() {
  local stub_dir="$1" outcomes="$2"
  shift 2
  (
    PATH="$stub_dir:$PATH"
    HELM_STUB_OUTCOMES="$outcomes"
    HELM_PUSH_DELAY=0
    export PATH HELM_STUB_OUTCOMES HELM_PUSH_DELAY
    bash "$PUSH_SH" "$@"
  ) 2>&1
}

# call_count <stub_dir>
# Echoes how many times the helm stub ran.
call_count() {
  if [ -f "$1/calls" ]; then
    wc -l <"$1/calls" | tr -d ' '
  else
    echo 0
  fi
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_success_pushes_once() {
  echo "Test: a successful push runs helm once with the archive and the remote"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stub "$tmp/bin"
  touch "$tmp/placement-operator-0.7.0.tgz"

  local output exit_code
  output="$(run_push "$tmp/bin" "ok" "$tmp/placement-operator-0.7.0.tgz" "$REMOTE")"
  exit_code=$?

  assert_eq "push exits 0" "0" "$exit_code"
  assert_eq "helm ran once" "1" "$(call_count "$tmp/bin")"
  assert_file_contains_fixed "helm got push, the archive and the remote" "$tmp/bin/calls" \
    "push $tmp/placement-operator-0.7.0.tgz $REMOTE"
  assert_contains "helm's output reaches the log" "$output" "Pushed: ghcr.io/c5c3/charts/placement-operator:0.7.0"
  assert_not_contains "no retry warning on success" "$output" "::warning::"
}

test_tag_404_is_retried() {
  echo "Test: the ghcr.io tag-fetch 404 is retried until the push succeeds"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stub "$tmp/bin"
  touch "$tmp/placement-operator-0.7.0.tgz"

  local output exit_code
  output="$(run_push "$tmp/bin" "tag404 tag404 ok" "$tmp/placement-operator-0.7.0.tgz" "$REMOTE")"
  exit_code=$?

  assert_eq "push exits 0 once the third attempt tags the manifest" "0" "$exit_code"
  assert_eq "helm ran three times" "3" "$(call_count "$tmp/bin")"
  assert_contains "the first failure is logged" "$output" "$TAG_404"
  assert_contains "the first retry is announced" "$output" "::warning::helm push attempt 1 of"
  assert_contains "the second retry is announced" "$output" "::warning::helm push attempt 2 of"
  assert_not_contains "no error once the push succeeds" "$output" "::error::"
}

test_retries_are_bounded() {
  echo "Test: the retries stop at HELM_PUSH_ATTEMPTS"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stub "$tmp/bin"
  touch "$tmp/placement-operator-0.7.0.tgz"

  local output exit_code
  output="$(run_push "$tmp/bin" "tag404" "$tmp/placement-operator-0.7.0.tgz" "$REMOTE")"
  exit_code=$?

  assert_eq "push exits with helm's status" "1" "$exit_code"
  assert_eq "helm ran the default three times" "3" "$(call_count "$tmp/bin")"
  assert_contains "the give-up is an ::error::" "$output" "could not tag it after 3 attempts"

  rm -f "$tmp/bin/calls"
  output="$(HELM_PUSH_ATTEMPTS=1 run_push "$tmp/bin" "tag404" "$tmp/placement-operator-0.7.0.tgz" "$REMOTE")"
  exit_code=$?

  assert_nonzero_exit "push fails with HELM_PUSH_ATTEMPTS=1" "$exit_code"
  assert_eq "HELM_PUSH_ATTEMPTS=1 runs helm once" "1" "$(call_count "$tmp/bin")"
}

test_other_errors_fail_at_once() {
  echo "Test: any other helm error fails on the first attempt"

  local tmp outcome output exit_code
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stub "$tmp/bin"
  touch "$tmp/placement-operator-0.7.0.tgz"

  for outcome in auth tag401; do
    rm -f "$tmp/bin/calls"
    output="$(run_push "$tmp/bin" "$outcome ok" "$tmp/placement-operator-0.7.0.tgz" "$REMOTE")"
    exit_code=$?

    assert_eq "$outcome: push exits with helm's status" "1" "$exit_code"
    assert_eq "$outcome: helm ran once" "1" "$(call_count "$tmp/bin")"
    assert_not_contains "$outcome: no retry" "$output" "::warning::"
  done
}

test_bad_arguments_fail_before_helm() {
  echo "Test: a wrong argument count or a missing archive fails before helm runs"

  local tmp output exit_code
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stub "$tmp/bin"
  touch "$tmp/a-operator-0.1.0.tgz" "$tmp/a-operator-0.2.0.tgz"

  # Two archives behind the workflow's glob arrive as three arguments.
  output="$(run_push "$tmp/bin" "ok" "$tmp/a-operator-0.1.0.tgz" "$tmp/a-operator-0.2.0.tgz" "$REMOTE")"
  exit_code=$?
  assert_eq "three arguments are a usage error" "2" "$exit_code"
  assert_contains "the usage line is printed" "$output" "usage:"

  # A glob that matched nothing arrives verbatim.
  output="$(run_push "$tmp/bin" "ok" "$tmp/b-operator-*.tgz" "$REMOTE")"
  exit_code=$?
  assert_eq "a missing archive exits 1" "1" "$exit_code"
  assert_contains "the missing archive is named" "$output" "::error::chart archive $tmp/b-operator-*.tgz does not exist"

  output="$(HELM_PUSH_ATTEMPTS=0 run_push "$tmp/bin" "ok" "$tmp/a-operator-0.1.0.tgz" "$REMOTE")"
  exit_code=$?
  assert_eq "HELM_PUSH_ATTEMPTS=0 is a usage error" "2" "$exit_code"

  assert_eq "helm never ran" "0" "$(call_count "$tmp/bin")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_success_pushes_once
test_tag_404_is_retried
test_retries_are_bounded
test_other_errors_fail_at_once
test_bad_arguments_fail_before_helm

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
