// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/apply"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/nova/internal/computeapi"
)

// The reasons of the TraitsReady condition. ComputeAPIError and
// NovaComputeListError are shared with AggregatesReady.
const (
	conditionReasonNoTraitsDeclared                = "NoTraitsDeclared"
	conditionReasonTraitsApplied                   = "TraitsApplied"
	conditionReasonWaitingForHypervisorOperator    = "WaitingForHypervisorOperator"
	conditionReasonNodesWithCustomTraitsAnnotation = "NodesWithCustomTraitsAnnotation"
	conditionReasonNodesWithoutHypervisor          = "NodesWithoutHypervisor"
	conditionReasonHypervisorLifecycleDisabled     = "HypervisorLifecycleDisabled"
	conditionReasonHypervisorTraitsFailed          = "HypervisorTraitsFailed"
	conditionReasonHypervisorOperatorNotInstalled  = "HypervisorOperatorNotInstalled"
	conditionReasonHypervisorsForbidden            = "HypervisorsForbidden"
	conditionReasonHypervisorError                 = "HypervisorError"
	conditionReasonPlacementEndpointNotFound       = "PlacementEndpointNotFound"
	conditionReasonPlacementAPIError               = "PlacementAPIError"
)

// hypervisorGVK is openstack-hypervisor-operator's cluster-scoped Hypervisor,
// one per Node labelled nova.openstack.cloud.sap/virt-driver and named after
// it. The step handles it unstructured, so no module of the operator is
// imported. The fields it reads and writes are those of the operator at
// a2baf3f, the commit images/openstack-hypervisor-operator pins.
var hypervisorGVK = schema.GroupVersionKind{Group: "kvm.cloud.sap", Version: "v1", Kind: "Hypervisor"}

// hypervisorConditionTraitsUpdated is the Hypervisor condition
// openstack-hypervisor-operator sets after it applied spec.customTraits to the
// node's resource provider, False with Placement's error when that failed.
const hypervisorConditionTraitsUpdated = "TraitsUpdated"

// customTraitPrefix starts every custom trait. openstack-hypervisor-operator
// applies only those; the virt driver reports the standard ones.
const customTraitPrefix = "CUSTOM_"

// hypervisorView is what the step reads from one Hypervisor.
type hypervisorView struct {
	// customTraits is spec.customTraits as stored.
	customTraits []string
	// lifecycleEnabled is spec.lifecycleEnabled; an absent field reads as the
	// CRD's default, true.
	lifecycleEnabled bool
	// observed is the CUSTOM_ subset of status.traits, sorted.
	observed []string
	// traitsUpdated is the TraitsUpdated condition, nil when absent.
	traitsUpdated *metav1.Condition
}

