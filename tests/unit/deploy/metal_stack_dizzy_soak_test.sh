#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the overlay of the dizzy soak, deploy/lab/metal-stack/dizzy-soak
# (#1274):
#   1. The directory holds kustomization.yaml, identity.yaml, rbac.yaml,
#      reports-pvc.yaml, job.yaml and scenario.yaml and nothing else, each
#      with the SPDX pair.
#   2. The kustomization lists identity.yaml, rbac.yaml and reports-pvc.yaml
#      and names no parent-directory path.
#   3. kustomize build renders the nine objects alone: the K-ORC Domain
#      dizzy-soak-domain (an import of Default), the Project and User
#      dizzy-soak, the Role dizzy-soak-admin-role (an import of admin) and
#      the RoleAssignment dizzy-soak-admin in openstack, each with the
#      credentials k-orc-clouds-yaml/admin; the ServiceAccount dizzy-soak and
#      the claim dizzy-soak-reports (ReadWriteOnce, 5Gi, no class) in dizzy;
#      and the ClusterRole and ClusterRoleBinding dizzy-soak-platform-reader.
#   4. The ClusterRole grants get and list alone, names no wildcard and no
#      secrets, and covers pods, namespaces, the pods of metrics.k8s.io and
#      the thirteen kinds whose conditions the runner reads, the
#      CONDITION_KINDS of hack/dizzy-soak-runner.sh.
#   5. job.yaml, read by file, is the batch/v1 Job dizzy-soak in dizzy with
#      the fields of the issue's boundary 6, the runner image pinned by tag
#      and digest, the dizzy image ghcr.io/b42labs/dizzy:DIZZY_VERSION once
#      and no version number. A copy that names a version fails that check.
#   6. scenario.yaml names the image and the flavor of the hypervisor
#      fixtures, turns off resize and cold migration of the Legacy persona,
#      keeps every lane off and every other value of dizzy's mix small
#      profile.
#   7. When bin/dizzy is the pinned version, `dizzy mix generate` plans the
#      scenario as ci, gardener and legacy with 3, 2 and 1 servers and no
#      lane.
#
# Checks 3 and 4 count as SKIP without kustomize or yq, 5 and 6 without yq,
# 7 without the pinned bin/dizzy or jq.
#
# Usage: bash tests/unit/deploy/metal_stack_dizzy_soak_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/kustomize_render.sh
source "$PROJECT_ROOT/tests/lib/kustomize_render.sh"

SOAK_DIR="$PROJECT_ROOT/deploy/lab/metal-stack/dizzy-soak"
JOB="$SOAK_DIR/job.yaml"
SCENARIO="$SOAK_DIR/scenario.yaml"
FIXTURES_DIR="$PROJECT_ROOT/deploy/kind/hypervisor-operator-fixtures"
DIZZY_SH="$PROJECT_ROOT/hack/dizzy.sh"
RUNNER_SH="$PROJECT_ROOT/hack/dizzy-soak-runner.sh"

RUNNER_IMAGE_PATTERN='^docker\.io/alpine/k8s:[^@]+@sha256:[a-f0-9]{64}$'
DIZZY_IMAGE='ghcr.io/b42labs/dizzy:DIZZY_VERSION'

RENDERED=""

# job_value <expression> — the first value of a yq expression over job.yaml.
job_value() {
  yq -N -r "select(. != null) | $1" "$JOB" | head -n 1
}

# obj <kind> <name> <expression> — the value of <expression> on the rendered
# object of <kind> named <name>. The object is selected first, in a yq run
# of its own that keeps the document separators: an array built after a
# select in the same run also yields an empty value for every document the
# select drops.
obj() {
  printf '%s\n' "$RENDERED" |
    yq "select(.kind == \"$1\" and .metadata.name == \"$2\")" - |
    yq -N -r "$3" -
}

# scenario_value <expression> — the same over scenario.yaml.
scenario_value() {
  yq -N -r "$1" "$SCENARIO"
}

# names_no_version <file> — succeeds when the file names no vX.Y.Z version
# and the dizzy image token exactly once.
names_no_version() {
  ! grep -qE 'v[0-9]+\.[0-9]+\.[0-9]+' "$1" &&
    [[ "$(grep -cF "$DIZZY_IMAGE" "$1")" == "1" ]]
}

