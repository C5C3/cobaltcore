// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Integration tests for the Nova reconciler running against a live envtest API
// server.
//
// The timeouts below are generous on purpose. An envtest environment runs the
// API server and etcd and nothing else, so every state this pipeline waits on
// has to be produced by the test: the RabbitMQ Cluster Operator publishing its
// default user, the MariaDB operator turning the eight Database/User/Grant CRs
// of the two schemas ready, the Job controller completing the migration, and the
// Deployment controller counting ready pods for five workloads. Between those
// writes the pipeline advances on its own wait intervals (RequeueDatabaseWait is
// 30s), so a budget sized to the happy path alone turns an ordinary requeue into
// a flake.
package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/nova/internal/computeapi/computeapitest"
	"github.com/c5c3/cobaltcore/operators/nova/internal/testutil"
)

// Test timeout constants for CI tuning.
const (
	// eventuallyTimeout is the default polling timeout for Eventually assertions.
	eventuallyTimeout = 30 * time.Second
	// eventuallyLongTimeout covers the states the pipeline reaches only after
	// waiting out one of its own requeue intervals, RequeueDatabaseWait above all.
	eventuallyLongTimeout = 2 * RequeueDatabaseWait
	// pollInterval is the polling interval for Eventually assertions.
	pollInterval = 500 * time.Millisecond
)

// The object names and fixture values the integration suites share.
const (
	integrationNovaName     = "nova"
	integrationMariaDBName  = "mariadb"
	integrationRabbitmqName = "rabbitmq"

	// #nosec G101 -- Secret object names, not credentials.
	integrationRabbitmqUserSecret = "rabbitmq-default-user"
	// #nosec G101 -- Secret object names, not credentials.
	integrationAPIDBSecretName = "nova-api-db"
	// #nosec G101 -- Secret object names, not credentials.
	integrationCellDBSecretName = "nova-db"
	// #nosec G101 -- Secret object names, not credentials.
	integrationServiceUserSecretName = "nova-service-user"
	// #nosec G101 -- Secret object names, not credentials.
	integrationSharedSecretName = "nova-metadata-secret"

	// integrationBrokerPort is the port the default-user Secret publishes, and
	// therefore the port the two RPC workloads probe their bus on. It is the TLS
	// AMQP port rather than the 5672 default, so an environment built from the
	// fallback instead of from the transport URL is visible in the assertion.
	integrationBrokerPort = "5671"

	// The two schemas of one Nova. They have to differ (the CEL rule on
	// NovaSpec), and cell0 is derived from the cell schema by appending "_cell0",
	// which is the convention nova-manage maps it by.
	integrationAPISchema   = "nova_api"
	integrationCellSchema  = "nova"
	integrationCell0Schema = integrationCellSchema + "_cell0"

	// integrationImageRepository is the repository every workload and every Job
	// of these suites runs; the tag is the release under test.
	integrationImageRepository = "ghcr.io/c5c3/nova"
	// integrationInitialRelease is the release the fixtures install, and
	// integrationTargetRelease the one the upgrade suite converges to.
	integrationInitialRelease = "2025.2"
	integrationTargetRelease  = "2026.1"

	// The cell map the db-sync Job reports through its pod's termination
	// message. cell0 carries the all-zero UUID nova assigns the holding pen; the
	// real cell carries the one nova generated when it was mapped, which is the
	// only place that value can be read back from.
	integrationCell0UUID = "00000000-0000-0000-0000-000000000000"
	integrationCell1UUID = "2683878f-66d5-4512-bac3-9d70555bdd23"
)

// --- Shared helpers ---

