// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/job"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// componentHostDiscovery is the app.kubernetes.io/component value of the host
// discovery Job and its pod.
const componentHostDiscovery = "discover-hosts"

// hostDiscoveryRetryInterval is how long a finished discovery Job stays before
// a pool that still sees an unmapped host replaces it. It bounds a host that
// never gets a compute node record to one Job per interval plus the Job's run
// time.
const hostDiscoveryRetryInterval = 30 * time.Second

// hostDiscoveryDeadlineSeconds bounds one discovery run, and
// hostDiscoveryTTLSeconds removes the last Job once no pool looks at it.
const (
	hostDiscoveryDeadlineSeconds int64 = 300
	hostDiscoveryTTLSeconds      int32 = 300
)

// hostDiscoveryJobName returns the name of the discovery Job of a Nova. Every
// pool of the Nova shares it. The API server copies a Job's name into a label
// of its pod, so the name is held to 63 characters: a longer one, from a Nova
// admitted before the name bound, collapses onto a hash as
// dbArchiveCronJobName does.
func hostDiscoveryJobName(novaName string) string {
	suffix := "-" + componentHostDiscovery
	if len(novaName)+len(suffix) <= validation.LabelValueMaxLength {
		return novaName + suffix
	}
	sum := sha256.Sum256([]byte(novaName))
	kept := strings.TrimRight(
		novaName[:validation.LabelValueMaxLength-len(suffix)-dbArchiveNameHashLength], "-.")
	return fmt.Sprintf("%s-%x%s", kept, sum[:4], suffix)
}

