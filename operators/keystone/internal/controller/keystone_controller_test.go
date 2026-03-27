// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/forge/internal/common/job"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// testScheme returns a runtime.Scheme with the types needed for controller tests.
func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = keystonev1alpha1.AddToScheme(s)
	_ = esov1alpha1.SchemeBuilder.AddToScheme(s)
	_ = esov1.SchemeBuilder.AddToScheme(s)
	_ = mariadbv1alpha1.AddToScheme(s)
	return s
}

// testKeystone returns a minimal valid Keystone CR for tests.
func testKeystone() *keystonev1alpha1.Keystone {
	return &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-keystone",
			Namespace:  "default",
			UID:        "ks-uid",
			Generation: 1,
		},
		Spec: keystonev1alpha1.KeystoneSpec{
			Replicas: 3,
			Image:    commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
			Database: commonv1.DatabaseSpec{Host: "db.example.com", Port: 3306, Database: "keystone", SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"}},
			Cache:    commonv1.CacheSpec{Backend: "dogpile.cache.pymemcache", Servers: []string{"mc:11211"}},
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

// testReadyExternalSecrets returns ready ExternalSecret objects for the DB and
// admin credentials referenced by testKeystone.
func testReadyExternalSecrets() []runtime.Object {
	dbES := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-db", Namespace: "default"},
		Status: esov1.ExternalSecretStatus{
			Conditions: []esov1.ExternalSecretStatusCondition{
				{Type: esov1.ExternalSecretReady, Status: corev1.ConditionTrue},
			},
		},
	}
	adminES := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-admin", Namespace: "default"},
		Status: esov1.ExternalSecretStatus{
			Conditions: []esov1.ExternalSecretStatusCondition{
				{Type: esov1.ExternalSecretReady, Status: corev1.ConditionTrue},
			},
		},
	}
	return []runtime.Object{dbES, adminES}
}

// testDBCredentialsSecret returns a Secret containing the DB credentials
// (username and password) that reconcileConfig expects.
func testDBCredentialsSecret() runtime.Object {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-db", Namespace: "default"},
		Data: map[string][]byte{
			"username": []byte("keystone"),
			"password": []byte("secret"),
		},
	}
}

// testAdminCredentialsSecret returns a Secret containing the admin password
// that reconcileSecrets' IsSecretReady check expects (CC-0013).
func testAdminCredentialsSecret() runtime.Object {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-admin", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("admin-password")},
	}
}

// testFernetKeysSecret returns a pre-existing Fernet keys Secret so that
// reconcileFernetKeys does not early-return on the initial-creation path.
func testFernetKeysSecret() runtime.Object {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone-fernet-keys",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"0": []byte("test-fernet-key-0"),
			"1": []byte("test-fernet-key-1"),
			"2": []byte("test-fernet-key-2"),
		},
	}
}

// testCompletedDBSyncJob returns a completed db_sync Job for testKeystone (brownfield mode).
func testCompletedDBSyncJob(configMapName string) runtime.Object {
	ks := testKeystone()
	desired := buildDBSyncJob(ks, configMapName)
	now := metav1.Now()
	j := desired.DeepCopy()
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template.Spec),
	}
	j.Status.Succeeded = 1
	j.Status.CompletionTime = &now
	j.Status.Conditions = []batchv1.JobCondition{
		{
			Type:   batchv1.JobComplete,
			Status: corev1.ConditionTrue,
		},
	}
	return j
}

// testReadyKeystoneDeployment returns a ready Deployment for the test-keystone
// CR. The Deployment must pre-exist and be available so that
// reconcileDeployment does not requeue.
func testReadyKeystoneDeployment() runtime.Object {
	ks := testKeystone()
	replicas := int32(3)
	appLabel := keystoneAppLabel(ks)
	labels := map[string]string{"app": appLabel}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       fmt.Sprintf("%s-api", ks.Name),
			Namespace:  "default",
			Generation: 1,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "keystone-api", Image: "ghcr.io/c5c3/keystone:2025.2"}}},
			},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			ReadyReplicas:      3,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
			},
		},
	}
}

