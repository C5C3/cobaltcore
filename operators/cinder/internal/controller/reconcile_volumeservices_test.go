// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	//nolint:gosec // G501: the test recomputes the digest the production path derives an NFS mount directory from.
	"crypto/md5"
	"encoding/hex"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/job"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// testShareServer and testSharePath are the export the fixture backend serves.
// Their digest is what TestShareMountPath pins, so the mount path in the
// goldens and the one os-brick computes stay comparable by hand.
const (
	testShareServer = "nfs-server.openstack.svc.cluster.local"
	testSharePath   = "/volumes"
)

// testBackendProjection returns what reconcileBackends hands the volume step for
// one backend, carrying the mount options admission materializes.
func testBackendProjection(name string) backendProjection {
	return backendProjection{
		name:         name,
		server:       testShareServer,
		path:         testSharePath,
		mountOptions: cinderv1alpha1.DefaultNFSMountOptions,
		secretName:   "cinder-backend-" + name + "-abc123",
	}
}

// readyVolumeDeployment returns the volume Deployment the step builds for one
// backend, with the status of a completed rollout.
func readyVolumeDeployment(cinder *cinderv1alpha1.Cinder, backend backendProjection) *appsv1.Deployment {
	return markDeploymentRolledOut(
		buildVolumeDeployment(cinder, backend, workloadArtifacts(), workloadDigests{}, testEgressPort))
}

// detachingBackend returns a CinderBackend that is being deleted and still holds
// the service-remove finalizer: the state the detach flow acts on.
func detachingBackend(name string) *cinderv1alpha1.CinderBackend {
	backend := credentialReadyBackend(name)
	deleted := metav1.Now()
	backend.DeletionTimestamp = &deleted
	backend.Finalizers = []string{CinderBackendServiceRemoveFinalizer}
	return backend
}

// terminalServiceRemoveJob returns the service-remove Job of one backend in the
// given terminal state, carrying the pod-spec hash the runner compares against
// and a stable UID for the terminal-metric dedupe.
func terminalServiceRemoveJob(cinder *cinderv1alpha1.Cinder, name string,
	conditionType batchv1.JobConditionType,
) *batchv1.Job {
	desired := buildServiceRemoveJob(cinder, name, workloadArtifacts())
	observed := desired.DeepCopy()
	observed.UID = types.UID(name + "-service-remove-uid")
	observed.Annotations = map[string]string{job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template)}
	observed.Status.Conditions = []batchv1.JobCondition{{Type: conditionType, Status: corev1.ConditionTrue}}
	return observed
}

// TestShareMountPath pins the mount directory of an export. cinder resolves the
// provider location of every volume to this path, so a change to it strands the
// volumes an existing deployment holds. The expectation is computed here as
// well: the pinned literal is what a reviewer compares against os-brick, and the
// computation is what proves the helper hashes the string os-brick hashes.
func TestShareMountPath(t *testing.T) {
	g := NewGomegaWithT(t)

	//nolint:gosec // G401: see the crypto/md5 import.
	sum := md5.Sum([]byte(testShareServer + ":" + testSharePath))
	digest := hex.EncodeToString(sum[:])

	g.Expect(digest).To(Equal("6f3cb55ed3b423dbb7791aaf3783754f"))
	g.Expect(shareMountPath(nfsMountPointBase, testShareServer, testSharePath)).To(
		Equal("/var/lib/cinder/mnt/6f3cb55ed3b423dbb7791aaf3783754f"))
	g.Expect(shareMountPath(backupMountPointBase, testShareServer, testSharePath)).To(
		Equal("/var/lib/cinder/backup_mount/6f3cb55ed3b423dbb7791aaf3783754f"),
		"the backup service mounts its own target beside the volumes it reads")
}

// TestInlineNFSVolume covers the option string: an empty one omits the attribute
// rather than passing the driver an empty -o flag, which it would reject.
func TestInlineNFSVolume(t *testing.T) {
	g := NewGomegaWithT(t)

	with := inlineNFSVolume("share-nfs", testShareServer, testSharePath, "nfsvers=4.1")
	g.Expect(with.CSI.Driver).To(Equal("nfs.csi.k8s.io"))
	g.Expect(with.CSI.VolumeAttributes).To(Equal(map[string]string{
		"server":       testShareServer,
		"share":        testSharePath,
		"mountOptions": "nfsvers=4.1",
	}))

	without := inlineNFSVolume("share-nfs", testShareServer, testSharePath, "")
	g.Expect(without.CSI.VolumeAttributes).To(Equal(map[string]string{
		"server": testShareServer,
		"share":  testSharePath,
	}))
}