# --- Test 1: the six files ---
test_files_exist_with_spdx() {
  echo "Test: the overlay holds its six files, each with the SPDX pair"

  assert_eq "the directory holds the six files and nothing else" \
    "identity.yaml job.yaml kustomization.yaml rbac.yaml reports-pvc.yaml scenario.yaml" \
    "$(cd "$SOAK_DIR" 2>/dev/null && find . -type f | sed 's#^\./##' | sort | tr '\n' ' ' | sed 's/ $//')"
  local f
  for f in kustomization.yaml identity.yaml rbac.yaml reports-pvc.yaml job.yaml scenario.yaml; do
    if [[ ! -f "$SOAK_DIR/$f" ]]; then
      echo "  FAIL: $f does not exist"
      FAIL=$((FAIL + 2))
      continue
    fi
    assert_file_contains "$f has the SPDX-FileCopyrightText header" \
      "$SOAK_DIR/$f" "SPDX-FileCopyrightText: Copyright 2026 SAP SE"
    assert_file_contains "$f has SPDX-License-Identifier: Apache-2.0" \
      "$SOAK_DIR/$f" "SPDX-License-Identifier: Apache-2.0"
  done
}

# --- Test 2: the kustomization's resources ---
test_kustomization_resources() {
  echo "Test: the kustomization lists the three files and no parent directory"

  assert_eq "resources lists identity.yaml, rbac.yaml and reports-pvc.yaml" \
    "identity.yaml rbac.yaml reports-pvc.yaml" \
    "$(resource_entries "$SOAK_DIR/kustomization.yaml" | tr '\n' ' ' | sed 's/ $//')"
  local parent_refs
  parent_refs="$( { grep -vE '^[[:space:]]*#' "$SOAK_DIR/kustomization.yaml" | grep -F '../' || true; } | wc -l)"
  assert_eq "kustomization.yaml names no '../' path" "0" "${parent_refs// /}"
}

# --- Test 3: the nine rendered objects ---
test_render() {
  echo "Test: the render holds the nine objects with their fields"

  render "$SOAK_DIR" 22 || return

  local objects
  objects="$(printf '%s\n' "$RENDERED" |
    yq -N -r '.kind + " " + (.metadata.namespace // "-") + " " + .metadata.name' - | sort)"
  assert_eq "the render holds the nine objects alone" "$(printf '%s\n' \
    'ClusterRole - dizzy-soak-platform-reader' \
    'ClusterRoleBinding - dizzy-soak-platform-reader' \
    'Domain openstack dizzy-soak-domain' \
    'PersistentVolumeClaim dizzy dizzy-soak-reports' \
    'Project openstack dizzy-soak' \
    'Role openstack dizzy-soak-admin-role' \
    'RoleAssignment openstack dizzy-soak-admin' \
    'ServiceAccount dizzy dizzy-soak' \
    'User openstack dizzy-soak')" "$objects"

  local korc kind name
  korc="$(printf '%s\n' "$RENDERED" |
    yq 'select(.apiVersion == "openstack.k-orc.cloud/v1alpha1")' - |
    yq -N -r '.kind + " " + .metadata.name' -)"
  while read -r kind name; do
    assert_eq "$kind/$name authenticates as k-orc-clouds-yaml/admin" "k-orc-clouds-yaml/admin" \
      "$(obj "$kind" "$name" '.spec.cloudCredentialsRef.secretName + "/" + .spec.cloudCredentialsRef.cloudName')"
  done <<<"$korc"

  assert_eq "the Domain imports Default" "unmanaged Default" \
    "$(obj Domain dizzy-soak-domain '.spec.managementPolicy + " " + .spec.import.filter.name')"
  assert_eq "the Project is managed and named dizzy-soak in that domain" "managed dizzy-soak dizzy-soak-domain" \
    "$(obj Project dizzy-soak '.spec.managementPolicy + " " + .spec.resource.name + " " + .spec.resource.domainRef')"
  assert_eq "the User is managed and named dizzy-soak" "managed dizzy-soak" \
    "$(obj User dizzy-soak '.spec.managementPolicy + " " + .spec.resource.name')"
  assert_eq "the User lives in that domain" "dizzy-soak-domain" "$(obj User dizzy-soak '.spec.resource.domainRef')"
  assert_eq "its default project is dizzy-soak" "dizzy-soak" "$(obj User dizzy-soak '.spec.resource.defaultProjectRef')"
  assert_eq "its password comes from dizzy-soak-user-password" "dizzy-soak-user-password" \
    "$(obj User dizzy-soak '.spec.resource.passwordRef')"
  assert_eq "the Role imports admin" "unmanaged admin" \
    "$(obj Role dizzy-soak-admin-role '.spec.managementPolicy + " " + .spec.import.filter.name')"
  assert_eq "the RoleAssignment is managed" "managed" "$(obj RoleAssignment dizzy-soak-admin '.spec.managementPolicy')"
  assert_eq "and assigns that role to that user on that project" "dizzy-soak-admin-role dizzy-soak dizzy-soak" \
    "$(obj RoleAssignment dizzy-soak-admin '.spec.resource.roleRef + " " + .spec.resource.userRef + " " + .spec.resource.projectRef')"

  assert_eq "the binding names the ClusterRole" "ClusterRole/dizzy-soak-platform-reader" \
    "$(obj ClusterRoleBinding dizzy-soak-platform-reader '.roleRef.kind + "/" + .roleRef.name')"
  assert_eq "and binds the ServiceAccount dizzy/dizzy-soak alone" "ServiceAccount dizzy dizzy-soak" \
    "$(obj ClusterRoleBinding dizzy-soak-platform-reader '.subjects[] | [.kind, .namespace, .name] | join(" ")')"
  assert_eq "the binding has one subject" "1" \
    "$(obj ClusterRoleBinding dizzy-soak-platform-reader '.subjects | length')"

  assert_eq "the claim is ReadWriteOnce" "ReadWriteOnce" \
    "$(obj PersistentVolumeClaim dizzy-soak-reports '.spec.accessModes | join(",")')"
  assert_eq "the claim requests 5Gi" "5Gi" \
    "$(obj PersistentVolumeClaim dizzy-soak-reports '.spec.resources.requests.storage')"
  assert_eq "the claim names no storage class" "absent" \
    "$(obj PersistentVolumeClaim dizzy-soak-reports '.spec.storageClassName // "absent"')"
  assert_eq "nothing renders a Namespace" "0" \
    "$(printf '%s\n' "$RENDERED" | yq -N -r 'select(.kind == "Namespace") | .metadata.name' - | grep -c .)"
}

