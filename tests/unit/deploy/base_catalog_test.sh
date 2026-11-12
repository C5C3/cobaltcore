#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Exercise the public deployment stages as a consumer would: render under the
# default load restrictions, check ownership and references across stages, and
# require all reusable assets to live in bases. Use real dashboard data in a temporary copy;
# this test neither contacts a cluster nor changes a developer's staged assets.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
PASS=0
FAIL=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

for tool in kustomize yq jq; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "SKIP: $tool is required for deployment catalog checks"
    exit 0
  fi
done

TEST_ROOT="$(mktemp -d)"
trap 'rm -rf "$TEST_ROOT"' EXIT
cp -R "$PROJECT_ROOT/deploy" "$TEST_ROOT/deploy"
cp "$PROJECT_ROOT/operators/keystone/dashboards/keystone-operator.json" \
  "$TEST_ROOT/deploy/bases/prometheus/keystone-operator.json"

# Canonicalize complete objects, retaining list order and every field value.
# Resource ordering and YAML key ordering are not deployment behavior.
canonicalize() {
  yq -o=json -I0 '.' - |
    jq -S -s 'map(select(. != null)) |
      sort_by(.apiVersion, .kind, .metadata.namespace // "", .metadata.name)'
}

render() {
  local path="$1" output="$2"
  if ! kustomize build "$TEST_ROOT/$path" > "$output.yaml" 2> "$output.stderr"; then
    echo "FAIL: kustomize build $path"
    cat "$output.stderr" >&2
    exit 1
  fi
  canonicalize < "$output.yaml" > "$output.json"
}

assert_json() {
  assert_eq "$1" "true" "$(jq -r "$3" "$2")"
}

# Kubernetes resource identity is group/kind/namespace/name. Different served
# versions of the same resource must not hide an ownership collision.
UNIQUE_IDENTITIES='def identity:
  [(.apiVersion | split("/") | if length == 1 then "" else .[0] end),
   .kind, .metadata.namespace // "", .metadata.name];
  length == (unique_by(identity) | length)'

echo "Test: public stage inventory"
for public in operators tls shared-openbao eso-store controlplane-operator; do
  render "deploy/bases/$public" "$TEST_ROOT/public-$public"
  assert_json "$public has unique resource identities" \
    "$TEST_ROOT/public-$public.json" "$UNIQUE_IDENTITIES"
done
assert_eq "deployment root contains only bases, overlays and examples" \
  "bases examples overlays" "$(find "$PROJECT_ROOT/deploy" -mindepth 1 -maxdepth 1 -type d -exec basename {} \; | sort | paste -sd ' ' -)"

OPERATORS="$TEST_ROOT/public-operators.json"
assert_json "operators contain only namespaces, sources and operator reconciliation objects" \
  "$OPERATORS" 'all(.[];
    .kind == "Namespace" or .kind == "HelmRepository" or .kind == "GitRepository" or
    .kind == "OCIRepository" or .kind == "HelmRelease" or
    (.kind == "Kustomization" and .apiVersion == "kustomize.toolkit.fluxcd.io/v1"))'
assert_json "operators exclude Flux bootstrap and both delayed server/operator releases" \
  "$OPERATORS" 'all(.[]; .kind != "FluxInstance" and
    (.kind != "HelmRelease" or (.metadata.name != "openbao" and .metadata.name != "c5c3-operator")))'
assert_json "operators protect every owned namespace from Flux pruning" "$OPERATORS" \
  '[.[] | select(.kind == "Namespace")] |
    length > 0 and all(.[]; .metadata.annotations["kustomize.toolkit.fluxcd.io/prune"] == "disabled")'
assert_eq "operators retain the three Git-sourced installer controllers" \
  "k-orc,messaging-topology-operator,rabbitmq-cluster-operator" \
  "$(jq -r '[.[] | select(.kind == "Kustomization") | .metadata.name] | sort | join(",")' "$OPERATORS")"
assert_json "operators include cert-manager, ESO and the separate OpenBao instance operator" \
  "$OPERATORS" '[.[] | select(.kind == "HelmRelease") | .metadata.name] as $names |
    ["cert-manager", "external-secrets", "openbao-operator"] | all(.[]; . as $name | $names | index($name) != null)'

assert_eq "TLS stage owns the four issuer identities" \
  "openbao-ca-issuer,openstack-db-ca-issuer,openstack-ovn-ca-issuer,selfsigned-cluster-issuer" \
  "$(jq -r '[.[] | select(.kind == "ClusterIssuer") | .metadata.name] | sort | join(",")' "$TEST_ROOT/public-tls.json")"
assert_json "TLS stage contains certificates and issuers, with no operator or secret backend" \
  "$TEST_ROOT/public-tls.json" 'length > 0 and all(.[]; .kind == "Certificate" or .kind == "ClusterIssuer")'
