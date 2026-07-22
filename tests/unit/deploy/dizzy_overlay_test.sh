#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the opt-in dizzy observability overlay (deploy/kind/dizzy):
#   - all 7 tracked overlay files carry the repo SPDX header.
#   - with the three dashboard JSONs staged into the git-ignored
#     deploy/kind/dizzy/dashboards/ dir, `kustomize build deploy/kind/dizzy`
#     renders exactly the expected resource set under the default
#     LoadRestrictionsRootOnly security check (no --load-restrictor flag):
#     one Namespace (dizzy), two HelmRepositories (victoria-metrics + grafana in
#     flux-system), two HelmReleases (dizzy-victoria-metrics + dizzy-grafana in
#     dizzy), one HTTPRoute (dizzy-grafana), and one ConfigMap
#     (grafana-dashboards) wrapping the three dashboard files.
#   - with dashboards/ absent, `kustomize build` FAILS — the documented staging
#     contract (hack/dizzy.sh stage-dashboards must run first).
#   - the victoria-metrics-single chart version pins to a single 0.x minor
#     window (>=0.N.x <0.(N+1).0), never a range spanning the whole 0.x line,
#     since a 0.x chart may break on any minor bump.
#
# STAGED-ASSET SAFETY a developer may have REAL dashboards staged at
# deploy/kind/dizzy/dashboards/ (hack/dizzy.sh stage-dashboards writes them
# there). This test moves any pre-existing dir aside up front and restores it
# via an EXIT trap, so it never destroys staged assets and never fails
# spuriously; the placeholder JSONs it stages for the render assertions are
# `{}` and are removed again afterwards.
#
# Usage: bash tests/unit/deploy/dizzy_overlay_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

DIZZY_DIR="$PROJECT_ROOT/deploy/kind/dizzy"
DASHBOARDS_DIR="$DIZZY_DIR/dashboards"
DASHBOARD_FILES=(overview.json api-operations.json time-to-ready.json)

# Backup location for a developer's pre-existing dashboards/ dir, empty when
# none existed at start.
DASHBOARDS_BACKUP=""

# ---------------------------------------------------------------------------
# Dashboards-dir guard — never destroy developer-staged assets.
# ---------------------------------------------------------------------------
# Move any pre-existing dashboards/ dir into a temp backup so the render tests
# operate on a known-empty slate. Recorded in DASHBOARDS_BACKUP for restore.
guard_setup_dashboards() {
  if [[ -e "$DASHBOARDS_DIR" ]]; then
    DASHBOARDS_BACKUP="$(mktemp -d)"
    mv "$DASHBOARDS_DIR" "$DASHBOARDS_BACKUP/dashboards"
  fi
}

# Remove any test-created dashboards/ dir, then restore the developer's original
# (moved aside in guard_setup_dashboards) byte-for-byte. Registered on EXIT so a
# mid-test failure still restores it.
guard_restore_dashboards() {
  rm -rf "$DASHBOARDS_DIR"
  if [[ -n "$DASHBOARDS_BACKUP" && -d "$DASHBOARDS_BACKUP/dashboards" ]]; then
    mv "$DASHBOARDS_BACKUP/dashboards" "$DASHBOARDS_DIR"
    rmdir "$DASHBOARDS_BACKUP" 2>/dev/null || true
  fi
}

# Stage the three placeholder dashboard JSONs so the configMapGenerator's
# `files:` resolve. Content is `{}` — the render assertions only care that the
# files exist and become ConfigMap data keys.
stage_placeholder_dashboards() {
  mkdir -p "$DASHBOARDS_DIR"
  local f
  for f in "${DASHBOARD_FILES[@]}"; do
    printf '{}' > "$DASHBOARDS_DIR/$f"
  done
}

# Count documents of a given kind in a rendered manifest stream read on stdin.
count_kind() {
  local kind="$1"
  grep -cE "^kind: ${kind}\$" 2>/dev/null || true
}

