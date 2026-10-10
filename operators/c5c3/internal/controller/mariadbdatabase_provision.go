// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/apply"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/database"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/openbao"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// provisionDatabase makes the order's schema and the OpenBao role that issues
// its users, and waits for ESO to materialise the first credential. It writes
// DatabaseReady and reports whether the credential is ready to deliver.
//
// The steps run in this order, each gating the next:
//  1. The MariaDB: Keystone's cluster resolves and the shared MariaDB is Ready.
//  2. The schema name: a MySQL identifier, not a system schema, and on no other
//     Database CR of the same MariaDB. The mariadb-operator runs CREATE
//     DATABASE IF NOT EXISTS, so an existing schema would be adopted silently
//     and the role would grant the owner ALL PRIVILEGES on it.
//  3. The shared OpenBao identity in the ControlPlane's namespace: the
//     ServiceAccount the generators present and the client certificate they
//     and the operator present, until cert-manager has issued it.
//  4. The OpenBao role, written by the operator itself.
//  5. The Database CR beside the MariaDB, until the mariadb-operator reports it
//     Ready.
//  6. The generator and the ExternalSecret, until ESO has materialised an
//     engine-issued credential.
func (r *MariaDBDatabaseReconciler) provisionDatabase(
	ctx context.Context, order *c5c3v1alpha1.MariaDBDatabase, cp *c5c3v1alpha1.ControlPlane, cluster string,
) (bool, ctrl.Result, error) {
	fail := mariaDBDatabaseFail(order, conditionTypeMariaDBDatabaseDatabaseReady)
	requeue := ctrl.Result{RequeueAfter: dbCredentialsRequeueAfter}
	ref := mariaDBDatabaseRef(order, cluster)
	db := effectiveKeystoneDatabase(cp)
	keystoneNS := cp.KeystoneNamespace()

	// 1. The MariaDB runs beside Keystone, so its Database CRs are written there.
	dbClient, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client, cp.KeystoneTargetClusterRef())
	if err != nil {
		fail(commonmulticluster.TargetClusterUnavailable, err.Error())
		return false, requeue, nil
	}
	ready, err := database.IsClusterReady(ctx, dbClient, db, keystoneNS)
	if err != nil {
		return false, ctrl.Result{}, fmt.Errorf("checking MariaDB cluster readiness: %w", err)
	}
	if !ready {
		fail(database.ReasonClusterNotReady, fmt.Sprintf("MariaDB cluster %q in namespace %q is not ready",
			db.ClusterRef.Name, keystoneNS))
		return false, requeue, nil
	}

	// 2. A schema the order did not create is never taken over. Only an edit
	// outside the order lifts any of the refusals, so none requeues.
	dbName := mariaDBDatabaseName(order)
	if !mariaDBSchemaNamePattern.MatchString(dbName) {
		fail(reasonMariaDBDatabaseNameInvalid, fmt.Sprintf(
			"database name %q is not a MySQL identifier of 1 to 64 characters from [A-Za-z0-9_]; delete and "+
				"re-create the order with another databaseName", dbName))
		return false, ctrl.Result{}, nil
	}
	if slices.ContainsFunc(mariaDBReservedSchemas, func(s string) bool { return strings.EqualFold(s, dbName) }) {
		fail(reasonMariaDBDatabaseNameReserved, fmt.Sprintf(
			"database name %q is a MariaDB system schema; delete and re-create the order with another databaseName", dbName))
		return false, ctrl.Result{}, nil
	}
	taken, err := r.mariaDBDatabaseTakenBy(ctx, dbClient, order, cluster, db, keystoneNS, dbName)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	if taken != "" {
		// The status is the order owner's to read, so the other Database CR, which
		// may be another owner's order, is named in the log only.
		log.FromContext(ctx).Info("the database an order names is taken on the shared MariaDB",
			"order", client.ObjectKeyFromObject(order), "cluster", cluster, "database", dbName,
			"mariadb", types.NamespacedName{Namespace: keystoneNS, Name: db.ClusterRef.Name}, "takenBy", taken)
		fail(reasonMariaDBDatabaseCollision, fmt.Sprintf(
			"database %q is already in use on the ControlPlane's shared MariaDB; the order never takes over a "+
				"database it did not create; delete and re-create the order with another databaseName", dbName))
		return false, ctrl.Result{}, nil
	}

	// 3. The identity every database order of the namespace shares. It carries no
	// order labels and no ownership claim: no order sweeps it, and it leaves with
	// the namespace.
	roleName := mariaDBDatabaseRoleName(cp.Namespace, order, cluster)
	target := dbCredentialTarget{
		namespace:    cp.Namespace,
		secretName:   mariaDBDatabaseGeneratorName(order, cluster),
		certName:     orderDBClientCertName,
		saName:       orderDBCredentialServiceAccountName,
		vaultRole:    orderDBDynamicVaultRole,
		credsPath:    dbEngineCredsPath(roleName),
		storeRef:     effectiveControlPlaneStoreRef(cp),
		targetLabels: ref.childLabels(),
	}
	certSecret, err := r.ensureOrderDBIdentity(ctx, target)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	if certSecret == nil {
		fail(reasonMariaDBDatabaseWaitingForClientCertificate, fmt.Sprintf(
			"the OpenBao client certificate %s/%s is not issued yet", cp.Namespace, orderDBClientCertName))
		return false, requeue, nil
	}

	// 4. The role. Every client error backs the pass off and counts in
	// c5c3_operator_reconcile_errors_total.
	if err := r.ensureOpenBaoRole(ctx, cp, certSecret, roleName, mariaDBDatabaseOpenBaoRole(cp, dbName)); err != nil {
		message := err.Error()
		if openbao.IsPermissionDenied(err) {
			message = mariaDBDatabaseOpenBaoBootstrapHint + message
		}
		fail(reasonMariaDBDatabaseOpenBaoError, message)
		return false, ctrl.Result{}, fmt.Errorf("ensuring OpenBao database role %q: %w", roleName, err)
	}
	order.Status.RoleName = roleName

	// 5. The Database CR is applied on every pass, so a changed deletionPolicy
	// reaches cleanupPolicy, which the mariadb-operator does not freeze.
	dbCR := mariaDBDatabaseChild(order, cluster, keystoneNS, db.ClusterRef.Name, dbName)
	if err := ensureOrderChild(ctx, dbClient, r.Scheme, order, ref, dbCR); err != nil {
		if errors.Is(err, errOrderAdoptRefused) {
			fail(reasonMariaDBDatabaseCollision, err.Error())
			return false, ctrl.Result{}, nil
		}
		return false, ctrl.Result{}, fmt.Errorf("ensuring Database %s/%s: %w", keystoneNS, dbCR.Name, err)
	}
	live := &mariadbv1alpha1.Database{}
	if err := dbClient.Get(ctx, client.ObjectKeyFromObject(dbCR), live); client.IgnoreNotFound(err) != nil {
		return false, ctrl.Result{}, fmt.Errorf("reading Database %s/%s: %w", keystoneNS, dbCR.Name, err)
	}
	if !conditions.IsReady(live.Status.Conditions) {
		fail(database.ReasonWaitingForDatabase, fmt.Sprintf("MariaDB Database %s/%s is not ready", keystoneNS, dbCR.Name))
		return false, requeue, nil
	}

	// 6. The generator reads the role through the same OpenBao connection the
	// operator just used.
	server, mount := openBaoConnectionFor(ctx, r.Client, cp.Namespace, target.storeRef)
	if err := r.ensureMariaDBDatabaseChild(ctx, order, cluster, dbCredentialVaultDynamicSecret(target, server, mount)); err != nil {
		return false, ctrl.Result{}, fmt.Errorf("ensuring order VaultDynamicSecret %s/%s: %w",
			cp.Namespace, target.secretName, err)
	}
	if err := r.ensureMariaDBDatabaseChild(ctx, order, cluster, dbCredentialGeneratorExternalSecret(target)); err != nil {
		return false, ctrl.Result{}, fmt.Errorf("ensuring order ExternalSecret %s/%s: %w",
			cp.Namespace, target.secretName, err)
	}
	esKey := types.NamespacedName{Namespace: cp.Namespace, Name: target.secretName}
	_, synced, err := secrets.WaitForExternalSecret(ctx, r.Client, esKey)
	if err != nil {
		return false, ctrl.Result{}, fmt.Errorf("checking order ExternalSecret %s: %w", esKey, err)
	}
	if !synced {
		fail(reasonMariaDBDatabaseWaitingForCredentials, fmt.Sprintf(
			"ESO has not issued a credential from OpenBao path %q yet (ExternalSecret %s/%s)",
			target.credsPath, esKey.Namespace, esKey.Name))
		return false, requeue, nil
	}
	materialised := &corev1.Secret{}
	if err := r.Get(ctx, esKey, materialised); client.IgnoreNotFound(err) != nil {
		return false, ctrl.Result{}, fmt.Errorf("reading the materialised credential %s: %w", esKey, err)
	}
	if !strings.HasPrefix(string(materialised.Data[mariaDBDatabaseUsernameKey]), engineIssuedUsernamePrefix) ||
		len(materialised.Data[mariaDBDatabasePasswordKey]) == 0 {
		fail(reasonMariaDBDatabaseWaitingForCredentials, fmt.Sprintf(
			"the materialised credential %s/%s does not carry an engine-issued username yet", esKey.Namespace, esKey.Name))
		return false, requeue, nil
	}

	order.Status.DatabaseName = dbName
	mariaDBDatabaseSetTrue(order, conditionTypeMariaDBDatabaseDatabaseReady, reasonMariaDBDatabaseProvisioned,
		fmt.Sprintf("database %q on MariaDB %s/%s is provisioned; OpenBao role %q issues its users",
			dbName, keystoneNS, db.ClusterRef.Name, roleName))
	return true, ctrl.Result{}, nil
}

