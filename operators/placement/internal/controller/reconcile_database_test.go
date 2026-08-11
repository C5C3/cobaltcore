// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/database"
	"github.com/c5c3/forge/internal/common/job"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	placementv1alpha1 "github.com/c5c3/forge/operators/placement/api/v1alpha1"
	placementmetrics "github.com/c5c3/forge/operators/placement/internal/metrics"
)

// dbConfigMapName is the rendered config ConfigMap the database step mounts in
// the migration Job. The database tests never render it, so any stable name does.
const dbConfigMapName = "test-placement-config-abc"

// syncJobKey is the object key of the db-sync Job built for the shared test
// fixture.
var syncJobKey = client.ObjectKey{Namespace: "default", Name: "test-placement-db-sync"}

// managedPlacement returns a Placement in managed database mode (ClusterRef
// set), the mode in which the shared provisioning flow gates on the MariaDB
// cluster's readiness.
func managedPlacement() *placementv1alpha1.Placement {
	placement := testPlacement()
	placement.Spec.Database = commonv1.DatabaseSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"},
		Database:   "placement",
		SecretRef:  commonv1.SecretRefSpec{Name: "placement-db"},
	}
	return placement
}

// terminatedSyncJob returns the db-sync Job for placement marked terminal with
// the given condition type, carrying the desired pod-spec hash (so the runner
// accepts it as the current Job) and a stable UID for the terminal-metric
// dedupe.
func terminatedSyncJob(placement *placementv1alpha1.Placement, condType batchv1.JobConditionType, uid string) *batchv1.Job {
	desired := database.SyncJob(placementJobSetParams(placement, dbConfigMapName))
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
func expectNoSyncJob(t *testing.T, r *PlacementReconciler) {
	t.Helper()
	g := NewGomegaWithT(t)
	var syncJob batchv1.Job
	err := r.Get(context.Background(), syncJobKey, &syncJob)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no migration Job may run on this path")
}

func TestReconcileDatabase_SyncJobCommandAndEnv(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	r := newPlacementTestReconciler(placement)

	_, err := r.reconcileDatabase(context.Background(), r.Client, placement, dbConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())

	var syncJob batchv1.Job
	g.Expect(r.Get(context.Background(), syncJobKey, &syncJob)).To(Succeed())

	container := syncJob.Spec.Template.Spec.Containers[0]
	// Pinned verbatim: the brace group is what scopes the exit-1 tolerance to the
	// upgrade check. Without it, `|| [ $? -eq 1 ]` would also swallow a failing
	// online_data_migrations, and an exit 2 from placement-status (errors, not
	// warnings) would stop failing the Job.
	g.Expect(container.Command).To(Equal([]string{
		"/bin/sh", "-eu", "-c",
		"placement-manage --config-file /etc/placement/placement.conf db sync && " +
			"placement-manage --config-file /etc/placement/placement.conf db online_data_migrations && " +
			"{ placement-status --config-file /etc/placement/placement.conf upgrade check || [ $? -eq 1 ]; }",
	}))

	// The rendered config is mounted at the directory the --config-file paths
	// resolve against.
	g.Expect(container.VolumeMounts[0].MountPath).To(Equal("/etc/placement"))

	var connEnv *corev1.EnvVar
	for i := range container.Env {
		if container.Env[i].Name == "OS_PLACEMENT_DATABASE__CONNECTION" {
			connEnv = &container.Env[i]
		}
	}
	g.Expect(connEnv).NotTo(BeNil(),
		"the db-sync Job overrides [placement_database] connection via env, not via the ConfigMap")
	g.Expect(connEnv.ValueFrom.SecretKeyRef.Name).To(Equal(database.ConnectionSecretName(placement.Name)))
}

// TestReconcileDatabase_SyncJobProjectsDBTLSKeypair pins the db-sync Job against
// the DSN the same CR derives: with database TLS enabled, the
// <name>-db-connection Secret carries ssl_ca/ssl_cert/ssl_key paths under
// dbTLSMountPath, so a Job pod without that mount cannot open them and
// placement-manage fails every migration until the backoff limit is burned.
func TestReconcileDatabase_SyncJobProjectsDBTLSKeypair(t *testing.T) {
	g := NewGomegaWithT(t)

	// Disabled: no volume, no mount.
	plain := database.SyncJob(placementJobSetParams(testPlacement(), dbConfigMapName))
	for _, v := range plain.Spec.Template.Spec.Volumes {
		g.Expect(v.Name).NotTo(Equal(dbTLSVolumeName))
	}

	placement := testPlacement()
	placement.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "placement-db-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "placement-db-client"},
	}
	r := newPlacementTestReconciler(placement)

	_, err := r.reconcileDatabase(context.Background(), r.Client, placement, dbConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())

	var syncJob batchv1.Job
	g.Expect(r.Get(context.Background(), syncJobKey, &syncJob)).To(Succeed())

	var tlsVolume *corev1.Volume
	for i, v := range syncJob.Spec.Template.Spec.Volumes {
		if v.Name == dbTLSVolumeName {
			tlsVolume = &syncJob.Spec.Template.Spec.Volumes[i]
		}
	}
	g.Expect(tlsVolume).NotTo(BeNil(), "db-tls volume must be projected into the db-sync Job")
	g.Expect(tlsVolume.Projected.Sources).To(HaveLen(2))

	var tlsMount *corev1.VolumeMount
	for i, m := range syncJob.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == dbTLSVolumeName {
			tlsMount = &syncJob.Spec.Template.Spec.Containers[0].VolumeMounts[i]
		}
	}
	g.Expect(tlsMount).NotTo(BeNil())
	g.Expect(tlsMount.MountPath).To(Equal(dbTLSMountPath),
		"the mount must land on the directory the derived DSN's ssl_* paths point at")
}

