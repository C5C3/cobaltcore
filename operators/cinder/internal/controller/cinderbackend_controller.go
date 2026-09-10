// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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

// conditionTypeConfigProjected reports whether the parent Cinder has rendered
// this satellite into the workload that reads it. Together with
// conditionTypeCredentialsReady it is what the aggregate Ready is derived from,
// for both satellite kinds.
const conditionTypeConfigProjected = "ConfigProjected"

// Condition reason constants of the two satellite controllers.
const (
	// conditionReasonCredentialsNotRequired is the CredentialsReady reason of an
	// NFS backend: the export is mounted with the pod's own identity, so there is
	// no credential to resolve and the gate is satisfied by construction. It is
	// still reported, because the Cinder-side projection gates on this very
	// condition and an absent one would read as "not ready yet".
	conditionReasonCredentialsNotRequired = "CredentialsNotRequired"
	// conditionReasonWaitingForParent is set while the referenced Cinder does
	// not exist: without it the cluster this satellite belongs on is unknown.
	conditionReasonWaitingForParent = "WaitingForParent"
	// conditionReasonConfigProjected and conditionReasonWaitingForProjection are
	// the two outcomes of the projection observation.
	conditionReasonConfigProjected      = "ConfigProjected"
	conditionReasonWaitingForProjection = "WaitingForProjection"
	// conditionReasonDetaching is the Ready reason of a deleting CinderBackend
	// held by the service-remove finalizer.
	conditionReasonDetaching = "Detaching"
)

// eventReasonServiceRemoveSkipped is recorded on a deleting CinderBackend whose
// parent Cinder is already gone: the service registry lives in that Cinder's
// database, so with the database's owner deleted there is no row left to remove
// and the finalizer is released unconditionally.
const eventReasonServiceRemoveSkipped = "ServiceRemoveSkipped"

// cinderBackendSubConditionTypes are the sub-conditions the aggregate Ready is
// derived from: SetAggregateReady requires every listed type present-and-True.
var cinderBackendSubConditionTypes = []string{
	conditionTypeCredentialsReady,
	conditionTypeConfigProjected,
}

// cinderBackendSkeleton carries the CinderBackend condition vocabulary and its
// status-conditions accessor, so the shared controller-skeleton glue (Ready
// aggregation, the write-only-on-change status persist) runs from one place.
var cinderBackendSkeleton = commonreconcile.Skeleton[*cinderv1alpha1.CinderBackend, cinderv1alpha1.CinderBackendStatus]{
	SubConditionTypes: cinderBackendSubConditionTypes,
	Conditions:        func(b *cinderv1alpha1.CinderBackend) *[]metav1.Condition { return &b.Status.Conditions },
}

// CinderBackendReconciler owns the CinderBackend CR lifecycle: the per-backend
// CredentialsReady / ConfigProjected / Ready conditions and the service-remove
// finalizer a detaching backend is held by. It is the SINGLE writer of
// CinderBackend status; the Cinder-side sub-reconciler only reads it (a
// credential-ready backend gates the projection) and writes an aggregated
// condition onto the Cinder CR instead.
//
// The finalizer is added here and removed by the Cinder-side volume step: the
// host identity to unregister lives in the parent's database, and only a Job
// running against that database can drop it (see
// CinderBackendServiceRemoveFinalizer). This controller only holds the CR and
// reports what it is waiting for — with one exception, a parent that no longer
// exists, where nothing can ever run the Job.
type CinderBackendReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// MaxConcurrentReconciles bounds how many backend CRs reconcile
	// concurrently; a value <= 0 falls back to the shared default.
	MaxConcurrentReconciles int

	// Resolver resolves the target cluster the PARENT Cinder names in
	// spec.targetClusterRef into the client this backend's observations read
	// from. A backend carries no target of its own: it exists to serve one
	// Cinder, so the Deployment it observes belongs wherever that Cinder's
	// workload runs. Nil means always-local.
	Resolver commonmulticluster.ClusterResolver
}

