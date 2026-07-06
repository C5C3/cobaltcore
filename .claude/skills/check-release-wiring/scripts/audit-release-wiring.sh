#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# audit-release-wiring.sh — mechanical release-wiring checks for the forge repo.
# Verifies that every OpenStack release under releases/<version>/ is fully
# wired through the repo, and that no release-shaped reference points at a
# version that has no releases/ directory:
#   L1  every releases/<version>/ carries the four mandatory config files and
#       every test-excludes/*.txt file maps back to a source-refs.yaml service
#   L2  every release has a Tempest config dir tests/tempest/keystone-<slug>/
#       with the four expected files, the CR tag matches the release, and no
#       orphan Tempest dir survives a removed release
#   L3  every release is covered by a basic-deployment e2e suite (the plain
#       suite's tag or a basic-deployment-<slug> variant), variant fixtures
#       and image refs carry the right version, and no orphan variant remains
#   L4  every default-release reference (deploy/kind ControlPlane, deploy-infra
#       preload, RELEASE:- fallbacks in hack/, image tags in ci.yaml) points at
#       an existing releases/<version>/ directory
#   L5  the Renovate regression tests reference only existing releases/ paths
#   L6  the upgrade-path e2e suites cover the newest sequential transition and
#       the skip-level fixture still names a non-existent release
#   L7  the release version pattern stays in lockstep across the CRD marker,
#       the webhook regexp, release.ParseRelease, and the generated CRD YAMLs
#
# Defers structural validation of the release config files to
# tests/container-images/verify_release_config.sh and the shell unit tests
# under tests/unit/. Pass --full to chain those gates after the inventory.
# Exit code 1 on any [FAIL].

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "${REPO_ROOT}"

FULL=0
if [[ "${1:-}" == "--full" ]]; then
  FULL=1
fi

FAIL_COUNT=0
fail() { echo "[FAIL] $*"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
pass() { echo "[PASS] $*"; }
info() { echo "[INFO] $*"; }
hdr()  { echo; echo "=== $* ==="; }

# ---------------------------------------------------------------------------
# Discover releases and their services (source-refs.yaml keys)
# ---------------------------------------------------------------------------
RELEASES=""
shopt -s nullglob
for d in releases/*/; do
  r="${d%/}"
  r="${r##*/}"
  RELEASES="${RELEASES} ${r}"
done
shopt -u nullglob
# YYYY.N sorts correctly lexicographically (fixed-width year, single-digit N).
RELEASES="$(echo "${RELEASES}" | tr ' ' '\n' | grep -v '^$' | sort)"

if [[ -z "${RELEASES}" ]]; then
  fail "no release directories found under releases/"
  echo; echo "=== Summary ==="; echo "[FAIL] 1 release-wiring finding(s)"
  exit 1
fi

# services_of <release> — top-level keys of source-refs.yaml, comments skipped.
services_of() {
  local refs="releases/$1/source-refs.yaml"
  [[ -f "${refs}" ]] || return 0
  grep -E '^[a-z0-9_-]+:' "${refs}" | cut -d: -f1
}

# release_exists <version> — true when releases/<version>/ is a directory.
release_exists() { [[ -d "releases/$1" ]]; }

hdr "Inventory — releases and services"
for r in ${RELEASES}; do
  svcs="$(services_of "${r}" | tr '\n' ' ')"
  info "release ${r}: services: ${svcs:-<none>}"
done

# ---------------------------------------------------------------------------
# L1 — mandatory release config files + test-excludes mapping
# ---------------------------------------------------------------------------
hdr "L1: releases/<version>/ mandatory files and test-excludes mapping"
for r in ${RELEASES}; do
  for f in source-refs.yaml test-refs.yaml extra-packages.yaml upper-constraints.txt; do
    if [[ -f "releases/${r}/${f}" ]]; then
      pass "releases/${r}/${f} present"
    else
      fail "releases/${r}/${f} missing — the build/test matrix or image build breaks"
    fi
  done
  # Every test-excludes file must map back to a service key (verify_release_config
  # Test 7); a service without an excludes file is fine (Test 5: optional).
  shopt -s nullglob
  for x in "releases/${r}/test-excludes/"*.txt; do
    svc="$(basename "${x}" .txt)"
    if services_of "${r}" | grep -qx "${svc}"; then
      pass "releases/${r}/test-excludes/${svc}.txt maps to service '${svc}'"
    else
      fail "releases/${r}/test-excludes/${svc}.txt has no '${svc}:' key in source-refs.yaml"
    fi
  done
  shopt -u nullglob
  for svc in $(services_of "${r}"); do
    if [[ ! -f "releases/${r}/test-excludes/${svc}.txt" ]]; then
      info "releases/${r}/test-excludes/${svc}.txt absent — unit tests run without an exclude list (allowed)"
    fi
  done
