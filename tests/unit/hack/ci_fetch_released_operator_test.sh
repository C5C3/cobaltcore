#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-fetch-released-operator.sh behaviour with a stubbed PATH:
#   - a failed `helm pull` surfaces an actionable ::error:: and exits non-zero;
#   - the success path resolves the chart version and emits chart_dir to
#     GITHUB_OUTPUT.
#
# Follows the project-native bash test pattern (tests/lib/assertions.sh),
# mirroring tests/unit/hack/deploy_infra_preflight_test.sh.
#
# Usage: bash tests/unit/hack/ci_fetch_released_operator_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
FETCH_SH="$PROJECT_ROOT/hack/ci-fetch-released-operator.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stubs <dir>
# Writes helm/docker/kind stubs into <dir>. The helm stub honours HELM_PULL_FAIL
# to simulate a pull failure, otherwise it materialises a minimal untarred chart
# with a version so the script can resolve it. docker/kind stubs succeed.
make_stubs() {
  local dir="$1"
  mkdir -p "$dir"

  cat >"$dir/helm" <<'STUB'
#!/bin/bash
if [ "${1:-}" = "pull" ]; then
  if [ "${HELM_PULL_FAIL:-}" = "1" ]; then
    exit 1
  fi
  untardir=""
  while [ $# -gt 0 ]; do
    if [ "$1" = "--untardir" ]; then
      untardir="$2"
    fi
    shift
  done
  mkdir -p "$untardir/keystone-operator"
  printf 'version: 9.9.9\nname: keystone-operator\n' >"$untardir/keystone-operator/Chart.yaml"
  exit 0
fi
exit 0
STUB
  chmod +x "$dir/helm"

  cat >"$dir/docker" <<'STUB'
#!/bin/bash
exit 0
STUB
  chmod +x "$dir/docker"

  cat >"$dir/kind" <<'STUB'
#!/bin/bash
exit 0
STUB
  chmod +x "$dir/kind"
}

# run_fetch <stub_dir> <gh_output_file>
# Runs the fetch script with the stub dir prepended to PATH (so coreutils still
# resolve) and GITHUB_OUTPUT pointed at <gh_output_file>. Echoes combined
# stdout/stderr; returns the script's exit status.
run_fetch() {
  local stub_dir="$1" gh_output="$2"
  (
    PATH="$stub_dir:$PATH"
    export PATH
    GITHUB_OUTPUT="$gh_output"
    export GITHUB_OUTPUT
    bash "$FETCH_SH"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test 1: a failed helm pull surfaces an actionable ::error:: and exits 1
# ---------------------------------------------------------------------------
test_pull_failure_errors() {
  echo "Test: failed helm pull exits non-zero with ::error::"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_stubs "$tmp"

  local output exit_code
  output="$(HELM_PULL_FAIL=1 run_fetch "$tmp" "$tmp/gh_output")"
  exit_code=$?

  assert_nonzero_exit "fetch exits non-zero when helm pull fails" "$exit_code"
  assert_contains "fetch emits a ::error:: about the failed chart pull" "$output" "::error::failed to pull"
}

# ---------------------------------------------------------------------------
# Test 2: success path emits chart_dir and resolves the chart version
# ---------------------------------------------------------------------------
test_success_emits_chart_dir() {
  echo "Test: success path resolves version and emits chart_dir"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"; rm -rf "$PROJECT_ROOT/_output/operator-upgrade"' RETURN

  make_stubs "$tmp"

  local output exit_code
  output="$(run_fetch "$tmp" "$tmp/gh_output")"
  exit_code=$?

  assert_eq "fetch exits 0 on the success path" "0" "$exit_code"
  assert_contains "fetch echoes the resolved chart version" "$output" "Resolved released chart version: 9.9.9"
  assert_file_contains "fetch writes chart_dir to GITHUB_OUTPUT" "$tmp/gh_output" \
    "chart_dir=_output/operator-upgrade/keystone-operator"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_pull_failure_errors
test_success_emits_chart_dir

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