// TestBuildVolumeDeployment covers the pod one backend runs in: the two
// projected files under their own config directories, the export mounted where
// cinder resolves its volumes to, and the single-writer shape the drivers
// require.
func TestBuildVolumeDeployment(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()
	backend := testBackendProjection("nfs")

	deploy := buildVolumeDeployment(cinder, backend, workloadArtifacts(), workloadDigests{}, testEgressPort)
	pod := deploy.Spec.Template.Spec
	container := pod.Containers[0]

	g.Expect(deploy.Name).To(Equal("cinder-volume-nfs"))
	g.Expect(deploy.Labels).To(HaveKeyWithValue("app.kubernetes.io/component", "volume-nfs"))
	g.Expect(deploy.Spec.Selector.MatchLabels).To(Equal(map[string]string{
		"app.kubernetes.io/name":      "cinder",
		"app.kubernetes.io/instance":  "cinder",
		"app.kubernetes.io/component": "volume-nfs",
	}))
	g.Expect(*deploy.Spec.Replicas).To(Equal(int32(1)),
		"two processes under one host identity hold the same volume state open twice")
	g.Expect(deploy.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))

	g.Expect(pod.SecurityContext.FSGroup).To(Equal(ptr.To(deployment.OpenStackUID)))
	g.Expect(pod.SecurityContext.FSGroupChangePolicy).NotTo(BeNil())
	g.Expect(*pod.SecurityContext.FSGroupChangePolicy).To(Equal(corev1.FSGroupChangeOnRootMismatch),
		"the default policy would chown a whole export on every pod start")

	g.Expect(container.Command).To(Equal([]string{
		"cinder-volume",
		"--config-dir", "/etc/cinder/cinder.conf.d",
		"--config-dir", "/etc/cinder/backends.conf.d",
		"--config-dir", "/etc/cinder/volume.conf.d",
	}))
	g.Expect(container.LivenessProbe).To(BeNil())
	g.Expect(container.ReadinessProbe.Exec.Command).To(Equal(
		[]string{"/var/lib/openstack/bin/cinder-amqp-ready"}))
	g.Expect(container.Env).To(ContainElement(corev1.EnvVar{Name: "CINDER_AMQP_PORT", Value: "5672"}))

	volumes := map[string]corev1.Volume{}
	for _, volume := range pod.Volumes {
		volumes[volume.Name] = volume
	}
	g.Expect(volumes[backendsVolumeName].Secret.SecretName).To(Equal("cinder-backend-nfs-abc123"))
	g.Expect(volumes[backendsVolumeName].Secret.Items).To(Equal([]corev1.KeyToPath{
		{Key: backendConfDataKey, Path: backendConfDataKey},
		{Key: sharesDataKey, Path: "nfs.shares"},
	}), "nfs_shares_config names the file by the backend it belongs to")
	g.Expect(volumes[volumeOverlayVolumeName].Secret.Items).To(Equal([]corev1.KeyToPath{
		{Key: volumeOverlayDataKey, Path: volumeOverlayDataKey},
	}))
	g.Expect(volumes["share-nfs"].CSI.VolumeAttributes).To(HaveKeyWithValue("share", testSharePath))

	mounts := map[string]corev1.VolumeMount{}
	for _, mount := range container.VolumeMounts {
		mounts[mount.Name] = mount
	}
	g.Expect(mounts[backendsVolumeName].MountPath).To(Equal("/etc/cinder/backends.conf.d"))
	g.Expect(mounts[volumeOverlayVolumeName].MountPath).To(Equal("/etc/cinder/volume.conf.d"))
	g.Expect(mounts["share-nfs"].MountPath).To(
		Equal("/var/lib/cinder/mnt/6f3cb55ed3b423dbb7791aaf3783754f"))
	g.Expect(mounts["share-nfs"].ReadOnly).To(BeFalse(), "the volume service writes the volumes")
}

