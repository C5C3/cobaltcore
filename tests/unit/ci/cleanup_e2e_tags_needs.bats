#!/usr/bin/env bats
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify that cleanup-e2e-tags in .github/workflows/ci.yaml waits for every job
# that pulls the run-scoped e2e images.
#
# cleanup-e2e-tags deletes the e2e-${run_id}-* tags build-e2e-images pushed,
# and starts as soon as the jobs in its `needs` have finished. A job that loads
# the images through the load-e2e-images action and is missing from that list
# races the deletion: on a run that skips every listed consumer, the cleanup
# starts the moment build-e2e-images ends and the job's pull fails with
# "manifest unknown". The list was written by hand and missed six jobs added
# after it, so this test derives the consumers from the workflow instead.
#
# Usage: bin/bats tests/unit/ci/cleanup_e2e_tags_needs.bats

load '../../lib/bats/common'

# shellcheck disable=SC2034 # read by tests/lib/ci_yaml.sh
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Echo, one per line, every top-level job whose body uses the load-e2e-images
# action.
image_consumers() {
  awk '
    /^  [a-z0-9-]+:$/ { job = substr($1, 1, length($1) - 1); next }
    /uses: \.\/\.github\/actions\/load-e2e-images$/ && job != "" { print job }
  ' "$CI_YAML" | sort -u
}

# Echo, one per line, the jobs in cleanup-e2e-tags' one-line `needs` list.
cleanup_needs() {
  job_block cleanup-e2e-tags \
    | sed -n 's/^    needs: \[\(.*\)\]$/\1/p' \
    | tr ',' '\n' | tr -d ' ' | sed '/^$/d' | sort -u
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

@test "the consumer scan finds the e2e jobs" {
  local consumers count
  consumers="$(image_consumers)"
  count="$(grep -c . <<<"${consumers}")"
  # The scan finding nothing would make the next test pass vacuously.
  assert_gte "at least ten jobs load the e2e images" "${count}" 10
  assert_eq "e2e-operator loads the e2e images" "e2e-operator" "$(grep -x e2e-operator <<<"${consumers}")"
  assert_eq "tempest loads the e2e images" "tempest" "$(grep -x tempest <<<"${consumers}")"
}

@test "cleanup-e2e-tags' needs list is read" {
  local needs
  needs="$(cleanup_needs)"
  assert_eq "the needs list names build-e2e-images" "build-e2e-images" "$(grep -x build-e2e-images <<<"${needs}")"
}

@test "cleanup-e2e-tags waits for every job that loads the e2e images" {
  local needs job
  needs="$(cleanup_needs)"
  while IFS= read -r job; do
    [[ -n "${job}" ]] || continue
    if grep -qx -- "${job}" <<<"${needs}"; then
      check_passed "cleanup-e2e-tags waits for ${job}"
    else
      soft fail "cleanup-e2e-tags does not wait for ${job}, which loads the e2e images"
    fi
  done < <(image_consumers)
}
