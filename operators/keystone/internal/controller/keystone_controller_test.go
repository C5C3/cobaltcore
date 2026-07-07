// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	commonconditions "github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/gateway"
	"github.com/c5c3/forge/internal/common/job"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
	"github.com/c5c3/forge/operators/keystone/internal/metrics"
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
	// Gateway API types are needed for reconcileHTTPRoute lifecycle tests.
	_ = gatewayv1.Install(s)
	return s
}

// testKeystone returns a minimal valid Keystone CR for tests. Both finalizers
// are pre-populated to match steady-state after Reconcile has persisted them,
// so tests exercising the sub-reconciler chain are not forced to first observe
// the one-shot AddFinalizer requeues for the MariaDB and OpenBao
// finalizers.
func testKeystone() *keystonev1alpha1.Keystone {
	return &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-keystone",
			Namespace:  "default",
			UID:        "ks-uid",
			Generation: 1,
			Finalizers: []string{keystoneFinalizer, keystoneOpenBaoFinalizer},
		},
		Spec: keystonev1alpha1.KeystoneSpec{
			Deployment: keystonev1alpha1.DeploymentSpec{Replicas: 3},
			Image:      commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
			Database:   commonv1.DatabaseSpec{Host: "db.example.com", Port: 3306, Database: "keystone", SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"}},
			Cache:      commonv1.CacheSpec{Backend: "dogpile.cache.pymemcache", Servers: []string{"mc:11211"}},
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
// gates on.
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
// that reconcileSecrets' IsSecretReady check expects.
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
// reconcileCredentialKeys does not early-return on the initial-creation path.
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
	desired := buildDBSyncJob(ks, configMapName, "")
	now := metav1.Now()
	j := desired.DeepCopy()
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template),
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

// testCompletedSchemaCheckJob returns a completed schema-check Job for testKeystone.
func testCompletedSchemaCheckJob(configMapName string) runtime.Object {
	ks := testKeystone()
	desired := buildSchemaCheckJob(ks, configMapName, "")
	now := metav1.Now()
	j := desired.DeepCopy()
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template),
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
			Name:       ks.Name,
			Namespace:  "default",
			Generation: 1,
			Labels:     labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: sel},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "keystone", Image: "ghcr.io/c5c3/keystone:2025.2"}}},
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
	// The full-reconcile tests using this helper include testAdminCredentialsSecret()
	// whose `password` value is "admin-password". The bootstrap Job's re-run key is
	// the admin-password digest (NOT the pod-template hash, so a release upgrade does not re-trigger it); stamp the matching digest so reconcileBootstrap
	// (which derives the same key from the Secret) does not see the Job as stale
	// during full reconcile.
	sum := sha256.Sum256([]byte("admin-password"))
	adminHash := hex.EncodeToString(sum[:])
	desired := buildBootstrapJob(ks, configMapName, "", fmt.Sprintf("%s-fernet-keys", ks.Name), adminHash)
	now := metav1.Now()
	j := desired.DeepCopy()
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: adminHash,
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
	name, err := r.reconcileConfig(context.Background(), ks, false)
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
	cb = cb.WithStatusSubresource(&keystonev1alpha1.Keystone{}, &esov1.ExternalSecret{}, &keystonev1alpha1.KeystoneIdentityBackend{})
	// Register the KeystoneIdentityBackend indexes with the production
	// extractors so reconcileIdentityBackends' MatchingFields List works
	// against the fake client.
	cb = cb.WithIndex(&keystonev1alpha1.KeystoneIdentityBackend{}, IdentityBackendKeystoneRefIndexKey, identityBackendKeystoneRefExtractor)
	cb = cb.WithIndex(&keystonev1alpha1.KeystoneIdentityBackend{}, IdentityBackendSecretNameIndexKey, identityBackendSecretNameExtractor)
	return &KeystoneReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}
}

