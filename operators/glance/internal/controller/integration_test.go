// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Package controller contains integration tests for the Glance and
// GlanceBackend reconcilers running together against a live envtest API server.
package controller

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/glance/internal/testutil"
)

// Test timeout constants for CI tuning.
const (
	// eventuallyTimeout is the default polling timeout for Eventually assertions.
	eventuallyTimeout = 30 * time.Second
	// eventuallyLongTimeout covers the db-sync/Deployment path, which depends on
	// the RequeueDatabaseWait backoff to rediscover a simulated Job completion.
	eventuallyLongTimeout = 2 * RequeueDatabaseWait
	// pollInterval is the polling interval for Eventually assertions.
	pollInterval = 500 * time.Millisecond
)

// --- Shared helpers ---

// setupEnvTestWithController wraps testutil.SetupGlanceEnvTestWithController with
// the v1alpha1 scheme and the webhook + controller registration callbacks. It
// wires BOTH webhooks (the manifests envtest installs carry both kinds,
// failurePolicy=Fail) and BOTH reconcilers.
//
// The reconcilers are built manually rather than via SetupWithManager: that
// method probes the RESTMapper for Gateway API (set explicitly here) and would
// trip controller-runtime's process-global controller-name validation across the
// sequential managers each test spins up, so the controllers opt out via
// SkipNameValidation, exactly as keystone's and horizon's integration setups do.
func setupEnvTestWithController(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupGlanceEnvTestWithController(
		t,
		glancev1alpha1.AddToScheme,
		func(mgr ctrl.Manager) error {
			if err := (&glancev1alpha1.GlanceWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
				return err
			}
			return (&glancev1alpha1.GlanceBackendWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
		},
		func(mgr ctrl.Manager) error {
			// Register both field-index sets here — the single registration site,
			// mirroring GlanceReconciler.SetupWithManager.
			if err := registerGlanceIndexes(context.Background(), mgr.GetFieldIndexer()); err != nil {
				return err
			}
			if err := registerGlanceBackendIndexes(context.Background(), mgr.GetFieldIndexer()); err != nil {
				return err
			}

			r := &GlanceReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("glance-controller"),
				// A healthy stub keeps the health-check probe from firing slow HTTP
				// GETs at the unreachable Service DNS; envtest has no kubelet.
				HTTPClient: &stubDoer{status: http.StatusOK},
				// envtest loads the fake HTTPRoute CRD, so the kind is available.
				// Mirror what SetupWithManager would set from the RESTMapper.
				gatewayAPIAvailable: true,
			}
			if err := ctrl.NewControllerManagedBy(mgr).
				For(&glancev1alpha1.Glance{}, builder.WithPredicates(watch.CRUpdatePredicate())).
				Owns(&appsv1.Deployment{}).
				Owns(&corev1.Service{}).
				Owns(&corev1.ConfigMap{}).
				Owns(&corev1.Secret{}).
				Owns(&policyv1.PodDisruptionBudget{}).
				Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
				Owns(&networkingv1.NetworkPolicy{}).
				Owns(&batchv1.Job{}).
				Owns(&gatewayv1.HTTPRoute{}).
				Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(
					secretToGlanceWithBackendsMapper(mgr.GetClient()),
				)).
				Watches(&glancev1alpha1.GlanceBackend{}, handler.EnqueueRequestsFromMapFunc(
					glanceBackendToGlanceMapper(),
				)).
				Watches(&mariadbv1alpha1.MariaDB{}, handler.EnqueueRequestsFromMapFunc(
					mariaDBToGlanceMapper(mgr.GetClient()),
				)).
				Watches(&esov1.ClusterSecretStore{}, handler.EnqueueRequestsFromMapFunc(
					storeToGlanceMapper(mgr.GetClient(), commonv1.SecretStoreKindCluster),
				)).
				Watches(&esov1.SecretStore{}, handler.EnqueueRequestsFromMapFunc(
					storeToGlanceMapper(mgr.GetClient(), commonv1.SecretStoreKindNamespaced),
				)).
				WithOptions(controller.Options{SkipNameValidation: ptr.To(true)}).
				Complete(r); err != nil {
				return err
			}

			br := &GlanceBackendReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("glancebackend-controller"),
			}
			return ctrl.NewControllerManagedBy(mgr).
				For(&glancev1alpha1.GlanceBackend{}, builder.WithPredicates(watch.CRUpdatePredicate())).
				Watches(&glancev1alpha1.Glance{}, handler.EnqueueRequestsFromMapFunc(
					glanceToGlanceBackendsMapper(mgr.GetClient()),
				)).
				WithOptions(controller.Options{SkipNameValidation: ptr.To(true)}).
				Complete(br)
		},
	)
}

