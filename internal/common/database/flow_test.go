// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/forge/internal/common/job"
	commonv1 "github.com/c5c3/forge/internal/common/types"
)

const (
	flowInstance  = "keystone"
	flowNamespace = "openstack"
)

func managedDBSpec() *commonv1.DatabaseSpec {
	return &commonv1.DatabaseSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"},
		Database:   "keystone",
		SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
	}
}

// flowOwner is an owner CR in the flow namespace so SetControllerReference on
// the provisioned/owned resources does not trip the cross-namespace guard.
func flowOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-owner", Namespace: flowNamespace, UID: "flow-uid"},
	}
}

func readyMariaDB() *mariadbv1alpha1.MariaDB {
	m := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "mariadb", Namespace: flowNamespace},
	}
	meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Running",
	})
	return m
}

func readyDatabaseCR() *mariadbv1alpha1.Database {
	db := &mariadbv1alpha1.Database{ObjectMeta: metav1.ObjectMeta{Name: flowInstance, Namespace: flowNamespace}}
	meta.SetStatusCondition(&db.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Created",
	})
	return db
}

func flowScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = mariadbv1alpha1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	return s
}

func provisionParamsFor(spec *commonv1.DatabaseSpec, conds *[]metav1.Condition, owner client.Object) ProvisionFlowParams {
	return ProvisionFlowParams{
		Client:        nil, // set by caller
		Scheme:        nil,
		Owner:         owner,
		InstanceName:  flowInstance,
		Namespace:     flowNamespace,
		Database:      spec,
		Conditions:    conds,
		Generation:    1,
		ConditionType: "DatabaseReady",
		RequeueAfter:  30 * time.Second,
	}
}

// --- IsClusterReady ---

func TestIsClusterReady(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	spec := managedDBSpec()
	ctx := context.Background()

	// Absent cluster -> false, nil.
	c := fake.NewClientBuilder().WithScheme(s).Build()
	ready, err := IsClusterReady(ctx, c, spec, flowNamespace)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())

	// Not-ready cluster -> false, nil.
	notReady := &mariadbv1alpha1.MariaDB{ObjectMeta: metav1.ObjectMeta{Name: "mariadb", Namespace: flowNamespace}}
	c = fake.NewClientBuilder().WithScheme(s).WithObjects(notReady).Build()
	ready, err = IsClusterReady(ctx, c, spec, flowNamespace)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())

	// Ready cluster -> true, nil.
	c = fake.NewClientBuilder().WithScheme(s).WithObjects(readyMariaDB()).Build()
	ready, err = IsClusterReady(ctx, c, spec, flowNamespace)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
}

func TestIsClusterReady_transientGetError(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	boom := errors.New("apiserver unavailable")
	c := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return boom
		},
	}).Build()

	ready, err := IsClusterReady(context.Background(), c, managedDBSpec(), flowNamespace)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("apiserver unavailable"))
	g.Expect(ready).To(BeFalse())
}

// --- ReconcileProvision ---

func TestReconcileProvision_brownfieldNoOp(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()

	spec := &commonv1.DatabaseSpec{Host: "db.example.com", Database: "keystone", SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"}}
	p := provisionParamsFor(spec, &conds, owner)
	p.Client, p.Scheme = c, s

	res, err := ReconcileProvision(context.Background(), p)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	// No condition is set on the brownfield no-op path.
	g.Expect(conds).To(BeEmpty())
}

func TestReconcileProvision_clusterNotReady(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()

	p := provisionParamsFor(managedDBSpec(), &conds, owner)
	p.Client, p.Scheme = c, s

	res, err := ReconcileProvision(context.Background(), p)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(30 * time.Second))
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal(ReasonClusterNotReady))
	g.Expect(cond.Message).To(Equal(`MariaDB cluster "mariadb" is not ready`))
}

func TestReconcileProvision_databaseNotReady(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	// Cluster Ready but no Database CR yet: EnsureDatabase applies it and reports
	// not-ready.
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner, readyMariaDB()).Build()

	p := provisionParamsFor(managedDBSpec(), &conds, owner)
	p.Client, p.Scheme = c, s

	res, err := ReconcileProvision(context.Background(), p)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(30 * time.Second))
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(ReasonWaitingForDatabase))
	g.Expect(cond.Message).To(Equal("MariaDB Database CR is not ready"))
}

func TestReconcileProvision_staticWaitsForUser(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	// Cluster + Database Ready, no User/Grant: Static mode must wait on them.
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, readyMariaDB(), readyDatabaseCR()).
		WithStatusSubresource(readyDatabaseCR()).
		Build()

	p := provisionParamsFor(managedDBSpec(), &conds, owner)
	p.Client, p.Scheme = c, s

	res, err := ReconcileProvision(context.Background(), p)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(30 * time.Second))
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(ReasonWaitingForDatabase))
	g.Expect(cond.Message).To(Equal("MariaDB User or Grant CR is not ready"))
}

func TestReconcileProvision_dynamicSkipsUser(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	// Cluster + Database Ready, Dynamic mode: the User/Grant are engine-owned, so
	// provisioning is complete without them.
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, readyMariaDB(), readyDatabaseCR()).
		WithStatusSubresource(readyDatabaseCR()).
		Build()

	spec := managedDBSpec()
	spec.CredentialsMode = commonv1.CredentialsModeDynamic
	p := provisionParamsFor(spec, &conds, owner)
	p.Client, p.Scheme = c, s

	res, err := ReconcileProvision(context.Background(), p)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	// No User/Grant CR was created.
	users := &mariadbv1alpha1.UserList{}
	g.Expect(c.List(context.Background(), users)).To(Succeed())
	g.Expect(users.Items).To(BeEmpty())
}

