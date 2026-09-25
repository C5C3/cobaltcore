#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# audit-validation-parity.sh — mechanical validation-parity checks for the
# CobaltCore repo. Verifies, per operator API surface:
#   V1  inventory: kubebuilder validation-marker families, XValidation/CEL
#       rules (with message), webhook field.* error-helper counts, and the
#       fixture count of every rejection corpus (tests/e2e/<op>/invalid-cr/
#       and tests/e2e/<op>/invalid-*-cr/) — printed for the hand cross-reference
#   V2  every *_webhook.go has a paired *_webhook_test.go
#   V3  every `contains($error, '…')` substring a rejection corpus asserts
#       still anchors to the current rule set. Anchor categories, first hit
#       wins (anchor-error-substrings.awk beside this script):
#         server-phrase  a field.ErrorType string apimachinery renders
#                        ("Not found", "Required value", …) or the API
#                        server's "admission webhook" denial prefix
#         schema-phrase  a go-openapi / apiextensions phrase ("should be at
#                        least 1 chars long", "should match", …) whose
#                        boundary a +kubebuilder marker or the suite kind's
#                        generated CRD carries; a boundary nothing carries is
#                        reported as stale
#         go             inside a Go string literal (concatenations joined)
#                        or a +kubebuilder marker line of the operator's
#                        corpus: its own api/, every operators/<Y>/api it
#                        imports, and all non-test Go under internal/common/
#         field-path     a server-rendered path ("cache.servers[0]",
#                        "resources.requests.memory"): each segment, index
#                        stripped, is a json tag, a property of the kind's
#                        CRD (embedded Kubernetes types), or a fixture key
#         fixture        a value echoed from a numbered fixture of the suite
#         template       consistent with a fmt verb template of the corpus
#                        ("name must be at most %d characters: …"): the
#                        literal parts match verbatim, every %d-style value
#                        is numeric, every %s/%v/%q value occurs verbatim in
#                        the corpus or a fixture, and at least 8 literal
#                        non-space characters over two words take part
#   V4  every XValidation:rule= marker carries a message= (a CEL rule
#       without a message rejects CRs with an opaque error)
#   V5  every operator that registers a ValidatingWebhookConfiguration has
#       every rejection corpus wired to a chainsaw-test.yaml, and every
#       validated resource is applied by some corpus fixture; a missing
#       corpus is reported as [INFO] GAP (graded in the skill report)
#
# The semantic CEL-rule ⇢ webhook-rule twin comparison and the sufficiency
# of the webhook unit tests are intentionally NOT scripted — see Procedure
# step 2 of the skill. Defers to `make verify-invalid-cr-fixtures` and the
# webhook unit tests as the authoritative gates. Read-only: the extracted
# Go/CRD records go to a mktemp directory that is removed on exit.
# Exit code 1 on any [FAIL].

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ANCHOR_AWK="${SCRIPT_DIR}/anchor-error-substrings.awk"
cd "${REPO_ROOT}"

