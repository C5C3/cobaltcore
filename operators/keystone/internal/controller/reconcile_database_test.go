// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
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
	ctrl "sigs.k8s.io/controller-runtime"
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

// --- Schema check test helpers (CC-0064) ---

// completedSchemaCheckJob returns a schema-check Job that matches what
// buildSchemaCheckJob produces for the given keystone and is marked as
// complete with the correct pod-spec hash (CC-0064).
func completedSchemaCheckJob(ks *keystonev1alpha1.Keystone) *batchv1.Job {
	desired := buildSchemaCheckJob(ks, "keystone-config-abc123")
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

// failedSchemaCheckJob returns a schema-check Job that is marked as
// permanently failed (CC-0064).
func failedSchemaCheckJob(ks *keystonev1alpha1.Keystone) *batchv1.Job {
	desired := buildSchemaCheckJob(ks, "keystone-config-abc123")
	j := desired.DeepCopy()
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template.Spec),
	}
	j.Status.Failed = 3
	j.Status.Conditions = []batchv1.JobCondition{
		{
			Type:   batchv1.JobFailed,
			Status: corev1.ConditionTrue,
		},
	}
	return j
}

// runningSchemaCheckJob returns a schema-check Job that exists but has not
// completed yet (CC-0064).
func runningSchemaCheckJob(ks *keystonev1alpha1.Keystone) *batchv1.Job {
	desired := buildSchemaCheckJob(ks, "keystone-config-abc123")
	j := desired.DeepCopy()
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template.Spec),
	}
	return j
}

// readyMariaDBCluster returns a MariaDB cluster CR with Ready=True matching
// the name referenced by ks.Spec.Database.ClusterRef (CC-0047).
func readyMariaDBCluster(ks *keystonev1alpha1.Keystone) *mariadbv1alpha1.MariaDB {
	mdb := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ks.Spec.Database.ClusterRef.Name,
			Namespace: ks.Namespace,
		},
	}
	meta.SetStatusCondition(&mdb.Status.Conditions, metav1.Condition{
		Type:   "Ready",
		Status: metav1.ConditionTrue,
		Reason: "StatefulSetReady",
	})
	return mdb
}

// notReadyMariaDBCluster returns a MariaDB cluster CR with Ready=False,
// simulating an upstream database outage (CC-0047).
func notReadyMariaDBCluster(ks *keystonev1alpha1.Keystone) *mariadbv1alpha1.MariaDB {
	mdb := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ks.Spec.Database.ClusterRef.Name,
			Namespace: ks.Namespace,
		},
	}
	meta.SetStatusCondition(&mdb.Status.Conditions, metav1.Condition{
		Type:   "Ready",
		Status: metav1.ConditionFalse,
		Reason: "StatefulSetNotReady",
	})
	return mdb
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
		readyMariaDBCluster(ks),
		readyDatabase(ks),
		readyUser(ks),
		readyGrant(ks),
		completedDBSyncJob(ks),
		completedSchemaCheckJob(ks),
	)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDatabaseSynced))

	expectEvent(g, r, "Normal DatabaseSynced")
}

func TestReconcileDatabase_Managed_DatabaseNotReady_Requeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := managedKeystone()

	// Database CR exists but is not ready (no Ready condition).
	db := buildDatabase(ks)
	r := newDBTestReconciler(s, ks, readyMariaDBCluster(ks), db)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForDatabase))

	expectNoEvent(g, r)
}

func TestReconcileDatabase_Managed_UserNotReady_Requeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := managedKeystone()

	// Database is ready, but User is not.
	r := newDBTestReconciler(s, ks,
		readyMariaDBCluster(ks),
		readyDatabase(ks),
		buildUser(ks), // exists but not ready
	)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForDatabase))

	expectNoEvent(g, r)
}

func TestReconcileDatabase_Managed_ClusterMissing_Requeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := managedKeystone()

	// No MariaDB cluster CR in the fake client — reconciler should report
	// DatabaseReady=False rather than proceeding to create the Database CR.
	r := newDBTestReconciler(s, ks)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonClusterNotReady))

	// No Database CR should be created while the cluster is unavailable.
	dbList := &mariadbv1alpha1.DatabaseList{}
	g.Expect(r.Client.List(context.Background(), dbList, client.InNamespace("default"))).To(Succeed())
	g.Expect(dbList.Items).To(BeEmpty())

	expectNoEvent(g, r)
}

func TestReconcileDatabase_Managed_ClusterNotReady_FlipsDatabaseReadyFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := managedKeystone()

	// Simulate a previously successful reconcile: DatabaseReady=True is set
	// and the db_sync Job has completed. A downstream MariaDB outage must
	// flip DatabaseReady back to False on the next reconcile (CC-0047).
	meta.SetStatusCondition(&ks.Status.Conditions, metav1.Condition{
		Type:   "DatabaseReady",
		Status: metav1.ConditionTrue,
		Reason: conditionReasonDatabaseSynced,
	})

	r := newDBTestReconciler(s, ks,
		notReadyMariaDBCluster(ks),
		readyDatabase(ks),
		readyUser(ks),
		readyGrant(ks),
		completedDBSyncJob(ks),
	)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonClusterNotReady))
	g.Expect(cond.Message).To(ContainSubstring("mariadb"))

	expectNoEvent(g, r)
}

// --- Brownfield mode tests ---