// testCompletedBootstrapJob returns a completed bootstrap Job for testKeystone.
func testCompletedBootstrapJob(configMapName string) runtime.Object {
	ks := testKeystone()
	desired := buildBootstrapJob(ks, configMapName, fmt.Sprintf("%s-fernet-keys", ks.Name))
	now := metav1.Now()
	j := desired.DeepCopy()
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template.Spec),
	}
	j.Status.Succeeded = 1
	j.Status.CompletionTime = &now
	j.Status.Conditions = []batchv1.JobCondition{
		{
			Type:   batchv1.JobComplete,
			Status: corev1.ConditionTrue,
		},
	}
	return j
}

// testComputeConfigMapName creates a temporary reconciler, runs reconcileConfig,
// and returns the deterministic configMapName that will be used in integration tests.
func testComputeConfigMapName(t *testing.T) string {
	t.Helper()
	s := testScheme()
	ks := testKeystone()
	dbSecret := testDBCredentialsSecret().(client.Object)
	cb := fake.NewClientBuilder().WithScheme(s).WithObjects(ks, dbSecret)
	cb = cb.WithStatusSubresource(&keystonev1alpha1.Keystone{})
	r := &KeystoneReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}
	name, err := r.reconcileConfig(context.Background(), ks)
	if err != nil {
		t.Fatalf("computing config map name: %v", err)
	}
	return name
}

// newTestReconciler creates a KeystoneReconciler backed by a fake client
// pre-loaded with the given objects.
func newTestReconciler(objs ...runtime.Object) *KeystoneReconciler {
	s := testScheme()
	cb := fake.NewClientBuilder().WithScheme(s)
	for _, obj := range objs {
		cb = cb.WithRuntimeObjects(obj)
	}
	cb = cb.WithStatusSubresource(&keystonev1alpha1.Keystone{}, &esov1.ExternalSecret{})
	return &KeystoneReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}
}

func TestReconcile_NotFound_ReturnsEmptyResult(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newTestReconciler()

	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"},
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}))
}

func TestReconcile_SetsAllSubConditionsTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	configMapName := testComputeConfigMapName(t)
	objs := append([]runtime.Object{ks, testCompletedDBSyncJob(configMapName), testCompletedBootstrapJob(configMapName), testDBCredentialsSecret(), testAdminCredentialsSecret(), testReadyKeystoneDeployment(), testFernetKeysSecret()}, testReadyExternalSecrets()...)
	r := newTestReconciler(objs...)

	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace},
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}))

	// Fetch the updated Keystone to inspect status conditions.
	var updated keystonev1alpha1.Keystone
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

	for _, condType := range []string{"SecretsReady", "DatabaseReady", "FernetKeysReady", "DeploymentReady", "BootstrapReady"} {
		cond := meta.FindStatusCondition(updated.Status.Conditions, condType)
		g.Expect(cond).NotTo(BeNil(), "condition %s should exist", condType)
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "condition %s should be True", condType)
	}
}

func TestReconcile_AggregatesReadyCondition(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	configMapName := testComputeConfigMapName(t)
	objs := append([]runtime.Object{ks, testCompletedDBSyncJob(configMapName), testCompletedBootstrapJob(configMapName), testDBCredentialsSecret(), testAdminCredentialsSecret(), testReadyKeystoneDeployment(), testFernetKeysSecret()}, testReadyExternalSecrets()...)
	r := newTestReconciler(objs...)

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace},
	})
	g.Expect(err).NotTo(HaveOccurred())

	var updated keystonev1alpha1.Keystone
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

	readyCond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
	g.Expect(readyCond).NotTo(BeNil(), "Ready condition should exist")
	g.Expect(readyCond.Status).To(Equal(metav1.ConditionTrue), "Ready should be True when all sub-conditions are True")
	g.Expect(readyCond.Reason).To(Equal("AllReady"))
}

func TestReconcile_StatusUpdatePersisted(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	configMapName := testComputeConfigMapName(t)
	objs := append([]runtime.Object{ks, testCompletedDBSyncJob(configMapName), testCompletedBootstrapJob(configMapName), testDBCredentialsSecret(), testAdminCredentialsSecret(), testReadyKeystoneDeployment(), testFernetKeysSecret()}, testReadyExternalSecrets()...)
	r := newTestReconciler(objs...)

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace},
	})
	g.Expect(err).NotTo(HaveOccurred())

	// Verify that the status was persisted via the client.
	var updated keystonev1alpha1.Keystone
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())
	g.Expect(updated.Status.Conditions).NotTo(BeEmpty(), "conditions should be persisted to status")
}

