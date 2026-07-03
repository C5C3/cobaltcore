// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the Keystone reconciler.
package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/forge/internal/common/conditions"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
	"github.com/c5c3/forge/operators/keystone/internal/metrics"
)

// keystoneFinalizer is the name of the finalizer added to every Keystone CR so
// that MariaDB Database, User, and Grant CRs are deterministically cleaned up
// before the Keystone CR is removed from etcd. Defined once as the single
// source of truth for Reconcile, the finalizer handler, tests, and docs
const keystoneFinalizer = "keystone.openstack.c5c3.io/finalizer"

// KeystoneSecretNameIndexKey is the field-indexer key under which Keystone
// CRs are indexed by the union of their referenced Secret names
// (spec.database.secretRef.name and spec.bootstrap.adminPasswordSecretRef.name).
// Used by SetupWithManager to register the indexer and by
// secretToKeystoneMapper to perform an O(1) reverse lookup, replacing the
// prior unfiltered List of all Keystone CRs in the namespace.
// #nosec G101 -- field-indexer key (a JSONPath-like field selector), not a credential.
const KeystoneSecretNameIndexKey = "spec.secretRefs.name"

// keystoneSecretNameExtractor is the controller-runtime IndexerFunc registered
// under KeystoneSecretNameIndexKey. It returns the deduplicated, non-empty
// union of Secret names referenced by a Keystone CR — currently
// spec.database.secretRef.name and spec.bootstrap.adminPasswordSecretRef.name —
// so the field indexer can resolve a Secret event to the referencing CR(s)
// without listing every Keystone in the namespace.
func keystoneSecretNameExtractor(obj client.Object) []string {
	ks, ok := obj.(*keystonev1alpha1.Keystone)
	if !ok {
		// controller-runtime should never call us with the wrong type; a nil
		// return is safer than a panic if it ever does.
		return nil
	}
	dbName := ks.Spec.Database.SecretRef.Name
	adminName := ks.Spec.Bootstrap.AdminPasswordSecretRef.Name

	names := make([]string, 0, 2)
	if dbName != "" {
		names = append(names, dbName)
	}
	if adminName != "" && adminName != dbName {
		names = append(names, adminName)
	}
	return names
}

// registerSecretNameIndex registers the Keystone field indexer under
// KeystoneSecretNameIndexKey with the given FieldIndexer. SetupWithManager
// calls this once against mgr.GetFieldIndexer() so that secretToKeystoneMapper
// can resolve a Secret event to the referencing Keystone CRs via an O(1)
// reverse lookup instead of an unfiltered namespace-scoped List. The returned
// error is wrapped with the index key so the registration site is identifiable
// in manager-startup failure logs.
func registerSecretNameIndex(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &keystonev1alpha1.Keystone{}, KeystoneSecretNameIndexKey, keystoneSecretNameExtractor); err != nil {
		return fmt.Errorf("registering field indexer %q: %w", KeystoneSecretNameIndexKey, err)
	}
	return nil
}

// subConditionTypes lists the condition types set by individual sub-reconcilers.
// The Ready condition is True only when all of these are True.
var subConditionTypes = []string{
	"SecretsReady",
	"FernetKeysReady",
	"CredentialKeysReady",
	"DatabaseReady",
	conditionTypeDatabaseTLSReady,
	conditionTypePolicyValidReady,
	"DeploymentReady",
	conditionTypeKeystoneAPIReady,
	"HPAReady",
	"NetworkPolicyReady",
	conditionTypeHTTPRouteReady,
	"BootstrapReady",
	"TrustFlushReady",
	conditionTypePasswordRotationReady,
}

// KeystoneReconciler reconciles a Keystone object.
type KeystoneReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   record.EventRecorder
	HTTPClient HTTPDoer

	// OperatorNamespace is the Namespace the operator Pod runs in (resolved at
	// startup by DetectOperatorNamespace). reconcileNetworkPolicy appends an
	// ingress peer for this Namespace so the operator's own health check can
	// reach the Keystone API on TCP 5000. Empty when the namespace could not be
	// determined, in which case no operator-namespace peer is added.
	OperatorNamespace string

	// gatewayAPIAvailable is set during SetupWithManager from the cluster's
	// RESTMapper and indicates whether the gateway.networking.k8s.io/v1
	// HTTPRoute CRD is installed. When false, the controller skips the
	// HTTPRoute watch entirely so it does not crash on a missing kind, and
	// reconcileHTTPRoute surfaces a clear HTTPRouteReady=False condition if
	// the user nonetheless sets spec.gateway.
	gatewayAPIAvailable bool

	// certManagerAvailable is set during SetupWithManager from the cluster's
	// RESTMapper and indicates whether the cert-manager.io/v1 Certificate CRD
	// is installed. When false, the controller skips the Certificate watch and
	// reconcileDatabaseTLS skips the disable-path Certificate delete (no
	// Certificate can exist), so the default no-TLS configuration never errors
	// with "no matches for kind Certificate" on clusters without cert-manager
	// (issue #475).
	certManagerAvailable bool

	// MaxConcurrentReconciles bounds how many Keystone CRs reconcile
	// concurrently. It is threaded from the --max-concurrent-reconciles flag
	// (see internal/common/bootstrap) and applied to the controller's
	// controller.Options in SetupWithManager. A value <= 0 falls back to
	// defaultMaxConcurrentReconciles via effectiveMaxConcurrentReconciles, so
	// the zero value is safe for programmatically constructed reconcilers.
	MaxConcurrentReconciles int

	// healthProbeCache memoizes the last successful Keystone API health probe
	// per CR so a steady-state reconcile does not fire a synchronous HTTP GET
	// (bounded by HealthCheckTimeout) on every pass. A cache hit re-upserts
	// KeystoneAPIReady=True without probing; any probe error or non-2xx evicts
	// the entry. Lazily initialised under healthProbeCacheMu, which also guards
	// concurrent access now that MaxConcurrentReconciles may exceed 1.
	healthProbeCache   map[types.NamespacedName]healthProbeCacheEntry
	healthProbeCacheMu sync.Mutex

	// now is the clock used for the health-probe cache TTL comparison. When
	// nil it defaults to time.Now; tests inject a controllable clock so the TTL
	// boundary is deterministic.
	now func() time.Time

	// configRenderCache memoizes the rendered config ConfigMap name per CR,
	// keyed on the CR UID + generation + the referenced policy ConfigMap's
	// ResourceVersion, so a steady-state reconcile returns the known name
	// without re-running RenderINI/RenderPastePipelineINI/RenderPolicyYAML. The
	// ConfigMap is content-addressed and immutable, so nothing else can change
	// its content between passes. Lazily initialised under configRenderCacheMu,
	// which also guards concurrent access under MaxConcurrentReconciles > 1.
	configRenderCache   map[types.NamespacedName]configRenderCacheEntry
	configRenderCacheMu sync.Mutex
}

