#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-cosign-sign.sh with a stubbed cosign on PATH:
#   - a signature that cosign writes and verifies runs `cosign sign --yes` and
#     then `cosign verify` against the issuer of GitHub Actions and the
#     subject https://github.com/<GITHUB_WORKFLOW_REF>, for a branch and for a
#     tag, keeps cosign's stderr and discards the payloads verify prints;
#   - a failed sign is retried until it succeeds, with a ::warning:: per retry,
#     and so is a failed verify;
#   - the retries stop at COSIGN_ATTEMPTS with an ::error:: and cosign's exit
#     status, and a sign that never succeeds is never verified;
#   - a verify that never succeeds fails with an ::error:: naming the identity
#     and cosign's exit status;
#   - a missing or malformed digest reference, a missing GITHUB_WORKFLOW_REF
#     and a bad COSIGN_ATTEMPTS or COSIGN_DELAY fail before cosign runs.
#
# The publish jobs that call the script run on a push only, so no pull request
# run shows a broken signing step before the artifacts are public.
#
# Usage: bash tests/unit/hack/ci_cosign_sign_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SIGN_SH="$PROJECT_ROOT/hack/ci-cosign-sign.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

REF="ghcr.io/c5c3/keystone-operator@sha256:093a120f0123456789abcdef0123456789abcdef0123456789abcdef01234567"
MAIN_REF="C5C3/cobaltcore/.github/workflows/ci.yaml@refs/heads/main"
TAG_REF="C5C3/cobaltcore/.github/workflows/ci.yaml@refs/tags/v0.1.0"
ISSUER="https://token.actions.githubusercontent.com"
USAGE="usage: hack/ci-cosign-sign.sh <repository>@sha256:<digest>"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stub <dir>
# Writes a cosign stub into <dir>. Each call appends its arguments to
# <dir>/calls and plays the next outcome of COSIGN_STUB_SIGN or
# COSIGN_STUB_VERIFY, by subcommand (space-separated; the last one repeats;
# default ok): ok exits 0, and on verify prints a payload to stdout as cosign
# does; fail prints an error to stderr and exits 3. The script exits 0 or 2 on
# its own, so an exit 3 is cosign's status passed on.
make_stub() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/cosign" <<'STUB'
#!/bin/bash
dir="$(dirname "$0")"
echo "$*" >>"$dir/calls"
sub="$1"
case "$sub" in
  sign) outcomes="${COSIGN_STUB_SIGN:-ok}" ;;
  verify) outcomes="${COSIGN_STUB_VERIFY:-ok}" ;;
  *) exit 99 ;;
esac
n="$(grep -c "^$sub " "$dir/calls")"
set -- $outcomes
while [ "$n" -gt 1 ] && [ $# -gt 1 ]; do
  shift
  n=$((n - 1))
done
case "$1" in
  ok)
    if [ "$sub" = verify ]; then
      echo "STUB-VERIFY-PAYLOAD"
    fi
    exit 0
    ;;
  fail)
    echo "Error: stub $sub failed" >&2
    exit 3
    ;;
esac
exit 99
STUB
  chmod +x "$dir/cosign"
}

# run_sign <stub_dir> <workflow ref | -> [args...]
# Runs the sign script with the stub dir prepended to PATH, no retry delay
# unless COSIGN_DELAY is set, and GITHUB_WORKFLOW_REF set to the given value,
# or unset for "-". The test-shell job runs inside GitHub Actions, where the
# runner sets GITHUB_WORKFLOW_REF, so the helper never inherits it. Echoes
# combined stdout/stderr; returns the script's exit status.
run_sign() {
  local stub_dir="$1" workflow_ref="$2"
  shift 2
  (
    PATH="$stub_dir:$PATH"
    COSIGN_DELAY="${COSIGN_DELAY:-0}"
    export PATH COSIGN_DELAY
    if [ "$workflow_ref" = "-" ]; then
      unset GITHUB_WORKFLOW_REF
    else
      export GITHUB_WORKFLOW_REF="$workflow_ref"
    fi
    bash "$SIGN_SH" "$@"
  ) 2>&1
}

# calls_of <stub_dir> <subcommand>
# Echoes how many times the cosign stub ran the subcommand.
calls_of() {
  if [ -f "$1/calls" ]; then
    grep -c "^$2 " "$1/calls"
  else
    echo 0
  fi
}

