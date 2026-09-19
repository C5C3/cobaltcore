#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-dump-diagnostics.sh — Dump diagnostic info after E2E failures.
#
# Consolidates diagnostic dump logic shared across e2e-infra, e2e-operator,
# and tempest CI jobs into a single script. Usable locally against any
# kubeconfig.
#
# Usage:
#   hack/ci-dump-diagnostics.sh                   # infra-only diagnostics
#   OPERATOR=keystone hack/ci-dump-diagnostics.sh  # + operator-specific diagnostics
#   OPERATOR=nova OPERATOR_ONLY=1 hack/ci-dump-diagnostics.sh  # the operator sections alone
#   KIND_CLUSTER=cobaltcore hack/ci-dump-diagnostics.sh  # kind cluster whose node answers dmesg (default: cobaltcore)
#
# Shared diagnostic dump script for all E2E jobs.
# set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

# Optional: operator name for operator-specific diagnostics.
OPERATOR="${OPERATOR:-}"
NAMESPACE="${NAMESPACE:-openstack}"
# The operator Deployment lives in its own namespace (keystone-system,
# horizon-system, ...), mirroring the ci-deploy-operator.sh default. Without
# an explicit -n the pod/log lookups below silently hit the default namespace
# and the dump loses exactly the operator evidence it exists to capture.
OPERATOR_NAMESPACE="${OPERATOR_NAMESPACE:-${OPERATOR}-system}"
# The kind cluster whose node containers the kernel-OOM block below asks for
# dmesg. Matches the KIND_CLUSTER env every e2e job exports.
KIND_CLUSTER="${KIND_CLUSTER:-cobaltcore}"
# Set by a caller that loops over several operators in one job, on every run
# after the first. Everything it turns off is invariant across the loop: the
# infrastructure sections read the whole cluster, and the Job and pod sections
# of the operator block read ${NAMESPACE}, which does not change with OPERATOR
# either. Repeating them per operator buries the three sections the loop is for
# under a sixth identical dump, and spends the grace window the `Delete kind
# cluster` step after it needs when the job wall cancels the run.
OPERATOR_ONLY="${OPERATOR_ONLY:-}"

if [[ -z "${OPERATOR_ONLY}" ]]; then
  # ---------------------------------------------------------------------------
  # Infrastructure diagnostics (always emitted)
  # ---------------------------------------------------------------------------
  echo "=== HelmReleases ==="
  kubectl get helmrelease --all-namespaces || true

  echo "=== Pods ==="
  kubectl get pods --all-namespaces || true

  echo "=== DaemonSets ==="
  kubectl get daemonsets --all-namespaces -o wide || true

  # ---------------------------------------------------------------------------
  # Node pressure (always emitted)
  # ---------------------------------------------------------------------------
  # A kind node that runs out of RAM leaves almost no trace in the pod table:
  # the kernel OOM killer takes the largest BestEffort process, the kubelet
  # restarts the container in place, and `kubectl get pods` shows nothing but
  # a restart count. CI run 34718789784 lost openstack-db-0 five times that way,
  # each start an InnoDB crash recovery, and the suite that failed was whichever
  # one waited on DatabaseReady at that moment. The four blocks below make the
  # cause legible from the job log alone: what the node has and what is
  # requested of it, which containers died and why, who holds the memory now,
  # and what the kernel says.
  echo "=== Node capacity and allocated resources ==="
  for node in $(kubectl get nodes -o name 2>/dev/null); do
    echo "--- ${node} ---"
    kubectl describe "${node}" 2>/dev/null \
      | sed -n -e '/^Capacity:/,/^System Info:/p' -e '/^Allocated resources:/,/^Events:/p' \
      | grep -vE '^(System Info:|Events:)' || true
  done

  echo "=== Containers with restarts (last termination reason, QoS class) ==="
  kubectl get pods --all-namespaces \
    -o custom-columns='NAMESPACE:.metadata.namespace,POD:.metadata.name,RESTARTS:.status.containerStatuses[*].restartCount,LAST_REASON:.status.containerStatuses[*].lastState.terminated.reason,LAST_EXIT:.status.containerStatuses[*].lastState.terminated.exitCode,QOS:.status.qosClass' 2>/dev/null \
    | awk 'NR == 1 || $3 ~ /[1-9]/' || true

  # The kubelet summary API needs no metrics-server: it is what `kubectl top`
  # would read if one were installed. Working set is the number the OOM killer
  # and the eviction manager act on.
  echo "=== Memory working set per pod (kubelet summary, top 20 per node) ==="
  if command -v jq >/dev/null 2>&1; then
    for node in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
      kubectl get --raw "/api/v1/nodes/${node}/proxy/stats/summary" 2>/dev/null \
        | jq -r --arg node "${node}" '
            "node \($node): workingSet \((.node.memory.workingSetBytes // 0) / 1048576 | floor) MiB, available \((.node.memory.availableBytes // 0) / 1048576 | floor) MiB",
            (.pods[] | "\((.memory.workingSetBytes // 0) / 1048576 | floor) MiB\t\(.podRef.namespace)/\(.podRef.name)")' 2>/dev/null \
        | { IFS= read -r header && echo "${header}" && sort -rn | head -20; } || true
    done
  else
    echo "SKIP: jq not installed"
  fi

  # The kind node is a privileged container, so dmesg inside it reads the
  # host kernel's ring buffer, where an OOM kill names the victim, its cgroup
  # and the memory state at the time. `kubectl` cannot reach that.
  echo "=== Kernel OOM events on the kind node(s) ==="
  if command -v docker >/dev/null 2>&1; then
    found=0
    # kind labels every node container with its cluster; a name filter would
    # also match unrelated containers whose name merely contains the prefix.
    for node in $(docker ps --filter "label=io.x-k8s.kind.cluster=${KIND_CLUSTER}" --format '{{.Names}}' 2>/dev/null); do
      found=1
      echo "--- ${node} ---"
      if ! err="$(docker exec "${node}" dmesg 2>&1 >/dev/null)"; then
        echo "(dmesg unavailable in ${node}: ${err})"
        continue
      fi
      docker exec "${node}" dmesg 2>/dev/null \
        | grep -iE 'out of memory|oom-kill|killed process' | tail -30 | grep . \
        || echo "(no OOM lines in dmesg)"
    done
    [ "${found}" -eq 1 ] || echo "SKIP: no kind node container for cluster '${KIND_CLUSTER}' on this host"
  else
    echo "SKIP: docker not installed"
  fi

  # Chaos Mesh is opt-in in the kind Quick Start the chaos-mesh
  # namespace only exists when the cluster was deployed with
  # WITH_CHAOS_MESH=true. Guard the describe so the log emits an explicit
  # SKIP line instead of swallowing the not-found error silently.
  echo "=== DaemonSet chaos-daemon detail ==="
  if kubectl get ns chaos-mesh >/dev/null 2>&1; then
    kubectl describe daemonset -n chaos-mesh chaos-daemon 2>/dev/null || true
  else
    echo "SKIP: chaos-mesh namespace not present (install with WITH_CHAOS_MESH=true)"
  fi

  echo "=== Events (last 50) ==="
  kubectl get events --all-namespaces --sort-by='.lastTimestamp' | tail -50 || true

  # Diagnostics-only Flux CLI block. After the FluxInstance migration
  # the CLI is opt-in — `hack/install-test-deps.sh` only
  # installs it when `WITH_FLUX_CLI=true`, so on the default CI path this
  # branch is dormant. The `fluxinstance,fluxreport` dump below compensates.
  if command -v flux >/dev/null 2>&1; then
    echo "=== Flux logs ==="
    flux logs --all-namespaces || true
  fi

  # flux-operator state emit FluxInstance and FluxReport only when
  # the flux-operator CRDs are registered on the cluster; otherwise a plain
  # `kubectl get` would error loudly on clusters that have not been bootstrapped
  # with flux-operator yet.
  if kubectl api-resources --api-group=fluxcd.controlplane.io 2>/dev/null \
      | grep -q '^fluxinstances'; then
    echo "=== FluxInstance / FluxReport ==="
    kubectl get fluxinstance,fluxreport -A -o yaml || true
  fi
