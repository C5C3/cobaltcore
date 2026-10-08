#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the `### Lab Prometheus stack` subsection of
# docs/reference/infrastructure/infrastructure-manifests.md (#1226):
#   1. the heading occurs once, after `#### Lab dizzy run`; `#### Lab
#      Prometheus run` occurs once, after it, and the next heading is
#      `### Lab ControlPlane`
#   2. the subsection names both files, the deploy command, the rows of its
#      table of differences from kind, both port-forward commands, Grafana's
#      default credentials, the posture with insecureSkipVerify, the teardown
#      command and the test that pins the overlay
#   3. the run subsection holds the commands of the run's seven steps, each a
#      whole line once its leading whitespace is stripped
#   4. each name comes from its source: the release name of the kind overlay,
#      the fullnameOverride of the hvo release (the job of hvo's target), the
#      dashboard's uid, the deploy's log line and the teardown's two new log
#      lines
#   5. the kind Prometheus section and the opening of `## Metal-stack lab` link
#      #lab-prometheus-stack, `### Lab overlay` names WITH_PROMETHEUS and
#      prometheus/, and the hvo rows of `### Lab hypervisors` name the 8
#      alerts without data, the ServiceMonitor patch and the patch of the
#      PrometheusRules' version label, which the section names too; those rows
#      and the section name the chart the alerts and the query counts were
#      read from, the ref.tag of hvo's chart in
#      deploy/lab/metal-stack/hypervisor/sources.yaml, which is also the value
#      of the version-label patch row; that row names the label the chart
#      renders from ref.tag and ref.digest
#   6. the run subsection holds either the sentence that no run is recorded
#      or a line that starts with `The run of 20YY-MM-DD`, not both and not
#      neither
#   7. the run block parses as bash, and its helpers run from the page with
#      kubectl and curl stubbed: `kube_system_objects` sorts the names,
#      `targets` prints one sorted line per target with its last error,
#      `query` prints the number of series, 0 for none, and passes the time it
#      is given, and `monitoring_pvs` counts the PersistentVolumes of claims in
#      monitoring, 0 on an empty list
#
# Check 7 needs jq and counts as one SKIP without it. Subsections run from
# their heading to the next heading of any level, fenced code included.
# INFRA_MANIFESTS_DOC overrides the page.
#
# Usage: bash tests/unit/docs/infrastructure_manifests_lab_prometheus_test.sh

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

STACK="$(subsection '### Lab Prometheus stack')"
RUN="$(subsection '#### Lab Prometheus run')"

