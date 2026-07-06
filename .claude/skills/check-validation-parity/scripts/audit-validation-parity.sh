#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# audit-validation-parity.sh — mechanical validation-parity checks for the
# forge repo. Verifies, per operator API surface:
#   V1  inventory: kubebuilder validation-marker families, XValidation/CEL
#       rules (with message), webhook field.* error-helper counts, and the
#       invalid-cr fixture count — printed for the hand cross-reference
#   V2  every *_webhook.go has a paired *_webhook_test.go
#   V3  every `contains($error, '…')` substring asserted by an invalid-cr
#       chainsaw suite still anchors to the current rule set: it appears in
#       the operator/common Go sources (webhook message or CEL message), in
#       a fixture of the same suite (rejected value echoed by the server),
#       is a server-rendered field path whose segments are real json tags,
#       or is a known apimachinery phrase
#   V4  every XValidation:rule= marker carries a message= (a CEL rule
#       without a message rejects CRs with an opaque error)
#   V5  every operator that registers a ValidatingWebhookConfiguration has
#       its invalid-cr e2e corpus wired to a chainsaw-test.yaml; a missing
#       corpus is reported as [INFO] GAP (graded in the skill report)
#
# The semantic CEL-rule ⇢ webhook-rule twin comparison and the sufficiency
# of the webhook unit tests are intentionally NOT scripted — see Procedure
# step 2 of the skill. Defers to `make verify-invalid-cr-fixtures` and the
# webhook unit tests as the authoritative gates. Exit code 1 on any [FAIL].

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "${REPO_ROOT}"

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

# Shared API surfaces consumed by the operator types (commonv1 et al.).
COMMON_SURFACES=()
for d in internal/common/types internal/common/validation; do
  [[ -d "${d}" ]] && COMMON_SURFACES+=("${d}")
done

hdr "Discovered API surfaces"
for op in "${OPERATORS[@]}"; do
  info "operator: ${op} (operators/${op}/api)"
done
for d in "${COMMON_SURFACES[@]}"; do
  info "shared:   ${d}"
done

# Non-test, non-generated Go sources of one operator plus the shared surfaces
# — the corpus every rule message lives in.
go_sources_for() {
  local op="$1"
  find "operators/${op}/api" "${COMMON_SURFACES[@]}" \
    -name '*.go' ! -name '*_test.go' ! -name 'zz_generated*' 2>/dev/null | sort
}

# ---------------------------------------------------------------------------
# V1 — inventory: markers, CEL rules, webhook error helpers, fixtures
# ---------------------------------------------------------------------------
hdr "V1: validation inventory (review aid — no pass/fail)"

