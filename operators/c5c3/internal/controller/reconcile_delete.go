// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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
//     still references it. Note that K-ORC cannot RUN those finalizers once the
//     credential the imports authenticate with is revoked (it re-fetches the
//     imported resource through an authenticated actuator before releasing any
//     finalizer), which is why reconcileDelete force-releases an unmanaged-only
//     remainder instead of waiting for K-ORC.
//   - Service, Endpoint — in Managed mode these are the managed catalog entries, so
//     the sweep deletes them from Keystone's catalog. In External mode the identity
//     Service and its per-interface Endpoints are ManagementPolicyUnmanaged imports
//     (see ensureExternalCatalogImports), so deleting them is a CR-only delete and
//     the external catalog is left bit-for-bit intact.
//   - The opt-in managed catalog entries (External mode only) are the ONE thing this
//     ControlPlane created in an external catalog, so they are the one thing it
//     removes from it — exactly mirroring the ApplicationCredential.
//   - The declarative service accounts (both modes): a managed User/Project (one the
//     operator CREATED, or one it ADOPTED — adoption makes it operator-owned) is
//     ManagementPolicyManaged, so its K-ORC finalizer DELETES it from Keystone at
//     teardown. This is the declared-ownership mirror of the opt-in catalog entries:
//     the operator destroys exactly what it owns. The collision-probe imports, the
//     per-account domain imports, and a create:false REFERENCED project are all
//     ManagementPolicyUnmanaged, so deleting their CRs is a CR-only delete that
//     leaves the external resource untouched.
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
	newUser := func() client.Object { return &orcv1alpha1.User{} }
	newProject := func() client.Object { return &orcv1alpha1.Project{} }
	newDomain := func() client.Object { return &orcv1alpha1.Domain{} }

	objs := []orcChildObject{
		{func() client.Object { return &orcv1alpha1.ApplicationCredential{} }, adminAppCredentialName(cp)},
		{newService, keystoneServiceName(cp)},
		{newEndpoint, keystoneEndpointName(cp)},
		{newUser, adminUserRef(cp)},
		{newDomain, adminDomainRef(cp)},
	}

	// Declarative service accounts are mode-independent, so their children are torn
	// down in BOTH keystone modes (before the External-only catalog additions). Each
	// declared entry projects a managed User and Project (whose K-ORC finalizers
	// delete them from Keystone) plus the collision-probe and per-account domain
	// imports (CR-only deletes). The password Secrets / PushSecret / ExternalSecret
	// are owner-reference-GC'd, not K-ORC CRs, so they are not part of this sweep.
	for i := range cp.Spec.KORC.ServiceAccounts {
		sa := cp.Spec.KORC.ServiceAccounts[i]
		objs = append(
			objs,
			orcChildObject{newUser, serviceAccountUserRef(cp, sa)},
			orcChildObject{newUser, serviceAccountUserProbeRef(cp, sa)},
			orcChildObject{newProject, serviceAccountProjectRef(cp, sa)},
			orcChildObject{newProject, serviceAccountProjectProbeRef(cp, sa)},
		)
		if domainRef := serviceAccountDomainRef(cp, sa); domainRef != adminDomainRef(cp) {
			objs = append(objs, orcChildObject{newDomain, domainRef})
		}
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
	seen := make(map[string]struct{}, len(children))
	for _, child := range children {
		seen[child.key()] = struct{}{}
	}
	merge := func(extra []orcChildObject) {
		for _, child := range extra {
			if _, dup := seen[child.key()]; dup {
				continue
			}
			seen[child.key()] = struct{}{}
			children = append(children, child)
		}
	}

	// Service accounts are mode-independent, so their owned-but-undeclared children
	// are folded in for both modes (a spec edit made moments before the delete can
	// leave a User/Project the reconcile-time prune never removed).
	saOwned, err := r.ownedServiceAccountChildren(ctx, cp)
	if err != nil {
		return nil, err
	}
	merge(saOwned)

	if cp.IsExternalKeystone() {
		catOwned, err := r.ownedCatalogEntryChildren(ctx, cp)
		if err != nil {
			return nil, err
		}
		merge(catOwned)
	}
	return children, nil
}

