#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/gen-option-catalog.sh unions the namespace lines of every
# generator config a service maps to. neutron splits its options across
# per-process generator files, so its catalog is built from three of them and
# the oslo.log namespace all three list must reach the generator once. A
# service that maps to a single path yields a generator config holding the
# [DEFAULT] header and that file's namespace lines only. The deduplicated
# union is also what the DROP_NAMESPACES filter consumes, so barbican's kmip
# entry is checked here too.
#
# The script is exercised with a recording docker stub prepended to PATH: at
# the moment of the first `docker run` the stub copies gen.conf out of the
# bind-mount source, then exits non-zero so the script stops there. No image is
# pulled and no container runs.
#
# Usage: bash tests/unit/hack/gen_option_catalog_multi_config_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
GEN_CATALOG_SH="$PROJECT_ROOT/hack/gen-option-catalog.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# fail_mktemp [dir]...
# Records a failed `mktemp -d` as a test failure and removes the scratch <dir>s
# the caller created before it. The file runs without -e, so an unchecked
# failure would continue with an empty directory name: the test bodies build
# absolute paths from it (writing the fixtures into /) and prepend it to PATH,
# whose leading empty element means the current directory. mktemp -d fails on
# an exhausted TMPDIR, so leaving the earlier directories behind makes the next
# run more likely to hit the same failure.
fail_mktemp() {
  echo "  FAIL: mktemp -d could not create a scratch directory"
  FAIL=$((FAIL + 1))
  if [ "$#" -gt 0 ]; then
    rm -rf "$@"
  fi
}

# require_gen_conf <gen_conf> <dir>...
# Fails the test when the docker stub never recorded a generator config and
# removes the scratch <dir>s. assert_file_not_contains reports PASS on a
# missing file, so an unguarded read prints green assertions about output that
# was never produced and points a debugger at the wrong pipeline stage.
require_gen_conf() {
  local gen_conf="$1"
  shift
  [ -f "$gen_conf" ] && return 0
  echo "  FAIL: the docker stub never recorded a generator config"
  FAIL=$((FAIL + 1))
  rm -rf "$@"
  return 1
}

# make_stubs <dir>
# Writes yq and docker shims into <dir>:
#   yq     — resolves any source-ref lookup to a fixed ref.
#   docker — `docker image inspect` succeeds so nothing is pulled;
#            `docker run` copies the gen.conf from the -v mount source to
#            $GEN_CONF_LOG, then exits 1 so the script under test stops after
#            the recording.
make_stubs() {
  local dir="$1"

  cat > "$dir/yq" <<'EOF'
#!/bin/bash
echo "27.0.3"
EOF

  cat > "$dir/docker" <<'EOF'
#!/bin/bash
if [ "$1" = "image" ]; then
  exit 0
fi
if [ "$1" = "run" ]; then
  src=""
  prev=""
  for arg in "$@"; do
    if [ "$prev" = "-v" ]; then
      src="${arg%%:*}"
    fi
    prev="$arg"
  done
  cp "$src/gen.conf" "$GEN_CONF_LOG"
  exit 1
fi
exit 0
EOF

  chmod +x "$dir/yq" "$dir/docker"
}

