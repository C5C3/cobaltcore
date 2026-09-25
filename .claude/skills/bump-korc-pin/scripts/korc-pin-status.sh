#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# korc-pin-status.sh — report the K-ORC upstream main-commit pin from every
# site that carries it and check that the sites agree:
#   K1  deploy/flux-system/sources/k-orc.yaml ref.commit is a full 40-hex SHA
#   K2  deploy/flux-system/releases/k-orc.yaml newTag == commit-<first 7 of
#       ref.commit> (the hack/ci-deploy-korc.sh drift guard) and digest is
#       sha256:<64 hex>
#   K3  operators/c5c3/go.mod pseudo-version commit == ref.commit; a go.mod
#       that lags the source is accepted only when the upstream range touches
#       no api/ or config/ file (the 653b3f12 expiry re-pin); go.sum carries
#       the go.mod version
#   K4  docs/reference/infrastructure/infrastructure-manifests.md mirrors the
#       commit and the tag
# Network checks (skipped with --offline, degrade to [INFO] when unreachable):
#   Q1  quay tag commit-<short>: present, not expired, expiry more than
#       --warn-days away, and pointing at the pinned digest
#   Q2  the pinned digest still resolves (crane manifest, else quay API) and
#       carries linux/amd64
#   Q3  ref.commit is on upstream main; how far main has moved since
#   Q4  go.mod-vs-source range classification for K3 (GitHub compare API)
# --candidate <sha|main>: resolve a bump target — on upstream main, quay tag
# and expiry, digest to pin, and the upstream paths changed since the current
# pin, with api/ and config/ flagged.
#
# Usage: korc-pin-status.sh [--offline] [--warn-days N] [--candidate <sha|main>]
#
# Read-only: prints a report, writes nothing. Exit code 1 on any [FAIL].

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "${REPO_ROOT}"

SOURCE_YAML="deploy/flux-system/sources/k-orc.yaml"
RELEASE_YAML="deploy/flux-system/releases/k-orc.yaml"
GOMOD="operators/c5c3/go.mod"
GOSUM="operators/c5c3/go.sum"
DOCS="docs/reference/infrastructure/infrastructure-manifests.md"
MODULE="github.com/k-orc/openstack-resource-controller/v2"
IMAGE_REPO="quay.io/orc/openstack-resource-controller"
QUAY_API="https://quay.io/api/v1/repository/orc/openstack-resource-controller"
GH_REPO="k-orc/openstack-resource-controller"

OFFLINE=0
WARN_DAYS=7
CANDIDATE=""

usage() {
  echo "usage: $0 [--offline] [--warn-days N] [--candidate <sha|main>]" >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --offline) OFFLINE=1 ;;
    --warn-days)
      [ $# -ge 2 ] || usage
      WARN_DAYS="$2"
      shift
      ;;
    --candidate)
      [ $# -ge 2 ] || usage
      CANDIDATE="$2"
      shift
      ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
  shift
done
case "${WARN_DAYS}" in
  ''|*[!0-9]*) echo "--warn-days needs a non-negative integer, got '${WARN_DAYS}'" >&2; exit 2 ;;
esac