func TestReconcileDatabase_Brownfield_DBSyncComplete_DatabaseSynced(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	r := newDBTestReconciler(s, ks, completedDBSyncJob(ks), completedSchemaCheckJob(ks))

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDatabaseSynced))

	expectEvent(g, r, "Normal DatabaseSynced")

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
	g.Expect(cond.Reason).To(Equal(conditionReasonDBSyncInProgress))

	expectNoEvent(g, r)
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
	g.Expect(cond.Reason).To(Equal(conditionReasonDBSyncFailed))

	expectEvent(g, r, "Warning DBSyncFailed")
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
	g.Expect(cond.Reason).To(Equal(conditionReasonDBSyncInProgress))
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

// Feature: CC-0056

// TestBuildUpgradeJobs verifies all three upgrade-phase Job builders
// (buildExpandJob, buildMigrateJob, buildContractJob) produce the correct Job
// metadata, image, command, security context, and config volume (CC-0056).
func TestBuildUpgradeJobs(t *testing.T) {
	cases := []struct {
		name          string
		buildFunc     func(*keystonev1alpha1.Keystone, string, string) *batchv1.Job
		expectedName  string
		containerName string
		expectedFlag  string
	}{
		{"Expand", buildExpandJob, "test-keystone-db-expand", "db-expand", "--expand"},
		{"Migrate", buildMigrateJob, "test-keystone-db-migrate", "db-migrate", "--migrate"},
		{"Contract", buildContractJob, "test-keystone-db-contract", "db-contract", "--contract"},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/JobName", func(t *testing.T) {
			g := NewGomegaWithT(t)
			ks := brownfieldKeystone()
			job := tc.buildFunc(ks, "keystone-config-abc123", "2024.1")

			g.Expect(job.Name).To(Equal(tc.expectedName))
			g.Expect(job.Namespace).To(Equal(ks.Namespace))
		})

		t.Run(tc.name+"/Image", func(t *testing.T) {
			g := NewGomegaWithT(t)
			ks := brownfieldKeystone()
			imageTag := "2024.1"
			job := tc.buildFunc(ks, "keystone-config-abc123", imageTag)

			container := findContainerByName(job.Spec.Template.Spec.Containers, tc.containerName)
			g.Expect(container).NotTo(BeNil())
			g.Expect(container.Image).To(Equal(fmt.Sprintf("%s:%s", ks.Spec.Image.Repository, imageTag)))
			// Ensure the imageTag parameter is used, not spec.Image.Tag.
			g.Expect(container.Image).NotTo(ContainSubstring(ks.Spec.Image.Tag))
		})

		t.Run(tc.name+"/Command", func(t *testing.T) {
			g := NewGomegaWithT(t)
			ks := brownfieldKeystone()
			job := tc.buildFunc(ks, "keystone-config-abc123", "2024.1")

			container := findContainerByName(job.Spec.Template.Spec.Containers, tc.containerName)
			g.Expect(container).NotTo(BeNil())
			g.Expect(container.Command).To(Equal([]string{
				"keystone-manage", "--config-dir=/etc/keystone/keystone.conf.d/", "db_sync", tc.expectedFlag,
			}))
		})

		t.Run(tc.name+"/SecurityContext", func(t *testing.T) {
			g := NewGomegaWithT(t)
			ks := brownfieldKeystone()
			job := tc.buildFunc(ks, "keystone-config-abc123", "2024.1")

			container := findContainerByName(job.Spec.Template.Spec.Containers, tc.containerName)
			expectRestrictedSecurityContext(g, container)
		})

		t.Run(tc.name+"/ConfigVolume", func(t *testing.T) {
			g := NewGomegaWithT(t)
			ks := brownfieldKeystone()
			configMap := "keystone-config-abc123"
			job := tc.buildFunc(ks, configMap, "2024.1")

			container := findContainerByName(job.Spec.Template.Spec.Containers, tc.containerName)
			g.Expect(container).NotTo(BeNil())
			g.Expect(container.VolumeMounts).To(HaveLen(1))
			g.Expect(container.VolumeMounts[0].Name).To(Equal("config"))
			g.Expect(container.VolumeMounts[0].MountPath).To(Equal("/etc/keystone/keystone.conf.d/"))
			g.Expect(container.VolumeMounts[0].ReadOnly).To(BeTrue())

			g.Expect(job.Spec.Template.Spec.Volumes).To(HaveLen(1))
			g.Expect(job.Spec.Template.Spec.Volumes[0].Name).To(Equal("config"))
			g.Expect(job.Spec.Template.Spec.Volumes[0].ConfigMap.Name).To(Equal(configMap))
		})

		t.Run(tc.name+"/BackoffLimit", func(t *testing.T) {
			g := NewGomegaWithT(t)
			ks := brownfieldKeystone()
			job := tc.buildFunc(ks, "keystone-config-abc123", "2024.1")

			g.Expect(job.Spec.BackoffLimit).NotTo(BeNil())
			g.Expect(*job.Spec.BackoffLimit).To(Equal(int32(4)))
		})

		t.Run(tc.name+"/RestartPolicy", func(t *testing.T) {
			g := NewGomegaWithT(t)
			ks := brownfieldKeystone()
			job := tc.buildFunc(ks, "keystone-config-abc123", "2024.1")

			g.Expect(job.Spec.Template.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
		})
	}
}

// --- Upgrade detection tests (CC-0056) ---

