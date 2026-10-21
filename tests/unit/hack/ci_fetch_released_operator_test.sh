#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-fetch-released-operator.sh with stubbed helm, docker and kind
# on PATH and a scratch pin file behind BASELINE_FILE:
#   - the chart and the image of the pinned version are pulled for the named
#     operator, and chart_dir and image_tag reach GITHUB_OUTPUT;
#   - a pin equal to the checkout's Chart.yaml version prints the ::notice::
#     that it still moves, and a lower pin prints that it is frozen;
#   - a missing or malformed OPERATOR and a missing, empty or non-X.Y.Z pin
#     fail before anything is pulled;
#   - a failed chart pull, a pulled chart of another version and a missing
#     image each fail with their own ::error:: line;
#   - a re-run clears the chart directory a previous run left.
#
# Follows the project-native bash test pattern (tests/lib/assertions.sh),
# mirroring tests/unit/hack/deploy_infra_preflight_test.sh.
#
# Usage: bash tests/unit/hack/ci_fetch_released_operator_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
FETCH_SH="$PROJECT_ROOT/hack/ci-fetch-released-operator.sh"
GLANCE_CHART_DIR="$PROJECT_ROOT/_output/operator-upgrade/glance-operator"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stubs <dir>
# Writes helm/docker/kind stubs into <dir>. Each logs its argv to $CALL_LOG.
# `helm pull` writes <untardir>/<chart>/Chart.yaml with the --version it was
# given, or $PULLED_VERSION when set; HELM_PULL_FAIL=1 makes it print two lines
# and exit 1 instead. `docker pull` prints "manifest unknown" under
# DOCKER_MISSING=1 and a network error under DOCKER_FAIL=1, and exits 1 for
# both. kind succeeds.
make_stubs() {
  local dir="$1"
  mkdir -p "$dir"

  cat >"$dir/helm" <<'STUB'
#!/bin/bash
echo "helm $*" >>"$CALL_LOG"
if [ "${1:-}" = "pull" ]; then
  if [ "${HELM_PULL_FAIL:-}" = "1" ]; then
    echo "pulling the chart"
    echo "Error: boom"
    exit 1
  fi
  ref="$2" version="" untardir=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --version) version="$2" ;;
      --untardir) untardir="$2" ;;
    esac
    shift
  done
  name="${ref##*/}"
  mkdir -p "$untardir/$name"
  printf 'apiVersion: v2\nname: %s\nversion: %s\n' "$name" "${PULLED_VERSION:-$version}" \
    >"$untardir/$name/Chart.yaml"
  echo "Pulled: $ref:$version"
fi
exit 0
STUB
  chmod +x "$dir/helm"

  cat >"$dir/docker" <<'STUB'
#!/bin/bash
echo "docker $*" >>"$CALL_LOG"
if [ "${DOCKER_MISSING:-}" = "1" ]; then
  echo "Error response from daemon: manifest unknown"
  exit 1
fi
if [ "${DOCKER_FAIL:-}" = "1" ]; then
  echo "Error response from daemon: Get \"https://ghcr.io/v2/\": dial tcp: i/o timeout"
  exit 1
fi
exit 0
STUB
  chmod +x "$dir/docker"

  cat >"$dir/kind" <<'STUB'
#!/bin/bash
echo "kind $*" >>"$CALL_LOG"
exit 0
STUB
  chmod +x "$dir/kind"
}

# new_tmp — a scratch directory with the stubs under bin/, an empty call log and
# an empty GITHUB_OUTPUT file.
new_tmp() {
  local tmp
  tmp="$(mktemp -d)"
  make_stubs "$tmp/bin"
  : >"$tmp/calls.log"
  : >"$tmp/gh_output"
  echo "$tmp"
}

# write_pin <file> <version line>
write_pin() {
  printf '# scratch pin\n%s\n' "$2" >"$1"
}

# run_fetch <tmp> [ENV=value ...]
# Runs the fetch script with the stubs of <tmp> prepended to PATH (so coreutils
# still resolve), GITHUB_OUTPUT and CALL_LOG inside <tmp>, OPERATOR=glance and
# BASELINE_FILE=<tmp>/baseline.yaml unless the arguments override them. Echoes
# combined stdout/stderr; returns the script's exit status.
run_fetch() {
  local tmp="$1"
  shift
  (
    PATH="$tmp/bin:$PATH"
    export PATH
    export CALL_LOG="$tmp/calls.log"
    export GITHUB_OUTPUT="$tmp/gh_output"
    export OPERATOR=glance
    export BASELINE_FILE="$tmp/baseline.yaml"
    for assignment in "$@"; do
      # shellcheck disable=SC2163 # deliberate: export the NAME=value argument
      export "$assignment"
    done
    bash "$FETCH_SH"
  ) 2>&1
}

# glance_chart_version — the version the checkout's glance Chart.yaml names.
glance_chart_version() {
  grep -E '^version:' "$PROJECT_ROOT/operators/glance/helm/glance-operator/Chart.yaml" |
    head -1 | awk '{print $2}' | tr -d '"'
}

