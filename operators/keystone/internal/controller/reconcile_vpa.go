// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
)

// reconcileVPA ensures the VerticalPodAutoscaler of the Keystone API
// Deployment matches spec.deployment.verticalAutoscaling, via the shared VPA
// flow.
func (r *KeystoneReconciler) reconcileVPA(ctx context.Context, children client.Client, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	return deployment.ReconcileVPAs(ctx, children, r.Scheme, keystone, deployment.VPAFlowParams{
		Targets:        keystoneVPATargets(keystone),
		Namespace:      keystone.Namespace,
		Labels:         commonLabels(keystone),
		LocalAvailable: r.vpaAvailable,
		Conditions:     &keystone.Status.Conditions,
		Generation:     keystone.Generation,
		ConditionType:  "VPAReady",
	})
}

// keystoneVPATargets lists the long-running workloads of a Keystone: the API
// Deployment, fed by spec.deployment.
func keystoneVPATargets(keystone *keystonev1alpha1.Keystone) []deployment.VPATarget {
	return []deployment.VPATarget{
		{Kind: "Deployment", Name: subResourceName(keystone), Spec: keystone.Spec.Deployment.VerticalAutoscaling},
	}
}