func TestIsUpgrade(t *testing.T) {
	cases := []struct {
		name             string
		installedRelease string
		tag              string
		want             bool
	}{
		{
			name:             "FreshDeployment_EmptyInstalledRelease",
			installedRelease: "",
			tag:              "2025.2",
			want:             false,
		},
		{
			name:             "SameVersion",
			installedRelease: "2025.2",
			tag:              "2025.2",
			want:             false,
		},
		{
			name:             "PatchOnlyChange",
			installedRelease: "2025.2",
			tag:              "2025.2-p1",
			want:             false,
		},
		{
			name:             "SequentialUpgrade",
			installedRelease: "2025.2",
			tag:              "2026.1",
			want:             true,
		},
		{
			name:             "SkipLevelUpgrade",
			installedRelease: "2024.2",
			tag:              "2026.1",
			want:             true,
		},
		{
			name:             "UnparseableInstalledRelease",
			installedRelease: "latest",
			tag:              "2025.2",
			want:             true,
		},
		{
			name:             "UnparseableTargetTag",
			installedRelease: "2025.2",
			tag:              "latest",
			want:             true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ks := brownfieldKeystone()
			ks.Spec.Image.Tag = tc.tag
			ks.Status.InstalledRelease = tc.installedRelease
			g.Expect(isUpgrade(ks)).To(Equal(tc.want))
		})
	}
}

func TestInitiateUpgrade_SequentialUpgrade(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()
	ks.Spec.Image.Tag = "2026.1"
	ks.Status.InstalledRelease = "2025.2"

	r := newDBTestReconciler(s, ks)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{Requeue: true}))

	// Verify status fields.
	g.Expect(ks.Status.TargetRelease).To(Equal("2026.1"))
	g.Expect(ks.Status.UpgradePhase).To(Equal(keystonev1alpha1.UpgradePhaseExpanding))

	// Verify condition.
	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonExpandInProgress))
	g.Expect(cond.Message).To(ContainSubstring("2025.2"))
	g.Expect(cond.Message).To(ContainSubstring("2026.1"))

	expectEvent(g, r, "Normal UpgradeInitiated")
}

func TestInitiateUpgrade_SkipLevelRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()
	ks.Spec.Image.Tag = "2026.1"
	ks.Status.InstalledRelease = "2024.2"

	r := newDBTestReconciler(s, ks)

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("sequential"))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonUpgradePathInvalid))

	expectEvent(g, r, "Warning UpgradePathInvalid")
}

func TestInitiateUpgrade_DowngradeRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()
	ks.Spec.Image.Tag = "2025.2"
	ks.Status.InstalledRelease = "2026.1"

	r := newDBTestReconciler(s, ks)

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("downgrade"))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDowngradeNotSupported))

	expectEvent(g, r, "Warning DowngradeNotSupported")
}

func TestInitiateUpgrade_InvalidVersionFormat(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()
	ks.Spec.Image.Tag = "latest"
	ks.Status.InstalledRelease = "2025.2"

	r := newDBTestReconciler(s, ks)

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("parse"))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonVersionParseError))

	expectEvent(g, r, "Warning VersionParseError")
}

// --- Upgrade detection in reconcileDatabase flow ---

func TestReconcileDatabase_FreshDeploy_SetsInstalledRelease(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()
	// No installedRelease set — fresh deployment.

	r := newDBTestReconciler(s, ks, completedDBSyncJob(ks), completedSchemaCheckJob(ks))

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	// After successful db_sync, installedRelease should be set.
	g.Expect(ks.Status.InstalledRelease).To(Equal("2025.2"))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDatabaseSynced))

	expectEvent(g, r, "Normal DatabaseSynced")
}

func TestReconcileDatabase_PatchOnly_UsesSimpleDBSync(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()
	ks.Spec.Image.Tag = "2025.2-p1"
	ks.Status.InstalledRelease = "2025.2"

	// Build a completed db_sync job matching the patched image tag.
	desired := buildDBSyncJob(ks, "keystone-config-abc123")
	now := metav1.Now()
	completedJob := desired.DeepCopy()
	completedJob.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template.Spec),
	}
	completedJob.Status.Succeeded = 1
	completedJob.Status.CompletionTime = &now
	completedJob.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}

	r := newDBTestReconciler(s, ks, completedJob, completedSchemaCheckJob(ks))

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	// Verify it used the simple db_sync path and updated installedRelease.
	g.Expect(ks.Status.InstalledRelease).To(Equal("2025.2-p1"))
	g.Expect(ks.Status.UpgradePhase).To(BeEmpty())

	expectEvent(g, r, "Normal DatabaseSynced")
}

func TestReconcileDatabase_ActiveUpgrade_DelegatesToReconcileUpgrade(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()
	ks.Spec.Image.Tag = "2026.1"
	ks.Status.InstalledRelease = "2025.2"
	ks.Status.TargetRelease = "2026.1"
	ks.Status.UpgradePhase = keystonev1alpha1.UpgradePhaseExpanding

	r := newDBTestReconciler(s, ks)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueUpgradeWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonExpandInProgress))
	g.Expect(cond.Message).To(ContainSubstring("2025.2"))
	g.Expect(cond.Message).To(ContainSubstring("2026.1"))

	expectNoEvent(g, r)
}

func TestReconcileDatabase_SameVersionWithInstalledRelease_UsesSimpleDBSync(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()
	ks.Status.InstalledRelease = "2025.2"

	r := newDBTestReconciler(s, ks, completedDBSyncJob(ks), completedSchemaCheckJob(ks))

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	// Still on the simple db_sync path.
	g.Expect(ks.Status.InstalledRelease).To(Equal("2025.2"))
	g.Expect(ks.Status.UpgradePhase).To(BeEmpty())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))

	expectEvent(g, r, "Normal DatabaseSynced")
}

