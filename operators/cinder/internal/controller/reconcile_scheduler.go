// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// Condition reason constants for SchedulerReady.
const (
	conditionReasonSchedulerReady      = "SchedulerReady"
	conditionReasonWaitingForScheduler = "WaitingForScheduler"
)

// componentScheduler is the app.kubernetes.io/component value of the scheduler
// pods, and the name suffix of their Deployment.
const componentScheduler = "scheduler"

// cinderSchedulerConfigDir is the second oslo.config --config-dir the scheduler
// loads: it carries scheduler.conf alone, the [DEFAULT] host overlay that puts
// every scheduler pod under one service-registry identity. It is a directory of
// its own rather than a key of the shared config mount because the other three
// processes must not read it.
const cinderSchedulerConfigDir = "/etc/cinder/scheduler.conf.d"

// schedulerConfigVolumeName is the pod volume carrying that overlay.
const schedulerConfigVolumeName = "scheduler-config"

// amqpPortEnvName is the environment variable the readiness probe reads the
// broker port from. images/cinder ships the probe as cinder-amqp-ready and
// defaults it to 5672; the operator passes the port of the transport URL the
// messaging step resolved, so a broker on a non-default port still answers.
const amqpPortEnvName = "CINDER_AMQP_PORT"

// amqpReadinessProbePath is the probe the three bus processes run. None of them
// serves an HTTP port, so readiness is "a process of this container holds a
// socket established to the broker port".
const amqpReadinessProbePath = "/var/lib/openstack/bin/cinder-amqp-ready"

// schedulerName returns the name of the scheduler Deployment. It is also the
// [DEFAULT] host the scheduler overlay renders, so the two must agree: the
// service registry keys the scheduler's entry by it.
func schedulerName(cinder *cinderv1alpha1.Cinder) string {
	return cinder.Name + "-" + componentScheduler
}

// reconcileScheduler ensures the cinder-scheduler Deployment exists with the
// correct spec and sets the SchedulerReady condition.
//
// Outside an upgrade it returns a zero result whatever the readiness is: the API
// step runs after it and must not be skipped over a scheduler that is still
// starting, and the controller's Owns(Deployment) watch re-enqueues the CR when
// the rollout progresses. During the RollingUpdate phase it requeues until the
// rollout has converged, so the contract phase cannot run against a scheduler
// still on the old image.
func (r *CinderReconciler) reconcileScheduler(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder, art configArtifacts, digests workloadDigests, egressPort int32,
) (ctrl.Result, error) {
	deploy := buildSchedulerDeployment(cinder, art, digests, egressPort)
	ready, err := deployment.EnsureDeployment(ctx, children, r.Scheme, cinder, deploy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring scheduler Deployment: %w", err)
	}

	if cinder.Status.UpgradePhase == commonv1.UpgradePhaseRollingUpdate &&
		!cinderDeploymentRolledOut(deploy) {
		conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
			Type:               "SchedulerReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cinder.Generation,
			Reason:             conditionReasonWaitingForScheduler,
			Message:            "Waiting for the upgraded image to finish rolling out on the scheduler",
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, nil
	}

	if !ready {
		conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
			Type:               "SchedulerReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cinder.Generation,
			Reason:             conditionReasonWaitingForScheduler,
			Message:            "Cinder scheduler deployment is not yet available",
		})
		return ctrl.Result{}, nil
	}

	conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
		Type:               "SchedulerReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cinder.Generation,
		Reason:             conditionReasonSchedulerReady,
		Message:            "Cinder scheduler deployment is available",
	})
	return ctrl.Result{}, nil
}

// buildSchedulerDeployment constructs the desired cinder-scheduler Deployment.
// It carries the workload volumes every process shares plus the scheduler
// overlay, and it takes its work off the message bus rather than off HTTP: no
// ports, no Service, no PodDisruptionBudget and no autoscaling.
func buildSchedulerDeployment(cinder *cinderv1alpha1.Cinder, art configArtifacts,
	digests workloadDigests, egressPort int32,
) *appsv1.Deployment {
	volumes, mounts := cinderWorkloadVolumes(cinder, art)
	volumes = append(volumes, corev1.Volume{
		Name: schedulerConfigVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: art.configMapName},
				Items: []corev1.KeyToPath{
					{Key: schedulerConfDataKey, Path: schedulerConfDataKey},
				},
			},
		},
	})
	mounts = append(mounts, corev1.VolumeMount{
		Name:      schedulerConfigVolumeName,
		MountPath: cinderSchedulerConfigDir,
		ReadOnly:  true,
	})

	return deployment.BuildWorkload(deployment.WorkloadParams{
		Namespace:      cinder.Namespace,
		Name:           schedulerName(cinder),
		Labels:         componentLabels(cinder, componentScheduler),
		SelectorLabels: componentSelectorLabels(cinder, componentScheduler),
		PodAnnotations: cinderRPCPodAnnotations(cinder, digests),
		Deployment:     &cinder.Spec.Scheduler.Deployment,
		// No HorizontalPodAutoscaler targets the scheduler: it has no request rate
		// to scale against, and spec.scheduler.deployment.replicas is the only
		// owner of the count.
		Autoscaling: nil,
		Container: deployment.ContainerParams{
			Name:  componentScheduler,
			Image: cinder.Spec.Image.Reference(),
			Command: []string{
				"cinder-scheduler",
				"--config-dir", cinderConfigDir,
				"--config-dir", cinderSchedulerConfigDir,
			},
			Env: append(cinderWorkloadEnv(cinder), amqpPortEnv(egressPort)),
			// Readiness alone. A liveness probe on the same signal would restart a
			// scheduler for a broker outage it did not cause and cannot fix, and the
			// restart would drop the RPC calls in flight; readiness is enough to keep
			// a disconnected pod from being reported as a working one.
			ReadinessProbe: amqpReadinessProbe(),
			VolumeMounts:   mounts,
		},
		Volumes: volumes,
	})
}

// amqpReadinessProbe returns the readiness probe of the three processes that
// serve no HTTP port. cinder-amqp-ready reports whether a process of this
// container holds a socket established to the broker port; it needs no
// capability and no writable filesystem.
func amqpReadinessProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: []string{amqpReadinessProbePath}},
		},
		PeriodSeconds:    5,
		FailureThreshold: 2,
		TimeoutSeconds:   5,
	}
}

// amqpPortEnv returns the broker port the readiness probe checks for. The port
// travels in the environment rather than in the probe's argv so the value stays
// out of the Unhealthy event the kubelet copies a probe's output into.
func amqpPortEnv(egressPort int32) corev1.EnvVar {
	return corev1.EnvVar{Name: amqpPortEnvName, Value: strconv.Itoa(int(egressPort))}
}
