#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the service image build figure names the files it draws, and that
# each of them exists in the repository.
#
# docs/diagrams/ci-service-image-build.drawio and .svg draw the files of
# REQUIRED and OPTIONAL as labels with the placeholders {release} and
# {service}. check_build_figure looks for every label in both files of the
# pair as a label of its own: in the .drawio a cell value, plain or as the
# only text of the bold span a file box carries, in the .svg a <text>
# element. It resolves {release} to the newest directory under releases/ and
# {service} to the first key of that release's source-refs.yaml. A path of
# REQUIRED must exist, as a directory when its label ends in a slash and as
# a file otherwise; of a path of OPTIONAL only the first component must
# exist, since a release without overrides or patches has neither file. It
# prints one problem per line and nothing for a consistent tree. The check
# runs one way: a file the figure does not draw is not reported. The fixture
# tests prove each message on a scratch tree before the last test runs the
# check on the repository.
#
# Usage: bash tests/unit/docs/ci_service_image_build_figure_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

FIGURE="ci-service-image-build"

REQUIRED="releases/{release}/source-refs.yaml
releases/{release}/upper-constraints.txt
releases/{release}/extra-packages.yaml
nix/base-requirements.txt
hack/nix-lock.sh
nix/locks/{release}/{service}/
images/{service}/image.nix
nix/lib/service-image.nix
nix/lib/python-env.nix
nix/lib/fhs-layout.nix
images/nova-compute/image.nix
nix/pkgs/libvirt.nix"

OPTIONAL="overrides/{release}/constraints.txt
patches/{service}/{release}/*.patch"

# check_build_figure <root> <diagrams-dir>: print one problem per line,
# sorted, and nothing when both files of the figure draw every label and
# every path resolves under <root>. A root without a release directory, or a
# newest release without a service key, is the only line.
check_build_figure() {
  local root="$1" dir="$2"
  local drawio="$dir/$FIGURE.drawio" svg="$dir/$FIGURE.svg"

  local release
  release="$(ls "$root/releases" 2>/dev/null | LC_ALL=C sort | awk 'END { print }')"
  if [[ -z "$release" ]]; then
    echo "no release directory under $root/releases/"
    return
  fi
  local refs="$root/releases/$release/source-refs.yaml" service=""
  if [[ -f "$refs" ]]; then
    service="$(awk '/^[a-z0-9-]+:/ { sub(/:.*/, ""); print; exit }' "$refs")"
  fi
  if [[ -z "$service" ]]; then
    echo "no service key in releases/$release/source-refs.yaml"
    return
  fi

  {
    [[ -f "$drawio" ]] || echo "missing figure: $FIGURE.drawio"
    [[ -f "$svg" ]] || echo "missing figure: $FIGURE.svg"
    local label path kind
    while IFS=' ' read -r kind label; do
      if [[ -f "$drawio" ]] && ! grep -F -q -- "value=\"$label\"" "$drawio" &&
        ! grep -F -q -- "&gt;$label&lt;/span&gt;\"" "$drawio"; then
        echo "not in drawio: $label"
      fi
      if [[ -f "$svg" ]] && ! grep -F -q -- ">$label<" "$svg"; then
        echo "not in svg: $label"
      fi
      path="${label//\{release\}/$release}"
      path="${path//\{service\}/$service}"
      if [[ "$kind" == optional ]]; then
        path="${path%%/*}/"
        [[ -d "$root/$path" ]] || echo "missing path: $path"
      elif [[ "$path" == */ ]]; then
        [[ -d "$root/$path" ]] || echo "missing path: $path"
      else
        [[ -f "$root/$path" ]] || echo "missing path: $path"
      fi
    done <<<"$(sed 's/^/required /' <<<"$REQUIRED"; sed 's/^/optional /' <<<"$OPTIONAL")"
  } | LC_ALL=C sort
}

# ---------------------------------------------------------------------------
# Fixture
# ---------------------------------------------------------------------------

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

