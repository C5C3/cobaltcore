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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/database"
	"github.com/c5c3/forge/internal/common/job"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	barbicanv1alpha1 "github.com/c5c3/forge/operators/barbican/api/v1alpha1"
)

// dbConfigSecretName is the rendered config Secret the database step mounts in
// the migration Job. The database tests never render it, so any stable name
// does.
const dbConfigSecretName = "test-barbican-config-abc"

// syncJobKey is the object key of the db-sync Job built for the shared test
// fixture.
var syncJobKey = client.ObjectKey{Namespace: testNamespace, Name: testBarbicanName + "-db-sync"}

// managedBarbican returns a Barbican in managed database mode (ClusterRef set),
// the mode in which the shared provisioning flow gates on the MariaDB cluster's
// readiness.
func managedBarbican() *barbicanv1alpha1.Barbican {
	barbican := testBarbican()
	barbican.Spec.Database = commonv1.DatabaseSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"},
		Database:   "barbican",
		SecretRef:  commonv1.SecretRefSpec{Name: "barbican-db"},
	}
	return barbican
}

// terminatedSyncJob returns the db-sync Job for barbican marked terminal with
// the given condition type, carrying the desired pod-spec hash (so the runner
// accepts it as the current Job) and a stable UID for the terminal-metric
// dedupe.
func terminatedSyncJob(barbican *barbicanv1alpha1.Barbican, condType batchv1.JobConditionType, uid string) *batchv1.Job {
	desired := database.SyncJob(barbicanJobSetParams(barbican, dbConfigSecretName))
	now := metav1.Now()
	j := desired.DeepCopy()
	j.UID = types.UID(uid)
	j.Annotations = map[string]string{job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template)}
	j.Status.Conditions = []batchv1.JobCondition{
		{Type: condType, Status: corev1.ConditionTrue, LastTransitionTime: now},
	}
	if condType == batchv1.JobComplete {
		j.Status.Succeeded = 1
		j.Status.CompletionTime = &now
	} else {
		j.Status.Failed = 5
	}
	return j
}

// expectNoSyncJob asserts that no db-sync Job was created for the shared test
// fixture — the observable difference between a rejected release transition and
// an accepted one.
func expectNoSyncJob(t *testing.T, r *BarbicanReconciler) {
	t.Helper()
	g := NewGomegaWithT(t)
	var syncJob batchv1.Job
	err := r.Get(context.Background(), syncJobKey, &syncJob)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no migration Job may run on this path")
}

// barbicanMaxUserConnections must track the CR's own worker topology: every
// uWSGI worker holds a pooled connection once loaded, so a cap below
// pods × processes × threads can never let the fleet become fully ready (the
// last workers fail their start-up store sync with MySQL error 1226 and
// --need-app crash-loops their pod). The default topology must keep sizing to
// 10, the cap the mariadb-operator CRD default granted it before the operator
// owned the field.
func TestBarbicanMaxUserConnections(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*barbicanv1alpha1.Barbican)
		expected int32
	}{
		{
			name:     "defaults keep the historical cap of 10",
			mutate:   func(*barbicanv1alpha1.Barbican) {},
			expected: (3+1)*2*1 + 2,
		},
		{
			name: "raised process count raises the cap",
			mutate: func(b *barbicanv1alpha1.Barbican) {
				b.Spec.Deployment.Replicas = 3
				b.Spec.APIServer = &barbicanv1alpha1.APIServerSpec{
					UWSGI: &barbicanv1alpha1.UWSGISpec{Processes: 4},
				}
			},
			expected: (3+1)*4*1 + 2,
		},
		{
			name: "threads multiply per-worker concurrency",
			mutate: func(b *barbicanv1alpha1.Barbican) {
				b.Spec.APIServer = &barbicanv1alpha1.APIServerSpec{
					UWSGI: &barbicanv1alpha1.UWSGISpec{Processes: 2, Threads: 2},
				}
			},
			expected: (3+1)*2*2 + 2,
		},
		{
			name: "autoscaling sizes for the HPA ceiling",
			mutate: func(b *barbicanv1alpha1.Barbican) {
				b.Spec.Autoscaling = &barbicanv1alpha1.AutoscalingSpec{MaxReplicas: 5}
				b.Spec.APIServer = &barbicanv1alpha1.APIServerSpec{
					UWSGI: &barbicanv1alpha1.UWSGISpec{Processes: 4},
				}
			},
			expected: (5+1)*4*1 + 2,
		},
		{
			name: "zero-valued replicas normalize to the default",
			mutate: func(b *barbicanv1alpha1.Barbican) {
				b.Spec.Deployment.Replicas = 0
			},
			expected: (3+1)*2*1 + 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			barbican := testBarbican()
			tc.mutate(barbican)
			g.Expect(barbicanMaxUserConnections(barbican)).To(Equal(tc.expected))
		})
	}
}

