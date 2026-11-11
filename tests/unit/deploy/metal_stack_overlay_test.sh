#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the metal-stack lab overlay that hack/deploy-infra.sh applies under
# EXTERNAL_CLUSTER=true:
#   1. deploy/lab/metal-stack/{base,infrastructure}/kustomization.yaml exist
#      with SPDX headers. The base lists exactly one resource, the kind base
#      (../../../kind/base); the infrastructure lists the kind infrastructure
#      overlay (../../../kind/infrastructure), envoy-public.yaml and
#      lab-ca.yaml, and not the template gateway-certificate.yaml.
#   2. The base render carries the OpenBao HelmRelease in standalone mode
#      with no storage class, the envoy-gateway HelmRelease, no kind
#      EnvoyProxy envoy-nodeport (the kind base carries it, the lab base
#      drops it), the nine suspended service-operator releases, no
#      metrics-server or vertical-pod-autoscaler release (the platform runs
#      both), and the Gardener apiserver-proxy opt-out label and the
#      chaos-mesh.org/inject: enabled annotation on every rendered Namespace,
#      none of which is a platform namespace, flux-system or chaos-mesh. The
#      GatewayClass envoy names the EnvoyProxy envoy-public, and openstack-gw
#      has one HTTPS listener on 443 for *.nip.io that terminates with the
#      Secret openstack-gw-tls and admits routes from openstack and dizzy
#      alone.
#   3. The infrastructure render carries MariaDB and both Garage volumes at
#      one replica with no storage class, the paused proving OpenBaoCluster
#      without egress fields (hack/deploy-infra.sh patches those in) and the
#      openstack OpenBaoTenant. Of the Gateway's pieces it carries the
#      EnvoyProxy envoy-public of type LoadBalancer with
#      externalTrafficPolicy Cluster, no source ranges and no node port, the
#      lab CA (a CA Certificate in cert-manager from the self-signed issuer,
#      constrained to nip.io alone, critically) and the ClusterIssuer
#      lab-ca-issuer on its Secret, and neither the kind EnvoyProxy
#      envoy-nodeport nor any *-nip-io-tls Certificate. The template
#      gateway-certificate.yaml is the Certificate openstack-gw-tls in the
#      Gateway's namespace, issued by lab-ca-issuer into the Secret the
#      listener names, for "*.PUBLIC_SUFFIX" as its common name and its one
#      DNS name, with the usage server auth.
#   4. Neither render names local-path, ceph-rbd or any storage class: no
#      line sets storageClass or storageClassName, so every volume binds to
#      the cluster's default class.
#   5. The kind infrastructure overlay still renders MariaDB on `standard`,
#      and the kind base annotates no Namespace for Chaos Mesh, so the lab
#      overlay changed nothing under deploy/kind/.
#
# Checks 2 to 5 are counted as SKIP when kustomize or yq is not on PATH; a
# failing kustomize build counts them as FAIL and prints the build's error. An
# empty render fails the object checks instead of passing on zero documents.
#
# Usage: bash tests/unit/deploy/metal_stack_overlay_test.sh

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

LAB_DIR="$PROJECT_ROOT/deploy/lab/metal-stack"
BASE_DIR="$LAB_DIR/base"
INFRA_DIR="$LAB_DIR/infrastructure"
KIND_INFRA_DIR="$PROJECT_ROOT/deploy/kind/infrastructure"
KIND_BASE_DIR="$PROJECT_ROOT/deploy/kind/base"

SERVICE_OPERATORS="keystone horizon glance placement barbican ovn neutron cinder nova"

RENDERED=""

# joined <kind> <name> <expression>
# Like val, for an <expression> that builds a list: yq prints an empty line
# for every other document of the render then, which val's first line would
# be.
joined() {
  printf '%s\n' "$RENDERED" |
    yq -N -r "select(.kind == \"$1\" and .metadata.name == \"$2\") | $3" - | grep -v '^$' | head -n 1
}

# --- Test 1: both kustomizations exist, carry SPDX and name the kind overlay ---
test_files_have_spdx_and_one_resource() {
  echo "Test: the lab kustomizations carry SPDX headers and take the kind overlay as their one resource"

  local dir expected f
  for dir in base infrastructure; do
    f="$LAB_DIR/$dir/kustomization.yaml"
    if [[ ! -f "$f" ]]; then
      echo "  FAIL: $f does not exist"
      FAIL=$((FAIL + 3))
      continue
    fi
    assert_file_contains "$dir/kustomization.yaml has SPDX FileCopyrightText header" \
      "$f" "SPDX-FileCopyrightText: Copyright 2026 SAP SE"
    assert_file_contains "$dir/kustomization.yaml has SPDX-License-Identifier: Apache-2.0" \
      "$f" "SPDX-License-Identifier: Apache-2.0"
    expected="../../../kind/$dir"
    if [[ "$dir" == infrastructure ]]; then
      expected="$(printf '%s\n' "$expected" envoy-public.yaml lab-ca.yaml)"
    fi
    assert_eq "$dir/kustomization.yaml lists $(tr '\n' ' ' <<<"$expected")" \
      "$expected" "$(resource_entries "$f")"
  done

  for f in envoy-public.yaml lab-ca.yaml gateway-certificate.yaml; do
    assert_file_contains "infrastructure/$f has SPDX-License-Identifier: Apache-2.0" \
      "$INFRA_DIR/$f" "SPDX-License-Identifier: Apache-2.0"
  done
}

