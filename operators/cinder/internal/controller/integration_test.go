// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Integration tests for the Cinder, CinderBackend and CinderBackupBackend
// reconcilers running together against a live envtest API server.
//
// The timeouts below are generous on purpose. An envtest environment runs the
// API server and etcd and nothing else, so every state this pipeline waits on
// has to be produced by the test: the MariaDB operator turning the
// Database/User/Grant CRs ready, the Job controller completing the schema
// migration and the detach Job, and the Deployment controller counting ready
// pods for four workloads. Between those writes the pipeline advances on its own
// wait intervals (RequeueDatabaseWait is 30s), so a budget sized to the happy
// path alone turns an ordinary requeue into a flake.
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
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/cinder/internal/testutil"
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
	integrationCinderName        = "cinder"
	integrationBackendName       = "nfs"
	integrationBackupBackendName = "backups"
	integrationMariaDBName       = "mariadb"

	// #nosec G101 -- Secret object names, not credentials.
	integrationDBSecretName  = "cinder-db"
	integrationBusSecretName = "cinder-bus"
	integrationBusURL        = "rabbit://user:pass@rabbit.openstack.svc:5672/"

	// integrationImageRepository is the repository every workload and every Job
	// of these suites runs; the tag is the release under test.
	integrationImageRepository = "ghcr.io/c5c3/cinder"
	// integrationInitialRelease is the release the fixtures install, and
	// integrationTargetRelease the one the upgrade suite converges to.
	integrationInitialRelease = "2025.2"
	integrationTargetRelease  = "2026.1"

	// The NFS export the volume backend serves and the one the backup service
	// writes to. Both live on the same server, which is what a single-export
	// deployment looks like and what makes the two mount points distinguishable
	// by their base directory alone.
	integrationNFSServer  = "nfs-server.openstack.svc.cluster.local"
	integrationVolumePath = "/volumes"
	integrationBackupPath = "/backups"

	// integrationVolumeMountPath is where os-brick's remotefs driver resolves
	// the volume export to: the hex MD5 of "server:path" below
	// nfsMountPointBase. It is spelled out rather than computed so a change to
	// the derivation is caught here rather than silently agreed with.
	integrationVolumeMountPath = nfsMountPointBase + "/6f3cb55ed3b423dbb7791aaf3783754f"
	// integrationBackupMountPath is the same derivation for the backup export,
	// below backupMountPointBase.
	integrationBackupMountPath = backupMountPointBase + "/266724f97f37cb5273c6525010245e68"
)

// --- Shared helpers ---

// registerCinderWebhooks wires all three webhook handlers onto mgr. The webhook
// manifests envtest installs carry all three kinds (failurePolicy=Fail), so an
// unserved kind would fail admission.
//
// mgr.GetAPIReader() mirrors main.go: admission lookups read the API server
// directly, never a stale informer cache.
func registerCinderWebhooks(mgr ctrl.Manager) error {
	if err := (&cinderv1alpha1.CinderWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
		return err
	}
	if err := (&cinderv1alpha1.CinderBackendWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
		return err
	}
	return (&cinderv1alpha1.CinderBackupBackendWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
}

// registerCinderControllers wires all three reconcilers onto mgr through the
// production watch chains, with resolver as the target-cluster resolver.
//
// setupWithOptions is the chain each SetupWithManager applies, so the legs, the
// field indexes and the Gateway API RESTMapper probe are the production ones
// rather than a hand-built copy that drifts the moment a leg is added. The only
// difference is SkipNameValidation: controller-runtime validates controller
// names against a process-global set, and this test binary starts one manager
// per test function. A skipped registration never claims the name, so nothing
// here can hide a duplicate registration in the operator binary.
//
// The Cinder reconciler is registered first, exactly as main.go does it: its
// setup is the single registration site for the field indexes all three use.
//
// The health-check stub is what keeps the probe from firing slow HTTP GETs at a
// Service DNS name nothing answers; envtest runs no kubelet.
func registerCinderControllers(mgr ctrl.Manager, mcMgr mcmanager.Manager,
	resolver commonmulticluster.ClusterResolver,
) error {
	opts := bootstrap.TypedControllerOptions[mcreconcile.Request](1)
	opts.SkipNameValidation = ptr.To(true)

	cinder := &CinderReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Recorder:   mgr.GetEventRecorderFor("cinder-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
		HTTPClient: &stubDoer{status: http.StatusOK},
		Resolver:   resolver,
	}
	if err := cinder.setupWithOptions(mcMgr, opts); err != nil {
		return err
	}

	satelliteOpts := bootstrap.ControllerOptions(1)
	satelliteOpts.SkipNameValidation = ptr.To(true)

	backend := &CinderBackendReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("cinderbackend-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
		Resolver: resolver,
	}
	if err := backend.setupWithOptions(mgr, satelliteOpts); err != nil {
		return err
	}

	backupBackend := &CinderBackupBackendReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("cinderbackupbackend-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
		Resolver: resolver,
	}
	return backupBackend.setupWithOptions(mgr, satelliteOpts)
}

// setupEnvTestWithController wraps testutil.SetupCinderEnvTestWithController with
// the v1alpha1 scheme and both registration callbacks. A nil provider engages no
// target cluster and a nil Resolver keeps every child on the management cluster,
// which is the single-cluster default path.
//
// gatewayAPIAvailable is not set by hand: the fake HTTPRoute CRD the helper
// installs is what the setup-time RESTMapper probe answers from, so the
// HTTPRoute watch and the gateway step run exactly as they do against a cluster
// that has Gateway API.
func setupEnvTestWithController(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupCinderEnvTestWithController(t,
		cinderv1alpha1.AddToScheme,
		registerCinderWebhooks,
		func(mgr ctrl.Manager) error {
			mcMgr, err := mcmanager.WithMultiCluster(mgr, nil)
			if err != nil {
				return err
			}
			return registerCinderControllers(mgr, mcMgr, nil)
		},
	)
}

// createTestNamespace creates a uniquely named namespace per test.
func createTestNamespace(t testing.TB, ctx context.Context, c client.Client) string {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "cinder-it-"}}
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

// expectAbsent asserts the object at key does not exist on c.
func expectAbsent(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey,
	obj client.Object, what string,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	err := c.Get(ctx, key, obj)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%s %s should not exist, got %v", what, key, err)
}