// createTestNamespace creates a uniquely named namespace per test.
func createTestNamespace(t testing.TB, ctx context.Context, c client.Client) string {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "glance-it-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")
	return ns.Name
}

// ensureReadyClusterSecretStore creates the OpenBao-backed ClusterSecretStore
// with a Ready=True condition. reconcileSecrets gates on this status; without it
// every integration test would flip to SecretsReady=False with reason
// SecretStoreNotReady.
func ensureReadyClusterSecretStore(t testing.TB, ctx context.Context, c client.Client) {
	t.Helper()
	g := NewGomegaWithT(t)

	store := &esov1.ClusterSecretStore{ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName}}
	g.Expect(c.Create(ctx, store)).To(Succeed(), "create ClusterSecretStore")

	store.Status = esov1.SecretStoreStatus{
		Conditions: []esov1.SecretStoreStatusCondition{
			{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
		},
	}
	g.Expect(c.Status().Update(ctx, store)).To(Succeed(), "update ClusterSecretStore status")
}

// createGlancePrerequisites materializes the secret store and the plain database
// and service-user Secrets the secrets gate reads. The gate reads the
// materialized Secret directly (its steady-state fast path), so no ExternalSecret
// fixture is required — mirroring the reconcile_secrets unit fixtures.
func createGlancePrerequisites(t testing.TB, ctx context.Context, c client.Client, ns string) {
	t.Helper()
	g := NewGomegaWithT(t)

	ensureReadyClusterSecretStore(t, ctx, c)

	dbSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "glance-db", Namespace: ns},
		Data:       map[string][]byte{"username": []byte("glance"), "password": []byte("db-pw")},
	}
	g.Expect(c.Create(ctx, dbSecret)).To(Succeed(), "create DB Secret")

	svcSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "glance-service-user", Namespace: ns},
		Data:       map[string][]byte{"password": []byte("svc-pw")},
	}
	g.Expect(c.Create(ctx, svcSecret)).To(Succeed(), "create service-user Secret")
}

// integrationGlance returns a valid brownfield Glance CR (spec.database.host set,
// no clusterRef) so no MariaDB cluster CR is needed to progress the pipeline.
func integrationGlance(name, ns string) *glancev1alpha1.Glance {
	return &glancev1alpha1.Glance{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: glancev1alpha1.GlanceSpec{
			OpenStackRelease: "2025.2",
			Deployment:       glancev1alpha1.DeploymentSpec{Replicas: 1},
			Image:            commonv1.ImageSpec{Repository: "ghcr.io/c5c3/glance", Tag: "2025.2"},
			Database: commonv1.DatabaseSpec{
				Host:      "db.example.com",
				Port:      3306,
				Database:  "glance",
				SecretRef: commonv1.SecretRefSpec{Name: "glance-db"},
			},
			Cache: commonv1.CacheSpec{
				Backend: commonv1.DefaultCacheBackend,
				Servers: []string{"mc:11211"},
			},
			KeystoneEndpoint: "http://keystone.openstack.svc.cluster.local:5000/v3",
			ServiceUser: glancev1alpha1.ServiceUserSpec{
				SecretRef: commonv1.SecretRefSpec{Name: "glance-service-user", Key: "password"},
			},
		},
	}
}

