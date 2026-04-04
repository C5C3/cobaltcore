// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/forge/internal/common/conditions"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Feature: CC-0039

// Condition type and reason constants for NetworkPolicy readiness (CC-0039).
const (
	conditionTypeNetworkPolicyReady        = "NetworkPolicyReady"
	conditionReasonNetworkPolicyReady      = "NetworkPolicyReady"
	conditionReasonNetworkPolicyNotRequired = "NetworkPolicyNotRequired"
)

// reconcileNetworkPolicy ensures the NetworkPolicy for the Keystone API
// deployment matches the desired state. Three lifecycle paths:
//   - spec.networkPolicy set: create or update NetworkPolicy (CC-0039, REQ-001)
//   - spec.networkPolicy nil: delete any existing NetworkPolicy (CC-0039, REQ-003)
//   - error: propagate errors from ensure/delete operations (CC-0039, REQ-007)
func (r *KeystoneReconciler) reconcileNetworkPolicy(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	// Path 2: networkPolicy disabled — delete any existing NetworkPolicy (CC-0039, REQ-003).
	if keystone.Spec.NetworkPolicy == nil {
		if err := deleteNetworkPolicy(ctx, r.Client, keystone.Namespace, apiResourceName(keystone)); err != nil {
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
	// for a hardening feature. Fail closed rather than open (CC-0039).
	if len(keystone.Spec.NetworkPolicy.Ingress) == 0 {
		return ctrl.Result{}, fmt.Errorf("spec.networkPolicy.ingress must not be empty: refusing to create NetworkPolicy that would allow all ingress (CC-0039)")
	}

	// Path 1: networkPolicy enabled — create or update NetworkPolicy (CC-0039, REQ-001).
	np := buildKeystoneNetworkPolicy(keystone)
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
// sources and auto-derives egress rules for DNS (UDP+TCP 53), MariaDB
// (TCP 3306 when database.ClusterRef is set), and Memcached (TCP 11211
// when cache.ClusterRef is set). AdditionalEgress rules are appended after
// auto-derived rules (CC-0039, REQ-001, REQ-004, REQ-005).
func buildKeystoneNetworkPolicy(keystone *keystonev1alpha1.Keystone) *networkingv1.NetworkPolicy {
	npSpec := keystone.Spec.NetworkPolicy

	// Build ingress peers from spec.networkPolicy.ingress sources (CC-0039, REQ-004).
	var peers []networkingv1.NetworkPolicyPeer
	for _, src := range npSpec.Ingress {
		peer := networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: src.NamespaceSelector,
			},
		}
		if len(src.PodSelector) > 0 {
			peer.PodSelector = &metav1.LabelSelector{
				MatchLabels: src.PodSelector,
			}
		}
		peers = append(peers, peer)
	}

	port5000 := intstr.FromInt32(5000)
	tcp := corev1.ProtocolTCP
	ingressRules := []networkingv1.NetworkPolicyIngressRule{
		{
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &tcp, Port: &port5000},
			},
			From: peers,
		},
	}

	// Auto-derive egress rules (CC-0039, REQ-005).
	egressRules := buildAutoEgressRules(keystone)

	// Append user-specified additional egress rules (CC-0039, REQ-006).
	egressRules = append(egressRules, npSpec.AdditionalEgress...)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      apiResourceName(keystone),
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

// buildAutoEgressRules constructs the auto-derived egress rules for DNS,
// MariaDB (managed mode only), and Memcached (managed mode only) (CC-0039, REQ-005).
func buildAutoEgressRules(keystone *keystonev1alpha1.Keystone) []networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP

	// DECISION: DNS egress allows traffic to any destination on port 53 rather
	// than restricting to kube-system — Chose broad rule because CoreDNS may
	// run outside kube-system (e.g., NodeLocal DNSCache in each node's
	// namespace). A namespace-restricted rule would silently break DNS
	// resolution in such environments. Reviewer: please verify. (CC-0039)

	// DNS egress: always required (UDP+TCP 53) (CC-0039, REQ-005).
	port53 := intstr.FromInt32(53)
	rules := []networkingv1.NetworkPolicyEgressRule{
		{
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &udp, Port: &port53},
				{Protocol: &tcp, Port: &port53},
			},
		},
	}

	// MariaDB egress: only in managed mode (database.ClusterRef set) (CC-0039, REQ-005).
	// NOTE: This rule restricts only the port (TCP 3306), not the destination — any
	// destination is allowed on that port. Tightening this to specific pod labels
	// requires resolving actual service pod labels from the ClusterRef, which adds
	// complexity and a potential circular dependency. Deferred to a future iteration.
	if keystone.Spec.Database.ClusterRef != nil {
		port3306 := intstr.FromInt32(3306)
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &tcp, Port: &port3306},
			},
		})
	}

	// Memcached egress: only in managed mode (cache.ClusterRef set) (CC-0039, REQ-005).
	// NOTE: Same as MariaDB above — restricts only the port (TCP 11211), not the
	// destination. Tightening requires resolving service pod labels from ClusterRef.
	// Deferred to a future iteration.
	if keystone.Spec.Cache.ClusterRef != nil {
		port11211 := intstr.FromInt32(11211)
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &tcp, Port: &port11211},
			},
		})
	}

	return rules
}

