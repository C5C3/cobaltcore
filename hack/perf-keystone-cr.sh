#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/perf-keystone-cr.sh — the Keystone CR the performance measurements
# apply. hack/perf-reconcile-benchmark.sh and hack/perf-token-baseline.sh
# source it, so both measure the same spec.

# perf_keystone_cr <name> <database> — print one Keystone CR named <name> with
# its own <database>. NAMESPACE, IMAGE_REPOSITORY, IMAGE_TAG, DB_CLUSTER_REF
# and CACHE_CLUSTER_REF come from the sourcing script.
perf_keystone_cr() {
  cat <<EOF
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: Keystone
metadata:
  name: $1
  namespace: ${NAMESPACE}
spec:
  deployment:
    replicas: 1
  image:
    repository: ${IMAGE_REPOSITORY}
    tag: "${IMAGE_TAG}"
  database:
    clusterRef:
      name: ${DB_CLUSTER_REF}
    database: $2
    secretRef:
      name: keystone-db
  cache:
    clusterRef:
      name: ${CACHE_CLUSTER_REF}
    backend: dogpile.cache.pymemcache
  fernet:
    rotationSchedule: "0 0 * * 0"
    maxActiveKeys: 3
  bootstrap:
    adminUser: admin
    adminPasswordSecretRef:
      name: keystone-admin
    region: RegionOne
EOF
}