cleanup() {
  rm -rf "$1" "$GLANCE_CHART_DIR"
}

# ---------------------------------------------------------------------------
# Test 1: the pinned chart and image are pulled, and both outputs are written
# ---------------------------------------------------------------------------
test_pinned_pull() {
  echo "Test: OPERATOR=glance with a 0.6.0 pin pulls the pinned chart and image"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'cleanup "$tmp"' RETURN
  write_pin "$tmp/baseline.yaml" 'version: "0.6.0"'

  output="$(run_fetch "$tmp")"
  rc=$?

  assert_eq "fetch exits 0" "0" "$rc"
  assert_file_contains_fixed "helm pulls the pinned chart" "$tmp/calls.log" \
    "helm pull oci://ghcr.io/c5c3/charts/glance-operator --version 0.6.0 --untar --untardir _output/operator-upgrade"
  assert_file_contains_fixed "docker pulls the pinned image" "$tmp/calls.log" \
    "docker pull ghcr.io/c5c3/glance-operator:0.6.0"
  assert_file_contains_fixed "kind loads the pinned image" "$tmp/calls.log" \
    "kind load docker-image ghcr.io/c5c3/glance-operator:0.6.0 --name cobaltcore"
  assert_eq "GITHUB_OUTPUT holds chart_dir and image_tag" \
    "chart_dir=_output/operator-upgrade/glance-operator
image_tag=0.6.0" "$(cat "$tmp/gh_output")"
  assert_contains "fetch echoes the resolved chart version" "$output" \
    "Resolved released chart version: 0.6.0"
}

# ---------------------------------------------------------------------------
# Test 2: the freshness line tells a moving pin from a frozen one
# ---------------------------------------------------------------------------
test_freshness_line() {
  echo "Test: a pin equal to the checkout's Chart.yaml version still moves; a lower one is frozen"
  local tmp output current
  tmp="$(new_tmp)"
  trap 'cleanup "$tmp"' RETURN
  current="$(glance_chart_version)"

  write_pin "$tmp/baseline.yaml" "version: \"${current}\""
  output="$(run_fetch "$tmp")"
  assert_contains "the current version prints the ::notice::" "$output" \
    "::notice::baseline glance-operator ${current} is the version main still publishes; its chart and image move with every push until Chart.yaml moves past ${current}"
  assert_not_contains "it does not call the pin frozen" "$output" "is frozen"

  write_pin "$tmp/baseline.yaml" 'version: "0.0.1"'
  output="$(run_fetch "$tmp")"
  assert_contains "a lower version is frozen" "$output" \
    "Baseline glance-operator 0.0.1 is frozen (main publishes ${current})"
  assert_not_contains "it prints no ::notice::" "$output" "::notice::"
}

# ---------------------------------------------------------------------------
# Test 3: OPERATOR is required and must be a plain name
# ---------------------------------------------------------------------------
test_operator_required() {
  echo "Test: a missing or malformed OPERATOR fails before any call"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'cleanup "$tmp"' RETURN
  write_pin "$tmp/baseline.yaml" 'version: "0.6.0"'

  output="$(run_fetch "$tmp" OPERATOR=)"
  rc=$?
  assert_eq "fetch exits 1 without OPERATOR" "1" "$rc"
  assert_contains "it names the missing OPERATOR" "$output" "::error::OPERATOR is required (e.g. keystone)"
  assert_eq "the stubs record no call" "" "$(cat "$tmp/calls.log")"

  output="$(run_fetch "$tmp" "OPERATOR=Glance;x")"
  rc=$?
  assert_eq "fetch exits 1 on a malformed OPERATOR" "1" "$rc"
  assert_contains "it quotes the malformed value" "$output" \
    "::error::OPERATOR must be lowercase alphanumeric (with hyphens), got 'Glance;x'"
  assert_eq "the stubs still record no call" "" "$(cat "$tmp/calls.log")"
}

# ---------------------------------------------------------------------------
# Test 4: a missing, empty or non-X.Y.Z pin fails without pulling
# ---------------------------------------------------------------------------
test_pin_checks() {
  echo "Test: a missing, empty or non-X.Y.Z pin fails without pulling"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'cleanup "$tmp"' RETURN

  output="$(run_fetch "$tmp")"
  rc=$?
  assert_eq "a missing pin exits 1" "1" "$rc"
  assert_contains "it names the missing file" "$output" "::error::no baseline pin at $tmp/baseline.yaml"
  assert_eq "nothing is pulled for a missing pin" "" "$(cat "$tmp/calls.log")"

  : >"$tmp/baseline.yaml"
  output="$(run_fetch "$tmp")"
  rc=$?
  assert_eq "an empty pin exits 1" "1" "$rc"
  assert_contains "it names the file without a version" "$output" "::error::$tmp/baseline.yaml has no version"
  assert_eq "nothing is pulled for an empty pin" "" "$(cat "$tmp/calls.log")"

  write_pin "$tmp/baseline.yaml" 'version: "latest"'
  output="$(run_fetch "$tmp")"
  rc=$?
  assert_eq "a non-X.Y.Z pin exits 1" "1" "$rc"
  assert_contains "it quotes the value" "$output" "::error::$tmp/baseline.yaml: version 'latest' is not X.Y.Z"
  assert_eq "nothing is pulled for a non-X.Y.Z pin" "" "$(cat "$tmp/calls.log")"
}

