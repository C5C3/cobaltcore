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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/database"
	testenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
)

var (
	k8sClient  client.Client
	testScheme *runtime.Scheme
)

const testNamespace = "test-database"

func TestMain(m *testing.M) {
	_, c, teardown, err := testenvtest.SetupEnvTest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup envtest: %v\n", err)
		os.Exit(1)
	}
	k8sClient = c

	testScheme = runtime.NewScheme()
	if err := corev1.AddToScheme(testScheme); err != nil {
		fmt.Fprintf(os.Stderr, "failed to add corev1 to scheme: %v\n", err)
		teardown()
		os.Exit(1)
	}
	if err := batchv1.AddToScheme(testScheme); err != nil {
		fmt.Fprintf(os.Stderr, "failed to add batchv1 to scheme: %v\n", err)
		teardown()
		os.Exit(1)
	}

	ctx := context.Background()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: testNamespace},
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

func newOwner(ctx context.Context, t *testing.T, name string) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
	}
	if err := k8sClient.Create(ctx, cm); err != nil {
		t.Fatalf("failed to create owner ConfigMap: %v", err)
	}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(cm), cm); err != nil {
		t.Fatalf("failed to get owner ConfigMap: %v", err)
	}
	return cm
}

// --- EnsureDatabase tests ---

func TestEnsureDatabase_Creates(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(ctx, t, "owner-db-creates")

	opts := database.DatabaseOpts{
		Name:         "db-creates",
		Namespace:    testNamespace,
		MariaDBRef:   "mariadb-instance",
		DatabaseName: "mydb",
	}

	name, err := database.EnsureDatabase(ctx, k8sClient, owner, testScheme, opts)
	if err != nil {
		t.Fatalf("EnsureDatabase returned error: %v", err)
	}
	if name != opts.Name {
		t.Fatalf("expected name %q, got %q", opts.Name, name)
	}

	// Verify the Database CR was created with correct spec fields.
	db := &unstructured.Unstructured{}
	db.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "Database",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: opts.Name, Namespace: testNamespace}, db); err != nil {
		t.Fatalf("failed to get Database: %v", err)
	}

	mariaDBRefName, _, _ := unstructured.NestedString(db.Object, "spec", "mariaDbRef", "name")
	if mariaDBRefName != opts.MariaDBRef {
		t.Fatalf("expected spec.mariaDbRef.name %q, got %q", opts.MariaDBRef, mariaDBRefName)
	}

	dbName, _, _ := unstructured.NestedString(db.Object, "spec", "name")
	if dbName != opts.DatabaseName {
		t.Fatalf("expected spec.name %q, got %q", opts.DatabaseName, dbName)
	}
}

func TestEnsureDatabase_Idempotent(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(ctx, t, "owner-db-idempotent")

	opts := database.DatabaseOpts{
		Name:         "db-idempotent",
		Namespace:    testNamespace,
		MariaDBRef:   "mariadb-instance",
		DatabaseName: "mydb",
	}

	if _, err := database.EnsureDatabase(ctx, k8sClient, owner, testScheme, opts); err != nil {
		t.Fatalf("first call to EnsureDatabase returned error: %v", err)
	}

	if _, err := database.EnsureDatabase(ctx, k8sClient, owner, testScheme, opts); err != nil {
		t.Fatalf("second call to EnsureDatabase returned error: %v", err)
	}
}

func TestEnsureDatabase_OwnerRef(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(ctx, t, "owner-db-ref")

	opts := database.DatabaseOpts{
		Name:         "db-ownerref",
		Namespace:    testNamespace,
		MariaDBRef:   "mariadb-instance",
		DatabaseName: "mydb",
	}

	if _, err := database.EnsureDatabase(ctx, k8sClient, owner, testScheme, opts); err != nil {
		t.Fatalf("EnsureDatabase returned error: %v", err)
	}

	db := &unstructured.Unstructured{}
	db.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "Database",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: opts.Name, Namespace: testNamespace}, db); err != nil {
		t.Fatalf("failed to get Database: %v", err)
	}

	ownerRefs := db.GetOwnerReferences()
	if len(ownerRefs) == 0 {
		t.Fatal("expected at least one owner reference, got none")
	}

	found := false
	for _, ref := range ownerRefs {
		if ref.UID == owner.UID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected owner reference with UID %s, not found in %v", owner.UID, ownerRefs)
	}
}

// --- EnsureDatabaseUser tests ---

