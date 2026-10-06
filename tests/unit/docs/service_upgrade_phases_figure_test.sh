#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the upgrade phases figure names every upgrade phase and every reason
# it draws.
#
# docs/diagrams/service-upgrade-phases.drawio and .svg draw the phased release
# upgrade the database-backed service operators share.
# internal/common/types/types.go defines each phase as
# `UpgradePhase<Name> UpgradePhase = "<value>"`, and
# internal/common/database/upgrade.go and flow.go define each reason as
# `Reason<Name> = "<value>"`; REASONS lists the reasons the figure draws.
# check_upgrade_figure looks for every value in both files of the pair as a
# word of a label, since one label can list several reasons: in the .drawio
# inside a cell value, in the .svg inside a <text> element. It prints one
# problem per line and nothing when the figure names every value. The check
# runs one way: a label the figure has and the code lacks is not reported. The
# fixture tests prove each check on a scratch tree before the last test runs
# it on the repository.
#
# Usage: bash tests/unit/docs/service_upgrade_phases_figure_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

FIGURE="service-upgrade-phases"

# The reasons the figure draws, each named by its constant without the Reason
# prefix.
REASONS=(UpgradeTargetChanged VersionParseError DowngradeNotSupported
  UpgradePathInvalid ExpandInProgress ExpandFailed MigrateInProgress
  MigrateFailed ContractInProgress ContractFailed UpgradeRollingUpdate
  DBSyncInProgress DBSyncFailed)

# check_upgrade_figure <types.go> <database-dir> <diagrams-dir>: print one
# problem per line, sorted, and nothing when both files of the figure name
# every phase and every reason. A missing types file or one without a phase
# constant is the only line. A reason whose constant neither upgrade.go nor
# flow.go of <database-dir> defines is reported in place of its value.
check_upgrade_figure() {
  local types="$1" database="$2" dir="$3"
  local drawio="$dir/$FIGURE.drawio" svg="$dir/$FIGURE.svg"
  local values reason value

  if [[ ! -f "$types" ]]; then
    echo "no types file: $types"
    return
  fi

  values="$(sed -n 's/^[[:space:]]*UpgradePhase[A-Za-z]*[[:space:]][[:space:]]*UpgradePhase[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "$types")"
  if [[ -z "$values" ]]; then
    echo "no UpgradePhase constants in $types"
    return
  fi

  {
    [[ -f "$drawio" ]] || echo "missing figure: $FIGURE.drawio"
    [[ -f "$svg" ]] || echo "missing figure: $FIGURE.svg"
    for reason in "${REASONS[@]}"; do
      value="$(sed -n "s/^[[:space:]]*Reason${reason}[[:space:]]*=[[:space:]]*\"\([^\"]*\)\".*/\1/p" \
        "$database/upgrade.go" "$database/flow.go")"
      if [[ -z "$value" ]]; then
        echo "no constant: Reason$reason"
      else
        values+=$'\n'"$value"
      fi
    done
    while IFS= read -r value; do
      if [[ -f "$drawio" ]] && ! grep -E -q -- "value=\"([^\"]*[^A-Za-z])?$value([^A-Za-z][^\"]*)?\"" "$drawio"; then
        echo "not in drawio: $value"
      fi
      if [[ -f "$svg" ]] && ! grep -E -q -- ">([^<]*[^A-Za-z])?$value([^A-Za-z][^<]*)?</text>" "$svg"; then
        echo "not in svg: $value"
      fi
    done <<<"$values"
  } | LC_ALL=C sort
}

# ---------------------------------------------------------------------------
# Fixture
# ---------------------------------------------------------------------------

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

