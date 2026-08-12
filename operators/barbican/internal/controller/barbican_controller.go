// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the Barbican and BarbicanSecretStore
// reconcilers.
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
	commonmulticluster "github.com/c5c3/forge/internal/common/multicluster"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	"github.com/c5c3/forge/internal/common/watch"
	barbicanv1alpha1 "github.com/c5c3/forge/operators/barbican/api/v1alpha1"
	barbicanmetrics "github.com/c5c3/forge/operators/barbican/internal/metrics"
)

// BarbicanSecretNameIndexKey is the field-indexer key under which Barbican CRs
// are indexed by the union of their referenced Secret names
// (spec.serviceUser.secretRef.name and spec.database.secretRef.name). Used by
// SetupWithManager to register the indexer and by the Secret watch mapper to
// perform an O(1) reverse lookup instead of an unfiltered List of all Barbican
// CRs in the namespace.
// #nosec G101 -- field-indexer key (a JSONPath-like field selector), not a credential.
const BarbicanSecretNameIndexKey = "spec.secretRefs.name"

// BarbicanSecretStoreSecretNameIndexKey is the field-indexer key under which
// BarbicanSecretStore CRs are indexed by the Secret names a brownfield store
// references (spec.openBao.server.credentialsSecretRef.name and
// spec.openBao.server.caBundleSecretRef.name). A managed store indexes under
// nothing: it references no Secret by name, since the operator derives both the
// credentials and the trust bundle from the instance name.
// #nosec G101 -- field-indexer key (a JSONPath-like field selector), not a credential.
const BarbicanSecretStoreSecretNameIndexKey = "spec.openBao.server.secretRefs.name"

// barbicanSecretNameExtractor is the controller-runtime IndexerFunc registered
// under BarbicanSecretNameIndexKey. It returns the deduplicated, non-empty union
// of Secret names referenced by a Barbican CR — spec.serviceUser.secretRef.name
// and spec.database.secretRef.name — so the field indexer can resolve a Secret
// event to the referencing CR(s) without listing every Barbican in the
// namespace.
func barbicanSecretNameExtractor(obj client.Object) []string {
	barbican, ok := obj.(*barbicanv1alpha1.Barbican)
	if !ok {
		// controller-runtime should never call us with the wrong type; a nil
		// return is safer than a panic if it ever does.
		return nil
	}
	serviceUser := barbican.Spec.ServiceUser.SecretRef.Name
	dbName := barbican.Spec.Database.SecretRef.Name

	names := make([]string, 0, 2)
	if serviceUser != "" {
		names = append(names, serviceUser)
	}
	if dbName != "" && dbName != serviceUser {
		names = append(names, dbName)
	}
	return names
}

// barbicanSecretStoreSecretNameExtractor is the IndexerFunc registered under
// BarbicanSecretStoreSecretNameIndexKey. It returns the deduplicated, non-empty
// union of the AppRole credentials and CA bundle Secret names a brownfield store
// references, so a rotated credential re-renders the owning Barbican's config
// through the store's parent.
func barbicanSecretStoreSecretNameExtractor(obj client.Object) []string {
	store, ok := obj.(*barbicanv1alpha1.BarbicanSecretStore)
	if !ok || store.Spec.OpenBao == nil || store.Spec.OpenBao.Server == nil {
		return nil
	}
	server := store.Spec.OpenBao.Server
	creds := server.CredentialsSecretRef.Name

	names := make([]string, 0, 2)
	if creds != "" {
		names = append(names, creds)
	}
	if server.CABundleSecretRef != nil && server.CABundleSecretRef.Name != "" && server.CABundleSecretRef.Name != creds {
		names = append(names, server.CABundleSecretRef.Name)
	}
	return names
}

// registerBarbicanIndexes registers the Barbican field indexer under
// BarbicanSecretNameIndexKey with the given FieldIndexer.
func registerBarbicanIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	return watch.RegisterSecretNameIndex(ctx, indexer, &barbicanv1alpha1.Barbican{}, BarbicanSecretNameIndexKey, barbicanSecretNameExtractor)
}

