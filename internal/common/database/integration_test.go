// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package database

import (
	"testing"

	. "github.com/onsi/gomega"

	envtestutil "github.com/c5c3/forge/internal/common/testutil/envtest"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Feature: CC-0005

func TestIntegration_EnsureDatabase(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-db-ensure"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "db-owner", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	db := &mariadbv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "integration-db",
			Namespace: ns.Name,
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

	// Create.
	ready, err := EnsureDatabase(ctx, c, scheme, owner, db)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "newly created database should not be ready")

	created := &mariadbv1alpha1.Database{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(db), created)).To(Succeed())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("db-owner"))

	// Update character set.
	updated := db.DeepCopy()
	updated.Spec.CharacterSet = "utf8mb4"
	ready, err = EnsureDatabase(ctx, c, scheme, owner, updated)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())

	fetched := &mariadbv1alpha1.Database{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(db), fetched)).To(Succeed())
	g.Expect(fetched.Spec.CharacterSet).To(Equal("utf8mb4"))
}

func TestIntegration_EnsureDatabase_idempotent(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-db-idem"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "db-owner-idem", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	db := &mariadbv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "idem-db",
			Namespace: ns.Name,
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

	_, err := EnsureDatabase(ctx, c, scheme, owner, db)
	g.Expect(err).NotTo(HaveOccurred())
	_, err = EnsureDatabase(ctx, c, scheme, owner, db)
	g.Expect(err).NotTo(HaveOccurred())

	list := &mariadbv1alpha1.DatabaseList{}
	g.Expect(c.List(ctx, list, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(list.Items).To(HaveLen(1))
}

func TestIntegration_EnsureDatabaseUser(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-db-user"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "user-owner", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	host := "%"
	user := &mariadbv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "integration-user",
			Namespace: ns.Name,
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

	grant := &mariadbv1alpha1.Grant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "integration-grant",
			Namespace: ns.Name,
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

	// Create.
	ready, err := EnsureDatabaseUser(ctx, c, scheme, owner, user, grant)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "newly created user/grant should not be ready")

	createdUser := &mariadbv1alpha1.User{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(user), createdUser)).To(Succeed())
	g.Expect(createdUser.OwnerReferences).To(HaveLen(1))
	g.Expect(createdUser.OwnerReferences[0].Name).To(Equal("user-owner"))

	createdGrant := &mariadbv1alpha1.Grant{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(grant), createdGrant)).To(Succeed())
	g.Expect(createdGrant.OwnerReferences).To(HaveLen(1))
	g.Expect(createdGrant.OwnerReferences[0].Name).To(Equal("user-owner"))

	// Update MaxUserConnections on User.
	updatedUser := user.DeepCopy()
	updatedUser.Spec.MaxUserConnections = 20

	// Update Privileges on Grant.
	updatedGrant := grant.DeepCopy()
	updatedGrant.Spec.Privileges = []string{"SELECT", "INSERT"}

	ready, err = EnsureDatabaseUser(ctx, c, scheme, owner, updatedUser, updatedGrant)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())

	fetchedUser := &mariadbv1alpha1.User{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(user), fetchedUser)).To(Succeed())
	g.Expect(fetchedUser.Spec.MaxUserConnections).To(Equal(int32(20)))

	fetchedGrant := &mariadbv1alpha1.Grant{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(grant), fetchedGrant)).To(Succeed())
	g.Expect(fetchedGrant.Spec.Privileges).To(Equal([]string{"SELECT", "INSERT"}))
}

func TestIntegration_EnsureDatabaseUser_idempotent(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-db-user-idem"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "user-owner-idem", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	host := "%"
	user := &mariadbv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "idem-user",
			Namespace: ns.Name,
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

	grant := &mariadbv1alpha1.Grant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "idem-grant",
			Namespace: ns.Name,
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

	_, err := EnsureDatabaseUser(ctx, c, scheme, owner, user, grant)
	g.Expect(err).NotTo(HaveOccurred())
	_, err = EnsureDatabaseUser(ctx, c, scheme, owner, user, grant)
	g.Expect(err).NotTo(HaveOccurred())

	userList := &mariadbv1alpha1.UserList{}
	g.Expect(c.List(ctx, userList, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(userList.Items).To(HaveLen(1))

	grantList := &mariadbv1alpha1.GrantList{}
	g.Expect(c.List(ctx, grantList, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(grantList.Items).To(HaveLen(1))
}

func TestIntegration_RunDBSyncJob(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-db-sync"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "sync-owner", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	syncJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "integration-sync-job",
			Namespace: ns.Name,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{Name: "sync", Image: "busybox:latest", Command: []string{"echo", "sync"}},
					},
				},
			},
		},
	}

	// First call creates the Job.
	ready, err := RunDBSyncJob(ctx, c, scheme, owner, syncJob)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())

	// Verify the Job exists with owner reference.
	created := &batchv1.Job{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(syncJob), created)).To(Succeed())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("sync-owner"))

	// Second call is idempotent — returns false (not complete) without error.
	ready, err = RunDBSyncJob(ctx, c, scheme, owner, syncJob)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
}
