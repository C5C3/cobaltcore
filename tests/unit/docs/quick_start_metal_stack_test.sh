#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify docs/quick-start-metal-stack.md, the lab's one runbook:
#   1. the frontmatter holds `title: Quick Start (metal-stack)` and no other
#      key, and the SPDX comment is present
#   2. docs/.vitepress/config.ts lists the page after the ControlPlane quick
#      start, in the Getting Started group
#   3. the seven `## ` sections occur once each and in order, Part 3 between
#      Part 2 and the Teardown, and the twenty-two `### Step` headings carry
#      their explicit anchors
#   4. every directory the page applies holds a kustomization.yaml, every
#      deploy/, hack/ or tests/ file it names exists and each such script is
#      executable, the three code imports resolve, and every #anchor it links
#      resolves on the target page
#   5. the page holds the deploy and teardown commands, the guide
#      conventions list it as a devstack with the same deploy command, and
#      the guide scaffold prints that command as the bring-up, refuses a
#      second `--opt-in WITH_CEPH=true` and a kind-only `--opt-in
#      WITH_REGISTRY_CACHE=true`, and accepts `--opt-in WITH_CHAOS_MESH=true`,
#      `--opt-in WITH_DIZZY=true`, `--opt-in WITH_PROMETHEUS=true` and
#      `--opt-in WITH_NFS=true`, which the lab overlay carries; of the
#      WITH_* flags hack/deploy-infra.sh declares, it refuses exactly those
#      its preflight_external_cluster refuses
#   6. Part 2 links no anchor of Part 1 and defines `nodes` and `zone` itself
#   7. the run-sequence commands of tests/lib/quick_start_metal_stack.sh occur
#      once on the page and not in
#      docs/reference/infrastructure/infrastructure-manifests.md, which links
#      the page; a line counts when the command is not followed by "-", so the
#      apply of controlplane/ leaves out the apply of controlplane-nfs/
#   8. Proven by names the date of a lab run and no chainsaw suite
#   9. Part 1, Step 4 holds no instruction to repeat the apply: deploy-infra
#      returns only once the cluster admits it
#  10. the page holds no instruction to run a block again: the MariaDB wait of
#      Part 1, Step 5 follows a create wait, the Hypervisor loop of Part 2,
#      Step 3 follows the waits for the operator's release and its CRD, and
#      the node port check of Part 2, Step 1 follows the reservation of the
#      migration ports and its rollout, and the eight volume, backup and
#      attach commands of Part 2 and the four of Part 3 occur once each and in
#      this order, each followed on the next line by a `timeout` wait for the
#      status it reaches
#  11. no line of the page holds a hand step the operators took over: the
#      highAvailability patch, the custom trait (its annotation and its
#      create) and the host discovery, nor any other file under tests/
#  12. the Prerequisites name no value only one shoot has: no class
#      `premium`, no address of the 10.248. service network, and no row whose
#      failure shows only later (`nothing checks it`, `already allocated`)
#  13. the bash block of the Teardown is `make teardown-infra` alone, with no
#      `openstack` command before it, because the teardown deletes the
#      servers, backups, snapshots and volumes itself; the section's prose
#      names every backup and volume the page's code creates, and the check
#      reads a name from every such create
#  14. no line of the page, nor of the "Checks outside the quick start"
#      section of infrastructure-manifests.md that builds on Part 2, indexes
#      a shell array by number or names a node as `nodes[<n>]`: bash counts
#      an array index from 0 and zsh from 1
#  15. Part 2, Step 5 boots a server on every node: its one server create
#      runs in a loop over `nodes` and names the loop's node as the host of
#      the availability zone
#  16. the Caveats link the lab fault runs of
#      docs/reference/infrastructure/infrastructure-manifests.md#lab-fault-runs
#      and do not say that no lab run has tested the NFS outage
#  17. Part 1, Step 3 links the platform's autoscalers of
#      docs/reference/infrastructure/infrastructure-manifests.md#lab-autoscaling,
#      and the page does not say that the VPA needs a kind node
#  18. Part 1, Step 3 names the optional WITH_PROMETHEUS=true with both
#      port-forwards and links
#      docs/reference/infrastructure/infrastructure-manifests.md#lab-prometheus-stack
#  19. Part 2, Step 1 annotates the nodes with an empty
#      nova.openstack.cloud.sap/aggregates after the lifecycle label, and
#      Step 4 shows the hosts of the zone's aggregate
#  20. Part 2, Step 11 chains with `&&`, in the condition of one `if`, the
#      checks that the two Deployment generations, the key generation and
#      the key were read, the patch of the CephClient cinder, the waits for a
#      new, non-empty generation of both Deployments, their rollouts and the
#      check that the key changed; its then branch waits for every node's new
#      digest, echoing a pod whose wait timed out and an empty pod list, and
#      its else branch echoes the failure; then Step 11 live-migrates lab-0
#      and attaches lab-vol to lab-1, in this order. Part 3, Step 1 applies
#      the NFS variant behind a `&&` on the checks of the NFS server and the
#      CSIDriver, in one bash block, before the waits for the NFS entry and
#      the backup Deployment's share; Part 3 sets host, domain and
#      libvirt_pod itself and links no #cp- anchor
#  21. the Caveats link the Lab Ceph of
#      docs/reference/infrastructure/infrastructure-manifests.md#lab-ceph
#  22. Parts 1 and 2 name the Ceph entries of
#      deploy/lab/metal-stack/controlplane/controlplane-lab.yaml: the RBD
#      volume backend with its volume Deployment, pool and libvirt secret UUID,
#      and the backup target with its pool; Part 3 names the NFS volume
#      backend of the JSON patch of
#      deploy/lab/metal-stack/controlplane-nfs/kustomization.yaml with its
#      volume Deployment and its share, and the page names no backend that
#      neither file declares
#
# Heading scans skip fenced code. QUICK_START_DOC overrides the page. Check 22
# is counted as SKIP when yq is not on PATH.
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
# shellcheck source=tests/lib/quick_start_metal_stack.sh
source "$PROJECT_ROOT/tests/lib/quick_start_metal_stack.sh"