// The sized cap must actually reach the provisioned User CR: the tempest
// topology (3 replicas x 4 processes = 12 pooled connections) exceeded the CRD
// default of 10 and crash-looped a pod at every cold start until the operator
// owned the field.
func TestReconcileDatabase_SizesTheUserConnectionCap(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := managedBarbican()
	barbican.Spec.APIServer = &barbicanv1alpha1.APIServerSpec{
		UWSGI: &barbicanv1alpha1.UWSGISpec{Processes: 4},
	}

	readyMariaDB := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "mariadb", Namespace: testNamespace},
	}
	meta.SetStatusCondition(&readyMariaDB.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Running",
	})
	readyDatabase := &mariadbv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{Name: testBarbicanName, Namespace: testNamespace},
	}
	meta.SetStatusCondition(&readyDatabase.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Created",
	})
	r := newBarbicanTestReconciler(barbican, readyMariaDB, readyDatabase)

	_, err := r.reconcileDatabase(context.Background(), barbican, dbConfigSecretName)
	g.Expect(err).NotTo(HaveOccurred())

	user := &mariadbv1alpha1.User{}
	g.Expect(r.Get(context.Background(), client.ObjectKey{Name: testBarbicanName, Namespace: testNamespace}, user)).To(Succeed())
	g.Expect(user.Spec.MaxUserConnections).To(Equal(int32((3+1)*4*1 + 2)))
}

// TestReconcileDatabase_SyncJobCommandAndMounts pins what the migration
// actually runs and reads: barbican-manage resolves barbican.conf through
// oslo.config's search path, so the config Secret has to land on that path, and
// the DB URL arrives through the env override rather than the rendered file.
func TestReconcileDatabase_SyncJobCommandAndMounts(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	r := newBarbicanTestReconciler(barbican)

	_, err := r.reconcileDatabase(context.Background(), barbican, dbConfigSecretName)
	g.Expect(err).NotTo(HaveOccurred())

	var syncJob batchv1.Job
	g.Expect(r.Get(context.Background(), syncJobKey, &syncJob)).To(Succeed())

	container := syncJob.Spec.Template.Spec.Containers[0]
	g.Expect(container.Command).To(Equal([]string{"barbican-manage", "db", "upgrade"}))
	g.Expect(container.VolumeMounts[0].MountPath).To(Equal("/etc/barbican"))

	// The rendered config is a Secret, not a ConfigMap: barbican.conf carries the
	// vault plugin's approle_role_id.
	g.Expect(syncJob.Spec.Template.Spec.Volumes).To(HaveLen(1))
	g.Expect(syncJob.Spec.Template.Spec.Volumes[0].ConfigMap).To(BeNil())
	g.Expect(syncJob.Spec.Template.Spec.Volumes[0].Secret.SecretName).To(Equal(dbConfigSecretName))

	var connEnv *corev1.EnvVar
	for i := range container.Env {
		if container.Env[i].Name == "OS_DATABASE__CONNECTION" {
			connEnv = &container.Env[i]
		}
	}
	g.Expect(connEnv).NotTo(BeNil(),
		"the db-sync Job overrides [database] connection via env, not via the config Secret")
	g.Expect(connEnv.ValueFrom.SecretKeyRef.Name).To(Equal(database.ConnectionSecretName(barbican.Name)))
}

