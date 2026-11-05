#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify how hack/ci-run-tempest.sh resolves ${NEUTRON_PUBLIC_NETWORK_ID}, with
# stub kubectl, curl and docker commands first on PATH:
#   - a tempest.conf without the placeholder makes no lookup, and the rendered
#     file differs from the template in the Keystone host and the password
#     alone, so every leg but the neutron ones renders as before;
#   - a tempest.conf with it lists the one external network through a docker
#     run of the tempest image and renders that id as public_network_id;
#   - an empty NEUTRON_K8S_NAME, a failed lookup, no id, two ids or a line that
#     is no network id each exit 1 with an ::error:: line before the Tempest
#     container starts;
#   - the admin password reaches the lookup container through its environment
#     and never appears on a docker command line.
#
# kubectl answers the admin-secret read and blocks on a sleeping port-forward,
# which the runner's cleanup kills. curl answers every readiness poll. docker
# records each argv in the case's docker.log. A call that lists networks also
# records the OS_PASSWORD it was handed in docker-env.log and answers with the
# case's lookup.stdout, lookup.stderr and lookup.rc; every other call is the
# Tempest run, which starts nothing. CONFIG_DIR, OUTPUT_DIR and
# GITHUB_WORKSPACE point into a scratch directory, so a run writes nothing
# into the repository tree.
#
# Usage: bash tests/unit/hack/ci_run_tempest_public_network_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SCRIPT="$PROJECT_ROOT/hack/ci-run-tempest.sh"
BASE_PATH="$PATH"

PASS=0
FAIL=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

PASSWORD="Tempest-Admin-Pw-1614"
NEUTRON_NAME="neutron-tempest-2026-1"
NETWORK_ID="0b6a3c1e-4f2d-4a8b-9c7e-1d2f3a4b5c6d"
OTHER_ID="7e9f0a1b-2c3d-4e5f-8a9b-0c1d2e3f4a5b"

STUBS="$TMP_DIR/stubs"
mkdir -p "$STUBS"

cat >"$STUBS/kubectl" <<STUB
#!/bin/bash
case "\$1" in
  get) printf '%s' "$(printf '%s' "$PASSWORD" | base64)" ;;
  # Absolute path: a real forward outlives the suite it serves.
  port-forward) exec /bin/sleep 600 ;;
esac
exit 0
STUB

cat >"$STUBS/curl" <<'STUB'
#!/bin/bash
exit 0
STUB

cat >"$STUBS/docker" <<'STUB'
#!/bin/bash
printf '%s\n' "$*" >>"$STUB_CASE_DIR/docker.log"
case " $* " in
  *" network list "*)
    printf '%s\n' "${OS_PASSWORD-<unset>}" >>"$STUB_CASE_DIR/docker-env.log"
    cat "$STUB_CASE_DIR/lookup.stdout"
    cat "$STUB_CASE_DIR/lookup.stderr" >&2
    exit "$(cat "$STUB_CASE_DIR/lookup.rc")"
    ;;
esac
exit 0
STUB
chmod +x "$STUBS/kubectl" "$STUBS/curl" "$STUBS/docker"

# The template every case renders. Only the placeholder cases append the
# public_network_id line.
TEMPLATE='[identity]
uri_v3 = http://keystone-neutron-tempest-2026-1.openstack.svc:5000/v3

[auth]
admin_password = ${KEYSTONE_ADMIN_PASSWORD}

[network]
project_networks_reachable = false'

# make_case <name> <with-placeholder: yes|no> <stdout> <stderr> <rc>
# Creates the case directory under $TMP_DIR and echoes its path.
make_case() {
  local dir="$TMP_DIR/$1"
  mkdir -p "$dir/config"
  printf '%s\n' "$TEMPLATE" >"$dir/config/tempest.conf"
  if [ "$2" = yes ]; then
    printf '%s\n' 'public_network_id = ${NEUTRON_PUBLIC_NETWORK_ID}' \
      >>"$dir/config/tempest.conf"
  fi
  printf '%s\n' 'tempest.api.network.test_networks' \
    >"$dir/config/include-tests.txt"
  printf '%s' "$3" >"$dir/lookup.stdout"
  printf '%s' "$4" >"$dir/lookup.stderr"
  printf '%s' "$5" >"$dir/lookup.rc"
  : >"$dir/docker.log"
  : >"$dir/docker-env.log"
  echo "$dir"
}

