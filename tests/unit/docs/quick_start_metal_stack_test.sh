#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify docs/quick-start-metal-stack.md, the lab's one runbook:
#   1. the frontmatter holds `title: Quick Start (metal-stack)` and no other
#      key, and the SPDX comment is present
#   2. docs/.vitepress/config.ts lists the page after the ControlPlane quick
#      start, in the Getting Started group
#   3. the six `## ` sections occur once each and in order, and the fifteen
#      `### Step` headings carry their explicit anchors
#   4. every directory the page applies holds a kustomization.yaml, every
#      deploy/, hack/ or tests/ file it names exists and each such script is
#      executable, both code imports resolve, and every #anchor it links
#      resolves on the target page
#   5. the page holds the deploy and teardown commands, and the guide
#      conventions list it as a devstack with the same deploy command
#   6. Part 2 links no anchor of Part 1 and defines `nodes` and `zone` itself
#   7. four commands of the run sequence occur once on the page and not in
#      docs/reference/infrastructure/infrastructure-manifests.md, which links
#      the page
#   8. Proven by names the date of a lab run and no chainsaw suite
#   9. Part 1, Step 4 holds no instruction to repeat the apply: deploy-infra
#      returns only once the cluster admits it
#
# Heading scans skip fenced code. QUICK_START_DOC overrides the page.
#
# Usage: bash tests/unit/docs/quick_start_metal_stack_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

QUICK_START_DOC="${QUICK_START_DOC:-$PROJECT_ROOT/docs/quick-start-metal-stack.md}"
VITEPRESS_CONFIG="$PROJECT_ROOT/docs/.vitepress/config.ts"
GUIDE_CONVENTIONS="$PROJECT_ROOT/docs/contributing/guide-conventions.md"
INFRA_MANIFESTS="$PROJECT_ROOT/docs/reference/infrastructure/infrastructure-manifests.md"

if [[ ! -f "$QUICK_START_DOC" ]]; then
  echo "FAIL: $QUICK_START_DOC does not exist"
  exit 1
fi

