#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/verify-helm-released-values.sh with a stubbed `helm pull`:
#   - the in-repo chart renders with released values it accepts, and fails on a
#     released key its schema rejects;
#   - a waiver strips the key before the render, every entry of a chart applies,
#     a list-index key deletes the element, and a stale waiver warns;
#   - one failing chart fails the run, and the summary counts every chart;
#   - with no operator, every chart of the glob is checked;
#   - an absent, empty, comment-only or {} waiver list, a zero-byte released
#     values.yaml and a released chart without one all pass;
#   - a missing or stale operator-library subchart is vendored afresh, and a
#     failed vendoring fails the chart before the pull;
#   - an unpublished chart is skipped after one pull, a failing pull is tried
#     three times, a flaky pull recovers on the second attempt, and a pulled
#     chart without Chart.yaml fails;
#   - a malformed waiver list, an unknown operator or a bad PULL_RETRY_DELAY
#     exits 1 before anything is pulled;
#   - a generic OUT_DIR in the environment is left alone.
#
# The helm stub answers `pull` from FAKE_PULL and appends one line per pull to
# PULL_COUNTER, and fails `dependency build` when FAKE_DEP is error; every other
# subcommand runs the real helm, so the render is the one CI runs. The fake released chart is version 9.9.9, its values.yaml a copy
# or a yq edit of the in-repo one. Every case is skipped when helm or
# mikefarah/yq is absent.
#
# Usage: bash tests/unit/hack/verify_helm_released_values_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
VERIFY_SH="$PROJECT_ROOT/hack/verify-helm-released-values.sh"
KEYSTONE_CHART="$PROJECT_ROOT/operators/keystone/helm/keystone-operator"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# Resolved before any stub directory is put on PATH.
REAL_HELM="$(command -v helm || true)"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# have_tools: return 0 when helm and mikefarah/yq are installed, else count a
# skip for the calling case.
have_tools() {
  if [ -n "$REAL_HELM" ] && grep -q mikefarah <<<"$(yq --version 2>&1)"; then
    return 0
  fi
  echo "  SKIP: helm or mikefarah/yq not installed"
  SKIP=$((SKIP + 1))
  return 1
}

# make_stub_helm <dir>
# Writes a helm stub into <dir>/bin. `helm pull <ref> --untar --untardir <d>`
# counts itself in PULL_COUNTER, refuses an existing <d>/<name> as the real helm
# does, then follows FAKE_PULL:
#   ok       copies FAKE_RELEASED_DIR/<name> to <d>/<name>;
#   notfound prints the registry's 404 name unknown and exits 1;
#   error    prints a DNS failure and exits 1;
#   flaky    on the first pull creates <d>/<name> and exits 1, then acts as ok.
# `helm dependency build` prints a lock error and exits 1 when FAKE_DEP is error.
make_stub_helm() {
  mkdir -p "$1/bin"
  cat >"$1/bin/helm" <<'STUB'
#!/bin/bash
if [ "${1:-}" = "dependency" ] && [ "${FAKE_DEP:-}" = "error" ]; then
  echo "Error: the lock file (Chart.lock) is out of sync with the dependencies file (Chart.yaml)"
  exit 1
fi
if [ "${1:-}" != "pull" ]; then
  exec "$REAL_HELM" "$@"
fi
name="${2##*/}"
untardir=""
while [ $# -gt 0 ]; do
  if [ "$1" = "--untardir" ]; then
    untardir="$2"
  fi
  shift
done
echo pull >>"$PULL_COUNTER"
if [ -e "$untardir/$name" ]; then
  echo "Error: failed to untar: a file or directory with the name $untardir/$name already exists"
  exit 1
fi
case "$FAKE_PULL" in
  notfound)
    echo "Error: GET \"https://ghcr.io/v2/c5c3/charts/$name/tags/list\": response status code 404: name unknown: repository name not known to registry"
    exit 1
    ;;
  error)
    echo "Error: dial tcp: lookup ghcr.io: no such host"
    exit 1
    ;;
  flaky)
    if [ "$(wc -l <"$PULL_COUNTER")" -eq 1 ]; then
      mkdir -p "$untardir/$name"
      exit 1
    fi
    ;;
esac
mkdir -p "$untardir"
cp -R "$FAKE_RELEASED_DIR/$name" "$untardir/$name"
STUB
  chmod +x "$1/bin/helm"
}