QUICK_START_DOC="${QUICK_START_DOC:-$PROJECT_ROOT/docs/quick-start-metal-stack.md}"
VITEPRESS_CONFIG="$PROJECT_ROOT/docs/.vitepress/config.ts"
GUIDE_CONVENTIONS="$PROJECT_ROOT/docs/contributing/guide-conventions.md"
INFRA_MANIFESTS="$PROJECT_ROOT/docs/reference/infrastructure/infrastructure-manifests.md"
GUIDE_SCAFFOLD="$PROJECT_ROOT/.claude/skills/prepare-new-guide/scripts/scaffold-guide.sh"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"
LAB_CONTROLPLANE="$PROJECT_ROOT/deploy/lab/metal-stack/controlplane/controlplane-lab.yaml"
NFS_VARIANT="$PROJECT_ROOT/deploy/lab/metal-stack/controlplane-nfs/kustomization.yaml"

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

# step_section prints the body of `### Step <n>:`, <n> being $2, inside the
# `## ` section whose heading matches the extended regular expression $1, up
# to the next `### `.
step_section() {
  section "$1" |
    awk -v n="$2" '$0 ~ "^### Step " n ":" { inside = 1; next } inside && /^### / { exit } inside { print }'
}

# assert_once_in_order <label> <text> <fixed string>...
# Asserts that each fixed string occurs on exactly one line of <text>, and
# records one PASS or FAIL named <label> for their order: each string on a
# later line than the one before it.
assert_once_in_order() {
  local label="$1" text="$2" string count line previous=0 ordered=1
  shift 2
  for string in "$@"; do
    count="$(grep -cF -- "$string" <<<"$text" || true)"
    assert_eq "'$string' occurs on one line" "1" "$count"
    line="$(grep -nF -- "$string" <<<"$text" | head -n 1 | cut -d: -f1)"
    if [[ -z "$line" ]] || ((line <= previous)); then
      ordered=0
    else
      previous="$line"
    fi
  done
  if [[ "$ordered" -eq 1 ]]; then
    echo "  PASS: $label are in this order"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $label are missing or out of order"
    FAIL=$((FAIL + 1))
  fi
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
  echo "Test: the seven sections occur once each, in order"
  local sections pattern name count line previous=0 ordered=1
  sections="$(headings '##')"
  for pattern in '^## Prerequisites$' '^## Part 1: ' '^## Part 2: ' '^## Part 3: ' '^## Teardown$' '^## Caveats$' '^## Proven by$'; do
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

  echo "Test: the twenty-two step headings carry their anchors"
  local want got
  want="1:cp-clone 2:cp-probe 3:cp-deploy 4:cp-apply 5:cp-tenant 6:cp-access 7:cp-verify 1:hv-nodes 2:hv-fixtures 3:hv-apply 4:hv-onboarding 5:hv-boot 6:hv-console 7:hv-volume 8:hv-migrate 9:hv-evict 10:hv-backup 11:hv-rotate 1:nfs-apply 2:nfs-volume 3:nfs-migrate 4:nfs-backup"
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
  assert_eq "the page holds three code imports" "3" "$found"

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
  local deploy='EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true WITH_CEPH=true make deploy-infra'
  assert_file_contains_fixed "the page holds the deploy command" "$QUICK_START_DOC" "$deploy"
  assert_file_contains_fixed "the page holds the teardown command" "$QUICK_START_DOC" \
    'EXTERNAL_CLUSTER=true make teardown-infra'
  local row
  row="$(grep -F '../quick-start-metal-stack.md' "$GUIDE_CONVENTIONS" | grep '^|' | grep -F "$deploy" || true)"
  assert_not_empty "guide-conventions.md has a devstack row with the page and its deploy command" "$row"

  # The scaffold prints the skeleton to stdout and writes no file.
  local out rc opt_in
  out="$(bash "$GUIDE_SCAFFOLD" probe --devstack quick-start-metal-stack 2>&1)"
  assert_not_empty "the guide scaffold prints the deploy command as the bring-up" \
    "$(grep -xF -- "$deploy" <<<"$out" || true)"
  out="$(bash "$GUIDE_SCAFFOLD" probe --devstack quick-start-metal-stack --opt-in WITH_CEPH=true 2>&1)"
  rc=$?
  assert_eq "the guide scaffold refuses a second WITH_CEPH=true" "2" "$rc"
  assert_contains "the refusal says the bring-up sets it" "$out" \
    "WITH_CEPH=true is already part of the metal-stack bring-up"
  for opt_in in WITH_NFS WITH_CHAOS_MESH WITH_DIZZY WITH_PROMETHEUS; do
    out="$(bash "$GUIDE_SCAFFOLD" probe --devstack quick-start-metal-stack --opt-in "${opt_in}=true" 2>&1)"
    rc=$?
    assert_eq "the guide scaffold accepts ${opt_in}=true, which the lab overlay carries" "0" "$rc"
    assert_eq "and adds it to the metal-stack bring-up" \
      "EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true WITH_CEPH=true ${opt_in}=true make deploy-infra" \
      "$(grep -xF -- "EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true WITH_CEPH=true ${opt_in}=true make deploy-infra" <<<"$out" || true)"
  done
  out="$(bash "$GUIDE_SCAFFOLD" probe --devstack quick-start-metal-stack --opt-in WITH_REGISTRY_CACHE=true 2>&1)"
  rc=$?
  assert_eq "the guide scaffold still refuses the kind-only WITH_REGISTRY_CACHE=true" "2" "$rc"
  assert_contains "the refusal names the flag" "$out" "WITH_REGISTRY_CACHE=true is kind-only"

  # The scaffold keeps its own copy of the refused opt-ins. Every WITH_* flag
  # hack/deploy-infra.sh declares goes through it: refused if and only if
  # preflight_external_cluster refuses it. WITH_CEPH is part of the bring-up and
  # WITH_CONTROLPLANE the devstack, so neither is an opt-in of the page.
  local refused flag expected
  refused="$(awk '/^preflight_external_cluster\(\)/,/; do$/' "$DEPLOY_INFRA_SH" |
    grep -oE '"WITH_[A-Z0-9_]+\|' | tr -d '"|' | paste -sd' ' -)"
  assert_not_empty "preflight_external_cluster refuses kind-only opt-ins" "$refused"
  for flag in $(grep -oE '^WITH_[A-Z0-9_]+=' "$DEPLOY_INFRA_SH" | tr -d = | LC_ALL=C sort -u); do
    case "$flag" in WITH_CEPH | WITH_CONTROLPLANE) continue ;; esac
    expected=0
    if [[ " $refused " == *" $flag "* ]]; then expected=2; fi
    bash "$GUIDE_SCAFFOLD" probe --devstack quick-start-metal-stack --opt-in "${flag}=true" >/dev/null 2>&1
    rc=$?
    assert_eq "the guide scaffold exits ${expected} for ${flag}=true, as hack/deploy-infra.sh refuses it or not" \
      "$expected" "$rc"
  done
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

