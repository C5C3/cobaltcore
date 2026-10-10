// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/database"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/openbao"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// The two sub-conditions a MariaDBDatabase carries, plus the aggregate Ready
// derived from them. DatabaseReady reports the OpenBao role, the schema and the
// issued credential; DeliveryReady reports the Secret beside the order.
const (
	conditionTypeMariaDBDatabaseDatabaseReady = "DatabaseReady"
	conditionTypeMariaDBDatabaseDeliveryReady = "DeliveryReady"
)

// mariaDBDatabaseSubConditionTypes are the sub-conditions the aggregate Ready
// is derived from.
var mariaDBDatabaseSubConditionTypes = []string{
	conditionTypeMariaDBDatabaseDatabaseReady,
	conditionTypeMariaDBDatabaseDeliveryReady,
}

// mariaDBDatabaseFinalizerName gates the teardown of everything an order
// created.
const mariaDBDatabaseFinalizerName = "c5c3.io/mariadbdatabase-teardown"

// The ownership labels an order's children carry, for the reason the
// KeystoneUser labels give: no child can hold an owner reference to the order.
const (
	mariaDBDatabaseNameLabel      = "c5c3.io/mariadbdatabase-name"
	mariaDBDatabaseNamespaceLabel = "c5c3.io/mariadbdatabase-namespace"
	mariaDBDatabaseClusterLabel   = "c5c3.io/mariadbdatabase-cluster"
)

// mariaDBDatabaseLabelKeys are the three ownership label keys as the shared
// order scaffold takes them.
var mariaDBDatabaseLabelKeys = orderLabelKeys{
	Name: mariaDBDatabaseNameLabel, Namespace: mariaDBDatabaseNamespaceLabel, Cluster: mariaDBDatabaseClusterLabel,
}

// mariaDBDatabaseGVK is the kind the order watch filters target clusters on.
var mariaDBDatabaseGVK = c5c3v1alpha1.GroupVersion.WithKind("MariaDBDatabase")

// Condition reasons this controller introduces. The admission gates write the
// scaffold's reasons (keystoneorder.go); the readiness of the MariaDB and of
// the Database CR reuse the database package's vocabulary
// (database.ReasonClusterNotReady, database.ReasonWaitingForDatabase), and an
// unresolvable cluster writes commonmulticluster.TargetClusterUnavailable.
const (
	// reasonMariaDBDatabaseDynamicUnavailable reports a ControlPlane whose shared
	// database issues no dynamic credentials, which an order needs.
	reasonMariaDBDatabaseDynamicUnavailable = "DynamicCredentialsUnavailable"
	// reasonMariaDBDatabaseWaitingForDBCredentials reports a ControlPlane whose
	// DBCredentialsReady is not True: the engine connection is not onboarded yet.
	reasonMariaDBDatabaseWaitingForDBCredentials = "WaitingForDBCredentials"
	// reasonMariaDBDatabaseNameInvalid reports a database name that is not a
	// MySQL identifier of mariaDBSchemaNamePattern.
	reasonMariaDBDatabaseNameInvalid = "DatabaseNameInvalid"
	// reasonMariaDBDatabaseNameReserved reports a database name that is a
	// MariaDB system schema.
	reasonMariaDBDatabaseNameReserved = "DatabaseNameReserved"
	// reasonMariaDBDatabaseCollision reports a schema another Database CR on
	// the same MariaDB already carries, or a Database CR of the child's name the
	// order did not create.
	reasonMariaDBDatabaseCollision = "DatabaseCollision"
	// reasonMariaDBDatabaseWaitingForClientCertificate reports that cert-manager
	// has not issued the OpenBao client certificate yet.
	reasonMariaDBDatabaseWaitingForClientCertificate = "WaitingForClientCertificate"
	// reasonMariaDBDatabaseOpenBaoError reports a failed login or role write
	// against OpenBao.
	reasonMariaDBDatabaseOpenBaoError = "OpenBaoError"
	// reasonMariaDBDatabaseWaitingForCredentials reports that ESO has not
	// materialised an engine-issued credential yet, or that the delivered
	// Secret does not carry it yet.
	reasonMariaDBDatabaseWaitingForCredentials = "WaitingForCredentials"
	// reasonMariaDBDatabaseProvisioned is DatabaseReady's True reason.
	reasonMariaDBDatabaseProvisioned = "DatabaseProvisioned"
	// reasonMariaDBDatabaseNotPublished reports an order on a cluster that
	// cannot reach the in-cluster MariaDB Service while the ControlPlane
	// publishes no database endpoint.
	reasonMariaDBDatabaseNotPublished = "DatabaseNotPublished"
	// reasonMariaDBDatabaseWaitingForCABundle reports a database that runs TLS
	// whose CA bundle Secret does not carry ca.crt yet.
	reasonMariaDBDatabaseWaitingForCABundle = "WaitingForCABundle"
	// reasonMariaDBDatabaseDelivered is DeliveryReady's True reason.
	reasonMariaDBDatabaseDelivered = "Delivered"
	// reasonMariaDBDatabaseDeliveryRefused reports a Secret of the delivered
	// name beside the order that the order does not control.
	reasonMariaDBDatabaseDeliveryRefused = "DeliveryRefused"
	// reasonMariaDBDatabaseDeliveryError reports a published endpoint that is not
	// host:port, or a Kubernetes-level failure reading or writing what the
	// delivery needs.
	reasonMariaDBDatabaseDeliveryError = "DeliveryError"
)

