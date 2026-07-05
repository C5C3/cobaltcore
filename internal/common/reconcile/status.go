// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
)

// SetAggregateReady upserts the aggregate Ready condition based on the given
// sub-condition types: True with reason AllReady when every sub-condition
// type is present and True, False with reason NotAllReady otherwise. It is
// re-aggregated on every status persist — including the early-return paths a
// sub-reconciler takes when it degrades after the CR was already Ready — so
// Ready never stays stale-True when a sub-condition flips (SC-CHAOS-006).
func SetAggregateReady(conds *[]metav1.Condition, observedGeneration int64, subConditionTypes []string) {
	if conditions.AllTrue(*conds, subConditionTypes...) {
		conditions.SetCondition(conds, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: observedGeneration,
			Reason:             "AllReady",
			Message:            "All sub-conditions are ready",
		})
	} else {
		conditions.SetCondition(conds, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: observedGeneration,
			Reason:             "NotAllReady",
			Message:            "One or more sub-conditions are not ready",
		})
	}
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
		log.FromContext(ctx).Error(err, "unable to update status")
		return ctrl.Result{}, errors.Join(reconcileErr, fmt.Errorf("updating status: %w", err))
	}
	return result, reconcileErr
}
