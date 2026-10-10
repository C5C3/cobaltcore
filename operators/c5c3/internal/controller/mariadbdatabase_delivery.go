// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/apply"
	"github.com/c5c3/cobaltcore/internal/common/database"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// deliverCredentials writes the Secret <name>-credentials beside the order, on
// the order's own cluster, with the address of the shared database, the schema
// name and the credential ESO materialised in the ControlPlane's namespace, and
// writes DeliveryReady. There is no backup: the store of record is OpenBao's
// lease.
//
// Each ESO refresh rewrites the materialised Secret, whose labels bring the
// order back, and this leg follows it. The apply is the repair as well: an
// edited key is rewritten, a deleted Secret comes back on the Owns event, and a
// key somebody else adds stays.
func (r *MariaDBDatabaseReconciler) deliverCredentials(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.MariaDBDatabase, cp *c5c3v1alpha1.ControlPlane,
	cluster string,
) (ctrl.Result, error) {
	fail := mariaDBDatabaseFail(order, conditionTypeMariaDBDatabaseDeliveryReady)
	requeue := ctrl.Result{RequeueAfter: dbCredentialsRequeueAfter}
	location := orderRef{Cluster: cluster}.location()
	db := effectiveKeystoneDatabase(cp)

	// Off the database's cluster the address is the published endpoint, and only
	// a ControlPlane edit can publish one, so there is no requeue.
	endpoint := mariaDBDatabaseEndpoint(cp, db, cluster)
	if endpoint == "" {
		fail(reasonMariaDBDatabaseNotPublished, fmt.Sprintf(
			"the order lives on %s, which cannot reach the in-cluster MariaDB Service %s; set "+
				"spec.infrastructure.publishedDatabaseEndpoint on ControlPlane %s/%s",
			location, database.ResolveHost(db, cp.KeystoneNamespace()), cp.Namespace, cp.Name))
		return ctrl.Result{}, nil
	}
	host, port, err := splitDatabaseEndpoint(endpoint)
	if err != nil {
		fail(reasonMariaDBDatabaseDeliveryError, fmt.Sprintf(
			"spec.infrastructure.publishedDatabaseEndpoint %q is not host:port: %v", endpoint, err))
		return ctrl.Result{}, nil
	}

	var caBundle []byte
	if db.TLS.IsEnabled() {
		dbClient, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client, cp.KeystoneTargetClusterRef())
		if err != nil {
			fail(commonmulticluster.TargetClusterUnavailable, err.Error())
			return requeue, nil
		}
		caKey := types.NamespacedName{Namespace: cp.KeystoneNamespace(), Name: db.TLS.CABundleSecretRef.Name}
		caSecret := &corev1.Secret{}
		if err := dbClient.Get(ctx, caKey, caSecret); client.IgnoreNotFound(err) != nil {
			fail(reasonMariaDBDatabaseDeliveryError, err.Error())
			return ctrl.Result{}, fmt.Errorf("reading database CA bundle %s: %w", caKey, err)
		}
		caBundle = caSecret.Data[database.TLSCAFileName]
		if len(caBundle) == 0 {
			fail(reasonMariaDBDatabaseWaitingForCABundle, fmt.Sprintf(
				"the database CA bundle Secret %s/%s does not carry %s yet", caKey.Namespace, caKey.Name, database.TLSCAFileName))
			return requeue, nil
		}
	}

	credKey := types.NamespacedName{Namespace: cp.Namespace, Name: mariaDBDatabaseGeneratorName(order, cluster)}
	materialised := &corev1.Secret{}
	if err := r.Get(ctx, credKey, materialised); client.IgnoreNotFound(err) != nil {
		fail(reasonMariaDBDatabaseDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("reading the materialised credential %s: %w", credKey, err)
	}
	username := materialised.Data[mariaDBDatabaseUsernameKey]
	password := materialised.Data[mariaDBDatabasePasswordKey]
	if len(username) == 0 || len(password) == 0 {
		fail(reasonMariaDBDatabaseWaitingForCredentials, fmt.Sprintf(
			"the materialised credential %s does not carry a username and a password yet", credKey))
		return requeue, nil
	}
	es := &esov1.ExternalSecret{}
	if err := r.Get(ctx, credKey, es); client.IgnoreNotFound(err) != nil {
		fail(reasonMariaDBDatabaseDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("reading order ExternalSecret %s: %w", credKey, err)
	}

	secretKey := types.NamespacedName{Namespace: order.Namespace, Name: mariaDBDatabaseCredentialsSecretName(order)}
	// Ownership is decided from live state: the order's cluster may be a target
	// cluster, whose cache can trail it.
	live := &corev1.Secret{}
	liveExists := false
	switch err := commonmulticluster.LiveReader(oc).Get(ctx, secretKey, live); {
	case apierrors.IsNotFound(err):
	case err != nil:
		fail(reasonMariaDBDatabaseDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("checking for a pre-existing Secret %s: %w", secretKey, err)
	case !metav1.IsControlledBy(live, order):
		fail(reasonMariaDBDatabaseDeliveryRefused, fmt.Sprintf(
			"Secret %s exists and is not owned by this order; delete it or rename the order", secretKey))
		return ctrl.Result{}, nil
	default:
		liveExists = true
	}

	keys := []string{
		mariaDBDatabaseHostKey, mariaDBDatabasePortKey, mariaDBDatabaseDatabaseKey,
		mariaDBDatabaseUsernameKey, mariaDBDatabasePasswordKey,
	}
	data := map[string][]byte{
		mariaDBDatabaseHostKey:     []byte(host),
		mariaDBDatabasePortKey:     []byte(strconv.Itoa(int(port))),
		mariaDBDatabaseDatabaseKey: []byte(order.Status.DatabaseName),
		mariaDBDatabaseUsernameKey: username,
		mariaDBDatabasePasswordKey: password,
	}
	if caBundle != nil {
		data[mariaDBDatabaseCAKey] = caBundle
		keys = append(keys, mariaDBDatabaseCAKey)
	}
	// The Secret is the order's through its controller reference, and carries no
	// cluster label for the reason the KeystoneUser delivery gives.
	labels := mariaDBDatabaseRef(order, cluster).childLabels()
	delete(labels, mariaDBDatabaseClusterLabel)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretKey.Name, Namespace: secretKey.Namespace, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
	if err := controllerutil.SetControllerReference(order, secret, r.Scheme); err != nil {
		fail(reasonMariaDBDatabaseDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("delivering Secret %s: %w", secretKey, err)
	}
	// ForceOwnership is the repair: the field manager owns the keys it applies,
	// so an edited key is rewritten, a ca.crt this pass omits is dropped, and a
	// key somebody else adds stays.
	if err := apply.EnsureUnownedObject(ctx, oc, r.Scheme, secret, apply.FieldManager); err != nil {
		fail(reasonMariaDBDatabaseDeliveryError, err.Error())
		return ctrl.Result{}, fmt.Errorf("delivering Secret %s: %w", secretKey, err)
	}

	// EnsureUnownedObject decoded the apply response into secret.
	if !bytes.Equal(secret.Data[mariaDBDatabaseUsernameKey], username) ||
		!bytes.Equal(secret.Data[mariaDBDatabasePasswordKey], password) {
		fail(reasonMariaDBDatabaseWaitingForCredentials, "the delivered Secret does not carry the current credential yet")
		return requeue, nil
	}
	if liveExists && !bytes.Equal(live.Data[mariaDBDatabaseUsernameKey], username) {
		log.FromContext(ctx).Info("delivered a refreshed database credential", "order", client.ObjectKeyFromObject(order),
			"username", string(username), "refreshTime", es.Status.RefreshTime)
	}

	order.Status.Host = host
	order.Status.Port = port
	order.Status.CredentialsRefreshedAt = nil
	if !es.Status.RefreshTime.IsZero() {
		order.Status.CredentialsRefreshedAt = es.Status.RefreshTime.DeepCopy()
	}
	order.Status.SecretName = secretKey.Name
	order.Status.SecretKeys = keys
	mariaDBDatabaseSetTrue(order, conditionTypeMariaDBDatabaseDeliveryReady, reasonMariaDBDatabaseDelivered, fmt.Sprintf(
		"credentials are delivered in Secret %q (keys %s) in namespace %q on %s",
		secretKey.Name, strings.Join(keys, ", "), secretKey.Namespace, location))
	return ctrl.Result{}, nil
}

// splitDatabaseEndpoint splits a host:port endpoint, a bracketed IPv6 host
// included, into the host and the port, which has to be a TCP port: 1 to 65535.
func splitDatabaseEndpoint(endpoint string) (string, int32, error) {
	host, portText, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return "", 0, fmt.Errorf("port %q is not in 1-65535", portText)
	}
	return host, int32(port), nil
}
