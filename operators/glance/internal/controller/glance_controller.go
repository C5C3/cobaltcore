// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the Glance and GlanceBackend reconcilers.
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
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
	glancemetrics "github.com/c5c3/forge/operators/glance/internal/metrics"
)

// This file carries the field-index plumbing shared by the Glance and
// GlanceBackend controllers. The GlanceReconciler and its sub-reconciler chain
// are added below the index section in a later commit; keeping the index keys,
// extractors, and registration helpers here mirrors keystone_controller.go,
// where both controllers rely on a single index-registration site
// (GlanceReconciler.SetupWithManager, next commit) run before the backend
// controller starts.

// GlanceSecretNameIndexKey is the field-indexer key under which Glance CRs are
// indexed by the union of their referenced Secret names
// (spec.serviceUser.secretRef.name and spec.database.secretRef.name). Used by
// SetupWithManager to register the indexer and by secretToGlanceMapper to
// perform an O(1) reverse lookup instead of an unfiltered List of all Glance
// CRs in the namespace.
// #nosec G101 -- field-indexer key (a JSONPath-like field selector), not a credential.
const GlanceSecretNameIndexKey = "spec.secretRefs.name"

// glanceSecretNameExtractor is the controller-runtime IndexerFunc registered
// under GlanceSecretNameIndexKey. It returns the deduplicated, non-empty union
// of Secret names referenced by a Glance CR — currently
// spec.serviceUser.secretRef.name and spec.database.secretRef.name — so the
// field indexer can resolve a Secret event to the referencing CR(s) without
// listing every Glance in the namespace. It mirrors keystone's extractor for
// the database block (spec.database.secretRef.name only).
func glanceSecretNameExtractor(obj client.Object) []string {
	g, ok := obj.(*glancev1alpha1.Glance)
	if !ok {
		// controller-runtime should never call us with the wrong type; a nil
		// return is safer than a panic if it ever does.
		return nil
	}
	serviceUser := g.Spec.ServiceUser.SecretRef.Name
	dbName := g.Spec.Database.SecretRef.Name

	names := make([]string, 0, 2)
	if serviceUser != "" {
		names = append(names, serviceUser)
	}
	if dbName != "" && dbName != serviceUser {
		names = append(names, dbName)
	}
	return names
}

// registerGlanceIndexes registers the Glance field indexer under
// GlanceSecretNameIndexKey with the given FieldIndexer.
// GlanceReconciler.SetupWithManager calls this once against
// mgr.GetFieldIndexer() (next commit) so secretToGlanceMapper can resolve a
// Secret event to the referencing Glance CRs via an O(1) reverse lookup instead
// of an unfiltered namespace-scoped List.
func registerGlanceIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	return watch.RegisterSecretNameIndex(ctx, indexer, &glancev1alpha1.Glance{}, GlanceSecretNameIndexKey, glanceSecretNameExtractor)
}

// GlanceBackendGlanceRefIndexKey is the field-indexer key under which
// GlanceBackend CRs are indexed by spec.glanceRef.name. Used by the glance-side
// sub-reconciler (list the backends attached to one Glance) and
// glanceToGlanceBackendsMapper (fan a Glance event out to its backends).
const GlanceBackendGlanceRefIndexKey = "spec.glanceRef.name"

// GlanceBackendSecretNameIndexKey is the field-indexer key under which
// GlanceBackend CRs are indexed by their referenced S3 credentials Secret name.
// Used by the Secret mapper so a credential Secret change re-renders the owning
// Glance's config through the backend's parent.
// #nosec G101 -- field-indexer key (a JSONPath-like field selector), not a credential.
const GlanceBackendSecretNameIndexKey = "spec.secretRefs.name"

// glanceBackendGlanceRefExtractor is the controller-runtime IndexerFunc
// registered under GlanceBackendGlanceRefIndexKey: it maps a backend to its
// spec.glanceRef.name so an attached-backends list is an O(1) indexed lookup.
// Exported to tests so fake clients can register the identical index.
func glanceBackendGlanceRefExtractor(obj client.Object) []string {
	b, ok := obj.(*glancev1alpha1.GlanceBackend)
	if !ok || b.Spec.GlanceRef.Name == "" {
		return nil
	}
	return []string{b.Spec.GlanceRef.Name}
}

