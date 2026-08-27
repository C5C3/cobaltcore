// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// conditionTypeChassisReady is the condition the chassis step reports under. It
// is the first gate of the agent pipeline: the nodes, the Southbound address and
// the client Secret it resolves parameterise every later step, so nothing is
// projected while it is False.
const conditionTypeChassisReady = "ChassisReady"

// The condition reasons of the chassis step.
const (
	conditionReasonChassisNotFound         = "ChassisNotFound"
	conditionReasonChassisReadError        = "ChassisReadError"
	conditionReasonChassisOnAnotherCluster = "ChassisOnAnotherCluster"
	conditionReasonCentralNotFound         = "CentralNotFound"
	conditionReasonCentralReadError        = "CentralReadError"
	conditionReasonCentralNotReady         = "CentralNotReady"
	conditionReasonChassisResolved         = "ChassisResolved"
)

// resolvedChassis carries what the metadata agent needs from the OVNChassis it
// runs alongside and from the OVNCentral that chassis attaches to. The agent
// pod has no API client of its own, so each of these values reaches it through
// the pod spec or through the rendered config file.
type resolvedChassis struct {
	// nodeSelector and tolerations are the chassis's own, copied verbatim: the
	// agent answers the metadata requests of the instances on the nodes the
	// chassis programs, so it runs on exactly those nodes.
	nodeSelector map[string]string
	tolerations  []corev1.Toleration
	// sbAddress is the Southbound address the agent reads the logical model
	// from. It is the database itself rather than a relay: the agent watches the
	// port bindings of its own node, and a read-through cache would add a hop to
	// a request an instance is waiting on.
	sbAddress string
	// clientSecretName names the Secret holding the client certificate the agent
	// presents to the Southbound database. The OVNCentral publishes it, and the
	// agent mounts it the same way the chassis pods do.
	clientSecretName string
}

// reconcileChassis resolves the OVNChassis this agent runs alongside and the
// OVNCentral that chassis attaches to.
//
// Both CRs are read through the management-cluster client rather than through
// the children client: spec.chassisRef is namespace-local and every CR of this
// control plane is written on the management cluster, whatever cluster the
// children land on.
func (r *NeutronMetadataAgentReconciler) reconcileChassis(ctx context.Context,
	cr *neutronv1alpha1.NeutronMetadataAgent,
) (resolvedChassis, ctrl.Result, error) {
	name := cr.Spec.ChassisRef.Name

	var chassis ovnv1alpha1.OVNChassis
	switch err := r.Get(ctx, client.ObjectKey{Namespace: cr.Namespace, Name: name}, &chassis); {
	case apierrors.IsNotFound(err):
		// An agent applied before its OVNChassis is an ordinary ordering of two
		// objects in one manifest, so this polls rather than failing the pass.
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeChassisReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonChassisNotFound,
			Message: fmt.Sprintf("OVNChassis %s does not exist in namespace %s; the agent stays "+
				"unprojected until it does", name, cr.Namespace),
		})
		return resolvedChassis{}, ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	case err != nil:
		err = fmt.Errorf("reading OVNChassis %s/%s: %w", cr.Namespace, name, err)
		agentSkeleton.MarkFailed(cr, conditionTypeChassisReady, conditionReasonChassisReadError, err)
		return resolvedChassis{}, ctrl.Result{}, err
	}

	// Both CRs have to project onto the same cluster. The agent pods share the
	// chassis's nodes and mount the client Secret the chassis's central
	// publishes, and neither a node nor a Secret crosses a cluster boundary.
	//
	// The check cannot move into the validating webhook: spec.chassisRef may name
	// an OVNChassis that does not exist at admission time, and by the time it does
	// the agent is no longer under review. It is a spec error rather than a wait,
	// so it does not requeue; the ref is immutable on both sides, so only deleting
	// and reapplying one of the two CRs can repair it.
	if !sameTargetCluster(cr.Spec.TargetClusterRef, chassis.Spec.TargetClusterRef) {
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeChassisReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonChassisOnAnotherCluster,
			Message: fmt.Sprintf("OVNChassis %s projects onto %s while this NeutronMetadataAgent "+
				"projects onto %s; both have to name the same cluster, because the agent mounts the "+
				"client Secret the central publishes and shares the chassis's node", name,
				describeTargetCluster(chassis.Spec.TargetClusterRef),
				describeTargetCluster(cr.Spec.TargetClusterRef)),
		})
		return resolvedChassis{}, ctrl.Result{}, nil
	}

	centralName := chassis.Spec.CentralRef.Name

	var central ovnv1alpha1.OVNCentral
	switch err := r.Get(ctx, client.ObjectKey{Namespace: cr.Namespace, Name: centralName}, &central); {
	case apierrors.IsNotFound(err):
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeChassisReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonCentralNotFound,
			Message: fmt.Sprintf("OVNCentral %s, which OVNChassis %s attaches to, does not exist in "+
				"namespace %s", centralName, name, cr.Namespace),
		})
		return resolvedChassis{}, ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	case err != nil:
		err = fmt.Errorf("reading OVNCentral %s/%s: %w", cr.Namespace, centralName, err)
		agentSkeleton.MarkFailed(cr, conditionTypeChassisReady, conditionReasonCentralReadError, err)
		return resolvedChassis{}, ctrl.Result{}, err
	}

	// The Southbound address and the client Secret are the two values without
	// which the agent has nothing to read the logical model from and nothing to
	// authenticate with.
	if central.Status.Southbound.InternalDbAddress == "" || central.Status.ClientSecretName == "" {
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeChassisReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonCentralNotReady,
			Message: fmt.Sprintf("Waiting for OVNCentral %s to publish its Southbound address and "+
				"its client Secret", centralName),
		})
		return resolvedChassis{}, ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	}

	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               conditionTypeChassisReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cr.Generation,
		Reason:             conditionReasonChassisResolved,
		Message: fmt.Sprintf("The agent runs on the nodes of OVNChassis %s and reads OVNCentral %s "+
			"at %s", name, centralName, central.Status.Southbound.InternalDbAddress),
	})
	return resolvedChassis{
		// Copied rather than aliased: the rendered DaemonSet must not share a
		// field with the CR it was rendered from, and the chassis publishes no
		// label constant an agent could rebuild the selection from.
		nodeSelector:     maps.Clone(chassis.Spec.NodeSelector),
		tolerations:      copyTolerations(chassis.Spec.Tolerations),
		sbAddress:        central.Status.Southbound.InternalDbAddress,
		clientSecretName: central.Status.ClientSecretName,
	}, ctrl.Result{}, nil
}

// copyTolerations deep-copies a toleration list, so a caller mutating the result
// leaves the OVNChassis it came from untouched. An empty list copies to nil, the
// value a pod spec that tolerates nothing carries.
func copyTolerations(in []corev1.Toleration) []corev1.Toleration {
	if len(in) == 0 {
		return nil
	}
	out := make([]corev1.Toleration, len(in))
	for i := range in {
		in[i].DeepCopyInto(&out[i])
	}
	return out
}
