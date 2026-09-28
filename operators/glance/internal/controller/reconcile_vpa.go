// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
)

// reconcileVPA ensures the VerticalPodAutoscaler of the Glance API
// Deployment matches spec.deployment.verticalAutoscaling, via the shared VPA
// flow.
func (r *GlanceReconciler) reconcileVPA(ctx context.Context, children client.Client, glance *glancev1alpha1.Glance) (ctrl.Result, error) {
	return deployment.ReconcileVPAs(ctx, children, r.Scheme, glance, deployment.VPAFlowParams{
		Targets:        glanceVPATargets(glance),
		Namespace:      glance.Namespace,
		Labels:         commonLabels(glance),
		LocalAvailable: r.vpaAvailable,
		Conditions:     &glance.Status.Conditions,
		Generation:     glance.Generation,
		ConditionType:  "VPAReady",
	})
}

// glanceVPATargets lists the long-running workloads of a Glance: the API
// Deployment, fed by spec.deployment.
func glanceVPATargets(glance *glancev1alpha1.Glance) []deployment.VPATarget {
	return []deployment.VPATarget{
		{Kind: "Deployment", Name: subResourceName(glance), Spec: glance.Spec.Deployment.VerticalAutoscaling},
	}
}
