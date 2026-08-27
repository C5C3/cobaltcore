// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/naming"
	"github.com/c5c3/cobaltcore/internal/common/networkpolicy"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// Condition type and reason constants for NetworkPolicy readiness. The reason
// vocabulary is shared across operators via the networkpolicy package.
const (
	conditionTypeNetworkPolicyReady         = "NetworkPolicyReady"
	conditionReasonNetworkPolicyReady       = networkpolicy.ReasonNetworkPolicyReady
	conditionReasonNetworkPolicyNotRequired = networkpolicy.ReasonNetworkPolicyNotRequired
)

// reconcileNetworkPolicy ensures the NetworkPolicy for the Neutron pods matches
// the desired state, via the shared network-policy flow. It keeps only the
// service-specific parts: the desired policy builder and the backend identity.
//
// It takes the resolved OVN endpoints and the broker port because both are
// egress destinations no label selector reaches: ovn resolves to the addresses
// the OVNCentral published, egressPort to the port of the transport URL the
// messaging step materialised. Both are zero-valued on the waiting paths of
// their steps, and the builder then omits the rule they would have opened.
func (r *NeutronReconciler) reconcileNetworkPolicy(ctx context.Context, children client.Client,
	neutron *neutronv1alpha1.Neutron, ovn resolvedOVNEndpoints, egressPort int32,
) (ctrl.Result, error) {
	// buildNeutronNetworkPolicy is only applied on the enabled+non-empty path;
	// build it lazily so a nil or empty-ingress spec takes the delete or
	// fail-closed path without a wasted build.
	var desired *networkingv1.NetworkPolicy
	ingressCount := 0
	if neutron.Spec.NetworkPolicy != nil {
		ingressCount = len(neutron.Spec.NetworkPolicy.Ingress)
		if ingressCount > 0 {
			desired = buildNeutronNetworkPolicy(neutron, r.OperatorNamespace, ovn, egressPort)
		}
	}
	return networkpolicy.Reconcile(ctx, children, r.Scheme, neutron, networkpolicy.FlowParams{
		Configured:         neutron.Spec.NetworkPolicy != nil,
		IngressSourceCount: ingressCount,
		Desired:            desired,
		Name:               neutron.Name,
		Namespace:          neutron.Namespace,
		Conditions:         &neutron.Status.Conditions,
		Generation:         neutron.Generation,
		ConditionType:      conditionTypeNetworkPolicyReady,
	})
}

