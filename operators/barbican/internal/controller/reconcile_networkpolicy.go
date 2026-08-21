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

	"github.com/c5c3/cobaltcore/internal/common/networkpolicy"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
)

// Condition type and reason constants for NetworkPolicy readiness. The reason
// vocabulary is shared across operators via the networkpolicy package.
const (
	conditionTypeNetworkPolicyReady         = "NetworkPolicyReady"
	conditionReasonNetworkPolicyReady       = networkpolicy.ReasonNetworkPolicyReady
	conditionReasonNetworkPolicyNotRequired = networkpolicy.ReasonNetworkPolicyNotRequired
)

// reconcileNetworkPolicy ensures the NetworkPolicy for the Barbican API
// deployment matches the desired state, via the shared network-policy flow. It
// keeps only the service-specific parts: the desired policy builder (with its
// DB/cache/OpenBao egress) and the backend identity. It takes the secret-store
// projection so the OpenBao egress set tracks every attached store's host.
func (r *BarbicanReconciler) reconcileNetworkPolicy(ctx context.Context, children client.Client, barbican *barbicanv1alpha1.Barbican, projection secretStoreProjection) (ctrl.Result, error) {
	// buildBarbicanNetworkPolicy is only applied on the enabled+non-empty path;
	// build it lazily so a nil or empty-ingress spec takes the delete or
	// fail-closed path without a wasted build.
	var desired *networkingv1.NetworkPolicy
	ingressCount := 0
	if barbican.Spec.NetworkPolicy != nil {
		ingressCount = len(barbican.Spec.NetworkPolicy.Ingress)
		if ingressCount > 0 {
			desired = buildBarbicanNetworkPolicy(barbican, r.OperatorNamespace, projection.hosts)
		}
	}
	return networkpolicy.Reconcile(ctx, children, r.Scheme, barbican, networkpolicy.FlowParams{
		Configured:         barbican.Spec.NetworkPolicy != nil,
		IngressSourceCount: ingressCount,
		Desired:            desired,
		Name:               subResourceName(barbican),
		Namespace:          barbican.Namespace,
		Conditions:         &barbican.Status.Conditions,
		Generation:         barbican.Generation,
		ConditionType:      conditionTypeNetworkPolicyReady,
	})
}

// buildBarbicanNetworkPolicy constructs the desired NetworkPolicy for the
// Barbican API pods. It restricts ingress to the API port from the specified
// sources and auto-derives egress rules for DNS (UDP+TCP 53), the database, the
// Keystone endpoint, the cache, and the OpenBao secret stores. AdditionalEgress
// rules are appended after the auto-derived rules.
//
// operatorNamespace is the Namespace the operator Pod runs in. When non-empty,
// an ingress peer selecting that Namespace is appended so the operator's own
// health check can reach the Barbican API. When empty (namespace unknown) no
// such peer is added. hosts is the server URL set of every attached secret
// store, used to derive the OpenBao egress ports.
func buildBarbicanNetworkPolicy(barbican *barbicanv1alpha1.Barbican, operatorNamespace string, hosts []string) *networkingv1.NetworkPolicy {
	npSpec := barbican.Spec.NetworkPolicy

	// When spec.gateway is set, an ingress peer selects the whole Gateway
	// namespace so the Gateway data plane can reach the API Service. An empty
	// parentRef.namespace defaults to the CR's own namespace.
	gatewayNamespace := ""
	if barbican.Spec.Gateway != nil {
		gatewayNamespace = barbican.Spec.Gateway.ParentRef.Namespace
		if gatewayNamespace == "" {
			gatewayNamespace = barbican.Namespace
		}
	}
	peers := networkpolicy.IngressPeers(networkpolicy.IngressPeersParams{
		Sources:           npSpec.Ingress,
		GatewayNamespace:  gatewayNamespace,
		OperatorNamespace: operatorNamespace,
	})

	apiPort := intstr.FromInt32(barbicanAPIPort)
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
	egressRules := buildAutoEgressRules(barbican, hosts)
	egressRules = append(egressRules, npSpec.AdditionalEgress...)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      subResourceName(barbican),
			Namespace: barbican.Namespace,
			Labels:    commonLabels(barbican),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: selectorLabels(barbican),
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
// endpoint (TCP, port from networkpolicy.KeystoneEndpointPort), the cache (TCP,
// ports from the cache spec), and the OpenBao servers (TCP, one port per distinct store host).
// Rule order is deterministic: DNS, database, keystone, cache, OpenBao. The
// database and keystone rules are always emitted (Barbican always has a database
// and always validates tokens); the cache and OpenBao rules are emitted only when
// their inputs yield ports.
func buildAutoEgressRules(barbican *barbicanv1alpha1.Barbican, hosts []string) []networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP

	rules := []networkingv1.NetworkPolicyEgressRule{
		// DNS egress: always required (UDP+TCP 53).
		networkpolicy.DNSEgressRule(),
		// Database egress: Barbican connects to MariaDB in both managed and
		// brownfield modes; the port matches the readiness posture.
		networkpolicy.DatabaseEgressRule(barbican.Spec.Database),
	}

	// Keystone egress: keystonemiddleware validates the token of every
	// authenticated request against spec.keystoneEndpoint server-side, so without
	// this rule the API answers 503 on all of them while both readiness signals —
	// the kubelet probes and the operator health check — keep passing, because
	// both target the unauthenticated healthcheck app. Port-only — destination
	// unrestricted, matching the DB/cache egress posture.
	keystonePort := intstr.FromInt32(networkpolicy.KeystoneEndpointPort(barbican.Spec.KeystoneEndpoint))
	rules = append(rules, networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &tcp, Port: &keystonePort},
		},
	})

	// Cache egress: emitted in both managed and brownfield modes. Cache egress
	// does not gate readiness, so a wrong cache port degrades caching without
	// depooling pods.
	if rule, ok := networkpolicy.CacheEgressRule(barbican.Spec.Cache); ok {
		rules = append(rules, rule)
	}

	// OpenBao egress: the servers the vault plugin stores secret material in.
	// Emitted only when at least one credential-ready store yields a usable port —
	// the projection collects the hosts of those stores alone, so a store nobody
	// has authenticated yet opens no port.
	if rule, ok := networkpolicy.HostPortsEgressRule(hosts); ok {
		rules = append(rules, rule)
	}

	return rules
}