// integrationBackend returns a valid S3-typed GlanceBackend attached to glanceRef
// and referencing the credentials Secret integrationS3CredentialsSecret creates.
func integrationBackend(name, ns, glanceRef string, isDefault bool) *glancev1alpha1.GlanceBackend {
	return &glancev1alpha1.GlanceBackend{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: glancev1alpha1.GlanceBackendSpec{
			GlanceRef: glancev1alpha1.GlanceRefSpec{Name: glanceRef},
			Type:      glancev1alpha1.GlanceBackendTypeS3,
			IsDefault: isDefault,
			S3: &glancev1alpha1.S3BackendSpec{
				Host:                 "https://s3.example.com",
				Bucket:               "images",
				CredentialsSecretRef: glancev1alpha1.SecretNameRefSpec{Name: name + "-creds"},
				BucketURLFormat:      "path",
			},
		},
	}
}

// integrationS3CredentialsSecret returns the S3 credentials Secret a backend
// built via integrationBackend references, carrying both contract data keys.
func integrationS3CredentialsSecret(backendName, ns string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: backendName + "-creds", Namespace: ns},
		Data: map[string][]byte{
			glancev1alpha1.S3AccessKeyIDKey:     []byte("AKIAEXAMPLE"),
			glancev1alpha1.S3SecretAccessKeyKey: []byte("secret-access-key-value"),
		},
	}
}

// waitForGlanceCondition polls the Glance CR until the named condition reaches
// the expected status. Returns the condition.
func waitForGlanceCondition(t testing.TB, ctx context.Context, c client.Client, key types.NamespacedName, condType string, expected metav1.ConditionStatus, timeout time.Duration) *metav1.Condition {
	t.Helper()
	g := NewGomegaWithT(t)

	var cond *metav1.Condition
	g.Eventually(func() metav1.ConditionStatus {
		var glance glancev1alpha1.Glance
		if err := c.Get(ctx, key, &glance); err != nil {
			return ""
		}
		cond = meta.FindStatusCondition(glance.Status.Conditions, condType)
		if cond == nil {
			return ""
		}
		return cond.Status
	}, timeout, pollInterval).Should(Equal(expected),
		fmt.Sprintf("Glance condition %s should reach %s", condType, expected))
	return cond
}

// waitForBackendCondition polls the GlanceBackend CR until the named condition
// reaches the expected status. Returns the condition.
func waitForBackendCondition(t testing.TB, ctx context.Context, c client.Client, key types.NamespacedName, condType string, expected metav1.ConditionStatus, timeout time.Duration) *metav1.Condition {
	t.Helper()
	g := NewGomegaWithT(t)

	var cond *metav1.Condition
	g.Eventually(func() metav1.ConditionStatus {
		var backend glancev1alpha1.GlanceBackend
		if err := c.Get(ctx, key, &backend); err != nil {
			return ""
		}
		cond = meta.FindStatusCondition(backend.Status.Conditions, condType)
		if cond == nil {
			return ""
		}
		return cond.Status
	}, timeout, pollInterval).Should(Equal(expected),
		fmt.Sprintf("GlanceBackend condition %s should reach %s", condType, expected))
	return cond
}

// --- Tests ---

