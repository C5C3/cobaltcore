#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the workflows push every Helm chart through hack/ci-helm-push.sh, and
# that every azure/setup-helm step installs the HELM_VERSION pin.
#
# A bare `helm push` fails the job whenever ghcr.io 404s the manifest helm has
# just uploaded and fetches back to tag it, as in runs 39266486878
# (helm-push (horizon)) and 39922685226 (helm-push (placement)): the manifest
# lands, the chart version keeps pointing at the previous push, and the job is
# red. A push step added later with a plain `helm push` brings that back, and
# only on main, where no pull request run shows it first.
#
# A setup-helm step without `version:` installs whatever helm release is newest
# when the job starts, so the push path above could change under a run with no
# commit to blame. Renovate moves the one pin in ci.yaml's env instead.
#
# Usage: bash tests/unit/ci/helm_wiring_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

test_chart_pushes_use_the_script() {
  echo "Test: helm-push and helm-push-target-cluster push through hack/ci-helm-push.sh"

  local job step
  for job in helm-push helm-push-target-cluster; do
    step="$(job_step "$job" "Package and push Helm chart")"
    assert_not_empty "$job has a Package and push Helm chart step" "$step"
    assert_contains "$job pushes through hack/ci-helm-push.sh" "$step" \
      'hack/ci-helm-push.sh '
  done
}

test_no_bare_helm_push() {
  echo "Test: no workflow or composite action runs helm push directly"

  local bare
  bare="$(grep -rnE '^[^#]*\bhelm push\b' "$PROJECT_ROOT/.github" | sed "s|^$PROJECT_ROOT/||")"
  assert_eq "no uncommented helm push under .github/" "" "$bare"
}

# setup_helm_versions — For every azure/setup-helm step under .github/, echo
# the `version:` input it passes, or <none>.
setup_helm_versions() {
  local f
  for f in "$PROJECT_ROOT"/.github/workflows/*.yaml "$PROJECT_ROOT"/.github/actions/*/action.yaml; do
    [ -f "$f" ] || continue
    awk '
      function flush() { if (in_step) print (version == "" ? "<none>" : version); in_step = 0; version = "" }
      /^ *- / { flush() }
      /^ *(- )?uses: azure\/setup-helm@/ { in_step = 1; next }
      in_step && /^ *version: / { sub(/^ *version: */, ""); version = $0 }
      END { flush() }
    ' "$f"
  done
}

test_setup_helm_steps_pass_the_pin() {
  echo "Test: every azure/setup-helm step installs HELM_VERSION"

  local versions count
  versions="$(setup_helm_versions)"
  count="$(printf '%s\n' "$versions" | grep -c . | tr -d ' ')"
  assert_gte "the workflows have setup-helm steps" "$count" 5
  assert_eq "every setup-helm step passes version: \${{ env.HELM_VERSION }}" \
    '${{ env.HELM_VERSION }}' "$(printf '%s\n' "$versions" | sort -u)"
  assert_not_empty "ci.yaml pins HELM_VERSION" \
    "$(sed -n 's/^  HELM_VERSION: \(v[0-9][0-9.]*\)$/\1/p' "$CI_YAML")"
}

test_chart_pushes_use_the_script
test_no_bare_helm_push
test_setup_helm_steps_pass_the_pin

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