// testHealthyHTTPClient returns a mock HTTPDoer that responds with HTTP 200 so
// that reconcileHealthCheck sets KeystoneAPIReady=True during integration tests.
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
	g.Expect(r.Get(context.Background(), types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

	// TrustFlushReady reason is TrustFlushNotRequired here because tests use a fake client that bypasses the defaulting webhook.
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
	g.Expect(r.Get(context.Background(), types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

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
	g.Expect(r.Get(context.Background(), types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())
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
	g.Expect(r.Get(context.Background(), types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())
	readyCond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
	g.Expect(readyCond).NotTo(BeNil())
	g.Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
}

func TestAggregateReady_AllTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	conditions := []metav1.Condition{
		{Type: "SecretsReady", Status: metav1.ConditionTrue},
		{Type: "FernetKeysReady", Status: metav1.ConditionTrue},
		{Type: "CredentialKeysReady", Status: metav1.ConditionTrue},
		{Type: "DatabaseReady", Status: metav1.ConditionTrue},
		{Type: conditionTypeDatabaseTLSReady, Status: metav1.ConditionTrue},
		{Type: conditionTypePolicyValidReady, Status: metav1.ConditionTrue},
		{Type: "DeploymentReady", Status: metav1.ConditionTrue},
		{Type: "KeystoneAPIReady", Status: metav1.ConditionTrue},
		{Type: "HPAReady", Status: metav1.ConditionTrue},
		{Type: "NetworkPolicyReady", Status: metav1.ConditionTrue},
		{Type: conditionTypeHTTPRouteReady, Status: metav1.ConditionTrue},
		{Type: "BootstrapReady", Status: metav1.ConditionTrue},
		{Type: "TrustFlushReady", Status: metav1.ConditionTrue},
		{Type: conditionTypePasswordRotationReady, Status: metav1.ConditionTrue},
		{Type: conditionTypeIdentityBackendsReady, Status: metav1.ConditionTrue},
	}
	g.Expect(commonconditions.AllTrue(conditions, subConditionTypes...)).To(BeTrue())
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
	g.Expect(commonconditions.AllTrue(conditions, subConditionTypes...)).To(BeFalse())
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
	g.Expect(commonconditions.AllTrue(conditions, subConditionTypes...)).To(BeFalse())
}

func TestAggregateReady_Empty(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(commonconditions.AllTrue(nil, subConditionTypes...)).To(BeFalse())
}

// TestSubConditionTypes_IncludesPolicyValidReady verifies that the
// PolicyValidReady condition type is registered in subConditionTypes so that
// the aggregate Ready condition gates on policy validation.
func TestSubConditionTypes_IncludesPolicyValidReady(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(subConditionTypes).To(ContainElement(conditionTypePolicyValidReady))
}

// TestRequeueValidationWait_Value verifies the polling interval for policy
// validation Job completion.
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
	g.Expect(r.Get(ctx, types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

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
	g.Expect(r.Get(ctx, types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

	// SecretsReady should be True (secrets are available).
	secretsCond := meta.FindStatusCondition(updated.Status.Conditions, "SecretsReady")
	g.Expect(secretsCond).NotTo(BeNil())
	g.Expect(secretsCond.Status).To(Equal(metav1.ConditionTrue))

	// DatabaseReady should be set to False.
	dbCond := meta.FindStatusCondition(updated.Status.Conditions, "DatabaseReady")
	g.Expect(dbCond).NotTo(BeNil(), "DatabaseReady condition should be set")
	g.Expect(dbCond.Status).To(Equal(metav1.ConditionFalse))

	// FernetKeysReady, CredentialKeysReady, and NetworkPolicyReady run in the
	// parallel group BEFORE Database, so they should be set.
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

// TestReconcile_ParallelGroupRequeueShortCircuitsChain reproduces issue #467
// Problem 2. When reconcileFernetKeys/reconcileCredentialKeys create their
// initial key Secret they request a requeue. That requeue flows through the
// parallel group's shortestRequeue, which keys off RequeueAfter — so the
// chain must short-circuit and NOT advance to reconcileDatabase in the same
// pass. The fixture is identical to TestReconcile_EarlyReturnOnDatabaseNotReady
// except the Fernet and credential Secrets are deliberately absent so the
// parallel group generates them and requeues.
//
// Before the fix, the producers returned the deprecated ctrl.Result{Requeue:
// true}; shortestRequeue zeroed it, the parallel group returned a zero result,
// and the chain wrongly continued into reconcileDatabase (DatabaseReady would
// be set). Asserting DatabaseReady is absent pins the short-circuit.
func TestReconcile_ParallelGroupRequeueShortCircuitsChain(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	// Secrets and DB credentials are ready, but the Fernet and credential key
	// Secrets do NOT exist yet — the parallel group must generate them and
	// request a requeue.
	objs := append([]runtime.Object{ks, testDBCredentialsSecret(), testAdminCredentialsSecret()}, testReadyExternalSecrets()...)
	r := newTestReconciler(objs...)

	ctx := context.Background()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}

	result, err := r.Reconcile(ctx, req)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter > 0).To(BeTrue(),
		"the fernet/credential requeue must short-circuit the chain (issue #467)")

	var updated keystonev1alpha1.Keystone
	g.Expect(r.Get(ctx, types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

	// The parallel group ran: FernetKeysReady/CredentialKeysReady are set False
	// (keys just generated, awaiting confirmation).
	for _, condType := range []string{"FernetKeysReady", "CredentialKeysReady"} {
		cond := meta.FindStatusCondition(updated.Status.Conditions, condType)
		g.Expect(cond).NotTo(BeNil(), "%s should be set by the parallel group", condType)
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	}

	// DatabaseReady must be ABSENT: the parallel group's requeue short-circuited
	// the chain before reconcileDatabase ran. With the pre-fix Requeue:true the
	// result was dropped and the chain would have reached Database.
	g.Expect(meta.FindStatusCondition(updated.Status.Conditions, "DatabaseReady")).To(BeNil(),
		"DatabaseReady must NOT be set — the parallel-group requeue short-circuited before reconcileDatabase (issue #467)")
}

// TestReconcile_ConfigFailureDoesNotLeaveReadyTrue reproduces issue #467
// Problem 1. A previously-healthy CR (every sub-condition True, Ready True)
// whose reconcileConfig then fails — here because policyOverrides.configMapRef
// points at a ConfigMap that does not exist — must flip Ready to False, not
// re-aggregate the stale-True sub-conditions into Ready=True at the new
// generation.
//
// Before the fix the Config step returned a naked error and setReadyCondition
// re-aggregated the still-True sub-conditions, so Ready stayed True and the
// failure was visible only in logs and the error counter. markConfigFailed now
// flips SecretsReady=False on the same path.
func TestReconcile_ConfigFailureDoesNotLeaveReadyTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	// Break reconcileConfig: point policyOverrides at a ConfigMap that does not
	// exist so buildPolicyYAML → LoadPolicyFromConfigMap returns NotFound.
	ks.Spec.PolicyOverrides = &commonv1.PolicySpec{
		ConfigMapRef: &corev1.LocalObjectReference{Name: "missing-policy-cm"},
	}
	// Seed every sub-condition True to simulate a CR that was already Ready. The
	// aggregate would therefore be Ready=True without the fix.
	for _, ct := range subConditionTypes {
		meta.SetStatusCondition(&ks.Status.Conditions, metav1.Condition{
			Type:   ct,
			Status: metav1.ConditionTrue,
			Reason: "Seeded",
		})
	}
	// Ready secrets + DB credentials so Secrets/DatabaseTLS/DBConnectionSecret
	// all pass and the chain reaches Config. Fernet/credential Secrets are not
	// needed — Config runs before the parallel group.
	objs := append([]runtime.Object{ks, testDBCredentialsSecret(), testAdminCredentialsSecret()}, testReadyExternalSecrets()...)
	r := newTestReconciler(objs...)

	ctx := context.Background()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}

	_, err := r.Reconcile(ctx, req)
	// reconcileConfig surfaces the error so controller-runtime backs off.
	g.Expect(err).To(HaveOccurred(), "reconcileConfig must surface the missing-ConfigMap error")

	var updated keystonev1alpha1.Keystone
	g.Expect(r.Get(ctx, types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

	// The config failure flipped SecretsReady=False with the dedicated reason.
	secretsCond := meta.FindStatusCondition(updated.Status.Conditions, "SecretsReady")
	g.Expect(secretsCond).NotTo(BeNil())
	g.Expect(secretsCond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(secretsCond.Reason).To(Equal(conditionReasonConfigError))

	// The aggregate Ready must follow it to False at the observed generation —
	// it must NOT stay stale-True (issue #467).
	readyCond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
	g.Expect(readyCond).NotTo(BeNil(), "Ready condition should be set")
	g.Expect(readyCond.Status).To(Equal(metav1.ConditionFalse),
		"Ready must be False when reconcileConfig fails, not stale-True (issue #467)")
	g.Expect(readyCond.ObservedGeneration).To(Equal(updated.Generation))
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
	g.Expect(r.Get(ctx, types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

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
		g.Expect(r.Create(ctx, obj.(client.Object))).To(Succeed())
	}
	g.Expect(r.Create(ctx, testFernetKeysSecret().(client.Object))).To(Succeed())
	g.Expect(r.Create(ctx, testCredentialKeysSecret().(client.Object))).To(Succeed())
	g.Expect(r.Create(ctx, testDBCredentialsSecret().(client.Object))).To(Succeed())
	g.Expect(r.Create(ctx, testAdminCredentialsSecret().(client.Object))).To(Succeed())

	result, err = r.Reconcile(ctx, req)
	g.Expect(err).NotTo(HaveOccurred())
	// Should still requeue because db_sync hasn't completed.
	g.Expect(result.RequeueAfter > 0).To(BeTrue(), "second reconcile should requeue for database")

	g.Expect(r.Get(ctx, types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

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
	g.Expect(r.Get(ctx, types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

	// DeploymentReady should be False.
	deployCond := meta.FindStatusCondition(updated.Status.Conditions, "DeploymentReady")
	g.Expect(deployCond).NotTo(BeNil(), "DeploymentReady condition should be set")
	g.Expect(deployCond.Status).To(Equal(metav1.ConditionFalse))

	// The aggregate Ready must follow DeploymentReady=False even though the
	// chain short-circuited at the deployment stage — updateStatus recomputes it
	// on every persist. This is the keystone-side network-partition contract:
	// a depooled Pod must surface as Ready=False, not a stale Ready=True
	// (SC-CHAOS-006).
	readyCond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
	g.Expect(readyCond).NotTo(BeNil(), "Ready condition should be set")
	g.Expect(readyCond.Status).To(Equal(metav1.ConditionFalse),
		"Ready must be False when the deployment is not available")
	g.Expect(readyCond.Reason).To(Equal("NotAllReady"))

	// NetworkPolicyReady runs before Deployment in the reconcile chain, so it
	// should be set even when the deployment is not available.
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
// after it (HPA, Bootstrap, TrustFlush) are not.
// TestReconcile_HealthCheckFailureDrivesReadyFalse verifies that a failing
// health check flips KeystoneAPIReady=False and drives the aggregate Ready to
// False. Since the second parallel group runs HTTPRoute, HealthCheck, HPA,
// Bootstrap, and TrustFlush concurrently (issue #361), a HealthCheck failure no
// longer short-circuits the other four — they still run and set their
// conditions in the same pass, and shortestRequeue surfaces the health-check
// requeue.
func TestReconcile_HealthCheckFailureDrivesReadyFalse(t *testing.T) {
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
	// HealthCheck requeues at RequeueHealthCheck (10s); the other group members
	// resolve to zero or longer requeues, so shortestRequeue returns 10s.
	g.Expect(result.RequeueAfter).To(Equal(RequeueHealthCheck))

	var updated keystonev1alpha1.Keystone
	g.Expect(r.Get(ctx, types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

	// Conditions set before the second group should be True.
	for _, condType := range []string{"SecretsReady", "FernetKeysReady", "CredentialKeysReady", "DatabaseReady", "DeploymentReady", "NetworkPolicyReady"} {
		cond := meta.FindStatusCondition(updated.Status.Conditions, condType)
		g.Expect(cond).NotTo(BeNil(), "condition %s should exist (runs before the second group)", condType)
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "condition %s should be True", condType)
	}

	// KeystoneAPIReady should be False with APIUnhealthy reason.
	apiCond := meta.FindStatusCondition(updated.Status.Conditions, "KeystoneAPIReady")
	g.Expect(apiCond).NotTo(BeNil(), "KeystoneAPIReady should be set")
	g.Expect(apiCond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(apiCond.Reason).To(Equal("APIUnhealthy"))

	// The other second-group members run in parallel with HealthCheck, so their
	// conditions are set in the same pass rather than skipped.
	for _, condType := range []string{"HTTPRouteReady", "HPAReady", "BootstrapReady", "TrustFlushReady"} {
		cond := meta.FindStatusCondition(updated.Status.Conditions, condType)
		g.Expect(cond).NotTo(BeNil(),
			"condition %s must be set: the parallel group runs it even when HealthCheck fails", condType)
	}

	// Ready must be re-aggregated to False: KeystoneAPIReady=False drives the
	// aggregate to False (SC-CHAOS-006).
	readyCond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
	g.Expect(readyCond).NotTo(BeNil(), "Ready should be set")
	g.Expect(readyCond.Status).To(Equal(metav1.ConditionFalse),
		"Ready must be False when KeystoneAPIReady is False")
	g.Expect(readyCond.Reason).To(Equal("NotAllReady"))
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
	g.Expect(r.Get(ctx, types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

	readyCond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
	g.Expect(readyCond).NotTo(BeNil(), "Ready condition should exist")
	g.Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(readyCond.ObservedGeneration).To(Equal(int64(42)),
		"Ready condition ObservedGeneration should match the Keystone CR's Generation")

	g.Expect(updated.Status.ObservedGeneration).To(Equal(int64(42)),
		"top-level status.observedGeneration should match the Keystone CR's Generation")
}

// newMapperFakeClientBuilder returns a ClientBuilder pre-registered with the
// KeystoneSecretNameIndexKey field indexer so the refactored
// secretToKeystoneMapper can resolve MatchingFields lookups against the fake
// client. Tests that need to inject interceptors can extend the returned
// builder before calling Build.
func newMapperFakeClientBuilder(objs ...client.Object) *fake.ClientBuilder {
	return fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objs...).
		WithIndex(&keystonev1alpha1.Keystone{}, KeystoneSecretNameIndexKey, keystoneSecretNameExtractor)
}

// newMapperFakeClient is the common-case shortcut for tests that only need a
// pre-indexed fake client with the given objects seeded.
func newMapperFakeClient(objs ...client.Object) client.Client {
	return newMapperFakeClientBuilder(objs...).Build()
}

// keystoneOwnerRef returns a well-formed OwnerReference pointing at the given
// Keystone CR — Kind and APIVersion are set so secretToKeystoneMapper's
// owner-ref path recognises the reference.
func keystoneOwnerRef(ks *keystonev1alpha1.Keystone) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: keystonev1alpha1.GroupVersion.String(),
		Kind:       "Keystone",
		Name:       ks.Name,
		UID:        ks.UID,
	}
}

func TestSecretToKeystoneMapper_ReferencedSecrets(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()

	c := newMapperFakeClient(ks)
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
	ks := testKeystone()

	c := newMapperFakeClient(ks)
	mapper := secretToKeystoneMapper(c)

	// A Secret carrying a Keystone OwnerReference should trigger reconcile.
	ownedSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "some-owned-secret",
			Namespace:       "default",
			OwnerReferences: []metav1.OwnerReference{keystoneOwnerRef(ks)},
		},
	}
	reqs := mapper(context.Background(), ownedSecret)
	g.Expect(reqs).To(HaveLen(1))
	g.Expect(reqs[0].NamespacedName.Name).To(Equal(ks.Name))
}

// TestSecretToKeystoneMapper_EnqueuesOnStagingSecretUpdate verifies that a
// rotation staging Secret — labelled with StagingSecretLabelKey AND
// owner-referenced to the Keystone CR — triggers reconcile when it changes.
// Staging Secrets carry the Keystone as owner, so the owner-ref branch of
// secretToKeystoneMapper is what enqueues them.
func TestSecretToKeystoneMapper_EnqueuesOnStagingSecretUpdate(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()

	c := newMapperFakeClient(ks)
	mapper := secretToKeystoneMapper(c)

	// Fernet staging Secret: label + Keystone ownerRef.
	fernetStaging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fernetStagingSecretName(ks),
			Namespace: "default",
			Labels: map[string]string{
				StagingSecretLabelKey: "fernet-keys",
			},
			OwnerReferences: []metav1.OwnerReference{keystoneOwnerRef(ks)},
		},
	}
	reqs := mapper(context.Background(), fernetStaging)
	g.Expect(reqs).To(HaveLen(1))
	g.Expect(reqs[0].NamespacedName.Name).To(Equal(ks.Name))

	// Credential staging Secret: same shape, different label value.
	credStaging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      credentialStagingSecretName(ks),
			Namespace: "default",
			Labels: map[string]string{
				StagingSecretLabelKey: "credential-keys",
			},
			OwnerReferences: []metav1.OwnerReference{keystoneOwnerRef(ks)},
		},
	}
	reqs = mapper(context.Background(), credStaging)
	g.Expect(reqs).To(HaveLen(1))
	g.Expect(reqs[0].NamespacedName.Name).To(Equal(ks.Name))
}

// TestSecretToKeystoneMapper_OrphanStagingSecretNotEnqueued verifies that a
// Secret carrying the rotation-target label but lacking both an owner
// reference to any Keystone CR AND a matching referenced Secret name does
// NOT enqueue a reconcile — defensive behavior to avoid reacting to stray
// Secrets that happen to carry our label.
func TestSecretToKeystoneMapper_OrphanStagingSecretNotEnqueued(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()

	c := newMapperFakeClient(ks)
	mapper := secretToKeystoneMapper(c)

	orphan := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			// Name does NOT match any SecretRef on ks.
			Name:      "someone-elses-rotation",
			Namespace: "default",
			Labels: map[string]string{
				StagingSecretLabelKey: "fernet-keys",
			},
			// No owner references.
		},
	}
	reqs := mapper(context.Background(), orphan)
	g.Expect(reqs).To(BeEmpty())
}

// --- updateStatus unit tests ---

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
// and both are unwrappable.
func TestUpdateStatus_BothErrors_Joined(t *testing.T) {
	g := NewGomegaWithT(t)

	reconcileErr := fmt.Errorf("database connection refused")
	statusErr := fmt.Errorf("simulated status update error")
	r, ks := newUpdateStatusReconciler(t, testKeystone(), statusErr)

	_, err := r.updateStatus(context.Background(), ks, nil, ctrl.Result{}, reconcileErr)

	g.Expect(err).To(HaveOccurred(), "should return an error when both fail")
	g.Expect(err.Error()).To(ContainSubstring("database connection refused"),
		"joined error must contain the reconcile error message")
	g.Expect(err.Error()).To(ContainSubstring("updating status:"),
		"joined error must contain the status update error prefix")
	g.Expect(err.Error()).To(ContainSubstring("simulated status update error"),
		"joined error must contain the status update error message")
}

// TestUpdateStatus_JoinedError_IsUnwrappable verifies that the joined error
// supports errors.Unwrap returning both constituent errors.
func TestUpdateStatus_JoinedError_IsUnwrappable(t *testing.T) {
	g := NewGomegaWithT(t)

	reconcileErr := fmt.Errorf("reconcile failed")
	statusErr := fmt.Errorf("status update failed")
	r, ks := newUpdateStatusReconciler(t, testKeystone(), statusErr)

	_, err := r.updateStatus(context.Background(), ks, nil, ctrl.Result{}, reconcileErr)

	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, reconcileErr)).To(BeTrue(),
		"errors.Is must match the original reconcile error")
	g.Expect(errors.Is(err, statusErr)).To(BeTrue(),
		"errors.Is must unwrap through the joined error to find the original status update error")
}

// TestUpdateStatus_ReconcileErrorOnly_Preserved verifies that when reconcileErr
// is non-nil but Status().Update() succeeds, the returned error equals
// reconcileErr exactly.
func TestUpdateStatus_ReconcileErrorOnly_Preserved(t *testing.T) {
	g := NewGomegaWithT(t)

	reconcileErr := fmt.Errorf("sub-reconciler failed")
	r, ks := newUpdateStatusReconciler(t, testKeystone(), nil) // status update succeeds

	_, err := r.updateStatus(context.Background(), ks, nil, ctrl.Result{}, reconcileErr)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err).To(Equal(reconcileErr),
		"returned error must be the original reconcile error, not wrapped")
}

// TestUpdateStatus_NoErrors_ReturnsNil verifies that when reconcileErr is nil
// and Status().Update() succeeds, the returned error is nil.
func TestUpdateStatus_NoErrors_ReturnsNil(t *testing.T) {
	g := NewGomegaWithT(t)

	r, ks := newUpdateStatusReconciler(t, testKeystone(), nil) // status update succeeds

	result, err := r.updateStatus(context.Background(), ks, nil, ctrl.Result{}, nil)

	g.Expect(err).NotTo(HaveOccurred(), "should return nil when both succeed")
	g.Expect(result).To(Equal(ctrl.Result{}))
}

// TestUpdateStatus_SkipsWriteWhenUnchanged verifies the C3 gate: when the
// snapshot equals the status updateStatus computes, no Status().Update is
// issued. The reconciler's Status().Update is wired to always fail, so a
// skipped write is observable as a nil error return.
func TestUpdateStatus_SkipsWriteWhenUnchanged(t *testing.T) {
	g := NewGomegaWithT(t)

	statusErr := fmt.Errorf("status update must not be called on an unchanged status")
	r, ks := newUpdateStatusReconciler(t, testKeystone(), statusErr)

	// Bring ks.Status into the exact state updateStatus would compute (Ready
	// aggregated + ObservedGeneration stamped), then snapshot it — a converged
	// steady-state pass.
	setReadyCondition(ks)
	ks.Status.ObservedGeneration = ks.Generation
	snapshot := ks.Status.DeepCopy()

	_, err := r.updateStatus(context.Background(), ks, snapshot, ctrl.Result{}, nil)
	g.Expect(err).NotTo(HaveOccurred(),
		"an unchanged status must skip the write; the failing Status().Update proves it was not called")
}

// TestUpdateStatus_WritesWhenChanged verifies the C3 gate still writes when the
// status differs from the snapshot: a differing snapshot forces the (failing)
// write, which surfaces the error.
func TestUpdateStatus_WritesWhenChanged(t *testing.T) {
	g := NewGomegaWithT(t)

	statusErr := fmt.Errorf("status write failed")
	r, ks := newUpdateStatusReconciler(t, testKeystone(), statusErr)

	// An empty snapshot differs from the final status (which gains a Ready
	// condition), so the write must be attempted.
	snapshot := &keystonev1alpha1.KeystoneStatus{}
	_, err := r.updateStatus(context.Background(), ks, snapshot, ctrl.Result{}, nil)
	g.Expect(err).To(HaveOccurred(), "a changed status must attempt the write")
	g.Expect(err.Error()).To(ContainSubstring("updating status:"))
}

// TestUpdateStatus_StatusErrorOnly_Returned verifies that when reconcileErr is
// nil and Status().Update() fails, the returned error wraps only the status
// error with 'updating status:' prefix.
func TestUpdateStatus_StatusErrorOnly_Returned(t *testing.T) {
	g := NewGomegaWithT(t)

	statusErr := fmt.Errorf("conflict on status update")
	r, ks := newUpdateStatusReconciler(t, testKeystone(), statusErr)

	_, err := r.updateStatus(context.Background(), ks, nil, ctrl.Result{}, nil)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("updating status:"))
	g.Expect(err.Error()).To(ContainSubstring("conflict on status update"))
}

// TestUpdateStatus_StatusErrorOnly_NoNilSegments verifies that when reconcileErr
// is nil and Status().Update() fails, the error string does not contain '<nil>'
// or empty segments from errors.Join.
func TestUpdateStatus_StatusErrorOnly_NoNilSegments(t *testing.T) {
	g := NewGomegaWithT(t)

	statusErr := fmt.Errorf("status write failed")
	r, ks := newUpdateStatusReconciler(t, testKeystone(), statusErr)

	_, err := r.updateStatus(context.Background(), ks, nil, ctrl.Result{}, nil)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).NotTo(ContainSubstring("<nil>"),
		"error string must not contain <nil> from nil error arguments")
	g.Expect(err.Error()).NotTo(HavePrefix("\n"),
		"error string must not start with a newline from joined nil")
}

// TestUpdateStatus_ResultPassthrough_DualFailure verifies that when both errors
// are non-nil, the returned ctrl.Result is ctrl.Result{} (empty).
func TestUpdateStatus_ResultPassthrough_DualFailure(t *testing.T) {
	g := NewGomegaWithT(t)

	reconcileErr := fmt.Errorf("reconcile error")
	statusErr := fmt.Errorf("status error")
	r, ks := newUpdateStatusReconciler(t, testKeystone(), statusErr)

	result, _ := r.updateStatus(context.Background(), ks, nil, ctrl.Result{RequeueAfter: 5 * time.Second}, reconcileErr)

	g.Expect(result).To(Equal(ctrl.Result{}),
		"dual-failure should return empty Result so controller-runtime applies error-based backoff")
}

// TestUpdateStatus_ResultPassthrough_WithRequeueAfter verifies that when status
// update succeeds and the input result has RequeueAfter set, the returned
// ctrl.Result preserves RequeueAfter.
func TestUpdateStatus_ResultPassthrough_WithRequeueAfter(t *testing.T) {
	g := NewGomegaWithT(t)

	r, ks := newUpdateStatusReconciler(t, testKeystone(), nil) // status update succeeds
	inputResult := ctrl.Result{RequeueAfter: 30 * time.Second}

	result, err := r.updateStatus(context.Background(), ks, nil, inputResult, nil)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(inputResult),
		"result must be passed through unchanged when status update succeeds")
}

// TestUpdateStatus_ReaggregatesReadyWhenSubConditionDegrades verifies that
// updateStatus recomputes the aggregate Ready condition on every persist, so a
// CR that was fully Ready flips to Ready=False the moment a sub-condition
// degrades and short-circuits the reconcile chain — rather than leaving Ready
// stale at True. This is the keystone-side network-partition scenario: the
// database-aware readiness probe depools the Pods, reconcileDeployment requeues
// with DeploymentReady=False, and the aggregate Ready must follow it down. A
// missing re-aggregation here was why the mariadb-network-partition chaos test
// timed out waiting for Ready=False (SC-CHAOS-006).
func TestUpdateStatus_ReaggregatesReadyWhenSubConditionDegrades(t *testing.T) {
	g := NewGomegaWithT(t)
	r, ks := newUpdateStatusReconciler(t, testKeystone(), nil)

	// Seed a status as if a prior fully-healthy pass had completed — every
	// sub-condition True and the aggregate Ready True — except DeploymentReady,
	// which has just degraded to False under the partition.
	for _, ct := range subConditionTypes {
		status := metav1.ConditionTrue
		if ct == "DeploymentReady" {
			status = metav1.ConditionFalse
		}
		meta.SetStatusCondition(&ks.Status.Conditions, metav1.Condition{
			Type:   ct,
			Status: status,
			Reason: "Seed",
		})
	}
	meta.SetStatusCondition(&ks.Status.Conditions, metav1.Condition{
		Type:   "Ready",
		Status: metav1.ConditionTrue,
		Reason: "AllReady",
	})

	_, err := r.updateStatus(context.Background(), ks, nil, ctrl.Result{RequeueAfter: time.Second}, nil)
	g.Expect(err).NotTo(HaveOccurred())

	updated := &keystonev1alpha1.Keystone{}
	g.Expect(r.Get(context.Background(), client.ObjectKeyFromObject(ks), updated)).To(Succeed())
	readyCond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
	g.Expect(readyCond).NotTo(BeNil(), "Ready condition must be present")
	g.Expect(readyCond.Status).To(Equal(metav1.ConditionFalse),
		"Ready must flip to False once DeploymentReady degrades, not stay stale at True")
	g.Expect(readyCond.Reason).To(Equal("NotAllReady"))
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
	g.Expect(commonconditions.AllTrue(conditions, subConditionTypes...)).To(BeFalse(),
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
		{Type: conditionTypeDatabaseTLSReady, Status: metav1.ConditionTrue},
		{Type: conditionTypePolicyValidReady, Status: metav1.ConditionTrue},
		{Type: "DeploymentReady", Status: metav1.ConditionTrue},
		{Type: "KeystoneAPIReady", Status: metav1.ConditionTrue},
		{Type: "HPAReady", Status: metav1.ConditionTrue},
		{Type: "NetworkPolicyReady", Status: metav1.ConditionTrue},
		{Type: conditionTypeHTTPRouteReady, Status: metav1.ConditionTrue},
		{Type: "BootstrapReady", Status: metav1.ConditionTrue},
		{Type: "TrustFlushReady", Status: metav1.ConditionTrue},
		{Type: conditionTypePasswordRotationReady, Status: metav1.ConditionTrue},
		{Type: conditionTypeIdentityBackendsReady, Status: metav1.ConditionTrue},
	}
	g.Expect(commonconditions.AllTrue(conditions, subConditionTypes...)).To(BeTrue(),
		"aggregateReady should return true when all conditions including KeystoneAPIReady are True")
}

// TestSubConditionTypes_IncludesHTTPRouteReady verifies the HTTPRouteReady
// condition participates in the aggregate Ready gating so that an HTTPRoute
// rejected by the Gateway controller flips Ready to False.
func TestSubConditionTypes_IncludesHTTPRouteReady(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(subConditionTypes).To(ContainElement(conditionTypeHTTPRouteReady))
}

// TestAggregateReady_MissingHTTPRouteReady_ReturnsFalse verifies that
// aggregateReady returns false when the HTTPRouteReady condition is absent,
// ensuring the Ready aggregate gates on HTTPRoute acceptance.
func TestAggregateReady_MissingHTTPRouteReady_ReturnsFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	// All expected sub-conditions True EXCEPT HTTPRouteReady is missing.
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
	g.Expect(commonconditions.AllTrue(conditions, subConditionTypes...)).To(BeFalse(),
		"aggregateReady should return false when HTTPRouteReady condition is missing")
}

// TestSubConditionTypes_IncludesPasswordRotationReady verifies the
// PasswordRotationReady condition participates in the aggregate Ready gating so
// that a failed scheduled admin-password rotation flips Ready to False
func TestSubConditionTypes_IncludesPasswordRotationReady(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(subConditionTypes).To(ContainElement(conditionTypePasswordRotationReady))
}

// TestSubReconcilerConditionTypes_MapsPasswordRotation verifies the
// "PasswordRotation" sub_reconciler label resolves to the
// PasswordRotationReady condition_type so error-counter metrics carry the
// correct condition_type label rather than the UNKNOWN sentinel.
func TestSubReconcilerConditionTypes_MapsPasswordRotation(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(subReconcilerConditionTypes).To(HaveKeyWithValue("PasswordRotation", conditionTypePasswordRotationReady))
}

// ---------------------------------------------------------------------------
// shortestRequeue tests
// ---------------------------------------------------------------------------

// TestShortestRequeue_AllZero verifies that shortestRequeue with all zero
// Results returns ctrl.Result{} (zero value).
func TestShortestRequeue_AllZero(t *testing.T) {
	g := NewGomegaWithT(t)

	result := commonreconcile.ShortestRequeue(ctrl.Result{}, ctrl.Result{}, ctrl.Result{})

	g.Expect(result).To(Equal(ctrl.Result{}),
		"all-zero inputs must produce a zero Result")
}

// TestShortestRequeue_SingleNonZero verifies that shortestRequeue with one
// non-zero RequeueAfter returns that Result.
func TestShortestRequeue_SingleNonZero(t *testing.T) {
	g := NewGomegaWithT(t)

	result := commonreconcile.ShortestRequeue(
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

	result := commonreconcile.ShortestRequeue(
		ctrl.Result{RequeueAfter: 30 * time.Second},
		ctrl.Result{RequeueAfter: 15 * time.Second},
	)

	g.Expect(result).To(Equal(ctrl.Result{RequeueAfter: 15 * time.Second}),
		"must pick the shortest non-zero RequeueAfter")
}

// TestShortestRequeue_NoArgs verifies that shortestRequeue with zero
// variadic arguments returns ctrl.Result{}.
func TestShortestRequeue_NoArgs(t *testing.T) {
	g := NewGomegaWithT(t)

	result := commonreconcile.ShortestRequeue()

	g.Expect(result).To(Equal(ctrl.Result{}),
		"zero arguments must produce a zero Result")
}

// ---------------------------------------------------------------------------
// mergeParallelConditions tests
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

	commonreconcile.MergeCondition(&dst.Status.Conditions, src.Status.Conditions, "FernetKeysReady")

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

	commonreconcile.MergeCondition(&dst.Status.Conditions, src.Status.Conditions, "FernetKeysReady")

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

	commonreconcile.MergeCondition(&dst.Status.Conditions, src.Status.Conditions, "FernetKeysReady")

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
// reconcileParallelGroup tests
// ---------------------------------------------------------------------------

// TestReconcileParallelGroup_SuccessPath verifies that all sub-reconcilers run
// concurrently, their conditions are merged onto the primary keystone, and the
// shortest non-zero RequeueAfter is returned.
func TestReconcileParallelGroup_SuccessPath(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newTestReconciler()
	ks := testKeystone()

	subs := []commonreconcile.ParallelStep[*keystonev1alpha1.Keystone]{
		{
			ConditionType: "FernetKeysReady",
			Fn: func(_ context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
				meta.SetStatusCondition(&ks.Status.Conditions, metav1.Condition{
					Type:   "FernetKeysReady",
					Status: metav1.ConditionTrue,
					Reason: "Ready",
				})
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			},
		},
		{
			ConditionType: "CredentialKeysReady",
			Fn: func(_ context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
				meta.SetStatusCondition(&ks.Status.Conditions, metav1.Condition{
					Type:   "CredentialKeysReady",
					Status: metav1.ConditionTrue,
					Reason: "Ready",
				})
				return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
			},
		},
		{
			ConditionType: "NetworkPolicyReady",
			Fn: func(_ context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
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
// propagated to the caller.
func TestReconcileParallelGroup_ErrorCancellation(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newTestReconciler()
	ks := testKeystone()

	subs := []commonreconcile.ParallelStep[*keystonev1alpha1.Keystone]{
		{
			ConditionType: "FernetKeysReady",
			Fn: func(_ context.Context, _ *keystonev1alpha1.Keystone) (ctrl.Result, error) {
				return ctrl.Result{}, fmt.Errorf("fernet rotation failed")
			},
		},
		{
			ConditionType: "CredentialKeysReady",
			Fn: func(ctx context.Context, _ *keystonev1alpha1.Keystone) (ctrl.Result, error) {
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
// partial progress is visible in status after updateStatus.
func TestReconcileParallelGroup_PartialConditionMerge(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newTestReconciler()
	ks := testKeystone()

	subs := []commonreconcile.ParallelStep[*keystonev1alpha1.Keystone]{
		{
			ConditionType: "FernetKeysReady",
			Fn: func(_ context.Context, ks *keystonev1alpha1.Keystone) (ctrl.Result, error) {
				meta.SetStatusCondition(&ks.Status.Conditions, metav1.Condition{
					Type:   "FernetKeysReady",
					Status: metav1.ConditionTrue,
					Reason: "Ready",
				})
				return ctrl.Result{}, nil
			},
		},
		{
			ConditionType: "DatabaseReady",
			Fn: func(_ context.Context, _ *keystonev1alpha1.Keystone) (ctrl.Result, error) {
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
// execution (TE-008).
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
	g.Expect(r.Get(context.Background(), types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

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

// ---------------------------------------------------------------------------
// Reconcile finalizer lifecycle tests
// ---------------------------------------------------------------------------

// keystoneGroupResource is the GroupResource used when synthesizing apierror
// values for Keystone in finalizer tests.
var keystoneGroupResource = schema.GroupResource{
	Group:    "keystone.openstack.c5c3.io",
	Resource: "keystones",
}

// markKeystoneTerminating issues Delete on the given Keystone CR via the fake
// client so that DeletionTimestamp is set while at least one finalizer blocks
// actual removal from the store. It returns the refreshed object so the caller
// sees the current ResourceVersion and DeletionTimestamp.
func markKeystoneTerminating(t *testing.T, c client.Client, ks *keystonev1alpha1.Keystone) *keystonev1alpha1.Keystone {
	t.Helper()
	g := NewGomegaWithT(t)
	g.Expect(c.Delete(context.Background(), ks)).To(Succeed())
	refreshed := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(ks), refreshed)).To(Succeed())
	g.Expect(refreshed.DeletionTimestamp.IsZero()).To(BeFalse(),
		"DeletionTimestamp must be set after Delete to simulate a terminating CR")
	return refreshed
}

// TestReconcile_AddsFinalizerOnFirstReconcile verifies that Reconcile installs
// the Keystone finalizer on a live CR that lacks it and returns Requeue=true so
// the next pass observes the persisted finalizer before any sub-reconciler
// runs.
func TestReconcile_AddsFinalizerOnFirstReconcile(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	ks.Finalizers = nil
	r := newTestReconciler(ks)

	ctx := context.Background()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}

	result, err := r.Reconcile(ctx, req)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{Requeue: true}),
		"first reconcile must return Requeue=true after persisting the finalizer")

	var updated keystonev1alpha1.Keystone
	g.Expect(r.Get(ctx, req.NamespacedName, &updated)).To(Succeed())
	g.Expect(controllerutil.ContainsFinalizer(&updated, keystoneFinalizer)).To(BeTrue(),
		"finalizer must be persisted to etcd on the first reconcile")

	// No sub-reconciler ran: SecretsReady (the first sub-reconciler's condition)
	// must be absent because we short-circuited after the finalizer Update.
	g.Expect(meta.FindStatusCondition(updated.Status.Conditions, "SecretsReady")).To(BeNil(),
		"sub-reconcilers must not run on the finalizer-install pass")
}

// TestReconcile_FinalizerAlreadyPresent_NoExtraUpdate verifies that when the
// Keystone CR already carries the finalizer, Reconcile does NOT call Update on
// the Keystone object (status updates go through SubResourceUpdate, not
// Update) — proving the AddFinalizer path is skipped.
func TestReconcile_FinalizerAlreadyPresent_NoExtraUpdate(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone() // testKeystone pre-populates the finalizer.

	s := testScheme()
	var keystoneUpdates int
	cb := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		WithStatusSubresource(&keystonev1alpha1.Keystone{}, &esov1.ExternalSecret{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*keystonev1alpha1.Keystone); ok {
					keystoneUpdates++
				}
				return cl.Update(ctx, obj, opts...)
			},
		})
	r := &KeystoneReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	ctx := context.Background()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}

	result, err := r.Reconcile(ctx, req)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).NotTo(Equal(ctrl.Result{Requeue: true}),
		"must not return Requeue=true when the finalizer is already present")
	g.Expect(keystoneUpdates).To(Equal(0),
		"no r.Update on Keystone must occur when the finalizer is already installed")
}

// TestReconcile_FinalizerUpdateConflict_ReturnsError verifies that a Conflict
// on the finalizer-installing Update is propagated as a reconciler error so
// controller-runtime retries with backoff.
func TestReconcile_FinalizerUpdateConflict_ReturnsError(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	ks.Finalizers = nil

	s := testScheme()
	conflictErr := apierrors.NewConflict(keystoneGroupResource, ks.Name,
		fmt.Errorf("the object has been modified; please apply your changes to the latest version and try again"))

	cb := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		WithStatusSubresource(&keystonev1alpha1.Keystone{}, &esov1.ExternalSecret{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
				if _, ok := obj.(*keystonev1alpha1.Keystone); ok {
					return conflictErr
				}
				return nil
			},
		})
	r := &KeystoneReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	ctx := context.Background()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}

	_, err := r.Reconcile(ctx, req)

	g.Expect(err).To(HaveOccurred(), "Conflict on AddFinalizer Update must surface as a reconciler error")
	g.Expect(err.Error()).To(ContainSubstring("adding finalizer"),
		"error must preserve the 'adding finalizer' wrapper from Reconcile")
	g.Expect(apierrors.IsConflict(err)).To(BeTrue(),
		"wrapped Conflict must remain recognizable via apierrors.IsConflict")
}

// TestReconcile_TerminatingCR_SkipsSubReconcilers verifies that when the CR is
// terminating with the Keystone finalizer present, Reconcile delegates to
// reconcileDelete and never invokes any sub-reconciler — proven by the absence
// of any sub-reconciler condition (e.g. SecretsReady) on the CR after the
// reconcile.
func TestReconcile_TerminatingCR_SkipsSubReconcilers(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	// Attach a foreign finalizer so the CR stays in etcd after reconcileDelete
	// releases the Keystone finalizer, letting the test inspect conditions on
	// the terminating CR.
	ks.Finalizers = append(ks.Finalizers, "foreign.example.com/keep-alive")
	db, user, grant := mariaDBResources(ks)

	r := newTestReconciler(ks, db, user, grant)
	ctx := context.Background()
	_ = markKeystoneTerminating(t, r.Client, ks)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}
	_, err := r.Reconcile(ctx, req)
	g.Expect(err).NotTo(HaveOccurred())

	var updated keystonev1alpha1.Keystone
	g.Expect(r.Get(ctx, req.NamespacedName, &updated)).To(Succeed(),
		"Keystone must remain while the foreign finalizer is attached")

	// No sub-reconciler ran: each first-phase sub-condition must be absent.
	for _, condType := range []string{"SecretsReady", "FernetKeysReady", "CredentialKeysReady", "DatabaseReady", "NetworkPolicyReady"} {
		g.Expect(meta.FindStatusCondition(updated.Status.Conditions, condType)).To(BeNil(),
			"condition %s must not be set on a terminating CR", condType)
	}
}

// TestReconcile_TerminatingCR_IssuesDeleteAndReleasesFinalizer verifies that
// Reconcile on a terminating CR issues Delete on every MariaDB CR and releases
// the Keystone finalizer in a single pass, even while the MariaDB CRs are held
// in Terminating state by another finalizer. Waiting for the MariaDB operator
// to complete teardown created a deadlock under concurrent deletions.
func TestReconcile_TerminatingCR_IssuesDeleteAndReleasesFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	db, user, grant := mariaDBResources(ks)
	// Pending-delete finalizers hold the MariaDB CRs in Terminating state so
	// the test can verify they were marked for deletion before the Keystone
	// finalizer was released.
	db.Finalizers = []string{"test.c5c3.io/pending-delete"}
	user.Finalizers = []string{"test.c5c3.io/pending-delete"}
	grant.Finalizers = []string{"test.c5c3.io/pending-delete"}

	r := newTestReconciler(ks, db, user, grant)
	ctx := context.Background()
	_ = markKeystoneTerminating(t, r.Client, ks)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}
	result, err := r.Reconcile(ctx, req)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}),
		"terminating CR must resolve in a single pass without requeue")

	// The Keystone CR no longer has our finalizer (may be gone from etcd if
	// the fake client collected it).
	var updated keystonev1alpha1.Keystone
	getErr := r.Get(ctx, req.NamespacedName, &updated)
	if getErr == nil {
		g.Expect(controllerutil.ContainsFinalizer(&updated, keystoneFinalizer)).To(BeFalse(),
			"Keystone finalizer must be released in a single reconcile pass")
	} else {
		g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
			"Keystone must either be finalizer-free or garbage-collected")
	}

	// Each MariaDB CR must now have DeletionTimestamp set — proof that
	// finalizeDatabaseResources issued Delete before returning.
	for name, obj := range map[string]client.Object{
		"Database": &mariadbv1alpha1.Database{},
		"User":     &mariadbv1alpha1.User{},
		"Grant":    &mariadbv1alpha1.Grant{},
	} {
		g.Expect(r.Get(ctx, client.ObjectKey{Name: ks.Name, Namespace: ks.Namespace}, obj)).
			To(Succeed(), "%s must still exist (held by pending-delete finalizer)", name)
		g.Expect(obj.GetDeletionTimestamp().IsZero()).To(BeFalse(),
			"%s must be marked for deletion after finalizeDatabaseResources runs", name)
	}
}

