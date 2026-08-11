// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller — reconcileDBConnectionSecret materialises the database
// connection URL into a derived Kubernetes Secret named
// <placement.Name>-db-connection.
//
// The shared database.ReconcileConnectionSecret reads the upstream credentials
// Secret (synced by ESO) and writes the fully-formed pymysql URL into a derived
// Secret. The Placement container later consumes the URL via the
// OS_PLACEMENT_DATABASE__CONNECTION env var (oslo.config OS_<GROUP>__<OPTION>
// override, and placement keeps its database options in [placement_database]),
// keeping the password out of the ConfigMap entirely. The derived Secret is a
// plain corev1.Secret — no PushSecret or ExternalSecret is created.

package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/database"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
	placementv1alpha1 "github.com/c5c3/forge/operators/placement/api/v1alpha1"
)

// dbTLSMountPath is the in-pod directory where the db-tls Secret (the client
// TLS keypair) is projected; the ssl_ca/ssl_cert/ssl_key DSN parameters
// reference files inside this directory so the keypair bytes never enter the
// operator process. The file names inside it come from database.TLSFilePaths, so
// the DSN paths and the workload mount layout stay in lockstep.
//
// It sits outside /etc/placement rather than under it, unlike the keystone and
// glance db-tls directories: for those services /etc/<service> is a plain image
// directory that takes sibling volume mounts, while placement's /etc/placement
// is itself the read-only ConfigMap mount (see placementConfigDir). A mount
// nested inside it could not be created, because the runtime cannot make the
// mountpoint directory in a read-only tmpfs.
const dbTLSMountPath = "/etc/placement-db-tls/"

// reconcileDBConnectionSecret derives the database connection URL from the
// upstream credentials Secret and writes it to <placement.Name>-db-connection,
// delegating to the shared database.ReconcileConnectionSecret. When the upstream
// Secret or its required keys are missing it sets SecretsReady=False with reason
// WaitingForDBCredentials and requeues; it never writes a derived Secret with
// empty credentials.
//
// It returns the SHA-256 digest of the assembled DSN so the deployment step can
// roll the Pods when a Dynamic (engine-issued) credential rotates without
// reading the Secret content itself; the digest is empty on the requeue/error
// paths where no derived Secret was materialised.
func (r *PlacementReconciler) reconcileDBConnectionSecret(ctx context.Context, children client.Client, placement *placementv1alpha1.Placement) (ctrl.Result, string, error) {
	return database.ReconcileConnectionSecret(ctx, database.ConnectionSecretFlowParams{
		Client:        children,
		Scheme:        r.Scheme,
		Owner:         placement,
		InstanceName:  placement.Name,
		Namespace:     placement.Namespace,
		Database:      &placement.Spec.Database,
		TLSMountPath:  dbTLSMountPath,
		Conditions:    &placement.Status.Conditions,
		Generation:    placement.Generation,
		ConditionType: "SecretsReady",
		RequeueAfter:  commonreconcile.RequeueSecretPolling,
	})
}
