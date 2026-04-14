// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/forge/internal/common/job"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// testScheme returns a runtime.Scheme with the types needed for controller tests.
func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = networkingv1.AddToScheme(s)
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
			CredentialKeys: keystonev1alpha1.CredentialKeysSpec{
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
// admin credentials referenced by testKeystone, plus the OpenBao-backed
// ClusterSecretStore with a Ready=True condition that reconcileSecrets now
// gates on (CC-0047).
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
	store := &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "openbao-cluster-store"},
		Status: esov1.SecretStoreStatus{
			Conditions: []esov1.SecretStoreStatusCondition{
				{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
			},
		},
	}
	return []runtime.Object{dbES, adminES, store}
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

// testCredentialKeysSecret returns a pre-existing credential keys Secret so that
// reconcileCredentialKeys does not early-return on the initial-creation path (CC-0036).
func testCredentialKeysSecret() runtime.Object {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone-credential-keys",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"0": []byte("test-credential-key-0"),
			"1": []byte("test-credential-key-1"),
			"2": []byte("test-credential-key-2"),
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

// testCompletedSchemaCheckJob returns a completed schema-check Job for testKeystone (CC-0064).
func testCompletedSchemaCheckJob(configMapName string) runtime.Object {
	ks := testKeystone()
	desired := buildSchemaCheckJob(ks, configMapName)
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
	sel := selectorLabels(ks)
	labels := commonLabels(ks)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       fmt.Sprintf("%s-api", ks.Name),
			Namespace:  "default",
			Generation: 1,
			Labels:     labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: sel},
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
func testComputeConfigMapName(t testing.TB) string {
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

// testHealthyHTTPClient returns a mock HTTPDoer that responds with HTTP 200 so
// that reconcileHealthCheck sets KeystoneAPIReady=True during integration tests (CC-0067).
func testHealthyHTTPClient() HTTPDoer {
	return &mockHTTPDoer{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("")),
		},
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
	objs := append([]runtime.Object{ks, testCompletedDBSyncJob(configMapName), testCompletedSchemaCheckJob(configMapName), testCompletedBootstrapJob(configMapName), testDBCredentialsSecret(), testAdminCredentialsSecret(), testReadyKeystoneDeployment(), testFernetKeysSecret(), testCredentialKeysSecret()}, testReadyExternalSecrets()...)
	r := newTestReconciler(objs...)
	r.HTTPClient = testHealthyHTTPClient()

	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace},
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}))

	// Fetch the updated Keystone to inspect status conditions.
	var updated keystonev1alpha1.Keystone
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

	for _, condType := range []string{"SecretsReady", "FernetKeysReady", "CredentialKeysReady", "DatabaseReady", conditionTypePolicyValidReady, "DeploymentReady", conditionTypeKeystoneAPIReady, "HPAReady", "NetworkPolicyReady", "BootstrapReady", "TrustFlushReady"} {
		cond := meta.FindStatusCondition(updated.Status.Conditions, condType)
		g.Expect(cond).NotTo(BeNil(), "condition %s should exist", condType)
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "condition %s should be True", condType)
	}
}

func TestReconcile_AggregatesReadyCondition(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	configMapName := testComputeConfigMapName(t)
	objs := append([]runtime.Object{ks, testCompletedDBSyncJob(configMapName), testCompletedSchemaCheckJob(configMapName), testCompletedBootstrapJob(configMapName), testDBCredentialsSecret(), testAdminCredentialsSecret(), testReadyKeystoneDeployment(), testFernetKeysSecret(), testCredentialKeysSecret()}, testReadyExternalSecrets()...)
	r := newTestReconciler(objs...)
	r.HTTPClient = testHealthyHTTPClient()

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
	objs := append([]runtime.Object{ks, testCompletedDBSyncJob(configMapName), testCompletedSchemaCheckJob(configMapName), testCompletedBootstrapJob(configMapName), testDBCredentialsSecret(), testAdminCredentialsSecret(), testReadyKeystoneDeployment(), testFernetKeysSecret(), testCredentialKeysSecret()}, testReadyExternalSecrets()...)
	r := newTestReconciler(objs...)
	r.HTTPClient = testHealthyHTTPClient()

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
	objs := append([]runtime.Object{ks, testCompletedDBSyncJob(configMapName), testCompletedSchemaCheckJob(configMapName), testCompletedBootstrapJob(configMapName), testDBCredentialsSecret(), testAdminCredentialsSecret(), testReadyKeystoneDeployment(), testFernetKeysSecret(), testCredentialKeysSecret()}, testReadyExternalSecrets()...)
	r := newTestReconciler(objs...)
	r.HTTPClient = testHealthyHTTPClient()

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
		{Type: "FernetKeysReady", Status: metav1.ConditionTrue},
		{Type: "CredentialKeysReady", Status: metav1.ConditionTrue},
		{Type: "DatabaseReady", Status: metav1.ConditionTrue},
		{Type: conditionTypePolicyValidReady, Status: metav1.ConditionTrue},
		{Type: "DeploymentReady", Status: metav1.ConditionTrue},
		{Type: "KeystoneAPIReady", Status: metav1.ConditionTrue},
		{Type: "HPAReady", Status: metav1.ConditionTrue},
		{Type: "NetworkPolicyReady", Status: metav1.ConditionTrue},
		{Type: "BootstrapReady", Status: metav1.ConditionTrue},
		{Type: "TrustFlushReady", Status: metav1.ConditionTrue},
	}
	g.Expect(aggregateReady(conditions)).To(BeTrue())
}

