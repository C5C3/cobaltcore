// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// Condition reason constants for BackupServiceReady.
const (
	conditionReasonBackupServiceReady      = "BackupServiceReady"
	conditionReasonWaitingForBackupService = "WaitingForBackupService"
	// conditionReasonBackupNotConfigured is set when no CinderBackupBackend is
	// projected. It is True rather than False: backups are opt-in, and a Cinder
	// without them serves volumes as before.
	conditionReasonBackupNotConfigured = "BackupNotConfigured"
)

// componentBackup is the app.kubernetes.io/component value of the backup pod,
// and the name suffix of its Deployment.
const componentBackup = "backup"

// cinderBackupConfigDir is the second oslo.config --config-dir cinder-backup
// loads: the [DEFAULT] backup driver section projected from the backup backend's
// Secret.
const cinderBackupConfigDir = "/etc/cinder/backup.conf.d"

// Pod volume names of the backup service: the projected driver section, and the
// export the backups are written to.
const (
	backupVolumeName      = "backup"
	backupShareVolumeName = "backup-share"
)

// backupDeploymentName returns the name of the cinder-backup Deployment.
func backupDeploymentName(cinder *cinderv1alpha1.Cinder) string {
	return cinder.Name + "-" + componentBackup
}

// reconcileBackupService ensures the cinder-backup Deployment matches the
// projected backup backend and sets the BackupServiceReady condition. A nil
// projection deletes the Deployment: the backup backend was detached or is no
// longer credential-ready, and a backup service without a target writes nowhere.
//
// backends are the projected volume backends, whose exports the backup service
// mounts as well: cinder-backup reads the source volume itself through os-brick,
// at the same path the volume service holds it at.
//
// It follows the scheduler's result contract: a zero result outside an upgrade
// whatever the readiness is, and polling during the RollingUpdate phase until
// the rollout has converged.
func (r *CinderReconciler) reconcileBackupService(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder, backup *backupProjection, backends []backendProjection,
	art configArtifacts, digests workloadDigests, egressPort int32,
) (ctrl.Result, error) {
	if backup == nil {
		stale := &appsv1.Deployment{}
		stale.SetName(backupDeploymentName(cinder))
		stale.SetNamespace(cinder.Namespace)
		if err := client.IgnoreNotFound(children.Delete(ctx, stale)); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting backup Deployment %s: %w", stale.GetName(), err)
		}
		conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
			Type:               "BackupServiceReady",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cinder.Generation,
			Reason:             conditionReasonBackupNotConfigured,
			Message:            "No CinderBackupBackend is projected; the backup service is not rendered",
		})
		return ctrl.Result{}, nil
	}

	r.warnSharedExportConflicts(ctx, cinder, backends)

	deploy := buildBackupDeployment(cinder, backup, backends, art, digests, egressPort)
	ready, err := deployment.EnsureDeployment(ctx, children, r.Scheme, cinder, deploy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring backup Deployment: %w", err)
	}

	if cinder.Status.UpgradePhase == commonv1.UpgradePhaseRollingUpdate &&
		!cinderDeploymentRolledOut(deploy) {
		conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
			Type:               "BackupServiceReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cinder.Generation,
			Reason:             conditionReasonWaitingForBackupService,
			Message:            "Waiting for the upgraded image to finish rolling out on the backup service",
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, nil
	}

	if !ready {
		conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
			Type:               "BackupServiceReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cinder.Generation,
			Reason:             conditionReasonWaitingForBackupService,
			Message:            "Cinder backup deployment is not yet available",
		})
		return ctrl.Result{}, nil
	}

	conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
		Type:               "BackupServiceReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cinder.Generation,
		Reason:             conditionReasonBackupServiceReady,
		Message:            "Cinder backup deployment is available",
	})
	return ctrl.Result{}, nil
}

// buildBackupDeployment constructs the desired cinder-backup Deployment: the
// workload volumes every process shares, the projected driver section, the
// export the backups are written to, and every volume backend's export beside
// it.
//
// The replica count and the Recreate strategy come from spec.backup.deployment,
// where the CEL rules pin them for the same reason they pin the volume service:
// the backup service owns its target through a host identity, and a second
// process under it would have the same state open twice.
func buildBackupDeployment(cinder *cinderv1alpha1.Cinder, backup *backupProjection,
	backends []backendProjection, art configArtifacts, digests workloadDigests, egressPort int32,
) *appsv1.Deployment {
	volumes, mounts := cinderWorkloadVolumes(cinder, art)

	volumes = append(volumes,
		corev1.Volume{
			Name: backupVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: backup.secretName,
					Items: []corev1.KeyToPath{
						{Key: backupConfDataKey, Path: backupConfDataKey},
					},
				},
			},
		},
		inlineNFSVolume(backupShareVolumeName, backup.server, backup.path, backup.mountOptions),
	)
	mounts = append(mounts,
		corev1.VolumeMount{Name: backupVolumeName, MountPath: cinderBackupConfigDir, ReadOnly: true},
		corev1.VolumeMount{
			Name:      backupShareVolumeName,
			MountPath: shareMountPath(backupMountPointBase, backup.server, backup.path),
		},
	)

	// The source side. A backup reads the volume itself rather than a copy the
	// volume service hands over, so every export a volume backend serves is
	// mounted here too, at the path cinder resolved the volume's provider
	// location to. Two backends on one export share a single mount; see
	// backupShareMounts.
	sources, _ := backupShareMounts(backends)
	for _, backend := range sources {
		name := shareVolumeName(backend.name)
		volumes = append(volumes, inlineNFSVolume(name, backend.server, backend.path, backend.mountOptions))
		mounts = append(mounts, corev1.VolumeMount{
			Name:      name,
			MountPath: shareMountPath(nfsMountPointBase, backend.server, backend.path),
		})
	}

	deploy := deployment.BuildWorkload(deployment.WorkloadParams{
		Namespace:      cinder.Namespace,
		Name:           backupDeploymentName(cinder),
		Labels:         componentLabels(cinder, componentBackup),
		SelectorLabels: componentSelectorLabels(cinder, componentBackup),
		PodAnnotations: cinderRPCPodAnnotations(cinder, digests),
		Deployment:     &cinder.Spec.Backup.Deployment,
		Autoscaling:    nil,
		Container: deployment.ContainerParams{
			Name:  componentBackup,
			Image: cinder.Spec.Image.Reference(),
			Command: []string{
				"cinder-backup",
				"--config-dir", cinderConfigDir,
				"--config-dir", cinderBackupConfigDir,
			},
			Env: append(cinderWorkloadEnv(cinder), amqpPortEnv(egressPort)),
			// Readiness alone, as on the scheduler and the volume services: a restart
			// would abort the backup in flight without reaching the broker any sooner.
			ReadinessProbe: amqpReadinessProbe(),
			VolumeMounts:   mounts,
		},
		Volumes: volumes,
	})
	return withFSGroupChangePolicy(deploy)
}