# --- Test 2: the base render ---
test_base_render() {
  echo "Test: kustomize build deploy/lab/metal-stack/base"

  render "$BASE_DIR" 23 || return

  assert_eq "HelmRelease openbao names no storage class" "false" \
    "$(val HelmRelease openbao '.spec.values.server.dataStorage | has("storageClass")')"
  assert_eq "HelmRelease openbao keeps HA disabled (inherited)" "false" \
    "$(val HelmRelease openbao '.spec.values.server.ha.enabled')"
  assert_eq "HelmRelease openbao keeps standalone mode (inherited)" "true" \
    "$(val HelmRelease openbao '.spec.values.server.standalone.enabled')"
  assert_eq "HelmRelease envoy-gateway is rendered" "1" \
    "$(count_named HelmRelease envoy-gateway)"
  assert_eq "GatewayClass envoy names the EnvoyProxy envoy-public" \
    "EnvoyProxy envoy-gateway-system envoy-public" \
    "$(joined GatewayClass envoy '.spec.parametersRef | [.kind, .namespace, .name] | join(" ")')"
  assert_eq "Gateway openstack-gw carries one listener" "1" \
    "$(val Gateway openstack-gw '.spec.listeners | length')"
  assert_eq "the listener is HTTPS on 443 for *.nip.io" "https HTTPS 443 *.nip.io" \
    "$(joined Gateway openstack-gw '.spec.listeners[0] | [.name, .protocol, (.port | tostring), .hostname] | join(" ")')"
  assert_eq "the listener terminates with the Secret openstack-gw-tls alone" "Terminate Secret openstack-gw-tls 1" \
    "$(joined Gateway openstack-gw '.spec.listeners[0].tls | [.mode, .certificateRefs[0].kind, .certificateRefs[0].name, (.certificateRefs | length | tostring)] | join(" ")')"
  assert_eq "the listener admits routes from openstack and dizzy alone" "Selector In openstack,dizzy" \
    "$(joined Gateway openstack-gw '.spec.listeners[0].allowedRoutes.namespaces | [.from, .selector.matchExpressions[0].operator, (.selector.matchExpressions[0].values | join(","))] | join(" ")')"
  assert_eq "the selector matches on the namespace name alone" "kubernetes.io/metadata.name 1" \
    "$(joined Gateway openstack-gw '.spec.listeners[0].allowedRoutes.namespaces.selector | [.matchExpressions[0].key, (.matchExpressions | length | tostring)] | join(" ")')"
  assert_eq "the kind EnvoyProxy envoy-nodeport is not rendered" "0" \
    "$(count_named EnvoyProxy envoy-nodeport)"

  local svc
  for svc in $SERVICE_OPERATORS; do
    assert_eq "HelmRelease ${svc}-operator stays suspended" "true" \
      "$(val HelmRelease "${svc}-operator" '.spec.suspend')"
  done

  assert_eq "no metrics-server HelmRelease is rendered" "0" \
    "$(count_named HelmRelease metrics-server)"
  assert_eq "no vertical-pod-autoscaler HelmRelease is rendered" "0" \
    "$(count_named HelmRelease vertical-pod-autoscaler)"

  # The stack's NetworkPolicies expect the API server by its in-cluster
  # address; Gardener's webhook sets a DNS name in every pod that does not
  # opt out, and the label is that opt-out. Every namespace, not a list.
  local namespaces labelled
  namespaces="$(printf '%s\n' "$RENDERED" |
    yq -N -r 'select(.kind == "Namespace") | .metadata.name' - | grep -c .)"
  labelled="$(printf '%s\n' "$RENDERED" |
    yq -N -r 'select(.kind == "Namespace" and .metadata.labels["apiserver-proxy.networking.gardener.cloud/inject"] == "disable") | .metadata.name' - |
    grep -c .)"
  assert_eq "the base renders namespaces" "true" "$([[ "$namespaces" -gt 0 ]] && echo true || echo false)"
  assert_eq "every rendered Namespace opts out of Gardener's KUBERNETES_SERVICE_HOST injection ($namespaces)" \
    "$namespaces" "$labelled"
  # The scope of the lab's Chaos Mesh, whose release filters by namespace
  # (deploy/lab/metal-stack/chaos-mesh): every namespace the lab declares.
  local annotated
  annotated="$(printf '%s\n' "$RENDERED" |
    yq -N -r 'select(.kind == "Namespace" and .metadata.annotations["chaos-mesh.org/inject"] == "enabled") | .metadata.name' - |
    grep -c .)"
  assert_eq "every rendered Namespace is selectable for Chaos Mesh ($namespaces)" \
    "$namespaces" "$annotated"
  # The scope stops at the lab's own namespaces: the platform's, flux-system
  # and chaos-mesh must never be rendered here, or the patch annotates them.
  assert_eq "no platform, Flux or Chaos Mesh namespace is in the Chaos Mesh scope" "" \
    "$(printf '%s\n' "$RENDERED" |
      yq -N -r 'select(.kind == "Namespace" and .metadata.annotations["chaos-mesh.org/inject"] == "enabled") | .metadata.name' - |
      grep -xE 'kube-system|kube-public|kube-node-lease|default|firewall|metallb-system|flux-system|chaos-mesh' || true)"
  assert_no_class "base"
}