// registerBarbicanSecretStoreIndexes registers the three BarbicanSecretStore
// field indexers. It lives beside registerBarbicanIndexes so index registration
// has a single site: BarbicanReconciler.SetupWithManager runs before
// BarbicanSecretStoreReconciler.SetupWithManager, so both controllers can rely
// on the indexes. The returned error is wrapped with the index key so the
// registration site is identifiable in manager-startup failure logs.
func registerBarbicanSecretStoreIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &barbicanv1alpha1.BarbicanSecretStore{}, BarbicanSecretStoreBarbicanRefIndexKey,
		barbicanSecretStoreBarbicanRefExtractor); err != nil {
		return fmt.Errorf("registering field indexer %q: %w", BarbicanSecretStoreBarbicanRefIndexKey, err)
	}
	if err := indexer.IndexField(ctx, &barbicanv1alpha1.BarbicanSecretStore{}, BarbicanSecretStoreInstanceRefIndexKey,
		barbicanSecretStoreInstanceRefExtractor); err != nil {
		return fmt.Errorf("registering field indexer %q: %w", BarbicanSecretStoreInstanceRefIndexKey, err)
	}
	if err := indexer.IndexField(ctx, &barbicanv1alpha1.BarbicanSecretStore{}, BarbicanSecretStoreSecretNameIndexKey,
		barbicanSecretStoreSecretNameExtractor); err != nil {
		return fmt.Errorf("registering field indexer %q: %w", BarbicanSecretStoreSecretNameIndexKey, err)
	}
	return nil
}

// subConditionTypes lists the condition types set by the individual Barbican
// sub-reconcilers. The aggregate Ready condition is True only when all of these
// are True. Every parallel-group member (HTTPRoute, HealthCheck, HPA,
// NetworkPolicy) always sets its condition — configured-ready, NotRequired, or
// waiting — so a gateway-less or autoscaling-less cluster still resolves the
// aggregate (the NotRequired paths report True), exactly as the sibling
// operators aggregate their optional conditions.
//
// A condition is spelled out only where the owning sub-reconciler declares no
// constant for it; the rest reference that constant, so a rename cannot leave a
// stale literal behind here.
var subConditionTypes = []string{
	"SecretsReady",
	"DatabaseReady",
	conditionTypeSecretStoresReady,
	"DeploymentReady",
	conditionTypeBarbicanAPIReady,
	"HPAReady",
	conditionTypeNetworkPolicyReady,
	conditionTypeHTTPRouteReady,
	conditionTypeDBCleanReady,
}

// barbicanFinalizer blocks removal of a Barbican CR from etcd until the MariaDB
// Database, User, and Grant CRs it owns have been issued a Delete, so the schema
// teardown is triggered before the owner-ref chain disappears. It is the single
// source of truth for Reconcile, the finalizer handler, and tests.
const barbicanFinalizer = "barbican.openstack.c5c3.io/finalizer"

// httpRouteGVK identifies the HTTPRoute kind the operator watches when Gateway
// API is installed. Availability is probed at setup time via the shared
// gateway.IsGVKAvailable RESTMapper probe.
var httpRouteGVK = schema.GroupVersionKind{
	Group:   gatewayv1.GroupVersion.Group,
	Version: gatewayv1.GroupVersion.Version,
	Kind:    "HTTPRoute",
}

// barbicanSkeleton bundles the shared controller-skeleton glue (Ready
// aggregation, no-op-skipping status writes, config-failure marking) with
// barbican's sub-condition vocabulary and status accessor. The wrapper helpers
// below delegate to it.
var barbicanSkeleton = commonreconcile.Skeleton[*barbicanv1alpha1.Barbican, barbicanv1alpha1.BarbicanStatus]{
	SubConditionTypes: subConditionTypes,
	Conditions:        func(b *barbicanv1alpha1.Barbican) *[]metav1.Condition { return &b.Status.Conditions },
}