// +kubebuilder:rbac:groups=cinder.openstack.c5c3.io,resources=cinderbackends,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=cinder.openstack.c5c3.io,resources=cinderbackends/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cinder.openstack.c5c3.io,resources=cinders,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives one CinderBackend CR: hold it with the service-remove
// finalizer, report that its export needs no credentials, observe whether the
// parent's volume service mounts this backend's rendered section, and persist
// the aggregated status.
func (r *CinderBackendReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var backend cinderv1alpha1.CinderBackend
	if err := r.Get(ctx, req.NamespacedName, &backend); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("CinderBackend not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching CinderBackend: %w", err)
	}

	if backend.DeletionTimestamp != nil {
		return r.reconcileDeleting(ctx, &backend)
	}

	// The finalizer is installed before anything else: a backend deleted between
	// its creation and its first projection still has to be unregistered, and a
	// finalizer added after the projection would miss exactly that window.
	added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &backend, CinderBackendServiceRemoveFinalizer)
	if err != nil {
		return ctrl.Result{}, err
	}
	if added {
		// The Update bumped the resourceVersion; the next pass reconciles the
		// persisted object rather than writing status against a stale one.
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
	}

	statusBefore := backend.Status.DeepCopy()
	result, err := r.reconcileNormal(ctx, &backend)
	return r.updateStatus(ctx, &backend, statusBefore, result, err)
}

// reconcileDeleting reports what a detaching backend is waiting for and releases
// it when nothing can ever release it otherwise.
//
// The finalizer is removed by the Cinder-side volume step, which stops the
// volume service and runs the service-remove Job against the parent's database.
// A parent that no longer exists can run no Job — the database went with it, and
// with it the service registry row — so holding the backend would wedge it in
// Terminating forever. The removal is therefore unconditional in that case, and
// recorded as an event so the skipped unregistration is visible rather than
// silent.
func (r *CinderBackendReconciler) reconcileDeleting(ctx context.Context, backend *cinderv1alpha1.CinderBackend) (ctrl.Result, error) {
	// Without the finalizer this controller holds nothing: the CR is on its way
	// out and neither a status write nor an event would reach anyone.
	if !controllerutil.ContainsFinalizer(backend, CinderBackendServiceRemoveFinalizer) {
		return ctrl.Result{}, nil
	}

	var parent cinderv1alpha1.Cinder
	parentKey := client.ObjectKey{Namespace: backend.Namespace, Name: backend.Spec.CinderRef.Name}
	if err := r.Get(ctx, parentKey, &parent); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("fetching parent Cinder %s: %w", parentKey, err)
		}
		controllerutil.RemoveFinalizer(backend, CinderBackendServiceRemoveFinalizer)
		if err := r.Update(ctx, backend); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing the service-remove finalizer from %q: %w", backend.Name, err)
		}
		r.Recorder.Eventf(backend, corev1.EventTypeNormal, eventReasonServiceRemoveSkipped,
			"Parent Cinder %s no longer exists, so no service registry row can be removed; released the backend", parentKey)
		return ctrl.Result{}, nil
	}

	// The status write goes through the plain helper rather than the skeleton:
	// the skeleton re-derives Ready from the sub-conditions, which would replace
	// this reason with AllReady/NotAllReady and lose the one fact a detaching
	// backend has to report.
	statusBefore := backend.Status.DeepCopy()
	conditions.SetCondition(&backend.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: backend.Generation,
		Reason:             conditionReasonDetaching,
		Message: fmt.Sprintf("waiting for the parent Cinder to run the %s Job that unregisters this backend's volume service",
			serviceRemoveJobName(&parent, backend.Name)),
	})
	return commonreconcile.UpdateStatus(ctx, r.Client, backend, statusBefore, &backend.Status, func() {
		backend.Status.ObservedGeneration = backend.Generation
	}, ctrl.Result{}, nil)
}

