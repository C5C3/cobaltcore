// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	openbaov1alpha1 "github.com/dc-tec/openbao-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/barbican/internal/metrics"
	"github.com/c5c3/cobaltcore/operators/barbican/internal/openbao"
)

// Condition types the BarbicanSecretStore controller owns.
//
// CredentialsReady describes the store's own AppRole credentials: the Secret
// exists, carries both contract keys, and the credentials in it are accepted by
// the server. ProvisioningReady describes the instance side of a managed store —
// the OpenBaoCluster is Available, reachable, and carries the KV mount and the
// AppRole the self-init contract provisions — and is never set for a brownfield
// store, which has no instance this operator provisions. ConfigProjected
// observes whether the rendered secret-store section reached the running
// Barbican Deployment; it does not gate the other two.
const (
	conditionTypeCredentialsReady  = "CredentialsReady"
	conditionTypeProvisioningReady = "ProvisioningReady"
	conditionTypeConfigProjected   = "ConfigProjected"
)

// Condition reason constants. Each waiting or failure state carries its own
// reason so the cause is readable off the condition alone, without correlating
// the message against operator logs.
const (
	// CredentialsReady reasons.
	conditionReasonCredentialsAvailable     = "CredentialsAvailable"
	conditionReasonWaitingForCredentials    = "WaitingForCredentials"
	conditionReasonWaitingForParent         = "WaitingForParent"
	conditionReasonInvalidCredentials       = "InvalidCredentials"
	conditionReasonInsufficientCapabilities = "InsufficientCapabilities"

	// ProvisioningReady reasons.
	conditionReasonProvisioned            = "Provisioned"
	conditionReasonInstanceNotFound       = "InstanceNotFound"
	conditionReasonWaitingForInstance     = "WaitingForInstance"
	conditionReasonWaitingForInstanceTLS  = "WaitingForInstanceTLS"
	conditionReasonInstanceNotProvisioned = "InstanceNotProvisioned"
	conditionReasonProvisioningDenied     = "ProvisioningDenied"

	// Shared by both credential conditions: the server did not answer.
	conditionReasonOpenBaoUnreachable = "OpenBaoUnreachable"

	// ConfigProjected reasons.
	conditionReasonConfigProjected      = "ConfigProjected"
	conditionReasonWaitingForProjection = "WaitingForProjection"
)

// The managed-mode naming convention. A managed store names an OpenBaoCluster
// and nothing else; everything the operator needs to talk to that instance is
// derived from the instance name, matching the delivered instance manifest.
const (
	// instanceCASecretSuffix names the CA Secret of an instance, carrying the
	// trust bundle under barbicanv1alpha1.OpenBaoCAKey.
	instanceCASecretSuffix = "-tls-ca"
	// instanceProvisionerSASuffix names the ServiceAccount the operator mints a
	// bound token for. The delivered kubernetes auth role binds exactly this
	// name.
	instanceProvisionerSASuffix = "-provisioner"
	// instanceAPIPort is the port the instance serves its API on.
	instanceAPIPort = 8200
	// provisionerAuthRole is the kubernetes auth role the operator logs in
	// under (auth/kubernetes/role/provisioner in the self-init contract).
	provisionerAuthRole = "provisioner"
	// storeAppRoleName is the AppRole barbican itself authenticates with, and the
	// only one the provisioner policy may hand out credentials for.
	storeAppRoleName = "barbican"
	// selfInitRequestNames are the delivered self-init requests that provision
	// what a managed store needs. The InstanceNotProvisioned message names them
	// so the gap can be matched against the instance manifest directly.
	selfInitRequestNames = "barbican_kv, approle_auth, barbican_approle_role"
)

// approleSecretNameSuffix is appended to the store name to name the credentials
// Secret the operator mints into and the parent Barbican reads the AppRole
// credentials from. It is the api package's constant, not a second copy: the
// validating webhook computes the metadata.name bound from the same symbol, so
// the admitted name and the derived Secret name cannot drift apart.
const approleSecretNameSuffix = barbicanv1alpha1.ApproleSecretNameSuffix

// The annotations stamped onto the credentials Secret. They are the only record
// of when a secret ID was minted and how long it lives: OpenBao hands the secret
// ID out once and never reads it back, so the re-mint timer has to be derived
// from what the operator wrote down at mint time.
const (
	// #nosec G101 -- annotation key naming the mint timestamp, not a credential.
	secretIDMintedAtAnnotation = "barbican.c5c3.io/secret-id-minted-at"
	// #nosec G101 -- annotation key naming the mint TTL, not a credential.
	secretIDTTLSecondsAnnotation = "barbican.c5c3.io/secret-id-ttl-seconds"
)

// childrenClusterAnnotation names the target cluster this store's credentials
// Secret was minted onto. The store itself has no target field: it inherits one
// from its parent Barbican, and the teardown has to reach that cluster after the
// parent may already be gone. Stamping the resolved name on the store is what
// keeps the deletion independent of the parent's own lifetime. Both inputs it is
// derived from are immutable, so the operator never has cause to rewrite what it
// wrote — but it is an ordinary annotation on a user-created CR, so the value is
// reconciled against the resolved name rather than trusted for being present.
const childrenClusterAnnotation = "openstack.c5c3.io/children-cluster"

// The values of the trigger label on the re-mint counter.
const (
	remintTriggerProactive = "proactive"
	remintTriggerReactive  = "reactive"
)

// openBaoRequestTimeout bounds every OpenBao call a single reconcile makes. A
// server that accepts the connection and then stalls must not hold a worker.
const openBaoRequestTimeout = 10 * time.Second

// reactiveRemintCooldown is the minimum age the credentials in hand must have
// before a server-side rejection re-mints them. It caps the rate at which the
// reactive path can hand out fresh secret IDs, so a rejection that a new secret
// ID does not cure costs one mint per cooldown instead of one per retry. It is
// well below the shortest secret-ID TTL worth configuring, so a genuinely
// revoked credential is still replaced promptly.
const reactiveRemintCooldown = 10 * time.Minute

// provisionerTokenTTL is the lifetime of the bound ServiceAccount token the
// operator mints to log in with. It is short because the token is used once,
// within the same reconcile.
const provisionerTokenTTL = 10 * time.Minute

// configVolumeName is the Volume name the barbican-side deployment step mounts
// the rendered barbican.conf through. The store controller reads it back off the
// parent Barbican Deployment to observe ConfigProjected, so both controllers
// share this one name as a contract.
const configVolumeName = "config"

// barbicanConfDataKey is the data key inside the mounted config Secret holding
// the rendered barbican.conf, the INI document that carries one
// [secretstore:<name>] section per attached, ready store.
const barbicanConfDataKey = "barbican.conf"

// storeSectionPrefix is the section-name prefix barbican resolves its per-store
// sections under: the CR name becomes the suffix of [secretstore:<name>].
const storeSectionPrefix = "secretstore:"

// requiredStoreCapabilities are the capabilities barbican's vault plugin needs
// on the KV data path to store, read and soft-delete secret material. A
// brownfield store is validated against exactly this list.
var requiredStoreCapabilities = []string{"create", "read", "update", "delete", "list"}

// managedSubConditionTypes / brownfieldSubConditionTypes are the sub-conditions
// the aggregate Ready is derived from per mode. ProvisioningReady applies to a
// managed store only, so a brownfield store aggregates over two conditions and
// never waits for one nothing sets.
var (
	managedSubConditionTypes = []string{
		conditionTypeCredentialsReady,
		conditionTypeProvisioningReady,
		conditionTypeConfigProjected,
	}
	brownfieldSubConditionTypes = []string{
		conditionTypeCredentialsReady,
		conditionTypeConfigProjected,
	}
)

// BarbicanSecretStoreBarbicanRefIndexKey is the field-indexer key under which
// BarbicanSecretStore CRs are indexed by spec.barbicanRef.name. Used by
// barbicanToSecretStoresMapper (fan a Barbican event out to its stores) and by
// the barbican-side sub-reconciler that aggregates the attached stores.
// #nosec G101 -- field-indexer key (a JSONPath-like field selector), not a credential.
const BarbicanSecretStoreBarbicanRefIndexKey = "spec.barbicanRef.name"

// BarbicanSecretStoreInstanceRefIndexKey is the field-indexer key under which
// managed BarbicanSecretStore CRs are indexed by
// spec.openBao.instanceRef.name, so an OpenBaoCluster turning Available wakes
// exactly the stores provisioning against it.
// #nosec G101 -- field-indexer key (a JSONPath-like field selector), not a credential.
const BarbicanSecretStoreInstanceRefIndexKey = "spec.openBao.instanceRef.name"