// TestReconcileDatabase_SyncJobProjectsDBTLSKeypair pins the db-sync Job against
// the DSN the same CR derives: with database TLS enabled, the
// <name>-db-connection Secret carries ssl_ca/ssl_cert/ssl_key paths under
// dbTLSMountPath, so a Job pod without that mount cannot open them and
// barbican-manage fails every migration until the backoff limit is burned.
func TestReconcileDatabase_SyncJobProjectsDBTLSKeypair(t *testing.T) {
	g := NewGomegaWithT(t)

	// Disabled: no volume, no mount.
	plain := database.SyncJob(barbicanJobSetParams(testBarbican(), dbConfigSecretName))
	for _, v := range plain.Spec.Template.Spec.Volumes {
		g.Expect(v.Name).NotTo(Equal(dbTLSVolumeName))
	}

	barbican := testBarbican()
	barbican.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "barbican-db-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "barbican-db-client"},
	}
	r := newBarbicanTestReconciler(barbican)

	_, err := r.reconcileDatabase(context.Background(), barbican, dbConfigSecretName)
	g.Expect(err).NotTo(HaveOccurred())

	var syncJob batchv1.Job
	g.Expect(r.Get(context.Background(), syncJobKey, &syncJob)).To(Succeed())

	tlsVol, tlsMount := barbicanDBTLSVolumeAndMount(barbican)
	g.Expect(syncJob.Spec.Template.Spec.Volumes).To(ContainElement(tlsVol))
	g.Expect(syncJob.Spec.Template.Spec.Containers[0].VolumeMounts).To(ContainElement(tlsMount))
	g.Expect(tlsMount.MountPath).To(Equal(dbTLSMountPath),
		"the mount must land on the directory the derived DSN's ssl_* paths point at")
}

func TestReconcileDatabase_ProvisionGatesOnClusterReady(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := managedBarbican()
	// The referenced MariaDB cluster does not exist yet, so provisioning gates.
	r := newBarbicanTestReconciler(barbican)

	res, err := r.reconcileDatabase(context.Background(), barbican, dbConfigSecretName)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	cond := barbicanCondition(barbican, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonClusterNotReady))
	expectNoSyncJob(t, r)
}

// A first install whose secret stores are not ready yet has no rendered config
// to migrate against. The migration Job mounts that Secret as its whole
// /etc/barbican, so building one with an empty name would render a volume the
// API server rejects and wedge every pass on the Job create.
func TestReconcileDatabase_WaitsWithoutARenderedConfig(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	r := newBarbicanTestReconciler(barbican)

	res, err := r.reconcileDatabase(context.Background(), barbican, "")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	cond := barbicanCondition(barbican, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForSecretStores))
	expectNoSyncJob(t, r)
}

func TestReconcileDatabase_FreshInstallRunsOneJob(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican() // InstalledRelease empty
	r := newBarbicanTestReconciler(barbican)

	res, err := r.reconcileDatabase(context.Background(), barbican, dbConfigSecretName)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))

	var jobs batchv1.JobList
	g.Expect(r.List(context.Background(), &jobs, client.InNamespace(testNamespace))).To(Succeed())
	g.Expect(jobs.Items).To(HaveLen(1), "the migration is one Job, not a phase chain")
	g.Expect(jobs.Items[0].Name).To(Equal(syncJobKey.Name))

	cond := barbicanCondition(barbican, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonDBSyncInProgress))
	g.Expect(barbican.Status.TargetRelease).To(BeEmpty(),
		"a fresh install converges to its only release; there is no bump to advertise")
}

func TestReconcileDatabase_InstalledReleasePromotedOnSuccess(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican() // InstalledRelease empty, OpenStackRelease 2026.1
	r := newBarbicanTestReconciler(barbican, terminatedSyncJob(barbican, batchv1.JobComplete, "sync-complete-uid"))

	res, err := r.reconcileDatabase(context.Background(), barbican, dbConfigSecretName)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(barbican.Status.InstalledRelease).To(Equal("2026.1"),
		"installedRelease is promoted to spec.openStackRelease on db-sync success")
	g.Expect(barbican.Status.TargetRelease).To(BeEmpty())
	cond := barbicanCondition(barbican, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(database.ReasonDatabaseSynced))

	// The terminal metric stamped its dedupe annotation on the CR.
	g.Expect(barbican.Annotations).To(HaveKey(dbJobUIDAnnotationKey("db-sync")))
}

