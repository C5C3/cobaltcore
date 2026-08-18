#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# audit-doc-drift.sh — mechanical doc-drift checks for the CobaltCore repo.
# Verifies that docs/ (and README.md) still describe the operator code
# and infrastructure stack accurately:
#   D1  Makefile OPERATORS default matches the operator modules under operators/
#       (directories with a go.mod; operators/shared/ is a helm library, not
#       an operator)
#   D2  every ### reconcile… heading under docs/reference/ names a real
#       Go function in an operators/*/internal/controller/ package
#   D3  every deploy/<component>/ named in docs/ or README.md still exists
#
# Condition-type doc coverage is audited by check-condition-coverage (K4/K5);
# internal feature/requirement-ID cross-referencing is retired repo-wide
# (scripts/check-no-feature-ids.sh forbids the IDs).
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

# ---------------------------------------------------------------------------
# D1 — Makefile OPERATORS default matches the operator modules under operators/
# ---------------------------------------------------------------------------
hdr "D1: Makefile OPERATORS default matches operator modules under operators/"
makefile_ops=$(grep -E '^OPERATORS \?=' Makefile | head -1 | sed -E 's/^OPERATORS \?= *//' | tr ' ' '\n' | sort)
fs_ops=$(for d in operators/*/; do if [[ -f "${d}go.mod" ]]; then basename "${d}"; fi; done | sort)
if diff <(echo "${makefile_ops}") <(echo "${fs_ops}") >/dev/null 2>&1; then
  pass "Makefile OPERATORS default matches operator modules (${fs_ops//$'\n'/, })"
else
  fail "OPERATORS drift — Makefile: $(echo "${makefile_ops}" | tr '\n' ',' | sed 's/,$//') vs filesystem: $(echo "${fs_ops}" | tr '\n' ',' | sed 's/,$//')"
fi

# ---------------------------------------------------------------------------
# D2 — every ### reconcile… heading under docs/reference/ names a real function
# ---------------------------------------------------------------------------
hdr "D2: every ### reconcile… heading under docs/reference/ names a real Go function"
heading_count=0
while IFS=: read -r file heading; do
  fn=$(echo "${heading}" | sed -nE 's/^### (reconcile[A-Za-z]+).*/\1/p')
  [[ -z "${fn}" ]] && continue
  heading_count=$((heading_count + 1))
  if grep -rqE "func .*\) ${fn}\(" operators/*/internal/controller/; then
    pass "${file}: ### ${fn} resolves to a Go function"
  else
    fail "${file}: ### ${fn} has no matching func under operators/*/internal/controller/ — renamed/removed?"
  fi
done < <(grep -rE --include='*.md' '^### reconcile[A-Z]' docs/reference/ 2>/dev/null)
info "reconcile headings checked: ${heading_count}"

# ---------------------------------------------------------------------------
# D3 — every deploy/<component> named in docs/ or README.md exists on disk
# ---------------------------------------------------------------------------
hdr "D3: deploy/<component>/ references in docs/ and README.md exist"
if [[ -d docs && -d deploy ]]; then
  refs=$(grep -rhoE --include='*.md' --exclude-dir=.vitepress 'deploy/[a-z][a-z0-9-]+/' docs/ README.md 2>/dev/null | sort -u || true)
  for ref in ${refs}; do
    # ref is like "deploy/openbao/"; strip trailing slash for the dir test
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
if [[ -d docs ]]; then
  # Match common patterns like "11 sub-conditions", "8-step", "three states".
  # Markdown sources only — .vitepress/ holds build output and cache. The
  # leading class keeps product names like "c5c3-operator" out of the matches.
  grep -rEn --include='*.md' --exclude-dir=.vitepress \
    '(^|[^a-z0-9-])[0-9]+[- ](sub-condition|step|reconciler|operator|condition|phase|version)' \
    docs/ 2>/dev/null | head -20 || true
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
