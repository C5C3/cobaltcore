// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Watch event mappers for the Cinder, CinderBackend and CinderBackupBackend
// reconcilers. Kept in one place so the controller files stay focused on their
// reconcile chains while the Secret and cross-CR event-to-request plumbing all
// three share lives here, mirroring glance_watches.go.
package controller

import (
	"slices"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	"github.com/c5c3/cobaltcore/internal/common/secrets"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// CinderSecretNameIndexKey is the field-indexer key under which Cinder CRs are
// indexed by the union of their referenced Secret names
// (spec.database.secretRef.name, spec.serviceUser.secretRef.name and
// spec.messaging.secretRef.name). Used by SetupWithManager to register the
// indexer and by secretToCinderMapper to perform an O(1) reverse lookup instead
// of an unfiltered List of all Cinder CRs in the namespace.
// #nosec G101 -- field-indexer key (a JSONPath-like field selector), not a credential.
const CinderSecretNameIndexKey = "spec.secretRefs.name"

// cinderSecretNameExtractor is the controller-runtime IndexerFunc registered
// under CinderSecretNameIndexKey. It returns the deduplicated, non-empty union
// of Secret names a Cinder CR references, so the field indexer can resolve a
// Secret event to the referencing CR(s) without listing every Cinder in the
// namespace. spec.serviceUser is nil on a Keystone-free Cinder, and
// spec.messaging.secretRef is nil in managed mode, where the transport URL is
// derived from a RabbitmqCluster instead of read from a Secret.
func cinderSecretNameExtractor(obj client.Object) []string {
	cinder, ok := obj.(*cinderv1alpha1.Cinder)
	if !ok {
		// controller-runtime should never call us with the wrong type; a nil
		// return is safer than a panic if it ever does.
		return nil
	}

	referenced := []string{cinder.Spec.Database.SecretRef.Name}
	if cinder.Spec.ServiceUser != nil {
		referenced = append(referenced, cinder.Spec.ServiceUser.SecretRef.Name)
	}
	if cinder.Spec.Messaging.SecretRef != nil {
		referenced = append(referenced, cinder.Spec.Messaging.SecretRef.Name)
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

// cinderBackendParentName returns the name of the Cinder a CinderBackend
// attaches to (spec.cinderRef.name), or "" for an object of another type.
func cinderBackendParentName(o client.Object) string {
	backend, ok := o.(*cinderv1alpha1.CinderBackend)
	if !ok {
		return ""
	}
	return backend.Spec.CinderRef.Name
}

// cinderBackupBackendParentName returns the name of the Cinder a
// CinderBackupBackend attaches to (spec.cinderRef.name), or "" for an object of
// another type.
func cinderBackupBackendParentName(o client.Object) string {
	backupBackend, ok := o.(*cinderv1alpha1.CinderBackupBackend)
	if !ok {
		return ""
	}
	return backupBackend.Spec.CinderRef.Name
}

// secretToCinderMapper returns a MapFunc that maps Secret events to reconcile
// requests for Cinder CRs that either reference the Secret by name (resolved via
// the CinderSecretNameIndexKey field indexer) or own it via an OwnerReference
// with Kind=Cinder and an APIVersion in the Cinder API group (the derived
// db-connection and transport-url Secrets, and the per-backend projections). It
// binds the shared watch.SecretToOwnersMapper to the Cinder types; the
// group-only owner-ref match and the cached staleness Get live there.
//
// There is no satellite leg: an NFS backend and an NFS backup target reference
// no Secret of their own, so no Secret event can reach a Cinder through one.
func secretToCinderMapper(c client.Reader) handler.MapFunc {
	return watch.SecretToOwnersMapper(c, watch.SecretMapperConfig{
		IndexKey:   CinderSecretNameIndexKey,
		NewList:    func() client.ObjectList { return &cinderv1alpha1.CinderList{} },
		OwnerGroup: cinderv1alpha1.GroupVersion.Group,
		OwnerKind:  "Cinder",
		NewObject:  func() client.Object { return &cinderv1alpha1.Cinder{} },
	})
}

// mariaDBToCinderMapper returns a MapFunc that maps MariaDB cluster events to
// reconcile requests for Cinder CRs whose spec.database.clusterRef targets the
// MariaDB by name in the same namespace. It binds the shared
// watch.ClusterRefMapper to the Cinder list type and its database clusterRef.
func mariaDBToCinderMapper(c client.Reader) handler.MapFunc {
	return watch.ClusterRefMapper(c,
		func() client.ObjectList { return &cinderv1alpha1.CinderList{} },
		func(o client.Object) string {
			cinder, ok := o.(*cinderv1alpha1.Cinder)
			if !ok || cinder.Spec.Database.ClusterRef == nil {
				return ""
			}
			return cinder.Spec.Database.ClusterRef.Name
		})
}

// storeToCinderMapper returns a MapFunc that enqueues the Cinder CRs whose
// effective secret store reference resolves to the changed store object.
// watchedKind selects which store scope this mapper is registered against — a
// cluster-scoped ClusterSecretStore (shared across namespaces) or a namespaced
// SecretStore (per tenant). A Cinder that omits spec.secretStoreRef resolves to
// the shared cluster store via secrets.EffectiveStoreRef, so the default
// backend-outage fan-out is preserved while a Cinder pinned to a namespaced
// store is only woken by its own store. It binds the shared watch.StoreRefFanOut
// to the Cinder list type.
func storeToCinderMapper(c client.Reader, watchedKind commonv1.SecretStoreRefKind) handler.MapFunc {
	return watch.StoreRefFanOut(c, watchedKind,
		func() client.ObjectList { return &cinderv1alpha1.CinderList{} },
		func(o client.Object) commonv1.SecretStoreRefSpec {
			cinder, ok := o.(*cinderv1alpha1.Cinder)
			if !ok {
				return commonv1.SecretStoreRefSpec{}
			}
			return secrets.EffectiveStoreRef(cinder.Spec.SecretStoreRef)
		})
}

// cinderBackendToCinderMapper returns a MapFunc that maps a CinderBackend event
// to a reconcile request for the Cinder it attaches to (spec.cinderRef). It
// binds the shared watch.SatelliteToParentMapper to the CinderBackend type; the
// no-generation-predicate registration rationale lives there.
func cinderBackendToCinderMapper() handler.MapFunc {
	return watch.SatelliteToParentMapper(cinderBackendParentName)
}

// cinderBackupBackendToCinderMapper returns a MapFunc that maps a
// CinderBackupBackend event to a reconcile request for the Cinder it attaches to
// (spec.cinderRef).
func cinderBackupBackendToCinderMapper() handler.MapFunc {
	return watch.SatelliteToParentMapper(cinderBackupBackendParentName)
}

// cinderToCinderBackendsMapper returns a MapFunc that fans a Cinder event out to
// every CinderBackend attached to it, resolved via the
// CinderBackendCinderRefIndexKey field indexer. It binds the shared
// watch.ParentToSatellitesMapper to the CinderBackend list type; the
// no-generation-predicate registration rationale and the log-and-continue
// contract live there.
func cinderToCinderBackendsMapper(c client.Reader) handler.MapFunc {
	return watch.ParentToSatellitesMapper(c,
		func() client.ObjectList { return &cinderv1alpha1.CinderBackendList{} },
		CinderBackendCinderRefIndexKey,
		"listing CinderBackends for Cinder watch",
	)
}

// cinderToCinderBackupBackendsMapper returns a MapFunc that fans a Cinder event
// out to every CinderBackupBackend attached to it, resolved via the
// CinderBackupBackendCinderRefIndexKey field indexer.
func cinderToCinderBackupBackendsMapper(c client.Reader) handler.MapFunc {
	return watch.ParentToSatellitesMapper(c,
		func() client.ObjectList { return &cinderv1alpha1.CinderBackupBackendList{} },
		CinderBackupBackendCinderRefIndexKey,
		"listing CinderBackupBackends for Cinder watch",
	)
}
