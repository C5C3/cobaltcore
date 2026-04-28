// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Package controller contains integration tests for the Keystone reconciler (CC-0014, F002).
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/forge/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
	"github.com/c5c3/forge/operators/keystone/internal/testutil"
)

// Feature: CC-0014

// Test timeout constants for CI tuning (CC-0014).
const (
	// eventuallyTimeout is the default polling timeout for Eventually assertions.
	eventuallyTimeout = 30 * time.Second
	// eventuallyLongTimeout is used for MariaDB User/Grant CR polling, which
	// depends on the controller's RequeueDatabaseWait delay to discover readiness
	// changes on unwatched MariaDB types (CC-0044).
	eventuallyLongTimeout = 2 * RequeueDatabaseWait
	// pollInterval is the polling interval for Eventually assertions.
	pollInterval = 500 * time.Millisecond
)

// --- Shared Helpers ---

// setupEnvTestWithController wraps testutil.SetupKeystoneEnvTestWithController with
// the v1alpha1 scheme, webhook, and controller registration callbacks (CC-0014).
func setupEnvTestWithController(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupKeystoneEnvTestWithController(t,
		keystonev1alpha1.AddToScheme,
		func(mgr ctrl.Manager) error {
			return (&keystonev1alpha1.KeystoneWebhook{Client: mgr.GetClient()}).SetupWebhookWithManager(mgr)
		},
		func(mgr ctrl.Manager) error {
			r := &KeystoneReconciler{
				Client:     mgr.GetClient(),
				Scheme:     mgr.GetScheme(),
				Recorder:   mgr.GetEventRecorderFor("keystone-controller"),
				HTTPClient: testHealthyHTTPClient(),
				// envtest loads the fake HTTPRoute CRD from internal/common/testutil/fake_crds/gateway-api,
				// so the Gateway API kind is available to the reconciler. Mirror
				// what SetupWithManager would set from the RESTMapper at startup
				// (CC-0065).
				gatewayAPIAvailable: true,
			}
			// Register the Keystone field indexer so secretToKeystoneMapper's
			// MatchingFields lookup works in integration tests, mirroring what
			// SetupWithManager does in production. Using context.Background()
			// because the envtest context is not yet available at registration
			// time — same pattern as keystone_controller.go:525 (CC-0087, REQ-008).
			if err := registerSecretNameIndex(context.Background(), mgr.GetFieldIndexer()); err != nil {
				return err
			}
			return ctrl.NewControllerManagedBy(mgr).
				For(&keystonev1alpha1.Keystone{}).
				Owns(&appsv1.Deployment{}).
				Owns(&corev1.Service{}).
				Owns(&corev1.ConfigMap{}).
				Owns(&batchv1.Job{}).
				Owns(&policyv1.PodDisruptionBudget{}).
				Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
				Owns(&gatewayv1.HTTPRoute{}).
				Owns(&batchv1.CronJob{}).
				Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(
					secretToKeystoneMapper(mgr.GetClient()),
				)).
				WithOptions(controller.Options{SkipNameValidation: ptr.To(true)}).
				Complete(r)
		},
	)
}

// integrationBrownfieldKeystone returns a valid Keystone CR for brownfield mode integration
// tests (spec.database.host set, no clusterRef) (CC-0014).
func integrationBrownfieldKeystone(name, namespace string) *keystonev1alpha1.Keystone {
	return &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: keystonev1alpha1.KeystoneSpec{
			Replicas: 3,
			Image:    commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
			Database: commonv1.DatabaseSpec{
				Host:      "db.example.com",
				Port:      3306,
				Database:  "keystone",
				SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"},
			},
			Cache: commonv1.CacheSpec{
				Backend: "dogpile.cache.pymemcache",
				Servers: []string{"mc:11211"},
			},
			Fernet: keystonev1alpha1.FernetSpec{
				RotationSchedule: "0 0 * * 0",
				MaxActiveKeys:    3,
			},
			Bootstrap: keystonev1alpha1.BootstrapSpec{
				AdminUser:              "admin",
				AdminPasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin"},
				Region:                 "RegionOne",
			},
		},
	}
}

// integrationManagedKeystone returns a valid Keystone CR for managed mode integration tests
// (spec.database.clusterRef set, no host) (CC-0014).
func integrationManagedKeystone(name, namespace string) *keystonev1alpha1.Keystone {
	return &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: keystonev1alpha1.KeystoneSpec{
			Replicas: 3,
			Image:    commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
			Database: commonv1.DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"},
				Port:       3306,
				Database:   "keystone",
				SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
			},
			Cache: commonv1.CacheSpec{
				Backend: "dogpile.cache.pymemcache",
				Servers: []string{"mc:11211"},
			},
			Fernet: keystonev1alpha1.FernetSpec{
				RotationSchedule: "0 0 * * 0",
				MaxActiveKeys:    3,
			},
			Bootstrap: keystonev1alpha1.BootstrapSpec{
				AdminUser:              "admin",
				AdminPasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin"},
				Region:                 "RegionOne",
			},
		},
	}
}

// ensureReadyClusterSecretStore creates or refreshes the OpenBao-backed
// ClusterSecretStore with a Ready=True condition. reconcileSecrets now gates
// on this status (CC-0047); without it every integration test would flip to
// SecretsReady=False with reason SecretStoreNotReady. Safe to call multiple
// times across namespaces since ClusterSecretStore is cluster-scoped.
func ensureReadyClusterSecretStore(t testing.TB, ctx context.Context, c client.Client) {
	t.Helper()
	g := NewGomegaWithT(t)

	store := &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "openbao-cluster-store"},
	}
	err := c.Get(ctx, client.ObjectKeyFromObject(store), store)
	if apierrors.IsNotFound(err) {
		g.Expect(c.Create(ctx, store)).To(Succeed(), "create ClusterSecretStore")
	} else {
		g.Expect(err).NotTo(HaveOccurred(), "get ClusterSecretStore")
	}

	store.Status = esov1.SecretStoreStatus{
		Conditions: []esov1.SecretStoreStatusCondition{
			{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
		},
	}
	g.Expect(c.Status().Update(ctx, store)).To(Succeed(), "update ClusterSecretStore status")
}

// createPrerequisites creates the ExternalSecret and Secret resources that the
// Keystone reconciler expects to find. It creates the DB credentials ExternalSecret
// and Secret (username+password), the admin credentials ExternalSecret and Secret
// (password), and calls SimulateExternalSecretSync for both (CC-0014).
func createPrerequisites(t testing.TB, ctx context.Context, c client.Client, ns string) {
	t.Helper()
	g := NewGomegaWithT(t)

	// Ensure the OpenBao-backed ClusterSecretStore reports Ready=True so
	// reconcileSecrets proceeds past the store gate (CC-0047).
	ensureReadyClusterSecretStore(t, ctx, c)

	// Create DB credentials ExternalSecret and Secret.
	dbES := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-db", Namespace: ns},
		Spec: esov1.ExternalSecretSpec{
			SecretStoreRef: esov1.SecretStoreRef{
				Kind: "ClusterSecretStore",
				Name: "openbao-cluster-store",
			},
			Target: esov1.ExternalSecretTarget{
				Name: "keystone-db",
			},
		},
	}
	g.Expect(c.Create(ctx, dbES)).To(Succeed(), "create DB ExternalSecret")

	dbSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-db", Namespace: ns},
		Data: map[string][]byte{
			"username": []byte("keystone"),
			"password": []byte("secret"),
		},
	}
	g.Expect(c.Create(ctx, dbSecret)).To(Succeed(), "create DB Secret")

	// Create admin credentials ExternalSecret and Secret.
	adminES := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-admin", Namespace: ns},
		Spec: esov1.ExternalSecretSpec{
			SecretStoreRef: esov1.SecretStoreRef{
				Kind: "ClusterSecretStore",
				Name: "openbao-cluster-store",
			},
			Target: esov1.ExternalSecretTarget{
				Name: "keystone-admin",
			},
		},
	}
	g.Expect(c.Create(ctx, adminES)).To(Succeed(), "create admin ExternalSecret")

	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-admin", Namespace: ns},
		Data:       map[string][]byte{"password": []byte("admin-password")},
	}
	g.Expect(c.Create(ctx, adminSecret)).To(Succeed(), "create admin Secret")

	// Simulate ESO sync for both ExternalSecrets.
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c, client.ObjectKey{Namespace: ns, Name: "keystone-db"})).
		To(Succeed(), "simulate DB ExternalSecret sync")
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c, client.ObjectKey{Namespace: ns, Name: "keystone-admin"})).
		To(Succeed(), "simulate admin ExternalSecret sync")
}

// waitForCondition polls the Keystone CR until the named condition reaches the
// expected status, or the timeout is reached. Returns the condition.
func waitForCondition(t testing.TB, ctx context.Context, c client.Client, key types.NamespacedName, condType string, expectedStatus metav1.ConditionStatus, timeout time.Duration) *metav1.Condition {
	t.Helper()
	g := NewGomegaWithT(t)

	var cond *metav1.Condition
	g.Eventually(func() metav1.ConditionStatus {
		ks := &keystonev1alpha1.Keystone{}
		if err := c.Get(ctx, key, ks); err != nil {
			return ""
		}
		cond = meta.FindStatusCondition(ks.Status.Conditions, condType)
		if cond == nil {
			return ""
		}
		return cond.Status
	}, timeout, pollInterval).Should(Equal(expectedStatus),
		fmt.Sprintf("condition %s should reach %s", condType, expectedStatus))

	return cond
}

// driveFullReconciliation simulates external dependencies to drive the
// reconciler through all phases to Ready=True. It waits for each phase's
// resources to appear before simulating their readiness (CC-0014).
func driveFullReconciliation(t testing.TB, ctx context.Context, c client.Client, ksName, ns string) {
	t.Helper()
	g := NewGomegaWithT(t)

	key := types.NamespacedName{Name: ksName, Namespace: ns}

	// Wait for SecretsReady=True (prerequisites already created).
	waitForCondition(t, ctx, c, key, "SecretsReady", metav1.ConditionTrue, eventuallyTimeout)

	// Wait for FernetKeysReady=True (reconciler creates fernet keys automatically).
	waitForCondition(t, ctx, c, key, "FernetKeysReady", metav1.ConditionTrue, eventuallyTimeout)

	// Wait for the db-sync Job to appear and simulate its completion.
	dbSyncKey := client.ObjectKey{Namespace: ns, Name: fmt.Sprintf("%s-db-sync", ksName)}
	g.Eventually(func() error {
		return c.Get(ctx, dbSyncKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "db-sync Job should appear")
	g.Expect(simulators.SimulateJobComplete(ctx, c, dbSyncKey)).To(Succeed(), "simulate db-sync Job completion")

	// Wait for the schema-check Job to appear and simulate its completion (CC-0064).
	schemaCheckKey := client.ObjectKey{Namespace: ns, Name: fmt.Sprintf("%s-schema-check", ksName)}
	g.Eventually(func() error {
		return c.Get(ctx, schemaCheckKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "schema-check Job should appear")
	g.Expect(simulators.SimulateJobComplete(ctx, c, schemaCheckKey)).To(Succeed(), "simulate schema-check Job completion")

	// Wait for DatabaseReady=True.
	waitForCondition(t, ctx, c, key, "DatabaseReady", metav1.ConditionTrue, eventuallyTimeout)

	// Wait for the Deployment to appear and simulate its readiness.
	deployKey := client.ObjectKey{Namespace: ns, Name: ksName}
	deploy := &appsv1.Deployment{}
	g.Eventually(func() error {
		return c.Get(ctx, deployKey, deploy)
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "Deployment should appear")
	g.Expect(simulators.SimulateDeploymentReady(ctx, c, deployKey, ptr.Deref(deploy.Spec.Replicas, 1))).To(Succeed(), "simulate Deployment ready")

	// Wait for DeploymentReady=True.
	waitForCondition(t, ctx, c, key, "DeploymentReady", metav1.ConditionTrue, eventuallyTimeout)

	// Wait for the bootstrap Job to appear and simulate its completion.
	bootstrapKey := client.ObjectKey{Namespace: ns, Name: fmt.Sprintf("%s-bootstrap", ksName)}
	g.Eventually(func() error {
		return c.Get(ctx, bootstrapKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "bootstrap Job should appear")
	g.Expect(simulators.SimulateJobComplete(ctx, c, bootstrapKey)).To(Succeed(), "simulate bootstrap Job completion")

	// Wait for BootstrapReady=True and then Ready=True.
	waitForCondition(t, ctx, c, key, "BootstrapReady", metav1.ConditionTrue, eventuallyTimeout)
	waitForCondition(t, ctx, c, key, "Ready", metav1.ConditionTrue, eventuallyTimeout)
}

// --- Task 2.1: Full reconcile brownfield test (REQ-003, REQ-005, REQ-006, REQ-007) ---

func TestIntegration_FullReconcile_Brownfield(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	// Create isolated namespace.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-brownfield-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	// Create prerequisites.
	createPrerequisites(t, ctx, c, ns.Name)

	// Create brownfield Keystone CR.
	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	// Drive the full reconciliation to Ready=True.
	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	// Fetch the final state.
	key := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}
	updated := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())

	// Verify all 7 conditions are True.
	for _, condType := range []string{"SecretsReady", "FernetKeysReady", "DatabaseReady", "DeploymentReady", "HPAReady", "BootstrapReady", "Ready"} {
		cond := meta.FindStatusCondition(updated.Status.Conditions, condType)
		g.Expect(cond).NotTo(BeNil(), "condition %s should exist", condType)
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "condition %s should be True", condType)
	}

	// Verify Ready condition has reason AllReady (REQ-003).
	readyCond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
	g.Expect(readyCond.Reason).To(Equal("AllReady"))

	// Verify status.endpoint (REQ-006).
	expectedEndpoint := fmt.Sprintf("http://%s.%s.svc.cluster.local:5000/v3", ks.Name, ns.Name)
	g.Expect(updated.Status.Endpoint).To(Equal(expectedEndpoint), "status.endpoint should be set correctly")

	// Verify ObservedGeneration on all conditions (REQ-007).
	for _, cond := range updated.Status.Conditions {
		g.Expect(cond.ObservedGeneration).To(Equal(updated.Generation),
			"condition %s ObservedGeneration should match CR generation", cond.Type)
	}
}

// --- Task 2.2: Condition progression test (REQ-008) ---

func TestIntegration_ConditionProgression(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-progression-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	// Ensure the ClusterSecretStore gate is open so this test still exercises
	// the per-ExternalSecret Ready progression rather than short-circuiting on
	// SecretStoreNotReady (CC-0047).
	ensureReadyClusterSecretStore(t, ctx, c)

	// Phase 0: Create ExternalSecrets and Secrets but do NOT simulate sync yet.
	// The reconciler should see SecretsReady=False because ESO hasn't set the
	// Ready condition on the ExternalSecret.
	dbES := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-db", Namespace: ns.Name},
		Spec: esov1.ExternalSecretSpec{
			SecretStoreRef: esov1.SecretStoreRef{Kind: "ClusterSecretStore", Name: "openbao-cluster-store"},
			Target:         esov1.ExternalSecretTarget{Name: "keystone-db"},
		},
	}
	g.Expect(c.Create(ctx, dbES)).To(Succeed())

	dbSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-db", Namespace: ns.Name},
		Data: map[string][]byte{
			"username": []byte("keystone"),
			"password": []byte("secret"),
		},
	}
	g.Expect(c.Create(ctx, dbSecret)).To(Succeed())

	adminES := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-admin", Namespace: ns.Name},
		Spec: esov1.ExternalSecretSpec{
			SecretStoreRef: esov1.SecretStoreRef{Kind: "ClusterSecretStore", Name: "openbao-cluster-store"},
			Target:         esov1.ExternalSecretTarget{Name: "keystone-admin"},
		},
	}
	g.Expect(c.Create(ctx, adminES)).To(Succeed())

	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-admin", Namespace: ns.Name},
		Data:       map[string][]byte{"password": []byte("admin-password")},
	}
	g.Expect(c.Create(ctx, adminSecret)).To(Succeed())

	// Create the Keystone CR.
	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())
	key := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}

	// Phase 1: SecretsReady should be False; later conditions absent.
	waitForCondition(t, ctx, c, key, "SecretsReady", metav1.ConditionFalse, eventuallyTimeout)

	g.Consistently(func(ig Gomega) {
		ksState := &keystonev1alpha1.Keystone{}
		ig.Expect(c.Get(ctx, key, ksState)).To(Succeed())
		ig.Expect(meta.FindStatusCondition(ksState.Status.Conditions, "FernetKeysReady")).To(BeNil(),
			"FernetKeysReady should be absent when SecretsReady is False")
		ig.Expect(meta.FindStatusCondition(ksState.Status.Conditions, "DatabaseReady")).To(BeNil(),
			"DatabaseReady should be absent when SecretsReady is False")
		ig.Expect(meta.FindStatusCondition(ksState.Status.Conditions, "DeploymentReady")).To(BeNil(),
			"DeploymentReady should be absent when SecretsReady is False")
		ig.Expect(meta.FindStatusCondition(ksState.Status.Conditions, "HPAReady")).To(BeNil(),
			"HPAReady should be absent when SecretsReady is False")
		ig.Expect(meta.FindStatusCondition(ksState.Status.Conditions, "BootstrapReady")).To(BeNil(),
			"BootstrapReady should be absent when SecretsReady is False")
	}, 2*time.Second, pollInterval).Should(Succeed())

	// Phase 2: Simulate ESO sync → SecretsReady=True, FernetKeysReady=True.
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c, client.ObjectKey{Namespace: ns.Name, Name: "keystone-db"})).
		To(Succeed())
	g.Expect(simulators.SimulateExternalSecretSync(ctx, c, client.ObjectKey{Namespace: ns.Name, Name: "keystone-admin"})).
		To(Succeed())

	waitForCondition(t, ctx, c, key, "SecretsReady", metav1.ConditionTrue, eventuallyTimeout)
	waitForCondition(t, ctx, c, key, "FernetKeysReady", metav1.ConditionTrue, eventuallyTimeout)

	// DatabaseReady should appear as False (DBSyncInProgress).
	dbCond := waitForCondition(t, ctx, c, key, "DatabaseReady", metav1.ConditionFalse, eventuallyTimeout)
	g.Expect(dbCond.Reason).To(Equal("DBSyncInProgress"), "DatabaseReady reason should be DBSyncInProgress")

	// Phase 3: Simulate db-sync completion → schema-check → DatabaseReady=True.
	dbSyncKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-db-sync", ks.Name)}
	g.Eventually(func() error {
		return c.Get(ctx, dbSyncKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed())
	g.Expect(simulators.SimulateJobComplete(ctx, c, dbSyncKey)).To(Succeed())

	// Wait for schema-check Job and simulate completion (CC-0064).
	schemaCheckKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-schema-check", ks.Name)}
	g.Eventually(func() error {
		return c.Get(ctx, schemaCheckKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed())
	g.Expect(simulators.SimulateJobComplete(ctx, c, schemaCheckKey)).To(Succeed())

	waitForCondition(t, ctx, c, key, "DatabaseReady", metav1.ConditionTrue, eventuallyTimeout)

	// DeploymentReady should appear as False (WaitingForDeployment).
	deployCond := waitForCondition(t, ctx, c, key, "DeploymentReady", metav1.ConditionFalse, eventuallyTimeout)
	g.Expect(deployCond.Reason).To(Equal("WaitingForDeployment"), "DeploymentReady reason should be WaitingForDeployment")

	// Phase 4: Simulate Deployment ready → DeploymentReady=True.
	deployKey := client.ObjectKey{Namespace: ns.Name, Name: ks.Name}
	deploy := &appsv1.Deployment{}
	g.Eventually(func() error {
		return c.Get(ctx, deployKey, deploy)
	}, eventuallyTimeout, pollInterval).Should(Succeed())
	g.Expect(simulators.SimulateDeploymentReady(ctx, c, deployKey, ptr.Deref(deploy.Spec.Replicas, 1))).To(Succeed())

	waitForCondition(t, ctx, c, key, "DeploymentReady", metav1.ConditionTrue, eventuallyTimeout)

	// HPAReady should be True with reason HPANotRequired (no autoscaling configured, CC-0038).
	hpaCond := waitForCondition(t, ctx, c, key, "HPAReady", metav1.ConditionTrue, eventuallyTimeout)
	g.Expect(hpaCond.Reason).To(Equal("HPANotRequired"), "HPAReady reason should be HPANotRequired when autoscaling is nil")

	// BootstrapReady should appear as False (BootstrapInProgress).
	bootstrapCond := waitForCondition(t, ctx, c, key, "BootstrapReady", metav1.ConditionFalse, eventuallyTimeout)
	g.Expect(bootstrapCond.Reason).To(Equal("BootstrapInProgress"), "BootstrapReady reason should be BootstrapInProgress")

	// Ready should NOT be True while BootstrapReady is False. Using
	// meta.IsStatusConditionTrue handles both the nil case (condition absent)
	// and the present-but-False case unconditionally (CC-0014).
	g.Consistently(func(ig Gomega) {
		ksState := &keystonev1alpha1.Keystone{}
		ig.Expect(c.Get(ctx, key, ksState)).To(Succeed())
		ig.Expect(meta.IsStatusConditionTrue(ksState.Status.Conditions, "Ready")).To(BeFalse(),
			"Ready condition should not be True while BootstrapReady is False")
	}, 2*time.Second, pollInterval).Should(Succeed())

	// Phase 5: Simulate bootstrap completion → BootstrapReady=True, Ready=True.
	bootstrapKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-bootstrap", ks.Name)}
	g.Eventually(func() error {
		return c.Get(ctx, bootstrapKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed())
	g.Expect(simulators.SimulateJobComplete(ctx, c, bootstrapKey)).To(Succeed())

	waitForCondition(t, ctx, c, key, "BootstrapReady", metav1.ConditionTrue, eventuallyTimeout)
	readyFinal := waitForCondition(t, ctx, c, key, "Ready", metav1.ConditionTrue, eventuallyTimeout)
	g.Expect(readyFinal.Reason).To(Equal("AllReady"))
}

// --- Task 2.3: Resource creation, status endpoint, and observed generation tests (REQ-005, REQ-006, REQ-007) ---

func TestIntegration_ResourceCreation(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-resources-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	// Verify all child resources exist (REQ-005).

	// Deployment.
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone"}, &appsv1.Deployment{})).
		To(Succeed(), "Deployment test-keystone should exist")

	// Service.
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone"}, &corev1.Service{})).
		To(Succeed(), "Service test-keystone should exist")

	// ConfigMap (immutable, hashed name: test-keystone-config-{hash}).
	configMaps := &corev1.ConfigMapList{}
	g.Expect(c.List(ctx, configMaps, client.InNamespace(ns.Name))).To(Succeed())
	var matchingCMs []corev1.ConfigMap
	for _, cm := range configMaps.Items {
		if strings.HasPrefix(cm.Name, "test-keystone-config-") {
			matchingCMs = append(matchingCMs, cm)
		}
	}
	g.Expect(matchingCMs).To(HaveLen(1), "expected exactly one immutable ConfigMap with prefix test-keystone-config-")
	g.Expect(matchingCMs[0].Immutable).NotTo(BeNil(), "ConfigMap should be immutable")
	g.Expect(*matchingCMs[0].Immutable).To(BeTrue(), "ConfigMap should be immutable")

	// Jobs.
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-db-sync"}, &batchv1.Job{})).
		To(Succeed(), "Job test-keystone-db-sync should exist")
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-bootstrap"}, &batchv1.Job{})).
		To(Succeed(), "Job test-keystone-bootstrap should exist")

	// CronJob.
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-fernet-rotate"}, &batchv1.CronJob{})).
		To(Succeed(), "CronJob test-keystone-fernet-rotate should exist")

	// Secret (fernet keys).
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-fernet-keys"}, &corev1.Secret{})).
		To(Succeed(), "Secret test-keystone-fernet-keys should exist")

	// RBAC resources.
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-fernet-rotate"}, &corev1.ServiceAccount{})).
		To(Succeed(), "ServiceAccount test-keystone-fernet-rotate should exist")
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-fernet-rotate"}, &rbacv1.Role{})).
		To(Succeed(), "Role test-keystone-fernet-rotate should exist")
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-fernet-rotate"}, &rbacv1.RoleBinding{})).
		To(Succeed(), "RoleBinding test-keystone-fernet-rotate should exist")

	// PushSecret.
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-fernet-keys-backup"}, &esov1alpha1.PushSecret{})).
		To(Succeed(), "PushSecret test-keystone-fernet-keys-backup should exist")

	// PodDisruptionBudget (CC-0037).
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone"}, &policyv1.PodDisruptionBudget{})).
		To(Succeed(), "PodDisruptionBudget test-keystone should exist")
}

func TestIntegration_StatusEndpoint(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-endpoint-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	// Verify status.endpoint (REQ-006).
	updated := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: ks.Name, Namespace: ns.Name}, updated)).To(Succeed())

	expectedEndpoint := fmt.Sprintf("http://%s.%s.svc.cluster.local:5000/v3", ks.Name, ns.Name)
	g.Expect(updated.Status.Endpoint).To(Equal(expectedEndpoint), "status.endpoint should match expected format")
}

func TestIntegration_ObservedGeneration(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-obsgen-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	// Verify ObservedGeneration on all conditions (REQ-007).
	updated := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: ks.Name, Namespace: ns.Name}, updated)).To(Succeed())

	for _, cond := range updated.Status.Conditions {
		g.Expect(cond.ObservedGeneration).To(Equal(updated.Generation),
			"condition %s ObservedGeneration should match CR generation", cond.Type)
	}
}

