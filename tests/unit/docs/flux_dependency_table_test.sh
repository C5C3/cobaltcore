#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the dependency table and figure of the Flux releases name what the
# manifests declare.
#
# "### Dependency Order" of docs/reference/infrastructure/
# infrastructure-manifests.md holds a table with one row per HelmRelease of
# deploy/flux-system/releases/ and its spec.dependsOn names in the order of
# the manifest, and the figure deploy-flux-dependencies draws every
# HelmRelease and every Flux Kustomization of that directory as a pill, and
# nothing else. check_flux_dependencies reads the manifests as plain text: a
# line "---" starts a new document, a document's kind is its first line that
# starts with "kind: ", its name the first "  name: " line after
# "metadata:", and its dependencies the "    - name: " lines between
# "  dependsOn:" and the next line that starts with two spaces and a letter.
# A pill is a cell of the .drawio in the style "arcSize=40;". It prints one
# problem per line, sorted, and nothing when the table and both files of the
# figure agree with the manifests. The fixture tests prove each check on a
# scratch tree before the last test runs it on the repository.
#
# Usage: bash tests/unit/docs/flux_dependency_table_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

FIGURE="deploy-flux-dependencies"
TAB="$(printf '\t')"

# One line per HelmRelease or Flux Kustomization: kind, name and the
# dependsOn names joined by ", ", separated by tabs.
# shellcheck disable=SC2016 # an awk program, not shell
RELEASES_AWK='
function trim(s) { sub(/^[ \t]+/, "", s); sub(/[ \t\r]+$/, "", s); return s }
function emit() {
  if ((kind == "HelmRelease" || kind == "Kustomization") && name != "")
    print kind "\t" name "\t" deps
}
FNR == 1 { emit(); kind = ""; meta = 0; name = ""; dep = 0; deps = "" }
/^---[ \t]*$/ { emit(); kind = ""; meta = 0; name = ""; dep = 0; deps = ""; next }
kind == "" && /^kind: / { kind = trim(substr($0, 7)) }
/^metadata:/ { meta = 1; next }
meta && name == "" && /^  name: / { name = trim(substr($0, 9)) }
dep && /^  [A-Za-z]/ { dep = 0 }
/^  dependsOn:/ { dep = 1; next }
dep && /^    - name: / {
  d = trim(substr($0, 13))
  deps = (deps == "" ? d : deps ", " d)
}
END { emit() }
'

# "TABLE" once the header row is found, then one line per row: the release
# and the dependsOn cell without backticks, separated by a tab.
# shellcheck disable=SC2016 # an awk program, not shell
TABLE_AWK='
function trim(s) { sub(/^[ \t]+/, "", s); sub(/[ \t\r]+$/, "", s); return s }
$0 == "### Dependency Order" { section = 1; next }
section && !table && $0 == "| Release | `dependsOn` |" { table = 1; print "TABLE"; next }
table {
  if ($0 !~ /^\|/) exit
  if ($0 ~ /^\| *-+ *\|/) next
  split($0, cell, "|")
  release = cell[2]; deps = cell[3]
  gsub(/`/, "", release); gsub(/`/, "", deps)
  print trim(release) "\t" trim(deps)
}
'

