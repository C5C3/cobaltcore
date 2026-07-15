// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/c5c3/forge/internal/common/database"
	"github.com/c5c3/forge/internal/common/job"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

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
			Deployment: keystonev1alpha1.DeploymentSpec{Replicas: 3},
			Image:      commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
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
			Deployment: keystonev1alpha1.DeploymentSpec{Replicas: 3},
			Image:      commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
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
	desired := buildDBSyncJob(ks, "keystone-config-abc123", "")
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

// failedDBSyncJob returns a db_sync Job that is marked as permanently failed.
func failedDBSyncJob(ks *keystonev1alpha1.Keystone) *batchv1.Job {
	desired := buildDBSyncJob(ks, "keystone-config-abc123", "")
	j := desired.DeepCopy()
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template),
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

// --- Schema check test helpers ---

// completedSchemaCheckJob returns a schema-check Job that matches what
// buildSchemaCheckJob produces for the given keystone and is marked as
// complete with the correct pod-spec hash. The UID is set so
// recordDBJobTerminalState can dedupe the per-phase metric emission across
// reconciles.
func completedSchemaCheckJob(ks *keystonev1alpha1.Keystone) *batchv1.Job {
	desired := buildSchemaCheckJob(ks, "keystone-config-abc123", "")
	now := metav1.Now()
	j := desired.DeepCopy()
	j.UID = types.UID(ks.Name + "-schema-check-complete-uid")
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

// failedSchemaCheckJob returns a schema-check Job that is marked as
// permanently failed. The UID is set so recordDBJobTerminalState
// can dedupe the per-phase metric emission across reconciles.
func failedSchemaCheckJob(ks *keystonev1alpha1.Keystone) *batchv1.Job {
	desired := buildSchemaCheckJob(ks, "keystone-config-abc123", "")
	j := desired.DeepCopy()
	j.UID = types.UID(ks.Name + "-schema-check-failed-uid")
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template),
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
// completed yet.
func runningSchemaCheckJob(ks *keystonev1alpha1.Keystone) *batchv1.Job {
	desired := buildSchemaCheckJob(ks, "keystone-config-abc123", "")
	j := desired.DeepCopy()
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template),
	}
	return j
}

// readyMariaDBCluster returns a MariaDB cluster CR with Ready=True matching
// the name referenced by ks.Spec.Database.ClusterRef.
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
// simulating an upstream database outage.
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

	r := newDBTestReconciler(
		s, ks,
		readyMariaDBCluster(ks),
		readyDatabase(ks),
		readyUser(ks),
		readyGrant(ks),
		completedDBSyncJob(ks),
		completedSchemaCheckJob(ks),
	)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(database.ReasonDatabaseSynced))

	expectEvent(g, r, "Normal DatabaseSynced")
}

// TestReconcileDatabase_DynamicManaged_CreatesDatabaseButNoUserGrant verifies
// that in Dynamic credentials mode the operator still provisions the schema
// (Database CR) but does NOT create MariaDB User/Grant CRs — the OpenBao engine
// owns the DB user lifecycle.
func TestReconcileDatabase_DynamicManaged_CreatesDatabaseButNoUserGrant(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := managedKeystone()
	ks.Spec.Database.CredentialsMode = commonv1.CredentialsModeDynamic

	r := newDBTestReconciler(
		s, ks,
		readyMariaDBCluster(ks),
		readyDatabase(ks),
		completedDBSyncJob(ks),
		completedSchemaCheckJob(ks),
	)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(database.ReasonDatabaseSynced))

	// No User/Grant CRs must have been created — the engine owns the DB user.
	userList := &mariadbv1alpha1.UserList{}
	g.Expect(r.Client.List(context.Background(), userList, client.InNamespace("default"))).To(Succeed())
	g.Expect(userList.Items).To(BeEmpty())
	grantList := &mariadbv1alpha1.GrantList{}
	g.Expect(r.Client.List(context.Background(), grantList, client.InNamespace("default"))).To(Succeed())
	g.Expect(grantList.Items).To(BeEmpty())
}

// TestReconcileDatabase_DynamicManaged_PreexistingUserGrantSurvive verifies that
// a User/Grant left over from a Static deployment mid-migration is NOT deleted
// by a Dynamic reconcile, so its grant overlaps engine-issued logins for a
// downtime-free cutover.
func TestReconcileDatabase_DynamicManaged_PreexistingUserGrantSurvive(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := managedKeystone()
	ks.Spec.Database.CredentialsMode = commonv1.CredentialsModeDynamic

	r := newDBTestReconciler(
		s, ks,
		readyMariaDBCluster(ks),
		readyDatabase(ks),
		readyUser(ks),  // left over from a prior Static deployment
		readyGrant(ks), // left over from a prior Static deployment
		completedDBSyncJob(ks),
		completedSchemaCheckJob(ks),
	)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	// The pre-existing User/Grant must still be present (not deleted).
	key := mariaDBResourceKey(ks)
	g.Expect(r.Get(context.Background(), key, &mariadbv1alpha1.User{})).To(Succeed())
	g.Expect(r.Get(context.Background(), key, &mariadbv1alpha1.Grant{})).To(Succeed())
}

func TestReconcileDatabase_Managed_DatabaseNotReady_Requeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := managedKeystone()

	// Database CR exists but is not ready (no Ready condition).
	db := buildDatabase(ks)
	r := newDBTestReconciler(s, ks, readyMariaDBCluster(ks), db)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonWaitingForDatabase))

	expectNoEvent(g, r)
}

func TestReconcileDatabase_Managed_UserNotReady_Requeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := managedKeystone()

	// Database is ready, but User is not.
	r := newDBTestReconciler(
		s, ks,
		readyMariaDBCluster(ks),
		readyDatabase(ks),
		buildUser(ks), // exists but not ready
	)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonWaitingForDatabase))

	expectNoEvent(g, r)
}

func TestReconcileDatabase_Managed_ClusterMissing_Requeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := managedKeystone()

	// No MariaDB cluster CR in the fake client — reconciler should report
	// DatabaseReady=False rather than proceeding to create the Database CR.
	r := newDBTestReconciler(s, ks)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonClusterNotReady))

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
	// flip DatabaseReady back to False on the next reconcile.
	meta.SetStatusCondition(&ks.Status.Conditions, metav1.Condition{
		Type:   "DatabaseReady",
		Status: metav1.ConditionTrue,
		Reason: database.ReasonDatabaseSynced,
	})

	r := newDBTestReconciler(
		s, ks,
		notReadyMariaDBCluster(ks),
		readyDatabase(ks),
		readyUser(ks),
		readyGrant(ks),
		completedDBSyncJob(ks),
	)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonClusterNotReady))
	g.Expect(cond.Message).To(ContainSubstring("mariadb"))

	expectNoEvent(g, r)
}

// --- Brownfield mode tests ---

func TestReconcileDatabase_Brownfield_DBSyncComplete_DatabaseSynced(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	r := newDBTestReconciler(s, ks, completedDBSyncJob(ks), completedSchemaCheckJob(ks))

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(database.ReasonDatabaseSynced))

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
	syncJob := buildDBSyncJob(ks, "keystone-config-abc123", "")
	syncJob.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&syncJob.Spec.Template),
	}
	r := newDBTestReconciler(s, ks, syncJob)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonDBSyncInProgress))

	expectNoEvent(g, r)
}

func TestReconcileDatabase_DBSyncFailed_ReturnsError(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	r := newDBTestReconciler(s, ks, failedDBSyncJob(ks))

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("db_sync"))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonDBSyncFailed))

	expectEvent(g, r, "Warning DBSyncFailed")
}

func TestReconcileDatabase_Brownfield_SkipsMariaDBCRs_CreatesDBSyncJob(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	// No pre-existing Job — should create one and requeue.
	r := newDBTestReconciler(s, ks)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
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

	// Verify config volume mount is present (: db_sync needs keystone.conf for DB connection).
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
	g.Expect(cond.Reason).To(Equal(database.ReasonDBSyncInProgress))
	g.Expect(cond.Message).To(Equal("db_sync job is running"))
}

// TestBuildDBSyncJob_SecurityContext verifies that the db-sync container in the
// Job returned by buildDBSyncJob has the correct SecurityContext with all four
// PSS Restricted profile fields (through).
func TestBuildDBSyncJob_SecurityContext(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := brownfieldKeystone()

	job := buildDBSyncJob(ks, "keystone-config-abc123", "")

	container := findContainerByName(job.Spec.Template.Spec.Containers, "db-sync")
	expectRestrictedSecurityContext(g, container)
}

// TestBuildDBJobVariants_DBConnectionEnv verifies that every variant produced
// by buildDBJob (db-sync, expand, migrate, contract, schema-check) carries the
// OS_DATABASE__CONNECTION env var sourced from the derived
// <name>-db-connection Secret so the DB URL is read from a Secret rather than
// the ConfigMap.
func TestBuildDBJobVariants_DBConnectionEnv(t *testing.T) {
	ks := brownfieldKeystone()
	expectedEnv := buildDBConnectionEnvVar(ks)

	cases := []struct {
		name          string
		job           *batchv1.Job
		containerName string
	}{
		{"db-sync", buildDBSyncJob(ks, "keystone-config-abc123", ""), "db-sync"},
		{"expand", buildExpandJob(ks, "keystone-config-abc123", "", ks.Spec.Image.Tag), "db-expand"},
		{"migrate", buildMigrateJob(ks, "keystone-config-abc123", "", ks.Spec.Image.Tag), "db-migrate"},
		{"contract", buildContractJob(ks, "keystone-config-abc123", "", ks.Spec.Image.Tag), "db-contract"},
		{"schema-check", buildSchemaCheckJob(ks, "keystone-config-abc123", ""), "schema-check"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			container := findContainerByName(tc.job.Spec.Template.Spec.Containers, tc.containerName)
			g.Expect(container).NotTo(BeNil())
			g.Expect(container.Env).To(ContainElement(expectedEnv),
				"%s container must source [database].connection from the derived Secret",
				tc.containerName)
		})
	}
}