// --- Task 2.4: Full reconcile managed mode test (REQ-004) ---

func TestIntegration_FullReconcile_Managed(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-managed-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	// Create prerequisites (same as brownfield — secrets are still needed).
	createPrerequisites(t, ctx, c, ns.Name)

	// Create a ready MariaDB cluster CR so the reconciler's cluster health
	// check passes (CC-0047).
	mdbCluster := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "mariadb", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, mdbCluster)).To(Succeed(), "create MariaDB cluster CR")
	g.Expect(simulators.SimulateMariaDBReady(ctx, c, client.ObjectKey{Namespace: ns.Name, Name: "mariadb"}, 1)).
		To(Succeed(), "simulate MariaDB cluster ready")

	// Create managed-mode Keystone CR (uses spec.database.clusterRef).
	ks := integrationManagedKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	key := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}

	// Wait for SecretsReady=True.
	waitForCondition(t, ctx, c, key, "SecretsReady", metav1.ConditionTrue, eventuallyTimeout)

	// Wait for FernetKeysReady=True.
	waitForCondition(t, ctx, c, key, "FernetKeysReady", metav1.ConditionTrue, eventuallyTimeout)

	// In managed mode, the reconciler creates MariaDB CRs sequentially:
	// Database first, then User and Grant only after Database is ready.

	// Wait for Database CR to appear and verify DatabaseReady=False.
	dbKey := client.ObjectKey{Namespace: ns.Name, Name: ks.Name}
	g.Eventually(func() error {
		return c.Get(ctx, dbKey, &mariadbv1alpha1.Database{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "MariaDB Database CR should appear")

	dbReadyCond := waitForCondition(t, ctx, c, key, "DatabaseReady", metav1.ConditionFalse, eventuallyTimeout)
	g.Expect(dbReadyCond.Reason).To(Equal("WaitingForDatabase"))

	// Simulate Database ready — this unblocks User/Grant creation.
	g.Expect(simulators.SimulateDatabaseReady(ctx, c, dbKey)).To(Succeed(), "simulate Database ready")

	// The controller watches the MariaDB cluster CR but not the Database,
	// User, or Grant CRs, so it relies on RequeueDatabaseWait to discover
	// their readiness changes. The reconciler creates User only after
	// Database is ready, and Grant only after User is ready, so we must
	// simulate each sequentially.

	// Wait for User CR to appear, then simulate User ready.
	userKey := client.ObjectKey{Namespace: ns.Name, Name: ks.Name}
	g.Eventually(func() error {
		return c.Get(ctx, userKey, &mariadbv1alpha1.User{})
	}, eventuallyLongTimeout, pollInterval).Should(Succeed(), "MariaDB User CR should appear")
	g.Expect(simulators.SimulateUserReady(ctx, c, userKey)).To(Succeed(), "simulate User ready")

	// Wait for Grant CR to appear (created after User is ready), then simulate Grant ready.
	grantKey := client.ObjectKey{Namespace: ns.Name, Name: ks.Name}
	g.Eventually(func() error {
		return c.Get(ctx, grantKey, &mariadbv1alpha1.Grant{})
	}, eventuallyLongTimeout, pollInterval).Should(Succeed(), "MariaDB Grant CR should appear")
	g.Expect(simulators.SimulateGrantReady(ctx, c, grantKey)).To(Succeed(), "simulate Grant ready")

	// Wait for the db-sync Job to appear and simulate its completion.
	// Uses eventuallyLongTimeout because the reconciler relies on RequeueDatabaseWait
	// to discover MariaDB readiness changes; after Grant becomes ready the next
	// reconciliation may be up to RequeueDatabaseWait away.
	dbSyncKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-db-sync", ks.Name)}
	g.Eventually(func() error {
		return c.Get(ctx, dbSyncKey, &batchv1.Job{})
	}, eventuallyLongTimeout, pollInterval).Should(Succeed(), "db-sync Job should appear")
	g.Expect(simulators.SimulateJobComplete(ctx, c, dbSyncKey)).To(Succeed())

	// Wait for schema-check Job and simulate completion (CC-0064).
	schemaCheckKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-schema-check", ks.Name)}
	g.Eventually(func() error {
		return c.Get(ctx, schemaCheckKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "schema-check Job should appear")
	g.Expect(simulators.SimulateJobComplete(ctx, c, schemaCheckKey)).To(Succeed())

	// Wait for DatabaseReady=True.
	waitForCondition(t, ctx, c, key, "DatabaseReady", metav1.ConditionTrue, eventuallyTimeout)

	// Wait for Deployment and simulate readiness.
	deployKey := client.ObjectKey{Namespace: ns.Name, Name: ks.Name}
	deploy := &appsv1.Deployment{}
	g.Eventually(func() error {
		return c.Get(ctx, deployKey, deploy)
	}, eventuallyTimeout, pollInterval).Should(Succeed())
	g.Expect(simulators.SimulateDeploymentReady(ctx, c, deployKey, ptr.Deref(deploy.Spec.Replicas, 1))).To(Succeed())

	waitForCondition(t, ctx, c, key, "DeploymentReady", metav1.ConditionTrue, eventuallyTimeout)

	// Wait for bootstrap Job and simulate completion.
	bootstrapKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-bootstrap", ks.Name)}
	g.Eventually(func() error {
		return c.Get(ctx, bootstrapKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed())
	g.Expect(simulators.SimulateJobComplete(ctx, c, bootstrapKey)).To(Succeed())

	waitForCondition(t, ctx, c, key, "BootstrapReady", metav1.ConditionTrue, eventuallyTimeout)
	waitForCondition(t, ctx, c, key, "Ready", metav1.ConditionTrue, eventuallyTimeout)

	// Fetch final state and verify.
	updated := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())

	// All 7 conditions should be True.
	for _, condType := range []string{"SecretsReady", "FernetKeysReady", "DatabaseReady", "DeploymentReady", "HPAReady", "BootstrapReady", "Ready"} {
		cond := meta.FindStatusCondition(updated.Status.Conditions, condType)
		g.Expect(cond).NotTo(BeNil(), "condition %s should exist", condType)
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "condition %s should be True", condType)
	}

	// Ready reason should be AllReady.
	readyCond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
	g.Expect(readyCond.Reason).To(Equal("AllReady"))

	// Status endpoint should be set.
	expectedEndpoint := fmt.Sprintf("http://%s.%s.svc.cluster.local:5000/v3", ks.Name, ns.Name)
	g.Expect(updated.Status.Endpoint).To(Equal(expectedEndpoint))

	// Verify MariaDB CRs still exist with correct names.
	g.Expect(c.Get(ctx, dbKey, &mariadbv1alpha1.Database{})).To(Succeed(), "MariaDB Database CR should still exist")
	g.Expect(c.Get(ctx, userKey, &mariadbv1alpha1.User{})).To(Succeed(), "MariaDB User CR should still exist")
	g.Expect(c.Get(ctx, grantKey, &mariadbv1alpha1.Grant{})).To(Succeed(), "MariaDB Grant CR should still exist")
}

// --- Task CC-0015/2.1: CronJob detailed spec test (REQ-006) ---

func TestIntegration_CronJobDetailedSpec(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cronjob-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	// Fetch the Fernet rotation CronJob (REQ-006).
	cronJob := &batchv1.CronJob{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-fernet-rotate"}, cronJob)).
		To(Succeed(), "CronJob test-keystone-fernet-rotate should exist")

	// Verify schedule matches spec.fernet.rotationSchedule.
	g.Expect(cronJob.Spec.Schedule).To(Equal(ks.Spec.Fernet.RotationSchedule),
		"CronJob schedule should match spec.fernet.rotationSchedule")

	podSpec := cronJob.Spec.JobTemplate.Spec.Template.Spec
	expectedImage := fmt.Sprintf("%s:%s", ks.Spec.Image.Repository, ks.Spec.Image.Tag)

	// Verify ServiceAccountName.
	g.Expect(podSpec.ServiceAccountName).To(Equal(fmt.Sprintf("%s-fernet-rotate", ks.Name)),
		"CronJob should use the fernet-rotate ServiceAccount")

	// Verify init container: copy-keys.
	g.Expect(podSpec.InitContainers).To(HaveLen(1), "CronJob should have exactly one init container")
	initContainer := podSpec.InitContainers[0]
	g.Expect(initContainer.Name).To(Equal("copy-keys"))
	g.Expect(initContainer.Image).To(Equal(expectedImage), "init container image should match spec")
	g.Expect(initContainer.Command).To(Equal([]string{"sh", "-c", "cp /fernet-keys-src/* /etc/keystone/fernet-keys/"}))

	// Verify init container volume mounts.
	g.Expect(initContainer.VolumeMounts).To(HaveLen(2))
	initMounts := map[string]corev1.VolumeMount{}
	for _, vm := range initContainer.VolumeMounts {
		initMounts[vm.Name] = vm
	}
	g.Expect(initMounts["fernet-keys-src"].MountPath).To(Equal("/fernet-keys-src"))
	g.Expect(initMounts["fernet-keys-src"].ReadOnly).To(BeTrue(), "fernet-keys-src should be read-only")
	g.Expect(initMounts["fernet-keys"].MountPath).To(Equal("/etc/keystone/fernet-keys"))

	// Verify main container: fernet-rotate.
	g.Expect(podSpec.Containers).To(HaveLen(1), "CronJob should have exactly one main container")
	mainContainer := podSpec.Containers[0]
	g.Expect(mainContainer.Name).To(Equal("fernet-rotate"))
	g.Expect(mainContainer.Image).To(Equal(expectedImage), "main container image should match spec")
	g.Expect(mainContainer.Command).To(Equal([]string{"/scripts/fernet_rotate.sh"}))

	// Verify main container env vars.
	envMap := map[string]corev1.EnvVar{}
	for _, env := range mainContainer.Env {
		envMap[env.Name] = env
	}
	g.Expect(envMap).To(HaveKey("SECRET_NAME"))
	// SECRET_NAME points at the staging Secret — CronJob SA cannot patch
	// the production Secret (CC-0081).
	g.Expect(envMap["SECRET_NAME"].Value).To(Equal(fmt.Sprintf("%s-fernet-keys-rotation", ks.Name)))

	g.Expect(envMap).To(HaveKey("SECRET_NAMESPACE"))
	g.Expect(envMap["SECRET_NAMESPACE"].ValueFrom).NotTo(BeNil(), "SECRET_NAMESPACE should use ValueFrom")
	g.Expect(envMap["SECRET_NAMESPACE"].ValueFrom.FieldRef).NotTo(BeNil(), "SECRET_NAMESPACE should use fieldRef")
	g.Expect(envMap["SECRET_NAMESPACE"].ValueFrom.FieldRef.FieldPath).To(Equal("metadata.namespace"))

	g.Expect(envMap).To(HaveKey("OS_fernet_tokens__max_active_keys"))
	g.Expect(envMap["OS_fernet_tokens__max_active_keys"].Value).To(Equal("3"),
		"OS_fernet_tokens__max_active_keys should match spec.fernet.maxActiveKeys")

	// Verify main container volume mounts.
	g.Expect(mainContainer.VolumeMounts).To(HaveLen(4))
	g.Expect(mainContainer.VolumeMounts[0].Name).To(Equal("fernet-keys"))
	g.Expect(mainContainer.VolumeMounts[0].MountPath).To(Equal("/etc/keystone/fernet-keys"))
	g.Expect(mainContainer.VolumeMounts[1].Name).To(Equal("credential-keys"))
	g.Expect(mainContainer.VolumeMounts[1].MountPath).To(Equal("/etc/keystone/credential-keys"))
	g.Expect(mainContainer.VolumeMounts[1].ReadOnly).To(BeTrue())
	g.Expect(mainContainer.VolumeMounts[2].Name).To(Equal("config"))
	g.Expect(mainContainer.VolumeMounts[2].MountPath).To(Equal("/etc/keystone/keystone.conf.d/"))
	g.Expect(mainContainer.VolumeMounts[2].ReadOnly).To(BeTrue())
	g.Expect(mainContainer.VolumeMounts[3].Name).To(Equal("scripts"))
	g.Expect(mainContainer.VolumeMounts[3].MountPath).To(Equal("/scripts"))
	g.Expect(mainContainer.VolumeMounts[3].ReadOnly).To(BeTrue())

	// Verify volumes: fernet-keys-src (Secret), fernet-keys (emptyDir), credential-keys (Secret), config (ConfigMap), and scripts (ConfigMap).
	volMap := map[string]corev1.Volume{}
	for _, v := range podSpec.Volumes {
		volMap[v.Name] = v
	}
	g.Expect(volMap).To(HaveLen(5))

	g.Expect(volMap).To(HaveKey("fernet-keys-src"))
	g.Expect(volMap["fernet-keys-src"].Secret).NotTo(BeNil(), "fernet-keys-src volume should be a Secret")
	g.Expect(volMap["fernet-keys-src"].Secret.SecretName).To(Equal(fmt.Sprintf("%s-fernet-keys", ks.Name)))

	g.Expect(volMap).To(HaveKey("fernet-keys"))
	g.Expect(volMap["fernet-keys"].EmptyDir).NotTo(BeNil(), "fernet-keys volume should be an emptyDir")

	g.Expect(volMap).To(HaveKey("credential-keys"))
	g.Expect(volMap["credential-keys"].Secret).NotTo(BeNil(), "credential-keys volume should be a Secret")
	g.Expect(volMap["credential-keys"].Secret.SecretName).To(Equal(fmt.Sprintf("%s-credential-keys", ks.Name)))

	g.Expect(volMap).To(HaveKey("config"))
	g.Expect(volMap["config"].ConfigMap).NotTo(BeNil(), "config volume should be a ConfigMap")
	g.Expect(volMap["config"].ConfigMap.Name).To(HavePrefix(fmt.Sprintf("%s-config-", ks.Name)),
		"config volume should reference a ConfigMap with the expected name prefix")

	g.Expect(volMap).To(HaveKey("scripts"))
	g.Expect(volMap["scripts"].ConfigMap).NotTo(BeNil(), "scripts volume should be a ConfigMap")
	g.Expect(volMap["scripts"].ConfigMap.Name).To(HavePrefix(fmt.Sprintf("%s-fernet-rotate-script-", ks.Name)),
		"scripts volume should reference a ConfigMap with the expected name prefix")
	g.Expect(volMap["scripts"].ConfigMap.DefaultMode).NotTo(BeNil())
	g.Expect(*volMap["scripts"].ConfigMap.DefaultMode).To(Equal(int32(0o555)))
}

// --- Task CC-0015/2.2: Bootstrap Job detailed spec test (REQ-007) ---

// driveReconciliationToBootstrapJob drives external dependencies through
// reconciliation phases until the bootstrap Job appears, without simulating
// bootstrap completion (CC-0015, REQ-007).
func driveReconciliationToBootstrapJob(t testing.TB, ctx context.Context, c client.Client, ksName, ns string) {
	t.Helper()
	g := NewGomegaWithT(t)

	key := types.NamespacedName{Name: ksName, Namespace: ns}

	waitForCondition(t, ctx, c, key, "SecretsReady", metav1.ConditionTrue, eventuallyTimeout)
	waitForCondition(t, ctx, c, key, "FernetKeysReady", metav1.ConditionTrue, eventuallyTimeout)

	dbSyncKey := client.ObjectKey{Namespace: ns, Name: fmt.Sprintf("%s-db-sync", ksName)}
	g.Eventually(func() error {
		return c.Get(ctx, dbSyncKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "db-sync Job should appear")
	g.Expect(simulators.SimulateJobComplete(ctx, c, dbSyncKey)).To(Succeed())

	// Wait for schema-check Job and simulate completion (CC-0064).
	schemaCheckKey := client.ObjectKey{Namespace: ns, Name: fmt.Sprintf("%s-schema-check", ksName)}
	g.Eventually(func() error {
		return c.Get(ctx, schemaCheckKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "schema-check Job should appear")
	g.Expect(simulators.SimulateJobComplete(ctx, c, schemaCheckKey)).To(Succeed())

	waitForCondition(t, ctx, c, key, "DatabaseReady", metav1.ConditionTrue, eventuallyTimeout)

	deployKey := client.ObjectKey{Namespace: ns, Name: ksName}
	deploy := &appsv1.Deployment{}
	g.Eventually(func() error {
		return c.Get(ctx, deployKey, deploy)
	}, eventuallyTimeout, pollInterval).Should(Succeed())
	g.Expect(simulators.SimulateDeploymentReady(ctx, c, deployKey, ptr.Deref(deploy.Spec.Replicas, 1))).To(Succeed())

	waitForCondition(t, ctx, c, key, "DeploymentReady", metav1.ConditionTrue, eventuallyTimeout)

	bootstrapKey := client.ObjectKey{Namespace: ns, Name: fmt.Sprintf("%s-bootstrap", ksName)}
	g.Eventually(func() error {
		return c.Get(ctx, bootstrapKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "bootstrap Job should appear")
}

func TestIntegration_BootstrapJobDetailedSpec(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-bootstrap-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	// Drive reconciliation until bootstrap Job appears, without completing it (REQ-007).
	driveReconciliationToBootstrapJob(t, ctx, c, ks.Name, ns.Name)

	// Fetch the bootstrap Job.
	bootstrapJob := &batchv1.Job{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-bootstrap"}, bootstrapJob)).
		To(Succeed(), "bootstrap Job should exist")

	// Verify backoffLimit (REQ-007).
	g.Expect(bootstrapJob.Spec.BackoffLimit).NotTo(BeNil())
	g.Expect(*bootstrapJob.Spec.BackoffLimit).To(Equal(int32(4)), "backoffLimit should be 4")

	// Verify ttlSecondsAfterFinished (REQ-007).
	g.Expect(bootstrapJob.Spec.TTLSecondsAfterFinished).NotTo(BeNil())
	g.Expect(*bootstrapJob.Spec.TTLSecondsAfterFinished).To(Equal(int32(300)), "ttlSecondsAfterFinished should be 300")

	// Verify container spec.
	podSpec := bootstrapJob.Spec.Template.Spec
	g.Expect(podSpec.Containers).To(HaveLen(1))
	container := podSpec.Containers[0]
	g.Expect(container.Name).To(Equal("bootstrap"))

	// Verify image.
	expectedImage := fmt.Sprintf("%s:%s", ks.Spec.Image.Repository, ks.Spec.Image.Tag)
	g.Expect(container.Image).To(Equal(expectedImage))

	// Verify command uses shell wrapper for idempotent bootstrap (REQ-007).
	g.Expect(container.Command[:3]).To(Equal([]string{"/bin/sh", "-eu", "-c"}))
	g.Expect(container.Command[3]).To(ContainSubstring("keystone-manage --config-dir=/etc/keystone/keystone.conf.d/ bootstrap"))
	expectedServiceURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:5000/v3", ks.Name, ns.Name)
	g.Expect(container.Command[3]).To(ContainSubstring(expectedServiceURL))
	g.Expect(container.Command[3]).To(ContainSubstring("--bootstrap-region-id " + ks.Spec.Bootstrap.Region))
	g.Expect(container.Args).To(BeNil())

	// Verify env: BOOTSTRAP_PASSWORD from admin Secret (REQ-007) and
	// OS_DATABASE__CONNECTION from the derived db-connection Secret
	// (CC-0080, REQ-004).
	g.Expect(container.Env).To(HaveLen(2))
	pwEnv := container.Env[0]
	g.Expect(pwEnv.Name).To(Equal("BOOTSTRAP_PASSWORD"))
	g.Expect(pwEnv.ValueFrom).NotTo(BeNil())
	g.Expect(pwEnv.ValueFrom.SecretKeyRef).NotTo(BeNil())
	g.Expect(pwEnv.ValueFrom.SecretKeyRef.Name).To(Equal(ks.Spec.Bootstrap.AdminPasswordSecretRef.Name),
		"BOOTSTRAP_PASSWORD should reference the admin password Secret")
	g.Expect(pwEnv.ValueFrom.SecretKeyRef.Key).To(Equal("password"))

	dbEnv := container.Env[1]
	g.Expect(dbEnv.Name).To(Equal("OS_DATABASE__CONNECTION"))
	g.Expect(dbEnv.ValueFrom).NotTo(BeNil())
	g.Expect(dbEnv.ValueFrom.SecretKeyRef).NotTo(BeNil())
	g.Expect(dbEnv.ValueFrom.SecretKeyRef.Name).To(Equal(ks.Name+"-db-connection"),
		"OS_DATABASE__CONNECTION should reference the derived db-connection Secret")
	g.Expect(dbEnv.ValueFrom.SecretKeyRef.Key).To(Equal(dbConnectionSecretKey))

	// Verify config volume mount (REQ-007).
	g.Expect(container.VolumeMounts).To(HaveLen(2))
	g.Expect(container.VolumeMounts[0].Name).To(Equal("config"))
	g.Expect(container.VolumeMounts[0].MountPath).To(Equal("/etc/keystone/keystone.conf.d/"))
	g.Expect(container.VolumeMounts[0].ReadOnly).To(BeTrue())

	// Verify fernet-keys volume mount (CC-0018: bootstrap needs fernet keys).
	g.Expect(container.VolumeMounts[1].Name).To(Equal("fernet-keys"))
	g.Expect(container.VolumeMounts[1].MountPath).To(Equal("/etc/keystone/fernet-keys/"))
	g.Expect(container.VolumeMounts[1].ReadOnly).To(BeTrue())

	// Verify config volume source is a ConfigMap with the expected name prefix.
	g.Expect(podSpec.Volumes).To(HaveLen(2))
	g.Expect(podSpec.Volumes[0].Name).To(Equal("config"))
	g.Expect(podSpec.Volumes[0].ConfigMap).NotTo(BeNil())
	g.Expect(podSpec.Volumes[0].ConfigMap.Name).To(HavePrefix(fmt.Sprintf("%s-config-", ks.Name)),
		"config volume should reference a ConfigMap with the expected name prefix")

	// Verify fernet-keys volume references the Secret.
	g.Expect(podSpec.Volumes[1].Name).To(Equal("fernet-keys"))
	g.Expect(podSpec.Volumes[1].Secret).NotTo(BeNil())
	g.Expect(podSpec.Volumes[1].Secret.SecretName).To(Equal(fmt.Sprintf("%s-fernet-keys", ks.Name)))

	// Verify RestartPolicy.
	g.Expect(podSpec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
}

// --- Task CC-0037: PodDisruptionBudget tests ---

func TestIntegration_PDBSpec(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-pdb-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	// Fetch the PDB (CC-0037).
	pdb := &policyv1.PodDisruptionBudget{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone"}, pdb)).
		To(Succeed(), "PDB test-keystone should exist")

	// Verify labels match commonLabels (CC-0037).
	g.Expect(pdb.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(pdb.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
	g.Expect(pdb.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "keystone-operator"))

	// Verify selector matches selectorLabels (CC-0037).
	g.Expect(pdb.Spec.Selector).NotTo(BeNil())
	g.Expect(pdb.Spec.Selector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(pdb.Spec.Selector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))

	// Verify PDB selector matches Deployment selector (CC-0037).
	deploy := &appsv1.Deployment{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone"}, deploy)).To(Succeed())
	g.Expect(pdb.Spec.Selector.MatchLabels).To(Equal(deploy.Spec.Selector.MatchLabels))

	// Replicas=3 → minAvailable=1 (CC-0037).
	g.Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
	g.Expect(*pdb.Spec.MinAvailable).To(Equal(intstr.FromInt32(1)))
	g.Expect(pdb.Spec.MaxUnavailable).To(BeNil())

	// Verify owner reference (CC-0037).
	g.Expect(pdb.OwnerReferences).To(HaveLen(1))
	g.Expect(pdb.OwnerReferences[0].Name).To(Equal("test-keystone"))
}

func TestIntegration_PDBUpdatedOnReplicaChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-pdb-replica-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	key := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}
	pdbKey := client.ObjectKey{Namespace: ns.Name, Name: "test-keystone"}

	// Initial state: replicas=3 → minAvailable=1 (CC-0037).
	pdb := &policyv1.PodDisruptionBudget{}
	g.Expect(c.Get(ctx, pdbKey, pdb)).To(Succeed())
	g.Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
	g.Expect(pdb.Spec.MaxUnavailable).To(BeNil())

	// Update replicas to 1 → PDB should switch to maxUnavailable=1 (CC-0037).
	updated := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())
	updated.Spec.Replicas = 1
	g.Expect(c.Update(ctx, updated)).To(Succeed())

	// Wait for the controller to reconcile and update the PDB (CC-0037).
	g.Eventually(func() *intstr.IntOrString {
		p := &policyv1.PodDisruptionBudget{}
		if err := c.Get(ctx, pdbKey, p); err != nil {
			return nil
		}
		return p.Spec.MaxUnavailable
	}, eventuallyTimeout, pollInterval).ShouldNot(BeNil(), "PDB should switch to maxUnavailable")

	g.Expect(c.Get(ctx, pdbKey, pdb)).To(Succeed())
	g.Expect(*pdb.Spec.MaxUnavailable).To(Equal(intstr.FromInt32(1)))
	g.Expect(pdb.Spec.MinAvailable).To(BeNil())
}

// --- HPA integration tests (CC-0038) ---

// integrationBrownfieldKeystoneWithAutoscaling returns a valid Keystone CR with autoscaling
// configured for integration tests (CC-0038).
func integrationBrownfieldKeystoneWithAutoscaling(name, namespace string, maxReplicas int32, cpuUtil *int32) *keystonev1alpha1.Keystone {
	ks := integrationBrownfieldKeystone(name, namespace)
	ks.Spec.Autoscaling = &keystonev1alpha1.AutoscalingSpec{
		MaxReplicas:          maxReplicas,
		TargetCPUUtilization: cpuUtil,
	}
	return ks
}

func TestIntegration_HPASpec(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-hpa-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	cpuUtil := int32(80)
	ks := integrationBrownfieldKeystoneWithAutoscaling("test-keystone", ns.Name, 10, &cpuUtil)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	// Fetch the HPA (CC-0038).
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone"}, hpa)).
		To(Succeed(), "HPA test-keystone should exist")

	// Verify ScaleTargetRef (CC-0038).
	g.Expect(hpa.Spec.ScaleTargetRef.Kind).To(Equal("Deployment"))
	g.Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal("test-keystone"))
	g.Expect(hpa.Spec.ScaleTargetRef.APIVersion).To(Equal("apps/v1"))

	// MinReplicas defaults to spec.replicas (3) when not explicitly set (CC-0038).
	g.Expect(hpa.Spec.MinReplicas).NotTo(BeNil())
	g.Expect(*hpa.Spec.MinReplicas).To(Equal(int32(3)))

	// MaxReplicas (CC-0038).
	g.Expect(hpa.Spec.MaxReplicas).To(Equal(int32(10)))

	// CPU metric (CC-0038).
	g.Expect(hpa.Spec.Metrics).To(HaveLen(1))
	g.Expect(hpa.Spec.Metrics[0].Resource.Name).To(Equal(corev1.ResourceCPU))
	g.Expect(*hpa.Spec.Metrics[0].Resource.Target.AverageUtilization).To(Equal(int32(80)))

	// Verify labels (CC-0038).
	g.Expect(hpa.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(hpa.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
	g.Expect(hpa.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "keystone-operator"))

	// Verify owner reference (CC-0038).
	g.Expect(hpa.OwnerReferences).To(HaveLen(1))
	g.Expect(hpa.OwnerReferences[0].Name).To(Equal("test-keystone"))

	// Verify HPAReady condition (CC-0038).
	key := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}
	ksState := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, key, ksState)).To(Succeed())
	hpaCond := meta.FindStatusCondition(ksState.Status.Conditions, "HPAReady")
	g.Expect(hpaCond).NotTo(BeNil())
	g.Expect(hpaCond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(hpaCond.Reason).To(Equal("HPAReady"))
}

func TestIntegration_HPAUpdatedOnAutoscalingChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-hpa-update-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	cpuUtil := int32(80)
	ks := integrationBrownfieldKeystoneWithAutoscaling("test-keystone", ns.Name, 10, &cpuUtil)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	hpaKey := client.ObjectKey{Namespace: ns.Name, Name: "test-keystone"}
	key := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}

	// Initial state: maxReplicas=10 (CC-0038).
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	g.Expect(c.Get(ctx, hpaKey, hpa)).To(Succeed())
	g.Expect(hpa.Spec.MaxReplicas).To(Equal(int32(10)))

	// Update maxReplicas to 20 (CC-0038).
	updated := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())
	updated.Spec.Autoscaling.MaxReplicas = 20
	g.Expect(c.Update(ctx, updated)).To(Succeed())

	// Wait for the controller to reconcile and update the HPA (CC-0038).
	g.Eventually(func() int32 {
		h := &autoscalingv2.HorizontalPodAutoscaler{}
		if err := c.Get(ctx, hpaKey, h); err != nil {
			return 0
		}
		return h.Spec.MaxReplicas
	}, eventuallyTimeout, pollInterval).Should(Equal(int32(20)), "HPA maxReplicas should be updated to 20")
}

func TestIntegration_HPADeletedWhenAutoscalingRemoved(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-hpa-delete-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	cpuUtil := int32(80)
	ks := integrationBrownfieldKeystoneWithAutoscaling("test-keystone", ns.Name, 10, &cpuUtil)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	hpaKey := client.ObjectKey{Namespace: ns.Name, Name: "test-keystone"}
	key := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}

	// HPA should exist initially (CC-0038).
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	g.Expect(c.Get(ctx, hpaKey, hpa)).To(Succeed(), "HPA should exist when autoscaling is configured")

	// Remove autoscaling (CC-0038).
	updated := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())
	updated.Spec.Autoscaling = nil
	g.Expect(c.Update(ctx, updated)).To(Succeed())

	// Wait for the HPA to be deleted (CC-0038).
	g.Eventually(func() bool {
		h := &autoscalingv2.HorizontalPodAutoscaler{}
		err := c.Get(ctx, hpaKey, h)
		return err != nil
	}, eventuallyTimeout, pollInterval).Should(BeTrue(), "HPA should be deleted when autoscaling is removed")

	// Verify HPAReady condition switches to HPANotRequired (CC-0038).
	g.Eventually(func() string {
		ksState := &keystonev1alpha1.Keystone{}
		if err := c.Get(ctx, key, ksState); err != nil {
			return ""
		}
		cond := meta.FindStatusCondition(ksState.Status.Conditions, "HPAReady")
		if cond == nil {
			return ""
		}
		return cond.Reason
	}, eventuallyTimeout, pollInterval).Should(Equal("HPANotRequired"), "HPAReady reason should be HPANotRequired")
}

// --- Task CC-0056/4.1: Fresh deployment — InstalledRelease tracking (REQ-008) ---

func TestIntegration_FreshDeployment_InstalledReleaseTracking(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-fresh-deploy-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	// Create brownfield Keystone CR with tag "2025.2" (CC-0056, REQ-008).
	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	key := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}

	// Drive the full reconciliation to Ready=True.
	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	// Fetch the final state.
	updated := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())

	// Verify status.installedRelease is set to spec.image.tag (CC-0056, REQ-008).
	g.Expect(updated.Status.InstalledRelease).To(Equal("2025.2"),
		"installedRelease should equal spec.image.tag after fresh deployment")

	// Verify no upgrade was triggered (CC-0056).
	g.Expect(string(updated.Status.UpgradePhase)).To(Equal(""),
		"upgradePhase should be empty for fresh deployment")
	g.Expect(updated.Status.TargetRelease).To(Equal(""),
		"targetRelease should be empty for fresh deployment")

	// Verify the db-sync Job used standard db_sync command without upgrade flags (CC-0056).
	dbSyncJob := &batchv1.Job{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-db-sync"}, dbSyncJob)).
		To(Succeed(), "standard db-sync Job should exist")
	container := dbSyncJob.Spec.Template.Spec.Containers[0]
	g.Expect(container.Command).To(Equal([]string{
		"keystone-manage", "--config-dir=/etc/keystone/keystone.conf.d/", "db_sync",
	}), "db-sync command should be standard db_sync without --expand/--migrate/--contract")
	g.Expect(container.Image).To(Equal(
		fmt.Sprintf("%s:%s", ks.Spec.Image.Repository, ks.Spec.Image.Tag)),
		"db-sync Job image should match spec.image.tag")

	// Verify no upgrade Jobs were created (CC-0056).
	for _, phase := range []string{"expand", "migrate", "contract"} {
		j := &batchv1.Job{}
		err := c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("test-keystone-db-%s", phase)}, j)
		g.Expect(err).To(HaveOccurred(),
			fmt.Sprintf("upgrade Job %s should not exist for fresh deployment", phase))
	}

	// Verify no regression: Ready=True with AllReady reason (CC-0056, REQ-008).
	readyCond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
	g.Expect(readyCond).NotTo(BeNil(), "Ready condition should exist")
	g.Expect(readyCond.Status).To(Equal(metav1.ConditionTrue), "Ready should be True")
	g.Expect(readyCond.Reason).To(Equal("AllReady"))
}

// --- Task CC-0056/4.2: Full expand-migrate-contract upgrade cycle (REQ-001 through REQ-006) ---

func TestIntegration_UpgradeCycle_ExpandMigrateContract(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-upgrade-cycle-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	// Create brownfield Keystone with initial release "2025.1" (CC-0056).
	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	ks.Spec.Image.Tag = "2025.1"
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	key := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}

	// Drive initial deployment to Ready=True.
	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	// Verify initial installedRelease (CC-0056, REQ-008).
	initial := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, key, initial)).To(Succeed())
	g.Expect(initial.Status.InstalledRelease).To(Equal("2025.1"),
		"installedRelease should be 2025.1 after initial deployment")

	expectedNewImage := fmt.Sprintf("%s:2025.2", ks.Spec.Image.Repository)

	// --- Trigger upgrade: update image tag to 2025.2 (CC-0056, REQ-001) ---
	current := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, key, current)).To(Succeed())
	current.Spec.Image.Tag = "2025.2"
	g.Expect(c.Update(ctx, current)).To(Succeed())

	// Phase 1: Expanding — expand Job with NEW image (CC-0056, REQ-002).
	g.Eventually(func() keystonev1alpha1.UpgradePhase {
		ks := &keystonev1alpha1.Keystone{}
		if err := c.Get(ctx, key, ks); err != nil {
			return ""
		}
		return ks.Status.UpgradePhase
	}, eventuallyTimeout, pollInterval).Should(Equal(keystonev1alpha1.UpgradePhaseExpanding),
		"upgradePhase should transition to Expanding")

	// Verify targetRelease is set (CC-0056, REQ-001).
	ksState := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, key, ksState)).To(Succeed())
	g.Expect(ksState.Status.TargetRelease).To(Equal("2025.2"))

	expandKey := client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-db-expand"}
	g.Eventually(func() error {
		return c.Get(ctx, expandKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "expand Job should appear")

	expandJob := &batchv1.Job{}
	g.Expect(c.Get(ctx, expandKey, expandJob)).To(Succeed())
	g.Expect(expandJob.Spec.Template.Spec.Containers[0].Image).To(Equal(expectedNewImage),
		"expand Job should use NEW image (target release)")
	g.Expect(expandJob.Spec.Template.Spec.Containers[0].Command).To(Equal([]string{
		"keystone-manage", "--config-dir=/etc/keystone/keystone.conf.d/", "db_sync", "--expand",
	}), "expand Job should use --expand flag")

	g.Expect(simulators.SimulateJobComplete(ctx, c, expandKey)).To(Succeed(), "simulate expand Job completion")

	// Phase 2: Migrating — migrate Job with NEW image (CC-0056, REQ-003).
	g.Eventually(func() keystonev1alpha1.UpgradePhase {
		ks := &keystonev1alpha1.Keystone{}
		if err := c.Get(ctx, key, ks); err != nil {
			return ""
		}
		return ks.Status.UpgradePhase
	}, eventuallyTimeout, pollInterval).Should(Equal(keystonev1alpha1.UpgradePhaseMigrating),
		"upgradePhase should transition to Migrating")

	migrateKey := client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-db-migrate"}
	g.Eventually(func() error {
		return c.Get(ctx, migrateKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "migrate Job should appear")

	migrateJob := &batchv1.Job{}
	g.Expect(c.Get(ctx, migrateKey, migrateJob)).To(Succeed())
	g.Expect(migrateJob.Spec.Template.Spec.Containers[0].Image).To(Equal(expectedNewImage),
		"migrate Job should use NEW image (target release)")
	g.Expect(migrateJob.Spec.Template.Spec.Containers[0].Command).To(Equal([]string{
		"keystone-manage", "--config-dir=/etc/keystone/keystone.conf.d/", "db_sync", "--migrate",
	}), "migrate Job should use --migrate flag")

	g.Expect(simulators.SimulateJobComplete(ctx, c, migrateKey)).To(Succeed(), "simulate migrate Job completion")

	// Phase 3: RollingUpdate — Deployment updated with NEW image (CC-0056, REQ-004).
	g.Eventually(func() keystonev1alpha1.UpgradePhase {
		ks := &keystonev1alpha1.Keystone{}
		if err := c.Get(ctx, key, ks); err != nil {
			return ""
		}
		return ks.Status.UpgradePhase
	}, eventuallyTimeout, pollInterval).Should(Equal(keystonev1alpha1.UpgradePhaseRollingUpdate),
		"upgradePhase should transition to RollingUpdate")

	// Wait for Deployment to be updated with new image (CC-0056, REQ-004).
	deployKey := client.ObjectKey{Namespace: ns.Name, Name: ks.Name}
	g.Eventually(func() string {
		d := &appsv1.Deployment{}
		if err := c.Get(ctx, deployKey, d); err != nil {
			return ""
		}
		if len(d.Spec.Template.Spec.Containers) == 0 {
			return ""
		}
		return d.Spec.Template.Spec.Containers[0].Image
	}, eventuallyTimeout, pollInterval).Should(Equal(expectedNewImage),
		"Deployment should be updated with new image during RollingUpdate")

	// Simulate Deployment rollout completion (CC-0056, REQ-004).
	deploy := &appsv1.Deployment{}
	g.Expect(c.Get(ctx, deployKey, deploy)).To(Succeed())
	g.Expect(simulators.SimulateDeploymentReady(ctx, c, deployKey, ptr.Deref(deploy.Spec.Replicas, 1))).
		To(Succeed(), "simulate Deployment rollout with new image")

	// Phase 4: Contracting — contract Job with NEW image (CC-0056, REQ-005).
	g.Eventually(func() keystonev1alpha1.UpgradePhase {
		ks := &keystonev1alpha1.Keystone{}
		if err := c.Get(ctx, key, ks); err != nil {
			return ""
		}
		return ks.Status.UpgradePhase
	}, eventuallyTimeout, pollInterval).Should(Equal(keystonev1alpha1.UpgradePhaseContracting),
		"upgradePhase should transition to Contracting")

	contractKey := client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-db-contract"}
	g.Eventually(func() error {
		return c.Get(ctx, contractKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "contract Job should appear")

	contractJob := &batchv1.Job{}
	g.Expect(c.Get(ctx, contractKey, contractJob)).To(Succeed())
	g.Expect(contractJob.Spec.Template.Spec.Containers[0].Image).To(Equal(expectedNewImage),
		"contract Job should use NEW image")
	g.Expect(contractJob.Spec.Template.Spec.Containers[0].Command).To(Equal([]string{
		"keystone-manage", "--config-dir=/etc/keystone/keystone.conf.d/", "db_sync", "--contract",
	}), "contract Job should use --contract flag")

	g.Expect(simulators.SimulateJobComplete(ctx, c, contractKey)).To(Succeed(), "simulate contract Job completion")

	// Verify upgrade completion: installedRelease updated, phase/target cleared (CC-0056, REQ-006).
	g.Eventually(func() string {
		ks := &keystonev1alpha1.Keystone{}
		if err := c.Get(ctx, key, ks); err != nil {
			return ""
		}
		return ks.Status.InstalledRelease
	}, eventuallyTimeout, pollInterval).Should(Equal("2025.2"),
		"installedRelease should be updated to 2025.2 after upgrade")

	postUpgrade := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, key, postUpgrade)).To(Succeed())
	g.Expect(postUpgrade.Status.TargetRelease).To(Equal(""),
		"targetRelease should be cleared after upgrade completes")
	g.Expect(string(postUpgrade.Status.UpgradePhase)).To(Equal(""),
		"upgradePhase should be cleared after upgrade completes")

	// Post-upgrade: the operator re-runs db_sync and bootstrap with the new image
	// because the PodSpec hash changed (CC-0005). Drive the remaining reconciliation.
	dbSyncKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-db-sync", ks.Name)}
	g.Eventually(func() bool {
		j := &batchv1.Job{}
		if err := c.Get(ctx, dbSyncKey, j); err != nil {
			return false
		}
		if len(j.Spec.Template.Spec.Containers) == 0 {
			return false
		}
		return j.Spec.Template.Spec.Containers[0].Image == expectedNewImage
	}, eventuallyTimeout, pollInterval).Should(BeTrue(), "db-sync Job should be re-created with new image")
	g.Expect(simulators.SimulateJobComplete(ctx, c, dbSyncKey)).To(Succeed(), "simulate post-upgrade db-sync completion")

	// Wait for schema-check Job re-created with new image (CC-0064).
	schemaCheckKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-schema-check", ks.Name)}
	g.Eventually(func() bool {
		j := &batchv1.Job{}
		if err := c.Get(ctx, schemaCheckKey, j); err != nil {
			return false
		}
		if len(j.Spec.Template.Spec.Containers) == 0 {
			return false
		}
		return j.Spec.Template.Spec.Containers[0].Image == expectedNewImage
	}, eventuallyTimeout, pollInterval).Should(BeTrue(), "schema-check Job should be re-created with new image")
	g.Expect(simulators.SimulateJobComplete(ctx, c, schemaCheckKey)).To(Succeed(), "simulate schema-check Job completion")

	waitForCondition(t, ctx, c, key, "DatabaseReady", metav1.ConditionTrue, eventuallyTimeout)

	// Bootstrap Job is also re-created with new image (CC-0005).
	bootstrapKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-bootstrap", ks.Name)}
	g.Eventually(func() bool {
		j := &batchv1.Job{}
		if err := c.Get(ctx, bootstrapKey, j); err != nil {
			return false
		}
		if len(j.Spec.Template.Spec.Containers) == 0 {
			return false
		}
		return j.Spec.Template.Spec.Containers[0].Image == expectedNewImage
	}, eventuallyTimeout, pollInterval).Should(BeTrue(), "bootstrap Job should be re-created with new image")
	g.Expect(simulators.SimulateJobComplete(ctx, c, bootstrapKey)).To(Succeed(), "simulate post-upgrade bootstrap completion")

	// Verify the system returns to Ready=True after the full upgrade cycle (CC-0056).
	waitForCondition(t, ctx, c, key, "Ready", metav1.ConditionTrue, eventuallyTimeout)

	final := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, key, final)).To(Succeed())
	g.Expect(final.Status.InstalledRelease).To(Equal("2025.2"))
	g.Expect(final.Status.Endpoint).To(Equal(
		fmt.Sprintf("http://test-keystone.%s.svc.cluster.local:5000/v3", ns.Name)),
		"endpoint should still be set after upgrade")
}

// --- Task CC-0057/4.1: Trust flush CronJob lifecycle integration tests (REQ-001, REQ-002, REQ-003) ---

// integrationBrownfieldKeystoneWithTrustFlush returns a valid Keystone CR with trustFlush
// configured for integration tests (CC-0057).
func integrationBrownfieldKeystoneWithTrustFlush(name, namespace, schedule string) *keystonev1alpha1.Keystone {
	ks := integrationBrownfieldKeystone(name, namespace)
	ks.Spec.TrustFlush = &keystonev1alpha1.TrustFlushSpec{
		Schedule: schedule,
	}
	return ks
}

func TestIntegration_TrustFlush_CronJobCreated(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-trustflush-create-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	// Create Keystone CR with trustFlush configured (CC-0057, REQ-001).
	ks := integrationBrownfieldKeystoneWithTrustFlush("test-keystone", ns.Name, "30 2 * * 0")
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	// Wait for the trust-flush CronJob to appear (CC-0057, REQ-001).
	cronJobKey := client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-trust-flush"}
	cronJob := &batchv1.CronJob{}
	g.Eventually(func() error {
		return c.Get(ctx, cronJobKey, cronJob)
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "CronJob test-keystone-trust-flush should appear")

	// Verify schedule matches spec.trustFlush.schedule (CC-0057, REQ-001).
	g.Expect(cronJob.Spec.Schedule).To(Equal("30 2 * * 0"),
		"CronJob schedule should match spec.trustFlush.schedule")

	// Verify suspend defaults to false (CC-0057, REQ-003).
	g.Expect(cronJob.Spec.Suspend).NotTo(BeNil())
	g.Expect(*cronJob.Spec.Suspend).To(BeFalse(), "CronJob should not be suspended by default")

	// Verify container image matches spec.image (CC-0057, REQ-004).
	podSpec := cronJob.Spec.JobTemplate.Spec.Template.Spec
	expectedImage := fmt.Sprintf("%s:%s", ks.Spec.Image.Repository, ks.Spec.Image.Tag)
	g.Expect(podSpec.Containers).To(HaveLen(1))
	container := podSpec.Containers[0]
	g.Expect(container.Name).To(Equal("trust-flush"))
	g.Expect(container.Image).To(Equal(expectedImage))

	// Verify command includes keystone-manage trust_flush with --config-dir (CC-0057, REQ-005).
	g.Expect(container.Command).To(Equal([]string{
		"keystone-manage", "--config-dir=/etc/keystone/keystone.conf.d/", "trust_flush",
	}))

	// Verify volume mounts (CC-0057, REQ-006).
	g.Expect(container.VolumeMounts).To(HaveLen(3))
	mountMap := map[string]corev1.VolumeMount{}
	for _, vm := range container.VolumeMounts {
		mountMap[vm.Name] = vm
	}
	g.Expect(mountMap["config"].MountPath).To(Equal("/etc/keystone/keystone.conf.d/"))
	g.Expect(mountMap["config"].ReadOnly).To(BeTrue())
	g.Expect(mountMap["fernet-keys"].MountPath).To(Equal("/etc/keystone/fernet-keys"))
	g.Expect(mountMap["fernet-keys"].ReadOnly).To(BeTrue())
	g.Expect(mountMap["credential-keys"].MountPath).To(Equal("/etc/keystone/credential-keys"))
	g.Expect(mountMap["credential-keys"].ReadOnly).To(BeTrue())

	// Verify volumes reference correct ConfigMap and Secrets (CC-0057, REQ-006).
	volMap := map[string]corev1.Volume{}
	for _, v := range podSpec.Volumes {
		volMap[v.Name] = v
	}
	g.Expect(volMap).To(HaveLen(3))
	g.Expect(volMap["config"].ConfigMap).NotTo(BeNil())
	g.Expect(volMap["fernet-keys"].Secret).NotTo(BeNil())
	g.Expect(volMap["fernet-keys"].Secret.SecretName).To(Equal("test-keystone-fernet-keys"))
	g.Expect(volMap["credential-keys"].Secret).NotTo(BeNil())
	g.Expect(volMap["credential-keys"].Secret.SecretName).To(Equal("test-keystone-credential-keys"))

	// Verify RestartPolicy (CC-0057, REQ-006).
	g.Expect(podSpec.RestartPolicy).To(Equal(corev1.RestartPolicyOnFailure))

	// Verify commonLabels on CronJob (CC-0057, REQ-009).
	g.Expect(cronJob.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(cronJob.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))

	// Verify ownerReference points to the Keystone CR (CC-0057, REQ-009).
	g.Expect(cronJob.OwnerReferences).To(HaveLen(1))
	g.Expect(cronJob.OwnerReferences[0].Name).To(Equal("test-keystone"))

	// Verify TrustFlushReady=True (CC-0057, REQ-001).
	key := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}
	cond := waitForCondition(t, ctx, c, key, "TrustFlushReady", metav1.ConditionTrue, eventuallyTimeout)
	g.Expect(cond.Reason).To(Equal("TrustFlushReady"))
	g.Expect(cond.Message).To(Equal("Trust flush CronJob is configured"))
}

func TestIntegration_TrustFlush_CronJobDeleted(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-trustflush-delete-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	// Create Keystone CR with trustFlush configured (CC-0057, REQ-001).
	ks := integrationBrownfieldKeystoneWithTrustFlush("test-keystone", ns.Name, "0 * * * *")
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	// Verify CronJob exists before removal (CC-0057, REQ-001).
	cronJobKey := client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-trust-flush"}
	g.Eventually(func() error {
		return c.Get(ctx, cronJobKey, &batchv1.CronJob{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "CronJob should exist before trustFlush removal")

	// Remove spec.trustFlush (CC-0057, REQ-002).
	key := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}
	updated := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())
	updated.Spec.TrustFlush = nil
	g.Expect(c.Update(ctx, updated)).To(Succeed())

	// Wait for the CronJob to be deleted (CC-0057, REQ-002).
	g.Eventually(func() bool {
		err := c.Get(ctx, cronJobKey, &batchv1.CronJob{})
		return err != nil
	}, eventuallyTimeout, pollInterval).Should(BeTrue(), "CronJob should be deleted when trustFlush is removed")

	// Verify TrustFlushReady=True with reason TrustFlushNotRequired (CC-0057, REQ-002).
	g.Eventually(func() string {
		ksState := &keystonev1alpha1.Keystone{}
		if err := c.Get(ctx, key, ksState); err != nil {
			return ""
		}
		cond := meta.FindStatusCondition(ksState.Status.Conditions, "TrustFlushReady")
		if cond == nil {
			return ""
		}
		return cond.Reason
	}, eventuallyTimeout, pollInterval).Should(Equal("TrustFlushNotRequired"),
		"TrustFlushReady reason should be TrustFlushNotRequired after removal")
}

func TestIntegration_TrustFlush_OmittedDoesNotCreateCronJob(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-trustflush-omit-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	// Create Keystone CR without trustFlush (nil) (CC-0057, REQ-003).
	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	// Verify TrustFlushReady=True with reason TrustFlushNotRequired (CC-0057, REQ-003).
	key := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}
	cond := waitForCondition(t, ctx, c, key, "TrustFlushReady", metav1.ConditionTrue, eventuallyTimeout)
	g.Expect(cond.Reason).To(Equal("TrustFlushNotRequired"))

	// Consistently verify no trust-flush CronJob is created (CC-0057, REQ-003).
	cronJobKey := client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-trust-flush"}
	g.Consistently(func() bool {
		err := c.Get(ctx, cronJobKey, &batchv1.CronJob{})
		return err != nil
	}, 2*time.Second, pollInterval).Should(BeTrue(),
		"trust-flush CronJob should not exist when trustFlush is omitted")
}

// TestIntegration_GracefulShutdownSpec verifies that the preStop lifecycle hook,
// terminationGracePeriodSeconds, and startup probe survive a full reconciliation
// cycle through the API server (CC-0063, REQ-006).
func TestIntegration_GracefulShutdownSpec(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-graceful-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	// Fetch the Deployment (CC-0063).
	deploy := &appsv1.Deployment{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone"}, deploy)).
		To(Succeed(), "Deployment test-keystone should exist")

	// Verify terminationGracePeriodSeconds (CC-0063, REQ-002): 30s gives 5s for
	// preStop sleep + 25s for uWSGI to drain in-flight requests.
	g.Expect(deploy.Spec.Template.Spec.TerminationGracePeriodSeconds).NotTo(BeNil(),
		"terminationGracePeriodSeconds must be set")
	g.Expect(*deploy.Spec.Template.Spec.TerminationGracePeriodSeconds).To(Equal(int64(30)))

	// Find the keystone container.
	container := findContainerByName(deploy.Spec.Template.Spec.Containers, "keystone")
	g.Expect(container).NotTo(BeNil(), "keystone container must exist")

	// Verify preStop lifecycle hook (CC-0063, REQ-001): 5-second sleep before
	// SIGTERM gives kube-proxy time to propagate endpoint removal.
	g.Expect(container.Lifecycle).NotTo(BeNil(), "Lifecycle must be set")
	g.Expect(container.Lifecycle.PreStop).NotTo(BeNil(), "PreStop hook must be set")
	g.Expect(container.Lifecycle.PreStop.Exec).NotTo(BeNil(), "PreStop must use exec")
	g.Expect(container.Lifecycle.PreStop.Exec.Command).To(Equal([]string{"/bin/sh", "-c", "sleep 5"}))

	// Verify startup probe (CC-0063, REQ-003): httpGet /v3 port 5000 with generous
	// failure threshold to survive slow cold starts.
	g.Expect(container.StartupProbe).NotTo(BeNil(), "StartupProbe must be set")
	g.Expect(container.StartupProbe.HTTPGet).NotTo(BeNil(), "StartupProbe must use httpGet")
	g.Expect(container.StartupProbe.HTTPGet.Path).To(Equal("/v3"))
	g.Expect(container.StartupProbe.HTTPGet.Port.IntValue()).To(Equal(5000))
	g.Expect(container.StartupProbe.FailureThreshold).To(Equal(int32(30)))
	g.Expect(container.StartupProbe.PeriodSeconds).To(Equal(int32(10)))
}

// --- Task CC-0058/3.1: PolicyValidation gating tests (REQ-004, REQ-008) ---

// driveReconciliationToPolicyValidation drives external dependencies through
// reconciliation phases until the policy validation Job appears, without
// simulating its completion. The Keystone CR MUST have spec.policyOverrides
// set so reconcilePolicyValidation creates a validation Job (CC-0058).
func driveReconciliationToPolicyValidation(t testing.TB, ctx context.Context, c client.Client, ksName, ns string) {
	t.Helper()
	g := NewGomegaWithT(t)

	key := types.NamespacedName{Name: ksName, Namespace: ns}

	// Wait for SecretsReady=True (prerequisites already created).
	waitForCondition(t, ctx, c, key, "SecretsReady", metav1.ConditionTrue, eventuallyTimeout)

	// Wait for FernetKeysReady=True (reconciler creates fernet keys automatically).
	waitForCondition(t, ctx, c, key, "FernetKeysReady", metav1.ConditionTrue, eventuallyTimeout)

	// Wait for the db-sync Job to appear and simulate its completion.
	dbSyncKey := client.ObjectKey{Namespace: ns, Name: fmt.Sprintf("%s-db-sync", ksName)}
	g.Eventually(func() error {
		return c.Get(ctx, dbSyncKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "db-sync Job should appear")
	g.Expect(simulators.SimulateJobComplete(ctx, c, dbSyncKey)).To(Succeed(), "simulate db-sync Job completion")

	// Wait for the schema-check Job to appear and simulate its completion (CC-0064).
	schemaCheckKey := client.ObjectKey{Namespace: ns, Name: fmt.Sprintf("%s-schema-check", ksName)}
	g.Eventually(func() error {
		return c.Get(ctx, schemaCheckKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "schema-check Job should appear")
	g.Expect(simulators.SimulateJobComplete(ctx, c, schemaCheckKey)).To(Succeed(), "simulate schema-check Job completion")

	// Wait for DatabaseReady=True.
	waitForCondition(t, ctx, c, key, "DatabaseReady", metav1.ConditionTrue, eventuallyTimeout)

	// Wait for the policy validation Job to appear.
	valJobKey := client.ObjectKey{Namespace: ns, Name: fmt.Sprintf("%s-policy-validation", ksName)}
	g.Eventually(func() error {
		return c.Get(ctx, valJobKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "policy-validation Job should appear")
}

// TestIntegration_PolicyValidation_GatesDeployment verifies that when
// spec.policyOverrides is set, the reconciler creates a validation Job BEFORE
// the Deployment and does not set DeploymentReady until the validation Job
// completes (CC-0058, REQ-004, REQ-008).
func TestIntegration_PolicyValidation_GatesDeployment(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-policyval-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	// Create a brownfield Keystone CR WITH inline policy overrides (CC-0058).
	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	ks.Spec.PolicyOverrides = &commonv1.PolicySpec{
		Rules: map[string]string{
			"identity:list_projects": "role:admin",
		},
	}
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	key := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}

	// Drive reconciliation through secrets, fernet, database, network policy
	// until the policy validation Job appears.
	driveReconciliationToPolicyValidation(t, ctx, c, ks.Name, ns.Name)

	// PolicyValidReady should be False with reason PolicyValidationInProgress
	// while the validation Job is running (CC-0058, REQ-008).
	pvCond := waitForCondition(t, ctx, c, key, conditionTypePolicyValidReady, metav1.ConditionFalse, eventuallyTimeout)
	g.Expect(pvCond.Reason).To(Equal(conditionReasonPolicyValidationInProgress),
		"PolicyValidReady reason should be PolicyValidationInProgress")

	// DeploymentReady should be absent (nil) — the Deployment must NOT be
	// created while policy validation is pending (CC-0058, REQ-004).
	g.Consistently(func(ig Gomega) {
		ksState := &keystonev1alpha1.Keystone{}
		ig.Expect(c.Get(ctx, key, ksState)).To(Succeed())
		ig.Expect(meta.FindStatusCondition(ksState.Status.Conditions, "DeploymentReady")).To(BeNil(),
			"DeploymentReady should be absent while policy validation is in progress")
	}, 2*time.Second, pollInterval).Should(Succeed())

	// Simulate the policy validation Job completion.
	valJobKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-policy-validation", ks.Name)}
	g.Expect(simulators.SimulateJobComplete(ctx, c, valJobKey)).To(Succeed(),
		"simulate policy-validation Job completion")

	// PolicyValidReady should transition to True with reason PolicyValidationPassed.
	pvCond = waitForCondition(t, ctx, c, key, conditionTypePolicyValidReady, metav1.ConditionTrue, eventuallyTimeout)
	g.Expect(pvCond.Reason).To(Equal(conditionReasonPolicyValidationPassed),
		"PolicyValidReady reason should be PolicyValidationPassed")

	// After validation passes, the Deployment should appear. Simulate readiness.
	deployKey := client.ObjectKey{Namespace: ns.Name, Name: ks.Name}
	deploy := &appsv1.Deployment{}
	g.Eventually(func() error {
		return c.Get(ctx, deployKey, deploy)
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "Deployment should appear after validation passes")
	g.Expect(simulators.SimulateDeploymentReady(ctx, c, deployKey, ptr.Deref(deploy.Spec.Replicas, 1))).
		To(Succeed(), "simulate Deployment ready")

	// Wait for DeploymentReady=True.
	waitForCondition(t, ctx, c, key, "DeploymentReady", metav1.ConditionTrue, eventuallyTimeout)

	// Continue through bootstrap to Ready=True to verify full lifecycle works.
	bootstrapKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-bootstrap", ks.Name)}
	g.Eventually(func() error {
		return c.Get(ctx, bootstrapKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "bootstrap Job should appear")
	g.Expect(simulators.SimulateJobComplete(ctx, c, bootstrapKey)).To(Succeed(),
		"simulate bootstrap Job completion")

	waitForCondition(t, ctx, c, key, "BootstrapReady", metav1.ConditionTrue, eventuallyTimeout)
	waitForCondition(t, ctx, c, key, "Ready", metav1.ConditionTrue, eventuallyTimeout)
}

// TestIntegration_PolicyValidation_NotRequired verifies that when
// spec.policyOverrides is nil, PolicyValidReady is set to True with reason
// PolicyValidationNotRequired and no validation Job is created (CC-0058, REQ-004).
func TestIntegration_PolicyValidation_NotRequired(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-policyval-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	// Create a brownfield Keystone CR WITHOUT policy overrides (default).
	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	// Drive the full reconciliation to Ready=True.
	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	// Verify PolicyValidReady=True with reason PolicyValidationNotRequired (CC-0058, REQ-004).
	key := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}
	cond := waitForCondition(t, ctx, c, key, conditionTypePolicyValidReady, metav1.ConditionTrue, eventuallyTimeout)
	g.Expect(cond.Reason).To(Equal(conditionReasonPolicyValidationNotRequired),
		"PolicyValidReady reason should be PolicyValidationNotRequired when policyOverrides is nil")

	// Verify no policy-validation Job exists (CC-0058, REQ-004).
	valJobKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-policy-validation", ks.Name)}
	g.Consistently(func() bool {
		err := c.Get(ctx, valJobKey, &batchv1.Job{})
		return err != nil
	}, 2*time.Second, pollInterval).Should(BeTrue(),
		"policy-validation Job should not exist when policyOverrides is nil")
}

// --- Task CC-0065/4.2: HTTPRoute sub-reconciler lifecycle tests (REQ-001, REQ-002, REQ-005) ---

// testGatewayParentName is the synthetic Gateway that integration HTTPRoutes
// attach to. The real Gateway resource is not installed in envtest; only the
// HTTPRoute is observed, so a name is sufficient (CC-0065, REQ-001).
const testGatewayParentName = "openstack-gateway"

// driveReconciliationToDeployment drives the reconciler through the secrets,
// fernet, database, and deployment phases until DeploymentReady=True. This
// leaves the controller positioned to run reconcileHTTPRoute on its next
// reconcile iteration (CC-0065).
func driveReconciliationToDeployment(t testing.TB, ctx context.Context, c client.Client, ksName, ns string) {
	t.Helper()
	g := NewGomegaWithT(t)

	key := types.NamespacedName{Name: ksName, Namespace: ns}

	waitForCondition(t, ctx, c, key, "SecretsReady", metav1.ConditionTrue, eventuallyTimeout)
	waitForCondition(t, ctx, c, key, "FernetKeysReady", metav1.ConditionTrue, eventuallyTimeout)

	dbSyncKey := client.ObjectKey{Namespace: ns, Name: fmt.Sprintf("%s-db-sync", ksName)}
	g.Eventually(func() error {
		return c.Get(ctx, dbSyncKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "db-sync Job should appear")
	g.Expect(simulators.SimulateJobComplete(ctx, c, dbSyncKey)).To(Succeed(), "simulate db-sync Job completion")

	schemaCheckKey := client.ObjectKey{Namespace: ns, Name: fmt.Sprintf("%s-schema-check", ksName)}
	g.Eventually(func() error {
		return c.Get(ctx, schemaCheckKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "schema-check Job should appear")
	g.Expect(simulators.SimulateJobComplete(ctx, c, schemaCheckKey)).To(Succeed(), "simulate schema-check Job completion")

	waitForCondition(t, ctx, c, key, "DatabaseReady", metav1.ConditionTrue, eventuallyTimeout)

	deployKey := client.ObjectKey{Namespace: ns, Name: ksName}
	deploy := &appsv1.Deployment{}
	g.Eventually(func() error {
		return c.Get(ctx, deployKey, deploy)
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "Deployment should appear")
	g.Expect(simulators.SimulateDeploymentReady(ctx, c, deployKey, ptr.Deref(deploy.Spec.Replicas, 1))).
		To(Succeed(), "simulate Deployment ready")

	waitForCondition(t, ctx, c, key, "DeploymentReady", metav1.ConditionTrue, eventuallyTimeout)
}

// integrationKeystoneWithGateway returns a brownfield Keystone CR configured
// with spec.gateway pointing at testGatewayParentName and the given hostname
// (CC-0065, REQ-001).
func integrationKeystoneWithGateway(name, namespace, hostname string) *keystonev1alpha1.Keystone {
	ks := integrationBrownfieldKeystone(name, namespace)
	ks.Spec.Gateway = &keystonev1alpha1.GatewaySpec{
		ParentRef: keystonev1alpha1.GatewayParentRefSpec{Name: testGatewayParentName},
		Hostname:  hostname,
	}
	return ks
}

// simulateHTTPRouteAccepted writes an Accepted=True condition onto the
// HTTPRoute's status.parents list, emulating the Gateway controller that would
// otherwise produce this transition. It is required for isHTTPRouteAccepted to
// return true in envtest, where no Gateway controller is running
// (CC-0065, REQ-005).
func simulateHTTPRouteAccepted(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	route := &gatewayv1.HTTPRoute{}
	g.Expect(c.Get(ctx, key, route)).To(Succeed(), "get HTTPRoute to simulate acceptance")
	g.Expect(route.Spec.ParentRefs).NotTo(BeEmpty(), "HTTPRoute must have at least one parentRef")

	route.Status = gatewayv1.HTTPRouteStatus{
		RouteStatus: gatewayv1.RouteStatus{
			Parents: []gatewayv1.RouteParentStatus{
				{
					ParentRef:      route.Spec.ParentRefs[0],
					ControllerName: gatewayv1.GatewayController("envtest.c5c3.io/fake-gateway-controller"),
					Conditions: []metav1.Condition{
						{
							Type:               string(gatewayv1.RouteConditionAccepted),
							Status:             metav1.ConditionTrue,
							Reason:             "Accepted",
							Message:            "simulated Accepted=True for envtest (CC-0065)",
							LastTransitionTime: metav1.Now(),
						},
					},
				},
			},
		},
	}
	g.Expect(c.Status().Update(ctx, route)).To(Succeed(), "update HTTPRoute status with simulated acceptance")
}

// TestIntegration_HTTPRoute_CreatedWhenGatewaySet verifies that configuring
// spec.gateway on a Keystone CR causes the operator to create an HTTPRoute
// with the correct parentRef/hostname/backendRef, to set HTTPRouteReady
// appropriately as acceptance is observed, and to own the HTTPRoute for
// garbage collection (CC-0065, REQ-001, REQ-003, REQ-005).
func TestIntegration_HTTPRoute_CreatedWhenGatewaySet(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-httproute-create-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	hostname := "keystone.example.com"
	ks := integrationKeystoneWithGateway("test-keystone", ns.Name, hostname)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveReconciliationToDeployment(t, ctx, c, ks.Name, ns.Name)

	// The HTTPRoute appears once reconcileHTTPRoute runs after DeploymentReady.
	routeKey := client.ObjectKey{Namespace: ns.Name, Name: ks.Name}
	route := &gatewayv1.HTTPRoute{}
	g.Eventually(func() error {
		return c.Get(ctx, routeKey, route)
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "HTTPRoute should be created when spec.gateway is set")

	// Validate parentRef (REQ-001) — Gateway referenced by name only.
	g.Expect(route.Spec.ParentRefs).To(HaveLen(1))
	g.Expect(string(route.Spec.ParentRefs[0].Name)).To(Equal(testGatewayParentName))

	// Validate hostname (REQ-001).
	g.Expect(route.Spec.Hostnames).To(HaveLen(1))
	g.Expect(string(route.Spec.Hostnames[0])).To(Equal(hostname))

	// Validate backendRef targets the {name} Service on port 5000 (CC-0095, REQ-004).
	g.Expect(route.Spec.Rules).To(HaveLen(1))
	g.Expect(route.Spec.Rules[0].BackendRefs).To(HaveLen(1))
	backend := route.Spec.Rules[0].BackendRefs[0]
	g.Expect(string(backend.Name)).To(Equal(ks.Name))
	g.Expect(backend.Port).NotTo(BeNil())
	g.Expect(int32(*backend.Port)).To(Equal(int32(5000)))

	// Validate owner reference points to the Keystone CR (garbage collection).
	g.Expect(route.OwnerReferences).NotTo(BeEmpty())
	g.Expect(route.OwnerReferences[0].Name).To(Equal(ks.Name))
	g.Expect(route.OwnerReferences[0].Kind).To(Equal("Keystone"))

	// Without a Gateway controller in envtest the route is not yet accepted,
	// so HTTPRouteReady must be False/HTTPRouteNotAccepted (REQ-005).
	crKey := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}
	cond := waitForCondition(t, ctx, c, crKey, conditionTypeHTTPRouteReady, metav1.ConditionFalse, eventuallyTimeout)
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotAccepted),
		"HTTPRouteReady reason should be HTTPRouteNotAccepted before the Gateway controller reports Accepted=True")

	// Simulate the Gateway controller writing Accepted=True on the HTTPRoute
	// status. The operator should pick this up on its next reconcile pass and
	// flip HTTPRouteReady to True/HTTPRouteAccepted (REQ-005).
	simulateHTTPRouteAccepted(t, ctx, c, routeKey)
	cond = waitForCondition(t, ctx, c, crKey, conditionTypeHTTPRouteReady, metav1.ConditionTrue, eventuallyTimeout)
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteAccepted),
		"HTTPRouteReady reason should be HTTPRouteAccepted once the Gateway controller accepts the route")
}

// TestIntegration_HTTPRoute_DeletedWhenGatewayRemoved verifies that removing
// spec.gateway from a CR deletes the HTTPRoute and transitions HTTPRouteReady
// to True/HTTPRouteNotRequired (CC-0065, REQ-002).
func TestIntegration_HTTPRoute_DeletedWhenGatewayRemoved(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-httproute-delete-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	ks := integrationKeystoneWithGateway("test-keystone", ns.Name, "keystone.example.com")
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveReconciliationToDeployment(t, ctx, c, ks.Name, ns.Name)

	routeKey := client.ObjectKey{Namespace: ns.Name, Name: ks.Name}
	g.Eventually(func() error {
		return c.Get(ctx, routeKey, &gatewayv1.HTTPRoute{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "HTTPRoute should be created initially")

	// Remove spec.gateway via a spec patch to force the reconciler to delete
	// the HTTPRoute (REQ-002). Use a retry-loop against optimistic concurrency
	// rejections by re-reading and re-patching until the update sticks.
	crKey := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}
	g.Eventually(func() error {
		current := &keystonev1alpha1.Keystone{}
		if err := c.Get(ctx, crKey, current); err != nil {
			return err
		}
		current.Spec.Gateway = nil
		return c.Update(ctx, current)
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "clear spec.gateway on the CR")

	// The HTTPRoute should be deleted.
	g.Eventually(func() bool {
		err := c.Get(ctx, routeKey, &gatewayv1.HTTPRoute{})
		return apierrors.IsNotFound(err)
	}, eventuallyTimeout, pollInterval).Should(BeTrue(), "HTTPRoute should be deleted after spec.gateway is removed")

	// HTTPRouteReady should transition to True/HTTPRouteNotRequired (REQ-002).
	cond := waitForCondition(t, ctx, c, crKey, conditionTypeHTTPRouteReady, metav1.ConditionTrue, eventuallyTimeout)
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotRequired),
		"HTTPRouteReady reason should be HTTPRouteNotRequired when spec.gateway is nil")
}

// TestIntegration_HTTPRoute_UpdatedWhenGatewayChanged verifies that changing
// spec.gateway.hostname on a CR updates the existing HTTPRoute in place
// without creating a duplicate (CC-0065, REQ-001).
func TestIntegration_HTTPRoute_UpdatedWhenGatewayChanged(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-httproute-update-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	originalHostname := "keystone.example.com"
	ks := integrationKeystoneWithGateway("test-keystone", ns.Name, originalHostname)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveReconciliationToDeployment(t, ctx, c, ks.Name, ns.Name)

	routeKey := client.ObjectKey{Namespace: ns.Name, Name: ks.Name}
	g.Eventually(func() error {
		return c.Get(ctx, routeKey, &gatewayv1.HTTPRoute{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "HTTPRoute should be created initially")

	// Patch the hostname. The reconciler should update the existing HTTPRoute
	// instead of creating a duplicate (REQ-001).
	updatedHostname := "auth.example.com"
	crKey := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}
	g.Eventually(func() error {
		current := &keystonev1alpha1.Keystone{}
		if err := c.Get(ctx, crKey, current); err != nil {
			return err
		}
		current.Spec.Gateway.Hostname = updatedHostname
		return c.Update(ctx, current)
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "patch spec.gateway.hostname")

	// The HTTPRoute hostname should reflect the new spec value.
	g.Eventually(func() string {
		route := &gatewayv1.HTTPRoute{}
		if err := c.Get(ctx, routeKey, route); err != nil {
			return ""
		}
		if len(route.Spec.Hostnames) == 0 {
			return ""
		}
		return string(route.Spec.Hostnames[0])
	}, eventuallyTimeout, pollInterval).Should(Equal(updatedHostname),
		"HTTPRoute.Spec.Hostnames should reflect the new spec.gateway.hostname")

	// No duplicate HTTPRoute should exist in the namespace.
	routes := &gatewayv1.HTTPRouteList{}
	g.Expect(c.List(ctx, routes, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(routes.Items).To(HaveLen(1), "exactly one HTTPRoute should exist after update")
}

// TestIntegration_HTTPRoute_EndpointDerivedFromGateway verifies that when
// spec.gateway is set, status.endpoint reflects the externally reachable URL
// https://{hostname}/v3 instead of the cluster-local Service DNS name
// (CC-0065, REQ-004).
func TestIntegration_HTTPRoute_EndpointDerivedFromGateway(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-httproute-endpoint-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	hostname := "keystone.example.com"
	ks := integrationKeystoneWithGateway("test-keystone", ns.Name, hostname)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveReconciliationToDeployment(t, ctx, c, ks.Name, ns.Name)

	// status.endpoint is set by reconcileDeployment after DeploymentReady=True.
	crKey := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}
	expectedEndpoint := fmt.Sprintf("https://%s/v3", hostname)
	g.Eventually(func() string {
		updated := &keystonev1alpha1.Keystone{}
		if err := c.Get(ctx, crKey, updated); err != nil {
			return ""
		}
		return updated.Status.Endpoint
	}, eventuallyTimeout, pollInterval).Should(Equal(expectedEndpoint),
		"status.endpoint should reflect https://{hostname}/v3 when spec.gateway is set")
}

// --- Task CC-0078/4.1: Finalizer lifecycle — managed mode (REQ-002, CC-0078) ---

// TestIntegration_FinalizerLifecycle_AddAndRemove verifies that the Keystone
// reconciler installs the finalizer on first observation of a managed-mode CR,
// and that deleting the CR drives finalizeDatabaseResources to issue Delete on
// every MariaDB Database, User, and Grant CR owned by the Keystone, followed
// by release of the Keystone finalizer so the CR is reclaimed from etcd
// (CC-0078, REQ-002).
func TestIntegration_FinalizerLifecycle_AddAndRemove(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-finalizer-managed-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	// Create a ready MariaDB cluster CR so the reconciler's cluster health
	// check passes (CC-0047).
	mdbCluster := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "mariadb", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, mdbCluster)).To(Succeed(), "create MariaDB cluster CR")
	g.Expect(simulators.SimulateMariaDBReady(ctx, c, client.ObjectKey{Namespace: ns.Name, Name: "mariadb"}, 1)).
		To(Succeed(), "simulate MariaDB cluster ready")

	// Create managed-mode Keystone CR.
	ks := integrationManagedKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())
	key := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}

	// Finalizer must be installed on first reconcile so a subsequent delete
	// is trapped and routed through reconcileDelete (CC-0078, REQ-001).
	g.Eventually(func() []string {
		ksState := &keystonev1alpha1.Keystone{}
		if err := c.Get(ctx, key, ksState); err != nil {
			return nil
		}
		return ksState.Finalizers
	}, eventuallyTimeout, pollInterval).Should(ContainElement(keystoneFinalizer),
		"Keystone CR should carry the MariaDB finalizer after first reconcile")

	// Drive the reconciler through the managed-mode database phase so that
	// Database, User, and Grant CRs actually exist when the Keystone CR is
	// deleted — otherwise finalizeDatabaseResources would have nothing to do.
	dbKey := client.ObjectKey{Namespace: ns.Name, Name: ks.Name}
	userKey := client.ObjectKey{Namespace: ns.Name, Name: ks.Name}
	grantKey := client.ObjectKey{Namespace: ns.Name, Name: ks.Name}

	g.Eventually(func() error {
		return c.Get(ctx, dbKey, &mariadbv1alpha1.Database{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "MariaDB Database CR should be created")
	g.Expect(simulators.SimulateDatabaseReady(ctx, c, dbKey)).To(Succeed())

	g.Eventually(func() error {
		return c.Get(ctx, userKey, &mariadbv1alpha1.User{})
	}, eventuallyLongTimeout, pollInterval).Should(Succeed(), "MariaDB User CR should be created")
	g.Expect(simulators.SimulateUserReady(ctx, c, userKey)).To(Succeed())

	g.Eventually(func() error {
		return c.Get(ctx, grantKey, &mariadbv1alpha1.Grant{})
	}, eventuallyLongTimeout, pollInterval).Should(Succeed(), "MariaDB Grant CR should be created")

	// Simulate ESO adopting both backup PushSecrets so the openbao-finalizer
	// Pass-0 adoption wait passes and the full deletion chain can run.
	// Without ESO in envtest both PushSecrets would remain unadopted and the
	// OpenBao finalizer would block forever, shadowing the MariaDB-finalizer
	// assertion this test is meant to make (CC-0091, REQ-001, REQ-003).
	fernetBackupKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-fernet-keys-backup", ks.Name)}
	credBackupKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-credential-keys-backup", ks.Name)}
	for _, key := range []client.ObjectKey{fernetBackupKey, credBackupKey} {
		g.Eventually(func() error {
			return c.Get(ctx, key, &esov1alpha1.PushSecret{})
		}, eventuallyTimeout, pollInterval).Should(Succeed(),
			"PushSecret %s should be provisioned", key)
		addESOFinalizerToPushSecret(t, ctx, c, key)
	}

	// Delete the Keystone CR; the API server sets DeletionTimestamp but blocks
	// removal from etcd while the finalizer is present.
	g.Expect(c.Delete(ctx, ks)).To(Succeed(), "delete Keystone CR")

	// After Pass-1 has issued Delete on both PushSecrets, clear the ESO
	// finalizers so the API server garbage-collects them — this mimics ESO
	// finishing its kv-v2 purge and is what allows Pass-2 to observe NotFound
	// and release the openbao finalizer (CC-0091, REQ-001).
	g.Eventually(func(ig Gomega) {
		for _, key := range []client.ObjectKey{fernetBackupKey, credBackupKey} {
			ps := &esov1alpha1.PushSecret{}
			ig.Expect(c.Get(ctx, key, ps)).To(Succeed(),
				"PushSecret %s should still exist while ESO finalizer is held", key)
			ig.Expect(ps.GetDeletionTimestamp().IsZero()).To(BeFalse(),
				"PushSecret %s should be Terminating after Pass-1 Delete", key)
		}
	}, eventuallyTimeout, pollInterval).Should(Succeed())
	clearESOFinalizerFromPushSecret(t, ctx, c, fernetBackupKey)
	clearESOFinalizerFromPushSecret(t, ctx, c, credBackupKey)

	// Every MariaDB CR must be reclaimed. In envtest there is no MariaDB
	// operator, so Delete resolves synchronously; in production the MariaDB
	// operator completes the teardown asynchronously after the Keystone CR is
	// gone — but the finalizer has guaranteed a Delete was issued on each CR.
	g.Eventually(func(ig Gomega) {
		ig.Expect(apierrors.IsNotFound(c.Get(ctx, dbKey, &mariadbv1alpha1.Database{}))).
			To(BeTrue(), "Database CR should be deleted")
		ig.Expect(apierrors.IsNotFound(c.Get(ctx, userKey, &mariadbv1alpha1.User{}))).
			To(BeTrue(), "User CR should be deleted")
		ig.Expect(apierrors.IsNotFound(c.Get(ctx, grantKey, &mariadbv1alpha1.Grant{}))).
			To(BeTrue(), "Grant CR should be deleted")
	}, eventuallyLongTimeout, pollInterval).Should(Succeed())

	// The reconciler releases the finalizer in the same pass that issued the
	// Deletes, so the API server garbage-collects the Keystone CR without
	// waiting on the MariaDB operator (CC-0078, REQ-002).
	g.Eventually(func() bool {
		return apierrors.IsNotFound(c.Get(ctx, key, &keystonev1alpha1.Keystone{}))
	}, eventuallyLongTimeout, pollInterval).Should(BeTrue(),
		"Keystone CR should be fully removed from etcd after finalizer release")
}

// --- Task CC-0078/4.2: Finalizer lifecycle — brownfield mode (REQ-002, CC-0078) ---

// TestIntegration_FinalizerBrownfieldDeletion verifies that a brownfield
// Keystone CR (Host-only, no ClusterRef) also carries the finalizer and that
// deletion completes cleanly without any MariaDB CR operations — since none
// were ever created (CC-0078, REQ-002).
func TestIntegration_FinalizerBrownfieldDeletion(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-finalizer-brownfield-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())
	key := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}

	// Finalizer must be installed even in brownfield mode so the Reconcile
	// path is uniform across both modes (CC-0078, REQ-001).
	g.Eventually(func() []string {
		ksState := &keystonev1alpha1.Keystone{}
		if err := c.Get(ctx, key, ksState); err != nil {
			return nil
		}
		return ksState.Finalizers
	}, eventuallyTimeout, pollInterval).Should(ContainElement(keystoneFinalizer),
		"brownfield Keystone CR should carry the MariaDB finalizer")

	// Brownfield mode never creates MariaDB CRs; assert they are absent before
	// deletion so we can attribute post-deletion NotFound to "never existed"
	// rather than "deleted by the finalizer."
	mdbKey := client.ObjectKey{Namespace: ns.Name, Name: ks.Name}
	g.Expect(apierrors.IsNotFound(c.Get(ctx, mdbKey, &mariadbv1alpha1.Database{}))).
		To(BeTrue(), "brownfield should not create a Database CR")
	g.Expect(apierrors.IsNotFound(c.Get(ctx, mdbKey, &mariadbv1alpha1.User{}))).
		To(BeTrue(), "brownfield should not create a User CR")
	g.Expect(apierrors.IsNotFound(c.Get(ctx, mdbKey, &mariadbv1alpha1.Grant{}))).
		To(BeTrue(), "brownfield should not create a Grant CR")

	// Brownfield Keystones still run reconcileFernetKeys / reconcileCredentialKeys,
	// so the backup PushSecrets exist and Pass-0 of finalizeOpenBaoSecrets
	// would block until ESO adopts them. Simulate ESO adoption (both
	// finalizers) so the deletion chain can run through Pass-1; we clear the
	// finalizers after Delete so the PushSecrets GC and Pass-2 releases the
	// openbao finalizer (CC-0091, REQ-001, REQ-003).
	fernetBackupKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-fernet-keys-backup", ks.Name)}
	credBackupKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-credential-keys-backup", ks.Name)}
	for _, key := range []client.ObjectKey{fernetBackupKey, credBackupKey} {
		g.Eventually(func() error {
			return c.Get(ctx, key, &esov1alpha1.PushSecret{})
		}, eventuallyTimeout, pollInterval).Should(Succeed(),
			"PushSecret %s should be provisioned", key)
		addESOFinalizerToPushSecret(t, ctx, c, key)
	}

	g.Expect(c.Delete(ctx, ks)).To(Succeed(), "delete brownfield Keystone CR")

	// Wait for Pass-1 Delete to flip both PushSecrets into Terminating, then
	// clear the ESO finalizers to mimic ESO finishing its kv-v2 purge so the
	// API server GCs the PushSecrets and Pass-2 can release the openbao
	// finalizer (CC-0091, REQ-001).
	g.Eventually(func(ig Gomega) {
		for _, key := range []client.ObjectKey{fernetBackupKey, credBackupKey} {
			ps := &esov1alpha1.PushSecret{}
			ig.Expect(c.Get(ctx, key, ps)).To(Succeed(),
				"PushSecret %s should still exist while ESO finalizer is held", key)
			ig.Expect(ps.GetDeletionTimestamp().IsZero()).To(BeFalse(),
				"PushSecret %s should be Terminating after Pass-1 Delete", key)
		}
	}, eventuallyTimeout, pollInterval).Should(Succeed())
	clearESOFinalizerFromPushSecret(t, ctx, c, fernetBackupKey)
	clearESOFinalizerFromPushSecret(t, ctx, c, credBackupKey)

	// finalizeDatabaseResources treats every NotFound Delete as success, so
	// the first pass through reconcileDelete releases the finalizer and the
	// API server removes the CR from etcd (CC-0078, REQ-002).
	g.Eventually(func() bool {
		return apierrors.IsNotFound(c.Get(ctx, key, &keystonev1alpha1.Keystone{}))
	}, eventuallyTimeout, pollInterval).Should(BeTrue(),
		"brownfield Keystone CR should be removed from etcd without MariaDB operations")

	// Re-check that no MariaDB CRs were created at any point (i.e., the
	// finalizer did not accidentally reify them).
	g.Expect(apierrors.IsNotFound(c.Get(ctx, mdbKey, &mariadbv1alpha1.Database{}))).
		To(BeTrue(), "no Database CR should exist after brownfield deletion")
	g.Expect(apierrors.IsNotFound(c.Get(ctx, mdbKey, &mariadbv1alpha1.User{}))).
		To(BeTrue(), "no User CR should exist after brownfield deletion")
	g.Expect(apierrors.IsNotFound(c.Get(ctx, mdbKey, &mariadbv1alpha1.Grant{}))).
		To(BeTrue(), "no Grant CR should exist after brownfield deletion")
}

// --- Task 4.1 / 4.2: split-compute-write rotation integration tests (CC-0081) ---

// eventuallyFindKeystoneEvent polls the Events API for an Event on the given
// Keystone CR with the matching reason and type. Returns the first match or
// fails the Eventually assertion (CC-0081).
func eventuallyFindKeystoneEvent(t testing.TB, ctx context.Context, c client.Client, ks *keystonev1alpha1.Keystone, reason, eventType string) corev1.Event {
	t.Helper()
	g := NewGomegaWithT(t)

	var match corev1.Event
	g.Eventually(func(ig Gomega) {
		events := &corev1.EventList{}
		ig.Expect(c.List(ctx, events, client.InNamespace(ks.Namespace))).To(Succeed())
		for _, e := range events.Items {
			if e.InvolvedObject.UID == ks.UID && e.Reason == reason && e.Type == eventType {
				match = e
				return
			}
		}
		ig.Expect(fmt.Errorf("no %s %s event yet for %s/%s", eventType, reason, ks.Namespace, ks.Name)).NotTo(HaveOccurred())
	}, eventuallyTimeout, pollInterval).Should(Succeed(),
		fmt.Sprintf("CC-0081: expected %s/%s event on Keystone %s/%s", eventType, reason, ks.Namespace, ks.Name))
	return match
}

// setupRotationEnvTest drives the envtest controller through its initial
// reconciliation so the staging Secret, production Fernet Secret, and
// Keystone CR are all live for rotation-apply scenarios (CC-0081, Task 4.1).
// It skips the test when envtest is unavailable, creates a per-test Namespace
// using nsGenerateName as the GenerateName prefix, seeds the namespace with
// prerequisite Secrets, creates a brownfield Keystone named "test-keystone",
// drives full reconciliation, and re-fetches the CR so the returned object
// carries a fresh UID/ResourceVersion for subsequent Updates and event
// lookups. Tests that need to vary the Keystone shape (managed mode, custom
// spec) must not use this helper.
func setupRotationEnvTest(t *testing.T, nsGenerateName string) (
	client.Client, context.Context, *keystonev1alpha1.Keystone, *corev1.Namespace,
) {
	t.Helper()
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: nsGenerateName}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	g.Expect(c.Get(ctx, types.NamespacedName{Name: ks.Name, Namespace: ns.Name}, ks)).
		To(Succeed(), "re-fetch Keystone CR post-reconcile (CC-0081)")

	return c, ctx, ks, ns
}

// TestRotationApplyEndToEnd_EnvTest drives the full split-compute-write Fernet
// rotation flow in envtest (CC-0081, Task 4.1): the operator creates the empty
// staging Secret, the test simulates the CronJob PATCH with valid keys and a
// completion annotation, and the reconciler copies the keys onto the production
// Secret, deletes the staging Secret, and emits a FernetKeysRotated event.
func TestRotationApplyEndToEnd_EnvTest(t *testing.T) {
	g := NewGomegaWithT(t)
	c, ctx, ks, ns := setupRotationEnvTest(t, "test-rotation-apply-")

	// Assert the staging Secret exists with empty Data, correct label, and
	// an OwnerReference back to the Keystone CR (CC-0081, REQ-005).
	stagingKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-fernet-keys-rotation", ks.Name)}
	staging := &corev1.Secret{}
	g.Expect(c.Get(ctx, stagingKey, staging)).To(Succeed(), "staging Secret should exist")
	g.Expect(staging.Data).To(BeEmpty(), "staging Secret Data should start empty (CC-0081)")
	g.Expect(staging.Labels).To(HaveKeyWithValue(StagingSecretLabelKey, "fernet-keys"))
	var ownsKs bool
	for _, or := range staging.OwnerReferences {
		if or.UID == ks.UID {
			ownsKs = true
			break
		}
	}
	g.Expect(ownsKs).To(BeTrue(), "staging Secret should be owned by the Keystone CR (CC-0081)")

	// Capture the production Secret's pre-rotation Data for comparison below.
	prodKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-fernet-keys", ks.Name)}
	prodBefore := &corev1.Secret{}
	g.Expect(c.Get(ctx, prodKey, prodBefore)).To(Succeed(), "production Fernet Secret should exist")
	g.Expect(prodBefore.Data).NotTo(BeEmpty(), "production Secret should have been populated by the initial reconcile")

	// Simulate the CronJob PATCH with the exact write shape emitted by
	// fernet_rotate.sh: a strategic-merge PATCH carrying only the `data`
	// and `metadata.annotations` subtrees (CC-0081, REQ-005, REQ-006, TE2).
	// This exercises the real apply path end-to-end rather than masking it
	// with a full-object Update.
	stagedData := map[string][]byte{}
	for i := 0; i < 3; i++ {
		k, err := generateFernetKey()
		g.Expect(err).NotTo(HaveOccurred())
		stagedData[fmt.Sprintf("%d", i)] = []byte(k)
	}
	g.Expect(cronJobStrategicMergePatch(ctx, c, stagingKey, stagedData)).To(Succeed(),
		"stage CronJob output onto staging Secret (CC-0081)")

	// Eventually: production Secret Data == staged Data (CC-0081, REQ-005).
	g.Eventually(func(ig Gomega) {
		got := &corev1.Secret{}
		ig.Expect(c.Get(ctx, prodKey, got)).To(Succeed())
		ig.Expect(got.Data).To(HaveLen(len(stagedData)))
		for k, v := range stagedData {
			ig.Expect(got.Data).To(HaveKeyWithValue(k, v))
		}
	}, eventuallyTimeout, pollInterval).Should(Succeed(),
		"production Secret Data should be replaced with staged keys (CC-0081)")

	// Eventually: the staging Secret's staged data and completion annotation
	// are gone. The operator deletes the staging Secret after applying and
	// ensureFernetStagingSecret re-creates it empty on the next reconcile —
	// so either NotFound OR a present-but-empty Secret without the
	// completion annotation is the correct terminal state (CC-0081).
	g.Eventually(func(ig Gomega) {
		got := &corev1.Secret{}
		err := c.Get(ctx, stagingKey, got)
		if apierrors.IsNotFound(err) {
			return
		}
		ig.Expect(err).NotTo(HaveOccurred())
		ig.Expect(got.Data).To(BeEmpty(),
			"staging Secret Data should be cleared after apply (CC-0081)")
		ig.Expect(got.Annotations).NotTo(HaveKey(RotationCompletedAnnotation),
			"staging Secret completion annotation should be gone after apply (CC-0081)")
	}, eventuallyTimeout, pollInterval).Should(Succeed(),
		"staging Secret should be deleted or reset after successful apply (CC-0081)")

	// Eventually: a Normal FernetKeysRotated event is emitted on the Keystone CR.
	eventuallyFindKeystoneEvent(t, ctx, c, ks, "FernetKeysRotated", corev1.EventTypeNormal)
}

// TestRotationApplyRejectsMalformedKeys_EnvTest verifies that a staging Secret
// with malformed Fernet keys (32-byte raw strings instead of 44-byte base64url)
// is rejected by the operator's validation step: production Secret is
// untouched, staging Secret is retained for inspection, and a
// RotationRejected Warning event is emitted (CC-0081, Task 4.2, REQ-006).
func TestRotationApplyRejectsMalformedKeys_EnvTest(t *testing.T) {
	g := NewGomegaWithT(t)
	c, ctx, ks, ns := setupRotationEnvTest(t, "test-rotation-reject-")

	// Snapshot production Secret Data before staging malformed keys (CC-0081).
	prodKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-fernet-keys", ks.Name)}
	prodBefore := &corev1.Secret{}
	g.Expect(c.Get(ctx, prodKey, prodBefore)).To(Succeed())
	g.Expect(prodBefore.Data).NotTo(BeEmpty())
	dataBefore := map[string][]byte{}
	for k, v := range prodBefore.Data {
		dataBefore[k] = append([]byte(nil), v...)
	}

	// Stage malformed keys: 32-byte raw strings rather than 44-byte base64url
	// (fails validateRotationOutput on length, CC-0081, REQ-006).
	stagingKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-fernet-keys-rotation", ks.Name)}
	malformed := map[string][]byte{
		"0": []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa0"),
		"1": []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb1"),
		"2": []byte("ccccccccccccccccccccccccccccccc2"),
	}
	// Use the same strategic-merge PATCH shape the CronJob actually emits so
	// the validation rejection is exercised against the real write path
	// (CC-0081, TE2).
	g.Expect(cronJobStrategicMergePatch(ctx, c, stagingKey, malformed)).To(Succeed(),
		"stage malformed rotation output (CC-0081)")

	// Eventually: Warning RotationRejected event appears on the Keystone CR.
	eventuallyFindKeystoneEvent(t, ctx, c, ks, "RotationRejected", corev1.EventTypeWarning)

	// Consistently: production Secret Data is unchanged (CC-0081, REQ-006).
	g.Consistently(func(ig Gomega) {
		got := &corev1.Secret{}
		ig.Expect(c.Get(ctx, prodKey, got)).To(Succeed())
		ig.Expect(got.Data).To(Equal(dataBefore),
			"production Secret must not be mutated by a rejected rotation (CC-0081)")
	}, 2*time.Second, pollInterval).Should(Succeed())

	// Staging Secret is retained with the malformed data + annotation (CC-0081).
	retained := &corev1.Secret{}
	g.Expect(c.Get(ctx, stagingKey, retained)).To(Succeed(),
		"staging Secret should be retained after a rejected apply (CC-0081)")
	g.Expect(retained.Data).To(Equal(malformed))
	g.Expect(retained.Annotations).To(HaveKey(RotationCompletedAnnotation))
}

// cronJobStrategicMergePatch emits the exact strategic-merge PATCH shape the
// fernet_rotate.sh / credential_rotate.sh CronJob scripts send to the staging
// Secret — `{"metadata":{"annotations":{"forge.c5c3.io/rotation-completed-at":"..."}}, "data":{...}}`
// — and writes it via the controller-runtime client. Using this in envtest
// (rather than c.Update) exercises the real write path so the operator's
// apply semantics are covered end-to-end (CC-0081, TE2).
func cronJobStrategicMergePatch(
	ctx context.Context,
	c client.Client,
	key client.ObjectKey,
	data map[string][]byte,
) error {
	payload := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{
				RotationCompletedAnnotation: time.Now().UTC().Format(time.RFC3339),
			},
		},
		// json.Marshal encodes []byte values as base64 strings, which matches
		// the Secret.Data wire format.
		"data": data,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshalling CronJob PATCH payload: %w", err)
	}
	target := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
	}
	return c.Patch(ctx, target, client.RawPatch(types.StrategicMergePatchType, raw))
}

// TestRotationApplyReplacesDisjointIndices_EnvTest seeds the production
// Fernet Secret with a key at an index the staging payload does NOT mention,
// then simulates the CronJob PATCH with a 3-key staging payload at
// `{"0","1","2"}`, and asserts the operator's apply path fully replaces the
// production Data (length == staging length, stale disjoint index removed).
// This is the envtest regression guard for the strategic-merge-vs-replace
// bug (CC-0081, T1).
func TestRotationApplyReplacesDisjointIndices_EnvTest(t *testing.T) {
	g := NewGomegaWithT(t)
	c, ctx, ks, ns := setupRotationEnvTest(t, "test-rotation-disjoint-")

	// Seed production with a key at index "9" that the staging payload below
	// does NOT mention. Under strategic-merge-by-key (the bug this test
	// guards against) "9" would survive; under full-replacement Update it is
	// removed (CC-0081).
	prodKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-fernet-keys", ks.Name)}
	prodBefore := &corev1.Secret{}
	g.Expect(c.Get(ctx, prodKey, prodBefore)).To(Succeed())
	prodBefore.Data["9"] = []byte("pre-existing-disjoint-stale-key")
	g.Expect(c.Update(ctx, prodBefore)).To(Succeed(),
		"seed production with a disjoint index (CC-0081, T1)")

	// Stage a 3-key payload at indices {"0","1","2"} via the real CronJob
	// strategic-merge PATCH shape.
	stagingKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-fernet-keys-rotation", ks.Name)}
	stagedData := map[string][]byte{}
	for i := 0; i < 3; i++ {
		k, err := generateFernetKey()
		g.Expect(err).NotTo(HaveOccurred())
		stagedData[fmt.Sprintf("%d", i)] = []byte(k)
	}
	g.Expect(cronJobStrategicMergePatch(ctx, c, stagingKey, stagedData)).To(Succeed(),
		"stage CronJob output onto staging Secret (CC-0081, T1)")

	// Eventually: production Data exactly equals staging — length, keys,
	// and values. The disjoint stale index "9" must be gone.
	g.Eventually(func(ig Gomega) {
		got := &corev1.Secret{}
		ig.Expect(c.Get(ctx, prodKey, got)).To(Succeed())
		ig.Expect(got.Data).To(HaveLen(len(stagedData)),
			"production Data length must equal staging length (CC-0081, REQ-006)")
		ig.Expect(got.Data).NotTo(HaveKey("9"),
			"stale disjoint index must be removed by full-replacement Update (CC-0081)")
		for k, v := range stagedData {
			ig.Expect(got.Data).To(HaveKeyWithValue(k, v))
		}
	}, eventuallyTimeout, pollInterval).Should(Succeed(),
		"production Secret must fully replace to staging keys, not merge (CC-0081, T1)")

	// A Normal FernetKeysRotated event is emitted on the Keystone CR.
	eventuallyFindKeystoneEvent(t, ctx, c, ks, "FernetKeysRotated", corev1.EventTypeNormal)
}

// --- CC-0080: ConfigMap/Secret separation via oslo.config env overrides ---

// TestIntegration_KeystonePodReachesDatabaseViaEnvOverride verifies the
// CC-0080 split: the keystone.conf ConfigMap must carry only the placeholder
// URL (never the real DB password), while the real URL is materialised into
// the derived <name>-db-connection Secret and injected into the Deployment
// pod spec via the OS_DATABASE__CONNECTION env var (CC-0080, REQ-001,
// REQ-002, REQ-003).
func TestIntegration_KeystonePodReachesDatabaseViaEnvOverride(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cc0080-env-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	// REQ-001: the rendered keystone.conf ConfigMap carries the placeholder
	// URL and MUST NOT contain the upstream DB password.
	configMaps := &corev1.ConfigMapList{}
	g.Expect(c.List(ctx, configMaps, client.InNamespace(ns.Name))).To(Succeed())
	var conf string
	for _, cm := range configMaps.Items {
		if strings.HasPrefix(cm.Name, "test-keystone-config-") {
			conf = cm.Data["keystone.conf"]
			break
		}
	}
	g.Expect(conf).NotTo(BeEmpty(), "keystone.conf should exist in a test-keystone-config-* ConfigMap")
	g.Expect(conf).To(ContainSubstring(dbConnectionPlaceholder),
		"keystone.conf [database] connection must be the placeholder (CC-0080, REQ-001)")
	// Guard against the specific leakage pattern: the rendered DB URL fragment
	// "<user>:<password>@" produced by url.UserPassword. createPrerequisites
	// seeds username=keystone, password=secret (CC-0080, REQ-001).
	g.Expect(conf).NotTo(ContainSubstring("keystone:secret@"),
		"keystone.conf must not contain the upstream DB credentials (CC-0080, REQ-001)")

	// REQ-002: derived Secret exists with a single "connection" key whose
	// value is a valid pymysql URL carrying the real credentials.
	derivedKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-db-connection", ks.Name)}
	derived := &corev1.Secret{}
	g.Expect(c.Get(ctx, derivedKey, derived)).To(Succeed(),
		"derived db-connection Secret must exist (CC-0080, REQ-002)")
	g.Expect(derived.Data).To(HaveLen(1), "derived Secret must contain exactly one key")
	connStr := string(derived.Data[dbConnectionSecretKey])
	g.Expect(connStr).To(HavePrefix("mysql+pymysql://"),
		"derived connection must be a pymysql URL (CC-0080, REQ-002)")
	g.Expect(connStr).To(ContainSubstring("keystone:secret@"),
		"derived connection must carry the upstream username and password")
	g.Expect(connStr).To(ContainSubstring("db.example.com"),
		"derived connection must carry the database host")
	g.Expect(derived.OwnerReferences).NotTo(BeEmpty(),
		"derived Secret must be owner-referenced by the Keystone CR")

	// REQ-003: the Deployment pod spec injects OS_DATABASE__CONNECTION sourced
	// from the derived Secret.
	deploy := &appsv1.Deployment{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: ks.Name}, deploy)).
		To(Succeed())
	container := findContainerByName(deploy.Spec.Template.Spec.Containers, "keystone")
	g.Expect(container).NotTo(BeNil(), "keystone container must exist")
	g.Expect(container.Env).To(ContainElement(buildDBConnectionEnvVar(ks)),
		"Deployment container must carry OS_DATABASE__CONNECTION from derived Secret (CC-0080, REQ-003)")
}

// TestIntegration_RecreateDerivedSecretWhenDeleted verifies that deleting the
// derived <name>-db-connection Secret triggers the secretToKeystoneMapper
// watch and causes reconciliation to re-create it (CC-0080, REQ-006).
func TestIntegration_RecreateDerivedSecretWhenDeleted(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cc0080-recreate-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	derivedKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-db-connection", ks.Name)}
	original := &corev1.Secret{}
	g.Expect(c.Get(ctx, derivedKey, original)).To(Succeed(),
		"derived db-connection Secret must exist before deletion")
	originalUID := original.UID
	originalConn := string(original.Data[dbConnectionSecretKey])
	g.Expect(originalConn).NotTo(BeEmpty(), "derived Secret must carry a connection value")

	// Delete the derived Secret out-of-band, then expect the watch-driven
	// reconcile to recreate it with the same contents (CC-0080, REQ-006).
	g.Expect(c.Delete(ctx, original)).To(Succeed())

	g.Eventually(func(g Gomega) {
		recreated := &corev1.Secret{}
		g.Expect(c.Get(ctx, derivedKey, recreated)).To(Succeed())
		// A fresh object: different UID from the deleted one.
		g.Expect(recreated.UID).NotTo(Equal(originalUID),
			"derived Secret must be a freshly created object after deletion")
		g.Expect(recreated.Data[dbConnectionSecretKey]).To(Equal([]byte(originalConn)),
			"recreated Secret must carry the same connection URL")
		g.Expect(recreated.OwnerReferences).NotTo(BeEmpty(),
			"recreated Secret must be owner-referenced by the Keystone CR")
	}, eventuallyTimeout, pollInterval).Should(Succeed(),
		"controller must recreate deleted db-connection Secret (CC-0080, REQ-006)")
}

// --- Task CC-0079/4.1 and 4.2: OpenBao finalizer lifecycle tests ---

// esoCleanupFinalizer matches the finalizer name that external-secrets adds to
// PushSecret CRs while it purges the remote kv-v2 path. Using the real
// finalizer string makes the tests exercise the same NotFound vs Terminating
// semantics the production controller sees (CC-0079, REQ-002, REQ-004).
const esoCleanupFinalizer = "external-secrets.io/cleanup"

// addESOFinalizerToPushSecret simulates full ESO adoption of the PushSecret by
// attaching both ESO-owned finalizers:
//
//   - esoPushSecretFinalizer is the adoption signal checked by Pass-0 of
//     finalizeOpenBaoSecrets. ESO's PushSecret controller installs this on
//     first reconcile, so its presence is the operator's evidence that ESO
//     will honour DeletionPolicy=Delete on a subsequent client.Delete
//     (CC-0091, REQ-001, REQ-007).
//   - esoCleanupFinalizer is the finalizer ESO holds while it purges the
//     remote kv-v2 path. Its presence keeps the PushSecret in Terminating
//     state after client.Delete instead of immediate etcd removal, which is
//     what Pass-2 of finalizeOpenBaoSecrets observes and surfaces as
//     OpenBaoFinalizerBlocked (CC-0079, REQ-002, REQ-004).
//
// Installing both mirrors the production pairing — ESO's DeletionPolicy=Delete
// branch only fires once adoption has happened, at which point both finalizers
// are on the object.
func addESOFinalizerToPushSecret(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	ps := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(ctx, key, ps)).To(Succeed(),
		"PushSecret %s must exist before adding ESO finalizers", key)

	changed := false
	if !controllerutil.ContainsFinalizer(ps, esoPushSecretFinalizer) {
		controllerutil.AddFinalizer(ps, esoPushSecretFinalizer)
		changed = true
	}
	if !controllerutil.ContainsFinalizer(ps, esoCleanupFinalizer) {
		controllerutil.AddFinalizer(ps, esoCleanupFinalizer)
		changed = true
	}
	if changed {
		g.Expect(c.Update(ctx, ps)).To(Succeed(),
			"add ESO finalizers to PushSecret %s", key)
	}
}

// addESOAdoptionFinalizerToPushSecret attaches only esoPushSecretFinalizer —
// the adoption signal. This simulates the narrow window after ESO has
// adopted a PushSecret (first reconcile has installed the adoption
// finalizer) but before a client.Delete has fired. Any finalizer still
// blocks etcd removal, so a subsequent Delete flips the object into
// Terminating exactly as it does with addESOFinalizerToPushSecret — use
// this helper when the test only needs to satisfy Pass-0 and does not
// care about modelling ESO's kv-v2 purge latency (CC-0091, REQ-001,
// REQ-003, REQ-007).
func addESOAdoptionFinalizerToPushSecret(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	ps := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(ctx, key, ps)).To(Succeed(),
		"PushSecret %s must exist before adding ESO adoption finalizer", key)
	if !controllerutil.ContainsFinalizer(ps, esoPushSecretFinalizer) {
		controllerutil.AddFinalizer(ps, esoPushSecretFinalizer)
		g.Expect(c.Update(ctx, ps)).To(Succeed(),
			"add ESO adoption finalizer to PushSecret %s", key)
	}
}

// clearESOFinalizerFromPushSecret removes both ESO-owned finalizers, letting
// the API server garbage-collect the already-Terminating PushSecret —
// mimicking ESO completing its kv-v2 purge and releasing the object
// (CC-0079, CC-0091, REQ-002, REQ-004).
func clearESOFinalizerFromPushSecret(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	g.Eventually(func() error {
		ps := &esov1alpha1.PushSecret{}
		if err := c.Get(ctx, key, ps); err != nil {
			return err
		}
		removedCleanup := controllerutil.RemoveFinalizer(ps, esoCleanupFinalizer)
		removedAdoption := controllerutil.RemoveFinalizer(ps, esoPushSecretFinalizer)
		if !removedCleanup && !removedAdoption {
			return nil
		}
		return c.Update(ctx, ps)
	}, eventuallyTimeout, pollInterval).Should(Succeed(),
		"clear ESO finalizers from PushSecret %s", key)
}

// TestIntegration_OpenBaoFinalizerLifecycle_AddAndRemove verifies that the
// Keystone reconciler installs keystoneOpenBaoFinalizer on first reconcile,
// provisions the fernet-keys-backup and credential-keys-backup PushSecrets
// with DeletionPolicy=Delete, and on deletion drives finalizeOpenBaoSecrets
// to Delete both PushSecrets. The test attaches ESO's cleanup finalizer to
// each PushSecret so that Delete flips them into Terminating state (matching
// production where ESO holds the object while it purges the kv-v2 path);
// clearing that finalizer then lets the API server garbage-collect them,
// which is what unblocks the Keystone CR from etcd (CC-0079, REQ-002,
// REQ-008).
func TestIntegration_OpenBaoFinalizerLifecycle_AddAndRemove(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-openbao-finalizer-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	// Managed mode exercises both the MariaDB (CC-0078) and OpenBao (CC-0079)
	// finalizers on the same CR, matching production where a Keystone with
	// spec.database.clusterRef carries both. A Ready MariaDB cluster CR keeps
	// reconcileDatabase's cluster-health gate happy (CC-0047).
	mdbCluster := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "mariadb", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, mdbCluster)).To(Succeed(), "create MariaDB cluster CR")
	g.Expect(simulators.SimulateMariaDBReady(ctx, c, client.ObjectKey{Namespace: ns.Name, Name: "mariadb"}, 1)).
		To(Succeed(), "simulate MariaDB cluster ready")

	ks := integrationManagedKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())
	ksKey := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}

	fernetKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-fernet-keys-backup", ks.Name)}
	credKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-credential-keys-backup", ks.Name)}

	// The OpenBao finalizer must be installed on first reconcile so a
	// subsequent delete is trapped through reconcileDeleteOpenBao (CC-0079,
	// REQ-001, REQ-006).
	g.Eventually(func() bool {
		ksState := &keystonev1alpha1.Keystone{}
		if err := c.Get(ctx, ksKey, ksState); err != nil {
			return false
		}
		return controllerutil.ContainsFinalizer(ksState, keystoneOpenBaoFinalizer)
	}, eventuallyTimeout, pollInterval).Should(BeTrue(),
		"Keystone CR should carry the OpenBao finalizer after first reconcile")

	// Both backup PushSecrets must be provisioned with DeletionPolicy=Delete
	// so that ESO purges the remote kv-v2 paths when the finalizer handler
	// Deletes them (CC-0079, REQ-008).
	for _, key := range []client.ObjectKey{fernetKey, credKey} {
		g.Eventually(func() error {
			return c.Get(ctx, key, &esov1alpha1.PushSecret{})
		}, eventuallyTimeout, pollInterval).Should(Succeed(),
			"PushSecret %s should be provisioned", key)

		ps := &esov1alpha1.PushSecret{}
		g.Expect(c.Get(ctx, key, ps)).To(Succeed())
		g.Expect(ps.Spec.DeletionPolicy).To(Equal(esov1alpha1.PushSecretDeletionPolicyDelete),
			"PushSecret %s must have DeletionPolicy=Delete so ESO purges the kv-v2 path", key)
	}

	// Attach the ESO cleanup finalizer to both PushSecrets before deleting
	// the CR. Without it the fake API server would remove them immediately on
	// the controller's Delete call, and the test would never exercise the
	// Terminating -> garbage-collected transition finalizeOpenBaoSecrets is
	// designed to handle (CC-0079, REQ-002).
	addESOFinalizerToPushSecret(t, ctx, c, fernetKey)
	addESOFinalizerToPushSecret(t, ctx, c, credKey)

	g.Expect(c.Delete(ctx, ks)).To(Succeed(), "delete Keystone CR")

	// finalizeOpenBaoSecrets now issues Delete on every backup PushSecret
	// up-front (pass 1) before verifying they are gone (pass 2), so both
	// PushSecrets transition to Terminating in parallel without ordering
	// constraints between them (CC-0079, REQ-002, REQ-004).
	g.Eventually(func(ig Gomega) {
		for _, key := range []client.ObjectKey{fernetKey, credKey} {
			ps := &esov1alpha1.PushSecret{}
			ig.Expect(c.Get(ctx, key, ps)).To(Succeed(),
				"PushSecret %s should still exist while ESO finalizer is held", key)
			ig.Expect(ps.GetDeletionTimestamp().IsZero()).To(BeFalse(),
				"PushSecret %s should be Terminating after CR deletion", key)
		}
	}, eventuallyTimeout, pollInterval).Should(Succeed())

	clearESOFinalizerFromPushSecret(t, ctx, c, fernetKey)
	clearESOFinalizerFromPushSecret(t, ctx, c, credKey)

	g.Eventually(func(ig Gomega) {
		ig.Expect(apierrors.IsNotFound(c.Get(ctx, fernetKey, &esov1alpha1.PushSecret{}))).
			To(BeTrue(), "fernet-keys-backup PushSecret should be garbage-collected")
		ig.Expect(apierrors.IsNotFound(c.Get(ctx, credKey, &esov1alpha1.PushSecret{}))).
			To(BeTrue(), "credential-keys-backup PushSecret should be garbage-collected")
	}, eventuallyLongTimeout, pollInterval).Should(Succeed())

	// Once both PushSecrets are NotFound, the next requeue drives
	// finalizeOpenBaoSecrets to done=true, releasing the OpenBao finalizer
	// (and the MariaDB finalizer was released earlier in the same termination
	// sequence), so the API server reclaims the Keystone CR from etcd
	// (CC-0079, REQ-002).
	g.Eventually(func() bool {
		return apierrors.IsNotFound(c.Get(ctx, ksKey, &keystonev1alpha1.Keystone{}))
	}, eventuallyLongTimeout, pollInterval).Should(BeTrue(),
		"Keystone CR should be removed from etcd after both finalizers release")
}

