#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# audit-fixture-drift.sh — mechanical fixture-drift checks for the CobaltCore repo.
# Verifies that test fixtures still match the CRD they claim to instantiate:
#   X1  every CobaltCore CR fixture names a known kind on a served apiVersion
#   X2  every spec field in such a fixture exists in that CRD's schema
#   X3  every <NN>-*.yaml next to a chainsaw-test.yaml is referenced from it
#   X4  every file referenced from a chainsaw-test.yaml exists
#   X5  invalid-cr generator gate (make verify-invalid-cr-fixtures)
#   X6  every invalid-cr generator is wired into that make target, and every
#       path the target names exists on disk
#
# X1/X2 cover every CRD kind in the repo, not just Keystone, and read every
# document of a multi-document fixture. They need a YAML parser, so they live
# in the check_fixture_schema.py helper beside this script.
#
# Pass --full to chain make verify-invalid-cr-fixtures. Exit code 1 on [FAIL].

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

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# strip_yaml_comments — drop YAML comments from a file so that a filename
# mentioned only in prose does not count as a step reference (X3) or as a step
# that must exist (X4). In YAML a '#' opens a comment at line start or after
# whitespace; anywhere else it is part of a scalar.
strip_yaml_comments() {
  sed -E 's/(^|[[:space:]])#.*$/\1/' "$1"
}

if ! compgen -G "operators/*/config/crd/bases/*.yaml" > /dev/null; then
  fail "no CRDs under operators/*/config/crd/bases/ — run: make manifests"
  exit 1
fi

# ---------------------------------------------------------------------------
# X1 + X2 — schema validation of every CobaltCore CR fixture (needs a YAML
# parser: fixtures are multi-document and the CRD schema is deeply nested).
# ---------------------------------------------------------------------------
SCHEMA_HELPER="${SCRIPT_DIR}/check_fixture_schema.py"
if ! command -v python3 > /dev/null 2>&1; then
  hdr "X1+X2: fixture schema validation"
  info "python3 not on PATH — X1/X2 skipped"
elif ! python3 -c 'import yaml' > /dev/null 2>&1; then
  hdr "X1+X2: fixture schema validation"
  info "PyYAML not importable — X1/X2 skipped (pip install pyyaml)"
else
  schema_out=$(python3 "${SCHEMA_HELPER}" 2>&1 || true)
  echo "${schema_out}"
  schema_fails=$(echo "${schema_out}" | grep -c '^\[FAIL\]' || true)
  FAIL_COUNT=$((FAIL_COUNT + schema_fails))
fi

# ---------------------------------------------------------------------------
# X3 — every <NN>-*.yaml next to a chainsaw-test.yaml is referenced
# ---------------------------------------------------------------------------
hdr "X3: every <NN>-*.yaml is referenced from its sibling chainsaw-test.yaml"
CHAINSAW_DIRS_LIST=$(find tests -name 'chainsaw-test.yaml' -exec dirname {} \; 2>/dev/null | sort -u || true)
while IFS= read -r d; do
  [[ -z "${d}" ]] && continue
  ct="${d}/chainsaw-test.yaml"
  [[ -f "${ct}" ]] || continue
  ct_body=$(strip_yaml_comments "${ct}")
  shopt -s nullglob
  for fx in "${d}"/[0-9][0-9]-*.yaml; do
    base=$(basename "${fx}")
    if grep -qF "${base}" <<< "${ct_body}"; then
      pass "${d}/${base}: referenced from chainsaw-test.yaml"
    else
      fail "${d}/${base}: orphan — not referenced from chainsaw-test.yaml"
    fi
  done
  shopt -u nullglob
done <<< "${CHAINSAW_DIRS_LIST}"

# ---------------------------------------------------------------------------
# X4 — every file referenced from a chainsaw-test.yaml exists
# ---------------------------------------------------------------------------
hdr "X4: every <NN>-*.yaml referenced from chainsaw-test.yaml exists on disk"
while IFS= read -r d; do
  [[ -z "${d}" ]] && continue
  ct="${d}/chainsaw-test.yaml"
  [[ -f "${ct}" ]] || continue
  # Reference style is typically: file: ./00-foo.yaml  OR  file: 00-foo.yaml.
  # Comments are stripped first: a step name discussed in prose (an
  # intentionally absent fixture, say) is not a reference that must resolve.
  refs=$(strip_yaml_comments "${ct}" | grep -oE '[0-9]{2}-[A-Za-z0-9_-]+\.yaml' | sort -u || true)
  for ref in ${refs}; do
    if [[ -f "${d}/${ref}" ]]; then
      pass "${d}: chainsaw step ${ref} exists"
    else
      fail "${d}: chainsaw step ${ref} referenced but missing on disk"
    fi
  done
