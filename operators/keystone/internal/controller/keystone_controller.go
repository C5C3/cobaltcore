// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the Keystone reconciler.
package controller

import (
	"context"
	"fmt"
	"net"
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
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
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
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/keystone/internal/metrics"
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
// credentials Secret (plus, when TLS is configured, the CA bundle Secret),
// the OIDC client secret, and the SAML IdP-metadata / SP-certificate Secrets.
// Including these means a rotated credential or refreshed metadata re-renders
// the content-hashed federation Secret via the owning Keystone.
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
	if b.Spec.SAML != nil {
		if b.Spec.SAML.IdPMetadata.SecretRef != nil {
			add(b.Spec.SAML.IdPMetadata.SecretRef.Name)
		}
		if b.Spec.SAML.SP != nil && b.Spec.SAML.SP.CertificateSecretRef != nil {
			add(b.Spec.SAML.SP.CertificateSecretRef.Name)
		}
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

	// Resolver resolves the target cluster a Keystone CR names in
	// spec.targetClusterRef into the client its children are read and written
	// with. Nil means always-local: every CR keeps its children on the
	// management cluster, which is what single-cluster tests and deployments
	// want.
	Resolver commonmulticluster.ClusterResolver

	// FederationMetadataAllowCIDRs is threaded from the
	// --federation-metadata-allow-cidrs binary flag: the CIDRs the
	// federation-metadata dial guard additionally permits for OIDC discovery /
	// SAML IdP-metadata fetches (e.g. the cluster's Service CIDR for a trusted
	// in-cluster IdP). nil keeps the hardened default (all non-public addresses
	// blocked). Deliberately operator-scoped configuration, never derived from
	// CR fields.
	FederationMetadataAllowCIDRs []*net.IPNet

	// OperatorNamespace is the Namespace the operator Pod runs in (resolved at
	// startup by bootstrap.DetectOperatorNamespace). reconcileNetworkPolicy appends an
	// ingress peer for this Namespace so the operator's own health check can
	// reach the Keystone API on TCP 5000. Empty when the namespace could not be
	// determined, in which case no operator-namespace peer is added.
	OperatorNamespace string

	// gatewayAPIAvailable is set during SetupWithManager from the
	// management cluster's RESTMapper and indicates whether the
	// gateway.networking.k8s.io/v1 HTTPRoute CRD is installed there. Two
	// consumers read it: the local HTTPRoute watch leg, which
	// setupWithOptions skips when false so the controller does not crash on
	// a missing kind, and commonmulticluster.ChildrenServeKind, which
	// answers with it for local children while probing the target cluster's
	// RESTMapper for remote ones. reconcileHTTPRoute takes that verdict and
	// surfaces a clear HTTPRouteReady=False condition if the user
	// nonetheless sets spec.gateway.
	gatewayAPIAvailable bool

	// apiReader is set during SetupWithManager from mgr.GetAPIReader(): a
	// direct, uncached reader. reconcileDeployment latches the two-phase
	// narrowing of the API Service selector on the live Service, and the
	// informer cache can still hold the pre-narrowing state right after the
	// narrowing write — a latch decided from it could widen the selector back.
	// Nil in unit tests that construct the reconciler without a manager; those
	// fall back to the (read-your-writes) fake client.
	apiReader client.Reader

	// certManagerAvailable is set during SetupWithManager from the
	// management cluster's RESTMapper and indicates whether the
	// cert-manager.io/v1 Certificate CRD is installed there. Two consumers
	// read it: the local Certificate watch leg, which setupWithOptions
	// skips when false, and commonmulticluster.ChildrenServeKind, which
	// answers with it for local children while probing the target cluster's
	// RESTMapper for remote ones. deleteManagedDBClientCertificate takes
	// that verdict and skips the disable-path Certificate delete where no
	// Certificate can exist, so the default no-TLS configuration never
	// errors with "no matches for kind Certificate" on a cluster without
	// cert-manager (issue #475).
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
	// fire a synchronous HTTP GET (bounded by healthcheck.HealthCheckTimeout)
	// on every pass. A cache hit re-upserts KeystoneAPIReady=True without
	// probing; any probe error or non-2xx evicts the entry. The cache's
	// internal mutex guards concurrent access now that MaxConcurrentReconciles
	// may exceed 1; tests inject a controllable clock via its Now field.
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

// KeystoneRemoteChildKinds are the kinds a Keystone CR projects into the
// namespace of the target cluster it names, and the kinds
// reconcileDeleteRemoteChildren sweeps by ownership label when that CR is
// deleted. Nothing on the target cluster collects them, so a kind missing from
// this list is a kind that keeps running after its CR is gone.
//
// The list is cross-checked against the create verbs of the kubebuilder RBAC
// markers on this controller below: the operator can only leave behind what it
// is allowed to create. ExternalSecret is the one create verb deliberately not
// swept: the marker grants it, but no code composes an ExternalSecret, so no
// Keystone owns one.
var KeystoneRemoteChildKinds = []schema.GroupVersionKind{
	appsv1.SchemeGroupVersion.WithKind("Deployment"),
	corev1.SchemeGroupVersion.WithKind("Service"),
	corev1.SchemeGroupVersion.WithKind("ConfigMap"),
	corev1.SchemeGroupVersion.WithKind("Secret"),
	corev1.SchemeGroupVersion.WithKind("ServiceAccount"),
	rbacv1.SchemeGroupVersion.WithKind("Role"),
	rbacv1.SchemeGroupVersion.WithKind("RoleBinding"),
	batchv1.SchemeGroupVersion.WithKind("Job"),
	batchv1.SchemeGroupVersion.WithKind("CronJob"),
	policyv1.SchemeGroupVersion.WithKind("PodDisruptionBudget"),
	autoscalingv2.SchemeGroupVersion.WithKind("HorizontalPodAutoscaler"),
	networkingv1.SchemeGroupVersion.WithKind("NetworkPolicy"),
	httpRouteGVK,
	certificateGVK,
	esov1alpha1.SchemeGroupVersion.WithKind("PushSecret"),
	mariadbv1alpha1.GroupVersion.WithKind("Database"),
	mariadbv1alpha1.GroupVersion.WithKind("User"),
	mariadbv1alpha1.GroupVersion.WithKind("Grant"),
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
// +kubebuilder:rbac:groups=external-secrets.io,resources=clustersecretstores;secretstores,verbs=get;list;watch
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
	//
	// The deletion branch comes before the target-cluster resolution below, and
	// resolves through the deletion variant, which never fails the pass: a CR
	// whose cluster was deregistered after the finalizers went on would
	// otherwise short-circuit on the unresolvable ref on every pass and stay
	// Terminating forever. A target that has not resolved yet requeues instead
	// of being given up on — engagement is asynchronous, so an operator restart
	// looks exactly like a deregistration until the provider has synced.
	if !keystone.DeletionTimestamp.IsZero() {
		children, wait := commonmulticluster.ResolveChildrenClientForDeletion(
			ctx, r.Resolver, r.Client, keystone.Spec.TargetClusterRef, *keystone.DeletionTimestamp)
		if wait {
			// The hold goes on the CR, not only into the operator's log. It is a
			// deliberate state a CR can sit in for minutes, and "Terminating,
			// waiting on the target cluster" has to be distinguishable from a
			// wedged finalizer without correlating logs across replicas.
			keystoneSkeleton.MarkFailed(&keystone, "SecretsReady",
				commonmulticluster.TargetClusterUnavailable,
				fmt.Errorf("target cluster %s does not resolve; waiting at least %s before abandoning its children",
					keystone.Spec.TargetClusterRef.Name, commonmulticluster.AbandonAfter))
			return r.updateStatus(ctx, &keystone, statusBefore,
				ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil)
		}
		if result, err := r.reconcileDelete(ctx, children, &keystone); !result.IsZero() || err != nil {
			return result, err
		}
		if result, err := r.reconcileDeleteOpenBao(ctx, children, &keystone); !result.IsZero() || err != nil {
			return r.updateStatus(ctx, &keystone, statusBefore, result, err)
		}
		// The label-selected sweep runs after the two named cleanup flows, never
		// before them: it would delete the backup PushSecrets out from under the
		// ESO purge the OpenBao flow waits for. The ordering mirrors the local
		// one, where the garbage collection cascade starts only once every
		// finalizer has been released.
		if err := r.reconcileDeleteRemoteChildren(ctx, children, &keystone); err != nil {
			return r.updateStatus(ctx, &keystone, statusBefore, ctrl.Result{}, err)
		}
		return ctrl.Result{}, nil
	}

	// Resolve the client every child object of this CR is read and written
	// with. The embedded client stays on the management cluster (the CR, its
	// status, its finalizers, and the sibling configuration CRs live there);
	// children carries everything the CR projects into the target cluster. The
	// resolution runs before the finalizer is added so a CR naming an
	// unresolvable cluster stays clean of finalizers: nothing was created for
	// it, so there is nothing to clean up, and a finalizer would only block its
	// deletion.
	children, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client, keystone.Spec.TargetClusterRef)
	if err != nil {
		keystoneSkeleton.MarkFailed(&keystone, "SecretsReady", commonmulticluster.TargetClusterUnavailable, err)
		return r.updateStatus(ctx, &keystone, statusBefore,
			ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil)
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

	// The remote-children finalizer goes on only when the CR projects onto a
	// target cluster. A local CR keeps the garbage collection cascade, which
	// reaps its children from their owner references, so it has nothing for this
	// finalizer to hold the CR open for. spec.targetClusterRef is immutable, so
	// the condition cannot flip under a live CR.
	if keystone.Spec.TargetClusterRef != nil {
		if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &keystone,
			commonmulticluster.RemoteChildrenFinalizer); err != nil {
			return ctrl.Result{}, err
		} else if added {
			return ctrl.Result{Requeue: true}, nil
		}
	}

	// Run the sub-reconciler pipeline. Steps are attempted in dependency order;
	// the first to return a non-zero result or an error short-circuits the chain
	// and funnels through updateStatus, so conditions and the requeue/error are
	// persisted by construction on every exit path. Named steps are wrapped in
	// instrumenter.Instrument (emitting duration/error series under their
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
			return r.reconcileSecrets(ctx, children, &keystone)
		}},
		// reconcileDatabaseTLS provisions the client certificate Keystone
		// presents to MariaDB/MaxScale for mutual TLS. It runs after Secrets (the
		// CA/client-cert material referenced by spec.database.tls must be synced
		// first) and before DBConnectionSecret (which appends the ssl_* DSN
		// parameters pointing at the issued client keypair).
		{Name: "DatabaseTLS", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDatabaseTLS(ctx, children, &keystone)
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
			res, dbConnectionHash, err = r.reconcileDBConnectionSecret(ctx, children, &keystone)
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
			projection, err := r.reconcileIdentityBackends(ctx, children, &keystone)
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
			configMapName, err = r.reconcileConfig(ctx, children, &keystone, domainsSecretName != "", federation)
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
						return r.reconcileFernetKeys(ctx, children, ks, configMapName, domainsSecretName)
					},
				},
				{
					Name:          "CredentialKeys",
					ConditionType: "CredentialKeysReady",
					Fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
						return r.reconcileCredentialKeys(ctx, children, ks, configMapName, domainsSecretName)
					},
				},
				{
					Name:          "NetworkPolicy",
					ConditionType: "NetworkPolicyReady",
					Fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
						return r.reconcileNetworkPolicy(ctx, children, ks, federation)
					},
				},
			})
		}},
		{Name: "Database", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDatabase(ctx, children, &keystone, configMapName, domainsSecretName)
		}},
		// Policy validation gates the Deployment: invalid oslo.policy overrides
		// must be caught before reaching running pods.
		{Name: "PolicyValidation", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcilePolicyValidation(ctx, children, &keystone, configMapName, domainsSecretName)
		}},
		{Name: "Deployment", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcileDeployment(ctx, children, &keystone, configMapName, dbConnectionHash, domainsSecretName, federation)
		}},
		// Prune stale immutable ConfigMaps and domains Secrets after
		// Deployment is ready so all pods run the new config before old
		// artefacts are deleted. Uninstrumented (no sub_reconciler name); a
		// prune failure is a config-concern failure, so it flips
		// SecretsReady=False via markConfigFailed rather than leaving the
		// aggregate Ready stale-True (issue #467).
		{Fn: func(ctx context.Context) (ctrl.Result, error) {
			if err := r.pruneStaleConfigMaps(ctx, children, &keystone, configMapName); err != nil {
				markConfigFailed(&keystone, err)
				return ctrl.Result{}, err
			}
			if err := r.pruneStaleDomainsSecrets(ctx, children, &keystone, domainsSecretName); err != nil {
				markConfigFailed(&keystone, err)
				return ctrl.Result{}, err
			}
			var federationSecretName string
			if federation != nil {
				federationSecretName = federation.SecretName
			}
			if err := r.pruneStaleFederationSecrets(ctx, children, &keystone, federationSecretName); err != nil {
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
						return r.reconcileHTTPRoute(ctx, children, ks)
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
						return r.reconcileHPA(ctx, children, ks)
					},
				},
				{
					Name:          "Bootstrap",
					ConditionType: "BootstrapReady",
					Fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
						return r.reconcileBootstrap(ctx, children, ks, configMapName, domainsSecretName)
					},
				},
				{
					Name:          "TrustFlush",
					ConditionType: "TrustFlushReady",
					Fn: func(ctx context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
						return r.reconcileTrustFlush(ctx, children, ks, configMapName, domainsSecretName)
					},
				},
			})
		}},
		// Scheduled admin-password rotation (Model B). Runs after Bootstrap has
		// seeded the initial admin credential so the rotation CronJob and
		// PushSecret never race the bootstrap seed; configMapName is accepted for
		// call-site symmetry only — the rotate script needs no keystone config
		{Name: "PasswordRotation", Fn: func(ctx context.Context) (ctrl.Result, error) {
			return r.reconcilePasswordRotation(ctx, children, &keystone, configMapName)
		}},
	}

	// commonreconcile.RunPipeline short-circuits on the first non-zero result
	// or error; both the short-circuit and the fully-successful chain funnel
	// through updateStatus, which recomputes the aggregate Ready condition.
	result, err := commonreconcile.RunPipeline(ctx, instrumenter.Instrument, pipeline)
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
//
// A nil children client means the target cluster this CR named is no longer
// registered. Its MariaDB CRs cannot be reached, so they are left behind and
// the finalizer is released anyway: holding it would only strand the CR in
// Terminating.
func (r *KeystoneReconciler) reconcileDelete(ctx context.Context, children client.Client, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(keystone, keystoneFinalizer) {
		return ctrl.Result{}, nil
	}

	if children == nil {
		r.Recorder.Event(keystone, corev1.EventTypeWarning, "RemoteChildrenAbandoned",
			"Target cluster is no longer registered; releasing the finalizer without deleting the MariaDB Database, User, and Grant on it")
	} else {
		// Only emit FinalizingDatabase when at least one MariaDB CR is still live
		// so brownfield CRs (no MariaDB CRs ever created) do not produce a
		// misleading "cleaning up" event.
		hasLiveCleanupWork, err := r.hasLiveMariaDBResources(ctx, children, keystone)
		if err != nil {
			return ctrl.Result{}, err
		}
		if hasLiveCleanupWork {
			r.Recorder.Event(keystone, corev1.EventTypeNormal, "FinalizingDatabase",
				"Cleaning up MariaDB Database, User, and Grant before removing Keystone")
		}

		if err := r.finalizeDatabaseResources(ctx, children, keystone); err != nil {
			return ctrl.Result{}, err
		}

		r.Recorder.Event(keystone, corev1.EventTypeNormal, "DatabaseFinalized",
			"MariaDB Database, User, and Grant marked for deletion; releasing finalizer")
	}

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
// is suppressed when there is nothing to announce. It delegates to the shared
// database.HasLiveResources.
func (r *KeystoneReconciler) hasLiveMariaDBResources(ctx context.Context, children client.Client, keystone *keystonev1alpha1.Keystone) (bool, error) {
	return database.HasLiveResources(ctx, children, mariaDBResourceKey(keystone))
}

// reconcileDeleteOpenBao drives the openbao-finalizer cleanup when the Keystone
// CR is being deleted. It is a no-op if the openbao finalizer is absent.
// Otherwise it emits FinalizingOpenBaoSecrets when at least one backup
// PushSecret has been adopted by ESO and is not yet Terminating (dedupes the
// event across requeues because subsequent passes observe the PushSecrets
// gone, Terminating, or still unadopted and suppress the emit), calls
// finalizeOpenBaoSecrets, and on done=true emits OpenBaoSecretsFinalized and
// releases the finalizer. A PushSecret held Terminating by ESO's cleanup
// finalizer surfaces as
// ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling} so the
// Keystone CR stays live until ESO has purged the kv-v2 path — bounded by
// OpenBaoCleanupStallTimeout, past which finalizeOpenBaoSecrets force-releases
// the stuck PushSecrets (see its doc) rather than wedging the CR forever.
func (r *KeystoneReconciler) reconcileDeleteOpenBao(ctx context.Context, children client.Client, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(keystone, keystoneOpenBaoFinalizer) {
		return ctrl.Result{}, nil
	}

	// A nil children client means the target cluster holding the PushSecrets is
	// no longer registered. ESO cannot be waited on across a cluster that
	// cannot be reached, so the kv-v2 paths stay as they are and the finalizer
	// is released rather than blocking the CR forever.
	if children == nil {
		r.Recorder.Event(keystone, corev1.EventTypeWarning, "RemoteChildrenAbandoned",
			"Target cluster is no longer registered; releasing the openbao-finalizer without deleting the backup PushSecrets on it")
		controllerutil.RemoveFinalizer(keystone, keystoneOpenBaoFinalizer)
		if err := r.Update(ctx, keystone); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing openbao finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// Only emit FinalizingOpenBaoSecrets when a backup PushSecret is adopted
	// by ESO and not yet Terminating — subsequent requeues observe the same
	// PushSecret Terminating (DeletionTimestamp set), absent, or still
	// unadopted and suppress the emit, giving exactly-once semantics per
	// termination. Gating on ESO adoption is what preserves the exactly-once
	// contract across the Pass-0 adoption-wait window; without the gate, the
	// 15s commonreconcile.RequeueSecretPolling tick would fire a fresh
	// FinalizingOpenBaoSecrets event on every requeue until ESO adopts
	hasLiveCleanupWork, err := r.hasLiveOpenBaoBackupPushSecrets(ctx, children, keystone)
	if err != nil {
		return ctrl.Result{}, err
	}
	if hasLiveCleanupWork {
		r.Recorder.Event(keystone, corev1.EventTypeNormal, "FinalizingOpenBaoSecrets",
			"Cleaning up OpenBao backup PushSecrets before removing Keystone")
	}

	done, err := r.finalizeOpenBaoSecrets(ctx, children, keystone)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
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
//     to announce yet. Without this gate the 15s
//     commonreconcile.RequeueSecretPolling tick would fire a fresh Event on
//     every requeue until ESO adopts, regressing the exactly-once contract.
func (r *KeystoneReconciler) hasLiveOpenBaoBackupPushSecrets(ctx context.Context, children client.Client, keystone *keystonev1alpha1.Keystone) (bool, error) {
	for _, name := range openBaoBackupPushSecretNames(keystone) {
		key := client.ObjectKey{Namespace: keystone.Namespace, Name: name}
		ps := &esov1alpha1.PushSecret{}
		err := children.Get(ctx, key, ps)
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

// reconcileDeleteRemoteChildren deletes everything this Keystone projected onto
// the target cluster it names and releases the remote-children finalizer, as
// commonmulticluster.SweepRemoteChildren documents. The two cleanup flows above
// it delete the handful of objects they track by name; this pass is what reaches
// the rest, selected on the ownership labels Claim stamped on them.
func (r *KeystoneReconciler) reconcileDeleteRemoteChildren(ctx context.Context, children client.Client, keystone *keystonev1alpha1.Keystone) error {
	return commonmulticluster.SweepRemoteChildren(ctx, r.Client, r.Resolver, r.Recorder, r.Scheme,
		keystone, keystone.Spec.TargetClusterRef, children, KeystoneRemoteChildKinds)
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
	return keystoneSkeleton.UpdateStatus(ctx, r.Client, keystone, statusBefore, &keystone.Status, func() {
		keystone.Status.ObservedGeneration = keystone.Generation
	}, result, reconcileErr)
}

// keystoneSkeleton bundles the shared controller-skeleton glue (Ready
// aggregation, no-op-skipping status writes, config-failure marking, and
// parallel-group execution) with keystone's sub-condition vocabulary and status
// accessor. The wrapper methods below delegate to it.
var keystoneSkeleton = commonreconcile.Skeleton[*keystonev1alpha1.Keystone, keystonev1alpha1.KeystoneStatus]{
	SubConditionTypes: subConditionTypes,
	Conditions:        func(ks *keystonev1alpha1.Keystone) *[]metav1.Condition { return &ks.Status.Conditions },
}

// setReadyCondition sets the aggregate Ready condition based on all
// sub-conditions, delegating to the shared skeleton with keystone's
// sub-condition vocabulary.
func setReadyCondition(keystone *keystonev1alpha1.Keystone) {
	keystoneSkeleton.SetReady(keystone)
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
	keystoneSkeleton.MarkFailed(keystone, "SecretsReady", conditionReasonConfigError, err)
}

// reconcileParallelGroup runs the given sub-reconcilers concurrently,
// delegating to commonreconcile.RunParallelGroup: each member operates on its
// own DeepCopy of the Keystone CR, conditions from every member — including
// those that succeeded before a peer failed — are merged back into the
// primary keystone, and on success the shortest non-zero RequeueAfter is
// returned. Members instrument individually via instrumenter.Instrument.
func (r *KeystoneReconciler) reconcileParallelGroup(
	ctx context.Context,
	keystone *keystonev1alpha1.Keystone,
	subs []commonreconcile.ParallelStep[*keystonev1alpha1.Keystone],
) (ctrl.Result, error) {
	return keystoneSkeleton.RunParallelGroup(ctx, keystone, instrumenter.Instrument, subs)
}

// SetupWithManager registers the KeystoneReconciler with the controller
// manager. The shared controller options it applies let independent CRs
// reconcile in parallel instead of serialising at the controller-runtime
// default of 1, and the tuned RateLimiter caps per-item failure backoff at 30s
// rather than the default 1000s (see bootstrap.TypedControllerOptions).
func (r *KeystoneReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return r.setupWithOptions(mgr, bootstrap.TypedControllerOptions[mcreconcile.Request](r.MaxConcurrentReconciles))
}

// setupWithOptions carries the production watch wiring SetupWithManager
// applies. The controller options are a parameter so the dual-envtest
// integration suite can register this exact chain with SkipNameValidation set,
// rather than a hand-built copy of it that drifts the moment a leg is added
// here.
func (r *KeystoneReconciler) setupWithOptions(mgr mcmanager.Manager, opts crcontroller.TypedOptions[mcreconcile.Request]) error {
	local := mgr.GetLocalManager()

	// Detect whether the Gateway API CRD is installed. spec.gateway is
	// optional, so the operator must run on clusters without
	// Gateway API. Adding Owns(HTTPRoute) unconditionally would cause the
	// controller to fail at Start with "no matches for kind HTTPRoute"
	// when the CRD is missing, preventing every Keystone CR from being
	// reconciled — including those that do not use spec.gateway.
	r.gatewayAPIAvailable = gateway.IsGVKAvailable(local.GetRESTMapper(), httpRouteGVK)
	r.apiReader = local.GetAPIReader()
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
	r.certManagerAvailable = gateway.IsGVKAvailable(local.GetRESTMapper(), certificateGVK)
	if r.certManagerAvailable {
		setupLog.Info("cert-manager detected; enabling Certificate watch for DatabaseTLSReady")
	} else {
		setupLog.Info("cert-manager not installed; Certificate watch disabled, managed DB-TLS Certificates will not be reconciled")
	}

	// Register the Keystone field indexer before Watches so
	// secretToKeystoneMapper can rely on it for its MatchingFields lookup.
	// The indexes go on the LOCAL field indexer, not mgr's: with a provider
	// configured, the multicluster manager's field indexer registers against
	// the provider clusters, which hold no Keystone CR. Registration stays
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

	// Register the KeystoneIdentityBackend indexes here — the single
	// registration site for both controllers (this reconciler is set up
	// before KeystoneIdentityBackendReconciler in main.go and in the envtest
	// helper). reconcileIdentityBackends and the mappers rely on them.
	if err := registerIdentityBackendIndexes(context.Background(), local.GetFieldIndexer()); err != nil {
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
		func(keystone *keystonev1alpha1.Keystone) *commonv1.TargetClusterRefSpec {
			return keystone.Spec.TargetClusterRef
		})

	b := mcbuilder.ControllerManagedBy(mgr).
		WithOptions(opts).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate).
		For(&keystonev1alpha1.Keystone{}, mcbuilder.WithPredicates(watch.CRUpdatePredicate()), engageLocal, engageNoProviders).
		Owns(&appsv1.Deployment{}, engageLocal, engageNoProviders).
		Owns(&corev1.Service{}, engageLocal, engageNoProviders).
		Owns(&corev1.ConfigMap{}, engageLocal, engageNoProviders).
		Owns(&batchv1.Job{}, engageLocal, engageNoProviders).
		Owns(&policyv1.PodDisruptionBudget{}, engageLocal, engageNoProviders).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}, engageLocal, engageNoProviders).
		Owns(&networkingv1.NetworkPolicy{}, engageLocal, engageNoProviders).
		Owns(&batchv1.CronJob{}, engageLocal, engageNoProviders)

	if r.gatewayAPIAvailable {
		b = b.Owns(&gatewayv1.HTTPRoute{}, engageLocal, engageNoProviders)
	}

	if r.certManagerAvailable {
		b = b.Owns(&certmanagerv1.Certificate{}, engageLocal, engageNoProviders)
	}

	// The same children, once more, on the clusters a CR can project onto.
	// Owns cannot see them: an owner reference does not cross a cluster
	// boundary, so the ownership labels are what maps a child back to its CR.
	// The PushSecret predicate keeps ESO's status-only ticks on the target from
	// waking the CR, exactly as it does locally; the other remote legs carry no
	// predicate, mirroring what Owns admits locally.
	b, err := commonmulticluster.AddRemoteChildWatches(b, local.GetScheme(), &keystonev1alpha1.Keystone{},
		targets, KeystoneRemoteChildKinds,
		map[schema.GroupVersionKind][]mcbuilder.WatchesOption{
			esov1alpha1.SchemeGroupVersion.WithKind("PushSecret"): {mcbuilder.WithPredicates(pushSecretRelevantChangePredicate)},
		})
	if err != nil {
		return err
	}

	// Watch Secrets and map to the Keystone CRs that reference them.
	// ESO-managed secrets (spec.database.secretRef, spec.bootstrap.adminPasswordSecretRef)
	// are owned by the ExternalSecret controller, not by the Keystone CR, so
	// EnqueueRequestForOwner would never match them. This MapFunc performs a
	// namespace-scoped lookup instead. The identity-backend leg additionally
	// maps LDAP bind/CA Secrets to the attached Keystone so a rotated bind
	// credential re-renders the content-hashed domains Secret.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &corev1.Secret{},
		secretToKeystoneWithBackendsMapper(local.GetClient()))
	if err != nil {
		return err
	}

	// Watch KeystoneIdentityBackends and map to their Keystone: backend
	// status flips (DomainReady) trigger projection, DeletionTimestamp
	// flips trigger de-projection. No generation predicate — the status
	// transitions ARE the signal. The leg stays local-only: a
	// KeystoneIdentityBackend is a management-plane CR and lives nowhere else.
	b = b.Watches(&keystonev1alpha1.KeystoneIdentityBackend{}, commonmulticluster.LocalRequests(
		identityBackendToKeystoneMapper(),
	), engageLocal, engageNoProviders)

	// Watch the MariaDB cluster CR referenced by spec.database.clusterRef so
	// that the operator reflects upstream database outages in DatabaseReady
	// without waiting for the next periodic requeue.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &mariadbv1alpha1.MariaDB{},
		mariaDBToKeystoneMapper(local.GetClient()))
	if err != nil {
		return err
	}

	// Watch both the cluster-scoped ClusterSecretStore and the namespaced
	// SecretStore a Keystone can select via spec.secretStoreRef, so the
	// operator reflects upstream secret-backend outages in SecretsReady as
	// soon as ESO flips the selected store's Ready condition, rather than
	// waiting for the next periodic requeue. Each mapper enqueues only the
	// Keystones whose effective store ref matches the changed store.
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &esov1.ClusterSecretStore{},
		storeToKeystoneMapper(local.GetClient(), commonv1.SecretStoreKindCluster))
	if err != nil {
		return err
	}
	b, err = commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &esov1.SecretStore{},
		storeToKeystoneMapper(local.GetClient(), commonv1.SecretStoreKindNamespaced))
	if err != nil {
		return err
	}

	return b.
		// Watch backup PushSecrets via a name-based mapper + predicate instead
		// of Owns(). Owns() wakes Keystone on every status-only PushSecret tick
		// ESO emits (SyncedResourceVersion, conditions); the explicit Watches
		// lets pushSecretRelevantChangePredicate suppress those and admit only
		// transitions that affect Pass-0 adoption (esoPushSecretFinalizer added)
		// or Pass-1 deletion (finalizer set churn, DeletionTimestamp flip). This
		// cuts the openbao-finalizer adoption-wait latency from up to
		// commonreconcile.RequeueSecretPolling (15s) to watch-event delivery
		// latency while avoiding the duplicate-enqueue churn of the prior
		// Owns() wiring
		Watches(
			&esov1alpha1.PushSecret{},
			commonmulticluster.LocalRequests(pushSecretToKeystoneMapper(local.GetClient())),
			mcbuilder.WithPredicates(pushSecretRelevantChangePredicate),
			engageLocal, engageNoProviders,
		).
		// The default wrapper turns an error matching multicluster.ErrClusterNotFound
		// into a successful reconcile. This operator instead surfaces an
		// unresolvable cluster as a TargetClusterUnavailable condition and
		// requeues, so the wrapper stays off and the error semantics remain
		// byte-identical to the classic builder's.
		WithClusterNotFoundWrapper(false).
		Complete(commonmulticluster.LocalReconciler(r))
}
