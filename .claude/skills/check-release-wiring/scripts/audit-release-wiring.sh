#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# audit-release-wiring.sh — mechanical release-wiring checks for the CobaltCore repo.
# Verifies that every OpenStack release under releases/<version>/ is fully
# wired through the repo, and that no release-shaped reference points at a
# version that has no releases/ directory:
#   L1  every releases/<version>/ carries the four mandatory config files, every
#       test-excludes/*.txt maps back to a source-refs.yaml service, every
#       operator that embeds option catalogs carries catalogs/<version>.json, a
#       service pinned in upper-constraints.txt is stripped by
#       overrides/<version>/constraints.txt, and no catalog, overrides/, or
#       patches/<svc>/ directory outlives its release
#   L2  every release has tests/tempest/<svc>-<slug>/ for every service in
#       ALL_TEMPEST_SERVICES of hack/ci-generate-tempest-matrix.sh, with the
#       three text files, CR fixtures whose release pins and slugged names all
#       name that release, and a fixture for every CR name the generator emits;
#       no orphan Tempest dir survives a removed release or an unknown service
#   L3  every service with a tests/e2e/<svc>/basic-deployment suite covers every
#       release (the plain suite pins the default release, a
#       basic-deployment-<slug> variant each other one), variants pin their own
#       release, every release pin in the tests/e2e*/ fixture trees resolves, and
#       the placed-services pins match the ci.yaml e2e-multicluster preloads
#   L4  every default-release reference (deploy/kind ControlPlane, deploy-infra
#       preload, ${VAR:-YYYY.N} fallbacks in hack/, image tags in ci.yaml)
#       points at an existing releases/<version>/ directory
#   L5  the Renovate regression tests and the per-release renovate.json
#       packageRules reference only existing releases/ paths
#   L6  every release-upgrade / upgrade-flow suite tests the newest sequential
#       transition, the skip-level fixture targets a release that is neither
#       wired nor sequential, and upgrade-abort wedges on the sequential
#       successor of a wired release: pulled from a .invalid registry (holds
#       whatever releases exist), or, on the old shape, a release not wired
#   L7  the release version pattern stays in lockstep across every
#       OpenStackRelease CRD marker, the ControlPlane webhook regexp,
#       release.ParseRelease, and the generated CRD YAMLs
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
# Helpers
# ---------------------------------------------------------------------------
# first — the first stdin line. awk drains its input, so an upstream writer
# never takes a SIGPIPE that pipefail would turn into a failure (head would).
first() { awk 'NR == 1'; }

# esc <version> — the version as an ERE literal (dots escaped).
esc() { echo "${1//./\\.}"; }

# in_list <word> <list> — true when <word> is one of the whitespace-separated
# words of <list>.
in_list() {
  local w
  for w in $2; do
    [[ "${w}" == "$1" ]] && return 0
  done
  return 1
}

# slug_to_ver <slug> — 2026-1 -> 2026.1; empty when <slug> is not release-shaped.
slug_to_ver() { echo "$1" | sed -nE 's/^([0-9]{4})-([12])$/\1.\2/p'; }

# next_release <YYYY.N> — the sequential successor (release.IsSequentialUpgrade).
next_release() {
  local y="${1%%.*}" n="${1#*.}"
  if [[ "${n}" == "1" ]]; then echo "${y}.2"; else echo "$((y + 1)).1"; fi
}

# noncomment <file...> — the files' lines minus YAML/INI/shell comment lines.
noncomment() { grep -hvE '^[[:space:]]*#' "$@" 2>/dev/null || true; }

# pins_of — one "<what> <YYYY.N>" line per release pin in the stdin lines.
# <what> is the field name (tag, openStackRelease, installedRelease) or, for a
# ghcr.io/c5c3/<image>:<YYYY.N> ref, the image name. A suffixed tag
# (2025.2-upgraded) reports its release base.
pins_of() {
  grep -oE '(^|[^A-Za-z])(tag|openStackRelease|installedRelease):[[:space:]]*"?[0-9]{4}\.[12]|ghcr\.io/c5c3/[a-z0-9_-]+:[0-9]{4}\.[12]' \
    | sed -E 's/^[^A-Za-z]*//; s|^ghcr\.io/c5c3/||; s/:[[:space:]]*"?/ /' || true
}

# release_pins <file...> — pins_of over the files' non-comment lines.
release_pins() { noncomment "$@" | pins_of; }

# release_pins_list — pins_of over the non-comment lines of the files named one
# per stdin line (a list too long, or too dynamic, for positional arguments).
release_pins_list() { { xargs grep -hvE '^[[:space:]]*#' 2>/dev/null || true; } | pins_of; }

# slug_tokens <file...> — every dashed or underscored release slug (YYYY-N,
# YYYY_N) on a non-comment line: the form CR, Service, and database names carry.
slug_tokens() {
  noncomment "$@" \
    | grep -oE '(^|[^0-9])[0-9]{4}[-_][12]([^0-9]|$)' \
    | grep -oE '[0-9]{4}[-_][12]' || true
}

