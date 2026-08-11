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
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/networkpolicy"
	placementv1alpha1 "github.com/c5c3/forge/operators/placement/api/v1alpha1"
)

// Condition type and reason constants for NetworkPolicy readiness. The reason
// vocabulary is shared across operators via the networkpolicy package.
const (
	conditionTypeNetworkPolicyReady         = "NetworkPolicyReady"
	conditionReasonNetworkPolicyReady       = networkpolicy.ReasonNetworkPolicyReady
	conditionReasonNetworkPolicyNotRequired = networkpolicy.ReasonNetworkPolicyNotRequired
)

// reconcileNetworkPolicy ensures the NetworkPolicy for the Placement API
// deployment matches the desired state, via the shared network-policy flow. It
// keeps only the service-specific part: the desired policy builder with its
// database and cache egress.
func (r *PlacementReconciler) reconcileNetworkPolicy(ctx context.Context, children client.Client, placement *placementv1alpha1.Placement) (ctrl.Result, error) {
	// buildPlacementNetworkPolicy is only applied on the enabled+non-empty path;
	// build it lazily so a nil or empty-ingress spec takes the delete or
	// fail-closed path without a wasted build.
	var desired *networkingv1.NetworkPolicy
	ingressCount := 0
	if placement.Spec.NetworkPolicy != nil {
		ingressCount = len(placement.Spec.NetworkPolicy.Ingress)
		if ingressCount > 0 {
			desired = buildPlacementNetworkPolicy(placement, r.OperatorNamespace)
		}
	}
	return networkpolicy.Reconcile(ctx, children, r.Scheme, placement, networkpolicy.FlowParams{
		Configured:         placement.Spec.NetworkPolicy != nil,
		IngressSourceCount: ingressCount,
		Desired:            desired,
		Name:               subResourceName(placement),
		Namespace:          placement.Namespace,
		Conditions:         &placement.Status.Conditions,
		Generation:         placement.Generation,
		ConditionType:      conditionTypeNetworkPolicyReady,
	})
}

// buildPlacementNetworkPolicy constructs the desired NetworkPolicy for the
// Placement API pods. It restricts ingress to the API port from the specified
// sources and auto-derives egress rules for DNS (UDP+TCP 53), the database, and
// the cache. AdditionalEgress rules are appended after the auto-derived rules.
//
// operatorNamespace is the Namespace the operator Pod runs in. When non-empty,
// an ingress peer selecting that Namespace is appended so the operator's own
// health check can reach the Placement API. When empty (namespace unknown) no
// such peer is added.
func buildPlacementNetworkPolicy(placement *placementv1alpha1.Placement, operatorNamespace string) *networkingv1.NetworkPolicy {
	npSpec := placement.Spec.NetworkPolicy

	// When spec.gateway is set, an ingress peer selects the whole Gateway
	// namespace so the Gateway data plane can reach the API Service. An empty
	// parentRef.namespace defaults to the CR's own namespace.
	gatewayNamespace := ""
	if placement.Spec.Gateway != nil {
		gatewayNamespace = placement.Spec.Gateway.ParentRef.Namespace
		if gatewayNamespace == "" {
			gatewayNamespace = placement.Namespace
		}
	}
	peers := networkpolicy.IngressPeers(networkpolicy.IngressPeersParams{
		Sources:           npSpec.Ingress,
		GatewayNamespace:  gatewayNamespace,
		OperatorNamespace: operatorNamespace,
	})

	apiPort := intstr.FromInt32(placementAPIPort)
	tcp := corev1.ProtocolTCP
	ingressRules := []networkingv1.NetworkPolicyIngressRule{
		{
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &tcp, Port: &apiPort},
			},
			From: peers,
		},
	}

	// Auto-derive egress rules, then append user-specified additional rules.
	egressRules := buildAutoEgressRules(placement)
	egressRules = append(egressRules, npSpec.AdditionalEgress...)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      subResourceName(placement),
			Namespace: placement.Namespace,
			Labels:    commonLabels(placement),
		},
		Spec: networkingv1.NetworkPolicySpec{
			// The selector carries no component key, so it covers the API pods and
			// the db-sync pods alike. Without the DNS and database egress,
			// placement-manage cannot reach the database and every migration fails.
			PodSelector: metav1.LabelSelector{
				MatchLabels: selectorLabels(placement),
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
// (UDP+TCP 53), the database (TCP, port from the database spec), the Keystone
// endpoint (TCP, port from networkpolicy.KeystoneEndpointPort), and the cache
// (TCP, ports from
// the cache spec). Rule order is deterministic: DNS, database, keystone, cache.
// The database and keystone rules are always emitted (Placement always has a
// database and always validates tokens); the cache rule is emitted only when the
// cache spec yields ports. Placement reads and writes nothing but its own
// database, so it needs no object-store egress.
func buildAutoEgressRules(placement *placementv1alpha1.Placement) []networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP

	rules := []networkingv1.NetworkPolicyEgressRule{
		// DNS egress: always required (UDP+TCP 53).
		networkpolicy.DNSEgressRule(),
		// Database egress: Placement connects to MariaDB in both managed and
		// brownfield modes; the port matches the readiness posture.
		networkpolicy.DatabaseEgressRule(placement.Spec.Database),
	}

	// Keystone egress: keystonemiddleware validates the token of every
	// authenticated request against spec.keystoneEndpoint server-side, so without
	// this rule the API answers 503 on all of them while both readiness signals —
	// the kubelet probes and the operator health check — keep passing, because
	// both target the unauthenticated version document. Port-only — destination
	// unrestricted, matching the DB/cache egress posture.
	keystonePort := intstr.FromInt32(networkpolicy.KeystoneEndpointPort(placement.Spec.KeystoneEndpoint))
	rules = append(rules, networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &tcp, Port: &keystonePort},
		},
	})

	// Cache egress: emitted in both managed and brownfield modes. Cache egress
	// does not gate readiness, so a wrong cache port degrades caching without
	// depooling pods.
	if rule, ok := networkpolicy.CacheEgressRule(placement.Spec.Cache); ok {
		rules = append(rules, rule)
	}

	return rules
}
