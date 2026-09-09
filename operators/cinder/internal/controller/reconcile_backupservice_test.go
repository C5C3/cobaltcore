// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// The export the fixture backup backend writes its backups to. It is a
// different one from the volume backends' export, which is what the two mount
// bases keep apart inside the pod.
const (
	testBackupServer = "backup-server.openstack.svc.cluster.local"
	testBackupPath   = "/backups"
)

// testBackupProjection returns what reconcileBackupBackend hands the backup step.
func testBackupProjection() *backupProjection {
	return &backupProjection{
		name:         "backups",
		server:       testBackupServer,
		path:         testBackupPath,
		mountOptions: cinderv1alpha1.DefaultNFSMountOptions,
		secretName:   "cinder-backup-backups-def456",
	}
}

// readyBackupDeployment returns the backup Deployment the step builds, with the
// status of a completed rollout.
func readyBackupDeployment(cinder *cinderv1alpha1.Cinder, backends []backendProjection) *appsv1.Deployment {
	return markDeploymentRolledOut(buildBackupDeployment(cinder, testBackupProjection(), backends,
		workloadArtifacts(), workloadDigests{}, testEgressPort))
}

// TestBuildBackupDeployment covers the two sides of a backup: the target export
// it writes to, and every volume export it reads its sources from. cinder-backup
// attaches the source volume itself through os-brick, at the path cinder
// resolved the volume's provider location to.
func TestBuildBackupDeployment(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()
	backend := testBackendProjection("nfs")

	deploy := buildBackupDeployment(cinder, testBackupProjection(), []backendProjection{backend},
		workloadArtifacts(), workloadDigests{}, testEgressPort)
	pod := deploy.Spec.Template.Spec
	container := pod.Containers[0]

	g.Expect(deploy.Name).To(Equal("cinder-backup"))
	g.Expect(deploy.Name).To(Equal(backupDeploymentName(cinder)))
	g.Expect(*deploy.Spec.Replicas).To(Equal(int32(1)))
	g.Expect(deploy.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
	g.Expect(container.Resources.Limits.Memory()).To(Equal(ptrQuantity(resource.MustParse("2Gi"))),
		"a backup compresses each chunk in memory, which the shared limit does not fit")
	g.Expect(container.Command).To(Equal([]string{
		"cinder-backup",
		"--config-dir", "/etc/cinder/cinder.conf.d",
		"--config-dir", "/etc/cinder/backup.conf.d",
	}))
	g.Expect(container.LivenessProbe).To(BeNil())
	g.Expect(container.ReadinessProbe.Exec.Command).To(Equal(
		[]string{"/var/lib/openstack/bin/cinder-amqp-ready"}))
	g.Expect(*pod.SecurityContext.FSGroupChangePolicy).To(Equal(corev1.FSGroupChangeOnRootMismatch))
	g.Expect(deploy.Spec.Template.Annotations).To(HaveKeyWithValue(installedReleaseAnnotation, "2026.1"))

	volumes := map[string]corev1.Volume{}
	for _, volume := range pod.Volumes {
		volumes[volume.Name] = volume
	}
	g.Expect(volumes[backupVolumeName].Secret.SecretName).To(Equal("cinder-backup-backups-def456"))
	g.Expect(volumes[backupVolumeName].Secret.Items).To(Equal([]corev1.KeyToPath{
		{Key: backupConfDataKey, Path: backupConfDataKey},
	}))
	g.Expect(volumes[backupShareVolumeName].CSI.VolumeAttributes).To(HaveKeyWithValue("share", testBackupPath))
	g.Expect(volumes["share-nfs"].CSI.VolumeAttributes).To(HaveKeyWithValue("share", testSharePath))

	mounts := map[string]corev1.VolumeMount{}
	for _, mount := range container.VolumeMounts {
		mounts[mount.Name] = mount
	}
	g.Expect(mounts[backupVolumeName].MountPath).To(Equal("/etc/cinder/backup.conf.d"))
	g.Expect(mounts[backupShareVolumeName].MountPath).To(HavePrefix("/var/lib/cinder/backup_mount/"))
	g.Expect(mounts["share-nfs"].MountPath).To(
		Equal("/var/lib/cinder/mnt/6f3cb55ed3b423dbb7791aaf3783754f"),
		"the source volume has to be where the volume service holds it")
}

// ptrQuantity returns a pointer to q, matching what ResourceList.Memory() hands
// back.
func ptrQuantity(q resource.Quantity) *resource.Quantity { return &q }

// TestBuildBackupDeployment_SharedExportMountedOnce covers two backends serving
// the same export. The mount path is derived from "server:path", so both resolve
// to the same directory: a second volumeMount under it would make the Deployment
// un-appliable, and the BackupService step sits before every other workload step
// in the pipeline, so the rejection would stop the whole Cinder from converging.
func TestBuildBackupDeployment_SharedExportMountedOnce(t *testing.T) {
	g := NewGomegaWithT(t)

	deploy := buildBackupDeployment(workloadCinder(), testBackupProjection(),
		[]backendProjection{testBackendProjection("nfs"), testBackendProjection("nfs-second")},
		workloadArtifacts(), workloadDigests{}, testEgressPort)
	container := deploy.Spec.Template.Spec.Containers[0]

	seen := map[string]string{}
	for _, mount := range container.VolumeMounts {
		g.Expect(seen).NotTo(HaveKey(mount.MountPath),
			"two volumeMounts share a mountPath, which the API server rejects")
		seen[mount.MountPath] = mount.Name
	}
	g.Expect(seen).To(HaveKeyWithValue(
		"/var/lib/cinder/mnt/6f3cb55ed3b423dbb7791aaf3783754f", "share-nfs"),
		"the shared export stays mounted once, under the first backend that names it")
}

// TestReconcileBackupService_SharedExportMountOptions covers the half of that
// dedup the rendered pod spec cannot show. Two backends on one export are
// mounted once, so the later one's mountOptions are not applied in the backup
// pod: an option the export needs would be missing there and the kubelet's CSI
// mount would fail for every backend, while the Deployment names no backend the
// discarded string belongs to. The divergence is therefore reported as a Warning
// event; matching options lose nothing and stay silent.
func TestReconcileBackupService_SharedExportMountOptions(t *testing.T) {
	ctx := context.Background()

	t.Run("divergent options are reported", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := workloadCinder()
		r := newCinderTestReconciler(cinder)
		second := testBackendProjection("nfs-second")
		second.mountOptions = "nfsvers=3,soft"

		_, err := r.reconcileBackupService(ctx, r.Client, cinder, testBackupProjection(),
			[]backendProjection{testBackendProjection("nfs"), second},
			workloadArtifacts(), workloadDigests{}, testEgressPort)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(ContainElement(SatisfyAll(
			ContainSubstring("SharedExportMountOptionsIgnored"),
			ContainSubstring("nfs-second"),
			ContainSubstring("nfsvers=3,soft"),
		)), "the discarded option string is named nowhere else")
	})

	t.Run("an unbounded option string is clipped", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := workloadCinder()
		r := newCinderTestReconciler(cinder)
		second := testBackendProjection("nfs-second")
		second.mountOptions = strings.Repeat("a", 4096)

		_, err := r.reconcileBackupService(ctx, r.Client, cinder, testBackupProjection(),
			[]backendProjection{testBackendProjection("nfs"), second},
			workloadArtifacts(), workloadDigests{}, testEgressPort)

		g.Expect(err).NotTo(HaveOccurred())
		events := collectEvents(r.Recorder.(*record.FakeRecorder))
		g.Expect(events).To(HaveLen(1))
		g.Expect(events[0]).To(ContainSubstring(strings.Repeat("a", sharedExportValueLimit) + "..."))
		g.Expect(len(events[0])).To(BeNumerically("<", 1024),
			"spec.nfs.mountOptions has no length marker, so an unclipped value would grow the "+
				"message to etcd-object size and the API server would reject the Event outright")
	})

	t.Run("matching options stay silent", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := workloadCinder()
		r := newCinderTestReconciler(cinder)

		_, err := r.reconcileBackupService(ctx, r.Client, cinder, testBackupProjection(),
			[]backendProjection{testBackendProjection("nfs"), testBackendProjection("nfs-second")},
			workloadArtifacts(), workloadDigests{}, testEgressPort)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).NotTo(ContainElement(
			ContainSubstring("SharedExportMountOptionsIgnored")),
			"a shared export mounted with the same options discards nothing")
	})
}

