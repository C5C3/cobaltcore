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
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// Condition reason constants for ConsoleProxyReady.
const (
	conditionReasonConsoleProxyReady      = "ConsoleProxyReady"
	conditionReasonWaitingForConsoleProxy = "WaitingForConsoleProxy"
	// conditionReasonConsoleProxyDisabled is set while spec.consoleProxy.enabled
	// is false. It is True rather than False: the console is opt-out, and a Nova
	// without it serves instances as before.
	conditionReasonConsoleProxyDisabled = "ConsoleProxyDisabled"
)

// componentConsoleProxy is the app.kubernetes.io/component value of the console
// proxy pods, and the name suffix of their Deployment and Service.
const componentConsoleProxy = "novncproxy"

// consoleProxyVNCPath is the noVNC client page the proxy serves from the
// directory [DEFAULT] web names. It is the readiness probe's target and the last
// path segment of the console URL the API hands a browser.
const consoleProxyVNCPath = "/vnc_lite.html"

// consoleProxyName returns the name of the console proxy Deployment and of the
// Service in front of it. The cluster-local console URL addresses that Service,
// so the name is part of what the API hands a browser.
func consoleProxyName(nova *novav1alpha1.Nova) string {
	return nova.Name + "-" + componentConsoleProxy
}

// reconcileConsoleProxy projects the nova-novncproxy Deployment and the Service
// in front of it, and sets the ConsoleProxyReady condition. A disabled proxy
// deletes both: the rendered config switches [vnc] enabled off in the same pass,
// so an instance is no longer offered a console the deployment does not serve.
//
// While the proxy is enabled it follows the scheduler's result contract: a zero
// result outside an upgrade whatever the readiness is, and polling during the
// RollingUpdate phase until the rollout has converged.
func (r *NovaReconciler) reconcileConsoleProxy(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova, art configArtifacts, digests workloadDigests,
) (ctrl.Result, error) {
	if !nova.Spec.ConsoleProxyEnabled() {
		if err := deleteConsoleProxy(ctx, children, nova); err != nil {
			return ctrl.Result{}, err
		}
		conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
			Type:               "ConsoleProxyReady",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: nova.Generation,
			Reason:             conditionReasonConsoleProxyDisabled,
			Message:            "spec.consoleProxy.enabled is false; the console proxy is not rendered",
		})
		return ctrl.Result{}, nil
	}

	deploy := buildConsoleProxyDeployment(nova, art, digests)
	ready, err := deployment.EnsureDeployment(ctx, children, r.Scheme, nova, deploy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring console proxy Deployment: %w", err)
	}

	svc := buildConsoleProxyService(nova)
	if err := deployment.EnsureService(ctx, children, r.Scheme, nova, svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring console proxy Service: %w", err)
	}

	if nova.Status.UpgradePhase == commonv1.UpgradePhaseRollingUpdate &&
		!novaDeploymentRolledOut(deploy) {
		conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
			Type:               "ConsoleProxyReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: nova.Generation,
			Reason:             conditionReasonWaitingForConsoleProxy,
			Message:            "Waiting for the upgraded image to finish rolling out on the console proxy",
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, nil
	}

	if !ready {
		conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
			Type:               "ConsoleProxyReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: nova.Generation,
			Reason:             conditionReasonWaitingForConsoleProxy,
			Message:            "Nova console proxy deployment is not yet available",
		})
		return ctrl.Result{}, nil
	}

	conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
		Type:               "ConsoleProxyReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: nova.Generation,
		Reason:             conditionReasonConsoleProxyReady,
		Message:            "Nova console proxy deployment is available",
	})
	return ctrl.Result{}, nil
}

