#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the overlay table names every kustomize directory under deploy/ with
# the bases it lists.
#
# "## Kustomize Overlay Structure" of docs/reference/infrastructure/
# e2e-deployment.md holds a table with one row per directory under deploy/
# that has a kustomization.yaml: the directory, its bases and who applies it.
# A base is an entry "  - <entry>" of the top-level resources: list that,
# resolved against the directory, is a directory with a kustomization.yaml;
# the table writes it as the path from the repository root. The figure
# deploy-overlay-inheritance names every such directory in a label of the
# .drawio and of the .svg, by its path or by its last path element as a word
# of its own, and every label that is a path deploy/<path> names a
# directory. The figure groups the kind and lab directories of one name in a
# box each ("chaos-mesh, dizzy, nfs, prometheus" under "four directories
# under deploy/kind"), so a last path element that several directories share
# (base, chaos-mesh, controlplane, dizzy, infrastructure, nfs, prometheus)
# satisfies this check for all of them at once, and the check does not tell
# those directories apart. check_overlay_table prints one problem per line,
# sorted, and nothing when the table and the figure match the tree. The
# fixture tests prove each check on a scratch tree before the last test runs
# it on the repository.
#
# Usage: bash tests/unit/docs/deploy_overlay_table_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TAB="$(printf '\t')"
FIGURE="deploy-overlay-inheritance"

# The entries of the top-level resources: list, one per line.
# shellcheck disable=SC2016 # an awk program, not shell
RESOURCES_AWK='
/^resources:/ { list = 1; next }
list && /^[A-Za-z]/ { list = 0 }
list && /^  - / { entry = substr($0, 5); sub(/[ \t\r]+$/, "", entry); print entry }
'

# "TABLE" once the header row is found, then one line per row: directory,
# base cell and applier cell without backticks in the first two, separated
# by tabs.
# shellcheck disable=SC2016 # an awk program, not shell
TABLE_AWK='
function trim(s) { sub(/^[ \t]+/, "", s); sub(/[ \t\r]+$/, "", s); return s }
$0 == "## Kustomize Overlay Structure" { section = 1; next }
section && !table && $0 == "| Directory | Base | Applied by |" { table = 1; print "TABLE"; next }
table {
  if ($0 !~ /^\|/) exit
  if ($0 ~ /^\| *-+ *\|/) next
  split($0, cell, "|")
  dir = cell[2]; base = cell[3]
  gsub(/`/, "", dir); gsub(/`/, "", base)
  print trim(dir) "\t" trim(base) "\t" trim(cell[4])
}
'

# One line per label: the value of a cell of a .drawio, the content of a
# <text> of an .svg.
# shellcheck disable=SC2016 # an awk program, not shell
LABELS_AWK='
{
  line = $0
  while (match(line, /value="[^"]*"|>[^<>]*<\/text>/)) {
    s = substr(line, RSTART, RLENGTH); line = substr(line, RSTART + RLENGTH)
    sub(/^value="/, "", s); sub(/"$/, "", s); sub(/^>/, "", s); sub(/<\/text>$/, "", s)
    if (s != "") print s
  }
}
'