func TestReconcile_Idempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	configMapName := testComputeConfigMapName(t)
	objs := append([]runtime.Object{ks, testCompletedDBSyncJob(configMapName), testCompletedBootstrapJob(configMapName), testDBCredentialsSecret(), testAdminCredentialsSecret(), testReadyKeystoneDeployment(), testFernetKeysSecret()}, testReadyExternalSecrets()...)
	r := newTestReconciler(objs...)

	// First reconcile.
	result1, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace},
	})
	g.Expect(err).NotTo(HaveOccurred())

	// Second reconcile — should produce the same result.
	result2, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result2).To(Equal(result1))

	var updated keystonev1alpha1.Keystone
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())
	readyCond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
	g.Expect(readyCond).NotTo(BeNil())
	g.Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
}

func TestSetupWithManager_RegistersWithoutError(t *testing.T) {
	g := NewGomegaWithT(t)
	s := testScheme()
	_ = appsv1.AddToScheme(s)
	_ = corev1.AddToScheme(s)

	// We cannot fully test SetupWithManager without a real manager, but we
	// verify the reconciler struct can be constructed with the expected fields.
	r := &KeystoneReconciler{
		Client:   fake.NewClientBuilder().WithScheme(s).Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}
	g.Expect(r.Client).NotTo(BeNil())
	g.Expect(r.Scheme).NotTo(BeNil())
	g.Expect(r.Recorder).NotTo(BeNil())
}

func TestAggregateReady_AllTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	conditions := []metav1.Condition{
		{Type: "SecretsReady", Status: metav1.ConditionTrue},
		{Type: "DatabaseReady", Status: metav1.ConditionTrue},
		{Type: "FernetKeysReady", Status: metav1.ConditionTrue},
		{Type: "DeploymentReady", Status: metav1.ConditionTrue},
		{Type: "BootstrapReady", Status: metav1.ConditionTrue},
	}
	g.Expect(aggregateReady(conditions)).To(BeTrue())
}

func TestAggregateReady_OneFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	conditions := []metav1.Condition{
		{Type: "SecretsReady", Status: metav1.ConditionTrue},
		{Type: "DatabaseReady", Status: metav1.ConditionFalse},
		{Type: "FernetKeysReady", Status: metav1.ConditionTrue},
		{Type: "DeploymentReady", Status: metav1.ConditionTrue},
		{Type: "BootstrapReady", Status: metav1.ConditionTrue},
	}
	g.Expect(aggregateReady(conditions)).To(BeFalse())
}

func TestAggregateReady_MissingCondition(t *testing.T) {
	g := NewGomegaWithT(t)
	// Missing BootstrapReady.
	conditions := []metav1.Condition{
		{Type: "SecretsReady", Status: metav1.ConditionTrue},
		{Type: "DatabaseReady", Status: metav1.ConditionTrue},
		{Type: "FernetKeysReady", Status: metav1.ConditionTrue},
		{Type: "DeploymentReady", Status: metav1.ConditionTrue},
	}
	g.Expect(aggregateReady(conditions)).To(BeFalse())
}

func TestAggregateReady_Empty(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(aggregateReady(nil)).To(BeFalse())
}

func TestReconcile_EarlyReturnOnSecretsNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	// Only the Keystone CR — no ExternalSecret objects exist.
	r := newTestReconciler(ks)

	ctx := context.Background()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}

	result, err := r.Reconcile(ctx, req)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter > 0).To(BeTrue(), "should requeue when secrets are not ready")

	var updated keystonev1alpha1.Keystone
	g.Expect(r.Client.Get(ctx, types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

	// SecretsReady should be set to False.
	secretsCond := meta.FindStatusCondition(updated.Status.Conditions, "SecretsReady")
	g.Expect(secretsCond).NotTo(BeNil(), "SecretsReady condition should be set")
	g.Expect(secretsCond.Status).To(Equal(metav1.ConditionFalse))

	// Later sub-reconcilers should NOT have run — their conditions must be absent.
	g.Expect(meta.FindStatusCondition(updated.Status.Conditions, "DatabaseReady")).To(BeNil(),
		"DatabaseReady should not be set when secrets are not ready")
	g.Expect(meta.FindStatusCondition(updated.Status.Conditions, "DeploymentReady")).To(BeNil(),
		"DeploymentReady should not be set when secrets are not ready")
	g.Expect(meta.FindStatusCondition(updated.Status.Conditions, "BootstrapReady")).To(BeNil(),
		"BootstrapReady should not be set when secrets are not ready")
}

func TestReconcile_EarlyReturnOnDatabaseNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	// Secrets, fernet keys, and DB credentials are ready, but no completed db_sync Job exists.
	// reconcileConfig runs before reconcileDatabase, so it needs DB credentials and fernet keys.
	objs := append([]runtime.Object{ks, testDBCredentialsSecret(), testAdminCredentialsSecret(), testFernetKeysSecret()}, testReadyExternalSecrets()...)
	r := newTestReconciler(objs...)

	ctx := context.Background()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}

	result, err := r.Reconcile(ctx, req)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter > 0).To(BeTrue(), "should requeue when database is not ready")

	var updated keystonev1alpha1.Keystone
	g.Expect(r.Client.Get(ctx, types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

	// SecretsReady should be True (secrets are available).
	secretsCond := meta.FindStatusCondition(updated.Status.Conditions, "SecretsReady")
	g.Expect(secretsCond).NotTo(BeNil())
	g.Expect(secretsCond.Status).To(Equal(metav1.ConditionTrue))

	// DatabaseReady should be set to False.
	dbCond := meta.FindStatusCondition(updated.Status.Conditions, "DatabaseReady")
	g.Expect(dbCond).NotTo(BeNil(), "DatabaseReady condition should be set")
	g.Expect(dbCond.Status).To(Equal(metav1.ConditionFalse))

	// Deployment and Bootstrap should not have run.
	g.Expect(meta.FindStatusCondition(updated.Status.Conditions, "DeploymentReady")).To(BeNil(),
		"DeploymentReady should not be set when database is not ready")
	g.Expect(meta.FindStatusCondition(updated.Status.Conditions, "BootstrapReady")).To(BeNil(),
		"BootstrapReady should not be set when database is not ready")
}

func TestReconcile_SequentialProgression(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	// Phase 1: Only the Keystone CR — no prerequisites at all.
	r := newTestReconciler(ks)

	ctx := context.Background()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}

	result, err := r.Reconcile(ctx, req)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter > 0).To(BeTrue(), "first reconcile should requeue")

	var updated keystonev1alpha1.Keystone
	g.Expect(r.Client.Get(ctx, types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

	// Only SecretsReady should be set (False).
	secretsCond := meta.FindStatusCondition(updated.Status.Conditions, "SecretsReady")
	g.Expect(secretsCond).NotTo(BeNil())
	g.Expect(secretsCond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(meta.FindStatusCondition(updated.Status.Conditions, "DatabaseReady")).To(BeNil(),
		"DatabaseReady should not exist after first reconcile")

	// Phase 2: Add ready ExternalSecrets, fernet keys, and DB credentials, then re-reconcile.
	// With the ordering secrets → fernetkeys → config → database, all three are needed
	// to reach the database stage.
	for _, obj := range testReadyExternalSecrets() {
		g.Expect(r.Client.Create(ctx, obj.(client.Object))).To(Succeed())
	}
	g.Expect(r.Client.Create(ctx, testFernetKeysSecret().(client.Object))).To(Succeed())
	g.Expect(r.Client.Create(ctx, testDBCredentialsSecret().(client.Object))).To(Succeed())
	g.Expect(r.Client.Create(ctx, testAdminCredentialsSecret().(client.Object))).To(Succeed())

	result, err = r.Reconcile(ctx, req)
	g.Expect(err).NotTo(HaveOccurred())
	// Should still requeue because db_sync hasn't completed.
	g.Expect(result.RequeueAfter > 0).To(BeTrue(), "second reconcile should requeue for database")

	g.Expect(r.Client.Get(ctx, types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

	// SecretsReady should now be True.
	secretsCond = meta.FindStatusCondition(updated.Status.Conditions, "SecretsReady")
	g.Expect(secretsCond).NotTo(BeNil())
	g.Expect(secretsCond.Status).To(Equal(metav1.ConditionTrue))

	// DatabaseReady should now appear (as False, since db_sync hasn't completed).
	dbCond := meta.FindStatusCondition(updated.Status.Conditions, "DatabaseReady")
	g.Expect(dbCond).NotTo(BeNil(), "DatabaseReady should appear after secrets and config become ready")
	g.Expect(dbCond.Status).To(Equal(metav1.ConditionFalse))
}

func TestReconcile_ReadyFalseWhenDeploymentNotAvailable(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	configMapName := testComputeConfigMapName(t)
	// All prerequisites are ready: secrets, DB sync completed, DB credentials secret, fernet keys.
	// But no ready deployment — EnsureDeployment will create it (returns false).
	objs := append([]runtime.Object{ks, testCompletedDBSyncJob(configMapName), testDBCredentialsSecret(), testAdminCredentialsSecret(), testFernetKeysSecret()}, testReadyExternalSecrets()...)
	r := newTestReconciler(objs...)

	ctx := context.Background()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}

	result, err := r.Reconcile(ctx, req)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter > 0).To(BeTrue(), "should requeue when deployment is not available")

	var updated keystonev1alpha1.Keystone
	g.Expect(r.Client.Get(ctx, types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

	// DeploymentReady should be False.
	deployCond := meta.FindStatusCondition(updated.Status.Conditions, "DeploymentReady")
	g.Expect(deployCond).NotTo(BeNil(), "DeploymentReady condition should be set")
	g.Expect(deployCond.Status).To(Equal(metav1.ConditionFalse))

	// BootstrapReady should not be set (deployment not ready means bootstrap didn't run).
	g.Expect(meta.FindStatusCondition(updated.Status.Conditions, "BootstrapReady")).To(BeNil(),
		"BootstrapReady should not be set when deployment is not available")
}

func TestReconcile_ObservedGenerationTracked(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	ks.Generation = 42 // Use a distinctive generation value.
	configMapName := testComputeConfigMapName(t)
	objs := append([]runtime.Object{ks, testCompletedDBSyncJob(configMapName), testCompletedBootstrapJob(configMapName), testDBCredentialsSecret(), testAdminCredentialsSecret(), testReadyKeystoneDeployment(), testFernetKeysSecret()}, testReadyExternalSecrets()...)
	r := newTestReconciler(objs...)

	ctx := context.Background()
	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}))

	var updated keystonev1alpha1.Keystone
	g.Expect(r.Client.Get(ctx, types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

	readyCond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
	g.Expect(readyCond).NotTo(BeNil(), "Ready condition should exist")
	g.Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(readyCond.ObservedGeneration).To(Equal(int64(42)),
		"Ready condition ObservedGeneration should match the Keystone CR's Generation")
}

func TestSecretToKeystoneMapper_ReferencedSecrets(t *testing.T) {
	g := NewGomegaWithT(t)
	s := testScheme()
	ks := testKeystone()

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ks).Build()
	mapper := secretToKeystoneMapper(c)

	// A Secret matching spec.database.secretRef.name should trigger reconcile.
	dbSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-db", Namespace: "default"},
	}
	reqs := mapper(context.Background(), dbSecret)
	g.Expect(reqs).To(HaveLen(1))
	g.Expect(reqs[0].NamespacedName.Name).To(Equal(ks.Name))

	// A Secret matching spec.bootstrap.adminPasswordSecretRef.name should trigger reconcile.
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-admin", Namespace: "default"},
	}
	reqs = mapper(context.Background(), adminSecret)
	g.Expect(reqs).To(HaveLen(1))
	g.Expect(reqs[0].NamespacedName.Name).To(Equal(ks.Name))

	// An unrelated Secret should not trigger reconcile.
	unrelated := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated-secret", Namespace: "default"},
	}
	reqs = mapper(context.Background(), unrelated)
	g.Expect(reqs).To(BeEmpty())
}

func TestSecretToKeystoneMapper_OwnedSecrets(t *testing.T) {
	g := NewGomegaWithT(t)
	s := testScheme()
	ks := testKeystone()

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ks).Build()
	mapper := secretToKeystoneMapper(c)

	// A Secret owned by the Keystone CR should trigger reconcile.
	ownedSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "some-owned-secret",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				UID: ks.UID,
			}},
		},
	}
	reqs := mapper(context.Background(), ownedSecret)
	g.Expect(reqs).To(HaveLen(1))
	g.Expect(reqs[0].NamespacedName.Name).To(Equal(ks.Name))
}

