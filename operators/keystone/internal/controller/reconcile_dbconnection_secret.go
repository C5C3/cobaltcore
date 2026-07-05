// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller — reconcileDBConnectionSecret materialises the database
// connection URL into a derived Kubernetes Secret named
// <keystone.Name>-db-connection.
//
// The previous design embedded the password into the keystone.conf ConfigMap.
// ConfigMaps lack encryption-at-rest and have weaker RBAC than Secrets, which
// caused credentials to be exposed at rest. This reconciler reads the upstream
// credentials Secret (synced by ESO) and writes the fully-formed pymysql URL
// into a derived Secret. The Keystone container later consumes the URL via the
// OS_DATABASE__CONNECTION env var (oslo.config OS_<GROUP>__<OPTION> override),
// keeping the password out of the ConfigMap entirely. The derived
// Secret is a plain corev1.Secret — no PushSecret or ExternalSecret is created.

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

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/database"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// dbConnectionSecretKey is the only Data key in the derived
// <keystone.Name>-db-connection Secret. It mirrors the env override key used at
// runtime (OS_DATABASE__CONNECTION) to keep the wiring obvious for the deployment
// reconciler that consumes it.
const dbConnectionSecretKey = "connection"

// dbTLSMountPath is the in-pod directory where the db-tls Secret (the client
// TLS keypair "<keystone.Name>-db-client") is projected; the ssl_ca/ssl_cert/
// ssl_key DSN parameters reference files inside this directory so the keypair
// bytes never enter the operator process. The
// matching VolumeMount is built by dbTLSVolumeAndMount in
// reconcile_databasetls.go and consumes this constant via dbTLSPathsForMount /
// the value directly, so the DSN consumer (this file) is the single source of
// truth for the path layout.
//
// dbTLSCAFileName / dbTLSCertFileName / dbTLSKeyFileName are the file names
// cert-manager writes into the issued client Secret with default keyEncoding.
//
// DECISION: file names — Chose ca.crt / tls.crt / tls.key because they are the
// default keys cert-manager writes into the issued client Secret. If a future
// task overrides cert-manager's secretTemplate keys, update these constants in
// lockstep. Reviewer: please verify the chosen file names match the keys
// produced by the task 3.1 Certificate.
const (
	dbTLSMountPath    = "/etc/keystone/db-tls/"
	dbTLSCAFileName   = "ca.crt"
	dbTLSCertFileName = "tls.crt"
	dbTLSKeyFileName  = "tls.key"
)

// dbTLSPathsForMount returns the in-pod file paths consumed by the
// ssl_ca/ssl_cert/ssl_key DSN parameters when the db-tls Secret is mounted at
// dbTLSMountPath (declared in reconcile_databasetls.go). Centralising the path
// layout here keeps the DSN assembly and the workload mount points in lockstep
func dbTLSPathsForMount() dbTLSPaths {
	return dbTLSPaths{
		CA:   dbTLSMountPath + dbTLSCAFileName,
		Cert: dbTLSMountPath + dbTLSCertFileName,
		Key:  dbTLSMountPath + dbTLSKeyFileName,
	}
}

// appendDBTLSParams merges the pymysql ssl_* DSN parameters into query when
// spec.database.tls is enabled on the Keystone CR.
// It is a no-op when TLS is nil, its mode is empty, or its mode is "disabled"
// (DatabaseTLSSpec.IsEnabled), preserving the plaintext DSN. The mode is
// validated by modeToSSLParams; an unknown
// mode (which the webhook + CRD enum reject earlier) is surfaced as an error
// rather than silently producing a partial DSN.
func appendDBTLSParams(keystone *keystonev1alpha1.Keystone, query url.Values) error {
	return database.AppendTLSParams(keystone.Spec.Database.TLS, dbTLSPathsForMount(), query)
}

// dbConnectionDigest returns the SHA-256 of the assembled DSN as a lowercase
// hex string. reconcileDeployment stamps it into the pod-template
// keystone.c5c3.io/db-connection-hash annotation in Dynamic credentials mode so
// a rotated engine-issued credential rolls the Deployment (the DSN is consumed
// via the OS_DATABASE__CONNECTION env var, which — unlike the hot-reloaded
// fernet/credential key volumes — only takes effect on a Pod restart).
func dbConnectionDigest(connStr string) string {
	return database.Digest(connStr)
}

