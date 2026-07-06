#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# audit-doc-drift.sh — mechanical doc-drift checks for the forge repo.
# Verifies that architecture/docs/ + docs/ still describe the operator code
# and infrastructure stack accurately:
#   D1  Makefile OPERATORS default matches operators/*/
#   D2  count of reconcile_*.go matches doc-side sub-reconciler section count
#   D3  every doc-side reconcileX() heading names a real function
#   D4  every condition type literal set in reconcile_*.go is documented
#   D5  retired (internal feature/requirement IDs removed repo-wide)
#   D6  every deploy/<component>/ named in architecture/docs/09-implementation/
#       still exists
#
# Defers prose-meaning checks to a human reviewer. Exit code 1 on any [FAIL].

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "${REPO_ROOT}"

FAIL_COUNT=0
fail() { echo "[FAIL] $*"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
pass() { echo "[PASS] $*"; }
info() { echo "[INFO] $*"; }
hdr()  { echo; echo "=== $* ==="; }

RECONCILER_DOC="architecture/docs/09-implementation/04-keystone-reconciler.md"
CRD_DOC="architecture/docs/09-implementation/03-crd-implementation.md"
CONTROLLER_DIR="operators/keystone/internal/controller"

# ---------------------------------------------------------------------------
# D1 — Makefile OPERATORS default matches operators/*/
# ---------------------------------------------------------------------------
hdr "D1: Makefile OPERATORS default matches operators/*/"
makefile_ops=$(grep -E '^OPERATORS \?=' Makefile | head -1 | sed -E 's/^OPERATORS \?= *//' | tr ' ' '\n' | sort)
fs_ops=$(for d in operators/*/; do basename "${d}"; done | sort)
if diff <(echo "${makefile_ops}") <(echo "${fs_ops}") >/dev/null 2>&1; then
  pass "Makefile OPERATORS default matches operators/* (${fs_ops//$'\n'/, })"
else
  fail "OPERATORS drift — Makefile: $(echo "${makefile_ops}" | tr '\n' ',' | sed 's/,$//') vs filesystem: $(echo "${fs_ops}" | tr '\n' ',' | sed 's/,$//')"
fi

# ---------------------------------------------------------------------------
# D2 — count of reconcile_*.go matches doc heading count
# ---------------------------------------------------------------------------
hdr "D2: reconcile_*.go file count vs ### reconcileX() heading count in ${RECONCILER_DOC}"
recon_files=$(find "${CONTROLLER_DIR}" -maxdepth 1 -name 'reconcile_*.go' -not -name '*_test.go' | wc -l | tr -d ' ')
if [[ -f "${RECONCILER_DOC}" ]]; then
  doc_headings=$(grep -cE '^### reconcile[A-Z][A-Za-z]*\(\)' "${RECONCILER_DOC}" || true)
  info "reconcile_*.go files: ${recon_files}; ### reconcileX() headings: ${doc_headings}"
  if [[ "${recon_files}" -ne "${doc_headings}" ]]; then
    fail "sub-reconciler count drift: ${recon_files} files vs ${doc_headings} doc headings (delta=$((recon_files - doc_headings)))"
  else
    pass "sub-reconciler count consistent"
  fi
else
  fail "missing reconciler doc: ${RECONCILER_DOC}"
fi

# ---------------------------------------------------------------------------
# D3 — every ### reconcileX() heading names a real Go function
# ---------------------------------------------------------------------------
hdr "D3: every ### reconcileX() heading names a real Go function"
if [[ -f "${RECONCILER_DOC}" ]]; then
  while IFS= read -r heading; do
    fn=$(echo "${heading}" | sed -nE 's/^### (reconcile[A-Z][A-Za-z]*)\(\).*/\1/p')
    [[ -z "${fn}" ]] && continue
    if grep -rqE "^func .*\) ${fn}\(" "${CONTROLLER_DIR}"; then
      pass "doc heading ### ${fn}() resolves to a Go function"
    else
      fail "doc heading ### ${fn}() has no matching func in ${CONTROLLER_DIR} — renamed/removed?"
    fi
  done < <(grep -E '^### reconcile[A-Z]' "${RECONCILER_DOC}")
fi

# ---------------------------------------------------------------------------
# D4 — every condition type literal set in reconcile_*.go is documented
# ---------------------------------------------------------------------------
hdr "D4: condition types set in code are documented in ${CRD_DOC}"
# Extract condition type literals from instrumentation.go map (the canonical set).
INSTRUMENTATION="${CONTROLLER_DIR}/instrumentation.go"
if [[ -f "${INSTRUMENTATION}" && -f "${CRD_DOC}" ]]; then
  # Map values in the form `"<Name>Ready"` or `conditionType<X>Ready` (constant).
  # We take the literal-quoted ones directly; the const ones require resolving in code.
  literal_types=$(grep -oE '"[A-Z][A-Za-z]+Ready"' "${INSTRUMENTATION}" | tr -d '"' | sort -u)
  for t in ${literal_types}; do
    if grep -q "${t}" "${CRD_DOC}"; then
      pass "condition type ${t} documented in ${CRD_DOC}"
    else
      fail "condition type ${t} set in code but not documented in ${CRD_DOC}"
    fi
  done
  # Also list constant-referenced types for the human to cross-check.
  const_refs=$(grep -oE 'conditionType[A-Z][A-Za-z]+Ready' "${INSTRUMENTATION}" | sort -u)
  for c in ${const_refs}; do
    info "condition type constant ${c} — confirm doc coverage by hand"
  done
else
  info "skipping D4: missing ${INSTRUMENTATION} or ${CRD_DOC}"
fi

# ---------------------------------------------------------------------------
# D5 — RETIRED. This check cross-referenced internal CC-NNNN feature-ID markers
# in the code against the architecture docs. Those internal IDs have been
# removed from the repository (a repo-wide gate, scripts/check-no-feature-ids.sh,
# now forbids them), so there are no code anchors left to cross-reference.
# ---------------------------------------------------------------------------
hdr "D5: code-anchor cross-reference (retired)"
info "D5 retired: internal feature/requirement IDs were removed repo-wide, so there are no code markers to cross-reference"
pass "D5 skipped (retired)"

# ---------------------------------------------------------------------------
# D6 — every deploy/<component> named in architecture/docs/09-implementation/
#       exists on disk
# ---------------------------------------------------------------------------
hdr "D6: deploy/<component>/ references in architecture/docs/09-implementation/ exist"
if [[ -d architecture/docs/09-implementation && -d deploy ]]; then
  refs=$(grep -rhoE 'deploy/[a-z][a-z0-9-]+/' architecture/docs/09-implementation/ 2>/dev/null \
    | sort -u || true)
  for ref in ${refs}; do
    # ref is like "deploy/cert-manager/"; strip trailing slash for stat
    path="${ref%/}"
    if [[ -d "${path}" ]]; then
      pass "doc reference ${ref} exists on disk"
    else
      fail "doc reference ${ref} has no matching directory under deploy/"
    fi
  done
fi

# ---------------------------------------------------------------------------
# Inventory — spelled-out numeric claims (review aid)
# ---------------------------------------------------------------------------
hdr "Inventory — numeric claims in docs (review aid)"
if [[ -d architecture/docs ]]; then
  # Match common patterns like "11 sub-conditions", "8-step", "three states".
  grep -rEn '[0-9]+[- ](sub-condition|step|reconciler|operator|condition|phase|version)' \
    architecture/docs/ 2>/dev/null | head -20 || true
fi

hdr "Inventory — FluxCD HelmReleases declared under deploy/"
if [[ -d deploy/flux-system/releases ]]; then
  find deploy/flux-system/releases -name '*.yaml' -exec basename {} .yaml \; 2>/dev/null | sort | sed 's/^/[INFO] flux release: /'
fi

# ---------------------------------------------------------------------------
hdr "Summary"
if [[ ${FAIL_COUNT} -eq 0 ]]; then
  echo "[PASS] no documentation-drift findings"
  exit 0
else
  echo "[FAIL] ${FAIL_COUNT} documentation-drift finding(s)"
  exit 1
fi
