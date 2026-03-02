//go:build integration

package database_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/database"
	"github.com/c5c3/forge/internal/common/testutil"
	testenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
)

var k8sClient client.Client

func TestMain(m *testing.M) {
	_, c, teardown, err := testenvtest.SetupEnvTest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup envtest: %v\n", err)
		os.Exit(1)
	}
	k8sClient = c
	code := m.Run()
	teardown()
	os.Exit(code)
}

func createTestNamespace(t *testing.T, ctx context.Context) *corev1.Namespace {
	return testutil.CreateTestNamespace(t, ctx, k8sClient, "test-database-")
}

// ---------------------------------------------------------------------------
// EnsureDatabase
// ---------------------------------------------------------------------------

func TestEnsureDatabase_Creates(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	err := database.EnsureDatabase(ctx, k8sClient,
		"test-db", ns.Name, "mydb", "mariadb-instance",
	)
	if err != nil {
		t.Fatalf("EnsureDatabase returned error: %v", err)
	}

	// Verify the Database CR exists with correct spec fields.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "Database",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-db", Namespace: ns.Name}, obj); err != nil {
		t.Fatalf("failed to get Database after creation: %v", err)
	}

	// Check spec.mariaDbRef.name
	refName, _, _ := unstructured.NestedString(obj.Object, "spec", "mariaDbRef", "name")
	if refName != "mariadb-instance" {
		t.Fatalf("expected spec.mariaDbRef.name=mariadb-instance, got %s", refName)
	}

	// Check spec.name
	dbName, _, _ := unstructured.NestedString(obj.Object, "spec", "name")
	if dbName != "mydb" {
		t.Fatalf("expected spec.name=mydb, got %s", dbName)
	}
}

func TestEnsureDatabase_Idempotent(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	if err := database.EnsureDatabase(ctx, k8sClient,
		"idem-db", ns.Name, "mydb", "mariadb-instance",
	); err != nil {
		t.Fatalf("first EnsureDatabase returned error: %v", err)
	}

	if err := database.EnsureDatabase(ctx, k8sClient,
		"idem-db", ns.Name, "mydb", "mariadb-instance",
	); err != nil {
		t.Fatalf("second EnsureDatabase returned error: %v", err)
	}
}

func TestEnsureDatabase_OwnerReferences(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	ownerRef := metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "my-deployment",
		UID:        "12345678-1234-1234-1234-123456789abc",
	}

	err := database.EnsureDatabase(ctx, k8sClient,
		"owned-db", ns.Name, "mydb", "mariadb-instance", ownerRef,
	)
	if err != nil {
		t.Fatalf("EnsureDatabase returned error: %v", err)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "Database",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "owned-db", Namespace: ns.Name}, obj); err != nil {
		t.Fatalf("failed to get Database: %v", err)
	}

	refs := obj.GetOwnerReferences()
	if len(refs) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(refs))
	}
	if refs[0].Name != "my-deployment" {
		t.Fatalf("expected owner reference name=my-deployment, got %s", refs[0].Name)
	}
	if refs[0].Kind != "Deployment" {
		t.Fatalf("expected owner reference kind=Deployment, got %s", refs[0].Kind)
	}
}

// ---------------------------------------------------------------------------
// EnsureDatabaseUser
// ---------------------------------------------------------------------------

