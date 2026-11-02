#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify how hack/deploy-infra.sh publishes the Gateway openstack-gw in the
# external-cluster mode (EXTERNAL_CLUSTER=true):
#   1. EXTERNAL_LOADBALANCER defaults to true, PUBLIC_SOURCE_RANGES to empty,
#      and GATEWAY_CERTIFICATE to openstack/openstack-gw-tls.
#   2. split_ranges splits at commas and blanks without globbing, and
#      valid_ipv4_cidr takes a.b.c.d/n with octets up to 255 and n up to 32.
#   3. preflight refuses PUBLIC_SOURCE_RANGES beside EXTERNAL_LOADBALANCER other
#      than true, an entry that is no IPv4 CIDR, the knob in kind mode, and an
#      overlay without infrastructure/gateway-certificate.yaml; it needs yq
#      for EXTERNAL_LOADBALANCER other than true and for PUBLIC_SOURCE_RANGES,
#      and not for the default.
#   4. resolve_public_source_ranges appends the pod and node networks of
#      ConfigMap kube-system/shoot-info to PUBLIC_SOURCE_RANGES, each once,
#      and refuses an absent or unreadable ConfigMap, a missing network and a
#      network that is no IPv4 CIDR.
#   5. render_envoy_proxy, on the lab's infrastructure render, leaves every
#      document unchanged for the default, writes the resolved ranges under
#      PUBLIC_SOURCE_RANGES, makes envoy-public a ClusterIP Service without
#      externalTrafficPolicy under EXTERNAL_LOADBALANCER=false, passes the
#      kind mode through, and changes no other document.
#   6. configure_public_gateway issues the certificate for 127-0-0-1.nip.io,
#      waits for the Service's address and issues it again for
#      <a-b-c-d>.nip.io; on a re-run whose Service already has the address it
#      applies once; it waits for Ready at the current generation, moves an
#      existing dizzy route onto the suffix and leaves an absent one alone;
#      it aborts without an address in time, naming EXTERNAL_LOADBALANCER=false,
#      and on a hostname in place of an address; under
#      EXTERNAL_LOADBALANCER=false it issues for 127-0-0-1.nip.io and reads no
#      Service.
#   7. main() wires it: both Step 5 applies pipe through render_envoy_proxy,
#      configure_public_gateway runs behind an EXTERNAL_CLUSTER gate after
#      them and before the Gateway wait, and check_external_cluster calls
#      resolve_public_source_ranges behind a PUBLIC_SOURCE_RANGES gate.
#
# The script is sourced (its BASH_SOURCE guard keeps main() from running) and
# driven against a kubectl stub that keeps its state in files. Check 5 is
# counted as SKIP when kustomize or yq is not on PATH.
#
# Usage: bash tests/unit/hack/deploy_infra_public_gateway_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"
LAB_INFRA="$PROJECT_ROOT/deploy/lab/metal-stack/infrastructure"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP="$(mktemp -d "${TMPDIR:-/tmp}/public-gateway-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

