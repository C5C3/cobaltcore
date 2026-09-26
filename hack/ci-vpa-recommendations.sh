#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-vpa-recommendations.sh — Record what a VerticalPodAutoscaler
# recommender recommends for every workload of a namespace.
#
# The sizing measurement of the `ci:measure-sizing` label runs this script in
# the e2e-controlplane, e2e-controlplane-sso and tempest jobs, on a cluster
# deployed with WITH_VPA=true (deploy/kind/vpa). It creates one VPA per
# Deployment, StatefulSet and DaemonSet with updateMode "Off", so the
# recommender recommends requests without changing any pod, and copies each
# recommendation into a snapshot file together with the workload's current
# containers. hack/derive-sizing-figures.py turns the report into the sizing
# figures; docs/reference/testing/sizing-calibration.md describes the run.
#
# All arithmetic runs in jq; the self-hosted runners ship jq, not python3,
# which is why hack/ci-check-node-budget.sh is written the same way.
#
# Subcommands:
#   snapshot <out-dir> <namespace>...
#       One pass. Applies the VPAs and overwrites
#       <out-dir>/snapshots/<namespace>/<vpa name>.json for every VPA whose
#       recommendation is non-empty and whose workload still exists. A VPA
#       without a recommendation writes nothing, so an earlier snapshot of it
#       survives the deletion of its workload. Each file is written to a
#       temporary name and renamed, so a killed pass never leaves a partial
#       snapshot.
#   watch <out-dir> <namespace>...
#       Writes its PID to <out-dir>/watch.pid, then runs a snapshot pass
#       every 60 seconds until it is killed. A failing pass prints its message
#       prefixed with "WARNING: " to stderr and the loop carries on.
#   report <out-dir>
#       Reads every snapshot and writes <out-dir>/recommendations.tsv (one row
#       per workload container, sorted by namespace, workload and container)
#       and <out-dir>/recommendations.md (the same table in Markdown, also
#       printed to stdout). CPU columns are millicores and memory columns MiB,
#       both rounded up; a request or limit the container does not set is "-".
#       Needs no cluster: the recommender image is looked up with kubectl and
#       reads "-" when that fails.
#
# Optional env vars (report; each reads "-" when unset):
#   GITHUB_RUN_ID, GITHUB_RUN_ATTEMPT, GITHUB_SHA, GITHUB_JOB
#                — the CI run the TSV comment line names
#   MEASURE_LEG  — the tempest leg (<service>-<release>) the TSV comment
#                  line names
#
# Usage:
#   hack/ci-vpa-recommendations.sh snapshot _output/sizing openstack
#   nohup hack/ci-vpa-recommendations.sh watch _output/sizing openstack \
#     >_output/sizing/watch.log 2>&1 &
#   hack/ci-vpa-recommendations.sh report _output/sizing
#
# Exit codes:
#   0 — done
#   1 — report found no snapshot (no recommendation was recorded)
#   2 — usage error, the cluster does not serve VerticalPodAutoscaler, a
#       failing kubectl call, an unparsable snapshot, or jq missing

set -euo pipefail

VPA_RESOURCE="verticalpodautoscalers.autoscaling.k8s.io"
MANAGED_BY="ci-vpa-recommendations"
WATCH_INTERVAL_SECONDS=60

# ---------------------------------------------------------------------------
# usage — Print the usage text to stderr and exit 2.
# ---------------------------------------------------------------------------
usage() {
  cat >&2 <<'EOF'
usage: hack/ci-vpa-recommendations.sh snapshot <out-dir> <namespace>...
       hack/ci-vpa-recommendations.sh watch <out-dir> <namespace>...
       hack/ci-vpa-recommendations.sh report <out-dir>
EOF
  exit 2
}

# ---------------------------------------------------------------------------
# die — Print a prefixed message to stderr and exit 2.
# ---------------------------------------------------------------------------
die() {
  echo "ci-vpa-recommendations: $*" >&2
  exit 2
}

errfile="$(mktemp)"
# The workload list travels through a file: a namespace's list outgrows the
# 128 KiB Linux allows for one argv element (--argjson).
workloads_file="$(mktemp)"
trap 'rm -f "${errfile}" "${workloads_file}"' EXIT

