#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh gates the transparent registry pull-through cache
# (#564) behind WITH_REGISTRY_CACHE so the default Quick Start and CI stay
# byte-for-byte unchanged, and that the three moving parts are correct:
#
#   - render_kind_config injects the containerd `config_path` registry-mirror
#     patch ONLY when WITH_REGISTRY_CACHE=true (and never touches the checked-in
#     hack/kind-config.yaml when the flag is off — CI feeds that file directly).
#   - start_registry_cache creates one labelled distribution-registry proxy per
#     upstream on the kind network, in proxy mode via REGISTRY_PROXY_REMOTEURL,
#     backed by a persistent volume, and is idempotent (a running proxy is left
#     alone).
#   - wire_node_registry_mirror writes a certs.d/<host>/hosts.toml per node/host
#     naming the upstream `server` fallback and the mirror host.
#
# Same project-native bash + tests/lib/assertions.sh strategy as the sibling
# deploy_infra_nofile_test.sh / deploy_infra_chaos_flag_test.sh: source the
# script (its BASH_SOURCE guard keeps main() from auto-running) and exercise the
# functions against recording stubs for docker/kind on PATH — no real cluster or
# Docker daemon is touched.
#
# Usage: bash tests/unit/hack/deploy_infra_registry_cache_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# resolve_flag [env_var=value...]
# Sources deploy-infra.sh in a subshell with the supplied env overrides and
# echoes the resolved WITH_REGISTRY_CACHE default.
resolve_flag() {
  (
    for assignment in "$@"; do
      export "${assignment?}"
    done
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    printf '%s' "${WITH_REGISTRY_CACHE}"
  )
}

# make_docker_stub <dir>
# A recording docker/kind stub. docker appends its argv to $DOCKER_LOG and
# returns per-subcommand exit codes driven by the environment:
#   network inspect → $DOCKER_NETWORK_EXISTS (default 0 = exists)
#   volume inspect  → 1 (absent, so `volume create` runs)
#   inspect -f …    → prints $DOCKER_RUNNING (default false = recreate)
#   everything else → 0
# kind prints $KIND_NODES for `kind get nodes`.
make_docker_stub() {
  local dir="$1"
  mkdir -p "$dir"

  cat >"$dir/docker" <<'STUB'
#!/bin/bash
echo "docker $*" >> "$DOCKER_LOG"
case "${1:-}" in
  network)
    if [ "${2:-}" = "inspect" ]; then exit "${DOCKER_NETWORK_EXISTS:-0}"; fi
    exit 0 ;;
  volume)
    if [ "${2:-}" = "inspect" ]; then exit 1; fi   # always "absent" → create
    exit 0 ;;
  inspect)
    printf '%s' "${DOCKER_RUNNING:-false}"; exit 0 ;;
  *)
    exit 0 ;;
esac
STUB

  cat >"$dir/kind" <<'STUB'
#!/bin/bash
if [ "${1:-}" = "get" ] && [ "${2:-}" = "nodes" ]; then
  printf '%s\n' ${KIND_NODES:-}
fi
STUB

  chmod +x "$dir/docker" "$dir/kind"
}