// The names every database order of a ControlPlane namespace shares, and the
// bounds of the role each order writes.
const (
	// orderDBCredentialServiceAccountName is the ServiceAccount the generators
	// present to OpenBao. It is bound by the order-db auth role
	// (deploy/openbao/bootstrap/setup-auth.sh).
	orderDBCredentialServiceAccountName = "order-db-creds" //nolint:gosec // G101 false positive: ServiceAccount name, not a credential.
	// orderDBClientCertName names the cert-manager Certificate, and the Secret it
	// materialises, carrying the mTLS keypair the generators and the operator's
	// own OpenBao client present to the listener.
	orderDBClientCertName = "order-db-openbao-client"
	// orderDBDynamicVaultRole is the Kubernetes-auth role the generators log in
	// with (setup-auth.sh, the order-db role).
	orderDBDynamicVaultRole = "order-db"
	// openBaoOperatorAuthRole is the Kubernetes-auth role the operator logs in
	// with (setup-auth.sh, the c5c3-operator role).
	openBaoOperatorAuthRole = "c5c3-operator"
	// dbEngineMount is the mount of the MariaDB database engine
	// (setup-secret-engines.sh), which the built-in roles share.
	dbEngineMount = "database/mariadb"
	// orderDBCredentialDefaultTTL and orderDBCredentialMaxTTL are the lease TTLs
	// of an order role. They mirror DB_CREDS_DEFAULT_TTL and DB_CREDS_MAX_TTL of
	// setup-database-tenant.sh, which the built-in roles use and which the
	// order-db auth role's 72h tokens are sized for.
	orderDBCredentialDefaultTTL = 48 * time.Hour
	orderDBCredentialMaxTTL     = 72 * time.Hour
	// operatorServiceAccountTokenPath is where the kubelet projects the
	// operator's own ServiceAccount token.
	operatorServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // G101 false positive: a file path, not a credential.
)

// dbEngineCredsPath is the engine path the credentials of role are read from,
// and the prefix their leases are revoked by.
func dbEngineCredsPath(role string) string {
	return dbEngineMount + "/creds/" + role
}

// mariaDBReservedSchemas are the MariaDB system schemas an order never names.
var mariaDBReservedSchemas = []string{"mysql", "information_schema", "performance_schema", "sys"}

// mariaDBSchemaNamePattern is the schema name an order may resolve to: the
// databaseName pattern and maxLength of the CRD. The reconciler checks it again
// because the name reaches SQL that OpenBao runs as the MariaDB root user,
// while the CRD of an order on a target cluster is installed by that cluster's
// administrator. A name of these characters carries no backtick that would end
// the quoted identifier and no % that a GRANT would read as a wildcard.
var mariaDBSchemaNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// The keys of the delivered Secret.
const (
	mariaDBDatabaseHostKey     = "host"
	mariaDBDatabasePortKey     = "port"
	mariaDBDatabaseDatabaseKey = "database"
	mariaDBDatabaseUsernameKey = "username"
	mariaDBDatabasePasswordKey = "password"
	mariaDBDatabaseCAKey       = database.TLSCAFileName
)

// mariaDBDatabaseCredentialsSecretSuffix is the tail of the delivered Secret's
// name. It is part of the MariaDBDatabase API.
const mariaDBDatabaseCredentialsSecretSuffix = "-credentials"

// mariaDBDatabaseOpenBaoBootstrapHint prefixes an OpenBaoError caused by a 403:
// the operator's auth role or policy, or the widened connection, is missing on
// a deployment that has not re-run the bootstrap.
const mariaDBDatabaseOpenBaoBootstrapHint = "the operator's OpenBao login or policy is missing; re-run " +
	"deploy/openbao/bootstrap/setup-auth.sh, setup-policies.sh and setup-database-tenant.sh: "

// MariaDBDatabaseReconciler owns the MariaDBDatabase lifecycle and is the
// single writer of its status. It serves orders on the management cluster and
// on target clusters the way the KeystoneUser reconciler does: the order, its
// status, its finalizer and its delivered Secret are read and written on the
// order's cluster. The ESO generator and ExternalSecret live in the
// ControlPlane's namespace on the management cluster, and the Database CR beside
// the MariaDB on Keystone's cluster. It records no Events, for the reason the
// KeystoneUser reconciler gives.
type MariaDBDatabaseReconciler struct {
	// Client is the management cluster's client.
	client.Client
	Scheme *runtime.Scheme
	// Resolver resolves the cluster an order lives on and the cluster the
	// MariaDB runs on; nil resolves every request to the management cluster.
	Resolver                commonmulticluster.ClusterResolver
	MaxConcurrentReconciles int
	// OpenBaoDial logs in on OpenBao; nil means openbao.Login.
	OpenBaoDial func(context.Context, openbao.Config) (openbao.Client, error)
	// ServiceAccountToken returns the token the operator logs in with; nil reads
	// operatorServiceAccountTokenPath on every call, because the kubelet rotates
	// the projected token in place.
	ServiceAccountToken func() (string, error)
}

