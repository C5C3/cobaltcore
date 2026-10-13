#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify docs/quick-start-orders.md, the quick start for the orders of a
# service the control plane does not manage:
#   1. the frontmatter holds `title: Quick Start (Orders)` and no other key,
#      the SPDX comment is present, and the H1 follows it
#   2. docs/.vitepress/config.ts lists the page after the metal-stack quick
#      start and before the Architecture group
#   3. the six `## ` sections occur once each and in order, every
#      `### Step` heading carries an explicit anchor, `cp-` in Part 1 and `tc-`
#      in Part 2, and Part 2 keeps `### The path from the target {#tc-path}`,
#      which the guide and the diagram catalogue link
#   4. every `chainsaw test --test-dir` path resolves to a chainsaw-test.yaml
#   5. every `<<< @/../` import names a file that holds its region once, as a
#      column-0 `# region <name>` before a column-0 `# endregion <name>`
#   6. the page names `targetPort: 31443` once, hack/kind-config.yaml maps
#      `containerPort: 31443`, the Envoy Gateway's NodePort the Service of Part
#      2 forwards to, and both forms of the target-path figure name
#      `NodePort 31443`
#   7. the page names publishedDatabaseEndpoint and publishedMessagingEndpoint,
#      and both are fields of the ControlPlane API
#   8. the Devstack box names ./quick-start-controlplane.md once and holds the
#      verbatim bring-up of that devstack
#   9. Part 2 registers the target with both keys, namespaces=openstack and
#      assignedNamespaces=workflow, and waits for `cluster engaged`
#  10. Part 1 numbers its steps 1 to 10, Step 7 is the rotation {#cp-rotation}
#      and Steps 8 to 10 keep {#cp-database}, {#cp-vhost} and {#cp-teardown};
#      only Part 1 shortens the schedule (interval: 3m, gracePeriod: 2m), Part 2
#      points at Part 1 Step 10, Step 6 keeps the generation-1 copy, the verify
#      Job still holds the two strings Step 7 rewrites with sed, and Step 7 runs
#      its commands in the order the rotation needs: the wait for generation 2
#      or later and its timeout message, the schedule off, the late reader's
#      wait for the Secret and its timeout message, copy, raise and wait, the
#      copy authenticating, the wait for the cleared previousCredentialID, the
#      copy refused, the raise by one, the wait for the Secret, the copy
#      deleted; both runs with the copy repoint the Job's Secret
#
# Heading scans skip fenced code. QUICK_START_DOC overrides the page. The test
# needs bash, grep, sed and awk alone and runs under Bash 3.2.
#
# Usage: bash tests/unit/docs/quick_start_orders_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

QUICK_START_DOC="${QUICK_START_DOC:-$PROJECT_ROOT/docs/quick-start-orders.md}"
VITEPRESS_CONFIG="$PROJECT_ROOT/docs/.vitepress/config.ts"
KIND_CONFIG="$PROJECT_ROOT/hack/kind-config.yaml"
CONTROLPLANE_TYPES="$PROJECT_ROOT/operators/c5c3/api/v1alpha1/controlplane_types.go"
APPCRED_VERIFY_JOB="$PROJECT_ROOT/tests/e2e/c5c3/keystone-user/11-openstack-appcred-verify-job.yaml"

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