// httpRouteGVK identifies the HTTPRoute kind the operator would watch when
// Gateway API is installed.
var httpRouteGVK = schema.GroupVersionKind{
	Group:   gatewayv1.GroupVersion.Group,
	Version: gatewayv1.GroupVersion.Version,
	Kind:    "HTTPRoute",
}

// isGatewayAPIAvailable probes the manager's RESTMapper for the HTTPRoute kind.
// Returns false when the mapper has no mapping (CRD not installed) and true
// when the mapping exists. Other mapper errors are treated as "unknown";
// returning false in that case is conservative — the operator starts without
// the HTTPRoute watch and a clear status condition replaces the cryptic
// controller-runtime "no matches for kind" startup error.
func isGatewayAPIAvailable(mapper meta.RESTMapper) bool {
	if mapper == nil {
		return false
	}
	if _, err := mapper.RESTMapping(httpRouteGVK.GroupKind(), httpRouteGVK.Version); err != nil {
		return false
	}
	return true
}

// certificateGVK identifies the cert-manager Certificate kind the operator
// owns when cert-manager is installed.
var certificateGVK = schema.GroupVersionKind{
	Group:   certmanagerv1.SchemeGroupVersion.Group,
	Version: certmanagerv1.SchemeGroupVersion.Version,
	Kind:    "Certificate",
}

// isCertManagerAvailable probes the manager's RESTMapper for the Certificate
// kind, mirroring isGatewayAPIAvailable. Returns false when the mapper has no
// mapping (CRD not installed); other mapper errors are treated conservatively
// as "not available" so the operator starts without the Certificate watch and
// reconcileDatabaseTLS skips the disable-path delete rather than erroring with
// "no matches for kind Certificate" (issue #475).
func isCertManagerAvailable(mapper meta.RESTMapper) bool {
	if mapper == nil {
		return false
	}
	if _, err := mapper.RESTMapping(certificateGVK.GroupKind(), certificateGVK.Version); err != nil {
		return false
	}
	return true
}

// subReconcilerStep is one entry in the sequential reconcile pipeline driven by
// Reconcile. name is the sub_reconciler metric label resolved via
// subReconcilerConditionTypes. A step with an empty name is NOT wrapped in
// instrumentSubReconciler because it either self-instruments its members (the
// parallel group, whose parallelSubReconciler entries instrument individually)
// or is intentionally uninstrumented (config pruning) (issue #467).
type subReconcilerStep struct {
	name string
	fn   func(ctx context.Context) (ctrl.Result, error)
}

// +kubebuilder:rbac:groups=keystone.openstack.c5c3.io,resources=keystones,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keystone.openstack.c5c3.io,resources=keystones/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=keystone.openstack.c5c3.io,resources=keystones/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services;configmaps;secrets;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list
// +kubebuilder:rbac:groups=batch,resources=jobs;cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=databases;users;grants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=mariadbs,verbs=get;list;watch
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=external-secrets.io,resources=pushsecrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=external-secrets.io,resources=clustersecretstores,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=get
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=scheduling.k8s.io,resources=priorityclasses,verbs=get;list;watch