// TestIntegrationGlance_BackendAttachProjectsConfig drives the end-to-end
// condition flow with both controllers running: a Glance with prerequisites but
// no backends parks at BackendsReady=False/NoDefaultBackend; attaching a default
// backend with credentials flips the backend CredentialsReady True, renders the
// parent's backends Secret and config ConfigMap, and — once the simulated db-sync
// lets the Deployment appear with the projected store — drives the backend's
// ConfigProjected True.
func TestIntegrationGlance_BackendAttachProjectsConfig(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)
	ns := createTestNamespace(t, ctx, c)
	createGlancePrerequisites(t, ctx, c, ns)

	glance := integrationGlance("glance", ns)
	g.Expect(c.Create(ctx, glance)).To(Succeed(), "create Glance CR")
	glanceKey := types.NamespacedName{Name: "glance", Namespace: ns}

	// The secrets gate must pass for the pipeline to reach the backends step at
	// all; reaching BackendsReady proves SecretsReady turned True first.
	waitForGlanceCondition(t, ctx, c, glanceKey, "SecretsReady", metav1.ConditionTrue, eventuallyTimeout)
	initial := waitForGlanceCondition(t, ctx, c, glanceKey, "BackendsReady", metav1.ConditionFalse, eventuallyTimeout)
	g.Expect(initial.Reason).To(Equal(conditionReasonNoDefaultBackend),
		"zero backends must surface as NoDefaultBackend")

	// Attach a default backend plus its credentials Secret.
	g.Expect(c.Create(ctx, integrationS3CredentialsSecret("store", ns))).To(Succeed(), "create S3 credentials Secret")
	backend := integrationBackend("store", ns, "glance", true)
	g.Expect(c.Create(ctx, backend)).To(Succeed(), "create GlanceBackend CR")
	backendKey := types.NamespacedName{Name: "store", Namespace: ns}

	// The dedicated backend controller resolves the credentials.
	waitForBackendCondition(t, ctx, c, backendKey, conditionTypeCredentialsReady, metav1.ConditionTrue, eventuallyTimeout)

	// The parent aggregates the credential-ready default and renders the backends
	// Secret; the config ConfigMap follows in the same pass.
	waitForGlanceCondition(t, ctx, c, glanceKey, "BackendsReady", metav1.ConditionTrue, eventuallyTimeout)

	// The database step db-syncs against the rendered config; simulate the Job so
	// the Deployment (which mounts the projection) can be created — envtest runs
	// no kubelet, so nothing completes the Job otherwise.
	dbSyncKey := client.ObjectKey{Namespace: ns, Name: "glance-db-sync"}
	g.Eventually(func() error {
		return c.Get(ctx, dbSyncKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "db-sync Job should appear")
	g.Expect(simulators.SimulateJobComplete(ctx, c, dbSyncKey)).To(Succeed(), "simulate db-sync Job completion")

	// The Deployment object appearing (with its config + backends volumes) is
	// enough for the backend to observe ConfigProjected — envtest has no kubelet
	// to make it Available.
	deployKey := client.ObjectKey{Namespace: ns, Name: "glance"}
	var deploy appsv1.Deployment
	g.Eventually(func() error {
		return c.Get(ctx, deployKey, &deploy)
	}, eventuallyLongTimeout, pollInterval).Should(Succeed(), "Glance Deployment should appear")

	// The Deployment mounts the content-hashed config ConfigMap and backends
	// Secret; both objects must exist under their <name>-config-/<name>-backends-
	// prefixes.
	var configMapName, backendsSecretName string
	for i := range deploy.Spec.Template.Spec.Volumes {
		v := &deploy.Spec.Template.Spec.Volumes[i]
		switch {
		case v.Name == configVolumeName && v.ConfigMap != nil:
			configMapName = v.ConfigMap.Name
		case v.Name == backendsVolumeName && v.Secret != nil:
			backendsSecretName = v.Secret.SecretName
		}
	}
	g.Expect(configMapName).To(HavePrefix("glance-config-"), "config volume must mount the rendered ConfigMap")
	g.Expect(backendsSecretName).To(HavePrefix("glance-backends-"), "backends volume must mount the rendered Secret")
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: configMapName}, &corev1.ConfigMap{})).
		To(Succeed(), "rendered config ConfigMap should exist")
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: backendsSecretName}, &corev1.Secret{})).
		To(Succeed(), "rendered backends Secret should exist")

	// With the store projected into the Deployment, the backend converges to
	// ConfigProjected=True.
	waitForBackendCondition(t, ctx, c, backendKey, conditionTypeConfigProjected, metav1.ConditionTrue, eventuallyLongTimeout)
}

