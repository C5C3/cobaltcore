// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// The two sub-conditions a KeystoneUser carries, plus the aggregate Ready
// derived from them. UserReady reports the Keystone user and its password;
// DeliveryReady reports the OpenBao backup and the Secret beside the order.
const (
	conditionTypeKeystoneUserUserReady     = "UserReady"
	conditionTypeKeystoneUserDeliveryReady = "DeliveryReady"
)

// keystoneUserSubConditionTypes are the sub-conditions the aggregate Ready is
// derived from.
var keystoneUserSubConditionTypes = []string{
	conditionTypeKeystoneUserUserReady,
	conditionTypeKeystoneUserDeliveryReady,
}

// keystoneUserFinalizerName gates the teardown of everything an order created.
const keystoneUserFinalizerName = "c5c3.io/keystoneuser-teardown"

// The ownership labels an order's children carry. No child in the ControlPlane's
// namespace can hold an owner reference to the order: it lives in another
// namespace, and for an order on a target cluster on another cluster as well. The
// cluster label is the third half of the order's identity, because the same
// namespace and name on two clusters are two orders. The management cluster is
// the empty value (c5c3v1alpha1.ManagementCluster).
const (
	keystoneUserNameLabel      = "c5c3.io/keystoneuser-name"
	keystoneUserNamespaceLabel = "c5c3.io/keystoneuser-namespace"
	keystoneUserClusterLabel   = "c5c3.io/keystoneuser-cluster"
)

// keystoneUserLabelKeys are the three ownership label keys as the shared order
// scaffold takes them.
var keystoneUserLabelKeys = orderLabelKeys{
	Name: keystoneUserNameLabel, Namespace: keystoneUserNamespaceLabel, Cluster: keystoneUserClusterLabel,
}

// keystoneUserGVK is the kind the order watch filters target clusters on: a
// cluster that does not serve it is not engaged for the leg.
var keystoneUserGVK = c5c3v1alpha1.GroupVersion.WithKind("KeystoneUser")

// Condition reasons this controller introduces. The admission gates write the
// scaffold's reasons (keystoneorder.go), and the gates it shares with the
// KeystoneService controller reuse that vocabulary instead
// (reasonKeystoneServiceControlPlaneNotFound, reasonWaitingForServiceAccountAdmin,
// reasonServiceAccountStoreNotReady, reasonProbingForCollision,
// reasonServiceAccountCollision, reasonWaitingForServiceAccounts,
// reasonServiceAccountsFailed, reasonServiceAccountError).
const (
	// reasonKeystoneUserProvisioned is UserReady's True reason.
	reasonKeystoneUserProvisioned = "UserProvisioned"
	// reasonKeystoneUserDelivered is DeliveryReady's True reason.
	reasonKeystoneUserDelivered = "Delivered"
	// reasonKeystoneUserBackupNotSynced reports that the PushSecret has not pushed
	// the password to OpenBao yet. Nothing is delivered before it has.
	reasonKeystoneUserBackupNotSynced = "BackupNotSynced"
	// reasonKeystoneUserKeystoneNotPublished reports an order on a cluster that
	// cannot reach the in-cluster Keystone Service while the ControlPlane
	// publishes no public endpoint.
	reasonKeystoneUserKeystoneNotPublished = "KeystoneNotPublished"
	// reasonKeystoneUserDeliveryRefused reports a Secret of the delivered name
	// that the order does not own.
	reasonKeystoneUserDeliveryRefused = "DeliveryRefused"
	// reasonKeystoneUserDeliveryError reports a Kubernetes-level failure writing
	// or reading a delivery object.
	reasonKeystoneUserDeliveryError = "DeliveryError"
)

// KeystoneUserReconciler owns the KeystoneUser lifecycle and is the single writer
// of its status. It is the one reconciler of a CR that may live on a target
// cluster: a request carries the cluster the order was seen on, and the order,
// its status, its finalizer and its delivered Secret are read and written there.
// Everything else it creates lives in the ControlPlane's namespace on the
// management cluster.
//
// It records no Events: the management cluster's recorder would write them into
// a namespace of the wrong cluster for an order on a target cluster, so the
// conditions carry everything.
type KeystoneUserReconciler struct {
	// Client is the management cluster's client.
	client.Client
	Scheme *runtime.Scheme
	// Resolver resolves the cluster an order lives on. It is the multicluster
	// manager in production; nil resolves every request to the management
	// cluster.
	Resolver                commonmulticluster.ClusterResolver
	MaxConcurrentReconciles int
}

