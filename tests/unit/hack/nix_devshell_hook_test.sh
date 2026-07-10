#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/nix-devshell-hook.sh resolves the CI toolchain pins from their
# canonical locations, so the Nix devshell installs the exact versions CI uses.
#
# The hook reads each pin with awk `match()`; this test re-extracts the same
# value a different way (grep/sed) from the same canonical file and asserts they
# agree. That bidirectional guard fails loudly if either the pin location moves
# or the hook's resolver drifts — the coupling the hook's header comment warns
# about.
#
# Usage: bash tests/unit/hack/nix_devshell_hook_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

HOOK="$PROJECT_ROOT/hack/nix-devshell-hook.sh"
CI="$PROJECT_ROOT/.github/workflows/ci.yaml"
MK="$PROJECT_ROOT/Makefile"
FLAKE="$PROJECT_ROOT/flake.nix"

# The seven keys the hook must emit, in order.
EXPECTED_KEYS="CONTROLLER_GEN_VERSION GOFUMPT_VERSION GOLANGCI_LINT_VERSION SETUP_ENVTEST_REF ENVTEST_K8S_VERSION KUSTOMIZE_VERSION HELM_UNITTEST_VERSION"

# Capture --print-pins once.
PINS_OUT="$(bash "$HOOK" --print-pins)"
PINS_RC=$?

# pin_value KEY — extract the value the hook printed for KEY.
pin_value() {
  printf '%s\n' "$PINS_OUT" | awk -F= -v k="$1" '$1 == k { print $2; exit }'
}

# Source the hook to exercise its pure helpers directly. The guard suppresses the
# side-effecting provisioning path so no network installs run during the test.
export NIX_DEVSHELL_HOOK_NO_PROVISION=1
# shellcheck source=hack/nix-devshell-hook.sh
source "$HOOK"

test_print_pins_exit_zero() {
  echo "Test: --print-pins exits 0"
  assert_eq "--print-pins exit code" "0" "$PINS_RC"
}

test_print_pins_emits_exactly_seven_keys() {
  echo "Test: --print-pins emits exactly the seven expected keys"

  local count
  count="$(printf '%s\n' "$PINS_OUT" | grep -Ec '^[A-Z0-9_]+=')"
  assert_eq "seven KEY=VALUE lines" "7" "$count"

  local key
  for key in $EXPECTED_KEYS; do
    if printf '%s\n' "$PINS_OUT" | grep -q "^${key}="; then
      echo "  PASS: key $key present"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: key $key missing from --print-pins output"
      FAIL=$((FAIL + 1))
    fi
  done
}

test_pins_non_empty_and_well_formed() {
  echo "Test: each pin is non-empty and matches its expected format"

  # Accept an optional pre-release / build / fourth component (e.g. v2.13.0-rc1
  # or v1.2.3.4) so a legitimate Renovate bump the hook resolves fine does not
  # false-fail here; the non-empty and canonical-file-match guards prove the pin.
  local semver='^v[0-9]+\.[0-9]+\.[0-9]+([.-].+)?$'
  local minor='^[0-9]+\.[0-9]+$'
  local branchref='^release-[0-9]+\.[0-9]+$'

  local key fmt val
  for key in $EXPECTED_KEYS; do
    case "$key" in
      SETUP_ENVTEST_REF) fmt="$branchref" ;;
      ENVTEST_K8S_VERSION) fmt="$minor" ;;
      *) fmt="$semver" ;;
    esac
    val="$(pin_value "$key")"
    assert_not_empty "$key is non-empty" "$val"
    if printf '%s' "$val" | grep -Eq "$fmt"; then
      echo "  PASS: $key '$val' matches $fmt"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $key '$val' does not match $fmt"
      FAIL=$((FAIL + 1))
    fi
  done
}

