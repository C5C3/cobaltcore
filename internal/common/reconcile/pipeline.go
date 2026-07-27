// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"context"
	"errors"

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

// run dispatches the step following the empty-Name convention documented on
// Step. Every group helper routes its members through this so the convention
// is implemented exactly once.
func (s Step) run(ctx context.Context, instrument InstrumentFunc) (ctrl.Result, error) {
	if s.Name == "" {
		return s.Fn(ctx)
	}
	return instrument(ctx, s.Name, s.Fn)
}

// InstrumentFunc wraps a sub-reconciler call with per-operator duration/error
// instrumentation. name is the sub_reconciler metric label value. Operators
// satisfy this by passing the bound Instrument method of their
// internal/common/instrumentation instrumenter.
type InstrumentFunc func(ctx context.Context, name string, fn func(context.Context) (ctrl.Result, error)) (ctrl.Result, error)

// RunPipeline runs the steps in order. Named steps are wrapped in instrument;
// empty-Name steps run bare. The first step to return a non-zero result or an
// error short-circuits the chain and is returned to the caller, which funnels
// it through its status writer so conditions and the requeue/error are
// persisted by construction on every exit path. A fully successful chain
// returns a zero result and nil error.
func RunPipeline(ctx context.Context, instrument InstrumentFunc, steps []Step) (ctrl.Result, error) {
	for _, s := range steps {
		result, err := s.run(ctx, instrument)
		if !result.IsZero() || err != nil {
			return result, err
		}
	}
	return ctrl.Result{}, nil
}

// RunSequentialGroup runs the steps in order, exactly once each, and never
// short-circuits: a step's non-zero result or non-nil error never prevents a
// later step from running. This suits a reconcile tail of independent,
// self-gated projections where every member must be attempted every pass.
// Named steps are wrapped in instrument; empty-Name steps run bare, the same
// convention RunPipeline follows.
//
// When no member returns an error, the shortest non-zero member RequeueAfter
// wins (via ShortestRequeue) and the error is nil. When one or more members
// return errors, the member requeues are discarded and ctrl.Result{} is
// returned together with errors.Join of every member error, so
// controller-runtime's error backoff drives the retry. A nil or empty steps
// slice returns a zero result and a nil error.
//
// Errors beat requeues deliberately, and the cost is real: controller-runtime
// discards Result whenever err != nil, so a single erroring member throws away
// a converged-but-waiting sibling's pacing and re-runs the whole chain on the
// failure ramp instead. Surfacing the error is still the right trade — it is
// what raises the reconcile error counter and puts the failure in the manager's
// log. Swallowing it to keep a sibling's interval would leave a persistently
// failing member visible only to whatever conditions its own body happened to
// stamp, which a general-purpose helper cannot assume. Callers that need a
// gentler cadence should bound the retry at the member (return a RequeueAfter
// with a stamped condition instead of an error), not here.
//
// This differs from RunParallelGroup in one respect worth knowing: that helper
// is errgroup-backed, so it surfaces only the FIRST member error, not a join of
// all of them.
func RunSequentialGroup(ctx context.Context, instrument InstrumentFunc, steps []Step) (ctrl.Result, error) {
	var (
		results []ctrl.Result
		errs    []error
	)
	for _, s := range steps {
		result, err := s.run(ctx, instrument)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		results = append(results, result)
	}
	if len(errs) > 0 {
		return ctrl.Result{}, errors.Join(errs...)
	}
	return ShortestRequeue(results...), nil
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
