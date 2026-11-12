#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the mount guards of setup-secret-engines.sh and setup-auth.sh read the
# whole mount list before they decide whether a mount exists.
#
# Each guard once piped `jq -r 'keys[]'` straight into `grep -qx`. grep exits at
# its first match, and a jq that still has output to write then dies of SIGPIPE
# (exit 141), which pipefail turns into a false condition. jq 1.8 writes its
# first result before `kubectl exec` closes its output and the rest afterwards,
# so on a re-run the check of the mount that sorts first (barbican/ among the
# engines, approle/ among the auth mounts) reported that mount absent. The
# script then failed on an enable OpenBao refuses: "path is already in use".
#
# CI runs jq 1.7, which writes all of its output when it exits, so the race
# never shows there. The jq on PATH in these tests is therefore a shim: it runs
# the real jq and delivers its output the way jq 1.8 does, the first line at
# once and the rest after a pause. The filtering is still the real jq's. A guard
# that reads the whole list passes whatever the timing; a guard that stops at
# the first match fails here on any jq.
#
# Usage: bash tests/unit/deploy/bases/shared-openbao_mount_guard_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
ENGINES_SCRIPT="$PROJECT_ROOT/deploy/bases/shared-openbao/bootstrap/setup-secret-engines.sh"
AUTH_SCRIPT="$PROJECT_ROOT/deploy/bases/shared-openbao/bootstrap/setup-auth.sh"

# The engine and auth maps of an instance both scripts have already run
# against: every mount they enable, plus the ones OpenBao always carries.
ENGINES_BOOTSTRAPPED='{"barbican/":{},"cubbyhole/":{},"database/mariadb/":{},"identity/":{},"kv-v2/":{},"pki/":{},"sys/":{}}'
AUTH_BOOTSTRAPPED='{"approle/":{},"kubernetes/control-plane/":{},"kubernetes/hypervisor/":{},"kubernetes/management/":{},"kubernetes/storage/":{},"token/":{}}'

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# Resolved before any stub directory goes onto PATH, so the shim never finds
# itself.
REAL_JQ="$(command -v jq || true)"

# make_stubs <dir>
# Installs two stubs in <dir>:
#
#   - kubectl records every invocation in <dir>/calls.log. `bao secrets list`
#     and `bao auth list` answer the bootstrapped maps above, which make_stubs
#     writes to <dir>/engines.json and <dir>/auth.json. An enable is refused
#     the way OpenBao refuses one on a path that is already mounted, since every
#     mount either script enables is in those maps. Everything else (tunes,
#     config and role writes) answers exit 0. With OPENBAO_UNREACHABLE set,
#     every invocation fails the way a dropped `kubectl exec` does.
#   - jq runs $REAL_JQ and writes its first output line, waits, and writes the
#     rest: the write pattern of jq 1.8 behind a `kubectl exec`.
make_stubs() {
  local dir="$1"
  mkdir -p "$dir"
  printf '%s\n' "$ENGINES_BOOTSTRAPPED" >"$dir/engines.json"
  printf '%s\n' "$AUTH_BOOTSTRAPPED" >"$dir/auth.json"

  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
printf '%s\n' "$*" >>"$STUB_DIR/calls.log"

if [[ -n "${OPENBAO_UNREACHABLE:-}" ]]; then
  echo 'error: unable to upgrade connection: container not found ("openbao")' >&2
  exit 1
fi

case "$*" in
  *"bao secrets list"*)
    cat "$STUB_DIR/engines.json"
    ;;
  *"bao auth list"*)
    cat "$STUB_DIR/auth.json"
    ;;
  *"bao secrets enable"* | *"bao auth enable"*)
    path=""
    for arg in "$@"; do
      case "$arg" in
        -path=*) path="${arg#-path=}" ;;
      esac
    done
    printf 'Error enabling: Error making API request.\n\nCode: 400. Errors:\n\n* path is already in use at %s/\n' "$path" >&2
    exit 2
    ;;
esac
exit 0
STUB

  cat >"$dir/jq" <<'STUB'
