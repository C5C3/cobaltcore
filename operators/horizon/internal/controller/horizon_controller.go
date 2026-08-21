// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the Horizon reconciler.
//
// Horizon is the deliberately thin operator profile: a stateless Django/WSGI
// dashboard with no database, message bus, fernet, bootstrap, or upgrade
// machinery. A CR that keeps its children on the management cluster carries no
// finalizer either: every owned resource is namespace-scoped and
// garbage-collected via ownerReferences when the CR is deleted. Only a CR that
// projects onto a target cluster is finalized, because no garbage collection
// cascade crosses a cluster boundary and the operator has to delete those
// children itself.
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
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	"github.com/c5c3/cobaltcore/internal/common/gateway"
	"github.com/c5c3/cobaltcore/internal/common/healthcheck"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	horizonv1alpha1 "github.com/c5c3/cobaltcore/operators/horizon/api/v1alpha1"
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

	// Recorder emits Kubernetes events for the Horizon CR (the extraConfig
	// ownership guard's Warning event). Wired from
	// mgr.GetEventRecorderFor in main.go; tests use record.NewFakeRecorder.
	Recorder record.EventRecorder

	// Resolver resolves the target cluster a Horizon CR names in
	// spec.targetClusterRef into the client its children are read and written
	// with. Nil means always-local: every CR keeps its children on the
	// management cluster, which is what single-cluster tests and deployments
	// want.
	Resolver commonmulticluster.ClusterResolver

	// OperatorNamespace is the Namespace the operator Pod runs in (resolved at
	// startup by bootstrap.DetectOperatorNamespace). reconcileNetworkPolicy
	// appends an ingress peer for this Namespace so the operator's own health
	// check can reach the dashboard on TCP 8080. Empty when the namespace
	// could not be determined, in which case no operator-namespace peer is
	// added.
	OperatorNamespace string

	// apiReader is set during SetupWithManager from mgr.GetAPIReader(): a
	// direct, uncached reader. reconcileDeployment latches the two-phase
	// narrowing of the dashboard Service selector on the live Service, and the
	// informer cache can still hold the pre-narrowing state right after the
	// narrowing write — a latch decided from it could widen the selector back.
	// Nil in unit tests that construct the reconciler without a manager; those
	// fall back to the (read-your-writes) fake client.
	apiReader client.Reader

	// gatewayAPIAvailable is set during SetupWithManager from the
	// management cluster's RESTMapper and indicates whether the
	// gateway.networking.k8s.io/v1 HTTPRoute CRD is installed there. Two
	// consumers read it: the local HTTPRoute watch leg, which
	// SetupWithManager skips when false so the controller does not crash on
	// a missing kind, and commonmulticluster.ChildrenServeKind, which
	// answers with it for local children while probing the target cluster's
	// RESTMapper for remote ones. reconcileHTTPRoute takes that verdict and
	// surfaces a clear HTTPRouteReady=False condition if the user
	// nonetheless sets spec.gateway.
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

