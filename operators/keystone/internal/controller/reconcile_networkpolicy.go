// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"net"
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
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Condition type and reason constants for NetworkPolicy readiness.
const (
	conditionTypeNetworkPolicyReady         = "NetworkPolicyReady"
	conditionReasonNetworkPolicyReady       = "NetworkPolicyReady"
	conditionReasonNetworkPolicyNotRequired = "NetworkPolicyNotRequired"
)

// reconcileNetworkPolicy ensures the NetworkPolicy for the Keystone API
// deployment matches the desired state. Three lifecycle paths:
//   - spec.networkPolicy set: create or update NetworkPolicy
//   - spec.networkPolicy nil: delete any existing NetworkPolicy
//   - error: propagate errors from ensure/delete operations
func (r *KeystoneReconciler) reconcileNetworkPolicy(ctx context.Context, keystone *keystonev1alpha1.Keystone, fed *federationProjection) (ctrl.Result, error) {
	// Path 2: networkPolicy disabled — delete any existing NetworkPolicy.
	if keystone.Spec.NetworkPolicy == nil {
		if err := deleteNetworkPolicy(ctx, r.Client, keystone.Namespace, subResourceName(keystone)); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting NetworkPolicy: %w", err)
		}
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypeNetworkPolicyReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason:             conditionReasonNetworkPolicyNotRequired,
			Message:            "Network isolation is not configured",
		})
		return ctrl.Result{}, nil
	}

	// Defensive guard: refuse to create a NetworkPolicy with empty ingress sources.
	// CRD validation (XValidation) requires size(self.ingress) > 0, but validation can be
	// bypassed (old stored objects, disabled webhooks, direct etcd writes). An Ingress rule
	// with an empty From slice allows all sources on port 5000, which is an unsafe default
	// for a hardening feature. Fail closed rather than open.
	if len(keystone.Spec.NetworkPolicy.Ingress) == 0 {
		return ctrl.Result{}, fmt.Errorf("spec.networkPolicy.ingress must not be empty: refusing to create NetworkPolicy that would allow all ingress")
	}

	// Path 1: networkPolicy enabled — create or update NetworkPolicy.
	np := buildKeystoneNetworkPolicy(keystone, r.OperatorNamespace, fed)
	if err := ensureNetworkPolicy(ctx, r.Client, r.Scheme, keystone, np); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring NetworkPolicy: %w", err)
	}
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               conditionTypeNetworkPolicyReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: keystone.Generation,
		Reason:             conditionReasonNetworkPolicyReady,
		Message:            "NetworkPolicy is configured",
	})
	return ctrl.Result{}, nil
}

