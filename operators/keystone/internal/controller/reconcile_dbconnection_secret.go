// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller — reconcileDBConnectionSecret materialises the database
// connection URL into a derived Kubernetes Secret named
// <keystone.Name>-db-connection (CC-0080, REQ-002, REQ-010).
//
// The previous design embedded the password into the keystone.conf ConfigMap.
// ConfigMaps lack encryption-at-rest and have weaker RBAC than Secrets, which
// caused credentials to be exposed at rest. This reconciler reads the upstream
// credentials Secret (synced by ESO) and writes the fully-formed pymysql URL
// into a derived Secret. The Keystone container later consumes the URL via the
// OS_DATABASE__CONNECTION env var (oslo.config OS_<GROUP>__<OPTION> override),
// keeping the password out of the ConfigMap entirely. Per REQ-010 the derived
// Secret is a plain corev1.Secret — no PushSecret or ExternalSecret is created.

package controller

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/secrets"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// dbConnectionSecretKey is the only Data key in the derived
// <keystone.Name>-db-connection Secret. It mirrors the env override key used at
// runtime (OS_DATABASE__CONNECTION) to keep the wiring obvious for the deployment
// reconciler that consumes it.
const dbConnectionSecretKey = "connection"

// Feature: CC-0106
//
// dbTLSMountPath is the in-pod directory where the db-tls Secret (the client
// TLS keypair "<keystone.Name>-db-client") is projected; the ssl_ca/ssl_cert/
// ssl_key DSN parameters reference files inside this directory so the keypair
// bytes never enter the operator process (CC-0106, REQ-004, REQ-014). The
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
// produced by the CC-0106 task 3.1 Certificate.
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
// (CC-0106, REQ-004, REQ-005).
func dbTLSPathsForMount() dbTLSPaths {
	return dbTLSPaths{
		CA:   dbTLSMountPath + dbTLSCAFileName,
		Cert: dbTLSMountPath + dbTLSCertFileName,
		Key:  dbTLSMountPath + dbTLSKeyFileName,
	}
}

// appendDBTLSParams merges the pymysql ssl_* DSN parameters into query when
// spec.database.tls is enabled on the Keystone CR (CC-0106, REQ-003, REQ-004).
// It is a no-op when TLS is nil or .enabled is false, preserving the pre-
// CC-0106 plaintext DSN. The mode is validated by modeToSSLParams; an unknown
// mode (which the webhook + CRD enum reject earlier) is surfaced as an error
// rather than silently producing a partial DSN.
func appendDBTLSParams(keystone *keystonev1alpha1.Keystone, query url.Values) error {
	tls := keystone.Spec.Database.TLS
	if tls == nil || !tls.Enabled {
		return nil
	}
	sslParams, err := modeToSSLParams(tls.Mode, dbTLSPathsForMount())
	if err != nil {
		return err
	}
	for key, values := range sslParams {
		query[key] = values
	}
	return nil
}

