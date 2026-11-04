// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Integration tests for the Placement reconciler running against a live envtest
// API server.
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
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	commontestutil "github.com/c5c3/cobaltcore/internal/common/testutil"
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	placementv1alpha1 "github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/placement/internal/testutil"
)

// Test timeout constants for CI tuning.
const (
	// eventuallyTimeout is the default polling timeout for Eventually assertions.
	eventuallyTimeout = 30 * time.Second
	// pollInterval is the polling interval for Eventually assertions.
	pollInterval = 500 * time.Millisecond
)

// --- Shared helpers ---

// setupEnvTestWithController wraps testutil.SetupPlacementEnvTestWithController
// with the v1alpha1 scheme and the webhook + controller registration callbacks.
//
// The reconciler is built manually rather than via SetupWithManager: that method
// probes the RESTMapper for Gateway API (set explicitly here) and would trip
// controller-runtime's process-global controller-name validation across the
// sequential managers each test spins up, so the controller opts out via
// SkipNameValidation, exactly as keystone's, horizon's and glance's integration
// setups do. Each opt is applied to the PlacementReconciler right after it is
// built.
func setupEnvTestWithController(t testing.TB, opts ...func(*PlacementReconciler)) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupPlacementEnvTestWithController(
		t,
		placementv1alpha1.AddToScheme,
		func(mgr ctrl.Manager) error {
			return (&placementv1alpha1.PlacementWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
		},
		func(mgr ctrl.Manager) error {
			// The single field-index registration site, mirroring
			// PlacementReconciler.SetupWithManager.
			if err := registerPlacementIndexes(context.Background(), mgr.GetFieldIndexer()); err != nil {
				return err
			}

			r := &PlacementReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("placement-controller"),
				// A healthy stub keeps the health-check probe from firing slow HTTP
				// GETs at the unreachable Service DNS; envtest has no kubelet.
				HTTPClient: &stubDoer{status: http.StatusOK},
				// envtest loads the fake HTTPRoute CRD, so the kind is available.
				// Mirror what SetupWithManager would set from the RESTMapper.
				gatewayAPIAvailable: true,
				// The fake VerticalPodAutoscaler CRD loads into every envtest too,
				// so the VPA latch is true as it would be at startup.
				vpaAvailable: true,
			}
			for _, opt := range opts {
				opt(r)
			}
			return ctrl.NewControllerManagedBy(mgr).
				For(&placementv1alpha1.Placement{}, builder.WithPredicates(watch.CRUpdatePredicate())).
				Owns(&appsv1.Deployment{}).
				Owns(&corev1.Service{}).
				Owns(&corev1.ConfigMap{}).
				Owns(&corev1.Secret{}).
				Owns(&policyv1.PodDisruptionBudget{}).
				Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
				Owns(&vpav1.VerticalPodAutoscaler{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
				Owns(&networkingv1.NetworkPolicy{}).
				Owns(&batchv1.Job{}).
				Owns(&gatewayv1.HTTPRoute{}).
				Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(
					secretToPlacementMapper(mgr.GetClient()),
				)).
				Watches(&mariadbv1alpha1.MariaDB{}, handler.EnqueueRequestsFromMapFunc(
					mariaDBToPlacementMapper(mgr.GetClient()),
				)).
				Watches(&esov1.ClusterSecretStore{}, handler.EnqueueRequestsFromMapFunc(
					storeToPlacementMapper(mgr.GetClient(), commonv1.SecretStoreKindCluster),
				)).
				Watches(&esov1.SecretStore{}, handler.EnqueueRequestsFromMapFunc(
					storeToPlacementMapper(mgr.GetClient(), commonv1.SecretStoreKindNamespaced),
				)).
				WithOptions(controller.Options{SkipNameValidation: ptr.To(true)}).
				Complete(r)
		},
	)
}

// createTestNamespace creates a uniquely named namespace per test.
func createTestNamespace(t testing.TB, ctx context.Context, c client.Client) string {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "placement-it-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")
	return ns.Name
}

// integrationPlacement returns a valid brownfield Placement CR (spec.database.host
// set, no clusterRef) so no MariaDB cluster CR is needed for the pipeline to run.
func integrationPlacement(name, ns string) *placementv1alpha1.Placement {
	return &placementv1alpha1.Placement{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: placementv1alpha1.PlacementSpec{
			OpenStackRelease: "2026.1",
			Deployment:       placementv1alpha1.DeploymentSpec{Replicas: 1},
			Image:            commonv1.ImageSpec{Repository: "ghcr.io/c5c3/placement", Tag: "2026.1"},
			Database: commonv1.DatabaseSpec{
				Host:      "db.example.com",
				Port:      3306,
				Database:  "placement",
				SecretRef: commonv1.SecretRefSpec{Name: "placement-db"},
			},
			Cache: commonv1.CacheSpec{
				Backend: commonv1.DefaultCacheBackend,
				Servers: []string{"mc:11211"},
			},
			KeystoneEndpoint: "http://keystone.openstack.svc.cluster.local:5000/v3",
			ServiceUser: placementv1alpha1.ServiceUserSpec{
				SecretRef: commonv1.SecretRefSpec{Name: "placement-service-user", Key: "password"},
			},
		},
	}
}

