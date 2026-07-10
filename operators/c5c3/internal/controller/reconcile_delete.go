// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// korcFinalizerPrefix is the common prefix of the finalizers K-ORC adds to the
// CRs it manages (e.g. "openstack.k-orc.cloud/applicationcredential"). The
// group prefix is stable across every K-ORC kind, so prefix-matching is what the
// stall escape uses to strip K-ORC's finalizers without hard-coding a suffix per
// kind. Non-K-ORC finalizers on the same object are preserved.
const korcFinalizerPrefix = orcv1alpha1.GroupName + "/"

// orcChildObject is one K-ORC CR the ControlPlane owns and must tear down first.
// newObj returns a zeroed object for Get/Delete.
type orcChildObject struct {
	newObj func() client.Object
	name   string
}

// key identifies a child within one teardown sweep, so the spec-derived and the
// ownership-derived lists can be merged without naming a CR twice (a duplicate
// would make forceRemoveKORCFinalizers Update the same object off two stale
// reads). The kind is part of the key because a Service and an Endpoint may share
// a name: an entry of type "image-public" and the "public" endpoint of entry
// "image" both render to "{cp}-catalog-image-public".
func (c orcChildObject) key() string {
	return fmt.Sprintf("%T/%s", c.newObj(), c.name)
}

// orcChildObjects is the spec-derived set of K-ORC CRs the ControlPlane owns.
// All the CRs live in childNamespace(cp). A name that never existed in the
// current mode is simply NotFound and is tolerated as already-gone.
//
// It is not the whole teardown set: an opt-in catalog entry whose declaration the
// spec dropped no longer appears here, yet its CRs may still exist. The sweep
// therefore drives orcTeardownChildren, which folds in whatever entry CRs the
// ControlPlane still owns.
//
// DELETION BLAST RADIUS. The sweep is correct for BOTH keystone modes, because
// what a Delete does to the external OpenStack installation is decided by each
// CR's ManagementPolicy, not by the ControlPlane's mode:
//
//   - ApplicationCredential — ManagementPolicyManaged. Its K-ORC finalizer revokes
//     the credential at the Keystone level BEFORE the CR delete returns, so
//     authenticating with it immediately afterwards yields 404 "Could not find
//     Application Credential" (not 401). This is the one identity object the
//     operator minted, so it is the one it destroys.
//   - User, Domain — ManagementPolicyUnmanaged imports (see ensureKORCAdminImports).
//     Deleting their CRs removes the Kubernetes objects and leaves the OpenStack
//     resources they imported untouched. K-ORC's deletion-guard finalizers also
//     enforce the teardown order: a User cannot go while an ApplicationCredential
//     still references it.
//   - Service, Endpoint — in Managed mode these are the managed catalog entries, so
//     the sweep deletes them from Keystone's catalog. In External mode the identity
//     Service and its per-interface Endpoints are ManagementPolicyUnmanaged imports
//     (see ensureExternalCatalogImports), so deleting them is a CR-only delete and
//     the external catalog is left bit-for-bit intact.
//   - The opt-in managed catalog entries (External mode only) are the ONE thing this
//     ControlPlane created in an external catalog, so they are the one thing it
//     removes from it — exactly mirroring the ApplicationCredential.
//
// That holds for a teardown K-ORC can complete. The stall escape in reconcileDelete
// is the deliberate exception: it strips the very finalizer that would have revoked
// the credential or removed the row, so every MANAGED CR it releases leaves its
// OpenStack resource behind with no Kubernetes object naming it. The alternative is
// a permanently wedged namespace, so the escape stays — and names what it orphaned.
//
// The OpenBao-backed Secrets are torn down by owner-reference GC, including the
// path behind the {name}-admin-app-credential-backup PushSecret: its
// DeletionPolicy is Delete (see adminAppCredentialPushSecret), so the credential
// this teardown revokes in Keystone does not outlive it in OpenBao. Nothing else
// is touched.
func orcChildObjects(cp *c5c3v1alpha1.ControlPlane) []orcChildObject {
	newService := func() client.Object { return &orcv1alpha1.Service{} }
	newEndpoint := func() client.Object { return &orcv1alpha1.Endpoint{} }

	objs := []orcChildObject{
		{func() client.Object { return &orcv1alpha1.ApplicationCredential{} }, adminAppCredentialName(cp)},
		{newService, keystoneServiceName(cp)},
		{newEndpoint, keystoneEndpointName(cp)},
		{func() client.Object { return &orcv1alpha1.User{} }, adminUserRef(cp)},
		{func() client.Object { return &orcv1alpha1.Domain{} }, adminDomainRef(cp)},
	}
	if !cp.IsExternalKeystone() {
		return objs
	}

	for _, iface := range externalCatalogInterfaces {
		objs = append(objs, orcChildObject{newEndpoint, keystoneEndpointImportName(cp, iface)})
	}
	for _, entry := range externalManagedCatalogEntries(cp) {
		objs = append(objs, orcChildObject{newService, catalogEntryServiceName(cp, entry.Type)})
		for _, ep := range entry.Endpoints {
			objs = append(objs, orcChildObject{newEndpoint, catalogEntryEndpointName(cp, entry.Type, ep.Interface)})
		}
	}
	return objs
}