// RBAC for the MariaDBDatabase kind: the controller reads the orders and
// updates them to install and release its finalizer; it never creates or
// deletes one. It writes the order's mariadb-operator Database CR beside the
// MariaDB. The ServiceAccount, Certificate, VaultDynamicSecret, ExternalSecret
// and Secrets it writes in the ControlPlane's namespace, and the ControlPlane
// and MariaDB reads, are granted by the ControlPlane's marker block. On a target
// cluster the target-cluster-access chart's Role for an assigned namespace
// grants the same verbs on the order.
// +kubebuilder:rbac:groups=c5c3.io,resources=mariadbdatabases,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=mariadbdatabases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=mariadbdatabases/finalizers,verbs=update
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=databases,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives one MariaDBDatabase: the gates, finalizer installation, the
// provision and delivery, and the teardown.
func (r *MariaDBDatabaseReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	cluster := string(req.ClusterName)
	oc, err := orderClient(ctx, r.Resolver, r.Client, cluster)
	if err != nil {
		// The cluster was deregistered between the event and this pass. Nothing
		// can be read or written there, and a requeue would only repeat this.
		log.FromContext(ctx).Info("the cluster the MariaDBDatabase lives on does not resolve; skipping it",
			"cluster", cluster, "order", req.NamespacedName, "reason", err.Error())
		return ctrl.Result{}, nil
	}

	var order c5c3v1alpha1.MariaDBDatabase
	if err := oc.Get(ctx, req.NamespacedName, &order); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("MariaDBDatabase not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching MariaDBDatabase: %w", err)
	}

	if order.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, oc, &order, cluster)
	}

	statusBefore := order.Status.DeepCopy()
	result, err := r.reconcileNormal(ctx, oc, &order, cluster)
	return r.updateStatus(ctx, oc, &order, statusBefore, result, err)
}

// openBaoDial logs in on OpenBao through OpenBaoDial, or openbao.Login when it
// is nil.
func (r *MariaDBDatabaseReconciler) openBaoDial(ctx context.Context, cfg openbao.Config) (openbao.Client, error) {
	if r.OpenBaoDial != nil {
		return r.OpenBaoDial(ctx, cfg)
	}
	return openbao.Login(ctx, cfg)
}

// serviceAccountToken returns the operator's ServiceAccount token through
// ServiceAccountToken, or read from operatorServiceAccountTokenPath when it is
// nil.
func (r *MariaDBDatabaseReconciler) serviceAccountToken() (string, error) {
	if r.ServiceAccountToken != nil {
		return r.ServiceAccountToken()
	}
	token, err := os.ReadFile(operatorServiceAccountTokenPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(token)), nil
}

// loginOpenBao logs the operator in on OpenBao with its own ServiceAccount
// token and the client keypair certSecret carries. The server and the
// Kubernetes-auth mount come from storeRef, a namespaced store resolved in
// namespace, the way the generators' connection does.
func (r *MariaDBDatabaseReconciler) loginOpenBao(
	ctx context.Context, namespace string, storeRef commonv1.SecretStoreRefSpec, certSecret *corev1.Secret,
) (openbao.Client, error) {
	jwt, err := r.serviceAccountToken()
	if err != nil {
		return nil, fmt.Errorf("reading the operator's ServiceAccount token: %w", err)
	}
	server, mount := openBaoConnectionFor(ctx, r.Client, namespace, storeRef)
	return r.openBaoDial(ctx, openbao.Config{
		Server:          server,
		KubernetesMount: mount,
		Role:            openBaoOperatorAuthRole,
		JWT:             jwt,
		CACert:          certSecret.Data[database.TLSCAFileName],
		ClientCert:      certSecret.Data[database.TLSCertFileName],
		ClientKey:       certSecret.Data[database.TLSKeyFileName],
	})
}

// closeOpenBao revokes the session's token. A failure is logged and dropped:
// the token expires within the c5c3-operator role's 15 minutes anyway.
func closeOpenBao(ctx context.Context, bao openbao.Client) {
	if err := bao.Close(ctx); err != nil {
		log.FromContext(ctx).V(1).Info("revoking the operator's OpenBao token failed", "error", err.Error())
	}
}

// openBaoClientCertIssued reports whether the client-certificate Secret carries
// the keypair and the CA a login needs.
func openBaoClientCertIssued(secret *corev1.Secret) bool {
	for _, key := range []string{database.TLSCertFileName, database.TLSKeyFileName, database.TLSCAFileName} {
		if len(secret.Data[key]) == 0 {
			return false
		}
	}
	return true
}

