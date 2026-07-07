#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/refresh-operator-image-digests.sh: resolving the digest behind
# the self-built operators' :latest images, rendering/applying the
# per-operator image-digest ConfigMaps, requesting a HelmRelease reconcile
# only when the digest changed, and tolerating per-image resolve failures.
# Also asserts (structurally) that hack/deploy-infra.sh calls the script only
# inside the WITH_CONTROLPLANE=true CONTROLPLANE_OPERATORS=flux branch and
# best-effort.
#
# Strategy: source the script in a subshell (the BASH_SOURCE guard keeps main
# from running) with recording docker/kubectl stubs prepended to PATH — the
# same harness as the sibling deploy_infra_*_test.sh files.
#
# Usage: bash tests/unit/hack/refresh_operator_image_digests_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
REFRESH_SH="$PROJECT_ROOT/hack/refresh-operator-image-digests.sh"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

KEYSTONE_DIGEST="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
C5C3_DIGEST="sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
HORIZON_DIGEST="sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stubs <dir>
# Recording docker/kubectl stubs.
#   docker buildx imagetools inspect <ref> → prints the JSON-quoted per-image
#     digest ("sha256:…"), or exits 1 when <ref> equals $DOCKER_FAIL_FOR.
#   kubectl get configmap <name> …         → prints the stored values payload
#     for that ConfigMap when KUBECTL_CM_EXISTS=true, else nothing.
#   kubectl apply -f -                     → records argv and captures stdin
#     into $KUBECTL_APPLY_LOG.
#   kubectl annotate …                     → appends argv to
#     $KUBECTL_ANNOTATE_LOG.
make_stubs() {
  local dir="$1"
  mkdir -p "$dir"

  cat >"$dir/docker" <<'STUB'
#!/bin/bash
echo "docker $*" >> "$DOCKER_LOG"
if [ "${1:-}" = "buildx" ] && [ "${2:-}" = "version" ]; then
  exit 0
fi
if [ "${1:-}" = "buildx" ] && [ "${2:-}" = "imagetools" ] && [ "${3:-}" = "inspect" ]; then
  ref="${4:-}"
  if [ -n "${DOCKER_FAIL_FOR:-}" ] && [ "$ref" = "${DOCKER_FAIL_FOR}" ]; then
    exit 1
  fi
  case "$ref" in
    *keystone-operator*) printf '"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"' ;;
    *c5c3-operator*)     printf '"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"' ;;
    *horizon-operator*)  printf '"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"' ;;
    *) exit 1 ;;
  esac
  exit 0
fi
exit 0
STUB

  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
if [ "${1:-}" = "get" ] && [ "${2:-}" = "configmap" ]; then
  if [ "${KUBECTL_CM_EXISTS:-false}" = "true" ]; then
    case "${3:-}" in
      keystone-operator-image-digest) printf 'image:\n  digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n' ;;
      c5c3-operator-image-digest)     printf 'image:\n  digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n' ;;
      horizon-operator-image-digest)  printf 'image:\n  digest: sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc\n' ;;
    esac
  fi
  exit 0
fi
if [ "${1:-}" = "apply" ]; then
  {
    echo "--- kubectl $*"
    cat
  } >> "$KUBECTL_APPLY_LOG"
  exit 0
fi
if [ "${1:-}" = "annotate" ]; then
  printf '%s\n' "annotate $*" >> "$KUBECTL_ANNOTATE_LOG"
  exit 0
fi
exit 0
STUB

  chmod +x "$dir/docker" "$dir/kubectl"
}

