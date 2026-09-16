// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// reconcileHPA ensures the HorizontalPodAutoscaler for the Nova API deployment
// matches the desired state, via the shared HPA flow. It keeps only the
// service-specific desired HPA builder.
//
// Only the API Deployment is autoscaled. The metadata API is sized by the
// instance population rather than by API traffic, and the scheduler, the
// conductor and the console proxy answer no HTTP request rate at all.
func (r *NovaReconciler) reconcileHPA(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova,
) (ctrl.Result, error) {
	var desired *autoscalingv2.HorizontalPodAutoscaler
	if nova.Spec.Autoscaling != nil {
		desired = buildNovaHPA(nova)
	}
	return deployment.ReconcileHPA(ctx, children, r.Scheme, nova, deployment.HPAFlowParams{
		Enabled:       nova.Spec.Autoscaling != nil,
		Desired:       desired,
		Name:          nova.Name,
		Namespace:     nova.Namespace,
		Conditions:    &nova.Status.Conditions,
		Generation:    nova.Generation,
		ConditionType: "HPAReady",
	})
}

// buildNovaHPA constructs the desired HorizontalPodAutoscaler for the Nova API
// deployment, delegating to the shared builder. MinReplicas defaults to the
// effective spec.api.deployment.replicas when autoscaling.minReplicas is not
// set.
func buildNovaHPA(nova *novav1alpha1.Nova) *autoscalingv2.HorizontalPodAutoscaler {
	return deployment.BuildHPA(nova.Namespace, nova.Name, commonLabels(nova),
		&nova.Spec.API.Deployment, nova.Spec.Autoscaling)
}
