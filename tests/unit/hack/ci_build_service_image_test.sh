#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-build-service-image.sh fetches the upstream source the way a
# self-hosted runner needs it to, using recording git/docker/yq/sleep stubs on
# PATH:
#   - git never waits for a credential prompt (GIT_TERMINAL_PROMPT=0);
#   - with GITHUB_TOKEN set, every git call carries the token as the
#     basic-auth header actions/checkout sends, scoped to https://github.com/,
#     through git's environment config, and the raw token reaches neither
#     git's argv nor docker's;
#   - a GIT_CONFIG_COUNT the caller set is appended to, not overwritten;
#   - without a token the clone stays anonymous;
#   - a github.com that rejects the clone is retried and then handed over to
#     opendev.org at the same ref;
#   - when no source serves the ref the build fails with a ::error:: before
#     docker runs.
#
# Follows the project-native bash test pattern (tests/lib/assertions.sh),
# mirroring tests/unit/hack/ci_build_ovn_image_test.sh.
#
# Usage: bash tests/unit/hack/ci_build_service_image_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
BUILD_SH="$PROJECT_ROOT/hack/ci-build-service-image.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

STUB_DIR="$TMP_DIR/bin"
GIT_LOG="$TMP_DIR/git.log"
DOCKER_LOG="$TMP_DIR/docker.log"

# scripts/apply-constraint-overrides.sh resolves releases/<release>/ against
# the working directory, so the builder runs in a throwaway tree carrying the
# one file that script insists on and no overrides: the repository's own
# constraints file is never touched.
WORK_DIR="$TMP_DIR/work"
mkdir -p "$WORK_DIR/releases/2025.2"
: >"$WORK_DIR/releases/2025.2/upper-constraints.txt"

GITHUB_URL="https://github.com/openstack/barbican.git"
OPENDEV_URL="https://opendev.org/openstack/barbican.git"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

make_stubs() {
  mkdir -p "$STUB_DIR"

  # git: records its argv and every GIT_CONFIG_* / GIT_TERMINAL_PROMPT it was
  # handed, one line per call. A clone whose argv contains one of the
  # space-separated substrings in GIT_STUB_FAIL fails like a rejected clone
  # (exit 128); everything else succeeds.
  cat >"$STUB_DIR/git" <<'STUB'
#!/bin/bash
{
  printf 'git %s' "$*"
  printf ' | GIT_TERMINAL_PROMPT=%s' "${GIT_TERMINAL_PROMPT-unset}"
  printf ' | GIT_CONFIG_COUNT=%s' "${GIT_CONFIG_COUNT-unset}"
  printf ' |'
  env | grep '^GIT_CONFIG_\(KEY\|VALUE\)_' | sort | while IFS= read -r entry; do
    printf ' %s' "$entry"
  done
  printf '\n'
} >>"$GIT_LOG"
if [ "$1" = clone ]; then
  for needle in ${GIT_STUB_FAIL:-}; do
    case "$*" in
      *"$needle"*)
        echo "fatal: could not read Username for 'https://github.com': terminal prompts disabled" >&2
        exit 128
        ;;
    esac
  done
fi
exit 0
STUB

  # docker: records its argv; `image inspect` succeeding means no base image
  # is rebuilt.
  cat >"$STUB_DIR/docker" <<'STUB'
#!/bin/bash
echo "docker $*" >>"$DOCKER_LOG"
exit 0
STUB

  # yq: one source ref for every service, and no extra packages.
  cat >"$STUB_DIR/yq" <<'STUB'
#!/bin/bash
case "$*" in
  *pip_extras*|*pip_packages*|*apt_packages*) echo "" ;;
  *) echo "21.0.0" ;;
esac
STUB

  # sleep: the backoff is asserted through the ::warning:: lines, not waited
  # for.
  cat >"$STUB_DIR/sleep" <<'STUB'
#!/bin/bash
exit 0
STUB

  chmod +x "$STUB_DIR"/git "$STUB_DIR"/docker "$STUB_DIR"/yq "$STUB_DIR"/sleep
}

