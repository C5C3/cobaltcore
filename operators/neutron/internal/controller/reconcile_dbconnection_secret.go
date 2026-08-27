// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller — reconcileDBConnectionSecret materialises the database
// connection URL into a derived Kubernetes Secret named
// <neutron.Name>-db-connection.
//
// The shared database.ReconcileConnectionSecret reads the upstream credentials
// Secret (synced by ESO) and writes the fully-formed pymysql URL into a derived
// Secret. The Neutron containers later consume the URL via the
// OS_DATABASE__CONNECTION env var (oslo.config OS_<GROUP>__<OPTION> override;
// neutron keeps its database options in the plain [database] section), keeping
// the password out of the rendered config entirely. The derived Secret is a
// plain corev1.Secret — no PushSecret or ExternalSecret is created.

package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/database"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// dbTLSMountPath is the in-pod directory where the db-tls Secret (the client
// TLS keypair) is projected; the ssl_ca/ssl_cert/ssl_key DSN parameters
// reference files inside this directory so the keypair bytes never enter the
// operator process. The file names inside it come from database.TLSFilePaths, so
// the DSN paths and the workload mount layout stay in lockstep.
//
// It sits outside /etc/neutron rather than under it: neutronConfigMountPath is
// itself the read-only config-ConfigMap mount, and a mount nested inside it
// could not be created, because the runtime cannot make the mountpoint directory
// in a read-only tmpfs.
const dbTLSMountPath = "/etc/neutron-db-tls/"

// reconcileDBConnectionSecret derives the database connection URL from the
// upstream credentials Secret and writes it to <neutron.Name>-db-connection,
// delegating to the shared database.ReconcileConnectionSecret. When the upstream
// Secret or its required keys are missing it sets SecretsReady=False with reason
// WaitingForDBCredentials and requeues; it never writes a derived Secret with
// empty credentials.
//
// It returns the SHA-256 digest of the assembled DSN so the deployment step can
// roll the Pods when a Dynamic (engine-issued) credential rotates without
// reading the Secret content itself; the digest is empty on the requeue/error
// paths where no derived Secret was materialised.
func (r *NeutronReconciler) reconcileDBConnectionSecret(ctx context.Context, children client.Client, neutron *neutronv1alpha1.Neutron) (ctrl.Result, string, error) {
	return database.ReconcileConnectionSecret(ctx, database.ConnectionSecretFlowParams{
		Client:        children,
		Scheme:        r.Scheme,
		Owner:         neutron,
		InstanceName:  neutron.Name,
		Namespace:     neutron.Namespace,
		Database:      &neutron.Spec.Database,
		TLSMountPath:  dbTLSMountPath,
		Conditions:    &neutron.Status.Conditions,
		Generation:    neutron.Generation,
		ConditionType: "SecretsReady",
		RequeueAfter:  commonreconcile.RequeueSecretPolling,
	})
}
