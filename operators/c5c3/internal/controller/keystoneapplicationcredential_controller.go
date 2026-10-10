// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// The two sub-conditions a KeystoneApplicationCredential carries, plus the
// aggregate Ready derived from them. CredentialReady reports the mint
// document, the credential generations and the schedule; DeliveryReady reports
// the OpenBao backup and the Secret beside the order.
const (
	conditionTypeKeystoneApplicationCredentialCredentialReady = "CredentialReady"
	conditionTypeKeystoneApplicationCredentialDeliveryReady   = "DeliveryReady"
)

// keystoneApplicationCredentialSubConditionTypes are the sub-conditions the
// aggregate Ready is derived from.
var keystoneApplicationCredentialSubConditionTypes = []string{
	conditionTypeKeystoneApplicationCredentialCredentialReady,
	conditionTypeKeystoneApplicationCredentialDeliveryReady,
}

// keystoneApplicationCredentialFinalizerName gates the teardown of everything
// an order created.
const keystoneApplicationCredentialFinalizerName = "c5c3.io/keystoneapplicationcredential-teardown"

// keystoneApplicationCredentialLabelKeys are the ownership labels an order's
// children carry, for the reason keystoneUserLabelKeys gives.
var keystoneApplicationCredentialLabelKeys = orderLabelKeys{
	Name:      "c5c3.io/keystoneapplicationcredential-name",
	Namespace: "c5c3.io/keystoneapplicationcredential-namespace",
	Cluster:   "c5c3.io/keystoneapplicationcredential-cluster",
}

// keystoneApplicationCredentialGVK is the kind the order watch filters target
// clusters on, and the kind the user, project and role assignment holds watch.
var keystoneApplicationCredentialGVK = c5c3v1alpha1.GroupVersion.WithKind("KeystoneApplicationCredential")

// The rotation defaults the schema applies. The reconciler resolves them for an
// object stored without them, because the kind has no webhook.
const (
	defaultApplicationCredentialRotationInterval    = 720 * time.Hour
	defaultApplicationCredentialRotationGracePeriod = 24 * time.Hour
)

// Condition reasons this controller introduces. The reference gates reuse the
// KeystoneRoleAssignment vocabulary (reasonKeystoneRoleAssignmentUserNotFound,
// reasonKeystoneRoleAssignmentProjectNotFound,
// reasonKeystoneRoleAssignmentControlPlaneMismatch,
// reasonKeystoneRoleAssignmentWaitingForUser, reasonKeystoneProjectWaiting),
// the delivery the KeystoneUser one (reasonKeystoneUserDelivered,
// reasonKeystoneUserBackupNotSynced, reasonKeystoneUserKeystoneNotPublished,
// reasonKeystoneUserDeliveryRefused, reasonKeystoneUserDeliveryError), and the
// admission gates the scaffold's.
const (
	// reasonKeystoneApplicationCredentialMinted is CredentialReady's True
	// reason, also while a successor is being minted.
	reasonKeystoneApplicationCredentialMinted = "CredentialMinted"
	// reasonKeystoneApplicationCredentialNoRoleOnProject reports that no
	// KeystoneRoleAssignment in the namespace has assigned the user a role on the
	// project.
	reasonKeystoneApplicationCredentialNoRoleOnProject = "NoRoleOnProject"
	// reasonKeystoneApplicationCredentialWaiting reports a credential K-ORC has
	// not made Available yet, and on DeliveryReady that nothing is minted yet.
	reasonKeystoneApplicationCredentialWaiting = "WaitingForCredential"
	// reasonKeystoneApplicationCredentialFailed reports a terminal K-ORC error
	// on a credential generation.
	reasonKeystoneApplicationCredentialFailed = "CredentialFailed"
	// reasonKeystoneApplicationCredentialWaitingForCABundle reports a Keystone
	// CA bundle the mint document cannot carry yet.
	reasonKeystoneApplicationCredentialWaitingForCABundle = "WaitingForCABundle"
	// reasonKeystoneApplicationCredentialError reports a Kubernetes-level failure
	// writing or reading the credential's children.
	reasonKeystoneApplicationCredentialError = "CredentialError"
)

