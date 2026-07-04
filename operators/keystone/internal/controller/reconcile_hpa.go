// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/deployment"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// subResourceName returns the canonical name for Keystone operator-managed
// sub-resources (Deployment, HPA, Service, PodDisruptionBudget, NetworkPolicy,
// HTTPRoute). Centralised here so the naming convention is defined in one
// place. The helper returns the bare CR name with no suffix — the
// historical `-api` suffix was dropped to align internal Service DNS with
// the public hostname posture. The name is
// deliberately neutral (not `apiResourceName`) so future readers do not
// expect an `-api` suffix or otherwise infer API-specific semantics
func subResourceName(keystone *keystonev1alpha1.Keystone) string {
	return keystone.Name
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
// through effectiveReplicas normalizes a zero-valued (webhook-bypassed) count to
// the default, so a bypassed spec never yields an invalid minReplicas=0 the API
// server would reject. Metrics are added for CPU and/or memory utilization based
// on the autoscaling spec.
func buildKeystoneHPA(keystone *keystonev1alpha1.Keystone) *autoscalingv2.HorizontalPodAutoscaler {
	autoscaling := keystone.Spec.Autoscaling

	minReplicas := autoscaling.MinReplicas
	if minReplicas == nil {
		defaultMin := effectiveReplicas(keystone)
		minReplicas = &defaultMin
	}

	name := subResourceName(keystone)
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: keystone.Namespace,
			Labels:    commonLabels(keystone),
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       name,
			},
			MinReplicas: minReplicas,
			MaxReplicas: autoscaling.MaxReplicas,
		},
	}

	if autoscaling.TargetCPUUtilization != nil {
		hpa.Spec.Metrics = append(hpa.Spec.Metrics, autoscalingv2.MetricSpec{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name: corev1.ResourceCPU,
				Target: autoscalingv2.MetricTarget{
					Type:               autoscalingv2.UtilizationMetricType,
					AverageUtilization: autoscaling.TargetCPUUtilization,
				},
			},
		})
	}

	if autoscaling.TargetMemoryUtilization != nil {
		hpa.Spec.Metrics = append(hpa.Spec.Metrics, autoscalingv2.MetricSpec{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name: corev1.ResourceMemory,
				Target: autoscalingv2.MetricTarget{
					Type:               autoscalingv2.UtilizationMetricType,
					AverageUtilization: autoscaling.TargetMemoryUtilization,
				},
			},
		})
	}

	return hpa
}