# ---------------------------------------------------------------------------
# Test 5: a failed chart pull names the chart, the version and helm's last line
# ---------------------------------------------------------------------------
test_chart_pull_failure() {
  echo "Test: a failed helm pull exits 1 with helm's last line"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'cleanup "$tmp"' RETURN
  write_pin "$tmp/baseline.yaml" 'version: "0.6.0"'

  output="$(run_fetch "$tmp" HELM_PULL_FAIL=1)"
  rc=$?
  assert_eq "fetch exits 1" "1" "$rc"
  assert_contains "the error names the chart, the version and helm's last line" "$output" \
    "::error::failed to pull oci://ghcr.io/c5c3/charts/glance-operator --version 0.6.0: Error: boom"
  assert_contains "helm's whole output is printed too" "$output" "pulling the chart"
  assert_file_not_contains "no image is pulled" "$tmp/calls.log" "docker pull"
  assert_eq "nothing is written to GITHUB_OUTPUT" "" "$(cat "$tmp/gh_output")"
}

# ---------------------------------------------------------------------------
# Test 6: a pulled chart of another version is refused
# ---------------------------------------------------------------------------
test_pulled_version_mismatch() {
  echo "Test: a pulled chart of version 0.5.0 against a 0.6.0 pin is refused"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'cleanup "$tmp"' RETURN
  write_pin "$tmp/baseline.yaml" 'version: "0.6.0"'

  output="$(run_fetch "$tmp" PULLED_VERSION=0.5.0)"
  rc=$?
  assert_eq "fetch exits 1" "1" "$rc"
  assert_contains "the error names both versions" "$output" \
    "::error::pulled chart reports version 0.5.0, pinned 0.6.0"
  assert_file_not_contains "no image is pulled" "$tmp/calls.log" "docker pull"
}

# ---------------------------------------------------------------------------
# Test 7: a missing image names the tag; any other failure keeps its message
# ---------------------------------------------------------------------------
test_image_pull_failures() {
  echo "Test: a missing image and a failed image pull fail with their own messages"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'cleanup "$tmp"' RETURN
  write_pin "$tmp/baseline.yaml" 'version: "0.6.0"'

  output="$(run_fetch "$tmp" DOCKER_MISSING=1)"
  rc=$?
  assert_eq "a missing image exits 1" "1" "$rc"
  assert_contains "the error says the tag does not exist and why" "$output" \
    "::error::ghcr.io/c5c3/glance-operator:0.6.0 does not exist; main tags every operator image it publishes with its chart version since #1495, so pin a version main has published since"
  assert_file_not_contains "nothing is loaded into kind" "$tmp/calls.log" "kind load"

  output="$(run_fetch "$tmp" DOCKER_FAIL=1)"
  rc=$?
  assert_eq "a failed image pull exits 1" "1" "$rc"
  assert_contains "a network failure keeps the generic message" "$output" \
    "::error::failed to pull ghcr.io/c5c3/glance-operator:0.6.0"
  assert_not_contains "it does not claim the tag is missing" "$output" "does not exist"
  assert_contains "docker's output is printed" "$output" "dial tcp: i/o timeout"
}

# ---------------------------------------------------------------------------
# Test 8: a re-run clears what a previous run left in the chart directory
# ---------------------------------------------------------------------------
test_rerun_clears_the_chart_dir() {
  echo "Test: a re-run clears the chart directory first"
  local tmp rc
  tmp="$(new_tmp)"
  trap 'cleanup "$tmp"' RETURN
  write_pin "$tmp/baseline.yaml" 'version: "0.6.0"'
  mkdir -p "$GLANCE_CHART_DIR"
  : >"$GLANCE_CHART_DIR/stale-template.yaml"

  run_fetch "$tmp" >/dev/null
  rc=$?
  assert_eq "fetch exits 0" "0" "$rc"
  if [ -e "$GLANCE_CHART_DIR/stale-template.yaml" ]; then
    assert_eq "the stale file of the previous run is gone" "absent" "present"
  else
    assert_eq "the stale file of the previous run is gone" "absent" "absent"
  fi
  assert_file_contains "the fresh chart is in place" "$GLANCE_CHART_DIR/Chart.yaml" "^version: 0.6.0$"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_pinned_pull
test_freshness_line
test_operator_required
test_pin_checks
test_chart_pull_failure
test_pulled_version_mismatch
test_image_pull_failures
test_rerun_clears_the_chart_dir

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
