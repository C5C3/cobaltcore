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
	"github.com/c5c3/forge/internal/common/naming"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// subResourceName returns the canonical name for Keystone operator-managed
// sub-resources (Deployment, HPA, Service, PodDisruptionBudget, NetworkPolicy,
// HTTPRoute). It delegates to the shared naming package
// (internal/common/naming), the single source of truth for the sub-resource
// naming convention across operators — the bare CR name with no suffix.
func subResourceName(keystone *keystonev1alpha1.Keystone) string {
	return naming.SubResourceName(keystone.Name)
}

// reconcileHPA ensures the HorizontalPodAutoscaler for the Keystone API
// deployment matches the desired state. Three lifecycle paths:
//   - spec.autoscaling set: create or update HPA targeting the deployment
//   - spec.autoscaling nil: delete any existing HPA
//   - error: propagate errors from ensure/delete operations
func (r *KeystoneReconciler) reconcileHPA(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	hpaName := subResourceName(keystone)

	// Path 2: autoscaling disabled — delete any existing HPA.
	if keystone.Spec.Autoscaling == nil {
		if err := deployment.DeleteHPA(ctx, r.Client, keystone.Namespace, hpaName); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting HorizontalPodAutoscaler: %w", err)
		}
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               "HPAReady",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason:             "HPANotRequired",
			Message:            "Autoscaling is not configured",
		})
		return ctrl.Result{}, nil
	}

	// Path 1: autoscaling enabled — create or update HPA.
	hpa := buildKeystoneHPA(keystone)
	if err := deployment.EnsureHPA(ctx, r.Client, r.Scheme, keystone, hpa); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring HorizontalPodAutoscaler: %w", err)
	}
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               "HPAReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: keystone.Generation,
		Reason:             "HPAReady",
		Message:            "HorizontalPodAutoscaler is configured",
	})
	return ctrl.Result{}, nil
}

// buildKeystoneHPA constructs the desired HorizontalPodAutoscaler for the
// Keystone API deployment. MinReplicas defaults to the effective
// spec.deployment.replicas when autoscaling.minReplicas is not set — routing
// through deployment.EffectiveReplicas normalizes a zero-valued (webhook-bypassed) count to
// the default, so a bypassed spec never yields an invalid minReplicas=0 the API
// server would reject. Metrics are added for CPU and/or memory utilization based
// on the autoscaling spec.
func buildKeystoneHPA(keystone *keystonev1alpha1.Keystone) *autoscalingv2.HorizontalPodAutoscaler {
	return deployment.BuildHPA(keystone.Namespace, subResourceName(keystone), commonLabels(keystone), &keystone.Spec.Deployment, keystone.Spec.Autoscaling)
}
