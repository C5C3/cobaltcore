// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Watch event mappers for the Nova reconciler. Kept in one place so the
// controller file stays focused on its reconcile chain while the Secret,
// MariaDB and secret-store event-to-request plumbing lives here, mirroring
// cinder_watches.go.
package controller

import (
	"context"
	"slices"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/secrets"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// NovaSecretNameIndexKey is the field-indexer key under which Nova CRs are
// indexed by the union of their referenced Secret names. Used by
// SetupWithManager to register the indexer and by secretToNovaMapper to perform
// an O(1) reverse lookup instead of an unfiltered List of all Nova CRs in the
// namespace.
// #nosec G101 -- field-indexer key (a JSONPath-like field selector), not a credential.
const NovaSecretNameIndexKey = "spec.secretRefs.name"

// novaSecretNameExtractor is the controller-runtime IndexerFunc registered under
// NovaSecretNameIndexKey. It returns the deduplicated, non-empty union of Secret
// names a Nova CR references: the two database credentials, the TLS material of
// each schema whose block is enabled, the service-user password, the metadata
// shared secret, the brownfield transport URL, the broker CA bundle, and the
// remote transport URL. A disabled TLS block keeps its references, so they are
// indexed only while the block is on and the connection actually reads them.
//
// spec.messaging.secretRef is nil in managed mode, where the transport URL is
// derived from a RabbitmqCluster instead of read from a Secret, and
// spec.messaging.tls is nil on a plaintext bus. spec.remoteCompute is nil on a
// Nova that publishes no remote contract; while it is set, a rotated remote
// URL, or a Secret that appears after the step waited for it, enqueues the Nova.
func novaSecretNameExtractor(obj client.Object) []string {
	nova, ok := obj.(*novav1alpha1.Nova)
	if !ok {
		// controller-runtime should never call us with the wrong type; a nil
		// return is safer than a panic if it ever does.
		return nil
	}

	referenced := []string{
		nova.Spec.APIDatabase.SecretRef.Name,
		nova.Spec.Database.SecretRef.Name,
		nova.Spec.ServiceUser.SecretRef.Name,
		nova.Spec.Metadata.SharedSecretRef.Name,
	}
	for _, db := range []commonv1.DatabaseSpec{nova.Spec.APIDatabase, nova.Spec.Database} {
		if db.TLS.IsEnabled() {
			referenced = append(referenced,
				db.TLS.CABundleSecretRef.Name, db.TLS.ClientCertSecretRef.Name)
		}
	}
	if nova.Spec.Messaging.SecretRef != nil {
		referenced = append(referenced, nova.Spec.Messaging.SecretRef.Name)
	}
	if nova.Spec.Messaging.TLS != nil {
		referenced = append(referenced, nova.Spec.Messaging.TLS.CABundleSecretRef.Name)
	}
	if rc := nova.Spec.RemoteCompute; rc != nil {
		referenced = append(referenced, rc.TransportURLSecretRef.Name)
	}

	names := make([]string, 0, len(referenced))
	for _, name := range referenced {
		if name == "" || slices.Contains(names, name) {
			continue
		}
		names = append(names, name)
	}
	return names
}

// secretToNovaMapper returns a MapFunc that maps Secret events to reconcile
// requests for Nova CRs that either reference the Secret by name (resolved via
// the NovaSecretNameIndexKey field indexer) or own it via an OwnerReference with
// Kind=Nova and an APIVersion in the Nova API group (the two derived
// db-connection Secrets, the transport-url Secret and the two compute contracts). It
// binds the shared watch.SecretToOwnersMapper to the Nova types; the group-only
// owner-ref match and the cached staleness Get live there.
func secretToNovaMapper(c client.Reader) handler.MapFunc {
	return watch.SecretToOwnersMapper(c, watch.SecretMapperConfig{
		IndexKey:   NovaSecretNameIndexKey,
		NewList:    func() client.ObjectList { return &novav1alpha1.NovaList{} },
		OwnerGroup: novav1alpha1.GroupVersion.Group,
		OwnerKind:  "Nova",
		NewObject:  func() client.Object { return &novav1alpha1.Nova{} },
	})
}

// mariaDBToNovaMapper returns a MapFunc that maps MariaDB cluster events to
// reconcile requests for Nova CRs whose spec.apiDatabase.clusterRef OR
// spec.database.clusterRef targets the MariaDB by name in the same namespace.
// Nova holds its two schemas on two independently referenced clusters, so both
// references have to reach the CR; a Nova that keeps them on one cluster is
// enqueued once, because a duplicated request would reconcile it twice for a
// single event.
func mariaDBToNovaMapper(c client.Reader) handler.MapFunc {
	apiCluster := novaClusterRefMapper(c, func(nova *novav1alpha1.Nova) *commonv1.DatabaseSpec {
		return &nova.Spec.APIDatabase
	})
	cellCluster := novaClusterRefMapper(c, func(nova *novav1alpha1.Nova) *commonv1.DatabaseSpec {
		return &nova.Spec.Database
	})
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		requests := apiCluster(ctx, obj)
		for _, request := range cellCluster(ctx, obj) {
			if !slices.Contains(requests, request) {
				requests = append(requests, request)
			}
		}
		return requests
	}
}

// novaClusterRefMapper binds the shared watch.ClusterRefMapper to the Nova list
// type and the clusterRef of the database block blockOf selects.
func novaClusterRefMapper(c client.Reader,
	blockOf func(*novav1alpha1.Nova) *commonv1.DatabaseSpec,
) handler.MapFunc {
	return watch.ClusterRefMapper(c,
		func() client.ObjectList { return &novav1alpha1.NovaList{} },
		func(o client.Object) string {
			nova, ok := o.(*novav1alpha1.Nova)
			if !ok {
				return ""
			}
			block := blockOf(nova)
			if block.ClusterRef == nil {
				return ""
			}
			return block.ClusterRef.Name
		})
}

// storeToNovaMapper returns a MapFunc that enqueues the Nova CRs whose effective
// secret store reference resolves to the changed store object. watchedKind
// selects which store scope this mapper is registered against, a cluster-scoped
// ClusterSecretStore (shared across namespaces) or a namespaced SecretStore (per
// tenant). A Nova that omits spec.secretStoreRef resolves to the shared cluster
// store via secrets.EffectiveStoreRef, so the default backend-outage fan-out is
// preserved while a Nova pinned to a namespaced store is only woken by its own
// store. It binds the shared watch.StoreRefFanOut to the Nova list type.
func storeToNovaMapper(c client.Reader, watchedKind commonv1.SecretStoreRefKind) handler.MapFunc {
	return watch.StoreRefFanOut(c, watchedKind,
		func() client.ObjectList { return &novav1alpha1.NovaList{} },
		func(o client.Object) commonv1.SecretStoreRefSpec {
			nova, ok := o.(*novav1alpha1.Nova)
			if !ok {
				return commonv1.SecretStoreRefSpec{}
			}
			return secrets.EffectiveStoreRef(nova.Spec.SecretStoreRef)
		})
}
