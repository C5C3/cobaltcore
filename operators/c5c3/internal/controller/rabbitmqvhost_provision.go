// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// rabbitMQVhostTopologyManifest is the file that installs the topology
// operator, which TopologyOperatorNotInstalled names.
const rabbitMQVhostTopologyManifest = "deploy/flux-system/releases/messaging-topology-operator.yaml"

// provisionVhost keeps the order's vhost provisioned and its user rotated and
// writes VhostReady. ok is true once a user generation is live, so the delivery
// leg has a password to deliver; res carries the time to the next rotation and
// to the end of a running grace period.
//
// The vhost is one Vhost CR, applied on every pass so a changed deletion policy
// reaches it. Each generation N is one password Secret password-v<N>, one User
// user-v<N> that imports the broker user <vhost>-v<N> from it, and one
// Permission permission-v<N> granting that user ".*" on the vhost. A rotation
// is a new generation, not a new password for the live user, so a consumer
// never holds a password the broker refuses: the successor is created, status
// switches to it once the topology operator reports its User and Permission
// Ready, the delivery leg rewrites the Secret in the same pass, and the
// superseded generation is deleted after the grace period.
func (r *RabbitMQVhostReconciler) provisionVhost(
	ctx context.Context, order *c5c3v1alpha1.RabbitMQVhost, cp *c5c3v1alpha1.ControlPlane, cluster string,
) (bool, ctrl.Result, error) {
	fail := rabbitMQVhostFail(order, conditionTypeRabbitMQVhostVhostReady)
	requeue := ctrl.Result{RequeueAfter: rabbitMQVhostRequeueAfter}
	now := r.now()
	interval, _ := rabbitMQVhostRotation(order)

	// The password is backed up through the ControlPlane namespace's own store,
	// so an ESO or OpenBao outage surfaces here rather than at the delivery.
	storeRef := effectiveControlPlaneStoreRef(cp)
	ready, err := secrets.IsStoreRefReady(ctx, r.Client, storeRef, cp.Namespace)
	if err != nil {
		err = fmt.Errorf("checking %s %q in namespace %q: %w", storeRef.Kind, storeRef.Name, cp.Namespace, err)
		fail(reasonRabbitMQVhostError, err.Error())
		return false, ctrl.Result{}, err
	}
	if !ready {
		fail(reasonServiceAccountStoreNotReady, fmt.Sprintf(
			"%s %q in namespace %q is not ready; the password cannot be backed up to OpenBao",
			storeRef.Kind, storeRef.Name, cp.Namespace))
		return false, requeue, nil
	}

	vhostName := rabbitMQVhostName(order, cluster)
	vhost, ok, res, err := r.applyTopologyChild(ctx, order, cluster, messaging.BuildVhost(
		rabbitMQVhostChildName(order, cluster), cp.Namespace, vhostName, cp.Spec.Infrastructure.Messaging.ClusterRef.Name,
		strings.ToLower(rabbitMQVhostDeletionPolicy(order))), fail)
	if !ok {
		return false, res, err
	}
	if msg := messaging.TopologyFailure(vhost); msg != "" {
		fail(reasonRabbitMQVhostFailed, fmt.Sprintf("the topology operator cannot create vhost %q: %s", vhostName, msg))
		return false, requeue, nil
	}
	if !messaging.TopologyReady(vhost) {
		fail(reasonRabbitMQVhostWaitingForVhost, fmt.Sprintf("Vhost %s/%s is not Ready yet",
			vhost.GetNamespace(), vhost.GetName()))
		return false, requeue, nil
	}
	order.Status.Vhost = vhostName

	children, err := r.vhostChildren(ctx, order, cluster, cp.Namespace)
	if err != nil {
		if meta.IsNoMatchError(err) {
			fail(reasonRabbitMQVhostTopologyOperatorNotInstalled, topologyNotInstalledMessage(err))
			return false, ctrl.Result{RequeueAfter: orderRefreshAfter}, nil
		}
		fail(reasonRabbitMQVhostError, err.Error())
		return false, ctrl.Result{}, err
	}
	cur := order.Status.PasswordGeneration
	desired := vhostDesiredGeneration(ctx, order, interval, children, now)
	if err := r.pruneVhostStrayGenerations(ctx, order, children, desired); err != nil {
		fail(reasonRabbitMQVhostError, err.Error())
		return false, ctrl.Result{}, err
	}

	var result ctrl.Result
	if desired > cur {
		result, err = r.mintVhostGeneration(ctx, order, cp, cluster, desired, now)
		if err != nil {
			return false, ctrl.Result{}, err
		}
	} else {
		rabbitMQVhostSetTrue(order, conditionTypeRabbitMQVhostVhostReady, reasonRabbitMQVhostProvisioned,
			vhostProvisionedMessage(order, cp))
	}

	if err := r.sweepVhostGrace(ctx, order, cluster, cp.Namespace, children, now); err != nil {
		fail(reasonRabbitMQVhostError, err.Error())
		return false, ctrl.Result{}, err
	}

	order.Status.NextRotation = vhostNextRotation(order, interval)
	return order.Status.PasswordGeneration > 0, commonreconcile.ShortestRequeue(result,
		requeueAt(order.Status.NextRotation, now),
		requeueAt(order.Status.PreviousPasswordDeleteAt, now)), nil
}