// TestIntegration_OpenBaoFinalizer_BlockedWhenPushSecretStuck verifies that
// when a backup PushSecret is held Terminating by ESO's cleanup finalizer,
// the reconciler keeps the Keystone CR alive and surfaces SecretsReady=False
// with reason OpenBaoFinalizerBlocked naming the stuck PushSecret. Clearing
// the finalizer then lets the termination complete and the CR is reclaimed
// (CC-0079, REQ-004, REQ-009).
func TestIntegration_OpenBaoFinalizer_BlockedWhenPushSecretStuck(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-openbao-blocked-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	mdbCluster := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "mariadb", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, mdbCluster)).To(Succeed(), "create MariaDB cluster CR")
	g.Expect(simulators.SimulateMariaDBReady(ctx, c, client.ObjectKey{Namespace: ns.Name, Name: "mariadb"}, 1)).
		To(Succeed(), "simulate MariaDB cluster ready")

	ks := integrationManagedKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())
	ksKey := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}

	fernetKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-fernet-keys-backup", ks.Name)}
	credKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-credential-keys-backup", ks.Name)}

	// Wait for both backup PushSecrets to be provisioned before we attach any
	// finalizer — otherwise the Update below races the reconciler's create.
	for _, key := range []client.ObjectKey{fernetKey, credKey} {
		g.Eventually(func() error {
			return c.Get(ctx, key, &esov1alpha1.PushSecret{})
		}, eventuallyTimeout, pollInterval).Should(Succeed(),
			"PushSecret %s should be provisioned", key)
	}

	// Attach the ESO cleanup finalizer ONLY to fernet-keys-backup so that a
	// subsequent client.Delete flips it into Terminating and stays there —
	// exercising Pass-2 of finalizeOpenBaoSecrets (CC-0079, REQ-004, REQ-009).
	addESOFinalizerToPushSecret(t, ctx, c, fernetKey)

	// Attach only the adoption finalizer to credential-keys-backup so Pass-0
	// of finalizeOpenBaoSecrets proceeds past it — otherwise the reconciler
	// would record WaitingForESOAdoption on credential-keys-backup and never
	// reach Pass-2 on fernet-keys-backup, shadowing the blocked condition
	// this test is meant to assert (CC-0091, REQ-001, REQ-003).
	addESOAdoptionFinalizerToPushSecret(t, ctx, c, credKey)

	g.Expect(c.Delete(ctx, ks)).To(Succeed(), "delete Keystone CR")

	// Because fernet-keys-backup is held Terminating, finalizeOpenBaoSecrets
	// returns done=false and records the blocked condition. The status update
	// persists through updateStatus so operators can see why the Keystone CR
	// has not finished deleting (CC-0079, REQ-004, REQ-009).
	g.Eventually(func(ig Gomega) {
		ksState := &keystonev1alpha1.Keystone{}
		ig.Expect(c.Get(ctx, ksKey, ksState)).To(Succeed())
		cond := meta.FindStatusCondition(ksState.Status.Conditions, "SecretsReady")
		ig.Expect(cond).NotTo(BeNil(), "SecretsReady condition must be present while finalizer is blocked")
		ig.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		ig.Expect(cond.Reason).To(Equal("OpenBaoFinalizerBlocked"))
		ig.Expect(cond.Message).To(ContainSubstring(fernetKey.Name),
			"blocked-condition message should name the stuck PushSecret")
	}, eventuallyLongTimeout, pollInterval).Should(Succeed())

	// The CR must still be present — the finalizer is holding it alive. Prove
	// it is not a stale Get by checking DeletionTimestamp is set.
	ksState := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, ksKey, ksState)).To(Succeed())
	g.Expect(ksState.GetDeletionTimestamp().IsZero()).To(BeFalse(),
		"Keystone CR should be Terminating while openbao finalizer is blocked")
	g.Expect(controllerutil.ContainsFinalizer(ksState, keystoneOpenBaoFinalizer)).To(BeTrue(),
		"openbao finalizer must still be present while blocked on stuck PushSecret")

	// Simulate ESO finishing its kv-v2 purge on both PushSecrets. The API
	// server garbage-collects them, the next reconcile observes them NotFound,
	// and the Keystone CR is reclaimed (CC-0079, CC-0091, REQ-002, REQ-004).
	clearESOFinalizerFromPushSecret(t, ctx, c, fernetKey)
	clearESOFinalizerFromPushSecret(t, ctx, c, credKey)

	g.Eventually(func() bool {
		return apierrors.IsNotFound(c.Get(ctx, ksKey, &keystonev1alpha1.Keystone{}))
	}, eventuallyLongTimeout, pollInterval).Should(BeTrue(),
		"Keystone CR should be removed from etcd after the stuck PushSecret clears")
}

