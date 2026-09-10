// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/satellite"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// cinderBackupBackendSubConditionTypes are the sub-conditions the aggregate
// Ready is derived from. They are the CinderBackend vocabulary: both satellites
// answer the same two questions, so an operator reads one status shape whichever
// kind is in front of them.
var cinderBackupBackendSubConditionTypes = []string{
	conditionTypeCredentialsReady,
	conditionTypeConfigProjected,
}

// cinderBackupBackendSkeleton carries the CinderBackupBackend condition
// vocabulary and its status-conditions accessor, so the shared
// controller-skeleton glue (Ready aggregation, the write-only-on-change status
// persist) runs from one place.
var cinderBackupBackendSkeleton = commonreconcile.Skeleton[
	*cinderv1alpha1.CinderBackupBackend, cinderv1alpha1.CinderBackupBackendStatus,
]{
	SubConditionTypes: cinderBackupBackendSubConditionTypes,
	Conditions: func(b *cinderv1alpha1.CinderBackupBackend) *[]metav1.Condition {
		return &b.Status.Conditions
	},
}

// CinderBackupBackendReconciler owns the CinderBackupBackend CR lifecycle: the
// CredentialsReady / ConfigProjected / Ready conditions of the single backup
// target a Cinder writes its backups to. It is the SINGLE writer of
// CinderBackupBackend status; the Cinder-side sub-reconciler only reads it and
// writes an aggregated condition onto the Cinder CR instead.
//
// Deliberately there is NO finalizer: a backup backend holds no row in the
// service registry — cinder-backup registers under the Cinder's own host
// identity rather than per target — so a detaching backup backend leaves nothing
// to unregister. The parent re-aggregates through its watch and deletes the
// backup Deployment on its next pass.
type CinderBackupBackendReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// MaxConcurrentReconciles bounds how many backup-backend CRs reconcile
	// concurrently; a value <= 0 falls back to the shared default.
	MaxConcurrentReconciles int

	// Resolver resolves the target cluster the PARENT Cinder names in
	// spec.targetClusterRef into the client this backend's observations read
	// from. A backup backend carries no target of its own: it exists to serve one
	// Cinder, so the Deployment it observes belongs wherever that Cinder's
	// workload runs. Nil means always-local.
	Resolver commonmulticluster.ClusterResolver
}

// +kubebuilder:rbac:groups=cinder.openstack.c5c3.io,resources=cinderbackupbackends,verbs=get;list;watch
// +kubebuilder:rbac:groups=cinder.openstack.c5c3.io,resources=cinderbackupbackends/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cinder.openstack.c5c3.io,resources=cinders,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

// Reconcile drives one CinderBackupBackend CR: report that its export needs no
// credentials, observe whether the parent's backup service mounts this backend's
// projection, and persist the aggregated status. A deleting backend returns
// early doing nothing — there is nothing to clean up (see the type doc).
func (r *CinderBackupBackendReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var backupBackend cinderv1alpha1.CinderBackupBackend
	if err := r.Get(ctx, req.NamespacedName, &backupBackend); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("CinderBackupBackend not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching CinderBackupBackend: %w", err)
	}

	if backupBackend.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	statusBefore := backupBackend.Status.DeepCopy()
	result, err := r.reconcileNormal(ctx, &backupBackend)
	return r.updateStatus(ctx, &backupBackend, statusBefore, result, err)
}

// reconcileNormal reports the credential gate and observes the config
// projection. Both steps read objects that belong to the parent Cinder, so it
// opens by resolving that parent's target cluster into the children client.
func (r *CinderBackupBackendReconciler) reconcileNormal(ctx context.Context,
	backupBackend *cinderv1alpha1.CinderBackupBackend,
) (ctrl.Result, error) {
	children, parent, result, err := r.resolveChildren(ctx, backupBackend)
	if children == nil {
		return result, err
	}

	r.gateCredentials(backupBackend)
	return r.observeConfigProjected(ctx, children, parent, backupBackend)
}