func TestReconcileDatabase_StaleDBSyncJob_Recreated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	// Create a completed Job with a stale hash.
	staleJob := completedDBSyncJob(ks)
	staleJob.Annotations[job.PodSpecHashAnnotation] = "stale-hash-from-previous-image"

	r := newDBTestReconciler(s, ks, staleJob)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))

	// Verify the old Job was deleted and a new one created with the correct hash.
	var newJob batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      "test-keystone-db-sync",
		Namespace: "default",
	}, &newJob)).To(Succeed())

	desired := buildDBSyncJob(ks, "keystone-config-abc123", "")
	expectedHash := job.PodSpecHash(&desired.Spec.Template)
	g.Expect(newJob.Annotations[job.PodSpecHashAnnotation]).To(Equal(expectedHash))
}

// TestBuildUpgradeJobs verifies all three upgrade-phase Job builders
// (buildExpandJob, buildMigrateJob, buildContractJob) produce the correct Job
// metadata, image, command, security context, and config volume.
func TestBuildUpgradeJobs(t *testing.T) {
	cases := []struct {
		name          string
		buildFunc     func(*keystonev1alpha1.Keystone, string, string, string) *batchv1.Job
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
			job := tc.buildFunc(ks, "keystone-config-abc123", "", "2024.1")

			g.Expect(job.Name).To(Equal(tc.expectedName))
			g.Expect(job.Namespace).To(Equal(ks.Namespace))
		})

		t.Run(tc.name+"/Image", func(t *testing.T) {
			g := NewGomegaWithT(t)
			ks := brownfieldKeystone()
			imageTag := "2024.1"
			job := tc.buildFunc(ks, "keystone-config-abc123", "", imageTag)

			container := findContainerByName(job.Spec.Template.Spec.Containers, tc.containerName)
			g.Expect(container).NotTo(BeNil())
			g.Expect(container.Image).To(Equal(fmt.Sprintf("%s:%s", ks.Spec.Image.Repository, imageTag)))
			// Ensure the imageTag parameter is used, not spec.Image.Tag.
			g.Expect(container.Image).NotTo(ContainSubstring(ks.Spec.Image.Tag))
		})

		t.Run(tc.name+"/Command", func(t *testing.T) {
			g := NewGomegaWithT(t)
			ks := brownfieldKeystone()
			job := tc.buildFunc(ks, "keystone-config-abc123", "", "2024.1")

			container := findContainerByName(job.Spec.Template.Spec.Containers, tc.containerName)
			g.Expect(container).NotTo(BeNil())
			g.Expect(container.Command).To(Equal([]string{
				"keystone-manage", "--config-dir=/etc/keystone/keystone.conf.d/", "db_sync", tc.expectedFlag,
			}))
		})

		t.Run(tc.name+"/SecurityContext", func(t *testing.T) {
			g := NewGomegaWithT(t)
			ks := brownfieldKeystone()
			job := tc.buildFunc(ks, "keystone-config-abc123", "", "2024.1")

			container := findContainerByName(job.Spec.Template.Spec.Containers, tc.containerName)
			expectRestrictedSecurityContext(g, container)
		})

		t.Run(tc.name+"/ConfigVolume", func(t *testing.T) {
			g := NewGomegaWithT(t)
			ks := brownfieldKeystone()
			configMap := "keystone-config-abc123"
			job := tc.buildFunc(ks, configMap, "", "2024.1")

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
			job := tc.buildFunc(ks, "keystone-config-abc123", "", "2024.1")

			g.Expect(job.Spec.BackoffLimit).NotTo(BeNil())
			g.Expect(*job.Spec.BackoffLimit).To(Equal(int32(4)))
		})

		t.Run(tc.name+"/RestartPolicy", func(t *testing.T) {
			g := NewGomegaWithT(t)
			ks := brownfieldKeystone()
			job := tc.buildFunc(ks, "keystone-config-abc123", "", "2024.1")

			g.Expect(job.Spec.Template.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
		})
	}
}

// --- Upgrade detection tests ---

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

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
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

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
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

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
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

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
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

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	// After successful db_sync, installedRelease should be set.
	g.Expect(ks.Status.InstalledRelease).To(Equal("2025.2"))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(database.ReasonDatabaseSynced))

	expectEvent(g, r, "Normal DatabaseSynced")
}

func TestReconcileDatabase_PatchOnly_UsesSimpleDBSync(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()
	ks.Spec.Image.Tag = "2025.2-p1"
	ks.Status.InstalledRelease = "2025.2"

	// Build a completed db_sync job matching the patched image tag.
	desired := buildDBSyncJob(ks, "keystone-config-abc123", "")
	now := metav1.Now()
	completedJob := desired.DeepCopy()
	completedJob.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template),
	}
	completedJob.Status.Succeeded = 1
	completedJob.Status.CompletionTime = &now
	completedJob.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}

	r := newDBTestReconciler(s, ks, completedJob, completedSchemaCheckJob(ks))

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
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

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
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

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
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

		_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
		g.Expect(err).NotTo(HaveOccurred())

		cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Message).To(Equal("MariaDB Database CR is not ready"))
		g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
	})

	t.Run("UserNotReady", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ks := managedKeystone()
		r := newDBTestReconciler(
			s, ks,
			readyMariaDBCluster(ks),
			readyDatabase(ks),
			buildUser(ks), // exists but not ready
		)

		_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
		g.Expect(err).NotTo(HaveOccurred())

		cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Message).To(Equal("MariaDB User or Grant CR is not ready"))
		g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
	})

	t.Run("DatabaseAndGrantUseSpecDatabaseName", func(t *testing.T) {
		// Verify that buildDatabase and buildGrant use spec.database.database
		// (not metadata.name) so the MariaDB database name, Grant, and connection
		// URL all target the same database.
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
		r := newDBTestReconciler(
			s, ks,
			readyMariaDBCluster(ks),
			readyDatabase(ks),
			readyUser(ks),
			readyGrant(ks),
			completedDBSyncJob(ks),
			completedSchemaCheckJob(ks),
		)

		_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
		g.Expect(err).NotTo(HaveOccurred())

		cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Message).To(Equal("Database schema is up to date (revision verified)"))
		g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
	})
}

// --- Upgrade phase test helpers ---

func upgradingKeystone(phase keystonev1alpha1.UpgradePhase) *keystonev1alpha1.Keystone {
	ks := brownfieldKeystone()
	ks.Spec.Image.Tag = "2026.1"
	ks.Status.InstalledRelease = "2025.2"
	ks.Status.TargetRelease = "2026.1"
	ks.Status.UpgradePhase = phase
	return ks
}

func completedUpgradeJob(ks *keystonev1alpha1.Keystone, configMapName, imageTag, phase, flag string) *batchv1.Job {
	desired := buildUpgradeJob(ks, configMapName, "", imageTag, phase, flag)
	now := metav1.Now()
	j := desired.DeepCopy()
	j.UID = types.UID(ks.Name + "-db-" + phase + "-complete-uid")
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template),
	}
	j.Status.Succeeded = 1
	j.Status.CompletionTime = &now
	j.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}
	return j
}

func failedUpgradeJob(ks *keystonev1alpha1.Keystone, configMapName, imageTag, phase, flag string) *batchv1.Job {
	desired := buildUpgradeJob(ks, configMapName, "", imageTag, phase, flag)
	j := desired.DeepCopy()
	j.UID = types.UID(ks.Name + "-db-" + phase + "-failed-uid")
	j.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template),
	}
	j.Status.Failed = 5
	j.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
	}
	return j
}

// --- Expand phase tests ---

func TestReconcileExpand_NoExistingJob_CreatesJob(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseExpanding)

	r := newDBTestReconciler(s, ks)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
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
	expandJob := buildExpandJob(ks, "keystone-config-abc123", "", ks.Spec.Image.Tag)
	expandJob.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&expandJob.Spec.Template),
	}

	r := newDBTestReconciler(s, ks, expandJob)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
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

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
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

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).To(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("ExpandFailed"))

	expectEvent(g, r, "Warning ExpandFailed")
}

// --- Migrate phase tests ---

func TestReconcileMigrate_JobRunning_Requeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseMigrating)

	// Create a running migrate Job (exists but not complete).
	migrateJob := buildMigrateJob(ks, "keystone-config-abc123", "", ks.Spec.Image.Tag)
	migrateJob.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&migrateJob.Spec.Template),
	}

	r := newDBTestReconciler(s, ks, migrateJob)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
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

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
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

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).To(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("MigrateFailed"))

	expectEvent(g, r, "Warning MigrateFailed")
}

// --- RollingUpdate phase tests ---

func TestReconcileRollingUpdate_PassesThrough(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseRollingUpdate)

	r := newDBTestReconciler(s, ks)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
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

// --- Contract phase tests ---

func TestReconcileContract_NoExistingJob_CreatesJobAndRequeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseContracting)

	r := newDBTestReconciler(s, ks)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
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
	contractJob := buildContractJob(ks, "keystone-config-abc123", "", ks.Spec.Image.Tag)
	contractJob.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&contractJob.Spec.Template),
	}

	r := newDBTestReconciler(s, ks, contractJob)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
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

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
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
	g.Expect(cond.Reason).To(Equal(database.ReasonDatabaseSynced))
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

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
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

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
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

