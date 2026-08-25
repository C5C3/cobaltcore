#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the multi-node kind config (hack/kind-config-multinode.yaml) and the
# KIND_CONFIG override in hack/deploy-infra.sh.
#
# Covers the shape of the checked-in file (one control-plane node with the two
# host-port bridges, two workers without any), the two render_kind_config paths
# that consume it (verbatim copy at the default host port, yq rewrite under a
# KIND_HOST_PORT override), the error path for an unreadable KIND_CONFIG, and
# the default the variable falls back to when it is unset or empty.
#
# render_kind_config is exercised by sourcing deploy-infra.sh in a subshell, so
# no cluster is created. The yq-dependent checks are skipped when `yq` is not
# installed.
#
# Usage: bash tests/unit/hack/kind_config_multinode_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"
MULTINODE_CONFIG="$PROJECT_ROOT/hack/kind-config-multinode.yaml"
DEFAULT_CONFIG="$PROJECT_ROOT/hack/kind-config.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# Source deploy-infra.sh and call render_kind_config in a subshell with the
# given KIND_CONFIG and KIND_HOST_PORT. Echoes the combined output and returns
# the function's exit status. The subshell isolates the env mutations and keeps
# `main` from running (the BASH_SOURCE guard at the bottom of deploy-infra.sh
# skips it when sourced).
run_render() {
  local out_path="$1"
  local kind_config="$2"
  local kind_host_port="$3"
  (
    export KIND_CONFIG="${kind_config}"
    export KIND_HOST_PORT="${kind_host_port}"
    # The third variable render_kind_config reads. Pinned rather than
    # inherited: a developer who exported WITH_REGISTRY_CACHE=true for a local
    # `make deploy-infra` would otherwise get the containerd mirror patch
    # appended here, and the verbatim-copy assertions below would fail on a
    # difference that has nothing to do with KIND_CONFIG.
    export WITH_REGISTRY_CACHE=false
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    render_kind_config "${out_path}"
  ) 2>&1
}

# Source deploy-infra.sh in a subshell with the given KIND_CONFIG and call
# warn_unused_kind_config. Echoes the combined output.
run_warn_unused() {
  local kind_config="$1"
  # Optional working directory for the subshell. A relative KIND_CONFIG resolves
  # against it, the way `make deploy-infra` resolves one against the repository
  # root. Defaults to the caller's, so the absolute-path cases are unaffected.
  local cwd="${2:-$PWD}"
  # Optional CDPATH for the subshell. Exported the way a developer's shell
  # profile exports one, so the resolution inside warn_unused_kind_config meets
  # it. Empty by default, which is the state every other case assumes.
  local cdpath="${3:-}"
  (
    cd "${cwd}" || exit 1
    export CDPATH="${cdpath}"
    export KIND_CONFIG="${kind_config}"
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    warn_unused_kind_config "the cluster is pre-created (SKIP_KIND_CREATE=true)"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test 1: node roster — one control plane first, then two workers.
# nodes[0] must be the control plane because render_kind_config rewrites
# nodes[0] only.
# ---------------------------------------------------------------------------
test_node_roles() {
  echo "Test: kind-config-multinode.yaml is 1 control-plane + 2 workers, in that order"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  assert_eq "three nodes" "3" "$(yq -r '.nodes | length' "$MULTINODE_CONFIG")"
  assert_eq "nodes[0].role is control-plane" "control-plane" \
    "$(yq -r '.nodes[0].role' "$MULTINODE_CONFIG")"
  assert_eq "nodes[1].role is worker" "worker" \
    "$(yq -r '.nodes[1].role' "$MULTINODE_CONFIG")"
  assert_eq "nodes[2].role is worker" "worker" \
    "$(yq -r '.nodes[2].role' "$MULTINODE_CONFIG")"
}

# ---------------------------------------------------------------------------
# Test 2: the control-plane node carries both host-port bridges from
# hack/kind-config.yaml with the same shape — 443 → 31443 (Envoy Gateway) and
# 8428 → 30428 (dizzy VictoriaMetrics), TCP, loopback-only.
# ---------------------------------------------------------------------------
test_control_plane_port_mappings() {
  echo "Test: nodes[0] carries the 443→31443 and 8428→30428 mappings"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (8 checks skipped)"
    SKIP=$((SKIP + 8))
    return
  fi

  local host container
  # containerPort each host port must bridge to.
  for host in 443 8428; do
    case "$host" in
      443) container=31443 ;;
      *) container=30428 ;;
    esac

    # Exactly one entry per host port — a duplicate makes kind fail to create
    # the cluster.
    assert_eq "exactly one hostPort=${host} entry under nodes[0]" "1" \
      "$(yq -r "[.nodes[0].extraPortMappings[] | select(.hostPort == ${host})] | length" "$MULTINODE_CONFIG")"
    assert_eq "hostPort=${host} bridges to containerPort ${container}" "$container" \
      "$(yq -r ".nodes[0].extraPortMappings[] | select(.hostPort == ${host}) | .containerPort" "$MULTINODE_CONFIG")"
    assert_eq "hostPort=${host} protocol is TCP" "TCP" \
      "$(yq -r ".nodes[0].extraPortMappings[] | select(.hostPort == ${host}) | .protocol" "$MULTINODE_CONFIG")"
    assert_eq "hostPort=${host} listenAddress is 127.0.0.1" "127.0.0.1" \
      "$(yq -r ".nodes[0].extraPortMappings[] | select(.hostPort == ${host}) | .listenAddress" "$MULTINODE_CONFIG")"
  done
}

