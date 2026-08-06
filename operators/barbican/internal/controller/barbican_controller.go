// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the Barbican and BarbicanSecretStore
// reconcilers.
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
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/bootstrap"
	"github.com/c5c3/forge/internal/common/database"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
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
// NetworkPolicy, DBClean) always sets its condition — configured-ready,
// NotRequired, or waiting — so a gateway-less or autoscaling-less cluster still
// resolves the aggregate (the NotRequired paths report True), exactly as the
// sibling operators aggregate their optional conditions.
var subConditionTypes = []string{
	"SecretsReady",
	"DatabaseReady",
	conditionTypeSecretStoresReady,
	"DeploymentReady",
	"BarbicanAPIReady",
	"HPAReady",
	"NetworkPolicyReady",
	"HTTPRouteReady",
	"DBCleanReady",
}

// barbicanFinalizer blocks removal of a Barbican CR from etcd until the MariaDB
// Database, User, and Grant CRs it owns have been issued a Delete, so the schema
// teardown is triggered before the owner-ref chain disappears. It is the single
// source of truth for Reconcile, the finalizer handler, and tests.
const barbicanFinalizer = "barbican.openstack.c5c3.io/finalizer"

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

	// apiReader is set during SetupWithManager from mgr.GetAPIReader(): a direct,
	// uncached reader. The deployment step latches the two-phase narrowing of the
	// API Service selector on the live Service, and the informer cache can still
	// hold the pre-narrowing state right after the narrowing write — a latch
	// decided from it could widen the selector back. Nil in unit tests that
	// construct the reconciler without a manager; those fall back to the
	// (read-your-writes) fake client.
	apiReader client.Reader
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
	// deletion path sets no conditions, so it returns directly without
	// updateStatus.
	if !barbican.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &barbican)
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

	// Snapshot the persisted status so updateStatus can skip the write when a
	// pass leaves status unchanged (no write → no watch event → no
	// resourceVersion churn). Taken after the finalizer add so an early requeue
	// there does not race a status write.
	statusBefore := barbican.Status.DeepCopy()

	result, err := commonreconcile.RunPipeline(ctx, instrumenter.Instrument, r.pipelineSteps(&barbican))
	return r.updateStatus(ctx, &barbican, statusBefore, result, err)
}

// pipelineSteps returns the ordered sub-reconciler pipeline for one Barbican.
// Each step runs in dependency order; the first to return a non-zero result or
// an error short-circuits the chain and funnels through updateStatus.
//
// It is a method rather than a literal inside Reconcile so the drift guard can
// enumerate the step names without running a reconcile.
//
// The chain is the credential-to-config half of the pipeline. The Database and
// Deployment steps and the post-deployment parallel group (HTTPRoute,
// HealthCheck, HPA, NetworkPolicy, DBClean) are appended by the following
// commits; instrumentation.go already carries the complete step-name vocabulary,
// so a step added there needs no instrumentation change.
func (r *BarbicanReconciler) pipelineSteps(barbican *barbicanv1alpha1.Barbican) []commonreconcile.Step {
	// projection is the secret-store aggregation the config step renders from,
	// the one value this half of the pipeline hands from one step to the next. The
	// digests the Secrets and DBConnectionSecret steps return, and the config
	// Secret name the Config step returns, are dropped until the deployment and
	// database steps that consume them land.
	var projection secretStoreProjection

	return []commonreconcile.Step{
		{Name: "Secrets", Fn: func(ctx context.Context) (ctrl.Result, error) {
			res, _, err := r.reconcileSecrets(ctx, barbican)
			return res, err
		}},
		// reconcileDBConnectionSecret materialises the DB URL into the derived
		// <barbican.Name>-db-connection Secret. It runs after Secrets (upstream
		// credentials must be synced) and before Config; failures set
		// SecretsReady=False, the same condition reconcileSecrets uses.
		{Name: "DBConnectionSecret", Fn: func(ctx context.Context) (ctrl.Result, error) {
			res, _, err := r.reconcileDBConnectionSecret(ctx, barbican)
			return res, err
		}},
		// reconcileSecretStores aggregates the attached, credential-ready
		// BarbicanSecretStores into the secret-store sections of barbican.conf.
		// Waiting states NEVER short-circuit the pipeline — the step returns a zero
		// result so first-install can proceed, and store status flips re-enqueue
		// this Barbican through the store watch.
		{Name: "SecretStores", Fn: func(ctx context.Context) (res ctrl.Result, err error) {
			res, projection, err = r.reconcileSecretStores(ctx, barbican)
			return res, err
		}},
		// reconcileConfig renders barbican.conf and barbican-api-paste.ini into an
		// immutable Secret. On an invalid projection it returns the live
		// Deployment's last-good Secret name instead of re-rendering. It self-marks
		// SecretsReady=False on failure via markConfigFailed, so the wrapper only
		// threads the result.
		{Name: "Config", Fn: func(ctx context.Context) (ctrl.Result, error) {
			res, _, err := r.reconcileConfig(ctx, barbican, projection)
			return res, err
		}},
	}
}