// waitForCinderCondition polls the Cinder CR until the named condition reaches
// the expected status. Returns the condition.
func waitForCinderCondition(t testing.TB, ctx context.Context, c client.Client, key types.NamespacedName,
	condType string, expected metav1.ConditionStatus, timeout time.Duration,
) *metav1.Condition {
	t.Helper()
	g := NewGomegaWithT(t)

	var cond *metav1.Condition
	g.Eventually(func() metav1.ConditionStatus {
		var cr cinderv1alpha1.Cinder
		if err := c.Get(ctx, key, &cr); err != nil {
			return ""
		}
		cond = meta.FindStatusCondition(cr.Status.Conditions, condType)
		if cond == nil {
			return ""
		}
		return cond.Status
	}, timeout, pollInterval).Should(Equal(expected),
		"Cinder condition %s should reach %s", condType, expected)
	return cond
}

// waitForBackendCondition polls the CinderBackend CR until the named condition
// reaches the expected status. Returns the condition.
func waitForBackendCondition(t testing.TB, ctx context.Context, c client.Client, key types.NamespacedName,
	condType string, expected metav1.ConditionStatus, timeout time.Duration,
) *metav1.Condition {
	t.Helper()
	g := NewGomegaWithT(t)

	var cond *metav1.Condition
	g.Eventually(func() metav1.ConditionStatus {
		var cr cinderv1alpha1.CinderBackend
		if err := c.Get(ctx, key, &cr); err != nil {
			return ""
		}
		cond = meta.FindStatusCondition(cr.Status.Conditions, condType)
		if cond == nil {
			return ""
		}
		return cond.Status
	}, timeout, pollInterval).Should(Equal(expected),
		"CinderBackend condition %s should reach %s", condType, expected)
	return cond
}

// waitForBackupBackendCondition polls the CinderBackupBackend CR until the named
// condition reaches the expected status. Returns the condition.
func waitForBackupBackendCondition(t testing.TB, ctx context.Context, c client.Client, key types.NamespacedName,
	condType string, expected metav1.ConditionStatus, timeout time.Duration,
) *metav1.Condition {
	t.Helper()
	g := NewGomegaWithT(t)

	var cond *metav1.Condition
	g.Eventually(func() metav1.ConditionStatus {
		var cr cinderv1alpha1.CinderBackupBackend
		if err := c.Get(ctx, key, &cr); err != nil {
			return ""
		}
		cond = meta.FindStatusCondition(cr.Status.Conditions, condType)
		if cond == nil {
			return ""
		}
		return cond.Status
	}, timeout, pollInterval).Should(Equal(expected),
		"CinderBackupBackend condition %s should reach %s", condType, expected)
	return cond
}

// integrationCinderCR returns the Cinder these suites drive: a managed database,
// a brownfield bus and a brownfield cache, and no Keystone at all — the pairing
// rule on CinderSpec keeps spec.keystoneEndpoint and spec.serviceUser together,
// and the storage path is what these suites exercise.
//
// All four Deployment blocks spell out replicas. The shared DeploymentSpec
// schema default of three is applied by the API server as soon as a deployment
// object is present at all, which a typed Go client always serialises, so the
// one-replica webhook default never reaches the volume and backup blocks and
// the CEL rules would reject the three they arrive with.
func integrationCinderCR(name, ns string, targetRef *commonv1.TargetClusterRefSpec) *cinderv1alpha1.Cinder {
	return &cinderv1alpha1.Cinder{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: cinderv1alpha1.CinderSpec{
			OpenStackRelease: integrationInitialRelease,
			Image: commonv1.ImageSpec{
				Repository: integrationImageRepository,
				Tag:        integrationInitialRelease,
			},
			API:       cinderv1alpha1.CinderAPISpec{Deployment: cinderv1alpha1.DeploymentSpec{Replicas: 1}},
			Scheduler: cinderv1alpha1.CinderSchedulerSpec{Deployment: cinderv1alpha1.DeploymentSpec{Replicas: 1}},
			Volume:    cinderv1alpha1.CinderVolumeSpec{Deployment: cinderv1alpha1.DeploymentSpec{Replicas: 1}},
			Backup:    cinderv1alpha1.CinderBackupSpec{Deployment: cinderv1alpha1.DeploymentSpec{Replicas: 1}},
			Database: commonv1.DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: integrationMariaDBName},
				Database:   "cinder",
				SecretRef:  commonv1.SecretRefSpec{Name: integrationDBSecretName},
			},
			Cache: commonv1.CacheSpec{
				Backend: commonv1.DefaultCacheBackend,
				Servers: []string{"mc:11211"},
			},
			Messaging: commonv1.MessagingSpec{
				SecretRef: &commonv1.SecretRefSpec{Name: integrationBusSecretName},
			},
			TargetClusterRef: targetRef,
		},
	}
}

// integrationBackendCR returns the NFS volume backend these suites attach. An
// NFS export needs no credentials, so the satellite controller reports its
// credential gate satisfied on the first pass and the projection follows.
func integrationBackendCR(name, ns, cinderName string) *cinderv1alpha1.CinderBackend {
	return &cinderv1alpha1.CinderBackend{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: cinderv1alpha1.CinderBackendSpec{
			CinderRef: cinderv1alpha1.CinderRefSpec{Name: cinderName},
			Type:      cinderv1alpha1.CinderBackendTypeNFS,
			NFS: &cinderv1alpha1.NFSBackendSpec{
				Server: integrationNFSServer,
				Path:   integrationVolumePath,
			},
		},
	}
}

// integrationBackupBackendCR returns the NFS backup target these suites attach.
// It writes to an export of its own beside the volumes it reads.
func integrationBackupBackendCR(name, ns, cinderName string) *cinderv1alpha1.CinderBackupBackend {
	return &cinderv1alpha1.CinderBackupBackend{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: cinderv1alpha1.CinderBackupBackendSpec{
			CinderRef: cinderv1alpha1.CinderRefSpec{Name: cinderName},
			Type:      cinderv1alpha1.CinderBackupBackendTypeNFS,
			NFS: &cinderv1alpha1.NFSBackupBackendSpec{
				Server: integrationNFSServer,
				Path:   integrationBackupPath,
			},
		},
	}
}

