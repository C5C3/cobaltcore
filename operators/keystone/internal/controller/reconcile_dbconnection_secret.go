// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller — reconcileDBConnectionSecret materialises the database
// connection URL into a derived Kubernetes Secret named
// <keystone.Name>-db-connection.
//
// The previous design embedded the password into the keystone.conf ConfigMap.
// ConfigMaps lack encryption-at-rest and have weaker RBAC than Secrets, which
// caused credentials to be exposed at rest. The shared
// database.ReconcileConnectionSecret reads the upstream credentials Secret
// (synced by ESO) and writes the fully-formed pymysql URL into a derived Secret.
// The Keystone container later consumes the URL via the OS_DATABASE__CONNECTION
// env var (oslo.config OS_<GROUP>__<OPTION> override), keeping the password out
// of the ConfigMap entirely. The derived Secret is a plain corev1.Secret — no
// PushSecret or ExternalSecret is created.

package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/c5c3/forge/internal/common/database"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// dbTLSMountPath is the in-pod directory where the db-tls Secret (the client
// TLS keypair "<keystone.Name>-db-client") is projected; the ssl_ca/ssl_cert/
// ssl_key DSN parameters reference files inside this directory so the keypair
// bytes never enter the operator process. The matching VolumeMount is built by
// dbTLSVolumeAndMount in reconcile_databasetls.go, and the file names inside it
// come from database.TLSCAFileName / TLSCertFileName / TLSKeyFileName (see
// database.TLSFilePaths), so the DSN paths and the workload mount layout stay in
// lockstep.
const dbTLSMountPath = "/etc/keystone/db-tls/"

// reconcileDBConnectionSecret derives the database connection URL from the
// upstream credentials Secret and writes it to <keystone.Name>-db-connection,
// delegating to the shared database.ReconcileConnectionSecret. When the upstream
// Secret or its required keys are missing it sets SecretsReady=False with reason
// WaitingForDBCredentials and requeues; it never writes a derived Secret with
// empty credentials.
//
// It returns the SHA-256 digest of the assembled DSN so the deployment step can
// roll the Pods when a Dynamic (engine-issued) credential rotates without
// reading the Secret content itself; the digest is empty on the requeue/error
// paths where no derived Secret was materialised.
func (r *KeystoneReconciler) reconcileDBConnectionSecret(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, string, error) {
	return database.ReconcileConnectionSecret(ctx, database.ConnectionSecretFlowParams{
		Client:        r.Client,
		Scheme:        r.Scheme,
		Owner:         keystone,
		InstanceName:  keystone.Name,
		Namespace:     keystone.Namespace,
		Database:      &keystone.Spec.Database,
		TLSMountPath:  dbTLSMountPath,
		Conditions:    &keystone.Status.Conditions,
		Generation:    keystone.Generation,
		ConditionType: "SecretsReady",
		RequeueAfter:  RequeueSecretPolling,
	})
}