// conditionReasonConfigError is the SecretsReady=False reason set when
// reconcileConfig fails. Config artefacts (the rendered barbican.conf Secret)
// gate the same downstream graph as the upstream credential Secrets, so failures
// reuse SecretsReady rather than a dedicated condition — matching
// reconcileDBConnectionSecret's Config→SecretsReady mapping.
const conditionReasonConfigError = "ConfigError"

// markConfigFailed flips SecretsReady to False so a reconcileConfig failure
// cannot leave the aggregate Ready condition stale-True at the new
// ObservedGeneration. It mirrors the sibling operators' markConfigFailed helper.
func markConfigFailed(barbican *barbicanv1alpha1.Barbican, err error) {
	barbicanSkeleton.MarkFailed(barbican, "SecretsReady", conditionReasonConfigError, err)
}

// BarbicanReconciler reconciles a Barbican object. Its fields mirror the sibling
// service reconcilers' core set.
type BarbicanReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// OperatorNamespace is the Namespace the operator Pod runs in (resolved at
	// startup by bootstrap.DetectOperatorNamespace). The networkpolicy step
	// appends an ingress peer for this Namespace so the operator's own health
	// check can reach the Barbican API. Empty when the namespace could not be
	// determined, in which case no operator-namespace peer is added.
	OperatorNamespace string

	// MaxConcurrentReconciles bounds how many Barbican CRs reconcile
	// concurrently. It is threaded from the --max-concurrent-reconciles flag and
	// applied to the controller's controller.Options in SetupWithManager. A value
	// <= 0 falls back to bootstrap.DefaultMaxConcurrentReconciles inside
	// bootstrap.ControllerOptions, so the zero value is safe.
	MaxConcurrentReconciles int

	// HTTPClient is the health-check client seam. Production leaves it nil so the
	// health check uses http.DefaultClient; tests inject a stub transport.
	HTTPClient healthcheck.HTTPDoer

	// Resolver resolves the target cluster a Barbican CR names in
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

	// gatewayAPIAvailable is set during SetupWithManager from the cluster's
	// RESTMapper and indicates whether the gateway.networking.k8s.io/v1 HTTPRoute
	// CRD is installed. When false, the controller skips the HTTPRoute watch
	// entirely so it does not crash on a missing kind.
	gatewayAPIAvailable bool

	// healthProbeCache memoizes the last successful Barbican API probe per CR
	// (shared TTL probe cache) so a steady-state reconcile does not fire a
	// synchronous HTTP GET on every pass. The cache's internal mutex guards
	// concurrent access under MaxConcurrentReconciles > 1.
	healthProbeCache healthcheck.ProbeCache
}

// barbicanRemoteChildKinds are the kinds a Barbican CR projects into the
// namespace of the target cluster it names, and the kinds
// reconcileDeleteRemoteChildren sweeps by ownership label when that CR is
// deleted. Nothing on the target cluster collects them, so a kind missing from
// this list is a kind that keeps running after its CR is gone.
//
// The list is cross-checked against the create verbs of the kubebuilder RBAC
// markers on this controller below: the operator can only leave behind what it
// is allowed to create.
var barbicanRemoteChildKinds = []schema.GroupVersionKind{
	appsv1.SchemeGroupVersion.WithKind("Deployment"),
	corev1.SchemeGroupVersion.WithKind("Service"),
	corev1.SchemeGroupVersion.WithKind("ConfigMap"),
	corev1.SchemeGroupVersion.WithKind("Secret"),
	batchv1.SchemeGroupVersion.WithKind("Job"),
	batchv1.SchemeGroupVersion.WithKind("CronJob"),
	policyv1.SchemeGroupVersion.WithKind("PodDisruptionBudget"),
	autoscalingv2.SchemeGroupVersion.WithKind("HorizontalPodAutoscaler"),
	networkingv1.SchemeGroupVersion.WithKind("NetworkPolicy"),
	httpRouteGVK,
	mariadbv1alpha1.GroupVersion.WithKind("Database"),
	mariadbv1alpha1.GroupVersion.WithKind("User"),
	mariadbv1alpha1.GroupVersion.WithKind("Grant"),
}

