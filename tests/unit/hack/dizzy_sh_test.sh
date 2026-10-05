#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the two cluster modes of hack/dizzy.sh:
#   1. generate_clouds_yaml under EXTERNAL_CLUSTER=true writes the auth URL of
#      the Gateway port-forward, https://keystone.127-0-0-1.nip.io:8443/v3, and
#      logs it, without calling docker; a non-empty DIZZY_AUTH_URL still wins,
#      an empty one does not. It collects the certificates of the Secrets
#      openstack/*-nip-io-tls into gateway-ca.pem beside clouds.yaml and points
#      cacert at that file instead of writing verify: false; without one it
#      exits 1 and writes no clouds.yaml.
#   2. Without EXTERNAL_CLUSTER, and with EXTERNAL_CLUSTER=yes, it reads the
#      Keystone host port with `docker port <cluster>-control-plane 31443/tcp`
#      and falls back to :443, logging the fallback, when that fails. The
#      file keeps verify: false, and no TLS Secret is read.
#   3. A Secret that cannot be read, or one with an empty password, exits 1
#      with its error and writes no file; a single quote in the password is
#      doubled in the YAML scalar.
#   4. probe_ingest under EXTERNAL_CLUSTER=true calls no docker; when
#      localhost:8428 does not answer it returns 0 and prints the
#      VictoriaMetrics warning and the port-forward to open. In kind mode it
#      warns about a missing 30428 mapping and names no port-forward.
#   5. EXTERNAL_PUBLIC_PORT equals PUBLIC_PORT of hack/deploy-infra.sh in
#      external mode.
#   6. Run without a subcommand, the script prints a usage that names
#      EXTERNAL_CLUSTER and exits 1.
#   7. stage_dashboards exits 1 with the download error when the release
#      tarball cannot be fetched.
#   8. run_chaos under EXTERNAL_CLUSTER=true has dizzy verify Keystone against
#      gateway-ca.pem even when the caller exports an OS_CACERT, which
#      gophercloud would prefer to the cacert of clouds.yaml.
#
# The script is sourced in a subshell (its BASH_SOURCE guard keeps main() from
# running) with stub kubectl, docker and curl first on PATH. CLOUDS_FILE,
# REPO_ROOT and DIZZY_CACHE_DIR are pointed at a temporary directory after
# sourcing, so a developer's _output/dizzy/ is never written.
#
# Usage: bash tests/unit/hack/dizzy_sh_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DIZZY_SH="$PROJECT_ROOT/hack/dizzy.sh"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

LAB_AUTH_URL="https://keystone.127-0-0-1.nip.io:8443/v3"
PORT_FORWARD_HINT="Open the port-forward first: kubectl -n dizzy port-forward svc/dizzy-victoria-metrics-server 8428:8428"

# What `kubectl get secret -n openstack --field-selector type=kubernetes.io/tls
# -o json` prints by default: one Gateway certificate, and a TLS Secret of
# another name whose certificate stays out of the CA file.
GATEWAY_PEM="$(printf '%s\n' '-----BEGIN CERTIFICATE-----' 'a2V5c3RvbmU=' '-----END CERTIFICATE-----')"
OTHER_PEM="$(printf '%s\n' '-----BEGIN CERTIFICATE-----' 'b3BlbmJhbw==' '-----END CERTIFICATE-----')"
TLS_SECRETS_JSON="$(jq -cn --arg gw "$(printf '%s\n' "$GATEWAY_PEM" | base64 | tr -d '\n')" \
  --arg other "$(printf '%s\n' "$OTHER_PEM" | base64 | tr -d '\n')" \
  '{items: [{metadata: {name: "keystone-nip-io-tls"}, data: {"ca.crt": $gw}},
            {metadata: {name: "openbao-tls"}, data: {"ca.crt": $other}}]}')"
export TLS_SECRETS_JSON

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stubs <dir>
# kubectl, docker and curl stubs that append their argv to $CALL_LOG and answer
# from the environment:
#   KUBECTL_SECRET_RC  non-empty: `get secret` fails with NotFound
#   KUBECTL_PASSWORD   the password the Secret holds, which `get secret` prints
#                      base64-encoded (default s3cr3t; empty: prints nothing)
#   TLS_SECRETS_JSON   what `get secret --field-selector ...` prints
#   DOCKER_PORT_OUT    what `docker port` prints (default nothing)
#   DOCKER_PORT_RC     exit code of `docker port` (default 0)
#   CURL_RC            exit code of curl (default 0)
make_stubs() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
echo "kubectl $*" >>"$CALL_LOG"
if [ -n "${KUBECTL_SECRET_RC:-}" ]; then
  echo 'Error from server (NotFound): secrets "controlplane-keystone-admin-credentials" not found' >&2
  exit 1