// barbicanSecretStoreBarbicanRefExtractor is the IndexerFunc registered under
// BarbicanSecretStoreBarbicanRefIndexKey.
func barbicanSecretStoreBarbicanRefExtractor(obj client.Object) []string {
	store, ok := obj.(*barbicanv1alpha1.BarbicanSecretStore)
	if !ok || store.Spec.BarbicanRef.Name == "" {
		return nil
	}
	return []string{store.Spec.BarbicanRef.Name}
}

// barbicanSecretStoreInstanceRefExtractor is the IndexerFunc registered under
// BarbicanSecretStoreInstanceRefIndexKey. A brownfield store indexes under
// nothing: it names no instance in this cluster.
func barbicanSecretStoreInstanceRefExtractor(obj client.Object) []string {
	store, ok := obj.(*barbicanv1alpha1.BarbicanSecretStore)
	if !ok || store.Spec.OpenBao == nil || store.Spec.OpenBao.InstanceRef == nil {
		return nil
	}
	if store.Spec.OpenBao.InstanceRef.Name == "" {
		return nil
	}
	return []string{store.Spec.OpenBao.InstanceRef.Name}
}

// BarbicanSecretStoreReconciler owns the BarbicanSecretStore CR lifecycle: the
// AppRole credentials of the store, the provisioning state of the OpenBao
// instance behind a managed store, and the per-store conditions. It is the
// SINGLE writer of BarbicanSecretStore status; the barbican-side sub-reconciler
// only reads it (a ready store gates config projection) and writes an aggregated
// condition onto the Barbican CR instead.
//
// A store has two deletion lifecycles, and which one applies follows from where
// its parent Barbican places its workload:
//
//   - Local parent. The credentials Secret is owned by the store CR and
//     garbage-collected with it, so the store carries no finalizer and deleting
//     it only detaches it.
//   - Remote parent (the Barbican names spec.targetClusterRef). The Secret lives
//     on that cluster, where an owner reference to this CR means nothing, so it
//     is unowned and carries the ownership labels instead. The store records the
//     cluster in the childrenClusterAnnotation and carries
//     multicluster.RemoteChildrenFinalizer, and its deletion removes the Secret
//     explicitly: no garbage collection cascade crosses the cluster boundary.
//
// The AppRole is shared instance state the self-init contract owns either way,
// not this CR, and is never revoked: revoking it on delete would break every
// other store on the same instance. In both cases the parent Barbican
// de-projects the dropped section on its next pass.
type BarbicanSecretStoreReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// NewOpenBaoClient builds the client for one OpenBao server. Nil means
	// openbao.New; tests substitute a fake so no unit test needs a live server.
	NewOpenBaoClient func(cfg openbao.Config) (openbao.Client, error)

	// MintServiceAccountToken mints a bound token for the named ServiceAccount,
	// scoped to a single audience. Nil means the TokenRequest subresource; tests
	// substitute a stub because a fake client serves no subresources.
	MintServiceAccountToken func(ctx context.Context, saNamespace, saName, audience string) (string, error)

	// Now reads the wall clock the re-mint timer is evaluated against. Nil means
	// time.Now; tests inject a fixed clock so the 2/3-of-TTL threshold is
	// exercised without waiting for it.
	Now func() time.Time

	// Resolver resolves the target cluster the PARENT Barbican names in
	// spec.targetClusterRef into the client this store's children are read and
	// written with. A store carries no target of its own: it exists to serve one
	// Barbican, so its credentials Secret and the server it authenticates
	// against belong wherever that Barbican's workload runs. Nil means
	// always-local.
	Resolver commonmulticluster.ClusterResolver
}

// +kubebuilder:rbac:groups=barbican.openstack.c5c3.io,resources=barbicansecretstores,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=barbican.openstack.c5c3.io,resources=barbicansecretstores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=barbican.openstack.c5c3.io,resources=barbicansecretstores/finalizers,verbs=update
// +kubebuilder:rbac:groups=barbican.openstack.c5c3.io,resources=barbicans,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// The TokenRequest grant is namespace-bound, not part of the operator's
// ClusterRole: a cluster-wide grant would let anyone reaching this operator's
// ServiceAccount mint a token for a ServiceAccount in any namespace. The chart
// renders it as a Role/RoleBinding per rbac.secretStoreNamespaces key, with
// resourceNames narrowed to the accounts listed under that key, so it is not
// expressible as a kubebuilder marker.
// +kubebuilder:rbac:groups=openbao.org,resources=openbaoclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

// Reconcile drives one BarbicanSecretStore CR: resolve the store's AppRole
// credentials (minting them against a managed OpenBaoCluster, validating them
// against a brownfield server), observe whether the parent Barbican Deployment
// carries this store's rendered section, and persist the aggregated status.
func (r *BarbicanSecretStoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var store barbicanv1alpha1.BarbicanSecretStore
	if err := r.Get(ctx, req.NamespacedName, &store); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("BarbicanSecretStore not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching BarbicanSecretStore: %w", err)
	}

	// A deleting store sheds its metric series on every pass, so the re-mint
	// counter does not keep reporting a CR that is gone; the eviction is
	// idempotent, which matters because the remote teardown below can requeue for
	// minutes. Status is left untouched — writing to a terminating object is a
	// no-op at best and a conflict at worst.
	//
	// A local store is done here (see the type doc): its credentials Secret is
	// collected from the owner reference. Only a store whose Secret sits on a
	// target cluster carries the finalizer, and only that one has cleanup to do.
	if store.DeletionTimestamp != nil {
		metrics.DeleteForBarbicanSecretStore(store.Name, store.Namespace)
		if controllerutil.ContainsFinalizer(&store, commonmulticluster.RemoteChildrenFinalizer) {
			return r.reconcileDeleteRemoteCredentials(ctx, &store)
		}
		return ctrl.Result{}, nil
	}

	statusBefore := store.Status.DeepCopy()
	result, err := r.reconcileNormal(ctx, &store)
	return r.updateStatus(ctx, &store, statusBefore, result, err)
}

// reconcileNormal runs the mode-specific credential resolution and, once it
// succeeded, the config-projection observation. A store that is not credential-
// ready short-circuits before the observation: the parent never projects a store
// that is not ready, so there is nothing to observe yet.
//
// Everything it touches beyond the store CR itself is a child of the parent
// Barbican, so it opens by resolving that parent's target cluster into the
// children client the rest of the pass reads and writes with.
func (r *BarbicanSecretStoreReconciler) reconcileNormal(ctx context.Context, store *barbicanv1alpha1.BarbicanSecretStore) (ctrl.Result, error) {
	children, childrenCluster, result, err := r.resolveChildren(ctx, store)
	if children == nil {
		return result, err
	}

	// Both marks go on before anything is minted onto the target cluster, so a
	// Secret can never exist there without a finalizer that deletes it.
	if commonmulticluster.IsRemote(children) {
		if result, err := r.ensureRemoteTeardownMarks(ctx, store, childrenCluster); !result.IsZero() || err != nil {
			return result, err
		}
	}

	spec := store.Spec.OpenBao
	if spec == nil || (spec.InstanceRef == nil && spec.Server == nil) {
		// The schema union rules guarantee exactly one store block matching
		// spec.type; a bypassed admission leaves nothing to resolve.
		_, result, err := r.fail(store, conditionTypeCredentialsReady, conditionReasonWaitingForCredentials,
			"spec.openBao names neither instanceRef nor server; there is no OpenBao server to resolve credentials against")
		return result, err
	}

	var ready bool
	if spec.InstanceRef != nil {
		ready, result, err = r.reconcileManaged(ctx, children, childrenCluster, store, spec)
	} else {
		ready, result, err = r.reconcileBrownfield(ctx, children, childrenCluster, store, spec)
	}
	if err != nil || !ready {
		return result, err
	}

	projection, err := r.observeConfigProjected(ctx, children, store)
	if err != nil {
		return ctrl.Result{}, err
	}
	return commonreconcile.ShortestRequeue(result, projection), nil
}

