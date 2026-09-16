// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/naming"
	"github.com/c5c3/cobaltcore/internal/common/networkpolicy"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// Condition type and reason constants for NetworkPolicy readiness. The reason
// vocabulary is shared across operators via the networkpolicy package.
const (
	conditionTypeNetworkPolicyReady         = "NetworkPolicyReady"
	conditionReasonNetworkPolicyReady       = networkpolicy.ReasonNetworkPolicyReady
	conditionReasonNetworkPolicyNotRequired = networkpolicy.ReasonNetworkPolicyNotRequired
)

// The VNC display ports a hypervisor serves instance consoles on: libvirt hands
// every guest a port from remote_display_port_min to remote_display_port_max,
// whose defaults span this whole range.
const (
	vncDisplayPortMin int32 = 5900
	vncDisplayPortMax int32 = 65535
)

// The ports of the siblings Nova calls as a client, used when a CR pins no
// override and the address comes out of the Keystone catalog. They are the
// upstream defaults every catalog entry of a colocated control plane carries.
const (
	placementDefaultPort int32 = 8778
	neutronDefaultPort   int32 = 9696
	glanceDefaultPort    int32 = 9292
	cinderDefaultPort    int32 = 8776
	barbicanDefaultPort  int32 = 9311
)

// reconcileNetworkPolicy ensures the NetworkPolicy for the Nova pods matches the
// desired state, via the shared network-policy flow. It keeps only the
// service-specific parts: the desired policy builder and the backend identity.
//
// It takes the broker port because no label selector reaches it: egressPort is
// the port of the transport URL the messaging step materialised. It is zero on
// that step's waiting path, and the builder then omits the rule it would have
// opened.
func (r *NovaReconciler) reconcileNetworkPolicy(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova, egressPort int32,
) (ctrl.Result, error) {
	// buildNovaNetworkPolicy is only applied on the enabled+non-empty path; build
	// it lazily so a nil or empty-ingress spec takes the delete or fail-closed
	// path without a wasted build.
	var desired *networkingv1.NetworkPolicy
	ingressCount := 0
	if nova.Spec.NetworkPolicy != nil {
		ingressCount = len(nova.Spec.NetworkPolicy.Ingress)
		if ingressCount > 0 {
			desired = buildNovaNetworkPolicy(nova, r.OperatorNamespace, egressPort)
		}
	}
	res, err := networkpolicy.Reconcile(ctx, children, r.Scheme, nova, networkpolicy.FlowParams{
		Configured:         nova.Spec.NetworkPolicy != nil,
		IngressSourceCount: ingressCount,
		Desired:            desired,
		Name:               nova.Name,
		Namespace:          nova.Namespace,
		Conditions:         &nova.Status.Conditions,
		Generation:         nova.Generation,
		ConditionType:      conditionTypeNetworkPolicyReady,
	})
	if err != nil || !res.IsZero() {
		return res, err
	}

	// The console proxy's own egress policy exists exactly while the main policy
	// isolates a projected proxy; the flow above has already failed closed on a
	// policy without ingress sources.
	if nova.Spec.NetworkPolicy == nil || !nova.Spec.ConsoleProxyEnabled() {
		if err := networkpolicy.Delete(ctx, children, nova.Namespace, consoleProxyName(nova)); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting console proxy NetworkPolicy: %w", err)
		}
		return res, nil
	}
	if err := networkpolicy.Ensure(ctx, children, r.Scheme, nova, buildConsoleProxyNetworkPolicy(nova)); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring console proxy NetworkPolicy: %w", err)
	}
	return res, nil
}

// buildConsoleProxyNetworkPolicy constructs the egress policy only the console
// proxy pods carry. nova-novncproxy bridges a browser's session to the VNC
// server of the hypervisor the instance runs on, a host outside the cluster on a
// port libvirt picked for that guest, so the main policy's egress set blocks
// every console while the proxy stays Ready. The rule is port-only for the same
// reason the bus rule is: the hypervisors are addressed by whatever the compute
// registered, not by anything a selector reaches. It lives in a policy of its
// own because policies are additive per pod: on the main policy it would open
// the whole display range to every Nova pod.
func buildConsoleProxyNetworkPolicy(nova *novav1alpha1.Nova) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	displayPort := intstr.FromInt32(vncDisplayPortMin)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      consoleProxyName(nova),
			Namespace: nova.Namespace,
			Labels:    commonLabels(nova),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: componentSelectorLabels(nova, componentConsoleProxy),
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				Ports: []networkingv1.NetworkPolicyPort{{
					Protocol: &tcp,
					Port:     &displayPort,
					EndPort:  ptr.To(vncDisplayPortMax),
				}},
			}},
		},
	}
}