// ownedServiceAccountChildren lists the service-account User/Project CRs this
// ControlPlane owns, whether or not the spec still declares them, so a spec edit
// made while the reconcile-time prune could not run (admin credential drifted, or
// the edit landed moments before the delete) cannot leave a K-ORC CR nobody names
// to wedge the namespace. Ownership is decided by ownsServiceAccountChild (the
// controller reference AND the "-service-account-" name prefix). An absent K-ORC
// CRD (meta.IsNoMatchError) reads as "nothing to sweep", matching
// deleteORCResources.
func (r *ControlPlaneReconciler) ownedServiceAccountChildren(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) ([]orcChildObject, error) {
	ns := childNamespace(cp)
	var out []orcChildObject

	var users orcv1alpha1.UserList
	switch err := r.List(ctx, &users, client.InNamespace(ns)); {
	case err == nil:
		for i := range users.Items {
			if r.ownsServiceAccountChild(cp, &users.Items[i]) {
				out = append(out, orcChildObject{func() client.Object { return &orcv1alpha1.User{} }, users.Items[i].Name})
			}
		}
	case meta.IsNoMatchError(err):
	default:
		return nil, fmt.Errorf("listing service-account Users for teardown: %w", err)
	}

	var projects orcv1alpha1.ProjectList
	switch err := r.List(ctx, &projects, client.InNamespace(ns)); {
	case err == nil:
		for i := range projects.Items {
			if r.ownsServiceAccountChild(cp, &projects.Items[i]) {
				out = append(out, orcChildObject{func() client.Object { return &orcv1alpha1.Project{} }, projects.Items[i].Name})
			}
		}
	case meta.IsNoMatchError(err):
	default:
		return nil, fmt.Errorf("listing service-account Projects for teardown: %w", err)
	}

	return out, nil
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
//     Also delete every owned PushSecret: their DeletionPolicy=Delete cleanup —
//     ESO removing the mirrored OpenBao data — needs the per-tenant SecretStore
//     and its ServiceAccount, which the post-release GC cascade reaps
//     unsequenced, so it must happen while the finalizer still holds them.
//  2. When no K-ORC CR and no owned PushSecret remain, release the finalizer so
//     GC tears down the rest.
//  3. When every CR still present is an Unmanaged import, force-remove their
//     K-ORC finalizers right away: an import's deletion is CR-only, but K-ORC
//     builds an authenticated delete actuator and re-fetches the imported
//     resource by ID before releasing ANY finalizer — and the imports
//     authenticate with the admin application credential whose revocation
//     step 1 already triggered (the managed children ride the admin-password
//     cloud instead). Waiting on them is waiting on a dead-credential retry
//     loop only the stall breaker would cut, five minutes later. Nothing is
//     orphaned: an import never owned the OpenStack resource behind it.
//  4. While managed CRs remain and the bounded orcTeardownStallTimeout has not
//     elapsed, report KORCReady=False/FinalizingORC and requeue.
//  5. Once the stall timeout elapses (K-ORC cannot make progress — most likely
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

	// The owned PushSecrets carry DeletionPolicy=Delete: ESO removes the
	// mirrored OpenBao data when it processes their deletion — and that needs
	// the per-tenant SecretStore and its eso-tenant-auth ServiceAccount, both of
	// which are CP-owned and die in the unsequenced GC cascade the moment the
	// finalizer is released. Delete the PushSecrets HERE, while the store still
	// authenticates, and gate the release on their disappearance; otherwise the
	// credential this teardown revokes in Keystone outlives it in OpenBao behind
	// an Errored, Terminating PushSecret.
	pushRemaining, err := r.deleteOwnedPushSecrets(ctx, cp)
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

	if len(remaining) == 0 && len(pushRemaining) == 0 {
		// Every owned K-ORC CR is gone (revoked and deleted, or never existed)
		// and ESO has finished the OpenBao cleanup behind the owned PushSecrets.
		//
		// The cross-namespace children are still standing, though: no GC cascade
		// reaches them, because they carry ownership labels rather than an owner
		// reference. Tear them down HERE, while the finalizer still holds the
		// ControlPlane — releasing first would strand every one of them, in a
		// namespace nothing points back from.
		done, err := r.teardownDedicatedNamespaces(ctx, cp)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !done {
			return ctrl.Result{RequeueAfter: namespaceRequeueAfter}, nil
		}

		// Release the finalizer so GC tears down Keystone/MariaDB and the rest.
		r.Recorder.Event(cp, "Normal", "ORCTeardownComplete",
			"No remaining K-ORC CRs; releasing the ControlPlane finalizer")
		controllerutil.RemoveFinalizer(cp, controlPlaneORCFinalizer)
		if err := r.Update(ctx, cp); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// Once only Unmanaged imports remain, K-ORC can never finish them: its
	// delete path re-fetches the imported resource by ID through an
	// authenticated actuator before releasing any finalizer, and the imports
	// authenticate with the admin application credential this sweep just
	// revoked. Force-release their K-ORC finalizers instead of waiting out the
	// stall window on a dead-credential retry loop. This is a Normal event, not
	// a Warning: an import's deletion is CR-only by definition, so the external
	// installation is left bit-for-bit intact and nothing is orphaned.
	onlyUnmanagedLeft := len(remaining) > 0
	for _, obj := range remaining {
		if isManagedORCChild(obj) {
			onlyUnmanagedLeft = false
			break
		}
	}
	if onlyUnmanagedLeft {
		names := make([]string, 0, len(remaining))
		for _, obj := range remaining {
			names = append(names, obj.GetName())
		}
		if err := r.forceRemoveKORCFinalizers(ctx, remaining); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Event(cp, "Normal", "ORCImportsReleased", fmt.Sprintf(
			"released the K-ORC finalizers of the remaining unmanaged import CR(s) %v: an import's deletion is "+
				"CR-only, and its finalizer cannot authenticate once the admin application credential is revoked",
			names,
		))
		log.FromContext(ctx).Info("released the K-ORC finalizers of the remaining unmanaged imports",
			"imports", names)
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// Managed K-ORC CRs are still Terminating behind a finalizer that revokes
	// against Keystone, and/or ESO is still deleting the OpenBao data behind the
	// owned PushSecrets. Within the stall window, wait and requeue; updateStatus
	// persists the FinalizingORC condition so the wait is operator-visible. The
	// reason matches the FinalizingORC event above.
	if time.Since(cp.DeletionTimestamp.Time) <= orcTeardownStallTimeout {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKORCReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "FinalizingORC",
			Message: fmt.Sprintf(
				"waiting for %d K-ORC CR(s) and %d PushSecret(s) to finish their teardown before releasing the ControlPlane",
				len(remaining), len(pushRemaining),
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

	// The stall can also be hit with ZERO K-ORC CRs left (only PushSecrets
	// stuck, handled below) — suppress the K-ORC Warning then, so it never
	// alarms about an empty list.
	if len(remaining) > 0 {
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
	}

	// A PushSecret still present past the stall window means ESO cannot finish
	// the OpenBao cleanup (backend or store gone). Strip its finalizers so the
	// namespace cannot wedge on it, and name the OpenBao paths that keep their
	// data — like the orphaned managed CRs, they are repair-by-hand outcomes.
	if len(pushRemaining) > 0 {
		stuckKeys := make([]string, 0, len(pushRemaining))
		for _, ps := range pushRemaining {
			for _, d := range ps.Spec.Data {
				stuckKeys = append(stuckKeys, d.Match.RemoteRef.RemoteKey)
			}
			ps.Finalizers = nil
			if err := r.Update(ctx, ps); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("force-removing finalizers from PushSecret %q: %w", ps.Name, err)
			}
		}
		r.Recorder.Event(cp, "Warning", "OpenBaoCleanupStalled", fmt.Sprintf(
			"ESO could not delete the mirrored OpenBao data behind %d PushSecret(s) within %s; the OpenBao "+
				"path(s) %v may still hold the revoked credential — delete them by hand",
			len(pushRemaining), orcTeardownStallTimeout, stuckKeys,
		))
	}

	controllerutil.RemoveFinalizer(cp, controlPlaneORCFinalizer)
	if err := r.Update(ctx, cp); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer after force-remove: %w", err)
	}
	return ctrl.Result{}, nil
}

// teardownDedicatedNamespaces deletes the children the ControlPlane placed in a
// namespace of its own, and reports whether the sweep is complete. It is the
// GARBAGE-COLLECTION MECHANISM that owner references cannot provide: a
// cross-namespace child carries no owner reference (Kubernetes forbids one), so
// nothing reaps it when the ControlPlane goes. This does, by hand, while the
// finalizer still holds the CR.
//
// The order is load-bearing:
//
//  1. The SERVICE CHILDREN (Keystone, Horizon) first, and the sweep waits for them
//     to disappear. Their own operators run a sequenced ESO cleanup on deletion —
//     the Keystone child's fernet/credential-key PushSecrets purge their OpenBao
//     paths — and that cleanup authenticates through the tenant store in the same
//     namespace. Removing the store first would leave the key material in OpenBao
//     with no Kubernetes object naming it.
//  2. Then the NAMESPACE, per lifecycle:
//     - Managed: delete the namespace, which cascades everything left in it. It is
//     deleted ONLY when it carries our ownership labels — reconcileNamespaces
//     never adopts a namespace it did not create, and neither does this. An
//     unlabelled one is left standing with a Warning, rather than destroying a
//     namespace (and every workload in it) the operator never owned.
//     - External: the namespace stays. Its residue is swept by name instead — the
//     backing services, the credential material, and the tenant-store trio LAST,
//     for the reason above.
//
// Past orcTeardownStallTimeout the sweep stops waiting: it emits a Warning naming
// what is stuck and reports done, so a wedged child can never make a namespace
// undeletable. That mirrors the ORC stall escape, and it is the same trade — a
// repairable leak beats a permanently wedged namespace.
func (r *ControlPlaneReconciler) teardownDedicatedNamespaces(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) (bool, error) {
	assignments := cp.DedicatedServiceNamespaces()
	if len(assignments) == 0 {
		return true, nil
	}
	logger := log.FromContext(ctx)

	stalled := time.Since(cp.DeletionTimestamp.Time) > orcTeardownStallTimeout

	var stuck []string
	for _, assignment := range assignments {
		remaining, err := r.deleteServiceChildrenIn(ctx, cp, assignment.Name)
		if err != nil {
			return false, err
		}
		if len(remaining) > 0 {
			stuck = append(stuck, remaining...)
			continue
		}

		if assignment.Lifecycle == c5c3v1alpha1.ServiceNamespaceLifecycleExternal {
			r.sweepExternalNamespaceResidue(ctx, cp, assignment.Name)
			continue
		}
		if err := r.deleteManagedNamespace(ctx, cp, assignment.Name); err != nil {
			return false, err
		}
	}

	if len(stuck) == 0 {
		return true, nil
	}

	if !stalled {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeNamespacesReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "FinalizingNamespaces",
			Message: fmt.Sprintf("waiting for %d cross-namespace child(ren) to finish their teardown before "+
				"releasing the ControlPlane: %v", len(stuck), stuck),
		})
		return false, nil
	}

	// Stalled. Proceed anyway rather than wedging the namespace forever, and name
	// what was left behind — like the orphaned K-ORC resources, it is a
	// repair-by-hand outcome.
	r.Recorder.Event(cp, "Warning", "NamespaceTeardownStalled", fmt.Sprintf(
		"cross-namespace child(ren) %v stayed present longer than %s; releasing the ControlPlane anyway. "+
			"They carry no owner reference, so nothing will garbage-collect them — remove them by hand",
		stuck, orcTeardownStallTimeout,
	))
	logger.Info("cross-namespace teardown stalled; releasing the ControlPlane anyway",
		"stuck", stuck, "stallTimeout", orcTeardownStallTimeout)
	return true, nil
}