// hostDiscoveryJob builds the Job that maps the compute hosts of the Nova's
// cells that have a compute node record and no host mapping yet. It runs the
// plain discovery: --by-service maps a host from its service record and leaves
// the compute node's mapped flag at 0, so every later discovery, the
// scheduler's periodic one included, still visits the node.
//
// The pod is the archive CronJob's nova-manage pod (dbArchiveCronJob): the
// whole config ConfigMap the API Deployment mounts, /tmp, the database TLS
// projections, the nova-manage environment, FSGroup and the Job pod settings.
// Its container runs image, the API Deployment's: that Deployment rolls to
// spec.image only once the Nova has migrated its schemas, so the code, the
// config and the schemas match during an upgrade too.
// A failed run is not retried inside the Job: the pool replaces a finished Job
// after hostDiscoveryRetryInterval while a host stays unmapped.
func hostDiscoveryJob(nova *novav1alpha1.Nova, configMapName, image string) *batchv1.Job {
	volumes := []corev1.Volume{{
		Name:         tmpVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
	mounts := []corev1.VolumeMount{{Name: tmpVolumeName, MountPath: tmpMountPath}}
	tlsVolumes, tlsMounts := novaDBTLSVolumesAndMounts(nova)
	volumes = append(volumes, tlsVolumes...)
	mounts = append(mounts, tlsMounts...)

	discovery := job.BuildMigrationJob(job.MigrationJobParams{
		Name:          hostDiscoveryJobName(nova.Name),
		Namespace:     nova.Namespace,
		Labels:        componentLabels(nova, componentHostDiscovery),
		Image:         image,
		ContainerName: componentHostDiscovery,
		Command: []string{
			"nova-manage", "--config-dir", novaConfigDir, "cell_v2", "discover_hosts", "--verbose",
		},
		ConfigMapName:           configMapName,
		ConfigMountPath:         novaConfigDir,
		Env:                     novaWorkloadEnv(nova, roleManage),
		ExtraVolumes:            volumes,
		ExtraVolumeMounts:       mounts,
		Pod:                     novaJobPod(nova),
		BackoffLimit:            0,
		TTLSecondsAfterFinished: ptr.To(hostDiscoveryTTLSeconds),
		SecurityContext:         deployment.RestrictedSecurityContext(),
	})
	// The shared builder has no parameter for these three.
	discovery.Spec.ActiveDeadlineSeconds = ptr.To(hostDiscoveryDeadlineSeconds)
	discovery.Spec.Template.Labels = componentLabels(nova, componentHostDiscovery)
	discovery.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{FSGroup: ptr.To(deployment.OpenStackUID)}
	return discovery
}

// ensureHostDiscovery runs the discovery Job of the pass's Nova for hosts, the
// registered compute hosts Nova does not list as mapped yet, and returns a note
// on the Job's state for the ServicesReady message. The Job lives in the
// Nova's namespace on the Nova's cluster:
//
//   - None exists: it is created, unless the Nova API Deployment mounts no
//     config yet.
//   - One runs: it is left alone.
//   - One finished: it is left alone for hostDiscoveryRetryInterval, then
//     deleted, so the next pass creates a fresh one. A failed one raises
//     HostDiscoveryFailed first.
//
// An existing Job has to be the Nova's own: on a target cluster one of that
// name without the Nova's ownership labels is refused, not read or deleted.
// One discovery maps every unmapped host, so two pools of a Nova share the Job,
// and whichever pass finds it absent creates it.
func (r *NovaComputeReconciler) ensureHostDiscovery(ctx context.Context, cr *novav1alpha1.NovaCompute,
	pass *novaComputePass, hosts []string,
) (string, error) {
	c := pass.novaChildren
	key := client.ObjectKey{Namespace: pass.nova.Namespace, Name: hostDiscoveryJobName(pass.nova.Name)}

	existing := &batchv1.Job{}
	if err := c.Get(ctx, key, existing); err != nil {
		if apierrors.IsNotFound(err) {
			return r.startHostDiscovery(ctx, cr, pass, key, hosts)
		}
		return "", fmt.Errorf("getting discovery Job %s: %w", key, err)
	}
	// The claim answers the ownership question on a copy, as
	// job.RunJobWithRerunKey does, so the Job read below stays what the API
	// server returned.
	if err := commonmulticluster.Claim(c, r.Scheme, pass.nova, existing.DeepCopy()); err != nil {
		return "", err
	}

	terminal := job.TerminalCondition(existing)
	if terminal == "" {
		return fmt.Sprintf("discovery Job %s is running", key), nil
	}
	var condition batchv1.JobCondition
	for _, cond := range existing.Status.Conditions {
		if cond.Type == terminal {
			condition = cond
			break
		}
	}
	outcome := "finished without mapping them"
	if terminal == batchv1.JobFailed {
		outcome = "failed"
	}
	if time.Since(condition.LastTransitionTime.Time) < hostDiscoveryRetryInterval {
		return fmt.Sprintf("discovery Job %s %s; it is replaced after %s", key, outcome, hostDiscoveryRetryInterval), nil
	}

	if terminal == batchv1.JobFailed {
		r.Recorder.Eventf(cr, corev1.EventTypeWarning, eventReasonHostDiscoveryFailed,
			"Job %s failed: %s", key, condition.Message)
	}
	// NotFound means the TTL controller removed the Job first.
	err := c.Delete(ctx, existing, client.PropagationPolicy(metav1.DeletePropagationBackground))
	if client.IgnoreNotFound(err) != nil {
		return "", fmt.Errorf("deleting discovery Job %s: %w", key, err)
	}
	return fmt.Sprintf("replacing the finished discovery Job %s", key), nil
}

// startHostDiscovery creates the discovery Job of the pass's Nova with the
// image and the config its API Deployment runs.
func (r *NovaComputeReconciler) startHostDiscovery(ctx context.Context, cr *novav1alpha1.NovaCompute,
	pass *novaComputePass, key client.ObjectKey, hosts []string,
) (string, error) {
	c := pass.novaChildren
	novaKey := client.ObjectKeyFromObject(pass.nova)
	configMapName, image, err := liveAPIDeployment(ctx, c, pass.nova)
	if err != nil {
		return "", fmt.Errorf("reading the config of Nova %s: %w", novaKey, err)
	}
	if configMapName == "" {
		return fmt.Sprintf("no discovery Job can run: the Nova API Deployment %s mounts no config yet", novaKey), nil
	}

	discovery := hostDiscoveryJob(pass.nova, configMapName, image)
	if err := commonmulticluster.Claim(c, r.Scheme, pass.nova, discovery); err != nil {
		return "", err
	}
	switch err := c.Create(ctx, discovery); {
	case apierrors.IsAlreadyExists(err):
		// Another pool of the Nova created it, or this pass read a cache that
		// had not seen it yet.
	case err != nil:
		return "", fmt.Errorf("creating discovery Job %s: %w", key, err)
	default:
		r.Recorder.Eventf(cr, corev1.EventTypeNormal, eventReasonHostDiscoveryStarted,
			"Started Job %s to map %s into the cell", key, strings.Join(hosts, ", "))
	}
	return fmt.Sprintf("started discovery Job %s", key), nil
}