// registerNovaWebhooks wires the webhook handlers of both kinds onto mgr. The
// webhook manifests envtest installs carry the Nova and NovaCompute kinds with
// failurePolicy=Fail, so an unserved handler would fail admission.
//
// mgr.GetAPIReader() mirrors main.go: admission lookups read the API server
// directly, never a stale informer cache.
func registerNovaWebhooks(mgr ctrl.Manager) error {
	if err := (&novav1alpha1.NovaWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
		return err
	}
	return (&novav1alpha1.NovaComputeWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
}

// registerNovaController wires the reconciler onto mgr through the production
// watch chain, with resolver as the target-cluster resolver.
//
// setupWithOptions is the chain SetupWithManager applies, so the legs, the field
// index and the Gateway API RESTMapper probe are the production ones rather than
// a hand-built copy that drifts the moment a leg is added. The only difference is
// SkipNameValidation: controller-runtime validates controller names against a
// process-global set, and this test binary starts one manager per test function.
// A skipped registration never claims the name, so nothing here can hide a
// duplicate registration in the operator binary.
//
// The health-check stub is what keeps the probe from firing slow HTTP GETs at a
// Service DNS name nothing answers; envtest runs no kubelet.
func registerNovaController(mgr ctrl.Manager, mcMgr mcmanager.Manager,
	resolver commonmulticluster.ClusterResolver,
) error {
	opts := bootstrap.TypedControllerOptions[mcreconcile.Request](1)
	opts.SkipNameValidation = ptr.To(true)

	nova := &NovaReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Recorder:   mgr.GetEventRecorderFor("nova-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
		HTTPClient: &stubDoer{status: http.StatusOK},
		Resolver:   resolver,
	}
	return nova.setupWithOptions(mcMgr, opts)
}

// registerNovaComputeController wires the NovaCompute reconciler onto mgr
// through its production watch chain, with resolver as the target-cluster
// resolver and api standing in for Keystone and the Nova API. Nodes and pods
// are read through the manager's API reader, as in main.go.
func registerNovaComputeController(mgr ctrl.Manager, mcMgr mcmanager.Manager,
	resolver commonmulticluster.ClusterResolver, api *computeapitest.Fake,
) error {
	opts := bootstrap.TypedControllerOptions[mcreconcile.Request](1)
	opts.SkipNameValidation = ptr.To(true)

	pool := &NovaComputeReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Recorder:   mgr.GetEventRecorderFor("novacompute-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
		APIReader:  mgr.GetAPIReader(),
		Resolver:   resolver,
		HTTPClient: api,
	}
	return pool.setupWithOptions(mcMgr, opts)
}

// setupEnvTestWithController wraps testutil.SetupNovaEnvTestWithController with
// the v1alpha1 scheme and both registration callbacks. A nil provider engages no
// target cluster and a nil Resolver keeps every child on the management cluster,
// which is the single-cluster default path.
//
// gatewayAPIAvailable is not set by hand: the fake HTTPRoute CRD the helper
// installs is what the setup-time RESTMapper probe answers from, so the HTTPRoute
// watch and the three route steps run exactly as they do against a cluster that
// has Gateway API.
func setupEnvTestWithController(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupNovaEnvTestWithController(t,
		novav1alpha1.AddToScheme,
		registerNovaWebhooks,
		func(mgr ctrl.Manager) error {
			mcMgr, err := mcmanager.WithMultiCluster(mgr, nil)
			if err != nil {
				return err
			}
			return registerNovaController(mgr, mcMgr, nil)
		},
	)
}

// createTestNamespace creates a uniquely named namespace per test.
func createTestNamespace(t testing.TB, ctx context.Context, c client.Client) string {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nova-it-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")
	return ns.Name
}

// eventuallyExists polls c until the object at key exists, decoding it into obj
// so the caller can read what the operator applied.
func eventuallyExists(
	t testing.TB,
	ctx context.Context,
	c client.Client,
	key client.ObjectKey,
	obj client.Object,
	what string,
	timeout time.Duration,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	g.Eventually(func() error {
		return c.Get(ctx, key, obj)
	}, timeout, pollInterval).Should(Succeed(), "%s %s should exist", what, key)
}

// waitForNovaCondition polls the Nova CR until the named condition reaches the
// expected status. Returns the condition.
func waitForNovaCondition(t testing.TB, ctx context.Context, c client.Client, key types.NamespacedName,
	condType string, expected metav1.ConditionStatus, timeout time.Duration,
) *metav1.Condition {
	t.Helper()
	g := NewGomegaWithT(t)

	var cond *metav1.Condition
	g.Eventually(func() metav1.ConditionStatus {
		var cr novav1alpha1.Nova
		if err := c.Get(ctx, key, &cr); err != nil {
			return ""
		}
		cond = meta.FindStatusCondition(cr.Status.Conditions, condType)
		if cond == nil {
			return ""
		}
		return cond.Status
	}, timeout, pollInterval).Should(Equal(expected),
		"Nova condition %s should reach %s", condType, expected)
	return cond
}

// waitForUpgradePhase polls the Nova CR until status.upgradePhase reaches the
// expected phase.
func waitForUpgradePhase(t testing.TB, ctx context.Context, c client.Client, key types.NamespacedName,
	expected commonv1.UpgradePhase,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	g.Eventually(func() commonv1.UpgradePhase {
		cur := &novav1alpha1.Nova{}
		if err := c.Get(ctx, key, cur); err != nil {
			return ""
		}
		return cur.Status.UpgradePhase
	}, eventuallyLongTimeout, pollInterval).Should(Equal(expected),
		"upgradePhase should transition to %s", expected)
}

// integrationNovaCR returns the Nova these suites drive: a managed database for
// both schemas, a managed cache and a managed bus, and no gateway anywhere, so
// the three route conditions resolve through their not-required paths.
//
// Every Deployment block spells out its replica count. The shared DeploymentSpec
// schema default of three is applied by the API server as soon as a deployment
// object is present at all, which a typed Go client always serialises, so the
// one-replica webhook defaults never reach the four non-API blocks and each
// workload would otherwise wait for three ready pods.
func integrationNovaCR(name, ns string, targetRef *commonv1.TargetClusterRefSpec) *novav1alpha1.Nova {
	return &novav1alpha1.Nova{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: novav1alpha1.NovaSpec{
			OpenStackRelease: integrationInitialRelease,
			Image: commonv1.ImageSpec{
				Repository: integrationImageRepository,
				Tag:        integrationInitialRelease,
			},
			APIDatabase: commonv1.DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: integrationMariaDBName},
				Database:   integrationAPISchema,
				SecretRef:  commonv1.SecretRefSpec{Name: integrationAPIDBSecretName},
			},
			Database: commonv1.DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: integrationMariaDBName},
				Database:   integrationCellSchema,
				SecretRef:  commonv1.SecretRefSpec{Name: integrationCellDBSecretName},
			},
			Cache: commonv1.CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "memcached"},
				Backend:    commonv1.DefaultCacheBackend,
			},
			Messaging: commonv1.MessagingSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: integrationRabbitmqName},
			},
			API:       novav1alpha1.NovaAPISpec{Deployment: novav1alpha1.DeploymentSpec{Replicas: 1}},
			Scheduler: novav1alpha1.NovaSchedulerSpec{Deployment: novav1alpha1.DeploymentSpec{Replicas: 1}},
			Conductor: novav1alpha1.NovaConductorSpec{Deployment: novav1alpha1.DeploymentSpec{Replicas: 1}},
			Metadata: novav1alpha1.NovaMetadataSpec{
				Deployment:      novav1alpha1.DeploymentSpec{Replicas: 1},
				SharedSecretRef: commonv1.SecretRefSpec{Name: integrationSharedSecretName},
			},
			ConsoleProxy: novav1alpha1.NovaConsoleProxySpec{
				Deployment: &novav1alpha1.DeploymentSpec{Replicas: 1},
			},
			KeystoneEndpoint: "http://keystone.openstack.svc.cluster.local:5000/v3",
			ServiceUser: novav1alpha1.ServiceUserSpec{
				SecretRef: commonv1.SecretRefSpec{Name: integrationServiceUserSecretName},
			},
			TargetClusterRef: targetRef,
		},
	}
}

// createNovaPrerequisites materialises everything the Nova pipeline reads but
// does not create: the secret store its credential gate checks, the four
// ESO-synced credential Secrets, the MariaDB cluster both schemas are
// provisioned in, and the broker with the default user it publishes. They live
// on the cluster the children do, which is the target cluster for a placed CR.
func createNovaPrerequisites(t testing.TB, ctx context.Context, c client.Client, ns string) {
	t.Helper()
	g := NewGomegaWithT(t)

	ensureReadyClusterSecretStore(t, ctx, c)

	for _, secret := range []struct {
		name string
		data map[string][]byte
	}{
		{integrationAPIDBSecretName, map[string][]byte{"username": []byte("nova-api"), "password": []byte("api-db-pw")}},
		{integrationCellDBSecretName, map[string][]byte{"username": []byte("nova"), "password": []byte("cell-db-pw")}},
		{integrationServiceUserSecretName, map[string][]byte{"password": []byte("svc-pw")}},
		{integrationSharedSecretName, map[string][]byte{novav1alpha1.DefaultSharedSecretKey: []byte("metadata-pw")}},
	} {
		g.Expect(c.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secret.name, Namespace: ns},
			Data:       secret.data,
		})).To(Succeed(), "create the %s Secret", secret.name)
	}

	mariadbKey := client.ObjectKey{Namespace: ns, Name: integrationMariaDBName}
	g.Expect(c.Create(ctx, &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: mariadbKey.Name, Namespace: mariadbKey.Namespace},
	})).To(Succeed(), "create the MariaDB cluster CR")
	g.Expect(simulators.SimulateMariaDBReady(ctx, c, mariadbKey, 1)).
		To(Succeed(), "mark the MariaDB cluster ready")

	// The broker publishes its credentials and its endpoint in one Secret, which
	// is what the managed messaging mode assembles the transport URL from.
	g.Expect(c.Create(ctx, rabbitmqCluster(integrationRabbitmqName, ns))).
		To(Succeed(), "create the RabbitmqCluster")
	g.Expect(c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: integrationRabbitmqUserSecret, Namespace: ns},
		Data: map[string][]byte{
			"username": []byte("default_user_abc"),
			"password": []byte("s3cr3t"),
			"host":     []byte(integrationRabbitmqName + "." + ns + ".svc"),
			"port":     []byte(integrationBrokerPort),
		},
	})).To(Succeed(), "create the default-user Secret")
	g.Expect(simulators.SimulateRabbitmqClusterReady(ctx, c, client.ObjectKey{
		Namespace: ns, Name: integrationRabbitmqName,
	}, integrationRabbitmqUserSecret)).To(Succeed(), "publish the RabbitmqCluster's default user")
}