// crossNamespaceServiceChildren returns the service children the ControlPlane
// placed in namespace: the Keystone child when the Keystone service is assigned
// there, the Horizon child likewise. Both are matched by their deterministic
// names; ownership is re-checked against the live object before anything is
// deleted.
func crossNamespaceServiceChildren(cp *c5c3v1alpha1.ControlPlane, namespace string) []client.Object {
	var children []client.Object
	if cp.KeystoneNamespace() == namespace {
		children = append(children, &keystonev1alpha1.Keystone{
			ObjectMeta: metav1.ObjectMeta{Name: keystoneName(cp), Namespace: namespace},
		})
	}
	if cp.HorizonNamespace() == namespace {
		children = append(children, &horizonv1alpha1.Horizon{
			ObjectMeta: metav1.ObjectMeta{Name: horizonName(cp), Namespace: namespace},
		})
	}
	return children
}

// deleteServiceChildrenIn deletes the service children this ControlPlane placed in
// namespace and returns those still present afterwards (Terminating behind their
// own operator's cleanup finalizers). A child that is not ours — same name, no
// ownership labels — is never touched and never reported as stuck.
func (r *ControlPlaneReconciler) deleteServiceChildrenIn(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, namespace string,
) ([]string, error) {
	var remaining []string
	for _, child := range crossNamespaceServiceChildren(cp, namespace) {
		key := client.ObjectKeyFromObject(child)
		switch err := r.Get(ctx, key, child); {
		case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
			continue
		case err != nil:
			return nil, fmt.Errorf("getting %T %s for cross-namespace teardown: %w", child, key, err)
		}
		if !isControlPlaneChild(child, cp) {
			continue
		}
		if child.GetDeletionTimestamp().IsZero() {
			if err := client.IgnoreNotFound(
				r.Delete(ctx, child, client.PropagationPolicy(metav1.DeletePropagationBackground)),
			); err != nil {
				return nil, fmt.Errorf("deleting %T %s: %w", child, key, err)
			}
		}
		remaining = append(remaining, fmt.Sprintf("%s/%s", namespace, child.GetName()))
	}
	return remaining, nil
}

