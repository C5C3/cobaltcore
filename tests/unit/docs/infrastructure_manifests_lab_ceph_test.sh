#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the `### Lab Ceph` subsection of
# docs/reference/infrastructure/infrastructure-manifests.md (#1344):
#   1. the heading occurs once, directly after `### Lab NFS stack`, and the
#      next heading is `### Lab Chaos Mesh`
#   2. its Files line names every file of deploy/lab/metal-stack/ceph and
#      ceph/cluster, read from the directories, and no other
#   3. the deploy command, the toolbox commands, the keyGeneration patch and
#      the teardown command are on the page, each toolbox command a whole line
#      once its leading whitespace is stripped; the two commands that read
#      the client key print its digest, never the key
#   4. each name comes from its source: the chart version of release.yaml,
#      the Ceph image of cluster.yaml, the toolbox Deployment, the OpenBao
#      paths and roles of keys-push.yaml and keys-pull.yaml, CEPH_TIMEOUT and
#      the three log lines of hack/deploy-infra.sh
#   5. the teardown is six numbered sub-steps in the order of teardown_ceph
#      in hack/teardown-infra.sh: the CephCluster is marked for deletion
#      before its dependents are deleted, then the cleanup Jobs, the claims
#      and the operator overlay
#
# Subsections run from their heading to the next heading of any level,
# fenced code included. INFRA_MANIFESTS_DOC overrides the page.
#
# Usage: bash tests/unit/docs/infrastructure_manifests_lab_ceph_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

DOC="${INFRA_MANIFESTS_DOC:-$PROJECT_ROOT/docs/reference/infrastructure/infrastructure-manifests.md}"
CEPH_DIR="$PROJECT_ROOT/deploy/lab/metal-stack/ceph"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"
TEARDOWN_SH="$PROJECT_ROOT/hack/teardown-infra.sh"

if [[ ! -f "$DOC" ]]; then
  echo "FAIL: $DOC does not exist"
  exit 1
fi

# headings prints "<line>:<text>" for every heading outside fenced code.
headings() {
  awk '
    /^[[:space:]]*(```|~~~)/ { fenced = !fenced; next }
    !fenced && /^#+ / { print NR ":" $0 }
  ' "$DOC"
}

# subsection prints the lines below the heading $1, fenced code included, up
# to the next heading of any level outside fenced code.
subsection() {
  awk -v heading="$1" '
    /^[[:space:]]*(```|~~~)/ { fenced = !fenced }
    !fenced && /^#+ / { if (inside) exit; if ($0 == heading) { inside = 1; next } }
    inside { print }
  ' "$DOC"
}

# count_lines <text> <line> prints how many lines of <text> are <line> once
# their leading whitespace is stripped.
count_lines() {
  want="$2" awk '{ sub(/^[[:space:]]+/, "") } $0 == ENVIRON["want"] { n++ } END { print n + 0 }' <<<"$1"
}

# first_line <file> <fixed string> prints the number of the first line of
# <file> that holds <fixed string>, empty when none does.
first_line() {
  grep -nF -- "$2" "$1" | head -n 1 | cut -d: -f1
}

SECTION="$(subsection '### Lab Ceph')"
# The numbered list of the teardown, one line per sub-step.
TEARDOWN="$(awk '/^\*\*Teardown\.\*\*/ { f = 1 } f && /^\*\*What the pods change/ { exit } f' <<<"$SECTION" |
  awk '/^[0-9]+\. / { if (item != "") print item; item = $0; next } item != "" && /^   / { sub(/^ +/, " "); item = item $0 } END { if (item != "") print item }')"

# --- Test 1: the place ---
test_position() {
  echo "Test: '### Lab Ceph' sits once between '### Lab NFS stack' and '### Lab Chaos Mesh'"
  local all before after
  all="$(headings)"
  assert_eq "'### Lab Ceph' occurs once" "1" "$(grep -cxE '[0-9]+:### Lab Ceph' <<<"$all" || true)"
  before="$(grep -B1 -xE '[0-9]+:### Lab Ceph' <<<"$all" | head -n 1 | sed 's/^[0-9]*://')"
  after="$(grep -A1 -xE '[0-9]+:### Lab Ceph' <<<"$all" | tail -n 1 | sed 's/^[0-9]*://')"
  assert_eq "the heading before it is '### Lab NFS stack'" "### Lab NFS stack" "$before"
  assert_eq "the heading after it is '### Lab Chaos Mesh'" "### Lab Chaos Mesh" "$after"
}