func TestAggregateReady_OneFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	conditions := []metav1.Condition{
		{Type: "SecretsReady", Status: metav1.ConditionTrue},
		{Type: "FernetKeysReady", Status: metav1.ConditionTrue},
		{Type: "CredentialKeysReady", Status: metav1.ConditionTrue},
		{Type: "DatabaseReady", Status: metav1.ConditionFalse},
		{Type: conditionTypePolicyValidReady, Status: metav1.ConditionTrue},
		{Type: "DeploymentReady", Status: metav1.ConditionTrue},
		{Type: "HPAReady", Status: metav1.ConditionTrue},
		{Type: "NetworkPolicyReady", Status: metav1.ConditionTrue},
		{Type: "BootstrapReady", Status: metav1.ConditionTrue},
		{Type: "TrustFlushReady", Status: metav1.ConditionTrue},
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

// TestSubConditionTypes_IncludesPolicyValidReady verifies that the
// PolicyValidReady condition type is registered in subConditionTypes so that
// the aggregate Ready condition gates on policy validation (CC-0058, REQ-008).
func TestSubConditionTypes_IncludesPolicyValidReady(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(subConditionTypes).To(ContainElement(conditionTypePolicyValidReady))
}

// TestRequeueValidationWait_Value verifies the polling interval for policy
// validation Job completion (CC-0058).
func TestRequeueValidationWait_Value(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(RequeueValidationWait).To(Equal(15 * time.Second))
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
	g.Expect(meta.FindStatusCondition(updated.Status.Conditions, "HPAReady")).To(BeNil(),
		"HPAReady should not be set when secrets are not ready")
	g.Expect(meta.FindStatusCondition(updated.Status.Conditions, "NetworkPolicyReady")).To(BeNil(),
		"NetworkPolicyReady should not be set when secrets are not ready")
	g.Expect(meta.FindStatusCondition(updated.Status.Conditions, "BootstrapReady")).To(BeNil(),
		"BootstrapReady should not be set when secrets are not ready")
}

func TestReconcile_EarlyReturnOnDatabaseNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	// Secrets, fernet keys, credential keys, and DB credentials are ready, but no completed db_sync Job exists.
	// reconcileConfig runs before reconcileDatabase, so it needs DB credentials and fernet keys.
	objs := append([]runtime.Object{ks, testDBCredentialsSecret(), testAdminCredentialsSecret(), testFernetKeysSecret(), testCredentialKeysSecret()}, testReadyExternalSecrets()...)
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

	// FernetKeysReady, CredentialKeysReady, and NetworkPolicyReady run in the
	// parallel group BEFORE Database, so they should be set (CC-0071).
	for _, condType := range []string{"FernetKeysReady", "CredentialKeysReady", "NetworkPolicyReady"} {
		cond := meta.FindStatusCondition(updated.Status.Conditions, condType)
		g.Expect(cond).NotTo(BeNil(), "%s should be set by the parallel group before database", condType)
	}

	// Deployment, HPA, and Bootstrap run AFTER Database, so they should not have run.
	g.Expect(meta.FindStatusCondition(updated.Status.Conditions, "DeploymentReady")).To(BeNil(),
		"DeploymentReady should not be set when database is not ready")
	g.Expect(meta.FindStatusCondition(updated.Status.Conditions, "HPAReady")).To(BeNil(),
		"HPAReady should not be set when database is not ready")
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
	g.Expect(r.Client.Create(ctx, testCredentialKeysSecret().(client.Object))).To(Succeed())
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
	// All prerequisites are ready: secrets, DB sync completed, DB credentials secret, fernet keys, credential keys.
	// But no ready deployment — EnsureDeployment will create it (returns false).
	objs := append([]runtime.Object{ks, testCompletedDBSyncJob(configMapName), testCompletedSchemaCheckJob(configMapName), testDBCredentialsSecret(), testAdminCredentialsSecret(), testFernetKeysSecret(), testCredentialKeysSecret()}, testReadyExternalSecrets()...)
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

	// NetworkPolicyReady runs before Deployment in the reconcile chain, so it
	// should be set even when the deployment is not available (CC-0039).
	npCond := meta.FindStatusCondition(updated.Status.Conditions, "NetworkPolicyReady")
	g.Expect(npCond).NotTo(BeNil(),
		"NetworkPolicyReady should be set because it runs before Deployment")
	g.Expect(npCond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(npCond.Reason).To(Equal("NetworkPolicyNotRequired"))

	// HPAReady and BootstrapReady should not be set (deployment not ready means they didn't run).
	g.Expect(meta.FindStatusCondition(updated.Status.Conditions, "HPAReady")).To(BeNil(),
		"HPAReady should not be set when deployment is not available")
	g.Expect(meta.FindStatusCondition(updated.Status.Conditions, "BootstrapReady")).To(BeNil(),
		"BootstrapReady should not be set when deployment is not available")
}

// TestReconcile_HealthCheckStopsChainOnFailure verifies that when the API
// health check returns a non-2xx response, the reconcile chain short-circuits:
// conditions before the health check in the chain are set, but conditions
// after it (HPA, Bootstrap, TrustFlush) are not (CC-0067, REQ-007).
func TestReconcile_HealthCheckStopsChainOnFailure(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	configMapName := testComputeConfigMapName(t)
	objs := append([]runtime.Object{ks, testCompletedDBSyncJob(configMapName), testCompletedSchemaCheckJob(configMapName), testCompletedBootstrapJob(configMapName), testDBCredentialsSecret(), testAdminCredentialsSecret(), testReadyKeystoneDeployment(), testFernetKeysSecret(), testCredentialKeysSecret()}, testReadyExternalSecrets()...)
	r := newTestReconciler(objs...)
	r.HTTPClient = &mockHTTPDoer{
		resp: &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("")),
		},
	}

	ctx := context.Background()
	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueHealthCheck))

	var updated keystonev1alpha1.Keystone
	g.Expect(r.Client.Get(ctx, types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

	// Conditions set BEFORE health check in the chain should be True.
	for _, condType := range []string{"SecretsReady", "FernetKeysReady", "CredentialKeysReady", "DatabaseReady", "DeploymentReady", "NetworkPolicyReady"} {
		cond := meta.FindStatusCondition(updated.Status.Conditions, condType)
		g.Expect(cond).NotTo(BeNil(), "condition %s should exist (runs before health check)", condType)
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "condition %s should be True", condType)
	}

	// KeystoneAPIReady should be False with APIUnhealthy reason.
	apiCond := meta.FindStatusCondition(updated.Status.Conditions, "KeystoneAPIReady")
	g.Expect(apiCond).NotTo(BeNil(), "KeystoneAPIReady should be set")
	g.Expect(apiCond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(apiCond.Reason).To(Equal("APIUnhealthy"))

	// Conditions set AFTER health check should NOT exist (chain stopped).
	for _, condType := range []string{"HPAReady", "BootstrapReady", "TrustFlushReady"} {
		cond := meta.FindStatusCondition(updated.Status.Conditions, condType)
		g.Expect(cond).To(BeNil(), "condition %s should not be set when health check fails", condType)
	}

	// Ready should not be set (updateStatus was called from health check, not from the end).
	readyCond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
	g.Expect(readyCond).To(BeNil(), "Ready should not be set when health check short-circuits the chain")
}

func TestReconcile_ObservedGenerationTracked(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	ks.Generation = 42 // Use a distinctive generation value.
	configMapName := testComputeConfigMapName(t)
	objs := append([]runtime.Object{ks, testCompletedDBSyncJob(configMapName), testCompletedSchemaCheckJob(configMapName), testCompletedBootstrapJob(configMapName), testDBCredentialsSecret(), testAdminCredentialsSecret(), testReadyKeystoneDeployment(), testFernetKeysSecret(), testCredentialKeysSecret()}, testReadyExternalSecrets()...)
	r := newTestReconciler(objs...)
	r.HTTPClient = testHealthyHTTPClient()

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

// TestCommonLabels_ExactKeyValues pins the exact key/value pairs produced by
// commonLabels so that future label refactors are caught immediately.
func TestCommonLabels_ExactKeyValues(t *testing.T) {
	ks := testKeystone()

	labels := commonLabels(ks)

	expected := map[string]string{
		"app.kubernetes.io/name":       "keystone",
		"app.kubernetes.io/instance":   ks.Name,
		"app.kubernetes.io/managed-by": "keystone-operator",
	}
	if len(labels) != len(expected) {
		t.Fatalf("commonLabels returned %d labels, expected %d: %#v", len(labels), len(expected), labels)
	}
	for k, v := range expected {
		got, ok := labels[k]
		if !ok {
			t.Fatalf("commonLabels missing expected key %q", k)
		}
		if got != v {
			t.Fatalf("commonLabels[%q] = %q, expected %q", k, got, v)
		}
	}
}

// TestSelectorLabels_ExactKeyValues pins the exact key/value pairs produced by
// selectorLabels so that future label refactors are caught immediately.
func TestSelectorLabels_ExactKeyValues(t *testing.T) {
	ks := testKeystone()

	sel := selectorLabels(ks)

	expected := map[string]string{
		"app.kubernetes.io/name":     "keystone",
		"app.kubernetes.io/instance": ks.Name,
	}
	if len(sel) != len(expected) {
		t.Fatalf("selectorLabels returned %d labels, expected %d: %#v", len(sel), len(expected), sel)
	}
	for k, v := range expected {
		got, ok := sel[k]
		if !ok {
			t.Fatalf("selectorLabels missing expected key %q", k)
		}
		if got != v {
			t.Fatalf("selectorLabels[%q] = %q, expected %q", k, got, v)
		}
	}
}

// TestSelectorLabels_SubsetOfCommonLabels verifies that every selector label
// key/value is present in commonLabels and that managed-by is excluded from
// the selector (keeping the Deployment selector stable across operator changes).
func TestSelectorLabels_SubsetOfCommonLabels(t *testing.T) {
	ks := testKeystone()

	common := commonLabels(ks)
	sel := selectorLabels(ks)

	for k, v := range sel {
		cv, ok := common[k]
		if !ok {
			t.Fatalf("selector key %q not present in commonLabels", k)
		}
		if cv != v {
			t.Fatalf("value mismatch for key %q: selector=%q, common=%q", k, v, cv)
		}
	}
	if _, ok := sel["app.kubernetes.io/managed-by"]; ok {
		t.Fatalf("selectorLabels must not include app.kubernetes.io/managed-by")
	}
	if _, ok := common["app.kubernetes.io/managed-by"]; !ok {
		t.Fatalf("commonLabels should include app.kubernetes.io/managed-by")
	}
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

// --- updateStatus unit tests (CC-0068) ---

// newUpdateStatusReconciler builds a KeystoneReconciler with the given Keystone CR
// pre-loaded and an optional SubResourceUpdate interceptor to simulate status
// update failures. It also fetches the Keystone object back from the fake client
// so that its ResourceVersion matches, allowing status updates to succeed when
// no interceptor error is injected.
func newUpdateStatusReconciler(t *testing.T, ks *keystonev1alpha1.Keystone, statusUpdateErr error) (*KeystoneReconciler, *keystonev1alpha1.Keystone) {
	t.Helper()
	s := testScheme()
	cb := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks.DeepCopy()).
		WithStatusSubresource(&keystonev1alpha1.Keystone{})
	if statusUpdateErr != nil {
		cb = cb.WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
				return statusUpdateErr
			},
		})
	}
	c := cb.Build()

	// Re-fetch so the object carries the ResourceVersion assigned by the fake client.
	fetched := &keystonev1alpha1.Keystone{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ks), fetched); err != nil {
		t.Fatalf("fetching Keystone from fake client: %v", err)
	}
	return &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}, fetched
}

