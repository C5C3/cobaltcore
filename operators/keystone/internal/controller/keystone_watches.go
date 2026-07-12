// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Watch event mappers and predicates for the Keystone reconciler. Extracted
// from keystone_controller.go so the controller file stays focused on the
// reconcile chain while the Secret/MariaDB/secret-store/PushSecret
// event-to-request plumbing lives in one place (issue #467).
package controller

import (
	"context"
	"slices"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/forge/internal/common/secrets"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	"github.com/c5c3/forge/internal/common/watch"

	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// secretToKeystoneMapper returns a MapFunc that maps Secret events to reconcile
// requests for Keystone CRs that either reference the Secret by name
// (resolved via the KeystoneSecretNameIndexKey field indexer) or own it via
// an OwnerReference with Kind=Keystone and APIVersion in the Keystone API
// group (e.g. rotation staging Secrets). It binds the shared
// watch.SecretToOwnersMapper to the Keystone types; the group-only owner-ref
// match and the cached staleness Get live there.
func secretToKeystoneMapper(c client.Reader) handler.MapFunc {
	return watch.SecretToOwnersMapper(c, watch.SecretMapperConfig{
		IndexKey:   KeystoneSecretNameIndexKey,
		NewList:    func() client.ObjectList { return &keystonev1alpha1.KeystoneList{} },
		OwnerGroup: keystonev1alpha1.GroupVersion.Group,
		OwnerKind:  "Keystone",
		NewObject:  func() client.Object { return &keystonev1alpha1.Keystone{} },
	})
}

// mariaDBToKeystoneMapper returns a MapFunc that maps MariaDB cluster events
// to reconcile requests for Keystone CRs whose spec.database.clusterRef
// targets the MariaDB by name in the same namespace. It binds the shared
// watch.ClusterRefMapper to the Keystone list type and its database clusterRef.
func mariaDBToKeystoneMapper(c client.Reader) handler.MapFunc {
	return watch.ClusterRefMapper(c,
		func() client.ObjectList { return &keystonev1alpha1.KeystoneList{} },
		func(o client.Object) string {
			ks, ok := o.(*keystonev1alpha1.Keystone)
			if !ok || ks.Spec.Database.ClusterRef == nil {
				return ""
			}
			return ks.Spec.Database.ClusterRef.Name
		})
}

// storeToKeystoneMapper returns a MapFunc that enqueues the Keystone CRs whose
// effective secret store reference resolves to the changed store object.
// watchedKind selects which store scope this mapper is registered against — a
// cluster-scoped ClusterSecretStore (shared across namespaces) or a namespaced
// SecretStore (per tenant). A Keystone that omits spec.secretStoreRef resolves
// to the shared cluster store via secrets.EffectiveStoreRef, so the default
// backend-outage fan-out is preserved while a Keystone pinned to a namespaced
// store is only woken by its own store. It binds the shared
// watch.StoreRefFanOut to the Keystone list type.
func storeToKeystoneMapper(c client.Reader, watchedKind commonv1.SecretStoreRefKind) handler.MapFunc {
	return watch.StoreRefFanOut(c, watchedKind,
		func() client.ObjectList { return &keystonev1alpha1.KeystoneList{} },
		func(o client.Object) commonv1.SecretStoreRefSpec {
			ks, ok := o.(*keystonev1alpha1.Keystone)
			if !ok {
				return commonv1.SecretStoreRefSpec{}
			}
			return secrets.EffectiveStoreRef(ks.Spec.SecretStoreRef)
		})
}

// identityBackendToKeystoneMapper returns a MapFunc that maps a
// KeystoneIdentityBackend event to a reconcile request for the Keystone it
// attaches to (spec.keystoneRef). Registered WITHOUT a generation predicate:
// backend status flips (DomainReady turning True) are exactly what wakes the
// keystone-side identitybackends sub-reconciler to project the domain config,
// and the DeletionTimestamp flip is what triggers de-projection.
func identityBackendToKeystoneMapper() handler.MapFunc {
	return func(_ context.Context, obj client.Object) []reconcile.Request {
		backend, ok := obj.(*keystonev1alpha1.KeystoneIdentityBackend)
		if !ok || backend.Spec.KeystoneRef.Name == "" {
			return nil
		}
		return []reconcile.Request{{
			NamespacedName: types.NamespacedName{
				Namespace: backend.Namespace,
				Name:      backend.Spec.KeystoneRef.Name,
			},
		}}
	}
}

// secretToKeystoneWithBackendsMapper extends secretToKeystoneMapper with the
// identity-backend leg: a Secret referenced by a KeystoneIdentityBackend
// (bind credentials or TLS CA bundle, resolved via the
// IdentityBackendSecretNameIndexKey field indexer) enqueues the backend's
// Keystone so the content-hashed domains Secret is re-rendered on bind/CA
// rotation. The base Keystone legs (name index + owner-ref) are unchanged;
// results are unioned by NamespacedName so a Secret matching both legs yields
// exactly one request. On a backend List error the mapper logs and returns
// the base results, matching the sibling mappers' log-and-continue contract.
func secretToKeystoneWithBackendsMapper(c client.Reader) handler.MapFunc {
	base := secretToKeystoneMapper(c)
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		requests := base(ctx, obj)

		var backends keystonev1alpha1.KeystoneIdentityBackendList
		if err := c.List(
			ctx, &backends,
			client.InNamespace(obj.GetNamespace()),
			client.MatchingFields{IdentityBackendSecretNameIndexKey: obj.GetName()},
		); err != nil {
			log.FromContext(ctx).Error(err, "listing KeystoneIdentityBackends for Secret watch")
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
			if b.Spec.KeystoneRef.Name == "" {
				continue
			}
			key := types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.KeystoneRef.Name}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			requests = append(requests, reconcile.Request{NamespacedName: key})
		}
		return requests
	}
}