// KeystoneApplicationCredentialReconciler owns the
// KeystoneApplicationCredential lifecycle and is the single writer of its
// status. It serves orders on the management cluster and on target clusters
// the way the KeystoneUser reconciler does: the order, its status, its
// finalizer and its delivered Secret are read and written on the order's
// cluster, and every other child lives in the ControlPlane's namespace on the
// management cluster. It records no Events, for the reason the KeystoneUser
// reconciler gives.
type KeystoneApplicationCredentialReconciler struct {
	// Client is the management cluster's client.
	client.Client
	Scheme *runtime.Scheme
	// Resolver resolves the cluster an order lives on; nil resolves every
	// request to the management cluster.
	Resolver                commonmulticluster.ClusterResolver
	MaxConcurrentReconciles int
	// Now is the clock the schedule reads; nil means time.Now.
	Now func() time.Time
}

// RBAC for the KeystoneApplicationCredential kind: the controller reads the
// orders and updates them to install and release its finalizer; it never
// creates or deletes one. It reads the KeystoneUser, KeystoneProject and
// KeystoneRoleAssignment orders the gates consult, which the markers of those
// kinds grant. The K-ORC ApplicationCredentials, Secrets and PushSecrets it
// writes in the ControlPlane's namespace are granted by the ControlPlane's
// marker block.
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneapplicationcredentials,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneapplicationcredentials/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneapplicationcredentials/finalizers,verbs=update

// Reconcile drives one KeystoneApplicationCredential: the gates, finalizer
// installation, the mint, the rotation, the delivery and the teardown.
func (r *KeystoneApplicationCredentialReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	cluster := string(req.ClusterName)
	oc, err := orderClient(ctx, r.Resolver, r.Client, cluster)
	if err != nil {
		// The cluster was deregistered between the event and this pass. Nothing
		// can be read or written there, and a requeue would only repeat this.
		log.FromContext(ctx).Info("the cluster the KeystoneApplicationCredential lives on does not resolve; skipping it",
			"cluster", cluster, "order", req.NamespacedName, "reason", err.Error())
		return ctrl.Result{}, nil
	}

	var order c5c3v1alpha1.KeystoneApplicationCredential
	if err := oc.Get(ctx, req.NamespacedName, &order); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("KeystoneApplicationCredential not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching KeystoneApplicationCredential: %w", err)
	}

	if order.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, oc, &order, cluster)
	}

	statusBefore := order.Status.DeepCopy()
	result, err := r.reconcileNormal(ctx, oc, &order, cluster)
	return r.updateStatus(ctx, oc, &order, statusBefore, result, err)
}

// now reads the reconciler's clock, truncated to the second a status time
// keeps once it is written.
func (r *KeystoneApplicationCredentialReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().Truncate(time.Second)
	}
	return time.Now().Truncate(time.Second)
}