// TestIntegrationGlance_FlipDefaultOffWakesParent proves the cross-CR watch
// wiring: flipping the attached backend's isDefault off — a backend spec edit,
// with no edit to the Glance spec — wakes the parent and flips BackendsReady back
// to False/NoDefaultBackend.
func TestIntegrationGlance_FlipDefaultOffWakesParent(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)
	ns := createTestNamespace(t, ctx, c)
	createGlancePrerequisites(t, ctx, c, ns)

	glance := integrationGlance("glance", ns)
	g.Expect(c.Create(ctx, glance)).To(Succeed(), "create Glance CR")
	glanceKey := types.NamespacedName{Name: "glance", Namespace: ns}

	g.Expect(c.Create(ctx, integrationS3CredentialsSecret("store", ns))).To(Succeed(), "create S3 credentials Secret")
	backend := integrationBackend("store", ns, "glance", true)
	g.Expect(c.Create(ctx, backend)).To(Succeed(), "create GlanceBackend CR")
	backendKey := types.NamespacedName{Name: "store", Namespace: ns}

	// Converge to a valid single-default projection.
	waitForGlanceCondition(t, ctx, c, glanceKey, "BackendsReady", metav1.ConditionTrue, eventuallyTimeout)

	// Flip isDefault off — no Glance spec edit. The parent's GlanceBackend watch
	// must wake it.
	var got glancev1alpha1.GlanceBackend
	g.Expect(c.Get(ctx, backendKey, &got)).To(Succeed())
	got.Spec.IsDefault = false
	g.Expect(c.Update(ctx, &got)).To(Succeed(), "flip isDefault off")

	flipped := waitForGlanceCondition(t, ctx, c, glanceKey, "BackendsReady", metav1.ConditionFalse, eventuallyTimeout)
	g.Expect(flipped.Reason).To(Equal(conditionReasonNoDefaultBackend),
		"dropping the only default must surface as NoDefaultBackend")
}

// TestIntegrationGlance_DeleteReleasesFinalizer proves the finalizer lifecycle:
// the reconciler stamps its finalizer, and deleting the Glance releases it and
// removes the object. In envtest there are no live MariaDB resources for a
// brownfield database, so the finalizer is released on the first deletion pass.
func TestIntegrationGlance_DeleteReleasesFinalizer(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)
	ns := createTestNamespace(t, ctx, c)

	glance := integrationGlance("glance", ns)
	g.Expect(c.Create(ctx, glance)).To(Succeed(), "create Glance CR")
	glanceKey := types.NamespacedName{Name: "glance", Namespace: ns}

	// The reconciler installs its finalizer before any sub-reconciler runs.
	g.Eventually(func() bool {
		var got glancev1alpha1.Glance
		if err := c.Get(ctx, glanceKey, &got); err != nil {
			return false
		}
		return controllerutil.ContainsFinalizer(&got, glanceFinalizer)
	}, eventuallyTimeout, pollInterval).Should(BeTrue(), "finalizer should be installed")

	g.Expect(c.Delete(ctx, glance)).To(Succeed(), "delete Glance CR")

	g.Eventually(func() bool {
		var got glancev1alpha1.Glance
		err := c.Get(ctx, glanceKey, &got)
		return apierrors.IsNotFound(err)
	}, eventuallyTimeout, pollInterval).Should(BeTrue(), "Glance should be fully removed after finalizer release")
}