// createCinderPrerequisites materialises everything the Cinder pipeline reads
// but does not create: the secret store its credential gate checks, the
// ESO-synced database credentials, the MariaDB cluster its schema is provisioned
// in, and the brownfield transport-URL Secret its bus is read from. They live on
// the cluster the children do, which is the target cluster for a placed CR.
func createCinderPrerequisites(t testing.TB, ctx context.Context, c client.Client, ns string) {
	t.Helper()
	g := NewGomegaWithT(t)

	ensureReadyClusterSecretStore(t, ctx, c)

	g.Expect(c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: integrationDBSecretName, Namespace: ns},
		Data:       map[string][]byte{"username": []byte("cinder"), "password": []byte("db-pw")},
	})).To(Succeed(), "create the database credentials Secret")

	g.Expect(c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: integrationBusSecretName, Namespace: ns},
		Data:       map[string][]byte{commonv1.DefaultTransportURLSecretKey: []byte(integrationBusURL)},
	})).To(Succeed(), "create the brownfield transport-URL Secret")

	mariadbKey := client.ObjectKey{Namespace: ns, Name: integrationMariaDBName}
	g.Expect(c.Create(ctx, &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: mariadbKey.Name, Namespace: mariadbKey.Namespace},
	})).To(Succeed(), "create the MariaDB cluster CR")
	g.Expect(simulators.SimulateMariaDBReady(ctx, c, mariadbKey, 1)).
		To(Succeed(), "mark the MariaDB cluster ready")
}

// ensureReadyClusterSecretStore creates the cluster-scoped store the credential
// gate checks and marks it Ready. It tolerates one that already exists: the
// store is cluster-scoped, so a second namespace on the same cluster finds the
// object the first call created.
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

// driveDatabase plays the MariaDB operator and the Job controller for one
// Cinder: the three provisioned CRs turn ready in the order the flow gates on
// them, then the schema migration completes. Each wait is the gate the pipeline
// actually has, so a helper that only wrote the simulated state would race the
// reconciler rather than drive it.
func driveDatabase(t testing.TB, ctx context.Context, childClient client.Client, name, ns string) {
	t.Helper()
	g := NewGomegaWithT(t)

	key := client.ObjectKey{Namespace: ns, Name: name}

	eventuallyExists(t, ctx, childClient, key, &mariadbv1alpha1.Database{}, "MariaDB Database", eventuallyLongTimeout)
	g.Expect(simulators.SimulateDatabaseReady(ctx, childClient, key)).To(Succeed(), "mark the Database ready")

	eventuallyExists(t, ctx, childClient, key, &mariadbv1alpha1.User{}, "MariaDB User", eventuallyLongTimeout)
	g.Expect(simulators.SimulateUserReady(ctx, childClient, key)).To(Succeed(), "mark the User ready")

	eventuallyExists(t, ctx, childClient, key, &mariadbv1alpha1.Grant{}, "MariaDB Grant", eventuallyLongTimeout)
	g.Expect(simulators.SimulateGrantReady(ctx, childClient, key)).To(Succeed(), "mark the Grant ready")

	dbSyncKey := client.ObjectKey{Namespace: ns, Name: name + "-db-sync"}
	eventuallyExists(t, ctx, childClient, dbSyncKey, &batchv1.Job{}, "db-sync Job", eventuallyLongTimeout)
	g.Expect(simulators.SimulateJobComplete(ctx, childClient, dbSyncKey)).To(Succeed(), "complete the db-sync Job")
}

// cinderWorkloadKeys returns the four Deployments one Cinder projects once a
// volume backend and a backup target are attached, in the order the pipeline
// creates them.
func cinderWorkloadKeys(name, ns, backendName string) []client.ObjectKey {
	return []client.ObjectKey{
		{Namespace: ns, Name: name + "-" + componentScheduler},
		{Namespace: ns, Name: name + "-" + componentVolumePrefix + backendName},
		{Namespace: ns, Name: name + "-" + componentBackup},
		{Namespace: ns, Name: name},
	}
}

// markDeploymentReady waits for the Deployment at key and writes the status a
// running Deployment controller would: every replica updated, ready and counted,
// with observedGeneration caught up. That is what both readiness gates read —
// the surge-tolerant deployment.IsDeploymentReady and the stricter
// cinderDeploymentRolledOut the RollingUpdate phase waits on.
func markDeploymentReady(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	deploy := &appsv1.Deployment{}
	eventuallyExists(t, ctx, c, key, deploy, "Deployment", eventuallyLongTimeout)
	g.Expect(simulators.SimulateDeploymentReady(ctx, c, key, ptr.Deref(deploy.Spec.Replicas, 1))).
		To(Succeed(), "mark Deployment %s available", key)
}

// mountedConfigMapName returns the name of the ConfigMap the workload's config
// volume mounts, which is the rendered cinder.conf carrier.
func mountedConfigMapName(spec *corev1.PodSpec) string {
	for i := range spec.Volumes {
		v := &spec.Volumes[i]
		if v.Name == configVolumeName && v.ConfigMap != nil {
			return v.ConfigMap.Name
		}
	}
	return ""
}

// mountedSecretName returns the name of the Secret the named pod volume projects,
// or "" when the volume is absent or is not a Secret volume.
func mountedSecretName(spec *corev1.PodSpec, volumeName string) string {
	for i := range spec.Volumes {
		v := &spec.Volumes[i]
		if v.Name == volumeName && v.Secret != nil {
			return v.Secret.SecretName
		}
	}
	return ""
}

