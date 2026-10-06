#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the figures name the operator constants they restate.
#
# Some pairs under docs/diagrams/ print a value an operator defines as a Go
# constant, `<constant> = "<value>"`, at the top level or in a const block.
# FIGURE_CONSTANTS lists one such value per row. check_figure_constants reads
# each constant's value, puts it into the row's label and looks for that label
# in both files of the pair: in the .drawio and in the .svg. It prints one
# problem per line and nothing when every figure names every value. The check
# runs one way: a value the figure prints and no row lists is not reported.
# The fixture tests prove each check on a scratch tree before the last test
# runs it on the repository.
#
# Usage: bash tests/unit/docs/figure_constants_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# One row per restated constant: <figure>|<Go file>|<constant>|<label>. The Go
# file is relative to the repository root. The label is the text the figure
# prints, with %s where the value stands, so that a value that only appears
# elsewhere in the figure does not count.
FIGURE_CONSTANTS="\
service-cinder-nfs-mounts|operators/cinder/internal/controller/reconcile_backends.go|nfsMountPointBase|%s/{md5}
service-cinder-nfs-mounts|operators/cinder/internal/controller/reconcile_backup_backend.go|backupMountPointBase|%s/{md5}
service-cinder-nfs-mounts|operators/cinder/internal/controller/reconcile_volumeservices.go|nfsCSIDriverName|driver %s:"

# check_figure_constants <root> <diagrams-dir> <rows>: print one problem per
# line, sorted and without repeats, and nothing when both files of every
# figure carry the label of every row, once for each row of the figure that
# comes out with the same label. A missing Go file or constant is the only
# line of its row. A label a file does not print is "not in" it; one it prints
# fewer times than rows come out with it is "too few in" it.
check_figure_constants() {
  local root="$1" dir="$2" rows="$3"
  local figure file constant label value needle pattern seen="" line sharers wanted count ext

  while IFS='|' read -r figure file constant label; do
    if [[ ! -f "$root/$file" ]]; then
      echo "no Go file: $file"
      continue
    fi
    value="$(sed -n "s/^[[:space:]]*\(const[[:space:]]*\)*${constant}[[:space:]]*=[[:space:]]*\"\([^\"]*\)\".*/\2/p" "$root/$file")"
    if [[ -z "$value" ]]; then
      echo "no constant $constant in $file"
      continue
    fi
    needle="${label%%\%s*}$value${label#*\%s}"
    # Bound the label on both sides, so a value that is only a prefix or a
    # suffix of the name the figure prints does not count.
    pattern="(^|[^[:alnum:]_./-])$(printf '%s' "$needle" | sed 's/[][\.*^$(){}+?|]/\\&/g')([^[:alnum:]_./-]|\$)"
    # A label that an earlier row of the figure came out with too needs one
    # more occurrence, so a value changed to another row's value is reported.
    # A shortfall names the constants of all those rows, the changed one too.
    seen+="$constant $figure|$needle"$'\n'
    sharers="" wanted=0
    while IFS= read -r line; do
      if [[ "${line#* }" == "$figure|$needle" ]]; then
        sharers+="${sharers:+, }${line%% *}"
        wanted=$((wanted + 1))
      fi
    done <<<"$seen"
    for ext in drawio svg; do
      if [[ ! -f "$dir/$figure.$ext" ]]; then
        echo "missing figure: $figure.$ext"
        continue
      fi
      count=$(($(grep -E -o -- "$pattern" "$dir/$figure.$ext" | wc -l)))
      if ((count == 0)); then
        echo "not in $ext: $figure: $needle"
      elif ((count < wanted)); then
        echo "too few in $ext: $figure: $needle: printed $count of $wanted times, once each for $sharers"
      fi
    done
  done <<<"$rows" | LC_ALL=C sort -u
}

# ---------------------------------------------------------------------------
# Fixture
# ---------------------------------------------------------------------------

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

FIXTURE_ROWS="\
fig|op/a.go|mountBase|%s/{md5}
fig|op/a.go|volumeName|emptyDir %s
fig|op/b.go|driverName|driver %s:"

