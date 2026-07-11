// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/c5c3/forge/internal/common/deployment"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
)

// reconcileHPA ensures the HorizontalPodAutoscaler for the dashboard deployment
// matches the desired state, via the shared HPA flow. It keeps only the
// service-specific desired HPA builder.
func (r *HorizonReconciler) reconcileHPA(ctx context.Context, horizon *horizonv1alpha1.Horizon) (ctrl.Result, error) {
	var desired *autoscalingv2.HorizontalPodAutoscaler
	if horizon.Spec.Autoscaling != nil {
		desired = buildHorizonHPA(horizon)
	}
	return deployment.ReconcileHPA(ctx, r.Client, r.Scheme, horizon, deployment.HPAFlowParams{
		Enabled:       horizon.Spec.Autoscaling != nil,
		Desired:       desired,
		Name:          subResourceName(horizon),
		Namespace:     horizon.Namespace,
		Conditions:    &horizon.Status.Conditions,
		Generation:    horizon.Generation,
		ConditionType: "HPAReady",
	})
}

// buildHorizonHPA constructs the desired HorizontalPodAutoscaler for the
// dashboard deployment, delegating to the shared builder. MinReplicas
// defaults to the effective spec.deployment.replicas when
// autoscaling.minReplicas is not set.
func buildHorizonHPA(horizon *horizonv1alpha1.Horizon) *autoscalingv2.HorizontalPodAutoscaler {
	return deployment.BuildHPA(horizon.Namespace, subResourceName(horizon), commonLabels(horizon), &horizon.Spec.Deployment, horizon.Spec.Autoscaling)
}