// TestSyncCommandReadsRenderedConfigKey keeps the --config-file path in the sync
// script and the config step's ConfigMap key in lockstep: the Job mounts the
// ConfigMap as a directory, so a renamed key would leave placement-manage
// pointing at a file that does not exist.
func TestSyncCommandReadsRenderedConfigKey(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := placementForConfig()
	r := newPlacementTestReconciler(placement)

	_, configMapName, err := r.reconcileConfig(context.Background(), r.Client, placement)
	g.Expect(err).NotTo(HaveOccurred())

	cm := renderedConfigMap(t, r, configMapName)
	g.Expect(cm.Data).To(HaveKey(strings.TrimPrefix(placementConfFilePath, placementConfigDir)))
}

func TestReconcileDatabase_ProvisionGatesOnClusterReady(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := managedPlacement()
	// The referenced MariaDB cluster does not exist yet, so provisioning gates.
	r := newPlacementTestReconciler(placement)

	res, err := r.reconcileDatabase(context.Background(), r.Client, placement, dbConfigMapName)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	cond := conditions.GetCondition(placement.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonClusterNotReady))
	expectNoSyncJob(t, r)
}

func TestReconcileDatabase_FreshInstallRunsOneJob(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement() // InstalledRelease empty
	r := newPlacementTestReconciler(placement)

	res, err := r.reconcileDatabase(context.Background(), r.Client, placement, dbConfigMapName)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))

	var jobs batchv1.JobList
	g.Expect(r.List(context.Background(), &jobs, client.InNamespace("default"))).To(Succeed())
	g.Expect(jobs.Items).To(HaveLen(1), "the migration is one Job, not a phase chain")
	g.Expect(jobs.Items[0].Name).To(Equal(syncJobKey.Name))

	cond := conditions.GetCondition(placement.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonDBSyncInProgress))
	g.Expect(placement.Status.TargetRelease).To(BeEmpty(),
		"a fresh install converges to its only release; there is no bump to advertise")
}

