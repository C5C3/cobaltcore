// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
)

// Condition types the GlanceBackend controller owns. The aggregate Ready is
// True only when both are True (glanceBackendSubConditionTypes).
const (
	conditionTypeCredentialsReady = "CredentialsReady"
	conditionTypeConfigProjected  = "ConfigProjected"
)

// Condition reason constants for the CredentialsReady and ConfigProjected
// conditions.
const (
	// CredentialsReady reasons.
	conditionReasonCredentialsAvailable  = "CredentialsAvailable"
	conditionReasonWaitingForCredentials = "WaitingForCredentials"
	conditionReasonWaitingForParent      = "WaitingForParent"

	// ConfigProjected reasons.
	conditionReasonConfigProjected      = "ConfigProjected"
	conditionReasonWaitingForProjection = "WaitingForProjection"
)

// backendsVolumeName is the name of the Volume the glance-side deployment step
// mounts the rendered store config through. The GlanceBackend controller reads
// it back off the parent Glance Deployment to observe ConfigProjected, and the
// deployment step (next commit) writes it under the same name — a naming
// contract shared across both controllers.
const backendsVolumeName = "backends"

// backendsConfDataKey is the data key inside the projection Secret that holds
// the rendered backends config (the INI document with one [<backend>] store
// section per attached, credential-ready backend).
const backendsConfDataKey = "backends.conf"

// glanceBackendSubConditionTypes are the sub-conditions the aggregate Ready is
// derived from: SetAggregateReady requires every listed type present-and-True.
var glanceBackendSubConditionTypes = []string{
	conditionTypeCredentialsReady,
	conditionTypeConfigProjected,
}

// GlanceBackendReconciler owns the GlanceBackend CR lifecycle: S3 credential
// resolution and the per-backend CredentialsReady / ConfigProjected / Ready
// conditions. It is the SINGLE writer of GlanceBackend status; the glance-side
// sub-reconciler only reads it (a credential-ready backend gates config
// projection) and writes an aggregated condition onto the Glance CR instead.
//
// Deliberately there is NO finalizer and no deletion logic: an S3 backend owns
// no external resources, so deletion just detaches the store — the parent Glance
// re-aggregates its attached backends through its watch and de-projects the
// dropped section on its next pass.
type GlanceBackendReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// MaxConcurrentReconciles bounds how many backend CRs reconcile
	// concurrently; a value <= 0 falls back to the shared default.
	MaxConcurrentReconciles int

	// Resolver resolves the target cluster the PARENT Glance names in
	// spec.targetClusterRef into the client this backend's observations read
	// from. A backend carries no target of its own: it exists to serve one
	// Glance, so the credentials it gates on and the Deployment it observes
	// belong wherever that Glance's workload runs. Nil means always-local.
	Resolver commonmulticluster.ClusterResolver
}

// +kubebuilder:rbac:groups=glance.openstack.c5c3.io,resources=glancebackends,verbs=get;list;watch
// +kubebuilder:rbac:groups=glance.openstack.c5c3.io,resources=glancebackends/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=glance.openstack.c5c3.io,resources=glances,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives one GlanceBackend CR: gate on the S3 credentials Secret,
// observe whether the parent Glance Deployment mounts this backend's rendered
// store section, and persist the aggregated status. A deleting backend returns
// early doing nothing — there is nothing to clean up (see the type doc).
func (r *GlanceBackendReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var backend glancev1alpha1.GlanceBackend
	if err := r.Get(ctx, req.NamespacedName, &backend); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("GlanceBackend not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching GlanceBackend: %w", err)
	}

	// No finalizer: an S3 backend has no external resources, so a deletion just
	// detaches. The parent Glance re-aggregates via its watch on GlanceBackend,
	// so there is nothing for this controller to do on teardown.
	if backend.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	statusBefore := backend.Status.DeepCopy()
	result, err := r.reconcileNormal(ctx, &backend)
	return r.updateStatus(ctx, &backend, statusBefore, result, err)
}