// reconcileDBConnectionSecret derives the database connection URL from the
// upstream credentials Secret and writes it to <keystone.Name>-db-connection
// (CC-0080, REQ-002). When the upstream Secret or its required keys are
// missing it sets SecretsReady=False with reason WaitingForDBCredentials and
// requeues; it never writes a derived Secret with empty credentials.
func (r *KeystoneReconciler) reconcileDBConnectionSecret(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	upstreamKey := client.ObjectKey{
		Namespace: keystone.Namespace,
		Name:      keystone.Spec.Database.SecretRef.Name,
	}

	// In managed mode the MariaDB User CR name (= keystone.Name) is the MySQL
	// username, so we skip the Secret read for it. Brownfield mode reads
	// "username" from the upstream Secret.
	var username string
	if keystone.Spec.Database.ClusterRef != nil {
		username = keystone.Name
	} else {
		u, err := secrets.GetSecretValue(ctx, r.Client, upstreamKey, "username")
		if err != nil {
			if secrets.IsMissingSecretOrKey(err) {
				conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
					Type:               "SecretsReady",
					Status:             metav1.ConditionFalse,
					ObservedGeneration: keystone.Generation,
					Reason:             "WaitingForDBCredentials",
					Message: fmt.Sprintf("Upstream database credentials Secret %s/%s missing or missing key %q",
						upstreamKey.Namespace, upstreamKey.Name, "username"),
				})
				return ctrl.Result{RequeueAfter: RequeueSecretPolling}, nil
			}
			return ctrl.Result{}, fmt.Errorf("reading database username: %w", err)
		}
		username = u
	}

	password, err := secrets.GetSecretValue(ctx, r.Client, upstreamKey, "password")
	if err != nil {
		if secrets.IsMissingSecretOrKey(err) {
			conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
				Type:               "SecretsReady",
				Status:             metav1.ConditionFalse,
				ObservedGeneration: keystone.Generation,
				Reason:             "WaitingForDBCredentials",
				Message: fmt.Sprintf("Upstream database credentials Secret %s/%s missing or missing key %q",
					upstreamKey.Namespace, upstreamKey.Name, "password"),
			})
			return ctrl.Result{RequeueAfter: RequeueSecretPolling}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading database password: %w", err)
	}

	// url.UserPassword percent-encodes reserved characters in the userinfo
	// component per RFC 3986, matching the encoding pymysql expects.
	//
	// Build the query parameters via url.Values so the optional ssl_* keys
	// (CC-0106, REQ-003, REQ-004) compose cleanly with the always-present
	// charset=utf8. url.Values.Encode() sorts keys lexically, yielding a
	// deterministic, stable DSN across reconciles.
	query := url.Values{}
	query.Set("charset", "utf8")
	if err := appendDBTLSParams(keystone, query); err != nil {
		return ctrl.Result{}, fmt.Errorf("assembling database TLS DSN parameters: %w", err)
	}
	connURL := &url.URL{
		Scheme: "mysql+pymysql",
		User:   url.UserPassword(username, password),
		Host:   resolveDatabaseHost(keystone),
		Path:   keystone.Spec.Database.Database,
		// url.Values.Encode percent-encodes "/" to "%2F" in the ssl_ca/ssl_cert/
		// ssl_key file paths. keystone-manage db_sync hands the DSN to alembic's
		// ConfigParser, which interprets "%" as interpolation syntax and aborts
		// with "invalid interpolation syntax". RFC 3986 allows literal "/" in the
		// query component, and our values contain neither "&" nor "=", so keeping
		// "/" literal is safe and round-trips cleanly through urllib.parse_qs in
		// scripts/bootstrap_db_seed.py.
		RawQuery: strings.ReplaceAll(query.Encode(), "%2F", "/"),
	}
	connStr := connURL.String()

	derivedKey := client.ObjectKey{
		Namespace: keystone.Namespace,
		Name:      fmt.Sprintf("%s-db-connection", keystone.Name),
	}

	existing := &corev1.Secret{}
	err = r.Get(ctx, derivedKey, existing)
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
			return ctrl.Result{}, fmt.Errorf("setting owner reference on derived Secret %s/%s: %w",
				derived.Namespace, derived.Name, err)
		}
		if err := r.Create(ctx, derived); err != nil {
			return ctrl.Result{}, fmt.Errorf("creating derived Secret %s/%s: %w",
				derived.Namespace, derived.Name, err)
		}
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting derived Secret %s/%s: %w",
			derivedKey.Namespace, derivedKey.Name, err)
	}

	// Per REQ-002 scenario 2 the derived Secret must contain exactly the one
	// "connection" key; replace Data wholesale on any drift (value change OR
	// extra keys present).
	current, ok := existing.Data[dbConnectionSecretKey]
	if len(existing.Data) != 1 || !ok || string(current) != connStr {
		existing.Data = map[string][]byte{
			dbConnectionSecretKey: []byte(connStr),
		}
		if err := r.Update(ctx, existing); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating derived Secret %s/%s: %w",
				existing.Namespace, existing.Name, err)
		}
	}

	return ctrl.Result{}, nil
}