// Reconcile is the main reconciliation loop for the Keystone CR.
func (r *KeystoneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Fetch the Keystone CR.
	var keystone keystonev1alpha1.Keystone
	if err := r.Get(ctx, req.NamespacedName, &keystone); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("Keystone resource not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching Keystone: %w", err)
	}

	// Snapshot the persisted status so updateStatus can skip the write when a
	// pass leaves status unchanged (no write → no watch event → no
	// resourceVersion churn). Taken before any sub-reconciler or finalizer
	// mutates conditions.
	statusBefore := keystone.Status.DeepCopy()

	// Handle CR deletion via finalizers: block removal from etcd until the
	// MariaDB Database/User/Grant CRs and the OpenBao backup
	// PushSecrets owned by this Keystone are cleaned up. Both
	// finalizers run in the same pass; reconcileDeleteOpenBao may requeue while
	// a PushSecret is still Terminating, in which case updateStatus persists
	// the OpenBaoFinalizerBlocked condition.
	if !keystone.DeletionTimestamp.IsZero() {
		if result, err := r.reconcileDelete(ctx, &keystone); !result.IsZero() || err != nil {
			return result, err
		}
		if result, err := r.reconcileDeleteOpenBao(ctx, &keystone); !result.IsZero() || err != nil {
			return r.updateStatus(ctx, &keystone, statusBefore, result, err)
		}
		return ctrl.Result{}, nil
	}

	// Ensure the MariaDB finalizer is installed before any sub-reconciler runs
	// so that a deletion issued between now and the next pass still funnels
	// through reconcileDelete. Returning
	// Requeue=true after the Update guarantees the next reconcile observes the
	// persisted finalizer rather than relying on the in-memory copy.
	if !controllerutil.ContainsFinalizer(&keystone, keystoneFinalizer) {
		controllerutil.AddFinalizer(&keystone, keystoneFinalizer)
		if err := r.Update(ctx, &keystone); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Ensure the OpenBao finalizer is installed so that deleting the Keystone
	// CR blocks on cleanup of the fernet-keys-backup and credential-keys-backup
	// PushSecrets, which ESO then uses to purge the kv-v2 paths in OpenBao
	if !controllerutil.ContainsFinalizer(&keystone, keystoneOpenBaoFinalizer) {
		controllerutil.AddFinalizer(&keystone, keystoneOpenBaoFinalizer)
		if err := r.Update(ctx, &keystone); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding openbao finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Run the sub-reconciler pipeline. Steps are attempted in dependency order;
	// the first to return a non-zero result or an error short-circuits the chain
	// and funnels through updateStatus, so conditions and the requeue/error are
	// persisted by construction on every exit path. Named steps are wrapped in
	// instrumentSubReconciler (emitting duration/error series under their
	// sub_reconciler label); the two empty-name steps are not wrapped because
	// they either self-instrument their members (the parallel group) or are
	// intentionally uninstrumented (config pruning) (issue #467).
	var configMapName string
	// dbConnectionHash is the SHA-256 of the DSN materialised by
	// reconcileDBConnectionSecret; it is threaded to reconcileDeployment (like
	// configMapName) so a rotated Dynamic (engine-issued) credential rolls the
	// Deployment. DBConnectionSecret runs before Deployment in this pipeline, so
	// the value is populated by the time the Deployment step reads it.
	var dbConnectionHash string
	pipeline := []subReconcilerStep{
		{name: "Secrets", fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileSecrets(ctx, &keystone)
		}},
		// reconcileDatabaseTLS provisions the client certificate Keystone
		// presents to MariaDB/MaxScale for mutual TLS. It runs after Secrets (the
		// CA/client-cert material referenced by spec.database.tls must be synced
		// first) and before DBConnectionSecret (which appends the ssl_* DSN
		// parameters pointing at the issued client keypair).
		{name: "DatabaseTLS", fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDatabaseTLS(ctx, &keystone)
		}},
		// reconcileDBConnectionSecret materialises the DB URL into the derived
		// <keystone.Name>-db-connection Secret. It runs after Secrets (upstream
		// credentials must be synced) and before Config (the derived Secret is
		// consumed by downstream pods/Jobs). Failures set SecretsReady=False —
		// the same condition used by reconcileSecrets.
		{name: "DBConnectionSecret", fn: func(ctx context.Context) (ctrl.Result, error) {
			var (
				res ctrl.Result
				err error
			)
			res, dbConnectionHash, err = r.reconcileDBConnectionSecret(ctx, &keystone)
			return res, err
		}},
		// reconcileConfig must run before the Fernet/credential CronJobs and the
		// db_sync Job, which all require the keystone.conf ConfigMap. It returns
		// (string, error) rather than the standard (ctrl.Result, error): the
		// wrapper captures the ConfigMap name via closure and, on failure, flips
		// SecretsReady=False via markConfigFailed so the aggregate Ready cannot
		// stay stale-True at the new generation (issue #467).
		{name: "Config", fn: func(ctx context.Context) (ctrl.Result, error) {
			var err error
			configMapName, err = r.reconcileConfig(ctx, &keystone)
			if err != nil {
				markConfigFailed(&keystone, err)
			}
			return ctrl.Result{}, err
		}},
		// FernetKeys, CredentialKeys, and NetworkPolicy are independent of each
		// other and run concurrently. All three depend on Config having
		// completed. NetworkPolicy has no data dependency on the Deployment — it
		// uses selectorLabels derived from the CR. The group's members
		// self-instrument, so the step carries no sub_reconciler name
		{fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileParallelGroup(ctx, &keystone, []parallelSubReconciler{
				{
					name:          "FernetKeys",
					conditionType: "FernetKeysReady",
					fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
						return r.reconcileFernetKeys(ctx, ks, configMapName)
					},
				},
				{
					name:          "CredentialKeys",
					conditionType: "CredentialKeysReady",
					fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
						return r.reconcileCredentialKeys(ctx, ks, configMapName)
					},
				},
				{
					name:          "NetworkPolicy",
					conditionType: "NetworkPolicyReady",
					fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
						return r.reconcileNetworkPolicy(ctx, ks)
					},
				},
			})
		}},
		{name: "Database", fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDatabase(ctx, &keystone, configMapName)
		}},
		// Policy validation gates the Deployment: invalid oslo.policy overrides
		// must be caught before reaching running pods.
		{name: "PolicyValidation", fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcilePolicyValidation(ctx, &keystone, configMapName)
		}},
		{name: "Deployment", fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDeployment(ctx, &keystone, configMapName, dbConnectionHash)
		}},
		// Prune stale immutable ConfigMaps after Deployment is ready so all pods
		// run the new config before old ConfigMaps are deleted. Uninstrumented
		// (no sub_reconciler name); a prune failure is a config-concern failure,
		// so it flips SecretsReady=False via markConfigFailed rather than leaving
		// the aggregate Ready stale-True (issue #467).
		{fn: func(ctx context.Context) (ctrl.Result, error) {
			if err := r.pruneStaleConfigMaps(ctx, &keystone, configMapName); err != nil {
				markConfigFailed(&keystone, err)
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}},
		// Second parallel group. Once the Deployment/Service/Config outputs are
		// in place, HTTPRoute, HealthCheck, HPA, Bootstrap, and TrustFlush have
		// no inter-dependency: HTTPRoute needs the backend Service, HealthCheck
		// needs Status.Endpoint (both set by reconcileDeployment above), HPA
		// targets the Deployment, and Bootstrap/TrustFlush run their own Jobs
		// against the config + DB. Each member sets exactly one condition type,
		// so they merge back independently via mergeParallelConditions. The
		// group self-instruments its members, so this step carries no
		// sub_reconciler name.
		//
		// Behaviour note: previously a non-zero result from an earlier step
		// (e.g. HTTPRoute waiting on Gateway acceptance) short-circuited before
		// the later steps ran; now all five run every pass and shortestRequeue
		// aggregates their requeues — the same semantics as the FernetKeys/
		// CredentialKeys/NetworkPolicy group above (issue #361). PasswordRotation
		// stays sequential after the group because it depends on Bootstrap having
		// seeded the initial admin credential.
		{fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileParallelGroup(ctx, &keystone, []parallelSubReconciler{
				{
					name:          "HTTPRoute",
					conditionType: conditionTypeHTTPRouteReady,
					fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
						return r.reconcileHTTPRoute(ctx, ks)
					},
				},
				{
					name:          "HealthCheck",
					conditionType: conditionTypeKeystoneAPIReady,
					fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
						return r.reconcileHealthCheck(ctx, ks)
					},
				},
				{
					name:          "HPA",
					conditionType: "HPAReady",
					fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
						return r.reconcileHPA(ctx, ks)
					},
				},
				{
					name:          "Bootstrap",
					conditionType: "BootstrapReady",
					fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
						return r.reconcileBootstrap(ctx, ks, configMapName)
					},
				},
				{
					name:          "TrustFlush",
					conditionType: "TrustFlushReady",
					fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
						return r.reconcileTrustFlush(ctx, ks, configMapName)
					},
				},
			})
		}},
		// Scheduled admin-password rotation (Model B). Runs after Bootstrap has
		// seeded the initial admin credential so the rotation CronJob and
		// PushSecret never race the bootstrap seed; configMapName is accepted for
		// call-site symmetry only — the rotate script needs no keystone config
		{name: "PasswordRotation", fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcilePasswordRotation(ctx, &keystone, configMapName)
		}},
	}

	for _, s := range pipeline {
		var (
			result ctrl.Result
			err    error
		)
		if s.name == "" {
			result, err = s.fn(ctx)
		} else {
			result, err = instrumentSubReconciler(ctx, s.name, s.fn)
		}
		if !result.IsZero() || err != nil {
			return r.updateStatus(ctx, &keystone, statusBefore, result, err)
		}
	}

	// The aggregate Ready condition is recomputed inside updateStatus, which
	// every return path — this final success path included — funnels through.
	return r.updateStatus(ctx, &keystone, statusBefore, ctrl.Result{}, nil)
}