// TestReconcile_TerminatingCR_WithoutFinalizer_NoOp verifies that a terminating
// CR without the Keystone finalizer is a no-op: no events, no Updates, empty
// result. This mirrors a CR created before this operator version, or one whose
// finalizer was already released in a prior pass.
func TestReconcile_TerminatingCR_WithoutFinalizer_NoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	// Replace the Keystone finalizer with an unrelated one so the CR is held in
	// Terminating state by someone else and reconcileDelete hits the
	// no-finalizer fast path.
	ks.Finalizers = []string{"foreign.example.com/finalizer"}

	r := newTestReconciler(ks)
	ctx := context.Background()
	_ = markKeystoneTerminating(t, r.Client, ks)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}
	result, err := r.Reconcile(ctx, req)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}),
		"no-finalizer terminating CR must produce an empty Result")
	expectNoEvent(g, r)
}

// ---------------------------------------------------------------------------
// Reconcile finalizer event-emission tests
// ---------------------------------------------------------------------------

// TestReconcile_TerminatingCR_EmitsFinalizingDatabaseEvent verifies that a
// terminating CR with live MariaDB CRs emits "FinalizingDatabase" to announce
// cleanup work and "DatabaseFinalized" after issuing the Deletes, both in the
// same reconcile pass.
func TestReconcile_TerminatingCR_EmitsFinalizingDatabaseEvent(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	db, user, grant := mariaDBResources(ks)

	r := newTestReconciler(ks, db, user, grant)
	ctx := context.Background()
	_ = markKeystoneTerminating(t, r.Client, ks)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}
	_, err := r.Reconcile(ctx, req)
	g.Expect(err).NotTo(HaveOccurred())

	fakeRecorder := r.Recorder.(*record.FakeRecorder)
	var events []string
