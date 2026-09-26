#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e-autoscaling/keystone-hpa.sh: name the HPA of the suite's Keystone
# child and its scale target, and clear the pods that skew its scale-down.
#
# Usage:
#   keystone-hpa.sh hpa            print the HPA's name
#   keystone-hpa.sh deploy         print the HPA's scale-target Deployment
#   keystone-hpa.sh drop-job-pods  delete the child's Succeeded Job pods
#
# drop-job-pods: the HPA reads every pod its scale target's selector matches.
# The Keystone API Deployment selects on the name and instance labels alone,
# which the pods of the hourly trust-flush CronJob (and of the key rotations)
# carry too. A Completed one has no metrics, and on a scale-down the HPA
# counts a pod without metrics at the full target, which holds the fleet one
# pod above its minimum. A step that waits for a scale-in calls this on every
# poll, so the scale-in it measures is the behavior's alone.

set -euo pipefail

NS="openstack"
KEYSTONE="cp-autoscaling-keystone"

hpa() {
  kubectl get hpa -n "${NS}" -l "app.kubernetes.io/instance=${KEYSTONE}" -o jsonpath='{.items[0].metadata.name}'
}

case "${1:-}" in
  hpa)
    hpa
    ;;
  deploy)
    name="$(hpa)"
    kubectl get hpa "${name}" -n "${NS}" -o jsonpath='{.spec.scaleTargetRef.name}'
    ;;
  drop-job-pods)
    kubectl delete pods -n "${NS}" --ignore-not-found --field-selector=status.phase==Succeeded \
      -l "app.kubernetes.io/name=keystone,app.kubernetes.io/instance=${KEYSTONE},job-name" >/dev/null
    ;;
  *)
    echo "usage: $0 hpa|deploy|drop-job-pods" >&2
    exit 2
    ;;
esac
