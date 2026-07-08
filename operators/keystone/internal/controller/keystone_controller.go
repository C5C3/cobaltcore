// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the Keystone reconciler.
package controller

import (
	"context"
	"fmt"
	"sync"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/forge/internal/common/bootstrap"
	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/gateway"
	"github.com/c5c3/forge/internal/common/healthcheck"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
	"github.com/c5c3/forge/internal/common/watch"
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
	return watch.RegisterSecretNameIndex(ctx, indexer, &keystonev1alpha1.Keystone{}, KeystoneSecretNameIndexKey, keystoneSecretNameExtractor)
}

// IdentityBackendKeystoneRefIndexKey is the field-indexer key under which
// KeystoneIdentityBackend CRs are indexed by spec.keystoneRef.name. Used by
// reconcileIdentityBackends (list the backends attached to one Keystone) and
// keystoneToIdentityBackendsMapper (fan a Keystone event out to its
// backends).
const IdentityBackendKeystoneRefIndexKey = "spec.keystoneRef.name"

// IdentityBackendSecretNameIndexKey is the field-indexer key under which
// KeystoneIdentityBackend CRs are indexed by the union of their referenced
// Secret names (bind credentials + optional TLS CA bundle). Used by the
// Secret mapper so bind/CA Secret rotation re-renders the content-hashed
// domains Secret via the owning Keystone.
// #nosec G101 -- field-indexer key (a JSONPath-like field selector), not a credential.
const IdentityBackendSecretNameIndexKey = "spec.secretRefs.name"

// identityBackendSecretNameExtractor returns the deduplicated, non-empty
// union of Secret names a KeystoneIdentityBackend references — the LDAP bind
// credentials Secret (plus, when TLS is configured, the CA bundle Secret)
// and the OIDC client secret. Including the client secret means a rotated
// relying-party credential re-renders the content-hashed federation Secret
// via the owning Keystone.
func identityBackendSecretNameExtractor(obj client.Object) []string {
	b, ok := obj.(*keystonev1alpha1.KeystoneIdentityBackend)
	if !ok {
		return nil
	}
	seen := make(map[string]struct{}, 2)
	var names []string
	add := func(name string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	if b.Spec.LDAP != nil {
		add(b.Spec.LDAP.BindCredentialsSecretRef.Name)
		if b.Spec.LDAP.TLS != nil {
			add(b.Spec.LDAP.TLS.CABundleSecretRef.Name)
		}
	}
	if b.Spec.OIDC != nil {
		add(b.Spec.OIDC.ClientSecretRef.Name)
	}
	return names
}

// identityBackendKeystoneRefExtractor is the controller-runtime IndexerFunc
// registered under IdentityBackendKeystoneRefIndexKey: it maps a backend to
// its spec.keystoneRef.name so an attached-backends list is an O(1) indexed
// lookup. Exported to tests so fake clients can register the identical index.
func identityBackendKeystoneRefExtractor(obj client.Object) []string {
	b, ok := obj.(*keystonev1alpha1.KeystoneIdentityBackend)
	if !ok || b.Spec.KeystoneRef.Name == "" {
		return nil
	}
	return []string{b.Spec.KeystoneRef.Name}
}

// registerIdentityBackendIndexes registers the two KeystoneIdentityBackend
// field indexers. It lives beside registerSecretNameIndex so index
// registration has a single site: KeystoneReconciler.SetupWithManager runs
// before KeystoneIdentityBackendReconciler.SetupWithManager in main.go (and
// in the envtest helper), so both controllers can rely on the indexes.
func registerIdentityBackendIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &keystonev1alpha1.KeystoneIdentityBackend{}, IdentityBackendKeystoneRefIndexKey,
		identityBackendKeystoneRefExtractor); err != nil {
		return fmt.Errorf("registering field indexer %q: %w", IdentityBackendKeystoneRefIndexKey, err)
	}
	if err := indexer.IndexField(ctx, &keystonev1alpha1.KeystoneIdentityBackend{}, IdentityBackendSecretNameIndexKey,
		identityBackendSecretNameExtractor); err != nil {
		return fmt.Errorf("registering field indexer %q: %w", IdentityBackendSecretNameIndexKey, err)
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
	// IdentityBackendsReady gates the aggregate Ready: while an attached
	// backend is pending projection the Keystone CR reports Ready=False.
	// Zero-backend clusters are unaffected (IdentityBackendsNotRequired).
	conditionTypeIdentityBackendsReady,
}

