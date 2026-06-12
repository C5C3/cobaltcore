// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Watch event mappers and predicates for the Keystone reconciler. Extracted
// from keystone_controller.go so the controller file stays focused on the
// reconcile chain while the Secret/MariaDB/ClusterSecretStore/PushSecret
// event-to-request plumbing lives in one place (issue #467).
package controller

import (
	"context"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// secretToKeystoneMapper returns a MapFunc that maps Secret events to reconcile
// requests for Keystone CRs that either reference the Secret by name
// (resolved via the KeystoneSecretNameIndexKey field indexer) or own it via
// an OwnerReference with Kind=Keystone and APIVersion in the Keystone API
// group (e.g. rotation staging Secrets) (CC-0087, REQ-001, REQ-002, REQ-003,
// REQ-005).
//
// Owner-ref matching is evaluated directly on the event object's metadata and
// is scoped to ref.Kind=="Keystone" and any version in
// keystonev1alpha1.GroupVersion.Group, so Secrets persisted with an older
// APIVersion continue to resolve correctly after a future API version bump.
// For each matching ref, the mapper performs a cached Get to drop owner-refs
// whose target Keystone no longer exists in the informer cache; any
// non-NotFound error falls through to enqueue, so a transient cache blip
// cannot swallow a legitimate event.
func secretToKeystoneMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		namespace := obj.GetNamespace()
		secretName := obj.GetName()
		seen := make(map[types.NamespacedName]struct{})

		var keystones keystonev1alpha1.KeystoneList
		if err := c.List(
			ctx, &keystones,
			client.InNamespace(namespace),
			client.MatchingFields{KeystoneSecretNameIndexKey: secretName},
		); err != nil {
			// Log and swallow: the owner-ref path below is independent of
			// the index and must still run for rotation staging Secrets.
			log.FromContext(ctx).Error(err, "listing Keystone CRs for secret watch")
		} else {
			for i := range keystones.Items {
				seen[client.ObjectKeyFromObject(&keystones.Items[i])] = struct{}{}
			}
		}

		expectedGroup := keystonev1alpha1.GroupVersion.Group
		for _, ref := range obj.GetOwnerReferences() {
			if ref.Kind != "Keystone" {
				continue
			}
			gv, err := schema.ParseGroupVersion(ref.APIVersion)
			if err != nil || gv.Group != expectedGroup {
				continue
			}
			key := types.NamespacedName{Namespace: namespace, Name: ref.Name}
			// Drop stale/spurious owner-refs whose target Keystone no longer
			// exists. A cached Get is an in-memory lookup — no API server
			// round-trip (CC-0087 review #1).
			var ks keystonev1alpha1.Keystone
			if err := c.Get(ctx, key, &ks); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				// Non-NotFound errors (cache mid-sync, disconnected informer,
				// unregistered GVK) must not silently drop the event; log at
				// V(1) and fall through to enqueue so reconcile can resolve
				// authoritatively (CC-0087 review #3).
				log.FromContext(ctx).V(1).Info("owner-ref Get returned non-NotFound error; enqueueing anyway",
					"secret", client.ObjectKeyFromObject(obj),
					"ownerRef", key,
					"error", err)
			}
			seen[key] = struct{}{}
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

// mariaDBToKeystoneMapper returns a MapFunc that maps MariaDB cluster events
// to reconcile requests for Keystone CRs whose spec.database.clusterRef
// targets the MariaDB by name in the same namespace (CC-0047).
func mariaDBToKeystoneMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var keystones keystonev1alpha1.KeystoneList
		if err := c.List(ctx, &keystones, client.InNamespace(obj.GetNamespace())); err != nil {
			log.FromContext(ctx).Error(err, "listing Keystone CRs for MariaDB watch")
			return nil
		}

		mariadbName := obj.GetName()
		var requests []reconcile.Request
		for i := range keystones.Items {
			ks := &keystones.Items[i]
			if ks.Spec.Database.ClusterRef != nil && ks.Spec.Database.ClusterRef.Name == mariadbName {
				requests = append(requests, reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(ks),
				})
			}
		}
		return requests
	}
}

// clusterSecretStoreToKeystoneMapper returns a MapFunc that enqueues every
// Keystone CR in the cluster when the OpenBao-backed ClusterSecretStore
// changes. The store is cluster-scoped and shared across namespaces, so any
// status transition (e.g. ESO losing the backend connection) must retrigger
// reconcile on all Keystones that route secrets through it (CC-0047).
func clusterSecretStoreToKeystoneMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		if obj.GetName() != openBaoClusterStoreName {
			return nil
		}

		var keystones keystonev1alpha1.KeystoneList
		if err := c.List(ctx, &keystones); err != nil {
			log.FromContext(ctx).Error(err, "listing Keystone CRs for ClusterSecretStore watch")
			return nil
		}

		requests := make([]reconcile.Request, 0, len(keystones.Items))
		for i := range keystones.Items {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&keystones.Items[i]),
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
// scoping is load-bearing — REQ-002 requires that a PushSecret event in ns-b
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
// single source of truth (CC-0092, REQ-001, REQ-002, REQ-003, REQ-007).
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
//     mapper's job, not the predicate's (CC-0092, REQ-004).
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
// (CC-0092, REQ-004).
//
// Finalizer comparison uses a sorted-slice compare rather than raw slice
// DeepEqual so a reorder by controllerutil.AddFinalizer / RemoveFinalizer is
// not mistaken for a semantic change (CC-0092, REQ-004).
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
// deletion state machine (CC-0092, REQ-004).
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