// orcTeardownChildren is the set the sweep actually drives: the spec-derived
// children plus every opt-in catalog-entry CR the ControlPlane still OWNS.
//
// The two differ whenever the spec stopped declaring an entry whose CRs the
// reconcile-time prune never removed. The prune lives in reconcileCatalogExternal,
// which reconcileCatalog gates on AdminCredentialReady and which never runs once
// DeletionTimestamp is set — so a spec edit made while the admin password is
// drifted, or made moments before the delete, leaves entry CRs nobody names. Those
// CRs would then be released by the finalizer, garbage-collected behind K-ORC's
// openstack.k-orc.cloud/* finalizers, and left Terminating forever with no
// credentials Secret to authenticate against: the namespace wedge reconcileDelete
// exists to prevent, and one the stall escape can never repair because it only
// iterates what the sweep named.
func (r *ControlPlaneReconciler) orcTeardownChildren(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) ([]orcChildObject, error) {
	children := orcChildObjects(cp)
	if !cp.IsExternalKeystone() {
		return children, nil
	}

	owned, err := r.ownedCatalogEntryChildren(ctx, cp)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(children))
	for _, child := range children {
		seen[child.key()] = struct{}{}
	}
	for _, child := range owned {
		if _, dup := seen[child.key()]; dup {
			continue
		}
		seen[child.key()] = struct{}{}
		children = append(children, child)
	}
	return children, nil
}

// ownedCatalogEntryChildren lists the opt-in catalog-entry CRs this ControlPlane
// owns, whether or not the spec still declares them. Ownership is decided by
// ownsCatalogEntry — the controller reference AND the "{cp}-catalog-" name prefix,
// exactly as the reconcile-time prune scopes itself — so the unmanaged identity
// imports and any foreign CR sharing the namespace can never be caught by it.
//
// An absent K-ORC CRD (meta.IsNoMatchError) reads as "nothing to sweep", matching
// deleteORCResources, so the finalizer can still release when the K-ORC stack is
// gone.
func (r *ControlPlaneReconciler) ownedCatalogEntryChildren(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) ([]orcChildObject, error) {
	ns := childNamespace(cp)
	var out []orcChildObject

	var endpoints orcv1alpha1.EndpointList
	switch err := r.List(ctx, &endpoints, client.InNamespace(ns)); {
	case err == nil:
		for i := range endpoints.Items {
			if r.ownsCatalogEntry(cp, &endpoints.Items[i]) {
				out = append(out, orcChildObject{
					func() client.Object { return &orcv1alpha1.Endpoint{} }, endpoints.Items[i].Name,
				})
			}
		}
	case meta.IsNoMatchError(err):
		// The Endpoint CRD is gone: nothing to sweep, and nothing that could wedge.
	default:
		return nil, fmt.Errorf("listing catalog entry Endpoints for teardown: %w", err)
	}

	var services orcv1alpha1.ServiceList
	switch err := r.List(ctx, &services, client.InNamespace(ns)); {
	case err == nil:
		for i := range services.Items {
			if r.ownsCatalogEntry(cp, &services.Items[i]) {
				out = append(out, orcChildObject{
					func() client.Object { return &orcv1alpha1.Service{} }, services.Items[i].Name,
				})
			}
		}
	case meta.IsNoMatchError(err):
		// The Service CRD is gone: nothing to sweep, and nothing that could wedge.
	default:
		return nil, fmt.Errorf("listing catalog entry Services for teardown: %w", err)
	}

	return out, nil
}