func TestReconcileDatabase_Managed_ConditionMessages(t *testing.T) {
	s := dbTestScheme()

	t.Run("DatabaseNotReady", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ks := managedKeystone()
		db := buildDatabase(ks)
		r := newDBTestReconciler(s, ks, readyMariaDBCluster(ks), db)

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
			readyMariaDBCluster(ks),
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
			readyMariaDBCluster(ks),
			readyDatabase(ks),
			readyUser(ks),
			readyGrant(ks),
			completedDBSyncJob(ks),
			completedSchemaCheckJob(ks),
		)

		_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
		g.Expect(err).NotTo(HaveOccurred())

		cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Message).To(Equal("Database schema is up to date (revision verified)"))
		g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
	})
}

// --- Upgrade phase test helpers (CC-0056) ---

func upgradingKeystone(phase keystonev1alpha1.UpgradePhase) *keystonev1alpha1.Keystone {
	ks := brownfieldKeystone()
	ks.Spec.Image.Tag = "2026.1"
	ks.Status.InstalledRelease = "2025.2"
	ks.Status.TargetRelease = "2026.1"
	ks.Status.UpgradePhase = phase
	return ks
}

func completedUpgradeJob(ks *keystonev1alpha1.Keystone, configMapName, imageTag, phase, flag string) *batchv1.Job {
	desired := buildUpgradeJob(ks, configMapName, imageTag, phase, flag)
	now := metav1.Now()
	j := desired.DeepCopy()
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template.Spec),
	}
	j.Status.Succeeded = 1
	j.Status.CompletionTime = &now
	j.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}
	return j
}

func failedUpgradeJob(ks *keystonev1alpha1.Keystone, configMapName, imageTag, phase, flag string) *batchv1.Job {
	desired := buildUpgradeJob(ks, configMapName, imageTag, phase, flag)
	j := desired.DeepCopy()
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template.Spec),
	}
	j.Status.Failed = 5
	j.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
	}
	return j
}

// --- Expand phase tests (CC-0056) ---

func TestReconcileExpand_NoExistingJob_CreatesJob(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseExpanding)

	r := newDBTestReconciler(s, ks)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueUpgradeWait))

	// Verify the expand Job was created.
	var expandJob batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-db-expand",
		Namespace: "default",
	}, &expandJob)).To(Succeed())
	g.Expect(expandJob.Annotations).To(HaveKey(job.PodSpecHashAnnotation))

	// Verify condition.
	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonExpandInProgress))

	expectNoEvent(g, r)
}

func TestReconcileExpand_JobRunning_Requeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseExpanding)

	// Create a running expand Job (exists but not complete).
	expandJob := buildExpandJob(ks, "keystone-config-abc123", ks.Spec.Image.Tag)
	expandJob.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&expandJob.Spec.Template.Spec),
	}

	r := newDBTestReconciler(s, ks, expandJob)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueUpgradeWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonExpandInProgress))
	g.Expect(cond.Message).To(ContainSubstring("2025.2"))
	g.Expect(cond.Message).To(ContainSubstring("2026.1"))

	expectNoEvent(g, r)
}

func TestReconcileExpand_JobCompleted_TransitionsToMigrating(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseExpanding)

	completed := completedUpgradeJob(ks, "keystone-config-abc123", ks.Spec.Image.Tag, "expand", "--expand")

	r := newDBTestReconciler(s, ks, completed)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{Requeue: true}))

	// Verify phase transition.
	g.Expect(ks.Status.UpgradePhase).To(Equal(keystonev1alpha1.UpgradePhaseMigrating))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonMigrateInProgress))

	expectEvent(g, r, "Normal ExpandComplete")
}

func TestReconcileExpand_JobFailed_ReturnsError(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseExpanding)

	failed := failedUpgradeJob(ks, "keystone-config-abc123", ks.Spec.Image.Tag, "expand", "--expand")

	r := newDBTestReconciler(s, ks, failed)

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("ExpandFailed"))

	expectEvent(g, r, "Warning ExpandFailed")
}

// --- Migrate phase tests (CC-0056) ---

func TestReconcileMigrate_JobRunning_Requeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseMigrating)

	// Create a running migrate Job (exists but not complete).
	migrateJob := buildMigrateJob(ks, "keystone-config-abc123", ks.Spec.Image.Tag)
	migrateJob.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&migrateJob.Spec.Template.Spec),
	}

	r := newDBTestReconciler(s, ks, migrateJob)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueUpgradeWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonMigrateInProgress))
	g.Expect(cond.Message).To(ContainSubstring("2025.2"))
	g.Expect(cond.Message).To(ContainSubstring("2026.1"))

	expectNoEvent(g, r)
}

func TestReconcileMigrate_JobCompleted_TransitionsToRollingUpdate(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseMigrating)

	completed := completedUpgradeJob(ks, "keystone-config-abc123", ks.Spec.Image.Tag, "migrate", "--migrate")

	r := newDBTestReconciler(s, ks, completed)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{Requeue: true}))

	// Verify phase transition.
	g.Expect(ks.Status.UpgradePhase).To(Equal(keystonev1alpha1.UpgradePhaseRollingUpdate))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonUpgradeRollingUpdate))
	g.Expect(cond.Message).To(ContainSubstring("Migrate complete"))
	g.Expect(cond.Message).To(ContainSubstring("2025.2"))
	g.Expect(cond.Message).To(ContainSubstring("2026.1"))

	expectEvent(g, r, "Normal MigrateComplete")
}

func TestReconcileMigrate_JobFailed_ReturnsError(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseMigrating)

	failed := failedUpgradeJob(ks, "keystone-config-abc123", ks.Spec.Image.Tag, "migrate", "--migrate")

	r := newDBTestReconciler(s, ks, failed)

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("MigrateFailed"))

	expectEvent(g, r, "Warning MigrateFailed")
}

// --- RollingUpdate phase tests (CC-0056) ---

