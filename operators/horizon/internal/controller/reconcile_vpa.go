// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	horizonv1alpha1 "github.com/c5c3/cobaltcore/operators/horizon/api/v1alpha1"
)

// reconcileVPA ensures the VerticalPodAutoscaler of the Horizon dashboard
// Deployment matches spec.deployment.verticalAutoscaling, via the shared VPA
// flow.
func (r *HorizonReconciler) reconcileVPA(ctx context.Context, children client.Client, horizon *horizonv1alpha1.Horizon) (ctrl.Result, error) {
	return deployment.ReconcileVPAs(ctx, children, r.Scheme, horizon, deployment.VPAFlowParams{
		Targets:        horizonVPATargets(horizon),
		Namespace:      horizon.Namespace,
		Labels:         commonLabels(horizon),
		LocalAvailable: r.vpaAvailable,
		Conditions:     &horizon.Status.Conditions,
		Generation:     horizon.Generation,
		ConditionType:  "VPAReady",
	})
}

// horizonVPATargets lists the long-running workloads of a Horizon: the dashboard
// Deployment, fed by spec.deployment.
func horizonVPATargets(horizon *horizonv1alpha1.Horizon) []deployment.VPATarget {
	return []deployment.VPATarget{
		{Kind: "Deployment", Name: subResourceName(horizon), Spec: horizon.Spec.Deployment.VerticalAutoscaling},
	}
}