// +kubebuilder:rbac:groups=barbican.openstack.c5c3.io,resources=barbicans,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=barbican.openstack.c5c3.io,resources=barbicans/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=barbican.openstack.c5c3.io,resources=barbicans/finalizers,verbs=update
// +kubebuilder:rbac:groups=barbican.openstack.c5c3.io,resources=barbicansecretstores,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services;configmaps;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=batch,resources=jobs;cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=databases;users;grants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=mariadbs,verbs=get;list;watch
// +kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=external-secrets.io,resources=clustersecretstores;secretstores,verbs=get;list;watch
// +kubebuilder:rbac:groups=openbao.org,resources=openbaoclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=get
// +kubebuilder:rbac:groups=scheduling.k8s.io,resources=priorityclasses,verbs=get;list;watch

// Reconcile is the main reconciliation loop for the Barbican CR. It fetches the
// CR, drives the finalizer-gated deletion path, ensures the finalizer, then runs
// the sub-reconciler pipeline. Every exit funnels through updateStatus, which
// re-aggregates the Ready condition and stamps ObservedGeneration.
func (r *BarbicanReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var barbican barbicanv1alpha1.Barbican
	if err := r.Get(ctx, req.NamespacedName, &barbican); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("Barbican resource not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching Barbican: %w", err)
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
	if !barbican.DeletionTimestamp.IsZero() {
		children, wait := commonmulticluster.ResolveChildrenClientForDeletion(
			ctx, r.Resolver, r.Client, barbican.Spec.TargetClusterRef, *barbican.DeletionTimestamp)
		if wait {
			// The hold goes on the CR, not only into the operator's log. It is a
			// deliberate state a CR can sit in for minutes, and "Terminating,
			// waiting on the target cluster" has to be distinguishable from a
			// wedged finalizer without correlating logs across replicas. This exit
			// precedes the pipeline's status snapshot below, so it takes its own
			// baseline for the skip-unchanged write.
			statusBefore := barbican.Status.DeepCopy()
			barbicanSkeleton.MarkFailed(&barbican, "SecretsReady",
				commonmulticluster.TargetClusterUnavailable,
				fmt.Errorf("target cluster %s does not resolve; waiting at least %s before abandoning its children",
					barbican.Spec.TargetClusterRef.Name, commonmulticluster.AbandonAfter))
			return r.updateStatus(ctx, &barbican, statusBefore,
				ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil)
		}
		if result, err := r.reconcileDelete(ctx, children, &barbican); !result.IsZero() || err != nil {
			return result, err
		}
		// The label-selected sweep runs after the named MariaDB cleanup, never
		// before it: that flow waits one pass on the CRs it deletes by name, and a
		// sweep running first would delete them out from under it. The ordering
		// mirrors the local one, where the garbage collection cascade starts only
		// once every finalizer has been released.
		if err := r.reconcileDeleteRemoteChildren(ctx, children, &barbican); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Resolve the client every child object of this CR is read and written with.
	// The embedded client stays on the management cluster (the CR, its status,
	// its finalizer, and the sibling BarbicanSecretStores live there); children
	// carries everything the CR projects into the target cluster. The resolution
	// runs before the finalizer is added so a CR naming an unresolvable cluster
	// stays clean of finalizers: nothing was created for it, so there is nothing
	// to clean up, and a finalizer would only block its deletion.
	children, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client, barbican.Spec.TargetClusterRef)
	if err != nil {
		// This exit precedes the pipeline's status snapshot below, so it takes its
		// own baseline for the skip-unchanged write.
		statusBefore := barbican.Status.DeepCopy()
		barbicanSkeleton.MarkFailed(&barbican, "SecretsReady", commonmulticluster.TargetClusterUnavailable, err)
		return r.updateStatus(ctx, &barbican, statusBefore,
			ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil)
	}

	// Ensure the finalizer is installed before any sub-reconciler runs so a
	// deletion issued before the next pass still funnels through reconcileDelete.
	// Requeuing after the Update guarantees the next reconcile observes the
	// persisted finalizer rather than the in-memory copy.
	if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &barbican, barbicanFinalizer); err != nil {
		return ctrl.Result{}, err
	} else if added {
		return ctrl.Result{Requeue: true}, nil
	}

	// The remote-children finalizer goes on only when the CR projects onto a
	// target cluster. A local CR keeps the garbage collection cascade, which
	// reaps its children from their owner references, so it has nothing for this
	// finalizer to hold the CR open for. spec.targetClusterRef is immutable, so
	// the condition cannot flip under a live CR.
	if barbican.Spec.TargetClusterRef != nil {
		if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &barbican,
			commonmulticluster.RemoteChildrenFinalizer); err != nil {
			return ctrl.Result{}, err
		} else if added {
			return ctrl.Result{Requeue: true}, nil
		}
	}

	// Snapshot the persisted status so updateStatus can skip the write when a
	// pass leaves status unchanged (no write → no watch event → no
	// resourceVersion churn). Taken after the finalizer add so an early requeue
	// there does not race a status write.
	statusBefore := barbican.Status.DeepCopy()

	result, err := commonreconcile.RunPipeline(ctx, instrumenter.Instrument, r.pipelineSteps(children, &barbican))
	return r.updateStatus(ctx, &barbican, statusBefore, result, err)
}