// reconcileDelete drives the finalizer cleanup when the Keystone CR is being
// deleted. It is a no-op if the Keystone finalizer is absent (e.g. a CR created
// before this operator version, or whose finalizer was already released).
// Otherwise it emits FinalizingDatabase when there is real cleanup work to
// announce, issues Delete on the MariaDB Database/User/Grant CRs, emits
// DatabaseFinalized, and releases the finalizer in a single pass.
//
// The handler deliberately does not wait for the MariaDB CRs to disappear from
// etcd: waiting created a deadlock where the Keystone finalizer kept the CR
// alive, Kubernetes GC could not cascade-delete the keystone Deployment,
// the Pod kept its connections open, and the MariaDB operator could not DROP
// DATABASE. Owner references set by reconcileDatabase ensure the MariaDB CRs
// are still reclaimed after the Keystone CR is gone — either via their own
// finalizers or via GC.
func (r *KeystoneReconciler) reconcileDelete(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(keystone, keystoneFinalizer) {
		return ctrl.Result{}, nil
	}

	// Only emit FinalizingDatabase when at least one MariaDB CR is still live
	// so brownfield CRs (no MariaDB CRs ever created) do not produce a
	// misleading "cleaning up" event.
	hasLiveCleanupWork, err := r.hasLiveMariaDBResources(ctx, keystone)
	if err != nil {
		return ctrl.Result{}, err
	}
	if hasLiveCleanupWork {
		r.Recorder.Event(keystone, corev1.EventTypeNormal, "FinalizingDatabase",
			"Cleaning up MariaDB Database, User, and Grant before removing Keystone")
	}

	if err := r.finalizeDatabaseResources(ctx, keystone); err != nil {
		return ctrl.Result{}, err
	}

	r.Recorder.Event(keystone, corev1.EventTypeNormal, "DatabaseFinalized",
		"MariaDB Database, User, and Grant marked for deletion; releasing finalizer")

	controllerutil.RemoveFinalizer(keystone, keystoneFinalizer)
	if err := r.Update(ctx, keystone); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	metrics.DeleteForKeystone(keystone.Name, keystone.Namespace)
	// Drop the per-CR hot-path caches so a CR recreated under the same
	// name/namespace never serves a stale health probe or config-render entry
	// keyed on the deleted CR's UID.
	r.evictHealthProbe(client.ObjectKeyFromObject(keystone))
	r.evictConfigRender(client.ObjectKeyFromObject(keystone))
	return ctrl.Result{}, nil
}

