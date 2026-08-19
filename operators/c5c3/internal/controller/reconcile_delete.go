// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
	placementv1alpha1 "github.com/c5c3/forge/operators/placement/api/v1alpha1"
	openbaov1alpha1 "github.com/dc-tec/openbao-operator/api/v1alpha1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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
	commonmulticluster "github.com/c5c3/forge/internal/common/multicluster"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	barbicanv1alpha1 "github.com/c5c3/forge/operators/barbican/api/v1alpha1"
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
	newRole := func() client.Object { return &orcv1alpha1.Role{} }
	newRoleAssignment := func() client.Object { return &orcv1alpha1.RoleAssignment{} }

	objs := []orcChildObject{
		{func() client.Object { return &orcv1alpha1.ApplicationCredential{} }, adminAppCredentialName(cp)},
	}

	// The managed-mode catalog children come from the same per-service table that
	// registers them (managedCatalogRows), so a second service is a table row here
	// too, not a second hard-coded pair. In External mode these very names belong to
	// the unmanaged identity imports instead; either way a name that never existed in
	// the current mode is simply NotFound and tolerated as already-gone.
	for _, row := range managedCatalogRows(cp) {
		objs = append(objs, orcChildObject{newService, row.crName})
		for _, ep := range row.endpoints {
			objs = append(objs, orcChildObject{newEndpoint, ep.crName})
		}
	}

	// Preserved-orphan cover for the image catalog row. When spec.services.glance is
	// set, managedCatalogRows already enumerated its Service/Endpoints above. When it
	// is UNSET the reconcile-time sweep (deleteOrphanedGlance) only removes those CRs
	// on the deletion opt-in, so a glance removal without the opt-in leaves the row
	// standing — enumerate its names unconditionally here so the ControlPlane teardown
	// still tears it down. A name that never existed is simply NotFound and tolerated
	// as already-gone.
	if cp.Spec.Services.Glance == nil {
		objs = append(
			objs,
			orcChildObject{newService, glanceCatalogServiceName(cp)},
			orcChildObject{newEndpoint, glanceCatalogEndpointName(cp, "internal")},
			orcChildObject{newEndpoint, glanceCatalogEndpointName(cp, "public")},
		)
	}

	// The same preserved-orphan cover for the placement catalog row: a placement
	// removal without the deletion opt-in leaves the row standing, so its names are
	// enumerated unconditionally here and torn down with the ControlPlane.
	if cp.Spec.Services.Placement == nil {
		objs = append(
			objs,
			orcChildObject{newService, placementCatalogServiceName(cp)},
			orcChildObject{newEndpoint, placementCatalogEndpointName(cp, "internal")},
			orcChildObject{newEndpoint, placementCatalogEndpointName(cp, "public")},
		)
	}

	// And the same cover for the key-manager catalog row: a barbican removal without
	// the deletion opt-in leaves the row standing, so its names are enumerated
	// unconditionally here and torn down with the ControlPlane.
	if cp.Spec.Services.Barbican == nil {
		objs = append(
			objs,
			orcChildObject{newService, barbicanCatalogServiceName(cp)},
			orcChildObject{newEndpoint, barbicanCatalogEndpointName(cp, "internal")},
			orcChildObject{newEndpoint, barbicanCatalogEndpointName(cp, "public")},
		)
	}

	objs = append(
		objs,
		orcChildObject{newUser, adminUserRef(cp)},
		orcChildObject{newDomain, adminDomainRef(cp)},
	)

	// Declarative service accounts are mode-independent, so their children are torn
	// down in BOTH keystone modes (before the External-only catalog additions). Each
	// declared entry projects a managed User and Project (whose K-ORC finalizers
	// delete them from Keystone), one managed RoleAssignment per declared role (also
	// deleted from Keystone), plus the collision-probe, per-account domain, and
	// per-role Role imports (CR-only deletes). The password Secrets / PushSecret /
	// ExternalSecret are owner-reference-GC'd, not K-ORC CRs, so they are not part of
	// this sweep.
	for i := range cp.Spec.KORC.ServiceAccounts {
		sa := cp.Spec.KORC.ServiceAccounts[i]
		objs = append(
			objs,
			orcChildObject{newUser, serviceAccountUserRef(cp, sa)},
			orcChildObject{newUser, serviceAccountUserProbeRef(cp, sa)},
			orcChildObject{newProject, serviceAccountProjectRef(cp, sa)},
			orcChildObject{newProject, serviceAccountProjectProbeRef(cp, sa)},
		)
		// RoleAssignment (managed) before its Role import (unmanaged), matching the
		// dependency direction K-ORC's own deletion guards enforce.
		for _, role := range sa.Roles {
			objs = append(
				objs,
				orcChildObject{newRoleAssignment, serviceAccountRoleAssignmentRef(cp, sa, role)},
				orcChildObject{newRole, serviceAccountRoleImportRef(cp, role)},
			)
		}
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
	declared := orcChildObjects(cp)
	var children []orcChildObject
	seen := make(map[string]struct{}, len(declared))
	merge := func(extra []orcChildObject) {
		for _, child := range extra {
			if _, dup := seen[child.key()]; dup {
				continue
			}
			seen[child.key()] = struct{}{}
			children = append(children, child)
		}
	}
	// The spec-derived list is itself merged, not taken verbatim: it walks the
	// declared accounts, and accounts sharing a role name the SAME Role import
	// (serviceAccountRoleImportRef is not keyed on the account), so it can name one
	// CR twice — the duplicate key() exists to prevent.
	merge(declared)

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

	var roleAssignments orcv1alpha1.RoleAssignmentList
	switch err := r.List(ctx, &roleAssignments, client.InNamespace(ns)); {
	case err == nil:
		for i := range roleAssignments.Items {
			if r.ownsServiceAccountChild(cp, &roleAssignments.Items[i]) {
				out = append(out, orcChildObject{
					func() client.Object { return &orcv1alpha1.RoleAssignment{} }, roleAssignments.Items[i].Name,
				})
			}
		}
	case meta.IsNoMatchError(err):
	default:
		return nil, fmt.Errorf("listing service-account RoleAssignments for teardown: %w", err)
	}

	var roles orcv1alpha1.RoleList
	switch err := r.List(ctx, &roles, client.InNamespace(ns)); {
	case err == nil:
		for i := range roles.Items {
			if r.ownsServiceAccountChild(cp, &roles.Items[i]) {
				out = append(out, orcChildObject{func() client.Object { return &orcv1alpha1.Role{} }, roles.Items[i].Name})
			}
		}
	case meta.IsNoMatchError(err):
	default:
		return nil, fmt.Errorf("listing service-account Roles for teardown: %w", err)
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
		// The remote-children finalizer outlives the ORC one on the stall path: the
		// escape below releases the ORC finalizer without ever reaching the namespace
		// sweep, so the placed namespaces are still standing. Finish that sweep here
		// — nothing else can — and release the remaining finalizer once it is done.
		// A ControlPlane that placed nothing carries no remote-children finalizer, so
		// this is the same no-op it always was.
		return r.reconcileDeleteRemoteChildren(ctx, cp)
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
		done, err := r.sweepNamespacesBeforeRelease(ctx, cp)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !done {
			return ctrl.Result{RequeueAfter: namespaceRequeueAfter}, nil
		}

		// Release the finalizers so GC tears down Keystone/MariaDB and the rest. The
		// remote-children one goes with it: the sweep above visited every placed
		// namespace, on the cluster it lives on or, past the abandon window, not at
		// all — either way nothing is left for it to hold the CR open for.
		r.Recorder.Event(cp, "Normal", "ORCTeardownComplete",
			"No remaining K-ORC CRs; releasing the ControlPlane finalizer")
		controllerutil.RemoveFinalizer(cp, controlPlaneORCFinalizer)
		controllerutil.RemoveFinalizer(cp, commonmulticluster.RemoteChildrenFinalizer)
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
		for _, pending := range pushRemaining {
			ps := pending.pushSecret
			for _, d := range ps.Spec.Data {
				stuckKeys = append(stuckKeys, d.Match.RemoteRef.RemoteKey)
			}
			ps.Finalizers = nil
			// Through the client of the cluster the PushSecret was found on: one in a
			// placed namespace exists on the target alone.
			if err := pending.cluster.Update(ctx, ps); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("force-removing finalizers from PushSecret %q: %w", ps.Name, err)
			}
		}
		r.Recorder.Event(cp, "Warning", "OpenBaoCleanupStalled", fmt.Sprintf(
			"ESO could not delete the mirrored OpenBao data behind %d PushSecret(s) within %s; the OpenBao "+
				"path(s) %v may still hold the revoked credential — delete them by hand",
			len(pushRemaining), orcTeardownStallTimeout, stuckKeys,
		))
	}

	// Only the ORC finalizer. The escape gave up on K-ORC, not on the children
	// this ControlPlane placed on a target cluster: the namespace sweep never ran
	// on this path, so the remote-children finalizer stays on and
	// reconcileDeleteRemoteChildren finishes the sweep from the next pass.
	controllerutil.RemoveFinalizer(cp, controlPlaneORCFinalizer)
	if err := r.Update(ctx, cp); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer after force-remove: %w", err)
	}
	return ctrl.Result{}, nil
}