// reconcileNormal gates on the S3 credentials Secret, then observes the config
// projection. It short-circuits before the projection observation while the
// credentials are not ready — the parent never projects a backend that is not
// credential-ready, so there is nothing to observe yet.
//
// Both observations read objects that belong to the parent Glance, so it opens
// by resolving that parent's target cluster into the children client.
func (r *GlanceBackendReconciler) reconcileNormal(ctx context.Context, backend *glancev1alpha1.GlanceBackend) (ctrl.Result, error) {
	children, result, err := r.resolveChildren(ctx, backend)
	if children == nil {
		return result, err
	}

	ready, err := r.gateCredentials(ctx, children, backend)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		// The referenced Secret is absent or missing keys. The parent's Secret
		// watch re-renders on rotation, but a genuinely absent Secret emits no
		// event, so poll as the liveness backstop.
		// commonreconcile.RequeueSecretPolling is that backstop for both of
		// this controller's gates: CredentialsReady here and ConfigProjected
		// below.
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	}
	return r.observeConfigProjected(ctx, children, backend)
}

// resolveChildren returns the client this backend's observations read from: the
// one the PARENT Glance's spec.targetClusterRef selects. The backend has no
// target field of its own, so the S3 credentials Secret it gates on and the
// Deployment it watches for its rendered store section resolve wherever the
// parent's workload runs — the same cluster the API pods mount them from.
//
// A parent that does not exist holds the pass on CredentialsReady=False and
// requeues, the same treatment an unresolvable target gets. Reading the
// management cluster instead would gate this backend on whatever Secret of that
// name happens to live there, so a backend attached to a Glance on a target
// cluster could report credentials it will never use — and the store keyed the
// same way mints into the wrong cluster, so both resolve alike.
//
// A missing parent also demotes a standing ConfigProjected=True: that condition
// claims the parent's Deployment mounts this backend's store section, and with
// the parent gone the claim is stale — the deletion-cleanup e2e pins the
// surviving backend on ConfigProjected=False. A backend that never converged
// keeps the condition absent (the observation never ran), and an unresolvable
// target demotes nothing: unobservable is not un-projected, matching the
// no-demote-on-error doctrine of observeConfigProjected.
//
// A named-but-unregistered target returns a nil client with the failure already
// on CredentialsReady, the backend's first gate condition.
func (r *GlanceBackendReconciler) resolveChildren(
	ctx context.Context, backend *glancev1alpha1.GlanceBackend,
) (client.Client, ctrl.Result, error) {
	var parent glancev1alpha1.Glance
	parentKey := client.ObjectKey{Namespace: backend.Namespace, Name: backend.Spec.GlanceRef.Name}
	if err := r.Get(ctx, parentKey, &parent); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, ctrl.Result{}, fmt.Errorf("fetching parent Glance %s: %w", parentKey, err)
		}
		r.setCredentialsReady(backend, metav1.ConditionFalse, conditionReasonWaitingForParent,
			fmt.Sprintf("parent Glance %s does not exist; the cluster this backend's credentials belong on is unknown", parentKey))
		if projected := conditions.GetCondition(backend.Status.Conditions, conditionTypeConfigProjected); projected != nil && projected.Status == metav1.ConditionTrue {
			r.setConfigProjected(backend, metav1.ConditionFalse, conditionReasonWaitingForProjection,
				fmt.Sprintf("parent Glance %s does not exist; nothing mounts this backend's rendered store section", parentKey))
		}
		return nil, ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	}

	children, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client, parent.Spec.TargetClusterRef)
	if err != nil {
		r.setCredentialsReady(backend, metav1.ConditionFalse,
			commonmulticluster.TargetClusterUnavailable, err.Error())
		return nil, ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	}
	return children, ctrl.Result{}, nil
}

