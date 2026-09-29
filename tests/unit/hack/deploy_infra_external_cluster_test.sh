#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the external-cluster mode of hack/deploy-infra.sh (EXTERNAL_CLUSTER=true):
#   1. EXTERNAL_CLUSTER and EXTERNAL_OVERLAY default and pass through verbatim,
#      and the derived OVERLAY_ROOT and PUBLIC_PORT follow the mode.
#   2. preflight_checks in external mode needs only kubectl and jq, still needs
#      yq under WITH_CONTROLPLANE=true, refuses each of the seven kind-bound
#      opt-ins by name, refuses an overlay without its two kustomizations and a
#      context whose API server does not answer; the kind mode still needs
#      docker and kind, EXTERNAL_CLUSTER=yes included.
#   3. check_external_cluster refuses a cluster without a default StorageClass,
#      one with a node-local-dns DaemonSet, one without a Ready node, and one it
#      cannot read, and passes a cluster with a default class, a NotFound
#      DaemonSet and a Ready node, logging both.
#   4. resolve_api_server_egress renders one egress rule per address on the
#      port EndpointSlice default/kubernetes publishes, /128 for IPv6, and
#      aborts on a slice without a port, without an address, or unreadable.
#   5. The script keeps one strict INFRA_ONLY gate, the un-pause patch carries
#      egressRules beside apiServerEndpointIPs and paused:false and follows
#      resolve_api_server_egress, the external Step 1 runs
#      check_external_cluster, Steps 3 and 5 apply the overlay root, the nofile
#      cap and the Keystone preload sit behind an EXTERNAL_CLUSTER gate, the CR
#      rewrite and the by-hand CR hint key on PUBLIC_PORT, and the external
#      banners name the port-forward on PUBLIC_PORT and the teardown.
#
# The script is sourced (its BASH_SOURCE guard keeps main() from running) and
# driven against a kubectl stub scripted through environment variables.
#
# Usage: bash tests/unit/hack/deploy_infra_external_cluster_test.sh

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

# make_kubectl_stub <dir>
# A kubectl that records its argv in $KUBECTL_LOG and answers from the
# environment:
#   KUBECTL_VERSION_RC      exit code of `version` (default 0)
#   KUBECTL_UNREACHABLE     non-empty: every `get` fails with connection refused
#   KUBECTL_STORAGECLASSES  file answering `get storageclass -o json`
#   KUBECTL_DS_PRESENT      true: `get daemonset node-local-dns` succeeds
#   KUBECTL_DS_ERROR        non-empty: that lookup fails with this message
#   KUBECTL_NODES           file answering `get nodes -o json`
#   KUBECTL_SLICE           file answering `get endpointslice kubernetes`
#   KUBECTL_SLICE_RC        exit code of that lookup (default 0)
make_kubectl_stub() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
echo "kubectl $*" >>"${KUBECTL_LOG:-/dev/null}"
case "${1:-}" in
  version)
    if [ "${KUBECTL_VERSION_RC:-0}" != "0" ]; then
      echo "The connection to the server api.lab.example:443 was refused - did you specify the right host or port?" >&2
      exit "${KUBECTL_VERSION_RC}"
    fi
    echo "Client Version: v1.35.0"
    exit 0
    ;;
  config)
    case "${2:-}" in
      current-context) echo "lab-forge" ;;
      view) echo "https://api.lab.example" ;;
    esac
    exit 0
    ;;
  get)
    if [ -n "${KUBECTL_UNREACHABLE:-}" ]; then
      echo "The connection to the server api.lab.example:443 was refused - did you specify the right host or port?" >&2
      exit 1
    fi
    case "${2:-}" in
      storageclass) cat "${KUBECTL_STORAGECLASSES}" ;;
      daemonset)
        if [ -n "${KUBECTL_DS_ERROR:-}" ]; then echo "${KUBECTL_DS_ERROR}" >&2; exit 1; fi
        if [ "${KUBECTL_DS_PRESENT:-false}" = "true" ]; then echo "node-local-dns   2   2   2"; exit 0; fi
        echo 'Error from server (NotFound): daemonsets.apps "node-local-dns" not found' >&2
        exit 1
        ;;
      nodes) cat "${KUBECTL_NODES}" ;;
      endpointslice)
        if [ "${KUBECTL_SLICE_RC:-0}" != "0" ]; then
          echo 'Error from server (Forbidden): endpointslices.discovery.k8s.io "kubernetes" is forbidden' >&2
          exit "${KUBECTL_SLICE_RC}"
        fi
        cat "${KUBECTL_SLICE}"
        ;;
    esac
    ;;