# --- Test 4: the ClusterRole reads and nothing else ---
# shellcheck disable=SC2016 # $g is a yq variable, not expanded here
test_cluster_role() {
  echo "Test: the ClusterRole grants get and list on the platform's pods, metrics and service kinds"

  render "$SOAK_DIR" 5 || return

  assert_eq "the verbs are get and list alone" "get list" \
    "$(obj ClusterRole dizzy-soak-platform-reader '[.rules[].verbs[]] | unique | join(" ")')"
  assert_eq "every rule grants both" "true" \
    "$(obj ClusterRole dizzy-soak-platform-reader '[.rules[] | (.verbs | sort | join(",")) == "get,list"] | all')"
  # Compared outside yq, whose == reads "*" as a glob that matches anything.
  assert_eq "no rule holds a wildcard" "0" \
    "$(obj ClusterRole dizzy-soak-platform-reader '.rules[] | (.apiGroups + .resources + .verbs)[]' | grep -cxF '*')"
  assert_eq "no rule names secrets" "0" \
    "$(obj ClusterRole dizzy-soak-platform-reader '[.rules[].resources[] | select(. == "secrets")] | length')"
  # The kinds come from the runner itself; its BASH_SOURCE guard keeps
  # main() from running when it is sourced.
  local want
  want="$( {
    bash -c 'source "$1" && printf "%s\n" $CONDITION_KINDS' _ "$RUNNER_SH"
    printf '%s\n' namespaces pods pods.metrics.k8s.io
  } | sort)"
  assert_eq "the resources are pods, namespaces, the pod metrics and every kind the runner reads" "$want" \
    "$(obj ClusterRole dizzy-soak-platform-reader '.rules[] | .apiGroups[] as $g | .resources[] |
      (select($g == "") // (. + "." + $g))' | sort)"
}