// glanceBackendSecretNameExtractor returns the S3 credentials Secret name a
// GlanceBackend references (nil for a wrong-type object or a nil S3 block).
// Indexing it means a rotated credential re-renders the owning Glance's config
// through the backend's parent.
func glanceBackendSecretNameExtractor(obj client.Object) []string {
	b, ok := obj.(*glancev1alpha1.GlanceBackend)
	if !ok || b.Spec.S3 == nil || b.Spec.S3.CredentialsSecretRef.Name == "" {
		return nil
	}
	return []string{b.Spec.S3.CredentialsSecretRef.Name}
}

// registerGlanceBackendIndexes registers the two GlanceBackend field indexers.
// It lives beside registerGlanceIndexes so index registration has a single
// site: GlanceReconciler.SetupWithManager runs before
// GlanceBackendReconciler.SetupWithManager (next commit), so both controllers
// can rely on the indexes. The returned error is wrapped with the index key so
// the registration site is identifiable in manager-startup failure logs.
func registerGlanceBackendIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &glancev1alpha1.GlanceBackend{}, GlanceBackendGlanceRefIndexKey,
		glanceBackendGlanceRefExtractor); err != nil {
		return fmt.Errorf("registering field indexer %q: %w", GlanceBackendGlanceRefIndexKey, err)
	}
	if err := indexer.IndexField(ctx, &glancev1alpha1.GlanceBackend{}, GlanceBackendSecretNameIndexKey,
		glanceBackendSecretNameExtractor); err != nil {
		return fmt.Errorf("registering field indexer %q: %w", GlanceBackendSecretNameIndexKey, err)
	}
	return nil
}

// subConditionTypes lists the condition types set by the individual Glance
// sub-reconcilers. The aggregate Ready condition is True only when all of these
// are True. Every parallel-group member (HTTPRoute, HealthCheck, HPA,
// NetworkPolicy) always sets its condition — configured-ready, NotRequired, or
// waiting — so a gateway-less or autoscaling-less cluster still resolves the
// aggregate (the NotRequired paths report True), exactly as keystone/horizon
// aggregate their optional conditions.
var subConditionTypes = []string{
	"SecretsReady",
	"BackendsReady",
	"DatabaseReady",
	"DeploymentReady",
	conditionTypeGlanceAPIReady,
	"HPAReady",
	conditionTypeNetworkPolicyReady,
	conditionTypeHTTPRouteReady,
}

// GlanceReconciler reconciles a Glance object. Its fields mirror
// KeystoneReconciler's core set; the pipeline (Reconcile/SetupWithManager) and
// the Gateway/cert-manager availability flags land with the workload
// sub-reconcilers in the next commit.
type GlanceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// OperatorNamespace is the Namespace the operator Pod runs in (resolved at
	// startup by bootstrap.DetectOperatorNamespace). The networkpolicy step
	// (next commit) appends an ingress peer for this Namespace so the operator's
	// own health check can reach the Glance API. Empty when the namespace could
	// not be determined, in which case no operator-namespace peer is added.
	OperatorNamespace string

	// MaxConcurrentReconciles bounds how many Glance CRs reconcile concurrently.
	// It is threaded from the --max-concurrent-reconciles flag and applied to
	// the controller's controller.Options in SetupWithManager. A value <= 0 falls
	// back to bootstrap.DefaultMaxConcurrentReconciles inside
	// bootstrap.ControllerOptions, so the zero value is safe.
	MaxConcurrentReconciles int

	// HTTPClient is the health-check client seam. Production leaves it nil so the
	// health check uses http.DefaultClient; tests inject a stub transport.
	HTTPClient HTTPDoer

	// gatewayAPIAvailable is set during SetupWithManager from the cluster's
	// RESTMapper and indicates whether the gateway.networking.k8s.io/v1 HTTPRoute
	// CRD is installed. When false, the controller skips the HTTPRoute watch
	// entirely so it does not crash on a missing kind, and reconcileHTTPRoute
	// surfaces a clear HTTPRouteReady=False condition if the user nonetheless sets
	// spec.gateway.
	gatewayAPIAvailable bool

	// healthProbeCache memoizes the last successful Glance API probe per CR
	// (shared TTL probe cache) so a steady-state reconcile does not fire a
	// synchronous HTTP GET on every pass. A cache hit re-upserts
	// GlanceAPIReady=True without probing; any probe error or non-2xx evicts the
	// entry. The cache's internal mutex guards concurrent access under
	// MaxConcurrentReconciles > 1.
	healthProbeCache healthcheck.ProbeCache
}