// resolveChildren returns the client this backup backend's observation reads
// from — the one the PARENT Cinder's spec.targetClusterRef selects — together
// with the parent it read on the way. It follows the CinderBackend contract: a
// missing parent holds the pass on CredentialsReady and demotes a standing
// projection claim, an unresolvable target holds it without demoting anything.
func (r *CinderBackupBackendReconciler) resolveChildren(ctx context.Context,
	backupBackend *cinderv1alpha1.CinderBackupBackend,
) (client.Client, *cinderv1alpha1.Cinder, ctrl.Result, error) {
	var parent cinderv1alpha1.Cinder
	parentKey := client.ObjectKey{Namespace: backupBackend.Namespace, Name: backupBackend.Spec.CinderRef.Name}
	children, result, err := satellite.ResolveParentChildren(ctx, satellite.ResolveParams{
		Client:                    r.Client,
		Resolver:                  r.Resolver,
		Parent:                    &parent,
		ParentKey:                 parentKey,
		ParentKind:                "Cinder",
		TargetClusterRef:          func() *commonv1.TargetClusterRefSpec { return parent.Spec.TargetClusterRef },
		Conditions:                &backupBackend.Status.Conditions,
		Generation:                backupBackend.Generation,
		GateConditionType:         conditionTypeCredentialsReady,
		WaitingForParentReason:    conditionReasonWaitingForParent,
		WaitingForParentMessage:   fmt.Sprintf("parent Cinder %s does not exist; the cluster this backup target is written from is unknown", parentKey),
		ProjectionConditionType:   conditionTypeConfigProjected,
		ProjectionDemotionReason:  conditionReasonWaitingForProjection,
		ProjectionDemotionMessage: fmt.Sprintf("parent Cinder %s does not exist; nothing mounts this backup target's projection", parentKey),
		RequeueAfter:              commonreconcile.RequeueSecretPolling,
	})
	if children == nil {
		return nil, nil, result, err
	}
	return children, &parent, result, err
}

// gateCredentials reports CredentialsReady for an NFS export, which is ready by
// construction: the mount authenticates with the pod's own identity, so there is
// no Secret to resolve and nothing that could be missing. NFS is the only backup
// target type this API admits; a type that carried credentials would gate on
// them here instead.
func (r *CinderBackupBackendReconciler) gateCredentials(backupBackend *cinderv1alpha1.CinderBackupBackend) {
	r.setCondition(backupBackend, conditionTypeCredentialsReady, metav1.ConditionTrue,
		conditionReasonCredentialsNotRequired,
		"the NFS export is mounted with the pod's own identity, so no credentials are required")
}

// observeConfigProjected derives the ConfigProjected condition from the backup
// Deployment's projection. A False observation carries a
// commonreconcile.RequeueSecretPolling safety net — the Cinder watch normally
// wakes this controller when the projection lands, but a converged Cinder status
// (no write, no event) must not strand the backend. A transient Get failure
// returns the error WITHOUT demoting a currently-True condition, so a converged
// backend does not flap on a cache blip.
func (r *CinderBackupBackendReconciler) observeConfigProjected(ctx context.Context, children client.Client,
	parent *cinderv1alpha1.Cinder, backupBackend *cinderv1alpha1.CinderBackupBackend,
) (ctrl.Result, error) {
	projected, err := r.isConfigProjected(ctx, children, parent, backupBackend)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !projected {
		r.setCondition(backupBackend, conditionTypeConfigProjected, metav1.ConditionFalse,
			conditionReasonWaitingForProjection,
			"waiting for the cinder-backup Deployment to mount this backup target's rendered driver section")
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	}
	r.setCondition(backupBackend, conditionTypeConfigProjected, metav1.ConditionTrue,
		conditionReasonConfigProjected,
		"the driver section is rendered into the cinder-backup Deployment")
	return ctrl.Result{}, nil
}

