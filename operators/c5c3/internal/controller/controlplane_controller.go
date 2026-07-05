// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the ControlPlane reconciler.
package controller

import (
	"context"
	"fmt"

	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/forge/internal/common/bootstrap"
	"github.com/c5c3/forge/internal/common/conditions"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// ControlPlaneSecretNameIndexKey is the field-indexer key under which ControlPlane
// CRs are indexed by the union of their referenced Secret names. Today that is the
// single EFFECTIVE admin-password Secret name in managed mode
// (Database.ClusterRef != nil) the operator-owned per-ControlPlane Secret
// adminPasswordSecretName(cp), and in brownfield mode the user-supplied
// spec.korc.adminCredential.passwordSecretRef.name. SetupWithManager registers the
// indexer and secretToControlPlaneMapper uses it for an O(1) reverse lookup from a
// Secret event to the referencing ControlPlane(s), mirroring the keystone operator's
// KeystoneSecretNameIndexKey. The constant's string value remains
// the spec passwordSecretRef field path because it is only an index-key identifier.
// #nosec G101 -- field-indexer key (a JSONPath-like field selector), not a credential.
const ControlPlaneSecretNameIndexKey = "spec.korc.adminCredential.passwordSecretRef.name"

// Condition types set by the ControlPlane controller. These constants are the
// single source of truth for the status contract: call sites (sub-reconcilers,
// setReadyCondition, the instrumentation map) MUST reference these constants
// rather than inline string literals so a rename is caught by the compiler and
// the no-inline-literals drift guard.
const (
	conditionTypeInfrastructureReady  = "InfrastructureReady"
	conditionTypeDBCredentialsReady   = "DBCredentialsReady" //nolint:gosec // G101 false positive: condition type name, not a credential.
	conditionTypeKeystoneReady        = "KeystoneReady"
	conditionTypeKORCReady            = "KORCReady"
	conditionTypeAdminCredentialReady = "AdminCredentialReady" //nolint:gosec // G101 false positive: condition type name, not a credential.
	conditionTypeAdminPasswordReady   = "AdminPasswordReady"   //nolint:gosec // G101 false positive: condition type name, not a credential.
	conditionTypeCatalogReady         = "CatalogReady"
	conditionTypeReady                = "Ready"
)

// controlPlaneORCFinalizer blocks the ControlPlane CR from leaving etcd until
// the operator has torn down the K-ORC CRs it owns
// (ApplicationCredential/Service/Endpoint/User/Domain). Those CRs carry K-ORC
// finalizers that revoke/delete against the Keystone API; holding the
// ControlPlane CR in etcd defers the owner-reference GC cascade that would
// otherwise tear Keystone (and its MariaDB) down concurrently, keeping Keystone
// reachable so K-ORC can finish. Defined once as the single source of truth for
// Reconcile, reconcileDelete, tests, and docs.
const controlPlaneORCFinalizer = "c5c3.io/orc-teardown"

// subConditionTypes lists the condition types set by individual sub-reconcilers.
// The Ready condition is True only when all of these are True.
var subConditionTypes = []string{
	conditionTypeInfrastructureReady,
	conditionTypeDBCredentialsReady,
	conditionTypeKeystoneReady,
	conditionTypeKORCReady,
	conditionTypeAdminCredentialReady,
	conditionTypeAdminPasswordReady,
	conditionTypeCatalogReady,
}

// ControlPlaneReconciler reconciles a ControlPlane object.
type ControlPlaneReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// MaxConcurrentReconciles bounds how many ControlPlane CRs reconcile
	// concurrently. It is threaded from the --max-concurrent-reconciles flag
	// (see internal/common/bootstrap) and applied to the controller's
	// controller.Options in SetupWithManager. A value <= 0 falls back to
	// bootstrap.DefaultMaxConcurrentReconciles inside
	// bootstrap.ControllerOptions, so the zero value is safe for
	// programmatically constructed reconcilers.
	MaxConcurrentReconciles int
}