# build_fixture: write a consistent tree and print its root. op/a.go defines
# one constant at the top level and one in a const block, padded the way gofmt
# aligns a shorter name and with a trailing comment. The figure prints the
# volume name a second time inside a path, outside its label.
build_fixture() {
  local root label
  root="$(mktemp -d "$TMP_ROOT/fixture.XXXXXX")"
  mkdir -p "$root/op" "$root/diagrams"
  cat >"$root/op/a.go" <<'EOF'
package controller

// mountBase is a top-level constant.
const mountBase = "/var/lib/svc/mnt"

const (
	volumeName     = "staging" // the emptyDir
	tasksVolumeKey = "tasks"
)
EOF
  printf 'package controller\n\nconst driverName = "csi.example.io"\n' >"$root/op/b.go"

  local labels=(
    "/var/lib/svc/mnt/{md5}"
    "emptyDir staging"
    "/var/lib/svc/staging"
    "an inline volume of the driver csi.example.io: the kubelet mounts it"
  )
  {
    echo '<mxfile host="drawio" type="device"><diagram id="f" name="F"><mxGraphModel><root>'
    for label in "${labels[@]}"; do
      echo "<mxCell value=\"$label\" vertex=\"1\" parent=\"1\"/>"
    done
    echo '</root></mxGraphModel></diagram></mxfile>'
  } >"$root/diagrams/fig.drawio"
  {
    echo '<svg xmlns="http://www.w3.org/2000/svg">'
    for label in "${labels[@]}"; do
      echo "<text x=\"0\" y=\"0\">$label</text>"
    done
    echo '</svg>'
  } >"$root/diagrams/fig.svg"
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
  echo "Test: a figure that names every constant reports nothing"

  local root
  root="$(build_fixture)"
  assert_eq "no problems" "" \
    "$(check_figure_constants "$root" "$root/diagrams" "$FIXTURE_ROWS")"
}

test_missing_go_file_is_the_only_report_of_its_row() {
  echo "Test: a Go file that is missing is the only report of its row"

  local root
  root="$(build_fixture)"
  rm "$root/op/b.go"
  rewrite "$root/diagrams/fig.svg" '\#>/var/lib/svc/mnt/{md5}<#d'
  assert_eq "no op/b.go, the other rows are still checked" \
    "no Go file: op/b.go"$'\n'"not in svg: fig: /var/lib/svc/mnt/{md5}" \
    "$(check_figure_constants "$root" "$root/diagrams" "$FIXTURE_ROWS")"
}

test_missing_constant_is_the_only_report_of_its_row() {
  echo "Test: a constant the Go file no longer defines is the only report of its row"

  local root
  root="$(build_fixture)"
  rewrite "$root/op/a.go" 's/^const mountBase = /const mountBasePath = /'
  assert_eq "a renamed top-level constant" "no constant mountBase in op/a.go" \
    "$(check_figure_constants "$root" "$root/diagrams" "$FIXTURE_ROWS")"
}

test_missing_figure_file_skips_only_its_searches() {
  echo "Test: a missing file of the pair is reported once and the other file is still searched"

  local root
  root="$(build_fixture)"
  rm "$root/diagrams/fig.drawio"
  rewrite "$root/diagrams/fig.svg" '/driver csi.example.io:/d'
  assert_eq "no drawio, the svg is still searched" \
    "missing figure: fig.drawio"$'\n'"not in svg: fig: driver csi.example.io:" \
    "$(check_figure_constants "$root" "$root/diagrams" "$FIXTURE_ROWS")"
}

# volumeName is the padded constant with a trailing comment, so this also
# proves that its value is read past the padding and without the comment.
test_changed_value_is_reported_in_both_files() {
  echo "Test: a value changed in the Go file is reported in both files"

  local root
  root="$(build_fixture)"
  rewrite "$root/op/a.go" 's/"staging"/"import-staging"/'
  assert_eq "a changed volume name" \
    "not in drawio: fig: emptyDir import-staging"$'\n'"not in svg: fig: emptyDir import-staging" \
    "$(check_figure_constants "$root" "$root/diagrams" "$FIXTURE_ROWS")"
}

test_value_outside_its_label_is_reported() {
  echo "Test: a value the figure prints only outside its label is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/diagrams/fig.drawio" 's/emptyDir staging/emptyDir scratch/'
  assert_eq "staging left only in a path" "not in drawio: fig: emptyDir staging" \
    "$(check_figure_constants "$root" "$root/diagrams" "$FIXTURE_ROWS")"
}

