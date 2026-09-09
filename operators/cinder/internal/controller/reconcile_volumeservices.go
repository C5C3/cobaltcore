// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	//nolint:gosec // G501: os-brick names an NFS mount directory by the MD5 of "server:path". The digest is a path, not a security control, and the name has to match what the driver computes.
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/config"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/job"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/cinder/internal/metrics"
)

// Condition reason constants for VolumeServicesReady. The empty-set reason is
// conditionReasonNoBackends, shared with the BackendsReady vocabulary: both
// describe the same fact, that no CinderBackend is projected.
const (
	conditionReasonAllVolumeServicesReady   = "AllVolumeServicesReady"
	conditionReasonWaitingForVolumeServices = "WaitingForVolumeServices"
	conditionReasonServiceRemoveJobFailed   = "ServiceRemoveJobFailed"
)

// Event reasons of the detach flow. The events go to the Cinder rather than to
// the backend: the backend is being deleted, so an event on it is collected with
// it, while the Cinder is where an operator reads why a detach is stuck.
const (
	eventReasonServiceRemoved         = "ServiceRemoved"
	eventReasonServiceRemoveJobFailed = "ServiceRemoveJobFailed"
)

// CinderBackendServiceRemoveFinalizer holds a detaching CinderBackend until its
// volume service has been unregistered from the service registry. It is
// exported because the CinderBackend controller adds it while the cinder-side
// step here is what removes it: the registry entry belongs to the Cinder, and
// only a Job running against the Cinder's database can drop it.
const CinderBackendServiceRemoveFinalizer = "cinder.openstack.c5c3.io/service-remove"

// nfsCSIDriverName is the CSI driver the inline share volumes name. The driver
// mounts the export at pod start, so no PersistentVolume outlives the pod and
// nothing has to be reclaimed when a backend detaches.
const nfsCSIDriverName = "nfs.csi.k8s.io"

// componentServiceRemove is the app.kubernetes.io/component value of the
// service-remove Job and its pod, so the Job pods are separable from the volume
// pods they replace.
const componentServiceRemove = "service-remove"

// componentVolumePrefix prefixes the component of one volume service with the
// backend it serves: each backend gets its own Deployment, and the component is
// what keeps the four selectors of one Cinder apart.
const componentVolumePrefix = "volume-"

// shareVolumePrefix prefixes the pod volume carrying one backend's export.
const shareVolumePrefix = "share-"

// serviceRemoveJobTTL is how long a finished service-remove Job is kept before
// the Job controller collects it. An hour leaves the logs of a detach readable
// after the fact without keeping the Job for the life of the Cinder.
const serviceRemoveJobTTL int32 = 3600

// serviceRemoveJobBackoffLimit is how often the service-remove Job is retried
// before it is declared failed. It matches the migration Jobs' limit.
const serviceRemoveJobBackoffLimit int32 = 4

// cinderVolumeConfigDir is the third oslo.config --config-dir a cinder-volume
// loads: the [DEFAULT] enabled_backends overlay naming the single backend this
// process serves. It is separate from the backend section's directory because
// the two files come from the same Secret under different keys and are read by
// different halves of the configuration.
const cinderVolumeConfigDir = "/etc/cinder/volume.conf.d"

// Pod volume names of one volume service's two projected files.
const (
	backendsVolumeName      = "backend"
	volumeOverlayVolumeName = "volume-overlay"
)

// The service-remove command, split at the host identity so the script test can
// run these exact bytes against a stubbed cinder-manage. Nothing but the tests
// uses the two halves on their own.
//
// Exit code 2 is "Host not found": a backend whose cinder-volume never
// registered has no entry to remove, and it must still detach, so the script
// normalises 2 to a clean exit and lets every other code fail the Job.
const (
	serviceRemoveScriptHead = "rc=0; cinder-manage --config-dir " + cinderConfigDir +
		" service remove cinder-volume "
	serviceRemoveScriptTail = ` || rc=$?; case "$rc" in 0|2) exit 0;; *) exit "$rc";; esac`
)

// volumeComponent returns the app.kubernetes.io/component value of the volume
// service of one backend.
func volumeComponent(name string) string {
	return componentVolumePrefix + name
}

