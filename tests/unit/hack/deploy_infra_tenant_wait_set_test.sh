#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the service loop in openbao_onboard_database_tenant
# (hack/deploy-infra.sh) covers exactly the services
# deploy/openbao/bootstrap/setup-database-tenant.sh reads a MariaDB root Secret
# for, i.e. the spec-service column of its SERVICE_TENANTS table.
#
# The loop resolves the service namespaces the onboarding waits for. A service
# in the table but not in the loop lets its leg run against a MariaDB that may
# not be Ready in its namespace yet, and that leg hard-exits: a half-applied
# onboarding, with the operator already primed to flip the service to Dynamic.
# A service in the loop but in no row waits for a MariaDB nothing reads, which
# blocks the deploy until timeout. Two rows (nova-api and nova-cell) share the
# nova spec-service block, so the comparison is on the distinct values of the
# column.
#
# The loop is read out of the script with awk, and the table through
# tests/lib/service_tenants.sh. The function itself runs against a recording
# kubectl stub, to check that the namespaces it waits for come from a single
# read of the ControlPlane and that an unreadable ControlPlane stops it before
# any wait. Nothing touches a cluster.
#
# Usage: bash tests/unit/hack/deploy_infra_tenant_wait_set_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/service_tenants.sh
source "$PROJECT_ROOT/tests/lib/service_tenants.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# wait_set prints one service per line: the words of the `for svc in ...; do`
# line inside openbao_onboard_database_tenant. The awk range ends at the
# function's closing brace, so a `for svc in` elsewhere in the script is not
# picked up.
wait_set() {
  awk '
    /^openbao_onboard_database_tenant\(\)/ { inside = 1 }
    inside && /^\}$/ { exit }
    inside && /for svc in .*; do/ {
      sub(/^.*for svc in /, "")
      sub(/; do.*$/, "")
      print
      exit
    }
  ' "$DEPLOY_INFRA_SH" | tr ' ' '\n' | grep -v '^$' | sort -u
}

# read_set prints one service per line: the distinct spec-service column of the
# SERVICE_TENANTS table.
read_set() {
  service_tenants_column 2 | sort -u
}

# ---------------------------------------------------------------------------
# Test 1: both sets are readable
# ---------------------------------------------------------------------------
# An awk range that stops matching (a renamed function, a reformatted loop) or a
# table that no longer parses would make every comparison below pass on two
# empty sets.
test_both_sets_are_readable() {
  echo "Test: the wait set and the SERVICE_TENANTS column are both readable"

  assert_not_empty "the wait set is found in openbao_onboard_database_tenant" "$(wait_set)"
  assert_not_empty "the spec-service column is found in SERVICE_TENANTS" "$(read_set)"
}

# ---------------------------------------------------------------------------
# Test 2: every service of the table is waited for
# ---------------------------------------------------------------------------
test_every_table_service_is_waited_for() {
  echo "Test: every SERVICE_TENANTS spec-service is in the wait set"

  local waited svc missing=""
  waited="$(wait_set)"
  while read -r svc; do
    if ! grep -qx "$svc" <<<"$waited"; then
      missing+="wait set lacks $svc"$'\n'
    fi
  done <<<"$(read_set)"

  assert_eq "the wait set covers every spec-service of the table" "" "$missing"
}

# ---------------------------------------------------------------------------
# Test 3: the wait set names no service the table never reads
# ---------------------------------------------------------------------------
test_the_wait_set_names_no_stranger() {
  echo "Test: the wait set names no service outside SERVICE_TENANTS"

  local read_services svc extra=""
  read_services="$(read_set)"
  while read -r svc; do
    if ! grep -qx "$svc" <<<"$read_services"; then
      extra+="wait set names $svc, which no SERVICE_TENANTS row reads"$'\n'
    fi
  done <<<"$(wait_set)"

  assert_eq "the wait set stays inside the table" "" "$extra"
}

