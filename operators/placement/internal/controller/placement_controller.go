// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the Placement reconciler.
package controller

import (
	"context"
	"fmt"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/forge/internal/common/bootstrap"
	"github.com/c5c3/forge/internal/common/database"
	"github.com/c5c3/forge/internal/common/gateway"
	"github.com/c5c3/forge/internal/common/healthcheck"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	"github.com/c5c3/forge/internal/common/watch"
	placementv1alpha1 "github.com/c5c3/forge/operators/placement/api/v1alpha1"
	placementmetrics "github.com/c5c3/forge/operators/placement/internal/metrics"
)

// PlacementSecretNameIndexKey is the field-indexer key under which Placement
// CRs are indexed by the union of their referenced Secret names
// (spec.serviceUser.secretRef.name and spec.database.secretRef.name). Used by
// SetupWithManager to register the indexer and by secretToPlacementMapper to
// perform an O(1) reverse lookup instead of an unfiltered List of all Placement
// CRs in the namespace.
// #nosec G101 -- field-indexer key (a JSONPath-like field selector), not a credential.
const PlacementSecretNameIndexKey = "spec.secretRefs.name"

// placementSecretNameExtractor is the controller-runtime IndexerFunc registered
// under PlacementSecretNameIndexKey. It returns the deduplicated, non-empty
// union of Secret names referenced by a Placement CR —
// spec.serviceUser.secretRef.name and spec.database.secretRef.name — so the
// field indexer can resolve a Secret event to the referencing CR(s) without
// listing every Placement in the namespace.
func placementSecretNameExtractor(obj client.Object) []string {
	p, ok := obj.(*placementv1alpha1.Placement)
	if !ok {
		// controller-runtime should never call us with the wrong type; a nil
		// return is safer than a panic if it ever does.
		return nil
	}
	serviceUser := p.Spec.ServiceUser.SecretRef.Name
	dbName := p.Spec.Database.SecretRef.Name

	names := make([]string, 0, 2)
	if serviceUser != "" {
		names = append(names, serviceUser)
	}
	if dbName != "" && dbName != serviceUser {
		names = append(names, dbName)
	}
	return names
}

// registerPlacementIndexes registers the Placement field indexer under
// PlacementSecretNameIndexKey with the given FieldIndexer.
// PlacementReconciler.SetupWithManager calls this once against
// mgr.GetFieldIndexer() so secretToPlacementMapper can resolve a Secret event to
// the referencing Placement CRs via an O(1) reverse lookup instead of an
// unfiltered namespace-scoped List.
func registerPlacementIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	return watch.RegisterSecretNameIndex(ctx, indexer, &placementv1alpha1.Placement{}, PlacementSecretNameIndexKey, placementSecretNameExtractor)
}

// subConditionTypes lists the condition types set by the individual Placement
// sub-reconcilers. The aggregate Ready condition is True only when all of these
// are True. Every parallel-group member (HTTPRoute, HealthCheck, HPA,
// NetworkPolicy) always sets its condition — configured-ready, NotRequired, or
// waiting — so a gateway-less or autoscaling-less cluster still resolves the
// aggregate (the NotRequired paths report True), exactly as the sibling
// operators aggregate their optional conditions.
var subConditionTypes = []string{
	"SecretsReady",
	"DatabaseReady",
	"DeploymentReady",
	"PlacementAPIReady",
	"HPAReady",
	"NetworkPolicyReady",
	"HTTPRouteReady",
}

// placementFinalizer blocks removal of a Placement CR from etcd until the
// MariaDB Database, User, and Grant CRs it owns have been issued a Delete, so
// the schema teardown is triggered before the owner-ref chain disappears. It is
// the single source of truth for Reconcile, the finalizer handler, and tests.
const placementFinalizer = "placement.openstack.c5c3.io/finalizer"

// httpRouteGVK identifies the HTTPRoute kind the operator watches when Gateway
// API is installed. Availability is probed at setup time via the shared
// gateway.IsGVKAvailable RESTMapper probe.
var httpRouteGVK = schema.GroupVersionKind{
	Group:   gatewayv1.GroupVersion.Group,
	Version: gatewayv1.GroupVersion.Version,
	Kind:    "HTTPRoute",
}

