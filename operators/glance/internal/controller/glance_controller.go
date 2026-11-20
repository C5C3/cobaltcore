// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the Glance and GlanceBackend reconcilers.
package controller

import (
	"context"
	"fmt"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	"github.com/c5c3/cobaltcore/internal/common/cache"
	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/gateway"
	"github.com/c5c3/cobaltcore/internal/common/healthcheck"
	"github.com/c5c3/cobaltcore/internal/common/lifecycle"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
	glancemetrics "github.com/c5c3/cobaltcore/operators/glance/internal/metrics"
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

const policyConfigMapIndexKey = "spec.policyOverrides.configMapRef.name"

func registerGlanceIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	obj := &glancev1alpha1.Glance{}
	if err := watch.RegisterSecretNameIndex(ctx, indexer, obj, GlanceSecretNameIndexKey, glanceSecretNameExtractor); err != nil {
		return err
	}
	return watch.RegisterParentRefIndex(ctx, indexer, obj, policyConfigMapIndexKey, glancePolicyConfigMapRef)
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

// glanceBackendParentName returns the name of the Glance a GlanceBackend
// attaches to (spec.glanceRef.name), or "" for an object of another type. An
// unattached backend carries no reference and so returns "" as well.
func glanceBackendParentName(o client.Object) string {
	b, ok := o.(*glancev1alpha1.GlanceBackend)
	if !ok {
		return ""
	}
	return b.Spec.GlanceRef.Name
}

// glanceBackendGlanceRefExtractor is the IndexerFunc for
// GlanceBackendGlanceRefIndexKey: it maps a backend to its spec.glanceRef.name
// so an attached-backends list is an O(1) indexed lookup. Production registers
// it through watch.RegisterParentRefIndex; this var exists so the fake clients
// in tests build the identical extractor.
var glanceBackendGlanceRefExtractor = watch.ParentRefIndexer(glanceBackendParentName)

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
	if err := watch.RegisterParentRefIndex(ctx, indexer, &glancev1alpha1.GlanceBackend{},
		GlanceBackendGlanceRefIndexKey, glanceBackendParentName); err != nil {
		return err
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
// NetworkPolicy, DBPurge) always sets its condition — configured-ready,
// NotRequired, or waiting — so a gateway-less or autoscaling-less cluster still
// resolves the aggregate (the NotRequired paths report True), exactly as
// keystone/horizon aggregate their optional conditions.
var subConditionTypes = []string{
	"SecretsReady",
	"BackendsReady",
	"DatabaseReady",
	"DeploymentReady",
	conditionTypeGlanceAPIReady,
	"HPAReady",
	"VPAReady",
	conditionTypeNetworkPolicyReady,
	conditionTypeHTTPRouteReady,
	conditionTypeDBPurgeReady,
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

	// NamespaceScoped is true when the operator runs with --namespace. A Role
	// cannot grant the cluster-scoped ClusterSecretStore, so its watch leg is
	// then not registered and reconcileSecrets refuses a CR whose effective
	// store is cluster-scoped with ClusterSecretStoreUnsupported instead of
	// reading it. The zero value is the cluster-wide operator.
	NamespaceScoped bool

	// MaxConcurrentReconciles bounds how many Glance CRs reconcile concurrently.
	// It is threaded from the --max-concurrent-reconciles flag and applied to
	// the controller's controller.Options in SetupWithManager. A value <= 0 falls
	// back to bootstrap.DefaultMaxConcurrentReconciles inside
	// bootstrap.ControllerOptions, so the zero value is safe.
	MaxConcurrentReconciles int

	// HTTPClient is the health-check client seam. Production leaves it nil so the
	// health check uses http.DefaultClient; tests inject a stub transport.
	HTTPClient HTTPDoer

	// Resolver resolves the target cluster a Glance CR names in
	// spec.targetClusterRef into the client its children are read and written
	// with. Nil means always-local: every CR keeps its children on the
	// management cluster, which is what single-cluster tests and deployments
	// want.
	Resolver commonmulticluster.ClusterResolver

	// apiReader is set during SetupWithManager from mgr.GetAPIReader(): a direct,
	// uncached reader. reconcileDeployment latches the two-phase narrowing of the
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
	// reconcileHTTPRoute takes that verdict and surfaces a clear
	// HTTPRouteReady=False condition if the user nonetheless sets spec.gateway.
	gatewayAPIAvailable bool

	// vpaAvailable is set during SetupWithManager from the management
	// cluster's RESTMapper and reports whether it serves the
	// autoscaling.k8s.io/v1 VerticalPodAutoscaler kind. It gates the local VPA
	// watch leg and answers commonmulticluster.ChildrenServeKind for local
	// children, so reconcileVPA reports VPAReady=False/VPANotInstalled for an
	// opt-in on a cluster without the VPA.
	vpaAvailable bool

	// memcachedAvailable is set during SetupWithManager from the management
	// cluster's RESTMapper and reports whether it serves the
	// memcached.c5c3.io/v1beta1 Memcached kind. It gates the Memcached watch,
	// which re-renders glance-api.conf when status.serverList changes. Without
	// the kind no leg is registered, and a managed cache renders the Service
	// name until a Memcached publishes a list.
	memcachedAvailable bool

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

var glanceSkeleton = commonreconcile.Skeleton[*glancev1alpha1.Glance, glancev1alpha1.GlanceStatus]{
	RequireCurrentGeneration: true,
	SubConditionTypes:        subConditionTypes,
	Conditions:               func(g *glancev1alpha1.Glance) *[]metav1.Condition { return &g.Status.Conditions },
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

// GlanceRemoteChildKinds are the kinds a Glance CR projects into the namespace
// of the target cluster it names, and the kinds reconcileDeleteRemoteChildren
// sweeps by ownership label when that CR is deleted. Nothing on the target
// cluster collects them, so a kind missing from this list is a kind that keeps
// running after its CR is gone.
//
// The list is cross-checked against the create verbs of the kubebuilder RBAC
// markers on this controller below: the operator can only leave behind what it
// is allowed to create.
var GlanceRemoteChildKinds = []schema.GroupVersionKind{
	appsv1.SchemeGroupVersion.WithKind("Deployment"),
	corev1.SchemeGroupVersion.WithKind("Service"),
	corev1.SchemeGroupVersion.WithKind("ConfigMap"),
	corev1.SchemeGroupVersion.WithKind("Secret"),
	batchv1.SchemeGroupVersion.WithKind("Job"),
	batchv1.SchemeGroupVersion.WithKind("CronJob"),
	policyv1.SchemeGroupVersion.WithKind("PodDisruptionBudget"),
	autoscalingv2.SchemeGroupVersion.WithKind("HorizontalPodAutoscaler"),
	deployment.VPAGVK,
	networkingv1.SchemeGroupVersion.WithKind("NetworkPolicy"),
	httpRouteGVK,
	mariadbv1alpha1.GroupVersion.WithKind("Database"),
	mariadbv1alpha1.GroupVersion.WithKind("User"),
	mariadbv1alpha1.GroupVersion.WithKind("Grant"),
}

// +kubebuilder:rbac:groups=glance.openstack.c5c3.io,resources=glances,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=glance.openstack.c5c3.io,resources=glances/status,verbs=get;update
// Unexercised by the e2e suites; used by operators/glance/internal/controller/reconcile_deployment.go:166 (reconcileDeployment) to update glances/finalizers.
// +kubebuilder:rbac:groups=glance.openstack.c5c3.io,resources=glances/finalizers,verbs=update
// Users create backend CRs; the operator only lists and watches them to
// assemble the aggregate Glance store configuration.
// +kubebuilder:rbac:groups=glance.openstack.c5c3.io,resources=glancebackends,verbs=get;list;watch
// +kubebuilder:rbac:groups=glance.openstack.c5c3.io,resources=glancebackends/status,verbs=get;update
// Unexercised by the e2e suites; used by operators/glance/internal/controller/reconcile_deployment.go:166 (reconcileDeployment) to delete deployments.
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;patch;delete
// Unexercised by the e2e suites; used by operators/glance/internal/controller/reconcile_deployment.go:210 (reconcileDeployment) to delete services.
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;patch;delete
// Unexercised by the e2e suites; used by operators/glance/internal/controller/reconcile_config.go:170 (pruneConfigHistory) to delete configmaps.
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;delete
// Unexercised by the e2e suites; used by operators/glance/internal/controller/reconcile_config.go:176 (pruneConfigHistory) to delete secrets.
// Unexercised by the e2e suites; used by operators/glance/internal/controller/reconcile_dbconnection_secret.go:49 (reconcileDBConnectionSecret) to update secrets.
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// Unexercised by the e2e suites; used by operators/glance/internal/controller/reconcile_dbpurge.go:154 (reconcileDBPurge) to delete cronjobs.
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=databases;users;grants,verbs=get;list;watch;create;patch;delete
// Required for the operator to observe the referenced MariaDB cluster's
// Ready condition and reflect outages in DatabaseReady.
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=mariadbs,verbs=get;list;watch
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=maxscales,verbs=get;list;watch
// Required to read status.serverList of the Memcached spec.cache.clusterRef
// names (an uncached Get) and to re-render glance-api.conf when the list changes.
// +kubebuilder:rbac:groups=memcached.c5c3.io,resources=memcacheds,verbs=get;list;watch
// The database and service-user credentials Secrets are ESO-managed; the
// operator only reads the ExternalSecrets to attribute a not-synced Secret in
// SecretsReady messages.
// Unexercised by the e2e suites; used by internal/common/secrets/secrets.go:68 (WaitForExternalSecret) to list and watch externalsecrets.
// +kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets,verbs=get;list;watch
// Required so the operator can observe the selected store's Ready condition
// and reflect upstream secret-backend (OpenBao) outages in SecretsReady. A
// Glance selects either the shared cluster-scoped ClusterSecretStore
// (default) or a namespaced SecretStore via spec.secretStoreRef, so both kinds
// must be watchable.
// +kubebuilder:rbac:groups=external-secrets.io,resources=clustersecretstores;secretstores,verbs=get;list;watch
// Unexercised by the e2e suites; used by operators/glance/internal/controller/reconcile_deployment.go:215 (reconcileDeployment) to delete poddisruptionbudgets.
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;patch;delete
// Unexercised by the e2e suites; used by operators/glance/internal/controller/reconcile_hpa.go:26 (reconcileHPA) to create and patch horizontalpodautoscalers.
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;patch;delete
// Unexercised by the e2e suites; used by internal/common/deployment/vpa_flow.go:190 (ReconcileVPAs) to create and patch verticalpodautoscalers.
// Unexercised by the e2e suites; used by internal/common/deployment/vpa_flow.go:213 (ReconcileVPAs) to delete verticalpodautoscalers.
// Unexercised by the e2e suites; used by operators/glance/internal/controller/glance_controller.go:716 (SetupWithManager) to list and watch verticalpodautoscalers.
// +kubebuilder:rbac:groups=autoscaling.k8s.io,resources=verticalpodautoscalers,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;patch;delete
// Required to create, patch and delete HTTPRoutes that expose the Glance API externally.
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;patch;delete
// Required so the operator can observe the Accepted condition set by the
// upstream Gateway controller and reflect it in HTTPRouteReady.
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=get
// Required for the webhook to validate that spec.priorityClassName references
// an existing PriorityClass at admission time.
// Unexercised by the e2e suites; used by internal/common/validation/validation.go:312 (PriorityClassExists) to get priorityclasses.
// +kubebuilder:rbac:groups=scheduling.k8s.io,resources=priorityclasses,verbs=get

// Reconcile runs the shared service lifecycle around the Glance pipeline.
// r.Client reads Glance and GlanceBackend objects on the management cluster.
// The hooks receive the target cluster's client for workloads, or r.Client for local workloads.
func (r *GlanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	driver := lifecycle.Service[*glancev1alpha1.Glance, glancev1alpha1.GlanceStatus]{
		Client:          r.Client,
		Resolver:        r.Resolver,
		Recorder:        r.Recorder,
		Skeleton:        glanceSkeleton,
		Finalizer:       glanceFinalizer,
		TargetCondition: "SecretsReady",
		Fields: func(g *glancev1alpha1.Glance) lifecycle.Fields[glancev1alpha1.GlanceStatus] {
			return lifecycle.Fields[glancev1alpha1.GlanceStatus]{
				Target:             g.Spec.TargetClusterRef,
				Suspended:          g.Spec.Suspend,
				Status:             &g.Status,
				CopyStatus:         g.Status.DeepCopy,
				ObservedGeneration: &g.Status.ObservedGeneration,
			}
		},
		Sync:     r.reconcileService,
		Finalize: r.reconcileDelete,
		Sweep:    r.reconcileDeleteRemoteChildren,
	}
	return driver.Reconcile(ctx, req, &glancev1alpha1.Glance{})
}

func (r *GlanceReconciler) reconcileService(ctx context.Context, children client.Client, glance *glancev1alpha1.Glance) (ctrl.Result, error) {
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
		{Name: "Secrets", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, authTokenDigest, err = r.reconcileSecrets(ctx, children, glance)
			return res, err
		}},
		// reconcileDBConnectionSecret materialises the DB URL into the derived
		// <glance.Name>-db-connection Secret. It runs after Secrets (upstream
		// credentials must be synced) and before Config; failures set
		// SecretsReady=False, the same condition reconcileSecrets uses.
		{Name: "DBConnectionSecret", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, dsnDigest, err = r.reconcileDBConnectionSecret(ctx, children, glance)
			return res, err
		}},
		// reconcileBackends aggregates the attached, credential-ready backends
		// into the content-hashed backends Secret. Waiting states NEVER
		// short-circuit the pipeline — the step returns a zero result so
		// first-install can proceed, and backend status flips re-enqueue this
		// Glance via the backend watch.
		{Name: "Backends", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, projection, err = r.reconcileBackends(ctx, children, glance)
			return res, err
		}},
		// reconcileConfig renders glance-api.conf into an immutable ConfigMap. On
		// an invalid projection it returns the live Deployment's last-good artefact
		// names instead of re-rendering. It self-marks SecretsReady=False on
		// failure via markConfigFailed, so the wrapper only threads the result.
		{Name: "Config", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, art, err = r.reconcileConfig(ctx, children, glance, projection)
			return res, err
		}},
		// reconcileDatabase provisions/migrates the schema. It waits (requeue)
		// while art.configMapName is empty (no rendered config to db-sync yet).
		{Name: "Database", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDatabase(ctx, children, glance, art.configMapName)
		}},
		// reconcileDeployment ensures the API Deployment/Service/PDB and stamps the
		// status endpoint. It re-applies the last-good config on an invalid
		// projection when a Deployment exists, else waits for a ready default
		// backend.
		{Name: "Deployment", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDeployment(ctx, children, glance, art, dsnDigest, authTokenDigest)
		}},
		// Once the Deployment/Service/Config outputs are in place, HTTPRoute,
		// HealthCheck, HPA, NetworkPolicy, and DBPurge have no inter-dependency and
		// run concurrently. Each member sets exactly one condition type; the group
		// self-instruments its members, so this step carries no sub_reconciler
		// name.
		{Fn: func(ctx context.Context) (ctrl.Result, error) {
			return glanceSkeleton.RunParallelGroup(ctx, glance, instrumenter.Instrument, []commonreconcile.ParallelStep[*glancev1alpha1.Glance]{
				{
					Name:          "HTTPRoute",
					ConditionType: conditionTypeHTTPRouteReady,
					Fn: func(ctx context.Context, g *glancev1alpha1.Glance) (ctrl.Result, error) {
						return r.reconcileHTTPRoute(ctx, children, g)
					},
				},
				{
					Name:          "HealthCheck",
					ConditionType: conditionTypeGlanceAPIReady,
					Fn:            r.reconcileHealthCheck,
				},
				{
					Name:          "HPA",
					ConditionType: "HPAReady",
					Fn: func(ctx context.Context, g *glancev1alpha1.Glance) (ctrl.Result, error) {
						return r.reconcileHPA(ctx, children, g)
					},
				},
				{
					Name:          "VPA",
					ConditionType: "VPAReady",
					Fn: func(ctx context.Context, g *glancev1alpha1.Glance) (ctrl.Result, error) {
						return r.reconcileVPA(ctx, children, g)
					},
				},
				{
					Name:          "NetworkPolicy",
					ConditionType: conditionTypeNetworkPolicyReady,
					Fn: func(ctx context.Context, g *glancev1alpha1.Glance) (ctrl.Result, error) {
						return r.reconcileNetworkPolicy(ctx, children, g, projection)
					},
				},
				// The purge CronJob mounts the rendered config, so it belongs after
				// the Database step — which the group already runs behind, leaving
				// art.configMapName non-empty here.
				{
					Name:          "DBPurge",
					ConditionType: conditionTypeDBPurgeReady,
					Fn: func(ctx context.Context, g *glancev1alpha1.Glance) (ctrl.Result, error) {
						return r.reconcileDBPurge(ctx, children, g, art.configMapName)
					},
				},
			})
		}},
	}

	result, err := commonreconcile.RunPipeline(ctx, instrumenter.Instrument, pipeline)
	// Prune only after the Deployment and purge CronJob use the new revisions.
	if err == nil && result.IsZero() && projection.valid {
		if err = r.pruneConfigHistory(ctx, children, glance, art); err != nil {
			markConfigFailed(glance, err)
		}
	}
	result = healthcheck.ScheduleReprobe(result, err, &r.healthProbeCache,
		client.ObjectKeyFromObject(glance))
	return result, err
}