// reconcileNormal runs the shared gates, the three reference gates, then the
// provision and the delivery.
//
// The freeze of #1327 D2 is the assignment gate of orderAdmission: without an
// entry the pass returns before anything is read or written in the
// ControlPlane's namespace, so nothing is minted, rotated, delivered, repaired
// or swept, and the schedule pauses. The reference gates run on every pass, so
// a reference that goes away after the mint pauses the schedule as well while
// the delivered Secret stays.
func (r *KeystoneApplicationCredentialReconciler) reconcileNormal(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.KeystoneApplicationCredential, cluster string,
) (ctrl.Result, error) {
	failBoth := func(reason, message string) { keystoneApplicationCredentialFailBoth(order, reason, message) }
	cp, _, result, err := orderAdmission(ctx, r.Client, keystoneApplicationCredentialRef(order, cluster),
		order.Spec.ControlPlaneRef, failBoth)
	if err != nil || cp == nil {
		return result, err
	}

	if added, err := commonreconcile.EnsureFinalizer(ctx, oc, order, keystoneApplicationCredentialFinalizerName); err != nil {
		return ctrl.Result{}, err
	} else if added {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
	}

	if !orderAdminCredentialGate(cp, failBoth) {
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	user, project, admitted, err := r.referenceGates(ctx, oc, order, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !admitted {
		// A delivered credential stays valid while a reference is refused, so
		// DeliveryReady is only written before the first mint.
		if order.Status.CredentialID == "" {
			keystoneApplicationCredentialFail(order, conditionTypeKeystoneApplicationCredentialDeliveryReady)(
				reasonKeystoneApplicationCredentialWaiting, "the credential is not minted yet; nothing is delivered")
		}
		return orderPassResult(cluster, false, ctrl.Result{RequeueAfter: orderRefreshAfter}), nil
	}

	provisionResult, err := instrumenter.Instrument(ctx, "KeystoneApplicationCredentialProvision",
		func(ctx context.Context) (ctrl.Result, error) {
			return r.provisionCredential(ctx, order, cp, cluster, user, project)
		})
	if err != nil {
		return ctrl.Result{}, err
	}

	deliveryResult, err := instrumenter.Instrument(ctx, "KeystoneApplicationCredentialDelivery",
		func(ctx context.Context) (ctrl.Result, error) {
			return r.deliverCredential(ctx, oc, order, cp, cluster)
		})
	if err != nil {
		return ctrl.Result{}, err
	}
	return keystoneApplicationCredentialPassResult(order, cluster,
		commonreconcile.ShortestRequeue(provisionResult, deliveryResult)), nil
}

// keystoneApplicationCredentialPassResult decides when the next pass runs once
// both legs had their say, with DeliveryReady as the converged condition
// (orderPassResult). The legs' requeue, which carries the rotation timers, is
// kept when it is the shorter one: a converged order on the management cluster
// waits for its timer or an event, and every other order comes back on
// orderRefreshAfter at the latest.
func keystoneApplicationCredentialPassResult(
	order *c5c3v1alpha1.KeystoneApplicationCredential, cluster string, result ctrl.Result,
) ctrl.Result {
	converged := conditions.AllTrue(order.Status.Conditions, conditionTypeKeystoneApplicationCredentialDeliveryReady)
	return commonreconcile.ShortestRequeue(orderPassResult(cluster, converged, ctrl.Result{}), result)
}

// referenceGates loads the KeystoneUser and the KeystoneProject the order
// names and reports whether the order may mint with them: both pass
// roleAssignmentReference.gate, and a KeystoneRoleAssignment in the namespace
// has assigned the user a role on the project. A refusal is written on
// CredentialReady. A read error other than NotFound is returned wrapped.
func (r *KeystoneApplicationCredentialReconciler) referenceGates(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.KeystoneApplicationCredential, cluster string,
) (*c5c3v1alpha1.KeystoneUser, *c5c3v1alpha1.KeystoneProject, bool, error) {
	fail := keystoneApplicationCredentialFail(order, conditionTypeKeystoneApplicationCredentialCredentialReady)
	user := &c5c3v1alpha1.KeystoneUser{}
	project := &c5c3v1alpha1.KeystoneProject{}
	cpKey := orderControlPlaneKey(order.Spec.ControlPlaneRef, order.Namespace)
	location := keystoneApplicationCredentialRef(order, cluster).location()
	for _, reference := range userAndProjectReferences(order.Spec.UserRef.Name, order.Spec.ProjectRef.Name, user, project) {
		admitted, err := reference.gate(ctx, oc, order.Namespace, location, cpKey, fail)
		if err != nil || !admitted {
			return nil, nil, false, err
		}
	}

	assigned, err := roleAssignedOnProject(ctx, oc, order)
	if err != nil {
		return nil, nil, false, err
	}
	if !assigned {
		fail(reasonKeystoneApplicationCredentialNoRoleOnProject, fmt.Sprintf(
			"no KeystoneRoleAssignment in namespace %q has assigned a role to user %q on project %q; the credential "+
				"is minted with a project-scoped token, which needs a role on the project",
			order.Namespace, order.Spec.UserRef.Name, order.Spec.ProjectRef.Name))
		return nil, nil, false, nil
	}
	return user, project, true, nil
}

// roleAssignedOnProject reports whether a KeystoneRoleAssignment in the order's
// namespace binds the order's user and project and K-ORC has reported the role
// it assigned (status.roleID). The role id is read rather than
// AssignmentReady: a frozen assignment keeps its role in Keystone. A cluster
// that does not serve the kind has none.
func roleAssignedOnProject(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.KeystoneApplicationCredential,
) (bool, error) {
	// The items are only read, so the cache's objects are listed without a copy.
	var list c5c3v1alpha1.KeystoneRoleAssignmentList
	if err := oc.List(ctx, &list, client.InNamespace(order.Namespace), client.UnsafeDisableDeepCopy); err != nil {
		if meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, fmt.Errorf("listing KeystoneRoleAssignments in %q: %w", order.Namespace, err)
	}
	return slices.ContainsFunc(list.Items, func(ra c5c3v1alpha1.KeystoneRoleAssignment) bool {
		return roleAssignmentBindsCredentialPair(&ra, order) && ra.Status.RoleID != ""
	}), nil
}

// roleAssignmentBindsCredentialPair reports whether ra assigns a role to the
// user on the project the credential order ac names. The role gate, the
// assignment's deletion hold and the watches between the two kinds all match by
// it.
func roleAssignmentBindsCredentialPair(
	ra *c5c3v1alpha1.KeystoneRoleAssignment, ac *c5c3v1alpha1.KeystoneApplicationCredential,
) bool {
	return ra.Spec.UserRef.Name == ac.Spec.UserRef.Name && ra.Spec.ProjectRef.Name == ac.Spec.ProjectRef.Name
}

// keystoneApplicationCredentialFail returns a closure bound to order and
// condType that writes a False condition, truncating the message for the reason
// keystoneUserFail gives.
func keystoneApplicationCredentialFail(
	order *c5c3v1alpha1.KeystoneApplicationCredential, condType string,
) func(reason, message string) {
	return func(reason, message string) {
		setTruncatedCondition(&order.Status.Conditions, order.Generation, condType, metav1.ConditionFalse, reason, message)
	}
}

// keystoneApplicationCredentialFailBoth writes one gate failure onto both
// sub-conditions.
func keystoneApplicationCredentialFailBoth(order *c5c3v1alpha1.KeystoneApplicationCredential, reason, message string) {
	for _, condType := range keystoneApplicationCredentialSubConditionTypes {
		keystoneApplicationCredentialFail(order, condType)(reason, message)
	}
}

// keystoneApplicationCredentialSetTrue writes a sub-condition as True.
func keystoneApplicationCredentialSetTrue(
	order *c5c3v1alpha1.KeystoneApplicationCredential, condType, reason, message string,
) {
	setTruncatedCondition(&order.Status.Conditions, order.Generation, condType, metav1.ConditionTrue, reason, message)
}

// updateStatus persists the status through the order's cluster client, as the
// KeystoneUser reconciler's does.
func (r *KeystoneApplicationCredentialReconciler) updateStatus(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.KeystoneApplicationCredential,
	statusBefore *c5c3v1alpha1.KeystoneApplicationCredentialStatus, result ctrl.Result, reconcileErr error,
) (ctrl.Result, error) {
	return commonreconcile.UpdateStatus(ctx, oc, order, statusBefore, &order.Status, func() {
		commonreconcile.SetAggregateReady(&order.Status.Conditions, order.Generation,
			keystoneApplicationCredentialSubConditionTypes)
		order.Status.ObservedGeneration = order.Generation
	}, result, reconcileErr)
}

// --- naming ---

// keystoneApplicationCredentialRef identifies the order to the shared
// scaffold: its children carry the "applicationcredential" segment and the
// keystoneapplicationcredential labels.
func keystoneApplicationCredentialRef(order *c5c3v1alpha1.KeystoneApplicationCredential, cluster string) orderRef {
	return orderRef{
		Name: order.Name, Namespace: order.Namespace, Cluster: cluster,
		Segment: "applicationcredential", Keys: keystoneApplicationCredentialLabelKeys,
	}
}

// keystoneApplicationCredentialChildPrefix scopes every child an order creates
// in the ControlPlane's namespace (orderRef.childPrefix).
func keystoneApplicationCredentialChildPrefix(order *c5c3v1alpha1.KeystoneApplicationCredential, cluster string) string {
	return keystoneApplicationCredentialRef(order, cluster).childPrefix()
}

// keystoneApplicationCredentialMintCloudName is the Secret holding the
// password clouds.yaml K-ORC mints and deletes every credential generation
// with, authenticated as the ordered user.
func keystoneApplicationCredentialMintCloudName(order *c5c3v1alpha1.KeystoneApplicationCredential, cluster string) string {
	return keystoneApplicationCredentialChildPrefix(order, cluster) + "mint-cloud"
}

// keystoneApplicationCredentialSecretPrefix is the prefix every generation's
// secret Secret shares.
func keystoneApplicationCredentialSecretPrefix(order *c5c3v1alpha1.KeystoneApplicationCredential, cluster string) string {
	return keystoneApplicationCredentialChildPrefix(order, cluster) + "secret-v"
}

// keystoneApplicationCredentialSecretName is the Secret K-ORC reads the secret
// of generation gen from (key value).
func keystoneApplicationCredentialSecretName(
	order *c5c3v1alpha1.KeystoneApplicationCredential, cluster string, gen int64,
) string {
	return keystoneApplicationCredentialSecretPrefix(order, cluster) + strconv.FormatInt(gen, 10)
}

// keystoneApplicationCredentialCredentialPrefix is the prefix every
// generation's K-ORC ApplicationCredential shares.
func keystoneApplicationCredentialCredentialPrefix(
	order *c5c3v1alpha1.KeystoneApplicationCredential, cluster string,
) string {
	return keystoneApplicationCredentialChildPrefix(order, cluster) + "credential-v"
}

// keystoneApplicationCredentialCredentialName is the K-ORC
// ApplicationCredential of generation gen. K-ORC names the Keystone credential
// after it.
func keystoneApplicationCredentialCredentialName(
	order *c5c3v1alpha1.KeystoneApplicationCredential, cluster string, gen int64,
) string {
	return keystoneApplicationCredentialCredentialPrefix(order, cluster) + strconv.FormatInt(gen, 10)
}

func keystoneApplicationCredentialSourceSecretName(order *c5c3v1alpha1.KeystoneApplicationCredential, cluster string) string {
	return keystoneApplicationCredentialChildPrefix(order, cluster) + "source"
}

func keystoneApplicationCredentialPushSecretName(order *c5c3v1alpha1.KeystoneApplicationCredential, cluster string) string {
	return keystoneApplicationCredentialChildPrefix(order, cluster) + "backup"
}

// keystoneApplicationCredentialCredentialsSecretSuffix is the tail of the
// delivered Secret's name, part of the KeystoneApplicationCredential API. It
// matches the KeystoneUser suffix, so a user order and a credential order of
// one name in one namespace would deliver the same Secret, and the younger one
// reports DeliveryRefused.
const keystoneApplicationCredentialCredentialsSecretSuffix = "-credentials"

// keystoneApplicationCredentialCredentialsSecretName is the delivered Secret's
// name, in the order's namespace.
func keystoneApplicationCredentialCredentialsSecretName(order *c5c3v1alpha1.KeystoneApplicationCredential) string {
	return order.Name + keystoneApplicationCredentialCredentialsSecretSuffix
}

// keystoneApplicationCredentialRemoteKeyFor returns the OpenBao path the
// order's credential is backed up to (orderRemoteKeyFor).
func keystoneApplicationCredentialRemoteKeyFor(cp *c5c3v1alpha1.ControlPlane, prefix string) string {
	return orderRemoteKeyFor(cp, prefix, "application-credential")
}

// keystoneApplicationCredentialGeneration resolves the declared credential
// generation. A zero, which only a CR stored without the schema default
// carries, reads as 1.
func keystoneApplicationCredentialGeneration(order *c5c3v1alpha1.KeystoneApplicationCredential) int64 {
	return max(order.Spec.CredentialGeneration, 1)
}

// keystoneApplicationCredentialRotation resolves the rotation interval and
// grace period. An absent field, which only a CR stored without the schema
// defaults carries, reads as its default.
func keystoneApplicationCredentialRotation(order *c5c3v1alpha1.KeystoneApplicationCredential) (interval, grace time.Duration) {
	interval, grace = defaultApplicationCredentialRotationInterval, defaultApplicationCredentialRotationGracePeriod
	if d := order.Spec.Rotation.Interval; d != nil {
		interval = d.Duration
	}
	if d := order.Spec.Rotation.GracePeriod; d != nil {
		grace = d.Duration
	}
	return interval, grace
}

// credentialGenerationOf reads the generation off a child named prefix<N>. A
// name without the prefix or with anything but a positive number after it is
// not a generation child.
func credentialGenerationOf(name, prefix string) (int64, bool) {
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return 0, false
	}
	gen, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || gen < 1 {
		return 0, false
	}
	return gen, true
}

// --- teardown ---

// reconcileDelete removes everything the order created and releases the
// finalizer through orderTeardown: K-ORC's finalizers take the credentials out
// of Keystone and ESO's takes the backup out of OpenBao.
func (r *KeystoneApplicationCredentialReconciler) reconcileDelete(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.KeystoneApplicationCredential, cluster string,
) (ctrl.Result, error) {
	cpKey := orderControlPlaneKey(order.Spec.ControlPlaneRef, order.Namespace)
	return orderTeardown(ctx, r.Client, oc, order, keystoneApplicationCredentialFinalizerName, cpKey,
		func(ctx context.Context, childNS string) (int, error) {
			return r.sweepKeystoneApplicationCredentialChildren(ctx, oc, order, cluster, cpKey, childNS)
		})
}

// sweepKeystoneApplicationCredentialChildren issues the deletes of every child
// the order owns in two phases and reports how many are still listed.
//
// While a K-ORC ApplicationCredential is listed, the pass deletes the
// PushSecret (its deletionPolicy removes the OpenBao path), every credential
// generation, every secret generation and the source Secret, and keeps the
// mint document: K-ORC authenticates the Keystone deletes with it and guards it
// with a finalizer. Once none is listed, the pass deletes the mint document and
// the delivered Secret beside the order. With the ControlPlane gone K-ORC has
// nothing left to reach Keystone with, so one pass issues every delete.
func (r *KeystoneApplicationCredentialReconciler) sweepKeystoneApplicationCredentialChildren(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.KeystoneApplicationCredential, cluster string,
	cpKey client.ObjectKey, childNS string,
) (int, error) {
	ref := keystoneApplicationCredentialRef(order, cluster)
	var credentials orcv1alpha1.ApplicationCredentialList
	if err := r.List(ctx, &credentials, client.InNamespace(childNS), client.MatchingLabels(ref.childLabels())); err != nil {
		return 0, fmt.Errorf("listing order %T: %w", &credentials, err)
	}
	controlPlaneGone := false
	if err := r.Get(ctx, cpKey, &c5c3v1alpha1.ControlPlane{}); err != nil {
		if !apierrors.IsNotFound(err) {
			return 0, fmt.Errorf("fetching ControlPlane %s during teardown: %w", cpKey, err)
		}
		controlPlaneGone = true
	}

	secretPrefix := keystoneApplicationCredentialSecretPrefix(order, cluster)
	secretsFirst := func(obj client.Object) bool { return strings.HasPrefix(obj.GetName(), secretPrefix) }
	credentialListed := slices.ContainsFunc(credentials.Items, func(ac orcv1alpha1.ApplicationCredential) bool {
		return ownsOrderChild(&ac, order, ref)
	})
	holdMint := credentialListed && !controlPlaneGone
	mintCloud := keystoneApplicationCredentialMintCloudName(order, cluster)

	remaining, err := sweepOrderLists(ctx, r.Client, order, ref, childNS,
		orderSweep{list: &esov1alpha1.PushSecretList{}},
		orderSweep{list: &orcv1alpha1.ApplicationCredentialList{}},
		orderSweep{list: &corev1.SecretList{}, first: secretsFirst, skip: func(obj client.Object) bool {
			return holdMint && obj.GetName() == mintCloud
		}},
	)
	if err != nil {
		return 0, err
	}
	if holdMint {
		return remaining, nil
	}
	delivered, err := sweepDeliveredSecret(ctx, oc, order,
		types.NamespacedName{Namespace: order.Namespace, Name: keystoneApplicationCredentialCredentialsSecretName(order)})
	if err != nil {
		return 0, err
	}
	return remaining + delivered, nil
}

// --- manager setup ---

// KeystoneApplicationCredentialControlPlaneRefIndexKey is the field-indexer key
// under which a KeystoneApplicationCredential on the management cluster is
// indexed by its resolved ControlPlane reference, "<namespace>/<name>".
const KeystoneApplicationCredentialControlPlaneRefIndexKey = "spec.controlPlaneRef"

// keystoneApplicationCredentialControlPlaneRefExtractor returns the resolved
// "<namespace>/<name>" of obj's ControlPlane reference. An empty name indexes
// nothing.
func keystoneApplicationCredentialControlPlaneRefExtractor(obj client.Object) []string {
	order, ok := obj.(*c5c3v1alpha1.KeystoneApplicationCredential)
	if !ok || order.Spec.ControlPlaneRef.Name == "" {
		return nil
	}
	key := orderControlPlaneKey(order.Spec.ControlPlaneRef, order.Namespace)
	return []string{keystoneServiceControlPlaneRefIndexValue(key.Namespace, key.Name)}
}

// controlPlaneToKeystoneApplicationCredentialsMapper maps a ControlPlane event
// to the orders on the management cluster that reference it. Orders on target
// clusters come back on orderRefreshAfter. A List failure is logged and maps
// to nothing.
func controlPlaneToKeystoneApplicationCredentialsMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var orders c5c3v1alpha1.KeystoneApplicationCredentialList
		if err := c.List(ctx, &orders, client.MatchingFields{
			KeystoneApplicationCredentialControlPlaneRefIndexKey: keystoneServiceControlPlaneRefIndexValue(
				obj.GetNamespace(), obj.GetName()),
		}); err != nil {
			log.FromContext(ctx).Error(err, "listing KeystoneApplicationCredentials for ControlPlane watch")
			return nil
		}
		requests := make([]reconcile.Request, 0, len(orders.Items))
		for i := range orders.Items {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&orders.Items[i])})
		}
		return requests
	}
}