// hasLiveMariaDBResources reports whether any of the three MariaDB CRs
// (Database, User, Grant) owned by this Keystone still exists with
// DeletionTimestamp unset — i.e., real cleanup work remains. Brownfield CRs
// (no MariaDB CRs ever created) report false so the FinalizingDatabase event
// is suppressed when there is nothing to announce.
func (r *KeystoneReconciler) hasLiveMariaDBResources(ctx context.Context, keystone *keystonev1alpha1.Keystone) (bool, error) {
	key := mariaDBResourceKey(keystone)
	for _, ctor := range mariaDBResourceCtors {
		obj := ctor()
		err := r.Get(ctx, key, obj)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("checking %T %s: %w", obj, key, err)
		}
		if obj.GetDeletionTimestamp().IsZero() {
			return true, nil
		}
	}
	return false, nil
}

// reconcileDeleteOpenBao drives the openbao-finalizer cleanup when the Keystone
// CR is being deleted. It is a no-op if the openbao finalizer is absent.
// Otherwise it emits FinalizingOpenBaoSecrets when at least one backup
// PushSecret has been adopted by ESO and is not yet Terminating (dedupes the
// event across requeues because subsequent passes observe the PushSecrets
// gone, Terminating, or still unadopted and suppress the emit), calls
// finalizeOpenBaoSecrets, and on done=true emits OpenBaoSecretsFinalized and
// releases the finalizer. A PushSecret held Terminating by ESO's cleanup
// finalizer surfaces as ctrl.Result{RequeueAfter: RequeueSecretPolling} so the
// Keystone CR stays live until ESO has purged the kv-v2 path.
func (r *KeystoneReconciler) reconcileDeleteOpenBao(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(keystone, keystoneOpenBaoFinalizer) {
		return ctrl.Result{}, nil
	}

	// Only emit FinalizingOpenBaoSecrets when a backup PushSecret is adopted
	// by ESO and not yet Terminating — subsequent requeues observe the same
	// PushSecret Terminating (DeletionTimestamp set), absent, or still
	// unadopted and suppress the emit, giving exactly-once semantics per
	// termination. Gating on ESO adoption is what preserves the exactly-once
	// contract across the Pass-0 adoption-wait window;
	// without the gate, the 15s RequeueSecretPolling tick would fire a fresh
	// FinalizingOpenBaoSecrets event on every requeue until ESO adopts
	hasLiveCleanupWork, err := r.hasLiveOpenBaoBackupPushSecrets(ctx, keystone)
	if err != nil {
		return ctrl.Result{}, err
	}
	if hasLiveCleanupWork {
		r.Recorder.Event(keystone, corev1.EventTypeNormal, "FinalizingOpenBaoSecrets",
			"Cleaning up OpenBao backup PushSecrets before removing Keystone")
	}

	done, err := r.finalizeOpenBaoSecrets(ctx, keystone)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		return ctrl.Result{RequeueAfter: RequeueSecretPolling}, nil
	}

	r.Recorder.Event(keystone, corev1.EventTypeNormal, "OpenBaoSecretsFinalized",
		"OpenBao backup PushSecrets deleted; releasing openbao-finalizer")

	controllerutil.RemoveFinalizer(keystone, keystoneOpenBaoFinalizer)
	if err := r.Update(ctx, keystone); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing openbao finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// hasLiveOpenBaoBackupPushSecrets reports whether any backup PushSecret is
// ready to be announced via FinalizingOpenBaoSecrets — i.e. is present, not
// Terminating, AND has already been adopted by ESO (carries the ESO cleanup
// finalizer). Three disqualifiers explicitly return false:
//
//   - NotFound: nothing to clean up.
//   - Terminating (DeletionTimestamp set): the event was already emitted on
//     the prior transition; counting it again would double-announce on every
//     requeue.
//   - Adopted=false: Pass-0 is still blocking the Delete, so there is nothing
//     to announce yet. Without this gate the 15s RequeueSecretPolling tick
//     would fire a fresh Event on every requeue until ESO adopts, regressing the
//     exactly-once contract.
func (r *KeystoneReconciler) hasLiveOpenBaoBackupPushSecrets(ctx context.Context, keystone *keystonev1alpha1.Keystone) (bool, error) {
	for _, name := range openBaoBackupPushSecretNames(keystone) {
		key := client.ObjectKey{Namespace: keystone.Namespace, Name: name}
		ps := &esov1alpha1.PushSecret{}
		err := r.Get(ctx, key, ps)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("checking PushSecret %s: %w", key, err)
		}
		if ps.GetDeletionTimestamp().IsZero() && hasESOFinalizer(ps) {
			return true, nil
		}
	}
	return false, nil
}

