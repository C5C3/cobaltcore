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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = mariadbv1alpha1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	return s
}

func testOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-owner",
			Namespace: "default",
			UID:       "test-uid",
		},
	}
}

func testDatabase() *mariadbv1alpha1.Database {
	return &mariadbv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-db",
			Namespace: "default",
		},
		Spec: mariadbv1alpha1.DatabaseSpec{
			MariaDBRef: mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: "mariadb"},
			},
			CharacterSet: "utf8",
			Collate:      "utf8_general_ci",
			Name:         "mydb",
		},
	}
}

func testUser() *mariadbv1alpha1.User {
	return &mariadbv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-user",
			Namespace: "default",
		},
		Spec: mariadbv1alpha1.UserSpec{
			MariaDBRef: mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: "mariadb"},
			},
			PasswordSecretKeyRef: &mariadbv1alpha1.SecretKeySelector{
				LocalObjectReference: mariadbv1alpha1.LocalObjectReference{Name: "user-secret"},
				Key:                  "password",
			},
			MaxUserConnections: 10,
			Name:               "appuser",
			Host:               "%",
		},
	}
}

func testGrant() *mariadbv1alpha1.Grant {
	host := "%"
	return &mariadbv1alpha1.Grant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-grant",
			Namespace: "default",
		},
		Spec: mariadbv1alpha1.GrantSpec{
			MariaDBRef: mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: "mariadb"},
			},
			Privileges: []string{"ALL PRIVILEGES"},
			Database:   "mydb",
			Table:      "*",
			Username:   "appuser",
			Host:       &host,
		},
	}
}

// --- EnsureDatabase ---

func TestEnsureDatabase_creates(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build()

	ready, err := EnsureDatabase(context.Background(), c, s, owner, testDatabase())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "newly created database should not be ready")

	created := &mariadbv1alpha1.Database{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-db", Namespace: "default"}, created)).To(Succeed())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("test-owner"))
}

func TestEnsureDatabase_existingNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	db := testDatabase()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, db).
		WithStatusSubresource(db).
		Build()

	ready, err := EnsureDatabase(context.Background(), c, s, owner, testDatabase())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
}

func TestEnsureDatabase_existingReady(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	db := testDatabase()
	meta.SetStatusCondition(&db.Status.Conditions, metav1.Condition{
		Type:   "Ready",
		Status: metav1.ConditionTrue,
		Reason: "Created",
	})

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, db).
		WithStatusSubresource(db).
		Build()

	ready, err := EnsureDatabase(context.Background(), c, s, owner, testDatabase())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
}

func TestEnsureDatabase_updates(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	existing := testDatabase()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		WithStatusSubresource(existing).
		Build()

	updated := testDatabase()
	updated.Spec.CharacterSet = "utf8mb4"

	ready, err := EnsureDatabase(context.Background(), c, s, owner, updated)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())

	fetched := &mariadbv1alpha1.Database{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	g.Expect(fetched.Spec.CharacterSet).To(Equal("utf8mb4"))
}

func TestEnsureDatabase_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build()

	ctx := context.Background()
	_, err := EnsureDatabase(ctx, c, s, owner, testDatabase())
	g.Expect(err).NotTo(HaveOccurred())
	_, err = EnsureDatabase(ctx, c, s, owner, testDatabase())
	g.Expect(err).NotTo(HaveOccurred())

	list := &mariadbv1alpha1.DatabaseList{}
	g.Expect(c.List(ctx, list, client.InNamespace("default"))).To(Succeed())
	g.Expect(list.Items).To(HaveLen(1))
}

// --- IsDatabaseReady ---

func TestIsDatabaseReady_true(t *testing.T) {
	g := NewGomegaWithT(t)
	db := &mariadbv1alpha1.Database{}
	meta.SetStatusCondition(&db.Status.Conditions, metav1.Condition{
		Type:   "Ready",
		Status: metav1.ConditionTrue,
		Reason: "Created",
	})
	g.Expect(IsDatabaseReady(db)).To(BeTrue())
}

func TestIsDatabaseReady_false_noConditions(t *testing.T) {
	g := NewGomegaWithT(t)
	db := &mariadbv1alpha1.Database{}
	g.Expect(IsDatabaseReady(db)).To(BeFalse())
}

func TestIsDatabaseReady_false_notTrue(t *testing.T) {
	g := NewGomegaWithT(t)
	db := &mariadbv1alpha1.Database{}
	meta.SetStatusCondition(&db.Status.Conditions, metav1.Condition{
		Type:   "Ready",
		Status: metav1.ConditionFalse,
		Reason: "Pending",
	})
	g.Expect(IsDatabaseReady(db)).To(BeFalse())
}

// --- EnsureDatabaseUser ---

func TestEnsureDatabaseUser_creates_userOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build()

	ready, err := EnsureDatabaseUser(context.Background(), c, s, owner, testUser(), testGrant())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "newly created user should not be ready")

	// User should be created.
	createdUser := &mariadbv1alpha1.User{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "test-user", Namespace: "default"}, createdUser)).To(Succeed())
	g.Expect(createdUser.OwnerReferences).To(HaveLen(1))
	g.Expect(createdUser.OwnerReferences[0].Name).To(Equal("test-owner"))

	// Grant should NOT be created because the User is not yet ready.
	// The MariaDB operator requires the MySQL-level user to exist before
	// a GRANT can succeed.
	createdGrant := &mariadbv1alpha1.Grant{}
	err = c.Get(context.Background(), client.ObjectKey{Name: "test-grant", Namespace: "default"}, createdGrant)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "grant should not be created when user is not ready")
}