// reconcileDeleteRemoteChildren finishes the teardown of a ControlPlane that has
// already released its ORC finalizer but still carries the remote-children one.
// Only the ORC stall escape leaves a CR in that state: it gives up on K-ORC
// without reaching the namespace sweep, so the children on the placed clusters
// are still standing and no cascade will ever collect them.
//
// It is a no-op for a CR without the finalizer, which is every ControlPlane that
// places no service on a target cluster.
func (r *ControlPlaneReconciler) reconcileDeleteRemoteChildren(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cp, commonmulticluster.RemoteChildrenFinalizer) {
		return ctrl.Result{}, nil
	}

	done, err := r.sweepNamespacesBeforeRelease(ctx, cp)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		return ctrl.Result{RequeueAfter: namespaceRequeueAfter}, nil
	}

	controllerutil.RemoveFinalizer(cp, commonmulticluster.RemoteChildrenFinalizer)
	if err := r.Update(ctx, cp); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing the remote-children finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// sweepNamespacesBeforeRelease is the cross-namespace teardown both release paths
// gate on: every namespace the ControlPlane placed a service in is swept, on the
// cluster it lives on, and the cluster-scoped auth-delegator binding no namespace
// sweep can reach is deleted afterwards. It reports whether the ControlPlane may
// be released.
//
// The auth-delegator binding is cluster-scoped, so the sweep reaches it only when
// Barbican runs in a namespace of its own. Co-located — the default — there is no
// dedicated namespace at all, teardownDedicatedNamespaces returns at once, and
// nothing else can collect it.
func (r *ControlPlaneReconciler) sweepNamespacesBeforeRelease(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) (bool, error) {
	done, err := r.teardownDedicatedNamespaces(ctx, cp)
	if err != nil || !done {
		return false, err
	}
	r.sweepRegistrationTenantStores(ctx, cp)
	if err := r.deleteBarbicanAuthDelegatorBinding(ctx, cp); err != nil {
		return false, err
	}
	return true, nil
}