# make_released_chart <tmp> <op> [yq-expr]
# Writes the fake released <op>-operator chart, version 9.9.9, under
# <tmp>/released. Its values.yaml is the in-repo one, edited by yq-expr if given.
make_released_chart() {
  local dir="$1/released/$2-operator"
  local in_repo="$PROJECT_ROOT/operators/$2/helm/$2-operator/values.yaml"
  mkdir -p "$dir"
  printf 'apiVersion: v2\nname: %s-operator\nversion: 9.9.9\n' "$2" >"$dir/Chart.yaml"
  if [ -n "${3:-}" ]; then
    yq "$3" "$in_repo" >"$dir/values.yaml"
  else
    cp "$in_repo" "$dir/values.yaml"
  fi
}

# new_case: echo a fresh case directory holding the helm stub.
new_case() {
  local tmp
  tmp="$(mktemp -d)"
  make_stub_helm "$tmp"
  echo "$tmp"
}

# run_verify <tmp> <operator...>
# Runs the script with the stub dir prepended to PATH, its scratch dir, waiver
# list and pull counter under <tmp>, and PULL_RETRY_DELAY=0 unless the caller
# sets one. FAKE_PULL defaults to ok. Echoes combined stdout/stderr; returns
# the script's exit status.
run_verify() {
  local tmp="$1"
  shift
  (
    PATH="$tmp/bin:$PATH"
    RELEASED_VALUES_OUT_DIR="$tmp/out"
    REMOVED_VALUES_FILE="${REMOVED_VALUES_FILE:-$tmp/waivers.yaml}"
    PULL_RETRY_DELAY="${PULL_RETRY_DELAY:-0}"
    FAKE_PULL="${FAKE_PULL:-ok}"
    FAKE_RELEASED_DIR="$tmp/released"
    PULL_COUNTER="$tmp/pulls"
    export PATH RELEASED_VALUES_OUT_DIR REMOVED_VALUES_FILE PULL_RETRY_DELAY \
      FAKE_PULL FAKE_RELEASED_DIR PULL_COUNTER REAL_HELM
    bash "$VERIFY_SH" "$@"
  ) 2>&1
}

# pulls <tmp>: echo how many pulls the stub counted.
pulls() {
  if [ -f "$1/pulls" ]; then
    wc -l <"$1/pulls" | tr -d ' '
  else
    echo 0
  fi
}

# ---------------------------------------------------------------------------
# Script, the happy path
# ---------------------------------------------------------------------------

test_identical_released_values_render_ok() {
  echo "Test: released values equal to the in-repo values render OK"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN
  make_released_chart "$tmp" keystone

  output="$(run_verify "$tmp" keystone)"
  exit_code=$?

  assert_eq "the script exits 0" "0" "$exit_code"
  assert_contains "the chart renders OK" "$output" "keystone-operator 9.9.9: OK"
  assert_eq "the chart is pulled once" "1" "$(pulls "$tmp")"
}

# ---------------------------------------------------------------------------
# Script, the rejection and its waiver
# ---------------------------------------------------------------------------

test_unknown_released_key_fails_the_render() {
  echo "Test: a released key the schema rejects fails the chart"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN
  make_released_chart "$tmp" keystone '.webhook.oldName = true'

  output="$(run_verify "$tmp" keystone)"
  exit_code=$?

  assert_eq "the script exits 1" "1" "$exit_code"
  assert_contains "helm's schema error names the key" "$output" "oldName"
  assert_contains "the script names the rejecting chart" "$output" \
    "::error::keystone-operator rejects the values of released keystone-operator 9.9.9"
  assert_contains "the error sends a removed key to the waiver list" "$output" \
    "If this pull request removes or renames a key the released values set, keep it or waive it in"
  assert_contains "the error tells a key main added apart from a change in this PR" "$output" \
    "if the rejected key is one main added after this run's base, rebase onto main instead of adding a waiver"
  assert_not_contains "the chart is not reported OK" "$output" "keystone-operator 9.9.9: OK"
}