// reconcileNormal reports the credential gate and observes the config
// projection. Both steps read objects that belong to the parent Cinder, so it
// opens by resolving that parent's target cluster into the children client.
func (r *CinderBackendReconciler) reconcileNormal(ctx context.Context, backend *cinderv1alpha1.CinderBackend) (ctrl.Result, error) {
	children, parent, result, err := r.resolveChildren(ctx, backend)
	if children == nil {
		return result, err
	}

	r.gateCredentials(backend)
	return r.observeConfigProjected(ctx, children, parent, backend)
}

// resolveChildren returns the client this backend's observations read from — the
// one the PARENT Cinder's spec.targetClusterRef selects — together with the
// parent it read on the way.
//
// A parent that does not exist holds the pass on CredentialsReady=False and
// requeues, the same treatment an unresolvable target gets: it is the parent's
// targetClusterRef that decides which cluster the volume service runs on, so
// falling back to the local client would observe the wrong one for the whole
// window between a GitOps apply landing the backend and the same apply landing
// its Cinder.
//
// A missing parent also demotes a standing ConfigProjected=True: that condition
// claims a volume Deployment mounts this backend's rendered section, and with
// the parent gone the claim is stale. A backend that never converged keeps the
// condition absent, and an unresolvable target demotes nothing — unobservable is
// not un-projected.
func (r *CinderBackendReconciler) resolveChildren(ctx context.Context, backend *cinderv1alpha1.CinderBackend) (
	client.Client, *cinderv1alpha1.Cinder, ctrl.Result, error,
) {
	var parent cinderv1alpha1.Cinder
	parentKey := client.ObjectKey{Namespace: backend.Namespace, Name: backend.Spec.CinderRef.Name}
	children, result, err := satellite.ResolveParentChildren(ctx, satellite.ResolveParams{
		Client:                    r.Client,
		Resolver:                  r.Resolver,
		Parent:                    &parent,
		ParentKey:                 parentKey,
		ParentKind:                "Cinder",
		TargetClusterRef:          func() *commonv1.TargetClusterRefSpec { return parent.Spec.TargetClusterRef },
		Conditions:                &backend.Status.Conditions,
		Generation:                backend.Generation,
		GateConditionType:         conditionTypeCredentialsReady,
		WaitingForParentReason:    conditionReasonWaitingForParent,
		WaitingForParentMessage:   fmt.Sprintf("parent Cinder %s does not exist; the cluster this backend's volume service runs on is unknown", parentKey),
		ProjectionConditionType:   conditionTypeConfigProjected,
		ProjectionDemotionReason:  conditionReasonWaitingForProjection,
		ProjectionDemotionMessage: fmt.Sprintf("parent Cinder %s does not exist; nothing mounts this backend's rendered section", parentKey),
		RequeueAfter:              commonreconcile.RequeueSecretPolling,
	})
	if children == nil {
		return nil, nil, result, err
	}
	return children, &parent, result, err
}

// gateCredentials reports CredentialsReady for an NFS export, which is ready by
// construction: the mount authenticates with the pod's own identity, so there is
// no Secret to resolve and nothing that could be missing. NFS is the only
// backend type this API admits; a type that carried credentials would gate on
// them here instead.
func (r *CinderBackendReconciler) gateCredentials(backend *cinderv1alpha1.CinderBackend) {
	r.setCondition(backend, conditionTypeCredentialsReady, metav1.ConditionTrue,
		conditionReasonCredentialsNotRequired,
		"the NFS export is mounted with the pod's own identity, so no credentials are required")
}

