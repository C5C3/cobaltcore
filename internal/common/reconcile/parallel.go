// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"context"

	"golang.org/x/sync/errgroup"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/c5c3/forge/internal/common/conditions"
)

// ParallelStep describes a sub-reconciler that runs in a parallel group. Each
// sub-reconciler receives its own DeepCopy of the CR and sets exactly one
// condition type.
//
// Name is the sub_reconciler label value used by the operator's instrument
// function so that duration/error series are attributed to the individual
// group member rather than the group as a whole.
type ParallelStep[T any] struct {
	Name          string
	ConditionType string
	Fn            func(ctx context.Context, cr T) (ctrl.Result, error)
}

// MergeCondition copies a single condition of the given type from src into
// dst. If src does not contain a condition of that type, dst is left
// unchanged. Pre-existing conditions on dst are preserved.
func MergeCondition(dst *[]metav1.Condition, src []metav1.Condition, conditionType string) {
	cond := conditions.GetCondition(src, conditionType)
	if cond == nil {
		return
	}
	conditions.SetCondition(dst, *cond)
}

// RunParallelGroup runs the given sub-reconcilers concurrently using
// errgroup.WithContext. Each goroutine operates on a DeepCopy of the CR to
// avoid data races; conditionsOf resolves a CR (primary or copy) to its
// status conditions slice. After all goroutines complete, conditions from
// every sub-reconciler — including those that succeeded before a peer failed
// — are merged back into the primary CR so that partial progress is visible
// in status. On success the shortest non-zero RequeueAfter is returned.
func RunParallelGroup[T interface{ DeepCopy() T }](
	ctx context.Context,
	cr T,
	conditionsOf func(T) *[]metav1.Condition,
	instrument InstrumentFunc,
	steps []ParallelStep[T],
) (ctrl.Result, error) {
	g, gctx := errgroup.WithContext(ctx)

	type outcome struct {
		result   ctrl.Result
		copy     T
		condType string
		err      error
	}
	outcomes := make([]outcome, len(steps))

	for i, sub := range steps {
		crCopy := cr.DeepCopy()
		outcomes[i].condType = sub.ConditionType
		outcomes[i].copy = crCopy
		g.Go(func() error {
			// Route through the instrument function so each parallel member
			// emits its own duration sample and — on failure — its own error
			// counter tagged with sub.Name.
			res, err := instrument(gctx, sub.Name, func(ctx context.Context) (ctrl.Result, error) {
				return sub.Fn(ctx, crCopy)
			})
			outcomes[i].result = res
			outcomes[i].err = err
			return err
		})
	}

	groupErr := g.Wait()

	// Merge conditions from all completed sub-reconcilers back into the
	// primary CR, even on partial failure, so the caller can persist partial
	// progress via its status writer.
	var results []ctrl.Result
	for _, o := range outcomes {
		MergeCondition(conditionsOf(cr), *conditionsOf(o.copy), o.condType)
		if o.err == nil {
			results = append(results, o.result)
		}
	}

	if groupErr != nil {
		return ctrl.Result{}, groupErr
	}

	return ShortestRequeue(results...), nil
}