// applyTopologyChild applies obj, a Vhost, User or Permission of the order,
// and returns the live object the apply answered with. A false ok means a
// refusal or an error was written through fail, and res and err are the pass's:
// a management cluster that serves no topology kind is a wait for an install,
// and a live object of the child's name the order did not create is a refusal
// only its removal lifts.
func (r *RabbitMQVhostReconciler) applyTopologyChild(
	ctx context.Context, order *c5c3v1alpha1.RabbitMQVhost, cluster string, obj *unstructured.Unstructured,
	fail func(reason, message string),
) (*unstructured.Unstructured, bool, ctrl.Result, error) {
	key := client.ObjectKeyFromObject(obj)
	kind := obj.GetKind()
	if err := r.ensureRabbitMQVhostChild(ctx, order, cluster, obj); err != nil {
		switch {
		case meta.IsNoMatchError(err):
			fail(reasonRabbitMQVhostTopologyOperatorNotInstalled, topologyNotInstalledMessage(err))
			return nil, false, ctrl.Result{RequeueAfter: orderRefreshAfter}, nil
		case errors.Is(err, errOrderChildForeign):
			fail(reasonRabbitMQVhostError, err.Error())
			return nil, false, ctrl.Result{}, nil
		default:
			err = fmt.Errorf("ensuring %s %s: %w", kind, key, err)
			fail(reasonRabbitMQVhostError, err.Error())
			return nil, false, ctrl.Result{}, err
		}
	}
	// EnsureUnownedObject decoded the apply response, status included, into obj.
	return obj, true, ctrl.Result{}, nil
}

// topologyNotInstalledMessage is the TopologyOperatorNotInstalled message for
// the NoMatch error err, which names the kind the cluster does not serve.
func topologyNotInstalledMessage(err error) string {
	resource := "the topology kinds"
	var noMatch *meta.NoKindMatchError
	if errors.As(err, &noMatch) {
		resource = strings.ToLower(noMatch.GroupKind.Kind) + "s." + noMatch.GroupKind.Group
	}
	return fmt.Sprintf("the cluster serves no %s; install the RabbitMQ Messaging Topology Operator (%s)",
		resource, rabbitMQVhostTopologyManifest)
}

// vhostGenerations are the order's generation children, keyed by generation.
type vhostGenerations struct {
	users, permissions map[int64]*unstructured.Unstructured
	secrets            map[int64]*corev1.Secret
}