test_value_renamed_to_a_prefix_is_reported() {
  echo "Test: a value renamed to a prefix of the printed name is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/op/a.go" 's/"staging"/"stag"/'
  assert_eq "a shortened volume name" \
    "not in drawio: fig: emptyDir stag"$'\n'"not in svg: fig: emptyDir stag" \
    "$(check_figure_constants "$root" "$root/diagrams" "$FIXTURE_ROWS")"
}

test_value_renamed_to_a_suffix_is_reported() {
  echo "Test: a value renamed to a suffix of the printed name is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/op/a.go" 's#"/var/lib/svc/mnt"#"svc/mnt"#'
  assert_eq "a shortened mount base" \
    "not in drawio: fig: svc/mnt/{md5}"$'\n'"not in svg: fig: svc/mnt/{md5}" \
    "$(check_figure_constants "$root" "$root/diagrams" "$FIXTURE_ROWS")"
}

# backupBase takes the value of mountBase, so its row comes out with the label
# the figure prints for mountBase.
test_label_two_rows_share_is_needed_once_per_row() {
  echo "Test: a label two rows of a figure come out with is needed once per row"

  local root rows
  root="$(build_fixture)"
  printf 'package controller\n\nconst backupBase = "/var/lib/svc/mnt"\n' >"$root/op/c.go"
  rows="$FIXTURE_ROWS"$'\n'"fig|op/c.go|backupBase|%s/{md5}"
  assert_eq "the label printed once" \
    "too few in drawio: fig: /var/lib/svc/mnt/{md5}: printed 1 of 2 times, once each for mountBase, backupBase"$'\n'"too few in svg: fig: /var/lib/svc/mnt/{md5}: printed 1 of 2 times, once each for mountBase, backupBase" \
    "$(check_figure_constants "$root" "$root/diagrams" "$rows")"

  rewrite "$root/diagrams/fig.drawio" '\#"/var/lib/svc/mnt/{md5}"#p'
  rewrite "$root/diagrams/fig.svg" '\#>/var/lib/svc/mnt/{md5}<#p'
  assert_eq "the label printed for both rows" "" \
    "$(check_figure_constants "$root" "$root/diagrams" "$rows")"

  rewrite "$root/diagrams/fig.drawio" '\#"/var/lib/svc/mnt/{md5}"#d'
  rewrite "$root/diagrams/fig.svg" '\#>/var/lib/svc/mnt/{md5}<#d'
  assert_eq "the label not printed" \
    "not in drawio: fig: /var/lib/svc/mnt/{md5}"$'\n'"not in svg: fig: /var/lib/svc/mnt/{md5}" \
    "$(check_figure_constants "$root" "$root/diagrams" "$rows")"
}

test_repository_figures_name_every_constant() {
  echo "Test: the repository's figures name every constant FIGURE_CONSTANTS lists"

  local copy figure
  assert_eq "no problems in docs/diagrams" "" \
    "$(check_figure_constants "$PROJECT_ROOT" "$PROJECT_ROOT/docs/diagrams" "$FIGURE_CONSTANTS")"

  # The same check on a copy whose caption names another CSI driver proves it
  # reads the real figures.
  copy="$(mktemp -d "$TMP_ROOT/copy.XXXXXX")"
  while IFS= read -r figure; do
    cp "$PROJECT_ROOT/docs/diagrams/$figure.drawio" "$PROJECT_ROOT/docs/diagrams/$figure.svg" "$copy/"
  done < <(cut -d'|' -f1 <<<"$FIGURE_CONSTANTS" | sort -u)
  rewrite "$copy/service-cinder-nfs-mounts.svg" 's/nfs\.csi\.k8s\.io/nfs.example.io/'
  assert_eq "a copy with another driver" \
    "not in svg: service-cinder-nfs-mounts: driver nfs.csi.k8s.io:" \
    "$(check_figure_constants "$PROJECT_ROOT" "$copy" "$FIGURE_CONSTANTS")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_consistent_fixture_reports_nothing
test_missing_go_file_is_the_only_report_of_its_row
test_missing_constant_is_the_only_report_of_its_row
test_missing_figure_file_skips_only_its_searches
test_changed_value_is_reported_in_both_files
test_value_outside_its_label_is_reported
test_value_renamed_to_a_prefix_is_reported
test_value_renamed_to_a_suffix_is_reported
test_label_two_rows_share_is_needed_once_per_row
test_repository_figures_name_every_constant

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
