#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-generate-build-matrix.sh — Generate CI build matrices from release directories.
#
# Scans releases/*/ directories, reads source-refs.yaml to build service×release
# matrices for build, test, and Tempest jobs.
#
# Required env vars:
#   GITHUB_EVENT_NAME — GitHub Actions event type (push, pull_request, etc.)
#
# Optional env vars:
#   SERVICES — Space-separated subset of the services to keep in the service
#              matrices, as hack/ci-resolve-image-changes.sh resolves it. Unset
#              or "all" keeps every service; the empty string keeps none, which
#              the consuming jobs gate on with has-services; an unknown name is
#              a typo worth failing on. The two Tempest matrices are never
#              filtered: the Tempest image is per release, not per service.
#   GITHUB_OUTPUT — GitHub Actions output file (default /dev/null)
#
# Outputs written to GITHUB_OUTPUT and printed to stdout:
#   matrix               — {service, release} pairs for test/verify jobs
#   build-matrix         — {service, release, platform, runner} for build jobs
#   tempest-matrix       — {release, platform, runner} for Tempest build jobs
#   tempest-release-matrix — {release} for Tempest merge jobs
#
# Extracted from inline workflow step to standalone script.

set -euo pipefail

GITHUB_EVENT_NAME="${GITHUB_EVENT_NAME:?GITHUB_EVENT_NAME is required}"
GITHUB_OUTPUT="${GITHUB_OUTPUT:-/dev/null}"

# emit <name> <value> — writes one output line, and echoes it so the script is
# readable when run by hand.
emit() {
  echo "$1=$2" >>"$GITHUB_OUTPUT"
  echo "$1=$2"
}

# ---------------------------------------------------------------------------
# 1. Discover release directories
# ---------------------------------------------------------------------------
shopt -s nullglob
dirs=(releases/*/)

if [[ ${#dirs[@]} -eq 0 ]]; then
  echo "::error::No release directories found under releases/"
  emit matrix '{"include":[]}'
  emit build-matrix '{"include":[]}'
  emit tempest-matrix '{"include":[]}'
  emit tempest-release-matrix '{"include":[]}'
  exit 0
fi

# ---------------------------------------------------------------------------
# 2. Collect {service, release} pairs
# ---------------------------------------------------------------------------
pairs=$(
  for release_dir in "${dirs[@]}"; do
    release="${release_dir%/}"
    release="${release#releases/}"
    while IFS= read -r service; do
      echo "{\"service\":\"${service}\",\"release\":\"${release}\"}"
    done < <(yq 'keys | .[]' "releases/${release}/source-refs.yaml")
  done | jq -sc '.'
)

# ---------------------------------------------------------------------------
# 3. Restrict the pairs to SERVICES
# ---------------------------------------------------------------------------
# A pull request resolves SERVICES to the services whose sources it changed, so
# the matrices carry those alone. Every other invocation leaves it unset and
# gets every pair, which is what the push path publishes and what a local run
# prints. An unknown name is a typo that would otherwise show up as a silently
# missing build leg, so it fails before a single output line is written.
if [[ "${SERVICES-all}" != "all" ]]; then
  # shellcheck disable=SC2086 # deliberate: split the space-separated list
  requested=$(printf '%s\n' ${SERVICES} | jq -Rnc '[inputs | select(length > 0)]')

  known=$(echo "$pairs" | jq -r '[.[].service] | unique | .[]')
  while IFS= read -r name; do
    if ! grep -Fxq -- "$name" <<<"$known"; then
      echo "::error::Unknown service '${name}' in SERVICES (not a key of any releases/*/source-refs.yaml)"
      exit 1
    fi
  done < <(jq -r '.[]' <<<"$requested")

  pairs=$(echo "$pairs" | jq -c --argjson keep "$requested" \
    '[.[] | select(.service as $s | $keep | index($s))]')
fi

emit matrix "$(echo "$pairs" | jq -c '{"include": .}')"

# ---------------------------------------------------------------------------
# 4. Build matrix: {service, release, platform, runner}
# ---------------------------------------------------------------------------
# ARM64 is excluded on pull_request to save CI time.
if [[ "${GITHUB_EVENT_NAME}" == "pull_request" ]]; then
  platforms='[{"platform":"linux/amd64","runner":"ubuntu-latest"}]'
else
  platforms='[{"platform":"linux/amd64","runner":"ubuntu-latest"},{"platform":"linux/arm64","runner":"ubuntu-24.04-arm"}]'
fi

build_matrix=$(echo "$pairs" | jq -c \
  --argjson p "$platforms" \
  '[.[] as $sr | $p[] | {service: $sr.service, release: $sr.release, platform: .platform, runner: .runner}] | {"include": .}')
emit build-matrix "${build_matrix}"

# ---------------------------------------------------------------------------
# 5. Tempest matrix: one image per release (not per service)
# ---------------------------------------------------------------------------
releases=$(
  for release_dir in "${dirs[@]}"; do
    release="${release_dir%/}"
    release="${release#releases/}"
    echo "{\"release\":\"${release}\"}"
  done | jq -sc '.'
)

emit tempest-release-matrix "$(echo "$releases" | jq -c '{"include": .}')"

tempest_matrix=$(echo "$releases" | jq -c \
  --argjson p "$platforms" \
  '[.[] as $r | $p[] | {release: $r.release, platform: .platform, runner: .runner}] | {"include": .}')
emit tempest-matrix "${tempest_matrix}"