# step_headings prints the `### Step` headings of Part $1 outside fenced code.
step_headings() {
  section "^## Part $1: " | awk '
    /^[[:space:]]*(```|~~~)/ { fenced = !fenced; next }
    !fenced && /^### Step / { print }
  '
}

# step_text prints Step $2 of Part $1, its heading included, up to the next
# `### ` heading.
step_text() {
  section "^## Part $1: " | awk -v heading="### Step $2: " '
    index($0, heading) == 1 { inside = 1; print; next }
    inside && /^### / { exit }
    inside { print }
  '
}

# record <ok 0|1> <description>: one PASS or FAIL.
record() {
  if [[ "$1" -eq 1 ]]; then
    echo "  PASS: $2"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $2"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 1: frontmatter, SPDX comment and H1 ---
test_frontmatter() {
  echo "Test: the frontmatter holds the title alone, the SPDX comment and the H1 follow"
  assert_eq "line 1 opens the frontmatter" "---" "$(head -n 1 "$QUICK_START_DOC")"
  local keys
  keys="$(awk 'NR == 1 && $0 != "---" { exit } NR > 1 && /^---$/ { exit } NR > 1 && /^[A-Za-z0-9_-]+:/ { print }' "$QUICK_START_DOC")"
  assert_eq "the frontmatter holds 'title: Quick Start (Orders)' and no other key" \
    "title: Quick Start (Orders)" "$keys"
  assert_file_contains_fixed "SPDX copyright line present" "$QUICK_START_DOC" \
    "SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company"
  assert_file_contains_fixed "SPDX license line present" "$QUICK_START_DOC" \
    "SPDX-License-Identifier: Apache-2.0"

  # The first non-blank line after the comment that closes the SPDX block is
  # the page's one H1.
  local after
  after="$(awk 'closed && NF { print; exit } /^-->$/ { closed = 1 }' "$QUICK_START_DOC")"
  assert_starts_with "the H1 follows the SPDX comment" "$after" "# Quick Start (Orders)"
  assert_eq "the page holds one H1" "1" "$(headings '#' | grep -c . || true)"
}

# --- Test 2: the sidebar item ---
test_sidebar() {
  echo "Test: the sidebar lists the page after the metal-stack quick start"
  local metal_stack orders architecture ok=0
  metal_stack="$(grep -nF "link: '/quick-start-metal-stack'" "$VITEPRESS_CONFIG" | head -n 1 | cut -d: -f1)"
  orders="$(grep -nF "{ text: 'Quick Start (Orders)', link: '/quick-start-orders' }" "$VITEPRESS_CONFIG" | head -n 1 | cut -d: -f1)"
  architecture="$(grep -nE "^[[:space:]]*text: 'Architecture',[[:space:]]*$" "$VITEPRESS_CONFIG" | head -n 1 | cut -d: -f1)"
  if [[ -n "$metal_stack" && -n "$orders" && -n "$architecture" ]] &&
    ((metal_stack < orders && orders < architecture)); then
    ok=1
  fi
  record "$ok" "sidebar item between the metal-stack item and the Architecture group (metal-stack '${metal_stack}', Orders '${orders}', Architecture '${architecture}')"
}

# --- Test 3: sections and step anchors ---
test_sections() {
  echo "Test: the six sections occur once each, in order"
  local sections pattern name count line previous=0 ordered=1
  sections="$(headings '##')"
  for pattern in '^## Prerequisites$' '^## Part 1: The management cluster' '^## Part 2: A target cluster' \
    '^## Teardown$' '^## Tested by$' '^## Related references$'; do
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
  record "$ordered" "the sections are in order"

  echo "Test: every step heading carries an explicit anchor of its part"
  local part steps step anchored
  for part in "1:cp" "2:tc"; do
    steps="$(step_headings "${part%%:*}")"
    if [[ -z "$steps" ]]; then
      record 0 "Part ${part%%:*} holds step headings"
      continue
    fi
    while IFS= read -r step; do
      anchored=0
      if grep -qE "\{#${part#*:}-[a-z0-9-]+\}[[:space:]]*$" <<<"$step"; then
        anchored=1
      fi
      record "$anchored" "'${step}' carries a {#${part#*:}-...} anchor"
    done <<<"$steps"
  done
  assert_contains "Part 2 keeps the path anchor other pages link" "$(section '^## Part 2: ')" \
    "### The path from the target {#tc-path}"
}

# --- Test 4: the suites Tested by names ---
test_tested_by_paths_resolve() {
  echo "Test: every chainsaw test --test-dir path resolves to a chainsaw-test.yaml"
  local dirs dir
  dirs="$(section '^## Tested by$' | grep -oE 'chainsaw test --test-dir [^[:space:]]+' |
    sed 's|^chainsaw test --test-dir ||' || true)"
  if [[ -z "$dirs" ]]; then
    record 0 "Tested by names a suite"
    return
  fi
  while IFS= read -r dir; do
    if [[ -f "$PROJECT_ROOT/$dir/chainsaw-test.yaml" ]]; then
      record 1 "$dir holds a chainsaw-test.yaml"
    else
      record 0 "$dir holds a chainsaw-test.yaml"
    fi
  done <<<"$dirs"
}

# --- Test 5: code imports and their regions ---
test_imports_resolve_with_balanced_regions() {
  echo "Test: every code import names a file that holds its region once, in order"
  local imports import file region opens closes first_open first_close ok
  imports="$(grep -E '^<<< @/\.\./' "$QUICK_START_DOC" | sed -E 's|^<<< @/\.\./||; s/[[:space:]]+$//' || true)"
  if [[ -z "$imports" ]]; then
    record 0 "the page imports a fixture"
    return
  fi
  while IFS= read -r import; do
    file="${import%%#*}"
    region="${import#*#}"
    if [[ "$region" == "$import" || -z "$region" ]]; then
      record 0 "code import $import names a region"
      continue
    fi
    if [[ ! -f "$PROJECT_ROOT/$file" ]]; then
      record 0 "code import $file exists"
      continue
    fi
    opens="$(grep -cxF "# region ${region}" "$PROJECT_ROOT/$file" || true)"
    closes="$(grep -cxF "# endregion ${region}" "$PROJECT_ROOT/$file" || true)"
    first_open="$(grep -nxF "# region ${region}" "$PROJECT_ROOT/$file" | head -n 1 | cut -d: -f1)"
    first_close="$(grep -nxF "# endregion ${region}" "$PROJECT_ROOT/$file" | head -n 1 | cut -d: -f1)"
    ok=0
    if [[ "$opens" == "1" && "$closes" == "1" ]] && ((first_open < first_close)); then
      ok=1
    fi
    record "$ok" "$file holds region '${region}' once, opened before it is closed"
  done <<<"$imports"
}

# --- Test 6: the Service of Part 2 forwards to the Gateway's NodePort ---
test_target_port_is_the_envoy_node_port() {
  echo "Test: the page forwards to the NodePort hack/kind-config.yaml bridges"
  assert_eq "the page names 'targetPort: 31443' once" "1" \
    "$(grep -cF 'targetPort: 31443' "$QUICK_START_DOC" || true)"
  assert_file_contains_fixed "hack/kind-config.yaml maps containerPort 31443" "$KIND_CONFIG" \
    "containerPort: 31443"
  local ext
  for ext in drawio svg; do
    assert_file_contains_fixed "the target-path figure (.$ext) names NodePort 31443" \
      "$PROJECT_ROOT/docs/diagrams/controlplane-orders-target-path.$ext" "NodePort 31443"
  done
}

# --- Test 7: the published fields exist ---
test_published_fields_exist() {
  echo "Test: the published addresses the page sets are ControlPlane fields"
  local field
  for field in publishedDatabaseEndpoint publishedMessagingEndpoint; do
    assert_file_contains_fixed "the page names $field" "$QUICK_START_DOC" "$field"
    assert_file_contains_fixed "$field is a ControlPlane field" "$CONTROLPLANE_TYPES" "json:\"${field},"
  done
}

# --- Test 8: the Devstack box ---
test_devstack_box() {
  echo "Test: the Devstack box names the ControlPlane quick start once with its bring-up"
  local box
  box="$(awk '/^::: info Devstack/ { inside = 1; next } inside && /^:::[[:space:]]*$/ { exit } inside { print }' "$QUICK_START_DOC")"
  assert_not_empty "the page holds a Devstack box" "$box"
  assert_eq "the box names ./quick-start-controlplane.md once" "1" \
    "$(grep -oF './quick-start-controlplane.md' <<<"$box" | grep -c . || true)"
  assert_contains "the box holds the verbatim bring-up" "$box" \
    "KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra"
}

# --- Test 9: the two-key registration of Part 2 ---
test_part_2_registers_two_keys() {
  echo "Test: Part 2 registers the target with both keys and waits for the engagement"
  local part2
  part2="$(section '^## Part 2: ')"
  assert_contains "the registration carries the placed namespace" "$part2" "--from-literal=namespaces=openstack"
  assert_contains "the registration carries the assigned namespace" "$part2" \
    "--from-literal=assignedNamespaces=workflow"
  assert_contains "the chart assigns the same namespace" "$part2" "assignedNamespaces={workflow}"
  assert_contains "Part 2 waits for the engagement" "$part2" "cluster engaged"
}

# --- Test 10: the rotation step of Part 1 ---
test_part_1_rotation_step() {
  echo "Test: Part 1 numbers ten steps and Step 7 watches a rotation in order"
  local steps numbers want_numbers part1 part2 step7 needle line previous=0 ordered=1 missing=""
  part1="$(section '^## Part 1: ')"
  part2="$(section '^## Part 2: ')"
  steps="$(step_headings 1)"
  numbers="$(sed -E 's/^### Step ([0-9]+):.*/\1/' <<<"$steps" | tr '\n' ' ')"
  want_numbers="1 2 3 4 5 6 7 8 9 10 "
  assert_eq "Part 1 numbers its steps 1 to 10 in order" "$want_numbers" "$numbers"
  assert_contains "Step 7 is the rotation" "$steps" "### Step 7: Watch the credential rotate {#cp-rotation}"
  assert_contains "Step 8 keeps its anchor" "$steps" "### Step 8: Order a database {#cp-database}"
  assert_contains "Step 9 keeps its anchor" "$steps" "### Step 9: Order a vhost {#cp-vhost}"
  assert_contains "Step 10 keeps its anchor" "$steps" "### Step 10: Tear the orders down {#cp-teardown}"

  assert_eq "Part 1 sets 'interval: 3m' once" "1" "$(grep -cE '^[[:space:]]+interval: 3m$' <<<"$part1" || true)"
  assert_eq "Part 1 sets 'gracePeriod: 2m' once" "1" "$(grep -cE '^[[:space:]]+gracePeriod: 2m$' <<<"$part1" || true)"
  assert_eq "Part 2 keeps the default schedule" "0" "$(grep -c 'gracePeriod:' <<<"$part2" || true)"
  assert_file_contains_fixed "Part 2 points at Part 1 Step 10" "$QUICK_START_DOC" "Part 1 Step 10"
  assert_file_not_contains "no reference to Part 1 Step 9 is left" "$QUICK_START_DOC" "Part 1 Step 9"

  assert_contains "Step 6 keeps the generation-1 copy Step 7 mounts" "$(step_text 1 6)" \
    'metadata: {name: "workflow-appcred-gen1", namespace: .metadata.namespace},'
  # Step 7 rewrites the verify Job with sed, which changes nothing silently
  # once the Job no longer holds the string it replaces.
  assert_file_contains_fixed "the verify Job Step 7 renames is named workflow-appcred-verify" \
    "$APPCRED_VERIFY_JOB" "name: workflow-appcred-verify"
  assert_file_contains_fixed "the verify Job Step 7 repoints mounts workflow-appcred-credentials" \
    "$APPCRED_VERIFY_JOB" "secretName: workflow-appcred-credentials"

  step7="$(step_text 1 7)"
  # Each needle is searched after the previous one's line, so a needle may
  # occur twice in the list.
  while IFS= read -r needle; do
    line="$(grep -nF -- "$needle" <<<"$step7" | awk -F: -v after="$previous" '$1 > after { print $1; exit }')"
    if [[ -n "$line" ]]; then
      previous="$line"
    else
      ordered=0
      if ! grep -qF -- "$needle" <<<"$step7"; then
        missing+="${missing:+, }'${needle}'"
      fi
    fi
  done <<'EOF'
[ "${gen:-0}" -ge 2 ] && break
[ "${gen:-0}" -ge 2 ] || echo "generation 2 did not appear within five minutes"
-p '{"spec":{"rotation":{"interval":"0s"}}}'
[ "$got" = "$id" ] && break
[ "$got" = "$id" ] || echo "the Secret did not take credential $id within three minutes"
metadata: {name: "workflow-appcred-gen1", namespace: .metadata.namespace},
-p "{\"spec\":{\"credentialGeneration\":$((gen + 1))}}"
--for=jsonpath="{.status.credentialGeneration}"=$((gen + 1))
secretName: workflow-appcred-credentials|secretName: workflow-appcred-gen1|
--for=condition=complete job/workflow-appcred-gen1-verify
-o jsonpath='{.status.previousCredentialID}')" ] && break
secretName: workflow-appcred-credentials|secretName: workflow-appcred-gen1|
--for=condition=failed job/workflow-appcred-gen1-verify
-p "{\"spec\":{\"credentialGeneration\":$((gen + 1))}}"
-o jsonpath='{.data.application_credential_id}' | base64 -d)" = "$id" ] && break
kubectl delete secret workflow-appcred-gen1 -n workflow
EOF
  record "$ordered" "Step 7 runs its commands in the order of the rotation${missing:+ (missing: ${missing})}"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_frontmatter
test_sidebar
test_sections
test_tested_by_paths_resolve
test_imports_resolve_with_balanced_regions
test_target_port_is_the_envoy_node_port
test_published_fields_exist
test_devstack_box
test_part_2_registers_two_keys
test_part_1_rotation_step

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
