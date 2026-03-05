// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Feature: CC-0005

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = mariadbv1alpha1.AddToScheme(s)
	return s
}

func testClient(scheme *runtime.Scheme, objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(
			&mariadbv1alpha1.Database{},
			&mariadbv1alpha1.User{},
			&mariadbv1alpha1.Grant{},
			&batchv1.Job{},
		).
		Build()
}

func testOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-owner",
			Namespace: "default",
			UID:       types.UID("owner-uid-1234"),
		},
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
	}
}

func desiredDatabase(name, namespace, mariadbName string) *mariadbv1alpha1.Database {
	return &mariadbv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: mariadbv1alpha1.DatabaseSpec{
			MariaDBRef: mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: mariadbName},
			},
			CharacterSet: "utf8mb4",
			Collate:      "utf8mb4_unicode_ci",
		},
	}
}

func desiredUser(name, namespace, mariadbName, passwordSecretName string) *mariadbv1alpha1.User {
	return &mariadbv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: mariadbv1alpha1.UserSpec{
			MariaDBRef: mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: mariadbName},
			},
			PasswordSecretKeyRef: &mariadbv1alpha1.SecretKeySelector{
				LocalObjectReference: mariadbv1alpha1.LocalObjectReference{Name: passwordSecretName},
				Key:                  "password",
			},
			MaxUserConnections: 20,
		},
	}
}

func desiredGrant(name, namespace, mariadbName, database, username string, privileges []string) *mariadbv1alpha1.Grant {
	return &mariadbv1alpha1.Grant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: mariadbv1alpha1.GrantSpec{
			MariaDBRef: mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: mariadbName},
			},
			Privileges: privileges,
			Database:   database,
			Table:      "*",
			Username:   username,
		},
	}
}

func desiredDBSyncJob(name, namespace string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "db-sync",
							Image:   "myapp/db-migrate:latest",
							Command: []string{"migrate", "up"},
						},
					},
				},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// EnsureDatabase tests
// ---------------------------------------------------------------------------

func TestEnsureDatabase_createsDatabase(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := testScheme()
	c := testClient(scheme)
	ctx := context.Background()
	owner := testOwner()

	desired := desiredDatabase("test-db", "default", "mariadb-primary")

	ready, err := EnsureDatabase(ctx, c, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "newly created Database should not be ready")

	// Verify the Database was created.
	created := &mariadbv1alpha1.Database{}
	err = c.Get(ctx, client.ObjectKeyFromObject(desired), created)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created.Spec.MariaDBRef.Name).To(Equal("mariadb-primary"))
	g.Expect(created.Spec.CharacterSet).To(Equal("utf8mb4"))
	g.Expect(created.Spec.Collate).To(Equal("utf8mb4_unicode_ci"))
}

func TestEnsureDatabase_setsOwnerReference(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := testScheme()
	c := testClient(scheme)
	ctx := context.Background()
	owner := testOwner()

	desired := desiredDatabase("test-db", "default", "mariadb-primary")

	_, err := EnsureDatabase(ctx, c, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	created := &mariadbv1alpha1.Database{}
	err = c.Get(ctx, client.ObjectKeyFromObject(desired), created)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("test-owner"))
	g.Expect(created.OwnerReferences[0].UID).To(Equal(types.UID("owner-uid-1234")))
}

func TestEnsureDatabase_updatesExisting(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := testScheme()
	c := testClient(scheme)
	ctx := context.Background()
	owner := testOwner()

	// Create initial Database.
	initial := desiredDatabase("test-db", "default", "mariadb-primary")
	_, err := EnsureDatabase(ctx, c, owner, initial)
	g.Expect(err).NotTo(HaveOccurred())

	// Update with different charset.
	updated := desiredDatabase("test-db", "default", "mariadb-primary")
	updated.Spec.CharacterSet = "utf8"
	updated.Spec.Collate = "utf8_general_ci"
	_, err = EnsureDatabase(ctx, c, owner, updated)
	g.Expect(err).NotTo(HaveOccurred())

	// Verify the update.
	result := &mariadbv1alpha1.Database{}
	err = c.Get(ctx, client.ObjectKeyFromObject(updated), result)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.Spec.CharacterSet).To(Equal("utf8"))
	g.Expect(result.Spec.Collate).To(Equal("utf8_general_ci"))
}