inventory_surface() {
  local label="$1"
  shift
  local files=("$@")
  [[ ${#files[@]} -eq 0 ]] && return 0
  echo
  info "--- ${label} ---"
  local counts
  counts="$(grep -hoE '\+kubebuilder:validation:[A-Za-z]+' "${files[@]}" 2>/dev/null \
    | sort | uniq -c | sed 's/^ *//' || true)"
  if [[ -n "${counts}" ]]; then
    while IFS= read -r line; do
      info "marker  ${line}"
    done <<< "${counts}"
  else
    info "marker  (none)"
  fi
  # CEL rules with their message, one per line.
  grep -nE '\+kubebuilder:validation:XValidation:rule=' "${files[@]}" /dev/null 2>/dev/null \
    | while IFS= read -r line; do
        loc="$(printf '%s' "${line}" | cut -d: -f1,2)"
        msg="$(printf '%s' "${line}" | sed -nE 's/.*message="([^"]*)".*/\1/p')"
        info "cel     ${loc} — ${msg:-<NO MESSAGE>}"
      done || true
}

for op in "${OPERATORS[@]}"; do
  type_files=()
  while IFS= read -r f; do type_files+=("${f}"); done \
    < <(find "operators/${op}/api" -name '*.go' ! -name '*_test.go' ! -name 'zz_generated*' | sort)
  inventory_surface "operators/${op}/api" "${type_files[@]}"

  # Webhook error-helper counts per webhook file.
  while IFS= read -r wh; do
    helpers="$(grep -oE 'field\.(Invalid|Required|Forbidden|Duplicate|NotSupported|TooMany|TooLong)' "${wh}" \
      | sort | uniq -c | sed 's/^ *//' | tr '\n' ' ' || true)"
    info "webhook ${wh}: ${helpers:-no field.* helpers}"
  done < <(find "operators/${op}/api" -name '*_webhook.go' ! -name '*_test.go' | sort)

  # invalid-cr corpus size, if present.
  corpus="tests/e2e/${op}/invalid-cr"
  if [[ -d "${corpus}" ]]; then
    n="$(find "${corpus}" -maxdepth 1 -name '[0-9][0-9]-*.yaml' | wc -l | tr -d ' ')"
    info "corpus  ${corpus}: ${n} rejection fixture(s)"
  fi
done

for d in "${COMMON_SURFACES[@]}"; do
  common_files=()
  while IFS= read -r f; do common_files+=("${f}"); done \
    < <(find "${d}" -name '*.go' ! -name '*_test.go' ! -name 'zz_generated*' | sort)
  inventory_surface "${d}" "${common_files[@]}"
done

# ---------------------------------------------------------------------------
# V2 — every *_webhook.go has a paired *_webhook_test.go
# ---------------------------------------------------------------------------
hdr "V2: every webhook has a paired unit-test file"
for op in "${OPERATORS[@]}"; do
  found=0
  while IFS= read -r wh; do
    found=1
    test_file="${wh%.go}_test.go"
    if [[ -f "${test_file}" ]]; then
      pass "${op}: $(basename "${wh}") is paired with $(basename "${test_file}")"
    else
      fail "${op}: ${wh} has no paired ${test_file}"
    fi
  done < <(find "operators/${op}/api" -name '*_webhook.go' ! -name '*_test.go' | sort)
  [[ "${found}" -eq 0 ]] && info "${op}: no webhook files under operators/${op}/api"
done

# ---------------------------------------------------------------------------
# V3 — chainsaw $error substrings anchor to the current rule set
# ---------------------------------------------------------------------------
hdr "V3: invalid-cr chainsaw error assertions anchor to current rules"

# Phrases rendered by apimachinery / the API server itself, not this repo.
is_server_phrase() {
  case "$1" in
    'Duplicate value'|'Invalid value'|'Required value'|'Unsupported value'|'Forbidden'|'Too many'|'Too long')
      return 0 ;;
    *) return 1 ;;
  esac
}

for op in "${OPERATORS[@]}"; do
  suite="tests/e2e/${op}/invalid-cr/chainsaw-test.yaml"
  [[ -f "${suite}" ]] || continue
  sources=()
  while IFS= read -r f; do sources+=("${f}"); done < <(go_sources_for "${op}")
  total=0
  anchored=0
  while IFS= read -r sub; do
    [[ -n "${sub}" ]] || continue
    total=$((total + 1))
    if is_server_phrase "${sub}"; then
      anchored=$((anchored + 1))
      continue
    fi
    # (a) message / marker text in the Go sources
    if grep -qF -- "${sub}" "${sources[@]}" 2>/dev/null; then
      anchored=$((anchored + 1))
      continue
    fi
    # (b) rejected value echoed from a fixture of the same suite
    if grep -qF -- "${sub}" "tests/e2e/${op}/invalid-cr"/[0-9][0-9]-*.yaml 2>/dev/null; then
      anchored=$((anchored + 1))
      continue
    fi
    # (c) server-rendered field path: every dot-segment is a real json tag
    if printf '%s' "${sub}" | grep -qE '^[a-zA-Z][a-zA-Z0-9]*(\.[a-zA-Z][a-zA-Z0-9]*)+$'; then
      all_tags=1
      for seg in $(printf '%s' "${sub}" | tr '.' ' '); do
        if ! grep -qE "json:\"${seg}[,\"]" "${sources[@]}" 2>/dev/null; then
          all_tags=0
          break
        fi
      done
      if [[ "${all_tags}" -eq 1 ]]; then
        anchored=$((anchored + 1))
        continue
      fi
    fi
    fail "${op}: chainsaw asserts contains(\$error, '${sub}') but the substring anchors to no Go message/marker, fixture value, or json-tag path — stale assertion? (${suite})"
  done < <(grep -oE "contains\(\\\$error, '[^']+'\)" "${suite}" \
    | sed -E "s/.*'([^']+)'.*/\1/" | sort -u)
  if [[ "${total}" -gt 0 && "${anchored}" -eq "${total}" ]]; then
    pass "${op}: all ${total} asserted \$error substring(s) anchor to current rules"
  elif [[ "${total}" -eq 0 ]]; then
    info "${op}: ${suite} asserts no \$error substrings"
  fi
done

# ---------------------------------------------------------------------------
# V4 — every XValidation rule carries a message
# ---------------------------------------------------------------------------
hdr "V4: every XValidation:rule= marker has a message="
V4_TOTAL=0
V4_BAD=0
while IFS= read -r line; do
  V4_TOTAL=$((V4_TOTAL + 1))
  if ! printf '%s' "${line}" | grep -q 'message='; then
    V4_BAD=$((V4_BAD + 1))
    fail "XValidation rule without message= — $(printf '%s' "${line}" | cut -d: -f1,2)"
  fi
done < <(grep -rnE '\+kubebuilder:validation:XValidation:rule=' \
  operators/*/api internal/common --include='*.go' 2>/dev/null || true)
if [[ "${V4_BAD}" -eq 0 ]]; then
  pass "all ${V4_TOTAL} XValidation rule(s) carry a message="
fi

# ---------------------------------------------------------------------------
# V5 — validating-webhook operators have a wired invalid-cr corpus
# ---------------------------------------------------------------------------
hdr "V5: validating webhook ⇢ invalid-cr e2e corpus wiring"
for op in "${OPERATORS[@]}"; do
  manifest="operators/${op}/config/webhook/manifests.yaml"
  [[ -f "${manifest}" ]] || { info "${op}: no ${manifest}"; continue; }
  if ! grep -q 'ValidatingWebhookConfiguration' "${manifest}"; then
    info "${op}: no ValidatingWebhookConfiguration in ${manifest}"
    continue
  fi
  corpus="tests/e2e/${op}/invalid-cr"
  if [[ ! -d "${corpus}" ]]; then
    info "${op}: GAP — validating webhook registered but no ${corpus}/ rejection corpus (grade per the skill report)"
    continue
  fi
  if [[ ! -f "${corpus}/chainsaw-test.yaml" ]]; then
    fail "${op}: ${corpus}/ exists but has no chainsaw-test.yaml — fixtures are unreachable"
    continue
  fi
  refs="$(grep -cE 'file: *[0-9][0-9]-' "${corpus}/chainsaw-test.yaml" || true)"
  if [[ "${refs}" -eq 0 ]]; then
    fail "${op}: ${corpus}/chainsaw-test.yaml references no [0-9][0-9]-*.yaml fixture"
  else
    pass "${op}: ${corpus}/chainsaw-test.yaml wires ${refs} fixture reference(s)"
  fi
done

# ---------------------------------------------------------------------------
hdr "Summary"
if [[ ${FAIL_COUNT} -eq 0 ]]; then
  echo "[PASS] no validation-parity findings"
  exit 0
else
  echo "[FAIL] ${FAIL_COUNT} validation-parity finding(s)"
  exit 1
fi