# first_pin <field> <file> — the first value of "<field>:" on a non-comment
# line of <file>, quotes stripped; empty when the file or the field is absent.
first_pin() {
  [[ -f "$2" ]] || return 0
  noncomment "$2" | sed -nE "s/^[[:space:]]*$1:[[:space:]]*\"?([^\"[:space:]]+)\"?.*/\1/p" | first
}

# glob_one <dir> <glob> — the single file in <dir> matching <glob>; empty when
# there is none or more than one.
glob_one() {
  local f hit="" n=0
  for f in "$1"/$2; do
    if [[ -f "${f}" ]]; then hit="${f}"; n=$((n + 1)); fi
  done
  if [[ "${n}" -eq 1 ]]; then echo "${hit}"; fi
  return 0
}

# release_exists <version> — true when releases/<version>/ is a directory.
release_exists() { [[ -d "releases/$1" ]]; }

# services_of <release> — top-level keys of source-refs.yaml, comments skipped.
services_of() {
  local refs="releases/$1/source-refs.yaml"
  [[ -f "${refs}" ]] || return 0
  grep -E '^[a-z0-9_-]+:' "${refs}" | cut -d: -f1 || true
}

# ---------------------------------------------------------------------------
# Discover releases, services, and the default release
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
RELEASES="$(echo "${RELEASES}" | tr ' ' '\n' | grep -v '^$' | sort || true)"

if [[ -z "${RELEASES}" ]]; then
  fail "no release directories found under releases/"
  echo; echo "=== Summary ==="; echo "[FAIL] 1 release-wiring finding(s)"
  exit 1
fi

DEFAULT_RELEASE="$(sed -nE 's/.*openStackRelease:[[:space:]]*"([0-9]{4}\.[12])".*/\1/p' \
  deploy/kind/controlplane/controlplane.yaml 2>/dev/null | first)"

TEMPEST_GEN="hack/ci-generate-tempest-matrix.sh"
TEMPEST_SERVICES="$(sed -nE 's/^ALL_TEMPEST_SERVICES=\(([^)]*)\).*/\1/p' "${TEMPEST_GEN}" 2>/dev/null | first)"

hdr "Inventory — releases and services"
for r in ${RELEASES}; do
  svcs="$(services_of "${r}" | tr '\n' ' ')"
  info "release ${r}: services: ${svcs:-<none>}"
done
info "default release (deploy/kind/controlplane/controlplane.yaml): ${DEFAULT_RELEASE:-<unparsed>}"
info "Tempest services (${TEMPEST_GEN} ALL_TEMPEST_SERVICES): ${TEMPEST_SERVICES:-<unparsed>}"

# ---------------------------------------------------------------------------
# L1 — mandatory release config files, test-excludes, catalogs, overrides, patches
# ---------------------------------------------------------------------------
hdr "L1: releases/<version>/ files, option catalogs, constraint overrides, patches"
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
    if in_list "${svc}" "$(services_of "${r}")"; then
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
  # A service pinned inside its own upper-constraints cannot be source-installed
  # against it; overrides/<version>/constraints.txt must strip the pin
  # (scripts/apply-constraint-overrides.sh, a "-<svc>" line).
  uc="releases/${r}/upper-constraints.txt"
  ov="overrides/${r}/constraints.txt"
  for svc in $(services_of "${r}"); do
    if [[ -f "${uc}" ]] && grep -qE "^${svc}===" "${uc}"; then
      if [[ -f "${ov}" ]] && grep -qx -e "-${svc}" "${ov}"; then
        pass "${uc} pins ${svc}; ${ov} strips it (-${svc})"
      else
        fail "${uc} pins ${svc} but ${ov} has no '-${svc}' line — the ${svc} source install fails against its own pin"
      fi
    fi
  done
done
# Service-set and test-refs drift between releases is legal (a service can be
# added in the newest release only) but worth surfacing.
first_set=""
first_refs=""
for r in ${RELEASES}; do
  set_r="$(services_of "${r}" | sort | tr '\n' ',')"
  refs_r="$(grep -E '^[a-z0-9_-]+:' "releases/${r}/test-refs.yaml" 2>/dev/null | cut -d: -f1 | sort | tr '\n' ',' || true)"
  if [[ -z "${first_set}" ]]; then
    first_set="${set_r}"
    first_refs="${refs_r}"
  else
    if [[ "${set_r}" != "${first_set}" ]]; then
      info "service sets differ across releases (${first_set} vs ${set_r}) — confirm this is intentional"
    fi
    if [[ "${refs_r}" != "${first_refs}" ]]; then
      info "test-refs.yaml key sets differ across releases (${first_refs} vs ${refs_r}) — a Tempest plugin may be missing"
    fi
  fi
done

