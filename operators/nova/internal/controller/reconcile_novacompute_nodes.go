// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// The reasons of the NodesReady condition.
const (
	conditionReasonNodesResolved   = "NodesResolved"
	conditionReasonNodesForbidden  = "NodesForbidden"
	conditionReasonNodeListError   = "NodeListError"
	conditionReasonNoMatchingNodes = "NoMatchingNodes"
	conditionReasonNodeConflict    = "NodeConflict"
)

// zoneLabel is the node label a pool's zone is read from. openstack-hypervisor-
// operator puts every host into the aggregate named after it.
const zoneLabel = "topology.kubernetes.io/zone"

// reconcileNovaComputeNodes resolves which nodes the pool holds and in which
// phase, and sets NodesReady.
//
// Nodes are read through an uncached reader: the target cluster's own for a
// placed CR, the management cluster's API reader for a local one. A cached
// Node read would start a cluster-wide Node informer, which cannot sync on an
// install whose RBAC is namespace-scoped. Only a node's name and labels count,
// so the reads fetch its metadata, not its status. A deleting CR selects
// nothing, so it lists nothing and only reads back the nodes it still holds.
//
// The rivals are the other NovaComputes of the same Nova on the same cluster.
// The rules are planNovaComputeNodes'.
func (r *NovaComputeReconciler) reconcileNovaComputeNodes(ctx context.Context, children client.Client,
	cr *novav1alpha1.NovaCompute, pass *novaComputePass,
) (ctrl.Result, error) {
	reader, err := commonmulticluster.ResolveChildrenAPIReader(ctx, r.Resolver, r.APIReader, cr.Spec.TargetClusterRef)
	if err != nil {
		novaComputeSkeleton.MarkFailed(cr, conditionTypeNodesReady, commonmulticluster.TargetClusterUnavailable, err)
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	}
	if reader == nil {
		reader = children
	}

	deleting := !cr.DeletionTimestamp.IsZero()
	var selected []metav1.PartialObjectMetadata
	if !deleting {
		list := &metav1.PartialObjectMetadataList{}
		list.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("NodeList"))
		if err := reader.List(ctx, list, client.MatchingLabels(cr.Spec.NodeSelector)); err != nil {
			return nodeReadFailed(cr, fmt.Errorf("listing nodes: %w", err))
		}
		selected = list.Items
	}
	selectedNames := make(map[string]bool, len(selected))
	for i := range selected {
		selectedNames[selected[i].Name] = true
	}

	// A held node the selection no longer returns is read back on its own: it
	// may still exist under other labels (and be what a rival selects now), or
	// be gone from the cluster.
	others := map[string]*metav1.PartialObjectMetadata{}
	for _, entry := range cr.Status.Nodes {
		if entry.Phase == novav1alpha1.NovaComputeNodeConflict || selectedNames[entry.Name] {
			continue
		}
		node := &metav1.PartialObjectMetadata{}
		node.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Node"))
		if err := reader.Get(ctx, types.NamespacedName{Name: entry.Name}, node); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nodeReadFailed(cr, fmt.Errorf("getting node %s: %w", entry.Name, err))
		}
		others[entry.Name] = node
	}

	siblings, err := novaComputesOfNova(ctx, r.Client, cr.Namespace, cr.Spec.NovaRef.Name)
	if err != nil {
		novaComputeSkeleton.MarkFailed(cr, conditionTypeNodesReady, conditionReasonNodeListError, err)
		return ctrl.Result{}, err
	}
	pass.siblings = siblings
	var rivals []novav1alpha1.NovaCompute
	for _, sibling := range siblings {
		if sibling.Name != cr.Name && sameTargetCluster(&sibling, cr) {
			rivals = append(rivals, sibling)
		}
	}

	plan := planNovaComputeNodes(nodePlanInput{self: cr, selected: selected, others: others, rivals: rivals})
	cr.Status.Nodes = plan.nodes
	pass.excluded = plan.excluded
	pass.keepPods = plan.keepPods
	pass.handover = plan.handover

	for _, c := range plan.newConflicts {
		r.Recorder.Eventf(cr, corev1.EventTypeWarning, eventReasonNodeConflict,
			"Node %s is held by NovaCompute %s; this pool runs no pod on it", c.node, c.rival)
	}

	condition := metav1.Condition{
		Type:               conditionTypeNodesReady,
		ObservedGeneration: cr.Generation,
	}
	switch {
	case len(selected) == 0 && !plan.holdsAny():
		condition.Status = metav1.ConditionFalse
		condition.Reason = conditionReasonNoMatchingNodes
		condition.Message = "No node matches spec.nodeSelector and the pool holds none"
	case len(plan.excluded) > 0:
		var held []string
		for _, entry := range plan.nodes {
			if entry.Phase == novav1alpha1.NovaComputeNodeConflict {
				held = append(held, fmt.Sprintf("%s (held by %s)", entry.Name, entry.ConflictsWith))
			}
		}
		condition.Status = metav1.ConditionFalse
		condition.Reason = conditionReasonNodeConflict
		condition.Message = "Selected nodes another NovaCompute of the same Nova holds: " + strings.Join(held, ", ")
	default:
		condition.Status = metav1.ConditionTrue
		condition.Reason = conditionReasonNodesResolved
		condition.Message = fmt.Sprintf("%d nodes selected, %d leaving", len(selected), plan.leaving())
	}
	conditions.SetCondition(&cr.Status.Conditions, condition)
	return ctrl.Result{}, nil
}