# count_command <command> <file>
# The number of lines of <file> that hold <command> not followed by "-", so
# the apply of controlplane/ leaves out the apply of a controlplane-<variant>.
count_command() {
  grep -F -- "$1" "$2" | grep -cvF -- "$1-" || true
}

# --- Test 7: one runbook ---
test_one_runbook() {
  echo "Test: the run sequence lives on the page alone"
  local command count
  for command in "${METAL_STACK_RUN_SEQUENCE[@]}"; do
    count="$(count_command "$command" "$QUICK_START_DOC")"
    assert_eq "'$command' occurs on one line of the page" "1" "$count"
    count="$(count_command "$command" "$INFRA_MANIFESTS")"
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
  step="$(step_section '^## Part 1: ' 4)"
  assert_not_empty "Part 1 holds Step 4" "$step"
  assert_not_contains "Step 4 holds no 'Repeat'" "$step" "Repeat"
  assert_not_contains "Step 4 holds no 'until it succeeds'" "$step" "until it succeeds"
}

# --- Test 10: every block passes on its first run ---
test_blocks_pass_on_first_run() {
  echo "Test: the page holds no instruction to run a block again, and the waits that replace them"
  local retries line count
  retries="$(tr '\n' ' ' <"$QUICK_START_DOC" | grep -oE '[Rr]un (it|the [a-z-]+) again' || true)"
  assert_eq "the page holds no 'run it again' or 'run the <block> again'" "" "$retries"

  count="$(grep -cF -- 'kubectl wait --for=create mariadb/openstack-db -n openstack' "$QUICK_START_DOC" || true)"
  assert_eq "the MariaDB create wait occurs on one line" "1" "$count"
  line="$(grep -nF -- 'kubectl wait --for=create mariadb/openstack-db -n openstack' "$QUICK_START_DOC" | head -n 1 | cut -d: -f1)"
  assert_contains "the MariaDB Ready wait follows it on the next line" \
    "$(if [[ -n "$line" ]]; then sed -n "$((line + 1))p" "$QUICK_START_DOC"; fi)" \
    'kubectl wait mariadb/openstack-db -n openstack --for=condition=Ready'

  local page
  page="$(cat "$QUICK_START_DOC")"
  assert_once_in_order "the release wait, the CRD wait and the Hypervisor wait" "$page" \
    'kubectl wait helmrelease/openstack-hypervisor-operator -n openstack --for=condition=Ready' \
    'kubectl wait crd/hypervisors.kvm.cloud.sap --for=condition=Established' \
    'kubectl wait --for=create "hypervisor/${node}"'

  count="$(grep -cF -- 'kubectl apply -k deploy/lab/metal-stack/migration-ports' "$QUICK_START_DOC" || true)"
  assert_eq "the apply of the migration port reservation occurs on one line" "1" "$count"
  line="$(grep -nF -- 'kubectl apply -k deploy/lab/metal-stack/migration-ports' "$QUICK_START_DOC" | head -n 1 | cut -d: -f1)"
  assert_contains "the wait for its rollout follows it on the next line" \
    "$(if [[ -n "$line" ]]; then sed -n "$((line + 1))p" "$QUICK_START_DOC"; fi)" \
    'kubectl rollout status daemonset/migration-port-reservation -n hypervisor-system'
  assert_eq "the node port check is the line after that" "hack/lab-node-ports.sh" \
    "$(if [[ -n "$line" ]]; then sed -n "$((line + 2))p" "$QUICK_START_DOC"; fi)"

  # A volume or backup command returns before its object reaches the status
  # the next command needs, so a bounded wait for that status follows it. The
  # backup create comes after the detach, because Cinder backs up an in-use
  # volume only with --force.
  # Each entry is a command and, after the `|`, the status its wait reaches.
  local part2=(
    'openstack volume create --size 1 lab-vol|available'
    'openstack server add volume lab-0 lab-vol|in-use'
    'openstack server remove volume lab-0 lab-vol|available'
    'openstack volume backup create --name lab-bk lab-vol|available'
    'openstack volume create --size 1 lab-restored|available'
    'openstack volume backup restore --force lab-bk lab-restored|available'
    'openstack server add volume lab-0 lab-restored|in-use'
    'openstack server add volume lab-1 lab-vol|in-use'
  )
  local part3=(
    'openstack volume create --type nfs --size 1 lab-nfs|available'
    'openstack server add volume lab-0 lab-nfs|in-use'
    'openstack server remove volume lab-0 lab-nfs|available'
    'openstack volume backup create --name lab-nfs-bk lab-nfs|available'
  )
  assert_once_in_order "the volume, backup and attach commands of Part 2" "$page" \
    "${part2[@]%|*}"
  assert_once_in_order "the volume, backup and attach commands of Part 3" "$page" \
    "${part3[@]%|*}"
  local pair command status next
  for pair in "${part2[@]}" "${part3[@]}"; do
    command="${pair%|*}"
    status="${pair##*|}"
    line="$(grep -nF -- "$command" "$QUICK_START_DOC" | head -n 1 | cut -d: -f1)"
    next="$(if [[ -n "$line" ]]; then sed -n "$((line + 1))p" "$QUICK_START_DOC"; fi)"
    assert_starts_with "the line after '$command' is a timeout wait" "$next" "timeout "
    assert_contains "the wait after '$command' waits for '$status'" "$next" "$status"
  done
}

