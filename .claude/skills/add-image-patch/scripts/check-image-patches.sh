#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# check-image-patches.sh — mechanical checks over the downstream source
# patches under patches/<svc>/<release>/NNNN-<slug>.patch that both service
# image build paths apply with `git apply` before installing:
#   P1  layout and naming: patches/<svc>/<release>/<file>, the file name
#       matches ^[0-9]{4}-[a-z0-9-]+\.patch$ (the build globs *.patch only)
#   P2  wiring: <release> exists under releases/, <svc> is a key of that
#       release's source-refs.yaml, and patches/<svc>/** is in the
#       build-images.yaml and ci.yaml paths filters
#   P3  format: the file carries a diff (diff --git, or ---/+++ headers); the
#       house header before it has the SPDX pair, an "Upstream status:" line,
#       and names the upstream tag source-refs.yaml pins
#   P4  numbering: gap-free from 0001 per directory, no duplicate numbers
#   P5  mirroring: the same slug in the service's other releases ([INFO])
#   P6  docs: the file name appears in docs/reference/ci-cd/container-images.md
#   P7  tests: a patch that changes non-test code and no test file must say
#       in its header why no upstream test pins the behaviour ([WARN])
#   P8  image proof: a patch that changes non-test code is named by
#       tests/container-images/verify_<svc>.sh ([WARN])
#   P9  (--apply, network) shallow-clone openstack/<svc> at the pinned tag into
#       a temporary directory outside the repo and apply every patch in build
#       order with `git apply --check`; a patch that only reverse-applies is
#       already upstream and should be retired
#
# Usage: check-image-patches.sh [--apply] [--service <svc>] [--release <rel>]
#
# Read-only for the repository: --apply works in a mktemp directory that is
# removed on exit. Exit code 1 on any [FAIL].

set -euo pipefail
# Untranslated git messages and byte-order sorting (the glob order the build
# applies patches in).
export LC_ALL=C

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "${REPO_ROOT}"

DOCS="docs/reference/ci-cd/container-images.md"
BUILD_WF=".github/workflows/build-images.yaml"
CI_WF=".github/workflows/ci.yaml"
NAME_RE='^[0-9]{4}-[a-z0-9-]+\.patch$'
TEST_PATH_RE='(^|/)tests?/|(^|/)test_[^/]*\.py$|_test\.py$'
# Header phrases that explain a missing test hunk (case-insensitive).
NO_TEST_RE='no test hunk|carries no test|needs no test|no upstream test|no test (pins|covers|asserts)|stays green|test-only'

APPLY=0
ONLY_SVC=""
ONLY_REL=""

usage() {
  echo "usage: $0 [--apply] [--service <svc>] [--release <rel>]" >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --apply) APPLY=1 ;;
    --service) [ $# -ge 2 ] || usage; ONLY_SVC="$2"; shift ;;
    --release) [ $# -ge 2 ] || usage; ONLY_REL="$2"; shift ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
  shift
done

FAIL_COUNT=0
WARN_COUNT=0
fail() { echo "[FAIL] $*"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
warn() { echo "[WARN] $*"; WARN_COUNT=$((WARN_COUNT + 1)); }
pass() { echo "[PASS] $*"; }
info() { echo "[INFO] $*"; }
hdr()  { echo; echo "=== $* ==="; }

# pinned_tag <svc> <release> — the source-refs.yaml value, quotes stripped.
pinned_tag() {
  awk -v k="$1" -F: '$1 == k { v = $2; sub(/^[[:space:]]*"?/, "", v); sub(/"?[[:space:]]*$/, "", v); print v; exit }' \
    "releases/$2/source-refs.yaml" 2> /dev/null || true
}

# header <patch> — the prose before the first diff.
header() {
  awk '/^diff --git |^--- (a\/|\/dev\/null)/ { exit } { print }' "$1"
}

# touched <patch> — the post-image paths the patch changes.
touched() {
  awk '/^diff --git / { p = $4; sub(/^b\//, "", p); print p; seen = 1; next }
       !seen && /^\+\+\+ / { p = $2; sub(/^b\//, "", p); print p }' "$1" | sort -u
}

# Every tracked or new (not ignored) file under patches/.
ALL_FILES="$(git ls-files --cached --others --exclude-standard -- patches | sort -u)"
if [ -z "${ALL_FILES}" ]; then
  info "no files under patches/ — nothing to check"
  exit 0
fi

# Keep the files the --service / --release filters select.
FILES=""
for f in ${ALL_FILES}; do
  rest="${f#patches/}"
  svc="${rest%%/*}"
  rel_rest="${rest#*/}"
  rel="${rel_rest%%/*}"
  [ -n "${ONLY_SVC}" ] && [ "${svc}" != "${ONLY_SVC}" ] && continue
  [ -n "${ONLY_REL}" ] && [ "${rel}" != "${ONLY_REL}" ] && continue
  FILES="${FILES} ${f}"
done
PATCHES=""
DIRS=""

# ---------------------------------------------------------------------------
# P1 — layout and naming
# ---------------------------------------------------------------------------
hdr "P1: layout patches/<svc>/<release>/NNNN-<slug>.patch"
for f in ${FILES}; do
  depth="$(printf '%s' "${f}" | awk -F/ '{ print NF }')"
  base="$(basename "${f}")"
  if [ "${depth}" -ne 4 ]; then
    fail "${f}: not at patches/<svc>/<release>/<file> — neither build path looks there"
    continue
  fi
  if grep -Eq "${NAME_RE}" <<<"${base}"; then
    pass "${f}"
    PATCHES="${PATCHES} ${f}"
    d="$(dirname "${f}")"
    case " ${DIRS} " in *" ${d} "*) ;; *) DIRS="${DIRS} ${d}" ;; esac
  elif [ "${base%.patch}" != "${base}" ]; then
    fail "${f}: name does not match NNNN-<lowercase-slug>.patch"
    PATCHES="${PATCHES} ${f}"
    d="$(dirname "${f}")"
    case " ${DIRS} " in *" ${d} "*) ;; *) DIRS="${DIRS} ${d}" ;; esac
  else
    fail "${f}: not a .patch file — the build applies *.patch only and derive-service-tags counts *.patch for the p<N> tag"
  fi