# --- Test 2: the files ---
# shellcheck disable=SC2016 # the page's literal backticks, not expanded here
test_files() {
  echo "Test: the Files line names every file of ceph/ and ceph/cluster/ and no other"
  local files named
  files="$(cd "$PROJECT_ROOT" && find deploy/lab/metal-stack/ceph -type f -name '*.yaml' | sort)"
  named="$(awk '/^\*\*Files:\*\*/ { f = 1 } f && /^$/ { exit } f' <<<"$SECTION" |
    grep -oE '`deploy/lab/metal-stack/ceph/[^`]+`' | tr -d '`' | sort)"
  assert_not_empty "the overlay has files" "$files"
  assert_eq "the Files line names exactly the overlay's files" "$files" "$named"
}

# --- Test 3: the commands ---
# shellcheck disable=SC2016,SC1003 # the page's literal lines, not expanded here
test_commands() {
  echo "Test: the section holds the deploy, toolbox, rotation and teardown commands, and never prints a key"
  local line
  assert_contains "the deploy command" "$SECTION" \
    'EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true WITH_NFS=true WITH_CEPH=true make deploy-infra'
  for line in \
    'kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph status' \
    'kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph osd pool ls' \
    'kubectl -n rook-ceph exec deploy/rook-ceph-tools -- rbd create volumes/lab-probe --size 1G' \
    'kubectl -n rook-ceph exec deploy/rook-ceph-tools -- rbd info volumes/lab-probe' \
    'kubectl -n rook-ceph exec deploy/rook-ceph-tools -- rbd rm volumes/lab-probe' \
    'kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph auth get client.cinder | grep caps' \
    "kubectl -n openstack get secret ceph-client-cinder -o jsonpath='{.data.userKey}' | base64 -d | sha256sum" \
    'kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph auth get-key client.cinder | sha256sum' \
    'kubectl -n rook-ceph patch cephclient cinder-backup --type merge \'; do
    assert_eq "the section holds '$line' once" "1" "$(count_lines "$SECTION" "$line")"
  done
  # A transcript of these commands ends up in issues and PR bodies.
  assert_eq "every command that reads a key prints its digest" "" \
    "$(grep -E '(get-key|userKey|ceph auth get )' <<<"$SECTION" | grep -E '^kubectl ' |
      grep -vE '\| (sha256sum|grep caps)$' || true)"
  assert_contains "the teardown command" "$SECTION" '`EXTERNAL_CLUSTER=true make teardown-infra`'
}

# --- Test 4: the names and their sources ---
# shellcheck disable=SC2016 # the sources' literal text, not expanded here
test_names_from_sources() {
  echo "Test: each name the section uses comes from its source"
  local chart image tools timeout keys
  chart="$(awk '/^      version: / { gsub(/"/, "", $2); print $2; exit }' "$CEPH_DIR/release.yaml")"
  assert_not_empty "release.yaml pins a chart version" "$chart"
  assert_contains "the section names the chart version ${chart}" "$SECTION" "\`rook-ceph\` \`${chart}\`"

  image="$(awk '/^    image: / { sub(/@.*/, "", $2); print $2; exit }' "$CEPH_DIR/cluster/cluster.yaml")"
  assert_not_empty "cluster.yaml names a Ceph image" "$image"
  assert_contains "the section names the Ceph image ${image}" "$SECTION" "\`${image}\`"

  tools="$(awk '/^kind: Deployment$/ { f = 1 } f && /^  name: / { print $2; exit }' "$CEPH_DIR/cluster/toolbox.yaml")"
  assert_eq "the toolbox Deployment is rook-ceph-tools" "rook-ceph-tools" "$tools"

  local path role
  for path in ceph/client-cinder ceph/client-cinder-backup; do
    assert_file_contains_fixed "keys-push.yaml writes ${path}" "$CEPH_DIR/cluster/keys-push.yaml" "remoteKey: ${path}"
    assert_file_contains_fixed "keys-pull.yaml reads ${path}" "$CEPH_DIR/cluster/keys-pull.yaml" "key: ${path}"
    assert_contains "the section names ${path}" "$SECTION" "\`${path}\`"
  done
  for role in push-ceph-keys:keys-push.yaml read-ceph-keys:keys-pull.yaml; do
    assert_file_contains_fixed "${role#*:} authenticates as ${role%%:*}" "$CEPH_DIR/cluster/${role#*:}" "role: ${role%%:*}"
    assert_contains "the section names the role ${role%%:*}" "$SECTION" "\`${role%%:*}\`"
  done

  timeout="$(sed -n 's/^CEPH_TIMEOUT="\${CEPH_TIMEOUT:-\([0-9]*\)}"$/\1/p' "$DEPLOY_INFRA_SH")"
  assert_not_empty "hack/deploy-infra.sh defaults CEPH_TIMEOUT" "$timeout"
  assert_contains "the section names its default" "$SECTION" "\`CEPH_TIMEOUT\` (${timeout})"

  assert_file_contains_fixed "the health wait logs its success line" "$DEPLOY_INFRA_SH" \
    'log "Ceph: phase Ready, HEALTH_OK, ${ready} of ${count} OSDs ready, fsid ${fsid:-unknown}"'
  assert_contains "the section quotes it" "$SECTION" '`Ceph: phase Ready, HEALTH_OK, 3 of 3 OSDs ready, fsid <fsid>`'
  assert_file_contains_fixed "and its timeout line" "$DEPLOY_INFRA_SH" \
    'log "ERROR: Ceph did not reach HEALTH_OK with ${count:-unknown} OSDs within ${CEPH_TIMEOUT}s (phase '"'"'${phase}'"'"', health '"'"'${health}'"'"', ${ready} OSDs ready)."'
  assert_contains "the section quotes it" "$SECTION" \
    "\`ERROR: Ceph did not reach HEALTH_OK with <count> OSDs within <n>s (phase '<phase>', health '<health>', <ready> OSDs ready).\`"

  keys="$(sed -n 's/^  local keys=(\(.*\))$/\1/p' "$DEPLOY_INFRA_SH")"
  assert_not_empty "sync_ceph_client_keys lists its keys" "$keys"
  assert_file_contains_fixed "the key sync logs the keys it synced" "$DEPLOY_INFRA_SH" \
    'log "Ceph client keys ${keys[*]} synced into openstack."'
  assert_contains "the section quotes that line with the keys ${keys}" "$SECTION" \
    "\`Ceph client keys ${keys} synced into openstack.\`"
}

