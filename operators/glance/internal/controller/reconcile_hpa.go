// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/c5c3/forge/internal/common/deployment"
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

// reconcileHPA ensures the HorizontalPodAutoscaler for the Glance API deployment
// matches the desired state, via the shared HPA flow. It keeps only the
// service-specific desired HPA builder.
func (r *GlanceReconciler) reconcileHPA(ctx context.Context, glance *glancev1alpha1.Glance) (ctrl.Result, error) {
	var desired *autoscalingv2.HorizontalPodAutoscaler
	if glance.Spec.Autoscaling != nil {
		desired = buildGlanceHPA(glance)
	}
	return deployment.ReconcileHPA(ctx, r.Client, r.Scheme, glance, deployment.HPAFlowParams{
		Enabled:       glance.Spec.Autoscaling != nil,
		Desired:       desired,
		Name:          subResourceName(glance),
		Namespace:     glance.Namespace,
		Conditions:    &glance.Status.Conditions,
		Generation:    glance.Generation,
		ConditionType: "HPAReady",
	})
}

// buildGlanceHPA constructs the desired HorizontalPodAutoscaler for the Glance
// API deployment, delegating to the shared builder. MinReplicas defaults to the
// effective spec.deployment.replicas when autoscaling.minReplicas is not set.
func buildGlanceHPA(glance *glancev1alpha1.Glance) *autoscalingv2.HorizontalPodAutoscaler {
	return deployment.BuildHPA(glance.Namespace, subResourceName(glance), commonLabels(glance), &glance.Spec.Deployment, glance.Spec.Autoscaling)
}