test_constraint_violating_released_value_fails_the_render() {
  echo "Test: a released value the schema constraint rejects fails the chart"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN
  make_released_chart "$tmp" keystone '.replicas = 0'

  output="$(run_verify "$tmp" keystone)"
  exit_code=$?

  assert_eq "the script exits 1" "1" "$exit_code"
  assert_contains "helm's schema error names the key" "$output" "/replicas"
  assert_contains "the error tells a narrowed key to keep the old contract" "$output" \
    "if it narrows a key or adds a template check the released values fail, keep the old contract;"
  assert_not_contains "the error does not send a narrowed key to the waiver list" "$output" \
    "narrows a key the released values set, or adds a template check they fail, keep the old contract or waive"
  assert_not_contains "the chart is not reported OK" "$output" "keystone-operator 9.9.9: OK"
}

test_waived_key_is_stripped_before_the_render() {
  echo "Test: a waived key is deleted from the released values before the render"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN
  make_released_chart "$tmp" keystone '.webhook.oldName = true'
  printf 'keystone-operator:\n  - key: webhook.oldName\n    reason: renamed to webhook.newName\n' >"$tmp/waivers.yaml"

  output="$(run_verify "$tmp" keystone)"
  exit_code=$?

  assert_eq "the script exits 0" "0" "$exit_code"
  assert_contains "the waiver is announced with its reason" "$output" \
    "::notice::keystone-operator: webhook.oldName waived (renamed to webhook.newName)"
  assert_contains "the chart renders OK" "$output" "keystone-operator 9.9.9: OK"
}

test_every_waiver_entry_is_applied_per_chart() {
  echo "Test: every waiver entry of a chart is applied, other charts are untouched"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN
  make_released_chart "$tmp" keystone '.webhook.oldName = true | .webhook.otherOld = 1'
  make_released_chart "$tmp" barbican
  printf 'keystone-operator:\n  - key: webhook.oldName\n    reason: renamed\n  - key: webhook.otherOld\n    reason: removed\n' >"$tmp/waivers.yaml"

  output="$(run_verify "$tmp" keystone barbican)"
  exit_code=$?

  assert_eq "the script exits 0" "0" "$exit_code"
  assert_contains "the first entry is applied" "$output" \
    "::notice::keystone-operator: webhook.oldName waived (renamed)"
  assert_contains "the second entry is applied" "$output" \
    "::notice::keystone-operator: webhook.otherOld waived (removed)"
  assert_contains "keystone renders OK" "$output" "keystone-operator 9.9.9: OK"
  assert_contains "barbican renders OK" "$output" "barbican-operator 9.9.9: OK"
  assert_not_contains "barbican gets no waiver message" "$output" "barbican-operator:"
}

test_list_index_waiver_deletes_the_element() {
  echo "Test: a waiver key with a list index deletes that element"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN
  make_released_chart "$tmp" keystone '.extraArgs = ["--old-flag"]'
  printf 'keystone-operator:\n  - key: extraArgs.0\n    reason: flag removed\n' >"$tmp/waivers.yaml"

  output="$(run_verify "$tmp" keystone)"
  exit_code=$?

  assert_eq "the script exits 0" "0" "$exit_code"
  assert_contains "the waiver is announced" "$output" \
    "::notice::keystone-operator: extraArgs.0 waived (flag removed)"
  assert_eq "the element is deleted from the rendered values" "0" \
    "$(yq '.extraArgs | length' "$tmp/out/keystone-operator.values.yaml")"
}

test_stale_waiver_warns_and_passes() {
  echo "Test: a waiver for a key the released values lack warns and passes"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN
  make_released_chart "$tmp" keystone
  printf 'keystone-operator:\n  - key: webhook.gone\n    reason: removed\n' >"$tmp/waivers.yaml"

  output="$(run_verify "$tmp" keystone)"
  exit_code=$?

  assert_eq "the script exits 0" "0" "$exit_code"
  assert_contains "the stale entry raises a warning" "$output" \
    "::warning::keystone-operator: webhook.gone is not in the values of released keystone-operator 9.9.9"
  assert_contains "the warning names the waiver list to edit" "$output" \
    "delete its entry from $tmp/waivers.yaml"
}