# check_flux_dependencies <releases-dir> <page> <diagrams-dir>: print one
# problem per line, sorted, and nothing when the table and the figure agree
# with the manifests. A releases directory without a HelmRelease, a missing
# page, a page without the table and a missing file of the figure are each
# the only line.
check_flux_dependencies() {
  local dir="$1" page="$2" diagrams="$3"
  local drawio="$diagrams/$FIGURE.drawio" svg="$diagrams/$FIGURE.svg"
  local records="" rows f
  local files=()

  if [[ -d "$dir" ]]; then
    for f in "$dir"/*.yaml; do
      [[ -f "$f" ]] && files+=("$f")
    done
  fi
  if [[ ${#files[@]} -gt 0 ]]; then
    records="$(awk "$RELEASES_AWK" "${files[@]}")"
  fi
  if ! grep -q "^HelmRelease$TAB" <<<"$records"; then
    echo "no HelmRelease under $dir"
    return
  fi
  if [[ ! -f "$page" ]]; then
    echo "missing $page"
    return
  fi
  rows="$(awk "$TABLE_AWK" "$page")"
  if [[ -z "$rows" ]]; then
    echo "no dependency table in $page"
    return
  fi
  if [[ ! -f "$drawio" ]]; then
    echo "missing $drawio"
    return
  fi
  if [[ ! -f "$svg" ]]; then
    echo "missing $svg"
    return
  fi

  {
    {
      sed "s/^/M$TAB/" <<<"$records"
      sed "s/^/T$TAB/" <<<"$rows"
    } | awk -F'\t' '
      $1 == "M" && $2 == "HelmRelease" {
        manifest[$3] = ($4 == "" ? "none" : $4); order[++n] = $3; next
      }
      $1 == "T" && $2 != "TABLE" { row[$2] = $3; rows[++m] = $2 }
      END {
        for (i = 1; i <= n; i++) {
          r = order[i]
          if (!(r in row)) print "release without a row: " r
          else if (row[r] != manifest[r])
            printf "dependsOn differs: %s: manifests \"%s\", table \"%s\"\n", r, manifest[r], row[r]
        }
        for (j = 1; j <= m; j++)
          if (!(rows[j] in manifest)) print "row without a release: " rows[j]
      }
    '
    local name
    while IFS="$TAB" read -r _ name _; do
      if ! grep -F -q -- "value=\"$name\"" "$drawio"; then
        echo "not in the figure: $name ($FIGURE.drawio)"
      fi
      if ! grep -F -q -- ">$name<" "$svg"; then
        echo "not in the figure: $name ($FIGURE.svg)"
      fi
    done <<<"$records"
    awk -F'\t' -v figure="$FIGURE.drawio" '
      NR == FNR { record[$2] = 1; next }
      /arcSize=40;/ && /vertex="1"/ && match($0, /value="[^"]*"/) {
        s = substr($0, RSTART, RLENGTH); sub(/^value="/, "", s); sub(/"$/, "", s)
        if (!(s in record)) print "pill without a manifest: " s " (" figure ")"
      }
    ' - "$drawio" <<<"$records"
  } | LC_ALL=C sort
}

# ---------------------------------------------------------------------------
# Fixture
# ---------------------------------------------------------------------------

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

# build_fixture: write a consistent tree and print its root. releases/ holds
# three HelmReleases (alpha without dependsOn, beta with a comment line in
# its block, gamma with two entries), the Flux Kustomization kappa with an
# images list and a comment between metadata: and its name, and a
# GitRepository that is neither. page.md has the table, diagrams/ a pair
# with the four names, as pills in the .drawio.
build_fixture() {
  local root name n=0
  root="$(mktemp -d "$TMP_ROOT/fixture.XXXXXX")"
  mkdir -p "$root/releases" "$root/diagrams"
  cat >"$root/releases/alpha.yaml" <<'EOF'
# alpha waits for nothing.
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: alpha
  namespace: alpha-system
spec:
  interval: 30m
  chart:
    spec:
      sourceRef:
        kind: HelmRepository
        name: alpha
EOF
  cat >"$root/releases/beta.yaml" <<'EOF'
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: beta
  namespace: beta-system
spec:
  interval: 30m
  dependsOn:
    # beta needs the CRDs of alpha.
    - name: alpha
      namespace: alpha-system
  values:
    replicas: 1
EOF
  cat >"$root/releases/gamma.yaml" <<'EOF'
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: gamma
  namespace: gamma-system
spec:
  dependsOn:
    - name: beta
      namespace: beta-system
    - name: alpha
      namespace: alpha-system
  interval: 30m
EOF
  cat >"$root/releases/kappa.yaml" <<'EOF'
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  # Short name for diagnostics.
  name: kappa
  namespace: flux-system
spec:
  images:
    - name: controller
      newTag: v1
EOF
  cat >"$root/releases/sigma.yaml" <<'EOF'
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: sigma
EOF
  cat >"$root/page.md" <<'EOF'
## HelmRelease Operators

### Dependency Order

The releases in layers.

| Release | `dependsOn` |
| --- | --- |
| `alpha` | none |
| `beta` | `alpha` |
| `gamma` | `beta`, `alpha` |

### alpha
EOF
  {
    echo '<mxfile host="drawio" type="device"><diagram id="f" name="F"><mxGraphModel><root>'
    for name in alpha beta gamma kappa; do
      n=$((n + 1))
      echo "<mxCell id=\"f-$n\" value=\"$name\" style=\"rounded=1;absoluteArcSize=1;arcSize=40;\" vertex=\"1\" parent=\"1\"/>"
    done
    echo '</root></mxGraphModel></diagram></mxfile>'
  } >"$root/diagrams/$FIGURE.drawio"
  {
    echo '<svg xmlns="http://www.w3.org/2000/svg">'
    for name in alpha beta gamma kappa; do
      echo "<text x=\"0\" y=\"0\">$name</text>"
    done
    echo '</svg>'
  } >"$root/diagrams/$FIGURE.svg"
  printf '%s\n' "$root"
}

# check <root>: run the check on a fixture tree.
check() {
  check_flux_dependencies "$1/releases" "$1/page.md" "$1/diagrams"
}

# rewrite <file> <sed-script>: apply a sed script in place (BSD and GNU sed).
rewrite() {
  sed "$2" "$1" >"$1.tmp" && mv "$1.tmp" "$1"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_consistent_fixture_reports_nothing() {
  echo "Test: a table and a figure that agree with the manifests report nothing"

  local root
  root="$(build_fixture)"
  assert_eq "no problems" "" "$(check "$root")"
}

test_release_without_a_row() {
  echo "Test: a HelmRelease the table lacks is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" '/^| `beta` |/d'
  assert_eq "beta has no row" "release without a row: beta" "$(check "$root")"
}

test_row_without_a_release() {
  echo "Test: a row that names no HelmRelease is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's/^| `gamma` |.*/&\
| `delta` | `alpha` |/'
  assert_eq "delta has no manifest" "row without a release: delta" "$(check "$root")"

  # The Flux Kustomization needs no row, and a row for it names no
  # HelmRelease either.
  root="$(build_fixture)"
  rewrite "$root/page.md" 's/^| `gamma` |.*/&\
| `kappa` | none |/'
  assert_eq "a row for the Kustomization" "row without a release: kappa" "$(check "$root")"
}

