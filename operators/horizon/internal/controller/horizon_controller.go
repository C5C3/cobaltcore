// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the Horizon reconciler.
//
// Horizon is the deliberately thin operator profile: a stateless Django/WSGI
// dashboard with no database, message bus, fernet, bootstrap, or upgrade
// machinery, and no finalizer — every owned resource is namespace-scoped and
// garbage-collected via ownerReferences when the CR is deleted.
package controller

import (
	"context"
	"fmt"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/forge/internal/common/bootstrap"
	"github.com/c5c3/forge/internal/common/gateway"
	"github.com/c5c3/forge/internal/common/healthcheck"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	"github.com/c5c3/forge/internal/common/watch"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
)

// HorizonSecretNameIndexKey is the field-indexer key under which Horizon CRs
// are indexed by their referenced Secret name (spec.secretKeyRef.name). Used
// by SetupWithManager to register the indexer and by secretToHorizonMapper to
// perform an O(1) reverse lookup instead of an unfiltered List of all Horizon
// CRs in the namespace.
// #nosec G101 -- field-indexer key (a JSONPath-like field selector), not a credential.
const HorizonSecretNameIndexKey = "spec.secretRefs.name"

// horizonSecretNameExtractor is the controller-runtime IndexerFunc registered
// under HorizonSecretNameIndexKey. It returns the non-empty Secret name
// referenced by a Horizon CR — currently only spec.secretKeyRef.name — so the
// field indexer can resolve a Secret event to the referencing CR(s).
func horizonSecretNameExtractor(obj client.Object) []string {
	h, ok := obj.(*horizonv1alpha1.Horizon)
	if !ok {
		// controller-runtime should never call us with the wrong type; a nil
		// return is safer than a panic if it ever does.
		return nil
	}
	if name := h.Spec.SecretKeyRef.Name; name != "" {
		return []string{name}
	}
	return nil
}

// registerSecretNameIndex registers the Horizon field indexer under
// HorizonSecretNameIndexKey with the given FieldIndexer.
func registerSecretNameIndex(ctx context.Context, indexer client.FieldIndexer) error {
	return watch.RegisterSecretNameIndex(ctx, indexer, &horizonv1alpha1.Horizon{}, HorizonSecretNameIndexKey, horizonSecretNameExtractor)
}

// subConditionTypes lists the condition types set by individual sub-reconcilers.
// The Ready condition is True only when all of these are True.
var subConditionTypes = []string{
	"SecretsReady",
	conditionTypeConfigReady,
	"DeploymentReady",
	conditionTypeHTTPRouteReady,
	conditionTypeHorizonAPIReady,
	"HPAReady",
	conditionTypeNetworkPolicyReady,
}

// HorizonReconciler reconciles a Horizon object.
type HorizonReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	HTTPClient HTTPDoer

	// OperatorNamespace is the Namespace the operator Pod runs in (resolved at
	// startup by bootstrap.DetectOperatorNamespace). reconcileNetworkPolicy
	// appends an ingress peer for this Namespace so the operator's own health
	// check can reach the dashboard on TCP 8080. Empty when the namespace
	// could not be determined, in which case no operator-namespace peer is
	// added.
	OperatorNamespace string

	// gatewayAPIAvailable is set during SetupWithManager from the cluster's
	// RESTMapper and indicates whether the gateway.networking.k8s.io/v1
	// HTTPRoute CRD is installed. When false, the controller skips the
	// HTTPRoute watch entirely so it does not crash on a missing kind, and
	// reconcileHTTPRoute surfaces a clear HTTPRouteReady=False condition if
	// the user nonetheless sets spec.gateway.
	gatewayAPIAvailable bool

	// MaxConcurrentReconciles bounds how many Horizon CRs reconcile
	// concurrently. It is threaded from the --max-concurrent-reconciles flag
	// (see internal/common/bootstrap) and applied to the controller's
	// controller.Options in SetupWithManager. A value <= 0 falls back to
	// bootstrap.DefaultMaxConcurrentReconciles inside
	// bootstrap.ControllerOptions.
	MaxConcurrentReconciles int

	// healthProbeCache memoizes the last successful login-page probe per CR
	// (shared TTL probe cache) so a steady-state reconcile does not fire a
	// synchronous HTTP GET on every pass. A cache hit re-upserts
	// HorizonAPIReady=True without probing; any probe error or non-2xx evicts
	// the entry.
	healthProbeCache healthcheck.ProbeCache
}

