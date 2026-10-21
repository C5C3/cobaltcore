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
#   9. run_pressure runs `dizzy keystone pressure` with the profile's file from
#      _output/dizzy/pressure/<label>/<profile>/, where dizzy writes its run
#      record; it exports with --otel only when the namespace dizzy exists, and
#      refuses a missing profile (listing the profiles), a malformed profile
#      name and a malformed label before it calls dizzy.
#  10. run_compare takes a run record or the newest run-*.json of a directory
#      for each side, hands DIZZY_ARGS to `dizzy keystone report --compare`
#      and keeps the comparison as text and HTML; a directory without a record
#      exits 1.
#
# The script is sourced in a subshell (its BASH_SOURCE guard keeps main() from
# running) with stub kubectl, docker and curl first on PATH. CLOUDS_FILE,
# REPO_ROOT, DIZZY_CACHE_DIR, PRESSURE_PROFILE_DIR and PRESSURE_OUTPUT_DIR are
# pointed at a temporary directory after sourcing, so a developer's
# _output/dizzy/ is never written.
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
#   KUBECTL_NO_DIZZY_NS non-empty: `get namespace dizzy` fails with NotFound
#   DOCKER_PORT_OUT    what `docker port` prints (default nothing)
#   DOCKER_PORT_RC     exit code of `docker port` (default 0)
#   CURL_RC            exit code of curl (default 0)
make_stubs() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
echo "kubectl $*" >>"$CALL_LOG"
case " $* " in
  *" get namespace dizzy "*)
    if [ -n "${KUBECTL_NO_DIZZY_NS:-}" ]; then
      echo 'Error from server (NotFound): namespaces "dizzy" not found' >&2
      exit 1
    fi
    exit 0
    ;;
esac
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
    unset EXTERNAL_CLUSTER KIND_CLUSTER DIZZY_SECRET DIZZY_CP_NAMESPACE DIZZY_AUTH_URL DIZZY_VERSION \
      DIZZY_SCENARIO DIZZY_ARGS DIZZY_PRESSURE_LABEL
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
    PRESSURE_PROFILE_DIR="$tmp/repo/tests/load/keystone-pressure"
    PRESSURE_OUTPUT_DIR="$tmp/repo/_output/dizzy/pressure"
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
# Test 9: the pressure run
# ---------------------------------------------------------------------------

# make_dizzy_stubs <tmp> — a go that reports the pinned version, so
# ensure_binary installs nothing, and a bin/dizzy that logs its working
# directory and arguments to $CALL_LOG.
make_dizzy_stubs() {
  local tmp="$1"
  cat >"$tmp/bin/go" <<'STUB'
#!/bin/bash
echo "dizzy ${DIZZY_VERSION}"
STUB
  mkdir -p "$tmp/repo/bin"
  cat >"$tmp/repo/bin/dizzy" <<'STUB'
#!/bin/bash
echo "dizzy in $(pwd): $*" >>"$CALL_LOG"
case " $* " in
  *" --format html "*) echo "<html>comparison</html>" ;;
  *" --compare "*) echo "Comparison table" ;;
esac
STUB
  chmod +x "$tmp/bin/go" "$tmp/repo/bin/dizzy"
}

# The wrappers run_fn calls, since it passes its function no args.
pressure_warm_cache() { run_pressure warm-cache; }
pressure_missing() { run_pressure no-such-profile; }
pressure_bad_name() { run_pressure ../warm-cache; }
pressure_no_profile() { run_pressure; }