// reconcileNovaComputeTraits keeps the custom Placement traits of the pool's
// hosts, and sets TraitsReady.
//
// A node's desired list is spec.traits merged with its hostTraits entry: the
// union for an Append entry or none, the entry's traits alone for a Replace
// entry. The step manages the Pending and Active nodes whose desired list or
// recorded traits are not empty; a deleting pool manages none. For each one it
// reads the node's Hypervisor, creates in Placement every desired trait the
// node does not carry yet, and writes the desired list, sorted, into
// spec.customTraits, from where openstack-hypervisor-operator applies it. That
// operator stays the only writer of the provider's custom traits. The node's
// status entry records the CUSTOM_ traits its provider carries and the list
// written for it, so a node whose last trait the pool drops stays managed until
// its Hypervisor lists none. A node carrying customTraitsAnnotation is neither
// read nor written: the operator overwrites spec.customTraits from the
// annotation on every Node reconcile.
//
// A trait the pool created is deleted from Placement once no non-deleting
// NovaCompute of the Nova declares it and no NovaCompute of the Nova records it
// on a node. A trait some resource provider still carries is kept.
//
// Progress the hypervisor operator owns keeps TraitsReady True under
// WaitingForHypervisorOperator, as AggregatesReady does for the aggregate
// membership. The step polls every RequeueComputeDrainPolling while a node or a
// deletion waits and after any False reason, because the Node watch misses
// annotation edits and nothing watches Hypervisors. A pool that declares no
// trait and records none makes no call at all.
func (r *NovaComputeReconciler) reconcileNovaComputeTraits(ctx context.Context, children client.Client,
	cr *novav1alpha1.NovaCompute, pass *novaComputePass,
) (ctrl.Result, error) {
	desired, unknown := desiredNodeTraits(cr)
	managed := managedTraitNodes(cr, desired)
	if len(managed) == 0 && len(cr.Status.CreatedTraits) == 0 {
		conditions.SetCondition(&cr.Status.Conditions, noTraitsDeclaredCondition(cr))
		pass.traitsSettled = true
		return ctrl.Result{}, nil
	}

	var annotated, readable []string
	for _, name := range managed {
		if pass.customTraitsAnnotated[name] {
			annotated = append(annotated, name)
		} else {
			readable = append(readable, name)
		}
	}

	views, withoutHypervisor, result, err := r.readHypervisors(ctx, children, cr, readable)
	if !result.IsZero() || err != nil {
		return result, err
	}

	if missing := missingTraits(desired, views); len(missing) > 0 {
		api, err := placementAPI(ctx, pass)
		if err != nil {
			return placementUnavailable(cr, err)
		}
		for _, trait := range missing {
			created, err := api.EnsureTrait(ctx, trait)
			if err != nil {
				// A trait missing from Placement fails the hypervisor
				// operator's update, so no Hypervisor is written in this pass.
				return traitsFailed(cr, conditionReasonPlacementAPIError, fmt.Errorf("creating trait %s: %w", trait, err))
			}
			if created {
				addCreatedTrait(cr, trait)
				r.Recorder.Eventf(cr, corev1.EventTypeNormal, eventReasonTraitCreated, "Created Placement trait %s", trait)
			}
		}
	}

	var written []string
	for _, name := range readable {
		view, ok := views[name]
		if !ok {
			continue
		}
		if !slices.Equal(view.customTraits, desired[name]) {
			if err := patchHypervisorTraits(ctx, children, name, desired[name]); err != nil {
				if apierrors.IsNotFound(err) {
					withoutHypervisor = append(withoutHypervisor, name)
					continue
				}
				return hypervisorFailed(cr, err)
			}
		}
		setNodeTraits(cr, name, sortedUnion(view.observed, desired[name]))
		written = append(written, name)
	}

	waitingDeletions, result, err := r.deleteUnusedTraits(ctx, cr, pass)
	if !result.IsZero() || err != nil {
		return result, err
	}

	condition := traitsCondition(cr, traitsOutcome{
		managed: managed, annotated: annotated, withoutHypervisor: withoutHypervisor,
		written: written, views: views, desired: desired, unknown: unknown, waitingDeletions: waitingDeletions,
	})
	conditions.SetCondition(&cr.Status.Conditions, condition)
	pass.traitsSettled = len(cr.Status.CreatedTraits) == 0
	if condition.Status == metav1.ConditionFalse || condition.Reason == conditionReasonWaitingForHypervisorOperator ||
		len(waitingDeletions) > 0 {
		return ctrl.Result{RequeueAfter: RequeueComputeDrainPolling}, nil
	}
	return ctrl.Result{}, nil
}