// TestIntegrationGlance_UpgradeCycle_ExpandMigrateContract drives a full glance
// release upgrade (2025.2 → 2026.1) through the shared expand-migrate-contract
// flow end to end against envtest, mirroring keystone's upgrade-cycle test. It
// locks four properties no unit test observes together:
//
//   - the Expanding → Migrating → RollingUpdate → Contracting phase walk, with
//     each glance-manage db expand|migrate|contract phase Job carrying the
//     target-release image;
//   - the rollout-gated contract flip: the phase stays RollingUpdate until the
//     re-imaged Deployment reports ready, then advances to Contracting;
//   - the eventlet → uWSGI launch-command switch the Deployment template takes on
//     the release bump (glanceUsesUWSGI derives the mode from openStackRelease);
//   - the post-upgrade steady-state db-sync Job re-running with the new image on
//     pod-spec-hash drift, so its db load_metadefs step loads the new release's
//     definitions.
//
// envtest runs no Job or Deployment controllers, so every Job completion and
// Deployment readiness is simulated; the phase machine cannot advance on its own.
func TestIntegrationGlance_UpgradeCycle_ExpandMigrateContract(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)
	ns := createTestNamespace(t, ctx, c)
	createGlancePrerequisites(t, ctx, c, ns)

	// Brownfield glance at release 2025.2 with spec.openStackRelease and
	// spec.image.tag in lockstep (the operator's existing bump contract).
	glance := integrationGlance("glance", ns)
	g.Expect(c.Create(ctx, glance)).To(Succeed(), "create Glance CR")
	glanceKey := types.NamespacedName{Name: "glance", Namespace: ns}

	// A default backend plus its credentials must exist before any config is
	// rendered and the schema/Deployment can converge.
	g.Expect(c.Create(ctx, integrationS3CredentialsSecret("store", ns))).To(Succeed(), "create S3 credentials Secret")
	g.Expect(c.Create(ctx, integrationBackend("store", ns, "glance", true))).To(Succeed(), "create GlanceBackend CR")
	waitForGlanceCondition(t, ctx, c, glanceKey, "BackendsReady", metav1.ConditionTrue, eventuallyTimeout)

	deployKey := client.ObjectKey{Namespace: ns, Name: "glance"}
	dbSyncKey := client.ObjectKey{Namespace: ns, Name: "glance-db-sync"}

	// Drive the initial 2025.2 install to Ready: simulate the db-sync Job complete
	// and the Deployment ready, exactly as the file's other tests do.
	g.Eventually(func() error {
		return c.Get(ctx, dbSyncKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "db-sync Job should appear")
	g.Expect(simulators.SimulateJobComplete(ctx, c, dbSyncKey)).To(Succeed(), "simulate db-sync Job completion")

	var deploy appsv1.Deployment
	g.Eventually(func() error {
		return c.Get(ctx, deployKey, &deploy)
	}, eventuallyLongTimeout, pollInterval).Should(Succeed(), "Glance Deployment should appear")
	g.Expect(simulators.SimulateDeploymentReady(ctx, c, deployKey, ptr.Deref(deploy.Spec.Replicas, 1))).
		To(Succeed(), "simulate initial Deployment readiness")

	waitForGlanceCondition(t, ctx, c, glanceKey, "DatabaseReady", metav1.ConditionTrue, eventuallyLongTimeout)
	waitForGlanceCondition(t, ctx, c, glanceKey, "Ready", metav1.ConditionTrue, eventuallyLongTimeout)

	// The initial release is tracked, no upgrade is in flight, and the Deployment
	// runs the pre-2026.1 eventlet launch (glance-api server, no uWSGI).
	initial := &glancev1alpha1.Glance{}
	g.Expect(c.Get(ctx, glanceKey, initial)).To(Succeed())
	g.Expect(initial.Status.InstalledRelease).To(Equal("2025.2"),
		"installedRelease should be 2025.2 after the initial install")
	g.Expect(string(initial.Status.UpgradePhase)).To(Equal(""),
		"no upgrade should be in flight after a fresh install")
	g.Expect(c.Get(ctx, deployKey, &deploy)).To(Succeed())
	initialCmd := deploy.Spec.Template.Spec.Containers[0].Command
	g.Expect(initialCmd).To(ContainElement("glance-api"),
		"2025.2 Deployment should run the eventlet glance-api launch")
	g.Expect(initialCmd).NotTo(ContainElement("uwsgi"),
		"2025.2 Deployment must not run the uWSGI launch")

	// --- Trigger the upgrade: bump spec.openStackRelease and spec.image.tag in
	// lockstep. The Get→Update loop retries on the conflict a concurrent status
	// write races in. ---
	g.Eventually(func() error {
		cur := &glancev1alpha1.Glance{}
		if err := c.Get(ctx, glanceKey, cur); err != nil {
			return err
		}
		cur.Spec.OpenStackRelease = "2026.1"
		cur.Spec.Image.Tag = "2026.1"
		return c.Update(ctx, cur)
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "bump glance to release 2026.1")

	// Phase 1: Expanding — the db-expand Job carries the target-release image and
	// the glance-manage db expand command.
	g.Eventually(func() commonv1.UpgradePhase {
		cur := &glancev1alpha1.Glance{}
		if err := c.Get(ctx, glanceKey, cur); err != nil {
			return ""
		}
		return cur.Status.UpgradePhase
	}, eventuallyTimeout, pollInterval).Should(Equal(commonv1.UpgradePhaseExpanding),
		"upgradePhase should transition to Expanding")

	upgrading := &glancev1alpha1.Glance{}
	g.Expect(c.Get(ctx, glanceKey, upgrading)).To(Succeed())
	g.Expect(upgrading.Status.TargetRelease).To(Equal("2026.1"),
		"targetRelease should record the in-flight upgrade")

	expandKey := client.ObjectKey{Namespace: ns, Name: "glance-db-expand"}
	g.Eventually(func() error {
		return c.Get(ctx, expandKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "db-expand Job should appear")
	expandJob := &batchv1.Job{}
	g.Expect(c.Get(ctx, expandKey, expandJob)).To(Succeed())
	g.Expect(expandJob.Spec.Template.Spec.Containers[0].Image).To(HaveSuffix(":2026.1"),
		"db-expand Job should run the target-release image")
	g.Expect(expandJob.Spec.Template.Spec.Containers[0].Command).To(ContainElements("glance-manage", "db", "expand"),
		"db-expand Job should run glance-manage db expand")
	g.Expect(simulators.SimulateJobComplete(ctx, c, expandKey)).To(Succeed(), "simulate db-expand Job completion")

	// Phase 2: Migrating — the db-migrate Job runs glance-manage db migrate.
	g.Eventually(func() commonv1.UpgradePhase {
		cur := &glancev1alpha1.Glance{}
		if err := c.Get(ctx, glanceKey, cur); err != nil {
			return ""
		}
		return cur.Status.UpgradePhase
	}, eventuallyTimeout, pollInterval).Should(Equal(commonv1.UpgradePhaseMigrating),
		"upgradePhase should transition to Migrating")

	migrateKey := client.ObjectKey{Namespace: ns, Name: "glance-db-migrate"}
	g.Eventually(func() error {
		return c.Get(ctx, migrateKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "db-migrate Job should appear")
	migrateJob := &batchv1.Job{}
	g.Expect(c.Get(ctx, migrateKey, migrateJob)).To(Succeed())
	g.Expect(migrateJob.Spec.Template.Spec.Containers[0].Command).To(ContainElements("glance-manage", "db", "migrate"),
		"db-migrate Job should run glance-manage db migrate")
	g.Expect(simulators.SimulateJobComplete(ctx, c, migrateKey)).To(Succeed(), "simulate db-migrate Job completion")

	// Phase 3: RollingUpdate — the Deployment template flips to the 2026.1 uWSGI
	// launch, which resets its simulated readiness.
	g.Eventually(func() commonv1.UpgradePhase {
		cur := &glancev1alpha1.Glance{}
		if err := c.Get(ctx, glanceKey, cur); err != nil {
			return ""
		}
		return cur.Status.UpgradePhase
	}, eventuallyTimeout, pollInterval).Should(Equal(commonv1.UpgradePhaseRollingUpdate),
		"upgradePhase should transition to RollingUpdate")

	g.Eventually(func(ig Gomega) {
		d := &appsv1.Deployment{}
		ig.Expect(c.Get(ctx, deployKey, d)).To(Succeed())
		ig.Expect(d.Spec.Template.Spec.Containers[0].Image).To(HaveSuffix(":2026.1"),
			"Deployment should carry the 2026.1 image during RollingUpdate")
		ig.Expect(d.Spec.Template.Spec.Containers[0].Command).To(ContainElement("uwsgi"),
			"Deployment should switch to the uWSGI launch on the release bump")
	}, eventuallyTimeout, pollInterval).Should(Succeed(),
		"Deployment should flip to the 2026.1 uWSGI launch during RollingUpdate")

	// Rollout-gated contract (Acceptance Criterion): while the re-imaged Deployment
	// is not ready (the template bump reset its simulated readiness), the phase
	// MUST NOT advance past RollingUpdate.
	g.Consistently(func(ig Gomega) {
		cur := &glancev1alpha1.Glance{}
		ig.Expect(c.Get(ctx, glanceKey, cur)).To(Succeed())
		ig.Expect(cur.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseRollingUpdate),
			"upgradePhase must stay RollingUpdate until the Deployment reports ready")
	}, 2*time.Second, pollInterval).Should(Succeed())

	// Simulate the re-imaged rollout completing; the flow advances to Contracting.
	var rollout appsv1.Deployment
	g.Expect(c.Get(ctx, deployKey, &rollout)).To(Succeed())
	g.Expect(simulators.SimulateDeploymentReady(ctx, c, deployKey, ptr.Deref(rollout.Spec.Replicas, 1))).
		To(Succeed(), "simulate the re-imaged Deployment rollout completing")

	// Phase 4: Contracting — the db-contract Job runs glance-manage db contract.
	g.Eventually(func() commonv1.UpgradePhase {
		cur := &glancev1alpha1.Glance{}
		if err := c.Get(ctx, glanceKey, cur); err != nil {
			return ""
		}
		return cur.Status.UpgradePhase
	}, eventuallyTimeout, pollInterval).Should(Equal(commonv1.UpgradePhaseContracting),
		"upgradePhase should transition to Contracting once the Deployment is ready")

	contractKey := client.ObjectKey{Namespace: ns, Name: "glance-db-contract"}
	g.Eventually(func() error {
		return c.Get(ctx, contractKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "db-contract Job should appear")
	contractJob := &batchv1.Job{}
	g.Expect(c.Get(ctx, contractKey, contractJob)).To(Succeed())
	g.Expect(contractJob.Spec.Template.Spec.Containers[0].Command).To(ContainElements("glance-manage", "db", "contract"),
		"db-contract Job should run glance-manage db contract")
	g.Expect(simulators.SimulateJobComplete(ctx, c, contractKey)).To(Succeed(), "simulate db-contract Job completion")

	// Upgrade completes: installedRelease promoted, phase and target cleared.
	g.Eventually(func() string {
		cur := &glancev1alpha1.Glance{}
		if err := c.Get(ctx, glanceKey, cur); err != nil {
			return ""
		}
		return cur.Status.InstalledRelease
	}, eventuallyTimeout, pollInterval).Should(Equal("2026.1"),
		"installedRelease should advance to 2026.1 after the contract phase")

	completed := &glancev1alpha1.Glance{}
	g.Expect(c.Get(ctx, glanceKey, completed)).To(Succeed())
	g.Expect(string(completed.Status.UpgradePhase)).To(Equal(""),
		"upgradePhase should be cleared once the upgrade completes")
	g.Expect(completed.Status.TargetRelease).To(Equal(""),
		"targetRelease should be cleared once the upgrade completes")

	// Post-upgrade steady state: the db-sync Job re-runs with the new image on
	// pod-spec-hash drift (its db load_metadefs step then loads the 2026.1
	// definitions). Its recreation is a delete-and-create, so the Get may miss it
	// transiently — Eventually rides that out.
	g.Eventually(func(ig Gomega) {
		j := &batchv1.Job{}
		ig.Expect(c.Get(ctx, dbSyncKey, j)).To(Succeed())
		ig.Expect(j.Spec.Template.Spec.Containers[0].Image).To(HaveSuffix(":2026.1"),
			"steady-state db-sync Job should be re-created with the 2026.1 image")
	}, eventuallyLongTimeout, pollInterval).Should(Succeed(),
		"steady-state db-sync Job should re-run with the new image")
	g.Expect(simulators.SimulateJobComplete(ctx, c, dbSyncKey)).To(Succeed(), "simulate post-upgrade db-sync completion")

	// The system returns to DatabaseReady/Ready after the full upgrade cycle. The
	// re-imaged Deployment was already simulated ready during the contract flip, so
	// no further Deployment simulation is required for the aggregate to converge.
	waitForGlanceCondition(t, ctx, c, glanceKey, "DatabaseReady", metav1.ConditionTrue, eventuallyLongTimeout)
	waitForGlanceCondition(t, ctx, c, glanceKey, "Ready", metav1.ConditionTrue, eventuallyLongTimeout)
}
