#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# next-fixture-number.sh — where the next invalid-cr rejection fixture goes.
# For each invalid-cr corpus it prints the next free ordinal prefix and checks
# the wiring a new fixture has to keep intact:
#   - the _EXPECTED_FIXTURE_COUNT pin in test_generate.py equals the number of
#     Fixture(filename="NN-...") entries in _generate.py (bump both together)
#   - chainsaw-test.yaml exists beside the generator
#   - make verify-invalid-cr-fixtures runs both `_generate.py --check` and
#     `test_generate.py` for the corpus
#   - a corpus at or past prefix 100 accepts three-digit prefixes: the orphan
#     sweep regex in _generate.py and any two-digit slice or regex in
#     test_generate.py must be widened (c5c3/invalid-cr is the precedent)
# Given an operator name it walks every tests/e2e/<op>/invalid-*/ corpus and
# also reports which of the operator's API symbols the ControlPlane
# (operators/c5c3/api/v1alpha1) embeds or calls, because a rule on those
# reaches the ControlPlane CRD, its webhook, and tests/e2e/c5c3/invalid-cr too.
#
# Read-only. Usage:
#   next-fixture-number.sh <operator>|<corpus-dir> [...]
#   e.g. next-fixture-number.sh nova
#        next-fixture-number.sh tests/e2e/cinder/invalid-cinderbackend-cr
# Exit code 1 on any [FAIL].

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "${REPO_ROOT}"

