// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/forge/internal/common/job"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// Feature: CC-0013

func dbTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = keystonev1alpha1.AddToScheme(s)
	_ = mariadbv1alpha1.AddToScheme(s)
	return s
}

func managedKeystone() *keystonev1alpha1.Keystone {
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
			Database: commonv1.DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"},
				Database:   "keystone",
				SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db-credentials"},
			},
			Cache: commonv1.CacheSpec{Backend: "dogpile.cache.pymemcache", Servers: []string{"mc:11211"}},
			Bootstrap: keystonev1alpha1.BootstrapSpec{
				AdminUser:              "admin",
				AdminPasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin"},
				Region:                 "RegionOne",
			},
		},
	}
}

func brownfieldKeystone() *keystonev1alpha1.Keystone {
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
			Database: commonv1.DatabaseSpec{
				Host:      "db.example.com",
				Port:      3306,
				Database:  "keystone",
				SecretRef: commonv1.SecretRefSpec{Name: "keystone-db-credentials"},
			},
			Cache: commonv1.CacheSpec{Backend: "dogpile.cache.pymemcache", Servers: []string{"mc:11211"}},
			Bootstrap: keystonev1alpha1.BootstrapSpec{
				AdminUser:              "admin",
				AdminPasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin"},
				Region:                 "RegionOne",
			},
		},
	}
}

func newDBTestReconciler(s *runtime.Scheme, objs ...client.Object) *KeystoneReconciler {
	cb := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...)
	cb = cb.WithStatusSubresource(&keystonev1alpha1.Keystone{}, &mariadbv1alpha1.Database{}, &mariadbv1alpha1.User{}, &mariadbv1alpha1.Grant{})
	return &KeystoneReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}
}

// completedDBSyncJob returns a db_sync Job that matches what buildDBSyncJob produces
// for the given keystone and is marked as complete with the correct pod-spec hash.
func completedDBSyncJob(ks *keystonev1alpha1.Keystone) *batchv1.Job {
	desired := buildDBSyncJob(ks, "keystone-config-abc123")
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

// failedDBSyncJob returns a db_sync Job that is marked as permanently failed.
func failedDBSyncJob(ks *keystonev1alpha1.Keystone) *batchv1.Job {
	desired := buildDBSyncJob(ks, "keystone-config-abc123")
	j := desired.DeepCopy()
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template.Spec),
	}
	j.Status.Failed = 5
	j.Status.Conditions = []batchv1.JobCondition{
		{
			Type:   batchv1.JobFailed,
			Status: corev1.ConditionTrue,
		},
	}
	return j
}

// readyDatabase returns a MariaDB Database CR with Ready=True.
func readyDatabase(ks *keystonev1alpha1.Keystone) *mariadbv1alpha1.Database {
	db := buildDatabase(ks)
	meta.SetStatusCondition(&db.Status.Conditions, metav1.Condition{
		Type:   "Ready",
		Status: metav1.ConditionTrue,
		Reason: "Created",
	})
	return db
}

// readyUser returns a MariaDB User CR with Ready=True.
func readyUser(ks *keystonev1alpha1.Keystone) *mariadbv1alpha1.User {
	u := buildUser(ks)
	meta.SetStatusCondition(&u.Status.Conditions, metav1.Condition{
		Type:   "Ready",
		Status: metav1.ConditionTrue,
		Reason: "Created",
	})
	return u
}

// readyGrant returns a MariaDB Grant CR with Ready=True.
func readyGrant(ks *keystonev1alpha1.Keystone) *mariadbv1alpha1.Grant {
	g := buildGrant(ks)
	meta.SetStatusCondition(&g.Status.Conditions, metav1.Condition{
		Type:   "Ready",
		Status: metav1.ConditionTrue,
		Reason: "Created",
	})
	return g
}

// --- Managed mode tests ---

func TestReconcileDatabase_Managed_AllReady_DatabaseSynced(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := managedKeystone()

	r := newDBTestReconciler(s, ks,
		readyDatabase(ks),
		readyUser(ks),
		readyGrant(ks),
		completedDBSyncJob(ks),
	)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("DatabaseSynced"))
}

func TestReconcileDatabase_Managed_DatabaseNotReady_Requeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := managedKeystone()

	// Database CR exists but is not ready (no Ready condition).
	db := buildDatabase(ks)
	r := newDBTestReconciler(s, ks, db)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForDatabase"))
}

func TestReconcileDatabase_Managed_UserNotReady_Requeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := managedKeystone()

	// Database is ready, but User is not.
	r := newDBTestReconciler(s, ks,
		readyDatabase(ks),
		buildUser(ks), // exists but not ready
	)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForDatabase"))
}

// --- Brownfield mode tests ---

func TestReconcileDatabase_Brownfield_DBSyncComplete_DatabaseSynced(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	r := newDBTestReconciler(s, ks, completedDBSyncJob(ks))

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("DatabaseSynced"))

	// Verify no MariaDB CRs were created.
	dbList := &mariadbv1alpha1.DatabaseList{}
	g.Expect(r.Client.List(context.Background(), dbList, client.InNamespace("default"))).To(Succeed())
	g.Expect(dbList.Items).To(BeEmpty())
}

// --- db_sync Job state tests ---

func TestReconcileDatabase_DBSyncRunning_Requeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	// Job exists but is not completed (still running).
	syncJob := buildDBSyncJob(ks, "keystone-config-abc123")
	syncJob.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&syncJob.Spec.Template.Spec),
	}
	r := newDBTestReconciler(s, ks, syncJob)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("DBSyncInProgress"))
}

func TestReconcileDatabase_DBSyncFailed_ReturnsError(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	r := newDBTestReconciler(s, ks, failedDBSyncJob(ks))

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("db_sync"))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("DBSyncFailed"))
}