// TestUpdateStatus_BothErrors_Joined verifies that when reconcileErr is non-nil
// and Status().Update() fails, the returned error contains both error messages
// and both are unwrappable (REQ-001, CC-0068).
func TestUpdateStatus_BothErrors_Joined(t *testing.T) {
	g := NewGomegaWithT(t)

	reconcileErr := fmt.Errorf("database connection refused")
	statusErr := fmt.Errorf("simulated status update error")
	r, ks := newUpdateStatusReconciler(t, testKeystone(), statusErr)

	_, err := r.updateStatus(context.Background(), ks, ctrl.Result{}, reconcileErr)

	g.Expect(err).To(HaveOccurred(), "should return an error when both fail")
	g.Expect(err.Error()).To(ContainSubstring("database connection refused"),
		"joined error must contain the reconcile error message")
	g.Expect(err.Error()).To(ContainSubstring("updating status:"),
		"joined error must contain the status update error prefix")
	g.Expect(err.Error()).To(ContainSubstring("simulated status update error"),
		"joined error must contain the status update error message")
}

// TestUpdateStatus_JoinedError_IsUnwrappable verifies that the joined error
// supports errors.Unwrap returning both constituent errors (REQ-001, CC-0068).
func TestUpdateStatus_JoinedError_IsUnwrappable(t *testing.T) {
	g := NewGomegaWithT(t)

	reconcileErr := fmt.Errorf("reconcile failed")
	statusErr := fmt.Errorf("status update failed")
	r, ks := newUpdateStatusReconciler(t, testKeystone(), statusErr)

	_, err := r.updateStatus(context.Background(), ks, ctrl.Result{}, reconcileErr)

	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, reconcileErr)).To(BeTrue(),
		"errors.Is must match the original reconcile error")
	g.Expect(errors.Is(err, statusErr)).To(BeTrue(),
		"errors.Is must unwrap through the joined error to find the original status update error")
}

