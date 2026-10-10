// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/cobaltcore/internal/common/apply"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// keystoneApplicationCredentialPushContentHashAnnotation carries the hash of
// the order's clouds.yaml on its PushSecret, so a rotated credential is pushed
// to OpenBao at once rather than at ESO's next refresh (stampOrderPush).
const keystoneApplicationCredentialPushContentHashAnnotation = "c5c3.io/keystoneapplicationcredential-push-hash" //nolint:gosec // G101 false positive: annotation key, not a credential.

// keystoneApplicationCredentialPushSyncedBeforeAnnotation records the
// PushSecret's status.syncedResourceVersion as it was when the content hash was
// stamped (orderPushFresh).
const keystoneApplicationCredentialPushSyncedBeforeAnnotation = "c5c3.io/keystoneapplicationcredential-push-synced-before"

// The keys of the delivered Secret beside the order. clouds.yaml is
// appCredCloudsYAMLKey.
const (
	applicationCredentialIDKey     = "application_credential_id"
	applicationCredentialSecretKey = "application_credential_secret" //nolint:gosec // G101 false positive: Secret data key, not a credential.
)

// deliverCredential backs the live credential up to OpenBao and writes the
// Secret <name>-credentials beside the order, on the order's own cluster, and
// writes DeliveryReady. It delivers the generation status.credentialGeneration
// names, so the Secret switches to a successor on the pass status does.
//
// The backup goes first, as for a KeystoneUser: a source Secret and a
// PushSecret in the ControlPlane's namespace push the document through that
// namespace's own store, and the delivered Secret is not written before ESO
// reports a push of the current document.
func (r *KeystoneApplicationCredentialReconciler) deliverCredential(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.KeystoneApplicationCredential,
	cp *c5c3v1alpha1.ControlPlane, cluster string,
) (ctrl.Result, error) {
	fail := keystoneApplicationCredentialFail(order, conditionTypeKeystoneApplicationCredentialDeliveryReady)
	requeue := ctrl.Result{RequeueAfter: korcRequeueAfter}
	orderRef := keystoneApplicationCredentialRef(order, cluster)
	location := orderRef.location()

	// The document is read on the order's cluster, so its auth_url is resolved
	// for that cluster; only a ControlPlane edit can publish an endpoint, so
	// there is no requeue.
	ref := orderRef.clusterRef()
	authURL := korcAuthURL(cp, ref)
	if authURL == "" {
		fail(reasonKeystoneUserKeystoneNotPublished, fmt.Sprintf(
			"the order lives on %s, which cannot reach the in-cluster Keystone Service; set "+
				"spec.services.keystone.publicEndpoint or spec.services.keystone.gateway on ControlPlane %s/%s",
			location, cp.Namespace, cp.Name))
		return ctrl.Result{}, nil
	}

	notMinted := func() (ctrl.Result, error) {
		fail(reasonKeystoneApplicationCredentialWaiting, "the credential is not minted yet; nothing is delivered")
		return ctrl.Result{}, nil
	}
	acID := order.Status.CredentialID
	if acID == "" {
		return notMinted()
	}
	valueKey := types.NamespacedName{
		Namespace: cp.Namespace,
		Name:      keystoneApplicationCredentialSecretName(order, cluster, order.Status.CredentialGeneration),
	}
	valueSecret := &corev1.Secret{}
	if err := r.Get(ctx, valueKey, valueSecret); client.IgnoreNotFound(err) != nil {
		fail(reasonKeystoneUserDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("reading the credential secret %s: %w", valueKey, err)
	}
	acSecret := valueSecret.Data[appCredSecretValueKey]
	if len(acSecret) == 0 {
		return notMinted()
	}

	cloudsYAML := []byte(buildAppCredCloudsYAML(cp, acID, string(acSecret), ref))
	sourceName := keystoneApplicationCredentialSourceSecretName(order, cluster)
	if err := ensureOrderSecret(ctx, r.Client, orderRef, sourceName, cp.Namespace, func(secret *corev1.Secret) error {
		secret.Data[appCredCloudsYAMLKey] = cloudsYAML
		secret.Data[applicationCredentialIDKey] = []byte(acID)
		secret.Data[applicationCredentialSecretKey] = acSecret
		secret.Data["auth_url"] = []byte(authURL)
		secret.Data["region_name"] = []byte(korcRegion(cp))
		return nil
	}); err != nil {
		fail(reasonKeystoneUserDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("assembling order source Secret %s/%s: %w", cp.Namespace, sourceName, err)
	}

	pushName := keystoneApplicationCredentialPushSecretName(order, cluster)
	push := accountPushSecretSpec(pushName, cp.Namespace, sourceName,
		keystoneApplicationCredentialRemoteKeyFor(cp, orderRef.childPrefix()), effectiveControlPlaneStoreRef(cp))
	if err := ensureOrderChild(ctx, r.Client, r.Scheme, order, orderRef, push); err != nil {
		fail(reasonKeystoneUserDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("ensuring order PushSecret %s/%s: %w", cp.Namespace, pushName, err)
	}
	sum := sha256.Sum256(cloudsYAML)
	hash := hex.EncodeToString(sum[:])
	pushKey := types.NamespacedName{Namespace: cp.Namespace, Name: pushName}
	if err := stampOrderPush(ctx, r.Client, pushKey, keystoneApplicationCredentialPushContentHashAnnotation,
		keystoneApplicationCredentialPushSyncedBeforeAnnotation, hash); err != nil {
		fail(reasonKeystoneUserDeliveryError, err.Error())
		return ctrl.Result{}, err
	}
	pushed := &esov1alpha1.PushSecret{}
	if err := r.Get(ctx, pushKey, pushed); err != nil {
		fail(reasonKeystoneUserDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("reading order PushSecret %s/%s: %w", cp.Namespace, pushName, err)
	}
	if !orderPushFresh(pushed, keystoneApplicationCredentialPushContentHashAnnotation,
		keystoneApplicationCredentialPushSyncedBeforeAnnotation, hash) {
		fail(reasonKeystoneUserBackupNotSynced, fmt.Sprintf(
			"the credential is not backed up to OpenBao yet (PushSecret %s/%s)", cp.Namespace, pushName))
		return requeue, nil
	}

	secretKey := types.NamespacedName{Namespace: order.Namespace, Name: keystoneApplicationCredentialCredentialsSecretName(order)}
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
	labels := orderRef.childLabels()
	delete(labels, keystoneApplicationCredentialLabelKeys.Cluster)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretKey.Name, Namespace: secretKey.Namespace, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			appCredCloudsYAMLKey:           cloudsYAML,
			applicationCredentialIDKey:     []byte(acID),
			applicationCredentialSecretKey: acSecret,
		},
	}
	if err := controllerutil.SetControllerReference(order, secret, r.Scheme); err != nil {
		fail(reasonKeystoneUserDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("delivering Secret %s: %w", secretKey, err)
	}
	// ForceOwnership is the repair: the field manager owns the three keys it
	// applies, so an edited key is rewritten, and a key somebody else adds stays.
	if err := apply.EnsureUnownedObject(ctx, oc, r.Scheme, secret, apply.FieldManager); err != nil {
		fail(reasonKeystoneUserDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("delivering Secret %s: %w", secretKey, err)
	}

	// EnsureUnownedObject decoded the apply response into secret.
	if string(secret.Data[applicationCredentialIDKey]) != acID {
		fail(reasonKeystoneApplicationCredentialWaiting, "the delivered Secret does not carry the current credential yet")
		return requeue, nil
	}

	order.Status.SecretName = secretKey.Name
	order.Status.SecretKeys = []string{appCredCloudsYAMLKey, applicationCredentialIDKey, applicationCredentialSecretKey}
	keystoneApplicationCredentialSetTrue(order, conditionTypeKeystoneApplicationCredentialDeliveryReady,
		reasonKeystoneUserDelivered, fmt.Sprintf(
			"credentials are delivered in Secret %q (keys clouds.yaml, application_credential_id, "+
				"application_credential_secret) in namespace %q on %s", secretKey.Name, secretKey.Namespace, location))
	return ctrl.Result{}, nil
}