esac
exit 0
STUB
  chmod +x "$dir/kubectl"
}

# make_stub_path <dir> <cmd>...
# A private PATH: an exit-0 shim for each <cmd>, plus the two utilities the
# sourced script needs itself (dirname for SCRIPT_DIR, date for log). A
# kubectl shim is the scripted stub above.
make_stub_path() {
  local dir="$1" cmd
  shift
  mkdir -p "$dir"
  ln -s "$(command -v dirname)" "$dir/dirname"
  ln -s "$(command -v date)" "$dir/date"
  for cmd in "$@"; do
    if [ "$cmd" = "kubectl" ]; then
      make_kubectl_stub "$dir"
      continue
    fi
    printf '#!/bin/bash\nexit 0\n' >"$dir/$cmd"
    chmod +x "$dir/$cmd"
  done
}

# resolve <expression> [env_var=value...]
# Sources deploy-infra.sh with the given overrides and echoes <expression>.
resolve() {
  local expression="$1"
  shift
  (
    unset EXTERNAL_CLUSTER EXTERNAL_OVERLAY KIND_HOST_PORT
    for assignment in "$@"; do
      export "${assignment?}"
    done
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    eval "printf '%s' \"${expression}\""
  )
}

# run_preflight <path> [env_var=value...]
# Sources deploy-infra.sh with PATH=<path> and the given overrides and runs
# preflight_checks. Echoes combined output; returns its exit status.
run_preflight() {
  local path="$1"
  shift
  (
    unset EXTERNAL_CLUSTER EXTERNAL_OVERLAY WITH_CONTROLPLANE WITH_VPA \
      WITH_METRICS_SERVER WITH_REGISTRY_CACHE WITH_CHAOS_MESH \
      WITH_OVN_KERNEL_MODULES WITH_NFS WITH_DIZZY
    for assignment in "$@"; do
      export "${assignment?}"
    done
    PATH="$path"
    export PATH
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    preflight_checks
  ) 2>&1
}

# run_fn <stub_dir> <function> [env_var=value...]
# Sources deploy-infra.sh with the kubectl stub first on the real PATH (jq is
# the real one) and runs <function>. Echoes combined output; returns its status.
run_fn() {
  local stub_dir="$1" fn="$2"
  shift 2
  (
    for assignment in "$@"; do
      export "${assignment?}"
    done
    PATH="$stub_dir:$PATH"
    export PATH
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    "$fn"
    if [ "$fn" = "resolve_api_server_egress" ]; then
      echo "IPS=${API_SERVER_ENDPOINT_IPS}"
      echo "PORT=${API_SERVER_PORT}"
      echo "RULES=${API_SERVER_EGRESS_RULES}"
    fi
  ) 2>&1
}

# line_of <fixed string> — the first line of deploy-infra.sh containing it.
line_of() {
  grep -nF -- "$1" "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1
}

# assert_before <description> <line a> <line b> — a is a line before b.
assert_before() {
  if [ -n "$2" ] && [ -n "$3" ] && [ "$2" -lt "$3" ]; then
    echo "  PASS: $1"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $1 (line '${2}' is not before line '${3}')"
    FAIL=$((FAIL + 1))
  fi
}

STORAGECLASSES_WITH_DEFAULT='{"items":[
  {"metadata":{"name":"premium","annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}},
  {"metadata":{"name":"standard"}}]}'
STORAGECLASSES_WITHOUT_DEFAULT='{"items":[
  {"metadata":{"name":"premium","annotations":{"storageclass.kubernetes.io/is-default-class":"false"}}},
  {"metadata":{"name":"standard"}}]}'