func TestEnsureDatabaseUser_existingNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	user := testUser()
	grant := testGrant()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, user, grant).
		WithStatusSubresource(user, grant).
		Build()

	ready, err := EnsureDatabaseUser(context.Background(), c, s, owner, testUser(), testGrant())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
}

func TestEnsureDatabaseUser_existingReady(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	user := testUser()
	meta.SetStatusCondition(&user.Status.Conditions, metav1.Condition{
		Type:   "Ready",
		Status: metav1.ConditionTrue,
		Reason: "Created",
	})

	grant := testGrant()
	meta.SetStatusCondition(&grant.Status.Conditions, metav1.Condition{
		Type:   "Ready",
		Status: metav1.ConditionTrue,
		Reason: "Created",
	})

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, user, grant).
		WithStatusSubresource(user, grant).
		Build()

	ready, err := EnsureDatabaseUser(context.Background(), c, s, owner, testUser(), testGrant())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
}

func TestEnsureDatabaseUser_partialReady(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	user := testUser()
	meta.SetStatusCondition(&user.Status.Conditions, metav1.Condition{
		Type:   "Ready",
		Status: metav1.ConditionTrue,
		Reason: "Created",
	})

	grant := testGrant()
	// Grant is NOT ready

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, user, grant).
		WithStatusSubresource(user, grant).
		Build()

	ready, err := EnsureDatabaseUser(context.Background(), c, s, owner, testUser(), testGrant())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
}

func TestEnsureDatabaseUser_updates_userOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()
	existingUser := testUser()
	existingGrant := testGrant()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existingUser, existingGrant).
		WithStatusSubresource(existingUser, existingGrant).
		Build()

	updatedUser := testUser()
	updatedUser.Spec.MaxUserConnections = 20
	updatedGrant := testGrant()
	updatedGrant.Spec.Privileges = []string{"SELECT", "INSERT"}

	ready, err := EnsureDatabaseUser(context.Background(), c, s, owner, updatedUser, updatedGrant)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())

	// User spec should be updated.
	fetchedUser := &mariadbv1alpha1.User{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existingUser), fetchedUser)).To(Succeed())
	g.Expect(fetchedUser.Spec.MaxUserConnections).To(Equal(int32(20)))

	// Grant spec should NOT be updated because user is not ready.
	fetchedGrant := &mariadbv1alpha1.Grant{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existingGrant), fetchedGrant)).To(Succeed())
	g.Expect(fetchedGrant.Spec.Privileges).To(Equal([]string{"ALL PRIVILEGES"}), "grant should remain unchanged when user is not ready")
}

func TestEnsureDatabaseUser_updatesGrant_whenUserReady(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	existingUser := testUser()
	meta.SetStatusCondition(&existingUser.Status.Conditions, metav1.Condition{
		Type:   "Ready",
		Status: metav1.ConditionTrue,
		Reason: "Created",
	})

	existingGrant := testGrant()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existingUser, existingGrant).
		WithStatusSubresource(existingUser, existingGrant).
		Build()

	updatedGrant := testGrant()
	updatedGrant.Spec.Privileges = []string{"SELECT", "INSERT"}

	ready, err := EnsureDatabaseUser(context.Background(), c, s, owner, testUser(), updatedGrant)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())

	// Grant spec should be updated because user is ready.
	fetchedGrant := &mariadbv1alpha1.Grant{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existingGrant), fetchedGrant)).To(Succeed())
	g.Expect(fetchedGrant.Spec.Privileges).To(Equal([]string{"SELECT", "INSERT"}))
}

func TestEnsureDatabaseUser_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build()

	ctx := context.Background()
	_, err := EnsureDatabaseUser(ctx, c, s, owner, testUser(), testGrant())
	g.Expect(err).NotTo(HaveOccurred())
	_, err = EnsureDatabaseUser(ctx, c, s, owner, testUser(), testGrant())
	g.Expect(err).NotTo(HaveOccurred())

	userList := &mariadbv1alpha1.UserList{}
	g.Expect(c.List(ctx, userList, client.InNamespace("default"))).To(Succeed())
	g.Expect(userList.Items).To(HaveLen(1))

	// Grant should not be created because user is never ready.
	grantList := &mariadbv1alpha1.GrantList{}
	g.Expect(c.List(ctx, grantList, client.InNamespace("default"))).To(Succeed())
	g.Expect(grantList.Items).To(BeEmpty())
}

// --- IsUserReady / IsGrantReady ---

func TestIsUserReady_true(t *testing.T) {
	g := NewGomegaWithT(t)
	user := &mariadbv1alpha1.User{}
	meta.SetStatusCondition(&user.Status.Conditions, metav1.Condition{
		Type:   "Ready",
		Status: metav1.ConditionTrue,
		Reason: "Created",
	})
	g.Expect(IsUserReady(user)).To(BeTrue())
}

func TestIsUserReady_false(t *testing.T) {
	g := NewGomegaWithT(t)
	user := &mariadbv1alpha1.User{}
	g.Expect(IsUserReady(user)).To(BeFalse())
}

func TestIsGrantReady_true(t *testing.T) {
	g := NewGomegaWithT(t)
	grant := &mariadbv1alpha1.Grant{}
	meta.SetStatusCondition(&grant.Status.Conditions, metav1.Condition{
		Type:   "Ready",
		Status: metav1.ConditionTrue,
		Reason: "Created",
	})
	g.Expect(IsGrantReady(grant)).To(BeTrue())
}

func TestIsGrantReady_false(t *testing.T) {
	g := NewGomegaWithT(t)
	grant := &mariadbv1alpha1.Grant{}
	g.Expect(IsGrantReady(grant)).To(BeFalse())
}
