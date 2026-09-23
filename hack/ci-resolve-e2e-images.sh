#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-resolve-e2e-images.sh — Decide which E2E images a pull request builds
# and which it reuses from what main published.
#
# The key set is every image an E2E consumer can name: one operator image per
# operators/<op>/ that has a go.mod, one service image per (operator, release)
# pair in releases/*/source-refs.yaml, one derived image per release of the
# service it is built from (nova-compute from nova), one Tempest image per
# releases/<release>/, and the release-independent federation proxy. Deriving it from the tree means a
# new operator or release directory extends the set with no workflow edit.
#
# An image whose sources this pull request changed is built in this job and
# pushed under the run-scoped tag. Every other image resolves to the digest
# behind the tag main last published, so the run pulls it instead of spending
# minutes rebuilding it. Consumers keep their canonical local references either
# way: load-e2e-images looks each one up in the map written here.
#
# An operator image is reused only when the commit it was built from, its
# org.opencontainers.image.revision label, has in its history every commit of
# this checkout that touches the operator's sources: the paths of the operator's
# own change filter and of go_common in the changes job of
# .github/workflows/ci.yaml. When one build-and-push leg fails on main,
# merge-operator-images publishes nothing and :latest keeps an older revision;
# such an image is built instead. The check needs the checkout's full history,
# and an image whose revision it cannot find is built too.
#
# Required env vars:
#   IMAGE_PREFIX   — Registry and owner, e.g. ghcr.io/c5c3
#   RUN_TAG        — Run-scoped tag prefix, e.g. e2e-<run_id>
#   GITHUB_OUTPUT  — GitHub Actions output file (set automatically)
#   GITHUB_ENV     — GitHub Actions env file (set automatically)
#
# Optional env vars:
#   CHANGED_OPERATORS   — JSON array of operators whose own sources changed.
#                         Unset, "", [] and ["__none__"] all mean none
#   CHANGED_SERVICES    — JSON array of services whose image sources changed,
#                         same empty forms
#   CHANGED_TEMPEST     — "true" when the Tempest image sources changed
#   CHANGED_PROXY       — "true" when the federation proxy sources changed
#   IMAGE_INSPECT_CMD   — Registry inspect command, default
#                         "docker buildx imagetools inspect". The unit test
#                         points it at a stub
#   INSPECT_RETRY_DELAY — Seconds before the second inspect attempt, tripled for
#                         the third (default 5)
#
# Written to GITHUB_ENV for the build and push steps to loop over:
#   BUILD_OPERATORS        — one operator name per line
#   BUILD_SERVICE_IMAGES   — one "<service> <release> [<image>]" line per
#                            image; the third field names a derived image
#                            built from the service's source, and a line
#                            without it builds the service's own image
#   BUILD_TEMPEST_RELEASES — one release per line
#   BUILD_PROXY            — true or false
#   NEEDS_BASE_IMAGES      — true when a service or Tempest image is built. Both
#                            build FROM python-base and venv-builder; the proxy
#                            builds FROM ubuntu:noble and needs neither
#
# Written to GITHUB_OUTPUT:
#   image-map — JSON object mapping each canonical local reference to the
#               reference to pull for it: <repo>:<RUN_TAG>-<tag> when this run
#               builds it, <repo>@sha256:... when it reuses what main published

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

IMAGE_PREFIX="${IMAGE_PREFIX:?IMAGE_PREFIX is required (e.g. ghcr.io/c5c3)}"
RUN_TAG="${RUN_TAG:?RUN_TAG is required (e.g. e2e-<run_id>)}"
GITHUB_OUTPUT="${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"
GITHUB_ENV="${GITHUB_ENV:?GITHUB_ENV is required}"

read -ra INSPECT_CMD <<<"${IMAGE_INSPECT_CMD:-docker buildx imagetools inspect}"
INSPECT_RETRY_DELAY="${INSPECT_RETRY_DELAY:-5}"
INSPECT_ATTEMPTS=3

INSPECT_STDERR="$(mktemp)"
trap 'rm -f "${INSPECT_STDERR}"' EXIT

# ---------------------------------------------------------------------------
# 1. Inputs
# ---------------------------------------------------------------------------

