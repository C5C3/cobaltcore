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
	conditionReasonCentralNotFound               = "CentralNotFound"
	conditionReasonCentralReadError              = "CentralReadError"
	conditionReasonCentralNotExternallyReachable = "CentralNotExternallyReachable"
	conditionReasonCentralNotReady               = "CentralNotReady"
	conditionReasonCentralUpgrading              = "CentralUpgrading"
	conditionReasonCentralResolved               = "CentralResolved"
)

// resolvedCentral carries what the chassis need from the OVNCentral they attach
// to. The chassis pod has no API client of its own, so every one of these values
// reaches a node through the mounted ConfigMap or through the pod spec rather
// than being looked up on the node.
type resolvedCentral struct {
	// ovnRemote is what ovn-controller dials: the Southbound relay when the
	// central runs one the chassis can reach, the Southbound database itself
	// otherwise.
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
	// chassis container presents: the Secret the pods mount on the chassis's
	// own cluster. The central step sets it only when the two CRs project onto
	// the same cluster, where it is the central's Secret itself; across a
	// cluster boundary the client-Secret step sets it to the copy it writes.
	clientSecretName string
	// sameCluster reports whether the chassis and the central project their
	// children onto the same cluster.
	sameCluster bool
	// centralTargetClusterRef is the cluster the central projects onto, which
	// is where the client-Secret step reads the source Secret from.
	centralTargetClusterRef *commonv1.TargetClusterRefSpec
	// sourceClientSecretName names the Secret the central publishes, in the
	// central's namespace on the central's cluster.
	sourceClientSecretName string
}

// reconcileCentral resolves the OVNCentral this chassis attaches to.
//
// The central CR is read through the management-cluster client rather than
// through the children client: spec.centralRef is namespace-local and both CRs
// are written by whoever deploys the control plane, so the OVNCentral lives
// beside the OVNChassis whatever cluster their children land on.
//
// The two may project their children onto different clusters. A chassis on
// another cluster than its central dials the addresses the central publishes
// on node ports, which requires both databases to be externally reachable, and
// mounts a copy of the client Secret the next step writes onto its own cluster.
// That requirement cannot move into the validating webhook: spec.centralRef may
// name an OVNCentral that does not exist at admission time, and by the time it
// does the chassis is no longer under review.
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

	// Which pair of published addresses applies follows from where the two CRs
	// project their children. Inside one cluster the databases are reached at
	// their Service addresses; from another cluster only the node ports the
	// central publishes for an externally reachable database are routable.
	sameCluster := sameTargetCluster(cr.Spec.TargetClusterRef, central.Spec.TargetClusterRef)
	nbAddress, sbAddress := central.Status.Northbound.InternalDbAddress, central.Status.Southbound.InternalDbAddress
	if !sameCluster {
		// Both databases have to be published. ovn-controller needs the
		// Southbound one alone, but the evacuation Job writes the Northbound
		// database, and a chassis whose gateway nodes could never be evacuated
		// is not a working chassis. The fix is an edit to the central, which
		// the central watch delivers, so this does not requeue.
		if !central.Spec.Northbound.ExternallyReachable || !central.Spec.Southbound.ExternallyReachable {
			conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
				Type:               conditionTypeCentralReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cr.Generation,
				Reason:             conditionReasonCentralNotExternallyReachable,
				Message: fmt.Sprintf("OVNCentral %s projects onto %s while this OVNChassis projects onto %s, "+
					"so the chassis reach it at the addresses published outside its cluster; set "+
					"spec.northbound.externallyReachable and spec.southbound.externallyReachable on the "+
					"OVNCentral to true to publish them", name, describeTargetCluster(central.Spec.TargetClusterRef),
					describeTargetCluster(cr.Spec.TargetClusterRef)),
			})
			return resolvedCentral{}, ctrl.Result{}, nil
		}
		nbAddress, sbAddress = central.Status.Northbound.DbAddress, central.Status.Southbound.DbAddress
	}

	// The Southbound address and the client Secret are the two values without
	// which a chassis has nothing to dial and nothing to authenticate with. The
	// Northbound address may still be empty here: only the evacuation Job reads
	// it, and that Job runs against a chassis the central has long registered.
	if sbAddress == "" || central.Status.ClientSecretName == "" {
		what := "its Southbound address"
		if !sameCluster {
			what += " outside its cluster"
		}
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeCentralReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonCentralNotReady,
			Message:            fmt.Sprintf("Waiting for OVNCentral %s to publish %s and its client Secret", name, what),
		})
		return resolvedCentral{}, ctrl.Result{RequeueAfter: RequeueRaftWait}, nil
	}

	// The relay wins when the central runs one the chassis can reach. Every
	// chassis holds an open Southbound connection, and pointing them at the
	// read-through cache instead of at the Raft leader is the whole reason the
	// relay tier exists. Across a cluster boundary that is a relay published
	// on a node port; one that is not published is skipped, since its cluster
	// IP is unreachable from the chassis's cluster.
	remote := central.Status.RelayAddress
	if !sameCluster {
		remote = ""
		if central.Spec.Relay != nil && central.Spec.Relay.ExternallyReachable {
			if central.Status.RelayDbAddress == "" {
				conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
					Type:               conditionTypeCentralReady,
					Status:             metav1.ConditionFalse,
					ObservedGeneration: cr.Generation,
					Reason:             conditionReasonCentralNotReady,
					Message:            fmt.Sprintf("Waiting for OVNCentral %s to publish its relay's node address", name),
				})
				return resolvedCentral{}, ctrl.Result{RequeueAfter: RequeueRaftWait}, nil
			}
			remote = central.Status.RelayDbAddress
		}
	}
	if remote == "" {
		remote = sbAddress
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

	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               conditionTypeCentralReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cr.Generation,
		Reason:             conditionReasonCentralResolved,
		Message:            fmt.Sprintf("The chassis connect to OVNCentral %s at %s", name, remote),
	})
	resolved := resolvedCentral{
		ovnRemote:               remote,
		nbAddress:               nbAddress,
		sbAddress:               sbAddress,
		sameCluster:             sameCluster,
		centralTargetClusterRef: central.Spec.TargetClusterRef,
		sourceClientSecretName:  central.Status.ClientSecretName,
	}
	if sameCluster {
		resolved.clientSecretName = central.Status.ClientSecretName
	}
	return resolved, ctrl.Result{}, nil
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
