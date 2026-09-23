#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# audit-go-workspace-deps.sh — mechanical workspace consistency checks.
#   W1  every directory in go.work's use(…) block exists with go.mod
#   W2  every go.mod under operators/ or internal/ is in go.work
#   W3  go.work `go` directive matches every member's go.mod `go` directive
#   W3b a `toolchain` directive, where any module or go.work declares one, is
#       identical everywhere
#   W4  every shared dep is pinned identically across modules that require it:
#       the fixed SHARED_DEPS list below (direct or // indirect), plus every
#       module that two or more members require DIRECTLY
#   W5  go.work.sum is present and tracked by git
#
# Defers `make verify-go-tidy` and `go build ./...` to the human. Exit 1 on [FAIL].

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "${REPO_ROOT}"

FAIL_COUNT=0
fail() { echo "[FAIL] $*"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
pass() { echo "[PASS] $*"; }
info() { echo "[INFO] $*"; }
hdr()  { echo; echo "=== $* ==="; }

# Shared deps the operators+common library are expected to align on even when a
# module only requires them // indirect: the k8s.io family moves as one
# monorepo release, and an indirect pin that lags (k8s.io/apiextensions-apiserver
# flows from internal/common's version) is what skews the merged workspace.
# Direct requirements shared by two or more modules are compared on top of this
# list without being named here.
SHARED_DEPS=(
  "sigs.k8s.io/controller-runtime"
  "sigs.k8s.io/multicluster-runtime"
  "k8s.io/api"
  "k8s.io/apimachinery"
  "k8s.io/client-go"
  "k8s.io/apiextensions-apiserver"
  "github.com/dc-tec/openbao-operator"
)

# dep_version <go.mod> <module path> — the version a go.mod requires for the
# module (direct or // indirect), from a require block or a one-line require.
# Exact field comparison, so k8s.io/api does not match k8s.io/apimachinery and
# the dots in a module path are not regex wildcards.
dep_version() {
  awk -v dep="$2" '
    /^require[[:space:]]*\(/ { inblk = 1; next }
    inblk && /^\)/           { inblk = 0; next }
    inblk && $1 == dep       { print $2; exit }
    /^require[[:space:]]/ && $2 == dep { print $3; exit }
  ' "$1"
}

# direct_deps <go.mod> — "<module> <version>" for every requirement that is not
# marked // indirect.
direct_deps() {
  awk '
    /^require[[:space:]]*\(/ { inblk = 1; next }
    inblk && /^\)/           { inblk = 0; next }
    /\/\/ indirect/          { next }
    inblk && $1 !~ /^\/\//   { print $1, $2 }
    /^require[[:space:]]/ && $2 != "(" { print $2, $3 }
  ' "$1"
}

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
  if grep -qx "${d}" <<<"${use_dirs}"; then
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
# W3b — toolchain directive consistency (only when one is declared anywhere)
# ---------------------------------------------------------------------------
hdr "W3b: 'toolchain' directive matches wherever one is declared"
toolchains=""
for f in go.work $(while IFS= read -r d; do [[ -n "${d}" ]] && echo "${d}/go.mod"; done <<< "${use_dirs}"); do
  [[ -f "${f}" ]] || continue
  t=$(grep -E '^toolchain[[:space:]]' "${f}" | head -1 | awk '{print $2}' || true)
  if [[ -n "${t}" ]]; then
    info "${f} toolchain ${t}"
    toolchains="${toolchains}${t}"$'\n'
  fi
done
if [[ -z "${toolchains}" ]]; then
  pass "no toolchain directive declared — the go directive alone selects the toolchain"
elif [[ "$(printf '%s' "${toolchains}" | sort -u | grep -c .)" -eq 1 ]]; then
  pass "toolchain directive identical wherever declared"
else
  fail "toolchain directives disagree — align them (or drop them and let the go directive decide)"
fi

# ---------------------------------------------------------------------------
# W4 — shared deps pinned identically per module that requires them
# ---------------------------------------------------------------------------
hdr "W4: shared deps pinned identically across modules"
# (a) The fixed SHARED_DEPS list, direct or // indirect.
for dep in "${SHARED_DEPS[@]}"; do
  versions=""
  holders=""
  while IFS= read -r d; do
    [[ -z "${d}" ]] && continue
    [[ -f "${d}/go.mod" ]] || continue
    v=$(dep_version "${d}/go.mod" "${dep}")
    # A module that does not require the dep at all is skipped, not flagged.
    [[ -z "${v}" ]] && continue
    versions="${versions}${v}"$'\n'
    holders="${holders} ${d}=${v}"
  done <<< "${use_dirs}"
  unique=$(printf '%s' "${versions}" | sort -u | grep -c . || true)
  if [[ "${unique}" -le 1 ]]; then
    pass "${dep}: consistent across modules ($(printf '%s' "${versions}" | head -1))"
  else
    fail "${dep}: divergent versions —${holders} — pick one and run 'go get ${dep}@<v>' in each module, then 'go mod tidy'"
  fi
done

# (b) Every module that two or more members require directly. Indirect pins
# outside SHARED_DEPS are left alone: go mod tidy computes them per module graph
# and they legitimately differ.
direct_tmp=$(mktemp)
trap 'rm -f "${direct_tmp}"' EXIT
while IFS= read -r d; do
  [[ -z "${d}" ]] && continue
  [[ -f "${d}/go.mod" ]] || continue
  direct_deps "${d}/go.mod" | awk -v m="${d}" '{ print $1, $2, m }' >> "${direct_tmp}"
done <<< "${use_dirs}"
shared_direct=$(awk '{ n[$1]++ } END { for (d in n) if (n[d] > 1) print d }' "${direct_tmp}" | sort)
checked=0
while IFS= read -r dep; do
  [[ -z "${dep}" ]] && continue
  case " ${SHARED_DEPS[*]} " in *" ${dep} "*) continue ;; esac
  checked=$((checked + 1))
  distinct=$(awk -v d="${dep}" '$1 == d { print $2 }' "${direct_tmp}" | sort -u | grep -c . || true)
  if [[ "${distinct}" -gt 1 ]]; then
    fail "${dep}: required directly at divergent versions — $(awk -v d="${dep}" '$1 == d { printf "%s=%s ", $3, $2 }' "${direct_tmp}")"
  fi
done <<< "${shared_direct}"
pass "checked ${checked} further direct requirement(s) shared by two or more modules"

# ---------------------------------------------------------------------------
# W5 — go.work.sum present
# ---------------------------------------------------------------------------
# go.work.sum is tracked on purpose: CI and every laptop then verify the same
# checksums for the modules only the workspace (not any single go.mod) pulls in.
hdr "W5: go.work.sum present and tracked"
if [[ -f go.work.sum ]]; then
  pass "go.work.sum present"
  if git ls-files --error-unmatch go.work.sum >/dev/null 2>&1; then
    pass "go.work.sum tracked by git"
  else
    fail "go.work.sum exists but is not tracked by git — do not add it to .gitignore"
  fi
else
  fail "go.work.sum missing — run a workspace build (go build ./...) and commit it"
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
    v=$(dep_version "${d}/go.mod" "${dep}")
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