func TestEnsureDatabaseUser_CreatesUserAndGrant(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(ctx, t, "owner-user-creates")

	opts := database.DatabaseUserOpts{
		Name:               "dbuser-creates",
		Namespace:          testNamespace,
		MariaDBRef:         "mariadb-instance",
		Username:           "app_user",
		PasswordSecretName: "db-password",
		PasswordSecretKey:  "password",
		DatabaseName:       "mydb",
		Privileges:         []string{"SELECT", "INSERT", "UPDATE", "DELETE"},
	}

	name, err := database.EnsureDatabaseUser(ctx, k8sClient, owner, testScheme, opts)
	if err != nil {
		t.Fatalf("EnsureDatabaseUser returned error: %v", err)
	}
	if name != opts.Name {
		t.Fatalf("expected name %q, got %q", opts.Name, name)
	}

	// Verify User CR.
	user := &unstructured.Unstructured{}
	user.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "User",
	})
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: opts.Name, Namespace: testNamespace}, user); err != nil {
		t.Fatalf("failed to get User: %v", err)
	}

	mariaDBRefName, _, _ := unstructured.NestedString(user.Object, "spec", "mariaDbRef", "name")
	if mariaDBRefName != opts.MariaDBRef {
		t.Fatalf("User spec.mariaDbRef.name: expected %q, got %q", opts.MariaDBRef, mariaDBRefName)
	}

	userName, _, _ := unstructured.NestedString(user.Object, "spec", "name")
	if userName != opts.Username {
		t.Fatalf("User spec.name: expected %q, got %q", opts.Username, userName)
	}

	pwSecretName, _, _ := unstructured.NestedString(user.Object, "spec", "passwordSecretKeyRef", "name")
	if pwSecretName != opts.PasswordSecretName {
		t.Fatalf("User spec.passwordSecretKeyRef.name: expected %q, got %q", opts.PasswordSecretName, pwSecretName)
	}

	pwSecretKey, _, _ := unstructured.NestedString(user.Object, "spec", "passwordSecretKeyRef", "key")
	if pwSecretKey != opts.PasswordSecretKey {
		t.Fatalf("User spec.passwordSecretKeyRef.key: expected %q, got %q", opts.PasswordSecretKey, pwSecretKey)
	}

	// Verify Grant CR.
	grant := &unstructured.Unstructured{}
	grant.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "Grant",
	})
	grantName := opts.Name + "-grant"
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: grantName, Namespace: testNamespace}, grant); err != nil {
		t.Fatalf("failed to get Grant: %v", err)
	}

	grantMariaDBRef, _, _ := unstructured.NestedString(grant.Object, "spec", "mariaDbRef", "name")
	if grantMariaDBRef != opts.MariaDBRef {
		t.Fatalf("Grant spec.mariaDbRef.name: expected %q, got %q", opts.MariaDBRef, grantMariaDBRef)
	}

	grantDB, _, _ := unstructured.NestedString(grant.Object, "spec", "database")
	if grantDB != opts.DatabaseName {
		t.Fatalf("Grant spec.database: expected %q, got %q", opts.DatabaseName, grantDB)
	}

	grantTable, _, _ := unstructured.NestedString(grant.Object, "spec", "table")
	if grantTable != "*" {
		t.Fatalf("Grant spec.table: expected %q, got %q", "*", grantTable)
	}

	grantUsername, _, _ := unstructured.NestedString(grant.Object, "spec", "username")
	if grantUsername != opts.Username {
		t.Fatalf("Grant spec.username: expected %q, got %q", opts.Username, grantUsername)
	}

	grantPrivileges, _, _ := unstructured.NestedStringSlice(grant.Object, "spec", "privileges")
	if len(grantPrivileges) != len(opts.Privileges) {
		t.Fatalf("Grant spec.privileges: expected %d items, got %d", len(opts.Privileges), len(grantPrivileges))
	}
	for i, expected := range opts.Privileges {
		if grantPrivileges[i] != expected {
			t.Fatalf("Grant spec.privileges[%d]: expected %q, got %q", i, expected, grantPrivileges[i])
		}
	}
}