// volumeDeploymentName returns the name of the Deployment running the volume
// service of one backend.
func volumeDeploymentName(cinder *cinderv1alpha1.Cinder, name string) string {
	return cinder.Name + "-" + volumeComponent(name)
}

// serviceRemoveJobName returns the name of the Job that unregisters one
// backend's volume service.
func serviceRemoveJobName(cinder *cinderv1alpha1.Cinder, name string) string {
	return cinder.Name + "-" + name + "-" + componentServiceRemove
}

// serviceRemoveJobSuffix returns the per-backend suffix the terminal-state
// metric of a service-remove Job is deduped under, so two backends detaching in
// the same pass are counted separately.
func serviceRemoveJobSuffix(name string) string {
	return componentServiceRemove + "-" + name
}

// shareVolumeName returns the pod volume carrying one backend's NFS export.
func shareVolumeName(name string) string {
	return shareVolumePrefix + name
}

// serviceRemoveScript is what the detach Job runs: it drops the backend's entry
// from the service registry, so a detached backend stops appearing in
// `cinder service-list` and its host identity can be reused.
func serviceRemoveScript(cinder *cinderv1alpha1.Cinder, name string) string {
	return serviceRemoveScriptHead + cinder.Name + "@" + name + serviceRemoveScriptTail
}

// shareMountPath returns the in-pod directory the export server:path is mounted
// at, below base. The directory name is the hex MD5 of "server:path", which is
// how os-brick's remotefs driver derives it: cinder resolves the provider
// location of every volume to that path, so a mount anywhere else leaves the
// volumes it holds unreachable.
func shareMountPath(base, server, path string) string {
	//nolint:gosec // G401: the digest names a mount directory (see the crypto/md5 import), not a security control.
	sum := md5.Sum([]byte(server + ":" + path))
	return base + "/" + hex.EncodeToString(sum[:])
}

// inlineNFSVolume returns the inline CSI volume mounting one NFS export. It is
// inline rather than a PersistentVolumeClaim because the export is a shared
// filesystem the pod only needs while it runs: there is no per-pod storage to
// bind, and an inline volume leaves nothing behind to reclaim when a backend
// detaches. An empty option string omits the attribute entirely, so the driver
// applies its own mount defaults instead of an empty -o flag.
func inlineNFSVolume(name, server, path, mountOptions string) corev1.Volume {
	attributes := map[string]string{
		"server": server,
		"share":  path,
	}
	if mountOptions != "" {
		attributes["mountOptions"] = mountOptions
	}
	return corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{
			CSI: &corev1.CSIVolumeSource{
				Driver:           nfsCSIDriverName,
				VolumeAttributes: attributes,
			},
		},
	}
}

// withFSGroupChangePolicy sets fsGroupChangePolicy: OnRootMismatch on a pod
// template that mounts an NFS export, keeping the fsGroup the shared workload
// builder applies. Under the default policy the kubelet walks and chowns every
// mounted volume on every pod start, which on an export holding a fleet's
// volumes is an unbounded recursive operation over the network: minutes to
// hours of startup, and a rewrite of the ownership the storage administrator
// set. OnRootMismatch checks the export's root and leaves it alone when it
// already carries the right group.
func withFSGroupChangePolicy(deploy *appsv1.Deployment) *appsv1.Deployment {
	sc := deploy.Spec.Template.Spec.SecurityContext
	if sc == nil {
		sc = &corev1.PodSecurityContext{}
		deploy.Spec.Template.Spec.SecurityContext = sc
	}
	sc.FSGroupChangePolicy = ptr.To(corev1.FSGroupChangeOnRootMismatch)
	return deploy
}