assert_json "shared OpenBao is one management HelmRelease with environment-selected storage" \
  "$TEST_ROOT/public-shared-openbao.json" 'length == 1 and
    .[0].kind == "HelmRelease" and .[0].metadata.name == "openbao" and
    .[0].metadata.namespace == "shared-services" and
    (.[0].spec.values.server.dataStorage | has("storageClass") | not) and
    .[0].spec.values.server.ha.enabled == true and .[0].spec.values.server.ha.replicas == 3'
assert_json "ESO stage is one shared store, retaining its namespace grant boundary" \
  "$TEST_ROOT/public-eso-store.json" 'length == 1 and
    .[0].kind == "ClusterSecretStore" and .[0].metadata.name == "openbao-cluster-store" and
    (.[0].spec.conditions | map(.namespaces) | add | sort) == ["openstack", "shared-services"]'
assert_json "controlplane-operator stage owns one operator release, not a ControlPlane instance" \
  "$TEST_ROOT/public-controlplane-operator.json" 'length == 1 and
    .[0].kind == "HelmRelease" and .[0].metadata.name == "c5c3-operator" and
    .[0].metadata.namespace == "c5c3-system"'

jq -s 'add' "$TEST_ROOT"/public-*.json > "$TEST_ROOT/stages.json"
assert_json "five independently reconciled stages have no overlapping resource ownership" \
  "$TEST_ROOT/stages.json" "$UNIQUE_IDENTITIES"
assert_json "every stage namespace is supplied, except pre-existing Flux bootstrap namespace" \
  "$TEST_ROOT/stages.json" '. as $all | all(.[];
    .metadata.namespace as $ns | $ns == null or $ns == "flux-system" or
    any($all[]; .kind == "Namespace" and .metadata.name == $ns))'
