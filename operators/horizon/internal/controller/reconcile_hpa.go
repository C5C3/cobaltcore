// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/deployment"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
)

// reconcileHPA ensures the HorizontalPodAutoscaler for the dashboard
// deployment matches the desired state. Three lifecycle paths:
//   - spec.autoscaling set: create or update HPA targeting the deployment
//   - spec.autoscaling nil: delete any existing HPA
//   - error: propagate errors from ensure/delete operations
func (r *HorizonReconciler) reconcileHPA(ctx context.Context, horizon *horizonv1alpha1.Horizon) (ctrl.Result, error) {
	hpaName := subResourceName(horizon)

	// Path 2: autoscaling disabled — delete any existing HPA.
	if horizon.Spec.Autoscaling == nil {
		if err := deployment.DeleteHPA(ctx, r.Client, horizon.Namespace, hpaName); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting HorizontalPodAutoscaler: %w", err)
		}
		conditions.SetCondition(&horizon.Status.Conditions, metav1.Condition{
			Type:               "HPAReady",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: horizon.Generation,
			Reason:             "HPANotRequired",
			Message:            "Autoscaling is not configured",
		})
		return ctrl.Result{}, nil
	}

	// Path 1: autoscaling enabled — create or update HPA.
	hpa := buildHorizonHPA(horizon)
	if err := deployment.EnsureHPA(ctx, r.Client, r.Scheme, horizon, hpa); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring HorizontalPodAutoscaler: %w", err)
	}
	conditions.SetCondition(&horizon.Status.Conditions, metav1.Condition{
		Type:               "HPAReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: horizon.Generation,
		Reason:             "HPAReady",
		Message:            "HorizontalPodAutoscaler is configured",
	})
	return ctrl.Result{}, nil
}

// buildHorizonHPA constructs the desired HorizontalPodAutoscaler for the
// dashboard deployment, delegating to the shared builder. MinReplicas
// defaults to the effective spec.deployment.replicas when
// autoscaling.minReplicas is not set.
func buildHorizonHPA(horizon *horizonv1alpha1.Horizon) *autoscalingv2.HorizontalPodAutoscaler {
	return deployment.BuildHPA(horizon.Namespace, subResourceName(horizon), commonLabels(horizon), &horizon.Spec.Deployment, horizon.Spec.Autoscaling)
}
