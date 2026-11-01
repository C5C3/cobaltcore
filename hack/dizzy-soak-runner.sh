#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/dizzy-soak-runner.sh — The runner of the dizzy soak (#1274), the
# command of the container runner of deploy/lab/metal-stack/dizzy-soak/job.yaml.
# hack/dizzy-soak.sh start ships it as the ConfigMap dizzy-soak-runner.
#
# It names the run after its UTC start and works in /reports/<run>/ on the
# claim dizzy-soak-reports. It starts `dizzy <service> chaos` in the
# background and samples the platform beside it: the containers of every pod
# of the platform's namespaces and their usage, and the Go runtime of every
# operator pod from its /metrics endpoint, every DIZZY_SOAK_SAMPLE_INTERVAL
# seconds, and the conditions of the ControlPlanes and the CobaltCore service
# kinds at the start and at the end. A TERM or INT goes to dizzy once; dizzy
# then tears its resources down and prints its leak check, and the runner
# waits for it. Then it renders dizzy's report, derives the platform tables,
# decides the verdict and writes report.md. It exits 0 on PASS and 1 on FAIL,
# so the Job ends Complete or Failed by verdict.
#
# Its settings come from the ConfigMap dizzy-soak-config: DIZZY_SOAK_SERVICE,
# DIZZY_SOAK_DURATION, DIZZY_ARGS, DIZZY_VERSION, DIZZY_SOAK_NAMESPACES,
# DIZZY_SOAK_SAMPLE_INTERVAL, DIZZY_SOAK_MAX_ERROR_RATE,
# DIZZY_SOAK_MAX_P95_SECONDS, DIZZY_SOAK_MAX_OPERATOR_HEAP_GROWTH_PERCENT,
# DIZZY_SOAK_MAX_OPERATOR_GOROUTINE_GROWTH,
# DIZZY_SOAK_MAX_MEMORY_GROWTH_PERCENT, DIZZY_SOAK_MAX_MEMORY_GROWTH_MIB and
# DIZZY_SOAK_MEMORY_GROWTH_SKIP_NAMESPACES.
# It parses under bash 3.2 and
# aggregates in jq and POSIX awk, because
# tests/unit/hack/dizzy_soak_runner_test.sh sources it under the stock macOS
# bash too.

# No set -e: a read that fails ends in platform/errors.log or in the detail
# of a check, never in a report that is not written.
set -uo pipefail

# awk prints numbers with a decimal point, and sort orders bytes, whatever
# the locale of the shell.
export LC_ALL=C

# ---------------------------------------------------------------------------
# The paths of the pod and the run's constants. Plain variables, so the unit
# test can point them elsewhere after sourcing the script.
# ---------------------------------------------------------------------------
REPORTS_DIR="/reports"
DIZZY_BIN="/tools/dizzy"
SCENARIO_FILE="/scenario/scenario.yaml"
CLOUDS_FILE="/etc/openstack/clouds.yaml"
# 512 MiB: a run of six hours writes a few MB, an unbounded one about 45 MB a
# day on a cluster of 150 containers.
MIN_FREE_KIB=524288
# The OTLP metrics endpoint of the dizzy VictoriaMetrics in the cluster, the
# Service the Grafana datasource of deploy/kind/dizzy/release-grafana.yaml
# reads.
OTLP_ENDPOINT="http://dizzy-victoria-metrics-server.dizzy.svc:8428/opentelemetry/v1/metrics"
# The namespaces an empty DIZZY_SOAK_NAMESPACES leaves out.
EXCLUDED_NAMESPACES="kube-system kube-public kube-node-lease default dizzy"
# The kinds whose status.conditions the runner reads at the start and the end.
CONDITION_KINDS="controlplanes.c5c3.io
keystones.keystone.openstack.c5c3.io
glances.glance.openstack.c5c3.io
placements.placement.openstack.c5c3.io
barbicans.barbican.openstack.c5c3.io
horizons.horizon.openstack.c5c3.io
neutrons.neutron.openstack.c5c3.io
neutronmetadataagents.neutron.openstack.c5c3.io
novas.nova.openstack.c5c3.io
novacomputes.nova.openstack.c5c3.io
cinders.cinder.openstack.c5c3.io
ovncentrals.ovn.openstack.c5c3.io
ovnchassis.ovn.openstack.c5c3.io"
# The lines dizzy prints when its leak check finds nothing of the run:
# run-tagged resources (mix, nova, neutron, cinder), run-tagged images
# (glance) and run-named resources (keystone).
LEAK_CHECK_CLEAN_RE='^leak check: no run-(tagged|named) (resources|images) remain$'
# The chart names, the label app.kubernetes.io/name, of the operator pods.
OPERATOR_NAMES="keystone-operator horizon-operator glance-operator placement-operator barbican-operator ovn-operator neutron-operator cinder-operator nova-operator c5c3-operator"
# The samples an operator pod needs before operator-leak judges it.
MIN_OPERATOR_SAMPLES=20
# The seconds one scrape of an operator pod may take, curl's --max-time.
OPERATOR_SCRAPE_TIMEOUT=10
# The seconds of it the connection may take, curl's --connect-timeout.
OPERATOR_CONNECT_TIMEOUT=3
# The memory samples a container needs before memory-growth judges it.
MIN_MEMORY_SAMPLES=20
# The pods no-restarts, operator-leak and memory-growth leave out, one
# <namespace>/<pod> per line. A soak judges every pod; hack/chaos-random.sh
# sets it after sourcing the runner, to the targets of its faults, when it
# judges a soak's tables again.
EXEMPT_PODS=""

# ---------------------------------------------------------------------------
# Settings, from the ConfigMap dizzy-soak-config.
# ---------------------------------------------------------------------------
DIZZY_SOAK_SERVICE="${DIZZY_SOAK_SERVICE:-mix}"
DIZZY_SOAK_DURATION="${DIZZY_SOAK_DURATION:-6h}"
DIZZY_ARGS="${DIZZY_ARGS:-}"
DIZZY_VERSION="${DIZZY_VERSION:-}"
DIZZY_SOAK_NAMESPACES="${DIZZY_SOAK_NAMESPACES:-}"
DIZZY_SOAK_SAMPLE_INTERVAL="${DIZZY_SOAK_SAMPLE_INTERVAL:-60}"
DIZZY_SOAK_MAX_ERROR_RATE="${DIZZY_SOAK_MAX_ERROR_RATE:-}"
DIZZY_SOAK_MAX_P95_SECONDS="${DIZZY_SOAK_MAX_P95_SECONDS:-}"
DIZZY_SOAK_MAX_OPERATOR_HEAP_GROWTH_PERCENT="${DIZZY_SOAK_MAX_OPERATOR_HEAP_GROWTH_PERCENT:-10}"
DIZZY_SOAK_MAX_OPERATOR_GOROUTINE_GROWTH="${DIZZY_SOAK_MAX_OPERATOR_GOROUTINE_GROWTH:-10}"
DIZZY_SOAK_MAX_MEMORY_GROWTH_PERCENT="${DIZZY_SOAK_MAX_MEMORY_GROWTH_PERCENT:-10}"
DIZZY_SOAK_MAX_MEMORY_GROWTH_MIB="${DIZZY_SOAK_MAX_MEMORY_GROWTH_MIB:-16}"
# The namespaces memory-growth does not judge, space-separated; empty judges
# every namespace. flux-system by default: its source-controller refreshes
# every source each hour and its helm-controller reconciles every HelmRelease
# each half hour, and the working set of both jumps for a few minutes each
# time (source-controller from 54 to 255 MiB on a kind cluster). A refresh in
# the last tenth reads as growth.
DIZZY_SOAK_MEMORY_GROWTH_SKIP_NAMESPACES="${DIZZY_SOAK_MEMORY_GROWTH_SKIP_NAMESPACES-flux-system}"

# ---------------------------------------------------------------------------
# The state of the run.
# ---------------------------------------------------------------------------
RUN_NAME=""
RUN_START=""
DIZZY_PID=""
DIZZY_RC=""
TEE_PID=""
DIZZY_FIFO=""
SAMPLER_PID=""
OPERATOR_PODS=""
WAIT_RC=""
STOP_REQUESTED=""
TERM_SENT=""

