#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the `helm dependency build` gating in hack/ci-deploy-operator.sh with a
# stubbed PATH:
#   - a pulled chart (CHART_DIR set) with a populated charts/ skips the build,
#     because its file:// dependency path does not resolve outside the repo;
#   - the in-repo chart (CHART_DIR unset) always runs `helm dependency build`
#     even when charts/ is already populated, so a stale vendored copy left by a
#     previous local run is re-built instead of silently reused;
#   - a pulled chart with an empty/absent charts/ still runs the build.
#
# The in-repo case is the regression guard: before the CHART_DIR gate, a
# populated charts/ alone was enough to skip the build for the in-repo chart.
#
# Follows the project-native bash test pattern (tests/lib/assertions.sh),
# mirroring tests/unit/hack/ci_fetch_released_operator_test.sh.
#
# Usage: bash tests/unit/hack/ci_deploy_operator_dependency_build_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_SH="$PROJECT_ROOT/hack/ci-deploy-operator.sh"
INREPO_CHART="$PROJECT_ROOT/operators/keystone/helm/keystone-operator"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stubs <dir>
# Writes helm/kubectl stubs into <dir>. The helm stub logs its full argv to
# $HELM_LOG and always succeeds. The kubectl stub returns non-zero for
# `get mutatingwebhookconfigurations` so the deploy script skips the webhook
# readiness wait (and its sleeps), and succeeds for everything else.
make_stubs() {
  local dir="$1"
  mkdir -p "$dir"

  cat >"$dir/helm" <<'STUB'
#!/bin/bash
echo "helm $*" >>"$HELM_LOG"
exit 0
STUB
  chmod +x "$dir/helm"

  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
if [ "${1:-}" = "get" ] && [ "${2:-}" = "mutatingwebhookconfigurations" ]; then
  exit 1
fi
exit 0
STUB
  chmod +x "$dir/kubectl"
}

# make_chart <dir>
# Materialises a minimal pulled-style chart under <dir>: a crds/ directory (the
# deploy script runs `kubectl apply -f <chart>/crds/`) and, when POPULATE_CHARTS
# is set, a vendored subchart under charts/.
make_chart() {
  local dir="$1"
  mkdir -p "$dir/crds"
  printf 'apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n' \
    >"$dir/crds/dummy.yaml"
  if [ "${POPULATE_CHARTS:-}" = "1" ]; then
    mkdir -p "$dir/charts"
    : >"$dir/charts/operator-library-0.0.0.tgz"
  fi
}

# cleanup_inrepo_charts <created_flag> <charts_dir>
# Removes the marker (and the charts/ dir) only when this test created them, so
# a real vendored copy is never clobbered.
cleanup_inrepo_charts() {
  if [ "${1:-}" = "1" ]; then
    rm -f "$2/.ci-deploy-operator-test-marker"
    rmdir "$2" 2>/dev/null || true
  fi
}

# run_deploy <stub_dir> <helm_log>
# Runs the deploy script with the stub dir prepended to PATH. CHART_DIR
# (optional) is inherited from the caller's environment so callers can select
# the pulled-chart vs in-repo path via a `CHART_DIR=... run_deploy ...` prefix.
run_deploy() {
  local stub_dir="$1" helm_log="$2"
  (
    PATH="$stub_dir:$PATH"
    export PATH
    export HELM_LOG="$helm_log"
    export OPERATOR="keystone"
    export IMAGE_REPO="ghcr.io/c5c3/keystone-operator"
    bash "$DEPLOY_SH"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test 1: pulled chart (CHART_DIR set) with vendored charts/ skips the build
# ---------------------------------------------------------------------------
test_pulled_chart_skips_build() {
  echo "Test: pulled chart with vendored charts/ skips dependency build"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_stubs "$tmp/bin"
  POPULATE_CHARTS=1 make_chart "$tmp/chart"

  local output exit_code
  output="$(CHART_DIR="$tmp/chart" run_deploy "$tmp/bin" "$tmp/helm.log")"
  exit_code=$?

  assert_eq "deploy exits 0 for a pulled chart" "0" "$exit_code"
  assert_contains "deploy logs the skip for the pulled chart" "$output" \
    "skipping dependency build"
  assert_file_not_contains "helm dependency build is NOT invoked for the pulled chart" \
    "$tmp/helm.log" "dependency build"
}

# ---------------------------------------------------------------------------
# Test 2: in-repo chart (CHART_DIR unset) rebuilds even with populated charts/
#
# Regression guard: the in-repo chart's charts/ is gitignored and vendored on
# demand, so on a developer re-run it can be populated. The old presence-only
# check skipped the build here, silently reusing a possibly-stale copy. Ensure
# charts/ is populated for the duration of this test (create+clean only when it
# was absent, to avoid clobbering a real vendored copy).
# ---------------------------------------------------------------------------
test_inrepo_chart_rebuilds() {
  echo "Test: in-repo chart rebuilds dependencies even when charts/ is populated"

  local tmp
  tmp="$(mktemp -d)"

  local created_charts=0
  if [ ! -d "$INREPO_CHART/charts" ] || [ -z "$(ls -A "$INREPO_CHART/charts" 2>/dev/null)" ]; then
    mkdir -p "$INREPO_CHART/charts"
    : >"$INREPO_CHART/charts/.ci-deploy-operator-test-marker"
    created_charts=1
  fi
  # The RETURN trap runs in this function's context, so it sees $created_charts.
  trap 'rm -rf "$tmp"; cleanup_inrepo_charts "$created_charts" "$INREPO_CHART/charts"' RETURN

  make_stubs "$tmp/bin"

  # CHART_DIR intentionally unset: the deploy script falls back to the in-repo
  # chart path and must run `helm dependency build` unconditionally.
  local output exit_code
  output="$(unset CHART_DIR; run_deploy "$tmp/bin" "$tmp/helm.log")"
  exit_code=$?

  assert_eq "deploy exits 0 for the in-repo chart" "0" "$exit_code"
  assert_file_contains "helm dependency build IS invoked for the in-repo chart" \
    "$tmp/helm.log" "dependency build"
}

# ---------------------------------------------------------------------------
# Test 3: pulled chart (CHART_DIR set) with an empty charts/ still builds
# ---------------------------------------------------------------------------
test_pulled_chart_empty_charts_builds() {
  echo "Test: pulled chart with an empty charts/ still runs dependency build"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_stubs "$tmp/bin"
  make_chart "$tmp/chart" # POPULATE_CHARTS unset -> no charts/ vendored

  local output exit_code
  output="$(CHART_DIR="$tmp/chart" run_deploy "$tmp/bin" "$tmp/helm.log")"
  exit_code=$?

  assert_eq "deploy exits 0 for a pulled chart without vendored deps" "0" "$exit_code"
  assert_file_contains "helm dependency build IS invoked when charts/ is empty" \
    "$tmp/helm.log" "dependency build"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_pulled_chart_skips_build
test_inrepo_chart_rebuilds
test_pulled_chart_empty_charts_builds

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