// mariaDBDatabaseTakenBy returns the name of a Database CR on the shared
// MariaDB that carries the schema dbName and that the order did not create, or
// the empty string. The MariaDB's cluster serves the kind by construction, so a
// list error is an error, a missing kind included.
func (r *MariaDBDatabaseReconciler) mariaDBDatabaseTakenBy(
	ctx context.Context, dbClient client.Client, order *c5c3v1alpha1.MariaDBDatabase, cluster string,
	db *commonv1.DatabaseSpec, namespace, dbName string,
) (string, error) {
	// The items are only read, so the cache's objects are listed without a copy.
	var list mariadbv1alpha1.DatabaseList
	if err := dbClient.List(ctx, &list, client.InNamespace(namespace), client.UnsafeDisableDeepCopy); err != nil {
		return "", fmt.Errorf("listing Databases in %q: %w", namespace, err)
	}
	for i := range list.Items {
		item := &list.Items[i]
		if item.Spec.MariaDBRef.Name == db.ClusterRef.Name && cmp.Or(item.Spec.Name, item.Name) == dbName &&
			!ownsMariaDBDatabaseChild(item, order, cluster) {
			return item.Name, nil
		}
	}
	return "", nil
}

// ensureOrderDBIdentity applies the ServiceAccount and the client Certificate
// every database order of t.namespace shares, and returns the Secret
// cert-manager issued for the Certificate, or nil while it does not carry the
// keypair and the CA yet.
func (r *MariaDBDatabaseReconciler) ensureOrderDBIdentity(ctx context.Context, t dbCredentialTarget) (*corev1.Secret, error) {
	if err := apply.EnsureUnownedObject(ctx, r.Client, r.Scheme, dbCredentialServiceAccount(t), apply.FieldManager); err != nil {
		return nil, fmt.Errorf("ensuring ServiceAccount %s/%s: %w", t.namespace, t.saName, err)
	}

	// The Certificate is unstructured (no Go module ships its type), so it stays
	// read-modify-write, as the ControlPlane's own client Certificates do.
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName(t.certName)
	cert.SetNamespace(t.namespace)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cert, func() error {
		applyDBCredentialCertificateSpec(cert, t.certName, t.namespace)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("ensuring Certificate %s/%s: %w", t.namespace, t.certName, err)
	}

	key := types.NamespacedName{Namespace: t.namespace, Name: t.certName}
	issued := &corev1.Secret{}
	switch err := r.Get(ctx, key, issued); {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("reading the OpenBao client certificate %s: %w", key, err)
	case !openBaoClientCertIssued(issued):
		return nil, nil
	}
	return issued, nil
}