// reconcileDelete drives the finalizer cleanup when the Barbican CR is being
// deleted. It is a no-op when the finalizer is absent. Otherwise it issues
// Delete on the MariaDB Database/User/Grant CRs (idempotent, NotFound-tolerant)
// and, while at least one of them was still live (not yet issued a Delete),
// holds the finalizer for one more pass so the schema teardown is triggered
// before the owner-ref chain disappears. Once no live resource remains it drops
// the per-CR metrics and releases the finalizer.
func (r *BarbicanReconciler) reconcileDelete(ctx context.Context, barbican *barbicanv1alpha1.Barbican) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(barbican, barbicanFinalizer) {
		return ctrl.Result{}, nil
	}

	key := client.ObjectKey{Name: barbican.Name, Namespace: barbican.Namespace}

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
		r.Recorder.Event(barbican, corev1.EventTypeNormal, "FinalizingDatabase",
			"Cleaning up MariaDB Database, User, and Grant before removing Barbican")
		return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
	}

	r.Recorder.Event(barbican, corev1.EventTypeNormal, "DatabaseFinalized",
		"MariaDB Database, User, and Grant marked for deletion; releasing finalizer")

	controllerutil.RemoveFinalizer(barbican, barbicanFinalizer)
	if err := r.Update(ctx, barbican); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	barbicanmetrics.DeleteForBarbican(barbican.Name, barbican.Namespace)
	return ctrl.Result{}, nil
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
// It registers BOTH the Barbican and BarbicanSecretStore field indexes (the
// single registration site for both controllers, so this reconciler MUST be set
// up before BarbicanSecretStoreReconciler) and wires the resources this half of
// the pipeline owns.
//
// The Deployment, Service, PodDisruptionBudget, HorizontalPodAutoscaler,
// NetworkPolicy, Job, CronJob and HTTPRoute ownerships, the Gateway API
// availability probe, and the Secret/BarbicanSecretStore/MariaDB/SecretStore
// watch mappers land with the workload and watch commits; only the config
// Secret is owned here.
func (r *BarbicanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.apiReader = mgr.GetAPIReader()

	// Register the Barbican field indexer before the store indexes so a mapper
	// added later can rely on it for its MatchingFields lookup.
	if err := registerBarbicanIndexes(context.Background(), mgr.GetFieldIndexer()); err != nil {
		return err
	}
	// Register the BarbicanSecretStore indexes here — the single registration
	// site for both controllers (this reconciler is set up before
	// BarbicanSecretStoreReconciler in main.go and the envtest helper).
	// reconcileSecretStores and the store controller's watch mappers rely on them.
	if err := registerBarbicanSecretStoreIndexes(context.Background(), mgr.GetFieldIndexer()); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		// Shared controller options: MaxConcurrentReconciles lets independent CRs
		// reconcile in parallel, and the tuned RateLimiter caps per-item failure
		// backoff at 30s (see bootstrap.ControllerOptions).
		WithOptions(bootstrap.ControllerOptions(r.MaxConcurrentReconciles)).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate).
		For(&barbicanv1alpha1.Barbican{}, builder.WithPredicates(watch.CRUpdatePredicate())).
		Owns(&corev1.Secret{}).
		Complete(r)
}
