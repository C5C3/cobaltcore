// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Watch event mappers for the NeutronMetadataAgent reconciler. Kept beside the
// Neutron ones so each controller file stays focused on its reconcile chain
// while the event-to-request plumbing lives in one place per kind.
package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/watch"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// secretToAgentMapper returns a MapFunc that maps Secret events to reconcile
// requests for the NeutronMetadataAgent CRs a Secret belongs to, over two legs:
// the agents that reference it by name (spec.novaMetadata.sharedSecretRef.name,
// spec.messaging.secretRef.name), resolved through the
// NeutronMetadataAgentSecretNameIndexKey field indexer, and the agents that own
// it through an OwnerReference (the derived transport-URL Secret).
//
// Both legs are namespace-scoped: an agent only ever references Secrets beside
// itself, because its pods mount them.
func secretToAgentMapper(c client.Reader) handler.MapFunc {
	return watch.SecretToOwnersMapper(c, watch.SecretMapperConfig{
		IndexKey:   NeutronMetadataAgentSecretNameIndexKey,
		NewList:    func() client.ObjectList { return &neutronv1alpha1.NeutronMetadataAgentList{} },
		OwnerGroup: neutronv1alpha1.GroupVersion.Group,
		OwnerKind:  "NeutronMetadataAgent",
		NewObject:  func() client.Object { return &neutronv1alpha1.NeutronMetadataAgent{} },
	})
}

// chassisToAgentsMapper maps an event on an OVNChassis to a reconcile request
// for every NeutronMetadataAgent running alongside it, resolved through the
// NeutronMetadataAgentChassisRefIndexKey index rather than by listing the
// namespace and filtering by hand.
//
// The leg carries no generation predicate: what the agents wait on is the
// chassis's placement and the central it attaches to, and a chassis whose
// status flips without a spec change can still be the event that resolves an
// agent's gate.
//
// The List is namespace-scoped because spec.chassisRef is: an agent runs
// alongside the OVNChassis beside it. A List failure is logged and maps to
// nothing, per the handler.MapFunc contract, and the requeue the chassis step
// polls with is the fallback.
func chassisToAgentsMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		chassis, ok := obj.(*ovnv1alpha1.OVNChassis)
		if !ok {
			// The leg watches OVNChassis alone, so this cannot happen; mapping an
			// object of another kind by its name would enqueue the agents of the
			// OVNChassis that happens to share it.
			return nil
		}
		return agentRequestsForChassis(ctx, c, chassis.Namespace, chassis.Name)
	}
}

// centralToAgentsMapper maps an event on an OVNCentral to a reconcile request
// for every NeutronMetadataAgent that reads it, resolved in two hops: the
// OVNChassis in the central's namespace whose spec.centralRef names it, and the
// agents indexed under each of those.
//
// The hop is what makes the leg necessary at all. An agent names a chassis, not
// a central, while the two values its pods cannot start without (the Southbound
// address, the client Secret name) are published on the central's status, which
// leaves the generation untouched. Both hops resolve through a field index —
// OVNChassisCentralRefIndexKey and NeutronMetadataAgentChassisRefIndexKey — so
// the leg copies the chassis it needs rather than every one in the namespace,
// each of which carries one status entry per node it selects.
//
// A List failure is logged and maps to nothing, per the handler.MapFunc
// contract.
func centralToAgentsMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		central, ok := obj.(*ovnv1alpha1.OVNCentral)
		if !ok {
			// The leg watches OVNCentral alone, so this cannot happen; mapping an
			// object of another kind by its name would enqueue the agents of the
			// OVNCentral that happens to share it.
			return nil
		}

		var chassis ovnv1alpha1.OVNChassisList
		if err := c.List(ctx, &chassis,
			client.InNamespace(central.Namespace),
			client.MatchingFields{OVNChassisCentralRefIndexKey: central.Name},
		); err != nil {
			log.FromContext(ctx).Error(err, "listing OVNChassis for the OVNCentral watch",
				"ovncentral", client.ObjectKeyFromObject(central))
			return nil
		}

		var requests []reconcile.Request
		seen := make(map[types.NamespacedName]struct{})
		for i := range chassis.Items {
			for _, req := range agentRequestsForChassis(ctx, c, central.Namespace, chassis.Items[i].Name) {
				if _, dup := seen[req.NamespacedName]; dup {
					continue
				}
				seen[req.NamespacedName] = struct{}{}
				requests = append(requests, req)
			}
		}
		return requests
	}
}

// agentRequestsForChassis lists the NeutronMetadataAgent CRs in a namespace that
// name the given OVNChassis, through the chassisRef index. Both OVN watch legs
// resolve through it, so they cannot drift apart on the index key or on the
// namespace scope.
func agentRequestsForChassis(ctx context.Context, c client.Reader, namespace, chassisName string) []reconcile.Request {
	var agents neutronv1alpha1.NeutronMetadataAgentList
	if err := c.List(ctx, &agents,
		client.InNamespace(namespace),
		client.MatchingFields{NeutronMetadataAgentChassisRefIndexKey: chassisName},
	); err != nil {
		log.FromContext(ctx).Error(err, "listing NeutronMetadataAgents for an OVN watch",
			"ovnchassis", client.ObjectKey{Namespace: namespace, Name: chassisName})
		return nil
	}

	requests := make([]reconcile.Request, 0, len(agents.Items))
	for i := range agents.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&agents.Items[i]),
		})
	}
	return requests
}