// waitForPlacementCondition polls the Placement CR until the named condition
// reaches the expected status. Returns the condition.
func waitForPlacementCondition(t testing.TB, ctx context.Context, c client.Client, key types.NamespacedName, condType string, expected metav1.ConditionStatus) *metav1.Condition {
	t.Helper()
	g := NewGomegaWithT(t)

	var cond *metav1.Condition
	g.Eventually(func() metav1.ConditionStatus {
		var placement placementv1alpha1.Placement
		if err := c.Get(ctx, key, &placement); err != nil {
			return ""
		}
		cond = meta.FindStatusCondition(placement.Status.Conditions, condType)
		if cond == nil {
			return ""
		}
		return cond.Status
	}, eventuallyTimeout, pollInterval).Should(Equal(expected),
		fmt.Sprintf("Placement condition %s should reach %s", condType, expected))
	return cond
}

// createPlacementPrerequisites creates what the secrets gate of
// integrationPlacement waits on: a Ready ClusterSecretStore and the two
// credential Secrets ESO would sync. The CR is brownfield, so no MariaDB cluster
// and no Database/User/Grant CRs take part.
func createPlacementPrerequisites(t testing.TB, ctx context.Context, c client.Client, ns string) {
	t.Helper()
	g := NewGomegaWithT(t)

	store := &esov1.ClusterSecretStore{ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName}}
	if err := c.Create(ctx, store); err != nil {
		g.Expect(apierrors.IsAlreadyExists(err)).To(BeTrue(), "create ClusterSecretStore: %v", err)
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(store), store)).To(Succeed())
	}
	store.Status = esov1.SecretStoreStatus{
		Conditions: []esov1.SecretStoreStatusCondition{
			{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
		},
	}
	g.Expect(c.Status().Update(ctx, store)).To(Succeed(), "mark the ClusterSecretStore Ready")

	g.Expect(c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "placement-db", Namespace: ns},
		Data:       map[string][]byte{"username": []byte("placement"), "password": []byte("db-password")},
	})).To(Succeed(), "create the database credentials Secret")
	g.Expect(c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "placement-service-user", Namespace: ns},
		Data:       map[string][]byte{"password": []byte("service-password")},
	})).To(Succeed(), "create the service-user credentials Secret")
}

// --- Tests ---

// TestIntegrationPlacement_FinalizerLifecycle_AddAndRemove proves the finalizer
// lifecycle through the live API server: the reconciler stamps its finalizer
// before any sub-reconciler runs, and deleting the Placement releases it and
// removes the object. The CR is brownfield, so no MariaDB Database/User/Grant CR
// is live and the finalizer is released on the first deletion pass.
func TestIntegrationPlacement_FinalizerLifecycle_AddAndRemove(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)
	ns := createTestNamespace(t, ctx, c)

	placement := integrationPlacement("placement", ns)
	g.Expect(c.Create(ctx, placement)).To(Succeed(), "create Placement CR")
	key := types.NamespacedName{Name: "placement", Namespace: ns}

	g.Eventually(func() bool {
		var got placementv1alpha1.Placement
		if err := c.Get(ctx, key, &got); err != nil {
			return false
		}
		return controllerutil.ContainsFinalizer(&got, placementFinalizer)
	}, eventuallyTimeout, pollInterval).Should(BeTrue(),
		"the reconciler should install "+placementFinalizer)

	g.Expect(c.Delete(ctx, placement)).To(Succeed(), "delete Placement CR")

	g.Eventually(func() bool {
		var got placementv1alpha1.Placement
		return apierrors.IsNotFound(c.Get(ctx, key, &got))
	}, eventuallyTimeout, pollInterval).Should(BeTrue(),
		"Placement should be fully removed after the finalizer is released")
}