// RBAC for the KeystoneUser kind: the controller reads the orders and updates
// them to install and release its finalizer; it never creates or deletes one.
// The kinds it writes in the ControlPlane's namespace (K-ORC users, Secrets,
// PushSecrets) and the ControlPlane reads are granted by the ControlPlane's
// marker block. On a target cluster the target-cluster-access chart's Role for
// an assigned namespace grants the same verbs.
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneusers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneusers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneusers/finalizers,verbs=update

// Reconcile drives one KeystoneUser: the gates, finalizer installation, the
// provision and delivery, and the teardown.
func (r *KeystoneUserReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	cluster := string(req.ClusterName)
	oc, err := orderClient(ctx, r.Resolver, r.Client, cluster)
	if err != nil {
		// The cluster was deregistered between the event and this pass. Nothing
		// can be read or written there, and a requeue would only repeat this.
		log.FromContext(ctx).Info("the cluster the KeystoneUser lives on does not resolve; skipping it",
			"cluster", cluster, "order", req.NamespacedName, "reason", err.Error())
		return ctrl.Result{}, nil
	}

	var order c5c3v1alpha1.KeystoneUser
	if err := oc.Get(ctx, req.NamespacedName, &order); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("KeystoneUser not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching KeystoneUser: %w", err)
	}

	if order.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, oc, &order, cluster)
	}

	statusBefore := order.Status.DeepCopy()
	result, err := r.reconcileNormal(ctx, oc, &order, cluster)
	return r.updateStatus(ctx, oc, &order, statusBefore, result, err)
}

// keystoneUserClusterRef returns the target-cluster ref of cluster, or nil for
// the management cluster.
func keystoneUserClusterRef(cluster string) *commonv1.TargetClusterRefSpec {
	return orderRef{Cluster: cluster}.clusterRef()
}

// keystoneUserLocation names the cluster an order lives on in the phrase the
// namespace-assignment messages share.
func keystoneUserLocation(cluster string) string {
	return orderRef{Cluster: cluster}.location()
}