// gateCredentials verifies the S3 credentials Secret exists and carries the
// contract data keys, maintaining CredentialsReady. It delegates the
// materialized-Secret-then-ExternalSecret ladder to secrets.GateCredential
// (which sets the False condition with a precise message) and stamps the True
// condition itself on success. A backend error is propagated so the workqueue
// backs off without demoting a currently-True condition.
//
// The Secret is read on the children cluster: the store credentials materialise
// beside the Glance pods that authenticate with them.
func (r *GlanceBackendReconciler) gateCredentials(ctx context.Context, children client.Client, backend *glancev1alpha1.GlanceBackend) (bool, error) {
	if backend.Spec.S3 == nil {
		// The schema union rule guarantees spec.s3 for a type-S3 backend; a
		// bypassed admission leaves nothing to gate on.
		r.setCredentialsReady(backend, metav1.ConditionFalse, conditionReasonWaitingForCredentials,
			"spec.s3 is not set; no S3 credentials to resolve")
		return false, nil
	}

	spec := secrets.CredentialGateSpec{
		Key: client.ObjectKey{
			Namespace: backend.Namespace,
			Name:      backend.Spec.S3.CredentialsSecretRef.Name,
		},
		Reason:     conditionReasonWaitingForCredentials,
		Noun:       "S3 credentials",
		WaitingMsg: "waiting for the S3 credentials Secret to carry the required keys",
		ExpectedKeys: []string{
			glancev1alpha1.S3AccessKeyIDKey,
			glancev1alpha1.S3SecretAccessKeyKey,
		},
	}
	ready, err := secrets.GateCredential(ctx, children, spec, &backend.Status.Conditions, backend.Generation, conditionTypeCredentialsReady)
	if err != nil {
		return false, err
	}
	if ready {
		r.setCredentialsReady(backend, metav1.ConditionTrue, conditionReasonCredentialsAvailable,
			fmt.Sprintf("S3 credentials Secret %q carries the required keys", backend.Spec.S3.CredentialsSecretRef.Name))
	}
	return ready, nil
}

// observeConfigProjected derives the ConfigProjected condition from the single
// authoritative pointer: the parent Glance Deployment's backends volume and the
// rendered store section inside the Secret it references. A False observation
// carries a commonreconcile.RequeueSecretPolling safety net — the Glance
// watch normally wakes this controller when the projection lands, but a
// converged Glance status (no write, no event) must not strand the backend.
// A transient Get/List failure returns the error WITHOUT demoting a
// currently-True condition, so a converged backend does not flap on a cache
// blip.
func (r *GlanceBackendReconciler) observeConfigProjected(ctx context.Context, children client.Client, backend *glancev1alpha1.GlanceBackend) (ctrl.Result, error) {
	projected, err := r.isConfigProjected(ctx, children, backend)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !projected {
		r.setConfigProjected(backend, metav1.ConditionFalse, conditionReasonWaitingForProjection,
			"waiting for the Glance Deployment to mount this backend's rendered store section")
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	}
	r.setConfigProjected(backend, metav1.ConditionTrue, conditionReasonConfigProjected,
		"store section is rendered into the Glance Deployment's backends config")
	return ctrl.Result{}, nil
}

// isConfigProjected reports whether the parent Glance Deployment mounts this
// backend's rendered store section: the backends-volume Secret's backends.conf
// carries the [<backend.Name>] section header. A missing parent, Deployment,
// volume, or Secret folds to (false, nil) — an authoritative "not projected
// yet". Only a non-NotFound client failure returns an error.
//
// The parent CR is read on the management cluster, the Deployment it projects
// and that Deployment's projection Secret on the children one.
func (r *GlanceBackendReconciler) isConfigProjected(ctx context.Context, children client.Client, backend *glancev1alpha1.GlanceBackend) (bool, error) {
	var glance glancev1alpha1.Glance
	glanceKey := client.ObjectKey{Namespace: backend.Namespace, Name: backend.Spec.GlanceRef.Name}
	if err := r.Get(ctx, glanceKey, &glance); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("fetching parent Glance %s: %w", glanceKey, err)
	}

	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Namespace: glance.Namespace, Name: subResourceName(&glance)}
	if err := children.Get(ctx, deployKey, &deploy); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("fetching Deployment %s: %w", deployKey, err)
	}

	var secretName string
	for i := range deploy.Spec.Template.Spec.Volumes {
		v := &deploy.Spec.Template.Spec.Volumes[i]
		if v.Name == backendsVolumeName && v.Secret != nil {
			secretName = v.Secret.SecretName
			break
		}
	}
	if secretName == "" {
		return false, nil
	}

	var secret corev1.Secret
	secretKey := client.ObjectKey{Namespace: glance.Namespace, Name: secretName}
	if err := children.Get(ctx, secretKey, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("fetching projection Secret %s: %w", secretKey, err)
	}
	return backendSectionPresent(secret.Data[backendsConfDataKey], backend.Name), nil
}

