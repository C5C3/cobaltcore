// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/cobaltcore/internal/common/apply"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// keystoneUserPushContentHashAnnotation carries the hash of the order's
// clouds.yaml on its PushSecret, so a changed document is pushed to OpenBao at
// once rather than at ESO's next refresh. It is a key of its own, apart from the
// KeystoneService and ControlPlane ones.
const keystoneUserPushContentHashAnnotation = "c5c3.io/keystoneuser-push-hash" //nolint:gosec // G101 false positive: annotation key, not a credential.

// keystoneUserPushSyncedBeforeAnnotation records the PushSecret's
// status.syncedResourceVersion as it was when the content hash was stamped. ESO
// sets that field only after a successful push, from the object's labels and
// annotations, so a value other than the recorded one proves a push that saw the
// current hash (keystoneUserPushFresh).
const keystoneUserPushSyncedBeforeAnnotation = "c5c3.io/keystoneuser-push-synced-before"

// deliverCredentials backs the password of generation gen up to OpenBao and
// writes the Secret <name>-credentials beside the order, on the order's own
// cluster, and writes DeliveryReady.
//
// The backup goes first: a source Secret and a PushSecret in the ControlPlane's
// namespace push the document through that namespace's own store, and the
// delivered Secret is not written before ESO reports a push of the current
// document. The order's namespace receives the Secret and nothing else, so
// nothing there can reach OpenBao.
func (r *KeystoneUserReconciler) deliverCredentials(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.KeystoneUser, cp *c5c3v1alpha1.ControlPlane,
	cluster string, gen int64,
) (ctrl.Result, error) {
	fail := keystoneUserFail(order, conditionTypeKeystoneUserDeliveryReady)
	requeue := ctrl.Result{RequeueAfter: korcRequeueAfter}
	location := keystoneUserLocation(cluster)

	// The document is read on the order's cluster, so its auth_url is resolved for
	// that cluster. Off Keystone's own cluster that is the published endpoint, and
	// only a ControlPlane edit can publish one, so there is no requeue.
	ref := keystoneUserClusterRef(cluster)
	authURL := korcAuthURL(cp, ref)
	if authURL == "" {
		fail(reasonKeystoneUserKeystoneNotPublished, fmt.Sprintf(
			"the order lives on %s, which cannot reach the in-cluster Keystone Service; set "+
				"spec.services.keystone.publicEndpoint or spec.services.keystone.gateway on ControlPlane %s/%s",
			location, cp.Namespace, cp.Name))
		return ctrl.Result{}, nil
	}

	pwKey := types.NamespacedName{Namespace: cp.Namespace, Name: keystoneUserPasswordSecretName(order, cluster, gen)}
	pwSecret := &corev1.Secret{}
	if err := r.Get(ctx, pwKey, pwSecret); client.IgnoreNotFound(err) != nil {
		fail(reasonKeystoneUserDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("reading order password Secret %s: %w", pwKey, err)
	}
	password := pwSecret.Data[serviceAccountPasswordKey]
	if len(password) == 0 {
		fail(reasonWaitingForServiceAccounts, fmt.Sprintf("the password of generation %d is not available yet", gen))
		return requeue, nil
	}

	userName := keystoneUserName(order)
	domain := adminDomainName(cp)
	cloudsYAML := []byte(buildUserCloudsYAML(cp, userName, domain, string(password), ref))

	sourceName := keystoneUserSourceSecretName(order, cluster)
	if err := r.ensureKeystoneUserSecret(ctx, order, cluster, sourceName, cp.Namespace, func(secret *corev1.Secret) error {
		secret.Data[serviceAccountPasswordKey] = password
		secret.Data["username"] = []byte(userName)
		secret.Data["user_domain_name"] = []byte(domain)
		secret.Data["auth_url"] = []byte(authURL)
		secret.Data["region_name"] = []byte(korcRegion(cp))
		secret.Data[appCredCloudsYAMLKey] = cloudsYAML
		return nil
	}); err != nil {
		fail(reasonKeystoneUserDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("assembling order source Secret %s/%s: %w", cp.Namespace, sourceName, err)
	}

	pushName := keystoneUserPushSecretName(order, cluster)
	push := accountPushSecretSpec(pushName, cp.Namespace, sourceName,
		keystoneUserRemoteKeyFor(cp, keystoneUserChildPrefix(order, cluster)), effectiveControlPlaneStoreRef(cp))
	if err := r.ensureKeystoneUserChild(ctx, order, cluster, push); err != nil {
		fail(reasonKeystoneUserDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("ensuring order PushSecret %s/%s: %w", cp.Namespace, pushName, err)
	}
	sum := sha256.Sum256(cloudsYAML)
	hash := hex.EncodeToString(sum[:])
	pushKey := types.NamespacedName{Namespace: cp.Namespace, Name: pushName}
	if err := stampKeystoneUserPush(ctx, r.Client, pushKey, hash); err != nil {
		fail(reasonKeystoneUserDeliveryError, err.Error())
		return ctrl.Result{}, err
	}
	pushed := &esov1alpha1.PushSecret{}
	if err := r.Get(ctx, pushKey, pushed); err != nil {
		fail(reasonKeystoneUserDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("reading order PushSecret %s/%s: %w", cp.Namespace, pushName, err)
	}
	if !keystoneUserPushFresh(pushed, hash) {
		fail(reasonKeystoneUserBackupNotSynced, fmt.Sprintf(
			"the password is not backed up to OpenBao yet (PushSecret %s/%s)", cp.Namespace, pushName))
		return requeue, nil
	}

	secretKey := types.NamespacedName{Namespace: order.Namespace, Name: keystoneUserCredentialsSecretName(order)}
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

	// The Secret is the order's through its controller reference, not its labels.
	// It carries the name and namespace labels so its owner can select it, and no
	// cluster label: that label is what makes an object a labelled child
	// (isKeystoneUserChild, the teardown's selector), and this Secret is never one.
	labels := keystoneUserChildLabels(order, cluster)
	delete(labels, keystoneUserClusterLabel)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretKey.Name,
			Namespace: secretKey.Namespace,
			Labels:    labels,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			appCredCloudsYAMLKey:      cloudsYAML,
			serviceAccountPasswordKey: password,
		},
	}
	// The order and its Secret share a cluster and a namespace, so the owner
	// reference is legal on either cluster and the garbage collector reaps the
	// Secret with the order.
	if err := controllerutil.SetControllerReference(order, secret, r.Scheme); err != nil {
		fail(reasonKeystoneUserDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("delivering Secret %s: %w", secretKey, err)
	}
	// ForceOwnership is the repair: the field manager owns the two keys it applies,
	// so an edited key is rewritten, and a key somebody else adds stays.
	if err := apply.EnsureUnownedObject(ctx, oc, r.Scheme, secret, apply.FieldManager); err != nil {
		fail(reasonKeystoneUserDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("delivering Secret %s: %w", secretKey, err)
	}

	// EnsureUnownedObject decoded the apply response into secret.
	if !bytes.Equal(secret.Data[serviceAccountPasswordKey], password) {
		fail(reasonWaitingForServiceAccounts, "the delivered Secret does not carry the current password yet")
		return requeue, nil
	}

	order.Status.SecretName = secretKey.Name
	order.Status.SecretKeys = []string{appCredCloudsYAMLKey, serviceAccountPasswordKey}
	keystoneUserSetTrue(order, conditionTypeKeystoneUserDeliveryReady, reasonKeystoneUserDelivered, fmt.Sprintf(
		"credentials are delivered in Secret %q (keys clouds.yaml, password) in namespace %q on %s",
		secretKey.Name, secretKey.Namespace, location))
	return ctrl.Result{}, nil
}

// stampKeystoneUserPush stamps the document hash on the order's PushSecret, so
// ESO pushes a changed document at once rather than at its next refresh, and
// records the syncedResourceVersion the stamp was written against. An unchanged
// hash writes nothing, and a missing PushSecret is a no-op: the read after it
// reports that. The update retries on conflict, because ESO writes the
// PushSecret's status on every push; the conflict is also what keeps a cached
// syncedResourceVersion older than the live one from being recorded.
func stampKeystoneUserPush(ctx context.Context, c client.Client, key types.NamespacedName, hash string) error {
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		ps := &esov1alpha1.PushSecret{}
		if err := c.Get(ctx, key, ps); err != nil {
			return client.IgnoreNotFound(err)
		}
		if ps.Annotations[keystoneUserPushContentHashAnnotation] == hash {
			return nil
		}
		if ps.Annotations == nil {
			ps.Annotations = map[string]string{}
		}
		ps.Annotations[keystoneUserPushContentHashAnnotation] = hash
		ps.Annotations[keystoneUserPushSyncedBeforeAnnotation] = ps.Status.SyncedResourceVersion
		return c.Update(ctx, ps)
	}); err != nil {
		return fmt.Errorf("stamping order PushSecret %s: %w", key, err)
	}
	return nil
}

// keystoneUserPushFresh reports whether ESO has pushed the document whose hash
// is hash. Ready alone does not say so: after a rotation it stays True from the
// previous push until ESO has processed the new stamp, and OpenBao would still
// hold a password Keystone no longer accepts.
func keystoneUserPushFresh(ps *esov1alpha1.PushSecret, hash string) bool {
	return pushSecretReady(ps) &&
		ps.Annotations[keystoneUserPushContentHashAnnotation] == hash &&
		ps.Status.SyncedResourceVersion != ps.Annotations[keystoneUserPushSyncedBeforeAnnotation]
}