// readHypervisors reads the Hypervisor of each named node through the uncached
// reader of the pool's cluster, one Get by name each. It returns the views by
// node name and the nodes without a Hypervisor. A non-zero result or an error
// is a pass-level failure already set on TraitsReady, which the caller returns.
func (r *NovaComputeReconciler) readHypervisors(ctx context.Context, children client.Client,
	cr *novav1alpha1.NovaCompute, names []string,
) (map[string]hypervisorView, []string, ctrl.Result, error) {
	views := make(map[string]hypervisorView, len(names))
	if len(names) == 0 {
		return views, nil, ctrl.Result{}, nil
	}
	reader, err := commonmulticluster.ResolveChildrenAPIReader(ctx, r.Resolver, r.APIReader, cr.Spec.TargetClusterRef)
	if err != nil {
		result, err := traitsFailed(cr, commonmulticluster.TargetClusterUnavailable, err)
		return nil, nil, result, err
	}
	if reader == nil {
		reader = children
	}

	var withoutHypervisor []string
	for _, name := range names {
		view, err := readHypervisor(ctx, reader, name)
		if apierrors.IsNotFound(err) {
			withoutHypervisor = append(withoutHypervisor, name)
			continue
		}
		if err != nil {
			result, err := hypervisorFailed(cr, err)
			return nil, nil, result, err
		}
		views[name] = view
	}
	return views, withoutHypervisor, ctrl.Result{}, nil
}

// readHypervisor gets the Hypervisor named after the node.
func readHypervisor(ctx context.Context, reader client.Reader, name string) (hypervisorView, error) {
	hv := &unstructured.Unstructured{}
	hv.SetGroupVersionKind(hypervisorGVK)
	if err := reader.Get(ctx, types.NamespacedName{Name: name}, hv); err != nil {
		return hypervisorView{}, fmt.Errorf("getting Hypervisor %s: %w", name, err)
	}

	var fields struct {
		Spec struct {
			CustomTraits     []string `json:"customTraits"`
			LifecycleEnabled *bool    `json:"lifecycleEnabled"`
		} `json:"spec"`
		Status struct {
			Traits     []string           `json:"traits"`
			Conditions []metav1.Condition `json:"conditions"`
		} `json:"status"`
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(hv.Object, &fields); err != nil {
		return hypervisorView{}, fmt.Errorf("decoding Hypervisor %s: %w", name, err)
	}
	return hypervisorView{
		customTraits:     fields.Spec.CustomTraits,
		lifecycleEnabled: fields.Spec.LifecycleEnabled == nil || *fields.Spec.LifecycleEnabled,
		observed:         customTraitSubset(fields.Status.Traits),
		traitsUpdated:    meta.FindStatusCondition(fields.Status.Conditions, hypervisorConditionTraitsUpdated),
	}, nil
}

// patchHypervisorTraits replaces the Hypervisor's spec.customTraits with
// traits through a JSON merge patch, an empty list as []. It is a merge patch
// rather than a server-side apply: openstack-hypervisor-operator overwrites
// the whole list from customTraitsAnnotation, and the step skips the nodes that
// carry it, so the pool is the field's only writer and replaces it whole.
func patchHypervisorTraits(ctx context.Context, children client.Client, name string, traits []string) error {
	if traits == nil {
		traits = []string{}
	}
	body, err := json.Marshal(map[string]any{"spec": map[string]any{"customTraits": traits}})
	if err != nil {
		return fmt.Errorf("marshaling the custom traits of Hypervisor %s: %w", name, err)
	}
	hv := &unstructured.Unstructured{}
	hv.SetGroupVersionKind(hypervisorGVK)
	hv.SetName(name)
	if err := children.Patch(ctx, hv, client.RawPatch(types.MergePatchType, body),
		client.FieldOwner(apply.FieldManager)); err != nil {
		return fmt.Errorf("patching the custom traits of Hypervisor %s: %w", name, err)
	}
	return nil
}

// hypervisorFailed maps a failed Hypervisor read or patch other than a 404 onto
// TraitsReady. A cluster without the Hypervisor kind and a missing grant only
// change from outside, so they wait; anything else is returned.
func hypervisorFailed(cr *novav1alpha1.NovaCompute, err error) (ctrl.Result, error) {
	switch {
	case meta.IsNoMatchError(err):
		return traitsFailed(cr, conditionReasonHypervisorOperatorNotInstalled, fmt.Errorf(
			"the pool's cluster serves no hypervisors.kvm.cloud.sap, so openstack-hypervisor-operator is not installed: %w", err))
	case apierrors.IsForbidden(err):
		return traitsFailed(cr, conditionReasonHypervisorsForbidden, fmt.Errorf(
			"%w; a placed pool needs hypervisorTraits: true in the target-cluster-access chart, a local pool the "+
				"nova-operator chart's ClusterRole, which rbac.namespaceScoped=true does not grant", err))
	}
	novaComputeSkeleton.MarkFailed(cr, conditionTypeTraitsReady, conditionReasonHypervisorError, err)
	return ctrl.Result{}, err
}

// placementAPI returns the pass's API client pointed at the Placement endpoint
// the Nova's own [placement] section resolves: the Nova's override, else the
// catalog's internal endpoint in the Nova's region. The client and its token
// are built on the pass's first call, so the create and the delete share them
// and a pass that needs neither makes no call.
func placementAPI(ctx context.Context, pass *novaComputePass) (*computeapi.Client, error) {
	api, err := pass.computeClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("authenticating: %w", err)
	}
	if err := api.UsePlacement(pass.nova.Spec.Endpoints.Placement.Override, pass.nova.Spec.Region); err != nil {
		return nil, err
	}
	return api, nil
}