// sweepRegistrationTenantStores deletes, best-effort, the tenant-store trios this
// ControlPlane provisioned in the allowlisted namespaces standalone registrations
// come from (see reconcileRegistrationTenantStores).
//
// Nothing else collects them. They sit outside every namespace
// teardownDedicatedNamespaces walks, and a cross-namespace child carries ownership
// labels rather than an owner reference, so no GC cascade reaches them either.
//
// Errors are logged rather than propagated, exactly like
// sweepExternalNamespaceResidue: this is one of the last steps before the
// ControlPlane is released, and a residual trio is a repairable leak, whereas an
// error here would wedge the CR on a finalizer that can never clear.
//
// Unlike the per-pass collection this does NOT wait for a namespace's last
// registration to leave. The plane is going away, so the store has nothing left to
// reach, and a KeystoneService still standing out there fails its own teardown open
// once the ControlPlane it references is gone. A registration mid-purge can lose
// the store its OpenBao cleanup rides on, which is the same trade the teardown
// already takes past its stall windows: a leak somebody can collect beats a CR
// nobody can delete.
func (r *ControlPlaneReconciler) sweepRegistrationTenantStores(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) {
	logger := log.FromContext(ctx)

	namespaces, err := r.registrationTenantStoreNamespaces(ctx, cp)
	if err != nil {
		logger.V(1).Info("best-effort registration tenant-store sweep could not enumerate its namespaces",
			"error", err.Error())
		return
	}
	for _, namespace := range namespaces {
		logger.Info("sweeping the registration tenant store of a terminating ControlPlane", "namespace", namespace)
		if err := r.deleteESOTenantStoreTrioIn(ctx, cp, namespace); err != nil {
			logger.V(1).Info("best-effort registration tenant-store sweep could not delete a trio",
				"namespace", namespace, "error", err.Error())
		}
	}
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
//     with no Kubernetes object naming it. The service CRs live on the MANAGEMENT
//     cluster whatever cluster their service was placed on, so waiting for them
//     here is also what waits out their own operators' remote sweeps.
//  2. Then what the ControlPlane put in that namespace, in the order its
//     lifecycle decides. One half of it is the LABEL-SELECTED sweep of the cluster
//     the namespace was placed on (commonmulticluster.DeleteRemoteChildren over
//     controlPlaneRemoteChildKinds), which is what reaches the children no cascade
//     and no owner reference can, both being confined to one cluster. It is a
//     no-op for a namespace nothing was placed on.
//     - Managed: the sweep, then the namespace itself, deleted on BOTH clusters
//     because reconcileNamespaces created it on both. It is deleted ONLY when it
//     carries our ownership labels — reconcileNamespaces never adopts a namespace
//     it did not create, and neither does this. An unlabelled one is left standing
//     with a Warning, rather than destroying a namespace (and every workload in
//     it) the operator never owned.
//     - External: the namespace stays, so its residue is named and deleted
//     instead — the backing services, the credential material, and the
//     tenant-store trio LAST, for the reason above — and the sweep follows it,
//     reaching whatever the names did not enumerate. That way round because the
//     named sweep is the only one of the two with an order to give.
//
// A placed namespace whose cluster does not resolve is not swept: the pass
// reports NOT done, so the ControlPlane keeps its finalizers and tries again.
// Past commonmulticluster.AbandonAfter the cluster is given up on instead — a
// Warning records that its children were left running, and the sweep continues
// without it, because holding the CR in Terminating forever helps nobody.
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

	var stuck, unresolved []string
	for _, assignment := range assignments {
		remaining, err := r.deleteServiceChildrenIn(ctx, cp, assignment.Name)
		if err != nil {
			return false, err
		}
		if len(remaining) > 0 {
			stuck = append(stuck, remaining...)
			continue
		}

		// The cluster this namespace lives on, resolved through the DELETION
		// resolver: a target that was deregistered under a terminating CR must not
		// fail the pass, or the finalizers could never be released.
		ref := targetClusterRefForNamespace(cp, assignment.Name)
		children, wait := commonmulticluster.ResolveChildrenClientForDeletion(
			ctx, r.Resolver, r.Client, ref, *cp.DeletionTimestamp)
		switch {
		case wait:
			unresolved = append(unresolved, ref.Name)
			continue
		case children == nil:
			// Past the abandon window. The children on that cluster are unreachable
			// either way, so they are left where they are and the teardown carries on.
			r.Recorder.Event(cp, "Warning", "RemoteChildrenAbandoned", fmt.Sprintf(
				"Target cluster %q is no longer registered; releasing the remote-children finalizer without "+
					"deleting the objects it holds in namespace %q labelled as owned by this ControlPlane",
				ref.Name, assignment.Name))
			// Abandoning the target's copy of the namespace does not license leaking
			// the management cluster's, which is reachable and ours: nothing comes
			// back for it once both finalizers are released (see clustersFor).
			if assignment.Lifecycle != c5c3v1alpha1.ServiceNamespaceLifecycleExternal {
				if err := r.deleteManagedNamespace(ctx, r.Client, cp, assignment.Name); err != nil {
					return false, err
				}
			}
			continue
		}

		// The External residue is swept by name, and BEFORE the label-selected sweep
		// below, because it is the only one of the two with an order to give: the
		// tenant-store trio goes last, after everything that authenticated through it.
		external := assignment.Lifecycle == c5c3v1alpha1.ServiceNamespaceLifecycleExternal
		if external {
			r.sweepExternalNamespaceResidue(ctx, children, cp, assignment.Name)
		}

		// Then everything else the ControlPlane left in this namespace on a target
		// cluster: what the named sweep does not enumerate, and what a namespace
		// deletion cannot cascade across a cluster boundary.
		if err := r.deleteRemoteNamespaceChildren(ctx, children, cp, ref, assignment.Name); err != nil {
			return false, err
		}
		if external {
			continue
		}

		// A placed namespace exists on the management cluster as well, because
		// reconcileNamespaces ensures it on both; deleting it on one alone would
		// leave the other standing with nothing left to reclaim it (see clustersFor).
		for _, c := range r.clustersFor(children) {
			if err := r.deleteManagedNamespace(ctx, c, cp, assignment.Name); err != nil {
				return false, err
			}
		}
	}

	// A cluster that may still be engaging holds the release whatever else the
	// pass found: its children are neither swept nor abandoned yet, and giving up
	// on them here would strand them on a cluster that is about to come back.
	if len(unresolved) > 0 {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeNamespacesReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             commonmulticluster.TargetClusterUnavailable,
			Message: fmt.Sprintf("target cluster(s) %v do not resolve; waiting at least %s for them before "+
				"abandoning the children the ControlPlane placed on them", unresolved, commonmulticluster.AbandonAfter),
		})
		return false, nil
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