// glanceFinalizer blocks removal of a Glance CR from etcd until the MariaDB
// Database, User, and Grant CRs it owns have been issued a Delete, so the schema
// teardown is triggered before the owner-ref chain disappears. It is the single
// source of truth for Reconcile, the finalizer handler, and tests.
const glanceFinalizer = "glance.openstack.c5c3.io/finalizer"

// httpRouteGVK identifies the HTTPRoute kind the operator watches when Gateway
// API is installed. Availability is probed at setup time via the shared
// gateway.IsGVKAvailable RESTMapper probe.
var httpRouteGVK = schema.GroupVersionKind{
	Group:   gatewayv1.GroupVersion.Group,
	Version: gatewayv1.GroupVersion.Version,
	Kind:    "HTTPRoute",
}

// glanceSkeleton bundles the shared controller-skeleton glue (Ready
// aggregation, no-op-skipping status writes, config-failure marking) with
// glance's sub-condition vocabulary and status accessor. The wrapper helpers
// below delegate to it; the pipeline wiring that also uses it (updateStatus,
// RunParallelGroup) lands in the next commit.
var glanceSkeleton = commonreconcile.Skeleton[*glancev1alpha1.Glance, glancev1alpha1.GlanceStatus]{
	SubConditionTypes: subConditionTypes,
	Conditions:        func(g *glancev1alpha1.Glance) *[]metav1.Condition { return &g.Status.Conditions },
}

// conditionReasonConfigError is the SecretsReady=False reason set when
// reconcileConfig fails. Config artefacts (the rendered glance-api.conf
// ConfigMap) gate the same downstream graph as the upstream credential
// Secrets, so failures reuse SecretsReady rather than a dedicated condition —
// matching reconcileDBConnectionSecret's Config→SecretsReady mapping.
const conditionReasonConfigError = "ConfigError"

// markConfigFailed flips SecretsReady to False so a reconcileConfig failure
// cannot leave the aggregate Ready condition stale-True at the new
// ObservedGeneration. It mirrors keystone's markConfigFailed helper.
func markConfigFailed(glance *glancev1alpha1.Glance, err error) {
	glanceSkeleton.MarkFailed(glance, "SecretsReady", conditionReasonConfigError, err)
}

// +kubebuilder:rbac:groups=glance.openstack.c5c3.io,resources=glances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=glance.openstack.c5c3.io,resources=glances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=glance.openstack.c5c3.io,resources=glances/finalizers,verbs=update
// +kubebuilder:rbac:groups=glance.openstack.c5c3.io,resources=glancebackends,verbs=get;list;watch
// +kubebuilder:rbac:groups=glance.openstack.c5c3.io,resources=glancebackends/status,verbs=get;update;patch
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