# ---------------------------------------------------------------------------
# Test 1: WITH_REGISTRY_CACHE default + explicit values
# ---------------------------------------------------------------------------
test_flag_defaults() {
  echo "Test: WITH_REGISTRY_CACHE defaults to false and is preserved"

  local resolved
  resolved="$(unset WITH_REGISTRY_CACHE; resolve_flag)"
  assert_eq "WITH_REGISTRY_CACHE defaults to false" "false" "$resolved"

  resolved="$(resolve_flag WITH_REGISTRY_CACHE=true)"
  assert_eq "WITH_REGISTRY_CACHE=true is preserved" "true" "$resolved"

  resolved="$(resolve_flag WITH_REGISTRY_CACHE=false)"
  assert_eq "WITH_REGISTRY_CACHE=false is preserved" "false" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 2: REGISTRY_CACHE_IMAGE is pinned by tag AND digest (reproducible +
# Renovate-able)
# ---------------------------------------------------------------------------
test_image_pinned_by_digest() {
  echo "Test: REGISTRY_CACHE_IMAGE is pinned by tag and sha256 digest"

  local img
  img="$(
    unset REGISTRY_CACHE_IMAGE
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    printf '%s' "${REGISTRY_CACHE_IMAGE}"
  )"
  assert_starts_with "REGISTRY_CACHE_IMAGE is the distribution-registry image" \
    "$img" "registry:"
  assert_contains "REGISTRY_CACHE_IMAGE carries a sha256 digest" "$img" "@sha256:"
}

# ---------------------------------------------------------------------------
# Test 3: render_kind_config injects the containerd patch only when enabled
# ---------------------------------------------------------------------------
test_render_kind_config_patch() {
  echo "Test: render_kind_config injects config_path only under WITH_REGISTRY_CACHE=true"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # Flag ON → containerdConfigPatches present with config_path.
  (
    WITH_REGISTRY_CACHE=true
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    render_kind_config "$tmp/on.yaml"
  )
  assert_file_contains "config_path is injected when enabled" \
    "$tmp/on.yaml" 'config_path = "/etc/containerd/certs.d"'
  assert_file_contains "the CRI registry table is injected when enabled" \
    "$tmp/on.yaml" 'io.containerd.grpc.v1.cri'

  # Flag OFF (default) → verbatim copy, no patch, so CI is unchanged.
  (
    unset WITH_REGISTRY_CACHE
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    WITH_REGISTRY_CACHE=false render_kind_config "$tmp/off.yaml"
  )
  if grep -q 'containerdConfigPatches' "$tmp/off.yaml"; then
    echo "  FAIL: render_kind_config injected a patch with the flag off"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: no containerdConfigPatches when the flag is off (CI unchanged)"
    PASS=$((PASS + 1))
  fi
}

# ---------------------------------------------------------------------------
# Test 4: start_registry_cache brings up one labelled proxy per upstream
# ---------------------------------------------------------------------------
test_start_registry_cache() {
  echo "Test: start_registry_cache creates one registry proxy per upstream on the kind network"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_docker_stub "$tmp/bin"

  (
    PATH="$tmp/bin:$PATH"; export PATH
    export DOCKER_LOG="$tmp/docker.log"; : >"$DOCKER_LOG"
    export DOCKER_RUNNING="false"           # nothing running → create all
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    WITH_REGISTRY_CACHE=true start_registry_cache
  ) >/dev/null 2>&1

  local log
  log="$(cat "$tmp/docker.log")"

  # One `docker run` per upstream, each with the pinned image + persistence + label.
  local runs
  runs="$(grep -c 'docker run -d' "$tmp/docker.log")"
  assert_eq "one docker run per upstream (6)" "6" "$runs"

  assert_contains "dockerio proxy is created" "$log" "--name registry-cache-dockerio"
  assert_contains "ghcr proxy is created" "$log" "--name registry-cache-ghcr"
  assert_contains "k8s proxy is created" "$log" "--name registry-cache-k8s"
  assert_contains "quay proxy is created" "$log" "--name registry-cache-quay"
  assert_contains "external-secrets vanity proxy is created" "$log" "--name registry-cache-eso"
  assert_contains "mariadb vanity proxy is created" "$log" "--name registry-cache-mariadb"

  assert_contains "containers join the kind network" "$log" "--network kind"
  assert_contains "containers carry the purge label" "$log" "--label forge.registry-cache=true"
  assert_contains "containers use the pinned registry image" "$log" "registry:"
  assert_contains "containers restart unless stopped" "$log" "--restart unless-stopped"
  assert_contains "storage volume is mounted at the registry data dir" "$log" "registry-cache-dockerio-data:/var/lib/registry"

  # Proxy mode is configured purely via REGISTRY_PROXY_REMOTEURL (no config file).
  assert_contains "docker.io proxy points at the Docker Hub v2 endpoint" \
    "$log" "REGISTRY_PROXY_REMOTEURL=https://registry-1.docker.io"
  assert_contains "quay proxy points at quay.io" \
    "$log" "REGISTRY_PROXY_REMOTEURL=https://quay.io"
  assert_contains "external-secrets proxy points at its vanity host" \
    "$log" "REGISTRY_PROXY_REMOTEURL=https://oci.external-secrets.io"
  assert_contains "mariadb proxy points at its vanity host" \
    "$log" "REGISTRY_PROXY_REMOTEURL=https://docker-registry3.mariadb.com"
  assert_not_contains "no Zot config is mounted anymore" "$log" "/etc/zot/config.json"

  # Persistent volumes are created with the same label so teardown can find them.
  assert_contains "a labelled volume is created" "$log" "docker volume create --label forge.registry-cache=true registry-cache-ghcr-data"
}

# ---------------------------------------------------------------------------
# Test 5: start_registry_cache is idempotent — a running proxy is left alone
# ---------------------------------------------------------------------------
test_start_registry_cache_idempotent() {
  echo "Test: start_registry_cache does not recreate an already-running proxy"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_docker_stub "$tmp/bin"

  (
    PATH="$tmp/bin:$PATH"; export PATH
    export DOCKER_LOG="$tmp/docker.log"; : >"$DOCKER_LOG"
    export DOCKER_RUNNING="true"            # all four already running
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    WITH_REGISTRY_CACHE=true start_registry_cache
  ) >/dev/null 2>&1

  local runs
  runs="$(grep -c 'docker run -d' "$tmp/docker.log" || true)"
  assert_eq "no docker run when every proxy is already running" "0" "$runs"
  # Idempotent path re-attaches to the network instead.
  assert_file_contains "running proxies are (re)connected to the kind network" \
    "$tmp/docker.log" "docker network connect kind registry-cache-dockerio"
}

# ---------------------------------------------------------------------------
# Test 6: start_registry_cache is a no-op when the flag is off
# ---------------------------------------------------------------------------
test_start_registry_cache_noop_when_off() {
  echo "Test: start_registry_cache is a no-op with WITH_REGISTRY_CACHE=false"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_docker_stub "$tmp/bin"

  (
    PATH="$tmp/bin:$PATH"; export PATH
    export DOCKER_LOG="$tmp/docker.log"; : >"$DOCKER_LOG"
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    WITH_REGISTRY_CACHE=false start_registry_cache
  ) >/dev/null 2>&1

  assert_eq "docker is never invoked when the cache is off" "" "$(cat "$tmp/docker.log")"
}

# ---------------------------------------------------------------------------
# Test 7: wire_node_registry_mirror writes a hosts.toml per node/host
# ---------------------------------------------------------------------------
test_wire_node_registry_mirror() {
  echo "Test: wire_node_registry_mirror wires every node's containerd at the caches"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_docker_stub "$tmp/bin"

  (
    PATH="$tmp/bin:$PATH"; export PATH
    export DOCKER_LOG="$tmp/docker.log"; : >"$DOCKER_LOG"
    export KIND_NODES=$'forge-control-plane\nforge-worker'
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    WITH_REGISTRY_CACHE=true wire_node_registry_mirror
  ) >/dev/null 2>&1

  local log
  log="$(cat "$tmp/docker.log")"

  # 2 nodes × 6 upstreams = 12 docker exec calls.
  local execs
  execs="$(grep -c 'docker exec' "$tmp/docker.log" || true)"
  assert_eq "one docker exec per node per upstream (2×6=12)" "12" "$execs"

  # Each exec passes the host/server/mirror as env so the node shell writes the
  # right hosts.toml. Spot-check docker.io, quay, and a vanity front.
  assert_contains "docker.io server fallback is passed" \
    "$log" "HOSTS_SERVER=https://registry-1.docker.io"
  assert_contains "docker.io mirror host is passed" \
    "$log" "HOSTS_MIRROR=http://registry-cache-dockerio:5000"
  assert_contains "control-plane node is targeted" "$log" "docker exec -e HOSTS_HOST=docker.io"
  assert_contains "worker node is targeted for quay" "$log" "HOSTS_MIRROR=http://registry-cache-quay:5000"
  assert_contains "the external-secrets vanity host is wired" \
    "$log" "HOSTS_HOST=oci.external-secrets.io"
  assert_contains "the mariadb vanity host mirror is wired" \
    "$log" "HOSTS_MIRROR=http://registry-cache-mariadb:5000"

  # The inner node script (part of argv) writes the certs.d hosts.toml with the
  # pull/resolve fallback capabilities.
  assert_contains "hosts.toml goes under certs.d" "$log" "/etc/containerd/certs.d"
  assert_contains "mirror advertises pull+resolve (origin fallback)" \
    "$log" 'capabilities = ["pull", "resolve"]'
}

# ---------------------------------------------------------------------------
# Test 8: main() gates both calls behind WITH_REGISTRY_CACHE
# ---------------------------------------------------------------------------
test_main_gates_calls() {
  echo "Test: main() gates start_registry_cache + wire_node_registry_mirror behind the flag"

  # The call sites must be preceded by a WITH_REGISTRY_CACHE==true gate.
  local start_line wire_line gate_line
  start_line="$(grep -n '^[[:space:]]*start_registry_cache$' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  wire_line="$(grep -n '^[[:space:]]*wire_node_registry_mirror$' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  assert_not_empty "start_registry_cache call site found" "$start_line"
  assert_not_empty "wire_node_registry_mirror call site found" "$wire_line"

  gate_line="$(grep -n '"${WITH_REGISTRY_CACHE}" == "true"' "$DEPLOY_INFRA_SH" \
    | awk -F: -v t="${start_line:-0}" '$1 < t { last = $1 } END { print last }')"
  assert_not_empty "a WITH_REGISTRY_CACHE gate precedes the call sites" "$gate_line"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_flag_defaults
test_image_pinned_by_digest
test_render_kind_config_patch
test_start_registry_cache
test_start_registry_cache_idempotent
test_start_registry_cache_noop_when_off
test_wire_node_registry_mirror
test_main_gates_calls

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
