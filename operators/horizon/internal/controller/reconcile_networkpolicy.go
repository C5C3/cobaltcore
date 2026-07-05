// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/apply"
	"github.com/c5c3/forge/internal/common/conditions"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
)

// Condition type and reason constants for NetworkPolicy readiness.
const (
	conditionTypeNetworkPolicyReady         = "NetworkPolicyReady"
	conditionReasonNetworkPolicyReady       = "NetworkPolicyReady"
	conditionReasonNetworkPolicyNotRequired = "NetworkPolicyNotRequired"
)

// reconcileNetworkPolicy ensures the NetworkPolicy for the dashboard
// deployment matches the desired state. Three lifecycle paths:
//   - spec.networkPolicy set: create or update NetworkPolicy
//   - spec.networkPolicy nil: delete any existing NetworkPolicy
//   - error: propagate errors from ensure/delete operations
func (r *HorizonReconciler) reconcileNetworkPolicy(ctx context.Context, horizon *horizonv1alpha1.Horizon) (ctrl.Result, error) {
	// Path 2: networkPolicy disabled — delete any existing NetworkPolicy.
	if horizon.Spec.NetworkPolicy == nil {
		if err := deleteNetworkPolicy(ctx, r.Client, horizon.Namespace, subResourceName(horizon)); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting NetworkPolicy: %w", err)
		}
		conditions.SetCondition(&horizon.Status.Conditions, metav1.Condition{
			Type:               conditionTypeNetworkPolicyReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: horizon.Generation,
			Reason:             conditionReasonNetworkPolicyNotRequired,
			Message:            "Network isolation is not configured",
		})
		return ctrl.Result{}, nil
	}

	// Defensive guard: refuse to create a NetworkPolicy with empty ingress
	// sources. CRD validation (XValidation) requires size(self.ingress) > 0,
	// but validation can be bypassed (old stored objects, disabled webhooks,
	// direct etcd writes). An Ingress rule with an empty From slice allows
	// all sources on port 8080, which is an unsafe default for a hardening
	// feature. Fail closed rather than open.
	if len(horizon.Spec.NetworkPolicy.Ingress) == 0 {
		return ctrl.Result{}, fmt.Errorf("spec.networkPolicy.ingress must not be empty: refusing to create NetworkPolicy that would allow all ingress")
	}

	// Path 1: networkPolicy enabled — create or update NetworkPolicy.
	np := buildHorizonNetworkPolicy(horizon, r.OperatorNamespace)
	if err := ensureNetworkPolicy(ctx, r.Client, r.Scheme, horizon, np); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring NetworkPolicy: %w", err)
	}
	conditions.SetCondition(&horizon.Status.Conditions, metav1.Condition{
		Type:               conditionTypeNetworkPolicyReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: horizon.Generation,
		Reason:             conditionReasonNetworkPolicyReady,
		Message:            "NetworkPolicy is configured",
	})
	return ctrl.Result{}, nil
}

