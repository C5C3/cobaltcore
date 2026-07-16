// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Watch event mappers for the Glance and GlanceBackend reconcilers. Kept in one
// place so the controller files stay focused on their reconcile chains while
// the Secret and cross-CR event-to-request plumbing both controllers share
// lives here, mirroring keystone_watches.go.
package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/forge/internal/common/secrets"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	"github.com/c5c3/forge/internal/common/watch"
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

// secretToGlanceMapper returns a MapFunc that maps Secret events to reconcile
// requests for Glance CRs that either reference the Secret by name (resolved
// via the GlanceSecretNameIndexKey field indexer) or own it via an
// OwnerReference with Kind=Glance and APIVersion in the Glance API group. It
// binds the shared watch.SecretToOwnersMapper to the Glance types; the
// group-only owner-ref match and the cached staleness Get live there.
func secretToGlanceMapper(c client.Reader) handler.MapFunc {
	return watch.SecretToOwnersMapper(c, watch.SecretMapperConfig{
		IndexKey:   GlanceSecretNameIndexKey,
		NewList:    func() client.ObjectList { return &glancev1alpha1.GlanceList{} },
		OwnerGroup: glancev1alpha1.GroupVersion.Group,
		OwnerKind:  "Glance",
		NewObject:  func() client.Object { return &glancev1alpha1.Glance{} },
	})
}

// secretToGlanceWithBackendsMapper extends secretToGlanceMapper with the
// backend leg: a Secret referenced by a GlanceBackend (the S3 credentials
// Secret, resolved via the GlanceBackendSecretNameIndexKey field indexer)
// enqueues the backend's parent Glance (spec.glanceRef.name) so the rendered
// store config is re-projected on credential rotation. The base
// Glance legs (name index + owner-ref) are unchanged; results are unioned by
// NamespacedName so a Secret matching both legs yields exactly one request. On
// a backend List error the mapper logs and returns the base results, matching
// the sibling mappers' log-and-continue contract.
func secretToGlanceWithBackendsMapper(c client.Reader) handler.MapFunc {
	base := secretToGlanceMapper(c)
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		requests := base(ctx, obj)

		var backends glancev1alpha1.GlanceBackendList
		if err := c.List(
			ctx, &backends,
			client.InNamespace(obj.GetNamespace()),
			client.MatchingFields{GlanceBackendSecretNameIndexKey: obj.GetName()},
		); err != nil {
			log.FromContext(ctx).Error(err, "listing GlanceBackends for Secret watch")
			return requests
		}
		if len(backends.Items) == 0 {
			return requests
		}

		seen := make(map[types.NamespacedName]struct{}, len(requests))
		for _, req := range requests {
			seen[req.NamespacedName] = struct{}{}
		}
		for i := range backends.Items {
			b := &backends.Items[i]
			if b.Spec.GlanceRef.Name == "" {
				continue
			}
			key := types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.GlanceRef.Name}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			requests = append(requests, reconcile.Request{NamespacedName: key})
		}
		return requests
	}
}

// glanceBackendToGlanceMapper returns a MapFunc that maps a GlanceBackend event
// to a reconcile request for the Glance it attaches to (spec.glanceRef).
// Registered WITHOUT a generation predicate on the parent's watch: backend
// status flips (CredentialsReady turning True) are exactly what wakes the
// glance-side sub-reconciler to project the store config, and the
// DeletionTimestamp flip is what triggers de-projection.
func glanceBackendToGlanceMapper() handler.MapFunc {
	return func(_ context.Context, obj client.Object) []reconcile.Request {
		backend, ok := obj.(*glancev1alpha1.GlanceBackend)
		if !ok || backend.Spec.GlanceRef.Name == "" {
			return nil
		}
		return []reconcile.Request{{
			NamespacedName: types.NamespacedName{
				Namespace: backend.Namespace,
				Name:      backend.Spec.GlanceRef.Name,
			},
		}}
	}
}

// mariaDBToGlanceMapper returns a MapFunc that maps MariaDB cluster events to
// reconcile requests for Glance CRs whose spec.database.clusterRef targets the
// MariaDB by name in the same namespace. It binds the shared
// watch.ClusterRefMapper to the Glance list type and its database clusterRef.
func mariaDBToGlanceMapper(c client.Reader) handler.MapFunc {
	return watch.ClusterRefMapper(c,
		func() client.ObjectList { return &glancev1alpha1.GlanceList{} },
		func(o client.Object) string {
			g, ok := o.(*glancev1alpha1.Glance)
			if !ok || g.Spec.Database.ClusterRef == nil {
				return ""
			}
			return g.Spec.Database.ClusterRef.Name
		})
}

// storeToGlanceMapper returns a MapFunc that enqueues the Glance CRs whose
// effective secret store reference resolves to the changed store object.
// watchedKind selects which store scope this mapper is registered against — a
// cluster-scoped ClusterSecretStore (shared across namespaces) or a namespaced
// SecretStore (per tenant). A Glance that omits spec.secretStoreRef resolves to
// the shared cluster store via secrets.EffectiveStoreRef, so the default
// backend-outage fan-out is preserved while a Glance pinned to a namespaced
// store is only woken by its own store. It binds the shared watch.StoreRefFanOut
// to the Glance list type.
func storeToGlanceMapper(c client.Reader, watchedKind commonv1.SecretStoreRefKind) handler.MapFunc {
	return watch.StoreRefFanOut(c, watchedKind,
		func() client.ObjectList { return &glancev1alpha1.GlanceList{} },
		func(o client.Object) commonv1.SecretStoreRefSpec {
			g, ok := o.(*glancev1alpha1.Glance)
			if !ok {
				return commonv1.SecretStoreRefSpec{}
			}
			return secrets.EffectiveStoreRef(g.Spec.SecretStoreRef)
		})
}

// glanceToGlanceBackendsMapper returns a MapFunc that fans a Glance event out
// to every GlanceBackend attached to it, resolved via the
// GlanceBackendGlanceRefIndexKey field indexer. Registered WITHOUT a generation
// predicate: Glance status flips (the store config landing in the Deployment)
// are exactly the transitions the backend controller's ConfigProjected gate
// waits on. On a List error the mapper logs and returns nil per the
// handler.MapFunc contract, matching the sibling mappers in this file.
func glanceToGlanceBackendsMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var backends glancev1alpha1.GlanceBackendList
		if err := c.List(
			ctx, &backends,
			client.InNamespace(obj.GetNamespace()),
			client.MatchingFields{GlanceBackendGlanceRefIndexKey: obj.GetName()},
		); err != nil {
			log.FromContext(ctx).Error(err, "listing GlanceBackends for Glance watch")
			return nil
		}
		requests := make([]reconcile.Request, 0, len(backends.Items))
		for i := range backends.Items {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&backends.Items[i]),
			})
		}
		return requests
	}
}
