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
# figure agree with the manifests.
#
# The same page counts the base kustomization three times: the Resource
# count line and the category table of "### Base Kustomization", and the
# sentence under "### Step 1: Apply base resources" that starts with "This
# applies". check_resource_counts reads the files the top-level resources:
# list of deploy/flux-system/kustomization.yaml names, counts their documents
# by kind and holds the three places to the files: the file count, the count
# and the names of every kind, the Total row, and every number of the
# sentence, where "sources" is the sum of the *Repository kinds.
#
# The fixture tests prove each check on a scratch tree before the last tests
# run both checks on the repository.
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
# Resource counts of the base kustomization
# ---------------------------------------------------------------------------

# The files of the top-level resources: list of a kustomization.yaml, one per
# line, without the comment that may follow an entry.
# shellcheck disable=SC2016 # an awk program, not shell
RESOURCES_AWK='
function trim(s) { sub(/^[ \t]+/, "", s); sub(/[ \t\r]+$/, "", s); return s }
/^resources:/ { list = 1; next }
list && /^[^ \t#]/ { list = 0 }
list && /^  - / { s = $0; sub(/^  - /, "", s); sub(/[ \t]+#.*$/, "", s); print trim(s) }
'

# One line per document with a kind: kind and name, separated by a tab.
# shellcheck disable=SC2016 # an awk program, not shell
DOCUMENTS_AWK='
function trim(s) { sub(/^[ \t]+/, "", s); sub(/[ \t\r]+$/, "", s); return s }
function emit() { if (kind != "") print kind "\t" name }
FNR == 1 { emit(); kind = ""; meta = 0; name = "" }
/^---[ \t]*$/ { emit(); kind = ""; meta = 0; name = ""; next }
kind == "" && /^kind: / { kind = trim(substr($0, 7)) }
/^metadata:/ { meta = 1; next }
meta && name == "" && /^  name: / { name = trim(substr($0, 9)) }
END { emit() }
'

# The count statements of the page: "COUNT<tab><line>" for the Resource
# count line of "### Base Kustomization", "TABLE" once the category table is
# found, "ROW<tab>kind<tab>count<tab>names" per row, "TOTAL<tab>count" for
# the Total row, and "SENTENCE<tab>text" for the sentence under "### Step 1"
# that starts with "This applies", joined across lines and cut at its period.
# shellcheck disable=SC2016 # an awk program, not shell
COUNTS_AWK='
function trim(s) { sub(/^[ \t]+/, "", s); sub(/[ \t\r]+$/, "", s); return s }
function flush(   t, i) {
  if (para == "") return
  t = substr(para, index(para, "This applies "))
  i = index(t, ". ")
  if (i > 0) t = substr(t, 1, i)
  print "SENTENCE\t" t
  para = ""
}
/^#/ { flush(); base = ($0 == "### Base Kustomization"); step1 = ($0 == "### Step 1: Apply base resources"); table = 0 }
base && /^\*\*Resource count:\*\* / { print "COUNT\t" $0 }
base && !table && $0 == "| Category | Count | Resources |" { table = 1; print "TABLE"; next }
table {
  if ($0 !~ /^\|/) table = 0
  else if ($0 !~ /^\| *-+ *\|/) {
    split($0, cell, "|")
    kind = trim(cell[2]); count = trim(cell[3]); names = trim(cell[4])
    gsub(/\*/, "", kind); gsub(/\*/, "", count); gsub(/`/, "", names)
    if (kind == "Total") print "TOTAL\t" count
    else print "ROW\t" kind "\t" count "\t" names
  }
}
step1 && para == "" && /This applies / { para = $0; next }
step1 && para != "" { if ($0 ~ /^[ \t]*$/) flush(); else para = para " " $0 }
END { flush() }
'

# The manifests, read as "M<tab>kind<tab>name", against the statements of
# the page and "FILES<tab>n", the number of files the kustomization lists.
# shellcheck disable=SC2016 # an awk program, not shell
COMPARE_AWK='
function trim(s) { sub(/^[ \t]+/, "", s); sub(/[ \t\r]+$/, "", s); return s }
function number(s, which,   n, i) {
  n = 0
  while (match(s, /[0-9]+/)) {
    n++
    if (n == which) return substr(s, RSTART, RLENGTH)
    s = substr(s, RSTART + RLENGTH)
  }
  return ""
}
function join(a, n,   i, s) { for (i = 1; i <= n; i++) s = (i == 1 ? a[i] : s ", " a[i]); return s }
$1 == "M" {
  if (!($2 in count)) kinds[++nk] = $2
  count[$2]++; total++
  names[$2] = (names[$2] == "" ? $3 : names[$2] SUBSEP $3)
  next
}
$1 == "FILES" { files = $2; next }
$1 == "COUNT" { countline = $2; next }
$1 == "ROW" { if (!($2 in row)) rows[++nr] = $2; row[$2] = $3; rownames[$2] = $4; next }
$1 == "TOTAL" { totalrow = $2; next }
$1 == "SENTENCE" { sentence = $2; next }
END {
  for (i = 1; i <= nk; i++) {
    k = kinds[i]
    if (!(k in row)) { print "kind without a row: " k; continue }
    if (row[k] != count[k]) printf "count differs: %s: manifests %d, table %s\n", k, count[k], row[k]
    s = rownames[k]; gsub(/ *\([^)]*\)/, "", s)
    nt = split(s, t, ","); delete intable
    for (j = 1; j <= nt; j++) { t[j] = trim(t[j]); if (t[j] != "") intable[t[j]] = 1 }
    nm = split(names[k], m, SUBSEP); delete inmanifest
    for (j = 1; j <= nm; j++) inmanifest[m[j]] = 1
    no = 0; for (j = 1; j <= nm; j++) if (!(m[j] in intable)) only[++no] = m[j]
    nx = 0; for (j = 1; j <= nt; j++) if (t[j] != "" && !(t[j] in inmanifest)) extra[++nx] = t[j]
    if (no > 0 || nx > 0)
      printf "names differ: %s: manifests only \"%s\", table only \"%s\"\n", k, join(only, no), join(extra, nx)
  }
  for (i = 1; i <= nr; i++) if (!(rows[i] in count)) print "row without a kind: " rows[i]
  if (totalrow == "") print "no Total row"
  else if (totalrow != total) printf "total differs: manifests %d, table %s\n", total, totalrow
  if (countline == "") print "no resource count line"
  else {
    if (number(countline, 1) != files) printf "file count differs: manifests %d, page %s\n", files, number(countline, 1)
    if (number(countline, 2) != total) printf "resource count differs: manifests %d, page %s\n", total, number(countline, 2)
  }
  if (sentence == "") { print "no resources sentence under Step 1"; exit }
  sources = 0
  for (i = 1; i <= nk; i++) if (kinds[i] ~ /Repository$/) sources += count[kinds[i]]
  ntok = split(sentence, tok, /[ ,():]+/)
  for (i = 1; i < ntok; i++) {
    if (tok[i] !~ /^[0-9]+$/) continue
    w = tok[i + 1]
    if (w == "Flux" && i + 2 <= ntok) w = tok[i + 2]
    if (w == "resources") expected = total
    else if (w == "sources") expected = sources
    else {
      kw = w
      if (!(kw in count)) { sub(/s$/, "", kw); kw = toupper(substr(kw, 1, 1)) substr(kw, 2) }
      if (!(kw in count)) { print "step 1 names no kind: " w; continue }
      mentioned[kw] = 1; expected = count[kw]
    }
    if (tok[i] != expected) printf "step 1 differs: %s: manifests %d, page %s\n", w, expected, tok[i]
  }
  for (i = 1; i <= nk; i++) if (!(kinds[i] in mentioned)) print "step 1 without " kinds[i]
}
'

# check_resource_counts <flux-system-dir> <page>: print one problem per
# line, sorted, and nothing when the Resource count line, the category table
# and the sentence under Step 1 of the page state what the files of the
# kustomization hold. A missing kustomization.yaml, one without a file, a
# missing page and a page without the table are each the only line.
check_resource_counts() {
  local dir="$1" page="$2"
  local kfile="$dir/kustomization.yaml"
  local entry docs statements problems=""
  local files=()

  if [[ ! -f "$kfile" ]]; then
    echo "missing $kfile"
    return
  fi
  while IFS= read -r entry; do
    [[ -n "$entry" ]] || continue
    if [[ -f "$dir/$entry" ]]; then
      files+=("$dir/$entry")
    else
      problems="${problems}not a file: $entry ($kfile)"$'\n'
    fi
  done < <(awk "$RESOURCES_AWK" "$kfile")
  if [[ ${#files[@]} -eq 0 ]]; then
    echo "no file under resources in $kfile"
    return
  fi
  if [[ ! -f "$page" ]]; then
    echo "missing $page"
    return
  fi
  statements="$(awk "$COUNTS_AWK" "$page")"
  if ! grep -q '^TABLE$' <<<"$statements"; then
    echo "no resource table in $page"
    return
  fi
  docs="$(awk "$DOCUMENTS_AWK" "${files[@]}")"

  {
    printf '%s' "$problems"
    {
      sed "s/^/M$TAB/" <<<"$docs"
      printf 'FILES\t%s\n' "${#files[@]}"
      printf '%s\n' "$statements"
    } | awk -F'\t' "$COMPARE_AWK"
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

# build_counts_fixture: write a base kustomization and its page and print the
# root. flux/kustomization.yaml lists six files: two Namespaces in one file,
# a FluxInstance, a HelmRepository, two HelmReleases and a Flux
# Kustomization, seven resources. page.md states them under "### Base
# Kustomization" and "### Step 1: Apply base resources".
build_counts_fixture() {
  local root
  root="$(mktemp -d "$TMP_ROOT/counts.XXXXXX")"
  mkdir -p "$root/flux/sources" "$root/flux/releases"
  cat >"$root/flux/kustomization.yaml" <<'EOF2'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
  # Namespaces first.
  - namespaces.yaml
  - fluxinstance.yaml # the instance
  - sources/alpha.yaml
  - releases/alpha.yaml
  - releases/beta.yaml
  - releases/kappa.yaml
EOF2
  cat >"$root/flux/namespaces.yaml" <<'EOF2'
---
apiVersion: v1
kind: Namespace
metadata:
  name: ns-a
---
apiVersion: v1
kind: Namespace
metadata:
  name: ns-b
EOF2
  cat >"$root/flux/fluxinstance.yaml" <<'EOF2'
apiVersion: fluxcd.controlplane.io/v1
kind: FluxInstance
metadata:
  name: flux
  namespace: flux-system
EOF2
  cat >"$root/flux/sources/alpha.yaml" <<'EOF2'
apiVersion: source.toolkit.fluxcd.io/v1
kind: HelmRepository
metadata:
  name: alpha
EOF2
  cat >"$root/flux/releases/alpha.yaml" <<'EOF2'
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: alpha
EOF2
  cat >"$root/flux/releases/beta.yaml" <<'EOF2'
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: beta
EOF2
  cat >"$root/flux/releases/kappa.yaml" <<'EOF2'
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: kappa
EOF2
  cat >"$root/page.md" <<'EOF2'
## Kustomization

### Base Kustomization

**File:** `flux/kustomization.yaml`

**Resource count:** 6 files producing 7 Kubernetes resources.

| Category | Count | Resources |
| --- | --- | --- |
| Namespace | 2 | ns-a, ns-b |
| FluxInstance | 1 | flux (drives the flux-operator) |
| HelmRepository | 1 | alpha |
| HelmRelease | 2 | alpha, beta |
| Kustomization | 1 | kappa |
| **Total** | **7** | |

The table above lists every resource.

### Infrastructure Kustomization

## Deployment

### Step 1: Apply base resources

```bash
kubectl apply -k flux/
```

This applies 7 resources: 2 namespaces, 1 FluxInstance, 1 sources
(1 HelmRepository), 2 HelmReleases and 1 Flux
Kustomizations (kappa). FluxCD resolves the dependency graph between
HelmReleases.

### Step 2: Apply infrastructure resources
EOF2
  printf '%s\n' "$root"
}

# check_counts <root>: run the count check on a fixture tree.
check_counts() {
  check_resource_counts "$1/flux" "$1/page.md"
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

test_counts_consistent_fixture_reports_nothing() {
  echo "Test: a page whose counts match the base kustomization reports nothing"

  local root
  root="$(build_counts_fixture)"
  assert_eq "no problems" "" "$(check_counts "$root")"
}

test_counts_table_row_differs() {
  echo "Test: a row whose count or names differ from the manifests is reported"

  local root
  root="$(build_counts_fixture)"
  rewrite "$root/page.md" 's/^| HelmRelease | 2 | alpha, beta |/| HelmRelease | 3 | alpha, beta |/'
  assert_eq "count off by one" "count differs: HelmRelease: manifests 2, table 3" "$(check_counts "$root")"

  root="$(build_counts_fixture)"
  rewrite "$root/page.md" 's/^| HelmRelease | 2 | alpha, beta |/| HelmRelease | 2 | alpha, gamma |/'
  assert_eq "one name replaced" \
    'names differ: HelmRelease: manifests only "beta", table only "gamma"' "$(check_counts "$root")"

  # The parenthesis after a name is not part of it.
  root="$(build_counts_fixture)"
  rewrite "$root/page.md" 's/^| FluxInstance | 1 | flux (drives the flux-operator) |/| FluxInstance | 1 | flux |/'
  assert_eq "a name without its parenthesis" "" "$(check_counts "$root")"
}

test_counts_kind_and_row_without_partner() {
  echo "Test: a kind without a row and a row without a kind are reported"

  local root
  root="$(build_counts_fixture)"
  rewrite "$root/page.md" '/^| Kustomization | 1 | kappa |/d'
  assert_eq "Kustomization row removed" \
    "kind without a row: Kustomization" "$(check_counts "$root")"

  root="$(build_counts_fixture)"
  rewrite "$root/page.md" 's/^| Kustomization | 1 | kappa |/&\
| OCIRepository | 1 | zeta |/'
  assert_eq "a row for a kind no file holds" \
    "row without a kind: OCIRepository" "$(check_counts "$root")"
}

test_counts_total_row() {
  echo "Test: the Total row is held to the number of documents"

  local root
  root="$(build_counts_fixture)"
  rewrite "$root/page.md" 's/^| \*\*Total\*\* | \*\*7\*\* | |/| **Total** | **8** | |/'
  assert_eq "a wrong total" "total differs: manifests 7, table 8" "$(check_counts "$root")"

  root="$(build_counts_fixture)"
  rewrite "$root/page.md" '/^| \*\*Total\*\* |/d'
  assert_eq "no Total row" "no Total row" "$(check_counts "$root")"
}

test_counts_resource_count_line() {
  echo "Test: the Resource count line is held to the files and the documents"

  local root
  root="$(build_counts_fixture)"
  rewrite "$root/page.md" 's/^\*\*Resource count:\*\* 6 files producing 7/**Resource count:** 5 files producing 8/'
  assert_eq "both numbers wrong" \
    "file count differs: manifests 6, page 5"$'\n'"resource count differs: manifests 7, page 8" \
    "$(check_counts "$root")"

  root="$(build_counts_fixture)"
  rewrite "$root/page.md" '/^\*\*Resource count:\*\*/d'
  assert_eq "no line" "no resource count line" "$(check_counts "$root")"
}

test_counts_step_1_sentence() {
  echo "Test: the numbers of the Step 1 sentence are held to the documents"

  local root
  root="$(build_counts_fixture)"
  rewrite "$root/page.md" 's/, 2 HelmReleases and/, 3 HelmReleases and/'
  assert_eq "a wrong number across the line break" \
    "step 1 differs: HelmReleases: manifests 2, page 3" "$(check_counts "$root")"

  root="$(build_counts_fixture)"
  rewrite "$root/page.md" 's/This applies 7 resources: 2 namespaces, 1 FluxInstance, 1 sources/This applies 8 resources: 2 namespaces, 2 sources/'
  assert_eq "a kind left out and two sums wrong" \
    "step 1 differs: resources: manifests 7, page 8"$'\n'"step 1 differs: sources: manifests 1, page 2"$'\n'"step 1 without FluxInstance" \
    "$(check_counts "$root")"

  root="$(build_counts_fixture)"
  rewrite "$root/page.md" 's/1 FluxInstance, /1 FluxInstance, 3 ImagePolicies, /'
  assert_eq "a word that is no kind" "step 1 names no kind: ImagePolicies" "$(check_counts "$root")"

  root="$(build_counts_fixture)"
  rewrite "$root/page.md" 's/^This applies 7 resources.*/The graph resolves itself./'
  assert_eq "no sentence" "no resources sentence under Step 1" "$(check_counts "$root")"
}

test_counts_only_reports() {
  echo "Test: a missing kustomization, page or table is the only report"

  local root
  root="$(build_counts_fixture)"
  rm "$root/page.md"
  assert_eq "no kustomization" "missing $root/absent/kustomization.yaml" \
    "$(check_resource_counts "$root/absent" "$root/page.md")"
  assert_eq "no page" "missing $root/page.md" "$(check_counts "$root")"

  root="$(build_counts_fixture)"
  rewrite "$root/page.md" 's/^| Category | Count | Resources |/| Kind | Count | Names |/'
  assert_eq "another header row" "no resource table in $root/page.md" "$(check_counts "$root")"

  root="$(build_counts_fixture)"
  rewrite "$root/flux/kustomization.yaml" 's/^  - .*/  - sources/'
  assert_eq "no file among the resources" "no file under resources in $root/flux/kustomization.yaml" \
    "$(check_counts "$root")"

  # A directory among the files is reported beside the other checks, and its
  # documents are not counted.
  root="$(build_counts_fixture)"
  rewrite "$root/flux/kustomization.yaml" 's/^  - sources\/alpha.yaml/  - sources/'
  assert_eq "a directory among the files" \
    "file count differs: manifests 5, page 6"$'\n'"not a file: sources ($root/flux/kustomization.yaml)"$'\n'"resource count differs: manifests 6, page 7"$'\n'"row without a kind: HelmRepository"$'\n'"step 1 differs: resources: manifests 6, page 7"$'\n'"step 1 differs: sources: manifests 0, page 1"$'\n'"step 1 names no kind: HelmRepository"$'\n'"total differs: manifests 6, table 7" \
    "$(check_counts "$root")"
}

test_repository_counts_match_the_base_kustomization() {
  echo "Test: the repository's Resource count, table and Step 1 sentence match deploy/flux-system"

  local flux="$PROJECT_ROOT/deploy/flux-system"
  local page="$PROJECT_ROOT/docs/reference/infrastructure/infrastructure-manifests.md"
  local copy
  assert_eq "no problems" "" "$(check_resource_counts "$flux" "$page")"

  # The same check on a copy with another total proves it reads the real
  # page against the real files.
  copy="$(mktemp -d "$TMP_ROOT/copy.XXXXXX")"
  cp "$page" "$copy/page.md"
  rewrite "$copy/page.md" 's/^| \*\*Total\*\* | \*\*53\*\* | |/| **Total** | **52** | |/'
  assert_eq "a copy with another total" "total differs: manifests 53, table 52" \
    "$(check_resource_counts "$flux" "$copy/page.md")"
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
test_counts_consistent_fixture_reports_nothing
test_counts_table_row_differs
test_counts_kind_and_row_without_partner
test_counts_total_row
test_counts_resource_count_line
test_counts_step_1_sentence
test_counts_only_reports
test_repository_counts_match_the_base_kustomization

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
