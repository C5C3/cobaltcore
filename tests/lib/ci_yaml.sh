#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Shared block extractors for the tests that assert .github/workflows/ci.yaml
# wiring (tests/unit/ci/*_e2e_matrix_test.sh).
#
# ci.yaml is asserted against by grepping scoped slices of it, and the slicing
# rules are one piece of knowledge: a job body ends at the next 2-space line, a
# step body at the next 6-space one, a matrix entry at the next 10-space one.
# Kept here so a change to those rules — an added indent level, a comment style
# the block terminator has to tolerate — is edited once instead of once per
# service test, where missing a copy leaves an assertion silently matching an
# empty string.
#
# Source this after tests/lib/assertions.sh, with CI_YAML set.

# filter_block <filter-key>
#
# Echo the paths-filter block of the given filter key. Filter keys sit at
# 12-space indent and their entries deeper, so the next key at that indent ends
# the block. Scoping matters for shared entries such as operators/Dockerfile,
# which every operator filter lists.
filter_block() {
  awk -v key="            $1:" '
    $0 == key { in_block = 1; next }
    in_block && /^            [a-z0-9_]+:$/ { exit }
    in_block { print }
  ' "$CI_YAML"
}

# job_block <job>
#
# Echo the body of the top-level ci.yaml job. The block ends at the next
# 2-space line that is not part of the body: the next job key, or the comment
# header introducing it.
job_block() {
  awk -v key="  $1:" '
    $0 == key { in_block = 1; next }
    in_block && /^  [#a-z0-9-]/ { exit }
    in_block { print }
  ' "$CI_YAML"
}

# job_step <job> <step-name>
#
# Echo the body of the named step of the named job. Steps sit at 6-space
# indent, so the next line at that indent ends the block: the following step,
# or the comment introducing it. Scoping to the job matters because
# e2e-operator, e2e-chaos and tempest all carry a "Load E2E images" step.
job_step() {
  job_block "$1" | awk -v key="      - name: $2" '
    $0 == key { in_block = 1; next }
    in_block && /^      [-#]/ { exit }
    in_block { print }
  '
}

# e2e_chaos_matrix_entry <suite>
#
# Echo the e2e-chaos matrix entry whose suite key is <suite>. Entries sit at
# 10-space indent and their keys deeper, so the next line at that indent ends
# the block: the following entry, or the comment introducing it.
e2e_chaos_matrix_entry() {
  job_block e2e-chaos | awk -v key="          - suite: $1" '
    $0 == key { in_block = 1; next }
    in_block && /^          [-#]/ { exit }
    in_block { print }
  '
}