// nodeReadFailed reports a failed Node read. A 403 is the namespace-scoped
// install that cannot read Nodes at all, which only an RBAC change fixes, so it
// waits instead of backing off.
func nodeReadFailed(cr *novav1alpha1.NovaCompute, err error) (ctrl.Result, error) {
	if apierrors.IsForbidden(err) {
		novaComputeSkeleton.MarkFailed(cr, conditionTypeNodesReady, conditionReasonNodesForbidden, err)
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	}
	novaComputeSkeleton.MarkFailed(cr, conditionTypeNodesReady, conditionReasonNodeListError, err)
	return ctrl.Result{}, err
}

// nodePlanInput is what planNovaComputeNodes decides from.
type nodePlanInput struct {
	// self is the CR, with its current status.nodes and deletion state.
	self *novav1alpha1.NovaCompute
	// selected are the nodes self's selector matches; empty while deleting.
	selected []metav1.PartialObjectMetadata
	// others are the held nodes selected does not contain and that still exist.
	others map[string]*metav1.PartialObjectMetadata
	// rivals are the other NovaComputes of the same Nova on the same cluster.
	rivals []novav1alpha1.NovaCompute
}

// nodeConflict is a node that went into Conflict in this pass.
type nodeConflict struct{ node, rival string }

// nodePlan is planNovaComputeNodes' result.
type nodePlan struct {
	// nodes is the new status.nodes, sorted by name.
	nodes []novav1alpha1.NovaComputeNodeStatus
	// excluded are the Conflict nodes, keepPods the Draining ones, both sorted.
	excluded []string
	keepPods []string
	// handover are the Releasing nodes a non-deleting rival selects now.
	handover map[string]bool
	// newConflicts are the conflicts that were not recorded before.
	newConflicts []nodeConflict
}

// holdsAny reports whether the plan keeps a node that is not in Conflict.
func (p nodePlan) holdsAny() bool {
	return slices.ContainsFunc(p.nodes, func(e novav1alpha1.NovaComputeNodeStatus) bool {
		return e.Phase != novav1alpha1.NovaComputeNodeConflict
	})
}

// leaving counts the nodes in Draining or Releasing.
func (p nodePlan) leaving() int {
	n := 0
	for _, e := range p.nodes {
		if e.Phase == novav1alpha1.NovaComputeNodeDraining || e.Phase == novav1alpha1.NovaComputeNodeReleasing {
			n++
		}
	}
	return n
}