# Per-release option catalogs. Each operator that embeds catalogs/*.json
# validates spec.extraConfig against catalogs/<release>.json; without it the
# webhook fails open for that release. verify_release_config.sh Test 8 requires
# the keystone and glance ones, build-images.yaml "Verify option catalog"
# (hack/gen-option-catalog.sh --check) the keystone, glance, neutron, cinder,
# and nova ones.
shopt -s nullglob
for cdir in operators/*/api/*/catalogs/; do
  cdir="${cdir%/}"
  op="${cdir#operators/}"
  op="${op%%/*}"
  for r in ${RELEASES}; do
    if [[ -f "${cdir}/${r}.json" ]]; then
      pass "${cdir}/${r}.json present"
    else
      fail "${cdir}/${r}.json missing — the ${op} webhook skips extraConfig validation for ${r} (hack/gen-option-catalog.sh ${op} ${r})"
    fi
  done
  for j in "${cdir}"/*.json; do
    v="$(basename "${j}" .json)"
    if ! release_exists "${v}"; then
      fail "${j} has no matching releases/${v}/ — orphan catalog of a removed release"
    fi
  done
done
# Orphan constraint overrides and patch directories; a service patched in one
# release but not another is legal (fixed upstream) but worth confirming.
for d in overrides/*/; do
  v="$(basename "${d%/}")"
  if ! release_exists "${v}"; then
    fail "overrides/${v}/ has no matching releases/${v}/ — orphan of a removed release"
  fi
done
for pdir in patches/*/; do
  svc="$(basename "${pdir%/}")"
  for d in "${pdir}"*/; do
    v="$(basename "${d%/}")"
    if ! release_exists "${v}"; then
      fail "patches/${svc}/${v}/ has no matching releases/${v}/ — orphan of a removed release"
    fi
  done
  for r in ${RELEASES}; do
    if [[ ! -d "patches/${svc}/${r}" ]]; then
      info "patches/${svc}/ carries no ${r}/ directory — ${r} builds ${svc} unpatched; confirm the fixes landed upstream"
    fi
  done
done
shopt -u nullglob

# ---------------------------------------------------------------------------
# L2 — Tempest config directories (hard CI dependency)
# ---------------------------------------------------------------------------
hdr "L2: tests/tempest/<svc>-<slug>/ per release and Tempest service (${TEMPEST_GEN} contract)"
if [[ -z "${TEMPEST_SERVICES// /}" ]]; then
  fail "${TEMPEST_GEN}: could not parse ALL_TEMPEST_SERVICES — the declaration changed shape; update this audit (no keystone-only fallback)"