// Reconcile is the main reconciliation loop for the Glance CR. It fetches the
// CR, drives the finalizer-gated deletion path, ensures the finalizer, then runs
// the sub-reconciler pipeline threading the digests, projection, and config
// artefacts through the closures. Every exit funnels through updateStatus, which
// re-aggregates the Ready condition and stamps ObservedGeneration.
func (r *GlanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var glance glancev1alpha1.Glance
	if err := r.Get(ctx, req.NamespacedName, &glance); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("Glance resource not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching Glance: %w", err)
	}

	// Handle deletion via the finalizer: issue Delete on the MariaDB CRs, then
	// release the finalizer once no live (not-yet-deleted) resource remains. The
	// deletion path sets no conditions, so it returns directly without
	// updateStatus.
	if !glance.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &glance)
	}

	// Ensure the finalizer is installed before any sub-reconciler runs so a
	// deletion issued before the next pass still funnels through reconcileDelete.
	// Requeuing after the Update guarantees the next reconcile observes the
	// persisted finalizer rather than the in-memory copy.
	if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &glance, glanceFinalizer); err != nil {
		return ctrl.Result{}, err
	} else if added {
		return ctrl.Result{Requeue: true}, nil
	}

	// Snapshot the persisted status so updateStatus can skip the write when a
	// pass leaves status unchanged (no write → no watch event → no
	// resourceVersion churn). Taken after the finalizer add so an early requeue
	// there does not race a status write.
	statusBefore := glance.Status.DeepCopy()

	// Pipeline locals threaded through the step closures. Each step runs in
	// dependency order; the first to return a non-zero result or an error
	// short-circuits the chain and funnels through updateStatus.
	var (
		// authTokenDigest is the SHA-256 of the service-user password produced by
		// reconcileSecrets; stamped into a pod-template annotation so a rotated
		// password rolls the pods (the password is env-var-consumed).
		authTokenDigest string
		// dsnDigest is the SHA-256 of the assembled DSN produced by
		// reconcileDBConnectionSecret; stamped into a pod-template annotation so a
		// rotated (e.g. Dynamic) database credential rolls the pods.
		dsnDigest string
		// projection is the backends projection produced by reconcileBackends;
		// consumed by reconcileConfig (the rendered config) and
		// reconcileNetworkPolicy (the S3 egress hosts).
		projection backendsProjection
		// art names the config ConfigMap and backends Secret produced by
		// reconcileConfig; consumed by reconcileDatabase and reconcileDeployment.
		art configArtifacts
	)
	pipeline := []commonreconcile.Step{
		{Name: "Secrets", Fn: func(ctx context.Context) (ctrl.Result, error) {
			var (
				res ctrl.Result
				err error
			)
			res, authTokenDigest, err = r.reconcileSecrets(ctx, &glance)
			return res, err
		}},
		// reconcileDBConnectionSecret materialises the DB URL into the derived
		// <glance.Name>-db-connection Secret. It runs after Secrets (upstream
		// credentials must be synced) and before Config; failures set
		// SecretsReady=False, the same condition reconcileSecrets uses.
		{Name: "DBConnectionSecret", Fn: func(ctx context.Context) (ctrl.Result, error) {
			var (
				res ctrl.Result
				err error
			)
			res, dsnDigest, err = r.reconcileDBConnectionSecret(ctx, &glance)
			return res, err
		}},
		// reconcileBackends aggregates the attached, credential-ready backends
		// into the content-hashed backends Secret. Waiting states NEVER
		// short-circuit the pipeline — the step returns a zero result so
		// first-install can proceed, and backend status flips re-enqueue this
		// Glance via the backend watch.
		{Name: "Backends", Fn: func(ctx context.Context) (ctrl.Result, error) {
			var (
				res ctrl.Result
				err error
			)
			res, projection, err = r.reconcileBackends(ctx, &glance)
			return res, err
		}},
		// reconcileConfig renders glance-api.conf into an immutable ConfigMap. On
		// an invalid projection it returns the live Deployment's last-good artefact
		// names instead of re-rendering. It self-marks SecretsReady=False on
		// failure via markConfigFailed, so the wrapper only threads the result.
		{Name: "Config", Fn: func(ctx context.Context) (ctrl.Result, error) {
			var (
				res ctrl.Result
				err error
			)
			res, art, err = r.reconcileConfig(ctx, &glance, projection)
			return res, err
		}},
		// reconcileDatabase provisions/migrates the schema. It waits (requeue)
		// while art.configMapName is empty (no rendered config to db-sync yet).
		{Name: "Database", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDatabase(ctx, &glance, art.configMapName)
		}},
		// reconcileDeployment ensures the API Deployment/Service/PDB and stamps the
		// status endpoint. It re-applies the last-good config on an invalid
		// projection when a Deployment exists, else waits for a ready default
		// backend.
		{Name: "Deployment", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDeployment(ctx, &glance, art, dsnDigest, authTokenDigest)
		}},
		// Once the Deployment/Service/Config outputs are in place, HTTPRoute,
		// HealthCheck, HPA, and NetworkPolicy have no inter-dependency and run
		// concurrently. Each member sets exactly one condition type; the group
		// self-instruments its members, so this step carries no sub_reconciler
		// name.
		{Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileParallelGroup(ctx, &glance, []commonreconcile.ParallelStep[*glancev1alpha1.Glance]{
				{
					Name:          "HTTPRoute",
					ConditionType: conditionTypeHTTPRouteReady,
					Fn: func(ctx context.Context, g *glancev1alpha1.Glance) (ctrl.Result, error) {
						return r.reconcileHTTPRoute(ctx, g)
					},
				},
				{
					Name:          "HealthCheck",
					ConditionType: conditionTypeGlanceAPIReady,
					Fn: func(ctx context.Context, g *glancev1alpha1.Glance) (ctrl.Result, error) {
						return r.reconcileHealthCheck(ctx, g)
					},
				},
				{
					Name:          "HPA",
					ConditionType: "HPAReady",
					Fn: func(ctx context.Context, g *glancev1alpha1.Glance) (ctrl.Result, error) {
						return r.reconcileHPA(ctx, g)
					},
				},
				{
					Name:          "NetworkPolicy",
					ConditionType: conditionTypeNetworkPolicyReady,
					Fn: func(ctx context.Context, g *glancev1alpha1.Glance) (ctrl.Result, error) {
						return r.reconcileNetworkPolicy(ctx, g, projection)
					},
				},
			})
		}},
	}

	result, err := commonreconcile.RunPipeline(ctx, instrumentSubReconciler, pipeline)
	return r.updateStatus(ctx, &glance, statusBefore, result, err)
}