// resolveChildren returns the client this store's children are read and written
// with: the one the PARENT Barbican's spec.targetClusterRef selects. The store
// has no target field of its own, so the AppRole credentials Secret it mints and
// the OpenBao instance it authenticates against land wherever the parent's
// workload runs — the same cluster the API pods read them from. It also returns
// the name of that cluster, empty for a parent that keeps its workload local, so
// the caller can record it on the store for the teardown.
//
// A parent that does not exist holds the pass on CredentialsReady=False and
// requeues, the same treatment an unresolvable target gets. Guessing the local
// client instead would be a write to the wrong cluster: this reconciler MINTS
// AppRole credentials, so a store whose parent is momentarily absent — the
// ordinary case when a GitOps apply lands both objects at once — would create a
// live secret ID against the management cluster's OpenBao and leave it behind
// unreferenced once the parent appears and the ref resolves elsewhere.
//
// A named-but-unregistered target returns a nil client with the failure already
// on CredentialsReady, the store's first gate condition.
func (r *BarbicanSecretStoreReconciler) resolveChildren(
	ctx context.Context, store *barbicanv1alpha1.BarbicanSecretStore,
) (client.Client, string, ctrl.Result, error) {
	var parent barbicanv1alpha1.Barbican
	parentKey := client.ObjectKey{Namespace: store.Namespace, Name: store.Spec.BarbicanRef.Name}
	if err := r.Get(ctx, parentKey, &parent); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, "", ctrl.Result{}, fmt.Errorf("fetching parent Barbican %s: %w", parentKey, err)
		}
		r.setCondition(store, conditionTypeCredentialsReady, metav1.ConditionFalse,
			conditionReasonWaitingForParent,
			fmt.Sprintf("parent Barbican %s does not exist; the cluster this store's credentials belong on is unknown", parentKey))
		return nil, "", ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	}

	children, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client, parent.Spec.TargetClusterRef)
	if err != nil {
		r.setCondition(store, conditionTypeCredentialsReady, metav1.ConditionFalse,
			commonmulticluster.TargetClusterUnavailable, err.Error())
		return nil, "", ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	}

	var childrenCluster string
	if parent.Spec.TargetClusterRef != nil {
		childrenCluster = parent.Spec.TargetClusterRef.Name
	}
	return children, childrenCluster, ctrl.Result{}, nil
}

// ensureRemoteTeardownMarks records what the deletion path needs to find the
// credentials Secret again: the annotation naming the cluster it is minted onto,
// and the finalizer that keeps the store in etcd long enough to delete it. Both
// are written before the mint, so a Secret on a target cluster always has a
// finalizer behind it.
//
// The order between them is the invariant, and it is why this is two writes
// rather than one: the annotation goes first, so a crash between them leaves an
// annotation without a finalizer (which costs nothing, since nothing was minted
// either) and never a finalizer whose handler cannot name the cluster it has to
// clean up on. The name is derived from two immutable fields, so a second pass
// finds the annotation it wrote and neither write repeats.
func (r *BarbicanSecretStoreReconciler) ensureRemoteTeardownMarks(
	ctx context.Context, store *barbicanv1alpha1.BarbicanSecretStore, childrenCluster string,
) (ctrl.Result, error) {
	// Compared against the resolved name, not merely tested for presence: the
	// store is a user-created CR, so a principal who may update it can put any
	// value under this key — an empty one included — before the operator's first
	// pass. Left standing, that value would send the teardown at a cluster the
	// Secret was never minted onto, or at none at all, and the credentials would
	// be abandoned on the real target while the CR reported a clean deletion.
	if store.Annotations[childrenClusterAnnotation] != childrenCluster {
		if store.Annotations == nil {
			store.Annotations = map[string]string{}
		}
		store.Annotations[childrenClusterAnnotation] = childrenCluster
		if err := r.Update(ctx, store); err != nil {
			return ctrl.Result{}, fmt.Errorf("recording the children cluster on the store: %w", err)
		}
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
	}

	if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, store, commonmulticluster.RemoteChildrenFinalizer); err != nil {
		return ctrl.Result{}, err
	} else if added {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
	}
	return ctrl.Result{}, nil
}

// reconcileDeleteRemoteCredentials deletes the AppRole credentials Secret this
// store minted onto a target cluster and then releases the remote-children
// finalizer. Nothing on that cluster collects the Secret: it carries the
// ownership labels rather than an owner reference, because a reference to a CR
// living on the management cluster names a UID the target's garbage collector
// cannot resolve.
//
// The AppRole behind those credentials is NOT touched. It is shared instance
// state the self-init contract provisions, and every other store on the same
// instance authenticates with it; only the Secret holding this store's secret ID
// is this CR's to remove.
//
// The clusters to visit are named by the parent Barbican wherever it can still
// be read and by the annotation the normal path stamped, which are usually the
// same one and are each visited when they are not (see childrenClusters). The
// annotation is what lets the store be deleted after its parent already is,
// which is the ordinary order when a whole namespace goes.
func (r *BarbicanSecretStoreReconciler) reconcileDeleteRemoteCredentials(
	ctx context.Context, store *barbicanv1alpha1.BarbicanSecretStore,
) (ctrl.Result, error) {
	clusters, err := r.childrenClusters(ctx, store)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(clusters) == 0 {
		// Neither mark survived: the annotation is absent and the parent is gone
		// too, so nothing names the cluster the Secret is on. Waiting cannot
		// produce the name, but the parent may still be terminating and about to
		// be read one more time, so the store waits out the same window an
		// unresolvable cluster gets before it gives the Secret up.
		if r.now().Sub(store.DeletionTimestamp.Time) < commonmulticluster.AbandonAfter {
			return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
		}
		r.Recorder.Event(store, corev1.EventTypeWarning, "RemoteChildrenAbandoned",
			"Neither this store nor a parent Barbican names the cluster its AppRole credentials Secret was minted onto; "+
				"releasing the remote-children finalizer without deleting it")
		return r.releaseRemoteChildrenFinalizer(ctx, store)
	}

	for _, cluster := range clusters {
		children, wait := commonmulticluster.ResolveChildrenClientForDeletion(ctx, r.Resolver, r.Client,
			&commonv1.TargetClusterRefSpec{Name: cluster}, *store.DeletionTimestamp)
		if wait {
			return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
		}
		if children == nil {
			r.Recorder.Event(store, corev1.EventTypeWarning, "RemoteChildrenAbandoned",
				fmt.Sprintf("Target cluster %s is no longer registered; releasing the remote-children finalizer without "+
					"deleting the AppRole credentials Secret on it", cluster))
			continue
		}

		if err := r.deleteRemoteCredentialsSecret(ctx, children, cluster, store); err != nil {
			return ctrl.Result{}, err
		}
	}
	return r.releaseRemoteChildrenFinalizer(ctx, store)
}

// childrenClusters names every cluster this store's credentials Secret may sit
// on: the parent Barbican's target wherever the parent can still answer, and the
// annotation the normal path stamped. They name the same cluster on every
// ordinary teardown, and then this is one name. An empty result means neither
// source answered, which is the parent having been deleted inside the crash
// window between the annotation write and the finalizer add.
//
// Both are visited when they disagree, because neither source is authoritative
// on its own and picking one leaves a live AppRole secret ID on the other with
// no CR left to reclaim it:
//
//   - The parent can be gone, which is the ordinary order when a whole namespace
//     is deleted, and it can also have been deleted and re-created under the same
//     name against a different target since the mint. Its
//     spec.targetClusterRef is immutable, but immutability is a transition rule
//     the API server evaluates on UPDATE, and a delete/create pair is not one.
//   - The annotation is an ordinary field of a user-created CR: anyone who may
//     update the store can put a different registered cluster under that key, and
//     nothing corrects the value once the store carries a deletionTimestamp,
//     since the API server takes annotation writes on a terminating object and
//     ensureRemoteTeardownMarks no longer runs.
//
// Visiting a cluster the Secret was never minted onto costs one read that finds
// nothing, and the delete is gated on ownership either way (see
// deleteRemoteCredentialsSecret), so a planted name cannot reach anything that
// is not this store's own child.
func (r *BarbicanSecretStoreReconciler) childrenClusters(ctx context.Context, store *barbicanv1alpha1.BarbicanSecretStore) ([]string, error) {
	var clusters []string

	var parent barbicanv1alpha1.Barbican
	parentKey := client.ObjectKey{Namespace: store.Namespace, Name: store.Spec.BarbicanRef.Name}
	switch err := r.Get(ctx, parentKey, &parent); {
	case err == nil:
		if parent.Spec.TargetClusterRef != nil {
			clusters = append(clusters, parent.Spec.TargetClusterRef.Name)
		}
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("fetching parent Barbican %s: %w", parentKey, err)
	}

	// A store carrying the finalizer minted somewhere, and the annotation is the
	// only record of where that survives its parent.
	annotated := store.Annotations[childrenClusterAnnotation]
	if annotated != "" && !slices.Contains(clusters, annotated) {
		clusters = append(clusters, annotated)
	}
	return clusters, nil
}

