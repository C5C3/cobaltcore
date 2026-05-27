#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# audit-fixture-drift.sh — mechanical fixture-drift checks for the forge repo.
# Verifies that test fixtures still match the CRD they claim to instantiate:
#   X1  every kind: Keystone fixture uses the current apiVersion
#   X2  every Spec field in a kind: Keystone fixture appears in the CRD schema
#   X3  every <NN>-*.yaml next to a chainsaw-test.yaml is referenced from it
#   X4  every file referenced from a chainsaw-test.yaml exists
#   X5  invalid-cr generator gate (make verify-invalid-cr-fixtures)
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

CRD_FILE="operators/keystone/config/crd/bases/keystone.openstack.c5c3.io_keystones.yaml"
EXPECTED_APIVERSION="keystone.openstack.c5c3.io/v1alpha1"

if [[ ! -f "${CRD_FILE}" ]]; then
  fail "missing CRD file ${CRD_FILE} — run: make manifests"
  exit 1
fi

# Discover all Keystone fixtures (kind: Keystone) under tests/.
KEYSTONE_FIXTURES_LIST=$(grep -lrE '^kind:[[:space:]]*Keystone[[:space:]]*$' tests/ 2>/dev/null | sort -u || true)
KEYSTONE_FIXTURES_COUNT=$(echo "${KEYSTONE_FIXTURES_LIST}" | grep -c . || true)
info "Found ${KEYSTONE_FIXTURES_COUNT} kind: Keystone fixture(s) under tests/"

# ---------------------------------------------------------------------------
# X1 — every fixture uses the current apiVersion
# ---------------------------------------------------------------------------
hdr "X1: every kind: Keystone fixture uses ${EXPECTED_APIVERSION}"
while IFS= read -r f; do
  [[ -z "${f}" ]] && continue
  apiver=$(grep -E '^apiVersion:' "${f}" | head -1 | awk '{print $2}')
  if [[ "${apiver}" == "${EXPECTED_APIVERSION}" ]]; then
    pass "${f}: apiVersion ${apiver}"
  else
    fail "${f}: apiVersion='${apiver}' but expected '${EXPECTED_APIVERSION}'"
  fi
done <<< "${KEYSTONE_FIXTURES_LIST}"

# ---------------------------------------------------------------------------
# X2 — every top-level Spec field in fixtures appears in the CRD schema
# ---------------------------------------------------------------------------
hdr "X2: top-level spec fields used in fixtures appear in the CRD schema"
# Extract the set of top-level spec field names from the CRD by reading the
# openAPIV3Schema properties list. We rely on the controller-gen output style:
# a section header `      spec:` followed by `        properties:` and then
# 10-space-indented `<field>:` lines.
crd_fields=$(awk '
  /^      spec:/{in_spec=1; next}
  in_spec && /^        properties:/{in_props=1; next}
  in_props && /^          [a-z][A-Za-z0-9]+:/{
    # capture field name
    gsub(/^[[:space:]]+/,""); sub(":.*","",$0); print
  }
  in_props && /^        [a-z]/{exit}
' "${CRD_FILE}" | sort -u)

if [[ -z "${crd_fields}" ]]; then
  info "could not extract spec properties from ${CRD_FILE} — X2 skipped (parse heuristic mismatched)"
else
  info "CRD spec top-level fields: $(echo "${crd_fields}" | tr '\n' ',' | sed 's/,$//')"
  while IFS= read -r f; do
  [[ -z "${f}" ]] && continue
    # Skip patch files (Chainsaw partial objects without a full spec block) — heuristic:
    # files whose name starts with the patch convention "<NN>-patch-…".
    base=$(basename "${f}")
    if [[ "${base}" =~ ^[0-9]+-patch- ]]; then
      info "${f}: skipped (patch fixture; partial spec by design)"
      continue
    fi
    # Extract top-level spec field names — first level under `spec:` (2-space indent).
    fixture_fields=$(awk '
      /^spec:/{in_spec=1; next}
      in_spec && /^[a-zA-Z]/{exit}
      in_spec && /^  [a-z][A-Za-z0-9]+:/{
        gsub(/^[[:space:]]+/,""); sub(":.*","",$0); print
      }
    ' "${f}" | sort -u)
    [[ -z "${fixture_fields}" ]] && continue
    fail_in_file=0
    for fld in ${fixture_fields}; do
      if echo "${crd_fields}" | grep -qx "${fld}"; then
        :
      else
        fail "${f}: spec field '${fld}' not in CRD schema"
        fail_in_file=$((fail_in_file + 1))
      fi
    done
    if [[ "${fail_in_file}" -eq 0 ]]; then
      pass "${f}: all $(echo "${fixture_fields}" | wc -w | tr -d ' ') top-level spec fields exist in CRD"
    fi
  done <<< "${KEYSTONE_FIXTURES_LIST}"
fi

# ---------------------------------------------------------------------------
# X3 — every <NN>-*.yaml next to a chainsaw-test.yaml is referenced
# ---------------------------------------------------------------------------
hdr "X3: every <NN>-*.yaml is referenced from its sibling chainsaw-test.yaml"
CHAINSAW_DIRS_LIST=$(find tests/e2e tests/e2e-chaos -name 'chainsaw-test.yaml' 2>/dev/null | xargs -n1 dirname 2>/dev/null | sort -u || true)
while IFS= read -r d; do
  [[ -z "${d}" ]] && continue
  ct="${d}/chainsaw-test.yaml"
  [[ -f "${ct}" ]] || continue
  shopt -s nullglob
  for fx in "${d}"/[0-9][0-9]-*.yaml; do
    base=$(basename "${fx}")
    if grep -q "${base}" "${ct}"; then
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
  # Reference style is typically: file: ./00-foo.yaml  OR  file: 00-foo.yaml
  refs=$(grep -oE '[0-9]{2}-[A-Za-z0-9_-]+\.yaml' "${ct}" | sort -u || true)
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
# Inventory
# ---------------------------------------------------------------------------
hdr "Inventory — Chainsaw test directories (review aid)"
while IFS= read -r d; do
  [[ -z "${d}" ]] && continue
  fx_count=$(find "${d}" -maxdepth 1 -name '[0-9][0-9]-*.yaml' | wc -l | tr -d ' ')
  info "${d}: ${fx_count} fixture(s)"
done <<< "${CHAINSAW_DIRS_LIST}"

hdr "Inventory — invalid-cr fixtures (review aid)"
shopt -s nullglob
for fx in tests/e2e/keystone/invalid-cr/[0-9][0-9]-*.yaml; do
  # Pull the REQ-NNN and feature ID from the SPDX/comment header.
  hint=$(grep -m1 -E 'REQ-[0-9]+|CC-[0-9]+' "${fx}" 2>/dev/null | head -1 | sed 's/^[#[:space:]]*//')
  info "$(basename "${fx}"): ${hint}"
done
shopt -u nullglob

# ---------------------------------------------------------------------------
hdr "Summary"
if [[ ${FAIL_COUNT} -eq 0 ]]; then
  echo "[PASS] no fixture-drift findings"
  exit 0
else
  echo "[FAIL] ${FAIL_COUNT} fixture-drift finding(s)"
  exit 1
fi
