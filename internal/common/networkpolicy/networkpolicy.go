// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package networkpolicy

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"slices"
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
	"github.com/c5c3/forge/internal/common/database"
	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// Condition reason constants for the NetworkPolicy readiness condition, shared
// so every operator's condition uses the same vocabulary.
const (
	ReasonNetworkPolicyReady       = "NetworkPolicyReady"
	ReasonNetworkPolicyNotRequired = "NetworkPolicyNotRequired"
)

// Ensure creates or updates the NetworkPolicy via Server-Side Apply under the
// shared field manager and sets the owner CR as its controller owner so it is
// garbage-collected with the CR.
//
// Merge strategy: the field manager owns exactly the fields the builder sets
// (the whole Spec and the operator's labels). Labels or annotations the operator
// no longer sets are relinquished and removed by the API server, while keys
// owned by other managers are preserved. A converged NetworkPolicy is applied
// without a write.
func Ensure(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, np *networkingv1.NetworkPolicy) error {
	return apply.EnsureObject(ctx, c, scheme, owner, np, apply.FieldManager)
}

// Delete deletes the NetworkPolicy identified by namespace and name. It is a
// no-op if the NetworkPolicy does not exist.
func Delete(ctx context.Context, c client.Client, namespace, name string) error {
	np := &networkingv1.NetworkPolicy{}
	np.SetName(name)
	np.SetNamespace(namespace)
	if err := client.IgnoreNotFound(c.Delete(ctx, np)); err != nil {
		return fmt.Errorf("deleting NetworkPolicy %s/%s: %w", namespace, name, err)
	}
	return nil
}

// DNSEgressRule returns the always-required DNS egress rule (UDP+TCP 53).
// Destination is unrestricted so NodeLocal DNSCache setups outside kube-system
// keep resolving.
func DNSEgressRule() networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	port53 := intstr.FromInt32(53)
	return networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &udp, Port: &port53},
			{Protocol: &tcp, Port: &port53},
		},
	}
}

// DatabaseEgressRule returns the database egress rule (TCP, port from the
// database spec, honoring spec.database.port with the 3306 default). It is
// emitted in both managed (ClusterRef) and brownfield (host) modes; the port is
// the same value the readiness probe TCP-connects to. Destination is
// unrestricted, matching the DNS/cache egress posture.
func DatabaseEgressRule(db commonv1.DatabaseSpec) networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP
	dbPort := intstr.FromInt32(database.Port(&db))
	return networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &tcp, Port: &dbPort},
		},
	}
}

// CacheEgressRule returns the cache egress rule (TCP, ports from
// CacheEgressPorts) and ok=true, or a zero rule and ok=false when no cache is
// configured (neither ClusterRef nor Servers), in which case no cache egress
// should be emitted.
func CacheEgressRule(cache commonv1.CacheSpec) (networkingv1.NetworkPolicyEgressRule, bool) {
	cachePorts := CacheEgressPorts(cache)
	if len(cachePorts) == 0 {
		return networkingv1.NetworkPolicyEgressRule{}, false
	}
	tcp := corev1.ProtocolTCP
	ports := make([]networkingv1.NetworkPolicyPort, 0, len(cachePorts))
	for i := range cachePorts {
		cachePort := intstr.FromInt32(cachePorts[i])
		ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &cachePort})
	}
	return networkingv1.NetworkPolicyEgressRule{Ports: ports}, true
}