// reconcileDelete drives the finalizer cleanup when the Glance CR is being
// deleted. It is a no-op when the finalizer is absent. In managed mode
// (spec.database.clusterRef is set) it requests deletion of the MariaDB
// Database, User, and Grant this Glance owns, and holds the finalizer for one
// more pass whenever it requested one, so the schema teardown starts before the
// owner-ref chain disappears. A brownfield CR skips that step. It then drops
// the per-CR metrics, evicts the health-probe cache, and releases the finalizer.
//
// A nil children client means the target cluster this CR named is no longer
// registered. Its MariaDB CRs cannot be reached, so they stay behind on a
// cluster that has not resolved for the whole abandon window, and the finalizer
// is released anyway: holding it would only strand the CR in Terminating.
func (r *GlanceReconciler) reconcileDelete(ctx context.Context, children client.Client, glance *glancev1alpha1.Glance) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(glance, glanceFinalizer) {
		return ctrl.Result{}, nil
	}

	if children == nil {
		r.Recorder.Event(glance, corev1.EventTypeWarning, "RemoteChildrenAbandoned",
			"Target cluster is no longer registered; releasing the Glance finalizer")
	} else if glance.Spec.Database.ClusterRef != nil {
		requested, err := database.FinalizeOwnedResources(ctx, children, r.Scheme, glance)
		if err != nil {
			return ctrl.Result{}, err
		}
		if requested {
			r.Recorder.Event(glance, corev1.EventTypeNormal, "FinalizingDatabase",
				"Cleaning up MariaDB Database, User, and Grant before removing Glance")
			return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
		}

		r.Recorder.Event(glance, corev1.EventTypeNormal, "DatabaseFinalized",
			"Owned MariaDB resources are absent or terminating; releasing finalizer")
	}

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