# --- Test 5: the Job template ---
test_job() {
  echo "Test: job.yaml is the soak Job of boundary 6 with one dizzy image token"

  if ! have yq; then
    echo "  SKIP: yq not installed (39 checks skipped)"
    SKIP=$((SKIP + 39))
    return
  fi

  assert_eq "it is a batch/v1 Job" "batch/v1 Job" "$(job_value '.apiVersion + " " + .kind')"
  assert_eq "named dizzy-soak in dizzy" "dizzy/dizzy-soak" "$(job_value '.metadata.namespace + "/" + .metadata.name')"
  assert_eq "it holds one document" "1" "$(yq -N -r 'select(. != null) | .kind' "$JOB" | grep -c .)"
  assert_eq "backoffLimit is 0" "0" "$(job_value '.spec.backoffLimit')"

  local pod='.spec.template.spec'
  assert_eq "restartPolicy is Never" "Never" "$(job_value "$pod.restartPolicy")"
  assert_eq "the ServiceAccount is dizzy-soak" "dizzy-soak" "$(job_value "$pod.serviceAccountName")"
  assert_eq "the grace period is 540s" "540" "$(job_value "$pod.terminationGracePeriodSeconds")"
  assert_eq "the pod runs as non-root" "true" "$(job_value "$pod.securityContext.runAsNonRoot")"
  assert_eq "as 65534:65534 with fsGroup 65534" "65534 65534 65534" \
    "$(job_value "$pod.securityContext | [.runAsUser, .runAsGroup, .fsGroup] | map(tostring) | join(\" \")")"
  assert_eq "with the RuntimeDefault seccomp profile" "RuntimeDefault" "$(job_value "$pod.securityContext.seccompProfile.type")"

  assert_eq "one init container, dizzy" "dizzy" "$(job_value "[$pod.initContainers[].name] | join(\" \")")"
  assert_eq "its image is the token image" "$DIZZY_IMAGE" "$(job_value "$pod.initContainers[0].image")"
  assert_eq "it copies the binary to /tools" "cp /usr/local/bin/dizzy /tools/dizzy" \
    "$(job_value "$pod.initContainers[0].command | join(\" \")")"
  assert_eq "it mounts tools at /tools" "tools:/tools" \
    "$(job_value "$pod.initContainers[0].volumeMounts | map(.name + \":\" + .mountPath) | join(\" \")")"
  assert_eq "its root filesystem is read-only" "true" "$(job_value "$pod.initContainers[0].securityContext.readOnlyRootFilesystem")"

  assert_eq "one container, runner" "runner" "$(job_value "[$pod.containers[].name] | join(\" \")")"
  local image
  image="$(job_value "$pod.containers[0].image")"
  if [[ "$image" =~ $RUNNER_IMAGE_PATTERN ]]; then
    echo "  PASS: the runner image is pinned by tag and digest ($image)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the runner image '$image' does not match $RUNNER_IMAGE_PATTERN"
    FAIL=$((FAIL + 1))
  fi
  assert_eq "it runs the runner script with bash" "bash /runner/dizzy-soak-runner.sh" \
    "$(job_value "$pod.containers[0].command | join(\" \")")"
  assert_eq "HOME is /tmp" "/tmp" "$(job_value "$pod.containers[0].env[] | select(.name == \"HOME\") | .value")"
  assert_eq "its settings come from dizzy-soak-config" "dizzy-soak-config" \
    "$(job_value "$pod.containers[0].envFrom | map(.configMapRef.name) | join(\" \")")"
  assert_eq "it requests 100m and 256Mi" "100m 256Mi" \
    "$(job_value "$pod.containers[0].resources.requests | .cpu + \" \" + .memory")"
  assert_eq "it sets no limits" "absent" "$(job_value "$pod.containers[0].resources.limits // \"absent\"")"
  assert_eq "it mounts the five volumes" "tools:/tools runner:/runner scenario:/scenario clouds:/etc/openstack reports:/reports" \
    "$(job_value "$pod.containers[0].volumeMounts | map(.name + \":\" + .mountPath) | join(\" \")")"
  assert_eq "the runner, scenario and clouds mounts are read-only" "runner scenario clouds" \
    "$(job_value "$pod.containers[0].volumeMounts | map(select(.readOnly == true) | .name) | join(\" \")")"

  local c
  for c in initContainers containers; do
    assert_eq "$c: no privilege escalation" "false" \
      "$(job_value "[$pod.${c}[].securityContext.allowPrivilegeEscalation] | unique | map(tostring) | join(\" \")")"
    assert_eq "$c: every capability dropped" "ALL" \
      "$(job_value "[$pod.${c}[].securityContext.capabilities.drop[]] | unique | join(\" \")")"
    assert_eq "$c: pulled IfNotPresent" "IfNotPresent" \
      "$(job_value "[$pod.${c}[].imagePullPolicy] | unique | join(\" \")")"
  done

  assert_eq "tools is an emptyDir" "{}" "$(job_value "$pod.volumes[] | select(.name == \"tools\") | .emptyDir | to_json")"
  assert_eq "runner is the ConfigMap dizzy-soak-runner" "dizzy-soak-runner" \
    "$(job_value "$pod.volumes[] | select(.name == \"runner\") | .configMap.name")"
  assert_eq "scenario is the key scenario.yaml of dizzy-soak-scenario" "dizzy-soak-scenario scenario.yaml" \
    "$(job_value "$pod.volumes[] | select(.name == \"scenario\") | .configMap.name + \" \" + .configMap.items[0].key")"
  assert_eq "clouds is the key clouds.yaml of dizzy-soak-clouds" "dizzy-soak-clouds clouds.yaml" \
    "$(job_value "$pod.volumes[] | select(.name == \"clouds\") | .secret.secretName + \" \" + .secret.items[0].key")"
  assert_eq "reports is the claim dizzy-soak-reports" "dizzy-soak-reports" \
    "$(job_value "$pod.volumes[] | select(.name == \"reports\") | .persistentVolumeClaim.claimName")"

  if names_no_version "$JOB"; then
    echo "  PASS: job.yaml names $DIZZY_IMAGE once and no version number"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: job.yaml names a version number or $DIZZY_IMAGE other than once"
    FAIL=$((FAIL + 1))
  fi

  # The guard itself: a copy with the token replaced by a version fails it.
  local tmp
  tmp="$(mktemp)"
  sed 's#dizzy:DIZZY_VERSION#dizzy:v0.3.0#' "$JOB" >"$tmp"
  if names_no_version "$tmp"; then
    echo "  FAIL: a job.yaml naming ghcr.io/b42labs/dizzy:v0.3.0 passes the version guard"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: a job.yaml naming a dizzy version fails the version guard"
    PASS=$((PASS + 1))
  fi
  rm -f "$tmp"

  assert_eq "the kustomization does not list job.yaml" "" \
    "$(resource_entries "$SOAK_DIR/kustomization.yaml" | grep -x job.yaml || true)"
  assert_eq "nor scenario.yaml" "" \
    "$(resource_entries "$SOAK_DIR/kustomization.yaml" | grep -x scenario.yaml || true)"
}