// httpRouteGVK identifies the HTTPRoute kind the operator would watch when
// Gateway API is installed. Availability is probed at setup time via the
// shared gateway.IsGVKAvailable RESTMapper probe.
var httpRouteGVK = schema.GroupVersionKind{
	Group:   gatewayv1.GroupVersion.Group,
	Version: gatewayv1.GroupVersion.Version,
	Kind:    "HTTPRoute",
}

// +kubebuilder:rbac:groups=horizon.openstack.c5c3.io,resources=horizons,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=horizon.openstack.c5c3.io,resources=horizons/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=horizon.openstack.c5c3.io,resources=horizons/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=get
// +kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=external-secrets.io,resources=clustersecretstores;secretstores,verbs=get;list;watch

// Reconcile is the main reconciliation loop for the Horizon CR.
func (r *HorizonReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Fetch the Horizon CR.
	var horizon horizonv1alpha1.Horizon
	if err := r.Get(ctx, req.NamespacedName, &horizon); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("Horizon resource not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching Horizon: %w", err)
	}

	// No finalizer: every owned resource is namespace-scoped and reclaimed by
	// Kubernetes garbage collection via ownerReferences, so a deleting CR
	// needs no cleanup pass — skip all work and let GC finish.
	if !horizon.DeletionTimestamp.IsZero() {
		log.FromContext(ctx).V(1).Info("Horizon resource is being deleted; owned resources are garbage-collected via ownerReferences")
		r.evictHealthProbe(client.ObjectKeyFromObject(&horizon))
		return ctrl.Result{}, nil
	}

	// Snapshot the persisted status so updateStatus can skip the write when a
	// pass leaves status unchanged (no write → no watch event → no
	// resourceVersion churn).
	statusBefore := horizon.Status.DeepCopy()

	// Run the sub-reconciler pipeline. Steps are attempted in dependency
	// order; the first to return a non-zero result or an error short-circuits
	// the chain and funnels through updateStatus, so conditions and the
	// requeue/error are persisted by construction on every exit path.
	var configMapName string
	// secretKeyHash is the SHA-256 digest of the Django SECRET_KEY material
	// produced by reconcileSecrets; it is threaded to reconcileDeployment as a
	// pod-template annotation so a rotated key rolls the Deployment (the key
	// is env-var-consumed, not volume-mounted, so it only takes effect on a
	// Pod restart).
	var secretKeyHash string
	pipeline := []commonreconcile.Step{
		{Name: "Secrets", Fn: func(ctx context.Context) (ctrl.Result, error) {
			var (
				res ctrl.Result
				err error
			)
			res, secretKeyHash, err = r.reconcileSecrets(ctx, &horizon)
			return res, err
		}},
		// reconcileConfig must run before Deployment, which mounts the
		// rendered local_settings.py ConfigMap. It returns (string, error)
		// rather than the standard (ctrl.Result, error): the wrapper captures
		// the ConfigMap name via closure and, on failure, flips
		// ConfigReady=False via markConfigFailed so the aggregate Ready cannot
		// stay stale-True at the new generation.
		{Name: "Config", Fn: func(ctx context.Context) (ctrl.Result, error) {
			var err error
			configMapName, err = r.reconcileConfig(ctx, &horizon)
			if err != nil {
				markConfigFailed(&horizon, err)
			}
			return ctrl.Result{}, err
		}},
		{Name: "Deployment", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDeployment(ctx, &horizon, configMapName, secretKeyHash)
		}},
		// Prune stale immutable ConfigMaps after Deployment is ready so all
		// pods run the new config before old ConfigMaps are deleted.
		// Uninstrumented (no sub_reconciler name); a prune failure is a
		// config-concern failure, so it flips ConfigReady=False via
		// markConfigFailed rather than leaving the aggregate Ready stale-True.
		{Fn: func(ctx context.Context) (ctrl.Result, error) {
			if err := r.pruneStaleConfigMaps(ctx, &horizon, configMapName); err != nil {
				markConfigFailed(&horizon, err)
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}},
		// Once the Deployment/Service outputs are in place, HTTPRoute,
		// HealthCheck, HPA, and NetworkPolicy have no inter-dependency:
		// HTTPRoute needs the backend Service, HealthCheck needs
		// Status.Endpoint (both set by reconcileDeployment above), HPA targets
		// the Deployment, and NetworkPolicy uses selector labels derived from
		// the CR. Each member sets exactly one condition type, so they merge
		// back independently. The group self-instruments its members, so this
		// step carries no sub_reconciler name.
		{Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileParallelGroup(ctx, &horizon, []commonreconcile.ParallelStep[*horizonv1alpha1.Horizon]{
				{
					Name:          "HTTPRoute",
					ConditionType: conditionTypeHTTPRouteReady,
					Fn: func(ctx context.Context, h *horizonv1alpha1.Horizon) (ctrl.Result, error) {
						return r.reconcileHTTPRoute(ctx, h)
					},
				},
				{
					Name:          "HealthCheck",
					ConditionType: conditionTypeHorizonAPIReady,
					Fn: func(ctx context.Context, h *horizonv1alpha1.Horizon) (ctrl.Result, error) {
						return r.reconcileHealthCheck(ctx, h)
					},
				},
				{
					Name:          "HPA",
					ConditionType: "HPAReady",
					Fn: func(ctx context.Context, h *horizonv1alpha1.Horizon) (ctrl.Result, error) {
						return r.reconcileHPA(ctx, h)
					},
				},
				{
					Name:          "NetworkPolicy",
					ConditionType: conditionTypeNetworkPolicyReady,
					Fn: func(ctx context.Context, h *horizonv1alpha1.Horizon) (ctrl.Result, error) {
						return r.reconcileNetworkPolicy(ctx, h)
					},
				},
			})
		}},
	}

	// commonreconcile.RunPipeline short-circuits on the first non-zero result
	// or error; both the short-circuit and the fully-successful chain funnel
	// through updateStatus, which recomputes the aggregate Ready condition.
	result, err := commonreconcile.RunPipeline(ctx, instrumentSubReconciler, pipeline)
	return r.updateStatus(ctx, &horizon, statusBefore, result, err)
}