// ---------------------------------------------------------------------------
// EnsureDatabaseUser tests
// ---------------------------------------------------------------------------

func TestEnsureDatabaseUser_createsUserAndGrant(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := testScheme()
	c := testClient(scheme)
	ctx := context.Background()
	owner := testOwner()

	user := desiredUser("test-user", "default", "mariadb-primary", "user-password")
	grant := desiredGrant("test-grant", "default", "mariadb-primary", "mydb", "test-user", []string{"SELECT", "INSERT"})

	ready, err := EnsureDatabaseUser(ctx, c, owner, user, grant)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "newly created User/Grant should not be ready")

	// Verify the User was created.
	createdUser := &mariadbv1alpha1.User{}
	err = c.Get(ctx, types.NamespacedName{Name: "test-user", Namespace: "default"}, createdUser)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(createdUser.Spec.MariaDBRef.Name).To(Equal("mariadb-primary"))
	g.Expect(createdUser.Spec.MaxUserConnections).To(Equal(int32(20)))

	// Verify the Grant was created.
	createdGrant := &mariadbv1alpha1.Grant{}
	err = c.Get(ctx, types.NamespacedName{Name: "test-grant", Namespace: "default"}, createdGrant)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(createdGrant.Spec.Privileges).To(ConsistOf("SELECT", "INSERT"))
	g.Expect(createdGrant.Spec.Database).To(Equal("mydb"))
	g.Expect(createdGrant.Spec.Username).To(Equal("test-user"))
}

func TestEnsureDatabaseUser_setsOwnerReferenceOnBoth(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := testScheme()
	c := testClient(scheme)
	ctx := context.Background()
	owner := testOwner()

	user := desiredUser("owned-user", "default", "mariadb-primary", "user-password")
	grant := desiredGrant("owned-grant", "default", "mariadb-primary", "mydb", "owned-user", []string{"ALL"})

	_, err := EnsureDatabaseUser(ctx, c, owner, user, grant)
	g.Expect(err).NotTo(HaveOccurred())

	createdUser := &mariadbv1alpha1.User{}
	err = c.Get(ctx, types.NamespacedName{Name: "owned-user", Namespace: "default"}, createdUser)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(createdUser.OwnerReferences).To(HaveLen(1))
	g.Expect(createdUser.OwnerReferences[0].Name).To(Equal("test-owner"))

	createdGrant := &mariadbv1alpha1.Grant{}
	err = c.Get(ctx, types.NamespacedName{Name: "owned-grant", Namespace: "default"}, createdGrant)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(createdGrant.OwnerReferences).To(HaveLen(1))
	g.Expect(createdGrant.OwnerReferences[0].Name).To(Equal("test-owner"))
}

func TestEnsureDatabaseUser_updatesExisting(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := testScheme()
	c := testClient(scheme)
	ctx := context.Background()
	owner := testOwner()

	// Create initial User and Grant.
	user := desiredUser("update-user", "default", "mariadb-primary", "old-password")
	grant := desiredGrant("update-grant", "default", "mariadb-primary", "mydb", "update-user", []string{"SELECT"})
	_, err := EnsureDatabaseUser(ctx, c, owner, user, grant)
	g.Expect(err).NotTo(HaveOccurred())

	// Update with different password ref and privileges.
	updatedUser := desiredUser("update-user", "default", "mariadb-primary", "new-password")
	updatedGrant := desiredGrant("update-grant", "default", "mariadb-primary", "mydb", "update-user", []string{"SELECT", "INSERT", "UPDATE"})
	_, err = EnsureDatabaseUser(ctx, c, owner, updatedUser, updatedGrant)
	g.Expect(err).NotTo(HaveOccurred())

	// Verify the updates.
	resultUser := &mariadbv1alpha1.User{}
	err = c.Get(ctx, types.NamespacedName{Name: "update-user", Namespace: "default"}, resultUser)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(resultUser.Spec.PasswordSecretKeyRef.Name).To(Equal("new-password"))

	resultGrant := &mariadbv1alpha1.Grant{}
	err = c.Get(ctx, types.NamespacedName{Name: "update-grant", Namespace: "default"}, resultGrant)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(resultGrant.Spec.Privileges).To(ConsistOf("SELECT", "INSERT", "UPDATE"))
}