NODES_READY='{"items":[
  {"metadata":{"name":"shoot-worker-a"},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
  {"metadata":{"name":"shoot-worker-b"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}'
NODES_NOT_READY='{"items":[
  {"metadata":{"name":"shoot-worker-a"},"status":{"conditions":[{"type":"Ready","status":"False"}]}}]}'

# ---------------------------------------------------------------------------
# Test 1: the knobs and the derived variables
# ---------------------------------------------------------------------------
test_knobs_and_derived_variables() {
  echo "Test: EXTERNAL_CLUSTER, EXTERNAL_OVERLAY, OVERLAY_ROOT and PUBLIC_PORT"

  assert_eq "EXTERNAL_CLUSTER defaults to false" "false" "$(resolve '${EXTERNAL_CLUSTER}')"
  assert_eq "EXTERNAL_OVERLAY defaults to deploy/lab/metal-stack" "deploy/lab/metal-stack" \
    "$(resolve '${EXTERNAL_OVERLAY}')"
  assert_eq "EXTERNAL_CLUSTER=true is preserved" "true" \
    "$(resolve '${EXTERNAL_CLUSTER}' EXTERNAL_CLUSTER=true)"
  assert_eq "EXTERNAL_CLUSTER=yes is preserved verbatim" "yes" \
    "$(resolve '${EXTERNAL_CLUSTER}' EXTERNAL_CLUSTER=yes)"
  assert_eq "EXTERNAL_OVERLAY passes through verbatim" "deploy/lab/other" \
    "$(resolve '${EXTERNAL_OVERLAY}' EXTERNAL_OVERLAY=deploy/lab/other)"

  assert_eq "OVERLAY_ROOT is deploy/kind by default" "$PROJECT_ROOT/deploy/kind" \
    "$(resolve '${OVERLAY_ROOT}')"
  assert_eq "OVERLAY_ROOT is the lab overlay under EXTERNAL_CLUSTER=true" \
    "$PROJECT_ROOT/deploy/lab/metal-stack" "$(resolve '${OVERLAY_ROOT}' EXTERNAL_CLUSTER=true)"
  assert_eq "an absolute EXTERNAL_OVERLAY is used unchanged" "/srv/overlays/lab" \
    "$(resolve '${OVERLAY_ROOT}' EXTERNAL_CLUSTER=true EXTERNAL_OVERLAY=/srv/overlays/lab)"
  assert_eq "EXTERNAL_OVERLAY is ignored in kind mode" "$PROJECT_ROOT/deploy/kind" \
    "$(resolve '${OVERLAY_ROOT}' EXTERNAL_OVERLAY=/srv/overlays/lab)"
  assert_eq "EXTERNAL_CLUSTER=yes keeps the kind overlay" "$PROJECT_ROOT/deploy/kind" \
    "$(resolve '${OVERLAY_ROOT}' EXTERNAL_CLUSTER=yes)"

  assert_eq "PUBLIC_PORT is 443 by default" "443" "$(resolve '${PUBLIC_PORT}')"
  assert_eq "PUBLIC_PORT follows KIND_HOST_PORT in kind mode" "8443" \
    "$(resolve '${PUBLIC_PORT}' KIND_HOST_PORT=8443)"
  assert_eq "PUBLIC_PORT is 8443 in external mode whatever KIND_HOST_PORT says" "8443" \
    "$(resolve '${PUBLIC_PORT}' EXTERNAL_CLUSTER=true KIND_HOST_PORT=9443)"
}

