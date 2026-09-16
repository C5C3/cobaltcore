// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller - reconcileDBConnectionSecrets materialises Nova's two
// database connection URLs into derived Kubernetes Secrets named
// <nova.Name>-api-db-connection and <nova.Name>-db-connection.
//
// The shared database.ReconcileConnectionSecret reads the upstream credentials
// Secret (synced by ESO) and writes the fully-formed pymysql URL into a derived
// Secret. Nova splits its state across two schemas: nova_api holds the cell map,
// the flavors and the instance mappings, and the cell schema holds the instances
// themselves. The API server reads both, the conductor reads the cell schema on
// behalf of every compute node, and each schema runs its own db-sync, so the two
// URLs are derived separately and consumed through separate oslo.config
// OS_<GROUP>__CONNECTION env vars, keeping the passwords out of the rendered
// config entirely. Both derived Secrets are plain corev1.Secrets: no PushSecret
// or ExternalSecret is created.

package controller

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/database"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// apiDBTLSMountPath and cellDBTLSMountPath are the in-pod directories where the
// two db-tls Secrets (the client TLS keypairs) are projected; the
// ssl_ca/ssl_cert/ssl_key DSN parameters of each schema reference files inside
// their own directory so the keypair bytes never enter the operator process. The
// file names inside them come from database.TLSFilePaths, so the DSN paths and
// the workload mount layout stay in lockstep. The two schemas get separate
// directories because each carries its own certificate references.
//
// They sit beside /etc/nova rather than inside the config mounts under it:
// novaConfigDir and every role overlay directory are read-only ConfigMap mounts,
// and a mount nested inside one could not be created, because the runtime cannot
// make the mountpoint directory in a read-only volume.
const (
	apiDBTLSMountPath  = "/etc/nova-db-tls/api/"
	cellDBTLSMountPath = "/etc/nova-db-tls/cell/"
)

// dsnDigests pairs the SHA-256 digest of the nova_api DSN with the digest of the
// cell DSN. The deployment steps stamp the digests a workload consumes into its
// pod-template annotations, so a rotated Dynamic (engine-issued) credential
// rolls the pods that read that schema without the operator reading the Secret
// content itself.
type dsnDigests struct {
	api  string
	cell string
}

// apiInstanceName returns the instance name of the nova_api schema,
// "<nova.Name>-api". It names that schema's MariaDB Database, User and Grant CRs
// and its derived <nova.Name>-api-db-connection Secret, so the two schemas of one
// Nova never collide on a shared MariaDB.
func apiInstanceName(nova *novav1alpha1.Nova) string {
	return nova.Name + "-api"
}

// reconcileDBConnectionSecrets derives both database connection URLs from their
// upstream credentials Secrets and writes them to <nova.Name>-api-db-connection
// and <nova.Name>-db-connection, delegating to the shared
// database.ReconcileConnectionSecret. When an upstream Secret or its required
// keys are missing the shared flow sets SecretsReady=False with reason
// WaitingForDBCredentials and requeues; it never writes a derived Secret with
// empty credentials.
//
// The API block runs first and returns on its own requeue, so a cell Secret is
// never derived while the nova_api credentials are still missing: every process
// that reads the cell schema reads nova_api too, and the pair is what db-sync
// and the cell mapping operate on.
//
// The returned digests are empty on the requeue and error paths where no derived
// Secret was materialised.
func (r *NovaReconciler) reconcileDBConnectionSecrets(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova,
) (ctrl.Result, dsnDigests, error) {
	result, apiDigest, err := database.ReconcileConnectionSecret(ctx, database.ConnectionSecretFlowParams{
		Client:        children,
		Scheme:        r.Scheme,
		Owner:         nova,
		InstanceName:  apiInstanceName(nova),
		Namespace:     nova.Namespace,
		Database:      &nova.Spec.APIDatabase,
		TLSMountPath:  apiDBTLSMountPath,
		Conditions:    &nova.Status.Conditions,
		Generation:    nova.Generation,
		ConditionType: "SecretsReady",
		RequeueAfter:  commonreconcile.RequeueSecretPolling,
	})
	if err != nil {
		return ctrl.Result{}, dsnDigests{}, fmt.Errorf("api database connection secret: %w", err)
	}
	if !result.IsZero() {
		return result, dsnDigests{}, nil
	}

	result, cellDigest, err := database.ReconcileConnectionSecret(ctx, database.ConnectionSecretFlowParams{
		Client:        children,
		Scheme:        r.Scheme,
		Owner:         nova,
		InstanceName:  nova.Name,
		Namespace:     nova.Namespace,
		Database:      &nova.Spec.Database,
		TLSMountPath:  cellDBTLSMountPath,
		Conditions:    &nova.Status.Conditions,
		Generation:    nova.Generation,
		ConditionType: "SecretsReady",
		RequeueAfter:  commonreconcile.RequeueSecretPolling,
	})
	if err != nil {
		return ctrl.Result{}, dsnDigests{}, fmt.Errorf("cell database connection secret: %w", err)
	}
	if !result.IsZero() {
		return result, dsnDigests{}, nil
	}

	return ctrl.Result{}, dsnDigests{api: apiDigest, cell: cellDigest}, nil
}
