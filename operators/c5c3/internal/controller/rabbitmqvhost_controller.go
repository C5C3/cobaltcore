// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// The two sub-conditions a RabbitMQVhost carries, plus the aggregate Ready
// derived from them. VhostReady reports the vhost, the user generations and the
// schedule; DeliveryReady reports the OpenBao backup and the Secret beside the
// order.
const (
	conditionTypeRabbitMQVhostVhostReady    = "VhostReady"
	conditionTypeRabbitMQVhostDeliveryReady = "DeliveryReady"
)

// rabbitMQVhostSubConditionTypes are the sub-conditions the aggregate Ready is
// derived from.
var rabbitMQVhostSubConditionTypes = []string{
	conditionTypeRabbitMQVhostVhostReady,
	conditionTypeRabbitMQVhostDeliveryReady,
}

// rabbitMQVhostFinalizerName gates the teardown of everything an order created.
const rabbitMQVhostFinalizerName = "c5c3.io/rabbitmqvhost-teardown"

// rabbitMQVhostLabelKeys are the ownership labels an order's children carry,
// for the reason keystoneUserLabelKeys gives.
var rabbitMQVhostLabelKeys = orderLabelKeys{
	Name:      "c5c3.io/rabbitmqvhost-name",
	Namespace: "c5c3.io/rabbitmqvhost-namespace",
	Cluster:   "c5c3.io/rabbitmqvhost-cluster",
}

// rabbitMQVhostGVK is the kind the order watch filters target clusters on.
var rabbitMQVhostGVK = c5c3v1alpha1.GroupVersion.WithKind("RabbitMQVhost")

// The rotation defaults the schema applies. The reconciler resolves them for an
// object stored without them, because the kind has no webhook.
const (
	defaultRabbitMQVhostRotationInterval    = 720 * time.Hour
	defaultRabbitMQVhostRotationGracePeriod = 24 * time.Hour
)

// The keys of the delivered Secret beside the order and of its source Secret.
// The password Secret of a generation carries the username and password keys,
// which the topology operator imports the user from.
const (
	rabbitMQVhostTransportURLKey = commonv1.DefaultTransportURLSecretKey
	rabbitMQVhostHostKey         = "host"
	rabbitMQVhostPortKey         = "port"
	rabbitMQVhostUsernameKey     = "username"
	rabbitMQVhostPasswordKey     = "password" //nolint:gosec // G101 false positive: Secret data key, not a credential.
	rabbitMQVhostVhostKey        = "vhost"
)

// rabbitMQVhostSecretKeys are the keys of the delivered Secret, in the order
// status.secretKeys lists them.
var rabbitMQVhostSecretKeys = []string{
	rabbitMQVhostTransportURLKey, rabbitMQVhostHostKey, rabbitMQVhostPortKey,
	rabbitMQVhostUsernameKey, rabbitMQVhostPasswordKey, rabbitMQVhostVhostKey,
}

// Condition reasons this controller introduces. The admission gates write the
// scaffold's reasons (keystoneorder.go) and reasonKeystoneServiceControlPlaneNotFound,
// the store gate reasonServiceAccountStoreNotReady, the bus gate
// messaging.ReasonWaitingForMessagingCredentials, and the delivery the
// KeystoneUser vocabulary (reasonKeystoneUserDelivered,
// reasonKeystoneUserBackupNotSynced, reasonKeystoneUserDeliveryRefused,
// reasonKeystoneUserDeliveryError).
const (
	// reasonRabbitMQVhostMessagingNotDeclared reports a ControlPlane that
	// declares no spec.infrastructure.messaging.
	reasonRabbitMQVhostMessagingNotDeclared = "MessagingNotDeclared"
	// reasonRabbitMQVhostMessagingNotManaged reports a ControlPlane that attaches
	// to a brownfield broker, which the operator holds no admin account on.
	reasonRabbitMQVhostMessagingNotManaged = "MessagingNotManaged"
	// reasonRabbitMQVhostWaitingForMessaging reports a managed bus that is not
	// AllReplicasReady.
	reasonRabbitMQVhostWaitingForMessaging = "WaitingForMessaging"
	// reasonRabbitMQVhostTopologyOperatorNotInstalled reports a management
	// cluster that serves none of the topology operator's kinds.
	reasonRabbitMQVhostTopologyOperatorNotInstalled = "TopologyOperatorNotInstalled"
	// reasonRabbitMQVhostWaitingForVhost reports a Vhost the topology operator
	// has not reported Ready yet.
	reasonRabbitMQVhostWaitingForVhost = "WaitingForVhost"
	// reasonRabbitMQVhostWaitingForUser reports a User or Permission of the
	// first generation the topology operator has not reported Ready yet.
	reasonRabbitMQVhostWaitingForUser = "WaitingForUser"
	// reasonRabbitMQVhostFailed reports a Vhost, User or Permission the broker
	// refused (the topology operator's FailedCreateOrUpdate).
	reasonRabbitMQVhostFailed = "VhostFailed"
	// reasonRabbitMQVhostProvisioned is VhostReady's True reason, also while a
	// successor generation is being created.
	reasonRabbitMQVhostProvisioned = "VhostProvisioned"
	// reasonRabbitMQVhostMessagingNotPublished reports an order on a cluster that
	// cannot reach the broker's Service while the ControlPlane publishes no
	// messaging endpoint.
	reasonRabbitMQVhostMessagingNotPublished = "MessagingNotPublished"
	// reasonRabbitMQVhostWaitingForPassword reports, on DeliveryReady, that no
	// password is provisioned or delivered yet.
	reasonRabbitMQVhostWaitingForPassword = "WaitingForPassword" //nolint:gosec // G101 false positive: condition reason name, not a credential.
	// reasonRabbitMQVhostError reports a Kubernetes-level failure writing or
	// reading the order's children, or a child of that name the order did not
	// create.
	reasonRabbitMQVhostError = "VhostError"
)