// sharedExportConflict names a projected volume backend whose mountOptions the
// backup pod does not apply, because a backend ahead of it in the projection
// order already mounted the same export with different ones.
type sharedExportConflict struct {
	backend backendProjection
	first   backendProjection
}

// backupShareMounts splits the projected volume backends into the ones the
// backup pod mounts a source export for, and the mount-option conflicts that
// split hides.
//
// The mount path is derived from "server:path", not from the backend name, so
// two backends serving the same export resolve to the same directory and it is
// mounted once. Emitting it per backend would give the pod two volumeMounts with
// the same mountPath, which the API server rejects — and that rejection sits
// before every later pipeline step, so it would wedge the whole Cinder.
//
// Mounting it once means the first backend naming an export decides how the
// backup pod mounts it. A later backend on the same export whose mountOptions
// differ is returned as a conflict rather than dropped in silence: its options
// are absent from a pod spec that names no backend they belong to, and if they
// were the ones the export needs, the kubelet's CSI mount fails and the backup
// pod never leaves ContainerCreating — for every backend, not just this one.
func backupShareMounts(backends []backendProjection) ([]backendProjection, []sharedExportConflict) {
	var sources []backendProjection
	var conflicts []sharedExportConflict
	mounted := make(map[string]backendProjection, len(backends))
	for _, backend := range backends {
		path := shareMountPath(nfsMountPointBase, backend.server, backend.path)
		first, seen := mounted[path]
		if !seen {
			mounted[path] = backend
			sources = append(sources, backend)
			continue
		}
		if first.mountOptions != backend.mountOptions {
			conflicts = append(conflicts, sharedExportConflict{backend: backend, first: first})
		}
	}
	return sources, conflicts
}

// sharedExportValueLimit bounds each spec-sourced value the shared-export
// warning interpolates. spec.nfs.path and spec.nfs.mountOptions carry a pattern
// but no length marker, so their only bound is the CR's own object-size limit,
// and the conflict the message reports is legitimate and permanent: it is
// re-emitted on every pass through reconcileBackupService for as long as the two
// backends stay attached. Three unbounded values in that message would be a log
// line of etcd-object size per reconcile, and an Event the API server rejects
// for exceeding the object-size limit — which would drop the warning the message
// exists to deliver.
const sharedExportValueLimit = 256

// truncateSharedExportValue shortens s to sharedExportValueLimit runes, marking
// the cut so a reader can tell a bounded value from a clipped one.
func truncateSharedExportValue(s string) string {
	runes := []rune(s)
	if len(runes) <= sharedExportValueLimit {
		return s
	}
	return string(runes[:sharedExportValueLimit]) + "..."
}

// warnSharedExportConflicts reports every mount-option conflict the backup pod's
// single mount per export hides, as a Warning event on the Cinder beside the log
// line. The rendered Deployment cannot carry the discarded option string, so
// this is the only place it is named — which is why each interpolated value is
// clipped rather than dropped.
func (r *CinderReconciler) warnSharedExportConflicts(ctx context.Context,
	cinder *cinderv1alpha1.Cinder, backends []backendProjection,
) {
	_, conflicts := backupShareMounts(backends)
	for _, conflict := range conflicts {
		msg := fmt.Sprintf(
			"Backend %s serves the export %s:%s that backend %s already mounts with %q in the backup service, "+
				"so its own mountOptions %q are not applied there",
			conflict.backend.name, conflict.backend.server,
			truncateSharedExportValue(conflict.backend.path), conflict.first.name,
			truncateSharedExportValue(conflict.first.mountOptions),
			truncateSharedExportValue(conflict.backend.mountOptions))
		log.FromContext(ctx).Info(msg)
		r.Recorder.Event(cinder, corev1.EventTypeWarning, "SharedExportMountOptionsIgnored", msg)
	}
}