# ---------------------------------------------------------------------------
# The kubectl stub. Every call is appended to $STUB/calls. Scripted through:
#   $STUB/address-after  polls of the Service before it has an address (0)
#   $STUB/address        the address it has then (empty: none, ever)
#   $STUB/cert.json      the Certificate `get certificate -o json` prints
#   $STUB/dizzy-route    non-empty: the HTTPRoute dizzy/dizzy-grafana exists
#   $STUB/shoot-info     the ConfigMap's JSON; absent: --ignore-not-found
#                        prints nothing
#   $STUB/shoot-info-rc  non-zero: that read fails
# Each `apply -f -` stores its stdin as $STUB/apply-<n>.yaml.
# ---------------------------------------------------------------------------
make_stub() {
  STUB="$1"
  mkdir -p "$STUB/bin"
  cat >"$STUB/bin/kubectl" <<'EOF'
#!/bin/bash
echo "kubectl $*" >>"$STUB/calls"
case "$*" in
  "-n envoy-gateway-system get svc"*jsonpath*)
    polls=$(( $(cat "$STUB/polls" 2>/dev/null || echo 0) + 1 ))
    echo "$polls" >"$STUB/polls"
    if [ "$polls" -gt "$(cat "$STUB/address-after" 2>/dev/null || echo 0)" ]; then
      cat "$STUB/address" 2>/dev/null
    fi
    ;;
  "-n envoy-gateway-system get"*) ;;
  "apply -f - -o jsonpath"*)
    n=$(( $(ls "$STUB"/apply-*.yaml 2>/dev/null | wc -l) + 1 ))
    cat >"$STUB/apply-$n.yaml"
    printf 'openstack/openstack-gw-tls'
    ;;
  "get certificate"*"-o json"*) cat "$STUB/cert.json" ;;
  "get certificate"*) printf 'openstack-gw-tls' ;;
  "-n dizzy get httproute"*) cat "$STUB/dizzy-route" 2>/dev/null ;;
  "get configmap shoot-info"*)
    if [ "$(cat "$STUB/shoot-info-rc" 2>/dev/null || echo 0)" != 0 ]; then
      echo 'Error from server (Forbidden): configmaps "shoot-info" is forbidden' >&2
      exit 1
    fi
    cat "$STUB/shoot-info" 2>/dev/null
    ;;
esac
exit 0
EOF
  chmod +x "$STUB/bin/kubectl"
  printf '#!/bin/bash\nexit 0\n' >"$STUB/bin/sleep"
  chmod +x "$STUB/bin/sleep"
}

# cert_json <generation> <observedGeneration of Ready=True>
cert_json() {
  printf '{"metadata":{"generation":%s},"status":{"conditions":[{"type":"Ready","status":"True","observedGeneration":%s}]}}' "$1" "$2"
}

# run_fn <stub> <command> [VAR=value...] — source deploy-infra.sh in a subshell
# with the stub first on PATH and the lab overlay as OVERLAY_ROOT, set the
# variables, run <command> (eval'd) and print its output and PUBLIC_SUFFIX.
run_fn() {
  local stub="$1" command="$2"
  shift 2
  (
    export STUB="$stub"
    PATH="$stub/bin:$PATH"
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    # shellcheck disable=SC2034 # read by the sourced script's functions
    EXTERNAL_CLUSTER=true OVERLAY_ROOT="$PROJECT_ROOT/deploy/lab/metal-stack" POD_TIMEOUT=60
    local assignment
    for assignment in "$@"; do
      eval "${assignment%%=*}=\"\${assignment#*=}\""
    done
    eval "$command"
    echo "PUBLIC_SUFFIX=${PUBLIC_SUFFIX}"
  ) 2>&1
}

# run_preflight <path> [VAR=value...] — preflight_checks under PATH=<path>.
run_preflight() {
  local path="$1"
  shift
  (
    unset EXTERNAL_CLUSTER EXTERNAL_OVERLAY EXTERNAL_LOADBALANCER PUBLIC_SOURCE_RANGES \
      WITH_CONTROLPLANE WITH_CONTROLPLANE_CR WITH_NFS WITH_CEPH WITH_DIZZY \
      WITH_CHAOS_MESH WITH_PROMETHEUS WITH_VPA WITH_METRICS_SERVER
    local assignment
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

# stub_path <dir> <with yq: true|false> — kubectl that answers `version`, the
# real jq, optionally the real yq, and what sourcing the script needs.
stub_path() {
  local dir="$1" cmd
  mkdir -p "$dir"
  for cmd in dirname date jq tr grep; do
    ln -sf "$(command -v "$cmd")" "$dir/$cmd"
  done
  if [[ "$2" == true ]] && command -v yq >/dev/null 2>&1; then
    ln -sf "$(command -v yq)" "$dir/yq"
  fi
  cat >"$dir/kubectl" <<'EOF'
#!/bin/bash
case "${1:-}" in
  config) [ "${2:-}" = current-context ] && echo lab ;;
esac
exit 0
EOF
  chmod +x "$dir/kubectl"
  printf '#!/bin/bash\nexit 0\n' >"$dir/docker"
  printf '#!/bin/bash\nexit 0\n' >"$dir/kind"
  chmod +x "$dir/docker" "$dir/kind"
}

# ---------------------------------------------------------------------------
# Test 1: the knobs
# ---------------------------------------------------------------------------
test_knobs() {
  echo "Test: the defaults of the Gateway knobs"
  local out
  out="$(unset EXTERNAL_LOADBALANCER PUBLIC_SOURCE_RANGES
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH" 2>/dev/null
    printf '%s|%s|%s' "$EXTERNAL_LOADBALANCER" "$PUBLIC_SOURCE_RANGES" "$GATEWAY_CERTIFICATE")"
  assert_eq "EXTERNAL_LOADBALANCER is true, PUBLIC_SOURCE_RANGES empty, the certificate openstack/openstack-gw-tls" \
    "true||openstack/openstack-gw-tls" "$out"
  out="$(export EXTERNAL_LOADBALANCER=false PUBLIC_SOURCE_RANGES=10.0.0.0/8
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH" 2>/dev/null
    printf '%s|%s' "$EXTERNAL_LOADBALANCER" "$PUBLIC_SOURCE_RANGES")"
  assert_eq "both pass through verbatim" "false|10.0.0.0/8" "$out"
}

