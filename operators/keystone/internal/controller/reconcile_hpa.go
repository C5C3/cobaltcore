// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/c5c3/forge/internal/common/deployment"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// subResourceName returns the canonical name for Keystone operator-managed
// sub-resources (Deployment, HPA, Service, PodDisruptionBudget, NetworkPolicy,
// HTTPRoute). Centralised here so the naming convention is defined in one
// place — the bare CR name with no suffix.
func subResourceName(keystone *keystonev1alpha1.Keystone) string {
	return keystone.Name
}

// reconcileHPA ensures the HorizontalPodAutoscaler for the Keystone API
// deployment matches the desired state, via the shared HPA flow. It keeps only
// the service-specific desired HPA builder.
func (r *KeystoneReconciler) reconcileHPA(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	var desired *autoscalingv2.HorizontalPodAutoscaler
	if keystone.Spec.Autoscaling != nil {
		desired = buildKeystoneHPA(keystone)
	}
	return deployment.ReconcileHPA(ctx, r.Client, r.Scheme, keystone, deployment.HPAFlowParams{
		Enabled:       keystone.Spec.Autoscaling != nil,
		Desired:       desired,
		Name:          subResourceName(keystone),
		Namespace:     keystone.Namespace,
		Conditions:    &keystone.Status.Conditions,
		Generation:    keystone.Generation,
		ConditionType: "HPAReady",
	})
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
