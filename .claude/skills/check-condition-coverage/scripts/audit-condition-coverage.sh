#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# audit-condition-coverage.sh — mechanical condition-coverage checks for
# every forge operator that wires sub-reconciler instrumentation
# (discovered via operators/*/internal/controller/instrumentation.go):
#   K1  every condition type set in reconcile_*.go (literal or resolved
#       conditionType<X>Ready constant) is registered in
#       subReconcilerConditionTypes; the bare aggregate "Ready" is exempt
#   K2  every conditionType<X>Ready constant is defined once and used in code
#   K3  every reconcile_*.go has a paired _test.go
#   K4  every condition type in the instrumentation map is documented
#   K5  every condition type referenced in docs is set in code
#
# The per-operator doc corpus is docs/reference/<op>/*.md; keystone
# additionally consults architecture/docs/09-implementation/03-crd-implementation.md
# when the architecture/ submodule is checked out. Operators without a
# reference-doc directory skip K4/K5 with [INFO].
#
# Defers Go-AST-aware checks to the existing per-operator drift-guard test:
#   TestSubReconcilerConditionTypesCoversAllNames
# Exit code 1 on [FAIL].

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "${REPO_ROOT}"

FAIL_COUNT=0
fail() { echo "[FAIL] $*"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
pass() { echo "[PASS] $*"; }
info() { echo "[INFO] $*"; }
hdr()  { echo; echo "=== $* ==="; }

# Discover instrumented operators.
OPERATORS=()
for instr in operators/*/internal/controller/instrumentation.go; do
  [[ -f "${instr}" ]] || continue
  op="${instr#operators/}"
  op="${op%%/*}"
  OPERATORS+=("${op}")
done

if [[ ${#OPERATORS[@]} -eq 0 ]]; then
  fail "no operators/*/internal/controller/instrumentation.go found"
  exit 1
fi
info "instrumented operators: ${OPERATORS[*]}"

# Resolve const ⇢ string from any *.go file in the given controller package.
# Accepts the bare aggregate value "Ready" as well as "<X>Ready".
resolve_const() {
  local dir="$1" name="$2"
  grep -hE "^[[:space:]]*(const[[:space:]]+)?${name}[[:space:]]*=[[:space:]]*\"[A-Za-z]*Ready\"" "${dir}"/*.go 2>/dev/null \
    | sed -nE "s/.*${name}[[:space:]]*=[[:space:]]*\"([A-Za-z]*Ready)\".*/\1/p" \
    | head -1
}

for op in "${OPERATORS[@]}"; do
  CONTROLLER_DIR="operators/${op}/internal/controller"
  INSTRUMENTATION="${CONTROLLER_DIR}/instrumentation.go"

  hdr "Operator: ${op}"

  # Per-operator doc corpus.
  DOC_FILES=()
  if [[ -d "docs/reference/${op}" ]]; then
    for d in "docs/reference/${op}"/*.md; do
      [[ -f "${d}" ]] && DOC_FILES+=("${d}")
    done
  fi
  if [[ "${op}" == "keystone" && -f "architecture/docs/09-implementation/03-crd-implementation.md" ]]; then
    DOC_FILES+=("architecture/docs/09-implementation/03-crd-implementation.md")
  fi

  # Build the union of condition types declared in the map (right-hand side).
  # Either side may be empty — keystone mixes literals and constants, c5c3
  # uses constants exclusively.
  map_lits=$(grep -oE '"[A-Z][A-Za-z]+Ready"' "${INSTRUMENTATION}" | tr -d '"' | sort -u || true)
  map_consts=$(grep -oE 'conditionType[A-Z][A-Za-z]+Ready' "${INSTRUMENTATION}" | sort -u || true)

  if [[ -n "${map_lits}" ]]; then
    info "literal condition types in ${op} instrumentation map:"
    echo "${map_lits}" | sed 's/^/[INFO]   /'
  fi
  if [[ -n "${map_consts}" ]]; then
    info "const-ref condition types in ${op} instrumentation map:"
    echo "${map_consts}" | sed 's/^/[INFO]   /'
  fi

  # Resolved string set (literals plus resolved constants).
  resolved_set="${map_lits}"
  while IFS= read -r c; do
    [[ -z "${c}" ]] && continue
    v=$(resolve_const "${CONTROLLER_DIR}" "${c}")
    if [[ -n "${v}" ]]; then
      resolved_set="${resolved_set}"$'\n'"${v}"
    else
      info "could not resolve const ${c} to a literal — confirm by hand"
    fi
  done <<< "${map_consts}"
  resolved_set=$(echo "${resolved_set}" | sort -u)

  # -------------------------------------------------------------------------
  # K1 — every condition type set in reconcile_*.go is in the map (resolved).
  # Covers literal Type: "<X>Ready" and Type: conditionType<X>Ready call
  # sites. The bare aggregate "Ready" is exempt: it is the top-level roll-up
  # condition, not a sub-reconciler condition (the drift-guard test comment
  # documents that aggregated conditions carry no map entry).
  # -------------------------------------------------------------------------
  hdr "K1 (${op}): every condition type set in reconcile_*.go is registered"
  callsite_lits=$(grep -hrE 'Type:[[:space:]]+"[A-Z][A-Za-z]+Ready"' "${CONTROLLER_DIR}"/reconcile_*.go 2>/dev/null \
    | grep -oE '"[A-Z][A-Za-z]+Ready"' | tr -d '"' | sort -u || true)
  for t in ${callsite_lits}; do
    if echo "${resolved_set}" | grep -qx "${t}"; then
      pass "callsite literal ${t} is registered in instrumentation map"
    else
      fail "callsite literal ${t} is NOT in ${op} instrumentation map — Prometheus condition_type will resolve to UNKNOWN"
    fi
  done
  callsite_consts=$(grep -hrE 'Type:[[:space:]]+conditionType[A-Z][A-Za-z]+Ready' "${CONTROLLER_DIR}"/reconcile_*.go 2>/dev/null \
    | grep -oE 'conditionType[A-Z][A-Za-z]+Ready' | sort -u || true)
  for c in ${callsite_consts}; do
    v=$(resolve_const "${CONTROLLER_DIR}" "${c}")
    if [[ -z "${v}" ]]; then
      info "callsite const ${c} could not be resolved to a literal — confirm by hand"
    elif [[ "${v}" == "Ready" ]]; then
      info "callsite const ${c} resolves to the aggregate \"Ready\" — exempt from the map"
    elif echo "${resolved_set}" | grep -qx "${v}"; then
      pass "callsite const ${c} (\"${v}\") is registered in instrumentation map"
    else
      fail "callsite const ${c} (\"${v}\") is NOT in ${op} instrumentation map — Prometheus condition_type will resolve to UNKNOWN"
    fi
  done
  if [[ -z "${callsite_lits}" && -z "${callsite_consts}" ]]; then
    info "no condition-type call sites found under ${CONTROLLER_DIR}/reconcile_*.go"
  fi

  # -------------------------------------------------------------------------
  # K2 — every conditionType<X>Ready constant is defined exactly once and used
  # -------------------------------------------------------------------------
  hdr "K2 (${op}): every conditionType<X>Ready constant is defined once and used"
  all_const_refs=$(grep -hrEo 'conditionType[A-Z][A-Za-z]+Ready' "${CONTROLLER_DIR}"/*.go 2>/dev/null \
    | sort -u || true)
  for c in ${all_const_refs}; do
    defs=$(grep -hcE "^[[:space:]]*(const[[:space:]]+)?${c}[[:space:]]*=" "${CONTROLLER_DIR}"/*.go 2>/dev/null \
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

  # -------------------------------------------------------------------------
  # K3 — every reconcile_*.go has a paired _test.go
  # -------------------------------------------------------------------------
  hdr "K3 (${op}): every reconcile_*.go has a paired _test.go"
  for f in "${CONTROLLER_DIR}"/reconcile_*.go; do
    [[ -f "${f}" ]] || continue
    case "${f}" in
      *_test.go) continue ;;
    esac
    paired="${f%.go}_test.go"
    if [[ -f "${paired}" ]]; then
      pass "$(basename "${f}") has paired test $(basename "${paired}")"
    else
      fail "${op}: $(basename "${f}") has no paired test (expected $(basename "${paired}"))"
    fi
  done

  # -------------------------------------------------------------------------
  # K4 — every condition type in the map is mentioned in the doc corpus
  # -------------------------------------------------------------------------
  hdr "K4 (${op}): every condition type in the map is documented"
  if [[ ${#DOC_FILES[@]} -gt 0 ]]; then
    info "doc corpus: ${DOC_FILES[*]}"
    while IFS= read -r t; do
      [[ -z "${t}" ]] && continue
      if grep -q "${t}" "${DOC_FILES[@]}"; then
        pass "${t} documented"
      else
        fail "${op}: ${t} set in code but absent from docs/reference/${op}/"
      fi
    done <<< "${resolved_set}"
  else
    info "no reference docs for ${op} (docs/reference/${op}/ missing) — skipping K4"
  fi

  # -------------------------------------------------------------------------
  # K5 — every condition type referenced in the doc corpus is set in code
  # -------------------------------------------------------------------------
  hdr "K5 (${op}): every condition type referenced in docs is set in code"
  if [[ ${#DOC_FILES[@]} -gt 0 ]]; then
    doc_types=$(grep -hoE '\b[A-Z][A-Za-z]+Ready\b' "${DOC_FILES[@]}" 2>/dev/null | sort -u || true)
    for t in ${doc_types}; do
      if echo "${resolved_set}" | grep -qx "${t}"; then
        pass "${t} (doc) is set somewhere in ${op} code"
      else
        # Could be set via a constant we did not resolve, set by another
        # operator (cross-references are legitimate in prose), an ASCII-art
        # abbreviation of a real type (diagrams truncate long names), or stale.
        stem="${t%Ready}"
        if grep -rqE "\"${t}\"|conditionType${t}\b|\b${t}\b" "${CONTROLLER_DIR}"; then
          info "${t} (doc) appears in ${op} code but not via the instrumentation map — confirm by hand"
        elif echo "${resolved_set}" | grep -qE "^${stem}[A-Za-z]+Ready$"; then
          full=$(echo "${resolved_set}" | grep -E "^${stem}[A-Za-z]+Ready$" | head -1)
          info "${t} (doc) looks like a diagram abbreviation of ${full} — confirm by hand"
        elif grep -rqE "\"${t}\"" operators/*/internal/controller/ operators/*/api/ 2>/dev/null; then
          info "${t} (doc) is set by another operator — cross-reference, confirm by hand"
        else
          fail "${op}: ${t} (doc) is not set anywhere in code — stale documentation?"
        fi
      fi
    done
  else
    info "no reference docs for ${op} — skipping K5"
  fi

  # -------------------------------------------------------------------------
  # Inventory — per-reconciler condition types and tests
  # -------------------------------------------------------------------------
  hdr "Inventory (${op}) — sub-reconcilers and the condition types they set"
  for f in "${CONTROLLER_DIR}"/reconcile_*.go; do
    [[ -f "${f}" ]] || continue
    case "${f}" in
      *_test.go) continue ;;
    esac
    fname=$(basename "${f}")
    lits=$( { grep -oE 'Type:[[:space:]]+"[A-Z][A-Za-z]+Ready"' "${f}" 2>/dev/null || true; } \
      | grep -oE '"[A-Z][A-Za-z]+Ready"' | tr -d '"' | sort -u | tr '\n' ',' | sed 's/,$//' || true)
    cnst=$( { grep -oE 'Type:[[:space:]]+conditionType[A-Z][A-Za-z]+Ready' "${f}" 2>/dev/null || true; } \
      | grep -oE 'conditionType[A-Z][A-Za-z]+Ready' | sort -u | tr '\n' ',' | sed 's/,$//' || true)
    info "${fname}: literals=[${lits}] consts=[${cnst}]"
  done
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