// deleteManagedNamespace deletes a namespace the operator created, which cascades
// everything left in it. It refuses to delete one that does not carry our
// ownership labels: reconcileNamespaces never adopts a foreign namespace, and
// deleting one here would destroy every workload in it. That case can only arise
// from a webhook-bypassed CR or a namespace re-created out-of-band under the same
// name, so it is reported as a Warning rather than acted on.
//
// The delete is fire-and-observe: the namespace's own termination (Kubernetes
// reaping every object in it) can take a while, and holding the ControlPlane
// finalizer for it would gain nothing — the children the ControlPlane is
// responsible for are already gone by the time this runs.
func (r *ControlPlaneReconciler) deleteManagedNamespace(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, name string,
) error {
	ns := &corev1.Namespace{}
	switch err := r.Get(ctx, types.NamespacedName{Name: name}, ns); {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("getting managed service namespace %q for teardown: %w", name, err)
	}
	if !ns.DeletionTimestamp.IsZero() {
		return nil
	}
	if !isControlPlaneChild(ns, cp) {
		r.Recorder.Event(cp, "Warning", "NamespaceNotOwned", fmt.Sprintf(
			"namespace %q does not carry this ControlPlane's ownership labels, so it was NOT deleted even though "+
				"its lifecycle is Managed; the operator never destroys a namespace it did not create", name,
		))
		return nil
	}
	if err := client.IgnoreNotFound(r.Delete(ctx, ns)); err != nil {
		return fmt.Errorf("deleting managed service namespace %q: %w", name, err)
	}
	log.FromContext(ctx).Info("deleted managed service namespace", "namespace", name)
	return nil
}

