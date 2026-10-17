#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Helpers of keystone-api-stress-autoscaling, sourced by its scripts from the
# suite directory with NAMESPACE set by Chainsaw.

# shellcheck source=tests/e2e-chaos/lib/faults.sh
. ../lib/faults.sh

HPA_KS=keystone-chaos-hpa
VPA_KS=keystone-chaos-vpa
STATE=keystone-stress-state

# api_pod <instance>: the name of a running API pod of a Keystone.
api_pod() {
  kubectl get pods -n "$NAMESPACE" --field-selector=status.phase=Running \
    -l "app.kubernetes.io/name=keystone,app.kubernetes.io/instance=$1,app.kubernetes.io/component=api" \
    -o jsonpath='{.items[0].metadata.name}'
}

# quantity_bytes <quantity>: a Kubernetes memory quantity such as 250Mi,
# 262144k or 1.5Gi in bytes. CPU quantities go through quantity_millicores.
quantity_bytes() {
  python3 -c '
import re, sys
q = sys.argv[1].strip()
m = re.fullmatch(r"([0-9.]+)([a-zA-Z]*)", q)
if not m:
    sys.exit("not a quantity: %r" % q)
units = {"": 1, "k": 1e3, "M": 1e6, "G": 1e9, "T": 1e12,
         "Ki": 2**10, "Mi": 2**20, "Gi": 2**30, "Ti": 2**40}
print(int(float(m.group(1)) * units[m.group(2)]))
' "$1"
}

# quantity_millicores <quantity>: a CPU quantity such as 1500m, 2 or
# 1234567890n in millicores.
quantity_millicores() {
  python3 -c '
import re, sys
q = sys.argv[1].strip()
m = re.fullmatch(r"([0-9.]+)([num]?)", q)
if not m:
    sys.exit("not a CPU quantity: %r" % q)
scale = {"": 1000, "m": 1, "u": 1e-3, "n": 1e-6}[m.group(2)]
print(int(float(m.group(1)) * scale))
' "$1"
}

# cpu_millicores <pod> <container>: the CPU usage metrics-server reports for
# the container, in millicores; empty while it reports none.
cpu_millicores() {
  local usage
  usage="$(kubectl get --raw "/apis/metrics.k8s.io/v1beta1/namespaces/${NAMESPACE}/pods/$1" 2>/dev/null |
    jq -r --arg c "$2" '.containers[] | select(.name == $c) | .usage.cpu' 2>/dev/null || true)"
  if [ -n "$usage" ]; then
    quantity_millicores "$usage"
  fi
}

# vpa_target: the memory target the VPA of keystone-chaos-vpa recommends for
# the keystone container, in bytes; empty while it recommends none.
vpa_target() {
  local target
  target="$(kubectl get vpa -n "$NAMESPACE" -l "app.kubernetes.io/instance=${VPA_KS}" \
    -o jsonpath='{.items[0].status.recommendation.containerRecommendations[?(@.containerName=="keystone")].target.memory}' 2>/dev/null || true)"
  if [ -n "$target" ]; then
    quantity_bytes "$target"
  fi
}

# drop_job_pods: delete the Succeeded Job pods of keystone-chaos-hpa. The HPA
# counts every pod its target's selector matches, and a completed Job pod,
# which carries the name and instance labels too, holds the fleet one pod
# above its minimum (tests/e2e-autoscaling/keystone-hpa.sh).
drop_job_pods() {
  kubectl delete pods -n "$NAMESPACE" --ignore-not-found --field-selector=status.phase==Succeeded \
    -l "app.kubernetes.io/name=keystone,app.kubernetes.io/instance=${HPA_KS},job-name" >/dev/null
}