# parse_changed <env var name> — set PARSED to the names in that JSON array, one
# per line. Unset, "", [] and the __none__ sentinel all yield nothing. Anything
# that is not a JSON array fails the step: the resolver produced this value, so a
# malformed one is a wiring bug rather than user input.
PARSED=""
parse_changed() {
  local var="$1" value="${!1:-}"
  PARSED=""
  if [[ -z "${value}" ]]; then
    return 0
  fi
  if ! jq -e 'type == "array"' <<<"${value}" >/dev/null 2>&1; then
    echo "::error::${var} is not a JSON array"
    exit 1
  fi
  PARSED=$(jq -r '.[]' <<<"${value}" | grep -vx '__none__' || true)
}

parse_changed CHANGED_OPERATORS
changed_operators="${PARSED}"
parse_changed CHANGED_SERVICES
changed_services="${PARSED}"

changed_tempest=false
if [[ "${CHANGED_TEMPEST:-}" == "true" ]]; then
  changed_tempest=true
fi
changed_proxy=false
if [[ "${CHANGED_PROXY:-}" == "true" ]]; then
  changed_proxy=true
fi

# has_line <newline-separated list> <name>
has_line() {
  grep -qxF -- "$2" <<<"$1"
}

# The commit this run tests. On a pull request it is main merged with the
# branch, and a reused operator image has to carry its commits.
if ! HEAD_COMMIT=$(git -C "${REPO_ROOT}" rev-parse --verify HEAD 2>&1); then
  echo "::error::cannot read HEAD of ${REPO_ROOT}: ${HEAD_COMMIT}"
  exit 1
fi

# The change filters of the paths-filter step in the changes job, as YAML.
CHANGE_FILTERS=$(yq '.jobs.changes.steps[] | select(.id == "filter") | .with.filters' \
  "${REPO_ROOT}/.github/workflows/ci.yaml")

# ---------------------------------------------------------------------------
# 2. The key set, derived from the tree
# ---------------------------------------------------------------------------
shopt -s nullglob

operators=()
for operator_dir in "${REPO_ROOT}"/operators/*/; do
  # The rule hack/ci-generate-cleanup-matrix.sh applies: only a directory with a
  # go.mod builds a <name>-operator image.
  [[ -f "${operator_dir}go.mod" ]] || continue
  operator="${operator_dir%/}"
  operators+=("${operator##*/}")
done

releases=()
for release_dir in "${REPO_ROOT}"/releases/*/; do
  release="${release_dir%/}"
  releases+=("${release##*/}")
done

# A changed name with no image here is reported and skipped rather than fatal.
# The resolver's operator list and this tree disagree by design for a directory
# that ships no image of that kind, and a typo should not block the pipeline.
while read -r name; do
  [[ -n "${name}" ]] || continue
  if [[ ! -f "${REPO_ROOT}/operators/${name}/go.mod" ]]; then
    echo "no e2e image for ${name}; ignored"
  fi
done <<<"${changed_operators}"

while read -r name; do
  [[ -n "${name}" ]] || continue
  if [[ -z "$(OPERATOR="${name}" "${REPO_ROOT}/hack/ci-service-image-releases.sh")" ]]; then
    echo "no e2e image for ${name}; ignored"
  fi
done <<<"${changed_services}"

# ---------------------------------------------------------------------------
# 3. Build or reuse, per image
# ---------------------------------------------------------------------------
RESOLVED_DIGEST=""
RESOLVED_REVISION=""

