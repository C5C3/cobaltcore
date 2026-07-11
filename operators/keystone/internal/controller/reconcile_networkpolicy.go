// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/c5c3/forge/internal/common/networkpolicy"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Condition type and reason constants for NetworkPolicy readiness. The reason
// vocabulary is shared across operators via the networkpolicy package.
const (
	conditionTypeNetworkPolicyReady         = "NetworkPolicyReady"
	conditionReasonNetworkPolicyReady       = networkpolicy.ReasonNetworkPolicyReady
	conditionReasonNetworkPolicyNotRequired = networkpolicy.ReasonNetworkPolicyNotRequired
)

// reconcileNetworkPolicy ensures the NetworkPolicy for the Keystone API
// deployment matches the desired state, via the shared network-policy flow. It
// keeps only the service-specific parts: the desired policy builder (with its
// federation- and apiserver-aware egress tail) and the backend identity.
func (r *KeystoneReconciler) reconcileNetworkPolicy(ctx context.Context, keystone *keystonev1alpha1.Keystone, fed *federationProjection) (ctrl.Result, error) {
	// buildKeystoneNetworkPolicy is only applied on the enabled+non-empty path;
	// build it lazily so a nil or empty-ingress spec takes the delete or
	// fail-closed path without a wasted build.
	var desired *networkingv1.NetworkPolicy
	ingressCount := 0
	if keystone.Spec.NetworkPolicy != nil {
		ingressCount = len(keystone.Spec.NetworkPolicy.Ingress)
		if ingressCount > 0 {
			desired = buildKeystoneNetworkPolicy(keystone, r.OperatorNamespace, fed)
		}
	}
	return networkpolicy.Reconcile(ctx, r.Client, r.Scheme, keystone, networkpolicy.FlowParams{
		Configured:         keystone.Spec.NetworkPolicy != nil,
		IngressSourceCount: ingressCount,
		Desired:            desired,
		Name:               subResourceName(keystone),
		Namespace:          keystone.Namespace,
		Conditions:         &keystone.Status.Conditions,
		Generation:         keystone.Generation,
		ConditionType:      conditionTypeNetworkPolicyReady,
	})
}

// buildKeystoneNetworkPolicy constructs the desired NetworkPolicy for the
// Keystone API pods. It restricts ingress to the API port from the specified
// sources and auto-derives egress rules for DNS (UDP+TCP 53), the
// kube-apiserver (TCP 443+6443, required by the rotation CronJob pods that
// share this selector), the database (TCP, port from the database spec, both
// managed and brownfield modes), and the cache (TCP, ports derived from the
// cache spec, both managed and brownfield modes). AdditionalEgress rules are
// appended after auto-derived rules.
//
// operatorNamespace is the Namespace the operator Pod runs in. When non-empty,
// an ingress peer selecting that Namespace is appended so the operator's own
// health check (reconcileHealthCheck GETs the Keystone Service) is not blocked
// by the policy (issue #461). When empty (namespace unknown) no such peer is
// added.
//
// fed is the federation projection: when set, the ingress target port is the
// sidecar's (the Service targetPort switched there) and per-issuer egress ports
// are appended so the sidecar can reach the identity provider.
func buildKeystoneNetworkPolicy(keystone *keystonev1alpha1.Keystone, operatorNamespace string, fed *federationProjection) *networkingv1.NetworkPolicy {
	npSpec := keystone.Spec.NetworkPolicy

	// When spec.gateway is set, the Gateway data plane's pods (labels
	// implementation-specific and unknown here) must reach the Service, so an
	// ingress peer selects the whole Gateway namespace. An empty
	// parentRef.namespace defaults to the CR's own namespace, matching the
	// ParentRef lookup semantics.
	gatewayNamespace := ""
	if keystone.Spec.Gateway != nil {
		gatewayNamespace = keystone.Spec.Gateway.ParentRef.Namespace
		if gatewayNamespace == "" {
			gatewayNamespace = keystone.Namespace
		}
	}
	peers := networkpolicy.IngressPeers(networkpolicy.IngressPeersParams{
		Sources:           npSpec.Ingress,
		GatewayNamespace:  gatewayNamespace,
		OperatorNamespace: operatorNamespace,
	})

	// The ingress target is the port the Service targetPort points at: the
	// sidecar's when federation is active, uWSGI's 5000 otherwise (NetworkPolicy
	// ports are evaluated against the pod port, post-DNAT).
	ingressPort := intstr.FromInt32(5000)
	if fed != nil {
		ingressPort = intstr.FromInt32(federationProxyPort)
	}
	tcp := corev1.ProtocolTCP
	ingressRules := []networkingv1.NetworkPolicyIngressRule{
		{
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &tcp, Port: &ingressPort},
			},
			From: peers,
		},
	}

	// Auto-derive egress rules.
	egressRules := buildAutoEgressRules(keystone)

	// Federation egress: the sidecar's mod_auth_openidc talks to the identity
	// provider (token/jwks/userinfo/introspection endpoints) from inside the
	// pod. Port-only like the database/cache rules.
	if fed != nil && len(fed.EgressPorts) > 0 {
		ports := make([]networkingv1.NetworkPolicyPort, 0, len(fed.EgressPorts))
		for _, p := range fed.EgressPorts {
			fedPort := intstr.FromInt32(p)
			ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &fedPort})
		}
		egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{Ports: ports})
	}

	// Append user-specified additional egress rules.
	egressRules = append(egressRules, npSpec.AdditionalEgress...)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      subResourceName(keystone),
			Namespace: keystone.Namespace,
			Labels:    commonLabels(keystone),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: selectorLabels(keystone),
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: ingressRules,
			Egress:  egressRules,
		},
	}
}