// TestReconcileDatabase_FailedJobIsAHardError covers the failure path: the
// condition, the Warning event, and the returned error that makes the controller
// back off instead of hammering a migration that cannot succeed.
func TestReconcileDatabase_FailedJobIsAHardError(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	failed := terminatedSyncJob(barbican, batchv1.JobFailed, "sync-failed-uid")
	r := newBarbicanTestReconciler(barbican, failed)

	_, err := r.reconcileDatabase(context.Background(), barbican, dbConfigSecretName)

	g.Expect(err).To(HaveOccurred(),
		"a permanently failed db-sync surfaces as a reconcile error so the controller backs off")
	cond := barbicanCondition(barbican, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonDBSyncFailed))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("Warning " + database.ReasonDBSyncFailed)))
	g.Expect(barbican.Annotations).To(HaveKey(dbJobUIDAnnotationKey("db-sync")),
		"the terminal metric bridge must stamp the dedupe annotation on the failure path too")
}

// TestReconcileDatabase_ReleaseBumpTracksTargetRelease walks an accepted
// sequential bump: the target is advertised while the Job runs against the new
// release's image, and it is cleared once the sync flow promotes the installed
// release.
func TestReconcileDatabase_ReleaseBumpTracksTargetRelease(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Spec.OpenStackRelease = "2026.1"
	barbican.Spec.Image.Tag = "2026.1"
	barbican.Status.InstalledRelease = "2025.2"
	r := newBarbicanTestReconciler(barbican)

	res, err := r.reconcileDatabase(context.Background(), barbican, dbConfigSecretName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	g.Expect(barbican.Status.TargetRelease).To(Equal("2026.1"))
	g.Expect(barbican.Status.InstalledRelease).To(Equal("2025.2"),
		"the marker only moves once the schema is actually migrated")

	var syncJob batchv1.Job
	g.Expect(r.Get(context.Background(), syncJobKey, &syncJob)).To(Succeed())
	g.Expect(syncJob.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/c5c3/barbican:2026.1"),
		"the migration runs the release being upgraded to")

	// The Job completes.
	now := metav1.Now()
	syncJob.Status.Succeeded = 1
	syncJob.Status.CompletionTime = &now
	syncJob.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
	}
	g.Expect(r.Status().Update(context.Background(), &syncJob)).To(Succeed())

	res, err = r.reconcileDatabase(context.Background(), barbican, dbConfigSecretName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(barbican.Status.InstalledRelease).To(Equal("2026.1"))
	g.Expect(barbican.Status.InstalledImage).To(Equal(barbican.Spec.Image.Reference()))
	g.Expect(barbican.Status.TargetRelease).To(BeEmpty(),
		"a CR that reached its target advertises no target")
}

// TestReconcileDatabase_RejectedTransitions walks every arm of the release gate.
// All four reject before a Job is launched, which is the point: a migration the
// gate would refuse must never touch the schema.
func TestReconcileDatabase_RejectedTransitions(t *testing.T) {
	cases := []struct {
		name string
		// mutate turns the shared fixture into the rejected transition.
		mutate func(*barbicanv1alpha1.Barbican)
		reason string
		// wantCause is the substring the returned error must carry, so the message
		// names the offending value rather than swallowing it.
		wantCause string
	}{
		{
			name: "downgrade",
			mutate: func(b *barbicanv1alpha1.Barbican) {
				// Release and image tag name 2025.2 in lockstep, so the mismatch guard
				// passes and the downgrade is what the gate rejects.
				b.Spec.OpenStackRelease = "2025.2"
				b.Spec.Image.Tag = "2025.2"
				b.Status.InstalledRelease = "2026.1"
			},
			reason:    database.ReasonDowngradeNotSupported,
			wantCause: "downgrade",
		},
		{
			name: "non-sequential jump",
			mutate: func(b *barbicanv1alpha1.Barbican) {
				b.Spec.OpenStackRelease = "2026.1"
				b.Spec.Image.Tag = "2026.1"
				b.Status.InstalledRelease = "2025.1" // skips 2025.2
			},
			reason:    database.ReasonUpgradePathInvalid,
			wantCause: "sequential",
		},
		{
			name: "unparseable installed release",
			mutate: func(b *barbicanv1alpha1.Barbican) {
				b.Status.InstalledRelease = "not-a-release"
			},
			reason:    database.ReasonVersionParseError,
			wantCause: `invalid release format "not-a-release"`,
		},
		{
			name: "release bump leaving the image untouched",
			mutate: func(b *barbicanv1alpha1.Barbican) {
				// Digest-pinned: nothing in the pod template changes, so the shared flow
				// would short-circuit on the completed Job and promote off a run of the
				// previous release's binary.
				b.Spec.Image.Tag = ""
				b.Spec.Image.Digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
				b.Spec.OpenStackRelease = "2026.1"
				b.Status.InstalledRelease = "2025.2"
				b.Status.InstalledImage = b.Spec.Image.Reference()
			},
			reason:    conditionReasonImageReleaseMismatch,
			wantCause: "leaves spec.image unchanged",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			barbican := testBarbican()
			tc.mutate(barbican)
			r := newBarbicanTestReconciler(barbican)

			_, err := r.reconcileDatabase(context.Background(), barbican, dbConfigSecretName)

			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.wantCause))
			g.Expect(barbican.Status.TargetRelease).To(BeEmpty())
			cond := barbicanCondition(barbican, "DatabaseReady")
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(tc.reason))
			g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
				To(ContainElement(ContainSubstring("Warning " + tc.reason)))
			expectNoSyncJob(t, r)
		})
	}
}

