// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/cobaltcore/internal/common/apply"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// rabbitMQVhostPushContentHashAnnotation carries the hash of the order's
// transport URL on its PushSecret, so a rotated user is pushed to OpenBao at
// once rather than at ESO's next refresh (stampOrderPush).
const rabbitMQVhostPushContentHashAnnotation = "c5c3.io/rabbitmqvhost-push-hash"

// rabbitMQVhostPushSyncedBeforeAnnotation records the PushSecret's
// status.syncedResourceVersion as it was when the content hash was stamped
// (orderPushFresh).
const rabbitMQVhostPushSyncedBeforeAnnotation = "c5c3.io/rabbitmqvhost-push-synced-before"

// splitBrokerEndpoint splits a host:port broker address into its host and a
// port between 1 and 65535. ok is false for anything else.
func splitBrokerEndpoint(endpoint string) (host string, port int32, ok bool) {
	host, portText, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", 0, false
	}
	parsed, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || parsed == 0 {
		return "", 0, false
	}
	return host, int32(parsed), true
}

// deliverCredentials backs the live user's credentials up to OpenBao and
// writes the Secret <name>-credentials beside the order, on the order's own
// cluster, and writes DeliveryReady. It delivers the generation
// status.passwordGeneration names, so the Secret switches to a successor on
// the pass status does. host and port are the broker's in-cluster address.
//
// The backup goes first, as for a KeystoneUser: a source Secret and a
// PushSecret in the ControlPlane's namespace push the credentials through that
// namespace's own store, and the delivered Secret is not written before ESO
// reports a push of the current transport URL. The order's namespace receives
// the Secret and nothing else.
func (r *RabbitMQVhostReconciler) deliverCredentials(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.RabbitMQVhost, cp *c5c3v1alpha1.ControlPlane,
	cluster, host string, port int32,
) (ctrl.Result, error) {
	fail := rabbitMQVhostFail(order, conditionTypeRabbitMQVhostDeliveryReady)
	requeue := ctrl.Result{RequeueAfter: rabbitMQVhostRequeueAfter}
	ref := rabbitMQVhostRef(order, cluster)
	location := ref.location()

	// The Secret is read on the order's cluster, so the broker address is
	// resolved for that cluster. Off the management cluster that is the
	// published endpoint, and only a ControlPlane edit can publish one, so
	// there is no requeue.
	endpoint := rabbitMQVhostEndpoint(cp, cluster, net.JoinHostPort(host, strconv.Itoa(int(port))))
	if endpoint == "" {
		fail(reasonRabbitMQVhostMessagingNotPublished, fmt.Sprintf(
			"the order lives on %s, which cannot reach the in-cluster RabbitMQ Service %s; set "+
				"spec.infrastructure.publishedMessagingEndpoint on ControlPlane %s/%s",
			location, host, cp.Namespace, cp.Name))
		return ctrl.Result{}, nil
	}
	deliveredHost, deliveredPort, ok := splitBrokerEndpoint(endpoint)
	if !ok {
		// Only a published value can fail: the schema admits a port of five
		// digits above 65535.
		fail(reasonKeystoneUserDeliveryError, fmt.Sprintf(
			"spec.infrastructure.publishedMessagingEndpoint %q of ControlPlane %s/%s is not a host and a port "+
				"between 1 and 65535", endpoint, cp.Namespace, cp.Name))
		return ctrl.Result{}, nil
	}

	gen := order.Status.PasswordGeneration
	if gen == 0 {
		fail(reasonRabbitMQVhostWaitingForPassword, rabbitMQVhostNotProvisionedMessage)
		return ctrl.Result{}, nil
	}
	pwKey := types.NamespacedName{Namespace: cp.Namespace, Name: rabbitMQVhostPasswordSecretName(order, cluster, gen)}
	pwSecret := &corev1.Secret{}
	if err := r.Get(ctx, pwKey, pwSecret); client.IgnoreNotFound(err) != nil {
		fail(reasonKeystoneUserDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("reading the password Secret %s: %w", pwKey, err)
	}
	password := pwSecret.Data[rabbitMQVhostPasswordKey]
	if len(password) == 0 {
		fail(reasonRabbitMQVhostWaitingForPassword, fmt.Sprintf("the password of generation %d is not available yet", gen))
		return requeue, nil
	}

	username := rabbitMQVhostBrokerUserName(order, cluster, gen)
	vhost := rabbitMQVhostName(order, cluster)
	transportURL, hash := messaging.BuildTransportURLForVhost(username, string(password), deliveredHost,
		deliveredPort, vhost)
	data := map[string][]byte{
		rabbitMQVhostTransportURLKey: []byte(transportURL),
		rabbitMQVhostHostKey:         []byte(deliveredHost),
		rabbitMQVhostPortKey:         []byte(strconv.Itoa(int(deliveredPort))),
		rabbitMQVhostUsernameKey:     []byte(username),
		rabbitMQVhostPasswordKey:     password,
		rabbitMQVhostVhostKey:        []byte(vhost),
	}

	sourceName := rabbitMQVhostSourceSecretName(order, cluster)
	if err := ensureOrderSecret(ctx, r.Client, ref, sourceName, cp.Namespace, func(secret *corev1.Secret) error {
		for key, value := range data {
			secret.Data[key] = value
		}
		return nil
	}); err != nil {
		fail(reasonKeystoneUserDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("assembling order source Secret %s/%s: %w", cp.Namespace, sourceName, err)
	}

	pushName := rabbitMQVhostPushSecretName(order, cluster)
	push := accountPushSecretSpec(pushName, cp.Namespace, sourceName,
		rabbitMQVhostRemoteKeyFor(cp, ref.childPrefix()), effectiveControlPlaneStoreRef(cp))
	if err := r.ensureRabbitMQVhostChild(ctx, order, cluster, push); err != nil {
		fail(reasonKeystoneUserDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("ensuring order PushSecret %s/%s: %w", cp.Namespace, pushName, err)
	}
	pushKey := types.NamespacedName{Namespace: cp.Namespace, Name: pushName}
	if err := stampOrderPush(ctx, r.Client, pushKey, rabbitMQVhostPushContentHashAnnotation,
		rabbitMQVhostPushSyncedBeforeAnnotation, hash); err != nil {
		fail(reasonKeystoneUserDeliveryError, err.Error())
		return ctrl.Result{}, err
	}
	pushed := &esov1alpha1.PushSecret{}
	if err := r.Get(ctx, pushKey, pushed); err != nil {
		fail(reasonKeystoneUserDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("reading order PushSecret %s/%s: %w", cp.Namespace, pushName, err)
	}
	if !orderPushFresh(pushed, rabbitMQVhostPushContentHashAnnotation, rabbitMQVhostPushSyncedBeforeAnnotation, hash) {
		fail(reasonKeystoneUserBackupNotSynced, fmt.Sprintf(
			"the credentials are not backed up to OpenBao yet (PushSecret %s/%s)", cp.Namespace, pushName))
		return requeue, nil
	}

	secretKey := types.NamespacedName{Namespace: order.Namespace, Name: rabbitMQVhostCredentialsSecretName(order)}
	// Ownership is decided from live state: the order's cluster may be a target
	// cluster, whose cache can trail it.
	live := &corev1.Secret{}
	switch err := commonmulticluster.LiveReader(oc).Get(ctx, secretKey, live); {
	case apierrors.IsNotFound(err):
	case err != nil:
		fail(reasonKeystoneUserDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("checking for a pre-existing Secret %s: %w", secretKey, err)
	case !metav1.IsControlledBy(live, order):
		fail(reasonKeystoneUserDeliveryRefused, fmt.Sprintf(
			"Secret %s exists and is not owned by this order; delete it or rename the order", secretKey))
		return ctrl.Result{}, nil
	}

	// The Secret carries the name and namespace labels and no cluster label, for
	// the reason the KeystoneUser delivery gives.
	labels := ref.childLabels()
	delete(labels, rabbitMQVhostLabelKeys.Cluster)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretKey.Name, Namespace: secretKey.Namespace, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
	if err := controllerutil.SetControllerReference(order, secret, r.Scheme); err != nil {
		fail(reasonKeystoneUserDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("delivering Secret %s: %w", secretKey, err)
	}
	// ForceOwnership is the repair: the field manager owns the six keys it
	// applies, so an edited key is rewritten, and a key somebody else adds stays.
	if err := apply.EnsureUnownedObject(ctx, oc, r.Scheme, secret, apply.FieldManager); err != nil {
		fail(reasonKeystoneUserDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("delivering Secret %s: %w", secretKey, err)
	}

	// EnsureUnownedObject decoded the apply response into secret.
	if !bytes.Equal(secret.Data[rabbitMQVhostPasswordKey], password) {
		fail(reasonRabbitMQVhostWaitingForPassword, "the delivered Secret does not carry the current password yet")
		return requeue, nil
	}

	order.Status.Host = deliveredHost
	order.Status.Port = deliveredPort
	order.Status.SecretName = secretKey.Name
	order.Status.SecretKeys = slices.Clone(rabbitMQVhostSecretKeys)
	rabbitMQVhostSetTrue(order, conditionTypeRabbitMQVhostDeliveryReady, reasonKeystoneUserDelivered, fmt.Sprintf(
		"credentials are delivered in Secret %q (keys %s) in namespace %q on %s",
		secretKey.Name, strings.Join(rabbitMQVhostSecretKeys, ", "), secretKey.Namespace, location))
	return ctrl.Result{}, nil
}