func TestEnsureDatabaseUser_Idempotent(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(ctx, t, "owner-user-idempotent")

	opts := database.DatabaseUserOpts{
		Name:               "dbuser-idempotent",
		Namespace:          testNamespace,
		MariaDBRef:         "mariadb-instance",
		Username:           "app_user",
		PasswordSecretName: "db-password",
		PasswordSecretKey:  "password",
		DatabaseName:       "mydb",
		Privileges:         []string{"SELECT", "INSERT"},
	}

	if _, err := database.EnsureDatabaseUser(ctx, k8sClient, owner, testScheme, opts); err != nil {
		t.Fatalf("first call to EnsureDatabaseUser returned error: %v", err)
	}

	if _, err := database.EnsureDatabaseUser(ctx, k8sClient, owner, testScheme, opts); err != nil {
		t.Fatalf("second call to EnsureDatabaseUser returned error: %v", err)
	}
}

// --- RunDBSyncJob tests ---

func TestRunDBSyncJob_Creates(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(ctx, t, "owner-job-creates")

	opts := database.DBSyncJobOpts{
		Name:      "dbsync-creates",
		Namespace: testNamespace,
		Image:     "myapp:latest",
		Command:   []string{"db-sync", "--migrate"},
		Env: []corev1.EnvVar{
			{Name: "DB_HOST", Value: "mariadb"},
			{Name: "DB_NAME", Value: "mydb"},
		},
	}

	name, err := database.RunDBSyncJob(ctx, k8sClient, owner, testScheme, opts)
	if err != nil {
		t.Fatalf("RunDBSyncJob returned error: %v", err)
	}
	if name != opts.Name {
		t.Fatalf("expected name %q, got %q", opts.Name, name)
	}

	// Verify the Job was created with correct spec.
	job := &batchv1.Job{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: opts.Name, Namespace: testNamespace}, job); err != nil {
		t.Fatalf("failed to get Job: %v", err)
	}

	containers := job.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(containers))
	}

	c := containers[0]
	if c.Image != opts.Image {
		t.Fatalf("expected image %q, got %q", opts.Image, c.Image)
	}

	if len(c.Command) != len(opts.Command) {
		t.Fatalf("expected %d command args, got %d", len(opts.Command), len(c.Command))
	}
	for i, expected := range opts.Command {
		if c.Command[i] != expected {
			t.Fatalf("command[%d]: expected %q, got %q", i, expected, c.Command[i])
		}
	}

	if len(c.Env) != len(opts.Env) {
		t.Fatalf("expected %d env vars, got %d", len(opts.Env), len(c.Env))
	}
	for i, expected := range opts.Env {
		if c.Env[i].Name != expected.Name || c.Env[i].Value != expected.Value {
			t.Fatalf("env[%d]: expected %s=%s, got %s=%s", i, expected.Name, expected.Value, c.Env[i].Name, c.Env[i].Value)
		}
	}

	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyOnFailure {
		t.Fatalf("expected RestartPolicy %q, got %q", corev1.RestartPolicyOnFailure, job.Spec.Template.Spec.RestartPolicy)
	}
}

func TestRunDBSyncJob_Idempotent(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(ctx, t, "owner-job-idempotent")

	opts := database.DBSyncJobOpts{
		Name:      "dbsync-idempotent",
		Namespace: testNamespace,
		Image:     "myapp:latest",
		Command:   []string{"db-sync"},
	}

	if _, err := database.RunDBSyncJob(ctx, k8sClient, owner, testScheme, opts); err != nil {
		t.Fatalf("first call to RunDBSyncJob returned error: %v", err)
	}

	if _, err := database.RunDBSyncJob(ctx, k8sClient, owner, testScheme, opts); err != nil {
		t.Fatalf("second call to RunDBSyncJob returned error: %v", err)
	}
}

func TestRunDBSyncJob_OwnerRef(t *testing.T) {
	ctx := context.Background()
	owner := newOwner(ctx, t, "owner-job-ref")

	opts := database.DBSyncJobOpts{
		Name:      "dbsync-ownerref",
		Namespace: testNamespace,
		Image:     "myapp:latest",
		Command:   []string{"db-sync"},
	}

	if _, err := database.RunDBSyncJob(ctx, k8sClient, owner, testScheme, opts); err != nil {
		t.Fatalf("RunDBSyncJob returned error: %v", err)
	}

	job := &batchv1.Job{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: opts.Name, Namespace: testNamespace}, job); err != nil {
		t.Fatalf("failed to get Job: %v", err)
	}

	ownerRefs := job.GetOwnerReferences()
	if len(ownerRefs) == 0 {
		t.Fatal("expected at least one owner reference, got none")
	}

	found := false
	for _, ref := range ownerRefs {
		if ref.UID == owner.UID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected owner reference with UID %s, not found in %v", owner.UID, ownerRefs)
	}
}