else
  for r in ${RELEASES}; do
    slug="${r//./-}"
    uslug="${slug//-/_}"
    for svc in ${TEMPEST_SERVICES}; do
      tdir="tests/tempest/${svc}-${slug}"
      if [[ ! -d "${tdir}" ]]; then
        fail "${tdir} missing — ${TEMPEST_GEN} fails the whole pipeline (service ${svc}, release ${r})"
        continue
      fi
      missing=""
      for f in exclude-tests.txt include-tests.txt tempest.conf; do
        [[ -f "${tdir}/${f}" ]] || missing="${missing} ${f}"
      done
      if [[ -z "${missing}" ]]; then
        pass "${tdir}: exclude-tests.txt, include-tests.txt, tempest.conf present"
      else
        fail "${tdir}: missing${missing}"
      fi
      n_yaml="$(find "${tdir}" -maxdepth 1 -name '*.yaml' | wc -l | tr -d ' ')"
      if [[ "${n_yaml}" -eq 0 ]]; then
        fail "${tdir}: no CR fixtures (*.yaml) — the leg has nothing to deploy"
        continue
      fi
      # Every release pin (tag, openStackRelease, ghcr.io/c5c3 image refs incl.
      # the tempest client) names the directory's release; every slugged name
      # (CR, Service, hostname, database) carries its slug.
      n_pins="$(release_pins "${tdir}"/* | wc -l | tr -d ' ')"
      bad="$(release_pins "${tdir}"/* | awk -v r="${r}" '$2 != r {print $1 ":" $2}' | sort -u | tr '\n' ' ')"
      if [[ "${n_pins}" -eq 0 ]]; then
        fail "${tdir}: no release pin (tag, openStackRelease, image ref) — cannot tell which release the leg deploys"
      elif [[ -z "${bad}" ]]; then
        pass "${tdir}: ${n_yaml} CR fixtures, all ${n_pins} release pins name ${r}"
      else
        fail "${tdir}: release pins for another release: ${bad}— the clone was not re-pinned"
      fi
      bad="$(slug_tokens "${tdir}"/* | awk -v s="${slug}" -v u="${uslug}" '$0 != s && $0 != u' | sort -u | tr '\n' ' ')"
      if [[ -z "${bad}" ]]; then
        pass "${tdir}: every slugged name carries ${slug} / ${uslug}"
      else
        fail "${tdir}: names carry another release's slug: ${bad}— rename every CR, Service, and database name"
      fi
    done
  done

  # Every CR name the generator emits (cr-name, service-k8s-name, <svc>-cr-name)
  # is what the CI job waits on and port-forwards to; a fixture must define it.
  if ! command -v jq >/dev/null 2>&1; then
    info "jq not on PATH — skipping the generator CR-name cross-check"
  elif gen_out="$(GITHUB_OUTPUT=/dev/stdout TEMPEST_SERVICES='' bash "${TEMPEST_GEN}" 2>/dev/null)"; then
    names="$(printf '%s\n' "${gen_out}" | sed -n 's/^tempest-releases=//p' \
      | jq -r '.include[] | .["config-dir"] as $d | to_entries[]
               | select(.key | test("(cr-name|service-k8s-name)$")) | "\($d) \(.value)"' \
      | sort -u || true)"
    ok=0
    while read -r d n; do
      [[ -z "${d}" ]] && continue
      if grep -qE "^  name:[[:space:]]*\"?${n}\"?[[:space:]]*$" "${d}"/*.yaml 2>/dev/null; then
        ok=$((ok + 1))
      else
        fail "${d}: no fixture defines metadata.name ${n}, which ${TEMPEST_GEN} emits for the CI job to wait on"
      fi
    done <<< "${names}"
    pass "${ok} generator-emitted CR names resolve to a fixture metadata.name"
  else
    info "${TEMPEST_GEN} exits non-zero (a directory above is missing) — CR-name cross-check skipped"
  fi
fi
# Orphan Tempest dirs (release removed, or a service the generator never runs).
shopt -s nullglob
for tdir in tests/tempest/*/; do
  base="$(basename "${tdir%/}")"
  svc="$(echo "${base}" | sed -nE 's/^(.+)-[0-9]{4}-[12]$/\1/p')"
  ver="$(slug_to_ver "${base#"${svc}"-}")"
  if [[ -z "${svc}" || -z "${ver}" ]]; then
    info "tests/tempest/${base}/ is not a <svc>-<slug> config dir — skipped"
    continue
  fi
  if ! release_exists "${ver}"; then
    fail "tests/tempest/${base} has no matching releases/${ver}/ — orphan of a removed release"
  fi
  if [[ -n "${TEMPEST_SERVICES// /}" ]] && ! in_list "${svc}" "${TEMPEST_SERVICES}"; then
    fail "tests/tempest/${base} names service '${svc}', which ALL_TEMPEST_SERVICES lacks — the generator never runs it"
  fi
done
shopt -u nullglob

# ---------------------------------------------------------------------------
# L3 — per-release e2e coverage, release pins, placed-services
# ---------------------------------------------------------------------------
hdr "L3: per-release e2e coverage (basic-deployment variants, fixture pins, placed-services)"
E2E_SERVICES=""
shopt -s nullglob
for d in tests/e2e/*/basic-deployment/; do
  s="${d#tests/e2e/}"
  E2E_SERVICES="${E2E_SERVICES} ${s%%/*}"
done
shopt -u nullglob
info "services with a basic-deployment suite:${E2E_SERVICES:- <none>}"
for svc in $(services_of "$(echo "${RELEASES}" | tail -1)"); do
  if ! in_list "${svc}" "${E2E_SERVICES}"; then
    info "service ${svc} has no tests/e2e/${svc}/basic-deployment suite — no per-release e2e coverage to check"
  fi
done

# The plain suite covers the default release; basic-deployment-<slug> covers
# each other release. The service's own CR is NN-<svc>-cr.yaml in both.
for svc in ${E2E_SERVICES}; do
  plain="tests/e2e/${svc}/basic-deployment"
  cr="$(glob_one "${plain}" "[0-9][0-9]-${svc}-cr.yaml")"
  if [[ -z "${cr}" ]]; then
    fail "${plain}: no single NN-${svc}-cr.yaml — cannot tell which release the plain suite pins"
    continue
  fi
  plain_rel="$(first_pin tag "${cr}")"
  osr="$(first_pin openStackRelease "${cr}")"
  if [[ -n "${osr}" && "${osr}" != "${plain_rel}" ]]; then
    fail "${cr}: tag \"${plain_rel}\" but openStackRelease \"${osr}\""
  fi
  if ! release_exists "${plain_rel}"; then
    fail "${cr} pins \"${plain_rel}\" but releases/${plain_rel}/ does not exist"
  elif [[ -n "${DEFAULT_RELEASE}" && "${plain_rel}" != "${DEFAULT_RELEASE}" ]]; then
    fail "${cr} pins \"${plain_rel}\" but the default release is ${DEFAULT_RELEASE} — the plain suite moves with the default"
  else
    pass "${plain} pins the default release ${plain_rel}"
  fi
  bad="$(release_pins "${plain}"/* | awk -v r="${plain_rel}" '$1 != "tempest" && $2 != r {print $1 ":" $2}' | sort -u | tr '\n' ' ')"
  if [[ -n "${bad}" ]]; then
    fail "${plain}: release pins off the suite's release ${plain_rel}: ${bad}"
  fi
  for r in ${RELEASES}; do
    vdir="tests/e2e/${svc}/basic-deployment-${r//./-}"
    if [[ "${plain_rel}" == "${r}" ]]; then
      pass "${svc}: release ${r} covered by the plain basic-deployment suite"
      if [[ -d "${vdir}" ]]; then
        info "${vdir} duplicates the plain suite's release ${r}"
      fi
    elif [[ -f "${vdir}/chainsaw-test.yaml" ]]; then
      pass "${svc}: release ${r} covered by ${vdir}"
    else
      fail "${svc}: release ${r} has no basic-deployment coverage — neither the plain suite (pins ${plain_rel}) nor ${vdir}/chainsaw-test.yaml"
    fi
  done
done
# Variant-internal consistency + orphan variants. The tempest client image a
# variant's Jobs run is exempt: the e2e legs load one tempest tag for every
# release (the tempest-pin sweep below checks that it resolves).
shopt -s nullglob
for vdir in tests/e2e/*/basic-deployment-*/; do
  vdir="${vdir%/}"
  svc="${vdir#tests/e2e/}"
  svc="${svc%%/*}"
  slug="${vdir##*/basic-deployment-}"
  ver="$(slug_to_ver "${slug}")"
  if [[ -z "${ver}" ]]; then
    info "${vdir} does not end in a release slug — skipped"
    continue
  fi
  if ! release_exists "${ver}"; then
    fail "${vdir} has no matching releases/${ver}/ — orphan of a removed release"
    continue
  fi
  cr="$(glob_one "${vdir}" "[0-9][0-9]-${svc}-cr.yaml")"
  tag="$(first_pin tag "${cr}")"
  if [[ -z "${cr}" ]]; then
    fail "${vdir}: no single NN-${svc}-cr.yaml — cannot tell which release the variant pins"
  elif [[ "${tag}" == "${ver}" ]]; then
    pass "${cr} pins tag \"${ver}\""
  else
    fail "${cr} pins tag \"${tag}\" but the variant directory is for release ${ver}"
  fi
  bad="$(release_pins "${vdir}"/* | awk -v r="${ver}" '$1 != "tempest" && $2 != r {print $1 ":" $2}' | sort -u | tr '\n' ' ')"
  if [[ -z "${bad}" ]]; then
    pass "${vdir}: every release pin (fields, service image refs) names ${ver}"
  else
    fail "${vdir}: release pins for another release: ${bad}— the variant was cloned without re-pinning"
  fi
  bad="$(slug_tokens "${vdir}"/* | awk -v s="${slug}" -v u="${slug//-/_}" '$0 != s && $0 != u' | sort -u | tr '\n' ' ')"
  if [[ -z "${bad}" ]]; then
    pass "${vdir}: every slugged name carries ${slug}"
  else
    fail "${vdir}: names carry another release's slug: ${bad}— rename every CR and database name"
  fi
done
shopt -u nullglob

# Every release pin in the chainsaw fixture trees resolves. This is the sweep a
# retired release leaves behind (fixtures, helper CRs, tempest client images).
# The two fixtures that must name a non-wired release are checked in L6.
PIN_FILES="$(find tests/e2e*/ -type f -name '*.yaml' \
  ! -name '*-patch-skip-level.yaml' ! -name '*-patch-stuck-upgrade.yaml' 2>/dev/null | sort)"
sweep="$(printf '%s\n' "${PIN_FILES}" | release_pins_list | awk '{print $2}' | sort | uniq -c)"
while read -r n v; do
  [[ -z "${v}" ]] && continue
  if release_exists "${v}"; then
    pass "${n} release pins under tests/e2e*/ name ${v} (exists)"
  else
    ve="$(esc "${v}")"
    files="$(printf '%s\n' "${PIN_FILES}" \
      | xargs grep -lE "(tag|openStackRelease|installedRelease):[[:space:]]*\"?${ve}|ghcr\.io/c5c3/[a-z0-9_-]+:${ve}" \
      | awk 'NR <= 5' | tr '\n' ' ' || true)"
    fail "${n} release pins under tests/e2e*/ name ${v}, which has no releases/${v}/ (e.g. ${files})"
  fi
done <<< "${sweep}"
tempest_tags="$(printf '%s\n' "${PIN_FILES}" | release_pins_list | awk '$1 == "tempest" {print $2}' | sort -u | tr '\n' ' ')"
info "tempest client image tags the e2e fixtures run: ${tempest_tags:-<none>}"

# The two-cluster placed-services fixtures pin one release per service image
# (repository + tag); the ci.yaml e2e-multicluster job preloads exactly those
# images into kind, so the two sides move together.
MC_DIR="tests/e2e-multicluster/placed-services"
if [[ -d "${MC_DIR}" ]]; then
  mc_pairs="$(awk '
    /^[[:space:]]*#/ { next }
    /repository:[[:space:]]*ghcr\.io\/c5c3\// {
      img = $0; sub(/.*ghcr\.io\/c5c3\//, "", img); sub(/[[:space:]"].*$/, "", img); next
    }
    img != "" && /^[[:space:]]*tag:/ {
      v = $0; sub(/.*tag:[[:space:]]*"?/, "", v); sub(/[";[:space:]].*$/, "", v); print img, v; img = ""
    }' "${MC_DIR}"/*.yaml | sort -u)"
  mc_job="$(awk '/^  e2e-multicluster:[[:space:]]*$/ {f = 1; next}
                 f && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ {f = 0}
                 f' .github/workflows/ci.yaml)"
  if [[ -z "${mc_job}" ]]; then
    fail ".github/workflows/ci.yaml: no e2e-multicluster job block — the job moved or was renamed; update this audit"
  else
    while read -r img v; do
      [[ -z "${img}" ]] && continue
      if grep -qF "/${img}:${v}" <<< "${mc_job}"; then
        pass "${MC_DIR} pins ${img}:${v} and the ci.yaml e2e-multicluster job preloads it"
      else
        fail "${MC_DIR} pins ${img}:${v} but the ci.yaml e2e-multicluster job preloads no /${img}:${v}"
      fi
    done <<< "${mc_pairs}"
    job_pairs="$(grep -oE '/[a-z0-9_-]+:[0-9]{4}\.[12]' <<< "${mc_job}" | sed 's|^/||; s/:/ /' | sort -u || true)"
    while read -r img v; do
      [[ -z "${img}" ]] && continue
      pin="$(awk -v i="${img}" '$1 == i {print $2}' <<< "${mc_pairs}" | first)"
      if [[ -z "${pin}" ]]; then
        info "the ci.yaml e2e-multicluster job preloads ${img}:${v}, which no ${MC_DIR} fixture pins"
      elif [[ "${pin}" != "${v}" ]]; then
        fail "the ci.yaml e2e-multicluster job preloads ${img}:${v} but ${MC_DIR} pins ${img}:${pin}"
      fi
    done <<< "${job_pairs}"
  fi
