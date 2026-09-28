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
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// reconcileVPA ensures the VerticalPodAutoscalers of the Nova Deployments match
// their verticalAutoscaling blocks, via the shared VPA flow.
func (r *NovaReconciler) reconcileVPA(ctx context.Context, children client.Client, nova *novav1alpha1.Nova) (ctrl.Result, error) {
	return deployment.ReconcileVPAs(ctx, children, r.Scheme, nova, deployment.VPAFlowParams{
		Targets:        novaVPATargets(nova),
		Namespace:      nova.Namespace,
		Labels:         commonLabels(nova),
		LocalAvailable: r.vpaAvailable,
		Conditions:     &nova.Status.Conditions,
		Generation:     nova.Generation,
		ConditionType:  "VPAReady",
	})
}

// novaVPATargets lists the long-running workloads of a Nova: the API,
// metadata, scheduler and conductor Deployments, each fed by the deployment
// block of its component, and the console proxy Deployment while the proxy is
// enabled.
func novaVPATargets(nova *novav1alpha1.Nova) []deployment.VPATarget {
	targets := []deployment.VPATarget{
		{Kind: "Deployment", Name: nova.Name, Spec: nova.Spec.API.Deployment.VerticalAutoscaling},
		{Kind: "Deployment", Name: metadataName(nova), Spec: nova.Spec.Metadata.Deployment.VerticalAutoscaling},
		{Kind: "Deployment", Name: schedulerName(nova), Spec: nova.Spec.Scheduler.Deployment.VerticalAutoscaling},
		{Kind: "Deployment", Name: conductorName(nova), Spec: nova.Spec.Conductor.Deployment.VerticalAutoscaling},
	}
	if nova.Spec.ConsoleProxyEnabled() {
		targets = append(targets, deployment.VPATarget{
			Kind: "Deployment", Name: consoleProxyName(nova), Spec: consoleProxyDeploymentSpec(nova).VerticalAutoscaling,
		})
	}
	return targets
}

// reconcileVPA ensures the VerticalPodAutoscaler of the pool's DaemonSet
// matches spec.verticalAutoscaling, via the shared VPA flow.
func (r *NovaComputeReconciler) reconcileVPA(ctx context.Context, children client.Client, cr *novav1alpha1.NovaCompute,
	pass *novaComputePass,
) (ctrl.Result, error) {
	return deployment.ReconcileVPAs(ctx, children, r.Scheme, cr, deployment.VPAFlowParams{
		Targets:        novaComputeVPATargets(cr, pass),
		Namespace:      cr.Namespace,
		Labels:         naming.CommonLabels(novaComputeAppName, cr.Name),
		LocalAvailable: r.vpaAvailable,
		Conditions:     &cr.Status.Conditions,
		Generation:     cr.Generation,
		ConditionType:  "VPAReady",
	})
}

// novaComputeVPATargets lists the one long-running workload of a NovaCompute:
// its DaemonSet, fed by spec.verticalAutoscaling. It opts in only when the
// DaemonSet step of the same pass rendered the DaemonSet rather than deleting
// it, which the step records on pass.
func novaComputeVPATargets(cr *novav1alpha1.NovaCompute, pass *novaComputePass) []deployment.VPATarget {
	target := deployment.VPATarget{Kind: "DaemonSet", Name: novaComputeDaemonSetName(cr)}
	if pass.daemonSetRendered {
		target.Spec = cr.Spec.VerticalAutoscaling
	}
	return []deployment.VPATarget{target}
}