FAIL_COUNT=0
WARN_COUNT=0
fail() { echo "[FAIL] $*"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
warn() { echo "[WARN] $*"; WARN_COUNT=$((WARN_COUNT + 1)); }
pass() { echo "[PASS] $*"; }
info() { echo "[INFO] $*"; }
hdr()  { echo; echo "=== $* ==="; }

is_hex() { # is_hex <string> <length>
  grep -Eq "^[0-9a-f]{$2}\$" <<<"$1"
}

# ---------------------------------------------------------------------------
# Tooling for the network half
# ---------------------------------------------------------------------------
HAVE_JQ=0; command -v jq > /dev/null 2>&1 && HAVE_JQ=1
HAVE_GH=0; command -v gh > /dev/null 2>&1 && HAVE_GH=1
HAVE_CRANE=0; command -v crane > /dev/null 2>&1 && HAVE_CRANE=1
HAVE_CURL=0; command -v curl > /dev/null 2>&1 && HAVE_CURL=1
NET=1
if [ "${OFFLINE}" -eq 1 ]; then
  NET=0
elif [ "${HAVE_JQ}" -eq 0 ] || [ "${HAVE_CURL}" -eq 0 ]; then
  NET=0
  info "jq or curl not on PATH — network checks skipped"
fi
NOW="$(date -u +%s)"

# gh_json <api path> — GitHub REST JSON via gh (authenticated), else curl.
gh_json() {
  if [ "${HAVE_GH}" -eq 1 ] && gh api "$1" 2> /dev/null; then
    return 0
  fi
  curl -fsS --max-time 20 -H 'Accept: application/vnd.github+json' \
    "https://api.github.com/$1" 2> /dev/null
}

# compare_json <base> <head> — GitHub compare of two upstream refs.
compare_json() {
  gh_json "repos/${GH_REPO}/compare/$1...$2"
}

# quay_tag <tag> — prints "<start_ts> <end_ts|null> <digest> <expiration>" for
# the newest history entry of <tag>; prints "GONE" when quay knows no such tag;
# returns 1 when quay is unreachable.
quay_tag() {
  local json
  json="$(curl -fsS --max-time 20 "${QUAY_API}/tag/?specificTag=$1" 2> /dev/null)" || return 1
  printf '%s' "${json}" | jq -r '
    if (.tags | length) == 0 then "GONE"
    else (.tags | sort_by(.start_ts) | last)
      | "\(.start_ts) \(.end_ts // "null") \(.manifest_digest) \(.expiration // "never")"
    end'
}

# check_tag <tag> <pinned digest|""> — Q1 verdict for one quay tag. Sets
# TAG_DIGEST to the digest the tag points at (empty when unknown).
TAG_DIGEST=""
check_tag() {
  local tag="$1" pinned="$2" row start end digest expiration left days
  TAG_DIGEST=""
  if ! row="$(quay_tag "${tag}")"; then
    info "quay.io unreachable — tag ${tag} not checked (skipped)"
    return 0
  fi
  if [ "${row}" = "GONE" ]; then
    fail "${IMAGE_REPO}:${tag} does not exist on quay (never pushed, or garbage-collected)"
    return 0
  fi
  start="${row%% *}"; row="${row#* }"
  end="${row%% *}"; row="${row#* }"
  digest="${row%% *}"; expiration="${row#* }"
  TAG_DIGEST="${digest}"
  if [ "${end}" = "null" ]; then
    pass "${tag}: no expiration set (pushed at epoch ${start})"
  else
    left=$((end - NOW))
    days=$((left / 86400))
    if [ "${left}" -le 0 ]; then
      fail "${tag}: expired ${expiration} — quay garbage-collects the manifest; every ci-deploy-korc.sh leg ends in ImagePullBackOff"
    elif [ "${days}" -lt "${WARN_DAYS}" ]; then
      warn "${tag}: expires ${expiration} (${days} days left, threshold ${WARN_DAYS}) — bump the pin before it lapses"
    else
      pass "${tag}: expires ${expiration} (${days} days left)"
    fi
  fi
  if [ -n "${pinned}" ]; then
    if [ "${digest}" = "${pinned}" ]; then
      pass "${tag} points at the pinned digest ${pinned}"
    else
      fail "${tag} points at ${digest}, the manifest pins ${pinned} — re-resolve the digest"
    fi
  fi
}

# digest_resolves <digest> — Q2: 0 resolves, 1 gone, 2 unknown (network).
digest_resolves() {
  local out code
  if [ "${HAVE_CRANE}" -eq 1 ]; then
    if crane manifest "${IMAGE_REPO}@$1" > /dev/null 2>&1; then
      return 0
    fi
    out="$(crane manifest "${IMAGE_REPO}@$1" 2>&1 || true)"
    case "${out}" in
      *MANIFEST_UNKNOWN*|*NOT_FOUND*|*"not found"*) return 1 ;;
    esac
  fi
  code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 20 \
    "${QUAY_API}/manifest/$1" 2> /dev/null || true)"
  case "${code}" in
    200) return 0 ;;
    404) return 1 ;;
  esac
  return 2
}

# platforms <digest> — os/arch list of an image index (crane only; prints
# nothing without crane or on any error).
platforms() {
  [ "${HAVE_CRANE}" -eq 1 ] || return 0
  crane manifest "${IMAGE_REPO}@$1" 2> /dev/null \
    | jq -r '.manifests[]?.platform | select(.os != "unknown") | "\(.os)/\(.architecture)"' 2> /dev/null \
    | sort -u | tr '\n' ' ' || true
}

