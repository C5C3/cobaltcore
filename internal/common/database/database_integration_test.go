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
	testenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
)

var k8sClient client.Client

const testNamespace = "test-database"

var databaseGVK = schema.GroupVersionKind{
	Group:   "k8s.mariadb.com",
	Version: "v1alpha1",
	Kind:    "Database",
}

func TestMain(m *testing.M) {
	_, c, teardown, err := testenvtest.SetupEnvTest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup envtest: %v\n", err)
		os.Exit(1)
	}
	k8sClient = c

	ctx := context.Background()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNamespace,
		},
	}
	if err := k8sClient.Create(ctx, ns); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create test namespace: %v\n", err)
		teardown()
		os.Exit(1)
	}

	code := m.Run()
	teardown()
	os.Exit(code)
}

// ---------------------------------------------------------------------------
// EnsureDatabase tests
// ---------------------------------------------------------------------------

func TestEnsureDatabase_CreatesWithCorrectSpec(t *testing.T) {
	ctx := context.Background()
	name := "test-db-spec"
	mariadbRef := "my-mariadb-instance"

	err := database.EnsureDatabase(ctx, k8sClient, name, testNamespace, mariadbRef)
	if err != nil {
		t.Fatalf("EnsureDatabase returned error: %v", err)
	}

	// Fetch the Database CR and verify spec fields.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(databaseGVK)
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: testNamespace}, obj); err != nil {
		t.Fatalf("failed to get Database %s/%s: %v", testNamespace, name, err)
	}

	// Verify spec.mariaDbRef.name
	refName, found, err := unstructured.NestedString(obj.Object, "spec", "mariaDbRef", "name")
	if err != nil {
		t.Fatalf("error reading spec.mariaDbRef.name: %v", err)
	}
	if !found {
		t.Fatal("spec.mariaDbRef.name not found")
	}
	if refName != mariadbRef {
		t.Fatalf("expected spec.mariaDbRef.name=%q, got %q", mariadbRef, refName)
	}

	// Verify spec.name
	dbName, found, err := unstructured.NestedString(obj.Object, "spec", "name")
	if err != nil {
		t.Fatalf("error reading spec.name: %v", err)
	}
	if !found {
		t.Fatal("spec.name not found")
	}
	if dbName != name {
		t.Fatalf("expected spec.name=%q, got %q", name, dbName)
	}
}

func TestEnsureDatabase_Idempotent(t *testing.T) {
	ctx := context.Background()
	name := "test-db-idempotent"
	mariadbRef := "my-mariadb-instance"

	err := database.EnsureDatabase(ctx, k8sClient, name, testNamespace, mariadbRef)
	if err != nil {
		t.Fatalf("first call to EnsureDatabase returned error: %v", err)
	}

	// Second call should return nil (AlreadyExists is swallowed).
	err = database.EnsureDatabase(ctx, k8sClient, name, testNamespace, mariadbRef)
	if err != nil {
		t.Fatalf("second call to EnsureDatabase returned error: %v", err)
	}
}

func TestEnsureDatabase_SetsOwnerReferences(t *testing.T) {
	ctx := context.Background()
	name := "test-db-ownerrefs"
	mariadbRef := "my-mariadb-instance"

	ownerRef := metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "my-deployment",
		UID:        "test-uid-12345",
	}

	err := database.EnsureDatabase(ctx, k8sClient, name, testNamespace, mariadbRef, ownerRef)
	if err != nil {
		t.Fatalf("EnsureDatabase returned error: %v", err)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(databaseGVK)
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: testNamespace}, obj); err != nil {
		t.Fatalf("failed to get Database %s/%s: %v", testNamespace, name, err)
	}

	refs := obj.GetOwnerReferences()
	if len(refs) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(refs))
	}
	if refs[0].Name != "my-deployment" {
		t.Fatalf("expected owner reference name=%q, got %q", "my-deployment", refs[0].Name)
	}
	if refs[0].Kind != "Deployment" {
		t.Fatalf("expected owner reference kind=%q, got %q", "Deployment", refs[0].Kind)
	}
	if refs[0].APIVersion != "apps/v1" {
		t.Fatalf("expected owner reference apiVersion=%q, got %q", "apps/v1", refs[0].APIVersion)
	}
	if string(refs[0].UID) != "test-uid-12345" {
		t.Fatalf("expected owner reference uid=%q, got %q", "test-uid-12345", refs[0].UID)
	}
}

