// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// reconcileVPA ensures the VerticalPodAutoscalers of the OVNCentral workloads
// match their verticalAutoscaling blocks, via the shared VPA flow.
func (r *OVNCentralReconciler) reconcileVPA(ctx context.Context, children client.Client, cr *ovnv1alpha1.OVNCentral) (ctrl.Result, error) {
	return deployment.ReconcileVPAs(ctx, children, r.Scheme, cr, deployment.VPAFlowParams{
		Targets:        centralVPATargets(cr),
		Namespace:      cr.Namespace,
		Labels:         naming.CommonLabels(centralAppName, cr.Name),
		LocalAvailable: r.vpaAvailable,
		Conditions:     &cr.Status.Conditions,
		Generation:     cr.Generation,
		ConditionType:  "VPAReady",
	})
}

// centralVPATargets lists the long-running workloads of an OVNCentral: the
// northd Deployment, the relay Deployment while spec.relay is set, and the two
// Raft StatefulSets, whose VPAs never go below the request floor the members
// render with.
func centralVPATargets(cr *ovnv1alpha1.OVNCentral) []deployment.VPATarget {
	targets := []deployment.VPATarget{
		{Kind: "Deployment", Name: northdName(cr), Spec: cr.Spec.Northd.Deployment.VerticalAutoscaling},
	}
	if cr.Spec.Relay != nil {
		targets = append(targets, deployment.VPATarget{Kind: "Deployment", Name: relayName(cr), Spec: cr.Spec.Relay.VerticalAutoscaling})
	}
	for _, db := range []raftDB{northboundDB(cr), southboundDB(cr)} {
		targets = append(targets, deployment.VPATarget{
			Kind: "StatefulSet", Name: raftName(cr, db), Spec: withRaftFloor(db.spec.VerticalAutoscaling),
		})
	}
	return targets
}

// withRaftFloor returns a copy of v whose minAllowed carries the Raft request
// floor, DefaultCPURequest for cpu and MemoryRequestFloor for memory, for each
// of the two the block does not name, so the VPA never moves a member below
// what commonv1.WithRequestFloor renders. A value the block names is kept,
// even one below the floor. A nil v stays nil.
func withRaftFloor(v *commonv1.VerticalAutoscalingSpec) *commonv1.VerticalAutoscalingSpec {
	if v == nil {
		return nil
	}
	out := v.DeepCopy()
	out.MinAllowed = commonv1.WithRequestFloor(&corev1.ResourceRequirements{Requests: v.MinAllowed}).Requests
	return out
}

// reconcileVPA ensures the VerticalPodAutoscalers of the two chassis
// DaemonSets match spec.verticalAutoscaling, via the shared VPA flow.
func (r *OVNChassisReconciler) reconcileVPA(ctx context.Context, children client.Client, cr *ovnv1alpha1.OVNChassis) (ctrl.Result, error) {
	return deployment.ReconcileVPAs(ctx, children, r.Scheme, cr, deployment.VPAFlowParams{
		Targets:        chassisVPATargets(cr),
		Namespace:      cr.Namespace,
		Labels:         naming.CommonLabels(chassisAppName, cr.Name),
		LocalAvailable: r.vpaAvailable,
		Conditions:     &cr.Status.Conditions,
		Generation:     cr.Generation,
		ConditionType:  "VPAReady",
	})
}

// chassisVPATargets lists the two DaemonSets of an OVNChassis, Open vSwitch
// and ovn-controller, both fed by spec.verticalAutoscaling.
func chassisVPATargets(cr *ovnv1alpha1.OVNChassis) []deployment.VPATarget {
	return []deployment.VPATarget{
		{Kind: "DaemonSet", Name: chassisOVSName(cr), Spec: cr.Spec.VerticalAutoscaling},
		{Kind: "DaemonSet", Name: chassisControllerName(cr), Spec: cr.Spec.VerticalAutoscaling},
	}
}