// placementSkeleton bundles the shared controller-skeleton glue (Ready
// aggregation, no-op-skipping status writes, config-failure marking) with
// placement's sub-condition vocabulary and status accessor. The wrapper helpers
// below delegate to it.
var placementSkeleton = commonreconcile.Skeleton[*placementv1alpha1.Placement, placementv1alpha1.PlacementStatus]{
	SubConditionTypes: subConditionTypes,
	Conditions:        func(p *placementv1alpha1.Placement) *[]metav1.Condition { return &p.Status.Conditions },
}

// conditionReasonConfigError is the SecretsReady=False reason set when
// reconcileConfig fails. Config artefacts (the rendered placement.conf
// ConfigMap) gate the same downstream graph as the upstream credential
// Secrets, so failures reuse SecretsReady rather than a dedicated condition —
// matching reconcileDBConnectionSecret's Config→SecretsReady mapping.
const conditionReasonConfigError = "ConfigError"

// markConfigFailed flips SecretsReady to False so a reconcileConfig failure
// cannot leave the aggregate Ready condition stale-True at the new
// ObservedGeneration. It mirrors the sibling operators' markConfigFailed helper.
func markConfigFailed(placement *placementv1alpha1.Placement, err error) {
	placementSkeleton.MarkFailed(placement, "SecretsReady", conditionReasonConfigError, err)
}

// PlacementReconciler reconciles a Placement object. Its fields mirror the
// sibling service reconcilers' core set.
type PlacementReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// OperatorNamespace is the Namespace the operator Pod runs in (resolved at
	// startup by bootstrap.DetectOperatorNamespace). The networkpolicy step
	// appends an ingress peer for this Namespace so the operator's own health
	// check can reach the Placement API. Empty when the namespace could not be
	// determined, in which case no operator-namespace peer is added.
	OperatorNamespace string

	// MaxConcurrentReconciles bounds how many Placement CRs reconcile
	// concurrently. It is threaded from the --max-concurrent-reconciles flag and
	// applied to the controller's controller.Options in SetupWithManager. A value
	// <= 0 falls back to bootstrap.DefaultMaxConcurrentReconciles inside
	// bootstrap.ControllerOptions, so the zero value is safe.
	MaxConcurrentReconciles int

	// HTTPClient is the health-check client seam. Production leaves it nil so the
	// health check uses http.DefaultClient; tests inject a stub transport.
	HTTPClient healthcheck.HTTPDoer

	// apiReader is set during SetupWithManager from mgr.GetAPIReader(): a direct,
	// uncached reader. The deployment step latches the two-phase narrowing of the
	// API Service selector on the live Service, and the informer cache can still
	// hold the pre-narrowing state right after the narrowing write — a latch
	// decided from it could widen the selector back. Nil in unit tests that
	// construct the reconciler without a manager; those fall back to the
	// (read-your-writes) fake client.
	apiReader client.Reader

	// gatewayAPIAvailable is set during SetupWithManager from the cluster's
	// RESTMapper and indicates whether the gateway.networking.k8s.io/v1 HTTPRoute
	// CRD is installed. When false, the controller skips the HTTPRoute watch
	// entirely so it does not crash on a missing kind.
	gatewayAPIAvailable bool

	// healthProbeCache memoizes the last successful Placement API probe per CR
	// (shared TTL probe cache) so a steady-state reconcile does not fire a
	// synchronous HTTP GET on every pass. The cache's internal mutex guards
	// concurrent access under MaxConcurrentReconciles > 1.
	healthProbeCache healthcheck.ProbeCache
}

// +kubebuilder:rbac:groups=placement.openstack.c5c3.io,resources=placements,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=placement.openstack.c5c3.io,resources=placements/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=placement.openstack.c5c3.io,resources=placements/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services;configmaps;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=databases;users;grants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=mariadbs,verbs=get;list;watch
// +kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=external-secrets.io,resources=clustersecretstores;secretstores,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=get
// +kubebuilder:rbac:groups=scheduling.k8s.io,resources=priorityclasses,verbs=get;list;watch