// --- Upgrade edge case tests ---

// TestReconcileDatabase_InterruptedExpand_Resumes verifies that after an operator
// restart during the Expanding phase, reconcileDatabase resumes from the persisted
// phase and transitions to Migrating when the expand Job is already complete,
// without re-creating the expand Job.
func TestReconcileDatabase_InterruptedExpand_Resumes(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseExpanding)

	// Simulate: expand Job completed before the operator restarted.
	completed := completedUpgradeJob(ks, "keystone-config-abc123", ks.Spec.Image.Tag, "expand", "--expand")

	r := newDBTestReconciler(s, ks, completed)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
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
// UpgradeTargetChanged.
func TestReconcileDatabase_TagChangedDuringUpgrade_Blocks(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseExpanding)

	// Simulate: operator is upgrading 2025.2 → 2026.1, but someone changes the
	// tag to 2026.2 mid-upgrade.
	ks.Spec.Image.Tag = "2026.2"

	r := newDBTestReconciler(s, ks)

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
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

// --- Upgrade abort tests (#468) ---

// TestReconcileDatabase_RevertToInstalledRelease_AbortsDuringExpand verifies that
// reverting spec.image.tag to status.installedRelease during the Expanding phase
// aborts the upgrade: the expand/migrate/contract Jobs are deleted, upgradePhase
// and targetRelease are cleared, installedRelease is left untouched, an
// UpgradeAborted event is emitted, and the reconcile requeues so the steady-state
// db_sync path takes over (#468).
func TestReconcileDatabase_RevertToInstalledRelease_AbortsDuringExpand(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseExpanding)
	// Operator reverts the tag from the in-flight target (2026.1) back to the
	// installed release (2025.2) to abort.
	ks.Spec.Image.Tag = "2025.2"

	// Pre-create the expand and migrate phase Jobs so the abort has something to
	// clean up (the contract Job is never created during Expanding).
	expandJob := buildExpandJob(ks, "keystone-config-abc123", "", "2026.1")
	migrateJob := buildMigrateJob(ks, "keystone-config-abc123", "", "2026.1")

	r := newDBTestReconciler(s, ks, expandJob, migrateJob)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{Requeue: true}))

	// Upgrade state cleared; installed release untouched.
	g.Expect(ks.Status.UpgradePhase).To(BeEmpty())
	g.Expect(ks.Status.TargetRelease).To(BeEmpty())
	g.Expect(ks.Status.InstalledRelease).To(Equal("2025.2"))

	// Assert absence of old state: every upgrade phase Job is gone.
	for _, suffix := range []string{"db-expand", "db-migrate", "db-contract"} {
		var jb batchv1.Job
		getErr := r.Get(context.Background(), client.ObjectKey{
			Name:      "test-keystone-" + suffix,
			Namespace: "default",
		}, &jb)
		g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
			"%s Job must be absent after abort", suffix)
	}

	expectEvent(g, r, "Normal UpgradeAborted")
}

// TestReconcileDatabase_RevertToInstalledRelease_AbortsFromAnyPhase verifies the
// abort path fires from every active upgrade phase — not just Expanding — and
// that reverting to the installed release never returns the UpgradeTargetChanged
// hard error a revert would otherwise trip (the reverted tag also differs from
// targetRelease). No phase Jobs are pre-created, so this also covers the
// idempotent NotFound delete path (#468).
func TestReconcileDatabase_RevertToInstalledRelease_AbortsFromAnyPhase(t *testing.T) {
	phases := []keystonev1alpha1.UpgradePhase{
		keystonev1alpha1.UpgradePhaseExpanding,
		keystonev1alpha1.UpgradePhaseMigrating,
		keystonev1alpha1.UpgradePhaseRollingUpdate,
		keystonev1alpha1.UpgradePhaseContracting,
	}
	for _, phase := range phases {
		t.Run(string(phase), func(t *testing.T) {
			g := NewGomegaWithT(t)
			s := dbTestScheme()
			ks := upgradingKeystone(phase)
			ks.Spec.Image.Tag = ks.Status.InstalledRelease

			r := newDBTestReconciler(s, ks)

			result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result).To(Equal(ctrl.Result{Requeue: true}))

			g.Expect(ks.Status.UpgradePhase).To(BeEmpty())
			g.Expect(ks.Status.TargetRelease).To(BeEmpty())
			g.Expect(ks.Status.InstalledRelease).To(Equal("2025.2"))

			expectEvent(g, r, "Normal UpgradeAborted")
		})
	}
}

// TestReconcileDatabase_AbortUpgrade_DeleteErrorRetainsState verifies that when
// deleting an upgrade phase Job fails with a non-NotFound error, the abort
// surfaces the error and leaves status.upgradePhase/targetRelease intact so the
// next reconcile retries the abort rather than dropping into the steady-state
// path with orphaned phase Jobs (#468).
func TestReconcileDatabase_AbortUpgrade_DeleteErrorRetainsState(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := upgradingKeystone(keystonev1alpha1.UpgradePhaseExpanding)
	ks.Spec.Image.Tag = "2025.2"

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*batchv1.Job); ok {
					return fmt.Errorf("simulated API server error")
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).
		Build()
	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("simulated API server error"))

	// Upgrade state must survive a failed abort so the retry can complete it.
	g.Expect(ks.Status.UpgradePhase).To(Equal(keystonev1alpha1.UpgradePhaseExpanding))
	g.Expect(ks.Status.TargetRelease).To(Equal("2026.1"))

	expectNoEvent(g, r)
}

// --- Schema check tests ---

// TestBuildSchemaCheckJob_Name verifies that the schema-check Job has the correct
// name ({keystone.Name}-schema-check) and namespace.
func TestBuildSchemaCheckJob_Name(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := brownfieldKeystone()

	j := buildSchemaCheckJob(ks, "keystone-config-abc123", "")

	g.Expect(j.Name).To(Equal("test-keystone-schema-check"))
	g.Expect(j.Namespace).To(Equal(ks.Namespace))
}

// TestBuildSchemaCheckJob_Image verifies that the schema-check container uses
// the correct Keystone image ({spec.image.repository}:{spec.image.tag}).
func TestBuildSchemaCheckJob_Image(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := brownfieldKeystone()

	j := buildSchemaCheckJob(ks, "keystone-config-abc123", "")

	container := findContainerByName(j.Spec.Template.Spec.Containers, "schema-check")
	g.Expect(container).NotTo(BeNil())
	g.Expect(container.Image).To(Equal(fmt.Sprintf("%s:%s", ks.Spec.Image.Repository, ks.Spec.Image.Tag)))
}

// TestBuildSchemaCheckJob_SecurityContext verifies that the schema-check container
// satisfies the PSS Restricted profile.
func TestBuildSchemaCheckJob_SecurityContext(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := brownfieldKeystone()

	j := buildSchemaCheckJob(ks, "keystone-config-abc123", "")

	container := findContainerByName(j.Spec.Template.Spec.Containers, "schema-check")
	expectRestrictedSecurityContext(g, container)
}

// TestBuildSchemaCheckJob_ConfigVolume verifies that the schema-check container
// mounts the config ConfigMap at /etc/keystone/keystone.conf.d/ read-only.
func TestBuildSchemaCheckJob_ConfigVolume(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := brownfieldKeystone()
	configMap := "keystone-config-abc123"

	j := buildSchemaCheckJob(ks, configMap, "")

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
// backoffLimit=2 (not the default 4 used by db_sync).
func TestBuildSchemaCheckJob_BackoffLimit(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := brownfieldKeystone()

	j := buildSchemaCheckJob(ks, "keystone-config-abc123", "")

	g.Expect(j.Spec.BackoffLimit).NotTo(BeNil())
	g.Expect(*j.Spec.BackoffLimit).To(Equal(int32(2)))
}

// TestBuildSchemaCheckJob_TTL verifies that the schema-check Job leaves
// ttlSecondsAfterFinished unset so the completed Job lingers as the RunJob
// state record instead of being garbage-collected and re-created in a loop
// (#415).
func TestBuildSchemaCheckJob_TTL(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := brownfieldKeystone()

	j := buildSchemaCheckJob(ks, "keystone-config-abc123", "")

	g.Expect(j.Spec.TTLSecondsAfterFinished).To(BeNil())
}

// TestBuildSchemaCheckJob_RestartPolicy verifies that the schema-check Job pod
// has RestartPolicy=Never.
func TestBuildSchemaCheckJob_RestartPolicy(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := brownfieldKeystone()

	j := buildSchemaCheckJob(ks, "keystone-config-abc123", "")

	g.Expect(j.Spec.Template.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
}

// TestBuildSchemaCheckJob_Command verifies that the schema-check container uses
// /bin/sh -eu -c with keystone-manage db_sync --check.
func TestBuildSchemaCheckJob_Command(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := brownfieldKeystone()

	j := buildSchemaCheckJob(ks, "keystone-config-abc123", "")

	container := findContainerByName(j.Spec.Template.Spec.Containers, "schema-check")
	g.Expect(container).NotTo(BeNil())
	g.Expect(container.Command).To(HaveLen(4))
	g.Expect(container.Command[:3]).To(Equal([]string{"/bin/sh", "-eu", "-c"}))
	g.Expect(container.Command[3]).To(ContainSubstring("keystone-manage"))
	g.Expect(container.Command[3]).To(ContainSubstring("db_sync --check"))
}

// TestReconcileDatabase_SchemaCheckRunning_Requeues verifies that when db_sync is
// complete but schema-check is still running, the reconciler requeues with
// RequeueDatabaseWait and sets the SchemaCheckInProgress condition.
func TestReconcileDatabase_SchemaCheckRunning_Requeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	r := newDBTestReconciler(s, ks, completedDBSyncJob(ks), runningSchemaCheckJob(ks))

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonSchemaCheckInProgress))

	expectNoEvent(g, r)
}