// HorizonRemoteChildKinds are the kinds a Horizon CR projects into the namespace
// of the target cluster it names, and the kinds reconcileDeleteRemoteChildren
// sweeps by ownership label when that CR is deleted. Nothing on the target
// cluster collects them, so a kind missing from this list is a kind that keeps
// running after its CR is gone.
//
// The list is cross-checked against the create verbs of the kubebuilder RBAC
// markers on this controller below: the operator can only leave behind what it
// is allowed to create. It is the shortest of the operators' lists, because
// horizon composes no Secret, Job, CronJob, or MariaDB CR — it reads the
// SECRET_KEY Secret that ESO materializes rather than writing one.
var HorizonRemoteChildKinds = []schema.GroupVersionKind{
	appsv1.SchemeGroupVersion.WithKind("Deployment"),
	corev1.SchemeGroupVersion.WithKind("Service"),
	corev1.SchemeGroupVersion.WithKind("ConfigMap"),
	policyv1.SchemeGroupVersion.WithKind("PodDisruptionBudget"),
	autoscalingv2.SchemeGroupVersion.WithKind("HorizontalPodAutoscaler"),
	networkingv1.SchemeGroupVersion.WithKind("NetworkPolicy"),
	httpRouteGVK,
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

	// A CR whose children stay on the management cluster carries no finalizer:
	// every owned resource is namespace-scoped and reclaimed by Kubernetes
	// garbage collection via ownerReferences, so a deleting CR needs no cleanup
	// pass — skip all work and let GC finish.
	//
	// A CR that projects onto a target cluster carries the remote-children
	// finalizer and sweeps instead. Its children are written unowned there (an
	// owner reference would name a UID that cluster cannot resolve) and no
	// cascade crosses the boundary, so the projection is deleted from here. A
	// target cluster that stays unresolvable past the abandon window is the one
	// case that keeps the projection.
	//
	// The sweep resolves through the deletion variant, which never fails the
	// pass: a CR whose cluster was deregistered after the finalizer went on would
	// otherwise short-circuit on the unresolvable ref on every pass and stay
	// Terminating forever. A target that has not resolved yet requeues instead of
	// being given up on — engagement is asynchronous, so an operator restart
	// looks exactly like a deregistration until the provider has synced.
	if !horizon.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&horizon, commonmulticluster.RemoteChildrenFinalizer) {
			children, wait := commonmulticluster.ResolveChildrenClientForDeletion(
				ctx, r.Resolver, r.Client, horizon.Spec.TargetClusterRef, *horizon.DeletionTimestamp)
			if wait {
				// The hold goes on the CR, not only into the operator's log. It is a
				// deliberate state a CR can sit in for minutes, and "Terminating,
				// waiting on the target cluster" has to be distinguishable from a
				// wedged finalizer without correlating logs across replicas.
				statusBefore := horizon.Status.DeepCopy()
				horizonSkeleton.MarkFailed(&horizon, "SecretsReady",
					commonmulticluster.TargetClusterUnavailable,
					fmt.Errorf("target cluster %s does not resolve; waiting at least %s before abandoning its children",
						horizon.Spec.TargetClusterRef.Name, commonmulticluster.AbandonAfter))
				return r.updateStatus(ctx, &horizon, statusBefore,
					ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil)
			}
			if err := r.reconcileDeleteRemoteChildren(ctx, children, &horizon); err != nil {
				return ctrl.Result{}, err
			}
		}
		log.FromContext(ctx).V(1).Info("Horizon resource is being deleted; owned resources are garbage-collected via ownerReferences")
		r.evictHealthProbe(client.ObjectKeyFromObject(&horizon))
		return ctrl.Result{}, nil
	}

	// Snapshot the persisted status so updateStatus can skip the write when a
	// pass leaves status unchanged (no write → no watch event → no
	// resourceVersion churn).
	statusBefore := horizon.Status.DeepCopy()

	// Resolve the client every child object of this CR is read and written
	// with. The embedded client stays on the management cluster (the CR and its
	// status live there); children carries everything the CR projects into the
	// target cluster. The deletion branch above already returned, so a
	// terminating CR never reaches this resolution: it fails the pass on an
	// unresolvable ref, which would keep a finalized CR from ever reaching its
	// sweep.
	children, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client, horizon.Spec.TargetClusterRef)
	if err != nil {
		horizonSkeleton.MarkFailed(&horizon, "SecretsReady", commonmulticluster.TargetClusterUnavailable, err)
		return r.updateStatus(ctx, &horizon, statusBefore,
			ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil)
	}

	// The remote-children finalizer is horizon's first and only finalizer, and
	// it goes on only when the CR projects onto a target cluster. A local CR
	// keeps the garbage collection cascade, which reaps its children from their
	// owner references, so it stays finalizer-free: a finalizer it does not need
	// is one more deletion-time step that can fail. spec.targetClusterRef is
	// immutable, so the condition cannot flip under a live CR.
	if horizon.Spec.TargetClusterRef != nil {
		if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &horizon,
			commonmulticluster.RemoteChildrenFinalizer); err != nil {
			return ctrl.Result{}, err
		} else if added {
			return ctrl.Result{Requeue: true}, nil
		}
	}

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
			res, secretKeyHash, err = r.reconcileSecrets(ctx, children, &horizon)
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
			configMapName, err = r.reconcileConfig(ctx, children, &horizon)
			if err != nil {
				markConfigFailed(&horizon, err)
			}
			return ctrl.Result{}, err
		}},
		{Name: "Deployment", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDeployment(ctx, children, &horizon, configMapName, secretKeyHash)
		}},
		// Prune stale immutable ConfigMaps after Deployment is ready so all
		// pods run the new config before old ConfigMaps are deleted.
		// Uninstrumented (no sub_reconciler name); a prune failure is a
		// config-concern failure, so it flips ConfigReady=False via
		// markConfigFailed rather than leaving the aggregate Ready stale-True.
		{Fn: func(ctx context.Context) (ctrl.Result, error) {
			if err := r.pruneStaleConfigMaps(ctx, children, &horizon, configMapName); err != nil {
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
						return r.reconcileHTTPRoute(ctx, children, h)
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
						return r.reconcileHPA(ctx, children, h)
					},
				},
				{
					Name:          "NetworkPolicy",
					ConditionType: conditionTypeNetworkPolicyReady,
					Fn: func(ctx context.Context, h *horizonv1alpha1.Horizon) (ctrl.Result, error) {
						return r.reconcileNetworkPolicy(ctx, children, h)
					},
				},
			})
		}},
	}

	// commonreconcile.RunPipeline short-circuits on the first non-zero result
	// or error; both the short-circuit and the fully-successful chain funnel
	// through updateStatus, which recomputes the aggregate Ready condition.
	result, err := commonreconcile.RunPipeline(ctx, instrumenter.Instrument, pipeline)
	return r.updateStatus(ctx, &horizon, statusBefore, result, err)
}