# ---------------------------------------------------------------------------
# Test 3: neither worker binds a host port. A second mapping for the same host
# port cannot bind, so only the control-plane node carries the bridges.
# ---------------------------------------------------------------------------
test_workers_have_no_port_mappings() {
  echo "Test: neither worker declares extraPortMappings"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  assert_eq "nodes[1].extraPortMappings is absent" "null" \
    "$(yq -r '.nodes[1].extraPortMappings' "$MULTINODE_CONFIG")"
  assert_eq "nodes[2].extraPortMappings is absent" "null" \
    "$(yq -r '.nodes[2].extraPortMappings' "$MULTINODE_CONFIG")"
}

# ---------------------------------------------------------------------------
# Test 4: KIND_CONFIG=<multinode> at the default host port copies the file
# verbatim, the same no-yq path the single-node default takes.
# ---------------------------------------------------------------------------
test_render_multinode_byte_equal_copy() {
  echo "Test: render_kind_config with KIND_CONFIG=<multinode>, KIND_HOST_PORT=443 copies verbatim"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  local out="$tmp/rendered.yaml"
  local output exit_code
  output="$(run_render "$out" "$MULTINODE_CONFIG" "443")"
  exit_code=$?

  assert_eq "render_kind_config exits 0" "0" "$exit_code"

  if [[ ! -f "$out" ]]; then
    echo "  FAIL: render_kind_config did not produce the output file"
    FAIL=$((FAIL + 1))
    echo "  output was: $output"
    return
  fi

  if cmp -s "$out" "$MULTINODE_CONFIG"; then
    echo "  PASS: rendered file is byte-equal to hack/kind-config-multinode.yaml"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: rendered file differs from hack/kind-config-multinode.yaml"
    FAIL=$((FAIL + 1))
    diff "$MULTINODE_CONFIG" "$out" | head -20
  fi
}

# ---------------------------------------------------------------------------
# Test 5: a KIND_HOST_PORT override on the multi-node config rewrites the 443
# mapping and nothing else — the 8428 dizzy bridge and both workers survive
# unchanged.
# ---------------------------------------------------------------------------
test_render_multinode_host_port_override() {
  echo "Test: render_kind_config with KIND_CONFIG=<multinode>, KIND_HOST_PORT=8443 rewrites only the 443 mapping"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  local out="$tmp/rendered.yaml"
  local output exit_code
  output="$(run_render "$out" "$MULTINODE_CONFIG" "8443")"
  exit_code=$?

  assert_eq "render_kind_config exits 0 with the override port" "0" "$exit_code"

  if [[ ! -f "$out" ]]; then
    echo "  FAIL: render_kind_config did not produce the output file"
    FAIL=$((FAIL + 1))
    echo "  output was: $output"
    return
  fi

  assert_eq "the 443 Envoy mapping mutated to 8443" "8443" \
    "$(yq -r '.nodes[0].extraPortMappings[] | select(.hostPort == 8443) | .hostPort' "$out")"

  if diff <(yq -r '.nodes[0].extraPortMappings[] | select(.hostPort == 8428)' "$MULTINODE_CONFIG") \
          <(yq -r '.nodes[0].extraPortMappings[] | select(.hostPort == 8428)' "$out") >/dev/null; then
    echo "  PASS: the 8428 dizzy mapping is unchanged under the override"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the 8428 dizzy mapping changed under the override"
    FAIL=$((FAIL + 1))
  fi

  local idx
  for idx in 1 2; do
    if diff <(yq -r ".nodes[${idx}]" "$MULTINODE_CONFIG") \
            <(yq -r ".nodes[${idx}]" "$out") >/dev/null; then
      echo "  PASS: nodes[${idx}] is unchanged under the override"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: nodes[${idx}] changed under the override"
      FAIL=$((FAIL + 1))
    fi
  done
}

