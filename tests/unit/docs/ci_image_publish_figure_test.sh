#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the image publish figure names the jobs it draws, and that each of
# them is a job of .github/workflows/build-images.yaml.
#
# docs/diagrams/ci-image-publish.drawio and .svg draw the seven jobs of JOBS.
# The jobs of a workflow are the keys at two spaces of indent after the line
# `jobs:`, up to the next line that starts in column 0 with anything but a
# comment. check_publish_figure prints `not a job of build-images.yaml: <job>`
# for a job of JOBS the workflow lacks, and `not in drawio: <job>` or
# `not in svg: <job>` for one that is not a label of its own in that file: in
# the .drawio a cell value, in the .svg a <text> element. It prints nothing
# for a consistent tree. The check runs one way: a job of the workflow the
# figure does not draw is not reported. The fixture tests prove each message
# on a scratch tree before the last test runs the check on the repository.
#
# Usage: bash tests/unit/docs/ci_image_publish_figure_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

FIGURE="ci-image-publish"
JOBS="nix-flake-check nix-lock-check generate-matrix build-service-images
test-service-images merge-service-images verify-service-images"

# Print one job key per line: the keys at two spaces of indent after `jobs:`.
# shellcheck disable=SC2016 # an awk program, not shell
JOBS_AWK='
/^jobs:[ \t]*(#.*)?$/ { in_jobs = 1; next }
!in_jobs { next }
/^[^ \t#]/ { in_jobs = 0; next }
/^  [A-Za-z0-9_-]+:[ \t]*(#.*)?$/ {
  job = $0
  sub(/^  /, "", job)
  sub(/:.*$/, "", job)
  print job
}
'

# check_publish_figure <build-images.yaml> <diagrams-dir>: print one problem
# per line, sorted, and nothing when the workflow has every job of JOBS and
# both files of the figure draw each as a label of its own. A missing or
# jobless workflow is the only line.
check_publish_figure() {
  local workflow="$1" dir="$2"
  local drawio="$dir/$FIGURE.drawio" svg="$dir/$FIGURE.svg"

  if [[ ! -f "$workflow" ]]; then
    echo "missing $workflow"
    return
  fi
  local jobs
  jobs="$(awk "$JOBS_AWK" "$workflow")"
  if [[ -z "$jobs" ]]; then
    echo "no job in $workflow"
    return
  fi

  {
    [[ -f "$drawio" ]] || echo "missing figure: $FIGURE.drawio"
    [[ -f "$svg" ]] || echo "missing figure: $FIGURE.svg"
    local job
    for job in $JOBS; do
      if ! grep -qxF -- "$job" <<<"$jobs"; then
        echo "not a job of build-images.yaml: $job"
      fi
      if [[ -f "$drawio" ]] && ! grep -F -q -- "value=\"$job\"" "$drawio"; then
        echo "not in drawio: $job"
      fi
      if [[ -f "$svg" ]] && ! grep -F -q -- ">$job<" "$svg"; then
        echo "not in svg: $job"
      fi
    done
  } | LC_ALL=C sort
}

# ---------------------------------------------------------------------------
# Fixture
# ---------------------------------------------------------------------------

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

# build_fixture: write a consistent tree and print its root. build-images.yaml
# has two-space keys before `jobs:`, the seven jobs (one with a comment after
# its key) around a column-0 comment, a `needs:` list that names a job, and a
# key after the jobs block. diagrams/ holds a pair with the seven jobs as
# labels.
build_fixture() {
  local root job n=0
  root="$(mktemp -d "$TMP_ROOT/fixture.XXXXXX")"
  mkdir -p "$root/diagrams"
  cat >"$root/build-images.yaml" <<'EOF'
name: Build Images
on:
  push:
    branches: [main]
jobs:
  nix-flake-check:
    runs-on: ubuntu-latest
  nix-lock-check: # the locks
    runs-on: ubuntu-latest
  generate-matrix:
    runs-on: ubuntu-latest
# the service images
  build-service-images:
    needs: [nix-flake-check, nix-lock-check, generate-matrix]
  test-service-images:
    needs: [nix-lock-check, generate-matrix]
  merge-service-images:
    needs: [build-service-images]
  verify-service-images:
    needs: [merge-service-images, test-service-images]
concurrency:
  group: build
EOF
  {
    echo '<mxfile host="drawio" type="device"><diagram id="f" name="F"><mxGraphModel><root>'
    for job in $JOBS; do
      n=$((n + 1))
      echo "<mxCell id=\"f-$n\" value=\"$job\" vertex=\"1\" parent=\"1\"/>"
    done
    echo '</root></mxGraphModel></diagram></mxfile>'
  } >"$root/diagrams/$FIGURE.drawio"
  {
    echo '<svg xmlns="http://www.w3.org/2000/svg">'
    for job in $JOBS; do
      echo "<text x=\"0\" y=\"0\">$job</text>"
    done
    echo '</svg>'
  } >"$root/diagrams/$FIGURE.svg"
  printf '%s\n' "$root"
}

# rewrite <file> <sed-script>: apply a sed script in place (BSD and GNU sed).
rewrite() {
  sed "$2" "$1" >"$1.tmp" && mv "$1.tmp" "$1"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_consistent_fixture_reports_nothing() {
  echo "Test: a figure and a workflow with every job report nothing"

  local root
  root="$(build_fixture)"
  assert_eq "no problems" "" \
    "$(check_publish_figure "$root/build-images.yaml" "$root/diagrams")"
}

test_missing_or_jobless_workflow_is_the_only_report() {
  echo "Test: a missing workflow or one without a job is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.svg"
  assert_eq "absent workflow" "missing $root/absent.yaml" \
    "$(check_publish_figure "$root/absent.yaml" "$root/diagrams")"

  printf 'name: Build Images\non:\n  push:\n' >"$root/build-images.yaml"
  assert_eq "workflow without jobs" "no job in $root/build-images.yaml" \
    "$(check_publish_figure "$root/build-images.yaml" "$root/diagrams")"
}

test_missing_figure_file_skips_only_its_searches() {
  echo "Test: a missing file of the pair skips only the searches in that file"

  local root
  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.drawio"
  rewrite "$root/diagrams/$FIGURE.svg" '/>generate-matrix</d'
  assert_eq "no drawio, the svg is still searched" \
    "missing figure: $FIGURE.drawio"$'\n'"not in svg: generate-matrix" \
    "$(check_publish_figure "$root/build-images.yaml" "$root/diagrams")"
}

test_job_missing_from_one_place_reports_exactly_its_line() {
  echo "Test: a job missing from the drawio, the svg or the workflow reports exactly its line"

  local root
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.drawio" '/value="merge-service-images"/d'
  assert_eq "a job missing from the drawio" "not in drawio: merge-service-images" \
    "$(check_publish_figure "$root/build-images.yaml" "$root/diagrams")"

  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.svg" '/>nix-lock-check</d'
  assert_eq "a job missing from the svg" "not in svg: nix-lock-check" \
    "$(check_publish_figure "$root/build-images.yaml" "$root/diagrams")"

  # nix-lock-check carries a comment after its key, so this also proves that
  # such a key is read as a job.
  root="$(build_fixture)"
  rewrite "$root/build-images.yaml" 's/^  test-service-images:/  unit-tests:/'
  assert_eq "a job renamed in the workflow" \
    "not a job of build-images.yaml: test-service-images" \
    "$(check_publish_figure "$root/build-images.yaml" "$root/diagrams")"
}

test_deeper_or_later_key_is_no_job() {
  echo "Test: a key deeper than two spaces or after the jobs block is no job"

  local root
  root="$(build_fixture)"
  rewrite "$root/build-images.yaml" 's/^  verify-service-images:/    verify-service-images:/'
  assert_eq "a job key at four spaces" \
    "not a job of build-images.yaml: verify-service-images" \
    "$(check_publish_figure "$root/build-images.yaml" "$root/diagrams")"

  root="$(build_fixture)"
  rewrite "$root/build-images.yaml" '/^  verify-service-images:/,/^    needs:.*test-service-images/d'
  printf 'outputs:\n  verify-service-images:\n' >>"$root/build-images.yaml"
  assert_eq "a two-space key after the jobs block" \
    "not a job of build-images.yaml: verify-service-images" \
    "$(check_publish_figure "$root/build-images.yaml" "$root/diagrams")"
}

test_label_must_be_its_own_element() {
  echo "Test: a job that only stands inside a longer label is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.drawio" 's#value="verify-service-images"#value="verify-service-images job"#'
  rewrite "$root/diagrams/$FIGURE.svg" 's#>verify-service-images<#>the verify-service-images job<#'
  assert_eq "label inside a longer cell and text" \
    "not in drawio: verify-service-images"$'\n'"not in svg: verify-service-images" \
    "$(check_publish_figure "$root/build-images.yaml" "$root/diagrams")"
}

test_repository_figure_names_every_job() {
  echo "Test: the repository's image publish figure draws jobs of build-images.yaml"

  local workflow="$PROJECT_ROOT/.github/workflows/build-images.yaml"
  local copy
  assert_eq "no problems in docs/diagrams" "" \
    "$(check_publish_figure "$workflow" "$PROJECT_ROOT/docs/diagrams")"

  # The same check on a copy without the verify-service-images label proves
  # it reads the real figure.
  copy="$(mktemp -d "$TMP_ROOT/copy.XXXXXX")"
  cp "$PROJECT_ROOT/docs/diagrams/$FIGURE.drawio" "$PROJECT_ROOT/docs/diagrams/$FIGURE.svg" "$copy/"
  rewrite "$copy/$FIGURE.svg" 's#>verify-service-images<#>verify<#'
  assert_eq "a copy without verify-service-images" "not in svg: verify-service-images" \
    "$(check_publish_figure "$workflow" "$copy")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_consistent_fixture_reports_nothing
test_missing_or_jobless_workflow_is_the_only_report
test_missing_figure_file_skips_only_its_searches
test_job_missing_from_one_place_reports_exactly_its_line
test_deeper_or_later_key_is_no_job
test_label_must_be_its_own_element
test_repository_figure_names_every_job

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