# build_fixture: write a consistent tree and print its root. The types stub
# defines the four phases, one of them padded the way gofmt aligns a shorter
# name and with a trailing comment, beside a constant of another type.
# database/upgrade.go defines the reasons but the two DBSync ones, which
# database/flow.go defines beside a reason the figure does not draw.
# diagrams/ holds a pair that gives each phase and each reason a label of its
# own, except the three rejection reasons, which share one label.
build_fixture() {
  local root reason value
  root="$(mktemp -d "$TMP_ROOT/fixture.XXXXXX")"
  mkdir -p "$root/database" "$root/diagrams"
  cat >"$root/types.go" <<'EOF'
package types

type UpgradePhase string

const (
	UpgradePhaseExpanding     UpgradePhase = "Expanding"
	UpgradePhaseMigrating     UpgradePhase = "Migrating" // expand is done
	UpgradePhaseRollingUpdate UpgradePhase = "RollingUpdate"
	UpgradePhaseContracting   UpgradePhase = "Contracting"
)

const UpgradePhaseLabel = "Label"
EOF
  {
    echo 'package database'
    echo 'const ('
    for reason in "${REASONS[@]}"; do
      [[ "$reason" == DBSync* ]] || printf '\tReason%s = "%s"\n' "$reason" "$reason"
    done
    echo ')'
  } >"$root/database/upgrade.go"
  cat >"$root/database/flow.go" <<'EOF'
package database

const (
	ReasonDBSyncFailed     = "DBSyncFailed"
	ReasonDBSyncInProgress = "DBSyncInProgress"
	ReasonDatabaseSynced   = "DatabaseSynced"
)
EOF
  {
    echo '<mxfile host="drawio" type="device"><diagram id="u" name="U"><mxGraphModel><root>'
    echo '<mxCell id="u-r" value="VersionParseError, DowngradeNotSupported or UpgradePathInvalid" vertex="1" parent="1"/>'
    for value in Expanding Migrating RollingUpdate Contracting "${REASONS[@]}"; do
      case "$value" in VersionParseError | DowngradeNotSupported | UpgradePathInvalid) continue ;; esac
      echo "<mxCell id=\"u-$value\" value=\"$value\" vertex=\"1\" parent=\"1\"/>"
    done
    echo '</root></mxGraphModel></diagram></mxfile>'
  } >"$root/diagrams/$FIGURE.drawio"
  {
    echo '<svg xmlns="http://www.w3.org/2000/svg">'
    echo '<text x="0" y="0">VersionParseError, DowngradeNotSupported or UpgradePathInvalid</text>'
    for value in Expanding Migrating RollingUpdate Contracting "${REASONS[@]}"; do
      case "$value" in VersionParseError | DowngradeNotSupported | UpgradePathInvalid) continue ;; esac
      echo "<text x=\"0\" y=\"0\">$value</text>"
    done
    echo '</svg>'
  } >"$root/diagrams/$FIGURE.svg"
  printf '%s\n' "$root"
}

# check_fixture <root>: run check_upgrade_figure on the fixture.
check_fixture() {
  check_upgrade_figure "$1/types.go" "$1/database" "$1/diagrams"
}