// updateStatus persists the current status conditions and returns the given result and error.
// It also stamps the top-level status.observedGeneration with the CR's generation so a stale
// status is distinguishable from one reflecting the current spec without scanning conditions.
// When both reconcileErr and the status update fail, both errors are preserved via errors.Join
// so that the original reconcile failure is visible in controller-runtime logs.
func (r *KeystoneReconciler) updateStatus(ctx context.Context, keystone *keystonev1alpha1.Keystone, statusBefore *keystonev1alpha1.KeystoneStatus, result ctrl.Result, reconcileErr error) (ctrl.Result, error) {
	// Re-aggregate the Ready condition on every status persist, including the
	// early-return paths a sub-reconciler takes when it degrades after the CR
	// was already Ready. reconcileDeployment, for example, requeues with
	// DeploymentReady=False the moment the database-aware readiness probe
	// depools the keystone Pods under a network partition. Aggregating only at
	// the end of a fully-successful chain left Ready stale at True whenever such
	// an early return short-circuited the chain, so a degraded CR kept
	// reporting Ready=True (SC-CHAOS-006).
	setReadyCondition(keystone)
	keystone.Status.ObservedGeneration = keystone.Generation

	// Skip the write when this pass left status byte-for-byte unchanged: no
	// write means no watch event and no resourceVersion churn. meta.SetStatusCondition
	// preserves LastTransitionTime on a no-op upsert, so a converged
	// steady-state pass produces a status identical to the snapshot taken at the
	// top of Reconcile (issue #361). A nil snapshot (defensive) always writes.
	if statusBefore != nil && equality.Semantic.DeepEqual(*statusBefore, keystone.Status) {
		return result, reconcileErr
	}

	if err := r.Status().Update(ctx, keystone); err != nil {
		log.FromContext(ctx).Error(err, "unable to update Keystone status")
		return ctrl.Result{}, errors.Join(reconcileErr, fmt.Errorf("updating status: %w", err))
	}
	return result, reconcileErr
}

// setReadyCondition sets the aggregate Ready condition based on all sub-conditions.
func setReadyCondition(keystone *keystonev1alpha1.Keystone) {
	if aggregateReady(keystone.Status.Conditions) {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason:             "AllReady",
			Message:            "All sub-conditions are ready",
		})
	} else {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             "NotAllReady",
			Message:            "One or more sub-conditions are not ready",
		})
	}
}

// aggregateReady returns true if all sub-condition types are True.
func aggregateReady(conds []metav1.Condition) bool {
	return conditions.AllTrue(conds, subConditionTypes...)
}

// conditionReasonConfigError is the SecretsReady=False reason set when
// reconcileConfig or config pruning fails. Config artefacts (the rendered
// keystone.conf ConfigMap and its pruning) gate the same downstream graph as
// the upstream credential Secrets, so failures reuse SecretsReady rather than a
// dedicated condition — matching the subReconcilerConditionTypes "Config" ->
// "SecretsReady" mapping and reconcileDBConnectionSecret (issue #467).
const conditionReasonConfigError = "ConfigError"

// markConfigFailed flips SecretsReady to False so a reconcileConfig or config
// prune failure cannot leave the aggregate Ready condition stale-True at the
// new ObservedGeneration. Before this, both error paths returned a naked error
// and setReadyCondition re-aggregated the still-True sub-conditions, persisting
// Ready=True at the new generation; the failure was visible only in logs and
// the reconcile_errors counter (issue #467).
func markConfigFailed(keystone *keystonev1alpha1.Keystone, err error) {
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "SecretsReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: keystone.Generation,
		Reason:             conditionReasonConfigError,
		Message:            err.Error(),
	})
}

// shortestRequeue returns the ctrl.Result with the shortest non-zero
// RequeueAfter from the given results. If no result requests a requeue,
// a zero ctrl.Result is returned.
//
// Sub-reconcilers signal a requeue exclusively via RequeueAfter — the
// non-deprecated requeue field — so this function intentionally keys off
// RequeueAfter only. The fernet/credential reconcilers in particular return
// RequeueAfter (not the deprecated ctrl.Result.Requeue) precisely so their
// short-circuit intent survives this aggregation in the parallel group
// (issue #467).
func shortestRequeue(results ...ctrl.Result) ctrl.Result {
	var shortest ctrl.Result
	for _, r := range results {
		if r.RequeueAfter <= 0 {
			continue
		}
		if shortest.RequeueAfter <= 0 || r.RequeueAfter < shortest.RequeueAfter {
			shortest = r
		}
	}
	return shortest
}

// mergeParallelConditions copies a single condition of the given type from src
// into dst. If src does not contain a condition of that type, dst is left
// unchanged. Pre-existing conditions on dst are preserved.
func mergeParallelConditions(dst, src *keystonev1alpha1.Keystone, conditionType string) {
	cond := conditions.GetCondition(src.Status.Conditions, conditionType)
	if cond == nil {
		return
	}
	conditions.SetCondition(&dst.Status.Conditions, *cond)
}