// applicationCredentialUserName reads the KeystoneUser a credential order
// names.
func applicationCredentialUserName(ac *c5c3v1alpha1.KeystoneApplicationCredential) string {
	return ac.Spec.UserRef.Name
}

// applicationCredentialProjectName reads the KeystoneProject a credential order
// names.
func applicationCredentialProjectName(ac *c5c3v1alpha1.KeystoneApplicationCredential) string {
	return ac.Spec.ProjectRef.Name
}

// keystoneApplicationCredentialMapper maps an event of another order on
// clusterName to every credential order in its namespace that matches selects,
// listing through c, that cluster's cache. A List failure is logged and maps to
// nothing.
func keystoneApplicationCredentialMapper(
	clusterName mcruntime.ClusterName, c client.Reader,
	matches func(*c5c3v1alpha1.KeystoneApplicationCredential, client.Object) bool,
) func(context.Context, client.Object) []mcreconcile.Request {
	return func(ctx context.Context, obj client.Object) []mcreconcile.Request {
		// The items are only read, matched and dropped, so no copy is taken.
		var orders c5c3v1alpha1.KeystoneApplicationCredentialList
		if err := c.List(ctx, &orders, client.InNamespace(obj.GetNamespace()), client.UnsafeDisableDeepCopy); err != nil {
			log.FromContext(ctx).Error(err, "listing KeystoneApplicationCredentials for a referenced order",
				"cluster", clusterName, "order", client.ObjectKeyFromObject(obj))
			return nil
		}
		var requests []mcreconcile.Request
		for i := range orders.Items {
			if matches(&orders.Items[i], obj) {
				requests = append(requests, mcreconcile.Request{
					Request:     reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&orders.Items[i])},
					ClusterName: clusterName,
				})
			}
		}
		return requests
	}
}