// TestIntegrationKeystone_DeleteRacingESOAdoption exercises the Pass-0
// adoption wait in finalizeOpenBaoSecrets (CC-0091, REQ-001, REQ-003,
// REQ-007). When a Keystone CR is deleted before ESO has reconciled the
// backup PushSecrets — i.e. before esoPushSecretFinalizer has been
// installed — the operator must NOT issue Delete on those PushSecrets.
// A racing Delete in that window would remove the PushSecret from the API
// server outright, ESO would never observe the DeletionTimestamp, and the
// kv-v2 path in OpenBao would be orphaned (the CI incident in run
// 24842115250 that motivated this fix).
//
// The test walks the three distinct states the handler must traverse:
//
//  1. Racing delete: delete the Keystone CR before adoption. The handler
//     must record SecretsReady=False/Reason=WaitingForESOAdoption and
//     leave both PushSecrets live with zero DeletionTimestamp.
//  2. Adoption: install both ESO finalizers on each PushSecret, matching
//     what ESO would do after draining its workqueue. The handler must
//     now fire Delete (Pass-1), after which both PushSecrets are held
//     Terminating by esoCleanupFinalizer and the condition flips to
//     Reason=OpenBaoFinalizerBlocked.
//  3. Cleanup: clear the ESO finalizers. Both PushSecrets garbage-collect,
//     the next reconcile observes them NotFound, finalizeOpenBaoSecrets
//     returns done=true, and the API server reclaims the Keystone CR.
func TestIntegrationKeystone_DeleteRacingESOAdoption(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-openbao-race-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	mdbCluster := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "mariadb", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, mdbCluster)).To(Succeed(), "create MariaDB cluster CR")
	g.Expect(simulators.SimulateMariaDBReady(ctx, c, client.ObjectKey{Namespace: ns.Name, Name: "mariadb"}, 1)).
		To(Succeed(), "simulate MariaDB cluster ready")

	ks := integrationManagedKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())
	ksKey := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}

	fernetKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-fernet-keys-backup", ks.Name)}
	credKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-credential-keys-backup", ks.Name)}

	// Both backup PushSecrets must be created first so deletion doesn't race
	// a missing object. We intentionally do NOT install any ESO finalizer
	// here — this is the point of the test: the operator must tolerate the
	// window between PushSecret creation and ESO's first reconcile
	// (CC-0091, REQ-001).
	for _, key := range []client.ObjectKey{fernetKey, credKey} {
		g.Eventually(func() error {
			return c.Get(ctx, key, &esov1alpha1.PushSecret{})
		}, eventuallyTimeout, pollInterval).Should(Succeed(),
			"PushSecret %s should be provisioned", key)
	}

	// Wait for the OpenBao finalizer so the subsequent Delete goes through
	// reconcileDeleteOpenBao rather than a straight cascade (CC-0091,
	// REQ-001, REQ-006).
	g.Eventually(func() bool {
		ksState := &keystonev1alpha1.Keystone{}
		if err := c.Get(ctx, ksKey, ksState); err != nil {
			return false
		}
		return controllerutil.ContainsFinalizer(ksState, keystoneOpenBaoFinalizer)
	}, eventuallyTimeout, pollInterval).Should(BeTrue(),
		"Keystone CR should carry the OpenBao finalizer before delete")

	g.Expect(c.Delete(ctx, ks)).To(Succeed(), "delete Keystone CR before ESO adopts")

	// Stage 1 — racing delete: finalizeOpenBaoSecrets must record
	// WaitingForESOAdoption and MUST NOT issue Delete on either PushSecret.
	// The message must name a concrete unadopted PushSecret so an SRE
	// reading `kubectl describe keystone` can see which resource is
	// blocking the handler (CC-0091, REQ-001, REQ-002, REQ-003).
	g.Eventually(func(ig Gomega) {
		ksState := &keystonev1alpha1.Keystone{}
		ig.Expect(c.Get(ctx, ksKey, ksState)).To(Succeed())
		cond := meta.FindStatusCondition(ksState.Status.Conditions, "SecretsReady")
		ig.Expect(cond).NotTo(BeNil(),
			"SecretsReady condition must be present while adoption is pending")
		ig.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		ig.Expect(cond.Reason).To(Equal("WaitingForESOAdoption"),
			"handler must gate Delete on ESO adoption to avoid orphaning the kv-v2 path")
		ig.Expect(cond.Message).To(SatisfyAny(
			ContainSubstring(fernetKey.Name),
			ContainSubstring(credKey.Name),
		), "adoption-wait message should name the unadopted PushSecret")
	}, eventuallyLongTimeout, pollInterval).Should(Succeed())

	// Both PushSecrets must still be live with zero DeletionTimestamp —
	// this is the core safety property of Pass-0. A single Delete here
	// would be the production bug the fix is guarding against
	// (CC-0091, REQ-001).
	for _, key := range []client.ObjectKey{fernetKey, credKey} {
		ps := &esov1alpha1.PushSecret{}
		g.Expect(c.Get(ctx, key, ps)).To(Succeed(),
			"PushSecret %s must still exist during adoption wait", key)
		g.Expect(ps.GetDeletionTimestamp().IsZero()).To(BeTrue(),
			"PushSecret %s must NOT be Terminating during adoption wait — "+
				"a racing Delete here orphans the kv-v2 path", key)
	}

	// The Keystone CR must still be alive — the openbao finalizer is
	// holding it, and that finalizer must not be released while Pass-0
	// is blocking (CC-0091, REQ-001, REQ-006).
	ksState := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, ksKey, ksState)).To(Succeed())
	g.Expect(ksState.GetDeletionTimestamp().IsZero()).To(BeFalse(),
		"Keystone CR should be Terminating while adoption wait holds the finalizer")
	g.Expect(controllerutil.ContainsFinalizer(ksState, keystoneOpenBaoFinalizer)).To(BeTrue(),
		"OpenBao finalizer must still be present while Pass-0 is blocking")
	// Capture the Keystone UID once — the cross-stage event-count assertion
	// at the end of this test runs after the CR has been garbage-collected,
	// so it cannot Get the CR to read the UID at that point.
	ksUID := ksState.UID

	// Across the adoption-wait window (stage 1) the reconciler must emit at
	// most one FinalizingOpenBaoSecrets event — preserving the exactly-once
	// contract established by CC-0079. The preceding Eventually already spans
	// well over one RequeueSecretPolling tick, so any regression that fires
	// the event per requeue would surface as stage1Finalizing>1 here. The
	// exactly-once gate (hasLiveOpenBaoBackupPushSecrets skipping unadopted
	// PushSecrets) means the expected count during stage 1 is 0; ≤1 is the
	// loosest assertion that still catches the per-requeue regression
	// (CC-0091, REQ-007).
	stage1Events := &corev1.EventList{}
	g.Expect(c.List(ctx, stage1Events, client.InNamespace(ns.Name))).To(Succeed())
	stage1Finalizing := 0
	for _, e := range stage1Events.Items {
		if e.InvolvedObject.UID == ksState.UID && e.Reason == "FinalizingOpenBaoSecrets" {
			stage1Finalizing++
		}
	}
	g.Expect(stage1Finalizing).To(BeNumerically("<=", 1),
		"FinalizingOpenBaoSecrets must fire at most once across the adoption-wait "+
			"window; a per-requeue emit regresses the exactly-once contract (CC-0079, CC-0091)")

	// Stage 2 — adoption: install both ESO finalizers. addESOFinalizerToPushSecret
	// installs esoPushSecretFinalizer (the Pass-0 adoption signal) and
	// esoCleanupFinalizer (the Pass-2 cleanup finalizer), matching the
	// shape ESO leaves on a DeletionPolicy=Delete PushSecret after its
	// first reconcile (CC-0091, REQ-001).
	addESOFinalizerToPushSecret(t, ctx, c, fernetKey)
	addESOFinalizerToPushSecret(t, ctx, c, credKey)

	// The handler must now proceed past Pass-0, fire Delete on both
	// PushSecrets (Pass-1), and — because esoCleanupFinalizer holds them
	// Terminating — surface OpenBaoFinalizerBlocked from Pass-2
	// (CC-0091, REQ-001, REQ-002).
	g.Eventually(func(ig Gomega) {
		ksState := &keystonev1alpha1.Keystone{}
		ig.Expect(c.Get(ctx, ksKey, ksState)).To(Succeed())
		cond := meta.FindStatusCondition(ksState.Status.Conditions, "SecretsReady")
		ig.Expect(cond).NotTo(BeNil())
		ig.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		ig.Expect(cond.Reason).To(Equal("OpenBaoFinalizerBlocked"),
			"handler must advance from Pass-0 to Pass-2 once both PushSecrets are adopted")
	}, eventuallyLongTimeout, pollInterval).Should(Succeed())

	for _, key := range []client.ObjectKey{fernetKey, credKey} {
		ps := &esov1alpha1.PushSecret{}
		g.Expect(c.Get(ctx, key, ps)).To(Succeed(),
			"PushSecret %s should still exist while ESO finalizers hold it", key)
		g.Expect(ps.GetDeletionTimestamp().IsZero()).To(BeFalse(),
			"PushSecret %s should be Terminating after Pass-1 Delete", key)
	}

	// Stage 3 — ESO finishes its purge: clear both finalizers. The API
	// server garbage-collects the PushSecrets, Pass-2 observes NotFound,
	// the handler returns done=true, and the API server reclaims the
	// Keystone CR (CC-0091, REQ-001, REQ-002, REQ-004).
	clearESOFinalizerFromPushSecret(t, ctx, c, fernetKey)
	clearESOFinalizerFromPushSecret(t, ctx, c, credKey)

	g.Eventually(func(ig Gomega) {
		ig.Expect(apierrors.IsNotFound(c.Get(ctx, fernetKey, &esov1alpha1.PushSecret{}))).
			To(BeTrue(), "fernet-keys-backup PushSecret should be garbage-collected")
		ig.Expect(apierrors.IsNotFound(c.Get(ctx, credKey, &esov1alpha1.PushSecret{}))).
			To(BeTrue(), "credential-keys-backup PushSecret should be garbage-collected")
	}, eventuallyLongTimeout, pollInterval).Should(Succeed())

	g.Eventually(func() bool {
		return apierrors.IsNotFound(c.Get(ctx, ksKey, &keystonev1alpha1.Keystone{}))
	}, eventuallyLongTimeout, pollInterval).Should(BeTrue(),
		"Keystone CR should be removed from etcd once both PushSecrets GC")

	// Cross-stage bound: across stages 1+2+3, FinalizingOpenBaoSecrets must
	// fire at most twice. The expected count is exactly 1 — emitted once
	// when Pass-0 clears in stage 2 and Pass-1 fires Delete; subsequent
	// requeues see the PushSecrets in Terminating and hasLiveOpenBaoBackup-
	// PushSecrets returns false. The ≤2 bound also catches a staggered-
	// adoption regression where each partial adoption could otherwise
	// trigger a fresh Pass-1 emit (CC-0079, CC-0091, REQ-007). Events are
	// retained namespace-wide and outlive the involved Keystone CR, so the
	// list survives the final GC above; ksUID was captured in stage 1.
	allEvents := &corev1.EventList{}
	g.Expect(c.List(ctx, allEvents, client.InNamespace(ns.Name))).To(Succeed())
	totalFinalizing := 0
	for _, e := range allEvents.Items {
		if e.InvolvedObject.UID == ksUID && e.Reason == "FinalizingOpenBaoSecrets" {
			totalFinalizing++
		}
	}
	g.Expect(totalFinalizing).To(BeNumerically("<=", 2),
		"FinalizingOpenBaoSecrets must fire at most twice across the full "+
			"termination (stages 1+2+3); a per-requeue or per-partial-adoption "+
			"emit regresses the exactly-once contract (CC-0079, CC-0091)")
}