// TestReconcileVolumeServices_ProjectsEveryBackend covers the steady state: one
// Deployment per projected backend, and a status entry naming the host identity
// the volumes of that backend are keyed by.
func TestReconcileVolumeServices_ProjectsEveryBackend(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cinder := workloadCinder()
	// Deliberately out of name order: the status is what other controllers read,
	// so it must not churn with the order the projections arrive in.
	backends := []backendProjection{testBackendProjection("second"), testBackendProjection("first")}
	r := newCinderTestReconciler(cinder,
		readyVolumeDeployment(cinder, backends[0]), readyVolumeDeployment(cinder, backends[1]))

	res, err := r.reconcileVolumeServices(ctx, r.Client, cinder, backends,
		workloadArtifacts(), workloadDigests{}, testEgressPort)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(r.Get(ctx, objectKey("cinder-volume-first"), &appsv1.Deployment{})).To(Succeed())
	g.Expect(r.Get(ctx, objectKey("cinder-volume-second"), &appsv1.Deployment{})).To(Succeed())
	g.Expect(cinder.Status.VolumeServices).To(Equal([]cinderv1alpha1.VolumeServiceStatus{
		{Backend: "first", Host: "cinder@first"},
		{Backend: "second", Host: "cinder@second"},
	}))
	cond := cinderCondition(cinder, "VolumeServicesReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonAllVolumeServicesReady))
}

// TestReconcileVolumeServices_WaitingNamesTheBackends covers the partial state: a
// backend whose pod has not come up keeps the condition False and is named, while
// its siblings keep running.
func TestReconcileVolumeServices_WaitingNamesTheBackends(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()
	ready := testBackendProjection("first")
	pending := testBackendProjection("second")
	r := newCinderTestReconciler(cinder, readyVolumeDeployment(cinder, ready))

	res, err := r.reconcileVolumeServices(context.Background(), r.Client, cinder,
		[]backendProjection{ready, pending}, workloadArtifacts(), workloadDigests{}, testEgressPort)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := cinderCondition(cinder, "VolumeServicesReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForVolumeServices))
	g.Expect(cond.Message).To(ContainSubstring("second"))
	g.Expect(cond.Message).NotTo(ContainSubstring("first"))
}

// TestReconcileVolumeServices_SweepsUnprojectedDeployments covers the removal
// path: a Deployment whose backend is no longer projected keeps re-registering
// the host identity a detach removes, so it goes even when nothing is projected
// at all.
func TestReconcileVolumeServices_SweepsUnprojectedDeployments(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cinder := workloadCinder()
	gone := readyVolumeDeployment(cinder, testBackendProjection("gone"))
	kept := readyVolumeDeployment(cinder, testBackendProjection("kept"))
	// A Deployment of the same CR that is not a volume service must survive.
	scheduler := readySchedulerDeployment(cinder)
	r := newCinderTestReconciler(cinder, gone, kept, scheduler)

	res, err := r.reconcileVolumeServices(ctx, r.Client, cinder,
		[]backendProjection{testBackendProjection("kept")},
		workloadArtifacts(), workloadDigests{}, testEgressPort)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(apierrors.IsNotFound(r.Get(ctx, objectKey("cinder-volume-gone"), &appsv1.Deployment{}))).
		To(BeTrue())
	g.Expect(r.Get(ctx, objectKey("cinder-volume-kept"), &appsv1.Deployment{})).To(Succeed())
	g.Expect(r.Get(ctx, objectKey("cinder-scheduler"), &appsv1.Deployment{})).To(Succeed())
}

// TestReconcileVolumeServices_NoBackends covers the empty set: a Cinder without
// storage serves its API and its scheduler, the last volume services are swept,
// and the status stops reporting hosts nothing runs under.
func TestReconcileVolumeServices_NoBackends(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cinder := workloadCinder()
	cinder.Status.VolumeServices = []cinderv1alpha1.VolumeServiceStatus{{Backend: "gone", Host: "cinder@gone"}}
	r := newCinderTestReconciler(cinder, readyVolumeDeployment(cinder, testBackendProjection("gone")))

	res, err := r.reconcileVolumeServices(ctx, r.Client, cinder, nil,
		workloadArtifacts(), workloadDigests{}, testEgressPort)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(apierrors.IsNotFound(r.Get(ctx, objectKey("cinder-volume-gone"), &appsv1.Deployment{}))).
		To(BeTrue())
	g.Expect(cinder.Status.VolumeServices).To(BeNil())
	cond := cinderCondition(cinder, "VolumeServicesReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonNoBackends))
}

// TestReconcileVolumeServices_ApplyFailureNamesTheDeployment covers the error
// path: one Cinder projects a Deployment per backend, so the message has to name
// which of them could not be applied.
func TestReconcileVolumeServices_ApplyFailureNamesTheDeployment(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()
	boom := errors.New("quota exceeded")
	r := failingApplyReconciler(boom, "Deployment", "cinder-volume-nfs", cinder)

	_, err := r.reconcileVolumeServices(context.Background(), r.Client, cinder,
		[]backendProjection{testBackendProjection("nfs")},
		workloadArtifacts(), workloadDigests{}, testEgressPort)

	g.Expect(err).To(MatchError(boom))
	g.Expect(err).To(MatchError(ContainSubstring("ensuring volume Deployment cinder-volume-nfs:")))
}

// TestReconcileVolumeServices_RollingUpdateHoldsUntilConverged covers the upgrade
// gate: the contract phase runs migrations the old volume services have no code
// for, so the pass polls until every one of them runs the new image.
func TestReconcileVolumeServices_RollingUpdateHoldsUntilConverged(t *testing.T) {
	ctx := context.Background()
	backend := testBackendProjection("nfs")

	t.Run("a replica still on the old template holds the pass", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := rollingUpdateCinder()
		lagging := readyVolumeDeployment(cinder, backend)
		lagging.Status.UpdatedReplicas = 0
		r := newCinderTestReconciler(cinder, lagging)

		res, err := r.reconcileVolumeServices(ctx, r.Client, cinder, []backendProjection{backend},
			workloadArtifacts(), workloadDigests{}, testEgressPort)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))
	})

	t.Run("a converged rollout releases the pass", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := rollingUpdateCinder()
		r := newCinderTestReconciler(cinder, readyVolumeDeployment(cinder, backend))

		res, err := r.reconcileVolumeServices(ctx, r.Client, cinder, []backendProjection{backend},
			workloadArtifacts(), workloadDigests{}, testEgressPort)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
	})
}

