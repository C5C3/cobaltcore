#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# audit-condition-coverage.sh — mechanical condition-coverage checks for
# the Keystone operator:
#   K1  every condition-type literal set in reconcile_*.go is registered in
#       subReconcilerConditionTypes
#   K2  every conditionType<X>Ready constant is defined once and used in code
#   K3  every reconcile_*.go has a paired _test.go
#   K4  every condition type set in code is documented
#   K5  every condition type referenced in docs is set in code
#
# Defers Go-AST-aware checks to the existing drift-guard tests:
#   TestSubReconcilerConditionTypesCoversAllNames
#   TestSubReconcilerConditionTypesCoversAllCallSites
# Exit code 1 on [FAIL].

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "${REPO_ROOT}"

FAIL_COUNT=0
fail() { echo "[FAIL] $*"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
pass() { echo "[PASS] $*"; }
info() { echo "[INFO] $*"; }
hdr()  { echo; echo "=== $* ==="; }

CONTROLLER_DIR="operators/keystone/internal/controller"
INSTRUMENTATION="${CONTROLLER_DIR}/instrumentation.go"
CRD_DOC="architecture/docs/09-implementation/03-crd-implementation.md"

if [[ ! -f "${INSTRUMENTATION}" ]]; then
  fail "missing ${INSTRUMENTATION}"
  exit 1
fi

# Build the union of literal condition types declared in the map (right-hand side).
# Captures both `"FooReady"` and `conditionTypeFooReady` (the latter is resolved
# below by reading const definitions).
map_lits=$(grep -oE '"[A-Z][A-Za-z]+Ready"' "${INSTRUMENTATION}" | tr -d '"' | sort -u)
map_consts=$(grep -oE 'conditionType[A-Z][A-Za-z]+Ready' "${INSTRUMENTATION}" | sort -u)

info "literal condition types in instrumentation map:"
echo "${map_lits}" | sed 's/^/[INFO]   /'
info "const-ref condition types in instrumentation map:"
echo "${map_consts}" | sed 's/^/[INFO]   /'

# Resolve const ⇢ string from any *.go file in the controller package.
resolve_const() {
  local name="$1"
  grep -hE "^[[:space:]]*${name}[[:space:]]*=[[:space:]]*\"[A-Z][A-Za-z]+Ready\"" "${CONTROLLER_DIR}"/*.go 2>/dev/null \
    | sed -nE "s/.*${name}[[:space:]]*=[[:space:]]*\"([A-Z][A-Za-z]+Ready)\".*/\1/p" \
    | head -1
}

# Resolved string set (literals plus resolved constants).
resolved_set="${map_lits}"
while IFS= read -r c; do
  [[ -z "${c}" ]] && continue
  v=$(resolve_const "${c}")
  if [[ -n "${v}" ]]; then
    resolved_set="${resolved_set}"$'\n'"${v}"
  else
    info "could not resolve const ${c} to a literal — confirm by hand"
  fi
done <<< "${map_consts}"
resolved_set=$(echo "${resolved_set}" | sort -u)

# ---------------------------------------------------------------------------
# K1 — every literal Type: "<X>Ready" in reconcile_*.go is in the map (resolved)
# ---------------------------------------------------------------------------
hdr "K1: every literal Type: \"<X>Ready\" set in reconcile_*.go is registered"
callsite_lits=$(grep -hrE 'Type:[[:space:]]+"[A-Z][A-Za-z]+Ready"' "${CONTROLLER_DIR}"/reconcile_*.go 2>/dev/null \
  | grep -oE '"[A-Z][A-Za-z]+Ready"' | tr -d '"' | sort -u || true)
for t in ${callsite_lits}; do
  if echo "${resolved_set}" | grep -qx "${t}"; then
    pass "callsite literal ${t} is registered in instrumentation map"
  else
    fail "callsite literal ${t} is NOT in instrumentation map — Prometheus condition_type will resolve to UNKNOWN"
  fi
done

# ---------------------------------------------------------------------------
# K2 — every conditionType<X>Ready constant is defined exactly once and used
# ---------------------------------------------------------------------------
hdr "K2: every conditionType<X>Ready constant is defined once and used"
all_const_refs=$(grep -hrEo 'conditionType[A-Z][A-Za-z]+Ready' "${CONTROLLER_DIR}"/*.go 2>/dev/null \
  | sort -u || true)
for c in ${all_const_refs}; do
  defs=$(grep -hcE "^[[:space:]]*${c}[[:space:]]*=" "${CONTROLLER_DIR}"/*.go 2>/dev/null \
    | awk -F: '{s+=$1} END {print s+0}')
  uses=$(grep -hcE "\\b${c}\\b" "${CONTROLLER_DIR}"/*.go 2>/dev/null \
    | awk -F: '{s+=$1} END {print s+0}')
  if [[ "${defs}" -eq 0 ]]; then
    fail "const ${c} referenced but never defined"
  elif [[ "${defs}" -gt 1 ]]; then
    fail "const ${c} defined ${defs} times — must be unique"
  elif [[ "${uses}" -le 1 ]]; then
    info "const ${c} defined but referenced only ${uses} time(s) — potential dead code"
  else
    pass "const ${c} defined once, referenced ${uses} times"
  fi
done

# ---------------------------------------------------------------------------
# K3 — every reconcile_*.go has a paired _test.go
# ---------------------------------------------------------------------------
hdr "K3: every reconcile_*.go has a paired _test.go"
for f in "${CONTROLLER_DIR}"/reconcile_*.go; do
  case "${f}" in
    *_test.go) continue ;;
  esac
  paired="${f%.go}_test.go"
  if [[ -f "${paired}" ]]; then
    pass "$(basename "${f}") has paired test $(basename "${paired}")"
  else
    fail "$(basename "${f}") has no paired test (expected $(basename "${paired}"))"
  fi
done

# ---------------------------------------------------------------------------
# K4 — every condition type set in code is mentioned in the CRD doc
# ---------------------------------------------------------------------------
hdr "K4: every condition type set in code is mentioned in ${CRD_DOC}"
if [[ -f "${CRD_DOC}" ]]; then
  while IFS= read -r t; do
    [[ -z "${t}" ]] && continue
    if grep -q "${t}" "${CRD_DOC}"; then
      pass "${t} documented"
    else
      fail "${t} set in code but absent from ${CRD_DOC}"
    fi
  done <<< "${resolved_set}"
else
  info "missing ${CRD_DOC} — skipping K4"
fi

# ---------------------------------------------------------------------------
# K5 — every condition type referenced in the doc is set in code
# ---------------------------------------------------------------------------
hdr "K5: every condition type referenced in ${CRD_DOC} is set in code"
if [[ -f "${CRD_DOC}" ]]; then
  doc_types=$(grep -oE '\b[A-Z][A-Za-z]+Ready\b' "${CRD_DOC}" 2>/dev/null | sort -u || true)
  for t in ${doc_types}; do
    if echo "${resolved_set}" | grep -qx "${t}"; then
      pass "${t} (doc) is set somewhere in code"
    else
      # Could be set via constant we did not resolve, or could be stale doc.
      if grep -rqE "\"${t}\"|conditionType${t}|${t}\b" "${CONTROLLER_DIR}"; then
        info "${t} (doc) appears in code but not via the instrumentation map — confirm by hand"
      else
        fail "${t} (doc) is not set anywhere in code — stale documentation?"
      fi
    fi
  done
else
  info "missing ${CRD_DOC} — skipping K5"
fi

# ---------------------------------------------------------------------------
# Inventory — per-reconciler condition types and tests
# ---------------------------------------------------------------------------
hdr "Inventory — sub-reconcilers and the condition types they set"
for f in "${CONTROLLER_DIR}"/reconcile_*.go; do
  case "${f}" in
    *_test.go) continue ;;
  esac
  fname=$(basename "${f}")
  lits=$(grep -oE 'Type:[[:space:]]+"[A-Z][A-Za-z]+Ready"' "${f}" 2>/dev/null \
    | grep -oE '"[A-Z][A-Za-z]+Ready"' | tr -d '"' | sort -u | tr '\n' ',' | sed 's/,$//')
  cnst=$(grep -oE 'Type:[[:space:]]+conditionType[A-Z][A-Za-z]+Ready' "${f}" 2>/dev/null \
    | grep -oE 'conditionType[A-Z][A-Za-z]+Ready' | sort -u | tr '\n' ',' | sed 's/,$//')
  info "${fname}: literals=[${lits}] consts=[${cnst}]"
done

# ---------------------------------------------------------------------------
hdr "Summary"
if [[ ${FAIL_COUNT} -eq 0 ]]; then
  echo "[PASS] no condition-coverage findings"
  exit 0
else
  echo "[FAIL] ${FAIL_COUNT} condition-coverage finding(s)"
  exit 1
fi