test_one_failing_chart_fails_the_run_and_counts_both() {
  echo "Test: one rejecting chart fails the run after every chart is checked"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN
  make_released_chart "$tmp" keystone '.webhook.oldName = true'
  make_released_chart "$tmp" barbican

  output="$(run_verify "$tmp" keystone barbican)"
  exit_code=$?

  assert_eq "the script exits 1" "1" "$exit_code"
  assert_contains "keystone is reported as rejecting" "$output" \
    "::error::keystone-operator rejects the values of released keystone-operator 9.9.9"
  assert_contains "barbican is still checked and passes" "$output" "barbican-operator 9.9.9: OK"
  assert_eq "the run ends on the failure count" \
    "::error::1 of 2 operator charts failed the released-values render" \
    "$(tail -n 1 <<<"$output")"
}

test_no_argument_checks_every_operator_chart() {
  echo "Test: with no operator every chart of the glob is checked"
  have_tools || return

  local tmp output exit_code chart expected=0
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(FAKE_PULL=notfound run_verify "$tmp")"
  exit_code=$?

  assert_eq "the script exits 0" "0" "$exit_code"
  for chart in "$PROJECT_ROOT"/operators/*/helm/*-operator; do
    expected=$((expected + 1))
    assert_contains "$(basename "$chart") is checked" "$output" \
      "::notice::$(basename "$chart") has no released chart"
  done
  assert_gte "the glob holds more than one chart" "$expected" 2
  assert_eq "every chart is pulled once" "$expected" "$(pulls "$tmp")"
}

# ---------------------------------------------------------------------------
# Script, empty and absent input
# ---------------------------------------------------------------------------

test_absent_waiver_file_means_no_waivers() {
  echo "Test: an absent waiver list holds no waivers"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN
  make_released_chart "$tmp" keystone

  output="$(REMOVED_VALUES_FILE="$tmp/does-not-exist.yaml" run_verify "$tmp" keystone)"
  exit_code=$?

  assert_eq "the script exits 0" "0" "$exit_code"
  assert_contains "the chart renders OK" "$output" "keystone-operator 9.9.9: OK"
  assert_not_contains "no key is waived" "$output" "waived"
  assert_not_contains "no waiver warns" "$output" "::warning::"
}

test_empty_comment_and_braces_waiver_files_mean_no_waivers() {
  echo "Test: an empty, comment-only or {} waiver list holds no waivers"
  have_tools || return

  local tmp content output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN
  make_released_chart "$tmp" keystone

  for content in "" "# only a comment" "{}"; do
    if [ -n "$content" ]; then
      echo "$content" >"$tmp/waivers.yaml"
    else
      : >"$tmp/waivers.yaml"
    fi
    output="$(run_verify "$tmp" keystone)"
    exit_code=$?
    assert_eq "the script exits 0 with '${content}'" "0" "$exit_code"
    assert_contains "the chart renders OK with '${content}'" "$output" "keystone-operator 9.9.9: OK"
    assert_not_contains "no waiver message with '${content}'" "$output" "waived"
  done
}

test_zero_byte_released_values_render_ok() {
  echo "Test: a zero-byte released values.yaml renders OK"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN
  make_released_chart "$tmp" keystone
  : >"$tmp/released/keystone-operator/values.yaml"

  output="$(run_verify "$tmp" keystone)"
  exit_code=$?

  assert_eq "the script exits 0" "0" "$exit_code"
  assert_contains "the chart renders OK" "$output" "keystone-operator 9.9.9: OK"
}

test_released_chart_without_values_is_skipped() {
  echo "Test: a released chart without values.yaml is skipped with a notice"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN
  make_released_chart "$tmp" keystone
  rm "$tmp/released/keystone-operator/values.yaml"

  output="$(run_verify "$tmp" keystone)"
  exit_code=$?

  assert_eq "the script exits 0" "0" "$exit_code"
  assert_contains "the skip is announced" "$output" \
    "::notice::released keystone-operator 9.9.9 ships no values.yaml; nothing to compare"
  assert_not_contains "nothing is rendered" "$output" ": OK"
}

test_missing_subchart_is_vendored_before_the_render() {
  echo "Test: a chart without its operator-library subchart is vendored, then rendered"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  # Puts the moved-aside subchart back unless the script vendored a new one, so
  # the working copy keeps its subchart whatever the outcome.
  trap 'compgen -G "$KEYSTONE_CHART/charts/operator-library-*.tgz" >/dev/null ||
    { [ -d "$tmp/aside" ] && mv "$tmp"/aside/operator-library-*.tgz "$KEYSTONE_CHART/charts/"; }
    rm -rf "$tmp"' RETURN
  make_released_chart "$tmp" keystone
  if compgen -G "$KEYSTONE_CHART/charts/operator-library-*.tgz" >/dev/null; then
    mkdir -p "$tmp/aside"
    mv "$KEYSTONE_CHART"/charts/operator-library-*.tgz "$tmp/aside/"
  fi

  output="$(run_verify "$tmp" keystone)"
  exit_code=$?

  assert_eq "the script exits 0" "0" "$exit_code"
  assert_contains "the chart renders OK" "$output" "keystone-operator 9.9.9: OK"
  assert_eq "the subchart is vendored after the run" "yes" \
    "$(compgen -G "$KEYSTONE_CHART/charts/operator-library-*.tgz" >/dev/null && echo yes || echo no)"
}

test_stale_subchart_is_vendored_afresh() {
  echo "Test: a stale operator-library subchart is vendored afresh before the render"
  have_tools || return

  local tmp output exit_code tgz
  tmp="$(new_case)"
  # Vendors the subchart from its source again, so the working copy never keeps
  # the stale tarball this case writes. helm loads charts/ before it vendors, so
  # the tarball is deleted first.
  trap 'rm -f "$KEYSTONE_CHART"/charts/operator-library-*.tgz
    "$REAL_HELM" dependency build --skip-refresh "$KEYSTONE_CHART" >/dev/null 2>&1
    rm -rf "$tmp"' RETURN
  make_released_chart "$tmp" keystone
  "$REAL_HELM" dependency build --skip-refresh "$KEYSTONE_CHART" >/dev/null 2>&1
  tgz="$(compgen -G "$KEYSTONE_CHART/charts/operator-library-*.tgz" | head -n 1)"
  # helm package, not tar: BSD tar adds AppleDouble entries that helm rejects.
  mkdir -p "$tmp/stale"
  tar -xzf "$tgz" -C "$tmp/stale"
  : >"$tmp/stale/operator-library/STALE"
  "$REAL_HELM" package "$tmp/stale/operator-library" --destination "$(dirname "$tgz")" >/dev/null 2>&1

  output="$(run_verify "$tmp" keystone)"
  exit_code=$?

  assert_eq "the script exits 0" "0" "$exit_code"
  assert_contains "the chart renders OK" "$output" "keystone-operator 9.9.9: OK"
  assert_eq "the stale tarball is replaced" "0" "$(tar -tzf "$tgz" | grep -c '/STALE$')"
}

test_failed_vendoring_fails_the_chart_before_the_pull() {
  echo "Test: a failed vendoring of the subchart fails the chart before the pull"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN
  make_released_chart "$tmp" keystone

  output="$(FAKE_DEP=error run_verify "$tmp" keystone)"
  exit_code=$?

  assert_eq "the script exits 1" "1" "$exit_code"
  assert_contains "the error names the chart" "$output" \
    "::error::failed to vendor the operator-library subchart into operators/keystone/helm/keystone-operator"
  assert_eq "nothing is pulled" "0" "$(pulls "$tmp")"
  assert_not_contains "nothing is rendered" "$output" ": OK"
}

# ---------------------------------------------------------------------------
# Script, upstream errors and bad input
# ---------------------------------------------------------------------------

test_unpublished_chart_is_skipped_after_one_pull() {
  echo "Test: a chart the registry does not know is skipped after one pull"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(FAKE_PULL=notfound run_verify "$tmp" keystone)"
  exit_code=$?

  assert_eq "the script exits 0" "0" "$exit_code"
  assert_contains "the skip is announced" "$output" \
    "::notice::keystone-operator has no released chart at oci://ghcr.io/c5c3/charts/keystone-operator; nothing to compare"
  assert_eq "the pull is not retried" "1" "$(pulls "$tmp")"
}

test_pull_failure_is_retried_three_times_then_fails() {
  echo "Test: a failing pull is tried three times, then fails the chart"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(FAKE_PULL=error run_verify "$tmp" keystone)"
  exit_code=$?

  assert_eq "the script exits 1" "1" "$exit_code"
  assert_contains "the error carries helm's last line" "$output" \
    "::error::failed to pull oci://ghcr.io/c5c3/charts/keystone-operator after 3 attempts: Error: dial tcp: lookup ghcr.io: no such host"
  assert_eq "the chart is pulled three times" "3" "$(pulls "$tmp")"
}

test_flaky_pull_succeeds_on_the_second_attempt() {
  echo "Test: a pull that leaves its directory behind succeeds on the retry"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN
  make_released_chart "$tmp" keystone

  output="$(FAKE_PULL=flaky run_verify "$tmp" keystone)"
  exit_code=$?

  assert_eq "the script exits 0" "0" "$exit_code"
  assert_not_contains "the retry starts from a clean directory" "$output" "failed to untar"
  assert_eq "the chart is pulled twice" "2" "$(pulls "$tmp")"
  assert_contains "the chart renders OK" "$output" "keystone-operator 9.9.9: OK"
}

test_pulled_chart_without_chart_yaml_fails() {
  echo "Test: a pulled chart without Chart.yaml fails the chart"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN
  make_released_chart "$tmp" keystone
  rm "$tmp/released/keystone-operator/Chart.yaml"

  output="$(run_verify "$tmp" keystone)"
  exit_code=$?

  assert_eq "the script exits 1" "1" "$exit_code"
  assert_contains "the error names the missing file" "$output" \
    "::error::pulled keystone-operator has no Chart.yaml under $tmp/out/keystone-operator/keystone-operator"
  assert_not_contains "nothing is rendered" "$output" ": OK"
}

test_waiver_key_that_is_not_a_dotted_path_is_rejected() {
  echo "Test: a waiver key that is not a dotted path is rejected before any pull"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN
  # shellcheck disable=SC2016 # the literal $(id) is the hostile key under test
  printf 'keystone-operator:\n  - key: webhook.$(id)\n    reason: hostile\n' >"$tmp/waivers.yaml"

  output="$(run_verify "$tmp" keystone)"
  exit_code=$?

  assert_eq "the script exits 1" "1" "$exit_code"
  assert_contains "the error names the configured file and the key" "$output" \
    "::error::$tmp/waivers.yaml: keystone-operator key 'webhook.\$(id)' is not a dotted values path"
  assert_eq "nothing is pulled" "0" "$(pulls "$tmp")"
}

test_waiver_for_unknown_chart_is_rejected() {
  echo "Test: a waiver for a chart outside the glob is rejected before any pull"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN
  printf 'nosuch-operator:\n  - key: webhook.oldName\n    reason: renamed\n' >"$tmp/waivers.yaml"

  output="$(run_verify "$tmp" keystone)"
  exit_code=$?

  assert_eq "the script exits 1" "1" "$exit_code"
  assert_contains "the error names the chart" "$output" "::error::$tmp/waivers.yaml names unknown chart nosuch-operator"
  assert_eq "nothing is pulled" "0" "$(pulls "$tmp")"
}

test_waiver_entry_without_reason_is_rejected() {
  echo "Test: a waiver entry with an empty reason is rejected before any pull"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN
  printf 'keystone-operator:\n  - key: webhook.oldName\n    reason: ""\n' >"$tmp/waivers.yaml"

  output="$(run_verify "$tmp" keystone)"
  exit_code=$?

  assert_eq "the script exits 1" "1" "$exit_code"
  assert_contains "the error names the entry" "$output" \
    "::error::$tmp/waivers.yaml: keystone-operator entry 0 needs a key and a non-empty reason"
  assert_eq "nothing is pulled" "0" "$(pulls "$tmp")"
}

test_waiver_file_with_list_top_level_is_rejected() {
  echo "Test: a waiver list whose top level is a list is rejected before any pull"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN
  printf -- '- keystone-operator\n' >"$tmp/waivers.yaml"

  output="$(run_verify "$tmp" keystone)"
  exit_code=$?

  assert_eq "the script exits 1" "1" "$exit_code"
  assert_contains "the error names the expected shape" "$output" \
    "::error::$tmp/waivers.yaml must map chart names to lists of {key, reason} entries"
  assert_eq "nothing is pulled" "0" "$(pulls "$tmp")"
}

test_malformed_waiver_lists_are_rejected() {
  echo "Test: malformed waiver lists are rejected before any pull"
  have_tools || return

  local tmp content want output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN
  # Each line is the waiver list (printf %b) and the error it must raise.
  while IFS='|' read -r content want; do
    printf '%b' "$content" >"$tmp/waivers.yaml"
    rm -f "$tmp/pulls"
    output="$(run_verify "$tmp" keystone)"
    exit_code=$?
    assert_eq "exits 1 for '$content'" "1" "$exit_code"
    assert_contains "names the defect for '$content'" "$output" "::error::$tmp/waivers.yaml$want"
    assert_eq "nothing is pulled for '$content'" "0" "$(pulls "$tmp")"
  done <<'EOF'
keystone-operator: [unclosed\n| must map chart names to lists of {key, reason} entries
keystone-operator:\n|: keystone-operator must be a list of {key, reason} entries
keystone-operator:\n  key: webhook.oldName\n  reason: renamed\n|: keystone-operator must be a list of {key, reason} entries
keystone-operator:\n  - reason: renamed\n|: keystone-operator entry 0 needs a key and a non-empty reason
keystone-operator:\n  - webhook.oldName\n|: keystone-operator entry 0 needs a key and a non-empty reason
keystone-operator:\n  - key: webhook.oldName\n    reason: renamed\n  - key: webhook.otherOld\n    reason: ""\n|: keystone-operator entry 1 needs a key and a non-empty reason
EOF
}

test_unknown_operator_argument_is_rejected() {
  echo "Test: an operator without a chart is rejected before any pull"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(run_verify "$tmp" nosuch)"
  exit_code=$?

  assert_eq "the script exits 1" "1" "$exit_code"
  assert_contains "the error names the missing chart" "$output" \
    "::error::no operator chart at operators/nosuch/helm/nosuch-operator"
  assert_eq "nothing is pulled" "0" "$(pulls "$tmp")"
}

test_non_numeric_pull_retry_delay_is_rejected() {
  echo "Test: a PULL_RETRY_DELAY that is not a whole number is rejected before any pull"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN
  make_released_chart "$tmp" keystone

  output="$(PULL_RETRY_DELAY=abc run_verify "$tmp" keystone)"
  exit_code=$?

  assert_eq "the script exits 1" "1" "$exit_code"
  assert_contains "the error quotes the value" "$output" \
    "::error::PULL_RETRY_DELAY must be a whole number of seconds (got: abc)"
  assert_eq "nothing is pulled" "0" "$(pulls "$tmp")"
}

test_generic_out_dir_is_left_alone() {
  echo "Test: an OUT_DIR in the environment is not cleared"
  have_tools || return

  local tmp output exit_code
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN
  make_released_chart "$tmp" keystone
  mkdir -p "$tmp/precious"
  : >"$tmp/precious/keep"

  output="$(OUT_DIR="$tmp/precious" run_verify "$tmp" keystone)"
  exit_code=$?

  assert_eq "the script exits 0" "0" "$exit_code"
  assert_eq "the OUT_DIR file survives" "yes" "$([ -f "$tmp/precious/keep" ] && echo yes || echo no)"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_identical_released_values_render_ok
test_unknown_released_key_fails_the_render
test_constraint_violating_released_value_fails_the_render
test_waived_key_is_stripped_before_the_render
test_every_waiver_entry_is_applied_per_chart
test_list_index_waiver_deletes_the_element
test_stale_waiver_warns_and_passes
test_one_failing_chart_fails_the_run_and_counts_both
test_no_argument_checks_every_operator_chart
test_absent_waiver_file_means_no_waivers
test_empty_comment_and_braces_waiver_files_mean_no_waivers
test_zero_byte_released_values_render_ok
test_released_chart_without_values_is_skipped
test_missing_subchart_is_vendored_before_the_render
test_stale_subchart_is_vendored_afresh
test_failed_vendoring_fails_the_chart_before_the_pull
test_unpublished_chart_is_skipped_after_one_pull
test_pull_failure_is_retried_three_times_then_fails
test_flaky_pull_succeeds_on_the_second_attempt
test_pulled_chart_without_chart_yaml_fails
test_waiver_key_that_is_not_a_dotted_path_is_rejected
test_waiver_for_unknown_chart_is_rejected
test_waiver_entry_without_reason_is_rejected
test_waiver_file_with_list_top_level_is_rejected
test_malformed_waiver_lists_are_rejected
test_unknown_operator_argument_is_rejected
test_non_numeric_pull_retry_delay_is_rejected
test_generic_out_dir_is_left_alone

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