// ensureReadyClusterSecretStore creates the cluster-scoped store the credential
// gate checks and marks it Ready. It tolerates one that already exists: the store
// is cluster-scoped, so a second namespace on the same cluster finds the object
// the first call created.
func ensureReadyClusterSecretStore(t testing.TB, ctx context.Context, c client.Client) {
	t.Helper()
	g := NewGomegaWithT(t)

	store := &esov1.ClusterSecretStore{ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName}}
	err := c.Create(ctx, store)
	if apierrors.IsAlreadyExists(err) {
		return
	}
	g.Expect(err).NotTo(HaveOccurred(), "create the ClusterSecretStore")

	store.Status = esov1.SecretStoreStatus{
		Conditions: []esov1.SecretStoreStatusCondition{
			{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
		},
	}
	g.Expect(c.Status().Update(ctx, store)).To(Succeed(), "mark the ClusterSecretStore Ready")
}

// novaDatabaseBlock is one set of MariaDB CRs a Nova provisions: where they
// live, the SQL schema they carry, and whether the set includes a User. cell0
// travels as an additional schema of the cell block, so it has a Database and a
// Grant but no user of its own.
type novaDatabaseBlock struct {
	key    client.ObjectKey
	user   bool
	schema string
}

// novaDatabaseBlocks returns the eight MariaDB CRs one Nova provisions, in the
// order the flow gates on them: the nova_api block first, then the cell block
// with cell0 behind it.
func novaDatabaseBlocks(name, ns string) []novaDatabaseBlock {
	return []novaDatabaseBlock{
		{key: client.ObjectKey{Namespace: ns, Name: name + "-api"}, user: true, schema: integrationAPISchema},
		{key: client.ObjectKey{Namespace: ns, Name: name}, user: true, schema: integrationCellSchema},
		{
			key: client.ObjectKey{
				Namespace: ns,
				Name:      database.AdditionalResourceName(name, integrationCell0Schema),
			},
			schema: integrationCell0Schema,
		},
	}
}

// driveDatabaseCRs plays the MariaDB operator for one Nova: the eight CRs of the
// two schemas turn ready in the order the flow gates on them. Each wait is the
// gate the pipeline actually has, so a helper that only wrote the simulated state
// would race the reconciler rather than drive it.
//
// The order below is the order the objects appear in, and it is not the order
// the eight are listed in. Within a block the flow ensures the primary schema,
// then every additional schema, then the user with its primary Grant, then the
// additional Grants: a Grant can only be issued once both the schema and the SQL
// user exist. Marking the cell user ready before cell0's Database would deadlock,
// because the flow does not reach the user until cell0's schema reports ready.
func driveDatabaseCRs(t testing.TB, ctx context.Context, childClient client.Client, name, ns string) {
	t.Helper()

	apiKey := client.ObjectKey{Namespace: ns, Name: name + "-api"}
	cellKey := client.ObjectKey{Namespace: ns, Name: name}
	cell0Key := client.ObjectKey{
		Namespace: ns,
		Name:      database.AdditionalResourceName(name, integrationCell0Schema),
	}

	// The nova_api block: the global half of nova's state, on a user of its own.
	markDatabaseReady(t, ctx, childClient, apiKey, integrationAPISchema)
	markUserReady(t, ctx, childClient, apiKey)
	markGrantReady(t, ctx, childClient, apiKey)

	// The cell block, with cell0 as an additional schema on the same user.
	markDatabaseReady(t, ctx, childClient, cellKey, integrationCellSchema)
	markDatabaseReady(t, ctx, childClient, cell0Key, integrationCell0Schema)
	markUserReady(t, ctx, childClient, cellKey)
	markGrantReady(t, ctx, childClient, cellKey)
	markGrantReady(t, ctx, childClient, cell0Key)
}

// markDatabaseReady waits for the MariaDB Database at key, checks it provisions
// the expected SQL schema, and marks it ready.
func markDatabaseReady(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey, schema string) {
	t.Helper()
	g := NewGomegaWithT(t)

	db := &mariadbv1alpha1.Database{}
	eventuallyExists(t, ctx, c, key, db, "MariaDB Database", eventuallyLongTimeout)
	g.Expect(db.Spec.Name).To(Equal(schema),
		"MariaDB Database %s should provision schema %s", key, schema)
	g.Expect(simulators.SimulateDatabaseReady(ctx, c, key)).To(Succeed(), "mark the Database %s ready", key)
}

// markUserReady waits for the MariaDB User at key and marks it ready.
func markUserReady(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	eventuallyExists(t, ctx, c, key, &mariadbv1alpha1.User{}, "MariaDB User", eventuallyLongTimeout)
	g.Expect(simulators.SimulateUserReady(ctx, c, key)).To(Succeed(), "mark the User %s ready", key)
}

// markGrantReady waits for the MariaDB Grant at key and marks it ready.
func markGrantReady(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	eventuallyExists(t, ctx, c, key, &mariadbv1alpha1.Grant{}, "MariaDB Grant", eventuallyLongTimeout)
	g.Expect(simulators.SimulateGrantReady(ctx, c, key)).To(Succeed(), "mark the Grant %s ready", key)
}

// completeDBSync plays the Job controller for the migration Job: the pod it
// labels reports the cell map through its termination message, and only then does
// the Job complete.
//
// The order is what the report depends on. reportCells reads the message once per
// Job UID, on the pass that first observes the Job terminal, so a pod written
// afterwards would never be read.
func completeDBSync(t testing.TB, ctx context.Context, childClient client.Client, name, ns string) {
	t.Helper()
	g := NewGomegaWithT(t)

	dbSyncKey := client.ObjectKey{Namespace: ns, Name: name + "-" + dbSyncJobSuffix}
	eventuallyExists(t, ctx, childClient, dbSyncKey, &batchv1.Job{}, "db-sync Job", eventuallyLongTimeout)

	writeCellsReport(t, ctx, childClient, ns, dbSyncKey.Name)
	g.Expect(simulators.SimulateJobComplete(ctx, childClient, dbSyncKey)).
		To(Succeed(), "complete the db-sync Job")
}

// writeCellsReport creates the pod a finished db-sync Job left behind and writes
// the cell map into its termination message, the file the awk stage of the sync
// script redirects its "<name>=<uuid>" lines into. envtest runs no Job
// controller, so the pod and its status are the test's to produce.
func writeCellsReport(t testing.TB, ctx context.Context, c client.Client, ns, jobName string) {
	t.Helper()
	g := NewGomegaWithT(t)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-pod",
			Namespace: ns,
			Labels:    map[string]string{"batch.kubernetes.io/job-name": jobName},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:  "db-sync",
				Image: releaseTag(integrationInitialRelease),
			}},
		},
	}
	g.Expect(c.Create(ctx, pod)).To(Succeed(), "create the db-sync Job's pod")

	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "db-sync",
		Image: releaseTag(integrationInitialRelease),
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 0,
			Message: "cell0=" + integrationCell0UUID + "\n" +
				"cell1=" + integrationCell1UUID,
		}},
	}}
	g.Expect(c.Status().Update(ctx, pod)).To(Succeed(), "write the pod's termination message")
}

