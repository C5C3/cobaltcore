#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify docs/quick-start-metal-stack.md, the lab's one runbook:
#   1. the frontmatter holds `title: Quick Start (metal-stack)` and no other
#      key, and the SPDX comment is present
#   2. docs/.vitepress/config.ts lists the page after the ControlPlane quick
#      start, in the Getting Started group
#   3. the six `## ` sections occur once each and in order, and the seventeen
#      `### Step` headings carry their explicit anchors
#   4. every directory the page applies holds a kustomization.yaml, every
#      deploy/, hack/ or tests/ file it names exists and each such script is
#      executable, both code imports resolve, and every #anchor it links
#      resolves on the target page
#   5. the page holds the deploy and teardown commands, the guide
#      conventions list it as a devstack with the same deploy command, and
#      the guide scaffold prints that command as the bring-up, refuses a
#      second `--opt-in WITH_NFS=true` and a kind-only `--opt-in
#      WITH_DIZZY=true`, and accepts `--opt-in WITH_CHAOS_MESH=true`, which
#      the lab overlay carries
#   6. Part 2 links no anchor of Part 1 and defines `nodes` and `zone` itself
#   7. four commands of the run sequence occur once on the page and not in
#      docs/reference/infrastructure/infrastructure-manifests.md, which links
#      the page
#   8. Proven by names the date of a lab run and no chainsaw suite
#   9. Part 1, Step 4 holds no instruction to repeat the apply: deploy-infra
#      returns only once the cluster admits it
#  10. the page holds no instruction to run a block again: the MariaDB wait of
#      Part 1, Step 5 follows a create wait, the Hypervisor loop of Part 2,
#      Step 3 follows the waits for the operator's release and its CRD, and
#      the node port check of Part 2, Step 1 follows the reservation of the
#      migration ports and its rollout, and the volume create, the attach, the
#      detach and the backup create of Part 2 occur once each and in this
#      order, each followed on the next line by a `timeout` wait for the
#      status it reaches
#  11. no line of the page holds a hand step the operators took over: the
#      highAvailability patch, the custom trait (its annotation and its
#      create) and the host discovery, nor any other file under tests/
#  12. the Prerequisites name no value only one shoot has: no class
#      `premium`, no address of the 10.248. service network, and no row whose
#      failure shows only later (`nothing checks it`, `already allocated`)
#  13. the bash block of the Teardown deletes the backup, the servers and the
#      volume once each and in this order, and runs `make teardown-infra`
#      after them
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
GUIDE_SCAFFOLD="$PROJECT_ROOT/.claude/skills/prepare-new-guide/scripts/scaffold-guide.sh"

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

  echo "Test: the seventeen step headings carry their anchors"
  local want got
  want="1:cp-clone 2:cp-probe 3:cp-deploy 4:cp-apply 5:cp-tenant 6:cp-access 7:cp-verify 1:hv-nodes 2:hv-fixtures 3:hv-apply 4:hv-onboarding 5:hv-boot 6:hv-console 7:hv-volume 8:hv-migrate 9:hv-evict 10:hv-backup"
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
  local deploy='EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true WITH_NFS=true make deploy-infra'
  assert_file_contains_fixed "the page holds the deploy command" "$QUICK_START_DOC" "$deploy"
  assert_file_contains_fixed "the page holds the teardown command" "$QUICK_START_DOC" \
    'EXTERNAL_CLUSTER=true make teardown-infra'
  local row
  row="$(grep -F '../quick-start-metal-stack.md' "$GUIDE_CONVENTIONS" | grep '^|' | grep -F "$deploy" || true)"
  assert_not_empty "guide-conventions.md has a devstack row with the page and its deploy command" "$row"

  # The scaffold prints the skeleton to stdout and writes no file.
  local out rc
  out="$(bash "$GUIDE_SCAFFOLD" probe --devstack quick-start-metal-stack 2>&1)"
  assert_not_empty "the guide scaffold prints the deploy command as the bring-up" \
    "$(grep -xF -- "$deploy" <<<"$out" || true)"
  out="$(bash "$GUIDE_SCAFFOLD" probe --devstack quick-start-metal-stack --opt-in WITH_NFS=true 2>&1)"
  rc=$?
  assert_eq "the guide scaffold refuses a second WITH_NFS=true" "2" "$rc"
  assert_contains "the refusal says the bring-up sets it" "$out" \
    "WITH_NFS=true is already part of the metal-stack bring-up"
  out="$(bash "$GUIDE_SCAFFOLD" probe --devstack quick-start-metal-stack --opt-in WITH_CHAOS_MESH=true 2>&1)"
  rc=$?
  assert_eq "the guide scaffold accepts WITH_CHAOS_MESH=true, which the lab overlay carries" "0" "$rc"
  assert_not_empty "and adds it to the bring-up" \
    "$(grep -xF -- 'EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true WITH_NFS=true WITH_CHAOS_MESH=true make deploy-infra' <<<"$out" || true)"
  out="$(bash "$GUIDE_SCAFFOLD" probe --devstack quick-start-metal-stack --opt-in WITH_DIZZY=true 2>&1)"
  rc=$?
  assert_eq "the guide scaffold still refuses the kind-only WITH_DIZZY=true" "2" "$rc"
  assert_contains "the refusal names the flag" "$out" "WITH_DIZZY=true is kind-only"
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
    'server create "lab-${i}"' \
    'server add volume lab-0 lab-vol'; do
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
  assert_once_in_order "the volume create, the attach, the detach and the backup create" "$page" \
    'openstack volume create --size 1 lab-vol' \
    'openstack server add volume lab-0 lab-vol' \
    'openstack server remove volume lab-0 lab-vol' \
    'openstack volume backup create --name lab-bk lab-vol'
  local pair command status next
  for pair in \
    'openstack volume create --size 1 lab-vol|available' \
    'openstack server add volume lab-0 lab-vol|in-use' \
    'openstack server remove volume lab-0 lab-vol|available' \
    'openstack volume backup create --name lab-bk lab-vol|available'; do
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

