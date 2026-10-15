#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the migration status table of docs/reference/ci-cd/nix-image-build.md
# matches the tree.
#
# The table follows the heading `## Migration status` and starts with the
# header `| Image | Build | Phase | Issue |`; it ends at the first line that does
# not start with `|`. The first cell names the image in backticks, the second
# says Nix or Docker. Every directory under images/ and every operator with an
# operators/<op>/go.mod (as <op>-operator) needs a row. An image row that says
# Nix needs images/<name>/image.nix and no Dockerfile; a row that says Docker
# needs the Dockerfile and no image.nix. An operator row builds from the shared
# operators/Dockerfile, so it is checked for its Build value only.
# check_status_table prints one problem per line, sorted, and nothing for a
# consistent tree. The fixture tests prove each message on a scratch tree
# before the last test runs the check on the repository.
#
# Usage: bash tests/unit/docs/nix_image_build_status_table_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

PAGE="docs/reference/ci-cd/nix-image-build.md"
TABLE_HEADER='| Image | Build | Phase | Issue |'

# Print the rows of the status table: a line "#table" once the header is
# found, then per row the image, a "|", and the Build cell.
# shellcheck disable=SC2016 # an awk program, not shell
ROWS_AWK='
function trim(s) {
  gsub(/^[ \t]+/, "", s)
  gsub(/[ \t]+$/, "", s)
  return s
}