// deleteRemoteCredentialsSecret removes the credentials Secret from the named
// target cluster, and only when this store owns it. A Secret of the same name
// that somebody else provisioned there carries no ownership labels of ours and
// is left standing: the store name is not a reservation on that namespace. That
// gate is also what makes it safe to visit every cluster childrenClusters
// returns rather than to pick one of them.
func (r *BarbicanSecretStoreReconciler) deleteRemoteCredentialsSecret(
	ctx context.Context, children client.Client, cluster string, store *barbicanv1alpha1.BarbicanSecretStore,
) error {
	credsKey := client.ObjectKey{Namespace: store.Namespace, Name: store.Name + approleSecretNameSuffix}

	// Read through the target cluster's uncached API reader, never through the
	// children client's cache. A NotFound here licenses the finalizer release, so
	// a Secret the cache has not caught up on — an operator restart, a cluster
	// engaged moments ago — would leave a live OpenBao secret ID materialized on
	// the target with no CR left to delete it.
	var creds corev1.Secret
	if err := commonmulticluster.LiveReader(children).Get(ctx, credsKey, &creds); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("fetching the AppRole credentials Secret %s for teardown: %w", credsKey, err)
	}

	owned, err := commonmulticluster.Controls(r.Scheme, store, &creds)
	if err != nil {
		return err
	}
	if !owned {
		log.FromContext(ctx).Info("leaving a foreign Secret of the store's credentials name behind",
			"secret", credsKey, "targetCluster", cluster)
		return nil
	}

	if err := children.Delete(ctx, &creds); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting the AppRole credentials Secret %s: %w", credsKey, err)
	}
	return nil
}

// releaseRemoteChildrenFinalizer drops the finalizer and persists the store, the
// single exit of every teardown path above.
func (r *BarbicanSecretStoreReconciler) releaseRemoteChildrenFinalizer(
	ctx context.Context, store *barbicanv1alpha1.BarbicanSecretStore,
) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(store, commonmulticluster.RemoteChildrenFinalizer)
	if err := r.Update(ctx, store); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing remote-children finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// reconcileManaged provisions the store against the OpenBaoCluster it names: it
// gates on the instance being Available, authenticates as the instance's
// provisioner, verifies that the self-init contract ran, and mints or refreshes
// the store's AppRole credentials. It returns (true, …) only when the
// credentials are live.
func (r *BarbicanSecretStoreReconciler) reconcileManaged(
	ctx context.Context, children client.Client, childrenCluster string,
	store *barbicanv1alpha1.BarbicanSecretStore, spec *barbicanv1alpha1.OpenBaoStoreSpec,
) (bool, ctrl.Result, error) {
	instanceName := spec.InstanceRef.Name
	instanceKey := client.ObjectKey{Namespace: store.Namespace, Name: instanceName}

	var instance openbaov1alpha1.OpenBaoCluster
	if err := children.Get(ctx, instanceKey, &instance); err != nil {
		if apierrors.IsNotFound(err) {
			return r.fail(store, conditionTypeProvisioningReady, conditionReasonInstanceNotFound,
				fmt.Sprintf("OpenBaoCluster %s does not exist", instanceKey))
		}
		return false, ctrl.Result{}, fmt.Errorf("fetching OpenBaoCluster %s: %w", instanceKey, err)
	}
	if !conditions.AllTrue(instance.Status.Conditions, string(openbaov1alpha1.ConditionAvailable)) {
		return r.fail(store, conditionTypeProvisioningReady, conditionReasonWaitingForInstance,
			fmt.Sprintf("OpenBaoCluster %s is not %s yet", instanceKey, openbaov1alpha1.ConditionAvailable))
	}

	caKey := client.ObjectKey{Namespace: store.Namespace, Name: instanceName + instanceCASecretSuffix}
	caPEM, err := secrets.GetSecretValue(ctx, children, caKey, barbicanv1alpha1.OpenBaoCAKey)
	if err != nil {
		if secrets.IsMissingSecretOrKey(err) {
			return r.fail(store, conditionTypeProvisioningReady, conditionReasonWaitingForInstanceTLS,
				fmt.Sprintf("waiting for Secret %s to carry the %q trust bundle of OpenBaoCluster %q",
					caKey, barbicanv1alpha1.OpenBaoCAKey, instanceName))
		}
		return false, ctrl.Result{}, fmt.Errorf("reading the trust bundle of OpenBaoCluster %q: %w", instanceName, err)
	}

	// The audience is the instance name: the delivered kubernetes auth role
	// accepts a token minted for this instance and nothing else, so a token
	// auto-mounted into some other pod cannot be replayed against it.
	saName := instanceName + instanceProvisionerSASuffix
	token, err := r.mintToken(ctx, children, store.Namespace, saName, instanceName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return r.fail(store, conditionTypeProvisioningReady, conditionReasonWaitingForInstance,
				fmt.Sprintf("waiting for the provisioner ServiceAccount %s/%s of OpenBaoCluster %q",
					store.Namespace, saName, instanceName))
		}
		// A 403 is the DEFAULT state of a namespace nobody opted in: the grant is
		// never part of the operator ClusterRole and is rendered as one Role per
		// rbac.secretStoreNamespaces key, narrowed to the accounts listed under
		// it — the map defaults to empty. Reported on the condition rather than
		// returned bare, which would leave ProvisioningReady absent and put the
		// only evidence of the missing grant in the operator log while the store
		// hot-loops on error backoff.
		if apierrors.IsForbidden(err) {
			return r.fail(store, conditionTypeProvisioningReady, conditionReasonProvisioningDenied,
				fmt.Sprintf("the operator may not mint a bound token for the provisioner ServiceAccount %s/%s: add "+
					"rbac.secretStoreNamespaces.%s = [%q], then upgrade the barbican-operator chart — the "+
					"TokenRequest grant is namespace-scoped and account-scoped by design and is not part of the "+
					"operator ClusterRole: %v",
					store.Namespace, saName, store.Namespace, saName, err))
		}
		return false, ctrl.Result{}, err
	}

	// The dial is resolved once and travels with the config: every later client
	// this pass builds — the credential probe above all — copies it and reaches
	// the same instance over the same route.
	dial, err := r.openBaoDialer(ctx, childrenCluster)
	if err != nil {
		return r.fail(store, conditionTypeProvisioningReady, conditionReasonOpenBaoUnreachable, err.Error())
	}

	cfg := openbao.Config{
		URL:         instanceURL(instanceName, store.Namespace),
		CACertPEM:   []byte(caPEM),
		Timeout:     openBaoRequestTimeout,
		DialContext: dial,
	}
	provisioner, err := r.newOpenBaoClient(cfg)
	if err != nil {
		return r.fail(store, conditionTypeProvisioningReady, conditionReasonOpenBaoUnreachable, err.Error())
	}
	// Deferred so the session is handed back on every path, including the
	// failure arms below: a reconcile that gives up must not leave a live
	// provisioner token behind.
	defer r.revokeSession(ctx, provisioner)

	if err := provisioner.LoginKubernetes(ctx, token, provisionerAuthRole); err != nil {
		wrapped := fmt.Errorf("logging in to OpenBao: %w", err)
		if openbao.IsPermissionDenied(err) {
			return r.fail(store, conditionTypeProvisioningReady, conditionReasonProvisioningDenied, wrapped.Error())
		}
		return r.fail(store, conditionTypeProvisioningReady, conditionReasonOpenBaoUnreachable, wrapped.Error())
	}

	mounted, err := provisioner.MountExists(ctx, spec.KVMountpoint)
	if err != nil {
		return r.failOpenBao(store, conditionTypeProvisioningReady, conditionReasonProvisioningDenied, err)
	}
	if !mounted {
		return r.fail(store, conditionTypeProvisioningReady, conditionReasonInstanceNotProvisioned,
			notProvisionedMessage(fmt.Sprintf("the KV mount %q is not mounted on OpenBaoCluster %q", spec.KVMountpoint, instanceName)))
	}

	// The role ID is read once here, both as the "the AppRole exists" probe and
	// as the value written into the credentials Secret: it is stable for the
	// lifetime of the AppRole, so there is nothing to re-read at mint time.
	roleID, err := provisioner.ReadRoleID(ctx, storeAppRoleName)
	if err != nil {
		if openbao.IsNotFound(err) {
			return r.fail(store, conditionTypeProvisioningReady, conditionReasonInstanceNotProvisioned,
				notProvisionedMessage(fmt.Sprintf("the AppRole %q does not exist on OpenBaoCluster %q", storeAppRoleName, instanceName)))
		}
		return r.failOpenBao(store, conditionTypeProvisioningReady, conditionReasonProvisioningDenied, err)
	}
	r.setCondition(store, conditionTypeProvisioningReady, metav1.ConditionTrue, conditionReasonProvisioned,
		fmt.Sprintf("OpenBaoCluster %q carries the KV mount %q and the AppRole %q", instanceName, spec.KVMountpoint, storeAppRoleName))

	return r.reconcileManagedCredentials(ctx, children, store, cfg, provisioner, roleID)
}

