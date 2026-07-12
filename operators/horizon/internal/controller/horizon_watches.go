// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Watch event mappers for the Horizon reconciler, kept beside the controller
// following the keystone layout so the controller file stays focused on the
// reconcile chain.
package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	"github.com/c5c3/forge/internal/common/secrets"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	"github.com/c5c3/forge/internal/common/watch"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
)

// secretToHorizonMapper returns a MapFunc that maps Secret events to
// reconcile requests for Horizon CRs that either reference the Secret by name
// (resolved via the HorizonSecretNameIndexKey field indexer) or own it via an
// OwnerReference with Kind=Horizon and APIVersion in the Horizon API group.
// It binds the shared watch.SecretToOwnersMapper to the Horizon types; the
// group-only owner-ref match and the cached staleness Get live there.
func secretToHorizonMapper(c client.Reader) handler.MapFunc {
	return watch.SecretToOwnersMapper(c, watch.SecretMapperConfig{
		IndexKey:   HorizonSecretNameIndexKey,
		NewList:    func() client.ObjectList { return &horizonv1alpha1.HorizonList{} },
		OwnerGroup: horizonv1alpha1.GroupVersion.Group,
		OwnerKind:  "Horizon",
		NewObject:  func() client.Object { return &horizonv1alpha1.Horizon{} },
	})
}

// storeToHorizonMapper returns a MapFunc that enqueues the Horizon CRs whose
// effective secret store reference resolves to the changed store object.
// watchedKind selects which store scope this mapper is registered against — a
// cluster-scoped ClusterSecretStore (shared across namespaces) or a namespaced
// SecretStore (per tenant). A Horizon that omits spec.secretStoreRef resolves
// to the shared cluster store via secrets.EffectiveStoreRef, so the default
// backend-outage fan-out is preserved. It binds the shared watch.StoreRefFanOut
// to the Horizon list type.
func storeToHorizonMapper(c client.Reader, watchedKind commonv1.SecretStoreRefKind) handler.MapFunc {
	return watch.StoreRefFanOut(c, watchedKind,
		func() client.ObjectList { return &horizonv1alpha1.HorizonList{} },
		func(o client.Object) commonv1.SecretStoreRefSpec {
			h, ok := o.(*horizonv1alpha1.Horizon)
			if !ok {
				return commonv1.SecretStoreRefSpec{}
			}
			return secrets.EffectiveStoreRef(h.Spec.SecretStoreRef)
		})
}
