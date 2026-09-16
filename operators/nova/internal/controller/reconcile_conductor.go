// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// Condition reason constants for ConductorReady.
const (
	conditionReasonConductorReady      = "ConductorReady"
	conditionReasonWaitingForConductor = "WaitingForConductor"
)

// componentConductor is the app.kubernetes.io/component value of the conductor
// pods, and the name suffix of their Deployment.
const componentConductor = "conductor"

// conductorName returns the name of the conductor Deployment.
func conductorName(nova *novav1alpha1.Nova) string {
	return nova.Name + "-" + componentConductor
}

// reconcileConductor projects the nova-conductor Deployment and sets the
// ConductorReady condition.
//
// The conductor is the only process that writes the cell schema on behalf of the
// compute nodes, so it runs in the control plane beside the scheduler. It
// follows the same result contract: a zero result outside an upgrade whatever
// the readiness is, and polling during the RollingUpdate phase until the rollout
// has converged.
func (r *NovaReconciler) reconcileConductor(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova, art configArtifacts, digests workloadDigests, egressPort int32,
) (ctrl.Result, error) {
	deploy := buildConductorDeployment(nova, art, digests, egressPort)
	ready, err := deployment.EnsureDeployment(ctx, children, r.Scheme, nova, deploy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring conductor Deployment: %w", err)
	}

	if nova.Status.UpgradePhase == commonv1.UpgradePhaseRollingUpdate &&
		!novaDeploymentRolledOut(deploy) {
		conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
			Type:               "ConductorReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: nova.Generation,
			Reason:             conditionReasonWaitingForConductor,
			Message:            "Waiting for the upgraded image to finish rolling out on the conductor",
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, nil
	}

	if !ready {
		conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
			Type:               "ConductorReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: nova.Generation,
			Reason:             conditionReasonWaitingForConductor,
			Message:            "Nova conductor deployment is not yet available",
		})
		return ctrl.Result{}, nil
	}

	conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
		Type:               "ConductorReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: nova.Generation,
		Reason:             conditionReasonConductorReady,
		Message:            "Nova conductor deployment is available",
	})
	return ctrl.Result{}, nil
}

// buildConductorDeployment constructs the desired nova-conductor Deployment, the
// scheduler's twin: the shared workload volumes plus its own overlay directory,
// the same bus readiness probe, and no HTTP surface of any kind.
func buildConductorDeployment(nova *novav1alpha1.Nova, art configArtifacts,
	digests workloadDigests, egressPort int32,
) *appsv1.Deployment {
	volumes, mounts := novaWorkloadVolumes(nova, art, roleConductor)
	return deployment.BuildWorkload(deployment.WorkloadParams{
		Namespace:      nova.Namespace,
		Name:           conductorName(nova),
		Labels:         componentLabels(nova, componentConductor),
		SelectorLabels: componentSelectorLabels(nova, componentConductor),
		PodAnnotations: novaRPCPodAnnotations(nova, digests),
		Deployment:     &nova.Spec.Conductor.Deployment,
		Autoscaling:    nil,
		Container: deployment.ContainerParams{
			Name:  componentConductor,
			Image: nova.Spec.Image.Reference(),
			Command: []string{
				"nova-conductor",
				"--config-dir", novaConfigDir,
				"--config-dir", roleOverlayDir(roleConductor),
			},
			Env:            append(novaWorkloadEnv(nova, roleConductor), amqpPortEnv(egressPort)),
			ReadinessProbe: amqpReadinessProbe(),
			VolumeMounts:   mounts,
		},
		Volumes: volumes,
	})
}