test_differing_name() {
  echo "Test: a row with another name than the manifest is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's/^| `beta` | `alpha` |/| `beta` | `gamma` |/'
  assert_eq "one differing name" \
    'dependsOn differs: beta: manifests "alpha", table "gamma"' "$(check "$root")"
}

test_names_in_another_order() {
  echo "Test: the names of a row stand in the order of the manifest"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's/^| `gamma` | `beta`, `alpha` |/| `gamma` | `alpha`, `beta` |/'
  assert_eq "two names swapped" \
    'dependsOn differs: gamma: manifests "beta, alpha", table "alpha, beta"' "$(check "$root")"
}

test_release_without_dependson() {
  echo "Test: a release without a dependsOn block matches none and nothing else"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's/^| `alpha` | none |/| `alpha` | `beta` |/'
  assert_eq "a name against no block" \
    'dependsOn differs: alpha: manifests "none", table "beta"' "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/page.md" 's/^| `alpha` | none |/| `alpha` | |/'
  assert_eq "an empty cell against no block" \
    'dependsOn differs: alpha: manifests "none", table ""' "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/page.md" 's/^| `beta` | `alpha` |/| `beta` | none |/'
  assert_eq "none against a block" \
    'dependsOn differs: beta: manifests "alpha", table "none"' "$(check "$root")"
}

test_label_missing_in_one_file() {
  echo "Test: a name missing from one file of the figure reports exactly its line"

  local root
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.drawio" '/value="gamma"/d'
  assert_eq "drawio only" "not in the figure: gamma ($FIGURE.drawio)" "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.svg" '/>gamma</d'
  assert_eq "svg only" "not in the figure: gamma ($FIGURE.svg)" "$(check "$root")"
}

test_label_inside_a_longer_label_is_missing() {
  echo "Test: a name that only stands inside a longer label is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.drawio" 's/value="beta"/value="beta-crds"/'
  rewrite "$root/diagrams/$FIGURE.svg" 's/>beta</>beta-crds</'
  assert_eq "beta beside beta-crds" \
    "not in the figure: beta ($FIGURE.drawio)"$'\n'"not in the figure: beta ($FIGURE.svg)"$'\n'"pill without a manifest: beta-crds ($FIGURE.drawio)" \
    "$(check "$root")"
}

test_pill_without_a_manifest() {
  echo "Test: a pill that names no HelmRelease or Kustomization is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.drawio" 's#^</root>#<mxCell value="omega" style="rounded=1;absoluteArcSize=1;arcSize=40;" vertex="1"/>&#'
  assert_eq "omega has no manifest" \
    "pill without a manifest: omega ($FIGURE.drawio)" "$(check "$root")"

  # A label in another style is no pill, and the GitRepository needs none.
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.drawio" 's#^</root>#<mxCell value="sigma" style="text;" vertex="1"/>&#'
  assert_eq "a text cell" "" "$(check "$root")"
}

test_multi_document_file() {
  echo "Test: every document of a file with several is a manifest of its own"

  local root
  root="$(build_fixture)"
  cat >"$root/releases/delta.yaml" <<'EOF'
apiVersion: source.toolkit.fluxcd.io/v1
kind: HelmRepository
metadata:
  name: delta-charts
  namespace: flux-system
spec:
  url: https://example.org/charts
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: delta
  namespace: delta-system
spec:
  dependsOn:
    - name: alpha
      namespace: alpha-system
EOF
  assert_eq "a HelmRelease after a HelmRepository" \
    "not in the figure: delta ($FIGURE.drawio)"$'\n'"not in the figure: delta ($FIGURE.svg)"$'\n'"release without a row: delta" \
    "$(check "$root")"

  # The dependsOn list of a Kustomization in the second document does not
  # count for the HelmRelease of the first.
  root="$(build_fixture)"
  cat >"$root/releases/delta.yaml" <<'EOF'
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: delta
spec:
  interval: 30m
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: epsilon
spec:
  dependsOn:
    - name: kappa
EOF
  rewrite "$root/page.md" 's/^| `gamma` |.*/&\
| `delta` | none |/'
  rewrite "$root/diagrams/$FIGURE.drawio" 's#^</root>#<mxCell value="delta" style="arcSize=40;" vertex="1"/><mxCell value="epsilon" style="arcSize=40;" vertex="1"/>&#'
  rewrite "$root/diagrams/$FIGURE.svg" 's#^</svg>#<text>delta</text><text>epsilon</text>&#'
  assert_eq "a HelmRelease before a Kustomization" "" "$(check "$root")"
}

