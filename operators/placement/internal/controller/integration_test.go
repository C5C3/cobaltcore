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
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

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
// setups do.
func setupEnvTestWithController(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
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
			}
			return ctrl.NewControllerManagedBy(mgr).
				For(&placementv1alpha1.Placement{}, builder.WithPredicates(watch.CRUpdatePredicate())).
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