// Reconcile is the main reconciliation loop for the Placement CR. It fetches the
// CR, drives the finalizer-gated deletion path, ensures the finalizer, then runs
// the sub-reconciler pipeline. Every exit funnels through updateStatus, which
// re-aggregates the Ready condition and stamps ObservedGeneration.
func (r *PlacementReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var placement placementv1alpha1.Placement
	if err := r.Get(ctx, req.NamespacedName, &placement); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("Placement resource not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching Placement: %w", err)
	}

	// Handle deletion via the finalizer: issue Delete on the MariaDB CRs, then
	// release the finalizer once no live (not-yet-deleted) resource remains. The
	// deletion path sets no conditions, so it returns directly without
	// updateStatus.
	if !placement.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &placement)
	}

	// Ensure the finalizer is installed before any sub-reconciler runs so a
	// deletion issued before the next pass still funnels through reconcileDelete.
	// Requeuing after the Update guarantees the next reconcile observes the
	// persisted finalizer rather than the in-memory copy.
	if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &placement, placementFinalizer); err != nil {
		return ctrl.Result{}, err
	} else if added {
		return ctrl.Result{Requeue: true}, nil
	}

	// Snapshot the persisted status so updateStatus can skip the write when a
	// pass leaves status unchanged (no write → no watch event → no
	// resourceVersion churn). Taken after the finalizer add so an early requeue
	// there does not race a status write.
	statusBefore := placement.Status.DeepCopy()

	result, err := commonreconcile.RunPipeline(ctx, instrumenter.Instrument, r.pipelineSteps(&placement))
	return r.updateStatus(ctx, &placement, statusBefore, result, err)
}

// pipelineSteps returns the ordered sub-reconciler pipeline for one Placement.
// Each step runs in dependency order; the first to return a non-zero result or
// an error short-circuits the chain and funnels through updateStatus.
//
// It is a method rather than a literal inside Reconcile so the drift guard can
// enumerate the step names without running a reconcile.
func (r *PlacementReconciler) pipelineSteps(placement *placementv1alpha1.Placement) []commonreconcile.Step {
	// The values one sub-reconciler hands to a later one within a single
	// reconcile pass, captured by the step closures. configMapName names the
	// rendered config ConfigMap produced by reconcileConfig; the database step
	// mounts it in the migration Job and the deployment step mounts it in the API
	// pods. authtokenDigest and dsnDigest are the content digests of the
	// service-user password and the assembled DSN; the deployment step stamps
	// them into pod-template annotations so a rotated credential rolls the pods.
	var configMapName, authtokenDigest, dsnDigest string

	return []commonreconcile.Step{
		{Name: "Secrets", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, authtokenDigest, err = r.reconcileSecrets(ctx, placement)
			return res, err
		}},
		// reconcileDBConnectionSecret materialises the DB URL into the derived
		// <placement.Name>-db-connection Secret. It runs after Secrets (upstream
		// credentials must be synced) and before Config; failures set
		// SecretsReady=False, the same condition reconcileSecrets uses.
		{Name: "DBConnectionSecret", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, dsnDigest, err = r.reconcileDBConnectionSecret(ctx, placement)
			return res, err
		}},
		// reconcileConfig renders placement.conf into an immutable ConfigMap. It
		// self-marks SecretsReady=False on failure via markConfigFailed, so the
		// wrapper only threads the result.
		{Name: "Config", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, configMapName, err = r.reconcileConfig(ctx, placement)
			return res, err
		}},
		// reconcileDatabase provisions the schema, gates the requested OpenStack
		// release against the installed one, and runs the db-sync Job against the
		// rendered config.
		{Name: "Database", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDatabase(ctx, placement, configMapName)
		}},
		// reconcileDeployment projects the API Deployment, its Service, and the
		// PodDisruptionBudget. It runs after Database so the pods only start once
		// the schema they query exists.
		{Name: "Deployment", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDeployment(ctx, placement, configMapName, dsnDigest, authtokenDigest)
		}},
		// Once the Deployment/Service outputs are in place, HTTPRoute,
		// HealthCheck, HPA, and NetworkPolicy have no inter-dependency and run
		// concurrently. Each member sets exactly one condition type; the group
		// self-instruments its members, so this step carries no sub_reconciler
		// name.
		{Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileParallelGroup(ctx, placement, r.parallelSteps())
		}},
	}
}