// observeConfigProjected derives the ConfigProjected condition from the single
// authoritative pointer: the backend's own volume Deployment and the rendered
// section inside the Secret it mounts. A False observation carries a
// commonreconcile.RequeueSecretPolling safety net — the Cinder watch normally
// wakes this controller when the projection lands, but a converged Cinder status
// (no write, no event) must not strand the backend. A transient Get failure
// returns the error WITHOUT demoting a currently-True condition, so a converged
// backend does not flap on a cache blip.
func (r *CinderBackendReconciler) observeConfigProjected(ctx context.Context, children client.Client,
	parent *cinderv1alpha1.Cinder, backend *cinderv1alpha1.CinderBackend,
) (ctrl.Result, error) {
	projected, err := r.isConfigProjected(ctx, children, parent, backend)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !projected {
		r.setCondition(backend, conditionTypeConfigProjected, metav1.ConditionFalse,
			conditionReasonWaitingForProjection,
			"waiting for this backend's cinder-volume Deployment to mount its rendered backend section")
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	}
	r.setCondition(backend, conditionTypeConfigProjected, metav1.ConditionTrue, conditionReasonConfigProjected,
		"the backend section is rendered into this backend's cinder-volume Deployment")
	return ctrl.Result{}, nil
}

// isConfigProjected reports whether this backend's volume Deployment mounts its
// rendered section: the backend-volume Secret's backend.conf carries the
// [<backend.Name>] section header. A missing Deployment, volume, Secret or key
// folds to (false, nil) — an authoritative "not projected yet". Only a
// non-NotFound client failure returns an error.
func (r *CinderBackendReconciler) isConfigProjected(ctx context.Context, children client.Client,
	parent *cinderv1alpha1.Cinder, backend *cinderv1alpha1.CinderBackend,
) (bool, error) {
	return satellite.SectionProjected(ctx, satellite.ObserveParams{
		Children: children,
		DeploymentKey: client.ObjectKey{
			Namespace: parent.Namespace,
			Name:      volumeDeploymentName(parent, backend.Name),
		},
		VolumeName:    backendsVolumeName,
		DataKey:       backendConfDataKey,
		SectionHeader: "[" + backend.Name + "]",
	})
}

// setCondition upserts one of the backend's own conditions at its generation.
func (r *CinderBackendReconciler) setCondition(backend *cinderv1alpha1.CinderBackend, conditionType string,
	status metav1.ConditionStatus, reason, message string,
) {
	conditions.SetCondition(&backend.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: backend.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// updateStatus persists the backend status via the shared helper: the write is
// skipped when the pass left status unchanged, the aggregate Ready is re-derived
// from the sub-condition set on every persist, and ObservedGeneration is
// stamped.
func (r *CinderBackendReconciler) updateStatus(ctx context.Context, backend *cinderv1alpha1.CinderBackend,
	statusBefore *cinderv1alpha1.CinderBackendStatus, result ctrl.Result, reconcileErr error,
) (ctrl.Result, error) {
	return cinderBackendSkeleton.UpdateStatus(ctx, r.Client, backend, statusBefore, &backend.Status, func() {
		backend.Status.ObservedGeneration = backend.Generation
	}, result, reconcileErr)
}

// SetupWithManager registers the CinderBackendReconciler with the controller
// manager. The CinderBackend field indexes are registered by
// CinderReconciler.SetupWithManager (the single registration site — every
// controller of this operator runs in one manager and that reconciler is set up
// first), so this controller registers none.
func (r *CinderBackendReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(bootstrap.ControllerOptions(r.MaxConcurrentReconciles)).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate).
		For(&cinderv1alpha1.CinderBackend{}, builder.WithPredicates(watch.CRUpdatePredicate())).
		// Watch the referenced Cinder WITHOUT a generation predicate: Cinder
		// status flips (the backend section landing in the volume Deployment) are
		// exactly the wake signal the ConfigProjected gate waits on. No own Secret
		// watch: an NFS backend references none.
		Watches(&cinderv1alpha1.Cinder{}, handler.EnqueueRequestsFromMapFunc(
			cinderToCinderBackendsMapper(mgr.GetClient()),
		)).
		Complete(r)
}