// novaWorkloadKeys returns the five Deployments one Nova projects, in the order
// the pipeline creates them.
func novaWorkloadKeys(name, ns string) []client.ObjectKey {
	return []client.ObjectKey{
		{Namespace: ns, Name: name + "-" + componentConductor},
		{Namespace: ns, Name: name + "-" + componentScheduler},
		{Namespace: ns, Name: name + "-" + componentMetadata},
		{Namespace: ns, Name: name + "-" + componentConsoleProxy},
		{Namespace: ns, Name: name},
	}
}

// markDeploymentReady waits for the Deployment at key and writes the status a
// running Deployment controller would: every replica updated, ready and counted,
// with observedGeneration caught up. That is what both readiness gates read, the
// surge-tolerant deployment.IsDeploymentReady and the stricter
// novaDeploymentRolledOut the RollingUpdate phase waits on.
func markDeploymentReady(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	deploy := &appsv1.Deployment{}
	eventuallyExists(t, ctx, c, key, deploy, "Deployment", eventuallyLongTimeout)
	g.Expect(simulators.SimulateDeploymentReady(ctx, c, key, ptr.Deref(deploy.Spec.Replicas, 1))).
		To(Succeed(), "mark Deployment %s available", key)
}

// mountedConfigMapName returns the name of the ConfigMap the workload's config
// volume mounts, which is the rendered nova.conf carrier.
func mountedConfigMapName(spec *corev1.PodSpec) string {
	for i := range spec.Volumes {
		v := &spec.Volumes[i]
		if v.Name == configVolumeName && v.ConfigMap != nil {
			return v.ConfigMap.Name
		}
	}
	return ""
}

// containerEnv returns the environment of the pod's first container, keyed by
// variable name, so an assertion can name the variable rather than its index.
func containerEnv(spec *corev1.PodSpec) map[string]corev1.EnvVar {
	env := map[string]corev1.EnvVar{}
	for _, v := range spec.Containers[0].Env {
		env[v.Name] = v
	}
	return env
}

// deploymentImage returns the image the Deployment's first container runs, or ""
// when the Deployment does not exist.
func deploymentImage(ctx context.Context, c client.Client, key client.ObjectKey) string {
	deploy := &appsv1.Deployment{}
	if err := c.Get(ctx, key, deploy); err != nil {
		return ""
	}
	return deploy.Spec.Template.Spec.Containers[0].Image
}

// releaseTag returns the image reference of one release, which is what every
// workload and every migration Job of that release runs.
func releaseTag(release string) string {
	return integrationImageRepository + ":" + release
}

// expectControlledByNova asserts obj carries a controller owner reference naming
// the Nova, which is what hands it to the garbage collection cascade when that CR
// is deleted.
func expectControlledByNova(t testing.TB, obj client.Object, novaName, what string) {
	t.Helper()
	g := NewGomegaWithT(t)

	owner := metav1.GetControllerOf(obj)
	g.Expect(owner).NotTo(BeNil(), "%s should carry a controller owner reference", what)
	g.Expect(owner.Kind).To(Equal("Nova"), "%s should be controlled by a Nova", what)
	g.Expect(owner.Name).To(Equal(novaName), "%s should be controlled by %s", what, novaName)
}

// --- Tests ---