// reconcileVolumeServices projects one cinder-volume Deployment per attached,
// credential-ready backend, reports the host identity each of them registers
// under, drives the detach of the backends that are being deleted, and sets the
// aggregated VolumeServicesReady condition.
//
// Each backend gets its own Deployment rather than one Deployment serving all of
// them: a cinder-volume owns its backend through a host identity, so a process
// serving two backends could not be restarted for one of them alone, and a
// backend whose export is unreachable would take its siblings down with it.
//
// Outside an upgrade it returns a zero result unless a detach is in flight; the
// Owns(Deployment) watch re-enqueues the CR as the rollouts progress. During the
// RollingUpdate phase it requeues until every projected Deployment has converged
// onto the new image.
func (r *CinderReconciler) reconcileVolumeServices(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder, backends []backendProjection, art configArtifacts,
	digests workloadDigests, egressPort int32,
) (ctrl.Result, error) {
	var notReady []string
	rolledOut := true
	for _, backend := range backends {
		deploy := buildVolumeDeployment(cinder, backend, art, digests, egressPort)
		ready, err := deployment.EnsureDeployment(ctx, children, r.Scheme, cinder, deploy)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensuring volume Deployment %s: %w", deploy.Name, err)
		}
		if !ready {
			notReady = append(notReady, backend.name)
		}
		if !cinderDeploymentRolledOut(deploy) {
			rolledOut = false
		}
	}

	stopped, err := r.pruneVolumeDeployments(ctx, children, cinder, backends)
	if err != nil {
		return ctrl.Result{}, err
	}
	cinder.Status.VolumeServices = volumeServiceStatuses(cinder, backends)

	detachResult, failedJob, err := r.reconcileDetachingBackends(ctx, children, cinder, art, stopped)
	if err != nil {
		return ctrl.Result{}, err
	}

	switch {
	case failedJob != "":
		// A failed detach outranks the fleet's own readiness: the projected volume
		// services may all be running while a backend cannot be released, and that
		// is what an operator has to act on.
		conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
			Type:               "VolumeServicesReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cinder.Generation,
			Reason:             conditionReasonServiceRemoveJobFailed,
			Message: fmt.Sprintf("Job %s could not remove the volume service from the service registry; "+
				"delete the Job to retry", failedJob),
		})
	case len(backends) == 0:
		conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
			Type:               "VolumeServicesReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cinder.Generation,
			Reason:             conditionReasonNoBackends,
			Message:            "No CinderBackend is projected; volume services are not rendered",
		})
	case len(notReady) > 0:
		conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
			Type:               "VolumeServicesReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cinder.Generation,
			Reason:             conditionReasonWaitingForVolumeServices,
			Message:            "Waiting for the volume services of: " + strings.Join(notReady, ", "),
		})
	default:
		conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
			Type:               "VolumeServicesReady",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cinder.Generation,
			Reason:             conditionReasonAllVolumeServicesReady,
			Message:            fmt.Sprintf("All %d projected volume services are available", len(backends)),
		})
	}

	if !detachResult.IsZero() {
		return detachResult, nil
	}
	if cinder.Status.UpgradePhase == commonv1.UpgradePhaseRollingUpdate && !rolledOut {
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, nil
	}
	return ctrl.Result{}, nil
}

// volumeServiceStatuses reports the host identity of every projected backend,
// sorted by backend name so the status does not churn between passes. It returns
// nil when nothing is projected, which clears the field rather than leaving the
// last fleet behind.
func volumeServiceStatuses(cinder *cinderv1alpha1.Cinder, backends []backendProjection) []cinderv1alpha1.VolumeServiceStatus {
	if len(backends) == 0 {
		return nil
	}
	statuses := make([]cinderv1alpha1.VolumeServiceStatus, 0, len(backends))
	for _, backend := range backends {
		statuses = append(statuses, cinderv1alpha1.VolumeServiceStatus{
			Backend: backend.name,
			Host:    cinder.Name + "@" + backend.name,
		})
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Backend < statuses[j].Backend })
	return statuses
}

