// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
)

const (
	// ConditionTypeSuspended is the condition a suspended resource carries. It is
	// never one of a Skeleton's SubConditionTypes, so it never feeds Ready.
	ConditionTypeSuspended = "Suspended"
	// ReasonReconciliationSuspended is the reason of the Suspended condition and
	// of the Normal event emitted when a resource becomes suspended.
	ReasonReconciliationSuspended = "ReconciliationSuspended"
	// ReasonReconciliationResumed is the reason of the Normal event emitted when
	// spec.suspend is cleared and the Suspended condition is removed.
	ReasonReconciliationResumed = "ReconciliationResumed"

	suspendedConditionMessage = "Reconciliation is suspended by spec.suspend; the operator does not " +
		"create, update or delete anything for this resource until spec.suspend is cleared"
	suspendedEventMessage = "Reconciliation suspended by spec.suspend"
	resumedEventMessage   = "Reconciliation resumed: spec.suspend is cleared"
)

// GateSuspend reports whether the caller must end this pass because the
// resource is suspended, and keeps the Suspended condition in step with
// suspended. obj is the fetched resource and conds points at its
// status.conditions; suspended is its spec.suspend.
//
// While suspended it sets Suspended=True at the current generation and
// returns stop=true. Once suspended is false it removes a Suspended condition
// left by an earlier pass and returns stop=false, so the caller runs its full
// pass. Every change is persisted here with one status write: the callers
// take their pipeline status snapshot at different points, and a change left
// to their write would be lost wherever the snapshot follows the gate. The
// gate touches no other condition, not status.observedGeneration and no
// finalizer. A Normal event is emitted only on a transition, after its write
// succeeded; a nil recorder skips it. A failed write returns stop=true and
// the wrapped error, so the caller backs off.
func GateSuspend(ctx context.Context, c client.Client, recorder record.EventRecorder,
	obj client.Object, conds *[]metav1.Condition, suspended bool,
) (stop bool, err error) {
	current := conditions.GetCondition(*conds, ConditionTypeSuspended)

	if !suspended {
		if current == nil {
			return false, nil
		}
		meta.RemoveStatusCondition(conds, ConditionTypeSuspended)
		if err := c.Status().Update(ctx, obj); err != nil {
			return true, suspendWriteError(err)
		}
		log.FromContext(ctx).Info("Reconciliation resumed", "generation", obj.GetGeneration())
		emitNormal(recorder, obj, ReasonReconciliationResumed, resumedEventMessage)
		return false, nil
	}

	transition := current == nil || current.Status != metav1.ConditionTrue
	if !transition && current.ObservedGeneration == obj.GetGeneration() {
		return true, nil
	}
	conditions.SetCondition(conds, metav1.Condition{
		Type:               ConditionTypeSuspended,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: obj.GetGeneration(),
		Reason:             ReasonReconciliationSuspended,
		Message:            suspendedConditionMessage,
	})
	if err := c.Status().Update(ctx, obj); err != nil {
		return true, suspendWriteError(err)
	}
	if transition {
		log.FromContext(ctx).Info("Reconciliation suspended", "generation", obj.GetGeneration())
		emitNormal(recorder, obj, ReasonReconciliationSuspended, suspendedEventMessage)
	}
	return true, nil
}

func suspendWriteError(err error) error {
	return fmt.Errorf("updating status for the %s condition: %w", ConditionTypeSuspended, err)
}

func emitNormal(recorder record.EventRecorder, obj client.Object, reason, message string) {
	if recorder != nil {
		recorder.Event(obj, corev1.EventTypeNormal, reason, message)
	}
}