// placementUnavailable maps a failed placementAPI onto TraitsReady.
func placementUnavailable(cr *novav1alpha1.NovaCompute, err error) (ctrl.Result, error) {
	if errors.Is(err, computeapi.ErrPlacementEndpointNotFound) {
		return traitsFailed(cr, conditionReasonPlacementEndpointNotFound, err)
	}
	return computeAPIFailed(cr, conditionTypeTraitsReady, err)
}

// traitsFailed sets TraitsReady False for a pass-level failure and polls again
// without an error, so the retry keeps its pace.
func traitsFailed(cr *novav1alpha1.NovaCompute, reason string, err error) (ctrl.Result, error) {
	novaComputeSkeleton.MarkFailed(cr, conditionTypeTraitsReady, reason, err)
	return ctrl.Result{RequeueAfter: RequeueComputeDrainPolling}, nil
}

// deleteUnusedTraits deletes the traits this pool created that no NovaCompute
// of the Nova needs any more, and drops them from status.createdTraits. A
// candidate is a created trait no non-deleting NovaCompute of the Nova declares;
// it waits while some NovaCompute of the Nova, this one and the deleting ones
// included, records it on a node. A deleting pool drops a created trait a
// non-deleting sibling still declares without deleting it, so its teardown
// does not wait on that sibling. It returns the waiting candidates, and a
// non-zero result or an error for a pass-level failure already set on
// TraitsReady.
func (r *NovaComputeReconciler) deleteUnusedTraits(ctx context.Context, cr *novav1alpha1.NovaCompute,
	pass *novaComputePass,
) ([]string, ctrl.Result, error) {
	if len(cr.Status.CreatedTraits) == 0 {
		return nil, ctrl.Result{}, nil
	}
	siblings, err := r.siblingsOf(ctx, cr, pass, conditionTypeTraitsReady)
	if err != nil {
		return nil, ctrl.Result{}, err
	}

	deleting := !cr.DeletionTimestamp.IsZero()
	recorded := recordedTraits(cr)
	declaredBy := map[string]string{}
	if !deleting {
		for trait := range declaredTraits(cr) {
			declaredBy[trait] = cr.Name
		}
	}
	for i := range siblings {
		sibling := &siblings[i]
		if sibling.Name == cr.Name {
			continue
		}
		for trait := range recordedTraits(sibling) {
			recorded[trait] = true
		}
		if !sibling.DeletionTimestamp.IsZero() {
			continue
		}
		for trait := range declaredTraits(sibling) {
			if _, ok := declaredBy[trait]; !ok {
				declaredBy[trait] = sibling.Name
			}
		}
	}

	created := slices.Clone(cr.Status.CreatedTraits)
	slices.Sort(created)
	var kept []string
	var waiting []string
	var api *computeapi.Client
	for i, trait := range created {
		if owner, ok := declaredBy[trait]; ok {
			if deleting {
				r.Recorder.Eventf(cr, corev1.EventTypeNormal, eventReasonTraitKept,
					"Kept Placement trait %s: NovaCompute %s of Nova %s declares it", trait, owner, cr.Spec.NovaRef.Name)
				continue
			}
			kept = append(kept, trait)
			continue
		}
		if recorded[trait] {
			kept = append(kept, trait)
			waiting = append(waiting, trait)
			continue
		}
		if api == nil {
			if api, err = placementAPI(ctx, pass); err != nil {
				cr.Status.CreatedTraits = slices.Concat(kept, created[i:])
				result, err := placementUnavailable(cr, err)
				return nil, result, err
			}
		}
		err := api.DeleteTrait(ctx, trait)
		switch {
		case err == nil:
			r.Recorder.Eventf(cr, corev1.EventTypeNormal, eventReasonTraitDeleted,
				"Deleted Placement trait %s: no NovaCompute of Nova %s declares or carries it", trait, cr.Spec.NovaRef.Name)
		case errors.Is(err, computeapi.ErrTraitInUse):
			r.Recorder.Eventf(cr, corev1.EventTypeNormal, eventReasonTraitKept,
				"Kept Placement trait %s: a resource provider no pool of Nova %s manages carries it", trait, cr.Spec.NovaRef.Name)
		default:
			cr.Status.CreatedTraits = slices.Concat(kept, created[i:])
			result, err := traitsFailed(cr, conditionReasonPlacementAPIError, fmt.Errorf("deleting trait %s: %w", trait, err))
			return nil, result, err
		}
	}
	cr.Status.CreatedTraits = kept
	return waiting, ctrl.Result{}, nil
}

