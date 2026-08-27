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
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// reconcileHPA ensures the HorizontalPodAutoscaler for the Neutron API
// deployment matches the desired state, via the shared HPA flow. It keeps only
// the service-specific desired HPA builder.
//
// Only the API Deployment is autoscaled. The two worker Deployments scale on
// spec.workers.deployment.replicas alone: they consume a work queue rather than
// serve requests, so there is no request rate for an HPA to track.
func (r *NeutronReconciler) reconcileHPA(ctx context.Context, children client.Client, neutron *neutronv1alpha1.Neutron) (ctrl.Result, error) {
	var desired *autoscalingv2.HorizontalPodAutoscaler
	if neutron.Spec.Autoscaling != nil {
		desired = buildNeutronHPA(neutron)
	}
	return deployment.ReconcileHPA(ctx, children, r.Scheme, neutron, deployment.HPAFlowParams{
		Enabled:       neutron.Spec.Autoscaling != nil,
		Desired:       desired,
		Name:          neutron.Name,
		Namespace:     neutron.Namespace,
		Conditions:    &neutron.Status.Conditions,
		Generation:    neutron.Generation,
		ConditionType: "HPAReady",
	})
}

// buildNeutronHPA constructs the desired HorizontalPodAutoscaler for the Neutron
// API deployment, delegating to the shared builder. MinReplicas defaults to the
// effective spec.deployment.replicas when autoscaling.minReplicas is not set.
func buildNeutronHPA(neutron *neutronv1alpha1.Neutron) *autoscalingv2.HorizontalPodAutoscaler {
	return deployment.BuildHPA(neutron.Namespace, neutron.Name, commonLabels(neutron), &neutron.Spec.Deployment, neutron.Spec.Autoscaling)
}
