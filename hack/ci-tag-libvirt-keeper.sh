#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-tag-libvirt-keeper.sh — Mint the keeper tag of a merged libvirt image.
#
# Tags the merged libvirt index DIGEST as <version>-r<N>, such as
# 10.0.0-2ubuntu8.19-r1:
#   <version>  the libvirt-daemon-system package version of the image's
#              linux/amd64 variant, read with dpkg-query
#   <N>        the number of commits that touched images/libvirt
# The tag is minted once and never moved: a build whose tag already exists
# leaves it on the digest it names. The lab manifests under
# deploy/lab/metal-stack/ pin that digest, and hack/ghcr-prune-stale-versions.py
# keeps every version that carries a tag of this shape. A registry that cannot
# say whether the tag exists is an error, never a reason to mint: moving an
# existing tag would take the keeper off a pinned digest.
#
# Writes tag=<tag> and minted=true|false to GITHUB_OUTPUT and stdout. A failure
# prints one ::error:: annotation on stderr and exits 1.
#
# Required env vars:
#   IMAGE  — Image name without tag (e.g. ghcr.io/c5c3/libvirt)
#   DIGEST — Digest of the merged index (sha256:<64 hex>)
#
# Optional env vars:
#   LIBVIRT_REPO_DIR — git checkout to count the commits in, with its full
#                      history (default: the repository root)
#   RETRY_DELAY      — seconds before the second read, tripled per read (default: 5)
#   GITHUB_OUTPUT    — step output file (default: /dev/null)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

IMAGE="${IMAGE:?IMAGE is required (e.g. ghcr.io/c5c3/libvirt)}"
DIGEST="${DIGEST:?DIGEST is required (sha256:<64 hex> of the merged index)}"
LIBVIRT_REPO_DIR="${LIBVIRT_REPO_DIR:-${REPO_ROOT}}"
RETRY_DELAY="${RETRY_DELAY:-5}"
GITHUB_OUTPUT="${GITHUB_OUTPUT:-/dev/null}"

err_file="$(mktemp)"
trap 'rm -f "${err_file}"' EXIT

# fail <message> — one ::error:: line on stderr, then exit 1. A message that
# carries a command's multi-line stderr is joined into that one line.
fail() {
  local message="$*"
  echo "::error::${message//$'\n'/ }" >&2
  exit 1
}

# write_outputs <minted> — tag and minted, to GITHUB_OUTPUT and stdout.
write_outputs() {
  printf 'tag=%s\nminted=%s\n' "${tag}" "$1" | tee -a "${GITHUB_OUTPUT}"
}

# inspect_tag — the digest the tag names, on stdout; buildx's stderr goes to
# err_file. Fails when buildx does.
inspect_tag() {
  docker buildx imagetools inspect "${IMAGE}:${tag}" \
    --format '{{json .Manifest.Digest}}' 2>"${err_file}" | tr -d '"'
}

# ---------------------------------------------------------------------------
# 1. The digest names one index
# ---------------------------------------------------------------------------
if [[ ! "${DIGEST}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  fail "DIGEST must be sha256:<64 hex>, got '${DIGEST}'"
fi

# ---------------------------------------------------------------------------
# 2. The image revision: commits under images/libvirt
# ---------------------------------------------------------------------------
# actions/checkout fetches one commit by default, and a shallow count is 1 on
# every build.
if ! shallow="$(git -C "${LIBVIRT_REPO_DIR}" rev-parse --is-shallow-repository 2>"${err_file}")"; then
  fail "cannot read the git history in ${LIBVIRT_REPO_DIR}: $(cat "${err_file}")"
fi
if [[ "${shallow}" == "true" ]]; then
  fail "hack/ci-tag-libvirt-keeper.sh needs the full history to count the commits under images/libvirt (actions/checkout with fetch-depth: 0)"
fi

if ! revision="$(git -C "${LIBVIRT_REPO_DIR}" rev-list --count HEAD -- images/libvirt 2>"${err_file}")"; then
  fail "cannot read the git history in ${LIBVIRT_REPO_DIR}: $(cat "${err_file}")"
fi
if [[ "${revision}" == "0" ]]; then
  fail "no commit touches images/libvirt in ${LIBVIRT_REPO_DIR}"
fi

# ---------------------------------------------------------------------------
# 3. The libvirt package version of the image
# ---------------------------------------------------------------------------
# The image has no entrypoint, so docker run executes dpkg-query directly.
# '${Version}' is dpkg-query's field name and stays unexpanded.
if ! version="$(docker run --rm --platform linux/amd64 "${IMAGE}@${DIGEST}" \
  dpkg-query -W -f='${Version}' libvirt-daemon-system)"; then
  fail "cannot read the libvirt-daemon-system version from ${IMAGE}@${DIGEST}"
fi

# The keeper shape <version>-r<N> has two more copies, which
# tests/unit/hack/ci_tag_libvirt_keeper_test.sh keeps equal to this one: the
# libvirt entry of DEFAULT_KEEP_PATTERNS in hack/ghcr-prune-stale-versions.py
# and allowedVersions of the lab libvirt image in renovate.json. An epoch
# (1:10.0.0-...) or a suffix (+esm1) fits neither, so such a version stops
# here.
version_shape='^[0-9]+(\.[0-9]+)+-[0-9]+ubuntu[0-9]+(\.[0-9]+)*$'
if [[ ! "${version}" =~ ${version_shape} ]]; then
  fail "libvirt-daemon-system version '${version}' is not <upstream>-<n>ubuntu<m>; the keep pattern in hack/ghcr-prune-stale-versions.py and allowedVersions in renovate.json assume that shape"
fi

tag="${version}-r${revision}"

# ---------------------------------------------------------------------------
# 4. An existing tag stays where it is
# ---------------------------------------------------------------------------
if existing="$(inspect_tag)"; then
  echo "${IMAGE}:${tag} already names ${existing}; not moved"
  write_outputs false
  exit 0
fi
# buildx prints "ERROR: <ref>: not found" for a tag the registry does not
# have. Anything else (a 5xx, a denied token) leaves the question open.
if ! grep -q 'not found' "${err_file}"; then
  fail "cannot tell whether ${IMAGE}:${tag} exists: $(cat "${err_file}")"
fi

# ---------------------------------------------------------------------------
# 5. Mint the tag
# ---------------------------------------------------------------------------
# With one source, imagetools create copies the index unchanged, so the tag
# names DIGEST.
if ! docker buildx imagetools create -t "${IMAGE}:${tag}" "${IMAGE}@${DIGEST}"; then
  fail "tagging ${IMAGE}@${DIGEST} as ${tag} failed"
fi

# ---------------------------------------------------------------------------
# 6. Read it back
# ---------------------------------------------------------------------------
# Retried for the reason hack/ci-merge-manifest.sh retries its inspect: ghcr.io
# can answer a read of a tag it has just written with a 404.
attempts=3
delay="${RETRY_DELAY}"
readback=""
for attempt in $(seq 1 "${attempts}"); do
  if readback="$(inspect_tag)" && [[ "${readback}" == "${DIGEST}" ]]; then
    break
  fi
  if [[ "${attempt}" -eq "${attempts}" ]]; then
    fail "${IMAGE}:${tag} names '${readback}', not ${DIGEST}"
  fi
  echo "::warning::${IMAGE}:${tag} names '${readback}' on read ${attempt}/${attempts}; retrying in ${delay}s"
  sleep "${delay}"
  delay=$((delay * 3))
done

# ---------------------------------------------------------------------------
# 7. Outputs
# ---------------------------------------------------------------------------
write_outputs true