// reconcileNormal runs the gates, then the provision and the delivery.
//
// The freeze of #1327 D2 is the assignment gate of orderAdmission: without an
// entry the pass writes NamespaceNotAssigned and returns before anything is read
// or written in the ControlPlane's namespace, in Keystone's namespace or in
// OpenBao. The finalizer is installed only past that gate.
func (r *MariaDBDatabaseReconciler) reconcileNormal(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.MariaDBDatabase, cluster string,
) (ctrl.Result, error) {
	failBoth := func(reason, message string) { mariaDBDatabaseFailBoth(order, reason, message) }
	cp, _, result, err := orderAdmission(ctx, r.Client, mariaDBDatabaseRef(order, cluster),
		order.Spec.ControlPlaneRef, failBoth)
	if err != nil || cp == nil {
		return result, err
	}

	if added, err := commonreconcile.EnsureFinalizer(ctx, oc, order, mariaDBDatabaseFinalizerName); err != nil {
		return ctrl.Result{}, err
	} else if added {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}, nil
	}

	if result, ok := mariaDBDatabaseDynamicGate(cp, failBoth); !ok {
		return mariaDBDatabasePassResult(order, cluster, result), nil
	}

	provisioned := false
	result, err = instrumenter.Instrument(ctx, "MariaDBDatabaseProvision", func(ctx context.Context) (ctrl.Result, error) {
		ok, res, err := r.provisionDatabase(ctx, order, cp, cluster)
		provisioned = ok
		return res, err
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	if !provisioned {
		mariaDBDatabaseFail(order, conditionTypeMariaDBDatabaseDeliveryReady)(reasonMariaDBDatabaseWaitingForCredentials,
			"the database is not provisioned yet; nothing is delivered")
		return mariaDBDatabasePassResult(order, cluster, result), nil
	}

	result, err = instrumenter.Instrument(ctx, "MariaDBDatabaseDelivery", func(ctx context.Context) (ctrl.Result, error) {
		return r.deliverCredentials(ctx, oc, order, cp, cluster)
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	return mariaDBDatabasePassResult(order, cluster, result), nil
}

// mariaDBDatabaseDynamicGate reports whether the ControlPlane's shared database
// issues the dynamic credentials an order needs. The OpenBao database engine
// holds a connection for the managed shared database alone, so External mode, a
// brownfield database, a dedicated Keystone database and credentialsMode Static
// are refused with a zero result: only a ControlPlane edit lifts the refusal,
// and the refresh or the ControlPlane watch brings the order back. A plane whose
// DBCredentialsReady is not True has not onboarded its engine connection yet,
// and the order requeues after dbCredentialsRequeueAfter. Both write the
// refusal on both conditions through failBoth.
func mariaDBDatabaseDynamicGate(cp *c5c3v1alpha1.ControlPlane, failBoth func(reason, message string)) (ctrl.Result, bool) {
	if effectiveKeystoneDatabase(cp) == nil || !dbCredentialsDynamicEnabled(cp) {
		failBoth(reasonMariaDBDatabaseDynamicUnavailable, fmt.Sprintf(
			"ControlPlane %s/%s does not issue dynamic credentials for its shared database (External mode, "+
				"a brownfield database, a dedicated Keystone database or credentialsMode Static); a MariaDBDatabase "+
				"order needs the OpenBao database engine, which issues against a managed shared database in Dynamic mode",
			cp.Namespace, cp.Name))
		return ctrl.Result{}, false
	}
	if !conditions.AllTrue(cp.Status.Conditions, conditionTypeDBCredentialsReady) {
		failBoth(reasonMariaDBDatabaseWaitingForDBCredentials, fmt.Sprintf(
			"ControlPlane %s/%s reports DBCredentialsReady is not True; the OpenBao database engine is not "+
				"onboarded for its database yet (deploy/openbao/bootstrap/setup-database-tenant.sh)",
			cp.Namespace, cp.Name))
		return ctrl.Result{RequeueAfter: dbCredentialsRequeueAfter}, false
	}
	return ctrl.Result{}, true
}

// mariaDBDatabasePassResult decides when the next pass runs, with
// DeliveryReady as the converged condition (orderPassResult).
func mariaDBDatabasePassResult(order *c5c3v1alpha1.MariaDBDatabase, cluster string, result ctrl.Result) ctrl.Result {
	return orderPassResult(cluster,
		conditions.AllTrue(order.Status.Conditions, conditionTypeMariaDBDatabaseDeliveryReady), result)
}

// mariaDBDatabaseControlPlaneKey resolves the order's controlPlaneRef, the
// namespace defaulting to the order's own.
func mariaDBDatabaseControlPlaneKey(order *c5c3v1alpha1.MariaDBDatabase) client.ObjectKey {
	return orderControlPlaneKey(order.Spec.ControlPlaneRef, order.Namespace)
}

// mariaDBDatabaseFail returns a closure bound to order and condType that writes
// a False condition. The message is truncated, because it relays OpenBao's and
// the API server's own text.
func mariaDBDatabaseFail(order *c5c3v1alpha1.MariaDBDatabase, condType string) func(reason, message string) {
	return func(reason, message string) {
		setTruncatedCondition(&order.Status.Conditions, order.Generation, condType, metav1.ConditionFalse, reason, message)
	}
}

// mariaDBDatabaseFailBoth writes one gate failure onto both sub-conditions.
func mariaDBDatabaseFailBoth(order *c5c3v1alpha1.MariaDBDatabase, reason, message string) {
	for _, condType := range mariaDBDatabaseSubConditionTypes {
		mariaDBDatabaseFail(order, condType)(reason, message)
	}
}

// mariaDBDatabaseSetTrue writes a sub-condition as True.
func mariaDBDatabaseSetTrue(order *c5c3v1alpha1.MariaDBDatabase, condType, reason, message string) {
	setTruncatedCondition(&order.Status.Conditions, order.Generation, condType, metav1.ConditionTrue, reason, message)
}

// updateStatus persists the status through the order's cluster client: the write
// is skipped when the pass changed nothing, the aggregate Ready is re-derived on
// every persist, and ObservedGeneration is set.
func (r *MariaDBDatabaseReconciler) updateStatus(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.MariaDBDatabase,
	statusBefore *c5c3v1alpha1.MariaDBDatabaseStatus, result ctrl.Result, reconcileErr error,
) (ctrl.Result, error) {
	return commonreconcile.UpdateStatus(ctx, oc, order, statusBefore, &order.Status, func() {
		commonreconcile.SetAggregateReady(&order.Status.Conditions, order.Generation, mariaDBDatabaseSubConditionTypes)
		order.Status.ObservedGeneration = order.Generation
	}, result, reconcileErr)
}

// --- naming ---

// mariaDBDatabaseRef identifies the order to the shared scaffold: its children
// carry the "database" segment and the mariadbdatabase labels.
func mariaDBDatabaseRef(order *c5c3v1alpha1.MariaDBDatabase, cluster string) orderRef {
	return orderRef{
		Name: order.Name, Namespace: order.Namespace, Cluster: cluster,
		Segment: "database", Keys: mariaDBDatabaseLabelKeys,
	}
}

// mariaDBDatabaseName resolves the SQL schema name, defaulting to the order's
// name with every dash replaced by an underscore. No defaulting webhook exists
// for the kind, so the reconciler is the only place the default is resolved.
func mariaDBDatabaseName(order *c5c3v1alpha1.MariaDBDatabase) string {
	return cmp.Or(order.Spec.DatabaseName, strings.ReplaceAll(order.Name, "-", "_"))
}

// mariaDBDatabaseDeletionPolicy resolves the deletion policy. A zero value,
// which only a CR stored without the schema default carries, reads as Retain.
func mariaDBDatabaseDeletionPolicy(order *c5c3v1alpha1.MariaDBDatabase) string {
	return cmp.Or(order.Spec.DeletionPolicy, c5c3v1alpha1.MariaDBDatabaseDeletionPolicyRetain)
}

// mariaDBDatabaseChildName names the order's Database CR, beside the MariaDB.
func mariaDBDatabaseChildName(order *c5c3v1alpha1.MariaDBDatabase, cluster string) string {
	return mariaDBDatabaseRef(order, cluster).childPrefix() + "database"
}

// mariaDBDatabaseGeneratorName names the order's VaultDynamicSecret, its
// ExternalSecret and the Secret ESO materialises, which share it as the
// built-in generators do.
func mariaDBDatabaseGeneratorName(order *c5c3v1alpha1.MariaDBDatabase, cluster string) string {
	return mariaDBDatabaseRef(order, cluster).childPrefix() + "credentials"
}

// mariaDBDatabaseRoleName names the order's OpenBao database role:
// order.<ControlPlane namespace>.<child prefix without its trailing dash>. The
// dots are the separators because a namespace cannot contain one, so the
// order-db-dynamic policy's order.<namespace>.* reaches exactly the roles of
// one namespace. The namespace is a string so the teardown can derive the name
// with the ControlPlane gone.
func mariaDBDatabaseRoleName(cpNamespace string, order *c5c3v1alpha1.MariaDBDatabase, cluster string) string {
	return "order." + cpNamespace + "." + strings.TrimSuffix(mariaDBDatabaseRef(order, cluster).childPrefix(), "-")
}

// mariaDBDatabaseCredentialsSecretName is the delivered Secret's name, in the
// order's namespace, predictable from the order's name alone.
func mariaDBDatabaseCredentialsSecretName(order *c5c3v1alpha1.MariaDBDatabase) string {
	return order.Name + mariaDBDatabaseCredentialsSecretSuffix
}

// --- ownership ---

// ownsMariaDBDatabaseChild reports whether obj is a child the order created
// (ownsOrderChild).
func ownsMariaDBDatabaseChild(obj client.Object, order *c5c3v1alpha1.MariaDBDatabase, cluster string) bool {
	return ownsOrderChild(obj, order, mariaDBDatabaseRef(order, cluster))
}

// ensureMariaDBDatabaseChild applies a child in the ControlPlane's namespace on
// the management cluster, refusing one the order did not create
// (ensureOrderChild).
func (r *MariaDBDatabaseReconciler) ensureMariaDBDatabaseChild(
	ctx context.Context, order *c5c3v1alpha1.MariaDBDatabase, cluster string, obj client.Object,
) error {
	return ensureOrderChild(ctx, r.Client, r.Scheme, order, mariaDBDatabaseRef(order, cluster), obj)
}

// --- teardown ---

// reconcileDelete removes everything the order created and releases the
// finalizer through orderTeardown. Nothing references a MariaDBDatabase, so no
// hold is installed.
func (r *MariaDBDatabaseReconciler) reconcileDelete(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.MariaDBDatabase, cluster string,
) (ctrl.Result, error) {
	return orderTeardown(ctx, r.Client, oc, order, mariaDBDatabaseFinalizerName, mariaDBDatabaseControlPlaneKey(order),
		func(ctx context.Context, childNS string) (int, error) {
			return r.sweepMariaDBDatabaseChildren(ctx, oc, order, cluster, childNS)
		})
}

// sweepMariaDBDatabaseChildren issues the deletes of everything the order
// created and reports how many are still listed. childNS is the ControlPlane's
// namespace, which orderTeardown passes whether or not the plane still exists.
//
// The steps run in this order:
//  1. OpenBao: every lease the order's role issued is revoked, which drops the
//     SQL users at once instead of at the end of their leases, and the role is
//     deleted. Without the client-certificate Secret the operator cannot log
//     in; the step is skipped and the leases end at their own max_ttl.
//  2. The ExternalSecret and the VaultDynamicSecret in childNS. ESO reaps the
//     materialised Secret through its owner reference.
//  3. The Database CR beside the MariaDB (sweepMariaDBDatabaseCR). The
//     mariadb-operator drops the schema behind its finalizer under
//     cleanupPolicy Delete and leaves it under Skip; the count holds the
//     order's finalizer until it has run.
//  4. The delivered Secret beside the order.
//
// With the plane present the first failing step ends the pass, which is
// retried, so nothing is deleted before the role is gone. With the plane gone
// orderTeardown releases the finalizer whatever the outcome, so a failing step
// does not keep the later ones from issuing their deletes, and the errors are
// returned together.
//
// With the plane gone, Keystone's namespace reads as childNS and Keystone's
// cluster as the management cluster: a Keystone the plane had placed elsewhere
// leaves its Database CR behind on that cluster.
func (r *MariaDBDatabaseReconciler) sweepMariaDBDatabaseChildren(
	ctx context.Context, oc client.Client, order *c5c3v1alpha1.MariaDBDatabase, cluster, childNS string,
) (int, error) {
	var cp *c5c3v1alpha1.ControlPlane
	live := &c5c3v1alpha1.ControlPlane{}
	switch err := r.Get(ctx, mariaDBDatabaseControlPlaneKey(order), live); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return 0, fmt.Errorf("fetching ControlPlane %s during teardown: %w", mariaDBDatabaseControlPlaneKey(order), err)
	default:
		cp = live
	}

	ref := mariaDBDatabaseRef(order, cluster)
	roleName := mariaDBDatabaseRoleName(childNS, order, cluster)
	steps := []func() (int, error){
		func() (int, error) {
			if err := r.removeOpenBaoRole(ctx, cp, childNS, roleName); err != nil {
				return 0, fmt.Errorf("removing OpenBao database role %q: %w", roleName, err)
			}
			return 0, nil
		},
		func() (int, error) {
			return sweepOrderLists(ctx, r.Client, order, ref, childNS,
				orderSweep{list: &esov1.ExternalSecretList{}},
				orderSweep{list: &esgenv1alpha1.VaultDynamicSecretList{}},
			)
		},
		func() (int, error) { return r.sweepMariaDBDatabaseCR(ctx, order, cp, cluster, childNS) },
		func() (int, error) {
			return sweepDeliveredSecret(ctx, oc, order,
				types.NamespacedName{Namespace: order.Namespace, Name: mariaDBDatabaseCredentialsSecretName(order)})
		},
	}
	issued := 0
	var errs []error
	for _, step := range steps {
		n, err := step()
		issued += n
		if err != nil {
			errs = append(errs, err)
			if cp != nil {
				break
			}
		}
	}
	return issued, errors.Join(errs...)
}

// sweepMariaDBDatabaseCR deletes the order's Database CR beside the MariaDB and
// returns the number of deletes issued. It first writes the deletion policy in
// force into the CR's cleanupPolicy, which the mariadb-operator reads when it
// finalizes the CR: the provision applies the policy only on a pass that gets
// that far, and a policy edit followed at once by the order's deletion reaches
// the reconciler as the deletion alone.
func (r *MariaDBDatabaseReconciler) sweepMariaDBDatabaseCR(
	ctx context.Context, order *c5c3v1alpha1.MariaDBDatabase, cp *c5c3v1alpha1.ControlPlane, cluster, childNS string,
) (int, error) {
	keystoneNS, keystoneCluster := childNS, (*commonv1.TargetClusterRefSpec)(nil)
	if cp != nil {
		keystoneNS, keystoneCluster = cp.KeystoneNamespace(), cp.KeystoneTargetClusterRef()
	}
	dbClient, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client, keystoneCluster)
	if err != nil {
		return 0, fmt.Errorf("resolving the MariaDB's cluster for teardown: %w", err)
	}

	ref := mariaDBDatabaseRef(order, cluster)
	var owned mariadbv1alpha1.DatabaseList
	if err := dbClient.List(ctx, &owned, client.InNamespace(keystoneNS), client.MatchingLabels(ref.childLabels())); err != nil {
		return 0, fmt.Errorf("listing order Databases: %w", err)
	}
	policy := mariaDBDatabaseCleanupPolicy(order)
	for i := range owned.Items {
		item := &owned.Items[i]
		if !ownsMariaDBDatabaseChild(item, order, cluster) || ptr.Deref(item.Spec.CleanupPolicy, "") == policy {
			continue
		}
		base := item.DeepCopy()
		item.Spec.CleanupPolicy = ptr.To(policy)
		if err := client.IgnoreNotFound(dbClient.Patch(ctx, item, client.MergeFrom(base))); err != nil {
			return 0, fmt.Errorf("writing cleanupPolicy %s into Database %s/%s: %w", policy, item.Namespace, item.Name, err)
		}
	}
	return sweepOrderList(ctx, dbClient, order, ref, keystoneNS, &mariadbv1alpha1.DatabaseList{}, nil, nil)
}

// removeOpenBaoRole revokes the leases of the order's role and deletes the
// role. cp is nil once the ControlPlane is gone; the default store then names
// the OpenBao connection. The step is idempotent, so a pass that follows a
// partial sweep repeats it without harm.
func (r *MariaDBDatabaseReconciler) removeOpenBaoRole(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, childNS, roleName string,
) error {
	certKey := types.NamespacedName{Namespace: childNS, Name: orderDBClientCertName}
	certSecret := &corev1.Secret{}
	if err := r.Get(ctx, certKey, certSecret); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("reading the OpenBao client certificate %s: %w", certKey, err)
	}
	if !openBaoClientCertIssued(certSecret) {
		log.FromContext(ctx).Info(fmt.Sprintf(
			"the OpenBao client certificate is gone; leaving database role %q to its TTLs", roleName))
		return nil
	}

	storeRef := secrets.EffectiveStoreRef(nil)
	if cp != nil {
		storeRef = effectiveControlPlaneStoreRef(cp)
	}
	bao, err := r.loginOpenBao(ctx, childNS, storeRef, certSecret)
	if err != nil {
		return err
	}
	defer closeOpenBao(ctx, bao)
	if err := bao.RevokeLeasePrefix(ctx, dbEngineCredsPath(roleName)); err != nil {
		return err
	}
	return bao.DeleteDatabaseRole(ctx, dbEngineMount, roleName)
}