// isConfigProjected reports whether the parent's cinder-backup Deployment mounts
// THIS backup target's projection: the Secret its backup volume names is the one
// rendered under this backend's base name.
//
// The name is the whole observation, where the CinderBackend controller reads a
// section header out of the file. One Cinder renders at most one backup target,
// so the rendered document has no per-backend section to look for — but every
// backup backend renders under a base name of its own (backupSecretBaseName), so
// the mounted Secret's name already says which of them the running service was
// built from. A missing Deployment or volume folds to (false, nil); only a
// non-NotFound client failure returns an error.
func (r *CinderBackupBackendReconciler) isConfigProjected(ctx context.Context, children client.Client,
	parent *cinderv1alpha1.Cinder, backupBackend *cinderv1alpha1.CinderBackupBackend,
) (bool, error) {
	key := client.ObjectKey{Namespace: parent.Namespace, Name: backupDeploymentName(parent)}
	var deploy appsv1.Deployment
	if err := children.Get(ctx, key, &deploy); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("fetching backup Deployment %s: %w", key, err)
	}

	secretName := satellite.SecretNameForVolume(&deploy.Spec.Template.Spec, backupVolumeName)
	if secretName == "" {
		return false, nil
	}
	// The projection Secret is named "<base>-<content hash>", so the base is
	// everything up to the last separator. Comparing it whole rather than by
	// prefix is what keeps a backend from reading a sibling's projection when one
	// base name is a prefix of the other: "cinder-backup-daily" is a prefix of
	// "cinder-backup-daily-archive-<hash>", separator included.
	separator := strings.LastIndex(secretName, "-")
	if separator < 0 {
		return false, nil
	}
	return secretName[:separator] == backupSecretBaseName(parent, backupBackend.Name), nil
}

// setCondition upserts one of the backup backend's own conditions at its
// generation.
func (r *CinderBackupBackendReconciler) setCondition(backupBackend *cinderv1alpha1.CinderBackupBackend,
	conditionType string, status metav1.ConditionStatus, reason, message string,
) {
	conditions.SetCondition(&backupBackend.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: backupBackend.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// updateStatus persists the backup backend status via the shared helper: the
// write is skipped when the pass left status unchanged, the aggregate Ready is
// re-derived from the sub-condition set on every persist, and ObservedGeneration
// is stamped.
func (r *CinderBackupBackendReconciler) updateStatus(ctx context.Context,
	backupBackend *cinderv1alpha1.CinderBackupBackend,
	statusBefore *cinderv1alpha1.CinderBackupBackendStatus, result ctrl.Result, reconcileErr error,
) (ctrl.Result, error) {
	return cinderBackupBackendSkeleton.UpdateStatus(ctx, r.Client, backupBackend, statusBefore,
		&backupBackend.Status, func() {
			backupBackend.Status.ObservedGeneration = backupBackend.Generation
		}, result, reconcileErr)
}

// SetupWithManager registers the CinderBackupBackendReconciler with the
// controller manager. The CinderBackupBackend field indexes are registered by
// CinderReconciler.SetupWithManager (the single registration site), so this
// controller registers none.
func (r *CinderBackupBackendReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.setupWithOptions(mgr, bootstrap.ControllerOptions(r.MaxConcurrentReconciles))
}

// setupWithOptions carries the production watch wiring SetupWithManager applies.
// The controller options are a parameter so an envtest integration suite can
// register this exact chain with SkipNameValidation set, rather than a hand-built
// copy of it that drifts the moment a leg is added here.
func (r *CinderBackupBackendReconciler) setupWithOptions(mgr ctrl.Manager, opts crcontroller.Options) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(opts).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate).
		For(&cinderv1alpha1.CinderBackupBackend{}, builder.WithPredicates(watch.CRUpdatePredicate())).
		// Watch the referenced Cinder WITHOUT a generation predicate: Cinder
		// status flips (the driver section landing in the backup Deployment) are
		// exactly the wake signal the ConfigProjected gate waits on.
		Watches(&cinderv1alpha1.Cinder{}, handler.EnqueueRequestsFromMapFunc(
			cinderToCinderBackupBackendsMapper(mgr.GetClient()),
		)).
		Complete(r)
}
