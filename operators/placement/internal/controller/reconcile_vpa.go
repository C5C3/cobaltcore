// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	placementv1alpha1 "github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1"
)

// reconcileVPA ensures the VerticalPodAutoscaler of the Placement API
// Deployment matches spec.deployment.verticalAutoscaling, via the shared VPA
// flow.
func (r *PlacementReconciler) reconcileVPA(ctx context.Context, children client.Client, placement *placementv1alpha1.Placement) (ctrl.Result, error) {
	return deployment.ReconcileVPAs(ctx, children, r.Scheme, placement, deployment.VPAFlowParams{
		Targets:        placementVPATargets(placement),
		Namespace:      placement.Namespace,
		Labels:         commonLabels(placement),
		LocalAvailable: r.vpaAvailable,
		Conditions:     &placement.Status.Conditions,
		Generation:     placement.Generation,
		ConditionType:  "VPAReady",
	})
}

// placementVPATargets lists the long-running workloads of a Placement: the API
// Deployment, fed by spec.deployment.
func placementVPATargets(placement *placementv1alpha1.Placement) []deployment.VPATarget {
	return []deployment.VPATarget{
		{Kind: "Deployment", Name: subResourceName(placement), Spec: placement.Spec.Deployment.VerticalAutoscaling},
	}
}