// pipelineSteps returns the ordered sub-reconciler pipeline for one Barbican.
// Each step runs in dependency order; the first to return a non-zero result or
// an error short-circuits the chain and funnels through updateStatus.
//
// It is a method rather than a literal inside Reconcile so the drift guard can
// enumerate the step names without running a reconcile.
func (r *BarbicanReconciler) pipelineSteps(children client.Client, barbican *barbicanv1alpha1.Barbican) []commonreconcile.Step {
	// The values one sub-reconciler hands to a later one within a single
	// reconcile pass, captured by the step closures. projection is the
	// secret-store aggregation the config, deployment and networkpolicy steps
	// read. configSecretName names the rendered config Secret the migration Job,
	// the API pods and the clean-up CronJob mount. authtokenDigest and dsnDigest
	// are the content digests of the service-user password and the assembled DSN;
	// the deployment step stamps them into pod-template annotations so a rotated
	// credential rolls the pods.
	var (
		projection                                   secretStoreProjection
		configSecretName, authtokenDigest, dsnDigest string
	)

	return []commonreconcile.Step{
		{Name: "Secrets", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, authtokenDigest, err = r.reconcileSecrets(ctx, children, barbican)
			return res, err
		}},
		// reconcileDBConnectionSecret materialises the DB URL into the derived
		// <barbican.Name>-db-connection Secret. It runs after Secrets (upstream
		// credentials must be synced) and before Config; failures set
		// SecretsReady=False, the same condition reconcileSecrets uses.
		{Name: "DBConnectionSecret", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, dsnDigest, err = r.reconcileDBConnectionSecret(ctx, children, barbican)
			return res, err
		}},
		// reconcileSecretStores aggregates the attached, credential-ready
		// BarbicanSecretStores into the secret-store sections of barbican.conf.
		// Waiting states NEVER short-circuit the pipeline — the step returns a zero
		// result so first-install can proceed, and store status flips re-enqueue
		// this Barbican through the store watch.
		{Name: "SecretStores", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, projection, err = r.reconcileSecretStores(ctx, children, barbican)
			return res, err
		}},
		// reconcileConfig renders barbican.conf and barbican-api-paste.ini into an
		// immutable Secret. On an invalid projection it returns the live
		// Deployment's last-good Secret name instead of re-rendering. It self-marks
		// SecretsReady=False on failure via markConfigFailed, so the wrapper only
		// threads the result.
		{Name: "Config", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, configSecretName, err = r.reconcileConfig(ctx, children, barbican, projection)
			return res, err
		}},
		// reconcileDBClean projects the recurring clean-up CronJob. It runs BEFORE
		// Database, not in the parallel group behind it: the clean-up's bulk
		// DELETEs contend with the DDL locks a schema migration holds, and the
		// pipeline short-circuits at the Database step for as long as the sync Job
		// runs — so a step placed behind it is never reached to pause the schedule
		// during the one window that needs pausing. Its only input is the rendered
		// config the CronJob mounts, which the Config step above already produced.
		{Name: "DBClean", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDBClean(ctx, children, barbican, configSecretName)
		}},
		// reconcileDatabase provisions the schema, gates the requested OpenStack
		// release against the installed one, and runs the db-sync Job against the
		// rendered config.
		{Name: "Database", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDatabase(ctx, children, barbican, configSecretName)
		}},
		// reconcileDeployment projects the API Deployment, its Service, and the
		// PodDisruptionBudget. It runs after Database so the pods only start once
		// the schema they query exists, and creates nothing while the secret-store
		// projection is invalid.
		{Name: "Deployment", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDeployment(ctx, children, barbican, projection, configSecretName, dsnDigest, authtokenDigest)
		}},
		// Once the Deployment/Service outputs are in place, HTTPRoute, HealthCheck,
		// HPA, and NetworkPolicy have no inter-dependency and run concurrently.
		// Each member sets exactly one condition type; the group self-instruments
		// its members, so this step carries no sub_reconciler name.
		{Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileParallelGroup(ctx, barbican, r.parallelSteps(children, projection))
		}},
	}
}