// ---------------------------------------------------------------------------
// RunDBSyncJob tests
// ---------------------------------------------------------------------------

func TestRunDBSyncJob_CreatesJobWithCorrectSpec(t *testing.T) {
	ctx := context.Background()
	name := "test-dbsync-spec"
	image := "keystone:latest"
	command := []string{"keystone-manage", "db_sync"}

	err := database.RunDBSyncJob(ctx, k8sClient, name, testNamespace, image, command, nil, nil)
	if err != nil {
		t.Fatalf("RunDBSyncJob returned error: %v", err)
	}

	got := &batchv1.Job{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: testNamespace}, got); err != nil {
		t.Fatalf("failed to get Job: %v", err)
	}

	// Verify backoffLimit.
	if got.Spec.BackoffLimit == nil || *got.Spec.BackoffLimit != 4 {
		t.Fatalf("expected backoffLimit=4, got %v", got.Spec.BackoffLimit)
	}

	// Verify TTL.
	if got.Spec.TTLSecondsAfterFinished == nil || *got.Spec.TTLSecondsAfterFinished != 300 {
		t.Fatalf("expected ttlSecondsAfterFinished=300, got %v", got.Spec.TTLSecondsAfterFinished)
	}

	// Verify RestartPolicy.
	if got.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("expected restartPolicy=Never, got %q", got.Spec.Template.Spec.RestartPolicy)
	}

	// Verify container spec.
	containers := got.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(containers))
	}
	c := containers[0]
	if c.Name != "db-sync" {
		t.Fatalf("expected container name=%q, got %q", "db-sync", c.Name)
	}
	if c.Image != image {
		t.Fatalf("expected container image=%q, got %q", image, c.Image)
	}
	if len(c.Command) != 2 || c.Command[0] != "keystone-manage" || c.Command[1] != "db_sync" {
		t.Fatalf("expected command=%v, got %v", command, c.Command)
	}
}

func TestRunDBSyncJob_Idempotent(t *testing.T) {
	ctx := context.Background()
	name := "test-dbsync-idempotent"
	image := "keystone:latest"
	command := []string{"keystone-manage", "db_sync"}

	err := database.RunDBSyncJob(ctx, k8sClient, name, testNamespace, image, command, nil, nil)
	if err != nil {
		t.Fatalf("first call to RunDBSyncJob returned error: %v", err)
	}

	// Second call should return nil (AlreadyExists is swallowed).
	err = database.RunDBSyncJob(ctx, k8sClient, name, testNamespace, image, command, nil, nil)
	if err != nil {
		t.Fatalf("second call to RunDBSyncJob returned error: %v", err)
	}
}

func TestRunDBSyncJob_SetsOwnerReferences(t *testing.T) {
	ctx := context.Background()
	name := "test-dbsync-ownerrefs"
	image := "keystone:latest"
	command := []string{"keystone-manage", "db_sync"}

	ownerRef := metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "my-deployment",
		UID:        "test-uid-67890",
	}

	err := database.RunDBSyncJob(ctx, k8sClient, name, testNamespace, image, command, nil, nil, ownerRef)
	if err != nil {
		t.Fatalf("RunDBSyncJob returned error: %v", err)
	}

	got := &batchv1.Job{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: testNamespace}, got); err != nil {
		t.Fatalf("failed to get Job: %v", err)
	}

	refs := got.GetOwnerReferences()
	if len(refs) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(refs))
	}
	if refs[0].Name != "my-deployment" {
		t.Fatalf("expected owner reference name=%q, got %q", "my-deployment", refs[0].Name)
	}
	if refs[0].Kind != "Deployment" {
		t.Fatalf("expected owner reference kind=%q, got %q", "Deployment", refs[0].Kind)
	}
	if refs[0].APIVersion != "apps/v1" {
		t.Fatalf("expected owner reference apiVersion=%q, got %q", "apps/v1", refs[0].APIVersion)
	}
	if string(refs[0].UID) != "test-uid-67890" {
		t.Fatalf("expected owner reference uid=%q, got %q", "test-uid-67890", refs[0].UID)
	}
}