# headings prints "<line>:<text>" for every Markdown heading outside fenced
# code; $1 is the heading level ("##", "###").
headings() {
  awk -v level="$1" '
    /^[[:space:]]*(```|~~~)/ { fenced = !fenced; next }
    !fenced && index($0, level " ") == 1 { print NR ":" $0 }
  ' "$QUICK_START_DOC"
}

# section prints the lines of the `## ` section whose heading matches the
# extended regular expression $1, fenced code included, up to the next `## `.
section() {
  awk -v start="$1" '
    /^[[:space:]]*(```|~~~)/ { fenced = !fenced }
    !fenced && /^## / { if (inside) exit; if ($0 ~ start) { inside = 1; next } }
    inside { print }
  ' "$QUICK_START_DOC"
}

# anchors prints the anchors of the Markdown page $1, outside fenced code: a
# heading's explicit {#id}, or else the slug VitePress derives from its text
# (links reduced to their text, every run of spaces and ASCII punctuation one
# hyphen, a leading digit prefixed with `_`, lower case, `-<n>` on a repeat).
anchors() {
  awk '
    /^[[:space:]]*(```|~~~)/ { fenced = !fenced; next }
    fenced || !/^#+ / { next }
    {
      t = $0
      sub(/^#+[[:space:]]+/, "", t)
      if (match(t, /\{#[^}]+\}[[:space:]]*$/)) {
        t = substr(t, RSTART + 2)
        sub(/\}[[:space:]]*$/, "", t)
        print t
        next
      }
      while (match(t, /\[[^]]*\]\([^)]*\)/)) {
        link = substr(t, RSTART + 1, RLENGTH - 1)
        sub(/\]\(.*$/, "", link)
        t = substr(t, 1, RSTART - 1) link substr(t, RSTART + RLENGTH)
      }
      gsub(/[][[:space:]~`!@#$%^&*()_+={}|\\;:"'\''<>,.?\/-]+/, "-", t)
      sub(/^-/, "", t)
      sub(/-$/, "", t)
      if (t ~ /^[0-9]/) t = "_" t
      t = tolower(t)
      n = seen[t]++
      print (n ? t "-" n : t)
    }
  ' "$1"
}

# --- Test 1: frontmatter and SPDX comment ---
test_frontmatter() {
  echo "Test: the frontmatter holds the title alone, and the SPDX comment is present"
  assert_eq "line 1 opens the frontmatter" "---" "$(head -n 1 "$QUICK_START_DOC")"
  local keys
  keys="$(awk 'NR == 1 && $0 != "---" { exit } NR > 1 && /^---$/ { exit } NR > 1 && /^[A-Za-z0-9_-]+:/ { print }' "$QUICK_START_DOC")"
  assert_eq "the frontmatter holds 'title: Quick Start (metal-stack)' and no other key" \
    "title: Quick Start (metal-stack)" "$keys"
  assert_file_contains_fixed "SPDX copyright line present" "$QUICK_START_DOC" \
    "SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company"
  assert_file_contains_fixed "SPDX license line present" "$QUICK_START_DOC" \
    "SPDX-License-Identifier: Apache-2.0"
}

# --- Test 2: the sidebar item ---
test_sidebar() {
  echo "Test: the sidebar lists the page after the ControlPlane quick start"
  local controlplane metal_stack architecture
  controlplane="$(grep -nF "link: '/quick-start-controlplane'" "$VITEPRESS_CONFIG" | head -n 1 | cut -d: -f1)"
  metal_stack="$(grep -nF "{ text: 'Quick Start (metal-stack)', link: '/quick-start-metal-stack' }" "$VITEPRESS_CONFIG" | head -n 1 | cut -d: -f1)"
  architecture="$(grep -nE "^[[:space:]]*text: 'Architecture',[[:space:]]*$" "$VITEPRESS_CONFIG" | head -n 1 | cut -d: -f1)"
  if [[ -n "$controlplane" && -n "$metal_stack" && -n "$architecture" ]] &&
    ((controlplane < metal_stack && metal_stack < architecture)); then
    echo "  PASS: sidebar item on line $metal_stack, between the ControlPlane item and the Architecture group"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: sidebar item missing or out of place (ControlPlane '${controlplane}', metal-stack '${metal_stack}', Architecture group '${architecture}')"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 3: sections and step anchors ---
test_sections() {
  echo "Test: the six sections occur once each, in order"
  local sections pattern name count line previous=0 ordered=1
  sections="$(headings '##')"
  for pattern in '^## Prerequisites$' '^## Part 1: ' '^## Part 2: ' '^## Teardown$' '^## Caveats$' '^## Proven by$'; do
    name="${pattern#^## }"
    name="${name%$}"
    count="$(cut -d: -f2- <<<"$sections" | grep -cE "$pattern" || true)"
    assert_eq "'## ${name}' occurs once" "1" "$count"
    line="$(grep -E "^[0-9]+:${pattern#^}" <<<"$sections" | head -n 1 | cut -d: -f1)"
    if [[ -z "$line" ]] || ((line < previous)); then
      ordered=0
    else
      previous="$line"
    fi
  done
  if [[ "$ordered" -eq 1 ]]; then
    echo "  PASS: the sections are in order"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the sections are missing or out of order"
    FAIL=$((FAIL + 1))
  fi

  echo "Test: the fifteen step headings carry their anchors"
  local want got
  want="1:cp-clone 2:cp-probe 3:cp-deploy 4:cp-apply 5:cp-tenant 6:cp-access 7:cp-verify 1:hv-nodes 2:hv-fixtures 3:hv-apply 4:hv-onboarding 5:hv-boot 6:hv-console 7:hv-migrate 8:hv-evict"
  got="$(headings '###' | cut -d: -f2- | grep '^### Step ' |
    sed -E 's/^### Step ([0-9]+):.*\{#([a-z-]+)\}[[:space:]]*$/\1:\2/' | tr '\n' ' ' | sed 's/ $//' || true)"
  assert_eq "step numbers and anchors" "$want" "$got"
}

# --- Test 4: what the page applies, names and links exists ---
test_references_resolve() {
  echo "Test: the applied directories, the named files, the code imports and the anchors exist"
  local dirs dir paths path imports import found=0 links link target page fragment
  dirs="$(grep -oE 'kubectl apply -k deploy/lab/metal-stack/[A-Za-z0-9_-]+' "$QUICK_START_DOC" |
    sed 's|^kubectl apply -k ||' | sort -u || true)"
  if [[ -z "$dirs" ]]; then
    echo "  FAIL: the page applies no deploy/lab/metal-stack/ directory"
    FAIL=$((FAIL + 1))
  fi
  while IFS= read -r dir; do
    [[ -z "$dir" ]] && continue
    if [[ -f "$PROJECT_ROOT/$dir/kustomization.yaml" ]]; then
      echo "  PASS: $dir holds a kustomization.yaml"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $dir holds no kustomization.yaml"
      FAIL=$((FAIL + 1))
    fi
  done <<<"$dirs"

  paths="$(grep -oE '(deploy|hack|tests)/[A-Za-z0-9_./-]+\.(sh|yaml)' "$QUICK_START_DOC" | sort -u || true)"
  if [[ -z "$paths" ]]; then
    echo "  FAIL: the page names no deploy/, hack/ or tests/ file"
    FAIL=$((FAIL + 1))
  fi
  while IFS= read -r path; do
    [[ -z "$path" ]] && continue
    if [[ ! -f "$PROJECT_ROOT/$path" ]]; then
      echo "  FAIL: $path does not exist"
      FAIL=$((FAIL + 1))
    elif [[ "$path" != *.sh ]]; then
      echo "  PASS: $path exists"
      PASS=$((PASS + 1))
    elif [[ -x "$PROJECT_ROOT/$path" ]]; then
      echo "  PASS: $path is executable"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $path is not executable"
      FAIL=$((FAIL + 1))
    fi
  done <<<"$paths"

  imports="$(grep -E '^<<< @/\.\./' "$QUICK_START_DOC" | sed -E 's|^<<< @/\.\./||; s/[{#].*$//; s/[[:space:]]+$//' || true)"
  while IFS= read -r import; do
    [[ -z "$import" ]] && continue
    found=$((found + 1))
    if [[ -f "$PROJECT_ROOT/$import" ]]; then
      echo "  PASS: code import $import resolves"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: code import $import does not resolve"
      FAIL=$((FAIL + 1))
    fi
  done <<<"$imports"
  assert_eq "the page holds two code imports" "2" "$found"

  links="$(grep -oE '\]\((\./[^)#[:space:]]+\.md)?#[^)[:space:]]+\)' "$QUICK_START_DOC" | sort -u || true)"
  while IFS= read -r link; do
    [[ -z "$link" ]] && continue
    target="${link#](}"
    target="${target%)}"
    fragment="${target#*#}"
    page="$QUICK_START_DOC"
    [[ "$target" == ./* ]] && page="$(dirname "$QUICK_START_DOC")/${target%%#*}"
    if [[ -f "$page" ]] && grep -qxF -- "$fragment" <<<"$(anchors "$page")"; then
      echo "  PASS: $target resolves"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $target does not resolve"
      FAIL=$((FAIL + 1))
    fi
  done <<<"$links"
}

# --- Test 5: bring-up, teardown and the devstack row ---
test_devstack() {
  echo "Test: the page names its bring-up and teardown, and the conventions list it"
  local deploy='EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true make deploy-infra'
  assert_file_contains_fixed "the page holds the deploy command" "$QUICK_START_DOC" "$deploy"
  assert_file_contains_fixed "the page holds the teardown command" "$QUICK_START_DOC" \
    'EXTERNAL_CLUSTER=true make teardown-infra'
  local row
  row="$(grep -F '../quick-start-metal-stack.md' "$GUIDE_CONVENTIONS" | grep '^|' | grep -F "$deploy" || true)"
  assert_not_empty "guide-conventions.md has a devstack row with the page and its deploy command" "$row"
}

# --- Test 6: Part 2 stands on its own ---
test_part_2_standalone() {
  echo "Test: Part 2 links no anchor of Part 1 and defines nodes and zone"
  local body ids id hits=0
  body="$(section '^## Part 2: ')"
  assert_not_contains "Part 2 holds no link to a #cp- anchor" "$body" "](#cp-"
  ids="$({
    headings '##' | grep -E '^[0-9]+:## Part 1: '
    section '^## Part 1: '
  } | grep -oE '\{#[A-Za-z0-9_-]+\}' | tr -d '{#}' | sort -u || true)"
  assert_not_empty "Part 1 carries explicit anchors" "$ids"
  while IFS= read -r id; do
    [[ -z "$id" ]] && continue
    if grep -qE "\]\(((\./)?quick-start-metal-stack\.md)?#${id}\)" <<<"$body"; then
      echo "  FAIL: Part 2 links #${id} of Part 1"
      FAIL=$((FAIL + 1))
      hits=$((hits + 1))
    fi
  done <<<"$ids"
  if [[ "$hits" -eq 0 ]]; then
    echo "  PASS: Part 2 links no anchor of Part 1"
    PASS=$((PASS + 1))
  fi
  assert_not_empty "Part 2 defines nodes=" "$(grep -E '^nodes=' <<<"$body" || true)"
  assert_not_empty "Part 2 defines zone=" "$(grep -E '^zone=' <<<"$body" || true)"
}

# --- Test 7: one runbook ---
test_one_runbook() {
  echo "Test: the run sequence lives on the page alone"
  local command count
  for command in \
    'kubectl apply -k deploy/lab/metal-stack/controlplane' \
    'kubectl apply -k deploy/lab/metal-stack/hypervisor-fixtures' \
    'trait create CUSTOM_C5C3_LAB' \
    'server create lab-a'; do
    count="$(grep -cF -- "$command" "$QUICK_START_DOC" || true)"
    assert_eq "'$command' occurs on one line of the page" "1" "$count"
    count="$(grep -cF -- "$command" "$INFRA_MANIFESTS" || true)"
    assert_eq "'$command' does not occur in infrastructure-manifests.md" "0" "$count"
  done
  assert_file_contains_fixed "infrastructure-manifests.md links the page" "$INFRA_MANIFESTS" \
    'quick-start-metal-stack.md'
}

# --- Test 8: Proven by ---
test_proven_by() {
  echo "Test: Proven by names the date of a lab run and no chainsaw suite"
  local body
  body="$(section '^## Proven by$')"
  assert_not_empty "Proven by holds a date (YYYY-MM-DD)" \
    "$(grep -E '20[0-9]{2}-[0-9]{2}-[0-9]{2}' <<<"$body" || true)"
  assert_not_contains "Proven by names no chainsaw suite" "$body" "chainsaw test --test-dir"
}

# --- Test 9: Step 4 applies once ---
test_step_4_has_no_retry_instruction() {
  echo "Test: Part 1, Step 4 holds no instruction to repeat the apply"
  local step
  step="$(section '^## Part 1: ' |
    awk '/^### Step 4:/ { inside = 1; next } inside && /^### / { exit } inside { print }')"
  assert_not_empty "Part 1 holds Step 4" "$step"
  assert_not_contains "Step 4 holds no 'Repeat'" "$step" "Repeat"
  assert_not_contains "Step 4 holds no 'until it succeeds'" "$step" "until it succeeds"
}

test_frontmatter
test_sidebar
test_sections
test_references_resolve
test_devstack
test_part_2_standalone
test_one_runbook
test_proven_by
test_step_4_has_no_retry_instruction

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