// planNovaComputeNodes applies the node rules. It is a pure function, so the
// rules are tested without a cluster.
//
// A node N is held by a CR when N is in that CR's status.nodes with a phase
// other than Conflict. In order:
//
//  1. N is selected by self. Held by self: it stays, and a Draining or
//     Releasing entry goes back to Active (or Pending without a service); its
//     service stays disabled, since the satellite never enables one. Only when
//     an older non-deleting rival holds it Pending or Active too, which a pass
//     that read the rival's status before its write landed leaves behind, does
//     self yield: Conflict, with the service left to the rival as on a
//     handover. Else held by a rival: Conflict. Else selected by an older
//     non-deleting rival (creationTimestamp, then name): Conflict. Else self
//     takes it, Pending until the Services step finds its service.
//  2. N is held by self but not selected, gone, or self is deleting. A
//     non-deleting rival that selects it takes it over: Releasing, with no
//     disable. Otherwise Draining; an entry already Releasing stays Releasing.
//  3. A Conflict entry that is no longer selected is dropped.
func planNovaComputeNodes(in nodePlanInput) nodePlan {
	prev := map[string]novav1alpha1.NovaComputeNodeStatus{}
	for _, e := range in.self.Status.Nodes {
		prev[e.Name] = e
	}

	plan := nodePlan{handover: map[string]bool{}}
	selectedNames := map[string]bool{}

	// The rivals' holdings and selectors are indexed once, so the rules stay
	// linear in the size of the pools.
	holds := rivalHolds(in.rivals)
	selectors := make([]labels.Selector, len(in.rivals))
	for i := range in.rivals {
		selectors[i] = poolSelector(&in.rivals[i])
	}

	for i := range in.selected {
		node := &in.selected[i]
		selectedNames[node.Name] = true
		entry, known := prev[node.Name]
		if !known {
			entry = novav1alpha1.NovaComputeNodeStatus{Name: node.Name}
		}
		entry.Zone = node.Labels[zoneLabel]
		entry.Instances = nil

		selfHolds := known && entry.Phase != novav1alpha1.NovaComputeNodeConflict
		holder := ""
		if h, ok := holds[node.Name]; ok {
			if !selfHolds || h.outranks(in.self) {
				holder = h.rival.Name
			}
		} else if !selfHolds {
			holder = olderSelectingRival(in.self, in.rivals, selectors, labels.Set(node.Labels))
		}

		switch {
		case holder != "":
			if entry.Phase != novav1alpha1.NovaComputeNodeConflict || entry.ConflictsWith != holder {
				plan.newConflicts = append(plan.newConflicts, nodeConflict{node: node.Name, rival: holder})
			}
			entry = novav1alpha1.NovaComputeNodeStatus{
				Name: node.Name, Phase: novav1alpha1.NovaComputeNodeConflict, Zone: entry.Zone, ConflictsWith: holder,
			}
		case selfHolds:
			if entry.Phase == novav1alpha1.NovaComputeNodeDraining || entry.Phase == novav1alpha1.NovaComputeNodeReleasing {
				entry.Phase = settledPhase(entry)
			}
		default:
			entry.ConflictsWith = ""
			entry.Phase = settledPhase(entry)
		}
		plan.nodes = append(plan.nodes, entry)
	}

	for _, entry := range in.self.Status.Nodes {
		if selectedNames[entry.Name] || entry.Phase == novav1alpha1.NovaComputeNodeConflict {
			continue
		}
		node := in.others[entry.Name]
		if node != nil {
			entry.Zone = node.Labels[zoneLabel]
		}
		switch {
		case node != nil && nonDeletingRivalSelects(in.rivals, selectors, labels.Set(node.Labels)):
			entry.Phase = novav1alpha1.NovaComputeNodeReleasing
			entry.Instances = nil
			plan.handover[entry.Name] = true
		case entry.Phase == novav1alpha1.NovaComputeNodeReleasing:
		default:
			entry.Phase = novav1alpha1.NovaComputeNodeDraining
		}
		plan.nodes = append(plan.nodes, entry)
	}

	slices.SortFunc(plan.nodes, func(a, b novav1alpha1.NovaComputeNodeStatus) int { return strings.Compare(a.Name, b.Name) })
	for _, e := range plan.nodes {
		switch e.Phase {
		case novav1alpha1.NovaComputeNodeConflict:
			plan.excluded = append(plan.excluded, e.Name)
		case novav1alpha1.NovaComputeNodeDraining:
			plan.keepPods = append(plan.keepPods, e.Name)
		case novav1alpha1.NovaComputeNodePending, novav1alpha1.NovaComputeNodeActive,
			novav1alpha1.NovaComputeNodeReleasing:
			// The selector term covers the first two; a Releasing node's pod is
			// released.
		}
	}
	return plan
}