// reconcileManagedCredentials decides whether the store's credentials Secret
// needs a fresh secret ID and mints one when it does.
//
// The decision ladder is: an absent, unstamped or incomplete Secret is filled
// like an initial mint; a stamped Secret past two thirds of its TTL is re-minted
// proactively, before barbican can run into an expired credential; anything else
// is verified with a login probe and re-minted reactively when the server no
// longer accepts it. A TTL of 0 means the secret ID does not expire, which
// disables the timer — the probe stays.
func (r *BarbicanSecretStoreReconciler) reconcileManagedCredentials(
	ctx context.Context, children client.Client, store *barbicanv1alpha1.BarbicanSecretStore,
	cfg openbao.Config, provisioner openbao.Client, roleID string,
) (bool, ctrl.Result, error) {
	credsKey := client.ObjectKey{Namespace: store.Namespace, Name: store.Name + approleSecretNameSuffix}

	var creds corev1.Secret
	if err := children.Get(ctx, credsKey, &creds); err != nil {
		if apierrors.IsNotFound(err) {
			return r.mintCredentials(ctx, children, store, cfg, provisioner, roleID, credsKey, "")
		}
		return false, ctrl.Result{}, fmt.Errorf("fetching the AppRole credentials Secret %s: %w", credsKey, err)
	}

	mintedAt, ttl, stamped := mintStamp(&creds)
	currentRoleID := string(creds.Data[barbicanv1alpha1.OpenBaoRoleIDKey])
	currentSecretID := string(creds.Data[barbicanv1alpha1.OpenBaoSecretIDKey])
	if !stamped || currentRoleID == "" || currentSecretID == "" {
		return r.mintCredentials(ctx, children, store, cfg, provisioner, roleID, credsKey, "")
	}
	if ttl > 0 && r.now().Sub(mintedAt) > proactiveThreshold(ttl) {
		return r.mintCredentials(ctx, children, store, cfg, provisioner, roleID, credsKey, remintTriggerProactive)
	}

	if err := r.probeAppRole(ctx, cfg, currentRoleID, currentSecretID); err != nil {
		if openbao.IsInvalidCredentials(err) {
			// The server rejects the credentials the store holds: re-mint once,
			// now, rather than waiting for the timer that evidently arrived late.
			//
			// Bounded by the age of the credentials in hand. A rejection the mint
			// cannot fix — an AppRole with secret_id_bound_cidrs the operator pod is
			// not in, bind_secret_id disabled, a proxy answering 400 — reproduces on
			// the freshly minted secret ID too, and every failure state of this
			// controller retries at RequeueSecretStoreRetry without reaching the
			// workqueue as an error. Without the floor that is one new secret-ID
			// accessor minted every retry interval, per store, indefinitely.
			if r.now().Sub(mintedAt) >= reactiveRemintCooldown {
				return r.mintCredentials(ctx, children, store, cfg, provisioner, roleID, credsKey, remintTriggerReactive)
			}
			return r.fail(store, conditionTypeCredentialsReady, conditionReasonInvalidCredentials,
				fmt.Sprintf("the server rejects the AppRole credentials of Secret %s, which were minted %s ago; "+
					"a fresh secret ID is minted at most once every %s, so the rejection is reported rather than re-minted: %v",
					credsKey, r.now().Sub(mintedAt).Truncate(time.Second), reactiveRemintCooldown, err))
		}
		return r.fail(store, conditionTypeCredentialsReady, conditionReasonOpenBaoUnreachable, err.Error())
	}
	return r.credentialsReady(store, credsKey, mintedAt, ttl)
}

// mintCredentials mints a secret ID through the AppRole generate endpoint,
// writes it into the credentials Secret together with the mint stamp, and
// verifies the result with a login probe. trigger labels the re-mint counter;
// an empty trigger marks an initial mint, which is not a re-mint and is not
// counted.
func (r *BarbicanSecretStoreReconciler) mintCredentials(
	ctx context.Context, children client.Client, store *barbicanv1alpha1.BarbicanSecretStore,
	cfg openbao.Config, provisioner openbao.Client, roleID string, credsKey client.ObjectKey, trigger string,
) (bool, ctrl.Result, error) {
	secretID, ttlSeconds, err := provisioner.GenerateSecretID(ctx, storeAppRoleName)
	if err != nil {
		return r.failOpenBao(store, conditionTypeCredentialsReady, conditionReasonProvisioningDenied, err)
	}
	mintedAt := r.now().UTC()

	creds := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: credsKey.Namespace, Name: credsKey.Name}}
	if _, err := controllerutil.CreateOrUpdate(ctx, children, creds, func() error {
		if creds.Data == nil {
			creds.Data = map[string][]byte{}
		}
		creds.Data[barbicanv1alpha1.OpenBaoRoleIDKey] = []byte(roleID)
		creds.Data[barbicanv1alpha1.OpenBaoSecretIDKey] = []byte(secretID)
		if creds.Annotations == nil {
			creds.Annotations = map[string]string{}
		}
		creds.Annotations[secretIDMintedAtAnnotation] = mintedAt.Format(time.RFC3339)
		creds.Annotations[secretIDTTLSecondsAnnotation] = strconv.FormatInt(ttlSeconds, 10)
		// A local Secret is claimed by a controller owner reference, one on a
		// target cluster by the ownership labels the teardown selects on. The
		// mutate touches no labels of its own, so the claim is the only writer of
		// them and nothing can drop what it stamped.
		return commonmulticluster.Claim(children, r.Scheme, store, creds)
	}); err != nil {
		return false, ctrl.Result{}, fmt.Errorf("writing the AppRole credentials Secret %s: %w", credsKey, err)
	}

	if err := r.probeAppRole(ctx, cfg, roleID, secretID); err != nil {
		return r.failOpenBao(store, conditionTypeCredentialsReady, conditionReasonInvalidCredentials, err)
	}
	if trigger != "" {
		metrics.RecordSecretStoreRemint(store.Name, store.Namespace, trigger)
	}
	return r.credentialsReady(store, credsKey, mintedAt, time.Duration(ttlSeconds)*time.Second)
}

// credentialsReady stamps CredentialsReady True and schedules the next pass at
// the proactive re-mint threshold of the credentials in hand.
func (r *BarbicanSecretStoreReconciler) credentialsReady(
	store *barbicanv1alpha1.BarbicanSecretStore, credsKey client.ObjectKey, mintedAt time.Time, ttl time.Duration,
) (bool, ctrl.Result, error) {
	r.setCondition(store, conditionTypeCredentialsReady, metav1.ConditionTrue, conditionReasonCredentialsAvailable,
		fmt.Sprintf("Secret %s carries AppRole credentials the server accepts", credsKey))
	return true, ctrl.Result{RequeueAfter: r.remintRequeue(mintedAt, ttl)}, nil
}