# build_fixture: write a consistent tree and print its root. releases/ holds
# 2026.1 and 2026.2, whose source-refs.yaml opens with comments and names
# alpha before beta, so the paths resolve to 2026.2 and alpha; every
# required path exists for that pair only, and overrides/ and patches/ are
# empty. diagrams/ holds a pair with every label, the files of REQUIRED as
# bold spans in the .drawio and the rest as plain values.
build_fixture() {
  local root label n=0
  root="$(mktemp -d "$TMP_ROOT/fixture.XXXXXX")"
  mkdir -p "$root/diagrams" "$root/releases/2026.1" "$root/releases/2026.2" \
    "$root/overrides" "$root/patches" "$root/hack" "$root/nix/lib" "$root/nix/pkgs" \
    "$root/nix/locks/2026.2/alpha" "$root/images/alpha" "$root/images/nova-compute"
  printf 'gamma: "1.0.0"\n' >"$root/releases/2026.1/source-refs.yaml"
  printf '# Git refs\n# key: ref\nalpha: "2.0.0"\nbeta: "3.0.0"\n' >"$root/releases/2026.2/source-refs.yaml"
  for label in releases/2026.2/upper-constraints.txt releases/2026.2/extra-packages.yaml \
    nix/base-requirements.txt hack/nix-lock.sh images/alpha/image.nix \
    nix/lib/service-image.nix nix/lib/python-env.nix nix/lib/fhs-layout.nix \
    images/nova-compute/image.nix nix/pkgs/libvirt.nix; do
    : >"$root/$label"
  done
  {
    echo '<mxfile host="drawio" type="device"><diagram id="f" name="F"><mxGraphModel><root>'
    while IFS= read -r label; do
      n=$((n + 1))
      echo "<mxCell id=\"f-$n\" value=\"&lt;span style=&quot;font-weight:600&quot;&gt;$label&lt;/span&gt;\" vertex=\"1\" parent=\"1\"/>"
    done <<<"$REQUIRED"
    while IFS= read -r label; do
      n=$((n + 1))
      echo "<mxCell id=\"f-$n\" value=\"$label\" vertex=\"1\" parent=\"1\"/>"
    done <<<"$OPTIONAL"
    echo '</root></mxGraphModel></diagram></mxfile>'
  } >"$root/diagrams/$FIGURE.drawio"
  {
    echo '<svg xmlns="http://www.w3.org/2000/svg">'
    while IFS= read -r label; do
      echo "<text x=\"0\" y=\"0\">$label</text>"
    done <<<"$REQUIRED"$'\n'"$OPTIONAL"
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
  echo "Test: a figure that draws every label of an existing path reports nothing"

  local root
  root="$(build_fixture)"
  assert_eq "no problems" "" "$(check_build_figure "$root" "$root/diagrams")"
}

test_missing_release_or_service_is_the_only_report() {
  echo "Test: no release directory, or no key in its source-refs.yaml, is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.svg"
  rm -r "$root/releases/2026.1" "$root/releases/2026.2"
  assert_eq "empty releases/" "no release directory under $root/releases/" \
    "$(check_build_figure "$root" "$root/diagrams")"
  assert_eq "no releases/ at all" "no release directory under $root/absent/releases/" \
    "$(check_build_figure "$root/absent" "$root/diagrams")"

  root="$(build_fixture)"
  printf '# no key yet\n' >"$root/releases/2026.2/source-refs.yaml"
  assert_eq "a source-refs.yaml without a key" \
    "no service key in releases/2026.2/source-refs.yaml" \
    "$(check_build_figure "$root" "$root/diagrams")"

  root="$(build_fixture)"
  rm "$root/releases/2026.2/source-refs.yaml"
  assert_eq "the newest release without source-refs.yaml" \
    "no service key in releases/2026.2/source-refs.yaml" \
    "$(check_build_figure "$root" "$root/diagrams")"
}

test_missing_figure_file_skips_only_its_searches() {
  echo "Test: a missing file of the pair skips only the searches in that file"

  local root
  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.drawio"
  rewrite "$root/diagrams/$FIGURE.svg" '\#>hack/nix-lock.sh<#d'
  assert_eq "no drawio, the svg is still searched" \
    "missing figure: $FIGURE.drawio"$'\n'"not in svg: hack/nix-lock.sh" \
    "$(check_build_figure "$root" "$root/diagrams")"
}

test_label_missing_from_one_file_reports_exactly_its_line() {
  echo "Test: a label missing from one file reports exactly its line"

  local root
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.drawio" '\#nix/locks/{release}/{service}/#d'
  assert_eq "a bold-span label missing from the drawio" \
    "not in drawio: nix/locks/{release}/{service}/" \
    "$(check_build_figure "$root" "$root/diagrams")"

  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.drawio" '\#value="patches/{service}/{release}/\*.patch"#d'
  assert_eq "a plain label missing from the drawio" \
    "not in drawio: patches/{service}/{release}/*.patch" \
    "$(check_build_figure "$root" "$root/diagrams")"

  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.svg" '\#>images/nova-compute/image.nix<#d'
  assert_eq "a label missing from the svg" "not in svg: images/nova-compute/image.nix" \
    "$(check_build_figure "$root" "$root/diagrams")"
}

test_label_must_be_its_own_element() {
  echo "Test: a label that only stands inside a longer label is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.drawio" 's#nix/pkgs/libvirt.nix&lt;/span#nix/pkgs/libvirt.nix and QEMU\&lt;/span#'
  rewrite "$root/diagrams/$FIGURE.svg" 's#>nix/pkgs/libvirt.nix<#>with nix/pkgs/libvirt.nix<#'
  assert_eq "label inside a longer span and text" \
    "not in drawio: nix/pkgs/libvirt.nix"$'\n'"not in svg: nix/pkgs/libvirt.nix" \
    "$(check_build_figure "$root" "$root/diagrams")"
}

test_missing_required_path_reports_the_resolved_path() {
  echo "Test: a required path missing for the newest release and its first service is reported"

  local root
  root="$(build_fixture)"
  rm -r "$root/nix/locks/2026.2/alpha"
  mkdir -p "$root/nix/locks/2026.1/alpha" "$root/nix/locks/2026.2/beta"
  assert_eq "the lock directory of 2026.2 and alpha" \
    "missing path: nix/locks/2026.2/alpha/" \
    "$(check_build_figure "$root" "$root/diagrams")"

  root="$(build_fixture)"
  rm "$root/images/alpha/image.nix" "$root/nix/lib/fhs-layout.nix"
  mkdir "$root/images/alpha/image.nix"
  assert_eq "a file that is gone or a directory" \
    "missing path: images/alpha/image.nix"$'\n'"missing path: nix/lib/fhs-layout.nix" \
    "$(check_build_figure "$root" "$root/diagrams")"
}

test_optional_path_needs_only_its_first_component() {
  echo "Test: an optional path needs only its first component"

  local root
  root="$(build_fixture)"
  assert_eq "no overrides/2026.2/constraints.txt and no patch" "" \
    "$(check_build_figure "$root" "$root/diagrams")"

  rmdir "$root/overrides" "$root/patches"
  assert_eq "neither overrides/ nor patches/" \
    "missing path: overrides/"$'\n'"missing path: patches/" \
    "$(check_build_figure "$root" "$root/diagrams")"
}

test_repository_figure_names_every_file() {
  echo "Test: the repository's service image build figure draws files that exist"

  local copy
  assert_eq "no problems in docs/diagrams" "" \
    "$(check_build_figure "$PROJECT_ROOT" "$PROJECT_ROOT/docs/diagrams")"

  # The same check on a copy without the nova-compute label proves it reads
  # the real figure.
  copy="$(mktemp -d "$TMP_ROOT/copy.XXXXXX")"
  cp "$PROJECT_ROOT/docs/diagrams/$FIGURE.drawio" "$PROJECT_ROOT/docs/diagrams/$FIGURE.svg" "$copy/"
  rewrite "$copy/$FIGURE.svg" 's#>images/nova-compute/image.nix<#>nova-compute<#'
  assert_eq "a copy without images/nova-compute/image.nix" \
    "not in svg: images/nova-compute/image.nix" \
    "$(check_build_figure "$PROJECT_ROOT" "$copy")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_consistent_fixture_reports_nothing
test_missing_release_or_service_is_the_only_report
test_missing_figure_file_skips_only_its_searches
test_label_missing_from_one_file_reports_exactly_its_line
test_label_must_be_its_own_element
test_missing_required_path_reports_the_resolved_path
test_optional_path_needs_only_its_first_component
test_repository_figure_names_every_file

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