done

# ---------------------------------------------------------------------------
# P2 — wiring into the release config and the CI paths filters
# ---------------------------------------------------------------------------
hdr "P2: release, source-refs key, and CI paths filters"
SVCS=""
for d in ${DIRS}; do
  rest="${d#patches/}"
  svc="${rest%%/*}"
  rel="${rest#*/}"
  case " ${SVCS} " in *" ${svc} "*) ;; *) SVCS="${SVCS} ${svc}" ;; esac
  if [ ! -d "releases/${rel}" ]; then
    fail "${d}: release ${rel} does not exist under releases/ — no matrix leg ever applies these patches"
    continue
  fi
  tag="$(pinned_tag "${svc}" "${rel}")"
  if [ -z "${tag}" ]; then
    fail "${d}: ${svc} is not a key of releases/${rel}/source-refs.yaml — the service is never built for ${rel}"
  else
    pass "${d}: ${svc} ${tag} pinned in releases/${rel}/source-refs.yaml"
  fi
done
for svc in ${SVCS}; do
  for wf in "${BUILD_WF}" "${CI_WF}"; do
    if grep -qF "'patches/${svc}/**'" "${wf}"; then
      pass "${wf} filters on patches/${svc}/**"
    else
      fail "${wf}: no 'patches/${svc}/**' paths filter — a patch-only PR would not rebuild or retest ${svc}"
    fi
  done
done

# ---------------------------------------------------------------------------
# P3 — diff content and the house header
# ---------------------------------------------------------------------------
hdr "P3: diff content and header (SPDX, Upstream status, pinned tag)"
for f in ${PATCHES}; do
  rest="${f#patches/}"
  svc="${rest%%/*}"
  rel_rest="${rest#*/}"
  rel="${rel_rest%%/*}"
  if grep -q '^diff --git a/' "${f}"; then
    pass "${f}: git diff ($(touched "${f}" | grep -c . || true) file(s))"
  elif grep -q '^--- ' "${f}" && grep -q '^+++ ' "${f}"; then
    pass "${f}: plain unified diff"
  else
    fail "${f}: no diff header — git apply reports 'No valid patches in input'"
    continue
  fi
  hbuf="$(header "${f}")"
  if grep -Eq '^From [0-9a-f]{40} |^Subject: ' <<< "${hbuf}"; then
    info "${f}: keeps the git format-patch mail envelope — the house style is SPDX pair, subject line, prose"
  fi
  if ! grep -q 'SPDX-FileCopyrightText' <<< "${hbuf}" \
    || ! grep -q 'SPDX-License-Identifier' <<< "${hbuf}"; then
    warn "${f}: header lacks the SPDX-FileCopyrightText / SPDX-License-Identifier pair every patch carries"
  fi
  if ! grep -q '^Upstream status:' <<< "${hbuf}"; then
    warn "${f}: header has no 'Upstream status:' line (not yet proposed / proposed as <review> / merged as <sha>)"
  fi
  tag="$(pinned_tag "${svc}" "${rel}")"
  if [ -n "${tag}" ]; then
    if ! grep -qF "${tag}" <<< "${hbuf}"; then
      warn "${f}: header never names ${svc} ${tag}, the tag releases/${rel}/source-refs.yaml pins — re-verify the patch against it and update the 'Applies to' line"
    fi
  fi