# ---------------------------------------------------------------------------
# Test 6: a KIND_CONFIG pointing at a missing file fails before `kind create
# cluster` runs, naming the path. A typo would otherwise surface as a `cp`
# error deep in the deploy.
# ---------------------------------------------------------------------------
test_missing_kind_config_rejected() {
  echo "Test: render_kind_config rejects an unreadable KIND_CONFIG"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  local output exit_code
  output="$(run_render "$tmp/rendered.yaml" "/nonexistent.yaml" "443")"
  exit_code=$?

  assert_nonzero_exit "missing KIND_CONFIG exits non-zero" "$exit_code"
  assert_contains "the error names the unreadable path" \
    "$output" "does not exist or is not readable"
  assert_contains "the error surfaces the offending value" \
    "$output" "KIND_CONFIG='/nonexistent.yaml'"
}

# ---------------------------------------------------------------------------
# Test 7: KIND_CONFIG defaults to the single-node hack/kind-config.yaml, both
# when unset and when passed empty. An empty value must not render an empty
# path.
# ---------------------------------------------------------------------------
test_kind_config_default() {
  echo "Test: KIND_CONFIG defaults to hack/kind-config.yaml when unset or empty"

  local resolved
  resolved="$(
    unset KIND_CONFIG
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH" >/dev/null 2>&1
    printf '%s' "$KIND_CONFIG"
  )"
  assert_eq "unset KIND_CONFIG resolves to the single-node config" \
    "$DEFAULT_CONFIG" "$resolved"

  resolved="$(
    export KIND_CONFIG=""
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH" >/dev/null 2>&1
    printf '%s' "$KIND_CONFIG"
  )"
  assert_eq "empty KIND_CONFIG resolves to the single-node config" \
    "$DEFAULT_CONFIG" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 8: the two configs bridge the same host ports.
# hack/kind-config-multinode.yaml duplicates nodes[0] from
# hack/kind-config.yaml because kind has no include mechanism. Test 2 pins the
# multi-node file against literals and kind_config_port_mapping_test.sh pins
# the single-node file against the same ones, so a mapping added to one file
# and forgotten in the other passes both. Compare them directly.
# ---------------------------------------------------------------------------
test_port_mappings_match_the_single_node_config() {
  echo "Test: nodes[0].extraPortMappings matches hack/kind-config.yaml"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  assert_eq "nodes[0].extraPortMappings is identical in both configs" \
    "$(yq -o=json -I=0 '.nodes[0].extraPortMappings' "$DEFAULT_CONFIG")" \
    "$(yq -o=json -I=0 '.nodes[0].extraPortMappings' "$MULTINODE_CONFIG")"
}

# ---------------------------------------------------------------------------
# Test 9: a KIND_CONFIG that reaches no cluster creation is reported.
# render_kind_config runs on one of the three Step-1 branches only, so on the
# other two the value changes nothing while the banner still prints it. Without
# the warning a developer who reruns `make deploy-infra` against an existing
# single-node cluster reads the banner as confirmation and debugs the operator
# instead of the cluster.
# ---------------------------------------------------------------------------
test_unused_kind_config_is_reported() {
  echo "Test: warn_unused_kind_config reports a non-default KIND_CONFIG"

  local output
  output="$(run_warn_unused "$MULTINODE_CONFIG")"

  assert_contains "the warning names the ignored value" \
    "$output" "KIND_CONFIG='${MULTINODE_CONFIG}' is ignored"
  assert_contains "the warning names why it is ignored" \
    "$output" "the cluster is pre-created (SKIP_KIND_CREATE=true)"
  assert_contains "the warning names where the config has to go instead" \
    "$output" "config:"
}

