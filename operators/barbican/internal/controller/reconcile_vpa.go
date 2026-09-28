// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
)

// reconcileVPA ensures the VerticalPodAutoscaler of the Barbican API
// Deployment matches spec.deployment.verticalAutoscaling, via the shared VPA
// flow.
func (r *BarbicanReconciler) reconcileVPA(ctx context.Context, children client.Client, barbican *barbicanv1alpha1.Barbican) (ctrl.Result, error) {
	return deployment.ReconcileVPAs(ctx, children, r.Scheme, barbican, deployment.VPAFlowParams{
		Targets:        barbicanVPATargets(barbican),
		Namespace:      barbican.Namespace,
		Labels:         commonLabels(barbican),
		LocalAvailable: r.vpaAvailable,
		Conditions:     &barbican.Status.Conditions,
		Generation:     barbican.Generation,
		ConditionType:  "VPAReady",
	})
}

// barbicanVPATargets lists the long-running workloads of a Barbican: the API
// Deployment, fed by spec.deployment.
func barbicanVPATargets(barbican *barbicanv1alpha1.Barbican) []deployment.VPATarget {
	return []deployment.VPATarget{
		{Kind: "Deployment", Name: subResourceName(barbican), Spec: barbican.Spec.Deployment.VerticalAutoscaling},
	}
}