// TestBuildBackupDeployment_WithoutVolumeBackends covers the degenerate fleet: a
// Cinder whose backup backend is attached before any volume backend is renders
// the target mount alone rather than an empty source mount.
func TestBuildBackupDeployment_WithoutVolumeBackends(t *testing.T) {
	g := NewGomegaWithT(t)

	deploy := buildBackupDeployment(workloadCinder(), testBackupProjection(), nil,
		workloadArtifacts(), workloadDigests{}, testEgressPort)

	for _, volume := range deploy.Spec.Template.Spec.Volumes {
		g.Expect(volume.Name).NotTo(HavePrefix(shareVolumePrefix + "n"))
	}
	g.Expect(deploy.Spec.Template.Spec.Volumes[len(deploy.Spec.Template.Spec.Volumes)-1].Name).
		To(Equal(backupShareVolumeName))
}

// TestReconcileBackupService_NotConfigured covers the opt-out: backups are a
// service a deployment adds, so a Cinder without one is complete rather than
// degraded, and the Deployment of a backup backend that was detached goes with
// it.
func TestReconcileBackupService_NotConfigured(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cinder := workloadCinder()
	r := newCinderTestReconciler(cinder, readyBackupDeployment(cinder, nil))

	res, err := r.reconcileBackupService(ctx, r.Client, cinder, nil, nil,
		workloadArtifacts(), workloadDigests{}, testEgressPort)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(apierrors.IsNotFound(r.Get(ctx, objectKey("cinder-backup"), &appsv1.Deployment{}))).To(BeTrue())
	cond := cinderCondition(cinder, "BackupServiceReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonBackupNotConfigured))
}

// TestReconcileBackupService_ReturnsZeroOutsideAnUpgrade covers the result
// contract: the steps after this one must still run while the backup service
// comes up, so its readiness is reported without holding the pass back.
func TestReconcileBackupService_ReturnsZeroOutsideAnUpgrade(t *testing.T) {
	ctx := context.Background()
	backends := []backendProjection{testBackendProjection("nfs")}

	t.Run("a backup service still rolling out", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := workloadCinder()
		r := newCinderTestReconciler(cinder)

		res, err := r.reconcileBackupService(ctx, r.Client, cinder, testBackupProjection(), backends,
			workloadArtifacts(), workloadTestDigests(), testEgressPort)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		g.Expect(r.Get(ctx, objectKey("cinder-backup"), &appsv1.Deployment{})).To(Succeed())
		cond := cinderCondition(cinder, "BackupServiceReady")
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForBackupService))
	})

	t.Run("an available backup service", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := workloadCinder()
		r := newCinderTestReconciler(cinder, readyBackupDeployment(cinder, backends))

		res, err := r.reconcileBackupService(ctx, r.Client, cinder, testBackupProjection(), backends,
			workloadArtifacts(), workloadDigests{}, testEgressPort)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		cond := cinderCondition(cinder, "BackupServiceReady")
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cond.Reason).To(Equal(conditionReasonBackupServiceReady))
	})
}