// reconcileDelete drives the finalizer cleanup when the Glance CR is being
// deleted. It is a no-op when the finalizer is absent. Otherwise it issues
// Delete on the MariaDB Database/User/Grant CRs (idempotent, NotFound-tolerant)
// and, while at least one of them was still live (not yet issued a Delete),
// holds the finalizer for one more pass so the schema teardown is triggered
// before the owner-ref chain disappears. Once no live resource remains it drops
// the per-CR metrics, evicts the health-probe cache, and releases the finalizer.
func (r *GlanceReconciler) reconcileDelete(ctx context.Context, glance *glancev1alpha1.Glance) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(glance, glanceFinalizer) {
		return ctrl.Result{}, nil
	}

	key := client.ObjectKey{Name: glance.Name, Namespace: glance.Namespace}

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
		r.Recorder.Event(glance, corev1.EventTypeNormal, "FinalizingDatabase",
			"Cleaning up MariaDB Database, User, and Grant before removing Glance")
		return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
	}

	r.Recorder.Event(glance, corev1.EventTypeNormal, "DatabaseFinalized",
		"MariaDB Database, User, and Grant marked for deletion; releasing finalizer")

	controllerutil.RemoveFinalizer(glance, glanceFinalizer)
	if err := r.Update(ctx, glance); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	glancemetrics.DeleteForGlance(glance.Name, glance.Namespace)
	// Drop the per-CR health-probe cache so a CR recreated under the same
	// name/namespace never serves a stale probe keyed on the deleted CR's UID.
	r.evictHealthProbe(client.ObjectKeyFromObject(glance))
	return ctrl.Result{}, nil
}

// updateStatus persists the current status conditions and returns the given
// result and error, delegating to the shared skeleton: the write is skipped when
// the pass left status semantically unchanged from the statusBefore snapshot, a
// failed write is joined with reconcileErr, and the mutate hook re-aggregates the
// Ready condition on every persist and stamps status.observedGeneration.
func (r *GlanceReconciler) updateStatus(ctx context.Context, glance *glancev1alpha1.Glance, statusBefore *glancev1alpha1.GlanceStatus, result ctrl.Result, reconcileErr error) (ctrl.Result, error) {
	return glanceSkeleton.UpdateStatus(ctx, r.Client, glance, statusBefore, &glance.Status, func() {
		glance.Status.ObservedGeneration = glance.Generation
	}, result, reconcileErr)
}

// setReadyCondition sets the aggregate Ready condition based on all
// sub-conditions, delegating to the shared skeleton with glance's sub-condition
// vocabulary.
func setReadyCondition(glance *glancev1alpha1.Glance) {
	glanceSkeleton.SetReady(glance)
}

// reconcileParallelGroup runs the given sub-reconcilers concurrently, delegating
// to the shared skeleton: each member operates on its own DeepCopy of the Glance
// CR, conditions from every member — including those that succeeded before a
// peer failed — are merged back into the primary glance, and on success the
// shortest non-zero RequeueAfter is returned. Members instrument individually
// via instrumentSubReconciler.
func (r *GlanceReconciler) reconcileParallelGroup(
	ctx context.Context,
	glance *glancev1alpha1.Glance,
	subs []commonreconcile.ParallelStep[*glancev1alpha1.Glance],
) (ctrl.Result, error) {
	return glanceSkeleton.RunParallelGroup(ctx, glance, instrumentSubReconciler, subs)
}

