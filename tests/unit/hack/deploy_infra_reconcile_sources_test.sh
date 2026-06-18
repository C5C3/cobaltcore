#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh `reconcile_helmrepository_sources` after the
# FluxInstance migration: instead of `flux reconcile source helm`, the
# bootstrap annotates each HelmRepository with reconcile.fluxcd.io/requestedAt
#
# DECISION: bats vs project-native bash test runner — same as the sibling
#   deploy_infra_preflight_test.sh; following the established assertions.sh
#   pattern.
#
# Usage: bash tests/unit/hack/deploy_infra_reconcile_sources_test.sh

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

# install_kubectl_stub <dir> <repos...>
# Writes a stub `kubectl` into <dir> that:
#   - on `kubectl get helmrepository ...`  → prints the space-joined <repos>
#   - on `kubectl annotate helmrepository/<name> ...` → appends one line per
#     invocation to ${KUBECTL_ANNOTATE_LOG}
# All other invocations exit 0 silently. The stub captures argv into the log
# in a fixed format so the test can assert on each annotate call.
install_kubectl_stub() {
  local dir="$1"
  shift
  mkdir -p "$dir"
  local repos="$*"
  local log_file="${KUBECTL_ANNOTATE_LOG}"

  cat >"$dir/kubectl" <<STUB
#!/bin/bash
# Stub kubectl for tests/unit/hack/deploy_infra_reconcile_sources_test.sh.
if [[ "\$1" == "get" && "\$2" == "helmrepository" ]]; then
  printf '%s' "${repos}"
  exit 0
fi
if [[ "\$1" == "annotate" && "\$2" == helmrepository/* ]]; then
  printf '%s\n' "annotate \$*" >>"${log_file}"
  exit 0
fi
exit 0
STUB
  chmod +x "$dir/kubectl"
}

# Source the script and call the function under test in a subshell with PATH
# pointing at the stub. Echoes combined stdout/stderr; returns the exit
# status.
run_reconcile() {
  local stub_dir="$1"
  (
    PATH="$stub_dir"
    export PATH KUBECTL_ANNOTATE_LOG
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    reconcile_helmrepository_sources
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test 1: happy path — annotate is invoked once per HelmRepository name
#
# ---------------------------------------------------------------------------
test_reconcile_annotates_each_repo() {
  echo "Test: reconcile_helmrepository_sources annotates each HelmRepository"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  KUBECTL_ANNOTATE_LOG="$tmp/annotate.log"
  : >"$KUBECTL_ANNOTATE_LOG"

  install_kubectl_stub "$tmp" "bitnami fluxcd-community jetstack"

  local output exit_code
  output="$(run_reconcile "$tmp")"
  exit_code=$?

  assert_eq "reconcile_helmrepository_sources exits 0 on happy path" "0" "$exit_code"
  assert_contains "log line announces reconcile step" "$output" "Reconciling HelmRepository sources..."

  local annotate_count
  annotate_count="$(wc -l <"$KUBECTL_ANNOTATE_LOG" | tr -d ' ')"
  assert_eq "kubectl annotate called once per HelmRepository (3 repos)" "3" "$annotate_count"

  assert_file_contains "annotate invoked for bitnami" \
    "$KUBECTL_ANNOTATE_LOG" "helmrepository/bitnami"
  assert_file_contains "annotate invoked for fluxcd-community" \
    "$KUBECTL_ANNOTATE_LOG" "helmrepository/fluxcd-community"
  assert_file_contains "annotate invoked for jetstack" \
    "$KUBECTL_ANNOTATE_LOG" "helmrepository/jetstack"

  assert_file_contains "annotate uses reconcile.fluxcd.io/requestedAt key" \
    "$KUBECTL_ANNOTATE_LOG" "reconcile.fluxcd.io/requestedAt="
  # Pattern is "overwrite" (not "--overwrite") because grep would interpret
  # the leading "--" as end-of-options and assert_file_contains has no escape
  # hatch for that.
  assert_file_contains "annotate passes --overwrite" \
    "$KUBECTL_ANNOTATE_LOG" "overwrite"
  assert_file_contains "annotate scopes to flux-system namespace" \
    "$KUBECTL_ANNOTATE_LOG" "flux-system"
}

# ---------------------------------------------------------------------------
# Test 2: empty list — when no HelmRepositories exist, no annotate is invoked
#
# ---------------------------------------------------------------------------
test_reconcile_empty_list_is_noop() {
  echo "Test: reconcile_helmrepository_sources is a no-op when list is empty"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  KUBECTL_ANNOTATE_LOG="$tmp/annotate.log"
  : >"$KUBECTL_ANNOTATE_LOG"

  install_kubectl_stub "$tmp" ""

  local output exit_code
  output="$(run_reconcile "$tmp")"
  exit_code=$?

  assert_eq "reconcile_helmrepository_sources exits 0 with empty list" "0" "$exit_code"
  assert_contains "log line announces reconcile step" "$output" "Reconciling HelmRepository sources..."

  local annotate_count
  annotate_count="$(wc -l <"$KUBECTL_ANNOTATE_LOG" | tr -d ' ')"
  assert_eq "kubectl annotate not invoked when no HelmRepositories exist" "0" "$annotate_count"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_reconcile_annotates_each_repo
test_reconcile_empty_list_is_noop

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