// mariaDBDatabaseOpenBaoRole is the role an order's generator reads: users
// with ALL PRIVILEGES on the order's schema and nothing else, dropped at lease
// end, against the Keystone connection of the ControlPlane, which is the
// shared database's. The schema's underscores are escaped as \_ because a
// database-level GRANT reads a bare _ as a one-character wildcard
// (setup-database-tenant.sh does the same). provisionDatabase admits only a
// name of mariaDBSchemaNamePattern, so it carries no backtick to escape.
func mariaDBDatabaseOpenBaoRole(cp *c5c3v1alpha1.ControlPlane, dbName string) openbao.DatabaseRole {
	return openbao.DatabaseRole{
		DBName: dbDynamicRoleFor(cp),
		CreationStatements: []string{
			"CREATE USER '{{name}}'@'%' IDENTIFIED BY '{{password}}';",
			"GRANT ALL PRIVILEGES ON `" + strings.ReplaceAll(dbName, "_", `\_`) + "`.* TO '{{name}}'@'%';",
		},
		RevocationStatements: []string{"DROP USER IF EXISTS '{{name}}'@'%';"},
		DefaultTTL:           orderDBCredentialDefaultTTL,
		MaxTTL:               orderDBCredentialMaxTTL,
	}
}

// ensureOpenBaoRole logs in on OpenBao and writes the role name when it is
// absent or differs from want. The session's token is revoked before it
// returns.
func (r *MariaDBDatabaseReconciler) ensureOpenBaoRole(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, certSecret *corev1.Secret, name string, want openbao.DatabaseRole,
) error {
	bao, err := r.loginOpenBao(ctx, cp.Namespace, effectiveControlPlaneStoreRef(cp), certSecret)
	if err != nil {
		return err
	}
	defer closeOpenBao(ctx, bao)
	have, ok, err := bao.ReadDatabaseRole(ctx, dbEngineMount, name)
	if err != nil {
		return err
	}
	if ok && reflect.DeepEqual(*have, want) {
		return nil
	}
	return bao.WriteDatabaseRole(ctx, dbEngineMount, name, want)
}