func TestRunDBSyncJob_WithVolumeMounts(t *testing.T) {
	ctx := context.Background()
	name := "test-dbsync-volumes"
	image := "keystone:latest"
	command := []string{"keystone-manage", "db_sync"}

	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "config-volume",
			MountPath: "/etc/keystone",
			ReadOnly:  true,
		},
	}
	volumes := []corev1.Volume{
		{
			Name: "config-volume",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: "keystone-config",
				},
			},
		},
	}

	err := database.RunDBSyncJob(ctx, k8sClient, name, testNamespace, image, command, volumeMounts, volumes)
	if err != nil {
		t.Fatalf("RunDBSyncJob returned error: %v", err)
	}

	got := &batchv1.Job{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: testNamespace}, got); err != nil {
		t.Fatalf("failed to get Job: %v", err)
	}

	// Verify volume mounts on container.
	containers := got.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(containers))
	}
	mounts := containers[0].VolumeMounts
	if len(mounts) != 1 {
		t.Fatalf("expected 1 volume mount, got %d", len(mounts))
	}
	if mounts[0].Name != "config-volume" {
		t.Fatalf("expected volume mount name=%q, got %q", "config-volume", mounts[0].Name)
	}
	if mounts[0].MountPath != "/etc/keystone" {
		t.Fatalf("expected volume mount mountPath=%q, got %q", "/etc/keystone", mounts[0].MountPath)
	}
	if !mounts[0].ReadOnly {
		t.Fatal("expected volume mount readOnly=true, got false")
	}

	// Verify volumes on pod spec.
	vols := got.Spec.Template.Spec.Volumes
	if len(vols) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(vols))
	}
	if vols[0].Name != "config-volume" {
		t.Fatalf("expected volume name=%q, got %q", "config-volume", vols[0].Name)
	}
	if vols[0].Secret == nil {
		t.Fatal("expected volume to have Secret source")
	}
	if vols[0].Secret.SecretName != "keystone-config" {
		t.Fatalf("expected secret name=%q, got %q", "keystone-config", vols[0].Secret.SecretName)
	}
}

// ---------------------------------------------------------------------------
// EnsureDatabaseUser tests
// ---------------------------------------------------------------------------

var userGVK = schema.GroupVersionKind{
	Group:   "k8s.mariadb.com",
	Version: "v1alpha1",
	Kind:    "User",
}

var grantGVK = schema.GroupVersionKind{
	Group:   "k8s.mariadb.com",
	Version: "v1alpha1",
	Kind:    "Grant",
}