# --- Test 6: the scenario ---
test_scenario() {
  echo "Test: scenario.yaml is the mix small profile with the lab's names"

  if ! have yq; then
    echo "  SKIP: yq not installed (26 checks skipped)"
    SKIP=$((SKIP + 26))
    return
  fi

  local fixture_image fixture_flavor
  fixture_image="$(yq -N -r 'select(.kind == "Image") | .spec.resource.name' "$FIXTURES_DIR/image.yaml")"
  fixture_flavor="$(yq -N -r 'select(.kind == "Flavor") | .spec.resource.name' "$FIXTURES_DIR/fixtures.yaml")"
  assert_not_empty "the fixtures name an image" "$fixture_image"
  assert_eq "image is the fixtures' image" "$fixture_image" "$(scenario_value '.image')"
  assert_eq "flavor is the fixtures' flavor" "$fixture_flavor" "$(scenario_value '.flavor')"
  assert_eq "name is lab-soak" "lab-soak" "$(scenario_value '.name')"
  assert_eq "resize_flavor is the empty string" "!!str:" \
    "$(scenario_value '.personas.legacy.resize_flavor | tag + ":" + .')"
  assert_eq "cold migration is off" "false" "$(scenario_value '.personas.legacy.cold_migration')"
  assert_eq "every lane is off" "cinder=false glance=false keystone=false neutron=false" \
    "$(scenario_value '.lanes | to_entries | map(.key + "=" + (.value.enabled | tostring)) | join(" ")')"
  assert_eq "chaos.duration is 6h" "6h" "$(scenario_value '.chaos.duration')"
  assert_eq "chaos.parallel.max is 4" "4" "$(scenario_value '.chaos.parallel.max')"
  assert_eq "seed is the profile's 42" "42" "$(scenario_value '.seed')"
  assert_eq "no service is bound" "0" "$(scenario_value '.services | length')"
  assert_eq "resources.servers is 6" "6" "$(scenario_value '.resources.servers')"
  assert_eq "the shares are 0.5, 0.3 and 0.2" "0.5 0.3 0.2" \
    "$(scenario_value '[.personas.ci.share, .personas.gardener.share, .personas.legacy.share] | map(tostring) | join(" ")')"
  assert_eq "every persona uses --os-cloud" "  " \
    "$(scenario_value '[.personas[].cloud] | join(" ")')"
  assert_eq "ci: 2 networks, 0 to 1 volumes per server" "2 0-1" \
    "$(scenario_value '.personas.ci | (.networks | tostring) + " " + (.volumes_per_server.min | tostring) + "-" + (.volumes_per_server.max | tostring)')"
  assert_eq "ci: interval 100ms to 1s" "100ms-1s" \
    "$(scenario_value '.personas.ci.interval | .min + "-" + .max')"
  assert_eq "ci: churn_ratio 0.5, target_fill 0.6" "0.5 0.6" \
    "$(scenario_value '.personas.ci | (.churn_ratio | tostring) + " " + (.target_fill | tostring)')"
  assert_eq "gardener: one cluster, soft-anti-affinity" "1 soft-anti-affinity" \
    "$(scenario_value '.personas.gardener | (.clusters | tostring) + " " + .policy')"
  assert_eq "gardener: interval 10s to 1m" "10s-1m" \
    "$(scenario_value '.personas.gardener.interval | .min + "-" + .max')"
  assert_eq "legacy: 1 network" "1" "$(scenario_value '.personas.legacy.networks')"
  assert_eq "legacy: 1 to 2 volumes per server" "1-2" \
    "$(scenario_value '.personas.legacy.volumes_per_server | (.min | tostring) + "-" + (.max | tostring)')"
  assert_eq "legacy: 0 to 1 extra ports per server" "0-1" \
    "$(scenario_value '.personas.legacy.ports_per_server | (.min | tostring) + "-" + (.max | tostring)')"
  assert_eq "legacy: interval 10s to 1m" "10s-1m" \
    "$(scenario_value '.personas.legacy.interval | .min + "-" + .max')"
  assert_eq "every volume is 1 to 2 GiB" "1-2" \
    "$(scenario_value '[.personas[].volume_gib | (.min | tostring) + "-" + (.max | tostring)] | unique | join(" ")')"
  assert_eq "every lane uses the small profile" "small" \
    "$(scenario_value '[.lanes[].profile] | unique | join(" ")')"
  assert_eq "the scenario names its origin" "1" \
    "$(grep -cF 'https://github.com/B42Labs/dizzy/blob/' "$SCENARIO")"
}