// reconcileBrownfield validates a store against a server provisioned outside
// this operator. It is read-only by contract (the K-ORC unmanaged posture): the
// credentials are read from the referenced Secret, verified with a login and a
// capability read, and nothing is ever written to the server — no mount, no
// policy, no AppRole, no secret material.
func (r *BarbicanSecretStoreReconciler) reconcileBrownfield(
	ctx context.Context, children client.Client, childrenCluster string,
	store *barbicanv1alpha1.BarbicanSecretStore, spec *barbicanv1alpha1.OpenBaoStoreSpec,
) (bool, ctrl.Result, error) {
	// Nothing provisions a brownfield server, so the condition must not linger
	// on a store that used to name an instanceRef.
	meta.RemoveStatusCondition(&store.Status.Conditions, conditionTypeProvisioningReady)

	server := spec.Server
	credsKey := client.ObjectKey{Namespace: store.Namespace, Name: server.CredentialsSecretRef.Name}
	roleID, missing, err := r.readCredential(ctx, children, credsKey, barbicanv1alpha1.OpenBaoRoleIDKey)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	if missing {
		return r.waitingForCredentials(store, credsKey, barbicanv1alpha1.OpenBaoRoleIDKey)
	}
	secretID, missing, err := r.readCredential(ctx, children, credsKey, barbicanv1alpha1.OpenBaoSecretIDKey)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	if missing {
		return r.waitingForCredentials(store, credsKey, barbicanv1alpha1.OpenBaoSecretIDKey)
	}

	var caPEM string
	if server.CABundleSecretRef != nil {
		caKey := client.ObjectKey{Namespace: store.Namespace, Name: server.CABundleSecretRef.Name}
		caPEM, missing, err = r.readCredential(ctx, children, caKey, barbicanv1alpha1.OpenBaoCAKey)
		if err != nil {
			return false, ctrl.Result{}, err
		}
		if missing {
			return r.waitingForCredentials(store, caKey, barbicanv1alpha1.OpenBaoCAKey)
		}
	}

	// spec.openBao.namespace scopes the operator's own login and capability read
	// the same way it scopes the rendered configuration: the AppRole lives in that
	// namespace, so a root-namespace login would be rejected and the store would
	// report InvalidCredentials against credentials that are correct.
	dial, err := r.openBaoDialer(ctx, childrenCluster)
	if err != nil {
		return r.fail(store, conditionTypeCredentialsReady, conditionReasonOpenBaoUnreachable, err.Error())
	}

	baoClient, err := r.newOpenBaoClient(openbao.Config{
		URL:         server.URL,
		CACertPEM:   []byte(caPEM),
		Namespace:   spec.Namespace,
		Timeout:     openBaoRequestTimeout,
		DialContext: dial,
	})
	if err != nil {
		return r.fail(store, conditionTypeCredentialsReady, conditionReasonOpenBaoUnreachable, err.Error())
	}
	defer r.revokeSession(ctx, baoClient)

	if err := baoClient.LoginAppRole(ctx, roleID, secretID); err != nil {
		if openbao.IsInvalidCredentials(err) {
			return r.fail(store, conditionTypeCredentialsReady, conditionReasonInvalidCredentials, err.Error())
		}
		return r.fail(store, conditionTypeCredentialsReady, conditionReasonOpenBaoUnreachable, err.Error())
	}

	// The probe path is never written to: sys/capabilities-self reports what the
	// policy grants without touching the mount.
	probePath := fmt.Sprintf("%s/data/probe", spec.KVMountpoint)
	granted, err := baoClient.CapabilitiesSelf(ctx, probePath)
	if err != nil {
		return r.failOpenBao(store, conditionTypeCredentialsReady, conditionReasonInsufficientCapabilities, err)
	}
	if missing := missingCapabilities(granted); len(missing) > 0 {
		return r.fail(store, conditionTypeCredentialsReady, conditionReasonInsufficientCapabilities,
			fmt.Sprintf("the AppRole credentials of Secret %s lack the %s capabilities on %q",
				credsKey, strings.Join(missing, ", "), probePath))
	}

	r.setCondition(store, conditionTypeCredentialsReady, metav1.ConditionTrue, conditionReasonCredentialsAvailable,
		fmt.Sprintf("Secret %s carries AppRole credentials with the %s capabilities on %q",
			credsKey, strings.Join(requiredStoreCapabilities, ", "), probePath))
	return true, ctrl.Result{RequeueAfter: RequeueSecretStoreRevalidate}, nil
}

// readCredential reads one data key of a referenced Secret. An absent Secret, an
// absent key and an empty value fold into missing: none of them is something the
// operator can authenticate with, and all three are states GitOps ordering
// produces routinely. Any other failure is a backend error and is propagated.
func (r *BarbicanSecretStoreReconciler) readCredential(ctx context.Context, children client.Client, key client.ObjectKey, dataKey string) (string, bool, error) {
	value, err := secrets.GetSecretValue(ctx, children, key, dataKey)
	if err != nil {
		if secrets.IsMissingSecretOrKey(err) {
			return "", true, nil
		}
		return "", false, fmt.Errorf("reading key %q of Secret %s: %w", dataKey, key, err)
	}
	return value, value == "", nil
}

// waitingForCredentials reports a Secret that does not carry a usable value
// under dataKey yet.
func (r *BarbicanSecretStoreReconciler) waitingForCredentials(
	store *barbicanv1alpha1.BarbicanSecretStore, key client.ObjectKey, dataKey string,
) (bool, ctrl.Result, error) {
	return r.fail(store, conditionTypeCredentialsReady, conditionReasonWaitingForCredentials,
		fmt.Sprintf("waiting for Secret %s to carry a non-empty %q", key, dataKey))
}

// probeAppRole verifies a pair of AppRole credentials by logging in with them
// and handing the session straight back. It is the only way to tell a live
// secret ID from a revoked one: OpenBao hands a secret ID out once and offers no
// read-back.
func (r *BarbicanSecretStoreReconciler) probeAppRole(ctx context.Context, cfg openbao.Config, roleID, secretID string) error {
	probe, err := r.newOpenBaoClient(cfg)
	if err != nil {
		return err
	}
	defer r.revokeSession(ctx, probe)
	return probe.LoginAppRole(ctx, roleID, secretID)
}

// observeConfigProjected derives the ConfigProjected condition from the single
// authoritative pointer: the parent Barbican Deployment's config volume and the
// rendered section inside the object it references. A False observation carries
// a RequeueSecretStoreRetry safety net — the Barbican watch normally wakes this
// controller when the projection lands, but a converged Barbican status (no
// write, no event) must not strand the store. A transient Get failure returns
// the error WITHOUT demoting a currently-True condition, so a converged store
// does not flap on a cache blip.
func (r *BarbicanSecretStoreReconciler) observeConfigProjected(ctx context.Context, children client.Client, store *barbicanv1alpha1.BarbicanSecretStore) (ctrl.Result, error) {
	projected, err := r.isConfigProjected(ctx, children, store)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !projected {
		r.setCondition(store, conditionTypeConfigProjected, metav1.ConditionFalse, conditionReasonWaitingForProjection,
			"waiting for the Barbican Deployment to mount this store's rendered secret-store section")
		return ctrl.Result{RequeueAfter: RequeueSecretStoreRetry}, nil
	}
	r.setCondition(store, conditionTypeConfigProjected, metav1.ConditionTrue, conditionReasonConfigProjected,
		"secret-store section is rendered into the Barbican Deployment's config")
	return ctrl.Result{}, nil
}

// isConfigProjected reports whether the parent Barbican Deployment mounts this
// store's rendered section: the config volume's barbican.conf carries the
// [secretstore:<store.Name>] section header. A missing parent, Deployment,
// volume, or config Secret folds to (false, nil) — an authoritative "not
// projected yet". Only a non-NotFound client failure returns an error.
//
// The parent CR is read on the management cluster, the Deployment it projects on
// the children one.
func (r *BarbicanSecretStoreReconciler) isConfigProjected(ctx context.Context, children client.Client, store *barbicanv1alpha1.BarbicanSecretStore) (bool, error) {
	var barbican barbicanv1alpha1.Barbican
	barbicanKey := client.ObjectKey{Namespace: store.Namespace, Name: store.Spec.BarbicanRef.Name}
	if err := r.Get(ctx, barbicanKey, &barbican); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("fetching parent Barbican %s: %w", barbicanKey, err)
	}

	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Namespace: barbican.Namespace, Name: subResourceName(&barbican)}
	if err := children.Get(ctx, deployKey, &deploy); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("fetching Deployment %s: %w", deployKey, err)
	}

	rendered, err := r.renderedConfig(ctx, children, &deploy)
	if err != nil {
		return false, err
	}
	return sectionPresent(rendered, storeSectionHeader(store.Name)), nil
}

// renderedConfig returns the barbican.conf the Deployment mounts through its
// config volume. The carrier is a Secret: the rendered file carries the vault
// plugin's approle_role_id, and barbican reads exactly one config artifact, so
// the whole document is credential-bearing. A config volume backed by anything
// else, an absent volume, or an absent Secret yields no bytes, which reads as
// "not projected yet".
func (r *BarbicanSecretStoreReconciler) renderedConfig(ctx context.Context, children client.Client, deploy *appsv1.Deployment) ([]byte, error) {
	var volume *corev1.Volume
	for i := range deploy.Spec.Template.Spec.Volumes {
		if deploy.Spec.Template.Spec.Volumes[i].Name == configVolumeName {
			volume = &deploy.Spec.Template.Spec.Volumes[i]
			break
		}
	}
	if volume == nil || volume.Secret == nil {
		return nil, nil
	}

	var secret corev1.Secret
	key := client.ObjectKey{Namespace: deploy.Namespace, Name: volume.Secret.SecretName}
	if err := children.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("fetching config Secret %s: %w", key, err)
	}
	return secret.Data[barbicanConfDataKey], nil
}