# ---------------------------------------------------------------------------
# Test 2: split_ranges and valid_ipv4_cidr
# ---------------------------------------------------------------------------
test_split_and_validate() {
  echo "Test: split_ranges and valid_ipv4_cidr"
  local out
  # A file in the working directory, which a glob of * would expand to.
  touch "$TMP/match-me"
  # shellcheck source=/dev/null
  out="$(cd "$TMP" && source "$DEPLOY_INFRA_SH" 2>/dev/null &&
    split_ranges $'1.2.3.4/32, 5.6.7.0/24,,*  9.0.0.0/8\t10.0.0.0/8' | tr '\n' ' ')"
  assert_eq "commas, blanks and tabs separate; empty entries drop; * is not globbed" \
    "1.2.3.4/32 5.6.7.0/24 * 9.0.0.0/8 10.0.0.0/8 " "$out"
  # shellcheck source=/dev/null
  out="$(source "$DEPLOY_INFRA_SH" 2>/dev/null; split_ranges "" | wc -l | tr -d ' ')"
  assert_eq "an empty list yields no line" "0" "$out"

  local cidr verdicts=""
  for cidr in 92.209.233.207/32 0.0.0.0/0 255.255.255.255/32 10.240.0.0/13 \
    1.2.3.4 256.0.0.0/8 1.2.3.4/33 1.2.3/24 a.b.c.d/8 '1.2.3.4/32 ' '*'; do
    # shellcheck source=/dev/null
    if (source "$DEPLOY_INFRA_SH" 2>/dev/null; valid_ipv4_cidr "$cidr"); then
      verdicts+="+"
    else
      verdicts+="-"
    fi
  done
  assert_eq "four CIDRs pass, seven malformed entries fail" "++++-------" "$verdicts"
}