// reconcileDeleteRemoteChildren deletes everything this Horizon projected onto
// the target cluster it names, selected on the ownership labels Claim stamped on
// those objects, and releases the remote-children finalizer that held the CR in
// etcd for it, as commonmulticluster.SweepRemoteChildren documents.
func (r *HorizonReconciler) reconcileDeleteRemoteChildren(ctx context.Context, children client.Client, horizon *horizonv1alpha1.Horizon) error {
	return commonmulticluster.SweepRemoteChildren(ctx, r.Client, r.Resolver, r.Recorder, r.Scheme,
		horizon, horizon.Spec.TargetClusterRef, children, HorizonRemoteChildKinds)
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
// returned. Members instrument individually via instrumenter.Instrument.
func (r *HorizonReconciler) reconcileParallelGroup(
	ctx context.Context,
	horizon *horizonv1alpha1.Horizon,
	subs []commonreconcile.ParallelStep[*horizonv1alpha1.Horizon],
) (ctrl.Result, error) {
	return horizonSkeleton.RunParallelGroup(ctx, horizon, instrumenter.Instrument, subs)
}

// SetupWithManager registers the HorizonReconciler with the controller manager.
func (r *HorizonReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	local := mgr.GetLocalManager()

	// Detect whether the Gateway API CRD is installed. spec.gateway is
	// optional, so the operator must run on clusters without Gateway API.
	// Adding Owns(HTTPRoute) unconditionally would cause the controller to
	// fail at Start with "no matches for kind HTTPRoute" when the CRD is
	// missing, preventing every Horizon CR from being reconciled.
	r.gatewayAPIAvailable = gateway.IsGVKAvailable(local.GetRESTMapper(), httpRouteGVK)
	r.apiReader = local.GetAPIReader()
	setupLog := ctrl.Log.WithName("horizon-setup")
	if r.gatewayAPIAvailable {
		setupLog.Info("Gateway API detected; enabling HTTPRoute watch and reconciliation")
	} else {
		setupLog.Info("Gateway API not installed; HTTPRoute watch disabled, spec.gateway will be rejected via HTTPRouteReady condition")
	}

	// Register the Horizon field indexer before Watches so
	// secretToHorizonMapper can rely on it for its MatchingFields lookup. The
	// index goes on the LOCAL field indexer, not mgr's: with a provider
	// configured, the multicluster manager's field indexer registers against
	// the provider clusters, which hold no Horizon CR. Registration stays
	// local by contract: the indexes are on CR kinds, which exist on the
	// management cluster alone, and every request the watches emit is
	// pinned to that cluster (LocalRequests / RemoteRequests,
	// internal/common/multicluster/watch.go), so a remote event resolves to
	// its CR through the local cache. Registering on the fleet would fail
	// the engagement of every target cluster, because the kubeconfig
	// provider applies its stored indexes while engaging one.
	if err := registerSecretNameIndex(context.Background(), local.GetFieldIndexer()); err != nil {
		return err
	}

	// Every leg watching the management cluster carries both engage options
	// below; see their definition for why an unpinned leg would stop watching
	// it once a provider is configured.
	engageLocal := commonmulticluster.EngageLocalCluster
	engageNoProviders := commonmulticluster.EngageNoProviderClusters

	// Every leg watching a target cluster is engaged on all of them, not on the
	// ones some CR names, so it has to drop the events belonging to a CR that
	// projects somewhere else (see commonmulticluster.RemoteRequests).
	targets := commonmulticluster.TargetClusterOf(local.GetClient(),
		func(horizon *horizonv1alpha1.Horizon) *commonv1.TargetClusterRefSpec {
			return horizon.Spec.TargetClusterRef
		})

	b := mcbuilder.ControllerManagedBy(mgr).
		// Shared controller options: MaxConcurrentReconciles lets independent
		// CRs reconcile in parallel, and the tuned RateLimiter caps per-item
		// failure backoff at 30s (see bootstrap.TypedControllerOptions).
		WithOptions(bootstrap.TypedControllerOptions[mcreconcile.Request](r.MaxConcurrentReconciles)).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate).
		For(&horizonv1alpha1.Horizon{}, mcbuilder.WithPredicates(watch.CRUpdatePredicate()), engageLocal, engageNoProviders).
		Owns(&appsv1.Deployment{}, engageLocal, engageNoProviders).
		Owns(&corev1.Service{}, engageLocal, engageNoProviders).
		Owns(&corev1.ConfigMap{}, engageLocal, engageNoProviders).
		Owns(&policyv1.PodDisruptionBudget{}, engageLocal, engageNoProviders).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}, engageLocal, engageNoProviders).
		Owns(&networkingv1.NetworkPolicy{}, engageLocal, engageNoProviders)

	if r.gatewayAPIAvailable {
		b = b.Owns(&gatewayv1.HTTPRoute{}, engageLocal, engageNoProviders)
	}

	// The same children, once more, on the clusters a CR can project onto. Owns
	// cannot see them: an owner reference does not cross a cluster boundary, so
	// the ownership labels are what maps a child back to its CR. No leg carries
	// a predicate, mirroring what Owns admits locally.
	b, err := commonmulticluster.AddRemoteChildWatches(b, local.GetScheme(), &horizonv1alpha1.Horizon{},
		targets, HorizonRemoteChildKinds, nil)
	if err != nil {
		return err
	}

	// Watch Secrets and map to the Horizon CRs that reference them.
	// The ESO-managed SECRET_KEY Secret is owned by the ExternalSecret
	// controller, not by the Horizon CR, so EnqueueRequestForOwner would
	// never match it. This MapFunc performs an indexed reverse lookup.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &corev1.Secret{},
		secretToHorizonMapper(local.GetClient()))
	if err != nil {
		return err
	}

	// Watch both the cluster-scoped ClusterSecretStore and the namespaced
	// SecretStore a Horizon can select via spec.secretStoreRef, so the
	// operator reflects upstream secret-backend outages in SecretsReady as
	// soon as ESO flips the selected store's Ready condition, rather than
	// waiting for the next periodic requeue. Each mapper enqueues only the
	// Horizons whose effective store ref matches the changed store.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &esov1.ClusterSecretStore{},
		storeToHorizonMapper(local.GetClient(), commonv1.SecretStoreKindCluster))
	if err != nil {
		return err
	}
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &esov1.SecretStore{},
		storeToHorizonMapper(local.GetClient(), commonv1.SecretStoreKindNamespaced))
	if err != nil {
		return err
	}

	return b.
		// The default wrapper turns an error matching
		// multicluster.ErrClusterNotFound into a successful reconcile. This
		// operator instead surfaces an unresolvable cluster as a
		// TargetClusterUnavailable condition and requeues, so the wrapper stays
		// off and the error semantics remain byte-identical to the classic
		// builder's.
		WithClusterNotFoundWrapper(false).
		Complete(commonmulticluster.LocalReconciler(r))
}
