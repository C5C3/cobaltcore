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
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// reconcileHPA ensures the HorizontalPodAutoscaler for the Cinder API deployment
// matches the desired state, via the shared HPA flow. It keeps only the
// service-specific desired HPA builder.
//
// Only the API Deployment is autoscaled. The scheduler is not sized by request
// rate, and the volume and backup Deployments are pinned at one replica: each
// owns its storage through a host identity a second pod would compete for.
func (r *CinderReconciler) reconcileHPA(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder,
) (ctrl.Result, error) {
	var desired *autoscalingv2.HorizontalPodAutoscaler
	if cinder.Spec.Autoscaling != nil {
		desired = buildCinderHPA(cinder)
	}
	return deployment.ReconcileHPA(ctx, children, r.Scheme, cinder, deployment.HPAFlowParams{
		Enabled:       cinder.Spec.Autoscaling != nil,
		Desired:       desired,
		Name:          cinder.Name,
		Namespace:     cinder.Namespace,
		Conditions:    &cinder.Status.Conditions,
		Generation:    cinder.Generation,
		ConditionType: "HPAReady",
	})
}

// buildCinderHPA constructs the desired HorizontalPodAutoscaler for the Cinder
// API deployment, delegating to the shared builder. MinReplicas defaults to the
// effective spec.api.deployment.replicas when autoscaling.minReplicas is not
// set.
func buildCinderHPA(cinder *cinderv1alpha1.Cinder) *autoscalingv2.HorizontalPodAutoscaler {
	return deployment.BuildHPA(cinder.Namespace, cinder.Name, commonLabels(cinder),
		&cinder.Spec.API.Deployment, cinder.Spec.Autoscaling)
}