// reconcileDBConnectionSecret derives the database connection URL from the
// upstream credentials Secret and writes it to <keystone.Name>-db-connection
// When the upstream Secret or its required keys are
// missing it sets SecretsReady=False with reason WaitingForDBCredentials and
// requeues; it never writes a derived Secret with empty credentials.
//
// It returns the SHA-256 digest of the assembled DSN so the deployment step can
// roll the Pods when a Dynamic (engine-issued) credential rotates without
// reading the Secret content itself; the digest is empty on the requeue/error
// paths where no derived Secret was materialised.
func (r *KeystoneReconciler) reconcileDBConnectionSecret(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, string, error) {
	upstreamKey := client.ObjectKey{
		Namespace: keystone.Namespace,
		Name:      keystone.Spec.Database.SecretRef.Name,
	}

	// waitForCredentials records SecretsReady=False / WaitingForDBCredentials and
	// returns the polling requeue with an empty digest, so a derived Secret is
	// never materialised with partial credentials.
	waitForCredentials := func(msg string) (ctrl.Result, string, error) {
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "SecretsReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: keystone.Generation,
			Reason:             "WaitingForDBCredentials",
			Message:            msg,
		})
		return ctrl.Result{RequeueAfter: RequeueSecretPolling}, "", nil
	}

	// Read the upstream credentials Secret exactly once so the username and
	// password are taken from a single, consistent object version. Reading each
	// half with a separate cache Get opens a window in which ESO's atomic Secret
	// update can land between the two reads, splicing one dynamic MySQL user's
	// username onto another user's password and yielding a DSN that can never
	// authenticate.
	secret := &corev1.Secret{}
	if err := r.Get(ctx, upstreamKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return waitForCredentials(fmt.Sprintf("Upstream database credentials Secret %s/%s not found",
				upstreamKey.Namespace, upstreamKey.Name))
		}
		return ctrl.Result{}, "", fmt.Errorf("reading database credentials Secret %s/%s: %w",
			upstreamKey.Namespace, upstreamKey.Name, err)
	}

	// In Static managed mode the MariaDB User CR name (= keystone.Name) is the
	// MySQL username, so it is not read from the Secret. Brownfield mode and
	// Dynamic managed mode both take "username" from the upstream Secret — the
	// dynamic engine issues an ephemeral username (e.g. v-kube-...) alongside
	// the password, so the username is not derivable from the CR name.
	username, ok := database.ResolveUsername(&keystone.Spec.Database, keystone.Name, secret.Data)
	if !ok {
		return waitForCredentials(fmt.Sprintf("Upstream database credentials Secret %s/%s missing key %q",
			upstreamKey.Namespace, upstreamKey.Name, "username"))
	}

	p, ok := secret.Data["password"]
	if !ok {
		return waitForCredentials(fmt.Sprintf("Upstream database credentials Secret %s/%s missing key %q",
			upstreamKey.Namespace, upstreamKey.Name, "password"))
	}
	password := string(p)

	// url.UserPassword percent-encodes reserved characters in the userinfo
	// component per RFC 3986, matching the encoding pymysql expects.
	//
	// Build the query parameters via url.Values so the optional ssl_* keys
	// compose cleanly with the always-present
	// charset=utf8. url.Values.Encode() sorts keys lexically, yielding a
	// deterministic, stable DSN across reconciles.
	query := url.Values{}
	query.Set("charset", "utf8")
	if err := appendDBTLSParams(keystone, query); err != nil {
		return ctrl.Result{}, "", fmt.Errorf("assembling database TLS DSN parameters: %w", err)
	}
	// database.BuildDSN assembles the pymysql URL (RFC 3986 userinfo encoding,
	// literal "/" preserved in the ssl_* query values so alembic's ConfigParser
	// never sees "%" interpolation syntax) and returns its rollout digest.
	connStr, digest := database.BuildDSN(username, password,
		resolveDatabaseHost(keystone), keystone.Spec.Database.Database, query)

	derivedKey := client.ObjectKey{
		Namespace: keystone.Namespace,
		Name:      fmt.Sprintf("%s-db-connection", keystone.Name),
	}

	existing := &corev1.Secret{}
	err := r.Get(ctx, derivedKey, existing)
	if apierrors.IsNotFound(err) {
		derived := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      derivedKey.Name,
				Namespace: derivedKey.Namespace,
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				dbConnectionSecretKey: []byte(connStr),
			},
		}
		if err := controllerutil.SetControllerReference(keystone, derived, r.Scheme); err != nil {
			return ctrl.Result{}, "", fmt.Errorf("setting owner reference on derived Secret %s/%s: %w",
				derived.Namespace, derived.Name, err)
		}
		if err := r.Create(ctx, derived); err != nil {
			return ctrl.Result{}, "", fmt.Errorf("creating derived Secret %s/%s: %w",
				derived.Namespace, derived.Name, err)
		}
		return ctrl.Result{}, digest, nil
	}
	if err != nil {
		return ctrl.Result{}, "", fmt.Errorf("getting derived Secret %s/%s: %w",
			derivedKey.Namespace, derivedKey.Name, err)
	}

	// Per scenario 2 the derived Secret must contain exactly the one
	// "connection" key; replace Data wholesale on any drift (value change OR
	// extra keys present).
	current, ok := existing.Data[dbConnectionSecretKey]
	if len(existing.Data) != 1 || !ok || string(current) != connStr {
		existing.Data = map[string][]byte{
			dbConnectionSecretKey: []byte(connStr),
		}
		if err := r.Update(ctx, existing); err != nil {
			return ctrl.Result{}, "", fmt.Errorf("updating derived Secret %s/%s: %w",
				existing.Namespace, existing.Name, err)
		}
	}

	return ctrl.Result{}, digest, nil
}