$0 == "## Migration status" { after = 1; next }
after && !in_table && /^## / { exit }
after && !in_table && $0 == header { in_table = 1; print "#table"; next }
in_table {
  if (substr($0, 1, 1) != "|") exit
  if ($0 ~ /^\|[ \t:|-]+$/) next
  split($0, cells, "|")
  name = cells[2]
  if (match(name, /`[^`]*`/)) name = substr(name, RSTART + 1, RLENGTH - 2)
  else name = trim(name)
  print name "|" trim(cells[3])
}
'

# check_status_table <root>: print one problem per line, sorted, and nothing
# when the table of <root>/docs/reference/ci-cd/nix-image-build.md matches the
# tree under <root>. A missing page, an images/ without a directory and a page
# without the table are each the only line.
check_status_table() {
  local root="$1"
  local page="$root/$PAGE"

  if [[ ! -f "$page" ]]; then
    echo "missing $page"
    return
  fi

  local images="" operators="" dir
  for dir in "$root"/images/*/; do
    [[ -d "$dir" ]] || continue
    dir="${dir%/}"
    images="$images${dir##*/}"$'\n'
  done
  if [[ -z "$images" ]]; then
    echo "no image directories under images/"
    return
  fi
  for dir in "$root"/operators/*/go.mod; do
    [[ -f "$dir" ]] || continue
    dir="${dir%/go.mod}"
    operators="$operators${dir##*/}-operator"$'\n'
  done

  local rows
  rows="$(awk -v header="$TABLE_HEADER" "$ROWS_AWK" "$page")"
  if [[ -z "$rows" ]]; then
    echo "no status table in $page"
    return
  fi

  {
    local name build named=""
    while IFS='|' read -r name build; do
      [[ "$name" == "#table" ]] && continue
      named="$named$name"$'\n'
      if grep -qxF -- "$name" <<<"$images"; then
        local nix="$root/images/$name/image.nix" dockerfile="$root/images/$name/Dockerfile"
        case "$build" in
          Nix)
            [[ -f "$nix" ]] || echo "$name says Nix but images/$name/image.nix is missing"
            [[ -f "$dockerfile" ]] && echo "$name says Nix but images/$name/Dockerfile exists"
            ;;
          Docker)
            [[ -f "$nix" ]] && echo "$name says Docker but images/$name/image.nix exists"
            [[ -f "$dockerfile" ]] || echo "$name says Docker but images/$name/Dockerfile is missing"
            ;;
          *) echo "$name has no build column" ;;
        esac
      elif grep -qxF -- "$name" <<<"$operators"; then
        case "$build" in
          Nix | Docker) ;;
          *) echo "$name has no build column" ;;
        esac
      else
        echo "row for $name names no directory"
      fi
    done <<<"$rows"

    while IFS= read -r name; do
      [[ -n "$name" ]] || continue
      grep -qxF -- "$name" <<<"$named" || echo "no row for $name"
    done <<<"$images$operators"
  } | LC_ALL=C sort
}

# ---------------------------------------------------------------------------
# Fixture
# ---------------------------------------------------------------------------

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

# build_fixture: write a consistent tree and print its root. images/a builds
# from a Dockerfile, images/b from image.nix, and operators/x is an operator.
# The page has a second table with the same header above the status section,
# which the check must not read, and text after the table.
build_fixture() {
  local root
  root="$(mktemp -d "$TMP_ROOT/fixture.XXXXXX")"
  mkdir -p "$root/images/a" "$root/images/b" "$root/operators/x" "$root/docs/reference/ci-cd"
  : >"$root/images/a/Dockerfile"
  : >"$root/images/b/image.nix"
  : >"$root/operators/x/go.mod"
  cat >"$root/$PAGE" <<'EOF'
# Nix Image Build

## Flake layout

| Image | Build | Phase | Issue |
| --- | --- | --- | --- |
| `elsewhere` | Nix | Phase 0 | #1 |

## Migration status

| Image | Build | Phase | Issue |
| --- | --- | --- | --- |
| `a` | Docker | Phase 2 | #2 |
| `b` | Nix | Phase 1 | #1 |
| `x-operator` | Docker | Phase 6 | #6 |

| `after` | Nix | Phase 9 | #9 |
EOF
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
  echo "Test: a table that matches the tree reports nothing"

  local root
  root="$(build_fixture)"
  assert_eq "no problems" "" "$(check_status_table "$root")"
}

test_missing_page_is_the_only_report() {
  echo "Test: a missing page is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/$PAGE"
  assert_eq "missing page" "missing $root/$PAGE" "$(check_status_table "$root")"
}

test_empty_images_is_the_only_report() {
  echo "Test: an images/ without a directory is the only report"

  local root
  root="$(build_fixture)"
  rm -r "$root/images/a" "$root/images/b"
  assert_eq "empty images/" "no image directories under images/" "$(check_status_table "$root")"

  rm -r "$root/images"
  assert_eq "no images/ at all" "no image directories under images/" "$(check_status_table "$root")"
}

test_page_without_the_table_is_the_only_report() {
  echo "Test: a page without the status table is the only report"

  local root
  root="$(build_fixture)"
  rewrite "$root/$PAGE" 's/^## Migration status$/## Status/'
  assert_eq "no Migration status heading" "no status table in $root/$PAGE" \
    "$(check_status_table "$root")"

  root="$(build_fixture)"
  rewrite "$root/$PAGE" '/^## Migration status$/,$s/^| Image | Build | Phase | Issue |$/| Image | Build |/'
  assert_eq "a different header" "no status table in $root/$PAGE" \
    "$(check_status_table "$root")"
}

test_directory_without_a_row() {
  echo "Test: an image directory or an operator without a row is reported"

  local root
  root="$(build_fixture)"
  mkdir -p "$root/images/c" "$root/operators/y"
  : >"$root/images/c/Dockerfile"
  : >"$root/operators/y/go.mod"
  # A directory under operators/ without a go.mod is no operator.
  mkdir -p "$root/operators/shared"
  assert_eq "two missing rows" "no row for c
no row for y-operator" "$(check_status_table "$root")"
}

test_nix_row_without_image_nix() {
  echo "Test: a row that says Nix without images/<name>/image.nix is reported"

  local root
  root="$(build_fixture)"
  rm "$root/images/b/image.nix"
  assert_eq "image.nix missing" "b says Nix but images/b/image.nix is missing" \
    "$(check_status_table "$root")"
}

test_nix_row_with_a_dockerfile() {
  echo "Test: a row that says Nix beside a Dockerfile is reported"

  local root
  root="$(build_fixture)"
  : >"$root/images/b/Dockerfile"
  assert_eq "Dockerfile exists" "b says Nix but images/b/Dockerfile exists" \
    "$(check_status_table "$root")"
}

test_docker_row_with_image_nix() {
  echo "Test: a row that says Docker beside an image.nix is reported"

  local root
  root="$(build_fixture)"
  : >"$root/images/a/image.nix"
  assert_eq "image.nix exists" "a says Docker but images/a/image.nix exists" \
    "$(check_status_table "$root")"
}

test_docker_row_without_a_dockerfile() {
  echo "Test: a row that says Docker without a Dockerfile is reported"

  local root
  root="$(build_fixture)"
  rm "$root/images/a/Dockerfile"
  assert_eq "Dockerfile missing" "a says Docker but images/a/Dockerfile is missing" \
    "$(check_status_table "$root")"
}

test_unknown_build_value() {
  echo "Test: a Build cell that is neither Nix nor Docker is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/$PAGE" 's/^| `a` | Docker |/| `a` | docker |/'
  rewrite "$root/$PAGE" 's/^| `x-operator` | Docker |/| `x-operator` |  |/'
  assert_eq "a lower-case value and an empty cell" "a has no build column
x-operator has no build column" "$(check_status_table "$root")"
}

test_row_naming_no_directory() {
  echo "Test: a row for neither an image directory nor an operator is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/$PAGE" 's/^| `x-operator` | Docker | Phase 6 | #6 |$/&\
| `z` | Docker | Phase 6 | #6 |\
| `x` | Docker | Phase 6 | #6 |/'
  assert_eq "two rows without a directory" "row for x names no directory
row for z names no directory" "$(check_status_table "$root")"
}

test_repository_table_matches_the_tree() {
  echo "Test: the repository's status table matches images/ and operators/"

  assert_eq "no problems in the repository" "" "$(check_status_table "$PROJECT_ROOT")"

  # The same check on a copy of the page without the backup-shifter row, over
  # the repository's own images/ and operators/, proves it reads the real
  # table.
  local copy
  copy="$(mktemp -d "$TMP_ROOT/copy.XXXXXX")"
  mkdir -p "$copy/docs/reference/ci-cd"
  ln -s "$PROJECT_ROOT/images" "$copy/images"
  ln -s "$PROJECT_ROOT/operators" "$copy/operators"
  sed '/^| `backup-shifter` |/d' "$PROJECT_ROOT/$PAGE" >"$copy/$PAGE"
  assert_eq "a copy without the backup-shifter row" "no row for backup-shifter" \
    "$(check_status_table "$copy")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_consistent_fixture_reports_nothing
test_missing_page_is_the_only_report
test_empty_images_is_the_only_report
test_page_without_the_table_is_the_only_report
test_directory_without_a_row
test_nix_row_without_image_nix
test_nix_row_with_a_dockerfile
test_docker_row_with_image_nix
test_docker_row_without_a_dockerfile
test_unknown_build_value
test_row_naming_no_directory
test_repository_table_matches_the_tree

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