func TestReconcileDatabase_Brownfield_SkipsMariaDBCRs_CreatesDBSyncJob(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	// No pre-existing Job — should create one and requeue.
	r := newDBTestReconciler(s, ks)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))

	// Verify no MariaDB CRs were created.
	dbList := &mariadbv1alpha1.DatabaseList{}
	g.Expect(r.Client.List(context.Background(), dbList, client.InNamespace("default"))).To(Succeed())
	g.Expect(dbList.Items).To(BeEmpty())

	userList := &mariadbv1alpha1.UserList{}
	g.Expect(r.Client.List(context.Background(), userList, client.InNamespace("default"))).To(Succeed())
	g.Expect(userList.Items).To(BeEmpty())

	grantList := &mariadbv1alpha1.GrantList{}
	g.Expect(r.Client.List(context.Background(), grantList, client.InNamespace("default"))).To(Succeed())
	g.Expect(grantList.Items).To(BeEmpty())

	// Verify the db_sync Job was created.
	var syncJob batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-db-sync",
		Namespace: "default",
	}, &syncJob)).To(Succeed())
	g.Expect(syncJob.Annotations).To(HaveKey(job.PodSpecHashAnnotation))

	// Verify config volume mount is present (CC-0013: db_sync needs keystone.conf for DB connection).
	container := syncJob.Spec.Template.Spec.Containers[0]
	g.Expect(container.VolumeMounts).To(HaveLen(1))
	g.Expect(container.VolumeMounts[0].Name).To(Equal("config"))
	g.Expect(container.VolumeMounts[0].MountPath).To(Equal("/etc/keystone/keystone.conf.d/"))
	g.Expect(container.VolumeMounts[0].ReadOnly).To(BeTrue())

	// Verify config volume references the ConfigMap.
	g.Expect(syncJob.Spec.Template.Spec.Volumes).To(HaveLen(1))
	g.Expect(syncJob.Spec.Template.Spec.Volumes[0].Name).To(Equal("config"))
	g.Expect(syncJob.Spec.Template.Spec.Volumes[0].ConfigMap.Name).To(Equal("keystone-config-abc123"))

	// Verify condition is DBSyncInProgress.
	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("DBSyncInProgress"))
	g.Expect(cond.Message).To(Equal("db_sync job is running"))
}

// TestBuildDBSyncJob_SecurityContext verifies that the db-sync container in the
// Job returned by buildDBSyncJob has the correct SecurityContext with all four
// PSS Restricted profile fields (CC-0045, REQ-001 through REQ-004).
func TestBuildDBSyncJob_SecurityContext(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := brownfieldKeystone()

	job := buildDBSyncJob(ks, "keystone-config-abc123")

	container := findContainerByName(job.Spec.Template.Spec.Containers, "db-sync")
	expectRestrictedSecurityContext(g, container)
}

func TestReconcileDatabase_StaleDBSyncJob_Recreated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	// Create a completed Job with a stale hash.
	staleJob := completedDBSyncJob(ks)
	staleJob.Annotations[job.PodSpecHashAnnotation] = "stale-hash-from-previous-image"

	r := newDBTestReconciler(s, ks, staleJob)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))

	// Verify the old Job was deleted and a new one created with the correct hash.
	var newJob batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-db-sync",
		Namespace: "default",
	}, &newJob)).To(Succeed())

	desired := buildDBSyncJob(ks, "keystone-config-abc123")
	expectedHash := job.PodSpecHash(&desired.Spec.Template.Spec)
	g.Expect(newJob.Annotations[job.PodSpecHashAnnotation]).To(Equal(expectedHash))
}

func TestReconcileDatabase_Managed_ConditionMessages(t *testing.T) {
	s := dbTestScheme()

	t.Run("DatabaseNotReady", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ks := managedKeystone()
		db := buildDatabase(ks)
		r := newDBTestReconciler(s, ks, db)

		_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
		g.Expect(err).NotTo(HaveOccurred())

		cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Message).To(Equal("MariaDB Database CR is not ready"))
		g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
	})

	t.Run("UserNotReady", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ks := managedKeystone()
		r := newDBTestReconciler(s, ks,
			readyDatabase(ks),
			buildUser(ks), // exists but not ready
		)

		_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
		g.Expect(err).NotTo(HaveOccurred())

		cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Message).To(Equal("MariaDB User or Grant CR is not ready"))
		g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
	})

	t.Run("DatabaseAndGrantUseSpecDatabaseName", func(t *testing.T) {
		// Verify that buildDatabase and buildGrant use spec.database.database
		// (not metadata.name) so the MariaDB database name, Grant, and connection
		// URL all target the same database (CC-0013).
		g := NewGomegaWithT(t)
		ks := managedKeystone()
		// Deliberately use a CR name that differs from spec.database.database.
		ks.Name = "keystone-prod"
		ks.Spec.Database.Database = "keystone"

		db := buildDatabase(ks)
		g.Expect(db.Spec.Name).To(Equal("keystone"),
			"Database CR Spec.Name must match spec.database.database, not metadata.name")

		grant := buildGrant(ks)
		g.Expect(grant.Spec.Database).To(Equal("keystone"),
			"Grant.Database must match spec.database.database, not metadata.name")
	})

	t.Run("DatabaseSynced", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ks := managedKeystone()
		r := newDBTestReconciler(s, ks,
			readyDatabase(ks),
			readyUser(ks),
			readyGrant(ks),
			completedDBSyncJob(ks),
		)

		_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
		g.Expect(err).NotTo(HaveOccurred())

		cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Message).To(Equal("Database schema is up to date"))
		g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
	})
}