# "not in the figure: <directory> (<file>)" for every directory, read as
# "D<tab><directory>", that no label, read as "L<tab><label>", names by its
# path or by its last path element with no letter, digit, "_" or "-" beside
# it.
# shellcheck disable=SC2016 # an awk program, not shell
FIGURE_AWK='
function has_word(label, s,   l, pos, i) {
  l = " " label " "; pos = 0
  while ((i = index(substr(l, pos + 1), s)) > 0) {
    pos += i
    if (substr(l, pos - 1, 1) !~ /[A-Za-z0-9_-]/ && substr(l, pos + length(s), 1) !~ /[A-Za-z0-9_-]/)
      return 1
  }
  return 0
}
$1 == "D" { dirs[++n] = $2; next }
$1 == "L" { labels[++m] = $2 }
END {
  for (i = 1; i <= n; i++) {
    d = dirs[i]; b = d; sub(/.*\//, "", b); found = 0
    for (j = 1; j <= m && !found; j++) found = has_word(labels[j], d) || has_word(labels[j], b)
    if (!found) print "not in the figure: " d " (" figure ")"
  }
}
'

# check_overlay_table <repo-root> <page> <diagrams-dir>: print one problem
# per line, sorted, and nothing when the table lists every directory with its
# bases and the figure names every directory. A tree without a
# kustomization.yaml under deploy/, a missing page, a page without the table
# and a missing file of the figure are each the only line.
check_overlay_table() {
  local root="$1" page="$2" diagrams="$3"
  local canon files file dir rel entry bases records="" rows labels label

  canon="$(cd "$root" 2>/dev/null && pwd -P)"
  files="$(find "$root/deploy" -name kustomization.yaml -type f 2>/dev/null | LC_ALL=C sort)"
  if [[ -z "$canon" || -z "$files" ]]; then
    echo "no kustomization.yaml under $root/deploy"
    return
  fi
  if [[ ! -f "$page" ]]; then
    echo "missing $page"
    return
  fi
  rows="$(awk "$TABLE_AWK" "$page")"
  if [[ -z "$rows" ]]; then
    echo "no overlay table in $page"
    return
  fi
  for file in "$diagrams/$FIGURE.drawio" "$diagrams/$FIGURE.svg"; do
    if [[ ! -f "$file" ]]; then
      echo "missing $file"
      return
    fi
  done

  while IFS= read -r file; do
    dir="$(cd "$(dirname "$file")" && pwd -P)"
    rel="${dir#"$canon"/}"
    bases="$(
      awk "$RESOURCES_AWK" "$file" | while IFS= read -r entry; do
        if [[ -f "$dir/$entry/kustomization.yaml" ]]; then
          entry="$(cd "$dir/$entry" && pwd -P)"
          printf '%s\n' "${entry#"$canon"/}"
        fi
      done | LC_ALL=C sort | awk 'NR > 1 { printf ", " } { printf "%s", $0 }'
    )"
    records="$records$rel$TAB${bases:-none}"$'\n'
  done <<<"$files"

  {
    {
      sed "s/^/M$TAB/" <<<"${records%$'\n'}"
      sed "s/^/T$TAB/" <<<"$rows"
    } | awk -F'\t' '
      # sorted <cell>: the names of a base cell, sorted, joined by ", ".
      function sorted(cell,   n, names, i, j, t, out) {
        n = split(cell, names, /, */)
        for (i = 2; i <= n; i++)
          for (j = i; j > 1 && names[j - 1] > names[j]; j--) {
            t = names[j]; names[j] = names[j - 1]; names[j - 1] = t
          }
        out = ""
        for (i = 1; i <= n; i++) out = out (i > 1 ? ", " : "") names[i]
        return out
      }
      $1 == "M" { base[$2] = $3; dirs[++n] = $2; next }
      $1 == "T" && $2 != "TABLE" {
        row[$2] = sorted($3); listed[++m] = $2
        if ($4 == "") print "no applier: " $2
      }
      END {
        for (i = 1; i <= n; i++) {
          d = dirs[i]
          if (!(d in row)) print "directory without a row: " d
          else if (row[d] != base[d])
            printf "base differs: %s: kustomization \"%s\", table \"%s\"\n", d, base[d], row[d]
        }
        for (j = 1; j <= m; j++)
          if (!(listed[j] in base)) print "row without a kustomization.yaml: " listed[j]
      }
    '
    for file in "$FIGURE.drawio" "$FIGURE.svg"; do
      labels="$(awk "$LABELS_AWK" "$diagrams/$file")"
      {
        cut -f1 <<<"${records%$'\n'}" | sed "s/^/D$TAB/"
        sed "s/^/L$TAB/" <<<"$labels"
      } | awk -F'\t' -v figure="$file" "$FIGURE_AWK"
      # A label that is a path names a directory.
      while IFS= read -r label; do
        case "$label" in
          deploy/*[!A-Za-z0-9/_.-]*) ;;
          deploy/*) [[ -d "$root/$label" ]] || echo "label without a directory: $label ($file)" ;;
        esac
      done <<<"$labels"
    done
  } | LC_ALL=C sort
}

# ---------------------------------------------------------------------------
# Fixture
# ---------------------------------------------------------------------------

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

# build_fixture: write a consistent tree and print its root. deploy/base has a
# resources: list of one file, deploy/overlay lists ../base, a file and a
# directory without a kustomization.yaml, and carries an indented resources:
# key with a list item inside a patch; deploy/solo has no resources: list.
# page.md holds the table, diagrams/ the figure: deploy/base and
# deploy/overlay by their paths, solo by its last path element, and the
# directory deploy/plain without a kustomization.yaml.
build_fixture() {
  local root
  root="$(mktemp -d "$TMP_ROOT/fixture.XXXXXX")"
  mkdir -p "$root/deploy/base" "$root/deploy/overlay" "$root/deploy/plain" "$root/deploy/solo" \
    "$root/diagrams"
  cat >"$root/deploy/base/kustomization.yaml" <<'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - release.yaml
EOF
  cat >"$root/deploy/overlay/kustomization.yaml" <<'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  # The production base.
  - ../base

  - extra.yaml
  - ../plain
patches:
  - target:
      kind: HelmRelease
    patch: |-
      - op: add
        path: /spec/values/resources
        value: {}
images:
  - name: controller
    resources:
  - ../solo
EOF
  cat >"$root/deploy/solo/kustomization.yaml" <<'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
configMapGenerator:
  - name: settings
EOF
  cat >"$root/page.md" <<'EOF'
## Kustomize Overlay Structure

The overlays.

| Directory | Base | Applied by |
| --- | --- | --- |
| `deploy/base` | none | a person |
| `deploy/overlay` | `deploy/base` | the script, Step 3 |
| `deploy/solo` | none | nothing on its own |

The overlays reference the production manifests.
EOF
  cat >"$root/diagrams/$FIGURE.drawio" <<'EOF'
<mxfile host="drawio" type="device"><diagram id="o" name="O"><mxGraphModel><root>
<mxCell id="o-1" value="deploy/base" style="text;" vertex="1" parent="1"/>
<mxCell id="o-2" value="" style="rounded=1;" vertex="1" parent="1"/>
<mxCell id="o-3" value="deploy/overlay" style="text;" vertex="1" parent="1"/><mxCell id="o-4" value="deploy/plain" style="text;" vertex="1" parent="1"/>
<mxCell id="o-5" value="solo: nothing on its own" style="text;" vertex="1" parent="1"/>
</root></mxGraphModel></diagram></mxfile>
EOF
  cat >"$root/diagrams/$FIGURE.svg" <<'EOF'
<svg xmlns="http://www.w3.org/2000/svg">
<text x="0" y="0">deploy/base</text>
<text x="0" y="0">deploy/overlay</text><text x="0" y="0">deploy/plain</text>
<text x="0" y="0">solo: nothing on its own</text>
</svg>
EOF
  printf '%s\n' "$root"
}

# check <root>: run the check on a fixture tree.
check() {
  check_overlay_table "$1" "$1/page.md" "$1/diagrams"
}

# rewrite <file> <sed-script>: apply a sed script in place (BSD and GNU sed).
rewrite() {
  sed "$2" "$1" >"$1.tmp" && mv "$1.tmp" "$1"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_consistent_fixture_reports_nothing() {
  echo "Test: a table that lists every directory with its bases reports nothing"

  local root
  root="$(build_fixture)"
  assert_eq "no problems" "" "$(check "$root")"
}

test_directory_without_a_row() {
  echo "Test: a directory with a kustomization.yaml and no row is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" '/^| `deploy\/solo` |/d'
  assert_eq "solo has no row" "directory without a row: deploy/solo" "$(check "$root")"
}

test_row_without_a_directory() {
  echo "Test: a row for a directory without a kustomization.yaml is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's/^| `deploy\/solo` |.*/&\
| `deploy\/plain` | none | nobody |/'
  assert_eq "plain has no kustomization.yaml" \
    "row without a kustomization.yaml: deploy/plain" "$(check "$root")"
}

test_wrong_base() {
  echo "Test: a row that names another base is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's/^| `deploy\/overlay` | `deploy\/base` |/| `deploy\/overlay` | `deploy\/solo` |/'
  assert_eq "solo in place of base" \
    'base differs: deploy/overlay: kustomization "deploy/base", table "deploy/solo"' "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/page.md" 's/^| `deploy\/overlay` | `deploy\/base` |/| `deploy\/overlay` | none |/'
  assert_eq "none for a directory with a base" \
    'base differs: deploy/overlay: kustomization "deploy/base", table "none"' "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/page.md" 's/^| `deploy\/solo` | none |/| `deploy\/solo` | `deploy\/base` |/'
  assert_eq "a base for a directory without a resources: list" \
    'base differs: deploy/solo: kustomization "none", table "deploy/base"' "$(check "$root")"
}

test_two_bases_compare_sorted() {
  echo "Test: two bases compare as a sorted list"

  local root
  root="$(build_fixture)"
  rewrite "$root/deploy/overlay/kustomization.yaml" 's#^  - ../plain$#  - ../solo#'
  rewrite "$root/page.md" 's/^| `deploy\/overlay` | `deploy\/base` |/| `deploy\/overlay` | `deploy\/solo`, `deploy\/base` |/'
  assert_eq "solo, base against base, solo" "" "$(check "$root")"

  rewrite "$root/page.md" 's/^| `deploy\/overlay` | `deploy\/solo`, `deploy\/base` |/| `deploy\/overlay` | `deploy\/base` |/'
  assert_eq "one of two bases" \
    'base differs: deploy/overlay: kustomization "deploy/base, deploy/solo", table "deploy/base"' \
    "$(check "$root")"
}

test_file_entry_is_no_base() {
  echo "Test: a resources entry that names a file is no base"

  local root
  root="$(build_fixture)"
  : >"$root/deploy/base/release.yaml"
  rewrite "$root/deploy/overlay/kustomization.yaml" 's#^  - extra.yaml$#  - ../base/release.yaml#'
  assert_eq "a file in the base directory" "" "$(check "$root")"
}

test_empty_applier() {
  echo "Test: a row with an empty third cell is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's/^| `deploy\/solo` | none | nothing on its own |/| `deploy\/solo` | none | |/'
  assert_eq "no applier" "no applier: deploy/solo" "$(check "$root")"
}

test_directory_missing_from_the_figure() {
  echo "Test: a directory that a file of the figure does not name is reported for that file"

  local root
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.drawio" 's/value="solo: nothing on its own"/value="nothing on its own"/'
  assert_eq "solo missing from the drawio" \
    "not in the figure: deploy/solo ($FIGURE.drawio)" "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.svg" 's#>deploy/overlay<#>deploy/overlays<#'
  assert_eq "overlay only inside a longer path in the svg" \
    "label without a directory: deploy/overlays ($FIGURE.svg)"$'\n'"not in the figure: deploy/overlay ($FIGURE.svg)" \
    "$(check "$root")"

  # The last path element counts only as a word of its own.
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.drawio" 's/value="solo: nothing on its own"/value="solo-run, resolo"/'
  assert_eq "solo inside longer words" \
    "not in the figure: deploy/solo ($FIGURE.drawio)" "$(check "$root")"
}

test_label_without_a_directory() {
  echo "Test: a label that is a path to no directory is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.drawio" 's#value="deploy/plain"#value="deploy/gone"#'
  assert_eq "deploy/gone in the drawio" \
    "label without a directory: deploy/gone ($FIGURE.drawio)" "$(check "$root")"

  # A label that only mentions a path is not a path.
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.svg" 's#>deploy/plain<#>four directories under deploy/gone<#'
  assert_eq "a sentence with a path" "" "$(check "$root")"
}

test_no_kustomization_is_the_only_report() {
  echo "Test: a tree without a kustomization.yaml under deploy/ is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/page.md"
  rm "$root"/deploy/*/kustomization.yaml
  assert_eq "no kustomization.yaml" "no kustomization.yaml under $root/deploy" "$(check "$root")"

  rm -r "$root/deploy"
  assert_eq "no deploy directory" "no kustomization.yaml under $root/deploy" "$(check "$root")"
}

test_missing_page_is_the_only_report() {
  echo "Test: a missing page is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/page.md"
  rm "$root/deploy/solo/kustomization.yaml"
  assert_eq "no page" "missing $root/page.md" "$(check "$root")"
}

test_page_without_table_is_the_only_report() {
  echo "Test: a page without the overlay table is the only report"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's/^| Directory | Base | Applied by |$/| Directory | Base |/'
  rm "$root/deploy/solo/kustomization.yaml"
  assert_eq "another header row" "no overlay table in $root/page.md" "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/page.md" 's/^## Kustomize Overlay Structure$/## Overlays/'
  assert_eq "no Kustomize Overlay Structure heading" "no overlay table in $root/page.md" \
    "$(check "$root")"
}

test_missing_figure_file_is_the_only_report() {
  echo "Test: a missing file of the figure is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.drawio"
  rewrite "$root/page.md" '/^| `deploy\/solo` |/d'
  assert_eq "no drawio" "missing $root/diagrams/$FIGURE.drawio" "$(check "$root")"

  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.svg"
  assert_eq "no svg" "missing $root/diagrams/$FIGURE.svg" "$(check "$root")"
}

test_repository_table_matches_the_tree() {
  echo "Test: the repository's overlay table and figure match the kustomizations under deploy/"

  local page="$PROJECT_ROOT/docs/reference/infrastructure/e2e-deployment.md"
  local diagrams="$PROJECT_ROOT/docs/diagrams"
  local copy
  assert_eq "no problems" "" "$(check_overlay_table "$PROJECT_ROOT" "$page" "$diagrams")"

  # The same check on a copy of the page without the deploy/kind/vpa row
  # proves it reads the real table and the real tree.
  copy="$(mktemp -d "$TMP_ROOT/copy.XXXXXX")"
  cp "$page" "$copy/e2e-deployment.md"
  rewrite "$copy/e2e-deployment.md" '/^| `deploy\/kind\/vpa` |/d'
  assert_eq "a copy without deploy/kind/vpa" "directory without a row: deploy/kind/vpa" \
    "$(check_overlay_table "$PROJECT_ROOT" "$copy/e2e-deployment.md" "$diagrams")"

  # A copy of the figure without vpa in the svg proves it reads the real
  # labels.
  copy="$(mktemp -d "$TMP_ROOT/copy.XXXXXX")"
  cp "$diagrams/$FIGURE.drawio" "$diagrams/$FIGURE.svg" "$copy/"
  rewrite "$copy/$FIGURE.svg" 's/>metrics-server, vpa: />metrics-server: /'
  assert_eq "a figure copy without vpa" "not in the figure: deploy/kind/vpa ($FIGURE.svg)" \
    "$(check_overlay_table "$PROJECT_ROOT" "$page" "$copy")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_consistent_fixture_reports_nothing
test_directory_without_a_row
test_row_without_a_directory
test_wrong_base
test_two_bases_compare_sorted
test_file_entry_is_no_base
test_empty_applier
test_directory_missing_from_the_figure
test_label_without_a_directory
test_no_kustomization_is_the_only_report
test_missing_page_is_the_only_report
test_page_without_table_is_the_only_report
test_missing_figure_file_is_the_only_report
test_repository_table_matches_the_tree

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