drain:
	for {
		select {
		case evt := <-fakeRecorder.Events:
			events = append(events, evt)
		default:
			break drain
		}
	}
	g.Expect(events).To(ContainElement(ContainSubstring("FinalizingDatabase")),
		"FinalizingDatabase event must be emitted when live MariaDB CRs exist")
	g.Expect(events).To(ContainElement(ContainSubstring("DatabaseFinalized")),
		"DatabaseFinalized event must be emitted after Delete is issued")
}

// TestReconcile_TerminatingCR_EmitsFinalizingDatabaseEventOnlyOnce guards the
// sentinel invariant documented at Reconcile and in the Events
// section of docs/reference/keystone-reconciler.md: "FinalizingDatabase" is
// emitted exactly once per termination, suppressed on subsequent reconcile
// passes while the MariaDB CRs remain Terminating. A regression that re-emits
// the event on every requeue would still pass the single-pass tests but flood
// the event stream in production — this test forces a second pass against the
// same Keystone CR and asserts the event count stays at one.
func TestReconcile_TerminatingCR_EmitsFinalizingDatabaseEventOnlyOnce(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	// Foreign finalizer keeps the Keystone CR in etcd after the first pass
	// releases the Keystone finalizer, so the second pass actually re-enters
	// reconcileDelete and must apply its own suppression logic.
	ks.Finalizers = append(ks.Finalizers, "foreign.example.com/keep-alive")
	db, user, grant := mariaDBResources(ks)
	// Pending-delete finalizers hold the MariaDB CRs in Terminating state
	// between passes — matching a real MariaDB operator still tearing them
	// down when Reconcile re-enters.
	db.Finalizers = []string{"test.c5c3.io/pending-delete"}
	user.Finalizers = []string{"test.c5c3.io/pending-delete"}
	grant.Finalizers = []string{"test.c5c3.io/pending-delete"}

	r := newTestReconciler(ks, db, user, grant)
	ctx := context.Background()
	_ = markKeystoneTerminating(t, r.Client, ks)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}

	_, err := r.Reconcile(ctx, req)
	g.Expect(err).NotTo(HaveOccurred(), "first reconcile pass must succeed")
	_, err = r.Reconcile(ctx, req)
	g.Expect(err).NotTo(HaveOccurred(), "second reconcile pass must succeed")

	fakeRecorder := r.Recorder.(*record.FakeRecorder)
	var events []string
drain:
	for {
		select {
		case evt := <-fakeRecorder.Events:
			events = append(events, evt)
		default:
			break drain
		}
	}

	finalizingCount := 0
	for _, evt := range events {
		if strings.Contains(evt, "FinalizingDatabase") {
			finalizingCount++
		}
	}
	g.Expect(finalizingCount).To(Equal(1),
		"FinalizingDatabase must be emitted exactly once across both reconcile passes; got events: %v", events)
}

// TestReconcile_TerminatingCR_SentinelSuppressesEventAfterUpdateConflict is a
// sibling to TestReconcile_TerminatingCR_EmitsFinalizingDatabaseEventOnlyOnce
// that specifically exercises the hasLiveMariaDBResources sentinel path with
// the keystoneFinalizer retained across passes. In the foreign-finalizer
// variant, the Keystone finalizer is removed on the first pass and the second
// pass exits early via the ContainsFinalizer guard — so suppression is proven
// end-to-end but the sentinel's DeletionTimestamp-aware branch is not directly
// exercised. Here we inject a Conflict on the RemoveFinalizer Update so the
// keystoneFinalizer persists, forcing the second pass to re-enter
// reconcileDelete, observe the now-Terminating MariaDB CRs, and suppress the
// FinalizingDatabase event via the sentinel rather than the guard
func TestReconcile_TerminatingCR_SentinelSuppressesEventAfterUpdateConflict(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	db, user, grant := mariaDBResources(ks)
	// Pending-delete finalizers hold the MariaDB CRs in Terminating state
	// after the first pass's Delete so the sentinel observes
	// DeletionTimestamp-set CRs on the second pass.
	db.Finalizers = []string{"test.c5c3.io/pending-delete"}
	user.Finalizers = []string{"test.c5c3.io/pending-delete"}
	grant.Finalizers = []string{"test.c5c3.io/pending-delete"}

	s := testScheme()
	conflictErr := apierrors.NewConflict(keystoneGroupResource, ks.Name,
		fmt.Errorf("the object has been modified; please apply your changes to the latest version and try again"))
	var keystoneUpdates int
	cb := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks, db, user, grant).
		WithStatusSubresource(&keystonev1alpha1.Keystone{}, &esov1.ExternalSecret{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*keystonev1alpha1.Keystone); ok {
					keystoneUpdates++
					if keystoneUpdates == 1 {
						return conflictErr
					}
				}
				return cl.Update(ctx, obj, opts...)
			},
		})
	r := &KeystoneReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	ctx := context.Background()
	_ = markKeystoneTerminating(t, r.Client, ks)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}

	// Pass 1: RemoveFinalizer Update fails with Conflict, so the keystone
	// finalizer remains on the persisted CR. Delete has already been issued
	// on the MariaDB CRs, transitioning them to Terminating.
	_, err := r.Reconcile(ctx, req)
	g.Expect(err).To(HaveOccurred(),
		"first pass must surface the Conflict on the finalizer-removing Update")
	g.Expect(err.Error()).To(ContainSubstring("removing finalizer"),
		"error must preserve the 'removing finalizer' wrapper from reconcileDelete")
	g.Expect(apierrors.IsConflict(err)).To(BeTrue(),
		"wrapped Conflict must remain recognizable via apierrors.IsConflict")

	// Verify both preconditions for the sentinel path on pass 2:
	// (a) the keystone finalizer is still on the persisted CR, so pass 2
	//     proceeds past the ContainsFinalizer guard into hasLiveMariaDBResources
	// (b) the MariaDB CRs are Terminating, so the sentinel returns false and
	//     suppresses FinalizingDatabase.
	persisted := &keystonev1alpha1.Keystone{}
	g.Expect(r.Get(ctx, req.NamespacedName, persisted)).To(Succeed())
	g.Expect(controllerutil.ContainsFinalizer(persisted, keystoneFinalizer)).To(BeTrue(),
		"keystoneFinalizer must remain on the CR after the Update conflict")
	for _, obj := range []client.Object{
		&mariadbv1alpha1.Database{},
		&mariadbv1alpha1.User{},
		&mariadbv1alpha1.Grant{},
	} {
		g.Expect(r.Get(ctx, client.ObjectKey{Name: ks.Name, Namespace: ks.Namespace}, obj)).To(Succeed(),
			"MariaDB %T must still exist after pass 1 (held by pending-delete finalizer)", obj)
		g.Expect(obj.GetDeletionTimestamp().IsZero()).To(BeFalse(),
			"MariaDB %T must be Terminating after pass 1 so the sentinel suppresses on pass 2", obj)
	}

	// Pass 2: sentinel observes Terminating CRs and suppresses the event;
	// the RemoveFinalizer Update now succeeds and the CR is released.
	_, err = r.Reconcile(ctx, req)
	g.Expect(err).NotTo(HaveOccurred(),
		"second pass must succeed once the conflict is cleared")
	getErr := r.Get(ctx, req.NamespacedName, &keystonev1alpha1.Keystone{})
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
		"Keystone must be removed after the second pass releases the finalizer")

	fakeRecorder := r.Recorder.(*record.FakeRecorder)
	var events []string
drain:
	for {
		select {
		case evt := <-fakeRecorder.Events:
			events = append(events, evt)
		default:
			break drain
		}
	}

	finalizingCount := 0
	for _, evt := range events {
		if strings.Contains(evt, "FinalizingDatabase") {
			finalizingCount++
		}
	}
	g.Expect(finalizingCount).To(Equal(1),
		"FinalizingDatabase must be emitted exactly once — by pass 1 when CRs were live, suppressed by the sentinel on pass 2; got events: %v", events)
}

// TestReconcile_TerminatingCR_EmitsDatabaseFinalizedEvent verifies that a
// brownfield terminating CR (no MariaDB CRs ever created) emits only the
// "DatabaseFinalized" Normal event before releasing the Keystone finalizer —
// the FinalizingDatabase sentinel is false because there is no live cleanup
// work to announce.
func TestReconcile_TerminatingCR_EmitsDatabaseFinalizedEvent(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()

	r := newTestReconciler(ks)
	ctx := context.Background()
	_ = markKeystoneTerminating(t, r.Client, ks)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}
	result, err := r.Reconcile(ctx, req)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}),
		"terminating brownfield CR must release the finalizer in a single pass")

	fakeRecorder := r.Recorder.(*record.FakeRecorder)
	var events []string
drain:
	for {
		select {
		case evt := <-fakeRecorder.Events:
			events = append(events, evt)
		default:
			break drain
		}
	}
	g.Expect(events).To(ContainElement(ContainSubstring("DatabaseFinalized")),
		"DatabaseFinalized event must be emitted after finalizeDatabaseResources reports done")
	g.Expect(events).NotTo(ContainElement(ContainSubstring("FinalizingDatabase")),
		"FinalizingDatabase must not be emitted for a brownfield CR with no MariaDB CRs")

	// Finalizer must have been released — the CR is gone from the store.
	getErr := r.Get(ctx, req.NamespacedName, &keystonev1alpha1.Keystone{})
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
		"Keystone must be removed after the finalizer is released")
}

// ---------------------------------------------------------------------------
// Reconcile openbao-finalizer lifecycle tests
// ---------------------------------------------------------------------------

// TestReconcile_AddsOpenBaoFinalizerOnFirstReconcile verifies that Reconcile
// installs the OpenBao finalizer on a live CR that lacks it and returns
// Requeue=true so the next pass observes the persisted finalizer before any
// sub-reconciler runs.
func TestReconcile_AddsOpenBaoFinalizerOnFirstReconcile(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	// Leave the MariaDB finalizer installed so the first pass skips straight
	// to the OpenBao AddFinalizer block instead of requeueing on MariaDB.
	ks.Finalizers = []string{keystoneFinalizer}
	r := newTestReconciler(ks)

	ctx := context.Background()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}

	result, err := r.Reconcile(ctx, req)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{Requeue: true}),
		"first reconcile must return Requeue=true after persisting the openbao finalizer")

	var updated keystonev1alpha1.Keystone
	g.Expect(r.Get(ctx, req.NamespacedName, &updated)).To(Succeed())
	g.Expect(controllerutil.ContainsFinalizer(&updated, keystoneOpenBaoFinalizer)).To(BeTrue(),
		"openbao finalizer must be persisted to etcd on the first reconcile")

	// No sub-reconciler ran: SecretsReady (the first sub-reconciler's condition)
	// must be absent because we short-circuited after the openbao AddFinalizer
	// Update.
	g.Expect(meta.FindStatusCondition(updated.Status.Conditions, "SecretsReady")).To(BeNil(),
		"sub-reconcilers must not run on the openbao-finalizer install pass")
}

// TestReconcile_OpenBaoFinalizerAlreadyPresent_NoExtraUpdate verifies that
// when the Keystone CR already carries both finalizers, Reconcile does NOT
// call Update on the Keystone object — proving the openbao AddFinalizer path
// is skipped in steady state.
func TestReconcile_OpenBaoFinalizerAlreadyPresent_NoExtraUpdate(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone() // testKeystone pre-populates BOTH finalizers.

	s := testScheme()
	var keystoneUpdates int
	cb := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		WithStatusSubresource(&keystonev1alpha1.Keystone{}, &esov1.ExternalSecret{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*keystonev1alpha1.Keystone); ok {
					keystoneUpdates++
				}
				return cl.Update(ctx, obj, opts...)
			},
		})
	r := &KeystoneReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	ctx := context.Background()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}

	result, err := r.Reconcile(ctx, req)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).NotTo(Equal(ctrl.Result{Requeue: true}),
		"must not return Requeue=true when both finalizers are already present")
	g.Expect(keystoneUpdates).To(Equal(0),
		"no r.Update on Keystone must occur when both finalizers are already installed")
}

// TestReconcile_OpenBaoFinalizerUpdateConflict_ReturnsError verifies that a
// Conflict on the openbao-finalizer-installing Update is propagated as a
// reconciler error so controller-runtime retries with backoff.
func TestReconcile_OpenBaoFinalizerUpdateConflict_ReturnsError(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	// MariaDB finalizer already installed so the conflict fires on the
	// openbao AddFinalizer Update, not the MariaDB one.
	ks.Finalizers = []string{keystoneFinalizer}

	s := testScheme()
	conflictErr := apierrors.NewConflict(keystoneGroupResource, ks.Name,
		fmt.Errorf("the object has been modified; please apply your changes to the latest version and try again"))

	cb := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		WithStatusSubresource(&keystonev1alpha1.Keystone{}, &esov1.ExternalSecret{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
				if _, ok := obj.(*keystonev1alpha1.Keystone); ok {
					return conflictErr
				}
				return nil
			},
		})
	r := &KeystoneReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	ctx := context.Background()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}

	_, err := r.Reconcile(ctx, req)

	g.Expect(err).To(HaveOccurred(), "Conflict on openbao AddFinalizer Update must surface as a reconciler error")
	g.Expect(err.Error()).To(ContainSubstring(`adding finalizer "keystone.openstack.c5c3.io/openbao-finalizer"`),
		"error must carry the finalizer-attributed wrapper from EnsureFinalizer")
	g.Expect(apierrors.IsConflict(err)).To(BeTrue(),
		"wrapped Conflict must remain recognizable via apierrors.IsConflict")
}