// KeystoneReconciler reconciles a Keystone object.
type KeystoneReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   record.EventRecorder
	HTTPClient HTTPDoer

	// OperatorNamespace is the Namespace the operator Pod runs in (resolved at
	// startup by bootstrap.DetectOperatorNamespace). reconcileNetworkPolicy appends an
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
	// bootstrap.DefaultMaxConcurrentReconciles inside
	// bootstrap.ControllerOptions, so the zero value is safe for
	// programmatically constructed reconcilers.
	MaxConcurrentReconciles int

	// healthProbeCache memoizes the last successful Keystone API health probe
	// per CR (shared TTL probe cache) so a steady-state reconcile does not
	// fire a synchronous HTTP GET (bounded by HealthCheckTimeout) on every
	// pass. A cache hit re-upserts KeystoneAPIReady=True without probing; any
	// probe error or non-2xx evicts the entry. The cache's internal mutex
	// guards concurrent access now that MaxConcurrentReconciles may exceed 1;
	// tests inject a controllable clock via its Now field.
	healthProbeCache healthcheck.ProbeCache

	// configRenderCache memoizes the rendered config ConfigMap name per CR,
	// keyed on the CR UID + generation + the referenced policy ConfigMap's
	// ResourceVersion, so a steady-state reconcile returns the known name
	// without re-running RenderINI/RenderPastePipelineINI/RenderPolicyYAML. The
	// ConfigMap is content-addressed and immutable, so nothing else can change
	// its content between passes. Lazily initialised under configRenderCacheMu,
	// which also guards concurrent access under MaxConcurrentReconciles > 1.
	configRenderCache   map[types.NamespacedName]configRenderCacheEntry
	configRenderCacheMu sync.Mutex

	// federationMetadataCache memoizes fetched OIDC discovery documents per
	// KeystoneIdentityBackend, keyed on the backend's (uid, generation), so
	// the steady-state reconcile cadence never hammers the identity provider.
	// Lazily initialised under federationMetadataCacheMu.
	federationMetadataCache   map[types.NamespacedName]federationMetadataCacheEntry
	federationMetadataCacheMu sync.Mutex
}

// httpRouteGVK identifies the HTTPRoute kind the operator would watch when
// Gateway API is installed. Availability is probed at setup time via the
// shared gateway.IsGVKAvailable RESTMapper probe.
var httpRouteGVK = schema.GroupVersionKind{
	Group:   gatewayv1.GroupVersion.Group,
	Version: gatewayv1.GroupVersion.Version,
	Kind:    "HTTPRoute",
}

// certificateGVK identifies the cert-manager Certificate kind the operator
// owns when cert-manager is installed. Availability is probed at setup time
// via the shared gateway.IsGVKAvailable RESTMapper probe (issue #475).
var certificateGVK = schema.GroupVersionKind{
	Group:   certmanagerv1.SchemeGroupVersion.Group,
	Version: certmanagerv1.SchemeGroupVersion.Version,
	Kind:    "Certificate",
}

