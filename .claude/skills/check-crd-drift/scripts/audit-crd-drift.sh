#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# audit-crd-drift.sh — mechanical CRD-drift checks for the forge repo.
# Verifies, per operator:
#   C1  every operators/<op>/api/ has a matching config/crd/bases/ with >=1 CRD
#   C2  every config/crd/bases/*.yaml is byte-equivalent (modulo leading `#`
#       comments) to helm/<op>-operator/crds/<same>
#   C3  every helm/<op>-operator/crds/*.yaml traces back to a base
#   C4  every *_types.go with +kubebuilder:object:root=true has a DeepCopy
#       block in zz_generated.deepcopy.go
#
# Defers regeneration to `make verify-crd-sync`. Pass --full to chain that
# gate after the inventory. Exit code 1 on any [FAIL].

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

# Discover operators that have an api/ directory.
OPERATORS=()
for d in operators/*/; do
  op="$(basename "${d}")"
  if [[ -d "operators/${op}/api" ]]; then
    OPERATORS+=("${op}")
  fi
done

if [[ ${#OPERATORS[@]} -eq 0 ]]; then
  fail "no operators with api/ found under operators/"
  exit 1
fi

hdr "Discovered operators"
for op in "${OPERATORS[@]}"; do
  info "operator: ${op}"
done

# ---------------------------------------------------------------------------
# C1 — every operator has config/crd/bases/*.yaml
# ---------------------------------------------------------------------------
hdr "C1: every operators/<op>/api/ has config/crd/bases/*.yaml"
for op in "${OPERATORS[@]}"; do
  base_dir="operators/${op}/config/crd/bases"
  if [[ ! -d "${base_dir}" ]]; then
    fail "${op}: missing ${base_dir} (run: make manifests OPERATOR=${op})"
    continue
  fi
  count=$(find "${base_dir}" -maxdepth 1 -name '*.yaml' -type f | wc -l | tr -d ' ')
  if [[ "${count}" -eq 0 ]]; then
    fail "${op}: ${base_dir} has no CRD YAML (run: make manifests OPERATOR=${op})"
  else
    pass "${op}: ${count} CRD base(s) under ${base_dir}"
  fi
done

# ---------------------------------------------------------------------------
# C2 — every base matches its helm copy modulo `^#` comment lines
# ---------------------------------------------------------------------------
hdr "C2: helm chart crds/ mirrors config/crd/bases/ (modulo comments)"
for op in "${OPERATORS[@]}"; do
  base_dir="operators/${op}/config/crd/bases"
  helm_dir="operators/${op}/helm/${op}-operator/crds"
  if [[ ! -d "${helm_dir}" ]]; then
    fail "${op}: missing ${helm_dir}"
    continue
  fi
  shopt -s nullglob
  for base in "${base_dir}"/*.yaml; do
    fname="$(basename "${base}")"
    helm="${helm_dir}/${fname}"
    if [[ ! -f "${helm}" ]]; then
      fail "${op}: ${fname} present in bases/ but missing under helm/${op}-operator/crds/"
      continue
    fi
    if diff -q \
        <(grep -v '^#' "${base}") \
        <(grep -v '^#' "${helm}") >/dev/null 2>&1; then
      pass "${op}: ${fname} mirrors base (modulo comments)"
    else
      fail "${op}: ${fname} drifts from base — run: make sync-crds OPERATOR=${op}"
    fi
  done
  shopt -u nullglob
done

# ---------------------------------------------------------------------------
# C3 — every helm crds/*.yaml has a base
# ---------------------------------------------------------------------------
hdr "C3: no orphan CRDs under helm/<op>-operator/crds/"
for op in "${OPERATORS[@]}"; do
  base_dir="operators/${op}/config/crd/bases"
  helm_dir="operators/${op}/helm/${op}-operator/crds"
  [[ -d "${helm_dir}" ]] || continue
  shopt -s nullglob
  for helm in "${helm_dir}"/*.yaml; do
    fname="$(basename "${helm}")"
    if [[ ! -f "${base_dir}/${fname}" ]]; then
      fail "${op}: orphan ${fname} in helm/${op}-operator/crds/ — no base under ${base_dir}/"
    else
      pass "${op}: helm ${fname} traces back to a base"
    fi
  done
  shopt -u nullglob
done

# ---------------------------------------------------------------------------
# C4 — every +kubebuilder:object:root=true type has a DeepCopy block
# ---------------------------------------------------------------------------
hdr "C4: every CRD kind has a DeepCopy block"
for op in "${OPERATORS[@]}"; do
  api_dir="operators/${op}/api/v1alpha1"
  deepcopy="${api_dir}/zz_generated.deepcopy.go"
  if [[ ! -f "${deepcopy}" ]]; then
    fail "${op}: missing ${deepcopy} — run: make generate-common"
    continue
  fi
  # Find every Go struct preceded by +kubebuilder:object:root=true.
  while IFS= read -r line; do
    # Line looks like: operators/keystone/api/v1alpha1/keystone_types.go:31:type Keystone struct {
    file="${line%%:*}"
    rest="${line#*:}"
    lineno="${rest%%:*}"
    # Extract the kind name from the next "type <Name> struct {" line at or
    # after the marker. Uses tail+sed for portability (BSD awk has no match() 3rd-arg).
    kind=$(tail -n "+${lineno}" "${file}" \
      | sed -nE 's/^type ([A-Z][A-Za-z0-9_]+) struct.*/\1/p' \
      | head -n 1)
    if [[ -z "${kind}" ]]; then
      info "${op}: marker at ${file}:${lineno} has no struct directly after it"
      continue
    fi
    if grep -q "^func (in \*${kind}) DeepCopyInto" "${deepcopy}"; then
      pass "${op}: ${kind} has DeepCopy block"
    else
      fail "${op}: ${kind} marked as CRD root but no DeepCopy in ${deepcopy} — run: make generate-common"
    fi
  done < <(grep -rn '+kubebuilder:object:root=true' "${api_dir}" || true)