# indent_list — indent a file list for the report, capped at 25 lines.
indent_list() {
  awk 'NR <= 25 { print "         " $0 } END { if (NR > 25) printf "         ... %d more\n", NR - 25 }'
}

# changed_paths <base> <head> — prints "status ahead_by" on line 1, then the
# changed filenames. Returns 1 when GitHub is unreachable.
changed_paths() {
  local json
  json="$(compare_json "$1" "$2")" || return 1
  [ -n "${json}" ] || return 1
  printf '%s' "${json}" | jq -r '"\(.status) \(.ahead_by)", (.files[]?.filename)'
}

# ---------------------------------------------------------------------------
# K1 — the Flux source commit
# ---------------------------------------------------------------------------
hdr "K1: Flux GitRepository commit (${SOURCE_YAML})"
SRC_COMMIT="$(awk '/^[[:space:]]*commit:[[:space:]]*/{print $2; exit}' "${SOURCE_YAML}")"
SRC_TAG_REF="$(awk '/^[[:space:]]*tag:[[:space:]]*/{print $2; exit}' "${SOURCE_YAML}")"
if [ -z "${SRC_COMMIT}" ] && [ -n "${SRC_TAG_REF}" ]; then
  info "the source is on release tag ${SRC_TAG_REF} (ref.tag), not a main commit — the main-pin checks do not apply"
  info "the revert to a release is described in the headers of ${SOURCE_YAML} and ${RELEASE_YAML}"
  exit 0
fi
if is_hex "${SRC_COMMIT}" 40; then
  pass "ref.commit = ${SRC_COMMIT}"
else
  fail "ref.commit '${SRC_COMMIT}' is not a 40-hex SHA — hack/ci-deploy-korc.sh refuses any other form"
fi
SHORT7="$(printf '%s' "${SRC_COMMIT}" | cut -c1-7)"
SHORT12="$(printf '%s' "${SRC_COMMIT}" | cut -c1-12)"

# ---------------------------------------------------------------------------
# K2 — the image override
# ---------------------------------------------------------------------------
hdr "K2: image override (${RELEASE_YAML})"
NEW_NAME="$(awk '/^[[:space:]]*newName:[[:space:]]*/{print $2; exit}' "${RELEASE_YAML}")"
NEW_TAG="$(awk '/^[[:space:]]*newTag:[[:space:]]*/{print $2; exit}' "${RELEASE_YAML}")"
DIGEST="$(awk '/^[[:space:]]*digest:[[:space:]]*/{print $2; exit}' "${RELEASE_YAML}")"
info "newName = ${NEW_NAME:-<missing>}, newTag = ${NEW_TAG:-<missing>}, digest = ${DIGEST:-<missing>}"
if [ "${NEW_NAME}" != "${IMAGE_REPO}" ]; then
  warn "newName is '${NEW_NAME}', expected ${IMAGE_REPO} — this script checks quay for the upstream repository only"
fi
if [ "${NEW_TAG}" = "commit-${SHORT7}" ]; then
  pass "newTag ${NEW_TAG} matches ref.commit (the ci-deploy-korc.sh drift guard holds)"
else
  fail "newTag '${NEW_TAG}' != commit-${SHORT7} — hack/ci-deploy-korc.sh fails every K-ORC leg with 'does not match the pinned commit'"
fi
if grep -Eq '^sha256:[0-9a-f]{64}$' <<<"${DIGEST}"; then
  pass "digest has the sha256:<64 hex> form"
else
  fail "digest '${DIGEST}' is not sha256:<64 hex> — hack/ci-deploy-korc.sh refuses a tag-only pull"
fi

# ---------------------------------------------------------------------------
# K3 — the Go module the c5c3-operator compiles against
# ---------------------------------------------------------------------------
hdr "K3: Go module pin (${GOMOD})"
GOMOD_VER="$(awk -v m="${MODULE}" '$1 == m {print $2; exit}' "${GOMOD}")"
GOMOD_COMMIT=""
if [ -z "${GOMOD_VER}" ]; then
  fail "${GOMOD} requires no ${MODULE}"