# ---------------------------------------------------------------------------
# Test 4: the waited namespaces come from one read of the ControlPlane
# ---------------------------------------------------------------------------
# openbao_onboard_database_tenant runs against a recording kubectl stub that
# answers `get controlplane -o json` with the CR below and succeeds every
# MariaDB lookup, wait, and root-token read. REPO_ROOT points at a tenant-script
# stub, so the run stops at the onboarding call. Each extra read of the same
# CR is another process and another API request per service on every
# WITH_CONTROLPLANE onboarding.
test_wait_namespaces_come_from_one_controlplane_read() {
  echo "Test: the wait namespaces are resolved from a single ControlPlane read"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # keystone and glance in namespaces of their own; placement as an empty block
  # (present, default namespace); barbican on a dedicated database (skipped);
  # cinder on a dedicated cache only (still on the shared database); nova in
  # glance's namespace (deduplicated); neutron absent.
  cat >"$tmp/controlplane.json" <<'JSON'
{"spec":{"services":{
  "keystone":{"namespace":{"name":"identity"}},
  "glance":{"namespace":{"name":"image"}},
  "placement":{},
  "barbican":{"namespace":{"name":"kms"},"dedicatedBackingServices":{"database":{}}},
  "cinder":{"namespace":{"name":"volume"},"dedicatedBackingServices":{"cache":{}}},
  "nova":{"namespace":{"name":"image"}}
}}}
JSON

  cat >"$tmp/kubectl" <<STUB
#!/bin/bash
printf '%s\n' "\$*" >>"$tmp/calls.log"
if [[ "\$1 \$2" == "get controlplane" ]]; then
  [[ " \$* " == *" -o json "* ]] && cat "$tmp/controlplane.json"
  exit 0
fi
if [[ "\$1 \$2" == "get secret" ]]; then
  printf '{"root_token":"dummy-token"}' | base64
fi
exit 0
STUB
  chmod +x "$tmp/kubectl"
  mkdir -p "$tmp/repo/deploy/openbao/bootstrap"
  printf '#!/bin/bash\nexit 0\n' >"$tmp/repo/deploy/openbao/bootstrap/setup-database-tenant.sh"

  local exit_code
  (
    PATH="$tmp:$PATH"
    export PATH
    # shellcheck source=hack/deploy-infra.sh
    source "$DEPLOY_INFRA_SH"
    REPO_ROOT="$tmp/repo"
    openbao_onboard_database_tenant openstack controlplane
  ) >"$tmp/output.log" 2>&1
  exit_code=$?

  assert_eq "the onboarding succeeds" "0" "$exit_code"
  assert_eq "the ControlPlane is read once" \
    "1" "$(grep -c '^get controlplane ' "$tmp/calls.log")"
  assert_eq "MariaDB is waited for in the ControlPlane's and each shared-database service namespace" \
    "openstack identity image volume" \
    "$(awk '$1 == "wait" && $2 == "mariadb/openstack-db" { printf "%s%s", sep, $4; sep = " " }' "$tmp/calls.log")"
}

# ---------------------------------------------------------------------------
# Test 5: an unreadable ControlPlane stops the onboarding before any wait
# ---------------------------------------------------------------------------
# The kubectl stub fails `get controlplane` (a CR that was never applied, or an
# API server outage during the read) and succeeds everything else. Treating the
# failure as an empty spec would wait only on the ControlPlane's namespace and
# run the tenant script against namespaces nobody waited for.
test_unreadable_controlplane_stops_the_onboarding() {
  echo "Test: an unreadable ControlPlane fails the onboarding before any MariaDB wait"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  cat >"$tmp/kubectl" <<STUB
#!/bin/bash
printf '%s\n' "\$*" >>"$tmp/calls.log"
if [[ "\$1 \$2" == "get controlplane" ]]; then
  echo 'Error from server (NotFound): controlplanes.c5c3.io "controlplane" not found' >&2
  exit 1
fi
if [[ "\$1 \$2" == "get secret" ]]; then
  printf '{"root_token":"dummy-token"}' | base64
fi
exit 0
STUB
  chmod +x "$tmp/kubectl"
  mkdir -p "$tmp/repo/deploy/openbao/bootstrap"
  printf '#!/bin/bash\necho tenant-script-ran\nexit 0\n' >"$tmp/repo/deploy/openbao/bootstrap/setup-database-tenant.sh"

  local exit_code
  (
    PATH="$tmp:$PATH"
    export PATH
    # shellcheck source=hack/deploy-infra.sh
    source "$DEPLOY_INFRA_SH"
    REPO_ROOT="$tmp/repo"
    openbao_onboard_database_tenant openstack controlplane
  ) >"$tmp/output.log" 2>&1
  exit_code=$?

  assert_eq "the onboarding fails" "1" "$exit_code"
  assert_eq "no MariaDB is looked up or waited for" \
    "0" "$(grep -cE '^(get|wait) mariadb/' "$tmp/calls.log")"
  assert_eq "the tenant script does not run" \
    "0" "$(grep -c 'tenant-script-ran' "$tmp/output.log")"
  assert_eq "the log names the unreadable ControlPlane" \
    "1" "$(grep -c "ERROR: ControlPlane 'controlplane' not found in namespace 'openstack'" "$tmp/output.log")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_both_sets_are_readable
test_every_table_service_is_waited_for
test_the_wait_set_names_no_stranger
test_wait_namespaces_come_from_one_controlplane_read
test_unreadable_controlplane_stops_the_onboarding

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