# --- Test 13: the teardown deletes the OpenStack objects before the stack ---
# The page deletes what it created while the APIs still answer: the backup
# first, then the servers before the volume, because deleting a server
# detaches the volume a run that stopped before Part 2, Step 10 left attached,
# and Cinder refuses to delete an attached volume. All of them go before the
# stack, whose NovaCompute pool keeps its finalizer while a server is left.
test_teardown_order() {
  echo "Test: the Teardown block deletes the backup, the servers and the volume before the stack"
  local block
  block="$(section '^## Teardown$' |
    awk '/^```bash[[:space:]]*$/ { inside = 1; next } inside && /^```/ { exit } inside { print }')"
  assert_not_empty "the Teardown section holds a bash block" "$block"
  assert_once_in_order "the backup, server and volume deletes and the stack teardown" "$block" \
    'volume backup delete lab-bk' \
    "server delete --wait \$(openstack server list --name '^lab-[0-9]+\$'" \
    'volume delete lab-vol' \
    'make teardown-infra'
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
  step="$(section '^## Part 2: ' |
    awk '/^### Step 5:/ { inside = 1; next } inside && /^### / { exit } inside { print }')"
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
  step="$(section '^## Part 1: ' |
    awk '/^### Step 3:/ { inside = 1; next } inside && /^### / { exit } inside { print }')"
  assert_not_empty "Part 1 holds Step 3" "$step"
  assert_contains "Step 3 links infrastructure-manifests.md#lab-autoscaling" "$step" \
    'infrastructure-manifests.md#lab-autoscaling'
  assert_not_contains "the page does not say 'the other flags that need a kind node'" \
    "$(tr -s '\n ' ' ' <"$QUICK_START_DOC")" 'the other flags that need a kind node'
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

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