# resolve_digest <published source> [revision] — set RESOLVED_DIGEST to the index
# digest of that source. Given "revision", the same inspect call also sets
# RESOLVED_REVISION to the org.opencontainers.image.revision label the platform
# images of the index agree on, or to "" when they carry none or disagree.
# Returns 1 when the source is not published yet, so the caller builds the image
# instead. Exits the step when the registry keeps failing or answers with
# something that is not a digest.
resolve_digest() {
  local source="$1" mode="${2:-}" format attempt delay out digest
  # No --platform, so this is the multi-arch index digest, the same digest
  # .github/workflows/check-base-image-updates.yaml compares against.
  format='{{json .Manifest.Digest}}'
  if [[ "${mode}" == "revision" ]]; then
    # .Image is the image config of each platform in the index, keyed by
    # platform, which is where build-and-push writes the revision label.
    format='{"digest":{{json .Manifest.Digest}},"image":{{json .Image}}}'
  fi
  delay="${INSPECT_RETRY_DELAY}"
  for attempt in $(seq 1 "${INSPECT_ATTEMPTS}"); do
    if out=$("${INSPECT_CMD[@]}" "${source}" --format "${format}" 2>"${INSPECT_STDERR}"); then
      if [[ "${mode}" == "revision" ]]; then
        digest=$(jq -r '.digest' <<<"${out}" 2>/dev/null) || digest="${out}"
        RESOLVED_REVISION=$(jq -r '[.image[].config.Labels["org.opencontainers.image.revision"] // ""]
          | unique | if length == 1 then .[0] else "" end' <<<"${out}" 2>/dev/null) ||
          RESOLVED_REVISION=""
      else
        digest=$(tr -d '"' <<<"${out}")
      fi
      if [[ ! "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
        echo "::error::unexpected digest for ${source}: ${digest}"
        exit 1
      fi
      RESOLVED_DIGEST="${digest}"
      return 0
    fi
    # A missing tag or package is not a flake. The image has never been
    # published, which is what a new operator, service or release looks like
    # before its first merge to main, so retrying cannot help.
    if grep -q 'not found' "${INSPECT_STDERR}"; then
      return 1
    fi
    if [[ "${attempt}" -lt "${INSPECT_ATTEMPTS}" ]]; then
      echo "::warning::inspect of ${source} failed (attempt ${attempt}/${INSPECT_ATTEMPTS}); retrying in ${delay}s"
      sleep "${delay}"
      delay=$((delay * 3))
    fi
  done
  echo "::error::cannot resolve ${source} after ${INSPECT_ATTEMPTS} attempts"
  exit 1
}

PATHSPECS=()

# add_pathspecs <change filter> — append the paths of that change filter to
# PATHSPECS as git pathspecs. Exits the step when ci.yaml has no such filter,
# since the commits under its paths would then never count against an image.
add_pathspecs() {
  local filter="$1" patterns pattern
  patterns=$(FILTER="${filter}" yq '.[strenv(FILTER)] // [] | .[]' <<<"${CHANGE_FILTERS}")
  if [[ -z "${patterns}" ]]; then
    echo "::error::.github/workflows/ci.yaml has no change filter named ${filter}"
    exit 1
  fi
  while read -r pattern; do
    # git's glob magic reads ** the way paths-filter does.
    PATHSPECS+=(":(glob)${pattern}")
  done <<<"${patterns}"
}

# image_is_current <published source> <operator> <canonical key> — succeed when
# RESOLVED_REVISION, the commit that source was built from, has in its history
# every commit of HEAD that touches the operator's sources. Otherwise print a
# notice that says why and fail, so the caller builds the image.
image_is_current() {
  local source="$1" operator="$2" key="$3" missing
  if [[ ! "${RESOLVED_REVISION}" =~ ^[0-9a-f]{40}$ ]]; then
    echo "::notice::${source} carries no revision label to check; building ${key}"
    return 1
  fi
  if ! git -C "${REPO_ROOT}" cat-file -e "${RESOLVED_REVISION}^{commit}" 2>/dev/null; then
    echo "::notice::${source} is at revision ${RESOLVED_REVISION}, which this checkout does not have; building ${key}"
    return 1
  fi
  PATHSPECS=()
  add_pathspecs "${operator}"
  add_pathspecs go_common
  # A commit the pull request itself adds under these paths already put the
  # operator in CHANGED_OPERATORS, so what is left to count comes from main.
  if ! missing=$(git -C "${REPO_ROOT}" rev-list --count \
    "${RESOLVED_REVISION}..${HEAD_COMMIT}" -- "${PATHSPECS[@]}"); then
    echo "::error::cannot count the commits between ${RESOLVED_REVISION} and ${HEAD_COMMIT}"
    exit 1
  fi
  if [[ "${missing}" -gt 0 ]]; then
    echo "::notice::${source} is stale: revision ${RESOLVED_REVISION} lacks ${missing} commit(s) touching the ${operator} sources; building ${key}"
    return 1
  fi
}

map_lines=()
log_lines=()
ENTRY_BUILT=false

# add_entry <canonical key> <published source> <true when the run builds it> [operator]
# Appends this key's map entry and sets ENTRY_BUILT to what the key ended up
# being, which is "true" for a source that is not published yet and, given the
# operator the image is built from, for an image older than that operator's
# sources.
add_entry() {
  local key="$1" source="$2" built="$3" operator="${4:-}"
  local repo="${key%:*}" tag="${key##*:}" mode=""

  ENTRY_BUILT="${built}"
  if [[ "${built}" != "true" ]]; then
    if [[ -n "${operator}" ]]; then
      mode=revision
    fi
    if resolve_digest "${source}" "${mode}"; then
      if [[ -z "${operator}" ]] || image_is_current "${source}" "${operator}" "${key}"; then
        map_lines+=("${key}"$'\t'"${repo}@${RESOLVED_DIGEST}")
        log_lines+=("${key} -> ${repo}@${RESOLVED_DIGEST} (reused)")
        return 0
      fi
    else
      echo "::notice::${source} is not published yet; building ${key}"
    fi
    ENTRY_BUILT=true
  fi
  # The exact reference the push step derives from the canonical one.
  map_lines+=("${key}"$'\t'"${repo}:${RUN_TAG}-${tag}")
  log_lines+=("${key} -> ${repo}:${RUN_TAG}-${tag} (built)")
}

build_operators=()
build_service_images=()
build_tempest_releases=()
build_proxy=false

for operator in "${operators[@]}"; do
  built=false
  if has_line "${changed_operators}" "${operator}"; then
    built=true
  fi
  add_entry "${IMAGE_PREFIX}/${operator}-operator:dev" \
    "${IMAGE_PREFIX}/${operator}-operator:latest" "${built}" "${operator}"
  if [[ "${ENTRY_BUILT}" == "true" ]]; then
    build_operators+=("${operator}")
  fi
done

for operator in "${operators[@]}"; do
  built=false
  if has_line "${changed_services}" "${operator}"; then
    built=true
  fi
  while read -r release; do
    [[ -n "${release}" ]] || continue
    add_entry "${IMAGE_PREFIX}/${operator}:${release}" \
      "${IMAGE_PREFIX}/${operator}:${release}" "${built}"
    if [[ "${ENTRY_BUILT}" == "true" ]]; then
      build_service_images+=("${operator} ${release}")
    fi
  done < <(OPERATOR="${operator}" "${REPO_ROOT}/hack/ci-service-image-releases.sh")
done

# The derived image nova-compute: an image built from nova's source, patches
# and constraints under a name of its own (images/nova-compute/). It is built
# whenever nova's image is, since both come from the same source, and otherwise
# reused from what main published under its own name.
while read -r release; do
  [[ -n "${release}" ]] || continue
  built=false
  if has_line "$(printf '%s\n' ${build_service_images[@]+"${build_service_images[@]}"})" "nova ${release}"; then
    built=true
  fi
  add_entry "${IMAGE_PREFIX}/nova-compute:${release}" \
    "${IMAGE_PREFIX}/nova-compute:${release}" "${built}"
  if [[ "${ENTRY_BUILT}" == "true" ]]; then
    build_service_images+=("nova ${release} nova-compute")
  fi
done < <(OPERATOR=nova "${REPO_ROOT}/hack/ci-service-image-releases.sh")

for release in "${releases[@]}"; do
  add_entry "${IMAGE_PREFIX}/tempest:${release}" \
    "${IMAGE_PREFIX}/tempest:${release}" "${changed_tempest}"
  if [[ "${ENTRY_BUILT}" == "true" ]]; then
    build_tempest_releases+=("${release}")
  fi
done

add_entry "${IMAGE_PREFIX}/keystone-federation-proxy:dev" \
  "${IMAGE_PREFIX}/keystone-federation-proxy:latest" "${changed_proxy}"
if [[ "${ENTRY_BUILT}" == "true" ]]; then
  build_proxy=true
fi

# ---------------------------------------------------------------------------
# 4. Emit
# ---------------------------------------------------------------------------
needs_base_images=false
if [[ ${#build_service_images[@]} -gt 0 || ${#build_tempest_releases[@]} -gt 0 ]]; then
  needs_base_images=true
fi

# emit_block <name> [value...] — a GITHUB_ENV heredoc block, empty when the run
# builds nothing of that kind.
emit_block() {
  local name="$1"
  shift
  {
    echo "${name}<<EOF"
    if [[ $# -gt 0 ]]; then
      printf '%s\n' "$@"
    fi
    echo "EOF"
  } >>"${GITHUB_ENV}"
}

emit_block BUILD_OPERATORS "${build_operators[@]}"
emit_block BUILD_SERVICE_IMAGES "${build_service_images[@]}"
emit_block BUILD_TEMPEST_RELEASES "${build_tempest_releases[@]}"
{
  echo "BUILD_PROXY=${build_proxy}"
  echo "NEEDS_BASE_IMAGES=${needs_base_images}"
} >>"${GITHUB_ENV}"

image_map=$(printf '%s\n' "${map_lines[@]}" |
  jq -Rsc 'split("\n") | map(select(length > 0) | split("\t") | {key: .[0], value: .[1]}) | from_entries')
echo "image-map=${image_map}" >>"${GITHUB_OUTPUT}"

echo "::group::Image map"
printf '%s\n' "${log_lines[@]}"
echo "::endgroup::"