// parallelSubReconciler describes a sub-reconciler that runs in a parallel
// group. Each sub-reconciler receives its own DeepCopy of the Keystone CR
// and sets exactly one condition type.
//
// name is the sub_reconciler label value used by the metrics helper so that
// duration/error series are attributed to the individual group member rather
// than the group as a whole.
type parallelSubReconciler struct {
	name          string
	conditionType string
	fn            func(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error)
}

// reconcileParallelGroup runs the given sub-reconcilers concurrently using
// errgroup.WithContext. Each goroutine operates on a DeepCopy of the Keystone
// CR to avoid data races. After all goroutines complete,
// conditions from every sub-reconciler — including those that succeeded before
// a peer failed — are merged back into the primary keystone so that partial
// progress is visible in status. On success the shortest non-zero RequeueAfter
// is returned.
func (r *KeystoneReconciler) reconcileParallelGroup(
	ctx context.Context,
	keystone *keystonev1alpha1.Keystone,
	subs []parallelSubReconciler,
) (ctrl.Result, error) {
	g, gctx := errgroup.WithContext(ctx)

	type outcome struct {
		result   ctrl.Result
		copy     *keystonev1alpha1.Keystone
		condType string
		err      error
	}
	outcomes := make([]outcome, len(subs))

	for i, sub := range subs {
		ksCopy := keystone.DeepCopy()
		outcomes[i].condType = sub.conditionType
		g.Go(func() error {
			// Route through instrumentSubReconciler so each parallel member
			// emits its own duration sample and — on failure — its own error
			// counter tagged with sub.name.
			res, err := instrumentSubReconciler(gctx, sub.name, func(ctx context.Context) (ctrl.Result, error) {
				return sub.fn(ctx, ksCopy)
			})
			outcomes[i].result = res
			outcomes[i].copy = ksCopy
			outcomes[i].err = err
			return err
		})
	}

	groupErr := g.Wait()

	// Merge conditions from all completed sub-reconcilers back into the
	// primary keystone, even on partial failure, so the caller can persist
	// partial progress via updateStatus.
	var results []ctrl.Result
	for _, o := range outcomes {
		mergeParallelConditions(keystone, o.copy, o.condType)
		if o.err == nil {
			results = append(results, o.result)
		}
	}

	if groupErr != nil {
		return ctrl.Result{}, groupErr
	}

	return shortestRequeue(results...), nil
}

// effectiveMaxConcurrentReconciles returns the worker count to hand to
// controller.Options.MaxConcurrentReconciles, falling back to
// defaultMaxConcurrentReconciles when the field is unset (<= 0) so a
// programmatically constructed reconciler still parallelises independent CRs.
func (r *KeystoneReconciler) effectiveMaxConcurrentReconciles() int {
	if r.MaxConcurrentReconciles > 0 {
		return r.MaxConcurrentReconciles
	}
	return defaultMaxConcurrentReconciles
}

// keystoneRateLimiter builds the controller's workqueue rate limiter. It is the
// same composition as workqueue.DefaultTypedControllerRateLimiter — a per-item
// exponential failure limiter maxed against a 10 qps / 100 burst token bucket —
// but with the per-item cap lowered from the controller-runtime default of
// 1000s to rateLimiterMaxDelay (30s). The 1000s default is far too conservative
// for an I/O-bound operator: a transiently failing CR would back off toward a
// ~16-minute retry. Lowering only the per-item cap keeps the overall
// 10 qps / 100-burst ceiling intact while bounding failure backoff to 30s.
// Genuinely slow external waits (DB, bootstrap) use explicit RequeueAfter, which
// enqueues via AddAfter and never touches this failure limiter.
func keystoneRateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](rateLimiterBaseDelay, rateLimiterMaxDelay),
		&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
	)
}

// keystoneCRPredicate filters update events on the Keystone CR's own For(...)
// watch so the controller is not re-woken by its own Status().Update writes.
// It admits an update only on a spec change (GenerationChangedPredicate) or a
// label/annotation change; a status-only update produces no reconcile.
//
// Trade-offs, all deliberate:
//   - Deletion still delivers: `kubectl delete keystone` on a CR carrying the
//     finalizer sets metadata.deletionTimestamp, which — because the CRD has a
//     status subresource — does NOT bump generation, and touches neither labels
//     nor annotations. The three predicates above would therefore filter the
//     live→Terminating Update, and the actual Delete event does not fire until
//     the finalizer is removed, so reconcileDelete would stall until the next
//     resync. The explicit `terminating` predicate admits that transition so
//     finalizer cleanup runs immediately (mirrors
//     pushSecretRelevantChangePredicate).
//   - The operator's own annotation patches (db_job_metrics dedupe annotation)
//     still pass via AnnotationChangedPredicate — rare and harmless.
//   - The 10-minute informer resync on the Keystone CR itself is filtered, but
//     every Owns()/Watches() secondary resource still resyncs and enqueues the
//     owner, so the drift-repair net is preserved.
//   - A future feature that must reconcile on CR *status* written by another
//     actor would be filtered by this predicate; that is an intentional part of
//     the contract.
func keystoneCRPredicate() predicate.Predicate {
	// DeletionTimestamp presence is compared via `== nil`/`!= nil` on the
	// returned *metav1.Time rather than `.IsZero()` so the check is obviously
	// nil-safe, matching pushSecretRelevantChangePredicate.
	terminating := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return false
			}
			return e.ObjectOld.GetDeletionTimestamp() == nil &&
				e.ObjectNew.GetDeletionTimestamp() != nil
		},
	}
	return predicate.Or(
		predicate.GenerationChangedPredicate{},
		predicate.LabelChangedPredicate{},
		predicate.AnnotationChangedPredicate{},
		terminating,
	)
}