// sweepExternalNamespaceResidue deletes, best-effort, the objects the ControlPlane
// placed in a namespace it does NOT own — the namespace itself must survive, so
// nothing cascades and every object has to be named. The set is deterministic
// (every name is derived from the ControlPlane), so nothing has to be discovered:
// the backing services, the admin-password and DB-credential material, and the
// tenant-store trio.
//
// The tenant-store trio goes LAST: the service children deleted before this ran
// their own ESO cleanup through that store, and an ESO PushSecret cannot purge its
// OpenBao path once the store it authenticates with is gone.
//
// Each object is ownership-checked against its live state, so a same-named object
// belonging to somebody else in this shared namespace is left alone. Errors are
// logged rather than propagated: this is the last step before the ControlPlane is
// released, and a residual object is a repairable leak, whereas an error here
// would wedge the namespace on a finalizer that can never clear.
func (r *ControlPlaneReconciler) sweepExternalNamespaceResidue(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, namespace string,
) {
	logger := log.FromContext(ctx)

	unstructuredIn := func(gvk schema.GroupVersionKind, name string) client.Object {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		u.SetName(name)
		u.SetNamespace(namespace)
		return u
	}

	var objs []client.Object
	// The backing services this namespace's services resolved to.
	for _, inst := range r.managedInfraInstances(cp) {
		if inst.namespace != namespace {
			continue
		}
		switch inst.kind {
		case "MariaDB":
			objs = append(objs, &mariadbv1alpha1.MariaDB{
				ObjectMeta: metav1.ObjectMeta{Name: inst.name, Namespace: namespace},
			})
		case "Memcached":
			objs = append(objs, unstructuredIn(memcachedGVK, inst.name))
		}
	}
	// The credential material, which follows the Keystone service.
	if cp.KeystoneNamespace() == namespace {
		objs = append(
			objs,
			&esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
				Name: adminPasswordSecretName(cp), Namespace: namespace,
			}},
			&esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
				Name: dbCredentialSecretName(cp), Namespace: namespace,
			}},
			&esgenv1alpha1.VaultDynamicSecret{ObjectMeta: metav1.ObjectMeta{
				Name: dbCredentialSecretName(cp), Namespace: namespace,
			}},
			unstructuredIn(certificateGVK, dbCredentialClientCertName(cp)),
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
				Name: dbCredentialServiceAccountName, Namespace: namespace,
			}},
		)
	}
	// The tenant store LAST: everything above authenticated through it.
	objs = append(
		objs,
		&esov1.SecretStore{ObjectMeta: metav1.ObjectMeta{Name: esoTenantStoreName, Namespace: namespace}},
		unstructuredIn(certificateGVK, esoTenantClientCertName),
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: esoTenantServiceAccountName, Namespace: namespace,
		}},
	)

	for _, obj := range objs {
		key := client.ObjectKeyFromObject(obj)
		switch err := r.Get(ctx, key, obj); {
		case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
			continue
		case err != nil:
			logger.V(1).Info("best-effort residue sweep could not read an object",
				"object", key, "error", err.Error())
			continue
		}
		if !isControlPlaneChild(obj, cp) {
			continue
		}
		if err := client.IgnoreNotFound(r.Delete(ctx, obj)); err != nil {
			logger.V(1).Info("best-effort residue sweep could not delete an object",
				"object", key, "error", err.Error())
		}
	}
}