test_kustomization_is_checked_in_the_figure() {
  echo "Test: a Flux Kustomization needs no row but a pill in both files"

  local root
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.drawio" '/value="kappa"/d'
  rewrite "$root/diagrams/$FIGURE.svg" '/>kappa</d'
  assert_eq "kappa missing from the figure" \
    "not in the figure: kappa ($FIGURE.drawio)"$'\n'"not in the figure: kappa ($FIGURE.svg)" \
    "$(check "$root")"
}

test_no_helmrelease_is_the_only_report() {
  echo "Test: a releases directory without a HelmRelease is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/page.md"
  assert_eq "a directory that does not exist" "no HelmRelease under $root/absent" \
    "$(check_flux_dependencies "$root/absent" "$root/page.md" "$root/diagrams")"

  mkdir "$root/empty"
  assert_eq "an empty directory" "no HelmRelease under $root/empty" \
    "$(check_flux_dependencies "$root/empty" "$root/page.md" "$root/diagrams")"

  rm "$root/releases/alpha.yaml" "$root/releases/beta.yaml" "$root/releases/gamma.yaml"
  assert_eq "a Kustomization and a GitRepository only" "no HelmRelease under $root/releases" \
    "$(check "$root")"
}

test_missing_page_is_the_only_report() {
  echo "Test: a missing page is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/page.md" "$root/diagrams/$FIGURE.svg"
  assert_eq "no page" "missing $root/page.md" "$(check "$root")"
}

test_page_without_table_is_the_only_report() {
  echo "Test: a page without the dependency table is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.drawio"
  rewrite "$root/page.md" 's/^| Release | `dependsOn` |$/| Release | Dependencies |/'
  assert_eq "another header row" "no dependency table in $root/page.md" "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/page.md" 's/^### Dependency Order$/### Install order/'
  assert_eq "no Dependency Order heading" "no dependency table in $root/page.md" "$(check "$root")"
}

test_missing_figure_file_is_the_only_report() {
  echo "Test: a missing file of the figure is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.drawio"
  rewrite "$root/page.md" '/^| `beta` |/d'
  assert_eq "no drawio" "missing $root/diagrams/$FIGURE.drawio" "$(check "$root")"

  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.svg"
  assert_eq "no svg" "missing $root/diagrams/$FIGURE.svg" "$(check "$root")"
}

test_repository_table_and_figure_match_the_releases() {
  echo "Test: the repository's table and figure match deploy/flux-system/releases"

  local releases="$PROJECT_ROOT/deploy/flux-system/releases"
  local page="$PROJECT_ROOT/docs/reference/infrastructure/infrastructure-manifests.md"
  local copy
  assert_eq "no problems" "" \
    "$(check_flux_dependencies "$releases" "$page" "$PROJECT_ROOT/docs/diagrams")"

  # The same check on a copy without the k-orc label proves it reads the real
  # figure and the Kustomizations of the real directory.
  copy="$(mktemp -d "$TMP_ROOT/copy.XXXXXX")"
  cp "$PROJECT_ROOT/docs/diagrams/$FIGURE.drawio" "$PROJECT_ROOT/docs/diagrams/$FIGURE.svg" "$copy/"
  rewrite "$copy/$FIGURE.svg" 's/>k-orc</>K-ORC</'
  assert_eq "a copy without k-orc" "not in the figure: k-orc ($FIGURE.svg)" \
    "$(check_flux_dependencies "$releases" "$page" "$copy")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_consistent_fixture_reports_nothing
test_release_without_a_row
test_row_without_a_release
test_differing_name
test_names_in_another_order
test_release_without_dependson
test_label_missing_in_one_file
test_label_inside_a_longer_label_is_missing
test_pill_without_a_manifest
test_multi_document_file
test_kustomization_is_checked_in_the_figure
test_no_helmrelease_is_the_only_report
test_missing_page_is_the_only_report
test_page_without_table_is_the_only_report
test_missing_figure_file_is_the_only_report
test_repository_table_and_figure_match_the_releases

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
