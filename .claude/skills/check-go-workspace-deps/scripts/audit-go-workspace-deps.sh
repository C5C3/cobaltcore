#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# audit-go-workspace-deps.sh — mechanical workspace consistency checks.
#   W1  every directory in go.work's use(…) block exists with go.mod
#   W2  every go.mod under operators/ or internal/ is in go.work
#   W3  go.work `go` directive matches every member's go.mod `go` directive
#   W4  every shared dep is pinned identically across modules that require it
#   W5  go.work.sum is present (CC-0001 REQ-009)
#
# Defers `go build ./...` and `go mod tidy && git diff` to the human. Exit 1 on [FAIL].

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "${REPO_ROOT}"

FAIL_COUNT=0
fail() { echo "[FAIL] $*"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
pass() { echo "[PASS] $*"; }
info() { echo "[INFO] $*"; }
hdr()  { echo; echo "=== $* ==="; }

# Shared deps the operators+common library are expected to align on.
SHARED_DEPS=(
  "sigs.k8s.io/controller-runtime"
  "k8s.io/api"
  "k8s.io/apimachinery"
  "k8s.io/client-go"
  "k8s.io/apiextensions-apiserver"
)

if [[ ! -f go.work ]]; then
  fail "missing go.work at repo root"
  exit 1
fi

# ---------------------------------------------------------------------------
# W1 — every directory in go.work use(…) exists with go.mod
# ---------------------------------------------------------------------------
hdr "W1: every go.work use(…) directory exists and contains go.mod"
use_dirs=$(awk '/^use \(/,/^\)/' go.work \
  | sed -nE 's|^[[:space:]]+\./?([^[:space:]]+).*|\1|p' \
  | sort -u)
if [[ -z "${use_dirs}" ]]; then
  fail "could not parse go.work use(…) block"
else
  while IFS= read -r d; do
    [[ -z "${d}" ]] && continue
    if [[ -f "${d}/go.mod" ]]; then
      pass "go.work member ./${d}/ has go.mod"
    else
      fail "go.work member ./${d}/ missing or has no go.mod"
    fi
  done <<< "${use_dirs}"
fi

# ---------------------------------------------------------------------------
# W2 — every go.mod under operators/ or internal/ is in go.work
# ---------------------------------------------------------------------------
hdr "W2: every operators/ + internal/ go.mod is listed in go.work"
fs_mods=$(find operators internal -name go.mod -exec dirname {} \; 2>/dev/null \
  | sort -u)
while IFS= read -r d; do
  [[ -z "${d}" ]] && continue
  if echo "${use_dirs}" | grep -qx "${d}"; then
    pass "fs module ${d} is in go.work"
  else
    fail "fs module ${d} has go.mod but is not listed in go.work — workspace ignores it"
  fi
done <<< "${fs_mods}"

# ---------------------------------------------------------------------------
# W3 — go directive consistency
# ---------------------------------------------------------------------------
hdr "W3: 'go' directive matches across go.work and every member go.mod"
workspace_go=$(grep -E '^go ' go.work | head -1 | awk '{print $2}')
info "go.work go directive: ${workspace_go}"
while IFS= read -r d; do
  [[ -z "${d}" ]] && continue
  [[ -f "${d}/go.mod" ]] || continue
  mod_go=$(grep -E '^go ' "${d}/go.mod" | head -1 | awk '{print $2}')
  if [[ "${mod_go}" == "${workspace_go}" ]]; then
    pass "${d}/go.mod go directive: ${mod_go}"
  else
    fail "${d}/go.mod go directive: ${mod_go} (workspace: ${workspace_go})"
  fi
done <<< "${use_dirs}"

# ---------------------------------------------------------------------------
# W4 — shared deps pinned identically per module that requires them
# ---------------------------------------------------------------------------
hdr "W4: shared deps pinned identically across modules"
# Collect (module, dep, version) — version is the field after the dep name
# in a require line, regardless of direct vs // indirect.
for dep in "${SHARED_DEPS[@]}"; do
  info "checking dep: ${dep}"
  versions=""
  while IFS= read -r d; do
    [[ -z "${d}" ]] && continue
    [[ -f "${d}/go.mod" ]] || continue
    v=$(grep -E "\s${dep}\s+v[0-9]" "${d}/go.mod" | head -1 | awk '{print $2}')
    if [[ -z "${v}" ]]; then
      # Some modules only depend transitively; skip rather than flag.
      continue
    fi
    info "  ${d} pins ${dep} ${v}"
    versions="${versions}${v}"$'\n'
  done <<< "${use_dirs}"
  unique=$(echo "${versions}" | sort -u | grep -c . || true)
  if [[ "${unique}" -le 1 ]]; then
    pass "${dep}: consistent across modules"
  else
    fail "${dep}: divergent versions across modules — pick one and run 'go get ${dep}@<v>' in each module"
  fi
done

# ---------------------------------------------------------------------------
# W5 — go.work.sum present
# ---------------------------------------------------------------------------
hdr "W5: go.work.sum tracked (CC-0001 REQ-009)"
if [[ -f go.work.sum ]]; then
  pass "go.work.sum present"
  if git ls-files --error-unmatch go.work.sum >/dev/null 2>&1; then
    pass "go.work.sum tracked by git"
  else
    fail "go.work.sum exists but is not tracked by git (REQ-009 violation)"
  fi
else
  fail "go.work.sum missing (CC-0001 REQ-009 requires it tracked)"
fi

# ---------------------------------------------------------------------------
# Inventory — per-dep version table
# ---------------------------------------------------------------------------
hdr "Inventory — shared-dep version table"
printf '[INFO] %-45s' "dependency"
while IFS= read -r d; do
  [[ -z "${d}" ]] && continue
  printf ' %-25s' "${d}"
done <<< "${use_dirs}"
printf '\n'
for dep in "${SHARED_DEPS[@]}"; do
  printf '[INFO] %-45s' "${dep}"
  while IFS= read -r d; do
    [[ -z "${d}" ]] && continue
    [[ -f "${d}/go.mod" ]] || { printf ' %-25s' '(no go.mod)'; continue; }
    v=$(grep -E "\s${dep}\s+v[0-9]" "${d}/go.mod" | head -1 | awk '{print $2}')
    [[ -z "${v}" ]] && v='—'
    printf ' %-25s' "${v}"
  done <<< "${use_dirs}"
  printf '\n'
done

# ---------------------------------------------------------------------------
hdr "Summary"
if [[ ${FAIL_COUNT} -eq 0 ]]; then
  echo "[PASS] no workspace-dep findings"
  exit 0
else
  echo "[FAIL] ${FAIL_COUNT} workspace-dep finding(s)"
  exit 1
fi