func TestReconcileDatabase_InstalledReleasePromotedOnSuccess(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement() // InstalledRelease empty, OpenStackRelease 2026.1
	r := newPlacementTestReconciler(placement, terminatedSyncJob(placement, batchv1.JobComplete, "sync-complete-uid"))

	res, err := r.reconcileDatabase(context.Background(), r.Client, placement, dbConfigMapName)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(placement.Status.InstalledRelease).To(Equal("2026.1"),
		"installedRelease is promoted to spec.openStackRelease on db-sync success")
	cond := conditions.GetCondition(placement.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(database.ReasonDatabaseSynced))

	// The terminal metric stamped its dedupe annotation on the CR.
	g.Expect(placement.Annotations).To(HaveKey(dbJobUIDAnnotationKey("db-sync")))
}

// TestReconcileDatabase_JobFailureRecordsMetricOncePerUID covers the failure
// path end to end: the condition, and the db-sync counter that must count a
// terminated Job exactly once no matter how often the CR is reconciled while the
// failed Job sits there.
func TestReconcileDatabase_JobFailureRecordsMetricOncePerUID(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(placementmetrics.Register()).To(Succeed())

	placement := testPlacement()
	placement.Name = "db-sync-failed-metric"
	t.Cleanup(func() { placementmetrics.DeleteForPlacement(placement.Name, placement.Namespace) })

	failed := terminatedSyncJob(placement, batchv1.JobFailed, "sync-failed-uid")
	r := newPlacementTestReconciler(placement, failed)

	_, err := r.reconcileDatabase(context.Background(), r.Client, placement, dbConfigMapName)
	g.Expect(err).To(HaveOccurred(),
		"a permanently failed db-sync surfaces as a reconcile error so the controller backs off")

	cond := conditions.GetCondition(placement.Status.Conditions, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonDBSyncFailed))

	counterLabels := map[string]string{
		"placement": placement.Name,
		"namespace": placement.Namespace,
		"result":    "failed",
	}
	metric := findMetricByLabels(t, ctrlmetrics.Registry, "placement_operator_db_sync_total", counterLabels)
	g.Expect(metric).NotTo(BeNil(), "a failed Job must be counted as result=failed")
	g.Expect(metric.GetCounter().GetValue()).To(Equal(1.0))

	// Same Job, same UID: a second pass must not count it again.
	_, err = r.reconcileDatabase(context.Background(), r.Client, placement, dbConfigMapName)
	g.Expect(err).To(HaveOccurred())
	metric = findMetricByLabels(t, ctrlmetrics.Registry, "placement_operator_db_sync_total", counterLabels)
	g.Expect(metric.GetCounter().GetValue()).To(Equal(1.0),
		"the terminal state MUST be recorded at most once per Job UID")
}

// TestReconcileDatabase_ReleaseBumpTracksTargetRelease walks an accepted
// sequential bump: the target is advertised while the Job runs against the new
// release's image, and it is cleared once the sync flow promotes the installed
// release.
func TestReconcileDatabase_ReleaseBumpTracksTargetRelease(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Spec.OpenStackRelease = "2026.1"
	placement.Spec.Image.Tag = "2026.1"
	placement.Status.InstalledRelease = "2025.2"
	r := newPlacementTestReconciler(placement)

	res, err := r.reconcileDatabase(context.Background(), r.Client, placement, dbConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	g.Expect(placement.Status.TargetRelease).To(Equal("2026.1"))
	g.Expect(placement.Status.InstalledRelease).To(Equal("2025.2"),
		"the marker only moves once the schema is actually migrated")

	var syncJob batchv1.Job
	g.Expect(r.Get(context.Background(), syncJobKey, &syncJob)).To(Succeed())
	g.Expect(syncJob.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/c5c3/placement:2026.1"),
		"the migration runs the release being upgraded to")

	// The Job completes.
	now := metav1.Now()
	syncJob.Status.Succeeded = 1
	syncJob.Status.CompletionTime = &now
	syncJob.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
	}
	g.Expect(r.Status().Update(context.Background(), &syncJob)).To(Succeed())

	res, err = r.reconcileDatabase(context.Background(), r.Client, placement, dbConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(placement.Status.InstalledRelease).To(Equal("2026.1"))
	g.Expect(placement.Status.TargetRelease).To(BeEmpty(),
		"a CR that reached its target advertises no target")
	cond := conditions.GetCondition(placement.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
}

func TestReconcileDatabase_DowngradeRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	// Release and image tag name 2025.2 in lockstep, so the mismatch guard passes
	// and the downgrade is what the gate rejects.
	placement.Spec.OpenStackRelease = "2025.2"
	placement.Spec.Image.Tag = "2025.2"
	placement.Status.InstalledRelease = "2026.1"
	r := newPlacementTestReconciler(placement)

	_, err := r.reconcileDatabase(context.Background(), r.Client, placement, dbConfigMapName)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("downgrade"))
	g.Expect(placement.Status.TargetRelease).To(BeEmpty())
	cond := conditions.GetCondition(placement.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonDowngradeNotSupported))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("Warning DowngradeNotSupported")))
	expectNoSyncJob(t, r)
}

