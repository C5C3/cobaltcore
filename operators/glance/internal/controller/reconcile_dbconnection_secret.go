// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller — reconcileDBConnectionSecret materialises the database
// connection URL into a derived Kubernetes Secret named
// <glance.Name>-db-connection.
//
// The shared database.ReconcileConnectionSecret reads the upstream credentials
// Secret (synced by ESO) and writes the fully-formed pymysql URL into a derived
// Secret. The Glance container later consumes the URL via the
// OS_DATABASE__CONNECTION env var (oslo.config OS_<GROUP>__<OPTION> override),
// keeping the password out of the ConfigMap entirely. The derived Secret is a
// plain corev1.Secret — no PushSecret or ExternalSecret is created.

package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/c5c3/forge/internal/common/database"
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

// dbTLSMountPath is the in-pod directory where the db-tls Secret (the client
// TLS keypair) is projected; the ssl_ca/ssl_cert/ssl_key DSN parameters
// reference files inside this directory so the keypair bytes never enter the
// operator process. The file names inside it come from database.TLSFilePaths, so
// the DSN paths and the workload mount layout (built by the deployment step in
// the next commit) stay in lockstep.
const dbTLSMountPath = "/etc/glance/db-tls/"

// reconcileDBConnectionSecret derives the database connection URL from the
// upstream credentials Secret and writes it to <glance.Name>-db-connection,
// delegating to the shared database.ReconcileConnectionSecret. When the upstream
// Secret or its required keys are missing it sets SecretsReady=False with reason
// WaitingForDBCredentials and requeues; it never writes a derived Secret with
// empty credentials.
//
// It returns the SHA-256 digest of the assembled DSN so the deployment step can
// roll the Pods when a Dynamic (engine-issued) credential rotates without
// reading the Secret content itself; the digest is empty on the requeue/error
// paths where no derived Secret was materialised.
func (r *GlanceReconciler) reconcileDBConnectionSecret(ctx context.Context, glance *glancev1alpha1.Glance) (ctrl.Result, string, error) {
	return database.ReconcileConnectionSecret(ctx, database.ConnectionSecretFlowParams{
		Client:        r.Client,
		Scheme:        r.Scheme,
		Owner:         glance,
		InstanceName:  glance.Name,
		Namespace:     glance.Namespace,
		Database:      &glance.Spec.Database,
		TLSMountPath:  dbTLSMountPath,
		Conditions:    &glance.Status.Conditions,
		Generation:    glance.Generation,
		ConditionType: "SecretsReady",
		RequeueAfter:  RequeueSecretPolling,
	})
}