# --- Test 5: the teardown sub-steps ---
# shellcheck disable=SC2016 # the page's and the script's literal text, not expanded here
test_teardown_steps() {
  echo "Test: the teardown is six sub-steps in the order of teardown_ceph"
  assert_eq "the teardown has six numbered sub-steps" "1 2 3 4 5 6" \
    "$(grep -oE '^[0-9]+\.' <<<"$TEARDOWN" | tr -d '.' | paste -sd' ' -)"

  local mark deps cleanup pvc operator
  mark="$(first_line "$TEARDOWN_SH" 'kubectl delete cephcluster rook-ceph -n rook-ceph --ignore-not-found --wait=false')"
  deps="$(first_line "$TEARDOWN_SH" 'delete_and_wait "the Ceph clients, pools, keys and toolbox"')"
  cleanup="$(first_line "$TEARDOWN_SH" 'wait_for_ceph_cleanup_jobs_done "${nodes}"')"
  pvc="$(first_line "$TEARDOWN_SH" 'delete_and_wait "the PVCs in rook-ceph"')"
  operator="$(first_line "$TEARDOWN_SH" 'delete_and_wait "the Ceph operator overlay (HelmRelease and HelmRepository)"')"
  if [[ -n "$mark" && -n "$deps" && -n "$cleanup" && -n "$pvc" && -n "$operator" ]] &&
    ((mark < deps && deps < cleanup && cleanup < pvc && pvc < operator)); then
    echo "  PASS: teardown_ceph marks the CephCluster, deletes its dependents, waits for the cleanup, then deletes the claims and the operator"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: teardown_ceph is out of the documented order (mark '${mark}', dependents '${deps}', cleanup '${cleanup}', claims '${pvc}', operator '${operator}')"
    FAIL=$((FAIL + 1))
  fi

  assert_contains "sub-step 1 renders ceph/cluster/" "$(grep '^1\. ' <<<"$TEARDOWN" || true)" 'renders `ceph/cluster/`'
  assert_contains "sub-step 2 confirms the cleanup policy" "$(grep '^2\. ' <<<"$TEARDOWN" || true)" '`yes-really-destroy-data`'
  assert_contains "and marks the CephCluster for deletion" "$(grep '^2\. ' <<<"$TEARDOWN" || true)" \
    'marks the CephCluster for deletion without waiting'
  assert_contains "sub-step 3 deletes the dependents" "$(grep '^3\. ' <<<"$TEARDOWN" || true)" \
    'deletes that render without its CephCluster'
  assert_contains "sub-step 4 waits for the cleanup Jobs" "$(grep '^4\. ' <<<"$TEARDOWN" || true)" \
    '`rook-ceph-cleanup=true`'
  assert_contains "sub-step 5 deletes the claims" "$(grep '^5\. ' <<<"$TEARDOWN" || true)" 'claims in `rook-ceph`'
  assert_contains "sub-step 6 deletes the render of ceph/" "$(grep '^6\. ' <<<"$TEARDOWN" || true)" \
    'render of `ceph/` without its Namespace'
}

# --- Run ---
test_position
test_files
test_commands
test_names_from_sources
test_teardown_steps

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