func TestReconcileRollingUpdate_PassesThrough(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseRollingUpdate)

	r := newDBTestReconciler(s, ks)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	// Pass-through: no requeue, empty result allows reconcileDeployment to proceed.
	g.Expect(result).To(Equal(ctrl.Result{}))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonUpgradeRollingUpdate))
	g.Expect(cond.Message).To(ContainSubstring("Waiting for Deployment rollout"))
	g.Expect(cond.Message).To(ContainSubstring("2025.2"))
	g.Expect(cond.Message).To(ContainSubstring("2026.1"))

	expectNoEvent(g, r)
}

// --- Contract phase tests (CC-0056) ---

func TestReconcileContract_NoExistingJob_CreatesJobAndRequeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseContracting)

	r := newDBTestReconciler(s, ks)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueUpgradeWait))

	// Verify the contract Job was created.
	var createdJob batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-db-contract",
		Namespace: "default",
	}, &createdJob)).To(Succeed())
	g.Expect(createdJob.Annotations).To(HaveKey(job.PodSpecHashAnnotation))

	// Verify the Job uses the NEW image tag ("2026.1"), not the old one ("2025.2").
	g.Expect(createdJob.Spec.Template.Spec.Containers[0].Image).To(ContainSubstring("2026.1"))

	// Verify condition.
	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("ContractInProgress"))

	expectNoEvent(g, r)
}

func TestReconcileContract_JobRunning_Requeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseContracting)

	// Create a running contract Job (exists but not complete).
	contractJob := buildContractJob(ks, "keystone-config-abc123", ks.Spec.Image.Tag)
	contractJob.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&contractJob.Spec.Template.Spec),
	}

	r := newDBTestReconciler(s, ks, contractJob)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueUpgradeWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("ContractInProgress"))
	g.Expect(cond.Message).To(ContainSubstring("2025.2"))
	g.Expect(cond.Message).To(ContainSubstring("2026.1"))

	expectNoEvent(g, r)
}

func TestReconcileContract_JobCompleted_CompletesUpgrade(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseContracting)

	completed := completedUpgradeJob(ks, "keystone-config-abc123", ks.Spec.Image.Tag, "contract", "--contract")

	r := newDBTestReconciler(s, ks, completed)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	// Upgrade complete: no requeue.
	g.Expect(result).To(Equal(ctrl.Result{}))

	// Verify status fields updated for upgrade completion.
	g.Expect(ks.Status.InstalledRelease).To(Equal("2026.1"))
	g.Expect(ks.Status.TargetRelease).To(BeEmpty())
	g.Expect(ks.Status.UpgradePhase).To(BeEmpty())

	// Verify condition.
	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDatabaseSynced))
	g.Expect(cond.Message).To(ContainSubstring("upgraded"))
	g.Expect(cond.Message).To(ContainSubstring("2025.2"))
	g.Expect(cond.Message).To(ContainSubstring("2026.1"))

	expectEvent(g, r, "Normal UpgradeComplete")
}

func TestReconcileContract_JobFailed_ReturnsError(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseContracting)

	failed := failedUpgradeJob(ks, "keystone-config-abc123", ks.Spec.Image.Tag, "contract", "--contract")

	r := newDBTestReconciler(s, ks, failed)

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("ContractFailed"))

	// Verify upgrade phase remains Contracting (not cleared on failure).
	g.Expect(ks.Status.UpgradePhase).To(Equal(keystonev1alpha1.UpgradePhaseContracting))

	expectEvent(g, r, "Warning ContractFailed")
}

func TestReconcileContract_UsesNewImage(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseContracting)

	r := newDBTestReconciler(s, ks)

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	// Fetch the created Job and verify it uses the NEW image tag.
	var createdJob batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-db-contract",
		Namespace: "default",
	}, &createdJob)).To(Succeed())

	// Contract uses the NEW image (spec.image.tag = "2026.1"), NOT the old one (installedRelease = "2025.2").
	g.Expect(createdJob.Spec.Template.Spec.Containers[0].Image).To(ContainSubstring("2026.1"))
	g.Expect(createdJob.Spec.Template.Spec.Containers[0].Image).NotTo(ContainSubstring("2025.2"))
}

// --- Upgrade edge case tests (CC-0056, REQ-009) ---

// TestReconcileDatabase_InterruptedExpand_Resumes verifies that after an operator
// restart during the Expanding phase, reconcileDatabase resumes from the persisted
// phase and transitions to Migrating when the expand Job is already complete,
// without re-creating the expand Job (CC-0056, REQ-009).
func TestReconcileDatabase_InterruptedExpand_Resumes(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseExpanding)

	// Simulate: expand Job completed before the operator restarted.
	completed := completedUpgradeJob(ks, "keystone-config-abc123", ks.Spec.Image.Tag, "expand", "--expand")

	r := newDBTestReconciler(s, ks, completed)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{Requeue: true}))

	// Verify phase transitioned to Migrating without re-creating the expand Job.
	g.Expect(ks.Status.UpgradePhase).To(Equal(keystonev1alpha1.UpgradePhaseMigrating))

	// Verify no duplicate expand Job was created (only the original exists).
	jobList := &batchv1.JobList{}
	g.Expect(r.Client.List(context.Background(), jobList)).To(Succeed())
	expandJobs := 0
	for _, j := range jobList.Items {
		if j.Name == "test-keystone-db-expand" {
			expandJobs++
		}
	}
	g.Expect(expandJobs).To(Equal(1), "expand Job should not be re-created after restart")

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonMigrateInProgress))

	expectEvent(g, r, "Normal ExpandComplete")
}