// TestUpdateStatus_ReconcileErrorOnly_Preserved verifies that when reconcileErr
// is non-nil but Status().Update() succeeds, the returned error equals
// reconcileErr exactly (REQ-002, CC-0068).
func TestUpdateStatus_ReconcileErrorOnly_Preserved(t *testing.T) {
	g := NewGomegaWithT(t)

	reconcileErr := fmt.Errorf("sub-reconciler failed")
	r, ks := newUpdateStatusReconciler(t, testKeystone(), nil) // status update succeeds

	_, err := r.updateStatus(context.Background(), ks, ctrl.Result{}, reconcileErr)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err).To(Equal(reconcileErr),
		"returned error must be the original reconcile error, not wrapped")
}

// TestUpdateStatus_NoErrors_ReturnsNil verifies that when reconcileErr is nil
// and Status().Update() succeeds, the returned error is nil (REQ-003, CC-0068).
func TestUpdateStatus_NoErrors_ReturnsNil(t *testing.T) {
	g := NewGomegaWithT(t)

	r, ks := newUpdateStatusReconciler(t, testKeystone(), nil) // status update succeeds

	result, err := r.updateStatus(context.Background(), ks, ctrl.Result{}, nil)

	g.Expect(err).NotTo(HaveOccurred(), "should return nil when both succeed")
	g.Expect(result).To(Equal(ctrl.Result{}))
}

// TestUpdateStatus_StatusErrorOnly_Returned verifies that when reconcileErr is
// nil and Status().Update() fails, the returned error wraps only the status
// error with 'updating status:' prefix (REQ-004, CC-0068).
func TestUpdateStatus_StatusErrorOnly_Returned(t *testing.T) {
	g := NewGomegaWithT(t)

	statusErr := fmt.Errorf("conflict on status update")
	r, ks := newUpdateStatusReconciler(t, testKeystone(), statusErr)

	_, err := r.updateStatus(context.Background(), ks, ctrl.Result{}, nil)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("updating status:"))
	g.Expect(err.Error()).To(ContainSubstring("conflict on status update"))
}