# ---------------------------------------------------------------------------
# Test 10: the default KIND_CONFIG stays silent. Every run that does not
# override the variable takes one of these branches, so a warning here would be
# noise on every CI job.
# ---------------------------------------------------------------------------
test_default_kind_config_is_not_reported() {
  echo "Test: warn_unused_kind_config stays silent on the default config"

  local output
  output="$(run_warn_unused "$DEFAULT_CONFIG")"

  assert_not_contains "no warning for the default config" "$output" "is ignored"
}

# ---------------------------------------------------------------------------
# Test 11: the default spelled the way the docs spell it stays silent too.
# hack/kind-config-multinode.yaml and the KIND_CONFIG row of
# docs/reference/infrastructure/e2e-deployment.md both name the configs as
# repository-relative paths, so `KIND_CONFIG=hack/kind-config.yaml make
# deploy-infra` is how a developer switches back. Compared as a raw string
# against the absolute default that warns about the default itself, and a
# warning that fires on the default is the one developers learn to skip.
# ---------------------------------------------------------------------------
test_relative_default_kind_config_is_not_reported() {
  echo "Test: warn_unused_kind_config stays silent on the relative default path"

  local output
  output="$(run_warn_unused "hack/kind-config.yaml" "$PROJECT_ROOT")"

  assert_not_contains "no warning for the relative default config" "$output" "is ignored"
}

# ---------------------------------------------------------------------------
# Test 12: resolving the path must not swallow a genuine override that happens
# to be spelled relatively — the exact form the multinode config documents.
# The warning still quotes the value as it was given, not as it resolved, so it
# matches what the developer typed.
# ---------------------------------------------------------------------------
test_relative_multinode_kind_config_is_reported() {
  echo "Test: warn_unused_kind_config still reports a relative non-default KIND_CONFIG"

  local output
  output="$(run_warn_unused "hack/kind-config-multinode.yaml" "$PROJECT_ROOT")"

  assert_contains "the warning names the ignored value as it was given" \
    "$output" "KIND_CONFIG='hack/kind-config-multinode.yaml' is ignored"
}

# ---------------------------------------------------------------------------
# Test 13: an exported CDPATH must not turn the default into a warning. `cd`
# writes the directory it landed in to stdout whenever it found it through a
# CDPATH component, and a relative KIND_CONFIG is the only kind that search
# applies to — so the resolution captures two lines and compares unequal, on the
# default config, for a developer who has `export CDPATH=...` in their profile.
# ---------------------------------------------------------------------------
test_default_kind_config_is_not_reported_under_cdpath() {
  echo "Test: warn_unused_kind_config stays silent on the default config under an exported CDPATH"

  # A CDPATH entry that carries a `hack` directory of its own, which is what
  # `cd hack` would resolve to instead of the repository's.
  local cdpath_root
  cdpath_root="$(mktemp -d)"
  mkdir -p "${cdpath_root}/hack"

  local output
  output="$(run_warn_unused "hack/kind-config.yaml" "$PROJECT_ROOT" "${cdpath_root}")"
  rm -rf "${cdpath_root}"

  assert_not_contains "no warning for the default config under CDPATH" \
    "$output" "is ignored"
}

# ---------------------------------------------------------------------------
# Test 14: both non-creating Step-1 branches call the warning. Running main()
# would need a cluster, so pin the two call sites in the source instead.
# ---------------------------------------------------------------------------
test_both_skip_branches_warn() {
  echo "Test: both Step-1 branches that skip creation call warn_unused_kind_config"

  local call_count
  call_count="$(grep -cE '^[[:space:]]+warn_unused_kind_config ' "$DEPLOY_INFRA_SH" || true)"
  assert_eq "warn_unused_kind_config is called on both skip branches" "2" "$call_count"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_node_roles
test_control_plane_port_mappings
test_workers_have_no_port_mappings
test_render_multinode_byte_equal_copy
test_render_multinode_host_port_override
test_missing_kind_config_rejected
test_kind_config_default
test_port_mappings_match_the_single_node_config
test_unused_kind_config_is_reported
test_default_kind_config_is_not_reported
test_relative_default_kind_config_is_not_reported
test_relative_multinode_kind_config_is_reported
test_default_kind_config_is_not_reported_under_cdpath
test_both_skip_branches_warn

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