// buildNovaNetworkPolicy constructs the desired NetworkPolicy for the Nova pods.
// It restricts ingress to the three front-end ports from the specified sources
// and auto-derives egress rules for DNS, the two databases, the cache, the
// Keystone endpoint, the siblings Nova calls as a client and the message bus.
// AdditionalEgress rules are appended after the auto-derived ones.
//
// operatorNamespace is the Namespace the operator Pod runs in. When non-empty,
// an ingress peer selecting that Namespace is appended so the operator's own
// health check can reach the Nova API. When empty (namespace unknown) no such
// peer is added.
func buildNovaNetworkPolicy(nova *novav1alpha1.Nova, operatorNamespace string,
	egressPort int32,
) *networkingv1.NetworkPolicy {
	npSpec := nova.Spec.NetworkPolicy

	tcp := corev1.ProtocolTCP
	ports := make([]networkingv1.NetworkPolicyPort, 0, 3)
	for _, port := range []int32{novaAPIPort, novaMetadataPort, novaConsolePort} {
		frontEndPort := intstr.FromInt32(port)
		ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &frontEndPort})
	}
	// One rule for all three front ends. Their peers are the same set: a Gateway
	// data plane reaches whichever of them it fronts, and the metadata agent and
	// the operator's health check arrive from namespaces the sources name.
	ingressRules := []networkingv1.NetworkPolicyIngressRule{
		{
			Ports: ports,
			From:  novaIngressPeers(nova, operatorNamespace),
		},
	}

	// Auto-derive egress rules, then append user-specified additional rules.
	egressRules := buildAutoEgressRules(nova, egressPort)
	egressRules = append(egressRules, npSpec.AdditionalEgress...)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nova.Name,
			Namespace: nova.Namespace,
			Labels:    commonLabels(nova),
		},
		Spec: networkingv1.NetworkPolicySpec{
			// The selector carries no component key, so one policy covers the API
			// pods, the metadata API, the scheduler, the conductor, the console
			// proxy and the Job pods alike. All of them read the same two schemas,
			// publish on the same broker and call the same siblings, so splitting
			// the egress set per component would duplicate every rule and let the
			// copies drift. The console proxy alone dials something none of the
			// others do, which buildConsoleProxyNetworkPolicy adds for it.
			PodSelector: metav1.LabelSelector{
				MatchLabels: naming.SelectorLabels(novaAppName, nova.Name),
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

// novaIngressPeers assembles the peers of the single ingress rule: the
// user-declared sources, then one peer per namespace a Gateway fronts a Nova
// front end from, then the operator's own namespace.
func novaIngressPeers(nova *novav1alpha1.Nova, operatorNamespace string) []networkingv1.NetworkPolicyPeer {
	peers := networkpolicy.IngressPeers(networkpolicy.IngressPeersParams{
		Sources: nova.Spec.NetworkPolicy.Ingress,
	})
	for _, namespace := range gatewayNamespaces(nova) {
		peers = append(peers, networkpolicy.IngressPeers(networkpolicy.IngressPeersParams{
			GatewayNamespace: namespace,
		})...)
	}
	return append(peers, networkpolicy.IngressPeers(networkpolicy.IngressPeersParams{
		OperatorNamespace: operatorNamespace,
	})...)
}

// gatewayNamespaces returns the namespaces of the Gateways that expose this
// Nova's front ends, in spec order and deduplicated: one Gateway commonly
// carries all three routes, and a repeated peer widens nothing. An empty
// parentRef.namespace defaults to the CR's own namespace. A disabled console
// proxy's gateway contributes nothing, exactly as the console route step treats
// it: no route fronts a proxy that is not projected.
func gatewayNamespaces(nova *novav1alpha1.Nova) []string {
	gateways := []*novav1alpha1.GatewaySpec{nova.Spec.Gateway, nova.Spec.Metadata.Gateway}
	if nova.Spec.ConsoleProxyEnabled() {
		gateways = append(gateways, nova.Spec.ConsoleProxy.Gateway)
	}
	var namespaces []string
	for _, gw := range gateways {
		if gw == nil {
			continue
		}
		namespace := gw.ParentRef.Namespace
		if namespace == "" {
			namespace = nova.Namespace
		}
		if !slices.Contains(namespaces, namespace) {
			namespaces = append(namespaces, namespace)
		}
	}
	return namespaces
}

// buildAutoEgressRules constructs the auto-derived egress rules in a
// deterministic order: DNS, the nova_api schema, the cell schema, the cache,
// Keystone, the siblings Nova calls as a client, the message bus. The two
// database rules and the Keystone rule are always emitted, because a Nova
// without either is not a posture the CRD offers; every other rule is emitted
// only when its input yields a port.
func buildAutoEgressRules(nova *novav1alpha1.Nova, egressPort int32) []networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP

	// The two schemas carry their own connection parameters and may sit on
	// different MariaDB instances, so each contributes its own rule rather than
	// one rule standing in for both.
	keystonePort := intstr.FromInt32(networkpolicy.KeystoneEndpointPort(nova.Spec.KeystoneEndpoint))
	rules := []networkingv1.NetworkPolicyEgressRule{
		networkpolicy.DNSEgressRule(),
		networkpolicy.DatabaseEgressRule(nova.Spec.APIDatabase),
		networkpolicy.DatabaseEgressRule(nova.Spec.Database),
	}

	// Cache egress: emitted in both managed and brownfield modes. Cache egress
	// does not gate readiness, so a wrong cache port degrades caching without
	// depooling pods.
	if rule, ok := networkpolicy.CacheEgressRule(nova.Spec.Cache); ok {
		rules = append(rules, rule)
	}

	// Keystone egress: keystonemiddleware validates the token of every
	// authenticated request against spec.keystoneEndpoint server-side, and every
	// call Nova makes to a sibling starts by fetching a token from it. Port-only:
	// destination unrestricted, matching the DB/cache egress posture.
	rules = append(rules, networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &tcp, Port: &keystonePort},
		},
	})

	// Sibling egress: Placement holds the allocation of every instance, Neutron
	// the ports, Glance the images, and the two optional ones the volumes and
	// their keys. The addresses are catalog entries or user-supplied overrides,
	// so the rule is port-only.
	if rule, ok := networkpolicy.HostPortsEgressRule(siblingEgressURLs(nova)); ok {
		rules = append(rules, rule)
	}

	// Messaging egress: the RPC bus the API, the scheduler, the conductor and
	// every nova-compute meet on. The broker is reached by whatever the transport
	// URL names, which may be a Service in another namespace or a host outside
	// the cluster, so there is no selector to write and the rule stays port-only.
	// The port is zero on the messaging step's waiting path, where no transport
	// URL has been materialised.
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

