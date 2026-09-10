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
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// Condition type and reason constants for NetworkPolicy readiness. The reason
// vocabulary is shared across operators via the networkpolicy package.
const (
	conditionTypeNetworkPolicyReady         = "NetworkPolicyReady"
	conditionReasonNetworkPolicyReady       = networkpolicy.ReasonNetworkPolicyReady
	conditionReasonNetworkPolicyNotRequired = networkpolicy.ReasonNetworkPolicyNotRequired
)

// reconcileNetworkPolicy ensures the NetworkPolicy for the Cinder pods matches
// the desired state, via the shared network-policy flow. It keeps only the
// service-specific parts: the desired policy builder and the backend identity.
//
// It takes the broker port and the export hosts because neither is reachable
// through a label selector: egressPort is the port of the transport URL the
// messaging step materialised, shareHosts are the NFS exports the backends step
// projected. Both are zero-valued on the waiting paths of their steps, and the
// builder then omits the rule they would have opened.
func (r *CinderReconciler) reconcileNetworkPolicy(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder, egressPort int32, shareHosts []string,
) (ctrl.Result, error) {
	// buildCinderNetworkPolicy is only applied on the enabled+non-empty path;
	// build it lazily so a nil or empty-ingress spec takes the delete or
	// fail-closed path without a wasted build.
	var desired *networkingv1.NetworkPolicy
	ingressCount := 0
	if cinder.Spec.NetworkPolicy != nil {
		ingressCount = len(cinder.Spec.NetworkPolicy.Ingress)
		if ingressCount > 0 {
			desired = buildCinderNetworkPolicy(cinder, r.OperatorNamespace, egressPort, shareHosts)
		}
	}
	return networkpolicy.Reconcile(ctx, children, r.Scheme, cinder, networkpolicy.FlowParams{
		Configured:         cinder.Spec.NetworkPolicy != nil,
		IngressSourceCount: ingressCount,
		Desired:            desired,
		Name:               cinder.Name,
		Namespace:          cinder.Namespace,
		Conditions:         &cinder.Status.Conditions,
		Generation:         cinder.Generation,
		ConditionType:      conditionTypeNetworkPolicyReady,
	})
}

