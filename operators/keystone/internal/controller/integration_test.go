// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Package controller contains integration tests for the Keystone reconciler (CC-0014, F002).
package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
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
	"sigs.k8s.io/controller-runtime/pkg/handler"

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
			return (&keystonev1alpha1.KeystoneWebhook{}).SetupWebhookWithManager(mgr)
		},
		func(mgr ctrl.Manager) error {
			r := &KeystoneReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("keystone-controller"),
			}
			return ctrl.NewControllerManagedBy(mgr).
				For(&keystonev1alpha1.Keystone{}).
				Owns(&appsv1.Deployment{}).
				Owns(&corev1.Service{}).
				Owns(&corev1.ConfigMap{}).
				Owns(&batchv1.Job{}).
				Owns(&policyv1.PodDisruptionBudget{}).
				Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
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

	// Wait for DatabaseReady=True.
	waitForCondition(t, ctx, c, key, "DatabaseReady", metav1.ConditionTrue, eventuallyTimeout)

	// Wait for the Deployment to appear and simulate its readiness.
	deployKey := client.ObjectKey{Namespace: ns, Name: fmt.Sprintf("%s-api", ksName)}
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
	expectedEndpoint := fmt.Sprintf("http://%s-api.%s.svc.cluster.local:5000/v3", ks.Name, ns.Name)
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

	// Phase 3: Simulate db-sync completion → DatabaseReady=True.
	dbSyncKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-db-sync", ks.Name)}
	g.Eventually(func() error {
		return c.Get(ctx, dbSyncKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed())
	g.Expect(simulators.SimulateJobComplete(ctx, c, dbSyncKey)).To(Succeed())

	waitForCondition(t, ctx, c, key, "DatabaseReady", metav1.ConditionTrue, eventuallyTimeout)

	// DeploymentReady should appear as False (WaitingForDeployment).
	deployCond := waitForCondition(t, ctx, c, key, "DeploymentReady", metav1.ConditionFalse, eventuallyTimeout)
	g.Expect(deployCond.Reason).To(Equal("WaitingForDeployment"), "DeploymentReady reason should be WaitingForDeployment")

	// Phase 4: Simulate Deployment ready → DeploymentReady=True.
	deployKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-api", ks.Name)}
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
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-api"}, &appsv1.Deployment{})).
		To(Succeed(), "Deployment test-keystone-api should exist")

	// Service.
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-api"}, &corev1.Service{})).
		To(Succeed(), "Service test-keystone-api should exist")

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
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-api"}, &policyv1.PodDisruptionBudget{})).
		To(Succeed(), "PodDisruptionBudget test-keystone-api should exist")
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

	expectedEndpoint := fmt.Sprintf("http://%s-api.%s.svc.cluster.local:5000/v3", ks.Name, ns.Name)
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

	// Wait for DatabaseReady=True.
	waitForCondition(t, ctx, c, key, "DatabaseReady", metav1.ConditionTrue, eventuallyTimeout)

	// Wait for Deployment and simulate readiness.
	deployKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-api", ks.Name)}
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
	expectedEndpoint := fmt.Sprintf("http://%s-api.%s.svc.cluster.local:5000/v3", ks.Name, ns.Name)
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
	g.Expect(mainContainer.Command).To(Equal([]string{"sh", "-c", fernetRotateScript}))

	// Verify main container env vars.
	envMap := map[string]corev1.EnvVar{}
	for _, env := range mainContainer.Env {
		envMap[env.Name] = env
	}
	g.Expect(envMap).To(HaveKey("SECRET_NAME"))
	g.Expect(envMap["SECRET_NAME"].Value).To(Equal(fmt.Sprintf("%s-fernet-keys", ks.Name)))

	g.Expect(envMap).To(HaveKey("SECRET_NAMESPACE"))
	g.Expect(envMap["SECRET_NAMESPACE"].ValueFrom).NotTo(BeNil(), "SECRET_NAMESPACE should use ValueFrom")
	g.Expect(envMap["SECRET_NAMESPACE"].ValueFrom.FieldRef).NotTo(BeNil(), "SECRET_NAMESPACE should use fieldRef")
	g.Expect(envMap["SECRET_NAMESPACE"].ValueFrom.FieldRef.FieldPath).To(Equal("metadata.namespace"))

	g.Expect(envMap).To(HaveKey("OS_fernet_tokens__max_active_keys"))
	g.Expect(envMap["OS_fernet_tokens__max_active_keys"].Value).To(Equal("3"),
		"OS_fernet_tokens__max_active_keys should match spec.fernet.maxActiveKeys")

	// Verify main container volume mounts.
	g.Expect(mainContainer.VolumeMounts).To(HaveLen(3))
	g.Expect(mainContainer.VolumeMounts[0].Name).To(Equal("fernet-keys"))
	g.Expect(mainContainer.VolumeMounts[0].MountPath).To(Equal("/etc/keystone/fernet-keys"))
	g.Expect(mainContainer.VolumeMounts[1].Name).To(Equal("credential-keys"))
	g.Expect(mainContainer.VolumeMounts[1].MountPath).To(Equal("/etc/keystone/credential-keys"))
	g.Expect(mainContainer.VolumeMounts[1].ReadOnly).To(BeTrue())
	g.Expect(mainContainer.VolumeMounts[2].Name).To(Equal("config"))
	g.Expect(mainContainer.VolumeMounts[2].MountPath).To(Equal("/etc/keystone/keystone.conf.d/"))
	g.Expect(mainContainer.VolumeMounts[2].ReadOnly).To(BeTrue())

	// Verify volumes: fernet-keys-src (Secret), fernet-keys (emptyDir), credential-keys (Secret), and config (ConfigMap).
	volMap := map[string]corev1.Volume{}
	for _, v := range podSpec.Volumes {
		volMap[v.Name] = v
	}
	g.Expect(volMap).To(HaveLen(4))

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

	waitForCondition(t, ctx, c, key, "DatabaseReady", metav1.ConditionTrue, eventuallyTimeout)

	deployKey := client.ObjectKey{Namespace: ns, Name: fmt.Sprintf("%s-api", ksName)}
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
	expectedServiceURL := fmt.Sprintf("http://%s-api.%s.svc.cluster.local:5000/v3", ks.Name, ns.Name)
	g.Expect(container.Command[3]).To(ContainSubstring(expectedServiceURL))
	g.Expect(container.Command[3]).To(ContainSubstring("--bootstrap-region-id " + ks.Spec.Bootstrap.Region))
	g.Expect(container.Args).To(BeNil())

	// Verify BOOTSTRAP_PASSWORD env from SecretKeyRef (REQ-007).
	g.Expect(container.Env).To(HaveLen(1))
	pwEnv := container.Env[0]
	g.Expect(pwEnv.Name).To(Equal("BOOTSTRAP_PASSWORD"))
	g.Expect(pwEnv.ValueFrom).NotTo(BeNil())
	g.Expect(pwEnv.ValueFrom.SecretKeyRef).NotTo(BeNil())
	g.Expect(pwEnv.ValueFrom.SecretKeyRef.Name).To(Equal(ks.Spec.Bootstrap.AdminPasswordSecretRef.Name),
		"BOOTSTRAP_PASSWORD should reference the admin password Secret")
	g.Expect(pwEnv.ValueFrom.SecretKeyRef.Key).To(Equal("password"))

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
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-api"}, pdb)).
		To(Succeed(), "PDB test-keystone-api should exist")

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
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-api"}, deploy)).To(Succeed())
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
	pdbKey := client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-api"}

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
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-api"}, hpa)).
		To(Succeed(), "HPA test-keystone-api should exist")

	// Verify ScaleTargetRef (CC-0038).
	g.Expect(hpa.Spec.ScaleTargetRef.Kind).To(Equal("Deployment"))
	g.Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal("test-keystone-api"))
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

	hpaKey := client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-api"}
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

	hpaKey := client.ObjectKey{Namespace: ns.Name, Name: "test-keystone-api"}
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
	deployKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-api", ks.Name)}
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
		fmt.Sprintf("http://test-keystone-api.%s.svc.cluster.local:5000/v3", ns.Name)),
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