// buildAutoEgressRules constructs the auto-derived egress rules for DNS
// (UDP+TCP 53), the kube-apiserver (TCP 443+6443), the database, and the cache.
// Rule order is deterministic: DNS, apiserver, database, cache.
func buildAutoEgressRules(keystone *keystonev1alpha1.Keystone) []networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP

	// DNS egress: always required (UDP+TCP 53).
	rules := []networkingv1.NetworkPolicyEgressRule{networkpolicy.DNSEgressRule()}

	// kube-apiserver egress: always required (TCP 443+6443). The
	// fernet/credential/admin-password rotation CronJob pods carry commonLabels
	// — a superset of the Deployment selectorLabels — so they are selected by
	// this NetworkPolicy's podSelector and share its egress rules. Those scripts
	// PATCH the rotated keys back to a Kubernetes Secret via
	// kubernetes.default.svc, so the policy must allow egress to the apiserver or
	// every scheduled rotation stops at its first run (issue #461). The ClusterIP
	// Service port is 443; on enforcing CNIs egress is evaluated after DNAT
	// against the kube-apiserver pod port, commonly 6443 — both are allowed.
	port443 := intstr.FromInt32(443)
	port6443 := intstr.FromInt32(6443)
	rules = append(rules, networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &tcp, Port: &port443},
			{Protocol: &tcp, Port: &port6443},
		},
	})

	// Database egress: emitted in both managed and brownfield modes. Without
	// this rule the readiness probe fails on an enforcing CNI and every pod is
	// depooled (issue #461).
	rules = append(rules, networkpolicy.DatabaseEgressRule(keystone.Spec.Database))

	// Cache egress: emitted in both managed and brownfield modes. Cache egress
	// does not gate readiness, so a wrong cache port degrades caching without
	// depooling pods.
	if rule, ok := networkpolicy.CacheEgressRule(keystone.Spec.Cache); ok {
		rules = append(rules, rule)
	}

	return rules
}

// cacheEgressPorts returns the distinct Memcached ports the Keystone API pods
// must reach, derived from the cache spec, via the shared helper.
func cacheEgressPorts(keystone *keystonev1alpha1.Keystone) []int32 {
	return networkpolicy.CacheEgressPorts(keystone.Spec.Cache)
}