// settledPhase is the phase of a node the pool holds and selects: Active once
// a service is known, Pending until then.
func settledPhase(entry novav1alpha1.NovaComputeNodeStatus) novav1alpha1.NovaComputeNodePhase {
	if entry.ServiceID != "" {
		return novav1alpha1.NovaComputeNodeActive
	}
	return novav1alpha1.NovaComputeNodePending
}

// rivalHold is a node a rival holds, and the phase it holds it in.
type rivalHold struct {
	rival *novav1alpha1.NovaCompute
	phase novav1alpha1.NovaComputeNodePhase
}

// outranks reports whether self, which holds the node too, yields it to the
// rival: the rival is older, not being deleted, and holds the node Pending or
// Active rather than on its way out.
func (h rivalHold) outranks(self *novav1alpha1.NovaCompute) bool {
	return h.rival.DeletionTimestamp.IsZero() && olderThan(h.rival, self) &&
		(h.phase == novav1alpha1.NovaComputeNodePending || h.phase == novav1alpha1.NovaComputeNodeActive)
}

// rivalHolds indexes the nodes the rivals hold by name. A node two rivals hold
// maps to the older of them.
func rivalHolds(rivals []novav1alpha1.NovaCompute) map[string]rivalHold {
	holds := map[string]rivalHold{}
	for i := range rivals {
		rival := &rivals[i]
		for _, e := range rival.Status.Nodes {
			if e.Phase == novav1alpha1.NovaComputeNodeConflict {
				continue
			}
			if h, ok := holds[e.Name]; ok && olderThan(h.rival, rival) {
				continue
			}
			holds[e.Name] = rivalHold{rival: rival, phase: e.Phase}
		}
	}
	return holds
}

// olderSelectingRival returns the name of the oldest non-deleting rival that
// is older than self and selects a node with nodeLabels, or "". selectors are
// the rivals' own, index for index.
func olderSelectingRival(self *novav1alpha1.NovaCompute, rivals []novav1alpha1.NovaCompute,
	selectors []labels.Selector, nodeLabels labels.Set,
) string {
	var oldestRival *novav1alpha1.NovaCompute
	for i := range rivals {
		rival := &rivals[i]
		if !rival.DeletionTimestamp.IsZero() || !selectors[i].Matches(nodeLabels) || !olderThan(rival, self) {
			continue
		}
		if oldestRival == nil || olderThan(rival, oldestRival) {
			oldestRival = rival
		}
	}
	if oldestRival == nil {
		return ""
	}
	return oldestRival.Name
}

// nonDeletingRivalSelects reports whether a rival that is not being deleted
// selects a node with nodeLabels. selectors are the rivals' own, index for
// index.
func nonDeletingRivalSelects(rivals []novav1alpha1.NovaCompute, selectors []labels.Selector,
	nodeLabels labels.Set,
) bool {
	for i := range rivals {
		if rivals[i].DeletionTimestamp.IsZero() && selectors[i].Matches(nodeLabels) {
			return true
		}
	}
	return false
}

// poolSelector is cr's node selector. An empty one selects nothing.
func poolSelector(cr *novav1alpha1.NovaCompute) labels.Selector {
	if len(cr.Spec.NodeSelector) == 0 {
		return labels.Nothing()
	}
	return labels.SelectorFromSet(cr.Spec.NodeSelector)
}

// olderThan orders two NovaComputes by creationTimestamp, then by name.
func olderThan(a, b *novav1alpha1.NovaCompute) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}