// +kubebuilder:rbac:groups=c5c3.io,resources=controlplanes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=c5c3.io,resources=controlplanes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=controlplanes/finalizers,verbs=update
// +kubebuilder:rbac:groups=c5c3.io,resources=credentialrotations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=c5c3.io,resources=credentialrotations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=secretaggregates,verbs=get;list;watch
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=mariadbs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=memcached.c5c3.io,resources=memcacheds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keystone.openstack.c5c3.io,resources=keystones,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=openstack.k-orc.cloud,resources=applicationcredentials;services;endpoints;users;domains,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets;pushsecrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=external-secrets.io,resources=clustersecretstores,verbs=get;list;watch
// +kubebuilder:rbac:groups=generators.external-secrets.io,resources=vaultdynamicsecrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// Reconcile is the main reconciliation loop for the ControlPlane CR.
func (r *ControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Fetch the ControlPlane CR.
	var cp c5c3v1alpha1.ControlPlane
	if err := r.Get(ctx, req.NamespacedName, &cp); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("ControlPlane resource not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching ControlPlane: %w", err)
	}

	// Snapshot the persisted status so updateStatus can skip the write when a
	// pass leaves status unchanged (no write → no watch event → no
	// resourceVersion churn). Taken before any sub-reconciler or finalizer
	// mutates conditions.
	statusBefore := cp.Status.DeepCopy()

	// Handle deletion via the ORC-teardown finalizer: delete the operator-owned
	// K-ORC CRs first and hold the ControlPlane CR (which defers the owner-ref GC
	// cascade so Keystone/MariaDB stay reachable) until they disappear, then
	// release the finalizer so GC tears down the rest. reconcileDelete requeues
	// while ORC CRs are still Terminating; route that path through updateStatus so
	// the KORCReady=False/FinalizingORC condition is persisted. On the terminal
	// release path it returns a zero result and removes the finalizer, so skip the
	// status write — the CR is about to be garbage-collected. Deletion is handled
	// before the duplicate guard so a Terminating ControlPlane that carries the
	// finalizer always releases it (reconcileDelete is a no-op when the finalizer
	// is absent), instead of being parked and wedged.
	if !cp.DeletionTimestamp.IsZero() {
		if result, err := r.reconcileDelete(ctx, &cp); !result.IsZero() || err != nil {
			return r.updateStatus(ctx, &cp, statusBefore, result, err)
		}
		return ctrl.Result{}, nil
	}

	// Defense-in-depth for the one-ControlPlane-per-namespace contract
	// the validating webhook rejects duplicate CREATEs,
	// but CRs that predate the guard, raced through the API server, or were
	// written with the webhook bypassed can still coexist. Park every
	// ControlPlane except the oldest so two reconcilers never operate on the
	// namespace's shared credential paths and child resources at once —
	// mirroring the CredentialRotation reconciler's AmbiguousControlPlane
	// handling.
	incumbent, err := r.duplicateControlPlaneIncumbent(ctx, &cp)
	if err != nil {
		return ctrl.Result{}, err
	}
	if incumbent != "" {
		return r.parkDuplicateControlPlane(ctx, &cp, incumbent)
	}

	// Ensure the ORC-teardown finalizer is installed before any sub-reconciler
	// projects a K-ORC CR, so a deletion issued between now and the next pass
	// still funnels through reconcileDelete. Installed after the duplicate guard
	// so only the active incumbent — the ControlPlane that actually projects K-ORC
	// CRs — carries the finalizer; parked duplicates return above and never need
	// it. Returning Requeue=true after the Update guarantees the next reconcile
	// observes the persisted finalizer.
	if !controllerutil.ContainsFinalizer(&cp, controlPlaneORCFinalizer) {
		controllerutil.AddFinalizer(&cp, controlPlaneORCFinalizer)
		if err := r.Update(ctx, &cp); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Run the sub-reconciler pipeline in dependency order via the shared
	// table-driven chain: the first step to return a non-zero result or an
	// error short-circuits and funnels through updateStatus, so conditions and
	// the requeue/error are persisted by construction on every exit path.
	// Every step is routed through instrumentSubReconciler so that duration
	// samples and error counters are emitted under a stable sub_reconciler
	// label. AdminPassword runs BEFORE Keystone: the keystone-operator's
	// SecretsReady gate needs the admin-password ExternalSecret to exist
	// before the projected Keystone child references it.
	pipeline := []commonreconcile.Step{
		{Name: "Infrastructure", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileInfrastructure(ctx, &cp)
		}},
		{Name: "DBCredentials", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDBCredentials(ctx, &cp)
		}},
		{Name: "AdminPassword", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileAdminPassword(ctx, &cp)
		}},
		{Name: "Keystone", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileKeystone(ctx, &cp)
		}},
		{Name: "KORC", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileKORC(ctx, &cp)
		}},
		{Name: "AdminCredential", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileAdminCredential(ctx, &cp)
		}},
		{Name: "Catalog", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileCatalog(ctx, &cp)
		}},
	}

	result, err := commonreconcile.RunPipeline(ctx, instrumentSubReconciler, pipeline)
	return r.updateStatus(ctx, &cp, statusBefore, result, err)
}

