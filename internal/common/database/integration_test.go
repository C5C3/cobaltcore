// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package database

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	envtestutil "github.com/c5c3/forge/internal/common/testutil/envtest"
	"github.com/c5c3/forge/internal/common/testutil/simulators"
)

// Feature: CC-0005

// createNamespace creates a unique namespace in the cluster for test isolation.
func createNamespace(ctx context.Context, g *GomegaWithT, c client.Client, name string) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())
	return ns
}

// createOwner creates a ConfigMap via the API server so it gets a real UID assigned.
func createOwner(ctx context.Context, g *GomegaWithT, c client.Client, namespace string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-owner",
			Namespace: namespace,
		},
	}
	g.Expect(c.Create(ctx, cm)).To(Succeed())
	return cm
}

// ---------------------------------------------------------------------------
// EnsureDatabase integration tests
// ---------------------------------------------------------------------------

func TestIntegration_EnsureDatabase_CreatesInCluster(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := createNamespace(ctx, g, c, "test-db-create")
	owner := createOwner(ctx, g, c, ns.Name)

	desired := &mariadbv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-db",
			Namespace: ns.Name,
		},
		Spec: mariadbv1alpha1.DatabaseSpec{
			MariaDBRef: mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: "mariadb-primary"},
			},
			CharacterSet: "utf8mb4",
			Collate:      "utf8mb4_unicode_ci",
		},
	}

	ready, err := EnsureDatabase(ctx, c, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "newly created Database should not be ready")

	// Verify the Database exists in the cluster.
	created := &mariadbv1alpha1.Database{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "test-db", Namespace: ns.Name}, created)).To(Succeed())

	// Verify spec correctness.
	g.Expect(created.Spec.MariaDBRef.Name).To(Equal("mariadb-primary"))
	g.Expect(created.Spec.CharacterSet).To(Equal("utf8mb4"))
	g.Expect(created.Spec.Collate).To(Equal("utf8mb4_unicode_ci"))

	// Verify owner reference is set.
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("test-owner"))
	g.Expect(created.OwnerReferences[0].UID).To(Equal(owner.UID))
}

func TestIntegration_EnsureDatabase_UpdatesExisting(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := createNamespace(ctx, g, c, "test-db-update")
	owner := createOwner(ctx, g, c, ns.Name)

	// Create initial Database.
	initial := &mariadbv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-db",
			Namespace: ns.Name,
		},
		Spec: mariadbv1alpha1.DatabaseSpec{
			MariaDBRef: mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: "mariadb-primary"},
			},
			CharacterSet: "utf8",
			Collate:      "utf8_general_ci",
		},
	}

	_, err := EnsureDatabase(ctx, c, owner, initial)
	g.Expect(err).NotTo(HaveOccurred())

	// Update with different charset.
	updated := &mariadbv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-db",
			Namespace: ns.Name,
		},
		Spec: mariadbv1alpha1.DatabaseSpec{
			MariaDBRef: mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: "mariadb-primary"},
			},
			CharacterSet: "utf8mb4",
			Collate:      "utf8mb4_unicode_ci",
		},
	}

	_, err = EnsureDatabase(ctx, c, owner, updated)
	g.Expect(err).NotTo(HaveOccurred())

	// Verify the Database was updated.
	result := &mariadbv1alpha1.Database{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "test-db", Namespace: ns.Name}, result)).To(Succeed())
	g.Expect(result.Spec.CharacterSet).To(Equal("utf8mb4"))
	g.Expect(result.Spec.Collate).To(Equal("utf8mb4_unicode_ci"))
}

func TestIntegration_EnsureDatabase_DetectsReady(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := createNamespace(ctx, g, c, "test-db-ready")
	owner := createOwner(ctx, g, c, ns.Name)

	desired := &mariadbv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-db",
			Namespace: ns.Name,
		},
		Spec: mariadbv1alpha1.DatabaseSpec{
			MariaDBRef: mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: "mariadb-primary"},
			},
		},
	}

	// Create the Database first.
	ready, err := EnsureDatabase(ctx, c, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())

	// Simulate readiness using the typed simulator.
	g.Expect(simulators.SimulateDatabaseReady(ctx, c, types.NamespacedName{
		Name: "test-db", Namespace: ns.Name,
	})).To(Succeed())

	// Call EnsureDatabase again — should now detect readiness.
	ready, err = EnsureDatabase(ctx, c, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue(), "Database with Ready=True should be detected as ready")
}

// ---------------------------------------------------------------------------
// EnsureDatabaseUser integration tests
// ---------------------------------------------------------------------------

