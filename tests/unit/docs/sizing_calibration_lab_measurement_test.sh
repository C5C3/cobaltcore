#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the `## Lab measurement` section of
# docs/reference/testing/sizing-calibration.md (#1224):
#   1. the heading occurs once and is the last `## ` heading of the page, and
#      below it come `### What differs from CI`, `### Running the lab
#      measurement`, `### Reading the lab report` and `### Recorded lab run`,
#      in this order, and no other heading
#   2. two lines start with `hack/ci-vpa-recommendations.sh prepare`, the first
#      before the `watch` line; the `watch` and `snapshot` lines name
#      `openstack hypervisor-system`; the watch's kill, the second `prepare`,
#      the `snapshot`, the `report` and the two `kubectl delete vpa` lines
#      come in this order
#   3. each name comes from its source: the script's managed-by label, the
#      libvirtd scope's unit, the libvirt pods' app label, the three CR
#      names of deploy/lab/metal-stack/hypervisor/compute.yaml that the five
#      workload names of `verdicts` start with, the components the operators
#      append to them, and the libvirt DaemonSet's name; the fixture header is
#      the report header of the script, whose columns 3, 9, 12, 13, 16 and 17
#      are the ones the helpers read
#   4. the page holds none of the four run-sequence commands that
#      test_one_runbook of tests/unit/docs/quick_start_metal_stack_test.sh
#      keeps on the quick start alone
#   5. the helpers run from the page on a fixture of five rows: `floors`,
#      `floor_bounds`, `verdicts` and `over_request` print their lines, and
#      `libvirtd_scope` prints a node and its scope's readings per pod;
#      `floor_bounds` takes each resource's bound from its own workload, and
#      a target equal to its request is `confirmed` and no `over_request`
#      line, a CPU target above it `contradicted`
#   6. empty: on a TSV of the comment and header lines alone the four awk
#      helpers print nothing and exit 0, `pod_state` prints nothing for an
#      empty pod list, and `no_recommendation` nothing when every VPA has a
#      recommendation
#   7. absent: a row without requests gets `no default` and no over_request
#      line, `pod_state` leaves a terminating pod out, sorts its lines, prints
#      `-` without the vpaUpdates annotation and `-/-` for a container
#      without requests, and `no_recommendation` lists a VPA without a status
#   8. upstream error: `verdicts` on a missing TSV exits 2 and names the file,
#      `floor_bounds` then prints nothing, `pod_state` and `no_recommendation`
#      print no line when `kubectl` cannot reach the server, and
#      `libvirtd_scope` prints the node and the error text per pod when
#      `kubectl exec` fails
#   9. the record: `### Recorded lab run` holds one line that starts with
#      `The run of <YYYY-MM-DD>`, and the page no longer says that no lab run
#      is recorded. Run on the record's report table, `verdicts` prints its
#      verdict table, `floors` the rows of its floor table, `floor_bounds`
#      what its floors sentence names and `over_request` its
#      `over-request.log` block, and the VPAs it counts are the table's
#      workloads
#
# The jq parts of checks 6 to 8 count as one SKIP each without jq. The
# section runs from its heading to the end of the page, fenced code included.
# SIZING_CALIBRATION_DOC overrides the page.
#
# Usage: bash tests/unit/docs/sizing_calibration_lab_measurement_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

DOC="${SIZING_CALIBRATION_DOC:-$PROJECT_ROOT/docs/reference/testing/sizing-calibration.md}"

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