// +kubebuilder:rbac:groups=keystone.openstack.c5c3.io,resources=keystones,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keystone.openstack.c5c3.io,resources=keystones/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=keystone.openstack.c5c3.io,resources=keystones/finalizers,verbs=update
// +kubebuilder:rbac:groups=keystone.openstack.c5c3.io,resources=keystoneidentitybackends,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=keystone.openstack.c5c3.io,resources=keystoneidentitybackends/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=keystone.openstack.c5c3.io,resources=keystoneidentitybackends/finalizers,verbs=update
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
	if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &keystone, keystoneFinalizer); err != nil {
		return ctrl.Result{}, err
	} else if added {
		return ctrl.Result{Requeue: true}, nil
	}

	// Ensure the OpenBao finalizer is installed so that deleting the Keystone
	// CR blocks on cleanup of the fernet-keys-backup and credential-keys-backup
	// PushSecrets, which ESO then uses to purge the kv-v2 paths in OpenBao
	if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &keystone, keystoneOpenBaoFinalizer); err != nil {
		return ctrl.Result{}, err
	} else if added {
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
	// domainsSecretName is the content-hashed per-domain config Secret
	// materialised by reconcileIdentityBackends ("" when no backend is
	// projected). It is threaded to reconcileConfig (flips the [identity]
	// domain-specific-driver options) and to every workload builder (mounts
	// /etc/keystone/domains). IdentityBackends runs before Config in this
	// pipeline, so the value is populated by the time those steps read it.
	var domainsSecretName string
	// federation is the mod_auth_openidc projection materialised by
	// reconcileIdentityBackends (nil when no OIDC backend is projected). It
	// drives the federation sections in the rendered keystone.conf, the
	// sidecar container/Service/NetworkPolicy shape, and the federation
	// Secret pruning.
	var federation *federationProjection
	pipeline := []commonreconcile.Step{
		{Name: "Secrets", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileSecrets(ctx, &keystone)
		}},
		// reconcileDatabaseTLS provisions the client certificate Keystone
		// presents to MariaDB/MaxScale for mutual TLS. It runs after Secrets (the
		// CA/client-cert material referenced by spec.database.tls must be synced
		// first) and before DBConnectionSecret (which appends the ssl_* DSN
		// parameters pointing at the issued client keypair).
		{Name: "DatabaseTLS", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDatabaseTLS(ctx, &keystone)
		}},
		// reconcileDBConnectionSecret materialises the DB URL into the derived
		// <keystone.Name>-db-connection Secret. It runs after Secrets (upstream
		// credentials must be synced) and before Config (the derived Secret is
		// consumed by downstream pods/Jobs). Failures set SecretsReady=False —
		// the same condition used by reconcileSecrets.
		{Name: "DBConnectionSecret", Fn: func(ctx context.Context) (ctrl.Result, error) {
			var (
				res ctrl.Result
				err error
			)
			res, dbConnectionHash, err = r.reconcileDBConnectionSecret(ctx, &keystone)
			return res, err
		}},
		// reconcileIdentityBackends aggregates the attached, DomainReady
		// KeystoneIdentityBackends into the content-hashed domains Secret. It
		// runs before Config because the projected-state flag flips the
		// [identity] domain-specific-driver options in the rendered
		// keystone.conf. Waiting states (pending domains, missing bind
		// Secrets) NEVER short-circuit the pipeline — the step returns a zero
		// result so first-install can bring the API up, and backend status
		// flips re-enqueue this Keystone via the backend watch.
		{Name: "IdentityBackends", Fn: func(ctx context.Context) (ctrl.Result, error) {
			projection, err := r.reconcileIdentityBackends(ctx, &keystone)
			domainsSecretName = projection.DomainsSecretName
			federation = projection.Federation
			return ctrl.Result{}, err
		}},
		// reconcileConfig must run before the Fernet/credential CronJobs and the
		// db_sync Job, which all require the keystone.conf ConfigMap. It returns
		// (string, error) rather than the standard (ctrl.Result, error): the
		// wrapper captures the ConfigMap name via closure and, on failure, flips
		// SecretsReady=False via markConfigFailed so the aggregate Ready cannot
		// stay stale-True at the new generation (issue #467).
		{Name: "Config", Fn: func(ctx context.Context) (ctrl.Result, error) {
			var err error
			configMapName, err = r.reconcileConfig(ctx, &keystone, domainsSecretName != "", federation)
			if err != nil {
				markConfigFailed(&keystone, err)
			}
			return ctrl.Result{}, err
		}},
		// FernetKeys, CredentialKeys, and NetworkPolicy are independent of each
		// other and run concurrently. All three depend on Config having
		// completed. NetworkPolicy has no data dependency on the Deployment — it
		// uses selectorLabels derived from the CR plus the federation
		// projection (ingress target port + IdP egress ports), which
		// IdentityBackends populated earlier in the pipeline. The group's
		// members self-instrument, so the step carries no sub_reconciler name
		{Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileParallelGroup(ctx, &keystone, []commonreconcile.ParallelStep[*keystonev1alpha1.Keystone]{
				{
					Name:          "FernetKeys",
					ConditionType: "FernetKeysReady",
					Fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
						return r.reconcileFernetKeys(ctx, ks, configMapName, domainsSecretName)
					},
				},
				{
					Name:          "CredentialKeys",
					ConditionType: "CredentialKeysReady",
					Fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
						return r.reconcileCredentialKeys(ctx, ks, configMapName, domainsSecretName)
					},
				},
				{
					Name:          "NetworkPolicy",
					ConditionType: "NetworkPolicyReady",
					Fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
						return r.reconcileNetworkPolicy(ctx, ks, federation)
					},
				},
			})
		}},
		{Name: "Database", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDatabase(ctx, &keystone, configMapName, domainsSecretName)
		}},
		// Policy validation gates the Deployment: invalid oslo.policy overrides
		// must be caught before reaching running pods.
		{Name: "PolicyValidation", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcilePolicyValidation(ctx, &keystone, configMapName, domainsSecretName)
		}},
		{Name: "Deployment", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDeployment(ctx, &keystone, configMapName, dbConnectionHash, domainsSecretName, federation)
		}},
		// Prune stale immutable ConfigMaps and domains Secrets after
		// Deployment is ready so all pods run the new config before old
		// artefacts are deleted. Uninstrumented (no sub_reconciler name); a
		// prune failure is a config-concern failure, so it flips
		// SecretsReady=False via markConfigFailed rather than leaving the
		// aggregate Ready stale-True (issue #467).
		{Fn: func(ctx context.Context) (ctrl.Result, error) {
			if err := r.pruneStaleConfigMaps(ctx, &keystone, configMapName); err != nil {
				markConfigFailed(&keystone, err)
				return ctrl.Result{}, err
			}
			if err := r.pruneStaleDomainsSecrets(ctx, &keystone, domainsSecretName); err != nil {
				markConfigFailed(&keystone, err)
				return ctrl.Result{}, err
			}
			var federationSecretName string
			if federation != nil {
				federationSecretName = federation.SecretName
			}
			if err := r.pruneStaleFederationSecrets(ctx, &keystone, federationSecretName); err != nil {
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
		{Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileParallelGroup(ctx, &keystone, []commonreconcile.ParallelStep[*keystonev1alpha1.Keystone]{
				{
					Name:          "HTTPRoute",
					ConditionType: conditionTypeHTTPRouteReady,
					Fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
						return r.reconcileHTTPRoute(ctx, ks)
					},
				},
				{
					Name:          "HealthCheck",
					ConditionType: conditionTypeKeystoneAPIReady,
					Fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
						return r.reconcileHealthCheck(ctx, ks)
					},
				},
				{
					Name:          "HPA",
					ConditionType: "HPAReady",
					Fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
						return r.reconcileHPA(ctx, ks)
					},
				},
				{
					Name:          "Bootstrap",
					ConditionType: "BootstrapReady",
					Fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
						return r.reconcileBootstrap(ctx, ks, configMapName, domainsSecretName)
					},
				},
				{
					Name:          "TrustFlush",
					ConditionType: "TrustFlushReady",
					Fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
						return r.reconcileTrustFlush(ctx, ks, configMapName, domainsSecretName)
					},
				},
			})
		}},
		// Scheduled admin-password rotation (Model B). Runs after Bootstrap has
		// seeded the initial admin credential so the rotation CronJob and
		// PushSecret never race the bootstrap seed; configMapName is accepted for
		// call-site symmetry only — the rotate script needs no keystone config
		{Name: "PasswordRotation", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcilePasswordRotation(ctx, &keystone, configMapName)
		}},
	}

	// commonreconcile.RunPipeline short-circuits on the first non-zero result
	// or error; both the short-circuit and the fully-successful chain funnel
	// through updateStatus, which recomputes the aggregate Ready condition.
	result, err := commonreconcile.RunPipeline(ctx, instrumentSubReconciler, pipeline)
	return r.updateStatus(ctx, &keystone, statusBefore, result, err)
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