// TestReconcile_TerminatingCR_RequeuesWhilePushSecretsExist verifies that a
// terminating CR carrying the openbao finalizer with a PushSecret held in
// Terminating state by ESO's cleanup finalizer returns
// ctrl.Result{RequeueAfter: RequeueSecretPolling} and records the
// OpenBaoFinalizerBlocked condition via updateStatus — keeping the CR alive
// until ESO has purged the kv-v2 path.
func TestReconcile_TerminatingCR_RequeuesWhilePushSecretsExist(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	// Drop the MariaDB finalizer so reconcileDelete short-circuits and the
	// test isolates the openbao-blocked path.
	ks.Finalizers = []string{keystoneOpenBaoFinalizer}

	fernet := pushSecretWithPendingDelete("test-keystone-fernet-keys-backup")

	r := newTestReconciler(ks, fernet)
	ctx := context.Background()
	_ = markKeystoneTerminating(t, r.Client, ks)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}
	result, err := r.Reconcile(ctx, req)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretPolling),
		"terminating CR with a stuck PushSecret must requeue after RequeueSecretPolling")

	var updated keystonev1alpha1.Keystone
	g.Expect(r.Get(ctx, req.NamespacedName, &updated)).To(Succeed(),
		"Keystone must remain while the openbao finalizer is blocked")
	g.Expect(controllerutil.ContainsFinalizer(&updated, keystoneOpenBaoFinalizer)).To(BeTrue(),
		"openbao finalizer must remain until the PushSecret is garbage-collected")

	cond := meta.FindStatusCondition(updated.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil(),
		"SecretsReady condition must be set when the openbao finalizer is blocked")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("OpenBaoFinalizerBlocked"))
	g.Expect(cond.Message).To(ContainSubstring("test-keystone-fernet-keys-backup"),
		"OpenBaoFinalizerBlocked message must name the stuck PushSecret")
}

// TestReconcile_TerminatingCR_WithoutOpenBaoFinalizer_NoOp verifies that a
// terminating CR without the openbao finalizer does not emit any openbao
// event and does not call finalizeOpenBaoSecrets. This mirrors a brownfield
// Keystone CR created before this operator version.
func TestReconcile_TerminatingCR_WithoutOpenBaoFinalizer_NoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	// Keep the MariaDB finalizer so the MariaDB cleanup runs, but drop the
	// openbao finalizer so reconcileDeleteOpenBao fast-paths.
	ks.Finalizers = []string{keystoneFinalizer}

	r := newTestReconciler(ks)
	ctx := context.Background()
	_ = markKeystoneTerminating(t, r.Client, ks)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}
	result, err := r.Reconcile(ctx, req)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}),
		"terminating CR without openbao finalizer must resolve without requeue")

	fakeRecorder := r.Recorder.(*record.FakeRecorder)
	var events []string
drain:
	for {
		select {
		case evt := <-fakeRecorder.Events:
			events = append(events, evt)
		default:
			break drain
		}
	}
	for _, evt := range events {
		g.Expect(evt).NotTo(ContainSubstring("OpenBao"),
			"no OpenBao-related event must be emitted when the openbao finalizer is absent; got: %v", events)
	}
}

// ---------------------------------------------------------------------------
// Reconcile openbao-finalizer event-emission tests
// ---------------------------------------------------------------------------

// TestReconcile_TerminatingCR_EmitsFinalizingOpenBaoSecretsEvent verifies that
// a terminating CR with a live backup PushSecret emits
// "FinalizingOpenBaoSecrets" to announce cleanup before issuing Delete
func TestReconcile_TerminatingCR_EmitsFinalizingOpenBaoSecretsEvent(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	// Isolate the openbao path — no MariaDB finalizer means no MariaDB events.
	ks.Finalizers = []string{keystoneOpenBaoFinalizer}

	fernet := backupPushSecret("test-keystone-fernet-keys-backup")
	credential := backupPushSecret("test-keystone-credential-keys-backup")

	r := newTestReconciler(ks, fernet, credential)
	ctx := context.Background()
	_ = markKeystoneTerminating(t, r.Client, ks)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}
	_, err := r.Reconcile(ctx, req)
	g.Expect(err).NotTo(HaveOccurred())

	fakeRecorder := r.Recorder.(*record.FakeRecorder)
	var events []string
drain:
	for {
		select {
		case evt := <-fakeRecorder.Events:
			events = append(events, evt)
		default:
			break drain
		}
	}
	g.Expect(events).To(ContainElement(ContainSubstring("FinalizingOpenBaoSecrets")),
		"FinalizingOpenBaoSecrets event must be emitted when live backup PushSecrets exist; got: %v", events)
}

// TestReconcile_TerminatingCR_EmitsOpenBaoSecretsFinalizedEvent verifies that
// a brownfield terminating CR (no backup PushSecrets) emits only
// "OpenBaoSecretsFinalized" before releasing the openbao finalizer — the
// FinalizingOpenBaoSecrets sentinel is false because there is no live
// cleanup work to announce.
func TestReconcile_TerminatingCR_EmitsOpenBaoSecretsFinalizedEvent(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	// Isolate the openbao path — no MariaDB finalizer means no MariaDB events.
	ks.Finalizers = []string{keystoneOpenBaoFinalizer}

	r := newTestReconciler(ks)
	ctx := context.Background()
	_ = markKeystoneTerminating(t, r.Client, ks)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}
	result, err := r.Reconcile(ctx, req)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}),
		"brownfield terminating CR must release the openbao finalizer in a single pass")

	fakeRecorder := r.Recorder.(*record.FakeRecorder)
	var events []string
drain:
	for {
		select {
		case evt := <-fakeRecorder.Events:
			events = append(events, evt)
		default:
			break drain
		}
	}
	g.Expect(events).To(ContainElement(ContainSubstring("OpenBaoSecretsFinalized")),
		"OpenBaoSecretsFinalized event must be emitted after finalizeOpenBaoSecrets reports done; got: %v", events)
	g.Expect(events).NotTo(ContainElement(ContainSubstring("FinalizingOpenBaoSecrets")),
		"FinalizingOpenBaoSecrets must not be emitted for a brownfield CR with no backup PushSecrets")

	// Openbao finalizer must have been released — the CR is gone from the store.
	getErr := r.Get(ctx, req.NamespacedName, &keystonev1alpha1.Keystone{})
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
		"Keystone must be removed after the openbao finalizer is released")
}

// TestReconcile_TerminatingCR_NoDuplicateStartEventOnRequeue guards the
// hasLiveOpenBaoBackupPushSecrets sentinel: FinalizingOpenBaoSecrets is
// emitted exactly once per termination, suppressed on subsequent reconcile
// passes while the PushSecret is Terminating. A regression that re-emits the
// event on every requeue would flood the event stream in production
func TestReconcile_TerminatingCR_NoDuplicateStartEventOnRequeue(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	// Only the openbao finalizer so the CR stays alive across passes.
	ks.Finalizers = []string{keystoneOpenBaoFinalizer}

	// Pending-delete finalizer holds the PushSecret Terminating between
	// passes — matching ESO still purging the kv-v2 path when Reconcile
	// re-enters.
	fernet := pushSecretWithPendingDelete("test-keystone-fernet-keys-backup")

	r := newTestReconciler(ks, fernet)
	ctx := context.Background()
	_ = markKeystoneTerminating(t, r.Client, ks)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}}

	_, err := r.Reconcile(ctx, req)
	g.Expect(err).NotTo(HaveOccurred(), "first reconcile pass must succeed")
	_, err = r.Reconcile(ctx, req)
	g.Expect(err).NotTo(HaveOccurred(), "second reconcile pass must succeed")

	fakeRecorder := r.Recorder.(*record.FakeRecorder)
	var events []string
drain:
	for {
		select {
		case evt := <-fakeRecorder.Events:
			events = append(events, evt)
		default:
			break drain
		}
	}

	finalizingCount := 0
	for _, evt := range events {
		if strings.Contains(evt, "FinalizingOpenBaoSecrets") {
			finalizingCount++
		}
	}
	g.Expect(finalizingCount).To(Equal(1),
		"FinalizingOpenBaoSecrets must be emitted exactly once across both reconcile passes; got events: %v", events)
	g.Expect(events).NotTo(ContainElement(ContainSubstring("OpenBaoSecretsFinalized")),
		"OpenBaoSecretsFinalized must not be emitted while the PushSecret is still Terminating "+
			"(the finalizer has not been released yet); got events: %v", events)
}

// BenchmarkReconcile_FullReconcile measures ns/op for a full reconcile cycle
// with a fake client and all sub-resources pre-created. This establishes a
// baseline for comparing sequential vs parallel execution latency
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
	// Verify the result to catch setup issues early.
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
// when API calls have realistic round-trip times.
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
	// parallelization gains become visible in wall-clock time.
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
	// Verify the result to catch setup issues early.
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

// --- Gateway API availability probe (production-hardening) ---

// fakeRESTMapper implements meta.RESTMapper just far enough for the
// RESTMapping call made by gateway.IsGVKAvailable. Availability is driven by
// the keys in the "available" set.
type fakeRESTMapper struct {
	meta.RESTMapper
	available map[string]bool
}

func (f *fakeRESTMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	if f.available[gk.String()] {
		return &meta.RESTMapping{}, nil
	}
	return nil, &meta.NoKindMatchError{GroupKind: gk, SearchedVersions: versions}
}

func TestIsGatewayAPIAvailable_NilMapper_ReturnsFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(gateway.IsGVKAvailable(nil, httpRouteGVK)).To(BeFalse())
}

func TestIsGatewayAPIAvailable_CRDPresent_ReturnsTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	m := &fakeRESTMapper{available: map[string]bool{"HTTPRoute.gateway.networking.k8s.io": true}}
	g.Expect(gateway.IsGVKAvailable(m, httpRouteGVK)).To(BeTrue())
}

func TestIsGatewayAPIAvailable_CRDMissing_ReturnsFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	m := &fakeRESTMapper{available: map[string]bool{}}
	g.Expect(gateway.IsGVKAvailable(m, httpRouteGVK)).To(BeFalse())
}

// --- cert-manager availability probe (issue #475, DB-TLS Certificate lifecycle) ---

func TestIsCertManagerAvailable_NilMapper_ReturnsFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(gateway.IsGVKAvailable(nil, certificateGVK)).To(BeFalse())
}

func TestIsCertManagerAvailable_CRDPresent_ReturnsTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	m := &fakeRESTMapper{available: map[string]bool{"Certificate.cert-manager.io": true}}
	g.Expect(gateway.IsGVKAvailable(m, certificateGVK)).To(BeTrue())
}

func TestIsCertManagerAvailable_CRDMissing_ReturnsFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	m := &fakeRESTMapper{available: map[string]bool{}}
	g.Expect(gateway.IsGVKAvailable(m, certificateGVK)).To(BeFalse())
}

// ---------------------------------------------------------------------------
// reconcileDBConnectionSecret wiring tests
// ---------------------------------------------------------------------------

// TestReconcile_DBConnectionSecretCreatedBeforeConfigMap verifies that in a
// full reconcile the derived <name>-db-connection Secret is created before any
// ConfigMap. The derived Secret is consumed at runtime via the
// OS_DATABASE__CONNECTION env var; if a ConfigMap referencing the placeholder
// were rendered first, downstream pods could mount a ConfigMap whose companion
// Secret does not yet exist.
func TestReconcile_DBConnectionSecretCreatedBeforeConfigMap(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	derivedSecretName := ks.Name + "-db-connection"

	s := testScheme()
	objs := append(
		[]runtime.Object{
			ks,
			testDBCredentialsSecret(),
			testAdminCredentialsSecret(),
			testFernetKeysSecret(),
			testCredentialKeysSecret(),
		},
		testReadyExternalSecrets()...,
	)
	cb := fake.NewClientBuilder().WithScheme(s)
	for _, obj := range objs {
		cb = cb.WithRuntimeObjects(obj)
	}
	cb = cb.WithStatusSubresource(&keystonev1alpha1.Keystone{}, &esov1.ExternalSecret{})
	cb = cb.WithIndex(&keystonev1alpha1.KeystoneIdentityBackend{}, IdentityBackendKeystoneRefIndexKey, identityBackendKeystoneRefExtractor)

	type createEvent struct {
		kind string
		name string
	}
	var (
		createsMu sync.Mutex
		creates   []createEvent
	)
	cb = cb.WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			switch obj.(type) {
			case *corev1.Secret:
				createsMu.Lock()
				creates = append(creates, createEvent{kind: "Secret", name: obj.GetName()})
				createsMu.Unlock()
			case *corev1.ConfigMap:
				createsMu.Lock()
				creates = append(creates, createEvent{kind: "ConfigMap", name: obj.GetName()})
				createsMu.Unlock()
			}
			return c.Create(ctx, obj, opts...)
		},
	})

	r := &KeystoneReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace},
	})
	g.Expect(err).NotTo(HaveOccurred())

	derivedIdx, firstConfigMapIdx := -1, -1
	for i, ev := range creates {
		if ev.kind == "Secret" && ev.name == derivedSecretName && derivedIdx == -1 {
			derivedIdx = i
		}
		if ev.kind == "ConfigMap" && firstConfigMapIdx == -1 {
			firstConfigMapIdx = i
		}
	}
	g.Expect(derivedIdx).NotTo(Equal(-1),
		"derived db-connection Secret must be created during reconcile")
	g.Expect(firstConfigMapIdx).NotTo(Equal(-1),
		"at least one ConfigMap must be created during reconcile")
	g.Expect(derivedIdx < firstConfigMapIdx).To(BeTrue(),
		"derived Secret must be created before any ConfigMap; got derived at %d, first ConfigMap at %d",
		derivedIdx, firstConfigMapIdx)
}