fi

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

check_ref deploy/kind/controlplane/controlplane.yaml "${DEFAULT_RELEASE}" "openStackRelease"

v="$(sed -nE 's/.*cp_release="([0-9]{4}\.[12])".*/\1/p' hack/deploy-infra.sh | first)"
check_ref hack/deploy-infra.sh "${v}" "cp_release preload default"

NAMED_FALLBACKS="hack/ci-build-service-image.sh hack/ci-build-tempest-image.sh hack/run-tempest.sh"
for f in ${NAMED_FALLBACKS}; do
  v="$(sed -nE 's/.*RELEASE:-([0-9]{4}\.[12]).*/\1/p' "${f}" | first)"
  check_ref "${f}" "${v}" "RELEASE fallback"
done
# Any other ${VAR:-YYYY.N} default in hack/ (e.g. perf-reconcile-benchmark.sh
# IMAGE_TAG) names the default release too.
other_fallbacks="$(grep -oHE '\$\{[A-Z_]+:-[0-9]{4}\.[12]\}' hack/*.sh 2>/dev/null || true)"
while IFS= read -r line; do
  [[ -z "${line}" ]] && continue
  f="${line%%:*}"
  in_list "${f}" "${NAMED_FALLBACKS}" && continue
  var="$(echo "${line}" | sed -nE 's/.*\$\{([A-Z_]+):-.*/\1/p')"
  v="$(echo "${line}" | sed -nE 's/.*:-([0-9]{4}\.[12])\}.*/\1/p')"
  check_ref "${f}" "${v}" "${var} fallback"