// subResourceName returns the canonical name for Barbican operator-managed
// sub-resources (Deployment, Service, and the rest of the workload). Centralised
// here so the naming convention is defined in one place — the bare CR name with
// no suffix.
func subResourceName(barbican *barbicanv1alpha1.Barbican) string {
	return barbican.Name
}

// storeSectionHeader returns the INI section header barbican reads a store's
// configuration from.
func storeSectionHeader(storeName string) string {
	return "[" + storeSectionPrefix + storeName + "]"
}

// sectionPresent reports whether the rendered config carries header as a whole
// line: the literal token bounded by the start of data or a newline on the left
// and a newline (LF or CR) or the end of data on the right. The boundary check
// guards against substring collisions — a section [secretstore:name2] must not
// satisfy a lookup for name, and a header appearing inside an option value must
// not either.
func sectionPresent(rendered []byte, header string) bool {
	for _, line := range strings.Split(string(rendered), "\n") {
		// Cut at the first CR so a CRLF-terminated line and a bare CR both end
		// the token, matching the newline forms the renderer can emit.
		if token, _, _ := strings.Cut(line, "\r"); token == header {
			return true
		}
	}
	return false
}

// fail records a False sub-condition, emits the matching Warning event, and
// returns the standard waiting result. Every waiting and failure state of this
// controller goes through it, so none of them reaches the workqueue as an error:
// an unreachable OpenBao server or a store whose credentials have not been
// provided yet is a state to report and retry, not a reconcile failure.
func (r *BarbicanSecretStoreReconciler) fail(
	store *barbicanv1alpha1.BarbicanSecretStore, conditionType, reason, message string,
) (bool, ctrl.Result, error) {
	r.setCondition(store, conditionType, metav1.ConditionFalse, reason, message)
	r.Recorder.Event(store, corev1.EventTypeWarning, reason, message)
	return false, ctrl.Result{RequeueAfter: RequeueSecretStoreRetry}, nil
}

// failOpenBao maps a failed OpenBao call onto conditionType: a 403 is the policy
// gap deniedReason names, anything else is reported as the server being out of
// reach. The client already names the call it made, so its error text is the
// condition message.
func (r *BarbicanSecretStoreReconciler) failOpenBao(
	store *barbicanv1alpha1.BarbicanSecretStore, conditionType, deniedReason string, err error,
) (bool, ctrl.Result, error) {
	if openbao.IsPermissionDenied(err) {
		return r.fail(store, conditionType, deniedReason, err.Error())
	}
	return r.fail(store, conditionType, conditionReasonOpenBaoUnreachable, err.Error())
}

// setCondition upserts one of the store's sub-conditions.
func (r *BarbicanSecretStoreReconciler) setCondition(
	store *barbicanv1alpha1.BarbicanSecretStore, conditionType string, status metav1.ConditionStatus, reason, message string,
) {
	conditions.SetCondition(&store.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: store.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// updateStatus persists the store status via the shared helper: the write is
// skipped when the pass left status unchanged, the aggregate Ready is re-derived
// from the mode's sub-condition set on every persist, and ObservedGeneration is
// stamped.
func (r *BarbicanSecretStoreReconciler) updateStatus(
	ctx context.Context, store *barbicanv1alpha1.BarbicanSecretStore,
	statusBefore *barbicanv1alpha1.BarbicanSecretStoreStatus, result ctrl.Result, reconcileErr error,
) (ctrl.Result, error) {
	return commonreconcile.UpdateStatus(ctx, r.Client, store, statusBefore, &store.Status, func() {
		commonreconcile.SetAggregateReady(&store.Status.Conditions, store.Generation, subConditionTypesFor(store))
		store.Status.ObservedGeneration = store.Generation
	}, result, reconcileErr)
}

// subConditionTypesFor returns the sub-conditions the aggregate Ready is derived
// from for this store's mode.
func subConditionTypesFor(store *barbicanv1alpha1.BarbicanSecretStore) []string {
	if store.Spec.OpenBao != nil && store.Spec.OpenBao.InstanceRef != nil {
		return managedSubConditionTypes
	}
	return brownfieldSubConditionTypes
}

// openBaoDialer returns the dial the store's OpenBao calls are opened with, or
// nil for the default one.
//
// A parent that keeps its workload local gets nil: the server's Service DNS name
// resolves from the operator, so nothing needs redirecting and the client is
// built byte for byte as it always was. A parent on a target cluster gets a
// tunnel through that cluster's API server, because the name resolves there and
// nowhere else.
//
// Only the TCP path moves. The URL stays the one the caller derived, which for a
// managed instance is the SAN its server certificate carries, and the handshake
// still runs against the instance itself — routing the request through the API
// server's service proxy instead would terminate TLS at the API server and break
// exactly that.
//
// A brownfield store may name a server that is no Service of the target cluster
// at all; the dialer passes such an address through to a plain dial, so setting
// it costs a remote brownfield store nothing.
func (r *BarbicanSecretStoreReconciler) openBaoDialer(
	ctx context.Context, childrenCluster string,
) (func(ctx context.Context, network, addr string) (net.Conn, error), error) {
	if r.Resolver == nil || childrenCluster == "" {
		return nil, nil
	}

	cl, err := r.Resolver.GetCluster(ctx, mcruntime.ClusterName(childrenCluster))
	if err != nil {
		return nil, err
	}
	dialer, err := commonmulticluster.NewPortForwardDialer(cl.GetConfig(), cl.GetAPIReader())
	if err != nil {
		return nil, err
	}
	return dialer.DialContext, nil
}

// newOpenBaoClient builds an OpenBao client through the seam, defaulting to the
// production constructor.
func (r *BarbicanSecretStoreReconciler) newOpenBaoClient(cfg openbao.Config) (openbao.Client, error) {
	if r.NewOpenBaoClient != nil {
		return r.NewOpenBaoClient(cfg)
	}
	baoClient, err := openbao.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("building the OpenBao client: %w", err)
	}
	return baoClient, nil
}

// mintToken mints a bound ServiceAccount token through the seam, defaulting to
// the TokenRequest subresource.
func (r *BarbicanSecretStoreReconciler) mintToken(ctx context.Context, children client.Client, saNamespace, saName, audience string) (string, error) {
	if r.MintServiceAccountToken != nil {
		return r.MintServiceAccountToken(ctx, saNamespace, saName, audience)
	}
	return r.mintServiceAccountToken(ctx, children, saNamespace, saName, audience)
}

// mintServiceAccountToken requests a short-lived token for the named
// ServiceAccount, bound to a single audience so it cannot be replayed against
// any other consumer. The token lives only as long as the reconcile that uses
// it, and it is never persisted. It is minted on the children cluster: the
// audience is the OpenBaoCluster the token is presented to, and that instance
// only accepts tokens its own API server issued.
func (r *BarbicanSecretStoreReconciler) mintServiceAccountToken(ctx context.Context, children client.Client, saNamespace, saName, audience string) (string, error) {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: saNamespace, Name: saName}}
	request := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{audience},
			ExpirationSeconds: ptr.To(int64(provisionerTokenTTL.Seconds())),
		},
	}
	if err := children.SubResource("token").Create(ctx, sa, request); err != nil {
		return "", fmt.Errorf("minting a token for ServiceAccount %s/%s: %w", saNamespace, saName, err)
	}
	return request.Status.Token, nil
}

// now reads the clock through the seam, defaulting to the wall clock.
func (r *BarbicanSecretStoreReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// revokeSession hands a session token back to the server. A failure is logged
// and swallowed: the token expires on its own, and failing the reconcile over it
// would only replace one live token with a second one on the retry.
func (r *BarbicanSecretStoreReconciler) revokeSession(ctx context.Context, c openbao.Client) {
	if err := c.RevokeSelfToken(ctx); err != nil {
		log.FromContext(ctx).Error(err, "revoking the OpenBao session token")
	}
}

// remintRequeue returns the delay after which credentials minted at mintedAt
// reach their proactive re-mint threshold. A TTL of 0 disables that timer (the
// secret ID does not expire, so there is nothing to be early for) and falls back
// to the revalidation cadence: without it, a secret ID revoked on the server
// would sit unnoticed until an unrelated event woke the store, and the login
// probe is what has to fail for the condition to say so. The delay is floored at
// RequeueSecretStoreRetry so a Secret that is already due — or nearly so — still
// requeues on the normal retry cadence instead of hammering the workqueue with a
// zero-length delay.
func (r *BarbicanSecretStoreReconciler) remintRequeue(mintedAt time.Time, ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return RequeueSecretStoreRevalidate
	}
	remaining := mintedAt.Add(proactiveThreshold(ttl)).Sub(r.now())
	if remaining < RequeueSecretStoreRetry {
		return RequeueSecretStoreRetry
	}
	return remaining
}