usage() {
  echo "usage: $0 <operator>|<corpus-dir> [...]" >&2
  exit 2
}
[[ $# -gt 0 ]] || usage

FAIL_COUNT=0
fail() { echo "[FAIL] $*"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
pass() { echo "[PASS] $*"; }
info() { echo "[INFO] $*"; }
hdr()  { echo; echo "=== $* ==="; }

# Recipe lines of the make target, up to the next non-recipe line.
MAKE_TARGET_BODY=$(awk '/^verify-invalid-cr-fixtures:/{on=1; next} on && /^[^\t]/{exit} on' Makefile)

report_corpus() {
  local dir="$1"
  local gen="${dir}/_generate.py" tst="${dir}/test_generate.py"
  hdr "${dir}"

  if [[ ! -d "${dir}" ]]; then
    info "no corpus yet: copy _generate.py, test_generate.py and chainsaw-test.yaml from a sibling corpus"
    info "(tests/e2e/nova/invalid-cr is the newest single-kind one), start at prefix 00, and add"
    info "'python3 ${gen} --check' and 'python3 ${tst}' to make verify-invalid-cr-fixtures"
    return 0
  fi

  local prefixes count highest next dups gaps
  prefixes=$(find "${dir}" -maxdepth 1 -type f \
    \( -name '[0-9][0-9]-*.yaml' -o -name '[0-9][0-9][0-9]-*.yaml' \) \
    -exec basename {} \; | awk -F- '{print $1}' | sort -n)
  count=$(printf '%s\n' "${prefixes}" | grep -c . || true)
  highest=$(printf '%s\n' "${prefixes}" | awk 'NF { if ($1 + 0 > m) m = $1 + 0; seen = 1 } END { print seen ? m : -1 }')
  next=$((highest + 1))
  if [[ "${next}" -lt 100 ]]; then
    next=$(printf '%02d' "${next}")
  fi
  info "${count} fixture(s) on disk; next free prefix: ${next} -> ${dir}/${next}-<rule>.yaml"

  dups=$(printf '%s\n' "${prefixes}" | grep . | uniq -d | tr '\n' ' ' || true)
  if [[ -n "${dups}" ]]; then
    info "shared prefixes (hand-numbered history; give the new fixture a unique one): ${dups}"
  fi
  gaps=$(printf '%s\n' "${prefixes}" | awk 'NF { n = $1 + 0; have[n] = 1; if (n > m) m = n }
    END { for (i = 0; i < m; i++) if (!(i in have)) printf "%02d ", i }')
  if [[ -n "${gaps}" ]]; then
    info "unused prefixes below the highest (reuse one only to keep a rule family together): ${gaps}"
  fi

  if [[ ! -f "${gen}" ]]; then
    fail "${gen} missing: the corpus has no generator"
    return 0
  fi
  local declared pinned
  declared=$(grep -cE 'filename="[0-9]{2,3}-' "${gen}" || true)
  if [[ ! -f "${tst}" ]]; then
    fail "${tst} missing: nothing pins the fixture count or the chainsaw cross-reference"
  else
    pinned=$(sed -n 's/^_EXPECTED_FIXTURE_COUNT = \([0-9][0-9]*\).*/\1/p' "${tst}" | head -1)
    if [[ -z "${pinned}" ]]; then
      info "${tst} carries no _EXPECTED_FIXTURE_COUNT pin"
    elif [[ "${pinned}" == "${declared}" ]]; then
      pass "_EXPECTED_FIXTURE_COUNT = ${pinned} matches the ${declared} FIXTURES entries; a new fixture bumps it to $((declared + 1))"
    else
      fail "_EXPECTED_FIXTURE_COUNT = ${pinned} in ${tst}, but ${gen} declares ${declared} fixture(s)"
    fi
  fi
  if [[ ! -f "${dir}/chainsaw-test.yaml" ]]; then
    fail "${dir}/chainsaw-test.yaml missing: no step applies the fixtures"
  fi

  if grep -qF "python3 ${gen} --check" <<< "${MAKE_TARGET_BODY}" \
    && grep -qF "python3 ${tst}" <<< "${MAKE_TARGET_BODY}"; then
    pass "make verify-invalid-cr-fixtures runs both _generate.py --check and test_generate.py"
  else
    fail "make verify-invalid-cr-fixtures does not run both 'python3 ${gen} --check' and 'python3 ${tst}'"
  fi

  # Three-digit prefixes: the orphan sweep regex and two-digit slices/regexes
  # in test_generate.py silently skip or mis-number NNN- files.
  local has_three=0 narrow
  if grep -qE '^[0-9]{3}$' <<<"${prefixes}"; then
    has_three=1
  fi
  if [[ "${has_three}" -eq 1 || "${highest}" -ge 99 ]]; then
    narrow=$( (grep -nE '_FIXTURE_FILENAME_PATTERN = re\.compile\(r"\^\[0-9\]\{2\}-' "${gen}" | sed "s|^|${gen}:|";
      [[ -f "${tst}" ]] && grep -nE '\[:2\]|\[0-9\]\{2\}-' "${tst}" | sed "s|^|${tst}:|") || true)
    if [[ -z "${narrow}" ]]; then
      pass "generator and unit test accept three-digit prefixes"
    else
      while IFS= read -r line; do
        [[ -z "${line}" ]] && continue
        if [[ "${has_three}" -eq 1 ]]; then
          fail "two-digit prefix assumption with NNN- fixtures on disk: ${line}"
        else
          info "widen to [0-9]{2,3} before adding prefix 100: ${line}"
        fi
      done <<< "${narrow}"
    fi
  fi
}

# report_controlplane_reach prints the <op> API symbols the ControlPlane API
# package uses: embedded types (their markers and CEL are copied into the
# ControlPlane CRD by controller-gen), delegated validators and bounds, and the
# extraConfig ownership tables.
report_controlplane_reach() {
  local op="$1" f alias syms
  hdr "ControlPlane reach of operators/${op}/api/v1alpha1"
  local found=0
  for f in operators/c5c3/api/v1alpha1/controlplane_types.go \
    operators/c5c3/api/v1alpha1/controlplane_webhook.go \
    operators/c5c3/api/v1alpha1/controlplane_extraconfig.go; do
    [[ -f "${f}" ]] || continue
    alias=$(awk -v p="cobaltcore/operators/${op}/api/v1alpha1\"" 'index($0, p) && NF == 2 { print $1; exit }' "${f}")
    [[ -n "${alias}" ]] || continue
    # Comment lines are skipped: doc comments name validators the file never calls.
    syms=$(grep -v '^[[:space:]]*//' "${f}" | grep -oE "${alias}\.[A-Z][A-Za-z0-9_]*" \
      | sed "s/^${alias}\.//" | sort -u | tr '\n' ' ' || true)
    [[ -n "${syms}" ]] || continue
    found=1
    info "$(basename "${f}") uses: ${syms}"
  done
  if [[ "${found}" -eq 0 ]]; then
    info "the ControlPlane API does not reference ${op}'s API package"
  else
    info "a rule on an embedded type regenerates into c5c3.io_controlplanes.yaml; a rule on a"
    info "projected field needs a c5c3 webhook twin and a tests/e2e/c5c3/invalid-cr fixture"
  fi
}

for arg in "$@"; do
  case "${arg}" in
    -h|--help) usage ;;
  esac
  arg="${arg%/}"
  arg="${arg#"${REPO_ROOT}"/}"
  if [[ "${arg}" != */* && -d "operators/${arg}" ]]; then
    corpora=$(find "tests/e2e/${arg}" -maxdepth 1 -type d -name 'invalid-*' 2>/dev/null | sort || true)
    if [[ -z "${corpora}" ]]; then
      report_corpus "tests/e2e/${arg}/invalid-cr"
    else
      while IFS= read -r d; do
        report_corpus "${d}"
      done <<< "${corpora}"
    fi
    if [[ "${arg}" != "c5c3" ]]; then
      report_controlplane_reach "${arg}"
    fi
  else
    report_corpus "${arg}"
  fi
done

echo
if [[ "${FAIL_COUNT}" -gt 0 ]]; then
  echo "${FAIL_COUNT} failure(s)"
  exit 1
fi
echo "no failures"