// referencedOrderToApplicationCredentialRequests is the event-handler factory
// of the user and project reference legs: it maps a KeystoneUser or
// KeystoneProject to the credential orders whose refName names it, on the
// cluster the event came from.
func referencedOrderToApplicationCredentialRequests(
	refName func(*c5c3v1alpha1.KeystoneApplicationCredential) string,
) mchandler.TypedEventHandlerFunc[client.Object, mcreconcile.Request] {
	return func(clusterName mcruntime.ClusterName, cl cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
		return mchandler.TypedEnqueueRequestsFromMapFuncWithClusterPreservation(keystoneApplicationCredentialMapper(
			clusterName, cl.GetClient(), func(ac *c5c3v1alpha1.KeystoneApplicationCredential, obj client.Object) bool {
				return refName(ac) == obj.GetName()
			}))
	}
}

// roleAssignmentToApplicationCredentialRequests is the event-handler factory
// of the role assignment leg: it maps a KeystoneRoleAssignment to the
// credential orders naming the same user and project, on the cluster the event
// came from.
func roleAssignmentToApplicationCredentialRequests() mchandler.TypedEventHandlerFunc[client.Object, mcreconcile.Request] {
	return func(clusterName mcruntime.ClusterName, cl cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
		return mchandler.TypedEnqueueRequestsFromMapFuncWithClusterPreservation(keystoneApplicationCredentialMapper(
			clusterName, cl.GetClient(), func(ac *c5c3v1alpha1.KeystoneApplicationCredential, obj client.Object) bool {
				ra, ok := obj.(*c5c3v1alpha1.KeystoneRoleAssignment)
				return ok && roleAssignmentBindsCredentialPair(ra, ac)
			}))
	}
}

