// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Feature: CC-0080

package controller

import (
	"context"
	"fmt"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/forge/internal/common/secrets"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// reconcileDBConnectionSecret materialises the derived
// <keystone-name>-db-connection Kubernetes Secret. The Secret carries a single
// key, "connection", whose value is the fully-qualified PyMySQL DSN built from
// the upstream DB credentials Secret plus the Database spec. The Secret is
// owned by the Keystone CR and is managed with a Get-then-Create-or-Update
// idiom so that rotations on the upstream Secret propagate deterministically.
//
// When the upstream DB credentials Secret is missing or is missing a required
// key, the function returns a requeue (RequeueSecretPolling) with nil error
// and leaves the derived Secret absent — the caller is responsible for
// surfacing the corresponding Condition. No ExternalSecret or PushSecret is
// ever created from this function; the derived object is a plain corev1.Secret
// (CC-0080, REQ-002, REQ-010).
func (r *KeystoneReconciler) reconcileDBConnectionSecret(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	upstreamKey := client.ObjectKey{
		Namespace: keystone.Namespace,
		Name:      keystone.Spec.Database.SecretRef.Name,
	}

	// Resolve username. In managed mode the MariaDB User CR name (= keystone.Name)
	// is the MySQL username; in brownfield the upstream Secret is authoritative.
	var username string
	if keystone.Spec.Database.ClusterRef != nil {
		username = keystone.Name
	} else {
		u, err := secrets.GetSecretValue(ctx, r.Client, upstreamKey, "username")
		if err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{RequeueAfter: RequeueSecretPolling}, nil
			}
			// DECISION: treat any missing-key error from GetSecretValue as a
			// transient wait (REQ-002). The caller sets the Condition; we only
			// requeue. Wrapped errors that are not IsNotFound are propagated
			// above; a missing-key is reported by the common helper as a
			// non-IsNotFound error, so detect it via the NotFound check first
			// and otherwise fall through to requeue with nil.
			return ctrl.Result{RequeueAfter: RequeueSecretPolling}, nil
		}
		username = u
	}

	password, err := secrets.GetSecretValue(ctx, r.Client, upstreamKey, "password")
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: RequeueSecretPolling}, nil
		}
		// Missing key (e.g. "password") produces a non-IsNotFound error from
		// the common helper. Treat as a transient wait per REQ-002.
		return ctrl.Result{RequeueAfter: RequeueSecretPolling}, nil
	}

	// Build the PyMySQL connection string. url.UserPassword percent-encodes
	// reserved userinfo characters per RFC 3986, matching the original
	// construction in reconcile_config.go prior to CC-0080.
	connURL := &url.URL{
		Scheme:   "mysql+pymysql",
		User:     url.UserPassword(username, password),
		Host:     resolveDatabaseHost(keystone),
		Path:     keystone.Spec.Database.Database,
		RawQuery: "charset=utf8",
	}
	connectionStr := connURL.String()

	derivedName := fmt.Sprintf("%s-db-connection", keystone.Name)
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      derivedName,
			Namespace: keystone.Namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"connection": []byte(connectionStr),
		},
	}

	existing := &corev1.Secret{}
	getErr := r.Get(ctx, client.ObjectKey{Namespace: keystone.Namespace, Name: derivedName}, existing)
	if apierrors.IsNotFound(getErr) {
		if err := controllerutil.SetControllerReference(keystone, desired, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("setting owner reference on db-connection Secret: %w", err)
		}
		if err := r.Create(ctx, desired); err != nil {
			return ctrl.Result{}, fmt.Errorf("creating db-connection Secret: %w", err)
		}
		return ctrl.Result{}, nil
	}
	if getErr != nil {
		return ctrl.Result{}, fmt.Errorf("getting db-connection Secret: %w", getErr)
	}

	// Update path: ensure Data is exactly {"connection": <url>}. Preserve
	// Name/UID/OwnerReferences by updating the existing object in place.
	needsUpdate := string(existing.Data["connection"]) != connectionStr || len(existing.Data) != 1
	if needsUpdate {
		existing.Type = corev1.SecretTypeOpaque
		existing.Data = map[string][]byte{
			"connection": []byte(connectionStr),
		}
		if err := r.Update(ctx, existing); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating db-connection Secret: %w", err)
		}
	}

	return ctrl.Result{}, nil
}
