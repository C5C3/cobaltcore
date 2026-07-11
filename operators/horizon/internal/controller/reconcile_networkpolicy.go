// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"net/url"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/c5c3/forge/internal/common/networkpolicy"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
)

// Condition type and reason constants for NetworkPolicy readiness. The reason
// vocabulary is shared across operators via the networkpolicy package.
const (
	conditionTypeNetworkPolicyReady         = "NetworkPolicyReady"
	conditionReasonNetworkPolicyReady       = networkpolicy.ReasonNetworkPolicyReady
	conditionReasonNetworkPolicyNotRequired = networkpolicy.ReasonNetworkPolicyNotRequired
)

// reconcileNetworkPolicy ensures the NetworkPolicy for the dashboard deployment
// matches the desired state, via the shared network-policy flow. It keeps only
// the service-specific parts: the desired policy builder (with its
// keystone-endpoint egress) and the backend identity.
func (r *HorizonReconciler) reconcileNetworkPolicy(ctx context.Context, horizon *horizonv1alpha1.Horizon) (ctrl.Result, error) {
	// buildHorizonNetworkPolicy is only applied on the enabled+non-empty path;
	// build it lazily so a nil or empty-ingress spec takes the delete or
	// fail-closed path without a wasted build.
	var desired *networkingv1.NetworkPolicy
	ingressCount := 0
	if horizon.Spec.NetworkPolicy != nil {
		ingressCount = len(horizon.Spec.NetworkPolicy.Ingress)
		if ingressCount > 0 {
			desired = buildHorizonNetworkPolicy(horizon, r.OperatorNamespace)
		}
	}
	return networkpolicy.Reconcile(ctx, r.Client, r.Scheme, horizon, networkpolicy.FlowParams{
		Configured:         horizon.Spec.NetworkPolicy != nil,
		IngressSourceCount: ingressCount,
		Desired:            desired,
		Name:               subResourceName(horizon),
		Namespace:          horizon.Namespace,
		Conditions:         &horizon.Status.Conditions,
		Generation:         horizon.Generation,
		ConditionType:      conditionTypeNetworkPolicyReady,
	})
}

// buildHorizonNetworkPolicy constructs the desired NetworkPolicy for the
// dashboard pods. It restricts ingress to the dashboard port from the specified
// sources and auto-derives egress rules for DNS (UDP+TCP 53), the Keystone
// endpoint (TCP, port parsed from spec.keystoneEndpoint), and the cache (TCP,
// ports derived from the cache spec). AdditionalEgress rules are appended after
// auto-derived rules.
//
// operatorNamespace is the Namespace the operator Pod runs in. When non-empty,
// an ingress peer selecting that Namespace is appended so the operator's own
// health check can reach the dashboard. When empty (namespace unknown) no such
// peer is added.
func buildHorizonNetworkPolicy(horizon *horizonv1alpha1.Horizon, operatorNamespace string) *networkingv1.NetworkPolicy {
	npSpec := horizon.Spec.NetworkPolicy

	// When spec.gateway is set, an ingress peer selects the whole Gateway
	// namespace so the Gateway data plane can reach the dashboard Service. An
	// empty parentRef.namespace defaults to the CR's own namespace.
	gatewayNamespace := ""
	if horizon.Spec.Gateway != nil {
		gatewayNamespace = horizon.Spec.Gateway.ParentRef.Namespace
		if gatewayNamespace == "" {
			gatewayNamespace = horizon.Namespace
		}
	}
	peers := networkpolicy.IngressPeers(networkpolicy.IngressPeersParams{
		Sources:           npSpec.Ingress,
		GatewayNamespace:  gatewayNamespace,
		OperatorNamespace: operatorNamespace,
	})

	dashboardPort := intstr.FromInt32(horizonAPIPort)
	tcp := corev1.ProtocolTCP
	ingressRules := []networkingv1.NetworkPolicyIngressRule{
		{
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &tcp, Port: &dashboardPort},
			},
			From: peers,
		},
	}

	// Auto-derive egress rules, then append user-specified additional rules.
	egressRules := buildAutoEgressRules(horizon)
	egressRules = append(egressRules, npSpec.AdditionalEgress...)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      subResourceName(horizon),
			Namespace: horizon.Namespace,
			Labels:    commonLabels(horizon),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: selectorLabels(horizon),
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
// (UDP+TCP 53), the Keystone endpoint (TCP, port from keystoneEndpointPort),
// and the cache (TCP, ports from the cache spec). The dashboard has no database
// and no rotation CronJobs, so no DB or kube-apiserver egress is emitted. Rule
// order is deterministic: DNS, keystone, cache.
func buildAutoEgressRules(horizon *horizonv1alpha1.Horizon) []networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP

	// DNS egress: always required (UDP+TCP 53).
	rules := []networkingv1.NetworkPolicyEgressRule{networkpolicy.DNSEgressRule()}

	// Keystone egress: the dashboard authenticates every login against
	// spec.keystoneEndpoint. Port-only — destination unrestricted, matching the
	// keystone-operator's DB/cache egress posture.
	keystonePort := intstr.FromInt32(keystoneEndpointPort(horizon))
	rules = append(rules, networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &tcp, Port: &keystonePort},
		},
	})

	// Cache egress: emitted in both managed and brownfield modes. Cache egress
	// does not gate readiness, so a wrong cache port degrades caching without
	// depooling pods.
	if rule, ok := networkpolicy.CacheEgressRule(horizon.Spec.Cache); ok {
		rules = append(rules, rule)
	}

	return rules
}

// keystoneEndpointPort returns the TCP port of spec.keystoneEndpoint: the
// explicit URL port when present, otherwise 443 for https and 80 for http. An
// unparseable URL (rejected by the webhook, but validation can be bypassed)
// falls back to 443 — the fail-closed choice for an egress rule.
func keystoneEndpointPort(horizon *horizonv1alpha1.Horizon) int32 {
	const maxPort = 65535
	u, err := url.Parse(horizon.Spec.KeystoneEndpoint)
	if err != nil {
		return 443
	}
	if portStr := u.Port(); portStr != "" {
		// ParseInt with bitSize 32 bounds the result to int32; the explicit
		// range check rejects non-port values before the conversion.
		if n, err := strconv.ParseInt(portStr, 10, 32); err == nil && n > 0 && n <= maxPort {
			return int32(n)
		}
	}
	if u.Scheme == "http" {
		return 80
	}
	return 443
}

// cacheEgressPorts returns the distinct Memcached ports the dashboard pods must
// reach, derived from the cache spec, via the shared helper.
func cacheEgressPorts(horizon *horizonv1alpha1.Horizon) []int32 {
	return networkpolicy.CacheEgressPorts(horizon.Spec.Cache)
}