// --- manager setup ---

// MariaDBDatabaseControlPlaneRefIndexKey is the field-indexer key under which a
// MariaDBDatabase on the management cluster is indexed by its resolved
// ControlPlane reference, "<namespace>/<name>".
const MariaDBDatabaseControlPlaneRefIndexKey = "spec.controlPlaneRef"

// mariaDBDatabaseControlPlaneRefExtractor returns the resolved
// "<namespace>/<name>" of obj's ControlPlane reference. An empty name indexes
// nothing.
func mariaDBDatabaseControlPlaneRefExtractor(obj client.Object) []string {
	order, ok := obj.(*c5c3v1alpha1.MariaDBDatabase)
	if !ok || order.Spec.ControlPlaneRef.Name == "" {
		return nil
	}
	key := mariaDBDatabaseControlPlaneKey(order)
	return []string{keystoneServiceControlPlaneRefIndexValue(key.Namespace, key.Name)}
}

// registerMariaDBDatabaseControlPlaneRefIndex registers the field indexer
// controlPlaneToMariaDBDatabasesMapper relies on.
func registerMariaDBDatabaseControlPlaneRefIndex(ctx context.Context, indexer client.FieldIndexer) error {
	return indexer.IndexField(ctx, &c5c3v1alpha1.MariaDBDatabase{},
		MariaDBDatabaseControlPlaneRefIndexKey, mariaDBDatabaseControlPlaneRefExtractor)
}