// SetupWithManager registers the KeystoneReconciler with the controller manager.
func (r *KeystoneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Detect whether the Gateway API CRD is installed. spec.gateway is
	// optional, so the operator must run on clusters without
	// Gateway API. Adding Owns(HTTPRoute) unconditionally would cause the
	// controller to fail at Start with "no matches for kind HTTPRoute"
	// when the CRD is missing, preventing every Keystone CR from being
	// reconciled — including those that do not use spec.gateway.
	r.gatewayAPIAvailable = isGatewayAPIAvailable(mgr.GetRESTMapper())
	setupLog := ctrl.Log.WithName("keystone-setup")
	if r.gatewayAPIAvailable {
		setupLog.Info("Gateway API detected; enabling HTTPRoute watch and reconciliation")
	} else {
		setupLog.Info("Gateway API not installed; HTTPRoute watch disabled, spec.gateway will be rejected via HTTPRouteReady condition")
	}

	// Detect cert-manager so the operator can Owns(Certificate) — surfacing
	// later DB-client Certificate issuance failures in DatabaseTLSReady — and
	// so reconcileDatabaseTLS knows whether a managed Certificate can exist on
	// the TLS-disable path. spec.database.tls is optional, so the operator must
	// run on clusters without cert-manager (issue #475).
	r.certManagerAvailable = isCertManagerAvailable(mgr.GetRESTMapper())
	if r.certManagerAvailable {
		setupLog.Info("cert-manager detected; enabling Certificate watch for DatabaseTLSReady")
	} else {
		setupLog.Info("cert-manager not installed; Certificate watch disabled, managed DB-TLS Certificates will not be reconciled")
	}

	// Register the Keystone field indexer before Watches so
	// secretToKeystoneMapper can rely on it for its MatchingFields lookup
	if err := registerSecretNameIndex(context.Background(), mgr.GetFieldIndexer()); err != nil {
		return err
	}

	b := ctrl.NewControllerManagedBy(mgr).
		// MaxConcurrentReconciles lets independent CRs reconcile in parallel
		// instead of serialising at the controller-runtime default of 1; the
		// tuned RateLimiter caps per-item failure backoff at 30s rather than the
		// default 1000s (see keystoneRateLimiter).
		WithOptions(crcontroller.Options{
			MaxConcurrentReconciles: r.effectiveMaxConcurrentReconciles(),
			RateLimiter:             keystoneRateLimiter(),
		}).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see keystoneCRPredicate).
		For(&keystonev1alpha1.Keystone{}, builder.WithPredicates(keystoneCRPredicate())).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&batchv1.Job{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&batchv1.CronJob{})

	if r.gatewayAPIAvailable {
		b = b.Owns(&gatewayv1.HTTPRoute{})
	}

	if r.certManagerAvailable {
		b = b.Owns(&certmanagerv1.Certificate{})
	}

	return b.
		// Watch Secrets and map to the Keystone CRs that reference them.
		// ESO-managed secrets (spec.database.secretRef, spec.bootstrap.adminPasswordSecretRef)
		// are owned by the ExternalSecret controller, not by the Keystone CR, so
		// EnqueueRequestForOwner would never match them. This MapFunc performs a
		// namespace-scoped lookup instead.
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(
			secretToKeystoneMapper(mgr.GetClient()),
		)).
		// Watch the MariaDB cluster CR referenced by spec.database.clusterRef so
		// that the operator reflects upstream database outages in DatabaseReady
		// without waiting for the next periodic requeue.
		Watches(&mariadbv1alpha1.MariaDB{}, handler.EnqueueRequestsFromMapFunc(
			mariaDBToKeystoneMapper(mgr.GetClient()),
		)).
		// Watch the OpenBao-backed ClusterSecretStore so the operator reflects
		// upstream secret-backend outages in SecretsReady as soon as ESO flips
		// the store's Ready condition, rather than waiting for the next periodic
		// requeue.
		Watches(&esov1.ClusterSecretStore{}, handler.EnqueueRequestsFromMapFunc(
			clusterSecretStoreToKeystoneMapper(mgr.GetClient()),
		)).
		// Watch backup PushSecrets via a name-based mapper + predicate instead
		// of Owns(). Owns() wakes Keystone on every status-only PushSecret tick
		// ESO emits (SyncedResourceVersion, conditions); the explicit Watches
		// lets pushSecretRelevantChangePredicate suppress those and admit only
		// transitions that affect Pass-0 adoption (esoPushSecretFinalizer added)
		// or Pass-1 deletion (finalizer set churn, DeletionTimestamp flip). This
		// cuts the openbao-finalizer adoption-wait latency from up to
		// RequeueSecretPolling (15s) to watch-event delivery latency while
		// avoiding the duplicate-enqueue churn of the prior Owns() wiring
		Watches(
			&esov1alpha1.PushSecret{},
			handler.EnqueueRequestsFromMapFunc(pushSecretToKeystoneMapper(mgr.GetClient())),
			builder.WithPredicates(pushSecretRelevantChangePredicate),
		).
		Complete(r)
}