func TestEnsureDatabaseUser_CreatesUserAndGrant(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	err := database.EnsureDatabaseUser(ctx, k8sClient,
		"test-user", ns.Name, "mariadb-instance",
		"db-password-secret", "password",
		"mydb", []string{"SELECT", "INSERT", "UPDATE"},
	)
	if err != nil {
		t.Fatalf("EnsureDatabaseUser returned error: %v", err)
	}

	// Verify User CR exists with correct spec fields.
	userObj := &unstructured.Unstructured{}
	userObj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "User",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-user", Namespace: ns.Name}, userObj); err != nil {
		t.Fatalf("failed to get User after creation: %v", err)
	}

	refName, _, _ := unstructured.NestedString(userObj.Object, "spec", "mariaDbRef", "name")
	if refName != "mariadb-instance" {
		t.Fatalf("expected User spec.mariaDbRef.name=mariadb-instance, got %s", refName)
	}

	pwSecretName, _, _ := unstructured.NestedString(userObj.Object, "spec", "passwordSecretKeyRef", "name")
	if pwSecretName != "db-password-secret" {
		t.Fatalf("expected User spec.passwordSecretKeyRef.name=db-password-secret, got %s", pwSecretName)
	}

	pwSecretKey, _, _ := unstructured.NestedString(userObj.Object, "spec", "passwordSecretKeyRef", "key")
	if pwSecretKey != "password" {
		t.Fatalf("expected User spec.passwordSecretKeyRef.key=password, got %s", pwSecretKey)
	}

	userName, _, _ := unstructured.NestedString(userObj.Object, "spec", "name")
	if userName != "test-user" {
		t.Fatalf("expected User spec.name=test-user, got %s", userName)
	}

	// Verify Grant CR exists with correct spec fields.
	grantObj := &unstructured.Unstructured{}
	grantObj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "Grant",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-user-grant", Namespace: ns.Name}, grantObj); err != nil {
		t.Fatalf("failed to get Grant after creation: %v", err)
	}

	grantRefName, _, _ := unstructured.NestedString(grantObj.Object, "spec", "mariaDbRef", "name")
	if grantRefName != "mariadb-instance" {
		t.Fatalf("expected Grant spec.mariaDbRef.name=mariadb-instance, got %s", grantRefName)
	}

	grantDB, _, _ := unstructured.NestedString(grantObj.Object, "spec", "database")
	if grantDB != "mydb" {
		t.Fatalf("expected Grant spec.database=mydb, got %s", grantDB)
	}

	grantTable, _, _ := unstructured.NestedString(grantObj.Object, "spec", "table")
	if grantTable != "*" {
		t.Fatalf("expected Grant spec.table=*, got %s", grantTable)
	}

	grantUsername, _, _ := unstructured.NestedString(grantObj.Object, "spec", "username")
	if grantUsername != "test-user" {
		t.Fatalf("expected Grant spec.username=test-user, got %s", grantUsername)
	}

	privs, _, _ := unstructured.NestedStringSlice(grantObj.Object, "spec", "privileges")
	if len(privs) != 3 || privs[0] != "SELECT" || privs[1] != "INSERT" || privs[2] != "UPDATE" {
		t.Fatalf("unexpected Grant spec.privileges: %v", privs)
	}
}

func TestEnsureDatabaseUser_Idempotent(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	args := func() error {
		return database.EnsureDatabaseUser(ctx, k8sClient,
			"idem-user", ns.Name, "mariadb-instance",
			"db-password-secret", "password",
			"mydb", []string{"SELECT"},
		)
	}

	if err := args(); err != nil {
		t.Fatalf("first EnsureDatabaseUser returned error: %v", err)
	}

	if err := args(); err != nil {
		t.Fatalf("second EnsureDatabaseUser returned error: %v", err)
	}

	// Verify both CRs still exist with correct spec after idempotent call. (CC-0005)
	userObj := &unstructured.Unstructured{}
	userObj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "User",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "idem-user", Namespace: ns.Name}, userObj); err != nil {
		t.Fatalf("failed to get User after idempotent call: %v", err)
	}

	userName, _, _ := unstructured.NestedString(userObj.Object, "spec", "name")
	if userName != "idem-user" {
		t.Fatalf("expected User spec.name=idem-user, got %s", userName)
	}

	grantObj := &unstructured.Unstructured{}
	grantObj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "Grant",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "idem-user-grant", Namespace: ns.Name}, grantObj); err != nil {
		t.Fatalf("failed to get Grant after idempotent call: %v", err)
	}

	grantDB, _, _ := unstructured.NestedString(grantObj.Object, "spec", "database")
	if grantDB != "mydb" {
		t.Fatalf("expected Grant spec.database=mydb, got %s", grantDB)
	}

	privs, _, _ := unstructured.NestedStringSlice(grantObj.Object, "spec", "privileges")
	if len(privs) != 1 || privs[0] != "SELECT" {
		t.Fatalf("unexpected Grant spec.privileges: %v", privs)
	}
}