// reconcileDelete drives the ORC-teardown finalizer when the ControlPlane CR is
// being deleted. It is a no-op if the finalizer is absent.
//
// The ControlPlane owns K-ORC CRs whose finalizers revoke/delete against the
// Keystone API. If the owner-reference GC cascade ran unsequenced, Keystone (and
// in managed mode its MariaDB) would be torn down at the same time as those ORC
// CRs, so the K-ORC finalizers could never complete and the ControlPlane /
// namespace would hang indefinitely on Terminating ORC CRs. Holding the
// ControlPlane CR in etcd (via this finalizer) defers the GC cascade, keeping
// Keystone reachable while K-ORC revokes. The flow:
//
//  1. Delete every owned K-ORC CR (idempotent; NotFound / CRD-absent tolerated)
//     and collect those still present (Terminating behind a K-ORC finalizer).
//  2. When none remain, release the finalizer so GC tears down the rest.
//  3. While some remain and the bounded orcTeardownStallTimeout has not elapsed,
//     report KORCReady=False/FinalizingORC and requeue.
//  4. Once the stall timeout elapses (K-ORC cannot make progress — most likely
//     Keystone is already gone, so it cannot revoke), force-remove the
//     openstack.k-orc.cloud/* finalizers, emit a Warning event, and release the
//     ControlPlane finalizer so deletion can complete. Every MANAGED CR released
//     that way orphans the OpenStack resource behind it, so a second Warning names
//     them: they are the only teardown outcome an operator has to repair by hand.
func (r *ControlPlaneReconciler) reconcileDelete(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cp, controlPlaneORCFinalizer) {
		return ctrl.Result{}, nil
	}

	remaining, hasLiveWork, err := r.deleteORCResources(ctx, cp)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Announce the teardown once, on the first pass where a live (not-yet-
	// Terminating) ORC CR is observed. Later requeues see only Terminating CRs
	// and suppress the event, giving exactly-once semantics per deletion.
	if hasLiveWork {
		r.Recorder.Event(cp, "Normal", "FinalizingORC",
			"Deleting owned K-ORC CRs before releasing the ControlPlane so K-ORC can revoke against a reachable Keystone")
	}

	if len(remaining) == 0 {
		// Every owned K-ORC CR is gone (revoked and deleted, or never existed).
		// Release the finalizer so GC tears down Keystone/MariaDB and the rest.
		r.Recorder.Event(cp, "Normal", "ORCTeardownComplete",
			"No remaining K-ORC CRs; releasing the ControlPlane finalizer")
		controllerutil.RemoveFinalizer(cp, controlPlaneORCFinalizer)
		if err := r.Update(ctx, cp); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// Some K-ORC CRs are still present — typically Terminating behind a K-ORC
	// finalizer that revokes against Keystone. Within the stall window, wait and
	// requeue; updateStatus persists the FinalizingORC condition so the wait is
	// operator-visible. The reason matches the FinalizingORC event above.
	if time.Since(cp.DeletionTimestamp.Time) <= orcTeardownStallTimeout {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKORCReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "FinalizingORC",
			Message: fmt.Sprintf(
				"waiting for %d K-ORC CR(s) to be revoked and deleted before releasing the ControlPlane",
				len(remaining),
			),
		})
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// Stall timeout elapsed: the K-ORC finalizers cannot complete (most likely
	// Keystone is already gone, so K-ORC cannot revoke). Force-remove the
	// openstack.k-orc.cloud/* finalizers so GC can reclaim the stuck CRs, warn so
	// the wedge is operator-visible, then release the ControlPlane finalizer.
	//
	// Classify BEFORE stripping. Releasing an Unmanaged import has zero blast radius:
	// deleting its CR never called OpenStack in the first place. A Managed CR is the
	// opposite — its K-ORC finalizer is what revokes the ApplicationCredential, or
	// takes an opt-in catalog row back out of a catalog this ControlPlane does not
	// own. Stripping it abandons that OpenStack resource, and once GC reclaims the CR
	// nothing in Kubernetes names it. A flat list of CR names reads as "K-ORC was
	// slow"; the operator has to be told which resources leaked, and where.
	names := make([]string, 0, len(remaining))
	var orphaned []string
	for _, obj := range remaining {
		names = append(names, obj.GetName())
		if isManagedORCChild(obj) {
			orphaned = append(orphaned, obj.GetName())
		}
	}

	if err := r.forceRemoveKORCFinalizers(ctx, remaining); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Event(cp, "Warning", "ORCTeardownStalled", fmt.Sprintf(
		"K-ORC CRs %v stayed Terminating longer than %s (K-ORC may be unable to reach Keystone to revoke); "+
			"force-removed their K-ORC finalizers and releasing the ControlPlane",
		names, orcTeardownStallTimeout,
	))
	if len(orphaned) > 0 {
		r.Recorder.Event(cp, "Warning", "ORCResourcesOrphaned", fmt.Sprintf(
			"the OpenStack resources behind the managed K-ORC CRs %v were NOT deleted: their finalizers were "+
				"force-removed before K-ORC could revoke the admin application credential or remove the "+
				"spec.services.keystone.external.catalog.managedEntries rows it registered. Nothing in "+
				"Kubernetes names them any more — remove them from Keystone by hand",
			orphaned,
		))
	}
	log.FromContext(ctx).Info("ORC teardown stalled; force-removed K-ORC finalizers",
		"remaining", names, "orphaned", orphaned, "stallTimeout", orcTeardownStallTimeout)

	controllerutil.RemoveFinalizer(cp, controlPlaneORCFinalizer)
	if err := r.Update(ctx, cp); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer after force-remove: %w", err)
	}
	return ctrl.Result{}, nil
}

