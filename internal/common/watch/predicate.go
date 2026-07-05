// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// CRUpdatePredicate filters update events on a CR's own For(...) watch so the
// controller is not re-woken by its own Status().Update writes. It admits an
// update only on a spec change (GenerationChangedPredicate) or a
// label/annotation change; a status-only update produces no reconcile.
//
// Trade-offs, all deliberate:
//   - Deletion still delivers: `kubectl delete` on a CR carrying a finalizer
//     sets metadata.deletionTimestamp, which — because the CRD has a status
//     subresource — does NOT bump generation, and touches neither labels nor
//     annotations. The three predicates above would therefore filter the
//     live→Terminating Update, and the actual Delete event does not fire until
//     the finalizer is removed, so finalizer cleanup would stall until the
//     next resync. The explicit `terminating` predicate admits that
//     transition so cleanup runs immediately.
//   - An operator's own annotation patches (e.g. keystone's db_job_metrics
//     dedupe annotation) still pass via AnnotationChangedPredicate — rare and
//     harmless.
//   - The periodic informer resync on the CR itself is filtered, but every
//     Owns()/Watches() secondary resource still resyncs and enqueues the
//     owner, so the drift-repair net is preserved.
//   - A future feature that must reconcile on CR *status* written by another
//     actor would be filtered by this predicate; that is an intentional part
//     of the contract.
func CRUpdatePredicate() predicate.Predicate {
	// DeletionTimestamp presence is compared via `== nil`/`!= nil` on the
	// returned *metav1.Time rather than `.IsZero()` so the check is obviously
	// nil-safe.
	terminating := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return false
			}
			return e.ObjectOld.GetDeletionTimestamp() == nil &&
				e.ObjectNew.GetDeletionTimestamp() != nil
		},
	}
	return predicate.Or(
		predicate.GenerationChangedPredicate{},
		predicate.LabelChangedPredicate{},
		predicate.AnnotationChangedPredicate{},
		terminating,
	)
}