# --- Test 11: no hand steps ---
# The hvo image creates each Hypervisor with spec.highAvailability false and
# sets TraitsUpdated without a custom trait, and the NovaCompute pool maps its
# hosts, so the page runs none of the three by hand, and no script of the e2e
# suites.
test_no_hand_steps() {
  echo "Test: the page holds no hand step the operators took over"
  local pattern lines
  for pattern in 'highAvailability":false' 'custom-traits' 'trait create' 'discover-hosts' 'tests/'; do
    lines="$(grep -nF -- "$pattern" "$QUICK_START_DOC" || true)"
    assert_eq "no line of the page contains '$pattern'" "" "$lines"
  done
}

# --- Test 12: the Prerequisites pin no value of one shoot ---
# The lab overlay names no storage class and hvo reads the catalog's internal
# endpoints, so the page asks for no class name and no service address, and
# every row names a check that runs before the step that needs it.
test_prerequisites_name_no_pinned_value() {
  echo "Test: the Prerequisites name no value only one shoot has"
  local body needle
  body="$(section '^## Prerequisites$')"
  assert_not_empty "the page holds the Prerequisites" "$body"
  for needle in 'premium' '10.248.' 'nothing checks it' 'already allocated'; do
    assert_not_contains "the Prerequisites do not contain '$needle'" "$body" "$needle"
  done
}

# --- Test 13: the teardown needs no hand deletes in OpenStack ---
# hack/teardown-infra.sh deletes the servers, volume backups, snapshots and
# volumes of every project itself, in step 0 before the NovaComputes, so the
# Teardown's bash block is the one make command and no `openstack` command
# precedes it. The section's prose names every backup and volume the page's
# code creates, so a reader knows what goes. The names are read from the
# creates in the page's code, so a backup or volume a Part adds without a
# mention fails the check, and so does a create whose name the check cannot
# read. A create counts with a global option before `volume`, and a name ends
# at a space or the line end, so a name with another character is not read as
# its prefix.

