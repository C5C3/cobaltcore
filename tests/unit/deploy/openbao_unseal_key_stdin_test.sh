#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify that neither unseal implementation ever passes a Shamir share as a
# command-line argument: the production bring-up path
# deploy/openbao/bootstrap/init-unseal.sh and the single-replica kind path
# hack/deploy-infra.sh::openbao_init_unseal.
#
# A share on the remote command line is readable twice. `kubectl exec` encodes
# every element of the remote command as a repeated `command=` query parameter
# and the API server records that request URI in its audit log, so anyone with
# read access to the audit sink can grep the shares back out. Inside the
# container the expanded argument sits in /proc/<pid>/cmdline for as long as
# the call runs. Three shares are the whole 3-of-5 threshold, so neither copy
# is acceptable — the share must arrive on stdin.
#
# Both paths run against a recording kubectl stub prepended to PATH, so no
# cluster is touched: the stub answers `bao status` as initialized-and-sealed,
# hands out a synthetic init-output Secret, logs every argv it is called with,
# and captures what the unseal call receives on stdin.
#
# Usage: bash tests/unit/deploy/openbao_unseal_key_stdin_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
INIT_UNSEAL_SH="$PROJECT_ROOT/deploy/openbao/bootstrap/init-unseal.sh"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# The shares the stubbed Secret hands out. Distinctive strings, so a substring
# search over the recorded argv cannot match one by accident.
SHARE_0="UnsealShareZero"
SHARE_1="UnsealShareOne"
SHARE_2="UnsealShareTwo"
INIT_OUTPUT="{\"unseal_keys_b64\":[\"${SHARE_0}\",\"${SHARE_1}\",\"${SHARE_2}\",\"UnsealShareThree\",\"UnsealShareFour\"],\"root_token\":\"RootTokenValue\"}"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_kubectl_stub <dir>
# Writes a recording kubectl shim into <dir>:
#   get secret …  — echoes $INIT_OUTPUT_B64 (the base64 the scripts pipe into
#                   `base64 -d` to recover the init JSON).
#   bao status    — reports initialized + sealed and exits 2, the real exit
#                   code for a sealed store, so the init branch is skipped and
#                   the unseal branch is taken.
#   bao write …   — the post-fix unseal call: appends its stdin to $STDIN_LOG.
# Every invocation appends its full argv to $KUBECTL_ARGV_LOG first, so a share
# passed as an argument — the pre-fix `bao operator unseal <key>` shape — is
# recorded there and fails the assertions below.
make_kubectl_stub() {
  local dir="$1"
  mkdir -p "$dir"

  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
printf '%s\n' "$*" >> "$KUBECTL_ARGV_LOG"

case "$*" in
  *"get secret"*)
    printf '%s' "$INIT_OUTPUT_B64"
    ;;
  *"bao write sys/unseal"*)
    { cat; echo; } >> "$STDIN_LOG"
    ;;
  *"bao status"*)
    echo '{"initialized":true,"sealed":true}'
    exit 2
    ;;
esac
exit 0
STUB

  chmod +x "$dir/kubectl"
}

# setup_env <tmpdir>
# Prepares the stub and the two log files, and exports what the stub reads.
setup_env() {
  local tmp="$1"
  make_kubectl_stub "$tmp/bin"
  export KUBECTL_ARGV_LOG="$tmp/kubectl-argv.log"
  export STDIN_LOG="$tmp/unseal-stdin.log"
  : >"$KUBECTL_ARGV_LOG"
  : >"$STDIN_LOG"
  INIT_OUTPUT_B64="$(printf '%s' "$INIT_OUTPUT" | base64 | tr -d '\n')"
  export INIT_OUTPUT_B64
}

# assert_shares_only_on_stdin <label> <threshold-count>
# The shared expectation of both paths: no share in any recorded argv, every
# share on stdin instead, and the unseal exec carrying -i plus the stdin-reading
# `key=-` form. <threshold-count> is how many shares the run should have applied.
assert_shares_only_on_stdin() {
  local label="$1" expected_writes="$2"
  local argv_log stdin_log
  argv_log="$(cat "$KUBECTL_ARGV_LOG")"
  stdin_log="$(cat "$STDIN_LOG")"

  local share
  for share in "$SHARE_0" "$SHARE_1" "$SHARE_2"; do
    assert_not_contains "${label}: share ${share} never reaches a kubectl argv" \
      "$argv_log" "$share"
    assert_contains "${label}: share ${share} is delivered on stdin" \
      "$stdin_log" "$share"
  done

  assert_contains "${label}: the unseal exec forwards stdin (-i)" \
    "$argv_log" "exec -i"
  assert_contains "${label}: the share is written to sys/unseal from stdin" \
    "$argv_log" "bao write sys/unseal key=-"
  assert_not_contains "${label}: the argv-borne unseal form is gone" \
    "$argv_log" "bao operator unseal"
  assert_eq "${label}: one write per share up to the threshold" \
    "$expected_writes" "$(grep -c . "$STDIN_LOG")"
}

# ---------------------------------------------------------------------------
# Test 1: the production init-unseal.sh keeps every share off the command line
#
# The script has no source guard (it ends in `main "$@"`), so it runs as a
# subprocess. It unseals all three replica pods at a threshold of 3, so a full
# run applies nine shares — the nine argv-borne copies the pre-fix loop emitted.
# ---------------------------------------------------------------------------
test_init_unseal_uses_stdin() {
  echo "Test: init-unseal.sh applies each share over stdin, never as an argument"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  setup_env "$tmp"

  local output rc=0
  output="$(PATH="$tmp/bin:$PATH" bash "$INIT_UNSEAL_SH" 2>&1)" || rc=$?

  assert_eq "init-unseal.sh completes" "0" "$rc"
  assert_contains "the run reports the unseal" "$output" "unsealed successfully"
  assert_shares_only_on_stdin "init-unseal.sh" 9
}

# ---------------------------------------------------------------------------
# Test 2: the kind single-replica path in deploy-infra.sh does the same
#
# deploy-infra.sh guards its `main`, so the function is called directly in a
# subshell. One pod at a threshold of 3 means three shares.
# ---------------------------------------------------------------------------
test_deploy_infra_uses_stdin() {
  echo "Test: deploy-infra.sh::openbao_init_unseal applies each share over stdin"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  setup_env "$tmp"

  local output rc=0
  output="$(
    PATH="$tmp/bin:$PATH"
    export PATH
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    openbao_init_unseal
  )" || rc=$?

  assert_eq "openbao_init_unseal completes" "0" "$rc"
  assert_contains "the run reports the unseal" "$output" "openbao-0 unsealed successfully"
  assert_shares_only_on_stdin "deploy-infra.sh" 3
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_init_unseal_uses_stdin
test_deploy_infra_uses_stdin

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