# rewrite <file> <sed-script>: apply a sed script in place (BSD and GNU sed).
rewrite() {
  sed "$2" "$1" >"$1.tmp" && mv "$1.tmp" "$1"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_consistent_fixture_reports_nothing() {
  echo "Test: a figure that names every phase and reason reports nothing, three reasons in one label"

  local root
  root="$(build_fixture)"
  assert_eq "no problems" "" "$(check_fixture "$root")"
}

test_missing_types_file_is_the_only_report() {
  echo "Test: a types path that is not a file is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.svg"
  assert_eq "absent path" "no types file: $root/absent.go" \
    "$(check_upgrade_figure "$root/absent.go" "$root/database" "$root/diagrams")"
}

test_types_without_phases_is_the_only_report() {
  echo "Test: a types file without a phase constant is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.drawio"
  rewrite "$root/types.go" '/UpgradePhase = /d'
  assert_eq "no phase constant" "no UpgradePhase constants in $root/types.go" \
    "$(check_fixture "$root")"
}

test_missing_figure_file_skips_only_its_searches() {
  echo "Test: a missing file of the pair skips only the searches in that file"

  local root
  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.drawio"
  rewrite "$root/diagrams/$FIGURE.svg" '/>Contracting</d'
  assert_eq "no drawio, the svg is still searched" \
    "missing figure: $FIGURE.drawio"$'\n'"not in svg: Contracting" \
    "$(check_fixture "$root")"
}

# Migrating is the padded phase with a trailing comment, and DBSyncFailed is
# read from flow.go, so this also proves both are read.
test_missing_label_reports_exactly_its_line() {
  echo "Test: a phase or a reason missing from one file reports exactly its line"

  local root
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.drawio" '/value="Migrating"/d'
  assert_eq "a phase missing from the drawio" "not in drawio: Migrating" "$(check_fixture "$root")"

  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.svg" '/>DBSyncFailed</d'
  assert_eq "a reason missing from the svg" "not in svg: DBSyncFailed" "$(check_fixture "$root")"

  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.drawio" 's/DowngradeNotSupported or //'
  assert_eq "a reason missing from a shared label" "not in drawio: DowngradeNotSupported" \
    "$(check_fixture "$root")"
}

test_code_change_without_label_is_reported() {
  echo "Test: a phase added, a reason renamed or a reason constant gone is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/types.go" 's/^)$/	UpgradePhaseVerifying UpgradePhase = "Verifying"\
)/'
  assert_eq "a new phase" "not in drawio: Verifying"$'\n'"not in svg: Verifying" \
    "$(check_fixture "$root")"

  root="$(build_fixture)"
  rewrite "$root/database/upgrade.go" 's/"MigrateFailed"/"MigrationFailed"/'
  assert_eq "a reason with a new value" "not in drawio: MigrationFailed"$'\n'"not in svg: MigrationFailed" \
    "$(check_fixture "$root")"

  root="$(build_fixture)"
  rewrite "$root/database/flow.go" '/ReasonDBSyncInProgress/d'
  assert_eq "a reason constant gone" "no constant: ReasonDBSyncInProgress" "$(check_fixture "$root")"
}

test_label_must_hold_the_value_as_a_word() {
  echo "Test: a value that only stands inside a longer word is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.drawio" '/value="RollingUpdate"/d'
  rewrite "$root/diagrams/$FIGURE.svg" '/>RollingUpdate</d'
  assert_eq "RollingUpdate only inside UpgradeRollingUpdate" \
    "not in drawio: RollingUpdate"$'\n'"not in svg: RollingUpdate" \
    "$(check_fixture "$root")"
}

test_repository_figure_names_every_phase_and_reason() {
  echo "Test: the repository's upgrade phases figure names every UpgradePhase and every reason it draws"

  local types="$PROJECT_ROOT/internal/common/types/types.go"
  local database="$PROJECT_ROOT/internal/common/database"
  local copy
  assert_eq "no problems in docs/diagrams" "" \
    "$(check_upgrade_figure "$types" "$database" "$PROJECT_ROOT/docs/diagrams")"

  # The same check on a copy without the ContractFailed label proves it reads
  # the real figure.
  copy="$(mktemp -d "$TMP_ROOT/copy.XXXXXX")"
  cp "$PROJECT_ROOT/docs/diagrams/$FIGURE.drawio" "$PROJECT_ROOT/docs/diagrams/$FIGURE.svg" "$copy/"
  rewrite "$copy/$FIGURE.svg" 's#>ContractFailed<#>ContractBroke<#'
  assert_eq "a copy without ContractFailed" "not in svg: ContractFailed" \
    "$(check_upgrade_figure "$types" "$database" "$copy")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_consistent_fixture_reports_nothing
test_missing_types_file_is_the_only_report
test_types_without_phases_is_the_only_report
test_missing_figure_file_skips_only_its_searches
test_missing_label_reports_exactly_its_line
test_code_change_without_label_is_reported
test_label_must_hold_the_value_as_a_word
test_repository_figure_names_every_phase_and_reason

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