done

# ---------------------------------------------------------------------------
# P4 — numbering
# ---------------------------------------------------------------------------
hdr "P4: numbering is gap-free from 0001 per directory"
for d in ${DIRS}; do
  nums=""
  for f in ${PATCHES}; do
    if [ "$(dirname "${f}")" = "${d}" ]; then
      nums="${nums}$(basename "${f}" | cut -c1-4)
"
    fi
  done
  nums="$(printf '%s' "${nums}" | sort)"
  dups="$(printf '%s\n' "${nums}" | uniq -d | tr '\n' ' ')"
  if [ -n "${dups// /}" ]; then
    fail "${d}: duplicate number(s) ${dups}— git apply takes them in glob order, which then depends on the slug"
  fi
  expect=1
  gap=""
  for n in $(printf '%s\n' "${nums}" | uniq); do
    case "${n}" in *[!0-9]*) continue ;; esac
    val=$((10#${n}))
    if [ "${val}" -ne "${expect}" ]; then
      gap="${gap} expected $(printf '%04d' "${expect}") got ${n};"
      expect="${val}"
    fi
    expect=$((expect + 1))
  done
  if [ -n "${gap}" ]; then
    fail "${d}: numbering gap:${gap} renumber so the order stays explicit"
  else
    pass "${d}: $(printf '%s\n' "${nums}" | grep -c . || true) patch(es), $(printf '%s\n' "${nums}" | tr '\n' ' ')"
  fi
done

# ---------------------------------------------------------------------------
# P5 — mirroring across releases
# ---------------------------------------------------------------------------
hdr "P5: the same slug in the service's other releases (review aid)"
for f in ${PATCHES}; do
  rest="${f#patches/}"
  svc="${rest%%/*}"
  rel_rest="${rest#*/}"
  rel="${rel_rest%%/*}"
  base="$(basename "${f}")"
  slug="${base#????-}"
  twins=""
  missing=""
  for r in releases/*/; do
    r="$(basename "${r}")"
    [ "${r}" = "${rel}" ] && continue
    [ -n "$(pinned_tag "${svc}" "${r}")" ] || continue
    twin=""
    for cand in "patches/${svc}/${r}/"*"-${slug}"; do
      [ -f "${cand}" ] && { twin="${cand}"; break; }
    done
    if [ -n "${twin}" ]; then
      if cmp -s "${f}" "${twin}"; then
        twins="${twins} ${r} (identical)"
      else
        twins="${twins} ${r} ($(basename "${twin}" | cut -c1-4), differs)"
      fi
    else
      missing="${missing} ${r}"
    fi
  done
  if [ -n "${twins}" ]; then
    info "${f}: mirrored in${twins}"
  fi
  if [ -n "${missing}" ]; then
    hbuf="$(header "${f}")"
    for r in ${missing}; do
      if grep -qF "${r}" <<< "${hbuf}"; then
        info "${f}: no twin in ${r} (the header names ${r})"
      else
        info "${f}: no twin in ${r} — decide backport/forward-port and say in the header why there is none"
      fi
    done
  fi
done

# ---------------------------------------------------------------------------
# P6 — docs
# ---------------------------------------------------------------------------
hdr "P6: every patch is documented in ${DOCS}"
for f in ${PATCHES}; do
  base="$(basename "${f}")"
  rest="${f#patches/}"
  svc="${rest%%/*}"
  if grep -qF "${base}" "${DOCS}"; then
    pass "${f}: named in ${DOCS}"
  else
    fail "${f}: ${DOCS} never names ${base} — add a **Source patch:** paragraph to its ### ${svc} section"
  fi
done

# ---------------------------------------------------------------------------
# P7 / P8 — test hunks and the image proof
# ---------------------------------------------------------------------------
hdr "P7/P8: test hunks, and the verify_<svc>.sh assertion"
for f in ${PATCHES}; do
  rest="${f#patches/}"
  svc="${rest%%/*}"
  base="$(basename "${f}")"
  slug="${base#????-}"
  slug="${slug%.patch}"
  paths="$(touched "${f}")"
  [ -n "${paths}" ] || continue
  tests="$(printf '%s\n' "${paths}" | grep -Ec "${TEST_PATH_RE}" || true)"
  total="$(printf '%s\n' "${paths}" | grep -c . || true)"
  code=$((total - tests))
  if [ "${code}" -eq 0 ]; then
    info "${f}: test-only (${tests} test file(s)) — changes nothing at runtime, no verify assertion needed"
    continue
  fi
  if [ "${tests}" -gt 0 ]; then
    info "${f}: ${code} code file(s), ${tests} test file(s)"
  elif hflat="$(header "${f}" | tr '\n' ' ')" && grep -Eiq "${NO_TEST_RE}" <<< "${hflat}"; then
    info "${f}: ${code} code file(s), no test file — the header states why"
  else
    warn "${f}: changes ${code} code file(s) and no test file, and the header does not say why no upstream test pins the behaviour — test-service-images runs the upstream suite against it"
  fi
  verify="tests/container-images/verify_${svc}.sh"
  if [ ! -f "${verify}" ]; then
    fail "${f}: ${verify} does not exist — build-service-images passes it as verify-script on every PR"
  elif grep -qF "${slug}" "${verify}"; then
    pass "${f}: ${verify} names the patch"
  else
    warn "${f}: ${verify} never names ${slug} — nothing proves the built image carries the change"
  fi
done

# ---------------------------------------------------------------------------
# P9 — apply against the pinned upstream tag (network, optional)
# ---------------------------------------------------------------------------
hdr "P9: git apply --check against the pinned upstream tag"
if [ "${APPLY}" -eq 0 ]; then
  info "skipped (pass --apply to clone upstream and check every patch)"
else
  WORK="$(mktemp -d "${TMPDIR:-/tmp}/check-image-patches.XXXXXX")"
  trap 'rm -rf "${WORK}"' EXIT
  export GIT_TERMINAL_PROMPT=0
  for d in ${DIRS}; do
    rest="${d#patches/}"
    svc="${rest%%/*}"
    rel="${rest#*/}"
    tag="$(pinned_tag "${svc}" "${rel}")"
    [ -n "${tag}" ] || { info "${d}: no pinned tag — skipped"; continue; }
    src="${WORK}/${svc}-${rel}"
    url=""
    for u in "https://github.com/openstack/${svc}.git" "https://opendev.org/openstack/${svc}.git"; do
      rc=0
      git ls-remote --exit-code --tags "${u}" "refs/tags/${tag}" > /dev/null 2>&1 || rc=$?
      if [ "${rc}" -eq 0 ]; then url="${u}"; break; fi
      if [ "${rc}" -eq 2 ]; then
        fail "${d}: ${u} has no tag ${tag}"
        url="none"
        break
      fi
    done
    if [ -z "${url}" ]; then
      info "${d}: upstream unreachable — skipped"
      continue
    fi
    [ "${url}" = "none" ] && continue
    if ! git clone --quiet --depth 1 --branch "${tag}" "${url}" "${src}" > /dev/null 2>&1; then
      info "${d}: clone of ${url} at ${tag} failed — skipped"
      continue
    fi
    info "${d}: ${svc} ${tag} from ${url}"
    # Build order: the same lexical glob order `git apply dir/*.patch` uses,
    # applied cumulatively so a later patch sees the earlier ones.
    for f in ${PATCHES}; do
      [ "$(dirname "${f}")" = "${d}" ] || continue
      if err="$(git -C "${src}" apply --check "${REPO_ROOT}/${f}" 2>&1)"; then
        git -C "${src}" apply "${REPO_ROOT}/${f}"
        pass "${f}: applies to ${svc} ${tag}"
      elif git -C "${src}" apply --check --reverse "${REPO_ROOT}/${f}" > /dev/null 2>&1; then
        fail "${f}: already contained in ${svc} ${tag} or an earlier patch of ${d} (reverse-applies) — retire it"
      else
        fail "${f}: does not apply to ${svc} ${tag}: $(printf '%s\n' "${err}" | head -2 | tr '\n' ' ')"
      fi
    done
  done
fi

# ---------------------------------------------------------------------------
hdr "Summary"
info "$(wc -w <<< "${PATCHES}" | tr -d ' ') patch file(s) in $(wc -w <<< "${DIRS}" | tr -d ' ') directory(ies); ${WARN_COUNT} warning(s)"
if [ "${FAIL_COUNT}" -eq 0 ]; then
  echo "[PASS] no image-patch findings"
  exit 0
else
  echo "[FAIL] ${FAIL_COUNT} image-patch finding(s)"
  exit 1
fi
