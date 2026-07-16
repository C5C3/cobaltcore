// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/database"
	"github.com/c5c3/forge/internal/common/job"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

// dbTestScheme adds the MariaDB API to the shared test scheme so the managed
// provisioning path can construct MariaDB CRs.
func dbTestScheme() *runtime.Scheme {
	s := testScheme()
	_ = mariadbv1alpha1.AddToScheme(s)
	return s
}

// newDBTestReconciler builds a GlanceReconciler over a fake client seeded with
// objs, using the MariaDB-aware scheme.
func newDBTestReconciler(s *runtime.Scheme, objs ...client.Object) *GlanceReconciler {
	return &GlanceReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
			WithStatusSubresource(&glancev1alpha1.Glance{}, &glancev1alpha1.GlanceBackend{}).Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(50),
	}
}

// managedGlance returns a Glance in managed database mode (ClusterRef set).
func managedGlance() *glancev1alpha1.Glance {
	glance := testGlance()
	glance.Spec.Database = commonv1.DatabaseSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"},
		Database:   "glance",
		SecretRef:  commonv1.SecretRefSpec{Name: "glance-db"},
	}
	return glance
}

func TestReconcileDatabase_ProvisionGatesOnClusterReady(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := managedGlance()
	// The referenced MariaDB cluster does not exist yet, so provisioning gates.
	r := newDBTestReconciler(dbTestScheme(), glance)

	res, err := r.reconcileDatabase(context.Background(), glance, "test-glance-config-abc")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonClusterNotReady))
}

func TestReconcileDatabase_NoConfigWaitsForBackends(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance() // brownfield, OpenStackRelease 2026.1
	r := newGlanceTestReconciler(glance)

	res, err := r.reconcileDatabase(context.Background(), glance, "")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	g.Expect(glance.Status.TargetRelease).To(Equal("2026.1"), "TargetRelease is stamped")
	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDatabaseWaitingForBackends))
}

func TestReconcileDatabase_DowngradeRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.OpenStackRelease = "2025.2"
	glance.Status.InstalledRelease = "2026.1"
	r := newGlanceTestReconciler(glance)

	res, err := r.reconcileDatabase(context.Background(), glance, "test-glance-config-abc")

	g.Expect(err).NotTo(HaveOccurred())
	// A rejection must return a non-zero requeue so RunPipeline short-circuits
	// before the Deployment step; a zero result would let the workload roll to
	// the new code against the un-migrated (downgraded) schema.
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait), "an invalid transition short-circuits the pipeline, it is not a zero result")
	g.Expect(glance.Status.TargetRelease).To(Equal("2025.2"))
	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonInvalidReleaseTransition))
}

func TestReconcileDatabase_NonSequentialJumpRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.OpenStackRelease = "2026.1"
	glance.Status.InstalledRelease = "2025.1" // skips 2025.2
	r := newGlanceTestReconciler(glance)

	res, err := r.reconcileDatabase(context.Background(), glance, "test-glance-config-abc")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait), "a non-sequential jump short-circuits the pipeline, it is not a zero result")
	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(conditionReasonInvalidReleaseTransition))
}

func TestReconcileDatabase_PatchOnlyAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.OpenStackRelease = "2026.1-p1" // patch of the installed release
	glance.Status.InstalledRelease = "2026.1"
	r := newGlanceTestReconciler(glance)

	res, err := r.reconcileDatabase(context.Background(), glance, "test-glance-config-abc")

	g.Expect(err).NotTo(HaveOccurred())
	// Accepted: the step proceeds to db-sync (a fresh Job is in progress) rather
	// than rejecting the transition.
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(database.ReasonDBSyncInProgress))
}

func TestReconcileDatabase_SequentialUpgradeAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	glance.Spec.OpenStackRelease = "2026.1"
	glance.Status.InstalledRelease = "2025.2"
	r := newGlanceTestReconciler(glance)

	res, err := r.reconcileDatabase(context.Background(), glance, "test-glance-config-abc")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(database.ReasonDBSyncInProgress))
}

func TestReconcileDatabase_SyncJobCommandAndEnv(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	r := newGlanceTestReconciler(glance)

	_, err := r.reconcileDatabase(context.Background(), glance, "test-glance-config-abc")
	g.Expect(err).NotTo(HaveOccurred())

	var syncJob batchv1.Job
	key := client.ObjectKey{Namespace: "default", Name: "test-glance-db-sync"}
	g.Expect(r.Get(context.Background(), key, &syncJob)).To(Succeed())

	container := syncJob.Spec.Template.Spec.Containers[0]
	g.Expect(container.Command).To(Equal([]string{
		"glance-manage", "--config-dir", "/etc/glance/glance-api.conf.d/", "db", "sync",
	}))

	var connEnv *corev1.EnvVar
	for i := range container.Env {
		if container.Env[i].Name == database.ConnectionEnvVarName {
			connEnv = &container.Env[i]
		}
	}
	g.Expect(connEnv).NotTo(BeNil(), "the db-sync Job overrides [database].connection via env")
	g.Expect(connEnv.ValueFrom.SecretKeyRef.Name).To(Equal(database.ConnectionSecretName(glance.Name)))
}

func TestReconcileDatabase_InstalledReleasePromotedOnSuccess(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance() // InstalledRelease empty (fresh), OpenStackRelease 2026.1
	configMapName := "test-glance-config-abc"

	// Seed a completed db-sync Job matching the desired pod-spec hash so
	// ReconcileSyncJobs observes it as done and promotes InstalledRelease.
	desired := database.SyncJob(glanceJobSetParams(glance, configMapName))
	now := metav1.Now()
	completed := desired.DeepCopy()
	completed.UID = "sync-job-uid"
	completed.Annotations = map[string]string{job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template)}
	completed.Status.Succeeded = 1
	completed.Status.CompletionTime = &now
	completed.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}
	r := newGlanceTestReconciler(glance, completed)

	res, err := r.reconcileDatabase(context.Background(), glance, configMapName)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(glance.Status.InstalledRelease).To(Equal("2026.1"),
		"InstalledRelease is promoted to spec.openStackRelease on db-sync success")
	cond := conditions.GetCondition(glance.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(database.ReasonDatabaseSynced))

	// The db-sync terminal metric dedupe annotation is stamped on the CR so the
	// metric emits at most once per Job UID.
	g.Expect(glance.Annotations).To(HaveKey(dbJobUIDAnnotationKey("db-sync")))
}