// TestReconcileDatabase_SchemaCheckComplete_DatabaseSynced verifies that when both
// db_sync and schema-check complete, DatabaseReady=True with reason DatabaseSynced
// and message containing 'revision'.
func TestReconcileDatabase_SchemaCheckComplete_DatabaseSynced(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	r := newDBTestReconciler(s, ks, completedDBSyncJob(ks), completedSchemaCheckJob(ks))

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(database.ReasonDatabaseSynced))
	g.Expect(cond.Message).To(ContainSubstring("revision"))

	expectEvent(g, r, "Normal DatabaseSynced")
}

// TestReconcileDatabase_SchemaCheckFailed_SchemaDriftDetected verifies that when
// the schema-check Job fails, DatabaseReady=False with reason SchemaDriftDetected
// and an error is returned.
func TestReconcileDatabase_SchemaCheckFailed_SchemaDriftDetected(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	r := newDBTestReconciler(s, ks, completedDBSyncJob(ks), failedSchemaCheckJob(ks))

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("schema-check"))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonSchemaDriftDetected))

	expectEvent(g, r, "Warning SchemaDriftDetected")
}

// TestReconcileDatabase_Managed_AllReady_WithSchemaCheck verifies that in managed
// mode, when all MariaDB CRs are ready and both db_sync and schema-check Jobs
// complete, DatabaseReady=True with reason DatabaseSynced and message containing
// 'revision'.
func TestReconcileDatabase_Managed_AllReady_WithSchemaCheck(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := managedKeystone()

	r := newDBTestReconciler(
		s, ks,
		readyMariaDBCluster(ks),
		readyDatabase(ks),
		readyUser(ks),
		readyGrant(ks),
		completedDBSyncJob(ks),
		completedSchemaCheckJob(ks),
	)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(database.ReasonDatabaseSynced))
	g.Expect(cond.Message).To(ContainSubstring("revision"))
	g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))

	expectEvent(g, r, "Normal DatabaseSynced")
}

// TestReconcileDatabase_SchemaCheckStale_Recreated verifies that a completed
// schema-check Job with a stale pod-spec hash triggers deletion and recreation,
// and the reconciler requeues.
func TestReconcileDatabase_SchemaCheckStale_Recreated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	// Completed schema-check with a stale hash.
	staleSchemaCheck := completedSchemaCheckJob(ks)
	staleSchemaCheck.Annotations[job.PodSpecHashAnnotation] = "stale-hash-from-previous-image"

	r := newDBTestReconciler(s, ks, completedDBSyncJob(ks), staleSchemaCheck)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))

	// Verify the old Job was deleted and a new one created with the correct hash.
	var newJob batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      fmt.Sprintf("%s-schema-check", ks.Name),
		Namespace: ks.Namespace,
	}, &newJob)).To(Succeed())

	desired := buildSchemaCheckJob(ks, "keystone-config-abc123", "")
	expectedHash := job.PodSpecHash(&desired.Spec.Template)
	g.Expect(newJob.Annotations[job.PodSpecHashAnnotation]).To(Equal(expectedHash))
}

// TestReconcileDatabase_SchemaCheckNotCreatedWhenDBSyncRunning verifies that when
// db_sync is still running, no schema-check Job is created and the condition
// remains DBSyncInProgress.
func TestReconcileDatabase_SchemaCheckNotCreatedWhenDBSyncRunning(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	// db_sync Job exists but is not completed (still running).
	syncJob := buildDBSyncJob(ks, "keystone-config-abc123", "")
	syncJob.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&syncJob.Spec.Template),
	}
	r := newDBTestReconciler(s, ks, syncJob)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonDBSyncInProgress))

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
// DBSyncFailed.
func TestReconcileDatabase_SchemaCheckNotCreatedWhenDBSyncFails(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	r := newDBTestReconciler(s, ks, failedDBSyncJob(ks))

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("db_sync"))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonDBSyncFailed))

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
// tag.
func TestReconcileDatabase_SchemaCheckFailed_InstalledReleaseNotUpdated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()
	// Fresh deploy — InstalledRelease is empty.

	r := newDBTestReconciler(s, ks, completedDBSyncJob(ks), failedSchemaCheckJob(ks))

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).To(HaveOccurred())

	// InstalledRelease must NOT be updated when schema-check fails.
	g.Expect(ks.Status.InstalledRelease).To(BeEmpty())

	expectEvent(g, r, "Warning SchemaDriftDetected")
}

// TestReconcileDatabase_ConditionObservedGeneration verifies that
// ObservedGeneration is set on the DatabaseReady condition for
// False (ClusterNotReady, WaitingForDatabase, DBSyncFailed,
// SchemaDriftDetected) and True (DatabaseSynced) paths with distinct
// generation values.
func TestReconcileDatabase_ConditionObservedGeneration(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()

	// Test ObservedGeneration for the ClusterNotReady path (cluster missing).
	ks := managedKeystone()
	ks.Generation = 7

	r := newDBTestReconciler(s, ks)

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.ObservedGeneration).To(Equal(int64(7)))

	// Test ObservedGeneration for the WaitingForDatabase path (User/Grant not ready).
	ks3 := managedKeystone()
	ks3.Generation = 5

	r3 := newDBTestReconciler(
		s, ks3,
		readyMariaDBCluster(ks3),
		readyDatabase(ks3),
		buildUser(ks3), // exists but not ready
	)

	_, err = r3.reconcileDatabase(context.Background(), ks3, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())

	cond3 := meta.FindStatusCondition(ks3.Status.Conditions, "DatabaseReady")
	g.Expect(cond3).NotTo(BeNil())
	g.Expect(cond3.ObservedGeneration).To(Equal(int64(5)))

	// Test ObservedGeneration for the DBSyncFailed path.
	ks4 := brownfieldKeystone()
	ks4.Generation = 9

	r4 := newDBTestReconciler(s, ks4, failedDBSyncJob(ks4))

	_, err = r4.reconcileDatabase(context.Background(), ks4, "keystone-config-abc123", "")
	g.Expect(err).To(HaveOccurred())

	cond4 := meta.FindStatusCondition(ks4.Status.Conditions, "DatabaseReady")
	g.Expect(cond4).NotTo(BeNil())
	g.Expect(cond4.ObservedGeneration).To(Equal(int64(9)))

	// Test ObservedGeneration for the SchemaDriftDetected path.
	ks5 := brownfieldKeystone()
	ks5.Generation = 15

	r5 := newDBTestReconciler(s, ks5, completedDBSyncJob(ks5), failedSchemaCheckJob(ks5))

	_, err = r5.reconcileDatabase(context.Background(), ks5, "keystone-config-abc123", "")
	g.Expect(err).To(HaveOccurred())

	cond5 := meta.FindStatusCondition(ks5.Status.Conditions, "DatabaseReady")
	g.Expect(cond5).NotTo(BeNil())
	g.Expect(cond5.ObservedGeneration).To(Equal(int64(15)))

	// Test ObservedGeneration for the DatabaseSynced path (all ready).
	ks2 := managedKeystone()
	ks2.Generation = 12

	r2 := newDBTestReconciler(
		s, ks2,
		readyMariaDBCluster(ks2),
		readyDatabase(ks2),
		readyUser(ks2),
		readyGrant(ks2),
		completedDBSyncJob(ks2),
		completedSchemaCheckJob(ks2),
	)

	_, err = r2.reconcileDatabase(context.Background(), ks2, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())

	cond2 := meta.FindStatusCondition(ks2.Status.Conditions, "DatabaseReady")
	g.Expect(cond2).NotTo(BeNil())
	g.Expect(cond2.ObservedGeneration).To(Equal(int64(12)))
}

// TestBuildDBSyncJob_PriorityClassNameSet verifies that when spec.PriorityClassName
// is set, the db-sync Job PodSpec includes the configured priority class.
func TestBuildDBSyncJob_PriorityClassNameSet(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := brownfieldKeystone()
	pcn := "system-cluster-critical"
	ks.Spec.Deployment.PriorityClassName = &pcn

	job := buildDBSyncJob(ks, "keystone-config-abc123", "")

	g.Expect(job.Spec.Template.Spec.PriorityClassName).To(Equal("system-cluster-critical"))
}

// TestBuildDBSyncJob_PriorityClassNameNil verifies that when spec.PriorityClassName
// is nil, the db-sync Job PodSpec has an empty priority class name.
func TestBuildDBSyncJob_PriorityClassNameNil(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := brownfieldKeystone()

	job := buildDBSyncJob(ks, "keystone-config-abc123", "")

	g.Expect(job.Spec.Template.Spec.PriorityClassName).To(BeEmpty())
}

// --- Finalizer test helpers ---

// mariaDBResources returns Database, User, and Grant CRs matching the names
// reconcileDatabase would create for the given Keystone CR. The namespace and
// name follow the same convention (keystone.Name, keystone.Namespace) used by
// buildDatabase/buildUser/buildGrant.
func mariaDBResources(ks *keystonev1alpha1.Keystone) (*mariadbv1alpha1.Database, *mariadbv1alpha1.User, *mariadbv1alpha1.Grant) {
	db := &mariadbv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{Name: ks.Name, Namespace: ks.Namespace},
	}
	user := &mariadbv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: ks.Name, Namespace: ks.Namespace},
	}
	grant := &mariadbv1alpha1.Grant{
		ObjectMeta: metav1.ObjectMeta{Name: ks.Name, Namespace: ks.Namespace},
	}
	return db, user, grant
}