// csiVolumeMountPath returns the path the named CSI volume is mounted at in the
// first container, or "" when the container does not mount it.
func csiVolumeMountPath(spec *corev1.PodSpec, volumeName string) string {
	for _, mount := range spec.Containers[0].VolumeMounts {
		if mount.Name == volumeName {
			return mount.MountPath
		}
	}
	return ""
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

// expectControlledByCinder asserts obj carries a controller owner reference
// naming the Cinder, which is what hands it to the garbage collection cascade
// when that CR is deleted.
func expectControlledByCinder(t testing.TB, obj client.Object, cinderName, what string) {
	t.Helper()
	g := NewGomegaWithT(t)

	owner := metav1.GetControllerOf(obj)
	g.Expect(owner).NotTo(BeNil(), "%s should carry a controller owner reference", what)
	g.Expect(owner.Kind).To(Equal("Cinder"), "%s should be controlled by a Cinder", what)
	g.Expect(owner.Name).To(Equal(cinderName), "%s should be controlled by %s", what, cinderName)
}

// hasEvent reports whether an Event with the given reason was recorded in ns.
// Events are the only trace a completed detach leaves on the parent, since the
// CR it was about is gone by then.
func hasEvent(ctx context.Context, c client.Client, ns, reason string) bool {
	var events corev1.EventList
	if err := c.List(ctx, &events, client.InNamespace(ns)); err != nil {
		return false
	}
	for i := range events.Items {
		if events.Items[i].Reason == reason {
			return true
		}
	}
	return false
}

// --- Tests ---

// TestIntegrationCinder_BackendLifecycle walks one Cinder from an empty
// deployment through an attached volume backend and backup target to their
// detach and its own deletion, against a live API server. The subtests are
// ordered and each builds on the state the previous left behind: the fixtures
// are the same objects throughout, which is what makes the transitions
// observable rather than four independent snapshots.
func TestIntegrationCinder_BackendLifecycle(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestWithController(t)
	ns := createTestNamespace(t, ctx, c)
	createCinderPrerequisites(t, ctx, c, ns)

	cinderKey := types.NamespacedName{Name: integrationCinderName, Namespace: ns}
	backendKey := types.NamespacedName{Name: integrationBackendName, Namespace: ns}
	backupBackendKey := types.NamespacedName{Name: integrationBackupBackendName, Namespace: ns}

	apiKey := client.ObjectKey{Namespace: ns, Name: integrationCinderName}
	schedulerKey := client.ObjectKey{Namespace: ns, Name: integrationCinderName + "-" + componentScheduler}
	volumeKey := client.ObjectKey{
		Namespace: ns,
		Name:      integrationCinderName + "-" + componentVolumePrefix + integrationBackendName,
	}
	backupKey := client.ObjectKey{Namespace: ns, Name: integrationCinderName + "-" + componentBackup}

	t.Run("a Cinder without backends serves its API and scheduler", func(t *testing.T) {
		g := NewGomegaWithT(t)

		g.Expect(c.Create(ctx, integrationCinderCR(integrationCinderName, ns, nil))).
			To(Succeed(), "create the Cinder CR")

		waitForCinderCondition(t, ctx, c, cinderKey, "SecretsReady", metav1.ConditionTrue, eventuallyTimeout)
		driveDatabase(t, ctx, c, integrationCinderName, ns)
		waitForCinderCondition(t, ctx, c, cinderKey, "DatabaseReady", metav1.ConditionTrue, eventuallyLongTimeout)

		// The API and the scheduler are projected whatever the storage looks
		// like: a Cinder with no backend still accepts requests and still
		// schedules them, it just has nowhere to place a volume.
		eventuallyExists(t, ctx, c, apiKey, &appsv1.Deployment{}, "API Deployment", eventuallyLongTimeout)
		eventuallyExists(t, ctx, c, schedulerKey, &appsv1.Deployment{}, "scheduler Deployment", eventuallyLongTimeout)

		backends := waitForCinderCondition(t, ctx, c, cinderKey, conditionTypeBackendsReady,
			metav1.ConditionTrue, eventuallyLongTimeout)
		g.Expect(backends.Reason).To(Equal(conditionReasonNoBackends),
			"an empty backend set is a valid state, not a violated invariant")

		volumeServices := waitForCinderCondition(t, ctx, c, cinderKey, "VolumeServicesReady",
			metav1.ConditionFalse, eventuallyLongTimeout)
		g.Expect(volumeServices.Reason).To(Equal(conditionReasonNoBackends),
			"with nothing attached there is no volume service to be ready")

		backupService := waitForCinderCondition(t, ctx, c, cinderKey, "BackupServiceReady",
			metav1.ConditionTrue, eventuallyLongTimeout)
		g.Expect(backupService.Reason).To(Equal(conditionReasonBackupNotConfigured),
			"backups are opt-in, so an unconfigured one is ready rather than failed")

		expectAbsent(t, ctx, c, volumeKey, &appsv1.Deployment{}, "volume Deployment")
		expectAbsent(t, ctx, c, backupKey, &appsv1.Deployment{}, "backup Deployment")

		// VolumeServicesReady is part of the aggregate, so the CR reports
		// not-ready for as long as it serves no storage.
		waitForCinderCondition(t, ctx, c, cinderKey, "Ready", metav1.ConditionFalse, eventuallyLongTimeout)
	})

	t.Run("attaching a backend and a backup target projects the whole fleet", func(t *testing.T) {
		g := NewGomegaWithT(t)

		g.Expect(c.Create(ctx, integrationBackendCR(integrationBackendName, ns, integrationCinderName))).
			To(Succeed(), "create the CinderBackend CR")
		g.Expect(c.Create(ctx, integrationBackupBackendCR(integrationBackupBackendName, ns, integrationCinderName))).
			To(Succeed(), "create the CinderBackupBackend CR")

		waitForBackendCondition(t, ctx, c, backendKey, conditionTypeCredentialsReady,
			metav1.ConditionTrue, eventuallyTimeout)
		waitForBackupBackendCondition(t, ctx, c, backupBackendKey, conditionTypeCredentialsReady,
			metav1.ConditionTrue, eventuallyTimeout)

		waitForCinderCondition(t, ctx, c, cinderKey, conditionTypeBackendsReady,
			metav1.ConditionTrue, eventuallyLongTimeout)
		waitForCinderCondition(t, ctx, c, cinderKey, conditionTypeBackupBackendReady,
			metav1.ConditionTrue, eventuallyLongTimeout)

		// One cinder-volume Deployment per backend, mounting that backend's
		// export at the path os-brick resolves its volumes to.
		volume := &appsv1.Deployment{}
		eventuallyExists(t, ctx, c, volumeKey, volume, "volume Deployment", eventuallyLongTimeout)
		shareVolume := shareVolumeName(integrationBackendName)
		g.Expect(csiVolumeMountPath(&volume.Spec.Template.Spec, shareVolume)).
			To(Equal(integrationVolumeMountPath),
				"the export must be mounted where cinder resolves its volumes' provider location to")

		var share *corev1.Volume
		for i := range volume.Spec.Template.Spec.Volumes {
			if volume.Spec.Template.Spec.Volumes[i].Name == shareVolume {
				share = &volume.Spec.Template.Spec.Volumes[i]
			}
		}
		g.Expect(share).NotTo(BeNil(), "the volume service must carry the %s volume", shareVolume)
		g.Expect(share.CSI).NotTo(BeNil(), "the export is an inline CSI volume, so nothing outlives the pod")
		g.Expect(share.CSI.Driver).To(Equal(nfsCSIDriverName))
		g.Expect(share.CSI.VolumeAttributes).To(HaveKeyWithValue("server", integrationNFSServer))
		g.Expect(share.CSI.VolumeAttributes).To(HaveKeyWithValue("share", integrationVolumePath))

		// The backend's three projected files, in a Secret of the backend's own
		// so a change to one backend never rolls the pods of another.
		backendSecretName := mountedSecretName(&volume.Spec.Template.Spec, backendsVolumeName)
		g.Expect(backendSecretName).To(HavePrefix(backendSecretBaseName(
			&cinderv1alpha1.Cinder{ObjectMeta: metav1.ObjectMeta{Name: integrationCinderName}},
			integrationBackendName)+"-"),
			"the backend volume must mount this backend's own projection Secret")
		backendSecret := &corev1.Secret{}
		g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: backendSecretName}, backendSecret)).
			To(Succeed(), "the backend projection Secret should exist")
		g.Expect(backendSecret.Data).To(HaveKey(backendConfDataKey))
		g.Expect(backendSecret.Data).To(HaveKey(sharesDataKey))
		g.Expect(backendSecret.Data).To(HaveKey(volumeOverlayDataKey))

		// The backup service writes to its own export and reads the volume
		// backends' exports beside it.
		backup := &appsv1.Deployment{}
		eventuallyExists(t, ctx, c, backupKey, backup, "backup Deployment", eventuallyLongTimeout)
		g.Expect(csiVolumeMountPath(&backup.Spec.Template.Spec, backupShareVolumeName)).
			To(Equal(integrationBackupMountPath))
		g.Expect(csiVolumeMountPath(&backup.Spec.Template.Spec, shareVolume)).
			To(Equal(integrationVolumeMountPath),
				"a backup reads the volume itself, at the path the volume service holds it at")

		backupSecretName := mountedSecretName(&backup.Spec.Template.Spec, backupVolumeName)
		g.Expect(backupSecretName).To(HavePrefix(backupSecretBaseName(
			&cinderv1alpha1.Cinder{ObjectMeta: metav1.ObjectMeta{Name: integrationCinderName}},
			integrationBackupBackendName)+"-"),
			"the backup volume must mount the backup target's own projection Secret")
		backupSecret := &corev1.Secret{}
		g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: backupSecretName}, backupSecret)).
			To(Succeed(), "the backup projection Secret should exist")
		g.Expect(backupSecret.Data).To(HaveKey(backupConfDataKey))

		// The rendered config every one of the four processes reads.
		configMapName := mountedConfigMapName(&volume.Spec.Template.Spec)
		g.Expect(configMapName).To(HavePrefix(integrationCinderName+"-config-"),
			"the config volume must mount the content-hashed ConfigMap")
		g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: configMapName}, &corev1.ConfigMap{})).
			To(Succeed(), "the rendered config ConfigMap should exist")

		// envtest runs no Deployment controller, so every rollout is completed
		// here. Only then can the aggregate resolve.
		for _, key := range cinderWorkloadKeys(integrationCinderName, ns, integrationBackendName) {
			markDeploymentReady(t, ctx, c, key)
		}

		waitForCinderCondition(t, ctx, c, cinderKey, "Ready", metav1.ConditionTrue, eventuallyLongTimeout)

		ready := &cinderv1alpha1.Cinder{}
		g.Expect(c.Get(ctx, cinderKey, ready)).To(Succeed())
		g.Expect(ready.Status.InstalledRelease).To(Equal(integrationInitialRelease),
			"installedRelease should be promoted after the db-sync")
		g.Expect(ready.Status.VolumeServices).To(Equal([]cinderv1alpha1.VolumeServiceStatus{{
			Backend: integrationBackendName,
			Host:    integrationCinderName + "@" + integrationBackendName,
		}}), "the status must report the host identity the backend's volumes are keyed by")

		// The purge CronJob runs on every Cinder: an unbounded soft-delete
		// backlog is a deferred outage rather than a posture worth offering.
		eventuallyExists(t, ctx, c, client.ObjectKey{
			Namespace: ns, Name: dbPurgeCronJobName(integrationCinderName),
		}, &batchv1.CronJob{}, "db-purge CronJob", eventuallyLongTimeout)

		// With the fleet projected, both satellites observe themselves in it.
		waitForBackendCondition(t, ctx, c, backendKey, conditionTypeConfigProjected,
			metav1.ConditionTrue, eventuallyLongTimeout)
		waitForBackendCondition(t, ctx, c, backendKey, "Ready", metav1.ConditionTrue, eventuallyLongTimeout)
		waitForBackupBackendCondition(t, ctx, c, backupBackendKey, conditionTypeConfigProjected,
			metav1.ConditionTrue, eventuallyLongTimeout)
		waitForBackupBackendCondition(t, ctx, c, backupBackendKey, "Ready",
			metav1.ConditionTrue, eventuallyLongTimeout)
	})

	t.Run("deleting the backend detaches it through the service-remove Job", func(t *testing.T) {
		g := NewGomegaWithT(t)

		backend := &cinderv1alpha1.CinderBackend{}
		g.Expect(c.Get(ctx, backendKey, backend)).To(Succeed())
		g.Expect(controllerutil.ContainsFinalizer(backend, CinderBackendServiceRemoveFinalizer)).To(BeTrue(),
			"the backend must be held until its volume service is unregistered")
		g.Expect(c.Delete(ctx, backend)).To(Succeed(), "delete the CinderBackend")

		// The finalizer holds the CR, and its own controller reports what the
		// detach is waiting for.
		detaching := waitForBackendCondition(t, ctx, c, backendKey, "Ready",
			metav1.ConditionFalse, eventuallyLongTimeout)
		g.Expect(detaching.Reason).To(Equal(conditionReasonDetaching))

		// The volume service has to be gone before the registry entry is
		// removed: a running cinder-volume reports itself back within seconds.
		g.Eventually(func() bool {
			return apierrors.IsNotFound(c.Get(ctx, volumeKey, &appsv1.Deployment{}))
		}, eventuallyLongTimeout, pollInterval).Should(BeTrue(),
			"the volume Deployment should be stopped before the service is unregistered")

		removeJobKey := client.ObjectKey{
			Namespace: ns,
			Name:      integrationCinderName + "-" + integrationBackendName + "-" + componentServiceRemove,
		}
		removeJob := &batchv1.Job{}
		eventuallyExists(t, ctx, c, removeJobKey, removeJob, "service-remove Job", eventuallyLongTimeout)
		g.Expect(removeJob.Spec.Template.Spec.Containers[0].Command).
			To(ContainElement(ContainSubstring("cinder-manage")),
				"the detach Job runs cinder-manage against the parent's database")
		g.Expect(strings.Join(removeJob.Spec.Template.Spec.Containers[0].Command, " ")).
			To(ContainSubstring("service remove cinder-volume "+
				integrationCinderName+"@"+integrationBackendName),
				"the Job must name the host identity the backend registered under")

		g.Expect(simulators.SimulateJobComplete(ctx, c, removeJobKey)).
			To(Succeed(), "complete the service-remove Job")

		// The finalizer comes off only once the registry entry is gone, so the
		// CR leaving etcd is the observable proof the detach completed.
		g.Eventually(func() bool {
			return apierrors.IsNotFound(c.Get(ctx, backendKey, &cinderv1alpha1.CinderBackend{}))
		}, eventuallyLongTimeout, pollInterval).Should(BeTrue(),
			"the CinderBackend should leave etcd once the service-remove finalizer is released")

		g.Eventually(func() bool {
			return hasEvent(ctx, c, ns, eventReasonServiceRemoved)
		}, eventuallyTimeout, pollInterval).Should(BeTrue(),
			"the completed detach should be recorded on the Cinder as a %s event", eventReasonServiceRemoved)

		// Nothing of the backend survives: the projection Secrets carry an
		// export this Cinder no longer serves, and the status no longer claims
		// a host identity for it.
		g.Eventually(func(ig Gomega) {
			var secrets corev1.SecretList
			ig.Expect(c.List(ctx, &secrets, client.InNamespace(ns))).To(Succeed())
			for i := range secrets.Items {
				ig.Expect(secrets.Items[i].Name).NotTo(HavePrefix(
					integrationCinderName+"-backend-"+integrationBackendName),
					"the detached backend's projection Secrets should be swept")
			}
			cur := &cinderv1alpha1.Cinder{}
			ig.Expect(c.Get(ctx, cinderKey, cur)).To(Succeed())
			ig.Expect(cur.Status.VolumeServices).To(BeEmpty(),
				"a detached backend must leave no host identity behind")
		}, eventuallyLongTimeout, pollInterval).Should(Succeed())
	})

	t.Run("deleting the Cinder releases its children and orphans the satellite", func(t *testing.T) {
		g := NewGomegaWithT(t)

		api := &appsv1.Deployment{}
		g.Expect(c.Get(ctx, apiKey, api)).To(Succeed())
		configMapName := mountedConfigMapName(&api.Spec.Template.Spec)
		g.Expect(configMapName).NotTo(BeEmpty(), "the API pods must mount a rendered config")

		// A local Cinder hands its children to the garbage collection cascade
		// rather than deleting them itself, so what has to hold before the
		// deletion is that every one of them names the CR as its controller.
		// envtest runs no garbage collector, so the cascade itself is asserted
		// where the operator performs it by hand: on a target cluster, in
		// TestIntegration_Multicluster_CinderTargetCluster.
		// The volume Deployment is not among them: the previous subtest detached
		// its backend, and a detached backend keeps no process.
		for _, key := range []client.ObjectKey{apiKey, schedulerKey, backupKey} {
			deploy := &appsv1.Deployment{}
			g.Expect(c.Get(ctx, key, deploy)).To(Succeed())
			expectControlledByCinder(t, deploy, integrationCinderName, "Deployment "+key.String())
		}
		purge := &batchv1.CronJob{}
		g.Expect(c.Get(ctx, client.ObjectKey{
			Namespace: ns, Name: dbPurgeCronJobName(integrationCinderName),
		}, purge)).To(Succeed())
		expectControlledByCinder(t, purge, integrationCinderName, "the purge CronJob")
		configMap := &corev1.ConfigMap{}
		g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: configMapName}, configMap)).To(Succeed())
		expectControlledByCinder(t, configMap, integrationCinderName, "the rendered config ConfigMap")
		for _, name := range []string{
			database.ConnectionSecretName(integrationCinderName),
			messaging.TransportURLSecretName(integrationCinderName),
		} {
			secret := &corev1.Secret{}
			g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, secret)).To(Succeed())
			expectControlledByCinder(t, secret, integrationCinderName, "the derived Secret "+name)
		}

		cinder := &cinderv1alpha1.Cinder{}
		g.Expect(c.Get(ctx, cinderKey, cinder)).To(Succeed())
		g.Expect(controllerutil.ContainsFinalizer(cinder, cinderFinalizer)).To(BeTrue(),
			"the Cinder is held until its MariaDB CRs have been issued a Delete")
		g.Expect(c.Delete(ctx, cinder)).To(Succeed(), "delete the Cinder CR")

		g.Eventually(func() bool {
			return apierrors.IsNotFound(c.Get(ctx, cinderKey, &cinderv1alpha1.Cinder{}))
		}, eventuallyLongTimeout, pollInterval).Should(BeTrue(),
			"the CR should leave etcd once the finalizer is released")

		// The three MariaDB CRs are the children the operator deletes by name,
		// which is the whole reason the finalizer holds the CR one pass longer:
		// the schema teardown has to be triggered before the owner-ref chain
		// disappears.
		dbKey := client.ObjectKey{Namespace: ns, Name: integrationCinderName}
		g.Eventually(func(ig Gomega) {
			ig.Expect(apierrors.IsNotFound(c.Get(ctx, dbKey, &mariadbv1alpha1.Database{}))).To(BeTrue(),
				"the MariaDB Database should have been deleted with its Cinder")
			ig.Expect(apierrors.IsNotFound(c.Get(ctx, dbKey, &mariadbv1alpha1.User{}))).To(BeTrue(),
				"the MariaDB User should have been deleted with its Cinder")
			ig.Expect(apierrors.IsNotFound(c.Get(ctx, dbKey, &mariadbv1alpha1.Grant{}))).To(BeTrue(),
				"the MariaDB Grant should have been deleted with its Cinder")
		}, eventuallyLongTimeout, pollInterval).Should(Succeed())

		// The satellite is a CR of its own: it survives its parent and reports
		// that the cluster its backup service ran on is now unknown.
		orphaned := waitForBackupBackendCondition(t, ctx, c, backupBackendKey, conditionTypeCredentialsReady,
			metav1.ConditionFalse, eventuallyLongTimeout)
		g.Expect(orphaned.Reason).To(Equal(conditionReasonWaitingForParent))
	})
}