# ---------------------------------------------------------------------------
# log — Print a timestamped log message (ISO 8601 UTC).
# ---------------------------------------------------------------------------
log() {
  echo "[$(now)] $*"
}

# now — The current UTC time as an RFC 3339 timestamp, the form of the
# timestamps of the Kubernetes API, so the two compare as strings.
now() {
  date -u '+%Y-%m-%dT%H:%M:%SZ'
}

# one_line <text> — <text> on one line, for platform/errors.log.
one_line() {
  printf '%s' "$1" | tr '\n' ' ' | sed 's/[[:space:]]*$//'
}

# record_error <source> <message> — Append a line to platform/errors.log.
record_error() {
  printf '%s %s: %s\n' "$(now)" "$1" "$(one_line "$2")" >>platform/errors.log
}

# namespace_filter — The jq definition of keep, which selects an object of a
# platform namespace: one DIZZY_SOAK_NAMESPACES lists or, with that setting
# empty, one EXCLUDED_NAMESPACES does not.
namespace_filter() {
  # shellcheck disable=SC2016 # jq variables, bound by the callers
  printf '%s' 'def keep: .metadata.namespace as $n |
    if ($listed | length) > 0 then ($n | IN($listed[])) else ($n | IN($excluded[]) | not) end;'
}

# words_json <words> — A JSON array of the space-separated words.
words_json() {
  jq -cn --arg s "$1" '$s | split(" ") | map(select(. != ""))'
}