# --- Test 3: the infrastructure render ---
test_infrastructure_render() {
  echo "Test: kustomize build deploy/lab/metal-stack/infrastructure"

  render "$INFRA_DIR" 15 || return

  assert_eq "MariaDB openstack-db names no storage class" "false" \
    "$(val MariaDB openstack-db '.spec.storage | has("storageClassName")')"
  assert_eq "MariaDB openstack-db keeps one replica (inherited)" "1" \
    "$(val MariaDB openstack-db '.spec.replicas')"
  assert_eq "MariaDB openstack-db keeps Galera disabled (inherited)" "false" \
    "$(val MariaDB openstack-db '.spec.galera.enabled')"
  assert_eq "GarageCluster garage names no storage class for its metadata" "false" \
    "$(val GarageCluster garage '.spec.storage.metadata | has("storageClassName")')"
  assert_eq "GarageCluster garage names no storage class for its data" "false" \
    "$(val GarageCluster garage '.spec.storage.data | has("storageClassName")')"
  assert_eq "GarageCluster garage keeps one storage node (inherited)" "1" \
    "$(val GarageCluster garage '.spec.storage.replicas')"
  assert_eq "the kind EnvoyProxy envoy-nodeport is not rendered" "0" \
    "$(count_named EnvoyProxy envoy-nodeport)"
  assert_eq "EnvoyProxy envoy-public is rendered in envoy-gateway-system" "envoy-gateway-system" \
    "$(val EnvoyProxy envoy-public '.metadata.namespace')"
  assert_eq "EnvoyProxy envoy-public asks for a LoadBalancer Service" "LoadBalancer" \
    "$(val EnvoyProxy envoy-public '.spec.provider.kubernetes.envoyService.type')"
  assert_eq "EnvoyProxy envoy-public keeps externalTrafficPolicy Cluster" "Cluster" \
    "$(val EnvoyProxy envoy-public '.spec.provider.kubernetes.envoyService.externalTrafficPolicy')"
  # hack/deploy-infra.sh patches the ranges in from PUBLIC_SOURCE_RANGES and
  # the shoot's networks; a value here would be wrong on the next shoot.
  assert_eq "EnvoyProxy envoy-public hardcodes no source ranges" "false" \
    "$(val EnvoyProxy envoy-public '.spec.provider.kubernetes.envoyService | has("loadBalancerSourceRanges")')"
  assert_eq "EnvoyProxy envoy-public pins no node port" "false" \
    "$(val EnvoyProxy envoy-public '.spec.provider.kubernetes.envoyService | has("patch")')"
  assert_eq "no *-nip-io-tls Certificate is rendered" "" \
    "$(printf '%s\n' "$RENDERED" |
      yq -N -r 'select(.kind == "Certificate") | .metadata.name' - | grep -- '-nip-io-tls$' || true)"
  assert_eq "Certificate lab-ca is a CA in cert-manager" "true cert-manager" \
    "$(joined Certificate lab-ca '[(.spec.isCA | tostring), .metadata.namespace] | join(" ")')"
  assert_eq "Certificate lab-ca is signed by selfsigned-cluster-issuer" "ClusterIssuer selfsigned-cluster-issuer" \
    "$(joined Certificate lab-ca '.spec.issuerRef | [.kind, .name] | join(" ")')"
  assert_eq "the lab CA is constrained to nip.io alone, critically" "true nip.io" \
    "$(joined Certificate lab-ca '.spec.nameConstraints | [(.critical | tostring), (.permitted.dnsDomains | join(","))] | join(" ")')"
  assert_eq "the lab CA permits no other kind of name and excludes none" "dnsDomains" \
    "$(joined Certificate lab-ca '.spec.nameConstraints | [(.permitted | keys | .[]), (.excluded // {} | keys | .[])] | join(",")')"
  assert_eq "ClusterIssuer lab-ca-issuer signs with the lab CA's Secret" \
    "$(val Certificate lab-ca '.spec.secretName')" \
    "$(val ClusterIssuer lab-ca-issuer '.spec.ca.secretName')"
  assert_eq "OpenBaoCluster openbao-instance starts paused" "true" \
    "$(val OpenBaoCluster openbao-instance '.spec.paused')"
  # hack/deploy-infra.sh patches both in from the live cluster together with
  # the un-pause; a hardcoded value would be wrong on the next cluster.
  assert_eq "OpenBaoCluster openbao-instance hardcodes no apiServerEndpointIPs" "null" \
    "$(val OpenBaoCluster openbao-instance '.spec.network.apiServerEndpointIPs')"
  assert_eq "OpenBaoCluster openbao-instance hardcodes no egressRules" "null" \
    "$(val OpenBaoCluster openbao-instance '.spec.network.egressRules')"
  assert_eq "OpenBaoTenant openstack is rendered" "1" \
    "$(count_named OpenBaoTenant openstack)"
  assert_no_class "infrastructure"
}