// TestReconcileBackupService_RollingUpdateHoldsUntilConverged covers the upgrade
// gate, which the backup service shares with the scheduler and the volume
// services.
func TestReconcileBackupService_RollingUpdateHoldsUntilConverged(t *testing.T) {
	ctx := context.Background()

	t.Run("a replica still on the old template holds the pass", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := rollingUpdateCinder()
		lagging := readyBackupDeployment(cinder, nil)
		lagging.Status.UpdatedReplicas = 0
		r := newCinderTestReconciler(cinder, lagging)

		res, err := r.reconcileBackupService(ctx, r.Client, cinder, testBackupProjection(), nil,
			workloadArtifacts(), workloadDigests{}, testEgressPort)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))
		g.Expect(cinderCondition(cinder, "BackupServiceReady").Reason).
			To(Equal(conditionReasonWaitingForBackupService))
	})

	t.Run("a converged rollout releases the pass", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := rollingUpdateCinder()
		r := newCinderTestReconciler(cinder, readyBackupDeployment(cinder, nil))

		res, err := r.reconcileBackupService(ctx, r.Client, cinder, testBackupProjection(), nil,
			workloadArtifacts(), workloadDigests{}, testEgressPort)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		g.Expect(cinderCondition(cinder, "BackupServiceReady").Status).To(Equal(metav1.ConditionTrue))
	})
}

// TestReconcileBackupService_ApplyFailureWrapsTheError covers the error path: the
// message names the backup service so a pipeline error is not confused with
// another workload's.
func TestReconcileBackupService_ApplyFailureWrapsTheError(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()
	boom := errors.New("quota exceeded")
	r := failingApplyReconciler(boom, "Deployment", backupDeploymentName(cinder), cinder)

	_, err := r.reconcileBackupService(context.Background(), r.Client, cinder, testBackupProjection(), nil,
		workloadArtifacts(), workloadDigests{}, testEgressPort)

	g.Expect(err).To(MatchError(boom))
	g.Expect(err).To(MatchError(ContainSubstring("ensuring backup Deployment:")))
}