test_pressure_run() {
  echo "Test: run_pressure runs dizzy keystone pressure from the run record's directory"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  make_dizzy_stubs "$tmp"
  mkdir -p "$tmp/repo/tests/load/keystone-pressure"
  : >"$tmp/repo/tests/load/keystone-pressure/warm-cache.yaml"
  : >"$tmp/repo/tests/load/keystone-pressure/revocation-lookup.yaml"
  export CALL_LOG="$tmp/calls.log"

  new_case "$tmp"
  output="$(run_fn "$tmp" pressure_warm_cache DIZZY_VERSION=v0.0.0 DIZZY_PRESSURE_LABEL=base \
    "DIZZY_ARGS=--duration 2m --seed 7")"
  rc=$?
  assert_eq "it exits 0" "0" "$rc"
  assert_contains "dizzy runs the profile's file from the label's run directory" "$(cat "$CALL_LOG")" \
    "dizzy in $tmp/repo/_output/dizzy/pressure/base/warm-cache: keystone pressure --os-cloud devstack-c5c3 --scenario $tmp/repo/tests/load/keystone-pressure/warm-cache.yaml --otel --duration 2m --seed 7"
  assert_file_contains_fixed "with the generated clouds.yaml" "$tmp/clouds.yaml" "devstack-c5c3:"

  new_case "$tmp"
  output="$(run_fn "$tmp" pressure_warm_cache DIZZY_VERSION=v0.0.0 KUBECTL_NO_DIZZY_NS=1)"
  rc=$?
  assert_eq "without the dizzy stack it exits 0" "0" "$rc"
  assert_contains "and says it exports nothing" "$output" \
    "No dizzy stack (namespace dizzy); the run exports no metrics."
  assert_contains "the default label is run" "$(cat "$CALL_LOG")" \
    "dizzy in $tmp/repo/_output/dizzy/pressure/run/warm-cache: keystone pressure"
  assert_not_contains "and dizzy runs without --otel" "$(cat "$CALL_LOG")" "--otel"

  new_case "$tmp"
  output="$(run_fn "$tmp" pressure_missing DIZZY_VERSION=v0.0.0)"
  rc=$?
  assert_eq "a missing profile exits 1" "1" "$rc"
  assert_contains "and names the file it looked for" "$output" \
    "ERROR: no pressure scenario at $tmp/repo/tests/load/keystone-pressure/no-such-profile.yaml; the profiles are:"
  assert_contains "and lists the profiles" "$output" "revocation-lookup"
  assert_not_contains "dizzy is not called" "$(cat "$CALL_LOG")" "dizzy in"

  new_case "$tmp"
  output="$(run_fn "$tmp" pressure_bad_name DIZZY_VERSION=v0.0.0)"
  rc=$?
  assert_eq "a malformed profile name exits 1" "1" "$rc"
  assert_contains "and says what a name looks like" "$output" "must be a name such as warm-cache"

  new_case "$tmp"
  output="$(run_fn "$tmp" pressure_warm_cache DIZZY_VERSION=v0.0.0 DIZZY_PRESSURE_LABEL=../x)"
  rc=$?
  assert_eq "a malformed label exits 1" "1" "$rc"
  assert_contains "and names the variable" "$output" "DIZZY_PRESSURE_LABEL '../x' must start with"
  assert_not_contains "dizzy is not called" "$(cat "$CALL_LOG")" "dizzy in"

  new_case "$tmp"
  output="$(run_fn "$tmp" pressure_no_profile DIZZY_VERSION=v0.0.0)"
  rc=$?
  assert_eq "no profile exits 1" "1" "$rc"
  assert_contains "with the usage" "$output" "Usage: hack/dizzy.sh <subcommand>"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Test 10: the comparison
# ---------------------------------------------------------------------------

compare_dirs() { run_compare "$COMPARE_BASE" "$COMPARE_OTHER"; }

test_compare() {
  echo "Test: run_compare compares the newest records of two directories, or two files"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  make_dizzy_stubs "$tmp"
  export CALL_LOG="$tmp/calls.log"

  mkdir -p "$tmp/base" "$tmp/patched" "$tmp/empty"
  : >"$tmp/base/run-old.json"
  : >"$tmp/base/run-new.json"
  touch -t 202601010000 "$tmp/base/run-old.json"
  touch -t 202602010000 "$tmp/base/run-new.json"
  : >"$tmp/patched/run-p.json"

  new_case "$tmp"
  output="$(run_fn "$tmp" compare_dirs DIZZY_VERSION=v0.0.0 "COMPARE_BASE=$tmp/base" \
    "COMPARE_OTHER=$tmp/patched" DIZZY_ARGS=--allow-mismatch)"
  rc=$?
  assert_eq "it exits 0" "0" "$rc"
  assert_contains "the newest record of each directory is compared" "$(cat "$CALL_LOG")" \
    "keystone report --run $tmp/base/run-new.json --compare $tmp/patched/run-p.json --allow-mismatch"
  assert_contains "the table goes to stdout" "$output" "Comparison table"
  assert_file_contains_fixed "and is kept as text" \
    "$tmp/repo/_output/dizzy/pressure/compare/run-new-vs-run-p.txt" "Comparison table"
  assert_file_contains_fixed "and as HTML" \
    "$tmp/repo/_output/dizzy/pressure/compare/run-new-vs-run-p.html" "<html>comparison</html>"

  new_case "$tmp"
  output="$(run_fn "$tmp" compare_dirs DIZZY_VERSION=v0.0.0 "COMPARE_BASE=$tmp/base/run-old.json" \
    "COMPARE_OTHER=$tmp/patched/run-p.json")"
  rc=$?
  assert_eq "two files exit 0" "0" "$rc"
  assert_contains "and are compared as given" "$(cat "$CALL_LOG")" \
    "keystone report --run $tmp/base/run-old.json --compare $tmp/patched/run-p.json"

  new_case "$tmp"
  output="$(run_fn "$tmp" compare_dirs DIZZY_VERSION=v0.0.0 "COMPARE_BASE=$tmp/empty" \
    "COMPARE_OTHER=$tmp/patched")"
  rc=$?
  assert_eq "a directory without a record exits 1" "1" "$rc"
  assert_contains "and says so" "$output" "ERROR: no run-*.json in $tmp/empty."
  assert_not_contains "dizzy is not called" "$(cat "$CALL_LOG")" "dizzy in"
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
test_pressure_run
test_compare

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