// TestReconcileDatabase_TagChangedDuringUpgrade_Blocks verifies that when the
// image tag is changed during an active upgrade to a value different from
// targetRelease, the operator blocks with DatabaseReady=False and reason
// UpgradeTargetChanged (CC-0056, REQ-009).
func TestReconcileDatabase_TagChangedDuringUpgrade_Blocks(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseExpanding)

	// Simulate: operator is upgrading 2025.2 → 2026.1, but someone changes the
	// tag to 2026.2 mid-upgrade.
	ks.Spec.Image.Tag = "2026.2"

	r := newDBTestReconciler(s, ks)

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("image tag changed during active upgrade"))
	g.Expect(err.Error()).To(ContainSubstring("2026.1"))
	g.Expect(err.Error()).To(ContainSubstring("2026.2"))

	// Verify condition.
	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonUpgradeTargetChanged))
	g.Expect(cond.Message).To(ContainSubstring("2026.2"))
	g.Expect(cond.Message).To(ContainSubstring("2025.2"))
	g.Expect(cond.Message).To(ContainSubstring("2026.1"))

	// Verify upgrade state was NOT modified.
	g.Expect(ks.Status.UpgradePhase).To(Equal(keystonev1alpha1.UpgradePhaseExpanding))
	g.Expect(ks.Status.TargetRelease).To(Equal("2026.1"))
	g.Expect(ks.Status.InstalledRelease).To(Equal("2025.2"))

	expectEvent(g, r, "Warning UpgradeTargetChanged")
}

// --- Schema check tests (CC-0064) ---

// TestBuildSchemaCheckJob_Name verifies that the schema-check Job has the correct
// name ({keystone.Name}-schema-check) and namespace (CC-0064, REQ-003).
func TestBuildSchemaCheckJob_Name(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := brownfieldKeystone()

	j := buildSchemaCheckJob(ks, "keystone-config-abc123")

	g.Expect(j.Name).To(Equal("test-keystone-schema-check"))
	g.Expect(j.Namespace).To(Equal(ks.Namespace))
}

// TestBuildSchemaCheckJob_Image verifies that the schema-check container uses
// the correct Keystone image ({spec.image.repository}:{spec.image.tag}) (CC-0064, REQ-003).
func TestBuildSchemaCheckJob_Image(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := brownfieldKeystone()

	j := buildSchemaCheckJob(ks, "keystone-config-abc123")

	container := findContainerByName(j.Spec.Template.Spec.Containers, "schema-check")
	g.Expect(container).NotTo(BeNil())
	g.Expect(container.Image).To(Equal(fmt.Sprintf("%s:%s", ks.Spec.Image.Repository, ks.Spec.Image.Tag)))
}

// TestBuildSchemaCheckJob_SecurityContext verifies that the schema-check container
// satisfies the PSS Restricted profile (CC-0064, REQ-003).
func TestBuildSchemaCheckJob_SecurityContext(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := brownfieldKeystone()

	j := buildSchemaCheckJob(ks, "keystone-config-abc123")

	container := findContainerByName(j.Spec.Template.Spec.Containers, "schema-check")
	expectRestrictedSecurityContext(g, container)
}

// TestBuildSchemaCheckJob_ConfigVolume verifies that the schema-check container
// mounts the config ConfigMap at /etc/keystone/keystone.conf.d/ read-only (CC-0064, REQ-003).
func TestBuildSchemaCheckJob_ConfigVolume(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := brownfieldKeystone()
	configMap := "keystone-config-abc123"

	j := buildSchemaCheckJob(ks, configMap)

	container := findContainerByName(j.Spec.Template.Spec.Containers, "schema-check")
	g.Expect(container).NotTo(BeNil())
	g.Expect(container.VolumeMounts).To(HaveLen(1))
	g.Expect(container.VolumeMounts[0].Name).To(Equal("config"))
	g.Expect(container.VolumeMounts[0].MountPath).To(Equal("/etc/keystone/keystone.conf.d/"))
	g.Expect(container.VolumeMounts[0].ReadOnly).To(BeTrue())

	g.Expect(j.Spec.Template.Spec.Volumes).To(HaveLen(1))
	g.Expect(j.Spec.Template.Spec.Volumes[0].Name).To(Equal("config"))
	g.Expect(j.Spec.Template.Spec.Volumes[0].ConfigMap.Name).To(Equal(configMap))
}

// TestBuildSchemaCheckJob_BackoffLimit verifies that the schema-check Job has
// backoffLimit=2 (not the default 4 used by db_sync) (CC-0064, REQ-004).
func TestBuildSchemaCheckJob_BackoffLimit(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := brownfieldKeystone()

	j := buildSchemaCheckJob(ks, "keystone-config-abc123")

	g.Expect(j.Spec.BackoffLimit).NotTo(BeNil())
	g.Expect(*j.Spec.BackoffLimit).To(Equal(int32(2)))
}

// TestBuildSchemaCheckJob_TTL verifies that the schema-check Job has
// ttlSecondsAfterFinished=300 for automatic cleanup (CC-0064, REQ-004).
func TestBuildSchemaCheckJob_TTL(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := brownfieldKeystone()

	j := buildSchemaCheckJob(ks, "keystone-config-abc123")

	g.Expect(j.Spec.TTLSecondsAfterFinished).NotTo(BeNil())
	g.Expect(*j.Spec.TTLSecondsAfterFinished).To(Equal(int32(300)))
}