// siblingEgressURLs returns the endpoints of the OpenStack services Nova calls
// as a client: the pinned override of each block, or the conventional
// cluster-local URL of that service when the block pins none, since an
// override-free Nova resolves the address from the Keystone catalog and the
// operator has no way to read it. Cinder and Barbican contribute only while
// their block is enabled; the three mandatory siblings always do.
func siblingEgressURLs(nova *novav1alpha1.Nova) []string {
	endpoints := nova.Spec.Endpoints
	urls := []string{
		siblingEgressURL(endpoints.Placement.Override, "placement", placementDefaultPort),
		siblingEgressURL(endpoints.Neutron.Override, "neutron", neutronDefaultPort),
		siblingEgressURL(endpoints.Glance.Override, "glance", glanceDefaultPort),
	}
	if endpoints.Cinder.Enabled {
		urls = append(urls, siblingEgressURL(endpoints.Cinder.Override, "cinder", cinderDefaultPort))
	}
	if endpoints.Barbican.Enabled {
		urls = append(urls, siblingEgressURL(endpoints.Barbican.Override, "barbican", barbicanDefaultPort))
	}
	return urls
}

// siblingEgressURL returns the override when the block pins one, and otherwise
// the conventional URL of that service. Only the port is read off the result, so
// the conventional host stands in for whatever the catalog resolves to.
func siblingEgressURL(override, service string, port int32) string {
	if override != "" {
		return override
	}
	return fmt.Sprintf("http://%s:%d", service, port)
}