// reconcileDeleteRemoteChildren deletes everything this Glance projected onto
// the target cluster it names and releases the remote-children finalizer, as
// commonmulticluster.SweepRemoteChildren documents. reconcileDelete above
// deletes the three MariaDB CRs it tracks by name; this pass is what reaches the
// rest, selected on the ownership labels Claim stamped on them.
func (r *GlanceReconciler) reconcileDeleteRemoteChildren(ctx context.Context, children client.Client, glance *glancev1alpha1.Glance) error {
	return commonmulticluster.SweepRemoteChildren(ctx, r.Client, r.Resolver, r.Recorder, r.Scheme,
		glance, glance.Spec.TargetClusterRef, children, GlanceRemoteChildKinds)
}

// SetupWithManager registers the GlanceReconciler with the controller manager.
// It probes Gateway API availability, registers BOTH the Glance and
// GlanceBackend field indexes (the single registration site for both
// controllers, so this reconciler MUST be set up before GlanceBackendReconciler)
// and wires the owned resources and cross-resource watches.
func (r *GlanceReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	local := mgr.GetLocalManager()

	// Detect whether the Gateway API CRD is installed. spec.gateway is optional,
	// so the operator must run on clusters without Gateway API. Adding
	// Owns(HTTPRoute) unconditionally would fail at Start with "no matches for
	// kind HTTPRoute" when the CRD is missing, blocking every Glance CR.
	var err error
	r.gatewayAPIAvailable, err = gateway.IsGVKAvailable(local.GetRESTMapper(), httpRouteGVK)
	if err != nil {
		return err
	}
	r.apiReader = local.GetAPIReader()
	setupLog := ctrl.Log.WithName("glance-setup")
	if r.gatewayAPIAvailable {
		setupLog.Info("Gateway API detected; enabling HTTPRoute watch and reconciliation")
	} else {
		setupLog.Info("Gateway API not installed; HTTPRoute watch disabled, spec.gateway will be rejected via HTTPRouteReady condition")
	}

	// Detect the VerticalPodAutoscaler CRD: verticalAutoscaling is an opt-in,
	// so the operator must run on clusters without the VPA, where an
	// unconditional Owns would fail the controller at Start.
	r.vpaAvailable, err = gateway.IsGVKAvailable(local.GetRESTMapper(), deployment.VPAGVK)
	if err != nil {
		return err
	}
	if r.vpaAvailable {
		setupLog.Info("VerticalPodAutoscaler CRD detected; enabling VPA watch and reconciliation")
	} else {
		setupLog.Info("VerticalPodAutoscaler CRD not installed; VPA watch disabled, verticalAutoscaling will be reported through VPAReady")
	}

	// Detect the Memcached CRD: memcached-operator is optional, so the
	// operator must run on clusters without it, where an unconditional watch
	// would fail the controller at Start.
	r.memcachedAvailable, err = gateway.IsGVKAvailable(local.GetRESTMapper(), cache.MemcachedGVK)
	if err != nil {
		return err
	}
	if r.memcachedAvailable {
		setupLog.Info("Memcached CRD detected; enabling the Memcached watch")
	} else {
		setupLog.Info("Memcached CRD not installed; Memcached watch disabled, managed caches render the Service name")
	}

	// Register the Glance field indexer before Watches so
	// secretToGlanceWithBackendsMapper can rely on it for its MatchingFields
	// lookup. The indexes go on the LOCAL field indexer, not mgr's: with a
	// provider configured, the multicluster manager's field indexer registers
	// against the provider clusters, which hold no Glance CR. Registration
	// stays local by contract: the indexes are on CR kinds, which exist on the
	// management cluster alone, and every request the watches emit is pinned to
	// that cluster (LocalRequests / RemoteRequests,
	// internal/common/multicluster/watch.go), so a remote event resolves to its
	// CR through the local cache. Registering on the fleet would fail the
	// engagement of every target cluster, because the kubeconfig provider
	// applies its stored indexes while engaging one.
	if err := registerGlanceIndexes(context.Background(), local.GetFieldIndexer()); err != nil {
		return err
	}
	// Register the GlanceBackend indexes here — the single registration site for
	// both controllers (this reconciler is set up before GlanceBackendReconciler
	// in main.go and the envtest helper). reconcileBackends and the mappers rely
	// on them. They go on the LOCAL field indexer for the same reason as the
	// Glance indexes above.
	if err := registerGlanceBackendIndexes(context.Background(), local.GetFieldIndexer()); err != nil {
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
		func(glance *glancev1alpha1.Glance) *commonv1.TargetClusterRefSpec {
			return glance.Spec.TargetClusterRef
		})

	options := bootstrap.TypedControllerOptions[mcreconcile.Request](r.MaxConcurrentReconciles)
	// multicluster-runtime does not inherit name validation from the host manager.
	options.SkipNameValidation = local.GetControllerOptions().SkipNameValidation
	b := mcbuilder.ControllerManagedBy(mgr).
		// Shared controller options: MaxConcurrentReconciles lets independent CRs
		// reconcile in parallel, and the tuned RateLimiter caps per-item failure
		// backoff at 30s (see bootstrap.TypedControllerOptions).
		WithOptions(options).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate).
		For(&glancev1alpha1.Glance{}, mcbuilder.WithPredicates(watch.CRUpdatePredicate()), engageLocal, engageNoProviders).
		Owns(&appsv1.Deployment{}, engageLocal, engageNoProviders).
		Owns(&corev1.Service{}, engageLocal, engageNoProviders).
		Owns(&corev1.ConfigMap{}, engageLocal, engageNoProviders).
		Owns(&corev1.Secret{}, engageLocal, engageNoProviders).
		Owns(&policyv1.PodDisruptionBudget{}, engageLocal, engageNoProviders).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}, engageLocal, engageNoProviders).
		Owns(&networkingv1.NetworkPolicy{}, engageLocal, engageNoProviders).
		Owns(&batchv1.Job{}, engageLocal, engageNoProviders).
		Owns(&batchv1.CronJob{}, engageLocal, engageNoProviders)

	if r.gatewayAPIAvailable {
		b = b.Owns(&gatewayv1.HTTPRoute{}, engageLocal, engageNoProviders)
	}

	// The recommender rewrites status.recommendation about once a minute; only
	// a spec change (drift from the applied VPA) wakes the CR.
	if r.vpaAvailable {
		b = b.Owns(&vpav1.VerticalPodAutoscaler{}, mcbuilder.WithPredicates(predicate.GenerationChangedPredicate{}),
			engageLocal, engageNoProviders)
	}

	// The same children, once more, on the clusters a CR can project onto. Owns
	// cannot see them: an owner reference does not cross a cluster boundary, so
	// the ownership labels are what maps a child back to its CR. The VPA leg
	// carries the same predicate as its local Owns; no other leg carries one,
	// mirroring what Owns admits locally.
	b, err = commonmulticluster.AddRemoteChildWatches(b, local.GetScheme(), &glancev1alpha1.Glance{},
		targets, GlanceRemoteChildKinds, map[schema.GroupVersionKind][]mcbuilder.WatchesOption{
			deployment.VPAGVK: {mcbuilder.WithPredicates(predicate.GenerationChangedPredicate{})},
		})
	if err != nil {
		return err
	}

	// Watch Secrets and map to the Glance CRs that reference them (directly or
	// via an attached GlanceBackend's S3 credentials/CA Secret). ESO-managed
	// Secrets are owned by the ExternalSecret controller, not the Glance CR, so
	// EnqueueRequestForOwner would never match them.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &corev1.Secret{},
		secretToGlanceWithBackendsMapper(local.GetClient()))
	if err != nil {
		return err
	}

	// A referenced policy ConfigMap is owned by the user and does not match Owns.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &corev1.ConfigMap{},
		policyConfigMapToGlanceMapper(local.GetClient()))
	if err != nil {
		return err
	}

	// Watch the MariaDB cluster CR referenced by spec.database.clusterRef so
	// the operator reflects upstream database outages in DatabaseReady without
	// waiting for the next periodic requeue.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &mariadbv1alpha1.MariaDB{},
		mariaDBToGlanceMapper(local.GetClient()))
	if err != nil {
		return err
	}

	// Watch the MaxScale a MariaDB names in spec.maxScaleRef: its services
	// decide whether the database connection routes through it
	// (database.ResolveEndpoint). The decision reads the spec only, and
	// MaxScale rewrites its status every 30 s, so only a generation change
	// reconciles.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &mariadbv1alpha1.MaxScale{},
		maxScaleToGlanceMapper(local.GetClient()), mcbuilder.WithPredicates(predicate.GenerationChangedPredicate{}))
	if err != nil {
		return err
	}

	// Watch the Memcached referenced by spec.cache.clusterRef so a changed
	// status.serverList re-renders glance-api.conf at watch latency. No
	// predicate: the list lives in status, which a generation predicate drops.
	if r.memcachedAvailable {
		b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, cache.NewMemcached(),
			memcachedToGlanceMapper(local.GetClient()))
		if err != nil {
			return err
		}
	}

	// Watch both the cluster-scoped ClusterSecretStore and the namespaced
	// SecretStore a Glance can select via spec.secretStoreRef, so the operator
	// reflects upstream secret-backend outages in SecretsReady as soon as ESO
	// flips the selected store's Ready condition.
	// Under --namespace the ClusterSecretStore leg is not registered: a Role
	// cannot grant the cluster-scoped kind, so its informer would never sync
	// and the manager would fail its cache sync at startup.
	b, err = secrets.AddStoreWatches(b, local.GetScheme(), targets, r.NamespaceScoped,
		local.GetClient(), storeToGlanceMapper)
	if err != nil {
		return err
	}

	return b.
		// Watch GlanceBackends and map to their parent Glance: backend status flips
		// (CredentialsReady turning True) trigger projection, DeletionTimestamp
		// flips trigger de-projection. No generation predicate — the status
		// transitions ARE the signal. The leg stays local-only: a GlanceBackend is
		// a management-plane CR and lives nowhere else.
		Watches(&glancev1alpha1.GlanceBackend{}, commonmulticluster.LocalRequests(
			glanceBackendToGlanceMapper(),
		), engageLocal, engageNoProviders).
		// The default wrapper turns an error matching multicluster.ErrClusterNotFound
		// into a successful reconcile. This operator instead surfaces an
		// unresolvable cluster as a TargetClusterUnavailable condition and
		// requeues, so the wrapper stays off and the error semantics remain
		// byte-identical to the classic builder's.
		WithClusterNotFoundWrapper(false).
		Complete(commonmulticluster.LocalReconciler(r))
}
