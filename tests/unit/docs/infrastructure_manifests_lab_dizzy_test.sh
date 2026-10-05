#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the `### Lab dizzy stack` subsection of
# docs/reference/infrastructure/infrastructure-manifests.md (#1225):
#   1. the heading occurs once, after `#### Lab fault runs` and before
#      `### Lab ControlPlane`; `#### Lab dizzy run` occurs once, after it, and
#      the next heading is `### Lab Prometheus stack`
#   2. the subsection names the overlay file, the deploy command, both
#      port-forward commands, `EXTERNAL_CLUSTER=true make dizzy-keystone`, the
#      claim server-volume-dizzy-victoria-metrics-server-0, the posture and the
#      teardown command
#   3. the run subsection holds the commands of the run's eight steps, each a
#      whole line once its leading whitespace is stripped
#   4. each name comes from its source: the fullnameOverride of the
#      VictoriaMetrics release (the Service and StatefulSet names), the
#      datasource uid of the Grafana release, the port-forward hint of
#      hack/dizzy.sh, the deploy's completion line and the teardown's log
#      lines
#   5. the kind dizzy section and the opening of `## Metal-stack lab` link
#      #lab-dizzy-stack, and `### Lab overlay` names WITH_DIZZY and dizzy/
#   6. the run subsection holds either the sentence that no run is recorded
#      or a line that starts with `The run of 20YY-MM-DD`, not both and not
#      neither
#   7. the run block parses as bash, and its helpers run from the page with
#      kubectl and curl stubbed: `dizzy_pvs` counts the PersistentVolumes of
#      claims in dizzy, 0 on an empty list, and `queries` prints one line per
#      metric with the frame's values, `[]` for a metric without samples
#
# Check 7 needs jq and counts as one SKIP without it. Subsections run from
# their heading to the next heading of any level, fenced code included.
# INFRA_MANIFESTS_DOC overrides the page.
#
# Usage: bash tests/unit/docs/infrastructure_manifests_lab_dizzy_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

DOC="${INFRA_MANIFESTS_DOC:-$PROJECT_ROOT/docs/reference/infrastructure/infrastructure-manifests.md}"

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
# their leading whitespace is stripped. <line> reaches awk through the
# environment, so a backslash in it stays one.
count_lines() {
  want="$2" awk '{ sub(/^[[:space:]]+/, "") } $0 == ENVIRON["want"] { n++ } END { print n + 0 }' <<<"$1"
}

# line_of_heading <text> prints the line number of the heading <text>, empty
# when it is missing.
line_of_heading() {
  headings | awk -v heading="$1" '{ line = $0; sub(/:.*/, "", line); sub(/^[0-9]+:/, "") } $0 == heading { print line; exit }'
}

STACK="$(subsection '### Lab dizzy stack')"
RUN="$(subsection '#### Lab dizzy run')"

# The run block: the fenced bash code of the run subsection.
BLOCK="$(awk '/^```bash$/ { f = 1; next } f && /^```$/ { exit } f { print }' <<<"$RUN")"

# --- Test 1: the place ---
test_position() {
  echo "Test: '### Lab dizzy stack' and '#### Lab dizzy run' sit once between the fault runs and Lab ControlPlane"
  local all faults stack run controlplane next
  all="$(headings)"
  assert_eq "'### Lab dizzy stack' occurs once" "1" "$(grep -cxE '[0-9]+:### Lab dizzy stack' <<<"$all" || true)"
  assert_eq "'#### Lab dizzy run' occurs once" "1" "$(grep -cxE '[0-9]+:#### Lab dizzy run' <<<"$all" || true)"
  faults="$(line_of_heading '#### Lab fault runs')"
  stack="$(line_of_heading '### Lab dizzy stack')"
  run="$(line_of_heading '#### Lab dizzy run')"
  controlplane="$(line_of_heading '### Lab ControlPlane')"
  if [[ -n "$faults" && -n "$stack" && -n "$run" && -n "$controlplane" ]] &&
    ((faults < stack && stack < run && run < controlplane)); then
    echo "  PASS: the section sits on line $stack and its run on line $run, between line $faults and line $controlplane"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the section is missing or out of place (Lab fault runs '${faults}', Lab dizzy stack '${stack}', Lab dizzy run '${run}', Lab ControlPlane '${controlplane}')"
    FAIL=$((FAIL + 1))
  fi
  next="$(awk -F: -v run="${run:-0}" '$1 > run { sub(/^[0-9]+:/, ""); print; exit }' <<<"$all")"
  assert_eq "the heading after '#### Lab dizzy run' is '### Lab Prometheus stack'" "### Lab Prometheus stack" "$next"
}