// withPendingDeleteFinalizer adds a non-MariaDB finalizer to obj so that the
// fake client's Delete transitions it to Terminating (sets DeletionTimestamp)
// rather than removing it from the store. This simulates the real MariaDB
// operator deferring actual deletion while it tears down external resources
func withPendingDeleteFinalizer(obj client.Object) client.Object {
	obj.SetFinalizers([]string{"test.c5c3.io/pending-delete"})
	return obj
}

// TestFinalizeDatabaseResources_DeletesAllThreeCRs verifies that the handler
// issues Delete for Database, User, and Grant. The fake client removes objects
// synchronously when no finalizer is attached, so all three are absent after
// the single call.
func TestFinalizeDatabaseResources_DeletesAllThreeCRs(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := managedKeystone()
	db, user, grant := mariaDBResources(ks)

	r := newDBTestReconciler(s, ks, db, user, grant)

	err := r.finalizeDatabaseResources(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())

	// Confirm the CRs no longer exist.
	for _, obj := range []client.Object{
		&mariadbv1alpha1.Database{},
		&mariadbv1alpha1.User{},
		&mariadbv1alpha1.Grant{},
	} {
		err := r.Get(context.Background(), client.ObjectKey{Name: ks.Name, Namespace: ks.Namespace}, obj)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%T should be NotFound after finalize", obj)
	}
}

// TestFinalizeDatabaseResources_BrownfieldIsNoop verifies that a brownfield
// Keystone CR (Host-only, no ClusterRef) returns without error because no
// MariaDB CRs were ever created and every Delete returns NotFound.
func TestFinalizeDatabaseResources_BrownfieldIsNoop(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	r := newDBTestReconciler(s, ks)

	err := r.finalizeDatabaseResources(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred(), "brownfield finalize must not error on absent MariaDB CRs")
}

// TestFinalizeDatabaseResources_NotFoundIsTolerated verifies that having only a
// subset of the three MariaDB CRs present does not cause an error. Delete
// returning NotFound on absent resources is tolerated and the present CR is
// still deleted.
func TestFinalizeDatabaseResources_NotFoundIsTolerated(t *testing.T) {
	testCases := []struct {
		name    string
		present func(ks *keystonev1alpha1.Keystone) []client.Object
	}{
		{
			name: "only Database present",
			present: func(ks *keystonev1alpha1.Keystone) []client.Object {
				db, _, _ := mariaDBResources(ks)
				return []client.Object{db}
			},
		},
		{
			name: "only User present",
			present: func(ks *keystonev1alpha1.Keystone) []client.Object {
				_, user, _ := mariaDBResources(ks)
				return []client.Object{user}
			},
		},
		{
			name: "only Grant present",
			present: func(ks *keystonev1alpha1.Keystone) []client.Object {
				_, _, grant := mariaDBResources(ks)
				return []client.Object{grant}
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			s := dbTestScheme()
			ks := managedKeystone()
			objs := append([]client.Object{ks}, tc.present(ks)...)
			r := newDBTestReconciler(s, objs...)

			err := r.finalizeDatabaseResources(context.Background(), ks)

			g.Expect(err).NotTo(HaveOccurred())
		})
	}
}

// TestFinalizeDatabaseResources_IsIdempotent verifies that a second invocation
// after a successful cleanup produces the same outcome without error, so
// re-entering the finalizer (operator restart, retry, external deletion) never
// blocks CR removal.
func TestFinalizeDatabaseResources_IsIdempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := managedKeystone()

	r := newDBTestReconciler(s, ks)

	g.Expect(r.finalizeDatabaseResources(context.Background(), ks)).To(Succeed())
	g.Expect(r.finalizeDatabaseResources(context.Background(), ks)).
		To(Succeed(), "re-invocation after completion remains a no-op")
}

// TestFinalizeDatabaseResources_IssuesDeleteWhenTerminating verifies that when
// a MariaDB CR is held in Terminating state by another finalizer, the handler
// still returns success and marks it for deletion — it no longer blocks the
// Keystone finalizer on the MariaDB operator completing its teardown
func TestFinalizeDatabaseResources_IssuesDeleteWhenTerminating(t *testing.T) {
	testCases := []struct {
		name       string
		terminates func(ks *keystonev1alpha1.Keystone) client.Object
	}{
		{
			name: "Database terminates",
			terminates: func(ks *keystonev1alpha1.Keystone) client.Object {
				db, _, _ := mariaDBResources(ks)
				return withPendingDeleteFinalizer(db)
			},
		},
		{
			name: "User terminates",
			terminates: func(ks *keystonev1alpha1.Keystone) client.Object {
				_, user, _ := mariaDBResources(ks)
				return withPendingDeleteFinalizer(user)
			},
		},
		{
			name: "Grant terminates",
			terminates: func(ks *keystonev1alpha1.Keystone) client.Object {
				_, _, grant := mariaDBResources(ks)
				return withPendingDeleteFinalizer(grant)
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			s := dbTestScheme()
			ks := managedKeystone()
			held := tc.terminates(ks)
			r := newDBTestReconciler(s, ks, held)

			g.Expect(r.finalizeDatabaseResources(context.Background(), ks)).To(Succeed())

			// The held resource must now carry a DeletionTimestamp.
			fresh := held.DeepCopyObject().(client.Object)
			fresh.SetFinalizers(nil)
			g.Expect(r.Get(context.Background(), client.ObjectKeyFromObject(held), fresh)).To(Succeed())
			g.Expect(fresh.GetDeletionTimestamp().IsZero()).To(BeFalse(),
				"held resource must be marked for deletion before the finalizer returns")
		})
	}
}

// TestFinalizeDatabaseResources_DeleteErrorIsPropagated verifies that a
// non-NotFound error from Delete propagates as a reconciler error so
// controller-runtime retries with backoff.
func TestFinalizeDatabaseResources_DeleteErrorIsPropagated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := managedKeystone()
	db, _, _ := mariaDBResources(ks)

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks, db).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*mariadbv1alpha1.Database); ok {
					return fmt.Errorf("simulated API server error")
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).
		Build()
	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	err := r.finalizeDatabaseResources(context.Background(), ks)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("simulated API server error"))
}

// --- db_sync metrics tests ---

// dbSyncMetricsTestKeystone returns a brownfield Keystone CR with per-test
// Name/Namespace so db_sync metric tests never share counter series.
func dbSyncMetricsTestKeystone(name, ns string) *keystonev1alpha1.Keystone {
	ks := brownfieldKeystone()
	ks.Name = name
	ks.Namespace = ns
	ks.UID = types.UID(name + "-uid")
	return ks
}

// TestDbSyncCompletionRecordsMetric verifies that reconcileDatabase records a
// "succeeded" db_sync metric when the db_sync Job transitions to Complete=True.
// Post-W-002 the same metric is also emitted for the post-sync schema-check
// Job, so a single reconcile contributes one sample for each — both deduped
// independently by per-phase Keystone CR annotation. The duration histogram
// observes condition.LastTransitionTime minus Job.CreationTimestamp on the
// db_sync sample. A second reconcile with the same per-phase Job UIDs must
// not re-emit.
func TestDbSyncCompletionRecordsMetric(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := dbSyncMetricsTestKeystone("dbsync-complete", "ns-dbsync-complete")

	desired := buildDBSyncJob(ks, "keystone-config-abc123", "")
	created := metav1.NewTime(time.Date(2026, 4, 22, 10, 0, 0, 0, time.UTC))
	terminated := metav1.NewTime(time.Date(2026, 4, 22, 10, 0, 15, 0, time.UTC))
	wantDBSyncDuration := 15 * time.Second

	dbJob := desired.DeepCopy()
	dbJob.UID = types.UID("dbsync-complete-job-uid")
	dbJob.CreationTimestamp = created
	dbJob.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template),
	}
	dbJob.Status.Succeeded = 1
	dbJob.Status.CompletionTime = &terminated
	dbJob.Status.Conditions = []batchv1.JobCondition{{
		Type:               batchv1.JobComplete,
		Status:             corev1.ConditionTrue,
		LastTransitionTime: terminated,
	}}

	r := newDBTestReconciler(s, ks, dbJob, completedSchemaCheckJob(ks))

	counterLabels := map[string]string{
		"keystone":  ks.Name,
		"namespace": ks.Namespace,
		"result":    "succeeded",
	}
	durationLabels := map[string]string{
		"keystone":  ks.Name,
		"namespace": ks.Namespace,
	}

	beforeCount := counterValue(t, "keystone_operator_db_sync_total", counterLabels)
	beforeSamples := histogramSampleCount(t, "keystone_operator_db_sync_duration_seconds", durationLabels)
	beforeSum := histogramSampleSum(t, "keystone_operator_db_sync_duration_seconds", durationLabels)

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())

	afterCount := counterValue(t, "keystone_operator_db_sync_total", counterLabels)
	afterSamples := histogramSampleCount(t, "keystone_operator_db_sync_duration_seconds", durationLabels)
	afterSum := histogramSampleSum(t, "keystone_operator_db_sync_duration_seconds", durationLabels)

	// Two terminal transitions are observed in a single reconcile:
	// db_sync (succeeded, 15 s) and the subsequent schema-check (succeeded,
	// 0 s — its Job timestamps are unset in the test helper).
	g.Expect(afterCount-beforeCount).To(Equal(2.0),
		"succeeded counter must increment by 1 for db_sync and 1 for schema-check on a single reconcile")
	g.Expect(afterSamples-beforeSamples).To(Equal(uint64(2)),
		"duration histogram must observe one sample per terminated DB-related Job")
	g.Expect(afterSum-beforeSum).To(BeNumerically("~", wantDBSyncDuration.Seconds(), 0.01),
		"histogram sample_sum delta equals the db_sync duration (schema-check helper has zero-valued timestamps so contributes 0)")

	// Idempotence: a second reconcile with the same per-phase Job UIDs must
	// NOT re-emit. Each phase keeps an independent
	// dedupe annotation, so neither db_sync nor schema-check should fire
	// again.
	_, _ = r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	finalCount := counterValue(t, "keystone_operator_db_sync_total", counterLabels)
	g.Expect(finalCount).To(Equal(afterCount),
		"metric MUST be emitted at-most-once per (phase, Job UID)")
}

