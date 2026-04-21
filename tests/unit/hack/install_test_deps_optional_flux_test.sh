#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/install-test-deps.sh gates install_flux behind WITH_FLUX_CLI
# after the FluxInstance bootstrap migration (CC-0085, REQ-004):
#   - Default run (WITH_FLUX_CLI unset) skips install_flux entirely.
#   - WITH_FLUX_CLI=true invokes install_flux and reaches verify_sha256.
#
# DECISION: bats vs project-native bash test runner
# Ambiguity: task spec asked for `*.bats`, but the repo has zero .bats files,
#   no bats binary in CI, and the Makefile wires `test-shell` to
#   `tests/unit/hack/*_test.sh`. Sibling tests
#   (deploy_infra_preflight_test.sh, deploy_infra_reconcile_sources_test.sh)
#   use project-native bash + tests/lib/assertions.sh.
# Chose: project-native bash test (tests/lib/assertions.sh).
# Reason: matches the established pattern, auto-registers with the existing
#   `make test-shell` glob, and avoids introducing an undeclared CI dependency.
# Reviewer: please verify this matches intent.
#
# Usage: bash tests/unit/hack/install_test_deps_optional_flux_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
INSTALL_SCRIPT="$PROJECT_ROOT/hack/install-test-deps.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Platform detection — mirrors install-test-deps.sh detect_platform so the
# test can look up the matching pinned hash for the current machine.
# ---------------------------------------------------------------------------
detect_test_platform() {
  local uname_os uname_arch
  uname_os="$(uname -s)"
  uname_arch="$(uname -m)"
  case "$uname_os" in
    Linux)  TEST_OS="linux" ;;
    Darwin) TEST_OS="darwin" ;;
    *)      TEST_OS="unsupported" ;;
  esac
  case "$uname_arch" in
    x86_64)          TEST_ARCH="amd64" ;;
    aarch64 | arm64) TEST_ARCH="arm64" ;;
    *)               TEST_ARCH="unsupported" ;;
  esac
}

# extract_pinned_hash <tool> — returns the FOO_SHA256_<os>_<arch> constant
# from install-test-deps.sh for the current platform.
extract_pinned_hash() {
  local tool="$1"
  local var="${tool}_SHA256_${TEST_OS}_${TEST_ARCH}"
  awk -F'"' -v v="$var" '$0 ~ "^" v "=" {print $2; exit}' "$INSTALL_SCRIPT"
}

# ---------------------------------------------------------------------------
# Stub factory — populates <dir> with stubs for curl / tar / sha256sum that
# let install_flux reach `install` without touching the network.
#
# Env contract:
#   STUB_FLUX_HASH  — hash the sha256sum stub echoes (must match the pinned
#                     FLUX_SHA256 value for the current OS/ARCH).
# ---------------------------------------------------------------------------
install_stubs() {
  local dir="$1"
  mkdir -p "$dir"

  # curl -fsSL <url> -o <path> → create an empty file at <path>.
  cat >"$dir/curl" <<'STUB'
#!/bin/bash
target=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -o) target="$2"; shift 2 ;;
    *)  shift ;;
  esac
done
if [[ -n "$target" ]]; then
  : >"$target"
fi
exit 0
STUB
  chmod +x "$dir/curl"

  # tar -xzf <archive> -C <dir> <name> → touch <dir>/<name>, executable.
  cat >"$dir/tar" <<'STUB'
#!/bin/bash
c_dir=""
positional=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    -C)    c_dir="$2"; shift 2 ;;
    -xzf)  shift 2 ;;
    -*)    shift ;;
    *)     positional+=("$1"); shift ;;
  esac
done
for name in "${positional[@]}"; do
  : >"$c_dir/$name"
  chmod +x "$c_dir/$name"
done
exit 0
STUB
  chmod +x "$dir/tar"

  # sha256sum <file> → prints "<STUB_FLUX_HASH>  <file>" so verify_sha256
  # compares equal to the pinned hash fed into it.
  cat >"$dir/sha256sum" <<STUB
#!/bin/bash
printf '%s  %s\n' "${STUB_FLUX_HASH}" "\$1"
exit 0
STUB
  chmod +x "$dir/sha256sum"
}

# Populate <dir> with "already installed" binaries for chainsaw/kind/kubectl
# so their install_* functions short-circuit and never hit our stubs with the
# wrong pinned hash. Flux is deliberately NOT populated: the tests that want
# to exercise the full install_flux code path (download + verify_sha256 +
# install) leave $dir/flux absent.
prepopulate_non_flux_tools() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/chainsaw" <<'STUB'
#!/bin/bash
echo "v0.2.14"
STUB
  cat >"$dir/kind" <<'STUB'
#!/bin/bash
echo "kind v0.27.0 go1.23.0 linux/amd64"
STUB
  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
echo "Client Version: v1.33.1"
STUB
  chmod +x "$dir/chainsaw" "$dir/kind" "$dir/kubectl"
}

# Populate <dir>/flux with a stub whose `version --client` output matches the
# pinned FLUX_VERSION, so install_flux's "already installed" short-circuit
# branch fires (CC-0085, REQ-004). Used by test 3 to cover the branch that
# tests 1 and 2 deliberately skip.
prepopulate_flux_with_correct_version() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/flux" <<'STUB'
#!/bin/bash
# Mirror `flux version --client` output shape closely enough for the
# grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1 extractor in install_flux.
echo "flux: v2.5.1"
STUB
  chmod +x "$dir/flux"
}

