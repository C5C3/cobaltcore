#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# audit-spdx-reuse.sh — mechanical SPDX / REUSE coverage checks.
#   S1  *.go under operators/, internal/, tests/ has both SPDX headers
#       (zz_generated and "Code generated … DO NOT EDIT." files exempted)
#   S2  *.sh under hack/, scripts/, tests/{scripts,unit,lib} has both headers
#   S3  hand-authored *.yaml / *.toml under deploy/, operators/<op>/config/,
#       releases/ has both headers (CRDs that are pure controller-gen output
#       are exempted)
#   S4  every SPDX-License-Identifier value has a matching LICENSES/<id>.txt
#   S5  every LICENSES/<id>.txt is referenced by at least one file
#   S6  architecture/REUSE.toml parses as valid TOML
#
# Defers full compliance to `reuse lint`. Exit code 1 on [FAIL].

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "${REPO_ROOT}"

FAIL_COUNT=0
fail() { echo "[FAIL] $*"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
pass() { echo "[PASS] $*"; }
info() { echo "[INFO] $*"; }
hdr()  { echo; echo "=== $* ==="; }

# Header check: returns 0 if both SPDX headers exist in the first 20 lines.
# Reads the first 20 lines into a variable so each grep sees the full slice
# (a single pipe would let the first grep consume the stream).
has_spdx_pair() {
  local f="$1"
  local head_buf
  head_buf=$(head -20 "${f}" 2>/dev/null) || return 1
  echo "${head_buf}" | grep -q 'SPDX-FileCopyrightText' || return 1
  echo "${head_buf}" | grep -q 'SPDX-License-Identifier' || return 1
  return 0
}

# Generated-file exemption.
is_generated_go() {
  local f="$1"
  case "$(basename "${f}")" in
    zz_generated.*) return 0 ;;
  esac
  head -10 "${f}" 2>/dev/null | grep -q 'Code generated .* DO NOT EDIT'
}

# Heuristic: a YAML is "pure controller-gen output" if it lives under
# config/crd/ AND starts with `---` followed by `apiVersion: apiextensions.k8s.io`.
is_generated_yaml() {
  local f="$1"
  case "${f}" in
    */config/crd/*) ;;
    *) return 1 ;;
  esac
  head -3 "${f}" 2>/dev/null | grep -qE 'apiextensions\.k8s\.io|controller-gen\.kubebuilder\.io'
}

# ---------------------------------------------------------------------------
# S1 — *.go under operators/, internal/, tests/
# ---------------------------------------------------------------------------
hdr "S1: *.go under operators/, internal/, tests/ has SPDX headers"
go_total=0
go_fail=0
while IFS= read -r f; do
  [[ -z "${f}" ]] && continue
  go_total=$((go_total + 1))
  if is_generated_go "${f}"; then
    continue
  fi
  if ! has_spdx_pair "${f}"; then
    fail "S1: ${f} missing SPDX header(s)"
    go_fail=$((go_fail + 1))
  fi
done < <(find operators internal tests -type f -name '*.go' 2>/dev/null)
info "S1 totals: scanned=${go_total} fail=${go_fail}"
[[ "${go_fail}" -eq 0 ]] && pass "S1 clean (${go_total} files scanned)"

# ---------------------------------------------------------------------------
# S2 — *.sh
# ---------------------------------------------------------------------------
hdr "S2: *.sh under hack/, scripts/, tests/{scripts,unit,lib} has SPDX headers"
sh_total=0
sh_fail=0
while IFS= read -r f; do
  [[ -z "${f}" ]] && continue
  sh_total=$((sh_total + 1))
  if ! has_spdx_pair "${f}"; then
    fail "S2: ${f} missing SPDX header(s)"
    sh_fail=$((sh_fail + 1))
  fi
done < <(find hack scripts tests/scripts tests/unit tests/lib -type f -name '*.sh' 2>/dev/null)
info "S2 totals: scanned=${sh_total} fail=${sh_fail}"
[[ "${sh_fail}" -eq 0 ]] && pass "S2 clean (${sh_total} files scanned)"

# ---------------------------------------------------------------------------
# S3 — hand-authored YAML / TOML
# ---------------------------------------------------------------------------
hdr "S3: hand-authored YAML/TOML under deploy/, operators/<op>/config/, releases/ has SPDX headers"
yaml_total=0
yaml_fail=0
while IFS= read -r f; do
  [[ -z "${f}" ]] && continue
  yaml_total=$((yaml_total + 1))
  if is_generated_yaml "${f}"; then
    continue
  fi
  if ! has_spdx_pair "${f}"; then
    fail "S3: ${f} missing SPDX header(s)"
    yaml_fail=$((yaml_fail + 1))
  fi
done < <(find deploy operators/*/config releases -type f \( -name '*.yaml' -o -name '*.yml' -o -name '*.toml' \) 2>/dev/null)
info "S3 totals: scanned=${yaml_total} fail=${yaml_fail}"
[[ "${yaml_fail}" -eq 0 ]] && pass "S3 clean (${yaml_total} files scanned)"

