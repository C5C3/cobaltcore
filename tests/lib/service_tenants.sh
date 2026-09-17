#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Shared reader for the SERVICE_TENANTS table of
# deploy/openbao/bootstrap/setup-database-tenant.sh, for the tests that pin
# another file against it (tests/unit/hack/deploy_infra_tenant_wait_set_test.sh,
# tests/unit/deploy/nova_openbao_bootstrap_test.sh).
#
# How to load the table without running the script, and the order of its
# columns, are one piece of knowledge. Kept here so a change to either — a new
# guard the script checks on load, a moved column — is edited once instead of
# once per test.
#
# Source this after tests/lib/assertions.sh, with PROJECT_ROOT set.

# service_tenants_column <1|2|3>
#
# Echo one column of the shipped SERVICE_TENANTS table, one row per line, in
# table order: 1 is the engine role name, 2 the spec-service block the row
# belongs to, 3 the comma-separated schema list. The script is sourced in a
# subshell, where its source guard keeps main from running; BAO_TOKEN and the
# two positional arguments satisfy its <namespace> <controlplane> guards.
service_tenants_column() {
  local column="$1"
  (
    export BAO_TOKEN="dummy-token"
    # shellcheck source=deploy/openbao/bootstrap/setup-database-tenant.sh
    source "$PROJECT_ROOT/deploy/openbao/bootstrap/setup-database-tenant.sh" openstack cp
    local entry fields
    for entry in "${SERVICE_TENANTS[@]}"; do
      read -r -a fields <<<"$entry"
      printf '%s\n' "${fields[column - 1]}"
    done
  )
}