# ---------------------------------------------------------------------------
# Test 3: preflight
# ---------------------------------------------------------------------------
test_preflight() {
  echo "Test: preflight of the Gateway knobs"
  local with_yq="$TMP/path-yq" without_yq="$TMP/path-no-yq" out rc
  stub_path "$with_yq" true
  stub_path "$without_yq" false

  out="$(run_preflight "$without_yq" EXTERNAL_CLUSTER=true)"
  rc=$?
  assert_eq "the default passes without yq" "0" "$rc"
  assert_contains "and reaches the end of preflight" "$out" "Pre-flight checks passed."

  out="$(run_preflight "$without_yq" EXTERNAL_CLUSTER=true EXTERNAL_LOADBALANCER=false)"
  rc=$?
  assert_eq "EXTERNAL_LOADBALANCER=false fails without yq" "1" "$rc"
  assert_contains "naming yq" "$out" "need 'yq' on PATH"
  out="$(run_preflight "$without_yq" EXTERNAL_CLUSTER=true PUBLIC_SOURCE_RANGES=10.0.0.0/8)"
  assert_contains "PUBLIC_SOURCE_RANGES fails without yq as well" "$out" "need 'yq' on PATH"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (6 checks skipped)"
    SKIP=$((SKIP + 6))
  else
    out="$(run_preflight "$with_yq" EXTERNAL_CLUSTER=true EXTERNAL_LOADBALANCER=false)"
    assert_contains "EXTERNAL_LOADBALANCER=false passes with yq" "$out" "Pre-flight checks passed."
    out="$(run_preflight "$with_yq" EXTERNAL_CLUSTER=true 'PUBLIC_SOURCE_RANGES=92.209.233.207/32, 203.0.113.0/24')"
    assert_contains "two ranges separated by a comma and a blank pass" "$out" "Pre-flight checks passed."
    out="$(run_preflight "$with_yq" EXTERNAL_CLUSTER=true EXTERNAL_LOADBALANCER=false PUBLIC_SOURCE_RANGES=10.0.0.0/8)"
    rc=$?
    assert_eq "PUBLIC_SOURCE_RANGES beside EXTERNAL_LOADBALANCER=false fails" "1" "$rc"
    assert_contains "naming the ClusterIP Service" "$out" "PUBLIC_SOURCE_RANGES needs EXTERNAL_LOADBALANCER=true"
    out="$(run_preflight "$with_yq" EXTERNAL_CLUSTER=true 'PUBLIC_SOURCE_RANGES=10.0.0.0/8,10.1.2.3')"
    assert_contains "an entry without a prefix length is refused by name" "$out" \
      "PUBLIC_SOURCE_RANGES holds '10.1.2.3', which is no IPv4 CIDR"
    out="$(run_preflight "$with_yq" EXTERNAL_CLUSTER=true 'PUBLIC_SOURCE_RANGES=300.0.0.0/8')"
    assert_contains "an octet over 255 is refused" "$out" "holds '300.0.0.0/8'"
  fi

  out="$(run_preflight "$with_yq" PUBLIC_SOURCE_RANGES=10.0.0.0/8)"
  rc=$?
  assert_eq "the kind mode refuses PUBLIC_SOURCE_RANGES" "1" "$rc"
  assert_contains "naming the mode that reads it" "$out" \
    "PUBLIC_SOURCE_RANGES is read only under EXTERNAL_CLUSTER=true EXTERNAL_LOADBALANCER=true"

  mkdir -p "$TMP/no-cert/base" "$TMP/no-cert/infrastructure"
  : >"$TMP/no-cert/base/kustomization.yaml"
  : >"$TMP/no-cert/infrastructure/kustomization.yaml"
  out="$(run_preflight "$with_yq" EXTERNAL_CLUSTER=true "EXTERNAL_OVERLAY=$TMP/no-cert")"
  rc=$?
  assert_eq "an overlay without gateway-certificate.yaml is refused" "1" "$rc"
  assert_contains "naming the template" "$out" "has no infrastructure/gateway-certificate.yaml"
}

