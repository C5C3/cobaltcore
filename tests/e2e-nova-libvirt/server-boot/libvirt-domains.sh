#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e-nova-libvirt/server-boot/libvirt-domains.sh: destroy and undefine
# every libvirt domain on the node through the libvirt-e2e pod of
# 00-libvirt.yaml, then fail when libvirt still lists one or does not answer.
#
# QEMU outlives that pod (hostPID) and the domain definitions are the node's,
# so a domain one suite leaves behind would run on under the next suite's
# libvirtd and count among its domains. Both suites of the root run this
# script in step 3, once libvirtd is up and before the pool exists, and in the
# cleanup of step 3, before the DaemonSet goes. undefine also removes a
# managed-save image, which Tempest's suspend leaves, because plain undefine
# refuses a domain that has one.
#
# A destroy or undefine that fails, as for a domain that stopped after the
# list, does not end the script: the last list of each pod is the verdict.
#
# Usage: libvirt-domains.sh, with NAMESPACE set.

set -uo pipefail

# virsh_in <pod> <args...>: virsh against the libvirtd of the pod.
virsh_in() {
  local pod="$1"
  shift
  kubectl exec -n "$NAMESPACE" "$pod" -c libvirtd -- virsh -c qemu:///system "$@"
}

# A libvirtd that a fault replaced may still be starting.
kubectl wait pod -n "$NAMESPACE" -l app.kubernetes.io/name=libvirt-e2e \
  --for=condition=Ready --timeout=120s || true
pods="$(kubectl get pod -n "$NAMESPACE" -o name -l app.kubernetes.io/name=libvirt-e2e)" || exit 1

rc=0
for pod in $pods; do
  for domain in $(virsh_in "$pod" list --name); do
    virsh_in "$pod" destroy "$domain" || true
  done
  for domain in $(virsh_in "$pod" list --all --name); do
    virsh_in "$pod" undefine --managed-save --snapshots-metadata --nvram "$domain" || true
  done
  if ! left="$(virsh_in "$pod" list --all --name)"; then
    echo "ERROR: libvirt in $pod does not answer, so its domains are unknown"
    rc=1
    continue
  fi
  left="$(sed '/^[[:space:]]*$/d' <<<"$left" | paste -sd ' ' -)"
  if [ -n "$left" ]; then
    echo "ERROR: libvirt in $pod still lists $left after the destroy and undefine"
    rc=1
    continue
  fi
  echo "OK: libvirt in $pod lists no domain"
done
exit "$rc"