// deleteRemoteNamespaceChildren deletes everything of controlPlaneRemoteChildKinds
// the ControlPlane owns in namespace on the cluster children writes to. It is a
// no-op for an unplaced namespace, whose children the local cascade collects, so
// the caller runs it without branching on where the namespace lives.
//
// The list goes through the target cluster's UNCACHED reader rather than through
// the children client's cache: this sweep is what licenses the finalizer release,
// and a child the cache has not caught up on would be missed, leaving no CR behind
// to delete it.
func (r *ControlPlaneReconciler) deleteRemoteNamespaceChildren(
	ctx context.Context, children client.Client, cp *c5c3v1alpha1.ControlPlane,
	ref *commonv1.TargetClusterRefSpec, namespace string,
) error {
	reader, err := commonmulticluster.ResolveChildrenAPIReader(ctx, r.Resolver, children, ref)
	if err != nil {
		return err
	}
	return commonmulticluster.DeleteRemoteChildren(ctx, reader, children, r.Scheme, cp,
		namespace, controlPlaneRemoteChildKinds)
}

// teardownReader returns the reader the teardown decides ownership from on the
// cluster c writes to: that cluster's own uncached reader for a resolved target
// client, and the management cluster's for the local one. Both are uncached,
// because a teardown read is one-shot and several of the kinds it names are ones
// the operator never watches (see ControlPlaneReconciler.APIReader).
func (r *ControlPlaneReconciler) teardownReader(c client.Client) client.Reader {
	if commonmulticluster.IsRemote(c) {
		return commonmulticluster.LiveReader(c)
	}
	return r.apiReader()
}