// TestIntegrationCinder_UpgradeCycle_ExpandMigrateContract drives a full release
// upgrade (2025.2 → 2026.1) end to end against envtest. It locks the two
// properties no unit test observes together:
//
//   - the Expanding → Migrating → RollingUpdate → Contracting phase walk, with
//     each phase Job carrying the target-release image and its own command;
//   - the sequenced rollout inside RollingUpdate: the scheduler re-images first
//     and the volume, backup and API workloads follow one at a time, each held
//     behind the previous one's converged rollout, so the contract phase cannot
//     run against a process still on the old code.
//
// envtest runs no Job or Deployment controller, so every Job completion and
// every rollout is written by the test; the phase machine cannot advance alone.
func TestIntegrationCinder_UpgradeCycle_ExpandMigrateContract(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)
	ns := createTestNamespace(t, ctx, c)
	createCinderPrerequisites(t, ctx, c, ns)

	cinderKey := types.NamespacedName{Name: integrationCinderName, Namespace: ns}
	apiKey := client.ObjectKey{Namespace: ns, Name: integrationCinderName}
	schedulerKey := client.ObjectKey{Namespace: ns, Name: integrationCinderName + "-" + componentScheduler}
	volumeKey := client.ObjectKey{
		Namespace: ns,
		Name:      integrationCinderName + "-" + componentVolumePrefix + integrationBackendName,
	}
	backupKey := client.ObjectKey{Namespace: ns, Name: integrationCinderName + "-" + componentBackup}

	g.Expect(c.Create(ctx, integrationCinderCR(integrationCinderName, ns, nil))).To(Succeed(), "create the Cinder CR")
	g.Expect(c.Create(ctx, integrationBackendCR(integrationBackendName, ns, integrationCinderName))).
		To(Succeed(), "create the CinderBackend CR")
	g.Expect(c.Create(ctx, integrationBackupBackendCR(integrationBackupBackendName, ns, integrationCinderName))).
		To(Succeed(), "create the CinderBackupBackend CR")

	// Drive the 2025.2 install to Ready, which is the state an upgrade starts
	// from: four workloads on the old image and a schema at the old release.
	driveDatabase(t, ctx, c, integrationCinderName, ns)
	for _, key := range cinderWorkloadKeys(integrationCinderName, ns, integrationBackendName) {
		markDeploymentReady(t, ctx, c, key)
	}
	waitForCinderCondition(t, ctx, c, cinderKey, "Ready", metav1.ConditionTrue, eventuallyLongTimeout)

	installed := &cinderv1alpha1.Cinder{}
	g.Expect(c.Get(ctx, cinderKey, installed)).To(Succeed())
	g.Expect(installed.Status.InstalledRelease).To(Equal(integrationInitialRelease))
	g.Expect(string(installed.Status.UpgradePhase)).To(BeEmpty(),
		"no upgrade should be in flight after a fresh install")

	// --- Trigger the upgrade: bump spec.openStackRelease and spec.image.tag in
	// lockstep, which is the contract checkImageReleaseMismatch enforces. The
	// Get→Update loop rides out the conflict a concurrent status write races in.
	g.Eventually(func() error {
		cur := &cinderv1alpha1.Cinder{}
		if err := c.Get(ctx, cinderKey, cur); err != nil {
			return err
		}
		cur.Spec.OpenStackRelease = integrationTargetRelease
		cur.Spec.Image.Tag = integrationTargetRelease
		return c.Update(ctx, cur)
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "bump the Cinder to release 2026.1")

	// Phase 1: Expanding. Cinder has no separate expand verb, so the phase runs
	// the same idempotent cinder-manage db sync the steady state does, with the
	// target release's migration tree.
	waitForUpgradePhase(t, ctx, c, cinderKey, commonv1.UpgradePhaseExpanding)
	expandKey := client.ObjectKey{Namespace: ns, Name: integrationCinderName + "-" + upgradeExpandJobSuffix}
	expandJob := &batchv1.Job{}
	eventuallyExists(t, ctx, c, expandKey, expandJob, "db-expand Job", eventuallyLongTimeout)
	g.Expect(expandJob.Spec.Template.Spec.Containers[0].Image).To(Equal(releaseTag(integrationTargetRelease)),
		"the expand Job runs the target release's binary")
	g.Expect(expandJob.Spec.Template.Spec.Containers[0].Command).
		To(ContainElements("cinder-manage", "db", "sync"))
	g.Expect(simulators.SimulateJobComplete(ctx, c, expandKey)).To(Succeed(), "complete the db-expand Job")

	upgrading := &cinderv1alpha1.Cinder{}
	g.Expect(c.Get(ctx, cinderKey, upgrading)).To(Succeed())
	g.Expect(upgrading.Status.TargetRelease).To(Equal(integrationTargetRelease),
		"targetRelease should record the in-flight upgrade")

	// Phase 2: Migrating. The Job runs cinder-status upgrade check and reports
	// its exit code through the termination log.
	waitForUpgradePhase(t, ctx, c, cinderKey, commonv1.UpgradePhaseMigrating)
	migrateKey := client.ObjectKey{Namespace: ns, Name: integrationCinderName + "-" + upgradeMigrateJobSuffix}
	migrateJob := &batchv1.Job{}
	eventuallyExists(t, ctx, c, migrateKey, migrateJob, "db-migrate Job", eventuallyLongTimeout)
	g.Expect(strings.Join(migrateJob.Spec.Template.Spec.Containers[0].Command, " ")).
		To(ContainSubstring("cinder-status"), "the migrate phase runs the upgrade check")
	g.Expect(simulators.SimulateJobComplete(ctx, c, migrateKey)).To(Succeed(), "complete the db-migrate Job")

	// Phase 3: RollingUpdate. The four workloads re-image one at a time, in
	// pipeline order, each held behind the previous one's converged rollout.
	waitForUpgradePhase(t, ctx, c, cinderKey, commonv1.UpgradePhaseRollingUpdate)

	g.Eventually(func() string {
		return deploymentImage(ctx, c, schedulerKey)
	}, eventuallyLongTimeout, pollInterval).Should(Equal(releaseTag(integrationTargetRelease)),
		"the scheduler is the first workload the RollingUpdate phase re-images")
	g.Expect(deploymentImage(ctx, c, volumeKey)).To(Equal(releaseTag(integrationInitialRelease)),
		"the volume service must wait behind the scheduler's rollout")
	g.Expect(deploymentImage(ctx, c, backupKey)).To(Equal(releaseTag(integrationInitialRelease)),
		"the backup service must wait behind the scheduler's rollout")
	g.Expect(deploymentImage(ctx, c, apiKey)).To(Equal(releaseTag(integrationInitialRelease)),
		"the API must wait behind the scheduler's rollout")

	// The phase must not advance while the re-imaged scheduler has not
	// converged; the contract phase would otherwise run against old code.
	g.Consistently(func() commonv1.UpgradePhase {
		cur := &cinderv1alpha1.Cinder{}
		if err := c.Get(ctx, cinderKey, cur); err != nil {
			return ""
		}
		return cur.Status.UpgradePhase
	}, 2*time.Second, pollInterval).Should(Equal(commonv1.UpgradePhaseRollingUpdate),
		"upgradePhase must stay RollingUpdate until every workload has rolled")

	markDeploymentReady(t, ctx, c, schedulerKey)
	g.Eventually(func() string {
		return deploymentImage(ctx, c, volumeKey)
	}, eventuallyLongTimeout, pollInterval).Should(Equal(releaseTag(integrationTargetRelease)),
		"the volume service re-images once the scheduler has rolled")
	g.Expect(deploymentImage(ctx, c, apiKey)).To(Equal(releaseTag(integrationInitialRelease)),
		"the API must still wait behind the volume service")

	markDeploymentReady(t, ctx, c, volumeKey)
	g.Eventually(func() string {
		return deploymentImage(ctx, c, backupKey)
	}, eventuallyLongTimeout, pollInterval).Should(Equal(releaseTag(integrationTargetRelease)),
		"the backup service re-images once the volume service has rolled")

	markDeploymentReady(t, ctx, c, backupKey)
	g.Eventually(func() string {
		return deploymentImage(ctx, c, apiKey)
	}, eventuallyLongTimeout, pollInterval).Should(Equal(releaseTag(integrationTargetRelease)),
		"the API re-images last, once every bus process has rolled")

	markDeploymentReady(t, ctx, c, apiKey)

	// Phase 4: Contracting. The contract phase runs the online data migrations
	// that backfill the rows the new schema needs, which the completed rollout
	// is what makes safe.
	waitForUpgradePhase(t, ctx, c, cinderKey, commonv1.UpgradePhaseContracting)
	contractKey := client.ObjectKey{Namespace: ns, Name: integrationCinderName + "-" + upgradeContractJobSuffix}
	contractJob := &batchv1.Job{}
	eventuallyExists(t, ctx, c, contractKey, contractJob, "db-contract Job", eventuallyLongTimeout)
	g.Expect(contractJob.Spec.Template.Spec.Containers[0].Command).
		To(ContainElements("cinder-manage", "db", "online_data_migrations"))
	g.Expect(simulators.SimulateJobComplete(ctx, c, contractKey)).To(Succeed(), "complete the db-contract Job")

	g.Eventually(func(ig Gomega) {
		cur := &cinderv1alpha1.Cinder{}
		ig.Expect(c.Get(ctx, cinderKey, cur)).To(Succeed())
		ig.Expect(cur.Status.InstalledRelease).To(Equal(integrationTargetRelease),
			"installedRelease should advance once the schema is contracted")
		ig.Expect(string(cur.Status.UpgradePhase)).To(BeEmpty(),
			"upgradePhase should be cleared once the upgrade completes")
		ig.Expect(cur.Status.TargetRelease).To(BeEmpty(),
			"targetRelease should be cleared once the upgrade completes")
	}, eventuallyLongTimeout, pollInterval).Should(Succeed())

	// The three bus processes cache the RPC versions their peers announced at
	// startup, so they are rolled once more on the promoted release. The API is
	// deliberately not: it rolled during RollingUpdate and came up holding the
	// new minimum.
	g.Eventually(func(ig Gomega) {
		for _, key := range []client.ObjectKey{schedulerKey, volumeKey, backupKey} {
			deploy := &appsv1.Deployment{}
			ig.Expect(c.Get(ctx, key, deploy)).To(Succeed())
			ig.Expect(deploy.Spec.Template.Annotations).To(
				HaveKeyWithValue(installedReleaseAnnotation, integrationTargetRelease),
				"%s must be rolled onto the promoted release", key)
		}
	}, eventuallyLongTimeout, pollInterval).Should(Succeed())

	api := &appsv1.Deployment{}
	g.Expect(c.Get(ctx, apiKey, api)).To(Succeed())
	g.Expect(api.Spec.Template.Annotations).NotTo(HaveKey(installedReleaseAnnotation),
		"the API carries no release stamp: it rolled during RollingUpdate already")
}

// waitForUpgradePhase polls the Cinder CR until status.upgradePhase reaches the
// expected phase.
func waitForUpgradePhase(t testing.TB, ctx context.Context, c client.Client, key types.NamespacedName,
	expected commonv1.UpgradePhase,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	g.Eventually(func() commonv1.UpgradePhase {
		cur := &cinderv1alpha1.Cinder{}
		if err := c.Get(ctx, key, cur); err != nil {
			return ""
		}
		return cur.Status.UpgradePhase
	}, eventuallyLongTimeout, pollInterval).Should(Equal(expected),
		"upgradePhase should transition to %s", expected)
}
