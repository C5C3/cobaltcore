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

// ClusterSecretStoreFanOut returns a MapFunc that enqueues every CR in the
// cluster when the named cluster-scoped store object changes. The store is
// shared across namespaces, so any status transition (e.g. ESO losing the
// backend connection) must retrigger reconcile on all CRs that route secrets
// through it; otherwise the secret-derived conditions would stay stale-True
// until the next periodic resync. On a List error the mapper logs via
// log.FromContext and returns nil per the handler.MapFunc contract.
func ClusterSecretStoreFanOut(c client.Reader, storeName string, newList func() client.ObjectList) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		if obj.GetName() != storeName {
			return nil
		}

		list := newList()
		if err := c.List(ctx, list); err != nil {
			log.FromContext(ctx).Error(err, "listing CRs for ClusterSecretStore watch")
			return nil
		}
		items, err := apimeta.ExtractList(list)
		if err != nil {
			log.FromContext(ctx).Error(err, "extracting CR list for ClusterSecretStore watch")
			return nil
		}

		requests := make([]reconcile.Request, 0, len(items))
		for _, item := range items {
			if o, ok := item.(client.Object); ok {
				requests = append(requests, reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(o),
				})
			}
		}
		return requests
	}
}