// crossNamespaceServiceChildren returns the service children the ControlPlane
// placed in namespace: the Keystone child when the Keystone service is assigned
// there, and the Horizon, Glance, Placement, and Barbican children likewise. Each
// is matched by its deterministic name; ownership is re-checked against the live
// object before anything is deleted.
//
// The Barbican arm names three objects rather than one, because its secret store
// and the dedicated OpenBao instance behind it belong in the WAIT SET too. The
// namespace must not be deleted until the openbao-operator has finished the
// instance's finalizer, and that finalizer runs under the tenant RBAC living in
// this very namespace: a namespace deleted first reaps the RBAC out from under it,
// leaving the instance unfinalizable and the namespace stuck Terminating. An
// external secret store projects no instance, so its name is simply NotFound and
// tolerated as already-gone.
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
	if cp.GlanceNamespace() == namespace {
		children = append(children, &glancev1alpha1.Glance{
			ObjectMeta: metav1.ObjectMeta{Name: glanceName(cp), Namespace: namespace},
		})
	}
	if cp.PlacementNamespace() == namespace {
		children = append(children, &placementv1alpha1.Placement{
			ObjectMeta: metav1.ObjectMeta{Name: placementName(cp), Namespace: namespace},
		})
	}
	if cp.BarbicanNamespace() == namespace {
		children = append(
			children,
			&barbicanv1alpha1.Barbican{
				ObjectMeta: metav1.ObjectMeta{Name: barbicanName(cp), Namespace: namespace},
			},
			&barbicanv1alpha1.BarbicanSecretStore{
				ObjectMeta: metav1.ObjectMeta{Name: barbicanSecretStoreName(cp), Namespace: namespace},
			},
			&openbaov1alpha1.OpenBaoCluster{
				ObjectMeta: metav1.ObjectMeta{Name: barbicanOpenBaoName(cp), Namespace: namespace},
			},
		)
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

	// Also sweep the projected GlanceBackend children the ControlPlane placed in this
	// namespace. They carry no external state, so deleting them alongside the Glance
	// child is safe. Only c5c3-owned children carrying the glance child's name prefix
	// are touched — a hand-created GlanceBackend that merely shares the namespace is
	// never deleted. Each still-present owned backend is reported as remaining, the
	// same way the service children are, so the sweep waits for it to disappear.
	if cp.GlanceNamespace() == namespace {
		var backends glancev1alpha1.GlanceBackendList
		if err := r.List(ctx, &backends, client.InNamespace(namespace)); err != nil {
			return nil, fmt.Errorf("listing GlanceBackends in %q for cross-namespace teardown: %w", namespace, err)
		}
		prefix := glanceBackendNamePrefix(cp)
		for i := range backends.Items {
			b := &backends.Items[i]
			if !isControlPlaneChild(b, cp) || !strings.HasPrefix(b.Name, prefix) {
				continue
			}
			if b.GetDeletionTimestamp().IsZero() {
				if err := client.IgnoreNotFound(
					r.Delete(ctx, b, client.PropagationPolicy(metav1.DeletePropagationBackground)),
				); err != nil {
					return nil, fmt.Errorf("deleting GlanceBackend %s/%s: %w", namespace, b.Name, err)
				}
			}
			remaining = append(remaining, fmt.Sprintf("%s/%s", namespace, b.Name))
		}
	}

	// The Barbican namespace carries two more sets. First the projected
	// BarbicanSecretStores, swept on the GlanceBackend terms: only c5c3-owned stores
	// carrying the Barbican child's name prefix, so a hand-created store attached to
	// the same Barbican is never deleted. The store the projection names today is
	// already in the wait set above; this catches one the reconcile-time prune never
	// removed, because a spec edit landing moments before the delete leaves a store
	// nobody names. An absent CRD (meta.IsNoMatchError) reads as nothing to sweep,
	// the same way deleteORCResources reads an absent K-ORC stack.
	if cp.BarbicanNamespace() == namespace {
		var stores barbicanv1alpha1.BarbicanSecretStoreList
		switch err := r.List(ctx, &stores, client.InNamespace(namespace)); {
		case err == nil:
			prefix := barbicanName(cp) + "-"
			for i := range stores.Items {
				store := &stores.Items[i]
				if store.Name == barbicanSecretStoreName(cp) {
					continue
				}
				if !isControlPlaneChild(store, cp) || !strings.HasPrefix(store.Name, prefix) {
					continue
				}
				if store.GetDeletionTimestamp().IsZero() {
					if derr := client.IgnoreNotFound(
						r.Delete(ctx, store, client.PropagationPolicy(metav1.DeletePropagationBackground)),
					); derr != nil {
						return nil, fmt.Errorf("deleting BarbicanSecretStore %s/%s: %w", namespace, store.Name, derr)
					}
				}
				remaining = append(remaining, fmt.Sprintf("%s/%s", namespace, store.Name))
			}
		case meta.IsNoMatchError(err):
		default:
			return nil, fmt.Errorf("listing BarbicanSecretStores in %q for cross-namespace teardown: %w", namespace, err)
		}

		// Then the rest of the dedicated OpenBao ensemble.
		if err := r.deleteBarbicanEnsembleIn(ctx, cp, namespace); err != nil {
			return nil, err
		}
	}
	return remaining, nil
}

// deleteBarbicanEnsembleIn deletes what the dedicated OpenBao ensemble leaves
// behind once its instance is on the way out: the tenant that admitted the
// namespace, the two transport Certificates, the provisioner ServiceAccount, the
// TokenRequest Role and RoleBinding, the static-seal Secret, and the cluster-scoped
// auth-delegator binding. The instance itself and the secret store in front of it
// are in the wait set (crossNamespaceServiceChildren), so nothing here can outrun
// them.
//
// The TENANT is the one gated object. It is what admits the namespace to the
// openbao-operator, and the instance's own finalizer runs under that admission, so
// it is held back while an instance this ControlPlane owns is still present. No
// requeue is needed for that: the instance is in the wait set, so the surrounding
// flow re-runs until it is gone and a later pass takes the tenant. A tenant this
// ControlPlane did not create is never touched — in the kind stack a proving tenant
// already admits the namespace, and deleting it would revoke an admission the
// operator does not own.
//
// The auth-delegator ClusterRoleBinding is cluster-scoped, so neither a namespace
// deletion nor an owner-reference cascade ever reclaims it. It is deleted here for
// the Managed lifecycle, by sweepExternalNamespaceResidue for the External one, and
// by deleteBarbicanAuthDelegatorBinding for the co-located default that reaches
// neither. Naming it more than once costs nothing: the second Get is a NotFound.
func (r *ControlPlaneReconciler) deleteBarbicanEnsembleIn(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, namespace string,
) error {
	name := barbicanOpenBaoName(cp)

	instanceHoldsTenant := false
	instance := &openbaov1alpha1.OpenBaoCluster{}
	switch err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance); {
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
	case err != nil:
		return fmt.Errorf("checking whether OpenBaoCluster %q is gone before releasing its OpenBaoTenant: %w", name, err)
	default:
		instanceHoldsTenant = isControlPlaneChild(instance, cp)
	}

	// The tenant leads the ensemble, so holding it back is dropping that entry.
	objs := barbicanOpenBaoEnsembleObjects(name, namespace)
	if instanceHoldsTenant {
		objs = objs[1:]
	}

	for _, obj := range objs {
		if err := r.deleteOwnedEnsembleObject(ctx, cp, obj); err != nil {
			return err
		}
	}
	return nil
}

