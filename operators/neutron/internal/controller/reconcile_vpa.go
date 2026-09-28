// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// reconcileVPA ensures the VerticalPodAutoscalers of the Neutron Deployments
// match their verticalAutoscaling blocks, via the shared VPA flow.
func (r *NeutronReconciler) reconcileVPA(ctx context.Context, children client.Client, neutron *neutronv1alpha1.Neutron) (ctrl.Result, error) {
	return deployment.ReconcileVPAs(ctx, children, r.Scheme, neutron, deployment.VPAFlowParams{
		Targets:        neutronVPATargets(neutron),
		Namespace:      neutron.Namespace,
		Labels:         commonLabels(neutron),
		LocalAvailable: r.vpaAvailable,
		Conditions:     &neutron.Status.Conditions,
		Generation:     neutron.Generation,
		ConditionType:  "VPAReady",
	})
}

// neutronVPATargets lists the long-running workloads of a Neutron: the API
// Deployment, fed by spec.deployment, and the two worker Deployments, both
// fed by spec.workers.deployment.
func neutronVPATargets(neutron *neutronv1alpha1.Neutron) []deployment.VPATarget {
	targets := []deployment.VPATarget{
		{Kind: "Deployment", Name: neutron.Name, Spec: neutron.Spec.Deployment.VerticalAutoscaling},
	}
	for _, worker := range neutronWorkers {
		targets = append(targets, deployment.VPATarget{
			Kind: "Deployment", Name: workerDeploymentName(neutron, worker.component),
			Spec: neutron.Spec.Workers.Deployment.VerticalAutoscaling,
		})
	}
	return targets
}

// reconcileVPA ensures the VerticalPodAutoscaler of the agent DaemonSet
// matches spec.verticalAutoscaling, via the shared VPA flow.
func (r *NeutronMetadataAgentReconciler) reconcileVPA(ctx context.Context, children client.Client,
	cr *neutronv1alpha1.NeutronMetadataAgent,
) (ctrl.Result, error) {
	return deployment.ReconcileVPAs(ctx, children, r.Scheme, cr, deployment.VPAFlowParams{
		Targets:        agentVPATargets(cr),
		Namespace:      cr.Namespace,
		Labels:         naming.CommonLabels(metadataAgentAppName, cr.Name),
		LocalAvailable: r.vpaAvailable,
		Conditions:     &cr.Status.Conditions,
		Generation:     cr.Generation,
		ConditionType:  "VPAReady",
	})
}

// agentVPATargets lists the one long-running workload of a
// NeutronMetadataAgent: its DaemonSet, fed by spec.verticalAutoscaling.
func agentVPATargets(cr *neutronv1alpha1.NeutronMetadataAgent) []deployment.VPATarget {
	return []deployment.VPATarget{
		{Kind: "DaemonSet", Name: agentDaemonSetName(cr), Spec: cr.Spec.VerticalAutoscaling},
	}
}