// SetupWithManager registers the GlanceReconciler with the controller manager.
// It probes Gateway API availability, registers BOTH the Glance and
// GlanceBackend field indexes (the single registration site for both
// controllers, so this reconciler MUST be set up before GlanceBackendReconciler)
// and wires the owned resources and cross-resource watches.
func (r *GlanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Detect whether the Gateway API CRD is installed. spec.gateway is optional,
	// so the operator must run on clusters without Gateway API. Adding
	// Owns(HTTPRoute) unconditionally would fail at Start with "no matches for
	// kind HTTPRoute" when the CRD is missing, blocking every Glance CR.
	r.gatewayAPIAvailable = gateway.IsGVKAvailable(mgr.GetRESTMapper(), httpRouteGVK)
	setupLog := ctrl.Log.WithName("glance-setup")
	if r.gatewayAPIAvailable {
		setupLog.Info("Gateway API detected; enabling HTTPRoute watch and reconciliation")
	} else {
		setupLog.Info("Gateway API not installed; HTTPRoute watch disabled, spec.gateway will be rejected via HTTPRouteReady condition")
	}

	// Register the Glance field indexer before Watches so
	// secretToGlanceWithBackendsMapper can rely on it for its MatchingFields
	// lookup.
	if err := registerGlanceIndexes(context.Background(), mgr.GetFieldIndexer()); err != nil {
		return err
	}
	// Register the GlanceBackend indexes here — the single registration site for
	// both controllers (this reconciler is set up before GlanceBackendReconciler
	// in main.go and the envtest helper). reconcileBackends and the mappers rely
	// on them.
	if err := registerGlanceBackendIndexes(context.Background(), mgr.GetFieldIndexer()); err != nil {
		return err
	}

	b := ctrl.NewControllerManagedBy(mgr).
		// Shared controller options: MaxConcurrentReconciles lets independent CRs
		// reconcile in parallel, and the tuned RateLimiter caps per-item failure
		// backoff at 30s (see bootstrap.ControllerOptions).
		WithOptions(bootstrap.ControllerOptions(r.MaxConcurrentReconciles)).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate).
		For(&glancev1alpha1.Glance{}, builder.WithPredicates(watch.CRUpdatePredicate())).
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
		// Watch Secrets and map to the Glance CRs that reference them (directly or
		// via an attached GlanceBackend's S3 credentials/CA Secret). ESO-managed
		// Secrets are owned by the ExternalSecret controller, not the Glance CR, so
		// EnqueueRequestForOwner would never match them.
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(
			secretToGlanceWithBackendsMapper(mgr.GetClient()),
		)).
		// Watch GlanceBackends and map to their parent Glance: backend status flips
		// (CredentialsReady turning True) trigger projection, DeletionTimestamp
		// flips trigger de-projection. No generation predicate — the status
		// transitions ARE the signal.
		Watches(&glancev1alpha1.GlanceBackend{}, handler.EnqueueRequestsFromMapFunc(
			glanceBackendToGlanceMapper(),
		)).
		// Watch the MariaDB cluster CR referenced by spec.database.clusterRef so
		// the operator reflects upstream database outages in DatabaseReady without
		// waiting for the next periodic requeue.
		Watches(&mariadbv1alpha1.MariaDB{}, handler.EnqueueRequestsFromMapFunc(
			mariaDBToGlanceMapper(mgr.GetClient()),
		)).
		// Watch both the cluster-scoped ClusterSecretStore and the namespaced
		// SecretStore a Glance can select via spec.secretStoreRef, so the operator
		// reflects upstream secret-backend outages in SecretsReady as soon as ESO
		// flips the selected store's Ready condition.
		Watches(&esov1.ClusterSecretStore{}, handler.EnqueueRequestsFromMapFunc(
			storeToGlanceMapper(mgr.GetClient(), commonv1.SecretStoreKindCluster),
		)).
		Watches(&esov1.SecretStore{}, handler.EnqueueRequestsFromMapFunc(
			storeToGlanceMapper(mgr.GetClient(), commonv1.SecretStoreKindNamespaced),
		)).
		Complete(r)
}