// buildCinderNetworkPolicy constructs the desired NetworkPolicy for the Cinder
// pods. It restricts ingress to the API port from the specified sources and
// auto-derives egress rules for DNS (UDP+TCP 53), the database, the cache, the
// Keystone endpoint, the Glance and Barbican endpoints, the message bus and the
// NFS exports. AdditionalEgress rules are appended after the auto-derived rules.
//
// operatorNamespace is the Namespace the operator Pod runs in. When non-empty,
// an ingress peer selecting that Namespace is appended so the operator's own
// health check can reach the Cinder API. When empty (namespace unknown) no such
// peer is added.
func buildCinderNetworkPolicy(cinder *cinderv1alpha1.Cinder, operatorNamespace string,
	egressPort int32, shareHosts []string,
) *networkingv1.NetworkPolicy {
	npSpec := cinder.Spec.NetworkPolicy

	// When spec.gateway is set, an ingress peer selects the whole Gateway
	// namespace so the Gateway data plane can reach the API Service. An empty
	// parentRef.namespace defaults to the CR's own namespace.
	gatewayNamespace := ""
	if cinder.Spec.Gateway != nil {
		gatewayNamespace = cinder.Spec.Gateway.ParentRef.Namespace
		if gatewayNamespace == "" {
			gatewayNamespace = cinder.Namespace
		}
	}
	peers := networkpolicy.IngressPeers(networkpolicy.IngressPeersParams{
		Sources:           npSpec.Ingress,
		GatewayNamespace:  gatewayNamespace,
		OperatorNamespace: operatorNamespace,
	})

	apiPort := intstr.FromInt32(cinderAPIPort)
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
	egressRules := buildAutoEgressRules(cinder, egressPort, shareHosts)
	egressRules = append(egressRules, npSpec.AdditionalEgress...)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cinder.Name,
			Namespace: cinder.Namespace,
			Labels:    commonLabels(cinder),
		},
		Spec: networkingv1.NetworkPolicySpec{
			// The selector carries no component key, so one policy covers the API
			// pods, the scheduler, every volume service, the backup service and the
			// Job pods alike. All of them read the same database, publish on the same
			// broker and mount the same exports, so splitting the egress set per
			// component would duplicate every rule and let the copies drift.
			PodSelector: metav1.LabelSelector{
				MatchLabels: selectorLabels(cinder),
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

// buildAutoEgressRules constructs the auto-derived egress rules in a
// deterministic order: DNS, database, cache, Keystone, the Glance and Barbican
// endpoints, the message bus, the NFS exports. The database rule is always
// emitted (Cinder always has a database); every other rule is emitted only when
// its input yields a port, which is what keeps a Keystone-free deployment from
// opening a Keystone port nothing dials.
func buildAutoEgressRules(cinder *cinderv1alpha1.Cinder, egressPort int32,
	shareHosts []string,
) []networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP

	rules := []networkingv1.NetworkPolicyEgressRule{
		// DNS egress: always required (UDP+TCP 53).
		networkpolicy.DNSEgressRule(),
		// Database egress: Cinder connects to MariaDB in both managed and
		// brownfield modes; the port matches the readiness posture.
		networkpolicy.DatabaseEgressRule(cinder.Spec.Database),
	}

	// Cache egress: emitted in both managed and brownfield modes. Cache egress
	// does not gate readiness, so a wrong cache port degrades caching without
	// depooling pods.
	if rule, ok := networkpolicy.CacheEgressRule(cinder.Spec.Cache); ok {
		rules = append(rules, rule)
	}

	// Keystone egress: keystonemiddleware validates the token of every
	// authenticated request against spec.keystoneEndpoint server-side, so without
	// this rule the API answers 503 on all of them while both readiness signals,
	// the kubelet probes and the operator health check, keep passing, because both
	// target the unauthenticated /healthcheck. Port-only: destination
	// unrestricted, matching the DB/cache egress posture. A Cinder without
	// spec.keystoneEndpoint validates no tokens and gets no rule.
	if cinder.Spec.KeystoneEndpoint != "" {
		keystonePort := intstr.FromInt32(networkpolicy.KeystoneEndpointPort(cinder.Spec.KeystoneEndpoint))
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &tcp, Port: &keystonePort},
			},
		})
	}

	// Service egress: Glance serves the image a create-volume-from-image request
	// reads, Barbican holds the keys castellan encrypts volumes with. Both are
	// user-supplied external endpoints, so the rule is port-only, and both are
	// optional: an endpoint left unset contributes nothing.
	if rule, ok := networkpolicy.HostPortsEgressRule(serviceEgressURLs(cinder)); ok {
		rules = append(rules, rule)
	}

	// Messaging egress: the RPC bus the API, the scheduler, the volume services
	// and the backup service publish on. The broker is reached by whatever the
	// transport URL names, which may be a Service in another namespace or a host
	// outside the cluster, so there is no selector to write and the rule stays
	// port-only. The port is zero on the messaging step's waiting path, where no
	// transport URL has been materialised.
	if egressPort != 0 {
		busPort := intstr.FromInt32(egressPort)
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &tcp, Port: &busPort},
			},
		})
	}

	// Export egress: the NFS servers the volume services mount their shares from.
	// Every export speaks NFSv4 on the same port, so a Cinder with any attached
	// backend opens 2049 once; a Cinder with none opens nothing.
	if rule, ok := networkpolicy.HostPortsEgressRule(shareHosts); ok {
		rules = append(rules, rule)
	}

	return rules
}

// serviceEgressURLs returns the endpoints of the OpenStack services Cinder calls
// out to, skipping the ones the CR leaves unset. Both are already URLs, so they
// need no rewriting before networkpolicy.HostPortsEgressRule parses their ports.
func serviceEgressURLs(cinder *cinderv1alpha1.Cinder) []string {
	var urls []string
	if cinder.Spec.GlanceEndpoint != "" {
		urls = append(urls, cinder.Spec.GlanceEndpoint)
	}
	if cinder.Spec.KeyManager != nil && cinder.Spec.KeyManager.Barbican != nil &&
		cinder.Spec.KeyManager.Barbican.Endpoint != "" {
		urls = append(urls, cinder.Spec.KeyManager.Barbican.Endpoint)
	}
	return urls
}
