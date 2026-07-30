// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Watch event mappers for the Placement reconciler. Kept in one place so the
// controller file stays focused on its reconcile chain while the Secret and
// cross-CR event-to-request plumbing lives here, mirroring glance_watches.go.
package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	"github.com/c5c3/forge/internal/common/secrets"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	"github.com/c5c3/forge/internal/common/watch"
	placementv1alpha1 "github.com/c5c3/forge/operators/placement/api/v1alpha1"
)

// secretToPlacementMapper returns a MapFunc that maps Secret events to reconcile
// requests for Placement CRs that either reference the Secret by name (resolved
// via the PlacementSecretNameIndexKey field indexer) or own it via an
// OwnerReference with Kind=Placement and APIVersion in the Placement API group.
// It binds the shared watch.SecretToOwnersMapper to the Placement types; the
// group-only owner-ref match and the cached staleness Get live there.
func secretToPlacementMapper(c client.Reader) handler.MapFunc {
	return watch.SecretToOwnersMapper(c, watch.SecretMapperConfig{
		IndexKey:   PlacementSecretNameIndexKey,
		NewList:    func() client.ObjectList { return &placementv1alpha1.PlacementList{} },
		OwnerGroup: placementv1alpha1.GroupVersion.Group,
		OwnerKind:  "Placement",
		NewObject:  func() client.Object { return &placementv1alpha1.Placement{} },
	})
}

// mariaDBToPlacementMapper returns a MapFunc that maps MariaDB cluster events to
// reconcile requests for Placement CRs whose spec.database.clusterRef targets the
// MariaDB by name in the same namespace. It binds the shared
// watch.ClusterRefMapper to the Placement list type and its database clusterRef.
func mariaDBToPlacementMapper(c client.Reader) handler.MapFunc {
	return watch.ClusterRefMapper(c,
		func() client.ObjectList { return &placementv1alpha1.PlacementList{} },
		func(o client.Object) string {
			p, ok := o.(*placementv1alpha1.Placement)
			if !ok || p.Spec.Database.ClusterRef == nil {
				return ""
			}
			return p.Spec.Database.ClusterRef.Name
		})
}

// storeToPlacementMapper returns a MapFunc that enqueues the Placement CRs whose
// effective secret store reference resolves to the changed store object.
// watchedKind selects which store scope this mapper is registered against — a
// cluster-scoped ClusterSecretStore (shared across namespaces) or a namespaced
// SecretStore (per tenant). A Placement that omits spec.secretStoreRef resolves
// to the shared cluster store via secrets.EffectiveStoreRef, so the default
// backend-outage fan-out is preserved while a Placement pinned to a namespaced
// store is only woken by its own store. It binds the shared watch.StoreRefFanOut
// to the Placement list type.
func storeToPlacementMapper(c client.Reader, watchedKind commonv1.SecretStoreRefKind) handler.MapFunc {
	return watch.StoreRefFanOut(c, watchedKind,
		func() client.ObjectList { return &placementv1alpha1.PlacementList{} },
		func(o client.Object) commonv1.SecretStoreRefSpec {
			p, ok := o.(*placementv1alpha1.Placement)
			if !ok {
				return commonv1.SecretStoreRefSpec{}
			}
			return secrets.EffectiveStoreRef(p.Spec.SecretStoreRef)
		})
}