assert_json "every chart and Git installer source reference resolves across the stages" \
  "$TEST_ROOT/stages.json" '. as $all |
    [.[] as $owner |
      (if $owner.kind == "HelmRelease" then $owner.spec.chart.spec.sourceRef // $owner.spec.chartRef
       elif $owner.kind == "Kustomization" then $owner.spec.sourceRef else empty end) as $ref |
      any($all[]; .kind == $ref.kind and .metadata.name == $ref.name and
        (.metadata.namespace // "") == ($ref.namespace // $owner.metadata.namespace // ""))] |
    length > 0 and all(.[]; . == true)'
assert_json "every HelmRelease dependency resolves to a release in the stage set" \
  "$TEST_ROOT/stages.json" '. as $all |
    [.[] | select(.kind == "HelmRelease") | . as $owner | .spec.dependsOn[]? as $ref |
      any($all[]; .kind == "HelmRelease" and .metadata.name == $ref.name and
        (.metadata.namespace // "") == ($ref.namespace // $owner.metadata.namespace // ""))] |
    length > 0 and all(.[]; . == true)'
assert_json "certificate issuer references resolve inside the TLS stage" \
  "$TEST_ROOT/public-tls.json" '. as $all |
    [.[] | select(.kind == "Certificate") | .spec.issuerRef as $ref |
      any($all[]; .kind == $ref.kind and .metadata.name == $ref.name)] |
    length > 0 and all(.[]; . == true)'

echo "Test: checked-in examples customize public bases under default load restrictions"
render "deploy/examples/base-customization/shared-openbao" "$TEST_ROOT/customized-openbao"
assert_json "the OpenBao example selects its storage class and volume size" \
  "$TEST_ROOT/customized-openbao.json" '.[0].spec.values.server.dataStorage.storageClass == "example-fast-storage" and
    .[0].spec.values.server.dataStorage.size == "20Gi"'
assert_eq "a storage customization preserves every unrelated OpenBao field" \
  "$(jq -S 'map(del(.spec.values.server.dataStorage.storageClass, .spec.values.server.dataStorage.size))' "$TEST_ROOT/public-shared-openbao.json")" \
  "$(jq -S 'map(del(.spec.values.server.dataStorage.storageClass, .spec.values.server.dataStorage.size))' "$TEST_ROOT/customized-openbao.json")"
render "deploy/examples/base-customization/operators" "$TEST_ROOT/customized-operators"
assert_json "the operator example sets Keystone replicas and resource requests" \
  "$TEST_ROOT/customized-operators.json" '[.[] | select(.kind == "HelmRelease" and
    .metadata.name == "keystone-operator" and .metadata.namespace == "keystone-system")] |
    length == 1 and .[0].spec.values.replicas == 2 and
    .[0].spec.values.resources == {requests: {cpu: "100m", memory: "192Mi"}}'
WITHOUT_KEYSTONE_SIZING='map(if .kind == "HelmRelease" and .metadata.name == "keystone-operator" and
  .metadata.namespace == "keystone-system" then del(.spec.values.replicas, .spec.values.resources) else . end)'
assert_eq "the operator example preserves all other operators and Keystone settings" \
  "$(jq -S "$WITHOUT_KEYSTONE_SIZING" "$OPERATORS")" \
  "$(jq -S "$WITHOUT_KEYSTONE_SIZING" "$TEST_ROOT/customized-operators.json")"

echo "Test: bootstrap files remain independently consumable"
canonicalize < "$PROJECT_ROOT/deploy/bases/operators/namespaces.yaml" > "$TEST_ROOT/raw-namespaces.json"
assert_json "raw namespace file remains a nonempty Namespace stream" \
  "$TEST_ROOT/raw-namespaces.json" 'length > 0 and all(.[]; .kind == "Namespace")'
canonicalize < "$PROJECT_ROOT/deploy/bases/flux/fluxinstance.yaml" > "$TEST_ROOT/raw-fluxinstance.json"
assert_json "raw FluxInstance remains the bootstrap CR without automatic Git synchronization" \
  "$TEST_ROOT/raw-fluxinstance.json" 'length == 1 and .[0].kind == "FluxInstance" and
    .[0].metadata.name == "flux" and .[0].metadata.namespace == "flux-system" and
    (.[0].spec | has("sync") | not)'
render "deploy/bases/flux" "$TEST_ROOT/flux-runtime"
assert_eq "the Flux base owns exactly the independently applied runtime CR" \
  "$(cat "$TEST_ROOT/raw-fluxinstance.json")" "$(cat "$TEST_ROOT/flux-runtime.json")"

for raw in cert-manager external-secrets garage-operator mariadb-operator openbao \
  c5c3-charts k-orc openbao-operator prometheus-community rabbitmq-cluster-operator \
  messaging-topology-operator; do
  canonicalize < "$PROJECT_ROOT/deploy/bases/operators/sources/$raw.yaml" > "$TEST_ROOT/raw-source.json"
  assert_json "$raw source remains readable inside its owning base" \
    "$TEST_ROOT/raw-source.json" 'length == 1 and
      (.[0].kind == "HelmRepository" or .[0].kind == "GitRepository" or .[0].kind == "OCIRepository")'
done

echo "Test: environment overlays are independent consumers of shared bases"
# A base may not depend on an environment. An overlay may depend on its own
# environment and shared bases, but not on a sibling environment's choices.
while IFS= read -r recipe; do
  recipe_dir="$(dirname "$recipe")"
  while IFS= read -r resource; do
    [[ -d "$recipe_dir/$resource" ]] || continue
    resolved="$(cd "$recipe_dir/$resource" && pwd -P)"
    case "$recipe" in
      */deploy/bases/*) forbidden="/deploy/overlays/" ;;
      */deploy/overlays/kind/*) forbidden="/deploy/overlays/metal-stack/" ;;
      */deploy/overlays/metal-stack/*) forbidden="/deploy/overlays/kind/" ;;
    esac
    assert_eq "${recipe#"$TEST_ROOT/"} does not import a dependent/sibling environment" \
      "false" "$(case "$resolved/" in *"$forbidden"*) echo true ;; *) echo false ;; esac)"
  done < <(yq -r '.resources[]' "$recipe")
done < <(find "$TEST_ROOT/deploy/bases" "$TEST_ROOT/deploy/overlays" -name kustomization.yaml -type f | sort)

echo "Test: monitoring namespace has one owner in managed deployments"
render "deploy/overlays/kind/prometheus" "$TEST_ROOT/prometheus-standalone"
render "deploy/overlays/kind/prometheus-managed" "$TEST_ROOT/prometheus-managed"
render "deploy/overlays/metal-stack/prometheus" "$TEST_ROOT/prometheus-lab"
render "deploy/overlays/kind/base" "$TEST_ROOT/kind-base"
assert_json "standalone Prometheus still supplies monitoring" \
  "$TEST_ROOT/prometheus-standalone.json" '[.[] | select(.kind == "Namespace" and .metadata.name == "monitoring")] | length == 1'
assert_json "managed Prometheus delegates namespaces to the environment base" \
  "$TEST_ROOT/prometheus-managed.json" 'all(.[]; .kind != "Namespace")'
assert_json "lab Prometheus also delegates namespaces to the environment base" \
  "$TEST_ROOT/prometheus-lab.json" 'all(.[]; .kind != "Namespace")'
assert_eq "managed Prometheus changes only namespace ownership" \
  "$(jq -S 'map(select(.kind != "Namespace"))' "$TEST_ROOT/prometheus-standalone.json")" \
  "$(cat "$TEST_ROOT/prometheus-managed.json")"
jq -s 'add' "$TEST_ROOT/kind-base.json" "$TEST_ROOT/prometheus-managed.json" \
  > "$TEST_ROOT/kind-with-monitoring.json"
assert_json "kind base plus managed Prometheus owns each resource once" \
  "$TEST_ROOT/kind-with-monitoring.json" "$UNIQUE_IDENTITIES"

echo ""
echo "Results: $PASS passed, $FAIL failed, 0 skipped"
[[ "$FAIL" -eq 0 ]]