// DECISION: ensureNetworkPolicy and deleteNetworkPolicy live in the controller
// package rather than internal/common/deployment/ — Chose to keep them here
// because NetworkPolicy is Keystone-specific (no second consumer yet). Moving
// to the common package is warranted when a second operator needs the same
// create-or-update logic. Reviewer: please verify. (CC-0039)

// ensureNetworkPolicy creates a NetworkPolicy if it does not exist or updates
// its spec and metadata if it already exists. An owner reference is set on the
// NetworkPolicy so that it is garbage-collected when the owning resource is
// deleted (CC-0039, REQ-002).
//
// Merge strategy: the operator is authoritative over .Spec — the entire Spec
// is overwritten with the desired state on every reconciliation. For labels and
// annotations, desired keys are written into the existing map (additive merge);
// user-added keys that the operator does not manage are preserved. Keys that
// were previously set by the operator but are no longer in the desired state
// will remain until manually removed. This is intentional: it avoids
// accidentally stripping third-party metadata (e.g. Istio sidecar annotations)
// while still ensuring the operator's own labels stay correct.
func ensureNetworkPolicy(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, np *networkingv1.NetworkPolicy) error {
	existing := &networkingv1.NetworkPolicy{}
	err := c.Get(ctx, client.ObjectKeyFromObject(np), existing)

	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(owner, np, scheme); err != nil {
			return fmt.Errorf("setting owner reference on NetworkPolicy %s/%s: %w", np.Namespace, np.Name, err)
		}
		if err := c.Create(ctx, np); err != nil {
			return fmt.Errorf("creating NetworkPolicy %s/%s: %w", np.Namespace, np.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting NetworkPolicy %s/%s: %w", np.Namespace, np.Name, err)
	}

	// NetworkPolicy exists: reconcile metadata (ownerRefs/labels/annotations) and spec.
	// Snapshot before mutations to detect whether an update is necessary (CC-0039).
	before := existing.DeepCopy()

	// Ensure controller owner reference is enforced so garbage collection
	// behaves correctly even if the ref was removed out-of-band (CC-0039).
	if err := controllerutil.SetControllerReference(owner, existing, scheme); err != nil {
		return fmt.Errorf("updating owner reference on NetworkPolicy %s/%s: %w", existing.Namespace, existing.Name, err)
	}

	// Merge desired labels into existing labels; extra user-added keys are
	// preserved, keys present on the desired NetworkPolicy are authoritative (CC-0039).
	if np.Labels != nil {
		if existing.Labels == nil {
			existing.Labels = make(map[string]string, len(np.Labels))
		}
		for k, v := range np.Labels {
			existing.Labels[k] = v
		}
	}

	// Merge desired annotations into existing annotations (CC-0039).
	if np.Annotations != nil {
		if existing.Annotations == nil {
			existing.Annotations = make(map[string]string, len(np.Annotations))
		}
		for k, v := range np.Annotations {
			existing.Annotations[k] = v
		}
	}

	// Reconcile spec to the desired state (CC-0039).
	existing.Spec = np.Spec

	// Only issue an API update when something actually changed to avoid
	// unnecessary write load, spurious watch events, and 409 Conflict
	// errors (CC-0039).
	if !apiequality.Semantic.DeepEqual(existing.Spec, before.Spec) ||
		!apiequality.Semantic.DeepEqual(npNormalizeMap(existing.Labels), npNormalizeMap(before.Labels)) ||
		!apiequality.Semantic.DeepEqual(npNormalizeMap(existing.Annotations), npNormalizeMap(before.Annotations)) ||
		!apiequality.Semantic.DeepEqual(existing.OwnerReferences, before.OwnerReferences) {
		if err := c.Update(ctx, existing); err != nil {
			return fmt.Errorf("updating NetworkPolicy %s/%s: %w", existing.Namespace, existing.Name, err)
		}
	}

	return nil
}

// npNormalizeMap converts empty maps to nil so apiequality.Semantic.DeepEqual
// does not report spurious diffs between nil and empty maps (CC-0039).
func npNormalizeMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

// deleteNetworkPolicy deletes the NetworkPolicy identified by namespace and
// name. It is a no-op if the NetworkPolicy does not exist (CC-0039, REQ-003).
func deleteNetworkPolicy(ctx context.Context, c client.Client, namespace, name string) error {
	np := &networkingv1.NetworkPolicy{}
	np.SetName(name)
	np.SetNamespace(namespace)
	if err := client.IgnoreNotFound(c.Delete(ctx, np)); err != nil {
		return fmt.Errorf("deleting NetworkPolicy %s/%s: %w", namespace, name, err)
	}
	return nil
}