# The section: every line below `## Lab measurement`, fenced code included.
SECTION="$(awk '
  /^[[:space:]]*(```|~~~)/ { fenced = !fenced }
  inside { print; next }
  !fenced && $0 == "## Lab measurement" { inside = 1 }
' "$DOC")"

# line_of <fixed string> prints the number of the first line of the section
# that holds the string, or nothing.
line_of() {
  awk -v want="$1" 'index($0, want) { print NR; exit }' <<<"$SECTION"
}

# starting_with <prefix> prints the lines of the section that start with
# <prefix>.
starting_with() {
  awk -v prefix="$1" 'index($0, prefix) == 1' <<<"$SECTION"
}

# helper <name> prints the definition of the shell function <name> from the
# section: its first line alone when that line ends in `}`, and otherwise
# every line up to the next one that is `}` alone.
helper() {
  awk -v name="$1" '
    !f && index($0, name "() {") == 1 { f = 1; print; if ($0 ~ /}[[:space:]]*$/) exit; next }
    f { print; if ($0 == "}") exit }
  ' <<<"$SECTION"
}

HELPER_NAMES=(flat pod_state no_recommendation libvirtd_scope floors floor_bounds verdicts over_request)
HELPERS=""
for name in "${HELPER_NAMES[@]}"; do
  HELPERS+="$(helper "$name")"$'\n'
done

# run <dir> <commands> runs <commands> in bash with the page's helpers.
# `kubectl` is a stub that answers from <dir>/stub/<key>.err (printed to
# stderr, exit 1) or <dir>/stub/<key>.out, and <dir>/stub/all.err fails every
# call. The key is the kind of a `kubectl get <kind>`, `node-<pod>` for
# libvirtd_scope's `kubectl get -n <ns> pod/<pod>`, and the verb otherwise.
run() {
  bash -c '
    eval "$1"
    dir=$2
    kubectl() {
      local key f
      case "$1 $2" in
        "get -n") key="node-${4#pod/}" ;;
        get\ *) key="$2" ;;
        *) key="$1" ;;
      esac
      for f in "$dir/stub/all" "$dir/stub/$key"; do
        if [[ -f "$f.err" ]]; then cat "$f.err" >&2; return 1; fi
      done
      if [[ -f "$dir/stub/$key.out" ]]; then cat "$dir/stub/$key.out"; fi
    }
    eval "$3"
  ' _ "$HELPERS" "$1" "$2"
}

# stub_dir prints a fresh directory with an empty stub/ below it.
stub_dir() {
  local dir
  dir="$(mktemp -d "$TMP/case.XXXXXX")"
  mkdir "$dir/stub"
  echo "$dir"
}

# need_jq counts one SKIP and fails when jq is not on PATH.
need_jq() {
  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed, the jq helpers cannot run"
    SKIP=$((SKIP + 1))
    return 1
  fi
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

COLUMNS_LINE="$(printf '%s\t' namespace kind workload replicas owner_kind owner_name app component \
  container processes threads cpu_target_m memory_target_mi cpu_upper_m memory_upper_mi \
  cpu_request_m memory_request_mi memory_limit_mi)snapshot"

# tsv <file> <row>... writes a recommendations.tsv: the comment line, the
# header, and one row per argument, whose fields are separated by spaces.
tsv() {
  local file="$1" row
  shift
  {
    echo "# run=- attempt=- sha=abc123 job=lab leg=newforge recommender=-"
    echo "$COLUMNS_LINE"
    for row in "$@"; do
      tr ' ' '\t' <<<"$row"
    done
  } >"$file"
}

# The fixture of check 5, in the column order of the header.
OVS_VSWITCHD='openstack DaemonSet lab-chassis-ovs - OVNChassis lab-chassis ovn ovs ovs-vswitchd - - 23 40 100 200 - - - 2026-10-05T12:00:00Z'
OVSDB_SERVER='openstack DaemonSet lab-chassis-ovs - OVNChassis lab-chassis ovn ovs ovsdb-server - - 11 11 50 50 - - - 2026-10-05T12:00:00Z'
AGENT='openstack DaemonSet lab-metadata-agent-metadata-agent - NeutronMetadataAgent lab-metadata-agent neutron metadata-agent metadata-agent - - 23 2100 100 4000 70 2048 2048 2026-10-05T12:00:00Z'
LIBVIRTD='openstack DaemonSet libvirt - - - libvirt - libvirtd - - 11 11 50 50 100 256 512 2026-10-05T12:00:00Z'
PLACEMENT='openstack Deployment controlplane-placement 1 Placement controlplane-placement placement api placement-api 1 1 11 400 50 800 15 368 368 2026-10-05T12:00:00Z'

# --- Test 1: the section's place and headings ---
test_position() {
  echo "Test: '## Lab measurement' is the last '## ' heading, with its four subsections in order"
  local all count last below
  all="$(headings)"
  count="$(grep -cxE '[0-9]+:## Lab measurement' <<<"$all" || true)"
  assert_eq "'## Lab measurement' occurs once" "1" "$count"
  last="$({ grep -E '^[0-9]+:## ' <<<"$all" || true; } | tail -n 1 | cut -d: -f2-)"
  assert_eq "it is the last '## ' heading" "## Lab measurement" "$last"
  below="$(awk -F: '$2 == "## Lab measurement" { found = 1; next } found { sub(/^[0-9]+:/, ""); print }' <<<"$all")"
  assert_eq "the headings below it are the four subsections, in order" \
    "### What differs from CI|### Running the lab measurement|### Reading the lab report|### Recorded lab run" \
    "$(paste -sd '|' <<<"$below")"
}

# --- Test 2: the measurement's command lines and their order ---
test_command_order() {
  echo "Test: prepare runs before the watch and again after it, and the VPAs go last"
  local prepare first watch kill second snapshot report del_os del_hs
  prepare="$(starting_with 'hack/ci-vpa-recommendations.sh prepare')"
  assert_eq "two lines start with 'hack/ci-vpa-recommendations.sh prepare'" "2" "$(grep -c . <<<"$prepare" || true)"
  first="$(awk 'index($0, "hack/ci-vpa-recommendations.sh prepare") == 1 { print NR; exit }' <<<"$SECTION")"
  second="$(awk 'index($0, "hack/ci-vpa-recommendations.sh prepare") == 1 { n++; if (n == 2) { print NR; exit } }' <<<"$SECTION")"
  watch="$(line_of 'hack/ci-vpa-recommendations.sh watch')"
  kill="$(line_of 'kill "$(cat "$out/watch.pid")"')"
  snapshot="$(line_of 'hack/ci-vpa-recommendations.sh snapshot')"
  report="$(line_of 'hack/ci-vpa-recommendations.sh report')"
  del_os="$(line_of 'kubectl delete vpa -n openstack -l app.kubernetes.io/managed-by=')"
  del_hs="$(line_of 'kubectl delete vpa -n hypervisor-system -l app.kubernetes.io/managed-by=')"
  assert_contains "the watch line names both namespaces" "$(sed -n "${watch:-0}p" <<<"$SECTION")" \
    'watch "$out" openstack hypervisor-system'
  assert_contains "the snapshot line names both namespaces" "$(sed -n "${snapshot:-0}p" <<<"$SECTION")" \
    'snapshot "$out" openstack hypervisor-system'
  if [[ -n "$first" && -n "$watch" ]] && ((first < watch)); then
    echo "  PASS: the first prepare (line $first) precedes the watch (line $watch)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the first prepare ('${first}') must precede the watch ('${watch}')"
    FAIL=$((FAIL + 1))
  fi
  if [[ -n "$kill" && -n "$second" && -n "$snapshot" && -n "$report" && -n "$del_os" && -n "$del_hs" ]] &&
    ((kill < second && second < snapshot && snapshot < report && report < del_os && del_os < del_hs)); then
    echo "  PASS: kill, prepare, snapshot, report and the two deletes follow each other"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the end of the measurement is out of order (kill '${kill}', prepare '${second}', snapshot '${snapshot}', report '${report}', deletes '${del_os}' '${del_hs}')"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 3: the names come from their sources ---
# A renamed label, unit, CR, component or report column fails here before the
# block reads nothing or the wrong column.
test_name_sources() {
  echo "Test: each name the section uses is the one its source sets"
  local hypervisor="$PROJECT_ROOT/deploy/lab/metal-stack/hypervisor"
  local verdicts_helper
  assert_file_contains_fixed "the script labels its VPAs managed-by ci-vpa-recommendations" \
    "$PROJECT_ROOT/hack/ci-vpa-recommendations.sh" 'MANAGED_BY="ci-vpa-recommendations"'
  assert_contains "the section selects the VPAs by that label" "$SECTION" \
    'app.kubernetes.io/managed-by=ci-vpa-recommendations'
  assert_file_contains_fixed "libvirtd runs in the scope cobaltcore-libvirtd" \
    "$hypervisor/libvirt-configmap.yaml" '--unit=cobaltcore-libvirtd'
  assert_contains "the section reads that scope" "$SECTION" \
    '/sys/fs/cgroup/system.slice/cobaltcore-libvirtd.scope'
  assert_file_contains_fixed "the libvirt pods carry app.kubernetes.io/name: libvirt" \
    "$hypervisor/libvirt-daemonset.yaml" 'app.kubernetes.io/name: libvirt'
  assert_contains "the section selects the libvirt pods by that label" "$SECTION" \
    'kubectl get pod -n openstack -l app.kubernetes.io/name=libvirt -o name'
  local name
  for name in lab lab-chassis lab-metadata-agent; do
    if grep -qxF "  name: $name" "$hypervisor/compute.yaml"; then
      echo "  PASS: compute.yaml names a CR '$name'"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: compute.yaml names no CR '$name'"
      FAIL=$((FAIL + 1))
    fi
  done
  verdicts_helper="$(helper verdicts)"
  assert_contains "verdicts picks the five compute workloads" "$verdicts_helper" \
    '/^(lab-nova-compute|lab-chassis-ovn-controller|lab-chassis-ovs|lab-metadata-agent-metadata-agent|libvirt)$/'
  # The operators name a DaemonSet <CR name>-<component>: the name function's
  # return line and the component's constant.
  local entry
  for entry in \
    'nova/internal/controller/reconcile_novacompute_daemonset.go|return cr.Name + "-" + novaComputeComponent' \
    'nova/internal/controller/novacompute_controller.go|const novaComputeComponent = "nova-compute"' \
    'ovn/internal/controller/reconcile_controller.go|return cr.Name + "-" + componentOVNController' \
    'ovn/internal/controller/reconcile_controller.go|const componentOVNController = "ovn-controller"' \
    'ovn/internal/controller/reconcile_ovs.go|return cr.Name + "-" + componentOVS' \
    'ovn/internal/controller/reconcile_ovs.go|const componentOVS = "ovs"' \
    'neutron/internal/controller/reconcile_daemonset.go|return cr.Name + "-" + metadataAgentComponent' \
    'neutron/internal/controller/neutronmetadataagent_controller.go|const metadataAgentComponent = "metadata-agent"'; do
    assert_file_contains_fixed "operators/${entry%%|*} holds '${entry#*|}'" \
      "$PROJECT_ROOT/operators/${entry%%|*}" "${entry#*|}"
  done
  assert_eq "the libvirt DaemonSet is named libvirt" "libvirt" \
    "$(awk '/^kind: DaemonSet$/ { k = 1 } k && /^  name: / { print $2; exit }' "$hypervisor/libvirt-daemonset.yaml")"
  # The helpers read the report by position, and the fixtures follow the
  # header the script writes.
  local script_cols pair
  script_cols="$(awk '/\| \(\["namespace", "kind"/,/as \$cols/' "$PROJECT_ROOT/hack/ci-vpa-recommendations.sh" |
    { grep -o '"[a-z_]*"' || true; } | tr -d '"' | paste -sd ' ')"
  assert_eq "the fixture header is the report header of the script" "$script_cols" "$(tr '\t' ' ' <<<"$COLUMNS_LINE")"
  for pair in 3:workload 9:container 12:cpu_target_m 13:memory_target_mi 16:cpu_request_m 17:memory_request_mi; do
    assert_eq "column ${pair%%:*} of the report is ${pair#*:}" "${pair#*:}" "$(cut -d' ' -f"${pair%%:*}" <<<"$script_cols")"
  done
}

# --- Test 4: the run sequence stays on the quick start ---
# The four commands are the ones test_one_runbook in
# tests/unit/docs/quick_start_metal_stack_test.sh keeps on the quick start.
test_no_run_sequence() {
  echo "Test: the page repeats none of the quick start's run-sequence commands"
  local command
  for command in \
    'kubectl apply -k deploy/lab/metal-stack/controlplane' \
    'kubectl apply -k deploy/lab/metal-stack/hypervisor-fixtures' \
    'server create "lab-${i}"' \
    'server add volume lab-0 lab-vol'; do
    assert_eq "'$command' does not occur on the page" "0" "$(grep -cF -- "$command" "$DOC" || true)"
  done
}

# --- Test 5: the helpers on the five-row fixture ---
test_helpers() {
  echo "Test: the helpers print the floors, the bounds, the verdicts and the rows above their request"
  local name dir out
  for name in "${HELPER_NAMES[@]}"; do
    assert_not_empty "the section defines $name" "$(helper "$name")"
  done
  dir="$(stub_dir)"
  tsv "$dir/r.tsv" "$OVS_VSWITCHD" "$OVSDB_SERVER" "$AGENT" "$LIBVIRTD" "$PLACEMENT"

  out="$(run "$dir" 'floors "$dir/r.tsv"' 2>&1 || true)"
  assert_eq "floors prints one line per workload" "4" "$(grep -c . <<<"$out" || true)"
  assert_contains "floors bounds lab-chassis-ovs by its two containers" "$out" \
    '24 22 2 openstack/DaemonSet/lab-chassis-ovs'
  assert_eq "floor_bounds prints the smallest bounds" "floor bounds: cpu below 12m, memory at most 11Mi" \
    "$(run "$dir" 'floor_bounds "$dir/r.tsv"' 2>&1 || true)"
  tsv "$dir/split.tsv" \
    'openstack Deployment a 1 - - a - a - - 11 300 50 600 15 368 368 2026-10-05T12:00:00Z' \
    'openstack Deployment b 1 - - b - b - - 40 11 50 50 15 368 368 2026-10-05T12:00:00Z'
  assert_eq "floor_bounds takes each resource's bound from its own workload" \
    "floor bounds: cpu below 12m, memory at most 11Mi" \
    "$(run "$dir" 'floor_bounds "$dir/split.tsv"' 2>&1 || true)"

  out="$(run "$dir" 'verdicts "$dir/r.tsv"' 2>&1 || true)"
  assert_eq "verdicts prints the four rows of the compute workloads" \
    "lab-chassis-ovs ovs-vswitchd cpu 23m request -: no default; memory 40Mi request -: no default
lab-chassis-ovs ovsdb-server cpu 11m request -: no default; memory 11Mi request -: no default
lab-metadata-agent-metadata-agent metadata-agent cpu 23m request 70: confirmed; memory 2100Mi request 2048: contradicted
libvirt libvirtd cpu 11m request 100: confirmed; memory 11Mi request 256: confirmed" "$out"

  assert_eq "over_request prints the agent's and the Placement row" \
    "openstack lab-metadata-agent-metadata-agent metadata-agent memory target 2100Mi above request 2048Mi
openstack controlplane-placement placement-api memory target 400Mi above request 368Mi" \
    "$(run "$dir" 'over_request "$dir/r.tsv"' 2>&1 || true)"
  tsv "$dir/boundary.tsv" \
    'openstack DaemonSet lab-nova-compute - NovaCompute lab nova nova-compute nova-compute - - 126 256 300 600 70 256 256 2026-10-05T12:00:00Z'
  assert_eq "verdicts contradicts a CPU target above its request and confirms a memory target equal to it" \
    "lab-nova-compute nova-compute cpu 126m request 70: contradicted; memory 256Mi request 256: confirmed" \
    "$(run "$dir" 'verdicts "$dir/boundary.tsv"' 2>&1 || true)"
  assert_eq "over_request prints no line for a memory target equal to its request" "" \
    "$(run "$dir" 'over_request "$dir/boundary.tsv"' 2>&1 || true)"

  printf 'pod/libvirt-a\npod/libvirt-b\n' >"$dir/stub/pod.out"
  echo worker-a >"$dir/stub/node-libvirt-a.out"
  echo worker-b >"$dir/stub/node-libvirt-b.out"
  printf '73400320\n104857600\nusage_usec 4200000\n' >"$dir/stub/exec.out"
  assert_eq "libvirtd_scope prints each node with its scope's readings" \
    "worker-a 73400320 104857600 usage_usec 4200000
worker-b 73400320 104857600 usage_usec 4200000" \
    "$(run "$dir" 'libvirtd_scope' 2>&1 || true)"
}

# --- Test 6: empty input ---
test_empty() {
  echo "Test: the helpers print nothing for an empty report, an empty pod list and recommended VPAs"
  local dir name out rc
  dir="$(stub_dir)"
  tsv "$dir/r.tsv"
  for name in floors floor_bounds verdicts over_request; do
    rc=0
    out="$(run "$dir" "$name \"\$dir/r.tsv\"" 2>&1)" || rc=$?
    assert_eq "$name prints nothing for the comment and header lines alone" "" "$out"
    assert_eq "$name exits 0 on them" "0" "$rc"
  done
  need_jq || return 0
  printf '%s' '{"items":[]}' >"$dir/stub/pods.out"
  assert_eq "pod_state prints nothing for an empty pod list" "" \
    "$(run "$dir" 'pod_state openstack' 2>&1 || true)"
  printf '%s' '{"items":[{"metadata":{"name":"sizing-daemonset-libvirt"},"status":{"recommendation":{"containerRecommendations":[{"containerName":"libvirtd","target":{"cpu":"11m","memory":"11500000"}}]}}}]}' \
    >"$dir/stub/vpa.out"
  assert_eq "no_recommendation prints nothing when every VPA has a recommendation" "" \
    "$(run "$dir" 'no_recommendation openstack' 2>&1 || true)"
}

# --- Test 7: absent fields ---
test_absent() {
  echo "Test: absent requests, annotations and status read as '-', 'no default' and a listed VPA"
  local dir
  dir="$(stub_dir)"
  tsv "$dir/r.tsv" 'openstack DaemonSet lab-nova-compute - NovaCompute lab nova nova-compute nova-compute - - 93 175 300 600 - - - 2026-10-05T12:00:00Z'
  assert_eq "verdicts gives a row without requests 'no default' twice" \
    "lab-nova-compute nova-compute cpu 93m request -: no default; memory 175Mi request -: no default" \
    "$(run "$dir" 'verdicts "$dir/r.tsv"' 2>&1 || true)"
  assert_eq "over_request prints no line for it" "" "$(run "$dir" 'over_request "$dir/r.tsv"' 2>&1 || true)"
  need_jq || return 0
  printf '%s' '{"items":[
    {"metadata":{"name":"p-2","uid":"u-2","annotations":{"vpaUpdates":"Pod resources updated by v: container 0: cpu request"}},
     "spec":{"containers":[{"name":"a","resources":{"requests":{"cpu":"15m","memory":"368Mi"}}}]},
     "status":{"containerStatuses":[{"restartCount":2}]}},
    {"metadata":{"name":"p-0","uid":"u-0","deletionTimestamp":"2026-10-05T12:00:00Z"},"spec":{"containers":[{"name":"c"}]}},
    {"metadata":{"name":"p-1","uid":"u-1"},"spec":{"containers":[{"name":"c"}]}}]}' >"$dir/stub/pods.out"
  assert_eq "pod_state leaves a terminating pod out, sorts, and prints - without the annotation and -/- without requests" \
    "$(printf 'openstack\tp-1\tu-1\t0\t-\tc=-/-\nopenstack\tp-2\tu-2\t2\tPod resources updated by v: container 0: cpu request\ta=15m/368Mi')" \
    "$(run "$dir" 'pod_state openstack' 2>&1 || true)"
  printf '%s' '{"items":[{"metadata":{"name":"sizing-daemonset-a"}},
    {"metadata":{"name":"sizing-deployment-b"},"status":{"conditions":[{"type":"NoPodsMatched","status":"True"}]}}]}' \
    >"$dir/stub/vpa.out"
  assert_eq "no_recommendation lists a VPA without a status and one without a recommendation" \
    "$(printf 'openstack\tsizing-daemonset-a\t\nopenstack\tsizing-deployment-b\tNoPodsMatched=True')" \
    "$(run "$dir" 'no_recommendation openstack' 2>&1 || true)"
}

# --- Test 8: upstream errors ---
test_upstream_error() {
  echo "Test: a missing report, an unreachable server and a failing exec"
  local dir rc out
  dir="$(stub_dir)"
  rc=0
  run "$dir" 'verdicts "$dir/missing.tsv"' >"$dir/out" 2>"$dir/err" || rc=$?
  assert_eq "verdicts on a missing TSV exits 2" "2" "$rc"
  assert_contains "awk's message names the file" "$(cat "$dir/err")" "$dir/missing.tsv"
  assert_eq "floor_bounds prints nothing for a missing TSV" "" \
    "$(run "$dir" 'floor_bounds "$dir/missing.tsv"' 2>/dev/null || true)"
  need_jq || return 0
  echo 'Unable to connect to the server: dial tcp 192.0.2.1:443: i/o timeout' >"$dir/stub/all.err"
  assert_eq "pod_state prints no line when kubectl cannot reach the server" "" \
    "$(run "$dir" 'pod_state openstack hypervisor-system' 2>/dev/null || true)"
  assert_eq "no_recommendation prints no line when kubectl cannot reach the server" "" \
    "$(run "$dir" 'no_recommendation openstack hypervisor-system' 2>/dev/null || true)"
  dir="$(stub_dir)"
  printf 'pod/libvirt-a\npod/libvirt-b\n' >"$dir/stub/pod.out"
  echo worker-a >"$dir/stub/node-libvirt-a.out"
  echo worker-b >"$dir/stub/node-libvirt-b.out"
  echo 'sh: cd: line 0: can'"'"'t cd to /sys/fs/cgroup/system.slice/cobaltcore-libvirtd.scope: No such file or directory' \
    >"$dir/stub/exec.err"
  out="$(run "$dir" 'libvirtd_scope' 2>&1 || true)"
  assert_eq "libvirtd_scope prints one line per pod" "2" "$(grep -c . <<<"$out" || true)"
  assert_contains "the first line holds the node and the error text" "$(sed -n 1p <<<"$out")" \
    "worker-a sh: cd: line 0: can't cd to /sys/fs/cgroup/system.slice/cobaltcore-libvirtd.scope: No such file or directory"
  assert_starts_with "the second line holds its own node" "$(sed -n 2p <<<"$out")" "worker-b sh: cd:"
}

# --- Test 9: the record of the lab run ---
test_record() {
  echo "Test: '### Recorded lab run' holds the record of one run, whose figures the helpers print"
  local record
  record="$(awk '
    /^[[:space:]]*(```|~~~)/ { fenced = !fenced }
    inside { print; next }
    !fenced && $0 == "### Recorded lab run" { inside = 1 }
  ' <<<"$SECTION")"
  assert_eq "one line of the subsection starts with 'The run of <date>'" "1" \
    "$(grep -cE '^The run of 20[0-9]{2}-[0-9]{2}-[0-9]{2}' <<<"$record" || true)"
  assert_eq "the page no longer says 'No lab run is recorded yet.'" "0" \
    "$(grep -cF 'No lab run is recorded yet.' "$DOC" || true)"

  # The record's figures, recomputed by the helpers from its report table.
  local report floor_rows row
  report="$TMP/record.tsv"
  assert_eq "the record's report table has the columns of the header" "$(tr '\t' ' ' <<<"$COLUMNS_LINE")" \
    "$(awk '/^\| namespace \|/ { sub(/^\| /, ""); sub(/ \|$/, ""); gsub(/ \| /, " "); print; exit }' <<<"$record")"
  {
    echo '# record'
    echo "$COLUMNS_LINE"
    awk '/^\| (hypervisor-system|openstack) \|/ { sub(/^\| /, ""); sub(/ \|$/, ""); gsub(/ \| /, "\t"); print }' <<<"$record"
  } >"$report"
  assert_eq "the verdict table is what verdicts prints" \
    "$(run "$TMP" 'verdicts "$dir/record.tsv"' 2>&1 || true)" \
    "$(awk '
      function q(v) { gsub(/`/, "", v); if (v == "none") return "-"; if (v ~ /Gi$/) return v * 1024; sub(/(m|Mi)$/, "", v); return v }
      /^\| / { sub(/^\| /, ""); sub(/ \|$/, "") }
      split($0, f, / \| /) == 8 && f[3] ~ /^[0-9]+$/ {
        gsub(/`/, "", f[1]); gsub(/`/, "", f[2]); split(f[8], v, /; /)
        printf "%s %s cpu %sm request %s: %s; memory %sMi request %s: %s\n", f[1], f[2], f[3], q(f[5]), v[1], f[4], q(f[6]), v[2] }
    ' <<<"$record")"
  floor_rows="$(awk '
    /^\| / { sub(/^\| /, ""); sub(/ \|$/, "") }
    split($0, f, / \| /) == 6 && f[2] ~ /^[0-9]+$/ { gsub(/`/, "", f[1]); gsub(/[^0-9]/, "", f[5]); gsub(/[^0-9]/, "", f[6]); print f[5], f[6], f[2], f[1] }
  ' <<<"$record")"
  assert_not_empty "the record has a floor table" "$floor_rows"
  while IFS= read -r row; do
    assert_eq "floors prints the floor table's row '$row'" "1" \
      "$(run "$TMP" 'floors "$dir/record.tsv"' 2>&1 | grep -cxF -- "$row" || true)"
  done <<<"$floor_rows"
  assert_contains "the floors sentence names what floor_bounds prints" "$record" \
    "\`floors.log\` ends in \`$(run "$TMP" 'floor_bounds "$dir/record.tsv"' 2>&1 || true)\`"
  assert_eq "the over-request.log block is what over_request prints" \
    "$(run "$TMP" 'over_request "$dir/record.tsv"' 2>&1 || true)" \
    "$(awk '/^```text$/ { f = 1; next } f && /^```$/ { exit } f' <<<"$record")"
  assert_contains "the record counts one VPA per workload of the report" "$record" \
    "each of the $(tail -n +3 "$report" | cut -f1-3 | sort -u | wc -l | tr -d ' ') VPAs"
}

test_position
test_command_order
test_name_sources
test_no_run_sequence
test_helpers
test_empty
test_absent
test_upstream_error
test_record

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