// TestRecordDBJobTerminalState_NilObserved_NoOp verifies that a nil observed
// Job (RunJob returns nil after creating the Job) is a no-op: no metric is
// emitted and no dedupe annotation is set.
func TestRecordDBJobTerminalState_NilObserved_NoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := dbSyncMetricsTestKeystone("nil-observed", "ns-nil-observed")
	r := newDBTestReconciler(s, ks)

	labels := map[string]string{"keystone": ks.Name, "namespace": ks.Namespace, "result": "succeeded"}
	before := counterValue(t, "keystone_operator_db_sync_total", labels)

	r.recordDBJobTerminalState(context.Background(), ks, "db-sync", nil)

	after := counterValue(t, "keystone_operator_db_sync_total", labels)
	g.Expect(after).To(Equal(before), "a nil observed Job must not emit a metric")
	g.Expect(ks.Annotations).NotTo(HaveKey(dbJobUIDAnnotationKey("db-sync")))
}

// TestRecordDBJobTerminalState_UsesObservedWithoutGet verifies that the terminal
// metric is emitted from the threaded observed Job without re-reading it: an
// interceptor errors on any Job Get, so a successful emission proves no Get
// occurred.
func TestRecordDBJobTerminalState_UsesObservedWithoutGet(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := dbSyncMetricsTestKeystone("observed-noget", "ns-observed-noget")

	created := metav1.NewTime(time.Date(2026, 4, 22, 14, 0, 0, 0, time.UTC))
	terminated := metav1.NewTime(time.Date(2026, 4, 22, 14, 0, 9, 0, time.UTC))
	observed := buildDBSyncJob(ks, "keystone-config-abc123", "")
	observed.UID = types.UID("observed-noget-job-uid")
	observed.CreationTimestamp = created
	observed.Status.Succeeded = 1
	observed.Status.CompletionTime = &terminated
	observed.Status.Conditions = []batchv1.JobCondition{{
		Type:               batchv1.JobComplete,
		Status:             corev1.ConditionTrue,
		LastTransitionTime: terminated,
	}}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		WithStatusSubresource(&keystonev1alpha1.Keystone{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, wc client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isJob := obj.(*batchv1.Job); isJob {
					return fmt.Errorf("recordDBJobTerminalState must not Get the Job; it takes the observed object")
				}
				return wc.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	r := &KeystoneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	labels := map[string]string{"keystone": ks.Name, "namespace": ks.Namespace, "result": "succeeded"}
	before := counterValue(t, "keystone_operator_db_sync_total", labels)

	r.recordDBJobTerminalState(context.Background(), ks, "db-sync", observed)

	after := counterValue(t, "keystone_operator_db_sync_total", labels)
	g.Expect(after-before).To(Equal(1.0), "the metric must be emitted from the observed Job without a re-Get")
	g.Expect(ks.Annotations).To(HaveKey(dbJobUIDAnnotationKey("db-sync")))
}

// TestDbSyncFailureRecordsMetric verifies that reconcileDatabase records a
// "failed" db_sync metric when the db_sync Job transitions to Failed=True.
// reconcileDatabase propagates the job.ErrJobFailed error to its caller; the
// metric must still be emitted on the terminal-transition observation path
func TestDbSyncFailureRecordsMetric(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := dbSyncMetricsTestKeystone("dbsync-failed", "ns-dbsync-failed")

	desired := buildDBSyncJob(ks, "keystone-config-abc123", "")
	created := metav1.NewTime(time.Date(2026, 4, 22, 11, 0, 0, 0, time.UTC))
	terminated := metav1.NewTime(time.Date(2026, 4, 22, 11, 0, 7, 0, time.UTC))
	wantDuration := 7 * time.Second

	dbJob := desired.DeepCopy()
	dbJob.UID = types.UID("dbsync-failed-job-uid")
	dbJob.CreationTimestamp = created
	dbJob.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template),
	}
	dbJob.Status.Failed = 5
	dbJob.Status.Conditions = []batchv1.JobCondition{{
		Type:               batchv1.JobFailed,
		Status:             corev1.ConditionTrue,
		LastTransitionTime: terminated,
	}}

	r := newDBTestReconciler(s, ks, dbJob)

	counterLabels := map[string]string{
		"keystone":  ks.Name,
		"namespace": ks.Namespace,
		"result":    "failed",
	}
	durationLabels := map[string]string{
		"keystone":  ks.Name,
		"namespace": ks.Namespace,
	}

	beforeCount := counterValue(t, "keystone_operator_db_sync_total", counterLabels)
	beforeSamples := histogramSampleCount(t, "keystone_operator_db_sync_duration_seconds", durationLabels)

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).To(HaveOccurred(),
		"reconcileDatabase must surface the job.ErrJobFailed error to the caller")

	afterCount := counterValue(t, "keystone_operator_db_sync_total", counterLabels)
	afterSamples := histogramSampleCount(t, "keystone_operator_db_sync_duration_seconds", durationLabels)

	g.Expect(afterCount-beforeCount).To(Equal(1.0),
		"failed counter must increment by 1 on terminal Failed transition")
	g.Expect(afterSamples-beforeSamples).To(Equal(uint64(1)),
		"duration histogram must observe exactly one sample on terminal Failed transition")

	m := findMetricByLabels(t, ctrlmetrics.Registry, "keystone_operator_db_sync_duration_seconds", durationLabels)
	g.Expect(m).NotTo(BeNil())
	g.Expect(m.GetHistogram().GetSampleSum()).To(BeNumerically("~", wantDuration.Seconds(), 0.01),
		"histogram sample_sum must equal condition.LastTransitionTime minus Job.CreationTimestamp")
}

// TestDbSyncInProgressDoesNotRecord verifies that reconcileDatabase does NOT
// emit a db_sync metric while the Job is still running (no terminal
// condition). Polling an unfinished Job must not inflate the counter
func TestDbSyncInProgressDoesNotRecord(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := dbSyncMetricsTestKeystone("dbsync-running", "ns-dbsync-running")

	desired := buildDBSyncJob(ks, "keystone-config-abc123", "")
	dbJob := desired.DeepCopy()
	dbJob.UID = types.UID("dbsync-running-job-uid")
	dbJob.CreationTimestamp = metav1.NewTime(time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC))
	dbJob.Annotations = map[string]string{
		job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template),
	}
	// Active Job: no terminal condition.
	dbJob.Status.Active = 1

	r := newDBTestReconciler(s, ks, dbJob)

	successLabels := map[string]string{
		"keystone":  ks.Name,
		"namespace": ks.Namespace,
		"result":    "succeeded",
	}
	failedLabels := map[string]string{
		"keystone":  ks.Name,
		"namespace": ks.Namespace,
		"result":    "failed",
	}
	durationLabels := map[string]string{
		"keystone":  ks.Name,
		"namespace": ks.Namespace,
	}

	beforeSuccess := counterValue(t, "keystone_operator_db_sync_total", successLabels)
	beforeFailed := counterValue(t, "keystone_operator_db_sync_total", failedLabels)
	beforeSamples := histogramSampleCount(t, "keystone_operator_db_sync_duration_seconds", durationLabels)

	_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())

	afterSuccess := counterValue(t, "keystone_operator_db_sync_total", successLabels)
	afterFailed := counterValue(t, "keystone_operator_db_sync_total", failedLabels)
	afterSamples := histogramSampleCount(t, "keystone_operator_db_sync_duration_seconds", durationLabels)

	g.Expect(afterSuccess-beforeSuccess).To(Equal(0.0),
		"running Job must NOT increment the succeeded counter")
	g.Expect(afterFailed-beforeFailed).To(Equal(0.0),
		"running Job must NOT increment the failed counter")
	g.Expect(afterSamples-beforeSamples).To(Equal(uint64(0)),
		"running Job must NOT observe a duration sample")
}