// deleteORCResources issues an idempotent Delete on every owned K-ORC CR
// (orcTeardownChildren) and returns those still present after the sweep.
// hasLiveWork reports whether any CR was observed live (present,
// DeletionTimestamp unset) — that is the one-shot signal for the FinalizingORC
// event. NotFound and "CRD not installed" (meta.IsNoMatchError) are tolerated as
// already-gone so the finalizer can still release when the K-ORC stack is absent.
func (r *ControlPlaneReconciler) deleteORCResources(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) (remaining []client.Object, hasLiveWork bool, err error) {
	logger := log.FromContext(ctx)
	ns := childNamespace(cp)
	children, err := r.orcTeardownChildren(ctx, cp)
	if err != nil {
		return nil, false, err
	}

	// Pass 1: classify and delete. A present CR with no DeletionTimestamp is the
	// live work whose Delete starts the teardown.
	for _, child := range children {
		obj := child.newObj()
		key := client.ObjectKey{Name: child.name, Namespace: ns}
		getErr := r.Get(ctx, key, obj)
		if apierrors.IsNotFound(getErr) || meta.IsNoMatchError(getErr) {
			continue
		}
		if getErr != nil {
			return nil, false, fmt.Errorf("getting %T %s: %w", obj, key, getErr)
		}
		if obj.GetDeletionTimestamp().IsZero() {
			hasLiveWork = true
		}
		if delErr := r.Delete(ctx, obj); delErr != nil {
			if apierrors.IsNotFound(delErr) || meta.IsNoMatchError(delErr) {
				continue
			}
			return nil, false, fmt.Errorf("deleting %T %s: %w", obj, key, delErr)
		}
	}

	// Pass 2: collect the CRs still present after the deletes. Re-Get so the
	// returned objects carry current finalizers for a possible force-remove.
	for _, child := range children {
		obj := child.newObj()
		key := client.ObjectKey{Name: child.name, Namespace: ns}
		getErr := r.Get(ctx, key, obj)
		if apierrors.IsNotFound(getErr) || meta.IsNoMatchError(getErr) {
			continue
		}
		if getErr != nil {
			return nil, false, fmt.Errorf("re-checking %T %s: %w", obj, key, getErr)
		}
		remaining = append(remaining, obj)
		logger.V(1).Info("K-ORC CR still present during ControlPlane teardown",
			"resource", fmt.Sprintf("%T", obj), "name", key.Name)
	}

	return remaining, hasLiveWork, nil
}

// isManagedORCChild reports whether force-removing obj's K-ORC finalizer abandons
// the OpenStack resource behind it. It answers by ManagementPolicy — the field that
// decides what a Delete does to the external installation, see orcChildObjects — and
// it fails LOUD: anything not explicitly Unmanaged counts as managed, so an unset
// policy (K-ORC defaults it to `managed`) or a kind added later is reported as a leak
// rather than silently omitted from the warning.
func isManagedORCChild(obj client.Object) bool {
	switch o := obj.(type) {
	case *orcv1alpha1.ApplicationCredential:
		return o.Spec.ManagementPolicy != orcv1alpha1.ManagementPolicyUnmanaged
	case *orcv1alpha1.Service:
		return o.Spec.ManagementPolicy != orcv1alpha1.ManagementPolicyUnmanaged
	case *orcv1alpha1.Endpoint:
		return o.Spec.ManagementPolicy != orcv1alpha1.ManagementPolicyUnmanaged
	case *orcv1alpha1.User:
		return o.Spec.ManagementPolicy != orcv1alpha1.ManagementPolicyUnmanaged
	case *orcv1alpha1.Domain:
		return o.Spec.ManagementPolicy != orcv1alpha1.ManagementPolicyUnmanaged
	default:
		return true
	}
}

// forceRemoveKORCFinalizers strips every openstack.k-orc.cloud/* finalizer from
// the given objects, preserving any non-K-ORC finalizers, and persists the
// change. Removing the last finalizer on an already-Terminating CR lets the API
// server complete its deletion. NotFound is tolerated (GC won the race).
func (r *ControlPlaneReconciler) forceRemoveKORCFinalizers(ctx context.Context, remaining []client.Object) error {
	for _, obj := range remaining {
		original := obj.GetFinalizers()
		kept := make([]string, 0, len(original))
		removed := false
		for _, f := range original {
			if strings.HasPrefix(f, korcFinalizerPrefix) {
				removed = true
				continue
			}
			kept = append(kept, f)
		}
		if !removed {
			continue
		}
		obj.SetFinalizers(kept)
		if err := r.Update(ctx, obj); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("force-removing K-ORC finalizers from %T %s: %w",
				obj, client.ObjectKeyFromObject(obj), err)
		}
	}
	return nil
}