done
# Service-set drift between releases is legal (a service can be added in the
# newest release only) but worth surfacing.
first_set=""
for r in ${RELEASES}; do
  set_r="$(services_of "${r}" | sort | tr '\n' ',')"
  if [[ -z "${first_set}" ]]; then
    first_set="${set_r}"
  elif [[ "${set_r}" != "${first_set}" ]]; then
    info "service sets differ across releases (${first_set} vs ${set_r}) — confirm this is intentional"
  fi
done

# ---------------------------------------------------------------------------
# L2 — Tempest config directories (hard CI dependency)
# ---------------------------------------------------------------------------
hdr "L2: tests/tempest/keystone-<slug>/ per release (ci-generate-tempest-matrix.sh contract)"
for r in ${RELEASES}; do
  slug="${r//./-}"
  tdir="tests/tempest/keystone-${slug}"
  if [[ ! -d "${tdir}" ]]; then
    fail "${tdir} missing — hack/ci-generate-tempest-matrix.sh fails the whole pipeline for release ${r}"
    continue
  fi
  pass "${tdir} present"
  for f in 00-keystone-cr.yaml exclude-tests.txt include-tests.txt tempest.conf; do
    if [[ -f "${tdir}/${f}" ]]; then
      pass "${tdir}/${f} present"
    else
      fail "${tdir}/${f} missing"
    fi
  done
  if [[ -f "${tdir}/00-keystone-cr.yaml" ]]; then
    tag="$(sed -nE 's/.*tag:[[:space:]]*"([^"]+)".*/\1/p' "${tdir}/00-keystone-cr.yaml" | head -1)"
    if [[ "${tag}" == "${r}" ]]; then
      pass "${tdir}/00-keystone-cr.yaml pins tag \"${r}\""
    else
      fail "${tdir}/00-keystone-cr.yaml pins tag \"${tag}\" but the directory is for release ${r}"
    fi
  fi
done
# Orphan Tempest dirs (release removed, dir left behind).
shopt -s nullglob
for tdir in tests/tempest/keystone-*/; do
  slug="$(basename "${tdir%/}")"
  slug="${slug#keystone-}"
  ver="$(echo "${slug}" | sed -E 's/^([0-9]{4})-([0-9])$/\1.\2/')"
  if ! release_exists "${ver}"; then
    fail "${tdir%/} has no matching releases/${ver}/ — orphan of a removed release"
  fi
done
shopt -u nullglob

# ---------------------------------------------------------------------------
# L3 — per-release basic-deployment e2e coverage
# ---------------------------------------------------------------------------
hdr "L3: basic-deployment e2e coverage per release"
plain_tag=""
if [[ -f tests/e2e/keystone/basic-deployment/00-keystone-cr.yaml ]]; then
  plain_tag="$(sed -nE 's/.*tag:[[:space:]]*"([^"]+)".*/\1/p' tests/e2e/keystone/basic-deployment/00-keystone-cr.yaml | head -1)"
  info "plain basic-deployment suite pins tag \"${plain_tag}\""
  if ! release_exists "${plain_tag}"; then
    fail "tests/e2e/keystone/basic-deployment/00-keystone-cr.yaml pins tag \"${plain_tag}\" but releases/${plain_tag}/ does not exist"
  fi
fi
for r in ${RELEASES}; do
  slug="${r//./-}"
  vdir="tests/e2e/keystone/basic-deployment-${slug}"
  if [[ "${plain_tag}" == "${r}" ]]; then
    pass "release ${r} covered by the plain basic-deployment suite"
  elif [[ -d "${vdir}" ]]; then
    pass "release ${r} covered by ${vdir}"
  else
    fail "release ${r} has no basic-deployment coverage — neither the plain suite tag nor ${vdir}"
  fi
done
# Variant-internal consistency + orphan variants.
shopt -s nullglob
for vdir in tests/e2e/keystone/basic-deployment-*/; do
  slug="$(basename "${vdir%/}")"
  slug="${slug#basic-deployment-}"
  ver="$(echo "${slug}" | sed -E 's/^([0-9]{4})-([0-9])$/\1.\2/')"
  if ! release_exists "${ver}"; then
    fail "${vdir%/} has no matching releases/${ver}/ — orphan of a removed release"
    continue
  fi
  cr="${vdir}00-keystone-cr.yaml"
  if [[ -f "${cr}" ]]; then
    tag="$(sed -nE 's/.*tag:[[:space:]]*"([^"]+)".*/\1/p' "${cr}" | head -1)"
    if [[ "${tag}" == "${ver}" ]]; then
      pass "${cr} pins tag \"${ver}\""
    else
      fail "${cr} pins tag \"${tag}\" but the variant directory is for release ${ver}"
    fi
  fi
  ct="${vdir}chainsaw-test.yaml"
  if [[ -f "${ct}" ]]; then
    stray="$(grep -oE 'ghcr\.io/[a-z0-9/-]+:[0-9]{4}\.[12]' "${ct}" | grep -v ":${ver}\$" || true)"
    if [[ -z "${stray}" ]]; then
      pass "${ct} image refs all pin :${ver}"
    else
      fail "${ct} carries image refs for another release: $(echo "${stray}" | tr '\n' ' ')"
    fi
  fi
done
shopt -u nullglob

# ---------------------------------------------------------------------------
# L4 — default-release references point at existing releases
# ---------------------------------------------------------------------------
hdr "L4: default-release references resolve to releases/<version>/"

check_ref() { # <file> <version> <what>
  local f="$1" v="$2" what="$3"
  if [[ -z "${v}" ]]; then
    fail "${f}: could not extract ${what} — the reference shape changed, update this audit"
  elif release_exists "${v}"; then
    pass "${f}: ${what} \"${v}\" exists under releases/"
  else
    fail "${f}: ${what} \"${v}\" has no releases/${v}/ directory"
  fi
}

v="$(sed -nE 's/.*openStackRelease:[[:space:]]*"([0-9]{4}\.[12])".*/\1/p' deploy/kind/controlplane/controlplane.yaml | head -1)"
check_ref deploy/kind/controlplane/controlplane.yaml "${v}" "openStackRelease"

v="$(sed -nE 's/.*cp_release="([0-9]{4}\.[12])".*/\1/p' hack/deploy-infra.sh | head -1)"
check_ref hack/deploy-infra.sh "${v}" "cp_release preload default"

for f in hack/ci-build-service-image.sh hack/ci-build-tempest-image.sh hack/run-tempest.sh; do
  v="$(sed -nE 's/.*RELEASE:-([0-9]{4}\.[12]).*/\1/p' "${f}" | head -1)"
  check_ref "${f}" "${v}" "RELEASE fallback"
done

# Image tags of the form <name>:<YYYY.N> hard-coded in ci.yaml (upgrade
# re-tagging, kind image preloads). Colon-anchored so prose dates do not match.
ci_versions="$(grep -oE ':[0-9]{4}\.[12]' .github/workflows/ci.yaml | tr -d ':' | sort -u)"
if [[ -z "${ci_versions}" ]]; then
  info ".github/workflows/ci.yaml: no hard-coded image-tag releases found"
else
  for v in ${ci_versions}; do
    check_ref .github/workflows/ci.yaml "${v}" "hard-coded image tag"
  done
fi

# ---------------------------------------------------------------------------
# L5 — Renovate regression tests reference existing releases
# ---------------------------------------------------------------------------
hdr "L5: tests/unit/renovate/ referenced release paths exist"
shopt -s nullglob
for t in tests/unit/renovate/*_test.sh; do
  refs="$(grep -oE 'releases/[0-9]{4}\.[12]' "${t}" | sort -u || true)"
  if [[ -z "${refs}" ]]; then
    continue
  fi
  for ref in ${refs}; do
    v="${ref#releases/}"
    if release_exists "${v}"; then
      pass "${t}: references ${ref} (exists)"
    else
      fail "${t}: references ${ref} which does not exist — the renovate unit test breaks"
    fi
  done
done
shopt -u nullglob

# ---------------------------------------------------------------------------
# L6 — upgrade-path e2e suites cover the newest sequential transition
# ---------------------------------------------------------------------------
hdr "L6: upgrade-path suites cover the newest sequential transition"
release_count="$(echo "${RELEASES}" | wc -l | tr -d ' ')"
if [[ "${release_count}" -lt 2 ]]; then
  info "fewer than two releases — no upgrade transition to cover"
else
  newest="$(echo "${RELEASES}" | tail -1)"
  prev="$(echo "${RELEASES}" | tail -2 | head -1)"
  info "newest transition: ${prev} -> ${newest}"
  for suite in release-upgrade upgrade-flow; do
    base="tests/e2e/keystone/${suite}"
    from="$(sed -nE 's/.*tag:[[:space:]]*"([^"]+)".*/\1/p' "${base}/00-keystone-cr.yaml" 2>/dev/null | head -1)"
    to="$(sed -nE 's/.*tag:[[:space:]]*"([^"]+)".*/\1/p' "${base}/01-patch-upgrade.yaml" 2>/dev/null | head -1)"
    if [[ "${from}" == "${prev}" && "${to}" == "${newest}" ]]; then
      pass "${base} tests ${from} -> ${to} (newest transition)"
    else
      fail "${base} tests ${from:-?} -> ${to:-?} but the newest transition is ${prev} -> ${newest}"
    fi
  done
  skip="$(sed -nE 's/.*tag:[[:space:]]*"([^"]+)".*/\1/p' tests/e2e/keystone/upgrade-flow/02-patch-skip-level.yaml 2>/dev/null | head -1)"
  if [[ -n "${skip}" ]]; then
    if release_exists "${skip}"; then
      fail "upgrade-flow/02-patch-skip-level.yaml targets ${skip} which now EXISTS — the skip-level rejection test no longer tests a skip"
    else
      pass "upgrade-flow/02-patch-skip-level.yaml targets ${skip} (correctly non-existent / skip-level)"
    fi
  fi
fi

# ---------------------------------------------------------------------------
# L7 — release version pattern lockstep
# ---------------------------------------------------------------------------
hdr "L7: version pattern lockstep (CRD marker / webhook regexp / ParseRelease / CRD YAMLs)"
TYPES_GO="operators/c5c3/api/v1alpha1/controlplane_types.go"
WEBHOOK_GO="operators/c5c3/api/v1alpha1/controlplane_webhook.go"
RELEASE_GO="internal/common/release/release.go"

marker="$(grep 'kubebuilder:validation:Pattern' "${TYPES_GO}" | sed -nE 's/.*Pattern=`([^`]+)`.*/\1/p' | head -1)"
webhook_re="$(grep 'controlPlaneReleaseRegexp = ' "${WEBHOOK_GO}" | sed -nE 's/.*MustCompile\(`([^`]+)`\).*/\1/p' | head -1)"
if [[ -z "${marker}" || -z "${webhook_re}" ]]; then
  fail "could not extract the version pattern from ${TYPES_GO} / ${WEBHOOK_GO} — shape changed, update this audit"
else
  if [[ "${marker}" == "${webhook_re}" ]]; then
    pass "CRD marker and webhook regexp agree: ${marker}"
  else
    fail "CRD marker (${marker}) and controlPlaneReleaseRegexp (${webhook_re}) diverge"
  fi
  for crd in operators/c5c3/config/crd/bases/c5c3.io_controlplanes.yaml \
             operators/c5c3/helm/c5c3-operator/crds/c5c3.io_controlplanes.yaml; do
    if grep -qF "pattern: ${marker}" "${crd}"; then
      pass "${crd} carries pattern ${marker}"
    else
      fail "${crd} does not carry pattern ${marker} — regenerate (make manifests && make sync-crds)"
    fi
  done
fi
if grep -q 'minor != 1 && minor != 2' "${RELEASE_GO}"; then
  pass "${RELEASE_GO} enforces the two-releases-per-year minor set {1,2}"
else
  fail "${RELEASE_GO} minor-version guard changed — verify it still matches the [12] pattern class"
fi

# ---------------------------------------------------------------------------
# Optional: chain the authoritative gates
# ---------------------------------------------------------------------------
if [[ "${FULL}" -eq 1 ]]; then
  hdr "--full: authoritative gates"
  if command -v yq >/dev/null 2>&1; then
    if bash tests/container-images/verify_release_config.sh; then
      pass "tests/container-images/verify_release_config.sh"
    else
      fail "tests/container-images/verify_release_config.sh"
    fi
  else
    info "yq not on PATH — skipping verify_release_config.sh"
  fi
  if command -v jq >/dev/null 2>&1 && command -v yq >/dev/null 2>&1; then
    if GITHUB_OUTPUT=/dev/null GITHUB_EVENT_NAME=pull_request bash hack/ci-generate-build-matrix.sh \
       && GITHUB_OUTPUT=/dev/null bash hack/ci-generate-tempest-matrix.sh; then
      pass "CI matrix generators run clean"
    else
      fail "a CI matrix generator failed — a release is only partially wired"
    fi
  else
    info "jq/yq not on PATH — skipping matrix generators"
  fi
fi

# ---------------------------------------------------------------------------
hdr "Summary"
if [[ ${FAIL_COUNT} -eq 0 ]]; then
  echo "[PASS] no release-wiring findings"
  exit 0
else
  echo "[FAIL] ${FAIL_COUNT} release-wiring finding(s)"
  exit 1
fi