// TestUpgradePhaseFailureRecordsDBSyncMetric verifies that each
// expand-migrate-contract phase Job contributes a `failed` increment to the
// db_sync metric on terminal failure. Without this,
// the dashboard panel and `keystone_operator_db_sync_total{result="failed"}`
// alerts go blank for the duration of an upgrade.
func TestUpgradePhaseFailureRecordsDBSyncMetric(t *testing.T) {
	cases := []struct {
		phaseTag  keystonev1alpha1.UpgradePhase
		jobPhase  string
		jobFlag   string
		nsSuffix  string
		nameSlug  string
		failedKey string
	}{
		{keystonev1alpha1.UpgradePhaseExpanding, "expand", "--expand", "expand-failed", "upgrade-expand-failed", "ExpandFailed"},
		{keystonev1alpha1.UpgradePhaseMigrating, "migrate", "--migrate", "migrate-failed", "upgrade-migrate-failed", "MigrateFailed"},
		{keystonev1alpha1.UpgradePhaseContracting, "contract", "--contract", "contract-failed", "upgrade-contract-failed", "ContractFailed"},
	}
	for _, tc := range cases {
		t.Run(tc.jobPhase, func(t *testing.T) {
			g := NewGomegaWithT(t)
			s := dbTestScheme()
			ks := upgradingKeystone(tc.phaseTag)
			ks.Name = tc.nameSlug
			ks.Namespace = "ns-" + tc.nsSuffix
			ks.UID = types.UID(tc.nameSlug + "-uid")

			failedJob := failedUpgradeJob(ks, "keystone-config-abc123", ks.Spec.Image.Tag, tc.jobPhase, tc.jobFlag)

			r := newDBTestReconciler(s, ks, failedJob)

			failedLabels := map[string]string{
				"keystone":  ks.Name,
				"namespace": ks.Namespace,
				"result":    "failed",
			}
			durationLabels := map[string]string{
				"keystone":  ks.Name,
				"namespace": ks.Namespace,
			}

			beforeFailed := counterValue(t, "keystone_operator_db_sync_total", failedLabels)
			beforeSamples := histogramSampleCount(t, "keystone_operator_db_sync_duration_seconds", durationLabels)

			_, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
			g.Expect(err).To(HaveOccurred(),
				"the failing upgrade-phase Job MUST surface as a reconcile error so callers retry")

			afterFailed := counterValue(t, "keystone_operator_db_sync_total", failedLabels)
			afterSamples := histogramSampleCount(t, "keystone_operator_db_sync_duration_seconds", durationLabels)

			g.Expect(afterFailed-beforeFailed).To(Equal(1.0),
				"%s phase failure must contribute one increment to db_sync_total{result=failed}", tc.jobPhase)
			g.Expect(afterSamples-beforeSamples).To(Equal(uint64(1)),
				"%s phase failure must contribute one duration sample", tc.jobPhase)

			cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal(tc.failedKey),
				"DatabaseReady reason MUST identify the failing phase so operators can attribute the metric increment")
		})
	}
}

// TestRecordDBJobTerminalState_DefersOnPatchFailure verifies the W-003
// ordering invariant: when the dedupe annotation patch fails, the metric is
// NOT emitted on this pass. The next reconcile re-evaluates the same Job and
// either emits then (after a successful patch) or defers again — but the
// at-most-once-per-(phase, Job UID) guarantee documented in
// docs/reference/keystone-operator-metrics.md is preserved against transient
// apiserver failures.
//
// Table-driven across every phase that calls recordDBJobTerminalState
// (review-2 suggestion 2): db-sync, db-expand, db-migrate,
// db-contract, schema-check. A regression that re-orders Patch and
// metrics.RecordDBSync in any single caller is then caught by this test
// alone, locking the W-003 invariant for the full set of caller sites at
// reconcile_database.go:329, :358, :546, :581, :626.
func TestRecordDBJobTerminalState_DefersOnPatchFailure(t *testing.T) {
	created := metav1.NewTime(time.Date(2026, 4, 22, 13, 0, 0, 0, time.UTC))
	terminated := metav1.NewTime(time.Date(2026, 4, 22, 13, 0, 5, 0, time.UTC))

	cases := []struct {
		// jobSuffix is the per-phase suffix passed to
		// recordDBJobTerminalState; it also selects the dedupe annotation
		// key via dbJobUIDAnnotationKey.
		jobSuffix string
		// buildJob returns the Job that the operator would observe for the
		// phase. Each case constructs the Job via the same builder used in
		// production so the test breaks loudly if Job naming or labels
		// drift.
		buildJob func(*keystonev1alpha1.Keystone, string) *batchv1.Job
		// nameSuffix isolates per-case Prometheus series so counter samples
		// do not bleed across subtests.
		nameSuffix string
	}{
		{
			jobSuffix: "db-sync",
			buildJob: func(ks *keystonev1alpha1.Keystone, cm string) *batchv1.Job {
				return buildDBSyncJob(ks, cm, "")
			},
			nameSuffix: "dbsync",
		},
		{
			jobSuffix: "db-expand",
			buildJob: func(ks *keystonev1alpha1.Keystone, cm string) *batchv1.Job {
				return buildExpandJob(ks, cm, "", ks.Spec.Image.Tag)
			},
			nameSuffix: "dbexpand",
		},
		{
			jobSuffix: "db-migrate",
			buildJob: func(ks *keystonev1alpha1.Keystone, cm string) *batchv1.Job {
				return buildMigrateJob(ks, cm, "", ks.Spec.Image.Tag)
			},
			nameSuffix: "dbmigrate",
		},
		{
			jobSuffix: "db-contract",
			buildJob: func(ks *keystonev1alpha1.Keystone, cm string) *batchv1.Job {
				return buildContractJob(ks, cm, "", ks.Spec.Image.Tag)
			},
			nameSuffix: "dbcontract",
		},
		{
			jobSuffix: "schema-check",
			buildJob: func(ks *keystonev1alpha1.Keystone, cm string) *batchv1.Job {
				return buildSchemaCheckJob(ks, cm, "")
			},
			nameSuffix: "schemacheck",
		},
	}

	for _, tc := range cases {
		t.Run(tc.jobSuffix, func(t *testing.T) {
			g := NewGomegaWithT(t)
			s := dbTestScheme()
			ks := dbSyncMetricsTestKeystone(
				tc.nameSuffix+"-patch-failure",
				"ns-"+tc.nameSuffix+"-patch-failure",
			)

			desired := tc.buildJob(ks, "keystone-config-abc123")
			dbJob := desired.DeepCopy()
			dbJob.UID = types.UID(tc.nameSuffix + "-patch-failure-job-uid")
			dbJob.CreationTimestamp = created
			dbJob.Annotations = map[string]string{
				job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template),
			}
			dbJob.Status.Succeeded = 1
			dbJob.Status.CompletionTime = &terminated
			dbJob.Status.Conditions = []batchv1.JobCondition{{
				Type:               batchv1.JobComplete,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: terminated,
			}}

			cb := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(ks, dbJob).
				WithStatusSubresource(&keystonev1alpha1.Keystone{}, &mariadbv1alpha1.Database{}, &mariadbv1alpha1.User{}, &mariadbv1alpha1.Grant{}).
				WithInterceptorFuncs(interceptor.Funcs{
					Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
						if _, isKeystone := obj.(*keystonev1alpha1.Keystone); isKeystone {
							return fmt.Errorf("simulated apiserver Patch failure (%s)", tc.jobSuffix)
						}
						return nil
					},
				})
			recorder := record.NewFakeRecorder(10)
			r := &KeystoneReconciler{
				Client:   cb.Build(),
				Scheme:   s,
				Recorder: recorder,
			}

			successLabels := map[string]string{
				"keystone":  ks.Name,
				"namespace": ks.Namespace,
				"result":    "succeeded",
			}
			durationLabels := map[string]string{
				"keystone":  ks.Name,
				"namespace": ks.Namespace,
			}

			beforeSuccess := counterValue(t, "keystone_operator_db_sync_total", successLabels)
			beforeSamples := histogramSampleCount(t, "keystone_operator_db_sync_duration_seconds", durationLabels)

			r.recordDBJobTerminalState(context.Background(), ks, tc.jobSuffix, dbJob)

			afterSuccess := counterValue(t, "keystone_operator_db_sync_total", successLabels)
			afterSamples := histogramSampleCount(t, "keystone_operator_db_sync_duration_seconds", durationLabels)

			g.Expect(afterSuccess-beforeSuccess).To(Equal(0.0),
				"%s: metric MUST NOT be emitted when the dedupe annotation patch fails", tc.jobSuffix)
			g.Expect(afterSamples-beforeSamples).To(Equal(uint64(0)),
				"%s: duration histogram MUST NOT receive a sample when the patch fails", tc.jobSuffix)
			g.Expect(ks.Annotations).NotTo(HaveKey(dbJobUIDAnnotationKey(tc.jobSuffix)),
				"%s: failed Patch MUST NOT mirror the dedupe annotation back onto the in-memory CR", tc.jobSuffix)

			// review-2 suggestion 1: persistent Patch failure surfaces a
			// CR-visible Warning event so the at-most-once-per-UID
			// degradation is not silent at default log levels.
			g.Expect(recorder.Events).To(Receive(ContainSubstring("Warning DBSyncMetricEmissionDeferred")),
				"%s: deferred-emission MUST raise a Warning event on the Keystone CR", tc.jobSuffix)
		})
	}
}

// dbTLSManagedKeystoneForJobs returns a managed-mode Keystone CR (ClusterRef
// set) with DB TLS enabled, used to assert that every db_sync Job variant
// projects the db-tls Secret into its pod.
func dbTLSManagedKeystoneForJobs() *keystonev1alpha1.Keystone {
	ks := managedKeystone()
	ks.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-server-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "test-keystone-db-client"},
	}
	return ks
}