// vhostChildren lists the order's Users, Permissions and Secrets in childNS
// and keys the generation children by their generation. A labelled object
// outside the order's prefix is not its child and is left out.
func (r *RabbitMQVhostReconciler) vhostChildren(
	ctx context.Context, order *c5c3v1alpha1.RabbitMQVhost, cluster, childNS string,
) (vhostGenerations, error) {
	selector := client.MatchingLabels(rabbitMQVhostRef(order, cluster).childLabels())
	children := vhostGenerations{
		users:       map[int64]*unstructured.Unstructured{},
		permissions: map[int64]*unstructured.Unstructured{},
		secrets:     map[int64]*corev1.Secret{},
	}

	for _, kind := range []struct {
		list   *unstructured.UnstructuredList
		prefix string
		into   map[int64]*unstructured.Unstructured
	}{
		{topologyList(messaging.UserGVK), rabbitMQVhostUserPrefix(order, cluster), children.users},
		{topologyList(messaging.PermissionGVK), rabbitMQVhostPermissionPrefix(order, cluster), children.permissions},
	} {
		if err := r.List(ctx, kind.list, client.InNamespace(childNS), selector); err != nil {
			return vhostGenerations{}, fmt.Errorf("listing the order's %s: %w", kind.list.GetKind(), err)
		}
		for i := range kind.list.Items {
			obj := &kind.list.Items[i]
			if gen, ok := credentialGenerationOf(obj.GetName(), kind.prefix); ok && ownsRabbitMQVhostChild(obj, order, cluster) {
				kind.into[gen] = obj
			}
		}
	}

	var secretList corev1.SecretList
	if err := r.List(ctx, &secretList, client.InNamespace(childNS), selector); err != nil {
		return vhostGenerations{}, fmt.Errorf("listing the order's Secrets: %w", err)
	}
	passwordPrefix := rabbitMQVhostPasswordSecretPrefix(order, cluster)
	for i := range secretList.Items {
		secret := &secretList.Items[i]
		if gen, ok := credentialGenerationOf(secret.Name, passwordPrefix); ok && ownsRabbitMQVhostChild(secret, order, cluster) {
			children.secrets[gen] = secret
		}
	}
	return children, nil
}

// vhostDesiredGeneration decides the generation the pass works toward, the
// way desiredGeneration does for an application credential, without an
// expiry. Before the first generation is live it is the declared generation.
// After it, a User above the live generation is a rotation in flight, and the
// highest one is continued; one the broker refused is given up once a higher
// generation is declared, so raising spec.passwordGeneration replaces a failed
// successor. Otherwise a rotation starts, toward the declared generation or
// the next one, whichever is higher, when the declared generation is above the
// live one, the schedule is due, or a piece of the live generation is lost: its
// password, its User or its Permission. No rotation starts while a grace period
// runs, so the order never holds more than two generations.
func vhostDesiredGeneration(
	ctx context.Context, order *c5c3v1alpha1.RabbitMQVhost, interval time.Duration, children vhostGenerations,
	now time.Time,
) int64 {
	declared := rabbitMQVhostGeneration(order)
	cur := order.Status.PasswordGeneration
	if cur == 0 {
		return declared
	}
	inFlight := int64(0)
	for gen, user := range children.users {
		if gen > cur && (messaging.TopologyFailure(user) == "" || gen >= declared) {
			inFlight = max(inFlight, gen)
		}
	}
	if inFlight > 0 {
		return inFlight
	}
	if order.Status.PreviousPasswordDeleteAt != nil {
		return cur
	}

	next := vhostNextRotation(order, interval)
	rotate := declared > cur || (next != nil && !now.Before(next.Time))
	switch secret := children.secrets[cur]; {
	case secret == nil || len(secret.Data[rabbitMQVhostPasswordKey]) == 0:
		log.FromContext(ctx).Info(fmt.Sprintf("the password of generation %d is lost; rotating", cur))
		rotate = true
	case children.users[cur] == nil || children.permissions[cur] == nil:
		log.FromContext(ctx).Info(fmt.Sprintf("the User or Permission of generation %d is lost; rotating", cur))
		rotate = true
	}
	if !rotate {
		return cur
	}
	return max(declared, cur+1)
}

// vhostNextRotation is when the schedule rotates the live user: the last
// rotation plus the interval. It is nil with the schedule off and before the
// first generation is live.
func vhostNextRotation(order *c5c3v1alpha1.RabbitMQVhost, interval time.Duration) *metav1.Time {
	last := order.Status.LastRotation
	if interval <= 0 || last == nil {
		return nil
	}
	return &metav1.Time{Time: last.Add(interval)}
}