# ---------------------------------------------------------------------------
# Test 4: resolve_public_source_ranges
# ---------------------------------------------------------------------------
test_resolve_public_source_ranges() {
  echo "Test: resolve_public_source_ranges adds the shoot's pod and node networks"
  local stub out rc
  stub="$TMP/resolve"
  make_stub "$stub"

  printf '%s' '{"data":{"podNetwork":"10.240.0.0/13","podNetworks":"10.240.0.0/13","nodeNetwork":"10.128.108.0/22","nodeNetworks":"10.128.108.0/22,10.130.0.0/22"}}' >"$stub/shoot-info"
  out="$(run_fn "$stub" 'resolve_public_source_ranges; printf "RESOLVED=%s\n" "$(tr "\n" " " <<<"$PUBLIC_SOURCE_RANGES_RESOLVED")"' \
    'PUBLIC_SOURCE_RANGES=92.209.233.207/32 10.240.0.0/13')"
  assert_contains "the ranges come first, then every pod and node network, each once" "$out" \
    "RESOLVED=92.209.233.207/32 10.240.0.0/13 10.128.108.0/22 10.130.0.0/22 "
  assert_contains "and are logged" "$out" "Gateway sources     : 92.209.233.207/32 10.240.0.0/13 10.128.108.0/22 10.130.0.0/22"

  rm -f "$stub/shoot-info"
  out="$(run_fn "$stub" resolve_public_source_ranges PUBLIC_SOURCE_RANGES=1.2.3.4/32)"
  rc=$?
  assert_eq "a cluster without shoot-info is refused" "1" "$rc"
  assert_contains "naming the ConfigMap and the way out" "$out" "Deploy without"

  echo 1 >"$stub/shoot-info-rc"
  out="$(run_fn "$stub" resolve_public_source_ranges PUBLIC_SOURCE_RANGES=1.2.3.4/32)"
  assert_contains "an unreadable ConfigMap is refused" "$out" "cannot read ConfigMap kube-system/shoot-info"
  rm -f "$stub/shoot-info-rc"

  printf '%s' '{"data":{"podNetworks":"10.240.0.0/13"}}' >"$stub/shoot-info"
  out="$(run_fn "$stub" resolve_public_source_ranges PUBLIC_SOURCE_RANGES=1.2.3.4/32)"
  assert_contains "a ConfigMap without a node network is refused" "$out" "names no pod network or no node network"

  printf '%s' '{"data":{"podNetworks":"10.240.0.0/13","nodeNetworks":"fd00::/64"}}' >"$stub/shoot-info"
  out="$(run_fn "$stub" resolve_public_source_ranges PUBLIC_SOURCE_RANGES=1.2.3.4/32)"
  assert_contains "a network that is no IPv4 CIDR is refused by name" "$out" "names 'fd00::/64' as a pod or node network"
}

# ---------------------------------------------------------------------------
# Test 5: render_envoy_proxy on the lab render
# ---------------------------------------------------------------------------
test_render_envoy_proxy() {
  echo "Test: render_envoy_proxy on deploy/lab/metal-stack/infrastructure"
  if ! command -v kustomize >/dev/null 2>&1 || ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: kustomize or yq not installed (8 checks skipped)"
    SKIP=$((SKIP + 8))
    return
  fi
  local render="$TMP/infra.yaml" out
  if ! kustomize build "$LAB_INFRA" >"$render" 2>"$TMP/kustomize.err"; then
    echo "  FAIL: kustomize build $LAB_INFRA failed:"
    head -5 "$TMP/kustomize.err"
    FAIL=$((FAIL + 8))
    return
  fi
  local service='select(.kind == "EnvoyProxy" and .metadata.name == "envoy-public") | .spec.provider.kubernetes.envoyService'
  local others='select(.kind != "EnvoyProxy")'

  filter() {
    (
      # shellcheck source=/dev/null
      source "$DEPLOY_INFRA_SH" 2>/dev/null
      # shellcheck disable=SC2034 # read by render_envoy_proxy
      EXTERNAL_CLUSTER="$1" EXTERNAL_LOADBALANCER="$2" PUBLIC_SOURCE_RANGES_RESOLVED="$3"
      render_envoy_proxy <"$render"
    )
  }

  out="$(filter true true "")"
  assert_eq "the default leaves the render unchanged" "$(yq -N '.' "$render")" "$(yq -N '.' - <<<"$out")"

  out="$(filter true true $'92.209.233.207/32\n10.240.0.0/13')"
  assert_eq "PUBLIC_SOURCE_RANGES sets the resolved ranges, in order" "92.209.233.207/32,10.240.0.0/13" \
    "$(yq -N -r "$service | .loadBalancerSourceRanges | join(\",\")" - <<<"$out")"
  assert_eq "and keeps the LoadBalancer" "LoadBalancer" "$(yq -N -r "$service | .type" - <<<"$out")"
  assert_eq "and changes no other document" "$(yq -N "$others" "$render")" "$(yq -N "$others" - <<<"$out")"

  out="$(filter true false "")"
  assert_eq "EXTERNAL_LOADBALANCER=false makes envoy-public a ClusterIP Service" "ClusterIP" \
    "$(yq -N -r "$service | .type" - <<<"$out")"
  assert_eq "without externalTrafficPolicy, which a ClusterIP Service refuses" "false" \
    "$(yq -N -r "$service | has(\"externalTrafficPolicy\")" - <<<"$out")"
  assert_eq "and changes no other document" "$(yq -N "$others" "$render")" "$(yq -N "$others" - <<<"$out")"

  out="$(filter false false $'10.0.0.0/8')"
  assert_eq "the kind mode passes the stream through untouched" "$(cat "$render")" "$out"
}

