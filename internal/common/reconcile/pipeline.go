// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
)

// Step is one entry in the sequential reconcile pipeline driven by
// RunPipeline. Name is the sub_reconciler metric label resolved via the
// operator's instrument function. A step with an empty Name is NOT wrapped in
// the instrument function because it either self-instruments its members (a
// parallel group, whose ParallelStep entries instrument individually) or is
// intentionally uninstrumented.
type Step struct {
	Name string
	Fn   func(ctx context.Context) (ctrl.Result, error)
}

// InstrumentFunc wraps a sub-reconciler call with per-operator duration/error
// instrumentation. name is the sub_reconciler metric label value. Both
// operators satisfy this with their instrumentSubReconciler glue over
// internal/common/instrumentation.
type InstrumentFunc func(ctx context.Context, name string, fn func(context.Context) (ctrl.Result, error)) (ctrl.Result, error)

// RunPipeline runs the steps in order. Named steps are wrapped in instrument;
// empty-Name steps run bare. The first step to return a non-zero result or an
// error short-circuits the chain and is returned to the caller, which funnels
// it through its status writer so conditions and the requeue/error are
// persisted by construction on every exit path. A fully successful chain
// returns a zero result and nil error.
func RunPipeline(ctx context.Context, instrument InstrumentFunc, steps []Step) (ctrl.Result, error) {
	for _, s := range steps {
		var (
			result ctrl.Result
			err    error
		)
		if s.Name == "" {
			result, err = s.Fn(ctx)
		} else {
			result, err = instrument(ctx, s.Name, s.Fn)
		}
		if !result.IsZero() || err != nil {
			return result, err
		}
	}
	return ctrl.Result{}, nil
}

// ShortestRequeue returns the ctrl.Result with the shortest non-zero
// RequeueAfter from the given results. If no result requests a requeue,
// a zero ctrl.Result is returned.
//
// Sub-reconcilers signal a requeue exclusively via RequeueAfter — the
// non-deprecated requeue field — so this function intentionally keys off
// RequeueAfter only. The keystone fernet/credential reconcilers in particular
// return RequeueAfter (not the deprecated ctrl.Result.Requeue) precisely so
// their short-circuit intent survives this aggregation in the parallel group
// (issue #467).
func ShortestRequeue(results ...ctrl.Result) ctrl.Result {
	var shortest ctrl.Result
	for _, r := range results {
		if r.RequeueAfter <= 0 {
			continue
		}
		if shortest.RequeueAfter <= 0 || r.RequeueAfter < shortest.RequeueAfter {
			shortest = r
		}
	}
	return shortest
}