// updateStatus persists the current status conditions and returns the given
// result and error, delegating to commonreconcile.UpdateStatus: the write is
// skipped when the pass left status semantically unchanged from the
// statusBefore snapshot, and a failed write is joined with reconcileErr so
// the original reconcile failure stays visible. The mutate hook re-aggregates
// the Ready condition on every persist and stamps status.observedGeneration.
func (r *HorizonReconciler) updateStatus(ctx context.Context, horizon *horizonv1alpha1.Horizon, statusBefore *horizonv1alpha1.HorizonStatus, result ctrl.Result, reconcileErr error) (ctrl.Result, error) {
	return horizonSkeleton.UpdateStatus(ctx, r.Client, horizon, statusBefore, &horizon.Status, func() {
		horizon.Status.ObservedGeneration = horizon.Generation
	}, result, reconcileErr)
}

// horizonSkeleton bundles the shared controller-skeleton glue (Ready
// aggregation, no-op-skipping status writes, config-failure marking, and
// parallel-group execution) with horizon's sub-condition vocabulary and status
// accessor. The wrapper methods below delegate to it.
var horizonSkeleton = commonreconcile.Skeleton[*horizonv1alpha1.Horizon, horizonv1alpha1.HorizonStatus]{
	SubConditionTypes: subConditionTypes,
	Conditions:        func(h *horizonv1alpha1.Horizon) *[]metav1.Condition { return &h.Status.Conditions },
}

// setReadyCondition sets the aggregate Ready condition based on all
// sub-conditions, delegating to the shared skeleton with horizon's
// sub-condition vocabulary.
func setReadyCondition(horizon *horizonv1alpha1.Horizon) {
	horizonSkeleton.SetReady(horizon)
}