// proactiveThreshold is the age at which a secret ID is re-minted before it
// expires: two thirds of its TTL. That leaves a full third of the lifetime as
// the margin in which a failing re-mint can be retried at the normal cadence
// while the credential in the pods is still valid.
func proactiveThreshold(ttl time.Duration) time.Duration {
	return ttl * 2 / 3
}

// mintStamp reads the mint annotations back off a credentials Secret. It reports
// stamped false when either annotation is absent or unparseable, which the
// caller treats as "this Secret carries no usable re-mint decision".
func mintStamp(creds *corev1.Secret) (mintedAt time.Time, ttl time.Duration, stamped bool) {
	raw, ok := creds.Annotations[secretIDMintedAtAnnotation]
	if !ok {
		return time.Time{}, 0, false
	}
	mintedAt, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, 0, false
	}
	rawTTL, ok := creds.Annotations[secretIDTTLSecondsAnnotation]
	if !ok {
		return time.Time{}, 0, false
	}
	seconds, err := strconv.ParseInt(rawTTL, 10, 64)
	if err != nil || seconds < 0 {
		return time.Time{}, 0, false
	}
	return mintedAt, time.Duration(seconds) * time.Second, true
}

// missingCapabilities returns the required capabilities the granted set does not
// cover, in the order they are required. A "root" grant covers everything.
func missingCapabilities(granted []string) []string {
	have := make(map[string]bool, len(granted))
	for _, capability := range granted {
		if capability == "root" {
			return nil
		}
		have[capability] = true
	}
	var missing []string
	for _, required := range requiredStoreCapabilities {
		if !have[required] {
			missing = append(missing, required)
		}
	}
	return missing
}

// notProvisionedMessage names the self-init requests a managed instance must
// have run, so the reported gap is actionable against the instance manifest.
func notProvisionedMessage(gap string) string {
	return fmt.Sprintf("%s; the instance must run the self-init requests %s", gap, selfInitRequestNames)
}

// instanceURL derives the API URL of an OpenBaoCluster from its name and
// namespace. It is the SAN the instance's server certificate carries, so it is
// also the only name the TLS handshake accepts.
func instanceURL(instanceName, namespace string) string {
	return fmt.Sprintf("https://%s.%s.svc:%d", instanceName, namespace, instanceAPIPort)
}

// barbicanToSecretStoresMapper returns a MapFunc that fans a Barbican event out
// to every BarbicanSecretStore attached to it, resolved via the
// BarbicanSecretStoreBarbicanRefIndexKey field indexer. On a List error the
// mapper logs and returns nil per the handler.MapFunc contract.
func barbicanToSecretStoresMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		return listStoreRequests(ctx, c, obj.GetNamespace(),
			client.MatchingFields{BarbicanSecretStoreBarbicanRefIndexKey: obj.GetName()},
			"listing BarbicanSecretStores for Barbican watch")
	}
}

// openBaoClusterToSecretStoresMapper returns a MapFunc that fans an
// OpenBaoCluster event out to every managed BarbicanSecretStore provisioning
// against it, resolved via the BarbicanSecretStoreInstanceRefIndexKey field
// indexer. An instance turning Available is what unblocks a store waiting on it.
func openBaoClusterToSecretStoresMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		return listStoreRequests(ctx, c, obj.GetNamespace(),
			client.MatchingFields{BarbicanSecretStoreInstanceRefIndexKey: obj.GetName()},
			"listing BarbicanSecretStores for OpenBaoCluster watch")
	}
}

// listStoreRequests resolves one indexed lookup into reconcile requests, shared
// by both watch mappers.
func listStoreRequests(ctx context.Context, c client.Reader, namespace string, match client.MatchingFields, errMsg string) []reconcile.Request {
	var stores barbicanv1alpha1.BarbicanSecretStoreList
	if err := c.List(ctx, &stores, client.InNamespace(namespace), match); err != nil {
		log.FromContext(ctx).Error(err, errMsg)
		return nil
	}
	requests := make([]reconcile.Request, 0, len(stores.Items))
	for i := range stores.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&stores.Items[i]),
		})
	}
	return requests
}

// storeTargetCluster is the lookup this controller's remote legs gate their
// requests on (see commonmulticluster.RemoteRequests). A BarbicanSecretStore has
// no target field of its own, so the cluster its children belong on is the one
// its parent Barbican names — the same two hops resolveChildren walks for the
// write side. A store whose parent does not exist has no known cluster, and its
// events on every target are dropped until it does.
func storeTargetCluster(c client.Reader) commonmulticluster.TargetClusterFunc {
	return func(ctx context.Context, key client.ObjectKey) (string, error) {
		var store barbicanv1alpha1.BarbicanSecretStore
		if err := c.Get(ctx, key, &store); err != nil {
			return "", err
		}

		var parent barbicanv1alpha1.Barbican
		parentKey := client.ObjectKey{Namespace: key.Namespace, Name: store.Spec.BarbicanRef.Name}
		if err := c.Get(ctx, parentKey, &parent); err != nil {
			return "", err
		}
		if parent.Spec.TargetClusterRef == nil {
			return "", nil
		}
		return parent.Spec.TargetClusterRef.Name, nil
	}
}

// SetupWithManager registers the BarbicanSecretStoreReconciler with the
// controller manager. The BarbicanSecretStore field indexes both mappers resolve
// against are registered by BarbicanReconciler.SetupWithManager (the single
// registration site — both controllers run in one manager and that reconciler is
// set up first), so this controller registers none.
func (r *BarbicanSecretStoreReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	local := mgr.GetLocalManager()

	// Every leg watching the management cluster carries both engage options
	// below; see their definition for why an unpinned leg would stop watching
	// it once a provider is configured.
	engageLocal := commonmulticluster.EngageLocalCluster
	engageNoProviders := commonmulticluster.EngageNoProviderClusters

	// Every leg watching a target cluster is engaged on all of them, not on the
	// ones a store's parent names, so it has to drop the events belonging to a
	// store that projects somewhere else (see commonmulticluster.RemoteRequests).
	targets := storeTargetCluster(local.GetClient())

	b := mcbuilder.ControllerManagedBy(mgr).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate).
		For(&barbicanv1alpha1.BarbicanSecretStore{}, mcbuilder.WithPredicates(watch.CRUpdatePredicate()), engageLocal, engageNoProviders).
		// Watch the referenced Barbican WITHOUT a generation predicate: Barbican
		// status flips (the store config landing in the Deployment) are exactly
		// the wake signal the ConfigProjected gate waits on.
		Watches(&barbicanv1alpha1.Barbican{}, commonmulticluster.LocalRequests(
			barbicanToSecretStoresMapper(local.GetClient()),
		), engageLocal, engageNoProviders)

	// Watch the referenced OpenBaoCluster for the same reason: its Available
	// condition is a status-only transition, and it is what a managed store
	// waiting on WaitingForInstance needs to hear about. The leg is paired onto
	// the clusters a store's parent can project onto because in the hardened
	// topology the instance lives on the target.
	b, err := commonmulticluster.AddInputWatch(b, local.GetScheme(), targets, &openbaov1alpha1.OpenBaoCluster{},
		openBaoClusterToSecretStoresMapper(local.GetClient()))
	if err != nil {
		return err
	}

	// The AppRole credentials Secret is this controller's own remote child: it
	// writes it onto the target cluster, where no owner reference reaches back
	// across the boundary, so the ownership labels map it to the store again.
	b, err = commonmulticluster.AddRemoteChildWatches(b, local.GetScheme(), &barbicanv1alpha1.BarbicanSecretStore{},
		targets, []schema.GroupVersionKind{corev1.SchemeGroupVersion.WithKind("Secret")}, nil)
	if err != nil {
		return err
	}

	return b.
		// The wrapper stays off for the same reason as on the Barbican
		// controller: an unresolvable cluster is surfaced as a condition and
		// requeued, not swallowed into a successful reconcile.
		WithClusterNotFoundWrapper(false).
		Complete(commonmulticluster.LocalReconciler(r))
}
