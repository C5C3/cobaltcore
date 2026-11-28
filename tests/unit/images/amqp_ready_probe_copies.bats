#!/usr/bin/env bats
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify images/nova/nova-amqp-ready stays a copy of
# images/cinder/cinder-amqp-ready.
#
# The two images are independent build contexts, so each carries its own copy
# of the exec readiness probe, and nothing else ties the copies together. A
# fix to one of them (another /proc race, a new table format) would reach only
# that image, and the other image's pods would stay NotReady or report ready
# wrongly. With comment and blank lines stripped and the service name read as
# nova, the two scripts have to be the same code; the comments may differ.
#
# Usage: bin/bats tests/unit/images/amqp_ready_probe_copies.bats

load '../../lib/bats/common'

CINDER_PROBE="images/cinder/cinder-amqp-ready"
NOVA_PROBE="images/nova/nova-amqp-ready"

# probe_code <path> prints the code of a probe: the shebang and every line that
# is neither a comment nor blank, with the service name read as nova.
probe_code() {
  awk 'NR == 1 || !/^[[:space:]]*(#|$)/' "$PROJECT_ROOT/$1" |
    sed -e 's/CINDER/NOVA/g' -e 's/cinder/nova/g'
}

# --- Test 1: the two probes are the same code ---
@test "nova-amqp-ready carries the code of cinder-amqp-ready" {
  local cinder_code nova_code
  cinder_code=$(probe_code "$CINDER_PROBE")
  nova_code=$(probe_code "$NOVA_PROBE")

  assert_not_empty "$CINDER_PROBE carries code" "$cinder_code"
  assert_not_empty "$NOVA_PROBE carries code" "$nova_code"
  assert_eq "$NOVA_PROBE and $CINDER_PROBE differ only in comments and the service name" \
    "" "$(diff <(printf '%s\n' "$cinder_code") <(printf '%s\n' "$nova_code"))"
}
