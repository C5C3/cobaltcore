// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/conditions"
)

// Skeleton bundles the per-operator controller-skeleton glue every service
// operator repeated: aggregating its sub-conditions into the Ready condition,
// writing the status subresource only when it changed, flipping a condition to
// False on a pre-aggregation failure, and running a group of sub-reconcilers in
// parallel. An operator declares one Skeleton value with its sub-condition
// vocabulary and its status-conditions accessor, then its thin wrapper methods
// delegate here.
//
// T is the operator's CR pointer type (a client.Object that also DeepCopies to
// itself, as controller-gen generates); S is its status struct type, compared
// by UpdateStatus to skip a no-op write.
type Skeleton[T interface {
	client.Object
	DeepCopy() T
}, S any] struct {
	// SubConditionTypes are the sub-condition types SetReady aggregates into the
	// Ready condition, in the operator's vocabulary.
	SubConditionTypes []string
	// Conditions resolves a CR (the primary or a DeepCopy) to its status
	// conditions slice pointer.
	Conditions func(T) *[]metav1.Condition
}

// SetReady aggregates the CR's sub-conditions into its Ready condition at the
// CR's current generation.
func (s Skeleton[T, S]) SetReady(obj T) {
	SetAggregateReady(s.Conditions(obj), obj.GetGeneration(), s.SubConditionTypes)
}

// UpdateStatus aggregates the Ready condition, runs extraMutate (typically
// stamping status.observedGeneration and any operator-specific status fields),
// then writes the status subresource only when it differs from before. Passing
// a nil extraMutate is allowed. It preserves the reconcile error semantics of
// the shared UpdateStatus: a converged steady-state pass produces no write.
func (s Skeleton[T, S]) UpdateStatus(ctx context.Context, c client.Client, obj T, before, current *S, extraMutate func(), result ctrl.Result, reconcileErr error) (ctrl.Result, error) {
	return UpdateStatus(ctx, c, obj, before, current, func() {
		s.SetReady(obj)
		if extraMutate != nil {
			extraMutate()
		}
	}, result, reconcileErr)
}

// MarkFailed flips the named condition to False with the given reason and the
// error's message, so a failed sub-reconciler that runs before Ready
// aggregation cannot leave the aggregate Ready stale-True at the new
// observedGeneration.
func (s Skeleton[T, S]) MarkFailed(obj T, conditionType, reason string, err error) {
	conditions.SetCondition(s.Conditions(obj), metav1.Condition{
		Type:               conditionType,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: obj.GetGeneration(),
		Reason:             reason,
		Message:            err.Error(),
	})
}

// RunParallelGroup runs the sub-reconcilers concurrently via RunParallelGroup,
// resolving each CR's conditions through the Skeleton's accessor and
// instrumenting each member through instrument. Conditions from every member —
// including those that succeeded before a peer failed — are merged back into
// obj, and on success the shortest non-zero RequeueAfter is returned.
func (s Skeleton[T, S]) RunParallelGroup(ctx context.Context, obj T, instrument InstrumentFunc, steps []ParallelStep[T]) (ctrl.Result, error) {
	return RunParallelGroup(ctx, obj, s.Conditions, instrument, steps)
}