# --- Test 3a: the template of the Gateway's certificate ---
test_gateway_certificate_template() {
  echo "Test: infrastructure/gateway-certificate.yaml, the template hack/deploy-infra.sh fills"

  local f="$INFRA_DIR/gateway-certificate.yaml" listener_secret
  if ! have yq; then
    echo "  SKIP: yq not installed (7 checks skipped)"
    SKIP=$((SKIP + 7))
    return
  fi
  if ! render "$BASE_DIR" 7; then
    return
  fi
  listener_secret="$(val Gateway openstack-gw '.spec.listeners[0].tls.certificateRefs[0].name')"

  assert_eq "the template is one Certificate openstack-gw-tls" "Certificate openstack-gw-tls 1" \
    "$(yq -N -r '[.kind, .metadata.name] | join(" ")' "$f") $(yq -N -r '.kind' "$f" | grep -c .)"
  assert_eq "it lives in the Gateway's namespace" "openstack" "$(yq -r '.metadata.namespace' "$f")"
  assert_eq "it writes the Secret the listener terminates with" "$listener_secret" \
    "$(yq -r '.spec.secretName' "$f")"
  assert_eq "the lab CA issues it" "ClusterIssuer lab-ca-issuer" \
    "$(yq -r '.spec.issuerRef | [.kind, .name] | join(" ")' "$f")"
  assert_eq "its one DNS name is the placeholder wildcard" "*.PUBLIC_SUFFIX" \
    "$(yq -r '.spec.dnsNames | join(",")' "$f")"
  assert_eq "its common name is the same wildcard" "*.PUBLIC_SUFFIX" \
    "$(yq -r '.spec.commonName' "$f")"
  assert_eq "it carries the usage server auth" "1" \
    "$(yq -r '.spec.usages[]' "$f" | grep -cx 'server auth')"
}

# assert_no_class <label>
# Three checks: the render in RENDERED names none of the production classes,
# and no line of it sets a storage class at all. A patch value that kustomize
# kept as `storageClass: null` fails the last check too.
assert_no_class() {
  local needle
  for needle in "local-path" "ceph-rbd"; do
    assert_not_contains "the $1 render names no '$needle'" "$RENDERED" "$needle"
  done
  assert_eq "no line of the $1 render sets storageClass or storageClassName" "" \
    "$(printf '%s\n' "$RENDERED" | grep -E '^[[:space:]]*storageClass(Name)?:')"
}

# --- Test 4: the kind overlay is unchanged ---
test_kind_overlay_unchanged() {
  echo "Test: kustomize build deploy/kind/infrastructure still renders MariaDB on standard"

  if render "$KIND_INFRA_DIR" 1; then
    assert_eq "the kind MariaDB stays on the standard class" "standard" \
      "$(val MariaDB openstack-db '.spec.storage.storageClassName')"
  fi

  # kind keeps the Chaos Mesh namespace filter off, so it needs no annotation.
  render "$KIND_BASE_DIR" 1 || return
  assert_eq "no Namespace of the kind base carries chaos-mesh.org/inject" "0" \
    "$(grep -c 'chaos-mesh.org/inject' <<<"$RENDERED")"
}

# --- Run ---
test_files_have_spdx_and_one_resource
test_base_render
test_infrastructure_render
test_gateway_certificate_template
test_kind_overlay_unchanged

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