// controlPlaneToMariaDBDatabasesMapper maps a ControlPlane event to the orders
// on the management cluster that reference it. Orders on target clusters come
// back on orderRefreshAfter. A List failure is logged and maps to nothing.
func controlPlaneToMariaDBDatabasesMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var orders c5c3v1alpha1.MariaDBDatabaseList
		if err := c.List(ctx, &orders, client.MatchingFields{
			MariaDBDatabaseControlPlaneRefIndexKey: keystoneServiceControlPlaneRefIndexValue(obj.GetNamespace(), obj.GetName()),
		}); err != nil {
			log.FromContext(ctx).Error(err, "listing MariaDBDatabases for ControlPlane watch")
			return nil
		}
		requests := make([]reconcile.Request, 0, len(orders.Items))
		for i := range orders.Items {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&orders.Items[i])})
		}
		return requests
	}
}

// databaseToMariaDBDatabaseRequests maps a Database CR event on clusterName
// back to its order by the three ownership labels, but only from a namespace a
// ControlPlane places Keystone in on that cluster: provisionDatabase writes
// every order's Database CR there, beside the MariaDB. The leg also watches the
// assigned namespaces of a target cluster's registration, where a service owner
// may write Database CRs, and one labelled after another owner's order would
// otherwise wake that order on every edit. A Database CR without the order
// labels maps to nothing before the ControlPlanes are listed. A List failure is
// logged and maps to nothing.
func databaseToMariaDBDatabaseRequests(
	c client.Reader, clusterName mcruntime.ClusterName,
) func(context.Context, client.Object) []mcreconcile.Request {
	toOrder := orderChildToRequests(mariaDBDatabaseLabelKeys)
	return func(ctx context.Context, obj client.Object) []mcreconcile.Request {
		requests := toOrder(ctx, obj)
		if len(requests) == 0 {
			return nil
		}
		// The items are only read, so the cache's objects are listed without a copy.
		var planes c5c3v1alpha1.ControlPlaneList
		if err := c.List(ctx, &planes, client.UnsafeDisableDeepCopy); err != nil {
			log.FromContext(ctx).Error(err, "listing ControlPlanes for Database watch")
			return nil
		}
		for i := range planes.Items {
			cp := &planes.Items[i]
			if clusterNameOf(cp.KeystoneTargetClusterRef()) == string(clusterName) &&
				cp.KeystoneNamespace() == obj.GetNamespace() {
				return requests
			}
		}
		return nil
	}
}

