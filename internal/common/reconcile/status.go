// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
)

// SetAggregateReady upserts the aggregate Ready condition based on the given
// sub-condition types: True with reason AllReady when every sub-condition
// type is present and True, False with reason NotAllReady otherwise. It is
// re-aggregated on every status persist — including the early-return paths a
// sub-reconciler takes when it degrades after the CR was already Ready — so
// Ready never stays stale-True when a sub-condition flips (SC-CHAOS-006).
func SetAggregateReady(conds *[]metav1.Condition, observedGeneration int64, subConditionTypes []string) {
	setAggregateReady(conds, observedGeneration, subConditionTypes, false, nil)
}

func setAggregateReady(conds *[]metav1.Condition, generation int64, required []string, requireCurrent bool, passErr error) {
	ready := metav1.Condition{Type: "Ready", ObservedGeneration: generation, Status: metav1.ConditionTrue,
		Reason: "AllReady", Message: "All sub-conditions are ready"}
	for _, name := range required {
		c := conditions.GetCondition(*conds, name)
		switch {
		case c == nil:
			ready.Message = name + " has not been reported"
		case requireCurrent && c.ObservedGeneration != generation:
			ready.Message = fmt.Sprintf("%s last observed generation %d", name, c.ObservedGeneration)
		case c.Status != metav1.ConditionTrue:
			ready.Message = name + ": " + c.Reason
		default:
			continue
		}
		ready.Status, ready.Reason = metav1.ConditionFalse, "NotAllReady"
		if !requireCurrent {
			ready.Message = "One or more sub-conditions are not ready"
		}
		break
	}
	if requireCurrent && passErr != nil && ready.Status == metav1.ConditionTrue {
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, "ReconcileError", "The reconciliation pass failed"
	}
	conditions.SetCondition(conds, ready)
}

// UpdateStatus persists obj's status and returns the given result and error.
// mutate runs first and applies the operator's status hooks (aggregate Ready,
// ObservedGeneration stamp, extra projections) to the status pointed to by
// current.
//
// The write is skipped when this pass left the status semantically unchanged
// from the before snapshot: no write means no watch event and no
// resourceVersion churn. conditions.SetCondition preserves LastTransitionTime
// on a no-op upsert, so a converged steady-state pass produces a status
// identical to the snapshot taken at the top of Reconcile (issue #361). A nil
// snapshot (defensive) always writes.
//
// When both reconcileErr and the status update fail, both errors are
// preserved via errors.Join so that the original reconcile failure is visible
// in controller-runtime logs.
//
// A 409 Conflict on the write is the exception. The object changed after this
// pass read it, so the computed status rests on a stale read: UpdateStatus
// drops it and logs one Info line instead of an error. With a nil reconcileErr
// it returns the shorter of result and RequeueNextPass and a nil error, so the
// next pass recomputes the status from a fresh read; the requeue is needed
// because watch.CRUpdatePredicate filters the CR's status-only updates. With a
// non-nil reconcileErr it returns an empty result and reconcileErr alone, so
// the real failure keeps its error backoff.
func UpdateStatus[S any](
	ctx context.Context,
	c client.Client,
	obj client.Object,
	before, current *S,
	mutate func(),
	result ctrl.Result,
	reconcileErr error,
) (ctrl.Result, error) {
	mutate()

	if before != nil && equality.Semantic.DeepEqual(*before, *current) {
		return result, reconcileErr
	}

	if err := c.Status().Update(ctx, obj); err != nil {
		if apierrors.IsConflict(err) {
			logStatusWriteConflict(ctx, err)
			if reconcileErr != nil {
				return ctrl.Result{}, reconcileErr
			}
			return ShortestRequeue(result, ctrl.Result{RequeueAfter: RequeueNextPass}), nil
		}
		log.FromContext(ctx).Error(err, "unable to update status")
		return ctrl.Result{}, errors.Join(reconcileErr, fmt.Errorf("updating status: %w", err))
	}
	return result, reconcileErr
}

// logStatusWriteConflict logs err, a 409 Conflict a status write returned
// because the object changed after this pass read it, as the Info line
// "status update conflict, requeueing", so the lost write stays visible
// without an error-level line.
func logStatusWriteConflict(ctx context.Context, err error) {
	log.FromContext(ctx).Info("status update conflict, requeueing", "error", err)
}