else
  info "${MODULE} ${GOMOD_VER}"
  if grep -Eq "^replace[[:space:]].*${MODULE}|^[[:space:]]+${MODULE}[[:space:]]+=>" "${GOMOD}"; then
    warn "${GOMOD} carries a replace directive for ${MODULE} — the pseudo-version below is not what builds"
  fi
  suffix="${GOMOD_VER##*-}"
  if is_hex "${suffix}" 12; then
    GOMOD_COMMIT="${suffix}"
    stamp="${GOMOD_VER%-*}"
    stamp="${stamp##*.}"
    info "pseudo-version commit ${GOMOD_COMMIT}, committed ${stamp} (UTC, yyyymmddhhmmss)"
    if [ "${GOMOD_COMMIT}" = "${SHORT12}" ]; then
      pass "go.mod pins the same commit as the Flux source (${SHORT12})"
    elif [ "${NET}" -eq 1 ]; then
      info "go.mod pins ${GOMOD_COMMIT}, the Flux source pins ${SHORT12} — classified against the upstream range in Q4"
    else
      warn "go.mod pins ${GOMOD_COMMIT}, the Flux source pins ${SHORT12} — allowed only for an image-only re-pin whose upstream range touches no api/ or config/ file; rerun without --offline to classify"
    fi
  else
    fail "go.mod pins ${GOMOD_VER}, not a main pseudo-version — no released K-ORC ships the RoleAssignment and Region kinds the c5c3-operator owns"
  fi
  if grep -qF "${MODULE} ${GOMOD_VER} h1:" "${GOSUM}" && grep -qF "${MODULE} ${GOMOD_VER}/go.mod h1:" "${GOSUM}"; then
    pass "${GOSUM} carries both hashes for ${GOMOD_VER}"
  else
    fail "${GOSUM} lacks the h1: or /go.mod h1: line for ${GOMOD_VER} — run go mod tidy in operators/c5c3"
  fi
fi

# ---------------------------------------------------------------------------
# K4 — the docs mirror
# ---------------------------------------------------------------------------
hdr "K4: reference docs mirror (${DOCS})"
if grep -qF "commit \`${SRC_COMMIT}\`" "${DOCS}"; then
  pass "docs name commit ${SRC_COMMIT}"
else
  row="$(grep -nE 'GitRepository.*[(]commit' "${DOCS}" | head -1 | cut -c1-160 || true)"
  fail "docs do not name commit ${SRC_COMMIT} — ${row:-no K-ORC Source row found}"
fi
if grep -qF "${IMAGE_REPO}:${NEW_TAG}\`" "${DOCS}"; then
  pass "docs name image tag ${NEW_TAG}"
else
  row="$(grep -nF "${IMAGE_REPO}:" "${DOCS}" | head -1 | cut -c1-160 || true)"
  fail "docs do not name ${IMAGE_REPO}:${NEW_TAG} — ${row:-no K-ORC Image row found}"
fi

# ---------------------------------------------------------------------------
# Q1–Q4 — registry and upstream state
# ---------------------------------------------------------------------------
if [ "${NET}" -eq 0 ]; then
  hdr "Q1–Q4: network checks"
  info "skipped (--offline or missing jq/curl)"