# ---------------------------------------------------------------------------
# sample_pods — Append one line per container of the platform's pods to
# platform/containers.tsv: timestamp, namespace, pod, pod UID, pod creation
# time, owner kind, container, restartCount, last terminated reason and its
# finishedAt, with - for an absent value. A read that succeeds appends its
# timestamp to platform/pod-samples.log, also when it lists no container.
# It sets OPERATOR_PODS to one line per Running operator pod with a pod IP
# and a container port named metrics: timestamp, namespace, pod, creation
# time, pod IP and that port, and appends them to platform/operator-pods.tsv.
# ---------------------------------------------------------------------------
sample_pods() {
  local ts out err lines operators
  OPERATOR_PODS=""
  ts="$(now)"
  err="$(mktemp)"
  if ! out="$(kubectl get pods -A -o json 2>"${err}")"; then
    record_error pods "$(cat "${err}")"
    rm -f "${err}"
    return 1
  fi
  rm -f "${err}"
  if ! lines="$(printf '%s' "${out}" | jq -r --arg ts "${ts}" \
      --argjson listed "$(words_json "${DIZZY_SOAK_NAMESPACES}")" \
      --argjson excluded "$(words_json "${EXCLUDED_NAMESPACES}")" \
      "$(namespace_filter)"'
      .items[] | select(keep) | . as $p
      | (($p.status.initContainerStatuses // []) + ($p.status.containerStatuses // []))[]
      | [$ts, $p.metadata.namespace, $p.metadata.name, $p.metadata.uid,
         $p.metadata.creationTimestamp, ($p.metadata.ownerReferences[0].kind // "-"),
         .name, (.restartCount | tostring), (.lastState.terminated.reason // "-"),
         (.lastState.terminated.finishedAt // "-")]
      | @tsv' 2>&1)"; then
    record_error pods "cannot parse the pod list: ${lines}"
    return 1
  fi
  if ! operators="$(printf '%s' "${out}" | jq -r --arg ts "${ts}" \
      --argjson listed "$(words_json "${DIZZY_SOAK_NAMESPACES}")" \
      --argjson excluded "$(words_json "${EXCLUDED_NAMESPACES}")" \
      --argjson operators "$(words_json "${OPERATOR_NAMES}")" \
      "$(namespace_filter)"'
      .items[] | select(keep)
      | select(.status.phase == "Running" and (.status.podIP // "") != ""
          and ((.metadata.labels["app.kubernetes.io/name"] // "") | IN($operators[])))
      | ([.spec.containers[]?.ports[]? | select(.name == "metrics") | .containerPort] | first) as $port
      | select($port != null)
      | [$ts, .metadata.namespace, .metadata.name, .metadata.creationTimestamp, .status.podIP, ($port | tostring)]
      | @tsv' 2>&1)"; then
    record_error pods "cannot parse the pod list: ${operators}"
    return 1
  fi
  if [[ -n "${lines}" ]]; then
    printf '%s\n' "${lines}" >>platform/containers.tsv
  fi
  if [[ -n "${operators}" ]]; then
    OPERATOR_PODS="${operators}"
    printf '%s\n' "${operators}" >>platform/operator-pods.tsv
  fi
  printf '%s\n' "${ts}" >>platform/pod-samples.log
}

# ---------------------------------------------------------------------------
# sample_usage — Append one line per container of the platform's pods from
# the metrics API to platform/usage.tsv: timestamp, namespace, pod,
# container, CPU in millicores and memory in bytes. A quantity of another
# form is logged to platform/errors.log and its line left out.
# ---------------------------------------------------------------------------
sample_usage() {
  local ts out err lines
  ts="$(now)"
  err="$(mktemp)"
  if ! out="$(kubectl get --raw /apis/metrics.k8s.io/v1beta1/pods 2>"${err}")"; then
    record_error usage "$(cat "${err}")"
    rm -f "${err}"
    return 1
  fi
  rm -f "${err}"
  if ! lines="$(printf '%s' "${out}" | jq -r --arg ts "${ts}" \
      --argjson listed "$(words_json "${DIZZY_SOAK_NAMESPACES}")" \
      --argjson excluded "$(words_json "${EXCLUDED_NAMESPACES}")" \
      "$(namespace_filter)"'
      .items[] | select(keep) | . as $p | .containers[]
      | [$ts, $p.metadata.namespace, $p.metadata.name, .name, .usage.cpu, .usage.memory]
      | @tsv' 2>&1)"; then
    record_error usage "cannot parse the pod metrics: ${lines}"
    return 1
  fi
  if [[ -n "${lines}" ]]; then
    printf '%s\n' "${lines}" | awk -F '\t' -v OFS='\t' -v errors=platform/errors.log '
      function strip(q, n) { return substr(q, 1, length(q) - n) }
      function millicores(q) {
        if (q ~ /^[0-9]+(\.[0-9]+)?n$/) return strip(q, 1) / 1000000
        if (q ~ /^[0-9]+(\.[0-9]+)?u$/) return strip(q, 1) / 1000
        if (q ~ /^[0-9]+(\.[0-9]+)?m$/) return strip(q, 1) + 0
        if (q ~ /^[0-9]+(\.[0-9]+)?$/) return q * 1000
        quantity_error = 1
        return 0
      }
      function bytes(q) {
        if (q ~ /^[0-9]+(\.[0-9]+)?Ki$/) return strip(q, 2) * 1024
        if (q ~ /^[0-9]+(\.[0-9]+)?Mi$/) return strip(q, 2) * 1048576
        if (q ~ /^[0-9]+(\.[0-9]+)?Gi$/) return strip(q, 2) * 1073741824
        if (q ~ /^[0-9]+$/) return q + 0
        quantity_error = 1
        return 0
      }
      function decimal(x, s) {
        s = sprintf("%.6f", x)
        sub(/0+$/, "", s)
        sub(/\.$/, "", s)
        return s
      }
      {
        quantity_error = 0
        cpu = millicores($5)
        memory = bytes($6)
        if (quantity_error) {
          print $1 " usage: cannot convert the quantities " $5 " and " $6 " of " $2 "/" $3 "/" $4 >>errors
          next
        }
        $5 = decimal(cpu)
        $6 = sprintf("%.0f", memory)
        print
      }' >>platform/usage.tsv
  fi
}

# ---------------------------------------------------------------------------
# sample_operators — Fetch http://<pod IP>:<port>/metrics of every operator
# pod of OPERATOR_PODS with curl and append one line per pod to
# platform/operators.tsv: timestamp, namespace, pod, go_goroutines,
# go_memstats_heap_inuse_bytes and process_resident_memory_bytes as whole
# numbers, and the leader: 1 when the sum of leader_election_master_status
# is above 0, 0 when it is 0, - without the gauge. A failed fetch, or a body
# in which one of the three series is absent or not a number, is logged to
# platform/errors.log and its line left out. A stop of the sampler,
# platform/.sampler-stop, ends the scrape after the pod at hand; main's
# final sample scrapes every pod.
# ---------------------------------------------------------------------------
sample_operators() {
  local ts ns pod ip port host body msg rc row
  [[ -n "${OPERATOR_PODS}" ]] || return 0
  ts="$(now)"
  while IFS=$'\t' read -r _ ns pod _ ip port; do
    host="${ip}"
    if [[ "${ip}" == *:* ]]; then
      host="[${ip}]"
    fi
    body="$(mktemp "${TMPDIR:-/tmp}/dizzy-soak-metrics.XXXXXX")"
    msg="$(curl -sS --fail --connect-timeout "${OPERATOR_CONNECT_TIMEOUT}" --max-time "${OPERATOR_SCRAPE_TIMEOUT}" \
      -o "${body}" "http://${host}:${port}/metrics" 2>&1)"
    rc=$?
    if ((rc != 0)); then
      record_error operators "${ns}/${pod}: ${msg:-curl exited ${rc}}"
    elif row="$(awk -v ts="${ts}" -v ns="${ns}" -v pod="${pod}" '
        $1 == "go_goroutines" || $1 == "go_memstats_heap_inuse_bytes" || $1 == "process_resident_memory_bytes" { v[$1] = $2 }
        index($1, "leader_election_master_status{") == 1 { leader += $2; seen = 1 }
        END {
          split("go_goroutines go_memstats_heap_inuse_bytes process_resident_memory_bytes", names, " ")
          for (i = 1; i <= 3; i++) {
            if (!(names[i] in v) || v[names[i]] !~ /^[0-9]+([.][0-9]+)?([eE][+-]?[0-9]+)?$/) {
              missing = missing (missing == "" ? "" : " ") names[i]
            }
          }
          if (missing != "") {
            print "metrics missing or not a number: " missing
            exit 1
          }
          printf "%s\t%s\t%s\t%.0f\t%.0f\t%.0f\t%s\n", ts, ns, pod, v[names[1]], v[names[2]], v[names[3]],
            (seen ? (leader > 0 ? 1 : 0) : "-")
        }' "${body}")"; then
      printf '%s\n' "${row}" >>platform/operators.tsv
    else
      record_error operators "${ns}/${pod}: ${row}"
    fi
    rm -f "${body}"
    # A metrics port that drops packets would otherwise hold the report back
    # by a timeout per pod.
    if [[ -e platform/.sampler-stop ]]; then
      break
    fi
  done <<<"${OPERATOR_PODS}"
}

# ---------------------------------------------------------------------------
# sampler_loop — Sample until platform/.sampler-stop exists. It sleeps in
# one-second slices, so a stop takes effect within a second of the sample.
# ---------------------------------------------------------------------------
sampler_loop() {
  local waited
  while [[ ! -e platform/.sampler-stop ]]; do
    sample_pods
    sample_usage
    sample_operators
    waited=0
    while ((waited < DIZZY_SOAK_SAMPLE_INTERVAL)) && [[ ! -e platform/.sampler-stop ]]; do
      sleep 1
      waited=$((waited + 1))
    done
  done
}

# ---------------------------------------------------------------------------
# read_conditions <file> — Write the status.conditions of every object of
# CONDITION_KINDS to <file> as {readAt, items: [{kind, namespace, name,
# conditions}], errors: [{kind, message}]}. A kind whose CRD is absent is
# skipped without a word; any other failed read goes to errors and to
# platform/errors.log. The items collect in a file, one per line: as a jq
# argument they would outgrow the size the kernel allows one argument.
# ---------------------------------------------------------------------------
read_conditions() {
  local file="$1" kind out err message items_file rows errors='[]'
  err="$(mktemp)"
  items_file="$(mktemp)"
  for kind in ${CONDITION_KINDS}; do
    if out="$(kubectl get "${kind}" -A -o json 2>"${err}")"; then
      if rows="$(printf '%s' "${out}" | jq -c --arg kind "${kind}" \
          '.items[] | {kind: $kind, namespace: (.metadata.namespace // "-"),
            name: .metadata.name, conditions: (.status.conditions // [])}' 2>&1)"; then
        if [[ -n "${rows}" ]]; then
          printf '%s\n' "${rows}" >>"${items_file}"
        fi
        continue
      fi
      message="cannot parse the list: ${rows}"
    else
      message="$(cat "${err}")"
      case "${message}" in
        *"doesn't have a resource type"* | *"no matches for kind"*) continue ;;
      esac
    fi
    record_error "conditions ${kind}" "${message}"
    errors="$(jq -c --arg kind "${kind}" --arg message "$(one_line "${message}")" \
      '. + [{kind: $kind, message: $message}]' <<<"${errors}")"
  done
  rm -f "${err}"
  jq -n --arg at "$(now)" --slurpfile items "${items_file}" --argjson errors "${errors}" \
    '{readAt: $at, items: $items, errors: $errors}' >"${file}"
  rm -f "${items_file}"
}

# ---------------------------------------------------------------------------
# derive_restarts — platform/restarts.tsv from platform/containers.tsv: per
# pod UID and container its last restartCount minus that of its first sample,
# or minus 0 for a pod created during the run, and its OOM kills, the
# distinct finishedAt values of a last termination OOMKilled inside the run.
# ---------------------------------------------------------------------------
derive_restarts() {
  {
    printf 'namespace\tpod\tcontainer\tpod_uid\towner_kind\trestarts\toom_kills\n'
    if [[ -s platform/containers.tsv ]]; then
      awk -F '\t' -v OFS='\t' -v start="${RUN_START}" '
        {
          k = $4 SUBSEP $7
          if (!(k in first)) {
            first[k] = ($5 >= start) ? 0 : $8
            order[++n] = k
            line[k] = $2 OFS $3 OFS $7 OFS $4 OFS $6
          }
          last[k] = $8
          if ($9 == "OOMKilled" && $10 >= start && !((k SUBSEP $10) in seen)) {
            seen[k SUBSEP $10] = 1
            oom[k]++
          }
        }
        END { for (i = 1; i <= n; i++) { k = order[i]; print line[k], last[k] - first[k], oom[k] + 0 } }
      ' platform/containers.tsv | sort
    fi
  } >platform/restarts.tsv
}

# ---------------------------------------------------------------------------
# derive_usage_summary — platform/usage-summary.tsv from platform/usage.tsv:
# per container its sample count, CPU mean and maximum in millicores, and its
# memory in MiB: the mean of its first tenth of samples, of its last tenth,
# the maximum, and the growth between the two means in MiB and percent. A
# tenth is at least one sample. awk reads the samples twice, counting first,
# so it holds one row per container, not per sample.
# ---------------------------------------------------------------------------
derive_usage_summary() {
  {
    printf 'namespace\tpod\tcontainer\tsamples\tcpu_mean_m\tcpu_max_m\tmemory_first_mib\tmemory_last_mib\tmemory_max_mib\tgrowth_mib\tgrowth_percent\n'
    if [[ -s platform/usage.tsv ]]; then
      awk -F '\t' '
        function tenth(k, t) { t = int(n[k] / 10); return t < 1 ? 1 : t }
        NR == FNR { n[$2 SUBSEP $3 SUBSEP $4]++; next }
        {
          k = $2 SUBSEP $3 SUBSEP $4
          i = ++seen[k]
          cpu[k] += $5
          if (i == 1 || $5 + 0 > cpu_max[k]) cpu_max[k] = $5 + 0
          if (i <= tenth(k)) memory_first[k] += $6
          if (i > n[k] - tenth(k)) memory_last[k] += $6
          if (i == 1 || $6 + 0 > memory_max[k]) memory_max[k] = $6 + 0
        }
        END {
          for (k in n) {
            split(k, p, SUBSEP)
            first = memory_first[k] / tenth(k) / 1048576
            last = memory_last[k] / tenth(k) / 1048576
            percent = first > 0 ? sprintf("%.1f", (last - first) / first * 100) : "-"
            printf "%s\t%s\t%s\t%d\t%.1f\t%.1f\t%.1f\t%.1f\t%.1f\t%.1f\t%s\n", p[1], p[2], p[3], n[k],
              cpu[k] / n[k], cpu_max[k], first, last, memory_max[k] / 1048576, last - first, percent
          }
        }
      ' platform/usage.tsv platform/usage.tsv | sort
    fi
  } >platform/usage-summary.tsv
}

# ---------------------------------------------------------------------------
# derive_operators_summary — platform/operators-summary.tsv from
# platform/operator-pods.tsv and platform/operators.tsv: per operator pod
# listed during the run its sample count, its leader state (first>last when
# it changed), the goroutines and the heap in MiB as the means of its first
# and of its last tenth of samples and the growth between them, the heap
# growth in percent, the RSS mean of its last tenth in MiB, and whether
# operator-leak judges it. A pod is not judged when it is in EXEMPT_PODS, when
# it has no sample, when its scrapes failed (listed in more than
# MIN_OPERATOR_SAMPLES pod samples, no sample in more than one of them and in
# more than a tenth; the one allows for the sampler's last scrape, which a
# stop may cut short, and a pod listed fewer times has too few samples to judge
# after one miss), when it was created during the run, has fewer than
# MIN_OPERATOR_SAMPLES samples, or changed its leadership. operator-leak fails
# a pod with no sample or failed scrapes; a fault's target is neither, since a
# killed pod's series ends at the fault and its last tenth is a few samples
# that say nothing about growth.
# ---------------------------------------------------------------------------
derive_operators_summary() {
  {
    printf 'namespace\tpod\tsamples\tleader\tgoroutines_first\tgoroutines_last\tgoroutines_growth\theap_first_mib\theap_last_mib\theap_growth_mib\theap_growth_percent\trss_last_mib\tjudged\n'
    if [[ -s platform/operator-pods.tsv ]]; then
      EXEMPT="${EXEMPT_PODS}" awk -F '\t' -v start="${RUN_START}" -v min="${MIN_OPERATOR_SAMPLES}" -v pods=platform/operator-pods.tsv '
        function tenth(k, t) { t = int(n[k] / 10); return t < 1 ? 1 : t }
        BEGIN {
          split(ENVIRON["EXEMPT"], e, "\n")
          for (i in e) if (e[i] != "") exempt[e[i]] = 1
          while ((getline line <pods) > 0) {
            split(line, f, "\t")
            k = f[2] SUBSEP f[3]
            if (!(k in created)) created[k] = f[4]
            listed[k]++
          }
          close(pods)
        }
        NR == FNR { n[$2 SUBSEP $3]++; next }
        {
          k = $2 SUBSEP $3
          i = ++seen[k]
          if (i == 1) leader_first[k] = $7
          leader_last[k] = $7
          if (i <= tenth(k)) { goroutines_first[k] += $4; heap_first[k] += $5 }
          if (i > n[k] - tenth(k)) { goroutines_last[k] += $4; heap_last[k] += $5; rss_last[k] += $6 }
        }
        END {
          for (k in created) {
            split(k, p, SUBSEP)
            misses = (k in n) ? listed[k] - n[k] : listed[k]
            if ((p[1] "/" p[2]) in exempt) judged = "fault target"
            else if (!(k in n)) judged = "no samples"
            else if (listed[k] > min && misses > 1 && misses * 10 > listed[k]) judged = "scrapes failed"
            else if (created[k] >= start) judged = "created during the run"
            else if (n[k] < min) judged = "fewer than " min " samples"
            else if (leader_first[k] != leader_last[k]) judged = "leadership changed"
            else judged = "yes"
            if (!(k in n)) {
              printf "%s\t%s\t0\t-\t-\t-\t-\t-\t-\t-\t-\t-\t%s\n", p[1], p[2], judged
              continue
            }
            t = tenth(k)
            gf = goroutines_first[k] / t
            gl = goroutines_last[k] / t
            hf = heap_first[k] / t / 1048576
            hl = heap_last[k] / t / 1048576
            leader = leader_first[k] == leader_last[k] ? leader_first[k] : leader_first[k] ">" leader_last[k]
            percent = hf > 0 ? sprintf("%.1f", (hl - hf) / hf * 100) : "-"
            printf "%s\t%s\t%d\t%s\t%.1f\t%.1f\t%.1f\t%.1f\t%.1f\t%.1f\t%s\t%.1f\t%s\n", p[1], p[2], n[k], leader,
              gf, gl, gl - gf, hf, hl, hl - hf, percent, rss_last[k] / t / 1048576, judged
          }
        }
      ' platform/operators.tsv platform/operators.tsv | sort
    fi
  } >platform/operators-summary.tsv
}

# ---------------------------------------------------------------------------
# derive_memory_growth — platform/memory-growth.tsv from platform/usage.tsv:
# per container its sample count, its memory in MiB as the mean of the
# samples around the run's midpoint (index ceil(0.45 n) to floor(0.55 n),
# the sample at round(n/2) when that range is empty) and the mean of its
# last tenth (at least one sample), the growth between the two in MiB and in
# percent (- at a midpoint mean of 0), and whether memory-growth judges it.
# The second half leaves out the warm-up a long run's caches fill in. A
# container is judged only when its pod is not in EXEMPT_PODS, its namespace
# is not in DIZZY_SOAK_MEMORY_GROWTH_SKIP_NAMESPACES, it has
# MIN_MEMORY_SAMPLES samples, its pod has one UID in every pod sample of
# platform/containers.tsv, was created before the run and its restart count
# in platform/restarts.tsv is 0; otherwise the column names the first reason
# that holds. awk reads the samples twice, counting first.
# ---------------------------------------------------------------------------
derive_memory_growth() {
  {
    printf 'namespace\tpod\tcontainer\tsamples\tmid_mib\tlast_mib\tgrowth_mib\tgrowth_percent\tjudged\n'
    if [[ -s platform/usage.tsv ]]; then
      EXEMPT="${EXEMPT_PODS}" awk -F '\t' -v start="${RUN_START}" -v min="${MIN_MEMORY_SAMPLES}" \
        -v skip="${DIZZY_SOAK_MEMORY_GROWTH_SKIP_NAMESPACES}" \
        -v containers=platform/containers.tsv -v restarts=platform/restarts.tsv -v samples=platform/pod-samples.log '
        function mean(sum, count) { return count > 0 ? sum / count / 1048576 : 0 }
        BEGIN {
          split(ENVIRON["EXEMPT"], e, "\n")
          for (i in e) if (e[i] != "") exempt[e[i]] = 1
          split(skip, s, " ")
          for (i in s) if (s[i] != "") skipped[s[i]] = 1
          # Distinct times: two samples within one second share a timestamp.
          while ((getline line <samples) > 0) if (line != "" && !(line in sample_time)) { sample_time[line] = 1; pod_samples++ }
          close(samples)
          while ((getline line <containers) > 0) {
            split(line, f, "\t")
            id = f[2] "/" f[3]
            if (!((id SUBSEP f[4]) in uid_seen)) { uid_seen[id SUBSEP f[4]] = 1; uids[id]++ }
            if (!((id SUBSEP f[1]) in sample_seen)) { sample_seen[id SUBSEP f[1]] = 1; appearances[id]++ }
            if (!(id in created) || f[5] < created[id]) created[id] = f[5]
          }
          close(containers)
          while ((getline line <restarts) > 0) {
            split(line, f, "\t")
            if (f[1] != "namespace") restarted[f[1] "/" f[2] "/" f[3]] += f[6]
          }
          close(restarts)
        }
        NR == FNR { n[$2 "/" $3 "/" $4]++; next }
        {
          k = $2 "/" $3 "/" $4
          i = ++seen[k]
          # Integer bounds: 0.45 * n in floating point can land above a whole
          # number it equals.
          lo = int((45 * n[k] + 99) / 100)
          hi = int(55 * n[k] / 100)
          if (lo > hi) { lo = int(n[k] / 2 + 0.5); hi = lo }
          if (i >= lo && i <= hi) { mid_sum[k] += $6; mid_count[k]++ }
          t = int(n[k] / 10)
          if (t < 1) t = 1
          if (i > n[k] - t) { last_sum[k] += $6; last_count[k]++ }
        }
        END {
          for (k in n) {
            split(k, p, "/")
            pod = p[1] "/" p[2]
            if (pod in exempt) judged = "exempt"
            else if (p[1] in skipped) judged = "namespace skipped"
            else if (n[k] < min) judged = "fewer than " min " samples"
            else if (!(pod in uids)) judged = "no pod sample"
            else if (uids[pod] > 1 || appearances[pod] < pod_samples) judged = "pod replaced during the run"
            else if (created[pod] >= start) judged = "created during the run"
            else if (restarted[k] > 0) judged = "restarted"
            else judged = "yes"
            mid = mean(mid_sum[k], mid_count[k])
            last = mean(last_sum[k], last_count[k])
            percent = mid > 0 ? sprintf("%.1f", (last - mid) / mid * 100) : "-"
            printf "%s\t%s\t%s\t%d\t%.1f\t%.1f\t%.1f\t%s\t%s\n", p[1], p[2], p[3], n[k], mid, last, last - mid, percent, judged
          }
        }
      ' platform/usage.tsv platform/usage.tsv | sort
    fi
  } >platform/memory-growth.tsv
}

# ---------------------------------------------------------------------------
# derive_conditions — platform/conditions.tsv from the two condition reads:
# per object its kind, namespace and name, its Ready status at the start and
# at the end (- without a Ready condition, absent when the object was not
# there), and every condition whose lastTransitionTime lies inside the run.
# ---------------------------------------------------------------------------
derive_conditions() {
  local rows
  if ! rows="$(jq -r -n --arg start "${RUN_START}" \
      --slurpfile s platform/conditions-start.json --slurpfile e platform/conditions-end.json '
      def ready: ([.conditions[]? | select(.type == "Ready") | .status] | first) // "-";
      def key: .kind + "/" + .namespace + "/" + .name;
      ($s[0].items // []) as $si | ($e[0].items // []) as $ei
      | ([$si[], $ei[]] | map(key) | unique)[] as $k
      | ([$si[] | select(key == $k)] | first) as $a
      | ([$ei[] | select(key == $k)] | first) as $b
      | ($b // $a) as $o
      | [$o.kind, $o.namespace, $o.name,
         (if $a then ($a | ready) else "absent" end),
         (if $b then ($b | ready) else "absent" end),
         ([$o.conditions[]? | select((.lastTransitionTime // "") >= $start)
           | .type + "=" + .status + " " + .lastTransitionTime] | join(", ") | if . == "" then "-" else . end)]
      | @tsv' 2>&1)"; then
    record_error conditions "cannot derive the conditions table: ${rows}"
    rows=""
  fi
  {
    printf 'kind\tnamespace\tname\tready_start\tready_end\ttransitions_in_run\n'
    if [[ -n "${rows}" ]]; then
      printf '%s\n' "${rows}"
    fi
  } >platform/conditions.tsv
}

# run_records — The run records dizzy wrote into the run directory.
run_records() {
  local f
  for f in run-*.json; do
    [[ -f "${f}" ]] && printf '%s\n' "${f}"
  done
}

# ---------------------------------------------------------------------------
# render_dizzy_reports — When dizzy wrote exactly one run record, render it
# with `dizzy <service> report` as dizzy-report.json, .html and .txt. A
# failed render leaves no partial file.
# ---------------------------------------------------------------------------
render_dizzy_reports() {
  local records format file
  records="$(run_records)"
  if [[ -z "${records}" || "$(printf '%s\n' "${records}" | wc -l | tr -d ' ')" != "1" ]]; then
    log "No single run record to report on; dizzy-report.* are not rendered."
    return 0
  fi
  for format in json html table; do
    case "${format}" in
      table) file="dizzy-report.txt" ;;
      *) file="dizzy-report.${format}" ;;
    esac
    if ! "${DIZZY_BIN}" "${DIZZY_SOAK_SERVICE}" report --run "${records}" --format "${format}" >"${file}" 2>dizzy-report.err; then
      log "WARNING: dizzy ${DIZZY_SOAK_SERVICE} report --format ${format} failed: $(one_line "$(cat dizzy-report.err)")"
      rm -f "${file}"
    fi
  done
  rm -f dizzy-report.err
}

# check <name> <pass> <detail> — One check as a JSON object.
check() {
  jq -cn --arg name "$1" --argjson pass "$2" --arg detail "$3" '{name: $name, pass: $pass, detail: $detail}'
}

# check_dizzy_exit — dizzy exited 0 and wrote exactly one run record.
check_dizzy_exit() {
  local records count
  if [[ -z "${DIZZY_RC}" ]]; then
    check dizzy-exit false "dizzy was not started: the soak was stopped before it began"
    return
  fi
  records="$(run_records)"
  count="$(printf '%s' "${records}" | grep -c . || true)"
  if [[ "${DIZZY_RC}" != "0" ]]; then
    check dizzy-exit false "dizzy exited ${DIZZY_RC}; ${count} run record(s)"
  elif [[ "${count}" == "0" ]]; then
    check dizzy-exit false "dizzy exited 0 and wrote no run record"
  elif [[ "${count}" != "1" ]]; then
    check dizzy-exit false "dizzy exited 0 and wrote ${count} run records"
  else
    check dizzy-exit true "dizzy exited 0 and wrote ${records}"
  fi
}

# check_leak — The last leak-check line of dizzy.log is a clean one.
check_leak() {
  local line
  line="$(grep -E '^leak check: ' dizzy.log 2>/dev/null | tail -n 1)"
  if [[ "${line}" =~ ${LEAK_CHECK_CLEAN_RE} ]]; then
    check leak-check true "${line}"
    return
  fi
  check leak-check false "${line:-leak check line not found}"
}

# check_controlplane_ready — At least one ControlPlane was read at the end,
# and each has Ready=True.
check_controlplane_ready() {
  local file=platform/conditions-end.json message states
  if ! jq -e . "${file}" >/dev/null 2>&1; then
    check controlplane-ready false "the conditions at the end of the run could not be read"
    return
  fi
  message="$(jq -r '.errors[] | select(.kind == "controlplanes.c5c3.io") | .message' "${file}" 2>/dev/null | head -n 1)"
  if [[ -n "${message}" ]]; then
    check controlplane-ready false "${message}"
    return
  fi
  states="$(jq -r '.items[] | select(.kind == "controlplanes.c5c3.io")
    | .namespace + "/" + .name + " Ready="
      + (([.conditions[] | select(.type == "Ready") | .status] | first) // "absent")' "${file}" 2>/dev/null)"
  if [[ -z "${states}" ]]; then
    check controlplane-ready false "no ControlPlane found"
  elif grep -qv ' Ready=True$' <<<"${states}"; then
    check controlplane-ready false "$(grep -v ' Ready=True$' <<<"${states}" | paste -sd ';' - | sed 's/;/; /g')"
  else
    check controlplane-ready true "$(paste -sd ';' - <<<"${states}" | sed 's/;/; /g')"
  fi
}

# check_no_restarts — At least one pod sample succeeded, and no container
# of a pod outside EXEMPT_PODS restarted or was OOM-killed during the run.
check_no_restarts() {
  local containers restarts ooms skipped suffix=""
  if [[ ! -s platform/pod-samples.log ]]; then
    check no-restarts false "pod state could not be read: $(grep ' pods: ' platform/errors.log 2>/dev/null | tail -n 1)"
    return
  fi
  read -r containers restarts ooms skipped <<<"$(EXEMPT="${EXEMPT_PODS}" awk -F '\t' '
    BEGIN { split(ENVIRON["EXEMPT"], e, "\n"); for (i in e) if (e[i] != "") exempt[e[i]] = 1 }
    NR > 1 && ($1 "/" $2) in exempt { if (!(($1 "/" $2) in seen)) { seen[$1 "/" $2] = 1; s++ } next }
    NR > 1 { n++; r += $6; o += $7 }
    END { print n + 0, r + 0, o + 0, s + 0 }' platform/restarts.tsv)"
  if [[ "${skipped}" != "0" ]]; then
    suffix="; ${skipped} exempt pod(s) skipped"
  fi
  if [[ "${containers}" == "0" ]]; then
    check no-restarts true "0 containers observed${suffix}"
  elif [[ "${restarts}" == "0" && "${ooms}" == "0" ]]; then
    check no-restarts true "${containers} containers observed, 0 restarts, 0 OOM kills${suffix}"
  else
    check no-restarts false "${restarts} restart(s) and ${ooms} OOM kill(s) in ${containers} containers observed${suffix}"
  fi
}

# check_memory_growth — At least one sample of the metrics API succeeded, at
# least one container of platform/memory-growth.tsv is judged, and no judged
# container grew by more than DIZZY_SOAK_MAX_MEMORY_GROWTH_MIB and by more
# than DIZZY_SOAK_MAX_MEMORY_GROWTH_PERCENT at once. It compares the values
# as the table prints them, so a growth at a bound passes. Either bound
# alone is no defect: a small container's percent and a large one's MiB grow
# with ordinary churn. The detail names how many containers of exempt pods
# and of DIZZY_SOAK_MEMORY_GROWTH_SKIP_NAMESPACES it skipped.
check_memory_growth() {
  if [[ ! -s platform/usage.tsv ]]; then
    check memory-growth false "memory samples could not be read: $(grep ' usage: ' platform/errors.log 2>/dev/null | tail -n 1)"
    return
  fi
  awk -F '\t' -v mib_bound="${DIZZY_SOAK_MAX_MEMORY_GROWTH_MIB}" -v percent_bound="${DIZZY_SOAK_MAX_MEMORY_GROWTH_PERCENT}" \
    -v skip="${DIZZY_SOAK_MEMORY_GROWTH_SKIP_NAMESPACES}" '
    NR == 1 { next }
    {
      m++
      id = $1 "/" $2 "/" $3
      if ($9 == "exempt") { skipped++; next }
      if ($9 == "namespace skipped") { ns_skipped++; next }
      if ($9 != "yes") {
        if (!($9 in reasons)) order[++nreasons] = $9
        reasons[$9]++
        next
      }
      judged++
      if (judged == 1 || $7 + 0 > largest + 0) { largest = $7; largest_percent = $8; largest_id = id }
      if ($7 + 0 > mib_bound + 0 && $8 != "-" && $8 + 0 > percent_bound + 0) {
        failures = failures (failures == "" ? "" : "; ") id ": " $5 " to " $6 " MiB (+" $7 " MiB, " $8 " %)"
      }
    }
    END {
      suffix = skipped > 0 ? "; " skipped " container(s) of exempt pods skipped" : ""
      if (ns_skipped > 0) {
        n = split(skip, s, " ")
        for (i = 1; i <= n; i++) if (s[i] != "") names = names (names == "" ? "" : ", ") s[i]
        suffix = suffix "; " ns_skipped " container(s) of " names " skipped"
      }
      if (failures != "") {
        printf "false\t%s%s\n", failures, suffix
        exit
      }
      if (judged == 0) {
        for (i = 1; i <= nreasons; i++) list = list (i == 1 ? "" : ", ") order[i] " (" reasons[order[i]] ")"
        if (skipped > 0) list = list (list == "" ? "" : ", ") "exempt (" skipped ")"
        if (ns_skipped > 0) list = list (list == "" ? "" : ", ") "namespace skipped (" ns_skipped ")"
        printf "false\tno container could be judged; the reasons: %s\n", list
        exit
      }
      printf "true\t%d of %d containers judged; largest growth %s MiB (%s %%, %s)%s\n", judged, m, largest, largest_percent, largest_id, suffix
    }
  ' platform/memory-growth.tsv | {
    IFS=$'\t' read -r pass detail
    check memory-growth "${pass}" "${detail}"
  }
}

# check_operator_leak — At least one operator pod was listed, no pod of
# platform/operators-summary.tsv has no samples or failed scrapes, and each
# one the summary judges kept its heap growth within
# DIZZY_SOAK_MAX_OPERATOR_HEAP_GROWTH_PERCENT and its goroutine growth
# within DIZZY_SOAK_MAX_OPERATOR_GOROUTINE_GROWTH. It compares the values as
# the summary prints them. A pod without samples or with failed scrapes
# names the last scrape error errors.log holds for it.
check_operator_leak() {
  if [[ ! -s platform/pod-samples.log ]]; then
    check operator-leak false "operator pods could not be listed: $(grep ' pods: ' platform/errors.log 2>/dev/null | tail -n 1)"
    return
  fi
  if [[ ! -s platform/operator-pods.tsv ]]; then
    check operator-leak false "no operator pod found in the sampled namespaces"
    return
  fi
  awk -F '\t' -v heap_bound="${DIZZY_SOAK_MAX_OPERATOR_HEAP_GROWTH_PERCENT}" \
    -v goroutine_bound="${DIZZY_SOAK_MAX_OPERATOR_GOROUTINE_GROWTH}" -v errors=platform/errors.log '
    # last_error(id) — The text after "operators: <id>: " in the last line
    # of errors.log that holds it, or none.
    function last_error(id, needle, i) {
      needle = "operators: " id ": "
      for (i = lines; i >= 1; i--) {
        if (index(error_line[i], needle) > 0) return substr(error_line[i], index(error_line[i], needle) + length(needle))
      }
      return "none"
    }
    BEGIN {
      while ((getline line <errors) > 0) error_line[++lines] = line
      close(errors)
    }
    NR == 1 { next }
    {
      n++
      id = $1 "/" $2
      if ($13 == "no samples") {
        failures = failures (failures == "" ? "" : "; ") id ": no metrics sample; last error: " last_error(id)
        next
      }
      if ($13 == "scrapes failed") {
        failures = failures (failures == "" ? "" : "; ") id ": scrapes failed, " $3 " metrics samples; last error: " last_error(id)
        next
      }
      if ($13 != "yes") {
        skipped = skipped (skipped == "" ? "" : ", ") id " (" $13 ")"
        next
      }
      judged++
      if (judged == 1 || ($11 != "-" && (largest_heap == "-" || $11 + 0 > largest_heap + 0))) { largest_heap = $11; largest_heap_id = id }
      if (judged == 1 || $7 + 0 > largest_goroutines + 0) { largest_goroutines = $7; largest_goroutines_id = id }
      if (($11 != "-" && $11 + 0 > heap_bound + 0) || $7 + 0 > goroutine_bound + 0) {
        failures = failures (failures == "" ? "" : "; ") id ": heap growth " $11 " % (" $8 " to " $9 " MiB), goroutine growth " $7 " (" $5 " to " $6 ")"
      }
    }
    END {
      if (failures != "") {
        printf "false\t%s\n", failures
        exit
      }
      detail = sprintf("%d of %d operator pods judged", judged, n)
      if (judged > 0) {
        detail = detail "; largest heap growth " largest_heap " % (" largest_heap_id "); largest goroutine growth " largest_goroutines " (" largest_goroutines_id ")"
      }
      if (skipped != "") detail = detail "; not judged: " skipped
      printf "true\t%s\n", detail
    }
  ' platform/operators-summary.tsv | {
    IFS=$'\t' read -r pass detail
    check operator-leak "${pass}" "${detail}"
  }
}

# check_error_rate — The share of failed operations of dizzy's report, in
# percent, is at most DIZZY_SOAK_MAX_ERROR_RATE. Runs only with that set.
check_error_rate() {
  local attempted failed
  [[ -n "${DIZZY_SOAK_MAX_ERROR_RATE}" ]] || return 0
  if ! attempted="$(jq -er '.metrics.overall.attempted' dizzy-report.json 2>/dev/null)" ||
    ! failed="$(jq -er '.metrics.overall.failed' dizzy-report.json 2>/dev/null)"; then
    check error-rate false "no dizzy report to read the error rate from"
    return
  fi
  if [[ "${attempted}" == "0" ]]; then
    check error-rate false "no operations attempted"
    return
  fi
  awk -v a="${attempted}" -v f="${failed}" -v max="${DIZZY_SOAK_MAX_ERROR_RATE}" 'BEGIN {
    rate = f * 100 / a
    printf "%s\t%s of %s operations failed (%.2f%%), %s the maximum of %s%%\n",
      (rate <= max ? "true" : "false"), f, a, rate, (rate <= max ? "within" : "above"), max
  }' | {
    IFS=$'\t' read -r pass detail
    check error-rate "${pass}" "${detail}"
  }
}

# check_p95 — The overall p95 latency of dizzy's report, which it gives in
# nanoseconds, is at most DIZZY_SOAK_MAX_P95_SECONDS. Runs only with that set.
check_p95() {
  local p95
  [[ -n "${DIZZY_SOAK_MAX_P95_SECONDS}" ]] || return 0
  if ! p95="$(jq -er '.metrics.overall.latency.p95' dizzy-report.json 2>/dev/null)"; then
    check p95 false "no dizzy report to read the p95 latency from"
    return
  fi
  awk -v ns="${p95}" -v max="${DIZZY_SOAK_MAX_P95_SECONDS}" 'BEGIN {
    s = ns / 1000000000
    printf "%s\tp95 %.3fs, %s the maximum of %ss\n", (s <= max ? "true" : "false"), s, (s <= max ? "within" : "above"), max
  }' | {
    IFS=$'\t' read -r pass detail
    check p95 "${pass}" "${detail}"
  }
}

# write_verdict — verdict.json from the checks: PASS when every check that
# ran passed.
write_verdict() {
  {
    check_dizzy_exit
    check_leak
    check_controlplane_ready
    check_no_restarts
    check_operator_leak
    check_memory_growth
    check_error_rate
    check_p95
  } | jq -s '{verdict: (if all(.[]; .pass) then "PASS" else "FAIL" end), checks: .}' >verdict.json
}

# tsv_table [rows] — A Markdown table of the TSV on stdin whose first line
# is the header, with at most [rows] rows.
tsv_table() {
  awk -F '\t' -v rows="${1:-0}" '
    NR == 1 { h = "|"; s = "|"; for (i = 1; i <= NF; i++) { h = h " " $i " |"; s = s " --- |" } print h; print s; next }
    rows == 0 || NR - 1 <= rows { r = "|"; for (i = 1; i <= NF; i++) r = r " " $i " |"; print r }
  '
}

# ---------------------------------------------------------------------------
# write_report_md — report.md: the verdict and its checks, the run's
# parameters and times, dizzy's report, the leak check, the restarts and OOM
# kills, the pods created during the run, the memory growth and CPU tables,
# the memory growth over the second half, the operator goroutines and heap,
# the conditions, and the count of lines in platform/errors.log.
# ---------------------------------------------------------------------------
write_report_md() {
  local header shown created judged
  {
    printf '# dizzy soak %s\n\n' "${RUN_NAME}"
    printf 'Verdict: **%s**\n\n' "$(jq -r '.verdict' verdict.json)"
    printf '| Check | Result | Detail |\n| --- | --- | --- |\n'
    jq -r '.checks[] | "| \(.name) | \(if .pass then "pass" else "FAIL" end) | \(.detail | gsub("\\|"; "\\|")) |"' verdict.json

    printf '\n## Parameters\n\n| Parameter | Value |\n| --- | --- |\n'
    jq -r 'to_entries[] | "| \(.key) | \(.value | if type == "array" then join(" ") else tostring end) |"' meta.json

    printf '\n## dizzy report\n\n'
    if [[ -s dizzy-report.txt ]]; then
      printf '```text\n'
      cat dizzy-report.txt
      printf '```\n'
    else
      printf 'No dizzy report: dizzy wrote no single run record, or its report did not render.\n'
    fi

    printf '\n## Leak check\n\n'
    printf '%s\n' "$(grep -E '^leak check: ' dizzy.log 2>/dev/null | tail -n 1 || true)" |
      sed 's/^$/No leak check line in dizzy.log./'

    printf '\n## Restarts and OOM kills\n\n'
    shown="$(awk -F '\t' 'NR == 1 || $6 > 0 || $7 > 0' platform/restarts.tsv)"
    if [[ "$(wc -l <<<"${shown}" | tr -d ' ')" -gt 1 ]]; then
      printf '%s\n' "${shown}" | tsv_table
    else
      printf 'No container restarted or was OOM-killed during the run (%s containers observed).\n' \
        "$(awk 'NR > 1' platform/restarts.tsv | grep -c . || true)"
    fi

    printf '\n## Pods created during the run\n\nPods whose owner is not a Job, per namespace.\n\n'
    created="$(pods_created_during_run)"
    if [[ "$(wc -l <<<"${created}" | tr -d ' ')" -gt 1 ]]; then
      printf '%s\n' "${created}" | tsv_table
    else
      printf 'None.\n'
    fi

    printf '\n## Resource usage\n\n'
    if [[ ! -s platform/usage.tsv ]]; then
      printf 'Resource usage: unavailable. No sample of the metrics API succeeded; see platform/errors.log.\n'
    else
      header="$(head -n 1 platform/usage-summary.tsv)"
      printf 'The 15 containers with the largest memory growth:\n\n'
      { printf '%s\n' "${header}"; awk 'NR > 1' platform/usage-summary.tsv | sort -t "$(printf '\t')" -k10,10nr; } |
        tsv_table 15
      printf '\nThe 15 containers with the highest mean CPU:\n\n'
      { printf '%s\n' "${header}"; awk 'NR > 1' platform/usage-summary.tsv | sort -t "$(printf '\t')" -k5,5nr; } |
        tsv_table 15
    fi

    printf '\n## Memory growth (second half)\n\n'
    judged="$(awk -F '\t' 'NR > 1 && $9 == "yes"' platform/memory-growth.tsv 2>/dev/null)"
    if [[ ! -s platform/usage.tsv ]]; then
      printf 'Memory growth: unavailable. No sample of the metrics API succeeded; see platform/errors.log.\n'
    elif [[ -z "${judged}" ]]; then
      printf 'No container was judged; platform/memory-growth.tsv names the reason for each.\n'
    else
      printf 'The 15 judged containers with the largest growth from the mean around the midpoint to the mean of the last tenth:\n\n'
      { head -n 1 platform/memory-growth.tsv; printf '%s\n' "${judged}" | sort -t "$(printf '\t')" -k7,7nr; } |
        tsv_table 15
    fi

    printf '\n## Operator goroutines and heap\n\n'
    if [[ "$(wc -l <platform/operators-summary.tsv | tr -d ' ')" -gt 1 ]]; then
      tsv_table <platform/operators-summary.tsv
    else
      printf 'No operator pod was listed in the sampled namespaces.\n'
    fi

    printf '\n## Conditions\n\n'
    if [[ "$(wc -l <platform/conditions.tsv | tr -d ' ')" -gt 1 ]]; then
      tsv_table <platform/conditions.tsv
    else
      printf 'No object of the condition kinds was read.\n'
    fi

    printf '\n## Errors\n\nplatform/errors.log holds %s lines.\n' \
      "$(cat platform/errors.log 2>/dev/null | wc -l | tr -d ' ')"
  } >report.md
}

# pods_created_during_run — Per namespace, the pods created during the run
# whose owner is not a Job, as a TSV with a header.
pods_created_during_run() {
  printf 'namespace\tpods\n'
  if [[ -s platform/containers.tsv ]]; then
    awk -F '\t' -v OFS='\t' -v start="${RUN_START}" '
      $5 >= start && $6 != "Job" && !($4 in seen) { seen[$4] = 1; n[$2]++ }
      END { for (ns in n) print ns, n[ns] }
    ' platform/containers.tsv | sort
  fi
}

# ---------------------------------------------------------------------------
# on_signal — The TERM and INT trap: remember the stop and forward it to
# dizzy, at most once.
# ---------------------------------------------------------------------------
on_signal() {
  STOP_REQUESTED=1
  forward_stop
}

forward_stop() {
  if [[ -n "${DIZZY_PID}" && -z "${TERM_SENT}" ]]; then
    TERM_SENT=1
    log "Stopping dizzy (pid ${DIZZY_PID}): it removes its resources and prints its leak check."
    kill -TERM "${DIZZY_PID}" 2>/dev/null
  fi
}

# ---------------------------------------------------------------------------
# start_dizzy — Start dizzy in the background, its output going to the pod
# log and to dizzy.log through a FIFO and tee, so DIZZY_PID is dizzy's own.
# ---------------------------------------------------------------------------
start_dizzy() {
  local extra_args=()
  # An empty DIZZY_ARGS adds no argument; ${a[@]+"${a[@]}"} keeps bash 3.2
  # from calling the empty array unbound under set -u.
  read -r -a extra_args <<<"${DIZZY_ARGS}"
  DIZZY_FIFO="$(mktemp -u "${TMPDIR:-/tmp}/dizzy-soak.XXXXXX")"
  mkfifo "${DIZZY_FIFO}"
  tee -a dizzy.log <"${DIZZY_FIFO}" &
  TEE_PID=$!
  log "Starting: dizzy ${DIZZY_SOAK_SERVICE} chaos --os-cloud dizzy-soak --scenario ${SCENARIO_FILE} --duration ${DIZZY_SOAK_DURATION} --otel ${DIZZY_ARGS}"
  OS_CLIENT_CONFIG_FILE="${CLOUDS_FILE}" \
    OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="${OTLP_ENDPOINT}" \
    OTEL_METRIC_EXPORT_INTERVAL=15000 \
    "${DIZZY_BIN}" "${DIZZY_SOAK_SERVICE}" chaos \
    --os-cloud dizzy-soak \
    --scenario "${SCENARIO_FILE}" \
    --duration "${DIZZY_SOAK_DURATION}" \
    --otel \
    ${extra_args[@]+"${extra_args[@]}"} >"${DIZZY_FIFO}" 2>&1 &
  DIZZY_PID=$!
}

# ---------------------------------------------------------------------------
# wait_for <pid> — Wait until the background process <pid> has exited and
# set WAIT_RC to its exit code. A trapped TERM cuts a wait short with a code
# above 128 while the process still runs, so the wait repeats until it is
# gone; a wait that was cut short has not taken the exit code, so one more
# wait does. It must run in the shell that started <pid>, never in a
# command substitution.
# ---------------------------------------------------------------------------
wait_for() {
  local pid="$1" rc again
  while :; do
    wait "${pid}"
    rc=$?
    if ((rc <= 128)); then
      break
    fi
    if kill -0 "${pid}" 2>/dev/null; then
      continue
    fi
    wait "${pid}" 2>/dev/null
    again=$?
    if ((again != 127)); then
      rc="${again}"
    fi
    break
  done
  WAIT_RC="${rc}"
}

# write_meta — meta.json at the start of the run.
write_meta() {
  local namespaces="null" out err
  if [[ -n "${DIZZY_SOAK_NAMESPACES}" ]]; then
    namespaces="$(words_json "${DIZZY_SOAK_NAMESPACES}")"
  else
    err="$(mktemp)"
    if ! out="$(kubectl get namespaces -o json 2>"${err}")"; then
      record_error namespaces "$(cat "${err}")"
    elif ! namespaces="$(printf '%s' "${out}" | jq -c --argjson excluded "$(words_json "${EXCLUDED_NAMESPACES}")" \
      '[.items[].metadata.name | select(IN($excluded[]) | not)]' 2>&1)"; then
      record_error namespaces "cannot parse the namespace list: ${namespaces}"
      namespaces="null"
    fi
    rm -f "${err}"
  fi
  jq -n --arg run "${RUN_NAME}" --arg start "${RUN_START}" --arg service "${DIZZY_SOAK_SERVICE}" \
    --arg duration "${DIZZY_SOAK_DURATION}" --arg version "${DIZZY_VERSION}" --arg args "${DIZZY_ARGS}" \
    --argjson namespaces "${namespaces}" --arg interval "${DIZZY_SOAK_SAMPLE_INTERVAL}" \
    --arg rate "${DIZZY_SOAK_MAX_ERROR_RATE}" --arg p95 "${DIZZY_SOAK_MAX_P95_SECONDS}" \
    --arg heap "${DIZZY_SOAK_MAX_OPERATOR_HEAP_GROWTH_PERCENT}" --arg goroutines "${DIZZY_SOAK_MAX_OPERATOR_GOROUTINE_GROWTH}" \
    --arg memory_percent "${DIZZY_SOAK_MAX_MEMORY_GROWTH_PERCENT}" --arg memory_mib "${DIZZY_SOAK_MAX_MEMORY_GROWTH_MIB}" \
    --argjson memory_skip "$(words_json "${DIZZY_SOAK_MEMORY_GROWTH_SKIP_NAMESPACES}")" '
    def number: if . == "" then null else tonumber end;
    {run: $run, startTime: $start, service: $service, duration: $duration, dizzyVersion: $version,
     dizzyArgs: $args, namespaces: $namespaces, sampleIntervalSeconds: ($interval | tonumber),
     maxErrorRatePercent: ($rate | number), maxP95Seconds: ($p95 | number),
     maxOperatorHeapGrowthPercent: ($heap | tonumber), maxOperatorGoroutineGrowth: ($goroutines | tonumber),
     maxMemoryGrowthPercent: ($memory_percent | tonumber), maxMemoryGrowthMiB: ($memory_mib | tonumber),
     memoryGrowthSkipNamespaces: $memory_skip}' >meta.json
}

# check_free_space — Refuse to start with less than MIN_FREE_KIB free on
# REPORTS_DIR.
check_free_space() {
  local free
  if ! free="$(df -Pk "${REPORTS_DIR}" 2>&1 | awk 'NR == 2 { print $4 }')" || [[ ! "${free}" =~ ^[0-9]+$ ]]; then
    log "ERROR: cannot read the free space of the claim dizzy-soak-reports at ${REPORTS_DIR}."
    return 1
  fi
  if ((free < MIN_FREE_KIB)); then
    log "ERROR: the claim dizzy-soak-reports has $((free / 1024)) MiB free at ${REPORTS_DIR}; a soak needs $((MIN_FREE_KIB / 1024)) MiB."
    log "       Fetch the runs you keep with make dizzy-soak-report and free the claim (see the section Freeing the claim of docs/reference/testing/dizzy-chaos-testing.md)."
    return 1
  fi
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------
main() {
  # First: the runner is PID 1 of its container, and PID 1 drops a signal it
  # has no handler for.
  trap on_signal TERM INT

  check_free_space || exit 1

  RUN_NAME="$(date -u '+%Y%m%dT%H%M%SZ')"
  RUN_START="${RUN_NAME:0:4}-${RUN_NAME:4:2}-${RUN_NAME:6:2}T${RUN_NAME:9:2}:${RUN_NAME:11:2}:${RUN_NAME:13:2}Z"
  if ! mkdir -p "${REPORTS_DIR}/${RUN_NAME}/platform" || ! cd "${REPORTS_DIR}/${RUN_NAME}"; then
    log "ERROR: cannot create ${REPORTS_DIR}/${RUN_NAME} on the claim dizzy-soak-reports."
    exit 1
  fi
  log "Soak run ${RUN_NAME}: dizzy ${DIZZY_VERSION} ${DIZZY_SOAK_SERVICE} chaos for ${DIZZY_SOAK_DURATION} (0 runs until stopped), report in ${REPORTS_DIR}/${RUN_NAME}."
  : >platform/containers.tsv
  : >platform/usage.tsv
  : >platform/operator-pods.tsv
  : >platform/operators.tsv
  : >platform/errors.log
  cp "${SCENARIO_FILE}" scenario.yaml
  write_meta
  read_conditions platform/conditions-start.json

  sampler_loop &
  SAMPLER_PID=$!

  if [[ -z "${STOP_REQUESTED}" ]]; then
    start_dizzy
    # A TERM that came while dizzy was being started.
    if [[ -n "${STOP_REQUESTED}" ]]; then
      forward_stop
    fi
    wait_for "${DIZZY_PID}"
    DIZZY_RC="${WAIT_RC}"
    log "dizzy exited ${DIZZY_RC}."
    wait_for "${TEE_PID}"
    rm -f "${DIZZY_FIFO}"
  else
    log "The soak was stopped before dizzy started."
  fi

  : >platform/.sampler-stop
  wait_for "${SAMPLER_PID}"
  rm -f platform/.sampler-stop
  sample_pods
  sample_usage
  sample_operators
  read_conditions platform/conditions-end.json
  jq --arg end "$(now)" --argjson rc "${DIZZY_RC:-null}" '. + {endTime: $end, dizzyExitCode: $rc}' \
    meta.json >meta.json.new && mv meta.json.new meta.json

  render_dizzy_reports
  derive_restarts
  derive_usage_summary
  derive_operators_summary
  derive_memory_growth
  derive_conditions
  write_verdict
  write_report_md
  cat report.md

  if [[ "$(jq -r '.verdict' verdict.json)" == "PASS" ]]; then
    exit 0
  fi
  exit 1
}

# Run main only when executed directly so unit tests (tests/unit/hack/) can
# source this script and exercise individual functions.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