# --- Test 2: the section's content ---
# shellcheck disable=SC2016,SC1003 # the page's literal text, not expanded here
test_section_content() {
  echo "Test: the section names the file, the commands, the claim, the posture and the teardown"
  assert_not_empty "the section has a body" "$STACK"
  local text
  for text in \
    '**File:** `deploy/lab/metal-stack/dizzy/kustomization.yaml`' \
    'EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true WITH_NFS=true WITH_DIZZY=true make deploy-infra' \
    'kubectl -n envoy-gateway-system port-forward \' \
    'kubectl -n dizzy port-forward svc/dizzy-victoria-metrics-server 8428:8428' \
    'EXTERNAL_CLUSTER=true make dizzy-keystone' \
    'server-volume-dizzy-victoria-metrics-server-0' \
    '**Posture.**' \
    '`EXTERNAL_CLUSTER=true make teardown-infra`' \
    'https://dizzy.127-0-0-1.nip.io:8443' \
    '`tests/unit/deploy/metal_stack_dizzy_test.sh`'; do
    assert_contains "the section holds '$text'" "$STACK" "$text"
  done
}

# --- Test 3: the run's commands ---
# shellcheck disable=SC2016,SC1003 # the page's literal lines, not expanded here
test_run_commands() {
  echo "Test: the run block holds the commands of its eight steps"
  assert_not_empty "the run subsection has a bash block" "$BLOCK"
  local line
  for line in \
    'kubectl get namespace openstack dizzy flux-system' \
    'EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true WITH_NFS=true WITH_DIZZY=true make deploy-infra' \
    'kubectl get helmrelease -n dizzy' \
    "kubectl get svc dizzy-victoria-metrics-server -n dizzy -o jsonpath='{.spec.type} {.spec.ports[0].nodePort}{\"\\n\"}'" \
    'kubectl get pvc -n dizzy' \
    'curl -fsS http://localhost:8428/health; echo' \
    'EXTERNAL_CLUSTER=true make dizzy-keystone; echo "exit $?"' \
    "curl -fsS http://localhost:8428/api/v1/label/__name__/values | jq -r '.data[]' | grep '^dizzy_'" \
    "curl -sk 'https://dizzy.127-0-0-1.nip.io:8443/api/search?type=dash-db' | jq -r '.[].uid' | sort" \
    'for metric in dizzy_iterations_total dizzy_operation_duration_seconds_count dizzy_resource_time_to_ready_seconds_count; do' \
    'curl -sk -H '"'"'Content-Type: application/json'"'"' https://dizzy.127-0-0-1.nip.io:8443/api/ds/query \' \
    'kubectl delete pod dizzy-victoria-metrics-server-0 -n dizzy' \
    'kubectl rollout status statefulset/dizzy-victoria-metrics-server -n dizzy --timeout=300s' \
    'EXTERNAL_CLUSTER=true make teardown-infra' \
    'kubectl get namespace dizzy' \
    'rm _output/dizzy/clouds.yaml'; do
    assert_eq "the block holds '$line' once" "1" "$(count_lines "$BLOCK" "$line")"
  done
  assert_eq "the queries run twice, before and after the pod restart" "2" "$(count_lines "$BLOCK" 'queries')"
  assert_contains "each query reads the provisioned datasource" "$BLOCK" '\"uid\":\"victoriametrics\"'
}

# --- Test 4: the names and their sources ---
# shellcheck disable=SC2016 # the page's literal text, not expanded here
test_names_from_sources() {
  echo "Test: each name the section uses comes from its source"
  local kind_dir="$PROJECT_ROOT/deploy/kind/dizzy"
  # The chart appends -server to the fullname for the Service and the
  # StatefulSet; the claim is <template>-<statefulset>-0.
  assert_file_contains_fixed "the VictoriaMetrics release sets fullnameOverride dizzy-victoria-metrics" \
    "$kind_dir/release-victoria-metrics.yaml" 'fullnameOverride: dizzy-victoria-metrics'
  assert_file_contains_fixed "the Grafana release provisions the datasource uid victoriametrics" \
    "$kind_dir/release-grafana.yaml" 'uid: victoriametrics'
  assert_file_contains_fixed "the Grafana datasource reads the Service dizzy-victoria-metrics-server" \
    "$kind_dir/release-grafana.yaml" 'url: http://dizzy-victoria-metrics-server.dizzy.svc:8428'
  assert_file_contains_fixed "the HTTPRoute serves dizzy.127-0-0-1.nip.io" \
    "$kind_dir/httproute.yaml" '- dizzy.127-0-0-1.nip.io'
  assert_file_contains_fixed "hack/dizzy.sh names the same VictoriaMetrics port-forward" \
    "$PROJECT_ROOT/hack/dizzy.sh" 'kubectl -n dizzy port-forward svc/dizzy-victoria-metrics-server 8428:8428'
  assert_file_contains_fixed "hack/deploy-infra.sh prints the same port-forward on completion" \
    "$PROJECT_ROOT/hack/deploy-infra.sh" 'log "dizzy:  kubectl -n dizzy port-forward svc/dizzy-victoria-metrics-server 8428:8428"'
  assert_file_contains_fixed "hack/deploy-infra.sh warns with the line the run expects absent" \
    "$PROJECT_ROOT/hack/deploy-infra.sh" 'predates the dizzy metrics port mapping'
  assert_contains "the run expects that warning absent" "$RUN" 'no `predates the dizzy metrics port mapping` warning'
  assert_file_contains_fixed "hack/teardown-infra.sh logs the HelmRelease delete the run expects" \
    "$PROJECT_ROOT/hack/teardown-infra.sh" 'delete_and_wait "the dizzy HelmReleases"'
  assert_file_contains_fixed "and the claim delete" \
    "$PROJECT_ROOT/hack/teardown-infra.sh" 'delete_and_wait "the PVCs in dizzy"'
  assert_contains "the run expects both log lines" "$RUN" \
    '`Deleting the dizzy HelmReleases...` and `Deleting the PVCs in dizzy...`'
}

