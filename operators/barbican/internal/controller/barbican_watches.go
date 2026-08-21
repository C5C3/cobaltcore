// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Watch event mappers for the Barbican reconciler. Kept in one place so the
// controller file stays focused on its reconcile chain while the Secret and
// cross-CR event-to-request plumbing lives here, mirroring glance_watches.go.
// The mappers the BarbicanSecretStore controller registers (Barbican and
// OpenBaoCluster fan-out) live beside that controller.
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
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
)

// secretToBarbicanMapper returns a MapFunc that maps Secret events to reconcile
// requests for Barbican CRs that either reference the Secret by name (resolved
// via the BarbicanSecretNameIndexKey field indexer) or own it via an
// OwnerReference with Kind=Barbican and APIVersion in the Barbican API group. It
// binds the shared watch.SecretToOwnersMapper to the Barbican types; the
// group-only owner-ref match and the cached staleness Get live there.
func secretToBarbicanMapper(c client.Reader) handler.MapFunc {
	return watch.SecretToOwnersMapper(c, watch.SecretMapperConfig{
		IndexKey:   BarbicanSecretNameIndexKey,
		NewList:    func() client.ObjectList { return &barbicanv1alpha1.BarbicanList{} },
		OwnerGroup: barbicanv1alpha1.GroupVersion.Group,
		OwnerKind:  "Barbican",
		NewObject:  func() client.Object { return &barbicanv1alpha1.Barbican{} },
	})
}

// secretToBarbicanWithStoresMapper extends secretToBarbicanMapper with the
// store leg: a Secret referenced by a BarbicanSecretStore (the AppRole
// credentials or CA bundle Secret of a brownfield store, resolved via the
// BarbicanSecretStoreSecretNameIndexKey field indexer) enqueues the store's
// parent Barbican (spec.barbicanRef.name) so the rendered secret-store config is
// re-projected on credential rotation. The base Barbican legs (name index +
// owner-ref) are unchanged; results are unioned by NamespacedName so a Secret
// matching both legs yields exactly one request. On a store List error the
// mapper logs and returns the base results, matching the sibling mappers'
// log-and-continue contract.
func secretToBarbicanWithStoresMapper(c client.Reader) handler.MapFunc {
	base := secretToBarbicanMapper(c)
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		requests := base(ctx, obj)

		var stores barbicanv1alpha1.BarbicanSecretStoreList
		if err := c.List(
			ctx, &stores,
			client.InNamespace(obj.GetNamespace()),
			client.MatchingFields{BarbicanSecretStoreSecretNameIndexKey: obj.GetName()},
		); err != nil {
			log.FromContext(ctx).Error(err, "listing BarbicanSecretStores for Secret watch")
			return requests
		}
		if len(stores.Items) == 0 {
			return requests
		}

		seen := make(map[types.NamespacedName]struct{}, len(requests))
		for _, req := range requests {
			seen[req.NamespacedName] = struct{}{}
		}
		for i := range stores.Items {
			store := &stores.Items[i]
			if store.Spec.BarbicanRef.Name == "" {
				continue
			}
			key := types.NamespacedName{Namespace: store.Namespace, Name: store.Spec.BarbicanRef.Name}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			requests = append(requests, reconcile.Request{NamespacedName: key})
		}
		return requests
	}
}

// barbicanSecretStoreToBarbicanMapper returns a MapFunc that maps a
// BarbicanSecretStore event to a reconcile request for the Barbican it attaches
// to (spec.barbicanRef). Registered WITHOUT a generation predicate on the
// parent's watch: store status flips (CredentialsReady turning True) are exactly
// what wakes the barbican-side sub-reconciler to project the secret-store
// sections, and the DeletionTimestamp flip is what triggers de-projection.
func barbicanSecretStoreToBarbicanMapper() handler.MapFunc {
	return func(_ context.Context, obj client.Object) []reconcile.Request {
		store, ok := obj.(*barbicanv1alpha1.BarbicanSecretStore)
		if !ok || store.Spec.BarbicanRef.Name == "" {
			return nil
		}
		return []reconcile.Request{{
			NamespacedName: types.NamespacedName{
				Namespace: store.Namespace,
				Name:      store.Spec.BarbicanRef.Name,
			},
		}}
	}
}

// mariaDBToBarbicanMapper returns a MapFunc that maps MariaDB cluster events to
// reconcile requests for Barbican CRs whose spec.database.clusterRef targets the
// MariaDB by name in the same namespace. It binds the shared
// watch.ClusterRefMapper to the Barbican list type and its database clusterRef.
func mariaDBToBarbicanMapper(c client.Reader) handler.MapFunc {
	return watch.ClusterRefMapper(c,
		func() client.ObjectList { return &barbicanv1alpha1.BarbicanList{} },
		func(o client.Object) string {
			b, ok := o.(*barbicanv1alpha1.Barbican)
			if !ok || b.Spec.Database.ClusterRef == nil {
				return ""
			}
			return b.Spec.Database.ClusterRef.Name
		})
}

// esoStoreToBarbicanMapper returns a MapFunc that enqueues the Barbican CRs
// whose effective secret store reference resolves to the changed External
// Secrets store object. watchedKind selects which store scope this mapper is
// registered against — a cluster-scoped ClusterSecretStore (shared across
// namespaces) or a namespaced SecretStore (per tenant). A Barbican that omits
// spec.secretStoreRef resolves to the shared cluster store via
// secrets.EffectiveStoreRef, so the default backend-outage fan-out is preserved
// while a Barbican pinned to a namespaced store is only woken by its own store.
// It binds the shared watch.StoreRefFanOut to the Barbican list type.
//
// The sibling operators call this mapper storeTo<Kind>Mapper; the eso prefix
// keeps it apart from the BarbicanSecretStore mappers, which this package also
// carries and which resolve an entirely different kind.
func esoStoreToBarbicanMapper(c client.Reader, watchedKind commonv1.SecretStoreRefKind) handler.MapFunc {
	return watch.StoreRefFanOut(c, watchedKind,
		func() client.ObjectList { return &barbicanv1alpha1.BarbicanList{} },
		func(o client.Object) commonv1.SecretStoreRefSpec {
			b, ok := o.(*barbicanv1alpha1.Barbican)
			if !ok {
				return commonv1.SecretStoreRefSpec{}
			}
			return secrets.EffectiveStoreRef(b.Spec.SecretStoreRef)
		})
}