// traitsOutcome is what the step learned in a pass that completed.
type traitsOutcome struct {
	// managed, annotated and withoutHypervisor are node names, as are written,
	// the managed nodes whose Hypervisor was read and written.
	managed, annotated, withoutHypervisor, written []string
	views                                          map[string]hypervisorView
	desired                                        map[string][]string
	// unknown are the hostTraits entries naming no node the pool holds, and
	// waitingDeletions the created traits that wait for their last carrier.
	unknown, waitingDeletions []string
}

// traitsCondition derives TraitsReady from a pass that completed; the first
// match wins.
func traitsCondition(cr *novav1alpha1.NovaCompute, o traitsOutcome) metav1.Condition {
	var lifecycleDisabled, waiting []string
	var failed []string
	failedMessage := ""
	for _, name := range o.written {
		view := o.views[name]
		if !view.lifecycleEnabled {
			lifecycleDisabled = append(lifecycleDisabled, name)
		}
		if view.traitsUpdated != nil && view.traitsUpdated.Status == metav1.ConditionFalse {
			if failed == nil {
				failedMessage = view.traitsUpdated.Message
			}
			failed = append(failed, name)
		}
		if !slices.Equal(view.observed, o.desired[name]) {
			waiting = append(waiting, name)
		}
	}
	slices.Sort(o.withoutHypervisor)

	condition := metav1.Condition{Type: conditionTypeTraitsReady, ObservedGeneration: cr.Generation}
	switch {
	case len(o.annotated) > 0:
		condition.Status = metav1.ConditionFalse
		condition.Reason = conditionReasonNodesWithCustomTraitsAnnotation
		condition.Message = "Nodes carrying the annotation " + customTraitsAnnotation +
			", from which openstack-hypervisor-operator overwrites the pool's list: " + namedNodes(o.annotated)
	case len(o.withoutHypervisor) > 0:
		condition.Status = metav1.ConditionFalse
		condition.Reason = conditionReasonNodesWithoutHypervisor
		condition.Message = "Nodes without a Hypervisor: " + namedNodes(o.withoutHypervisor)
	case len(lifecycleDisabled) > 0:
		condition.Status = metav1.ConditionFalse
		condition.Reason = conditionReasonHypervisorLifecycleDisabled
		condition.Message = "Hypervisors with spec.lifecycleEnabled false, on which openstack-hypervisor-operator " +
			"applies no trait: " + namedNodes(lifecycleDisabled)
	case len(failed) > 0:
		condition.Status = metav1.ConditionFalse
		condition.Reason = conditionReasonHypervisorTraitsFailed
		condition.Message = fmt.Sprintf("openstack-hypervisor-operator could not apply the traits on %s: %s",
			namedNodes(failed), failedMessage)
	case len(waiting) > 0:
		condition.Status = metav1.ConditionTrue
		condition.Reason = conditionReasonWaitingForHypervisorOperator
		condition.Message = "Waiting for openstack-hypervisor-operator to apply the custom traits on " +
			namedNodes(waiting)
	case noTraitsLeft(cr, o.desired):
		condition = noTraitsDeclaredCondition(cr)
	default:
		condition.Status = metav1.ConditionTrue
		condition.Reason = conditionReasonTraitsApplied
		condition.Message = fmt.Sprintf("Every managed node carries its custom traits (%d nodes)", len(o.managed))
		if len(o.waitingDeletions) > 0 {
			condition.Message += "; created traits waiting for their last carrier before deletion: " +
				strings.Join(o.waitingDeletions, ", ")
		}
		if len(o.unknown) > 0 {
			condition.Message += "; hostTraits entries naming no node of the pool: " + namedNodes(o.unknown)
		}
	}
	return condition
}