// rabbitMQVhostNotProvisionedMessage is the WaitingForPassword message of an
// order without a live user generation.
const rabbitMQVhostNotProvisionedMessage = "the vhost and its user are not provisioned yet; nothing is delivered"

// RabbitMQVhostReconciler owns the RabbitMQVhost lifecycle and is the single
// writer of its status. It serves orders on the management cluster and on
// target clusters the way the KeystoneUser reconciler does: the order, its
// status, its finalizer and its delivered Secret are read and written on the
// order's cluster, and every other child lives in the ControlPlane's namespace
// on the management cluster. It records no Events, for the reason the
// KeystoneUser reconciler gives, and it reads nothing from Keystone, so it has
// no admin-credential gate.
type RabbitMQVhostReconciler struct {
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

// RBAC for the RabbitMQVhost kind: the controller reads the orders and updates
// them to install and release its finalizer; it never creates or deletes one.
// It writes the topology operator's Vhost, User and Permission kinds in the
// ControlPlane's namespace. The Secrets and PushSecrets it writes there and the
// ControlPlane and RabbitmqCluster reads are granted by the ControlPlane's
// marker block.
// +kubebuilder:rbac:groups=c5c3.io,resources=rabbitmqvhosts,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=rabbitmqvhosts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=rabbitmqvhosts/finalizers,verbs=update
// +kubebuilder:rbac:groups=rabbitmq.com,resources=vhosts;users;permissions,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives one RabbitMQVhost: the gates, finalizer installation, the
// provision, the rotation, the delivery and the teardown.
func (r *RabbitMQVhostReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	cluster := string(req.ClusterName)
	oc, err := orderClient(ctx, r.Resolver, r.Client, cluster)
	if err != nil {
		// The cluster was deregistered between the event and this pass. Nothing
		// can be read or written there, and a requeue would only repeat this.
		log.FromContext(ctx).Info("the cluster the RabbitMQVhost lives on does not resolve; skipping it",
			"cluster", cluster, "order", req.NamespacedName, "reason", err.Error())
		return ctrl.Result{}, nil
	}

	var order c5c3v1alpha1.RabbitMQVhost
	if err := oc.Get(ctx, req.NamespacedName, &order); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("RabbitMQVhost not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching RabbitMQVhost: %w", err)
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
func (r *RabbitMQVhostReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().Truncate(time.Second)
	}
	return time.Now().Truncate(time.Second)
}

// reconcileNormal runs the admission gates, the messaging gate, then the
// provision and the delivery.
//
// The freeze of #1327 D2 is the assignment gate of orderAdmission: without an
// entry the pass returns before anything is read or written in the
// ControlPlane's namespace, so nothing is provisioned, rotated, delivered,
// repaired or swept, and the schedule pauses.
func (r *RabbitMQVhostReconciler) reconcileNormal(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.RabbitMQVhost, cluster string,
) (ctrl.Result, error) {
	failBoth := func(reason, message string) { rabbitMQVhostFailBoth(order, reason, message) }
	cp, _, result, err := orderAdmission(ctx, r.Client, rabbitMQVhostRef(order, cluster),
		order.Spec.ControlPlaneRef, failBoth)
	if err != nil || cp == nil {
		return result, err
	}

	if added, err := commonreconcile.EnsureFinalizer(ctx, oc, order, rabbitMQVhostFinalizerName); err != nil {
		return ctrl.Result{}, err
	} else if added {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
	}

	host, port, result, ok, err := r.rabbitMQVhostMessagingGate(ctx, cp, failBoth)
	if err != nil || !ok {
		return rabbitMQVhostPassResult(order, cluster, result), err
	}

	provisioned := false
	provisionResult, err := instrumenter.Instrument(ctx, "RabbitMQVhostProvision",
		func(ctx context.Context) (ctrl.Result, error) {
			ok, res, err := r.provisionVhost(ctx, order, cp, cluster)
			provisioned = ok
			return res, err
		})
	if err != nil {
		return ctrl.Result{}, err
	}
	if !provisioned {
		rabbitMQVhostFail(order, conditionTypeRabbitMQVhostDeliveryReady)(reasonRabbitMQVhostWaitingForPassword,
			rabbitMQVhostNotProvisionedMessage)
		return rabbitMQVhostPassResult(order, cluster, provisionResult), nil
	}

	deliveryResult, err := instrumenter.Instrument(ctx, "RabbitMQVhostDelivery",
		func(ctx context.Context) (ctrl.Result, error) {
			return r.deliverCredentials(ctx, oc, order, cp, cluster, host, port)
		})
	if err != nil {
		return ctrl.Result{}, err
	}
	return rabbitMQVhostPassResult(order, cluster, commonreconcile.ShortestRequeue(provisionResult, deliveryResult)), nil
}

// rabbitMQVhostPassResult decides when the next pass runs once the legs had
// their say, with DeliveryReady as the converged condition (orderPassResult).
// The legs' requeue, which carries the rotation timers, is kept when it is the
// shorter one, as keystoneApplicationCredentialPassResult does.
func rabbitMQVhostPassResult(order *c5c3v1alpha1.RabbitMQVhost, cluster string, result ctrl.Result) ctrl.Result {
	converged := conditions.AllTrue(order.Status.Conditions, conditionTypeRabbitMQVhostDeliveryReady)
	return commonreconcile.ShortestRequeue(orderPassResult(cluster, converged, ctrl.Result{}), result)
}

// rabbitMQVhostMessagingGate admits an order only onto a managed bus that is
// up. It returns the broker's in-cluster host and port, read off the
// default-user Secret the RabbitmqCluster names, once the bus is
// AllReplicasReady. A false ok means failBoth wrote the refusal or the wait and
// res is the gate's own result: none for a refusal only a ControlPlane edit
// lifts, rabbitMQVhostRequeueAfter for a wait.
func (r *RabbitMQVhostReconciler) rabbitMQVhostMessagingGate(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, failBoth func(reason, message string),
) (host string, port int32, res ctrl.Result, ok bool, err error) {
	requeue := ctrl.Result{RequeueAfter: rabbitMQVhostRequeueAfter}
	if cp.Spec.Infrastructure == nil || cp.Spec.Infrastructure.Messaging == nil {
		failBoth(reasonRabbitMQVhostMessagingNotDeclared, fmt.Sprintf(
			"ControlPlane %s/%s declares no spec.infrastructure.messaging; a RabbitMQVhost order needs the managed "+
				"message bus", cp.Namespace, cp.Name))
		return "", 0, ctrl.Result{}, false, nil
	}
	bus := cp.Spec.Infrastructure.Messaging
	if bus.ClusterRef == nil {
		failBoth(reasonRabbitMQVhostMessagingNotManaged, fmt.Sprintf(
			"ControlPlane %s/%s attaches to a brownfield broker through spec.infrastructure.messaging.secretRef, "+
				"whose management API and admin account the operator does not hold; RabbitMQVhost orders need a "+
				"managed clusterRef bus", cp.Namespace, cp.Name))
		return "", 0, ctrl.Result{}, false, nil
	}

	transportURL, _, waitMsg, err := messaging.ResolveTransportURL(ctx, messaging.TransportURLSecretFlowParams{
		Client: r.Client, Namespace: cp.Namespace, Messaging: bus,
	})
	if err != nil {
		err = fmt.Errorf("resolving the managed bus of ControlPlane %s/%s: %w", cp.Namespace, cp.Name, err)
		failBoth(reasonRabbitMQVhostError, err.Error())
		return "", 0, ctrl.Result{}, false, err
	}
	if waitMsg != "" {
		failBoth(messaging.ReasonWaitingForMessagingCredentials, waitMsg)
		return "", 0, requeue, false, nil
	}
	// The URL carries the broker's admin password, so neither it nor a parse
	// error quoting it ends up in a condition.
	parsed, err := url.Parse(transportURL)
	if err != nil {
		err = fmt.Errorf("parsing the transport URL of ControlPlane %s/%s", cp.Namespace, cp.Name)
		failBoth(reasonRabbitMQVhostError, err.Error())
		return "", 0, ctrl.Result{}, false, err
	}
	parsedPort, err := strconv.ParseInt(parsed.Port(), 10, 32)
	if err != nil {
		err = fmt.Errorf("parsing the broker port of ControlPlane %s/%s: %w", cp.Namespace, cp.Name, err)
		failBoth(reasonRabbitMQVhostError, err.Error())
		return "", 0, ctrl.Result{}, false, err
	}

	rabbitmq := &unstructured.Unstructured{}
	rabbitmq.SetGroupVersionKind(messaging.RabbitmqClusterGVK)
	key := types.NamespacedName{Namespace: cp.Namespace, Name: bus.ClusterRef.Name}
	if err := r.Get(ctx, key, rabbitmq); err != nil {
		if apierrors.IsNotFound(err) {
			failBoth(messaging.ReasonWaitingForMessagingCredentials, fmt.Sprintf("RabbitmqCluster %s not found", key))
			return "", 0, requeue, false, nil
		}
		err = fmt.Errorf("reading RabbitmqCluster %s: %w", key, err)
		failBoth(reasonRabbitMQVhostError, err.Error())
		return "", 0, ctrl.Result{}, false, err
	}
	if !unstructuredConditionTrue(rabbitmq, messaging.RabbitmqClusterReadyCondition) {
		failBoth(reasonRabbitMQVhostWaitingForMessaging, fmt.Sprintf("RabbitmqCluster %s is not AllReplicasReady", key))
		return "", 0, requeue, false, nil
	}
	return parsed.Hostname(), int32(parsedPort), ctrl.Result{}, true, nil
}

// rabbitMQVhostFail returns a closure bound to order and condType that writes
// a False condition, truncating the message for the reason keystoneUserFail
// gives: it may relay the topology operator's own text.
func rabbitMQVhostFail(order *c5c3v1alpha1.RabbitMQVhost, condType string) func(reason, message string) {
	return func(reason, message string) {
		setTruncatedCondition(&order.Status.Conditions, order.Generation, condType, metav1.ConditionFalse, reason, message)
	}
}

// rabbitMQVhostFailBoth writes one gate failure onto both sub-conditions.
func rabbitMQVhostFailBoth(order *c5c3v1alpha1.RabbitMQVhost, reason, message string) {
	for _, condType := range rabbitMQVhostSubConditionTypes {
		rabbitMQVhostFail(order, condType)(reason, message)
	}
}

// rabbitMQVhostSetTrue writes a sub-condition as True.
func rabbitMQVhostSetTrue(order *c5c3v1alpha1.RabbitMQVhost, condType, reason, message string) {
	setTruncatedCondition(&order.Status.Conditions, order.Generation, condType, metav1.ConditionTrue, reason, message)
}

// updateStatus persists the status through the order's cluster client, as the
// KeystoneUser reconciler's does.
func (r *RabbitMQVhostReconciler) updateStatus(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.RabbitMQVhost,
	statusBefore *c5c3v1alpha1.RabbitMQVhostStatus, result ctrl.Result, reconcileErr error,
) (ctrl.Result, error) {
	return commonreconcile.UpdateStatus(ctx, oc, order, statusBefore, &order.Status, func() {
		commonreconcile.SetAggregateReady(&order.Status.Conditions, order.Generation, rabbitMQVhostSubConditionTypes)
		order.Status.ObservedGeneration = order.Generation
	}, result, reconcileErr)
}

// --- naming ---

// rabbitMQVhostRef identifies the order to the shared scaffold: its children
// carry the "vhost" segment and the rabbitmqvhost labels.
func rabbitMQVhostRef(order *c5c3v1alpha1.RabbitMQVhost, cluster string) orderRef {
	return orderRef{
		Name: order.Name, Namespace: order.Namespace, Cluster: cluster,
		Segment: "vhost", Keys: rabbitMQVhostLabelKeys,
	}
}

// rabbitMQVhostChildPrefix scopes every child an order creates in the
// ControlPlane's namespace (orderRef.childPrefix).
func rabbitMQVhostChildPrefix(order *c5c3v1alpha1.RabbitMQVhost, cluster string) string {
	return rabbitMQVhostRef(order, cluster).childPrefix()
}

// rabbitMQVhostName is the vhost on the broker: the child prefix without its
// "-vhost-" tail, <metadata.name>-<8 hex>. The hash covers the cluster, the
// namespace and the name, so two orders never share a vhost, and an order
// re-created with the same name reattaches to a vhost a Retain deletion kept.
func rabbitMQVhostName(order *c5c3v1alpha1.RabbitMQVhost, cluster string) string {
	return strings.TrimSuffix(rabbitMQVhostChildPrefix(order, cluster), "-vhost-")
}

// rabbitMQVhostChildName is the Vhost CR.
func rabbitMQVhostChildName(order *c5c3v1alpha1.RabbitMQVhost, cluster string) string {
	return rabbitMQVhostChildPrefix(order, cluster) + "vhost"
}

// rabbitMQVhostPasswordSecretPrefix is the prefix every generation's password
// Secret shares.
func rabbitMQVhostPasswordSecretPrefix(order *c5c3v1alpha1.RabbitMQVhost, cluster string) string {
	return rabbitMQVhostChildPrefix(order, cluster) + "password-v"
}

// rabbitMQVhostPasswordSecretName is the Secret the User of generation gen
// imports its username and password from.
func rabbitMQVhostPasswordSecretName(order *c5c3v1alpha1.RabbitMQVhost, cluster string, gen int64) string {
	return rabbitMQVhostPasswordSecretPrefix(order, cluster) + strconv.FormatInt(gen, 10)
}

// rabbitMQVhostUserPrefix is the prefix every generation's User CR shares.
func rabbitMQVhostUserPrefix(order *c5c3v1alpha1.RabbitMQVhost, cluster string) string {
	return rabbitMQVhostChildPrefix(order, cluster) + "user-v"
}

// rabbitMQVhostUserName is the User CR of generation gen.
func rabbitMQVhostUserName(order *c5c3v1alpha1.RabbitMQVhost, cluster string, gen int64) string {
	return rabbitMQVhostUserPrefix(order, cluster) + strconv.FormatInt(gen, 10)
}

// rabbitMQVhostPermissionPrefix is the prefix every generation's Permission CR
// shares.
func rabbitMQVhostPermissionPrefix(order *c5c3v1alpha1.RabbitMQVhost, cluster string) string {
	return rabbitMQVhostChildPrefix(order, cluster) + "permission-v"
}

// rabbitMQVhostPermissionName is the Permission CR of generation gen.
func rabbitMQVhostPermissionName(order *c5c3v1alpha1.RabbitMQVhost, cluster string, gen int64) string {
	return rabbitMQVhostPermissionPrefix(order, cluster) + strconv.FormatInt(gen, 10)
}

// rabbitMQVhostBrokerUserName is the user of generation gen on the broker,
// <vhost>-v<gen>.
func rabbitMQVhostBrokerUserName(order *c5c3v1alpha1.RabbitMQVhost, cluster string, gen int64) string {
	return rabbitMQVhostName(order, cluster) + "-v" + strconv.FormatInt(gen, 10)
}

func rabbitMQVhostSourceSecretName(order *c5c3v1alpha1.RabbitMQVhost, cluster string) string {
	return rabbitMQVhostChildPrefix(order, cluster) + "source"
}

func rabbitMQVhostPushSecretName(order *c5c3v1alpha1.RabbitMQVhost, cluster string) string {
	return rabbitMQVhostChildPrefix(order, cluster) + "backup"
}

// rabbitMQVhostCredentialsSecretSuffix is the tail of the delivered Secret's
// name, part of the RabbitMQVhost API. It matches the Keystone order kinds'
// suffix, so a Keystone order and a vhost order of one name in one namespace
// would deliver the same Secret, and the younger one reports DeliveryRefused.
const rabbitMQVhostCredentialsSecretSuffix = "-credentials"

// rabbitMQVhostCredentialsSecretName is the delivered Secret's name, in the
// order's namespace.
func rabbitMQVhostCredentialsSecretName(order *c5c3v1alpha1.RabbitMQVhost) string {
	return order.Name + rabbitMQVhostCredentialsSecretSuffix
}

// rabbitMQVhostRemoteKeyFor returns the OpenBao path the order's credentials
// are backed up to. It sits under the ControlPlane's namespace, which is what
// the eso-tenant policy grants that namespace's store
// (openstack/rabbitmq/{store namespace}/+/credentials), and the prefix fills
// the segment in between, so no two orders share a path.
func rabbitMQVhostRemoteKeyFor(cp *c5c3v1alpha1.ControlPlane, prefix string) string {
	return "openstack/rabbitmq/" + cp.Namespace + "/" + strings.TrimSuffix(prefix, "-") + "/credentials"
}

// rabbitMQVhostGeneration resolves the declared password generation. A zero,
// which only a CR stored without the schema default carries, reads as 1.
func rabbitMQVhostGeneration(order *c5c3v1alpha1.RabbitMQVhost) int64 {
	return max(order.Spec.PasswordGeneration, 1)
}

// rabbitMQVhostRotation resolves the rotation interval and grace period. An
// absent field, which only a CR stored without the schema defaults carries,
// reads as its default.
func rabbitMQVhostRotation(order *c5c3v1alpha1.RabbitMQVhost) (interval, grace time.Duration) {
	interval, grace = defaultRabbitMQVhostRotationInterval, defaultRabbitMQVhostRotationGracePeriod
	if d := order.Spec.Rotation.Interval; d != nil {
		interval = d.Duration
	}
	if d := order.Spec.Rotation.GracePeriod; d != nil {
		grace = d.Duration
	}
	return interval, grace
}

// rabbitMQVhostDeletionPolicy resolves the deletion policy. An empty value,
// which only a CR stored without the schema default carries, reads as Retain.
func rabbitMQVhostDeletionPolicy(order *c5c3v1alpha1.RabbitMQVhost) string {
	return cmp.Or(order.Spec.DeletionPolicy, c5c3v1alpha1.RabbitMQVhostDeletionPolicyRetain)
}

// --- ownership ---

// ownsRabbitMQVhostChild reports whether obj is a child the order created in
// the ControlPlane's namespace (ownsOrderChild).
func ownsRabbitMQVhostChild(obj client.Object, order *c5c3v1alpha1.RabbitMQVhost, cluster string) bool {
	return ownsOrderChild(obj, order, rabbitMQVhostRef(order, cluster))
}

// ensureRabbitMQVhostChild applies a child in the ControlPlane's namespace,
// refusing one the order did not create (ensureOrderChild).
func (r *RabbitMQVhostReconciler) ensureRabbitMQVhostChild(
	ctx context.Context, order *c5c3v1alpha1.RabbitMQVhost, cluster string, obj client.Object,
) error {
	return ensureOrderChild(ctx, r.Client, r.Scheme, order, rabbitMQVhostRef(order, cluster), obj)
}

// topologyList returns an empty list of the topology kind gvk.
func topologyList(gvk schema.GroupVersionKind) *unstructured.UnstructuredList {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	return list
}

// --- teardown ---

// reconcileDelete removes everything the order created and releases the
// finalizer through orderTeardown: the topology operator's finalizers take the
// user and its permission off the broker, and the vhost under the Delete
// policy; ESO's finalizer takes the backup out of OpenBao. Nothing references a
// RabbitMQVhost, so the teardown never holds on another order.
func (r *RabbitMQVhostReconciler) reconcileDelete(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.RabbitMQVhost, cluster string,
) (ctrl.Result, error) {
	return orderTeardown(ctx, r.Client, oc, order, rabbitMQVhostFinalizerName,
		orderControlPlaneKey(order.Spec.ControlPlaneRef, order.Namespace),
		func(ctx context.Context, childNS string) (int, error) {
			return r.sweepRabbitMQVhostChildren(ctx, oc, order, cluster, childNS)
		})
}

// sweepRabbitMQVhostChildren issues the deletes of every child the order owns
// and reports how many are still listed.
//
// The deletes run in this order, all in childNS on the management cluster: the
// PushSecret (its deletionPolicy removes the OpenBao path), every Permission,
// every User, the Vhost (it first takes the order's current policy, then the
// topology operator deletes it from the broker under the Delete policy and
// leaves it under Retain), the password Secrets and the source Secret; then
// the delivered Secret beside the order. A child is counted on the pass that
// issued its delete, so the finalizer is held until the topology operator's
// finalizers let go. A cluster that serves none of a topology kind holds none
// of it.
//
// A Vhost whose policy cannot be written is not deleted under a stale one. The
// other deletes are still issued, and the write's error is returned after
// them, joined to the error of a later delete that fails: with the
// ControlPlane gone, orderTeardown logs that error and releases the finalizer,
// and the credentials and the broker user must not outlive it.
func (r *RabbitMQVhostReconciler) sweepRabbitMQVhostChildren(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.RabbitMQVhost, cluster, childNS string,
) (int, error) {
	ref := rabbitMQVhostRef(order, cluster)
	remaining, err := sweepOrderLists(ctx, r.Client, order, ref, childNS, orderSweep{list: &esov1alpha1.PushSecretList{}})
	if err != nil {
		return 0, err
	}
	projectErr := r.projectRabbitMQVhostDeletionPolicy(ctx, order, cluster, childNS)
	for _, gvk := range []schema.GroupVersionKind{messaging.PermissionGVK, messaging.UserGVK, messaging.VhostGVK} {
		if gvk == messaging.VhostGVK && projectErr != nil {
			continue
		}
		n, err := sweepOrderLists(ctx, r.Client, order, ref, childNS, orderSweep{list: topologyList(gvk)})
		if err != nil && !meta.IsNoMatchError(err) {
			return 0, errors.Join(err, projectErr)
		}
		remaining += n
	}
	passwordPrefix := rabbitMQVhostPasswordSecretPrefix(order, cluster)
	secrets, err := sweepOrderLists(ctx, r.Client, order, ref, childNS,
		orderSweep{list: &corev1.SecretList{}, first: func(obj client.Object) bool {
			return strings.HasPrefix(obj.GetName(), passwordPrefix)
		}})
	if err != nil {
		return 0, errors.Join(err, projectErr)
	}
	delivered, err := sweepDeliveredSecret(ctx, oc, order,
		types.NamespacedName{Namespace: order.Namespace, Name: rabbitMQVhostCredentialsSecretName(order)})
	if err != nil {
		return 0, errors.Join(err, projectErr)
	}
	if projectErr != nil {
		return 0, projectErr
	}
	return remaining + secrets + delivered, nil
}

// projectRabbitMQVhostDeletionPolicy writes the order's current deletion policy
// onto its Vhost CR before the sweep deletes it. The topology operator reads
// the policy off the CR it deletes, and only a provisioning pass applies the
// CR: a policy changed right before the delete, or while the order was frozen
// or waiting on a gate, reaches the CR here. A Vhost already being deleted
// keeps the policy it was deleted under.
func (r *RabbitMQVhostReconciler) projectRabbitMQVhostDeletionPolicy(
	ctx context.Context, order *c5c3v1alpha1.RabbitMQVhost, cluster, childNS string,
) error {
	vhost := &unstructured.Unstructured{}
	vhost.SetGroupVersionKind(messaging.VhostGVK)
	key := types.NamespacedName{Namespace: childNS, Name: rabbitMQVhostChildName(order, cluster)}
	if err := r.Get(ctx, key, vhost); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("reading Vhost %s before its delete: %w", key, err)
	}
	if !ownsRabbitMQVhostChild(vhost, order, cluster) || vhost.GetDeletionTimestamp() != nil {
		return nil
	}
	want := strings.ToLower(rabbitMQVhostDeletionPolicy(order))
	if got, _, _ := unstructured.NestedString(vhost.Object, "spec", "deletionPolicy"); got == want {
		return nil
	}
	patch := client.MergeFrom(vhost.DeepCopy())
	_ = unstructured.SetNestedField(vhost.Object, want, "spec", "deletionPolicy")
	if err := r.Patch(ctx, vhost, patch); err != nil {
		return fmt.Errorf("writing deletionPolicy %q onto Vhost %s before its delete: %w", want, key, err)
	}
	return nil
}