// TestReconcile_DBConnectionSecret_SecretsReadyFalseWhenUpstreamMissing
// verifies that when reconcileDBConnectionSecret observes the upstream DB
// credentials Secret as missing, SecretsReady=False with reason
// WaitingForDBCredentials is persisted on the Keystone CR status and the
// reconcile chain short-circuits before ConfigMap creation.
//
// To isolate the reconcileDBConnectionSecret path from reconcileSecrets (which
// also checks the upstream Secret), a Get interceptor lets the first Get on
// the upstream Secret succeed (satisfying reconcileSecrets.IsSecretReady) and
// returns NotFound for subsequent Gets (failing
// reconcileDBConnectionSecret.GetSecretValue). This simulates a race where
// the Secret disappears between the two sub-reconcilers.
func TestReconcile_DBConnectionSecret_SecretsReadyFalseWhenUpstreamMissing(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	upstreamKey := client.ObjectKey{Namespace: ks.Namespace, Name: ks.Spec.Database.SecretRef.Name}

	s := testScheme()
	objs := append(
		[]runtime.Object{ks, testDBCredentialsSecret(), testAdminCredentialsSecret()},
		testReadyExternalSecrets()...,
	)
	cb := fake.NewClientBuilder().WithScheme(s)
	for _, obj := range objs {
		cb = cb.WithRuntimeObjects(obj)
	}
	cb = cb.WithStatusSubresource(&keystonev1alpha1.Keystone{}, &esov1.ExternalSecret{})

	var upstreamGets int
	cb = cb.WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); ok && key == upstreamKey {
				upstreamGets++
				if upstreamGets > 1 {
					return apierrors.NewNotFound(corev1.Resource("secrets"), key.Name)
				}
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	r := &KeystoneReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretPolling),
		"missing upstream Secret must trigger the secret-polling requeue")

	var updated keystonev1alpha1.Keystone
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace}, &updated)).To(Succeed())

	secretsCond := meta.FindStatusCondition(updated.Status.Conditions, "SecretsReady")
	g.Expect(secretsCond).NotTo(BeNil(), "SecretsReady condition must be persisted to status")
	g.Expect(secretsCond.Status).To(Equal(metav1.ConditionFalse),
		"SecretsReady must be False when reconcileDBConnectionSecret sees the upstream Secret as missing")
	g.Expect(secretsCond.Reason).To(Equal("WaitingForDBCredentials"),
		"SecretsReady reason must be WaitingForDBCredentials (set by reconcileDBConnectionSecret)")

	var cms corev1.ConfigMapList
	g.Expect(r.Client.List(context.Background(), &cms, client.InNamespace(ks.Namespace))).To(Succeed())
	for _, cm := range cms.Items {
		g.Expect(cm.Name).NotTo(HavePrefix(ks.Name+"-config"),
			"ConfigMap must not be created when reconcileDBConnectionSecret short-circuits")
	}

	derived := &corev1.Secret{}
	getErr := r.Get(context.Background(), client.ObjectKey{Namespace: ks.Namespace, Name: ks.Name + "-db-connection"}, derived)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
		"derived db-connection Secret must not be created when upstream is missing")
}

// TestSecretToKeystoneMapper_DerivedDBConnectionSecret verifies that the
// mapper enqueues a reconcile request for the owning Keystone when a change
// event arrives on the derived <name>-db-connection Secret, via its
// ownerReference. The name pattern is operator-chosen and
// not referenced by spec.database.secretRef, so the enqueue path MUST be
// ownership-based.
func TestSecretToKeystoneMapper_DerivedDBConnectionSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()

	c := newMapperFakeClient(ks)
	mapper := secretToKeystoneMapper(c)

	derived := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            ks.Name + "-db-connection",
			Namespace:       ks.Namespace,
			OwnerReferences: []metav1.OwnerReference{keystoneOwnerRef(ks)},
		},
	}
	reqs := mapper(context.Background(), derived)
	g.Expect(reqs).To(HaveLen(1),
		"derived db-connection Secret must enqueue exactly one reconcile request")
	g.Expect(reqs[0].NamespacedName.Name).To(Equal(ks.Name))
	g.Expect(reqs[0].NamespacedName.Namespace).To(Equal(ks.Namespace))
}

// TestKeystoneSecretIndexer_ExtractsBothReferencedSecretNames verifies that
// keystoneSecretNameExtractor returns both spec.database.secretRef.name and
// spec.bootstrap.adminPasswordSecretRef.name when they hold distinct values,
// so the field indexer registered under KeystoneSecretNameIndexKey contains
// one entry per referenced Secret name.
func TestKeystoneSecretIndexer_ExtractsBothReferencedSecretNames(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	ks.Spec.Database.SecretRef.Name = "keystone-db"
	ks.Spec.Bootstrap.AdminPasswordSecretRef.Name = "keystone-admin"

	got := keystoneSecretNameExtractor(ks)

	g.Expect(got).To(ConsistOf("keystone-db", "keystone-admin"),
		"extractor must return both referenced Secret names exactly once")
}

// TestKeystoneSecretIndexer_DeduplicatesIdenticalNames verifies that when both
// SecretRef fields hold the same Secret name, the extractor returns it only
// once so the field indexer does not store duplicate entries for the same CR
// under the same key.
func TestKeystoneSecretIndexer_DeduplicatesIdenticalNames(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	ks.Spec.Database.SecretRef.Name = "shared-secret"
	ks.Spec.Bootstrap.AdminPasswordSecretRef.Name = "shared-secret"

	got := keystoneSecretNameExtractor(ks)

	g.Expect(got).To(Equal([]string{"shared-secret"}),
		"extractor must deduplicate identical Secret names")
}

// TestKeystoneSecretIndexer_SkipsEmptyNames verifies that the extractor omits
// empty SecretRef.Name values so unset/optional fields do not pollute the
// field index with empty-string keys.
func TestKeystoneSecretIndexer_SkipsEmptyNames(t *testing.T) {
	t.Run("admin name empty", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ks := testKeystone()
		ks.Spec.Database.SecretRef.Name = "keystone-db"
		ks.Spec.Bootstrap.AdminPasswordSecretRef.Name = ""

		got := keystoneSecretNameExtractor(ks)

		g.Expect(got).To(Equal([]string{"keystone-db"}),
			"empty admin name must be filtered out")
	})

	t.Run("database name empty", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ks := testKeystone()
		ks.Spec.Database.SecretRef.Name = ""
		ks.Spec.Bootstrap.AdminPasswordSecretRef.Name = "keystone-admin"

		got := keystoneSecretNameExtractor(ks)

		g.Expect(got).To(Equal([]string{"keystone-admin"}),
			"empty database name must be filtered out")
	})

	t.Run("both empty", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ks := testKeystone()
		ks.Spec.Database.SecretRef.Name = ""
		ks.Spec.Bootstrap.AdminPasswordSecretRef.Name = ""

		got := keystoneSecretNameExtractor(ks)

		g.Expect(got).To(BeEmpty(),
			"when both names are empty the extractor must return an empty result")
	})
}

// --- secretToKeystoneMapper indexed-lookup behaviour ---

// listCall captures the shape of a single List invocation so tests can assert
// on the ListOptions that secretToKeystoneMapper issued.
type listCall struct {
	options client.ListOptions
}

// recordingListInterceptor returns an interceptor.Funcs that records each
// List call's ApplyToList view of the caller's ListOptions and delegates to
// the wrapped client. A nil listErr lets the real List run; a non-nil listErr
// is returned instead so the mapper's error branch can be exercised
func recordingListInterceptor(listErr error) (*[]listCall, interceptor.Funcs) {
	calls := &[]listCall{}
	var mu sync.Mutex
	return calls, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			var lo client.ListOptions
			for _, o := range opts {
				o.ApplyToList(&lo)
			}
			mu.Lock()
			*calls = append(*calls, listCall{options: lo})
			mu.Unlock()
			if listErr != nil {
				return listErr
			}
			return c.List(ctx, list, opts...)
		},
	}
}

// TestSecretToKeystoneMapper_UsesIndexedLookup verifies that every List call
// issued by the refactored mapper carries the KeystoneSecretNameIndexKey
// field selector set to the Secret's name — i.e. the mapper no longer pulls
// every Keystone in the namespace.
func TestSecretToKeystoneMapper_UsesIndexedLookup(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()

	calls, ifuncs := recordingListInterceptor(nil)
	c := newMapperFakeClientBuilder(ks).WithInterceptorFuncs(ifuncs).Build()
	mapper := secretToKeystoneMapper(c)

	dbSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-db", Namespace: "default"},
	}
	reqs := mapper(context.Background(), dbSecret)

	g.Expect(reqs).To(HaveLen(1),
		"referenced Secret must enqueue its Keystone via the field indexer")
	g.Expect(reqs[0].NamespacedName.Name).To(Equal(ks.Name))

	g.Expect(*calls).To(HaveLen(1), "mapper must perform exactly one List call")
	fs := (*calls)[0].options.FieldSelector
	g.Expect(fs).ToNot(BeNil(), "List must use a FieldSelector (indexed lookup)")
	g.Expect(fs.String()).To(ContainSubstring(KeystoneSecretNameIndexKey+"=keystone-db"),
		"List must select on the KeystoneSecretNameIndexKey for the Secret's name")
}

// TestSecretToKeystoneMapper_IndexedLookupScopedToNamespace verifies that the
// indexed List is scoped to the Secret's namespace so a Keystone referencing
// the same Secret name in a different namespace is not enqueued and the List
// does not fan out cluster-wide.
func TestSecretToKeystoneMapper_IndexedLookupScopedToNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	// Two Keystones in different namespaces referencing a Secret of the
	// same name. Only the one in the event's namespace must be enqueued.
	ksA := testKeystone()
	ksA.Name = "keystone-a"
	ksA.UID = "uid-a"
	ksA.Namespace = "ns-a"
	ksA.Spec.Database.SecretRef.Name = "shared-secret"

	ksB := testKeystone()
	ksB.Name = "keystone-b"
	ksB.UID = "uid-b"
	ksB.Namespace = "ns-b"
	ksB.Spec.Database.SecretRef.Name = "shared-secret"

	calls, ifuncs := recordingListInterceptor(nil)
	c := newMapperFakeClientBuilder(ksA, ksB).WithInterceptorFuncs(ifuncs).Build()
	mapper := secretToKeystoneMapper(c)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-secret", Namespace: "ns-a"},
	}
	reqs := mapper(context.Background(), secret)

	g.Expect(reqs).To(HaveLen(1),
		"indexed lookup must not cross namespace boundaries")
	g.Expect(reqs[0].NamespacedName).To(Equal(types.NamespacedName{
		Namespace: "ns-a",
		Name:      "keystone-a",
	}))

	g.Expect(*calls).To(HaveLen(1))
	g.Expect((*calls)[0].options.Namespace).To(Equal("ns-a"),
		"List must be scoped to the event's namespace")
}

// TestSecretToKeystoneMapper_IndexedLookupErrorLoggedAndSwallowed verifies
// that an error from the indexed List is swallowed — the mapper does not
// return the error and the owner-ref path still contributes results so that
// rotation staging Secrets remain wired to their Keystone during a transient
// indexer failure.
func TestSecretToKeystoneMapper_IndexedLookupErrorLoggedAndSwallowed(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()

	injected := errors.New("simulated indexer failure")
	_, ifuncs := recordingListInterceptor(injected)
	c := newMapperFakeClientBuilder(ks).WithInterceptorFuncs(ifuncs).Build()
	mapper := secretToKeystoneMapper(c)

	// Secret name does not match any SecretRef, but it owns a Keystone
	// via OwnerReference — the owner-ref path must still fire.
	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "unrelated-name-for-indexer",
			Namespace:       ks.Namespace,
			OwnerReferences: []metav1.OwnerReference{keystoneOwnerRef(ks)},
		},
	}

	// Must not panic, must not return the List error.
	var reqs []reconcile.Request
	g.Expect(func() { reqs = mapper(context.Background(), staging) }).ToNot(Panic())
	g.Expect(reqs).To(HaveLen(1),
		"owner-ref path must still enqueue the Keystone when the indexed List fails")
	g.Expect(reqs[0].NamespacedName.Name).To(Equal(ks.Name))
}

// TestSecretToKeystoneMapper_NoUnfilteredListCall pins the invariant that
// every List the mapper issues carries the KeystoneSecretNameIndexKey field
// selector; a regression that dropped the MatchingFields option would revert
// to the pre-existing unfiltered namespace-scoped List and re-introduce the
// API server amplification this feature was designed to eliminate
func TestSecretToKeystoneMapper_NoUnfilteredListCall(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()

	calls, ifuncs := recordingListInterceptor(nil)
	c := newMapperFakeClientBuilder(ks).WithInterceptorFuncs(ifuncs).Build()
	mapper := secretToKeystoneMapper(c)

	for _, name := range []string{"keystone-db", "keystone-admin", "unrelated"} {
		_ = mapper(context.Background(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		})
	}

	g.Expect(*calls).ToNot(BeEmpty(), "mapper should issue at least one List")
	for i, call := range *calls {
		fs := call.options.FieldSelector
		g.Expect(fs).ToNot(BeNil(),
			"List #%d must carry a FieldSelector; unfiltered Lists are a regression", i)
		g.Expect(fs.String()).To(ContainSubstring(KeystoneSecretNameIndexKey+"="),
			"List #%d must select on %s", i, KeystoneSecretNameIndexKey)
	}
}

// TestSecretToKeystoneMapper_OwnerRefPathDoesNotList verifies that the owner-
// reference scan does not issue a List of its own: it operates on the
// Secret's in-memory metadata. When the indexed List fails, the mapper still
// emits requests for every Keystone ownerRef on the event — with zero
// additional API calls.
func TestSecretToKeystoneMapper_OwnerRefPathDoesNotList(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()

	// Fail the indexed List so any List at all is undesirable.
	calls, ifuncs := recordingListInterceptor(errors.New("boom"))
	c := newMapperFakeClientBuilder(ks).WithInterceptorFuncs(ifuncs).Build()
	mapper := secretToKeystoneMapper(c)

	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "rotation-staging",
			Namespace:       ks.Namespace,
			OwnerReferences: []metav1.OwnerReference{keystoneOwnerRef(ks)},
		},
	}
	reqs := mapper(context.Background(), staging)

	g.Expect(reqs).To(HaveLen(1),
		"owner-ref path must enqueue the Keystone without needing a second List")
	g.Expect(reqs[0].NamespacedName.Name).To(Equal(ks.Name))

	// The indexed List is expected (and failed); the owner-ref path must
	// not contribute its own List on top.
	g.Expect(*calls).To(HaveLen(1),
		"owner-ref path must not issue an additional List call")
}

// TestSecretToKeystoneMapper_DeduplicatesIndexAndOwnerPaths verifies that a
// Secret simultaneously referenced by name (index hit) and owner-referenced
// (owner-ref hit) enqueues the target Keystone exactly once — the dedup
// invariant that keeps workqueue traffic proportional to the set of distinct
// referencing CRs rather than to the number of relationships.
func TestSecretToKeystoneMapper_DeduplicatesIndexAndOwnerPaths(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()

	c := newMapperFakeClient(ks)
	mapper := secretToKeystoneMapper(c)

	// Secret name matches ks.Spec.Database.SecretRef.Name AND carries an
	// ownerRef pointing at ks. Both paths must resolve to the same request.
	overlap := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "keystone-db",
			Namespace:       ks.Namespace,
			OwnerReferences: []metav1.OwnerReference{keystoneOwnerRef(ks)},
		},
	}
	reqs := mapper(context.Background(), overlap)

	g.Expect(reqs).To(HaveLen(1),
		"index + owner-ref must dedupe to a single reconcile request")
	g.Expect(reqs[0].NamespacedName).To(Equal(types.NamespacedName{
		Namespace: ks.Namespace,
		Name:      ks.Name,
	}))
}

// TestSecretToKeystoneMapper_MultipleReferencingKeystonesAllEnqueued verifies
// that when several Keystone CRs in the same namespace reference the same
// Secret name, every one of them is enqueued on a single Secret event — the
// field indexer returns the full set, not just the first match.
func TestSecretToKeystoneMapper_MultipleReferencingKeystonesAllEnqueued(t *testing.T) {
	g := NewGomegaWithT(t)

	ks1 := testKeystone()
	ks1.Name = "keystone-one"
	ks1.UID = "uid-one"
	ks1.Spec.Database.SecretRef.Name = "shared-secret"

	ks2 := testKeystone()
	ks2.Name = "keystone-two"
	ks2.UID = "uid-two"
	ks2.Spec.Database.SecretRef.Name = "shared-secret"

	c := newMapperFakeClient(ks1, ks2)
	mapper := secretToKeystoneMapper(c)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-secret", Namespace: "default"},
	}
	reqs := mapper(context.Background(), secret)

	g.Expect(reqs).To(HaveLen(2),
		"both referencing Keystones must be enqueued")
	names := []string{reqs[0].Name, reqs[1].Name}
	g.Expect(names).To(ConsistOf("keystone-one", "keystone-two"))
}