// parallelSteps returns the members of the post-deployment parallel group. Each
// member sets exactly one condition type and receives its own copy of the CR, so
// none of them reads a value another one produces. projection is the secret-store
// aggregation an earlier step produced in the same pass.
//
// It is a method rather than a literal inside pipelineSteps so the drift guard
// can enumerate the member names and their condition types without running a
// reconcile.
func (r *BarbicanReconciler) parallelSteps(children client.Client, projection secretStoreProjection) []commonreconcile.ParallelStep[*barbicanv1alpha1.Barbican] {
	return []commonreconcile.ParallelStep[*barbicanv1alpha1.Barbican]{
		{
			Name:          "HTTPRoute",
			ConditionType: conditionTypeHTTPRouteReady,
			Fn: func(ctx context.Context, b *barbicanv1alpha1.Barbican) (ctrl.Result, error) {
				return r.reconcileHTTPRoute(ctx, children, b)
			},
		},
		{
			Name:          "HealthCheck",
			ConditionType: conditionTypeBarbicanAPIReady,
			Fn: func(ctx context.Context, b *barbicanv1alpha1.Barbican) (ctrl.Result, error) {
				return r.reconcileHealthCheck(ctx, b)
			},
		},
		{
			Name:          "HPA",
			ConditionType: "HPAReady",
			Fn: func(ctx context.Context, b *barbicanv1alpha1.Barbican) (ctrl.Result, error) {
				return r.reconcileHPA(ctx, children, b)
			},
		},
		{
			Name:          "NetworkPolicy",
			ConditionType: conditionTypeNetworkPolicyReady,
			Fn: func(ctx context.Context, b *barbicanv1alpha1.Barbican) (ctrl.Result, error) {
				return r.reconcileNetworkPolicy(ctx, children, b, projection)
			},
		},
	}
}

// reconcileParallelGroup runs the given sub-reconcilers concurrently, delegating
// to the shared skeleton: each member operates on its own DeepCopy of the
// Barbican CR, conditions from every member (including those that succeeded
// before a peer failed) are merged back into the primary barbican, and on
// success the shortest non-zero RequeueAfter is returned. Members instrument
// individually via instrumenter.Instrument.
func (r *BarbicanReconciler) reconcileParallelGroup(
	ctx context.Context,
	barbican *barbicanv1alpha1.Barbican,
	subs []commonreconcile.ParallelStep[*barbicanv1alpha1.Barbican],
) (ctrl.Result, error) {
	return barbicanSkeleton.RunParallelGroup(ctx, barbican, instrumenter.Instrument, subs)
}