else
  hdr "Q1: quay tag ${NEW_TAG}"
  check_tag "${NEW_TAG}" "${DIGEST}"

  hdr "Q2: pinned digest resolves"
  rc=0
  digest_resolves "${DIGEST}" || rc=$?
  case "${rc}" in
    0) pass "${IMAGE_REPO}@${DIGEST} resolves" ;;
    1) fail "${IMAGE_REPO}@${DIGEST} is gone (manifest unknown) — the pull fails with ImagePullBackOff; bump the pin" ;;
    *) info "registry unreachable — digest not checked (skipped)" ;;
  esac
  if [ "${rc}" -eq 0 ]; then
    plats="$(platforms "${DIGEST}")"
    if [ -n "${plats}" ]; then
      info "platforms: ${plats}"
      case " ${plats}" in
        *" linux/amd64 "*) pass "the index carries linux/amd64 (the CI runners)" ;;
        *) warn "the index carries no linux/amd64 image" ;;
      esac
    fi
  fi

  hdr "Q3: ref.commit against upstream main"
  if main_row="$(changed_paths "${SRC_COMMIT}" main)"; then
    main_status="$(printf '%s\n' "${main_row}" | head -1)"
    case "${main_status%% *}" in
      identical) pass "ref.commit is upstream main's HEAD" ;;
      ahead) pass "ref.commit is on upstream main; main is ${main_status#* } commit(s) ahead" ;;
      *) fail "ref.commit is not an ancestor of upstream main (compare status '${main_status%% *}') — a release-branch commit also gets a commit-<sha> tag" ;;
    esac
    head_json="$(gh_json "repos/${GH_REPO}/commits/main" || true)"
    if [ -n "${head_json}" ]; then
      info "upstream main HEAD: $(printf '%s' "${head_json}" | jq -r '"\(.sha[0:7]) (\(.commit.committer.date))"') — try: bash $0 --candidate main"
    fi
  else
    info "GitHub unreachable — main ancestry not checked (skipped)"
  fi

  hdr "Q4: go.mod vs Flux source range"
  if [ -z "${GOMOD_COMMIT}" ] || [ "${GOMOD_COMMIT}" = "${SHORT12}" ]; then
    info "nothing to classify (go.mod and source agree, or go.mod carries no pseudo-version)"
  elif range="$(changed_paths "${GOMOD_COMMIT}" "${SRC_COMMIT}")"; then
    rstatus="$(printf '%s\n' "${range}" | head -1)"
    api_cfg="$(printf '%s\n' "${range}" | sed 1d | grep -E '^(api|config)/' || true)"
    if [ "${rstatus%% *}" != "ahead" ]; then
      fail "go.mod commit ${GOMOD_COMMIT} is not behind the source commit (compare status '${rstatus%% *}') — the operator compiles against an API the deployed CRDs do not carry"
    elif [ -n "${api_cfg}" ]; then
      fail "go.mod lags the source by ${rstatus#* } commit(s) and the range touches $(printf '%s\n' "${api_cfg}" | wc -l | tr -d ' ') api/ or config/ file(s) — move the Go module in lockstep"
      printf '%s\n' "${api_cfg}" | indent_list
    else
      info "expected: go.mod lags the source by ${rstatus#* } commit(s) and the range touches no api/ or config/ file (image-only re-pin, precedent 653b3f12)"
    fi
  else
    warn "GitHub unreachable — cannot tell whether the go.mod lag is an image-only re-pin (skipped)"
  fi
fi

