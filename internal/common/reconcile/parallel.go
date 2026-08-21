// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"context"

	"golang.org/x/sync/errgroup"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
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
// in status, along with the metadata a member persisted on its copy (see
// adoptMetadataWrites). On success the shortest non-zero RequeueAfter is
// returned.
func RunParallelGroup[T interface {
	client.Object
	DeepCopy() T
}](
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
	// Every copy started from this resourceVersion, so it — not the primary's
	// possibly already-adopted one — is what identifies a member that wrote.
	baseResourceVersion := cr.GetResourceVersion()

	var results []ctrl.Result
	for _, o := range outcomes {
		MergeCondition(conditionsOf(cr), *conditionsOf(o.copy), o.condType)
		adoptMetadataWrites(cr, o.copy, baseResourceVersion)
		if o.err == nil {
			results = append(results, o.result)
		}
	}

	if groupErr != nil {
		return ctrl.Result{}, groupErr
	}

	return ShortestRequeue(results...), nil
}

// adoptMetadataWrites carries a member's own writes to the CR's metadata back
// onto the primary object. A member that patched the CR — job.RecordJobTerminalState
// stamping its once-per-UID dedupe annotation is the case today — patched the
// live object, so the stored resourceVersion moved on while the primary still
// holds the pre-patch one. Left alone, the caller's trailing Status().Update
// fails with a 409 and drops the very condition transition the group just
// merged, on exactly the passes an operator is watching for.
//
// Adopting the version is only sound because the member's patch is
// optimistically locked on baseResourceVersion (see job.RecordJobTerminalState):
// a version that differs from the base is therefore this reconcile's own
// snapshot advanced by that patch and nothing else, so the trailing status write
// is still validated against what its conditions were computed from. A blind
// patch would instead land on whatever the API server currently holds and
// adopting its version would disable that check.
//
// Annotations are merged key-by-key rather than replaced so two members that
// both stamp one do not overwrite each other. The same locking bounds the
// several-patchers case to one winner per pass: the losers' patches fail the
// precondition, they adopt nothing, and they retry on the requeue.
func adoptMetadataWrites(cr, member client.Object, baseResourceVersion string) {
	if member.GetResourceVersion() == baseResourceVersion {
		return
	}
	anns := cr.GetAnnotations()
	if anns == nil {
		anns = map[string]string{}
	}
	for k, v := range member.GetAnnotations() {
		anns[k] = v
	}
	cr.SetAnnotations(anns)
	cr.SetResourceVersion(member.GetResourceVersion())
}
