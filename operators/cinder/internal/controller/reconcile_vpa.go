// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// reconcileVPA ensures the VerticalPodAutoscalers of the Cinder Deployments
// match their verticalAutoscaling blocks, via the shared VPA flow. backends and
// backup are the projections this pass rendered, so a detached backend or a
// removed backup target loses its VPA with its Deployment.
func (r *CinderReconciler) reconcileVPA(ctx context.Context, children client.Client, cinder *cinderv1alpha1.Cinder,
	backends []backendProjection, backup *backupProjection,
) (ctrl.Result, error) {
	return deployment.ReconcileVPAs(ctx, children, r.Scheme, cinder, deployment.VPAFlowParams{
		Targets:        cinderVPATargets(cinder, backends, backup),
		Namespace:      cinder.Namespace,
		Labels:         commonLabels(cinder),
		LocalAvailable: r.vpaAvailable,
		Conditions:     &cinder.Status.Conditions,
		Generation:     cinder.Generation,
		ConditionType:  "VPAReady",
	})
}

// cinderVPATargets lists the long-running workloads of a Cinder: the API and
// scheduler Deployments, one volume Deployment per projected backend (all fed
// by spec.volume.deployment), and the backup Deployment while a backup target
// is projected.
func cinderVPATargets(cinder *cinderv1alpha1.Cinder, backends []backendProjection, backup *backupProjection) []deployment.VPATarget {
	targets := []deployment.VPATarget{
		{Kind: "Deployment", Name: cinder.Name, Spec: cinder.Spec.API.Deployment.VerticalAutoscaling},
		{Kind: "Deployment", Name: schedulerName(cinder), Spec: cinder.Spec.Scheduler.Deployment.VerticalAutoscaling},
	}
	for _, backend := range backends {
		targets = append(targets, deployment.VPATarget{
			Kind: "Deployment", Name: volumeDeploymentName(cinder, backend.name), Spec: cinder.Spec.Volume.Deployment.VerticalAutoscaling,
		})
	}
	if backup != nil {
		targets = append(targets, deployment.VPATarget{
			Kind: "Deployment", Name: backupDeploymentName(cinder), Spec: cinder.Spec.Backup.Deployment.VerticalAutoscaling,
		})
	}
	return targets
}
