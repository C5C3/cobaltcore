// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// conditionTypeCentralReady is the condition the central step reports under. It
// is the first gate of the chassis pipeline: the Southbound address and the
// client Secret it resolves parameterise every later step, so nothing is
// projected while it is False.
const conditionTypeCentralReady = "CentralReady"

// The condition reasons of the central step.
const (
	conditionReasonCentralNotFound         = "CentralNotFound"
	conditionReasonCentralReadError        = "CentralReadError"
	conditionReasonCentralOnAnotherCluster = "CentralOnAnotherCluster"
	conditionReasonCentralNotReady         = "CentralNotReady"
	conditionReasonCentralUpgrading        = "CentralUpgrading"
	conditionReasonCentralResolved         = "CentralResolved"
)

// resolvedCentral carries what the chassis need from the OVNCentral they attach
// to. The chassis pod has no API client of its own, so every one of these values
// reaches a node through the mounted ConfigMap or through the pod spec rather
// than being looked up on the node.
type resolvedCentral struct {
	// ovnRemote is what ovn-controller dials: the Southbound relay when the
	// central runs one, the Southbound database itself otherwise.
	ovnRemote string
	// nbAddress is the Northbound address the gateway-evacuation Job talks to,
	// which is the one maintenance action that edits the logical model rather
	// than reading the Southbound one.
	nbAddress string
	// sbAddress is the Southbound address the chassis-deletion Job talks to. It
	// is the database itself even when a relay exists: a relay forwards writes,
	// but a deregistration that has to be durable is better aimed at the source.
	sbAddress string
	// clientSecretName names the Secret holding the client certificate every
	// chassis container presents.
	clientSecretName string
}

// reconcileCentral resolves the OVNCentral this chassis attaches to.
//
// The central CR is read through the management-cluster client rather than
// through the children client: spec.centralRef is namespace-local and both CRs
// are written by whoever deploys the control plane, so the OVNCentral lives
// beside the OVNChassis whatever cluster their children land on.
func (r *OVNChassisReconciler) reconcileCentral(ctx context.Context, cr *ovnv1alpha1.OVNChassis) (resolvedCentral, ctrl.Result, error) {
	name := cr.Spec.CentralRef.Name

	var central ovnv1alpha1.OVNCentral
	switch err := r.Get(ctx, client.ObjectKey{Namespace: cr.Namespace, Name: name}, &central); {
	case apierrors.IsNotFound(err):
		// An OVNChassis applied before its OVNCentral is an ordinary ordering of
		// two objects in one manifest, so this polls rather than failing the pass.
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeCentralReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonCentralNotFound,
			Message: fmt.Sprintf("OVNCentral %s does not exist in namespace %s; the chassis stay "+
				"unconfigured until it does", name, cr.Namespace),
		})
		return resolvedCentral{}, ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	case err != nil:
		err = fmt.Errorf("reading OVNCentral %s/%s: %w", cr.Namespace, name, err)
		chassisSkeleton.MarkFailed(cr, conditionTypeCentralReady, conditionReasonCentralReadError, err)
		return resolvedCentral{}, ctrl.Result{}, err
	}

	// Both CRs have to project onto the same cluster. A chassis mounts the client
	// Secret the central publishes, and a Secret does not cross a cluster
	// boundary, so a mismatched pair would leave every chassis pod stuck on a
	// volume that never mounts.
	//
	// The check cannot move into the validating webhook: spec.centralRef may name
	// an OVNCentral that does not exist at admission time, and by the time it
	// does the chassis is no longer under review. It is a spec error rather than
	// a wait, so it does not requeue; the ref is immutable on both sides, so only
	// deleting and reapplying one of the two CRs can repair it, and that produces
	// its own event.
	if !sameTargetCluster(cr.Spec.TargetClusterRef, central.Spec.TargetClusterRef) {
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeCentralReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonCentralOnAnotherCluster,
			Message: fmt.Sprintf("OVNCentral %s projects onto %s while this OVNChassis projects onto %s; "+
				"both have to name the same cluster, because a chassis mounts the client Secret the "+
				"central publishes", name, describeTargetCluster(central.Spec.TargetClusterRef),
				describeTargetCluster(cr.Spec.TargetClusterRef)),
		})
		return resolvedCentral{}, ctrl.Result{}, nil
	}

	// The Southbound address and the client Secret are the two values without
	// which a chassis has nothing to dial and nothing to authenticate with. The
	// Northbound address may still be empty here: only the evacuation Job reads
	// it, and that Job runs against a chassis the central has long registered.
	if central.Status.Southbound.InternalDbAddress == "" || central.Status.ClientSecretName == "" {
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeCentralReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonCentralNotReady,
			Message: fmt.Sprintf("Waiting for OVNCentral %s to publish its Southbound address and its "+
				"client Secret", name),
		})
		return resolvedCentral{}, ctrl.Result{RequeueAfter: RequeueRaftWait}, nil
	}

	// OVN's supported upgrade order is central first, hypervisors second:
	// ovn-controller reads the Southbound schema the central owns, so a chassis
	// running ahead of its databases talks to a schema that does not carry what
	// it asks for. The two kinds are reconciled by controllers that know nothing
	// of each other, and both resolve the operator's default image when they
	// leave spec.image unset, so an operator upgrade moves both at once and
	// nothing else keeps the DaemonSets from finishing their rolling update
	// while the StatefulSets are still going member by member.
	//
	// status.installedImage is written once northd runs on the image the central
	// resolves, so the two differ exactly while the central's own rollout is in
	// flight. The chassis image is not compared against it: a chassis pinned to
	// an older image than the central is the direction OVN supports, and a gate
	// that demanded equality would wedge it.
	if resolved := effectiveImage(central.Spec.Image).Reference(); central.Status.InstalledImage != resolved {
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeCentralReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonCentralUpgrading,
			Message: fmt.Sprintf("Waiting for OVNCentral %s to finish rolling out %s; the chassis "+
				"follow the central, because ovn-controller reads the Southbound schema the "+
				"central owns", name, resolved),
		})
		return resolvedCentral{}, ctrl.Result{RequeueAfter: RequeueRaftWait}, nil
	}

	// The relay wins when the central runs one. Every chassis holds an open
	// Southbound connection, and pointing them at the read-through cache instead
	// of at the Raft leader is the whole reason the relay tier exists.
	remote := central.Status.RelayAddress
	if remote == "" {
		remote = central.Status.Southbound.InternalDbAddress
	}

	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               conditionTypeCentralReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cr.Generation,
		Reason:             conditionReasonCentralResolved,
		Message:            fmt.Sprintf("The chassis connect to OVNCentral %s at %s", name, remote),
	})
	return resolvedCentral{
		ovnRemote:        remote,
		nbAddress:        central.Status.Northbound.InternalDbAddress,
		sbAddress:        central.Status.Southbound.InternalDbAddress,
		clientSecretName: central.Status.ClientSecretName,
	}, ctrl.Result{}, nil
}

// sameTargetCluster reports whether two CRs project their children onto the
// same cluster. Two nil refs both mean the management cluster; two set refs
// agree when they name the same registered cluster.
func sameTargetCluster(a, b *commonv1.TargetClusterRefSpec) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Name == b.Name
}

// describeTargetCluster names the cluster a ref selects, for a condition message
// a reader can act on. A nil ref is the cluster the operator itself runs in.
func describeTargetCluster(ref *commonv1.TargetClusterRefSpec) string {
	if ref == nil {
		return "the management cluster"
	}
	return "target cluster " + ref.Name
}