// buildKeystoneNetworkPolicy constructs the desired NetworkPolicy for the
// Keystone API pods. It restricts ingress to TCP 5000 from the specified
// sources and auto-derives egress rules for DNS (UDP+TCP 53), the
// kube-apiserver (TCP 443+6443, required by the rotation CronJob pods that
// share this selector), the database (TCP, port from dbPort, both managed and
// brownfield modes), and the cache (TCP, ports derived from the cache spec,
// both managed and brownfield modes). AdditionalEgress rules are appended after
// auto-derived rules.
//
// operatorNamespace is the Namespace the operator Pod runs in. When non-empty,
// an ingress peer selecting that Namespace is appended so the operator's own
// health check (reconcileHealthCheck GETs the Keystone Service on TCP 5000) is
// not blocked by the policy (issue #461). When empty (namespace unknown) no
// such peer is added.
//
// fed is the federation projection: when set, the ingress target port is the
// sidecar's (the Service targetPort switched there) and per-issuer egress
// ports are appended so the sidecar can reach the identity provider.
func buildKeystoneNetworkPolicy(keystone *keystonev1alpha1.Keystone, operatorNamespace string, fed *federationProjection) *networkingv1.NetworkPolicy {
	npSpec := keystone.Spec.NetworkPolicy

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

	// DECISION: When spec.gateway is also set, append an ingress peer targeting
	// the Gateway's namespace (via kubernetes.io/metadata.name) on TCP 5000 so
	// the Gateway's data-plane pods can reach the Keystone Service. The gateway
	// data plane's pod labels are implementation-specific (Kong/Envoy/NGINX/…)
	// and not known to this operator, so we select by the entire gateway
	// namespace rather than by pod labels. When spec.gateway.parentRef.namespace
	// is empty, the Keystone CR's own namespace is assumed, matching the
	// ParentRef lookup semantics. When spec.networkPolicy is nil we take the
	// delete path above — no extra rule is needed. requires Gateway
	// reachability whenever both spec.gateway and spec.networkPolicy are set;
	// the tradeoff is that shared Gateway namespaces grant namespace-wide
	// ingress rather than pod-level scoping.
	if keystone.Spec.Gateway != nil {
		gatewayNS := keystone.Spec.Gateway.ParentRef.Namespace
		if gatewayNS == "" {
			gatewayNS = keystone.Namespace
		}
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"kubernetes.io/metadata.name": gatewayNS,
				},
			},
		})
	}

	// DECISION: Append an ingress peer for the operator's own Namespace so
	// reconcileHealthCheck can reach the Keystone API Service on TCP 5000. The
	// operator runs in a dedicated Namespace (keystone-system) distinct from the
	// workload Namespace, and ingress otherwise only admits the user-declared
	// sources and the gateway namespace — so without this peer KeystoneAPIReady
	// flips False permanently for a healthy deployment (issue #461). The peer
	// selects the entire operator Namespace by the well-known
	// kubernetes.io/metadata.name label rather than the operator's pod labels,
	// which are not known to this build helper. When operatorNamespace is empty
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

	// The ingress target is the port the Service targetPort points at: the
	// sidecar's when federation is active, uWSGI's 5000 otherwise
	// (NetworkPolicy ports are evaluated against the pod port, post-DNAT).
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
	// pod. Port-only like the database/cache rules — see the DNS DECISION in
	// buildAutoEgressRules for why destination scoping is deferred.
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
// (UDP+TCP 53), the kube-apiserver (TCP 443+6443), the database (TCP, port from
// dbPort), and the cache (TCP, ports from cacheEgressPorts). Database and cache
// egress are emitted in both managed (ClusterRef) and brownfield modes. Rule
// order is deterministic: DNS, apiserver, database, cache.
func buildAutoEgressRules(keystone *keystonev1alpha1.Keystone) []networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP

	// DECISION: DNS egress allows traffic to any destination on port 53 rather
	// than restricting to kube-system — Chose broad rule because CoreDNS may
	// run outside kube-system (e.g., NodeLocal DNSCache in each node's
	// namespace). A namespace-restricted rule would silently break DNS
	// resolution in such environments.

	// DNS egress: always required (UDP+TCP 53).
	port53 := intstr.FromInt32(53)
	rules := []networkingv1.NetworkPolicyEgressRule{
		{
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &udp, Port: &port53},
				{Protocol: &tcp, Port: &port53},
			},
		},
	}

	// kube-apiserver egress: always required (TCP 443+6443).
	// DECISION: The fernet/credential/admin-password rotation CronJob pods carry
	// commonLabels — a superset of the Deployment selectorLabels — so they are
	// selected by this NetworkPolicy's podSelector and share its egress rules.
	// Those scripts PATCH the rotated keys back to a Kubernetes Secret via
	// kubernetes.default.svc, so the policy must allow egress to the apiserver or
	// every scheduled rotation stops at its first run (issue #461). The ClusterIP
	// Service port is 443; on enforcing CNIs (Calico/Cilium) egress is evaluated
	// after DNAT against the kube-apiserver pod port, commonly 6443 — both are
	// allowed (port-only, destination unrestricted). Giving the apiserver port to
	// the Keystone API pods that share this selector is a small, accepted
	// attack-surface increase: the Deployment pod selector is immutable, so the
	// rotation pods cannot be handed a distinct label set without recreating the
	// Deployment.
	port443 := intstr.FromInt32(443)
	port6443 := intstr.FromInt32(6443)
	rules = append(rules, networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &tcp, Port: &port443},
			{Protocol: &tcp, Port: &port6443},
		},
	})

	// Database egress: emitted in both managed (database.ClusterRef) and
	// brownfield (database.host) modes. The port is derived from dbPort, which
	// honors spec.database.port (default 3306) — the same value the readiness
	// probe TCP-connects to via OS_DATABASE__CONNECTION. Without this rule the
	// probe fails on an enforcing CNI and every pod is depooled (issue #461).
	// NOTE: port-only — destination unrestricted, see the DNS DECISION above for
	// why tightening to pod labels is deferred.
	dbPortValue := intstr.FromInt32(dbPort(keystone))
	rules = append(rules, networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &tcp, Port: &dbPortValue},
		},
	})

	// Cache egress: emitted in both managed (cache.ClusterRef) and brownfield
	// (cache.servers) modes. Ports are derived from cacheEgressPorts; in
	// brownfield mode they come from the "host:port" server strings (issue #461).
	// Cache egress does not gate readiness, so a wrong cache port degrades
	// caching without depooling pods. NOTE: port-only — destination unrestricted.
	if cachePorts := cacheEgressPorts(keystone); len(cachePorts) > 0 {
		ports := make([]networkingv1.NetworkPolicyPort, 0, len(cachePorts))
		for i := range cachePorts {
			cachePort := intstr.FromInt32(cachePorts[i])
			ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &cachePort})
		}
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{Ports: ports})
	}

	return rules
}

// cacheEgressPorts returns the distinct Memcached ports the Keystone API pods
// must reach, derived from the cache spec. In managed mode (cache.clusterRef
// set) the memcached operator exposes the standard 11211. In brownfield mode
// (cache.servers set) the ports are parsed from each "host:port" entry,
// defaulting to 11211 when an entry omits the port or cannot be parsed. The
// result preserves input order and is deduplicated. It returns nil when no
// cache is configured (neither ClusterRef nor Servers), in which case no cache
// egress rule is emitted.
func cacheEgressPorts(keystone *keystonev1alpha1.Keystone) []int32 {
	servers := keystone.Spec.Cache.Servers
	if len(servers) == 0 {
		if keystone.Spec.Cache.ClusterRef != nil {
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
			// ParseInt with bitSize 32 bounds the result to int32; the explicit
			// range check rejects non-port values before the conversion.
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

// DECISION: ensureNetworkPolicy and deleteNetworkPolicy live in the controller
// package rather than internal/common/deployment/ — Chose to keep them here
// because NetworkPolicy is Keystone-specific (no second consumer yet). Moving
// to the common package is warranted when a second operator needs the same
// create-or-update logic.

// ensureNetworkPolicy creates or updates the NetworkPolicy via Server-Side
// Apply under a fixed field manager and sets the Keystone CR as its controller
// owner so it is garbage-collected with the CR.
//
// Merge strategy: the field manager owns exactly the fields the builder sets
// (the whole Spec and the operator's labels). Labels or annotations the
// operator no longer sets are relinquished and removed by the API server, while
// keys owned by other managers (e.g. user- or mesh-added metadata) are
// preserved. A converged NetworkPolicy is applied without a write.
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