// mariaDBDatabaseChild builds the order's Database CR in Keystone's namespace on
// the MariaDB clusterRef names. The mariadb-operator resolves mariaDbRef in the
// Database's own namespace, which is why the CR lives beside the MariaDB rather
// than in the ControlPlane's namespace. cleanupPolicy carries the deletion
// policy: Delete drops the schema with the CR, Skip leaves it.
func mariaDBDatabaseChild(
	order *c5c3v1alpha1.MariaDBDatabase, cluster, namespace, clusterRef, dbName string,
) *mariadbv1alpha1.Database {
	dbCR := database.BuildDatabase(database.ProvisionParams{
		Name:         mariaDBDatabaseChildName(order, cluster),
		Namespace:    namespace,
		Labels:       mariaDBDatabaseRef(order, cluster).childLabels(),
		ClusterRef:   clusterRef,
		DatabaseName: dbName,
	})
	dbCR.Spec.MariaDBRef.WaitForIt = true
	dbCR.Spec.CleanupPolicy = ptr.To(mariaDBDatabaseCleanupPolicy(order))
	return dbCR
}

// mariaDBDatabaseCleanupPolicy is the Database CR's cleanupPolicy the order's
// deletion policy selects: Delete for Delete, Skip for Retain.
func mariaDBDatabaseCleanupPolicy(order *c5c3v1alpha1.MariaDBDatabase) mariadbv1alpha1.CleanupPolicy {
	if mariaDBDatabaseDeletionPolicy(order) == c5c3v1alpha1.MariaDBDatabaseDeletionPolicyDelete {
		return mariadbv1alpha1.CleanupPolicyDelete
	}
	return mariadbv1alpha1.CleanupPolicySkip
}