// TestBuildDBJobVariants_DBTLSVolumeAndMount_WhenEnabled verifies that every
// variant produced by buildDBJob (db-sync, expand, migrate, contract,
// schema-check) carries the Secret-backed "db-tls" Volume and a matching
// read-only VolumeMount at /etc/keystone/db-tls/ when TLS is enabled
// Name-based assertions only — additive
// volumes must not perturb the existing volume ordering.
func TestBuildDBJobVariants_DBTLSVolumeAndMount_WhenEnabled(t *testing.T) {
	ks := dbTLSManagedKeystoneForJobs()

	cases := []struct {
		name          string
		job           *batchv1.Job
		containerName string
	}{
		{"db-sync", buildDBSyncJob(ks, "keystone-config-abc123", ""), "db-sync"},
		{"expand", buildExpandJob(ks, "keystone-config-abc123", "", ks.Spec.Image.Tag), "db-expand"},
		{"migrate", buildMigrateJob(ks, "keystone-config-abc123", "", ks.Spec.Image.Tag), "db-migrate"},
		{"contract", buildContractJob(ks, "keystone-config-abc123", "", ks.Spec.Image.Tag), "db-contract"},
		{"schema-check", buildSchemaCheckJob(ks, "keystone-config-abc123", ""), "schema-check"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			var tlsVol corev1.Volume
			var tlsVolFound bool
			for _, v := range tc.job.Spec.Template.Spec.Volumes {
				if v.Name == "db-tls" {
					tlsVol = v
					tlsVolFound = true
					break
				}
			}
			g.Expect(tlsVolFound).To(BeTrue(),
				"%s: db-tls Volume must be present when TLS enabled",
				tc.name)
			g.Expect(tlsVol.Projected).NotTo(BeNil(),
				"%s: db-tls Volume must be Projected so both Secret refs are honored",
				tc.name)
			g.Expect(tlsVol.Projected.DefaultMode).NotTo(BeNil(),
				"%s: db-tls Volume must set DefaultMode",
				tc.name)
			g.Expect(*tlsVol.Projected.DefaultMode).To(Equal(int32(0o400)),
				"%s: db-tls Volume DefaultMode must be 0o400 (owner read-only)",
				tc.name)
			expectDBTLSProjection(g, tlsVol.Projected, "db-server-ca", "test-keystone-db-client")

			container := findContainerByName(tc.job.Spec.Template.Spec.Containers, tc.containerName)
			g.Expect(container).NotTo(BeNil())
			var tlsMount corev1.VolumeMount
			var tlsMountFound bool
			for _, m := range container.VolumeMounts {
				if m.Name == "db-tls" {
					tlsMount = m
					tlsMountFound = true
					break
				}
			}
			g.Expect(tlsMountFound).To(BeTrue(),
				"%s: db-tls VolumeMount must be present on container when TLS enabled",
				tc.name)
			g.Expect(tlsMount.MountPath).To(Equal("/etc/keystone/db-tls/"),
				"%s: db-tls VolumeMount path must match ssl_* DSN parameter directory",
				tc.name)
			g.Expect(tlsMount.ReadOnly).To(BeTrue(),
				"%s: db-tls VolumeMount must be read-only",
				tc.name)
		})
	}
}

// TestBuildDBJobVariants_DBTLSVolume_UsesUserSuppliedSecretNames verifies the
// BLOCKER fix from review #1 across all db_sync Job variants: the projected
// db-tls Volume must reference the user-supplied caBundleSecretRef.Name and
// clientCertSecretRef.Name verbatim (not a hardcoded "<name>-db-client" name).
// This exercises the brownfield/enterprise-PKI shape where the trust bundle
// and client keypair live in separate Secrets.
func TestBuildDBJobVariants_DBTLSVolume_UsesUserSuppliedSecretNames(t *testing.T) {
	ks := managedKeystone()
	ks.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "enterprise-root-ca-bundle"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "site-specific-client-keypair"},
	}

	cases := []struct {
		name string
		job  *batchv1.Job
	}{
		{"db-sync", buildDBSyncJob(ks, "keystone-config-abc123", "")},
		{"expand", buildExpandJob(ks, "keystone-config-abc123", "", ks.Spec.Image.Tag)},
		{"migrate", buildMigrateJob(ks, "keystone-config-abc123", "", ks.Spec.Image.Tag)},
		{"contract", buildContractJob(ks, "keystone-config-abc123", "", ks.Spec.Image.Tag)},
		{"schema-check", buildSchemaCheckJob(ks, "keystone-config-abc123", "")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			tlsVol := findVolumeByName(tc.job.Spec.Template.Spec.Volumes, "db-tls")
			g.Expect(tlsVol).NotTo(BeNil(),
				"%s: db-tls Volume must be present", tc.name)
			g.Expect(tlsVol.Projected).NotTo(BeNil(),
				"%s: db-tls Volume must be Projected", tc.name)
			expectDBTLSProjection(g, tlsVol.Projected,
				"enterprise-root-ca-bundle", "site-specific-client-keypair")
		})
	}
}

// TestBuildDBJobVariants_DBTLSVolumeAbsent_WhenNil verifies that no db-tls
// Volume or VolumeMount is added by any db_sync Job variant when
// spec.database.tls is nil — preserves pre-existing behaviour.
func TestBuildDBJobVariants_DBTLSVolumeAbsent_WhenNil(t *testing.T) {
	ks := brownfieldKeystone() // TLS == nil

	cases := []struct {
		name          string
		job           *batchv1.Job
		containerName string
	}{
		{"db-sync", buildDBSyncJob(ks, "keystone-config-abc123", ""), "db-sync"},
		{"expand", buildExpandJob(ks, "keystone-config-abc123", "", ks.Spec.Image.Tag), "db-expand"},
		{"migrate", buildMigrateJob(ks, "keystone-config-abc123", "", ks.Spec.Image.Tag), "db-migrate"},
		{"contract", buildContractJob(ks, "keystone-config-abc123", "", ks.Spec.Image.Tag), "db-contract"},
		{"schema-check", buildSchemaCheckJob(ks, "keystone-config-abc123", ""), "schema-check"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(ks.Spec.Database.TLS).To(BeNil(),
				"precondition: brownfieldKeystone must leave Database.TLS nil")

			for _, v := range tc.job.Spec.Template.Spec.Volumes {
				g.Expect(v.Name).NotTo(Equal("db-tls"),
					"%s: db-tls Volume must NOT be present when TLS is nil",
					tc.name)
			}
			container := findContainerByName(tc.job.Spec.Template.Spec.Containers, tc.containerName)
			g.Expect(container).NotTo(BeNil())
			for _, m := range container.VolumeMounts {
				g.Expect(m.Name).NotTo(Equal("db-tls"),
					"%s: db-tls VolumeMount must NOT be present when TLS is nil",
					tc.name)
			}
		})
	}
}

// TestBuildDBJobVariants_DBTLSVolumeAbsent_WhenDisabled verifies the
// disabled-mode gate for every Job variant: a TLS block with mode: "disabled"
// must not project the keypair into the Job pod.
func TestBuildDBJobVariants_DBTLSVolumeAbsent_WhenDisabled(t *testing.T) {
	ks := managedKeystone()
	ks.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "disabled",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-server-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "test-keystone-db-client"},
	}

	cases := []struct {
		name          string
		job           *batchv1.Job
		containerName string
	}{
		{"db-sync", buildDBSyncJob(ks, "keystone-config-abc123", ""), "db-sync"},
		{"expand", buildExpandJob(ks, "keystone-config-abc123", "", ks.Spec.Image.Tag), "db-expand"},
		{"migrate", buildMigrateJob(ks, "keystone-config-abc123", "", ks.Spec.Image.Tag), "db-migrate"},
		{"contract", buildContractJob(ks, "keystone-config-abc123", "", ks.Spec.Image.Tag), "db-contract"},
		{"schema-check", buildSchemaCheckJob(ks, "keystone-config-abc123", ""), "schema-check"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			for _, v := range tc.job.Spec.Template.Spec.Volumes {
				g.Expect(v.Name).NotTo(Equal("db-tls"),
					"%s: db-tls Volume must NOT be present when TLS mode is disabled",
					tc.name)
			}
			container := findContainerByName(tc.job.Spec.Template.Spec.Containers, tc.containerName)
			g.Expect(container).NotTo(BeNil())
			for _, m := range container.VolumeMounts {
				g.Expect(m.Name).NotTo(Equal("db-tls"),
					"%s: db-tls VolumeMount must NOT be present when TLS mode is disabled",
					tc.name)
			}
		})
	}
}

// TestReconcileDatabase_CompletedSchemaCheck_SameHash_NotRecreated verifies the
// steady state (#415): when the schema-check Job is already
// complete and its pod-spec hash matches the desired spec, the reconciler must
// NOT delete or recreate the Job. Since TTL was removed, the completed Job
// lingers as the RunJob pod-spec-hash state record and must carry no TTL.
// Recreation is detected via the retained Job's UID staying unchanged.
func TestReconcileDatabase_CompletedSchemaCheck_SameHash_NotRecreated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := dbTestScheme()
	ks := brownfieldKeystone()

	completed := completedSchemaCheckJob(ks)
	originalUID := completed.UID

	r := newDBTestReconciler(s, ks, completedDBSyncJob(ks), completed)

	result, err := r.reconcileDatabase(context.Background(), ks, "keystone-config-abc123", "")
	g.Expect(err).NotTo(HaveOccurred())
	// Steady state: no churn, no requeue.
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(database.ReasonDatabaseSynced))

	// The completed schema-check Job must still exist with the SAME UID,
	// proving it was neither deleted nor recreated.
	var retained batchv1.Job
	g.Expect(r.Client.Get(context.Background(), client.ObjectKey{
		Name:      fmt.Sprintf("%s-schema-check", ks.Name),
		Namespace: ks.Namespace,
	}, &retained)).To(Succeed())
	g.Expect(retained.UID).To(Equal(originalUID))

	// The lingering Job carries no TTL (: TTL removed to stop the loop).
	g.Expect(retained.Spec.TTLSecondsAfterFinished).To(BeNil())
}