// TestReconcileDatabase_ImageReleaseMismatchBlocks pins the decoupled-field
// contract: the migration Job runs spec.image while release tracking keys on
// spec.openStackRelease, so a tag-pinned image naming another release would
// promote installedRelease to a release the pods never migrated to. Unlike the
// gate rejections it requeues rather than erroring — the fields disagree, which
// an edit fixes, not a retry.
func TestReconcileDatabase_ImageReleaseMismatchBlocks(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	// The release was bumped but the image tag was left behind.
	barbican.Spec.OpenStackRelease = "2026.1"
	barbican.Spec.Image.Tag = "2025.2"
	barbican.Status.InstalledRelease = "2025.2"
	r := newBarbicanTestReconciler(barbican)

	res, err := r.reconcileDatabase(context.Background(), barbican, dbConfigSecretName)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	g.Expect(barbican.Status.TargetRelease).To(BeEmpty(),
		"the release gate is never reached while the fields disagree")
	g.Expect(barbican.Status.InstalledRelease).To(Equal("2025.2"))
	cond := barbicanCondition(barbican, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonImageReleaseMismatch))
	expectNoSyncJob(t, r)
}

// TestReconcileDatabase_DigestPinnedImageSkipsMismatchCheck covers the digest
// escape hatch: a digest-pinned image carries no tag to compare, so
// spec.openStackRelease is taken at its word and the migration proceeds.
func TestReconcileDatabase_DigestPinnedImageSkipsMismatchCheck(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	barbican.Spec.Image.Tag = ""
	barbican.Spec.Image.Digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	r := newBarbicanTestReconciler(barbican)

	res, err := r.reconcileDatabase(context.Background(), barbican, dbConfigSecretName)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	cond := barbicanCondition(barbican, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(database.ReasonDBSyncInProgress),
		"the digest-pinned image is not blocked by the tag/release comparison")

	var syncJob batchv1.Job
	g.Expect(r.Get(context.Background(), syncJobKey, &syncJob)).To(Succeed())
	g.Expect(syncJob.Spec.Template.Spec.Containers[0].Image).To(ContainSubstring("@sha256:"))
}