#!/bin/bash
out="$("$REAL_JQ" "$@")" || exit
[[ -n "$out" ]] || exit 0
printf '%s\n' "${out%%$'\n'*}"
[[ "$out" == *$'\n'* ]] || exit 0
sleep 0.2
printf '%s\n' "${out#*$'\n'}"
STUB

  chmod +x "$dir/kubectl" "$dir/jq"
}

# run_bootstrap <stub_dir> <script>
# Runs a bootstrap script with the stubs prepended to PATH. Echoes the combined
# stdout/stderr; returns the script's exit code.
run_bootstrap() {
  local stub_dir="$1" script="$2"
  (
    PATH="$stub_dir:$PATH"
    BAO_TOKEN="dummy-token"
    STUB_DIR="$stub_dir"
    export PATH BAO_TOKEN STUB_DIR REAL_JQ
    bash "$script"
  ) 2>&1
}

# need_jq counts one SKIP and fails when jq is not on PATH.
need_jq() {
  if [[ -z "$REAL_JQ" ]]; then
    echo "  SKIP: jq not installed, the scripts under test cannot run"
    SKIP=$((SKIP + 1))
    return 1
  fi
}

# ---------------------------------------------------------------------------
# Test: a re-run of the engine setup skips every mounted engine
# ---------------------------------------------------------------------------
test_engine_rerun_skips_every_mounted_engine() {
  echo "Test: setup-secret-engines.sh re-run skips every engine that is already mounted"
  need_jq || return

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_stubs "$tmp"

  local output exit_code calls
  output="$(run_bootstrap "$tmp" "$ENGINES_SCRIPT")"
  exit_code=$?
  calls="$(cat "$tmp/calls.log")"

  assert_eq "the re-run succeeds" "0" "$exit_code"
  assert_not_contains "no engine is enabled a second time" \
    "$calls" "bao secrets enable"
  assert_contains "barbican/, the first mount in the list, is found" \
    "$output" "Secret engine already enabled at barbican/."
  assert_contains "the run reports success" "$output" "=== Done ==="
}

# ---------------------------------------------------------------------------
# Test: a re-run of the auth setup skips every mounted auth method
# ---------------------------------------------------------------------------
test_auth_rerun_skips_every_mounted_auth_method() {
  echo "Test: setup-auth.sh re-run skips every auth mount that is already enabled"
  need_jq || return

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_stubs "$tmp"

  local output exit_code calls
  output="$(run_bootstrap "$tmp" "$AUTH_SCRIPT")"
  exit_code=$?
  calls="$(cat "$tmp/calls.log")"

  assert_eq "the re-run succeeds" "0" "$exit_code"
  assert_not_contains "no auth method is enabled a second time" \
    "$calls" "bao auth enable"
  assert_contains "approle/, the first mount in the list, is found" \
    "$output" "Auth mount 'approle/' already enabled"
  assert_contains "the kubernetes mounts are found" \
    "$output" "Auth mount 'kubernetes/control-plane/' already enabled"
  assert_contains "the run reports success" "$output" "=== Done ==="
}

# ---------------------------------------------------------------------------
# Test: an unreadable auth list stops the auth setup before any enable
# ---------------------------------------------------------------------------
# A list the script cannot read says nothing about which mounts exist. Reading
# it as empty would send an enable for every mount to an OpenBao that may
# already have them.
test_auth_setup_stops_on_an_unreadable_list() {
  echo "Test: setup-auth.sh stops before any enable when the auth list cannot be read"
  need_jq || return

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_stubs "$tmp"

  local output exit_code calls
  output="$(OPENBAO_UNREACHABLE=1 run_bootstrap "$tmp" "$AUTH_SCRIPT")"
  exit_code=$?
  calls="$(cat "$tmp/calls.log")"

  assert_nonzero_exit "the script exits non-zero" "$exit_code"
  assert_contains "the failure names the list it could not read" \
    "$output" "ERROR: could not list the auth mounts"
  assert_not_contains "no auth method is enabled" "$calls" "bao auth enable"
  assert_not_contains "the run does not report success" "$output" "=== Done ==="
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_engine_rerun_skips_every_mounted_engine
test_auth_rerun_skips_every_mounted_auth_method
test_auth_setup_stops_on_an_unreadable_list

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