done

# ---------------------------------------------------------------------------
# Inventory — per CRD, version names, printer columns, byte delta vs helm copy
# ---------------------------------------------------------------------------
hdr "Inventory"
for op in "${OPERATORS[@]}"; do
  base_dir="operators/${op}/config/crd/bases"
  helm_dir="operators/${op}/helm/${op}-operator/crds"
  [[ -d "${base_dir}" ]] || continue
  shopt -s nullglob
  for base in "${base_dir}"/*.yaml; do
    fname="$(basename "${base}")"
    helm="${helm_dir}/${fname}"
    versions=$(awk '/^  versions:/{flag=1;next} /^[a-zA-Z]/{flag=0} flag && /- name:/{print $3}' "${base}" | tr '\n' ',' | sed 's/,$//')
    printer_columns=$(grep -c '^    - jsonPath:' "${base}" || true)
    cel_rules=$(grep -c 'x-kubernetes-validations:' "${base}" || true)
    base_bytes=$(grep -cv '^#' "${base}" || true)
    helm_bytes=0
    [[ -f "${helm}" ]] && helm_bytes=$(grep -cv '^#' "${helm}" || true)
    delta=$((base_bytes - helm_bytes))
    info "${op}/${fname}: versions=[${versions}] printer-cols=${printer_columns} cel-rules=${cel_rules} non-comment-lines base=${base_bytes} helm=${helm_bytes} delta=${delta}"
  done
  shopt -u nullglob
done

# ---------------------------------------------------------------------------
# Optional: chain the authoritative gate
# ---------------------------------------------------------------------------
if [[ "${FULL}" -eq 1 ]]; then
  hdr "Authoritative gate — make verify-crd-sync"
  if make verify-crd-sync; then
    pass "make verify-crd-sync: clean"
  else
    fail "make verify-crd-sync: drift detected (see output above)"
  fi
fi

# ---------------------------------------------------------------------------
hdr "Summary"
if [[ ${FAIL_COUNT} -eq 0 ]]; then
  echo "[PASS] no CRD-drift findings"
  exit 0
else
  echo "[FAIL] ${FAIL_COUNT} CRD-drift finding(s)"
  exit 1
fi