// TestIntegrationNova_Lifecycle walks one Nova from an empty namespace through
// its two provisioned schemas, the cell map its migration Job reports, the five
// projected workloads and the compute contract, to a disabled console proxy and
// its own deletion, against a live API server. The subtests are ordered and each
// builds on the state the previous left behind: the fixtures are the same objects
// throughout, which is what makes the transitions observable rather than eight
// independent snapshots.
func TestIntegrationNova_Lifecycle(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestWithController(t)
	ns := createTestNamespace(t, ctx, c)
	createNovaPrerequisites(t, ctx, c, ns)

	novaKey := types.NamespacedName{Name: integrationNovaName, Namespace: ns}
	apiKey := client.ObjectKey{Namespace: ns, Name: integrationNovaName}
	consoleKey := client.ObjectKey{Namespace: ns, Name: integrationNovaName + "-" + componentConsoleProxy}

	t.Run("both schemas are provisioned with cell0 on the cell block", func(t *testing.T) {
		g := NewGomegaWithT(t)

		g.Expect(c.Create(ctx, integrationNovaCR(integrationNovaName, ns, nil))).
			To(Succeed(), "create the Nova CR")

		waitForNovaCondition(t, ctx, c, novaKey, "SecretsReady", metav1.ConditionTrue, eventuallyTimeout)
		driveDatabaseCRs(t, ctx, c, integrationNovaName, ns)

		// cell0 is provisioned as an additional schema of the cell block, so its
		// Grant names the cell block's user: a separate user would leave the
		// conductor unable to read the instances that never reached a cell.
		cell0 := &mariadbv1alpha1.Grant{}
		g.Expect(c.Get(ctx, client.ObjectKey{
			Namespace: ns,
			Name:      database.AdditionalResourceName(integrationNovaName, integrationCell0Schema),
		}, cell0)).To(Succeed())
		g.Expect(cell0.Spec.Username).To(Equal(integrationNovaName),
			"cell0 must be granted on the cell block's own user")
	})

	t.Run("the db-sync Job migrates both schemas and maps the cells", func(t *testing.T) {
		g := NewGomegaWithT(t)

		job := &batchv1.Job{}
		eventuallyExists(t, ctx, c, client.ObjectKey{
			Namespace: ns, Name: integrationNovaName + "-" + dbSyncJobSuffix,
		}, job, "db-sync Job", eventuallyLongTimeout)

		// One Job migrates both schemas, so it carries both connection URLs.
		env := containerEnv(&job.Spec.Template.Spec)
		g.Expect(env).To(HaveKey("OS_API_DATABASE__CONNECTION"))
		g.Expect(env).To(HaveKey("OS_DATABASE__CONNECTION"))

		command := strings.Join(job.Spec.Template.Spec.Containers[0].Command, " ")
		g.Expect(command).To(ContainSubstring("cell_v2 map_cell0"),
			"cell0 must be mapped before the cell schema is migrated")
		g.Expect(command).To(ContainSubstring("create_cell --name cell1"),
			"the single real cell must be created under the name the compute contract publishes")
	})

	t.Run("the completed Job publishes the cell map it recorded", func(t *testing.T) {
		g := NewGomegaWithT(t)

		completeDBSync(t, ctx, c, integrationNovaName, ns)
		waitForNovaCondition(t, ctx, c, novaKey, "DatabaseReady", metav1.ConditionTrue, eventuallyLongTimeout)

		// The UUIDs are generated at map time and read back out of the Job's
		// termination log; nothing else in the deployment carries them.
		g.Eventually(func(ig Gomega) {
			cur := &novav1alpha1.Nova{}
			ig.Expect(c.Get(ctx, novaKey, cur)).To(Succeed())
			ig.Expect(cur.Status.Cells).To(Equal([]novav1alpha1.NovaCellStatus{
				{Name: "cell0", UUID: integrationCell0UUID},
				{Name: "cell1", UUID: integrationCell1UUID},
			}), "status.cells should report the cell map the db-sync Job recorded")
			ig.Expect(cur.Status.InstalledRelease).To(Equal(integrationInitialRelease),
				"installedRelease should be promoted after the db-sync")
		}, eventuallyLongTimeout, pollInterval).Should(Succeed())
	})

	t.Run("the five workloads carry the config, env and mounts of their role", func(t *testing.T) {
		g := NewGomegaWithT(t)

		for _, role := range []struct {
			key        client.ObjectKey
			component  string
			probePath  string
			probePort  int32
			overlayDir string
			wantEnv    []string
			absentEnv  []string
		}{
			{
				key:       apiKey,
				component: naming.ComponentAPI,
				probePath: "/",
				probePort: novaAPIPort,
				wantEnv:   []string{novaConfigFilesEnvVarName, "OS_API_DATABASE__CONNECTION"},
				absentEnv: []string{metadataSharedSecretEnvVarName, amqpPortEnvName},
			},
			{
				key:       client.ObjectKey{Namespace: ns, Name: integrationNovaName + "-" + componentMetadata},
				component: componentMetadata,
				probePath: "/",
				probePort: novaMetadataPort,
				wantEnv:   []string{novaConfigFilesEnvVarName, metadataSharedSecretEnvVarName},
				absentEnv: []string{amqpPortEnvName},
			},
			{
				key:        client.ObjectKey{Namespace: ns, Name: integrationNovaName + "-" + componentScheduler},
				component:  componentScheduler,
				overlayDir: roleOverlayDir(roleScheduler),
				wantEnv:    []string{amqpPortEnvName},
				absentEnv:  []string{novaConfigFilesEnvVarName, metadataSharedSecretEnvVarName},
			},
			{
				key:        client.ObjectKey{Namespace: ns, Name: integrationNovaName + "-" + componentConductor},
				component:  componentConductor,
				overlayDir: roleOverlayDir(roleConductor),
				wantEnv:    []string{amqpPortEnvName},
				absentEnv:  []string{novaConfigFilesEnvVarName, metadataSharedSecretEnvVarName},
			},
			{
				key:        consoleKey,
				component:  componentConsoleProxy,
				probePath:  consoleProxyVNCPath,
				probePort:  novaConsolePort,
				overlayDir: roleOverlayDir(roleConsoleProxy),
				// The proxy reads the console tokens out of the cell schema alone,
				// so it is the one role without a nova_api connection.
				absentEnv: []string{"OS_API_DATABASE__CONNECTION", novaConfigFilesEnvVarName},
			},
		} {
			deploy := &appsv1.Deployment{}
			eventuallyExists(t, ctx, c, role.key, deploy, role.component+" Deployment", eventuallyLongTimeout)

			g.Expect(deploy.Spec.Template.Labels).To(HaveKeyWithValue(naming.LabelKeyComponent, role.component),
				"%s pods must be labelled with their own component", role.key)
			g.Expect(deploy.Spec.Selector.MatchLabels).To(HaveKeyWithValue(naming.LabelKeyComponent, role.component),
				"a selector without the component key would adopt the pods of every other role")

			spec := &deploy.Spec.Template.Spec
			env := containerEnv(spec)
			for _, name := range role.wantEnv {
				g.Expect(env).To(HaveKey(name), "%s must carry %s", role.key, name)
			}
			for _, name := range role.absentEnv {
				g.Expect(env).NotTo(HaveKey(name), "%s must not carry %s", role.key, name)
			}
			if _, ok := env[amqpPortEnvName]; ok {
				g.Expect(env[amqpPortEnvName].Value).To(Equal(integrationBrokerPort),
					"the bus probe must check the port the transport URL names")
			}

			g.Expect(mountPaths(spec.Containers[0].VolumeMounts)).To(ContainElement(novaConfigDir),
				"every role reads the shared config directory")
			if role.overlayDir != "" {
				g.Expect(mountPaths(spec.Containers[0].VolumeMounts)).To(ContainElement(role.overlayDir),
					"a console-script role reads its own overlay from a directory of its own")
			}

			probe := spec.Containers[0].ReadinessProbe
			g.Expect(probe).NotTo(BeNil(), "%s must carry a readiness probe", role.key)
			if role.probePath == "" {
				g.Expect(probe.Exec).NotTo(BeNil(),
					"a role that serves no HTTP port is probed by the bus check")
				continue
			}
			g.Expect(probe.HTTPGet).NotTo(BeNil())
			g.Expect(probe.HTTPGet.Path).To(Equal(role.probePath))
			g.Expect(probe.HTTPGet.Port.IntValue()).To(Equal(int(role.probePort)))
		}

		// The three addresses this CR publishes, and the budget that protects the
		// only role with clients that do not retry.
		for _, svc := range []struct {
			key       client.ObjectKey
			component string
			port      int32
		}{
			{apiKey, naming.ComponentAPI, novaAPIPort},
			{client.ObjectKey{Namespace: ns, Name: integrationNovaName + "-" + componentMetadata}, componentMetadata, novaMetadataPort},
			{consoleKey, componentConsoleProxy, novaConsolePort},
		} {
			service := &corev1.Service{}
			eventuallyExists(t, ctx, c, svc.key, service, svc.component+" Service", eventuallyLongTimeout)
			g.Expect(service.Spec.Selector).To(HaveKeyWithValue(naming.LabelKeyComponent, svc.component))
			g.Expect(service.Spec.Ports[0].Port).To(Equal(svc.port))
		}

		// envtest runs no Deployment controller, so every rollout is completed
		// here. Only then does the pipeline reach the archive CronJob and the
		// parallel group behind the API step.
		for _, key := range novaWorkloadKeys(integrationNovaName, ns) {
			markDeploymentReady(t, ctx, c, key)
		}

		eventuallyExists(t, ctx, c, apiKey, &policyv1.PodDisruptionBudget{},
			"API PodDisruptionBudget", eventuallyLongTimeout)
		eventuallyExists(t, ctx, c, client.ObjectKey{
			Namespace: ns, Name: dbArchiveCronJobName(integrationNovaName),
		}, &batchv1.CronJob{}, "db-archive CronJob", eventuallyLongTimeout)
	})

	t.Run("the compute contract is published for the nodes this operator does not deploy", func(t *testing.T) {
		g := NewGomegaWithT(t)

		contract := &corev1.Secret{}
		eventuallyExists(t, ctx, c, client.ObjectKey{
			Namespace: ns, Name: integrationNovaName + "-" + componentComputeConfig,
		}, contract, "compute-config Secret", eventuallyLongTimeout)

		for _, key := range []string{
			computeConfigFragmentKey, transportURLKey, passwordKey, metadataSharedSecretKey, cellNameKey,
		} {
			g.Expect(contract.Data).To(HaveKey(key))
		}
		g.Expect(contract.Data).NotTo(HaveKey(caBundleKey),
			"a plaintext bus ships no CA bundle: the fragment names no ssl_ca_file to read it")
		g.Expect(string(contract.Data[cellNameKey])).To(Equal(computeCellName))

		published := waitForNovaCondition(t, ctx, c, novaKey, conditionTypeComputeConfigReady,
			metav1.ConditionTrue, eventuallyLongTimeout)
		g.Expect(published.Reason).To(Equal(conditionReasonComputeConfigPublished))

		cur := &novav1alpha1.Nova{}
		g.Expect(c.Get(ctx, novaKey, cur)).To(Succeed())
		g.Expect(cur.Status.ComputeConfigSecretRef).NotTo(BeNil(),
			"the handover point has to be named on the CR a compute cluster reads")
		g.Expect(cur.Status.ComputeConfigSecretRef.Name).To(Equal(contract.Name))
	})

	t.Run("the aggregate resolves once every sub-condition has reported", func(t *testing.T) {
		g := NewGomegaWithT(t)

		waitForNovaCondition(t, ctx, c, novaKey, "Ready", metav1.ConditionTrue, eventuallyLongTimeout)

		// The fixture exposes nothing through a gateway, so all three route
		// conditions resolve through their not-required path. They are part of the
		// aggregate, so a route that reported nothing would have held Ready back.
		for _, condType := range []string{
			conditionTypeHTTPRouteReady, conditionTypeMetadataHTTPRouteReady, conditionTypeConsoleHTTPRouteReady,
		} {
			cond := waitForNovaCondition(t, ctx, c, novaKey, condType, metav1.ConditionTrue, eventuallyTimeout)
			g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotRequired),
				"%s should report that no gateway asked for a route", condType)
		}

		cur := &novav1alpha1.Nova{}
		g.Expect(c.Get(ctx, novaKey, cur)).To(Succeed())
		g.Expect(cur.Status.Endpoint).To(Equal(internalNovaURL(cur)),
			"without a gateway the endpoint is the cluster-local Service address")
	})

	t.Run("disabling the console proxy removes its Deployment and Service", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// The block is a pointer so that a disabled proxy can leave it absent,
		// which is what the console rule on NovaSpec requires. The Get/Update loop
		// rides out the conflict a concurrent status write races in.
		g.Eventually(func() error {
			cur := &novav1alpha1.Nova{}
			if err := c.Get(ctx, novaKey, cur); err != nil {
				return err
			}
			cur.Spec.ConsoleProxy.Enabled = ptr.To(false)
			cur.Spec.ConsoleProxy.Deployment = nil
			return c.Update(ctx, cur)
		}, eventuallyTimeout, pollInterval).Should(Succeed(), "switch the console proxy off")

		// The switch rewrites [vnc] in the shared document, so the render produces
		// a new immutable ConfigMap, and the migration Job mounts that ConfigMap:
		// the shared database flow deletes the completed Job and re-runs it against
		// the new document before the pipeline reaches the console step at all.
		// envtest completes no Job, so the re-run is completed here.
		g.Eventually(func(ig Gomega) {
			job := &batchv1.Job{}
			ig.Expect(c.Get(ctx, client.ObjectKey{
				Namespace: ns, Name: integrationNovaName + "-" + dbSyncJobSuffix,
			}, job)).To(Succeed())
			ig.Expect(job.Status.Succeeded).To(BeZero(),
				"the db-sync Job should have been re-created for the re-rendered config")
			ig.Expect(simulators.SimulateJobComplete(ctx, c, client.ObjectKeyFromObject(job))).To(Succeed())
		}, eventuallyLongTimeout, pollInterval).Should(Succeed(), "complete the re-run db-sync Job")

		g.Eventually(func(ig Gomega) {
			ig.Expect(apierrors.IsNotFound(c.Get(ctx, consoleKey, &appsv1.Deployment{}))).To(BeTrue(),
				"the console proxy Deployment should be removed")
			ig.Expect(apierrors.IsNotFound(c.Get(ctx, consoleKey, &corev1.Service{}))).To(BeTrue(),
				"the console proxy Service should be removed")
		}, eventuallyLongTimeout, pollInterval).Should(Succeed())

		disabled := waitForNovaCondition(t, ctx, c, novaKey, "ConsoleProxyReady",
			metav1.ConditionTrue, eventuallyLongTimeout)
		g.Expect(disabled.Reason).To(Equal(conditionReasonConsoleProxyDisabled),
			"a console nobody asked for is a deliberate posture, not a failure")

		// The re-rendered config rolls the four surviving workloads, so their
		// rollouts are completed once more before the aggregate can resolve again.
		// It does resolve: the disabled proxy reports ready rather than dropping
		// out of the fleet.
		for _, key := range novaWorkloadKeys(integrationNovaName, ns) {
			if key == consoleKey {
				continue
			}
			markDeploymentReady(t, ctx, c, key)
		}
		waitForNovaCondition(t, ctx, c, novaKey, "Ready", metav1.ConditionTrue, eventuallyLongTimeout)
	})

	t.Run("deleting the Nova tears down both schemas and releases its children", func(t *testing.T) {
		g := NewGomegaWithT(t)

		api := &appsv1.Deployment{}
		g.Expect(c.Get(ctx, apiKey, api)).To(Succeed())
		configMapName := mountedConfigMapName(&api.Spec.Template.Spec)
		g.Expect(configMapName).To(HavePrefix(integrationNovaName+"-config-"),
			"the API pods must mount the content-hashed config ConfigMap")

		// A local Nova hands its children to the garbage collection cascade rather
		// than deleting them itself, so what has to hold before the deletion is
		// that every one of them names the CR as its controller. envtest runs no
		// garbage collector, so the cascade itself is asserted where the operator
		// performs it by hand: on a target cluster, in
		// TestIntegration_Multicluster_NovaTargetCluster.
		for _, child := range []struct {
			name string
			obj  client.Object
		}{
			{database.ConnectionSecretName(integrationNovaName + "-api"), &corev1.Secret{}},
			{database.ConnectionSecretName(integrationNovaName), &corev1.Secret{}},
			{messaging.TransportURLSecretName(integrationNovaName), &corev1.Secret{}},
			{integrationNovaName + "-" + componentComputeConfig, &corev1.Secret{}},
			{configMapName, &corev1.ConfigMap{}},
		} {
			g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: child.name}, child.obj)).To(Succeed())
			expectControlledByNova(t, child.obj, integrationNovaName, "the derived child "+child.name)
		}

		nova := &novav1alpha1.Nova{}
		g.Expect(c.Get(ctx, novaKey, nova)).To(Succeed())
		g.Expect(controllerutil.ContainsFinalizer(nova, novaFinalizer)).To(BeTrue(),
			"the Nova is held until the MariaDB CRs of both schemas have been issued a Delete")
		g.Expect(c.Delete(ctx, nova)).To(Succeed(), "delete the Nova CR")

		g.Eventually(func() bool {
			return apierrors.IsNotFound(c.Get(ctx, novaKey, &novav1alpha1.Nova{}))
		}, eventuallyLongTimeout, pollInterval).Should(BeTrue(),
			"the CR should leave etcd once the finalizer is released")

		// The eight MariaDB CRs are the children the operator deletes by name,
		// which is the whole reason the finalizer holds the CR one pass longer: the
		// teardown of both schemas has to be triggered before the owner-ref chain
		// disappears.
		g.Eventually(func(ig Gomega) {
			for _, block := range novaDatabaseBlocks(integrationNovaName, ns) {
				ig.Expect(apierrors.IsNotFound(c.Get(ctx, block.key, &mariadbv1alpha1.Database{}))).To(BeTrue(),
					"the MariaDB Database %s should have been deleted with its Nova", block.key)
				ig.Expect(apierrors.IsNotFound(c.Get(ctx, block.key, &mariadbv1alpha1.Grant{}))).To(BeTrue(),
					"the MariaDB Grant %s should have been deleted with its Nova", block.key)
				if block.user {
					ig.Expect(apierrors.IsNotFound(c.Get(ctx, block.key, &mariadbv1alpha1.User{}))).To(BeTrue(),
						"the MariaDB User %s should have been deleted with its Nova", block.key)
				}
			}
		}, eventuallyLongTimeout, pollInterval).Should(Succeed())
	})
}