# ---------------------------------------------------------------------------
# Test 2: preflight in external mode
# ---------------------------------------------------------------------------
test_external_preflight() {
  echo "Test: preflight_checks under EXTERNAL_CLUSTER=true"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_stub_path "$tmp/kubectl-jq" kubectl jq
  output="$(run_preflight "$tmp/kubectl-jq" EXTERNAL_CLUSTER=true)"
  rc=$?
  assert_eq "passes with only kubectl and jq on PATH" "0" "$rc"
  assert_contains "reports success" "$output" "Pre-flight checks passed."
  assert_contains "logs the kubeconfig context" "$output" "Kubeconfig context  : lab-forge"
  assert_contains "logs the API server URL" "$output" "API server          : https://api.lab.example"
  assert_not_contains "never asks for docker" "$output" "'docker' is not installed"

  make_stub_path "$tmp/jq-only" jq
  output="$(run_preflight "$tmp/jq-only" EXTERNAL_CLUSTER=true)"
  rc=$?
  assert_nonzero_exit "fails without kubectl" "$rc"
  assert_contains "names kubectl as missing" "$output" "'kubectl' is not installed"

  output="$(run_preflight "$tmp/kubectl-jq" EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true)"
  rc=$?
  assert_nonzero_exit "fails without yq under WITH_CONTROLPLANE=true" "$rc"
  assert_contains "names yq as required" "$output" "requires 'yq'"

  output="$(run_preflight "$tmp/kubectl-jq" EXTERNAL_CLUSTER=true KUBECTL_VERSION_RC=1)"
  rc=$?
  assert_nonzero_exit "fails when the API server does not answer" "$rc"
  assert_contains "quotes kubectl's error" "$output" "api.lab.example:443 was refused"

  output="$(run_preflight "$tmp/kubectl-jq" EXTERNAL_CLUSTER=true EXTERNAL_OVERLAY=/nonexistent)"
  rc=$?
  assert_nonzero_exit "fails on an overlay without its kustomizations" "$rc"
  assert_contains "names EXTERNAL_OVERLAY" "$output" "EXTERNAL_OVERLAY='/nonexistent' has no base/ and infrastructure/ kustomization"

  mkdir -p "$tmp/half-overlay/base"
  : >"$tmp/half-overlay/base/kustomization.yaml"
  output="$(run_preflight "$tmp/kubectl-jq" EXTERNAL_CLUSTER=true EXTERNAL_OVERLAY="$tmp/half-overlay")"
  rc=$?
  assert_nonzero_exit "fails on an overlay with base/ alone" "$rc"
  assert_contains "names EXTERNAL_OVERLAY for the half overlay" "$output" "EXTERNAL_OVERLAY='$tmp/half-overlay'"
}

# ---------------------------------------------------------------------------
# Test 3: the refused kind-bound opt-ins, each named in its message
# ---------------------------------------------------------------------------
test_refused_flags() {
  echo "Test: preflight_checks under EXTERNAL_CLUSTER=true refuses each kind-bound opt-in by name"

  local tmp output rc flag
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stub_path "$tmp/bin" kubectl jq
  export KUBECTL_LOG="$tmp/kubectl.log"

  # WITH_VPA=true folds into WITH_METRICS_SERVER=true at the top of the script,
  # so it has to be checked first to be named at all.
  for flag in WITH_VPA WITH_METRICS_SERVER WITH_REGISTRY_CACHE WITH_CHAOS_MESH \
    WITH_OVN_KERNEL_MODULES WITH_NFS WITH_DIZZY; do
    : >"$KUBECTL_LOG"
    output="$(run_preflight "$tmp/bin" EXTERNAL_CLUSTER=true "${flag}=true")"
    rc=$?
    assert_nonzero_exit "${flag}=true is refused" "$rc"
    assert_contains "the refusal names ${flag}" "$output" \
      "ERROR: EXTERNAL_CLUSTER=true does not support ${flag}=true:"
    assert_eq "${flag}=true is refused before the cluster is contacted" "" "$(cat "$KUBECTL_LOG")"
  done
  unset KUBECTL_LOG

  # The opt-ins that are plain manifests stay allowed.
  output="$(run_preflight "$tmp/bin" EXTERNAL_CLUSTER=true WITH_PROMETHEUS=true WITH_MESSAGING=true INFRA_ONLY=true)"
  rc=$?
  assert_eq "WITH_PROMETHEUS, WITH_MESSAGING and INFRA_ONLY stay allowed" "0" "$rc"
}

# ---------------------------------------------------------------------------
# Test 4: the kind mode still needs docker and kind
# ---------------------------------------------------------------------------
test_kind_preflight_unchanged() {
  echo "Test: preflight_checks in kind mode still requires docker and kind"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_stub_path "$tmp/no-docker" kind kubectl jq
  output="$(run_preflight "$tmp/no-docker")"
  rc=$?
  assert_nonzero_exit "the kind mode fails without docker" "$rc"
  assert_contains "names docker as missing" "$output" "'docker' is not installed"

  make_stub_path "$tmp/no-kind" docker kubectl jq
  output="$(run_preflight "$tmp/no-kind" EXTERNAL_CLUSTER=yes)"
  rc=$?
  assert_nonzero_exit "EXTERNAL_CLUSTER=yes keeps the kind mode and fails without kind" "$rc"
  assert_contains "names kind as missing" "$output" "'kind' is not installed"
}