// reconcileNormal runs the gates, then the provision and the delivery.
//
// The freeze of #1327 D2 is the assignment gate of orderAdmission: without an
// entry the pass writes NamespaceNotAssigned and returns before anything is read
// or written in the ControlPlane's namespace, so nothing is projected,
// delivered, repaired or swept.
//
// The finalizer is installed only past that gate. An order the operator does not
// serve has nothing to tear down, and in a namespace that is not assigned to it
// on a target cluster the operator is not granted the update anyway.
func (r *KeystoneUserReconciler) reconcileNormal(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.KeystoneUser, cluster string,
) (ctrl.Result, error) {
	failBoth := func(reason, message string) { keystoneUserFailBoth(order, reason, message) }
	cp, _, result, err := orderAdmission(ctx, r.Client, keystoneUserRef(order, cluster),
		order.Spec.ControlPlaneRef, failBoth)
	if err != nil || cp == nil {
		return result, err
	}

	if added, err := commonreconcile.EnsureFinalizer(ctx, oc, order, keystoneUserFinalizerName); err != nil {
		return ctrl.Result{}, err
	} else if added {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
	}

	if !orderAdminCredentialGate(cp, failBoth) {
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	provisioned := false
	result, err = instrumenter.Instrument(ctx, "KeystoneUserProvision", func(ctx context.Context) (ctrl.Result, error) {
		ok, res, err := r.provisionUser(ctx, order, cp, cluster)
		provisioned = ok
		return res, err
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	if !provisioned {
		keystoneUserFail(order, conditionTypeKeystoneUserDeliveryReady)(reasonWaitingForServiceAccounts,
			"the user is not provisioned yet; nothing is delivered")
		return keystoneUserPassResult(order, cluster, result), nil
	}

	result, err = instrumenter.Instrument(ctx, "KeystoneUserDelivery", func(ctx context.Context) (ctrl.Result, error) {
		return r.deliverCredentials(ctx, oc, order, cp, cluster, order.Status.PasswordGeneration)
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	return keystoneUserPassResult(order, cluster, result), nil
}

// keystoneUserPassResult decides when the next pass runs once both legs had
// their say, with DeliveryReady as the converged condition (orderPassResult). A
// refusal only an edit outside the order's own watches lifts (an unpublished
// Keystone, a Secret somebody else owns, the admin identity) comes back on the
// refresh.
func keystoneUserPassResult(order *c5c3v1alpha1.KeystoneUser, cluster string, result ctrl.Result) ctrl.Result {
	return orderPassResult(cluster,
		conditions.AllTrue(order.Status.Conditions, conditionTypeKeystoneUserDeliveryReady), result)
}

// keystoneUserControlPlaneKey resolves the order's controlPlaneRef, the
// namespace defaulting to the order's own.
func keystoneUserControlPlaneKey(order *c5c3v1alpha1.KeystoneUser) client.ObjectKey {
	return orderControlPlaneKey(order.Spec.ControlPlaneRef, order.Namespace)
}

// keystoneUserFail returns a closure bound to order and condType that writes a
// False condition. The message is truncated, because it relays K-ORC's own text
// and one over-long message would make the whole status write fail.
func keystoneUserFail(order *c5c3v1alpha1.KeystoneUser, condType string) func(reason, message string) {
	return func(reason, message string) {
		setTruncatedCondition(&order.Status.Conditions, order.Generation, condType, metav1.ConditionFalse, reason, message)
	}
}

// keystoneUserFailBoth writes one gate failure onto both sub-conditions.
func keystoneUserFailBoth(order *c5c3v1alpha1.KeystoneUser, reason, message string) {
	for _, condType := range keystoneUserSubConditionTypes {
		keystoneUserFail(order, condType)(reason, message)
	}
}

// keystoneUserSetTrue writes a sub-condition as True, truncating the message
// for the reason keystoneUserFail does.
func keystoneUserSetTrue(order *c5c3v1alpha1.KeystoneUser, condType, reason, message string) {
	setTruncatedCondition(&order.Status.Conditions, order.Generation, condType, metav1.ConditionTrue, reason, message)
}

// updateStatus persists the status through the order's cluster client: the write
// is skipped when the pass changed nothing, the aggregate Ready is re-derived on
// every persist, and ObservedGeneration is set.
func (r *KeystoneUserReconciler) updateStatus(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.KeystoneUser,
	statusBefore *c5c3v1alpha1.KeystoneUserStatus, result ctrl.Result, reconcileErr error,
) (ctrl.Result, error) {
	return commonreconcile.UpdateStatus(ctx, oc, order, statusBefore, &order.Status, func() {
		commonreconcile.SetAggregateReady(&order.Status.Conditions, order.Generation, keystoneUserSubConditionTypes)
		order.Status.ObservedGeneration = order.Generation
	}, result, reconcileErr)
}

// --- naming ---

// keystoneUserRef identifies the order to the shared scaffold: its children
// carry the "user" segment and the keystoneuser labels.
func keystoneUserRef(order *c5c3v1alpha1.KeystoneUser, cluster string) orderRef {
	return orderRef{
		Name: order.Name, Namespace: order.Namespace, Cluster: cluster,
		Segment: "user", Keys: keystoneUserLabelKeys,
	}
}

// keystoneUserChildPrefix scopes every child an order creates in the
// ControlPlane's namespace, and every sweep that removes one (orderRef.childPrefix).
func keystoneUserChildPrefix(order *c5c3v1alpha1.KeystoneUser, cluster string) string {
	return keystoneUserRef(order, cluster).childPrefix()
}

func keystoneUserUserRef(order *c5c3v1alpha1.KeystoneUser, cluster string) string {
	return keystoneUserChildPrefix(order, cluster) + "user"
}

func keystoneUserUserProbeRef(order *c5c3v1alpha1.KeystoneUser, cluster string) string {
	return keystoneUserChildPrefix(order, cluster) + "user-probe"
}

// keystoneUserPasswordSecretPrefix is the prefix every generation's password
// Secret shares; the teardown and the superseded-generation prune match on it.
func keystoneUserPasswordSecretPrefix(order *c5c3v1alpha1.KeystoneUser, cluster string) string {
	return keystoneUserChildPrefix(order, cluster) + "password-v"
}

func keystoneUserPasswordSecretName(order *c5c3v1alpha1.KeystoneUser, cluster string, gen int64) string {
	return fmt.Sprintf("%s%d", keystoneUserPasswordSecretPrefix(order, cluster), gen)
}

func keystoneUserSourceSecretName(order *c5c3v1alpha1.KeystoneUser, cluster string) string {
	return keystoneUserChildPrefix(order, cluster) + "source"
}

func keystoneUserPushSecretName(order *c5c3v1alpha1.KeystoneUser, cluster string) string {
	return keystoneUserChildPrefix(order, cluster) + "backup"
}

// keystoneUserCredentialsSecretSuffix is the tail of the delivered Secret's
// name. It is part of the KeystoneUser API, apart from the KeystoneService
// suffix of the same spelling, so neither kind's contract moves with the other.
const keystoneUserCredentialsSecretSuffix = "-credentials"

// keystoneUserCredentialsSecretName is the delivered Secret's name, in the
// order's namespace. It carries no prefix: it is the contract the owner reads
// the credentials from, predictable from the order's name alone.
func keystoneUserCredentialsSecretName(order *c5c3v1alpha1.KeystoneUser) string {
	return order.Name + keystoneUserCredentialsSecretSuffix
}

// keystoneUserRemoteKeyFor returns the OpenBao path the order's credentials are
// backed up to. The PushSecret writing it lives in the ControlPlane's namespace
// and pushes through that namespace's own store, so the path sits under that
// namespace, which is what the eso-tenant policy grants the store
// (openstack/keystone/{store namespace}/+/service-accounts/+). The prefix fills
// the segment in between, so no two orders share a path.
func keystoneUserRemoteKeyFor(cp *c5c3v1alpha1.ControlPlane, prefix string) string {
	return "openstack/keystone/" + cp.Namespace + "/" + strings.TrimSuffix(prefix, "-") + "/service-accounts/credentials"
}

// keystoneUserName resolves the Keystone user name, defaulting to the order's
// own name. No defaulting webhook exists for the kind, so the reconciler is the
// only place the default is resolved.
func keystoneUserName(order *c5c3v1alpha1.KeystoneUser) string {
	return cmp.Or(order.Spec.UserName, order.Name)
}

// keystoneUserPasswordGeneration resolves the declared password generation. A
// zero, which only a CR stored without the schema default carries, reads as 1.
func keystoneUserPasswordGeneration(order *c5c3v1alpha1.KeystoneUser) int64 {
	return max(order.Spec.PasswordGeneration, 1)
}

// --- ownership ---

// keystoneUserChildLabels returns the three ownership labels of the order's
// children in the ControlPlane's namespace.
func keystoneUserChildLabels(order *c5c3v1alpha1.KeystoneUser, cluster string) map[string]string {
	return keystoneUserRef(order, cluster).childLabels()
}

// ownsKeystoneUserChild reports whether obj is a child the order created in the
// ControlPlane's namespace (ownsOrderChild).
func ownsKeystoneUserChild(obj client.Object, order *c5c3v1alpha1.KeystoneUser, cluster string) bool {
	return ownsOrderChild(obj, order, keystoneUserRef(order, cluster))
}

// claimKeystoneUserChild sets the order's labels on obj (claimOrderChild).
func claimKeystoneUserChild(obj client.Object, order *c5c3v1alpha1.KeystoneUser, cluster string) {
	claimOrderChild(obj, keystoneUserRef(order, cluster))
}

// ensureKeystoneUserChild applies a child in the ControlPlane's namespace,
// refusing one the order did not create (ensureOrderChild).
func (r *KeystoneUserReconciler) ensureKeystoneUserChild(
	ctx context.Context, order *c5c3v1alpha1.KeystoneUser, cluster string, obj client.Object,
) error {
	return ensureOrderChild(ctx, r.Client, r.Scheme, order, keystoneUserRef(order, cluster), obj)
}

// keystoneUserEnsure returns the ensure seam the shared projection helpers take,
// bound to the order (orderEnsure).
func (r *KeystoneUserReconciler) keystoneUserEnsure(order *c5c3v1alpha1.KeystoneUser, cluster string) registrationEnsure {
	return orderEnsure(r.Client, r.Scheme, order, keystoneUserRef(order, cluster))
}

// ensureKeystoneUserSecret create-or-updates a labelled Secret in the
// ControlPlane's namespace. It stays read-modify-write because mutate reads the
// live data: a generated password is preserved across passes.
func (r *KeystoneUserReconciler) ensureKeystoneUserSecret(
	ctx context.Context, order *c5c3v1alpha1.KeystoneUser, cluster, name, namespace string,
	mutate func(*corev1.Secret) error,
) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		if err := mutate(secret); err != nil {
			return err
		}
		claimKeystoneUserChild(secret, order, cluster)
		return nil
	})
	return err
}

// --- teardown ---

// reconcileDelete removes everything the order created and releases the
// finalizer through orderTeardown: K-ORC's finalizer takes the user out of
// Keystone and ESO's takes the password out of OpenBao.
func (r *KeystoneUserReconciler) reconcileDelete(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.KeystoneUser, cluster string,
) (ctrl.Result, error) {
	return orderTeardown(ctx, r.Client, oc, order, keystoneUserFinalizerName, keystoneUserControlPlaneKey(order),
		func(ctx context.Context, childNS string) (int, error) {
			return r.sweepKeystoneUserChildren(ctx, oc, order, cluster, childNS)
		})
}

// sweepKeystoneUserChildren issues the deletes of every child the order owns and
// reports how many are still listed.
//
// The deletes run in this order: the PushSecret (its deletionPolicy removes the
// OpenBao path), the managed User (K-ORC deletes the Keystone user), the probe,
// the password Secrets and the source Secret, all in childNS on the management
// cluster, then the delivered Secret beside the order. The garbage collector
// would reap that one too; deleting it here makes the teardown observable
// without waiting on it.
func (r *KeystoneUserReconciler) sweepKeystoneUserChildren(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.KeystoneUser, cluster, childNS string,
) (int, error) {
	ref := keystoneUserRef(order, cluster)
	userRef := keystoneUserUserRef(order, cluster)
	passwordPrefix := keystoneUserPasswordSecretPrefix(order, cluster)
	remaining := 0
	for _, leg := range []struct {
		list  client.ObjectList
		first func(client.Object) bool
	}{
		{list: &esov1alpha1.PushSecretList{}},
		// The managed User first, then the probe, both of the one kind.
		{list: &orcv1alpha1.UserList{}, first: func(obj client.Object) bool { return obj.GetName() == userRef }},
		// The password Secrets, then the source Secret.
		{list: &corev1.SecretList{}, first: func(obj client.Object) bool {
			return strings.HasPrefix(obj.GetName(), passwordPrefix)
		}},
	} {
		n, err := sweepOrderList(ctx, r.Client, order, ref, childNS, leg.list, leg.first)
		if err != nil {
			return 0, err
		}
		remaining += n
	}

	delivered := &corev1.Secret{}
	deliveredKey := types.NamespacedName{Namespace: order.Namespace, Name: keystoneUserCredentialsSecretName(order)}
	switch err := oc.Get(ctx, deliveredKey, delivered); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return 0, fmt.Errorf("reading delivered Secret %s: %w", deliveredKey, err)
	case metav1.IsControlledBy(delivered, order):
		log.FromContext(ctx).Info("removing the delivered KeystoneUser Secret", "secret", deliveredKey)
		if err := client.IgnoreNotFound(oc.Delete(ctx, delivered)); err != nil {
			return 0, fmt.Errorf("deleting delivered Secret %s: %w", deliveredKey, err)
		}
		remaining++
	}
	return remaining, nil
}

// --- manager setup ---

// KeystoneUserControlPlaneRefIndexKey is the field-indexer key under which a
// KeystoneUser on the management cluster is indexed by its resolved ControlPlane
// reference, "<namespace>/<name>".
const KeystoneUserControlPlaneRefIndexKey = "spec.controlPlaneRef"

// keystoneUserControlPlaneRefExtractor returns the resolved "<namespace>/<name>"
// of obj's ControlPlane reference, in the encoding the KeystoneService index
// shares. An empty name indexes nothing.
func keystoneUserControlPlaneRefExtractor(obj client.Object) []string {
	order, ok := obj.(*c5c3v1alpha1.KeystoneUser)
	if !ok || order.Spec.ControlPlaneRef.Name == "" {
		return nil
	}
	key := keystoneUserControlPlaneKey(order)
	return []string{keystoneServiceControlPlaneRefIndexValue(key.Namespace, key.Name)}
}

// registerKeystoneUserControlPlaneRefIndex registers the field indexer
// controlPlaneToKeystoneUsersMapper relies on.
func registerKeystoneUserControlPlaneRefIndex(ctx context.Context, indexer client.FieldIndexer) error {
	return indexer.IndexField(ctx, &c5c3v1alpha1.KeystoneUser{},
		KeystoneUserControlPlaneRefIndexKey, keystoneUserControlPlaneRefExtractor)
}

// controlPlaneToKeystoneUsersMapper maps a ControlPlane event to the orders on
// the management cluster that reference it. Orders on target clusters are not in
// the local cache it lists; they come back on keystoneUserRefreshAfter. A List
// failure is logged and maps to nothing.
func controlPlaneToKeystoneUsersMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var orders c5c3v1alpha1.KeystoneUserList
		if err := c.List(ctx, &orders, client.MatchingFields{
			KeystoneUserControlPlaneRefIndexKey: keystoneServiceControlPlaneRefIndexValue(obj.GetNamespace(), obj.GetName()),
		}); err != nil {
			log.FromContext(ctx).Error(err, "listing KeystoneUsers for ControlPlane watch")
			return nil
		}
		requests := make([]reconcile.Request, 0, len(orders.Items))
		for i := range orders.Items {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&orders.Items[i])})
		}
		return requests
	}
}