# ---------------------------------------------------------------------------
# Candidate mode
# ---------------------------------------------------------------------------
if [ -n "${CANDIDATE}" ]; then
  hdr "Candidate ${CANDIDATE}"
  if [ "${NET}" -eq 0 ]; then
    info "candidate mode needs quay.io and GitHub — skipped"
  else
    CAND=""
    cmeta="$(gh_json "repos/${GH_REPO}/commits/${CANDIDATE}" || true)"
    if [ -n "${cmeta}" ]; then
      CAND="$(printf '%s' "${cmeta}" | jq -r '.sha // empty' 2> /dev/null || true)"
    fi
    CAND_FAIL_BASE="${FAIL_COUNT}"
    if [ -z "${CAND}" ]; then
      if gh_json "repos/${GH_REPO}" > /dev/null; then
        fail "'${CANDIDATE}' names no commit in ${GH_REPO}"
      else
        info "GitHub unreachable — '${CANDIDATE}' not resolved (skipped)"
      fi
    else
      C7="$(printf '%s' "${CAND}" | cut -c1-7)"
      C12="$(printf '%s' "${CAND}" | cut -c1-12)"
      info "${CAND}: $(printf '%s' "${cmeta}" | jq -r '"\(.commit.committer.date) — \(.commit.message | split("\n")[0])"')"
      if crow="$(changed_paths "${CAND}" main)"; then
        cstatus="$(printf '%s\n' "${crow}" | head -1)"
        case "${cstatus%% *}" in
          identical) pass "candidate is upstream main's HEAD" ;;
          ahead) pass "candidate is on upstream main (${cstatus#* } commit(s) behind HEAD)" ;;
          *) fail "candidate is not on upstream main (compare status '${cstatus%% *}')" ;;
        esac
      fi
      check_tag "commit-${C7}" ""
      cdigest="${TAG_DIGEST}"
      if [ "${HAVE_CRANE}" -eq 1 ]; then
        crane_digest="$(crane digest "${IMAGE_REPO}:commit-${C7}" 2> /dev/null || true)"
        if [ -n "${crane_digest}" ]; then
          if [ -n "${cdigest}" ] && [ "${crane_digest}" != "${cdigest}" ]; then
            warn "crane digest ${crane_digest} differs from quay's listing ${cdigest}"
          fi
          cdigest="${crane_digest}"
        fi
      fi
      if [ -n "${cdigest}" ]; then
        plats="$(platforms "${cdigest}")"
        [ -n "${plats}" ] && info "candidate platforms: ${plats}"
      fi

      if range="$(changed_paths "${SRC_COMMIT}" "${CAND}")"; then
        rstatus="$(printf '%s\n' "${range}" | head -1)"
        files="$(printf '%s\n' "${range}" | sed 1d)"
        if [ "${rstatus%% *}" = "behind" ]; then
          # An older candidate: the files the move would revert are the ones
          # the current pin gained, i.e. the reverse range.
          files="$(changed_paths "${CAND}" "${SRC_COMMIT}" | sed 1d || true)"
        fi
        nfiles="$(printf '%s\n' "${files}" | grep -c . || true)"
        case "${rstatus%% *}" in
          ahead) info "current pin ${SHORT7} -> candidate ${C7}: ${rstatus#* } commit(s), ${nfiles} file(s) changed" ;;
          identical) info "candidate equals the current pin" ;;
          behind) warn "candidate is OLDER than the current pin ${SHORT7} (${nfiles} file(s) would revert) — only sensible when every newer tag is gone" ;;
          *) fail "candidate and current pin diverged (compare status '${rstatus%% *}')" ;;
        esac
        [ "${nfiles}" -ge 300 ] && warn "GitHub caps the compare file list at 300 — clone upstream and run git diff --name-only for the full range"
        if [ "${nfiles}" -gt 0 ]; then
          info "changed paths by top-level directory:"
          printf '%s\n' "${files}" | awk -F/ 'NF { d = (NF > 1 ? $1 "/" : $1); n[d]++ } END { for (d in n) printf "         %4d  %s\n", n[d], d }' | sort -k2
        fi
        api_cfg="$(printf '%s\n' "${files}" | grep -E '^(api|config)/' || true)"
        if [ -n "${api_cfg}" ]; then
          warn "the range touches api/ or config/ — move the Go module, go mod tidy, build and vet c5c3, and review the CRD consumers:"
          printf '%s\n' "${api_cfg}" | indent_list
        else
          info "the range touches no api/ or config/ file — the CRDs and the Go API are unchanged"
        fi
        if grep -Eq '^go\.(mod|sum)$' <<<"${files}"; then
          info "upstream go.mod/go.sum changed — expect transitive bumps in ${GOSUM}; see the check-go-workspace-deps skill"
        fi
      else
        info "GitHub unreachable — range not listed (skipped)"
      fi

      hdr "Candidate edit set (move in one commit)"
      if [ "${FAIL_COUNT}" -gt "${CAND_FAIL_BASE}" ]; then
        info "withheld: the candidate failed a check above — pick another commit"
      else
        info "${SOURCE_YAML}: commit: ${CAND}"
        info "${RELEASE_YAML}: newTag: commit-${C7}"
        info "${RELEASE_YAML}: digest: ${cdigest:-<resolve: crane digest ${IMAGE_REPO}:commit-${C7}>}"
        info "${DOCS}: commit \`${CAND}\` and \`${IMAGE_REPO}:commit-${C7}\`"
        info "${GOMOD}: (cd operators/c5c3 && go get ${MODULE}@${C12} && go mod tidy)"
      fi
    fi
  fi
fi

# ---------------------------------------------------------------------------
hdr "Summary"
info "pin: source ${SHORT7}, image ${NEW_TAG:-?}, go.mod ${GOMOD_COMMIT:-${GOMOD_VER:-?}}; ${WARN_COUNT} warning(s)"
if [ "${FAIL_COUNT}" -eq 0 ]; then
  echo "[PASS] K-ORC pin sites agree"
  exit 0
else
  echo "[FAIL] ${FAIL_COUNT} K-ORC pin finding(s)"
  exit 1
fi