# assert_prose_names_creates <object> <prose> <creates> <regex>
# Asserts that <creates> holds a line, that the extended regular expression
# <regex> reads a name, its last field, from each line, and that <prose>, the
# Teardown section, names it in backticks.
assert_prose_names_creates() {
  local object="$1" prose="$2" creates="$3" regex="$4" create name
  assert_not_empty "the page creates a $object" "$creates"
  while IFS= read -r create; do
    [[ -n "$create" ]] || continue
    name="$(grep -oE -- "$regex" <<<"$create" | awk '{ print $NF }' || true)"
    assert_not_empty "the check reads the $object name of '$create'" "$name"
    [[ -z "$name" ]] || assert_contains "the Teardown names the $object $name" "$prose" "\`$name\`"
  done <<<"$creates"
}

test_teardown_order() {
  echo "Test: the Teardown block is the make command alone, and its prose names what the teardown deletes"
  local prose block
  prose="$(section '^## Teardown$')"
  block="$(awk '/^```bash[[:space:]]*$/ { inside = 1; next } inside && /^```/ { exit } inside { print }' <<<"$prose")"
  assert_eq "the Teardown's bash block is the teardown command alone" \
    'EXTERNAL_CLUSTER=true make teardown-infra' "$block"
  assert_not_contains "the section runs no openstack command by hand" \
    "$(awk '/^[[:space:]]*(```|~~~)/ { fenced = !fenced; next } fenced { print }' <<<"$prose")" 'openstack '
  assert_contains "the prose says the teardown deletes the servers, backups, snapshots and volumes" "$prose" \
    'the servers of every
project, then the volume backups, the volume snapshots and the volumes'

  local code
  code="$(awk '/^[[:space:]]*(```|~~~)/ { fenced = !fenced; next } fenced { print }' "$QUICK_START_DOC")"
  assert_prose_names_creates backup "$prose" "$(grep -E 'volume backup create( |$)' <<<"$code" || true)" \
    'volume backup create( --[a-z-]+)* --name [a-z0-9][a-z0-9-]*( |$)'
  assert_prose_names_creates volume "$prose" "$(grep -E 'volume create( |$)' <<<"$code" || true)" \
    'volume create( --(size|type|image|snapshot|source|description|availability-zone) [^ ]+| --(bootable|non-bootable|read-only|read-write))* [a-z0-9][a-z0-9-]*( |$)'
}

# --- Test 14: no shell array indexed by number ---
# bash counts an array index from 0 and zsh from 1, so `${nodes[0]}` names the
# first node in bash and nothing in zsh, and `${nodes[1]}` a different node in
# each. A command takes one node by slice, `${nodes[@]:0:1}`, which both shells
# count from 0, and the prose names no node by its index.

# assert_no_array_index <label> <text>
assert_no_array_index() {
  local label="$1" text="$2" lines
  lines="$(grep -nE '\$\{[A-Za-z_][A-Za-z0-9_]*\[[0-9]+\]\}' <<<"$text" || true)"
  assert_eq "no line of $label indexes a shell array by number" "" "$lines"
  lines="$(grep -nE 'nodes\[[0-9]+\]' <<<"$text" || true)"
  assert_eq "no line of $label names a node as nodes[<n>]" "" "$lines"
}