// SetupWithManager registers the KeystoneUserReconciler with the multicluster
// manager.
func (r *KeystoneUserReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return r.setupWithOptions(mgr, bootstrap.TypedControllerOptions[mcreconcile.Request](r.MaxConcurrentReconciles))
}

// setupWithOptions carries the production setup SetupWithManager applies, with
// the controller options as a parameter so the integration suites register this
// chain with SkipNameValidation set.
//
// The watches:
//   - For: the order on the management cluster and on every engaged target
//     cluster that serves the kind. ClusterServesKind keeps a target without the
//     CRD engaged for every other watch; a CRD installed later is watched once
//     the cluster is engaged again.
//   - Owns: the delivered Secret beside the order, on the same clusters. An edit
//     or a deletion brings the order back with the event's cluster.
//   - The User, Secret and PushSecret children in the ControlPlane's namespace,
//     mapped back by their labels.
//   - ControlPlane: the orders on the management cluster that reference it,
//     on the updates orderControlPlanePredicate passes.
//
// The index goes on the local field indexer: with a provider configured, the
// multicluster manager's indexer would register against every target cluster as
// well, and a target cluster without the kind would fail its engagement.
func (r *KeystoneUserReconciler) setupWithOptions(mgr mcmanager.Manager, opts crcontroller.TypedOptions[mcreconcile.Request]) error {
	local := mgr.GetLocalManager()
	if err := registerKeystoneUserControlPlaneRefIndex(context.Background(), local.GetFieldIndexer()); err != nil {
		return err
	}
	engageLocal := commonmulticluster.EngageLocalCluster
	engageNoProviders := commonmulticluster.EngageNoProviderClusters
	engageProviders := mcbuilder.WithEngageWithProviderClusters(true)
	servesKind := mcbuilder.WithClusterFilter(commonmulticluster.ClusterServesKind(keystoneUserGVK))
	children := orderChildRequests(keystoneUserLabelKeys)

	return mcbuilder.ControllerManagedBy(mgr).
		WithOptions(opts).
		// Filter the order's own status-only updates so the status write does not
		// wake the controller again (see watch.CRUpdatePredicate).
		For(&c5c3v1alpha1.KeystoneUser{}, mcbuilder.WithPredicates(watch.CRUpdatePredicate()),
			engageLocal, engageProviders, servesKind).
		Owns(&corev1.Secret{}, engageLocal, engageProviders, servesKind).
		Watches(&orcv1alpha1.User{}, children, engageLocal, engageNoProviders).
		Watches(&corev1.Secret{}, children, engageLocal, engageNoProviders).
		Watches(&esov1alpha1.PushSecret{}, children, engageLocal, engageNoProviders).
		Watches(&c5c3v1alpha1.ControlPlane{},
			commonmulticluster.LocalRequests(controlPlaneToKeystoneUsersMapper(local.GetClient())),
			mcbuilder.WithPredicates(orderControlPlanePredicate()), engageLocal, engageNoProviders).
		// Reconcile answers an unresolvable cluster itself, so the wrapper that
		// would turn a cluster-not-found error into a success stays off.
		WithClusterNotFoundWrapper(false).
		Complete(r)
}
