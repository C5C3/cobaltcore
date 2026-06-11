// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the ControlPlane reconciler (CC-0110).
package controller

import (
	"context"
	"errors"
	"fmt"

	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/forge/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// ControlPlaneSecretNameIndexKey is the field-indexer key under which ControlPlane
// CRs are indexed by the union of their referenced Secret names. Today that is the
// single EFFECTIVE admin-password Secret name (CC-0117, REQ-005): in managed mode
// (Database.ClusterRef != nil) the operator-owned per-ControlPlane Secret
// adminPasswordSecretName(cp), and in brownfield mode the user-supplied
// spec.korc.adminCredential.passwordSecretRef.name. SetupWithManager registers the
// indexer and secretToControlPlaneMapper uses it for an O(1) reverse lookup from a
// Secret event to the referencing ControlPlane(s), mirroring the keystone operator's
// KeystoneSecretNameIndexKey (CC-0110, REQ-012). The constant's string value remains
// the spec passwordSecretRef field path because it is only an index-key identifier.
// #nosec G101 -- field-indexer key (a JSONPath-like field selector), not a credential.
const ControlPlaneSecretNameIndexKey = "spec.korc.adminCredential.passwordSecretRef.name"

// Condition types set by the ControlPlane controller. These constants are the
// single source of truth for the status contract: call sites (sub-reconcilers,
// setReadyCondition, the instrumentation map) MUST reference these constants
// rather than inline string literals so a rename is caught by the compiler and
// the no-inline-literals drift guard (CC-0110, REQ-007).
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

// subConditionTypes lists the condition types set by individual sub-reconcilers.
// The Ready condition is True only when all of these are True (CC-0110, REQ-007).
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
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
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

	// Run sub-reconcilers in dependency order. Every sub-reconciler call is
	// routed through instrumentSubReconciler so that duration samples and error
	// counters are emitted under a stable sub_reconciler label (CC-0110,
	// REQ-007, REQ-026).
	if result, err := instrumentSubReconciler(ctx, "Infrastructure", func(ctx context.Context) (ctrl.Result, error) {
		return r.reconcileInfrastructure(ctx, &cp)
	}); !result.IsZero() || err != nil {
		return r.updateStatus(ctx, &cp, result, err)
	}

	if result, err := instrumentSubReconciler(ctx, "DBCredentials", func(ctx context.Context) (ctrl.Result, error) {
		return r.reconcileDBCredentials(ctx, &cp)
	}); !result.IsZero() || err != nil {
		return r.updateStatus(ctx, &cp, result, err)
	}

	// AdminPassword runs BEFORE Keystone (CC-0117): the keystone-operator's
	// SecretsReady gate needs the admin-password ExternalSecret to exist before the
	// projected Keystone child references it.
	if result, err := instrumentSubReconciler(ctx, "AdminPassword", func(ctx context.Context) (ctrl.Result, error) {
		return r.reconcileAdminPassword(ctx, &cp)
	}); !result.IsZero() || err != nil {
		return r.updateStatus(ctx, &cp, result, err)
	}

	if result, err := instrumentSubReconciler(ctx, "Keystone", func(ctx context.Context) (ctrl.Result, error) {
		return r.reconcileKeystone(ctx, &cp)
	}); !result.IsZero() || err != nil {
		return r.updateStatus(ctx, &cp, result, err)
	}

	if result, err := instrumentSubReconciler(ctx, "KORC", func(ctx context.Context) (ctrl.Result, error) {
		return r.reconcileKORC(ctx, &cp)
	}); !result.IsZero() || err != nil {
		return r.updateStatus(ctx, &cp, result, err)
	}

	if result, err := instrumentSubReconciler(ctx, "AdminCredential", func(ctx context.Context) (ctrl.Result, error) {
		return r.reconcileAdminCredential(ctx, &cp)
	}); !result.IsZero() || err != nil {
		return r.updateStatus(ctx, &cp, result, err)
	}

	if result, err := instrumentSubReconciler(ctx, "Catalog", func(ctx context.Context) (ctrl.Result, error) {
		return r.reconcileCatalog(ctx, &cp)
	}); !result.IsZero() || err != nil {
		return r.updateStatus(ctx, &cp, result, err)
	}

	return r.updateStatus(ctx, &cp, ctrl.Result{}, nil)
}