// CacheEgressPorts returns the distinct Memcached ports the pods must reach,
// derived from the cache spec. In managed mode (cache.clusterRef set) the
// memcached operator exposes the standard 11211. In brownfield mode
// (cache.servers set) the ports are parsed from each "host:port" entry,
// defaulting to 11211 when an entry omits the port or cannot be parsed. The
// result preserves input order and is deduplicated. It returns nil when no cache
// is configured.
func CacheEgressPorts(cache commonv1.CacheSpec) []int32 {
	servers := cache.Servers
	if len(servers) == 0 {
		if cache.ClusterRef != nil {
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

// S3EgressRule returns the object-store egress rule (TCP, one port per distinct
// endpoint parsed from hostURLs) and ok=true, or a zero rule and ok=false when
// no entry yields a usable port. Each port is the URL's explicit port, or the
// scheme default (443 for https, 80 for http) when none is given; entries that
// fail to parse or carry no usable host or port (invalid or out-of-range ports)
// are skipped. Destination is unrestricted, matching the DNS/database/cache
// egress posture, because the backends are user-supplied external hosts.
//
// Unlike CacheEgressPorts, which preserves input order, the ports are sorted
// ascending: S3 backends come from an unordered set of backend CRs, so a stable
// order keeps the rendered rule deterministic.
func S3EgressRule(hostURLs []string) (networkingv1.NetworkPolicyEgressRule, bool) {
	const maxPort = 65535
	seen := make(map[int32]struct{}, len(hostURLs))
	var ports []int32
	for _, hostURL := range hostURLs {
		u, err := url.Parse(hostURL)
		if err != nil || u.Hostname() == "" {
			continue
		}
		var port int32
		if portStr := u.Port(); portStr != "" {
			// ParseInt with bitSize 32 bounds the result to int32; the explicit
			// range check rejects non-port values before the conversion.
			n, err := strconv.ParseInt(portStr, 10, 32)
			if err != nil || n <= 0 || n > maxPort {
				continue
			}
			port = int32(n)
		} else {
			switch u.Scheme {
			case "https":
				port = 443
			case "http":
				port = 80
			default:
				continue
			}
		}
		if _, ok := seen[port]; ok {
			continue
		}
		seen[port] = struct{}{}
		ports = append(ports, port)
	}
	if len(ports) == 0 {
		return networkingv1.NetworkPolicyEgressRule{}, false
	}
	slices.Sort(ports)
	tcp := corev1.ProtocolTCP
	rulePorts := make([]networkingv1.NetworkPolicyPort, 0, len(ports))
	for i := range ports {
		s3Port := intstr.FromInt32(ports[i])
		rulePorts = append(rulePorts, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &s3Port})
	}
	return networkingv1.NetworkPolicyEgressRule{Ports: rulePorts}, true
}

// IngressPeersParams carries the inputs of IngressPeers: the user-declared
// sources plus the optional gateway- and operator-namespace peers.
type IngressPeersParams struct {
	// Sources are the user-declared ingress sources.
	Sources []commonv1.NetworkPolicyIngressSource
	// GatewayNamespace, when non-empty, appends a peer selecting that entire
	// namespace so the Gateway data plane can reach the Service.
	GatewayNamespace string
	// OperatorNamespace, when non-empty, appends a peer selecting the operator's
	// namespace so its health check can reach the Service.
	OperatorNamespace string
}

// IngressPeers assembles the NetworkPolicy ingress peers in a deterministic
// order: the user-declared sources, then the gateway-namespace peer (if any),
// then the operator-namespace peer (if any). The gateway and operator peers
// select the entire namespace by the well-known kubernetes.io/metadata.name
// label because those pods' labels are not known to the caller.
func IngressPeers(p IngressPeersParams) []networkingv1.NetworkPolicyPeer {
	var peers []networkingv1.NetworkPolicyPeer
	for _, src := range p.Sources {
		peer := networkingv1.NetworkPolicyPeer{
			NamespaceSelector: src.NamespaceSelector.DeepCopy(),
		}
		if src.PodSelector != nil {
			peer.PodSelector = src.PodSelector.DeepCopy()
		}
		peers = append(peers, peer)
	}
	if p.GatewayNamespace != "" {
		peers = append(peers, namespacePeer(p.GatewayNamespace))
	}
	if p.OperatorNamespace != "" {
		peers = append(peers, namespacePeer(p.OperatorNamespace))
	}
	return peers
}

func namespacePeer(namespace string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"kubernetes.io/metadata.name": namespace,
			},
		},
	}
}

// FlowParams carries everything Reconcile needs. The service-specific parts —
// whether spec.networkPolicy is set, the built desired policy, its identity, and
// the condition type — are supplied by the caller; the three-path flow itself
// (delete / fail-closed guard / ensure) is identical across operators.
type FlowParams struct {
	// Configured reports whether spec.networkPolicy is set.
	Configured bool
	// IngressSourceCount is the number of user-declared ingress sources; the
	// fail-closed guard refuses to create a policy with zero sources.
	IngressSourceCount int
	// Desired is the built NetworkPolicy, applied when Configured is true.
	Desired *networkingv1.NetworkPolicy
	// Name and Namespace identify the policy for the delete path.
	Name      string
	Namespace string
	// Conditions is the CR's condition slice, mutated in place.
	Conditions *[]metav1.Condition
	// Generation is stamped onto every condition the flow writes.
	Generation int64
	// ConditionType is the readiness condition the flow reports on.
	ConditionType string
}

// Reconcile ensures the NetworkPolicy matches the desired state. It is the
// shared body of every operator's reconcileNetworkPolicy sub-reconciler:
//
//   - spec.networkPolicy nil: delete any existing NetworkPolicy and set the
//     condition True/NetworkPolicyNotRequired.
//   - empty ingress sources: refuse to create a policy that would allow all
//     ingress (fail closed), returning an error.
//   - spec.networkPolicy set: apply the policy via SSA and set the condition
//     True/NetworkPolicyReady.
func Reconcile(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, p FlowParams) (ctrl.Result, error) {
	// Path 2: networkPolicy disabled — delete any existing NetworkPolicy.
	if !p.Configured {
		if err := Delete(ctx, c, p.Namespace, p.Name); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting NetworkPolicy: %w", err)
		}
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: p.Generation,
			Reason:             ReasonNetworkPolicyNotRequired,
			Message:            "Network isolation is not configured",
		})
		return ctrl.Result{}, nil
	}

	// Defensive guard: refuse to create a NetworkPolicy with empty ingress
	// sources. CRD validation (XValidation) requires size(self.ingress) > 0, but
	// validation can be bypassed (old stored objects, disabled webhooks, direct
	// etcd writes). An Ingress rule with an empty From slice allows all sources,
	// which is an unsafe default for a hardening feature. Fail closed.
	if p.IngressSourceCount == 0 {
		return ctrl.Result{}, fmt.Errorf("spec.networkPolicy.ingress must not be empty: refusing to create NetworkPolicy that would allow all ingress")
	}

	// Path 1: networkPolicy enabled — create or update NetworkPolicy.
	if err := Ensure(ctx, c, scheme, owner, p.Desired); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring NetworkPolicy: %w", err)
	}
	conditions.SetCondition(p.Conditions, metav1.Condition{
		Type:               p.ConditionType,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: p.Generation,
		Reason:             ReasonNetworkPolicyReady,
		Message:            "NetworkPolicy is configured",
	})
	return ctrl.Result{}, nil
}