// pruneVolumeDeployments deletes the volume Deployments of backends this Cinder
// no longer projects: a backend that was detached, renamed or skipped keeps no
// process, and one left running would keep re-registering the host identity the
// detach removes. It returns the names it deleted in this pass, which is what
// the detach flow waits out before it unregisters anything.
func (r *CinderReconciler) pruneVolumeDeployments(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder, backends []backendProjection,
) (map[string]struct{}, error) {
	var list appsv1.DeploymentList
	if err := children.List(ctx, &list, client.InNamespace(cinder.Namespace),
		client.MatchingLabels(commonLabels(cinder))); err != nil {
		return nil, fmt.Errorf("listing volume Deployments in namespace %s: %w", cinder.Namespace, err)
	}

	projected := make(map[string]struct{}, len(backends))
	for _, backend := range backends {
		projected[volumeDeploymentName(cinder, backend.name)] = struct{}{}
	}

	stopped := map[string]struct{}{}
	prefix := cinder.Name + "-" + componentVolumePrefix
	for i := range list.Items {
		name := list.Items[i].Name
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if _, ok := projected[name]; ok {
			continue
		}
		if err := client.IgnoreNotFound(children.Delete(ctx, &list.Items[i])); err != nil {
			return nil, fmt.Errorf("deleting unprojected volume Deployment %s: %w", name, err)
		}
		stopped[name] = struct{}{}
		log.FromContext(ctx).Info("deleted unprojected volume Deployment", "deployment", name)
	}
	return stopped, nil
}

// reconcileDetachingBackends releases the backends that are being deleted, in
// name order, one pass at a time. It returns the requeue a detach in flight
// needs and the name of a service-remove Job that has permanently failed, which
// the caller turns into the VolumeServicesReady condition.
//
// stopped names the volume Deployments this very pass deleted, so a detach never
// unregisters a process that was still running a moment ago.
func (r *CinderReconciler) reconcileDetachingBackends(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder, art configArtifacts, stopped map[string]struct{},
) (ctrl.Result, string, error) {
	// The backends are sibling configuration CRs on the management cluster, so
	// this list and the finalizer update below go through the embedded client;
	// only the Deployment and the Job live on children.
	var list cinderv1alpha1.CinderBackendList
	if err := r.List(ctx, &list, client.InNamespace(cinder.Namespace),
		client.MatchingFields{CinderBackendCinderRefIndexKey: cinder.Name}); err != nil {
		return ctrl.Result{}, "", fmt.Errorf("listing CinderBackends for %s: %w", cinder.Name, err)
	}

	detaching := make([]*cinderv1alpha1.CinderBackend, 0, len(list.Items))
	for i := range list.Items {
		backend := &list.Items[i]
		if backend.DeletionTimestamp == nil ||
			!controllerutil.ContainsFinalizer(backend, CinderBackendServiceRemoveFinalizer) {
			continue
		}
		detaching = append(detaching, backend)
	}
	sort.Slice(detaching, func(i, j int) bool { return detaching[i].Name < detaching[j].Name })

	for _, backend := range detaching {
		res, failedJob, err := r.detachBackend(ctx, children, cinder, backend, art, stopped)
		if err != nil || failedJob != "" || !res.IsZero() {
			return res, failedJob, err
		}
	}
	return ctrl.Result{}, "", nil
}