# ---------------------------------------------------------------------------
# Test 5: check_external_cluster
# ---------------------------------------------------------------------------
test_check_external_cluster() {
  echo "Test: check_external_cluster"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_kubectl_stub "$tmp/bin"
  printf '%s\n' "$STORAGECLASSES_WITH_DEFAULT" >"$tmp/sc-default.json"
  printf '%s\n' "$STORAGECLASSES_WITHOUT_DEFAULT" >"$tmp/sc-none.json"
  printf '%s\n' "$NODES_READY" >"$tmp/nodes-ready.json"
  printf '%s\n' "$NODES_NOT_READY" >"$tmp/nodes-not-ready.json"

  output="$(run_fn "$tmp/bin" check_external_cluster \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_eq "passes a cluster with a default class, no node-local-dns and Ready nodes" "0" "$rc"
  assert_contains "logs the default class" "$output" "Default StorageClass: premium"
  assert_contains "logs the Ready node names" "$output" "shoot-worker-a shoot-worker-b"

  output="$(run_fn "$tmp/bin" check_external_cluster \
    KUBECTL_STORAGECLASSES="$tmp/sc-none.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_nonzero_exit "refuses a cluster without a default StorageClass" "$rc"
  assert_contains "names the missing default class" "$output" "no default StorageClass"

  output="$(run_fn "$tmp/bin" check_external_cluster KUBECTL_DS_PRESENT=true \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_nonzero_exit "refuses a cluster with a node-local-dns DaemonSet" "$rc"
  assert_contains "names spec.network.dnsEndpointIPs" "$output" "spec.network.dnsEndpointIPs"

  output="$(run_fn "$tmp/bin" check_external_cluster \
    KUBECTL_DS_ERROR="Unable to connect to the server: net/http: TLS handshake timeout" \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_nonzero_exit "refuses when the DaemonSet lookup fails with anything but not found" "$rc"
  assert_contains "quotes the lookup's error" "$output" "TLS handshake timeout"

  output="$(run_fn "$tmp/bin" check_external_cluster KUBECTL_UNREACHABLE=1)"
  rc=$?
  assert_nonzero_exit "refuses a cluster it cannot read" "$rc"
  assert_contains "reports the failed StorageClass read" "$output" "cannot list the cluster's StorageClasses"

  output="$(run_fn "$tmp/bin" check_external_cluster \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-not-ready.json")"
  rc=$?
  assert_nonzero_exit "refuses a cluster without a Ready node" "$rc"
  assert_contains "names the missing Ready node" "$output" "no Ready node"
}