// --- CC-0087: field-indexer-driven Secret watch ---

// TestIntegration_SecretEventTriggersReconcileViaIndexer verifies that the
// field indexer registered under KeystoneSecretNameIndexKey wires the
// Secret watch to the Keystone CR end-to-end: after the CR is created and
// the referenced Secrets exist, the reconciler observes the current
// generation via SecretsReady.ObservedGeneration, which is only possible
// when the indexer-backed mapper enqueued at least one reconcile request
// (CC-0087, REQ-008).
func TestIntegration_SecretEventTriggersReconcileViaIndexer(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cc0087-indexer-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	// createPrerequisites creates both the keystone-db and keystone-admin
	// ExternalSecrets + Secrets and simulates ESO sync so SecretsReady can
	// flip to True.
	createPrerequisites(t, ctx, c, ns.Name)

	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	key := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}

	g.Eventually(func(ig Gomega) {
		got := &keystonev1alpha1.Keystone{}
		ig.Expect(c.Get(ctx, key, got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, "SecretsReady")
		ig.Expect(cond).NotTo(BeNil(),
			"SecretsReady condition should be set once the indexer-backed mapper enqueues a reconcile")
		// ObservedGeneration == Generation proves a reconcile has run
		// against the current spec (REQ-007 / REQ-008).
		ig.Expect(cond.ObservedGeneration).To(Equal(got.Generation),
			"SecretsReady.ObservedGeneration must match the current Keystone generation")
	}, eventuallyTimeout, pollInterval).Should(Succeed(),
		"indexer-driven Secret watch must cause the controller to reconcile the Keystone CR (CC-0087, REQ-008)")
}