// deleteConsoleProxy removes the Deployment and the Service of a proxy that is
// switched off, so a Nova whose console is disabled after it ran keeps no pod
// and no address behind.
func deleteConsoleProxy(ctx context.Context, children client.Client, nova *novav1alpha1.Nova) error {
	name := consoleProxyName(nova)

	stale := &appsv1.Deployment{}
	stale.SetName(name)
	stale.SetNamespace(nova.Namespace)
	if err := client.IgnoreNotFound(children.Delete(ctx, stale)); err != nil {
		return fmt.Errorf("deleting console proxy Deployment %s: %w", name, err)
	}

	staleSvc := &corev1.Service{}
	staleSvc.SetName(name)
	staleSvc.SetNamespace(nova.Namespace)
	if err := client.IgnoreNotFound(children.Delete(ctx, staleSvc)); err != nil {
		return fmt.Errorf("deleting console proxy Service %s: %w", name, err)
	}
	return nil
}

// buildConsoleProxyDeployment constructs the desired nova-novncproxy Deployment:
// the noVNC client and the websocket proxy behind it, reading the shared config
// plus the overlay carrying its listen address.
//
// It is the one process that opens the cell schema alone (it reads the console
// tokens out of it) and needs no nova_api connection, which novaWorkloadEnv
// reflects for roleConsoleProxy.
func buildConsoleProxyDeployment(nova *novav1alpha1.Nova, art configArtifacts,
	digests workloadDigests,
) *appsv1.Deployment {
	volumes, mounts := novaWorkloadVolumes(nova, art, roleConsoleProxy)
	return deployment.BuildWorkload(deployment.WorkloadParams{
		Namespace:      nova.Namespace,
		Name:           consoleProxyName(nova),
		Labels:         componentLabels(nova, componentConsoleProxy),
		SelectorLabels: componentSelectorLabels(nova, componentConsoleProxy),
		PodAnnotations: novaRPCPodAnnotations(nova, digests),
		Deployment:     consoleProxyDeploymentSpec(nova),
		Autoscaling:    nil,
		// The console proxy runs one single-threaded process.
		DefaultMemory: commonv1.MemoryForProcesses(commonv1.DefaultMemoryPerProcess(), 1, 1),
		Container: deployment.ContainerParams{
			Name:  componentConsoleProxy,
			Image: nova.Spec.Image.Reference(),
			Command: []string{
				"nova-novncproxy",
				"--config-dir", novaConfigDir,
				"--config-dir", roleOverlayDir(roleConsoleProxy),
			},
			Env: novaWorkloadEnv(nova, roleConsoleProxy),
			Ports: []corev1.ContainerPort{{
				Name:          componentConsoleProxy,
				ContainerPort: novaConsolePort,
			}},
			// Readiness alone, on the client page the proxy serves itself. A
			// liveness probe would restart a proxy over a page a browser can retry,
			// and the restart would cut every console session in flight.
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path: consoleProxyVNCPath,
						Port: intstr.FromInt32(novaConsolePort),
					},
				},
				InitialDelaySeconds: 10,
				PeriodSeconds:       15,
				TimeoutSeconds:      10,
				FailureThreshold:    3,
			},
			VolumeMounts: mounts,
		},
		Volumes: volumes,
	})
}

// consoleProxyDeploymentSpec resolves the pod-level knob block of the proxy. The
// block is a pointer the defaulting webhook only materializes while the proxy is
// enabled, so a CR that bypassed admission is sized like the webhook would have
// sized it: one replica, every other knob at the shared default.
func consoleProxyDeploymentSpec(nova *novav1alpha1.Nova) *commonv1.DeploymentSpec {
	if nova.Spec.ConsoleProxy.Deployment != nil {
		return nova.Spec.ConsoleProxy.Deployment
	}
	return &commonv1.DeploymentSpec{Replicas: 1}
}

// buildConsoleProxyService builds the Service the console URL addresses,
// selecting the proxy component so only the proxy pods can become its endpoints.
func buildConsoleProxyService(nova *novav1alpha1.Nova) *corev1.Service {
	return deployment.BuildService(nova.Namespace, consoleProxyName(nova), commonLabels(nova),
		componentSelectorLabels(nova, componentConsoleProxy), novaConsolePort, novaConsolePort)
}
