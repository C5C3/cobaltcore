#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-dump-diagnostics.sh guards the chaos-daemon describe with a
# `kubectl get ns chaos-mesh` check (CC-0097, REQ-008). Chaos Mesh is now
# opt-in in the kind Quick Start; the diagnostic dumper must print an explicit
# SKIP line when the namespace is absent rather than silently swallowing the
# error, and must still emit the describe output when the namespace exists.
#
# Usage: bash tests/unit/hack/ci_dump_diagnostics_chaos_absent_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DUMP_SH="$PROJECT_ROOT/hack/ci-dump-diagnostics.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# write_kubectl_stub <dir> <ns_exit>
# Creates a kubectl shim in <dir> tightened to only the verbs the dump
# script under test invokes (CC-0097, REQ-008):
#   - `get ns chaos-mesh`                          → exits <ns_exit>
#   - `describe daemonset -n chaos-mesh chaos-daemon` → emits a stub marker
#   - infra-summary verbs the dump always runs (`get helmrelease`,
#     `get pods`, `get daemonsets`, `get events`, `api-resources`) → exit 0
#     silently so the script's `|| true`s pass through cleanly without
#     polluting the stdout the test asserts against.
# Any other invocation emits an `[kubectl-stub] unexpected invocation: …`
# line on stderr and exits non-zero — a future change that adds a kubectl
# call which is supposed to fail-fast (e.g. a required secret) will
# surface here instead of silently passing under a blanket exit 0.
write_kubectl_stub() {
  local dir="$1" ns_exit="$2"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<STUB
#!/bin/bash
# Test stub (CC-0097): tightened kubectl dispatcher — only the verbs the
# dump script under test actually invokes are recognised. See
# write_kubectl_stub() in tests/unit/hack/ci_dump_diagnostics_chaos_absent_test.sh
# for the rationale.
case "\$1" in
  get)
    case "\$2" in
      ns)
        if [ "\$3" = "chaos-mesh" ]; then
          exit ${ns_exit}
        fi
        ;;
      helmrelease|pods|daemonsets|events|fluxinstance,fluxreport)
        exit 0
        ;;
    esac
    ;;
  describe)
    if [ "\$2" = "daemonset" ] && [ "\$3" = "-n" ] \\
        && [ "\$4" = "chaos-mesh" ] && [ "\$5" = "chaos-daemon" ]; then
      echo "STUBBED-DESCRIBE-CHAOS-DAEMON"
      exit 0
    fi
    ;;
  api-resources)
    # Empty output — \`grep -q '^fluxinstances'\` returns non-zero, so the
    # FluxInstance/FluxReport block is skipped (kept out of the stdout
    # the test asserts against).
    exit 0
    ;;
esac
echo "[kubectl-stub] unexpected invocation: \$*" >&2
exit 64
STUB
  chmod +x "$dir/kubectl"

  # Also stub flux so the optional Flux logs block doesn't pollute stdout.
  cat >"$dir/flux" <<'STUB'
#!/bin/bash
exit 0
STUB
  chmod +x "$dir/flux"
}

# run_dump <stub_dir>
# Executes the dump script with PATH limited to <stub_dir> (plus /usr/bin and
# /bin so `tail`, `grep`, etc. are available). Echoes combined stdout/stderr;
# returns the script exit code.
run_dump() {
  local stub_dir="$1"
  (
    PATH="$stub_dir:/usr/bin:/bin"
    export PATH
    OPERATOR=""
    export OPERATOR
    bash "$DUMP_SH"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test A: chaos-mesh namespace ABSENT -> SKIP line, no describe output
# ---------------------------------------------------------------------------
test_skip_when_namespace_absent() {
  echo "Test: prints SKIP when chaos-mesh namespace is absent (CC-0097, REQ-008)"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  write_kubectl_stub "$tmp" 1

  local output exit_code
  output="$(run_dump "$tmp")"
  exit_code=$?

  assert_eq "dump script exits 0 when namespace absent" "0" "$exit_code"
  assert_contains "stdout contains explicit SKIP line" \
    "$output" "SKIP: chaos-mesh namespace not present"
  assert_not_contains "describe stub output is NOT emitted when namespace absent" \
    "$output" "STUBBED-DESCRIBE-CHAOS-DAEMON"
}

# ---------------------------------------------------------------------------
# Test B: chaos-mesh namespace PRESENT -> describe runs, no SKIP line
# ---------------------------------------------------------------------------
test_describe_when_namespace_present() {
  echo "Test: dumps describe output when chaos-mesh namespace present (CC-0097, REQ-008)"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  write_kubectl_stub "$tmp" 0

  local output exit_code
  output="$(run_dump "$tmp")"
  exit_code=$?

  assert_eq "dump script exits 0 when namespace present" "0" "$exit_code"
  assert_contains "stdout contains describe stub marker" \
    "$output" "STUBBED-DESCRIBE-CHAOS-DAEMON"
  assert_not_contains "stdout does NOT contain SKIP line when namespace present" \
    "$output" "SKIP: chaos-mesh namespace not present"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_skip_when_namespace_absent
test_describe_when_namespace_present

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