// TestIntegration_UnrelatedSecretDoesNotTriggerReconcile verifies the
// contract of the field indexer: a Secret event whose name is NOT present
// in KeystoneSecretNameIndexKey (i.e. not referenced by spec.database.secretRef.name
// or spec.bootstrap.adminPasswordSecretRef.name) MUST NOT drive a
// mapper-enqueued reconcile of the Keystone CR. Without the indexer the
// mapper would List every Keystone in the namespace and return a request
// for each, so this is the negative counterpart of
// TestIntegration_SecretEventTriggersReconcileViaIndexer (CC-0087, REQ-008).
func TestIntegration_UnrelatedSecretDoesNotTriggerReconcile(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cc0087-unrelated-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	// Drive the CR all the way to Ready=True so subsequent reconciles that
	// *are* triggered still produce no spec/status churn (steady state).
	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	key := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}

	// Capture the steady-state ResourceVersion.
	stable := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, key, stable)).To(Succeed())
	stableRV := stable.ResourceVersion
	g.Expect(stableRV).NotTo(BeEmpty())

	// Create an unrelated Secret — a name NOT referenced by the Keystone CR
	// (keystone-db and keystone-admin are the only referenced names). This
	// Secret shares the namespace but is invisible to the indexer, so the
	// mapper must return no requests for this event.
	unrelated := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unrelated-secret",
			Namespace: ns.Name,
		},
		Data: map[string][]byte{"key": []byte("value")},
	}
	g.Expect(c.Create(ctx, unrelated)).To(Succeed())

	// DECISION: The controller has periodic requeues that can advance the
	// Keystone's status (and hence ResourceVersion) independently of the
	// Secret event. To isolate the Secret-event contract, we bound the
	// observation window to ~1s — shorter than the controller's periodic
	// requeue cadence — and assert via Consistently that the Keystone's
	// ResourceVersion does not advance because of the unrelated Secret
	// event flowing through the mapper. If this proves flaky in CI, the
	// alternative discussed in the task brief is a sample-at-t0 vs.
	// sample-at-t1 check with ~500ms between samples.
	g.Consistently(func(ig Gomega) {
		got := &keystonev1alpha1.Keystone{}
		ig.Expect(c.Get(ctx, key, got)).To(Succeed())
		ig.Expect(got.ResourceVersion).To(Equal(stableRV),
			"Keystone ResourceVersion must not advance because of an unrelated Secret event (CC-0087, REQ-008)")
	}, 1*time.Second, pollInterval/2).Should(Succeed(),
		"unrelated Secret events must not drive mapper-enqueued reconciles through the indexer (CC-0087, REQ-008)")
}