// parallelSteps returns the members of the post-deployment parallel group. Each
// member sets exactly one condition type and receives its own copy of the CR, so
// none of them reads a value another one produces.
func (r *PlacementReconciler) parallelSteps() []commonreconcile.ParallelStep[*placementv1alpha1.Placement] {
	return []commonreconcile.ParallelStep[*placementv1alpha1.Placement]{
		{
			Name:          "HTTPRoute",
			ConditionType: conditionTypeHTTPRouteReady,
			Fn: func(ctx context.Context, p *placementv1alpha1.Placement) (ctrl.Result, error) {
				return r.reconcileHTTPRoute(ctx, p)
			},
		},
		{
			Name:          "HealthCheck",
			ConditionType: conditionTypePlacementAPIReady,
			Fn: func(ctx context.Context, p *placementv1alpha1.Placement) (ctrl.Result, error) {
				return r.reconcileHealthCheck(ctx, p)
			},
		},
		{
			Name:          "HPA",
			ConditionType: "HPAReady",
			Fn: func(ctx context.Context, p *placementv1alpha1.Placement) (ctrl.Result, error) {
				return r.reconcileHPA(ctx, p)
			},
		},
		{
			Name:          "NetworkPolicy",
			ConditionType: conditionTypeNetworkPolicyReady,
			Fn: func(ctx context.Context, p *placementv1alpha1.Placement) (ctrl.Result, error) {
				return r.reconcileNetworkPolicy(ctx, p)
			},
		},
	}
}

// reconcileDelete drives the finalizer cleanup when the Placement CR is being
// deleted. It is a no-op when the finalizer is absent. Otherwise it issues
// Delete on the MariaDB Database/User/Grant CRs (idempotent, NotFound-tolerant)
// and, while at least one of them was still live (not yet issued a Delete),
// holds the finalizer for one more pass so the schema teardown is triggered
// before the owner-ref chain disappears. Once no live resource remains it drops
// the per-CR metrics, evicts the health-probe cache, and releases the finalizer.
func (r *PlacementReconciler) reconcileDelete(ctx context.Context, placement *placementv1alpha1.Placement) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(placement, placementFinalizer) {
		return ctrl.Result{}, nil
	}

	key := client.ObjectKey{Name: placement.Name, Namespace: placement.Namespace}

	// Observe whether any MariaDB CR is still live BEFORE issuing the Delete: a
	// Delete flips DeletionTimestamp, so a post-Delete check would always report
	// none-live and release immediately. Gating on the pre-Delete observation
	// keeps the CR alive one extra pass so the teardown is actually triggered.
	hasLive, err := database.HasLiveResources(ctx, r.Client, key)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := database.FinalizeResources(ctx, r.Client, key); err != nil {
		return ctrl.Result{}, err
	}

	if hasLive {
		r.Recorder.Event(placement, corev1.EventTypeNormal, "FinalizingDatabase",
			"Cleaning up MariaDB Database, User, and Grant before removing Placement")
		return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
	}

	r.Recorder.Event(placement, corev1.EventTypeNormal, "DatabaseFinalized",
		"MariaDB Database, User, and Grant marked for deletion; releasing finalizer")

	controllerutil.RemoveFinalizer(placement, placementFinalizer)
	if err := r.Update(ctx, placement); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	placementmetrics.DeleteForPlacement(placement.Name, placement.Namespace)
	// Drop the per-CR health-probe cache so a CR recreated under the same
	// name/namespace never serves a stale probe keyed on the deleted CR's UID.
	r.healthProbeCache.Evict(key)
	return ctrl.Result{}, nil
}

// updateStatus persists the current status conditions and returns the given
// result and error, delegating to the shared skeleton: the write is skipped when
// the pass left status semantically unchanged from the statusBefore snapshot, a
// failed write is joined with reconcileErr, and the mutate hook re-aggregates the
// Ready condition on every persist and stamps status.observedGeneration.
func (r *PlacementReconciler) updateStatus(ctx context.Context, placement *placementv1alpha1.Placement, statusBefore *placementv1alpha1.PlacementStatus, result ctrl.Result, reconcileErr error) (ctrl.Result, error) {
	return placementSkeleton.UpdateStatus(ctx, r.Client, placement, statusBefore, &placement.Status, func() {
		placement.Status.ObservedGeneration = placement.Generation
	}, result, reconcileErr)
}