// TestUpdateStatus_StatusErrorOnly_NoNilSegments verifies that when reconcileErr
// is nil and Status().Update() fails, the error string does not contain '<nil>'
// or empty segments from errors.Join (REQ-004, CC-0068).
func TestUpdateStatus_StatusErrorOnly_NoNilSegments(t *testing.T) {
	g := NewGomegaWithT(t)

	statusErr := fmt.Errorf("status write failed")
	r, ks := newUpdateStatusReconciler(t, testKeystone(), statusErr)

	_, err := r.updateStatus(context.Background(), ks, ctrl.Result{}, nil)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).NotTo(ContainSubstring("<nil>"),
		"error string must not contain <nil> from nil error arguments")
	g.Expect(err.Error()).NotTo(HavePrefix("\n"),
		"error string must not start with a newline from joined nil")
}

// TestUpdateStatus_ResultPassthrough_DualFailure verifies that when both errors
// are non-nil, the returned ctrl.Result is ctrl.Result{} (empty) (REQ-001, CC-0068).
func TestUpdateStatus_ResultPassthrough_DualFailure(t *testing.T) {
	g := NewGomegaWithT(t)

	reconcileErr := fmt.Errorf("reconcile error")
	statusErr := fmt.Errorf("status error")
	r, ks := newUpdateStatusReconciler(t, testKeystone(), statusErr)

	result, _ := r.updateStatus(context.Background(), ks, ctrl.Result{RequeueAfter: 5 * time.Second}, reconcileErr)

	g.Expect(result).To(Equal(ctrl.Result{}),
		"dual-failure should return empty Result so controller-runtime applies error-based backoff")
}

// TestUpdateStatus_ResultPassthrough_WithRequeueAfter verifies that when status
// update succeeds and the input result has RequeueAfter set, the returned
// ctrl.Result preserves RequeueAfter (REQ-002, CC-0068).
func TestUpdateStatus_ResultPassthrough_WithRequeueAfter(t *testing.T) {
	g := NewGomegaWithT(t)

	r, ks := newUpdateStatusReconciler(t, testKeystone(), nil) // status update succeeds
	inputResult := ctrl.Result{RequeueAfter: 30 * time.Second}

	result, err := r.updateStatus(context.Background(), ks, inputResult, nil)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(inputResult),
		"result must be passed through unchanged when status update succeeds")
}

func TestSubConditionTypesIncludesKeystoneAPIReady(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(subConditionTypes).To(ContainElement("KeystoneAPIReady"))
}

func TestAggregateReadyIncludesKeystoneAPIReady(t *testing.T) {
	g := NewGomegaWithT(t)
	// All expected sub-conditions are True EXCEPT KeystoneAPIReady is missing.
	// aggregateReady must return false when KeystoneAPIReady is absent.
	conditions := []metav1.Condition{
		{Type: "SecretsReady", Status: metav1.ConditionTrue},
		{Type: "FernetKeysReady", Status: metav1.ConditionTrue},
		{Type: "CredentialKeysReady", Status: metav1.ConditionTrue},
		{Type: "DatabaseReady", Status: metav1.ConditionTrue},
		{Type: conditionTypePolicyValidReady, Status: metav1.ConditionTrue},
		{Type: "DeploymentReady", Status: metav1.ConditionTrue},
		{Type: "HPAReady", Status: metav1.ConditionTrue},
		{Type: "NetworkPolicyReady", Status: metav1.ConditionTrue},
		{Type: "BootstrapReady", Status: metav1.ConditionTrue},
		{Type: "TrustFlushReady", Status: metav1.ConditionTrue},
	}
	g.Expect(aggregateReady(conditions)).To(BeFalse(),
		"aggregateReady should return false when KeystoneAPIReady condition is missing")
}

func TestAggregateReadyAllTrueWithKeystoneAPIReady(t *testing.T) {
	g := NewGomegaWithT(t)
	// All sub-conditions including KeystoneAPIReady are True.
	conditions := []metav1.Condition{
		{Type: "SecretsReady", Status: metav1.ConditionTrue},
		{Type: "FernetKeysReady", Status: metav1.ConditionTrue},
		{Type: "CredentialKeysReady", Status: metav1.ConditionTrue},
		{Type: "DatabaseReady", Status: metav1.ConditionTrue},
		{Type: conditionTypePolicyValidReady, Status: metav1.ConditionTrue},
		{Type: "DeploymentReady", Status: metav1.ConditionTrue},
		{Type: "KeystoneAPIReady", Status: metav1.ConditionTrue},
		{Type: "HPAReady", Status: metav1.ConditionTrue},
		{Type: "NetworkPolicyReady", Status: metav1.ConditionTrue},
		{Type: "BootstrapReady", Status: metav1.ConditionTrue},
		{Type: "TrustFlushReady", Status: metav1.ConditionTrue},
	}
	g.Expect(aggregateReady(conditions)).To(BeTrue(),
		"aggregateReady should return true when all conditions including KeystoneAPIReady are True")
}

// ---------------------------------------------------------------------------
// shortestRequeue tests (CC-0071, REQ-003)
// ---------------------------------------------------------------------------

