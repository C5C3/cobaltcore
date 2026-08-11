// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/deployment"
	placementv1alpha1 "github.com/c5c3/forge/operators/placement/api/v1alpha1"
)

// reconcileHPA ensures the HorizontalPodAutoscaler for the Placement API
// deployment matches the desired state, via the shared HPA flow. It keeps only
// the service-specific desired HPA builder.
func (r *PlacementReconciler) reconcileHPA(ctx context.Context, children client.Client, placement *placementv1alpha1.Placement) (ctrl.Result, error) {
	var desired *autoscalingv2.HorizontalPodAutoscaler
	if placement.Spec.Autoscaling != nil {
		desired = buildPlacementHPA(placement)
	}
	return deployment.ReconcileHPA(ctx, children, r.Scheme, placement, deployment.HPAFlowParams{
		Enabled:       placement.Spec.Autoscaling != nil,
		Desired:       desired,
		Name:          subResourceName(placement),
		Namespace:     placement.Namespace,
		Conditions:    &placement.Status.Conditions,
		Generation:    placement.Generation,
		ConditionType: "HPAReady",
	})
}

// buildPlacementHPA constructs the desired HorizontalPodAutoscaler for the
// Placement API deployment, delegating to the shared builder. MinReplicas
// defaults to the effective spec.deployment.replicas when
// autoscaling.minReplicas is not set.
func buildPlacementHPA(placement *placementv1alpha1.Placement) *autoscalingv2.HorizontalPodAutoscaler {
	return deployment.BuildHPA(placement.Namespace, subResourceName(placement), commonLabels(placement), &placement.Spec.Deployment, placement.Spec.Autoscaling)
}