# run_case <dir> <neutron-name>
# Runs hack/ci-run-tempest.sh as the neutron leg against the case in <dir>, with
# NEUTRON_K8S_NAME set to <neutron-name> (empty is a leg that names no
# Neutron). Sets OUTPUT to the combined output and RC to the exit code.
run_case() {
  local dir="$1" neutron="$2"
  OUTPUT=$(
    unset GLANCE_K8S_NAME BARBICAN_K8S_NAME CINDER_K8S_NAME NOVA_K8S_NAME \
      NOVA_CONSOLE_K8S_NAME PLACEMENT_K8S_NAME OS_PASSWORD
    PATH="$STUBS:$BASE_PATH"
    CONFIG_DIR="$dir/config"
    OUTPUT_DIR="$dir/output"
    GITHUB_WORKSPACE="$dir"
    SERVICE_K8S_NAME=keystone-neutron-tempest-2026-1
    NEUTRON_K8S_NAME="$neutron"
    TEMPEST_IMAGE=ghcr.io/c5c3/tempest:2026.1
    STUB_CASE_DIR="$dir"
    export PATH CONFIG_DIR OUTPUT_DIR GITHUB_WORKSPACE SERVICE_K8S_NAME
    export NEUTRON_K8S_NAME TEMPEST_IMAGE STUB_CASE_DIR
    bash "$SCRIPT" 2>&1
  )
  RC=$?
}