// TestShortestRequeue_AllZero verifies that shortestRequeue with all zero
// Results returns ctrl.Result{} (zero value).
func TestShortestRequeue_AllZero(t *testing.T) {
	g := NewGomegaWithT(t)

	result := shortestRequeue(ctrl.Result{}, ctrl.Result{}, ctrl.Result{})

	g.Expect(result).To(Equal(ctrl.Result{}),
		"all-zero inputs must produce a zero Result")
}

// TestShortestRequeue_SingleNonZero verifies that shortestRequeue with one
// non-zero RequeueAfter returns that Result.
func TestShortestRequeue_SingleNonZero(t *testing.T) {
	g := NewGomegaWithT(t)

	result := shortestRequeue(
		ctrl.Result{},
		ctrl.Result{RequeueAfter: 15 * time.Second},
		ctrl.Result{},
	)

	g.Expect(result).To(Equal(ctrl.Result{RequeueAfter: 15 * time.Second}),
		"single non-zero RequeueAfter must be returned")
}

// TestShortestRequeue_PicksMinimum verifies that shortestRequeue with
// RequeueAfter 15s and 30s returns ctrl.Result{RequeueAfter: 15s}.
func TestShortestRequeue_PicksMinimum(t *testing.T) {
	g := NewGomegaWithT(t)

	result := shortestRequeue(
		ctrl.Result{RequeueAfter: 30 * time.Second},
		ctrl.Result{RequeueAfter: 15 * time.Second},
	)

	g.Expect(result).To(Equal(ctrl.Result{RequeueAfter: 15 * time.Second}),
		"must pick the shortest non-zero RequeueAfter")
}

// TestShortestRequeue_NoArgs verifies that shortestRequeue with zero
// variadic arguments returns ctrl.Result{} (CC-0071, REQ-003).
func TestShortestRequeue_NoArgs(t *testing.T) {
	g := NewGomegaWithT(t)

	result := shortestRequeue()

	g.Expect(result).To(Equal(ctrl.Result{}),
		"zero arguments must produce a zero Result")
}

// ---------------------------------------------------------------------------
// mergeParallelConditions tests (CC-0071, REQ-004)
// ---------------------------------------------------------------------------

// TestMergeParallelConditions_MergesCorrectly verifies that
// mergeParallelConditions extracts FernetKeysReady from source copy and sets
// it on destination.
func TestMergeParallelConditions_MergesCorrectly(t *testing.T) {
	g := NewGomegaWithT(t)

	dst := testKeystone()
	src := dst.DeepCopy()
	// Simulate a parallel sub-reconciler setting FernetKeysReady on the copy.
	meta.SetStatusCondition(&src.Status.Conditions, metav1.Condition{
		Type:               "FernetKeysReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: src.Generation,
		Reason:             "FernetKeysReady",
		Message:            "Fernet keys are ready",
	})

	mergeParallelConditions(dst, src, "FernetKeysReady")

	cond := meta.FindStatusCondition(dst.Status.Conditions, "FernetKeysReady")
	g.Expect(cond).NotTo(BeNil(), "FernetKeysReady must be present on dst after merge")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("FernetKeysReady"))
}

// TestMergeParallelConditions_SkipsMissingCondition verifies that
// mergeParallelConditions does not modify destination when source copy lacks
// the expected condition type.
func TestMergeParallelConditions_SkipsMissingCondition(t *testing.T) {
	g := NewGomegaWithT(t)

	dst := testKeystone()
	src := dst.DeepCopy()
	// src has no FernetKeysReady condition.

	mergeParallelConditions(dst, src, "FernetKeysReady")

	cond := meta.FindStatusCondition(dst.Status.Conditions, "FernetKeysReady")
	g.Expect(cond).To(BeNil(),
		"FernetKeysReady must not appear on dst when absent from src")
}

// TestMergeParallelConditions_PreservesExistingConditions verifies that
// mergeParallelConditions does not overwrite pre-existing conditions (e.g.
// SecretsReady) on the destination.
func TestMergeParallelConditions_PreservesExistingConditions(t *testing.T) {
	g := NewGomegaWithT(t)

	dst := testKeystone()
	// Simulate SecretsReady already set on dst by a prior sequential reconciler.
	meta.SetStatusCondition(&dst.Status.Conditions, metav1.Condition{
		Type:               "SecretsReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: dst.Generation,
		Reason:             "SecretsReady",
		Message:            "Secrets are ready",
	})

	src := dst.DeepCopy()
	// Simulate parallel sub-reconciler setting FernetKeysReady on the copy.
	meta.SetStatusCondition(&src.Status.Conditions, metav1.Condition{
		Type:               "FernetKeysReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: src.Generation,
		Reason:             "FernetKeysReady",
		Message:            "Fernet keys are ready",
	})

	mergeParallelConditions(dst, src, "FernetKeysReady")

	// SecretsReady must still be present and unchanged.
	secretsCond := meta.FindStatusCondition(dst.Status.Conditions, "SecretsReady")
	g.Expect(secretsCond).NotTo(BeNil(), "SecretsReady must be preserved on dst")
	g.Expect(secretsCond.Status).To(Equal(metav1.ConditionTrue))

	// FernetKeysReady must also be present.
	fernetCond := meta.FindStatusCondition(dst.Status.Conditions, "FernetKeysReady")
	g.Expect(fernetCond).NotTo(BeNil(), "FernetKeysReady must be merged onto dst")
	g.Expect(fernetCond.Status).To(Equal(metav1.ConditionTrue))
}