// databaseChildRequests is the event-handler factory of the Database leg. It
// hands databaseToMariaDBDatabaseRequests the cluster the event came from and
// keeps the cluster the mapper chose.
func databaseChildRequests(c client.Reader) mchandler.TypedEventHandlerFunc[client.Object, mcreconcile.Request] {
	return func(clusterName mcruntime.ClusterName, _ cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
		return mchandler.TypedEnqueueRequestsFromMapFuncWithClusterPreservation(databaseToMariaDBDatabaseRequests(c, clusterName))
	}
}

// SetupWithManager registers the MariaDBDatabaseReconciler with the
// multicluster manager.
func (r *MariaDBDatabaseReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return r.setupWithOptions(mgr, bootstrap.TypedControllerOptions[mcreconcile.Request](r.MaxConcurrentReconciles))
}

// setupWithOptions carries the production setup SetupWithManager applies, with
// the controller options as a parameter so the integration suites register this
// chain with SkipNameValidation set.
//
// The watches:
//   - For: the order on the management cluster and on every engaged target
//     cluster that serves the kind.
//   - Owns: the delivered Secret beside the order, on the same clusters.
//   - The Secret, ExternalSecret and VaultDynamicSecret children in the
//     ControlPlane's namespace, mapped back by their labels. The materialised
//     Secret carries them through the ExternalSecret's target template, so each
//     ESO refresh brings the order back.
//   - The Database CR, on every cluster that serves the kind: it lives on
//     Keystone's cluster while its cluster label names the order's. Only one in
//     Keystone's namespace maps back (databaseToMariaDBDatabaseRequests).
//   - ControlPlane: the orders on the management cluster that reference it, on
//     a spec change, a deletion and a flip of DBCredentialsReady.
func (r *MariaDBDatabaseReconciler) setupWithOptions(mgr mcmanager.Manager, opts crcontroller.TypedOptions[mcreconcile.Request]) error {
	local := mgr.GetLocalManager()
	if err := registerMariaDBDatabaseControlPlaneRefIndex(context.Background(), local.GetFieldIndexer()); err != nil {
		return err
	}
	engageLocal := commonmulticluster.EngageLocalCluster
	engageNoProviders := commonmulticluster.EngageNoProviderClusters
	engageProviders := mcbuilder.WithEngageWithProviderClusters(true)
	servesKind := mcbuilder.WithClusterFilter(commonmulticluster.ClusterServesKind(mariaDBDatabaseGVK))
	children := orderChildRequests(mariaDBDatabaseLabelKeys)

	return mcbuilder.ControllerManagedBy(mgr).
		WithOptions(opts).
		For(&c5c3v1alpha1.MariaDBDatabase{}, mcbuilder.WithPredicates(watch.CRUpdatePredicate()),
			engageLocal, engageProviders, servesKind).
		Owns(&corev1.Secret{}, engageLocal, engageProviders, servesKind).
		Watches(&corev1.Secret{}, children, engageLocal, engageNoProviders).
		Watches(&esov1.ExternalSecret{}, children, engageLocal, engageNoProviders).
		Watches(&esgenv1alpha1.VaultDynamicSecret{}, children, engageLocal, engageNoProviders).
		Watches(&mariadbv1alpha1.Database{}, databaseChildRequests(local.GetClient()), engageLocal, engageProviders,
			mcbuilder.WithClusterFilter(commonmulticluster.ClusterServesKind(mariadbv1alpha1.GroupVersion.WithKind("Database")))).
		Watches(&c5c3v1alpha1.ControlPlane{},
			commonmulticluster.LocalRequests(controlPlaneToMariaDBDatabasesMapper(local.GetClient())),
			mcbuilder.WithPredicates(orderControlPlanePredicateFor(conditionTypeDBCredentialsReady)),
			engageLocal, engageNoProviders).
		// Reconcile answers an unresolvable cluster itself, so the wrapper that
		// would turn a cluster-not-found error into a success stays off.
		WithClusterNotFoundWrapper(false).
		Complete(r)
}