// updateStatus persists the current status conditions and returns the given
// result and error, delegating to commonreconcile.UpdateStatus: the write is
// skipped when the pass left status semantically unchanged from the
// statusBefore snapshot (no write → no watch event → no resourceVersion
// churn), and a failed write is joined with reconcileErr so the original
// reconcile failure stays visible. The mutate hook recomputes the aggregate
// Ready condition on EVERY status write — including the in-progress/
// early-return paths where a sub-reconciler requeued before the chain
// converged — projects status.services/status.updatePhase via
// setServicesStatus, and stamps status.observedGeneration so a stale status
// is distinguishable from a current one.
func (r *ControlPlaneReconciler) updateStatus(ctx context.Context, cp *c5c3v1alpha1.ControlPlane, statusBefore *c5c3v1alpha1.ControlPlaneStatus, result ctrl.Result, reconcileErr error) (ctrl.Result, error) {
	return commonreconcile.UpdateStatus(ctx, r.Client, cp, statusBefore, &cp.Status, func() {
		setReadyCondition(cp)
		setServicesStatus(cp)
		cp.Status.ObservedGeneration = cp.Generation
	}, result, reconcileErr)
}

// setReadyCondition sets the aggregate Ready condition based on all
// sub-conditions, delegating to the shared aggregation helper with the
// ControlPlane sub-condition vocabulary. conditions.AllTrue checks only the
// subConditionTypes, not the Ready condition itself, so this is not
// self-referential.
func setReadyCondition(cp *c5c3v1alpha1.ControlPlane) {
	commonreconcile.SetAggregateReady(&cp.Status.Conditions, cp.Generation, subConditionTypes)
}

// duplicateControlPlaneIncumbent returns the name of the ControlPlane that owns
// cp's namespace when cp is NOT it — i.e. when cp must be parked. The owner is
// the oldest ControlPlane in the namespace by CreationTimestamp, with the
// lexically smallest Name breaking creation-time ties, so every evaluation
// deterministically picks the same incumbent. An empty string means cp itself
// is the incumbent (or the only ControlPlane) and reconciliation may proceed.
// The List goes through the informer cache: unlike the admission-time webhook
// check this guard runs on every reconcile, so eventual consistency is enough
// — a briefly stale cache only delays the parking by one requeue.
func (r *ControlPlaneReconciler) duplicateControlPlaneIncumbent(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (string, error) {
	var cps c5c3v1alpha1.ControlPlaneList
	if err := r.List(ctx, &cps, client.InNamespace(cp.Namespace)); err != nil {
		return "", fmt.Errorf("listing ControlPlanes in namespace %q for the duplicate guard: %w", cp.Namespace, err)
	}
	incumbent := cp
	for i := range cps.Items {
		other := &cps.Items[i]
		if other.UID == cp.UID {
			continue
		}
		if other.CreationTimestamp.Before(&incumbent.CreationTimestamp) ||
			(other.CreationTimestamp.Equal(&incumbent.CreationTimestamp) && other.Name < incumbent.Name) {
			incumbent = other
		}
	}
	if incumbent.UID == cp.UID {
		return "", nil
	}
	return incumbent.Name, nil
}

// parkDuplicateControlPlane sets Ready=False with reason DuplicateControlPlane
// naming the incumbent, persists the status, and requeues. It deliberately
// bypasses updateStatus: setReadyCondition would recompute Ready from the
// sub-conditions and overwrite the DuplicateControlPlane reason. The periodic
// requeue lets the parked CR take over automatically once the incumbent is
// fully deleted — no watch event fires on the duplicate's behalf when that
// happens.
func (r *ControlPlaneReconciler) parkDuplicateControlPlane(ctx context.Context, cp *c5c3v1alpha1.ControlPlane, incumbent string) (ctrl.Result, error) {
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: cp.Generation,
		Reason:             "DuplicateControlPlane",
		Message: fmt.Sprintf(
			"parked: ControlPlane %q is older and owns namespace %q; only one ControlPlane is permitted per namespace",
			incumbent, cp.Namespace,
		),
	})
	cp.Status.ObservedGeneration = cp.Generation
	if err := r.Status().Update(ctx, cp); err != nil {
		log.FromContext(ctx).Error(err, "unable to update ControlPlane status")
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}
	return ctrl.Result{RequeueAfter: duplicateControlPlaneRequeueAfter}, nil
}

// keystoneServiceKey is the key under which status.services reports the
// projected Keystone service readiness.
const keystoneServiceKey = "keystone"

