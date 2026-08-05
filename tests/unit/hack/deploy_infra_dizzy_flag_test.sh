#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh gates the dizzy observability stack
# (VictoriaMetrics + Grafana) behind WITH_DIZZY so the kind Quick Start stays
# minimal by default and only installs the stack when explicitly requested.
#
# Implementation: bash + tests/lib/assertions.sh — mirrors the sibling
# tests/unit/hack/deploy_infra_metrics_server_flag_test.sh pattern. The repo has
# zero .bats files and no shellspec runner; introducing one would add an
# undeclared test dependency.
#
# Strategy: hybrid — source the script (the `BASH_SOURCE[0] == ${0}` guard at
# the bottom of deploy-infra.sh keeps main() from auto-running) to assert the
# runtime default of WITH_DIZZY for each env scenario, and grep the script
# source to lock in the three strict gate locations:
#   1. Step 3 overlay apply (stage-dashboards + kubectl apply -k deploy/kind/dizzy)
#   2. Phase 3 helm-release wait list append (dizzy-victoria-metrics, dizzy-grafana)
#   3. post-wait Grafana URL log
# The configuration-banner line is asserted via grep so the user-visible summary
# stays in lockstep with the runtime value. The 8428 kind extraPortMapping is
# pinned here too: it must survive a KIND_HOST_PORT override byte-for-byte so a
# non-privileged host port never disturbs the dizzy OTLP ingest bridge.
#
# Usage: bash tests/unit/hack/deploy_infra_dizzy_flag_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"
KIND_CONFIG_FILE="$PROJECT_ROOT/hack/kind-config.yaml"
DIZZY_KUSTOMIZATION="$PROJECT_ROOT/deploy/kind/dizzy/kustomization.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# resolve_with_dizzy [env_var=value...]
# Sources deploy-infra.sh in a subshell with the supplied env overrides and
# echoes the resolved value of WITH_DIZZY after the configuration block runs.
# The `BASH_SOURCE[0] == ${0}` guard at the bottom of deploy-infra.sh keeps
# main() from auto-running when the script is sourced.
resolve_with_dizzy() {
  (
    for assignment in "$@"; do
      export "${assignment?}"
    done
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    printf '%s' "${WITH_DIZZY}"
  )
}