// reconcileDelete drives the finalizer cleanup when the Barbican CR is being
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
func (r *BarbicanReconciler) reconcileDelete(ctx context.Context, children client.Client, barbican *barbicanv1alpha1.Barbican) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(barbican, barbicanFinalizer) {
		return ctrl.Result{}, nil
	}

	key := client.ObjectKey{Name: barbican.Name, Namespace: barbican.Namespace}

	if children == nil {
		r.Recorder.Event(barbican, corev1.EventTypeWarning, "RemoteChildrenAbandoned",
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
			r.Recorder.Event(barbican, corev1.EventTypeNormal, "FinalizingDatabase",
				"Cleaning up MariaDB Database, User, and Grant before removing Barbican")
			return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
		}

		r.Recorder.Event(barbican, corev1.EventTypeNormal, "DatabaseFinalized",
			"MariaDB Database, User, and Grant marked for deletion; releasing finalizer")
	}

	controllerutil.RemoveFinalizer(barbican, barbicanFinalizer)
	if err := r.Update(ctx, barbican); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	barbicanmetrics.DeleteForBarbican(barbican.Name, barbican.Namespace)
	// Drop the per-CR health-probe cache so a CR recreated under the same
	// name/namespace never serves a stale probe keyed on the deleted CR's UID.
	r.healthProbeCache.Evict(key)
	return ctrl.Result{}, nil
}

// reconcileDeleteRemoteChildren deletes everything this Barbican projected onto
// the target cluster it names and releases the remote-children finalizer, as
// commonmulticluster.SweepRemoteChildren documents. reconcileDelete above
// deletes the three MariaDB CRs it tracks by name; this pass is what reaches the
// rest, selected on the ownership labels Claim stamped on them.
func (r *BarbicanReconciler) reconcileDeleteRemoteChildren(ctx context.Context, children client.Client, barbican *barbicanv1alpha1.Barbican) error {
	return commonmulticluster.SweepRemoteChildren(ctx, r.Client, r.Resolver, r.Recorder, r.Scheme,
		barbican, barbican.Spec.TargetClusterRef, children, barbicanRemoteChildKinds)
}

// updateStatus persists the current status conditions and returns the given
// result and error, delegating to the shared skeleton: the write is skipped when
// the pass left status semantically unchanged from the statusBefore snapshot, a
// failed write is joined with reconcileErr, and the mutate hook re-aggregates the
// Ready condition on every persist and stamps status.observedGeneration.
func (r *BarbicanReconciler) updateStatus(ctx context.Context, barbican *barbicanv1alpha1.Barbican, statusBefore *barbicanv1alpha1.BarbicanStatus, result ctrl.Result, reconcileErr error) (ctrl.Result, error) {
	return barbicanSkeleton.UpdateStatus(ctx, r.Client, barbican, statusBefore, &barbican.Status, func() {
		barbican.Status.ObservedGeneration = barbican.Generation
	}, result, reconcileErr)
}

// setReadyCondition sets the aggregate Ready condition based on all
// sub-conditions, delegating to the shared skeleton with barbican's
// sub-condition vocabulary.
func setReadyCondition(barbican *barbicanv1alpha1.Barbican) {
	barbicanSkeleton.SetReady(barbican)
}