// updateStatus persists the current status conditions and returns the given
// result and error. cp.Status.ObservedGeneration is set to the current
// generation so a stale status is distinguishable from a current one. When both
// reconcileErr and the status update fail, both errors are preserved via
// errors.Join so that the original reconcile failure is visible in
// controller-runtime logs (CC-0110, REQ-007).
func (r *ControlPlaneReconciler) updateStatus(ctx context.Context, cp *c5c3v1alpha1.ControlPlane, result ctrl.Result, reconcileErr error) (ctrl.Result, error) {
	// Recompute the aggregate Ready condition on EVERY status write — including
	// the in-progress/early-return paths where a sub-reconciler requeued before
	// the chain converged — so Status.Ready reflects the current aggregate state
	// (Ready=False while converging) rather than remaining absent until the full
	// chain passes (CC-0110, REQ-007). conditions.AllTrue checks only the
	// subConditionTypes, not the Ready condition itself, so this is not
	// self-referential.
	setReadyCondition(cp)
	cp.Status.ObservedGeneration = cp.Generation
	if err := r.Status().Update(ctx, cp); err != nil {
		log.FromContext(ctx).Error(err, "unable to update ControlPlane status")
		return ctrl.Result{}, errors.Join(reconcileErr, fmt.Errorf("updating status: %w", err))
	}
	return result, reconcileErr
}

// setReadyCondition sets the aggregate Ready condition based on all sub-conditions.
func setReadyCondition(cp *c5c3v1alpha1.ControlPlane) {
	if conditions.AllTrue(cp.Status.Conditions, subConditionTypes...) {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             "AllReady",
			Message:            "All sub-conditions are ready",
		})
	} else {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "NotAllReady",
			Message:            "One or more sub-conditions are not ready",
		})
	}
}

// controlPlaneSecretNameExtractor is the controller-runtime IndexerFunc registered
// under ControlPlaneSecretNameIndexKey. It returns the deduplicated, non-empty
// set of Secret names a ControlPlane CR references — currently only the EFFECTIVE
// admin-password Secret name (CC-0117, REQ-005): the operator-owned per-ControlPlane
// Secret name in managed mode, the user-supplied spec.korc.adminCredential
// .passwordSecretRef.name in brownfield mode — so the field indexer can resolve a
// Secret event to the referencing CR(s) without listing every ControlPlane in the
// namespace (CC-0110, REQ-012).
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
// failure logs (CC-0110, REQ-012).
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
// admin password rotates (CC-0110, REQ-012). On a List error the mapper logs via
// log.FromContext and returns nil per the handler.MapFunc contract, mirroring
// secretToKeystoneMapper.
func secretToControlPlaneMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var cps c5c3v1alpha1.ControlPlaneList
		if err := c.List(ctx, &cps,
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

// SetupWithManager registers the ControlPlaneReconciler with the controller
// manager. It Owns every child CR the sub-reconcilers project (MariaDB,
// Keystone, the K-ORC ApplicationCredential/Service/Endpoint, and the Memcached
// CR) so an upstream child status transition retriggers reconcile, and Watches
// Secrets so an admin-password rotation wakes the owning ControlPlane via the
// field indexer (CC-0110, REQ-012).
//
// DECISION (Memcached Owns): memcached.c5c3.io ships no Go module (see
// memcachedGVK in reconcile_infrastructure.go), so the Memcached child is owned
// as an *unstructured.Unstructured carrying that shared GVK rather than a typed
// client object — exactly how ensureMemcached creates it. The same memcachedGVK
// constant is reused so the watch and the create-or-update agree on the kind.
func (r *ControlPlaneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Register the field indexer before Watches so secretToControlPlaneMapper can
	// rely on it for its MatchingFields lookup (CC-0110, REQ-012).
	if err := registerControlPlaneSecretNameIndex(context.Background(), mgr.GetFieldIndexer()); err != nil {
		return err
	}

	memcached := &unstructured.Unstructured{}
	memcached.SetGroupVersionKind(memcachedGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(&c5c3v1alpha1.ControlPlane{}).
		Owns(&mariadbv1alpha1.MariaDB{}).
		Owns(&keystonev1alpha1.Keystone{}).
		Owns(&orcv1alpha1.ApplicationCredential{}).
		Owns(&orcv1alpha1.Service{}).
		Owns(&orcv1alpha1.Endpoint{}).
		Owns(&orcv1alpha1.User{}).
		Owns(&orcv1alpha1.Domain{}).
		Owns(memcached).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(
			secretToControlPlaneMapper(mgr.GetClient()),
		)).
		Complete(r)
}
