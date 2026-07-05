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

// clusterSecretStoreToHorizonMapper returns a MapFunc that enqueues every
// Horizon CR in the cluster when the OpenBao-backed ClusterSecretStore
// changes. The store is cluster-scoped and shared across namespaces, so any
// status transition (e.g. ESO losing the backend connection) must retrigger
// reconcile on all Horizons that route secrets through it. It binds the
// shared watch.ClusterSecretStoreFanOut to the Horizon list type.
func clusterSecretStoreToHorizonMapper(c client.Reader) handler.MapFunc {
	return watch.ClusterSecretStoreFanOut(c, openBaoClusterStoreName,
		func() client.ObjectList { return &horizonv1alpha1.HorizonList{} })
}