// ---------------------------------------------------------------------------
// reconcileParallelGroup tests (CC-0071, REQ-001, REQ-002, REQ-008)
// ---------------------------------------------------------------------------

// TestReconcileParallelGroup_SuccessPath verifies that all sub-reconcilers run
// concurrently, their conditions are merged onto the primary keystone, and the
// shortest non-zero RequeueAfter is returned (CC-0071, REQ-001).
func TestReconcileParallelGroup_SuccessPath(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newTestReconciler()
	ks := testKeystone()

	subs := []parallelSubReconciler{
		{
			conditionType: "FernetKeysReady",
			fn: func(_ context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
				meta.SetStatusCondition(&ks.Status.Conditions, metav1.Condition{
					Type:   "FernetKeysReady",
					Status: metav1.ConditionTrue,
					Reason: "Ready",
				})
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			},
		},
		{
			conditionType: "CredentialKeysReady",
			fn: func(_ context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
				meta.SetStatusCondition(&ks.Status.Conditions, metav1.Condition{
					Type:   "CredentialKeysReady",
					Status: metav1.ConditionTrue,
					Reason: "Ready",
				})
				return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
			},
		},
		{
			conditionType: "NetworkPolicyReady",
			fn: func(_ context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
				meta.SetStatusCondition(&ks.Status.Conditions, metav1.Condition{
					Type:   "NetworkPolicyReady",
					Status: metav1.ConditionTrue,
					Reason: "Ready",
				})
				return ctrl.Result{}, nil
			},
		},
	}

	result, err := r.reconcileParallelGroup(context.Background(), ks, subs)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{RequeueAfter: 15 * time.Second}),
		"must return shortest non-zero requeue")

	// All conditions must be merged onto the primary keystone.
	for _, condType := range []string{"FernetKeysReady", "CredentialKeysReady", "NetworkPolicyReady"} {
		cond := meta.FindStatusCondition(ks.Status.Conditions, condType)
		g.Expect(cond).NotTo(BeNil(), "condition %s must be merged", condType)
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	}
}

// TestReconcileParallelGroup_ErrorCancellation verifies that when one
// sub-reconciler fails, errgroup cancels the derived context and the error is
// propagated to the caller (CC-0071, REQ-002).
func TestReconcileParallelGroup_ErrorCancellation(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newTestReconciler()
	ks := testKeystone()

	subs := []parallelSubReconciler{
		{
			conditionType: "FernetKeysReady",
			fn: func(_ context.Context, _ *keystonev1alpha1.Keystone) (ctrl.Result, error) {
				return ctrl.Result{}, fmt.Errorf("fernet rotation failed")
			},
		},
		{
			conditionType: "CredentialKeysReady",
			fn: func(ctx context.Context, _ *keystonev1alpha1.Keystone) (ctrl.Result, error) {
				// Block until the context is cancelled by errgroup. If
				// cancellation does not propagate, this goroutine hangs and
				// the test times out — proving the contract.
				<-ctx.Done()
				return ctrl.Result{}, nil
			},
		},
	}

	_, err := r.reconcileParallelGroup(context.Background(), ks, subs)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("fernet rotation failed"))
}

// TestReconcileParallelGroup_PartialConditionMerge verifies that conditions
// from successful sub-reconcilers are merged even when a peer fails, so that
// partial progress is visible in status after updateStatus (CC-0071, REQ-008).
func TestReconcileParallelGroup_PartialConditionMerge(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newTestReconciler()
	ks := testKeystone()

	subs := []parallelSubReconciler{
		{
			conditionType: "FernetKeysReady",
			fn: func(_ context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
				meta.SetStatusCondition(&ks.Status.Conditions, metav1.Condition{
					Type:   "FernetKeysReady",
					Status: metav1.ConditionTrue,
					Reason: "Ready",
				})
				return ctrl.Result{}, nil
			},
		},
		{
			conditionType: "DatabaseReady",
			fn: func(_ context.Context, _ *keystonev1alpha1.Keystone) (ctrl.Result, error) {
				// Fail without setting a condition.
				return ctrl.Result{}, fmt.Errorf("db migration failed")
			},
		},
	}

	_, err := r.reconcileParallelGroup(context.Background(), ks, subs)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("db migration failed"))

	// FernetKeysReady must still be merged despite the peer failure.
	cond := meta.FindStatusCondition(ks.Status.Conditions, "FernetKeysReady")
	g.Expect(cond).NotTo(BeNil(), "FernetKeysReady must be merged from successful goroutine")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))

	// DatabaseReady should not be present (failed goroutine did not set it).
	g.Expect(meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")).To(BeNil(),
		"DatabaseReady should not be set by the failing goroutine")
}