func TestReconcileDatabase_NonSequentialJumpRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Spec.OpenStackRelease = "2026.1"
	placement.Spec.Image.Tag = "2026.1"
	placement.Status.InstalledRelease = "2025.1" // skips 2025.2
	r := newPlacementTestReconciler(placement)

	_, err := r.reconcileDatabase(context.Background(), r.Client, placement, dbConfigMapName)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("sequential"))
	g.Expect(placement.Status.TargetRelease).To(BeEmpty())
	cond := conditions.GetCondition(placement.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonUpgradePathInvalid))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("Warning UpgradePathInvalid")))
	expectNoSyncJob(t, r)
}

// TestReconcileDatabase_UnparseableReleaseRejected covers both sides of the
// comparison. Neither value can reach the API server through the CRD pattern and
// the validating webhook, so this is the defence for a CR (or a status) written
// past them.
func TestReconcileDatabase_UnparseableReleaseRejected(t *testing.T) {
	cases := []struct {
		name      string
		installed string
		requested string
		// wantCause is the ParseRelease message the gate must wrap rather than
		// swallow.
		wantCause string
	}{
		{
			name:      "installed release",
			installed: "not-a-release",
			requested: "2026.1",
			wantCause: `invalid release format "not-a-release"`,
		},
		{
			name:      "requested release",
			installed: "2026.1",
			requested: "latest",
			wantCause: `invalid release format "latest"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			placement := testPlacement()
			placement.Spec.OpenStackRelease = tc.requested
			placement.Status.InstalledRelease = tc.installed
			r := newPlacementTestReconciler(placement)

			_, err := r.reconcileDatabase(context.Background(), r.Client, placement, dbConfigMapName)

			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.wantCause),
				"the parse error must stay wrapped so the message names the offending value")
			cond := conditions.GetCondition(placement.Status.Conditions, "DatabaseReady")
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(database.ReasonVersionParseError))
			expectNoSyncJob(t, r)
		})
	}
}

// TestReconcileDatabase_ImageReleaseMismatchBlocks pins the decoupled-field
// contract: the migration Job runs spec.image while release tracking keys on
// spec.openStackRelease, so a tag-pinned image naming another release would
// promote installedRelease to a release the pods never migrated to.
func TestReconcileDatabase_ImageReleaseMismatchBlocks(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	// The release was bumped but the image tag was left behind.
	placement.Spec.OpenStackRelease = "2026.1"
	placement.Spec.Image.Tag = "2025.2"
	placement.Status.InstalledRelease = "2025.2"
	r := newPlacementTestReconciler(placement)

	res, err := r.reconcileDatabase(context.Background(), r.Client, placement, dbConfigMapName)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	g.Expect(placement.Status.TargetRelease).To(BeEmpty(),
		"the release gate is never reached while the fields disagree")
	g.Expect(placement.Status.InstalledRelease).To(Equal("2025.2"))
	cond := conditions.GetCondition(placement.Status.Conditions, "DatabaseReady")
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
	placement := testPlacement()
	placement.Spec.Image.Tag = ""
	placement.Spec.Image.Digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	r := newPlacementTestReconciler(placement)

	res, err := r.reconcileDatabase(context.Background(), r.Client, placement, dbConfigMapName)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	cond := conditions.GetCondition(placement.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(database.ReasonDBSyncInProgress),
		"the digest-pinned image is not blocked by the tag/release comparison")

	var syncJob batchv1.Job
	g.Expect(r.Get(context.Background(), syncJobKey, &syncJob)).To(Succeed())
	g.Expect(syncJob.Spec.Template.Spec.Containers[0].Image).To(ContainSubstring("@sha256:"))
}

// TestReconcileDatabase_ReleaseBumpWithUnchangedImageRejected closes the
// digest-pinned counterpart of the tag mismatch: a digest carries no release to
// compare, so bumping spec.openStackRelease alone leaves the db-sync Job's pod
// template byte-identical (same image, release-independent placement.conf). The
// shared flow would short-circuit on the completed Job and promote
// installedRelease off a run of the previous release's binary — and the next bump
// would then pass the sequential gate against that false marker, so the migration
// that finally runs would be a multi-release jump.
func TestReconcileDatabase_ReleaseBumpWithUnchangedImageRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Spec.Image.Tag = ""
	placement.Spec.Image.Digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	placement.Spec.OpenStackRelease = "2026.1"
	placement.Status.InstalledRelease = "2025.2"
	// 2025.2 was migrated by exactly the image the CR still pins.
	placement.Status.InstalledImage = placement.Spec.Image.Reference()
	r := newPlacementTestReconciler(placement)

	_, err := r.reconcileDatabase(context.Background(), r.Client, placement, dbConfigMapName)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("leaves spec.image unchanged"))
	g.Expect(placement.Status.TargetRelease).To(BeEmpty())
	g.Expect(placement.Status.InstalledRelease).To(Equal("2025.2"),
		"the marker must not move without a migration")
	cond := conditions.GetCondition(placement.Status.Conditions, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonImageReleaseMismatch))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).
		To(ContainElement(ContainSubstring("Warning ImageReleaseMismatch")))
	expectNoSyncJob(t, r)
}

// TestReconcileDatabase_ReleaseBumpWithNewDigestAccepted is the other half of the
// gate: bumping the digest alongside the release is the supported digest-pinned
// upgrade, so it must still reach the migration Job. It also pins the recording
// side — installedImage is stamped once the sync settles, which is what makes the
// rejection above possible on the next bump.
func TestReconcileDatabase_ReleaseBumpWithNewDigestAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	placement.Spec.Image.Tag = ""
	placement.Spec.Image.Digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	placement.Spec.OpenStackRelease = "2026.1"
	placement.Status.InstalledRelease = "2025.2"
	placement.Status.InstalledImage = placement.Spec.Image.Repository +
		"@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	r := newPlacementTestReconciler(placement)

	res, err := r.reconcileDatabase(context.Background(), r.Client, placement, dbConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	g.Expect(placement.Status.TargetRelease).To(Equal("2026.1"))

	// The Job completes.
	var syncJob batchv1.Job
	g.Expect(r.Get(context.Background(), syncJobKey, &syncJob)).To(Succeed())
	now := metav1.Now()
	syncJob.Status.Succeeded = 1
	syncJob.Status.CompletionTime = &now
	syncJob.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
	}
	g.Expect(r.Status().Update(context.Background(), &syncJob)).To(Succeed())

	res, err = r.reconcileDatabase(context.Background(), r.Client, placement, dbConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(placement.Status.InstalledRelease).To(Equal("2026.1"))
	g.Expect(placement.Status.InstalledImage).To(Equal(placement.Spec.Image.Reference()),
		"the image that ran the migration is recorded alongside the release it installed")
}