# ---------------------------------------------------------------------------
# S4 / S5 — licence inventory
# ---------------------------------------------------------------------------
hdr "S4: every SPDX-License-Identifier value has a matching LICENSES/<id>.txt"
ids=$(grep -rhE 'SPDX-License-Identifier:[[:space:]]*[A-Za-z0-9.+-]+' \
  --include='*.go' --include='*.sh' --include='*.yaml' --include='*.yml' \
  --include='*.toml' --include='*.py' --include='Makefile' --include='Dockerfile' \
  . 2>/dev/null \
  | sed -nE 's/.*SPDX-License-Identifier:[[:space:]]*([A-Za-z0-9.+-]+).*/\1/p' \
  | sort -u || true)
for id in ${ids}; do
  if [[ -f "LICENSES/${id}.txt" ]]; then
    pass "licence ${id} has LICENSES/${id}.txt"
  else
    fail "licence ${id} referenced in header(s) but no LICENSES/${id}.txt — run: reuse download ${id}"
  fi
done

hdr "S5: every LICENSES/<id>.txt is referenced by at least one file"
if [[ -d LICENSES ]]; then
  for lf in LICENSES/*.txt; do
    [[ -f "${lf}" ]] || continue
    id=$(basename "${lf}" .txt)
    if echo "${ids}" | grep -qx "${id}"; then
      pass "${id} referenced"
    else
      info "${id} in LICENSES/ but no file references it — unused inventory"
    fi
  done
fi

# ---------------------------------------------------------------------------
# S6 — REUSE.toml parses
# ---------------------------------------------------------------------------
hdr "S6: architecture/REUSE.toml parses as valid TOML"
if [[ -f architecture/REUSE.toml ]]; then
  if command -v python3 >/dev/null 2>&1; then
    if python3 -c 'import tomllib,sys; tomllib.load(open("architecture/REUSE.toml","rb"))' 2>/dev/null; then
      pass "architecture/REUSE.toml parses"
    else
      fail "architecture/REUSE.toml does not parse as TOML"
    fi
  else
    info "python3 not on PATH — S6 skipped"
  fi
else
  info "no architecture/REUSE.toml — S6 skipped"
fi

# ---------------------------------------------------------------------------
# Inventory — counts by extension
# ---------------------------------------------------------------------------
hdr "Inventory — header coverage breakdown by extension"
info "Go files scanned=${go_total} missing-header=${go_fail}"
info "Shell scripts scanned=${sh_total} missing-header=${sh_fail}"
info "YAML/TOML scanned=${yaml_total} missing-header=${yaml_fail}"

# ---------------------------------------------------------------------------
hdr "Summary"
if [[ ${FAIL_COUNT} -eq 0 ]]; then
  echo "[PASS] no SPDX/REUSE findings"
  exit 0
else
  echo "[FAIL] ${FAIL_COUNT} SPDX/REUSE finding(s)"
  exit 1
fi
