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
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// Condition reason constants for SchedulerReady.
const (
	conditionReasonSchedulerReady      = "SchedulerReady"
	conditionReasonWaitingForScheduler = "WaitingForScheduler"
)

// componentScheduler is the app.kubernetes.io/component value of the scheduler
// pods, and the name suffix of their Deployment.
const componentScheduler = "scheduler"

// amqpPortEnvName is the environment variable the readiness probe reads the
// broker port from. images/nova ships the probe as nova-amqp-ready and defaults
// it to 5672; the operator passes the port of the transport URL the messaging
// step resolved, so a broker on a non-default port still answers.
const amqpPortEnvName = "NOVA_AMQP_PORT"

// amqpReadinessProbePath is the probe the bus processes run. Neither the
// scheduler nor the conductor serves an HTTP port, so readiness is "a process of
// this container holds a socket established to the broker port".
const amqpReadinessProbePath = "/var/lib/openstack/bin/nova-amqp-ready"

// schedulerName returns the name of the scheduler Deployment.
func schedulerName(nova *novav1alpha1.Nova) string {
	return nova.Name + "-" + componentScheduler
}

// reconcileScheduler projects the nova-scheduler Deployment and sets the
// SchedulerReady condition.
//
// It follows the metadata step's result contract: a zero result outside an
// upgrade whatever the readiness is, and polling during the RollingUpdate phase
// until the rollout has converged.
func (r *NovaReconciler) reconcileScheduler(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova, art configArtifacts, digests workloadDigests, egressPort int32,
) (ctrl.Result, error) {
	deploy := buildSchedulerDeployment(nova, art, digests, egressPort)
	ready, err := deployment.EnsureDeployment(ctx, children, r.Scheme, nova, deploy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring scheduler Deployment: %w", err)
	}

	if nova.Status.UpgradePhase == commonv1.UpgradePhaseRollingUpdate &&
		!novaDeploymentRolledOut(deploy) {
		conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
			Type:               "SchedulerReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: nova.Generation,
			Reason:             conditionReasonWaitingForScheduler,
			Message:            "Waiting for the upgraded image to finish rolling out on the scheduler",
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, nil
	}

	if !ready {
		conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
			Type:               "SchedulerReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: nova.Generation,
			Reason:             conditionReasonWaitingForScheduler,
			Message:            "Nova scheduler deployment is not yet available",
		})
		return ctrl.Result{}, nil
	}

	conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
		Type:               "SchedulerReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: nova.Generation,
		Reason:             conditionReasonSchedulerReady,
		Message:            "Nova scheduler deployment is available",
	})
	return ctrl.Result{}, nil
}

// buildSchedulerDeployment constructs the desired nova-scheduler Deployment. It
// carries the workload volumes every process shares plus its own overlay
// directory, and it takes its work off the message bus rather than off HTTP: no
// ports, no Service, no PodDisruptionBudget and no autoscaling.
//
// No replica is given a [DEFAULT] host of its own. Each registers under its pod
// name, which is what lets the replicas hold separate service records and elect
// a leader for the periodic host discovery among themselves; one shared identity
// would collapse the fleet into a single record.
func buildSchedulerDeployment(nova *novav1alpha1.Nova, art configArtifacts,
	digests workloadDigests, egressPort int32,
) *appsv1.Deployment {
	volumes, mounts := novaWorkloadVolumes(nova, art, roleScheduler)
	return deployment.BuildWorkload(deployment.WorkloadParams{
		Namespace:      nova.Namespace,
		Name:           schedulerName(nova),
		Labels:         componentLabels(nova, componentScheduler),
		SelectorLabels: componentSelectorLabels(nova, componentScheduler),
		PodAnnotations: novaRPCPodAnnotations(nova, digests),
		Deployment:     &nova.Spec.Scheduler.Deployment,
		// No HorizontalPodAutoscaler targets the scheduler: it has no request rate
		// to scale against, and spec.scheduler.deployment.replicas is the only
		// owner of the count.
		Autoscaling: nil,
		Container: deployment.ContainerParams{
			Name:  componentScheduler,
			Image: nova.Spec.Image.Reference(),
			Command: []string{
				"nova-scheduler",
				"--config-dir", novaConfigDir,
				"--config-dir", roleOverlayDir(roleScheduler),
			},
			Env: append(novaWorkloadEnv(nova, roleScheduler), amqpPortEnv(egressPort)),
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

// amqpReadinessProbe returns the readiness probe of the processes that serve no
// HTTP port. nova-amqp-ready reports whether a process of this container holds a
// socket established to the broker port; it needs no capability and no writable
// filesystem.
func amqpReadinessProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: []string{amqpReadinessProbePath}},
		},
		PeriodSeconds:    5,
		FailureThreshold: 1,
		TimeoutSeconds:   5,
	}
}

// amqpPortEnv returns the broker port the readiness probe checks for. The port
// travels in the environment rather than in the probe's argv so the value stays
// out of the Unhealthy event the kubelet copies a probe's output into.
func amqpPortEnv(egressPort int32) corev1.EnvVar {
	return corev1.EnvVar{Name: amqpPortEnvName, Value: strconv.Itoa(int(egressPort))}
}
