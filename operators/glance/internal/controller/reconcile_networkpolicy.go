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
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

// Condition type and reason constants for NetworkPolicy readiness. The reason
// vocabulary is shared across operators via the networkpolicy package.
const (
	conditionTypeNetworkPolicyReady         = "NetworkPolicyReady"
	conditionReasonNetworkPolicyReady       = networkpolicy.ReasonNetworkPolicyReady
	conditionReasonNetworkPolicyNotRequired = networkpolicy.ReasonNetworkPolicyNotRequired
)

// reconcileNetworkPolicy ensures the NetworkPolicy for the Glance API deployment
// matches the desired state, via the shared network-policy flow. It keeps only
// the service-specific parts: the desired policy builder (with its DB/cache/S3
// egress) and the backend identity. It takes the backends projection so the S3
// egress set tracks every attached store's host.
func (r *GlanceReconciler) reconcileNetworkPolicy(ctx context.Context, glance *glancev1alpha1.Glance, projection backendsProjection) (ctrl.Result, error) {
	// buildGlanceNetworkPolicy is only applied on the enabled+non-empty path;
	// build it lazily so a nil or empty-ingress spec takes the delete or
	// fail-closed path without a wasted build.
	var desired *networkingv1.NetworkPolicy
	ingressCount := 0
	if glance.Spec.NetworkPolicy != nil {
		ingressCount = len(glance.Spec.NetworkPolicy.Ingress)
		if ingressCount > 0 {
			desired = buildGlanceNetworkPolicy(glance, r.OperatorNamespace, projection.hosts)
		}
	}
	return networkpolicy.Reconcile(ctx, r.Client, r.Scheme, glance, networkpolicy.FlowParams{
		Configured:         glance.Spec.NetworkPolicy != nil,
		IngressSourceCount: ingressCount,
		Desired:            desired,
		Name:               subResourceName(glance),
		Namespace:          glance.Namespace,
		Conditions:         &glance.Status.Conditions,
		Generation:         glance.Generation,
		ConditionType:      conditionTypeNetworkPolicyReady,
	})
}

// buildGlanceNetworkPolicy constructs the desired NetworkPolicy for the Glance
// API pods. It restricts ingress to the API port from the specified sources and
// auto-derives egress rules for DNS (UDP+TCP 53), the database, the cache, and
// the S3 object-store backends. AdditionalEgress rules are appended after the
// auto-derived rules.
//
// operatorNamespace is the Namespace the operator Pod runs in. When non-empty,
// an ingress peer selecting that Namespace is appended so the operator's own
// health check can reach the Glance API. When empty (namespace unknown) no such
// peer is added. hosts is the S3 host URL set of every attached backend, used to
// derive the object-store egress ports.
func buildGlanceNetworkPolicy(glance *glancev1alpha1.Glance, operatorNamespace string, hosts []string) *networkingv1.NetworkPolicy {
	npSpec := glance.Spec.NetworkPolicy

	// When spec.gateway is set, an ingress peer selects the whole Gateway
	// namespace so the Gateway data plane can reach the API Service. An empty
	// parentRef.namespace defaults to the CR's own namespace.
	gatewayNamespace := ""
	if glance.Spec.Gateway != nil {
		gatewayNamespace = glance.Spec.Gateway.ParentRef.Namespace
		if gatewayNamespace == "" {
			gatewayNamespace = glance.Namespace
		}
	}
	peers := networkpolicy.IngressPeers(networkpolicy.IngressPeersParams{
		Sources:           npSpec.Ingress,
		GatewayNamespace:  gatewayNamespace,
		OperatorNamespace: operatorNamespace,
	})

	apiPort := intstr.FromInt32(glanceAPIPort)
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
	egressRules := buildAutoEgressRules(glance, hosts)
	egressRules = append(egressRules, npSpec.AdditionalEgress...)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      subResourceName(glance),
			Namespace: glance.Namespace,
			Labels:    commonLabels(glance),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: selectorLabels(glance),
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
// (UDP+TCP 53), the database (TCP, port from the database spec), the cache (TCP,
// ports from the cache spec), and the S3 object stores (TCP, one port per
// distinct backend host). Rule order is deterministic: DNS, database, cache, S3.
// The database rule is always emitted (Glance always has a database); the cache
// and S3 rules are emitted only when their inputs yield ports.
func buildAutoEgressRules(glance *glancev1alpha1.Glance, hosts []string) []networkingv1.NetworkPolicyEgressRule {
	rules := []networkingv1.NetworkPolicyEgressRule{
		// DNS egress: always required (UDP+TCP 53).
		networkpolicy.DNSEgressRule(),
		// Database egress: Glance connects to MariaDB in both managed and
		// brownfield modes; the port matches the readiness posture.
		networkpolicy.DatabaseEgressRule(glance.Spec.Database),
	}

	// Cache egress: emitted in both managed and brownfield modes. Cache egress
	// does not gate readiness, so a wrong cache port degrades caching without
	// depooling pods.
	if rule, ok := networkpolicy.CacheEgressRule(glance.Spec.Cache); ok {
		rules = append(rules, rule)
	}

	// S3 egress: the object-store endpoints the attached backends read/write
	// images through. Emitted only when at least one backend host yields a usable
	// port; the destination is unrestricted because the hosts are user-supplied
	// external endpoints.
	if rule, ok := networkpolicy.S3EgressRule(hosts); ok {
		rules = append(rules, rule)
	}

	return rules
}