// TestIntegrationNova_UpgradeCycle_ExpandMigrateContract drives a full release
// upgrade (2025.2 to 2026.1) end to end against envtest. It locks the two
// properties no unit test observes together:
//
//   - the Expanding, Migrating, RollingUpdate, Contracting phase walk, with each
//     phase Job carrying the target-release image and its own command;
//   - the sequenced rollout inside RollingUpdate: the conductor re-images first
//     and the scheduler, metadata, console proxy and API workloads follow one at
//     a time, each held behind the previous one's converged rollout, so the
//     contract phase cannot run against a process still on the old code.
//
// envtest runs no Job or Deployment controller, so every Job completion and every
// rollout is written by the test; the phase machine cannot advance alone.
func TestIntegrationNova_UpgradeCycle_ExpandMigrateContract(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)
	ns := createTestNamespace(t, ctx, c)
	createNovaPrerequisites(t, ctx, c, ns)

	novaKey := types.NamespacedName{Name: integrationNovaName, Namespace: ns}
	workloads := novaWorkloadKeys(integrationNovaName, ns)

	g.Expect(c.Create(ctx, integrationNovaCR(integrationNovaName, ns, nil))).To(Succeed(), "create the Nova CR")

	// Drive the 2025.2 install to Ready, which is the state an upgrade starts
	// from: five workloads on the old image and two schemas at the old release.
	driveDatabaseCRs(t, ctx, c, integrationNovaName, ns)
	completeDBSync(t, ctx, c, integrationNovaName, ns)
	for _, key := range workloads {
		markDeploymentReady(t, ctx, c, key)
	}
	waitForNovaCondition(t, ctx, c, novaKey, "Ready", metav1.ConditionTrue, eventuallyLongTimeout)

	installed := &novav1alpha1.Nova{}
	g.Expect(c.Get(ctx, novaKey, installed)).To(Succeed())
	g.Expect(installed.Status.InstalledRelease).To(Equal(integrationInitialRelease))
	g.Expect(string(installed.Status.UpgradePhase)).To(BeEmpty(),
		"no upgrade should be in flight after a fresh install")

	// Trigger the upgrade: bump spec.openStackRelease and spec.image.tag in
	// lockstep, which is the contract checkImageReleaseMismatch enforces. The
	// Get/Update loop rides out the conflict a concurrent status write races in.
	g.Eventually(func() error {
		cur := &novav1alpha1.Nova{}
		if err := c.Get(ctx, novaKey, cur); err != nil {
			return err
		}
		cur.Spec.OpenStackRelease = integrationTargetRelease
		cur.Spec.Image.Tag = integrationTargetRelease
		return c.Update(ctx, cur)
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "bump the Nova to release 2026.1")

	// Phase 1: Expanding. Nova's expand runs the readiness check and then both
	// schemas' migrations: "nova-manage db sync" is additive, so the old release
	// keeps running against the widened schema.
	waitForUpgradePhase(t, ctx, c, novaKey, commonv1.UpgradePhaseExpanding)
	expandKey := client.ObjectKey{Namespace: ns, Name: integrationNovaName + "-" + upgradeExpandJobSuffix}
	expandJob := &batchv1.Job{}
	eventuallyExists(t, ctx, c, expandKey, expandJob, "db-expand Job", eventuallyLongTimeout)
	g.Expect(expandJob.Spec.Template.Spec.Containers[0].Image).To(Equal(releaseTag(integrationTargetRelease)),
		"the expand Job runs the target release's binary")
	g.Expect(strings.Join(expandJob.Spec.Template.Spec.Containers[0].Command, " ")).
		To(ContainSubstring("nova-status"), "the expand phase runs the upgrade check ahead of the migrations")
	g.Expect(simulators.SimulateJobComplete(ctx, c, expandKey)).To(Succeed(), "complete the db-expand Job")

	upgrading := &novav1alpha1.Nova{}
	g.Expect(c.Get(ctx, novaKey, upgrading)).To(Succeed())
	g.Expect(upgrading.Status.TargetRelease).To(Equal(integrationTargetRelease),
		"targetRelease should record the in-flight upgrade")

	// Phase 2: Migrating. Nova has no migrate verb of its own, so the phase is a
	// read that proves the new code can address the nova_api schema expand just
	// migrated.
	waitForUpgradePhase(t, ctx, c, novaKey, commonv1.UpgradePhaseMigrating)
	migrateKey := client.ObjectKey{Namespace: ns, Name: integrationNovaName + "-" + upgradeMigrateJobSuffix}
	migrateJob := &batchv1.Job{}
	eventuallyExists(t, ctx, c, migrateKey, migrateJob, "db-migrate Job", eventuallyLongTimeout)
	g.Expect(migrateJob.Spec.Template.Spec.Containers[0].Command).To(Equal(upgradeMigrateCommand))
	g.Expect(simulators.SimulateJobComplete(ctx, c, migrateKey)).To(Succeed(), "complete the db-migrate Job")

	// Phase 3: RollingUpdate. The five workloads re-image one at a time, in
	// pipeline order, each held behind the previous one's converged rollout.
	waitForUpgradePhase(t, ctx, c, novaKey, commonv1.UpgradePhaseRollingUpdate)

	for i, key := range workloads {
		g.Eventually(func() string {
			return deploymentImage(ctx, c, key)
		}, eventuallyLongTimeout, pollInterval).Should(Equal(releaseTag(integrationTargetRelease)),
			"%s should be re-imaged once every workload ahead of it has rolled", key)

		for _, waiting := range workloads[i+1:] {
			g.Expect(deploymentImage(ctx, c, waiting)).To(Equal(releaseTag(integrationInitialRelease)),
				"%s must wait behind %s", waiting, key)
		}

		// The phase must not advance while a re-imaged workload has not converged;
		// the contract phase would otherwise run the data migrations against
		// processes that have no code for them.
		g.Consistently(func() commonv1.UpgradePhase {
			cur := &novav1alpha1.Nova{}
			if err := c.Get(ctx, novaKey, cur); err != nil {
				return ""
			}
			return cur.Status.UpgradePhase
		}, 2*time.Second, pollInterval).Should(Equal(commonv1.UpgradePhaseRollingUpdate),
			"upgradePhase must stay RollingUpdate until every role has rolled")

		markDeploymentReady(t, ctx, c, key)
	}

	// Phase 4: Contracting. The contract phase runs the online data migrations
	// that backfill the rows the new schema needs, which the completed rollout of
	// every role is what makes safe.
	waitForUpgradePhase(t, ctx, c, novaKey, commonv1.UpgradePhaseContracting)
	contractKey := client.ObjectKey{Namespace: ns, Name: integrationNovaName + "-" + upgradeContractJobSuffix}
	contractJob := &batchv1.Job{}
	eventuallyExists(t, ctx, c, contractKey, contractJob, "db-contract Job", eventuallyLongTimeout)
	g.Expect(strings.Join(contractJob.Spec.Template.Spec.Containers[0].Command, " ")).
		To(ContainSubstring("online_data_migrations"))
	g.Expect(simulators.SimulateJobComplete(ctx, c, contractKey)).To(Succeed(), "complete the db-contract Job")

	g.Eventually(func(ig Gomega) {
		cur := &novav1alpha1.Nova{}
		ig.Expect(c.Get(ctx, novaKey, cur)).To(Succeed())
		ig.Expect(cur.Status.InstalledRelease).To(Equal(integrationTargetRelease),
			"installedRelease should advance once both schemas are contracted")
		ig.Expect(string(cur.Status.UpgradePhase)).To(BeEmpty(),
			"upgradePhase should be cleared once the upgrade completes")
		ig.Expect(cur.Status.TargetRelease).To(BeEmpty(),
			"targetRelease should be cleared once the upgrade completes")
	}, eventuallyLongTimeout, pollInterval).Should(Succeed())
}