// pruneVhostStrayGenerations deletes every User, Permission and password Secret
// of a generation other than the live one, the superseded one while its grace
// period runs, and the one the pass works toward, so a pass that was
// interrupted leaves nothing behind.
func (r *RabbitMQVhostReconciler) pruneVhostStrayGenerations(
	ctx context.Context, order *c5c3v1alpha1.RabbitMQVhost, children vhostGenerations, desired int64,
) error {
	keep := func(gen int64) bool {
		return gen == order.Status.PasswordGeneration || gen == desired ||
			(order.Status.PreviousPasswordDeleteAt != nil && gen == order.Status.PreviousPasswordGeneration)
	}
	var stray []client.Object
	for gen, permission := range children.permissions {
		if !keep(gen) {
			stray = append(stray, permission)
		}
	}
	for gen, user := range children.users {
		if !keep(gen) {
			stray = append(stray, user)
		}
	}
	for gen, secret := range children.secrets {
		if !keep(gen) {
			stray = append(stray, secret)
		}
	}
	for _, obj := range stray {
		if obj.GetDeletionTimestamp() != nil {
			continue
		}
		log.FromContext(ctx).Info("removing a stray vhost user generation", "name", obj.GetName())
		if err := client.IgnoreNotFound(r.Delete(ctx, obj)); err != nil {
			return fmt.Errorf("deleting stray vhost user generation %s: %w", obj.GetName(), err)
		}
	}
	return nil
}

// mintVhostGeneration ensures generation gen's password Secret, User and
// Permission, and switches status to it once the topology operator reports
// both Ready. The Permission is applied only once the User is Ready: the
// broker refuses permissions for a user it does not hold yet.
func (r *RabbitMQVhostReconciler) mintVhostGeneration(
	ctx context.Context, order *c5c3v1alpha1.RabbitMQVhost, cp *c5c3v1alpha1.ControlPlane, cluster string,
	gen int64, now time.Time,
) (ctrl.Result, error) {
	fail := rabbitMQVhostFail(order, conditionTypeRabbitMQVhostVhostReady)
	requeue := ctrl.Result{RequeueAfter: rabbitMQVhostRequeueAfter}
	_, grace := rabbitMQVhostRotation(order)
	cur := order.Status.PasswordGeneration
	clusterName := cp.Spec.Infrastructure.Messaging.ClusterRef.Name
	brokerUser := rabbitMQVhostBrokerUserName(order, cluster, gen)

	secretName := rabbitMQVhostPasswordSecretName(order, cluster, gen)
	if err := ensureOrderSecret(ctx, r.Client, rabbitMQVhostRef(order, cluster), secretName, cp.Namespace,
		func(secret *corev1.Secret) error {
			secret.Data[rabbitMQVhostUsernameKey] = []byte(brokerUser)
			// The topology operator's admission webhook refuses a User whose
			// imported Secret lacks the label, and its informer caches only
			// labelled Secrets.
			if secret.Labels == nil {
				secret.Labels = map[string]string{}
			}
			secret.Labels[messaging.TopologyOperatorLabel] = "true"
			return generatedVhostPasswordMutator(secret)
		}); err != nil {
		err = fmt.Errorf("ensuring the password Secret of generation %d: %w", gen, err)
		fail(reasonRabbitMQVhostError, err.Error())
		return ctrl.Result{}, err
	}

	user, ok, res, err := r.applyTopologyChild(ctx, order, cluster,
		messaging.BuildUser(rabbitMQVhostUserName(order, cluster, gen), cp.Namespace, clusterName, secretName), fail)
	if !ok {
		return res, err
	}
	var permission *unstructured.Unstructured
	if messaging.TopologyReady(user) {
		permission, ok, res, err = r.applyTopologyChild(ctx, order, cluster, messaging.BuildPermission(
			rabbitMQVhostPermissionName(order, cluster, gen), cp.Namespace, clusterName,
			rabbitMQVhostName(order, cluster), brokerUser), fail)
		if !ok {
			return res, err
		}
	}
	for _, obj := range []*unstructured.Unstructured{user, permission} {
		if obj == nil {
			continue
		}
		if msg := messaging.TopologyFailure(obj); msg != "" {
			fail(reasonRabbitMQVhostFailed, fmt.Sprintf(
				"the topology operator cannot create %s %s/%s of generation %d: %s",
				obj.GetKind(), obj.GetNamespace(), obj.GetName(), gen, msg))
			return requeue, nil
		}
	}
	if permission == nil || !messaging.TopologyReady(user) || !messaging.TopologyReady(permission) {
		if cur == 0 {
			fail(reasonRabbitMQVhostWaitingForUser, fmt.Sprintf(
				"User %s/%s or Permission %s/%s of generation %d is not Ready yet", cp.Namespace,
				rabbitMQVhostUserName(order, cluster, gen), cp.Namespace,
				rabbitMQVhostPermissionName(order, cluster, gen), gen))
			return requeue, nil
		}
		// The delivered user is valid while its successor is created, so a
		// scheduled rotation does not read as a failure.
		rabbitMQVhostSetTrue(order, conditionTypeRabbitMQVhostVhostReady, reasonRabbitMQVhostProvisioned,
			fmt.Sprintf("%s; generation %d is being created", vhostProvisionedMessage(order, cp), gen))
		return requeue, nil
	}

	if cur > 0 {
		order.Status.PreviousPasswordGeneration = cur
		order.Status.PreviousPasswordDeleteAt = &metav1.Time{Time: now.Add(grace)}
	}
	order.Status.PasswordGeneration = gen
	order.Status.Username = brokerUser
	order.Status.LastRotation = &metav1.Time{Time: now}
	rabbitMQVhostSetTrue(order, conditionTypeRabbitMQVhostVhostReady, reasonRabbitMQVhostProvisioned,
		vhostProvisionedMessage(order, cp))
	return ctrl.Result{}, nil
}

