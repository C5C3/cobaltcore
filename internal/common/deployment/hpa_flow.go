// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/conditions"
)

// Condition reason constants for the HPA readiness condition, shared so every
// operator's condition uses the same vocabulary.
const (
	ReasonHPAReady       = "HPAReady"
	ReasonHPANotRequired = "HPANotRequired"
)

// HPAFlowParams carries everything ReconcileHPA needs. The service-specific
// parts — whether autoscaling is enabled, the built desired HPA, the identity,
// and the condition type — are supplied by the caller; the two-path flow itself
// is identical across operators.
type HPAFlowParams struct {
	// Enabled reports whether spec.autoscaling is set.
	Enabled bool
	// Desired is the built HorizontalPodAutoscaler, applied when Enabled.
	Desired *autoscalingv2.HorizontalPodAutoscaler
	// Name and Namespace identify the HPA for the delete path.
	Name      string
	Namespace string
	// Conditions is the CR's condition slice, mutated in place.
	Conditions *[]metav1.Condition
	// Generation is stamped onto every condition the flow writes.
	Generation int64
	// ConditionType is the readiness condition the flow reports on.
	ConditionType string
}

// ReconcileHPA ensures the HorizontalPodAutoscaler matches the desired state. It
// is the shared body of every operator's reconcileHPA sub-reconciler:
//
//   - spec.autoscaling nil: delete any existing HPA and set the condition
//     True/HPANotRequired.
//   - spec.autoscaling set: apply the HPA via SSA and set the condition
//     True/HPAReady.
func ReconcileHPA(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, p HPAFlowParams) (ctrl.Result, error) {
	if !p.Enabled {
		if err := DeleteHPA(ctx, c, p.Namespace, p.Name); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting HorizontalPodAutoscaler: %w", err)
		}
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: p.Generation,
			Reason:             ReasonHPANotRequired,
			Message:            "Autoscaling is not configured",
		})
		return ctrl.Result{}, nil
	}

	if err := EnsureHPA(ctx, c, scheme, owner, p.Desired); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring HorizontalPodAutoscaler: %w", err)
	}
	conditions.SetCondition(p.Conditions, metav1.Condition{
		Type:               p.ConditionType,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: p.Generation,
		Reason:             ReasonHPAReady,
		Message:            "HorizontalPodAutoscaler is configured",
	})
	return ctrl.Result{}, nil
}