// buildHorizonNetworkPolicy constructs the desired NetworkPolicy for the
// dashboard pods. It restricts ingress to TCP 8080 from the specified
// sources and auto-derives egress rules for DNS (UDP+TCP 53), the Keystone
// endpoint (TCP, port parsed from spec.keystoneEndpoint), and the cache
// (TCP, ports derived from the cache spec). AdditionalEgress rules are
// appended after auto-derived rules.
//
// operatorNamespace is the Namespace the operator Pod runs in. When
// non-empty, an ingress peer selecting that Namespace is appended so the
// operator's own health check can reach the dashboard on TCP 8080. When
// empty (namespace unknown) no such peer is added.
func buildHorizonNetworkPolicy(horizon *horizonv1alpha1.Horizon, operatorNamespace string) *networkingv1.NetworkPolicy {
	npSpec := horizon.Spec.NetworkPolicy

	// Build ingress peers from spec.networkPolicy.ingress sources.
	var peers []networkingv1.NetworkPolicyPeer
	for _, src := range npSpec.Ingress {
		peer := networkingv1.NetworkPolicyPeer{
			NamespaceSelector: src.NamespaceSelector.DeepCopy(),
		}
		if src.PodSelector != nil {
			peer.PodSelector = src.PodSelector.DeepCopy()
		}
		peers = append(peers, peer)
	}

	// When spec.gateway is also set, append an ingress peer targeting the
	// Gateway's namespace (via kubernetes.io/metadata.name) so the Gateway's
	// data-plane pods can reach the dashboard Service. The gateway data
	// plane's pod labels are implementation-specific and not known to this
	// operator, so we select the entire gateway namespace. When
	// spec.gateway.parentRef.namespace is empty, the Horizon CR's own
	// namespace is assumed, matching the ParentRef lookup semantics.
	if horizon.Spec.Gateway != nil {
		gatewayNS := horizon.Spec.Gateway.ParentRef.Namespace
		if gatewayNS == "" {
			gatewayNS = horizon.Namespace
		}
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"kubernetes.io/metadata.name": gatewayNS,
				},
			},
		})
	}

	// Append an ingress peer for the operator's own Namespace so
	// reconcileHealthCheck can reach the dashboard Service on TCP 8080. The
	// peer selects the entire operator Namespace by the well-known
	// kubernetes.io/metadata.name label. When operatorNamespace is empty
	// (namespace could not be resolved) no peer is added.
	if operatorNamespace != "" {
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"kubernetes.io/metadata.name": operatorNamespace,
				},
			},
		})
	}

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
// and the cache (TCP, ports from cacheEgressPorts). The dashboard has no
// database and no rotation CronJobs, so no DB or kube-apiserver egress is
// emitted. Rule order is deterministic: DNS, keystone, cache.
func buildAutoEgressRules(horizon *horizonv1alpha1.Horizon) []networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP

	// DNS egress: always required (UDP+TCP 53). Destination unrestricted so
	// NodeLocal DNSCache setups outside kube-system keep resolving.
	port53 := intstr.FromInt32(53)
	rules := []networkingv1.NetworkPolicyEgressRule{
		{
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &udp, Port: &port53},
				{Protocol: &tcp, Port: &port53},
			},
		},
	}

	// Keystone egress: the dashboard authenticates every login against
	// spec.keystoneEndpoint. Port-only — destination unrestricted, matching
	// the keystone-operator's DB/cache egress posture.
	keystonePort := intstr.FromInt32(keystoneEndpointPort(horizon))
	rules = append(rules, networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &tcp, Port: &keystonePort},
		},
	})

	// Cache egress: emitted in both managed (cache.clusterRef) and brownfield
	// (cache.servers) modes. Cache egress does not gate readiness, so a wrong
	// cache port degrades caching without depooling pods.
	if cachePorts := cacheEgressPorts(horizon); len(cachePorts) > 0 {
		ports := make([]networkingv1.NetworkPolicyPort, 0, len(cachePorts))
		for i := range cachePorts {
			cachePort := intstr.FromInt32(cachePorts[i])
			ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &cachePort})
		}
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{Ports: ports})
	}

	return rules
}

// keystoneEndpointPort returns the TCP port of spec.keystoneEndpoint: the
// explicit URL port when present, otherwise 443 for https and 80 for http.
// An unparseable URL (rejected by the webhook, but validation can be
// bypassed) falls back to 443 — the fail-closed choice for an egress rule.
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

// cacheEgressPorts returns the distinct Memcached ports the dashboard pods
// must reach, derived from the cache spec. In managed mode (cache.clusterRef
// set) the memcached operator exposes the standard 11211. In brownfield mode
// (cache.servers set) the ports are parsed from each "host:port" entry,
// defaulting to 11211 when an entry omits the port or cannot be parsed. The
// result preserves input order and is deduplicated. It returns nil when no
// cache is configured, in which case no cache egress rule is emitted.
func cacheEgressPorts(horizon *horizonv1alpha1.Horizon) []int32 {
	servers := horizon.Spec.Cache.Servers
	if len(servers) == 0 {
		if horizon.Spec.Cache.ClusterRef != nil {
			return []int32{11211}
		}
		return nil
	}

	const defaultMemcachedPort int32 = 11211
	const maxPort = 65535
	seen := make(map[int32]struct{}, len(servers))
	var ports []int32
	for _, server := range servers {
		port := defaultMemcachedPort
		if _, portStr, err := net.SplitHostPort(server); err == nil {
			if n, err := strconv.ParseInt(portStr, 10, 32); err == nil && n > 0 && n <= maxPort {
				port = int32(n)
			}
		}
		if _, ok := seen[port]; ok {
			continue
		}
		seen[port] = struct{}{}
		ports = append(ports, port)
	}
	return ports
}

// ensureNetworkPolicy creates or updates the NetworkPolicy via Server-Side
// Apply under a fixed field manager and sets the Horizon CR as its controller
// owner so it is garbage-collected with the CR.
//
// Merge strategy: the field manager owns exactly the fields the builder sets
// (the whole Spec and the operator's labels). Labels or annotations the
// operator no longer sets are relinquished and removed by the API server,
// while keys owned by other managers are preserved.
func ensureNetworkPolicy(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, np *networkingv1.NetworkPolicy) error {
	return apply.EnsureObject(ctx, c, scheme, owner, np, apply.FieldManager)
}

// deleteNetworkPolicy deletes the NetworkPolicy identified by namespace and
// name. It is a no-op if the NetworkPolicy does not exist.
func deleteNetworkPolicy(ctx context.Context, c client.Client, namespace, name string) error {
	np := &networkingv1.NetworkPolicy{}
	np.SetName(name)
	np.SetNamespace(namespace)
	if err := client.IgnoreNotFound(c.Delete(ctx, np)); err != nil {
		return fmt.Errorf("deleting NetworkPolicy %s/%s: %w", namespace, name, err)
	}
	return nil
}