// TestReconcile_ParallelGroupSetsAllConditions exercises the full Reconcile()
// entry point and verifies that FernetKeysReady, CredentialKeysReady, and
// NetworkPolicyReady — all three conditions produced by reconcileParallelGroup —
// are set correctly on the Keystone status. This proves the parallel group
// is wired into Reconcile() and produces the same outcome as sequential
// execution (CC-0071, REQ-001, TE-008).
func TestReconcile_ParallelGroupSetsAllConditions(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	configMapName := testComputeConfigMapName(t)

	objs := append(
		[]runtime.Object{
			ks,
			testCompletedDBSyncJob(configMapName),
			testCompletedSchemaCheckJob(configMapName),
			testCompletedBootstrapJob(configMapName),
			testDBCredentialsSecret(),
			testAdminCredentialsSecret(),
			testReadyKeystoneDeployment(),
			testFernetKeysSecret(),
			testCredentialKeysSecret(),
		},
		testReadyExternalSecrets()...,
	)
	r := newTestReconciler(objs...)
	r.HTTPClient = testHealthyHTTPClient()

	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}), "full reconcile should not requeue")

	var updated keystonev1alpha1.Keystone
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

	// Verify all three parallel-group conditions are True with correct ObservedGeneration.
	for _, condType := range []string{"FernetKeysReady", "CredentialKeysReady", "NetworkPolicyReady"} {
		cond := meta.FindStatusCondition(updated.Status.Conditions, condType)
		g.Expect(cond).NotTo(BeNil(), "condition %s must exist after full reconcile through Reconcile()", condType)
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "condition %s must be True", condType)
		g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation),
			"condition %s must track ObservedGeneration", condType)
	}

	// Verify overall Ready condition aggregates correctly.
	readyCond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
	g.Expect(readyCond).NotTo(BeNil(), "Ready condition must exist")
	g.Expect(readyCond.Status).To(Equal(metav1.ConditionTrue), "Ready must be True when all sub-conditions are True")
}

// BenchmarkReconcile_FullReconcile measures ns/op for a full reconcile cycle
// with a fake client and all sub-resources pre-created. This establishes a
// baseline for comparing sequential vs parallel execution latency
// (CC-0071, REQ-007).
func BenchmarkReconcile_FullReconcile(b *testing.B) {
	configMapName := testComputeConfigMapName(b)
	ks := testKeystone()
	objs := append(
		[]runtime.Object{
			ks,
			testCompletedDBSyncJob(configMapName),
			testCompletedSchemaCheckJob(configMapName),
			testCompletedBootstrapJob(configMapName),
			testDBCredentialsSecret(),
			testAdminCredentialsSecret(),
			testReadyKeystoneDeployment(),
			testFernetKeysSecret(),
			testCredentialKeysSecret(),
		},
		testReadyExternalSecrets()...,
	)
	r := newTestReconciler(objs...)
	r.HTTPClient = testHealthyHTTPClient()
	req := reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace},
	}

	// Warm-up: run one reconcile so all resources are created/updated.
	// Verify the result to catch setup issues early (CC-0071).
	if res, err := r.Reconcile(context.Background(), req); err != nil {
		b.Fatalf("warm-up reconcile failed: %v", err)
	} else if !res.IsZero() {
		b.Fatalf("warm-up reconcile returned non-zero result: %+v", res)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = r.Reconcile(context.Background(), req)
	}
}

// BenchmarkReconcile_FullReconcile_WithLatency measures ns/op for a full
// reconcile cycle with simulated network latency injected into every API call.
// Unlike BenchmarkReconcile_FullReconcile (which uses an in-memory fake client
// completing in microseconds), this benchmark validates that parallelizing
// independent sub-reconcilers produces a measurable wall-clock improvement
// when API calls have realistic round-trip times (CC-0071, REQ-007).
func BenchmarkReconcile_FullReconcile_WithLatency(b *testing.B) {
	const apiLatency = 5 * time.Millisecond

	configMapName := testComputeConfigMapName(b)
	ks := testKeystone()
	objs := append(
		[]runtime.Object{
			ks,
			testCompletedDBSyncJob(configMapName),
			testCompletedSchemaCheckJob(configMapName),
			testCompletedBootstrapJob(configMapName),
			testDBCredentialsSecret(),
			testAdminCredentialsSecret(),
			testReadyKeystoneDeployment(),
			testFernetKeysSecret(),
			testCredentialKeysSecret(),
		},
		testReadyExternalSecrets()...,
	)

	s := testScheme()
	cb := fake.NewClientBuilder().WithScheme(s)
	for _, obj := range objs {
		cb = cb.WithRuntimeObjects(obj)
	}
	cb = cb.WithStatusSubresource(&keystonev1alpha1.Keystone{}, &esov1.ExternalSecret{})

	// Inject simulated network latency into every client operation so that
	// parallelization gains become visible in wall-clock time (CC-0071).
	cb = cb.WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			time.Sleep(apiLatency)
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			time.Sleep(apiLatency)
			return c.List(ctx, list, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			time.Sleep(apiLatency)
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			time.Sleep(apiLatency)
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			time.Sleep(apiLatency)
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			time.Sleep(apiLatency)
			return c.Delete(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			time.Sleep(apiLatency)
			return c.Status().Update(ctx, obj, opts...)
		},
	})

	r := &KeystoneReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}
	r.HTTPClient = testHealthyHTTPClient()
	req := reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace},
	}

	// Warm-up: run one reconcile so all resources are created/updated.
	// Verify the result to catch setup issues early (CC-0071).
	if res, err := r.Reconcile(context.Background(), req); err != nil {
		b.Fatalf("warm-up reconcile failed: %v", err)
	} else if !res.IsZero() {
		b.Fatalf("warm-up reconcile returned non-zero result: %+v", res)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = r.Reconcile(context.Background(), req)
	}
}
