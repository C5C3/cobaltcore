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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/gateway"
	"github.com/c5c3/cobaltcore/internal/common/healthcheck"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	placementv1alpha1 "github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1"
	placementmetrics "github.com/c5c3/cobaltcore/operators/placement/internal/metrics"
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

	// Resolver resolves the target cluster a Placement CR names in
	// spec.targetClusterRef into the client its children are read and written
	// with. Nil means always-local: every CR keeps its children on the
	// management cluster, which is what single-cluster tests and deployments
	// want.
	Resolver commonmulticluster.ClusterResolver

	// apiReader is set during SetupWithManager from mgr.GetAPIReader(): a direct,
	// uncached reader. The deployment step latches the two-phase narrowing of the
	// API Service selector on the live Service, and the informer cache can still
	// hold the pre-narrowing state right after the narrowing write — a latch
	// decided from it could widen the selector back. Nil in unit tests that
	// construct the reconciler without a manager; those fall back to the
	// (read-your-writes) fake client.
	apiReader client.Reader

	// gatewayAPIAvailable is set during SetupWithManager from the management
	// cluster's RESTMapper and indicates whether the
	// gateway.networking.k8s.io/v1 HTTPRoute CRD is installed there. Two
	// consumers read it: the local HTTPRoute watch leg, which SetupWithManager
	// skips when false so the controller does not crash on a missing kind, and
	// commonmulticluster.ChildrenServeKind, which answers with it for local
	// children while probing the target cluster's RESTMapper for remote ones.
	gatewayAPIAvailable bool

	// healthProbeCache memoizes the last successful Placement API probe per CR
	// (shared TTL probe cache) so a steady-state reconcile does not fire a
	// synchronous HTTP GET on every pass. The cache's internal mutex guards
	// concurrent access under MaxConcurrentReconciles > 1.
	healthProbeCache healthcheck.ProbeCache
}