// detachBackend releases one deleting backend: it stops the volume service,
// removes the host identity from the service registry, sweeps the projected
// Secrets, and lets the CR go by removing the finalizer.
//
// The order is what makes the removal stick. A running cinder-volume reports
// itself to the registry every few seconds, so an entry removed while the
// process still runs comes back; the Deployment therefore has to be gone before
// the Job runs, and the pass that deletes it returns rather than continuing.
func (r *CinderReconciler) detachBackend(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder, backend *cinderv1alpha1.CinderBackend, art configArtifacts,
	stopped map[string]struct{},
) (ctrl.Result, string, error) {
	key := client.ObjectKey{Namespace: cinder.Namespace, Name: volumeDeploymentName(cinder, backend.Name)}
	if _, justStopped := stopped[key.Name]; justStopped {
		// The volume service was stopped in this very pass, so its pods are still
		// terminating and would report themselves back into the registry after the
		// removal. The Job waits for a pass that finds the Deployment already gone.
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, "", nil
	}

	var deploy appsv1.Deployment
	switch err := children.Get(ctx, key, &deploy); {
	case err == nil:
		if err := client.IgnoreNotFound(children.Delete(ctx, &deploy)); err != nil {
			return ctrl.Result{}, "", fmt.Errorf("deleting volume Deployment %s: %w", key.Name, err)
		}
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, "", nil
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, "", fmt.Errorf("getting volume Deployment %s: %w", key.Name, err)
	}

	removeJob := buildServiceRemoveJob(cinder, backend.Name, art)
	done, observed, err := job.RunJob(ctx, children, r.Scheme, cinder, removeJob)
	switch {
	case errors.Is(err, job.ErrJobFailed):
		// The finalizer stays: the registry entry is still there, and deleting the
		// failed Job is what retries the removal once the cause is understood.
		r.recordServiceRemoveTerminalState(ctx, cinder, backend.Name, observed)
		r.Recorder.Eventf(cinder, corev1.EventTypeWarning, eventReasonServiceRemoveJobFailed,
			"Job %s could not remove the volume service of backend %s", removeJob.Name, backend.Name)
		return ctrl.Result{}, removeJob.Name, nil
	case err != nil:
		return ctrl.Result{}, "", fmt.Errorf("running service-remove Job %s: %w", removeJob.Name, err)
	case !done:
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, "", nil
	}

	// The projection Secrets carry the export of a backend this Cinder no longer
	// serves, so the base name is swept whole rather than kept for a rollback.
	if err := config.PruneImmutableSecrets(ctx, children, r.Scheme, cinder, config.PruneOptions{
		BaseName:    backendSecretBaseName(cinder, backend.Name),
		Namespace:   cinder.Namespace,
		CurrentName: "",
		Retain:      0,
	}); err != nil {
		return ctrl.Result{}, "", fmt.Errorf("pruning Secrets of detaching backend %q: %w", backend.Name, err)
	}

	controllerutil.RemoveFinalizer(backend, CinderBackendServiceRemoveFinalizer)
	if err := r.Update(ctx, backend); err != nil {
		return ctrl.Result{}, "", fmt.Errorf("removing the service-remove finalizer from %q: %w", backend.Name, err)
	}

	r.Recorder.Eventf(cinder, corev1.EventTypeNormal, eventReasonServiceRemoved,
		"Removed the volume service %s@%s from the service registry", cinder.Name, backend.Name)
	r.recordServiceRemoveTerminalState(ctx, cinder, backend.Name, observed)
	log.FromContext(ctx).Info("detached CinderBackend", "backend", backend.Name)
	return ctrl.Result{}, "", nil
}

// recordServiceRemoveTerminalState observes one backend's service-remove Job and
// emits cinder_operator_service_remove_total /
// cinder_operator_service_remove_duration_seconds exactly once per (backend, Job
// UID) tuple, delegating to the shared job.RecordJobTerminalState. It is
// best-effort: a transient patch failure defers emission to the next pass and
// records a ServiceRemoveMetricEmissionDeferred Warning event so the degradation
// is visible via `kubectl describe cinder`.
func (r *CinderReconciler) recordServiceRemoveTerminalState(ctx context.Context,
	cinder *cinderv1alpha1.Cinder, name string, observed *batchv1.Job,
) {
	job.RecordJobTerminalState(ctx, r.Client, r.Recorder, cinder, serviceRemoveJobSuffix(name), observed,
		"ServiceRemoveMetricEmissionDeferred",
		func(result string, duration time.Duration) {
			metrics.RecordServiceRemove(cinder.Name, cinder.Namespace, result, duration)
		})
}