# assert_password_not_on_docker_argv <case> <dir>
assert_password_not_on_docker_argv() {
  assert_not_contains "$1: the admin password is on no docker command line" \
    "$(cat "$2/docker.log")" "$PASSWORD"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_a_template_without_the_placeholder_renders_as_before() {
  echo "Test: a tempest.conf without the placeholder makes no lookup"

  # NEUTRON_K8S_NAME is set, as on the cinder and nova legs: the placeholder
  # alone decides whether the lookup runs.
  local dir
  dir=$(make_case no-placeholder no "" "" 0)
  run_case "$dir" "$NEUTRON_NAME"

  assert_eq "it exits 0" "0" "$RC"
  assert_not_contains "it prints no ::error::" "$OUTPUT" "::error::"
  assert_not_contains "it lists no networks" "$(cat "$dir/docker.log")" \
    "network list"
  assert_eq "its one docker call is the Tempest run" "1" \
    "$(grep -c . "$dir/docker.log")"
  assert_contains "and that call runs the suites" "$(cat "$dir/docker.log")" \
    "run-tests.sh"
  assert_not_contains "it names no external network" "$OUTPUT" \
    "External network for Tempest"
  assert_eq "it keeps no lookup log" "no" \
    "$([ -e "$dir/output/public-network-lookup.log" ] && echo yes || echo no)"

  local expected='[identity]
uri_v3 = http://localhost:5000/v3

[auth]
admin_password = Tempest-Admin-Pw-1614

[network]
project_networks_reachable = false'
  assert_eq "the rendered file differs only in the Keystone host and password" \
    "$expected" "$(cat "$dir/output/config/tempest.conf")"
  assert_password_not_on_docker_argv "no placeholder" "$dir"
}

test_one_external_network_is_rendered() {
  echo "Test: the placeholder and one external network render its id"

  # The warning on stderr is the Python noise the separate log keeps away
  # from the ids.
  local dir lookup
  dir=$(make_case one-id yes "$NETWORK_ID"$'\n' \
    "WARNING: a deprecation warning"$'\n' 0)
  run_case "$dir" "$NEUTRON_NAME"

  assert_eq "it exits 0" "0" "$RC"
  assert_not_contains "it prints no ::error::" "$OUTPUT" "::error::"
  assert_contains "it names the network it found" "$OUTPUT" \
    "External network for Tempest: $NETWORK_ID"
  assert_file_contains_fixed "the rendered tempest.conf carries that id" \
    "$dir/output/config/tempest.conf" "public_network_id = $NETWORK_ID"
  assert_file_contains_fixed "the lookup's stderr lands in its own log" \
    "$dir/output/public-network-lookup.log" "WARNING: a deprecation warning"

  lookup=$(grep -F "network list" "$dir/docker.log")
  assert_contains "the lookup runs on the host network" "$lookup" \
    "--network host"
  assert_contains "it reaches Neutron through the 9696 forward" "$lookup" \
    "--add-host $NEUTRON_NAME.openstack.svc:127.0.0.1"
  assert_contains "it hands the password over by name only" "$lookup" \
    "-e OS_PASSWORD ghcr.io/c5c3/tempest:2026.1 "
  assert_contains "it lists the external networks' ids" "$lookup" \
    "openstack network list --external --format value -c ID"
  assert_eq "the container gets the password through its environment" \
    "$PASSWORD" "$(cat "$dir/docker-env.log")"
  assert_contains "the Tempest container runs afterwards" \
    "$(cat "$dir/docker.log")" "run-tests.sh"
  assert_password_not_on_docker_argv "one id" "$dir"
}

test_an_empty_neutron_name_stops_before_docker() {
  echo "Test: the placeholder without NEUTRON_K8S_NAME exits 1 before docker"

  local dir
  dir=$(make_case no-neutron yes "$NETWORK_ID"$'\n' "" 0)
  run_case "$dir" ""

  assert_eq "it exits 1" "1" "$RC"
  assert_contains "it says NEUTRON_K8S_NAME is empty" "$OUTPUT" \
    "::error::$dir/config/tempest.conf names \${NEUTRON_PUBLIC_NETWORK_ID}, but NEUTRON_K8S_NAME is empty"
  assert_eq "docker never runs" "" "$(cat "$dir/docker.log")"
  assert_password_not_on_docker_argv "no NEUTRON_K8S_NAME" "$dir"
}

test_no_external_network_stops_the_run() {
  echo "Test: a lookup that finds no external network exits 1"

  local dir
  dir=$(make_case no-id yes "" "" 0)
  run_case "$dir" "$NEUTRON_NAME"

  assert_eq "it exits 1" "1" "$RC"
  assert_contains "it says no external network exists" "$OUTPUT" \
    "::error::No external network exists; the neutron leg's 05-external-network-job.yaml creates one before this step"
  assert_contains "it did look" "$(cat "$dir/docker.log")" "network list"
  assert_not_contains "the Tempest container never starts" \
    "$(cat "$dir/docker.log")" "run-tests.sh"
  assert_password_not_on_docker_argv "no id" "$dir"
}

test_two_external_networks_stop_the_run() {
  echo "Test: a lookup that finds two external networks exits 1"

  local dir
  dir=$(make_case two-ids yes "$NETWORK_ID"$'\n'"$OTHER_ID"$'\n' "" 0)
  run_case "$dir" "$NEUTRON_NAME"

  assert_eq "it exits 1" "1" "$RC"
  assert_contains "it names both" "$OUTPUT" \
    "::error::Expected one external network, found 2: $NETWORK_ID $OTHER_ID"
  assert_not_contains "the Tempest container never starts" \
    "$(cat "$dir/docker.log")" "run-tests.sh"
  assert_password_not_on_docker_argv "two ids" "$dir"
}

test_a_failed_lookup_stops_the_run() {
  echo "Test: a lookup that exits 1 stops the run and keeps its stderr"

  local dir
  dir=$(make_case lookup-failed yes "" \
    "The request you have made requires authentication. (HTTP 401)"$'\n' 1)
  run_case "$dir" "$NEUTRON_NAME"

  assert_eq "it exits 1" "1" "$RC"
  assert_contains "it says the lookup failed and where to look" "$OUTPUT" \
    "::error::Looking up the external network failed (exit 1); see $dir/output/public-network-lookup.log"
  assert_contains "it prints the log's last line" "$OUTPUT" "(HTTP 401)"
  assert_file_contains_fixed "the log holds the client's error" \
    "$dir/output/public-network-lookup.log" "HTTP 401"
  assert_not_contains "the Tempest container never starts" \
    "$(cat "$dir/docker.log")" "run-tests.sh"
  assert_password_not_on_docker_argv "lookup failed" "$dir"
}

test_a_line_that_is_no_id_stops_the_run() {
  echo "Test: a lookup that prints no network id exits 1"

  local dir
  dir=$(make_case not-a-uuid yes "not-a-uuid"$'\n' "" 0)
  run_case "$dir" "$NEUTRON_NAME"

  assert_eq "it exits 1" "1" "$RC"
  assert_contains "it quotes what came back" "$OUTPUT" \
    "::error::The external network lookup returned 'not-a-uuid', not a network id"
  assert_not_contains "the Tempest container never starts" \
    "$(cat "$dir/docker.log")" "run-tests.sh"
  assert_password_not_on_docker_argv "not a uuid" "$dir"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_a_template_without_the_placeholder_renders_as_before
test_one_external_network_is_rendered
test_an_empty_neutron_name_stops_before_docker
test_no_external_network_stops_the_run
test_two_external_networks_stop_the_run
test_a_failed_lookup_stops_the_run
test_a_line_that_is_no_id_stops_the_run

echo ""
echo "Results: $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