// noTraitsLeft reports whether no Pending or Active node desires or records a
// trait and the pool holds no created trait.
func noTraitsLeft(cr *novav1alpha1.NovaCompute, desired map[string][]string) bool {
	if len(cr.Status.CreatedTraits) > 0 {
		return false
	}
	for _, entry := range cr.Status.Nodes {
		if nodeCarriesTraits(entry, desired) {
			return false
		}
	}
	return true
}

// noTraitsDeclaredCondition is TraitsReady for a pool of which no node desires
// or records a trait and that holds no created trait.
func noTraitsDeclaredCondition(cr *novav1alpha1.NovaCompute) metav1.Condition {
	return metav1.Condition{
		Type: conditionTypeTraitsReady, Status: metav1.ConditionTrue, ObservedGeneration: cr.Generation,
		Reason: conditionReasonNoTraitsDeclared, Message: "No custom trait is declared or carried by the pool",
	}
}

// nodeCarriesTraits reports whether a Pending or Active node desires or records
// a trait.
func nodeCarriesTraits(entry novav1alpha1.NovaComputeNodeStatus, desired map[string][]string) bool {
	want, ok := desired[entry.Name]
	return ok && (len(want) > 0 || len(entry.Traits) > 0)
}

// desiredNodeTraits returns the sorted, deduplicated desired list of every
// Pending and Active node of the pool, an empty list rather than nil for a node
// that desires none, and the sorted hostTraits node names that match no node
// the pool holds. A node is held when its entry is not in Conflict.
func desiredNodeTraits(cr *novav1alpha1.NovaCompute) (map[string][]string, []string) {
	entries := make(map[string]novav1alpha1.NovaComputeHostTraits, len(cr.Spec.HostTraits))
	for _, entry := range cr.Spec.HostTraits {
		entries[entry.NodeName] = entry
	}

	desired := map[string][]string{}
	held := map[string]bool{}
	for _, node := range cr.Status.Nodes {
		if node.Phase != novav1alpha1.NovaComputeNodeConflict {
			held[node.Name] = true
		}
		if node.Phase != novav1alpha1.NovaComputeNodePending && node.Phase != novav1alpha1.NovaComputeNodeActive {
			continue
		}
		pool := cr.Spec.Traits
		entry := entries[node.Name]
		if entry.Mode == novav1alpha1.NovaComputeHostTraitsReplace {
			pool = nil
		}
		desired[node.Name] = sortedUnion(pool, entry.Traits)
	}

	var unknown []string
	for _, entry := range cr.Spec.HostTraits {
		if !held[entry.NodeName] {
			unknown = append(unknown, entry.NodeName)
		}
	}
	slices.Sort(unknown)
	return desired, unknown
}