// --- manager setup ---

// RabbitMQVhostControlPlaneRefIndexKey is the field-indexer key under which a
// RabbitMQVhost on the management cluster is indexed by its resolved
// ControlPlane reference, "<namespace>/<name>".
const RabbitMQVhostControlPlaneRefIndexKey = "spec.controlPlaneRef"

// rabbitMQVhostControlPlaneRefExtractor returns the resolved
// "<namespace>/<name>" of obj's ControlPlane reference. An empty name indexes
// nothing.
func rabbitMQVhostControlPlaneRefExtractor(obj client.Object) []string {
	order, ok := obj.(*c5c3v1alpha1.RabbitMQVhost)
	if !ok || order.Spec.ControlPlaneRef.Name == "" {
		return nil
	}
	key := orderControlPlaneKey(order.Spec.ControlPlaneRef, order.Namespace)
	return []string{keystoneServiceControlPlaneRefIndexValue(key.Namespace, key.Name)}
}

// controlPlaneToRabbitMQVhostsMapper maps a ControlPlane event to the orders on
// the management cluster that reference it. Orders on target clusters come back
// on orderRefreshAfter. A List failure is logged and maps to nothing.
func controlPlaneToRabbitMQVhostsMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var orders c5c3v1alpha1.RabbitMQVhostList
		if err := c.List(ctx, &orders, client.MatchingFields{
			RabbitMQVhostControlPlaneRefIndexKey: keystoneServiceControlPlaneRefIndexValue(obj.GetNamespace(), obj.GetName()),
		}); err != nil {
			log.FromContext(ctx).Error(err, "listing RabbitMQVhosts for ControlPlane watch")
			return nil
		}
		requests := make([]reconcile.Request, 0, len(orders.Items))
		for i := range orders.Items {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&orders.Items[i])})
		}
		return requests
	}
}

