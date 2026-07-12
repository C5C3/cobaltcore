// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// RegisterSecretNameIndex registers a field indexer for the given CR type
// under indexKey with the given extractor. SetupWithManager calls this once
// against mgr.GetFieldIndexer() so SecretToOwnersMapper can resolve a Secret
// event to the referencing CRs via an O(1) reverse lookup instead of an
// unfiltered namespace-scoped List. The returned error is wrapped with the
// index key so the registration site is identifiable in manager-startup
// failure logs.
func RegisterSecretNameIndex(ctx context.Context, indexer client.FieldIndexer, obj client.Object, indexKey string, extract client.IndexerFunc) error {
	if err := indexer.IndexField(ctx, obj, indexKey, extract); err != nil {
		return fmt.Errorf("registering field indexer %q: %w", indexKey, err)
	}
	return nil
}

// SecretMapperConfig parameterizes SecretToOwnersMapper on the CR type.
type SecretMapperConfig struct {
	// IndexKey is the field-indexer key registered via
	// RegisterSecretNameIndex under which the CRs are indexed by the Secret
	// names they reference.
	IndexKey string

	// NewList constructs an empty typed list of the CR type for the indexed
	// namespace-scoped List.
	NewList func() client.ObjectList

	// OwnerGroup and OwnerKind enable the owner-reference leg: a Secret whose
	// ownerReferences carry Kind==OwnerKind and any APIVersion in OwnerGroup
	// also enqueues the owning CR (e.g. keystone rotation staging Secrets).
	// An empty OwnerKind disables the leg so an index-only shape is
	// expressible. Matching on the group only — not the exact APIVersion —
	// keeps Secrets persisted with an older APIVersion resolving correctly
	// after a future API version bump.
	OwnerGroup string
	OwnerKind  string

	// NewObject constructs an empty CR used for the cached staleness Get on
	// the owner-ref leg. Required when OwnerKind is set.
	NewObject func() client.Object
}

// SecretToOwnersMapper returns a MapFunc that maps Secret events to reconcile
// requests for CRs that either reference the Secret by name (resolved via the
// cfg.IndexKey field indexer) or — when the owner-ref leg is enabled — own it
// via an OwnerReference.
//
// For each matching owner ref, the mapper performs a cached Get to drop
// owner-refs whose target CR no longer exists in the informer cache; any
// non-NotFound error falls through to enqueue, so a transient cache blip
// cannot swallow a legitimate event.
func SecretToOwnersMapper(c client.Reader, cfg SecretMapperConfig) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		namespace := obj.GetNamespace()
		secretName := obj.GetName()
		seen := make(map[types.NamespacedName]struct{})

		list := cfg.NewList()
		if err := c.List(
			ctx, list,
			client.InNamespace(namespace),
			client.MatchingFields{cfg.IndexKey: secretName},
		); err != nil {
			// Log and swallow: the owner-ref path below is independent of
			// the index and must still run.
			log.FromContext(ctx).Error(err, "listing CRs for secret watch", "indexKey", cfg.IndexKey)
		} else {
			items, err := apimeta.ExtractList(list)
			if err != nil {
				log.FromContext(ctx).Error(err, "extracting CR list for secret watch")
			} else {
				for _, item := range items {
					if o, ok := item.(client.Object); ok {
						seen[client.ObjectKeyFromObject(o)] = struct{}{}
					}
				}
			}
		}

		if cfg.OwnerKind != "" {
			for _, ref := range obj.GetOwnerReferences() {
				if ref.Kind != cfg.OwnerKind {
					continue
				}
				gv, err := schema.ParseGroupVersion(ref.APIVersion)
				if err != nil || gv.Group != cfg.OwnerGroup {
					continue
				}
				key := types.NamespacedName{Namespace: namespace, Name: ref.Name}
				// Drop stale/spurious owner-refs whose target CR no longer
				// exists. A cached Get is an in-memory lookup — no API server
				// round-trip.
				if err := c.Get(ctx, key, cfg.NewObject()); err != nil {
					if apierrors.IsNotFound(err) {
						continue
					}
					// Non-NotFound errors (cache mid-sync, disconnected
					// informer, unregistered GVK) must not silently drop the
					// event; log at V(1) and fall through to enqueue so
					// reconcile can resolve authoritatively.
					log.FromContext(ctx).V(1).Info("owner-ref Get returned non-NotFound error; enqueueing anyway",
						"secret", client.ObjectKeyFromObject(obj),
						"ownerRef", key,
						"error", err)
				}
				seen[key] = struct{}{}
			}
		}

		if len(seen) == 0 {
			return nil
		}
		requests := make([]reconcile.Request, 0, len(seen))
		for key := range seen {
			requests = append(requests, reconcile.Request{NamespacedName: key})
		}
		return requests
	}
}