// managedTraitNodes returns the Pending and Active nodes whose desired list or
// recorded traits are not empty, in status order. A deleting pool manages
// none.
func managedTraitNodes(cr *novav1alpha1.NovaCompute, desired map[string][]string) []string {
	if !cr.DeletionTimestamp.IsZero() {
		return nil
	}
	var managed []string
	for _, entry := range cr.Status.Nodes {
		if nodeCarriesTraits(entry, desired) {
			managed = append(managed, entry.Name)
		}
	}
	return managed
}

// missingTraits returns, sorted, every trait a read node desires but does not
// carry yet.
func missingTraits(desired map[string][]string, views map[string]hypervisorView) []string {
	var missing []string
	for name, view := range views {
		for _, trait := range desired[name] {
			if !slices.Contains(view.observed, trait) {
				missing = append(missing, trait)
			}
		}
	}
	slices.Sort(missing)
	return slices.Compact(missing)
}

// customTraitSubset returns the CUSTOM_ entries of traits, sorted and
// deduplicated.
func customTraitSubset(traits []string) []string {
	var custom []string
	for _, trait := range traits {
		if strings.HasPrefix(trait, customTraitPrefix) {
			custom = append(custom, trait)
		}
	}
	slices.Sort(custom)
	return slices.Compact(custom)
}

// sortedUnion returns the union of a and b, sorted ascending in byte order, the
// order in which openstack-hypervisor-operator stores status.traits and
// compares the lists, and never nil.
func sortedUnion(a, b []string) []string {
	union := slices.Concat(a, b)
	slices.Sort(union)
	union = slices.Compact(union)
	if union == nil {
		union = []string{}
	}
	return union
}

// setNodeTraits records traits on the node's status entry.
func setNodeTraits(cr *novav1alpha1.NovaCompute, name string, traits []string) {
	for i := range cr.Status.Nodes {
		if cr.Status.Nodes[i].Name != name {
			continue
		}
		cr.Status.Nodes[i].Traits = nil
		if len(traits) > 0 {
			cr.Status.Nodes[i].Traits = traits
		}
	}
}

// addCreatedTrait records a trait the pool created, keeping the list sorted.
func addCreatedTrait(cr *novav1alpha1.NovaCompute, trait string) {
	if !slices.Contains(cr.Status.CreatedTraits, trait) {
		cr.Status.CreatedTraits = append(cr.Status.CreatedTraits, trait)
		slices.Sort(cr.Status.CreatedTraits)
	}
}

// declaredTraits is the set of traits nc declares in spec.traits or in any
// hostTraits entry.
func declaredTraits(nc *novav1alpha1.NovaCompute) map[string]bool {
	declared := map[string]bool{}
	for _, trait := range nc.Spec.Traits {
		declared[trait] = true
	}
	for _, entry := range nc.Spec.HostTraits {
		for _, trait := range entry.Traits {
			declared[trait] = true
		}
	}
	return declared
}

// recordedTraits is the set of traits nc records on any of its nodes.
func recordedTraits(nc *novav1alpha1.NovaCompute) map[string]bool {
	recorded := map[string]bool{}
	for _, entry := range nc.Status.Nodes {
		for _, trait := range entry.Traits {
			recorded[trait] = true
		}
	}
	return recorded
}