fi

# ---------------------------------------------------------------------------
# Operator-specific diagnostics (only when OPERATOR is set)
# ---------------------------------------------------------------------------
if [[ -n "${OPERATOR}" ]]; then
  echo "=== Operator pods ==="
  kubectl get pods -n "${OPERATOR_NAMESPACE}" -l "app.kubernetes.io/name=${OPERATOR}-operator" || true

  echo "=== Operator logs ==="
  kubectl logs -n "${OPERATOR_NAMESPACE}" -l "app.kubernetes.io/name=${OPERATOR}-operator" --tail=100 --prefix || true

  # The three blocks below read ${NAMESPACE}, not the operator's own, so they
  # are the same dump for every OPERATOR a looping caller passes.
  if [[ -z "${OPERATOR_ONLY}" ]]; then
    echo "=== Job descriptions ==="
    for job in $(kubectl get jobs -n "${NAMESPACE}" -o name 2>/dev/null); do
      echo "--- describe ${job} ---"
      kubectl describe -n "${NAMESPACE}" "${job}" 2>&1 | tail -30 || true
    done

    echo "=== Failed Job logs ==="
    for job in $(kubectl get jobs -n "${NAMESPACE}" -o name 2>/dev/null); do
      echo "--- logs for ${job} ---"
      kubectl logs -n "${NAMESPACE}" "${job}" --all-containers --tail=50 2>&1 || true
    done

    echo "=== All pod logs in ${NAMESPACE} ==="
    for pod in $(kubectl get pods -n "${NAMESPACE}" -o name 2>/dev/null); do
      echo "--- ${pod} (current) ---"
      kubectl logs -n "${NAMESPACE}" "${pod}" --all-containers --tail=30 2>&1 || true
      echo "--- ${pod} (previous) ---"
      kubectl logs -n "${NAMESPACE}" "${pod}" --all-containers --tail=30 --previous 2>&1 || true
    done
  fi

  echo "=== Operator CR status ==="
  kubectl get "${OPERATOR}" -n "${NAMESPACE}" -o yaml 2>/dev/null | grep -A30 "conditions:" || true

  # Namespace-wide like the three above, and kept on the same guard.
  if [[ -z "${OPERATOR_ONLY}" ]]; then
    echo "=== ConfigMaps in ${NAMESPACE} namespace ==="
    kubectl get cm -n "${NAMESPACE}" 2>/dev/null || true
  fi
fi