test_pins_match_canonical_files() {
  echo "Test: each pin agrees with an independent extraction from its canonical file"

  # Independent (grep/sed) extractions — deliberately not the hook's awk match().
  local ci_controller ci_golangci ci_setup_ref ci_kustomize ci_helm mk_envtest
  ci_controller="$(grep -Em1 '^[[:space:]]+CONTROLLER_GEN_VERSION:' "$CI" | sed -E 's/.*:[[:space:]]*//')"
  ci_golangci="$(grep -Em1 '^[[:space:]]+GOLANGCI_LINT_VERSION:' "$CI" | sed -E 's/.*:[[:space:]]*//')"
  ci_setup_ref="$(grep -Em1 'setup-envtest@' "$CI" | sed -E 's/.*setup-envtest@([^[:space:]]+).*/\1/')"
  ci_kustomize="$(grep -Em1 'KUSTOMIZE_VERSION=v' "$CI" | sed -E 's/.*KUSTOMIZE_VERSION=(v[0-9.]+).*/\1/')"
  ci_helm="$(grep -Em1 'helm-unittest.*--version' "$CI" | sed -E 's/.*--version[[:space:]]+(v[0-9.]+).*/\1/')"
  mk_envtest="$(grep -Em1 'ENVTEST_K8S_VERSION' "$MK" | sed -E 's/.*\?=[[:space:]]*//')"

  assert_eq "CONTROLLER_GEN_VERSION matches ci.yaml" "$ci_controller" "$(pin_value CONTROLLER_GEN_VERSION)"
  assert_eq "GOLANGCI_LINT_VERSION matches ci.yaml" "$ci_golangci" "$(pin_value GOLANGCI_LINT_VERSION)"
  assert_eq "SETUP_ENVTEST_REF matches ci.yaml" "$ci_setup_ref" "$(pin_value SETUP_ENVTEST_REF)"
  assert_eq "KUSTOMIZE_VERSION matches ci.yaml" "$ci_kustomize" "$(pin_value KUSTOMIZE_VERSION)"
  assert_eq "HELM_UNITTEST_VERSION matches ci.yaml" "$ci_helm" "$(pin_value HELM_UNITTEST_VERSION)"
  assert_eq "ENVTEST_K8S_VERSION matches Makefile" "$mk_envtest" "$(pin_value ENVTEST_K8S_VERSION)"
}

test_gofumpt_pin_consistent_across_ci_and_makefile() {
  echo "Test: GOFUMPT_VERSION agrees across ci.yaml, the Makefile, and the hook"

  local ci_gofumpt mk_gofumpt hook_gofumpt
  ci_gofumpt="$(grep -Em1 '^[[:space:]]+GOFUMPT_VERSION:' "$CI" | sed -E 's/.*:[[:space:]]*//')"
  mk_gofumpt="$(grep -Em1 '^GOFUMPT_VERSION' "$MK" | sed -E 's/.*(v[0-9]+\.[0-9]+\.[0-9]+).*/\1/')"
  hook_gofumpt="$(pin_value GOFUMPT_VERSION)"

  assert_eq "hook GOFUMPT_VERSION matches ci.yaml env block" "$ci_gofumpt" "$hook_gofumpt"
  assert_eq "ci.yaml and Makefile GOFUMPT_VERSION are in sync" "$ci_gofumpt" "$mk_gofumpt"
}

test_detect_platform() {
  echo "Test: _detect_platform maps supported uname pairs and rejects others"

  local out rc
  # Supported pairs — stub uname inside a subshell so the real binary is untouched.
  out="$( uname() { case "$1" in -s) echo Linux ;; -m) echo x86_64 ;; esac; }; _detect_platform )"
  assert_eq "linux/x86_64 -> 'linux amd64'" "linux amd64" "$out"

  out="$( uname() { case "$1" in -s) echo Darwin ;; -m) echo arm64 ;; esac; }; _detect_platform )"
  assert_eq "darwin/arm64 -> 'darwin arm64'" "darwin arm64" "$out"

  # aarch64 must fold to arm64 — the mapping the kustomize download URL relies on
  # for Apple Silicon and Linux ARM, the platforms the flake advertises.
  out="$( uname() { case "$1" in -s) echo Linux ;; -m) echo aarch64 ;; esac; }; _detect_platform )"
  assert_eq "linux/aarch64 -> 'linux arm64'" "linux arm64" "$out"

  # Unsupported OS / arch must fail loudly (return 1) rather than emit a bad pair.
  ( uname() { case "$1" in -s) echo Windows_NT ;; -m) echo x86_64 ;; esac; }; _detect_platform >/dev/null 2>&1 )
  rc=$?
  assert_nonzero_exit "unsupported OS exits non-zero" "$rc"

  ( uname() { case "$1" in -s) echo Linux ;; -m) echo riscv64 ;; esac; }; _detect_platform >/dev/null 2>&1 )
  rc=$?
  assert_nonzero_exit "unsupported arch exits non-zero" "$rc"
}