fi
case " $* " in
  *" --field-selector "*) printf '%s' "${TLS_SECRETS_JSON:-}"; exit 0 ;;
esac
printf '%s' "${KUBECTL_PASSWORD-s3cr3t}" | base64 | tr -d '\n'
STUB
  cat >"$dir/docker" <<'STUB'
#!/bin/bash
echo "docker $*" >>"$CALL_LOG"
if [ -n "${DOCKER_PORT_OUT:-}" ]; then echo "$DOCKER_PORT_OUT"; fi
exit "${DOCKER_PORT_RC:-0}"
STUB
  cat >"$dir/curl" <<'STUB'
#!/bin/bash
echo "curl $*" >>"$CALL_LOG"
exit "${CURL_RC:-0}"
STUB
  chmod +x "$dir/kubectl" "$dir/docker" "$dir/curl"
}

# run_fn <tmp> <function> [env_var=value...]
# Sources hack/dizzy.sh with the stubs of <tmp>/bin first on the real PATH and
# the given overrides, points CLOUDS_FILE at <tmp>/clouds.yaml and REPO_ROOT at
# <tmp>/repo, and runs <function>. Echoes combined output; returns its status.
# shellcheck disable=SC2034 # the functions of the sourced script read the three
run_fn() {
  local tmp="$1" fn="$2"
  shift 2
  (
    unset EXTERNAL_CLUSTER KIND_CLUSTER DIZZY_SECRET DIZZY_CP_NAMESPACE DIZZY_AUTH_URL DIZZY_VERSION
    for assignment in "$@"; do
      export "${assignment?}"
    done
    PATH="$tmp/bin:$PATH"
    export PATH
    # shellcheck source=/dev/null
    source "$DIZZY_SH"
    CLOUDS_FILE="$tmp/clouds.yaml"
    REPO_ROOT="$tmp/repo"
    DIZZY_CACHE_DIR="$tmp/repo/_output/dizzy/${DIZZY_VERSION}"
    "$fn"
  ) 2>&1
}

# auth_url <file> — the auth_url line's value in a written clouds.yaml.
auth_url() {
  sed -n 's/^[[:space:]]*auth_url: //p' "$1"
}

# new_case <tmp> — empties the call log and removes a written clouds.yaml and
# CA file.
new_case() {
  : >"$CALL_LOG"
  rm -f "$1/clouds.yaml" "$1/gateway-ca.pem"
}