// subResourceName returns the canonical name for Glance operator-managed
// sub-resources (Deployment, HPA, Service, PodDisruptionBudget, NetworkPolicy,
// HTTPRoute). Centralised here so the naming convention is defined in one
// place — the bare CR name with no suffix. The glance-side deployment step
// (next commit) reuses this exact helper.
func subResourceName(glance *glancev1alpha1.Glance) string {
	return glance.Name
}

// backendSectionPresent reports whether the rendered backends.conf carries the
// store-section header for the named backend as a whole line: the literal token
// "[name]" bounded by the start of data or a newline on the left and a newline
// (LF or CR) or the end of data on the right. The boundary check guards against
// substring collisions — a section [name2] must not satisfy a lookup for name,
// and a "[name]" appearing inside an option value must not either.
func backendSectionPresent(rendered []byte, name string) bool {
	header := "[" + name + "]"
	data := string(rendered)
	for start := 0; ; {
		idx := strings.Index(data[start:], header)
		if idx < 0 {
			return false
		}
		idx += start
		leftOK := idx == 0 || data[idx-1] == '\n'
		right := idx + len(header)
		rightOK := right == len(data) || data[right] == '\n' || data[right] == '\r'
		if leftOK && rightOK {
			return true
		}
		start = idx + len(header)
	}
}

// setCredentialsReady upserts the CredentialsReady condition.
func (r *GlanceBackendReconciler) setCredentialsReady(backend *glancev1alpha1.GlanceBackend, status metav1.ConditionStatus, reason, message string) {
	conditions.SetCondition(&backend.Status.Conditions, metav1.Condition{
		Type:               conditionTypeCredentialsReady,
		Status:             status,
		ObservedGeneration: backend.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// setConfigProjected upserts the ConfigProjected condition.
func (r *GlanceBackendReconciler) setConfigProjected(backend *glancev1alpha1.GlanceBackend, status metav1.ConditionStatus, reason, message string) {
	conditions.SetCondition(&backend.Status.Conditions, metav1.Condition{
		Type:               conditionTypeConfigProjected,
		Status:             status,
		ObservedGeneration: backend.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// updateStatus persists the backend status via the shared helper: the write is
// skipped when the pass left status unchanged, the aggregate Ready is
// re-derived from the sub-condition set on every persist, and ObservedGeneration
// is stamped.
func (r *GlanceBackendReconciler) updateStatus(ctx context.Context, backend *glancev1alpha1.GlanceBackend, statusBefore *glancev1alpha1.GlanceBackendStatus, result ctrl.Result, reconcileErr error) (ctrl.Result, error) {
	return commonreconcile.UpdateStatus(ctx, r.Client, backend, statusBefore, &backend.Status, func() {
		commonreconcile.SetAggregateReady(&backend.Status.Conditions, backend.Generation, glanceBackendSubConditionTypes)
		backend.Status.ObservedGeneration = backend.Generation
	}, result, reconcileErr)
}

// SetupWithManager registers the GlanceBackendReconciler with the controller
// manager. The GlanceBackend field indexes are registered by
// GlanceReconciler.SetupWithManager (the single registration site — both
// controllers run in one manager and that reconciler is set up first in
// main.go and the envtest helper), so this controller registers none.
func (r *GlanceBackendReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(bootstrap.ControllerOptions(r.MaxConcurrentReconciles)).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate).
		For(&glancev1alpha1.GlanceBackend{}, builder.WithPredicates(watch.CRUpdatePredicate())).
		// Watch the referenced Glance WITHOUT a generation predicate: Glance
		// status flips (the store config landing in the Deployment) are exactly
		// the wake signal the ConfigProjected gate waits on. No own Secret watch:
		// credential changes re-render via the Glance side, and the resulting
		// projection flip reaches this controller through the Glance watch.
		Watches(&glancev1alpha1.Glance{}, handler.EnqueueRequestsFromMapFunc(
			glanceToGlanceBackendsMapper(mgr.GetClient()),
		)).
		Complete(r)
}
