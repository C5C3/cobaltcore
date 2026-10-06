#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Shared reader for the condition table of Step 6 in
# docs/quick-start-controlplane.md, for the tests that pin it
# (tests/unit/docs/quick_start_controlplane_cinder_test.sh,
# tests/unit/docs/quick_start_controlplane_nova_test.sh,
# tests/unit/docs/controlplane_call_order_test.sh).
#
# Kept here so a change to the Step 6 heading or the table's row format is
# edited once instead of once per test.

# step6_chain <doc>
#
# Print the condition names of the Step 6 table in row order, joined with
# ' → ' so an assertion can name a run of neighbours as one string. Prints an
# empty line when the page has no such table.
step6_chain() {
  awk '
    /^## Step 6 / { in_step = 1; next }
    in_step && /^## / { exit }
    in_step && /^\| `[A-Za-z]+Ready` \|/ {
      split($0, cell, "`")
      chain = chain (chain == "" ? "" : " → ") cell[2]
    }
    END { print chain }
  ' "$1"
}