func TestEnsureDatabaseUser_CreatesUserAndGrant(t *testing.T) {
	ctx := context.Background()
	name := "test-user-spec"
	mariadbRef := "my-mariadb-instance"
	passwordSecretName := "my-secret"
	passwordSecretKey := "password"

	err := database.EnsureDatabaseUser(ctx, k8sClient, name, testNamespace, mariadbRef, passwordSecretName, passwordSecretKey)
	if err != nil {
		t.Fatalf("EnsureDatabaseUser returned error: %v", err)
	}

	// Fetch and verify the User CR.
	user := &unstructured.Unstructured{}
	user.SetGroupVersionKind(userGVK)
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: testNamespace}, user); err != nil {
		t.Fatalf("failed to get User %s/%s: %v", testNamespace, name, err)
	}

	// Verify spec.mariaDbRef.name
	refName, found, err := unstructured.NestedString(user.Object, "spec", "mariaDbRef", "name")
	if err != nil {
		t.Fatalf("error reading User spec.mariaDbRef.name: %v", err)
	}
	if !found {
		t.Fatal("User spec.mariaDbRef.name not found")
	}
	if refName != mariadbRef {
		t.Fatalf("expected User spec.mariaDbRef.name=%q, got %q", mariadbRef, refName)
	}

	// Verify spec.name
	userName, found, err := unstructured.NestedString(user.Object, "spec", "name")
	if err != nil {
		t.Fatalf("error reading User spec.name: %v", err)
	}
	if !found {
		t.Fatal("User spec.name not found")
	}
	if userName != name {
		t.Fatalf("expected User spec.name=%q, got %q", name, userName)
	}

	// Verify spec.passwordSecretKeyRef.name
	secretName, found, err := unstructured.NestedString(user.Object, "spec", "passwordSecretKeyRef", "name")
	if err != nil {
		t.Fatalf("error reading User spec.passwordSecretKeyRef.name: %v", err)
	}
	if !found {
		t.Fatal("User spec.passwordSecretKeyRef.name not found")
	}
	if secretName != passwordSecretName {
		t.Fatalf("expected User spec.passwordSecretKeyRef.name=%q, got %q", passwordSecretName, secretName)
	}

	// Verify spec.passwordSecretKeyRef.key
	secretKey, found, err := unstructured.NestedString(user.Object, "spec", "passwordSecretKeyRef", "key")
	if err != nil {
		t.Fatalf("error reading User spec.passwordSecretKeyRef.key: %v", err)
	}
	if !found {
		t.Fatal("User spec.passwordSecretKeyRef.key not found")
	}
	if secretKey != passwordSecretKey {
		t.Fatalf("expected User spec.passwordSecretKeyRef.key=%q, got %q", passwordSecretKey, secretKey)
	}

	// Fetch and verify the Grant CR.
	grant := &unstructured.Unstructured{}
	grant.SetGroupVersionKind(grantGVK)
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name + "-grant", Namespace: testNamespace}, grant); err != nil {
		t.Fatalf("failed to get Grant %s/%s-grant: %v", testNamespace, name, err)
	}

	// Verify Grant spec.mariaDbRef.name
	grantRefName, found, err := unstructured.NestedString(grant.Object, "spec", "mariaDbRef", "name")
	if err != nil {
		t.Fatalf("error reading Grant spec.mariaDbRef.name: %v", err)
	}
	if !found {
		t.Fatal("Grant spec.mariaDbRef.name not found")
	}
	if grantRefName != mariadbRef {
		t.Fatalf("expected Grant spec.mariaDbRef.name=%q, got %q", mariadbRef, grantRefName)
	}

	// Verify Grant spec.database
	dbVal, found, err := unstructured.NestedString(grant.Object, "spec", "database")
	if err != nil {
		t.Fatalf("error reading Grant spec.database: %v", err)
	}
	if !found {
		t.Fatal("Grant spec.database not found")
	}
	if dbVal != "*" {
		t.Fatalf("expected Grant spec.database=%q, got %q", "*", dbVal)
	}

	// Verify Grant spec.table
	tableVal, found, err := unstructured.NestedString(grant.Object, "spec", "table")
	if err != nil {
		t.Fatalf("error reading Grant spec.table: %v", err)
	}
	if !found {
		t.Fatal("Grant spec.table not found")
	}
	if tableVal != "*" {
		t.Fatalf("expected Grant spec.table=%q, got %q", "*", tableVal)
	}

	// Verify Grant spec.username
	grantUsername, found, err := unstructured.NestedString(grant.Object, "spec", "username")
	if err != nil {
		t.Fatalf("error reading Grant spec.username: %v", err)
	}
	if !found {
		t.Fatal("Grant spec.username not found")
	}
	if grantUsername != name {
		t.Fatalf("expected Grant spec.username=%q, got %q", name, grantUsername)
	}

	// Verify Grant spec.privileges
	privs, found, err := unstructured.NestedStringSlice(grant.Object, "spec", "privileges")
	if err != nil {
		t.Fatalf("error reading Grant spec.privileges: %v", err)
	}
	if !found {
		t.Fatal("Grant spec.privileges not found")
	}
	if len(privs) != 1 || privs[0] != "ALL" {
		t.Fatalf("expected Grant spec.privileges=[\"ALL\"], got %v", privs)
	}
}