// deleteBarbicanAuthDelegatorBinding removes the cluster-scoped ClusterRoleBinding
// that lets a dedicated OpenBao instance run its TokenReviews. It is the one child
// the ControlPlane places outside every namespace, and the per-namespace sweeps
// reach it only when Barbican was assigned a namespace of its own. Co-located in
// the ControlPlane's own namespace it has no sweep at all: an owner reference
// cannot cross from a namespaced owner into cluster scope, so the GC cascade skips
// it too, and the binding would outlive the ControlPlane under a name a
// same-named ControlPlane later adopts.
//
// Only a binding carrying this ControlPlane's ownership labels is deleted. The name
// is cluster-wide, so a collision is not confined to one namespace.
//
// It is deleted on the cluster Barbican was placed on, where the ensemble wrote it,
// and on the management cluster for a Barbican that names none. A cluster that does
// not resolve leaves the binding where it is: teardownDedicatedNamespaces has
// already decided whether that cluster is being waited for or was abandoned, and
// this is the same unreachable cluster. A cluster that denies the delete leaves it
// too, reported as an event — the write grants are opt-in on the target's access
// chart, and no retry turns a withdrawn grant into a delete this ControlPlane may
// wait for.
func (r *ControlPlaneReconciler) deleteBarbicanAuthDelegatorBinding(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) error {
	children, _ := commonmulticluster.ResolveChildrenClientForDeletion(ctx, r.Resolver, r.Client,
		targetClusterRefForNamespace(cp, cp.BarbicanNamespace()), *cp.DeletionTimestamp)
	if children == nil {
		return nil
	}

	name := barbicanOpenBaoAuthDelegatorName(barbicanOpenBaoName(cp), cp.BarbicanNamespace())
	binding := &rbacv1.ClusterRoleBinding{}
	// Uncached (see ControlPlaneReconciler.APIReader): this runs on EVERY
	// ControlPlane teardown, Barbican or not, and a cached read would install a
	// cluster-wide ClusterRoleBinding informer for one object.
	switch err := r.teardownReader(children).Get(ctx, types.NamespacedName{Name: name}, binding); {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("getting ClusterRoleBinding %q for teardown: %w", name, err)
	}
	if !isControlPlaneChild(binding, cp) {
		return nil
	}
	if err := client.IgnoreNotFound(children.Delete(ctx, binding)); err != nil {
		// Forbidden is the read's failure one step further along, and the target
		// cluster's access chart makes it reachable: it grants get on
		// ClusterRoleBindings unconditionally, but create, patch and delete only
		// behind authDelegatorBinding. A cluster that had the flag on when this
		// binding was written and has it off now — a GitOps overlay, a decommission,
		// an upgrade that drops the override — answers the read with the binding and
		// the delete with a 403, on this pass and on every pass after it. Nothing
		// breaks that loop: the NamespaceTeardownStalled escape hatch lives inside
		// teardownDedicatedNamespaces, which has already returned done by the time
		// this runs. Wedging the ControlPlane in Terminating over a grant the target
		// cluster withdrew is the worse outcome, so it is released and what stays
		// behind is named — like the orphaned K-ORC resources, a repair-by-hand
		// outcome, and one that matters because a same-named ControlPlane would
		// otherwise adopt the binding left standing.
		if apierrors.IsForbidden(err) {
			r.Recorder.Event(cp, "Warning", "AuthDelegatorBindingNotReclaimed", fmt.Sprintf(
				"no permission to delete ClusterRoleBinding %q on the cluster Barbican was placed on; "+
					"releasing the ControlPlane anyway. It is cluster-scoped, so nothing will "+
					"garbage-collect it — remove it by hand, or set authDelegatorBinding=true on that "+
					"cluster's target-cluster-access release: %v", name, err))
			log.FromContext(ctx).Info("auth-delegator binding could not be reclaimed; releasing the "+
				"ControlPlane anyway", "clusterRoleBinding", name, "error", err.Error())
			return nil
		}
		return fmt.Errorf("deleting ClusterRoleBinding %q: %w", name, err)
	}
	return nil
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
//
// c selects the cluster: a placed namespace was created on the management cluster
// and on the target, so it is deleted once per cluster, each read and each
// ownership check answered by the cluster it is about to delete on.
//
// The read goes through that cluster's UNCACHED reader (teardownReader), as the
// adoption verdict on the same object does (ensureServiceNamespace). It is the
// read that decides whether a whole namespace — with the service's database, its
// PVC, and its tenant store in it — is deleted or leaked, and the marks it decides
// from are written on the target cluster by this very operator: a cache one resync
// behind would answer with a namespace that had not been stamped yet.
func (r *ControlPlaneReconciler) deleteManagedNamespace(
	ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane, name string,
) error {
	ns := &corev1.Namespace{}
	switch err := r.teardownReader(c).Get(ctx, types.NamespacedName{Name: name}, ns); {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("getting managed service namespace %q for teardown: %w", name, err)
	}
	if !ns.DeletionTimestamp.IsZero() {
		return nil
	}
	// On a target cluster the ownership labels are not proof enough: they name a
	// ControlPlane by name and namespace, and a cluster registered by two
	// management clusters can carry two of those. reapableManagedNamespace refuses
	// one whose UID mark names the other ControlPlane there — and, unlike the
	// adoption verdict, still reaps one whose mark was stripped, because this is the
	// last pass anything makes over it.
	if !reapableManagedNamespace(c, ns, cp) {
		r.Recorder.Event(cp, "Warning", "NamespaceNotOwned", fmt.Sprintf(
			"namespace %q does not carry this ControlPlane's ownership marks, so it was NOT deleted even though "+
				"its lifecycle is Managed; the operator never destroys a namespace it did not create", name,
		))
		return nil
	}
	if err := client.IgnoreNotFound(c.Delete(ctx, ns)); err != nil {
		return fmt.Errorf("deleting managed service namespace %q: %w", name, err)
	}
	log.FromContext(ctx).Info("deleted managed service namespace", "namespace", name)
	return nil
}

// sweepExternalNamespaceResidue deletes, best-effort, the objects the ControlPlane
// placed in a namespace it does NOT own — the namespace itself must survive, so
// nothing cascades and every object has to be named. The set is deterministic
// (every name is derived from the ControlPlane), so nothing has to be discovered:
// the backing services, the admin-password and Keystone DB-credential material, the
// Glance, Placement, and Barbican DB-credential material, the Barbican secret store
// with the dedicated OpenBao ensemble behind it, and the tenant-store trio.
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
//
// c is the client of the cluster the namespace lives on, so a placed namespace's
// residue is read and deleted there. It runs before the label-selected sweep of
// the same namespace, which reaches everything this one does not enumerate but
// has no order to give.
func (r *ControlPlaneReconciler) sweepExternalNamespaceResidue(
	ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane, namespace string,
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
	// The Glance credential material, which follows the Glance service: the
	// DB-credential ExternalSecret plus, in Dynamic mode, the VaultDynamicSecret
	// generator, its mTLS client Certificate, and the ServiceAccount whose token it
	// authenticates with.
	if cp.GlanceNamespace() == namespace {
		objs = append(
			objs,
			&esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
				Name: glanceDBCredentialSecretName(cp), Namespace: namespace,
			}},
			&esgenv1alpha1.VaultDynamicSecret{ObjectMeta: metav1.ObjectMeta{
				Name: glanceDBCredentialSecretName(cp), Namespace: namespace,
			}},
			unstructuredIn(certificateGVK, glanceDBCredentialClientCertName(cp)),
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
				Name: glanceDBCredentialServiceAccountName, Namespace: namespace,
			}},
		)
	}
	// The Placement credential material, which follows the Placement service, in the
	// same four shapes as Glance's above.
	if cp.PlacementNamespace() == namespace {
		objs = append(
			objs,
			&esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
				Name: placementDBCredentialSecretName(cp), Namespace: namespace,
			}},
			&esgenv1alpha1.VaultDynamicSecret{ObjectMeta: metav1.ObjectMeta{
				Name: placementDBCredentialSecretName(cp), Namespace: namespace,
			}},
			unstructuredIn(certificateGVK, placementDBCredentialClientCertName(cp)),
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
				Name: placementDBCredentialServiceAccountName, Namespace: namespace,
			}},
		)
	}
	// Everything that follows the Barbican service: the child and its secret store,
	// the dedicated OpenBao ensemble behind them (instance, tenant, transport
	// Certificates, provisioner account, TokenRequest grant, static-seal Secret, and
	// the cluster-scoped auth-delegator binding no namespace deletion reclaims), and
	// the DB-credential material in the same four shapes as Glance's above.
	if cp.BarbicanNamespace() == namespace {
		instance := barbicanOpenBaoName(cp)
		objs = append(
			objs,
			&barbicanv1alpha1.Barbican{ObjectMeta: metav1.ObjectMeta{
				Name: barbicanName(cp), Namespace: namespace,
			}},
			&barbicanv1alpha1.BarbicanSecretStore{ObjectMeta: metav1.ObjectMeta{
				Name: barbicanSecretStoreName(cp), Namespace: namespace,
			}},
			&openbaov1alpha1.OpenBaoCluster{ObjectMeta: metav1.ObjectMeta{
				Name: instance, Namespace: namespace,
			}},
		)
		objs = append(objs, barbicanOpenBaoEnsembleObjects(instance, namespace)...)
		objs = append(
			objs,
			&esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
				Name: barbicanDBCredentialSecretName(cp), Namespace: namespace,
			}},
			&esgenv1alpha1.VaultDynamicSecret{ObjectMeta: metav1.ObjectMeta{
				Name: barbicanDBCredentialSecretName(cp), Namespace: namespace,
			}},
			unstructuredIn(certificateGVK, barbicanDBCredentialClientCertName(cp)),
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
				Name: barbicanDBCredentialServiceAccountName, Namespace: namespace,
			}},
		)
	}
	// Any service-account credential delivered into this namespace: its source
	// Secret and consumer ExternalSecret, both operator-owned by label here. The
	// PushSecret was already deleted by deleteOwnedPushSecrets (before this ran, so
	// its OpenBao purge finished while the tenant store was still alive), and the
	// materialized credentials Secret is ESO-owned and dies with the ExternalSecret,
	// so neither is named here.
	for i := range cp.Spec.KORC.ServiceAccounts {
		sa := cp.Spec.KORC.ServiceAccounts[i]
		if serviceAccountDeliveryNamespace(cp, sa) != namespace {
			continue
		}
		objs = append(
			objs,
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: serviceAccountSourceSecretName(cp, sa), Namespace: namespace,
			}},
			&esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{
				Name: serviceAccountCredentialsSecretName(cp, sa), Namespace: namespace,
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

	reader := r.teardownReader(c)
	for _, obj := range objs {
		key := client.ObjectKeyFromObject(obj)
		// Uncached: the Barbican arm above adds the ensemble's three RBAC kinds
		// to this list (see ControlPlaneReconciler.APIReader), and a one-shot
		// teardown read has nothing to gain from an informer anyway.
		switch err := reader.Get(ctx, key, obj); {
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
		if err := client.IgnoreNotFound(c.Delete(ctx, obj)); err != nil {
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
//
// It sweeps every namespace the ControlPlane occupies and matches
// isControlPlaneChild, not a bare controller reference: a service-account
// credential delivered into a dedicated service namespace carries the ownership
// labels rather than an owner reference, and its DeletionPolicy=Delete OpenBao
// purge must run while that namespace's tenant store is still alive — which the
// existing ordering guarantees, since this runs before teardownDedicatedNamespaces
// (and its per-namespace tenant-store sweep).
//
// A placed namespace is swept on its target cluster as well as at home: the
// PushSecrets are written there, and so is the tenant store their purge
// authenticates through, so the ordering has to hold on that cluster too. The
// cluster is resolved through the DELETION resolver, which never fails the pass —
// one that does not resolve is left to teardownDedicatedNamespaces, which is where
// waiting for it and abandoning it are decided.
func (r *ControlPlaneReconciler) deleteOwnedPushSecrets(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) ([]pendingPushSecret, error) {
	var remaining []pendingPushSecret
	for _, namespace := range controlPlaneNamespaces(cp) {
		children, _ := commonmulticluster.ResolveChildrenClientForDeletion(ctx, r.Resolver, r.Client,
			targetClusterRefForNamespace(cp, namespace), *cp.DeletionTimestamp)
		for _, c := range r.clustersFor(children) {
			left, err := r.deleteOwnedPushSecretsIn(ctx, c, cp, namespace)
			if err != nil {
				return nil, err
			}
			remaining = append(remaining, left...)
		}
	}
	return remaining, nil
}

// pendingPushSecret is one PushSecret still present after the teardown sweep,
// paired with the client of the cluster it lives on. The stall escape strips its
// finalizers through that client: a PushSecret in a placed namespace exists on
// the target cluster alone, so an Update issued at home would reach no object.
type pendingPushSecret struct {
	pushSecret *esov1alpha1.PushSecret
	cluster    client.Client
}

// deleteOwnedPushSecretsIn is deleteOwnedPushSecrets for one namespace on the
// cluster c reads and writes, and returns the PushSecrets still present there
// afterwards. An absent PushSecret CRD (meta.IsNoMatchError) reads as nothing to
// clean on THAT cluster: a target that does not serve ESO can hold no PushSecret,
// and the management cluster's sweep is unaffected either way.
func (r *ControlPlaneReconciler) deleteOwnedPushSecretsIn(
	ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane, namespace string,
) ([]pendingPushSecret, error) {
	var list esov1alpha1.PushSecretList
	switch err := c.List(ctx, &list, client.InNamespace(namespace)); {
	case err == nil:
	case meta.IsNoMatchError(err):
		return nil, nil
	default:
		return nil, fmt.Errorf("listing owned PushSecrets in namespace %q for teardown: %w", namespace, err)
	}

	var remaining []pendingPushSecret
	for i := range list.Items {
		ps := &list.Items[i]
		if !isControlPlaneChild(ps, cp) {
			continue
		}
		if ps.DeletionTimestamp.IsZero() {
			if err := client.IgnoreNotFound(c.Delete(ctx, ps)); err != nil {
				return nil, fmt.Errorf("deleting PushSecret %q: %w", ps.Name, err)
			}
		}
		// Re-Get: a finalizer-less PushSecret is gone with the Delete, while one
		// held by ESO stays present until the remote delete is confirmed.
		current := &esov1alpha1.PushSecret{}
		switch err := c.Get(ctx, client.ObjectKeyFromObject(ps), current); {
		case err == nil:
			remaining = append(remaining, pendingPushSecret{pushSecret: current, cluster: c})
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
	case *orcv1alpha1.RoleAssignment:
		return o.Spec.ManagementPolicy != orcv1alpha1.ManagementPolicyUnmanaged
	case *orcv1alpha1.Role:
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