// PlacementRemoteChildKinds are the kinds a Placement CR projects into the
// namespace of the target cluster it names, and the kinds
// reconcileDeleteRemoteChildren sweeps by ownership label when that CR is
// deleted. Nothing on the target cluster collects them, so a kind missing from
// this list is a kind that keeps running after its CR is gone.
//
// The list is cross-checked against the create verbs of the kubebuilder RBAC
// markers on this controller below: the operator can only leave behind what it
// is allowed to create. CronJob is the one kind the sibling operators sweep and
// this one does not: the markers grant create on jobs alone, because placement
// renders no CronJob.
var PlacementRemoteChildKinds = []schema.GroupVersionKind{
	appsv1.SchemeGroupVersion.WithKind("Deployment"),
	corev1.SchemeGroupVersion.WithKind("Service"),
	corev1.SchemeGroupVersion.WithKind("ConfigMap"),
	corev1.SchemeGroupVersion.WithKind("Secret"),
	batchv1.SchemeGroupVersion.WithKind("Job"),
	policyv1.SchemeGroupVersion.WithKind("PodDisruptionBudget"),
	autoscalingv2.SchemeGroupVersion.WithKind("HorizontalPodAutoscaler"),
	networkingv1.SchemeGroupVersion.WithKind("NetworkPolicy"),
	httpRouteGVK,
	mariadbv1alpha1.GroupVersion.WithKind("Database"),
	mariadbv1alpha1.GroupVersion.WithKind("User"),
	mariadbv1alpha1.GroupVersion.WithKind("Grant"),
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
	// cleanup itself sets no conditions, so it returns directly without
	// updateStatus; only the hold on an unresolvable target below reports.
	//
	// It comes before the target-cluster resolution below and uses the deletion
	// variant, which never fails the pass: a CR whose cluster was deregistered
	// after the finalizer went on would otherwise short-circuit on the
	// unresolvable ref on every pass and stay Terminating forever. A target that
	// has not resolved yet requeues instead of being given up on — engagement is
	// asynchronous, so an operator restart looks exactly like a deregistration
	// until the provider has synced.
	if !placement.DeletionTimestamp.IsZero() {
		children, wait := commonmulticluster.ResolveChildrenClientForDeletion(
			ctx, r.Resolver, r.Client, placement.Spec.TargetClusterRef, *placement.DeletionTimestamp)
		if wait {
			// The hold goes on the CR, not only into the operator's log. It is a
			// deliberate state a CR can sit in for minutes, and "Terminating,
			// waiting on the target cluster" has to be distinguishable from a
			// wedged finalizer without correlating logs across replicas. This exit
			// precedes the pipeline's status snapshot below, so it takes its own
			// baseline for the skip-unchanged write.
			statusBefore := placement.Status.DeepCopy()
			placementSkeleton.MarkFailed(&placement, "SecretsReady",
				commonmulticluster.TargetClusterUnavailable,
				fmt.Errorf("target cluster %s does not resolve; waiting at least %s before abandoning its children",
					placement.Spec.TargetClusterRef.Name, commonmulticluster.AbandonAfter))
			return r.updateStatus(ctx, &placement, statusBefore,
				ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil)
		}
		if result, err := r.reconcileDelete(ctx, children, &placement); !result.IsZero() || err != nil {
			return result, err
		}
		// The label-selected sweep runs after the named MariaDB cleanup, never
		// before it: that flow waits one pass on the CRs it deletes by name, and a
		// sweep running first would delete them out from under it. The ordering
		// mirrors the local one, where the garbage collection cascade starts only
		// once every finalizer has been released.
		if err := r.reconcileDeleteRemoteChildren(ctx, children, &placement); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Resolve the client every child object of this CR is read and written with.
	// The embedded client stays on the management cluster (the CR, its status,
	// and its finalizer live there); children carries everything the CR projects
	// into the target cluster. The resolution runs before the finalizer is added
	// so a CR naming an unresolvable cluster stays clean of finalizers: nothing
	// was created for it, so there is nothing to clean up, and a finalizer would
	// only block its deletion.
	children, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client, placement.Spec.TargetClusterRef)
	if err != nil {
		// This exit precedes the pipeline's status snapshot below, so it takes its
		// own baseline for the skip-unchanged write.
		statusBefore := placement.Status.DeepCopy()
		placementSkeleton.MarkFailed(&placement, "SecretsReady", commonmulticluster.TargetClusterUnavailable, err)
		return r.updateStatus(ctx, &placement, statusBefore,
			ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil)
	}

	// Ensure the finalizer is installed before any sub-reconciler runs so a
	// deletion issued before the next pass still funnels through reconcileDelete.
	// Requeuing after the Update guarantees the next reconcile observes the
	// persisted finalizer rather than the in-memory copy.
	if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &placement, placementFinalizer); err != nil {
		return ctrl.Result{}, err
	} else if added {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
	}

	// The remote-children finalizer goes on only when the CR projects onto a
	// target cluster. A local CR keeps the garbage collection cascade, which
	// reaps its children from their owner references, so it has nothing for this
	// finalizer to hold the CR open for. spec.targetClusterRef is immutable, so
	// the condition cannot flip under a live CR.
	if placement.Spec.TargetClusterRef != nil {
		if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &placement,
			commonmulticluster.RemoteChildrenFinalizer); err != nil {
			return ctrl.Result{}, err
		} else if added {
			return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
		}
	}

	// Snapshot the persisted status so updateStatus can skip the write when a
	// pass leaves status unchanged (no write → no watch event → no
	// resourceVersion churn). Taken after the finalizer add so an early requeue
	// there does not race a status write.
	statusBefore := placement.Status.DeepCopy()

	result, err := commonreconcile.RunPipeline(ctx, instrumenter.Instrument, r.pipelineSteps(children, &placement))
	return r.updateStatus(ctx, &placement, statusBefore, result, err)
}

// pipelineSteps returns the ordered sub-reconciler pipeline for one Placement.
// Each step runs in dependency order; the first to return a non-zero result or
// an error short-circuits the chain and funnels through updateStatus.
//
// It is a method rather than a literal inside Reconcile so the drift guard can
// enumerate the step names without running a reconcile.
func (r *PlacementReconciler) pipelineSteps(children client.Client, placement *placementv1alpha1.Placement) []commonreconcile.Step {
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
			res, authtokenDigest, err = r.reconcileSecrets(ctx, children, placement)
			return res, err
		}},
		// reconcileDBConnectionSecret materialises the DB URL into the derived
		// <placement.Name>-db-connection Secret. It runs after Secrets (upstream
		// credentials must be synced) and before Config; failures set
		// SecretsReady=False, the same condition reconcileSecrets uses.
		{Name: "DBConnectionSecret", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, dsnDigest, err = r.reconcileDBConnectionSecret(ctx, children, placement)
			return res, err
		}},
		// reconcileConfig renders placement.conf into an immutable ConfigMap. It
		// self-marks SecretsReady=False on failure via markConfigFailed, so the
		// wrapper only threads the result.
		{Name: "Config", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, configMapName, err = r.reconcileConfig(ctx, children, placement)
			return res, err
		}},
		// reconcileDatabase provisions the schema, gates the requested OpenStack
		// release against the installed one, and runs the db-sync Job against the
		// rendered config.
		{Name: "Database", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDatabase(ctx, children, placement, configMapName)
		}},
		// reconcileDeployment projects the API Deployment, its Service, and the
		// PodDisruptionBudget. It runs after Database so the pods only start once
		// the schema they query exists.
		{Name: "Deployment", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDeployment(ctx, children, placement, configMapName, dsnDigest, authtokenDigest)
		}},
		// Once the Deployment/Service outputs are in place, HTTPRoute,
		// HealthCheck, HPA, and NetworkPolicy have no inter-dependency and run
		// concurrently. Each member sets exactly one condition type; the group
		// self-instruments its members, so this step carries no sub_reconciler
		// name.
		{Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileParallelGroup(ctx, placement, r.parallelSteps(children))
		}},
	}
}