# ---------------------------------------------------------------------------
# Test 6: resolve_api_server_egress
# ---------------------------------------------------------------------------
test_resolve_api_server_egress() {
  echo "Test: resolve_api_server_egress renders the egress rules on the slice's port"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_kubectl_stub "$tmp/bin"

  printf '%s\n' '{"endpoints":[{"addresses":["240.248.220.69"]}],"ports":[{"name":"https","port":443,"protocol":"TCP"}]}' \
    >"$tmp/gardener.json"
  output="$(run_fn "$tmp/bin" resolve_api_server_egress KUBECTL_SLICE="$tmp/gardener.json")"
  rc=$?
  assert_eq "resolves a Gardener-shaped slice" "0" "$rc"
  assert_contains "keeps the address list" "$output" 'IPS=["240.248.220.69"]'
  assert_contains "reads the published port" "$output" "PORT=443"
  assert_contains "renders one /32 rule on that port" "$output" \
    'RULES=[{"to":[{"ipBlock":{"cidr":"240.248.220.69/32"}}],"ports":[{"protocol":"TCP","port":443}]}]'

  printf '%s\n' '{"endpoints":[{"addresses":["fd00::1"]}],"ports":[{"name":"https","port":6443,"protocol":"TCP"}]}' \
    >"$tmp/ipv6.json"
  output="$(run_fn "$tmp/bin" resolve_api_server_egress KUBECTL_SLICE="$tmp/ipv6.json")"
  assert_contains "an IPv6 address gets a /128 block" "$output" '"cidr":"fd00::1/128"'

  printf '%s\n' '{"endpoints":[{"addresses":["172.18.0.2"]}]}' >"$tmp/no-port.json"
  output="$(run_fn "$tmp/bin" resolve_api_server_egress KUBECTL_SLICE="$tmp/no-port.json")"
  rc=$?
  assert_nonzero_exit "aborts on a slice without a port" "$rc"
  assert_contains "says the slice carries no port" "$output" \
    "EndpointSlice default/kubernetes carries no port"

  printf '%s\n' '{"endpoints":[{"addresses":["172.18.0.2"]}],"ports":[{"name":"https","protocol":"TCP"}]}' \
    >"$tmp/unnumbered-port.json"
  output="$(run_fn "$tmp/bin" resolve_api_server_egress KUBECTL_SLICE="$tmp/unnumbered-port.json")"
  rc=$?
  assert_nonzero_exit "aborts on a slice whose only port carries no number" "$rc"
  assert_contains "says that slice carries no port" "$output" \
    "EndpointSlice default/kubernetes carries no port"

  printf '%s\n' '{"endpoints":[{"addresses":["172.18.0.2"]}],"ports":[{"name":"none"},{"name":"https","port":443,"protocol":"TCP"}]}' \
    >"$tmp/second-port.json"
  output="$(run_fn "$tmp/bin" resolve_api_server_egress KUBECTL_SLICE="$tmp/second-port.json")"
  rc=$?
  assert_eq "skips a port entry without a number" "0" "$rc"
  assert_contains "and takes the first numbered port" "$output" "PORT=443"

  printf '%s\n' '{"endpoints":[],"ports":[{"name":"https","port":6443,"protocol":"TCP"}]}' >"$tmp/empty.json"
  output="$(run_fn "$tmp/bin" resolve_api_server_egress KUBECTL_SLICE="$tmp/empty.json")"
  rc=$?
  assert_nonzero_exit "aborts on a slice without an address" "$rc"
  assert_contains "says there is no API server address" "$output" \
    "no API server address in EndpointSlice default/kubernetes"

  output="$(run_fn "$tmp/bin" resolve_api_server_egress KUBECTL_SLICE_RC=1)"
  rc=$?
  assert_nonzero_exit "aborts on an unreadable slice" "$rc"
  assert_contains "prints kubectl's error" "$output" "is forbidden"
  assert_contains "with the no-address explanation" "$output" \
    "no API server address in EndpointSlice default/kubernetes"
}

