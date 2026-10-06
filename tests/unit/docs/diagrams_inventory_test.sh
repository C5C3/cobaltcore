#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the docs figures, their inventory and their embeds stay in step.
#
# Every figure in docs/diagrams/ is a pair, <name>.drawio and <name>.svg.
# The "## Inventory" section of docs/contributing/architecture-diagrams.md
# has one row per figure, and the row's last column links every page that
# embeds it. A page embeds a figure as ![alt](<path>/diagrams/<name>.svg) on
# one line, with the path spelled ./diagrams/ directly under docs/ and with
# one ../ per directory level below it, without a title or angle brackets.
# check_diagrams prints one line per drift and nothing for a consistent
# tree. The fixture tests prove each check on a scratch tree before the last
# test runs it on docs/.
#
# A missing file is left to `npm run docs:build`, which fails on an image
# it cannot resolve.
#
# Usage: bash tests/unit/docs/diagrams_inventory_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

INVENTORY_PAGE="contributing/architecture-diagrams.md"

# The cross-check behind check_diagrams. It runs in the docs root and reads
# every page; FIGURES in the environment lists the files of diagrams/, one
# file name per line. Pages are paths relative to the docs root.
# shellcheck disable=SC2016 # an awk program, not shell
CHECK_AWK='
function canonical(page, name,   depth, prefix, i) {
  depth = gsub(/\//, "/", page)
  if (depth == 0) return "./diagrams/" name ".svg"
  prefix = ""
  for (i = 0; i < depth; i++) prefix = prefix "../"
  return prefix "diagrams/" name ".svg"
}

BEGIN {
  count = split(ENVIRON["FIGURES"], files, "\n")
  for (i = 1; i <= count; i++) {
    f = files[i]
    if (f ~ /\.drawio$/) { n = substr(f, 1, length(f) - 7); drawio[n] = 1; fig[n] = 1 }
    else if (f ~ /\.svg$/) { n = substr(f, 1, length(f) - 4); svg[n] = 1; fig[n] = 1 }
  }
  invdir = inv
  sub(/[^\/]*$/, "", invdir)
}

FNR == 1 {
  page = FILENAME
  sub(/^\.\//, "", page)
  exists[page] = 1
  fence = ""
  in_inventory = 0
}

{
  line = $0
  sub(/^[ \t]+/, "", line)
  marker = substr(line, 1, 3)
  if (fence == "" && (marker == "```" || marker == "~~~")) { fence = marker; next }
  if (fence != "") { if (marker == fence) fence = ""; next }
}

page == inv {
  if ($0 == "## Inventory") in_inventory = 1
  else if (substr($0, 1, 3) == "## ") in_inventory = 0
  if (in_inventory && substr($0, 1, 3) == "| `") {
    ncell = split($0, cell, "|")
    name = cell[2]
    gsub(/[ \t`]/, "", name)
    rows[name]++
    # Only the last column, "Embedded in", lists pages.
    rest = cell[ncell]
    if (rest ~ /^[ \t]*$/) rest = cell[ncell - 1]
    while ((j = index(rest, "](")) > 0) {
      rest = substr(rest, j + 2)
      k = index(rest, ")")
      if (k == 0) break
      target = substr(rest, 1, k - 1)
      rest = substr(rest, k + 1)
      sub(/#.*/, "", target)
      base = invdir
      up = ""
      if (substr(target, 1, 2) == "./") target = substr(target, 3)
      while (substr(target, 1, 3) == "../") {
        target = substr(target, 4)
        # A ../ above the docs root stays, so the page is reported missing.
        if (base == "") up = up "../"
        else sub(/[^\/]*\/$/, "", base)
      }
      target = up base target
      listed[name SUBSEP target] = 1
      has_pages[name] = 1
    }
  }
}

{
  rest = $0
  while ((i = index(rest, "![")) > 0) {
    rest = substr(rest, i + 2)
    j = index(rest, "](")
    if (j == 0) break
    alt = substr(rest, 1, j - 1)
    rest = substr(rest, j + 2)
    k = index(rest, ")")
    if (k == 0) break
    target = substr(rest, 1, k - 1)
    rest = substr(rest, k + 1)
    # Cut a title, a fragment or a closing angle bracket off the path.
    name = target
    sub(/[ \t#?>"].*$/, "", name)
    if (name !~ /\.svg$/ || index(name, "diagrams/") == 0) continue
    sub(/.*\//, "", name)
    sub(/\.svg$/, "", name)
    if (alt ~ /^[ \t]*$/) print "empty alt text: " page ":" FNR
    if (target != canonical(page, name)) print "non-canonical path: " page ":" FNR
    embedded[name SUBSEP page] = 1
  }
  if (index($0, "<img") > 0 && index($0, "diagrams/") > 0) print "html image: " page ":" FNR
}

END {
  for (n in fig) {
    if (!(n in drawio)) print "missing source: " n ".drawio"
    if (!(n in svg)) print "missing export: " n ".svg"
    if (!(n in rows)) print "not in inventory: " n
  }
  for (n in rows) {
    if (!(n in fig)) print "inventory row without files: " n
    if (rows[n] > 1) print "duplicate inventory row: " n
    if (!(n in has_pages)) print "inventory row without pages: " n
  }
  for (key in listed) {
    split(key, pair, SUBSEP)
    if (!(pair[2] in exists)) print "listed page missing: " pair[1] " -> " pair[2]
    else if (!(key in embedded)) print "listed but not embedded: " pair[1] " -> " pair[2]
  }
  for (key in embedded) {
    split(key, pair, SUBSEP)
    if (!(key in listed)) print "embedded but not listed: " pair[1] " -> " pair[2]
  }
}
'

# check_diagrams <docs-root>: print one problem per line, sorted, and nothing
# when figure files, inventory and embeds agree.
check_diagrams() {
  local root="$1"

  if [[ ! -d "$root" ]]; then
    echo "no docs root: $root"
    return
  fi
  if [[ ! -d "$root/diagrams" ]]; then
    echo "no diagrams directory: $root/diagrams"
    return
  fi
  if [[ ! -f "$root/$INVENTORY_PAGE" ]] || ! grep -qx '## Inventory' "$root/$INVENTORY_PAGE"; then
    echo "no inventory section: $INVENTORY_PAGE"
    return
  fi

  local figures="" f
  for f in "$root"/diagrams/*.drawio "$root"/diagrams/*.svg; do
    if [[ -f "$f" ]]; then
      figures="$figures${f##*/}"$'\n'
    fi
  done

  local pages=() p
  while IFS= read -r p; do
    pages+=("$p")
  done < <(cd "$root" && find . \( -name .vitepress -o -name node_modules \) -prune \
    -o -type f -name '*.md' -print | LC_ALL=C sort)

  (cd "$root" && FIGURES="$figures" awk -v inv="$INVENTORY_PAGE" "$CHECK_AWK" "${pages[@]}") \
    | LC_ALL=C sort
}

# ---------------------------------------------------------------------------
# Fixture
# ---------------------------------------------------------------------------

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

# build_fixture: write a consistent docs tree and print its root. Two figure
# pairs, alpha embedded at depth 0 and beta at depth 2, and an example embed
# inside a fenced block on the conventions page.
build_fixture() {
  local root n
  root="$(mktemp -d "$TMP_ROOT/fixture.XXXXXX")"
  mkdir -p "$root/diagrams" "$root/contributing" "$root/reference/area"
  for n in alpha beta; do
    printf '<mxfile/>\n' >"$root/diagrams/$n.drawio"
    printf '<svg/>\n' >"$root/diagrams/$n.svg"
  done
  cat >"$root/$INVENTORY_PAGE" <<'EOF'
# Architecture Diagrams

## Inventory

| Diagram | Shows | Embedded in |
| --- | --- | --- |
| `alpha` | The first figure | [Start](../index.md) |
| `beta` | The second figure | [Deep page](../reference/area/deep.md#section) |

## Change a diagram

```markdown
![Example alt](../diagrams/example.svg)
```
EOF
  printf '%s\n' '# Start' '' '![Alpha alt](./diagrams/alpha.svg)' >"$root/index.md"
  printf '%s\n' '# Deep page' '' '## Section' '' \
    '![Beta alt](../../diagrams/beta.svg)' >"$root/reference/area/deep.md"
  printf '%s\n' "$root"
}

# rewrite <file> <sed-script>: apply a sed script in place (BSD and GNU sed).
rewrite() {
  sed "$2" "$1" >"$1.tmp" && mv "$1.tmp" "$1"
}

# append_after <file> <line-prefix> <line>: add a line after every line that
# starts with the prefix.
append_after() {
  awk -v prefix="$2" -v add="$3" '{ print } index($0, prefix) == 1 { print add }' \
    "$1" >"$1.tmp" && mv "$1.tmp" "$1"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_consistent_tree_reports_nothing() {
  echo "Test: a consistent tree, with embeds at depth 0 and 2, reports nothing"

  local root
  root="$(build_fixture)"
  assert_file_contains_fixed "depth-0 embed spelled ./diagrams/" \
    "$root/index.md" '](./diagrams/alpha.svg)'
  assert_file_contains_fixed "depth-2 embed spelled ../../diagrams/" \
    "$root/reference/area/deep.md" '](../../diagrams/beta.svg)'
  assert_eq "no problems" "" "$(check_diagrams "$root")"
}

# Without its fence the example embed counts, so the fence is what hides it.
test_fenced_example_embed_is_ignored() {
  echo "Test: an embed inside a fenced code block is ignored"

  local root
  root="$(build_fixture)"
  rewrite "$root/$INVENTORY_PAGE" '/^```/d'
  assert_eq "the unfenced example is an embed" \
    "embedded but not listed: example -> $INVENTORY_PAGE" \
    "$(check_diagrams "$root")"
}

test_empty_diagrams_directory_without_rows_reports_nothing() {
  echo "Test: an empty diagrams directory and an inventory without rows report nothing"

  local root
  root="$(mktemp -d "$TMP_ROOT/empty.XXXXXX")"
  mkdir -p "$root/diagrams" "$root/contributing"
  printf '%s\n' '# Architecture Diagrams' '' '## Inventory' '' \
    '| Diagram | Shows | Embedded in |' '| --- | --- | --- |' >"$root/$INVENTORY_PAGE"
  assert_eq "no problems" "" "$(check_diagrams "$root")"
}

test_missing_docs_root_is_the_only_report() {
  echo "Test: a docs root that does not exist is the only report"

  assert_eq "one line" "no docs root: $TMP_ROOT/absent" \
    "$(check_diagrams "$TMP_ROOT/absent")"
}

test_missing_diagrams_directory_is_the_only_report() {
  echo "Test: a missing diagrams directory is the only report"

  local root
  root="$(build_fixture)"
  rm -rf "$root/diagrams"
  assert_eq "one line" "no diagrams directory: $root/diagrams" \
    "$(check_diagrams "$root")"
}

test_missing_inventory_section_is_the_only_report() {
  echo "Test: a missing inventory section or conventions page is the only report"

  local root
  root="$(build_fixture)"
  rewrite "$root/$INVENTORY_PAGE" '/^## Inventory$/d'
  assert_eq "no '## Inventory' heading" "no inventory section: $INVENTORY_PAGE" \
    "$(check_diagrams "$root")"

  root="$(build_fixture)"
  rm "$root/$INVENTORY_PAGE"
  assert_eq "no conventions page" "no inventory section: $INVENTORY_PAGE" \
    "$(check_diagrams "$root")"
}

test_each_drift_reports_exactly_its_line() {
  echo "Test: each drift reports exactly its own line"

  local root inv

  root="$(build_fixture)"
  rm "$root/diagrams/alpha.drawio"
  assert_eq "svg without drawio" "missing source: alpha.drawio" "$(check_diagrams "$root")"

  root="$(build_fixture)"
  rm "$root/diagrams/alpha.svg"
  assert_eq "drawio without svg" "missing export: alpha.svg" "$(check_diagrams "$root")"

  root="$(build_fixture)"
  touch "$root/diagrams/gamma.drawio" "$root/diagrams/gamma.svg"
  assert_eq "figure without row" "not in inventory: gamma" "$(check_diagrams "$root")"

  root="$(build_fixture)"
  rm "$root/diagrams/alpha.drawio" "$root/diagrams/alpha.svg"
  assert_eq "row without files" "inventory row without files: alpha" "$(check_diagrams "$root")"

  root="$(build_fixture)"
  inv="$root/$INVENTORY_PAGE"
  # shellcheck disable=SC2016 # literal Markdown, not shell
  append_after "$inv" '| `alpha`' '| `alpha` | The first figure | [Start](../index.md) |'
  assert_eq "two rows for one name" "duplicate inventory row: alpha" "$(check_diagrams "$root")"

  root="$(build_fixture)"
  inv="$root/$INVENTORY_PAGE"
  touch "$root/diagrams/gamma.drawio" "$root/diagrams/gamma.svg"
  # shellcheck disable=SC2016 # literal Markdown, not shell
  append_after "$inv" '| `beta`' '| `gamma` | A third figure | nowhere yet |'
  assert_eq "row without a link" "inventory row without pages: gamma" "$(check_diagrams "$root")"

  root="$(build_fixture)"
  rewrite "$root/$INVENTORY_PAGE" 's#(../index.md)#(../index.md), [Gone](../gone.md)#'
  assert_eq "listed page that does not exist" "listed page missing: alpha -> gone.md" \
    "$(check_diagrams "$root")"

  root="$(build_fixture)"
  rewrite "$root/$INVENTORY_PAGE" 's#(../index.md)#(../../index.md)#'
  assert_eq "listed page above the docs root" \
    "embedded but not listed: alpha -> index.md"$'\n'"listed page missing: alpha -> ../index.md" \
    "$(check_diagrams "$root")"

  root="$(build_fixture)"
  rewrite "$root/$INVENTORY_PAGE" 's#(../index.md)#(../index.md), [Deep page](../reference/area/deep.md)#'
  assert_eq "listed page without the embed" \
    "listed but not embedded: alpha -> reference/area/deep.md" "$(check_diagrams "$root")"

  root="$(build_fixture)"
  printf '%s\n' '' '![Alpha alt](../../diagrams/alpha.svg)' >>"$root/reference/area/deep.md"
  assert_eq "embed on a page the row does not list" \
    "embedded but not listed: alpha -> reference/area/deep.md" "$(check_diagrams "$root")"

  root="$(build_fixture)"
  rewrite "$root/index.md" 's#!\[Alpha alt\]#![ ]#'
  assert_eq "alt text of white space" "empty alt text: index.md:3" "$(check_diagrams "$root")"

  root="$(build_fixture)"
  rewrite "$root/index.md" 's#(./diagrams/alpha.svg)#(diagrams/alpha.svg)#'
  assert_eq "path without ./" "non-canonical path: index.md:3" "$(check_diagrams "$root")"

  root="$(build_fixture)"
  rewrite "$root/index.md" 's#(./diagrams/alpha.svg)#(./diagrams/alpha.svg "Alpha")#'
  assert_eq "embed with a title" "non-canonical path: index.md:3" "$(check_diagrams "$root")"

  root="$(build_fixture)"
  rewrite "$root/index.md" 's#(./diagrams/alpha.svg)#(<./diagrams/alpha.svg>)#'
  assert_eq "embed in angle brackets" "non-canonical path: index.md:3" "$(check_diagrams "$root")"

  root="$(build_fixture)"
  printf '%s\n' '' '![Alpha alt](../../diagrams/alpha.svg "Alpha")' >>"$root/reference/area/deep.md"
  assert_eq "embed with a title on a page the row does not list" \
    "embedded but not listed: alpha -> reference/area/deep.md"$'\n'"non-canonical path: reference/area/deep.md:7" \
    "$(check_diagrams "$root")"

  root="$(build_fixture)"
  printf '%s\n' '<img src="./diagrams/alpha.svg" alt="x">' >>"$root/index.md"
  assert_eq "HTML image" "html image: index.md:4" "$(check_diagrams "$root")"
}

test_parser_edges_report_nothing() {
  echo "Test: other links in a row, self-links, rows after the inventory and other fences report nothing"

  local root inv

  root="$(build_fixture)"
  rewrite "$root/$INVENTORY_PAGE" \
    's#| The first figure |#| The [first](../reference/area/deep.md) of [two](https://example.com) |#'
  assert_eq "links outside the Embedded in column" "" "$(check_diagrams "$root")"

  root="$(build_fixture)"
  inv="$root/$INVENTORY_PAGE"
  touch "$root/diagrams/gamma.drawio" "$root/diagrams/gamma.svg"
  # shellcheck disable=SC2016 # literal Markdown, not shell
  append_after "$inv" '| `beta`' \
    '| `gamma` | Self | [Self](./architecture-diagrams.md#x), [Again](architecture-diagrams.md) |'
  printf '%s\n' '' '![Gamma alt](../diagrams/gamma.svg)' >>"$inv"
  assert_eq "links with ./ and without a prefix" "" "$(check_diagrams "$root")"

  root="$(build_fixture)"
  # shellcheck disable=SC2016 # literal Markdown, not shell
  append_after "$root/$INVENTORY_PAGE" '## Change a diagram' '| `ghost` | not a figure |'
  assert_eq "a row after the inventory section" "" "$(check_diagrams "$root")"

  root="$(build_fixture)"
  printf '%s\n' '' '~~~markdown' '![T](../diagrams/tilde.svg)' '~~~' '' \
    '1. Step' '   ```markdown' '   ![I](../diagrams/indent.svg)' '   ```' >>"$root/$INVENTORY_PAGE"
  assert_eq "tilde and indented fences" "" "$(check_diagrams "$root")"
}

test_repository_docs_are_consistent() {
  echo "Test: docs/ figures, inventory and embeds are consistent"

  assert_eq "no problems in docs/" "" "$(check_diagrams "$PROJECT_ROOT/docs")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_consistent_tree_reports_nothing
test_fenced_example_embed_is_ignored
test_empty_diagrams_directory_without_rows_reports_nothing
test_missing_docs_root_is_the_only_report
test_missing_diagrams_directory_is_the_only_report
test_missing_inventory_section_is_the_only_report
test_each_drift_reports_exactly_its_line
test_parser_edges_report_nothing
test_repository_docs_are_consistent

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