// setServicesStatus records status.services and status.updatePhase on every
// status write (#476). Both fields were declared on ControlPlaneStatus but never
// written. status.updatePhase is fixed at Idle until the release-update state
// machine is implemented — the other UpdatePhase values are reserved (see the
// UpdatePhase DECISION comment), and "no update in progress" is the honest L1
// state. status.services maps each projected service to its observed readiness,
// derived from the corresponding sub-condition, with the release the service is
// being driven to.
func setServicesStatus(cp *c5c3v1alpha1.ControlPlane) {
	cp.Status.UpdatePhase = c5c3v1alpha1.UpdatePhaseIdle
	// Only report the Keystone service when it is actually managed by this
	// ControlPlane (spec.services.keystone set). When unset the ControlPlane
	// manages no Keystone, so status.services stays empty rather than reporting a
	// service that does not exist.
	if cp.Spec.Services.Keystone == nil {
		cp.Status.Services = nil
		return
	}
	cp.Status.Services = []c5c3v1alpha1.ServiceStatus{
		{
			Name:    keystoneServiceKey,
			Ready:   conditions.AllTrue(cp.Status.Conditions, conditionTypeKeystoneReady),
			Release: cp.Spec.OpenStackRelease,
		},
	}
}

// controlPlaneSecretNameExtractor is the controller-runtime IndexerFunc registered
// under ControlPlaneSecretNameIndexKey. It returns the deduplicated, non-empty
// set of Secret names a ControlPlane CR references — currently only the EFFECTIVE
// admin-password Secret name the operator-owned per-ControlPlane
// Secret name in managed mode, the user-supplied spec.korc.adminCredential
// .passwordSecretRef.name in brownfield mode — so the field indexer can resolve a
// Secret event to the referencing CR(s) without listing every ControlPlane in the
// namespace.
func controlPlaneSecretNameExtractor(obj client.Object) []string {
	cp, ok := obj.(*c5c3v1alpha1.ControlPlane)
	if !ok {
		// controller-runtime should never call us with the wrong type; a nil
		// return is safer than a panic if it ever does.
		return nil
	}
	name := effectiveAdminPasswordSecretRef(cp).Name
	if name == "" {
		return []string{}
	}
	return []string{name}
}

// registerControlPlaneSecretNameIndex registers the ControlPlane field indexer
// under ControlPlaneSecretNameIndexKey with the given FieldIndexer.
// SetupWithManager calls this once against mgr.GetFieldIndexer() so
// secretToControlPlaneMapper can resolve a Secret event to the referencing
// ControlPlane CRs via an O(1) reverse lookup. The returned error is wrapped with
// the index key so the registration site is identifiable in manager-startup
// failure logs.
func registerControlPlaneSecretNameIndex(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &c5c3v1alpha1.ControlPlane{}, ControlPlaneSecretNameIndexKey, controlPlaneSecretNameExtractor); err != nil {
		return fmt.Errorf("registering field indexer %q: %w", ControlPlaneSecretNameIndexKey, err)
	}
	return nil
}

// secretToControlPlaneMapper returns a MapFunc that maps Secret events to
// reconcile requests for ControlPlane CRs that reference the Secret by name,
// resolved via the ControlPlaneSecretNameIndexKey field indexer. The admin
// password Secret is typically ESO-managed (owned by the ExternalSecret
// controller, not the ControlPlane), so an owner-ref watch would never match it;
// the index-backed namespace-scoped List is what wakes the ControlPlane when its
// admin password rotates. On a List error the mapper logs via
// log.FromContext and returns nil per the handler.MapFunc contract, mirroring
// secretToKeystoneMapper.
func secretToControlPlaneMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var cps c5c3v1alpha1.ControlPlaneList
		if err := c.List(
			ctx, &cps,
			client.InNamespace(obj.GetNamespace()),
			client.MatchingFields{ControlPlaneSecretNameIndexKey: obj.GetName()},
		); err != nil {
			log.FromContext(ctx).Error(err, "listing ControlPlane CRs for secret watch")
			return nil
		}

		if len(cps.Items) == 0 {
			return nil
		}
		requests := make([]reconcile.Request, 0, len(cps.Items))
		for i := range cps.Items {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&cps.Items[i]),
			})
		}
		return requests
	}
}