// deleteOwnedPushSecrets issues an idempotent Delete on every PushSecret this
// ControlPlane owns and returns those still present after the sweep. The owned
// PushSecrets carry DeletionPolicy=Delete, so ESO deletes the mirrored OpenBao
// data while processing their deletion — which only works while the per-tenant
// SecretStore and its eso-tenant-auth ServiceAccount are still alive, i.e.
// BEFORE the ControlPlane finalizer is released and the GC cascade reaps them.
// An absent PushSecret CRD (meta.IsNoMatchError) reads as nothing-to-clean so
// the finalizer can still release when the ESO stack is gone.
func (r *ControlPlaneReconciler) deleteOwnedPushSecrets(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) ([]*esov1alpha1.PushSecret, error) {
	var list esov1alpha1.PushSecretList
	switch err := r.List(ctx, &list, client.InNamespace(childNamespace(cp))); {
	case err == nil:
	case meta.IsNoMatchError(err):
		return nil, nil
	default:
		return nil, fmt.Errorf("listing owned PushSecrets for teardown: %w", err)
	}

	var remaining []*esov1alpha1.PushSecret
	for i := range list.Items {
		ps := &list.Items[i]
		if !metav1.IsControlledBy(ps, cp) {
			continue
		}
		if ps.DeletionTimestamp.IsZero() {
			if err := client.IgnoreNotFound(r.Delete(ctx, ps)); err != nil {
				return nil, fmt.Errorf("deleting PushSecret %q: %w", ps.Name, err)
			}
		}
		// Re-Get: a finalizer-less PushSecret is gone with the Delete, while one
		// held by ESO stays present until the remote delete is confirmed.
		current := &esov1alpha1.PushSecret{}
		switch err := r.Get(ctx, client.ObjectKeyFromObject(ps), current); {
		case err == nil:
			remaining = append(remaining, current)
		case apierrors.IsNotFound(err):
		default:
			return nil, fmt.Errorf("re-checking PushSecret %q: %w", ps.Name, err)
		}
	}
	return remaining, nil
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
	case *orcv1alpha1.Project:
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
