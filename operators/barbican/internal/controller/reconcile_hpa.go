// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/c5c3/forge/internal/common/deployment"
	barbicanv1alpha1 "github.com/c5c3/forge/operators/barbican/api/v1alpha1"
)

// reconcileHPA ensures the HorizontalPodAutoscaler for the Barbican API
// deployment matches the desired state, via the shared HPA flow. It keeps only
// the service-specific desired HPA builder.
func (r *BarbicanReconciler) reconcileHPA(ctx context.Context, barbican *barbicanv1alpha1.Barbican) (ctrl.Result, error) {
	var desired *autoscalingv2.HorizontalPodAutoscaler
	if barbican.Spec.Autoscaling != nil {
		desired = buildBarbicanHPA(barbican)
	}
	return deployment.ReconcileHPA(ctx, r.Client, r.Scheme, barbican, deployment.HPAFlowParams{
		Enabled:       barbican.Spec.Autoscaling != nil,
		Desired:       desired,
		Name:          subResourceName(barbican),
		Namespace:     barbican.Namespace,
		Conditions:    &barbican.Status.Conditions,
		Generation:    barbican.Generation,
		ConditionType: "HPAReady",
	})
}

// buildBarbicanHPA constructs the desired HorizontalPodAutoscaler for the
// Barbican API deployment, delegating to the shared builder. MinReplicas
// defaults to the effective spec.deployment.replicas when
// autoscaling.minReplicas is not set.
func buildBarbicanHPA(barbican *barbicanv1alpha1.Barbican) *autoscalingv2.HorizontalPodAutoscaler {
	return deployment.BuildHPA(barbican.Namespace, subResourceName(barbican), commonLabels(barbican), &barbican.Spec.Deployment, barbican.Spec.Autoscaling)
}