// sweepVhostGrace ends a running grace period once its time has come: it
// deletes the superseded Permission and User, and the topology operator takes
// the user off the broker. On the pass that no longer lists the User, the
// superseded password Secret goes too and the previous* fields are cleared.
// The User child watch brings the order back once the topology operator has
// let go.
func (r *RabbitMQVhostReconciler) sweepVhostGrace(
	ctx context.Context, order *c5c3v1alpha1.RabbitMQVhost, cluster, childNS string, children vhostGenerations,
	now time.Time,
) error {
	deleteAt := order.Status.PreviousPasswordDeleteAt
	if deleteAt == nil || now.Before(deleteAt.Time) {
		return nil
	}
	prev := order.Status.PreviousPasswordGeneration
	if user, listed := children.users[prev]; listed {
		for _, obj := range []*unstructured.Unstructured{children.permissions[prev], user} {
			if obj == nil || obj.GetDeletionTimestamp() != nil {
				continue
			}
			if err := client.IgnoreNotFound(r.Delete(ctx, obj)); err != nil {
				return fmt.Errorf("deleting the %s of superseded generation %d: %w", obj.GetKind(), prev, err)
			}
		}
		return nil
	}

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: childNS, Name: rabbitMQVhostPasswordSecretName(order, cluster, prev),
	}}
	if err := client.IgnoreNotFound(r.Delete(ctx, secret)); err != nil {
		return fmt.Errorf("deleting the password Secret of superseded generation %d: %w", prev, err)
	}
	log.FromContext(ctx).Info(fmt.Sprintf("deleted superseded user generation %d (%s)",
		prev, rabbitMQVhostBrokerUserName(order, cluster, prev)))
	order.Status.PreviousPasswordGeneration = 0
	order.Status.PreviousPasswordDeleteAt = nil
	return nil
}

// generatedVhostPasswordMutator fills the password key of a generation's
// password Secret, once. A value already present is preserved: the broker
// holds the user with it.
func generatedVhostPasswordMutator(secret *corev1.Secret) error {
	if len(secret.Data[rabbitMQVhostPasswordKey]) > 0 {
		return nil
	}
	v, err := generateAppCredSecretValue()
	if err != nil {
		return err
	}
	secret.Data[rabbitMQVhostPasswordKey] = []byte(v)
	return nil
}

// vhostProvisionedMessage is VhostReady's True message for the live
// generation.
func vhostProvisionedMessage(order *c5c3v1alpha1.RabbitMQVhost, cp *c5c3v1alpha1.ControlPlane) string {
	return fmt.Sprintf("vhost %q and user %q (generation %d) are provisioned on RabbitmqCluster %s/%s",
		order.Status.Vhost, order.Status.Username, order.Status.PasswordGeneration,
		cp.Namespace, cp.Spec.Infrastructure.Messaging.ClusterRef.Name)
}
