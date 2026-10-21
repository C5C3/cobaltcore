#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-operator-upgrade-main-matrix.sh — Decide which operator upgrade
# suites a push to main runs, and map every image they load.
#
# .github/workflows/operator-upgrade.yaml runs this after a successful CI or
# Build Images push run on main. An operator is in the matrix when that commit
# published its image: merge-operator-images tags every image it publishes
# sha-<HEAD_SHA>, so the tag's presence answers the question. The suites then
# upgrade from their pinned baseline to exactly that image.
#
# A pin equal to the commit's Chart.yaml version is left out with a notice.
# The same merge-operator-images run tagged the commit's image with that
# version, and helm-push pushed the commit's chart under it, so the baseline
# is the commit itself and the suite would only restart the operator. The
# operator joins the matrix once a Chart.yaml bump freezes its pin.
#
# The image map (the image-map input of .github/actions/load-e2e-images) sends
# each operator's <prefix>/<op>-operator:dev to the digest the commit published,
# or to <prefix>/<op>-operator:latest for an operator it did not publish (a
# sibling a suite runs beside its own operator). Every other ref the leg table
# hack/ci-operator-upgrade-leg.sh names for a published operator is a tag main
# publishes and maps to itself, once the registry confirms it exists. A ref
# that does not exist yet drops every operator whose suite needs it: Build
# Images publishes service and OVN images after CI often enough, and the run
# its completion triggers covers them.
#
# Required env vars:
#   HEAD_SHA            — The commit the triggering run built (40 hex digits)
#
# Optional env vars:
#   IMAGE_PREFIX        — Registry and owner (default: ghcr.io/c5c3)
#   IMAGE_INSPECT_CMD   — Registry inspect command, default
#                         "docker buildx imagetools inspect". The unit test
#                         points it at a stub
#   INSPECT_RETRY_DELAY — Seconds before the second inspect attempt, tripled for
#                         the third (default 5)
#   GITHUB_OUTPUT       — Output file the values are appended to (default:
#                         /dev/null)
#
# Writes to GITHUB_OUTPUT and to stdout:
#   operators     — {"operator":[...]} in the order of the
#                   operators/*/helm/*-operator glob, or {"operator":["__none__"]}
#   has-operators — "true" or "false"
#   image-map     — compact JSON object, canonical ref to the ref to pull
#
# A registry that keeps failing fails the script: an operator is never skipped
# because its tag could not be read. Requires jq on PATH.
#
# set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
LEG_SH="${SCRIPT_DIR}/ci-operator-upgrade-leg.sh"
READ_VERSION_SH="${SCRIPT_DIR}/ci-read-chart-version.sh"

# ---------------------------------------------------------------------------
# 1. Inputs, all checked before the first inspect
# ---------------------------------------------------------------------------
HEAD_SHA="${HEAD_SHA:-}"
if [[ ! "${HEAD_SHA}" =~ ^[0-9a-f]{40}$ ]]; then
  echo "::error::HEAD_SHA must be a 40-character commit SHA (got: '${HEAD_SHA}')"
  exit 1
fi
IMAGE_PREFIX="${IMAGE_PREFIX:-ghcr.io/c5c3}"
read -ra INSPECT_CMD <<<"${IMAGE_INSPECT_CMD:-docker buildx imagetools inspect}"
INSPECT_RETRY_DELAY="${INSPECT_RETRY_DELAY:-5}"
if [[ ! "${INSPECT_RETRY_DELAY}" =~ ^[0-9]+$ ]]; then
  echo "::error::INSPECT_RETRY_DELAY must be a whole number of seconds (got: '${INSPECT_RETRY_DELAY}')"
  exit 1
fi
GITHUB_OUTPUT="${GITHUB_OUTPUT:-/dev/null}"
INSPECT_ATTEMPTS=3

