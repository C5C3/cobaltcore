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
	"github.com/c5c3/cobaltcore/internal/common/testutil"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// The export the fixture backup backend writes its backups to. It is a
// different one from the volume backends' export, which is what the two mount
// bases keep apart inside the pod.
const (
	testBackupServer = "backup-server.openstack.svc.cluster.local"
	testBackupPath   = "/backups"
)

// testBackupProjection returns what reconcileBackupBackend hands the backup step
// for an NFS target.
func testBackupProjection() *backupProjection {
	return &backupProjection{
		name:         "backups",
		backupType:   cinderv1alpha1.CinderBackupBackendTypeNFS,
		server:       testBackupServer,
		path:         testBackupPath,
		mountOptions: cinderv1alpha1.DefaultNFSMountOptions,
		secretName:   "cinder-backup-backups-def456",
	}
}

// testRBDBackupProjection returns what reconcileBackupBackend hands the backup
// step for an RBD target of the cluster ceph and the user cinder-backup.
func testRBDBackupProjection() *backupProjection {
	return &backupProjection{
		name:       "rbdbk",
		backupType: cinderv1alpha1.CinderBackupBackendTypeRBD,
		rbd: &rbdProjection{
			clusterName: "ceph",
			user:        "cinder-backup",
			networks:    []string{"10.244.0.0/16", "10.96.0.0/12"},
			keyDigest:   "5f7c6b3f0f4d2e1a9c8b7a6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f",
		},
		secretName: "cinder-backup-rbdbk-def456",
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
	g.Expect(container.Env).To(ContainElement(corev1.EnvVar{Name: "MALLOC_ARENA_MAX", Value: "2"}),
		"the chunk buffers the thread pool frees stay in per-thread arenas; uncapped, "+
			"the process grows past the 2Gi limit over a run of backups and is killed mid-backup")
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
	g.Expect(volumes).NotTo(HaveKey(cephVolumeName), "nothing is RBD, so /etc/ceph stays the image's own")

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

// TestBuildBackupDeployment_ProjectsRBDVolumeKeyrings covers a Cinder serving an
// NFS and an RBD backend behind an NFS target. Only the NFS backend has an
// export to mount: an RBD projection carries no server or path, and mounting
// one would derive a mount path from the empty export ":". The RBD backend's
// keyring is projected into /etc/ceph instead, because a backup of its volume
// reads it through os-brick with that backend's user.
func TestBuildBackupDeployment_ProjectsRBDVolumeKeyrings(t *testing.T) {
	g := NewGomegaWithT(t)

	deploy := buildBackupDeployment(workloadCinder(), testBackupProjection(),
		[]backendProjection{testBackendProjection("nfs"), testRBDBackendProjection("rbd")},
		workloadArtifacts(), workloadDigests{}, testEgressPort)
	pod := deploy.Spec.Template.Spec

	var shareVolumes []string
	for _, volume := range pod.Volumes {
		if strings.HasPrefix(volume.Name, shareVolumePrefix) {
			shareVolumes = append(shareVolumes, volume.Name)
		}
	}
	g.Expect(shareVolumes).To(ConsistOf("share-nfs"))
	emptyExport := shareMountPath(nfsMountPointBase, "", "")
	for _, mount := range pod.Containers[0].VolumeMounts {
		g.Expect(mount.MountPath).NotTo(Equal(emptyExport), "no mount is derived from an empty export")
	}

	ceph := podVolume(pod, cephVolumeName)
	g.Expect(ceph).NotTo(BeNil())
	g.Expect(ceph.Projected).NotTo(BeNil())
	g.Expect(ceph.Projected.Sources).To(Equal([]corev1.VolumeProjection{{
		Secret: &corev1.SecretProjection{
			LocalObjectReference: corev1.LocalObjectReference{Name: "cinder-backend-rbd-abc123"},
			Items:                []corev1.KeyToPath{{Key: keyringDataKey, Path: "ceph.client.cinder.keyring"}},
		},
	}}), "the volume backend's ceph.conf is not projected: os-brick builds its own")
	g.Expect(podMount(pod, cephVolumeName)).To(Equal(&corev1.VolumeMount{
		Name: cephVolumeName, MountPath: "/etc/ceph", ReadOnly: true,
	}))

	t.Run("two RBD backends are neither mounted nor in conflict", func(t *testing.T) {
		g := NewGomegaWithT(t)
		rbdBackends := []backendProjection{testRBDBackendProjection("rbd-a"), testRBDBackendProjection("rbd-b")}

		sources, conflicts := backupShareMounts(rbdBackends)
		g.Expect(sources).To(BeEmpty())
		g.Expect(conflicts).To(BeEmpty())

		recorder := record.NewFakeRecorder(10)
		r := &CinderReconciler{Recorder: recorder}
		r.warnSharedExportConflicts(context.Background(), workloadCinder(), rbdBackends)
		g.Expect(collectEvents(recorder)).To(BeEmpty(),
			"two RBD backends share no export, whatever their empty fields compare as")
	})
}

// podVolume returns the pod's volume of the given name, or nil.
func podVolume(pod corev1.PodSpec, name string) *corev1.Volume {
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == name {
			return &pod.Volumes[i]
		}
	}
	return nil
}

// podMount returns the first container's mount of the given volume, or nil.
func podMount(pod corev1.PodSpec, name string) *corev1.VolumeMount {
	for i := range pod.Containers[0].VolumeMounts {
		if pod.Containers[0].VolumeMounts[i].Name == name {
			return &pod.Containers[0].VolumeMounts[i]
		}
	}
	return nil
}

// TestBuildBackupDeployment_RBDTarget covers an RBD target: it mounts no export,
// and the Ceph backup driver finds its configuration and keyring in /etc/ceph,
// projected from the target's own Secret. The backup volume keeps its shape,
// because the CinderBackupBackend controller observes the projection by that
// volume's Secret name.
func TestBuildBackupDeployment_RBDTarget(t *testing.T) {
	g := NewGomegaWithT(t)

	deploy := buildBackupDeployment(workloadCinder(), testRBDBackupProjection(), nil,
		workloadArtifacts(), workloadDigests{}, testEgressPort)
	pod := deploy.Spec.Template.Spec

	g.Expect(podVolume(pod, backupShareVolumeName)).To(BeNil(), "an RBD target has no export")
	for _, mount := range pod.Containers[0].VolumeMounts {
		g.Expect(mount.MountPath).NotTo(HavePrefix(backupMountPointBase))
	}

	backup := podVolume(pod, backupVolumeName)
	g.Expect(backup.Secret).NotTo(BeNil(), "the backup volume stays a plain Secret volume")
	g.Expect(backup.Secret.SecretName).To(Equal("cinder-backup-rbdbk-def456"))
	g.Expect(backup.Secret.Items).To(Equal([]corev1.KeyToPath{{Key: backupConfDataKey, Path: backupConfDataKey}}))

	ceph := podVolume(pod, cephVolumeName)
	g.Expect(ceph).NotTo(BeNil())
	g.Expect(ceph.Projected.Sources).To(Equal([]corev1.VolumeProjection{{
		Secret: &corev1.SecretProjection{
			LocalObjectReference: corev1.LocalObjectReference{Name: "cinder-backup-rbdbk-def456"},
			Items: []corev1.KeyToPath{
				{Key: cephConfDataKey, Path: "ceph.conf"},
				{Key: keyringDataKey, Path: "ceph.client.cinder-backup.keyring"},
			},
		},
	}}))
	g.Expect(podMount(pod, cephVolumeName)).To(Equal(&corev1.VolumeMount{
		Name: cephVolumeName, MountPath: "/etc/ceph", ReadOnly: true,
	}))
}

// TestBackupCephProjection_DedupesAndReportsConflicts covers the paths two
// sources would project one keyring file under. The API server refuses a
// projected volume that maps two items to one path, so the first writer keeps
// the path; a later source with the same key is dropped silently, one with a
// different key is dropped and reported.
func TestBackupCephProjection_DedupesAndReportsConflicts(t *testing.T) {
	keyringOf := func(sources []corev1.VolumeProjection) map[string]string {
		owners := map[string]string{}
		for _, source := range sources {
			for _, item := range source.Secret.Items {
				owners[item.Path] = source.Secret.Name
			}
		}
		return owners
	}

	t.Run("two backends sharing one user and one key project one keyring", func(t *testing.T) {
		g := NewGomegaWithT(t)
		first := testRBDBackendProjection("rbd-a")
		second := testRBDBackendProjection("rbd-b")

		sources, conflicts := backupCephProjection(testRBDBackupProjection(), []backendProjection{first, second})

		g.Expect(conflicts).To(BeEmpty())
		g.Expect(sources).To(HaveLen(2), "the target's source and the first backend's; the second is left empty")
		g.Expect(keyringOf(sources)).To(Equal(map[string]string{
			"ceph.conf":                         "cinder-backup-rbdbk-def456",
			"ceph.client.cinder-backup.keyring": "cinder-backup-rbdbk-def456",
			"ceph.client.cinder.keyring":        "cinder-backend-rbd-a-abc123",
		}))
	})

	t.Run("two backends with different keys keep the first and report the second", func(t *testing.T) {
		g := NewGomegaWithT(t)
		first := testRBDBackendProjection("rbd-a")
		second := testRBDBackendProjection("rbd-b")
		second.rbd.keyDigest = strings.Repeat("f", 64)

		sources, conflicts := backupCephProjection(testRBDBackupProjection(), []backendProjection{first, second})

		g.Expect(keyringOf(sources)).To(HaveKeyWithValue("ceph.client.cinder.keyring", "cinder-backend-rbd-a-abc123"))
		g.Expect(conflicts).To(Equal([]cephKeyringConflict{{
			path:   "/etc/ceph/ceph.client.cinder.keyring",
			first:  "backend rbd-a",
			second: "backend rbd-b",
		}}))
	})

	t.Run("the target keeps its keyring over a backend on the same path", func(t *testing.T) {
		g := NewGomegaWithT(t)
		backend := testRBDBackendProjection("rbd-a")
		backend.rbd.user = "cinder-backup"

		sources, conflicts := backupCephProjection(testRBDBackupProjection(), []backendProjection{backend})

		g.Expect(sources).To(HaveLen(1), "the backend's source is left without items")
		g.Expect(keyringOf(sources)).To(HaveKeyWithValue("ceph.client.cinder-backup.keyring",
			"cinder-backup-rbdbk-def456"))
		g.Expect(conflicts).To(Equal([]cephKeyringConflict{{
			path:   "/etc/ceph/ceph.client.cinder-backup.keyring",
			first:  "backup backend rbdbk",
			second: "backend rbd-a",
		}}))
	})

	t.Run("nothing RBD projects nothing", func(t *testing.T) {
		g := NewGomegaWithT(t)

		sources, conflicts := backupCephProjection(testBackupProjection(),
			[]backendProjection{testBackendProjection("nfs")})
		g.Expect(sources).To(BeNil())
		g.Expect(conflicts).To(BeNil())

		sources, conflicts = backupCephProjection(nil, nil)
		g.Expect(sources).To(BeNil())
		g.Expect(conflicts).To(BeNil())
	})
}

// TestReconcileBackupService_CephKeyringConflictWarns covers the half of that
// dedup the rendered pod spec cannot show: the keyring a backend does not get
// into the backup pod is named in a Warning event, because a backup of its
// volume then fails with os-brick's "Keyring path ... is not readable".
func TestReconcileBackupService_CephKeyringConflictWarns(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cinder := workloadCinder()
	r := newCinderTestReconciler(cinder)
	second := testRBDBackendProjection("rbd-b")
	second.rbd.keyDigest = strings.Repeat("f", 64)

	_, err := r.reconcileBackupService(ctx, r.Client, cinder, testRBDBackupProjection(),
		[]backendProjection{testRBDBackendProjection("rbd-a"), second, testRBDBackendProjection("rbd-c")},
		workloadArtifacts(), workloadDigests{}, testEgressPort)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(ConsistOf(
		"Warning CephKeyringConflict backend rbd-b projects the keyring /etc/ceph/ceph.client.cinder.keyring "+
			"that backend rbd-a already projects into the backup service with a different key, "+
			"so its keyring is not applied there"),
		"rbd-c carries rbd-a's key and loses nothing")
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

// TestBuildBackupDeployment_RendersResourceDefaults verifies that cinder-backup
// renders its fixed 2Gi as memory request and limit, beside a 70m CPU request
// and no CPU limit, when spec.backup.deployment.resources names nothing: its
// footprint follows the backup chunk size, not a process count.
func TestBuildBackupDeployment_RendersResourceDefaults(t *testing.T) {
	g := NewGomegaWithT(t)

	deploy := buildBackupDeployment(workloadCinder(), testBackupProjection(),
		[]backendProjection{testBackendProjection("nfs")}, workloadArtifacts(), workloadDigests{}, testEgressPort)

	g.Expect(deploy.Spec.Template.Spec.Containers[0].Resources).To(Equal(testutil.RenderedResourceDefaults("2Gi")))
}