// setReadyCondition sets the aggregate Ready condition based on all
// sub-conditions, delegating to the shared skeleton with placement's
// sub-condition vocabulary.
func setReadyCondition(placement *placementv1alpha1.Placement) {
	placementSkeleton.SetReady(placement)
}

// reconcileParallelGroup runs the given sub-reconcilers concurrently, delegating
// to the shared skeleton: each member operates on its own DeepCopy of the
// Placement CR, conditions from every member (including those that succeeded
// before a peer failed) are merged back into the primary placement, and on
// success the shortest non-zero RequeueAfter is returned. Members instrument
// individually via instrumenter.Instrument.
func (r *PlacementReconciler) reconcileParallelGroup(
	ctx context.Context,
	placement *placementv1alpha1.Placement,
	subs []commonreconcile.ParallelStep[*placementv1alpha1.Placement],
) (ctrl.Result, error) {
	return placementSkeleton.RunParallelGroup(ctx, placement, instrumenter.Instrument, subs)
}

// SetupWithManager registers the PlacementReconciler with the controller
// manager. It probes Gateway API availability, registers the Placement field
// index, and wires the owned resources and cross-resource watches.
func (r *PlacementReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Detect whether the Gateway API CRD is installed. spec.gateway is optional,
	// so the operator must run on clusters without Gateway API. Adding
	// Owns(HTTPRoute) unconditionally would fail at Start with "no matches for
	// kind HTTPRoute" when the CRD is missing, blocking every Placement CR.
	r.gatewayAPIAvailable = gateway.IsGVKAvailable(mgr.GetRESTMapper(), httpRouteGVK)
	r.apiReader = mgr.GetAPIReader()
	setupLog := ctrl.Log.WithName("placement-setup")
	if r.gatewayAPIAvailable {
		setupLog.Info("Gateway API detected; enabling HTTPRoute watch and reconciliation")
	} else {
		setupLog.Info("Gateway API not installed; HTTPRoute watch disabled, spec.gateway will be rejected via HTTPRouteReady condition")
	}

	// Register the Placement field indexer before Watches so
	// secretToPlacementMapper can rely on it for its MatchingFields lookup.
	if err := registerPlacementIndexes(context.Background(), mgr.GetFieldIndexer()); err != nil {
		return err
	}

	b := ctrl.NewControllerManagedBy(mgr).
		// Shared controller options: MaxConcurrentReconciles lets independent CRs
		// reconcile in parallel, and the tuned RateLimiter caps per-item failure
		// backoff at 30s (see bootstrap.ControllerOptions).
		WithOptions(bootstrap.ControllerOptions(r.MaxConcurrentReconciles)).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate).
		For(&placementv1alpha1.Placement{}, builder.WithPredicates(watch.CRUpdatePredicate())).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&batchv1.Job{})

	if r.gatewayAPIAvailable {
		b = b.Owns(&gatewayv1.HTTPRoute{})
	}

	return b.
		// Watch Secrets and map to the Placement CRs that reference them.
		// ESO-managed Secrets are owned by the ExternalSecret controller, not the
		// Placement CR, so EnqueueRequestForOwner would never match them.
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(
			secretToPlacementMapper(mgr.GetClient()),
		)).
		// Watch the MariaDB cluster CR referenced by spec.database.clusterRef so
		// the operator reflects upstream database outages in DatabaseReady without
		// waiting for the next periodic requeue.
		Watches(&mariadbv1alpha1.MariaDB{}, handler.EnqueueRequestsFromMapFunc(
			mariaDBToPlacementMapper(mgr.GetClient()),
		)).
		// Watch both the cluster-scoped ClusterSecretStore and the namespaced
		// SecretStore a Placement can select via spec.secretStoreRef, so the
		// operator reflects upstream secret-backend outages in SecretsReady as soon
		// as ESO flips the selected store's Ready condition.
		Watches(&esov1.ClusterSecretStore{}, handler.EnqueueRequestsFromMapFunc(
			storeToPlacementMapper(mgr.GetClient(), commonv1.SecretStoreKindCluster),
		)).
		Watches(&esov1.SecretStore{}, handler.EnqueueRequestsFromMapFunc(
			storeToPlacementMapper(mgr.GetClient(), commonv1.SecretStoreKindNamespaced),
		)).
		Complete(r)
}