# run_build [VAR=value ...]
# Runs the builder for barbican with the stubs first on PATH and fresh logs.
# GITHUB_TOKEN, GIT_CONFIG_* and GIT_STUB_FAIL start out unset; the arguments
# are exported on top of that. Stores the combined stdout/stderr in OUTPUT,
# the exit status in RC, and the recorded git and docker calls in GIT_CALLS
# and DOCKER_CALLS.
run_build() {
  RC=0
  rm -f "$GIT_LOG" "$DOCKER_LOG"
  OUTPUT="$(
    cd "$WORK_DIR" || exit 99
    unset GITHUB_TOKEN GIT_CONFIG_COUNT GIT_CONFIG_KEY_0 GIT_CONFIG_VALUE_0 GIT_STUB_FAIL
    for assignment in "$@"; do
      export "${assignment?}"
    done
    OPERATOR=barbican IMAGE_PREFIX=ghcr.io/c5c3 \
      PATH="$STUB_DIR:$PATH" GIT_LOG="$GIT_LOG" DOCKER_LOG="$DOCKER_LOG" \
      bash "$BUILD_SH" 2>&1
  )" || RC=$?

  GIT_CALLS=""
  if [ -f "$GIT_LOG" ]; then
    GIT_CALLS="$(cat "$GIT_LOG")"
  fi
  DOCKER_CALLS=""
  if [ -f "$DOCKER_LOG" ]; then
    DOCKER_CALLS="$(cat "$DOCKER_LOG")"
  fi
}

# clone_calls
# Echoes the recorded `git clone` lines.
clone_calls() {
  printf '%s\n' "$GIT_CALLS" | grep '^git clone' || true
}

# count_lines
# Echoes the number of non-empty lines on stdin.
count_lines() {
  grep -c . || true
}

make_stubs

# ---------------------------------------------------------------------------
# Test 1: without a token the clone is anonymous and never prompts
# ---------------------------------------------------------------------------
test_anonymous_without_token() {
  echo "Test: without GITHUB_TOKEN the clone is anonymous and never prompts"

  run_build

  local clone
  clone="$(clone_calls)"

  assert_eq "builder exits 0" "0" "$RC"
  assert_contains "the clone comes from the GitHub mirror at the pinned ref" "$clone" \
    "git clone --depth 1 --branch 21.0.0 ${GITHUB_URL}"
  assert_eq "exactly one clone is made" "1" "$(clone_calls | count_lines)"
  assert_contains "git never waits for a credential prompt" "$clone" "GIT_TERMINAL_PROMPT=0"
  assert_contains "no auth header is configured without a token" "$clone" "GIT_CONFIG_COUNT=unset"
  assert_not_contains "no extraheader reaches git without a token" "$GIT_CALLS" "extraheader"
  assert_contains "the service image is built from the clone" "$DOCKER_CALLS" \
    "docker build -t ghcr.io/c5c3/barbican:2025.2"
}

# ---------------------------------------------------------------------------
# Test 2: GITHUB_TOKEN becomes the github.com-scoped header
# ---------------------------------------------------------------------------
test_token_becomes_scoped_auth_header() {
  echo "Test: GITHUB_TOKEN reaches git as the header actions/checkout sends, scoped to github.com"

  local token="ghs_testtoken1234567890"
  local expected_value
  expected_value="AUTHORIZATION: basic $(printf 'x-access-token:%s' "$token" | base64 | tr -d '\n')"

  run_build "GITHUB_TOKEN=$token"

  local clone argv_only
  clone="$(clone_calls)"
  # The recorded argv, without the environment the stub appends after ' | '.
  argv_only="$(printf '%s\n' "$GIT_CALLS" | sed 's/ | .*//')"

  assert_eq "builder exits 0 with a token" "0" "$RC"
  assert_contains "one config entry is added" "$clone" "GIT_CONFIG_COUNT=1"
  assert_contains "the header is scoped to https://github.com/" "$clone" \
    "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader"
  assert_contains "the header carries the token as x-access-token basic auth" "$clone" \
    "GIT_CONFIG_VALUE_0=${expected_value}"
  assert_not_contains "the raw token is in no git argument" "$argv_only" "$token"
  assert_not_contains "the raw token is in no docker argument" "$DOCKER_CALLS" "$token"
  assert_contains "the clone still goes to the GitHub mirror" "$clone" "${GITHUB_URL}"
}