# checks_section prints the "Checks outside the quick start" section of
# infrastructure-manifests.md, fenced code included, up to the next heading.
checks_section() {
  awk '
    /^[[:space:]]*(```|~~~)/ { fenced = !fenced }
    !fenced && /^#+ / { if (inside) exit; if ($0 == "#### Checks outside the quick start") { inside = 1; next } }
    inside { print }
  ' "$INFRA_MANIFESTS"
}

test_no_array_index() {
  echo "Test: no shell array indexed by number"
  local checks
  assert_no_array_index "the page" "$(cat "$QUICK_START_DOC")"
  checks="$(checks_section)"
  assert_not_empty "infrastructure-manifests.md holds the Checks outside the quick start" "$checks"
  assert_no_array_index "the Checks outside the quick start" "$checks"
}

# --- Test 15: Step 5 boots a server on every node ---
# A server create per node taken by slice leaves every node past the ones it
# names without a server, so Step 5 creates the servers in a loop over nodes.
test_step_5_boots_every_node() {
  echo "Test: Part 2, Step 5 boots a server on every node"
  local step
  step="$(step_section '^## Part 2: ' 5)"
  assert_not_empty "Part 2 holds Step 5" "$step"
  assert_once_in_order "the loop over nodes, the server create and its availability zone" "$step" \
    'for node in "${nodes[@]}"; do' \
    'openstack server create "lab-${i}"' \
    '--availability-zone "${zone}:${node}"'
}

# --- Test 16: the caveats rest on the lab fault runs ---
# The NFS outage and the hypervisor pod kills ran on the lab, so the Caveats
# link their record and do not call the outage untested. The section is
# joined into one line, so a sentence wrapped differently still counts.
test_caveats_name_the_fault_runs() {
  echo "Test: the Caveats link the lab fault runs"
  local body
  body="$(section '^## Caveats$')"
  assert_not_empty "the page holds the Caveats" "$body"
  assert_contains "the Caveats link infrastructure-manifests.md#lab-fault-runs" "$body" \
    'infrastructure-manifests.md#lab-fault-runs'
  assert_not_contains "the Caveats do not say 'no lab run has tested that'" \
    "$(tr -s '\n ' ' ' <<<"$body")" 'no lab run has tested that'
}

# --- Test 17: Step 3 names the platform's autoscalers ---
# The shoot runs a VPA and a metrics-server, so Step 3 links what a workload
# opts into instead of calling WITH_VPA a flag that needs a kind node. The
# page is joined into one line, so a sentence wrapped differently still
# counts.
test_step_3_names_the_platform_autoscalers() {
  echo "Test: Part 1, Step 3 links the platform's autoscalers"
  local step
  step="$(step_section '^## Part 1: ' 3)"
  assert_not_empty "Part 1 holds Step 3" "$step"
  assert_contains "Step 3 links infrastructure-manifests.md#lab-autoscaling" "$step" \
    'infrastructure-manifests.md#lab-autoscaling'
  assert_not_contains "the page does not say 'the other flags that need a kind node'" \
    "$(tr -s '\n ' ' ' <"$QUICK_START_DOC")" 'the other flags that need a kind node'
}

# --- Test 18: Step 3 names the optional Prometheus stack ---
# shellcheck disable=SC2016 # the page's literal text, not expanded here
test_step_3_names_the_lab_prometheus() {
  echo "Test: Part 1, Step 3 names WITH_PROMETHEUS=true and links the Lab Prometheus stack"
  local step
  step="$(step_section '^## Part 1: ' 3)"
  assert_contains "Step 3 names WITH_PROMETHEUS=true" "$step" '`WITH_PROMETHEUS=true`'
  assert_contains "and the Prometheus port-forward" "$step" \
    '`kubectl -n monitoring port-forward svc/kube-prometheus-stack-prometheus 9090:9090`'
  assert_contains "and the Grafana port-forward" "$step" \
    '`kubectl -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80`'
  assert_contains "Step 3 links infrastructure-manifests.md#lab-prometheus-stack" "$step" \
    'infrastructure-manifests.md#lab-prometheus-stack'
  # The deploy command of the page stays the one without the flag.
  assert_eq "no fenced line of the page sets WITH_PROMETHEUS" "0" \
    "$(awk '/^```/ { f = !f; next } f' "$QUICK_START_DOC" | grep -c 'WITH_PROMETHEUS' || true)"
}

# --- Test 19: the nodes stay in their zone's aggregate ---
# openstack-hypervisor-operator keeps a host only in the aggregates the
# annotation names plus the zone's once onboarding ends, so without it the
# zone has no host and the zone:host boots of Step 5 fail (#1314).
# shellcheck disable=SC2016 # the page's literal text, not expanded here
test_part_2_keeps_the_zone_aggregate() {
  echo "Test: Part 2 annotates the nodes and shows the zone aggregate's hosts"
  local step
  step="$(step_section '^## Part 2: ' 1)"
  assert_not_empty "Part 2 holds Step 1" "$step"
  assert_once_in_order "the lifecycle label and the aggregates annotation" "$step" \
    'cobaltcore.cloud.sap/node-hypervisor-lifecycle=skip-tests' \
    'kubectl annotate node --all nova.openstack.cloud.sap/aggregates='
  step="$(step_section '^## Part 2: ' 4)"
  assert_not_empty "Part 2 holds Step 4" "$step"
  assert_contains "Step 4 shows the hosts of the zone's aggregate" "$step" \
    'openstack aggregate show "${zone}" -c hosts -f value'
}

# --- Test 20: the rotation of Part 2 and the order of Part 3 ---
# The rotation of Part 2, Step 11 chains its reads, the patch, the waits, the
# rollouts and a check that the key changed in the condition of one `if`,
# because a skipped patch or a failed wait leaves the old key, whose digest
# every node logged at its start and the digest loop would match. Its waits
# end only on a new generation they read, because an empty read differs from
# every generation. It waits for both Deployments and every node's libvirt
# secret before the migration and the attach that prove the new key. The
# digest loop echoes a pod whose wait timed out and an empty pod list,
# because the migration fails on a node whose libvirt secret still holds the
# old key. The NFS variant of Part 3 puts nfs1 into the backup pod as well,
# whose inline volume waits for a share nothing serves on a lab without the
# NFS stack, so a pasted block applies it only behind the checks of the NFS
# server and the CSIDriver. Part 2 moved lab-0 and left domain on lab-1, so
# Part 3 reads host, domain and libvirt_pod itself.
# shellcheck disable=SC2016 # the page's literal text, not expanded here
test_rotation_and_part_3_order() {
  echo "Test: Part 2 rotates before the migration, and Part 3 checks the NFS stack before the apply"
  local body step block
  step="$(step_section '^## Part 2: ' 11)"
  assert_not_empty "Part 2 holds Step 11" "$step"
  assert_once_in_order "the guarded patch, the generation waits, the rollout, the key check, the digest wait, the failure, the migration and the attach" "$step" \
    'old_key=$(kubectl -n openstack get secret ceph-client-cinder' \
    'if [ -n "${volume_generation}" ] && [ -n "${backup_generation}" ] && [ -n "${key_generation}" ] && [ -n "${old_key}" ] &&' \
    'kubectl -n rook-ceph patch cephclient cinder --type merge' \
    'controlplane-cinder-volume-ceph1 -o jsonpath="{.metadata.generation}") && [ -n "${g}" ] && [ "${g}" != "${volume_generation}" ]' \
    'controlplane-cinder-backup -o jsonpath="{.metadata.generation}") && [ -n "${g}" ] && [ "${g}" != "${backup_generation}" ]' \
    'kubectl -n openstack rollout status deploy/controlplane-cinder-volume-ceph1' \
    '[ -n "${key}" ] && [ "${key}" != "${old_key}" ]; then' \
    "grep -qF '(sha256 \${digest})'" \
    "echo 'a read, the patch, a wait or the key check failed: repeat the block'" \
    'openstack server migrate --live-migration --wait lab-0' \
    'openstack server add volume lab-1 lab-vol'
  # Each line of the rotation's if statement, prefixed with its part: c for
  # the condition up to "; then", t for the then branch, e for the else branch.
  local parts condition then_branch
  parts="$(awk '
    /^if / { part = "c" }
    part == "t" && /^else$/ { part = "e"; next }
    part != "" && /^fi$/ { exit }
    part != "" { print part ":" $0 }
    part == "c" && /; then$/ { part = "t" }
  ' <<<"$step")"
  condition="$(sed -n 's/^c://p' <<<"$parts")"
  assert_not_empty "Step 11 holds the rotation's if statement" "$condition"
  assert_eq "every line of the rotation's condition ends in '&&', a line continuation or '; then'" "" \
    "$(grep -vE '(&&|\\|; then)$' <<<"$condition" || true)"
  then_branch="$(sed -n 's/^t://p' <<<"$parts")"
  assert_contains "the then branch waits for every node's new digest" "$then_branch" \
    "grep -qF '(sha256 \${digest})'"
  assert_eq "the digest wait ends in '||'" "||" \
    "$(grep -F "grep -qF '(sha256 \${digest})'" <<<"$then_branch" | awk '{ print $NF }')"
  assert_contains "the then branch names a pod whose digest wait timed out" "$then_branch" \
    'echo "${pod} did not log the new digest within 300 s: repeat the loop before the migration"'
  assert_contains "the then branch names an empty pod list" "$then_branch" \
    "[ -n \"\${listed}\" ] || echo 'no libvirt pod was listed: repeat the loop before the migration'"
  assert_contains "the else branch names the failure" "$(sed -n 's/^e://p' <<<"$parts")" \
    "echo 'a read, the patch, a wait or the key check failed: repeat the block'"

  body="$(section '^## Part 3: ')"
  assert_not_empty "the page holds Part 3" "$body"
  step="$(step_section '^## Part 3: ' 1)"
  assert_not_empty "Part 3 holds Step 1" "$step"
  assert_once_in_order "the checks, the apply and the waits" "$step" \
    'kubectl -n openstack rollout status deployment/nfs-server' \
    'kubectl apply -k deploy/lab/metal-stack/controlplane-nfs' \
    'kubectl wait --for=create cinderbackend/nfs1' \
    '--for=jsonpath='"'"'{.spec.template.spec.volumes[?(@.name=="share-nfs1")].csi.driver}'"'"'=nfs.csi.k8s.io' \
    'kubectl -n openstack rollout status deploy/controlplane-cinder-backup'
  block="$(awk '
    /^```bash[[:space:]]*$/ { inside = 1; text = ""; next }
    inside && /^```/ { if (text ~ /controlplane-nfs/) { printf "%s", text; exit } inside = 0; next }
    inside { text = text $0 "\n" }
  ' <<<"$step")"
  assert_eq "the checks of the NFS stack and the apply chained to them form one block" \
    "$(printf '%s\n' \
      'kubectl -n openstack rollout status deployment/nfs-server --timeout=60s &&' \
      '  kubectl get csidriver nfs.csi.k8s.io &&' \
      '  kubectl apply -k deploy/lab/metal-stack/controlplane-nfs')" \
    "$block"
  assert_not_empty "Part 3 defines host=" "$(grep -E '^host=' <<<"$body" || true)"
  assert_not_empty "Part 3 defines domain=" "$(grep -E '^domain=' <<<"$body" || true)"
  assert_not_empty "Part 3 defines libvirt_pod=" "$(grep -E '^libvirt_pod=' <<<"$body" || true)"
  assert_not_contains "Part 3 holds no link to a #cp- anchor" "$body" "](#cp-"
}

# --- Test 21: the caveats name the lab Ceph ---
test_caveats_name_the_lab_ceph() {
  echo "Test: the Caveats link the Lab Ceph"
  local body
  body="$(section '^## Caveats$')"
  assert_not_empty "the page holds the Caveats" "$body"
  assert_contains "the Caveats link infrastructure-manifests.md#lab-ceph" "$body" \
    'infrastructure-manifests.md#lab-ceph'
}

# --- Test 22: the page names the lab's backends ---
# Parts 1 and 2 repeat the names and values of the Ceph entries of the lab
# ControlPlane, and Part 3 those of the NFS variant's JSON patch, so a rename
# there fails here instead of leaving the page on a backend that no longer
# exists. A name the page uses for a backend that neither file declares
# fails too.

# lab_field <entry> <expression>
# <expression> on the Cinder volume backend or backup target named <entry> of
# the lab ControlPlane, empty when there is none.
lab_field() {
  yq -r "select(.kind == \"ControlPlane\") | .spec.services.cinder | (.backends // []) + [.backupBackend] | .[] | select(.name == \"$1\") | .$2 // \"\"" \
    "$LAB_CONTROLPLANE" 2>/dev/null
}

test_page_names_the_backends() {
  echo "Test: the page names the backends of the lab ControlPlane and its NFS variant"
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (13 checks skipped)"
    SKIP=$((SKIP + 13))
    return
  fi
  local name pool uuid secret backup backup_pool backup_secret value body patch nfs nfs_share
  name="$(yq -r 'select(.kind == "ControlPlane") | .spec.services.cinder.backends[0].name // ""' "$LAB_CONTROLPLANE" 2>/dev/null)"
  pool="$(lab_field "$name" rbd.pool)"
  uuid="$(lab_field "$name" rbd.secretUUID)"
  secret="$(lab_field "$name" rbd.keySecretRef.name)"
  backup="$(yq -r 'select(.kind == "ControlPlane") | .spec.services.cinder.backupBackend.name // ""' "$LAB_CONTROLPLANE" 2>/dev/null)"
  backup_pool="$(lab_field "$backup" rbd.pool)"
  backup_secret="$(lab_field "$backup" rbd.keySecretRef.name)"
  patch="$(yq -r '.patches[] | select(.target.kind == "ControlPlane") | .patch // ""' "$NFS_VARIANT" 2>/dev/null)"
  nfs="$(yq -r '.[] | select(.path == "/spec/services/cinder/backends/-" and .value.type == "NFS") | .value.name // ""' - <<<"$patch" 2>/dev/null)"
  nfs_share="$(yq -r '.[] | select(.path == "/spec/services/cinder/backends/-" and .value.type == "NFS") | .value.nfs.server + ":" + .value.nfs.path' - <<<"$patch" 2>/dev/null)"
  for value in "$name" "$pool" "$uuid" "$secret" "$backup" "$backup_pool" "$backup_secret" "$nfs" "$nfs_share"; do
    if [[ -z "$value" || "$value" == ":" ]]; then
      echo "  FAIL: could not read the backends from ${LAB_CONTROLPLANE#"$PROJECT_ROOT"/} and ${NFS_VARIANT#"$PROJECT_ROOT"/}"
      FAIL=$((FAIL + 13))
      return
    fi
  done
  body="$(section '^## Part 1: '; section '^## Part 2: ')"
  assert_contains "Parts 1 and 2 wait for a pool of $name" "$body" "@$name#"
  assert_contains "Parts 1 and 2 name the volume service host of $name" "$body" "controlplane-cinder@$name"
  assert_contains "Parts 1 and 2 roll the volume Deployment of $name" "$body" "controlplane-cinder-volume-$name"
  assert_contains "Parts 1 and 2 read the image in the pool $pool" "$body" "\"$pool/volume-"
  assert_contains "Parts 1 and 2 list the pool $backup_pool" "$body" "rbd ls $backup_pool"
  assert_contains "Parts 1 and 2 name the libvirt secret $uuid" "$body" "uuid='$uuid'"
  assert_contains "Parts 1 and 2 name the backup target $backup" "$body" "\`$backup\`"
  assert_contains "Parts 1 and 2 read the Secret $secret" "$body" "get secret ceph-client-cinder"
  body="$(section '^## Part 3: ')"
  assert_contains "Part 3 creates the volume type on $nfs" "$body" "volume_backend_name=$nfs"
  assert_contains "Part 3 waits for the CinderBackend $nfs" "$body" "cinderbackend/$nfs"
  assert_contains "Part 3 reads the volume Deployment of $nfs" "$body" "controlplane-cinder-volume-$nfs"
  assert_contains "Part 3 names the share $nfs_share" "$body" "\`${nfs_share}\`"
  # Backends the page names by their volume Deployment, their CinderBackend or
  # a host of the volume service; each must be one of the two files'.
  local named
  named="$(grep -oE 'controlplane-cinder-volume-[a-z0-9-]+|cinderbackend/[a-z0-9-]+|controlplane-cinder@[a-z0-9-]+' "$QUICK_START_DOC" |
    sed -E 's/^(controlplane-cinder-volume-|cinderbackend\/|controlplane-cinder@)//' | sort -u | paste -sd' ' -)"
  assert_eq "the page names the volume backends $name and $nfs alone" \
    "$(printf '%s\n' "$name" "$nfs" | sort -u | paste -sd' ' -)" "$named"
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
test_blocks_pass_on_first_run
test_no_hand_steps
test_prerequisites_name_no_pinned_value
test_teardown_order
test_no_array_index
test_step_5_boots_every_node
test_caveats_name_the_fault_runs
test_step_3_names_the_platform_autoscalers
test_step_3_names_the_lab_prometheus
test_part_2_keeps_the_zone_aggregate
test_rotation_and_part_3_order
test_caveats_name_the_lab_ceph
test_page_names_the_backends

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