# make_neutron_source <dir>
# Writes the three per-process generator configs the neutron catalog unions
# into a SOURCE_DIR fixture, each listing the shared oslo.log namespace.
make_neutron_source() {
  local dir="$1"
  mkdir -p "$dir/etc/oslo-config-generator"

  cat > "$dir/etc/oslo-config-generator/neutron.conf" <<'EOF'
[DEFAULT]
output_file = etc/neutron.conf.sample
wrap_width = 79
namespace = neutron
namespace = oslo.log
EOF

  cat > "$dir/etc/oslo-config-generator/ml2_conf.ini" <<'EOF'
[DEFAULT]
namespace = neutron.ml2
namespace = oslo.log
EOF

  cat > "$dir/etc/oslo-config-generator/neutron_ovn_metadata_agent.ini" <<'EOF'
[DEFAULT]
namespace = neutron.ovn.metadata.agent
namespace = oslo.log
EOF
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_union_of_three_generator_files() {
  echo "Test: neutron unions the namespaces of its three generator configs"
  local stub_dir src_dir log_dir gen_conf
  stub_dir="$(mktemp -d)" || { fail_mktemp; return 1; }
  src_dir="$(mktemp -d)" || { fail_mktemp "$stub_dir"; return 1; }
  log_dir="$(mktemp -d)" || { fail_mktemp "$stub_dir" "$src_dir"; return 1; }
  gen_conf="$log_dir/gen.conf"
  make_stubs "$stub_dir"
  make_neutron_source "$src_dir"

  PATH="$stub_dir:$PATH" GEN_CONF_LOG="$gen_conf" SOURCE_DIR="$src_dir" \
    bash "$GEN_CATALOG_SH" neutron 2025.2 ghcr.io/c5c3/neutron:test >/dev/null 2>&1

  require_gen_conf "$gen_conf" "$stub_dir" "$src_dir" "$log_dir" || return 1
  assert_eq "the generator config opens with the [DEFAULT] header" \
    "[DEFAULT]" "$(head -n 1 "$gen_conf")"
  assert_eq "the namespaces are the first-seen union of the three files" \
    "namespace = neutron,namespace = oslo.log,namespace = neutron.ml2,namespace = neutron.ovn.metadata.agent" \
    "$(grep '^namespace' "$gen_conf" | paste -sd, -)"
  assert_eq "the shared oslo.log namespace is kept once" \
    "1" "$(grep -c 'oslo.log' "$gen_conf")"
  assert_file_not_contains "output_file is dropped" "$gen_conf" "output_file"
  assert_file_not_contains "wrap_width is dropped" "$gen_conf" "wrap_width"

  rm -rf "$stub_dir" "$src_dir" "$log_dir"
}

test_missing_generator_file_fails_before_docker() {
  echo "Test: a generator config missing from SOURCE_DIR fails before docker"
  local stub_dir src_dir log_dir gen_conf stderr_file stderr exit_code docker_ran
  stub_dir="$(mktemp -d)" || { fail_mktemp; return 1; }
  src_dir="$(mktemp -d)" || { fail_mktemp "$stub_dir"; return 1; }
  log_dir="$(mktemp -d)" || { fail_mktemp "$stub_dir" "$src_dir"; return 1; }
  gen_conf="$log_dir/gen.conf"
  stderr_file="$log_dir/stderr.txt"
  make_stubs "$stub_dir"
  make_neutron_source "$src_dir"
  rm "$src_dir/etc/oslo-config-generator/ml2_conf.ini"

  PATH="$stub_dir:$PATH" GEN_CONF_LOG="$gen_conf" SOURCE_DIR="$src_dir" \
    bash "$GEN_CATALOG_SH" neutron 2025.2 ghcr.io/c5c3/neutron:test \
    >/dev/null 2>"$stderr_file"
  exit_code=$?
  stderr="$(cat "$stderr_file")"
  docker_ran="no"
  [ -e "$gen_conf" ] && docker_ran="yes"

  assert_nonzero_exit "the incomplete source tree is rejected" "$exit_code"
  assert_eq "the script exits 1" "1" "$exit_code"
  assert_contains "stderr reports the missing generator config" \
    "$stderr" "generator config not found:"
  assert_contains "stderr names the path that is missing" \
    "$stderr" "$src_dir/etc/oslo-config-generator/ml2_conf.ini"
  assert_eq "docker is never invoked" "no" "$docker_ran"

  rm -rf "$stub_dir" "$src_dir" "$log_dir"
}

test_single_path_service_unchanged() {
  echo "Test: a service mapped to one generator config keeps its own namespaces"
  local stub_dir src_dir log_dir gen_conf expected
  stub_dir="$(mktemp -d)" || { fail_mktemp; return 1; }
  src_dir="$(mktemp -d)" || { fail_mktemp "$stub_dir"; return 1; }
  log_dir="$(mktemp -d)" || { fail_mktemp "$stub_dir" "$src_dir"; return 1; }
  gen_conf="$log_dir/gen.conf"
  make_stubs "$stub_dir"
  mkdir -p "$src_dir/config-generator"
  cat > "$src_dir/config-generator/keystone.conf" <<'EOF'
[DEFAULT]
namespace = keystone
EOF

  PATH="$stub_dir:$PATH" GEN_CONF_LOG="$gen_conf" SOURCE_DIR="$src_dir" \
    bash "$GEN_CATALOG_SH" keystone 2025.2 ghcr.io/c5c3/keystone:test >/dev/null 2>&1

  require_gen_conf "$gen_conf" "$stub_dir" "$src_dir" "$log_dir" || return 1
  expected="$(printf '[DEFAULT]\nnamespace = keystone\n')"
  assert_eq "the generator config holds the header and the one namespace" \
    "$expected" "$(cat "$gen_conf")"

  rm -rf "$stub_dir" "$src_dir" "$log_dir"
}

test_drop_namespaces_filtered() {
  echo "Test: a DROP_NAMESPACES entry is removed from the generator config"
  # barbican is the only service with a DROP_NAMESPACES entry: it registers
  # its kmip secret store unconditionally while the image ships no pykmip, and
  # a namespace the image cannot import aborts the whole generator run. The
  # filter reads the deduplicated namespace union, so a later edit to that
  # pipeline can silently stop dropping the entry.
  local stub_dir src_dir log_dir gen_conf
  stub_dir="$(mktemp -d)" || { fail_mktemp; return 1; }
  src_dir="$(mktemp -d)" || { fail_mktemp "$stub_dir"; return 1; }
  log_dir="$(mktemp -d)" || { fail_mktemp "$stub_dir" "$src_dir"; return 1; }
  gen_conf="$log_dir/gen.conf"
  make_stubs "$stub_dir"
  mkdir -p "$src_dir/etc/oslo-config-generator"
  cat > "$src_dir/etc/oslo-config-generator/barbican.conf" <<'EOF'
[DEFAULT]
namespace = barbican.common.config
namespace = barbican.plugin.secret_store.kmip
namespace = oslo.log
EOF

  PATH="$stub_dir:$PATH" GEN_CONF_LOG="$gen_conf" SOURCE_DIR="$src_dir" \
    bash "$GEN_CATALOG_SH" barbican 2025.2 ghcr.io/c5c3/barbican:test >/dev/null 2>&1

  require_gen_conf "$gen_conf" "$stub_dir" "$src_dir" "$log_dir" || return 1
  assert_file_not_contains "the kmip namespace is dropped" "$gen_conf" "kmip"
  assert_eq "the surviving namespaces keep their order" \
    "namespace = barbican.common.config,namespace = oslo.log" \
    "$(grep '^namespace' "$gen_conf" | paste -sd, -)"

  rm -rf "$stub_dir" "$src_dir" "$log_dir"
}

test_union_of_three_generator_files
test_missing_generator_file_fails_before_docker
test_single_path_service_unchanged
test_drop_namespaces_filtered

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