func TestIntegration_EnsureDatabaseUser_CreatesInCluster(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := createNamespace(ctx, g, c, "test-dbuser-create")
	owner := createOwner(ctx, g, c, ns.Name)

	user := &mariadbv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-user",
			Namespace: ns.Name,
		},
		Spec: mariadbv1alpha1.UserSpec{
			MariaDBRef: mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: "mariadb-primary"},
			},
			PasswordSecretKeyRef: &mariadbv1alpha1.SecretKeySelector{
				LocalObjectReference: mariadbv1alpha1.LocalObjectReference{Name: "user-password"},
				Key:                  "password",
			},
			MaxUserConnections: 20,
		},
	}

	grant := &mariadbv1alpha1.Grant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-grant",
			Namespace: ns.Name,
		},
		Spec: mariadbv1alpha1.GrantSpec{
			MariaDBRef: mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: "mariadb-primary"},
			},
			Privileges: []string{"SELECT", "INSERT"},
			Database:   "mydb",
			Table:      "*",
			Username:   "test-user",
		},
	}

	ready, err := EnsureDatabaseUser(ctx, c, owner, user, grant)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "newly created User/Grant should not be ready")

	// Verify the User exists with owner reference.
	createdUser := &mariadbv1alpha1.User{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "test-user", Namespace: ns.Name}, createdUser)).To(Succeed())
	g.Expect(createdUser.Spec.MariaDBRef.Name).To(Equal("mariadb-primary"))
	g.Expect(createdUser.Spec.MaxUserConnections).To(Equal(int32(20)))
	g.Expect(createdUser.OwnerReferences).To(HaveLen(1))
	g.Expect(createdUser.OwnerReferences[0].UID).To(Equal(owner.UID))

	// Verify the Grant exists with owner reference.
	createdGrant := &mariadbv1alpha1.Grant{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "test-grant", Namespace: ns.Name}, createdGrant)).To(Succeed())
	g.Expect(createdGrant.Spec.Privileges).To(ConsistOf("SELECT", "INSERT"))
	g.Expect(createdGrant.Spec.Database).To(Equal("mydb"))
	g.Expect(createdGrant.Spec.Username).To(Equal("test-user"))
	g.Expect(createdGrant.OwnerReferences).To(HaveLen(1))
	g.Expect(createdGrant.OwnerReferences[0].UID).To(Equal(owner.UID))
}

func TestIntegration_EnsureDatabaseUser_DetectsReady(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := createNamespace(ctx, g, c, "test-dbuser-ready")
	owner := createOwner(ctx, g, c, ns.Name)

	user := &mariadbv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-user",
			Namespace: ns.Name,
		},
		Spec: mariadbv1alpha1.UserSpec{
			MariaDBRef: mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: "mariadb-primary"},
			},
			PasswordSecretKeyRef: &mariadbv1alpha1.SecretKeySelector{
				LocalObjectReference: mariadbv1alpha1.LocalObjectReference{Name: "user-password"},
				Key:                  "password",
			},
		},
	}

	grant := &mariadbv1alpha1.Grant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-grant",
			Namespace: ns.Name,
		},
		Spec: mariadbv1alpha1.GrantSpec{
			MariaDBRef: mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: "mariadb-primary"},
			},
			Privileges: []string{"ALL"},
			Database:   "mydb",
			Table:      "*",
			Username:   "test-user",
		},
	}

	// Create User and Grant.
	ready, err := EnsureDatabaseUser(ctx, c, owner, user, grant)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())

	// Simulate only User ready — should still return false.
	g.Expect(simulators.SimulateUserReady(ctx, c, types.NamespacedName{
		Name: "test-user", Namespace: ns.Name,
	})).To(Succeed())

	ready, err = EnsureDatabaseUser(ctx, c, owner, user, grant)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "User ready but Grant not ready — should return false")

	// Simulate Grant ready — now both should be ready.
	g.Expect(simulators.SimulateGrantReady(ctx, c, types.NamespacedName{
		Name: "test-grant", Namespace: ns.Name,
	})).To(Succeed())

	ready, err = EnsureDatabaseUser(ctx, c, owner, user, grant)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue(), "both User and Grant ready — should return true")
}

// ---------------------------------------------------------------------------
// RunDBSyncJob integration tests
// ---------------------------------------------------------------------------

func TestIntegration_RunDBSyncJob_CreatesInCluster(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := createNamespace(ctx, g, c, "test-dbsync-create")
	owner := createOwner(ctx, g, c, ns.Name)

	desired := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "db-sync",
			Namespace: ns.Name,
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

	completed, err := RunDBSyncJob(ctx, c, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(completed).To(BeFalse(), "newly created Job should not be complete")

	// Verify the Job exists in the cluster.
	fetched := &batchv1.Job{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "db-sync", Namespace: ns.Name}, fetched)).To(Succeed())

	// Verify spec.
	g.Expect(fetched.Spec.Template.Spec.Containers).NotTo(BeEmpty())
	g.Expect(fetched.Spec.Template.Spec.Containers[0].Image).To(Equal("myapp/db-migrate:latest"))

	// Verify owner reference.
	g.Expect(fetched.OwnerReferences).NotTo(BeEmpty())
	g.Expect(fetched.OwnerReferences[0].UID).To(Equal(owner.UID))
	g.Expect(fetched.OwnerReferences[0].Name).To(Equal("test-owner"))
}

func TestIntegration_RunDBSyncJob_DetectsCompleted(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	ns := createNamespace(ctx, g, c, "test-dbsync-complete")
	owner := createOwner(ctx, g, c, ns.Name)

	// Create a Job manually first.
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "completed-sync",
			Namespace: ns.Name,
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
	g.Expect(c.Create(ctx, job)).To(Succeed())

	// Patch the Job status to Succeeded=1.
	job.Status.Succeeded = 1
	g.Expect(c.Status().Update(ctx, job)).To(Succeed())

	// Call RunDBSyncJob with the same name — should detect completion.
	desired := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "completed-sync",
			Namespace: ns.Name,
		},
		Spec: job.Spec,
	}

	completed, err := RunDBSyncJob(ctx, c, owner, desired)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(completed).To(BeTrue(), "Job with Succeeded=1 should be detected as complete")
}