// TestSecretToKeystoneMapper_OwnerRefGroupMatchIgnoresVersion verifies that
// the owner-ref fallback matches any version within
// keystonev1alpha1.GroupVersion.Group, not just an exact APIVersion string.
// Secrets persisted with an older APIVersion (e.g. v1alpha1) must continue to
// resolve to their owning Keystone after a future API version bump
// (e.g. v1beta1) — pinned per review #1.
func TestSecretToKeystoneMapper_OwnerRefGroupMatchIgnoresVersion(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()

	c := newMapperFakeClient(ks)
	mapper := secretToKeystoneMapper(c)

	// Owner-ref with a different (future) version in the same group.
	// The mapper must still enqueue the owning Keystone.
	futureVersionOwner := metav1.OwnerReference{
		APIVersion: keystonev1alpha1.GroupVersion.Group + "/v1beta1",
		Kind:       "Keystone",
		Name:       ks.Name,
		UID:        ks.UID,
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "rotation-staging",
			Namespace:       ks.Namespace,
			OwnerReferences: []metav1.OwnerReference{futureVersionOwner},
		},
	}

	reqs := mapper(context.Background(), secret)
	g.Expect(reqs).To(HaveLen(1),
		"owner-ref with same group but different version must still match")
}

// TestSecretToKeystoneMapper_OwnerRefDifferentGroupIgnored verifies that the
// owner-ref fallback ignores references with Kind=="Keystone" whose
// APIVersion is in a different API group (e.g. a foreign Keystone CRD). Only
// the keystone.openstack.c5c3.io group is considered (review #1).
func TestSecretToKeystoneMapper_OwnerRefDifferentGroupIgnored(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()

	c := newMapperFakeClient(ks)
	mapper := secretToKeystoneMapper(c)

	foreignOwner := metav1.OwnerReference{
		APIVersion: "other.example.com/v1alpha1",
		Kind:       "Keystone",
		Name:       ks.Name,
		UID:        ks.UID,
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "not-owned-by-us",
			Namespace:       ks.Namespace,
			OwnerReferences: []metav1.OwnerReference{foreignOwner},
		},
	}

	reqs := mapper(context.Background(), secret)
	g.Expect(reqs).To(BeEmpty(),
		"owner-ref in a foreign API group must not enqueue our Keystone")
}

// TestSecretToKeystoneMapper_OwnerRefMalformedAPIVersionIgnored verifies that
// an OwnerReference whose APIVersion cannot be parsed as GroupVersion is
// silently skipped rather than panicking or enqueuing spuriously
// (review #1).
func TestSecretToKeystoneMapper_OwnerRefMalformedAPIVersionIgnored(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()

	c := newMapperFakeClient(ks)
	mapper := secretToKeystoneMapper(c)

	malformedOwner := metav1.OwnerReference{
		APIVersion: "a/b/c/d", // schema.ParseGroupVersion rejects more than one '/'
		Kind:       "Keystone",
		Name:       ks.Name,
		UID:        ks.UID,
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "malformed-owner",
			Namespace:       ks.Namespace,
			OwnerReferences: []metav1.OwnerReference{malformedOwner},
		},
	}

	reqs := mapper(context.Background(), secret)
	g.Expect(reqs).To(BeEmpty(),
		"malformed APIVersion must be skipped without panic or spurious enqueue")
}

// TestSecretToKeystoneMapper_OwnerRefStaleKeystoneSkipped verifies that an
// OwnerReference pointing at a Keystone that no longer exists in the cache
// is dropped from the enqueue set — the cached Get returning NotFound is the
// signal that the owner-ref is stale or spurious. Prevents enqueuing reconcile
// work for non-existent CRs (review #1).
func TestSecretToKeystoneMapper_OwnerRefStaleKeystoneSkipped(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()

	// Build the fake client WITHOUT seeding ks, so the cached Get returns
	// NotFound for the owner-ref target.
	c := newMapperFakeClient() // no objects seeded
	mapper := secretToKeystoneMapper(c)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "orphaned-staging",
			Namespace:       ks.Namespace,
			OwnerReferences: []metav1.OwnerReference{keystoneOwnerRef(ks)},
		},
	}

	reqs := mapper(context.Background(), secret)
	g.Expect(reqs).To(BeEmpty(),
		"owner-ref pointing at a non-existent Keystone must be skipped")
}

// TestSecretToKeystoneMapper_OwnerRefTransientGetErrorEnqueues verifies that a
// non-NotFound error from the cached Get during the owner-ref path does NOT
// cause the mapper to drop the ref. The guard's purpose is to eliminate
// clearly stale references (NotFound); every other outcome must fall through
// to enqueue so a transient cache blip cannot swallow a legitimate event
// (review #1).
func TestSecretToKeystoneMapper_OwnerRefTransientGetErrorEnqueues(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()

	// Interceptor injects a non-NotFound error on Get so the fallback path
	// is exercised. The Keystone is still seeded in the fake client, but the
	// interceptor short-circuits Get before it reaches the store.
	ifuncs := interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return errors.New("simulated transient cache error")
		},
	}
	c := newMapperFakeClientBuilder(ks).WithInterceptorFuncs(ifuncs).Build()
	mapper := secretToKeystoneMapper(c)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "rotation-staging",
			Namespace:       ks.Namespace,
			OwnerReferences: []metav1.OwnerReference{keystoneOwnerRef(ks)},
		},
	}

	reqs := mapper(context.Background(), secret)
	g.Expect(reqs).To(HaveLen(1),
		"transient (non-NotFound) Get errors must not drop the owner-ref")
	g.Expect(reqs[0].NamespacedName.Name).To(Equal(ks.Name))
}

// --- pushSecretToKeystoneMapper behaviour ---

// pushSecretObj builds a minimal PushSecret client.Object with the given name
// and namespace. The mapper only inspects name/namespace metadata — no spec or
// status is needed to exercise its enqueue contract.
func pushSecretObj(name, namespace string) client.Object {
	return &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

// TestPushSecretToKeystoneMapper_FernetBackupEnqueuesKeystone verifies that a
// PushSecret whose name matches the fernet backup for a Keystone in the same
// namespace enqueues exactly one reconcile.Request for that Keystone
func TestPushSecretToKeystoneMapper_FernetBackupEnqueuesKeystone(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()

	c := newMapperFakeClient(ks)
	mapper := pushSecretToKeystoneMapper(c)

	ps := pushSecretObj(fmt.Sprintf("%s-fernet-keys-backup", ks.Name), ks.Namespace)
	reqs := mapper(context.Background(), ps)

	g.Expect(reqs).To(HaveLen(1),
		"fernet backup PushSecret must enqueue its owning Keystone")
	g.Expect(reqs[0].NamespacedName).To(Equal(types.NamespacedName{
		Namespace: ks.Namespace,
		Name:      ks.Name,
	}))
}

// TestPushSecretToKeystoneMapper_CredentialBackupEnqueuesKeystone verifies the
// same contract as the fernet case for the credential backup — both entries of
// openBaoBackupPushSecretNames must be observed by the mapper
func TestPushSecretToKeystoneMapper_CredentialBackupEnqueuesKeystone(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()

	c := newMapperFakeClient(ks)
	mapper := pushSecretToKeystoneMapper(c)

	ps := pushSecretObj(fmt.Sprintf("%s-credential-keys-backup", ks.Name), ks.Namespace)
	reqs := mapper(context.Background(), ps)

	g.Expect(reqs).To(HaveLen(1),
		"credential backup PushSecret must enqueue its owning Keystone")
	g.Expect(reqs[0].NamespacedName).To(Equal(types.NamespacedName{
		Namespace: ks.Namespace,
		Name:      ks.Name,
	}))
}

// TestPushSecretToKeystoneMapper_UnrelatedNameReturnsNil verifies that a
// PushSecret whose name matches no openBaoBackupPushSecretNames entry for any
// Keystone in its namespace is dropped by the mapper.
func TestPushSecretToKeystoneMapper_UnrelatedNameReturnsNil(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()

	c := newMapperFakeClient(ks)
	mapper := pushSecretToKeystoneMapper(c)

	ps := pushSecretObj("some-other-pushsecret", ks.Namespace)
	reqs := mapper(context.Background(), ps)

	g.Expect(reqs).To(BeNil(),
		"non-backup PushSecret name must yield no reconcile.Request")
}

// TestPushSecretToKeystoneMapper_MultipleKeystonesOnlyMatchingCREnqueued
// verifies the shared-namespace no-op dedup contract: in a namespace with two
// Keystone CRs, a backup PushSecret for one CR must not wake the other
func TestPushSecretToKeystoneMapper_MultipleKeystonesOnlyMatchingCREnqueued(t *testing.T) {
	g := NewGomegaWithT(t)

	ksA := testKeystone()
	ksA.Name = "ks-a"
	ksA.UID = "uid-a"

	ksB := testKeystone()
	ksB.Name = "ks-b"
	ksB.UID = "uid-b"

	c := newMapperFakeClient(ksA, ksB)
	mapper := pushSecretToKeystoneMapper(c)

	ps := pushSecretObj(fmt.Sprintf("%s-fernet-keys-backup", ksA.Name), ksA.Namespace)
	reqs := mapper(context.Background(), ps)

	g.Expect(reqs).To(HaveLen(1),
		"only the Keystone whose backup pattern matches must be enqueued")
	g.Expect(reqs[0].NamespacedName).To(Equal(types.NamespacedName{
		Namespace: ksA.Namespace,
		Name:      ksA.Name,
	}))
}

// TestPushSecretToKeystoneMapper_NoKeystonesInNamespaceReturnsNil verifies
// the empty-list path: when the event's (non-empty) namespace contains no
// Keystone CR, the mapper returns nothing without erroring. PushSecret is a
// namespaced resource, so the empty-namespace case is precluded by the
// apiserver and is intentionally not covered here.
func TestPushSecretToKeystoneMapper_NoKeystonesInNamespaceReturnsNil(t *testing.T) {
	g := NewGomegaWithT(t)

	c := newMapperFakeClient()
	mapper := pushSecretToKeystoneMapper(c)

	ps := pushSecretObj("anything-backup", "default")
	reqs := mapper(context.Background(), ps)

	g.Expect(reqs).To(BeEmpty(),
		"empty Keystone list in namespace must produce no reconcile.Request")
}

// TestPushSecretToKeystoneMapper_CrossNamespaceReturnsNil verifies that the
// Keystone List is namespace-scoped — a PushSecret in ns-b must not wake a
// Keystone that only exists in ns-a, even if the PushSecret's name matches
// that Keystone's backup pattern. The recording interceptor pins that the
// List carries client.InNamespace(ps.Namespace) and never fans out
// cluster-wide.
func TestPushSecretToKeystoneMapper_CrossNamespaceReturnsNil(t *testing.T) {
	g := NewGomegaWithT(t)

	ks := testKeystone()
	ks.Namespace = "ns-a"

	calls, ifuncs := recordingListInterceptor(nil)
	c := newMapperFakeClientBuilder(ks).WithInterceptorFuncs(ifuncs).Build()
	mapper := pushSecretToKeystoneMapper(c)

	ps := pushSecretObj(fmt.Sprintf("%s-fernet-keys-backup", ks.Name), "ns-b")
	reqs := mapper(context.Background(), ps)

	g.Expect(reqs).To(BeEmpty(),
		"namespace-scoped List must not cross namespaces")

	g.Expect(*calls).To(HaveLen(1),
		"mapper must issue exactly one List call")
	g.Expect((*calls)[0].options.Namespace).To(Equal("ns-b"),
		"List must be scoped to the PushSecret's namespace, never cluster-wide")
}

// TestPushSecretToKeystoneMapper_ListErrorIsSwallowed verifies that a List
// error is logged and the mapper returns nil instead of propagating — matching
// the log-and-swallow contract of secretToKeystoneMapper and the
// handler.MapFunc signature which has no error return.
func TestPushSecretToKeystoneMapper_ListErrorIsSwallowed(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()

	injected := errors.New("boom")
	calls, ifuncs := recordingListInterceptor(injected)
	c := newMapperFakeClientBuilder(ks).WithInterceptorFuncs(ifuncs).Build()
	mapper := pushSecretToKeystoneMapper(c)

	ps := pushSecretObj(fmt.Sprintf("%s-fernet-keys-backup", ks.Name), ks.Namespace)

	var reqs []reconcile.Request
	g.Expect(func() { reqs = mapper(context.Background(), ps) }).ToNot(Panic())
	g.Expect(reqs).To(BeNil(),
		"List error must be swallowed: mapper returns nil per MapFunc contract")
	g.Expect(*calls).To(HaveLen(1),
		"exactly one List must be attempted before the error is swallowed")
}

// --- pushSecretRelevantChangePredicate behaviour ---

// pushSecretWithMeta builds a PushSecret carrying the supplied metadata fields
// relevant to the predicate: Generation, Finalizers, and DeletionTimestamp.
// Name/Namespace are fixed since the predicate is purely metadata-driven and
// name-level filtering belongs to the mapper.
func pushSecretWithMeta(generation int64, finalizers []string, deletionTS *metav1.Time) *esov1alpha1.PushSecret {
	ps := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-ks-fernet-keys-backup",
			Namespace:  "default",
			Generation: generation,
			Finalizers: append([]string(nil), finalizers...),
		},
	}
	if deletionTS != nil {
		dt := *deletionTS
		ps.DeletionTimestamp = &dt
	}
	return ps
}

// TestPushSecretPredicate_FinalizerAddAdmitted verifies that gaining a
// finalizer (ESO installing esoPushSecretFinalizer on first sync — the
// Pass-0 adoption signal) passes the predicate so Keystone is re-reconciled
// and can progress past WaitingForESOAdoption.
func TestPushSecretPredicate_FinalizerAddAdmitted(t *testing.T) {
	g := NewGomegaWithT(t)

	oldObj := pushSecretWithMeta(1, nil, nil)
	newObj := pushSecretWithMeta(1, []string{esoPushSecretFinalizer}, nil)

	admitted := pushSecretRelevantChangePredicate.Update(event.UpdateEvent{
		ObjectOld: oldObj,
		ObjectNew: newObj,
	})

	g.Expect(admitted).To(BeTrue(),
		"finalizer add (ESO adoption signal) must pass the predicate")
}

// TestPushSecretPredicate_FinalizerRemoveAdmitted verifies that losing a
// finalizer (ESO dropping its cleanup finalizer after purging the target
// Secret — the Pass-1 unblock signal) passes the predicate so Keystone can
// remove its own openbao finalizer without waiting for the 15s periodic
// requeue. Uses the package-level esoCleanupFinalizer constant declared in
// reconcile_secrets.go as the single source of truth shared with the
// integration tests.
func TestPushSecretPredicate_FinalizerRemoveAdmitted(t *testing.T) {
	g := NewGomegaWithT(t)

	oldObj := pushSecretWithMeta(1, []string{esoCleanupFinalizer}, nil)
	newObj := pushSecretWithMeta(1, nil, nil)

	admitted := pushSecretRelevantChangePredicate.Update(event.UpdateEvent{
		ObjectOld: oldObj,
		ObjectNew: newObj,
	})

	g.Expect(admitted).To(BeTrue(),
		"finalizer removal (ESO cleanup complete) must pass the predicate")
}

// TestPushSecretPredicate_DeletionTimestampSetAdmitted verifies that a
// PushSecret transitioning from live to Terminating (DeletionTimestamp first
// set) passes the predicate. This is the edge that kicks Pass-1 of the
// Keystone delete path.
func TestPushSecretPredicate_DeletionTimestampSetAdmitted(t *testing.T) {
	g := NewGomegaWithT(t)

	now := metav1.Now()
	oldObj := pushSecretWithMeta(1, []string{esoPushSecretFinalizer}, nil)
	newObj := pushSecretWithMeta(1, []string{esoPushSecretFinalizer}, &now)

	admitted := pushSecretRelevantChangePredicate.Update(event.UpdateEvent{
		ObjectOld: oldObj,
		ObjectNew: newObj,
	})

	g.Expect(admitted).To(BeTrue(),
		"DeletionTimestamp first set (live → Terminating) must pass the predicate")
}