// updateStatus persists the current status conditions and returns the given
// result and error, delegating to commonreconcile.UpdateStatus: the write is
// skipped when the pass left status semantically unchanged from the
// statusBefore snapshot (issue #361), and a failed write is joined with
// reconcileErr so the original reconcile failure stays visible. The mutate
// hook re-aggregates the Ready condition on every persist — including the
// early-return paths a sub-reconciler takes when it degrades after the CR was
// already Ready (SC-CHAOS-006) — and stamps status.observedGeneration so a
// stale status is distinguishable from one reflecting the current spec.
func (r *KeystoneReconciler) updateStatus(ctx context.Context, keystone *keystonev1alpha1.Keystone, statusBefore *keystonev1alpha1.KeystoneStatus, result ctrl.Result, reconcileErr error) (ctrl.Result, error) {
	return commonreconcile.UpdateStatus(ctx, r.Client, keystone, statusBefore, &keystone.Status, func() {
		setReadyCondition(keystone)
		keystone.Status.ObservedGeneration = keystone.Generation
	}, result, reconcileErr)
}

// setReadyCondition sets the aggregate Ready condition based on all
// sub-conditions, delegating to the shared aggregation helper with keystone's
// sub-condition vocabulary.
func setReadyCondition(keystone *keystonev1alpha1.Keystone) {
	commonreconcile.SetAggregateReady(&keystone.Status.Conditions, keystone.Generation, subConditionTypes)
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

// reconcileParallelGroup runs the given sub-reconcilers concurrently,
// delegating to commonreconcile.RunParallelGroup: each member operates on its
// own DeepCopy of the Keystone CR, conditions from every member — including
// those that succeeded before a peer failed — are merged back into the
// primary keystone, and on success the shortest non-zero RequeueAfter is
// returned. Members instrument individually via instrumentSubReconciler.
func (r *KeystoneReconciler) reconcileParallelGroup(
	ctx context.Context,
	keystone *keystonev1alpha1.Keystone,
	subs []commonreconcile.ParallelStep[*keystonev1alpha1.Keystone],
) (ctrl.Result, error) {
	return commonreconcile.RunParallelGroup(ctx, keystone,
		func(ks *keystonev1alpha1.Keystone) *[]metav1.Condition { return &ks.Status.Conditions },
		instrumentSubReconciler, subs)
}

// SetupWithManager registers the KeystoneReconciler with the controller manager.
func (r *KeystoneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Detect whether the Gateway API CRD is installed. spec.gateway is
	// optional, so the operator must run on clusters without
	// Gateway API. Adding Owns(HTTPRoute) unconditionally would cause the
	// controller to fail at Start with "no matches for kind HTTPRoute"
	// when the CRD is missing, preventing every Keystone CR from being
	// reconciled — including those that do not use spec.gateway.
	r.gatewayAPIAvailable = gateway.IsGVKAvailable(mgr.GetRESTMapper(), httpRouteGVK)
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
	r.certManagerAvailable = gateway.IsGVKAvailable(mgr.GetRESTMapper(), certificateGVK)
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

	// Register the KeystoneIdentityBackend indexes here — the single
	// registration site for both controllers (this reconciler is set up
	// before KeystoneIdentityBackendReconciler in main.go and in the envtest
	// helper). reconcileIdentityBackends and the mappers rely on them.
	if err := registerIdentityBackendIndexes(context.Background(), mgr.GetFieldIndexer()); err != nil {
		return err
	}

	b := ctrl.NewControllerManagedBy(mgr).
		// Shared controller options: MaxConcurrentReconciles lets independent
		// CRs reconcile in parallel instead of serialising at the
		// controller-runtime default of 1, and the tuned RateLimiter caps
		// per-item failure backoff at 30s rather than the default 1000s (see
		// bootstrap.ControllerOptions).
		WithOptions(bootstrap.ControllerOptions(r.MaxConcurrentReconciles)).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate).
		For(&keystonev1alpha1.Keystone{}, builder.WithPredicates(watch.CRUpdatePredicate())).
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
		// namespace-scoped lookup instead. The identity-backend leg additionally
		// maps LDAP bind/CA Secrets to the attached Keystone so a rotated bind
		// credential re-renders the content-hashed domains Secret.
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(
			secretToKeystoneWithBackendsMapper(mgr.GetClient()),
		)).
		// Watch KeystoneIdentityBackends and map to their Keystone: backend
		// status flips (DomainReady) trigger projection, DeletionTimestamp
		// flips trigger de-projection. No generation predicate — the status
		// transitions ARE the signal.
		Watches(&keystonev1alpha1.KeystoneIdentityBackend{}, handler.EnqueueRequestsFromMapFunc(
			identityBackendToKeystoneMapper(),
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