# --- Test 1: all 7 tracked overlay files carry the SPDX header ---
test_overlay_files_have_spdx() {
  echo "Test: every tracked deploy/kind/dizzy file carries the SPDX header"

  local tracked
  tracked="$(git -C "$PROJECT_ROOT" ls-files 'deploy/kind/dizzy')"

  local count
  count="$(printf '%s\n' "$tracked" | grep -c . || true)"
  assert_eq "deploy/kind/dizzy has exactly 7 tracked files" "7" "$count"

  local f
  while IFS= read -r f; do
    [[ -z "$f" ]] && continue
    assert_file_contains "$(basename "$f") has SPDX FileCopyrightText header" \
      "$PROJECT_ROOT/$f" "SPDX-FileCopyrightText: Copyright 2026 SAP SE"
    assert_file_contains "$(basename "$f") has SPDX-License-Identifier: Apache-2.0" \
      "$PROJECT_ROOT/$f" "SPDX-License-Identifier: Apache-2.0"
  done <<< "$tracked"
}

# --- Test 2: kustomize build FAILS when dashboards/ is unstaged ---
test_build_fails_without_dashboards() {
  echo "Test: kustomize build deploy/kind/dizzy FAILS when dashboards/ is absent"

  if ! command -v kustomize >/dev/null 2>&1; then
    echo "  SKIP: kustomize not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  # Guarantee absence regardless of test order.
  rm -rf "$DASHBOARDS_DIR"

  local output exit_code
  output="$(kustomize build "$DIZZY_DIR" 2>&1)"
  exit_code=$?
  assert_nonzero_exit "kustomize build fails without the staged dashboards/ dir" "$exit_code"
}