func TestEnsureDatabaseUser_OwnerReferences(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	ownerRef := metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "my-deployment",
		UID:        "12345678-1234-1234-1234-123456789abc",
	}

	err := database.EnsureDatabaseUser(ctx, k8sClient,
		"owned-user", ns.Name, "mariadb-instance",
		"db-password-secret", "password",
		"mydb", []string{"ALL PRIVILEGES"},
		ownerRef,
	)
	if err != nil {
		t.Fatalf("EnsureDatabaseUser returned error: %v", err)
	}

	// Verify owner references on User CR.
	userObj := &unstructured.Unstructured{}
	userObj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "User",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "owned-user", Namespace: ns.Name}, userObj); err != nil {
		t.Fatalf("failed to get User: %v", err)
	}

	userRefs := userObj.GetOwnerReferences()
	if len(userRefs) != 1 {
		t.Fatalf("expected 1 owner reference on User, got %d", len(userRefs))
	}
	if userRefs[0].Name != "my-deployment" || userRefs[0].Kind != "Deployment" {
		t.Fatalf("unexpected User owner reference: %v", userRefs[0])
	}

	// Verify owner references on Grant CR.
	grantObj := &unstructured.Unstructured{}
	grantObj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "Grant",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "owned-user-grant", Namespace: ns.Name}, grantObj); err != nil {
		t.Fatalf("failed to get Grant: %v", err)
	}

	grantRefs := grantObj.GetOwnerReferences()
	if len(grantRefs) != 1 {
		t.Fatalf("expected 1 owner reference on Grant, got %d", len(grantRefs))
	}
	if grantRefs[0].Name != "my-deployment" || grantRefs[0].Kind != "Deployment" {
		t.Fatalf("unexpected Grant owner reference: %v", grantRefs[0])
	}
}

// ---------------------------------------------------------------------------
// RunDBSyncJob
// ---------------------------------------------------------------------------

func TestRunDBSyncJob_Creates(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	env := []corev1.EnvVar{
		{Name: "DB_HOST", Value: "mariadb.default.svc"},
	}
	err := database.RunDBSyncJob(ctx, k8sClient,
		"test-db-sync", ns.Name, "myimage:latest",
		[]string{"db-sync"}, env,
	)
	if err != nil {
		t.Fatalf("RunDBSyncJob returned error: %v", err)
	}

	// Verify the Job exists with correct container spec.
	job := &batchv1.Job{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-db-sync", Namespace: ns.Name}, job); err != nil {
		t.Fatalf("failed to get Job after creation: %v", err)
	}

	containers := job.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(containers))
	}
	if containers[0].Name != "db-sync" {
		t.Fatalf("expected container name=db-sync, got %s", containers[0].Name)
	}
	if containers[0].Image != "myimage:latest" {
		t.Fatalf("expected container image=myimage:latest, got %s", containers[0].Image)
	}
	if len(containers[0].Command) != 1 || containers[0].Command[0] != "db-sync" {
		t.Fatalf("unexpected container command: %v", containers[0].Command)
	}
	if len(containers[0].Env) != 1 || containers[0].Env[0].Name != "DB_HOST" || containers[0].Env[0].Value != "mariadb.default.svc" {
		t.Fatalf("unexpected container env: %v", containers[0].Env)
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyOnFailure {
		t.Fatalf("expected RestartPolicy=OnFailure, got %s", job.Spec.Template.Spec.RestartPolicy)
	}
}

func TestRunDBSyncJob_Idempotent(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	args := func() error {
		return database.RunDBSyncJob(ctx, k8sClient,
			"idem-db-sync", ns.Name, "myimage:latest",
			[]string{"db-sync"}, nil,
		)
	}

	if err := args(); err != nil {
		t.Fatalf("first RunDBSyncJob returned error: %v", err)
	}

	if err := args(); err != nil {
		t.Fatalf("second RunDBSyncJob returned error: %v", err)
	}
}

func TestRunDBSyncJob_OwnerReferences(t *testing.T) {
	ctx := context.Background()
	ns := createTestNamespace(t, ctx)

	ownerRef := metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "my-deployment",
		UID:        "12345678-1234-1234-1234-123456789abc",
	}

	err := database.RunDBSyncJob(ctx, k8sClient,
		"owned-db-sync", ns.Name, "myimage:latest",
		[]string{"db-sync"}, nil,
		ownerRef,
	)
	if err != nil {
		t.Fatalf("RunDBSyncJob returned error: %v", err)
	}

	job := &batchv1.Job{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "owned-db-sync", Namespace: ns.Name}, job); err != nil {
		t.Fatalf("failed to get Job: %v", err)
	}

	refs := job.GetOwnerReferences()
	if len(refs) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(refs))
	}
	if refs[0].Name != "my-deployment" {
		t.Fatalf("expected owner reference name=my-deployment, got %s", refs[0].Name)
	}
	if refs[0].Kind != "Deployment" {
		t.Fatalf("expected owner reference kind=Deployment, got %s", refs[0].Kind)
	}
}