// clusterSecretStoreToControlPlaneMapper returns a MapFunc that enqueues every
// ControlPlane in the cluster when the OpenBao-backed ClusterSecretStore
// changes. The store is cluster-scoped and shared across namespaces, so any
// status transition (e.g. ESO losing the backend connection) must retrigger
// reconcile on all ControlPlanes that route credentials through it; otherwise
// DBCredentialsReady / AdminPasswordReady / AdminCredentialReady would stay
// stale-True until the next periodic resync. Mirrors the keystone operator's
// clusterSecretStoreToKeystoneMapper (#476). On a List error the mapper logs and
// returns nil per the handler.MapFunc contract.
func clusterSecretStoreToControlPlaneMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		if obj.GetName() != openBaoClusterStoreName {
			return nil
		}

		var cps c5c3v1alpha1.ControlPlaneList
		if err := c.List(ctx, &cps); err != nil {
			log.FromContext(ctx).Error(err, "listing ControlPlane CRs for ClusterSecretStore watch")
			return nil
		}

		requests := make([]reconcile.Request, 0, len(cps.Items))
		for i := range cps.Items {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&cps.Items[i]),
			})
		}
		return requests
	}
}

// SetupWithManager registers the ControlPlaneReconciler with the controller
// manager. It Owns every child CR the sub-reconcilers project (MariaDB,
// Keystone, the K-ORC ApplicationCredential/Service/Endpoint, the Memcached CR,
// and the ESO ExternalSecret/PushSecret) so an upstream child status transition
// retriggers reconcile, Watches Secrets so an admin-password rotation wakes the
// owning ControlPlane via the field indexer, and Watches the OpenBao-backed
// ClusterSecretStore so an ESO/OpenBao outage reflects in the credential
// conditions promptly rather than after the next periodic resync (#476).
//
// DECISION (Memcached Owns): memcached.c5c3.io ships no Go module (see
// memcachedGVK in reconcile_infrastructure.go), so the Memcached child is owned
// as an *unstructured.Unstructured carrying that shared GVK rather than a typed
// client object — exactly how ensureMemcached creates it. The same memcachedGVK
// constant is reused so the watch and the create-or-update agree on the kind.
//
// DECISION (ESO Owns): the ExternalSecret and PushSecret children are owned via
// SetControllerReference by the DB-credential, admin-password and K-ORC
// sub-reconcilers, so Owns() is the direct wiring (no name-based mapper needed).
// It wakes the ControlPlane on every ESO status tick, which is acceptable for
// the goal of reflecting ESO sync/outage transitions in the credential
// conditions promptly; a relevance predicate could be added later if the
// reconcile volume becomes a concern.
func (r *ControlPlaneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Register the field indexer before Watches so secretToControlPlaneMapper can
	// rely on it for its MatchingFields lookup.
	if err := registerControlPlaneSecretNameIndex(context.Background(), mgr.GetFieldIndexer()); err != nil {
		return err
	}

	memcached := &unstructured.Unstructured{}
	memcached.SetGroupVersionKind(memcachedGVK)

	// The per-ControlPlane mTLS client Certificate is Owned as an
	// *unstructured.Unstructured carrying certificateGVK (mirroring the Memcached
	// Owns) so the c5c3 operator takes no cert-manager Go dependency.
	certificate := &unstructured.Unstructured{}
	certificate.SetGroupVersionKind(certificateGVK)

	return ctrl.NewControllerManagedBy(mgr).
		// Shared controller options: MaxConcurrentReconciles lets independent
		// CRs reconcile in parallel instead of serialising at the
		// controller-runtime default of 1, and the tuned RateLimiter caps
		// per-item failure backoff at 30s rather than the default 1000s (see
		// bootstrap.ControllerOptions).
		WithOptions(bootstrap.ControllerOptions(r.MaxConcurrentReconciles)).
		For(&c5c3v1alpha1.ControlPlane{}).
		Owns(&mariadbv1alpha1.MariaDB{}).
		Owns(&keystonev1alpha1.Keystone{}).
		Owns(&orcv1alpha1.ApplicationCredential{}).
		Owns(&orcv1alpha1.Service{}).
		Owns(&orcv1alpha1.Endpoint{}).
		Owns(&orcv1alpha1.User{}).
		Owns(&orcv1alpha1.Domain{}).
		Owns(memcached).
		Owns(certificate).
		Owns(&esov1.ExternalSecret{}).
		Owns(&esov1alpha1.PushSecret{}).
		Owns(&esgenv1alpha1.VaultDynamicSecret{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(
			secretToControlPlaneMapper(mgr.GetClient()),
		)).
		Watches(&esov1.ClusterSecretStore{}, handler.EnqueueRequestsFromMapFunc(
			clusterSecretStoreToControlPlaneMapper(mgr.GetClient()),
		)).
		Complete(r)
}
