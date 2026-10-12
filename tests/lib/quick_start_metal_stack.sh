#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# The run-sequence commands of docs/quick-start-metal-stack.md, for the tests
# that keep them on that page alone
# (tests/unit/docs/quick_start_metal_stack_test.sh,
# tests/unit/docs/sizing_calibration_lab_measurement_test.sh).
#
# Kept here so a change to the run sequence is edited once instead of once per
# test.

# shellcheck disable=SC2016,SC2034 # the page's literal text, read by the sourcing tests
METAL_STACK_RUN_SEQUENCE=(
  'kubectl apply -k deploy/lab/metal-stack/controlplane'
  'kubectl apply -k deploy/lab/metal-stack/controlplane-nfs'
  'kubectl apply -k deploy/lab/metal-stack/hypervisor-fixtures'
  'server create "lab-${i}"'
  'server add volume lab-0 lab-vol'
)