# all_calls <stub_dir>
# Echoes every call of the cosign stub, one per line.
all_calls() {
  if [ -f "$1/calls" ]; then
    cat "$1/calls"
  fi
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_success_signs_then_verifies() {
  echo "Test: a signature cosign writes and verifies runs sign, then verify against the workflow identity"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stub "$tmp/bin"

  local output exit_code
  output="$(run_sign "$tmp/bin" "$MAIN_REF" "$REF")"
  exit_code=$?

  assert_eq "the script exits 0" "0" "$exit_code"
  assert_eq "cosign ran sign, then verify, once each" \
    "sign --yes $REF
verify --certificate-oidc-issuer $ISSUER --certificate-identity https://github.com/$MAIN_REF $REF" \
    "$(all_calls "$tmp/bin")"
  assert_contains "the log names the reference and the identity" "$output" \
    "Signed $REF; verified against https://github.com/$MAIN_REF"
  assert_not_contains "no retry warning on success" "$output" "::warning::"
  assert_not_contains "the payloads verify prints stay out of the log" "$output" "STUB-VERIFY-PAYLOAD"
}

test_a_tag_push_verifies_against_the_tag() {
  echo "Test: on a tag push the identity ends in the tag ref"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stub "$tmp/bin"

  local output exit_code
  output="$(run_sign "$tmp/bin" "$TAG_REF" "$REF")"
  exit_code=$?

  assert_eq "the script exits 0" "0" "$exit_code"
  assert_file_contains_fixed "verify gets the tag identity" "$tmp/bin/calls" \
    "--certificate-identity https://github.com/C5C3/cobaltcore/.github/workflows/ci.yaml@refs/tags/v0.1.0 $REF"
  assert_contains "the log names the tag identity" "$output" \
    "verified against https://github.com/$TAG_REF"
}

test_a_failed_sign_is_retried() {
  echo "Test: a failed sign is retried until it succeeds"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stub "$tmp/bin"

  local output exit_code
  output="$(COSIGN_STUB_SIGN="fail fail ok" run_sign "$tmp/bin" "$MAIN_REF" "$REF")"
  exit_code=$?

  assert_eq "the script exits 0 once the third sign succeeds" "0" "$exit_code"
  assert_eq "sign ran three times" "3" "$(calls_of "$tmp/bin" sign)"
  assert_eq "verify ran once" "1" "$(calls_of "$tmp/bin" verify)"
  assert_contains "the first retry is announced" "$output" "::warning::cosign sign attempt 1 of $REF failed; retrying in 0s"
  assert_contains "the second retry is announced" "$output" "::warning::cosign sign attempt 2 of"
  assert_not_contains "no error once the sign succeeds" "$output" "::error::"
}

test_the_retry_pause_triples() {
  echo "Test: the pause starts at COSIGN_DELAY and triples per retry"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stub "$tmp/bin"
  # sleep is stubbed too, so the pauses the script announces take no time.
  printf '#!/bin/bash\nexit 0\n' >"$tmp/bin/sleep"
  chmod +x "$tmp/bin/sleep"

  local output
  output="$(COSIGN_DELAY=5 COSIGN_STUB_SIGN="fail fail ok" run_sign "$tmp/bin" "$MAIN_REF" "$REF")"

  assert_contains "the first pause is COSIGN_DELAY" "$output" "attempt 1 of $REF failed; retrying in 5s"
  assert_contains "the second pause is three times as long" "$output" "attempt 2 of $REF failed; retrying in 15s"
}

test_a_sign_that_never_succeeds_fails() {
  echo "Test: the sign retries stop at COSIGN_ATTEMPTS, and nothing is verified"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stub "$tmp/bin"

  local output exit_code
  output="$(COSIGN_STUB_SIGN=fail run_sign "$tmp/bin" "$MAIN_REF" "$REF")"
  exit_code=$?

  assert_eq "the script exits with cosign's status" "3" "$exit_code"
  assert_eq "sign ran the default three times" "3" "$(calls_of "$tmp/bin" sign)"
  assert_eq "verify never ran" "0" "$(calls_of "$tmp/bin" verify)"
  assert_contains "the give-up is an ::error::" "$output" \
    "::error::cosign sign of $REF failed after 3 attempts"
  assert_contains "cosign's error reaches the log" "$output" "Error: stub sign failed"
  assert_not_contains "nothing claims a signature" "$output" "Signed "

  rm -f "$tmp/bin/calls"
  output="$(COSIGN_ATTEMPTS=1 COSIGN_STUB_SIGN=fail run_sign "$tmp/bin" "$MAIN_REF" "$REF")"
  exit_code=$?

  assert_nonzero_exit "the script fails with COSIGN_ATTEMPTS=1" "$exit_code"
  assert_eq "COSIGN_ATTEMPTS=1 runs sign once" "1" "$(calls_of "$tmp/bin" sign)"
  assert_not_contains "COSIGN_ATTEMPTS=1 retries nothing" "$output" "::warning::"
}

test_a_verify_that_never_succeeds_fails() {
  echo "Test: a verify that never succeeds fails with the identity in the error"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stub "$tmp/bin"

  local output exit_code
  output="$(COSIGN_STUB_VERIFY=fail run_sign "$tmp/bin" "$MAIN_REF" "$REF")"
  exit_code=$?

  assert_eq "the script exits with cosign's status" "3" "$exit_code"
  assert_eq "sign ran once" "1" "$(calls_of "$tmp/bin" sign)"
  assert_eq "verify ran three times" "3" "$(calls_of "$tmp/bin" verify)"
  assert_contains "each verify retry is announced" "$output" "::warning::cosign verify attempt 2 of $REF failed"
  assert_contains "the give-up names the reference and the identity" "$output" \
    "::error::cosign verify of $REF against https://github.com/$MAIN_REF failed after 3 attempts"
  assert_contains "cosign's stderr stays in the log" "$output" "Error: stub verify failed"
  assert_not_contains "nothing claims a signature" "$output" "Signed "
}

test_a_failed_verify_is_retried() {
  echo "Test: a failed verify is retried until it succeeds, without a second sign"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stub "$tmp/bin"

  local output exit_code
  output="$(COSIGN_STUB_VERIFY="fail ok" run_sign "$tmp/bin" "$MAIN_REF" "$REF")"
  exit_code=$?

  assert_eq "the script exits 0 once the second verify succeeds" "0" "$exit_code"
  assert_eq "sign ran once" "1" "$(calls_of "$tmp/bin" sign)"
  assert_eq "verify ran twice" "2" "$(calls_of "$tmp/bin" verify)"
  assert_eq "the one retry is announced once" "1" \
    "$(grep -cF "::warning::cosign verify attempt 1 of $REF failed" <<<"$output")"
  assert_contains "the log names the reference and the identity" "$output" \
    "Signed $REF; verified against https://github.com/$MAIN_REF"
  assert_not_contains "no error once the verify succeeds" "$output" "::error::"
}

test_bad_references_fail_before_cosign() {
  echo "Test: a missing or malformed digest reference fails before cosign runs"

  local tmp output exit_code
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stub "$tmp/bin"

  output="$(run_sign "$tmp/bin" "$MAIN_REF")"
  exit_code=$?
  assert_eq "no argument exits 2" "2" "$exit_code"
  assert_eq "no argument prints the usage line" "$USAGE" "$output"

  output="$(run_sign "$tmp/bin" "$MAIN_REF" "$REF" "$REF")"
  exit_code=$?
  assert_eq "two arguments exit 2" "2" "$exit_code"
  assert_eq "two arguments print the usage line" "$USAGE" "$output"

  # A step output that was never written expands to the empty string.
  output="$(run_sign "$tmp/bin" "$MAIN_REF" "")"
  exit_code=$?
  assert_eq "the empty string exits 2" "2" "$exit_code"
  assert_eq "the empty string prints the usage line" "$USAGE" "$output"

  local bad
  for bad in "ghcr.io/c5c3/keystone-operator:latest" \
    "ghcr.io/c5c3/keystone-operator@sha256:abc" \
    "$REF " \
    "@sha256:${REF##*sha256:}"; do
    output="$(run_sign "$tmp/bin" "$MAIN_REF" "$bad")"
    exit_code=$?
    assert_eq "'$bad' exits 2" "2" "$exit_code"
    assert_eq "'$bad' prints the usage line" "$USAGE" "$output"
  done

  assert_eq "cosign never ran" "" "$(all_calls "$tmp/bin")"
}

test_bad_environment_fails_before_cosign() {
  echo "Test: a missing GITHUB_WORKFLOW_REF or a bad retry setting fails before cosign runs"

  local tmp output exit_code
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stub "$tmp/bin"

  output="$(run_sign "$tmp/bin" - "$REF")"
  exit_code=$?
  assert_eq "an unset GITHUB_WORKFLOW_REF exits 2" "2" "$exit_code"
  assert_contains "the error names GITHUB_WORKFLOW_REF" "$output" \
    "::error::GITHUB_WORKFLOW_REF is not set; it names the certificate identity the signature is verified against"

  output="$(run_sign "$tmp/bin" "" "$REF")"
  exit_code=$?
  assert_eq "an empty GITHUB_WORKFLOW_REF exits 2" "2" "$exit_code"
  assert_contains "the error names GITHUB_WORKFLOW_REF" "$output" "GITHUB_WORKFLOW_REF"

  local setting
  for setting in COSIGN_ATTEMPTS=0 COSIGN_ATTEMPTS=abc COSIGN_DELAY=abc COSIGN_DELAY=-1; do
    output="$(export "${setting?}" && run_sign "$tmp/bin" "$MAIN_REF" "$REF")"
    exit_code=$?
    assert_eq "$setting exits 2" "2" "$exit_code"
    assert_contains "$setting is named in the error" "$output" \
      "::error::COSIGN_ATTEMPTS must be a positive integer and COSIGN_DELAY a non-negative one"
  done

  assert_eq "cosign never ran" "" "$(all_calls "$tmp/bin")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_success_signs_then_verifies
test_a_tag_push_verifies_against_the_tag
test_a_failed_sign_is_retried
test_the_retry_pause_triples
test_a_sign_that_never_succeeds_fails
test_a_verify_that_never_succeeds_fails
test_a_failed_verify_is_retried
test_bad_references_fail_before_cosign
test_bad_environment_fails_before_cosign

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