done <<< "${other_fallbacks}"

# Image tags of the form <name>:<YYYY.N> hard-coded in ci.yaml (upgrade
# re-tagging, kind image preloads). Colon-anchored so prose dates do not match.
ci_versions="$(grep -oE ':[0-9]{4}\.[12]' .github/workflows/ci.yaml | tr -d ':' | sort -u || true)"
if [[ -z "${ci_versions}" ]]; then
  info ".github/workflows/ci.yaml: no hard-coded image-tag releases found"
else
  for v in ${ci_versions}; do
    check_ref .github/workflows/ci.yaml "${v}" "hard-coded image tag"
  done
fi

# ---------------------------------------------------------------------------
# L5 — Renovate regression tests and per-release rules reference existing releases
# ---------------------------------------------------------------------------
hdr "L5: tests/unit/renovate/ and renovate.json referenced release paths exist"
shopt -s nullglob
for t in tests/unit/renovate/*_test.sh renovate.json; do
  refs="$(grep -oE 'releases/[0-9]{4}\.[12]' "${t}" | sort -u || true)"
  for ref in ${refs}; do
    v="${ref#releases/}"
    if release_exists "${v}"; then
      pass "${t}: references ${ref} (exists)"
    elif [[ "${t}" == "renovate.json" ]]; then
      fail "${t}: a packageRule matches ${ref}/, which does not exist — dead per-release rule"
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
newest="$(echo "${RELEASES}" | tail -1)"
prev=""
if [[ "${release_count}" -ge 2 ]]; then
  prev="$(echo "${RELEASES}" | tail -2 | first)"
  info "newest transition: ${prev} -> ${newest}"
else
  info "fewer than two releases — no upgrade transition to cover"
fi
# release-upgrade (every service) and upgrade-flow (keystone): the service's own
# NN-<svc>-cr.yaml holds the from release, NN-patch-upgrade.yaml the to release;
# helper CRs and chainsaw assertions may name only those two.
shopt -s nullglob
for base in tests/e2e/*/release-upgrade tests/e2e/*/upgrade-flow; do
  [[ -d "${base}" ]] || continue
  svc="${base#tests/e2e/}"
  svc="${svc%%/*}"
  from_f="$(glob_one "${base}" "[0-9][0-9]-${svc}-cr.yaml")"
  to_f="$(glob_one "${base}" "[0-9][0-9]-patch-upgrade.yaml")"
  from="$(first_pin tag "${from_f}")"
  to="$(first_pin tag "${to_f}")"
  if [[ -z "${from}" || -z "${to}" ]]; then
    fail "${base}: cannot read the transition (NN-${svc}-cr.yaml tag -> NN-patch-upgrade.yaml tag) — shape changed, update this audit"
    continue
  fi
  for pair in "${from_f}:${from}" "${to_f}:${to}"; do
    f="${pair%:*}"
    osr="$(first_pin openStackRelease "${f}")"
    if [[ -n "${osr}" && "${osr}" != "${pair##*:}" ]]; then
      fail "${f}: tag \"${pair##*:}\" but openStackRelease \"${osr}\""
    fi
  done
  if [[ -z "${prev}" ]]; then
    info "${base} tests ${from} -> ${to}"
  elif [[ "${from}" == "${prev}" && "${to}" == "${newest}" ]]; then
    pass "${base} tests ${from} -> ${to} (newest transition)"
  else
    fail "${base} tests ${from} -> ${to} but the newest transition is ${prev} -> ${newest}"
  fi
  bad="$(find "${base}" -maxdepth 1 -type f ! -name '*-patch-skip-level.yaml' | release_pins_list | awk -v a="${from}" -v b="${to}" '$1 != "tempest" && $2 != a && $2 != b {print $1 ":" $2}' | sort -u | tr '\n' ' ')"
  if [[ -n "${bad}" ]]; then
    fail "${base}: release pins outside ${from} -> ${to}: ${bad}"
  fi
done
# The skip-level rejection test needs a target that is neither wired nor the
# sequential successor of the release the suite upgraded to.
for skip_f in tests/e2e/*/upgrade-flow/*-patch-skip-level.yaml; do
  base="${skip_f%/*}"
  svc="${base#tests/e2e/}"
  svc="${svc%%/*}"
  skip="$(first_pin tag "${skip_f}")"
  to="$(first_pin tag "$(glob_one "${base}" "[0-9][0-9]-patch-upgrade.yaml")")"
  if [[ -z "${skip}" ]]; then
    fail "${skip_f}: no tag — shape changed, update this audit"
  elif release_exists "${skip}"; then
    fail "${skip_f} targets ${skip}, which now EXISTS under releases/ — the rejection test no longer tests a skip"
  elif [[ -n "${to}" && "${skip}" == "$(next_release "${to}")" ]]; then
    fail "${skip_f} targets ${skip}, the sequential successor of ${to} — the webhook accepts it; move the target past ${skip}"
  else
    pass "${skip_f} targets ${skip} (not wired, not sequential from ${to:-?})"
  fi
done
# upgrade-abort wedges an upgrade on the sequential successor of its start
# release. The stuck patch points spec.image.repository at a registry under the
# reserved .invalid TLD, so the db-expand pull fails whether or not the target
# release has an image; the abort patch restores the start repository and tag.
# A stuck patch on the real repository only wedges while the target is not
# wired (the leg loads <svc>:<release> for every releases/ entry through
# hack/ci-service-image-releases.sh), so that older shape is still checked.
for base in tests/e2e/*/upgrade-abort; do
  [[ -d "${base}" ]] || continue
  svc="${base#tests/e2e/}"
  svc="${svc%%/*}"
  cr_f="$(glob_one "${base}" "[0-9][0-9]-${svc}-cr.yaml")"
  stuck_f="$(glob_one "${base}" "[0-9][0-9]-patch-stuck-upgrade.yaml")"
  abort_f="$(glob_one "${base}" "[0-9][0-9]-patch-abort.yaml")"
  from="$(first_pin tag "${cr_f}")"
  stuck="$(first_pin tag "${stuck_f}")"
  from_repo="$(first_pin repository "${cr_f}")"
  stuck_repo="$(first_pin repository "${stuck_f}")"
  abort_repo="$(first_pin repository "${abort_f}")"
  abort_tag="$(first_pin tag "${abort_f}")"
  stuck_host="${stuck_repo%%/*}"
  if [[ -z "${from}" || -z "${stuck}" ]]; then
    fail "${base}: cannot read the stuck transition (NN-${svc}-cr.yaml -> NN-patch-stuck-upgrade.yaml) — shape changed, update this audit"
  elif ! release_exists "${from}"; then
    fail "${base} starts at ${from}, which has no releases/${from}/ — the start image is never loaded"
  elif [[ "${stuck}" != "$(next_release "${from}")" ]]; then
    fail "${base} upgrades ${from} -> ${stuck}, not a sequential transition — the webhook rejects it instead of wedging"
  elif [[ "${stuck_host}" == *.invalid ]]; then
    if [[ "${abort_tag}" != "${from}" || ( -n "${from_repo}" && "${abort_repo}" != "${from_repo}" ) ]]; then
      fail "${abort_f:-${base}/NN-patch-abort.yaml} must restore ${from_repo:-the start repository}:${from}, found ${abort_repo:-<unchanged>}:${abort_tag:-<none>} — the abort would pull from ${stuck_host} too"
    else
      pass "${base} wedges ${from} -> ${stuck} on ${stuck_host} (independent of releases/) and aborts back to ${from}"
    fi
  elif release_exists "${stuck}"; then
    fail "${base} wedges on ${stuck}, which is now wired — its image is loaded and the upgrade completes; point the stuck patch's repository at a .invalid registry"
  else
    pass "${base} wedges ${from} -> ${stuck} (sequential, not wired; a .invalid repository in the stuck patch would make it independent of releases/)"
  fi