# ---------------------------------------------------------------------------
# Test 3: a GIT_CONFIG_COUNT the caller set is appended to
# ---------------------------------------------------------------------------
test_existing_config_entries_are_kept() {
  echo "Test: a GIT_CONFIG_COUNT the caller set is appended to, not overwritten"

  run_build "GITHUB_TOKEN=ghs_x" "GIT_CONFIG_COUNT=1" \
    "GIT_CONFIG_KEY_0=core.sshCommand" "GIT_CONFIG_VALUE_0=ssh -v"

  local clone
  clone="$(clone_calls)"

  assert_eq "builder exits 0" "0" "$RC"
  assert_contains "the count covers both entries" "$clone" "GIT_CONFIG_COUNT=2"
  assert_contains "the caller's entry survives" "$clone" "GIT_CONFIG_KEY_0=core.sshCommand"
  assert_contains "the auth header takes the next slot" "$clone" \
    "GIT_CONFIG_KEY_1=http.https://github.com/.extraheader"
}

# ---------------------------------------------------------------------------
# Test 4: a rejecting github.com is retried, then opendev.org serves the ref
# ---------------------------------------------------------------------------
test_github_rejection_falls_back_to_opendev() {
  echo "Test: a github.com that rejects the clone is retried, then opendev.org serves the same ref"

  run_build "GIT_STUB_FAIL=github.com"

  local clone
  clone="$(clone_calls)"

  assert_eq "builder exits 0 through the fallback" "0" "$RC"
  assert_eq "github.com is tried three times" "3" \
    "$(clone_calls | grep -c "${GITHUB_URL}")"
  assert_eq "opendev.org is tried once" "1" \
    "$(clone_calls | grep -c "${OPENDEV_URL}")"
  assert_contains "opendev.org is cloned at the same pinned ref" "$clone" \
    "--branch 21.0.0 ${OPENDEV_URL}"
  assert_contains "opendev.org is the last source tried" "$(clone_calls | tail -1)" "${OPENDEV_URL}"
  assert_contains "the retry is announced with its backoff" "$OUTPUT" \
    "::warning::git clone of ${GITHUB_URL} at 21.0.0 failed (attempt 1/3); retrying in 5s"
  assert_contains "the second retry backs off longer" "$OUTPUT" \
    "(attempt 2/3); retrying in 10s"
  assert_contains "the handover is announced" "$OUTPUT" \
    "::warning::${GITHUB_URL} did not serve 21.0.0 after 3 attempts"
  assert_contains "the image is still built" "$DOCKER_CALLS" \
    "docker build -t ghcr.io/c5c3/barbican:2025.2"
}

# ---------------------------------------------------------------------------
# Test 5: no source at all fails before docker runs
# ---------------------------------------------------------------------------
test_no_source_fails_before_docker() {
  echo "Test: when no source serves the ref the build fails with ::error:: and never builds"

  run_build "GIT_STUB_FAIL=github.com opendev.org"

  assert_nonzero_exit "builder fails" "$RC"
  assert_contains "the error names the ref and both sources" "$OUTPUT" \
    "::error::Could not clone openstack/barbican at 21.0.0 from any source: ${GITHUB_URL} ${OPENDEV_URL}"
  assert_eq "every source got its three attempts" "6" "$(clone_calls | count_lines)"
  assert_eq "docker never runs" "" "$DOCKER_CALLS"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_anonymous_without_token
test_token_becomes_scoped_auth_header
test_existing_config_entries_are_kept
test_github_rejection_falls_back_to_opendev
test_no_source_fails_before_docker

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