// TestBuildSchemaCheckJob_RestartPolicy verifies that the schema-check Job pod
// has RestartPolicy=Never (CC-0064, REQ-004).
func TestBuildSchemaCheckJob_RestartPolicy(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := brownfieldKeystone()

	j := buildSchemaCheckJob(ks, "keystone-config-abc123")

	g.Expect(j.Spec.Template.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
}

// TestBuildSchemaCheckJob_Command verifies that the schema-check container uses
// /bin/sh -eu -c with keystone-manage db_sync --check (CC-0064, REQ-003).
func TestBuildSchemaCheckJob_Command(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := brownfieldKeystone()

	j := buildSchemaCheckJob(ks, "keystone-config-abc123")

	container := findContainerByName(j.Spec.Template.Spec.Containers, "schema-check")
	g.Expect(container).NotTo(BeNil())
	g.Expect(container.Command).To(HaveLen(4))
	g.Expect(container.Command[:3]).To(Equal([]string{"/bin/sh", "-eu", "-c"}))
	g.Expect(container.Command[3]).To(ContainSubstring("keystone-manage"))
	g.Expect(container.Command[3]).To(ContainSubstring("db_sync --check"))
}

// TestReconcileDatabase_SchemaCheckRunning_Requeues verifies that when db_sync is
// complete but schema-check is still running, the reconciler requeues with
// RequeueDatabaseWait and sets the SchemaCheckInProgress condition (CC-0064).
func TestReconcileDatabase_SchemaCheckRunning_Requeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	r := newDBTestReconciler(s, ks, completedDBSyncJob(ks), runningSchemaCheckJob(ks))

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonSchemaCheckInProgress))

	expectNoEvent(g, r)
}

// TestReconcileDatabase_SchemaCheckComplete_DatabaseSynced verifies that when both
// db_sync and schema-check complete, DatabaseReady=True with reason DatabaseSynced
// and message containing 'revision' (CC-0064).
func TestReconcileDatabase_SchemaCheckComplete_DatabaseSynced(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	r := newDBTestReconciler(s, ks, completedDBSyncJob(ks), completedSchemaCheckJob(ks))

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDatabaseSynced))
	g.Expect(cond.Message).To(ContainSubstring("revision"))

	expectEvent(g, r, "Normal DatabaseSynced")
}

// TestReconcileDatabase_SchemaCheckFailed_SchemaDriftDetected verifies that when
// the schema-check Job fails, DatabaseReady=False with reason SchemaDriftDetected
// and an error is returned (CC-0064).
func TestReconcileDatabase_SchemaCheckFailed_SchemaDriftDetected(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	r := newDBTestReconciler(s, ks, completedDBSyncJob(ks), failedSchemaCheckJob(ks))

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("schema-check"))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonSchemaDriftDetected))

	expectEvent(g, r, "Warning SchemaDriftDetected")
}

// TestReconcileDatabase_Managed_AllReady_WithSchemaCheck verifies that in managed
// mode, when all MariaDB CRs are ready and both db_sync and schema-check Jobs
// complete, DatabaseReady=True with reason DatabaseSynced and message containing
// 'revision' (CC-0064).
func TestReconcileDatabase_Managed_AllReady_WithSchemaCheck(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := managedKeystone()

	r := newDBTestReconciler(s, ks,
		readyMariaDBCluster(ks),
		readyDatabase(ks),
		readyUser(ks),
		readyGrant(ks),
		completedDBSyncJob(ks),
		completedSchemaCheckJob(ks),
	)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDatabaseSynced))
	g.Expect(cond.Message).To(ContainSubstring("revision"))
	g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))

	expectEvent(g, r, "Normal DatabaseSynced")
}

// TestReconcileDatabase_SchemaCheckStale_Recreated verifies that a completed
// schema-check Job with a stale pod-spec hash triggers deletion and recreation,
// and the reconciler requeues (CC-0064).
func TestReconcileDatabase_SchemaCheckStale_Recreated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	// Completed schema-check with a stale hash.
	staleSchemaCheck := completedSchemaCheckJob(ks)
	staleSchemaCheck.Annotations[job.PodSpecHashAnnotation] = "stale-hash-from-previous-image"

	r := newDBTestReconciler(s, ks, completedDBSyncJob(ks), staleSchemaCheck)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))

	// Verify the old Job was deleted and a new one created with the correct hash.
	var newJob batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      fmt.Sprintf("%s-schema-check", ks.Name),
		Namespace: ks.Namespace,
	}, &newJob)).To(Succeed())

	desired := buildSchemaCheckJob(ks, "keystone-config-abc123")
	expectedHash := job.PodSpecHash(&desired.Spec.Template.Spec)
	g.Expect(newJob.Annotations[job.PodSpecHashAnnotation]).To(Equal(expectedHash))
}

// TestReconcileDatabase_SchemaCheckNotCreatedWhenDBSyncRunning verifies that when
// db_sync is still running, no schema-check Job is created and the condition
// remains DBSyncInProgress (CC-0064).
func TestReconcileDatabase_SchemaCheckNotCreatedWhenDBSyncRunning(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	// db_sync Job exists but is not completed (still running).
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
	g.Expect(cond.Reason).To(Equal(conditionReasonDBSyncInProgress))

	// Verify no schema-check Job was created.
	var schemaCheckJob batchv1.Job
	err = r.Get(context.Background(), client.ObjectKey{
		Name:      fmt.Sprintf("%s-schema-check", ks.Name),
		Namespace: ks.Namespace,
	}, &schemaCheckJob)
	g.Expect(err).To(HaveOccurred(), "schema-check Job should not exist when db_sync is running")

	expectNoEvent(g, r)
}