// ---------------------------------------------------------------------------
// RunDBSyncJob tests
// ---------------------------------------------------------------------------

func TestRunDBSyncJob_createsJob(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := testScheme()
	c := testClient(scheme)
	ctx := context.Background()
	owner := testOwner()

	desired := desiredDBSyncJob("db-sync", "default")

	completed, err := RunDBSyncJob(ctx, c, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(completed).To(BeFalse(), "newly created Job should not be complete")

	// Verify the Job was created.
	created := &batchv1.Job{}
	err = c.Get(ctx, types.NamespacedName{Name: "db-sync", Namespace: "default"}, created)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created.Spec.Template.Spec.Containers[0].Image).To(Equal("myapp/db-migrate:latest"))
	g.Expect(created.Spec.Template.Spec.Containers[0].Command).To(Equal([]string{"migrate", "up"}))
}

func TestRunDBSyncJob_setsOwnerReference(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := testScheme()
	c := testClient(scheme)
	ctx := context.Background()
	owner := testOwner()

	desired := desiredDBSyncJob("owned-sync", "default")

	_, err := RunDBSyncJob(ctx, c, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())

	created := &batchv1.Job{}
	err = c.Get(ctx, types.NamespacedName{Name: "owned-sync", Namespace: "default"}, created)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("test-owner"))
	g.Expect(created.OwnerReferences[0].UID).To(Equal(types.UID("owner-uid-1234")))
}

func TestRunDBSyncJob_detectsCompletedJob(t *testing.T) {
	g := NewGomegaWithT(t)
	scheme := testScheme()

	// Pre-create a completed Job.
	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "completed-sync",
			Namespace: "default",
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "db-sync",
							Image:   "myapp/db-migrate:latest",
							Command: []string{"migrate", "up"},
						},
					},
				},
			},
		},
		Status: batchv1.JobStatus{
			Succeeded: 1,
		},
	}
	c := testClient(scheme, existingJob)
	ctx := context.Background()
	owner := testOwner()

	desired := desiredDBSyncJob("completed-sync", "default")

	completed, err := RunDBSyncJob(ctx, c, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(completed).To(BeTrue(), "Job with Succeeded=1 should be detected as complete")
}

// ---------------------------------------------------------------------------
// isDatabaseReady tests
// ---------------------------------------------------------------------------

func TestIsDatabaseReady_falseWhenNoConditions(t *testing.T) {
	g := NewGomegaWithT(t)

	db := &mariadbv1alpha1.Database{}
	g.Expect(isDatabaseReady(db)).To(BeFalse())
}

func TestIsDatabaseReady_trueWhenReadyCondition(t *testing.T) {
	g := NewGomegaWithT(t)

	db := &mariadbv1alpha1.Database{
		Status: mariadbv1alpha1.DatabaseStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionTrue,
				},
			},
		},
	}
	g.Expect(isDatabaseReady(db)).To(BeTrue())
}

func TestIsUserReady_falseWhenNotReady(t *testing.T) {
	g := NewGomegaWithT(t)

	user := &mariadbv1alpha1.User{
		Status: mariadbv1alpha1.UserStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionFalse,
				},
			},
		},
	}
	g.Expect(isUserReady(user)).To(BeFalse())
}

func TestIsGrantReady_trueWhenReadyCondition(t *testing.T) {
	g := NewGomegaWithT(t)

	grant := &mariadbv1alpha1.Grant{
		Status: mariadbv1alpha1.GrantStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionTrue,
				},
			},
		},
	}
	g.Expect(isGrantReady(grant)).To(BeTrue())
}
