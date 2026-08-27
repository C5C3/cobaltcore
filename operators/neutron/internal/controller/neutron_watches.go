// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Watch event mappers for the Neutron reconciler. Kept in one place so the
// controller file stays focused on its reconcile chain while the Secret and
// cross-CR event-to-request plumbing lives here, mirroring barbican_watches.go.
package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/secrets"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// secretToNeutronMapper returns a MapFunc that maps Secret events to reconcile
// requests for the Neutron CRs a Secret belongs to, over three legs:
//
//   - the CRs that reference it by name (spec.database.secretRef.name,
//     spec.serviceUser.secretRef.name, spec.messaging.secretRef.name), resolved
//     via the NeutronSecretNameIndexKey field indexer;
//   - the CRs that own it through an OwnerReference with Kind=Neutron and an
//     APIVersion in the Neutron API group (the derived db-connection,
//     transport-url and OVN client Secrets);
//   - the CRs driving an OVNCentral that published this Secret as its client
//     identity (status.clientSecretName). That leg crosses namespaces: the OVN
//     control plane commonly lives in the privileged networking namespace while
//     the Neutron API lives with the rest of the control plane, so the Secret and
//     the CRs waiting on it are rarely in the same one.
//
// The first two legs come from the shared watch.SecretToOwnersMapper; results are
// unioned by NamespacedName so a Secret matching several legs yields exactly one
// request. On a List error the mapper logs and returns what it has, matching the
// sibling mappers' log-and-continue contract.
func secretToNeutronMapper(c client.Reader) handler.MapFunc {
	base := watch.SecretToOwnersMapper(c, watch.SecretMapperConfig{
		IndexKey:   NeutronSecretNameIndexKey,
		NewList:    func() client.ObjectList { return &neutronv1alpha1.NeutronList{} },
		OwnerGroup: neutronv1alpha1.GroupVersion.Group,
		OwnerKind:  "Neutron",
		NewObject:  func() client.Object { return &neutronv1alpha1.Neutron{} },
	})

	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		requests := base(ctx, obj)

		// The centrals are listed in the Secret's own namespace: a central
		// publishes its client identity beside itself.
		var centrals ovnv1alpha1.OVNCentralList
		if err := c.List(ctx, &centrals, client.InNamespace(obj.GetNamespace())); err != nil {
			log.FromContext(ctx).Error(err, "listing OVNCentrals for the Secret watch")
			return requests
		}

		seen := make(map[types.NamespacedName]struct{}, len(requests))
		for _, req := range requests {
			seen[req.NamespacedName] = struct{}{}
		}

		for i := range centrals.Items {
			central := &centrals.Items[i]
			if central.Status.ClientSecretName != obj.GetName() {
				continue
			}
			// Cluster-wide, because the indexed value already pins the central by
			// namespace and name; a Neutron driving it from anywhere has to be woken.
			var neutrons neutronv1alpha1.NeutronList
			if err := c.List(ctx, &neutrons, client.MatchingFields{
				NeutronOVNCentralRefIndexKey: ovnCentralRefIndexValue(central.Namespace, central.Name),
			}); err != nil {
				log.FromContext(ctx).Error(err, "listing Neutrons for the OVN client Secret watch",
					"ovncentral", client.ObjectKeyFromObject(central))
				return requests
			}
			for j := range neutrons.Items {
				key := client.ObjectKeyFromObject(&neutrons.Items[j])
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
				requests = append(requests, reconcile.Request{NamespacedName: key})
			}
		}
		return requests
	}
}

// centralToNeutronsMapper maps an event on an OVNCentral to a reconcile request
// for every Neutron driving it, resolved through the NeutronOVNCentralRefIndexKey
// index rather than by listing every namespace and filtering by hand.
//
// The leg carries no generation predicate: what the Neutrons wait on are the
// central's status flips (the two database addresses being published, the client
// Secret being named), and those leave the generation untouched.
//
// The List is cluster-wide because spec.ovn.centralRef is not namespace-bound: a
// Neutron in the control-plane namespace commonly drives a central in the
// privileged networking namespace. A List failure is logged and maps to nothing,
// per the handler.MapFunc contract, and the requeue the OVN steps poll with is
// the fallback.
func centralToNeutronsMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		central, ok := obj.(*ovnv1alpha1.OVNCentral)
		if !ok {
			// The leg watches OVNCentral alone, so this cannot happen; mapping an
			// object of another kind by its name would enqueue the Neutrons of the
			// OVNCentral that happens to share it.
			return nil
		}

		var neutrons neutronv1alpha1.NeutronList
		if err := c.List(ctx, &neutrons, client.MatchingFields{
			NeutronOVNCentralRefIndexKey: ovnCentralRefIndexValue(central.Namespace, central.Name),
		}); err != nil {
			log.FromContext(ctx).Error(err, "listing Neutrons for the OVNCentral watch",
				"ovncentral", client.ObjectKeyFromObject(central))
			return nil
		}

		requests := make([]reconcile.Request, 0, len(neutrons.Items))
		for i := range neutrons.Items {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&neutrons.Items[i]),
			})
		}
		return requests
	}
}

// mariaDBToNeutronMapper returns a MapFunc that maps MariaDB cluster events to
// reconcile requests for Neutron CRs whose spec.database.clusterRef targets the
// MariaDB by name in the same namespace. It binds the shared
// watch.ClusterRefMapper to the Neutron list type and its database clusterRef.
func mariaDBToNeutronMapper(c client.Reader) handler.MapFunc {
	return watch.ClusterRefMapper(c,
		func() client.ObjectList { return &neutronv1alpha1.NeutronList{} },
		func(o client.Object) string {
			n, ok := o.(*neutronv1alpha1.Neutron)
			if !ok || n.Spec.Database.ClusterRef == nil {
				return ""
			}
			return n.Spec.Database.ClusterRef.Name
		})
}

// esoStoreToNeutronMapper returns a MapFunc that enqueues the Neutron CRs whose
// effective secret store reference resolves to the changed External Secrets store
// object. watchedKind selects which store scope this mapper is registered
// against, a cluster-scoped ClusterSecretStore (shared across namespaces) or a
// namespaced SecretStore (per tenant). A Neutron that omits spec.secretStoreRef
// resolves to the shared cluster store via secrets.EffectiveStoreRef, so the
// default backend-outage fan-out is preserved while a Neutron pinned to a
// namespaced store is only woken by its own store. It binds the shared
// watch.StoreRefFanOut to the Neutron list type.
func esoStoreToNeutronMapper(c client.Reader, watchedKind commonv1.SecretStoreRefKind) handler.MapFunc {
	return watch.StoreRefFanOut(c, watchedKind,
		func() client.ObjectList { return &neutronv1alpha1.NeutronList{} },
		func(o client.Object) commonv1.SecretStoreRefSpec {
			n, ok := o.(*neutronv1alpha1.Neutron)
			if !ok {
				return commonv1.SecretStoreRefSpec{}
			}
			return secrets.EffectiveStoreRef(n.Spec.SecretStoreRef)
		})
}