// TestIntegration_IndexerRegistrationFailsManagerStartCleanly verifies that
// registerSecretNameIndex returns an error when the same key is registered
// twice against a single FieldIndexer, and that the error message mentions
// the index key so the failure is actionable in manager-startup logs
// (CC-0087, REQ-001).
//
// Controller-runtime's FieldIndexer keys registrations by (GVK, field); a
// second registration for the same key returns an "indexer conflict" error.
// The test does NOT start the manager — IndexField is safely callable on
// the FieldIndexer before Start.
func TestIntegration_IndexerRegistrationFailsManagerStartCleanly(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	// DECISION: we use the minimal testutil helper (SetupMinimalEnvTest) to
	// avoid paying for webhook wiring and controller registration. The
	// helper returns an unstarted manager whose scheme knows the Keystone
	// type, which is all IndexField needs. We intentionally do NOT call
	// mgr.Start(ctx); IndexField is valid pre-Start.
	mgr, ctx, _ := testutil.SetupMinimalEnvTest(t, keystonev1alpha1.AddToScheme)

	indexer := mgr.GetFieldIndexer()

	// First registration must succeed.
	g.Expect(registerSecretNameIndex(ctx, indexer)).To(Succeed(),
		"first registration of KeystoneSecretNameIndexKey must succeed")

	// Second registration with the same key/extractor must fail. Controller-runtime
	// returns an "indexer conflict" error keyed by (GVK, field).
	err := registerSecretNameIndex(ctx, indexer)
	g.Expect(err).To(HaveOccurred(),
		"duplicate registration of KeystoneSecretNameIndexKey must return an error")
	g.Expect(err.Error()).To(ContainSubstring(KeystoneSecretNameIndexKey),
		"error message must mention the index key so manager-startup logs identify the conflict (CC-0087, REQ-001)")
}