// TestReconcileVolumeServices_DetachStopsTheProcessFirst covers the ordering the
// detach depends on: a running cinder-volume re-registers itself every few
// seconds, so the registry entry may only be removed once its Deployment is
// gone. The pass that deletes it creates no Job and requeues.
func TestReconcileVolumeServices_DetachStopsTheProcessFirst(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cinder := workloadCinder()
	backend := detachingBackend("nfs")
	r := newCinderTestReconciler(cinder, backend,
		readyVolumeDeployment(cinder, testBackendProjection("nfs")))

	res, err := r.reconcileVolumeServices(ctx, r.Client, cinder, nil,
		workloadArtifacts(), workloadDigests{}, testEgressPort)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))
	g.Expect(apierrors.IsNotFound(r.Get(ctx, objectKey("cinder-volume-nfs"), &appsv1.Deployment{}))).
		To(BeTrue())
	g.Expect(apierrors.IsNotFound(r.Get(ctx, objectKey("cinder-nfs-service-remove"), &batchv1.Job{}))).
		To(BeTrue(), "the Job must not run while the process it unregisters is still up")
	g.Expect(backend.Finalizers).To(ContainElement(CinderBackendServiceRemoveFinalizer))
}

// TestReconcileVolumeServices_DetachRunsTheRemoveJob covers the second pass: the
// process is gone, so the Job that drops the registry entry is created and the
// pass polls for it.
func TestReconcileVolumeServices_DetachRunsTheRemoveJob(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cinder := workloadCinder()
	r := newCinderTestReconciler(cinder, detachingBackend("nfs"))

	res, err := r.reconcileVolumeServices(ctx, r.Client, cinder, nil,
		workloadArtifacts(), workloadDigests{}, testEgressPort)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))

	var removeJob batchv1.Job
	g.Expect(r.Get(ctx, objectKey("cinder-nfs-service-remove"), &removeJob)).To(Succeed())
	g.Expect(removeJob.Labels).To(HaveKeyWithValue("app.kubernetes.io/component", "service-remove"))
	g.Expect(removeJob.Spec.Template.Labels).To(
		HaveKeyWithValue("app.kubernetes.io/component", "service-remove"),
		"the pod carries the labels the NetworkPolicy selects the database egress by")
	g.Expect(*removeJob.Spec.TTLSecondsAfterFinished).To(Equal(int32(3600)))
	g.Expect(*removeJob.Spec.BackoffLimit).To(Equal(int32(4)))
	g.Expect(removeJob.Spec.Template.Spec.Containers[0].Command).To(Equal([]string{
		"/bin/sh", "-eu", "-c", serviceRemoveScript(cinder, "nfs"),
	}))
}