# ---------------------------------------------------------------------------
# snapshot_pass <out-dir> <namespace>... — One snapshot pass.
# ---------------------------------------------------------------------------
snapshot_pass() {
  local out="$1"
  shift

  local resources
  if ! resources="$(kubectl api-resources --api-group=autoscaling.k8s.io -o name 2>"${errfile}")"; then
    die "listing the autoscaling.k8s.io API resources failed: $(cat "${errfile}")"
  fi
  if ! grep -qx "${VPA_RESOURCE}" <<<"${resources}"; then
    die "the cluster does not serve autoscaling.k8s.io VerticalPodAutoscaler; deploy with WITH_VPA=true"
  fi

  local pass_time ns count vpas line name doc dir
  pass_time="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  for ns in "$@"; do
    # kubectl answers a namespace that does not exist with an empty list.
    if ! kubectl get deployments,statefulsets,daemonsets -n "${ns}" -o json \
        >"${workloads_file}" 2>"${errfile}"; then
      die "listing workloads in ${ns} failed: $(cat "${errfile}")"
    fi
    count="$(jq '.items | length' "${workloads_file}")"
    if [[ "${count}" -eq 0 ]]; then
      echo "no workloads in ${ns}"
      continue
    fi

    # One List per namespace, so a pass costs one apply.
    # shellcheck disable=SC2016 # $mb and $ns are jq variables
    if ! jq -c --arg mb "${MANAGED_BY}" --arg ns "${ns}" '
        {apiVersion: "v1", kind: "List", items: [.items[] | {
          apiVersion: "autoscaling.k8s.io/v1",
          kind: "VerticalPodAutoscaler",
          metadata: {
            name: "sizing-\(.kind | ascii_downcase)-\(.metadata.name)",
            namespace: $ns,
            labels: {"app.kubernetes.io/managed-by": $mb}
          },
          spec: {
            targetRef: {apiVersion: "apps/v1", kind: .kind, name: .metadata.name},
            updatePolicy: {updateMode: "Off"}
          }
        }]}' "${workloads_file}" | kubectl apply -n "${ns}" -f - >/dev/null 2>"${errfile}"; then
      die "applying the VPAs in ${ns} failed: $(cat "${errfile}")"
    fi

    if ! vpas="$(kubectl get "${VPA_RESOURCE}" -n "${ns}" -l "app.kubernetes.io/managed-by=${MANAGED_BY}" -o json 2>"${errfile}")"; then
      die "listing the VPAs in ${ns} failed: $(cat "${errfile}")"
    fi

    # One "<vpa name><TAB><compact snapshot>" line per VPA that has a
    # recommendation and whose workload this pass still lists.
    dir="${out}/snapshots/${ns}"
    mkdir -p "${dir}"
    # shellcheck disable=SC2016 # $w, $ns, $t, $wl and $byref are jq variables
    while IFS= read -r line; do
      name="${line%%$'\t'*}"
      doc="${line#*$'\t'}"
      printf '%s\n' "${doc}" >"${dir}/${name}.json.tmp"
      mv -f "${dir}/${name}.json.tmp" "${dir}/${name}.json"
    done < <(jq -r --slurpfile w "${workloads_file}" --arg ns "${ns}" --arg t "${pass_time}" '
        ($w[0].items | map({key: "\(.kind)/\(.metadata.name)", value: .}) | from_entries) as $byref
        | .items[]
        | select((.status.recommendation.containerRecommendations // []) | length > 0)
        | $byref["\(.spec.targetRef.kind)/\(.spec.targetRef.name)"] as $wl
        | select($wl != null)
        | .metadata.name + "\t" + ({
            namespace: $ns,
            vpa: .metadata.name,
            time: $t,
            recommendation: .status.recommendation,
            workload: {
              kind: $wl.kind,
              name: $wl.metadata.name,
              replicas: ($wl.spec.replicas // null),
              owner: ([($wl.metadata.ownerReferences // [])[]
                       | select(.controller == true) | {kind, name}] | first),
              app: ($wl.spec.template.metadata.labels["app.kubernetes.io/name"] // null),
              component: ($wl.spec.template.metadata.labels["app.kubernetes.io/component"] // null)
            },
            containers: [($wl.spec.template.spec.containers // [])[]
                         | {name, command: (.command // []), args: (.args // []),
                            resources: (.resources // {})}]
          } | tojson)' <<<"${vpas}")
  done
}

# ---------------------------------------------------------------------------
# cmd_watch <out-dir> <namespace>... — Snapshot every 60 seconds until killed.
# ---------------------------------------------------------------------------
cmd_watch() {
  local out="$1"
  shift
  mkdir -p "${out}"
  echo "$$" >"${out}/watch.pid"

  local pass_err line
  pass_err="$(mktemp)"
  while true; do
    # The subshell keeps die's exit 2 from ending the loop: a transient API
    # error must not end a measurement that runs for hours.
    if ( snapshot_pass "${out}" "$@" ) 2>"${pass_err}"; then
      cat "${pass_err}" >&2
    else
      while IFS= read -r line; do
        echo "WARNING: ${line}" >&2
      done <"${pass_err}"
    fi
    sleep "${WATCH_INTERVAL_SECONDS}"
  done
}

# ---------------------------------------------------------------------------
# cmd_report <out-dir> — Write recommendations.tsv and recommendations.md.
# ---------------------------------------------------------------------------
cmd_report() {
  local out="$1"
  local files=() f
  if [[ -d "${out}/snapshots" ]]; then
    while IFS= read -r f; do
      files+=("${f}")
    done < <(find "${out}/snapshots" -mindepth 2 -maxdepth 2 -type f -name '*.json' | sort)
  fi
  if [[ "${#files[@]}" -eq 0 ]]; then
    echo "ci-vpa-recommendations: no recommendation was recorded under ${out}/snapshots" >&2
    exit 1
  fi
  for f in "${files[@]}"; do
    if ! jq empty "${f}" 2>"${errfile}"; then
      die "cannot parse snapshot ${f}: $(cat "${errfile}")"
    fi
  done

  local recommender
  recommender="$(kubectl get deployments -n kube-system -l app.kubernetes.io/component=recommender \
    --request-timeout=10s -o jsonpath='{.items[0].spec.template.spec.containers[0].image}' 2>/dev/null || true)"
  local comment
  comment="run=${GITHUB_RUN_ID:--} attempt=${GITHUB_RUN_ATTEMPT:--} sha=${GITHUB_SHA:--} job=${GITHUB_JOB:--} leg=${MEASURE_LEG:--} recommender=${recommender:--}"

  # factors, roundup and qty follow hack/ci-check-node-budget.sh: qty parses a
  # quantity into millicores (cpu) or bytes (memory), rounded up the way
  # Quantity.MilliValue() and Quantity.Value() round.
  # shellcheck disable=SC2016 # $vars below are jq variables, not shell ones
  local jq_program='
def factors:
  {
    cpu: {"": 1000, m: 1, k: 1e6, M: 1e9, G: 1e12, T: 1e15,
          Ki: 1024000, Mi: 1048576000, Gi: 1073741824000, Ti: 1099511627776000},
    memory: {"": 1, m: 0.001, k: 1e3, M: 1e6, G: 1e9, T: 1e12,
             Ki: 1024, Mi: 1048576, Gi: 1073741824, Ti: 1099511627776}
  };

def roundup: if . > 0 then . - 1e-9 | ceil else 0 end;

def qty($res; $where):
  tostring as $q
  | ([$q | capture("^(?<n>[+-]?([0-9]+(\\.[0-9]*)?|\\.[0-9]+)([eE][+-]?[0-9]+)?)(?<s>Ki|Mi|Gi|Ti|m|k|M|G|T)?$")] | first) as $c
  | if $c == null then
      "ci-vpa-recommendations: cannot parse snapshot \($where): quantity \"\($q)\"\n" | halt_error(2)
    else
      ($c.n | tonumber) * factors[$res][$c.s // ""] | roundup
    end;

# A quantity in the column unit: millicores for cpu, MiB for memory, or "-"
# when absent.
def col($res; $where):
  if . == null then "-"
  elif $res == "cpu" then qty("cpu"; $where)
  else qty("memory"; $where) / 1048576 | roundup
  end;

# The value after a flag in an argv, as "--flag N" or "--flag=N".
def flagval($argv; $flag):
  ([range(0; $argv | length) as $i | select($argv[$i] == $flag) | $argv[$i + 1]
    | select(. != null)]
   + [$argv[] | strings | select(startswith($flag + "=")) | ltrimstr($flag + "=")])
  | first;

def dash: if . == null then "-" else tostring end;

[inputs | {doc: ., file: input_filename}]
| [.[] | .file as $f | .doc as $s
   | ($s.recommendation.containerRecommendations // [])[]
   | . as $r
   | ([$s.containers[]? | select(.name == $r.containerName)] | first // {}) as $c
   | (($c.command // []) + ($c.args // [])) as $argv
   | {
       namespace: $s.namespace,
       kind: $s.workload.kind,
       workload: $s.workload.name,
       replicas: ($s.workload.replicas | dash),
       owner_kind: ($s.workload.owner.kind | dash),
       owner_name: ($s.workload.owner.name | dash),
       app: ($s.workload.app | dash),
       component: ($s.workload.component | dash),
       container: $r.containerName,
       processes: (flagval($argv; "--processes") | dash),
       threads: ((flagval($argv; "--threads") // flagval($argv; "--n-threads")) | dash),
       cpu_target_m: ($r.target.cpu | col("cpu"; $f)),
       memory_target_mi: ($r.target.memory | col("memory"; $f)),
       cpu_upper_m: ($r.upperBound.cpu | col("cpu"; $f)),
       memory_upper_mi: ($r.upperBound.memory | col("memory"; $f)),
       cpu_request_m: ($c.resources.requests.cpu | col("cpu"; $f)),
       memory_request_mi: ($c.resources.requests.memory | col("memory"; $f)),
       memory_limit_mi: ($c.resources.limits.memory | col("memory"; $f)),
       snapshot: $s.time
     }]
| sort_by([.namespace, .workload, .container, .kind])
| (["namespace", "kind", "workload", "replicas", "owner_kind", "owner_name",
    "app", "component", "container", "processes", "threads", "cpu_target_m",
    "memory_target_mi", "cpu_upper_m", "memory_upper_mi", "cpu_request_m",
    "memory_request_mi", "memory_limit_mi", "snapshot"]) as $cols
| if $format == "tsv" then
    ("# \($comment)", ($cols | @tsv), (.[] | [.[$cols[]] | tostring] | @tsv))
  else
    ("Sizing measurement: `\($comment)`", "",
     "| " + ($cols | join(" | ")) + " |",
     "|" + ($cols | map(" --- ") | join("|")) + "|",
     (.[] | "| " + ([.[$cols[]] | tostring] | join(" | ")) + " |"))
  end
'

  local format
  for format in tsv md; do
    if ! jq -rn --arg format "${format}" --arg comment "${comment}" "${jq_program}" "${files[@]}" \
        >"${out}/recommendations.${format}.tmp"; then
      rm -f "${out}/recommendations.${format}.tmp"
      exit 2
    fi
    mv -f "${out}/recommendations.${format}.tmp" "${out}/recommendations.${format}"
  done
  cat "${out}/recommendations.md"
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------
[[ $# -ge 1 ]] || usage
subcommand="$1"
shift
case "${subcommand}" in
  snapshot | watch)
    [[ $# -ge 2 && -n "$1" ]] || usage
    ;;
  report)
    [[ $# -eq 1 && -n "$1" ]] || usage
    ;;
  *)
    usage
    ;;
esac

command -v jq >/dev/null 2>&1 || die "jq is required"

case "${subcommand}" in
  snapshot)
    mkdir -p "$1"
    snapshot_pass "$@"
    ;;
  watch)
    cmd_watch "$@"
    ;;
  report)
    cmd_report "$1"
    ;;
esac