// TestReconcileDatabase_SchemaCheckNotCreatedWhenDBSyncFails verifies that when
// db_sync fails, no schema-check Job is created and the condition is set to
// DBSyncFailed (CC-0064).
func TestReconcileDatabase_SchemaCheckNotCreatedWhenDBSyncFails(t *testing.T) {
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
	g.Expect(cond.Reason).To(Equal(conditionReasonDBSyncFailed))

	// Verify no schema-check Job was created.
	var schemaCheckJob batchv1.Job
	err = r.Get(context.Background(), client.ObjectKey{
		Name:      fmt.Sprintf("%s-schema-check", ks.Name),
		Namespace: ks.Namespace,
	}, &schemaCheckJob)
	g.Expect(err).To(HaveOccurred(), "schema-check Job should not exist when db_sync fails")

	expectEvent(g, r, "Warning DBSyncFailed")
}

// TestReconcileDatabase_SchemaCheckFailed_InstalledReleaseNotUpdated verifies that
// when the schema-check Job fails, InstalledRelease is NOT updated to the new
// tag (CC-0064).
func TestReconcileDatabase_SchemaCheckFailed_InstalledReleaseNotUpdated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()
	// Fresh deploy — InstalledRelease is empty.

	r := newDBTestReconciler(s, ks, completedDBSyncJob(ks), failedSchemaCheckJob(ks))

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())

	// InstalledRelease must NOT be updated when schema-check fails.
	g.Expect(ks.Status.InstalledRelease).To(BeEmpty())

	expectEvent(g, r, "Warning SchemaDriftDetected")
}

// TestReconcileDatabase_ConditionObservedGeneration verifies that
// ObservedGeneration is set on the DatabaseReady condition for
// False (ClusterNotReady, WaitingForDatabase, DBSyncFailed,
// SchemaDriftDetected) and True (DatabaseSynced) paths with distinct
// generation values (CC-0072, REQ-002, REQ-003).
func TestReconcileDatabase_ConditionObservedGeneration(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()

	// Test ObservedGeneration for the ClusterNotReady path (cluster missing).
	ks := managedKeystone()
	ks.Generation = 7

	r := newDBTestReconciler(s, ks)

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.ObservedGeneration).To(Equal(int64(7)))

	// Test ObservedGeneration for the WaitingForDatabase path (User/Grant not ready).
	ks3 := managedKeystone()
	ks3.Generation = 5

	r3 := newDBTestReconciler(s, ks3,
		readyMariaDBCluster(ks3),
		readyDatabase(ks3),
		buildUser(ks3), // exists but not ready
	)

	_, err = r3.reconcileDatabase(context.Background(), ks3, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	cond3 := meta.FindStatusCondition(ks3.Status.Conditions, "DatabaseReady")
	g.Expect(cond3).NotTo(BeNil())
	g.Expect(cond3.ObservedGeneration).To(Equal(int64(5)))

	// Test ObservedGeneration for the DBSyncFailed path.
	ks4 := brownfieldKeystone()
	ks4.Generation = 9

	r4 := newDBTestReconciler(s, ks4, failedDBSyncJob(ks4))

	_, err = r4.reconcileDatabase(context.Background(), ks4, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())

	cond4 := meta.FindStatusCondition(ks4.Status.Conditions, "DatabaseReady")
	g.Expect(cond4).NotTo(BeNil())
	g.Expect(cond4.ObservedGeneration).To(Equal(int64(9)))

	// Test ObservedGeneration for the SchemaDriftDetected path.
	ks5 := brownfieldKeystone()
	ks5.Generation = 15

	r5 := newDBTestReconciler(s, ks5, completedDBSyncJob(ks5), failedSchemaCheckJob(ks5))

	_, err = r5.reconcileDatabase(context.Background(), ks5, "keystone-config-abc123")
	g.Expect(err).To(HaveOccurred())

	cond5 := meta.FindStatusCondition(ks5.Status.Conditions, "DatabaseReady")
	g.Expect(cond5).NotTo(BeNil())
	g.Expect(cond5.ObservedGeneration).To(Equal(int64(15)))

	// Test ObservedGeneration for the DatabaseSynced path (all ready).
	ks2 := managedKeystone()
	ks2.Generation = 12

	r2 := newDBTestReconciler(s, ks2,
		readyMariaDBCluster(ks2),
		readyDatabase(ks2),
		readyUser(ks2),
		readyGrant(ks2),
		completedDBSyncJob(ks2),
		completedSchemaCheckJob(ks2),
	)

	_, err = r2.reconcileDatabase(context.Background(), ks2, "keystone-config-abc123")
	g.Expect(err).NotTo(HaveOccurred())

	cond2 := meta.FindStatusCondition(ks2.Status.Conditions, "DatabaseReady")
	g.Expect(cond2).NotTo(BeNil())
	g.Expect(cond2.ObservedGeneration).To(Equal(int64(12)))
}

// TestBuildDBSyncJob_PriorityClassNameSet verifies that when spec.PriorityClassName
// is set, the db-sync Job PodSpec includes the configured priority class (CC-0075).
func TestBuildDBSyncJob_PriorityClassNameSet(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := brownfieldKeystone()
	pcn := "system-cluster-critical"
	ks.Spec.PriorityClassName = &pcn

	job := buildDBSyncJob(ks, "keystone-config-abc123")

	g.Expect(job.Spec.Template.Spec.PriorityClassName).To(Equal("system-cluster-critical"))
}

// TestBuildDBSyncJob_PriorityClassNameNil verifies that when spec.PriorityClassName
// is nil, the db-sync Job PodSpec has an empty priority class name (CC-0075).
func TestBuildDBSyncJob_PriorityClassNameNil(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := brownfieldKeystone()

	job := buildDBSyncJob(ks, "keystone-config-abc123")

	g.Expect(job.Spec.Template.Spec.PriorityClassName).To(BeEmpty())
}