FAIL_COUNT=0
fail() { echo "[FAIL] $*"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
pass() { echo "[PASS] $*"; }
info() { echo "[INFO] $*"; }
hdr()  { echo; echo "=== $* ==="; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/validation-parity.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT

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

# corpus_dirs_for — every rejection corpus of one operator: invalid-cr/ plus
# the per-kind invalid-<kind>-cr/ directories.
corpus_dirs_for() {
  local op="$1" d
  for d in "tests/e2e/${op}/invalid-cr" "tests/e2e/${op}"/invalid-*-cr; do
    if [[ -d "${d}" ]]; then printf '%s\n' "${d}"; fi
  done
  return 0
}

# numbered_fixtures — the NN-*.yaml / NNN-*.yaml fixtures of one corpus. The
# ordinal prefix is two OR three digits: a corpus past 99 names its later
# fixtures 100-, and a two-digit-only glob would drop them.
numbered_fixtures() {
  find "$1" -maxdepth 1 \( -name '[0-9][0-9]-*.yaml' -o -name '[0-9][0-9][0-9]-*.yaml' \) | sort
}

# go_surfaces_for — the Go packages one operator's webhooks can reach: its own
# api/, every operators/<Y>/api its non-test code imports (the c5c3 webhook
# delegates to the service APIs), and internal/common.
go_surfaces_for() {
  local op="$1"
  {
    echo "operators/${op}/api"
    grep -rhoE 'github.com/c5c3/cobaltcore/operators/[a-z0-9]+/api' "operators/${op}" \
      --include='*.go' --exclude='*_test.go' 2>/dev/null \
      | sed 's#^github.com/c5c3/cobaltcore/##' || true
    echo "internal/common"
  } | sort -u | tr '\n' ' '
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
    helpers="$(grep -oE 'field\.(Invalid|Required|Forbidden|Duplicate|NotSupported|NotFound|TooMany|TooLong)' "${wh}" \
      | sort | uniq -c | sed 's/^ *//' | tr '\n' ' ' || true)"
    info "webhook ${wh}: ${helpers:-no field.* helpers}"
  done < <(find "operators/${op}/api" -name '*_webhook.go' ! -name '*_test.go' | sort)

  # Size of every rejection corpus.
  while IFS= read -r corpus; do
    n="$(numbered_fixtures "${corpus}" | wc -l | tr -d ' ')"
    info "corpus  ${corpus}: ${n} rejection fixture(s)"
  done < <(corpus_dirs_for "${op}")
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
hdr "V3: rejection-corpus chainsaw error assertions anchor to current rules"

# Extract the Go literal / marker records and the CRD property / boundary
# records once; every suite filters them to its operator's surfaces and kinds.
find operators/*/api internal/common -path internal/common/testutil -prune -o \
  -name '*.go' ! -name '*_test.go' ! -name 'zz_generated*' -print | sort > "${WORK}/go-files"
xargs awk -v mode=go -f "${ANCHOR_AWK}" < "${WORK}/go-files" > "${WORK}/go.rec"
awk -v mode=crd -f "${ANCHOR_AWK}" operators/*/config/crd/bases/*.yaml > "${WORK}/crd.rec"

TAB="$(printf '\t')"
V3_CATEGORIES="go template schema-phrase server-phrase field-path fixture"
for op in "${OPERATORS[@]}"; do
  surfaces="$(go_surfaces_for "${op}")"
  while IFS= read -r corpus; do
    suite="${corpus}/chainsaw-test.yaml"
    [[ -f "${suite}" ]] || continue
    label="${op} $(basename "${corpus}")"
    # Asserted substrings, commented-out steps excluded.
    grep -v '^[[:space:]]*#' "${suite}" \
      | grep -oE "contains\(\\\$error, '[^']+'\)" \
      | sed -E "s/^contains\(\\\$error, '(.*)'\)\$/\1/" | sort -u > "${WORK}/subs" || true
    fixtures=()
    while IFS= read -r f; do fixtures+=("${f}"); done < <(numbered_fixtures "${corpus}")
    awk -v mode=anchor -v op="${op}" -v surfaces="${surfaces}" \
      -v crdrec="${WORK}/crd.rec" -v gorec="${WORK}/go.rec" -v subs="${WORK}/subs" \
      -f "${ANCHOR_AWK}" ${fixtures[@]+"${fixtures[@]}"} \
      "${WORK}/crd.rec" "${WORK}/go.rec" "${WORK}/subs" > "${WORK}/anchors"
    total=0
    anchored=0
    while IFS="${TAB}" read -r cat sub reason; do
      total=$((total + 1))
      case "${cat}" in
        stale-boundary)
          fail "${label}: contains(\$error, '${sub}') — ${reason}; stale boundary? (${suite})" ;;
        none)
          fail "${label}: contains(\$error, '${sub}') — ${reason}; stale assertion? (${suite})" ;;
        *)
          anchored=$((anchored + 1)) ;;
      esac
    done < "${WORK}/anchors"
    breakdown="$(awk -F'\t' -v order="${V3_CATEGORIES}" '{ n[$1]++ }
      END { k = split(order, o, " "); s = ""
            for (i = 1; i <= k; i++) if (n[o[i]]) s = s (s == "" ? "" : ", ") o[i] " " n[o[i]]
            print s }' "${WORK}/anchors")"
    # The defaulting webhook's typed round trip materializes an omitted
    # non-pointer field, so the schema's required list rarely answers.
    if grep -qx 'Required value' "${WORK}/subs"; then
      info "${label}: asserts 'Required value' — on a cluster the defaulting webhook writes an omitted non-pointer field back as {} / \"\" and the next rule answers; confirm the field is a pointer or the webhook's own field.Required (${suite})"
    fi
    if [[ "${total}" -eq 0 ]]; then
      info "${label}: ${suite} asserts no \$error substrings"
    elif [[ "${anchored}" -eq "${total}" ]]; then
      pass "${label}: ${anchored}/${total} anchored (${breakdown})"
    else
      info "${label}: ${anchored}/${total} anchored (${breakdown:-none})"
    fi
  done < <(corpus_dirs_for "${op}")
done

# ---------------------------------------------------------------------------
# V4 — every XValidation rule carries a message
# ---------------------------------------------------------------------------
hdr "V4: every XValidation:rule= marker has a message="
V4_TOTAL=0
V4_BAD=0
while IFS= read -r line; do
  V4_TOTAL=$((V4_TOTAL + 1))
  if ! grep -q 'message=' <<<"${line}"; then
    V4_BAD=$((V4_BAD + 1))
    fail "XValidation rule without message= — $(printf '%s' "${line}" | cut -d: -f1,2)"
  fi
done < <(grep -rnE '\+kubebuilder:validation:XValidation:rule=' \
  operators/*/api internal/common --include='*.go' 2>/dev/null || true)
if [[ "${V4_BAD}" -eq 0 ]]; then
  pass "all ${V4_TOTAL} XValidation rule(s) carry a message="
fi

# ---------------------------------------------------------------------------
# V5 — validating-webhook operators have wired rejection corpora
# ---------------------------------------------------------------------------
hdr "V5: validating webhook ⇢ rejection corpus wiring"

# plural<TAB>kind for every generated CRD.
for crd in operators/*/config/crd/bases/*.yaml; do
  awk '/^    kind: / && k == "" { k = $2 } /^    plural: / && p == "" { p = $2 }
       END { if (k != "" && p != "") print p "\t" k }' "${crd}"
done > "${WORK}/plural-kind"

for op in "${OPERATORS[@]}"; do
  manifest="operators/${op}/config/webhook/manifests.yaml"
  [[ -f "${manifest}" ]] || { info "${op}: no ${manifest}"; continue; }
  if ! grep -q 'ValidatingWebhookConfiguration' "${manifest}"; then
    info "${op}: no ValidatingWebhookConfiguration in ${manifest}"
    continue
  fi
  corpora=()
  while IFS= read -r corpus; do corpora+=("${corpus}"); done < <(corpus_dirs_for "${op}")
  if [[ ${#corpora[@]} -eq 0 ]]; then
    info "${op}: GAP — validating webhook registered but no tests/e2e/${op}/invalid-cr/ or invalid-*-cr/ rejection corpus (grade per the skill report)"
    continue
  fi
  for corpus in "${corpora[@]}"; do
    if [[ ! -f "${corpus}/chainsaw-test.yaml" ]]; then
      fail "${op}: ${corpus}/ exists but has no chainsaw-test.yaml — fixtures are unreachable"
      continue
    fi
    refs="$(grep -cE 'file: *[0-9][0-9][0-9]?-' "${corpus}/chainsaw-test.yaml" || true)"
    if [[ "${refs}" -eq 0 ]]; then
      fail "${op}: ${corpus}/chainsaw-test.yaml references no [0-9][0-9][0-9]?-*.yaml fixture"
    else
      pass "${op}: ${corpus}/chainsaw-test.yaml wires ${refs} fixture reference(s)"
    fi
  done
  # Every resource the ValidatingWebhookConfiguration covers is applied by a
  # fixture of some corpus of this operator.
  corpus_fixtures=()
  for corpus in "${corpora[@]}"; do
    while IFS= read -r f; do corpus_fixtures+=("${f}"); done < <(numbered_fixtures "${corpus}")
  done
  applied=""
  if [[ ${#corpus_fixtures[@]} -gt 0 ]]; then
    applied="$(awk '/^kind:[ \t]/ { print $2 }' "${corpus_fixtures[@]}" | sort -u | tr '\n' ' ')"
  fi
  while IFS= read -r res; do
    [[ -n "${res}" ]] || continue
    kind="$(awk -F'\t' -v p="${res}" '$1 == p { print $2; exit }' "${WORK}/plural-kind")"
    if [[ -z "${kind}" ]]; then
      info "${op}: validated resource ${res} has no generated CRD under operators/*/config/crd/bases/"
    elif [[ " ${applied} " == *" ${kind} "* ]]; then
      pass "${op}: validated resource ${res} (${kind}) is applied by a rejection corpus"
    else
      info "${op}: GAP — ${kind} (${res}) is webhook-validated but no rejection corpus applies it (grade per the skill report)"
    fi
  done < <(awk '/^kind: ValidatingWebhookConfiguration/ { v = 1; next }
                /^kind: / { v = 0 }
                v && /^[ \t]*resources:/ { r = 1; next }
                v && r && /^[ \t]*- / { sub(/^[ \t]*- /, ""); print; next }
                { r = 0 }' "${manifest}" | sort -u)
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