test_tool_has_anchors_version_token() {
  echo "Test: _tool_has matches whole version tokens, not prefixes (regression)"

  local dir tool rc
  dir="$(mktemp -d)"
  tool="$dir/faketool"

  # Installed reports 2.12.20; a pin of 2.12.2 is a PREFIX and must NOT match, or
  # the reinstall of the real pin would be wrongly skipped (the grep -qF bug).
  printf '#!/usr/bin/env bash\necho "faketool has version 2.12.20 built from x"\n' >"$tool"
  chmod +x "$tool"

  _tool_has "$tool" version "2.12.2"
  rc=$?
  assert_eq "prefix pin 2.12.2 does not match installed 2.12.20" "1" "$rc"

  _tool_has "$tool" version "2.12.20"
  rc=$?
  assert_eq "exact pin 2.12.20 matches installed 2.12.20" "0" "$rc"

  # v-prefixed format (controller-gen / kustomize style) is anchored the same way.
  printf '#!/usr/bin/env bash\necho "Version: v0.17.30"\n' >"$tool"
  _tool_has "$tool" --version "v0.17.3"
  rc=$?
  assert_eq "prefix pin v0.17.3 does not match installed v0.17.30" "1" "$rc"

  _tool_has "$dir/absent" version "1.0.0"
  rc=$?
  assert_eq "missing binary reported as not-installed" "1" "$rc"

  rm -rf "$dir"
}

test_helm_plugin_pinned_anchors_version_token() {
  echo "Test: _helm_plugin_pinned matches whole version tokens, not prefixes (regression)"

  # `helm plugin list` already reports unittest 1.0.30; a pin of 1.0.3 is a
  # PREFIX and must NOT match, or the install of the real pin would be wrongly
  # skipped — the same anchoring guard _tool_has applies (kept in sync here).
  local list rc
  list=$'NAME\tVERSION\tDESCRIPTION\nunittest\t1.0.30\tunit test'

  _helm_plugin_pinned "$list" unittest "1.0.3"
  rc=$?
  assert_eq "prefix pin 1.0.3 does not match installed 1.0.30" "1" "$rc"

  _helm_plugin_pinned "$list" unittest "1.0.30"
  rc=$?
  assert_eq "exact pin 1.0.30 matches installed 1.0.30" "0" "$rc"

  # A v-prefixed pin (as resolved from ci.yaml) is stripped before anchoring.
  _helm_plugin_pinned "$list" unittest "v1.0.30"
  rc=$?
  assert_eq "v-prefixed pin v1.0.30 matches installed 1.0.30" "0" "$rc"

  # Only the header row present (plugin not installed) reports not-installed.
  _helm_plugin_pinned $'NAME\tVERSION\tDESCRIPTION' unittest "1.0.30"
  rc=$?
  assert_eq "absent plugin reported as not-installed" "1" "$rc"
}

test_flake_sources_hook() {
  echo "Test: flake.nix shellHook sources the devshell hook"

  if [[ ! -f "$FLAKE" ]]; then
    echo "  FAIL: $FLAKE does not exist"
    FAIL=$((FAIL + 1))
    return
  fi
  assert_file_contains "flake.nix references the hook script" \
    "$FLAKE" "hack/nix-devshell-hook.sh"
  assert_file_contains "flake.nix sources the hook" \
    "$FLAKE" "source \"\$_forge_root/hack/nix-devshell-hook.sh\""
}

test_print_pins_exit_zero
test_print_pins_emits_exactly_seven_keys
test_pins_non_empty_and_well_formed
test_pins_match_canonical_files
test_gofumpt_pin_consistent_across_ci_and_makefile
test_detect_platform
test_tool_has_anchors_version_token
test_helm_plugin_pinned_anchors_version_token
test_flake_sources_hook

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
