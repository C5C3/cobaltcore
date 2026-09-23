#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# Inventory the touch points for an OpenStack release across the repo:
# release config files, per-operator option catalogs, service patches,
# constraint overrides, the Tempest config directory of every service in
# ALL_TEMPEST_SERVICES, the per-service basic-deployment e2e variant, and — for
# the newest release — the upgrade-path suites, plus the global default-release
# decision points.
#
# Usage: inventory-release-touchpoints.sh [<version>]
#
# With <version> (e.g. 2026.2) the inventory targets that release — for a
# release being added, everything is [TODO] by design. Without an argument
# it walks every existing releases/<version>/ directory.
#
# Prints [DONE]/[TODO] per touch point plus decision-point reminders. This
# is an inventory, not a gate: it exits 0 unless invoked incorrectly (2) or a
# source-of-truth file changed shape so the inventory cannot be trusted (1).

set -euo pipefail

if [[ $# -gt 1 ]] || { [[ $# -eq 1 ]] && [[ ! "$1" =~ ^[0-9]{4}\.[12]$ ]]; }; then
  echo "usage: $0 [<version>]   (YYYY.N with N in {1,2}, e.g. 2026.2)" >&2
  exit 2
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
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

# first — the first stdin line; awk drains its input, so no writer upstream
# takes a SIGPIPE that pipefail would turn into a failure (head would).
first() { awk 'NR == 1'; }

# noncomment <file...> — the files' lines minus YAML/INI/shell comment lines.
noncomment() { grep -hvE '^[[:space:]]*#' "$@" 2>/dev/null || true; }

# release_pins <file...> — one "<what> <YYYY.N>" line per release pin on a
# non-comment line: the tag/openStackRelease/installedRelease field name, or
# the image name of a ghcr.io/c5c3/<image>:<YYYY.N> ref.
release_pins() {
  noncomment "$@" \
    | grep -oE '(^|[^A-Za-z])(tag|openStackRelease|installedRelease):[[:space:]]*"?[0-9]{4}\.[12]|ghcr\.io/c5c3/[a-z0-9_-]+:[0-9]{4}\.[12]' \
    | sed -E 's/^[^A-Za-z]*//; s|^ghcr\.io/c5c3/||; s/:[[:space:]]*"?/ /' || true
}

# pins_only <release> <dir> <exempt-tempest:0|1> — true when <dir> carries at
# least one release pin and every one names <release>. The e2e suites run one
# tempest client tag for every release, so their variants exempt it.
pins_only() {
  local verdict
  [[ -d "$2" ]] || return 1
  verdict="$(release_pins "$2"/* | awk -v r="$1" -v ex="$3" '
    !(ex == 1 && $1 == "tempest") { n++; if ($2 != r) bad++ }
    END { print ((n > 0 && bad == 0) ? "yes" : "no") }')"
  [[ "${verdict}" == "yes" ]]
}

# first_pin <field> <file> — the first value of "<field>:" on a non-comment
# line of <file>, quotes stripped; empty when the file or the field is absent.
first_pin() {
  [[ -f "$2" ]] || return 0
  noncomment "$2" | sed -nE "s/^[[:space:]]*$1:[[:space:]]*\"?([^\"[:space:]]+)\"?.*/\1/p" | first
}

# glob_one <dir> <glob> — the single file in <dir> matching <glob>, else empty.
glob_one() {
  local f hit="" n=0
  for f in "$1"/$2; do
    if [[ -f "${f}" ]]; then hit="${f}"; n=$((n + 1)); fi
  done
  if [[ "${n}" -eq 1 ]]; then echo "${hit}"; fi
  return 0
}

# next_release <YYYY.N> — the sequential successor (release.IsSequentialUpgrade).
next_release() {
  local y="${1%%.*}" n="${1#*.}"
  if [[ "${n}" == "1" ]]; then echo "${y}.2"; else echo "$((y + 1)).1"; fi
}

# suite_transition <suite-dir> — "<from> <to>" of a release-upgrade/upgrade-flow
# suite: the tag of the service's own NN-<svc>-cr.yaml and of NN-patch-upgrade.yaml.
suite_transition() {
  local svc="${1#tests/e2e/}"
  svc="${svc%%/*}"
  echo "$(first_pin tag "$(glob_one "$1" "[0-9][0-9]-${svc}-cr.yaml")") $(first_pin tag "$(glob_one "$1" "[0-9][0-9]-patch-upgrade.yaml")")"
}

# same_pair <a> <b> <want-a> <want-b> — true when a == want-a and b == want-b.
same_pair() { [[ "$1" == "$3" && "$2" == "$4" ]]; }

# skip_target_ok <skip> <to> — true when the skip-level target is set, not
# wired under releases/, and neither <to> nor its sequential successor.
skip_target_ok() {
  [[ -n "$1" && "$1" != "$2" && "$1" != "$(next_release "$2")" && ! -d "releases/$1" ]]
}

# Services = union of source-refs.yaml keys across all existing releases.
SERVICES="$(cat releases/*/source-refs.yaml 2>/dev/null \
  | grep -E '^[a-z0-9_-]+:' | cut -d: -f1 | sort -u || true)"
if [[ -z "${SERVICES}" ]]; then
  echo "error: no service keys in releases/*/source-refs.yaml — nothing to inventory" >&2
  exit 1
fi

# Tempest-covered services, straight from the matrix generator that
# hard-requires their directories. No fallback: a keystone-only guess would
# hide five of six required directories.
TEMPEST_GEN="hack/ci-generate-tempest-matrix.sh"
TEMPEST_SERVICES="$(sed -nE 's/^ALL_TEMPEST_SERVICES=\(([^)]*)\).*/\1/p' "${TEMPEST_GEN}" 2>/dev/null | first)"
if [[ -z "${TEMPEST_SERVICES// /}" ]]; then
  echo "error: could not parse ALL_TEMPEST_SERVICES from ${TEMPEST_GEN} — the declaration changed shape; update this inventory" >&2
  exit 1
fi

EXISTING="$(find releases -mindepth 1 -maxdepth 1 -type d 2>/dev/null | sed 's|^releases/||' | sort)"
# Newest existing release (YYYY.N sorts correctly lexicographically) — the
# template for a not-yet-created release.
NEWEST_EXISTING="$(echo "${EXISTING}" | tail -1)"

if [[ $# -eq 1 ]]; then
  VERSIONS="$1"
else
  VERSIONS="${EXISTING}"
fi
# The release set once the target exists; the upgrade path covers its newest pair.
ALL_RELEASES="$(printf '%s\n%s\n' "${EXISTING}" "${VERSIONS}" | grep -v '^$' | sort -u)"
NEWEST="$(echo "${ALL_RELEASES}" | tail -1)"

E2E_SERVICES=""
shopt -s nullglob
for d in tests/e2e/*/basic-deployment/; do
  s="${d#tests/e2e/}"
  E2E_SERVICES="${E2E_SERVICES} ${s%%/*}"
done
CATALOG_DIRS=""
for d in operators/*/api/*/catalogs/; do
  CATALOG_DIRS="${CATALOG_DIRS} ${d%/}"
done
PATCH_SERVICES=""
for d in patches/*/; do
  d="${d%/}"
  PATCH_SERVICES="${PATCH_SERVICES} ${d##*/}"
done
UPGRADE_SUITES=""
for d in tests/e2e/*/release-upgrade tests/e2e/*/upgrade-flow; do
  [[ -d "${d}" ]] && UPGRADE_SUITES="${UPGRADE_SUITES} ${d}"
done
shopt -u nullglob

for v in ${VERSIONS}; do
  slug="${v//./-}"
  printf '\n########## release %s ##########\n' "${v}"

  header "Release config (releases/${v}/)"
  check "releases/${v}/source-refs.yaml (activates build/test matrices)" \
    test -f "releases/${v}/source-refs.yaml"
  check "releases/${v}/test-refs.yaml (tempest + plugin PyPI pins)" \
    test -f "releases/${v}/test-refs.yaml"
  if [[ "${v}" != "${NEWEST_EXISTING}" && -f "releases/${NEWEST_EXISTING}/test-refs.yaml" ]]; then
    for key in $(grep -E '^[a-z0-9_-]+:' "releases/${NEWEST_EXISTING}/test-refs.yaml" | cut -d: -f1); do
      check "releases/${v}/test-refs.yaml pins ${key} (as releases/${NEWEST_EXISTING}/ does)" \
        grep -qE "^${key}:" "releases/${v}/test-refs.yaml"
    done
  fi
  check "releases/${v}/extra-packages.yaml (per-service pip/apt extras)" \
    test -f "releases/${v}/extra-packages.yaml"
  check "releases/${v}/upper-constraints.txt (upstream constraints snapshot)" \
    test -f "releases/${v}/upper-constraints.txt"
  # Excludes are optional; only a service some release already excludes tests
  # for needs its list carried over and re-triaged.
  for svc in ${SERVICES}; do
    if compgen -G "releases/*/test-excludes/${svc}.txt" >/dev/null; then
      check "releases/${v}/test-excludes/${svc}.txt (carry over, re-triage per series)" \
        test -f "releases/${v}/test-excludes/${svc}.txt"
    fi
  done

  header "Option catalogs (hack/gen-option-catalog.sh <op> ${v})"
  for cdir in ${CATALOG_DIRS}; do
    check "${cdir}/${v}.json" test -f "${cdir}/${v}.json"
  done

  header "Service patches (re-triage each: carry over, or drop once fixed upstream)"
  for svc in ${PATCH_SERVICES}; do
    note=""
    if [[ "${v}" != "${NEWEST_EXISTING}" ]]; then
      n="$(find "patches/${svc}/${NEWEST_EXISTING}" -name '*.patch' 2>/dev/null | wc -l | tr -d ' ')"
      note=" (${NEWEST_EXISTING} carries ${n} patch(es))"
    fi
    check "patches/${svc}/${v}/${note}" test -d "patches/${svc}/${v}"
  done

  header "Constraint overrides (overrides/${v}/constraints.txt)"
  uc="releases/${v}/upper-constraints.txt"
  [[ -f "${uc}" ]] || uc="releases/${NEWEST_EXISTING}/upper-constraints.txt"
  ov="overrides/${v}/constraints.txt"
  found_pin=0
  for svc in ${SERVICES}; do
    if [[ -f "${uc}" ]] && grep -Eq "^${svc}===" "${uc}"; then
      found_pin=1
      check "${ov} strips the ${svc} pin ${uc} carries (-${svc}, scripts/apply-constraint-overrides.sh)" \
        grep -qx -e "-${svc}" "${ov}"
    fi
  done
  if [[ "${found_pin}" -eq 0 ]]; then
    printf '  [INFO] no service is pinned in %s — no strip line needed\n' "${uc}"
  fi
  prev_ov="overrides/${NEWEST_EXISTING}/constraints.txt"
  if [[ "${v}" != "${NEWEST_EXISTING}" && -f "${prev_ov}" ]]; then
    for pkg in $(grep -E '^[A-Za-z0-9_.-]+===' "${prev_ov}" | sed 's/===.*//'); do
      check "${ov} pins ${pkg} (as ${prev_ov} does)" grep -qE "^${pkg}===" "${ov}"
    done
  fi

  header "Tempest (hard CI dependency — every service in ${TEMPEST_GEN} ALL_TEMPEST_SERVICES)"
  for svc in ${TEMPEST_SERVICES}; do
    tdir="tests/tempest/${svc}-${slug}"
    check "${tdir}/ directory" test -d "${tdir}"
    for f in exclude-tests.txt include-tests.txt tempest.conf; do
      check "${tdir}/${f}" test -f "${tdir}/${f}"
    done
    check "${tdir}/ CR fixtures pin \"${v}\" everywhere (tags, openStackRelease, image refs)" \
      pins_only "${v}" "${tdir}" 0
    check "docs/reference/testing/tempest-test-infrastructure.md lists ${tdir}/" \
      grep -qF "\`${tdir}/\`" docs/reference/testing/tempest-test-infrastructure.md
  done

  header "Per-release e2e coverage (tests/e2e/<svc>/basic-deployment)"
  for svc in ${E2E_SERVICES}; do
    plain_tag="$(first_pin tag "$(glob_one "tests/e2e/${svc}/basic-deployment" "[0-9][0-9]-${svc}-cr.yaml")")"
    vdir="tests/e2e/${svc}/basic-deployment-${slug}"
    if [[ "${plain_tag}" == "${v}" ]]; then
      printf '  [DONE] %s: covered by the plain basic-deployment suite (tag "%s")\n' "${svc}" "${v}"
      DONE_COUNT=$((DONE_COUNT + 1))
    else
      check "${vdir}/ variant (chainsaw-test.yaml)" test -f "${vdir}/chainsaw-test.yaml"
      check "${vdir}/ pins \"${v}\" everywhere (tempest client image exempt)" \
        pins_only "${v}" "${vdir}" 1
    fi
  done

  header "Upgrade path (the newest sequential transition)"
  if [[ "${v}" != "${NEWEST}" ]]; then
    printf '  [INFO] %s is not the newest release (%s) — the upgrade suites cover the newest transition only\n' "${v}" "${NEWEST}"
  else
    prev="$(echo "${ALL_RELEASES}" | grep -vx "${v}" | tail -1 || true)"
    if [[ -z "${prev}" ]]; then
      printf '  [INFO] no older release — no transition to cover\n'
    else
      for suite in ${UPGRADE_SUITES}; do
        read -r from to <<< "$(suite_transition "${suite}")"
        check "${suite} tests ${prev} -> ${v} (now ${from:-?} -> ${to:-?})" \
          same_pair "${from:-}" "${to:-}" "${prev}" "${v}"
      done
      for skip_f in tests/e2e/*/upgrade-flow/*-patch-skip-level.yaml; do
        [[ -f "${skip_f}" ]] || continue
        skip="$(first_pin tag "${skip_f}")"
        check "${skip_f} targets a release neither wired nor sequential from ${v} (now ${skip:-?})" \
          skip_target_ok "${skip}" "${v}"
      done
      for base in tests/e2e/*/upgrade-abort; do
        [[ -d "${base}" ]] || continue
        svc="${base#tests/e2e/}"
        svc="${svc%%/*}"
        from="$(first_pin tag "$(glob_one "${base}" "[0-9][0-9]-${svc}-cr.yaml")")"
        stuck_f="$(glob_one "${base}" "[0-9][0-9]-patch-stuck-upgrade.yaml")"
        stuck="$(first_pin tag "${stuck_f}")"
        stuck_repo="$(first_pin repository "${stuck_f}")"
        if [[ "${stuck_repo%%/*}" == *.invalid ]]; then
          # The stuck pull targets an unresolvable registry, so the suite does
          # not care which releases exist; it only has to start at a wired one.
          check "${base} starts at a wired release and wedges on its successor via ${stuck_repo%%/*} (now ${from:-?} -> ${stuck:-?})" \
            same_pair "${from}" "${stuck}" "${from}" "$(next_release "${from:-0000.1}")"
        else
          check "${base} wedges ${v} -> $(next_release "${v}") on a target the e2e leg loads no image for (now ${from:-?} -> ${stuck:-?})" \
            same_pair "${from}" "${stuck}" "${v}" "$(next_release "${v}")"
        fi
      done
    fi
  fi
done

header "Decision points (global — review, not per-release TODOs)"
printf '  [INFO] default ControlPlane release: deploy/kind/controlplane/controlplane.yaml -> %s\n' \
  "$(sed -nE 's/.*openStackRelease:[[:space:]]*"([^"]+)".*/\1/p' deploy/kind/controlplane/controlplane.yaml | first)"
printf '  [INFO] deploy-infra image preload: hack/deploy-infra.sh cp_release -> %s\n' \
  "$(sed -nE 's/.*cp_release="([^"]+)".*/\1/p' hack/deploy-infra.sh | first)"
grep -oHE '\$\{[A-Z_]+:-[0-9]{4}\.[12]\}' hack/*.sh 2>/dev/null \
  | sed -E 's/^([^:]+):\$\{([A-Z_]+):-([0-9.]+)\}$/  [INFO] \2 fallback: \1 -> \3/' || true
printf '  [INFO] ci.yaml hard-coded image tags: %s\n' \
  "$(grep -oE ':[0-9]{4}\.[12]' .github/workflows/ci.yaml | tr -d ':' | sort -u | tr '\n' ' ' || true)"
for svc in ${E2E_SERVICES}; do
  printf '  [INFO] plain basic-deployment suite: tests/e2e/%s/basic-deployment -> %s\n' "${svc}" \
    "$(first_pin tag "$(glob_one "tests/e2e/${svc}/basic-deployment" "[0-9][0-9]-${svc}-cr.yaml")")"
done
# Bash 3.2 misparses a multi-line pipeline with nested quotes inside a quoted
# command substitution, so the count is built by a function first.
fixture_pin_counts() {
  find tests/e2e*/ -type f -name '*.yaml' -exec grep -hvE '^[[:space:]]*#' {} + 2>/dev/null \
    | grep -oE '(tag|openStackRelease):[[:space:]]*"?[0-9]{4}\.[12]|ghcr\.io/c5c3/[a-z0-9_-]+:[0-9]{4}\.[12]' \
    | grep -oE '[0-9]{4}\.[12]$' | sort | uniq -c | awk '{ printf("%s x%s  ", $2, $1) }' || true
}
pin_counts="$(fixture_pin_counts)"
printf '  [INFO] release pins under tests/e2e*/ (a default move or a retire sweeps them): %s\n' "${pin_counts}"
printf '  [INFO] tempest client image the e2e fixtures run: %s\n' \
  "$(grep -rhoE 'ghcr\.io/c5c3/tempest:[0-9]{4}\.[12]' tests/e2e*/ 2>/dev/null | sort -u | tr '\n' ' ' || true)"
printf '  [INFO] e2e-multicluster placed-services fixtures pin: %s\n' \
  "$(grep -rhoE '(tag|openStackRelease):[[:space:]]*"[0-9]{4}\.[12]"' tests/e2e-multicluster/ 2>/dev/null \
    | grep -oE '[0-9]{4}\.[12]' | sort -u | tr '\n' ' ' || true)"
printf '  [INFO] renovate regression tests probe: %s\n' \
  "$(grep -ohE 'releases/[0-9]{4}\.[12]' tests/unit/renovate/*_test.sh 2>/dev/null | sort -u | tr '\n' ' ' || true)"
printf '  [INFO] renovate.json per-release packageRules: %s\n' \
  "$(grep -oE 'releases/[0-9]{4}\.[12]/[a-z-]+\.yaml' renovate.json 2>/dev/null | sort -u | tr '\n' ' ' || true)"
for suite in ${UPGRADE_SUITES}; do
  read -r from to <<< "$(suite_transition "${suite}")"
  printf '  [INFO] upgrade suite %s tests: %s -> %s\n' "${suite}" "${from:-?}" "${to:-?}"
done
printf '  [INFO] docs code-import a release-slugged fixture: %s\n' \
  "$(grep -rhoE '@/\.\./tests/[a-z0-9/_.-]*[0-9]{4}-[12][a-z0-9/_.-]*' docs --include='*.md' 2>/dev/null | sort -u | tr '\n' ' ' || true)"

printf '\nSummary: %d done, %d todo (inventory only — todo is expected for a release being added)\n' \
  "${DONE_COUNT}" "${TODO_COUNT}"
exit 0