// markConfigFailed flips ConfigReady to False so a reconcileConfig or config
// prune failure cannot leave the aggregate Ready condition stale-True at the
// new ObservedGeneration.
func markConfigFailed(horizon *horizonv1alpha1.Horizon, err error) {
	horizonSkeleton.MarkFailed(horizon, conditionTypeConfigReady, conditionReasonConfigError, err)
}

// reconcileParallelGroup runs the given sub-reconcilers concurrently,
// delegating to commonreconcile.RunParallelGroup: each member operates on its
// own DeepCopy of the Horizon CR, conditions from every member — including
// those that succeeded before a peer failed — are merged back into the
// primary horizon, and on success the shortest non-zero RequeueAfter is
// returned. Members instrument individually via instrumentSubReconciler.
func (r *HorizonReconciler) reconcileParallelGroup(
	ctx context.Context,
	horizon *horizonv1alpha1.Horizon,
	subs []commonreconcile.ParallelStep[*horizonv1alpha1.Horizon],
) (ctrl.Result, error) {
	return horizonSkeleton.RunParallelGroup(ctx, horizon, instrumentSubReconciler, subs)
}

// SetupWithManager registers the HorizonReconciler with the controller manager.
func (r *HorizonReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Detect whether the Gateway API CRD is installed. spec.gateway is
	// optional, so the operator must run on clusters without Gateway API.
	// Adding Owns(HTTPRoute) unconditionally would cause the controller to
	// fail at Start with "no matches for kind HTTPRoute" when the CRD is
	// missing, preventing every Horizon CR from being reconciled.
	r.gatewayAPIAvailable = gateway.IsGVKAvailable(mgr.GetRESTMapper(), httpRouteGVK)
	setupLog := ctrl.Log.WithName("horizon-setup")
	if r.gatewayAPIAvailable {
		setupLog.Info("Gateway API detected; enabling HTTPRoute watch and reconciliation")
	} else {
		setupLog.Info("Gateway API not installed; HTTPRoute watch disabled, spec.gateway will be rejected via HTTPRouteReady condition")
	}

	// Register the Horizon field indexer before Watches so
	// secretToHorizonMapper can rely on it for its MatchingFields lookup.
	if err := registerSecretNameIndex(context.Background(), mgr.GetFieldIndexer()); err != nil {
		return err
	}

	b := ctrl.NewControllerManagedBy(mgr).
		// Shared controller options: MaxConcurrentReconciles lets independent
		// CRs reconcile in parallel, and the tuned RateLimiter caps per-item
		// failure backoff at 30s (see bootstrap.ControllerOptions).
		WithOptions(bootstrap.ControllerOptions(r.MaxConcurrentReconciles)).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate).
		For(&horizonv1alpha1.Horizon{}, builder.WithPredicates(watch.CRUpdatePredicate())).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Owns(&networkingv1.NetworkPolicy{})

	if r.gatewayAPIAvailable {
		b = b.Owns(&gatewayv1.HTTPRoute{})
	}

	return b.
		// Watch Secrets and map to the Horizon CRs that reference them.
		// The ESO-managed SECRET_KEY Secret is owned by the ExternalSecret
		// controller, not by the Horizon CR, so EnqueueRequestForOwner would
		// never match it. This MapFunc performs an indexed reverse lookup.
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(
			secretToHorizonMapper(mgr.GetClient()),
		)).
		// Watch both the cluster-scoped ClusterSecretStore and the namespaced
		// SecretStore a Horizon can select via spec.secretStoreRef, so the
		// operator reflects upstream secret-backend outages in SecretsReady as
		// soon as ESO flips the selected store's Ready condition, rather than
		// waiting for the next periodic requeue. Each mapper enqueues only the
		// Horizons whose effective store ref matches the changed store.
		Watches(&esov1.ClusterSecretStore{}, handler.EnqueueRequestsFromMapFunc(
			storeToHorizonMapper(mgr.GetClient(), commonv1.SecretStoreKindCluster),
		)).
		Watches(&esov1.SecretStore{}, handler.EnqueueRequestsFromMapFunc(
			storeToHorizonMapper(mgr.GetClient(), commonv1.SecretStoreKindNamespaced),
		)).
		Complete(r)
}