# ---------------------------------------------------------------------------
# Test 6: configure_public_gateway
# ---------------------------------------------------------------------------
test_configure_public_gateway() {
  echo "Test: configure_public_gateway issues the certificate for the Gateway's address"
  local stub out rc

  # First run: no address on the first two polls.
  stub="$TMP/first"
  make_stub "$stub"
  echo 2 >"$stub/address-after"
  echo 154.41.192.90 >"$stub/address"
  cert_json 2 2 >"$stub/cert.json"
  echo httproute.gateway.networking.k8s.io/dizzy-grafana >"$stub/dizzy-route"
  out="$(run_fn "$stub" configure_public_gateway)"
  rc=$?
  assert_eq "a first run exits 0" "0" "$rc"
  assert_contains "it sets PUBLIC_SUFFIX from the address" "$out" "PUBLIC_SUFFIX=154-41-192-90.nip.io"
  assert_eq "it applies twice" "2" "$(ls "$stub"/apply-*.yaml | wc -l | tr -d ' ')"
  assert_eq "first for 127-0-0-1.nip.io, so the listener can start" "*.127-0-0-1.nip.io *.127-0-0-1.nip.io" \
    "$(yq -N -r '[.spec.commonName, .spec.dnsNames[0]] | join(" ")' "$stub/apply-1.yaml")"
  assert_eq "then for the address" "*.154-41-192-90.nip.io *.154-41-192-90.nip.io" \
    "$(yq -N -r '[.spec.commonName, .spec.dnsNames[0]] | join(" ")' "$stub/apply-2.yaml")"
  assert_eq "no placeholder is left" "" "$(grep -h PUBLIC_SUFFIX "$stub"/apply-*.yaml || true)"
  # Without comments: the template's header names the placeholder, which the
  # substitution replaces there as well.
  assert_eq "the rest of the template is applied as it is" \
    "$(yq -N 'del(.spec.commonName) | del(.spec.dnsNames) | ... comments=""' "$LAB_INFRA/gateway-certificate.yaml")" \
    "$(yq -N 'del(.spec.commonName) | del(.spec.dnsNames) | ... comments=""' "$stub/apply-2.yaml")"
  assert_contains "it reads the certificate the apply returned" "$(cat "$stub/calls")" \
    "kubectl get certificate openstack-gw-tls -n openstack -o json"
  assert_contains "it moves the dizzy route onto the suffix" "$(cat "$stub/calls")" \
    'kubectl -n dizzy patch httproute dizzy-grafana --type=merge -p {"spec":{"hostnames":["dizzy.154-41-192-90.nip.io"]}}'

  # Re-run: the Service has its address from the start; no dizzy route.
  stub="$TMP/rerun"
  make_stub "$stub"
  echo 154.41.192.90 >"$stub/address"
  cert_json 1 1 >"$stub/cert.json"
  out="$(run_fn "$stub" configure_public_gateway)"
  assert_eq "a re-run applies once" "1" "$(ls "$stub"/apply-*.yaml | wc -l | tr -d ' ')"
  assert_eq "for the address it already has" "*.154-41-192-90.nip.io" \
    "$(yq -N -r '.spec.dnsNames[0]' "$stub/apply-1.yaml")"
  assert_not_contains "and patches no absent dizzy route" "$(cat "$stub/calls")" "patch httproute"

  # A Ready condition of the previous generation does not count.
  stub="$TMP/stale"
  make_stub "$stub"
  echo 154.41.192.90 >"$stub/address"
  cert_json 2 1 >"$stub/cert.json"
  out="$(run_fn "$stub" configure_public_gateway POD_TIMEOUT=0)"
  rc=$?
  assert_eq "a certificate Ready at an older generation times out" "1" "$rc"
  assert_contains "naming it" "$out" "Timed out waiting for Certificate/openstack-gw-tls in 'openstack'"

  # No address in time.
  stub="$TMP/no-address"
  make_stub "$stub"
  cert_json 1 1 >"$stub/cert.json"
  out="$(run_fn "$stub" configure_public_gateway POD_TIMEOUT=0)"
  rc=$?
  assert_eq "a Service without an address in time aborts" "1" "$rc"
  assert_contains "naming the way out" "$out" "rerun with EXTERNAL_LOADBALANCER=false"

  # A hostname in place of an address.
  stub="$TMP/hostname"
  make_stub "$stub"
  echo lb.example.net >"$stub/address"
  cert_json 1 1 >"$stub/cert.json"
  out="$(run_fn "$stub" configure_public_gateway)"
  rc=$?
  assert_eq "a hostname in place of an address aborts" "1" "$rc"
  assert_contains "because nip.io needs an IPv4 address" "$out" "got 'lb.example.net' from the load balancer"

  # EXTERNAL_LOADBALANCER=false: the port-forward suffix, no Service read.
  stub="$TMP/clusterip"
  make_stub "$stub"
  cert_json 1 1 >"$stub/cert.json"
  out="$(run_fn "$stub" configure_public_gateway EXTERNAL_LOADBALANCER=false)"
  assert_contains "EXTERNAL_LOADBALANCER=false keeps 127-0-0-1.nip.io" "$out" "PUBLIC_SUFFIX=127-0-0-1.nip.io"
  assert_eq "it applies once, for that suffix" "*.127-0-0-1.nip.io" \
    "$(yq -N -r '.spec.dnsNames[0]' "$stub/apply-1.yaml")"
  assert_not_contains "and reads no Service" "$(cat "$stub/calls")" "get svc"
}