# --- Test 7: dizzy's own plan of the scenario ---
# shellcheck disable=SC2016 # the pin line's literal text, not expanded here
test_mix_generate() {
  echo "Test: the pinned dizzy plans the scenario as 3 CI, 2 Gardener and 1 Legacy server"

  local pinned bin="$PROJECT_ROOT/bin/dizzy"
  pinned="$(sed -n 's/^DIZZY_VERSION="${DIZZY_VERSION:-\(v[0-9.]*\)}"$/\1/p' "$DIZZY_SH")"
  if ! have jq || ! have go || ! go version -m "$bin" 2>/dev/null | grep -qF "$pinned"; then
    echo "  SKIP: jq, go or bin/dizzy $pinned missing (4 checks skipped; GOBIN=\$PWD/bin go install github.com/B42Labs/dizzy/cmd/dizzy@$pinned)"
    SKIP=$((SKIP + 4))
    return
  fi

  local plan rc
  plan="$("$bin" mix generate --scenario "$SCENARIO" 2>/dev/null)"
  rc=$?
  assert_eq "mix generate exits 0" "0" "$rc"
  assert_eq "the personas are ci, gardener and legacy with 3, 2 and 1 servers" "ci=3 gardener=2 legacy=1" \
    "$(jq -r '[.personas[] | .name + "=" + (.servers | tostring)] | join(" ")' <<<"$plan")"
  assert_eq "the plan has no lane" "0" "$(jq -r '.lanes // [] | length' <<<"$plan")"
  assert_eq "the Legacy server live-migrates and neither resizes nor cold-migrates" "true false false" \
    "$(jq -r '.personas[] | select(.name == "legacy") | .nova.servers[0] |
      [(.liveMigrate // false), (.resize // false), (.coldMigrate // false)] | map(tostring) | join(" ")' <<<"$plan")"
}

# --- Run ---
test_files_exist_with_spdx
test_kustomization_resources
test_render
test_cluster_role
test_job
test_scenario
test_mix_generate

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