// --- ReconcileSyncJobs ---

// syncParams builds a keystone-shaped SyncFlowParams against c, recording every
// terminal callback into *calls.
func syncParams(c client.Client, s *runtime.Scheme, owner client.Object, conds *[]metav1.Condition, rec record.EventRecorder, installed *string, calls *[]string) SyncFlowParams {
	return SyncFlowParams{
		Client:   c,
		Scheme:   s,
		Recorder: rec,
		Owner:    owner,
		Jobs:     keystoneJobSet(),
		RecordTerminal: func(jobSuffix string, _ *batchv1.Job) {
			*calls = append(*calls, jobSuffix)
		},
		Conditions:       conds,
		Generation:       1,
		ConditionType:    "DatabaseReady",
		RequeueAfter:     30 * time.Second,
		InstalledRelease: installed,
		ImageTag:         "2026.1",
	}
}

func completedJob(name string, hash string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: flowNamespace,
			Annotations: map[string]string{job.PodSpecHashAnnotation: hash},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
		}},
	}
}

func failedJob(name string, hash string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: flowNamespace,
			Annotations: map[string]string{job.PodSpecHashAnnotation: hash},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
		}},
	}
}

func TestReconcileSyncJobs_dbSyncInProgress(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	var calls []string
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()

	res, err := ReconcileSyncJobs(context.Background(), syncParams(c, s, owner, &conds, nil, nil, &calls))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(30 * time.Second))
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(ReasonDBSyncInProgress))
	// The db-sync Job was created and the terminal callback observed it.
	g.Expect(calls).To(Equal([]string{"db-sync"}))
	created := &batchv1.Job{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "keystone-db-sync", Namespace: flowNamespace}, created)).To(Succeed())
}

func TestReconcileSyncJobs_schemaCheckSequenced(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	var calls []string
	p := keystoneJobSet()
	syncHash := job.PodSpecHash(&SyncJob(p).Spec.Template)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, completedJob("keystone-db-sync", syncHash)).
		Build()

	res, err := ReconcileSyncJobs(context.Background(), syncParams(c, s, owner, &conds, nil, nil, &calls))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(30 * time.Second))
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(ReasonSchemaCheckInProgress))
	// db-sync observed (complete) then schema-check created.
	g.Expect(calls).To(Equal([]string{"db-sync", "schema-check"}))
	created := &batchv1.Job{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "keystone-schema-check", Namespace: flowNamespace}, created)).To(Succeed())
}

func TestReconcileSyncJobs_success(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	var calls []string
	rec := record.NewFakeRecorder(10)
	installed := ""
	p := keystoneJobSet()
	syncHash := job.PodSpecHash(&SyncJob(p).Spec.Template)
	checkHash := job.PodSpecHash(&SchemaCheckJob(p).Spec.Template)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(
			owner,
			completedJob("keystone-db-sync", syncHash),
			completedJob("keystone-schema-check", checkHash),
		).Build()

	res, err := ReconcileSyncJobs(context.Background(), syncParams(c, s, owner, &conds, rec, &installed, &calls))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(ReasonDatabaseSynced))
	// InstalledRelease is promoted to the image tag on success.
	g.Expect(installed).To(Equal("2026.1"))
	g.Expect(rec.Events).To(Receive(ContainSubstring(ReasonDatabaseSynced)))
}

func TestReconcileSyncJobs_dbSyncFailed(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	var calls []string
	rec := record.NewFakeRecorder(10)
	installed := ""
	p := keystoneJobSet()
	syncHash := job.PodSpecHash(&SyncJob(p).Spec.Template)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, failedJob("keystone-db-sync", syncHash)).
		Build()

	res, err := ReconcileSyncJobs(context.Background(), syncParams(c, s, owner, &conds, rec, &installed, &calls))
	g.Expect(err).To(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(ReasonDBSyncFailed))
	// InstalledRelease is NOT promoted on failure.
	g.Expect(installed).To(BeEmpty())
	g.Expect(rec.Events).To(Receive(ContainSubstring(ReasonDBSyncFailed)))
}

// TestReconcileSyncJobs_noSchemaCheck exercises the nil-SchemaCheckCommand edge
// path: a service without a schema-check step reaches DatabaseSynced straight off
// a completed db-sync, and no schema-check Job is created.
func TestReconcileSyncJobs_noSchemaCheck(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	var calls []string
	p := keystoneJobSet()
	p.SchemaCheckCommand = nil
	syncHash := job.PodSpecHash(&SyncJob(p).Spec.Template)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, completedJob("keystone-db-sync", syncHash)).
		Build()

	params := syncParams(c, s, owner, &conds, nil, nil, &calls)
	params.Jobs = p
	res, err := ReconcileSyncJobs(context.Background(), params)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(ReasonDatabaseSynced))
	g.Expect(calls).To(Equal([]string{"db-sync"}))
	// No schema-check Job was created.
	check := &batchv1.Job{}
	err = c.Get(context.Background(), client.ObjectKey{Name: "keystone-schema-check", Namespace: flowNamespace}, check)
	g.Expect(err).To(HaveOccurred())
}