# --- Test 3: kustomize build renders the expected set with dashboards staged ---
test_build_renders_expected_set() {
  echo "Test: kustomize build deploy/kind/dizzy renders the expected resource set"

  if ! command -v kustomize >/dev/null 2>&1; then
    echo "  SKIP: kustomize not installed (13 checks skipped)"
    SKIP=$((SKIP + 13))
    return
  fi
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (13 checks skipped)"
    SKIP=$((SKIP + 13))
    return
  fi

  stage_placeholder_dashboards

  # Mirror the production invocation: NO --load-restrictor flag. kubectl's
  # embedded kustomize (used by hack/deploy-infra.sh) does not expose one.
  local rendered
  if ! rendered="$(kustomize build "$DIZZY_DIR" 2>&1)"; then
    echo "  FAIL: kustomize build $DIZZY_DIR failed (default LoadRestrictionsRootOnly):"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 13))
    rm -rf "$DASHBOARDS_DIR"
    return
  fi

  # Namespace: exactly one, named dizzy.
  assert_eq "renders exactly one Namespace" \
    "1" "$(printf '%s\n' "$rendered" | count_kind Namespace)"
  assert_eq "the Namespace is named dizzy" \
    "dizzy" "$(printf '%s\n' "$rendered" | yq -r 'select(.kind == "Namespace") | .metadata.name' | head -n1)"

  # HelmRepositories: exactly two, both in flux-system.
  assert_eq "renders exactly two HelmRepositories" \
    "2" "$(printf '%s\n' "$rendered" | count_kind HelmRepository)"
  assert_eq "HelmRepository/victoria-metrics lives in flux-system" \
    "flux-system" "$(printf '%s\n' "$rendered" | yq -r 'select(.kind == "HelmRepository" and .metadata.name == "victoria-metrics") | .metadata.namespace' | head -n1)"
  assert_eq "HelmRepository/grafana lives in flux-system" \
    "flux-system" "$(printf '%s\n' "$rendered" | yq -r 'select(.kind == "HelmRepository" and .metadata.name == "grafana") | .metadata.namespace' | head -n1)"

  # HelmReleases: exactly two, both in dizzy.
  assert_eq "renders exactly two HelmReleases" \
    "2" "$(printf '%s\n' "$rendered" | count_kind HelmRelease)"
  assert_eq "HelmRelease/dizzy-victoria-metrics lives in dizzy" \
    "dizzy" "$(printf '%s\n' "$rendered" | yq -r 'select(.kind == "HelmRelease" and .metadata.name == "dizzy-victoria-metrics") | .metadata.namespace' | head -n1)"
  assert_eq "HelmRelease/dizzy-grafana lives in dizzy" \
    "dizzy" "$(printf '%s\n' "$rendered" | yq -r 'select(.kind == "HelmRelease" and .metadata.name == "dizzy-grafana") | .metadata.namespace' | head -n1)"

  # HTTPRoute: exactly one, named dizzy-grafana in dizzy.
  assert_eq "renders exactly one HTTPRoute" \
    "1" "$(printf '%s\n' "$rendered" | count_kind HTTPRoute)"
  assert_eq "HTTPRoute/dizzy-grafana lives in dizzy" \
    "dizzy" "$(printf '%s\n' "$rendered" | yq -r 'select(.kind == "HTTPRoute" and .metadata.name == "dizzy-grafana") | .metadata.namespace' | head -n1)"

  # ConfigMap: exactly one, named grafana-dashboards, with the three data keys.
  assert_eq "renders exactly one ConfigMap" \
    "1" "$(printf '%s\n' "$rendered" | count_kind ConfigMap)"
  assert_eq "the ConfigMap is named grafana-dashboards in dizzy" \
    "dizzy" "$(printf '%s\n' "$rendered" | yq -r 'select(.kind == "ConfigMap" and .metadata.name == "grafana-dashboards") | .metadata.namespace' | head -n1)"

  local keys
  keys="$(printf '%s\n' "$rendered" | yq -r 'select(.kind == "ConfigMap" and .metadata.name == "grafana-dashboards") | .data | keys | .[]' | sort | tr '\n' ' ' | sed 's/ $//')"
  assert_eq "the ConfigMap wraps the three dashboard JSONs" \
    "api-operations.json overview.json time-to-ready.json" "$keys"

  # Remove the placeholders we staged (the EXIT trap is the safety net).
  rm -rf "$DASHBOARDS_DIR"
}

# --- Test 4: the victoria-metrics chart pins to a single 0.x minor ---
test_victoria_metrics_version_is_single_minor() {
  echo "Test: the dizzy-victoria-metrics chart version pins to a single 0.x minor"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local version
  version="$(yq -r '.spec.chart.spec.version' "$DIZZY_DIR/release-victoria-metrics.yaml")"

  # victoria-metrics-single is a 0.x chart, so SemVer permits breaking changes
  # on every minor bump and the constraint MUST bound a single minor
  # (>=0.N.x <0.(N+1).0). A range spanning the whole 0.x line (>=0.40.0 <1.0.0)
  # lets Flux auto-pull a breaking minor that can rename the server Service the
  # hard-coded Grafana datasource URL depends on — silent "no data" dashboards.
  if [[ "$version" =~ ^\>=0\.([0-9]+)\.[0-9]+\ \<0\.([0-9]+)\.0$ ]]; then
    local lower_minor="${BASH_REMATCH[1]}" upper_minor="${BASH_REMATCH[2]}"
    assert_eq "the version window spans exactly one 0.x minor ($version)" \
      "$((lower_minor + 1))" "$upper_minor"
  else
    echo "  FAIL: version '$version' is not a single 0.x minor window (>=0.N.x <0.(N+1).0)"
    FAIL=$((FAIL + 1))
  fi
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
guard_setup_dashboards
trap guard_restore_dashboards EXIT

test_overlay_files_have_spdx
test_build_fails_without_dashboards
test_build_renders_expected_set
test_victoria_metrics_version_is_single_minor

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