# --- Test 5: the links ---
# shellcheck disable=SC2016 # the page's literal text, not expanded here
test_links() {
  echo "Test: the kind section and the lab opening link the section, and Lab overlay names WITH_DIZZY"
  assert_contains "the kind dizzy section links #lab-dizzy-stack" \
    "$(subsection '### dizzy load/chaos stack (kind-only opt-in)')" '(#lab-dizzy-stack)'
  assert_contains "the opening of Metal-stack lab links #lab-dizzy-stack" \
    "$(subsection '## Metal-stack lab')" '(#lab-dizzy-stack)'
  local overlay
  overlay="$(subsection '### Lab overlay')"
  assert_contains "Lab overlay names WITH_DIZZY among the accepted opt-ins" "$overlay" '`WITH_DIZZY` aside'
  assert_contains "and the dizzy/ it applies" "$overlay" '`dizzy/`)'
}

# --- Test 6: the record ---
test_record_or_placeholder() {
  echo "Test: the run subsection holds the record of a lab run or says that none is recorded"
  local placeholder record
  placeholder="$(count_lines "$RUN" 'No lab run of this stack is recorded yet.')"
  record="$({ grep -E '^The run of 20[0-9]{2}-[0-9]{2}-[0-9]{2}' <<<"$RUN" || true; } | grep -c . || true)"
  if [[ $((placeholder + record)) -ge 1 && ("$placeholder" -eq 0 || "$record" -eq 0) ]]; then
    echo "  PASS: the subsection holds the placeholder ($placeholder) or a record ($record), not both"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the subsection holds $placeholder placeholder and $record record lines"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 7: the block parses, and its helpers run ---
# run_helper <dir> <commands> runs <commands> in bash with the block's
# dizzy_pvs and queries defined and kubectl and curl replaced: kubectl prints
# <dir>/pv.json, curl prints <dir>/frame.json.
run_helper() {
  bash -c '
    eval "$1"
    kubectl() { cat "$dir/pv.json"; }
    curl() { cat "$dir/frame.json"; }
    eval "$3"
  ' _ "$(awk '
    /^(dizzy_pvs|queries)\(\) \{$/ { f = 1 }
    f { print }
    f && /^}$/ { f = 0 }
  ' <<<"$BLOCK")" "$1" "dir=$1; $2"
}

test_block_runs() {
  echo "Test: the run block parses, and dizzy_pvs and queries run from the page"
  local rc=0
  bash -n <<<"$BLOCK" 2>/dev/null || rc=$?
  assert_eq "the run block parses as bash" "0" "$rc"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed, the helpers cannot run"
    SKIP=$((SKIP + 1))
    return
  fi

  local dir
  dir="$(mktemp -d)"
  cat >"$dir/pv.json" <<'JSON'
{"items":[
  {"metadata":{"name":"pv-a"},"spec":{"claimRef":{"namespace":"dizzy","name":"server-volume-dizzy-victoria-metrics-server-0"}}},
  {"metadata":{"name":"pv-b"},"spec":{"claimRef":{"namespace":"openstack","name":"nfs-server-exports"}}},
  {"metadata":{"name":"pv-c"},"spec":{}}]}
JSON
  assert_eq "dizzy_pvs counts the one volume claimed in dizzy" "1" "$(run_helper "$dir" dizzy_pvs 2>&1 || true)"
  echo '{"items":[]}' >"$dir/pv.json"
  assert_eq "and prints 0 on an empty list" "0" "$(run_helper "$dir" dizzy_pvs 2>&1 || true)"

  echo '{"results":{"A":{"frames":[{"data":{"values":[[1760000000000],[42]]}}]}}}' >"$dir/frame.json"
  assert_eq "queries prints one line per metric with the frame's values" \
    "$(printf '%s\n' \
      '{"metric":"dizzy_iterations_total","values":[[1760000000000],[42]]}' \
      '{"metric":"dizzy_operation_duration_seconds_count","values":[[1760000000000],[42]]}' \
      '{"metric":"dizzy_resource_time_to_ready_seconds_count","values":[[1760000000000],[42]]}')" \
    "$(run_helper "$dir" queries 2>&1 || true)"
  echo '{"results":{"A":{"frames":[{"data":{"values":[]}}]}}}' >"$dir/frame.json"
  assert_eq "and [] for a metric without samples" \
    '{"metric":"dizzy_iterations_total","values":[]}' \
    "$(run_helper "$dir" queries 2>&1 | head -n 1 || true)"
  rm -rf "$dir"
}

# --- Run ---
test_position
test_section_content
test_run_commands
test_names_from_sources
test_links
test_record_or_placeholder
test_block_runs

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