# Run install-test-deps.sh in a fresh subshell with a controlled INSTALL_DIR,
# stubbed network commands on PATH, and the given WITH_FLUX_CLI value.
#
# Usage: run_install_script <install_dir> <stub_dir> [with_flux_cli]
run_install_script() {
  local install_dir="$1"
  local stub_dir="$2"
  local with_flux="${3-}"

  if [[ $# -ge 3 ]]; then
    PATH="$stub_dir:$PATH" \
      INSTALL_DIR="$install_dir" \
      WITH_FLUX_CLI="$with_flux" \
      bash "$INSTALL_SCRIPT" 2>&1
  else
    PATH="$stub_dir:$PATH" \
      INSTALL_DIR="$install_dir" \
      bash "$INSTALL_SCRIPT" 2>&1
  fi
}

# ---------------------------------------------------------------------------
# Test 1: default run — WITH_FLUX_CLI unset → install_flux is not invoked.
# (CC-0085, REQ-004)
# ---------------------------------------------------------------------------
test_default_skips_install_flux() {
  echo "Test: default run skips install_flux (CC-0085, REQ-004)"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  STUB_FLUX_HASH="$(extract_pinned_hash FLUX)"
  export STUB_FLUX_HASH

  install_stubs "$tmp/stubs"
  prepopulate_non_flux_tools "$tmp/install"

  local output exit_code
  output="$(run_install_script "$tmp/install" "$tmp/stubs")"
  exit_code=$?

  assert_eq "install-test-deps.sh exits 0 with WITH_FLUX_CLI unset" "0" "$exit_code"
  # install_flux must not run — neither the "Installing" branch nor the
  # "already installed" short-circuit should appear.
  assert_not_contains "no 'Installing flux' log line" "$output" "Installing flux"
  assert_not_contains "no 'flux ... already installed' log line" "$output" "flux 2.5.1 already installed"
  # Other installers still run (they short-circuit on the pre-populated stubs).
  assert_contains "install_chainsaw still runs" "$output" "chainsaw v0.2.14 already installed"
  assert_contains "install_kind still runs" "$output" "kind v0.27.0 already installed"
  assert_contains "install_kubectl still runs" "$output" "kubectl v1.33.1 already installed"
  # Script did not abort before "=== Done ===".
  assert_contains "script reached Done" "$output" "=== Done ==="
}

# ---------------------------------------------------------------------------
# Test 2: WITH_FLUX_CLI=true — install_flux runs end-to-end and verify_sha256
# is invoked (observed via its success log line). (CC-0085, REQ-004)
# ---------------------------------------------------------------------------
test_with_flux_cli_true_invokes_install_flux() {
  echo "Test: WITH_FLUX_CLI=true invokes install_flux and verify_sha256 (CC-0085, REQ-004)"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  STUB_FLUX_HASH="$(extract_pinned_hash FLUX)"
  export STUB_FLUX_HASH

  install_stubs "$tmp/stubs"
  prepopulate_non_flux_tools "$tmp/install"

  local output exit_code
  output="$(run_install_script "$tmp/install" "$tmp/stubs" "true")"
  exit_code=$?

  assert_eq "install-test-deps.sh exits 0 with WITH_FLUX_CLI=true" "0" "$exit_code"
  # install_flux ran: network download log line present.
  assert_contains "install_flux was invoked" "$output" "Installing flux 2.5.1"
  # verify_sha256 ran and succeeded (signature: "SHA256 checksum verified.").
  assert_contains "verify_sha256 was invoked" "$output" "SHA256 checksum verified."
  # Final install log line from install_flux.
  assert_contains "install_flux completed" "$output" "flux 2.5.1 installed to"
  # Binary actually landed in INSTALL_DIR.
  if [[ -x "$tmp/install/flux" ]]; then
    echo "  PASS: flux binary installed under INSTALL_DIR"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: flux binary not installed under INSTALL_DIR"
    FAIL=$((FAIL + 1))
  fi
}

# ---------------------------------------------------------------------------
# Test 3: WITH_FLUX_CLI=true with a pre-populated flux binary at the correct
# version — install_flux short-circuits (no download, no verify_sha256).
# This complements test 2, which forces the full install path by leaving
# $tmp/install/flux absent. Together they cover both branches of the
# version-check guard in install_flux (CC-0085, REQ-004).
# ---------------------------------------------------------------------------
test_with_flux_cli_true_short_circuits_on_correct_version() {
  echo "Test: WITH_FLUX_CLI=true short-circuits when flux is already installed (CC-0085, REQ-004)"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  STUB_FLUX_HASH="$(extract_pinned_hash FLUX)"
  export STUB_FLUX_HASH

  install_stubs "$tmp/stubs"
  prepopulate_non_flux_tools "$tmp/install"
  prepopulate_flux_with_correct_version "$tmp/install"

  local output exit_code
  output="$(run_install_script "$tmp/install" "$tmp/stubs" "true")"
  exit_code=$?

  assert_eq "install-test-deps.sh exits 0 with flux pre-installed" "0" "$exit_code"
  # Short-circuit log line from install_flux is emitted.
  assert_contains "short-circuit log line appears" "$output" "flux 2.5.1 already installed"
  # The download + verify branches must NOT have run.
  assert_not_contains "no 'Installing flux' log line" "$output" "Installing flux 2.5.1"
  assert_not_contains "no 'SHA256 checksum verified.' log line" "$output" "SHA256 checksum verified."
  # Script still reaches Done.
  assert_contains "script reached Done" "$output" "=== Done ==="
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
detect_test_platform
if [[ "$TEST_OS" == "unsupported" || "$TEST_ARCH" == "unsupported" ]]; then
  echo "SKIP: unsupported test platform ($TEST_OS/$TEST_ARCH)"
  exit 0
fi

test_default_skips_install_flux
test_with_flux_cli_true_invokes_install_flux
test_with_flux_cli_true_short_circuits_on_correct_version

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