func TestEnsureDatabaseUser_Idempotent(t *testing.T) {
	ctx := context.Background()
	name := "test-user-idempotent"
	mariadbRef := "my-mariadb-instance"
	passwordSecretName := "my-secret"
	passwordSecretKey := "password"

	err := database.EnsureDatabaseUser(ctx, k8sClient, name, testNamespace, mariadbRef, passwordSecretName, passwordSecretKey)
	if err != nil {
		t.Fatalf("first call to EnsureDatabaseUser returned error: %v", err)
	}

	// Second call should return nil (AlreadyExists is swallowed for both User and Grant).
	err = database.EnsureDatabaseUser(ctx, k8sClient, name, testNamespace, mariadbRef, passwordSecretName, passwordSecretKey)
	if err != nil {
		t.Fatalf("second call to EnsureDatabaseUser returned error: %v", err)
	}
}

func TestEnsureDatabaseUser_SetsOwnerReferences(t *testing.T) {
	ctx := context.Background()
	name := "test-user-ownerrefs"
	mariadbRef := "my-mariadb-instance"
	passwordSecretName := "my-secret"
	passwordSecretKey := "password"

	ownerRef := metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "my-deployment",
		UID:        "test-uid-user-99999",
	}

	err := database.EnsureDatabaseUser(ctx, k8sClient, name, testNamespace, mariadbRef, passwordSecretName, passwordSecretKey, ownerRef)
	if err != nil {
		t.Fatalf("EnsureDatabaseUser returned error: %v", err)
	}

	// Verify owner references on User CR.
	user := &unstructured.Unstructured{}
	user.SetGroupVersionKind(userGVK)
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: testNamespace}, user); err != nil {
		t.Fatalf("failed to get User %s/%s: %v", testNamespace, name, err)
	}

	userRefs := user.GetOwnerReferences()
	if len(userRefs) != 1 {
		t.Fatalf("expected 1 owner reference on User, got %d", len(userRefs))
	}
	if userRefs[0].Name != "my-deployment" {
		t.Fatalf("expected User owner reference name=%q, got %q", "my-deployment", userRefs[0].Name)
	}
	if userRefs[0].Kind != "Deployment" {
		t.Fatalf("expected User owner reference kind=%q, got %q", "Deployment", userRefs[0].Kind)
	}
	if userRefs[0].APIVersion != "apps/v1" {
		t.Fatalf("expected User owner reference apiVersion=%q, got %q", "apps/v1", userRefs[0].APIVersion)
	}
	if string(userRefs[0].UID) != "test-uid-user-99999" {
		t.Fatalf("expected User owner reference uid=%q, got %q", "test-uid-user-99999", userRefs[0].UID)
	}

	// Verify owner references on Grant CR.
	grant := &unstructured.Unstructured{}
	grant.SetGroupVersionKind(grantGVK)
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name + "-grant", Namespace: testNamespace}, grant); err != nil {
		t.Fatalf("failed to get Grant %s/%s-grant: %v", testNamespace, name, err)
	}

	grantRefs := grant.GetOwnerReferences()
	if len(grantRefs) != 1 {
		t.Fatalf("expected 1 owner reference on Grant, got %d", len(grantRefs))
	}
	if grantRefs[0].Name != "my-deployment" {
		t.Fatalf("expected Grant owner reference name=%q, got %q", "my-deployment", grantRefs[0].Name)
	}
	if grantRefs[0].Kind != "Deployment" {
		t.Fatalf("expected Grant owner reference kind=%q, got %q", "Deployment", grantRefs[0].Kind)
	}
	if grantRefs[0].APIVersion != "apps/v1" {
		t.Fatalf("expected Grant owner reference apiVersion=%q, got %q", "apps/v1", grantRefs[0].APIVersion)
	}
	if string(grantRefs[0].UID) != "test-uid-user-99999" {
		t.Fatalf("expected Grant owner reference uid=%q, got %q", "test-uid-user-99999", grantRefs[0].UID)
	}
}