shopt -s nullglob
charts=("${REPO_ROOT}"/operators/*/helm/*-operator/Chart.yaml)
shopt -u nullglob
if [[ ${#charts[@]} -eq 0 ]]; then
  echo "::error::no operator chart found under operators/*/helm/"
  exit 1
fi
operators=()
for chart in "${charts[@]}"; do
  op="${chart#"${REPO_ROOT}/operators/"}"
  operators+=("${op%%/*}")
done

INSPECT_STDERR="$(mktemp)"
trap 'rm -f "${INSPECT_STDERR}"' EXIT

# ---------------------------------------------------------------------------
# 2. Registry lookups
# ---------------------------------------------------------------------------
DIGEST=""

# inspect <ref> — set DIGEST to the index digest <ref> names and return 0, or
# return 1 when the registry has no such ref. The loop is resolve_digest() of
# hack/ci-resolve-e2e-images.sh without its revision mode; keep the two in
# step. A missing tag is an answer, so it is not retried; any other failure is
# retried twice before the script fails.
inspect() {
  local ref="$1" attempt delay out
  delay="${INSPECT_RETRY_DELAY}"
  for attempt in $(seq 1 "${INSPECT_ATTEMPTS}"); do
    if out=$("${INSPECT_CMD[@]}" "${ref}" --format '{{json .Manifest.Digest}}' 2>"${INSPECT_STDERR}"); then
      DIGEST=$(tr -d '"' <<<"${out}")
      if [[ ! "${DIGEST}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
        echo "::error::unexpected digest for ${ref}: ${DIGEST}"
        exit 1
      fi
      return 0
    fi
    if grep -q 'not found' "${INSPECT_STDERR}"; then
      return 1
    fi
    if [[ "${attempt}" -lt "${INSPECT_ATTEMPTS}" ]]; then
      echo "::warning::inspect of ${ref} failed (attempt ${attempt}/${INSPECT_ATTEMPTS}); retrying in ${delay}s"
      sleep "${delay}"
      delay=$((delay * 3))
    fi
  done
  echo "::error::cannot tell whether ${ref} exists: $(tail -n1 "${INSPECT_STDERR}")"
  exit 1
}

# ---------------------------------------------------------------------------
# 3. Which operators this commit published
# ---------------------------------------------------------------------------
map_lines=()
published=()
operator_dev_refs=""
for op in "${operators[@]}"; do
  dev_ref="${IMAGE_PREFIX}/${op}-operator:dev"
  operator_dev_refs="${operator_dev_refs}${dev_ref}"$'\n'
  if inspect "${IMAGE_PREFIX}/${op}-operator:sha-${HEAD_SHA}"; then
    echo "${op}-operator: published at ${DIGEST}"
    # Mapped even when the suite is left out below: another selected suite
    # may run this operator as a sibling.
    map_lines+=("${dev_ref}"$'\t'"${IMAGE_PREFIX}/${op}-operator@${DIGEST}")
    pin="$("${READ_VERSION_SH}" "${REPO_ROOT}/tests/e2e-operator-upgrade/${op}-helm-upgrade/baseline.yaml")"
    if [[ -n "${pin}" ]] &&
      [[ "${pin}" == "$("${READ_VERSION_SH}" "${REPO_ROOT}/operators/${op}/helm/${op}-operator/Chart.yaml")" ]]; then
      echo "::notice::${op}-operator: the baseline ${pin} is what ${HEAD_SHA:0:7} published, so the suite would upgrade the commit onto itself; skipped until Chart.yaml moves past ${pin}"
    else
      published+=("${op}")
    fi
  else
    echo "${op}-operator: not published by ${HEAD_SHA:0:7}"
    map_lines+=("${dev_ref}"$'\t'"${IMAGE_PREFIX}/${op}-operator:latest")
  fi
done

# ---------------------------------------------------------------------------
# 4. The other images each published operator's suite loads
# ---------------------------------------------------------------------------
# Each ref is inspected once, however many suites name it.
found_refs=""
missing_refs=""
selected=()
for op in ${published[@]+"${published[@]}"}; do
  if ! leg="$(OPERATOR="${op}" IMAGE_PREFIX="${IMAGE_PREFIX}" GITHUB_OUTPUT=/dev/null "${LEG_SH}")"; then
    exit 1
  fi
  refs="$(sed -n '/^refs<<EOF$/,/^EOF$/p' <<<"${leg}" | sed '1d;$d')"
  missing=""
  while IFS= read -r ref; do
    [[ -n "${ref}" ]] || continue
    if grep -Fxq -- "${ref}" <<<"${operator_dev_refs}"; then
      continue
    fi
    if ! grep -Fxq -- "${ref}" <<<"${found_refs}${missing_refs}"; then
      if inspect "${ref}"; then
        found_refs="${found_refs}${ref}"$'\n'
        map_lines+=("${ref}"$'\t'"${ref}")
      else
        missing_refs="${missing_refs}${ref}"$'\n'
      fi
    fi
    if grep -Fxq -- "${ref}" <<<"${missing_refs}"; then
      echo "::warning::${op}-operator: ${ref} is not published yet; the run that follows Build Images covers it"
      missing=true
    fi
  done <<<"${refs}"
  if [[ -z "${missing}" ]]; then
    selected+=("${op}")
  fi
done

# ---------------------------------------------------------------------------
# 5. Outputs
# ---------------------------------------------------------------------------
if [[ ${#selected[@]} -eq 0 ]]; then
  operators_json='{"operator":["__none__"]}'
  has_operators=false
else
  operators_json=$(printf '%s\n' "${selected[@]}" | jq -Rnc '{operator: [inputs | select(length > 0)]}')
  has_operators=true
fi
image_map=$(printf '%s\n' "${map_lines[@]}" |
  jq -Rsc 'split("\n") | map(select(length > 0) | split("\t") | {key: .[0], value: .[1]}) | from_entries')

{
  echo "operators=${operators_json}"
  echo "has-operators=${has_operators}"
  echo "image-map=${image_map}"
} | tee -a "${GITHUB_OUTPUT}"