# run_render OUT_PATH KIND_HOST_PORT
# Sources the script and calls render_kind_config in a subshell with the given
# KIND_HOST_PORT, writing to OUT_PATH. Echoes combined output and returns the
# function's exit status. Subshell isolates env mutations and keeps main() from
# running (BASH_SOURCE guard). Mirrors deploy_infra_kind_host_port_test.sh.
run_render() {
  local out_path="$1"
  local kind_host_port="$2"
  (
    export KIND_HOST_PORT="${kind_host_port}"
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    render_kind_config "${out_path}"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test 1: WITH_DIZZY defaults to false
# ---------------------------------------------------------------------------
test_default_is_false() {
  echo "Test: WITH_DIZZY defaults to false"

  # Unset any inherited value so we observe the script's own default.
  local resolved
  resolved="$(unset WITH_DIZZY; resolve_with_dizzy)"
  assert_eq "WITH_DIZZY defaults to false" "false" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 2: explicit WITH_DIZZY=true
# ---------------------------------------------------------------------------
test_explicit_true() {
  echo "Test: WITH_DIZZY=true is preserved"

  local resolved
  resolved="$(resolve_with_dizzy WITH_DIZZY=true)"
  assert_eq "WITH_DIZZY=true is preserved" "true" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 3: explicit WITH_DIZZY=false
# ---------------------------------------------------------------------------
test_explicit_false() {
  echo "Test: WITH_DIZZY=false is preserved"

  local resolved
  resolved="$(resolve_with_dizzy WITH_DIZZY=false)"
  assert_eq "WITH_DIZZY=false is preserved" "false" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 4: defensive non-true value
# A typo like WITH_DIZZY=yes must NOT enable the overlay; every gate uses the
# strict `== "true"` comparison. We assert the value passes through verbatim AND
# that all gate sites use exact-match. There are exactly three runtime gates:
#   - Step 3 stage-dashboards + overlay apply
#   - Phase 3 helm_releases append
#   - post-wait Grafana URL log
# ---------------------------------------------------------------------------
test_non_true_value_does_not_trigger_install() {
  echo "Test: WITH_DIZZY=yes passes through but does not trigger install"

  local resolved
  resolved="$(resolve_with_dizzy WITH_DIZZY=yes)"
  assert_eq "WITH_DIZZY=yes is preserved verbatim" "yes" "$resolved"

  local gate_count
  gate_count="$(grep -cE '"\$\{WITH_DIZZY\}" == "true"' "$DEPLOY_INFRA_SH" || true)"
  assert_eq "deploy-infra.sh has exactly 3 strict WITH_DIZZY==true gates" "3" "$gate_count"
}

# ---------------------------------------------------------------------------
# Test 5: configuration banner line
# The user-visible summary must surface the WITH_DIZZY state so operators can
# spot accidental opt-ins / opt-outs at a glance, and name the stack it gates.
# ---------------------------------------------------------------------------
test_banner_includes_dizzy_line() {
  echo "Test: configuration banner surfaces WITH_DIZZY state"

  assert_file_contains \
    "deploy-infra.sh banner mentions the dizzy stack" \
    "$DEPLOY_INFRA_SH" \
    'dizzy stack'

  assert_file_contains \
    "deploy-infra.sh banner names the VictoriaMetrics + Grafana stack the flag gates" \
    "$DEPLOY_INFRA_SH" \
    'VictoriaMetrics + Grafana'
}

# ---------------------------------------------------------------------------
# Test 6: Step 3 overlay apply is gated, and stage-dashboards runs first
# The kustomize apply for deploy/kind/dizzy must live inside the WITH_DIZZY gate
# so the default Quick Start does not install it, and dizzy.sh stage-dashboards
# MUST run inside the SAME gate BEFORE the apply so the configMapGenerator's
# staged JSONs exist when kustomize renders the ConfigMap.
# ---------------------------------------------------------------------------
test_dizzy_apply_is_gated_after_staging() {
  echo "Test: dizzy overlay apply is gated and staged after dizzy.sh stage-dashboards"

  # There is exactly one apply of the dizzy overlay.
  local apply_hits
  apply_hits="$(grep -cF 'kubectl apply -k "${REPO_ROOT}/deploy/kind/dizzy"' "$DEPLOY_INFRA_SH" || true)"
  assert_eq "deploy-infra.sh applies the dizzy overlay exactly once" "1" "$apply_hits"

  local apply_line stage_line gate_before_apply gate_before_stage
  apply_line="$(grep -nF 'kubectl apply -k "${REPO_ROOT}/deploy/kind/dizzy"' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  stage_line="$(grep -nF 'dizzy.sh" stage-dashboards' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"

  assert_not_empty "kubectl apply -k deploy/kind/dizzy line is found" "$apply_line"
  assert_not_empty "dizzy.sh stage-dashboards line is found" "$stage_line"

  # The most recent WITH_DIZZY gate above each anchor line.
  gate_before_apply="$(grep -n '"${WITH_DIZZY}" == "true"' "$DEPLOY_INFRA_SH" | awk -F: -v target="${apply_line:-0}" '$1 < target { last = $1 } END { print last }')"
  gate_before_stage="$(grep -n '"${WITH_DIZZY}" == "true"' "$DEPLOY_INFRA_SH" | awk -F: -v target="${stage_line:-0}" '$1 < target { last = $1 } END { print last }')"

  assert_not_empty "WITH_DIZZY gate precedes the overlay apply" "$gate_before_apply"
  assert_not_empty "WITH_DIZZY gate precedes stage-dashboards" "$gate_before_stage"

  # stage-dashboards must run before the apply, and both under the same gate.
  if [[ -n "$stage_line" && -n "$apply_line" && "$stage_line" -lt "$apply_line" ]]; then
    echo "  PASS: stage-dashboards ($stage_line) runs before the overlay apply ($apply_line)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: stage-dashboards ($stage_line) does not run before the overlay apply ($apply_line)"
    FAIL=$((FAIL + 1))
  fi

  assert_eq "stage-dashboards and the overlay apply share the same WITH_DIZZY gate" \
    "$gate_before_apply" "$gate_before_stage"
}

# ---------------------------------------------------------------------------
# Test 7: Phase 3 wait list append is gated, base array is byte-identical
# The Phase 3 wait list must NOT statically include the dizzy releases; they
# must be appended dynamically inside the WITH_DIZZY gate so the relative
# ordering of the base releases is preserved. The base array declaration line is
# pinned byte-for-byte so an accidental edit to it is caught here.
# ---------------------------------------------------------------------------
test_phase3_append_is_gated() {
  echo "Test: dizzy releases are appended dynamically to the helm-release wait list"

  # The base array must NOT include the dizzy releases inline.
  assert_file_not_contains \
    "deploy-infra.sh does not hard-code dizzy-victoria-metrics in the base wait list" \
    "$DEPLOY_INFRA_SH" \
    'openbao-operator dizzy-victoria-metrics'

  # The base array line is pinned byte-for-byte (copied verbatim from the
  # script) so an accidental reorder or rename of a base release trips here.
  assert_file_contains \
    "helm_releases base array line is byte-identical to the expected literal" \
    "$DEPLOY_INFRA_SH" \
    'local helm_releases=(prometheus-operator-crds openbao mariadb-operator-crds mariadb-operator external-secrets memcached-operator envoy-gateway garage-operator openbao-operator)'

  # The dynamic append exists.
  assert_file_contains \
    "dizzy releases are appended via helm_releases+=(dizzy-victoria-metrics dizzy-grafana)" \
    "$DEPLOY_INFRA_SH" \
    'helm_releases+=(dizzy-victoria-metrics dizzy-grafana)'

  # And the append sits inside a WITH_DIZZY gate.
  local append_line gate_before_append
  append_line="$(grep -nF 'helm_releases+=(dizzy-victoria-metrics dizzy-grafana)' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  gate_before_append="$(grep -n '"${WITH_DIZZY}" == "true"' "$DEPLOY_INFRA_SH" | awk -F: -v target="${append_line:-0}" '$1 < target { last = $1 } END { print last }')"
  assert_not_empty "the dizzy helm_releases append line is found" "$append_line"
  assert_not_empty "a WITH_DIZZY gate precedes the dizzy helm_releases append" "$gate_before_append"
}

# ---------------------------------------------------------------------------
# Test 8: overlay is self-contained (no parent-directory resource entries)
# kubectl's embedded kustomize (used by the production apply) does NOT expose
# --load-restrictor (kubernetes/kubectl#948), so a `../..` resource reference
# would re-introduce the LoadRestrictionsRootOnly failure. Assert zero such
# entries in the kustomization AND in every tracked file of the overlay. The
# pattern is anchored to a YAML list item (`- ../..`) — the same discipline as
# the metrics-server sibling — so the prose that DOCUMENTS the no-`../..`
# contract in the overlay's own comments does not trip the check.
# ---------------------------------------------------------------------------
test_overlay_is_self_contained() {
  echo "Test: dizzy overlay has no '../..' parent-directory resource entries"

  if [[ ! -f "$DIZZY_KUSTOMIZATION" ]]; then
    echo "  FAIL: $DIZZY_KUSTOMIZATION does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  local kust_refs
  kust_refs="$( { grep -E '^[[:space:]]*-[[:space:]]+\.\./\.\.' "$DIZZY_KUSTOMIZATION" || true; } | wc -l | tr -d '[:space:]')"
  assert_eq "kustomization.yaml has no '../..' parent-directory resource entries" "0" "$kust_refs"

  # Every tracked file of the overlay must be free of '../..' resource entries.
  # `git ls-files` yields exactly the tracked set (the git-ignored dashboards/
  # dir is excluded even when a developer has staged JSONs into it).
  local tracked f total_refs=0 checked=0
  tracked="$(git -C "$PROJECT_ROOT" ls-files 'deploy/kind/dizzy' 2>/dev/null)"
  while IFS= read -r f; do
    [[ -z "$f" ]] && continue
    checked=$((checked + 1))
    local n
    n="$( { grep -E '^[[:space:]]*-[[:space:]]+\.\./\.\.' "$PROJECT_ROOT/$f" || true; } | wc -l | tr -d '[:space:]')"
    total_refs=$((total_refs + n))
  done <<< "$tracked"

  if [[ "$checked" -eq 0 ]]; then
    echo "  FAIL: git ls-files returned no tracked overlay files"
    FAIL=$((FAIL + 1))
    return
  fi
  assert_eq "no tracked overlay file ($checked scanned) has a '../..' resource entry" \
    "0" "$total_refs"
}

# ---------------------------------------------------------------------------
# Test 9: kind-config.yaml carries the 8428 → 30428 OTLP ingest mapping
# The dizzy OTLP ingest bridge (host 8428 → NodePort 30428) is declared
# unconditionally so WITH_DIZZY=true can be enabled on any cluster created after
# the mapping landed. Pin exactly one such mapping with the expected fields.
# ---------------------------------------------------------------------------
test_kind_config_has_8428_mapping() {
  echo "Test: hack/kind-config.yaml has exactly one 8428 → 30428 mapping with the expected fields"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  local count
  count="$(yq -r '[.nodes[0].extraPortMappings[] | select(.hostPort == 8428)] | length' "$KIND_CONFIG_FILE")"
  assert_eq "exactly one extraPortMappings entry has hostPort 8428" "1" "$count"

  assert_eq "8428 mapping targets containerPort 30428" \
    "30428" \
    "$(yq -r '.nodes[0].extraPortMappings[] | select(.hostPort == 8428) | .containerPort' "$KIND_CONFIG_FILE")"

  assert_eq "8428 mapping uses protocol TCP" \
    "TCP" \
    "$(yq -r '.nodes[0].extraPortMappings[] | select(.hostPort == 8428) | .protocol' "$KIND_CONFIG_FILE")"

  assert_eq "8428 mapping listens on 127.0.0.1 only" \
    "127.0.0.1" \
    "$(yq -r '.nodes[0].extraPortMappings[] | select(.hostPort == 8428) | .listenAddress' "$KIND_CONFIG_FILE")"

  # There must still be a hostPort=443 entry (the Envoy data-plane bridge) so
  # this mapping is genuinely additive, not a replacement.
  assert_eq "the 443 Envoy data-plane mapping still exists alongside 8428" \
    "1" \
    "$(yq -r '[.nodes[0].extraPortMappings[] | select(.hostPort == 443)] | length' "$KIND_CONFIG_FILE")"
}

# ---------------------------------------------------------------------------
# Test 10: the 8428 mapping survives a KIND_HOST_PORT override byte-for-byte
# render_kind_config rewrites only the hostPort=443 entry to KIND_HOST_PORT; the
# dizzy 8428 mapping must be untouched, or a non-privileged host port would
# collaterally break the dizzy OTLP ingest bridge.
# ---------------------------------------------------------------------------
test_8428_mapping_survives_host_port_override() {
  echo "Test: render_kind_config KIND_HOST_PORT=8443 leaves the 8428 mapping byte-identical"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  local out="$tmp/rendered.yaml"
  local output exit_code
  output="$(run_render "$out" "8443")"
  exit_code=$?

  assert_eq "render_kind_config exits 0 with KIND_HOST_PORT=8443" "0" "$exit_code"

  if [[ ! -f "$out" ]]; then
    echo "  FAIL: render_kind_config did not produce the output file"
    FAIL=$((FAIL + 1))
    echo "  output was: $output"
    return
  fi

  # The 443 entry mutated to 8443.
  assert_eq "the 443 Envoy mapping mutated to the override port 8443" \
    "8443" \
    "$(yq -r '.nodes[0].extraPortMappings[] | select(.hostPort == 8443) | .hostPort' "$out")"

  # The 8428 dizzy mapping is byte-identical between source and rendered output.
  if diff <(yq -r '.nodes[0].extraPortMappings[] | select(.hostPort == 8428)' "$KIND_CONFIG_FILE") \
          <(yq -r '.nodes[0].extraPortMappings[] | select(.hostPort == 8428)' "$out") >/dev/null; then
    echo "  PASS: the 8428 dizzy mapping is byte-identical after the KIND_HOST_PORT override"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the 8428 dizzy mapping changed under the KIND_HOST_PORT override"
    FAIL=$((FAIL + 1))
  fi
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_default_is_false
test_explicit_true
test_explicit_false
test_non_true_value_does_not_trigger_install
test_banner_includes_dizzy_line
test_dizzy_apply_is_gated_after_staging
test_phase3_append_is_gated
test_overlay_is_self_contained
test_kind_config_has_8428_mapping
test_8428_mapping_survives_host_port_override

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
