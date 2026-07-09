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

// orcChildResources is the single source of truth for the K-ORC CRs the
// ControlPlane owns and must tear down first. Both the deletion sweep
// (deleteORCResources) and the stall escape (forceRemoveKORCFinalizers, via the
// objects it returns) iterate this list, so adding a kind only requires one
// entry here. Each newObj returns a zeroed object for Get/Delete; name derives
// the deterministic per-ControlPlane CR name (all live in childNamespace(cp)).
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
//   - Service, Endpoint — managed catalog entries today, so the sweep deletes them
//     from Keystone's catalog. In External mode the catalog is owned by the
//     external installation; the import-first catalog work turns these into
//     unmanaged imports too, at which point this entry becomes a CR-only delete.
//
// The OpenBao-backed Secrets are torn down by owner-reference GC, EXCEPT the path
// behind the {name}-admin-app-credential-backup PushSecret: its DeletionPolicy is
// deliberately None (see adminAppCredentialPushSecret), so the last-pushed
// credential survives at its OpenBao path. Nothing else is touched.
var orcChildResources = []struct {
	newObj func() client.Object
	name   func(*c5c3v1alpha1.ControlPlane) string
}{
	{func() client.Object { return &orcv1alpha1.ApplicationCredential{} }, adminAppCredentialName},
	{func() client.Object { return &orcv1alpha1.Service{} }, keystoneServiceName},
	{func() client.Object { return &orcv1alpha1.Endpoint{} }, keystoneEndpointName},
	{func() client.Object { return &orcv1alpha1.User{} }, adminUserRef},
	{func() client.Object { return &orcv1alpha1.Domain{} }, adminDomainRef},
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
//     ControlPlane finalizer so deletion can complete.
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
	if err := r.forceRemoveKORCFinalizers(ctx, remaining); err != nil {
		return ctrl.Result{}, err
	}
	names := make([]string, 0, len(remaining))
	for _, obj := range remaining {
		names = append(names, obj.GetName())
	}
	r.Recorder.Event(cp, "Warning", "ORCTeardownStalled", fmt.Sprintf(
		"K-ORC CRs %v stayed Terminating longer than %s (K-ORC may be unable to reach Keystone to revoke); "+
			"force-removed their K-ORC finalizers and releasing the ControlPlane",
		names, orcTeardownStallTimeout,
	))
	log.FromContext(ctx).Info("ORC teardown stalled; force-removed K-ORC finalizers",
		"remaining", names, "stallTimeout", orcTeardownStallTimeout)

	controllerutil.RemoveFinalizer(cp, controlPlaneORCFinalizer)
	if err := r.Update(ctx, cp); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer after force-remove: %w", err)
	}
	return ctrl.Result{}, nil
}

// deleteORCResources issues an idempotent Delete on every owned K-ORC CR and
// returns those still present after the sweep. hasLiveWork reports whether any
// CR was observed live (present, DeletionTimestamp unset) — that is the one-shot
// signal for the FinalizingORC event. NotFound and "CRD not installed"
// (meta.IsNoMatchError) are tolerated as already-gone so the finalizer can still
// release when the K-ORC stack is absent.
func (r *ControlPlaneReconciler) deleteORCResources(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) (remaining []client.Object, hasLiveWork bool, err error) {
	logger := log.FromContext(ctx)
	ns := childNamespace(cp)

	// Pass 1: classify and delete. A present CR with no DeletionTimestamp is the
	// live work whose Delete starts the teardown.
	for _, child := range orcChildResources {
		obj := child.newObj()
		key := client.ObjectKey{Name: child.name(cp), Namespace: ns}
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
	for _, child := range orcChildResources {
		obj := child.newObj()
		key := client.ObjectKey{Name: child.name(cp), Namespace: ns}
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
