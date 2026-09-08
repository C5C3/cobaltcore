// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package satellite

import (
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CollectParams is the parent's view of its attached satellites: the list it
// read, the gate deciding which of them may be projected, and the optional
// default rule.
type CollectParams[T any, PT interface {
	*T
	client.Object
}] struct {
	// Items is the caller's indexed List result, in any order. A List type
	// holding values, as GlanceBackendList.Items does, is turned into pointers
	// by appending &list.Items[i] over the index.
	Items []PT
	// Gate is the D-gate, glance's CredentialsReady == True. A nil gate passes
	// everything.
	Gate func(PT) bool
	// IsDefault marks a satellite as the parent's default. Nil means the parent
	// has no default rule, which makes Valid always true.
	IsDefault func(PT) bool
}

// Collection is what one pass sees: the attached satellites in a deterministic
// order, the subset the gate lets through, the default candidates among them,
// and whether the exactly-one-default rule holds.
type Collection[T any, PT interface {
	*T
	client.Object
}] struct {
	// Attached holds the items that are not being deleted, sorted by name. The
	// pointers alias the caller's objects.
	Attached []PT
	// Gated holds the items of Attached that pass the gate, in the same order.
	Gated []PT
	// DefaultCandidates holds the items of Gated that IsDefault marks, in the
	// same order.
	DefaultCandidates []PT
	// Valid reports whether the default rule holds: no rule at all, or exactly
	// one gated candidate.
	Valid bool
}

// Collect turns a parent's list of attached satellites into one pass's view of
// them. It never returns an error and never touches the API: everything it
// needs is already on the objects the caller listed.
//
// A deleting satellite is dropped here, so the pass de-projects it immediately
// instead of waiting for the object to go. Sorting by name is what makes the
// rendered artefact, and therefore its content hash, stable across passes.
//
// Valid is a decision, not a condition. The parent stamps its own condition type
// with its own reason and message, which is also what the condition-coverage
// audit needs: it finds a sub-reconciler's condition by grepping the operator's
// reconcile_*.go for it.
func Collect[T any, PT interface {
	*T
	client.Object
}](p CollectParams[T, PT]) Collection[T, PT] {
	attached := make([]PT, 0, len(p.Items))
	for _, item := range p.Items {
		if item.GetDeletionTimestamp() == nil {
			attached = append(attached, item)
		}
	}
	sort.Slice(attached, func(i, j int) bool { return attached[i].GetName() < attached[j].GetName() })

	gated := make([]PT, 0, len(attached))
	for _, item := range attached {
		if p.Gate == nil || p.Gate(item) {
			gated = append(gated, item)
		}
	}

	var candidates []PT
	if p.IsDefault != nil {
		for _, item := range gated {
			if p.IsDefault(item) {
				candidates = append(candidates, item)
			}
		}
	}

	return Collection[T, PT]{
		Attached:          attached,
		Gated:             gated,
		DefaultCandidates: candidates,
		Valid:             p.IsDefault == nil || len(candidates) == 1,
	}
}
