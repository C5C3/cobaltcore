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

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esov1beta1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1beta1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
	// depends on the controller's 30s RequeueAfter delay to discover readiness
	// changes on unwatched MariaDB types.
	eventuallyLongTimeout = 60 * time.Second
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

// createPrerequisites creates the ExternalSecret and Secret resources that the
// Keystone reconciler expects to find. It creates the DB credentials ExternalSecret
// and Secret (username+password), the admin credentials ExternalSecret and Secret
// (password), and calls SimulateExternalSecretSync for both (CC-0014).
func createPrerequisites(t testing.TB, ctx context.Context, c client.Client, ns string) {
	t.Helper()
	g := NewGomegaWithT(t)

	// Create DB credentials ExternalSecret and Secret.
	dbES := &esov1beta1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-db", Namespace: ns},
		Spec: esov1beta1.ExternalSecretSpec{
			SecretStoreRef: esov1beta1.SecretStoreRef{
				Kind: "ClusterSecretStore",
				Name: "openbao",
			},
			Target: esov1beta1.ExternalSecretTarget{
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
	adminES := &esov1beta1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-admin", Namespace: ns},
		Spec: esov1beta1.ExternalSecretSpec{
			SecretStoreRef: esov1beta1.SecretStoreRef{
				Kind: "ClusterSecretStore",
				Name: "openbao",
			},
			Target: esov1beta1.ExternalSecretTarget{
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

	// Verify all 6 conditions are True.
	for _, condType := range []string{"SecretsReady", "FernetKeysReady", "DatabaseReady", "DeploymentReady", "BootstrapReady", "Ready"} {
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

	// Phase 0: Create ExternalSecrets and Secrets but do NOT simulate sync yet.
	// The reconciler should see SecretsReady=False because ESO hasn't set the
	// Ready condition on the ExternalSecret.
	dbES := &esov1beta1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-db", Namespace: ns.Name},
		Spec: esov1beta1.ExternalSecretSpec{
			SecretStoreRef: esov1beta1.SecretStoreRef{Kind: "ClusterSecretStore", Name: "openbao"},
			Target:         esov1beta1.ExternalSecretTarget{Name: "keystone-db"},
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

	adminES := &esov1beta1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-admin", Namespace: ns.Name},
		Spec: esov1beta1.ExternalSecretSpec{
			SecretStoreRef: esov1beta1.SecretStoreRef{Kind: "ClusterSecretStore", Name: "openbao"},
			Target:         esov1beta1.ExternalSecretTarget{Name: "keystone-admin"},
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

	// The controller does not watch MariaDB types, so it relies on
	// RequeueAfter (30s) to discover readiness changes. The reconciler
	// creates User only after Database is ready, and Grant only after
	// User is ready, so we must simulate each sequentially.

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
	dbSyncKey := client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-db-sync", ks.Name)}
	g.Eventually(func() error {
		return c.Get(ctx, dbSyncKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "db-sync Job should appear")
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

	// All 6 conditions should be True.
	for _, condType := range []string{"SecretsReady", "FernetKeysReady", "DatabaseReady", "DeploymentReady", "BootstrapReady", "Ready"} {
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