// keystoneApplicationCredentialReferencePredicate passes the updates of a
// referenced KeystoneUser or KeystoneProject the gates and the mint document
// read: what keystoneRoleAssignmentReferencePredicate passes, and a move of the
// user's status.passwordGeneration, which the mint document must follow.
func keystoneApplicationCredentialReferencePredicate() predicate.Funcs {
	readiness := keystoneRoleAssignmentReferencePredicate()
	passwordGeneration := func(obj client.Object) int64 {
		if user, ok := obj.(*c5c3v1alpha1.KeystoneUser); ok {
			return user.Status.PasswordGeneration
		}
		return 0
	}
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return readiness.Update(e) || passwordGeneration(e.ObjectOld) != passwordGeneration(e.ObjectNew)
		},
	}
}

// keystoneApplicationCredentialRoleAssignmentPredicate passes the updates of a
// KeystoneRoleAssignment the role gate reads: a spec change, a deletion and a
// change of status.roleID.
func keystoneApplicationCredentialRoleAssignmentPredicate() predicate.Funcs {
	roleID := func(obj client.Object) string {
		if ra, ok := obj.(*c5c3v1alpha1.KeystoneRoleAssignment); ok {
			return ra.Status.RoleID
		}
		return ""
	}
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() ||
				!e.ObjectOld.GetDeletionTimestamp().Equal(e.ObjectNew.GetDeletionTimestamp()) ||
				roleID(e.ObjectOld) != roleID(e.ObjectNew)
		},
	}
}