// StoreRefFanOut returns a MapFunc that enqueues the CRs whose effective secret
// store reference resolves to the changed store object. watchedKind is the
// scope of the store object the returned mapper is registered against — either
// SecretStoreKindCluster (a cluster-scoped ClusterSecretStore, shared across
// namespaces) or SecretStoreKindNamespaced (a per-tenant SecretStore). newList
// supplies the CR list type and effectiveRef extracts a CR's already-resolved
// store reference (secrets.EffectiveStoreRef(...) so nil/empty-kind CRs still
// carry the concrete default).
//
// For a cluster-scoped store every CR in the cluster is a candidate, so the
// List is unscoped; for a namespaced store only CRs in the store's own
// namespace can reference it, so the List carries client.InNamespace. A CR is
// enqueued only when its effective ref's kind AND name match the event object,
// so a store status flip (e.g. ESO losing the backend connection) retriggers
// reconcile precisely on the CRs that route secrets through it — CRs pinned to
// a different store stay untouched. On a List or extract error the mapper logs
// via log.FromContext and returns nil per the handler.MapFunc contract.
func StoreRefFanOut(
	c client.Reader,
	watchedKind commonv1.SecretStoreRefKind,
	newList func() client.ObjectList,
	effectiveRef func(client.Object) commonv1.SecretStoreRefSpec,
) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		list := newList()
		var opts []client.ListOption
		if watchedKind == commonv1.SecretStoreKindNamespaced {
			opts = append(opts, client.InNamespace(obj.GetNamespace()))
		}
		if err := c.List(ctx, list, opts...); err != nil {
			log.FromContext(ctx).Error(err, "listing CRs for secret-store watch", "storeKind", watchedKind)
			return nil
		}
		items, err := apimeta.ExtractList(list)
		if err != nil {
			log.FromContext(ctx).Error(err, "extracting CR list for secret-store watch", "storeKind", watchedKind)
			return nil
		}

		storeName := obj.GetName()
		var requests []reconcile.Request
		for _, item := range items {
			o, ok := item.(client.Object)
			if !ok {
				continue
			}
			ref := effectiveRef(o)
			if ref.Kind == watchedKind && ref.Name == storeName {
				requests = append(requests, reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(o),
				})
			}
		}
		return requests
	}
}

// ClusterRefMapper returns a MapFunc that maps a database-cluster event (a
// MariaDB cluster, a Memcached cluster, …) to reconcile requests for the CRs in
// the same namespace whose clusterRef targets that cluster by name. newList
// supplies the CR list type and clusterRefName extracts a CR's cluster
// reference name (empty when it has none). On a List or extract error the mapper
// logs via log.FromContext and returns nil per the handler.MapFunc contract.
func ClusterRefMapper(c client.Reader, newList func() client.ObjectList, clusterRefName func(client.Object) string) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		list := newList()
		if err := c.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
			log.FromContext(ctx).Error(err, "listing CRs for cluster-ref watch")
			return nil
		}
		items, err := apimeta.ExtractList(list)
		if err != nil {
			log.FromContext(ctx).Error(err, "extracting CR list for cluster-ref watch")
			return nil
		}

		clusterName := obj.GetName()
		var requests []reconcile.Request
		for _, item := range items {
			o, ok := item.(client.Object)
			if !ok {
				continue
			}
			if clusterRefName(o) == clusterName {
				requests = append(requests, reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(o),
				})
			}
		}
		return requests
	}
}