// SetupWithManager registers the BarbicanReconciler with the controller manager.
// It probes Gateway API availability, registers BOTH the Barbican and
// BarbicanSecretStore field indexes (the single registration site for both
// controllers, so this reconciler MUST be set up before
// BarbicanSecretStoreReconciler), and wires the owned resources and
// cross-resource watches.
func (r *BarbicanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Detect whether the Gateway API CRD is installed. spec.gateway is optional,
	// so the operator must run on clusters without Gateway API. Adding
	// Owns(HTTPRoute) unconditionally would fail at Start with "no matches for
	// kind HTTPRoute" when the CRD is missing, blocking every Barbican CR.
	r.gatewayAPIAvailable = gateway.IsGVKAvailable(mgr.GetRESTMapper(), httpRouteGVK)
	r.apiReader = mgr.GetAPIReader()
	setupLog := ctrl.Log.WithName("barbican-setup")
	if r.gatewayAPIAvailable {
		setupLog.Info("Gateway API detected; enabling HTTPRoute watch and reconciliation")
	} else {
		setupLog.Info("Gateway API not installed; HTTPRoute watch disabled, spec.gateway will be rejected via HTTPRouteReady condition")
	}

	// Register the Barbican field indexer before Watches so
	// secretToBarbicanWithStoresMapper can rely on it for its MatchingFields
	// lookup.
	if err := registerBarbicanIndexes(context.Background(), mgr.GetFieldIndexer()); err != nil {
		return err
	}
	// Register the BarbicanSecretStore indexes here — the single registration
	// site for both controllers (this reconciler is set up before
	// BarbicanSecretStoreReconciler in main.go and the envtest helper).
	// reconcileSecretStores and the mappers rely on them.
	if err := registerBarbicanSecretStoreIndexes(context.Background(), mgr.GetFieldIndexer()); err != nil {
		return err
	}

	b := ctrl.NewControllerManagedBy(mgr).
		// Shared controller options: MaxConcurrentReconciles lets independent CRs
		// reconcile in parallel, and the tuned RateLimiter caps per-item failure
		// backoff at 30s (see bootstrap.ControllerOptions).
		WithOptions(bootstrap.ControllerOptions(r.MaxConcurrentReconciles)).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate).
		For(&barbicanv1alpha1.Barbican{}, builder.WithPredicates(watch.CRUpdatePredicate())).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&batchv1.Job{}).
		Owns(&batchv1.CronJob{})

	if r.gatewayAPIAvailable {
		b = b.Owns(&gatewayv1.HTTPRoute{})
	}

	return b.
		// Watch Secrets and map to the Barbican CRs that reference them (directly
		// or via an attached BarbicanSecretStore's AppRole credentials/CA Secret).
		// ESO-managed Secrets are owned by the ExternalSecret controller, not the
		// Barbican CR, so EnqueueRequestForOwner would never match them.
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(
			secretToBarbicanWithStoresMapper(mgr.GetClient()),
		)).
		// Watch BarbicanSecretStores and map to their parent Barbican: store
		// status flips (CredentialsReady turning True) trigger projection,
		// DeletionTimestamp flips trigger de-projection. No generation predicate —
		// the status transitions ARE the signal.
		Watches(&barbicanv1alpha1.BarbicanSecretStore{}, handler.EnqueueRequestsFromMapFunc(
			barbicanSecretStoreToBarbicanMapper(),
		)).
		// Watch the MariaDB cluster CR referenced by spec.database.clusterRef so
		// the operator reflects upstream database outages in DatabaseReady without
		// waiting for the next periodic requeue.
		Watches(&mariadbv1alpha1.MariaDB{}, handler.EnqueueRequestsFromMapFunc(
			mariaDBToBarbicanMapper(mgr.GetClient()),
		)).
		// Watch both the cluster-scoped ClusterSecretStore and the namespaced
		// SecretStore a Barbican can select via spec.secretStoreRef, so the
		// operator reflects upstream secret-backend outages in SecretsReady as soon
		// as ESO flips the selected store's Ready condition.
		Watches(&esov1.ClusterSecretStore{}, handler.EnqueueRequestsFromMapFunc(
			esoStoreToBarbicanMapper(mgr.GetClient(), commonv1.SecretStoreKindCluster),
		)).
		Watches(&esov1.SecretStore{}, handler.EnqueueRequestsFromMapFunc(
			esoStoreToBarbicanMapper(mgr.GetClient(), commonv1.SecretStoreKindNamespaced),
		)).
		Complete(r)
}