// TestIntegrationNovaCompute_ReachesReady drives one node pool to Ready against
// a real API server. No Nova controller runs here: the Nova's status is written
// by hand, the way the Nova reconciler would publish it, so the suite exercises
// the pool alone. envtest runs no DaemonSet controller, so the rollout is
// completed by the test, and the fake Nova already knows the node's service.
func TestIntegrationNovaCompute_ReachesReady(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	api := computeapitest.New()
	c, ctx, _ := testutil.SetupNovaEnvTestWithController(t,
		novav1alpha1.AddToScheme,
		registerNovaWebhooks,
		func(mgr ctrl.Manager) error {
			mcMgr, err := mcmanager.WithMultiCluster(mgr, nil)
			if err != nil {
				return err
			}
			return registerNovaComputeController(mgr, mcMgr, nil, api)
		},
	)
	g := NewGomegaWithT(t)
	ns := createTestNamespace(t, ctx, c)

	// Nodes are cluster-scoped, so the node and its pool label carry the
	// namespace to keep them apart from any other test's.
	nodeName := "compute-" + ns
	poolLabel := map[string]string{"openstack.c5c3.io/nova-compute-pool": ns}
	api.AddService(nodeName, "enabled", "up")

	nova := integrationNovaCR(integrationNovaName, ns, nil)
	g.Expect(c.Create(ctx, nova)).To(Succeed())
	nova.Status.InstalledRelease = integrationInitialRelease
	nova.Status.ComputeConfigSecretRef = &corev1.LocalObjectReference{Name: computeConfigSecretName(nova)}
	g.Expect(c.Status().Update(ctx, nova)).To(Succeed(), "publish the Nova's release and contract")

	for _, secret := range []*corev1.Secret{
		{
			ObjectMeta: metav1.ObjectMeta{Name: integrationServiceUserSecretName, Namespace: ns},
			Data:       map[string][]byte{"password": []byte("svc-pw")},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: computeConfigSecretName(nova), Namespace: ns},
			Data: map[string][]byte{
				computeConfigFragmentKey: []byte("[DEFAULT]\n"),
				transportURLKey:          []byte("rabbit://nova:pw@rabbitmq:5672/"),
				passwordKey:              []byte("svc-pw"),
			},
		},
	} {
		g.Expect(c.Create(ctx, secret)).To(Succeed(), "create Secret %s", secret.Name)
	}

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   nodeName,
		Labels: map[string]string{"openstack.c5c3.io/nova-compute-pool": ns, zoneLabel: "az-it"},
	}}
	g.Expect(c.Create(ctx, node)).To(Succeed())

	pool := &novav1alpha1.NovaCompute{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-it", Namespace: ns},
		Spec: novav1alpha1.NovaComputeSpec{
			NovaRef:      novav1alpha1.NovaRef{Name: integrationNovaName},
			NodeSelector: poolLabel,
		},
	}
	g.Expect(c.Create(ctx, pool)).To(Succeed())

	poolKey := client.ObjectKeyFromObject(pool)
	dsKey := client.ObjectKey{Namespace: ns, Name: "pool-it-nova-compute"}
	g.Eventually(func(ig Gomega) {
		// Every template change bumps the generation, so the rollout is marked
		// complete on every poll; a DaemonSet not created yet is simply retried.
		_ = simulators.MarkDaemonSetReady(ctx, c, dsKey)

		got := &novav1alpha1.NovaCompute{}
		ig.Expect(c.Get(ctx, poolKey, got)).To(Succeed())
		ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
		ig.Expect(ready).NotTo(BeNil())
		ig.Expect(ready.Status).To(Equal(metav1.ConditionTrue), "conditions: %+v", got.Status.Conditions)
		ig.Expect(ready.Reason).To(Equal("AllReady"))
	}, eventuallyLongTimeout, pollInterval).Should(Succeed())

	got := &novav1alpha1.NovaCompute{}
	g.Expect(c.Get(ctx, poolKey, got)).To(Succeed())
	g.Expect(got.Status.Nodes).To(ConsistOf(novav1alpha1.NovaComputeNodeStatus{
		Name: nodeName, Phase: novav1alpha1.NovaComputeNodeActive, Zone: "az-it",
		ServiceID: api.Services()[0].ID, ServiceStatus: "enabled", ServiceState: "up",
	}))
	g.Expect(got.Finalizers).To(Equal([]string{novaComputeDrainFinalizer}))

	ds := &appsv1.DaemonSet{}
	g.Expect(c.Get(ctx, dsKey, ds)).To(Succeed())
	g.Expect(metav1.IsControlledBy(ds, got)).To(BeTrue(), "a local DaemonSet is owned by reference")
	_, ok := api.Aggregate("az-it")
	g.Expect(ok).To(BeTrue(), "the zone's aggregate is created")
}