// keystoneToIdentityBackendsMapper returns a MapFunc that fans a Keystone
// event out to every KeystoneIdentityBackend attached to it, resolved via the
// IdentityBackendKeystoneRefIndexKey field indexer. Registered WITHOUT a
// generation predicate: Keystone status flips (KeystoneAPIReady flipping
// True, the projection landing) are exactly the transitions the backend
// controller's DomainReady / ConfigProjected gates wait on. On a List error
// the mapper logs and returns nil per the handler.MapFunc contract, matching
// the sibling mappers in this file.
func keystoneToIdentityBackendsMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var backends keystonev1alpha1.KeystoneIdentityBackendList
		if err := c.List(
			ctx, &backends,
			client.InNamespace(obj.GetNamespace()),
			client.MatchingFields{IdentityBackendKeystoneRefIndexKey: obj.GetName()},
		); err != nil {
			log.FromContext(ctx).Error(err, "listing KeystoneIdentityBackends for Keystone watch")
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

// pushSecretToKeystoneMapper returns a MapFunc that maps PushSecret events to
// reconcile requests for the Keystone CR that owns the event's backup
// PushSecret by name. The mapper performs a namespace-scoped Keystone List
// and, for each CR, compares the event object's Name against each entry of
// openBaoBackupPushSecretNames(&ks); a match records the CR in a
// map[types.NamespacedName]struct{} before emission.
//
// Rationale: the backup name set is a 2-element deterministic slice derived
// from keystone.Name, so an O(n_keystones_in_ns * 2) string compare is cheaper
// than registering a dedicated field indexer and avoids any cross-reference
// invariant between PushSecret creation sites and the mapper. Namespace
// scoping is load-bearing — requires that a PushSecret event in ns-b
// never wake a Keystone that lives in ns-a, so the List MUST carry
// client.InNamespace(obj.GetNamespace()) only (never cluster-wide). PushSecret
// is a namespaced resource, so the apiserver guarantees obj.GetNamespace() is
// non-empty in practice; the mapper therefore relies on that guarantee rather
// than guarding the empty-string case (which controller-runtime would treat as
// cluster-wide). On a List error the mapper logs via log.FromContext and
// returns nil per the handler.MapFunc contract (no error return), matching
// the behaviour of
// secretToKeystoneMapper / mariaDBToKeystoneMapper / clusterSecretStoreTo-
// KeystoneMapper. Owner-ref inspection is deliberately omitted: backup
// PushSecrets are created by the keystone operator but an Owns() link on the
// watch would double-enqueue with the name-based mapper, so name match is the
// single source of truth.
//
// On dedup: a given PushSecret name uniquely identifies at most one Keystone
// today, because openBaoBackupPushSecretNames(ks) returns
// {"<ks.Name>-fernet-keys-backup", "<ks.Name>-credential-keys-backup"} and
// both suffixes are prefixed by the CR name. In current behaviour len(seen)
// is therefore always 0 or 1 and the map-based dedup is a no-op. It is kept
// as a future-proofing safety net — symmetric with secretToKeystoneMapper —
// so that if the backup name convention is ever relaxed (e.g. shared backup
// PushSecrets across CRs, or multiple CRs naming the same backup during a
// rename migration) the mapper continues to enqueue each owning CR exactly
// once without a correctness regression.
func pushSecretToKeystoneMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var keystones keystonev1alpha1.KeystoneList
		if err := c.List(ctx, &keystones, client.InNamespace(obj.GetNamespace())); err != nil {
			log.FromContext(ctx).Error(err, "listing Keystone CRs for PushSecret watch")
			return nil
		}

		name := obj.GetName()
		seen := make(map[types.NamespacedName]struct{})
		for i := range keystones.Items {
			ks := &keystones.Items[i]
			for _, backup := range openBaoBackupPushSecretNames(ks) {
				if backup == name {
					seen[client.ObjectKeyFromObject(ks)] = struct{}{}
					break
				}
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

// pushSecretRelevantChangePredicate filters PushSecret watch events so the
// Keystone workqueue is woken only by state transitions that affect Pass-0
// adoption (esoPushSecretFinalizer added) or Pass-1 deletion (finalizer set
// churn, DeletionTimestamp first becoming non-zero) — never by status-only
// ticks ESO emits on every successful sync (SyncedResourceVersion, conditions,
// LastTransitionTime).
//
// Admission rules:
//   - Create/Delete/Generic: always admitted — name-level filtering is the
//     mapper's job, not the predicate's.
//   - Update: admitted iff at least one of finalizers set, DeletionTimestamp
//     presence (nil vs non-nil), or Generation differs between ObjectOld and
//     ObjectNew. A status-only update (identical finalizers, both
//     DeletionTimestamps nil, identical Generation) is suppressed.
//
// DeletionTimestamp presence is compared via `== nil` on the returned
// *metav1.Time rather than `.IsZero()` so the check is obviously safe against
// a nil pointer without readers having to know that metav1.Time.IsZero carries
// a nil-receiver guard. For DeletionTimestamp specifically the two forms are
// equivalent — the apiserver never sets the pointer to a non-nil zero-time
// value — but `== nil` removes the implicit dependency on that guard
//
// Finalizer comparison uses a sorted-slice compare rather than raw slice
// DeepEqual so a reorder by controllerutil.AddFinalizer / RemoveFinalizer is
// not mistaken for a semantic change.
var pushSecretRelevantChangePredicate = predicate.Funcs{
	CreateFunc:  func(event.CreateEvent) bool { return true },
	DeleteFunc:  func(event.DeleteEvent) bool { return true },
	GenericFunc: func(event.GenericEvent) bool { return true },
	UpdateFunc: func(e event.UpdateEvent) bool {
		if e.ObjectOld == nil || e.ObjectNew == nil {
			return true
		}
		if e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() {
			return true
		}
		if (e.ObjectOld.GetDeletionTimestamp() == nil) != (e.ObjectNew.GetDeletionTimestamp() == nil) {
			return true
		}
		return !finalizersEqualAsSet(e.ObjectOld.GetFinalizers(), e.ObjectNew.GetFinalizers())
	},
}

// finalizersEqualAsSet returns true iff a and b contain the same finalizer
// strings regardless of order. Order is deliberately ignored because
// controllerutil.AddFinalizer / RemoveFinalizer do not guarantee a stable
// ordering and a mere reorder is not a semantic change for the adoption /
// deletion state machine.
func finalizersEqualAsSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aSorted := slices.Clone(a)
	bSorted := slices.Clone(b)
	slices.Sort(aSorted)
	slices.Sort(bSorted)
	return slices.Equal(aSorted, bSorted)
}