// TestReconcileVolumeServices_DetachCompletes covers the release: the registry
// entry is gone, so the projected Secrets are swept, the finalizer is dropped,
// the removal is reported on the Cinder, and the Job's terminal state is counted
// once.
func TestReconcileVolumeServices_DetachCompletes(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cinder := workloadCinder()
	backend := detachingBackend("nfs")
	stale := staleProjectionSecret(t, cinder, backendSecretBaseName(cinder, "nfs"))
	r := newCinderTestReconciler(cinder, backend, stale,
		terminalServiceRemoveJob(cinder, "nfs", batchv1.JobComplete))

	res, err := r.reconcileVolumeServices(ctx, r.Client, cinder, nil,
		workloadArtifacts(), workloadDigests{}, testEgressPort)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	var released cinderv1alpha1.CinderBackend
	g.Expect(apierrors.IsNotFound(r.Get(ctx, client.ObjectKeyFromObject(backend), &released))).To(BeTrue(),
		"a backend whose last finalizer is removed is collected")
	g.Expect(apierrors.IsNotFound(r.Get(ctx, client.ObjectKeyFromObject(stale), &corev1.Secret{}))).
		To(BeTrue(), "the projection carries an export this Cinder no longer serves")
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(ContainElement(
		ContainSubstring("Normal ServiceRemoved")))
	g.Expect(cinder.Annotations).To(HaveKeyWithValue(
		job.JobUIDAnnotationKey(serviceRemoveJobSuffix("nfs")), "nfs-service-remove-uid"),
		"the terminal state is counted at most once per Job UID")
	g.Expect(cinderCondition(cinder, "VolumeServicesReady").Reason).To(Equal(conditionReasonNoBackends))
}

// TestReconcileVolumeServices_DetachJobFailed covers the wedged detach: the
// registry entry is still there, so the finalizer is held, the failure is named
// on the CR and as an event, and the pass returns without a requeue because
// nothing changes until an operator deletes the Job.
func TestReconcileVolumeServices_DetachJobFailed(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cinder := workloadCinder()
	backend := detachingBackend("nfs")
	r := newCinderTestReconciler(cinder, backend,
		terminalServiceRemoveJob(cinder, "nfs", batchv1.JobFailed))

	res, err := r.reconcileVolumeServices(ctx, r.Client, cinder, nil,
		workloadArtifacts(), workloadDigests{}, testEgressPort)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := cinderCondition(cinder, "VolumeServicesReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonServiceRemoveJobFailed))
	g.Expect(cond.Message).To(ContainSubstring("cinder-nfs-service-remove"))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(ContainElement(
		ContainSubstring("Warning ServiceRemoveJobFailed")))

	var held cinderv1alpha1.CinderBackend
	g.Expect(r.Get(ctx, client.ObjectKeyFromObject(backend), &held)).To(Succeed())
	g.Expect(held.Finalizers).To(ContainElement(CinderBackendServiceRemoveFinalizer),
		"the backend is held until its registry entry is gone")

	// Deleting the Job is what retries the removal: the next pass finds none and
	// creates it again.
	g.Expect(r.Delete(ctx, terminalServiceRemoveJob(cinder, "nfs", batchv1.JobFailed))).To(Succeed())
	res, err = r.reconcileVolumeServices(ctx, r.Client, cinder, nil,
		workloadArtifacts(), workloadDigests{}, testEgressPort)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))
	g.Expect(r.Get(ctx, objectKey("cinder-nfs-service-remove"), &batchv1.Job{})).To(Succeed())
}

// TestBuildServiceRemoveJob_MountsTheDatabaseTLSKeypair covers the optional
// projection: the Job talks to the same database the volume service does, so
// without the keypair the DSN's ssl_* paths name files that are not there.
func TestBuildServiceRemoveJob_MountsTheDatabaseTLSKeypair(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()

	plain := buildServiceRemoveJob(cinder, "nfs", workloadArtifacts())
	g.Expect(plain.Spec.Template.Spec.Volumes).To(HaveLen(1))

	cinder.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "db-client"},
	}
	withTLS := buildServiceRemoveJob(cinder, "nfs", workloadArtifacts())
	g.Expect(withTLS.Spec.Template.Spec.Volumes).To(HaveLen(2))
	g.Expect(withTLS.Spec.Template.Spec.Volumes[1].Name).To(Equal(dbTLSVolumeName))
	mounts := withTLS.Spec.Template.Spec.Containers[0].VolumeMounts
	g.Expect(mounts[len(mounts)-1].MountPath).To(Equal(dbTLSMountPath))
}