// SetupWithManager registers the KeystoneApplicationCredentialReconciler with
// the multicluster manager.
func (r *KeystoneApplicationCredentialReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return r.setupWithOptions(mgr, bootstrap.TypedControllerOptions[mcreconcile.Request](r.MaxConcurrentReconciles))
}

// setupWithOptions carries the production setup SetupWithManager applies, with
// the controller options as a parameter so the integration suites register this
// chain with SkipNameValidation set.
//
// The watches:
//   - For: the order on the management cluster and on every engaged target
//     cluster that serves the kind.
//   - Owns: the delivered Secret beside the order, on the same clusters.
//   - The ApplicationCredential, Secret and PushSecret children in the
//     ControlPlane's namespace, mapped back by their labels. K-ORC removing a
//     superseded credential reaches the grace sweep this way.
//   - KeystoneUser and KeystoneProject: the credential orders naming them, on
//     the updates keystoneApplicationCredentialReferencePredicate passes.
//   - KeystoneRoleAssignment: the credential orders naming the same user and
//     project, on the updates
//     keystoneApplicationCredentialRoleAssignmentPredicate passes.
//   - ControlPlane: the orders on the management cluster that reference it, on
//     the updates orderControlPlanePredicate passes.
//
// The index goes on the local field indexer, for the reason the KeystoneUser
// reconciler gives.
func (r *KeystoneApplicationCredentialReconciler) setupWithOptions(
	mgr mcmanager.Manager, opts crcontroller.TypedOptions[mcreconcile.Request],
) error {
	local := mgr.GetLocalManager()
	if err := local.GetFieldIndexer().IndexField(context.Background(), &c5c3v1alpha1.KeystoneApplicationCredential{},
		KeystoneApplicationCredentialControlPlaneRefIndexKey, keystoneApplicationCredentialControlPlaneRefExtractor); err != nil {
		return err
	}
	engageLocal := commonmulticluster.EngageLocalCluster
	engageNoProviders := commonmulticluster.EngageNoProviderClusters
	engageProviders := mcbuilder.WithEngageWithProviderClusters(true)
	servesKind := mcbuilder.WithClusterFilter(commonmulticluster.ClusterServesKind(keystoneApplicationCredentialGVK))
	children := orderChildRequests(keystoneApplicationCredentialLabelKeys)
	referencePredicate := mcbuilder.WithPredicates(keystoneApplicationCredentialReferencePredicate())

	return mcbuilder.ControllerManagedBy(mgr).
		WithOptions(opts).
		For(&c5c3v1alpha1.KeystoneApplicationCredential{}, mcbuilder.WithPredicates(watch.CRUpdatePredicate()),
			engageLocal, engageProviders, servesKind).
		Owns(&corev1.Secret{}, engageLocal, engageProviders, servesKind).
		Watches(&orcv1alpha1.ApplicationCredential{}, children, engageLocal, engageNoProviders).
		Watches(&corev1.Secret{}, children, engageLocal, engageNoProviders).
		Watches(&esov1alpha1.PushSecret{}, children, engageLocal, engageNoProviders).
		Watches(&c5c3v1alpha1.KeystoneUser{},
			referencedOrderToApplicationCredentialRequests(applicationCredentialUserName),
			referencePredicate, engageLocal, engageProviders,
			mcbuilder.WithClusterFilter(commonmulticluster.ClusterServesKind(keystoneUserGVK))).
		Watches(&c5c3v1alpha1.KeystoneProject{},
			referencedOrderToApplicationCredentialRequests(applicationCredentialProjectName),
			referencePredicate, engageLocal, engageProviders,
			mcbuilder.WithClusterFilter(commonmulticluster.ClusterServesKind(keystoneProjectGVK))).
		Watches(&c5c3v1alpha1.KeystoneRoleAssignment{}, roleAssignmentToApplicationCredentialRequests(),
			mcbuilder.WithPredicates(keystoneApplicationCredentialRoleAssignmentPredicate()), engageLocal, engageProviders,
			mcbuilder.WithClusterFilter(commonmulticluster.ClusterServesKind(keystoneRoleAssignmentGVK))).
		Watches(&c5c3v1alpha1.ControlPlane{},
			commonmulticluster.LocalRequests(controlPlaneToKeystoneApplicationCredentialsMapper(local.GetClient())),
			mcbuilder.WithPredicates(orderControlPlanePredicate()), engageLocal, engageNoProviders).
		// Reconcile answers an unresolvable cluster itself, so the wrapper that
		// would turn a cluster-not-found error into a success stays off.
		WithClusterNotFoundWrapper(false).
		Complete(r)
}