// --- CC-0084: graceful pod termination / rolling update ---

// TestIntegration_TerminationGracePeriodAppliedToDeployment verifies that user-
// supplied spec.terminationGracePeriodSeconds and spec.preStopSleepSeconds are
// propagated verbatim to the Deployment pod template and keystone
// container's preStop hook (CC-0084, REQ-001).
func TestIntegration_TerminationGracePeriodAppliedToDeployment(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-tgps-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	ks.Spec.TerminationGracePeriodSeconds = ptr.To(int64(60))
	ks.Spec.PreStopSleepSeconds = ptr.To(int64(10))
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	deploy := &appsv1.Deployment{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone"}, deploy)).
		To(Succeed(), "Deployment test-keystone should exist")

	g.Expect(deploy.Spec.Template.Spec.TerminationGracePeriodSeconds).NotTo(BeNil(),
		"terminationGracePeriodSeconds must be set")
	g.Expect(*deploy.Spec.Template.Spec.TerminationGracePeriodSeconds).To(Equal(int64(60)))

	container := findContainerByName(deploy.Spec.Template.Spec.Containers, "keystone")
	g.Expect(container).NotTo(BeNil(), "keystone container must exist")
	g.Expect(container.Lifecycle).NotTo(BeNil(), "Lifecycle must be set")
	g.Expect(container.Lifecycle.PreStop).NotTo(BeNil(), "PreStop hook must be set")
	g.Expect(container.Lifecycle.PreStop.Exec).NotTo(BeNil(), "PreStop must use exec")
	g.Expect(container.Lifecycle.PreStop.Exec.Command).To(Equal([]string{"/bin/sh", "-c", "sleep 10"}))
}

// TestIntegration_DefaultStrategyAppliedToDeployment verifies that when
// spec.strategy is nil the reconciler applies the default RollingUpdate
// strategy with MaxUnavailable=0 and MaxSurge=1 (CC-0084, REQ-005).
func TestIntegration_DefaultStrategyAppliedToDeployment(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-strategy-default-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(ks.Spec.Strategy).To(BeNil(), "strategy must be left nil for default test")
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	deploy := &appsv1.Deployment{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone"}, deploy)).
		To(Succeed(), "Deployment test-keystone should exist")

	g.Expect(deploy.Spec.Strategy.Type).To(Equal(appsv1.RollingUpdateDeploymentStrategyType))
	g.Expect(deploy.Spec.Strategy.RollingUpdate).NotTo(BeNil(), "RollingUpdate must be set")
	g.Expect(deploy.Spec.Strategy.RollingUpdate.MaxUnavailable).NotTo(BeNil())
	g.Expect(*deploy.Spec.Strategy.RollingUpdate.MaxUnavailable).To(Equal(intstr.FromInt(0)))
	g.Expect(deploy.Spec.Strategy.RollingUpdate.MaxSurge).NotTo(BeNil())
	g.Expect(*deploy.Spec.Strategy.RollingUpdate.MaxSurge).To(Equal(intstr.FromInt(1)))
}

// TestIntegration_StrategyOverrideAppliedToDeployment verifies that a user-
// supplied spec.strategy is propagated verbatim to the Deployment
// (CC-0084, REQ-006).
func TestIntegration_StrategyOverrideAppliedToDeployment(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-strategy-override-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	ks.Spec.Strategy = &appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxUnavailable: ptr.To(intstr.FromString("25%")),
			MaxSurge:       ptr.To(intstr.FromString("25%")),
		},
	}
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	deploy := &appsv1.Deployment{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone"}, deploy)).
		To(Succeed(), "Deployment test-keystone should exist")

	g.Expect(deploy.Spec.Strategy.Type).To(Equal(appsv1.RollingUpdateDeploymentStrategyType))
	g.Expect(deploy.Spec.Strategy.RollingUpdate).NotTo(BeNil(), "RollingUpdate must be set")
	g.Expect(deploy.Spec.Strategy.RollingUpdate.MaxUnavailable).NotTo(BeNil())
	g.Expect(*deploy.Spec.Strategy.RollingUpdate.MaxUnavailable).To(Equal(intstr.FromString("25%")))
	g.Expect(deploy.Spec.Strategy.RollingUpdate.MaxSurge).NotTo(BeNil())
	g.Expect(*deploy.Spec.Strategy.RollingUpdate.MaxSurge).To(Equal(intstr.FromString("25%")))
}

// TestIntegrationKeystone_PushSecretRemoteKeyIsPerCR verifies that two
// Keystone CRs in the same namespace produce two backup PushSecrets with
// distinct, per-CR-scoped RemoteKey values for both fernet-keys
// (CC-0093, REQ-001) and credential-keys (CC-0093, REQ-002) materials.
//
// Regression guard: before CC-0093, both PushSecrets wrote to the shared
// path openstack/keystone/<material>, causing concurrent two-CR
// deployments in the same namespace to race on the remote kv-v2 store.
// The table-driven form keeps per-CR assertions for every key material in
// one place so adding a new material (or changing the path layout)
// requires a single edit (sourcery-review-1).
func TestIntegrationKeystone_PushSecretRemoteKeyIsPerCR(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	cases := []struct {
		material    string // kv-v2 leaf segment and PushSecret name suffix
		requirement string // spec requirement this row covers (REQ-001 / REQ-002)
	}{
		{material: "fernet-keys", requirement: "REQ-001"},
		{material: "credential-keys", requirement: "REQ-002"},
	}

	for _, tc := range cases {
		t.Run(tc.material, func(t *testing.T) {
			g := NewGomegaWithT(t)

			c, ctx, _ := setupEnvTestWithController(t)

			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-per-cr-" + tc.material + "-",
			}}
			g.Expect(c.Create(ctx, ns)).To(Succeed())

			createPrerequisites(t, ctx, c, ns.Name)

			// kA and kB intentionally share the namespace-scoped keystone-db and
			// keystone-admin Secret refs created by createPrerequisites: this test
			// asserts PushSecret RemoteKey distinctness (CC-0093, REQ-001 for
			// fernet / REQ-002 for credential), not isolation of the read-only
			// credential Secrets.
			kA := integrationBrownfieldKeystone("keystone-a", ns.Name)
			kB := integrationBrownfieldKeystone("keystone-b", ns.Name)
			g.Expect(c.Create(ctx, kA)).To(Succeed())
			g.Expect(c.Create(ctx, kB)).To(Succeed())

			driveFullReconciliation(t, ctx, c, kA.Name, ns.Name)
			driveFullReconciliation(t, ctx, c, kB.Name, ns.Name)

			psA := &esov1alpha1.PushSecret{}
			psB := &esov1alpha1.PushSecret{}
			nameA := kA.Name + "-" + tc.material + "-backup"
			nameB := kB.Name + "-" + tc.material + "-backup"
			g.Eventually(func() error {
				return c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: nameA}, psA)
			}, eventuallyTimeout).Should(Succeed())
			g.Eventually(func() error {
				return c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: nameB}, psB)
			}, eventuallyTimeout).Should(Succeed())

			g.Expect(psA.Spec.Data).ToNot(BeEmpty())
			g.Expect(psB.Spec.Data).ToNot(BeEmpty())

			keyA := psA.Spec.Data[0].Match.RemoteRef.RemoteKey
			keyB := psB.Spec.Data[0].Match.RemoteRef.RemoteKey

			wantA := "openstack/keystone/" + kA.Name + "/" + tc.material
			wantB := "openstack/keystone/" + kB.Name + "/" + tc.material

			g.Expect(keyA).To(Equal(wantA),
				kA.Name+" RemoteKey must embed CR name ("+tc.requirement+")")
			g.Expect(keyB).To(Equal(wantB),
				kB.Name+" RemoteKey must embed CR name ("+tc.requirement+")")
			g.Expect(keyA).ToNot(Equal(keyB),
				"RemoteKeys must be distinct per-CR to prevent concurrent write collision")
			g.Expect(keyA).To(ContainSubstring(kA.Name))
			g.Expect(keyB).To(ContainSubstring(kB.Name))
		})
	}
}

// --- CC-0095: Sub-resource rename to bare CR name (REQ-003, REQ-004) ---

// TestIntegration_ReconcileProducesRenamedSubResources end-to-end-validates
// that after a full reconciliation the operator emits every sub-resource at
// `<cr-name>` (no `-api` suffix). Symmetric with the per-builder unit tests
// (`TestBuildKeystoneDeployment_NameMatchesCR`, etc.) but exercises the live
// reconciler against envtest so any future regression in name composition is
// caught at the integration layer (CC-0095, REQ-003, REQ-004).
func TestIntegration_ReconcileProducesRenamedSubResources(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cc0095-rename-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())
	createPrerequisites(t, ctx, c, ns.Name)

	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	// All operator-managed sub-resources must exist under the bare CR name.
	bareKey := client.ObjectKey{Namespace: ns.Name, Name: ks.Name}

	g.Expect(c.Get(ctx, bareKey, &appsv1.Deployment{})).
		To(Succeed(), "Deployment must exist at <cr-name> (CC-0095, REQ-003)")
	g.Expect(c.Get(ctx, bareKey, &corev1.Service{})).
		To(Succeed(), "Service must exist at <cr-name> (CC-0095, REQ-004)")
	g.Expect(c.Get(ctx, bareKey, &policyv1.PodDisruptionBudget{})).
		To(Succeed(), "PodDisruptionBudget must exist at <cr-name> (CC-0095, REQ-004)")
}

// TestIntegration_FreshReconcileEmitsNoLegacyApiSuffixedResources pins the
// post-rename steady state: starting from an empty namespace, a fresh
// reconcile must not emit any operator-managed sub-resource at the legacy
// `<cr-name>-api` name. // CC-0095 legacy: pre-rename name referenced for traceability.
// A regression here would either re-introduce the `-api` suffix in a builder,
// or leave dual-writes after a partial revert — both visible to live clients
// (CC-0095, REQ-004).
//
// This test does NOT exercise upgrade-from-pre-CC-0095 orphan cleanup: it
// never pre-seeds legacy `<cr-name>-api` Deployment/Service/PDB, // CC-0095 legacy: pre-rename name referenced for traceability.
// so it cannot detect orphan persistence on a real upgrade path. See
// docs/reference/keystone-upgrade-flow.md for the manual cleanup runbook
// that currently covers that scenario (CC-0095).
func TestIntegration_FreshReconcileEmitsNoLegacyApiSuffixedResources(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cc0095-noorphans-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())
	createPrerequisites(t, ctx, c, ns.Name)

	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	legacyKey := client.ObjectKey{Namespace: ns.Name, Name: ks.Name + "-api"}

	err := c.Get(ctx, legacyKey, &appsv1.Deployment{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"no legacy <cr-name>-api Deployment must exist after reconcile (CC-0095, REQ-004)") // CC-0095 legacy: assertion pins absence of the pre-rename name.

	err = c.Get(ctx, legacyKey, &corev1.Service{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"no legacy <cr-name>-api Service must exist after reconcile (CC-0095, REQ-004)") // CC-0095 legacy: assertion pins absence of the pre-rename name.

	err = c.Get(ctx, legacyKey, &policyv1.PodDisruptionBudget{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"no legacy <cr-name>-api PodDisruptionBudget must exist after reconcile (CC-0095, REQ-004)") // CC-0095 legacy: assertion pins absence of the pre-rename name.
}

// --- Task 7.1: Metrics endpoint exposes Keystone operator collectors (CC-0089, REQ-008) ---

// TestMetricsEndpointServesKeystoneOperatorCollectors proves the Keystone
// operator's Prometheus collectors are reachable in Prometheus text format on
// the controller-runtime metrics registry that the operator's metrics server
// would serve in production (CC-0089, REQ-008).
func TestMetricsEndpointServesKeystoneOperatorCollectors(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-cc0089-metrics-endpoint-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	createPrerequisites(t, ctx, c, ns.Name)

	ks := integrationBrownfieldKeystone("test-keystone", ns.Name)
	g.Expect(c.Create(ctx, ks)).To(Succeed())

	// Drive at least one full reconcile so that duration histogram samples
	// (and other collectors) are populated and observable by a scrape.
	driveFullReconciliation(t, ctx, c, ks.Name, ns.Name)

	// DECISION: the envtest manager runs with metrics BindAddress="0", which
	// disables the manager's metrics HTTP server. We therefore cannot scrape
	// mgr.GetMetricsServer() directly. Instead we serve the same registry
	// the production metrics server uses — controller-runtime's package-level
	// ctrlmetrics.Registry — through promhttp.HandlerFor via httptest. This
	// is contract-equivalent because the production metrics server wraps the
	// exact same Registry, so any series visible here is visible at the
	// real /metrics endpoint (CC-0089, REQ-008).
	srv := httptest.NewServer(promhttp.HandlerFor(ctrlmetrics.Registry, promhttp.HandlerOpts{}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/metrics", nil)
	g.Expect(err).NotTo(HaveOccurred(), "build /metrics request")

	resp, err := http.DefaultClient.Do(req)
	g.Expect(err).NotTo(HaveOccurred(), "GET /metrics")
	defer func() { _ = resp.Body.Close() }()

	g.Expect(resp.StatusCode).To(Equal(http.StatusOK), "/metrics should return 200")

	body, err := io.ReadAll(resp.Body)
	g.Expect(err).NotTo(HaveOccurred(), "read /metrics body")

	g.Expect(string(body)).To(ContainSubstring(
		"# TYPE keystone_operator_reconcile_duration_seconds histogram"),
		"Prometheus text exposition must declare the reconcile duration histogram")
}

// --- Task 7.2: Reconcile errors counter increments on induced failure (CC-0089, REQ-002, REQ-008) ---

// The unit test that addresses Task 7.2 lives next to the other testReconciler
// pattern tests in keystone_controller_test.go, where it can use a fake client
// with interceptor.Funcs to inject a deterministic error from
// reconcileDBConnectionSecret. The interceptor approach is independent of
// whether the controller materializes the derived Secret via Update or
// Server-Side Apply, so it survives a future SSA migration without silently
// passing for the wrong reason (CC-0089 I-001 review feedback).
//
// See: TestReconcileErrorsTotalIncrementsOnInducedFailure in
// keystone_controller_test.go.