done <<< "${CHAINSAW_DIRS_LIST}"

# ---------------------------------------------------------------------------
# X5 — invalid-cr generator gate (only with --full or explicit Python)
# ---------------------------------------------------------------------------
hdr "X5: invalid-cr generator gate (make verify-invalid-cr-fixtures)"
if [[ "${FULL}" -eq 1 ]]; then
  if command -v python3 >/dev/null 2>&1; then
    if make verify-invalid-cr-fixtures; then
      pass "make verify-invalid-cr-fixtures: clean"
    else
      fail "make verify-invalid-cr-fixtures: drift detected"
    fi
  else
    info "python3 not on PATH — skipping X5"
  fi
else
  info "skipped (run with --full to chain make verify-invalid-cr-fixtures)"
fi

# ---------------------------------------------------------------------------
# X6 — every invalid-cr generator is wired into the make target, both ways
# ---------------------------------------------------------------------------
hdr "X6: every invalid-cr generator is wired into make verify-invalid-cr-fixtures"
GENERATORS_LIST=$(find tests -name '_generate.py' 2>/dev/null | sort || true)
# The recipe lines of the target, up to the next non-recipe line.
MAKE_TARGET_BODY=$(awk '/^verify-invalid-cr-fixtures:/{on=1; next} on && /^[^\t]/{exit} on' Makefile)
if [[ -z "${MAKE_TARGET_BODY}" ]]; then
  fail "no verify-invalid-cr-fixtures recipe found in Makefile"
else
  while IFS= read -r gen; do
    [[ -z "${gen}" ]] && continue
    d=$(dirname "${gen}")
    if grep -qF "${gen} --check" <<< "${MAKE_TARGET_BODY}"; then
      pass "${gen}: run with --check by the make target"
    else
      fail "${gen}: exists but the make target never runs it — the corpus has no drift gate"
    fi
    if [[ ! -f "${d}/test_generate.py" ]]; then
      fail "${d}: has _generate.py but no test_generate.py"
    elif grep -qF "${d}/test_generate.py" <<< "${MAKE_TARGET_BODY}"; then
      pass "${d}/test_generate.py: run by the make target"
    else
      fail "${d}/test_generate.py: exists but the make target never runs it"
    fi
  done <<< "${GENERATORS_LIST}"
  # Reverse direction: every path the target names still exists.
  while IFS= read -r ref; do
    [[ -z "${ref}" ]] && continue
    if [[ -f "${ref}" ]]; then
      pass "make target path ${ref} exists"
    else
      fail "make target runs ${ref}, which does not exist on disk"
    fi
  done <<< "$(grep -oE 'tests/[A-Za-z0-9_/-]+\.py' <<< "${MAKE_TARGET_BODY}" | sort -u || true)"
fi

# ---------------------------------------------------------------------------
# Inventory
# ---------------------------------------------------------------------------
hdr "Inventory — Chainsaw test directories (review aid)"
while IFS= read -r d; do
  [[ -z "${d}" ]] && continue
  fx_count=$(find "${d}" -maxdepth 1 -name '[0-9][0-9]-*.yaml' | wc -l | tr -d ' ')
  info "${d}: ${fx_count} fixture(s)"
done <<< "${CHAINSAW_DIRS_LIST}"

hdr "Inventory — invalid-cr corpora (review aid)"
while IFS= read -r gen; do
  [[ -z "${gen}" ]] && continue
  d=$(dirname "${gen}")
  fx_count=$(find "${d}" -maxdepth 1 -name '[0-9][0-9]-*.yaml' | wc -l | tr -d ' ')
  # Every <NN>-*.yaml the generator names, whether in its FIXTURES list or in
  # an exemption set (keystone/invalid-cr carries two pre-CC-0094 fixtures it
  # deliberately does not regenerate). A file on disk that the generator never
  # names is outside the --check gate.
  named=$(grep -oE '[0-9]{2}-[A-Za-z0-9_-]+\.yaml' "${gen}" 2>/dev/null | sort -u | grep -c . || true)
  info "${d}: ${fx_count} fixture(s) on disk, ${named} named in $(basename "${gen}")"
done <<< "${GENERATORS_LIST}"

# ---------------------------------------------------------------------------
hdr "Summary"
if [[ ${FAIL_COUNT} -eq 0 ]]; then
  echo "[PASS] no fixture-drift findings"
  exit 0
else
  echo "[FAIL] ${FAIL_COUNT} fixture-drift finding(s)"
  exit 1
fi