// SetupWithManager registers the RabbitMQVhostReconciler with the multicluster
// manager.
func (r *RabbitMQVhostReconciler) SetupWithManager(mgr mcmanager.Manager) error {
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
//   - The Secret and PushSecret children in the ControlPlane's namespace, and
//     the Vhost, User and Permission children of every topology kind the
//     management cluster serves, mapped back by their labels. The topology
//     operator is optional infrastructure, so a kind the discovery probe does
//     not find is skipped; the ControlPlane controller's crdWatchGate restarts
//     the process once it appears.
//   - ControlPlane: the orders on the management cluster that reference it, on
//     the updates orderControlPlanePredicate passes.
//
// The index goes on the local field indexer, for the reason the KeystoneUser
// reconciler gives.
func (r *RabbitMQVhostReconciler) setupWithOptions(
	mgr mcmanager.Manager, opts crcontroller.TypedOptions[mcreconcile.Request],
) error {
	local := mgr.GetLocalManager()
	if err := local.GetFieldIndexer().IndexField(context.Background(), &c5c3v1alpha1.RabbitMQVhost{},
		RabbitMQVhostControlPlaneRefIndexKey, rabbitMQVhostControlPlaneRefExtractor); err != nil {
		return err
	}
	disco, err := discovery.NewDiscoveryClientForConfig(local.GetConfig())
	if err != nil {
		return fmt.Errorf("building discovery client for the topology CRD probe: %w", err)
	}
	served, _, err := probeOptionalWatches(disco, r.Scheme)
	if err != nil {
		return err
	}

	engageLocal := commonmulticluster.EngageLocalCluster
	engageNoProviders := commonmulticluster.EngageNoProviderClusters
	engageProviders := mcbuilder.WithEngageWithProviderClusters(true)
	servesKind := mcbuilder.WithClusterFilter(commonmulticluster.ClusterServesKind(rabbitMQVhostGVK))
	children := orderChildRequests(rabbitMQVhostLabelKeys)

	b := mcbuilder.ControllerManagedBy(mgr).
		WithOptions(opts).
		For(&c5c3v1alpha1.RabbitMQVhost{}, mcbuilder.WithPredicates(watch.CRUpdatePredicate()),
			engageLocal, engageProviders, servesKind).
		Owns(&corev1.Secret{}, engageLocal, engageProviders, servesKind).
		Watches(&corev1.Secret{}, children, engageLocal, engageNoProviders).
		Watches(&esov1alpha1.PushSecret{}, children, engageLocal, engageNoProviders)
	for _, gvk := range []schema.GroupVersionKind{messaging.VhostGVK, messaging.UserGVK, messaging.PermissionGVK} {
		if !served[gvk] {
			ctrl.Log.WithName("setup").V(1).Info(
				"the management cluster serves no topology kind; RabbitMQVhost skips its watch", "kind", gvk.String())
			continue
		}
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		b = b.Watches(obj, children, engageLocal, engageNoProviders)
	}
	return b.
		Watches(&c5c3v1alpha1.ControlPlane{},
			commonmulticluster.LocalRequests(controlPlaneToRabbitMQVhostsMapper(local.GetClient())),
			mcbuilder.WithPredicates(orderControlPlanePredicate()), engageLocal, engageNoProviders).
		// Reconcile answers an unresolvable cluster itself, so the wrapper that
		// would turn a cluster-not-found error into a success stays off.
		WithClusterNotFoundWrapper(false).
		Complete(r)
}
