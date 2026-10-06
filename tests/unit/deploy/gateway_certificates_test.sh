#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the subject of the Gateway certificates. The CA file of the
# metal-stack quick start (docs/quick-start-metal-stack.md, Part 1, Step 7)
# holds every TLS Secret in openstack whose name ends in -nip-io-tls, so the
# suite checks every Certificate of that Secret name in the render of
# deploy/kind/infrastructure, which the lab's infrastructure overlay takes as
# its base, whatever file declares it:
#
#   1. There are twelve, the count the page names, and each has one dnsNames
#      entry and a commonName equal to it.
#   2. No two name the same commonName.
#
# The issuer is self-signed, so a Certificate without a commonName gets an
# empty subject and issuer. OpenSSL finds a trust anchor by its subject and,
# of several such certificates in one CA file, trusts only the first. On the
# lab only the first hostname verified (#1189).
#
# The checks are counted as SKIP when kustomize or yq is not on PATH.
#
# Usage: bash tests/unit/deploy/gateway_certificates_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# shellcheck source=tests/lib/kustomize_render.sh
source "$PROJECT_ROOT/tests/lib/kustomize_render.sh"

INFRASTRUCTURE_DIR="$PROJECT_ROOT/deploy/kind/infrastructure"

# gateway_certificates <yq expression>
# <expression> over every rendered Certificate in openstack whose Secret name
# ends in -nip-io-tls, the Secrets Part 1, Step 7 collects into its CA file.
gateway_certificates() {
  printf '%s\n' "$RENDERED" |
    yq -N -r "select(.kind == \"Certificate\" and .metadata.namespace == \"openstack\"
      and (.spec.secretName | test(\"-nip-io-tls\$\"))) | $1" -
}

# --- Test 1: one hostname per Certificate, named as its subject ---
test_common_name_is_the_hostname() {
  echo "Test: each Gateway Certificate names its one hostname as commonName"
  render "$INFRASTRUCTURE_DIR" 2 || return

  assert_eq "the CA file collects twelve Gateway Certificates" "12" \
    "$(gateway_certificates '.metadata.name' | grep -c .)"
  assert_eq "the Gateway Certificates without one hostname named as commonName" "" \
    "$(gateway_certificates \
      'select((.spec.dnsNames | length) != 1 or .spec.commonName != .spec.dnsNames[0]) | .metadata.name' |
      tr '\n' ' ')"
}

# --- Test 2: the subjects differ ---
test_common_names_differ() {
  echo "Test: no two Gateway Certificates share a commonName"
  render "$INFRASTRUCTURE_DIR" 1 || return

  assert_eq "the commonNames that occur more than once" "" \
    "$(gateway_certificates '.spec.commonName' | sort | uniq -d | tr '\n' ' ')"
}

test_common_name_is_the_hostname
test_common_names_differ

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