// buildNeutronNetworkPolicy constructs the desired NetworkPolicy for the Neutron
// pods. It restricts ingress to the API port from the specified sources and
// auto-derives egress rules for DNS (UDP+TCP 53), the database, the Keystone
// endpoint, the cache, the OVN databases, and the message bus. AdditionalEgress
// rules are appended after the auto-derived rules.
//
// operatorNamespace is the Namespace the operator Pod runs in. When non-empty,
// an ingress peer selecting that Namespace is appended so the operator's own
// health check can reach the Neutron API. When empty (namespace unknown) no such
// peer is added.
func buildNeutronNetworkPolicy(neutron *neutronv1alpha1.Neutron, operatorNamespace string,
	ovn resolvedOVNEndpoints, egressPort int32,
) *networkingv1.NetworkPolicy {
	npSpec := neutron.Spec.NetworkPolicy

	// When spec.gateway is set, an ingress peer selects the whole Gateway
	// namespace so the Gateway data plane can reach the API Service. An empty
	// parentRef.namespace defaults to the CR's own namespace.
	gatewayNamespace := ""
	if neutron.Spec.Gateway != nil {
		gatewayNamespace = neutron.Spec.Gateway.ParentRef.Namespace
		if gatewayNamespace == "" {
			gatewayNamespace = neutron.Namespace
		}
	}
	peers := networkpolicy.IngressPeers(networkpolicy.IngressPeersParams{
		Sources:           npSpec.Ingress,
		GatewayNamespace:  gatewayNamespace,
		OperatorNamespace: operatorNamespace,
	})

	apiPort := intstr.FromInt32(neutronAPIPort)
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
	egressRules := buildAutoEgressRules(neutron, ovn, egressPort)
	egressRules = append(egressRules, npSpec.AdditionalEgress...)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      neutron.Name,
			Namespace: neutron.Namespace,
			Labels:    commonLabels(neutron),
		},
		Spec: networkingv1.NetworkPolicySpec{
			// The selector carries no component key, so one policy covers the API
			// pods, both worker Deployments and the ovn-db-sync Job pods alike. All of
			// them read the same database, publish on the same broker and dial the same
			// two OVN databases, so splitting the egress set per component would
			// duplicate every rule and let the copies drift.
			PodSelector: metav1.LabelSelector{
				MatchLabels: naming.SelectorLabels(neutronAppName, neutron.Name),
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

// buildAutoEgressRules constructs the auto-derived egress rules for DNS (UDP+TCP
// 53), the database (TCP, port from the database spec), the Keystone endpoint
// (TCP, port from networkpolicy.KeystoneEndpointPort), the cache (TCP, ports
// from the cache spec), the OVN databases (TCP, one port per distinct published
// endpoint), and the message bus (TCP, the transport URL's port). Rule order is
// deterministic: DNS, database, keystone, cache, OVN, messaging. The database
// and keystone rules are always emitted (Neutron always has a database and
// always validates tokens); the other three are emitted only when their inputs
// yield a port.
func buildAutoEgressRules(neutron *neutronv1alpha1.Neutron, ovn resolvedOVNEndpoints,
	egressPort int32,
) []networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP

	rules := []networkingv1.NetworkPolicyEgressRule{
		// DNS egress: always required (UDP+TCP 53).
		networkpolicy.DNSEgressRule(),
		// Database egress: Neutron connects to MariaDB in both managed and
		// brownfield modes; the port matches the readiness posture.
		networkpolicy.DatabaseEgressRule(neutron.Spec.Database),
	}

	// Keystone egress: keystonemiddleware validates the token of every
	// authenticated request against spec.keystoneEndpoint server-side, so without
	// this rule the API answers 503 on all of them while both readiness signals,
	// the kubelet probes and the operator health check, keep passing, because both
	// target the unauthenticated version document. Port-only: destination
	// unrestricted, matching the DB/cache egress posture.
	keystonePort := intstr.FromInt32(networkpolicy.KeystoneEndpointPort(neutron.Spec.KeystoneEndpoint))
	rules = append(rules, networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &tcp, Port: &keystonePort},
		},
	})

	// Cache egress: emitted in both managed and brownfield modes. Cache egress
	// does not gate readiness, so a wrong cache port degrades caching without
	// depooling pods.
	if rule, ok := networkpolicy.CacheEgressRule(neutron.Spec.Cache); ok {
		rules = append(rules, rule)
	}

	// OVN egress: the two databases the ML2/OVN mechanism driver and the
	// maintenance worker dial. Nothing is emitted while the OVNCentral has not
	// published its addresses yet, which is the same pass on which the config step
	// renders no [ovn] section.
	if rule, ok := networkpolicy.HostPortsEgressRule(ovnEgressURLs(ovn.nbAddress, ovn.sbAddress)); ok {
		rules = append(rules, rule)
	}

	// Messaging egress: the RPC bus every Neutron process publishes on. The broker
	// is reached by whatever the transport URL names, which may be a Service in
	// another namespace or a host outside the cluster, so there is no selector to
	// write and the rule stays port-only. The port is zero on the messaging step's
	// waiting path, where no transport URL has been materialised.
	if egressPort != 0 {
		busPort := intstr.FromInt32(egressPort)
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &tcp, Port: &busPort},
			},
		})
	}

	return rules
}

// ovnEgressURLs rewrites the published Northbound and Southbound addresses into
// the URL form networkpolicy.HostPortsEgressRule parses. Each address is a
// comma-separated list of ovsdb connection strings ("ssl:10.0.0.1:6641" or
// "tcp:10.0.0.1:6641"), which url.Parse reads as a scheme followed by an opaque
// body rather than as a host and a port, so every member is rewritten to
// "tcp://<host>:<port>". The scheme is only there to make the string parse; the
// rule is TCP either way.
//
// The helper reshapes strings and nothing else. HostPortsEgressRule parses the
// ports out of the result, skips what carries no usable port, deduplicates and
// sorts them (internal/common/networkpolicy/networkpolicy.go:159-204), so two
// members on the same port open one port. Empty addresses yield an empty slice
// and no rule at all.
func ovnEgressURLs(nb, sb string) []string {
	var urls []string
	for _, address := range []string{nb, sb} {
		for _, member := range strings.Split(address, ",") {
			hostPort, ok := strings.CutPrefix(strings.TrimSpace(member), "ssl:")
			if !ok {
				hostPort, ok = strings.CutPrefix(strings.TrimSpace(member), "tcp:")
			}
			if !ok || hostPort == "" {
				continue
			}
			urls = append(urls, "tcp://"+hostPort)
		}
	}
	return urls
}