// TestIntegrationPlacement_WaitsOnMissingSecrets covers the conditions the
// pipeline reaches without any database infrastructure. The secrets step is the
// first in the chain, so a CR whose secret store and credential Secrets are
// absent parks there and every downstream condition stays unset, leaving the
// aggregate Ready False.
//
// It walks the two waiting states in order: with no store at all the gate
// reports SecretStoreNotReady; creating a Ready ClusterSecretStore wakes the CR
// through the store watch and moves the gate on to the missing database
// credentials.
func TestIntegrationPlacement_WaitsOnMissingSecrets(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)
	ns := createTestNamespace(t, ctx, c)

	placement := integrationPlacement("placement", ns)
	g.Expect(c.Create(ctx, placement)).To(Succeed(), "create Placement CR")
	key := types.NamespacedName{Name: "placement", Namespace: ns}

	// No ClusterSecretStore exists, so the store gate fails first.
	storeGate := waitForPlacementCondition(t, ctx, c, key, "SecretsReady", metav1.ConditionFalse)
	g.Expect(storeGate.Reason).To(Equal("SecretStoreNotReady"),
		"an absent secret store must surface as SecretStoreNotReady")

	// The aggregate is False and the status reflects the current spec.
	ready := waitForPlacementCondition(t, ctx, c, key, "Ready", metav1.ConditionFalse)
	g.Expect(ready.Reason).To(Equal("NotAllReady"))

	var waiting placementv1alpha1.Placement
	g.Expect(c.Get(ctx, key, &waiting)).To(Succeed())
	g.Expect(waiting.Status.ObservedGeneration).To(Equal(waiting.Generation),
		"observedGeneration must be stamped with the reconciled generation")
	// The steps after Secrets never ran, so they set no conditions at all.
	g.Expect(meta.FindStatusCondition(waiting.Status.Conditions, "DatabaseReady")).To(BeNil(),
		"the database step must not run while the secrets gate is closed")
	g.Expect(meta.FindStatusCondition(waiting.Status.Conditions, "DeploymentReady")).To(BeNil(),
		"the deployment step must not run while the secrets gate is closed")

	// Opening the store gate moves the wait on to the credential Secrets, which
	// are still absent.
	store := &esov1.ClusterSecretStore{ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName}}
	g.Expect(c.Create(ctx, store)).To(Succeed(), "create ClusterSecretStore")
	store.Status = esov1.SecretStoreStatus{
		Conditions: []esov1.SecretStoreStatusCondition{
			{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
		},
	}
	g.Expect(c.Status().Update(ctx, store)).To(Succeed(), "mark the ClusterSecretStore Ready")

	g.Eventually(func() string {
		var got placementv1alpha1.Placement
		if err := c.Get(ctx, key, &got); err != nil {
			return ""
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, "SecretsReady")
		if cond == nil {
			return ""
		}
		return cond.Reason
	}, eventuallyTimeout, pollInterval).Should(Equal("WaitingForDBCredentials"),
		"with the store Ready the gate should wait on the database credentials Secret")
}

// TestIntegrationPlacement_ReachesReadyAndReprobes drives a brownfield Placement
// through the whole pipeline to Ready=True. On the settled CR it then stops the
// API answering without writing any object and sees PlacementAPIReady and Ready
// turn False, then True again.
//
// envtest runs no Job or Deployment controller, so the db-sync completion and
// the Deployment rollout are simulated.
func TestIntegrationPlacement_ReachesReadyAndReprobes(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	doer := commontestutil.NewSwitchableDoer(http.StatusOK)
	c, ctx, _ := setupEnvTestWithController(t, func(r *PlacementReconciler) { r.HTTPClient = doer })
	ns := createTestNamespace(t, ctx, c)
	createPlacementPrerequisites(t, ctx, c, ns)

	g.Expect(c.Create(ctx, integrationPlacement("placement", ns))).To(Succeed(), "create Placement CR")
	key := types.NamespacedName{Name: "placement", Namespace: ns}

	waitForPlacementCondition(t, ctx, c, key, "SecretsReady", metav1.ConditionTrue)

	dbSyncKey := client.ObjectKey{Namespace: ns, Name: "placement-db-sync"}
	g.Eventually(func() error {
		return c.Get(ctx, dbSyncKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "db-sync Job should appear")
	g.Expect(simulators.SimulateJobComplete(ctx, c, dbSyncKey)).To(Succeed(), "simulate db-sync Job completion")
	waitForPlacementCondition(t, ctx, c, key, "DatabaseReady", metav1.ConditionTrue)

	deployKey := client.ObjectKey{Namespace: ns, Name: "placement"}
	var deploy appsv1.Deployment
	g.Eventually(func() error {
		return c.Get(ctx, deployKey, &deploy)
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "Placement Deployment should appear")
	g.Expect(simulators.SimulateDeploymentReady(ctx, c, deployKey, ptr.Deref(deploy.Spec.Replicas, 1))).
		To(Succeed(), "simulate Deployment readiness")
	waitForPlacementCondition(t, ctx, c, key, "DeploymentReady", metav1.ConditionTrue)
	waitForPlacementCondition(t, ctx, c, key, conditionTypePlacementAPIReady, metav1.ConditionTrue)
	waitForPlacementCondition(t, ctx, c, key, "Ready", metav1.ConditionTrue)

	var ready placementv1alpha1.Placement
	g.Expect(c.Get(ctx, key, &ready)).To(Succeed())
	g.Expect(ready.Status.InstalledRelease).To(Equal("2026.1"),
		"installedRelease should be promoted after the db-sync")
	g.Expect(ready.Status.Endpoint).NotTo(BeEmpty(),
		"the endpoint should be advertised once the workload is available")

	commontestutil.ExpectReprobeTracksAPI(t, doer, func(g Gomega) []metav1.Condition {
		var got placementv1alpha1.Placement
		g.Expect(c.Get(ctx, key, &got)).To(Succeed())
		return got.Status.Conditions
	}, conditionTypePlacementAPIReady, pollInterval)
}