# ---------------------------------------------------------------------------
# Test 7: the wiring in main() and check_external_cluster
# ---------------------------------------------------------------------------
line_of() {
  grep -nF -- "$1" "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1
}

assert_before() {
  if [ -n "$2" ] && [ -n "$3" ] && [ "$2" -lt "$3" ]; then
    echo "  PASS: $1"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $1 (line '${2}' is not before line '${3}')"
    FAIL=$((FAIL + 1))
  fi
}

test_wiring() {
  echo "Test: main() and check_external_cluster call the Gateway functions"
  assert_eq "both Step 5 applies of the overlay pipe through render_envoy_proxy" "2" \
    "$(grep -cE '(\| render_envoy_proxy \\$|\| render_envoy_proxy \| kubectl apply -f -$)' "$DEPLOY_INFRA_SH")"
  assert_contains "configure_public_gateway runs behind an EXTERNAL_CLUSTER gate" \
    "$(grep -B1 -E '^    configure_public_gateway$' "$DEPLOY_INFRA_SH")" \
    'if [[ "${EXTERNAL_CLUSTER}" == "true" ]]; then'
  assert_before "after the infrastructure apply of Step 5" \
    "$(line_of 'kubectl kustomize "${OVERLAY_ROOT}/infrastructure" | render_envoy_proxy | kubectl apply -f -')" \
    "$(grep -nE '^    configure_public_gateway$' "$DEPLOY_INFRA_SH" | cut -d: -f1)"
  assert_before "and before the Gateway wait" \
    "$(grep -nE '^    configure_public_gateway$' "$DEPLOY_INFRA_SH" | cut -d: -f1)" \
    "$(line_of 'wait_for_gateway_programmed openstack-gw openstack')"
  assert_contains "check_external_cluster resolves the ranges behind a PUBLIC_SOURCE_RANGES gate" \
    "$(grep -B1 -E '^    resolve_public_source_ranges$' "$DEPLOY_INFRA_SH")" \
    'if [[ -n "${PUBLIC_SOURCE_RANGES}" ]]; then'
  assert_contains "preflight_external_cluster checks the knobs" \
    "$(awk '/^preflight_external_cluster\(\) \{/,/^}/' "$DEPLOY_INFRA_SH")" "preflight_public_source_ranges"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_knobs
test_split_and_validate
test_preflight
test_resolve_public_source_ranges
test_render_envoy_proxy
test_configure_public_gateway
test_wiring

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