# ---------------------------------------------------------------------------
# Test 1: the external auth URL
# ---------------------------------------------------------------------------
test_external_auth_url() {
  echo "Test: generate_clouds_yaml under EXTERNAL_CLUSTER=true writes the Gateway port-forward's URL without docker"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  export CALL_LOG="$tmp/calls.log"

  new_case "$tmp"
  output="$(run_fn "$tmp" generate_clouds_yaml EXTERNAL_CLUSTER=true "KUBECTL_PASSWORD=s3cr'et")"
  rc=$?
  assert_eq "it exits 0" "0" "$rc"
  assert_eq "auth_url is the Gateway port-forward's" "$LAB_AUTH_URL" "$(auth_url "$tmp/clouds.yaml")"
  assert_contains "it logs the auth URL and where it comes from" "$output" \
    "Keystone auth URL: $LAB_AUTH_URL (EXTERNAL_CLUSTER=true, the Gateway port-forward on 8443)."
  assert_not_contains "docker is not called" "$(cat "$CALL_LOG")" "docker"
  assert_eq "the file is mode 600" "600" "$(stat -c '%a' "$tmp/clouds.yaml")"
  assert_file_contains_fixed "a single quote in the password is doubled" \
    "$tmp/clouds.yaml" "password: 's3cr''et'"
  assert_contains "kubectl reads the TLS Secrets of openstack" "$(cat "$CALL_LOG")" \
    "kubectl get secret -n openstack --field-selector type=kubernetes.io/tls -o json"
  assert_eq "gateway-ca.pem holds the certificate of keystone-nip-io-tls alone" \
    "$GATEWAY_PEM" "$(cat "$tmp/gateway-ca.pem")"
  assert_file_contains_fixed "cacert points at gateway-ca.pem" \
    "$tmp/clouds.yaml" "    cacert: '$tmp/gateway-ca.pem'"
  assert_file_not_contains "the file has no verify: false" "$tmp/clouds.yaml" "verify: false"

  new_case "$tmp"
  output="$(run_fn "$tmp" generate_clouds_yaml EXTERNAL_CLUSTER=true DIZZY_AUTH_URL=https://example.test:9/v3)"
  rc=$?
  assert_eq "with DIZZY_AUTH_URL it exits 0" "0" "$rc"
  assert_eq "a non-empty DIZZY_AUTH_URL wins" "https://example.test:9/v3" "$(auth_url "$tmp/clouds.yaml")"
  assert_not_contains "and docker is not called" "$(cat "$CALL_LOG")" "docker"
  assert_file_contains_fixed "and cacert still points at gateway-ca.pem" \
    "$tmp/clouds.yaml" "    cacert: '$tmp/gateway-ca.pem'"

  new_case "$tmp"
  output="$(run_fn "$tmp" generate_clouds_yaml EXTERNAL_CLUSTER=true DIZZY_AUTH_URL=)"
  rc=$?
  assert_eq "with an empty DIZZY_AUTH_URL it exits 0" "0" "$rc"
  assert_eq "an empty DIZZY_AUTH_URL gives the Gateway port-forward's URL" "$LAB_AUTH_URL" \
    "$(auth_url "$tmp/clouds.yaml")"

  for secrets in '{"items":[]}' \
    '{"items":[{"metadata":{"name":"openbao-tls"},"data":{"ca.crt":"b3BlbmJhbw=="}}]}'; do
    new_case "$tmp"
    output="$(run_fn "$tmp" generate_clouds_yaml EXTERNAL_CLUSTER=true "TLS_SECRETS_JSON=$secrets")"
    rc=$?
    assert_eq "without a Gateway certificate it exits 1 ($secrets)" "1" "$rc"
    assert_contains "and says it sends no password unverified" "$output" \
      "ERROR: found no Gateway certificate in the Secrets openstack/*-nip-io-tls;"
    assert_eq "and writes no clouds.yaml" "false" "$([[ -e "$tmp/clouds.yaml" ]] && echo true || echo false)"
  done
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Test 2: the kind auth URL
# ---------------------------------------------------------------------------
test_kind_auth_url() {
  echo "Test: generate_clouds_yaml outside the external mode reads the Keystone host port with docker port"

  local tmp output rc mode
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  export CALL_LOG="$tmp/calls.log"

  # Only the value true selects the external mode.
  for mode in "" EXTERNAL_CLUSTER=yes; do
    new_case "$tmp"
    output="$(run_fn "$tmp" generate_clouds_yaml ${mode:+"$mode"} DOCKER_PORT_OUT=127.0.0.1:8443)"
    rc=$?
    assert_eq "${mode:-no EXTERNAL_CLUSTER}: it exits 0" "0" "$rc"
    assert_contains "${mode:-no EXTERNAL_CLUSTER}: docker port reads the kind node's 31443 mapping" \
      "$(cat "$CALL_LOG")" "docker port cobaltcore-control-plane 31443/tcp"
    assert_eq "${mode:-no EXTERNAL_CLUSTER}: auth_url takes the published host port" \
      "https://keystone.127-0-0-1.nip.io:8443/v3" "$(auth_url "$tmp/clouds.yaml")"
    assert_not_contains "${mode:-no EXTERNAL_CLUSTER}: no external auth URL line" "$output" "EXTERNAL_CLUSTER=true, the Gateway"
    assert_file_contains_fixed "${mode:-no EXTERNAL_CLUSTER}: the file keeps verify: false" \
      "$tmp/clouds.yaml" "    verify: false"
    assert_not_contains "${mode:-no EXTERNAL_CLUSTER}: no TLS Secret is read" "$(cat "$CALL_LOG")" "--field-selector"

    new_case "$tmp"
    output="$(run_fn "$tmp" generate_clouds_yaml ${mode:+"$mode"} DOCKER_PORT_RC=1)"
    rc=$?
    assert_eq "${mode:-no EXTERNAL_CLUSTER}: a failing docker port exits 0" "0" "$rc"
    assert_eq "${mode:-no EXTERNAL_CLUSTER}: auth_url falls back to :443" \
      "https://keystone.127-0-0-1.nip.io:443/v3" "$(auth_url "$tmp/clouds.yaml")"
    assert_contains "${mode:-no EXTERNAL_CLUSTER}: the fallback is logged" "$output" \
      "docker port probe for the Keystone host port failed; falling back to :443."
  done
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Test 3: the Secret
# ---------------------------------------------------------------------------
test_secret_errors() {
  echo "Test: generate_clouds_yaml exits 1 without a readable password and writes no file"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  export CALL_LOG="$tmp/calls.log"

  new_case "$tmp"
  output="$(run_fn "$tmp" generate_clouds_yaml EXTERNAL_CLUSTER=true KUBECTL_SECRET_RC=1)"
  rc=$?
  assert_eq "a Secret that cannot be read exits 1" "1" "$rc"
  assert_contains "and names the Secret and its namespace" "$output" \
    "ERROR: could not read admin Secret 'controlplane-keystone-admin-credentials' in namespace 'openstack'."
  assert_eq "no clouds.yaml is written" "false" "$([[ -e "$tmp/clouds.yaml" ]] && echo true || echo false)"

  new_case "$tmp"
  output="$(run_fn "$tmp" generate_clouds_yaml EXTERNAL_CLUSTER=true KUBECTL_PASSWORD=)"
  rc=$?
  assert_eq "a Secret with an empty password exits 1" "1" "$rc"
  assert_contains "and says so" "$output" "has an empty password key"
  assert_eq "no clouds.yaml is written" "false" "$([[ -e "$tmp/clouds.yaml" ]] && echo true || echo false)"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Test 4: the ingest probe
# ---------------------------------------------------------------------------
test_probe_ingest() {
  echo "Test: probe_ingest asks docker only in kind mode and names the port-forward in external mode"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  export CALL_LOG="$tmp/calls.log"

  new_case "$tmp"
  output="$(run_fn "$tmp" probe_ingest EXTERNAL_CLUSTER=true)"
  rc=$?
  assert_eq "external mode with VictoriaMetrics answering returns 0" "0" "$rc"
  assert_eq "and prints nothing" "" "$output"
  assert_contains "curl probes localhost:8428/health" "$(cat "$CALL_LOG")" "curl -fsS http://localhost:8428/health"
  assert_not_contains "docker is not called" "$(cat "$CALL_LOG")" "docker"

  # curl's exit code for a refused connection.
  new_case "$tmp"
  output="$(run_fn "$tmp" probe_ingest EXTERNAL_CLUSTER=true CURL_RC=7)"
  rc=$?
  assert_eq "external mode without VictoriaMetrics still returns 0" "0" "$rc"
  assert_contains "it warns that nothing answers" "$output" \
    "WARNING: no VictoriaMetrics at http://localhost:8428/health"
  assert_contains "and names the port-forward to open" "$output" "$PORT_FORWARD_HINT"
  assert_not_contains "it prints no 30428 warning" "$output" "30428 host-port mapping"
  assert_not_contains "docker is not called" "$(cat "$CALL_LOG")" "docker"

  new_case "$tmp"
  output="$(run_fn "$tmp" probe_ingest CURL_RC=7)"
  rc=$?
  assert_eq "kind mode without a mapping returns 0" "0" "$rc"
  assert_contains "docker port reads the kind node's 30428 mapping" "$(cat "$CALL_LOG")" \
    "docker port cobaltcore-control-plane 30428/tcp"
  assert_contains "an empty answer gives the 30428 warning" "$output" \
    "WARNING: kind cluster 'cobaltcore' has no 30428 host-port mapping;"
  assert_not_contains "the kind mode names no port-forward" "$output" "Open the port-forward first"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Test 5: the port the two scripts share
# ---------------------------------------------------------------------------
test_public_port_matches_deploy_infra() {
  echo "Test: EXTERNAL_PUBLIC_PORT equals the external PUBLIC_PORT of hack/deploy-infra.sh"

  local dizzy_port deploy_port
  # shellcheck source=/dev/null
  dizzy_port="$(unset EXTERNAL_CLUSTER; source "$DIZZY_SH"; printf '%s' "$EXTERNAL_PUBLIC_PORT")"
  # shellcheck source=/dev/null
  deploy_port="$(unset EXTERNAL_OVERLAY KIND_HOST_PORT; export EXTERNAL_CLUSTER=true; source "$DEPLOY_INFRA_SH"; printf '%s' "$PUBLIC_PORT")"
  assert_not_empty "hack/deploy-infra.sh sets PUBLIC_PORT in external mode" "$deploy_port"
  assert_eq "the two ports are equal" "$deploy_port" "$dizzy_port"
}

# ---------------------------------------------------------------------------
# Test 6: the usage
# ---------------------------------------------------------------------------
test_usage() {
  echo "Test: hack/dizzy.sh without a subcommand prints a usage that names EXTERNAL_CLUSTER"

  local output rc
  output="$(bash "$DIZZY_SH" 2>&1)"
  rc=$?
  assert_eq "it exits 1" "1" "$rc"
  assert_contains "it prints the usage" "$output" "Usage: hack/dizzy.sh <subcommand>"
  assert_contains "which names EXTERNAL_CLUSTER" "$output" "EXTERNAL_CLUSTER"
}

# ---------------------------------------------------------------------------
# Test 7: a failed download
# ---------------------------------------------------------------------------
test_stage_dashboards_download_fails() {
  echo "Test: stage_dashboards exits 1 when the release tarball cannot be fetched"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  export CALL_LOG="$tmp/calls.log"

  # curl's exit code for an HTTP error with -f.
  new_case "$tmp"
  output="$(run_fn "$tmp" stage_dashboards DIZZY_VERSION=v0.0.0 CURL_RC=22)"
  rc=$?
  assert_eq "a failed download exits 1" "1" "$rc"
  assert_contains "and names the URL" "$output" \
    "ERROR: failed to download dizzy sources from https://github.com/B42Labs/dizzy/archive/refs/tags/v0.0.0.tar.gz"
  assert_eq "no dashboard is staged" "false" \
    "$([[ -e "$tmp/repo/deploy/kind/dizzy/dashboards" ]] && echo true || echo false)"
  assert_eq "no cache directory is left" "false" \
    "$([[ -e "$tmp/repo/_output/dizzy/v0.0.0" ]] && echo true || echo false)"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Test 8: the CA file of the soak
# ---------------------------------------------------------------------------

# chaos_keystone — run_chaos for keystone; run_fn passes its function no args.
chaos_keystone() {
  run_chaos keystone
}

test_chaos_ignores_os_cacert() {
  echo "Test: run_chaos under EXTERNAL_CLUSTER=true has dizzy verify against gateway-ca.pem despite an exported OS_CACERT"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  export CALL_LOG="$tmp/calls.log"

  # go reports the pinned version, so ensure_binary installs nothing, and the
  # cache directory exists, so ensure_cache fetches nothing. dizzy picks its CA
  # file as gophercloud v2.12.0 does: a non-empty OS_CACERT first, the cacert of
  # the clouds.yaml in OS_CLIENT_CONFIG_FILE otherwise.
  cat >"$tmp/bin/go" <<'STUB'
#!/bin/bash
echo "go $*" >>"$CALL_LOG"
echo "dizzy ${DIZZY_VERSION}"
STUB
  mkdir -p "$tmp/repo/bin" "$tmp/repo/_output/dizzy/v0.0.0"
  cat >"$tmp/repo/bin/dizzy" <<'STUB'
#!/bin/bash
ca="${OS_CACERT:-$(sed -n "s/^ *cacert: '\(.*\)'\$/\1/p" "$OS_CLIENT_CONFIG_FILE")}"
echo "dizzy $* (CA file: $ca)" >>"$CALL_LOG"
STUB
  chmod +x "$tmp/bin/go" "$tmp/repo/bin/dizzy"

  # What Step 7 of docs/quick-start-metal-stack.md leaves exported, naming a
  # file the teardown deleted.
  new_case "$tmp"
  output="$(run_fn "$tmp" chaos_keystone EXTERNAL_CLUSTER=true DIZZY_VERSION=v0.0.0 \
    OS_CACERT=/gone/gateway-ca.pem)"
  rc=$?
  assert_eq "it exits 0" "0" "$rc"
  assert_contains "dizzy runs the keystone soak" "$(cat "$CALL_LOG")" \
    "dizzy keystone chaos --os-cloud devstack-c5c3"
  assert_contains "and verifies against gateway-ca.pem, not OS_CACERT" "$(cat "$CALL_LOG")" \
    "(CA file: $tmp/gateway-ca.pem)"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_external_auth_url
test_kind_auth_url
test_secret_errors
test_probe_ingest
test_public_port_matches_deploy_infra
test_usage
test_stage_dashboards_download_fails
test_chaos_ignores_os_cacert

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