// parallelSteps returns the members of the post-deployment parallel group. Each
// member sets exactly one condition type and receives its own copy of the CR, so
// none of them reads a value another one produces.
func (r *PlacementReconciler) parallelSteps(children client.Client) []commonreconcile.ParallelStep[*placementv1alpha1.Placement] {
	return []commonreconcile.ParallelStep[*placementv1alpha1.Placement]{
		{
			Name:          "HTTPRoute",
			ConditionType: conditionTypeHTTPRouteReady,
			Fn: func(ctx context.Context, p *placementv1alpha1.Placement) (ctrl.Result, error) {
				return r.reconcileHTTPRoute(ctx, children, p)
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
				return r.reconcileHPA(ctx, children, p)
			},
		},
		{
			Name:          "NetworkPolicy",
			ConditionType: conditionTypeNetworkPolicyReady,
			Fn: func(ctx context.Context, p *placementv1alpha1.Placement) (ctrl.Result, error) {
				return r.reconcileNetworkPolicy(ctx, children, p)
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
//
// A nil children client means the target cluster this CR named is no longer
// registered. Its MariaDB CRs cannot be reached, so they stay behind on a
// cluster that has not resolved for the whole abandon window, and the finalizer
// is released anyway: holding it would only strand the CR in Terminating.
func (r *PlacementReconciler) reconcileDelete(ctx context.Context, children client.Client, placement *placementv1alpha1.Placement) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(placement, placementFinalizer) {
		return ctrl.Result{}, nil
	}

	key := client.ObjectKey{Name: placement.Name, Namespace: placement.Namespace}

	if children == nil {
		r.Recorder.Event(placement, corev1.EventTypeWarning, "RemoteChildrenAbandoned",
			"Target cluster is no longer registered; releasing the finalizer without deleting the MariaDB Database, User, and Grant on it")
	} else {
		// Observe whether any MariaDB CR is still live BEFORE issuing the Delete: a
		// Delete flips DeletionTimestamp, so a post-Delete check would always report
		// none-live and release immediately. Gating on the pre-Delete observation
		// keeps the CR alive one extra pass so the teardown is actually triggered.
		hasLive, err := database.HasLiveResources(ctx, children, key)
		if err != nil {
			return ctrl.Result{}, err
		}

		if err := database.FinalizeResources(ctx, children, key); err != nil {
			return ctrl.Result{}, err
		}

		if hasLive {
			r.Recorder.Event(placement, corev1.EventTypeNormal, "FinalizingDatabase",
				"Cleaning up MariaDB Database, User, and Grant before removing Placement")
			return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
		}

		r.Recorder.Event(placement, corev1.EventTypeNormal, "DatabaseFinalized",
			"MariaDB Database, User, and Grant marked for deletion; releasing finalizer")
	}

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

// reconcileDeleteRemoteChildren deletes everything this Placement projected onto
// the target cluster it names and releases the remote-children finalizer, as
// commonmulticluster.SweepRemoteChildren documents. reconcileDelete above
// deletes the three MariaDB CRs it tracks by name; this pass is what reaches the
// rest, selected on the ownership labels Claim stamped on them.
func (r *PlacementReconciler) reconcileDeleteRemoteChildren(ctx context.Context, children client.Client, placement *placementv1alpha1.Placement) error {
	return commonmulticluster.SweepRemoteChildren(ctx, r.Client, r.Resolver, r.Recorder, r.Scheme,
		placement, placement.Spec.TargetClusterRef, children, PlacementRemoteChildKinds)
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
func (r *PlacementReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	local := mgr.GetLocalManager()

	// Detect whether the Gateway API CRD is installed. spec.gateway is optional,
	// so the operator must run on clusters without Gateway API. Adding
	// Owns(HTTPRoute) unconditionally would fail at Start with "no matches for
	// kind HTTPRoute" when the CRD is missing, blocking every Placement CR.
	r.gatewayAPIAvailable = gateway.IsGVKAvailable(local.GetRESTMapper(), httpRouteGVK)
	r.apiReader = local.GetAPIReader()
	setupLog := ctrl.Log.WithName("placement-setup")
	if r.gatewayAPIAvailable {
		setupLog.Info("Gateway API detected; enabling HTTPRoute watch and reconciliation")
	} else {
		setupLog.Info("Gateway API not installed; HTTPRoute watch disabled, spec.gateway will be rejected via HTTPRouteReady condition")
	}

	// Register the Placement field indexer before Watches so
	// secretToPlacementMapper can rely on it for its MatchingFields lookup. The
	// index goes on the LOCAL field indexer, not mgr's: with a provider
	// configured, the multicluster manager's field indexer registers against the
	// provider clusters, which hold no Placement CR. Registration stays local
	// by contract: the indexes are on CR kinds, which exist on the management
	// cluster alone, and every request the watches emit is pinned to that
	// cluster (LocalRequests / RemoteRequests,
	// internal/common/multicluster/watch.go), so a remote event resolves to its
	// CR through the local cache. Registering on the fleet would fail the
	// engagement of every target cluster, because the kubeconfig provider
	// applies its stored indexes while engaging one.
	if err := registerPlacementIndexes(context.Background(), local.GetFieldIndexer()); err != nil {
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
		func(placement *placementv1alpha1.Placement) *commonv1.TargetClusterRefSpec {
			return placement.Spec.TargetClusterRef
		})

	b := mcbuilder.ControllerManagedBy(mgr).
		// Shared controller options: MaxConcurrentReconciles lets independent CRs
		// reconcile in parallel, and the tuned RateLimiter caps per-item failure
		// backoff at 30s (see bootstrap.TypedControllerOptions).
		WithOptions(bootstrap.TypedControllerOptions[mcreconcile.Request](r.MaxConcurrentReconciles)).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate).
		For(&placementv1alpha1.Placement{}, mcbuilder.WithPredicates(watch.CRUpdatePredicate()), engageLocal, engageNoProviders).
		Owns(&appsv1.Deployment{}, engageLocal, engageNoProviders).
		Owns(&corev1.Service{}, engageLocal, engageNoProviders).
		Owns(&corev1.ConfigMap{}, engageLocal, engageNoProviders).
		Owns(&corev1.Secret{}, engageLocal, engageNoProviders).
		Owns(&policyv1.PodDisruptionBudget{}, engageLocal, engageNoProviders).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}, engageLocal, engageNoProviders).
		Owns(&networkingv1.NetworkPolicy{}, engageLocal, engageNoProviders).
		Owns(&batchv1.Job{}, engageLocal, engageNoProviders)

	if r.gatewayAPIAvailable {
		b = b.Owns(&gatewayv1.HTTPRoute{}, engageLocal, engageNoProviders)
	}

	// The same children, once more, on the clusters a CR can project onto. Owns
	// cannot see them: an owner reference does not cross a cluster boundary, so
	// the ownership labels are what maps a child back to its CR. No leg carries a
	// predicate, mirroring what Owns admits locally.
	b, err := commonmulticluster.AddRemoteChildWatches(b, local.GetScheme(), &placementv1alpha1.Placement{},
		targets, PlacementRemoteChildKinds, nil)
	if err != nil {
		return err
	}

	// Watch Secrets and map to the Placement CRs that reference them.
	// ESO-managed Secrets are owned by the ExternalSecret controller, not the
	// Placement CR, so EnqueueRequestForOwner would never match them.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &corev1.Secret{},
		secretToPlacementMapper(local.GetClient()))
	if err != nil {
		return err
	}

	// Watch the MariaDB cluster CR referenced by spec.database.clusterRef so
	// the operator reflects upstream database outages in DatabaseReady without
	// waiting for the next periodic requeue.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &mariadbv1alpha1.MariaDB{},
		mariaDBToPlacementMapper(local.GetClient()))
	if err != nil {
		return err
	}

	// Watch both the cluster-scoped ClusterSecretStore and the namespaced
	// SecretStore a Placement can select via spec.secretStoreRef, so the
	// operator reflects upstream secret-backend outages in SecretsReady as soon
	// as ESO flips the selected store's Ready condition.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &esov1.ClusterSecretStore{},
		storeToPlacementMapper(local.GetClient(), commonv1.SecretStoreKindCluster))
	if err != nil {
		return err
	}
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &esov1.SecretStore{},
		storeToPlacementMapper(local.GetClient(), commonv1.SecretStoreKindNamespaced))
	if err != nil {
		return err
	}

	return b.
		// The default wrapper turns an error matching multicluster.ErrClusterNotFound
		// into a successful reconcile. This operator instead surfaces an
		// unresolvable cluster as a TargetClusterUnavailable condition and
		// requeues, so the wrapper stays off and the error semantics remain
		// byte-identical to the classic builder's.
		WithClusterNotFoundWrapper(false).
		Complete(commonmulticluster.LocalReconciler(r))
}
