#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# Inventory the touch points for an OpenStack release across the repo:
# release config files, Tempest config directory, per-release e2e coverage,
# constraint overrides, plus the global default-release decision points.
#
# Usage: inventory-release-touchpoints.sh [<version>]
#
# With <version> (e.g. 2026.2) the inventory targets that release — for a
# release being added, everything is [TODO] by design. Without an argument
# it walks every existing releases/<version>/ directory.
#
# Prints [DONE]/[TODO] per touch point plus decision-point reminders. This
# is an inventory, not a gate: it always exits 0 unless invoked incorrectly.

set -euo pipefail

if [[ $# -gt 1 ]] || { [[ $# -eq 1 ]] && [[ ! "$1" =~ ^[0-9]{4}\.[12]$ ]]; }; then
  echo "usage: $0 [<version>]   (YYYY.N with N in {1,2}, e.g. 2026.2)" >&2
  exit 2
fi

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "${REPO_ROOT}"

DONE_COUNT=0
TODO_COUNT=0

check() { # <description> <command...>
  local desc="$1"
  shift
  if "$@" >/dev/null 2>&1; then
    printf '  [DONE] %s\n' "${desc}"
    DONE_COUNT=$((DONE_COUNT + 1))
  else
    printf '  [TODO] %s\n' "${desc}"
    TODO_COUNT=$((TODO_COUNT + 1))
  fi
}

header() { printf '\n== %s ==\n' "$1"; }

# Services = union of source-refs.yaml keys across all existing releases.
SERVICES="$(cat releases/*/source-refs.yaml 2>/dev/null \
  | grep -E '^[a-z0-9_-]+:' | cut -d: -f1 | sort -u)"
[[ -n "${SERVICES}" ]] || SERVICES="keystone"

# Newest existing release (YYYY.N sorts correctly lexicographically) — used
# to predict upper-constraints pin conflicts for a not-yet-created release.
NEWEST_EXISTING="$(ls -d releases/*/ 2>/dev/null | sed 's|releases/||; s|/||' | sort | tail -1)"

if [[ $# -eq 1 ]]; then
  VERSIONS="$1"
else
  VERSIONS="$(ls -d releases/*/ 2>/dev/null | sed 's|releases/||; s|/||' | sort)"
fi

for v in ${VERSIONS}; do
  slug="${v//./-}"
  printf '\n########## release %s ##########\n' "${v}"

  header "Release config (releases/${v}/)"
  check "releases/${v}/source-refs.yaml (activates build/test matrices)" \
    test -f "releases/${v}/source-refs.yaml"
  check "releases/${v}/test-refs.yaml (tempest + plugin PyPI pins)" \
    test -f "releases/${v}/test-refs.yaml"
  check "releases/${v}/extra-packages.yaml (per-service pip/apt extras)" \
    test -f "releases/${v}/extra-packages.yaml"
  check "releases/${v}/upper-constraints.txt (upstream constraints snapshot)" \
    test -f "releases/${v}/upper-constraints.txt"
  for svc in ${SERVICES}; do
    check "releases/${v}/test-excludes/${svc}.txt (optional stestr excludes)" \
      test -f "releases/${v}/test-excludes/${svc}.txt"
  done

  header "Tempest (hard CI dependency)"
  check "tests/tempest/keystone-${slug}/ directory" \
    test -d "tests/tempest/keystone-${slug}"
  for f in 00-keystone-cr.yaml exclude-tests.txt include-tests.txt tempest.conf; do
    check "tests/tempest/keystone-${slug}/${f}" \
      test -f "tests/tempest/keystone-${slug}/${f}"
  done

  header "Per-release e2e coverage"
  plain_tag="$(sed -nE 's/.*tag:[[:space:]]*"([^"]+)".*/\1/p' \
    tests/e2e/keystone/basic-deployment/00-keystone-cr.yaml 2>/dev/null | head -1)"
  if [[ "${plain_tag}" == "${v}" ]]; then
    printf '  [DONE] covered by the plain basic-deployment suite (tag "%s")\n' "${v}"
    DONE_COUNT=$((DONE_COUNT + 1))
  else
    check "tests/e2e/keystone/basic-deployment-${slug}/ variant (00-keystone-cr.yaml + chainsaw-test.yaml)" \
      test -f "tests/e2e/keystone/basic-deployment-${slug}/chainsaw-test.yaml"
  fi

  header "Constraint overrides (only when a service is pinned upstream)"
  uc="releases/${v}/upper-constraints.txt"
  [[ -f "${uc}" ]] || uc="releases/${NEWEST_EXISTING}/upper-constraints.txt"
  found_pin=0
  for svc in ${SERVICES}; do
    if [[ -f "${uc}" ]] && grep -Eq "^${svc}===" "${uc}"; then
      found_pin=1
      pin="$(grep -E "^${svc}===" "${uc}")"
      printf '  [WARN] %s pins %s — needs a "-%s" line in overrides/%s/constraints.txt (scripts/apply-constraint-overrides.sh)\n' \
        "${uc}" "${pin}" "${svc}" "${v}"
      check "overrides/${v}/constraints.txt" test -f "overrides/${v}/constraints.txt"
    fi
  done
  [[ "${found_pin}" -eq 0 ]] && printf '  [INFO] no service is pinned in %s — no override file needed\n' "${uc}"
done

header "Decision points (global — review, not per-release TODOs)"
printf '  [INFO] default ControlPlane release: deploy/kind/controlplane/controlplane.yaml -> %s\n' \
  "$(sed -nE 's/.*openStackRelease:[[:space:]]*"([^"]+)".*/\1/p' deploy/kind/controlplane/controlplane.yaml | head -1)"
printf '  [INFO] deploy-infra image preload: hack/deploy-infra.sh cp_release -> %s\n' \
  "$(sed -nE 's/.*cp_release="([^"]+)".*/\1/p' hack/deploy-infra.sh | head -1)"
for f in hack/ci-build-service-image.sh hack/ci-build-tempest-image.sh hack/run-tempest.sh; do
  printf '  [INFO] RELEASE fallback: %s -> %s\n' "${f}" \
    "$(sed -nE 's/.*RELEASE:-([0-9]{4}\.[12]).*/\1/p' "${f}" | head -1)"
done
printf '  [INFO] ci.yaml hard-coded image tags: %s\n' \
  "$(grep -oE ':[0-9]{4}\.[12]' .github/workflows/ci.yaml | tr -d ':' | sort -u | tr '\n' ' ')"
printf '  [INFO] renovate regression tests probe: %s\n' \
  "$(grep -ohE 'releases/[0-9]{4}\.[12]' tests/unit/renovate/*_test.sh 2>/dev/null | sort -u | tr '\n' ' ')"
from_ru="$(sed -nE 's/.*tag:[[:space:]]*"([^"]+)".*/\1/p' tests/e2e/keystone/release-upgrade/00-keystone-cr.yaml 2>/dev/null | head -1)"
to_ru="$(sed -nE 's/.*tag:[[:space:]]*"([^"]+)".*/\1/p' tests/e2e/keystone/release-upgrade/01-patch-upgrade.yaml 2>/dev/null | head -1)"
printf '  [INFO] upgrade-path suites currently test: %s -> %s\n' "${from_ru:-?}" "${to_ru:-?}"

printf '\nSummary: %d done, %d todo (inventory only — todo is expected for a release being added)\n' \
  "${DONE_COUNT}" "${TODO_COUNT}"
exit 0