# run_refresh <tmp dir> [env assignments...]
# Sources the script with stubs on PATH and runs the refresh loop. Echoes the
# combined output; the return code is the loop's return code.
run_refresh() {
  local tmp="$1"
  shift
  (
    for assignment in "$@"; do
      export "${assignment?}"
    done
    export PATH="$tmp/bin:$PATH"
    export DOCKER_LOG="$tmp/docker.log"
    export KUBECTL_APPLY_LOG="$tmp/apply.log"
    export KUBECTL_ANNOTATE_LOG="$tmp/annotate.log"
    touch "$DOCKER_LOG" "$KUBECTL_APPLY_LOG" "$KUBECTL_ANNOTATE_LOG"
    # shellcheck source=/dev/null
    source "$REFRESH_SH"
    refresh_operator_image_digests
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test 1: resolve_image_digest strips the JSON quotes
# ---------------------------------------------------------------------------
test_resolve_image_digest_success() {
  echo "Test: resolve_image_digest resolves and strips JSON quotes"
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"

  local digest
  digest=$(
    export PATH="$tmp/bin:$PATH" DOCKER_LOG="$tmp/docker.log"
    # shellcheck source=/dev/null
    source "$REFRESH_SH"
    resolve_image_digest "ghcr.io/c5c3/keystone-operator:latest"
  )
  assert_eq "digest resolved without quotes" "$KEYSTONE_DIGEST" "$digest"
}

# ---------------------------------------------------------------------------
# Test 2: resolve_image_digest fails when the registry is unreachable
# ---------------------------------------------------------------------------
test_resolve_image_digest_failure() {
  echo "Test: resolve_image_digest returns non-zero on inspect failure"
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"

  local out rc=0
  out=$(
    export PATH="$tmp/bin:$PATH" DOCKER_LOG="$tmp/docker.log"
    export DOCKER_FAIL_FOR="ghcr.io/c5c3/keystone-operator:latest"
    # shellcheck source=/dev/null
    source "$REFRESH_SH"
    resolve_image_digest "ghcr.io/c5c3/keystone-operator:latest"
  ) || rc=$?
  assert_nonzero_exit "resolve failure propagates" "$rc"
  assert_eq "no digest is echoed on failure" "" "$out"
}

# ---------------------------------------------------------------------------
# Test 3: render_digest_configmap renders the Flux values payload
# ---------------------------------------------------------------------------
test_render_digest_configmap_yaml() {
  echo "Test: render_digest_configmap renders name/namespace and the indented payload"
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"

  local rendered
  rendered=$(
    export PATH="$tmp/bin:$PATH"
    # shellcheck source=/dev/null
    source "$REFRESH_SH"
    render_digest_configmap "keystone-operator-image-digest" "keystone-system" "$KEYSTONE_DIGEST"
  )
  assert_contains "kind" "$rendered" "kind: ConfigMap"
  assert_contains "name" "$rendered" "name: keystone-operator-image-digest"
  assert_contains "namespace" "$rendered" "namespace: keystone-system"
  assert_contains "data key" "$rendered" "values.yaml: |"
  # The 4-space block-scalar indentation is load-bearing: Flux parses the
  # payload as YAML and merges image.digest into the HelmRelease values.
  assert_contains "indented image key" "$rendered" $'\n    image:\n      digest: '"$KEYSTONE_DIGEST"
}

# ---------------------------------------------------------------------------
# Test 4: the loop applies all three ConfigMaps and requests reconciles
# ---------------------------------------------------------------------------
test_refresh_applies_and_annotates_all_operators() {
  echo "Test: refresh applies 3 ConfigMaps and annotates 3 HelmReleases"
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"

  local out rc=0
  out=$(run_refresh "$tmp") || rc=$?
  assert_eq "refresh succeeds" "0" "$rc"

  local apply_log annotate_log
  apply_log="$(cat "$tmp/apply.log")"
  annotate_log="$(cat "$tmp/annotate.log")"

  assert_contains "keystone ConfigMap applied" "$apply_log" "name: keystone-operator-image-digest"
  assert_contains "keystone ConfigMap namespace" "$apply_log" "namespace: keystone-system"
  assert_contains "keystone digest in payload" "$apply_log" "$KEYSTONE_DIGEST"
  assert_contains "c5c3 ConfigMap applied" "$apply_log" "name: c5c3-operator-image-digest"
  assert_contains "c5c3 ConfigMap namespace" "$apply_log" "namespace: c5c3-system"
  assert_contains "c5c3 digest in payload" "$apply_log" "$C5C3_DIGEST"
  assert_contains "horizon ConfigMap applied" "$apply_log" "name: horizon-operator-image-digest"
  assert_contains "horizon ConfigMap namespace" "$apply_log" "namespace: horizon-system"
  assert_contains "horizon digest in payload" "$apply_log" "$HORIZON_DIGEST"

  assert_eq "three reconcile annotations" "3" "$(grep -c 'annotate helmrelease/' "$tmp/annotate.log")"
  assert_contains "keystone reconcile requested" "$annotate_log" "helmrelease/keystone-operator"
  assert_contains "keystone reconcile namespace" "$annotate_log" "-n keystone-system"
  assert_contains "c5c3 reconcile requested" "$annotate_log" "helmrelease/c5c3-operator"
  assert_contains "horizon reconcile requested" "$annotate_log" "helmrelease/horizon-operator"
  assert_contains "requestedAt annotation" "$annotate_log" "reconcile.fluxcd.io/requestedAt="
  assert_contains "annotation is idempotent (overwrite)" "$annotate_log" "overwrite"
  assert_contains "pinned log line" "$out" "pinned to"
}

# ---------------------------------------------------------------------------
# Test 5: unchanged digests re-apply the ConfigMaps but skip the reconcile
# ---------------------------------------------------------------------------
test_refresh_skips_annotate_when_digest_unchanged() {
  echo "Test: unchanged digest skips the reconcile request"
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"

  local out rc=0
  out=$(run_refresh "$tmp" KUBECTL_CM_EXISTS=true) || rc=$?
  assert_eq "refresh succeeds" "0" "$rc"
  assert_eq "three ConfigMap applies (idempotent re-apply)" "3" "$(grep -c -- '--- kubectl apply' "$tmp/apply.log")"
  assert_eq "no reconcile annotations" "0" "$(grep -c 'annotate' "$tmp/annotate.log")"
  assert_contains "unchanged log line" "$out" "digest unchanged"
}

# ---------------------------------------------------------------------------
# Test 6: one unresolvable image does not block the others
# ---------------------------------------------------------------------------
test_refresh_continues_on_resolve_failure() {
  echo "Test: a per-image resolve failure warns, continues, and fails the exit code"
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"

  local out rc=0
  out=$(run_refresh "$tmp" DOCKER_FAIL_FOR=ghcr.io/c5c3/keystone-operator:latest) || rc=$?
  assert_nonzero_exit "overall exit code reflects the failure" "$rc"
  assert_contains "warning logged" "$out" "could not resolve digest for ghcr.io/c5c3/keystone-operator:latest"

  local apply_log
  apply_log="$(cat "$tmp/apply.log")"
  assert_not_contains "keystone ConfigMap not applied" "$apply_log" "name: keystone-operator-image-digest"
  assert_contains "c5c3 ConfigMap still applied" "$apply_log" "name: c5c3-operator-image-digest"
  assert_contains "horizon ConfigMap still applied" "$apply_log" "name: horizon-operator-image-digest"
  assert_eq "two reconcile annotations" "2" "$(grep -c 'annotate helmrelease/' "$tmp/annotate.log")"
}

# ---------------------------------------------------------------------------
# Test 7: deploy-infra.sh calls the refresh only on the flux ControlPlane path
# ---------------------------------------------------------------------------
test_deploy_infra_gates_refresh_call() {
  echo "Test: deploy-infra.sh gates the refresh call inside the flux branch, best-effort"

  local call_line gate_line elif_line
  call_line="$(grep -n 'refresh-operator-image-digests.sh' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  gate_line="$(grep -n '"${WITH_CONTROLPLANE}" == "true" && "${CONTROLPLANE_OPERATORS}" == "flux"' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  elif_line="$(grep -n 'elif \[\[ "${WITH_CONTROLPLANE}" == "true" \]\]' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"

  assert_not_empty "refresh call site found" "$call_line"
  assert_not_empty "flux gate found" "$gate_line"
  assert_not_empty "external-operators elif found" "$elif_line"

  if [ -n "$call_line" ] && [ -n "$gate_line" ] && [ -n "$elif_line" ]; then
    assert_eq "flux gate precedes the call" "1" "$((gate_line < call_line ? 1 : 0))"
    assert_eq "call precedes the external-operators elif" "1" "$((call_line < elif_line ? 1 : 0))"
    assert_contains "call is best-effort" "$(sed -n "${call_line},$((call_line + 1))p" "$DEPLOY_INFRA_SH")" "||"
  fi
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_resolve_image_digest_success
test_resolve_image_digest_failure
test_render_digest_configmap_yaml
test_refresh_applies_and_annotates_all_operators
test_refresh_skips_annotate_when_digest_unchanged
test_refresh_continues_on_resolve_failure
test_deploy_infra_gates_refresh_call

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