// buildVolumeDeployment constructs the cinder-volume Deployment of one backend:
// the workload volumes every process shares, the backend's own projected files,
// and the export it serves its volumes from.
//
// The replica count and the Recreate strategy come from spec.volume.deployment,
// where the CEL rules pin them: two cinder-volume processes under one host
// identity have the same volume state open twice, which the NFS drivers refuse,
// and a rolling update would run exactly that overlap.
func buildVolumeDeployment(cinder *cinderv1alpha1.Cinder, backend backendProjection,
	art configArtifacts, digests workloadDigests, egressPort int32,
) *appsv1.Deployment {
	component := volumeComponent(backend.name)
	shareVolume := shareVolumeName(backend.name)
	volumes, mounts := cinderWorkloadVolumes(cinder, art)

	volumes = append(volumes,
		corev1.Volume{
			Name: backendsVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: backend.secretName,
					Items: []corev1.KeyToPath{
						{Key: backendConfDataKey, Path: backendConfDataKey},
						// The file name the backend's nfs_shares_config option points at,
						// which carries the backend name so the mount is readable in a
						// process serving one backend.
						{Key: sharesDataKey, Path: backend.name + ".shares"},
					},
				},
			},
		},
		corev1.Volume{
			Name: volumeOverlayVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: backend.secretName,
					Items: []corev1.KeyToPath{
						{Key: volumeOverlayDataKey, Path: volumeOverlayDataKey},
					},
				},
			},
		},
		inlineNFSVolume(shareVolume, backend.server, backend.path, backend.mountOptions),
	)
	mounts = append(mounts,
		corev1.VolumeMount{Name: backendsVolumeName, MountPath: cinderBackendsConfigDir, ReadOnly: true},
		corev1.VolumeMount{Name: volumeOverlayVolumeName, MountPath: cinderVolumeConfigDir, ReadOnly: true},
		corev1.VolumeMount{
			Name:      shareVolume,
			MountPath: shareMountPath(nfsMountPointBase, backend.server, backend.path),
		},
	)

	deploy := deployment.BuildWorkload(deployment.WorkloadParams{
		Namespace:      cinder.Namespace,
		Name:           volumeDeploymentName(cinder, backend.name),
		Labels:         componentLabels(cinder, component),
		SelectorLabels: componentSelectorLabels(cinder, component),
		PodAnnotations: cinderRPCPodAnnotations(cinder, digests),
		Deployment:     &cinder.Spec.Volume.Deployment,
		Autoscaling:    nil,
		Container: deployment.ContainerParams{
			Name:  componentVolumePrefix + backend.name,
			Image: cinder.Spec.Image.Reference(),
			Command: []string{
				"cinder-volume",
				"--config-dir", cinderConfigDir,
				"--config-dir", cinderBackendsConfigDir,
				"--config-dir", cinderVolumeConfigDir,
			},
			Env: append(cinderWorkloadEnv(cinder), amqpPortEnv(egressPort)),
			// Readiness alone, as on the scheduler: restarting a cinder-volume for a
			// broker outage would abort the transfers in flight without bringing the
			// broker back.
			ReadinessProbe: amqpReadinessProbe(),
			VolumeMounts:   mounts,
		},
		Volumes: volumes,
	})
	return withFSGroupChangePolicy(deploy)
}

// buildServiceRemoveJob constructs the Job that unregisters one backend's volume
// service. It runs the same image and the same environment the volume service
// does, because it talks to the same database through the same rendered config.
func buildServiceRemoveJob(cinder *cinderv1alpha1.Cinder, name string, art configArtifacts) *batchv1.Job {
	var extraVolumes []corev1.Volume
	var extraMounts []corev1.VolumeMount
	if cinder.Spec.Database.TLS.IsEnabled() {
		volume, mount := cinderDBTLSVolumeAndMount(cinder)
		extraVolumes = append(extraVolumes, volume)
		extraMounts = append(extraMounts, mount)
	}

	removeJob := job.BuildMigrationJob(job.MigrationJobParams{
		Name:          serviceRemoveJobName(cinder, name),
		Namespace:     cinder.Namespace,
		Labels:        componentLabels(cinder, componentServiceRemove),
		Image:         cinder.Spec.Image.Reference(),
		ContainerName: componentServiceRemove,
		Command:       []string{"/bin/sh", "-eu", "-c", serviceRemoveScript(cinder, name)},
		// The whole ConfigMap, as the migration Jobs mount it: cinder-manage reads
		// no file selection, and the one extra file it sees is scheduler.conf, whose
		// host identity it never consults, because the command names the host to
		// remove.
		ConfigMapName:           art.configMapName,
		ConfigMountPath:         cinderConfigDir,
		Env:                     cinderWorkloadEnv(cinder),
		ExtraVolumes:            extraVolumes,
		ExtraVolumeMounts:       extraMounts,
		BackoffLimit:            serviceRemoveJobBackoffLimit,
		TTLSecondsAfterFinished: ptr.To(serviceRemoveJobTTL),
		SecurityContext:         deployment.RestrictedSecurityContext(),
	})
	// The shared builder labels the Job alone. The pod carries them too, so the
	// NetworkPolicy that lets this Cinder's pods reach the database covers the Job
	// pod as well.
	removeJob.Spec.Template.Labels = componentLabels(cinder, componentServiceRemove)
	return removeJob
}