// TestPushSecretPredicate_StatusOnlyUpdateSuppressed verifies the core
// workqueue-quieting contract: an Update whose finalizers, DeletionTimestamp
// presence, and Generation are unchanged (e.g. ESO bumping
// status.syncedResourceVersion on every successful sync) must NOT wake the
// Keystone reconciler. Without this filter the Keystone workqueue would
// receive one wake-up per ESO sync tick per owned PushSecret
func TestPushSecretPredicate_StatusOnlyUpdateSuppressed(t *testing.T) {
	g := NewGomegaWithT(t)

	finalizers := []string{esoPushSecretFinalizer}

	oldObj := pushSecretWithMeta(3, finalizers, nil)
	oldObj.ResourceVersion = "100"

	newObj := pushSecretWithMeta(3, finalizers, nil)
	// Simulate a status-only ESO tick: ResourceVersion bumped but nothing the
	// predicate keys on has changed.
	newObj.ResourceVersion = "101"

	admitted := pushSecretRelevantChangePredicate.Update(event.UpdateEvent{
		ObjectOld: oldObj,
		ObjectNew: newObj,
	})

	g.Expect(admitted).To(BeFalse(),
		"status-only update (no finalizer/DT/generation change) must be suppressed")
}

// TestPushSecretPredicate_CreateAndDeleteAlwaysAdmitted verifies that Create,
// Delete, and Generic events are admitted unconditionally — including for a
// PushSecret whose name does not match any Keystone backup pattern. The
// predicate is deliberately name-agnostic; name-level filtering is the
// mapper's responsibility and belongs on the Watches() MapFunc, not here
func TestPushSecretPredicate_CreateAndDeleteAlwaysAdmitted(t *testing.T) {
	g := NewGomegaWithT(t)

	unrelated := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unrelated-push-secret",
			Namespace: "default",
		},
	}

	g.Expect(pushSecretRelevantChangePredicate.Create(event.CreateEvent{
		Object: unrelated,
	})).To(BeTrue(),
		"CreateEvent must pass unconditionally; name filtering is the mapper's job")

	g.Expect(pushSecretRelevantChangePredicate.Delete(event.DeleteEvent{
		Object: unrelated,
	})).To(BeTrue(),
		"DeleteEvent must pass unconditionally; name filtering is the mapper's job")

	g.Expect(pushSecretRelevantChangePredicate.Generic(event.GenericEvent{
		Object: unrelated,
	})).To(BeTrue(),
		"GenericEvent must pass unconditionally; name filtering is the mapper's job")
}

// TestPushSecretPredicate_GenerationChangeAdmitted verifies that a spec
// mutation (observable as ObjectOld.Generation != ObjectNew.Generation)
// passes the predicate. ESO would not normally mutate the PushSecret spec
// itself, but a user or controller doing so must still re-trigger Keystone
// reconciliation to re-evaluate adoption state.
func TestPushSecretPredicate_GenerationChangeAdmitted(t *testing.T) {
	g := NewGomegaWithT(t)

	oldObj := pushSecretWithMeta(1, []string{esoPushSecretFinalizer}, nil)
	newObj := pushSecretWithMeta(2, []string{esoPushSecretFinalizer}, nil)

	admitted := pushSecretRelevantChangePredicate.Update(event.UpdateEvent{
		ObjectOld: oldObj,
		ObjectNew: newObj,
	})

	g.Expect(admitted).To(BeTrue(),
		"Generation change (spec mutation) must pass the predicate")
}

// --- registerSecretNameIndex helper ---

// recordingFieldIndexer captures each IndexField invocation so tests can
// assert the key passed to registerSecretNameIndex matches the exported
// const — defending against literal drift and silent rename of the index key.
type recordingFieldIndexer struct {
	calls []recordedIndexFieldCall
	err   error
}

type recordedIndexFieldCall struct {
	obj   client.Object
	field string
	fn    client.IndexerFunc
}

func (r *recordingFieldIndexer) IndexField(_ context.Context, obj client.Object, field string, fn client.IndexerFunc) error {
	r.calls = append(r.calls, recordedIndexFieldCall{obj: obj, field: field, fn: fn})
	return r.err
}

// TestKeystoneSecretIndexerKey_IsDeclaredAsConst asserts that
// registerSecretNameIndex sources its index key from the exported
// KeystoneSecretNameIndexKey constant rather than a duplicated literal,
// keeping registration and lookup sites in sync.
func TestKeystoneSecretIndexerKey_IsDeclaredAsConst(t *testing.T) {
	g := NewGomegaWithT(t)

	rec := &recordingFieldIndexer{}
	err := registerSecretNameIndex(context.Background(), rec)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(rec.calls).To(HaveLen(1),
		"registerSecretNameIndex must register exactly one field indexer")
	g.Expect(rec.calls[0].field).To(Equal(KeystoneSecretNameIndexKey),
		"registration key must be sourced from the exported const, not a literal")
	g.Expect(rec.calls[0].obj).To(BeAssignableToTypeOf(&keystonev1alpha1.Keystone{}),
		"indexer must be registered against the Keystone type")
}

// TestRegisterSecretNameIndex_WrapsErrorWithKey verifies that an IndexField
// failure is returned wrapped with the index key so manager-startup logs
// identify which indexer failed to register.
func TestRegisterSecretNameIndex_WrapsErrorWithKey(t *testing.T) {
	g := NewGomegaWithT(t)
	rec := &recordingFieldIndexer{err: errors.New("boom")}

	err := registerSecretNameIndex(context.Background(), rec)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(KeystoneSecretNameIndexKey),
		"wrapped error must mention the index key for debuggability")
	g.Expect(err.Error()).To(ContainSubstring("boom"),
		"wrapped error must preserve the underlying IndexField error")
}

// TestReconcileEmitsDurationForEverySubReconciler runs a single happy-path
// Reconcile pass and asserts that the reconcile-duration histogram observed at
// least one sample for every sub_reconciler name registered in
// subReconcilerConditionTypes. This is the wiring check: every
// sub-reconciler call site in Reconcile must flow through
// instrumentSubReconciler.
func TestReconcileEmitsDurationForEverySubReconciler(t *testing.T) {
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

	// Capture a baseline per sub_reconciler so the test tolerates counts from
	// prior tests sharing the global controller-runtime registry.
	baseline := make(map[string]uint64, len(subReconcilerConditionTypes))
	for name := range subReconcilerConditionTypes {
		baseline[name] = histogramSampleCount(t, "keystone_operator_reconcile_duration_seconds",
			map[string]string{"sub_reconciler": name})
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace},
	})
	g.Expect(err).NotTo(HaveOccurred())

	for name := range subReconcilerConditionTypes {
		after := histogramSampleCount(t, "keystone_operator_reconcile_duration_seconds",
			map[string]string{"sub_reconciler": name})
		g.Expect(after-baseline[name]).To(BeNumerically(">=", uint64(1)),
			"sub_reconciler %q must emit at least one duration sample per Reconcile pass", name)
	}
}

// TestReconcileErrorsTotalIncrementsOnInducedFailure proves that a real
// sub-reconciler error in the SecretsReady chain advances the
// keystone_operator_reconcile_errors_total counter.
//
// The induction mechanism uses interceptor.Funcs to fail every write attempt
// (Create, Update, Patch) on the derived <name>-db-connection Secret with a
// deterministic Forbidden error. This is intentionally write-method-agnostic
// so the test stays valid regardless of whether reconcileDBConnectionSecret
// materializes the Secret via Update (current code) or Server-Side Apply (a
// possible future migration). Earlier versions of this test relied on the
// apiserver rejecting an Update to an Immutable=true Secret, which would have
// silently passed for the wrong reason after an SSA migration — the I-001
// review feedback explicitly called out that fragility.
//
// NOTE: ctrlmetrics.Registry is process-global and shared across every test
// in this package. The before/after delta tolerates other tests in the same
// package contributing to the same counter while this test is running.
func TestReconcileErrorsTotalIncrementsOnInducedFailure(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	derivedName := ks.Name + "-db-connection"

	s := testScheme()
	objs := append(
		[]runtime.Object{
			ks,
			testDBCredentialsSecret(),
			testAdminCredentialsSecret(),
			testFernetKeysSecret(),
			testCredentialKeysSecret(),
		},
		testReadyExternalSecrets()...,
	)
	cb := fake.NewClientBuilder().WithScheme(s)
	for _, obj := range objs {
		cb = cb.WithRuntimeObjects(obj)
	}
	cb = cb.WithStatusSubresource(&keystonev1alpha1.Keystone{}, &esov1.ExternalSecret{})

	denyDerivedSecretWrite := apierrors.NewForbidden(
		corev1.Resource("secrets"),
		derivedName,
		fmt.Errorf("simulated apiserver rejection (I-001)"),
	)
	isDerivedSecret := func(obj client.Object) bool {
		sec, ok := obj.(*corev1.Secret)
		return ok && sec.GetName() == derivedName
	}
	cb = cb.WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if isDerivedSecret(obj) {
				return denyDerivedSecretWrite
			}
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if isDerivedSecret(obj) {
				return denyDerivedSecretWrite
			}
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if isDerivedSecret(obj) {
				return denyDerivedSecretWrite
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	r := &KeystoneReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	errLabels := map[string]string{
		"sub_reconciler": "DBConnectionSecret",
		"condition_type": "SecretsReady",
	}
	before := counterValue(t, "keystone_operator_reconcile_errors_total", errLabels)

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: ks.Name, Namespace: ks.Namespace},
	})
	g.Expect(err).To(HaveOccurred(),
		"the induced derived-Secret write rejection MUST surface as a reconcile error so instrumentSubReconciler increments the counter")
	g.Expect(strings.Contains(err.Error(), "simulated apiserver rejection")).To(BeTrue(),
		"reconcile error MUST preserve the underlying interceptor message so the failure path is unambiguous (got: %v)", err)

	after := counterValue(t, "keystone_operator_reconcile_errors_total", errLabels)
	g.Expect(after-before).To(BeNumerically(">=", 1.0),
		"keystone_operator_reconcile_errors_total{sub_reconciler=\"DBConnectionSecret\","+
			"condition_type=\"SecretsReady\"} must advance by at least 1 after the induced "+
			"DBConnectionSecret failure")
}

// TestReconcileParallelGroupErrorCountsAreAttributed exercises
// reconcileParallelGroup with a failing CredentialKeys member and asserts that
// the error counter is attributed to CredentialKeys (not FernetKeys or
// NetworkPolicy). This is the wiring check for on the parallel
// group: each member must emit its own sub_reconciler label.
func TestReconcileParallelGroupErrorCountsAreAttributed(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newTestReconciler()
	ks := testKeystone()

	credLabels := map[string]string{"sub_reconciler": "CredentialKeys", "condition_type": "CredentialKeysReady"}
	fernetLabels := map[string]string{"sub_reconciler": "FernetKeys", "condition_type": "FernetKeysReady"}
	netLabels := map[string]string{"sub_reconciler": "NetworkPolicy", "condition_type": "NetworkPolicyReady"}

	baseCred := counterValue(t, "keystone_operator_reconcile_errors_total", credLabels)
	baseFernet := counterValue(t, "keystone_operator_reconcile_errors_total", fernetLabels)
	baseNet := counterValue(t, "keystone_operator_reconcile_errors_total", netLabels)

	subs := []commonreconcile.ParallelStep[*keystonev1alpha1.Keystone]{
		{
			Name:          "FernetKeys",
			ConditionType: "FernetKeysReady",
			Fn: func(_ context.Context, _ *keystonev1alpha1.Keystone) (ctrl.Result, error) {
				return ctrl.Result{}, nil
			},
		},
		{
			Name:          "CredentialKeys",
			ConditionType: "CredentialKeysReady",
			Fn: func(_ context.Context, _ *keystonev1alpha1.Keystone) (ctrl.Result, error) {
				return ctrl.Result{}, errors.New("boom")
			},
		},
		{
			Name:          "NetworkPolicy",
			ConditionType: "NetworkPolicyReady",
			Fn: func(_ context.Context, _ *keystonev1alpha1.Keystone) (ctrl.Result, error) {
				return ctrl.Result{}, nil
			},
		},
	}

	_, err := r.reconcileParallelGroup(context.Background(), ks, subs)
	g.Expect(err).To(HaveOccurred())

	afterCred := counterValue(t, "keystone_operator_reconcile_errors_total", credLabels)
	afterFernet := counterValue(t, "keystone_operator_reconcile_errors_total", fernetLabels)
	afterNet := counterValue(t, "keystone_operator_reconcile_errors_total", netLabels)

	g.Expect(afterCred-baseCred).To(Equal(1.0),
		"failing CredentialKeys member must increment reconcile_errors with its own sub_reconciler label")
	g.Expect(afterFernet-baseFernet).To(Equal(0.0),
		"FernetKeys succeeded; it must NOT be credited with an error")
	g.Expect(afterNet-baseNet).To(Equal(0.0),
		"NetworkPolicy succeeded; it must NOT be credited with an error")
}

// TestReconcileDeleteRemovesRotationAgeSeries verifies that reconcileDelete
// drops every per-CR metric series (key_rotation_age gauges, db_sync counters
// and duration samples) after it releases the Keystone finalizer, so a
// deleted CR never lingers in Prometheus output and a replacement CR with the
// same name starts with a clean slate.
func TestReconcileDeleteRemovesRotationAgeSeries(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := testKeystone()
	ks.Name = "rot-delete-cleanup"
	ks.Namespace = "ns-rot-delete-cleanup"

	r := newTestReconciler(ks)
	ctx := context.Background()
	_ = markKeystoneTerminating(t, r.Client, ks)

	// Seed the gauge on the global registry for both key types.
	completedAt := time.Now().Add(-30 * time.Minute)
	g.Expect(metrics.SetKeyRotationAge(ks.Name, ks.Namespace, "fernet", completedAt)).To(Succeed())
	g.Expect(metrics.SetKeyRotationAge(ks.Name, ks.Namespace, "credential", completedAt)).To(Succeed())

	fernetLabels := map[string]string{
		"keystone": ks.Name, "namespace": ks.Namespace, "key_type": "fernet",
	}
	credLabels := map[string]string{
		"keystone": ks.Name, "namespace": ks.Namespace, "key_type": "credential",
	}
	g.Expect(findMetricByLabels(t, ctrlmetrics.Registry, "keystone_operator_key_rotation_age_seconds", fernetLabels)).
		NotTo(BeNil(), "precondition: fernet gauge series must exist before reconcileDelete runs")
	g.Expect(findMetricByLabels(t, ctrlmetrics.Registry, "keystone_operator_key_rotation_age_seconds", credLabels)).
		NotTo(BeNil(), "precondition: credential gauge series must exist before reconcileDelete runs")

	// Refresh the terminating CR so the reconciler sees the in-etcd state.
	var terminating keystonev1alpha1.Keystone
	g.Expect(r.Get(ctx, client.ObjectKeyFromObject(ks), &terminating)).To(Succeed())

	_, err := r.reconcileDelete(ctx, &terminating)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(findMetricByLabels(t, ctrlmetrics.Registry, "keystone_operator_key_rotation_age_seconds", fernetLabels)).
		To(BeNil(), "reconcileDelete MUST remove the fernet rotation-age gauge series")
	g.Expect(findMetricByLabels(t, ctrlmetrics.Registry, "keystone_operator_key_rotation_age_seconds", credLabels)).
		To(BeNil(), "reconcileDelete MUST remove the credential rotation-age gauge series")
}