done
shopt -u nullglob

# ---------------------------------------------------------------------------
# L7 — release version pattern lockstep
# ---------------------------------------------------------------------------
hdr "L7: version pattern lockstep (CRD markers / webhook regexp / ParseRelease / CRD YAMLs)"
TYPES_GO="operators/c5c3/api/v1alpha1/controlplane_types.go"
WEBHOOK_GO="operators/c5c3/api/v1alpha1/controlplane_webhook.go"
RELEASE_GO="internal/common/release/release.go"

# release_markers <types.go> — the Pattern marker in the comment block directly
# above each OpenStackRelease field, "<none>" for a field without one.
release_markers() {
  awk '
    /^[[:space:]]*\/\// {
      if (index($0, "kubebuilder:validation:Pattern=") > 0) {
        p = $0; sub(/.*Pattern=`/, "", p); sub(/`.*/, "", p); pat = p
      }
      next
    }
    /^[[:space:]]*OpenStackRelease[[:space:]]+\*?string/ {
      print (pat == "" ? "<none>" : pat); pat = ""; next
    }
    { pat = "" }' "$1"
}

marker="$(release_markers "${TYPES_GO}" | first)"
webhook_re="$(grep 'controlPlaneReleaseRegexp = ' "${WEBHOOK_GO}" | sed -nE 's/.*MustCompile\(`([^`]+)`\).*/\1/p' | first)"
if [[ -z "${marker}" || "${marker}" == "<none>" || -z "${webhook_re}" ]]; then
  fail "could not extract the version pattern from ${TYPES_GO} / ${WEBHOOK_GO} — shape changed, update this audit"
else
  if [[ "${marker}" == "${webhook_re}" ]]; then
    pass "ControlPlane CRD marker and webhook regexp agree: ${marker}"
  else
    fail "ControlPlane CRD marker (${marker}) and controlPlaneReleaseRegexp (${webhook_re}) diverge"
  fi
  # Every service CR's openStackRelease carries the same marker.
  for tf in $(grep -lE 'OpenStackRelease[[:space:]]+\*?string' operators/*/api/*/*_types.go 2>/dev/null || true); do
    [[ "${tf}" == "${TYPES_GO}" ]] && continue
    for m in $(release_markers "${tf}"); do
      if [[ "${m}" == "${marker}" ]]; then
        pass "${tf}: OpenStackRelease marker ${m}"
      else
        fail "${tf}: OpenStackRelease marker ${m} diverges from ${TYPES_GO} (${marker})"
      fi
    done
  done
  # Every generated CRD carrying an openStackRelease property carries the pattern.
  for crd in $(grep -lE '^[[:space:]]+openStackRelease:[[:space:]]*$' \
                 operators/*/config/crd/bases/*.yaml operators/*/helm/*/crds/*.yaml 2>/dev/null || true); do
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