# The ref.tag and the ref.digest of the OCIRepository
# openstack-hypervisor-operator.
HVO_CHART_TAG="$(awk '
  /^  name: / { inside = ($2 == "openstack-hypervisor-operator") }
  inside && /^    tag: / { gsub(/"/, "", $2); print $2; exit }
' "$PROJECT_ROOT/deploy/lab/metal-stack/hypervisor/sources.yaml")"
HVO_CHART_DIGEST="$(awk '
  /^  name: / { inside = ($2 == "openstack-hypervisor-operator") }
  inside && /^    digest: / { gsub(/"/, "", $2); print $2; exit }
' "$PROJECT_ROOT/deploy/lab/metal-stack/hypervisor/sources.yaml")"

# The run block: the fenced bash code of the run subsection.
BLOCK="$(awk '/^```bash$/ { f = 1; next } f && /^```$/ { exit } f { print }' <<<"$RUN")"

# --- Test 1: the place ---
test_position() {
  echo "Test: '### Lab Prometheus stack' and '#### Lab Prometheus run' sit once between the dizzy run and Lab ControlPlane"
  local all dizzy stack run controlplane next
  all="$(headings)"
  assert_eq "'### Lab Prometheus stack' occurs once" "1" "$(grep -cxE '[0-9]+:### Lab Prometheus stack' <<<"$all" || true)"
  assert_eq "'#### Lab Prometheus run' occurs once" "1" "$(grep -cxE '[0-9]+:#### Lab Prometheus run' <<<"$all" || true)"
  dizzy="$(line_of_heading '#### Lab dizzy run')"
  stack="$(line_of_heading '### Lab Prometheus stack')"
  run="$(line_of_heading '#### Lab Prometheus run')"
  controlplane="$(line_of_heading '### Lab ControlPlane')"
  if [[ -n "$dizzy" && -n "$stack" && -n "$run" && -n "$controlplane" ]] &&
    ((dizzy < stack && stack < run && run < controlplane)); then
    echo "  PASS: the section sits on line $stack and its run on line $run, between line $dizzy and line $controlplane"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the section is missing or out of place (Lab dizzy run '${dizzy}', Lab Prometheus stack '${stack}', Lab Prometheus run '${run}', Lab ControlPlane '${controlplane}')"
    FAIL=$((FAIL + 1))
  fi
  next="$(awk -F: -v run="${run:-0}" '$1 > run { sub(/^[0-9]+:/, ""); print; exit }' <<<"$all")"
  assert_eq "the heading after '#### Lab Prometheus run' is '### Lab ControlPlane'" "### Lab ControlPlane" "$next"
}

# --- Test 2: the section's content ---
# shellcheck disable=SC2016 # the page's literal text, not expanded here
test_section_content() {
  echo "Test: the section names the files, the commands, the differences from kind, the posture and the teardown"
  assert_not_empty "the section has a body" "$STACK"
  local text
  for text in \
    '**Files:** `deploy/lab/metal-stack/prometheus/kustomization.yaml`,' \
    '`deploy/lab/metal-stack/prometheus/hypervisor-operator.json`' \
    'EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true WITH_NFS=true WITH_PROMETHEUS=true make deploy-infra' \
    '| Namespace `monitoring` |' \
    '| Scrape jobs |' \
    '| Retention | `6h` | `7d`, bounded by `retentionSize: 8GB` |' \
    '| Storage | emptyDir |' \
    '| Rule selector |' \
    '| Prometheus memory | request `256Mi`, limit `512Mi`, sized for one kind node | request `1Gi`, limit `2Gi`:' \
    '| Dashboards | `keystone-operator` | `keystone-operator` and `hypervisor-operator` |' \
    'kubectl -n monitoring port-forward svc/kube-prometheus-stack-prometheus 9090:9090' \
    'kubectl -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80' \
    'signs in `admin`' \
    'password `prom-operator`' \
    '**Posture.**' \
    '`insecureSkipVerify`' \
    '`EXTERNAL_CLUSTER=true make teardown-infra`' \
    '`tests/unit/deploy/metal_stack_prometheus_test.sh`'; do
    assert_contains "the section holds '$text'" "$STACK" "$text"
  done
  local scrape
  scrape="$(grep -F '| Scrape jobs |' <<<"$STACK" || true)"
  for text in '`kubeEtcd`' '`kubeScheduler`' '`kubeControllerManager`' '`kubeProxy`' 'seed' '`k8s-app: kube-proxy`'; do
    assert_contains "the scrape jobs row names $text" "$scrape" "$text"
  done
  assert_contains "the posture names the token Prometheus sends to hvo" \
    "$(awk '/^\*\*Posture\.\*\*/,/^$/' <<<"$STACK" | tr '\n' ' ')" 'ServiceAccount token to hvo'"'"'s metrics Service with `insecureSkipVerify`'
}

# --- Test 3: the run's commands ---
# shellcheck disable=SC2016,SC1003 # the page's literal lines, not expanded here
test_run_commands() {
  echo "Test: the run block holds the commands of its seven steps"
  assert_not_empty "the run subsection has a bash block" "$BLOCK"
  local line
  for line in \
    'kubectl get namespace openstack monitoring flux-system' \
    'kube_system_objects >_output/kube-system-before.txt' \
    'EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true WITH_NFS=true WITH_PROMETHEUS=true make deploy-infra' \
    'kubectl get helmrelease kube-prometheus-stack -n monitoring' \
    "kubectl get namespace monitoring -o jsonpath='{.metadata.labels}{\"\\n\"}{.metadata.annotations}{\"\\n\"}'" \
    'kubectl get pvc -n monitoring' \
    'kubectl get storageclass' \
    "kubectl get service -n kube-system -o name | grep '^service/kube-prometheus-stack'" \
    'kubectl get servicemonitor,prometheusrule -n openstack' \
    "curl -fsS 'http://localhost:9090/api/v1/targets?state=active' |" \
    "jq -r '.panels[].targets[].expr' deploy/lab/metal-stack/prometheus/hypervisor-operator.json |" \
    'curl -fsS http://localhost:9090/api/v1/rules |' \
    "curl -fsS -u admin:prom-operator 'http://localhost:3000/api/search?type=dash-db' | jq -r '.[].uid' | sort" \
    'kubectl delete pod prometheus-kube-prometheus-stack-prometheus-0 -n monitoring' \
    'kubectl rollout status statefulset/prometheus-kube-prometheus-stack-prometheus -n monitoring --timeout=300s' \
    'query up "$before"' \
    'kubectl top pod prometheus-kube-prometheus-stack-prometheus-0 -n monitoring --containers' \
    'EXTERNAL_CLUSTER=true make teardown-infra' \
    'kube_system_objects | diff _output/kube-system-before.txt - && echo "kube-system unchanged"' \
    'kubectl get namespace monitoring' \
    'rm _output/kube-system-before.txt'; do
    assert_eq "the block holds '$line' once" "1" "$(count_lines "$BLOCK" "$line")"
  done
  assert_eq "the targets are listed once, in step 3" "1" "$(count_lines "$BLOCK" 'targets')"
  assert_contains "the rules are filtered to hvo's two PrometheusRules" "$BLOCK" \
    'test("openstack-hypervisor-operator-(operator|eviction)-alerts")'
}

# --- Test 4: the names and their sources ---
# shellcheck disable=SC2016 # the page's literal text, not expanded here
test_names_from_sources() {
  echo "Test: each name the section uses comes from its source"
  assert_file_contains_fixed "the kind release is named kube-prometheus-stack" \
    "$PROJECT_ROOT/deploy/kind/prometheus/release.yaml" '  name: kube-prometheus-stack'
  # The chart names the metrics Service <fullname>-controller-manager-metrics-service.
  assert_file_contains_fixed "the hvo release sets fullnameOverride hypervisor-operator" \
    "$PROJECT_ROOT/deploy/lab/metal-stack/hypervisor/hvo-release.yaml" 'fullnameOverride: hypervisor-operator'
  assert_contains "the section names the job of hvo's target" "$STACK" \
    '`hypervisor-operator-controller-manager-metrics-service`'
  assert_file_contains_fixed "the dashboard's uid is hypervisor-operator" \
    "$PROJECT_ROOT/deploy/lab/metal-stack/prometheus/hypervisor-operator.json" '"uid": "hypervisor-operator"'
  assert_file_contains_fixed "hack/deploy-infra.sh logs the apply the run expects" \
    "$PROJECT_ROOT/hack/deploy-infra.sh" 'log "Prometheus overlay ${OVERLAY_ROOT}/prometheus applied (WITH_PROMETHEUS=true)."'
  assert_contains "the run expects that line with the lab overlay" "$RUN" \
    '`Prometheus overlay <clone>/deploy/lab/metal-stack/prometheus applied (WITH_PROMETHEUS=true).`'
  assert_file_contains_fixed "hack/teardown-infra.sh logs the claim delete the run expects" \
    "$PROJECT_ROOT/hack/teardown-infra.sh" 'delete_and_wait "the PVCs in monitoring"'
  assert_file_contains_fixed "and the kubelet Service delete" \
    "$PROJECT_ROOT/hack/teardown-infra.sh" \
    'delete_and_wait "the kubelet Service and Endpoints the Prometheus Operator left in kube-system"'
  assert_contains "the run expects both log lines" "$RUN" \
    '`Deleting the PVCs in monitoring...` and `Deleting the kubelet Service and Endpoints the Prometheus Operator left in kube-system...`'
}

# --- Test 5: the links and the hvo rows ---
# shellcheck disable=SC2016 # the page's literal text, not expanded here
test_links() {
  echo "Test: the kind section and the lab opening link the section, Lab overlay names WITH_PROMETHEUS, and the hvo rows name the alerts"
  assert_contains "the kind Prometheus section links #lab-prometheus-stack" \
    "$(subsection '### kube-prometheus-stack (kind-only opt-in)')" '(#lab-prometheus-stack)'
  assert_contains "the opening of Metal-stack lab links #lab-prometheus-stack" \
    "$(subsection '## Metal-stack lab')" '(#lab-prometheus-stack)'
  local overlay
  overlay="$(subsection '### Lab overlay')"
  assert_contains "Lab overlay names WITH_PROMETHEUS" "$overlay" '`WITH_PROMETHEUS=true`'
  assert_contains "and the prometheus/ it applies" "$overlay" '`prometheus/`'

  local hvo alert
  hvo="$(subsection '### Lab hypervisors' | grep -E '^\| hvo \|' || true)"
  assert_contains "the hvo rows turn the ServiceMonitor and the PrometheusRules on" "$hvo" \
    '| hvo | `serviceMonitor.enabled`, `prometheusRules.create` | `true` |'
  assert_contains "and leave the custom-resource metrics off" "$hvo" '| hvo | `customResourceMetrics.create` | `false` |'
  assert_contains "and the dashboards, a Perses dashboard" "$hvo" '| hvo | `dashboards.create` | `false` | The chart'"'"'s dashboard is a Perses dashboard'
  assert_contains "the hvo rows name the ServiceMonitor patch" "$hvo" \
    '`bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token`'
  for alert in HypervisorOnboardingStuck HypervisorEvictionStuck HypervisorEvictedTooLong \
    HypervisorTraitSyncFailed HypervisorAggregateSyncFailed EvictionFailed \
    EvictionMigrationFailing EvictionOutstandingRamHigh; do
    assert_contains "the hvo rows name the alert without data $alert" "$hvo" "\`$alert\`"
  done
  # The alerts and the query counts are copied from the pinned chart; a chart
  # move that leaves the tag beside them leaves them unread.
  assert_not_empty "sources.yaml pins a ref.tag for hvo's chart" "$HVO_CHART_TAG"
  assert_contains "the hvo rows name the chart their alerts were read from, ref.tag ${HVO_CHART_TAG}" "$hvo" \
    "chart \`${HVO_CHART_TAG}\` renders"
  assert_contains "and the section names it beside the query counts of the chart's dashboard" "$STACK" \
    "in chart \`${HVO_CHART_TAG}\`"
  # The chart writes its version, with a '+', into the label; the patch
  # writes ref.tag, so a chart move that leaves the row's value fails here.
  assert_contains "the hvo rows name the version-label patch on the PrometheusRules, with the ref.tag" "$hvo" \
    "| hvo | post-renderer | \`app.kubernetes.io/version: ${HVO_CHART_TAG}\` on both PrometheusRules |"
  # helm-controller sets the build metadata of a chart pinned by digest to
  # the digest's first 12 characters, so a move of either ref moves the label.
  local hex="${HVO_CHART_DIGEST#*:}"
  assert_contains "and the label the chart renders from ref.tag and ref.digest" "$hvo" \
    "so the label reads \`${HVO_CHART_TAG%%_*}+${hex:0:12}\`"
  assert_contains "and the section says the post-renderer replaces that label of both rules" "$STACK" \
    'label `app.kubernetes.io/version` of both rules'
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
# run_helper <dir> <commands> runs <commands> in bash with the block's four
# helpers defined and kubectl and curl replaced: kubectl prints
# <dir>/kubectl.out, curl writes its arguments to <dir>/curl.args and prints
# <dir>/curl.out.
run_helper() {
  bash -c '
    eval "$1"
    kubectl() { cat "$dir/kubectl.out"; }
    curl() { printf "%s\n" "$@" >"$dir/curl.args"; cat "$dir/curl.out"; }
    eval "$3"
  ' _ "$(awk '
    /^(kube_system_objects|targets|query|monitoring_pvs)\(\) \{$/ { f = 1 }
    f { print }
    f && /^}$/ { f = 0 }
  ' <<<"$BLOCK")" "$1" "dir=$1; $2"
}

test_block_runs() {
  echo "Test: the run block parses, and its four helpers run from the page"
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
  printf '%s\n' service/kube-dns endpoints/kube-dns service/kube-prometheus-stack-coredns >"$dir/kubectl.out"
  assert_eq "kube_system_objects sorts the names" \
    "$(printf '%s\n' endpoints/kube-dns service/kube-dns service/kube-prometheus-stack-coredns)" \
    "$(run_helper "$dir" kube_system_objects 2>&1 || true)"

  cat >"$dir/curl.out" <<'JSON'
{"data":{"activeTargets":[
  {"labels":{"job":"nova-operator-metrics","namespace":"nova-system"},"health":"up","lastError":""},
  {"labels":{"job":"hypervisor-operator-controller-manager-metrics-service","namespace":"openstack"},"health":"down",
   "lastError":"server returned HTTP status 401 Unauthorized"}]}}
JSON
  assert_eq "targets prints one sorted line per target, with its last error" \
    "$(printf '%s\t%s\t%s\t%s\n' \
      hypervisor-operator-controller-manager-metrics-service openstack down 'server returned HTTP status 401 Unauthorized' \
      nova-operator-metrics nova-system up '')" \
    "$(run_helper "$dir" targets 2>&1 || true)"

  echo '{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"1"]},{"metric":{},"value":[1,"0"]}]}}' >"$dir/curl.out"
  assert_eq "query prints the number of series" "2" "$(run_helper "$dir" 'query up 1760000000' 2>&1 || true)"
  assert_contains "and asks for the time it is given" "$(cat "$dir/curl.args")" "time=1760000000"
  echo '{"status":"success","data":{"resultType":"vector","result":[]}}' >"$dir/curl.out"
  assert_eq "and 0 for an empty result" "0" "$(run_helper "$dir" 'query up' 2>&1 || true)"

  cat >"$dir/kubectl.out" <<'JSON'
{"items":[
  {"metadata":{"name":"pv-a"},"spec":{"claimRef":{"namespace":"monitoring","name":"prometheus-kube-prometheus-stack-prometheus-db-prometheus-kube-prometheus-stack-prometheus-0"}}},
  {"metadata":{"name":"pv-b"},"spec":{"claimRef":{"namespace":"openstack","name":"nfs-server-exports"}}},
  {"metadata":{"name":"pv-c"},"spec":{}}]}
JSON
  assert_eq "monitoring_pvs counts the one volume claimed in monitoring" "1" "$(run_helper "$dir" monitoring_pvs 2>&1 || true)"
  echo '{"items":[]}' >"$dir/kubectl.out"
  assert_eq "and prints 0 on an empty list" "0" "$(run_helper "$dir" monitoring_pvs 2>&1 || true)"
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
