// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Watch event mappers for the Glance and GlanceBackend reconcilers. Kept in one
// place so the controller files stay focused on their reconcile chains while
// the Secret and cross-CR event-to-request plumbing both controllers share
// lives here, mirroring keystone_watches.go.
package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	"github.com/c5c3/cobaltcore/internal/common/secrets"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
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
// store config is re-projected on credential rotation. It binds the shared
// watch.SecretToParentsViaSatellitesMapper to the Glance and GlanceBackend
// types; the request union and the log-and-continue contract live there.
func secretToGlanceWithBackendsMapper(c client.Reader) handler.MapFunc {
	return watch.SecretToParentsViaSatellitesMapper(
		secretToGlanceMapper(c), c,
		func() client.ObjectList { return &glancev1alpha1.GlanceBackendList{} },
		GlanceBackendSecretNameIndexKey,
		"listing GlanceBackends for Secret watch",
		glanceBackendParentName,
	)
}

// glanceBackendToGlanceMapper returns a MapFunc that maps a GlanceBackend event
// to a reconcile request for the Glance it attaches to (spec.glanceRef). It
// binds the shared watch.SatelliteToParentMapper to the GlanceBackend type; the
// no-generation-predicate registration rationale lives there.
func glanceBackendToGlanceMapper() handler.MapFunc {
	return watch.SatelliteToParentMapper(glanceBackendParentName)
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
// GlanceBackendGlanceRefIndexKey field indexer. It binds the shared
// watch.ParentToSatellitesMapper to the GlanceBackend list type; the
// no-generation-predicate registration rationale and the log-and-continue
// contract live there.
func glanceToGlanceBackendsMapper(c client.Reader) handler.MapFunc {
	return watch.ParentToSatellitesMapper(c,
		func() client.ObjectList { return &glancev1alpha1.GlanceBackendList{} },
		GlanceBackendGlanceRefIndexKey,
		"listing GlanceBackends for Glance watch",
	)
}