# ---------------------------------------------------------------------------
# Test 7: the gates and lines the mode adds to main()
# ---------------------------------------------------------------------------
test_main_gates() {
  echo "Test: main() gates the kind-only steps and names the port-forward"

  local gate_count
  gate_count="$(grep -cE '"\$\{INFRA_ONLY\}" == "true"' "$DEPLOY_INFRA_SH" || true)"
  assert_eq "deploy-infra.sh still has exactly 1 strict INFRA_ONLY==true gate" "1" "$gate_count"

  # Backslashes are stripped so the check holds for either quoting style.
  local unpause_patch
  unpause_patch="$(grep -A2 'kubectl patch openbaocluster openbao-instance' "$DEPLOY_INFRA_SH" |
    tr -d '\\' | tr '\n' ' ')"
  assert_contains "the un-pause patch carries egressRules" "$unpause_patch" '"egressRules":'
  assert_contains "the un-pause patch carries apiServerEndpointIPs" "$unpause_patch" '"apiServerEndpointIPs":'
  assert_contains "the un-pause patch clears spec.paused" "$unpause_patch" '"paused":false'
  assert_before "resolve_api_server_egress runs before the un-pause patch" \
    "$(grep -nE '^[[:space:]]+resolve_api_server_egress$' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)" \
    "$(line_of 'kubectl patch openbaocluster openbao-instance')"

  assert_contains "the external Step 1 branch runs check_external_cluster" \
    "$(grep -A3 -F 'Step 1/8: External cluster (EXTERNAL_CLUSTER=true)' "$DEPLOY_INFRA_SH")" \
    '    check_external_cluster'
  assert_file_contains_fixed "Step 3 applies the overlay root's base" \
    "$DEPLOY_INFRA_SH" 'kubectl apply -k "${OVERLAY_ROOT}/base"'
  assert_file_not_contains "Step 3 no longer hardcodes the kind base" \
    "$DEPLOY_INFRA_SH" 'kubectl apply -k "${REPO_ROOT}/deploy/kind/base"'
  assert_file_contains_fixed "the WITH_CONTROLPLANE render of Step 5 reads the overlay root" \
    "$DEPLOY_INFRA_SH" 'kubectl kustomize "${OVERLAY_ROOT}/infrastructure"'

  assert_contains "the Keystone preload sits behind an EXTERNAL_CLUSTER gate" \
    "$(grep -B3 -F 'docker pull "ghcr.io/c5c3/keystone:' "$DEPLOY_INFRA_SH")" \
    'if [[ "${EXTERNAL_CLUSTER}" != "true" ]]; then'
  assert_contains "the nofile cap sits behind an EXTERNAL_CLUSTER gate" \
    "$(grep -B1 -E '^    cap_node_nofile$' "$DEPLOY_INFRA_SH")" \
    'if [[ "${EXTERNAL_CLUSTER}" != "true" ]]; then'
  assert_before "the external Step 1 branch precedes the SKIP_KIND_CREATE branch" \
    "$(line_of 'Step 1/8: External cluster (EXTERNAL_CLUSTER=true)')" \
    "$(line_of '"${SKIP_KIND_CREATE:-false}" == "true"')"

  assert_file_contains_fixed "the CR rewrite reads strenv(PUBLIC_PORT)" \
    "$DEPLOY_INFRA_SH" 'strenv(PUBLIC_PORT)'
  assert_file_not_contains "the CR rewrite no longer reads strenv(KIND_HOST_PORT)" \
    "$DEPLOY_INFRA_SH" 'strenv(KIND_HOST_PORT)'
  assert_file_contains_fixed "the CR rewrite is gated on PUBLIC_PORT" \
    "$DEPLOY_INFRA_SH" 'if [[ "${PUBLIC_PORT}" != "443" ]]; then'
  local hint
  hint="$(grep -B5 -F 'Or re-run with WITH_CONTROLPLANE_CR=true' "$DEPLOY_INFRA_SH")"
  assert_contains "the by-hand CR hint is gated on PUBLIC_PORT" "$hint" \
    'if [[ "${PUBLIC_PORT}" != "443" ]]; then'
  assert_contains "the by-hand CR hint names the URL on the public port" "$hint" \
    'https://keystone.127-0-0-1.nip.io:${PUBLIC_PORT}/v3'
  assert_not_contains "the by-hand CR hint no longer keys on KIND_HOST_PORT" "$hint" "KIND_HOST_PORT"

  local banner
  banner="$(awk '/Infrastructure deployment complete!/,/^}/' "$DEPLOY_INFRA_SH")"
  assert_contains "the completion banner names the port-forward" "$banner" "port-forward"
  assert_contains "the completion banner forwards PUBLIC_PORT to 443" "$banner" '${PUBLIC_PORT}:443'
  assert_not_contains "the completion banner repeats no literal 8443" "$banner" "8443"
  assert_contains "the completion banner resolves the Envoy Service by its Gateway label" \
    "$banner" "gateway.envoyproxy.io/owning-gateway-name=openstack-gw"
  assert_contains "the completion banner names the external teardown" \
    "$banner" "EXTERNAL_CLUSTER=true make teardown-infra"
  assert_file_contains_fixed "the run banner reports the cluster mode" \
    "$DEPLOY_INFRA_SH" 'Cluster mode        : external (EXTERNAL_CLUSTER=true; overlay ${OVERLAY_ROOT}; port-forward on ${PUBLIC_PORT})'
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_knobs_and_derived_variables
test_external_preflight
test_refused_flags
test_kind_preflight_unchanged
test_check_external_cluster
test_resolve_api_server_egress
test_main_gates

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
