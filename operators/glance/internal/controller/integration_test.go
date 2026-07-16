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

	"github.com/c5c3/forge/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	"github.com/c5c3/forge/internal/common/watch"
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
	"github.com/c5c3/forge/operators/glance/internal/testutil"
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
